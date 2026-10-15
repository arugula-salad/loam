package nodelet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/internal/nodelet/firecracker"
	"github.com/arugula-salad/loam/internal/snapstore"
	"github.com/arugula-salad/loam/internal/snapstore/header"
	"github.com/arugula-salad/loam/internal/snapstore/storage"
)

// The fakes stand in for Firecracker and the guest agent, so the lifecycle,
// the HTTP layer and the persistence run without root, KVM or a network.

// pipeListener hands net.Pipe ends to an http.Server.
type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *pipeListener) Addr() net.Addr { return &net.UnixAddr{Name: "pipe", Net: "unix"} }

func (l *pipeListener) dial(ctx context.Context) (net.Conn, error) {
	a, b := net.Pipe()
	select {
	case l.conns <- b:
		return a, nil
	case <-l.closed:
		return nil, errors.New("connection refused")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// fakeAgent is the guest agent's internal API.
type fakeAgent struct {
	mu     sync.Mutex
	wakes  []nodeapi.Wake
	calls  []string
	noWake bool // answer /internal/wake 404, as sandpit's agent does
}

// handler is the agent of machine m.
func (a *fakeAgent) handler(m *fakeMachine) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/wake", func(w http.ResponseWriter, r *http.Request) {
		a.note(r)
		if a.noWake {
			http.NotFound(w, r)
			return
		}
		var wk nodeapi.Wake
		json.NewDecoder(r.Body).Decode(&wk)
		a.mu.Lock()
		a.wakes = append(a.wakes, wk)
		a.mu.Unlock()
		// Every wake leaves a mark in memory: a resumed guest's memory then
		// differs from its snapshot's, and its next snapshot is a real diff.
		m.mu.Lock()
		m.wakes++
		page := int64(1+m.wakes%64) * pageSize
		m.mu.Unlock()
		m.write(page, "wake of "+wk.Sandbox)
		w.WriteHeader(http.StatusNoContent)
	})
	for _, p := range []string{"/internal/presuspend", "/internal/sync"} {
		mux.HandleFunc("POST "+p, func(w http.ResponseWriter, r *http.Request) {
			a.note(r)
			w.WriteHeader(http.StatusNoContent)
		})
	}
	mux.HandleFunc("POST /internal/poweroff", func(w http.ResponseWriter, r *http.Request) {
		a.note(r)
		w.WriteHeader(http.StatusNoContent)
		go m.exit()
	})
	mux.HandleFunc("GET /internal/activity", func(w http.ResponseWriter, r *http.Request) {
		a.note(r)
		json.NewEncoder(w).Encode(nodeapi.Activity{IdleMS: 1234, SessionsAttached: 1, Tasks: 2, Busy: true})
	})
	mux.HandleFunc("GET /hello", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello from the guest")
	})
	return mux
}

func (a *fakeAgent) note(r *http.Request) {
	a.mu.Lock()
	a.calls = append(a.calls, r.Method+" "+r.URL.Path)
	a.mu.Unlock()
}

func (a *fakeAgent) lastWake() nodeapi.Wake {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.wakes) == 0 {
		return nodeapi.Wake{}
	}
	return a.wakes[len(a.wakes)-1]
}

func (a *fakeAgent) wakeCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.wakes)
}

// fakeMachine is a VM whose agent is a fakeAgent. Its memory is pages: what
// the guest wrote (write) since its boot or restore, with Firecracker's dirty
// log, so a diff snapshot holds exactly the pages written since the last one.
type fakeMachine struct {
	cfg    firecracker.Config
	ln     *pipeListener
	srv    *http.Server
	mu     sync.Mutex
	paused bool
	log    []string
	mem    map[int64]string // page offset -> content; absent is zeros
	dirty  map[int64]bool
	lazy   bool // restored through a pager
	wakes  int
	once   sync.Once
	exited chan struct{}
}

func (m *fakeMachine) memSize() int64 { return int64(m.cfg.MemMiB) << 20 }

// write is the guest writing s at page off.
func (m *fakeMachine) write(off int64, s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mem[off] = s
	m.dirty[off] = true
}

func (m *fakeMachine) read(off int64) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mem[off]
}

func (m *fakeMachine) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	if port != AgentPort {
		return nil, fmt.Errorf("no listener on port %d", port)
	}
	return m.ln.dial(ctx)
}

