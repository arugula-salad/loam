// Package snapstore is loam's snapshot store: every snapshot a nodelet takes, durable in object
// storage, so a sandbox can wake on any node. The format, the read path and the UFFD handler are
// E2B's (github.com/e2b-dev/infra at cf536c2, Apache-2.0, see LICENSE-e2b), copied and trimmed
// into the sub-packages: header (the per-snapshot file headers), storage (S3 and filesystem
// backends, framed compression, the node-local chunk directory), block (the per-layer sparse
// cache, copy-on-write overlay) and uffd (lazy memory for Firecracker). This package is what the
// nodelet calls; docs/STORE.md describes it, the object layout and the garbage collection rules.
package snapstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/arugula-salad/loam/internal/snapstore/header"
	"github.com/arugula-salad/loam/internal/snapstore/storage"
)

// Kind is one of a snapshot's files.
type Kind string

const (
	Memory Kind = "memory"
	Disk   Kind = "disk"
)

func (k Kind) valid() bool { return k == Memory || k == Disk }

// ErrNotFound is returned for a snapshot that isn't stored (or not completely: its meta.json is
// written last).
var ErrNotFound = errors.New("snapshot not found")

// Options configure a Store.
type Options struct {
	// CacheDir is node-local scratch: the chunk directory (CacheDir/chunks), shared by every
	// process on the node that reads the store, and each process's per-layer cache files
	// (CacheDir/layers/<pid>-*). Required.
	CacheDir string
	// Compression is how new layers are written; the zero value is
	// storage.DefaultCompressConfig (zstd level 1, 2 MiB frames).
	Compression *storage.CompressConfig
}

// Store is a snapshot store opened by one process.
type Store struct {
	b        storage.Backend
	chunks   *storage.ChunkDir
	stats    *storage.Stats
	layerDir string
	compress storage.CompressConfig

	mu     sync.Mutex
	layers map[layerKey]*layerReader
}

// Open opens the store a URL names (see storage.ParseURL), such as
// s3://loam-snapshots?endpoint=http://minio.minio.svc.cluster.local:9000&s3ForcePathStyle=true&region=us-east-1
// with the key in AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY.
func Open(url string, opts Options) (*Store, error) {
	b, err := storage.Open(url)
	if err != nil {
		return nil, err
	}
	return New(b, opts)
}

// New opens a store on a backend.
func New(b storage.Backend, opts Options) (*Store, error) {
	if opts.CacheDir == "" {
		return nil, errors.New("snapstore: a cache directory is required")
	}
	layerDir, err := newLayerDir(filepath.Join(opts.CacheDir, "layers"))
	if err != nil {
		return nil, err
	}
	cfg := storage.DefaultCompressConfig()
	if opts.Compression != nil {
		cfg = *opts.Compression
	}
	stats := &storage.Stats{}
	return &Store{
		b:        b,
		chunks:   storage.NewChunkDir(filepath.Join(opts.CacheDir, "chunks"), b, stats),
		stats:    stats,
		layerDir: layerDir,
		compress: cfg,
		layers:   map[layerKey]*layerReader{},
	}, nil
}

// Stats are the store's read counters.
type Stats struct {
	// Fetches and FetchedBytes are ranged reads from the backend.
	Fetches, FetchedBytes int64
	// ChunkHits are chunks served from the node's chunk directory.
	ChunkHits int64
}

func (s *Store) Stats() Stats {
	return Stats{s.stats.Fetches.Load(), s.stats.FetchedBytes.Load(), s.stats.ChunkHits.Load()}
}

// Evict drops chunks from the node's chunk directory: those unused for maxAge, then the least
// recently used until it holds at most maxBytes; never one used in the last hour. The nodelet
// runs it hourly, like its image cache's.
func (s *Store) Evict(maxAge time.Duration, maxBytes int64) (removed int, left int64, err error) {
	return s.chunks.Evict(maxAge, maxBytes)
}

// Close waits for chunk-directory writes in flight and removes this process's layer caches.
// Files must be closed first.
func (s *Store) Close() error {
	s.chunks.Wait()
	return os.RemoveAll(s.layerDir)
}

