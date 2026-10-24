package snapstore

import (
	"bytes"
	"context"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/arugula-salad/loam/internal/snapstore/block"
	"github.com/arugula-salad/loam/internal/snapstore/header"
	"github.com/arugula-salad/loam/internal/snapstore/storage"
)

const page = header.PageSize

// newStore opens a store on dir's bucket with a cache of its own (a node).
func newStore(t *testing.T, bucket string, ct header.CompressionType) *Store {
	t.Helper()
	b, err := storage.NewFS(bucket)
	if err != nil {
		t.Fatal(err)
	}
	cfg := storage.DefaultCompressConfig()
	cfg.Type = ct
	cfg.FrameSize = 256 << 10
	cfg.PartSize = 1 << 20
	s, err := New(b, Options{CacheDir: t.TempDir(), Compression: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// memoryFile writes a full-snapshot-like memory file: compressible pages, some zero pages
// written as data, and holes.
func memoryFile(t *testing.T, r *rand.Rand, path string, size int) []byte {
	t.Helper()
	content := make([]byte, size)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(int64(size)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < size/page; i++ {
		switch i % 9 {
		case 0: // a hole
			continue
		case 1: // a page of zeros written out
		default:
			pat := make([]byte, 64)
			r.Read(pat)
			for j := 0; j < page; j += 64 {
				copy(content[i*page+j:], pat)
			}
		}
		if _, err := f.WriteAt(content[i*page:(i+1)*page], int64(i*page)); err != nil {
			t.Fatal(err)
		}
	}
	return content
}

// diffFile writes a Firecracker-diff-like file: only the pages changed, at their offsets, the
// rest holes. Some changes are to zeros. It returns the new content and the changed extents.
func diffFile(t *testing.T, r *rand.Rand, path string, cur []byte, writes int) ([]byte, []Extent) {
	t.Helper()
	next := bytes.Clone(cur)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.Truncate(int64(len(cur)))
	var es []Extent
	for range writes {
		start := r.Intn(len(cur)/page - 8)
		n := 1 + r.Intn(8)
		for j := start; j < start+n; j++ {
			b := next[j*page : (j+1)*page]
			if r.Intn(6) == 0 {
				clear(b)
			} else {
				r.Read(b)
			}
		}
		f.WriteAt(next[start*page:(start+n)*page], int64(start*page))
		es = append(es, Extent{int64(start * page), int64(n * page)})
	}
	return next, es
}

func readAll(t *testing.T, f *File) []byte {
	t.Helper()
	out := make([]byte, f.Size())
	// Odd-sized, unaligned reads exercise the mapping arithmetic.
	for off := 0; off < len(out); {
		n := min(len(out)-off, 300_001)
		if _, err := f.ReadAt(context.Background(), out[off:off+n], int64(off)); err != nil {
			t.Fatalf("read at %d: %v", off, err)
		}
		off += n
	}
	return out
}

func TestMemoryGenerationsRoundTrip(t *testing.T) {
	for _, ct := range []header.CompressionType{header.CompressionZstd, header.CompressionLZ4, header.CompressionNone} {
		t.Run(ct.String(), func(t *testing.T) {
			ctx := context.Background()
			const size = 16 << 20
			bucket, work := t.TempDir(), t.TempDir()
			r := rand.New(rand.NewSource(42))
			s := newStore(t, bucket, ct)

			// Generation 1: a full snapshot, its data extents the dirty set.
			mem := filepath.Join(work, "mem1")
			want := memoryFile(t, r, mem, size)
			dirty, err := DataExtents(mem)
			if err != nil {
				t.Fatal(err)
			}
			parent, _ := header.NewZero(size, page)
			layer := header.NewLayerID()
			pl, err := s.NewLayer(LayerSpec{Layer: layer, Kind: Memory, Path: mem, Size: size, Dirty: dirty})
			if err != nil {
				t.Fatal(err)
			}
			// The provisional header serves the file before anything is uploaded.
			ph, err := pl.Provisional(parent)
			if err != nil {
				t.Fatal(err)
			}
			pf, err := s.OpenFile(ph, Memory, "")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(readAll(t, pf), want) {
				t.Fatal("provisional read differs")
			}
			h1, err := pl.Upload(ctx, parent)
			if err != nil {
				t.Fatal(err)
			}
			if err := pf.SwapHeader(h1); err != nil {
				t.Fatal(err)
			}
			os.Remove(mem) // the durable header must not need the local file
			if !bytes.Equal(readAll(t, pf), want) {
				t.Fatal("read after swapping in the durable header differs")
			}
			pf.Close()
			disk, _ := header.NewBase(size, page)
			meta, _ := json.Marshal(map[string]any{"template": "t"})
			if err := s.PutSnapshot(ctx, &Snapshot{ID: "sb.@1", Memory: h1, Disk: disk, Meta: meta}); err != nil {
				t.Fatal(err)
			}

			// Generations 2..4: Firecracker diff snapshots over the stored parent.
			cur := want
			prev := "sb.@1"
			for gen := 2; gen <= 4; gen++ {
				sn, err := s.GetSnapshot(ctx, prev)
				if err != nil {
					t.Fatal(err)
				}
				diff := filepath.Join(work, "diff")
				var es []Extent
				cur, es = diffFile(t, r, diff, cur, 30)
				pl, err := s.NewLayer(LayerSpec{Layer: header.NewLayerID(), Kind: Memory, Path: diff, Size: size, Dirty: es})
				if err != nil {
					t.Fatal(err)
				}
				h, err := pl.Upload(ctx, sn.Memory)
				if err != nil {
					t.Fatal(err)
				}
				id := "sb.@" + string(rune('0'+gen))
				if err := s.PutSnapshot(ctx, &Snapshot{ID: id, Memory: h, Disk: disk, Meta: meta}); err != nil {
					t.Fatal(err)
				}
				prev = id
			}

			// Another node reads the newest snapshot cold, then a third shares its chunks.
			nodeCache := t.TempDir()
			b, _ := storage.NewFS(bucket)
			other, _ := New(b, Options{CacheDir: nodeCache})
			sn, err := other.GetSnapshot(ctx, prev)
			if err != nil {
				t.Fatal(err)
			}
			if sn.Memory.Metadata.Generation != 4 || !bytes.Equal(sn.Meta, meta) {
				t.Fatalf("snapshot %s: generation %d, meta %s", prev, sn.Memory.Metadata.Generation, sn.Meta)
			}
			if got := sn.Layers(); len(got) < 2 || got[len(got)-1] != sn.Memory.Metadata.Layer {
				t.Fatalf("layers %v: want the snapshot's own last", got)
			}
			f, err := other.OpenFile(sn.Memory, Memory, "")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(readAll(t, f), cur) {
				t.Fatal("cold read of generation 4 differs")
			}
			f.Close()
			other.Close()
			cold := other.Stats()
			if cold.Fetches == 0 {
				t.Fatal("a cold read fetched nothing")
			}
			third, _ := New(b, Options{CacheDir: nodeCache})
			t.Cleanup(func() { third.Close() })
			f, _ = third.OpenFile(sn.Memory, Memory, "")
			if !bytes.Equal(readAll(t, f), cur) {
				t.Fatal("warm read of generation 4 differs")
			}
			f.Close()
			if warm := third.Stats(); warm.Fetches != 0 || warm.ChunkHits == 0 {
				t.Fatalf("warm chunk directory: %+v (cold was %+v)", warm, cold)
			}
		})
	}
}

func TestZeroExtents(t *testing.T) {
	ctx := context.Background()
	const size = 1 << 20
	s := newStore(t, t.TempDir(), header.CompressionZstd)
	path := filepath.Join(t.TempDir(), "mem")
	data := bytes.Repeat([]byte{1}, size)
	os.WriteFile(path, data, 0o600)
	parent, _ := header.NewZero(size, page)
	pl, _ := s.NewLayer(LayerSpec{Layer: header.NewLayerID(), Kind: Memory, Path: path, Size: size, Dirty: []Extent{{0, size}}})
	h1, _ := pl.Upload(ctx, parent)
	// The guest gave back pages 2-5 (the handler's Zeroed), and wrote page 4 again since.
	zeroed := header.NewBitmap()
	zeroed.AddRange(2, 6)
	os.WriteFile(path, nil, 0o600)
	os.Truncate(path, size)
	f, _ := os.OpenFile(path, os.O_RDWR, 0)
	f.WriteAt(bytes.Repeat([]byte{9}, page), 4*page)
	f.Close()
	pl, _ = s.NewLayer(LayerSpec{Layer: header.NewLayerID(), Kind: Memory, Path: path, Size: size,
		Dirty: []Extent{{4 * page, page}}, Zero: BitmapExtents(zeroed, page)})
	h2, err := pl.Upload(ctx, h1)
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Clone(data)
	clear(want[2*page : 6*page])
	copy(want[4*page:], bytes.Repeat([]byte{9}, page))
	rf, _ := s.OpenFile(h2, Memory, "")
	defer rf.Close()
	if !bytes.Equal(readAll(t, rf), want) {
		t.Fatal("zeroed pages read wrong")
	}
}

func TestMaterializeDisk(t *testing.T) {
	ctx := context.Background()
	const size = 8 << 20
	r := rand.New(rand.NewSource(7))
	work := t.TempDir()
	s := newStore(t, t.TempDir(), header.CompressionZstd)
	base := make([]byte, size)
	r.Read(base)
	basePath := filepath.Join(work, "template.ext4")
	os.WriteFile(basePath, base, 0o600)

	// The VM's disk: the template with some blocks changed and some zeroed (TRIM).
	vm := bytes.Clone(base)
	var es []Extent
	for range 25 {
		start, n := r.Intn(size/page-16), 1+r.Intn(16)
		if r.Intn(3) == 0 {
			clear(vm[start*page : (start+n)*page])
		} else {
			r.Read(vm[start*page : (start+n)*page])
		}
		es = append(es, Extent{int64(start * page), int64(n * page)})
	}
	vmPath := filepath.Join(work, "vm.ext4")
	os.WriteFile(vmPath, vm, 0o600)
	parent, _ := header.NewBase(size, page)
	pl, err := s.NewLayer(LayerSpec{Layer: header.NewLayerID(), Kind: Disk, Path: vmPath, Size: size, Dirty: es})
	if err != nil {
		t.Fatal(err)
	}
	h, err := pl.Upload(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := header.Serialize(h)
	h, _ = header.Deserialize(data)

	// Another node: a clone of its own template, then the layers applied.
	dst := filepath.Join(work, "restored.ext4")
	os.WriteFile(dst, base, 0o600)
	if err := s.MaterializeDisk(ctx, h, dst); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, vm) {
		t.Fatal("materialized disk differs from the VM's")
	}
	// Reading through the header with the base file gives the same bytes.
	f, _ := s.OpenFile(h, Disk, basePath)
	defer f.Close()
	if !bytes.Equal(readAll(t, f), vm) {
		t.Fatal("read through the header differs")
	}
	// Without the base file, a Base range is an error, not zeros.
	nf, _ := s.OpenFile(h, Disk, "")
	defer nf.Close()
	if _, err := nf.ReadAt(ctx, make([]byte, size), 0); err == nil {
		t.Fatal("read a base range with no base file")
	}
	// A destination of the wrong size is refused.
	os.WriteFile(dst, base[:size/2], 0o600)
	if err := s.MaterializeDisk(ctx, h, dst); err == nil {
		t.Fatal("materialized onto a file of the wrong size")
	}
}

func TestOverlayExportsTheNextLayer(t *testing.T) {
	ctx := context.Background()
	const size = 4 << 20
	r := rand.New(rand.NewSource(3))
	work := t.TempDir()
	s := newStore(t, t.TempDir(), header.CompressionZstd)
	mem := filepath.Join(work, "mem")
	content := memoryFile(t, r, mem, size)
	dirty, _ := DataExtents(mem)
	parent, _ := header.NewZero(size, page)
	pl, _ := s.NewLayer(LayerSpec{Layer: header.NewLayerID(), Kind: Memory, Path: mem, Size: size, Dirty: dirty})
	h1, err := pl.Upload(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := s.OpenFile(h1, Memory, "")
	defer f.Close()
	c, err := block.NewCache(size, page, filepath.Join(work, "cow"))
	if err != nil {
		t.Fatal(err)
	}
	ov, err := block.NewOverlay(f, c)
	if err != nil {
		t.Fatal(err)
	}
	defer ov.Close()
	want := bytes.Clone(content)
	for range 40 {
		off := r.Intn(size/page-3) * page
		b := make([]byte, page*(1+r.Intn(3)))
		if r.Intn(4) > 0 {
			r.Read(b)
		}
		if _, err := ov.WriteAt(b, int64(off)); err != nil {
			t.Fatal(err)
		}
		copy(want[off:], b)
	}
	got := make([]byte, size)
	if _, err := ov.ReadAt(ctx, got, 0); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("overlay read differs (%v)", err)
	}
	// The overlay's writes, exported packed, are the next layer.
	out, _ := os.Create(filepath.Join(work, "cow.diff"))
	defer out.Close()
	dm, err := ov.ExportDiff(out)
	if err != nil {
		t.Fatal(err)
	}
	layer := header.NewLayerID()
	h2, err := dm.ToDiffHeader(h1, layer)
	if err != nil {
		t.Fatal(err)
	}
	// Store the exported object as the layer's (uncompressed) and read generation 2 back.
	packed, _ := os.ReadFile(out.Name())
	if err := s.b.Put(ctx, layerKeyFor(layer, Memory, header.CompressionNone), packed); err != nil {
		t.Fatal(err)
	}
	h2.SetLayer(layer, header.LayerData{Size: int64(len(packed))})
	f2, _ := s.OpenFile(h2, Memory, "")
	defer f2.Close()
	if !bytes.Equal(readAll(t, f2), want) {
		t.Fatal("generation 2 from the overlay's export differs")
	}
}

func TestListAndDelete(t *testing.T) {
	ctx := context.Background()
	const size = 1 << 20
	work := t.TempDir()
	s := newStore(t, t.TempDir(), header.CompressionZstd)
	mem := filepath.Join(work, "mem")
	memoryFile(t, rand.New(rand.NewSource(1)), mem, size)
	dirty, _ := DataExtents(mem)
	parent, _ := header.NewZero(size, page)
	layer := header.NewLayerID()
	pl, _ := s.NewLayer(LayerSpec{Layer: layer, Kind: Memory, Path: mem, Size: size, Dirty: dirty})
	h, err := pl.Upload(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	disk, _ := header.NewBase(size, page)
	for _, id := range []string{"a.@1", "b.@1"} {
		if err := s.PutSnapshot(ctx, &Snapshot{ID: id, Memory: h, Disk: disk, Meta: []byte("{}")}); err != nil {
			t.Fatal(err)
		}
	}
	// A disk-only snapshot.
	if err := s.PutSnapshot(ctx, &Snapshot{ID: "c.@1", Disk: disk, Meta: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	if sn, err := s.GetSnapshot(ctx, "c.@1"); err != nil || sn.Memory != nil || len(sn.Layers()) != 0 {
		t.Fatalf("disk-only snapshot: %+v, %v", sn, err)
	}
	ids, _ := s.ListSnapshots(ctx)
	if !slices.Equal(ids, []string{"a.@1", "b.@1", "c.@1"}) {
		t.Fatalf("snapshots %v", ids)
	}
	layers, _ := s.ListLayers(ctx)
	if !slices.Equal(layers, []header.LayerID{layer}) {
		t.Fatalf("layers %v, want %s", layers, layer)
	}
	if err := s.DeleteSnapshot(ctx, "a.@1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSnapshot(ctx, "a.@1"); err == nil {
		t.Fatal("got a deleted snapshot")
	}
	if err := s.DeleteLayer(ctx, layer); err != nil {
		t.Fatal(err)
	}
	if layers, _ := s.ListLayers(ctx); len(layers) != 0 {
		t.Fatalf("layers left: %v", layers)
	}
	for _, bad := range []string{"../x", "", ".hidden", "a/b"} {
		if _, err := s.GetSnapshot(ctx, bad); err == nil {
			t.Fatalf("snapshot id %q accepted", bad)
		}
	}
	// A provisional header is never stored.
	ph, _ := pl.Provisional(parent)
	if err := s.PutSnapshot(ctx, &Snapshot{ID: "p.@1", Memory: ph, Disk: disk, Meta: []byte("{}")}); err == nil {
		t.Fatal("stored a provisional header")
	}
}

func TestChunkDirEviction(t *testing.T) {
	ctx := context.Background()
	const size = 4 << 20
	bucket, cache, work := t.TempDir(), t.TempDir(), t.TempDir()
	b, _ := storage.NewFS(bucket)
	cfg := storage.DefaultCompressConfig()
	cfg.FrameSize = 64 << 10
	s, _ := New(b, Options{CacheDir: cache, Compression: &cfg})
	mem := filepath.Join(work, "mem")
	want := memoryFile(t, rand.New(rand.NewSource(5)), mem, size)
	dirty, _ := DataExtents(mem)
	parent, _ := header.NewZero(size, page)
	pl, _ := s.NewLayer(LayerSpec{Layer: header.NewLayerID(), Kind: Memory, Path: mem, Size: size, Dirty: dirty})
	h, err := pl.Upload(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := s.OpenFile(h, Memory, "")
	readAll(t, f)
	f.Close()
	s.Close()
	var files []string
	filepath.Walk(filepath.Join(cache, "chunks"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) < 4 {
		t.Fatalf("%d chunk files", len(files))
	}
	// Everything just used is kept, whatever the limits say.
	if removed, _, _ := s.Evict(0, 0); removed != 0 {
		t.Fatalf("evicted %d chunks used just now", removed)
	}
	// Age them: half two days old, half two hours; drop the old ones by age, then the rest by
	// size, oldest first.
	for i, p := range files {
		age := 2 * time.Hour
		if i%2 == 0 {
			age = 48 * time.Hour
		}
		ts := time.Now().Add(-age)
		os.Chtimes(p, ts, ts)
	}
	removed, left, err := s.Evict(24*time.Hour, 1<<40)
	if err != nil || removed != (len(files)+1)/2 {
		t.Fatalf("evicted %d by age (%v), want %d", removed, err, (len(files)+1)/2)
	}
	removed, left, _ = s.Evict(24*time.Hour, 0)
	if removed != len(files)/2 || left != 0 {
		t.Fatalf("evicted %d by size, %d bytes left", removed, left)
	}
	// With the chunks gone the file still reads, from the store.
	s2, _ := New(b, Options{CacheDir: cache})
	t.Cleanup(func() { s2.Close() })
	f, _ = s2.OpenFile(h, Memory, "")
	defer f.Close()
	if !bytes.Equal(readAll(t, f), want) || s2.Stats().Fetches == 0 {
		t.Fatal("read after eviction")
	}
}

func TestCorruptChunkIsRefetched(t *testing.T) {
	ctx := context.Background()
	const size = 1 << 20
	bucket, cache, work := t.TempDir(), t.TempDir(), t.TempDir()
	b, _ := storage.NewFS(bucket)
	cfg := storage.DefaultCompressConfig()
	cfg.FrameSize = 64 << 10
	s, _ := New(b, Options{CacheDir: cache, Compression: &cfg})
	mem := filepath.Join(work, "mem")
	want := memoryFile(t, rand.New(rand.NewSource(9)), mem, size)
	dirty, _ := DataExtents(mem)
	parent, _ := header.NewZero(size, page)
	pl, _ := s.NewLayer(LayerSpec{Layer: header.NewLayerID(), Kind: Memory, Path: mem, Size: size, Dirty: dirty})
	h, _ := pl.Upload(ctx, parent)
	f, _ := s.OpenFile(h, Memory, "")
	readAll(t, f)
	f.Close()
	s.Close()
	// Flip a byte in the middle of every cached frame, keeping its size.
	filepath.Walk(filepath.Join(cache, "chunks"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			data, _ := os.ReadFile(p)
			data[len(data)/2] ^= 0xff
			os.WriteFile(p, data, 0o600)
		}
		return nil
	})
	// A corrupt chunk is noticed (a frame is decoded and its CRC checked before any of its bytes
	// are used), dropped and fetched again.
	s2, _ := New(b, Options{CacheDir: cache})
	t.Cleanup(func() { s2.Close() })
	f, _ = s2.OpenFile(h, Memory, "")
	defer f.Close()
	if !bytes.Equal(readAll(t, f), want) || s2.Stats().Fetches == 0 {
		t.Fatal("read after refetch differs")
	}
}
