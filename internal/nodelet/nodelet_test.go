package nodelet

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/internal/nodelet/firecracker"
	"github.com/arugula-salad/loam/internal/nodelet/network"
	"github.com/arugula-salad/loam/internal/snapstore"
	"github.com/arugula-salad/loam/internal/snapstore/header"
	"github.com/arugula-salad/loam/pkg/api"
)

func resources(mib int) api.Resources { return api.Resources{CPUs: 2, MemoryMiB: mib, DiskGiB: 10} }

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// writeDisk is the guest writing s to its disk at off.
func writeDisk(t *testing.T, h *harness, id string, off int64, s string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(h.data, "vms", id, firecracker.DiskFile), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte(s), off); err != nil {
		t.Fatal(err)
	}
}

func readDisk(t *testing.T, h *harness, id string, off int64, n int) string {
	t.Helper()
	f, err := os.Open(filepath.Join(h.data, "vms", id, firecracker.DiskFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, n)
	f.ReadAt(b, off)
	return string(b)
}

func (h *harness) snap(id string) nodeapi.Snapshot {
	h.t.Helper()
	s, err := h.n.GetSnapshot(context.Background(), id)
	if err != nil {
		h.t.Fatalf("snapshot %s: %v", id, err)
	}
	return s
}

// ownLayer is snapshot id's own layer, from its local copy.
func (h *harness) ownLayer(id string) string {
	h.t.Helper()
	_, spec, err := h.n.localSnap(id)
	if err != nil {
		h.t.Fatal(err)
	}
	return spec.Layer.String()
}

// A VM goes through its whole life: boot, pause (a memory snapshot, stored,
// and no process), resume (restored from it), a second pause (a diff), stop
// (a disk snapshot), a fresh boot, delete (which leaves the snapshots).
func TestLifecycle(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	v, err := h.n.Create(ctx, createReq("sb-one"))
	if err != nil {
		t.Fatal(err)
	}
	if v.State != nodeapi.VMRunning || v.BootedAt == nil || v.Resources.MemoryMiB != 1024 || v.Snapshot != "" {
		t.Fatalf("created: %+v", v)
	}
	if w := h.vmm.agent.lastWake(); w.Sandbox != "sb-one" || w.Env["FOO"] != "bar" || w.Time.IsZero() {
		t.Errorf("wake: %+v", w)
	}
	if got := h.vmm.boots[0].BootArgs; !slices.Contains(got, "loam.root=/dev/vda") || !slices.Contains(got, "loam.hostname=scratch") {
		t.Errorf("boot args %v", got)
	}
	if n := h.n.Node(); n.MemoryMiBAllocated != 1024 || n.VMs != 1 || n.Name != "geek" || !n.Ready {
		t.Errorf("node: %+v", n)
	}

	m := h.vmm.last()
	if v, err = h.n.Pause(ctx, "sb-one", "sb-one.@1"); err != nil || v.State != nodeapi.VMPaused || v.Snapshot != "sb-one.@1" {
		t.Fatalf("pause: %+v %v", v, err)
	}
	if want := []string{"squeeze", "pause", "snapshot", "kill"}; !slices.Equal(m.calls(), want) {
		t.Errorf("pause did %v, want %v", m.calls(), want)
	}
	sdir := filepath.Join(h.data, "snapshots", "sb-one.@1")
	for _, f := range []string{snapFile, specFile, snapState, snapMem, firecracker.DiskFile} {
		if !fileExists(filepath.Join(sdir, f)) {
			t.Errorf("the pause's snapshot lacks %s", f)
		}
	}
	if n := h.n.Node(); n.MemoryMiBAllocated != 0 {
		t.Errorf("a paused VM holds %d MiB", n.MemoryMiBAllocated)
	}
	if !h.stored("sb-one.@1") {
		t.Fatal("the pause's snapshot was not stored")
	}
	s1 := h.snap("sb-one.@1")
	if !s1.Stored || !s1.Memory || s1.Parent != "" || !slices.Equal(s1.Layers, []string{h.ownLayer("sb-one.@1")}) || s1.VM != "sb-one" {
		t.Errorf("first pause's snapshot: %+v", s1)
	}
	if v, _ := h.n.Pause(ctx, "sb-one", "sb-one.@9"); v.State != nodeapi.VMPaused || v.Snapshot != "sb-one.@1" || fileExists(filepath.Join(h.data, "snapshots", "sb-one.@9")) {
		t.Errorf("pausing a paused VM is not a no-op: %+v", v)
	}
	if _, err := h.n.Activity(ctx, "sb-one"); code(err) != api.CodeConflict {
		t.Errorf("activity of a paused VM: %v", err)
	}

	if v, err = h.n.Resume(ctx, "sb-one", nodeapi.Resume{Wake: nodeapi.Wake{Sandbox: "sb-one", Hostname: "scratch"}}); err != nil || v.State != nodeapi.VMRunning {
		t.Fatalf("resume: %+v %v", v, err)
	}
	if len(h.vmm.restores) != 1 || h.vmm.restores[0] != filepath.Join(sdir, snapState) {
		t.Errorf("resume restored %v", h.vmm.restores)
	}
	m = h.vmm.last()
	if m.lazy || m.read(0) != "booted sb-one" {
		t.Errorf("resumed memory: lazy %v, page 0 %q", m.lazy, m.read(0))
	}
	if !fileExists(sdir) {
		t.Error("the pause's snapshot went with the resume")
	}
	if got := m.calls(); !slices.Equal(got, []string{"balloon 0"}) {
		t.Errorf("after the restore: %v, want the balloon let down", got)
	}
	a, err := h.n.Activity(ctx, "sb-one")
	if err != nil || a.Tasks != 2 || !a.Busy {
		t.Errorf("activity: %+v %v", a, err)
	}

	// Resume on a running VM hands the agent the new wake, and nothing else.
	wakes := h.vmm.agent.wakeCount()
	if _, err := h.n.Resume(ctx, "sb-one", nodeapi.Resume{Wake: nodeapi.Wake{Sandbox: "sb-one", Env: map[string]string{"NEW": "1"}}}); err != nil {
		t.Fatal(err)
	}
	if h.vmm.agent.wakeCount() != wakes+1 || h.vmm.agent.lastWake().Env["NEW"] != "1" || len(h.vmm.machines) != 2 {
		t.Error("resume of a running VM did more or less than a wake")
	}

	// The second pause is a diff over the first.
	m.write(7*pageSize, "a process's state")
	if v, err = h.n.Pause(ctx, "sb-one", "sb-one.@2"); err != nil || v.Snapshot != "sb-one.@2" {
		t.Fatalf("second pause: %+v %v", v, err)
	}
	if !slices.Contains(m.calls(), "snapshot diff") {
		t.Errorf("the second pause did %v, want a diff", m.calls())
	}
	h.stored("sb-one.@2")
	s2 := h.snap("sb-one.@2")
	if s2.Parent != "sb-one.@1" || !slices.Equal(s2.Layers, []string{h.ownLayer("sb-one.@1"), h.ownLayer("sb-one.@2")}) || !s2.Stored {
		t.Errorf("second pause's snapshot: %+v", s2)
	}
	// Its local memory is whole: the diff over the first's.
	if pages, err := readPages(filepath.Join(h.data, "snapshots", "sb-one.@2", snapMem)); err != nil ||
		pages[0] != "booted sb-one" || pages[7*pageSize] != "a process's state" {
		t.Errorf("the second pause's memory: %v %v", pages, err)
	}
	if _, err := h.n.Resume(ctx, "sb-one", nodeapi.Resume{Wake: nodeapi.Wake{Sandbox: "sb-one"}}); err != nil {
		t.Fatal(err)
	}
	if got := h.vmm.last().read(7 * pageSize); got != "a process's state" {
		t.Errorf("after the second resume: %q", got)
	}

	if v, err = h.n.Stop(ctx, "sb-one", "sb-one.@3"); err != nil || v.State != nodeapi.VMStopped || v.BootedAt != nil || v.Snapshot != "sb-one.@3" {
		t.Fatalf("stop: %+v %v", v, err)
	}
	if !slices.Contains(h.vmm.agent.calls, "POST /internal/poweroff") {
		t.Error("stop did not ask the agent to power off")
	}
	if s3 := h.snap("sb-one.@3"); s3.Memory || s3.Parent != "sb-one.@2" {
		t.Errorf("stop's snapshot: %+v", s3)
	}
	if v, err := h.n.Stop(ctx, "sb-one", "sb-one.@4"); err != nil || v.Snapshot != "sb-one.@3" {
		t.Errorf("stopping a stopped VM: %+v %v", v, err)
	}
	if _, err := h.n.Pause(ctx, "sb-one", "sb-one.@5"); code(err) != api.CodeConflict {
		t.Errorf("pause of a stopped VM: %v", err)
	}
	if v, err = h.n.Resume(ctx, "sb-one", nodeapi.Resume{Wake: nodeapi.Wake{Sandbox: "sb-one"}}); err != nil || v.State != nodeapi.VMRunning || v.Snapshot != "sb-one.@3" {
		t.Fatalf("resume from stopped: %+v %v", v, err)
	}
	if len(h.vmm.boots) != 2 {
		t.Errorf("resume from stopped: %d boots, want a fresh one", len(h.vmm.boots))
	}

	if err := h.n.Delete("sb-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.n.VM("sb-one"); code(err) != api.CodeNotFound {
		t.Errorf("deleted VM: %v", err)
	}
	if fileExists(filepath.Join(h.data, "vms", "sb-one")) {
		t.Error("the VM's directory survived its delete")
	}
	if n := h.n.Node(); n.MemoryMiBAllocated != 0 || n.VMs != 0 {
		t.Errorf("node after delete: %+v", n)
	}
	if len(h.n.Snapshots()) != 3 || !h.stored("sb-one.@1") {
		t.Error("deleting a VM deleted its snapshots")
	}
}

// Pause and stop name their snapshot; one that is missing, malformed or
// taken is refused.
func TestPauseNames(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.n.Create(ctx, createReq("sb-n"))
	for _, name := range []string{"", "../x", ".hidden", "a/b"} {
		if _, err := h.n.Pause(ctx, "sb-n", name); code(err) != api.CodeInvalid {
			t.Errorf("pause into %q: %v", name, err)
		}
		if _, err := h.n.Stop(ctx, "sb-n", name); code(err) != api.CodeInvalid {
			t.Errorf("stop into %q: %v", name, err)
		}
	}
	if _, err := h.n.Snapshot(ctx, "sb-n", nodeapi.CreateSnapshot{ID: "sb-n.v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.n.Pause(ctx, "sb-n", "sb-n.v1"); code(err) != api.CodeAlreadyExists {
		t.Errorf("pause into a taken name: %v", err)
	}
	if v, _ := h.n.VM("sb-n"); v.State != nodeapi.VMRunning {
		t.Errorf("a refused pause left the VM %s", v.State)
	}
}

// A memory snapshot of a running VM cuts the agent streams open into it:
// Firecracker resets the guest's vsock transport and leaves the host's ends
// open, so the nodelet closes them. A disk-only snapshot leaves them be.
func TestMemorySnapshotCutsStreams(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	if _, err := h.n.Create(ctx, createReq("sb-s")); err != nil {
		t.Fatal(err)
	}
	c, err := h.n.DialAgent(ctx, "sb-s")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ended := make(chan error, 1)
	go func() {
		_, err := c.Read(make([]byte, 1))
		ended <- err
	}()
	if _, err := h.n.Snapshot(ctx, "sb-s", nodeapi.CreateSnapshot{ID: "sb-s.v1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ended:
		t.Fatalf("a disk-only snapshot ended the stream: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := h.n.Snapshot(ctx, "sb-s", nodeapi.CreateSnapshot{ID: "sb-s.v2", Memory: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("the stream read data after the memory snapshot")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a memory snapshot left the agent stream open")
	}
	// The VM runs on, and a new stream reaches its agent.
	hc := &http.Client{Transport: nodeapi.AgentTransport(func(ctx context.Context) (net.Conn, error) { return h.n.DialAgent(ctx, "sb-s") })}
	resp, err := hc.Get("http://agent/hello")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// The transport closes its stream behind the answer, not before.
	v := h.n.vms["sb-s"]
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		v.streamMu.Lock()
		n := len(v.streams)
		v.streamMu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d streams still listed after they closed", n)
		}
	}
}

// A restarted nodelet keeps its paused and stopped VMs and their newest
// snapshots, reports the running ones stopped (their memory died with the old
// one), and reaps their VMMs.
func TestRestartKeepsState(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	for _, id := range []string{"sb-paused", "sb-running", "sb-stopped", "sb-ck"} {
		if _, err := h.n.Create(ctx, createReq(id)); err != nil {
			t.Fatal(err)
		}
	}
	h.n.Pause(ctx, "sb-paused", "sb-paused.@1")
	h.n.Stop(ctx, "sb-stopped", "sb-stopped.@1")
	h.n.Snapshot(ctx, "sb-ck", nodeapi.CreateSnapshot{ID: "sb-ck.v1", Memory: true})
	h.n.WaitUploads(ctx)

	h.reopen() // the old one's machines are left as a crash would leave them
	want := map[string]nodeapi.VM{"sb-paused": {State: nodeapi.VMPaused, Snapshot: "sb-paused.@1"}, "sb-running": {State: nodeapi.VMStopped},
		"sb-stopped": {State: nodeapi.VMStopped, Snapshot: "sb-stopped.@1"}, "sb-ck": {State: nodeapi.VMStopped, Snapshot: "sb-ck.v1"}}
	vms := h.n.VMs()
	if len(vms) != len(want) {
		t.Fatalf("%d VMs after restart: %+v", len(vms), vms)
	}
	for _, v := range vms {
		if v.State != want[v.ID].State || v.Snapshot != want[v.ID].Snapshot {
			t.Errorf("%s is %s on %q after restart, want %+v", v.ID, v.State, v.Snapshot, want[v.ID])
		}
	}
	if !slices.Contains(h.vmm.reaped, "sb-running") {
		t.Errorf("reaped %v", h.vmm.reaped)
	}
	if h.n.Node().MemoryMiBAllocated != 0 {
		t.Error("a restarted nodelet holds memory for VMs it doesn't run")
	}
	if v, err := h.n.Resume(ctx, "sb-paused", nodeapi.Resume{Wake: nodeapi.Wake{}}); err != nil || v.State != nodeapi.VMRunning {
		t.Fatalf("resume after restart: %+v %v", v, err)
	}
	if got := h.vmm.restores[len(h.vmm.restores)-1]; got != filepath.Join(h.data, "snapshots", "sb-paused.@1", snapState) {
		t.Errorf("restored from %s", got)
	}
	// sb-ck was running: it boots fresh, and its next pause's memory is whole again.
	h.n.Resume(ctx, "sb-ck", nodeapi.Resume{Wake: nodeapi.Wake{}})
	m := h.vmm.last()
	h.n.Pause(ctx, "sb-ck", "sb-ck.@1")
	if !slices.Contains(m.calls(), "snapshot") {
		t.Errorf("a fresh boot's pause did %v", m.calls())
	}
}

// An upload the store holds up survives the nodelet: the next one finishes it.
func TestUploadAfterRestart(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.n.Create(ctx, createReq("sb-u"))
	h.shared.gate.hold()
	if _, err := h.n.Pause(ctx, "sb-u", "sb-u.@1"); err != nil {
		t.Fatal(err)
	}
	if s := h.snap("sb-u.@1"); s.Stored || len(s.Layers) != 1 {
		t.Errorf("a snapshot whose upload is held: %+v", s)
	}
	h.n.Shutdown()
	h.shared.gate.release()
	h.reopen()
	if !h.stored("sb-u.@1") || !h.snap("sb-u.@1").Stored {
		t.Error("the restarted nodelet did not finish the upload")
	}
}

// Admission reserves each running VM's whole memory and refuses what doesn't fit.
func TestAdmission(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.MemoryMiB = 2048 })
	ctx := context.Background()
	if _, err := h.n.Create(ctx, createReq("sb-a")); err != nil {
		t.Fatal(err)
	}
	big := createReq("sb-b")
	big.Resources = resources(1536)
	_, err := h.n.Create(ctx, big)
	var e *Error
	if !errors.As(err, &e) || e.Code != api.CodeCapacity || e.Status != http.StatusServiceUnavailable || e.RetryAfter == 0 {
		t.Fatalf("over budget: %v", err)
	}
	if _, err := h.n.VM("sb-b"); code(err) != api.CodeNotFound {
		t.Error("a refused create left a VM behind")
	}
	if fileExists(filepath.Join(h.data, "vms", "sb-b")) {
		t.Error("a refused create left a directory behind")
	}
	// Pausing the first frees its share; the second then fits.
	if _, err := h.n.Pause(ctx, "sb-a", "sb-a.@1"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.n.Create(ctx, big); err != nil {
		t.Fatal(err)
	}
	// And now the first can't wake, and stays paused.
	if _, err := h.n.Resume(ctx, "sb-a", nodeapi.Resume{Wake: nodeapi.Wake{}}); code(err) != api.CodeCapacity {
		t.Errorf("resume over budget: %v", err)
	}
	if v, _ := h.n.VM("sb-a"); v.State != nodeapi.VMPaused || v.Snapshot != "sb-a.@1" {
		t.Errorf("a refused resume changed the VM: %+v", v)
	}
	big.Resources = resources(8192)
	if _, err := h.n.Create(ctx, big); code(err) != api.CodeInvalid {
		t.Errorf("a VM larger than the node: %v", err)
	}
}

