// Derived from github.com/e2b-dev/infra, packages/shared/pkg/storage/header/header.go and
// metadata.go at commit cf536c2 (Copyright 2023 FoundryLabs, Inc., Apache-2.0, see
// ../LICENSE-e2b). Modified for loam: builds renamed layers, only the V5 format, the pre-V3
// normalize-fix leniency dropped (every loam header is strict), Zero and Base starting headers
// added.

// Package header is the snapshot header format: per file of a snapshot (memory, disk), a
// flattened map from every offset to (layer, offset in the layer's object), Empty (zeros) or
// Base (the local base file), plus each referenced layer's size, checksum and frame table.
// Reading a snapshot never walks a chain of parents: each generation's header is complete.
package header

import (
	"errors"
	"fmt"
	"maps"
	"slices"
)

const (
	PageSize     = 4 << 10 // 4 KiB
	HugepageSize = 2 << 20 // 2 MiB
)

// EmptyHugePage is a page of zeros to serve Empty ranges from.
var EmptyHugePage = make([]byte, HugepageSize)

// MetadataVersion is the only header format loam writes and reads: E2B's V5 (a columnar,
// varint-coded mapping section and a layer table with frame tables, LZ4-compressed).
const MetadataVersion = 5

// Metadata is the header's fixed prefix, written as is (little-endian, 64 bytes).
type Metadata struct {
	Version    uint64
	BlockSize  uint64
	Size       uint64
	Generation uint64
	// Layer is the layer this generation wrote (E2B's BuildId).
	Layer LayerID
	// BaseLayer is the first generation's layer (E2B's BaseBuildId), carried along unchanged.
	BaseLayer LayerID
}

// NextGeneration returns the metadata of the generation after m, written by layer.
func (m *Metadata) NextGeneration(layer LayerID) *Metadata {
	return &Metadata{
		Version:    m.Version,
		Generation: m.Generation + 1,
		BlockSize:  m.BlockSize,
		Size:       m.Size,
		Layer:      layer,
		BaseLayer:  m.BaseLayer,
	}
}

// LayerData is what a header knows of a layer it references: the size of its object
// uncompressed, a SHA-256 of that, and its frame table (nil when stored uncompressed). A header
// carries an entry for every stored layer its mapping references; serialization trims each frame
// table to the frames the mapping needs.
type LayerData struct {
	Size     int64
	Checksum [32]byte
	Frames   *FrameTable
}

type Header struct {
	Metadata *Metadata
	// Layers maps the layer IDs the mapping references to what reading them needs.
	Layers map[LayerID]LayerData
	// Mapping is the per-block source map, stored compactly; read via At / All / Slice.
	Mapping Mapping
	// Local is set on a provisional header: one whose newest layers are not in the store yet
	// and are read from the unpacked files a pause wrote, at identity offsets (the layer's
	// blocks at their own offsets in the file). It maps each such layer to its file. Such a
	// header lets a resume on this node serve at once while the upload runs; it is never
	// uploaded (Serialize refuses it), and the upload's durable header replaces it. In memory
	// only.
	Local map[LayerID]string
}

// NewHeader makes a header from metadata and a mapping. An empty mapping means the whole file
// is metadata.Layer, at identity offsets.
func NewHeader(metadata *Metadata, mapping []LayerMap) (*Header, error) {
	if metadata.BlockSize == 0 {
		return nil, errors.New("block size cannot be zero")
	}
	if metadata.Size%PageSize != 0 {
		return nil, fmt.Errorf("size %d is not a multiple of the page size %d", metadata.Size, PageSize)
	}
	if len(mapping) == 0 && metadata.Size > 0 {
		mapping = []LayerMap{{Offset: 0, Length: metadata.Size, Layer: metadata.Layer}}
	}
	compact, err := NewMapping(PageSize, mapping)
	if err != nil {
		return nil, fmt.Errorf("compact mapping: %w", err)
	}
	if err := compact.Validate(metadata.Size); err != nil {
		return nil, err
	}
	return &Header{Metadata: metadata, Mapping: compact}, nil
}

// NewZero returns generation 0 of a file of size bytes that reads as zeros: the parent of a memory
// file's first layer.
func NewZero(size, blockSize uint64) (*Header, error) {
	return start(size, blockSize, Empty)
}

// NewBase returns generation 0 of a file of size bytes that is entirely its base file: the parent
// of a disk's first layer, whose base is the template image.
func NewBase(size, blockSize uint64) (*Header, error) {
	return start(size, blockSize, Base)
}

