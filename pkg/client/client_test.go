package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arugula-salad/loam/pkg/api"
)

var upgrader = websocket.Upgrader{}

// serve starts a test API with mux, checking every request carries the key.
func serve(t *testing.T, mux *http.ServeMux) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer loam_test" {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(api.Error{Error: api.ErrorDetail{Code: api.CodeUnauthorized, Message: "no key"}})
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL+"/", "loam_test")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestErrors(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes/{sb}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 404, api.Error{Error: api.ErrorDetail{Code: api.CodeNotFound, Message: "no sandbox " + r.PathValue("sb")}})
	})
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		writeJSON(w, 503, api.Error{Error: api.ErrorDetail{Code: api.CodeCapacity, Message: "full"}})
	})
	mux.HandleFunc("GET /v1/templates", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	})
	c := serve(t, mux)

	_, err := c.GetSandbox(ctx(t), "nope")
	if !IsNotFound(err) || !strings.Contains(err.Error(), "no sandbox nope") {
		t.Fatalf("get: %v", err)
	}
	_, err = c.CreateSandbox(ctx(t), api.CreateSandbox{})
	var e *Error
	if !errors.As(err, &e) || e.Code != api.CodeCapacity || e.RetryAfter != 7*time.Second {
		t.Fatalf("create: %#v", err)
	}
	_, err = c.ListTemplates(ctx(t))
	if !errors.As(err, &e) || e.Status != 502 || e.Code != "http_502" || e.Message != "bad gateway" {
		t.Fatalf("templates: %#v", err)
	}
	c.Token = "wrong"
	if _, err := c.Whoami(ctx(t)); ErrorCode(err) != api.CodeUnauthorized {
		t.Fatalf("whoami with a bad key: %v", err)
	}
}

func TestSandboxRoutes(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, strings.TrimSpace(fmt.Sprintf("%s %s %s", r.Method, r.URL.RequestURI(), body)))
		mu.Unlock()
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/checkpoints") && r.Method == http.MethodPost:
			writeJSON(w, 201, api.Checkpoint{ID: "v1", Memory: true})
		case strings.HasSuffix(r.URL.Path, "/checkpoints"):
			writeJSON(w, 200, api.CheckpointList{Checkpoints: []api.Checkpoint{{ID: "v1"}, {ID: "auto-1", Auto: true}}})
		case r.URL.Path == "/v1/sandboxes" && r.Method == http.MethodGet:
			writeJSON(w, 200, api.SandboxList{Sandboxes: []api.Sandbox{{Name: "a"}}})
		default:
			writeJSON(w, 200, api.Sandbox{Name: "a", State: api.StateRunning})
		}
	})
	c := serve(t, mux)
	x := ctx(t)

	if _, err := c.CreateSandbox(x, api.CreateSandbox{Name: "a", Resources: &api.Resources{CPUs: 2}, Idle: &api.Idle{TimeoutS: 30, Action: "stop"}}); err != nil {
		t.Fatal(err)
	}
	if l, err := c.ListSandboxes(x, ListOptions{Labels: map[string]string{"b": "2", "a": "1"}, State: "paused"}); err != nil || len(l) != 1 {
		t.Fatal(l, err)
	}
	protected := false
	if _, err := c.UpdateSandbox(x, "a", api.UpdateSandbox{Env: map[string]string{"K": "V"}, Protected: &protected}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []func(context.Context, string) (*api.Sandbox, error){c.Pause, c.Stop, c.Wake} {
		if _, err := f(x, "a"); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.DeleteSandbox(x, "a"); err != nil {
		t.Fatal(err)
	}
	f := false
	if cp, err := c.CreateCheckpoint(x, "a", api.CreateCheckpoint{Comment: "c", Memory: &f}); err != nil || cp.ID != "v1" {
		t.Fatal(cp, err)
	}
	if l, err := c.ListCheckpoints(x, "a"); err != nil || len(l) != 2 || !l[1].Auto {
		t.Fatal(l, err)
	}
	if _, err := c.RestoreCheckpoint(x, "a", "v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fork(x, "a", "v1", "b"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`POST /v1/sandboxes {"name":"a","resources":{"cpus":2,"memory_mib":0,"disk_gib":0},"idle":{"timeout_s":30,"action":"stop"}}`,
		`GET /v1/sandboxes?label=a%3D1&label=b%3D2&state=paused`,
		`PATCH /v1/sandboxes/a {"env":{"K":"V"},"protected":false}`,
		`POST /v1/sandboxes/a/pause`,
		`POST /v1/sandboxes/a/stop`,
		`POST /v1/sandboxes/a/wake`,
		`DELETE /v1/sandboxes/a`,
		`POST /v1/sandboxes/a/checkpoints {"comment":"c","memory":false}`,
		`GET /v1/sandboxes/a/checkpoints`,
		`POST /v1/sandboxes/a/checkpoints/v1/restore`,
		`POST /v1/sandboxes {"name":"b","from":{"sandbox":"a","checkpoint":"v1"}}`,
	}
	if strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(seen, "\n"), strings.Join(want, "\n"))
	}
}

