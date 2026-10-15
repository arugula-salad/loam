// Derived from github.com/e2b-dev/infra, packages/shared/pkg/storage/header/metadata.go and
// diff.go at commit cf536c2 (Copyright 2023 FoundryLabs, Inc., Apache-2.0, see ../LICENSE-e2b).
// Modified for loam: builds renamed layers, roaring replaced by Bitmap, telemetry dropped, the
// resized-rootfs variant dropped (a loam disk keeps its size).

package header

import (
	"bytes"
	"fmt"
	"io"
	"slices"
)

// IsZero reports whether b is all-zero. Samples first/middle/last byte to reject most non-zero
// buffers from one cache line, then falls back to bytes.Equal(b[:n-1], b[1:]): true exactly when
// every adjacent pair of bytes is equal, i.e. all bytes equal b[0] (which the sample already
// proved is zero).
func IsZero(b []byte) bool {
	n := len(b)
	if n == 0 {
		return true
	}
	if b[0]|b[n-1]|b[n/2] != 0 {
		return false
	}
	if n <= 3 {
		return true
	}
	return bytes.Equal(b[:n-1], b[1:])
}

// DiffMetadata is what one generation changed in a file, in blocks of BlockSize: Dirty blocks,
// whose content is in the new layer, and Empty blocks, which now read as zeros and are stored
// nowhere. Every other block is unchanged from the parent.
type DiffMetadata struct {
	Dirty *Bitmap
	Empty *Bitmap

	BlockSize int64
}

func NewDiffMetadata(blockSize int64, dirty, empty *Bitmap) *DiffMetadata {
	if dirty == nil {
		dirty = NewBitmap()
	}
	if empty == nil {
		empty = NewBitmap()
	}
	return &DiffMetadata{Dirty: dirty, Empty: empty, BlockSize: blockSize}
}

// ToProvisionalDiffHeader builds a local-only header that reads the dirty blocks straight from
// path, the unpacked file a pause just wrote (identity offsets, see createIdentityMapping),
// composed over parent, which may itself be provisional. Unlike ToDiffHeader it needs no packed
// layer object, so it is available at pause time and lets a resume on this node serve at once
// while the upload runs. It is never uploaded: the uploaded header always comes from ToDiffHeader
// over a durable parent.
func (d *DiffMetadata) ToProvisionalDiffHeader(parent *Header, layer LayerID, path string) (*Header, error) {
	dirty := createIdentityMapping(layer, d.Dirty, d.BlockSize)
	empty := CreateMapping(Empty, d.Empty, d.BlockSize)
	diff := MergeMappings(dirty, empty)
	m := NormalizeMappings(MergeMappings(parent.Mapping.Slice(), diff))
	h, err := newDiffHeader(parent.Metadata.NextGeneration(layer), m, parent.Layers)
	if err != nil {
		return nil, fmt.Errorf("provisional header: %w", err)
	}
	h.Local = map[LayerID]string{layer: path}
	for id, p := range parent.Local {
		if slices.Contains(h.Mapping.Layers(), id) {
			h.Local[id] = p
		}
	}
	return h, nil
}

// ToDiffHeader composes the diff over parent into the next generation's header, its dirty blocks
// packed in order into layer's object. The header knows what parent knew of every layer it still
// references; the caller adds layer's own entry (SetLayer) once the object is stored.
func (d *DiffMetadata) ToDiffHeader(parent *Header, layer LayerID) (*Header, error) {
	dirty := CreateMapping(layer, d.Dirty, d.BlockSize)
	// The layer ID is intentionally ignored for empty blocks.
	empty := CreateMapping(Empty, d.Empty, d.BlockSize)
	diff := MergeMappings(dirty, empty)
	// MergeMappings/NormalizeMappings operate on the transient []LayerMap; the parent's compact
	// mapping is materialized once here.
	m := NormalizeMappings(MergeMappings(parent.Mapping.Slice(), diff))
	h, err := newDiffHeader(parent.Metadata.NextGeneration(layer), m, parent.Layers)
	if err != nil {
		return nil, fmt.Errorf("diff header: %w", err)
	}
	return h, nil
}

// DiffMetadataBuilder classifies the blocks of a diff as they are written to a layer: a block of
// zeros becomes Empty and is not written, any other block is written to out and becomes Dirty.
type DiffMetadataBuilder struct {
	dirty     *Bitmap
	empty     *Bitmap
	blockSize int64
}

func NewDiffMetadataBuilder(blockSize int64) *DiffMetadataBuilder {
	return &DiffMetadataBuilder{dirty: NewBitmap(), empty: NewBitmap(), blockSize: blockSize}
}

// Process classifies the block at offset (one block of blockSize bytes) and writes it to out if
// it is not all zeros.
func (b *DiffMetadataBuilder) Process(block []byte, out io.Writer, offset int64) error {
	if int64(len(block)) != b.blockSize {
		return fmt.Errorf("block of %d bytes, want %d", len(block), b.blockSize)
	}
	blockIdx := uint32(BlockIdx(offset, b.blockSize))
	if IsZero(block) {
		b.empty.Add(blockIdx)
		return nil
	}
	b.dirty.Add(blockIdx)
	n, err := out.Write(block)
	if err != nil {
		return err
	}
	if int64(n) != b.blockSize {
		return fmt.Errorf("short write: %d != %d", n, b.blockSize)
	}
	return nil
}

// MarkEmpty records the block at offset as zeros without reading it: a block the caller knows
// the guest freed (a UFFD REMOVE).
func (b *DiffMetadataBuilder) MarkEmpty(offset int64) {
	b.empty.Add(uint32(BlockIdx(offset, b.blockSize)))
}

func (b *DiffMetadataBuilder) Build() *DiffMetadata {
	return &DiffMetadata{Dirty: b.dirty, Empty: b.empty, BlockSize: b.blockSize}
}
