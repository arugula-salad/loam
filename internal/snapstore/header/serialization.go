// Derived from github.com/e2b-dev/infra, packages/shared/pkg/storage/header/serialization.go,
// serialization_v4.go and serialization_v5.go at commit cf536c2 (Copyright 2023 FoundryLabs,
// Inc., Apache-2.0, see ../LICENSE-e2b). Modified for loam: only V5 (V3 and V4 never existed
// here), builds renamed layers, storage access moved to the caller.

package header

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"

	lz4 "github.com/pierrec/lz4/v4"
)

// The serialized header is
//
//	[Metadata, 64 bytes] [uint8 flags] [uint32 uncompressed block size] [LZ4(block)]
//
// and the block is the layers section followed by the mapping section:
//
//	uint32 L, then L × { [16]byte layer, int64 size, [32]byte checksum, frame table }
//	uint32 M                       distinct non-empty layer-id count
//	M × [16]byte                   layer-id table
//	uint32 N                       non-empty entry count
//	N × uvarint                    offset-block deltas (offset[i]-offset[i-1])
//	N × uvarint                    length in blocks
//	N × varint                     storage-block deltas (can go negative when switching layers)
//	N × uvarint                    layer-id table index
//
// The columnar, varint-coded mapping is far smaller and far more compressible than fixed
// records for the large, fragmented headers page-granular memory diffs produce. Empty ranges are
// not written: the reader reconstructs them as the gaps.

const (
	sizePrefixLen = 4
	flagsLen      = 1
	// flagIncomplete is bit 0 of the flags byte (E2B: a build whose upload had not finalized).
	// Loam never stores such a header and refuses to read one.
	flagIncomplete    uint8 = 1 << 0
	maxMappingEntries       = 8 << 20
)

// maxUncompressedHeaderSize caps the uncompressed block as an anti-decompression-bomb guard. A
// var so tests can lower it.
var maxUncompressedHeaderSize = 256 << 20

// metadataSize is the binary size of the Metadata struct.
var metadataSize = binary.Size(Metadata{})

type serializableLayerInfo struct {
	Layer    LayerID
	FileSize int64
	Checksum [32]byte
}

var (
	layerInfoSize        = binary.Size(serializableLayerInfo{})
	frameTableHeaderSize = 2 * binary.Size(uint32(0))
)

// Serialize encodes a header for the store. It refuses a provisional header, and one too big for
// Deserialize to accept: that would upload fine and then fail every restore.
func Serialize(h *Header) ([]byte, error) {
	if h.IsProvisional() {
		return nil, errors.New("refusing to serialize a provisional header")
	}
	if h.Metadata.Version != MetadataVersion {
		return nil, fmt.Errorf("unsupported header version %d", h.Metadata.Version)
	}
	for _, id := range h.Mapping.Layers() {
		if _, ok := h.Layers[id]; id.IsStored() && !ok {
			return nil, fmt.Errorf("header references layer %s it has no entry for", id)
		}
	}
	var metaBuf bytes.Buffer
	if err := binary.Write(&metaBuf, binary.LittleEndian, h.Metadata); err != nil {
		return nil, fmt.Errorf("write metadata: %w", err)
	}
	var block bytes.Buffer
	if err := writeLayersSection(&block, h.Layers, h.Mapping); err != nil {
		return nil, err
	}
	writeMappingSection(&block, h.Mapping)
	blockBytes := block.Bytes()
	if len(blockBytes) > maxUncompressedHeaderSize {
		return nil, fmt.Errorf("header block %d bytes exceeds cap %d", len(blockBytes), maxUncompressedHeaderSize)
	}
	compressed, err := compressLZ4(blockBytes)
	if err != nil {
		return nil, fmt.Errorf("lz4-compress header block: %w", err)
	}
	out := make([]byte, metadataSize+flagsLen+sizePrefixLen+len(compressed))
	copy(out, metaBuf.Bytes())
	out[metadataSize] = 0
	binary.LittleEndian.PutUint32(out[metadataSize+flagsLen:], uint32(len(blockBytes)))
	copy(out[metadataSize+flagsLen+sizePrefixLen:], compressed)
	return out, nil
}

