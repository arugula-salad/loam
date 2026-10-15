package nodelet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/internal/nodelet/firecracker"
	"github.com/arugula-salad/loam/internal/snapstore"
	"github.com/arugula-salad/loam/internal/snapstore/header"
)

// Every snapshot is stored (docs/NODE.md, "Phase 2: the snapshot store"): a
// pause's, a stop's and a checkpoint's. Each is first a directory under
// snapshots/ (the local copy, which a restore on this node reads directly),
// then a generation in the store: one new layer per file holding what changed
// since its parent, and a header per file mapping every block (docs/STORE.md).
// The answer to the request that made it comes once the local copy is
// complete; the upload runs on, and its end is the event `snapshot`.
//
// A generation's parent is the snapshot the VM last paused into, was restored
// from or stopped as (record.Snapshot), so a sandbox that pauses again and
// again uploads only what each run changed. Its upload needs its parent's
// durable headers, so it waits for the parent's upload when that is still
// running here; the layers it reports are final when it is made, worked out
// from the parent's provisional headers if need be.

// Files in a snapshot's directory.
const (
	snapFile   = "snapshot.json" // nodeapi.Snapshot; its absence means the directory is not (or no longer) a snapshot
	specFile   = "layer.json"    // layerSpec
	snapState  = "snap.vmstate"
	snapMem    = "snap.mem"
	memHeader  = "memory.header" // the durable headers, once stored
	diskHeader = "disk.header"
)

// What a generation's file is a diff against.
const (
	overZero   = "zero"   // nothing: the first layer of a memory file, or of a disk made before phase 2
	overBase   = "base"   // the template image (BaseImage): a disk's first layer
	overParent = "parent" // the parent snapshot's header
)

// layerSpec is what a snapshot's upload needs besides its files: which blocks
// of each file are this generation's, over what. Kept in its directory, so a
// restarted nodelet can finish the upload.
type layerSpec struct {
	Layer header.LayerID `json:"layer"`
	// BaseImage is the template image (pinned by digest) a disk over the base
	// is a diff against; every node builds it byte for byte the same.
	BaseImage string `json:"base_image,omitempty"`

	// MemFull says snap.mem holds the whole memory, so a restore here maps it
	// as it is. Otherwise it holds only this generation's pages (a diff of a
	// VM whose memory came lazily from the store), and a restore reads the
	// rest through the store, once this snapshot is stored.
	MemFull  bool               `json:"mem_full,omitempty"`
	MemSize  int64              `json:"mem_size,omitempty"` // 0: no memory
	MemOver  string             `json:"mem_over,omitempty"`
	MemDirty []snapstore.Extent `json:"mem_dirty,omitempty"`
	MemZero  []snapstore.Extent `json:"mem_zero,omitempty"`

	DiskSize  int64              `json:"disk_size"`
	DiskOver  string             `json:"disk_over"`
	DiskDirty []snapstore.Extent `json:"disk_dirty,omitempty"`
}

// storedMeta is a snapshot's meta.json in the store: everything a node
// without its local copy needs to restore it, besides its headers.
type storedMeta struct {
	Snapshot  nodeapi.Snapshot `json:"snapshot"`
	BaseImage string           `json:"base_image,omitempty"`
	// VMState is Firecracker's state file, for a memory snapshot.
	VMState []byte `json:"vmstate,omitempty"`
}

// upload is one snapshot's way into the store.
type upload struct {
	id, dir string
	snap    nodeapi.Snapshot
	spec    layerSpec
	// parent is the parent's upload when it was still running here as this
	// one was made; otherwise pm and pd are the parent's durable headers.
	parent *upload
	pm, pd *header.Header
	// prov are this generation's provisional headers (nil for a file it
	// hasn't): what a child made before this one is stored composes over.
	prov [2]*header.Header

	ctx    context.Context
	cancel context.CancelFunc
	gate   <-chan struct{}
	done   chan struct{}
	// Set before done closes.
	dur [2]*header.Header
	err error

	mu      sync.Mutex // orders storing the snapshot against deleting it
	deleted bool
}

const (
	// uploadWorkers bounds the snapshots uploading at once (a drain pauses
	// every VM together); each upload has its own bounded part workers.
	uploadWorkers = 4
	// uploadAttempts and the backoff ride out the store being away for a
	// minute or so; a snapshot whose upload gives up stays local and unstored,
	// and is tried again at the nodelet's next start.
	uploadAttempts = 8
)

