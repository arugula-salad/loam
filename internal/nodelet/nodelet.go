// Package nodelet runs the VMs of one node (docs/NODE.md): it boots them from
// template images or snapshots, pauses them to memory snapshots and resumes
// them, stops and deletes them, snapshots and restores them, and carries
// loam-api's streams to their agents and their agents' requests to loam-api.
// Every snapshot it takes goes to the snapshot store (snapshots.go), and a VM
// can start from any stored snapshot, on any node. It knows nothing of
// projects, records or lifecycle rules: loam-api owns those, and the
// nodelet's own state (which VMs and snapshots exist, in what state) is a
// cache of what it runs, kept in its data directory so that a restarted
// nodelet picks up its paused and stopped VMs.
package nodelet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/internal/nodelet/firecracker"
	"github.com/arugula-salad/loam/internal/nodelet/network"
	"github.com/arugula-salad/loam/internal/snapstore"
	"github.com/arugula-salad/loam/internal/snapstore/header"
	"github.com/arugula-salad/loam/pkg/api"
)

// Locking. Each VM has a transition lock (vm.mu), held across slow work: a
// boot, a pause, a snapshot, a guest round trip. Nodelet.mu is a leaf, held
// only for bookkeeping: the VM table, every VM's record (vm.rec), the memory
// reservations. snapMu orders reading a snapshot's local files (a create or
// restore from it) against deleting it. The order is vm.mu, then snapMu, then
// Nodelet.mu; at most one vm.mu is held at a time. upMu (the uploads in
// flight) and each upload's mu are leaves too; an upload never takes the
// others.

// Options configures a Nodelet.
type Options struct {
	// Data is the data directory: VMs, snapshots and the image cache.
	Data string
	// MemoryMiB is what the nodelet may hand to running VMs (docs/NODE.md, "The node").
	MemoryMiB int
	NodeName  string
	Version   string
	// Token is the node token, which every request must carry and which the
	// host channel presents to loam-api.
	Token string
	// APIURL is loam-api, for the host channel. Empty answers the guest 503.
	APIURL string
	// DevAllowCopy lets the data directory be a volume without reflinks, where
	// every clone is a full sparse copy. Never in production.
	DevAllowCopy bool
	// Store is the snapshot store (docs/STORE.md), shared by every node.
	Store *snapstore.Store
	// Pagers serve the memory of VMs restored from the store.
	Pagers Pagers
	// BrokerCA is the broker CA's root certificate (PEM), which every wake
	// carries; "" for a nodelet without it, which refuses a VM with secrets.
	BrokerCA string
	Log      *slog.Logger
}

// Net is the VMs' network; nil boots VMs without a NIC (tests), which have
// no network for a policy to restrict.
type Net interface {
	AddTap(ctx context.Context, index int) (string, error)
	RemoveTap(index int)
	// SetPolicy puts a VM's policy in place: before its guest runs, or over
	// the one it has. An error leaves the one it had.
	SetPolicy(ctx context.Context, index int, id string, p *network.Policy) error
	// ClearPolicy forgets it, once the guest no longer runs.
	ClearPolicy(index int)
}

// Images builds template disks (ImageCache).
type Images interface {
	// Disk returns the template disk of image at gib GiB, and image pinned by
	// its digest, which builds the same disk on any node.
	Disk(ctx context.Context, image string, gib int) (path, pinned string, err error)
}

// Files in a VM's directory, besides Firecracker's own.
const (
	vmFile = "vm.json"
	// parentDisk is a clone of the disk of the VM's newest snapshot, or of its
	// template, which the next snapshot's disk layer is the difference from
	// (extents.go). parentMem is a clone of that snapshot's whole memory, when
	// it has one here, which a diff snapshot's pages go over to make the next
	// whole memory.
	parentDisk = "parent.ext4"
	parentMem  = "parent.mem"
	// stateFile is a snapshot's state, written here when the snapshot came
	// from the store; uffdSock is where Firecracker meets the VM's pager.
	stateFile = "restore.vmstate"
	uffdSock  = "uffd.sock"
	// A pause made before phase 2 kept its memory here, not as a snapshot.
	pauseState = "pause.vmstate"
	pauseMem   = "pause.mem"
)

