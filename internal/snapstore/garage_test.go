//go:build garage

package snapstore

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/arugula-salad/loam/internal/snapstore/header"
)

// TestGarageStore is the store's round trip against a real bucket (see TestGarageBackend for the
// environment): two memory generations and a disk layer stored, read back cold by another node
// and warm by a third, the disk materialized, then everything deleted.
func TestGarageStore(t *testing.T) {
	url := os.Getenv("LOAM_STORE_URL")
	if url == "" {
		t.Fatal("LOAM_STORE_URL is not set")
	}
	ctx := context.Background()
	const size = 32 << 20
	r := rand.New(rand.NewSource(11))
	work := t.TempDir()
	s, err := Open(url, Options{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mem := filepath.Join(work, "mem")
	cur := memoryFile(t, r, mem, size)
	dirty, _ := DataExtents(mem)
	zero, _ := header.NewZero(size, page)
	pl, _ := s.NewLayer(LayerSpec{Layer: header.NewLayerID(), Kind: Memory, Path: mem, Size: size, Dirty: dirty})
	h1, err := pl.Upload(ctx, zero)
	if err != nil {
		t.Fatal(err)
	}
	diff := filepath.Join(work, "diff")
	var es []Extent
	cur, es = diffFile(t, r, diff, cur, 50)
	layer2 := header.NewLayerID()
	pl, _ = s.NewLayer(LayerSpec{Layer: layer2, Kind: Memory, Path: diff, Size: size, Dirty: es})
	h2, err := pl.Upload(ctx, h1)
	if err != nil {
		t.Fatal(err)
	}

	base := make([]byte, size)
	r.Read(base)
	basePath := filepath.Join(work, "template")
	os.WriteFile(basePath, base, 0o600)
	vm := bytes.Clone(base)
	r.Read(vm[size/2 : size/2+5*page])
	clear(vm[page : 9*page])
	vmPath := filepath.Join(work, "vm")
	os.WriteFile(vmPath, vm, 0o600)
	baseHeader, _ := header.NewBase(size, page)
	pl, _ = s.NewLayer(LayerSpec{Layer: layer2, Kind: Disk, Path: vmPath, Size: size,
		Dirty: []Extent{{size / 2, 5 * page}, {page, 8 * page}}})
	d2, err := pl.Upload(ctx, baseHeader)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutSnapshot(ctx, &Snapshot{ID: "garage-test.@2", Memory: h2, Disk: d2, Meta: []byte(`{"test":true}`)}); err != nil {
		t.Fatal(err)
	}

	nodeCache := t.TempDir()
	for pass, name := range []string{"cold", "warm"} {
		other, err := Open(url, Options{CacheDir: nodeCache})
		if err != nil {
			t.Fatal(err)
		}
		sn, err := other.GetSnapshot(ctx, "garage-test.@2")
		if err != nil {
			t.Fatal(err)
		}
		f, _ := other.OpenFile(sn.Memory, Memory, "")
		if !bytes.Equal(readAll(t, f), cur) {
			t.Fatalf("%s read of memory differs", name)
		}
		f.Close()
		dst := filepath.Join(work, "restored-"+name)
		os.WriteFile(dst, base, 0o600)
		if err := other.MaterializeDisk(ctx, sn.Disk, dst); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(dst); !bytes.Equal(got, vm) {
			t.Fatalf("%s materialized disk differs", name)
		}
		other.Close()
		st := other.Stats()
		t.Logf("%s node: %+v", name, st)
		if pass == 1 && st.Fetches != 0 {
			t.Fatalf("warm node fetched %d chunks", st.Fetches)
		}
		if pass == 0 {
			t.Logf("layers %v", sn.Layers())
		}
	}

	if err := s.DeleteSnapshot(ctx, "garage-test.@2"); err != nil {
		t.Fatal(err)
	}
	layers, err := s.ListLayers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range layers {
		if err := s.DeleteLayer(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if keys, _ := s.b.List(ctx, ""); len(keys) != 0 {
		t.Fatalf("left in the bucket: %v", keys)
	}
}
