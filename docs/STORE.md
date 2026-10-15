# The snapshot store

`internal/snapstore` keeps every snapshot a nodelet takes in object storage, so a sandbox can
wake on any node (`docs/NODE.md`, "Phase 2: the snapshot store"). It is E2B's snapshot storage
(github.com/e2b-dev/infra at cf536c2, Apache-2.0, `internal/snapstore/LICENSE-e2b`) copied and
trimmed, as the phase 0 spike recommended (`docs/spikes/e2b-snapshot-store.md`): the header
format, framed compression, the S3 and filesystem backends, the chunked read path with its two
node caches, the copy-on-write overlay and the UFFD serve loop. Every copied file says where it
came from and what changed.

Vocabulary: E2B's *build* is loam's **layer** (the blocks one snapshot generation changed in a
file, one object per file); a **snapshot** is a header per file plus `meta.json`.

| Package | Holds |
|---|---|
| `snapstore` | The API the nodelet calls: `Store`, snapshots, layers, `File`, `MaterializeDisk` |
| `snapstore/header` | The header format: mapping, layer table, frame tables, diffs, V5 serialization |
| `snapstore/storage` | `Backend` (S3, filesystem), store URLs, framed compression, the chunk directory |
| `snapstore/block` | The per-layer sparse cache and chunker, the copy-on-write overlay |
| `snapstore/uffd` | Firecracker's UFFD handshake and the page-fault serve loop |

## Objects in the bucket

```
layers/<layer>/memory[.zst|.lz4]   a layer's memory blocks, packed in order, in frames
layers/<layer>/disk[.zst|.lz4]     its disk blocks
snapshots/<id>/memory.header       the memory file's header (absent for a disk-only snapshot)
snapshots/<id>/disk.header         the disk's header
snapshots/<id>/meta.json           the nodelet's metadata, verbatim; written last, deleted first
```

A layer ID is a random UUID, shared by a generation's memory and disk layers. A snapshot ID is
loam-api's (`<sandbox>.@<n>`): letters, digits and `._@-`, at most 200 characters. A layer with
no non-zero blocks has no object. The suffix is the layer's compression, which its frame table in
every header that references it also records.

**Ordering.** Layer objects are uploaded first, then the headers, then `meta.json`: a snapshot is
stored exactly when its `meta.json` is, so a reader never finds half of one. Deletion is the
reverse: `meta.json` first.

## The header

One per file per snapshot, complete on its own: reading a snapshot never walks a chain of
parents.

```
[Metadata: version=5, block size, size, generation, layer, base layer]   64 bytes, little-endian
[flags: 1 byte, always 0] [uint32 uncompressed block size] [LZ4(block)]

block = layers section:  count, then per layer { id, uncompressed size, SHA-256, frame table }
        mapping section: layer-id table, then the non-empty ranges as varint columns
                         (offset deltas, lengths, storage-offset deltas, table indices), in pages
```

The mapping sends every 4 KiB page of the file to one of: a **stored layer** at an offset in its
object; **Empty**, zeros stored nowhere (the gaps between written ranges); or **Base**, unchanged
from the file's base, at the same offset. Only a disk has a base: its template image, which the
store never holds, because every node builds or caches it. Base is loam's addition to E2B's
format (a reserved all-ones layer ID).

Each layer entry carries the layer's **frame table**: its object is cut into 2 MiB (uncompressed)
frames, each compressed independently (zstd level 1 by default; lz4; or none, with no table).
A read of any page fetches one frame with a ranged GET and decodes it. A header's frame tables are
trimmed to the frames its mapping references. A generation's header is its parent's mapping with
the new layer's ranges and the new zero ranges merged over it, and adjacent ranges of one layer
joined where their storage is contiguous. Each generation adds what it changed; nothing gets
longer to read.

## The API

