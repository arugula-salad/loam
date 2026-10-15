package apiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/db/dbtest"
	"github.com/arugula-salad/loam/internal/nodeapi/fake"
	"github.com/arugula-salad/loam/pkg/api"
)

// The handler tests run loam-api against the fake nodelet, over real HTTP on
// both sides, and against Memory or, with LOAM_TEST_DATABASE_URL, Postgres.

const (
	adminToken = "admin-secret"
	nodeToken  = "node-secret"
	testDomain = "loam.test"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type harness struct {
	t     *testing.T
	srv   *Server
	ts    *httptest.Server
	node  *fake.Nodelet // the first node
	fakes map[string]*fake.Nodelet
	nodes *Nodes
	clock *clock
	store db.Store
}

// nodeSpec is a fake node: its name and the memory it gives VMs.
type nodeSpec struct {
	name string
	mem  int
}

// newHarness is loam-api with one node, geek, as in phase 1.
func newHarness(t *testing.T) *harness {
	return newCluster(t, nodeSpec{"geek", 8192})
}

// newCluster is loam-api with several nodes sharing one snapshot store. A
// fresh sandbox goes to the node with the most free memory.
func newCluster(t *testing.T, specs ...nodeSpec) *harness {
	t.Helper()
	return newClusterWith(t, nil, specs...)
}

// newClusterWith is newCluster with the Options changed by mod first.
func newClusterWith(t *testing.T, mod func(*Options), specs ...nodeSpec) *harness {
	t.Helper()
	st := fake.NewStore()
	fakes := map[string]*fake.Nodelet{}
	static := map[string]string{}
	var first *fake.Nodelet
	for _, sp := range specs {
		n := fake.New(sp.name, nodeToken, sp.mem).WithStore(st)
		n.Keepalive = 50 * time.Millisecond
		nts := httptest.NewServer(n)
		t.Cleanup(nts.Close)
		fakes[sp.name] = n
		static[sp.name] = strings.TrimPrefix(nts.URL, "http://")
		if first == nil {
			first = n
		}
	}
	nodes := &Nodes{Token: nodeToken, Static: static, Wait: 300 * time.Millisecond}
	clk := &clock{t: time.Date(2026, 10, 11, 18, 0, 0, 0, time.UTC)}
	dbs := dbtest.Store(t)
	opts := Options{
		Store: dbs, Nodes: nodes, Domain: testDomain, AdminToken: adminToken, NodeToken: nodeToken,
		Templates: Templates{
			"base": {Name: "base", Image: "ghcr.io/arugula-salad/loam-base@sha256:abc", Resources: api.Resources{CPUs: 2, MemoryMiB: 2048, DiskGiB: 20}},
			"big":  {Name: "big", Image: "ghcr.io/arugula-salad/loam-big@sha256:def", Resources: api.Resources{CPUs: 4, MemoryMiB: 4096, DiskGiB: 40}},
		},
		Log:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:                  clk.Now,
		PortWait:             300 * time.Millisecond,
		GuestCheckpointDelay: time.Millisecond,
		EventStreamIdle:      500 * time.Millisecond,
		SecretKey:            bytes.Repeat([]byte{7}, SecretKeySize),
	}
	if mod != nil {
		mod(&opts)
	}
	srv := New(opts)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &harness{t: t, srv: srv, ts: ts, node: first, fakes: fakes, nodes: nodes, clock: clk, store: dbs}
}

// call makes a request and returns the status and body. body may be nil, a
// []byte (sent raw) or anything else (sent as JSON).
func (h *harness) call(method, path, token string, body any) (int, []byte, http.Header) {
	h.t.Helper()
	return h.callHost("", method, path, token, body)
}

func (h *harness) callHost(host, method, path, token string, body any) (int, []byte, http.Header) {
	h.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, rd)
	if err != nil {
		h.t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, resp.Header
}

// must is call with an expected status, decoding the answer into out.
func (h *harness) must(want int, method, path, token string, body, out any) {
	h.t.Helper()
	status, raw, _ := h.call(method, path, token, body)
	if status != want {
		h.t.Fatalf("%s %s: %d %s, want %d", method, path, status, raw, want)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			h.t.Fatalf("%s %s: %v in %s", method, path, err, raw)
		}
	}
}

// errCode is a call expected to fail with status and code.
func (h *harness) errCode(status int, code, method, path, token string, body any) {
	h.t.Helper()
	got, raw, _ := h.call(method, path, token, body)
	var e api.Error
	json.Unmarshal(raw, &e)
	if got != status || e.Error.Code != code {
		h.t.Fatalf("%s %s: %d %s, want %d %s", method, path, got, raw, status, code)
	}
}

// project makes a project and returns a key for it.
func (h *harness) project(name string) string {
	h.t.Helper()
	h.must(201, "POST", "/v1/projects", adminToken, map[string]string{"name": name}, nil)
	var k api.Key
	h.must(201, "POST", "/v1/projects/"+name+"/keys", adminToken, map[string]string{"name": "test"}, &k)
	return k.Secret
}

func (h *harness) create(key string, req api.CreateSandbox) api.Sandbox {
	h.t.Helper()
	var sb api.Sandbox
	h.must(201, "POST", "/v1/sandboxes", key, req, &sb)
	return sb
}

func (h *harness) get(key, ref string) api.Sandbox {
	h.t.Helper()
	var sb api.Sandbox
	h.must(200, "GET", "/v1/sandboxes/"+ref, key, nil, &sb)
	return sb
}

func (h *harness) exec(key, ref string, cmd ...string) api.ExecResult {
	h.t.Helper()
	var res api.ExecResult
	h.must(200, "POST", "/v1/sandboxes/"+ref+"/exec/run", key, api.ExecRun{Cmd: cmd}, &res)
	return res
}

// dialWS opens a WebSocket to the API.
func (h *harness) dialWS(path, token string) (*websocket.Conn, *http.Response, error) {
	h.t.Helper()
	hdr := http.Header{}
	if token != "" {
		hdr.Set("Authorization", "Bearer "+token)
	}
	return websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.ts.URL, "http")+path, hdr)
}

// eventually polls cond for up to two seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// callsSince returns the node's calls and empties the log.
func (h *harness) callsSince() []string {
	c := h.node.Calls()
	h.node.ResetCalls()
	return c
}

func wantCalls(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("node calls:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func (h *harness) tick() { h.srv.lifecycleTick(context.Background()) }

// gc runs one collection of snapshots and layers.
func (h *harness) gc() { h.srv.collect(context.Background()) }
