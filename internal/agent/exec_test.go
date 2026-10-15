package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arugula-salad/loam/pkg/api"
)

type execResult struct {
	first          api.ExecMessage
	stdout, stderr bytes.Buffer
	exit           *int
	errFrame       *api.ErrorDetail
	closeCode      int
}

// collect reads an exec or attach socket to the end.
func collect(t *testing.T, conn *websocket.Conn) *execResult {
	t.Helper()
	r := &execResult{closeCode: -1}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for i := 0; ; i++ {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			if ce, ok := err.(*websocket.CloseError); ok {
				r.closeCode = ce.Code
			}
			return r
		}
		if typ == websocket.TextMessage {
			var m api.ExecMessage
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatalf("bad text frame %q", data)
			}
			switch {
			case m.Type == "exit":
				r.exit = m.Code
			case i == 0:
				r.first = m
				if m.Type == "error" {
					r.errFrame = m.Error
				}
			}
			continue
		}
		switch data[0] {
		case api.StreamStdout:
			r.stdout.Write(data[1:])
		case api.StreamStderr:
			r.stderr.Write(data[1:])
		default:
			t.Fatalf("unexpected stream byte %d", data[0])
		}
	}
}

func execQuery(cmd ...string) url.Values {
	return url.Values{"cmd": cmd}
}

func TestExecStreams(t *testing.T) {
	h := newHarness(t)
	conn := h.dial("/exec", execQuery("sh", "-c", "echo out; echo err >&2; exit 3"))
	r := collect(t, conn)
	if r.first.Type != "started" || !strings.HasPrefix(r.first.Session, "s-") || r.first.PID == 0 {
		t.Fatalf("first frame %+v", r.first)
	}
	if r.stdout.String() != "out\n" || r.stderr.String() != "err\n" {
		t.Fatalf("stdout %q stderr %q", r.stdout.String(), r.stderr.String())
	}
	if r.exit == nil || *r.exit != 3 || r.closeCode != websocket.CloseNormalClosure {
		t.Fatalf("exit %v close %d", r.exit, r.closeCode)
	}
}

func TestExecStdinAndEOF(t *testing.T) {
	h := newHarness(t)
	conn := h.dial("/exec", execQuery("cat"))
	conn.WriteMessage(websocket.BinaryMessage, append([]byte{api.StreamStdin}, "hello"...))
	conn.WriteMessage(websocket.BinaryMessage, []byte{api.StreamStdinEOF})
	r := collect(t, conn)
	if r.stdout.String() != "hello" || r.exit == nil || *r.exit != 0 {
		t.Fatalf("stdout %q exit %v", r.stdout.String(), r.exit)
	}
}

