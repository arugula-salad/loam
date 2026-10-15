package apiserver

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

// Phase 2: several nodes sharing a snapshot store (docs/NODE.md, "Phase 2:
// the snapshot store"). The fake nodes give every snapshot a layer of its own,
// "L-<snapshot id>", on top of the layers of the VM it was taken of.

// callsOf returns a node's calls and empties its log.
func (h *harness) callsOf(node string) []string {
	n := h.fakes[node]
	c := n.Calls()
	n.ResetCalls()
	return c
}

func (h *harness) resetCalls() {
	for _, n := range h.fakes {
		n.ResetCalls()
	}
}

func (h *harness) record(id string) db.Sandbox {
	h.t.Helper()
	sb, err := h.store.Sandbox(context.Background(), id)
	if err != nil {
		h.t.Fatalf("record %s: %v", id, err)
	}
	return sb
}

func (h *harness) snap(id string) db.Snapshot {
	h.t.Helper()
	s, err := h.store.Snapshot(context.Background(), id)
	if err != nil {
		h.t.Fatalf("snapshot record %s: %v", id, err)
	}
	return s
}

func (h *harness) hasSnapRecord(id string) bool {
	_, err := h.store.Snapshot(context.Background(), id)
	return err == nil
}

// follow runs loam-api's node watchers (event streams, reconciliation) until
// the test ends.
func (h *harness) follow() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	h.srv.watchNodes(ctx)
	return ctx
}

// lastEvent is the newest event of a sandbox.
func (h *harness) lastEvent(project, id string) api.Event {
	h.t.Helper()
	evs, err := h.store.Events(context.Background(), project, time.Time{})
	if err != nil {
		h.t.Fatal(err)
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Sandbox == id {
			return evs[i].Event
		}
	}
	h.t.Fatalf("no event for %s", id)
	return api.Event{}
}

func (h *harness) storeLayers() string { return strings.Join(h.node.Store().Layers(), " ") }

func (h *harness) storeSnapshots() string { return strings.Join(h.node.Store().Snapshots(), " ") }

// eventuallyFor is eventually with a longer wait, for what goes through a
// follower's reconnect (a second at first).
func eventuallyFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A sandbox paused on one node wakes on another from its stored snapshot when
// its own can't take it, with its disk; the VM it left is deleted. Its next
// pause is a diff on the first, which supersedes it, and its delete frees
// every layer.
func TestPauseOnAWakeOnB(t *testing.T) {
	h := newCluster(t, nodeSpec{"a", 8192}, nodeSpec{"b", 6144})
	key := h.project("jake")
	x := h.create(key, api.CreateSandbox{Name: "x"})
	id := x.ID
	if r := h.record(id); r.Node != "a" || r.Snapshot != "" {
		t.Fatalf("created: %+v", r)
	}
	h.must(200, "PUT", "/v1/sandboxes/x/fs/file?path=/data", key, []byte("one"), nil)
	h.must(200, "POST", "/v1/sandboxes/x/pause", key, nil, nil)
	p1 := id + ".@1"
	if r := h.record(id); r.Snapshot != p1 {
		t.Fatalf("after pause: %+v", r)
	}
	if s := h.snap(p1); !s.Stored || s.Node != "a" || s.Kind != db.SnapPause || !s.Memory || s.State != db.SnapReady ||
		strings.Join(s.Layers, " ") != "L-"+p1 {
		t.Fatalf("pause snapshot: %+v", s)
	}

	// a stops taking VMs (draining, say): the wake goes to b.
	h.fakes["a"].SetReady(false)
	h.resetCalls()
	if out := h.exec(key, "x", "cat", "/data"); string(out.Stdout) != "one" {
		t.Fatalf("the disk after the move: %+v", out)
	}
	wantCalls(t, h.callsOf("b"), "create "+id+" from "+p1, "agent "+id+" POST /exec/run")
	wantCalls(t, h.callsOf("a"), "delete "+id)
	if r := h.record(id); r.Node != "b" || r.State != api.StateRunning || r.Snapshot != p1 {
		t.Fatalf("after the move: %+v", r)
	}
	if e := h.lastEvent("jake", id); e.Type != "sandbox.running" || e.Detail["moved"] != true || e.Detail["from"] != "paused" {
		t.Fatalf("move event: %+v", e)
	}
	if _, wake, _ := h.fakes["b"].VM(id); wake.Sandbox != id || wake.Hostname != "x" {
		t.Fatalf("wake on b: %+v", wake)
	}
	h.fakes["a"].SetReady(true)

	// Paused again on b: a diff on @1, which it supersedes once stored. @1 is
	// deleted through a, the node that made it; its layer stays in @2.
	h.must(200, "POST", "/v1/sandboxes/x/pause", key, nil, nil)
	p2 := id + ".@2"
	if s := h.snap(p2); s.Node != "b" || s.Parent != p1 || strings.Join(s.Layers, " ") != "L-"+p1+" L-"+p2 || !s.Stored {
		t.Fatalf("second pause: %+v", s)
	}
	h.resetCalls()
	h.gc()
	wantCalls(t, h.callsOf("a"), "delete-snapshot "+p1)
	if h.hasSnapRecord(p1) || h.node.Store().HasSnapshot(p1) {
		t.Fatal("a superseded pause snapshot outlived the collection")
	}
	if l := h.storeLayers(); l != "L-"+p1+" L-"+p2 {
		t.Fatalf("layers: %s", l)
	}
	// A second pass has nothing to do.
	h.resetCalls()
	h.gc()
	if c := append(h.callsOf("a"), h.callsOf("b")...); len(c) != 0 {
		t.Fatalf("an idle collection called: %v", c)
	}

	// Waking where it is resumes there.
	h.exec(key, "x", "echo")
	if r := h.record(id); r.Node != "b" {
		t.Fatalf("woke on %s", r.Node)
	}
	h.must(204, "DELETE", "/v1/sandboxes/x", key, nil, nil)
	h.gc()
	if l, s := h.storeLayers(), h.storeSnapshots(); l != "" || s != "" {
		t.Fatalf("after the delete: layers %q, snapshots %q", l, s)
	}
	if layers, _ := h.store.Layers(context.Background()); len(layers) != 0 {
		t.Fatalf("layer records: %v", layers)
	}
}

