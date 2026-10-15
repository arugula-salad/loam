// Derived from github.com/e2b-dev/infra, packages/shared/pkg/storage/compress_frame_table.go at
// commit cf536c2 (Copyright 2023 FoundryLabs, Inc., Apache-2.0, see ../LICENSE-e2b). Modified for
// loam: moved into the header package, the FullFrameTable wrapper dropped (loam's uploads always
// produce whole tables and only serialization trims them).

package header

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
)

type CompressionType byte

const (
	CompressionNone = CompressionType(iota)
	CompressionZstd
	CompressionLZ4
)

// maxDeserializedFrames caps the number of frames read from a serialized FrameTable to prevent
// OOM from corrupted headers. 1M frames = 2 TiB uncompressed at 2 MiB frame size.
const maxDeserializedFrames = 1024 * 1024

// FrameTable is a decompression index for compressed layer objects.
//
// Dirty blocks are grouped into frames and each frame is compressed independently. Two address
// spaces describe the same data:
//
//	U-space (uncompressed):  |-- frame 0 (2M) --|-- frame 1 (2M) --| ...
//	C-space (compressed):    |-- f0 (.6M) --|-- f1 (.7M) --| ...
//
// Each frame is a frameEntry with absolute offsets (StartU, StartC) and sizes (SizeU, SizeC).
// Lookups are a binary search on StartU. A table is immutable after construction and safe to
// share across goroutines. Sparse tables (gaps between entries) are supported: a header carries
// only the frames its mapping references. A nil table means the object is stored uncompressed.
type FrameTable struct {
	compressionType CompressionType
	entries         []frameEntry // sorted by StartU
}

// FrameSize holds the uncompressed (U) and compressed (C) byte size of a single frame.
type FrameSize struct {
	U int32
	C int32
}

// Range is a byte range [Offset, Offset+Length).
type Range struct {
	Offset int64
	Length int
}

func (r Range) String() string { return fmt.Sprintf("%d/%d", r.Offset, r.Length) }

// frameEntry stores one frame as an absolute start offset plus size in both address spaces.
// Fields must be exported for encoding/binary. Two int64 then two int32 = 24 bytes, no padding.
type frameEntry struct {
	StartU int64
	StartC int64
	SizeU  int32
	SizeC  int32
}

func (e frameEntry) endU() int64 { return e.StartU + int64(e.SizeU) }
func (e frameEntry) endC() int64 { return e.StartC + int64(e.SizeC) }

// NewFrameTable creates a FrameTable from consecutive frame sizes, computing absolute offsets
// starting from zero.
func NewFrameTable(ct CompressionType, sizes []FrameSize) *FrameTable {
	entries := make([]frameEntry, len(sizes))
	var u, c int64
	for i, s := range sizes {
		entries[i] = frameEntry{StartU: u, StartC: c, SizeU: s.U, SizeC: s.C}
		u += int64(s.U)
		c += int64(s.C)
	}
	return &FrameTable{compressionType: ct, entries: entries}
}

// CompressionType returns the compression type. Nil-safe: CompressionNone for nil.
func (ft *FrameTable) CompressionType() CompressionType {
	if ft == nil {
		return CompressionNone
	}
	return ft.compressionType
}

// IsCompressed reports whether ft is non-nil and has a compression type set.
func (ft *FrameTable) IsCompressed() bool {
	return ft != nil && ft.compressionType != CompressionNone
}

func (ft *FrameTable) NumFrames() int {
	if ft == nil {
		return 0
	}
	return len(ft.entries)
}

func (ft *FrameTable) FrameAt(i int) (startU, endU, startC, endC int64) {
	e := ft.entries[i]
	return e.StartU, e.endU(), e.StartC, e.endC()
}

// UncompressedSize returns the total uncompressed size across all frames.
func (ft *FrameTable) UncompressedSize() int64 {
	if ft == nil {
		return 0
	}
	var total int64
	for _, e := range ft.entries {
		total += int64(e.SizeU)
	}
	return total
}

// CompressedSize returns the total compressed size across all frames.
func (ft *FrameTable) CompressedSize() int64 {
	if ft == nil {
		return 0
	}
	var total int64
	for _, e := range ft.entries {
		total += int64(e.SizeC)
	}
	return total
}

// locate finds the frame containing the given uncompressed offset.
func (ft *FrameTable) locate(offset int64) (frameEntry, error) {
	if ft == nil {
		return frameEntry{}, errors.New("locate called with nil frame table: data is not compressed")
	}
	// Binary search: find the last entry whose StartU <= offset.
	i := sort.Search(len(ft.entries), func(i int) bool { return ft.entries[i].StartU > offset }) - 1
	if i < 0 {
		return frameEntry{}, fmt.Errorf("offset %d not found in frame table", offset)
	}
	e := ft.entries[i]
	if offset >= e.endU() {
		return frameEntry{}, fmt.Errorf("offset %d is in a gap (not covered by any frame)", offset)
	}
	return e, nil
}

