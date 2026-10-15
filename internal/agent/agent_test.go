package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

// fakeSystem records what the agent asked of the machine.
type fakeSystem struct {
	mu        sync.Mutex
	clock     time.Time
	hostname  string
	network   *nodeapi.WakeNetwork
	drops     int
	syncs     int
	poweroffs int
}

func (f *fakeSystem) SetClock(t time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = t
	return nil
}
func (f *fakeSystem) SetHostname(h string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hostname = h
	return nil
}
func (f *fakeSystem) ConfigureNetwork(n *nodeapi.WakeNetwork) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.network = n
	return nil
}
func (f *fakeSystem) DropCaches() { f.mu.Lock(); f.drops++; f.mu.Unlock() }
func (f *fakeSystem) Sync()       { f.mu.Lock(); f.syncs++; f.mu.Unlock() }
func (f *fakeSystem) Poweroff()   { f.mu.Lock(); f.poweroffs++; f.mu.Unlock() }

type harness struct {
	t     *testing.T
	srv   *Server
	ts    *httptest.Server
	sys   *fakeSystem
	state string
	run   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	return newHarnessIn(t, dir+"/state", dir+"/run")
}

func newHarnessIn(t *testing.T, state, run string) *harness {
	t.Helper()
	sys := &fakeSystem{}
	srv := New(Config{StateDir: state, RunDir: run, System: sys, RunAsSelf: true, ImageMeta: state + "/image.json",
		TrustRoot: filepath.Join(filepath.Dir(state), "root")})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
		srv.Close()
	})
	return &harness{t: t, srv: srv, ts: ts, sys: sys, state: state, run: run}
}

// do sends a request and decodes a JSON answer into out (when not nil).
func (h *harness) do(method, path string, body any, out any) *http.Response {
	h.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, h.ts.URL+path, rd)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			h.t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
		}
	}
	return resp
}

// wantStatus checks a status and, for an error, the error shape of docs/API.md.
func (h *harness) wantStatus(resp *http.Response, status int, code string) {
	h.t.Helper()
	if resp.StatusCode != status {
		b, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("%s %s: status %d, want %d: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, status, b)
	}
	if code == "" {
		return
	}
	var e api.Error
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &e); err != nil || e.Error.Code != code || e.Error.Message == "" {
		h.t.Fatalf("error body %q, want code %q with a message", b, code)
	}
}

func (h *harness) dial(path string, q url.Values) *websocket.Conn {
	h.t.Helper()
	u := "ws" + strings.TrimPrefix(h.ts.URL, "http") + path
	if q != nil {
		u += "?" + q.Encode()
	}
	conn, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		body := ""
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			body = string(b)
		}
		h.t.Fatalf("dial %s: %v %s", u, err, body)
	}
	h.t.Cleanup(func() { conn.Close() })
	return conn
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestUnknownRouteIsJSON404(t *testing.T) {
	h := newHarness(t)
	h.wantStatus(h.do("GET", "/nope", nil, nil), 404, api.CodeNotFound)
}

func TestWakeAppliesAndPersists(t *testing.T) {
	h := newHarness(t)
	at := time.Date(2026, 10, 11, 18, 0, 0, 0, time.UTC)
	wk := nodeapi.Wake{Sandbox: "sb-x", Hostname: "scratch", Env: map[string]string{"FOO": "bar"}, Time: at,
		Network: &nodeapi.WakeNetwork{IP: "10.240.0.7/16", Gateway: "10.240.0.1"}}
	h.wantStatus(h.do("POST", "/internal/wake", wk, nil), 204, "")
	if !h.sys.clock.Equal(at) || h.sys.hostname != "scratch" || h.sys.network == nil || h.sys.network.IP != "10.240.0.7/16" {
		t.Fatalf("system after wake: %+v", h.sys)
	}
	// The sandbox's env reaches new processes.
	var res api.ExecResult
	h.do("POST", "/exec/run", api.ExecRun{Cmd: []string{"sh", "-c", "echo $FOO"}}, &res)
	if string(res.Stdout) != "bar\n" {
		t.Fatalf("stdout %q", res.Stdout)
	}
	// A server process restarted on the same boot takes the wake up again.
	h2 := newHarnessIn(t, h.state, h.run)
	h2.do("POST", "/exec/run", api.ExecRun{Cmd: []string{"sh", "-c", "echo $FOO"}}, &res)
	if string(res.Stdout) != "bar\n" {
		t.Fatalf("after restart, stdout %q", res.Stdout)
	}
	// Unknown fields are refused.
	h.wantStatus(h.do("POST", "/internal/wake", `{"sandbox":"x","bogus":1}`, nil), 400, api.CodeInvalid)
}

