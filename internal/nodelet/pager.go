package nodelet

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/arugula-salad/loam/internal/snapstore"
	"github.com/arugula-salad/loam/internal/snapstore/uffd"
)

// A VM restored from a snapshot whose memory isn't whole on this node (one
// made on another node, or a diff of one) gets its memory lazily: Firecracker
// maps anonymous memory, registers it with a userfaultfd and hands that to a
// pager, which answers each page fault with the page read from the store
// (docs/STORE.md, "The UFFD helper process"). The pager is a process of its
// own, the nodelet re-run as `loam-nodelet uffd-serve`: a Go thread blocked in
// a fault its own process should serve can't be stopped for the garbage
// collector, and a bad fault must not take the nodelet down with it. One per
// restored VM, living exactly as long as its Firecracker; nothing tells it
// Firecracker exited (it holds the userfaultfd open), so the nodelet stops it.

// Pager serves one VM's memory.
type Pager interface {
	// Socket is where Firecracker connects (mem_backend Uffd).
	Socket() string
	// Zeroed returns the ranges of the memory file that read as zeros now
	// because the guest gave them back (the balloon), which Firecracker's
	// dirty log doesn't show: a diff snapshot of this VM records them as zeros
	// rather than inheriting the parent's pages there.
	Zeroed(ctx context.Context) ([]snapstore.Extent, error)
	// Failed is closed when the pager stops serving on its own: a page it
	// could not fetch, so a guest thread is stuck. The VM must be stopped.
	Failed() <-chan struct{}
	Err() error
	// Close stops the pager.
	Close()
}

// Pagers starts pagers.
type Pagers interface {
	Start(ctx context.Context, snapshot, socket string) (Pager, error)
}

// ProcessPagers runs each pager as a helper process: Command(args) is the
// helper, which must end up in RunPager with args.
type ProcessPagers struct {
	Command  func(args []string) *exec.Cmd
	StoreURL string
	CacheDir string
	Log      *slog.Logger
}

// SelfPager is the default Command: this binary's uffd-serve subcommand.
func SelfPager(args []string) *exec.Cmd {
	return exec.Command("/proc/self/exe", append([]string{"uffd-serve"}, args...)...)
}

// pagerStart bounds a helper's start: opening the store and reading a
// snapshot's headers.
const pagerStart = 30 * time.Second

func (p *ProcessPagers) Start(ctx context.Context, snapshot, socket string) (Pager, error) {
	cmd := p.Command([]string{"--store", p.StoreURL, "--cache", p.CacheDir, "--snapshot", snapshot, "--socket", socket})
	// The helper dies with the nodelet: its VM does too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the pager: %w", err)
	}
	pp := &procPager{cmd: cmd, in: in, socket: socket, replies: make(chan pagerMsg, 1), failed: make(chan struct{}), exited: make(chan struct{})}
	ready := make(chan pagerMsg, 1)
	go pp.read(out, ready)
	go func() {
		cmd.Wait()
		close(pp.exited)
	}()
	t := time.NewTimer(pagerStart)
	defer t.Stop()
	select {
	case m := <-ready:
		if m.Error != "" {
			pp.Close()
			return nil, errors.New(m.Error)
		}
		return pp, nil
	case <-pp.failed:
		pp.Close()
		return nil, fmt.Errorf("the pager failed to start: %v", pp.Err())
	case <-t.C:
	case <-ctx.Done():
	}
	pp.Close()
	return nil, errors.New("the pager did not start in time")
}

// pagerMsg is one line of the helper's protocol. The helper writes
// {"ready":true} once it listens on the socket (or {"error"} if it can't),
// answers {"op":"zeroed"} with {"zeroed":[[off,len],...]}, and writes
// {"error"} and exits when it can't serve a page. Its stdin closing stops it.
type pagerMsg struct {
	Op     string     `json:"op,omitempty"`
	Ready  bool       `json:"ready,omitempty"`
	Zeroed [][2]int64 `json:"zeroed,omitempty"`
	Error  string     `json:"error,omitempty"`
}

type procPager struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	socket string

	reqMu   sync.Mutex // one request at a time
	replies chan pagerMsg

	failOnce sync.Once
	failed   chan struct{}
	err      error
	exited   chan struct{}
	close    sync.Once
}

func (p *procPager) read(out io.Reader, ready chan<- pagerMsg) {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	first := true
	for sc.Scan() {
		var m pagerMsg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch {
		case first:
			first = false
			ready <- m
		case m.Error != "":
			p.fail(errors.New(m.Error))
		default:
			select {
			case p.replies <- m:
			default:
			}
		}
	}
	p.fail(errors.New("the pager exited"))
}

func (p *procPager) fail(err error) {
	p.failOnce.Do(func() {
		p.err = err
		close(p.failed)
	})
}

func (p *procPager) Socket() string          { return p.socket }
func (p *procPager) Failed() <-chan struct{} { return p.failed }

func (p *procPager) Err() error {
	select {
	case <-p.failed:
		return p.err
	default:
		return nil
	}
}