func TestAdmin(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/projects/{p}/keys", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		json.NewDecoder(r.Body).Decode(&b)
		writeJSON(w, 200, api.Key{ID: "k1", Name: b["name"], Project: r.PathValue("p"), Secret: "loam_secret"})
	})
	mux.HandleFunc("DELETE /v1/projects/{p}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 409, api.Error{Error: api.ErrorDetail{Code: api.CodeProjectNotEmpty, Message: "has sandboxes"}})
	})
	c := serve(t, mux)
	k, err := c.CreateKey(ctx(t), "jake", "laptop")
	if err != nil || k.Secret != "loam_secret" || k.Project != "jake" || k.Name != "laptop" {
		t.Fatal(k, err)
	}
	if err := c.DeleteProject(ctx(t), "jake"); ErrorCode(err) != api.CodeProjectNotEmpty {
		t.Fatal(err)
	}
}

// execServer is a fake exec route: it upper-cases stdin to stdout, says
// "err" on stderr at EOF, records control messages, and exits 3.
func execServer(t *testing.T, got *[]string, mu *sync.Mutex) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*got = append(*got, "query "+r.URL.RawQuery)
		mu.Unlock()
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer ws.Close()
		ws.WriteJSON(api.ExecMessage{Type: "started", Session: "s-1", PID: 42})
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.TextMessage {
				mu.Lock()
				*got = append(*got, string(data))
				mu.Unlock()
				continue
			}
			if data[0] == api.StreamStdinEOF {
				ws.WriteMessage(websocket.BinaryMessage, append([]byte{api.StreamStderr}, "err"...))
				code := 3
				ws.WriteJSON(api.ExecMessage{Type: "exit", Code: &code})
				ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			}
			ws.WriteMessage(websocket.BinaryMessage, append([]byte{api.StreamStdout}, bytes.ToUpper(data[1:])...))
		}
	}
}

