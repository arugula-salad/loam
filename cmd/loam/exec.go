package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/term"

	"github.com/arugula-salad/loam/pkg/api"
	"github.com/arugula-salad/loam/pkg/client"
)

// detachKey (Ctrl-]) leaves an interactive session without ending it, as in telnet.
const detachKey = 0x1d

// exitUnknown is the CLI's exit code when a session ended without an exit status.
const exitUnknown = 255

func cmdExec(a *app, args []string) error {
	fs := a.flags("loam exec")
	var o client.ExecOptions
	env := kvFlag{}
	var interactive, detach bool
	fs.BoolVar(&o.TTY, "t", false, "allocate a TTY (raw mode, resized with the terminal)")
	fs.BoolVar(&o.TTY, "tty", false, "allocate a TTY")
	fs.BoolVar(&interactive, "i", false, "forward stdin even when it is a terminal")
	fs.BoolVar(&interactive, "stdin", false, "forward stdin even when it is a terminal")
	fs.StringVar(&o.Cwd, "cwd", "", "working directory")
	fs.Var(env, "env", "K=V on top of the sandbox's env (repeatable)")
	fs.StringVar(&o.User, "user", "", "run as this user (default loam)")
	fs.BoolVar(&o.Detachable, "detachable", false, "the session outlives this connection (Ctrl-] detaches a TTY)")
	fs.BoolVar(&detach, "d", false, "start a detachable session, print its ID and leave it running")
	fs.BoolVar(&detach, "detach", false, "start a detachable session, print its ID and leave it running")
	pos, err := parseArgs(fs, args, 1)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return errUsage
	}
	o.Cmd = pos[1:]
	if len(env) > 0 {
		o.Env = env
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	if detach {
		o.Detachable = true
		s, err := c.Exec(a.ctx, pos[0], o)
		if err != nil {
			return err
		}
		s.Close()
		return a.output(map[string]any{"session": s.Session, "pid": s.PID}, func() { fmt.Fprintln(a.stdout, s.Session) })
	}
	return a.interact(o.TTY, interactive, o.Detachable, func(cols, rows int, io client.IO) (*client.Stream, error) {
		o.Cols, o.Rows, o.IO = cols, rows, io
		return c.Exec(a.ctx, pos[0], o)
	})
}

// loginShell runs the user's login shell, whatever the template installs.
var loginShell = []string{"/bin/sh", "-c", `exec "${SHELL:-/bin/sh}" -l`}

func cmdConsole(a *app, args []string) error {
	fs := a.flags("loam console")
	var o client.ExecOptions
	fs.StringVar(&o.User, "user", "", "log in as this user (default loam)")
	fs.BoolVar(&o.Detachable, "detachable", false, "the session outlives this connection (Ctrl-] detaches)")
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
	o.Cmd, o.TTY = loginShell, true
	return a.interact(true, true, o.Detachable, func(cols, rows int, io client.IO) (*client.Stream, error) {
		o.Cols, o.Rows, o.IO = cols, rows, io
		return c.Exec(a.ctx, pos[0], o)
	})
}

func cmdAttach(a *app, args []string) error {
	fs := a.flags("loam attach")
	offset := fs.Int64("offset", -1, "replay output from this byte (default: everything kept)")
	interactive := fs.Bool("i", false, "forward stdin even when it is a terminal (non-TTY sessions)")
	pos, err := parseArgs(fs, args, -1)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errUsage
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	// The session's own TTY flag decides the terminal mode.
	tty := false
	if ss, err := c.Sessions(a.ctx, pos[0]); err == nil {
		for _, s := range ss {
			if s.ID == pos[1] {
				tty = s.TTY
			}
		}
	}
	o := client.AttachOptions{}
	if *offset >= 0 {
		o.Offset = offset
	}
	return a.interact(tty, *interactive, true, func(cols, rows int, io client.IO) (*client.Stream, error) {
		o.IO = io
		s, err := c.Attach(a.ctx, pos[0], pos[1], o)
		if err == nil && tty && cols > 0 {
			s.Resize(cols, rows)
		}
		return s, err
	})
}

