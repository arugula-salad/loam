// Derived from github.com/e2b-dev/infra, packages/shared/pkg/storage/header/compact.go at commit
// cf536c2 (Copyright 2023 FoundryLabs, Inc., Apache-2.0, see ../LICENSE-e2b). Modified for loam:
// builds renamed layers; the legacy non-contiguous fallbacks kept only where a hand-built
// mapping can still produce them.

package header

import (
	"errors"
	"fmt"
	"iter"
	"math"
)

// Mapping is the compact in-memory representation of a Header's mapping list. A header can be
// held for a VM's whole life (the UFFD handler's), so each entry is shrunk from 40 bytes (a
// LayerMap) to 9 or 10 by encoding offset/storage as uint32 block indices, deduplicating the
// layer ID into a per-header table addressed by one byte (two once a header references more than
// 255 layers), storing the columns as parallel slices, and deriving each entry's length from
// where the next one starts. Immutable; read via At / All / Slice.
type Mapping struct {
	blockSize uint64
	layers    []LayerID
	offsets   []uint32
	// lengths is nil when every entry starts where the previous one ends: the length is then
	// offsets[i+1]-offsets[i], and endBlocks-offsets[i] for the last entry. Only a mapping with
	// gaps or overlaps between entries stores the column explicitly (and fails Validate).
	lengths []uint32
	storage []uint32
	// Exactly one layer-index column is set for a non-empty mapping: layerIdx8 while the header
	// references at most maxLayers8 layers, layerIdx16 otherwise. Each addresses layers, with
	// its own sentinel for an Empty (zero) region.
	layerIdx8  []uint8
	layerIdx16 []uint16
	// endBlocks is the block at which the last entry ends.
	endBlocks uint64
}

const (
	nilLayerIdx8  = math.MaxUint8
	nilLayerIdx16 = math.MaxUint16

	// maxLayers8 is how many layers the one-byte index column can address, with nilLayerIdx8
	// reserved for empty regions.
	maxLayers8 = nilLayerIdx8
	// maxLayersPerHeader leaves nilLayerIdx16 reserved for empty regions.
	maxLayersPerHeader = nilLayerIdx16
)

// maxBlockIdx is the largest block index representable by the uint32 columns. At PageSize
// granularity this caps a single file at ~16 TiB; NewMapping errors if a value exceeds it.
const maxBlockIdx = math.MaxUint32

// NewMapping packs src into the compact representation. blockSize is the unit for the block
// indices and must divide every Offset, Length, and LayerOffset in src (callers pass PageSize,
// the universal granularity).
func NewMapping(blockSize uint64, src []LayerMap) (Mapping, error) {
	if blockSize == 0 {
		return Mapping{}, errors.New("compact mapping: block size cannot be zero")
	}
	if len(src) == 0 {
		return Mapping{blockSize: blockSize}, nil
	}
	idxByLayer := make(map[LayerID]uint16, 8)
	layers := make([]LayerID, 0, 8)
	offsets := make([]uint32, len(src))
	storage := make([]uint32, len(src))
	// Wide scratch column; narrowed to one byte per entry once the layer count is known.
	layerIdx := make([]uint16, len(src))
	contiguous := true
	var prevEnd uint64
	for i, m := range src {
		if m.Offset%blockSize != 0 {
			return Mapping{}, fmt.Errorf("compact mapping: offset %d at index %d not block-aligned to %d", m.Offset, i, blockSize)
		}
		if m.Length%blockSize != 0 {
			return Mapping{}, fmt.Errorf("compact mapping: length %d at index %d not block-aligned to %d", m.Length, i, blockSize)
		}
		if m.LayerOffset%blockSize != 0 {
			return Mapping{}, fmt.Errorf("compact mapping: layer offset %d at index %d not block-aligned to %d", m.LayerOffset, i, blockSize)
		}
		offBlocks := m.Offset / blockSize
		lenBlocks := m.Length / blockSize
		stoBlocks := m.LayerOffset / blockSize
		if offBlocks > maxBlockIdx || lenBlocks > maxBlockIdx || stoBlocks > maxBlockIdx {
			return Mapping{}, fmt.Errorf("compact mapping: block index out of uint32 range at entry %d", i)
		}
		if i > 0 && offBlocks != prevEnd {
			contiguous = false
		}
		prevEnd = offBlocks + lenBlocks
		idx := uint16(nilLayerIdx16)
		if m.Layer != Empty {
			var ok bool
			idx, ok = idxByLayer[m.Layer]
			if !ok {
				if len(layers) >= maxLayersPerHeader {
					return Mapping{}, fmt.Errorf("compact mapping: more than %d unique layer IDs", maxLayersPerHeader)
				}
				idx = uint16(len(layers))
				idxByLayer[m.Layer] = idx
				layers = append(layers, m.Layer)
			}
		}
		offsets[i] = uint32(offBlocks)
		storage[i] = uint32(stoBlocks)
		layerIdx[i] = idx
	}
	out := Mapping{
		blockSize: blockSize,
		layers:    layers,
		offsets:   offsets,
		storage:   storage,
		endBlocks: prevEnd,
	}
	out.layerIdx8, out.layerIdx16 = layerIdxColumns(len(layers), layerIdx)
	if !contiguous {
		out.lengths = make([]uint32, len(src))
		for i, m := range src {
			out.lengths[i] = uint32(m.Length / blockSize)
		}
	}
	return out, nil
}

// layerIdxColumns chooses the retained layer-index column from its wide form: one byte per entry
// when every layer fits, otherwise the wide column as is. Exactly one is non-nil.
func layerIdxColumns(nLayers int, wide []uint16) ([]uint8, []uint16) {
	if nLayers > maxLayers8 {
		return nil, wide
	}
	narrow := make([]uint8, len(wide))
	for i, idx := range wide {
		if idx == nilLayerIdx16 {
			narrow[i] = nilLayerIdx8
		} else {
			narrow[i] = uint8(idx)
		}
	}
	return narrow, nil
}