// newLayerDir makes this store's directory for layer cache files under root: <pid>-<random>.
// Several processes share a node's cache directory (the nodelet and its UFFD helpers), and each
// one's layer caches are its own, mapped into its memory; a directory whose process is gone is
// garbage, and the next store to open removes it.
func newLayerDir(root string) (string, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	if des, err := os.ReadDir(root); err == nil {
		for _, de := range des {
			pid, _, _ := strings.Cut(de.Name(), "-")
			if n, err := strconv.Atoi(pid); err == nil && n != os.Getpid() && syscall.Kill(n, 0) == syscall.ESRCH {
				os.RemoveAll(filepath.Join(root, de.Name()))
			}
		}
	}
	return os.MkdirTemp(root, strconv.Itoa(os.Getpid())+"-")
}

// The object layout (docs/STORE.md):
//
//	layers/<layer>/memory[.zst|.lz4]   a layer's memory blocks, packed, in frames
//	layers/<layer>/disk[.zst|.lz4]     its disk blocks
//	snapshots/<id>/memory.header       the memory file's header (absent for disk-only)
//	snapshots/<id>/disk.header         the disk's header
//	snapshots/<id>/meta.json           the nodelet's metadata; written last, deleted first

func layerKeyFor(id header.LayerID, kind Kind, ct header.CompressionType) string {
	return "layers/" + id.String() + "/" + string(kind) + ct.Suffix()
}

func snapshotKey(id, name string) string { return "snapshots/" + id + "/" + name }

var snapshotIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,199}$`)

// ValidSnapshotID reports whether id can name a snapshot: letters, digits and ._@-, not
// starting with a punctuation mark, at most 200 characters (loam-api's <sandbox>.@<n> fits).
func ValidSnapshotID(id string) error {
	if !snapshotIDPattern.MatchString(id) {
		return fmt.Errorf("snapshot id %q: want letters, digits and ._@- (not leading), at most 200", id)
	}
	return nil
}

// Snapshot is a stored snapshot: a header per file and the nodelet's meta.json.
type Snapshot struct {
	ID string
	// Memory is nil for a disk-only snapshot (a stop's).
	Memory *header.Header
	Disk   *header.Header
	// Meta is meta.json, the nodelet's own (template, resources, Firecracker's vmstate, ...),
	// stored and returned verbatim.
	Meta []byte
}

// Layers returns every stored layer the snapshot's headers reference, the snapshot's own last:
// what NODE.md's Snapshot.layers reports and what must outlive the snapshot.
func (sn *Snapshot) Layers() []header.LayerID {
	var ancestors, own []header.LayerID
	for _, h := range []*header.Header{sn.Memory, sn.Disk} {
		if h == nil {
			continue
		}
		for _, id := range h.StoredLayers() {
			switch {
			case id == h.Metadata.Layer && !containsID(own, id):
				own = append(own, id)
			case id != h.Metadata.Layer && !containsID(ancestors, id):
				ancestors = append(ancestors, id)
			}
		}
	}
	return append(removeIDs(ancestors, own), own...)
}

func containsID(ids []header.LayerID, id header.LayerID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func removeIDs(ids, drop []header.LayerID) []header.LayerID {
	out := ids[:0]
	for _, id := range ids {
		if !containsID(drop, id) {
			out = append(out, id)
		}
	}
	return out
}

// PutSnapshot stores a snapshot's headers, then its meta.json: a snapshot is stored once
// meta.json is, so a reader never finds half of one. The layers its headers reference must be
// uploaded already. Putting an ID again replaces the snapshot.
func (s *Store) PutSnapshot(ctx context.Context, sn *Snapshot) error {
	if err := ValidSnapshotID(sn.ID); err != nil {
		return err
	}
	if sn.Disk == nil {
		return errors.New("snapshot has no disk header")
	}
	if sn.Meta == nil {
		return errors.New("snapshot has no meta.json")
	}
	for kind, h := range map[Kind]*header.Header{Memory: sn.Memory, Disk: sn.Disk} {
		if h == nil {
			if err := s.b.Delete(ctx, snapshotKey(sn.ID, string(kind)+".header")); err != nil {
				return err
			}
			continue
		}
		data, err := header.Serialize(h)
		if err != nil {
			return fmt.Errorf("%s header: %w", kind, err)
		}
		if err := s.b.Put(ctx, snapshotKey(sn.ID, string(kind)+".header"), data); err != nil {
			return err
		}
	}
	return s.b.Put(ctx, snapshotKey(sn.ID, "meta.json"), sn.Meta)
}

// GetSnapshot reads a stored snapshot's meta.json and headers.
func (s *Store) GetSnapshot(ctx context.Context, id string) (*Snapshot, error) {
	if err := ValidSnapshotID(id); err != nil {
		return nil, err
	}
	meta, err := s.b.Get(ctx, snapshotKey(id, "meta.json"))
	if errors.Is(err, storage.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	sn := &Snapshot{ID: id, Meta: meta}
	for _, kind := range []Kind{Memory, Disk} {
		data, err := s.b.Get(ctx, snapshotKey(id, string(kind)+".header"))
		if errors.Is(err, storage.ErrNotExist) && kind == Memory {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("snapshot %s: %s header: %w", id, kind, err)
		}
		h, err := header.Deserialize(data)
		if err != nil {
			return nil, fmt.Errorf("snapshot %s: %s header: %w", id, kind, err)
		}
		if kind == Memory {
			sn.Memory = h
		} else {
			sn.Disk = h
		}
	}
	return sn, nil
}

// ListSnapshots returns the IDs of the stored snapshots.
func (s *Store) ListSnapshots(ctx context.Context) ([]string, error) {
	keys, err := s.b.List(ctx, "snapshots/")
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, k := range keys {
		if id, ok := strings.CutSuffix(strings.TrimPrefix(k, "snapshots/"), "/meta.json"); ok && !strings.Contains(id, "/") {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// DeleteSnapshot deletes a snapshot's meta.json, then its headers. Not its layers: other
// snapshots may reference them (see DeleteLayer). Deleting a snapshot that isn't there is not
// an error.
func (s *Store) DeleteSnapshot(ctx context.Context, id string) error {
	if err := ValidSnapshotID(id); err != nil {
		return err
	}
	for _, name := range []string{"meta.json", "memory.header", "disk.header"} {
		if err := s.b.Delete(ctx, snapshotKey(id, name)); err != nil {
			return err
		}
	}
	return nil
}

// ListLayers returns the IDs of the layers that have objects in the store.
func (s *Store) ListLayers(ctx context.Context) ([]header.LayerID, error) {
	keys, err := s.b.List(ctx, "layers/")
	if err != nil {
		return nil, err
	}
	var ids []header.LayerID
	for _, k := range keys {
		name, _, _ := strings.Cut(strings.TrimPrefix(k, "layers/"), "/")
		id, err := header.ParseLayerID(name)
		if err == nil && (len(ids) == 0 || ids[len(ids)-1] != id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// HasLayer reports whether a layer has objects in the store.
func (s *Store) HasLayer(ctx context.Context, id header.LayerID) (bool, error) {
	if !id.IsStored() {
		return false, nil
	}
	keys, err := s.b.List(ctx, "layers/"+id.String()+"/")
	return len(keys) > 0, err
}

// HasSnapshot reports whether a snapshot is stored (its meta.json is there), reading only that.
func (s *Store) HasSnapshot(ctx context.Context, id string) (bool, error) {
	if err := ValidSnapshotID(id); err != nil {
		return false, err
	}
	keys, err := s.b.List(ctx, snapshotKey(id, "meta.json"))
	for _, k := range keys {
		if k == snapshotKey(id, "meta.json") {
			return true, err
		}
	}
	return false, err
}

// DeleteLayer deletes a layer's objects from the store and its chunks from this node's chunk
// directory. Every snapshot that references it is broken by this; the caller (loam-api, by its
// reference counts) makes sure none does. Deleting a layer that isn't there is not an error.
func (s *Store) DeleteLayer(ctx context.Context, id header.LayerID) error {
	if !id.IsStored() {
		return fmt.Errorf("layer %s is not a stored layer", id)
	}
	prefix := "layers/" + id.String() + "/"
	keys, err := s.b.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := s.b.Delete(ctx, k); err != nil {
			return err
		}
	}
	return s.chunks.Remove(prefix)
}
