package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arugula-salad/loam/pkg/api"
)

// ErrExitUnknown is what Wait returns when the stream ended without an exit
// frame: the sandbox went away under the session (deleted, restored, its node
// lost; close status 1011), or the connection broke. It is never success.
var ErrExitUnknown = errors.New("the session ended without an exit status")

// ExecOptions describe a streaming exec.
type ExecOptions struct {
	Cmd        []string // argv; required
	TTY        bool
	Cols, Rows int // the TTY's initial size
	Cwd        string
	Env        map[string]string // on top of the sandbox's env
	User       string            // default loam
	// Detachable sessions outlive the socket; attach to them again with Attach.
	Detachable bool

	IO
}

// IO wires a stream to the caller. Stdin, when set, is copied in and then
// closed (EOF sent); leave it nil to write with Stream.Write yourself.
// Output for a nil writer is dropped.
type IO struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Stream is a running exec session, from Exec or Attach.
type Stream struct {
	// Session is the session's ID; PID its process (Exec only).
	Session string
	PID     int
	// Offset is where the replay of an attach began.
	Offset int64

	ws   *websocket.Conn
	wmu  sync.Mutex
	io   IO
	done chan struct{}
	code *int
	err  error
	stop func() bool
}

// Exec starts cmd in the sandbox and answers once the session has started.
// The output flows to o.Stdout and o.Stderr until Wait returns.
func (c *Client) Exec(ctx context.Context, sandbox string, o ExecOptions) (*Stream, error) {
	if len(o.Cmd) == 0 {
		return nil, errors.New("exec: no command")
	}
	q := url.Values{"cmd": o.Cmd}
	if o.TTY {
		q.Set("tty", "true")
	}
	if o.Cols > 0 && o.Rows > 0 {
		q.Set("cols", strconv.Itoa(o.Cols))
		q.Set("rows", strconv.Itoa(o.Rows))
	}
	if o.Cwd != "" {
		q.Set("cwd", o.Cwd)
	}
	for _, kv := range envList(o.Env) {
		q.Add("env", kv)
	}
	if o.User != "" {
		q.Set("user", o.User)
	}
	if o.Detachable {
		q.Set("detachable", "true")
	}
	ws, err := c.dial(ctx, sbPath(sandbox, "exec"), q)
	if err != nil {
		return nil, err
	}
	return startStream(ctx, ws, "started", o.IO)
}

// AttachOptions describe an attach. Offset nil replays everything the session
// kept; a value replays from that byte of its output.
type AttachOptions struct {
	Offset *int64
	IO
}

// Attach joins a session again: its kept output is replayed from the offset,
// then it is live.
func (c *Client) Attach(ctx context.Context, sandbox, session string, o AttachOptions) (*Stream, error) {
	q := url.Values{}
	if o.Offset != nil {
		q.Set("offset", strconv.FormatInt(*o.Offset, 10))
	}
	ws, err := c.dial(ctx, sbPath(sandbox, "exec", "sessions", url.PathEscape(session)), q)
	if err != nil {
		return nil, err
	}
	return startStream(ctx, ws, "attached", o.IO)
}

// startStream reads the first frame, which must be want ("started" or
// "attached") or an error, then pumps the rest in the background.
func startStream(ctx context.Context, ws *websocket.Conn, want string, o IO) (*Stream, error) {
	mt, data, err := ws.ReadMessage()
	if err != nil {
		ws.Close()
		return nil, fmt.Errorf("exec: reading the first frame: %w", err)
	}
	var m api.ExecMessage
	if mt != websocket.TextMessage || json.Unmarshal(data, &m) != nil {
		ws.Close()
		return nil, fmt.Errorf("exec: the first frame is not a %q message", want)
	}
	if m.Type == "error" && m.Error != nil {
		ws.Close()
		return nil, &Error{Status: http.StatusBadRequest, Code: m.Error.Code, Message: m.Error.Message}
	}
	if m.Type != want {
		ws.Close()
		return nil, fmt.Errorf("exec: first frame %q, want %q", m.Type, want)
	}
	s := &Stream{Session: m.Session, PID: m.PID, Offset: m.Offset, ws: ws, io: o, done: make(chan struct{})}
	s.stop = context.AfterFunc(ctx, func() { ws.Close() })
	go s.read()
	if o.Stdin != nil {
		go s.copyStdin(o.Stdin)
	}
	return s, nil
}