// layerIndex returns the i-th entry's index into layers, or -1 for an Empty region.
func (m Mapping) layerIndex(i int) int {
	if m.layerIdx8 != nil {
		if idx := m.layerIdx8[i]; idx != nilLayerIdx8 {
			return int(idx)
		}
		return -1
	}
	if idx := m.layerIdx16[i]; idx != nilLayerIdx16 {
		return int(idx)
	}
	return -1
}

// newMappingFromColumns builds a Mapping from already-decoded columns, avoiding the []LayerMap
// intermediate on the deserialize path. The entries must be contiguous, with the last one ending
// at endBlocks, so no lengths column is stored. All column slices must have the same length, and
// every layerIdx must index layers.
func newMappingFromColumns(blockSize uint64, layers []LayerID, offsets, storage []uint32, layerIdx []uint16, endBlocks uint64) (Mapping, error) {
	n := len(offsets)
	if len(storage) != n || len(layerIdx) != n {
		return Mapping{}, fmt.Errorf("compact mapping: column length mismatch (offsets=%d storage=%d layerIdx=%d)", n, len(storage), len(layerIdx))
	}
	if n > 0 && uint64(offsets[n-1]) > endBlocks {
		return Mapping{}, fmt.Errorf("compact mapping: last offset block %d beyond end block %d", offsets[n-1], endBlocks)
	}
	for i, li := range layerIdx {
		if li != nilLayerIdx16 && int(li) >= len(layers) {
			return Mapping{}, fmt.Errorf("compact mapping: layerIdx %d at entry %d out of range (%d layers)", li, i, len(layers))
		}
	}
	out := Mapping{blockSize: blockSize, layers: layers, offsets: offsets, storage: storage, endBlocks: endBlocks}
	out.layerIdx8, out.layerIdx16 = layerIdxColumns(len(layers), layerIdx)
	return out, nil
}

// Len returns the number of entries.
func (m Mapping) Len() int { return len(m.offsets) }

// BlockSize returns the block size used for block<->byte conversions.
func (m Mapping) BlockSize() uint64 { return m.blockSize }

// Layers returns the deduplicated layer IDs the mapping references, Base included, in order of
// first appearance. The slice is shared with the Mapping; callers must not mutate it.
func (m Mapping) Layers() []LayerID { return m.layers }

// lengthBlocks returns the i-th entry's length in blocks.
func (m Mapping) lengthBlocks(i int) uint32 {
	if m.lengths != nil {
		return m.lengths[i]
	}
	if i+1 < len(m.offsets) {
		return m.offsets[i+1] - m.offsets[i]
	}
	return uint32(m.endBlocks - uint64(m.offsets[i]))
}

// At materializes the i-th entry as a LayerMap. Panics if i is out of range.
func (m Mapping) At(i int) LayerMap {
	layer := Empty
	if li := m.layerIndex(i); li >= 0 {
		layer = m.layers[li]
	}
	return LayerMap{
		Offset:      uint64(m.offsets[i]) * m.blockSize,
		Length:      uint64(m.lengthBlocks(i)) * m.blockSize,
		Layer:       layer,
		LayerOffset: uint64(m.storage[i]) * m.blockSize,
	}
}

// All iterates the mapping, materializing each entry as a LayerMap.
func (m Mapping) All() iter.Seq2[int, LayerMap] {
	return func(yield func(int, LayerMap) bool) {
		for i := range m.offsets {
			if !yield(i, m.At(i)) {
				return
			}
		}
	}
}

// BytesByLayer sums the bytes attributed to each referenced layer (Base included, Empty not).
func (m Mapping) BytesByLayer() map[LayerID]uint64 {
	sums := make([]uint64, len(m.layers))
	for i := range m.offsets {
		if li := m.layerIndex(i); li >= 0 {
			sums[li] += uint64(m.lengthBlocks(i))
		}
	}
	out := make(map[LayerID]uint64, len(m.layers))
	for li, blocks := range sums {
		out[m.layers[li]] = blocks * m.blockSize
	}
	return out
}

// Slice materializes the full mapping as []LayerMap (~40 bytes/entry), for merging and tests.
func (m Mapping) Slice() []LayerMap {
	out := make([]LayerMap, len(m.offsets))
	for i := range m.offsets {
		out[i] = m.At(i)
	}
	return out
}

// Validate checks that entries are contiguous and cover exactly size bytes.
func (m Mapping) Validate(size uint64) error {
	var currentOffset uint64
	for i := range m.offsets {
		offset := uint64(m.offsets[i]) * m.blockSize
		length := uint64(m.lengthBlocks(i)) * m.blockSize
		if currentOffset != offset {
			return fmt.Errorf("mapping validation failed at index %d: expected offset %d, got %d", i, currentOffset, offset)
		}
		if currentOffset+length > size {
			return fmt.Errorf("mapping validation failed at index %d: %d (current offset) + %d (length) > %d (size)", i, currentOffset, length, size)
		}
		currentOffset += length
	}
	if currentOffset != size {
		return fmt.Errorf("mapping validation failed: total %d != size %d", currentOffset, size)
	}
	return nil
}

// SearchOffset returns the first index i whose byte offset is strictly greater than off,
// matching sort.Search semantics on Offset. It compares in block units, so entries are never
// materialized.
func (m Mapping) SearchOffset(off int64) int {
	if off < 0 || len(m.offsets) == 0 {
		return 0
	}
	target := uint64(off) / m.blockSize
	lo, hi := 0, len(m.offsets)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if uint64(m.offsets[mid]) > target {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}