func TestExecStream(t *testing.T) {
	var got []string
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes/sb/exec", execServer(t, &got, &mu))
	c := serve(t, mux)

	var stdout, stderr bytes.Buffer
	s, err := c.Exec(ctx(t), "sb", ExecOptions{
		Cmd: []string{"sh", "-c", "cat"}, TTY: true, Cols: 80, Rows: 24, Env: map[string]string{"B": "2", "A": "1"},
		User: "root", Cwd: "/tmp", Detachable: true,
		IO: IO{Stdout: &stdout, Stderr: &stderr},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Session != "s-1" || s.PID != 42 {
		t.Fatalf("started: %+v", s)
	}
	s.Resize(100, 40)
	s.Signal("INT")
	s.Write([]byte("hello "))
	s.Write([]byte("world"))
	s.CloseStdin()
	code, err := s.Wait()
	if err != nil || code != 3 || stdout.String() != "HELLO WORLD" || stderr.String() != "err" {
		t.Fatalf("code=%d err=%v stdout=%q stderr=%q", code, err, stdout.String(), stderr.String())
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"query cmd=sh&cmd=-c&cmd=cat&cols=80&cwd=%2Ftmp&detachable=true&env=A%3D1&env=B%3D2&rows=24&tty=true&user=root",
		`{"type":"resize","cols":100,"rows":40}`,
		`{"type":"signal","signal":"INT"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("server saw:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestExecStdinReader(t *testing.T) {
	var got []string
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes/sb/exec", execServer(t, &got, &mu))
	c := serve(t, mux)
	var out bytes.Buffer
	s, err := c.Exec(ctx(t), "sb", ExecOptions{Cmd: []string{"cat"}, IO: IO{Stdin: strings.NewReader("piped"), Stdout: &out}})
	if err != nil {
		t.Fatal(err)
	}
	if code, err := s.Wait(); code != 3 || err != nil || out.String() != "PIPED" {
		t.Fatalf("code=%d err=%v out=%q", code, err, out.String())
	}
}

func TestExecEndsWithoutExit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes/sb/exec", func(w http.ResponseWriter, r *http.Request) {
		ws, _ := upgrader.Upgrade(w, r, nil)
		defer ws.Close()
		ws.WriteJSON(api.ExecMessage{Type: "started", Session: "s-1"})
		ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "sandbox deleted"))
	})
	mux.HandleFunc("GET /v1/sandboxes/bad/exec", func(w http.ResponseWriter, r *http.Request) {
		ws, _ := upgrader.Upgrade(w, r, nil)
		defer ws.Close()
		ws.WriteJSON(api.ExecMessage{Type: "error", Error: &api.ErrorDetail{Code: "invalid", Message: "no such file"}})
	})
	c := serve(t, mux)
	s, err := c.Exec(ctx(t), "sb", ExecOptions{Cmd: []string{"sleep", "9"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Wait(); !errors.Is(err, ErrExitUnknown) {
		t.Fatalf("after 1011: %v", err)
	}
	if _, err := c.Exec(ctx(t), "bad", ExecOptions{Cmd: []string{"nope"}}); ErrorCode(err) != "invalid" {
		t.Fatalf("error frame: %v", err)
	}
	if _, err := c.Exec(ctx(t), "missing", ExecOptions{Cmd: []string{"x"}}); !IsNotFound(err) {
		t.Fatalf("refused handshake: %v", err)
	}
}

func TestAttach(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes/sb/exec/sessions/{s}", func(w http.ResponseWriter, r *http.Request) {
		ws, _ := upgrader.Upgrade(w, r, nil)
		defer ws.Close()
		var off int64
		fmt.Sscan(r.URL.Query().Get("offset"), &off)
		ws.WriteJSON(api.ExecMessage{Type: "attached", Session: r.PathValue("s"), Offset: off})
		ws.WriteMessage(websocket.BinaryMessage, append([]byte{api.StreamStdout}, "0123456789"[off:]...))
		code := 0
		ws.WriteJSON(api.ExecMessage{Type: "exit", Code: &code})
		ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	})
	c := serve(t, mux)
	var out bytes.Buffer
	off := int64(4)
	s, err := c.Attach(ctx(t), "sb", "s-9", AttachOptions{Offset: &off, IO: IO{Stdout: &out}})
	if err != nil {
		t.Fatal(err)
	}
	if code, err := s.Wait(); code != 0 || err != nil || out.String() != "456789" || s.Session != "s-9" || s.Offset != 4 {
		t.Fatalf("code=%d err=%v out=%q s=%+v", code, err, out.String(), s)
	}
}

func TestRun(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/sb/exec/run", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if string(raw) != `{"cmd":["cat"],"stdin":"aGk=","timeout_s":5}` {
			t.Errorf("body %s", raw)
		}
		w.Write([]byte(`{"exit_code":1,"stdout":"aGk=","stderr":"","timed_out":false}`))
	})
	c := serve(t, mux)
	r, err := c.Run(ctx(t), "sb", api.ExecRun{Cmd: []string{"cat"}, Stdin: []byte("hi"), TimeoutS: 5})
	if err != nil || r.ExitCode != 1 || string(r.Stdout) != "hi" {
		t.Fatal(r, err)
	}
}

func TestFiles(t *testing.T) {
	files := map[string][]byte{}
	var tarBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/sandboxes/sb/fs/file", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("mode") != "0600" || q.Get("mkdir") != "true" || q.Get("user") != "loam" {
			t.Errorf("write query %s", r.URL.RawQuery)
		}
		b, _ := io.ReadAll(r.Body)
		files[q.Get("path")] = b
		writeJSON(w, 200, api.Stat{Path: q.Get("path"), Size: int64(len(b)), Mode: "0600", Type: "file"})
	})
	mux.HandleFunc("GET /v1/sandboxes/sb/fs/file", func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Query().Get("path")]
		if !ok {
			writeJSON(w, 404, api.Error{Error: api.ErrorDetail{Code: "not_found", Message: "no file"}})
			return
		}
		w.Write(b)
	})
	mux.HandleFunc("DELETE /v1/sandboxes/sb/fs/file", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("recursive") != "true" {
			t.Errorf("delete query %s", r.URL.RawQuery)
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /v1/sandboxes/sb/fs/mkdir", func(w http.ResponseWriter, r *http.Request) {
		var m api.Mkdir
		json.NewDecoder(r.Body).Decode(&m)
		writeJSON(w, 200, api.Stat{Path: m.Path, Type: "dir"})
	})
	mux.HandleFunc("POST /v1/sandboxes/sb/fs/rename", func(w http.ResponseWriter, r *http.Request) {
		var m api.Rename
		json.NewDecoder(r.Body).Decode(&m)
		writeJSON(w, 200, api.Stat{Path: m.To})
	})
	mux.HandleFunc("GET /v1/sandboxes/sb/fs/list", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, api.StatList{Entries: []api.Stat{{Name: "a"}, {Name: "b"}}})
	})
	mux.HandleFunc("PUT /v1/sandboxes/sb/fs/tar", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/x-tar" {
			t.Errorf("tar content type %q", r.Header.Get("Content-Type"))
		}
		tarBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /v1/sandboxes/sb/fs/tar", func(w http.ResponseWriter, r *http.Request) { w.Write(tarBody) })
	mux.HandleFunc("GET /v1/sandboxes/sb/fs/watch", func(w http.ResponseWriter, r *http.Request) {
		ws, _ := upgrader.Upgrade(w, r, nil)
		defer ws.Close()
		ws.WriteJSON(api.WatchEvent{Type: "create", Path: r.URL.Query().Get("path") + "/x"})
		ws.WriteJSON(api.WatchEvent{Type: "write", Path: r.URL.Query().Get("path") + "/x"})
		ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	})
	c := serve(t, mux)
	x := ctx(t)

	st, err := c.WriteFile(x, "sb", "/home/loam/a b", strings.NewReader("data"), Mode(0o600), MakeParents(), AsUser("loam"))
	if err != nil || st.Size != 4 {
		t.Fatal(st, err)
	}
	if b, err := c.ReadFile(x, "sb", "/home/loam/a b"); err != nil || string(b) != "data" {
		t.Fatal(string(b), err)
	}
	if _, err := c.ReadFile(x, "sb", "/nope"); !IsNotFound(err) {
		t.Fatal(err)
	}
	if err := c.Remove(x, "sb", "/d", Recursive()); err != nil {
		t.Fatal(err)
	}
	if st, err := c.Mkdir(x, "sb", api.Mkdir{Path: "/d", Parents: true}); err != nil || st.Type != "dir" {
		t.Fatal(st, err)
	}
	if st, err := c.Rename(x, "sb", "/a", "/b"); err != nil || st.Path != "/b" {
		t.Fatal(st, err)
	}
	if l, err := c.List(x, "sb", "/"); err != nil || len(l) != 2 {
		t.Fatal(l, err)
	}
	if err := c.PutTar(x, "sb", "/d", strings.NewReader("tarbytes")); err != nil {
		t.Fatal(err)
	}
	r, err := c.GetTar(x, "sb", "/d")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(r); string(b) != "tarbytes" {
		t.Fatalf("tar %q", b)
	}
	r.Close()
	w, err := c.WatchFiles(x, "sb", "/d", Recursive())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, want := range []string{"create", "write"} {
		ev, err := w.Next()
		if err != nil || ev.Type != want || ev.Path != "/d/x" {
			t.Fatal(ev, err)
		}
	}
	if _, err := w.Next(); err != io.EOF {
		t.Fatalf("end of watch: %v", err)
	}
}

func TestPorts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes/sb/ports/watch", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": keepalive\n\nevent: opened\ndata: {\"port\":8080,\"address\":\"0.0.0.0\",\"pid\":7,\"session\":\"s-1\"}\n\n")
		fmt.Fprint(w, "event: closed\ndata: {\"port\":8080,\"address\":\"0.0.0.0\",\"pid\":7}\n\n")
	})
	mux.HandleFunc("GET /v1/sandboxes/sb/ports/{port}/connect", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("port") != "5432" {
			t.Errorf("port %s", r.PathValue("port"))
		}
		ws, _ := upgrader.Upgrade(w, r, nil)
		defer ws.Close()
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			ws.WriteMessage(mt, append([]byte("echo:"), data...))
		}
	})
	mux.HandleFunc("PUT /v1/sandboxes/sb/ports/{port}", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		json.NewDecoder(r.Body).Decode(&b)
		writeJSON(w, 200, api.PortSetting{Port: 3000, Access: b["access"], URL: "https://3000--sb--p.example"})
	})
	c := serve(t, mux)
	x := ctx(t)

	pw, err := c.WatchPorts(x, "sb")
	if err != nil {
		t.Fatal(err)
	}
	ev, err := pw.Next()
	if err != nil || ev.Type != "opened" || ev.Port.Port != 8080 || ev.Port.Session != "s-1" {
		t.Fatal(ev, err)
	}
	if ev, err = pw.Next(); err != nil || ev.Type != "closed" {
		t.Fatal(ev, err)
	}
	if _, err := pw.Next(); err != io.EOF {
		t.Fatal(err)
	}
	pw.Close()

	conn, err := c.ConnectPort(x, "sb", 5432)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("ping"))
	buf := make([]byte, 3)
	var got []byte
	for len(got) < len("echo:ping") {
		n, err := conn.Read(buf) // a small buffer reads one frame in pieces
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, buf[:n]...)
	}
	if string(got) != "echo:ping" {
		t.Fatalf("got %q", got)
	}
	conn.Close()

	if s, err := c.SetPortAccess(x, "sb", 3000, "public"); err != nil || s.Access != "public" {
		t.Fatal(s, err)
	}
}

func TestServicesAndEvents(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/sandboxes/sb/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		var spec api.ServiceSpec
		json.NewDecoder(r.Body).Decode(&spec)
		writeJSON(w, 200, api.Service{Name: r.PathValue("name"), ServiceSpec: spec, State: "starting"})
	})
	mux.HandleFunc("POST /v1/sandboxes/sb/services/{name}/{verb}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, api.Service{Name: r.PathValue("name"), State: r.PathValue("verb") + "ed"})
	})
	mux.HandleFunc("GET /v1/sandboxes/sb/services/{name}/logs", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "follow=%s tail=%s", r.URL.Query().Get("follow"), r.URL.Query().Get("tail"))
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "event: sandbox.paused\ndata: {\"type\":\"sandbox.paused\",\"sandbox\":%q,\"at\":\"2026-10-11T18:00:00Z\"}\n\n", r.URL.Query().Get("sandbox"))
	})
	c := serve(t, mux)
	x := ctx(t)
	s, err := c.PutService(x, "sb", "web", api.ServiceSpec{Cmd: []string{"python3", "-m", "http.server"}, HTTPPort: 8000})
	if err != nil || s.Name != "web" || s.HTTPPort != 8000 || s.Cmd[0] != "python3" {
		t.Fatal(s, err)
	}
	if s, err := c.RestartService(x, "sb", "web"); err != nil || s.State != "restarted" {
		t.Fatal(s, err)
	}
	r, err := c.ServiceLogs(x, "sb", "web", LogOptions{Follow: true, Tail: 10})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(r); string(b) != "follow=true tail=10" {
		t.Fatalf("logs %q", b)
	}
	es, err := c.Events(x, "sb")
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	ev, err := es.Next()
	if err != nil || ev.Type != "sandbox.paused" || ev.Sandbox != "sb" || ev.At.IsZero() {
		t.Fatal(ev, err)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("LOAM_API_URL", "")
	t.Setenv("LOAM_TOKEN", "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("no token should be an error")
	}
	t.Setenv("LOAM_TOKEN", "loam_x")
	if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "LOAM_API_URL") {
		t.Fatal("no URL should be an error naming LOAM_API_URL:", err)
	}
	t.Setenv("LOAM_API_URL", "http://localhost:8080/")
	c, err := FromEnv()
	if err != nil || c.URL != "http://localhost:8080" || c.Token != "loam_x" {
		t.Fatal(c, err)
	}
}

func TestSecretsAndNetwork(t *testing.T) {
	mux := http.NewServeMux()
	var put api.PutSecret
	mux.HandleFunc("PUT /v1/secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&put)
		writeJSON(w, 200, api.Secret{Name: r.PathValue("name"), Hosts: put.Hosts, Placeholder: "__loam_x__"})
	})
	mux.HandleFunc("GET /v1/secrets", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, api.SecretList{Secrets: []api.Secret{{Name: "X"}}})
	})
	mux.HandleFunc("GET /v1/secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 501, api.Error{Error: api.ErrorDetail{Code: api.CodeUnsupported, Message: "no key"}})
	})
	mux.HandleFunc("DELETE /v1/secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 409, api.Error{Error: api.ErrorDetail{Code: api.CodeConflict, Message: "named"}})
	})
	mux.HandleFunc("GET /v1/network/defaults", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, api.Network{Egress: []api.Rule{{Domain: "github.com", Action: api.RuleAllow}}})
	})
	c := serve(t, mux)

	s, err := c.PutSecret(ctx(t), "X", api.PutSecret{Value: "v", Hosts: []string{"a.com"}})
	if err != nil || s.Name != "X" || put.Value != "v" || put.Hosts[0] != "a.com" {
		t.Fatalf("put: %+v %v (sent %+v)", s, err, put)
	}
	if l, err := c.ListSecrets(ctx(t)); err != nil || len(l) != 1 {
		t.Fatalf("list: %+v %v", l, err)
	}
	if _, err := c.GetSecret(ctx(t), "X"); ErrorCode(err) != api.CodeUnsupported {
		t.Fatalf("get: %v", err)
	}
	if err := c.DeleteSecret(ctx(t), "X"); ErrorCode(err) != api.CodeConflict {
		t.Fatalf("delete: %v", err)
	}
	if d, err := c.NetworkDefaults(ctx(t)); err != nil || len(d) != 1 || d[0].Domain != "github.com" {
		t.Fatalf("defaults: %+v %v", d, err)
	}
}