func (n *Nodelet) snapDir(id string) string { return filepath.Join(n.snapsDir(), id) }

// localSnap reads the local copy of snapshot id. The caller holds snapMu.
func (n *Nodelet) localSnap(id string) (nodeapi.Snapshot, layerSpec, error) {
	var s nodeapi.Snapshot
	var spec layerSpec
	dir := n.snapDir(id)
	b, err := os.ReadFile(filepath.Join(dir, snapFile))
	if errors.Is(err, os.ErrNotExist) {
		return s, spec, notFound("snapshot", id)
	}
	if err == nil {
		err = json.Unmarshal(b, &s)
	}
	if err != nil {
		return s, spec, err
	}
	b, err = os.ReadFile(filepath.Join(dir, specFile))
	if err == nil {
		err = json.Unmarshal(b, &spec)
	}
	return s, spec, err
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

// storeSnapshot reads snapshot id from the store.
func (n *Nodelet) storeSnapshot(ctx context.Context, id string) (*snapstore.Snapshot, storedMeta, error) {
	var meta storedMeta
	sn, err := n.store.GetSnapshot(ctx, id)
	if errors.Is(err, snapstore.ErrNotFound) {
		return nil, meta, notFound("snapshot", id)
	}
	if err != nil {
		return nil, meta, fmt.Errorf("snapshot %s from the store: %w", id, err)
	}
	if err := json.Unmarshal(sn.Meta, &meta); err != nil {
		return nil, meta, fmt.Errorf("snapshot %s: meta.json: %w", id, err)
	}
	meta.Snapshot.Stored = true
	return sn, meta, nil
}

// inflight is snapshot id's upload, if it is running here.
func (n *Nodelet) inflight(id string) *upload {
	n.upMu.Lock()
	defer n.upMu.Unlock()
	return n.uploads[id]
}

// durableHeaders returns stored snapshot id's headers, from its local copy
// or the store. The memory header is nil for a disk-only snapshot.
func (n *Nodelet) durableHeaders(ctx context.Context, id string) (mem, disk *header.Header, err error) {
	dir := n.snapDir(id)
	if b, err := os.ReadFile(filepath.Join(dir, diskHeader)); err == nil {
		if disk, err = header.Deserialize(b); err == nil {
			if b, err := os.ReadFile(filepath.Join(dir, memHeader)); err == nil {
				if mem, err = header.Deserialize(b); err != nil {
					return nil, nil, err
				}
			}
			return mem, disk, nil
		}
	}
	sn, _, err := n.storeSnapshot(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return sn.Memory, sn.Disk, nil
}

// commit makes the snapshot staged in staging (its files complete) snapshot
// snap.ID, local and listed, and starts its upload. snap.Layers are worked out
// here, final. A snapshot whose parent can't be read is still made, here:
// its upload fails, and it stays a local one.
//
// An upload starts at once, unless gate is given: then once gate closes (a
// drain's pause tells the VM's change first, its snapshot's after).
func (n *Nodelet) commit(ctx context.Context, staging string, snap nodeapi.Snapshot, spec layerSpec, gate <-chan struct{}) (nodeapi.Snapshot, error) {
	final := n.snapDir(snap.ID)
	u, err := n.newUpload(ctx, final, snap, spec)
	snap.Layers = u.snap.Layers
	if err != nil {
		n.log.Error("a snapshot's parent can't be read: it is made, but can't be stored", "snapshot", snap.ID, "parent", snap.Parent, "err", err)
		u.err = err
	}
	for _, f := range []string{firecracker.DiskFile, snapState, snapMem} {
		if p := filepath.Join(staging, f); fileExistsAt(p) {
			snap.SizeBytes += allocated(p)
		}
	}
	u.snap = snap
	if err := writeJSONFile(filepath.Join(staging, specFile), spec); err != nil {
		return snap, err
	}
	if err := writeJSONFile(filepath.Join(staging, snapFile), snap); err != nil {
		return snap, err
	}
	n.snapMu.RLock()
	err = os.Rename(staging, final)
	n.snapMu.RUnlock()
	if err != nil {
		return snap, err
	}
	u.gate = gate
	n.startUpload(u)
	return snap, nil
}

func fileExistsAt(p string) bool { _, err := os.Stat(p); return err == nil }

// newUpload prepares snap's upload: its parent's headers (or upload), its
// provisional headers and, from them, its layers. On an error the upload is
// returned anyway, its layers only its own.
func (n *Nodelet) newUpload(ctx context.Context, dir string, snap nodeapi.Snapshot, spec layerSpec) (*upload, error) {
	u := &upload{id: snap.ID, dir: dir, snap: snap, spec: spec, done: make(chan struct{})}
	u.ctx, u.cancel = context.WithCancel(context.Background())
	u.snap.Layers = []string{spec.Layer.String()}
	var pm, pd *header.Header
	if spec.MemOver == overParent || spec.DiskOver == overParent {
		if snap.Parent == "" {
			return u, errors.New("a diff with no parent")
		}
		if p := n.inflight(snap.Parent); p != nil {
			u.parent = p
			pm, pd = p.prov[0], p.prov[1]
		} else {
			hctx, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			var err error
			if pm, pd, err = n.durableHeaders(hctx, snap.Parent); err != nil {
				return u, fmt.Errorf("parent %s: %w", snap.Parent, err)
			}
			u.pm, u.pd = pm, pd
		}
	}
	memBase, diskBase, err := bases(spec, pm, pd)
	if err != nil {
		return u, err
	}
	if spec.MemSize > 0 {
		l, err := n.store.NewLayer(snapstore.LayerSpec{Layer: spec.Layer, Kind: snapstore.Memory, Path: filepath.Join(dir, snapMem),
			Size: spec.MemSize, Dirty: spec.MemDirty, Zero: spec.MemZero})
		if err == nil {
			u.prov[0], err = l.Provisional(memBase)
		}
		if err != nil {
			return u, fmt.Errorf("memory layer: %w", err)
		}
	}
	l, err := n.store.NewLayer(snapstore.LayerSpec{Layer: spec.Layer, Kind: snapstore.Disk, Path: filepath.Join(dir, firecracker.DiskFile),
		Size: spec.DiskSize, Dirty: spec.DiskDirty})
	if err == nil {
		u.prov[1], err = l.Provisional(diskBase)
	}
	if err != nil {
		return u, fmt.Errorf("disk layer: %w", err)
	}
	u.snap.Layers = nil
	layers := (&snapstore.Snapshot{Memory: u.prov[0], Disk: u.prov[1]}).Layers()
	if lazyDiff(spec) && pm != nil {
		// The upload drops pages that didn't change (lazyDiff), which may
		// leave more of the parent's layers referenced than the provisional
		// header says: every one of the parent's is reported, ahead of the
		// snapshot's own, so the list is final now.
		var with []header.LayerID
		for _, id := range append(pm.StoredLayers(), layers...) {
			if id != spec.Layer && !slices.Contains(with, id) {
				with = append(with, id)
			}
		}
		layers = append(with, spec.Layer)
	}
	for _, id := range layers {
		u.snap.Layers = append(u.snap.Layers, id.String())
	}
	return u, nil
}

// lazyDiff says spec's memory is a diff of a VM whose memory came lazily from
// the store. KVM logs as dirty every page the pager installed and the guest
// so much as read (the pager's copies are writable pages, which KVM maps
// writable at once), so the diff holds the pages the guest touched, not only
// those it wrote; the upload compares them with the parent's and keeps the
// ones that changed.
func lazyDiff(spec layerSpec) bool {
	return spec.MemSize > 0 && spec.MemOver == overParent && !spec.MemFull
}

// changedPages returns the pages of dirty whose content in path differs from
// parent's (read through the store; the pager fetched them into the node's
// chunk directory already).
func (n *Nodelet) changedPages(ctx context.Context, path string, parent *header.Header, dirty []snapstore.Extent) ([]snapstore.Extent, error) {
	pf, err := n.store.OpenFile(parent, snapstore.Memory, "")
	if err != nil {
		return nil, err
	}
	defer pf.Close()
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const chunk = 1 << 20
	a, b := make([]byte, chunk), make([]byte, chunk)
	var out []snapstore.Extent
	for _, e := range unionExtents(dirty) {
		for off := e.Off; off < e.Off+e.Len; off += chunk {
			m := min(chunk, e.Off+e.Len-off)
			if err := readFull(f, a[:m], off); err != nil {
				return nil, err
			}
			if _, err := pf.ReadAt(ctx, b[:m], off); err != nil {
				return nil, err
			}
			for p := int64(0); p < m; p += pageSize {
				if bytes.Equal(a[p:p+pageSize], b[p:p+pageSize]) {
					continue
				}
				if k := len(out); k > 0 && out[k-1].Off+out[k-1].Len == off+p {
					out[k-1].Len += pageSize
				} else {
					out = append(out, snapstore.Extent{Off: off + p, Len: pageSize})
				}
			}
		}
	}
	return out, nil
}

// bases are the headers a generation's memory and disk layers go over.
func bases(spec layerSpec, pm, pd *header.Header) (mem, disk *header.Header, err error) {
	if spec.MemSize > 0 {
		switch spec.MemOver {
		case overZero:
			mem, err = header.NewZero(uint64(spec.MemSize), header.PageSize)
		case overParent:
			if mem = pm; mem == nil {
				err = errors.New("the parent snapshot has no memory")
			}
		default:
			err = fmt.Errorf("memory over %q", spec.MemOver)
		}
		if err != nil {
			return nil, nil, err
		}
	}
	switch spec.DiskOver {
	case overZero:
		disk, err = header.NewZero(uint64(spec.DiskSize), header.PageSize)
	case overBase:
		disk, err = header.NewBase(uint64(spec.DiskSize), header.PageSize)
	case overParent:
		if disk = pd; disk == nil {
			err = errors.New("the parent snapshot has no disk header")
		}
	default:
		err = fmt.Errorf("disk over %q", spec.DiskOver)
	}
	return mem, disk, err
}

func (n *Nodelet) startUpload(u *upload) {
	n.upMu.Lock()
	n.uploads[u.id] = u
	n.upMu.Unlock()
	n.upWG.Add(1)
	go n.runUpload(u)
}

func (n *Nodelet) runUpload(u *upload) {
	defer n.upWG.Done()
	defer n.finishUpload(u)
	if u.err != nil {
		return
	}
	if u.gate != nil {
		select {
		case <-u.gate:
		case <-u.ctx.Done():
			u.err = u.ctx.Err()
			return
		}
	}
	if p := u.parent; p != nil {
		select {
		case <-p.done:
		case <-u.ctx.Done():
			u.err = u.ctx.Err()
			return
		}
		if p.err != nil {
			u.err = fmt.Errorf("its parent %s was not stored: %v", p.id, p.err)
			return
		}
		u.pm, u.pd = p.dur[0], p.dur[1]
	}
	select {
	case n.upSlots <- struct{}{}:
		defer func() { <-n.upSlots }()
	case <-u.ctx.Done():
		u.err = u.ctx.Err()
		return
	}
	start := time.Now()
	for attempt := 1; ; attempt++ {
		u.err = n.uploadOnce(u)
		if u.err == nil || u.ctx.Err() != nil || attempt == uploadAttempts {
			break
		}
		wait := min(time.Second<<attempt, time.Minute)
		n.log.Warn("snapshot upload failed; trying again", "snapshot", u.id, "attempt", attempt, "in", wait, "err", u.err)
		select {
		case <-time.After(wait):
		case <-u.ctx.Done():
		}
	}
	if u.err != nil {
		return
	}
	if u.isDeleted() {
		n.log.Info("uploaded the layers of a deleted snapshot, for its children", "snapshot", u.id, "layer", u.spec.Layer)
		return
	}
	var bytes int64
	for _, h := range u.dur {
		if h != nil {
			if ld, ok := h.Layers[u.spec.Layer]; ok && ld.Frames != nil {
				bytes += ld.Frames.CompressedSize()
			} else if ok {
				bytes += ld.Size
			}
		}
	}
	n.log.Info("snapshot stored", "snapshot", u.id, "layer", u.spec.Layer, "took", time.Since(start).Round(time.Millisecond),
		"uploaded_bytes", bytes, "mem_dirty_mib", extentsBytes(u.spec.MemDirty)>>20, "disk_dirty_mib", extentsBytes(u.spec.DiskDirty)>>20)
}

func extentsBytes(es []snapstore.Extent) int64 {
	var t int64
	for _, e := range es {
		t += e.Len
	}
	return t
}

// uploadOnce uploads u's layers, then (unless it was deleted meanwhile) its
// headers and meta.json, and marks the local copy stored.
func (n *Nodelet) uploadOnce(u *upload) error {
	memBase, diskBase, err := bases(u.spec, u.pm, u.pd)
	if err != nil {
		return err
	}
	var hm, hd *header.Header
	if u.spec.MemSize > 0 {
		dirty, zero := u.spec.MemDirty, u.spec.MemZero
		if lazyDiff(u.spec) {
			// A page both zeroed and dirtied was written after it was given
			// back: it is in neither set once dropped as unchanged.
			zero = subtractExtents(unionExtents(zero), unionExtents(dirty))
			start := time.Now()
			if dirty, err = n.changedPages(u.ctx, filepath.Join(u.dir, snapMem), memBase, dirty); err != nil {
				return fmt.Errorf("compare the diff with its parent: %w", err)
			}
			n.log.Info("dropped the pages a lazily restored VM only read", "snapshot", u.id, "logged_mib", extentsBytes(u.spec.MemDirty)>>20,
				"changed_mib", extentsBytes(dirty)>>20, "took", time.Since(start).Round(time.Millisecond))
		}
		l, err := n.store.NewLayer(snapstore.LayerSpec{Layer: u.spec.Layer, Kind: snapstore.Memory, Path: filepath.Join(u.dir, snapMem),
			Size: u.spec.MemSize, Dirty: dirty, Zero: zero})
		if err != nil {
			return err
		}
		if hm, err = l.Upload(u.ctx, memBase); err != nil {
			return err
		}
	}
	l, err := n.store.NewLayer(snapstore.LayerSpec{Layer: u.spec.Layer, Kind: snapstore.Disk, Path: filepath.Join(u.dir, firecracker.DiskFile),
		Size: u.spec.DiskSize, Dirty: u.spec.DiskDirty})
	if err != nil {
		return err
	}
	if hd, err = l.Upload(u.ctx, diskBase); err != nil {
		return err
	}
	u.dur = [2]*header.Header{hm, hd}
	meta := storedMeta{Snapshot: u.snap, BaseImage: u.spec.BaseImage}
	meta.Snapshot.Stored = false
	if u.snap.Memory {
		if meta.VMState, err = os.ReadFile(filepath.Join(u.dir, snapState)); err != nil {
			return err
		}
	}
	mb, _ := json.Marshal(meta)

	u.mu.Lock()
	defer u.mu.Unlock()
	if u.deleted {
		return nil // its layers serve the children waiting for them; the snapshot itself is gone
	}
	if err := n.store.PutSnapshot(u.ctx, &snapstore.Snapshot{ID: u.id, Memory: hm, Disk: hd, Meta: mb}); err != nil {
		return err
	}
	// The local copy keeps the durable headers, which a child's upload reads.
	for i, h := range u.dur {
		if h == nil {
			continue
		}
		b, err := header.Serialize(h)
		if err == nil {
			err = writeFileAtomic(filepath.Join(u.dir, []string{memHeader, diskHeader}[i]), b)
		}
		if err != nil {
			n.log.Warn("could not keep a stored snapshot's header locally", "snapshot", u.id, "err", err)
		}
	}
	u.snap.Stored = true
	if err := writeJSONFile(filepath.Join(u.dir, snapFile), u.snap); err != nil {
		n.log.Warn("could not mark a local snapshot stored", "snapshot", u.id, "err", err)
	}
	n.events.publish(event{nodeapi.EventSnapshot, u.snap})
	return nil
}

func (n *Nodelet) finishUpload(u *upload) {
	if u.err != nil && !u.isDeleted() {
		n.log.Error("snapshot not stored: it stays on this node only", "snapshot", u.id, "err", u.err)
	}
	n.upMu.Lock()
	if n.uploads[u.id] == u {
		delete(n.uploads, u.id)
	}
	n.upMu.Unlock()
	close(u.done)
	if u.isDeleted() {
		os.RemoveAll(u.dir)
	}
}

func (u *upload) isDeleted() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.deleted
}

// hasDependents says whether an upload still running waits for u.
func (n *Nodelet) hasDependents(u *upload) bool {
	n.upMu.Lock()
	defer n.upMu.Unlock()
	for _, o := range n.uploads {
		if o.parent == u {
			return true
		}
	}
	return false
}

// WaitUploads waits for every upload in flight, or ctx.
func (n *Nodelet) WaitUploads(ctx context.Context) error {
	done := make(chan struct{})
	go func() { n.upWG.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		n.upMu.Lock()
		left := len(n.uploads)
		n.upMu.Unlock()
		return fmt.Errorf("%d uploads still running: %w", left, ctx.Err())
	}
}

// GetSnapshot is snapshot id, from the local copy or the store's.
func (n *Nodelet) GetSnapshot(ctx context.Context, id string) (nodeapi.Snapshot, error) {
	if snapstore.ValidSnapshotID(id) != nil {
		return nodeapi.Snapshot{}, notFound("snapshot", id)
	}
	n.snapMu.RLock()
	s, _, err := n.localSnap(id)
	n.snapMu.RUnlock()
	if code(err) != "not_found" {
		return s, err
	}
	_, meta, err := n.storeSnapshot(ctx, id)
	return meta.Snapshot, err
}

func code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// Snapshots lists the snapshots on this node, by ID.
func (n *Nodelet) Snapshots() []nodeapi.Snapshot {
	n.snapMu.RLock()
	defer n.snapMu.RUnlock()
	out := []nodeapi.Snapshot{}
	entries, _ := os.ReadDir(n.snapsDir())
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if s, _, err := n.localSnap(e.Name()); err == nil {
			out = append(out, s)
		}
	}
	return out
}