// Deserialize decodes a header written by Serialize.
func Deserialize(data []byte) (*Header, error) {
	if len(data) < metadataSize+flagsLen+sizePrefixLen {
		return nil, fmt.Errorf("header too short: %d bytes", len(data))
	}
	var md Metadata
	if err := binary.Read(bytes.NewReader(data[:metadataSize]), binary.LittleEndian, &md); err != nil {
		return nil, fmt.Errorf("read metadata: %w", err)
	}
	if md.Version != MetadataVersion {
		return nil, fmt.Errorf("unsupported header version %d", md.Version)
	}
	if md.BlockSize == 0 {
		return nil, errors.New("header has zero block size")
	}
	if md.Size%PageSize != 0 {
		return nil, fmt.Errorf("header size %d is not page-aligned", md.Size)
	}
	rest := data[metadataSize:]
	if rest[0]&flagIncomplete != 0 {
		return nil, errors.New("header is flagged incomplete")
	}
	size := binary.LittleEndian.Uint32(rest[flagsLen:])
	if uint64(size) > uint64(maxUncompressedHeaderSize) {
		return nil, fmt.Errorf("header uncompressed size %d exceeds cap %d", size, maxUncompressedHeaderSize)
	}
	decompressed, err := decompressLZ4(rest[flagsLen+sizePrefixLen:], int(size))
	if err != nil {
		return nil, fmt.Errorf("lz4-decompress header block: %w", err)
	}
	if len(decompressed) != int(size) {
		return nil, fmt.Errorf("header decompressed size %d != prefix %d", len(decompressed), size)
	}
	r := bytes.NewReader(decompressed)
	layers, err := readLayersSection(r)
	if err != nil {
		return nil, err
	}
	// The compact mapping is encoded in PageSize units, not the file's logical block size.
	mapping, err := readMappingSection(r, PageSize, md.Size)
	if err != nil {
		return nil, err
	}
	if r.Len() != 0 {
		return nil, fmt.Errorf("%d trailing bytes after the mapping", r.Len())
	}
	h := &Header{Metadata: &md, Layers: layers, Mapping: mapping}
	for _, id := range mapping.Layers() {
		if _, ok := layers[id]; id.IsStored() && !ok {
			return nil, fmt.Errorf("header references layer %s it has no entry for", id)
		}
	}
	return h, nil
}