// A sandbox whose node is full wakes elsewhere if its newest snapshot is
// stored; one whose snapshot is still uploading can't, which is capacity, and
// can once the upload is done.
func TestWakeWhenItsNodeIsFull(t *testing.T) {
	h := newCluster(t, nodeSpec{"a", 4096}, nodeSpec{"b", 3072})
	key := h.project("jake")
	a := h.fakes["a"]
	x := h.create(key, api.CreateSandbox{Name: "x"})
	a.HoldUploads(true)
	h.must(200, "POST", "/v1/sandboxes/x/pause", key, nil, nil)
	if s := h.snap(x.ID + ".@1"); s.Stored {
		t.Fatalf("stored while uploads are held: %+v", s)
	}
	big := h.create(key, api.CreateSandbox{Name: "big", Template: "big"}) // fills a
	if r := h.record(big.ID); r.Node != "a" {
		t.Fatalf("big on %s", r.Node)
	}
	status, raw, hdr := h.call("POST", "/v1/sandboxes/x/wake", key, nil)
	if status != 503 || !strings.Contains(string(raw), api.CodeCapacity) || hdr.Get("Retry-After") == "" {
		t.Fatalf("waking with the snapshot unstored and the node full: %d %s", status, raw)
	}
	if r := h.record(x.ID); r.State != api.StatePaused || r.Node != "a" {
		t.Fatalf("after the refused wake: %+v", r)
	}

	// The upload finishes; the snapshot event says so.
	h.follow()
	a.FinishUploads()
	eventually(t, "the snapshot to be recorded stored", func() bool { return h.snap(x.ID + ".@1").Stored })
	h.resetCalls()
	h.must(200, "POST", "/v1/sandboxes/x/wake", key, nil, nil)
	wantCalls(t, h.callsOf("b"), "create "+x.ID+" from "+x.ID+".@1")
	wantCalls(t, h.callsOf("a"), "delete "+x.ID)

	// No node with room is capacity.
	h.errCode(503, api.CodeCapacity, "POST", "/v1/sandboxes", key, api.CreateSandbox{Name: "more", Template: "big"})
}