// interact runs a stream against the terminal: raw mode and resizes with a
// TTY, signal forwarding without one, stdin when it should go, and the
// remote exit code as the CLI's.
func (a *app) interact(tty, forceStdin, detachable bool, start func(cols, rows int, io client.IO) (*client.Stream, error)) error {
	inFile, _ := a.stdin.(*os.File)
	inTerm := inFile != nil && term.IsTerminal(int(inFile.Fd()))
	size := func() (int, int) {
		for _, w := range []any{a.stdout, a.stdin} {
			if f, ok := w.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
				if cols, rows, err := term.GetSize(int(f.Fd())); err == nil {
					return cols, rows
				}
			}
		}
		return 0, 0
	}
	cols, rows := 0, 0
	if tty {
		cols, rows = size()
	}
	s, err := start(cols, rows, client.IO{Stdout: a.stdout, Stderr: a.stderr})
	if err != nil {
		return err
	}
	restore := func() {}
	if tty && inTerm {
		old, err := term.MakeRaw(int(inFile.Fd()))
		if err != nil {
			s.Close()
			return fmt.Errorf("raw mode: %w", err)
		}
		restore = func() { term.Restore(int(inFile.Fd()), old) }
		stop := onResize(func() {
			if c, r := size(); c > 0 {
				s.Resize(c, r)
			}
		})
		defer stop()
	}
	if !tty {
		sigs := make(chan os.Signal, 1)
		for sig := range forwardedSignals {
			signal.Notify(sigs, sig)
		}
		defer signal.Stop(sigs)
		go func() {
			for {
				select {
				case sig := <-sigs:
					s.Signal(forwardedSignals[sig])
				case <-s.Done():
					return
				}
			}
		}()
	}

	var detached atomic.Bool
	if tty || forceStdin || !inTerm {
		watchKey := tty && detachable && inTerm
		go func() {
			buf := make([]byte, 32<<10)
			for {
				n, err := a.stdin.Read(buf)
				p := buf[:n]
				if watchKey {
					if i := bytes.IndexByte(p, detachKey); i >= 0 {
						s.Write(p[:i])
						detached.Store(true)
						s.Close()
						return
					}
				}
				if len(p) > 0 {
					if _, werr := s.Write(p); werr != nil {
						return
					}
				}
				if err != nil {
					s.CloseStdin()
					return
				}
			}
		}()
	} else {
		// A terminal on stdin without -i: the command gets EOF rather than
		// waiting on input nobody will type.
		s.CloseStdin()
	}

	code, err := s.Wait()
	restore()
	if detached.Load() {
		fmt.Fprintf(a.stderr, "\r\ndetached from session %s\n", s.Session)
		return nil
	}
	if err != nil {
		fmt.Fprintf(a.stderr, "loam: %v\n", err)
		return exitError{exitUnknown}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func cmdRun(a *app, args []string) error {
	fs := a.flags("loam run")
	var req api.ExecRun
	env := kvFlag{}
	var timeout time.Duration
	fs.StringVar(&req.Cwd, "cwd", "", "working directory")
	fs.Var(env, "env", "K=V on top of the sandbox's env (repeatable)")
	fs.StringVar(&req.User, "user", "", "run as this user (default loam)")
	fs.DurationVar(&timeout, "timeout", 0, "kill the command after this long (default 60s, max 1h)")
	pos, err := parseArgs(fs, args, 1)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return errUsage
	}
	req.Cmd = pos[1:]
	if len(env) > 0 {
		req.Env = env
	}
	req.TimeoutS = int((timeout + time.Second - 1) / time.Second)
	if f, ok := a.stdin.(*os.File); !ok || !term.IsTerminal(int(f.Fd())) {
		if req.Stdin, err = io.ReadAll(a.stdin); err != nil {
			return err
		}
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	r, err := c.Run(a.ctx, pos[0], req)
	if err != nil {
		return err
	}
	if a.json {
		if err := a.printJSON(r); err != nil {
			return err
		}
	} else {
		a.stdout.Write(r.Stdout)
		a.stderr.Write(r.Stderr)
		if r.Truncated {
			fmt.Fprintln(a.stderr, "loam: output truncated at 16 MiB per stream")
		}
		if r.TimedOut {
			fmt.Fprintln(a.stderr, "loam: timed out")
		}
	}
	if r.ExitCode != 0 {
		return exitError{r.ExitCode}
	}
	return nil
}

func cmdSessions(a *app, args []string) error {
	pos, err := positionals(a, "loam sessions", args, 1)
	if err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	l, err := c.Sessions(a.ctx, pos[0])
	if err != nil {
		return err
	}
	if l == nil {
		l = []api.Session{}
	}
	return a.output(l, func() {
		var rows [][]string
		for _, s := range l {
			status := "running"
			if s.Exited {
				status = "exited"
				if s.ExitCode != nil {
					status += " " + strconv.Itoa(*s.ExitCode)
				}
			}
			rows = append(rows, []string{s.ID, strconv.Itoa(s.PID), yesNo(s.TTY), yesNo(s.Attached), status, age(s.StartedAt), strings.Join(s.Cmd, " ")})
		}
		a.table([]string{"ID", "PID", "TTY", "ATTACHED", "STATUS", "AGE", "CMD"}, rows)
	})
}

func cmdKill(a *app, args []string) error {
	fs := a.flags("loam kill")
	sig := fs.String("signal", "TERM", "the signal, by name")
	pos, err := parseArgs(fs, args, -1)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errUsage
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	return c.SignalSession(a.ctx, pos[0], pos[1], strings.TrimPrefix(strings.ToUpper(*sig), "SIG"))
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