```go
// Opening. A URL for Garage in the cluster:
//   s3://loam-snapshots?endpoint=http://s3.garage.svc.cluster.local:3900&s3ForcePathStyle=true&region=garage
// with AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY in the environment (region is Garage's s3_region).
// file:///dir works too (tests, a single node).
func Open(url string, opts Options) (*Store, error)
func New(b storage.Backend, opts Options) (*Store, error)
type Options struct {
	CacheDir    string                  // node-local: CacheDir/chunks (shared) and CacheDir/layers/<pid>-* (this process's)
	Compression *storage.CompressConfig // nil: zstd level 1, 2 MiB frames
}
func (s *Store) Close() error

// Writing a generation of one file.
type Extent struct{ Off, Len int64 }
func DataExtents(path string) ([]Extent, error)              // SEEK_DATA/SEEK_HOLE
func BitmapExtents(b *header.Bitmap, unit int64) []Extent    // e.g. a UFFD handler's Zeroed pages
type LayerSpec struct {
	Layer header.LayerID // header.NewLayerID(), one per generation, shared by memory and disk
	Kind  Kind           // Memory or Disk
	Path  string         // the file, each block at its own offset
	Size  int64          // the file's size; the parent's
	Dirty []Extent       // the blocks to take from Path; every other block inherits from the parent
	Zero  []Extent       // blocks known to read as zeros now, not read; Dirty wins
}
func (s *Store) NewLayer(spec LayerSpec) (*PendingLayer, error)
func (p *PendingLayer) Provisional(parent *header.Header) (*header.Header, error)       // no I/O
func (p *PendingLayer) Upload(ctx context.Context, parent *header.Header) (*header.Header, error)

// Generation 0, the parent of a file's first layer.
func header.NewZero(size, blockSize uint64) (*header.Header, error) // memory: all zeros
func header.NewBase(size, blockSize uint64) (*header.Header, error) // disk: all template

// Snapshots.
type Snapshot struct {
	ID     string
	Memory *header.Header // nil for a disk-only snapshot
	Disk   *header.Header
	Meta   []byte         // meta.json, the nodelet's own
}
func (sn *Snapshot) Layers() []header.LayerID // every stored layer referenced, its own last
func (s *Store) PutSnapshot(ctx context.Context, sn *Snapshot) error
func (s *Store) GetSnapshot(ctx context.Context, id string) (*Snapshot, error) // ErrNotFound
func (s *Store) ListSnapshots(ctx context.Context) ([]string, error)
func (s *Store) DeleteSnapshot(ctx context.Context, id string) error           // not its layers
func (s *Store) ListLayers(ctx context.Context) ([]header.LayerID, error)
func (s *Store) DeleteLayer(ctx context.Context, id header.LayerID) error

// Reading.
func (s *Store) OpenFile(h *header.Header, kind Kind, base string) (*File, error) // base: template path, or ""
func (f *File) ReadAt(ctx context.Context, p []byte, off int64) (int, error)
func (f *File) SwapHeader(h *header.Header) error // provisional -> durable
func (f *File) Size() int64
func (f *File) Close() error
func (s *Store) MaterializeDisk(ctx context.Context, h *header.Header, dst string) error

// The node's chunk directory.
func (s *Store) Evict(maxAge time.Duration, maxBytes int64) (removed int, left int64, err error)
func (s *Store) Stats() Stats // backend fetches, fetched bytes, chunk-directory hits

// Lazy memory (package uffd), in a helper process.
func uffd.Accept(ctx context.Context, l *net.UnixListener) (*uffd.Handshake, error) // {FD, Regions}
func uffd.New(fd int, regions []uffd.Region, src uffd.PageSource) (*uffd.Handler, error) // src: a *File
func (h *Handler) Serve(ctx context.Context) error
func (h *Handler) Prefetch(ctx context.Context, offsets []int64, workers int)
func (h *Handler) Faults() []int64          // pages in first-touch order: a prefetch list
func (h *Handler) Zeroed() *header.Bitmap   // pages that read as zeros now (balloon), in PageSize() units
func (h *Handler) Close() error
```

### How the nodelet uses it

**Pause (or checkpoint) of a VM that booted fresh.** Take a full snapshot; the memory file is
sparse (holes for zero pages). `NewLayer{Layer: L, Kind: Memory, Path: mem, Size, Dirty:
DataExtents(mem)}` over `header.NewZero(size, 4096)`, and `NewLayer{Layer: L, Kind: Disk, Path:
disk, Size, Dirty: <the ranges where the disk and a reflink clone of the template map to different
physical blocks (FIEMAP on both)>}` over `header.NewBase(size, 4096)`. (The `SHARED` flag alone
can't tell: a checkpoint's clone of the disk shares the blocks the guest wrote too.) Answer the pause once the files are on local disk; then `Upload`
both, `PutSnapshot`, and report the snapshot stored (`Snapshot.layers` is `sn.Layers()`).

**Pause of a VM restored from the store.** Take a *diff* snapshot: its memory file holds only the
pages dirtied since the restore. `Dirty: DataExtents(diffMem)`, `Zero: BitmapExtents(handler.Zeroed(),
handler.PageSize())` (ask the UFFD helper for it before stopping it), parent: the snapshot's memory
header. The disk is the same as above, over the snapshot's disk header, with the extents changed
since the restore. Holes in a diff snapshot mean *unchanged*, never zero; that is why the dirty
set is the data extents and the parent supplies the rest.

Only the first snapshot after a restore may take `Zeroed()` whole. A page the handler installed
as zeros and the guest wrote afterwards stays in `Zeroed()` (the handler never sees the write),
and once an earlier snapshot of the same run has recorded it, a later snapshot's dirty log no
longer has it to win: so a later snapshot passes only the pages zeroed since the previous one
(the nodelet keeps the last answer and subtracts it).