// A node lost for two minutes: its running sandboxes with a stored snapshot
// are set paused (with an event saying what is lost) and wake elsewhere; one
// without a stored snapshot can't. A sandbox deleted meanwhile, and the copy
// of the one that moved, are deleted when the node is back.
func TestLostNode(t *testing.T) {
	h := newCluster(t, nodeSpec{"a", 16384}, nodeSpec{"b", 6144})
	key := h.project("jake")
	a, b := h.fakes["a"], h.fakes["b"]
	x := h.create(key, api.CreateSandbox{Name: "x"})
	h.must(200, "PUT", "/v1/sandboxes/x/fs/file?path=/data", key, []byte("before"), nil)
	h.must(200, "POST", "/v1/sandboxes/x/pause", key, nil, nil)
	h.must(200, "PUT", "/v1/sandboxes/x/fs/file?path=/data", key, []byte("after"), nil) // wakes it on a
	y := h.create(key, api.CreateSandbox{Name: "y"})                                    // never snapshotted
	z := h.create(key, api.CreateSandbox{Name: "z"})
	a.HoldUploads(true)
	h.must(200, "POST", "/v1/sandboxes/z/pause", key, nil, nil) // never stored
	a.HoldUploads(false)
	w := h.create(key, api.CreateSandbox{Name: "w"})
	for _, sb := range []api.Sandbox{x, y, z, w} {
		if r := h.record(sb.ID); r.Node != "a" {
			t.Fatalf("%s on %s", sb.Name, r.Node)
		}
	}

	ctx := h.follow()
	a.Down(true)
	// Deleting a sandbox whose node is away leaves a tombstone.
	h.must(204, "DELETE", "/v1/sandboxes/w", key, nil, nil)

	h.clock.Advance(time.Minute)
	h.srv.watchNodes(ctx)
	if r := h.record(x.ID); r.State != api.StateRunning {
		t.Fatalf("x after a minute away: %s", r.State)
	}
	// A wake of a sandbox whose node is away but not lost waits for it, then
	// gives up.
	h.errCode(503, api.CodeSandboxUnavailable, "POST", "/v1/sandboxes/z/wake", key, nil)

	h.clock.Advance(time.Minute + time.Second)
	h.srv.watchNodes(ctx)
	if r := h.record(x.ID); r.State != api.StatePaused || r.Node != "a" {
		t.Fatalf("x after its node was lost: %+v", r)
	}
	if e := h.lastEvent("jake", x.ID); e.Type != "sandbox.paused" || e.Detail["reason"] != "node_lost" || e.Detail["lost"] == nil {
		t.Fatalf("lost event: %+v", e)
	}
	if r := h.record(y.ID); r.State != api.StateRunning {
		t.Fatalf("y, with no snapshot, after its node was lost: %s", r.State)
	}

	// x wakes on b from its pause: what it wrote after is lost.
	b.ResetCalls()
	if out := h.exec(key, "x", "cat", "/data"); string(out.Stdout) != "before" {
		t.Fatalf("x on b: %+v", out)
	}
	wantCalls(t, h.callsOf("b"), "create "+x.ID+" from "+x.ID+".@1", "agent "+x.ID+" POST /exec/run")
	if r := h.record(x.ID); r.Node != "b" || r.State != api.StateRunning {
		t.Fatalf("x after the move: %+v", r)
	}
	// z's snapshot never reached the store, and y has none.
	h.errCode(503, api.CodeSandboxUnavailable, "POST", "/v1/sandboxes/z/wake", key, nil)
	if r := h.record(z.ID); r.State != api.StatePaused || r.Node != "a" {
		t.Fatalf("z after a refused wake: %+v", r)
	}
	h.errCode(503, api.CodeSandboxUnavailable, "POST", "/v1/sandboxes/y/exec/run", key, api.ExecRun{Cmd: []string{"echo"}})

	// a is back: its copy of x (the record names b) and w (deleted) go; y
	// and z are still its, as they were.
	a.Down(false)
	h.srv.watchNodes(ctx)
	eventuallyFor(t, 5*time.Second, "a's stale VMs to be deleted", func() bool {
		return slices.Equal(a.VMs(), sortedIDs(y.ID, z.ID))
	})
	if r := h.record(y.ID); r.State != api.StateRunning || r.Node != "a" {
		t.Fatalf("y after a came back: %+v", r)
	}
	if tombs, _ := h.store.Tombstones(context.Background(), "a"); len(tombs) != 0 {
		t.Fatalf("tombstones left: %v", tombs)
	}
	a.ResetCalls()
	h.must(200, "POST", "/v1/sandboxes/z/wake", key, nil, nil)
	wantCalls(t, a.Calls(), "resume "+z.ID)
}

func sortedIDs(ids ...string) []string {
	slices.Sort(ids)
	return ids
}

