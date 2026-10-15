package apiserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

func isClose(err error, code int) bool {
	var ce *websocket.CloseError
	return errors.As(err, &ce) && ce.Code == code
}

// Exec over WebSocket end to end: client → loam-api → nodelet agent stream →
// fake agent, frames unchanged both ways, and the agent's normal close passed on.
func TestExecWebSocket(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	sb := h.create(key, api.CreateSandbox{Name: "ws"})
	h.must(200, "POST", "/v1/sandboxes/ws/pause", key, nil, nil)

	// No key, no socket.
	if _, resp, err := h.dialWS("/v1/sandboxes/ws/exec?cmd=cat", ""); err == nil || resp.StatusCode != 401 {
		t.Fatalf("keyless exec: %v", err)
	}
	h.callsSince()
	ws, _, err := h.dialWS("/v1/sandboxes/ws/exec?cmd=cat", key)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	wantCalls(t, h.callsSince(), "resume "+sb.ID, "agent "+sb.ID+" GET /exec") // the socket woke it
	var started api.ExecMessage
	if err := ws.ReadJSON(&started); err != nil || started.Type != "started" || started.Session == "" {
		t.Fatalf("started: %+v %v", started, err)
	}
	ws.WriteMessage(websocket.BinaryMessage, append([]byte{api.StreamStdin}, "hello"...))
	kind, msg, err := ws.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage || string(msg) != "\x01hello" {
		t.Fatalf("stdout: %d %q %v", kind, msg, err)
	}
	ws.WriteMessage(websocket.BinaryMessage, []byte{api.StreamStdinEOF})
	var exit api.ExecMessage
	if err := ws.ReadJSON(&exit); err != nil || exit.Type != "exit" || exit.Code == nil || *exit.Code != 0 {
		t.Fatalf("exit: %+v %v", exit, err)
	}
	if _, _, err := ws.ReadMessage(); !isClose(err, websocket.CloseNormalClosure) {
		t.Fatalf("after exit: %v, want a normal close", err)
	}

	// A refused socket is the agent's own answer.
	_, resp, err := h.dialWS("/v1/sandboxes/ws/exec", key)
	if err == nil || resp.StatusCode != 400 {
		t.Fatalf("exec without cmd: %v", err)
	}

	// One-shot output and exit codes pass through.
	if out := h.exec(key, "ws", "exit", "3"); out.ExitCode != 3 {
		t.Fatalf("exit code: %+v", out)
	}
	if out := h.exec(key, "ws", "hostname"); string(out.Stdout) != "ws\n" {
		t.Fatalf("hostname: %q", out.Stdout)
	}
}

// A session ends with 1011, and no exit frame, when the sandbox goes away
// under it: a memory checkpoint taken, restored, deleted. A disk-only
// checkpoint leaves it be.
func TestSessionsEndWith1011(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	h.create(key, api.CreateSandbox{Name: "s"})

	open := func() *websocket.Conn {
		ws, _, err := h.dialWS("/v1/sandboxes/s/exec?cmd=sleep", key)
		if err != nil {
			t.Fatal(err)
		}
		ws.ReadMessage() // started
		return ws
	}
	ws := open()
	h.must(201, "POST", "/v1/sandboxes/s/checkpoints", key, map[string]any{"memory": false}, nil)
	ws.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, _, err := ws.ReadMessage(); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("after a disk-only checkpoint: %v, want the session still open", err)
	}
	ws.Close()
	ws = open()
	h.must(201, "POST", "/v1/sandboxes/s/checkpoints", key, map[string]any{}, nil)
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := ws.ReadMessage(); !isClose(err, websocket.CloseInternalServerErr) {
		t.Fatalf("after a memory checkpoint: %v, want 1011", err)
	}
	ws = open()
	h.must(200, "POST", "/v1/sandboxes/s/checkpoints/v1/restore", key, nil, nil)
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := ws.ReadMessage(); !isClose(err, websocket.CloseInternalServerErr) {
		t.Fatalf("after restore: %v, want 1011", err)
	}
	ws = open()
	h.must(204, "DELETE", "/v1/sandboxes/s", key, nil, nil)
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := ws.ReadMessage(); !isClose(err, websocket.CloseInternalServerErr) {
		t.Fatalf("after delete: %v, want 1011", err)
	}
}

