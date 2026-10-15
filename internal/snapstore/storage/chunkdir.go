// Derived from github.com/e2b-dev/infra, packages/shared/pkg/storage/storage_cache.go,
// storage_cache_seekable.go and storage_cache_seekable_compressed.go at commit cf536c2
// (Copyright 2023 FoundryLabs, Inc., Apache-2.0, see ../LICENSE-e2b), E2B's "NFS cache". Modified
// for loam: a node-local directory instead of a shared NFS mount, rename instead of lock files,
// eviction by age and size like the nodelet's image cache, counters instead of OTel.

package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/arugula-salad/loam/internal/snapstore/header"
)

// Stats counts where reads were served from. The store shares one across its chunk directory
// and layer caches; tests use it to show a warm cache makes no backend fetches.
type Stats struct {
	// Fetches and FetchedBytes count ranged reads from the backend (compressed bytes).
	Fetches      atomic.Int64
	FetchedBytes atomic.Int64
	// ChunkHits counts chunks served from the chunk directory.
	ChunkHits atomic.Int64
}

// ChunkDir is the node-local cache of layer chunks: every frame (or, for an uncompressed layer,
// every chunk) a read fetched from the backend, as one file, under the object's key. It
// outlives the processes that read through it, so a second VM restored from the same layers on
// this node, or the same one after a nodelet restart, fetches nothing. Files are written
// through a temporary file and a rename, so two processes fetching the same frame at once both
// write it and either copy wins. A read refreshes the file's mtime; Evict drops the least
// recently used.
type ChunkDir struct {
	root  string
	b     Backend
	stats *Stats
	wg    sync.WaitGroup
}

// NewChunkDir returns reads of b through root. An empty root caches nothing.
func NewChunkDir(root string, b Backend, stats *Stats) *ChunkDir {
	if stats == nil {
		stats = &Stats{}
	}
	return &ChunkDir{root: root, b: b, stats: stats}
}

// Backend is the backend the chunk directory reads from.
func (c *ChunkDir) Backend() Backend { return c.b }

// OpenChunk returns a reader of the uncompressed bytes [offU, offU+lenU) of the object key. For a
// compressed object (ft non-nil) the range must be exactly one frame of ft; for an uncompressed
// one, any range. Close reports a frame that fails to decode (CRC, truncation), so a caller must
// not trust bytes it read until Close returned nil.
func (c *ChunkDir) OpenChunk(ctx context.Context, key string, offU, lenU int64, ft *header.FrameTable) (io.ReadCloser, error) {
	if err := ValidKey(key); err != nil {
		return nil, err
	}
	if ft.IsCompressed() {
		return c.openCompressed(ctx, key, offU, lenU, ft)
	}
	path := c.chunkPath(key, "bin", offU, lenU)
	if f := c.openCached(path, lenU); f != nil {
		return f, nil
	}
	raw, err := c.fetch(ctx, key, offU, lenU)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return raw, nil
	}
	return newCaptureReader(raw, int(lenU), func(data []byte) { c.writeBack(path, data, lenU) }), nil
}

func (c *ChunkDir) openCompressed(ctx context.Context, key string, offU, lenU int64, ft *header.FrameTable) (io.ReadCloser, error) {
	rng, err := ft.LocateCompressed(offU)
	if err != nil {
		return nil, fmt.Errorf("frame lookup for offset %d of %s: %w", offU, key, err)
	}
	if u, _ := ft.LocateUncompressed(offU); int64(u.Length) != lenU {
		return nil, fmt.Errorf("read of %d bytes at %d of %s is not one frame (%d bytes)", lenU, offU, key, u.Length)
	}
	path := c.chunkPath(key, "frm", rng.Offset, int64(rng.Length))
	// A hit decodes the cached frame whole before handing out a byte: a frame that no longer
	// decodes (bit rot, or a torn write that still has the right size) is dropped and fetched
	// again, so a bad chunk file never fails a read.
	if f := c.openCached(path, int64(rng.Length)); f != nil {
		data, err := decodeFrame(f, ft.CompressionType(), lenU)
		if err == nil {
			return io.NopCloser(bytes.NewReader(data)), nil
		}
		os.Remove(path)
	}
	raw, err := c.fetch(ctx, key, rng.Offset, int64(rng.Length))
	if err != nil {
		return nil, err
	}
	// Tee the compressed bytes and write them back only if the frame decoded cleanly, so a
	// corrupt-but-right-sized frame is never cached.
	var captured []byte
	src := raw
	if path != "" {
		src = newCaptureReader(raw, rng.Length, func(data []byte) { captured = data })
	}
	dec, err := NewDecompressReader(src, ft.CompressionType())
	if err != nil {
		raw.Close()
		return nil, err
	}
	return &closeHook{ReadCloser: dec, onClose: func(err error) {
		if err == nil && path != "" {
			c.writeBack(path, captured, int64(rng.Length))
		}
	}}, nil
}

func (c *ChunkDir) fetch(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	r, err := c.b.OpenRange(ctx, key, off, n)
	if err != nil {
		return nil, fmt.Errorf("fetch %d bytes at %d of %s: %w", n, off, key, err)
	}
	c.stats.Fetches.Add(1)
	c.stats.FetchedBytes.Add(n)
	return r, nil
}

