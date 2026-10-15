package agent

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/sys/unix"

	"github.com/arugula-salad/loam/pkg/api"
)

const (
	replayBufferBytes  = 1 << 20 // output retained per session for late attach
	clientHighWater    = 4 << 20 // pause reading process output while a client is this far behind
	exitedSessionTTL   = 10 * time.Minute
	outputDrainTimeout = 500 * time.Millisecond
)

// sessionOpts describes a new exec session; the server has resolved the
// account and the environment.
type sessionOpts struct {
	cmd        []string
	env        []string
	dir        string
	acct       *account
	tty        bool
	cols, rows uint16
	detachable bool
}

type frame struct {
	stream byte
	data   []byte
	end    int64 // cumulative output payload bytes through this frame
}

type wsMsg struct {
	typ   int
	data  []byte
	close bool
}

// client is one attached WebSocket. Its queue never blocks the producer;
// backpressure is applied in the session's output reader instead.
type client struct {
	mu     sync.Mutex
	q      []wsMsg
	qBytes int
	notify chan struct{}
	closed chan struct{}
	once   sync.Once
}

func newClient() *client {
	return &client{notify: make(chan struct{}, 1), closed: make(chan struct{})}
}

func (c *client) enqueue(m wsMsg) {
	c.mu.Lock()
	c.q = append(c.q, m)
	c.qBytes += len(m.data)
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

func (c *client) enqueueJSON(v any) {
	b, _ := json.Marshal(v)
	c.enqueue(wsMsg{typ: websocket.TextMessage, data: b})
}

func (c *client) backlog() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.qBytes
}

func (c *client) shutdown() { c.once.Do(func() { close(c.closed) }) }

// drain writes queued messages through send until it reaches a close marker
// (true), or the client is shut down or send fails (false).
func (c *client) drain(send func(typ int, data []byte) error) (finished bool) {
	for {
		select {
		case <-c.notify:
		case <-c.closed:
			return false
		}
		for {
			c.mu.Lock()
			if len(c.q) == 0 {
				c.mu.Unlock()
				break
			}
			m := c.q[0]
			c.q = c.q[1:]
			c.qBytes -= len(m.data)
			c.mu.Unlock()
			if m.close {
				return true
			}
			if err := send(m.typ, m.data); err != nil {
				return false
			}
		}
	}
}

// Session is a running (or recently exited) command.
type Session struct {
	ID         string
	Cmd        []string
	TTY        bool
	Detachable bool
	StartedAt  time.Time
	PID        int

	mgr *Manager
	cmd *exec.Cmd

	mu       sync.Mutex
	ptmx     *os.File
	stdin    io.WriteCloser
	frames   []frame
	bufBytes int
	total    int64
	clients  map[*client]struct{}
	exited   bool
	exitCode int
	done     chan struct{}
}

// Manager owns all sessions in the guest.
type Manager struct {
	mu           sync.Mutex
	sessions     map[string]*Session
	lastActivity time.Time
}

func newManager() *Manager {
	return &Manager{sessions: map[string]*Session{}, lastActivity: time.Now()}
}

// touch records activity. lastActivity keeps Go's monotonic reading, so the
// time a VM spent paused (when the monotonic clock stood still) is not idle time.
func (m *Manager) touch() {
	m.mu.Lock()
	m.lastActivity = time.Now()
	m.mu.Unlock()
}

// activity reports when anything last happened and how many live sessions
// have a client attached.
func (m *Manager) activity() (last time.Time, attached int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		s.mu.Lock()
		if !s.exited && len(s.clients) > 0 {
			attached++
		}
		s.mu.Unlock()
	}
	return m.lastActivity, attached
}

func (m *Manager) get(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

func (m *Manager) list() []api.Session {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	out := make([]api.Session, 0, len(all))
	for _, s := range all {
		out = append(out, s.info())
	}
	sortSessions(out)
	return out
}

func sortSessions(ss []api.Session) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j].StartedAt.Before(ss[j-1].StartedAt); j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
}

func (s *Session) info() api.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := api.Session{ID: s.ID, Cmd: s.Cmd, TTY: s.TTY, PID: s.PID, StartedAt: s.StartedAt.UTC(),
		Exited: s.exited, Attached: len(s.clients) > 0}
	if s.exited {
		code := s.exitCode
		out.ExitCode = &code
	}
	return out
}

