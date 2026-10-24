package block

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arugula-salad/loam/internal/snapstore/header"
)

const page = header.PageSize

func TestChunkerSharesFetches(t *testing.T) {
	const size = 3*MemoryChunkSize + 5*page
	data := make([]byte, size)
	rand.New(rand.NewSource(1)).Read(data)
	var opens atomic.Int64
	up := func(_ context.Context, off, length int64, _ *header.FrameTable) (io.ReadCloser, error) {
		opens.Add(1)
		return io.NopCloser(bytes.NewReader(data[off : off+length])), nil
	}
	c, err := NewChunker(size, page, filepath.Join(t.TempDir(), "layer"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			r := rand.New(rand.NewSource(int64(i)))
			for range 50 {
				off := int64(r.Intn(size - 3*page))
				n := 1 + r.Intn(3*page)
				buf := make([]byte, n)
				if _, err := c.ReadAt(context.Background(), buf, off, up, nil); err != nil {
					t.Error(err)
					return
				}
				if !bytes.Equal(buf, data[off:off+int64(n)]) {
					t.Errorf("read at %d differs", off)
					return
				}
			}
		})
	}
	wg.Wait()
	// Four chunks, each fetched once however many readers wanted it.
	if n := opens.Load(); n != 4 {
		t.Fatalf("%d fetches, want 4", n)
	}
	if !c.IsCached(0, size) {
		t.Fatal("not all cached")
	}
}

func TestChunkerFailedFetchIsRetried(t *testing.T) {
	const size = MemoryChunkSize
	data := bytes.Repeat([]byte{7}, size)
	fail := true
	up := func(_ context.Context, off, length int64, _ *header.FrameTable) (io.ReadCloser, error) {
		if fail {
			return nil, errors.New("store unreachable")
		}
		return io.NopCloser(bytes.NewReader(data[off : off+length])), nil
	}
	c, _ := NewChunker(size, page, filepath.Join(t.TempDir(), "layer"))
	defer c.Close()
	buf := make([]byte, page)
	if _, err := c.ReadAt(context.Background(), buf, 0, up, nil); err == nil {
		t.Fatal("read through a failing upstream")
	}
	fail = false
	if _, err := c.ReadAt(context.Background(), buf, 0, up, nil); err != nil || buf[0] != 7 {
		t.Fatalf("retry: %v", err)
	}
	if _, err := c.ReadAt(context.Background(), buf, size-page/2, up, nil); err == nil {
		t.Fatal("read past the layer")
	}
}

func TestCacheZeroDetectionAndExport(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCache(16*page, page, filepath.Join(dir, "c"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	w := make([]byte, 4*page)
	rand.New(rand.NewSource(2)).Read(w[:2*page]) // two data blocks, two zero blocks
	if _, err := c.WriteAt(w, 2*page); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WriteZeroesAt(10*page, page); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadAt(make([]byte, page), 0); !errors.As(err, &BytesNotAvailableError{}) {
		t.Fatalf("read of an unwritten block: %v", err)
	}
	out, _ := os.Create(filepath.Join(dir, "diff"))
	defer out.Close()
	dm, err := c.ExportToDiff(out)
	if err != nil {
		t.Fatal(err)
	}
	if dm.Dirty.Cardinality() != 2 || dm.Empty.Cardinality() != 3 {
		t.Fatalf("dirty %d empty %d, want 2 and 3", dm.Dirty.Cardinality(), dm.Empty.Cardinality())
	}
	packed, _ := os.ReadFile(out.Name())
	if !bytes.Equal(packed, w[:2*page]) {
		t.Fatal("exported diff differs from the dirty blocks")
	}
	if _, err := c.WriteAt(make([]byte, 10), 0); err == nil {
		t.Fatal("misaligned write accepted")
	}
}

func TestChunkerFailureWaitsForSessionRelease(t *testing.T) {
	c, err := NewChunker(MemoryChunkSize, page, filepath.Join(t.TempDir(), "layer"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	session := newFetchSession(0, MemoryChunkSize, c.cache)
	c.fetchSessions = []*fetchSession{session}
	c.fetchMu.Lock()
	var once sync.Once
	unlock := func() { once.Do(c.fetchMu.Unlock) }
	defer unlock()
	entered := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		c.runFetch(context.Background(), session, func(context.Context, int64, int64, *header.FrameTable) (io.ReadCloser, error) {
			close(entered)
			return nil, errors.New("offline")
		}, nil)
	}()
	<-entered
	result := make(chan error, 1)
	go func() { result <- session.registerAndWait(context.Background(), 0) }()
	select {
	case err := <-result:
		unlock()
		<-finished
		t.Fatalf("published failure before removing its session: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("lost fetch failure")
		}
	case <-time.After(time.Second):
		t.Fatal("failure was not published")
	}
	<-finished
}