func (m *fakeMachine) record(s string) {
	m.mu.Lock()
	m.log = append(m.log, s)
	m.mu.Unlock()
}

func (m *fakeMachine) Pause(context.Context) error {
	m.record("pause")
	m.mu.Lock()
	m.paused = true
	m.mu.Unlock()
	return nil
}

func (m *fakeMachine) Resume(context.Context) error {
	m.record("resume")
	m.mu.Lock()
	m.paused = false
	m.mu.Unlock()
	return nil
}
func (m *fakeMachine) Squeeze(context.Context) (int, error) {
	m.record("squeeze")
	return 100, nil
}
func (m *fakeMachine) SetBalloon(_ context.Context, mib int) error {
	m.record(fmt.Sprintf("balloon %d", mib))
	return nil
}

// Snapshot writes the state and the memory as Firecracker does: a full one
// every page (zeros included, here as holes), a diff only the dirty ones.
func (m *fakeMachine) Snapshot(_ context.Context, state, mem string, diff bool) error {
	if diff {
		m.record("snapshot diff")
	} else {
		m.record("snapshot")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.paused {
		return errors.New("snapshot of a running VM")
	}
	if err := os.WriteFile(state, []byte("vmstate of "+m.cfg.ID), 0o644); err != nil {
		return err
	}
	f, err := os.Create(mem)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(m.memSize()); err != nil {
		return err
	}
	for off, s := range m.mem {
		if diff && !m.dirty[off] {
			continue
		}
		page := make([]byte, pageSize)
		copy(page, s)
		if _, err := f.WriteAt(page, off); err != nil {
			return err
		}
	}
	m.dirty = map[int64]bool{}
	return nil
}

func (m *fakeMachine) Kill() {
	m.record("kill")
	m.exit()
}

func (m *fakeMachine) exit() {
	m.once.Do(func() {
		m.srv.Close()
		m.ln.Close()
		close(m.exited)
	})
}

func (m *fakeMachine) Exited() <-chan struct{} { return m.exited }

func (m *fakeMachine) calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.log...)
}

// fakeVMM starts fakeMachines, all with one agent.
type fakeVMM struct {
	agent  *fakeAgent
	pagers *fakePagers
	mu     sync.Mutex
	boots  []firecracker.Config
	// restores are the state files restored from.
	restores []string
	machines []*fakeMachine
	reaped   []string
	failBoot error
}

func (f *fakeVMM) start(cfg firecracker.Config, mem map[int64]string) *fakeMachine {
	m := &fakeMachine{cfg: cfg, ln: newPipeListener(), exited: make(chan struct{}), mem: mem, dirty: map[int64]bool{}}
	m.srv = &http.Server{Handler: f.agent.handler(m)}
	go m.srv.Serve(m.ln)
	f.mu.Lock()
	f.machines = append(f.machines, m)
	f.mu.Unlock()
	return m
}

func (f *fakeVMM) Boot(_ context.Context, cfg firecracker.Config) (Machine, error) {
	if f.failBoot != nil {
		return nil, f.failBoot
	}
	if _, err := os.Stat(filepath.Join(cfg.Dir, firecracker.DiskFile)); err != nil {
		return nil, fmt.Errorf("no disk: %w", err)
	}
	f.mu.Lock()
	f.boots = append(f.boots, cfg)
	f.mu.Unlock()
	m := f.start(cfg, map[int64]string{})
	m.write(0, "booted "+cfg.ID)
	return m, nil
}

func (f *fakeVMM) Restore(_ context.Context, cfg firecracker.Config, state string, mem firecracker.Memory) (Machine, error) {
	for _, p := range []string{state, filepath.Join(cfg.Dir, firecracker.DiskFile)} {
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("restore: %w", err)
		}
	}
	var pages map[int64]string
	var err error
	if mem.Uffd != "" {
		pages, err = f.pagers.connect(mem.Uffd)
	} else {
		pages, err = readPages(mem.File)
	}
	if err != nil {
		return nil, fmt.Errorf("restore: %w", err)
	}
	f.mu.Lock()
	f.restores = append(f.restores, state)
	f.mu.Unlock()
	m := f.start(cfg, pages)
	if m.lazy = mem.Uffd != ""; m.lazy {
		// As KVM does: the pages a pager installed are logged dirty once
		// the guest touches them, written or not.
		for off := range pages {
			m.dirty[off] = true
		}
	}
	return m, nil
}