// LocateCompressed maps a frame-aligned U-space offset to its C-space byte range: the bytes to
// fetch from the store. A mid-frame offset would fetch and decode that frame from its start,
// silently returning data for the wrong position, so it is refused; align via
// LocateUncompressed first.
func (ft *FrameTable) LocateCompressed(offset int64) (Range, error) {
	e, err := ft.locate(offset)
	if err != nil {
		return Range{}, err
	}
	if offset != e.StartU {
		return Range{}, fmt.Errorf("offset %d is not frame-aligned (frame starts at %d); align via LocateUncompressed", offset, e.StartU)
	}
	return Range{Offset: e.StartC, Length: int(e.SizeC)}, nil
}

// LocateUncompressed returns the uncompressed byte range of the frame containing offset.
func (ft *FrameTable) LocateUncompressed(offset int64) (Range, error) {
	e, err := ft.locate(offset)
	if err != nil {
		return Range{}, err
	}
	return Range{Offset: e.StartU, Length: int(e.SizeU)}, nil
}

// Serialize writes the frame table to w in binary little-endian format. Nil-safe: writes zeros
// for type and count.
func (ft *FrameTable) Serialize(w io.Writer) error {
	var ct CompressionType
	var n int
	if ft != nil && ft.compressionType != CompressionNone {
		ct = ft.compressionType
		n = len(ft.entries)
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(ct)); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(n)); err != nil {
		return err
	}
	if n > 0 {
		return binary.Write(w, binary.LittleEndian, ft.entries)
	}
	return nil
}

// DeserializeFrameTable reads a FrameTable from r. Returns nil for uncompressed layers
// (compressionType=0 or numFrames=0).
func DeserializeFrameTable(r io.Reader) (*FrameTable, error) {
	var ct, n uint32
	if err := binary.Read(r, binary.LittleEndian, &ct); err != nil {
		return nil, fmt.Errorf("read compression type: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return nil, fmt.Errorf("read frame count: %w", err)
	}
	if ct == 0 && n > 0 {
		return nil, fmt.Errorf("compression type is 0 but frame count is %d: corrupted header", n)
	}
	if ct == 0 || n == 0 {
		return nil, nil
	}
	if ct > uint32(CompressionLZ4) {
		return nil, fmt.Errorf("unknown compression type %d", ct)
	}
	if n > maxDeserializedFrames {
		return nil, fmt.Errorf("frame count %d exceeds maximum %d", n, maxDeserializedFrames)
	}
	entries := make([]frameEntry, n)
	if err := binary.Read(r, binary.LittleEndian, entries); err != nil {
		return nil, fmt.Errorf("read frame entries: %w", err)
	}
	for i := range entries {
		if entries[i].SizeU <= 0 || entries[i].SizeC <= 0 {
			return nil, fmt.Errorf("frame %d has zero or negative size: SizeU=%d SizeC=%d", i, entries[i].SizeU, entries[i].SizeC)
		}
		if i > 0 && entries[i].StartU < entries[i-1].endU() {
			return nil, fmt.Errorf("frame %d StartU %d < previous endU %d: U-entries not sorted", i, entries[i].StartU, entries[i-1].endU())
		}
		if i > 0 && entries[i].StartC < entries[i-1].endC() {
			return nil, fmt.Errorf("frame %d StartC %d < previous endC %d: C-entries not sorted", i, entries[i].StartC, entries[i-1].endC())
		}
	}
	return &FrameTable{compressionType: CompressionType(ct), entries: entries}, nil
}

// TrimToRanges returns a new FrameTable containing only the frames that overlap at least one of
// the given U-space byte ranges. Used during header serialization to keep headers compact when a
// layer has many frames but only a few are referenced by this header. Nil-safe: returns ft
// unchanged when ft is nil or ranges is empty.
func (ft *FrameTable) TrimToRanges(ranges []Range) *FrameTable {
	if ft == nil || len(ft.entries) == 0 || len(ranges) == 0 {
		return ft
	}
	keep := make([]bool, len(ft.entries))
	kept := 0
	for _, r := range ranges {
		startU, endU := r.Offset, r.Offset+int64(r.Length)
		// Binary search: first frame whose EndU > startU.
		lo := sort.Search(len(ft.entries), func(i int) bool { return ft.entries[i].endU() > startU })
		for i := lo; i < len(ft.entries) && ft.entries[i].StartU < endU; i++ {
			if !keep[i] {
				keep[i] = true
				kept++
			}
		}
	}
	if kept == len(ft.entries) {
		return ft
	}
	trimmed := make([]frameEntry, 0, kept)
	for i, e := range ft.entries {
		if keep[i] {
			trimmed = append(trimmed, e)
		}
	}
	return &FrameTable{compressionType: ft.compressionType, entries: trimmed}
}

// Suffix is the object-name suffix for a layer stored with this compression.
func (ct CompressionType) Suffix() string {
	switch ct {
	case CompressionZstd:
		return ".zst"
	case CompressionLZ4:
		return ".lz4"
	default:
		return ""
	}
}

func (ct CompressionType) String() string {
	switch ct {
	case CompressionZstd:
		return "zstd"
	case CompressionLZ4:
		return "lz4"
	default:
		return "none"
	}
}

// ParseCompressionType converts "zstd", "lz4" or "none" to a CompressionType.
func ParseCompressionType(s string) (CompressionType, error) {
	switch s {
	case "zstd":
		return CompressionZstd, nil
	case "lz4":
		return CompressionLZ4, nil
	case "none", "":
		return CompressionNone, nil
	default:
		return 0, fmt.Errorf("unknown compression %q (want zstd, lz4 or none)", s)
	}
}