// A node that drains pauses its VMs into snapshots of its own naming; loam-api
// records them from the events, and once the node is gone the next call wakes
// each sandbox elsewhere, processes and all, without waiting two minutes.
func TestDrainMovesSandboxes(t *testing.T) {
	h := newCluster(t, nodeSpec{"a", 8192}, nodeSpec{"b", 6144})
	key := h.project("jake")
	a, b := h.fakes["a"], h.fakes["b"]
	x := h.create(key, api.CreateSandbox{Name: "x"})
	h.must(200, "PUT", "/v1/sandboxes/x/fs/file?path=/data", key, []byte("kept"), nil)
	ctx := h.follow()
	eventually(t, "loam-api to follow a", func() bool { return a.Subscribers() > 0 })

	a.Drain()
	drained := x.ID + ".@drain-1"
	eventually(t, "the drain to be recorded", func() bool {
		r := h.record(x.ID)
		return r.State == api.StatePaused && r.Snapshot == drained && h.hasSnapRecord(drained) && h.snap(drained).Stored
	})
	if s := h.snap(drained); s.Kind != db.SnapPause || s.Node != "a" || s.Sandbox != x.ID || !s.Memory {
		t.Fatalf("drain snapshot: %+v", s)
	}
	a.Down(true)
	b.ResetCalls()
	if out := h.exec(key, "x", "cat", "/data"); string(out.Stdout) != "kept" {
		t.Fatalf("after the drain: %+v", out)
	}
	wantCalls(t, h.callsOf("b"), "create "+x.ID+" from "+drained, "agent "+x.ID+" POST /exec/run")
	if tombs, _ := h.store.Tombstones(context.Background(), "a"); !slices.Equal(tombs, []string{x.ID}) {
		t.Fatalf("tombstones: %v", tombs)
	}

	// The node comes back (a new pod): the copy it kept is deleted.
	a.Down(false)
	a.SetReady(true)
	h.srv.watchNodes(ctx)
	eventuallyFor(t, 5*time.Second, "a's copy of x to be deleted", func() bool { return len(a.VMs()) == 0 })
}

// A node with nothing to pause drains in a moment, between two calls: loam-api
// hears of it on the event stream, and wakes the node's paused sandboxes
// elsewhere once it is gone, without waiting two minutes.
func TestQuickDrainIsHeard(t *testing.T) {
	h := newCluster(t, nodeSpec{"a", 8192}, nodeSpec{"b", 6144})
	key := h.project("jake")
	a, b := h.fakes["a"], h.fakes["b"]
	x := h.create(key, api.CreateSandbox{Name: "x"})
	h.must(200, "PUT", "/v1/sandboxes/x/fs/file?path=/data", key, []byte("kept"), nil)
	h.must(200, "POST", "/v1/sandboxes/x/pause", key, nil, nil)
	p1 := x.ID + ".@1"
	eventually(t, "the pause to be stored", func() bool { return h.snap(p1).Stored })
	h.follow()
	eventually(t, "loam-api to follow a", func() bool { return a.Subscribers() > 0 })

	a.Drain()
	eventually(t, "the drain to be heard", func() bool {
		sn, ok := h.srv.nodes.lastSeen("a")
		return ok && !sn.ready
	})
	a.Down(true)
	b.ResetCalls()
	h.must(200, "POST", "/v1/sandboxes/x/wake", key, nil, nil)
	wantCalls(t, h.callsOf("b"), "create "+x.ID+" from "+p1)
	if r := h.record(x.ID); r.Node != "b" || r.State != api.StateRunning {
		t.Fatalf("after the wake: %+v", r)
	}
}

// A frozen nodelet (SIGSTOP) keeps loam-api's stream to it open but silent.
// loam-api drops it for saying nothing, and the reconnect reconciles what
// changed meanwhile.
func TestSilentStreamIsReconnected(t *testing.T) {
	h := newCluster(t, nodeSpec{"a", 8192})
	key := h.project("jake")
	a := h.node
	x := h.create(key, api.CreateSandbox{Name: "x"})
	h.follow()
	eventually(t, "loam-api to follow a", func() bool { return a.Subscribers() > 0 })
	a.Stall(true)
	a.PauseQuietly(x.ID)
	eventuallyFor(t, 5*time.Second, "the quiet pause to be reconciled", func() bool {
		return h.record(x.ID).State == api.StatePaused
	})
}

// A running sandbox whose node paused it on its way out (loam-api didn't hear)
// is woken elsewhere by the next call, not refused.
func TestCallMovesASandboxItsDrainingNodePaused(t *testing.T) {
	h := newCluster(t, nodeSpec{"a", 8192}, nodeSpec{"b", 6144})
	key := h.project("jake")
	a := h.fakes["a"]
	x := h.create(key, api.CreateSandbox{Name: "x"})
	a.Drain() // nobody follows a: loam-api still thinks x is running
	if r := h.record(x.ID); r.State != api.StateRunning {
		t.Fatalf("record: %s", r.State)
	}
	h.resetCalls()
	if out := h.exec(key, "x", "echo", "hi"); string(out.Stdout) != "hi\n" {
		t.Fatalf("exec: %+v", out)
	}
	wantCalls(t, h.callsOf("b"), "create "+x.ID+" from "+x.ID+".@drain-1", "agent "+x.ID+" POST /exec/run")
	if r := h.record(x.ID); r.Node != "b" || r.State != api.StateRunning || r.Snapshot != x.ID+".@drain-1" {
		t.Fatalf("after: %+v", r)
	}
}