func TestPortURLs(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	other := h.project("eve")
	sb := h.create(key, api.CreateSandbox{Name: "web"})
	var seen http.Header
	h.node.Listen(sb.ID, 8080, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		io.WriteString(w, "app at "+r.Host+r.URL.Path)
	}))
	host := "web--jake." + testDomain

	// Private by default: no key, another project's key, or a bad token is 401.
	for _, tok := range []string{"", other} {
		if status, _, _ := h.callHost(host, "GET", "/", tok, nil); status != 401 {
			t.Fatalf("private port with %q: %d", tok, status)
		}
	}
	status, body, _ := h.callHost(host, "GET", "/hello", key, nil)
	if status != 200 || string(body) != "app at "+host+"/hello" {
		t.Fatalf("with the key: %d %q", status, body)
	}
	if seen.Get("Authorization") != "" {
		t.Fatal("the project key reached the app")
	}

	// ?loam_token= sets the cookie and redirects without the token; the cookie then works.
	status, _, hdr := h.callHost(host, "GET", "/page?a=1&loam_token="+url.QueryEscape(key), "", nil)
	if status != 302 || hdr.Get("Location") != "/page?a=1" || !strings.Contains(hdr.Get("Set-Cookie"), "loam_token="+key) {
		t.Fatalf("token visit: %d %v", status, hdr)
	}
	if status, _, _ := h.callHost(host, "GET", "/?loam_token="+url.QueryEscape(other), "", nil); status != 401 {
		t.Fatalf("another project's token: %d", status)
	}
	req, _ := http.NewRequest("GET", h.ts.URL+"/", nil)
	req.Host = host
	req.AddCookie(&http.Cookie{Name: "loam_token", Value: key})
	req.AddCookie(&http.Cookie{Name: "app", Value: "x"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("with the cookie: %v %v", resp, err)
	}
	resp.Body.Close()
	if c := seen.Get("Cookie"); c != "app=x" {
		t.Fatalf("cookies at the app: %q", c)
	}

	// Public: anyone, and an Authorization header is the app's.
	h.must(200, "PUT", "/v1/sandboxes/web/ports/8080", key, map[string]string{"access": "public"}, nil)
	h.must(200, "PUT", "/v1/sandboxes/web/ports/3000", key, map[string]string{"access": "private"}, nil)
	var settings api.PortSettingList
	h.must(200, "GET", "/v1/sandboxes/web/ports/settings", key, nil, &settings)
	if len(settings.Ports) != 2 || settings.Ports[1].Port != 8080 || settings.Ports[1].URL != "https://8080--web--jake."+testDomain {
		t.Fatalf("settings: %+v", settings)
	}
	h.errCode(400, api.CodeInvalid, "PUT", "/v1/sandboxes/web/ports/8080", key, map[string]string{"access": "open"})
	if status, _, _ := h.callHost(host, "GET", "/", "", nil); status != 200 {
		t.Fatalf("public: %d", status)
	}
	h.callHost(host, "GET", "/", "app-token", nil)
	if seen.Get("Authorization") != "Bearer app-token" {
		t.Fatalf("the app's own Authorization was dropped: %v", seen)
	}

	// <port>--<name>--<project> reaches any port; nothing listening is 502
	// port_closed after the wait; a sleeping sandbox is woken by the request.
	h.node.Listen(sb.ID, 3000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "three") }))
	if status, body, _ := h.callHost("3000--web--jake."+testDomain, "GET", "/", key, nil); status != 200 || string(body) != "three" {
		t.Fatalf("port 3000: %d %q", status, body)
	}
	h.must(200, "PUT", "/v1/sandboxes/web/ports/4000", key, map[string]string{"access": "public"}, nil)
	start := time.Now()
	status, body, _ = h.callHost("4000--web--jake."+testDomain, "GET", "/", "", nil)
	var e api.Error
	json.Unmarshal(body, &e)
	if status != 502 || e.Error.Code != api.CodePortClosed || time.Since(start) < 250*time.Millisecond {
		t.Fatalf("closed port: %d %s after %v", status, body, time.Since(start))
	}
	h.must(200, "POST", "/v1/sandboxes/web/pause", key, nil, nil)
	h.callsSince()
	if status, _, _ := h.callHost(host, "GET", "/", "", nil); status != 200 {
		t.Fatalf("waking request: %d", status)
	}
	if c := h.callsSince(); len(c) == 0 || c[0] != "resume "+sb.ID {
		t.Fatalf("the request didn't wake it: %v", c)
	}
	// Unknown sandboxes and malformed hosts.
	if status, _, _ := h.callHost("nope--jake."+testDomain, "GET", "/", key, nil); status != 404 {
		t.Fatalf("unknown sandbox: %d", status)
	}
	if status, _, _ := h.callHost("api."+testDomain, "GET", "/healthz", "", nil); status != 200 {
		t.Fatalf("the API host isn't a port URL: %d", status)
	}
}

