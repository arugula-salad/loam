// Derived from github.com/e2b-dev/infra, packages/orchestrator/pkg/sandbox/block/streaming_chunk.go
// and fetch_session.go at commit cf536c2 (Copyright 2023 FoundryLabs, Inc., Apache-2.0, see
// ../LICENSE-e2b). Modified for loam: the upstream is a plain open function, no feature flags,
// metrics or peer transitions.

package block

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arugula-salad/loam/internal/snapstore/header"
)

const (
	// defaultFetchTimeout bounds a single chunk fetch: if the upstream hangs, the goroutine
	// won't live forever.
	defaultFetchTimeout = 60 * time.Second
	// MemoryChunkSize is the fetch unit of an uncompressed layer (a compressed one fetches a
	// frame at a time).
	MemoryChunkSize = 4 << 20
	// readBatch is how much a fetch reads before waking waiters on an uncompressed chunk.
	readBatch = 256 << 10
)

// Upstream opens the uncompressed bytes [off, off+length) of a layer: one frame of ft for a
// compressed layer, one chunk otherwise. Its Close verifies the frame.
type Upstream func(ctx context.Context, off, length int64, ft *header.FrameTable) (io.ReadCloser, error)

// Chunker is one layer's local cache: a sparse mmapped file the size of the layer, filled a
// chunk at a time from upstream. Concurrent reads of the same chunk share one fetch (a fetch
// session), and a reader of an uncompressed chunk is released as soon as its own blocks have
// streamed in.
type Chunker struct {
	cache        *Cache
	size         int64
	fetchTimeout time.Duration

	fetchMu       sync.Mutex
	fetchSessions []*fetchSession
}

// NewChunker makes the cache file at cachePath for a layer of size bytes.
func NewChunker(size, blockSize int64, cachePath string) (*Chunker, error) {
	cache, err := NewCache(size, blockSize, cachePath)
	if err != nil {
		return nil, err
	}
	return &Chunker{size: size, cache: cache, fetchTimeout: defaultFetchTimeout}, nil
}

func (c *Chunker) ReadAt(ctx context.Context, b []byte, off int64, up Upstream, ft *header.FrameTable) (int, error) {
	slice, err := c.Slice(ctx, off, int64(len(b)), up, ft)
	if err != nil {
		return 0, fmt.Errorf("failed to slice cache at %d-%d: %w", off, off+int64(len(b)), err)
	}
	return copy(b, slice), nil
}

// Slice returns the bytes [off, off+length) of the layer, fetching every chunk the range spans
// that isn't cached. The slice is only valid until Close.
func (c *Chunker) Slice(ctx context.Context, off, length int64, up Upstream, ft *header.FrameTable) ([]byte, error) {
	if off < 0 || length < 0 || off+length > c.size {
		return nil, fmt.Errorf("range %d+%d outside layer of %d bytes", off, length, c.size)
	}
	b, err := c.cache.Slice(off, length)
	if err == nil {
		return b, nil
	}
	if !errors.As(err, &BytesNotAvailableError{}) {
		return nil, fmt.Errorf("failed read from cache at offset %d: %w", off, err)
	}
	end := off + length
	for cur := off; cur < end; {
		chunkOff, chunkLen, err := c.locateChunk(cur, ft)
		if err != nil {
			return nil, fmt.Errorf("failed to locate chunk for offset %d: %w", cur, err)
		}
		chunkEnd := chunkOff + chunkLen
		if err := c.fetch(ctx, cur, min(end, chunkEnd)-cur, up, ft); err != nil {
			return nil, fmt.Errorf("failed to ensure data at %d-%d: %w", cur, min(end, chunkEnd), err)
		}
		cur = chunkEnd
	}
	// The waiters confirmed the data is in the mapping.
	return c.cache.sliceDirect(off, length)
}

// getOrCreateSession returns the fetch session for the chunk at [off, off+length), or (nil,
// true) if it is already cached.
func (c *Chunker) getOrCreateSession(ctx context.Context, off, length int64, up Upstream, ft *header.FrameTable) (*fetchSession, bool) {
	c.fetchMu.Lock()
	defer c.fetchMu.Unlock()
	for _, s := range c.fetchSessions {
		if s.contains(off, length) {
			return s, false
		}
	}
	// Re-check under fetchMu: a fetch can finish (marking its blocks cached) and remove itself
	// between the lock-free Slice and the scan above.
	if c.cache.isCached(off, length) {
		return nil, true
	}
	s := newFetchSession(off, length, c.cache)
	c.fetchSessions = append(c.fetchSessions, s)
	// Detach from the caller's cancellation so the shared fetch goes on if the first caller
	// gives up.
	go c.runFetch(context.WithoutCancel(ctx), s, up, ft)
	return s, false
}

