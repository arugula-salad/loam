// loam-agent is the runtime in every sandbox VM (docs/AGENT.md). As PID 1,
// booted from the initramfs, it sets the system up and supervises a copy of
// itself running `serve`, which serves the agent's HTTP API on vsock port
// 1024. Splitting the two keeps PID 1's wait4(-1) orphan reaping from
// stealing exit statuses that os/exec is waiting on in the server.
//
// `loam-agent serve --listen tcp:127.0.0.1:7000` runs the server alone on a
// host, as the agent's own user, for trying the API by hand.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arugula-salad/loam/internal/agent"
	"github.com/arugula-salad/loam/internal/nodeapi"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("loam-agent: ")
	if os.Getpid() == 1 {
		runInit()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		serve(os.Args[2:])
		return
	}
	fmt.Fprintln(os.Stderr, "usage: loam-agent serve [--listen vsock|tcp:ADDR|unix:PATH]")
	os.Exit(2)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "vsock", "vsock, or tcp:HOST:PORT or unix:PATH for trying the agent on a host")
	stateDir := fs.String("state-dir", agent.DefaultStateDir, "services, their logs and hooks (on the sandbox's disk)")
	runDir := fs.String("run-dir", agent.DefaultRunDir, "pid files and the last wake; must not survive a reboot")
	guestSock := fs.String("guest-socket", "", "the guest API socket (default <run-dir>/api.sock in a VM, none otherwise)")
	fs.Parse(args)

	inVM := *listen == "vsock"
	var ln net.Listener
	var err error
	switch {
	case inVM:
		ln, err = agent.ListenVsock(agent.AgentPort)
	case strings.HasPrefix(*listen, "tcp:"):
		ln, err = net.Listen("tcp", strings.TrimPrefix(*listen, "tcp:"))
	case strings.HasPrefix(*listen, "unix:"):
		ln, err = net.Listen("unix", strings.TrimPrefix(*listen, "unix:"))
	default:
		log.Fatalf("bad --listen %q", *listen)
	}
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	cfg := agent.Config{StateDir: *stateDir, RunDir: *runDir, RunAsSelf: os.Getuid() != 0}
	if inVM {
		cfg.System = agent.NewSystem()
		cfg.HostDial = func(ctx context.Context) (net.Conn, error) {
			return agent.DialVsock(ctx, agent.HostCID, agent.HostPort)
		}
		if *guestSock == "" {
			*guestSock = *runDir + "/api.sock"
		}
	} else {
		// On a host the agent must not set the host's clock or network.
		cfg.System = hostSystem{}
		cfg.TrustRoot = filepath.Join(*stateDir, "root")
	}
	srv := agent.New(cfg)

	if *guestSock != "" {
		if gl, err := agent.ListenGuest(*guestSock); err != nil {
			log.Printf("%s: %v", *guestSock, err)
		} else {
			gs := &http.Server{Handler: srv.GuestHandler(), ReadHeaderTimeout: 10 * time.Second}
			go func() { log.Fatal(gs.Serve(gl)) }()
		}
	}
	log.Printf("serving on %s", *listen)
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(hs.Serve(ln))
}

// hostSystem is the System of an agent tried out on a host: the internal
// routes that would change the machine answer an error instead.
type hostSystem struct{}

var errHost = fmt.Errorf("not in a VM")

func (hostSystem) SetClock(time.Time) error                    { return errHost }
func (hostSystem) SetSandboxID(string) error                   { return errHost }
func (hostSystem) SetHostname(string) error                    { return errHost }
func (hostSystem) ConfigureNetwork(*nodeapi.WakeNetwork) error { return errHost }
func (hostSystem) DropCaches()                                 {}
func (hostSystem) Sync()                                       {}
func (hostSystem) Poweroff()                                   { os.Exit(0) }