// A fork goes to any node with room, its source's first; a checkpoint that
// isn't stored yet only to the node that took it.
func TestForkPlacement(t *testing.T) {
	h := newCluster(t, nodeSpec{"a", 5120}, nodeSpec{"b", 4096})
	key := h.project("jake")
	a := h.fakes["a"]
	src := h.create(key, api.CreateSandbox{Name: "src"})
	h.must(201, "POST", "/v1/sandboxes/src/checkpoints", key, map[string]any{}, nil)
	h.resetCalls()
	f1 := h.create(key, api.CreateSandbox{Name: "f1", From: &api.From{Sandbox: "src", Checkpoint: "v1"}})
	if r := h.record(f1.ID); r.Node != "a" || r.Snapshot != src.ID+".v1" {
		t.Fatalf("f1: %+v", r)
	}
	// a is full now (src and f1 run there): the next fork goes to b.
	h.resetCalls()
	f2 := h.create(key, api.CreateSandbox{Name: "f2", From: &api.From{Sandbox: "src", Checkpoint: "v1"}})
	if r := h.record(f2.ID); r.Node != "b" {
		t.Fatalf("f2 on %s", r.Node)
	}
	wantCalls(t, h.callsOf("b"), "create "+f2.ID+" from "+src.ID+".v1")

	// A checkpoint still uploading can only be forked where it was taken,
	// and a has no room.
	a.HoldUploads(true)
	h.must(201, "POST", "/v1/sandboxes/src/checkpoints", key, map[string]any{}, nil)
	h.errCode(503, api.CodeCapacity, "POST", "/v1/sandboxes", key, api.CreateSandbox{Name: "f3", From: &api.From{Sandbox: "src", Checkpoint: "v2"}})
}

// Layers are refcounted across every snapshot record: restores, forks and
// deletes free exactly the layers nothing references any more.
func TestLayerCollection(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	x := h.create(key, api.CreateSandbox{Name: "x"})
	id := x.ID
	L := func(s string) string { return "L-" + id + "." + s }
	h.must(201, "POST", "/v1/sandboxes/x/checkpoints", key, map[string]any{}, nil) // v1: [v1]
	h.must(200, "POST", "/v1/sandboxes/x/pause", key, nil, nil)                    // @1: [v1 @1]
	// The restore keeps the current state as auto-1 ([v1 @1 auto-1]) and goes back to v1.
	h.must(200, "POST", "/v1/sandboxes/x/checkpoints/v1/restore", key, nil, nil)
	if r := h.record(id); r.Snapshot != id+".v1" {
		t.Fatalf("after the restore the newest snapshot is %q", r.Snapshot)
	}
	h.gc()
	// auto-1 is newer and stored: @1 is superseded, but its layer is in auto-1.
	if h.hasSnapRecord(id + ".@1") {
		t.Fatal("@1 outlived a newer stored snapshot")
	}
	if l := h.storeLayers(); l != strings.Join(sortedIDs(L("v1"), L("@1"), L("auto-1")), " ") {
		t.Fatalf("layers after the restore: %s", l)
	}
	h.must(200, "POST", "/v1/sandboxes/x/pause", key, nil, nil) // @2: [v1 @2]
	if s := h.snap(id + ".@2"); s.Parent != id+".v1" {
		t.Fatalf("@2's parent: %q", s.Parent)
	}
	h.must(204, "DELETE", "/v1/sandboxes/x/checkpoints/auto-1", key, nil, nil)
	h.node.ResetCalls()
	h.gc()
	wantCalls(t, h.node.Calls(), "delete-snapshot "+id+".auto-1", "delete-layer "+L("@1"), "delete-layer "+L("auto-1"))

	// A fork starts from v1 and pauses: [v1 f.@1].
	f := h.create(key, api.CreateSandbox{Name: "f", From: &api.From{Sandbox: "x", Checkpoint: "v1"}})
	h.must(200, "POST", "/v1/sandboxes/f/pause", key, nil, nil)
	h.must(204, "DELETE", "/v1/sandboxes/x", key, nil, nil)
	h.gc()
	// v1 and @2 go; v1's layer stays, in the fork's pause.
	if s := h.storeSnapshots(); s != f.ID+".@1" {
		t.Fatalf("snapshots after x's delete: %s", s)
	}
	if l := h.storeLayers(); l != strings.Join(sortedIDs(L("v1"), "L-"+f.ID+".@1"), " ") {
		t.Fatalf("layers after x's delete: %s", l)
	}
	h.must(204, "DELETE", "/v1/sandboxes/f", key, nil, nil)
	h.gc()
	if l, s := h.storeLayers(), h.storeSnapshots(); l != "" || s != "" {
		t.Fatalf("after both deletes: layers %q snapshots %q", l, s)
	}
}