**Stop.** A disk-only snapshot: `PutSnapshot` with `Memory: nil`.

**Waking here, before the upload is done.** `p.Provisional(parent)` returns a header that reads the
new blocks from `Path` at once; `OpenFile` it, and `SwapHeader` the durable header in when `Upload`
returns. The local file must stay until then. A provisional header is never stored, and `Upload`
refuses a provisional parent: upload a generation's parent before the generation (wait for the
previous upload before uploading the next). In practice a wake on the same node can simply use the
local files, as phase 1 does.

**Waking elsewhere.** `GetSnapshot`. Disk: reflink-clone the template image (built or from the
image cache) to the VM's disk path and `MaterializeDisk(sn.Disk, path)`: it writes only the ranges
that differ from the template (layer blocks fetched, zero ranges punched). Memory: start the UFFD
helper for the snapshot, then Firecracker with `mem_backend: {"backend_type": "Uffd",
"backend_path": <the helper's socket>}`.

**Deleting.** `DELETE /v1/snapshots/{id}` is `DeleteSnapshot`; `DELETE /v1/layers/{layer}` is
`DeleteLayer`. See the GC rules below.

**Eviction.** Hourly, `Evict(maxAge, maxBytes)`, like the image cache: chunks unused for `maxAge`
go, then the least recently used until the directory is within `maxBytes`; a chunk used in the last
hour is never evicted (another process may be about to open it).

### The UFFD helper process

