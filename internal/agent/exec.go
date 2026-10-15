package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arugula-salad/loam/pkg/api"
)

const (
	runDefaultTimeout = 60 * time.Second
	runMaxTimeout     = 3600 * time.Second
	runMaxOutput      = 16 << 20 // per stream; beyond it the output is cut
)

func (s *Server) registerExec(mux *http.ServeMux) {
	mux.HandleFunc("GET /exec", s.handleExec)
	mux.HandleFunc("GET /exec/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, api.SessionList{Sessions: s.sessions.list()})
	})
	mux.HandleFunc("GET /exec/sessions/{session}", s.handleAttach)
	mux.HandleFunc("POST /exec/sessions/{session}/signal", s.handleSessionSignal)
	mux.HandleFunc("POST /exec/run", s.handleRun)
}

// execOpts reads the query of GET /exec. Every parameter that is present
// must make sense: a typo is an error, not a default.
func (s *Server) execOpts(q url.Values) (sessionOpts, error) {
	var o sessionOpts
	if len(q["cmd"]) == 0 || q["cmd"][0] == "" {
		return o, errors.New("cmd is required")
	}
	o.cmd = q["cmd"]
	var err error
	if o.tty, err = parseBool(q.Get("tty"), false); err != nil {
		return o, errors.New("tty must be true or false")
	}
	if o.detachable, err = parseBool(q.Get("detachable"), false); err != nil {
		return o, errors.New("detachable must be true or false")
	}
	for _, k := range []string{"cols", "rows"} {
		if v := q.Get(k); v != "" {
			n, err := strconv.ParseUint(v, 10, 16)
			if err != nil || n == 0 {
				return o, errors.New(k + " must be a positive integer")
			}
			if k == "cols" {
				o.cols = uint16(n)
			} else {
				o.rows = uint16(n)
			}
		}
	}
	var call []string
	for _, kv := range q["env"] {
		if k, _, ok := strings.Cut(kv, "="); !ok || k == "" {
			return o, errors.New("env must be K=V")
		}
		call = append(call, kv)
	}
	if o.acct, err = s.account(q.Get("user")); err != nil {
		return o, err
	}
	o.env = s.processEnv(o.acct, o.tty, call)
	o.dir = q.Get("cwd")
	if o.dir == "" {
		o.dir = o.acct.home
	}
	return o, nil
}

// accountErr answers a user that can't be had.
func accountErr(w http.ResponseWriter, err error) {
	if errors.Is(err, errUnsupportedUser) {
		writeErr(w, http.StatusNotImplemented, api.CodeUnsupported, err.Error())
		return
	}
	writeErr(w, http.StatusBadRequest, api.CodeInvalid, err.Error())
}

// handleExec is the streaming exec (docs/API.md, "Exec"). A bad request is
// answered before the upgrade; a command that can't start gets an error frame.
func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		writeErr(w, http.StatusBadRequest, api.CodeInvalid, "exec is a WebSocket; see /exec/run for one-shot commands")
		return
	}
	o, err := s.execOpts(r.URL.Query())
	if err != nil {
		accountErr(w, err)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	sess, err := s.sessions.start(o)
	if err != nil {
		b, _ := json.Marshal(api.ExecMessage{Type: "error", Error: &api.ErrorDetail{Code: api.CodeInvalid, Message: err.Error()}})
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		conn.WriteMessage(websocket.TextMessage, b)
		closeWS(conn, websocket.CloseNormalClosure, "")
		return
	}
	started := func(int64) any { return api.ExecMessage{Type: "started", Session: sess.ID, PID: sess.PID} }
	s.serveWS(conn, sess, 0, started, true)
}

func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		writeErr(w, http.StatusBadRequest, api.CodeInvalid, "attach is a WebSocket; GET /exec/sessions lists them")
		return
	}
	sess := s.sessions.get(r.PathValue("session"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, api.CodeNotFound, "no exec session "+r.PathValue("session"))
		return
	}
	var offset int64
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, api.CodeInvalid, "offset must be a non-negative integer")
			return
		}
		offset = n
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	attached := func(start int64) any {
		return api.ExecAttached{Type: "attached", Session: sess.ID, Offset: start}
	}
	s.serveWS(conn, sess, offset, attached, false)
}

func closeWS(conn *websocket.Conn, code int, text string) {
	conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, text), time.Now().Add(time.Second))
	// Give the peer a moment to read the close before the teardown.
	time.Sleep(50 * time.Millisecond)
}

// serveWS is the exec wire protocol for one attached peer: it replays and
// streams the session's output and applies the peer's frames (stdin, EOF,
// resize, signal) until the session ends or the peer goes. When the owner of
// a session that is not detachable goes, the session ends with it.
func (s *Server) serveWS(conn *websocket.Conn, sess *Session, offset int64, first func(int64) any, owner bool) {
	c := newClient()
	sess.attach(c, offset, first)
	defer sess.detach(c)
	defer c.shutdown()

	go func() {
		defer c.shutdown()
		for {
			typ, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			s.sessionInput(sess, typ, data)
		}
	}()

	send := func(typ int, data []byte) error {
		conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		return conn.WriteMessage(typ, data)
	}
	if c.drain(send) {
		closeWS(conn, websocket.CloseNormalClosure, "")
		return
	}
	if owner && !sess.Detachable && !sess.hasExited() {
		sess.signal(syscall.SIGHUP)
	}
}

