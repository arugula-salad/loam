package apiserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

func TestProjectsAndKeys(t *testing.T) {
	h := newHarness(t)
	h.errCode(401, api.CodeUnauthorized, "POST", "/v1/projects", "", map[string]string{"name": "jake"})
	h.errCode(400, api.CodeInvalid, "POST", "/v1/projects", adminToken, map[string]string{"name": "a--b"})
	h.errCode(400, api.CodeInvalid, "POST", "/v1/projects", adminToken, map[string]string{"name": "Jake"})
	key := h.project("jake")
	h.errCode(409, api.CodeAlreadyExists, "POST", "/v1/projects", adminToken, map[string]string{"name": "jake"})
	if !keyRE.MatchString(key) {
		t.Fatalf("key %q is not loam_<32 base32>", key)
	}

	// A project key can't manage projects, and the admin token is no project.
	h.errCode(401, api.CodeUnauthorized, "GET", "/v1/projects", key, nil)
	h.errCode(401, api.CodeUnauthorized, "GET", "/v1/sandboxes", adminToken, nil)
	h.errCode(401, api.CodeUnauthorized, "GET", "/v1/sandboxes", "loam_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil)

	var who api.Whoami
	h.must(200, "GET", "/v1/whoami", key, nil, &who)
	if who.Project.Name != "jake" || who.Key.Name != "test" || who.Key.Secret != "" {
		t.Fatalf("whoami: %+v", who)
	}
	var keys struct{ Keys []api.Key }
	h.must(200, "GET", "/v1/projects/jake/keys", adminToken, nil, &keys)
	if len(keys.Keys) != 1 || keys.Keys[0].Secret != "" {
		t.Fatalf("keys: %+v", keys)
	}
	h.errCode(404, api.CodeNotFound, "GET", "/v1/projects/nope/keys", adminToken, nil)

	// Only the hash is stored.
	stored, _ := h.store.Keys(t.Context(), "jake")
	if len(stored) != 1 || strings.Contains(string(stored[0].Hash), key) || len(stored[0].Hash) != 32 {
		t.Fatalf("stored key: %+v", stored)
	}

	sb := h.create(key, api.CreateSandbox{Name: "one"})
	h.errCode(409, api.CodeProjectNotEmpty, "DELETE", "/v1/projects/jake", adminToken, nil)
	h.must(204, "DELETE", "/v1/sandboxes/"+sb.ID, key, nil, nil)

	h.must(204, "DELETE", "/v1/projects/jake/keys/"+who.Key.ID, adminToken, nil, nil)
	h.errCode(401, api.CodeUnauthorized, "GET", "/v1/whoami", key, nil)
	h.must(204, "DELETE", "/v1/projects/jake", adminToken, nil, nil)
	h.errCode(404, api.CodeNotFound, "DELETE", "/v1/projects/jake", adminToken, nil)
}

// A key never sees another project's sandboxes: they are 404, by ID and by
// name, on every kind of route, and nothing reaches the node for them.
func TestProjectIsolation(t *testing.T) {
	h := newHarness(t)
	a, b := h.project("alice"), h.project("bob")
	sb := h.create(a, api.CreateSandbox{Name: "mine"})
	h.callsSince()

	for _, ref := range []string{sb.ID, "mine"} {
		p := "/v1/sandboxes/" + ref
		h.errCode(404, api.CodeNotFound, "GET", p, b, nil)
		h.errCode(404, api.CodeNotFound, "PATCH", p, b, map[string]any{"protected": true})
		h.errCode(404, api.CodeNotFound, "DELETE", p, b, nil)
		h.errCode(404, api.CodeNotFound, "POST", p+"/pause", b, nil)
		h.errCode(404, api.CodeNotFound, "POST", p+"/exec/run", b, api.ExecRun{Cmd: []string{"echo"}})
		h.errCode(404, api.CodeNotFound, "GET", p+"/fs/file?path=/etc/hosts", b, nil)
		h.errCode(404, api.CodeNotFound, "POST", p+"/checkpoints", b, map[string]any{})
		h.errCode(404, api.CodeNotFound, "GET", p+"/checkpoints", b, nil)
		h.errCode(404, api.CodeNotFound, "PUT", p+"/ports/80", b, map[string]string{"access": "public"})
		h.errCode(404, api.CodeNotFound, "GET", "/v1/events?sandbox="+ref, b, nil)
		if _, resp, err := h.dialWS(p+"/exec?cmd=cat", b); err == nil || resp.StatusCode != 404 {
			t.Fatalf("exec socket into another project's sandbox: %v", err)
		}
	}
	// A fork from another project's checkpoint is not found either.
	var cp api.Checkpoint
	h.must(201, "POST", "/v1/sandboxes/mine/checkpoints", a, map[string]any{}, &cp)
	h.callsSince()
	h.errCode(404, api.CodeNotFound, "POST", "/v1/sandboxes", b, api.CreateSandbox{From: &api.From{Sandbox: sb.ID, Checkpoint: cp.ID}})

	var list api.SandboxList
	h.must(200, "GET", "/v1/sandboxes", b, nil, &list)
	if len(list.Sandboxes) != 0 {
		t.Fatalf("bob sees %+v", list.Sandboxes)
	}
	wantCalls(t, h.callsSince())
	// Bob may have a sandbox of the same name.
	h.create(b, api.CreateSandbox{Name: "mine"})
	if got := h.get(a, "mine"); got.ID != sb.ID {
		t.Fatalf("alice's mine is now %s", got.ID)
	}
}

func TestCreateAndTransitions(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	sb := h.create(key, api.CreateSandbox{Name: "scratch", Env: map[string]string{"FOO": "bar"}, Labels: map[string]string{"owner": "me"}})
	if sb.State != api.StateRunning || sb.Template != "base" || sb.Resources.MemoryMiB != 2048 ||
		sb.Idle != (api.Idle{TimeoutS: 300, Action: api.ActionPause}) || sb.HTTPPort != 8080 ||
		sb.URL != "https://scratch--jake.loam.test" || !idRE.MatchString(sb.ID) {
		t.Fatalf("created: %+v", sb)
	}
	vm, wake, ok := h.node.VM(sb.ID)
	if !ok || vm.Template.Image != "ghcr.io/arugula-salad/loam-base@sha256:abc" || wake.Env["FOO"] != "bar" ||
		wake.Hostname != "scratch" || wake.Project != "jake" || wake.User != "loam" {
		t.Fatalf("VM %+v wake %+v", vm, wake)
	}
	wantCalls(t, h.callsSince(), "create "+sb.ID)

	h.errCode(409, api.CodeAlreadyExists, "POST", "/v1/sandboxes", key, api.CreateSandbox{Name: "scratch"})
	h.errCode(400, api.CodeInvalid, "POST", "/v1/sandboxes", key, api.CreateSandbox{Template: "nope"})
	h.errCode(400, api.CodeInvalid, "POST", "/v1/sandboxes", key, api.CreateSandbox{Idle: &api.Idle{TimeoutS: 5}})
	h.errCode(400, api.CodeInvalid, "POST", "/v1/sandboxes", key, api.CreateSandbox{Idle: &api.Idle{Action: "sleep"}})
	h.errCode(400, api.CodeInvalid, "POST", "/v1/sandboxes", key, api.CreateSandbox{Deadline: &api.Deadline{At: h.clock.Now().Add(-time.Hour), Action: "delete"}})
	h.errCode(400, api.CodeInvalid, "POST", "/v1/sandboxes", key, map[string]any{"name": "x", "bogus": true})
	h.errCode(400, api.CodeInvalid, "POST", "/v1/sandboxes", key, api.CreateSandbox{Name: "has--dashes"})

	var p api.Sandbox
	h.must(200, "POST", "/v1/sandboxes/scratch/pause", key, nil, &p)
	if p.State != api.StatePaused {
		t.Fatalf("pause: %s", p.State)
	}
	h.must(200, "POST", "/v1/sandboxes/scratch/pause", key, nil, &p) // again: nothing to do
	h.must(200, "POST", "/v1/sandboxes/scratch/wake", key, nil, &p)
	if p.State != api.StateRunning {
		t.Fatalf("wake: %s", p.State)
	}
	h.must(200, "POST", "/v1/sandboxes/scratch/stop", key, nil, &p)
	if p.State != api.StateStopped {
		t.Fatalf("stop: %s", p.State)
	}
	h.errCode(409, api.CodeConflict, "POST", "/v1/sandboxes/scratch/pause", key, nil)
	// Every pause and stop makes a snapshot of loam-api's naming.
	wantCalls(t, h.callsSince(), "pause "+sb.ID+" "+sb.ID+".@1", "resume "+sb.ID, "stop "+sb.ID+" "+sb.ID+".@2")

	// Protected: no delete until unprotected.
	h.must(200, "PATCH", "/v1/sandboxes/scratch", key, map[string]any{"protected": true}, &p)
	h.errCode(409, api.CodeConflict, "DELETE", "/v1/sandboxes/scratch", key, nil)
	h.must(200, "PATCH", "/v1/sandboxes/scratch", key, map[string]any{"protected": false}, &p)
	h.must(204, "DELETE", "/v1/sandboxes/scratch", key, nil, nil)
	h.errCode(404, api.CodeNotFound, "GET", "/v1/sandboxes/scratch", key, nil)
	if _, _, ok := h.node.VM(sb.ID); ok {
		t.Fatal("the VM outlived its sandbox")
	}

	// Generated names, list filters.
	g := h.create(key, api.CreateSandbox{Labels: map[string]string{"team": "a"}})
	if !validName(g.Name) || len(strings.Split(g.Name, "-")) != 3 {
		t.Fatalf("generated name %q", g.Name)
	}
	h.create(key, api.CreateSandbox{Name: "other", Labels: map[string]string{"team": "b"}})
	h.must(200, "POST", "/v1/sandboxes/other/pause", key, nil, nil)
	var list api.SandboxList
	h.must(200, "GET", "/v1/sandboxes?label=team=a", key, nil, &list)
	if len(list.Sandboxes) != 1 || list.Sandboxes[0].ID != g.ID {
		t.Fatalf("label filter: %+v", list.Sandboxes)
	}
	h.must(200, "GET", "/v1/sandboxes?state=paused", key, nil, &list)
	if len(list.Sandboxes) != 1 || list.Sandboxes[0].Name != "other" {
		t.Fatalf("state filter: %+v", list.Sandboxes)
	}
}

// An idle PATCH with one field keeps the other's current value.
func TestPatchIdleMerges(t *testing.T) {
	h := newHarness(t)
	key := h.project("p")
	var got api.Sandbox
	h.must(201, "POST", "/v1/sandboxes", key, map[string]any{"name": "i", "idle": map[string]any{"timeout_s": 600}}, &got)
	h.must(200, "PATCH", "/v1/sandboxes/i", key, map[string]any{"idle": map[string]any{"action": "stop"}}, &got)
	if got.Idle != (api.Idle{TimeoutS: 600, Action: "stop"}) {
		t.Errorf("idle after PATCH of action: %+v, want 600s stop", got.Idle)
	}
	h.errCode(400, api.CodeInvalid, "PATCH", "/v1/sandboxes/i", key, map[string]any{"idle": map[string]any{"timeout_s": 5}})
}

func TestPatch(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	sb := h.create(key, api.CreateSandbox{Name: "p", Env: map[string]string{"A": "1"}})
	h.callsSince()

	// New env reaches a running sandbox at once, through a resume of the running VM.
	var got api.Sandbox
	h.must(200, "PATCH", "/v1/sandboxes/p", key, map[string]any{"env": map[string]string{"B": "2"}}, &got)
	if got.Env["B"] != "2" || got.Env["A"] != "" {
		t.Fatalf("env: %+v", got.Env)
	}
	if _, wake, _ := h.node.VM(sb.ID); wake.Env["B"] != "2" {
		t.Fatalf("the agent wasn't handed the new env: %+v", wake.Env)
	}
	wantCalls(t, h.callsSince(), "resume "+sb.ID)
	if out := h.exec(key, "p", "env"); string(out.Stdout) != "B=2\n" {
		t.Fatalf("env inside: %q", out.Stdout)
	}

	at := h.clock.Now().Add(time.Hour).Truncate(time.Second)
	h.must(200, "PATCH", "/v1/sandboxes/p", key, map[string]any{"deadline": map[string]any{"at": at, "action": "stop"},
		"idle": map[string]any{"timeout_s": 60, "action": "stop"}, "http_port": 3000}, &got)
	if got.Deadline == nil || !got.Deadline.At.Equal(at) || got.Idle.TimeoutS != 60 || got.HTTPPort != 3000 {
		t.Fatalf("patch: %+v", got)
	}
	// Leaving deadline out keeps it; null clears it.
	h.must(200, "PATCH", "/v1/sandboxes/p", key, map[string]any{"labels": map[string]string{"x": "y"}}, &got)
	if got.Deadline == nil {
		t.Fatal("a PATCH without deadline cleared it")
	}
	status, raw, _ := h.call("PATCH", "/v1/sandboxes/p", key, []byte(`{"deadline": null}`))
	json.Unmarshal(raw, &got)
	if status != 200 || got.Deadline != nil {
		t.Fatalf("clearing the deadline: %d %s", status, raw)
	}
	h.errCode(400, api.CodeInvalid, "PATCH", "/v1/sandboxes/p", key, map[string]any{"idle": map[string]any{"timeout_s": 10}})
	h.errCode(400, api.CodeInvalid, "PATCH", "/v1/sandboxes/p", key, map[string]any{"http_port": 70000})
	h.errCode(400, api.CodeInvalid, "PATCH", "/v1/sandboxes/p", key, map[string]any{"name": "renamed"})
}

func TestCapacity(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	h.create(key, api.CreateSandbox{Resources: &api.Resources{MemoryMiB: 7000}})
	status, raw, hdr := h.call("POST", "/v1/sandboxes", key, api.CreateSandbox{Name: "nope"})
	if status != 503 || !strings.Contains(string(raw), api.CodeCapacity) || hdr.Get("Retry-After") == "" {
		t.Fatalf("over capacity: %d %s %v", status, raw, hdr)
	}
	// The node refusing at create (a race with another placement) is capacity
	// too, and leaves no record behind.
	h.node.Fail("create", &nodeapi.Err{Status: 503, ErrorDetail: api.ErrorDetail{Code: api.CodeCapacity, Message: "full"}})
	h.errCode(503, api.CodeCapacity, "POST", "/v1/sandboxes", key, api.CreateSandbox{Name: "race", Resources: &api.Resources{MemoryMiB: 512}})
	h.errCode(404, api.CodeNotFound, "GET", "/v1/sandboxes/race", key, nil)
	// A node that isn't ready takes nothing.
	h.node.SetReady(false)
	h.errCode(503, api.CodeCapacity, "POST", "/v1/sandboxes", key, api.CreateSandbox{Resources: &api.Resources{MemoryMiB: 512}})
	h.node.SetReady(true)
	// Any other failure leaves the sandbox in the error state, to be deleted.
	h.node.Fail("create", &nodeapi.Err{Status: 500, ErrorDetail: api.ErrorDetail{Code: "boot_failed", Message: "kernel panic"}})
	h.errCode(503, api.CodeSandboxUnavailable, "POST", "/v1/sandboxes", key, api.CreateSandbox{Name: "broken", Resources: &api.Resources{MemoryMiB: 512}})
	if sb := h.get(key, "broken"); sb.State != api.StateError || !strings.Contains(sb.Error, "kernel panic") {
		t.Fatalf("broken: %+v", sb)
	}
	h.must(204, "DELETE", "/v1/sandboxes/broken", key, nil, nil)
}

// Any sub-API call wakes a paused or stopped sandbox, and holds it awake.
func TestAcquireWakes(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	sb := h.create(key, api.CreateSandbox{Name: "w"})
	h.must(200, "POST", "/v1/sandboxes/w/pause", key, nil, nil)
	h.callsSince()
	if out := h.exec(key, "w", "echo", "hi"); string(out.Stdout) != "hi\n" {
		t.Fatalf("exec: %+v", out)
	}
	wantCalls(t, h.callsSince(), "resume "+sb.ID, "agent "+sb.ID+" POST /exec/run")
	if got := h.get(key, "w"); got.State != api.StateRunning {
		t.Fatalf("after exec: %s", got.State)
	}

	h.must(200, "POST", "/v1/sandboxes/w/stop", key, nil, nil)
	h.callsSince()
	h.must(200, "PUT", "/v1/sandboxes/w/fs/file?path=/tmp/x", key, []byte("data"), nil)
	wantCalls(t, h.callsSince(), "resume "+sb.ID, "agent "+sb.ID+" PUT /fs/file")
	if b, _ := h.node.File(sb.ID, "/tmp/x"); string(b) != "data" {
		t.Fatalf("file: %q", b)
	}
	status, raw, _ := h.call("GET", "/v1/sandboxes/w/fs/file?path=/tmp/x", key, nil)
	if status != 200 || string(raw) != "data" {
		t.Fatalf("read back: %d %q", status, raw)
	}
	// The agent's own errors pass through unchanged.
	h.errCode(404, api.CodeNotFound, "GET", "/v1/sandboxes/w/fs/file?path=/nope", key, nil)

	// A wake that fails is sandbox_unavailable, and the record stays as it was.
	h.must(200, "POST", "/v1/sandboxes/w/pause", key, nil, nil)
	h.node.Fail("resume", &nodeapi.Err{Status: 500, ErrorDetail: api.ErrorDetail{Code: "restore_failed", Message: "bad snapshot"}})
	h.errCode(503, api.CodeSandboxUnavailable, "POST", "/v1/sandboxes/w/exec/run", key, api.ExecRun{Cmd: []string{"echo"}})
	if got := h.get(key, "w"); got.State != api.StatePaused {
		t.Fatalf("after a failed wake: %s", got.State)
	}
}

// A VM its node paused while loam-api wasn't listening (a nodelet draining on
// SIGTERM) is resumed by the next call, not refused.
func TestCallResumesAVMPausedBehindItsBack(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	sb := h.create(key, api.CreateSandbox{Name: "d"})
	h.node.PauseQuietly(sb.ID)
	h.callsSince()
	if out := h.exec(key, "d", "echo", "back"); string(out.Stdout) != "back\n" {
		t.Fatalf("exec after a quiet pause: %+v", out)
	}
	// The refused dial and the VM lookup aren't in the fake's log; the resume is.
	wantCalls(t, h.callsSince(), "resume "+sb.ID, "agent "+sb.ID+" POST /exec/run")
	if got := h.get(key, "d"); got.State != api.StateRunning {
		t.Fatalf("record after the call: %s", got.State)
	}
	if vm, _, _ := h.node.VM(sb.ID); vm.State != nodeapi.VMRunning {
		t.Fatalf("VM after the call: %s", vm.State)
	}
}

// Nothing under /internal/ is ever forwarded, however it is spelled, and a
// path the agent doesn't serve doesn't wake the sandbox.
func TestNeverForwardInternal(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	h.create(key, api.CreateSandbox{Name: "i"})
	h.must(200, "POST", "/v1/sandboxes/i/pause", key, nil, nil)
	h.callsSince()
	for _, p := range []string{"/internal/activity", "/internal/wake", "/exec/../internal/wake", "/exec/%2e%2e/internal/wake", "/nope"} {
		status, _, _ := h.call("POST", "/v1/sandboxes/i"+p, key, nil)
		if status != 404 && status != 301 && status != 307 && status != 308 {
			t.Errorf("%s: %d", p, status)
		}
	}
	for _, c := range h.callsSince() {
		t.Errorf("node call for an unforwarded path: %s", c)
	}
}

// Every area of the sandbox sub-API the agent serves (docs/AGENT.md) is
// forwarded to it. Tasks once weren't, and answered "route not found".
func TestForwardsEveryAgentArea(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	sb := h.create(key, api.CreateSandbox{Name: "f"})
	for _, p := range []string{"/exec/sessions", "/fs/stat?path=/", "/ports", "/services", "/tasks"} {
		h.callsSince()
		h.call("GET", "/v1/sandboxes/f"+p, key, nil)
		route, _, _ := strings.Cut(p, "?")
		wantCalls(t, h.callsSince(), "agent "+sb.ID+" GET "+route)
	}
}

func TestTemplatesAndEventsList(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	var ts api.TemplateList
	h.must(200, "GET", "/v1/templates", key, nil, &ts)
	if len(ts.Templates) != 2 || ts.Templates[0].Name != "base" || ts.Templates[1].Resources.CPUs != 4 {
		t.Fatalf("templates: %+v", ts)
	}
	sb := h.create(key, api.CreateSandbox{Template: "big", Resources: &api.Resources{CPUs: 1}})
	if sb.Resources != (api.Resources{CPUs: 1, MemoryMiB: 4096, DiskGiB: 40}) {
		t.Fatalf("resources: %+v", sb.Resources)
	}
	h.errCode(400, api.CodeInvalid, "POST", "/v1/sandboxes", key, api.CreateSandbox{Resources: &api.Resources{CPUs: 99}})
	_ = http.StatusOK
}

// A read key reads its project and nothing else: no writes, and no WebSocket,
// since exec and port connections are upgrades of a GET.
func TestReadKey(t *testing.T) {
	h := newHarness(t)
	full := h.project("jake")
	h.create(full, api.CreateSandbox{Name: "rk"})
	var k api.Key
	h.must(201, "POST", "/v1/projects/jake/keys", adminToken, map[string]string{"name": "logs", "scope": "read"}, &k)
	if k.Scope != api.ScopeRead {
		t.Fatalf("scope %q", k.Scope)
	}
	h.errCode(400, api.CodeInvalid, "POST", "/v1/projects/jake/keys", adminToken, map[string]string{"name": "x", "scope": "admin"})
	var keys struct{ Keys []api.Key }
	h.must(200, "GET", "/v1/projects/jake/keys", adminToken, nil, &keys)
	scopes := map[string]string{}
	for _, kk := range keys.Keys {
		scopes[kk.Name] = kk.Scope
	}
	if scopes["test"] != api.ScopeFull || scopes["logs"] != api.ScopeRead {
		t.Fatalf("scopes %v", scopes)
	}

	read := k.Secret
	var list api.SandboxList
	h.must(200, "GET", "/v1/sandboxes", read, nil, &list)
	if len(list.Sandboxes) != 1 {
		t.Fatalf("read key lists %d", len(list.Sandboxes))
	}
	h.must(200, "GET", "/v1/sandboxes/rk", read, nil, nil)
	h.errCode(403, api.CodePermissionDenied, "POST", "/v1/sandboxes", read, api.CreateSandbox{Name: "nope"})
	h.errCode(403, api.CodePermissionDenied, "DELETE", "/v1/sandboxes/rk", read, nil)
	h.errCode(403, api.CodePermissionDenied, "POST", "/v1/sandboxes/rk/pause", read, nil)
	h.errCode(403, api.CodePermissionDenied, "PUT", "/v1/sandboxes/rk/services/x", read, map[string]any{"cmd": []string{"true"}})
	if _, resp, err := h.dialWS("/v1/sandboxes/rk/exec?cmd=true", read); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("exec with a read key: %v %v", resp, err)
	}
	// The full key still does all of it.
	h.must(200, "POST", "/v1/sandboxes/rk/pause", full, nil, nil)
}