// Snapshots: memory and disk while running (the VM runs on), disk of a
// stopped VM, forks from both kinds, restores, and the refusals.
func TestSnapshots(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	if _, err := h.n.Create(ctx, createReq("sb-src")); err != nil {
		t.Fatal(err)
	}
	m := h.vmm.last()
	s, err := h.n.Snapshot(ctx, "sb-src", nodeapi.CreateSnapshot{ID: "sb-src.v1", Memory: true})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Memory || s.VM != "sb-src" || s.Template.Name != "base" || s.Resources != resources(1024) || s.SizeBytes == 0 || len(s.Layers) != 1 {
		t.Errorf("snapshot: %+v", s)
	}
	if want := []string{"squeeze", "pause", "snapshot", "resume", "balloon 0"}; !slices.Equal(m.calls(), want) {
		t.Errorf("memory snapshot did %v, want %v", m.calls(), want)
	}
	// A memory snapshot is the VM's newest: it runs on from it.
	if v, _ := h.n.VM("sb-src"); v.State != nodeapi.VMRunning || v.Snapshot != "sb-src.v1" {
		t.Errorf("after a memory snapshot the VM is %+v", v)
	}
	sdir := filepath.Join(h.data, "snapshots", "sb-src.v1")
	for _, f := range []string{snapFile, snapState, snapMem, firecracker.DiskFile} {
		if !fileExists(filepath.Join(sdir, f)) {
			t.Errorf("snapshot lacks %s", f)
		}
	}
	if _, err := h.n.Snapshot(ctx, "sb-src", nodeapi.CreateSnapshot{ID: "sb-src.v1"}); code(err) != api.CodeAlreadyExists {
		t.Errorf("a second snapshot with one ID: %v", err)
	}
	writeDisk(t, h, "sb-src", 8192, "after v1")
	s2, err := h.n.Snapshot(ctx, "sb-src", nodeapi.CreateSnapshot{ID: "sb-src.v2"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(h.vmm.agent.calls, "POST /internal/sync") {
		t.Error("a disk snapshot did not sync the guest first")
	}
	// Not the newest: waking from it would lose the memory the VM has.
	if v, _ := h.n.VM("sb-src"); v.Snapshot != "sb-src.v1" || s2.Parent != "sb-src.v1" || s2.Memory {
		t.Errorf("a disk snapshot of a running VM: %+v, the VM's newest %s", s2, v.Snapshot)
	}
	if got := len(h.n.Snapshots()); got != 2 {
		t.Errorf("%d snapshots listed", got)
	}
	// The next memory snapshot is a diff over v1.
	m.write(9*pageSize, "since v1")
	if s3, err := h.n.Snapshot(ctx, "sb-src", nodeapi.CreateSnapshot{ID: "sb-src.v3", Memory: true}); err != nil || s3.Parent != "sb-src.v1" || !slices.Contains(m.calls(), "snapshot diff") {
		t.Errorf("a second memory snapshot: %+v %v, %v", s3, err, m.calls())
	}

	// A fork from the memory snapshot restores its memory; one from the disk snapshot boots.
	boots := len(h.vmm.boots)
	fork := nodeapi.CreateVM{ID: "sb-fork", From: &nodeapi.FromSnapshot{Snapshot: "sb-src.v1"}, Wake: nodeapi.Wake{Sandbox: "sb-fork"}}
	v, err := h.n.Create(ctx, fork)
	if err != nil {
		t.Fatal(err)
	}
	if v.Template.Name != "base" || v.Resources != resources(1024) || h.vmm.restores[len(h.vmm.restores)-1] != filepath.Join(sdir, snapState) || v.Snapshot != "sb-src.v1" {
		t.Errorf("fork: %+v from %v", v, h.vmm.restores)
	}
	if got := readDisk(t, h, "sb-fork", 0, 9); got != "rootfs of" {
		t.Errorf("the fork's disk is not the snapshot's: %q", got)
	}
	fork.ID, fork.From.Snapshot = "sb-fork2", "sb-src.v2"
	if _, err := h.n.Create(ctx, fork); err != nil || len(h.vmm.boots) != boots+1 {
		t.Errorf("fork from a disk snapshot: %v, %d boots", err, len(h.vmm.boots)-boots)
	}
	if got := readDisk(t, h, "sb-fork2", 8192, 8); got != "after v1" {
		t.Errorf("the disk fork's disk: %q", got)
	}
	fork.ID, fork.Resources = "sb-fork3", resources(2048)
	if _, err := h.n.Create(ctx, fork); code(err) != api.CodeInvalid {
		t.Errorf("fork with other resources: %v", err)
	}
	fork.Resources, fork.From.Snapshot = api.Resources{}, "sb-nope.v9"
	if _, err := h.n.Create(ctx, fork); code(err) != api.CodeNotFound {
		t.Errorf("fork from a missing snapshot: %v", err)
	}

	// Snapshots of a stopped VM hold its disk only, and become its newest.
	h.n.Stop(ctx, "sb-fork2", "sb-fork2.@1")
	if _, err := h.n.Snapshot(ctx, "sb-fork2", nodeapi.CreateSnapshot{ID: "sb-fork2.v1", Memory: true}); code(err) != api.CodeConflict {
		t.Errorf("memory snapshot of a stopped VM: %v", err)
	}
	if _, err := h.n.Snapshot(ctx, "sb-fork2", nodeapi.CreateSnapshot{ID: "sb-fork2.v1"}); err != nil {
		t.Fatal(err)
	}
	if v, _ := h.n.VM("sb-fork2"); v.Snapshot != "sb-fork2.v1" {
		t.Errorf("a disk snapshot of a stopped VM is not its newest: %+v", v)
	}
	// And of a paused one, its pause again, which it is paused on now.
	h.n.Pause(ctx, "sb-fork", "sb-fork.@1")
	if s, err := h.n.Snapshot(ctx, "sb-fork", nodeapi.CreateSnapshot{ID: "sb-fork.v1", Memory: true}); err != nil || !s.Memory || s.Parent != "sb-fork.@1" {
		t.Fatalf("memory snapshot of a paused VM: %+v %v", s, err)
	}
	if v, _ := h.n.VM("sb-fork"); v.State != nodeapi.VMPaused || v.Snapshot != "sb-fork.v1" {
		t.Errorf("snapshot of a paused VM left it %+v", v)
	}
	if _, err := h.n.Resume(ctx, "sb-fork", nodeapi.Resume{Wake: nodeapi.Wake{}}); err != nil || h.vmm.last().read(0) != "booted sb-src" {
		t.Errorf("resume from the paused VM's snapshot: %v", err)
	}

	// Restore: memory replaces the running VM; disk-only boots fresh.
	restores := len(h.vmm.restores)
	if v, err := h.n.Restore(ctx, "sb-src", nodeapi.Restore{Snapshot: "sb-src.v1"}); err != nil || v.State != nodeapi.VMRunning || v.Snapshot != "sb-src.v1" {
		t.Fatalf("restore: %+v %v", v, err)
	}
	if len(h.vmm.restores) != restores+1 || h.vmm.last().read(9*pageSize) != "" {
		t.Error("a memory restore did not restore memory")
	}
	big := createReq("sb-big")
	big.Resources = resources(512)
	h.n.Create(ctx, big)
	if _, err := h.n.Restore(ctx, "sb-big", nodeapi.Restore{Snapshot: "sb-src.v1"}); code(err) != api.CodeInvalid {
		t.Errorf("restoring 1024 MiB of memory into a 512 MiB VM: %v", err)
	}
	if v, err := h.n.Restore(ctx, "sb-big", nodeapi.Restore{Snapshot: "sb-src.v2"}); err != nil || v.Snapshot != "sb-src.v2" {
		t.Errorf("disk restore into another shape: %+v %v", v, err)
	}

	// A snapshot outlives its VM, and goes when deleted.
	if err := h.n.Delete("sb-src"); err != nil {
		t.Fatal(err)
	}
	if !fileExists(sdir) {
		t.Error("deleting a VM deleted its snapshot")
	}
	h.n.WaitUploads(ctx)
	if err := h.n.DeleteSnapshot(ctx, "sb-src.v1"); err != nil {
		t.Fatal(err)
	}
	if fileExists(sdir) || h.stored("sb-src.v1") {
		t.Error("a deleted snapshot is still here or stored")
	}
	if err := h.n.DeleteSnapshot(ctx, "sb-src.v1"); code(err) != api.CodeNotFound {
		t.Errorf("deleting a deleted snapshot: %v", err)
	}
	if err := h.n.DeleteSnapshot(ctx, "../vms"); code(err) != api.CodeNotFound {
		t.Errorf("a path as a snapshot id: %v", err)
	}
}

// A sandbox moves: paused on one node, it is created under its own ID from
// that snapshot on another, its memory served from the store and its disk
// fetched; paused there it uploads a diff; and it moves back.
func TestMove(t *testing.T) {
	sh := newShared(t)
	geek, basil := newNode(t, "geek", sh, nil), newNode(t, "basil", sh, nil)
	ctx := context.Background()
	if _, err := geek.n.Create(ctx, createReq("sb-mv")); err != nil {
		t.Fatal(err)
	}
	geek.vmm.last().write(10*pageSize, "a process on geek")
	writeDisk(t, geek, "sb-mv", 8192, "written on geek")
	if _, err := geek.n.Pause(ctx, "sb-mv", "sb-mv.@1"); err != nil {
		t.Fatal(err)
	}
	if !geek.stored("sb-mv.@1") {
		t.Fatal("not stored")
	}
	_, spec, _ := geek.n.localSnap("sb-mv.@1")
	if spec.DiskOver != overBase || len(spec.DiskDirty) != 1 || spec.DiskDirty[0].Off != 8192 {
		t.Errorf("the first pause's disk layer: over %s, %v", spec.DiskOver, spec.DiskDirty)
	}

	v, err := basil.n.Create(ctx, nodeapi.CreateVM{ID: "sb-mv", From: &nodeapi.FromSnapshot{Snapshot: "sb-mv.@1"}, Wake: nodeapi.Wake{Sandbox: "sb-mv"}})
	if err != nil {
		t.Fatal(err)
	}
	mb := basil.vmm.last()
	if v.State != nodeapi.VMRunning || v.Snapshot != "sb-mv.@1" || v.Template.Image != createReq("").Template.Image || !mb.lazy || basil.vmm.pagers.count() != 1 {
		t.Fatalf("moved: %+v, lazy %v", v, mb.lazy)
	}
	if mb.read(0) != "booted sb-mv" || mb.read(10*pageSize) != "a process on geek" {
		t.Errorf("the moved VM's memory: %q %q", mb.read(0), mb.read(10*pageSize))
	}
	if got, tmpl := readDisk(t, basil, "sb-mv", 8192, 15), readDisk(t, basil, "sb-mv", 32<<10, 20); got != "written on geek" || tmpl != "more of the template" {
		t.Errorf("the moved VM's disk: %q %q", got, tmpl)
	}
	n := basil.n
	n.mu.Lock()
	serving := n.vms["sb-mv"].serving
	n.mu.Unlock()
	if serving != "sb-mv.@1" {
		t.Errorf("serving %q", serving)
	}
	if err := basil.n.DeleteSnapshot(ctx, "sb-mv.@1"); code(err) != api.CodeConflict {
		t.Errorf("deleting the snapshot a VM's memory is served from: %v", err)
	}

	mb.write(11*pageSize, "a process on basil")
	writeDisk(t, basil, "sb-mv", 12288, "written on basil")
	basil.vmm.pagers.zeroed = []snapstore.Extent{{Off: 20 * pageSize, Len: 2 * pageSize}}
	if _, err := basil.n.Pause(ctx, "sb-mv", "sb-mv.@2"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(mb.calls(), "snapshot diff") {
		t.Errorf("the pause on basil did %v", mb.calls())
	}
	_, spec2, _ := basil.n.localSnap("sb-mv.@2")
	if spec2.MemFull || spec2.MemOver != overParent || len(spec2.MemZero) != 1 || spec2.DiskOver != overParent ||
		len(spec2.DiskDirty) != 1 || spec2.DiskDirty[0].Off != 12288 {
		t.Errorf("the diff's spec: %+v", spec2)
	}
	if !basil.stored("sb-mv.@2") {
		t.Fatal("the diff was not stored")
	}
	s2 := basil.snap("sb-mv.@2")
	l1, l2 := geek.ownLayer("sb-mv.@1"), basil.ownLayer("sb-mv.@2")
	if s2.Parent != "sb-mv.@1" || !slices.Equal(s2.Layers, []string{l1, l2}) {
		t.Errorf("the diff: %+v, want layers %s %s", s2, l1, l2)
	}
	// The diff's memory layer holds the one page written on basil: not the
	// memory, nor the pages basil only read (which KVM logs dirty too).
	sn, err := basil.store.GetSnapshot(ctx, "sb-mv.@2")
	if err != nil {
		t.Fatal(err)
	}
	if ld := sn.Memory.Layers[header.LayerID(spec2.Layer)]; ld.Size != pageSize {
		t.Errorf("the diff's memory layer is %d bytes", ld.Size)
	}

	// Back on geek: loam-api deletes the old VM there and creates it from @2.
	if err := geek.n.Delete("sb-mv"); err != nil {
		t.Fatal(err)
	}
	if _, err := geek.n.Create(ctx, nodeapi.CreateVM{ID: "sb-mv", From: &nodeapi.FromSnapshot{Snapshot: "sb-mv.@2"}, Wake: nodeapi.Wake{Sandbox: "sb-mv"}}); err != nil {
		t.Fatal(err)
	}
	mg := geek.vmm.last()
	if mg.read(0) != "booted sb-mv" || mg.read(10*pageSize) != "a process on geek" || mg.read(11*pageSize) != "a process on basil" {
		t.Errorf("back on geek, memory: %q %q %q", mg.read(0), mg.read(10*pageSize), mg.read(11*pageSize))
	}
	if a, b := readDisk(t, geek, "sb-mv", 8192, 15), readDisk(t, geek, "sb-mv", 12288, 16); a != "written on geek" || b != "written on basil" {
		t.Errorf("back on geek, disk: %q %q", a, b)
	}

	// A checkpoint on geek forks on basil.
	if _, err := geek.n.Snapshot(ctx, "sb-mv", nodeapi.CreateSnapshot{ID: "sb-mv.v1", Memory: true}); err != nil {
		t.Fatal(err)
	}
	if !geek.stored("sb-mv.v1") {
		t.Fatal("checkpoint not stored")
	}
	if _, err := basil.n.Create(ctx, nodeapi.CreateVM{ID: "sb-fork", From: &nodeapi.FromSnapshot{Snapshot: "sb-mv.v1"}, Wake: nodeapi.Wake{Sandbox: "sb-fork"}}); err != nil {
		t.Fatal(err)
	}
	if mf := basil.vmm.last(); mf.read(11*pageSize) != "a process on basil" || !mf.lazy {
		t.Errorf("the fork on basil: %q", mf.read(11*pageSize))
	}

	// A paused VM whose memory came lazily resumes here once its snapshot is stored.
	if _, err := basil.n.Pause(ctx, "sb-fork", "sb-fork.@1"); err != nil {
		t.Fatal(err)
	}
	if _, err := basil.n.Resume(ctx, "sb-fork", nodeapi.Resume{Wake: nodeapi.Wake{Sandbox: "sb-fork"}}); err != nil {
		t.Fatal(err)
	}
	if mf := basil.vmm.last(); mf.read(11*pageSize) != "a process on basil" || !mf.lazy {
		t.Errorf("resumed on basil: %q", mf.read(11*pageSize))
	}
}

// On SIGTERM the nodelet stops being ready, pauses what runs into snapshots
// of its own naming, tells the event stream, and stores them all; the
// sandboxes then wake on another node.
func TestDrain(t *testing.T) {
	sh := newShared(t)
	h, other := newNode(t, "geek", sh, nil), newNode(t, "basil", sh, nil)
	ctx := context.Background()
	for _, id := range []string{"sb-a", "sb-b", "sb-c"} {
		if _, err := h.n.Create(ctx, createReq(id)); err != nil {
			t.Fatal(err)
		}
	}
	h.n.Stop(ctx, "sb-c", "sb-c.@1")
	h.n.WaitUploads(ctx)
	srv := httptest.NewServer(h.n.Handler())
	t.Cleanup(srv.Close)
	events := subscribe(t, srv.URL)
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	h.n.mu.Lock()
	h.n.draining = true // as Drain begins
	h.n.mu.Unlock()
	if _, err := h.n.Create(ctx, createReq("sb-d")); code(err) != codeNotReady {
		t.Errorf("create while draining: %v", err)
	}
	if _, err := h.n.Resume(ctx, "sb-c", nodeapi.Resume{Wake: nodeapi.Wake{}}); code(err) != codeNotReady {
		t.Errorf("resume while draining: %v", err)
	}
	if h.n.Node().Ready {
		t.Error("a draining node says it is ready")
	}
	h.n.Drain(dctx)

	drained := regexp.MustCompile(`^sb-[ab]\.@drain-\d+$`)
	paused, stored := map[string]string{}, map[string]bool{}
	heardNode := false
	for !heardNode || len(paused) < 2 || len(stored) < 2 {
		select {
		case e := <-events:
			switch e.kind {
			case nodeapi.EventNode:
				var nd nodeapi.Node
				json.Unmarshal([]byte(e.data), &nd)
				if nd.Ready || len(paused) > 0 {
					t.Errorf("node event %+v after %d pauses (want not ready, first)", nd, len(paused))
				}
				heardNode = true
			case nodeapi.EventVM:
				var v nodeapi.VM
				json.Unmarshal([]byte(e.data), &v)
				if v.State != nodeapi.VMPaused || !drained.MatchString(v.Snapshot) || stored[v.Snapshot] {
					t.Errorf("drain event %+v (vm before its snapshot's)", v)
				}
				paused[v.ID] = v.Snapshot
			case nodeapi.EventSnapshot:
				var s nodeapi.Snapshot
				json.Unmarshal([]byte(e.data), &s)
				if !s.Stored || !drained.MatchString(s.ID) {
					t.Errorf("snapshot event %+v", s)
				}
				stored[s.ID] = true
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("events: paused %v, stored %v", paused, stored)
		}
	}
	for _, id := range []string{"sb-a", "sb-b"} {
		if !h.stored(paused[id]) {
			t.Errorf("%s's drain snapshot is not stored", id)
		}
		if _, err := other.n.Create(ctx, nodeapi.CreateVM{ID: id, From: &nodeapi.FromSnapshot{Snapshot: paused[id]}, Wake: nodeapi.Wake{Sandbox: id}}); err != nil {
			t.Errorf("%s on the other node: %v", id, err)
		} else if got := other.vmm.last().read(0); got != "booted "+id {
			t.Errorf("%s on the other node: memory %q", id, got)
		}
	}
	h.reopen()
	for id, want := range map[string]string{"sb-a": nodeapi.VMPaused, "sb-b": nodeapi.VMPaused, "sb-c": nodeapi.VMStopped} {
		if v, _ := h.n.VM(id); v.State != want {
			t.Errorf("%s is %s after a drain and a restart, want %s", id, v.State, want)
		}
	}
}

type sse struct{ kind, data string }

// subscribe reads a nodelet's event stream.
func subscribe(t *testing.T, url string) <-chan sse {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content type %q", ct)
	}
	ch := make(chan sse, 64)
	go func() {
		br := bufio.NewReader(resp.Body)
		var e sse
		for {
			l, err := br.ReadString('\n')
			if err != nil {
				return
			}
			l = strings.TrimSpace(l)
			switch {
			case strings.HasPrefix(l, "event: "):
				e.kind = strings.TrimPrefix(l, "event: ")
			case strings.HasPrefix(l, "data: "):
				e.data = strings.TrimPrefix(l, "data: ")
				ch <- e
				e = sse{}
			}
		}
	}()
	return ch
}

// Deleting snapshots: refused while a VM here is paused on it, through any
// node for a stored one, an upload in flight cancelled first, and the layers
// left for loam-api.
func TestDeleteSnapshot(t *testing.T) {
	sh := newShared(t)
	h, other := newNode(t, "geek", sh, nil), newNode(t, "basil", sh, nil)
	ctx := context.Background()
	h.n.Create(ctx, createReq("sb-d"))
	h.n.Snapshot(ctx, "sb-d", nodeapi.CreateSnapshot{ID: "sb-d.v1"})
	h.n.Pause(ctx, "sb-d", "sb-d.@1")
	h.n.WaitUploads(ctx)
	if err := h.n.DeleteSnapshot(ctx, "sb-d.@1"); code(err) != api.CodeConflict {
		t.Errorf("deleting the snapshot a VM is paused on: %v", err)
	}
	before, _ := h.store.ListLayers(ctx)
	layersBefore := len(before)
	// Through another node, which has no copy: the store's goes.
	if err := other.n.DeleteSnapshot(ctx, "sb-d.v1"); err != nil {
		t.Fatal(err)
	}
	if h.stored("sb-d.v1") {
		t.Error("deleted through another node, still stored")
	}
	if _, err := other.n.GetSnapshot(ctx, "sb-d.v1"); code(err) != api.CodeNotFound {
		t.Errorf("get a deleted snapshot elsewhere: %v", err)
	}
	if err := other.n.DeleteSnapshot(ctx, "sb-d.v1"); code(err) != api.CodeNotFound {
		t.Errorf("delete it again there: %v", err)
	}
	// The local copy here goes at the next reconcile.
	if err := h.n.ReconcileStored(ctx); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(h.data, "snapshots", "sb-d.v1")) {
		t.Error("the local copy of a snapshot the store lost is still here")
	}
	if !fileExists(filepath.Join(h.data, "snapshots", "sb-d.@1")) {
		t.Error("reconcile deleted a stored snapshot")
	}
	// Layers are loam-api's to delete: every one stays.
	if layers, _ := h.store.ListLayers(ctx); len(layers) != layersBefore {
		t.Errorf("%d layers in the store, want the %d there were", len(layers), layersBefore)
	}

	// An upload in flight is cancelled, and nothing of the snapshot is written after.
	h.n.Resume(ctx, "sb-d", nodeapi.Resume{Wake: nodeapi.Wake{}})
	sh.gate.hold()
	if _, err := h.n.Snapshot(ctx, "sb-d", nodeapi.CreateSnapshot{ID: "sb-d.v2"}); err != nil {
		t.Fatal(err)
	}
	if err := h.n.DeleteSnapshot(ctx, "sb-d.v2"); err != nil {
		t.Fatal(err)
	}
	sh.gate.release()
	if h.stored("sb-d.v2") || fileExists(filepath.Join(h.data, "snapshots", "sb-d.v2")) {
		t.Error("a snapshot deleted mid-upload was stored or kept")
	}

	// One whose child's upload waits for it: its layers are stored for the
	// child, the snapshot itself is not.
	sh.gate.hold()
	if _, err := h.n.Snapshot(ctx, "sb-d", nodeapi.CreateSnapshot{ID: "sb-d.v3", Memory: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.n.Pause(ctx, "sb-d", "sb-d.@2"); err != nil {
		t.Fatal(err)
	}
	if err := h.n.DeleteSnapshot(ctx, "sb-d.v3"); err != nil {
		t.Fatal(err)
	}
	sh.gate.release()
	if !h.stored("sb-d.@2") || h.stored("sb-d.v3") {
		t.Error("the child of a deleted snapshot was not stored, or the deleted one was")
	}
	if s := h.snap("sb-d.@2"); s.Parent != "sb-d.v3" || len(s.Layers) < 2 {
		t.Errorf("the child: %+v", s)
	}
	if _, err := other.n.Create(ctx, nodeapi.CreateVM{ID: "sb-d", From: &nodeapi.FromSnapshot{Snapshot: "sb-d.@2"}}); err != nil {
		t.Errorf("the child restores elsewhere: %v", err)
	}

	// The parent deleted first, then the child that kept its upload going
	// (pause, wake, disk checkpoint, delete the sandbox): both uploads stop
	// before the child's delete answers, so no layer of either is written
	// after loam-api collects them.
	if _, err := h.n.Resume(ctx, "sb-d", nodeapi.Resume{Wake: nodeapi.Wake{}}); err != nil {
		t.Fatal(err)
	}
	sh.gate.hold()
	if _, err := h.n.Snapshot(ctx, "sb-d", nodeapi.CreateSnapshot{ID: "sb-d.v4", Memory: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.n.Snapshot(ctx, "sb-d", nodeapi.CreateSnapshot{ID: "sb-d.v5"}); err != nil {
		t.Fatal(err)
	}
	if p := h.snap("sb-d.v5").Parent; p != "sb-d.v4" {
		t.Fatalf("v5's parent is %q", p)
	}
	own := []string{h.ownLayer("sb-d.v4"), h.ownLayer("sb-d.v5")}
	if err := h.n.DeleteSnapshot(ctx, "sb-d.v4"); err != nil {
		t.Fatal(err)
	}
	if err := h.n.DeleteSnapshot(ctx, "sb-d.v5"); err != nil {
		t.Fatal(err)
	}
	sh.gate.release()
	if err := h.n.WaitUploads(ctx); err != nil {
		t.Fatal(err)
	}
	layers, _ := h.store.ListLayers(ctx)
	for _, l := range layers {
		if slices.Contains(own, l.String()) {
			t.Errorf("layer %s of a deleted snapshot was written", l)
		}
	}
}

// Layers are deleted through any node.
func TestDeleteLayer(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.n.Create(ctx, createReq("sb-l"))
	h.n.Pause(ctx, "sb-l", "sb-l.@1")
	h.stored("sb-l.@1")
	srv := httptest.NewServer(h.n.Handler())
	t.Cleanup(srv.Close)
	cl := &nodeapi.Client{Addr: strings.TrimPrefix(srv.URL, "http://"), Token: "tok"}
	layer := h.snap("sb-l.@1").Layers[0]
	if err := cl.DeleteLayer(ctx, layer); err != nil {
		t.Fatal(err)
	}
	if layers, _ := h.store.ListLayers(ctx); len(layers) != 0 {
		t.Errorf("layers left: %v", layers)
	}
	for _, l := range []string{layer, "not-a-layer", header.NewLayerID().String()} {
		if err := cl.DeleteLayer(ctx, l); !nodeapi.IsCode(err, api.CodeNotFound) {
			t.Errorf("delete layer %s: %v", l, err)
		}
	}
}

// What phase 1 left on a node: a VM paused into its own directory and a
// checkpoint without layers. The VM resumes; the checkpoint is stored whole.
func TestPhase1Leftovers(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.n.Create(ctx, createReq("sb-old"))
	h.n.Snapshot(ctx, "sb-old", nodeapi.CreateSnapshot{ID: "sb-old.v1", Memory: true})
	h.n.Pause(ctx, "sb-old", "sb-old.@1")
	h.n.WaitUploads(ctx)
	h.n.Shutdown()
	// Turn it into phase 1's layout.
	vdir, sdir := filepath.Join(h.data, "vms", "sb-old"), filepath.Join(h.data, "snapshots")
	os.Rename(filepath.Join(sdir, "sb-old.@1", snapState), filepath.Join(vdir, pauseState))
	os.Rename(filepath.Join(sdir, "sb-old.@1", snapMem), filepath.Join(vdir, pauseMem))
	os.RemoveAll(filepath.Join(sdir, "sb-old.@1"))
	var r record
	b, _ := os.ReadFile(filepath.Join(vdir, vmFile))
	json.Unmarshal(b, &r)
	r.Snapshot, r.MemParent, r.BaseImage = "", false, ""
	saveRecord(vdir, r)
	for _, f := range []string{specFile, memHeader, diskHeader} {
		os.Remove(filepath.Join(sdir, "sb-old.v1", f))
	}
	var s nodeapi.Snapshot
	b, _ = os.ReadFile(filepath.Join(sdir, "sb-old.v1", snapFile))
	json.Unmarshal(b, &s)
	h.store.DeleteSnapshot(ctx, "sb-old.v1")
	os.WriteFile(filepath.Join(sdir, "sb-old.v1", snapFile), []byte(`{"id":"sb-old.v1","vm":"sb-old","memory":true,"template":{"name":"base","image":"x"},"resources":{"cpus":2,"memory_mib":1024,"disk_gib":10},"size_bytes":1,"created_at":"2026-10-01T00:00:00Z"}`), 0o644)

	h.reopen()
	if !h.stored("sb-old.v1") {
		t.Fatal("a phase 1 snapshot was not stored")
	}
	if s := h.snap("sb-old.v1"); len(s.Layers) != 1 || s.Parent != "" || !s.Stored {
		t.Errorf("the phase 1 snapshot: %+v", s)
	}
	if v, _ := h.n.VM("sb-old"); v.State != nodeapi.VMPaused || v.Snapshot != "" {
		t.Fatalf("the phase 1 pause: %+v", v)
	}
	if _, err := h.n.Resume(ctx, "sb-old", nodeapi.Resume{Wake: nodeapi.Wake{}}); err != nil {
		t.Fatal(err)
	}
	if hasPause(vdir) || h.vmm.last().read(0) != "booted sb-old" {
		t.Error("the phase 1 pause did not resume as one")
	}
	if _, err := h.n.Pause(ctx, "sb-old", "sb-old.@2"); err != nil {
		t.Fatal(err)
	}
	_, spec, _ := h.n.localSnap("sb-old.@2")
	if spec.MemOver != overZero || spec.DiskOver != overZero || len(spec.DiskDirty) == 0 {
		t.Errorf("a phase 1 VM's first pause: %+v", spec)
	}
	if !h.stored("sb-old.@2") {
		t.Error("not stored")
	}
	other := newNode(t, "basil", h.shared, nil)
	if _, err := other.n.Create(ctx, nodeapi.CreateVM{ID: "sb-old", From: &nodeapi.FromSnapshot{Snapshot: "sb-old.@2"}}); err != nil {
		t.Fatal(err)
	}
	if got := readDisk(t, other, "sb-old", 0, 9); got != "rootfs of" {
		t.Errorf("the phase 1 VM's disk elsewhere: %q", got)
	}
}

// An agent without /internal/wake is not one that will answer it: the boot
// fails at once rather than at the timeout.
func TestNoWakeRoute(t *testing.T) {
	h := newHarness(t, nil)
	h.vmm.agent.noWake = true
	if _, err := h.n.Create(context.Background(), createReq("sb-x")); code(err) != api.CodeSandboxUnavailable || !strings.Contains(err.Error(), "no such route") {
		t.Fatalf("a VM whose agent has no /internal/wake: %v", err)
	}
}

// A VM that ends on its own is reported stopped, on the event stream too.
func TestExitIsAnEvent(t *testing.T) {
	h := newHarness(t, nil)
	srv := httptest.NewServer(h.n.Handler())
	t.Cleanup(srv.Close)
	events := subscribe(t, srv.URL)
	if _, err := h.n.Create(context.Background(), createReq("sb-crash")); err != nil {
		t.Fatal(err)
	}
	h.vmm.last().exit() // the guest powers off
	select {
	case e := <-events:
		if e.kind != "vm" || !strings.Contains(e.data, `"id":"sb-crash"`) || !strings.Contains(e.data, `"state":"stopped"`) {
			t.Errorf("event %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	if v, _ := h.n.VM("sb-crash"); v.State != nodeapi.VMStopped || h.n.Node().MemoryMiBAllocated != 0 {
		t.Errorf("after the exit: %+v", v)
	}
}

// The node protocol over HTTP, through nodeapi's client.
func TestHTTP(t *testing.T) {
	h := newHarness(t, nil)
	srv := httptest.NewServer(h.n.Handler())
	t.Cleanup(srv.Close)
	ctx := context.Background()
	cl := &nodeapi.Client{Addr: strings.TrimPrefix(srv.URL, "http://"), Token: "tok"}

	bad := &nodeapi.Client{Addr: cl.Addr, Token: "nope"}
	if _, err := bad.Node(ctx); !nodeapi.IsCode(err, api.CodeUnauthorized) {
		t.Errorf("wrong token: %v", err)
	}
	if _, err := cl.VM(ctx, "sb-none"); !nodeapi.IsCode(err, api.CodeNotFound) {
		t.Errorf("missing VM: %v", err)
	}
	v, err := cl.Create(ctx, createReq("sb-http"))
	if err != nil || v.State != nodeapi.VMRunning {
		t.Fatalf("create: %+v %v", v, err)
	}
	if _, err := cl.Create(ctx, createReq("sb-http")); !nodeapi.IsCode(err, api.CodeAlreadyExists) {
		t.Errorf("duplicate create: %v", err)
	}
	bigReq := createReq("sb-huge")
	bigReq.Resources = resources(4000)
	_, err = cl.Create(ctx, bigReq)
	var ne *nodeapi.Err
	if !errors.As(err, &ne) || ne.Code != api.CodeCapacity || ne.Status != http.StatusServiceUnavailable {
		t.Errorf("over capacity: %v", err)
	}
	if vms, err := cl.VMs(ctx); err != nil || len(vms) != 1 {
		t.Errorf("list: %+v %v", vms, err)
	}

	// The agent stream reaches the guest's agent.
	s, err := cl.DialAgent(ctx, "sb-http")
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Transport: nodeapi.AgentTransport(func(context.Context) (net.Conn, error) { return s, nil })}
	resp, err := hc.Get("http://agent/hello")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello from the guest" {
		t.Errorf("through the agent stream: %q", body)
	}
	if a, err := cl.Activity(ctx, "sb-http"); err != nil || a.IdleMS != 1234 {
		t.Errorf("activity: %+v %v", a, err)
	}
	allowGitHub := nodeapi.Policy{Egress: []api.Rule{{Domain: "github.com", Action: api.RuleAllow}}}
	if err := cl.SetPolicy(ctx, "sb-http", allowGitHub); err != nil {
		t.Errorf("policy: %v", err)
	}
	if err := cl.SetPolicy(ctx, "sb-http", nodeapi.Policy{Egress: []api.Rule{{Domain: "1.2.3.4", Action: api.RuleAllow}}}); !nodeapi.IsCode(err, api.CodeInvalid) {
		t.Errorf("a policy that doesn't compile: %v", err)
	}

	if _, err := cl.Snapshot(ctx, "sb-http", nodeapi.CreateSnapshot{ID: "sb-http.v1", Memory: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Pause(ctx, "sb-http"); !nodeapi.IsCode(err, api.CodeInvalid) {
		t.Errorf("a pause without a snapshot name: %v", err)
	}
	if v, err := cl.PauseInto(ctx, "sb-http", "sb-http.@1"); err != nil || v.State != nodeapi.VMPaused || v.Snapshot != "sb-http.@1" {
		t.Fatalf("pause: %+v %v", v, err)
	}
	if _, err := cl.DialAgent(ctx, "sb-http"); !nodeapi.IsCode(err, api.CodeConflict) {
		t.Errorf("agent stream to a paused VM: %v", err)
	}
	if err := cl.SetPolicy(ctx, "sb-http", allowGitHub); !nodeapi.IsCode(err, api.CodeConflict) {
		t.Errorf("the policy of a paused VM: %v", err)
	}
	if v, err := cl.Resume(ctx, "sb-http", nodeapi.Resume{Wake: nodeapi.Wake{Sandbox: "sb-http"}}); err != nil || v.State != nodeapi.VMRunning {
		t.Fatalf("resume: %+v %v", v, err)
	}
	if v, err := cl.Restore(ctx, "sb-http", nodeapi.Restore{Snapshot: "sb-http.v1"}); err != nil || v.State != nodeapi.VMRunning {
		t.Fatalf("restore: %+v %v", v, err)
	}
	if v, err := cl.StopInto(ctx, "sb-http", "sb-http.@2"); err != nil || v.State != nodeapi.VMStopped || v.Snapshot != "sb-http.@2" {
		t.Fatalf("stop: %+v %v", v, err)
	}
	if snaps, err := cl.Snapshots(ctx); err != nil || len(snaps) != 3 {
		t.Errorf("snapshots: %+v %v", snaps, err)
	}
	h.n.WaitUploads(ctx)
	if s, err := cl.GetSnapshot(ctx, "sb-http.@2"); err != nil || !s.Stored || s.Parent != "sb-http.v1" {
		t.Errorf("get snapshot: %+v %v", s, err)
	}
	if _, err := cl.GetSnapshot(ctx, "sb-http.@9"); !nodeapi.IsCode(err, api.CodeNotFound) {
		t.Errorf("get a missing snapshot: %v", err)
	}
	if err := cl.DeleteSnapshot(ctx, "sb-http.v1"); err != nil {
		t.Error(err)
	}
	if err := cl.Delete(ctx, "sb-http"); err != nil {
		t.Error(err)
	}

	// A body field the nodelet doesn't know is an option it can't honour.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/vms", strings.NewReader(`{"id":"sb-z","gpu":true}`))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown field: %s", resp.Status)
	}
}

// The host channel forwards a guest's request to loam-api as its own VM's,
// whatever the guest claims.
func TestHostChannel(t *testing.T) {
	got := make(chan *http.Request, 1)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r
		io.WriteString(w, `{"id":"sb-guest"}`)
	}))
	defer api.Close()
	h := newHarness(t, func(o *Options) { o.APIURL = api.URL })
	if _, err := h.n.Create(context.Background(), createReq("sb-guest")); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(h.data, "vms", "sb-guest", firecracker.VsockSock+"_1025")
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	req, _ := http.NewRequest(http.MethodGet, "http://host/self?x=1", nil)
	req.Header.Set(nodeapi.HeaderSandbox, "sb-other")
	req.Header.Set("Authorization", "Bearer guest-made-this-up")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != `{"id":"sb-guest"}` {
		t.Fatalf("answer %s %q", resp.Status, body)
	}
	r := <-got
	if r.URL.Path != "/internal/guest/self" || r.URL.RawQuery != "x=1" {
		t.Errorf("forwarded to %s", r.URL)
	}
	if r.Header.Values(nodeapi.HeaderSandbox)[0] != "sb-guest" || len(r.Header.Values(nodeapi.HeaderSandbox)) != 1 {
		t.Errorf("X-Loam-Sandbox %v", r.Header.Values(nodeapi.HeaderSandbox))
	}
	if r.Header.Get("Authorization") != "Bearer tok" {
		t.Errorf("Authorization %q", r.Header.Get("Authorization"))
	}

	// Paused, the VM has no channel.
	h.n.Pause(context.Background(), "sb-guest", "sb-guest.@1")
	if fileExists(sock) {
		t.Error("a paused VM's host channel is still listening")
	}
}

// A failed boot leaves nothing behind.
func TestFailedCreate(t *testing.T) {
	h := newHarness(t, nil)
	h.vmm.failBoot = errors.New("no kvm")
	if _, err := h.n.Create(context.Background(), createReq("sb-fail")); code(err) != api.CodeSandboxUnavailable {
		t.Fatalf("failed boot: %v", err)
	}
	if fileExists(filepath.Join(h.data, "vms", "sb-fail")) || len(h.n.VMs()) != 0 || h.n.Node().MemoryMiBAllocated != 0 {
		t.Error("a failed create left something behind")
	}
	for _, id := range []string{"SB-UPPER", "../x", "", strings.Repeat("a", 65)} {
		if _, err := h.n.Create(context.Background(), createReq(id)); code(err) != api.CodeInvalid {
			t.Errorf("id %q: %v", id, err)
		}
	}
	req := createReq("sb-img")
	req.Template.Image = "missing"
	if _, err := h.n.Create(context.Background(), req); code(err) != api.CodeInvalid {
		t.Errorf("missing image: %v", err)
	}
}

type fakeNet struct {
	taps map[int]bool
	// policies is each VM's policy, by network index, and failPolicy makes
	// SetPolicy fail.
	policies   map[int]*network.Policy
	failPolicy error
}

func (f *fakeNet) AddTap(_ context.Context, i int) (string, error) {
	f.taps[i] = true
	return network.Tap(i), nil
}
func (f *fakeNet) RemoveTap(i int) { delete(f.taps, i) }

func (f *fakeNet) SetPolicy(_ context.Context, i int, _ string, p *network.Policy) error {
	if f.failPolicy != nil {
		return f.failPolicy
	}
	if f.policies == nil {
		f.policies = map[int]*network.Policy{}
	}
	f.policies[i] = p
	return nil
}
func (f *fakeNet) ClearPolicy(i int) { delete(f.policies, i) }

// With a network, every VM gets its own address: on the kernel command line
// at a fresh boot, and in every wake, which is what moves a fork off its
// source's address.
func TestNetworkInWake(t *testing.T) {
	h := newHarness(t, nil)
	nw := &fakeNet{taps: map[int]bool{}}
	h.n.Shutdown()
	n, err := New(h.opts, h.vmm, nw, fakeImages{dir: filepath.Join(h.data, "images")})
	if err != nil {
		t.Fatal(err)
	}
	h.n = n
	ctx := context.Background()
	v, err := n.Create(ctx, createReq("sb-src"))
	if err != nil {
		t.Fatal(err)
	}
	if v.IP != "10.240.0.2" || !nw.taps[2] {
		t.Errorf("first VM: %+v, taps %v", v, nw.taps)
	}
	args := h.vmm.boots[0].BootArgs
	for _, want := range []string{"loam.ip=10.240.0.2/16", "loam.gw=10.240.0.1", "loam.dns=10.240.0.1"} {
		if !slices.Contains(args, want) {
			t.Errorf("boot args %v lack %s", args, want)
		}
	}
	if h.vmm.boots[0].Tap != "lt2" || h.vmm.boots[0].MAC != "06:00:0a:f0:00:02" {
		t.Errorf("NIC %s %s", h.vmm.boots[0].Tap, h.vmm.boots[0].MAC)
	}
	n.Snapshot(ctx, "sb-src", nodeapi.CreateSnapshot{ID: "sb-src.v1", Memory: true})
	fork, err := n.Create(ctx, nodeapi.CreateVM{ID: "sb-fork", From: &nodeapi.FromSnapshot{Snapshot: "sb-src.v1"}})
	if err != nil {
		t.Fatal(err)
	}
	w := h.vmm.agent.lastWake()
	if fork.IP != "10.240.0.3" || w.Network == nil || w.Network.IP != "10.240.0.3/16" || w.Network.MAC != "06:00:0a:f0:00:03" ||
		w.Network.Gateway != "10.240.0.1" || !slices.Equal(w.Network.DNS, []string{"10.240.0.1"}) {
		t.Errorf("fork %+v woke with %+v", fork, w.Network)
	}
	n.Pause(ctx, "sb-fork", "sb-fork.@1")
	if nw.taps[3] {
		t.Error("a paused VM kept its tap")
	}
	n.Delete("sb-src")
	if v, _ := n.Create(ctx, createReq("sb-next")); v.IP != "10.240.0.2" {
		t.Errorf("a deleted VM's address was not reused: %s", v.IP)
	}
}

// A VM's policy is in place before its guest runs, held only while it runs,
// and replaced on a running VM; one that can't be put in place keeps the VM
// from running, or stops a running one.
func TestPolicy(t *testing.T) {
	h := newHarness(t, nil)
	nw := &fakeNet{taps: map[int]bool{}}
	h.n.Shutdown()
	n, err := New(h.opts, h.vmm, nw, fakeImages{dir: filepath.Join(h.data, "images")})
	if err != nil {
		t.Fatal(err)
	}
	h.n = n
	ctx := context.Background()
	restricted := nodeapi.Policy{Egress: []api.Rule{{Domain: "github.com", Action: api.RuleAllow}}}

	req := createReq("sb-p")
	req.Policy = restricted
	if _, err := n.Create(ctx, req); err != nil {
		t.Fatal(err)
	}
	if p := nw.policies[2]; !p.Restricted() || !p.Allows("github.com", 443) || p.Allows("example.com", 443) {
		t.Errorf("created with %+v", p)
	}
	if err := n.SetPolicy(ctx, "sb-p", nodeapi.Policy{}); err != nil || nw.policies[2].Restricted() {
		t.Errorf("replaced: %v %+v", err, nw.policies[2])
	}
	if _, err := n.Pause(ctx, "sb-p", "sb-p.@1"); err != nil {
		t.Fatal(err)
	}
	if _, held := nw.policies[2]; held {
		t.Error("a paused VM's policy is still held")
	}
	if err := n.SetPolicy(ctx, "sb-p", restricted); code(err) != api.CodeConflict {
		t.Errorf("the policy of a paused VM: %v", err)
	}
	if _, err := n.Resume(ctx, "sb-p", nodeapi.Resume{Policy: restricted}); err != nil || !nw.policies[2].Restricted() {
		t.Errorf("resumed: %v %+v", err, nw.policies[2])
	}

	// Refused before anything is made.
	boots := len(h.vmm.boots)
	bad := createReq("sb-bad")
	bad.Policy = nodeapi.Policy{Egress: []api.Rule{{Domain: "github.com", Action: "maybe"}}}
	if _, err := n.Create(ctx, bad); code(err) != api.CodeInvalid {
		t.Errorf("a policy that doesn't compile: %v", err)
	}
	secret := createReq("sb-secret")
	secret.Policy = nodeapi.Policy{Secrets: []nodeapi.PolicySecret{{Name: "TOKEN", Value: "v", Hosts: []string{"api.github.com"}, Placeholder: "__loam_token__"}}}
	if _, err := n.Create(ctx, secret); code(err) != api.CodeUnsupported {
		t.Errorf("secrets without the broker CA: %v", err)
	}

	// Fail closed: a policy that can't be put in place leaves no VM running.
	nw.failPolicy = errors.New("nft: no")
	if _, err := n.Create(ctx, createReq("sb-fail")); err == nil {
		t.Error("created without its policy")
	}
	if len(h.vmm.boots) != boots {
		t.Errorf("%d VMs booted without their policy", len(h.vmm.boots)-boots)
	}
	if _, err := n.VM("sb-fail"); code(err) != api.CodeNotFound {
		t.Errorf("a VM whose policy failed: %v", err)
	}
	if err := n.SetPolicy(ctx, "sb-p", nodeapi.Policy{}); err == nil {
		t.Error("a policy that failed was accepted")
	}
	if v, _ := n.VM("sb-p"); v.State != nodeapi.VMStopped {
		t.Errorf("a running VM whose new policy failed is %s", v.State)
	}
}

// A nodelet with the broker CA says so, runs VMs with secrets, and hands the
// root to every wake.
func TestBrokerCA(t *testing.T) {
	h := newHarness(t, nil)
	h.n.Shutdown()
	h.opts.BrokerCA = "-----BEGIN CERTIFICATE-----\n…\n-----END CERTIFICATE-----\n"
	n, err := New(h.opts, h.vmm, &fakeNet{taps: map[int]bool{}}, fakeImages{dir: filepath.Join(h.data, "images")})
	if err != nil {
		t.Fatal(err)
	}
	h.n = n
	if !n.Node().Broker {
		t.Error("node without broker")
	}
	req := createReq("sb-s")
	req.Policy = nodeapi.Policy{Secrets: []nodeapi.PolicySecret{{Name: "TOKEN", Value: "v", Hosts: []string{"api.github.com"}, Placeholder: "__loam_token__"}}}
	if _, err := n.Create(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if w := h.vmm.agent.lastWake(); w.CA != h.opts.BrokerCA {
		t.Errorf("wake CA %q", w.CA)
	}
}

// The disk's changed ranges, from the extent maps and from the contents.
func TestChangedExtents(t *testing.T) {
	ext := func(l, p, n int64) extent { return extent{logical: l, physical: p, length: n} }
	got := compareMaps(
		[]extent{ext(0, 1000, 8192), ext(8192, 50000, 4096), ext(16384, 3000, 4096)},
		[]extent{ext(0, 1000, 12288), ext(16384, 3000, 8192)})
	want := []snapstore.Extent{{Off: 8192, Len: 4096}, {Off: 20480, Len: 4096}}
	if !slices.Equal(got, want) {
		t.Errorf("compareMaps: %v, want %v", got, want)
	}

	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	base := make([]byte, 64<<10)
	copy(base[4096:], "same")
	copy(base[20480:], "will go")
	os.WriteFile(b, base, 0o644)
	mod := append([]byte(nil), base...)
	copy(mod[40960:], "new")
	clear(mod[20480:24576])
	os.WriteFile(a, mod, 0o644)
	changed, err := changedExtents(a, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []snapstore.Extent{{Off: 20480, Len: 4096}, {Off: 40960, Len: 4096}}; !slices.Equal(changed, want) {
		t.Errorf("changed %v, want %v", changed, want)
	}
	if got := subtractExtents([]snapstore.Extent{{Off: 0, Len: 100}, {Off: 200, Len: 100}}, []snapstore.Extent{{Off: 50, Len: 200}}); !slices.Equal(got, []snapstore.Extent{{Off: 0, Len: 50}, {Off: 250, Len: 50}}) {
		t.Errorf("subtract: %v", got)
	}
}

// A sandbox made from an image has no template name (docs/NODE.md): the disk
// is built from the image alone, and a snapshot of it forks under the same
// nameless template.
func TestImageWithoutTemplate(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	req := createReq("sb-img")
	req.Template = nodeapi.Template{Image: "ghcr.io/arugula-salad/app@sha256:" + strings.Repeat("a", 64)}
	v, err := h.n.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if v.Template != req.Template {
		t.Errorf("VM template %+v, want %+v", v.Template, req.Template)
	}
	if got := readDisk(t, h, "sb-img", 0, 9); got != "rootfs of" {
		t.Errorf("the disk is not the image's: %q", got)
	}
	if _, err := h.n.Snapshot(ctx, "sb-img", nodeapi.CreateSnapshot{ID: "sb-img.v1"}); err != nil {
		t.Fatal(err)
	}
	fork := nodeapi.CreateVM{ID: "sb-imgfork", Template: req.Template, Resources: req.Resources,
		From: &nodeapi.FromSnapshot{Snapshot: "sb-img.v1"}, Wake: nodeapi.Wake{Sandbox: "sb-imgfork"}}
	if v, err := h.n.Create(ctx, fork); err != nil || v.Template != req.Template {
		t.Fatalf("fork: %+v %v", v, err)
	}
	fork.ID, fork.Template.Name = "sb-imgfork2", "base"
	if _, err := h.n.Create(ctx, fork); code(err) != api.CodeInvalid {
		t.Errorf("a fork naming a template the snapshot doesn't have: %v", err)
	}
}