func (c *ChunkDir) chunkPath(key, ext string, off, n int64) string {
	if c.root == "" {
		return ""
	}
	return filepath.Join(c.root, filepath.FromSlash(key), fmt.Sprintf("%016x-%x.%s", off, n, ext))
}

// touchAfter is how stale a chunk's mtime may get before a read refreshes it: often enough for
// eviction's hour floor, rarely enough that hot reads don't each cost a syscall.
const touchAfter = time.Minute

// openCached opens a cached chunk of size bytes, or returns nil. A file of the wrong size is
// dropped.
func (c *ChunkDir) openCached(path string, size int64) *os.File {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	fi, err := f.Stat()
	if err != nil || fi.Size() != size {
		f.Close()
		os.Remove(path)
		return nil
	}
	if now := time.Now(); now.Sub(fi.ModTime()) > touchAfter {
		os.Chtimes(path, now, now)
	}
	c.stats.ChunkHits.Add(1)
	return f
}

// writeBack stores a fetched chunk in a goroutine of its own: the read that fetched it has its
// bytes already and must not wait for the disk.
func (c *ChunkDir) writeBack(path string, data []byte, want int64) {
	if int64(len(data)) != want {
		return
	}
	c.wg.Go(func() { writeAtomic(path, func(f *os.File) error { _, err := f.Write(data); return err }) })
}

// Wait waits for the chunk writes in flight.
func (c *ChunkDir) Wait() { c.wg.Wait() }

// Remove drops every cached chunk of the objects under prefix (a deleted layer's).
func (c *ChunkDir) Remove(prefix string) error {
	if c.root == "" {
		return nil
	}
	if err := ValidKey(strings.TrimSuffix(prefix, "/")); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(c.root, filepath.FromSlash(prefix)))
}

// EvictFloor is how recently used a chunk must not have been for Evict to drop it, whatever the
// limits say: a reader in another process may be about to open it.
const EvictFloor = time.Hour

// Evict removes chunks unused for longer than maxAge, then the least recently used until the
// directory holds at most maxBytes (allocated), never one used within EvictFloor. It returns
// how many files it removed and the bytes left.
func (c *ChunkDir) Evict(maxAge time.Duration, maxBytes int64) (removed int, left int64, err error) {
	if c.root == "" {
		return 0, 0, nil
	}
	type entry struct {
		path  string
		mtime time.Time
		bytes int64
	}
	var entries []entry
	var total int64
	now := time.Now()
	err = filepath.WalkDir(c.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		var st syscall.Stat_t
		if syscall.Stat(p, &st) != nil {
			return nil
		}
		mtime := time.Unix(st.Mtim.Sec, st.Mtim.Nsec)
		// A temporary file a crashed writer left behind.
		if strings.HasPrefix(d.Name(), ".tmp-") {
			if now.Sub(mtime) > EvictFloor {
				os.Remove(p)
			}
			return nil
		}
		e := entry{p, mtime, st.Blocks * 512}
		total += e.bytes
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return 0, total, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].mtime.Before(entries[j].mtime) })
	for _, e := range entries {
		if now.Sub(e.mtime) < EvictFloor {
			continue
		}
		if now.Sub(e.mtime) <= maxAge && total <= maxBytes {
			continue
		}
		if os.Remove(e.path) != nil {
			continue
		}
		total -= e.bytes
		removed++
		for d := filepath.Dir(e.path); d != c.root && strings.HasPrefix(d, c.root); d = filepath.Dir(d) {
			if os.Remove(d) != nil {
				break
			}
		}
	}
	return removed, total, nil
}

// decodeFrame reads and decodes a whole frame of lenU uncompressed bytes from f, and closes f.
func decodeFrame(f *os.File, ct header.CompressionType, lenU int64) ([]byte, error) {
	dec, err := NewDecompressReader(f, ct)
	if err != nil {
		f.Close()
		return nil, err
	}
	data := make([]byte, lenU)
	_, err = io.ReadFull(dec, data)
	return data, errors.Join(err, dec.Close())
}

// captureReader tees every byte read into a buffer and hands it to onClose when the reader
// closes cleanly, after reading the rest: the cache needs the whole chunk however much the
// consumer above demanded.
type captureReader struct {
	inner   io.ReadCloser
	buf     *bytes.Buffer
	onClose func([]byte)
}

func newCaptureReader(inner io.ReadCloser, capHint int, onClose func([]byte)) *captureReader {
	return &captureReader{inner: inner, buf: bytes.NewBuffer(make([]byte, 0, capHint)), onClose: onClose}
}

func (r *captureReader) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	r.buf.Write(p[:n])
	return n, err
}

func (r *captureReader) Close() error {
	_, cerr := io.Copy(io.Discard, r)
	err := r.inner.Close()
	if err == nil && cerr == nil {
		r.onClose(r.buf.Bytes())
	}
	return errors.Join(cerr, err)
}

// closeHook runs onClose once with the wrapped reader's Close error.
type closeHook struct {
	io.ReadCloser
	onClose func(error)
}

func (r *closeHook) Close() error {
	err := r.ReadCloser.Close()
	if r.onClose != nil {
		r.onClose(err)
		r.onClose = nil
	}
	return err
}
