package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arugula-salad/loam/pkg/api"
)

func cmdPorts(a *app, args []string) error {
	fs := a.flags("loam ports")
	watch := fs.Bool("watch", false, "stream listeners opening and closing")
	pos, err := parseArgs(fs, args, -1)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errUsage
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	if *watch {
		w, err := c.WatchPorts(a.ctx, pos[0])
		if err != nil {
			return err
		}
		defer w.Close()
		for {
			ev, err := w.Next()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			if a.json {
				b, _ := jsonLine(map[string]any{"type": ev.Type, "port": ev.Port})
				a.stdout.Write(b)
				continue
			}
			fmt.Fprintf(a.stdout, "%s  %-6s %s:%d pid %d %s\n", time.Now().Format(time.TimeOnly), ev.Type, ev.Port.Address, ev.Port.Port, ev.Port.PID, ev.Port.Session)
		}
	}
	l, err := c.Ports(a.ctx, pos[0])
	if err != nil {
		return err
	}
	if l == nil {
		l = []api.Port{}
	}
	return a.output(l, func() {
		var rows [][]string
		for _, p := range l {
			rows = append(rows, []string{strconv.Itoa(p.Port), p.Address, strconv.Itoa(p.PID), p.Session})
		}
		a.table([]string{"PORT", "ADDRESS", "PID", "SESSION"}, rows)
	})
}

type forward struct {
	local  string // host:port to listen on
	remote int
}

// parseForward reads <local>:<remote>, [addr:]<local>:<remote> or a bare
// <port> for the same number on both sides. Local listeners are on loopback
// unless an address is given.
func parseForward(s string) (forward, error) {
	parts := strings.Split(s, ":")
	var host, lp, rp string
	switch len(parts) {
	case 1:
		host, lp, rp = "127.0.0.1", parts[0], parts[0]
	case 2:
		host, lp, rp = "127.0.0.1", parts[0], parts[1]
	case 3:
		host, lp, rp = parts[0], parts[1], parts[2]
	default:
		return forward{}, fmt.Errorf("bad forward %q: want <local>:<remote>", s)
	}
	r, err := strconv.Atoi(rp)
	if err != nil || r <= 0 || r > 65535 {
		return forward{}, fmt.Errorf("bad remote port in %q", s)
	}
	if _, err := strconv.Atoi(lp); err != nil {
		return forward{}, fmt.Errorf("bad local port in %q", s)
	}
	return forward{local: net.JoinHostPort(host, lp), remote: r}, nil
}

func cmdPortForward(a *app, args []string) error {
	fs := a.flags("loam port-forward")
	pos, err := parseArgs(fs, args, -1)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return errUsage
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	sandbox := pos[0]
	var lns []net.Listener
	defer func() {
		for _, ln := range lns {
			ln.Close()
		}
	}()
	var wg sync.WaitGroup
	errc := make(chan error, len(pos))
	for _, spec := range pos[1:] {
		f, err := parseForward(spec)
		if err != nil {
			return err
		}
		ln, err := net.Listen("tcp", f.local)
		if err != nil {
			return err
		}
		lns = append(lns, ln)
		fmt.Fprintf(a.stderr, "forwarding %s -> %s:%d\n", ln.Addr(), sandbox, f.remote)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				conn, err := ln.Accept()
				if err != nil {
					errc <- err
					return
				}
				go func() {
					defer conn.Close()
					rc, err := c.ConnectPort(a.ctx, sandbox, f.remote)
					if err != nil {
						fmt.Fprintf(a.stderr, "loam: %s:%d: %v\n", sandbox, f.remote, err)
						return
					}
					defer rc.Close()
					pipe(conn, rc)
				}()
			}
		}()
	}
	select {
	case err := <-errc:
		return err
	case <-a.ctx.Done():
		return nil
	}
}

// pipe copies both ways until either side ends. The stream to the sandbox
// has no half-close, so the first side to finish ends both.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
	a.Close()
	b.Close()
	<-done
}
