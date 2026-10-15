package db_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/db/dbtest"
	"github.com/arugula-salad/loam/pkg/api"
)

// Both stores keep the same contract: Memory always, Postgres when
// LOAM_TEST_DATABASE_URL names one.
func TestMemory(t *testing.T)   { conformance(t, db.NewMemory()) }
func TestPostgres(t *testing.T) { conformance(t, dbtest.Postgres(t)) }

func TestMemorySnapshots(t *testing.T)   { snapshotConformance(t, db.NewMemory()) }
func TestPostgresSnapshots(t *testing.T) { snapshotConformance(t, dbtest.Postgres(t)) }

func TestMemorySecrets(t *testing.T)   { secretConformance(t, db.NewMemory()) }
func TestPostgresSecrets(t *testing.T) { secretConformance(t, dbtest.Postgres(t)) }

func TestMemoryTraffic(t *testing.T)   { trafficConformance(t, db.NewMemory()) }
func TestPostgresTraffic(t *testing.T) { trafficConformance(t, dbtest.Postgres(t)) }

func conformance(t *testing.T, st db.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	is := func(err, want error) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Fatalf("got %v, want %v", err, want)
		}
	}

	must(st.CreateProject(ctx, api.Project{Name: "jake", CreatedAt: now}))
	must(st.CreateProject(ctx, api.Project{Name: "other", CreatedAt: now.Add(time.Second)}))
	is(st.CreateProject(ctx, api.Project{Name: "jake", CreatedAt: now}), db.ErrExists)
	ps, err := st.Projects(ctx)
	must(err)
	if len(ps) != 2 || ps[0].Name != "jake" || !ps[0].CreatedAt.Equal(now) {
		t.Fatalf("projects: %+v", ps)
	}
	_, err = st.Project(ctx, "nope")
	is(err, db.ErrNotFound)

	k := db.Key{ID: "key-1", Project: "jake", Name: "laptop", Hash: []byte{1, 2, 3}, CreatedAt: now}
	must(st.CreateKey(ctx, k))
	is(st.CreateKey(ctx, db.Key{ID: "key-2", Project: "jake", Name: "laptop", Hash: []byte{4}, CreatedAt: now}), db.ErrExists)
	got, err := st.KeyByHash(ctx, []byte{1, 2, 3})
	must(err)
	if got.ID != "key-1" || got.Project != "jake" {
		t.Fatalf("key: %+v", got)
	}
	_, err = st.KeyByHash(ctx, []byte{9})
	is(err, db.ErrNotFound)
	must(st.CreateKey(ctx, db.Key{ID: "key-3", Project: "jake", Name: "ci", Hash: []byte{5}, CreatedAt: now}))
	must(st.DeleteKey(ctx, "jake", "ci")) // by name
	is(st.DeleteKey(ctx, "other", "key-1"), db.ErrNotFound)
	keys, err := st.Keys(ctx, "jake")
	must(err)
	if len(keys) != 1 {
		t.Fatalf("keys: %+v", keys)
	}

	sb := db.Sandbox{
		ID: "sb-aaaaaaaaaaaa", Name: "scratch", Project: "jake", Template: "base", State: api.StateCreating,
		Resources: api.Resources{CPUs: 2, MemoryMiB: 2048, DiskGiB: 20},
		Env:       map[string]string{"FOO": "bar"}, Labels: map[string]string{"owner": "me"},
		Idle: api.Idle{TimeoutS: 300, Action: api.ActionPause}, HTTPPort: 8080, Node: "geek",
		CreatedAt: now, UpdatedAt: now, LastActiveAt: now,
	}
	must(st.CreateSandbox(ctx, sb))
	dupe := sb
	dupe.ID = "sb-bbbbbbbbbbbb"
	is(st.CreateSandbox(ctx, dupe), db.ErrExists)
	dupe.Project = "other"
	must(st.CreateSandbox(ctx, dupe)) // names are per project

	got2, err := st.SandboxByName(ctx, "jake", "scratch")
	must(err)
	if got2.ID != sb.ID || got2.Env["FOO"] != "bar" || got2.Node != "geek" || !got2.CreatedAt.Equal(now) || got2.Deadline != nil {
		t.Fatalf("sandbox: %+v", got2)
	}
	// A sandbox made from an image keeps its digest, with no template.
	img := sb
	img.ID, img.Name, img.Template, img.Image = "sb-cccccccccccc", "fromimage", "", "ghcr.io/arugula-salad/app@sha256:"+strings.Repeat("a", 64)
	must(st.CreateSandbox(ctx, img))
	if got, err := st.Sandbox(ctx, img.ID); err != nil || got.Image != img.Image || got.Template != "" {
		t.Fatalf("image sandbox: %+v %v", got, err)
	}
	if got, _ := st.Sandbox(ctx, sb.ID); got.Image != "" {
		t.Fatalf("a template sandbox has an image: %+v", got)
	}
	must(st.DeleteSandbox(ctx, img.ID))
	at := now.Add(time.Hour)
	up, err := st.UpdateSandbox(ctx, sb.ID, func(s *db.Sandbox) error {
		s.State = api.StateRunning
		s.Deadline = &api.Deadline{At: at, Action: api.ActionStop}
		s.NextCheckpoint++
		s.Labels["x"] = "y"
		return nil
	})
	must(err)
	if up.State != api.StateRunning || up.NextCheckpoint != 1 {
		t.Fatalf("update: %+v", up)
	}
	got3, err := st.Sandbox(ctx, sb.ID)
	must(err)
	if got3.Deadline == nil || !got3.Deadline.At.Equal(at) || got3.Deadline.Action != api.ActionStop || got3.Labels["x"] != "y" {
		t.Fatalf("after update: %+v", got3)
	}
	boom := errors.New("boom")
	_, err = st.UpdateSandbox(ctx, sb.ID, func(s *db.Sandbox) error { s.State = "x"; return boom })
	is(err, boom)
	if g, _ := st.Sandbox(ctx, sb.ID); g.State != api.StateRunning {
		t.Fatalf("aborted update stored: %s", g.State)
	}
	_, err = st.UpdateSandbox(ctx, "sb-none", func(*db.Sandbox) error { return nil })
	is(err, db.ErrNotFound)

	all, err := st.Sandboxes(ctx, "")
	must(err)
	mine, err := st.Sandboxes(ctx, "jake")
	must(err)
	if len(all) != 2 || len(mine) != 1 {
		t.Fatalf("lists: %d all, %d mine", len(all), len(mine))
	}

	is(st.DeleteProject(ctx, "jake"), db.ErrNotEmpty)

	for i, id := range []string{"v1", "auto-1", "v2"} {
		must(st.CreateCheckpoint(ctx, api.Checkpoint{ID: id, Sandbox: sb.ID, Memory: i != 1, Auto: id == "auto-1", CreatedAt: now}))
	}
	is(st.CreateCheckpoint(ctx, api.Checkpoint{ID: "v1", Sandbox: sb.ID, CreatedAt: now}), db.ErrExists)
	cps, err := st.Checkpoints(ctx, sb.ID)
	must(err)
	if len(cps) != 3 || cps[0].ID != "v1" || cps[1].ID != "auto-1" || cps[2].ID != "v2" || !cps[1].Auto || cps[1].Memory {
		t.Fatalf("checkpoints: %+v", cps)
	}
	must(st.DeleteCheckpoint(ctx, sb.ID, "auto-1"))
	_, err = st.Checkpoint(ctx, sb.ID, "auto-1")
	is(err, db.ErrNotFound)

	must(st.SetPort(ctx, db.PortSetting{Sandbox: sb.ID, Port: 3000, Access: "private"}))
	must(st.SetPort(ctx, db.PortSetting{Sandbox: sb.ID, Port: 3000, Access: "public"}))
	must(st.SetPort(ctx, db.PortSetting{Sandbox: sb.ID, Port: 80, Access: "private"}))
	ports, err := st.Ports(ctx, sb.ID)
	must(err)
	if len(ports) != 2 || ports[0].Port != 80 || ports[1].Access != "public" {
		t.Fatalf("ports: %+v", ports)
	}

	must(st.AddEvent(ctx, db.Event{Project: "jake", Event: api.Event{Type: "sandbox.created", Sandbox: sb.ID, At: now}}))
	must(st.AddEvent(ctx, db.Event{Project: "jake", Event: api.Event{Type: "sandbox.running", Sandbox: sb.ID, At: now.Add(time.Second), Detail: map[string]any{"why": "test"}}}))
	evs, err := st.Events(ctx, "jake", now)
	must(err)
	if len(evs) != 2 || evs[1].Detail["why"] != "test" {
		t.Fatalf("events: %+v", evs)
	}
	must(st.PruneEvents(ctx, now.Add(time.Millisecond)))
	evs, _ = st.Events(ctx, "jake", time.Time{})
	if len(evs) != 1 {
		t.Fatalf("after prune: %+v", evs)
	}

	must(st.DeleteSandbox(ctx, sb.ID))
	is(st.DeleteSandbox(ctx, sb.ID), db.ErrNotFound)
	if cps, _ := st.Checkpoints(ctx, sb.ID); len(cps) != 0 {
		t.Fatalf("checkpoints outlived their sandbox: %+v", cps)
	}
	must(st.DeleteProject(ctx, "jake"))
	if _, err := st.KeyByHash(ctx, []byte{1, 2, 3}); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("key outlived its project: %v", err)
	}
	is(st.DeleteProject(ctx, "jake"), db.ErrNotFound)
}

