// Derived from github.com/e2b-dev/infra, packages/shared/pkg/storage/header/mapping.go and
// block.go at commit cf536c2 (Copyright 2023 FoundryLabs, Inc., Apache-2.0, see
// ../LICENSE-e2b). Modified for loam: builds renamed layers, roaring replaced by Bitmap, and
// Base ranges (unchanged from the local base file) merge like any layer.

package header

import (
	"fmt"
	"slices"
)

// LayerMap maps a byte range of the file to a region of a layer's object (E2B's BuildMap).
// Offset, Length and LayerOffset are in bytes. Layer is Empty for a range of zeros and Base for
// a range unchanged from the base file, whose LayerOffset is then the offset in that file.
type LayerMap struct {
	// Offset is the starting position of this range in the file.
	Offset      uint64
	Length      uint64
	Layer       LayerID
	LayerOffset uint64
}

// Format renders the mapping as
//
//	startBlock-endBlock [offset, offset+length) := [layerOffset, layerOffset+length) ⊂ layer, length in bytes
//
// for debugging and tests.
func (m LayerMap) Format(blockSize uint64) string {
	r := fmt.Sprintf("%d-%d", m.Offset/blockSize, (m.Offset+m.Length)/blockSize)
	return fmt.Sprintf("%-14s [%11d,%11d) := [%11d,%11d) ⊂ %s, %d B",
		r, m.Offset, m.Offset+m.Length, m.LayerOffset, m.LayerOffset+m.Length, m.Layer, m.Length)
}

// TotalBlocks is the number of blocks of blockSize that cover size bytes.
func TotalBlocks(size, blockSize int64) int64 { return (size + blockSize - 1) / blockSize }

// BlocksOffsets returns the offset of every block of blockSize within size bytes.
func BlocksOffsets(size, blockSize int64) []int64 {
	offsets := make([]int64, TotalBlocks(size, blockSize))
	for i := range offsets {
		offsets[i] = BlockOffset(int64(i), blockSize)
	}
	return offsets
}

// BlockIdx returns the index of the block containing byte offset off (floor division).
func BlockIdx(off, blockSize int64) int64 { return off / blockSize }

// BlockCeilIdx returns the index of the first block after byte offset off (ceiling division).
func BlockCeilIdx(off, blockSize int64) int64 { return (off + blockSize - 1) / blockSize }

func BlockOffset(idx, blockSize int64) int64 { return idx * blockSize }

// CreateMapping maps each run of dirty blocks to layer, packing the runs contiguously in the
// layer's object in order: the layout a layer object has.
func CreateMapping(layer LayerID, dirty *Bitmap, blockSize int64) []LayerMap {
	var mappings []LayerMap
	var layerOffset uint64
	for start, endExcl := range dirty.Ranges() {
		m := LayerMap{
			Offset:      uint64(BlockOffset(int64(start), blockSize)),
			Length:      uint64(BlockOffset(int64(endExcl)-int64(start), blockSize)),
			Layer:       layer,
			LayerOffset: layerOffset,
		}
		mappings = append(mappings, m)
		layerOffset += m.Length
	}
	return mappings
}

// createIdentityMapping is like CreateMapping but sets each range's LayerOffset equal to its
// file Offset instead of packing the ranges contiguously. It is for a layer whose data is
// addressed by absolute file offset (the local, unpacked file a pause just wrote) rather than a
// packed layer object: a provisional, local-only header while the upload runs.
func createIdentityMapping(layer LayerID, dirty *Bitmap, blockSize int64) []LayerMap {
	var mappings []LayerMap
	for start, endExcl := range dirty.Ranges() {
		off := uint64(BlockOffset(int64(start), blockSize))
		mappings = append(mappings, LayerMap{
			Offset:      off,
			Length:      uint64(BlockOffset(int64(endExcl)-int64(start), blockSize)),
			Layer:       layer,
			LayerOffset: off,
		})
	}
	return mappings
}