// readPages reads a memory file's non-zero pages.
func readPages(path string) (map[int64]string, error) {
	es, err := snapstore.DataExtents(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	pages := map[int64]string{}
	page := make([]byte, pageSize)
	for _, e := range es {
		for off := e.Off &^ (pageSize - 1); off < e.Off+e.Len; off += pageSize {
			if err := readFull(f, page, off); err != nil {
				return nil, err
			}
			if s := strings.TrimRight(string(page), "\x00"); s != "" {
				pages[off] = s
			}
		}
	}
	return pages, nil
}

func (f *fakeVMM) ReapOrphan(dir string) {
	f.mu.Lock()
	f.reaped = append(f.reaped, filepath.Base(dir))
	f.mu.Unlock()
}

func (f *fakeVMM) last() *fakeMachine {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.machines[len(f.machines)-1]
}

// fakePagers serve memory from the store as the uffd-serve helper does,
// reading the whole snapshot when Firecracker (the fake) connects.
type fakePagers struct {
	store   *snapstore.Store
	mu      sync.Mutex
	started []string // snapshots
	pagers  map[string]*fakePager
	zeroed  []snapstore.Extent // what every pager reports
}

type fakePager struct {
	ps       *fakePagers
	snapshot string
	socket   string
	failed   chan struct{}
	err      error
	closed   bool
}

func (ps *fakePagers) Start(ctx context.Context, snapshot, socket string) (Pager, error) {
	if _, err := ps.store.GetSnapshot(ctx, snapshot); err != nil {
		return nil, err
	}
	p := &fakePager{ps: ps, snapshot: snapshot, socket: socket, failed: make(chan struct{})}
	ps.mu.Lock()
	ps.started = append(ps.started, snapshot)
	ps.pagers[socket] = p
	ps.mu.Unlock()
	return p, nil
}

// connect is Firecracker's load: the memory it gets, read through the store.
func (ps *fakePagers) connect(socket string) (map[int64]string, error) {
	ps.mu.Lock()
	p := ps.pagers[socket]
	ps.mu.Unlock()
	if p == nil || p.closed {
		return nil, errors.New("no pager on " + socket)
	}
	sn, err := ps.store.GetSnapshot(context.Background(), p.snapshot)
	if err != nil {
		return nil, err
	}
	f, err := ps.store.OpenFile(sn.Memory, snapstore.Memory, "")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	pages := map[int64]string{}
	page := make([]byte, pageSize)
	for _, m := range sn.Memory.Mapping.All() {
		if m.Layer == header.Empty {
			continue
		}
		for off := int64(m.Offset); off < int64(m.Offset+m.Length); off += pageSize {
			if _, err := f.ReadAt(context.Background(), page, off); err != nil {
				return nil, err
			}
			if s := strings.TrimRight(string(page), "\x00"); s != "" {
				pages[off] = s
			}
		}
	}
	return pages, nil
}

func (ps *fakePagers) count() int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return len(ps.started)
}

func (p *fakePager) Socket() string          { return p.socket }
func (p *fakePager) Failed() <-chan struct{} { return p.failed }
func (p *fakePager) Err() error              { return p.err }
func (p *fakePager) Close() {
	p.ps.mu.Lock()
	p.closed = true
	p.ps.mu.Unlock()
}
func (p *fakePager) Zeroed(context.Context) ([]snapstore.Extent, error) {
	p.ps.mu.Lock()
	defer p.ps.mu.Unlock()
	return append([]snapstore.Extent(nil), p.ps.zeroed...), nil
}

// fakeImages makes a template disk per image and size: 64 KiB with the image's
// name in its first block, the same on every node.
type fakeImages struct{ dir string }

func (f fakeImages) Disk(_ context.Context, image string, gib int) (string, string, error) {
	if image == "missing" {
		return "", "", invalid("no such image")
	}
	image = strings.TrimSuffix(image, "@sha256:fake")
	p := filepath.Join(f.dir, fmt.Sprintf("img-%s-%dg.ext4", strings.NewReplacer("/", "_", ":", "_").Replace(image), gib))
	if _, err := os.Stat(p); err != nil {
		b := make([]byte, 64<<10)
		copy(b, "rootfs of "+image)
		copy(b[32<<10:], "more of the template")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return "", "", err
		}
	}
	return p, image + "@sha256:fake", nil
}