// snapshotConformance covers the phase 2 records: snapshots, the garbage rule
// (db.go, GarbageSnapshots), the layer set and tombstones.
func snapshotConformance(t *testing.T, st db.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	garbage := func() string {
		t.Helper()
		g, err := st.GarbageSnapshots(ctx)
		must(err)
		var ids []string
		for _, s := range g {
			ids = append(ids, s.ID)
		}
		return strings.Join(ids, " ")
	}
	strs := func(l []string, err error) string {
		t.Helper()
		must(err)
		return strings.Join(l, " ")
	}

	must(st.CreateProject(ctx, api.Project{Name: "p", CreatedAt: now}))
	mk := func(id string) db.Sandbox {
		sb := db.Sandbox{ID: id, Name: id, Project: "p", Template: "base", State: api.StateRunning,
			Idle: api.Idle{TimeoutS: 300, Action: api.ActionPause}, HTTPPort: 8080, Node: "a",
			CreatedAt: now, UpdatedAt: now, LastActiveAt: now}
		must(st.CreateSandbox(ctx, sb))
		return sb
	}
	x := mk("sb-x")
	snap := func(id, kind, cp string, layers ...string) db.Snapshot {
		s, err := st.CreateSnapshot(ctx, db.Snapshot{ID: id, Sandbox: x.ID, Kind: kind, Checkpoint: cp, Node: "a",
			Layers: layers, Memory: true, State: db.SnapPending, CreatedAt: now})
		must(err)
		return s
	}
	ready := func(id string, stored bool) {
		_, err := st.UpdateSnapshot(ctx, id, func(s *db.Snapshot) error { s.State, s.Stored = db.SnapReady, stored; return nil })
		must(err)
	}
	point := func(sb, id string) {
		_, err := st.UpdateSandbox(ctx, sb, func(s *db.Sandbox) error { s.Snapshot = id; return nil })
		must(err)
	}

	p1 := snap("sb-x.@1", db.SnapPause, "", "L1")
	if p1.Seq == 0 {
		t.Fatal("no seq")
	}
	if _, err := st.CreateSnapshot(ctx, db.Snapshot{ID: "sb-x.@1", Sandbox: x.ID, Kind: db.SnapPause, Node: "a", State: db.SnapPending, CreatedAt: now}); !errors.Is(err, db.ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if g := garbage(); g != "" {
		t.Fatalf("a pending record is garbage: %s", g)
	}
	ready("sb-x.@1", true)
	point(x.ID, "sb-x.@1")
	got, err := st.Snapshot(ctx, "sb-x.@1")
	must(err)
	if !got.Stored || got.State != db.SnapReady || strings.Join(got.Layers, ",") != "L1" || got.Seq != p1.Seq || !got.CreatedAt.Equal(now) {
		t.Fatalf("snapshot: %+v", got)
	}

	// A newer pause that isn't stored yet supersedes nothing.
	snap("sb-x.@2", db.SnapPause, "", "L1", "L2")
	ready("sb-x.@2", false)
	point(x.ID, "sb-x.@2")
	if g := garbage(); g != "" {
		t.Fatalf("garbage before the newer one is stored: %s", g)
	}
	// Stored, it does.
	ready("sb-x.@2", true)
	if g := garbage(); g != "sb-x.@1" {
		t.Fatalf("garbage once @2 is stored: %q", g)
	}
	must(st.DeleteSnapshot(ctx, "sb-x.@1"))

	// A checkpoint's snapshot lives as long as its checkpoint, or a sandbox naming it.
	snap("sb-x.v1", db.SnapCheckpoint, "v1", "L1", "L2", "L3")
	ready("sb-x.v1", true)
	must(st.CreateCheckpoint(ctx, api.Checkpoint{ID: "v1", Sandbox: x.ID, Memory: true, CreatedAt: now}))
	point(x.ID, "sb-x.v1") // supersedes @2 too
	if g := garbage(); g != "sb-x.@2" {
		t.Fatalf("garbage after v1: %q", g)
	}
	must(st.DeleteSnapshot(ctx, "sb-x.@2"))
	fork := mk("sb-f")
	point(fork.ID, "sb-x.v1")
	point(x.ID, "")
	must(st.DeleteCheckpoint(ctx, x.ID, "v1"))
	if g := garbage(); g != "" {
		t.Fatalf("a fork's starting point is garbage: %q", g)
	}
	point(fork.ID, "")
	if g := garbage(); g != "sb-x.v1" {
		t.Fatalf("garbage after the fork moved on: %q", g)
	}

	// Abandoned records are garbage; a deleted sandbox's pauses are too.
	snap("sb-x.@3", db.SnapPause, "", "L9")
	_, err = st.UpdateSnapshot(ctx, "sb-x.@3", func(s *db.Snapshot) error { s.State = db.SnapAbandoned; return nil })
	must(err)
	snap("sb-x.@4", db.SnapStop, "", "L4")
	ready("sb-x.@4", false)
	if g := garbage(); g != "sb-x.v1 sb-x.@3" {
		t.Fatalf("garbage: %q", g)
	}
	must(st.DeleteSandbox(ctx, x.ID))
	if g := garbage(); g != "sb-x.v1 sb-x.@3 sb-x.@4" {
		t.Fatalf("garbage after the sandbox's delete: %q", g)
	}
	of, err := st.SnapshotsOf(ctx, x.ID)
	must(err)
	on, err := st.SnapshotsOn(ctx, "a")
	must(err)
	if len(of) != 3 || len(on) != 3 || of[0].ID != "sb-x.v1" {
		t.Fatalf("lists: %+v %+v", of, on)
	}

	// Layers: the set is every layer a record referenced; deleting one that is
	// referenced is refused.
	if l := strs(st.Layers(ctx)); l != "L1 L2 L3 L4 L9" {
		t.Fatalf("layers: %s", l)
	}
	if l := strs(st.UnreferencedLayers(ctx)); l != "" {
		t.Fatalf("unreferenced: %s", l)
	}
	must(st.DeleteSnapshot(ctx, "sb-x.v1"))
	if l := strs(st.UnreferencedLayers(ctx)); l != "L1 L2 L3" {
		t.Fatalf("unreferenced after v1: %s", l)
	}
	if err := st.DeleteLayer(ctx, "L4"); !errors.Is(err, db.ErrInUse) {
		t.Fatalf("deleting a referenced layer: %v", err)
	}
	must(st.DeleteLayer(ctx, "L1"))
	if err := st.DeleteLayer(ctx, "L1"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("deleting it again: %v", err)
	}
	must(st.AddLayers(ctx, []string{"L1"}))
	if l := strs(st.Layers(ctx)); l != "L1 L2 L3 L4 L9" {
		t.Fatalf("layers after adding back: %s", l)
	}

	must(st.AddTombstone(ctx, "a", "sb-x"))
	must(st.AddTombstone(ctx, "a", "sb-x"))
	must(st.AddTombstone(ctx, "b", "sb-y"))
	if l := strs(st.Tombstones(ctx, "a")); l != "sb-x" {
		t.Fatalf("tombstones: %s", l)
	}
	must(st.DeleteTombstone(ctx, "a", "sb-x"))
	if err := st.DeleteTombstone(ctx, "a", "sb-x"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("tombstone deleted twice: %v", err)
	}
}

// secretConformance covers the phase 3 records: a sandbox's network and
// secrets, and the secrets themselves, which a sandbox can only name while
// they exist and which can't be deleted while one names them.
func secretConformance(t *testing.T, st db.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	is := func(err, want error) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Fatalf("got %v, want %v", err, want)
		}
	}
	must(st.CreateProject(ctx, api.Project{Name: "p", CreatedAt: now}))
	must(st.CreateProject(ctx, api.Project{Name: "q", CreatedAt: now}))
	secret := func(project, name string, at time.Time, value ...byte) db.Secret {
		return db.Secret{Project: project, Name: name, Hosts: []string{"a.example.com"},
			Placeholder: "__loam_" + strings.ToLower(name) + "__", Value: value, CreatedAt: at, UpdatedAt: at}
	}

	_, err := st.PutSecret(ctx, secret("nope", "X", now, 1))
	is(err, db.ErrNotFound)
	gh := secret("p", "GITHUB_TOKEN", now, 1, 2, 3)
	gh.Hosts = []string{"api.github.com", "github.com"}
	got, err := st.PutSecret(ctx, gh)
	must(err)
	if got.Inject != nil || len(got.Hosts) != 2 || !got.CreatedAt.Equal(now) {
		t.Fatalf("put: %+v", got)
	}
	later := now.Add(time.Minute)
	repl := secret("p", "GITHUB_TOKEN", later, 4, 5)
	repl.Placeholder = "x-__loam_github_token__"
	repl.Inject = &api.Inject{Header: "authorization", Format: "Bearer {value}"}
	got, err = st.PutSecret(ctx, repl)
	must(err)
	if !got.CreatedAt.Equal(now) || !got.UpdatedAt.Equal(later) || got.Inject == nil || got.Inject.Format != "Bearer {value}" {
		t.Fatalf("replace: %+v", got)
	}
	got, err = st.Secret(ctx, "p", "GITHUB_TOKEN")
	must(err)
	if !bytes.Equal(got.Value, []byte{4, 5}) || got.Placeholder != "x-__loam_github_token__" || got.Hosts[0] != "a.example.com" {
		t.Fatalf("get: %+v", got)
	}
	_, err = st.Secret(ctx, "q", "GITHUB_TOKEN")
	is(err, db.ErrNotFound)
	_, err = st.PutSecret(ctx, secret("p", "A", now, 9))
	must(err)
	l, err := st.Secrets(ctx, "p")
	must(err)
	if len(l) != 2 || l[0].Name != "A" || l[1].Name != "GITHUB_TOKEN" {
		t.Fatalf("list: %+v", l)
	}

	// A sandbox names only secrets its project has.
	network := api.Network{Egress: []api.Rule{{Include: "defaults"}, {Domain: "github.com", Action: api.RuleAllow, Ports: []int{22, 443}}}}
	sb := db.Sandbox{ID: "sb-s", Name: "s", Project: "p", Template: "base", State: api.StateRunning,
		Idle: api.Idle{TimeoutS: 300, Action: api.ActionPause}, HTTPPort: 8080, Network: network,
		Secrets: []string{"GITHUB_TOKEN", "NOPE"}, CreatedAt: now, UpdatedAt: now, LastActiveAt: now}
	is(st.CreateSandbox(ctx, sb), db.ErrNoSecret)
	other := sb
	other.ID, other.Project, other.Secrets = "sb-o", "q", []string{"GITHUB_TOKEN"}
	is(st.CreateSandbox(ctx, other), db.ErrNoSecret)
	sb.Secrets = []string{"GITHUB_TOKEN"}
	must(st.CreateSandbox(ctx, sb))
	back, err := st.Sandbox(ctx, sb.ID)
	must(err)
	if len(back.Network.Egress) != 2 || back.Network.Egress[0].Include != "defaults" ||
		fmt.Sprint(back.Network.Egress[1].Ports) != "[22 443]" || strings.Join(back.Secrets, " ") != "GITHUB_TOKEN" {
		t.Fatalf("sandbox: %+v", back)
	}
	_, err = st.UpdateSandbox(ctx, sb.ID, func(s *db.Sandbox) error { s.Secrets = append(s.Secrets, "NOPE"); return nil })
	is(err, db.ErrNoSecret)

	// A bare sandbox stores an empty network and no secrets, not nulls.
	bare := db.Sandbox{ID: "sb-b", Name: "b", Project: "p", Template: "base", State: api.StateRunning,
		Idle: api.Idle{TimeoutS: 300, Action: api.ActionPause}, HTTPPort: 8080, CreatedAt: now, UpdatedAt: now, LastActiveAt: now}
	must(st.CreateSandbox(ctx, bare))
	if back, _ := st.Sandbox(ctx, bare.ID); back.Network.Egress == nil || back.Secrets == nil {
		t.Fatalf("bare sandbox: %+v", back)
	}

	is(st.DeleteSecret(ctx, "p", "GITHUB_TOKEN"), db.ErrInUse)
	is(st.DeleteSecret(ctx, "q", "GITHUB_TOKEN"), db.ErrNotFound)
	_, err = st.UpdateSandbox(ctx, sb.ID, func(s *db.Sandbox) error { s.Secrets = []string{"A"}; return nil })
	must(err)
	must(st.DeleteSecret(ctx, "p", "GITHUB_TOKEN"))
	is(st.DeleteSecret(ctx, "p", "GITHUB_TOKEN"), db.ErrNotFound)
	must(st.DeleteSandbox(ctx, sb.ID))
	must(st.DeleteSandbox(ctx, bare.ID))
	must(st.DeleteSecret(ctx, "p", "A"))

	// A project's secrets go with it.
	_, err = st.PutSecret(ctx, secret("q", "B", now, 1))
	must(err)
	must(st.DeleteProject(ctx, "q"))
	must(st.CreateProject(ctx, api.Project{Name: "q", CreatedAt: now}))
	_, err = st.Secret(ctx, "q", "B")
	is(err, db.ErrNotFound)
}