func TestExecSignalIsExitCode(t *testing.T) {
	h := newHarness(t)
	conn := h.dial("/exec", execQuery("sleep", "30"))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	conn.ReadMessage() // started
	conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"signal","signal":"TERM"}`))
	r := collect(t, conn)
	if r.exit == nil || *r.exit != 128+15 {
		t.Fatalf("exit %v, want 143", r.exit)
	}
}

func TestExecTTY(t *testing.T) {
	h := newHarness(t)
	q := execQuery("sh", "-c", "stty size; echo $TERM; test -t 0 && echo tty")
	q.Set("tty", "true")
	q.Set("cols", "100")
	q.Set("rows", "30")
	r := collect(t, h.dial("/exec", q))
	out := strings.ReplaceAll(r.stdout.String(), "\r", "")
	if out != "30 100\nxterm-256color\ntty\n" || r.stderr.Len() != 0 {
		t.Fatalf("tty output %q (stderr %q)", out, r.stderr.String())
	}
}

func TestExecRefusals(t *testing.T) {
	h := newHarness(t)
	// Bad parameters are refused before the upgrade, in the error shape.
	h.wantStatus(h.do("GET", "/exec", nil, nil), 400, api.CodeInvalid)
	// A user this agent can't switch to (it isn't root) is unsupported, not ignored.
	q := execQuery("true")
	q.Set("user", "root-but-not-really")
	req := h.ts.URL + "/exec?" + q.Encode()
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(req, "http"), nil)
	if err == nil {
		conn.Close()
		t.Fatal("dial with another user succeeded")
	}
	if resp.StatusCode != 501 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	// A command that can't start is an error frame, then a close.
	r := collect(t, h.dial("/exec", execQuery("no-such-command-xyz")))
	if r.errFrame == nil || r.errFrame.Code != api.CodeInvalid || r.exit != nil {
		t.Fatalf("got %+v", r)
	}
}

func TestAttachReplaysFromOffset(t *testing.T) {
	h := newHarness(t)
	q := execQuery("sh", "-c", "printf abcdef; sleep 30")
	q.Set("detachable", "true")
	conn := h.dial("/exec", q)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, _ := conn.ReadMessage()
	var started api.ExecMessage
	json.Unmarshal(data, &started)
	// Wait for the output, then leave: a detachable session keeps running.
	for got := ""; got != "abcdef"; {
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		got += string(data[1:])
	}
	conn.Close()
	waitFor(t, "detach", func() bool {
		s := h.srv.sessions.get(started.Session)
		return s != nil && !s.info().Attached
	})
	if h.srv.sessions.get(started.Session).hasExited() {
		t.Fatal("detachable session ended with its socket")
	}

	att := h.dial("/exec/sessions/"+started.Session, url.Values{"offset": {"2"}})
	att.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, _ = att.ReadMessage()
	var m api.ExecMessage
	json.Unmarshal(data, &m)
	if m.Type != "attached" || m.Session != started.Session || m.Offset != 2 {
		t.Fatalf("attached frame %+v", m)
	}
	_, data, _ = att.ReadMessage()
	if string(data) != "\x01cdef" {
		t.Fatalf("replay %q", data)
	}

	var list api.SessionList
	h.do("GET", "/exec/sessions", nil, &list)
	if len(list.Sessions) != 1 || !list.Sessions[0].Attached || list.Sessions[0].Exited {
		t.Fatalf("sessions %+v", list)
	}
	// Attaching from the start says so: the offset is there when it is 0.
	att0 := h.dial("/exec/sessions/"+started.Session, nil)
	att0.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, _ = att0.ReadMessage()
	if want := `{"type":"attached","session":"` + started.Session + `","offset":0}`; string(data) != want {
		t.Fatalf("attached frame %s, want %s", data, want)
	}
	att0.Close()
	h.wantStatus(h.do("POST", "/exec/sessions/"+started.Session+"/signal", api.Signal{Signal: "KILL"}, nil), 204, "")
	r := collect(t, att)
	if r.exit == nil || *r.exit != 137 {
		t.Fatalf("exit %v", r.exit)
	}
	h.do("GET", "/exec/sessions", nil, &list)
	if !list.Sessions[0].Exited || list.Sessions[0].ExitCode == nil || *list.Sessions[0].ExitCode != 137 {
		t.Fatalf("exited session %+v", list.Sessions[0])
	}
	h.wantStatus(h.do("POST", "/exec/sessions/"+started.Session+"/signal", api.Signal{Signal: "TERM"}, nil), 409, api.CodeConflict)
	h.wantStatus(h.do("POST", "/exec/sessions/s-nope/signal", api.Signal{Signal: "TERM"}, nil), 404, api.CodeNotFound)
}

func TestNonDetachableEndsWithSocket(t *testing.T) {
	h := newHarness(t)
	conn := h.dial("/exec", execQuery("sleep", "30"))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, _ := conn.ReadMessage()
	var started api.ExecMessage
	json.Unmarshal(data, &started)
	conn.Close()
	waitFor(t, "SIGHUP", func() bool { return h.srv.sessions.get(started.Session).hasExited() })
	if code := h.srv.sessions.get(started.Session).info().ExitCode; code == nil || *code != 128+1 {
		t.Fatalf("exit code %v, want 129", code)
	}
}

func TestRun(t *testing.T) {
	h := newHarness(t)
	var res api.ExecResult
	h.wantStatus(h.do("POST", "/exec/run", api.ExecRun{Cmd: []string{"sh", "-c", "cat; echo err >&2; echo $A; exit 2"},
		Stdin: []byte("in\n"), Env: map[string]string{"A": "b"}}, &res), 200, "")
	if string(res.Stdout) != "in\nb\n" || string(res.Stderr) != "err\n" || res.ExitCode != 2 || res.TimedOut {
		t.Fatalf("result %+v", res)
	}
	resp := h.do("POST", "/exec/run", api.ExecRun{Cmd: []string{"true"}}, nil)
	if b, _ := io.ReadAll(resp.Body); !strings.Contains(string(b), `"stdout":""`) || !strings.Contains(string(b), `"stderr":""`) {
		t.Fatalf("empty output encodes as %s", b)
	}
	h.do("POST", "/exec/run", api.ExecRun{Cmd: []string{"pwd"}, Cwd: "/tmp"}, &res)
	if string(res.Stdout) != "/tmp\n" {
		t.Fatalf("cwd: %q", res.Stdout)
	}
	start := time.Now()
	h.do("POST", "/exec/run", api.ExecRun{Cmd: []string{"sleep", "30"}, TimeoutS: 1}, &res)
	if !res.TimedOut || res.ExitCode != 137 || time.Since(start) > 5*time.Second {
		t.Fatalf("timeout: %+v after %s", res, time.Since(start))
	}
	h.wantStatus(h.do("POST", "/exec/run", api.ExecRun{}, nil), 400, api.CodeInvalid)
	h.wantStatus(h.do("POST", "/exec/run", api.ExecRun{Cmd: []string{"true"}, TimeoutS: 4000}, nil), 400, api.CodeInvalid)
	h.wantStatus(h.do("POST", "/exec/run", api.ExecRun{Cmd: []string{"no-such-command-xyz"}}, nil), 400, api.CodeInvalid)
}

func TestRunTruncates(t *testing.T) {
	h := newHarness(t)
	var res api.ExecResult
	h.do("POST", "/exec/run", api.ExecRun{Cmd: []string{"sh", "-c", "head -c 17000000 /dev/zero"}}, &res)
	if len(res.Stdout) != runMaxOutput || !res.Truncated || res.ExitCode != 0 {
		t.Fatalf("len %d truncated %v exit %d", len(res.Stdout), res.Truncated, res.ExitCode)
	}
}

func TestParseSignal(t *testing.T) {
	for in, want := range map[string]int{"TERM": 15, "sigkill": 9, "HUP": 1, "2": 2} {
		if got, err := parseSignal(in); err != nil || int(got) != want {
			t.Errorf("parseSignal(%q) = %d, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "NOPE", "0", "99"} {
		if _, err := parseSignal(bad); err == nil {
			t.Errorf("parseSignal(%q) succeeded", bad)
		}
	}
}