// A snapshot whose upload is in flight is never collected through another
// node, nor superseded until a newer one is stored; a layer a layer node
// can't delete is tried again.
func TestCollectionWaitsForUploads(t *testing.T) {
	h := newCluster(t, nodeSpec{"a", 8192}, nodeSpec{"b", 6144})
	key := h.project("jake")
	a := h.fakes["a"]
	x := h.create(key, api.CreateSandbox{Name: "x"})
	a.HoldUploads(true)
	h.must(200, "POST", "/v1/sandboxes/x/pause", key, nil, nil)
	h.must(200, "POST", "/v1/sandboxes/x/wake", key, nil, nil)
	h.must(200, "POST", "/v1/sandboxes/x/pause", key, nil, nil)
	h.gc()
	if !h.hasSnapRecord(x.ID+".@1") || !h.hasSnapRecord(x.ID+".@2") {
		t.Fatal("an unstored pause was collected")
	}
	// Stored (the follower hears it), @2 supersedes @1.
	h.follow()
	a.FinishUploads()
	eventually(t, "both stored", func() bool { return h.snap(x.ID+".@1").Stored && h.snap(x.ID+".@2").Stored })
	h.gc()
	if h.hasSnapRecord(x.ID + ".@1") {
		t.Fatal("@1 outlived @2's upload")
	}

	// Deleted while its node is away, with a snapshot never stored: nothing
	// is deleted through b, and a's copy goes once a is back.
	a.HoldUploads(true)
	h.must(200, "POST", "/v1/sandboxes/x/wake", key, nil, nil)
	h.must(200, "POST", "/v1/sandboxes/x/stop", key, nil, nil) // @3, unstored
	a.Down(true)
	h.must(204, "DELETE", "/v1/sandboxes/x", key, nil, nil)
	h.clock.Advance(3 * time.Minute)
	h.fakes["b"].ResetCalls()
	h.gc()
	// @2 is stored and its node lost: it goes through b. @3 waits for a,
	// and keeps every layer it shares with @2.
	wantCalls(t, h.callsOf("b"), "delete-snapshot "+x.ID+".@2")
	if !h.hasSnapRecord(x.ID + ".@3") {
		t.Fatal("an unstored snapshot was collected while its node was away")
	}
	a.Down(false)
	a.ResetCalls()
	h.gc()
	if h.hasSnapRecord(x.ID+".@3") || a.HasSnapshot(x.ID+".@3") {
		t.Fatal("@3 outlived its node's return")
	}
	if c := a.Calls(); !slices.Contains(c, "delete-snapshot "+x.ID+".@3") {
		t.Fatalf("a's calls: %v", c)
	}
	if l := h.storeLayers(); l != "" {
		t.Fatalf("layers left: %s", l)
	}

	// A layer delete that fails is tried again on the next pass.
	y := h.create(key, api.CreateSandbox{Name: "y"})
	a.HoldUploads(false)
	h.must(200, "POST", "/v1/sandboxes/y/pause", key, nil, nil)
	h.must(204, "DELETE", "/v1/sandboxes/y", key, nil, nil)
	for _, n := range h.fakes {
		n.Fail("delete-layer", &nodeapi.Err{Status: 500, ErrorDetail: api.ErrorDetail{Code: "store_error", Message: "garage is down"}})
	}
	h.gc()
	if l := h.storeLayers(); !strings.Contains(l, "L-"+y.ID+".@1") {
		t.Fatalf("a layer went although its delete failed: %q", l)
	}
	for _, n := range h.fakes {
		n.Fail("delete-layer", nil)
	}
	h.gc()
	if l := h.storeLayers(); strings.Contains(l, y.ID) {
		t.Fatalf("the layer outlived a second pass: %q", l)
	}
}

// A pause the node refused is abandoned, and its snapshot (in case it exists)
// collected; the sandbox keeps the snapshot it had.
func TestRefusedPauseIsAbandoned(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	x := h.create(key, api.CreateSandbox{Name: "x"})
	h.node.Fail("pause", &nodeapi.Err{Status: 500, ErrorDetail: api.ErrorDetail{Code: "snapshot_failed", Message: "disk full"}})
	h.errCode(503, api.CodeSandboxUnavailable, "POST", "/v1/sandboxes/x/pause", key, nil)
	if s := h.snap(x.ID + ".@1"); s.State != db.SnapAbandoned {
		t.Fatalf("refused pause: %+v", s)
	}
	if r := h.record(x.ID); r.State != api.StateRunning || r.Snapshot != "" {
		t.Fatalf("after a refused pause: %+v", r)
	}
	h.gc()
	if h.hasSnapRecord(x.ID + ".@1") {
		t.Fatal("an abandoned record outlived the collection")
	}
	// The next pause takes the next name.
	h.must(200, "POST", "/v1/sandboxes/x/pause", key, nil, nil)
	if r := h.record(x.ID); r.Snapshot != x.ID+".@2" {
		t.Fatalf("after the second pause: %+v", r)
	}
}