func TestActivityAndTasks(t *testing.T) {
	h := newHarness(t)
	var act nodeapi.Activity
	h.do("GET", "/internal/activity", nil, &act)
	if act.Busy || act.Tasks != 0 || act.SessionsAttached != 0 {
		t.Fatalf("idle sandbox: %+v", act)
	}
	time.Sleep(30 * time.Millisecond)
	h.do("GET", "/internal/activity", nil, &act)
	if act.IdleMS < 20 {
		t.Fatalf("idle_ms %d after 30ms", act.IdleMS)
	}

	var task api.Task
	h.wantStatus(h.do("PUT", "/tasks/build", api.PutTask{ExpireS: 60}, &task), 200, "")
	if task.Name != "build" || task.ExpiresAt.Sub(task.StartedAt) != time.Minute {
		t.Fatalf("task %+v", task)
	}
	h.do("GET", "/internal/activity", nil, &act)
	if !act.Busy || act.Tasks != 1 {
		t.Fatalf("with a task: %+v", act)
	}
	var list api.TaskList
	h.do("GET", "/tasks", nil, &list)
	if len(list.Tasks) != 1 {
		t.Fatalf("tasks %+v", list)
	}
	h.wantStatus(h.do("PUT", "/tasks/x", api.PutTask{ExpireS: 7200}, nil), 400, api.CodeInvalid)
	h.wantStatus(h.do("DELETE", "/tasks/build", nil, nil), 204, "")
	h.wantStatus(h.do("DELETE", "/tasks/build", nil, nil), 404, api.CodeNotFound)
	h.do("GET", "/internal/activity", nil, &act)
	if act.Busy {
		t.Fatalf("after delete: %+v", act)
	}
}

func TestPresuspendRunsHooksAndDropsCaches(t *testing.T) {
	h := newHarness(t)
	hooks := h.state + "/hooks/presuspend.d"
	mustWrite(t, hooks+"/10-mark", "#!/bin/sh\ntouch "+h.state+"/hooked\n", 0o755)
	h.wantStatus(h.do("POST", "/internal/presuspend", nil, nil), 204, "")
	if !exists(h.state + "/hooked") {
		t.Fatal("hook did not run")
	}
	if h.sys.drops != 1 {
		t.Fatalf("drops %d", h.sys.drops)
	}
	h.wantStatus(h.do("POST", "/internal/sync", nil, nil), 204, "")
	if h.sys.syncs != 1 {
		t.Fatalf("syncs %d", h.sys.syncs)
	}
}

func TestPoweroffStopsServicesFirst(t *testing.T) {
	h := newHarness(t)
	var svc api.Service
	h.wantStatus(h.do("PUT", "/services/sleeper", api.ServiceSpec{Cmd: []string{"sleep", "60"}}, &svc), 200, "")
	h.wantStatus(h.do("POST", "/internal/poweroff", nil, nil), 204, "")
	waitFor(t, "poweroff", func() bool { h.sys.mu.Lock(); defer h.sys.mu.Unlock(); return h.sys.poweroffs == 1 })
	h.do("GET", "/services/sleeper", nil, &svc)
	if svc.State != "stopped" {
		t.Fatalf("service %+v after poweroff", svc)
	}
}

func TestGuestHandler(t *testing.T) {
	// The host channel: loam-api's side, as the nodelet forwards it.
	var got struct {
		method, path, query string
	}
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.query = r.Method, r.URL.Path, r.URL.RawQuery
		if r.URL.Path == "/identity/token" {
			writeJSON(w, 200, api.IdentityToken{Token: "a.b.c"})
			return
		}
		writeJSON(w, 201, api.Checkpoint{ID: "v1"})
	}))
	defer host.Close()
	dir := t.TempDir()
	srv := New(Config{StateDir: dir + "/state", RunDir: dir + "/run", System: &fakeSystem{}, RunAsSelf: true,
		TrustRoot: dir + "/root",
		HostDial:  func(ctx context.Context) (net.Conn, error) { return dialTCP(ctx, host.Listener.Addr().String()) }})
	defer srv.Close()
	sock := dir + "/run/api.sock"
	ln, err := ListenGuest(sock)
	if err != nil {
		t.Fatal(err)
	}
	gs := &http.Server{Handler: srv.GuestHandler()}
	go gs.Serve(ln)
	defer gs.Close()
	c := unixClient(sock)

	resp, err := c.Post("http://loam/checkpoints", "application/json", strings.NewReader(`{"comment":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	var cp api.Checkpoint
	json.NewDecoder(resp.Body).Decode(&cp)
	resp.Body.Close()
	if resp.StatusCode != 201 || cp.ID != "v1" || got.method != "POST" || got.path != "/checkpoints" {
		t.Fatalf("checkpoint relayed as %+v, answered %d %+v", got, resp.StatusCode, cp)
	}
	resp, err = c.Get("http://loam/identity/token?audience=sts.amazonaws.com&ttl_s=60")
	if err != nil {
		t.Fatal(err)
	}
	var tok api.IdentityToken
	json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if resp.StatusCode != 200 || tok.Token != "a.b.c" || got.method != "GET" || got.path != "/identity/token" ||
		got.query != "audience=sts.amazonaws.com&ttl_s=60" {
		t.Fatalf("identity token relayed as %+v, answered %d %+v", got, resp.StatusCode, tok)
	}
	// The sub-API is there; /internal is not.
	resp, _ = c.Get("http://loam/tasks")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /tasks on the guest socket: %d", resp.StatusCode)
	}
	resp, _ = c.Get("http://loam/internal/activity")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("GET /internal/activity on the guest socket: %d", resp.StatusCode)
	}
	if fi, err := statMode(sock); err != nil || fi != 0o666 {
		t.Fatalf("socket mode %o %v", fi, err)
	}
}