func (p *procPager) Zeroed(ctx context.Context) ([]snapstore.Extent, error) {
	p.reqMu.Lock()
	defer p.reqMu.Unlock()
	b, _ := json.Marshal(pagerMsg{Op: "zeroed"})
	if _, err := p.in.Write(append(b, '\n')); err != nil {
		return nil, fmt.Errorf("ask the pager: %w", err)
	}
	select {
	case m := <-p.replies:
		out := make([]snapstore.Extent, len(m.Zeroed))
		for i, z := range m.Zeroed {
			out[i] = snapstore.Extent{Off: z[0], Len: z[1]}
		}
		return out, nil
	case <-p.failed:
		return nil, p.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *procPager) Close() {
	p.close.Do(func() {
		p.in.Close()
		select {
		case <-p.exited:
		case <-time.After(5 * time.Second):
			p.cmd.Process.Kill()
			<-p.exited
		}
		os.Remove(p.socket)
	})
}

// RunPager is the helper: `loam-nodelet uffd-serve --store URL --cache DIR
// --snapshot ID --socket PATH`. It serves snapshot ID's memory, read through
// the store with the node's chunk directory (CacheDir), to the Firecracker
// that connects to PATH, and speaks the protocol of pagerMsg on stdin and
// stdout. The store's credentials come from the environment, as the nodelet's.
func RunPager(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("uffd-serve", flag.ContinueOnError)
	storeURL := fs.String("store", "", "the snapshot store's URL")
	cacheDir := fs.String("cache", "", "the node's store cache directory")
	snapshot := fs.String("snapshot", "", "the snapshot whose memory to serve")
	socket := fs.String("socket", "", "the unix socket Firecracker connects to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var outMu sync.Mutex
	send := func(m pagerMsg) {
		b, _ := json.Marshal(m)
		outMu.Lock()
		stdout.Write(append(b, '\n'))
		outMu.Unlock()
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	l, f, st, err := openPager(ctx, *storeURL, *cacheDir, *snapshot, *socket)
	if err != nil {
		send(pagerMsg{Error: err.Error()})
		return err
	}
	defer st.Close()
	defer f.Close()
	defer l.Close()
	send(pagerMsg{Ready: true})

	var (
		hMu sync.Mutex
		h   *uffd.Handler
	)
	// The nodelet's requests; stdin closing is the order to stop.
	go func() {
		sc := bufio.NewScanner(stdin)
		for sc.Scan() {
			var m pagerMsg
			if json.Unmarshal(sc.Bytes(), &m) != nil || m.Op != "zeroed" {
				continue
			}
			hMu.Lock()
			hh := h
			hMu.Unlock()
			reply := pagerMsg{Zeroed: [][2]int64{}}
			if hh != nil {
				for _, e := range snapstore.BitmapExtents(hh.Zeroed(), hh.PageSize()) {
					reply.Zeroed = append(reply.Zeroed, [2]int64{e.Off, e.Len})
				}
			}
			send(reply)
		}
		cancel()
	}()

	start := time.Now()
	hs, err := uffd.Accept(ctx, l)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		send(pagerMsg{Error: err.Error()})
		return err
	}
	hh, err := uffd.New(hs.FD, hs.Regions, f)
	if err != nil {
		send(pagerMsg{Error: err.Error()})
		return err
	}
	defer hh.Close()
	hMu.Lock()
	h = hh
	hMu.Unlock()
	err = hh.Serve(ctx)
	st0 := st.Stats()
	fmt.Fprintf(os.Stderr, "loam-nodelet uffd-serve: snapshot %s: served %d faults in %v, %d fetches (%d MiB) from the store, %d from the chunk directory\n",
		*snapshot, len(hh.Faults()), time.Since(start).Round(time.Millisecond), st0.Fetches, st0.FetchedBytes>>20, st0.ChunkHits)
	if err != nil {
		send(pagerMsg{Error: err.Error()})
		return err
	}
	return nil
}

func openPager(ctx context.Context, storeURL, cacheDir, snapshot, socket string) (*net.UnixListener, *snapstore.File, *snapstore.Store, error) {
	if storeURL == "" || cacheDir == "" || snapshot == "" || socket == "" {
		return nil, nil, nil, errors.New("uffd-serve needs --store, --cache, --snapshot and --socket")
	}
	st, err := snapstore.Open(storeURL, snapstore.Options{CacheDir: cacheDir})
	if err != nil {
		return nil, nil, nil, err
	}
	gctx, cancel := context.WithTimeout(ctx, pagerStart)
	defer cancel()
	sn, err := st.GetSnapshot(gctx, snapshot)
	if err == nil && sn.Memory == nil {
		err = fmt.Errorf("snapshot %s has no memory", snapshot)
	}
	if err != nil {
		st.Close()
		return nil, nil, nil, err
	}
	f, err := st.OpenFile(sn.Memory, snapstore.Memory, "")
	if err != nil {
		st.Close()
		return nil, nil, nil, err
	}
	os.Remove(socket)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		f.Close()
		st.Close()
		return nil, nil, nil, err
	}
	return l, f, st, nil
}