// gate holds a store backend's writes while closed, as a slow or away store would.
type gate struct {
	storage.Backend
	mu     sync.Mutex
	open   chan struct{}
	writes int
}

func newGate(b storage.Backend) *gate {
	g := &gate{Backend: b, open: make(chan struct{})}
	close(g.open)
	return g
}

func (g *gate) hold() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if isClosed(g.open) {
		g.open = make(chan struct{})
	}
}

func (g *gate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !isClosed(g.open) {
		close(g.open)
	}
}

func (g *gate) wait(ctx context.Context) error {
	g.mu.Lock()
	ch := g.open
	g.writes++
	g.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *gate) Put(ctx context.Context, key string, data []byte) error {
	if err := g.wait(ctx); err != nil {
		return err
	}
	return g.Backend.Put(ctx, key, data)
}

func (g *gate) NewMultipart(ctx context.Context, key string) (storage.Multipart, error) {
	if err := g.wait(ctx); err != nil {
		return nil, err
	}
	return g.Backend.NewMultipart(ctx, key)
}

// shared is a snapshot store several harnesses (nodes) use.
type shared struct {
	dir  string
	gate *gate
}

func newShared(t *testing.T) *shared {
	dir := t.TempDir()
	b, err := storage.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &shared{dir: dir, gate: newGate(b)}
}

type harness struct {
	t      *testing.T
	n      *Nodelet
	vmm    *fakeVMM
	data   string
	opts   Options
	shared *shared
	store  *snapstore.Store
}

func newHarness(t *testing.T, mod func(*Options)) *harness {
	return newNode(t, "geek", newShared(t), mod)
}

// newNode is a node called name on the store sh.
func newNode(t *testing.T, name string, sh *shared, mod func(*Options)) *harness {
	t.Helper()
	data := t.TempDir()
	opts := Options{Data: data, MemoryMiB: 4096, NodeName: name, Version: "test", Token: "tok",
		DevAllowCopy: true, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if testing.Verbose() {
		opts.Log = slog.New(slog.NewTextHandler(os.Stderr, nil)).With("node", name)
	}
	if mod != nil {
		mod(&opts)
	}
	h := &harness{t: t, vmm: &fakeVMM{agent: &fakeAgent{}}, data: data, opts: opts, shared: sh}
	h.reopen()
	t.Cleanup(func() { h.n.Shutdown(); h.store.Close() })
	return h
}

// reopen starts a new Nodelet over the same data directory, as a restarted pod would.
func (h *harness) reopen() {
	h.t.Helper()
	imgs := filepath.Join(h.data, "images")
	os.MkdirAll(imgs, 0o755)
	st, err := snapstore.New(h.shared.gate, snapstore.Options{CacheDir: filepath.Join(h.data, "store")})
	if err != nil {
		h.t.Fatal(err)
	}
	h.store = st
	h.vmm.pagers = &fakePagers{store: st, pagers: map[string]*fakePager{}}
	h.opts.Store, h.opts.Pagers = st, h.vmm.pagers
	n, err := New(h.opts, h.vmm, nil, fakeImages{dir: imgs})
	if err != nil {
		h.t.Fatal(err)
	}
	h.n = n
}

// stored waits for snapshot id's upload and says whether it is stored.
func (h *harness) stored(id string) bool {
	h.t.Helper()
	if u := h.n.inflight(id); u != nil {
		select {
		case <-u.done:
		case <-time.After(10 * time.Second):
			h.t.Fatalf("snapshot %s: upload still running", id)
		}
	}
	ok, err := h.store.HasSnapshot(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return ok
}

func createReq(id string) nodeapi.CreateVM {
	return nodeapi.CreateVM{ID: id, Template: nodeapi.Template{Name: "base", Image: "ghcr.io/arugula-salad/loam-base:1"},
		Resources: resources(1024), Wake: nodeapi.Wake{Sandbox: id, Name: "scratch", Hostname: "scratch", Env: map[string]string{"FOO": "bar"}}}
}
