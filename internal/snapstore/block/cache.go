// Derived from github.com/e2b-dev/infra, packages/orchestrator/pkg/sandbox/block/cache.go,
// device.go and range.go at commit cf536c2 (Copyright 2023 FoundryLabs, Inc., Apache-2.0, see
// ../LICENSE-e2b). Modified for loam: mmap through golang.org/x/sys/unix instead of mmap-go;
// E2B's dedup, memfd, process-memory copy and fold paths dropped.

// Package block is the local side of the snapshot store's read path and of copy-on-write: a
// sparse, mmapped cache file that tracks which blocks it holds, the chunker that fills one per
// layer from the store, and an overlay that takes writes over a read-only device and exports
// them as the next layer's diff.
package block

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"

	"github.com/arugula-salad/loam/internal/snapstore/header"
)

// BytesNotAvailableError indicates the requested range is not cached yet.
type BytesNotAvailableError struct{}

func (BytesNotAvailableError) Error() string {
	return "the requested bytes are not available on the device"
}

// CacheClosedError is returned by a cache used after Close.
type CacheClosedError struct{ filePath string }

func (e *CacheClosedError) Error() string {
	return fmt.Sprintf("block cache already closed for path %s", e.filePath)
}

// ReadonlyDevice is a file the overlay reads under its writes: a snapshot's file through the
// store, a local base file.
type ReadonlyDevice interface {
	ReadAt(ctx context.Context, p []byte, off int64) (int, error)
	Size() int64
	BlockSize() int64
}

// Range is a byte range [Start, Start+Size).
type Range struct {
	Start int64
	Size  int64
}

func (r Range) End() int64 { return r.Start + r.Size }

// BitsetRanges returns the byte ranges of the runs of set bits of b.
func BitsetRanges(b *header.Bitmap, blockSize int64) func(func(Range) bool) {
	return func(yield func(Range) bool) {
		for start, endExcl := range b.Ranges() {
			if !yield(Range{Start: int64(start) * blockSize, Size: (int64(endExcl) - int64(start)) * blockSize}) {
				return
			}
		}
	}
}

// Cache is a sparse file of size bytes, mmapped, with a tracker of which blocks it holds: Dirty
// blocks have their bytes in the file, Zero blocks are punched holes.
type Cache struct {
	filePath  string
	size      int64
	blockSize int64
	mmap      []byte
	mu        sync.RWMutex
	tracker   *Tracker
	closed    atomic.Bool
}

// NewCache creates (or truncates to size) filePath, sparse, and maps it.
func NewCache(size, blockSize int64, filePath string) (*Cache, error) {
	f, err := os.OpenFile(filePath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open cache file: %w", err)
	}
	defer f.Close()
	c := &Cache{filePath: filePath, size: size, blockSize: blockSize, tracker: NewTracker()}
	if size == 0 {
		return c, nil
	}
	// Truncate makes a sparse file: only what is fetched or written takes disk.
	if err := f.Truncate(size); err != nil {
		return nil, fmt.Errorf("size cache file: %w", err)
	}
	c.mmap, err = unix.Mmap(int(f.Fd()), 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("map cache file: %w", err)
	}
	return c, nil
}

func (c *Cache) isClosed() bool { return c.closed.Load() }

// DiffMetadata returns the dirty/empty sets from the tracker without copying any block data.
func (c *Cache) DiffMetadata() (*header.DiffMetadata, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.isClosed() {
		return nil, &CacheClosedError{c.filePath}
	}
	dirty, empty := c.tracker.Export()
	return header.NewDiffMetadata(c.blockSize, dirty, empty), nil
}

// ExportToDiff writes the cache's dirty blocks to out, packed in order (a layer object's
// layout), and returns the diff metadata that describes them.
func (c *Cache) ExportToDiff(out *os.File) (*header.DiffMetadata, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed() {
		return nil, &CacheClosedError{c.filePath}
	}
	dirty, empty := c.tracker.Export()
	meta := header.NewDiffMetadata(c.blockSize, dirty, empty)
	if c.mmap == nil {
		return meta, nil
	}
	f, err := os.Open(c.filePath)
	if err != nil {
		return nil, fmt.Errorf("open cache file: %w", err)
	}
	defer f.Close()
	src := int(f.Fd())
	// An explicit msync is not needed: copy_file_range sees the page cache the mapping writes
	// to. sync_file_range only starts writeback early, so its failure doesn't matter.
	unix.SyncFileRange(src, 0, c.size, unix.SYNC_FILE_RANGE_WRITE)
	dst := int(out.Fd())
	var writeOffset int64
	fallback := false
	for r := range BitsetRanges(meta.Dirty, meta.BlockSize) {
		remaining := int(r.Size)
		readOffset := r.Start
		// The kernel may return short copies (capped at MAX_RW_COUNT on non-reflink
		// filesystems), so loop until the range is copied; it advances the offsets.
		for remaining > 0 {
			if !fallback {
				// On XFS this reflinks.
				n, err := unix.CopyFileRange(src, &readOffset, dst, &writeOffset, remaining, 0)
				switch {
				case errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL):
					fallback = true
				case err != nil:
					return nil, fmt.Errorf("copy file range: %w", err)
				case n == 0:
					return nil, fmt.Errorf("copy_file_range returned 0 with %d bytes remaining", remaining)
				default:
					remaining -= n
				}
			}
			if fallback && remaining > 0 {
				if _, err := out.Seek(writeOffset, io.SeekStart); err != nil {
					return nil, err
				}
				if _, err := io.Copy(out, io.NewSectionReader(f, readOffset, int64(remaining))); err != nil {
					return nil, fmt.Errorf("copy range: %w", err)
				}
				writeOffset += int64(remaining)
				remaining = 0
			}
		}
	}
	return meta, nil
}