// A WebSocket to a port URL is proxied as an upgrade end to end.
func TestPortURLWebSocket(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	sb := h.create(key, api.CreateSandbox{Name: "app", HTTPPort: 9000})
	up := websocket.Upgrader{}
	h.node.Listen(sb.ID, 9000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			kind, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			c.WriteMessage(kind, append([]byte("echo "), msg...))
		}
	}))
	d := websocket.Dialer{}
	hdr := http.Header{"Authorization": {"Bearer " + key}, "Host": {"app--jake." + testDomain}}
	u := "ws" + strings.TrimPrefix(h.ts.URL, "http") + "/socket"
	ws, resp, err := d.Dial(u, hdr)
	if err != nil {
		t.Fatalf("dial: %v %v", err, resp)
	}
	defer ws.Close()
	ws.WriteMessage(websocket.TextMessage, []byte("hi"))
	if _, msg, err := ws.ReadMessage(); err != nil || string(msg) != "echo hi" {
		t.Fatalf("echo: %q %v", msg, err)
	}
}

func TestEventsStream(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	other := h.project("other")
	req, _ := http.NewRequest("GET", h.ts.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
	h.create(other, api.CreateSandbox{Name: "theirs"}) // not ours to see
	sb := h.create(key, api.CreateSandbox{Name: "ev"})
	h.must(200, "POST", "/v1/sandboxes/ev/pause", key, nil, nil)
	h.must(201, "POST", "/v1/sandboxes/ev/checkpoints", key, map[string]any{}, nil)

	sc := bufio.NewScanner(resp.Body)
	var got []string
	for len(got) < 5 && sc.Scan() {
		line := sc.Text()
		if d, ok := strings.CutPrefix(line, "data: "); ok {
			var e api.Event
			json.Unmarshal([]byte(d), &e)
			if e.Sandbox != sb.ID {
				t.Fatalf("event for another sandbox: %s", d)
			}
			got = append(got, e.Type)
		}
	}
	want := "sandbox.created sandbox.running sandbox.paused sandbox.running checkpoint.created"
	if strings.Join(got, " ") != want {
		t.Fatalf("events: %v, want %s", got, want)
	}
}

// Reconciliation: a VM that crashes is reported by the nodelet's event
// stream; one that vanished is found on the list at (re)connect.
func TestReconcile(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	a := h.create(key, api.CreateSandbox{Name: "a"})
	b := h.create(key, api.CreateSandbox{Name: "b"})
	h.node.Forget(b.ID) // lost while nobody watched

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.srv.watchNodes(ctx)
	eventually(t, "loam-api to follow the node", func() bool { return h.node.Subscribers() > 0 })
	eventually(t, "b to be reconciled", func() bool { return h.get(key, "b").State == api.StateError })

	h.node.Crash(a.ID, "firecracker exited")
	eventually(t, "a to be reconciled", func() bool { return h.get(key, "a").State == api.StateError })
	if got := h.get(key, "a"); !strings.Contains(got.Error, "firecracker exited") {
		t.Fatalf("a: %+v", got)
	}
	// A failed sandbox can be stopped and woken again.
	h.must(200, "POST", "/v1/sandboxes/a/stop", key, nil, nil)
	if out := h.exec(key, "a", "echo", "back"); string(out.Stdout) != "back\n" {
		t.Fatalf("after recovery: %+v", out)
	}
}

// The guest's host channel: the node token and the sandbox the nodelet names.
func TestGuestChannel(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	sb := h.create(key, api.CreateSandbox{Name: "inside"})
	guest := func(method, path, token, sandbox string, body any) (int, []byte) {
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, h.ts.URL+path, strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(nodeapi.HeaderSandbox, sandbox)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	if status, _ := guest("GET", "/internal/guest/self", key, sb.ID, nil); status != 401 {
		t.Fatalf("a project key on the host channel: %d", status)
	}
	if status, _ := guest("GET", "/internal/guest/self", nodeToken, "sb-aaaaaaaaaaaa", nil); status != 404 {
		t.Fatalf("unknown sandbox: %d", status)
	}
	status, body := guest("GET", "/internal/guest/self", nodeToken, sb.ID, nil)
	var self api.Sandbox
	json.Unmarshal(body, &self)
	if status != 200 || self.ID != sb.ID || self.Name != "inside" {
		t.Fatalf("self: %d %s", status, body)
	}

	// A memory checkpoint is answered first, then taken.
	status, body = guest("POST", "/internal/guest/checkpoints", nodeToken, sb.ID, map[string]any{"comment": "from inside"})
	var cp api.Checkpoint
	json.Unmarshal(body, &cp)
	if status != 201 || cp.ID != "v1" || !cp.Memory {
		t.Fatalf("guest checkpoint: %d %s", status, body)
	}
	eventually(t, "the checkpoint to be taken", func() bool {
		_, err := h.store.Checkpoint(context.Background(), sb.ID, "v1")
		return err == nil && h.node.HasSnapshot(sb.ID+".v1")
	})
	// A disk-only one is taken before the answer.
	status, body = guest("POST", "/internal/guest/checkpoints", nodeToken, sb.ID, map[string]any{"memory": false})
	if status != 201 || !h.node.HasSnapshot(sb.ID+".v2") {
		t.Fatalf("disk checkpoint: %d %s", status, body)
	}
}

// Ports watching wakes the sandbox but is not activity, and a pause ends it.
func TestPortsWatchIsNotActivity(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	sb := h.create(key, api.CreateSandbox{Name: "pw"})
	// The fake agent has no /ports/watch: the forwarded call answers its 404,
	// and the point here is only the hold.
	h.call("GET", "/v1/sandboxes/pw/ports/watch", key, nil)
	if _, ok := h.srv.rt(sb.ID).idleFor(h.clock.Now()); !ok {
		t.Fatal("ports/watch left a hold")
	}
	var ports api.PortList
	h.node.Listen(sb.ID, 5000, http.NotFoundHandler())
	h.must(200, "GET", "/v1/sandboxes/pw/ports", key, nil, &ports)
	if len(ports.Ports) != 1 || ports.Ports[0].Port != 5000 {
		t.Fatalf("ports: %+v", ports)
	}
}

func TestNodeDiscoveryByService(t *testing.T) {
	h := newHarness(t)
	addr := h.nodes.Static["geek"]
	host, port, _ := strings.Cut(addr, ":")
	var lookups int
	n := &Nodes{Token: nodeToken, Service: "loam-nodelet.loam.svc.cluster.local",
		Resolve: func(_ context.Context, name string) ([]string, error) {
			lookups++
			if name != "loam-nodelet.loam.svc.cluster.local" {
				t.Errorf("resolved %q", name)
			}
			return []string{host}, nil
		}}
	n.Port, _ = strconv.Atoi(port)
	nodes, err := n.List(context.Background())
	if err != nil || len(nodes) != 1 || nodes[0].Name != "geek" || nodes[0].MemoryMiB != 8192 {
		t.Fatalf("list: %+v %v", nodes, err)
	}
	cl, err := n.Client(context.Background(), "geek")
	if err != nil || cl.Addr != addr {
		t.Fatalf("client: %+v %v", cl, err)
	}
	if _, err := n.Client(context.Background(), "basil"); !errors.Is(err, ErrNoNode) {
		t.Fatalf("unknown node: %v", err)
	}
	if lookups < 2 {
		t.Fatalf("the Service was resolved %d times; a miss must resolve again", lookups)
	}

	if got := place([]NodeInfo{
		{Node: nodeapi.Node{Name: "a", MemoryMiB: 8000, MemoryMiBAllocated: 6000, Ready: true}},
		{Node: nodeapi.Node{Name: "b", MemoryMiB: 8000, MemoryMiBAllocated: 1000, Ready: false}},
		{Node: nodeapi.Node{Name: "c", MemoryMiB: 8000, MemoryMiBAllocated: 3000, Ready: true}},
	}, 2048); got != "c" {
		t.Fatalf("placed on %q", got)
	}
}

func TestLoadTemplates(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "t.yaml")
		os.WriteFile(p, []byte(s), 0o644)
		return p
	}
	ts, err := LoadTemplates(write(`
templates:
  - name: base
    image: ghcr.io/arugula-salad/loam-base@sha256:abc
    resources: {cpus: 2, memory_mib: 2048, disk_gib: 20}
`))
	if err != nil || ts["base"].Resources.MemoryMiB != 2048 {
		t.Fatalf("%+v %v", ts, err)
	}
	for _, bad := range []string{
		"templates: []",
		"templates:\n  - name: base\n    resources: {cpus: 2, memory_mib: 2048, disk_gib: 20}",
		"templates:\n  - name: base\n    image: x\n    resources: {cpus: 0, memory_mib: 2048, disk_gib: 20}",
		"templates:\n  - name: base\n    image: x\n    bogus: 1\n    resources: {cpus: 2, memory_mib: 2048, disk_gib: 20}",
	} {
		if _, err := LoadTemplates(write(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// A service's log, followed, wakes the sandbox but is not activity, and with
// wake=false a sandbox that isn't running is left so: what a log shipper needs
// to read an idle app without keeping it awake or waking it.
func TestServiceLogsForShippers(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	sb := h.create(key, api.CreateSandbox{Name: "lg"})
	// The fake agent may answer the log itself with a 404; the point is the hold.
	h.call("GET", "/v1/sandboxes/lg/services/app/logs?follow=true", key, nil)
	if _, ok := h.srv.rt(sb.ID).idleFor(h.clock.Now()); !ok {
		t.Fatal("following a log left a hold")
	}
	// A plain read is activity like any other call.
	h.call("GET", "/v1/sandboxes/lg/services/app/logs", key, nil)
	h.must(200, "POST", "/v1/sandboxes/lg/pause", key, nil, nil)
	h.errCode(409, api.CodeConflict, "GET", "/v1/sandboxes/lg/services/app/logs?follow=true&wake=false", key, nil)
	var got api.Sandbox
	h.must(200, "GET", "/v1/sandboxes/lg", key, nil, &got)
	if got.State != api.StatePaused {
		t.Fatalf("wake=false woke it: %s", got.State)
	}
	// Elsewhere wake=false means nothing: only a log may decline to wake.
	h.call("GET", "/v1/sandboxes/lg/ports?wake=false", key, nil)
	h.must(200, "GET", "/v1/sandboxes/lg", key, nil, &got)
	if got.State != api.StateRunning {
		t.Fatalf("a ports read with wake=false should still wake: %s", got.State)
	}
}