// killAll ends every session; for tests.
func (m *Manager) killAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		s.signal(unix.SIGKILL)
	}
}

// newSessionID is "s-" and 10 lowercase base32 characters.
func newSessionID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	b := make([]byte, 10)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[b[i]%32]
	}
	return "s-" + string(b)
}

// start launches a command and registers the session.
func (m *Manager) start(o sessionOpts) (*Session, error) {
	path, err := lookPath(o.cmd[0], o.env)
	if err != nil {
		return nil, err
	}
	cmd := &exec.Cmd{Path: path, Args: o.cmd, Env: o.env, Dir: o.dir}
	now := time.Now()
	s := &Session{
		ID: newSessionID(), Cmd: o.cmd, TTY: o.tty, Detachable: o.detachable, StartedAt: now,
		mgr: m, cmd: cmd, clients: map[*client]struct{}{}, done: make(chan struct{}),
	}

	var readers sync.WaitGroup
	if o.tty {
		cols, rows := o.cols, o.rows
		if cols == 0 {
			cols = 80
		}
		if rows == 0 {
			rows = 24
		}
		ptmx, tty, err := openPTY(cols, rows)
		if err != nil {
			return nil, err
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
		// A session of its own with the terminal as its controlling tty, which
		// also makes the child a process group leader.
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: o.acct.credential(), Setsid: true, Setctty: true, Ctty: 0}
		err = cmd.Start()
		tty.Close()
		if err != nil {
			ptmx.Close()
			return nil, err
		}
		s.ptmx = ptmx
		readers.Add(1)
		go s.pump(ptmx, api.StreamStdout, &readers)
	} else {
		// setsid rather than only setpgid: every session is its own process
		// group and session (docs/AGENT.md), so a signal reaches what it started.
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: o.acct.credential(), Setsid: true}
		outR, outW, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		errR, errW, err := os.Pipe()
		if err != nil {
			outR.Close()
			outW.Close()
			return nil, err
		}
		inR, inW, err := os.Pipe()
		if err != nil {
			outR.Close()
			outW.Close()
			errR.Close()
			errW.Close()
			return nil, err
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
		err = cmd.Start()
		outW.Close()
		errW.Close()
		inR.Close()
		if err != nil {
			outR.Close()
			errR.Close()
			inW.Close()
			return nil, err
		}
		s.stdin = inW
		readers.Add(2)
		go s.pump(outR, api.StreamStdout, &readers)
		go s.pump(errR, api.StreamStderr, &readers)
	}
	s.PID = cmd.Process.Pid

	m.mu.Lock()
	m.sessions[s.ID] = s
	m.lastActivity = now
	m.mu.Unlock()

	go s.wait(&readers)
	return s, nil
}

// pump copies process output into the replay buffer and to attached clients.
func (s *Session) pump(r *os.File, stream byte, wg *sync.WaitGroup) {
	defer wg.Done()
	defer r.Close()
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			s.broadcast(stream, data)
			s.throttle()
		}
		if err != nil {
			return // EOF, or EIO from a pty whose other end has gone
		}
	}
}

func outMsg(stream byte, data []byte) wsMsg {
	return wsMsg{typ: websocket.BinaryMessage, data: append([]byte{stream}, data...)}
}

func (s *Session) broadcast(stream byte, data []byte) {
	s.mu.Lock()
	s.total += int64(len(data))
	s.frames = append(s.frames, frame{stream: stream, data: data, end: s.total})
	s.bufBytes += len(data)
	for s.bufBytes > replayBufferBytes && len(s.frames) > 1 {
		s.bufBytes -= len(s.frames[0].data)
		s.frames = s.frames[1:]
	}
	msg := outMsg(stream, data)
	for c := range s.clients {
		c.enqueue(msg)
	}
	s.mu.Unlock()
	s.mgr.touch()
}