// needs names the VM on this node that needs snapshot id to stay: paused on
// it, or running with its memory served from it. The caller holds n.mu.
func (n *Nodelet) needs(id string) string {
	for _, v := range n.vms {
		if v.ready && !v.gone && ((v.rec.State == nodeapi.VMPaused && v.rec.Snapshot == id) || v.serving == id) {
			return v.id
		}
	}
	return ""
}

// DeleteSnapshot deletes snapshot id's local copy and the store's
// (docs/NODE.md): not its layers. An upload in flight is cancelled first,
// unless the upload of a child waits for its layers: then only the snapshot
// itself (its headers and meta) is never written.
func (n *Nodelet) DeleteSnapshot(ctx context.Context, id string) error {
	if snapstore.ValidSnapshotID(id) != nil {
		return notFound("snapshot", id)
	}
	n.snapMu.Lock()
	defer n.snapMu.Unlock()
	n.mu.Lock()
	vm := n.needs(id)
	n.mu.Unlock()
	if vm != "" {
		return conflict("VM %s on this node needs snapshot %s", vm, id)
	}
	dir := n.snapDir(id)
	local := fileExistsAt(filepath.Join(dir, snapFile))
	u := n.inflight(id)
	if !local && u == nil {
		stored, err := n.store.HasSnapshot(ctx, id)
		if err != nil {
			return fmt.Errorf("snapshot %s in the store: %w", id, err)
		}
		if !stored {
			return notFound("snapshot", id)
		}
	}
	if u != nil {
		u.mu.Lock()
		u.deleted = true
		u.mu.Unlock()
		if !n.hasDependents(u) {
			u.cancel()
			<-u.done
			// Its parent may be a deleted snapshot whose upload ran on only
			// for it: stop that too, before answering, or it writes a layer
			// after loam-api has collected it (a snapshot's own layer is
			// unreferenced once its record and its children's are gone).
			for p := u.parent; p != nil && p.isDeleted() && !n.hasDependents(p); p = p.parent {
				p.cancel()
				<-p.done
			}
		}
	}
	if err := n.store.DeleteSnapshot(ctx, id); err != nil {
		return fmt.Errorf("delete snapshot %s from the store: %w", id, err)
	}
	if err := os.Remove(filepath.Join(dir, snapFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if u == nil || isClosed(u.done) {
		os.RemoveAll(dir)
	}
	n.log.Info("snapshot deleted", "snapshot", id, "local", local)
	return nil
}

func isClosed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// DeleteLayer deletes a layer from the store (docs/NODE.md, DELETE /v1/layers).
func (n *Nodelet) DeleteLayer(ctx context.Context, layer string) error {
	id, err := header.ParseLayerID(layer)
	if err != nil || !id.IsStored() {
		return notFound("layer", layer)
	}
	ok, err := n.store.HasLayer(ctx, id)
	if err != nil {
		return fmt.Errorf("layer %s in the store: %w", layer, err)
	}
	if !ok {
		return errorf(404, "not_found", "no layer %s in the store", layer)
	}
	if err := n.store.DeleteLayer(ctx, id); err != nil {
		return fmt.Errorf("delete layer %s: %w", layer, err)
	}
	n.log.Info("layer deleted", "layer", layer)
	return nil
}

// loadSnapshots goes over the local snapshots at start: it clears what a
// previous nodelet left half done, gives a snapshot from before phase 2 a
// layer of its own (the whole of its files, over nothing), and resumes the
// uploads of the snapshots not stored yet, parents first.
func (n *Nodelet) loadSnapshots() error {
	entries, err := os.ReadDir(n.snapsDir())
	if err != nil {
		return err
	}
	type local struct {
		snap nodeapi.Snapshot
		spec layerSpec
	}
	pending := map[string]local{}
	for _, e := range entries {
		dir := filepath.Join(n.snapsDir(), e.Name())
		if strings.HasPrefix(e.Name(), ".") || !fileExistsAt(filepath.Join(dir, snapFile)) {
			// A staging directory, or one deleted while its upload ran.
			os.RemoveAll(dir)
			continue
		}
		snap, spec, err := n.localSnap(e.Name())
		if errors.Is(err, os.ErrNotExist) {
			if spec, err = n.adoptLegacy(dir, &snap); err != nil {
				n.log.Error("a snapshot from before phase 2 can't be given a layer; it stays local", "snapshot", e.Name(), "err", err)
				continue
			}
		} else if err != nil {
			n.log.Warn("ignoring a snapshot directory without a readable record", "dir", dir, "err", err)
			continue
		}
		if !snap.Stored {
			pending[snap.ID] = local{snap, spec}
		}
	}
	var start func(id string)
	started := map[string]bool{}
	start = func(id string) {
		l, ok := pending[id]
		if !ok || started[id] {
			return
		}
		started[id] = true
		start(l.snap.Parent)
		u, err := n.newUpload(context.Background(), n.snapDir(id), l.snap, l.spec)
		u.snap = l.snap
		if err != nil {
			u.err = err
		}
		n.log.Info("resuming a snapshot's upload", "snapshot", id)
		n.startUpload(u)
	}
	for id := range pending {
		start(id)
	}
	return nil
}

// adoptLegacy makes a phase 1 snapshot (its files, no layer.json) one that
// can be stored: one layer with all of it, over nothing.
func (n *Nodelet) adoptLegacy(dir string, snap *nodeapi.Snapshot) (layerSpec, error) {
	b, err := os.ReadFile(filepath.Join(dir, snapFile))
	if err != nil {
		return layerSpec{}, err
	}
	if err := json.Unmarshal(b, snap); err != nil {
		return layerSpec{}, err
	}
	spec := layerSpec{Layer: header.NewLayerID(), DiskOver: overZero}
	disk := filepath.Join(dir, firecracker.DiskFile)
	if spec.DiskSize, err = fileSize(disk); err != nil {
		return spec, err
	}
	if spec.DiskDirty, err = snapstore.DataExtents(disk); err != nil {
		return spec, err
	}
	if snap.Memory {
		mem := filepath.Join(dir, snapMem)
		spec.MemFull, spec.MemOver, spec.MemSize = true, overZero, int64(snap.Resources.MemoryMiB)<<20
		if spec.MemDirty, err = snapstore.DataExtents(mem); err != nil {
			return spec, err
		}
	}
	snap.Parent, snap.Layers, snap.Stored = "", []string{spec.Layer.String()}, false
	if err := writeJSONFile(filepath.Join(dir, specFile), spec); err != nil {
		return spec, err
	}
	n.log.Info("a snapshot from before phase 2 gets a layer of its own", "snapshot", snap.ID, "layer", spec.Layer)
	return spec, writeJSONFile(filepath.Join(dir, snapFile), snap)
}

// ReconcileStored deletes the local copies of stored snapshots that the
// store no longer has: deleted through another node while this one was away
// (docs/NODE.md, "Local copies"). One a VM here is paused on stays.
func (n *Nodelet) ReconcileStored(ctx context.Context) error {
	ids, err := n.store.ListSnapshots(ctx)
	if err != nil {
		return err
	}
	inStore := map[string]bool{}
	for _, id := range ids {
		inStore[id] = true
	}
	for _, s := range n.Snapshots() {
		if !s.Stored || inStore[s.ID] {
			continue
		}
		n.snapMu.Lock()
		n.mu.Lock()
		vm := n.needs(s.ID)
		n.mu.Unlock()
		if vm == "" && n.inflight(s.ID) == nil {
			os.Remove(filepath.Join(n.snapDir(s.ID), snapFile))
			os.RemoveAll(n.snapDir(s.ID))
			n.log.Info("deleted the local copy of a snapshot the store no longer has", "snapshot", s.ID)
		}
		n.snapMu.Unlock()
	}
	return nil
}