// fetch makes sure the chunk holding [off, off+length) is fetched and waits for every block the
// range spans.
func (c *Chunker) fetch(ctx context.Context, off, length int64, up Upstream, ft *header.FrameTable) error {
	chunkOff, chunkLen, err := c.locateChunk(off, ft)
	if err != nil {
		return err
	}
	session, cached := c.getOrCreateSession(ctx, chunkOff, chunkLen, up, ft)
	if cached {
		return nil
	}
	blockSize := c.cache.BlockSize()
	startBlock := (off / blockSize) * blockSize
	endBlock := ((off + length - 1) / blockSize) * blockSize
	chunkEnd := session.chunkOff + session.chunkLen
	for b := startBlock; b <= endBlock && b < chunkEnd; b += blockSize {
		if err := session.registerAndWait(ctx, b); err != nil {
			return err
		}
	}
	return nil
}

// runFetch fetches a chunk into the mapping. It runs in a goroutine of its own.
func (c *Chunker) runFetch(ctx context.Context, s *fetchSession, up Upstream, ft *header.FrameTable) {
	ctx, cancel := context.WithTimeout(ctx, c.fetchTimeout)
	defer cancel()
	var fetchErr error
	// Remove a failed session before notifying its waiters. Otherwise a caller
	// can observe the error and immediately retry into the same failed session.
	// The mapping release registered below runs first, before another fetch
	// can begin writing this chunk.
	defer func() {
		if r := recover(); r != nil {
			fetchErr = fmt.Errorf("fetch panicked: %v", r)
		}
		c.releaseSession(s)
		if fetchErr == nil {
			fetchErr = errors.New("fetch exited without completing")
		}
		s.failIfRunning(fetchErr)
	}()
	mmapSlice, release, err := c.cache.addressBytes(s.chunkOff, s.chunkLen)
	if err != nil {
		fetchErr = err
		return
	}
	defer release()
	if err := c.progressiveFetch(ctx, s, mmapSlice, up, ft); err != nil {
		fetchErr = err
		return
	}
	// Mark the chunk cached before releasing waiters, so isCached is true before the session
	// leaves fetchSessions.
	c.cache.setIsCached(s.chunkOff, s.chunkLen)
	s.setDone()
}

func (c *Chunker) progressiveFetch(ctx context.Context, s *fetchSession, mmapSlice []byte, up Upstream, ft *header.FrameTable) (err error) {
	reader, err := up(ctx, s.chunkOff, s.chunkLen, ft)
	if err != nil {
		return fmt.Errorf("failed to open range reader at %d: %w", s.chunkOff, err)
	}
	defer func() {
		// A compressed frame's CRC is only verified once its footer is consumed on Close, so a
		// Close error fails the fetch: a corrupt frame is neither cached nor released.
		if cerr := reader.Close(); err == nil {
			err = cerr
		}
	}()
	batch := max(c.cache.BlockSize(), readBatch)
	var total int64
	for total < s.chunkLen {
		n, rerr := io.ReadFull(reader, mmapSlice[total:min(total+batch, s.chunkLen)])
		total += int64(n)
		// Compressed chunks are a single frame verified only on Close, so their waiters are
		// released by setDone after it, not here.
		if n > 0 && !ft.IsCompressed() {
			s.advance(total)
		}
		if rerr != nil {
			if total >= s.chunkLen {
				break
			}
			return fmt.Errorf("failed reading at offset %d after %d bytes: %w", s.chunkOff, total, rerr)
		}
	}
	return nil
}

func (c *Chunker) releaseSession(s *fetchSession) {
	c.fetchMu.Lock()
	defer c.fetchMu.Unlock()
	for i, a := range c.fetchSessions {
		if a == s {
			c.fetchSessions[i] = c.fetchSessions[len(c.fetchSessions)-1]
			c.fetchSessions[len(c.fetchSessions)-1] = nil
			c.fetchSessions = c.fetchSessions[:len(c.fetchSessions)-1]
			return
		}
	}
}

