package e2b

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arugula-salad/loam/internal/apiserver"
	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/db/dbtest"
	"github.com/arugula-salad/loam/internal/nodeapi/fake"
	"github.com/arugula-salad/loam/pkg/api"
)

// The adapter's tests run it on a real loam-api (internal/apiserver) over the
// fake nodelet, whose VMs run a fake envd on 49983 from every fresh boot
// (fake.StartAtBoot), and against Memory or, with LOAM_TEST_DATABASE_URL,
// Postgres.

const (
	adminToken = "admin-secret"
	nodeToken  = "node-secret"
	domain     = "e2b.test"
	apiHost    = "api.e2b.test"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

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

// fixture is the adapter on loam-api on one fake node.
type fixture struct {
	t     *testing.T
	f     *Frontend
	srv   *apiserver.Server
	ts    *httptest.Server
	node  *fake.Nodelet
	store db.Store
	clock *clock
	key   string // project p's
	other string // project q's

	mu    sync.Mutex
	envds map[string][]*fakeEnvd // by sandbox, one per boot
}

func newFixture(t *testing.T) *fixture {
	return newFixtureMem(t, 8192)
}

func newFixtureMem(t *testing.T, memMiB int) *fixture {
	return newFixtureWith(t, memMiB, nil)
}

// newFixtureWith is the fixture with loam-api's options changed by mod first.
func newFixtureWith(t *testing.T, memMiB int, mod func(*apiserver.Options)) *fixture {
	t.Helper()
	fx := &fixture{t: t, envds: map[string][]*fakeEnvd{}}
	fx.node = fake.New("geek", nodeToken, memMiB)
	fx.node.StartAtBoot(EnvdPort, func(vm string) http.Handler {
		e := &fakeEnvd{vm: vm}
		fx.mu.Lock()
		fx.envds[vm] = append(fx.envds[vm], e)
		fx.mu.Unlock()
		return e
	})
	nts := httptest.NewServer(fx.node)
	t.Cleanup(nts.Close)
	fx.clock = &clock{t: time.Date(2026, 10, 11, 18, 0, 0, 0, time.UTC)}
	fx.store = dbtest.Store(t)
	opts := apiserver.Options{
		Store: fx.store, Nodes: &apiserver.Nodes{Token: nodeToken, Static: map[string]string{"geek": strings.TrimPrefix(nts.URL, "http://")}, Wait: 300 * time.Millisecond},
		Domain: "loam.test", AdminToken: adminToken, NodeToken: nodeToken,
		Templates: apiserver.Templates{
			"base":         {Name: "base", Image: "ghcr.io/arugula-salad/loam-guest@sha256:abc", Resources: api.Resources{CPUs: 2, MemoryMiB: 2048, DiskGiB: 20}},
			"e2b":          {Name: "e2b", Image: "ghcr.io/arugula-salad/loam-e2b@sha256:def", Resources: api.Resources{CPUs: 2, MemoryMiB: 1024, DiskGiB: 10}},
			"e2b-fountain": {Name: "e2b-fountain", Image: "ghcr.io/arugula-salad/loam-e2b-fountain@sha256:fed", Resources: api.Resources{CPUs: 2, MemoryMiB: 2048, DiskGiB: 10}},
		},
		Log: quiet, Now: fx.clock.Now, PortWait: 300 * time.Millisecond, SecretKey: bytes.Repeat([]byte{7}, apiserver.SecretKeySize),
	}
	if mod != nil {
		mod(&opts)
	}
	fx.srv = apiserver.New(opts)
	fx.f = New(Options{Domain: domain, Template: "e2b", Templates: map[string]Template{"fountain": {Loam: "e2b-fountain", User: "sprite"}}, Log: quiet, Now: fx.clock.Now, InitWait: time.Second}, fx.srv, fx.store)
	fx.ts = httptest.NewServer(fx.f.Handler(fx.srv))
	t.Cleanup(fx.ts.Close)
	fx.key = fx.project("p")
	fx.other = fx.project("q")
	return fx
}

// project makes a loam project and returns a key for it.
func (fx *fixture) project(name string) string {
	fx.t.Helper()
	if st, raw := fx.loam("POST", "/v1/projects", adminToken, map[string]string{"name": name}, nil); st != 201 {
		fx.t.Fatalf("project: %d %s", st, raw)
	}
	var k api.Key
	if st, raw := fx.loam("POST", "/v1/projects/"+name+"/keys", adminToken, map[string]string{"name": "test"}, &k); st != 201 {
		fx.t.Fatalf("key: %d %s", st, raw)
	}
	return k.Secret
}

// loam is a call on loam's own API, with a bearer token.
func (fx *fixture) loam(method, path, token string, body, out any) (int, []byte) {
	fx.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, fx.ts.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fx.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 {
		json.Unmarshal(raw, out)
	}
	return resp.StatusCode, raw
}

func (fx *fixture) sandbox(id string) api.Sandbox {
	fx.t.Helper()
	var sb api.Sandbox
	if st, raw := fx.loam("GET", "/v1/sandboxes/"+id, fx.key, nil, &sb); st != 200 {
		fx.t.Fatalf("loam get %s: %d %s", id, st, raw)
	}
	return sb
}

// response is an answer from the adapter.
type response struct {
	Code   int
	Body   []byte
	Header http.Header
}

func (r response) String() string { return string(r.Body) }

// do makes a request on the adapter's control host (or the Host given in
// hdr), with key as X-API-Key when not "", and hdr as further headers.
func (fx *fixture) do(method, path, key string, body any, hdr ...string) response {
	fx.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, fx.ts.URL+path, rd)
	if err != nil {
		fx.t.Fatal(err)
	}
	req.Host = apiHost
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if strings.EqualFold(hdr[i], "Host") {
			req.Host = hdr[i+1]
		} else {
			req.Header.Set(hdr[i], hdr[i+1])
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fx.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return response{resp.StatusCode, raw, resp.Header}
}

func decode[T any](t *testing.T, r response) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(r.Body, &v); err != nil {
		t.Fatalf("%d %q: %v", r.Code, r.Body, err)
	}
	return v
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func (fx *fixture) create(body map[string]any) string {
	fx.t.Helper()
	w := fx.do("POST", "/v2/sandboxes", fx.key, body)
	if w.Code != http.StatusCreated {
		fx.t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	return decode[map[string]any](fx.t, w)["sandboxID"].(string)
}

func (fx *fixture) row(id string) db.E2BSandbox {
	fx.t.Helper()
	row, err := fx.store.E2B(context.Background(), id)
	if err != nil {
		fx.t.Fatal(err)
	}
	return row
}

// token is the sandbox's envd access token, opened.
func (fx *fixture) token(id string) string {
	fx.t.Helper()
	b, err := fx.srv.Open(id, fx.row(id).SealedToken)
	if err != nil {
		fx.t.Fatal(err)
	}
	return string(b)
}

// envd is the sandbox's envd in its latest boot, and how many boots it had.
func (fx *fixture) envd(id string) (*fakeEnvd, int) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	all := fx.envds[id]
	if len(all) == 0 {
		return nil, 0
	}
	return all[len(all)-1], len(all)
}

func wantErr(t *testing.T, w response, status int, msg string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body)
	}
	got := decode[map[string]any](t, w)
	if got["code"] != float64(status) || !strings.Contains(got["message"].(string), msg) {
		t.Fatalf("body %v, want code %d and a message with %q", got, status, msg)
	}
}

// fakeEnvd is envd in one boot of a VM: /init sets its token, after which
// everything but /init, /health and /files needs it, as envd's does.
type fakeEnvd struct {
	vm string

	mu      sync.Mutex
	token   string
	inits   []initRequest
	refuse  int // a status to answer /init with, instead of taking it
	handler http.Handler
}

func (e *fakeEnvd) Inits() []initRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]initRequest(nil), e.inits...)
}