// A disk-only checkpoint of a running sandbox doesn't become its newest
// snapshot (waking from it would lose memory); a memory one does, and so does
// a disk-only one of a stopped sandbox.
func TestNewestSnapshot(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	x := h.create(key, api.CreateSandbox{Name: "x"})
	h.must(200, "POST", "/v1/sandboxes/x/pause", key, nil, nil)
	h.must(200, "POST", "/v1/sandboxes/x/wake", key, nil, nil)
	h.must(201, "POST", "/v1/sandboxes/x/checkpoints", key, map[string]any{"memory": false}, nil)
	if r := h.record(x.ID); r.Snapshot != x.ID+".@1" {
		t.Fatalf("after a disk-only checkpoint of a running sandbox: %q", r.Snapshot)
	}
	h.must(201, "POST", "/v1/sandboxes/x/checkpoints", key, map[string]any{}, nil)
	if r := h.record(x.ID); r.Snapshot != x.ID+".v2" {
		t.Fatalf("after a memory checkpoint: %q", r.Snapshot)
	}
	h.must(200, "POST", "/v1/sandboxes/x/stop", key, nil, nil)
	h.must(201, "POST", "/v1/sandboxes/x/checkpoints", key, map[string]any{"memory": false}, nil)
	// The checkpoint wakes it first (it is a sub-API route), so it is running.
	if r := h.record(x.ID); r.Snapshot != x.ID+".@2" {
		t.Fatalf("after a disk-only checkpoint of a woken sandbox: %q", r.Snapshot)
	}
	if vm, _, _ := h.node.VM(x.ID); vm.Snapshot != x.ID+".@2" {
		t.Fatalf("the node's newest: %q", vm.Snapshot)
	}
	_ = http.StatusOK
}