// locateChunk returns the aligned (offset, length) of the chunk holding off: the frame for a
// compressed layer, a MemoryChunkSize-aligned chunk (clamped to the layer) otherwise.
func (c *Chunker) locateChunk(off int64, ft *header.FrameTable) (int64, int64, error) {
	if ft.IsCompressed() {
		r, err := ft.LocateUncompressed(off)
		if err != nil {
			return 0, 0, err
		}
		return r.Offset, int64(r.Length), nil
	}
	chunkOff := (off / MemoryChunkSize) * MemoryChunkSize
	return chunkOff, min(int64(MemoryChunkSize), c.size-chunkOff), nil
}

// IsCached reports whether [off, off+length) is held locally, without fetching.
func (c *Chunker) IsCached(off, length int64) bool { return c.cache.isCached(off, length) }

func (c *Chunker) Size() int64 { return c.size }

func (c *Chunker) Close() error { return c.cache.Close() }

type fetchSession struct {
	// chunkOff/chunkLen is what is being fetched, absolute offsets in U-space.
	chunkOff int64
	chunkLen int64
	cache    *Cache

	mu       sync.Mutex
	cond     sync.Cond
	fetchErr error
	done     bool

	// bytesReady is the byte count (from chunkOff) up to which all blocks are written. Atomic
	// so registerAndWait has a lock-free fast path: it only increases.
	bytesReady atomic.Int64
}

func newFetchSession(chunkOff, chunkLen int64, cache *Cache) *fetchSession {
	s := &fetchSession{chunkOff: chunkOff, chunkLen: chunkLen, cache: cache}
	s.cond.L = &s.mu
	return s
}

func (s *fetchSession) contains(off, length int64) bool {
	return s.chunkOff <= off && s.chunkOff+s.chunkLen >= off+length
}

// registerAndWait blocks until the block at blockOff is written, the session ends, or ctx is
// done.
func (s *fetchSession) registerAndWait(ctx context.Context, blockOff int64) error {
	blockSize := s.cache.blockSize
	if blockOff%blockSize != 0 {
		return fmt.Errorf("blockOff %d is not aligned to block size %d", blockOff, blockSize)
	}
	if blockOff < s.chunkOff || blockOff >= s.chunkOff+s.chunkLen {
		return fmt.Errorf("blockOff %d is outside session range [%d, %d)", blockOff, s.chunkOff, s.chunkOff+s.chunkLen)
	}
	endByte := min(blockOff+blockSize-s.chunkOff, s.chunkLen)
	if s.bytesReady.Load() >= endByte {
		return nil
	}
	// Serialize the cancellation wake with the predicate check and the Wait registration, so
	// the callback can't spend its only Broadcast in the gap between them.
	stop := context.AfterFunc(ctx, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.cond.Broadcast()
	})
	defer stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if s.bytesReady.Load() >= endByte {
			return nil
		}
		if s.done {
			// A session that ended short of our block failed, unless an earlier session
			// already fetched it.
			if s.cache.isCached(blockOff, blockSize) {
				return nil
			}
			if s.fetchErr == nil {
				return fmt.Errorf("fetch session ended without error but block %d not cached", blockOff/blockSize)
			}
			return fmt.Errorf("fetch failed: %w", s.fetchErr)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.cond.Wait()
	}
}

func (s *fetchSession) advance(bytesReady int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bytesReady.Store(bytesReady)
	s.cond.Broadcast()
}

func (s *fetchSession) setDone() {
	s.mu.Lock()
	s.bytesReady.Store(s.chunkLen)
	s.done = true
	s.mu.Unlock()
	s.cond.Broadcast()
}

func (s *fetchSession) fail(err error) {
	s.mu.Lock()
	s.fetchErr = err
	s.done = true
	s.mu.Unlock()
	s.cond.Broadcast()
}

// failIfRunning records err only if the session has not ended, so the safety-net defer can't
// override a success.
func (s *fetchSession) failIfRunning(err error) {
	s.mu.Lock()
	if !s.done {
		s.fetchErr = err
		s.done = true
	}
	s.mu.Unlock()
	s.cond.Broadcast()
}
