//go:build kvm

package nodelet

// Phase 2 on real Firecracker: two nodelets ("geek" and "basil", in one
// process and one network namespace) sharing a snapshot store. A sandbox
// paused on one wakes on the other with its processes, lazily; a pause there
// uploads a diff; it moves back; a checkpoint forks across; a drain moves a
// running one; and deleting the snapshots and layers empties the store. It
// measures pauses, uploads and resumes as it goes. Environment, besides
// TestKVM's:
//
//	LOAM_KVM_DATA, LOAM_KVM_DATA_B  the two nodes' data directories (reflinks if they have them)
//	LOAM_KVM_STORE_URL              the store (default a directory under LOAM_KVM_DATA);
//	                                s3://... with AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY for Garage
//
// The two nodelets share the bridge, so the test never runs two VMs with one
// network index at once (each node numbers its own from 2).

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/internal/snapstore"
	"github.com/arugula-salad/loam/internal/snapstore/header"
	"github.com/arugula-salad/loam/pkg/api"
)

type kvmGuest struct {
	t  *testing.T
	cl *nodeapi.Client
}

func (g kvmGuest) agent(id, method, path, body string) (int, string) {
	g.t.Helper()
	hc := &http.Client{Transport: nodeapi.AgentTransport(func(ctx context.Context) (net.Conn, error) { return g.cl.DialAgent(ctx, id) }), Timeout: 60 * time.Second}
	req, _ := http.NewRequest(method, "http://agent"+path, strings.NewReader(body))
	resp, err := hc.Do(req)
	if err != nil {
		g.t.Fatalf("%s %s on %s: %v", method, path, id, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (g kvmGuest) run(id string, cmd ...string) (int, string) {
	g.t.Helper()
	body, _ := json.Marshal(api.ExecRun{Cmd: cmd, User: "root", TimeoutS: 50})
	code, b := g.agent(id, "POST", "/exec/run", string(body))
	var res api.ExecResult
	if code != 200 || json.Unmarshal([]byte(b), &res) != nil {
		g.t.Fatalf("exec %v on %s: %d %s", cmd, id, code, b)
	}
	return res.ExitCode, strings.TrimSpace(string(res.Stdout) + string(res.Stderr))
}

// mark starts a process whose state lives only in memory: a counter in
// /dev/shm, and its PID. check says it is the same process, still counting.
func (g kvmGuest) mark(id string) {
	g.t.Helper()
	if code, out := g.run(id, "sh", "-c", `setsid sh -c 'i=0; while :; do i=$((i+1)); echo $i > /dev/shm/n; sleep 0.2; done' >/dev/null 2>&1 </dev/null & echo $! > /dev/shm/pid; cat /dev/shm/pid`); code != 0 {
		g.t.Fatalf("mark %s: %s", id, out)
	}
}

func (g kvmGuest) check(id, where string) {
	g.t.Helper()
	code, out := g.run(id, "sh", "-c", `p=$(cat /dev/shm/pid) && kill -0 $p && a=$(cat /dev/shm/n) && sleep 1 && b=$(cat /dev/shm/n) && echo "$p $a $b"`)
	f := strings.Fields(out)
	if code != 0 || len(f) != 3 {
		g.t.Errorf("%s: the marked process is gone (%d %q)", where, code, out)
		return
	}
	a, _ := strconv.Atoi(f[1])
	b, _ := strconv.Atoi(f[2])
	if b <= a {
		g.t.Errorf("%s: the marked process stopped counting (%s)", where, out)
		return
	}
	g.t.Logf("%s: pid %s alive, counter %d -> %d", where, f[0], a, b)
}

func (g kvmGuest) sum(id, path string) string {
	g.t.Helper()
	code, out := g.run(id, "sh", "-c", "sha256sum "+path+" | cut -c1-16")
	if code != 0 {
		g.t.Fatalf("sum %s on %s: %s", path, id, out)
	}
	return out
}

// waitStored waits for snapshot id's upload on n and returns how long it
// took from now and the bytes its own layer put in the store.
func waitStored(t *testing.T, n *Nodelet, st *snapstore.Store, id string) (time.Duration, int64) {
	t.Helper()
	start := time.Now()
	if u := n.inflight(id); u != nil {
		select {
		case <-u.done:
		case <-time.After(5 * time.Minute):
			t.Fatalf("snapshot %s: upload still running", id)
		}
		if u.err != nil {
			t.Fatalf("snapshot %s: upload failed: %v", id, u.err)
		}
	}
	took := time.Since(start)
	sn, err := st.GetSnapshot(context.Background(), id)
	if err != nil {
		t.Fatalf("snapshot %s: %v", id, err)
	}
	_, spec, _ := n.localSnap(id)
	var bytes int64
	for _, h := range []*header.Header{sn.Memory, sn.Disk} {
		if h == nil {
			continue
		}
		if ld, ok := h.Layers[spec.Layer]; ok {
			if ld.Frames != nil {
				bytes += ld.Frames.CompressedSize()
			} else {
				bytes += ld.Size
			}
		}
	}
	return took, bytes
}

func TestKVMStore(t *testing.T) {
	kvmSetup(t)
	ctx := context.Background()
	dataA, dataB := env("LOAM_KVM_DATA", ""), env("LOAM_KVM_DATA_B", "")
	if dataA == "" {
		dataA = t.TempDir()
	}
	if dataB == "" {
		dataB = t.TempDir()
	}
	storeURL := env("LOAM_KVM_STORE_URL", "file://"+filepath.Join(dataA, "shared-store"))
	t.Logf("store %s", strings.SplitN(storeURL, "?", 2)[0])
	A, clA, stA := kvmNode(t, "geek", filepath.Join(dataA, "geek"), storeURL, "")
	B, clB, stB := kvmNode(t, "basil", filepath.Join(dataB, "basil"), storeURL, "")
	ga, gb := kvmGuest{t, clA}, kvmGuest{t, clB}
	res := api.Resources{CPUs: 2, MemoryMiB: 1024, DiskGiB: 4}
	wake := func(id string) nodeapi.Wake {
		return nodeapi.Wake{Sandbox: id, Name: id, Hostname: id, User: "root"}
	}
	var report []string
	measure := func(what string, d time.Duration, extra string) {
		line := fmt.Sprintf("%-44s %8v %s", what, d.Round(time.Millisecond), extra)
		report = append(report, line)
		t.Log(line)
	}
	defer func() { t.Log("measurements:\n" + strings.Join(report, "\n")) }()
	timed := func(what string, f func() (nodeapi.VM, error)) nodeapi.VM {
		t.Helper()
		start := time.Now()
		v, err := f()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		measure(what, time.Since(start), v.State+" "+v.Snapshot)
		return v
	}

	// 1. A sandbox on geek with a process, 256 MiB of memory in use, and a disk change.
	timed("create on geek (pull + build + boot)", func() (nodeapi.VM, error) {
		return clA.Create(ctx, nodeapi.CreateVM{ID: "sb-mv", Template: nodeapi.Template{Name: "base", Image: kvm.image}, Resources: res, Wake: wake("sb-mv")})
	})
	ga.mark("sb-mv")
	if code, out := ga.run("sb-mv", "sh", "-c", "head -c 256M /dev/urandom > /dev/shm/blob && echo geek > /root/where && sync"); code != 0 {
		t.Fatal(out)
	}
	blob := ga.sum("sb-mv", "/dev/shm/blob")

	timed("pause on geek (full)", func() (nodeapi.VM, error) { return clA.PauseInto(ctx, "sb-mv", "sb-mv.@1") })
	took, bytes := waitStored(t, A, stA, "sb-mv.@1")
	measure("  upload of the full snapshot", took, fmt.Sprintf("%d MiB", bytes>>20))

	// 2. It moves to basil: memory lazily from the store, disk materialized.
	start := time.Now()
	v := timed("move to basil (create from @1, cold cache)", func() (nodeapi.VM, error) {
		return clB.Create(ctx, nodeapi.CreateVM{ID: "sb-mv", From: &nodeapi.FromSnapshot{Snapshot: "sb-mv.@1"}, Wake: wake("sb-mv")})
	})
	t0 := time.Now()
	gb.run("sb-mv", "true")
	measure("  first exec after the move", time.Since(t0), "")
	measure("  move, until a command ran", time.Since(start), "")
	if v.Snapshot != "sb-mv.@1" {
		t.Errorf("moved VM: %+v", v)
	}
	gb.check("sb-mv", "on basil after the move")
	if got := gb.sum("sb-mv", "/dev/shm/blob"); got != blob {
		t.Errorf("256 MiB of guest memory after the move: %s, want %s", got, blob)
	}
	t0 = time.Now()
	gb.sum("sb-mv", "/dev/shm/blob")
	measure("  read 256 MiB of guest memory again", time.Since(t0), "")
	if _, out := gb.run("sb-mv", "cat", "/root/where"); out != "geek" {
		t.Errorf("the disk on basil: %q", out)
	}
	templates := func(data string) string {
		m, _ := filepath.Glob(filepath.Join(data, "images", "*-r1.ext4"))
		if len(m) != 1 {
			return fmt.Sprint(m)
		}
		f, _ := os.Open(m[0])
		defer f.Close()
		return fmt.Sprint(fileDigest(f))
	}
	if a, b := templates(filepath.Join(dataA, "geek")), templates(filepath.Join(dataB, "basil")); a != b {
		t.Errorf("the two nodes built different template disks: %s %s", a, b)
	} else {
		t.Logf("both nodes built the template disk byte for byte the same (%s)", a[:16])
	}

	// 3. A pause on basil uploads only what changed there.
	if code, out := gb.run("sb-mv", "sh", "-c", "head -c 32M /dev/urandom > /dev/shm/basil && echo basil > /root/where && sync"); code != 0 {
		t.Fatal(out)
	}
	timed("pause on basil (diff)", func() (nodeapi.VM, error) { return clB.PauseInto(ctx, "sb-mv", "sb-mv.@2") })
	took, bytes = waitStored(t, B, stB, "sb-mv.@2")
	measure("  upload of the diff", took, fmt.Sprintf("%d MiB", bytes>>20))
	if s, _ := clB.GetSnapshot(ctx, "sb-mv.@2"); s.Parent != "sb-mv.@1" || len(s.Layers) < 2 {
		t.Errorf("the diff: %+v", s)
	}

	// 4. And back to geek (loam-api deletes the VM there first).
	if err := clA.Delete(ctx, "sb-mv"); err != nil {
		t.Fatal(err)
	}
	timed("move back to geek (create from @2)", func() (nodeapi.VM, error) {
		return clA.Create(ctx, nodeapi.CreateVM{ID: "sb-mv", From: &nodeapi.FromSnapshot{Snapshot: "sb-mv.@2"}, Wake: wake("sb-mv")})
	})
	ga.check("sb-mv", "on geek after moving back")
	if got := ga.sum("sb-mv", "/dev/shm/blob"); got != blob {
		t.Errorf("guest memory back on geek: %s", got)
	}
	if _, out := ga.run("sb-mv", "sh", "-c", "cat /root/where; ls -l /dev/shm/basil | wc -l"); out != "basil\n1" {
		t.Errorf("back on geek: %q", out)
	}

	// 5. Same-node resumes: from a whole local memory file, and from the
	// store through a pager (a VM whose memory came lazily).
	timed("create sb-same on geek", func() (nodeapi.VM, error) {
		return clA.Create(ctx, nodeapi.CreateVM{ID: "sb-same", Template: nodeapi.Template{Name: "base", Image: kvm.image}, Resources: res, Wake: wake("sb-same")})
	})
	ga.mark("sb-same")
	timed("pause sb-same (full)", func() (nodeapi.VM, error) { return clA.PauseInto(ctx, "sb-same", "sb-same.@1") })
	timed("same-node resume (local memory file)", func() (nodeapi.VM, error) { return clA.Resume(ctx, "sb-same", nodeapi.Resume{Wake: wake("sb-same")}) })
	ga.check("sb-same", "same-node resume")
	ga.run("sb-same", "sh", "-c", "head -c 16M /dev/urandom > /dev/shm/more")
	timed("pause sb-same again (diff over a whole file)", func() (nodeapi.VM, error) { return clA.PauseInto(ctx, "sb-same", "sb-same.@2") })
	took, bytes = waitStored(t, A, stA, "sb-same.@2")
	measure("  upload of the diff", took, fmt.Sprintf("%d MiB", bytes>>20))
	timed("same-node resume (merged memory file)", func() (nodeapi.VM, error) { return clA.Resume(ctx, "sb-same", nodeapi.Resume{Wake: wake("sb-same")}) })
	ga.check("sb-same", "same-node resume after a diff")
	if err := clA.Delete(ctx, "sb-same"); err != nil {
		t.Fatal(err)
	}
	timed("pause sb-mv on geek (diff, lazy memory)", func() (nodeapi.VM, error) { return clA.PauseInto(ctx, "sb-mv", "sb-mv.@3") })
	timed("same-node resume (through the store)", func() (nodeapi.VM, error) { return clA.Resume(ctx, "sb-mv", nodeapi.Resume{Wake: wake("sb-mv")}) })
	ga.check("sb-mv", "same-node resume of a lazily restored VM")

	// 6. A checkpoint on geek forks on basil.
	if _, err := clA.Snapshot(ctx, "sb-mv", nodeapi.CreateSnapshot{ID: "sb-mv.v1", Memory: true}); err != nil {
		t.Fatal(err)
	}
	waitStored(t, A, stA, "sb-mv.v1")
	// (basil's stale sb-mv stays a moment: it holds basil's first network
	// index, which geek's running sb-mv also has.)
	timed("fork on basil from geek's checkpoint", func() (nodeapi.VM, error) {
		return clB.Create(ctx, nodeapi.CreateVM{ID: "sb-fork", From: &nodeapi.FromSnapshot{Snapshot: "sb-mv.v1"}, Wake: wake("sb-fork")})
	})
	gb.check("sb-fork", "the fork on basil")

	// 7. geek drains while sb-mv runs: paused, stored, told; it wakes on basil.
	events := subscribe(t, "http://"+clA.Addr)
	dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	start = time.Now()
	A.Drain(dctx)
	cancel()
	measure("drain of geek (pause + upload)", time.Since(start), "")
	var drained string
	for drained == "" {
		select {
		case e := <-events:
			if e.kind == nodeapi.EventSnapshot && strings.Contains(e.data, `"sb-mv.@drain-`) {
				var s nodeapi.Snapshot
				json.Unmarshal([]byte(e.data), &s)
				drained = s.ID
			}
		case <-time.After(30 * time.Second):
			t.Fatal("no stored drain snapshot on the event stream")
		}
	}
	if err := clB.Delete(ctx, "sb-mv"); err != nil { // basil's stale copy: it moved
		t.Fatal(err)
	}
	timed("wake the drained sandbox on basil", func() (nodeapi.VM, error) {
		return clB.Create(ctx, nodeapi.CreateVM{ID: "sb-mv", From: &nodeapi.FromSnapshot{Snapshot: drained}, Wake: wake("sb-mv")})
	})
	gb.check("sb-mv", "on basil after geek drained")

	// 8. Garbage: the VMs, then every snapshot and layer, through basil.
	for _, id := range []string{"sb-mv", "sb-fork"} {
		if err := clB.Delete(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := stB.ListSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	layers := map[string]bool{}
	for _, id := range ids {
		s, err := clB.GetSnapshot(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range s.Layers {
			layers[l] = true
		}
		if err := clB.DeleteSnapshot(ctx, id); err != nil {
			t.Errorf("delete snapshot %s: %v", id, err)
		}
	}
	for l := range layers {
		if err := clB.DeleteLayer(ctx, l); err != nil && !nodeapi.IsCode(err, api.CodeNotFound) {
			t.Errorf("delete layer %s: %v", l, err)
		}
	}
	left, _ := stB.ListSnapshots(ctx)
	leftLayers, _ := stB.ListLayers(ctx)
	if len(left) != 0 || len(leftLayers) != 0 {
		t.Errorf("the store still holds %d snapshots and %d layers", len(left), len(leftLayers))
	}
	t.Logf("deleted %d snapshots and %d layers; the store is empty", len(ids), len(layers))
	B.ReconcileStored(ctx)
}

func fileDigest(r io.Reader) string {
	h := sha256.New()
	io.Copy(h, r)
	return fmt.Sprintf("%x", h.Sum(nil))
}