func trafficConformance(t *testing.T, st db.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.CreateProject(ctx, api.Project{Name: "p", CreatedAt: now}))
	sb := db.Sandbox{ID: "sb-t", Name: "t", Project: "p", Template: "base", State: api.StateRunning,
		Idle: api.Idle{TimeoutS: 300, Action: api.ActionPause}, HTTPPort: 8080, CreatedAt: now, UpdatedAt: now, LastActiveAt: now}
	must(st.CreateSandbox(ctx, sb))

	day := func(d string, req int64, visitors ...string) db.Traffic {
		t := db.Traffic{Sandbox: sb.ID, Day: d, Traffic: api.Traffic{Requests: req, Status2xx: req, BytesSent: 10 * req}}
		for _, v := range visitors {
			t.NewVisitors = append(t.NewVisitors, []byte(v))
		}
		return t
	}
	// Counts add up and visitors are a set; a deleted sandbox's traffic is dropped.
	must(st.AddTraffic(ctx, []db.Traffic{day("2026-10-11", 2, "a", "b"), day("2026-10-12", 1, "a")}))
	must(st.AddTraffic(ctx, []db.Traffic{day("2026-10-12", 3, "a", "c"), {Sandbox: "sb-gone", Day: "2026-10-12", Traffic: api.Traffic{Requests: 1}, NewVisitors: [][]byte{[]byte("x")}}}))
	got, err := st.TrafficOf(ctx, sb.ID, "2026-10-01")
	must(err)
	if len(got) != 2 || got[0].Day != "2026-10-11" || got[0].Requests != 2 || got[0].Visitors != 2 ||
		got[1].Day != "2026-10-12" || got[1].Requests != 4 || got[1].BytesSent != 40 || got[1].Visitors != 2 {
		t.Fatalf("traffic: %+v", got)
	}
	if got, _ := st.TrafficOf(ctx, sb.ID, "2026-10-12"); len(got) != 1 || got[0].Day != "2026-10-12" {
		t.Fatalf("since: %+v", got)
	}
	must(st.PruneTraffic(ctx, "2026-10-12"))
	if got, _ := st.TrafficOf(ctx, sb.ID, "2026-01-01"); len(got) != 1 || got[0].Visitors != 2 {
		t.Fatalf("pruned: %+v", got)
	}

	// A day's salt is stable; a later day's replaces it.
	s1, err := st.VisitorSalt(ctx, "2026-10-12")
	must(err)
	again, err := st.VisitorSalt(ctx, "2026-10-12")
	must(err)
	if len(s1) != 32 || !bytes.Equal(s1, again) {
		t.Fatalf("salt: %x then %x", s1, again)
	}
	s2, err := st.VisitorSalt(ctx, "2026-10-13")
	must(err)
	if bytes.Equal(s1, s2) {
		t.Fatal("two days share a salt")
	}

	must(st.DeleteSandbox(ctx, sb.ID))
	if got, _ := st.TrafficOf(ctx, sb.ID, "2026-01-01"); len(got) != 0 {
		t.Fatalf("a deleted sandbox's traffic: %+v", got)
	}
}
