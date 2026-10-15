package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arugula-salad/loam/pkg/api"
)

// fakeAPI is just enough of loam-api for the CLI's tests: sandbox records,
// a toy exec (echo, cat, exit N, sleep), files on a temp directory, an echo
// port and a log of every request.
type fakeAPI struct {
	t    *testing.T
	root string // the sandboxes' shared filesystem
	srv  *httptest.Server

	mu        sync.Mutex
	sandboxes map[string]*api.Sandbox

	rmu      sync.Mutex // requests and bodies, which handlers holding mu record too
	requests []string
	bodies   map[string]string // last body per "METHOD path"
}

func newFake(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, root: t.TempDir(), sandboxes: map[string]*api.Sandbox{}, bodies: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, api.Whoami{Project: api.Project{Name: "jake"}, Key: api.Key{ID: "k1", Name: "laptop", Project: "jake"}})
	})
	mux.HandleFunc("POST /v1/sandboxes", f.create)
	mux.HandleFunc("GET /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		l := api.SandboxList{Sandboxes: []api.Sandbox{}}
		for _, s := range f.sandboxes {
			l.Sandboxes = append(l.Sandboxes, *s)
		}
		writeJSON(w, 200, l)
	})
	mux.HandleFunc("GET /v1/sandboxes/{sb}", f.withSandbox(func(w http.ResponseWriter, r *http.Request, s *api.Sandbox) { writeJSON(w, 200, s) }))
	mux.HandleFunc("PATCH /v1/sandboxes/{sb}", f.withSandbox(func(w http.ResponseWriter, r *http.Request, s *api.Sandbox) {
		var u api.UpdateSandbox
		json.Unmarshal([]byte(f.body(r)), &u)
		if u.Env != nil {
			s.Env = u.Env
		}
		if u.Labels != nil {
			s.Labels = u.Labels
		}
		if u.Idle != nil {
			s.Idle = *u.Idle
		}
		if u.Network != nil {
			s.Network = *u.Network
		}
		if u.Secrets != nil {
			s.Secrets = *u.Secrets
		}
		writeJSON(w, 200, s)
	}))
	mux.HandleFunc("GET /v1/sandboxes/{sb}/stats", f.withSandbox(func(w http.ResponseWriter, r *http.Request, s *api.Sandbox) {
		f.log(r)
		day := api.StatsDay{Day: "2026-10-13", Traffic: api.Traffic{Requests: 40, Status2xx: 38, Status4xx: 2, BytesSent: 3 << 20}, Visitors: 9}
		writeJSON(w, 200, api.Stats{Sandbox: s.ID, Name: s.Name, Days: []api.StatsDay{
			{Day: "2026-10-12"}, day}, Total: day.Traffic, VisitorDays: 9})
	}))
	mux.HandleFunc("DELETE /v1/sandboxes/{sb}", f.withSandbox(func(w http.ResponseWriter, r *http.Request, s *api.Sandbox) {
		delete(f.sandboxes, s.Name)
		w.WriteHeader(204)
	}))
	for verb, state := range map[string]string{"pause": api.StatePaused, "stop": api.StateStopped, "wake": api.StateRunning} {
		mux.HandleFunc("POST /v1/sandboxes/{sb}/"+verb, f.withSandbox(func(w http.ResponseWriter, r *http.Request, s *api.Sandbox) {
			s.State = state
			writeJSON(w, 200, s)
		}))
	}
	mux.HandleFunc("GET /v1/sandboxes/{sb}/exec", f.exec)
	mux.HandleFunc("POST /v1/sandboxes/{sb}/exec/run", func(w http.ResponseWriter, r *http.Request) {
		var req api.ExecRun
		json.Unmarshal([]byte(f.body(r)), &req)
		var out bytes.Buffer
		code := toyCommand(req.Cmd, bytes.NewReader(req.Stdin), &out)
		writeJSON(w, 200, api.ExecResult{ExitCode: code, Stdout: out.Bytes(), Stderr: []byte("on stderr\n")})
	})
	mux.HandleFunc("GET /v1/sandboxes/{sb}/fs/stat", func(w http.ResponseWriter, r *http.Request) {
		info, err := os.Lstat(f.path(r))
		if err != nil {
			writeJSON(w, 404, api.Error{Error: api.ErrorDetail{Code: api.CodeNotFound, Message: "no such file"}})
			return
		}
		typ := "file"
		if info.IsDir() {
			typ = "dir"
		}
		writeJSON(w, 200, api.Stat{Path: r.URL.Query().Get("path"), Name: info.Name(), Type: typ, Size: info.Size(), Mode: fmt.Sprintf("%04o", info.Mode().Perm())})
	})
	mux.HandleFunc("GET /v1/sandboxes/{sb}/fs/file", func(w http.ResponseWriter, r *http.Request) {
		f.log(r)
		http.ServeFile(w, r, f.path(r))
	})
	mux.HandleFunc("PUT /v1/sandboxes/{sb}/fs/file", func(w http.ResponseWriter, r *http.Request) {
		f.log(r)
		p := f.path(r)
		mode, err := strconv.ParseUint(r.URL.Query().Get("mode"), 8, 32)
		if err != nil {
			mode = 0o644
		}
		if r.URL.Query().Get("mkdir") == "true" {
			os.MkdirAll(filepath.Dir(p), 0o755)
		}
		b, _ := io.ReadAll(r.Body)
		if err := os.WriteFile(p, b, os.FileMode(mode)); err != nil {
			writeJSON(w, 400, api.Error{Error: api.ErrorDetail{Code: api.CodeInvalid, Message: err.Error()}})
			return
		}
		os.Chmod(p, os.FileMode(mode))
		writeJSON(w, 200, api.Stat{Path: r.URL.Query().Get("path"), Size: int64(len(b))})
	})
	mux.HandleFunc("PUT /v1/sandboxes/{sb}/fs/tar", func(w http.ResponseWriter, r *http.Request) {
		f.log(r)
		os.MkdirAll(f.path(r), 0o755)
		if err := extractTar(r.Body, f.path(r)); err != nil {
			writeJSON(w, 400, api.Error{Error: api.ErrorDetail{Code: api.CodeInvalid, Message: err.Error()}})
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /v1/sandboxes/{sb}/fs/tar", func(w http.ResponseWriter, r *http.Request) {
		f.log(r)
		writeTar(w, f.path(r))
	})
	mux.HandleFunc("GET /v1/sandboxes/{sb}/ports/{port}/connect", func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			ws.WriteMessage(mt, append([]byte(r.PathValue("port")+":"), data...))
		}
	})
	mux.HandleFunc("GET /v1/sandboxes/{sb}/ports/settings", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, api.PortSettingList{Ports: []api.PortSetting{{Port: 5000, Access: "public", URL: "https://5000--box--jake.loam.test"}}})
	})
	mux.HandleFunc("PUT /v1/sandboxes/{sb}/ports/{port}", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		json.Unmarshal([]byte(f.body(r)), &b)
		p, _ := strconv.Atoi(r.PathValue("port"))
		writeJSON(w, 200, api.PortSetting{Port: p, Access: b["access"], URL: fmt.Sprintf("https://%d--box--jake.loam.test", p)})
	})
	mux.HandleFunc("PUT /v1/secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
		var req api.PutSecret
		json.Unmarshal([]byte(f.body(r)), &req)
		writeJSON(w, 200, api.Secret{Name: r.PathValue("name"), Hosts: req.Hosts, Inject: req.Inject,
			Placeholder: req.PlaceholderPrefix + "__loam_" + strings.ToLower(r.PathValue("name")) + "__"})
	})
	mux.HandleFunc("GET /v1/network/defaults", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, api.Network{Egress: []api.Rule{{Domain: "github.com", Action: "allow"}, {Domain: "*.npmjs.org", Action: "allow"}}})
	})
	mux.HandleFunc("POST /v1/projects/{p}/keys", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, api.Key{ID: "k2", Name: "ci", Project: r.PathValue("p"), Secret: "loam_SECRET"})
	})
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer loam_") {
			writeJSON(w, 401, api.Error{Error: api.ErrorDetail{Code: api.CodeUnauthorized, Message: "bad key"}})
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