// A sandbox that moved away and back: the tombstone left when its old node
// didn't answer doesn't delete the VM it now has there.
func TestMoveBackKeepsTheLiveVM(t *testing.T) {
	h := newCluster(t, nodeSpec{"a", 8192}, nodeSpec{"b", 6144})
	key := h.project("jake")
	a, b := h.fakes["a"], h.fakes["b"]
	x := h.create(key, api.CreateSandbox{Name: "x"})
	h.must(200, "POST", "/v1/sandboxes/x/pause", key, nil, nil)
	a.Down(true)
	h.clock.Advance(3 * time.Minute)
	h.must(200, "POST", "/v1/sandboxes/x/wake", key, nil, nil) // to b; a's copy is tombstoned
	h.must(200, "POST", "/v1/sandboxes/x/pause", key, nil, nil)
	a.Down(false)
	b.SetReady(false)
	a.ResetCalls()
	h.must(200, "POST", "/v1/sandboxes/x/wake", key, nil, nil) // back to a, over its stale copy
	wantCalls(t, a.Calls(), "create "+x.ID+" from "+x.ID+".@2", "delete "+x.ID, "create "+x.ID+" from "+x.ID+".@2")
	if r := h.record(x.ID); r.Node != "a" {
		t.Fatalf("x on %s", r.Node)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.SetReady(true)
	h.srv.watchNodes(ctx)
	if _, _, ok := a.VM(x.ID); !ok {
		t.Fatal("the tombstone deleted the live VM")
	}
	if tombs, _ := h.store.Tombstones(context.Background(), "a"); len(tombs) != 0 {
		t.Fatalf("tombstones: %v", tombs)
	}
	if _, _, ok := b.VM(x.ID); ok {
		t.Fatal("b kept its copy")
	}
}

// A node that comes back with a VM whose record names another node (the
// sandbox moved meanwhile, and no tombstone says so) has it deleted at
// reconcile.
func TestReconcileDeletesStaleVMs(t *testing.T) {
	h := newCluster(t, nodeSpec{"a", 8192}, nodeSpec{"b", 6144})
	key := h.project("jake")
	a := h.fakes["a"]
	x := h.create(key, api.CreateSandbox{Name: "x"})
	h.must(200, "POST", "/v1/sandboxes/x/pause", key, nil, nil)
	a.Down(true)
	h.clock.Advance(3 * time.Minute)
	h.must(200, "POST", "/v1/sandboxes/x/wake", key, nil, nil)
	if r := h.record(x.ID); r.Node != "b" {
		t.Fatalf("x on %s", r.Node)
	}
	// As if loam-api had died before it left the tombstone.
	if err := h.store.DeleteTombstone(context.Background(), "a", x.ID); err != nil {
		t.Fatal(err)
	}
	a.Down(false)
	a.ResetCalls()
	ctx := h.follow()
	// b's follower reconciles x too, under x's lock, and when it holds the
	// lock as a's looks, the delete waits for the tombstone's next pass.
	h.buryStale(ctx, "a", x.ID)
	if r := h.record(x.ID); r.Node != "b" || r.State != api.StateRunning {
		t.Fatalf("x after a's reconcile: %+v", r)
	}
}

// buryStale waits for node's stale VM of sandbox id to be deleted and its
// tombstone, if one was left, to go, making the watchNodes passes the
// lifecycle loop makes every nodeWatchEvery, only quicker. The follower's own
// pass may race one of them to the same tombstone, so the node may be asked
// to delete the VM more than once (the second gets a not found); it must be
// asked nothing else.
func (h *harness) buryStale(ctx context.Context, node, id string) {
	h.t.Helper()
	n := h.fakes[node]
	eventuallyFor(h.t, 5*time.Second, node+"'s stale copy to be deleted", func() bool {
		tombs, _ := h.store.Tombstones(context.Background(), node)
		if len(n.VMs()) == 0 && !slices.Contains(tombs, id) {
			return true
		}
		h.srv.watchNodes(ctx)
		return false
	})
	calls := n.Calls()
	if len(calls) == 0 || slices.ContainsFunc(calls, func(c string) bool { return c != "delete "+id }) {
		wantCalls(h.t, calls, "delete "+id)
	}
}

// A stale VM found while its sandbox is in a transition is not forgotten:
// the follower's stream stays up, so no reconcile comes again, and the
// tombstone left for it is what deletes it on the next watchNodes pass.
func TestStaleVMOfABusySandboxIsTombstoned(t *testing.T) {
	h := newCluster(t, nodeSpec{"a", 8192}, nodeSpec{"b", 6144})
	key := h.project("jake")
	a := h.fakes["a"]
	x := h.create(key, api.CreateSandbox{Name: "x"})
	h.must(200, "POST", "/v1/sandboxes/x/pause", key, nil, nil)
	a.Down(true)
	h.clock.Advance(3 * time.Minute)
	h.must(200, "POST", "/v1/sandboxes/x/wake", key, nil, nil)
	bg := context.Background()
	if err := h.store.DeleteTombstone(bg, "a", x.ID); err != nil {
		t.Fatal(err)
	}
	a.Down(false)
	a.ResetCalls()

	// x is busy (a transition holds its lock) while a reconnects.
	rt := h.srv.rt(x.ID)
	rt.mu.Lock()
	ctx := h.follow()
	eventuallyFor(t, 5*time.Second, "a tombstone for a's stale copy", func() bool {
		tombs, _ := h.store.Tombstones(bg, "a")
		return slices.Contains(tombs, x.ID)
	})
	if len(a.VMs()) != 1 {
		t.Fatalf("a's copy went while x was busy: %v", a.VMs())
	}
	rt.mu.Unlock()

	h.buryStale(ctx, "a", x.ID)
	if r := h.record(x.ID); r.Node != "b" || r.State != api.StateRunning {
		t.Fatalf("x after a's reconcile: %+v", r)
	}
}

// A snapshot record left pending (loam-api died while it waited for the
// nodelet) is settled when loam-api next connects to the node: finished if
// the node has the snapshot, abandoned if not.
func TestPendingSnapshotsAreSettled(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	x := h.create(key, api.CreateSandbox{Name: "x"})
	h.must(200, "POST", "/v1/sandboxes/x/pause", key, nil, nil)
	ctx := context.Background()
	made := x.ID + ".@1"
	if _, err := h.store.UpdateSnapshot(ctx, made, func(s *db.Snapshot) error {
		s.State, s.Stored, s.Layers = db.SnapPending, false, nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	never := x.ID + ".@9"
	if _, err := h.store.CreateSnapshot(ctx, db.Snapshot{ID: never, Sandbox: x.ID, Kind: db.SnapPause, Node: "geek",
		Memory: true, State: db.SnapPending, CreatedAt: h.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	h.follow()
	eventually(t, "the pending records to be settled", func() bool {
		return h.snap(made).State == db.SnapReady && h.snap(never).State == db.SnapAbandoned
	})
	if s := h.snap(made); !s.Stored || strings.Join(s.Layers, " ") != "L-"+made {
		t.Fatalf("settled from the node: %+v", s)
	}
	h.gc()
	if h.hasSnapRecord(never) || !h.hasSnapRecord(made) {
		t.Fatal("the collection took the wrong record")
	}
}