// compressLZ4 compresses the header block with the LZ4 frame format; the settings are part of
// the wire format.
func compressLZ4(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(len(data))
	w := lz4.NewWriter(&buf)
	if err := w.Apply(
		lz4.BlockSizeOption(lz4.Block4Mb),
		lz4.BlockChecksumOption(true),
		lz4.ChecksumOption(true),
		lz4.CompressionLevelOption(lz4.Fast),
	); err != nil {
		return nil, fmt.Errorf("lz4 options: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decompressLZ4 reads up to expected+1 bytes; the caller rejects any overrun. The bound defends
// against LZ4's ~255x expansion on malformed input.
func decompressLZ4(src []byte, expected int) ([]byte, error) {
	r := lz4.NewReader(bytes.NewReader(src))
	buf := bytes.NewBuffer(make([]byte, 0, expected))
	if _, err := io.CopyN(buf, r, int64(expected)+1); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeLayersSection writes [uint32 count] then, per layer (sorted by ID for determinism), the
// fixed info plus a frame table trimmed to the ranges the mapping references.
func writeLayersSection(block *bytes.Buffer, layers map[LayerID]LayerData, mapping Mapping) error {
	ids := make([]LayerID, 0, len(layers))
	for id := range layers {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, compareIDs)
	if err := binary.Write(block, binary.LittleEndian, uint32(len(ids))); err != nil {
		return err
	}
	ranges := relevantRanges(mapping)
	for _, id := range ids {
		ld := layers[id]
		entry := serializableLayerInfo{Layer: id, FileSize: ld.Size, Checksum: ld.Checksum}
		if err := binary.Write(block, binary.LittleEndian, &entry); err != nil {
			return err
		}
		if err := ld.Frames.TrimToRanges(ranges[id]).Serialize(block); err != nil {
			return fmt.Errorf("write frame table of layer %s: %w", id, err)
		}
	}
	return nil
}

func readLayersSection(r *bytes.Reader) (map[LayerID]LayerData, error) {
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return nil, fmt.Errorf("read layer count: %w", err)
	}
	if uint64(n) > uint64(r.Len())/uint64(layerInfoSize+frameTableHeaderSize) {
		return nil, fmt.Errorf("layer count %d exceeds remaining %d bytes", n, r.Len())
	}
	layers := make(map[LayerID]LayerData, n)
	for range n {
		var entry serializableLayerInfo
		if err := binary.Read(r, binary.LittleEndian, &entry); err != nil {
			return nil, fmt.Errorf("read layer info: %w", err)
		}
		ft, err := DeserializeFrameTable(r)
		if err != nil {
			return nil, fmt.Errorf("read frame table for layer %s: %w", entry.Layer, err)
		}
		layers[entry.Layer] = LayerData{Size: entry.FileSize, Checksum: entry.Checksum, Frames: ft}
	}
	return layers, nil
}

// relevantRanges groups the mapping into per-layer U-space ranges for frame table trimming.
func relevantRanges(m Mapping) map[LayerID][]Range {
	ranges := make(map[LayerID][]Range)
	for _, e := range m.All() {
		ranges[e.Layer] = append(ranges[e.Layer], Range{Offset: int64(e.LayerOffset), Length: int(e.Length)})
	}
	return ranges
}

// writeMappingSection writes the columnar, varint-coded mapping (see the format above).
func writeMappingSection(block *bytes.Buffer, m Mapping) {
	var n int
	for i := range m.offsets {
		if m.layerIndex(i) >= 0 {
			n++
		}
	}
	binary.Write(block, binary.LittleEndian, uint32(len(m.layers)))
	for _, id := range m.layers {
		block.Write(id[:])
	}
	binary.Write(block, binary.LittleEndian, uint32(n))
	var buf [binary.MaxVarintLen64]byte
	var prevOffset uint64
	for i, v := range m.offsets {
		if m.layerIndex(i) < 0 {
			continue
		}
		block.Write(buf[:binary.PutUvarint(buf[:], uint64(v)-prevOffset)])
		prevOffset = uint64(v)
	}
	for i := range m.offsets {
		if m.layerIndex(i) < 0 {
			continue
		}
		block.Write(buf[:binary.PutUvarint(buf[:], uint64(m.lengthBlocks(i)))])
	}
	var prevStorage int64
	for i, v := range m.storage {
		if m.layerIndex(i) < 0 {
			continue
		}
		block.Write(buf[:binary.PutVarint(buf[:], int64(v)-prevStorage)])
		prevStorage = int64(v)
	}
	for i := range m.offsets {
		li := m.layerIndex(i)
		if li < 0 {
			continue
		}
		block.Write(buf[:binary.PutUvarint(buf[:], uint64(li))])
	}
}

// readMappingSection reads the columnar mapping and reconstructs the compact Mapping directly,
// filling every gap with an Empty entry.
func readMappingSection(r *bytes.Reader, blockSize, size uint64) (Mapping, error) {
	sizeBlocks := (size + blockSize - 1) / blockSize
	if sizeBlocks > maxBlockIdx {
		return Mapping{}, fmt.Errorf("mapping size block %d exceeds uint32", sizeBlocks)
	}
	var m, n uint32
	if err := binary.Read(r, binary.LittleEndian, &m); err != nil {
		return Mapping{}, fmt.Errorf("read mapping layer count: %w", err)
	}
	if m > maxLayersPerHeader {
		return Mapping{}, fmt.Errorf("mapping layer count %d exceeds maximum %d", m, maxLayersPerHeader)
	}
	if uint64(m) > uint64(r.Len())/16 {
		return Mapping{}, fmt.Errorf("mapping layer count %d exceeds remaining %d bytes", m, r.Len())
	}
	layers := make([]LayerID, m)
	for i := range layers {
		if _, err := io.ReadFull(r, layers[i][:]); err != nil {
			return Mapping{}, fmt.Errorf("read mapping layer id %d: %w", i, err)
		}
	}
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return Mapping{}, fmt.Errorf("read mapping count: %w", err)
	}
	if n > maxMappingEntries {
		return Mapping{}, fmt.Errorf("mapping count %d exceeds maximum %d", n, maxMappingEntries)
	}
	if n == 0 {
		if size == 0 {
			return newMappingFromColumns(blockSize, layers, nil, nil, nil, 0)
		}
		return newMappingFromColumns(blockSize, layers, []uint32{0}, []uint32{0}, []uint16{nilLayerIdx16}, sizeBlocks)
	}
	// Each entry needs at least one byte in each of the four encoded columns, which bounds a
	// crafted count before anything is allocated.
	if uint64(n) > uint64(r.Len())/4 {
		return Mapping{}, fmt.Errorf("mapping count %d exceeds remaining %d bytes", n, r.Len())
	}
	encOffsets := make([]uint32, n)
	encLengths := make([]uint32, n)
	encStorage := make([]uint32, n)
	encLayerIdx := make([]uint16, n)
	var prevOffset uint64
	for i := range int(n) {
		d, err := binary.ReadUvarint(r)
		if err != nil {
			return Mapping{}, fmt.Errorf("read offset delta %d: %w", i, err)
		}
		// Bound the delta before adding so the running sum can't overflow and wrap past the
		// maxBlockIdx check.
		if d > maxBlockIdx {
			return Mapping{}, fmt.Errorf("offset delta %d at entry %d exceeds uint32", d, i)
		}
		prevOffset += d
		if prevOffset > maxBlockIdx {
			return Mapping{}, fmt.Errorf("offset block %d at entry %d exceeds uint32", prevOffset, i)
		}
		encOffsets[i] = uint32(prevOffset)
	}
	for i := range int(n) {
		v, err := binary.ReadUvarint(r)
		if err != nil {
			return Mapping{}, fmt.Errorf("read length %d: %w", i, err)
		}
		if v > maxBlockIdx {
			return Mapping{}, fmt.Errorf("length block %d at entry %d exceeds uint32", v, i)
		}
		encLengths[i] = uint32(v)
	}
	var prevStorage int64
	for i := range int(n) {
		d, err := binary.ReadVarint(r)
		if err != nil {
			return Mapping{}, fmt.Errorf("read storage delta %d: %w", i, err)
		}
		prevStorage += d
		if prevStorage < 0 || prevStorage > maxBlockIdx {
			return Mapping{}, fmt.Errorf("storage block %d at entry %d out of range", prevStorage, i)
		}
		encStorage[i] = uint32(prevStorage)
	}
	for i := range int(n) {
		v, err := binary.ReadUvarint(r)
		if err != nil {
			return Mapping{}, fmt.Errorf("read layer index %d: %w", i, err)
		}
		if v >= uint64(m) {
			return Mapping{}, fmt.Errorf("layer index %d at entry %d out of range (%d layers)", v, i, m)
		}
		encLayerIdx[i] = uint16(v)
	}
	// The reconstruction fills every gap with an Empty entry and rejects overlaps, so the result
	// is contiguous from block 0 to sizeBlocks and the wire lengths need not be retained.
	offsets := make([]uint32, 0, 2*int(n)+1)
	storage := make([]uint32, 0, 2*int(n)+1)
	layerIdx := make([]uint16, 0, 2*int(n)+1)
	appendEntry := func(off, sto uint32, idx uint16) {
		offsets = append(offsets, off)
		storage = append(storage, sto)
		layerIdx = append(layerIdx, idx)
	}
	var current uint32
	for i := range int(n) {
		off, length := encOffsets[i], encLengths[i]
		if length == 0 {
			return Mapping{}, fmt.Errorf("zero-length mapping at entry %d", i)
		}
		if off < current {
			return Mapping{}, fmt.Errorf("mapping offset %d at entry %d overlaps previous end %d", off, i, current)
		}
		if off > current {
			appendEntry(current, 0, nilLayerIdx16)
		}
		end := uint64(off) + uint64(length)
		if end > sizeBlocks {
			return Mapping{}, fmt.Errorf("mapping end block %d at entry %d exceeds size %d", end, i, sizeBlocks)
		}
		appendEntry(off, encStorage[i], encLayerIdx[i])
		current = uint32(end)
	}
	if uint64(current) < sizeBlocks {
		appendEntry(current, 0, nilLayerIdx16)
	}
	return newMappingFromColumns(blockSize, layers, slices.Clip(offsets), slices.Clip(storage), slices.Clip(layerIdx), sizeBlocks)
}