var upgrader = websocket.Upgrader{}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (f *fakeAPI) log(r *http.Request) {
	f.rmu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	f.rmu.Unlock()
}

// body reads and records a request's body.
func (f *fakeAPI) body(r *http.Request) string {
	b, _ := io.ReadAll(r.Body)
	f.rmu.Lock()
	f.bodies[r.Method+" "+r.URL.Path] = string(b)
	f.rmu.Unlock()
	return string(b)
}

func (f *fakeAPI) path(r *http.Request) string {
	return filepath.Join(f.root, filepath.FromSlash(r.URL.Query().Get("path")))
}

func (f *fakeAPI) withSandbox(h func(http.ResponseWriter, *http.Request, *api.Sandbox)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		s := f.sandboxes[r.PathValue("sb")]
		f.mu.Unlock()
		if s == nil {
			writeJSON(w, 404, api.Error{Error: api.ErrorDetail{Code: api.CodeNotFound, Message: "sandbox not found"}})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		h(w, r, s)
	}
}

func (f *fakeAPI) create(w http.ResponseWriter, r *http.Request) {
	var req api.CreateSandbox
	json.Unmarshal([]byte(f.body(r)), &req)
	if req.Name == "" {
		req.Name = "brave-otter-0001"
	}
	s := &api.Sandbox{ID: "sb-" + strings.Repeat("a", 12), Name: req.Name, Project: "jake", Template: "base", State: api.StateRunning,
		Resources: api.Resources{CPUs: 2, MemoryMiB: 2048, DiskGiB: 20}, Env: req.Env, Labels: req.Labels,
		Idle: api.Idle{TimeoutS: 300, Action: "pause"}, HTTPPort: 8080, URL: "https://" + req.Name + "--jake.loam.test",
		CreatedAt: time.Now().Add(-90 * time.Minute), Network: api.Network{Egress: []api.Rule{}}, Secrets: []string{}}
	if req.Network != nil {
		s.Network = *req.Network
	}
	if req.Secrets != nil {
		s.Secrets = *req.Secrets
	}
	if req.Image != "" {
		s.Template, s.Image = "", req.Image
		s.URLs = []string{"https://" + req.Name + ".games.test"}
	}
	f.mu.Lock()
	f.sandboxes[s.Name] = s
	f.mu.Unlock()
	writeJSON(w, 201, s)
}