// sessionInput applies one frame from an attached peer.
func (s *Server) sessionInput(sess *Session, typ int, data []byte) {
	switch typ {
	case websocket.BinaryMessage:
		if len(data) == 0 {
			return
		}
		switch data[0] {
		case api.StreamStdin:
			sess.writeStdin(data[1:])
		case api.StreamStdinEOF:
			sess.closeStdin()
		}
	case websocket.TextMessage:
		var m api.ExecMessage
		if json.Unmarshal(data, &m) != nil {
			return
		}
		switch m.Type {
		case "resize":
			sess.resize(m.Cols, m.Rows)
		case "signal":
			if sig, err := parseSignal(m.Signal); err == nil {
				sess.signal(sig)
			}
		}
	}
}

func (s *Server) handleSessionSignal(w http.ResponseWriter, r *http.Request) {
	sess := s.sessions.get(r.PathValue("session"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, api.CodeNotFound, "no exec session "+r.PathValue("session"))
		return
	}
	var body api.Signal
	if !decodeBody(w, r, 1<<16, &body) {
		return
	}
	sig, err := parseSignal(body.Signal)
	if err != nil {
		writeErr(w, http.StatusBadRequest, api.CodeInvalid, err.Error())
		return
	}
	if sess.hasExited() {
		writeErr(w, http.StatusConflict, api.CodeConflict, "the session has exited")
		return
	}
	if err := sess.signal(sig); err != nil {
		writeErr(w, http.StatusConflict, api.CodeConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// capped keeps the first max bytes written to it and says whether it cut
// anything; a write is activity.
type capped struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	max   int
	cut   bool
	touch func()
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - c.buf.Len(); room < len(p) {
		c.buf.Write(p[:max(room, 0)])
		c.cut = true
	} else {
		c.buf.Write(p)
	}
	c.touch()
	return len(p), nil
}

// handleRun is the one-shot exec: run to completion, or to the timeout, when
// the process group is killed.
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	var req api.ExecRun
	if !decodeBody(w, r, 64<<20, &req) {
		return
	}
	if len(req.Cmd) == 0 || req.Cmd[0] == "" {
		writeErr(w, http.StatusBadRequest, api.CodeInvalid, "cmd is required")
		return
	}
	timeout := runDefaultTimeout
	if req.TimeoutS != 0 {
		if req.TimeoutS < 0 || time.Duration(req.TimeoutS)*time.Second > runMaxTimeout {
			writeErr(w, http.StatusBadRequest, api.CodeInvalid, "timeout_s must be between 1 and 3600")
			return
		}
		timeout = time.Duration(req.TimeoutS) * time.Second
	}
	call, err := envList(req.Env)
	if err != nil {
		writeErr(w, http.StatusBadRequest, api.CodeInvalid, err.Error())
		return
	}
	acct, err := s.account(req.User)
	if err != nil {
		accountErr(w, err)
		return
	}
	env := s.processEnv(acct, false, call)
	path, err := lookPath(req.Cmd[0], env)
	if err != nil {
		writeErr(w, http.StatusBadRequest, api.CodeInvalid, err.Error())
		return
	}
	dir := req.Cwd
	if dir == "" {
		dir = acct.home
	}
	stdout := &capped{max: runMaxOutput, touch: s.touch}
	stderr := &capped{max: runMaxOutput, touch: s.touch}
	cmd := &exec.Cmd{Path: path, Args: req.Cmd, Env: env, Dir: dir, Stdout: stdout, Stderr: stderr,
		Stdin:       bytes.NewReader(req.Stdin),
		SysProcAttr: &syscall.SysProcAttr{Credential: acct.credential(), Setsid: true}}
	// Once the command is done, a grandchild still holding its output open
	// must not hold up the answer.
	cmd.WaitDelay = outputDrainTimeout
	s.touch()
	if err := cmd.Start(); err != nil {
		writeErr(w, http.StatusBadRequest, api.CodeInvalid, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var res api.ExecResult
	var werr error
	select {
	case werr = <-waited:
	case <-ctx.Done():
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		werr = <-waited
		res.TimedOut = r.Context().Err() == nil
	}
	s.touch()
	if r.Context().Err() != nil {
		return // the caller went away; the process group is gone with it
	}
	res.ExitCode = exitCode(cmd, werr)
	// Empty output is "", not null: append to a non-nil slice.
	res.Stdout = append([]byte{}, stdout.buf.Bytes()...)
	res.Stderr = append([]byte{}, stderr.buf.Bytes()...)
	res.Truncated = stdout.cut || stderr.cut
	writeJSON(w, http.StatusOK, res)
}
