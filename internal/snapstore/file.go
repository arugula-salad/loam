// The read path is derived from github.com/e2b-dev/infra,
// packages/orchestrator/pkg/sandbox/build/build.go, storage_diff.go and cache.go at commit
// cf536c2 (Copyright 2023 FoundryLabs, Inc., Apache-2.0, see LICENSE-e2b). Modified for loam:
// every header carries what reading its layers needs, so the ancestor-header refresh, the peer
// transitions and soft-delete checks are gone; layer caches are reference-counted by the open
// files instead of a TTL cache; Base ranges read a local base file; local (provisional) layers
// read the unpacked file a pause wrote.

package snapstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"

	"github.com/arugula-salad/loam/internal/snapstore/block"
	"github.com/arugula-salad/loam/internal/snapstore/header"
)

type layerKey struct {
	id   header.LayerID
	kind Kind
}

// layerReader is one stored layer's local cache in this process, shared by every open file that
// reads the layer and closed (its cache file removed) when the last one closes.
type layerReader struct {
	chunker *block.Chunker
	up      block.Upstream
	refs    int
}

func (s *Store) acquireLayer(id header.LayerID, kind Kind, ld header.LayerData) (*layerReader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := layerKey{id, kind}
	if lr, ok := s.layers[k]; ok {
		if lr.chunker.Size() != ld.Size {
			return nil, fmt.Errorf("layer %s %s is %d bytes in one header and %d in another", id, kind, lr.chunker.Size(), ld.Size)
		}
		lr.refs++
		return lr, nil
	}
	var r [6]byte
	rand.Read(r[:])
	path := filepath.Join(s.layerDir, fmt.Sprintf("%s-%s-%s", id, kind, hex.EncodeToString(r[:])))
	c, err := block.NewChunker(ld.Size, header.PageSize, path)
	if err != nil {
		return nil, fmt.Errorf("layer %s %s cache: %w", id, kind, err)
	}
	key := layerKeyFor(id, kind, ld.Frames.CompressionType())
	lr := &layerReader{
		chunker: c,
		up: func(ctx context.Context, off, length int64, ft *header.FrameTable) (io.ReadCloser, error) {
			return s.chunks.OpenChunk(ctx, key, off, length, ft)
		},
		refs: 1,
	}
	s.layers[k] = lr
	return lr, nil
}

func (s *Store) releaseLayer(id header.LayerID, kind Kind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := layerKey{id, kind}
	lr := s.layers[k]
	if lr == nil {
		return
	}
	if lr.refs--; lr.refs == 0 {
		delete(s.layers, k)
		lr.chunker.Close()
	}
}

// File is a snapshot's file read through the store: every offset resolved by the header to its
// layer, a range of zeros, or the base file. Reads of stored layers go through this process's
// per-layer cache, then the node's chunk directory, then the backend. Safe for concurrent use.
type File struct {
	s      *Store
	kind   Kind
	header atomic.Pointer[header.Header]
	base   *os.File

	mu     sync.Mutex
	layers map[header.LayerID]*layerReader
	locals map[string]*os.File
	closed bool
}

// OpenFile opens a header's file for reading. base is the local file Base ranges read from (a
// disk's template image, at the same offsets); "" when the header has none, and reading a Base
// range is then an error.
func (s *Store) OpenFile(h *header.Header, kind Kind, base string) (*File, error) {
	if !kind.valid() {
		return nil, fmt.Errorf("unknown file kind %q", kind)
	}
	f := &File{s: s, kind: kind, layers: map[header.LayerID]*layerReader{}, locals: map[string]*os.File{}}
	if base != "" {
		b, err := os.Open(base)
		if err != nil {
			return nil, err
		}
		f.base = b
	}
	f.header.Store(h)
	return f, nil
}

// Header is the header the file reads through.
func (f *File) Header() *header.Header { return f.header.Load() }

// SwapHeader replaces the header, typically a provisional one with the durable header its upload
// produced: both describe the same bytes, so reads in flight are unaffected. The local files the
// provisional header read may go once SwapHeader returns and the reads in flight have finished.
func (f *File) SwapHeader(h *header.Header) error {
	old := f.header.Load()
	if old.Metadata.Size != h.Metadata.Size {
		return fmt.Errorf("swap header: size %d != %d", h.Metadata.Size, old.Metadata.Size)
	}
	f.header.Store(h)
	return nil
}

func (f *File) Size() int64      { return int64(f.header.Load().Metadata.Size) }
func (f *File) BlockSize() int64 { return header.PageSize }