// toyCommand runs the few commands the fake understands.
func toyCommand(cmd []string, stdin io.Reader, out io.Writer) int {
	switch cmd[0] {
	case "echo":
		fmt.Fprintln(out, strings.Join(cmd[1:], " "))
	case "cat":
		io.Copy(out, stdin)
	case "exit":
		n, _ := strconv.Atoi(cmd[1])
		return n
	}
	return 0
}

// exec is a streaming exec of a toy command; "sleep" waits for a signal and exits 128+15.
func (f *fakeAPI) exec(w http.ResponseWriter, r *http.Request) {
	f.log(r)
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	cmd := r.URL.Query()["cmd"]
	ws.WriteJSON(api.ExecMessage{Type: "started", Session: "s-1", PID: 10})
	stdin, stdinW := io.Pipe()
	signals := make(chan string, 1)
	go func() {
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				stdinW.Close()
				return
			}
			if mt == websocket.TextMessage {
				var m api.ExecMessage
				json.Unmarshal(data, &m)
				if m.Type == "signal" {
					signals <- m.Signal
				}
				continue
			}
			if data[0] == api.StreamStdinEOF {
				stdinW.Close()
			} else {
				stdinW.Write(data[1:])
			}
		}
	}()
	var code int
	if cmd[0] == "sleep" {
		<-signals
		code = 143
	} else {
		var out bytes.Buffer
		code = toyCommand(cmd, stdin, &out)
		ws.WriteMessage(websocket.BinaryMessage, append([]byte{api.StreamStdout}, out.Bytes()...))
		ws.WriteMessage(websocket.BinaryMessage, append([]byte{api.StreamStderr}, "done\n"...))
	}
	ws.WriteJSON(api.ExecMessage{Type: "exit", Code: &code})
	ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
}

// cli runs the CLI against f with stdin, returning its exit code and output.
func (f *fakeAPI) cli(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	return f.cliCtx(context.Background(), t, stdin, args...)
}

func (f *fakeAPI) cliCtx(ctx context.Context, t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	env := map[string]string{"LOAM_API_URL": f.srv.URL, "LOAM_TOKEN": "loam_test", "LOAM_CONFIG": filepath.Join(t.TempDir(), "config.json")}
	a := &app{ctx: ctx, stdin: strings.NewReader(stdin), stdout: &stdout, stderr: &stderr, getenv: func(k string) string { return env[k] }}
	code := a.main(args)
	return code, stdout.String(), stderr.String()
}