// MergeMappings merges two sets of mappings, diff over base. Both are sorted by Offset; base
// must cover the whole size, and so does the result.
func MergeMappings(baseMapping []LayerMap, diffMapping []LayerMap) []LayerMap {
	if len(diffMapping) == 0 {
		return baseMapping
	}
	baseMapping = slices.Clone(baseMapping)
	mappings := make([]LayerMap, 0)
	var baseIdx, diffIdx int
	for baseIdx < len(baseMapping) && diffIdx < len(diffMapping) {
		base := &baseMapping[baseIdx]
		diff := diffMapping[diffIdx]
		if base.Length == 0 {
			baseIdx++
			continue
		}
		if diff.Length == 0 {
			diffIdx++
			continue
		}
		// base is before diff and there is no overlap: no diff will overlap it.
		if base.Offset+base.Length <= diff.Offset {
			mappings = append(mappings, *base)
			baseIdx++
			continue
		}
		// diff is before base and there is no overlap.
		if diff.Offset+diff.Length <= base.Offset {
			mappings = append(mappings, diff)
			diffIdx++
			continue
		}
		// base is inside diff: drop it, the diff covers it.
		if base.Offset >= diff.Offset && base.Offset+base.Length <= diff.Offset+diff.Length {
			baseIdx++
			continue
		}
		// diff is inside base (the ends may coincide): split base into a left part (kept if not
		// empty), the diff, and a right part that stays in baseMapping for the next round.
		if diff.Offset >= base.Offset && diff.Offset+diff.Length <= base.Offset+base.Length {
			leftBaseLength := int64(diff.Offset) - int64(base.Offset)
			if leftBaseLength > 0 {
				mappings = append(mappings, LayerMap{
					Offset:      base.Offset,
					Length:      uint64(leftBaseLength),
					Layer:       base.Layer,
					LayerOffset: base.LayerOffset,
				})
			}
			mappings = append(mappings, diff)
			diffIdx++
			rightBaseShift := int64(diff.Offset) + int64(diff.Length) - int64(base.Offset)
			rightBaseLength := int64(base.Length) - rightBaseShift
			if rightBaseLength > 0 {
				baseMapping[baseIdx] = LayerMap{
					Offset:      base.Offset + uint64(rightBaseShift),
					Length:      uint64(rightBaseLength),
					Layer:       base.Layer,
					LayerOffset: base.LayerOffset + uint64(rightBaseShift),
				}
			} else {
				baseIdx++
			}
			continue
		}
		// base starts after diff and they overlap: keep the diff, and the right part of base.
		if base.Offset > diff.Offset {
			mappings = append(mappings, diff)
			diffIdx++
			rightBaseShift := int64(diff.Offset) + int64(diff.Length) - int64(base.Offset)
			rightBaseLength := int64(base.Length) - rightBaseShift
			if rightBaseLength > 0 {
				baseMapping[baseIdx] = LayerMap{
					Offset:      base.Offset + uint64(rightBaseShift),
					Length:      uint64(rightBaseLength),
					Layer:       base.Layer,
					LayerOffset: base.LayerOffset + uint64(rightBaseShift),
				}
			} else {
				baseIdx++
			}
			continue
		}
		// diff starts after base and they overlap: keep the left part of base.
		if diff.Offset > base.Offset {
			leftBaseLength := int64(diff.Offset) - int64(base.Offset)
			if leftBaseLength > 0 {
				mappings = append(mappings, LayerMap{
					Offset:      base.Offset,
					Length:      uint64(leftBaseLength),
					Layer:       base.Layer,
					LayerOffset: base.LayerOffset,
				})
			}
			baseIdx++
			continue
		}
		panic(fmt.Sprintf("invalid case during merge mappings: %+v %+v", base, diff))
	}
	mappings = append(mappings, baseMapping[baseIdx:]...)
	mappings = append(mappings, diffMapping[diffIdx:]...)
	return mappings
}

// NormalizeMappings joins adjacent mappings of the same layer.
//
// Same-layer runs are only merged when their storage is contiguous, i.e. LayerOffset == current
// end. The read path resolves bytes as LayerOffset + shift, so merging a run across a storage
// discontinuity would silently remap reads to the wrong bytes. Empty regions are served as zeros
// and never read from storage, so their storage offset is irrelevant and they always merge.
//
// The result is a clone, so the oversized intermediate (cap == len(input)) is released.
func NormalizeMappings(mappings []LayerMap) []LayerMap {
	if len(mappings) == 0 {
		return nil
	}
	result := make([]LayerMap, 0, len(mappings))
	current := mappings[0]
	for _, mp := range mappings[1:] {
		storageContiguous := mp.Layer == Empty || mp.LayerOffset == current.LayerOffset+current.Length
		if mp.Layer == current.Layer && storageContiguous {
			current.Length += mp.Length
		} else {
			result = append(result, current)
			current = mp
		}
	}
	result = append(result, current)
	return slices.Clone(result)
}
