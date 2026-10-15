package apiserver

import (
	"testing"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

func TestCheckpointRestoreFork(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	sb := h.create(key, api.CreateSandbox{Name: "src"})
	id := sb.ID
	h.must(200, "PUT", "/v1/sandboxes/src/fs/file?path=/data", key, []byte("one"), nil)
	h.callsSince()

	var v1, v2 api.Checkpoint
	h.must(201, "POST", "/v1/sandboxes/src/checkpoints", key, map[string]any{"comment": "first"}, &v1)
	if v1.ID != "v1" || !v1.Memory || v1.Auto || v1.Comment != "first" || v1.Sandbox != id {
		t.Fatalf("v1: %+v", v1)
	}
	h.must(200, "PUT", "/v1/sandboxes/src/fs/file?path=/data", key, []byte("two"), nil)
	h.must(201, "POST", "/v1/sandboxes/src/checkpoints", key, map[string]any{"memory": false}, &v2)
	if v2.ID != "v2" || v2.Memory {
		t.Fatalf("v2: %+v", v2)
	}
	wantCalls(t, h.callsSince(),
		"snapshot "+id+" "+id+".v1 memory",
		"agent "+id+" PUT /fs/file",
		"snapshot "+id+" "+id+".v2 disk")

	// A checkpoint wakes a paused sandbox first, as every route under it does.
	h.must(200, "POST", "/v1/sandboxes/src/pause", key, nil, nil)
	h.callsSince()
	var v3 api.Checkpoint
	h.must(201, "POST", "/v1/sandboxes/src/checkpoints", key, map[string]any{}, &v3)
	wantCalls(t, h.callsSince(), "resume "+id, "snapshot "+id+" "+id+".v3 memory")

	// Restore keeps the current state as auto-1 first, then restores.
	var restored api.Sandbox
	h.must(200, "POST", "/v1/sandboxes/src/checkpoints/v1/restore", key, nil, &restored)
	if restored.State != api.StateRunning {
		t.Fatalf("restored: %+v", restored)
	}
	wantCalls(t, h.callsSince(), "snapshot "+id+" "+id+".auto-1 memory", "restore "+id+" "+id+".v1")
	if b, _ := h.node.File(id, "/data"); string(b) != "one" {
		t.Fatalf("after restoring v1: %q", b)
	}
	var list api.CheckpointList
	h.must(200, "GET", "/v1/sandboxes/src/checkpoints", key, nil, &list)
	if ids := cpIDs(list); ids != "v1 v2 v3 auto-1" {
		t.Fatalf("checkpoints: %s", ids)
	}
	var auto api.Checkpoint
	h.must(200, "GET", "/v1/sandboxes/src/checkpoints/auto-1", key, nil, &auto)
	if !auto.Auto || !auto.Memory {
		t.Fatalf("auto-1: %+v", auto)
	}

	// A stopped sandbox's state is kept disk-only.
	h.must(200, "POST", "/v1/sandboxes/src/stop", key, nil, nil)
	h.callsSince()
	h.must(200, "POST", "/v1/sandboxes/src/checkpoints/v2/restore", key, nil, nil)
	wantCalls(t, h.callsSince(), "snapshot "+id+" "+id+".auto-2 disk", "restore "+id+" "+id+".v2")
	if b, _ := h.node.File(id, "/data"); string(b) != "two" {
		t.Fatalf("after restoring v2: %q", b)
	}

	// Only the newest five automatic checkpoints are kept.
	for range 5 {
		h.must(200, "POST", "/v1/sandboxes/src/checkpoints/v1/restore", key, nil, nil)
	}
	h.must(200, "GET", "/v1/sandboxes/src/checkpoints", key, nil, &list)
	if ids := cpIDs(list); ids != "v1 v2 v3 auto-3 auto-4 auto-5 auto-6 auto-7" {
		t.Fatalf("after pruning: %s", ids)
	}
	h.gc() // the snapshots of deleted checkpoints go at the next collection
	if h.node.HasSnapshot(id+".auto-1") || h.node.HasSnapshot(id+".auto-2") || !h.node.HasSnapshot(id+".auto-3") {
		t.Fatal("pruned automatic checkpoints' snapshots were not deleted (or kept ones were)")
	}
	h.errCode(404, api.CodeNotFound, "POST", "/v1/sandboxes/src/checkpoints/auto-1/restore", key, nil)

	// Fork: a new sandbox from v1 of src, with src's template and resources, on src's node.
	h.callsSince()
	fork := h.create(key, api.CreateSandbox{Name: "fork", From: &api.From{Sandbox: "src", Checkpoint: "v1"}, Env: map[string]string{"X": "1"}})
	if fork.Template != sb.Template || fork.Resources != sb.Resources || fork.State != api.StateRunning {
		t.Fatalf("fork: %+v", fork)
	}
	wantCalls(t, h.callsSince(), "create "+fork.ID+" from "+id+".v1")
	if b, _ := h.node.File(fork.ID, "/data"); string(b) != "one" {
		t.Fatalf("fork's file: %q", b)
	}
	h.errCode(400, api.CodeInvalid, "POST", "/v1/sandboxes", key, api.CreateSandbox{Template: "base", From: &api.From{Sandbox: "src", Checkpoint: "v1"}})
	h.errCode(400, api.CodeInvalid, "POST", "/v1/sandboxes", key, api.CreateSandbox{Resources: &api.Resources{CPUs: 1}, From: &api.From{Sandbox: "src", Checkpoint: "v1"}})
	h.errCode(404, api.CodeNotFound, "POST", "/v1/sandboxes", key, api.CreateSandbox{From: &api.From{Sandbox: "src", Checkpoint: "v99"}})

	// Delete one; then the sandbox, which takes its checkpoints' snapshots with it.
	h.must(204, "DELETE", "/v1/sandboxes/src/checkpoints/v3", key, nil, nil)
	h.gc()
	if h.node.HasSnapshot(id + ".v3") {
		t.Fatal("v3's snapshot outlived it")
	}
	h.errCode(404, api.CodeNotFound, "GET", "/v1/sandboxes/src/checkpoints/v3", key, nil)
	var next api.Checkpoint
	h.must(201, "POST", "/v1/sandboxes/src/checkpoints", key, map[string]any{}, &next)
	if next.ID != "v4" {
		t.Fatalf("IDs are reused: %s", next.ID)
	}
	h.must(204, "DELETE", "/v1/sandboxes/src", key, nil, nil)
	h.gc()
	// The fork starts from v1, so v1 stays; the rest go.
	if !h.node.HasSnapshot(id + ".v1") {
		t.Fatal("the checkpoint a fork starts from was deleted with its sandbox")
	}
	for _, c := range []string{"v2", "v4", "auto-7"} {
		if h.node.HasSnapshot(id + "." + c) {
			t.Fatalf("%s outlived its sandbox", c)
		}
	}
	// The fork is its own sandbox and stays.
	if got := h.get(key, "fork"); got.State != api.StateRunning {
		t.Fatalf("fork after its source's delete: %+v", got)
	}
}

func cpIDs(l api.CheckpointList) string {
	s := ""
	for i, c := range l.Checkpoints {
		if i > 0 {
			s += " "
		}
		s += c.ID
	}
	return s
}

// The idle rule: no call in flight for the timeout and an idle agent pause
// (or stop) a running sandbox; anything else keeps it running.
func TestIdleRule(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	sb := h.create(key, api.CreateSandbox{Name: "idle", Idle: &api.Idle{TimeoutS: 60}})
	h.node.SetActivity(sb.ID, idleAgent(30))

	h.clock.Advance(59e9)
	h.tick()
	if got := h.get(key, "idle"); got.State != api.StateRunning {
		t.Fatalf("paused before its timeout: %s", got.State)
	}
	h.clock.Advance(2e9)
	// The agent has seen activity more recently than the timeout.
	h.tick()
	if got := h.get(key, "idle"); got.State != api.StateRunning {
		t.Fatalf("paused while the agent was active: %s", got.State)
	}
	for _, busy := range []struct {
		SessionsAttached, Tasks int
		Busy                    bool
	}{{1, 0, false}, {0, 1, false}, {0, 0, true}} {
		a := idleAgent(120)
		a.SessionsAttached, a.Tasks, a.Busy = busy.SessionsAttached, busy.Tasks, busy.Busy
		h.node.SetActivity(sb.ID, a)
		h.tick()
		if got := h.get(key, "idle"); got.State != api.StateRunning {
			t.Fatalf("paused while %+v: %s", busy, got.State)
		}
	}

	// A call in flight holds it, however long: an exec socket open.
	h.node.SetActivity(sb.ID, idleAgent(120))
	ws, _, err := h.dialWS("/v1/sandboxes/idle/exec?cmd=sleep", key)
	if err != nil {
		t.Fatal(err)
	}
	ws.ReadMessage() // started
	h.clock.Advance(3600e9)
	h.tick()
	if got := h.get(key, "idle"); got.State != api.StateRunning {
		t.Fatalf("paused with a call in flight: %s", got.State)
	}
	ws.Close()
	eventually(t, "the exec call to end", func() bool { _, ok := h.srv.rt(sb.ID).idleFor(h.clock.Now()); return ok })

	// The idle period starts when the last call ended.
	h.clock.Advance(30e9)
	h.tick()
	if got := h.get(key, "idle"); got.State != api.StateRunning {
		t.Fatalf("paused 30 s after the last call: %s", got.State)
	}
	h.clock.Advance(31e9)
	h.callsSince()
	h.tick()
	if got := h.get(key, "idle"); got.State != api.StatePaused {
		t.Fatalf("not paused when idle: %s", got.State)
	}
	wantCalls(t, h.callsSince(), "pause "+sb.ID+" "+sb.ID+".@1")

	// idle.action stop stops it; none leaves it running.
	h.must(200, "PATCH", "/v1/sandboxes/idle", key, map[string]any{"idle": map[string]any{"timeout_s": 60, "action": "stop"}}, nil)
	h.exec(key, "idle", "echo")
	h.node.SetActivity(sb.ID, idleAgent(120))
	h.clock.Advance(61e9)
	h.tick()
	if got := h.get(key, "idle"); got.State != api.StateStopped {
		t.Fatalf("idle stop: %s", got.State)
	}
	h.must(200, "PATCH", "/v1/sandboxes/idle", key, map[string]any{"idle": map[string]any{"action": "none"}}, nil)
	h.exec(key, "idle", "echo")
	h.node.SetActivity(sb.ID, idleAgent(1e6))
	h.clock.Advance(1e6 * 1e9)
	h.tick()
	if got := h.get(key, "idle"); got.State != api.StateRunning {
		t.Fatalf("idle none: %s", got.State)
	}
}

func idleAgent(seconds int64) nodeapi.Activity { return nodeapi.Activity{IdleMS: seconds * 1000} }

// Deadlines act whatever the sandbox is doing, unless it is protected; pause
// and stop spend the deadline, delete deletes.
func TestDeadlines(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	at := h.clock.Now().Add(10 * 60e9)
	for _, a := range []string{"pause", "stop", "delete"} {
		h.create(key, api.CreateSandbox{Name: "d-" + a, Deadline: &api.Deadline{At: at, Action: a}})
	}
	h.create(key, api.CreateSandbox{Name: "kept", Protected: true, Deadline: &api.Deadline{At: at, Action: "delete"}})

	// A call in flight doesn't hold a deadline off.
	ws, _, err := h.dialWS("/v1/sandboxes/d-pause/exec?cmd=sleep", key)
	if err != nil {
		t.Fatal(err)
	}
	ws.ReadMessage()
	h.clock.Advance(10*60e9 + 1)
	h.tick()
	// The open session was ended by the pause, with 1011.
	if _, _, err := ws.ReadMessage(); !isClose(err, 1011) {
		t.Fatalf("exec socket at the deadline's pause: %v", err)
	}

	if got := h.get(key, "d-pause"); got.State != api.StatePaused || got.Deadline != nil {
		t.Fatalf("d-pause: %+v", got)
	}
	if got := h.get(key, "d-stop"); got.State != api.StateStopped || got.Deadline != nil {
		t.Fatalf("d-stop: %+v", got)
	}
	h.errCode(404, api.CodeNotFound, "GET", "/v1/sandboxes/d-delete", key, nil)
	kept := h.get(key, "kept")
	if kept.State != api.StateRunning || kept.Deadline == nil {
		t.Fatalf("protected: %+v", kept)
	}
	// Waking after a spent deadline isn't undone on the next pass.
	h.exec(key, "d-pause", "echo")
	h.tick()
	if got := h.get(key, "d-pause"); got.State != api.StateRunning {
		t.Fatalf("woken after the deadline: %s", got.State)
	}
	// Unprotecting hands it straight back to the rule.
	h.must(200, "PATCH", "/v1/sandboxes/kept", key, map[string]any{"protected": false}, nil)
	h.tick()
	h.errCode(404, api.CodeNotFound, "GET", "/v1/sandboxes/kept", key, nil)
}