func (s *Stream) read() {
	defer close(s.done)
	defer s.stop()
	for {
		mt, data, err := s.ws.ReadMessage()
		if err != nil {
			switch {
			case s.code != nil || s.err != nil:
				// The exit (or error) frame arrived; how the socket went after it doesn't matter.
			case websocket.IsCloseError(err, websocket.CloseInternalServerErr):
				s.err = fmt.Errorf("%w (the sandbox went away)", ErrExitUnknown)
			default:
				s.err = fmt.Errorf("%w: %v", ErrExitUnknown, err)
			}
			s.ws.Close()
			return
		}
		switch mt {
		case websocket.BinaryMessage:
			if len(data) == 0 {
				continue
			}
			var w io.Writer
			switch data[0] {
			case api.StreamStdout:
				w = s.io.Stdout
			case api.StreamStderr:
				w = s.io.Stderr
			}
			if w != nil {
				w.Write(data[1:])
			}
		case websocket.TextMessage:
			var m api.ExecMessage
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			switch m.Type {
			case "exit":
				if m.Code != nil {
					code := *m.Code
					s.code = &code
				}
			case "error":
				if m.Error != nil {
					s.err = &Error{Status: http.StatusInternalServerError, Code: m.Error.Code, Message: m.Error.Message}
				}
			}
		}
	}
}

func (s *Stream) copyStdin(r io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := s.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			s.CloseStdin()
			return
		}
	}
}

func (s *Stream) send(mt int, data []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.ws.WriteMessage(mt, data)
}

// Write sends stdin bytes.
func (s *Stream) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	frame := make([]byte, 1+len(p))
	frame[0] = api.StreamStdin
	copy(frame[1:], p)
	if err := s.send(websocket.BinaryMessage, frame); err != nil {
		return 0, err
	}
	return len(p), nil
}

// CloseStdin sends EOF on stdin.
func (s *Stream) CloseStdin() error {
	return s.send(websocket.BinaryMessage, []byte{api.StreamStdinEOF})
}

// Resize sets the TTY's size.
func (s *Stream) Resize(cols, rows int) error {
	b, _ := json.Marshal(api.ExecMessage{Type: "resize", Cols: cols, Rows: rows})
	return s.send(websocket.TextMessage, b)
}

// Signal signals the session's process group; sig is a name as in `kill -l`
// ("TERM", "INT", "KILL").
func (s *Stream) Signal(sig string) error {
	b, _ := json.Marshal(api.ExecMessage{Type: "signal", Signal: sig})
	return s.send(websocket.TextMessage, b)
}

// Done is closed when the stream has ended.
func (s *Stream) Done() <-chan struct{} { return s.done }

// Wait waits for the stream to end and returns the exit code. A killed
// process exits 128+signal. Without an exit frame the error is ErrExitUnknown.
func (s *Stream) Wait() (int, error) {
	<-s.done
	if s.err != nil {
		return -1, s.err
	}
	if s.code == nil {
		return -1, ErrExitUnknown
	}
	return *s.code, nil
}

// Close closes the socket: a detachable session goes on without it, any
// other is hung up (SIGHUP to its process group).
func (s *Stream) Close() error {
	s.wmu.Lock()
	s.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	s.wmu.Unlock()
	err := s.ws.Close()
	<-s.done
	return err
}

// envList is env as sorted K=V pairs, so requests are reproducible.
func envList(env map[string]string) []string {
	l := make([]string, 0, len(env))
	for k, v := range env {
		l = append(l, k+"="+v)
	}
	sort.Strings(l)
	return l
}

// Sessions lists the sandbox's exec sessions, running and recently exited.
func (c *Client) Sessions(ctx context.Context, sandbox string) ([]api.Session, error) {
	var l api.SessionList
	return l.Sessions, c.do(ctx, http.MethodGet, sbPath(sandbox, "exec", "sessions"), nil, nil, &l)
}

// SignalSession signals a session's process group without a stream to it.
func (c *Client) SignalSession(ctx context.Context, sandbox, session, sig string) error {
	return c.do(ctx, http.MethodPost, sbPath(sandbox, "exec", "sessions", url.PathEscape(session), "signal"), nil, api.Signal{Signal: sig}, nil)
}

// Run is the one-shot exec: it answers when the command has exited (or timed
// out) with all of its output.
func (c *Client) Run(ctx context.Context, sandbox string, req api.ExecRun) (*api.ExecResult, error) {
	var r api.ExecResult
	return &r, c.do(ctx, http.MethodPost, sbPath(sandbox, "exec", "run"), nil, req, &r)
}