func (e *fakeEnvd) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	tok, refuse, h := e.token, e.refuse, e.handler
	e.mu.Unlock()
	switch r.URL.Path {
	case "/init":
		var req initRequest
		json.NewDecoder(r.Body).Decode(&req)
		if refuse != 0 {
			w.WriteHeader(refuse)
			return
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.token != "" && req.AccessToken != e.token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		e.token = req.AccessToken
		e.inits = append(e.inits, req)
		w.WriteHeader(http.StatusNoContent)
		return
	case "/health":
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if tok != "" && r.URL.Path != "/files" && r.Header.Get("X-Access-Token") != tok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if h != nil {
		h.ServeHTTP(w, r)
		return
	}
	switch r.URL.Path {
	case "/metrics":
		json.NewEncoder(w).Encode(map[string]any{"ts": 1790987920, "cpu_count": 2, "cpu_used_pct": 27.5, "mem_total": 1 << 30, "mem_used": 1 << 26})
	default:
		io.WriteString(w, "envd "+r.URL.Path+" in "+e.vm)
	}
}

func (e *fakeEnvd) set(h http.Handler) {
	e.mu.Lock()
	e.handler = h
	e.mu.Unlock()
}

func TestCreate(t *testing.T) {
	fx := newFixture(t)
	now := fx.clock.Now()
	w := fx.do("POST", "/v2/sandboxes", fx.key, map[string]any{"templateID": "base", "timeout": 120,
		"metadata": map[string]string{"k": "v"}, "envVars": map[string]string{"A": "b"}})
	if w.Code != http.StatusCreated {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	got := decode[map[string]any](t, w)
	if k := keys(got); !slices.Equal(k, []string{"alias", "clientID", "domain", "envdAccessToken", "envdVersion", "sandboxID", "templateID"}) {
		t.Fatalf("keys %v", k)
	}
	id := got["sandboxID"].(string)
	// The ID is loam's, which the SDKs' <port>-<id>.<domain> carries as is.
	if !regexp.MustCompile(`^sb-[a-z2-7]{12}$`).MatchString(id) {
		t.Fatalf("id %q", id)
	}
	if got["alias"] != "base" || got["templateID"] != "base" || got["clientID"] != clientID || got["envdVersion"] != EnvdVersion || got["domain"] != domain {
		t.Fatalf("%v", got)
	}
	// A loam sandbox in the key's project: the e2b template, never idle-paused,
	// deleted at its timeout, its metadata as labels and its env vars as env.
	sb := fx.sandbox(id)
	if sb.Project != "p" || sb.Template != "e2b" || sb.State != api.StateRunning || sb.Idle.Action != api.ActionNone ||
		sb.Labels["e2b/k"] != "v" || sb.Env["A"] != "b" || len(sb.Network.Egress) != 0 {
		t.Fatalf("loam's sandbox %+v", sb)
	}
	if sb.Deadline == nil || sb.Deadline.Action != api.ActionDelete || !sb.Deadline.At.Equal(now.Add(120*time.Second)) {
		t.Fatalf("deadline %+v", sb.Deadline)
	}
	row := fx.row(id)
	if fx.token(id) != got["envdAccessToken"] || row.Project != "p" || row.TimeoutS != 120 || !row.EndAt.Equal(now.Add(120*time.Second)) || row.Paused {
		t.Fatalf("record %+v", row)
	}
	// envd took its token before create answered.
	e, boots := fx.envd(id)
	if boots != 1 || len(e.Inits()) != 1 || e.Inits()[0].AccessToken != fx.token(id) {
		t.Fatalf("envd: %d boots, inits %+v", boots, e.Inits())
	}

	// Defaults: no body is the base template for 300 s; autoPause pauses at the
	// deadline, and without its memory stops.
	id2 := fx.create(nil)
	if d := fx.sandbox(id2).Deadline; d == nil || !d.At.Equal(now.Add(300*time.Second)) {
		t.Fatalf("default timeout: %+v", d)
	}
	id3 := fx.create(map[string]any{"autoPause": true})
	if d := fx.sandbox(id3).Deadline; d.Action != api.ActionPause {
		t.Fatalf("autoPause: %+v", d)
	}
	id4 := fx.create(map[string]any{"autoPause": true, "autoPauseMemory": false})
	if d := fx.sandbox(id4).Deadline; d.Action != api.ActionStop {
		t.Fatalf("autoPause without memory: %+v", d)
	}
	// The deprecated v1 create defaults to 15 s.
	w = fx.do("POST", "/sandboxes", fx.key, nil)
	if w.Code != 201 {
		t.Fatalf("v1: %d %s", w.Code, w.Body)
	}
	if d := fx.sandbox(decode[map[string]any](t, w)["sandboxID"].(string)).Deadline; !d.At.Equal(now.Add(15 * time.Second)) {
		t.Fatalf("v1 timeout: %+v", d)
	}
}

func TestCreateErrors(t *testing.T) {
	fx := newFixture(t)
	wantErr(t, fx.do("POST", "/v2/sandboxes", "", nil), 401, "Invalid API key")
	wantErr(t, fx.do("POST", "/v2/sandboxes", "wrong", nil), 401, "Invalid API key")
	wantErr(t, fx.do("POST", "/v2/sandboxes", "loam_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil), 401, "Invalid API key")
	wantErr(t, fx.do("POST", "/v2/sandboxes", fx.key, map[string]any{"templateID": "python"}), 404, "template 'python' not found")
	wantErr(t, fx.do("POST", "/v2/sandboxes", fx.key, map[string]any{"timeout": 100 * 3600}), 400, "Timeout")
	wantErr(t, fx.do("POST", "/v2/sandboxes", fx.key, map[string]any{"timeout": 0}), 400, "Timeout")
	// Options this server can't honour are refused, not dropped.
	for body, msg := range map[string]string{
		`{"mcp": {"x": 1}}`: "mcp",
		`{"volumeMounts": [{"name": "v", "path": "/v"}]}`: "volumeMounts",
		`{"secure": false}`:                 "secure",
		`{"autoResume": {"enabled": true}}`: "autoResume",
		`{"autoPause": true, "autoPauseMemory": false, "autoResume": {"enabled": true}}`: "autoResume",
		`{"network": {"egressProxy": {"address": "x"}}}`:                                 "egressProxy",
		`{"metadata": {"a=b": "c"}}`:                                                     "label",
		`{"envVars": {"not a name": "x"}}`:                                               "env",
	} {
		w := fx.do("POST", "/v2/sandboxes", fx.key, []byte(body))
		if w.Code != 400 || !strings.Contains(w.String(), msg) {
			t.Errorf("%s: %d %s, want 400 about %s", body, w.Code, w.Body, msg)
		}
	}
	// Nulls are what an SDK sends for an option it wasn't given.
	if w := fx.do("POST", "/v2/sandboxes", fx.key, []byte(`{"mcp": null, "secure": true, "network": {"rules": null}}`)); w.Code != 201 {
		t.Fatalf("nulls: %d %s", w.Code, w.Body)
	}
	var all api.SandboxList
	fx.loam("GET", "/v1/sandboxes", fx.key, nil, &all)
	if len(all.Sandboxes) != 1 {
		t.Fatalf("%d sandboxes after refused creates", len(all.Sandboxes))
	}
}

func TestGetAndKill(t *testing.T) {
	fx := newFixture(t)
	id := fx.create(map[string]any{"metadata": map[string]string{"k": "v"}})
	w := fx.do("GET", "/sandboxes/"+id, fx.key, nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	d := decode[map[string]any](t, w)
	want := []string{"alias", "clientID", "cpuCount", "diskSizeMB", "domain", "endAt", "envdAccessToken", "envdVersion",
		"lifecycle", "memoryMB", "metadata", "sandboxID", "startedAt", "state", "templateID", "volumeMounts"}
	if k := keys(d); !slices.Equal(k, want) {
		t.Fatalf("keys %v\nwant %v", k, want)
	}
	if d["state"] != "running" || d["cpuCount"] != 2.0 || d["memoryMB"] != 1024.0 || d["diskSizeMB"] != 10240.0 {
		t.Fatalf("%v", d)
	}
	if md := d["metadata"].(map[string]any); len(md) != 1 || md["k"] != "v" {
		t.Fatalf("metadata %v", md)
	}
	if lc := d["lifecycle"].(map[string]any); lc["onTimeout"] != "kill" || lc["autoResume"] != false {
		t.Fatalf("lifecycle %v", lc)
	}
	if vm, ok := d["volumeMounts"].([]any); !ok || len(vm) != 0 {
		t.Fatalf("volumeMounts %v", d["volumeMounts"])
	}
	for _, f := range []string{"startedAt", "endAt"} {
		if _, err := time.Parse(time.RFC3339Nano, d[f].(string)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	// Labels given through loam's API are not E2B metadata.
	fx.loam("PATCH", "/v1/sandboxes/"+id, fx.key, map[string]any{"labels": map[string]string{"e2b/k": "v", "owner": "me"}}, nil)
	if md := decode[map[string]any](t, fx.do("GET", "/sandboxes/"+id, fx.key, nil))["metadata"].(map[string]any); len(md) != 1 {
		t.Fatalf("metadata after a loam label %v", md)
	}

	// Another project's key doesn't see it.
	msg := `Sandbox "` + id + `" doesn't exist or you don't have access to it`
	wantErr(t, fx.do("GET", "/sandboxes/"+id, fx.other, nil), 404, msg)
	wantErr(t, fx.do("DELETE", "/sandboxes/"+id, fx.other, nil), 404, msg)

	if w := fx.do("DELETE", "/sandboxes/"+id, fx.key, nil); w.Code != 204 {
		t.Fatalf("kill: %d %s", w.Code, w.Body)
	}
	if st, _ := fx.loam("GET", "/v1/sandboxes/"+id, fx.key, nil, nil); st != 404 {
		t.Fatalf("loam still has it: %d", st)
	}
	wantErr(t, fx.do("GET", "/sandboxes/"+id, fx.key, nil), 404, msg)
	wantErr(t, fx.do("DELETE", "/sandboxes/"+id, fx.key, nil), 404, msg)
	wantErr(t, fx.do("POST", "/v2/sandboxes/"+id+"/connect", fx.key, map[string]any{}), 404, msg)

	// A loam sandbox the adapter didn't make is not an E2B sandbox, even by ID.
	var sb api.Sandbox
	if st, raw := fx.loam("POST", "/v1/sandboxes", fx.key, map[string]any{}, &sb); st != 201 {
		t.Fatalf("loam create: %d %s", st, raw)
	}
	wantErr(t, fx.do("GET", "/sandboxes/"+sb.ID, fx.key, nil), 404, "doesn't exist")
	wantErr(t, fx.do("DELETE", "/sandboxes/"+sb.ID, fx.key, nil), 404, "doesn't exist")
	fx.sandbox(sb.ID)

	// Deleted through loam's API, it is gone from E2B's too.
	id2 := fx.create(nil)
	fx.loam("DELETE", "/v1/sandboxes/"+id2, fx.key, nil, nil)
	wantErr(t, fx.do("GET", "/sandboxes/"+id2, fx.key, nil), 404, "doesn't exist")
}

func TestTimeout(t *testing.T) {
	fx := newFixture(t)
	id := fx.create(nil)
	if w := fx.do("POST", "/sandboxes/"+id+"/timeout", fx.key, map[string]any{"timeout": 30}); w.Code != 204 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	end := fx.clock.Now().Add(30 * time.Second)
	if d := fx.sandbox(id).Deadline; d == nil || !d.At.Equal(end) || d.Action != api.ActionDelete || !fx.row(id).EndAt.Equal(end) {
		t.Fatalf("deadline %+v, endAt %v", d, fx.row(id).EndAt)
	}
	wantErr(t, fx.do("POST", "/sandboxes/"+id+"/timeout", fx.key, map[string]any{}), 400, "timeout")
	wantErr(t, fx.do("POST", "/sandboxes/nope/timeout", fx.key, map[string]any{"timeout": 3}), 404, "doesn't exist")
	// The deprecated refresh only extends.
	if w := fx.do("POST", "/sandboxes/"+id+"/refreshes", fx.key, map[string]any{"duration": 10}); w.Code != 204 || !fx.row(id).EndAt.Equal(end) {
		t.Fatalf("refresh shortened: %d, endAt %v", w.Code, fx.row(id).EndAt)
	}
	if w := fx.do("POST", "/sandboxes/"+id+"/refreshes", fx.key, map[string]any{"duration": 60}); w.Code != 204 || !fx.row(id).EndAt.Equal(fx.clock.Now().Add(time.Minute)) {
		t.Fatalf("refresh: %d, endAt %v", w.Code, fx.row(id).EndAt)
	}
}

// TestTemplates: a template beyond base is made from its own loam template,
// answers its own name, lists apart from base's, and refuses a tag.
func TestTemplates(t *testing.T) {
	fx := newFixture(t)
	w := fx.do("POST", "/sandboxes", fx.key, map[string]any{"templateID": "fountain", "timeout": 60})
	if w.Code != 201 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	got := decode[map[string]any](t, w)
	id := got["sandboxID"].(string)
	if got["alias"] != "fountain" || got["templateID"] != "fountain" {
		t.Fatalf("%v", got)
	}
	if sb := fx.sandbox(id); sb.Template != "e2b-fountain" || sb.Resources.MemoryMiB != 2048 {
		t.Fatalf("loam's sandbox %+v", sb)
	}
	if d := decode[map[string]any](t, fx.do("GET", "/sandboxes/"+id, fx.key, nil)); d["alias"] != "fountain" || d["templateID"] != "fountain" {
		t.Fatalf("detail %v", d)
	}
	// envd's defaults are the template's user and that user's home; base's
	// stay user's.
	if e, _ := fx.envd(id); e.Inits()[0].DefaultUser != "sprite" || e.Inits()[0].DefaultWorkdir != "/home/sprite" {
		t.Fatalf("fountain's init %+v", e.Inits()[0])
	}
	base := fx.create(nil)
	if e, _ := fx.envd(base); e.Inits()[0].DefaultUser != "user" || e.Inits()[0].DefaultWorkdir != "/home/user" {
		t.Fatalf("base's init %+v", e.Inits()[0])
	}
	for tmpl, want := range map[string]string{"fountain": id, "base": base} {
		l := decode[[]map[string]any](t, fx.do("GET", "/v2/sandboxes?template="+tmpl, fx.key, nil))
		if len(l) != 1 || l[0]["sandboxID"] != want {
			t.Fatalf("list of %s: %v", tmpl, l)
		}
	}
	if w := fx.do("POST", "/sandboxes", fx.key, map[string]any{"templateID": "fountain:v2"}); w.Code != 404 {
		t.Fatalf("a tag: %d %s", w.Code, w.Body)
	}
	if w := fx.do("POST", "/sandboxes", fx.key, map[string]any{"templateID": "e2b-fountain"}); w.Code != 404 {
		t.Fatalf("a loam template's own name: %d %s", w.Code, w.Body)
	}
}

// TestConnectUnversioned: POST /sandboxes/{id}/connect is v2's connect.
func TestConnectUnversioned(t *testing.T) {
	fx := newFixture(t)
	id := fx.create(map[string]any{"timeout": 600})
	if w := fx.do("POST", "/sandboxes/"+id+"/pause", fx.key, map[string]any{}); w.Code != 204 {
		t.Fatalf("pause: %d %s", w.Code, w.Body)
	}
	if w := fx.do("POST", "/sandboxes/"+id+"/connect", fx.key, map[string]any{"timeout": 60}); w.Code != 201 {
		t.Fatalf("connect: %d %s", w.Code, w.Body)
	}
	if sb := fx.sandbox(id); sb.State != api.StateRunning {
		t.Fatalf("after connect: %s", sb.State)
	}
}

func TestPauseConnect(t *testing.T) {
	fx := newFixture(t)
	id := fx.create(map[string]any{"timeout": 600})
	if w := fx.do("POST", "/sandboxes/"+id+"/pause", fx.key, map[string]any{}); w.Code != 204 {
		t.Fatalf("pause: %d %s", w.Code, w.Body)
	}
	if sb := fx.sandbox(id); sb.State != api.StatePaused || sb.Deadline != nil {
		t.Fatalf("loam's after pause: %s, deadline %+v", sb.State, sb.Deadline)
	}
	d := decode[map[string]any](t, fx.do("GET", "/sandboxes/"+id, fx.key, nil))
	if d["state"] != "paused" || d["volumeMounts"] != nil {
		t.Fatalf("after pause %v", d)
	}
	if end, _ := time.Parse(time.RFC3339Nano, d["endAt"].(string)); !end.Equal(fx.clock.Now()) {
		t.Fatalf("endAt %v is not the pause time", end)
	}
	wantErr(t, fx.do("POST", "/sandboxes/"+id+"/pause", fx.key, map[string]any{}), 409,
		"Error pausing sandbox - sandbox '"+id+"' is already paused")
	wantErr(t, fx.do("POST", "/sandboxes/"+id+"/timeout", fx.key, map[string]any{"timeout": 30}), 404, "paused")
	// Its traffic does not wake it.
	if w := fx.do("POST", "/process.Process/List", "", map[string]any{}, "E2b-Sandbox-Id", id, "X-Access-Token", fx.token(id)); w.Code != 502 ||
		decode[map[string]any](t, w)["message"] != "The sandbox was not found" {
		t.Fatalf("envd traffic to a paused sandbox: %d %s", w.Code, w.Body)
	}
	if fx.sandbox(id).State != api.StatePaused {
		t.Fatal("traffic woke a paused sandbox")
	}

	fx.clock.Advance(time.Minute)
	w := fx.do("POST", "/v2/sandboxes/"+id+"/connect", fx.key, map[string]any{})
	if w.Code != 201 {
		t.Fatalf("resume: %d %s", w.Code, w.Body)
	}
	if k := keys(decode[map[string]any](t, w)); !slices.Equal(k, []string{"alias", "clientID", "domain", "envdAccessToken", "envdVersion", "sandboxID", "templateID"}) {
		t.Fatalf("keys %v", k)
	}
	// Resumed with the default 300 s, not the 600 it was created with, as hosted does.
	now := fx.clock.Now()
	row, sb := fx.row(id), fx.sandbox(id)
	if row.Paused || !row.EndAt.Equal(now.Add(300*time.Second)) || !row.StartedAt.Equal(now) || sb.State != api.StateRunning ||
		sb.Deadline == nil || !sb.Deadline.At.Equal(row.EndAt) {
		t.Fatalf("after resume: %+v, loam's %s %+v", row, sb.State, sb.Deadline)
	}
	// A resume from memory is the same boot: envd keeps its token, no new /init.
	if e, boots := fx.envd(id); boots != 1 || len(e.Inits()) != 1 {
		t.Fatalf("after a memory resume: %d boots, %d inits", boots, len(e.Inits()))
	}
	// Connecting to a running sandbox extends, never shortens.
	w = fx.do("POST", "/v2/sandboxes/"+id+"/connect", fx.key, map[string]any{"timeout": 10})
	if w.Code != 200 || !fx.row(id).EndAt.Equal(row.EndAt) {
		t.Fatalf("connect running: %d, endAt %v -> %v", w.Code, row.EndAt, fx.row(id).EndAt)
	}
	w = fx.do("POST", "/v2/sandboxes/"+id+"/connect", fx.key, map[string]any{"timeout": 900})
	if w.Code != 200 || !fx.sandbox(id).Deadline.At.Equal(now.Add(900*time.Second)) {
		t.Fatalf("connect extend: %d, deadline %v", w.Code, fx.sandbox(id).Deadline)
	}
	// The deprecated resume is 409 on a running sandbox.
	wantErr(t, fx.do("POST", "/sandboxes/"+id+"/resume", fx.key, map[string]any{}), 409, "already running")

	// Without its memory a pause is loam's stop, and the connect a fresh boot,
	// whose envd is initialized again with the same token.
	if w := fx.do("POST", "/sandboxes/"+id+"/pause", fx.key, map[string]any{"memory": false}); w.Code != 204 {
		t.Fatalf("pause without memory: %d %s", w.Code, w.Body)
	}
	if fx.sandbox(id).State != api.StateStopped {
		t.Fatalf("loam's state %s", fx.sandbox(id).State)
	}
	if w := fx.do("POST", "/sandboxes/"+id+"/resume", fx.key, map[string]any{"timeout": 60}); w.Code != 201 {
		t.Fatalf("v1 resume: %d %s", w.Code, w.Body)
	}
	e, boots := fx.envd(id)
	if boots != 2 || len(e.Inits()) != 1 || e.Inits()[0].AccessToken != fx.token(id) {
		t.Fatalf("after a fresh boot: %d boots, inits %+v", boots, e.Inits())
	}
}

// An autoPause sandbox past its deadline is paused, whether or not loam has
// got round to pausing it yet.
func TestAutoPauseState(t *testing.T) {
	now := time.Now()
	row := db.E2BSandbox{AutoPause: true, EndAt: now.Add(-time.Second)}
	if stateOf(row, now) != "paused" {
		t.Fatal("not paused past its deadline")
	}
	row.AutoPause = false
	if stateOf(row, now) != "running" {
		t.Fatal("a kill-on-timeout sandbox is running until it is deleted")
	}
}

// autoResume: traffic for a paused sandbox resumes it, with the timeout it
// was created with.
func TestAutoResume(t *testing.T) {
	fx := newFixture(t)
	id := fx.create(map[string]any{"timeout": 120, "autoPause": true, "autoResume": map[string]any{"enabled": true}})
	d := decode[map[string]any](t, fx.do("GET", "/sandboxes/"+id, fx.key, nil))
	if lc := d["lifecycle"].(map[string]any); lc["onTimeout"] != "pause" || lc["autoResume"] != true {
		t.Fatalf("lifecycle %v", lc)
	}
	fx.do("POST", "/sandboxes/"+id+"/pause", fx.key, nil)
	// Unauthorized traffic doesn't.
	if w := fx.do("GET", "/x", "", nil, "E2b-Sandbox-Id", id); w.Code != 401 || fx.sandbox(id).State != api.StatePaused {
		t.Fatalf("unauthorized: %d, %s", w.Code, fx.sandbox(id).State)
	}
	w := fx.do("GET", "/x", "", nil, "E2b-Sandbox-Id", id, "X-Access-Token", fx.token(id))
	if w.Code != 200 || w.String() != "envd /x in "+id {
		t.Fatalf("traffic: %d %s", w.Code, w.Body)
	}
	row := fx.row(id)
	if row.Paused || !row.EndAt.Equal(fx.clock.Now().Add(120*time.Second)) || fx.sandbox(id).State != api.StateRunning {
		t.Fatalf("after auto-resume: %+v", row)
	}
}

func TestList(t *testing.T) {
	fx := newFixture(t)
	var ids []string
	base := fx.clock.Now().Add(-time.Hour)
	for i := range 5 {
		id := fx.create(map[string]any{"metadata": map[string]string{"run": "r", "n": string(rune('a' + i))}})
		// Distinct start times, oldest first.
		fx.store.UpdateE2B(context.Background(), id, func(e *db.E2BSandbox) error {
			e.StartedAt = base.Add(time.Duration(i) * time.Minute)
			return nil
		})
		ids = append(ids, id)
	}
	fx.create(map[string]any{"metadata": map[string]string{"run": "other"}})
	fx.do("POST", "/sandboxes/"+ids[1]+"/pause", fx.key, nil)
	// Neither another project's sandboxes nor the project's own loam ones are listed.
	fx.loam("POST", "/v1/sandboxes", fx.key, map[string]any{"labels": map[string]string{"e2b/run": "r"}}, nil)
	if w := fx.do("POST", "/v2/sandboxes", fx.other, map[string]any{"metadata": map[string]string{"run": "r"}}); w.Code != 201 {
		t.Fatalf("other's create: %d", w.Code)
	}

	var got []string
	tok := ""
	pages := 0
	for {
		path := "/v2/sandboxes?limit=2&metadata=run%3Dr"
		if tok != "" {
			path += "&nextToken=" + tok
		}
		w := fx.do("GET", path, fx.key, nil)
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		if w.Header.Get("X-Total-Running") != "4" {
			t.Fatalf("X-Total-Running %q", w.Header.Get("X-Total-Running"))
		}
		items := decode[[]map[string]any](t, w)
		for _, it := range items {
			if _, ok := it["envdAccessToken"]; ok {
				t.Fatal("a listed sandbox carries its token")
			}
			if _, ok := it["lifecycle"]; ok {
				t.Fatal("a listed sandbox carries its lifecycle")
			}
			got = append(got, it["sandboxID"].(string))
		}
		pages++
		if tok = w.Header.Get("X-Next-Token"); tok == "" {
			break
		}
	}
	want := []string{ids[4], ids[3], ids[2], ids[1], ids[0]} // newest first
	if !slices.Equal(got, want) || pages != 3 {
		t.Fatalf("got %v in %d pages, want %v in 3", got, pages, want)
	}

	items := decode[[]map[string]any](t, fx.do("GET", "/v2/sandboxes?state=paused", fx.key, nil))
	if len(items) != 1 || items[0]["sandboxID"] != ids[1] || items[0]["state"] != "paused" {
		t.Fatalf("paused: %v", items)
	}
	if w := fx.do("GET", "/v2/sandboxes?state=paused", fx.key, nil); w.Header.Get("X-Total-Running") != "" {
		t.Fatal("X-Total-Running on a paused-only listing")
	}
	items = decode[[]map[string]any](t, fx.do("GET", "/v2/sandboxes?order=asc&limit=1&metadata=run%3Dr%26n%3Dc", fx.key, nil))
	if len(items) != 1 || items[0]["sandboxID"] != ids[2] {
		t.Fatalf("metadata k=v&k=v: %v", items)
	}
	if items := decode[[]map[string]any](t, fx.do("GET", "/v2/sandboxes", fx.key, nil)); len(items) != 6 {
		t.Fatalf("unfiltered: %d", len(items))
	}
	if items := decode[[]map[string]any](t, fx.do("GET", "/v2/sandboxes?template=python", fx.key, nil)); len(items) != 0 {
		t.Fatalf("unknown template: %v", items)
	}
	wantErr(t, fx.do("GET", "/v2/sandboxes?nextToken=bm90LWEtY3Vyc29y", fx.key, nil), 400, "Invalid next token")
	wantErr(t, fx.do("GET", "/v2/sandboxes?metadata=nokv", fx.key, nil), 400, "metadata")
	wantErr(t, fx.do("GET", "/v2/sandboxes?state=dead", fx.key, nil), 400, "state")
	if items := decode[[]map[string]any](t, fx.do("GET", "/sandboxes", fx.key, nil)); len(items) != 5 {
		t.Fatalf("v1 list (running only): %d", len(items))
	}
}

func TestRoute(t *testing.T) {
	fx := newFixture(t)
	for _, c := range []struct {
		host, id string
		port     int
		hdr      []string
		api      bool
	}{
		{host: "127.0.0.1:7823", api: true},
		{host: apiHost, api: true},
		{host: domain, api: true},
		{host: "8080-sb-abcdefghijkl.e2b.test:443", id: "sb-abcdefghijkl", port: 8080},
		{host: "49983-sb-abcdefghijkl.e2b.test", id: "sb-abcdefghijkl", port: 49983},
		{host: "8080-SB-ABCDEFGHIJKL.E2B.test", id: "sb-abcdefghijkl", port: 8080},
		{host: apiHost, hdr: []string{"E2b-Sandbox-Id", "sb-abc", "E2b-Sandbox-Port", "49983"}, id: "sb-abc", port: 49983},
		{host: apiHost, hdr: []string{"E2b-Sandbox-Id", "sb-abc"}, id: "sb-abc", port: 49983},
		{host: "x-sb-abc.e2b.test", api: true},
		{host: "8080-sb-abc.other.test", api: true},
	} {
		r := httptest.NewRequest("POST", "/process.Process/List", nil)
		r.Host = c.host
		for i := 0; i < len(c.hdr); i += 2 {
			r.Header.Set(c.hdr[i], c.hdr[i+1])
		}
		tg, ok := fx.f.route(r)
		if ok == c.api || (!c.api && (tg.id != c.id || tg.port != c.port)) {
			t.Errorf("%s %v: got %+v %v", c.host, c.hdr, tg, ok)
		}
	}
	// The adapter's hosts are its own; every other one is loam's.
	for host, ours := range map[string]bool{apiHost: true, domain: true, "8080-sb-abc.e2b.test": true, "api.loam.test": false,
		"x.y.e2b.test": false, "e2b.test.evil": false, "8080--box--p.loam.test": false} {
		if fx.f.ours(host) != ours {
			t.Errorf("ours(%q) = %v", host, !ours)
		}
	}
	// A domain with a port (a local loam-api) is matched without it, and
	// reported with it, so that getHost(port) reaches that server.
	local := New(Options{Domain: "E2B.localhost:7901"}, fx.srv, fx.store)
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "8080-sb-abc.e2b.localhost:7901"
	rec := httptest.NewRecorder()
	local.writeSandbox(rec, 201, db.E2BSandbox{}, "")
	var answer sandboxJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if tg, ok := local.route(r); !local.ours(r.Host) || !local.ours("api.e2b.localhost:7901") || !ok || tg.port != 8080 ||
		answer.Domain != "e2b.localhost:7901" {
		t.Errorf("a domain with a port: %+v %v", tg, ok)
	}
	// So loam's own API goes on working behind it.
	if st, raw := fx.loam("GET", "/v1/whoami", fx.key, nil, nil); st != 200 {
		t.Fatalf("loam behind the adapter: %d %s", st, raw)
	}
}

func TestSignatureRouting(t *testing.T) {
	fx := newFixture(t)
	a, b := fx.create(nil), fx.create(nil)
	c := ""
	if w := fx.do("POST", "/v2/sandboxes", fx.other, nil); w.Code == 201 {
		c = decode[map[string]any](t, w)["sandboxID"].(string)
	}
	q := func(path, user, sig, exp string) string {
		v := "/files?path=" + url.QueryEscape(path) + "&signature=" + url.QueryEscape(sig)
		if user != "" {
			v += "&username=" + user
		}
		if exp != "" {
			v += "&signature_expiration=" + exp
		}
		return v
	}
	for _, tc := range []struct {
		method, url, want string
	}{
		{"GET", q("/tmp/x", "", fileSignature("/tmp/x", "read", "", fx.token(b), ""), ""), b},
		{"POST", q("/tmp/x", "user", fileSignature("/tmp/x", "write", "user", fx.token(a), "1790988051"), "1790988051"), a},
		{"GET", q("/tmp/x", "", fileSignature("/tmp/x", "read", "", fx.token(c), ""), ""), c},    // another project's: the token is the credential
		{"GET", q("/tmp/x", "", fileSignature("/tmp/x", "write", "", fx.token(a), ""), ""), ""},  // signed for a write
		{"GET", q("/tmp/y", "", fileSignature("/tmp/x", "read", "", fx.token(a), ""), ""), ""},   // another path
		{"GET", q("/tmp/x", "", fileSignature("/tmp/x", "read", "", "not-a-token", ""), ""), ""}, // nobody's token
	} {
		r := httptest.NewRequest(tc.method, tc.url, nil)
		tg, ok := fx.f.route(r)
		if !ok || (tc.want == "") != tg.unmatched || tg.id != tc.want || (tc.want != "" && tg.port != EnvdPort) {
			t.Errorf("%s %s: got %+v %v, want %q", tc.method, tc.url, tg, ok, tc.want)
		}
	}
	// A vector from the SDK itself: e2b.sandbox.signature.get_signature("/a", "read", None, "tok").
	if got := fileSignature("/a", "read", "", "tok", ""); got != "v1_2/gNbgFPxFSqkheS/SD0NHlzA121E/nxkOtc/ovxeoI" {
		t.Errorf("signature %s", got)
	}
	// A paused sandbox's URLs do not route.
	fx.do("POST", "/sandboxes/"+a+"/pause", fx.key, nil)
	r := httptest.NewRequest("GET", q("/tmp/x", "", fileSignature("/tmp/x", "read", "", fx.token(a), ""), ""), nil)
	if tg, _ := fx.f.route(r); !tg.unmatched {
		t.Errorf("paused sandbox matched: %+v", tg)
	}
	wantErr(t, fx.do("GET", q("/tmp/x", "", "v1_nope", ""), "", nil), 401, "signature")
	// And one that routes reaches envd.
	w := fx.do("GET", q("/tmp/x", "", fileSignature("/tmp/x", "read", "", fx.token(b), ""), ""), "", nil)
	if w.Code != 200 || w.String() != "envd /files in "+b {
		t.Fatalf("signed download: %d %s", w.Code, w.Body)
	}
}

// envelope is one Connect envelope.
func envelope(flags byte, msg string) []byte {
	b := make([]byte, 5, 5+len(msg))
	b[0] = flags
	binary.BigEndian.PutUint32(b[1:], uint32(len(msg)))
	return append(b, msg...)
}

func TestProxy(t *testing.T) {
	fx := newFixture(t)
	id := fx.create(nil)
	var mu sync.Mutex
	var seen *http.Request
	e, _ := fx.envd(id)
	e.set(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = r
		mu.Unlock()
		switch r.URL.Path {
		case "/process.Process/Start": // a stream that ends properly
			w.Header().Set("Content-Type", "application/connect+json")
			w.Write(envelope(0, `{"event":{"start":{"pid":7}}}`))
			w.(http.Flusher).Flush()
			w.Write(envelope(2, `{}`))
		case "/process.Process/Connect": // a stream the guest drops mid-way, as a pause does
			w.Header().Set("Content-Type", "application/connect+json")
			w.Write(envelope(0, `{"event":{"start":{"pid":7}}}`))
			w.(http.Flusher).Flush()
			conn, _, _ := http.NewResponseController(w).Hijack()
			conn.Close()
		default:
			io.WriteString(w, "envd "+r.Host)
		}
	}))

	tok := fx.token(id)
	w := fx.do("POST", "/process.Process/Start", "", nil, "E2b-Sandbox-Id", id, "E2b-Sandbox-Port", "49983", "X-Access-Token", tok)
	want := append(envelope(0, `{"event":{"start":{"pid":7}}}`), envelope(2, `{}`)...)
	if w.Code != 200 || !bytes.Equal(w.Body, want) {
		t.Fatalf("stream: %d %q", w.Code, w.Body)
	}
	mu.Lock()
	if seen.Header.Get("X-Access-Token") != tok {
		t.Fatal("X-Access-Token was not passed through")
	}
	mu.Unlock()

	w = fx.do("POST", "/process.Process/Connect", "", nil, "E2b-Sandbox-Id", id, "X-Access-Token", tok)
	first := envelope(0, `{"event":{"start":{"pid":7}}}`)
	if !bytes.HasPrefix(w.Body, first) {
		t.Fatalf("cut stream: %q", w.Body)
	}
	end := w.Body[len(first):]
	if len(end) < 5 || end[0] != 2 || !strings.Contains(string(end[5:]), `"code":"unavailable"`) ||
		!strings.Contains(string(end[5:]), "the connection to sandbox "+id+" ended before the stream completed") {
		t.Fatalf("cut stream ends with %q", end)
	}

	// User traffic by Host keeps the Host, and needs no token: ports are public.
	host := "8080-" + id + "." + domain
	fx.node.Listen(id, 8080, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "port "+r.Host) }))
	w = fx.do("GET", "/", "", nil, "Host", host)
	if w.Code != 200 || w.String() != "port "+host {
		t.Fatalf("port: %d %q", w.Code, w.Body)
	}
	// envd by Host, as the SDKs reach it without E2B_SANDBOX_URL.
	w = fx.do("GET", "/x", "", nil, "Host", "49983-"+id+"."+domain, "X-Access-Token", tok)
	if w.Code != 200 || w.String() != "envd 49983-"+id+"."+domain {
		t.Fatalf("envd by host: %d %q", w.Code, w.Body)
	}
	// envd's internal API is not reachable from outside.
	wantErr(t, fx.do("POST", "/init", "", map[string]any{}, "E2b-Sandbox-Id", id), 404, "internal")
	// No such sandbox: the proxy's 502.
	w = fx.do("GET", "/health", "", nil, "E2b-Sandbox-Id", "sb-nope")
	if got := decode[map[string]any](t, w); w.Code != 502 || got["sandboxId"] != "sb-nope" || got["code"] != 502.0 {
		t.Fatalf("unknown sandbox: %d %v", w.Code, got)
	}
	// Nothing listening: 502, port not open.
	w = fx.do("GET", "/", "", nil, "Host", "3000-"+id+"."+domain)
	if got := decode[map[string]any](t, w); w.Code != 502 || got["message"] != "The sandbox is running but port is not open" {
		t.Fatalf("closed port: %d %v", w.Code, got)
	}
}

// A pause ends the streams open on the sandbox, and a Connect stream ends
// with an end-stream envelope rather than cut off.
func TestPauseEndsStreams(t *testing.T) {
	fx := newFixture(t)
	id := fx.create(nil)
	e, _ := fx.envd(id)
	started := make(chan struct{})
	e.set(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/connect+json")
		w.Write(envelope(0, `{"event":{"start":{"pid":9}}}`))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	done := make(chan response)
	go func() {
		done <- fx.do("POST", "/process.Process/Start", "", nil, "E2b-Sandbox-Id", id, "X-Access-Token", fx.token(id))
	}()
	<-started
	if w := fx.do("POST", "/sandboxes/"+id+"/pause", fx.key, nil); w.Code != 204 {
		t.Fatalf("pause: %d %s", w.Code, w.Body)
	}
	select {
	case w := <-done:
		first := envelope(0, `{"event":{"start":{"pid":9}}}`)
		if !bytes.HasPrefix(w.Body, first) || len(w.Body) <= len(first) || w.Body[len(first)] != 2 ||
			!strings.Contains(string(w.Body[len(first):]), "unavailable") {
			t.Fatalf("stream across a pause: %q", w.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream outlived the pause")
	}
}

// envd traffic without the sandbox's token (or a good signature) is refused
// before the sandbox is woken; user ports need nothing.
func TestEnvdAuthBeforeWake(t *testing.T) {
	fx := newFixture(t)
	id := fx.create(nil)
	fx.node.Listen(id, 8080, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	tok := fx.token(id)
	future := strconv.FormatInt(fx.clock.Now().Add(time.Hour).Unix(), 10)
	past := strconv.FormatInt(fx.clock.Now().Add(-time.Hour).Unix(), 10)
	files := func(path, user, op, token, exp string) string {
		v := "/files?path=" + url.QueryEscape(path) + "&username=" + user + "&signature=" + url.QueryEscape(fileSignature(path, op, user, token, exp))
		if exp != "" {
			v += "&signature_expiration=" + exp
		}
		return v
	}
	envd := []string{"E2b-Sandbox-Id", id, "E2b-Sandbox-Port", "49983"}
	cases := []struct {
		name, method, path string
		hdr                []string
		ok                 bool
	}{
		{"no token", "POST", "/process.Process/Start", envd, false},
		{"wrong token", "POST", "/process.Process/Start", append(envd, "X-Access-Token", "nope"), false},
		{"token", "POST", "/process.Process/Start", append(envd, "X-Access-Token", tok), true},
		{"no token, by host", "GET", "/health", []string{"Host", "49983-" + id + "." + domain}, false},
		{"token, by host", "GET", "/health", []string{"Host", "49983-" + id + "." + domain, "X-Access-Token", tok}, true},
		{"signed read", "GET", files("/tmp/x", "user", "read", tok, future), envd, true},
		{"signed read, no expiry", "GET", files("/tmp/x", "user", "read", tok, ""), envd, true},
		{"signed write", "POST", files("/tmp/x", "user", "write", tok, future), envd, true},
		{"expired signature", "GET", files("/tmp/x", "user", "read", tok, past), envd, false},
		{"signature for another op", "POST", files("/tmp/x", "user", "read", tok, future), envd, false},
		{"signature with another token", "GET", files("/tmp/x", "user", "read", "other", future), envd, false},
		{"signature off /files", "GET", "/process.Process/List?signature=x", envd, false},
		{"user port, no token", "GET", "/", []string{"Host", "8080-" + id + "." + domain}, true},
	}
	for _, c := range cases {
		// Asleep in loam (a drain, say) while E2B calls it running: only
		// traffic that envd would take wakes it.
		if st, raw := fx.loam("POST", "/v1/sandboxes/"+id+"/pause", fx.key, nil, nil); st != 200 {
			t.Fatalf("loam pause: %d %s", st, raw)
		}
		w := fx.do(c.method, c.path, "", nil, c.hdr...)
		woke := fx.sandbox(id).State == api.StateRunning
		if c.ok && (w.Code >= 300 || !woke) {
			t.Errorf("%s: %d %s (woke %v), want it served", c.name, w.Code, w.Body, woke)
		}
		if !c.ok && (w.Code != 401 || woke) {
			t.Errorf("%s: %d %s (woke %v), want 401 without waking it", c.name, w.Code, w.Body, woke)
		}
	}
}

func TestEnvelopes(t *testing.T) {
	stream := append(append(envelope(0, `{"a":1}`), envelope(0, ``)...), envelope(2, `{}`)...)
	for split := range len(stream) {
		var e envelopes
		e.advance(stream[:split])
		if e.ended {
			t.Fatalf("ended after %d bytes", split)
		}
		e.advance(stream[split:])
		if !e.ended || !e.atBoundary() {
			t.Fatalf("split at %d: %+v", split, e)
		}
	}
	var e envelopes
	e.advance(envelope(0, `{"a":1}`)[:7])
	if e.atBoundary() {
		t.Fatal("inside a message is not a boundary")
	}
}

func TestInitEnvd(t *testing.T) {
	fx := newFixture(t)
	id := fx.create(map[string]any{"envVars": map[string]string{"K": "V"}})
	e, _ := fx.envd(id)
	got := e.Inits()[0]
	if got.AccessToken != fx.token(id) || got.DefaultUser != "user" || got.DefaultWorkdir != "/home/user" || got.EnvVars["K"] != "V" ||
		got.EnvVars["E2B_SANDBOX"] != "true" || got.EnvVars["E2B_SANDBOX_ID"] != id || got.EnvVars["E2B_TEMPLATE_ID"] != "base" ||
		got.EnvVars["NODE_EXTRA_CA_CERTS"] != "" {
		t.Fatalf("init %+v", got)
	}
	if ts, err := time.Parse(time.RFC3339Nano, got.Timestamp); err != nil || !ts.Equal(fx.clock.Now()) {
		t.Fatalf("timestamp %q", got.Timestamp)
	}
	tok := []string{"E2b-Sandbox-Id", id, "X-Access-Token", fx.token(id)}
	// The same boot: once is enough.
	for range 3 {
		if w := fx.do("GET", "/x", "", nil, tok...); w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
	if n := len(e.Inits()); n != 1 {
		t.Fatalf("%d inits in one boot", n)
	}
	// Stopped and woken through loam's API, which E2B never hears of: the
	// next E2B call finds a fresh boot and initializes its envd first.
	fx.loam("POST", "/v1/sandboxes/"+id+"/stop", fx.key, nil, nil)
	if w := fx.do("GET", "/x", "", nil, tok...); w.Code != 200 {
		t.Fatalf("after a stop: %d %s", w.Code, w.Body)
	}
	e2, boots := fx.envd(id)
	if boots != 2 || len(e2.Inits()) != 1 || e2.Inits()[0].AccessToken != fx.token(id) {
		t.Fatalf("after a fresh boot: %d boots, %+v", boots, e2.Inits())
	}
	// Paused and woken: the same boot, envd as it was.
	fx.loam("POST", "/v1/sandboxes/"+id+"/pause", fx.key, nil, nil)
	fx.do("GET", "/x", "", nil, tok...)
	if _, boots := fx.envd(id); boots != 2 || len(e2.Inits()) != 1 {
		t.Fatalf("after a memory wake: %d boots, %d inits", boots, len(e2.Inits()))
	}
	// A loam-api that restarted knows no boot: one more /init, the same token.
	fx.f.forgetInit(id)
	fx.do("GET", "/x", "", nil, tok...)
	if n := len(e2.Inits()); n != 2 {
		t.Fatalf("%d inits after forgetting", n)
	}
	// envd that took another token first: the call fails rather than reach it.
	fx.f.forgetInit(id)
	e2.mu.Lock()
	e2.token = "someone-elses"
	e2.mu.Unlock()
	if w := fx.do("GET", "/x", "", nil, tok...); w.Code != 502 {
		t.Fatalf("envd with another token: %d %s", w.Code, w.Body)
	}

	// A sandbox with secrets gets their placeholders and the broker CA's variables.
	if st, raw := fx.loam("PUT", "/v1/secrets/GH", fx.key, map[string]any{"value": "ghp_x", "hosts": []string{"api.github.com"}}, nil); st != 200 {
		t.Fatalf("secret: %d %s", st, raw)
	}
	id2 := fx.create(nil)
	fx.loam("PATCH", "/v1/sandboxes/"+id2, fx.key, map[string]any{"secrets": []string{"GH"}}, nil)
	fx.loam("POST", "/v1/sandboxes/"+id2+"/stop", fx.key, nil, nil)
	fx.do("GET", "/x", "", nil, "E2b-Sandbox-Id", id2, "X-Access-Token", fx.token(id2))
	e3, _ := fx.envd(id2)
	env := e3.Inits()[0].EnvVars
	if env["GH"] != "__loam_gh__" || env["NODE_EXTRA_CA_CERTS"] != caFile || env["SSL_CERT_FILE"] != caBundle {
		t.Fatalf("env with a secret: %v", env)
	}
}

// envd that refuses /init, or never answers, fails the create, which leaves
// nothing behind.
func TestCreateFailsWithoutEnvd(t *testing.T) {
	fx := newFixture(t)
	fx.node.StartAtBoot(EnvdPort, func(vm string) http.Handler { return &fakeEnvd{vm: vm, refuse: http.StatusUnauthorized} })
	wantErr(t, fx.do("POST", "/v2/sandboxes", fx.key, nil), 500, "another token")
	fx.node.StartAtBoot(EnvdPort, func(vm string) http.Handler { return nil })
	start := time.Now()
	wantErr(t, fx.do("POST", "/v2/sandboxes", fx.key, nil), 500, "did not take /init")
	if time.Since(start) > 5*time.Second {
		t.Fatalf("waited %v for envd", time.Since(start))
	}
	var all api.SandboxList
	fx.loam("GET", "/v1/sandboxes", fx.key, nil, &all)
	if len(all.Sandboxes) != 0 {
		t.Fatalf("left behind: %+v", all.Sandboxes)
	}
}

// A sandbox port is a number from 1 to 65535, normalized, whichever way it is
// named: anything else could slip past the internal-endpoint check (049983)
// or be spliced into the dial (49983&host=...), so it is refused.
func TestSandboxPortsAreNumbers(t *testing.T) {
	fx := newFixture(t)
	id := fx.create(nil)
	for _, c := range []struct {
		host, header string
		want         int
		bad          bool
	}{
		{host: "8080-" + id + "." + domain, want: 8080},
		{host: "049983-" + id + "." + domain, want: 49983},
		{host: "0-" + id + "." + domain, bad: true},
		{host: "70000-" + id + "." + domain, bad: true},
		{header: "49983&host=10.0.0.1", bad: true},
		{header: "+80", want: 80},
		{header: "x", bad: true},
		{header: "0049983", want: 49983},
	} {
		r := httptest.NewRequest("GET", "/health", nil)
		if c.host != "" {
			r.Host = c.host
		} else {
			r.Header.Set("E2b-Sandbox-Id", id)
			r.Header.Set("E2b-Sandbox-Port", c.header)
		}
		tg, ok := fx.f.route(r)
		if !ok || tg.badPort != c.bad || (!c.bad && tg.port != c.want) {
			t.Errorf("host %q header %q: got %+v, want port %d bad %v", c.host, c.header, tg, c.want, c.bad)
		}
	}
	// A refused port answers 400 before anything is dialled, and envd's
	// internal endpoints stay unreachable however the port or path is spelled.
	wantErr(t, fx.do("POST", "/init", "", nil, "E2b-Sandbox-Id", id, "E2b-Sandbox-Port", "49983&x=1"), 400, "port")
	wantErr(t, fx.do("POST", "/init", "", nil, "Host", "049983-"+id+"."+domain), 404, "internal")
	wantErr(t, fx.do("POST", "//init", "", nil, "E2b-Sandbox-Id", id), 404, "internal")
	wantErr(t, fx.do("POST", "/x/../init", "", nil, "E2b-Sandbox-Id", id), 404, "internal")
}

// No node with room is hosted's 429 for too many sandboxes, which the SDKs
// retry after Retry-After.
func TestCreateWithoutRoom(t *testing.T) {
	fx := newFixtureMem(t, 1024)
	fx.create(nil)
	w := fx.do("POST", "/v2/sandboxes", fx.key, nil)
	wantErr(t, w, 429, "capacity")
	if w.Header.Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
}

func TestHealth(t *testing.T) {
	fx := newFixture(t)
	if w := fx.do("GET", "/health", "", nil); w.Code != 200 || w.String() != "Health check successful" {
		t.Fatalf("health: %d %q", w.Code, w.Body)
	}
	// On a sandbox's host it is the sandbox's own path.
	id := fx.create(nil)
	fx.node.Listen(id, 8080, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "app") }))
	if w := fx.do("GET", "/health", "", nil, "Host", "8080-"+id+"."+domain); w.String() != "app" {
		t.Fatalf("health on a sandbox host: %d %q", w.Code, w.Body)
	}
	wantErr(t, fx.do("GET", "/templates", fx.key, nil), 404, "does not implement")
}

func TestMetrics(t *testing.T) {
	fx := newFixture(t)
	id := fx.create(nil)
	ms := decode[[]map[string]any](t, fx.do("GET", "/sandboxes/"+id+"/metrics", fx.key, nil))
	if len(ms) != 1 || ms[0]["cpuCount"] != 2.0 || ms[0]["timestampUnix"] != 1790987920.0 || ms[0]["timestamp"] != "2026-10-03T00:38:40Z" {
		t.Fatalf("metrics %v", ms)
	}
	// Asleep, there is no sample, and asking doesn't wake it.
	fx.loam("POST", "/v1/sandboxes/"+id+"/pause", fx.key, nil, nil)
	if ms := decode[[]map[string]any](t, fx.do("GET", "/sandboxes/"+id+"/metrics", fx.key, nil)); len(ms) != 0 || fx.sandbox(id).State != api.StatePaused {
		t.Fatalf("metrics of a sleeping sandbox %v", ms)
	}
}

// What loam can enforce of E2B's egress options it does; the rest is refused.
func TestEgress(t *testing.T) {
	fx := newFixture(t)
	deny := api.Rule{Domain: "*", Action: api.RuleDeny}
	for _, c := range []struct {
		body map[string]any
		want []api.Rule
	}{
		{map[string]any{"allow_internet_access": false}, []api.Rule{deny}},
		{map[string]any{"allow_internet_access": true}, nil},
		{map[string]any{"network": map[string]any{"denyOut": []string{allTraffic}, "allowOut": []string{"API.github.com", "*.pypi.org"}}},
			[]api.Rule{deny, {Domain: "api.github.com", Action: api.RuleAllow}, {Domain: "*.pypi.org", Action: api.RuleAllow}}},
		{map[string]any{"allow_internet_access": false, "network": map[string]any{"denyOut": []string{"evil.example.com"}, "allowOut": []string{"good.example.com"}}},
			[]api.Rule{deny, {Domain: "evil.example.com", Action: api.RuleDeny}, {Domain: "good.example.com", Action: api.RuleAllow}}},
		// allowOut alone limits nothing; allowOut of everything wins over a deny.
		{map[string]any{"network": map[string]any{"allowOut": []string{"github.com"}}}, nil},
		{map[string]any{"network": map[string]any{"denyOut": []string{allTraffic}, "allowOut": []string{allTraffic}}}, nil},
	} {
		id := fx.create(c.body)
		got := fx.sandbox(id).Network.Egress
		if len(got) == 0 {
			got = nil
		}
		if !rulesEqual(got, c.want) {
			t.Errorf("%v: rules %+v, want %+v", c.body, got, c.want)
		}
		// Enforced by its node: the policy went with the create.
		if pol, ok := fx.node.Policy(id); !ok || len(pol.Egress) != len(c.want) {
			t.Errorf("%v: the node's policy %+v", c.body, pol.Egress)
		}
	}
	for body, msg := range map[string]string{
		`{"network": {"denyOut": ["10.0.0.0/8"]}}`:                                      "not addresses",
		`{"network": {"denyOut": ["0.0.0.0/0"], "allowOut": ["1.1.1.1"]}}`:              "not addresses",
		`{"network": {"denyOut": ["evil.example.com"]}}`:                                "denies all other traffic",
		`{"network": {"denyOut": ["0.0.0.0/0", "api.x.com"], "allowOut": ["*.x.com"]}}`: "covers",
		`{"allow_internet_access": false, "network": {"allowOut": ["not a name"]}}`:     "network.egress",
	} {
		w := fx.do("POST", "/v2/sandboxes", fx.key, []byte(body))
		if w.Code != 400 || !strings.Contains(w.String(), msg) {
			t.Errorf("%s: %d %s, want 400 about %s", body, w.Code, w.Body, msg)
		}
	}

	// PUT .../network changes it on a running sandbox, at once.
	id := fx.create(nil)
	if w := fx.do("PUT", "/sandboxes/"+id+"/network", fx.key, map[string]any{"allow_internet_access": false, "allowOut": []string{"github.com"}}); w.Code != 204 {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	if pol, _ := fx.node.Policy(id); !rulesEqual(pol.Egress, []api.Rule{deny, {Domain: "github.com", Action: api.RuleAllow}}) {
		t.Fatalf("the node's policy after the update %+v", pol.Egress)
	}
	if w := fx.do("PUT", "/sandboxes/"+id+"/network", fx.key, map[string]any{}); w.Code != 204 || len(fx.sandbox(id).Network.Egress) != 0 {
		t.Fatalf("update to open: %d %s", w.Code, w.Body)
	}
	wantErr(t, fx.do("PUT", "/sandboxes/"+id+"/network", fx.key, map[string]any{"rules": map[string]any{"x": 1}}), 400, "rules")
}

func rulesEqual(a, b []api.Rule) bool {
	return slices.EqualFunc(a, b, func(x, y api.Rule) bool {
		return x.Domain == y.Domain && x.Action == y.Action && x.Include == y.Include && slices.Equal(x.Ports, y.Ports)
	})
}

// envd's access token is kept sealed under loam-api's secret key, bound to its
// sandbox: never in the clear in the record, and a ciphertext tampered with
// or moved to another sandbox's row doesn't open, so its sandbox is refused
// rather than reached with a token that isn't its own.
func TestTokenSealed(t *testing.T) {
	fx := newFixture(t)
	a, b := fx.create(nil), fx.create(nil)
	tokA, tokB := fx.token(a), fx.token(b)
	rowA := fx.row(a)
	if len(tokA) != 64 || bytes.Contains(rowA.SealedToken, []byte(tokA)) || tokA == tokB {
		t.Fatalf("token %q, sealed %x", tokA, rowA.SealedToken)
	}
	// The round trip: the SDK's token from create, get and connect is what
	// opens, and what envd took.
	if got := decode[map[string]any](t, fx.do("GET", "/sandboxes/"+a, fx.key, nil))["envdAccessToken"]; got != tokA {
		t.Fatalf("get's token %v", got)
	}
	if got := decode[map[string]any](t, fx.do("POST", "/v2/sandboxes/"+a+"/connect", fx.key, nil))["envdAccessToken"]; got != tokA {
		t.Fatalf("connect's token %v", got)
	}
	if e, _ := fx.envd(a); e.Inits()[0].AccessToken != tokA {
		t.Fatalf("envd took %q", e.Inits()[0].AccessToken)
	}
	if w := fx.do("GET", "/x", "", nil, "E2b-Sandbox-Id", a, "X-Access-Token", tokA); w.Code != 200 {
		t.Fatalf("with the token: %d %s", w.Code, w.Body)
	}
	// Sealed under another sandbox's ID, it doesn't open.
	if _, err := fx.srv.Open(b, rowA.SealedToken); err == nil {
		t.Fatal("a's sealed token opened as b's")
	}

	set := func(id string, sealed []byte) {
		t.Helper()
		if _, err := fx.store.UpdateE2B(context.Background(), id, func(e *db.E2BSandbox) error {
			e.SealedToken = sealed
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Tampered with: a flipped bit anywhere.
	for _, i := range []int{0, len(rowA.SealedToken) / 2, len(rowA.SealedToken) - 1} {
		bad := append([]byte{}, rowA.SealedToken...)
		bad[i] ^= 1
		set(a, bad)
		if w := fx.do("GET", "/x", "", nil, "E2b-Sandbox-Id", a, "X-Access-Token", tokA); w.Code != 502 {
			t.Fatalf("tampered at %d: %d %s", i, w.Code, w.Body)
		}
		if w := fx.do("GET", "/sandboxes/"+a, fx.key, nil); w.Code != 500 || strings.Contains(w.String(), tokA) {
			t.Fatalf("get with a tampered token: %d %s", w.Code, w.Body)
		}
	}
	// Moved: b's row holding a's sealed token reaches nothing with a's token.
	set(b, rowA.SealedToken)
	if w := fx.do("GET", "/x", "", nil, "E2b-Sandbox-Id", b, "X-Access-Token", tokA); w.Code != 502 {
		t.Fatalf("a's token moved to b: %d %s", w.Code, w.Body)
	}
	// Put back, it works again.
	set(a, rowA.SealedToken)
	if w := fx.do("GET", "/x", "", nil, "E2b-Sandbox-Id", a, "X-Access-Token", tokA); w.Code != 200 {
		t.Fatalf("restored: %d %s", w.Code, w.Body)
	}
}

// Without a secret key a token can't be kept sealed, so E2B creates are
// refused, and nothing is created.
func TestNoSecretKey(t *testing.T) {
	fx := newFixtureWith(t, 8192, func(o *apiserver.Options) { o.SecretKey = nil })
	wantErr(t, fx.do("POST", "/v2/sandboxes", fx.key, nil), 501, "no secret key")
	var all api.SandboxList
	fx.loam("GET", "/v1/sandboxes", fx.key, nil, &all)
	if len(all.Sandboxes) != 0 {
		t.Fatalf("created %+v", all.Sandboxes)
	}
	if fx.srv.CanSeal() {
		t.Fatal("CanSeal without a key")
	}
	if _, err := fx.srv.Seal("sb-x", []byte("t")); err == nil {
		t.Fatal("sealed without a key")
	}
}

func TestPrivateTraffic(t *testing.T) {
	fx := newFixture(t)
	w := fx.do("POST", "/v2/sandboxes", fx.key, map[string]any{"templateID": "base", "network": map[string]any{"allowPublicTraffic": false}, "autoPause": true, "autoResume": map[string]any{"enabled": true}})
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	created := decode[sandboxJSON](t, w)
	id, token := created.SandboxID, created.TrafficAccessToken
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(token) || !fx.row(id).PrivateTraffic {
		t.Fatalf("private create: %+v", created)
	}
	host := "8080-" + id + "." + domain
	fx.node.Listen(id, 8080, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Header.Get("e2b-traffic-access-token"))
	}))
	for _, tc := range []struct{ token, message string }{{"", "is missing"}, {"wrong", "is invalid"}} {
		w := fx.do("GET", "/", "", nil, "Host", host, "e2b-traffic-access-token", tc.token)
		if w.Code != 403 || !strings.Contains(w.String(), tc.message) || w.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("denied: %+v", w)
		}
	}
	w = fx.do("GET", "/", "", nil, "Host", host, "e2b-traffic-access-token", token)
	if w.Code != 200 || w.String() != token {
		t.Fatalf("authorized/forwarded: %+v", w)
	}
	w = fx.do("OPTIONS", "/", "", nil, "Host", host, "Access-Control-Request-Headers", "e2b-traffic-access-token")
	if w.Code != 403 {
		t.Fatalf("preflight: %+v", w)
	}
	for _, tc := range []struct {
		mode, accept string
		html         bool
	}{{"navigate", "text/html", true}, {"cors", "text/html", false}, {"navigate", "application/json", false}, {"websocket", "*/*", false}} {
		w := fx.do("GET", "/", "", nil, "Host", host, "User-Agent", "Mozilla/5.0", "Accept", tc.accept, "Sec-Fetch-Mode", tc.mode)
		if strings.HasPrefix(w.Header.Get("Content-Type"), "text/html") != tc.html {
			t.Fatalf("negotiation: %+v", w)
		}
	}
	w = fx.do("GET", "/x", "", nil, "Host", "49983-"+id+"."+domain, "X-Access-Token", created.EnvdAccessToken)
	if w.Code != 200 {
		t.Fatalf("envd exemption: %+v", w)
	}
	for _, path := range []string{"/sandboxes/" + id, "/v2/sandboxes"} {
		w := fx.do("GET", path, fx.key, nil)
		if w.Code != 200 || strings.Contains(w.String(), "trafficAccessToken") {
			t.Fatalf("detail/list leaked: %+v", w)
		}
	}
	for _, tc := range []struct {
		body   string
		status int
	}{{`{"allowPublicTraffic":false}`, 204}, {`{"allowPublicTraffic":true}`, 400}, {`{}`, 204}} {
		w := fx.do("PUT", "/sandboxes/"+id+"/network", fx.key, []byte(tc.body))
		if w.Code != tc.status {
			t.Fatalf("update %s: %+v", tc.body, w)
		}
	}
	w = fx.do("POST", "/sandboxes/"+id+"/pause", fx.key, nil)
	if w.Code != 204 {
		t.Fatalf("pause: %+v", w)
	}
	w = fx.do("GET", "/", "", nil, "Host", host)
	if w.Code != 403 || fx.sandbox(id).State != api.StatePaused {
		t.Fatalf("unauthorized wake: %+v", w)
	}
	w = fx.do("POST", "/sandboxes/"+id+"/connect", fx.key, map[string]any{})
	if w.Code >= 300 || decode[sandboxJSON](t, w).TrafficAccessToken != token {
		t.Fatalf("connect token: %+v", w)
	}
}