// record is a VM as the nodelet persists it, in its directory's vm.json.
type record struct {
	ID        string           `json:"id"`
	Template  nodeapi.Template `json:"template"`
	Resources api.Resources    `json:"resources"`
	State     string           `json:"state"`
	Error     string           `json:"error,omitempty"`
	NetIndex  int              `json:"net_index"`
	BootedAt  *time.Time       `json:"booted_at,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
	// Snapshot is the VM's newest snapshot (docs/NODE.md, VM.snapshot): the
	// one its next snapshot is a diff against. A paused VM is paused on it.
	Snapshot string `json:"snapshot,omitempty"`
	// MemParent says the VM's memory continues Snapshot's: it was restored or
	// paused from it, so its next memory snapshot can be a diff. A fresh boot
	// has none.
	MemParent bool `json:"mem_parent,omitempty"`
	// BaseImage is the template image, pinned by digest, the VM's disk
	// descends from; "" for a VM made before phase 2, whose disk layers go
	// over nothing (its template was built differently on every node).
	BaseImage string `json:"base_image,omitempty"`
}

type vm struct {
	id, dir string

	mu sync.Mutex // transitions; see "Locking"
	// Guarded by mu.
	m     Machine // nil unless running
	guest *guestChan
	tap   bool
	pager Pager
	// zeroed is what the pager reported zeroed at the VM's last snapshot in
	// this run: those pages are already recorded, and the guest may have
	// written some since (a pager can't see writes to a page it installed).
	zeroed []snapstore.Extent

	// Guarded by Nodelet.mu.
	rec      record
	ready    bool   // false while it is being created: not listed, not found
	gone     bool   // deleted; a caller that waited for mu finds it gone
	reserved int    // MiB held against the node's memory
	serving  string // the snapshot its pager serves memory from

	// The agent streams open into it, which a memory snapshot cuts (see
	// cutStreams). Guarded by streamMu, a leaf: a stream closes without vm.mu.
	streamMu sync.Mutex
	streams  map[*agentStream]struct{}
}

// agentStream is one stream to a VM's agent, which leaves its VM's set when
// it closes.
type agentStream struct {
	net.Conn
	v    *vm
	once sync.Once
}

func (c *agentStream) Close() error {
	c.once.Do(func() {
		c.v.streamMu.Lock()
		delete(c.v.streams, c)
		c.v.streamMu.Unlock()
	})
	return c.Conn.Close()
}

// CloseWrite half-closes the stream as nodeapi.Splice does a bare one, which
// the wrapping would otherwise hide from it.
func (c *agentStream) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.SetReadDeadline(time.Now())
}

// cutStreams closes every agent stream open into v. A memory snapshot ends
// them in the guest: Firecracker resets the guest's vsock transport when it
// saves the device, and the guest drops its connections without a word to
// the host, so the host's ends stay open on streams nothing will ever answer
// or close. A pause gets this for free when it kills the process; a snapshot
// the VM runs on from has to do it, or every exec socket on it hangs.
func (v *vm) cutStreams() int {
	v.streamMu.Lock()
	cut := make([]*agentStream, 0, len(v.streams))
	for c := range v.streams {
		cut = append(cut, c)
	}
	v.streamMu.Unlock()
	for _, c := range cut {
		c.Close()
	}
	return len(cut)
}

// Nodelet runs the VMs of one node.
type Nodelet struct {
	opts   Options
	log    *slog.Logger
	vmm    VMM
	net    Net
	images Images
	clone  cloner
	events *bus
	store  *snapstore.Store
	pagers Pagers

	snapMu sync.RWMutex

	upMu    sync.Mutex
	uploads map[string]*upload
	upWG    sync.WaitGroup
	upSlots chan struct{}

	// reflinks: the data volume has them, so a clone shares blocks with its
	// source and disks are compared by their extent maps (extents.go).
	reflinks bool

	mu        sync.Mutex
	vms       map[string]*vm
	allocated int
	draining  bool
}

// New opens the data directory: it checks the volume, reaps the Firecracker
// processes a previous nodelet left running, loads the VMs it knew and
// resumes the uploads of its snapshots not stored yet. A VM that was running
// is stopped now (its memory died with that nodelet); a paused one is still
// paused.
func New(opts Options, vmm VMM, nw Net, images Images) (*Nodelet, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Store == nil {
		return nil, errors.New("a snapshot store is required")
	}
	n := &Nodelet{opts: opts, log: opts.Log, vmm: vmm, net: nw, images: images, store: opts.Store, pagers: opts.Pagers,
		clone: cloner{allowCopy: opts.DevAllowCopy}, events: newBus(), vms: map[string]*vm{},
		uploads: map[string]*upload{}, upSlots: make(chan struct{}, uploadWorkers)}
	for _, d := range []string{n.vmsDir(), n.snapsDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	n.reflinks = ProbeReflink(n.vmsDir())
	if !n.reflinks {
		if !opts.DevAllowCopy {
			return nil, fmt.Errorf("--data %s does not support reflinks (it must be XFS with reflink=1, or btrfs)", opts.Data)
		}
		n.log.Warn("the data directory has no reflinks: every disk clone is a full copy (--dev-allow-copy)")
	}
	if err := n.load(); err != nil {
		return nil, err
	}
	if err := n.loadSnapshots(); err != nil {
		return nil, err
	}
	return n, nil
}

func (n *Nodelet) vmsDir() string   { return filepath.Join(n.opts.Data, "vms") }
func (n *Nodelet) snapsDir() string { return filepath.Join(n.opts.Data, "snapshots") }

func (n *Nodelet) load() error {
	entries, err := os.ReadDir(n.vmsDir())
	if err != nil {
		return err
	}
	for _, e := range entries {
		dir := filepath.Join(n.vmsDir(), e.Name())
		var r record
		b, err := os.ReadFile(filepath.Join(dir, vmFile))
		if err == nil {
			err = json.Unmarshal(b, &r)
		}
		if err != nil || r.ID != e.Name() {
			n.log.Warn("ignoring a VM directory without a readable record", "dir", dir, "err", err)
			continue
		}
		n.vmm.ReapOrphan(dir)
		for _, f := range []string{pauseState + ".tmp", pauseMem + ".tmp", pauseMem + ".sparse", firecracker.DiskFile + ".tmp",
			vmFile + ".tmp", parentDisk + ".tmp", parentMem + ".tmp", uffdSock} {
			os.Remove(filepath.Join(dir, f))
		}
		was := r.State
		switch {
		case r.State == nodeapi.VMRunning:
			r.State, r.BootedAt, r.MemParent = nodeapi.VMStopped, nil, false
		case r.State == nodeapi.VMPaused && r.Snapshot == "" && !hasPause(dir):
			r.State, r.BootedAt = nodeapi.VMStopped, nil
		}
		if r.State != was {
			n.log.Info("VM did not survive the nodelet's restart", "vm", r.ID, "was", was, "now", r.State)
			if err := saveRecord(dir, r); err != nil {
				return err
			}
		}
		n.vms[r.ID] = &vm{id: r.ID, dir: dir, rec: r, ready: true}
	}
	return nil
}

func saveRecord(dir string, r record) error {
	b, _ := json.MarshalIndent(r, "", "  ")
	return writeFileAtomic(filepath.Join(dir, vmFile), b)
}

// hasPause says whether dir holds a pause made before phase 2.
func hasPause(dir string) bool {
	for _, f := range []string{pauseState, pauseMem} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return false
		}
	}
	return true
}

func discardPause(dir string) {
	for _, f := range []string{pauseState, pauseMem, pauseState + ".tmp", pauseMem + ".tmp", pauseMem + ".sparse"} {
		os.Remove(filepath.Join(dir, f))
	}
}

// IDs. A VM's is its sandbox's (sb-…); a snapshot's is loam-api's
// (<sandbox id>.<checkpoint id>, <sandbox id>.@<n>) or a drain's
// (<sandbox id>.@drain-<unix ms>), as the store allows (snapstore.ValidSnapshotID).
const maxIDLen = 64

var vmIDRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func validVMID(id string) bool   { return len(id) <= maxIDLen && vmIDRE.MatchString(id) }
func validSnapID(id string) bool { return snapstore.ValidSnapshotID(id) == nil }

// Resource bounds a VM may ask for. Firecracker boots at most 32 vCPUs.
const (
	maxCPUs     = 32
	minMemMiB   = 128
	maxDiskGiB  = 4096
	bootTimeout = 60 * time.Second
	wakeTimeout = 20 * time.Second
	// restoreTimeout bounds a start from a snapshot, which may first fetch its
	// disk layers from the store and wait for its own upload.
	restoreTimeout = 5 * time.Minute
)

func (n *Nodelet) validResources(r api.Resources) error {
	switch {
	case r.CPUs < 1 || r.CPUs > maxCPUs:
		return invalid("resources.cpus must be 1 to %d", maxCPUs)
	case r.MemoryMiB < minMemMiB:
		return invalid("resources.memory_mib must be at least %d", minMemMiB)
	case r.MemoryMiB > n.opts.MemoryMiB:
		return invalid("resources.memory_mib %d is more than this node hands to VMs (%d)", r.MemoryMiB, n.opts.MemoryMiB)
	case r.DiskGiB < 1 || r.DiskGiB > maxDiskGiB:
		return invalid("resources.disk_gib must be 1 to %d", maxDiskGiB)
	}
	return nil
}

// view is v as the node protocol shows it. The caller holds n.mu.
func (n *Nodelet) view(v *vm) nodeapi.VM {
	r := v.rec
	out := nodeapi.VM{ID: r.ID, State: r.State, Error: r.Error, Template: r.Template, Resources: r.Resources, BootedAt: r.BootedAt, Snapshot: r.Snapshot}
	if n.net != nil && r.NetIndex > 0 {
		out.IP = network.Addr(r.NetIndex).String()
	}
	return out
}

func (n *Nodelet) viewOf(v *vm) nodeapi.VM {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.view(v)
}

func (n *Nodelet) recordOf(v *vm) record {
	n.mu.Lock()
	defer n.mu.Unlock()
	return v.rec
}

// update changes v's record and persists it. The caller holds v.mu, which
// orders the writes.
func (n *Nodelet) update(v *vm, f func(*record)) error {
	n.mu.Lock()
	f(&v.rec)
	r := v.rec
	n.mu.Unlock()
	return saveRecord(v.dir, r)
}

// lock finds VM id and takes its transition lock.
func (n *Nodelet) lock(id string) (*vm, error) {
	n.mu.Lock()
	v := n.vms[id]
	n.mu.Unlock()
	if v == nil {
		return nil, notFound("VM", id)
	}
	v.mu.Lock()
	n.mu.Lock()
	ok := v.ready && !v.gone
	n.mu.Unlock()
	if !ok {
		v.mu.Unlock()
		return nil, notFound("VM", id)
	}
	return v, nil
}

// reserve claims v's memory against the node's budget; release gives it back.
// Admission reserves the ceiling: the balloon is an economy, not a boundary.
func (n *Nodelet) reserve(v *vm, mib int) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if v.reserved > 0 {
		return nil
	}
	if n.allocated+mib > n.opts.MemoryMiB {
		return capacity("this node's VMs already hold %d MiB of its %d MiB and %s needs %d MiB", n.allocated, n.opts.MemoryMiB, v.id, mib)
	}
	v.reserved = mib
	n.allocated += mib
	return nil
}

func (n *Nodelet) release(v *vm) {
	n.mu.Lock()
	n.allocated -= v.reserved
	v.reserved = 0
	n.mu.Unlock()
}

// Node describes this node.
func (n *Nodelet) Node() nodeapi.Node {
	n.mu.Lock()
	defer n.mu.Unlock()
	count := 0
	for _, v := range n.vms {
		if v.ready {
			count++
		}
	}
	return nodeapi.Node{Name: n.opts.NodeName, Version: n.opts.Version, CPUs: runtime.NumCPU(),
		MemoryMiB: n.opts.MemoryMiB, MemoryMiBAllocated: n.allocated,
		DiskGiBFree: int(freeBytes(n.opts.Data) >> 30), VMs: count, Ready: !n.draining, Broker: n.broker()}
}

// notReady refuses what would start a VM on a node that is draining.
func (n *Nodelet) notReady() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.draining {
		return errorf(http.StatusServiceUnavailable, codeNotReady, "node %s is draining", n.opts.NodeName)
	}
	return nil
}

// codeNotReady is a nodelet refusing to start a VM while it drains (docs/NODE.md, "Drain").
const codeNotReady = "not_ready"

// VMs lists the VMs, by ID.
func (n *Nodelet) VMs() []nodeapi.VM {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := []nodeapi.VM{}
	for _, v := range n.vms {
		if v.ready {
			out = append(out, n.view(v))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// VM is one VM.
func (n *Nodelet) VM(id string) (nodeapi.VM, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	v := n.vms[id]
	if v == nil || !v.ready || v.gone {
		return nodeapi.VM{}, notFound("VM", id)
	}
	return n.view(v), nil
}

// source is what a VM starts from: a snapshot's state and memory, or (no
// state) a fresh boot on its disk. A pager, when there is one, serves the
// memory and becomes the VM's.
type source struct {
	state    string
	mem      firecracker.Memory
	pager    Pager
	snapshot string // the snapshot the pager serves
}

func (s source) close() {
	if s.pager != nil {
		s.pager.Close()
	}
}

// start runs v, from src, under policy pol, and hands its agent w. The caller
// holds v.mu and v is not running. ran says whether the guest may have run,
// i.e. whether its disk may now differ from what src's memory expects. src is
// v's or closed by the time start returns.
func (n *Nodelet) start(ctx context.Context, v *vm, src source, w nodeapi.Wake, pol *network.Policy) (ran bool, err error) {
	r := n.recordOf(v)
	ok := false
	defer func() {
		if !ok {
			src.close()
		}
	}()
	if err := n.reserve(v, r.Resources.MemoryMiB); err != nil {
		return false, err
	}
	defer func() {
		if !ok {
			n.release(v)
		}
	}()
	cfg := firecracker.Config{ID: v.id, Dir: v.dir, VCPUs: r.Resources.CPUs, MemMiB: r.Resources.MemoryMiB}
	if n.net != nil {
		tap, err := n.net.AddTap(ctx, r.NetIndex)
		if err != nil {
			return false, fmt.Errorf("tap for %s: %w", v.id, err)
		}
		defer func() {
			if !ok {
				n.net.RemoveTap(r.NetIndex)
			}
		}()
		cfg.Tap, cfg.MAC = tap, network.MAC(r.NetIndex)
		// Before the guest runs (a restored one runs from its first
		// instruction on): it never runs with less than its policy.
		if err := n.net.SetPolicy(ctx, r.NetIndex, v.id, pol); err != nil {
			return false, err
		}
		defer func() {
			if !ok {
				n.net.ClearPolicy(r.NetIndex)
			}
		}()
	}
	cfg.BootArgs = n.bootArgs(r, w)
	// The guest may call the host as soon as it runs, so its channel comes first.
	guest, err := n.openGuest(v)
	if err != nil {
		return false, fmt.Errorf("host channel for %s: %w", v.id, err)
	}
	defer func() {
		if !ok {
			guest.close()
		}
	}()
	fresh := src.state == ""
	var m Machine
	if fresh {
		m, err = n.vmm.Boot(ctx, cfg)
	} else {
		m, err = n.vmm.Restore(ctx, cfg, src.state, src.mem)
	}
	if err != nil {
		return false, unavailable("start %s: %v", v.id, err)
	}
	if !fresh {
		// A snapshot is taken with the balloon squeezed over the guest's free
		// memory (Squeeze); give it back.
		if err := m.SetBalloon(ctx, 0); err != nil {
			n.log.Warn("could not let the balloon down after a restore", "vm", v.id, "err", err)
		}
	}
	timeout := wakeTimeout
	if fresh {
		timeout = bootTimeout
	}
	if err := n.wake(ctx, m, r, w, timeout); err != nil {
		m.Kill()
		return true, unavailable("the agent of %s did not answer the wake: %v%s", v.id, err, firecracker.ConsoleTail(v.dir))
	}
	ok = true
	v.m, v.guest, v.tap, v.pager, v.zeroed = m, guest, n.net != nil, src.pager, nil
	now := time.Now().UTC()
	n.update(v, func(r *record) {
		r.State, r.Error = nodeapi.VMRunning, ""
		if fresh || r.BootedAt == nil {
			r.BootedAt = &now
		}
		if fresh {
			r.MemParent = false
		}
	})
	if src.pager != nil {
		n.mu.Lock()
		v.serving = src.snapshot
		n.mu.Unlock()
		go n.watchPager(v, m, src.pager)
	}
	go n.watch(v, m)
	return true, nil
}

// bootArgs is the kernel command line for a fresh boot (docs/AGENT.md, "Kernel command line").
func (n *Nodelet) bootArgs(r record, w nodeapi.Wake) []string {
	args := []string{"loam.root=/dev/vda"}
	host := w.Hostname
	if !vmIDRE.MatchString(host) {
		host = r.ID
	}
	args = append(args, "loam.hostname="+host)
	if n.net != nil && r.NetIndex > 0 {
		gw := network.Gateway.String()
		args = append(args, "loam.ip="+network.Prefix(r.NetIndex), "loam.gw="+gw, "loam.dns="+gw)
	}
	return args
}

// wakeNet is the network the nodelet adds to every wake (docs/NODE.md).
func (n *Nodelet) wakeNet(r record) *nodeapi.WakeNetwork {
	if n.net == nil || r.NetIndex == 0 {
		return nil
	}
	gw := network.Gateway.String()
	return &nodeapi.WakeNetwork{IP: network.Prefix(r.NetIndex), Gateway: gw, DNS: []string{gw}, MAC: network.MAC(r.NetIndex)}
}

// wake hands the agent w, retrying until it answers: on a fresh boot this is
// the readiness probe. The nodelet sets the time, the network and the broker
// CA as it sends.
func (n *Nodelet) wake(ctx context.Context, m Machine, r record, w nodeapi.Wake, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	w.Network = n.wakeNet(r)
	w.CA = n.opts.BrokerCA
	for {
		w.Time = time.Now().UTC()
		cctx, ccancel := context.WithTimeout(ctx, 2*time.Second)
		err := agentCall(cctx, m, http.MethodPost, "/internal/wake", w, nil)
		ccancel()
		if err == nil {
			return nil
		}
		if errors.Is(err, errNoRoute) {
			return err // an agent that answers but has no wake is not one that will
		}
		select {
		case <-m.Exited():
			return errors.New("the VM exited")
		case <-ctx.Done():
			return fmt.Errorf("%w (last error: %v)", ctx.Err(), err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// watch reports a VM that ends on its own (the guest powered off, Firecracker
// died) as stopped. One the nodelet ended itself is no longer v.m by the time
// this gets the lock.
func (n *Nodelet) watch(v *vm, m Machine) {
	<-m.Exited()
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.m != m {
		return
	}
	n.cleanup(v)
	n.update(v, func(r *record) { r.State, r.BootedAt, r.MemParent = nodeapi.VMStopped, nil, false })
	n.log.Info("VM exited", "vm", v.id)
	n.events.publish(event{nodeapi.EventVM, n.viewOf(v)})
}

// watchPager stops a VM whose pager gave up: a guest thread waits on a page
// that will never come.
func (n *Nodelet) watchPager(v *vm, m Machine, p Pager) {
	select {
	case <-p.Failed():
	case <-m.Exited():
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.m != m || v.pager != p {
		return
	}
	n.log.Error("the VM's pager failed; the VM is stopped", "vm", v.id, "err", p.Err())
	n.kill(v)
	n.update(v, func(r *record) {
		r.State, r.BootedAt, r.MemParent = nodeapi.VMError, nil, false
		r.Error = fmt.Sprintf("its memory could not be served from the store: %v", p.Err())
	})
	n.events.publish(event{nodeapi.EventVM, n.viewOf(v)})
}

// cleanup forgets v's machine, which has exited or been killed, and gives
// back its tap, its host channel, its pager and its memory. The caller holds v.mu.
func (n *Nodelet) cleanup(v *vm) {
	v.m = nil
	v.guest.close()
	v.guest = nil
	if v.tap {
		n.net.ClearPolicy(n.recordOf(v).NetIndex)
		n.net.RemoveTap(n.recordOf(v).NetIndex)
		v.tap = false
	}
	if v.pager != nil {
		v.pager.Close()
		v.pager, v.zeroed = nil, nil
		n.mu.Lock()
		v.serving = ""
		n.mu.Unlock()
	}
	n.release(v)
}

// kill ends v's VM now. The caller holds v.mu.
func (n *Nodelet) kill(v *vm) {
	if v.m != nil {
		v.m.Kill()
		n.cleanup(v)
	}
}

// snapInfo is a snapshot a VM starts from: its local copy, or the store's.
type snapInfo struct {
	snap nodeapi.Snapshot
	// local: the local copy's layer spec. Else sn and meta are the store's.
	local bool
	spec  layerSpec
	sn    *snapstore.Snapshot
	meta  storedMeta
}

func (s snapInfo) baseImage() string {
	if s.local {
		return s.spec.BaseImage
	}
	return s.meta.BaseImage
}

// findSnap finds snapshot id here or in the store. The caller holds snapMu (read).
func (n *Nodelet) findSnap(ctx context.Context, id string) (snapInfo, error) {
	s, spec, err := n.localSnap(id)
	if err == nil {
		return snapInfo{snap: s, local: true, spec: spec}, nil
	}
	if code(err) != api.CodeNotFound {
		return snapInfo{}, err
	}
	sn, meta, err := n.storeSnapshot(ctx, id)
	if err != nil {
		return snapInfo{}, err
	}
	return snapInfo{snap: meta.Snapshot, sn: sn, meta: meta}, nil
}

// prepare puts snapshot s's disk (unless keepDisk: the VM's own is s's
// already, as a paused VM's is) and its memory's source in place for v, and
// sets v's record to descend from it. The caller holds v.mu and snapMu (read).
func (n *Nodelet) prepare(ctx context.Context, v *vm, s snapInfo, keepDisk bool) (source, error) {
	disk := filepath.Join(v.dir, firecracker.DiskFile)
	switch {
	case keepDisk:
	case s.local:
		if err := n.clone.clone(filepath.Join(n.snapDir(s.snap.ID), firecracker.DiskFile), disk); err != nil {
			return source{}, fmt.Errorf("disk from snapshot %s: %w", s.snap.ID, err)
		}
	default:
		if err := n.materialize(ctx, s, disk); err != nil {
			return source{}, fmt.Errorf("disk from stored snapshot %s: %w", s.snap.ID, err)
		}
	}
	if err := n.clone.clone(disk, filepath.Join(v.dir, parentDisk)); err != nil {
		return source{}, err
	}
	os.Remove(filepath.Join(v.dir, parentMem))
	var src source
	if s.snap.Memory {
		var err error
		if src, err = n.memSource(ctx, v, s); err != nil {
			return source{}, err
		}
	}
	err := n.update(v, func(r *record) {
		r.Snapshot, r.MemParent, r.BaseImage = s.snap.ID, false, s.baseImage()
	})
	if err != nil {
		src.close()
	}
	return src, err
}

// memSource is how v's memory comes from memory snapshot s: mapped from the
// whole memory file of a local copy that has one, or else served by a pager
// from the store, which for a local copy means once its upload is done.
func (n *Nodelet) memSource(ctx context.Context, v *vm, s snapInfo) (source, error) {
	if s.local {
		dir := n.snapDir(s.snap.ID)
		if s.spec.MemFull {
			// The VM's own clone: the next diff snapshot's pages go over it.
			pm := filepath.Join(v.dir, parentMem)
			if err := n.clone.clone(filepath.Join(dir, snapMem), pm); err != nil {
				return source{}, err
			}
			return source{state: filepath.Join(dir, snapState), mem: firecracker.Memory{File: pm}}, nil
		}
		if u := n.inflight(s.snap.ID); u != nil {
			n.log.Info("waiting for a snapshot's upload to restore its memory", "snapshot", s.snap.ID)
			select {
			case <-u.done:
			case <-ctx.Done():
				return source{}, unavailable("snapshot %s is still uploading: %v", s.snap.ID, ctx.Err())
			}
			if u.err != nil {
				return source{}, unavailable("snapshot %s holds only part of its memory here, and its upload failed: %v", s.snap.ID, u.err)
			}
		}
		if s.sn == nil {
			var err error
			if s.sn, s.meta, err = n.storeSnapshot(ctx, s.snap.ID); err != nil {
				return source{}, unavailable("snapshot %s holds only part of its memory here, and is not stored: %v", s.snap.ID, err)
			}
		}
	}
	if n.pagers == nil {
		return source{}, unavailable("this nodelet can't serve memory from the store")
	}
	state := filepath.Join(v.dir, stateFile)
	if err := writeFileAtomic(state, s.meta.VMState); err != nil {
		return source{}, err
	}
	p, err := n.pagers.Start(ctx, s.snap.ID, filepath.Join(v.dir, uffdSock))
	if err != nil {
		return source{}, unavailable("serve the memory of %s: %v", s.snap.ID, err)
	}
	return source{state: state, mem: firecracker.Memory{Uffd: p.Socket()}, pager: p, snapshot: s.snap.ID}, nil
}

// materialize makes dst the disk of stored snapshot s: a clone of its
// template's disk (built here if need be) with the snapshot's layers written
// over it, or, for a disk with no template under it, the layers alone.
func (n *Nodelet) materialize(ctx context.Context, s snapInfo, dst string) error {
	h := s.sn.Disk
	usesBase := false
	for _, m := range h.Mapping.All() {
		if m.Layer == header.Base {
			usesBase = true
			break
		}
	}
	start := time.Now()
	if usesBase {
		if s.meta.BaseImage == "" {
			return fmt.Errorf("its disk is a diff against a template it doesn't name")
		}
		base, _, err := n.images.Disk(ctx, s.meta.BaseImage, s.snap.Resources.DiskGiB)
		if err != nil {
			return err
		}
		if err := n.clone.clone(base, dst); err != nil {
			return err
		}
	} else {
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err == nil {
			err = f.Truncate(int64(h.Metadata.Size))
			f.Close()
		}
		if err != nil {
			return err
		}
	}
	before := n.store.Stats()
	if err := n.store.MaterializeDisk(ctx, h, dst); err != nil {
		return err
	}
	after := n.store.Stats()
	n.log.Info("disk fetched from the store", "snapshot", s.snap.ID, "took", time.Since(start).Round(time.Millisecond),
		"fetched_bytes", after.FetchedBytes-before.FetchedBytes, "chunk_hits", after.ChunkHits-before.ChunkHits)
	return nil
}

// Create makes and boots a VM (docs/NODE.md, POST /v1/vms).
func (n *Nodelet) Create(ctx context.Context, req nodeapi.CreateVM) (nodeapi.VM, error) {
	if !validVMID(req.ID) {
		return nodeapi.VM{}, invalid("invalid VM id %q", req.ID)
	}
	// A VM's sockets live in its directory, and sun_path holds 107 bytes.
	if l := len(filepath.Join(n.vmsDir(), req.ID, fmt.Sprintf("%s_%d", firecracker.VsockSock, HostPort))); l > 107 {
		return nodeapi.VM{}, invalid("VM id %q is too long for this node's data directory (its sockets' paths would be %d bytes of 107)", req.ID, l)
	}
	if err := n.notReady(); err != nil {
		return nodeapi.VM{}, err
	}
	pol, err := n.compilePolicy(req.Policy)
	if err != nil {
		return nodeapi.VM{}, err
	}
	// Detached from the request: a client that hangs up mid-boot must not
	// leave a half-started VM behind.
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout+bootTimeout)
	defer cancel()
	var snap *snapInfo
	if req.From != nil {
		if !validSnapID(req.From.Snapshot) {
			return nodeapi.VM{}, invalid("invalid snapshot id %q", req.From.Snapshot)
		}
		n.snapMu.RLock()
		defer n.snapMu.RUnlock()
		s, err := n.findSnap(bctx, req.From.Snapshot)
		if err != nil {
			return nodeapi.VM{}, err
		}
		// The snapshot records what it was taken of; a request that says
		// otherwise asks for something the snapshot can't be.
		if req.Template != (nodeapi.Template{}) && req.Template != s.snap.Template {
			return nodeapi.VM{}, invalid("template %+v does not match snapshot %s's %+v", req.Template, s.snap.ID, s.snap.Template)
		}
		if req.Resources != (api.Resources{}) && req.Resources != s.snap.Resources {
			return nodeapi.VM{}, invalid("resources %+v do not match snapshot %s's %+v", req.Resources, s.snap.ID, s.snap.Resources)
		}
		req.Template, req.Resources = s.snap.Template, s.snap.Resources
		snap = &s
	} else if req.Template.Image == "" {
		return nodeapi.VM{}, invalid("template.image is required without from")
	}
	if err := n.validResources(req.Resources); err != nil {
		return nodeapi.VM{}, err
	}

	n.mu.Lock()
	if n.vms[req.ID] != nil {
		n.mu.Unlock()
		return nodeapi.VM{}, exists("VM", req.ID)
	}
	used := map[int]bool{}
	for _, o := range n.vms {
		used[o.rec.NetIndex] = true
	}
	idx, err := network.FreeIndex(used)
	if err != nil {
		n.mu.Unlock()
		return nodeapi.VM{}, capacity("%v", err)
	}
	v := &vm{id: req.ID, dir: filepath.Join(n.vmsDir(), req.ID)}
	v.rec = record{ID: req.ID, Template: req.Template, Resources: req.Resources, State: nodeapi.VMStopped,
		NetIndex: idx, CreatedAt: time.Now().UTC()}
	v.mu.Lock()
	n.vms[req.ID] = v
	n.mu.Unlock()
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(v.dir)
			n.mu.Lock()
			delete(n.vms, v.id)
			v.gone = true
			n.mu.Unlock()
		}
		v.mu.Unlock()
	}()

	if err := os.Mkdir(v.dir, 0o755); err != nil {
		return nodeapi.VM{}, err
	}
	if err := saveRecord(v.dir, v.rec); err != nil {
		return nodeapi.VM{}, err
	}
	var src source
	if snap != nil {
		if src, err = n.prepare(bctx, v, *snap, false); err != nil {
			return nodeapi.VM{}, err
		}
	} else {
		image, pinned, err := n.images.Disk(bctx, req.Template.Image, req.Resources.DiskGiB)
		if err != nil {
			return nodeapi.VM{}, err
		}
		for _, f := range []string{firecracker.DiskFile, parentDisk} {
			if err := n.clone.clone(image, filepath.Join(v.dir, f)); err != nil {
				return nodeapi.VM{}, fmt.Errorf("disk from %s: %w", req.Template.Image, err)
			}
		}
		if err := n.update(v, func(r *record) { r.BaseImage = pinned }); err != nil {
			return nodeapi.VM{}, err
		}
	}
	start := time.Now()
	if _, err := n.start(bctx, v, src, req.Wake, pol); err != nil {
		return nodeapi.VM{}, err
	}
	if snap != nil && snap.snap.Memory {
		n.update(v, func(r *record) { r.MemParent = true })
	}
	ok = true
	n.mu.Lock()
	v.ready = true
	out := n.view(v)
	n.mu.Unlock()
	from := ""
	if snap != nil {
		from = snap.snap.ID
	}
	n.log.Info("VM created", "vm", v.id, "template", req.Template.Name, "from_snapshot", from, "lazy_memory", src.pager != nil,
		"start_took", time.Since(start).Round(time.Millisecond))
	return out, nil
}

// Resume makes a VM run (docs/NODE.md, POST /v1/vms/{id}/resume).
func (n *Nodelet) Resume(ctx context.Context, id string, req nodeapi.Resume) (nodeapi.VM, error) {
	pol, err := n.compilePolicy(req.Policy)
	if err != nil {
		return nodeapi.VM{}, err
	}
	w := req.Wake
	v, err := n.lock(id)
	if err != nil {
		return nodeapi.VM{}, err
	}
	defer v.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout+bootTimeout)
	defer cancel()
	r := n.recordOf(v)
	if v.m != nil {
		if err := n.replacePolicy(ctx, v, pol); err != nil {
			return nodeapi.VM{}, err
		}
		if err := n.wake(ctx, v.m, r, w, wakeTimeout); err != nil {
			return nodeapi.VM{}, unavailable("the agent of %s did not answer the wake: %v", id, err)
		}
		return n.viewOf(v), nil
	}
	if err := n.notReady(); err != nil {
		return nodeapi.VM{}, err
	}
	switch {
	case r.State == nodeapi.VMPaused && r.Snapshot == "":
		// Paused before phase 2: the memory is in the VM's directory.
		ran, err := n.start(ctx, v, source{state: filepath.Join(v.dir, pauseState), mem: firecracker.Memory{File: filepath.Join(v.dir, pauseMem)}}, w, pol)
		if ran {
			// The guest has run on this disk, or may have: the memory snapshot no
			// longer matches it, so it goes whether the wake worked or not.
			discardPause(v.dir)
		}
		if err != nil {
			if ran {
				n.update(v, func(r *record) { r.State, r.BootedAt = nodeapi.VMStopped, nil })
			}
			return nodeapi.VM{}, err
		}
	case r.State == nodeapi.VMPaused:
		n.snapMu.RLock()
		s, err := n.findSnap(ctx, r.Snapshot)
		var src source
		if err == nil {
			src, err = n.prepare(ctx, v, s, true)
		}
		n.snapMu.RUnlock()
		if err != nil {
			return nodeapi.VM{}, err
		}
		start := time.Now()
		ran, err := n.start(ctx, v, src, w, pol)
		if err != nil {
			if ran {
				// Its guest ran on the disk: the snapshot's memory no longer matches it.
				n.update(v, func(r *record) { r.State, r.BootedAt = nodeapi.VMStopped, nil })
			}
			return nodeapi.VM{}, err
		}
		n.update(v, func(r *record) { r.MemParent = true })
		n.log.Info("VM resumed", "vm", id, "snapshot", r.Snapshot, "lazy_memory", src.pager != nil, "took", time.Since(start).Round(time.Millisecond))
	default:
		if _, err := n.start(ctx, v, source{}, w, pol); err != nil {
			return nodeapi.VM{}, err
		}
	}
	return n.viewOf(v), nil
}

// memSnapshotRoom is the free space a memory snapshot of mib needs: Firecracker
// writes the whole RAM, and the sparse copy of it can take up to half as much
// again before the whole-RAM file is deleted.
func memSnapshotRoom(mib int) int64 { return int64(mib)<<20*3/2 + 64<<20 }

// quiesce gets a running VM ready for a memory snapshot: the agent's
// presuspend, then the balloon over the guest's free memory, then the vCPUs
// frozen. On error the VM is left running.
func (n *Nodelet) quiesce(ctx context.Context, v *vm, where string) error {
	mib := n.recordOf(v).Resources.MemoryMiB
	if free := freeBytes(where); free < memSnapshotRoom(mib) {
		return capacity("not enough disk for a memory snapshot of %s: %d MiB free, %d MiB needed", v.id, free>>20, memSnapshotRoom(mib)>>20)
	}
	if err := agentCall(ctx, v.m, http.MethodPost, "/internal/presuspend", nil, nil); err != nil {
		return unavailable("presuspend in %s: %v", v.id, err)
	}
	// Best effort: the snapshot is right without it, only larger.
	if _, err := v.m.Squeeze(ctx); err != nil {
		n.log.Warn("could not squeeze free memory before a snapshot", "vm", v.id, "err", err)
	}
	if err := v.m.Pause(ctx); err != nil {
		v.m.SetBalloon(ctx, 0)
		return fmt.Errorf("pause vCPUs of %s: %w", v.id, err)
	}
	return nil
}

// thaw undoes quiesce on a VM that runs on.
func (n *Nodelet) thaw(ctx context.Context, v *vm) {
	if err := v.m.Resume(ctx); err != nil {
		n.log.Error("resume after a snapshot failed", "vm", v.id, "err", err)
	}
	if err := v.m.SetBalloon(ctx, 0); err != nil {
		n.log.Warn("could not let the balloon down after a snapshot", "vm", v.id, "err", err)
	}
}

// staging makes the directory a new snapshot is assembled in, refusing an ID
// that is taken here.
func (n *Nodelet) staging(id string) (string, error) {
	if !validSnapID(id) {
		return "", invalid("invalid snapshot id %q", id)
	}
	if fileExistsAt(n.snapDir(id)) || n.inflight(id) != nil {
		return "", exists("snapshot", id)
	}
	staging := filepath.Join(n.snapsDir(), ".tmp-"+id)
	if err := os.Mkdir(staging, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", exists("snapshot", id)
		}
		return "", err
	}
	return staging, nil
}

// memSnap is a memory snapshot of a running VM, taken: Firecracker's files in
// the staging directory.
type memSnap struct {
	diff bool
	tmp  string // what Firecracker wrote: the whole memory, or a diff
	zero []snapstore.Extent
}

// snapshotMemory writes running v's state and memory into staging (as
// snapState and memSnap.tmp), its vCPUs frozen around it by quiesce, and its
// disk at the same moment. A VM whose memory continues a snapshot's writes a
// diff. The caller thaws or kills the VM afterwards.
func (n *Nodelet) snapshotMemory(ctx context.Context, v *vm, staging string) (memSnap, error) {
	r := n.recordOf(v)
	ms := memSnap{diff: r.MemParent && r.Snapshot != "", tmp: filepath.Join(staging, "firecracker.mem")}
	if err := v.m.Snapshot(ctx, filepath.Join(staging, snapState+".tmp"), ms.tmp, ms.diff); err != nil {
		return ms, err
	}
	if v.pager != nil {
		// Pages the guest gave back since the last snapshot read as zeros now;
		// those it gave back before are in that snapshot already, and the
		// guest may have written them since.
		z, err := v.pager.Zeroed(ctx)
		if err != nil {
			return ms, fmt.Errorf("ask the pager for the zeroed pages: %w", err)
		}
		ms.zero = subtractExtents(unionExtents(z), v.zeroed)
		v.zeroed = unionExtents(z)
	}
	// The disk as it is with the vCPUs frozen: the same moment as the memory.
	return ms, n.clone.clone(filepath.Join(v.dir, firecracker.DiskFile), filepath.Join(staging, firecracker.DiskFile))
}

// finishMemory makes staging's memory file snapMem and fills in the memory
// half of spec: a full snapshot is made sparse; a diff's pages go over a
// clone of the parent's whole memory when the VM has one, so the snapshot's
// memory is whole here too, and otherwise the diff is kept as it is.
func (n *Nodelet) finishMemory(v *vm, staging string, ms memSnap, spec *layerSpec) error {
	r := n.recordOf(v)
	mem := filepath.Join(staging, snapMem)
	spec.MemSize = int64(r.Resources.MemoryMiB) << 20
	spec.MemZero = ms.zero
	if !ms.diff {
		if err := firecracker.CompactMemory(ms.tmp, mem); err != nil {
			return err
		}
		var err error
		spec.MemFull, spec.MemOver = true, overZero
		if spec.MemDirty, err = snapstore.DataExtents(mem); err != nil {
			return err
		}
	} else {
		var err error
		if spec.MemDirty, err = snapstore.DataExtents(ms.tmp); err != nil {
			return err
		}
		spec.MemOver = overParent
		if pm := filepath.Join(v.dir, parentMem); fileExistsAt(pm) {
			if err := n.clone.clone(pm, mem); err != nil {
				return err
			}
			if err := overlay(mem, ms.tmp, spec.MemDirty); err != nil {
				return err
			}
			os.Remove(ms.tmp)
			spec.MemFull = true
		} else {
			if err := firecracker.SyncFile(ms.tmp); err != nil {
				return err
			}
			if err := os.Rename(ms.tmp, mem); err != nil {
				return err
			}
		}
	}
	if err := firecracker.SyncFile(filepath.Join(staging, snapState+".tmp")); err != nil {
		return err
	}
	return os.Rename(filepath.Join(staging, snapState+".tmp"), filepath.Join(staging, snapState))
}

// diskSpec fills in the disk half of spec for disk, a clone of v's disk:
// what changed since v's newest snapshot, or its template, or (a VM from
// before phase 2) all of it. It returns the snapshot the layers go over.
func (n *Nodelet) diskSpec(v *vm, disk string, spec *layerSpec) (parent string, err error) {
	r := n.recordOf(v)
	if spec.DiskSize, err = fileSize(disk); err != nil {
		return "", err
	}
	spec.BaseImage = r.BaseImage
	switch {
	case r.Snapshot != "":
		spec.DiskOver, parent = overParent, r.Snapshot
	case r.BaseImage != "":
		spec.DiskOver = overBase
	default:
		spec.DiskOver = overZero
		spec.DiskDirty, err = snapstore.DataExtents(disk)
		return "", err
	}
	if pd := filepath.Join(v.dir, parentDisk); fileExistsAt(pd) {
		spec.DiskDirty, err = changedExtents(disk, pd, n.reflinks)
	} else {
		// Nothing to tell the changes by: all of it, zeros included.
		spec.DiskDirty = []snapstore.Extent{{Off: 0, Len: spec.DiskSize}}
	}
	return parent, err
}

// advance makes snapshot id v's newest, with its disk (and whole memory, if
// it has that here) as the parents of v's next snapshot. The caller holds v.mu.
func (n *Nodelet) advance(v *vm, id string, memParent, memFull bool) error {
	dir := n.snapDir(id)
	if err := n.clone.clone(filepath.Join(dir, firecracker.DiskFile), filepath.Join(v.dir, parentDisk)); err != nil {
		return err
	}
	pm := filepath.Join(v.dir, parentMem)
	if memParent && memFull && v.m != nil {
		// Running on: a later diff goes over this snapshot's memory. (The
		// memory Firecracker maps is the old file, which the rename leaves be.)
		if err := n.clone.clone(filepath.Join(dir, snapMem), pm); err != nil {
			return err
		}
	} else {
		os.Remove(pm)
	}
	return n.update(v, func(r *record) { r.Snapshot, r.MemParent = id, memParent })
}

// Pause snapshots a running VM's memory and disk into snapshot snapID and
// ends its process (docs/NODE.md, POST /v1/vms/{id}/pause).
func (n *Nodelet) Pause(ctx context.Context, id, snapID string) (nodeapi.VM, error) {
	v, err := n.lock(id)
	if err != nil {
		return nodeapi.VM{}, err
	}
	defer v.mu.Unlock()
	return n.pause(ctx, v, snapID, false)
}

// pause pauses v into snapID. With announce (a pause no request asked for),
// the VM's change goes to the event stream before its snapshot's upload starts.
func (n *Nodelet) pause(ctx context.Context, v *vm, snapID string, announce bool) (nodeapi.VM, error) {
	switch r := n.recordOf(v); {
	case r.State == nodeapi.VMPaused:
		return n.viewOf(v), nil
	case v.m == nil:
		return nodeapi.VM{}, conflict("VM %s is %s; only a running VM can be paused", v.id, r.State)
	case snapID == "":
		return nodeapi.VM{}, invalid("a pause needs the snapshot to make: {\"snapshot\": \"<id>\"}")
	}
	staging, err := n.staging(snapID)
	if err != nil {
		return nodeapi.VM{}, err
	}
	defer os.RemoveAll(staging)
	// Detached from the request: a client hanging up must not leave the VM frozen.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	start := time.Now()
	if err := n.quiesce(ctx, v, staging); err != nil {
		return nodeapi.VM{}, err
	}
	ms, err := n.snapshotMemory(ctx, v, staging)
	if err != nil {
		n.thaw(ctx, v)
		return nodeapi.VM{}, fmt.Errorf("snapshot %s: %w", v.id, err)
	}
	n.kill(v)
	frozen := time.Since(start)
	// The VM is gone either way from here: a failure to write the snapshot
	// out leaves the disk, which the guest synced in presuspend.
	r := n.recordOf(v)
	spec := layerSpec{Layer: header.NewLayerID()}
	err = n.finishMemory(v, staging, ms, &spec)
	var parent string
	if err == nil {
		parent, err = n.diskSpec(v, filepath.Join(staging, firecracker.DiskFile), &spec)
	}
	if ms.diff {
		parent = r.Snapshot
	}
	var snap nodeapi.Snapshot
	gate := make(chan struct{})
	defer close(gate)
	if err == nil {
		snap, err = n.commit(ctx, staging, nodeapi.Snapshot{ID: snapID, VM: v.id, Memory: true, Template: r.Template,
			Resources: r.Resources, CreatedAt: start.UTC(), Parent: parent}, spec, gate)
	}
	if err == nil {
		err = n.advance(v, snapID, true, spec.MemFull)
	}
	if err != nil {
		n.update(v, func(r *record) { r.State, r.BootedAt, r.MemParent = nodeapi.VMStopped, nil, false })
		return nodeapi.VM{}, fmt.Errorf("write the memory snapshot of %s (it is stopped instead): %w", v.id, err)
	}
	discardPause(v.dir)
	n.update(v, func(r *record) { r.State = nodeapi.VMPaused })
	if announce {
		n.events.publish(event{nodeapi.EventVM, n.viewOf(v)})
	}
	n.log.Info("VM paused", "vm", v.id, "snapshot", snapID, "diff", ms.diff, "took", time.Since(start).Round(time.Millisecond),
		"frozen", frozen.Round(time.Millisecond), "mem_dirty_mib", extentsBytes(spec.MemDirty)>>20,
		"disk_dirty_mib", extentsBytes(spec.DiskDirty)>>20, "layers", len(snap.Layers))
	return n.viewOf(v), nil
}

// Stop powers a VM off through its agent, killing it after 10 s, and keeps its
// disk as snapshot snapID (docs/NODE.md, POST /v1/vms/{id}/stop). A paused VM
// is stopped too, its disk kept the same way.
func (n *Nodelet) Stop(ctx context.Context, id, snapID string) (nodeapi.VM, error) {
	v, err := n.lock(id)
	if err != nil {
		return nodeapi.VM{}, err
	}
	defer v.mu.Unlock()
	r := n.recordOf(v)
	if v.m == nil && r.State == nodeapi.VMStopped {
		return n.viewOf(v), nil
	}
	if snapID == "" {
		return nodeapi.VM{}, invalid("a stop needs the snapshot to make: {\"snapshot\": \"<id>\"}")
	}
	staging, err := n.staging(snapID)
	if err != nil {
		return nodeapi.VM{}, err
	}
	defer os.RemoveAll(staging)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	start := time.Now()
	if v.m != nil {
		m := v.m
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			// The agent may not answer: it is powering off.
			agentCall(ctx, m, http.MethodPost, "/internal/poweroff", nil, nil)
		}()
		select {
		case <-m.Exited():
		case <-time.After(10 * time.Second):
			n.log.Warn("VM did not power off in 10 s; killed", "vm", id)
		}
		n.kill(v)
	}
	discardPause(v.dir)
	n.update(v, func(r *record) { r.State, r.Error, r.BootedAt, r.MemParent = nodeapi.VMStopped, "", nil, false })
	if err := n.diskSnapshot(ctx, v, staging, snapID, start, true); err != nil {
		return nodeapi.VM{}, fmt.Errorf("keep the disk of %s (it is stopped): %w", id, err)
	}
	return n.viewOf(v), nil
}

// diskSnapshot makes snapshot snapID of v's disk, which is still (v is not
// running, or frozen), and makes it v's newest with newest.
func (n *Nodelet) diskSnapshot(ctx context.Context, v *vm, staging, snapID string, start time.Time, newest bool) error {
	disk := filepath.Join(staging, firecracker.DiskFile)
	if !fileExistsAt(disk) {
		if err := n.clone.clone(filepath.Join(v.dir, firecracker.DiskFile), disk); err != nil {
			return err
		}
	}
	r := n.recordOf(v)
	spec := layerSpec{Layer: header.NewLayerID()}
	parent, err := n.diskSpec(v, disk, &spec)
	if err != nil {
		return err
	}
	if _, err := n.commit(ctx, staging, nodeapi.Snapshot{ID: snapID, VM: v.id, Template: r.Template, Resources: r.Resources,
		CreatedAt: start.UTC(), Parent: parent}, spec, nil); err != nil {
		return err
	}
	if newest {
		if err := n.advance(v, snapID, false, false); err != nil {
			return err
		}
	}
	n.log.Info("disk snapshot taken", "vm", v.id, "snapshot", snapID, "took", time.Since(start).Round(time.Millisecond),
		"disk_dirty_mib", extentsBytes(spec.DiskDirty)>>20)
	return nil
}

// Delete removes a VM and its local files: never a snapshot, its pause's
// included (docs/NODE.md).
func (n *Nodelet) Delete(id string) error {
	v, err := n.lock(id)
	if err != nil {
		return err
	}
	defer v.mu.Unlock()
	n.kill(v)
	if err := os.RemoveAll(v.dir); err != nil {
		return err
	}
	n.mu.Lock()
	delete(n.vms, id)
	v.gone = true
	n.mu.Unlock()
	n.log.Info("VM deleted", "vm", id)
	return nil
}

// Snapshot snapshots a VM's disk, and its memory with req.Memory
// (docs/NODE.md, POST /v1/vms/{id}/snapshots).
func (n *Nodelet) Snapshot(ctx context.Context, vmID string, req nodeapi.CreateSnapshot) (nodeapi.Snapshot, error) {
	if !validSnapID(req.ID) {
		return nodeapi.Snapshot{}, invalid("invalid snapshot id %q", req.ID)
	}
	v, err := n.lock(vmID)
	if err != nil {
		return nodeapi.Snapshot{}, err
	}
	defer v.mu.Unlock()
	r := n.recordOf(v)
	running, paused := v.m != nil, r.State == nodeapi.VMPaused
	if req.Memory && !running && !paused {
		return nodeapi.Snapshot{}, conflict("VM %s is %s; a memory snapshot needs it running or paused", vmID, r.State)
	}
	staging, err := n.staging(req.ID)
	if err != nil {
		return nodeapi.Snapshot{}, err
	}
	defer os.RemoveAll(staging)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	start := time.Now()
	switch {
	case running && req.Memory:
		if err := n.quiesce(ctx, v, staging); err != nil {
			return nodeapi.Snapshot{}, err
		}
		ms, err := n.snapshotMemory(ctx, v, staging)
		n.thaw(ctx, v)
		if cut := v.cutStreams(); cut > 0 {
			n.log.Info("cut the agent streams a memory snapshot ended", "vm", vmID, "streams", cut)
		}
		if err != nil {
			return nodeapi.Snapshot{}, fmt.Errorf("snapshot %s: %w", vmID, err)
		}
		// Made whole or sparse with the VM running again: the pause lasts only
		// as long as Firecracker's write.
		spec := layerSpec{Layer: header.NewLayerID()}
		if err := n.finishMemory(v, staging, ms, &spec); err != nil {
			return nodeapi.Snapshot{}, err
		}
		parent, err := n.diskSpec(v, filepath.Join(staging, firecracker.DiskFile), &spec)
		if err != nil {
			return nodeapi.Snapshot{}, err
		}
		if ms.diff {
			parent = r.Snapshot
		}
		snap, err := n.commit(ctx, staging, nodeapi.Snapshot{ID: req.ID, VM: vmID, Memory: true, Template: r.Template,
			Resources: r.Resources, CreatedAt: start.UTC(), Parent: parent}, spec, nil)
		if err != nil {
			return snap, err
		}
		// The VM runs on from this snapshot's memory: it is the newest.
		if err := n.advance(v, req.ID, true, spec.MemFull); err != nil {
			return snap, err
		}
		n.log.Info("snapshot taken", "vm", vmID, "snapshot", req.ID, "memory", true, "diff", ms.diff, "took", time.Since(start).Round(time.Millisecond))
		return snap, nil
	case running:
		// The guest's dirty pages reach its disk first; then the vCPUs freeze
		// for the length of a reflink, so the copy is one moment's.
		if err := agentCall(ctx, v.m, http.MethodPost, "/internal/sync", nil, nil); err != nil {
			return nodeapi.Snapshot{}, unavailable("sync in %s: %v", vmID, err)
		}
		if err := v.m.Pause(ctx); err != nil {
			return nodeapi.Snapshot{}, fmt.Errorf("pause vCPUs of %s: %w", vmID, err)
		}
		err = n.clone.clone(filepath.Join(v.dir, firecracker.DiskFile), filepath.Join(staging, firecracker.DiskFile))
		if rerr := v.m.Resume(ctx); rerr != nil {
			n.log.Error("resume after a snapshot failed", "vm", vmID, "err", rerr)
		}
		if err != nil {
			return nodeapi.Snapshot{}, err
		}
		// Not the VM's newest: waking from it would lose the memory it has.
		err = n.diskSnapshot(ctx, v, staging, req.ID, start, false)
	case paused && req.Memory:
		err = n.pausedSnapshot(ctx, v, staging, req.ID, start)
	default:
		// A disk-only snapshot of a stopped VM is its newest; of a paused one it isn't.
		err = n.diskSnapshot(ctx, v, staging, req.ID, start, !paused)
	}
	if err != nil {
		return nodeapi.Snapshot{}, err
	}
	n.snapMu.RLock()
	defer n.snapMu.RUnlock()
	s, _, err := n.localSnap(req.ID)
	return s, err
}

// pausedSnapshot is a memory snapshot of a paused VM: its pause, again, under
// another name. In the store it is a generation over the pause's that
// changes nothing. It becomes the VM's newest, which the VM is paused on now.
func (n *Nodelet) pausedSnapshot(ctx context.Context, v *vm, staging, snapID string, start time.Time) error {
	r := n.recordOf(v)
	spec := layerSpec{Layer: header.NewLayerID(), BaseImage: r.BaseImage, MemSize: int64(r.Resources.MemoryMiB) << 20}
	snap := nodeapi.Snapshot{ID: snapID, VM: v.id, Memory: true, Template: r.Template, Resources: r.Resources, CreatedAt: start.UTC()}
	pairs := [][2]string{}
	if r.Snapshot == "" {
		// Paused before phase 2: a whole generation of its own.
		pairs = append(pairs, [2]string{filepath.Join(v.dir, pauseState), snapState}, [2]string{filepath.Join(v.dir, pauseMem), snapMem},
			[2]string{filepath.Join(v.dir, firecracker.DiskFile), firecracker.DiskFile})
		spec.MemFull, spec.MemOver, spec.DiskOver = true, overZero, overZero
	} else {
		n.snapMu.RLock()
		_, pspec, err := n.localSnap(r.Snapshot)
		n.snapMu.RUnlock()
		if err != nil {
			return fmt.Errorf("the snapshot %s is paused on: %w", v.id, err)
		}
		dir := n.snapDir(r.Snapshot)
		pairs = append(pairs, [2]string{filepath.Join(dir, snapState), snapState}, [2]string{filepath.Join(dir, firecracker.DiskFile), firecracker.DiskFile})
		if pspec.MemFull {
			pairs = append(pairs, [2]string{filepath.Join(dir, snapMem), snapMem})
		}
		spec.MemFull, spec.MemOver, spec.DiskOver, snap.Parent = pspec.MemFull, overParent, overParent, r.Snapshot
	}
	for _, p := range pairs {
		if err := n.clone.clone(p[0], filepath.Join(staging, p[1])); err != nil {
			return err
		}
	}
	mem := filepath.Join(staging, snapMem)
	if !spec.MemFull {
		// Nothing of its own: an empty diff, all of it from the parent.
		if err := os.WriteFile(mem, nil, 0o644); err != nil {
			return err
		}
	} else if spec.MemOver == overZero {
		var err error
		if spec.MemDirty, err = snapstore.DataExtents(mem); err != nil {
			return err
		}
	}
	var err error
	disk := filepath.Join(staging, firecracker.DiskFile)
	if spec.DiskSize, err = fileSize(disk); err != nil {
		return err
	}
	if spec.DiskOver == overZero {
		if spec.DiskDirty, err = snapstore.DataExtents(disk); err != nil {
			return err
		}
	}
	if _, err := n.commit(ctx, staging, snap, spec, nil); err != nil {
		return err
	}
	if r.Snapshot == "" {
		// The VM's memory is the new snapshot's now, not the pause files'.
		if err := n.advance(v, snapID, true, true); err != nil {
			return err
		}
		discardPause(v.dir)
		return nil
	}
	return n.update(v, func(r *record) { r.Snapshot = snapID })
}

// Restore replaces a VM's disk, and its memory if the snapshot has it, with a
// snapshot's, and runs it (docs/NODE.md, POST /v1/vms/{id}/restore).
func (n *Nodelet) Restore(ctx context.Context, vmID string, req nodeapi.Restore) (nodeapi.VM, error) {
	if !validSnapID(req.Snapshot) {
		return nodeapi.VM{}, invalid("invalid snapshot id %q", req.Snapshot)
	}
	pol, err := n.compilePolicy(req.Policy)
	if err != nil {
		return nodeapi.VM{}, err
	}
	v, err := n.lock(vmID)
	if err != nil {
		return nodeapi.VM{}, err
	}
	defer v.mu.Unlock()
	if err := n.notReady(); err != nil {
		return nodeapi.VM{}, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout+bootTimeout)
	defer cancel()
	n.snapMu.RLock()
	defer n.snapMu.RUnlock()
	s, err := n.findSnap(ctx, req.Snapshot)
	if err != nil {
		return nodeapi.VM{}, err
	}
	r := n.recordOf(v)
	if s.snap.Memory && (s.snap.Resources.CPUs != r.Resources.CPUs || s.snap.Resources.MemoryMiB != r.Resources.MemoryMiB) {
		return nodeapi.VM{}, invalid("snapshot %s holds the memory of a %d-CPU %d MiB VM; %s is %d CPUs and %d MiB",
			s.snap.ID, s.snap.Resources.CPUs, s.snap.Resources.MemoryMiB, vmID, r.Resources.CPUs, r.Resources.MemoryMiB)
	}
	// The running filesystem is about to be replaced; nothing in it is worth flushing.
	n.kill(v)
	discardPause(v.dir)
	n.update(v, func(r *record) {
		r.State, r.BootedAt, r.MemParent, r.Resources.DiskGiB = nodeapi.VMStopped, nil, false, s.snap.Resources.DiskGiB
	})
	src, err := n.prepare(ctx, v, s, false)
	if err != nil {
		return nodeapi.VM{}, fmt.Errorf("restore %s from %s: %w", vmID, s.snap.ID, err)
	}
	if _, err := n.start(ctx, v, src, req.Wake, pol); err != nil {
		return nodeapi.VM{}, err
	}
	n.update(v, func(r *record) { r.MemParent = s.snap.Memory })
	n.log.Info("snapshot restored", "vm", vmID, "snapshot", s.snap.ID, "memory", s.snap.Memory, "lazy_memory", src.pager != nil)
	return n.viewOf(v), nil
}

// running returns VM id's machine if it is running, without holding its lock
// afterwards: streams and activity must not hold up a pause.
func (n *Nodelet) running(id string) (Machine, error) {
	v, err := n.lock(id)
	if err != nil {
		return nil, err
	}
	defer v.mu.Unlock()
	if v.m == nil {
		return nil, conflict("VM %s is %s, not running", id, n.recordOf(v).State)
	}
	return v.m, nil
}

// Activity is the agent's /internal/activity, passed through.
func (n *Nodelet) Activity(ctx context.Context, id string) (nodeapi.Activity, error) {
	m, err := n.running(id)
	if err != nil {
		return nodeapi.Activity{}, err
	}
	var a nodeapi.Activity
	if err := agentCall(ctx, m, http.MethodGet, "/internal/activity", nil, &a); err != nil {
		return a, unavailable("activity of %s: %v", id, err)
	}
	return a, nil
}

// DialAgent opens a stream to VM id's agent, which stays the VM's until it
// closes, so that a memory snapshot can cut it.
func (n *Nodelet) DialAgent(ctx context.Context, id string) (net.Conn, error) {
	v, err := n.lock(id)
	if err != nil {
		return nil, err
	}
	m := v.m
	v.mu.Unlock()
	if m == nil {
		return nil, conflict("VM %s is %s, not running", id, n.recordOf(v).State)
	}
	c, err := m.Dial(ctx, AgentPort)
	if err != nil {
		return nil, unavailable("agent of %s: %v", id, err)
	}
	s := &agentStream{Conn: c, v: v}
	v.streamMu.Lock()
	if v.streams == nil {
		v.streams = map[*agentStream]struct{}{}
	}
	v.streams[s] = struct{}{}
	v.streamMu.Unlock()
	return s, nil
}

// Drain is what the nodelet does on SIGTERM (docs/NODE.md, "Drain"): it stops
// being ready, pauses every running VM, in parallel, into a snapshot of its
// own naming (<sandbox>.@drain-<unix ms>), telling the event stream, and
// waits for every upload, so the sandboxes wake on another node with their
// processes. A VM that can't be paused before ctx ends (or at all) is killed,
// and its record says stopped. VMs mid-transition are waited for.
func (n *Nodelet) Drain(ctx context.Context) {
	n.mu.Lock()
	n.draining = true
	vms := make([]*vm, 0, len(n.vms))
	for _, v := range n.vms {
		if v.ready {
			vms = append(vms, v)
		}
	}
	n.mu.Unlock()
	// Tell loam-api first: once this node stops answering, it wakes the
	// node's sandboxes elsewhere without waiting for it (docs/NODE.md, "Drain").
	n.events.publish(event{nodeapi.EventNode, n.Node()})
	defer n.events.delivered(2 * time.Second)
	var wg sync.WaitGroup
	for _, v := range vms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v.mu.Lock()
			defer v.mu.Unlock()
			if v.m == nil {
				return
			}
			id := fmt.Sprintf("%s.@drain-%d", v.id, time.Now().UnixMilli())
			if _, err := n.pause(ctx, v, id, true); err != nil {
				n.log.Error("could not pause a VM on the way out", "vm", v.id, "err", err)
				n.events.publish(event{nodeapi.EventVM, n.viewOf(v)})
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		n.log.Error("ran out of time pausing VMs; the rest are stopped")
	}
	if err := n.WaitUploads(ctx); err != nil {
		n.log.Error("ran out of time uploading snapshots; they stay on this node, and upload at its next start", "err", err)
	} else {
		n.log.Info("drained: every snapshot is stored")
	}
	n.Shutdown()
}

// Shutdown ends every running VM now: a running VM does not survive its
// nodelet, and a restarted one reports it stopped. Uploads in flight are
// cancelled; the next nodelet resumes them.
func (n *Nodelet) Shutdown() {
	n.mu.Lock()
	vms := make([]*vm, 0, len(n.vms))
	for _, v := range n.vms {
		vms = append(vms, v)
	}
	n.mu.Unlock()
	for _, v := range vms {
		if !v.mu.TryLock() {
			// A pause still in flight past Drain's deadline: its VM process
			// dies with the pod, and the next nodelet reports what it finds.
			continue
		}
		if v.m != nil {
			n.kill(v)
			n.update(v, func(r *record) { r.State, r.BootedAt, r.MemParent = nodeapi.VMStopped, nil, false })
		}
		v.mu.Unlock()
	}
	n.upMu.Lock()
	for _, u := range n.uploads {
		u.cancel()
	}
	n.upMu.Unlock()
	n.upWG.Wait()
}