func (c *Cache) ReadAt(b []byte, off int64) (int, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.isClosed() {
		return 0, &CacheClosedError{c.filePath}
	}
	if c.mmap == nil {
		return 0, nil
	}
	slice, err := c.Slice(off, int64(len(b)))
	if err != nil {
		return 0, err
	}
	return copy(b, slice), nil
}

// Slice returns the mapped bytes [off, off+length) if the cache holds them all. The slice is
// only valid until Close; the caller keeps writes to a block exclusive.
func (c *Cache) Slice(off, length int64) ([]byte, error) {
	if c.isClosed() {
		return nil, &CacheClosedError{c.filePath}
	}
	if c.mmap == nil {
		return nil, nil
	}
	if c.isCached(off, length) {
		return c.mmap[off:min(off+length, c.size)], nil
	}
	return nil, BytesNotAvailableError{}
}

// sliceDirect returns a slice of the mapping without checking it is cached: the chunker's waiter
// has confirmed it.
func (c *Cache) sliceDirect(off, length int64) ([]byte, error) {
	if c.isClosed() {
		return nil, &CacheClosedError{c.filePath}
	}
	if off < 0 || off >= c.size {
		return nil, BytesNotAvailableError{}
	}
	return c.mmap[off:min(off+length, c.size)], nil
}

// isCached reports whether every block of [off, off+length) is held. Zero blocks count: the
// mapping reads them back as zeros (punched).
func (c *Cache) isCached(off, length int64) bool {
	start := uint32(header.BlockIdx(off, c.blockSize))
	end := uint32(header.BlockCeilIdx(min(off+length, c.size), c.blockSize))
	return c.tracker.Present(start, end)
}

func (c *Cache) setIsCached(off, length int64) {
	start := uint32(header.BlockIdx(off, c.blockSize))
	end := uint32(header.BlockCeilIdx(off+length, c.blockSize))
	c.tracker.SetRange(start, end, Dirty)
}

// punchHole frees backing pages; clear() if MADV_REMOVE is unsupported.
func (c *Cache) punchHole(off, length int64) {
	if err := unix.Madvise(c.mmap[off:off+length], unix.MADV_REMOVE); err != nil {
		clear(c.mmap[off : off+length])
	}
}

// WriteAt writes block-aligned b at off. Runs of zero blocks are punched and recorded Zero
// (detect-zeroes=unmap), the rest copied and recorded Dirty.
func (c *Cache) WriteAt(b []byte, off int64) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed() {
		return 0, &CacheClosedError{c.filePath}
	}
	if c.mmap == nil {
		return 0, nil
	}
	if int64(len(b))%c.blockSize != 0 || off%c.blockSize != 0 {
		return 0, fmt.Errorf("misaligned write: len=%d off=%d block=%d", len(b), off, c.blockSize)
	}
	end := min(off+int64(len(b)), c.size)
	if end <= off {
		return 0, nil
	}
	flush := func(runStart, runEnd int64, runZero bool) {
		startIdx := uint32(header.BlockIdx(runStart, c.blockSize))
		endIdx := uint32(header.BlockCeilIdx(runEnd, c.blockSize))
		if runZero {
			c.punchHole(runStart, runEnd-runStart)
			c.tracker.SetRange(startIdx, endIdx, Zero)
		} else {
			copy(c.mmap[runStart:runEnd], b[runStart-off:runEnd-off])
			c.tracker.SetRange(startIdx, endIdx, Dirty)
		}
	}
	runStart := off
	runZero := header.IsZero(b[:c.blockSize])
	for i := off + c.blockSize; i < end; i += c.blockSize {
		z := header.IsZero(b[i-off : i-off+c.blockSize])
		if z == runZero {
			continue
		}
		flush(runStart, i, runZero)
		runStart, runZero = i, z
	}
	flush(runStart, end, runZero)
	return int(end - off), nil
}

// WriteZeroesAt punches the block-aligned range and marks its blocks Zero.
func (c *Cache) WriteZeroesAt(off, length int64) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed() {
		return 0, &CacheClosedError{c.filePath}
	}
	if c.mmap == nil {
		return 0, nil
	}
	end := min(off+length, c.size)
	if end <= off {
		return 0, nil
	}
	c.punchHole(off, end-off)
	c.tracker.SetRange(uint32(header.BlockIdx(off, c.blockSize)), uint32(header.BlockCeilIdx(end, c.blockSize)), Zero)
	return int(end - off), nil
}

// addressBytes returns a slice of the mapping and a function that releases the read lock that
// keeps the cache from being closed under it.
func (c *Cache) addressBytes(off, length int64) ([]byte, func(), error) {
	c.mu.RLock()
	if c.isClosed() {
		c.mu.RUnlock()
		return nil, func() {}, &CacheClosedError{c.filePath}
	}
	if off >= c.size {
		c.mu.RUnlock()
		return nil, func() {}, fmt.Errorf("offset %d is out of bounds", off)
	}
	return c.mmap[off:min(off+length, c.size)], c.mu.RUnlock, nil
}

func (c *Cache) Size() int64      { return c.size }
func (c *Cache) BlockSize() int64 { return c.blockSize }
func (c *Cache) Path() string     { return c.filePath }

// Close unmaps the cache and removes its file.
func (c *Cache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed.CompareAndSwap(false, true) {
		return &CacheClosedError{c.filePath}
	}
	var err error
	if c.mmap != nil {
		err = unix.Munmap(c.mmap)
	}
	return errors.Join(err, os.RemoveAll(c.filePath))
}