The handler must run in a process other than the one whose memory it serves: a Go thread blocked
in a fault its own process should serve can't be stopped for the garbage collector, and the
process deadlocks. Firecracker owns the memory, so the handler could be in the nodelet, but the
nodelet must not die with a bad fault either: one helper per restored VM (a `loam-nodelet`
subcommand, the nodelet workstream's), shaped like this:

```go
s, _ := snapstore.Open(storeURL, snapstore.Options{CacheDir: dataDir + "/store"})  // same CacheDir as the nodelet
sn, _ := s.GetSnapshot(ctx, id)
f, _ := s.OpenFile(sn.Memory, snapstore.Memory, "")
l, _ := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"}) // short path: sun_path is 108 bytes
// ... tell the nodelet it is listening; it starts Firecracker's /snapshot/load ...
hs, _ := uffd.Accept(ctx, l)
h, _ := uffd.New(hs.FD, hs.Regions, f)
go h.Prefetch(ctx, prefetchList, 4) // optional: the template's recorded Faults()
err := h.Serve(ctx)                 // until told to stop (SIGTERM), or a page can't be served
```

The handshake is Firecracker's: on `/snapshot/load` it connects to the socket and sends, in one
message, the guest memory regions as JSON (`base_host_virt_addr`, `size`, `offset`, `page_size`
and the deprecated `page_size_kib`, both in bytes) and its userfaultfd as `SCM_RIGHTS`. The load
returns only once the handler is serving. `Serve` returning an error means a guest thread is stuck
on a page that could not be fetched (after retries): stop the VM. Nothing tells the helper that
Firecracker exited (the uffd stays open while the helper holds it), so the nodelet stops the helper
when it stops the VM, after asking it for `Zeroed()` if a snapshot follows. The helper shares the
node's chunk directory, so a second restore of the same layers on the node fetches nothing.

### Privileges

- **Firecracker** creates the userfaultfd, and it must catch KVM's faults, which an unprivileged
  (`UFFD_USER_MODE_ONLY`) one can't. In the nodelet pod the `userfaultfd(2)` syscall is refused by
  containerd's RuntimeDefault seccomp profile whatever the capabilities, so the pod gets the device
  `/dev/userfaultfd` (a device plugin, the infra workstream's), whose `USERFAULTFD_IOC_NEW` ioctl
  makes a full userfaultfd under RuntimeDefault; Firecracker 1.17 tries the device first.
- **The helper** needs nothing: it receives the fd. Nothing in `snapstore` calls the
  `userfaultfd(2)` syscall; `uffd.Open` (for tests) uses the device.
- Upstream Firecracker registers guest memory for *missing* faults only (no write-protect), so the
  handler installs pages plainly (`UFFDIO_COPY`, `UFFDIO_ZEROPAGE`); E2B's write-protect paths
  belong to its Firecracker fork and are not carried over. With a balloon, Firecracker enables
  `UFFD_EVENT_REMOVE`, and the handler records the pages the guest gave back so they read as zeros.

### Firecracker settings

- Boot and restore with `track_dirty_pages: true` (machine config; and on `/snapshot/load`), so a
  later `/snapshot/create` with `snapshot_type: "Diff"` writes only the pages dirtied since the
  restore. Pages the handler installed are not dirty until the guest touches them, but then they
are even if it only read them: the handler's copies are writable pages, which KVM maps writable at
the first access and logs dirty (measured 2026-10-11: reading 256 MiB after a lazy restore put
292 MiB in the next diff). So the nodelet compares a lazily restored VM's diff pages with its
parent's (already in the chunk directory) and uploads only those that changed, reporting the
parent's layers in `layers` as well, since dropping pages may keep more of them referenced. A
restore from a whole local memory file (mapped privately) has no such problem.
- Restore with `mem_backend: {"backend_type": "Uffd", "backend_path": <socket>}`.
- 4 KiB pages; with hugepages the regions say 2 MiB, and the handler installs 2 MiB pages (the
  header still maps 4 KiB pages; the memory file's layout is the same).

## Caches on a node

Two tiers, as E2B has them: a per-process **layer cache** (`CacheDir/layers/<pid>-*`), one sparse
mmapped file the size of each layer a process reads, filled a frame at a time, with concurrent
reads of one frame sharing one fetch; and the node's **chunk directory** (`CacheDir/chunks/<object
key>/<offset>-<length>.frm`), every compressed frame any process fetched, kept across restarts
and verified on use (a frame that no longer decodes is dropped and fetched again). The
chunk directory belongs on the data volume (`--data`); each file's mtime is refreshed on use
(at most once a minute) for eviction. A process's layer caches go when it closes the store, and a
dead process's when the next store opens.

## Garbage collection

- **A snapshot's layers outlive it.** `DeleteSnapshot` deletes its `meta.json` and headers, never
  layers. A layer is referenced by every snapshot whose headers name it (`Snapshot.Layers()`), not
  only by the snapshot that wrote it: a child generation reads its ancestors' layers directly.
- **loam-api decides.** It records each stored snapshot's `layers`, counts references per layer,
  and calls `DELETE /v1/layers/{layer}` (any nodelet; they share the store) when the count drops to
  zero. Deleting a layer a stored snapshot still references breaks that snapshot.
- **Uploads in flight.** A layer object exists before the snapshot that references it is stored
  (the upload order above). An orphan sweep (`ListLayers` against loam-api's records) must spare
  layers younger than the longest upload (say a day), and a snapshot that never got its
  `meta.json` (`ListSnapshots` doesn't list it) leaves headers that a sweep of `snapshots/` may
  delete after the same grace.
- **Chunks are a cache.** `DeleteLayer` also drops the layer's chunks on the node that ran it;
  other nodes' chunks of it age out through `Evict`.

## Tests

`go test ./internal/snapstore/...` needs no root, KVM or network: header building, merging and
multi-generation diffs against a model; compressed round trips (zstd, lz4, none) through the
filesystem backend and a fake S3 server; the SigV4 signer against AWS's published example;
provisional headers; cold and warm chunk directories; eviction; a corrupt cached frame; disk
materialization; the overlay's exported diff as the next generation; and the UFFD handler serving
a child process that plays Firecracker (the test binary itself), which skips where the kernel
refuses an unprivileged userfaultfd (no `/dev/userfaultfd` access and a seccomp that refuses the
syscall, as in a default container).

`-tags garage` runs the backend and store round trips against a real, empty bucket. Against
Garage running in a cluster, with a temporary bucket and key (and nothing else touched):

```sh
G="kubectl -n garage exec garage-3 -- /garage"
$G bucket create loam-store-test
$G key create loam-store-test-key              # note the key ID and secret
$G bucket allow --read --write --owner loam-store-test --key loam-store-test-key
kubectl -n garage port-forward svc/s3 39000:3900 &
AWS_ACCESS_KEY_ID=GK... AWS_SECRET_ACCESS_KEY=... \
LOAM_STORE_URL='s3://loam-store-test?endpoint=http://127.0.0.1:39000&s3ForcePathStyle=true&region=garage' \
  go test -p 1 -tags garage -count=1 -run Garage ./internal/snapstore/...
$G bucket delete --yes loam-store-test          # the tests leave it empty
$G key delete --yes loam-store-test-key
```

## Dependencies

- `github.com/klauspost/compress` (zstd): the frame codec. Already in the module graph (via
  go-containerregistry).
- `github.com/pierrec/lz4/v4`: the header block's codec (part of the format) and the lz4 frame
  codec. No dependencies of its own.
- `golang.org/x/sync` (errgroup): the upload pipeline's bounded workers. Already in the graph.
- `golang.org/x/sys`: mmap, `copy_file_range`, `fallocate`, ioctls. Already a direct dependency.

Not taken from E2B: the AWS SDK (a dozen modules for eight S3 requests: the S3 client is ~400
lines of stdlib with SigV4), roaring bitmaps (a dense bitset is enough for loam's file sizes),
`google/uuid`, mmap-go, OTel, LaunchDarkly, zap, cgo (the userfaultfd constants are written out).