func start(size, blockSize uint64, layer LayerID) (*Header, error) {
	md := &Metadata{Version: MetadataVersion, BlockSize: blockSize, Size: size, Layer: layer, BaseLayer: layer}
	var m []LayerMap
	if size > 0 {
		m = []LayerMap{{Offset: 0, Length: size, Layer: layer}}
	}
	return NewHeader(md, m)
}

func newDiffHeader(metadata *Metadata, mapping []LayerMap, sourceLayers map[LayerID]LayerData) (*Header, error) {
	h, err := NewHeader(metadata, mapping)
	if err != nil {
		return nil, err
	}
	h.Layers = make(map[LayerID]LayerData, len(sourceLayers))
	for _, id := range h.Mapping.Layers() {
		if ld, ok := sourceLayers[id]; ok {
			h.Layers[id] = ld
		}
	}
	return h, nil
}

// Clone returns a copy whose Metadata and Layers map can be changed without racing readers of
// the original. Mapping is shared: it is immutable.
func (t *Header) Clone() *Header {
	md := *t.Metadata
	return &Header{Metadata: &md, Layers: maps.Clone(t.Layers), Mapping: t.Mapping, Local: maps.Clone(t.Local)}
}

// SetLayer adds or replaces what the header knows of a layer.
func (t *Header) SetLayer(id LayerID, ld LayerData) {
	if t.Layers == nil {
		t.Layers = make(map[LayerID]LayerData)
	}
	t.Layers[id] = ld
}

// StoredLayers returns the layers the header reads from the store, every one of which must
// outlive it: its ancestors' in a stable order (by ID), then its own, if it wrote any blocks.
func (t *Header) StoredLayers() []LayerID {
	var out []LayerID
	own := false
	for _, id := range t.Mapping.Layers() {
		switch {
		case !id.IsStored():
		case id == t.Metadata.Layer:
			own = true
		default:
			out = append(out, id)
		}
	}
	slices.SortFunc(out, func(a, b LayerID) int { return compareIDs(a, b) })
	if own {
		out = append(out, t.Metadata.Layer)
	}
	return out
}

func compareIDs(a, b LayerID) int {
	for i := range a {
		if a[i] != b[i] {
			return int(a[i]) - int(b[i])
		}
	}
	return 0
}

func (t *Header) String() string {
	if t == nil {
		return "[nil Header]"
	}
	return fmt.Sprintf("[Header: version=%d, size=%d, blockSize=%d, generation=%d, layer=%s, mappings=%d]",
		t.Metadata.Version, t.Metadata.Size, t.Metadata.BlockSize, t.Metadata.Generation, t.Metadata.Layer, t.Mapping.Len())
}

// GetShiftedMapping resolves a file offset to a layer-local range: the layer that holds the
// byte, the offset of the byte in that layer, and how many bytes from there the same mapping
// covers. Callers then use LayerFrames for the layer's frame table.
func (t *Header) GetShiftedMapping(offset int64) (LayerMap, error) {
	if offset < 0 || offset >= int64(t.Metadata.Size) {
		return LayerMap{}, fmt.Errorf("offset %d is out of bounds (size: %d)", offset, t.Metadata.Size)
	}
	if offset%PageSize != 0 {
		return LayerMap{}, fmt.Errorf("offset %d is not aligned to page size %d", offset, PageSize)
	}
	i := t.Mapping.SearchOffset(offset)
	if i == 0 {
		return LayerMap{}, fmt.Errorf("no source found for offset %d", offset)
	}
	m := t.Mapping.At(i - 1)
	shift := offset - int64(m.Offset)
	if shift >= int64(m.Length) {
		return LayerMap{}, fmt.Errorf("offset %d is beyond the end of mapping at offset %d (ends at %d)", offset, m.Offset, m.Offset+m.Length)
	}
	return LayerMap{
		Offset:      uint64(offset),
		Length:      m.Length - uint64(shift),
		Layer:       m.Layer,
		LayerOffset: m.LayerOffset + uint64(shift),
	}, nil
}

// IsProvisional reports whether the header reads any layer from a local file (see Local).
func (t *Header) IsProvisional() bool { return len(t.Local) > 0 }

// LayerFrames returns the frame table for a layer and whether the header knows the layer at
// all. A nil table with ok means the layer is stored uncompressed.
func (t *Header) LayerFrames(id LayerID) (*FrameTable, bool) {
	ld, ok := t.Layers[id]
	return ld.Frames, ok
}