// ReadAt fills p from off. A read past the end is short, with io.EOF.
func (f *File) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	h := f.header.Load()
	size := int64(h.Metadata.Size)
	n := 0
	for n < len(p) {
		cur := off + int64(n)
		if cur >= size {
			return n, io.EOF
		}
		aligned := cur - cur%header.PageSize
		m, err := h.GetShiftedMapping(aligned)
		if err != nil {
			return n, err
		}
		shift := cur - aligned
		length := min(int64(m.Length)-shift, int64(len(p)-n))
		dst := p[n : n+int(length)]
		src := int64(m.LayerOffset) + shift
		switch {
		case m.Layer == header.Empty:
			clear(dst)
		case m.Layer == header.Base:
			if f.base == nil {
				return n, fmt.Errorf("offset %d is in the base file, and none was given", cur)
			}
			if _, err := f.base.ReadAt(dst, src); err != nil {
				return n, fmt.Errorf("read base file at %d: %w", src, err)
			}
		case h.Local[m.Layer] != "":
			lf, err := f.local(h.Local[m.Layer])
			if err != nil {
				return n, err
			}
			// A sparse file may end before the file it describes: the rest reads as zeros.
			got, err := lf.ReadAt(dst, src)
			if err != nil && !errors.Is(err, io.EOF) {
				return n, fmt.Errorf("read local layer %s at %d: %w", m.Layer, src, err)
			}
			clear(dst[got:])
		default:
			ld, ok := h.Layers[m.Layer]
			if !ok {
				return n, fmt.Errorf("header has no entry for layer %s", m.Layer)
			}
			lr, err := f.layer(m.Layer, ld)
			if err != nil {
				return n, err
			}
			if _, err := lr.chunker.ReadAt(ctx, dst, src, lr.up, ld.Frames); err != nil {
				return n, fmt.Errorf("layer %s %s: %w", m.Layer, f.kind, err)
			}
		}
		n += int(length)
	}
	return n, nil
}

func (f *File) layer(id header.LayerID, ld header.LayerData) (*layerReader, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, errors.New("file closed")
	}
	if lr := f.layers[id]; lr != nil {
		return lr, nil
	}
	lr, err := f.s.acquireLayer(id, f.kind, ld)
	if err != nil {
		return nil, err
	}
	f.layers[id] = lr
	return lr, nil
}

func (f *File) local(path string) (*os.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, errors.New("file closed")
	}
	if lf := f.locals[path]; lf != nil {
		return lf, nil
	}
	lf, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("local layer file: %w", err)
	}
	f.locals[path] = lf
	return lf, nil
}

// Close releases the file's layer caches and closes its local files.
func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	for id := range f.layers {
		f.s.releaseLayer(id, f.kind)
	}
	for _, lf := range f.locals {
		lf.Close()
	}
	if f.base != nil {
		f.base.Close()
	}
	return nil
}

// MaterializeDisk makes dst the disk a header describes. dst must already be a copy (a reflink
// clone, in practice) of the disk's base file, the template image, and of the header's size:
// only the ranges that differ from the base are written, the layers' blocks fetched from the
// store and the zero ranges punched out. Disk layers are small next to the disk, so this is
// eager: the VM starts on a complete local disk.
func (s *Store) MaterializeDisk(ctx context.Context, h *header.Header, dst string) error {
	out, err := os.OpenFile(dst, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer out.Close()
	fi, err := out.Stat()
	if err != nil {
		return err
	}
	if fi.Size() != int64(h.Metadata.Size) {
		return fmt.Errorf("materialize %s: it is %d bytes, the header %d", dst, fi.Size(), h.Metadata.Size)
	}
	f, err := s.OpenFile(h, Disk, "")
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	for _, m := range h.Mapping.All() {
		switch m.Layer {
		case header.Base:
			continue
		case header.Empty:
			if err := punch(out, int64(m.Offset), int64(m.Length)); err != nil {
				return err
			}
			continue
		}
		for off := int64(m.Offset); off < int64(m.Offset+m.Length); {
			chunk := buf[:min(int64(len(buf)), int64(m.Offset+m.Length)-off)]
			if _, err := f.ReadAt(ctx, chunk, off); err != nil {
				return err
			}
			if _, err := out.WriteAt(chunk, off); err != nil {
				return err
			}
			off += int64(len(chunk))
		}
	}
	return out.Sync()
}

// punch makes [off, off+n) of f read as zeros, freeing its blocks where the filesystem can.
func punch(f *os.File, off, n int64) error {
	err := unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, off, n)
	if err == nil {
		return nil
	}
	if !errors.Is(err, unix.EOPNOTSUPP) && !errors.Is(err, unix.ENOSYS) {
		return fmt.Errorf("punch %d+%d: %w", off, n, err)
	}
	zeros := make([]byte, min(n, 1<<20))
	for n > 0 {
		w := min(n, int64(len(zeros)))
		if _, err := f.WriteAt(zeros[:w], off); err != nil {
			return err
		}
		off, n = off+w, n-w
	}
	return nil
}

// A File is the device an Overlay goes over, and the page source a UFFD handler serves from.
var _ block.ReadonlyDevice = (*File)(nil)