// throttle blocks the output reader while any attached client is far behind.
func (s *Session) throttle() {
	for {
		slow := false
		s.mu.Lock()
		for c := range s.clients {
			if c.backlog() > clientHighWater {
				slow = true
			}
		}
		s.mu.Unlock()
		if !slow {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// exitCode is the code docs/API.md reports for a finished command: its exit
// status, or 128+signal when a signal killed it.
func exitCode(cmd *exec.Cmd, err error) int {
	if ps := cmd.ProcessState; ps != nil {
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ps.ExitCode()
	}
	if err != nil {
		return 1
	}
	return 0
}

func (s *Session) wait(readers *sync.WaitGroup) {
	code := exitCode(s.cmd, s.cmd.Wait())

	// Let output drain, but don't hang on a grandchild that kept the pipe open.
	drained := make(chan struct{})
	go func() { readers.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(outputDrainTimeout):
	}
	s.mu.Lock()
	if s.ptmx != nil {
		s.ptmx.Close()
	}
	if s.stdin != nil {
		s.stdin.Close()
		s.stdin = nil
	}
	s.exited, s.exitCode = true, code
	for c := range s.clients {
		for _, m := range s.exitMsgs() {
			c.enqueue(m)
		}
	}
	s.mu.Unlock()
	close(s.done)
	s.mgr.touch()

	time.AfterFunc(exitedSessionTTL, func() {
		s.mgr.mu.Lock()
		delete(s.mgr.sessions, s.ID)
		s.mgr.mu.Unlock()
	})
}

func (s *Session) exitMsgs() []wsMsg {
	code := s.exitCode
	b, _ := json.Marshal(api.ExecMessage{Type: "exit", Code: &code})
	return []wsMsg{{typ: websocket.TextMessage, data: b}, {close: true}}
}

// attach registers a client, replaying buffered output from offset first so
// there is no gap or duplicate between replay and live output. first is the
// frame that opens the stream (started or attached), which the caller builds
// from the offset the replay really starts at.
func (s *Session) attach(c *client, offset int64, first func(start int64) any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	oldest := s.total - int64(s.bufBytes)
	start := min(max(offset, oldest), s.total)
	c.enqueueJSON(first(start))
	for _, f := range s.frames {
		if f.end <= start {
			continue
		}
		data := f.data
		if fs := f.end - int64(len(f.data)); start > fs {
			data = data[start-fs:]
		}
		c.enqueue(outMsg(f.stream, data))
	}
	if s.exited {
		for _, m := range s.exitMsgs() {
			c.enqueue(m)
		}
		return
	}
	s.clients[c] = struct{}{}
}

func (s *Session) detach(c *client) {
	s.mu.Lock()
	delete(s.clients, c)
	s.mu.Unlock()
}

func (s *Session) writeStdin(p []byte) {
	s.mu.Lock()
	var w io.Writer
	if s.ptmx != nil {
		w = s.ptmx
	} else if s.stdin != nil {
		w = s.stdin
	}
	s.mu.Unlock()
	s.mgr.touch()
	if w != nil {
		w.Write(p)
	}
}

// closeStdin is EOF on stdin. A terminal has no EOF of its own to give, so
// it gets the EOF character, which is what a user typing ^D sends.
func (s *Session) closeStdin() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptmx != nil {
		s.ptmx.Write([]byte{4})
		return
	}
	if s.stdin != nil {
		s.stdin.Close()
		s.stdin = nil
	}
}

func (s *Session) resize(cols, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptmx == nil || cols <= 0 || rows <= 0 || cols > 65535 || rows > 65535 {
		return
	}
	setWinsize(s.ptmx, uint16(cols), uint16(rows))
}

// signal delivers sig to the session's whole process group.
func (s *Session) signal(sig syscall.Signal) error {
	return syscall.Kill(-s.PID, sig)
}

func (s *Session) hasExited() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exited
}

// parseSignal accepts "TERM", "SIGTERM" or a number, as kill -l names them.
func parseSignal(name string) (syscall.Signal, error) {
	if name == "" {
		return 0, errors.New("signal is required")
	}
	if n, err := strconv.Atoi(name); err == nil {
		if n <= 0 || n > 64 {
			return 0, fmt.Errorf("unknown signal %q", name)
		}
		return syscall.Signal(n), nil
	}
	name = strings.ToUpper(name)
	if !strings.HasPrefix(name, "SIG") {
		name = "SIG" + name
	}
	if sig := unix.SignalNum(name); sig != 0 {
		return sig, nil
	}
	return 0, fmt.Errorf("unknown signal %q", name)
}
