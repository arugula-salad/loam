package header

import (
	"bytes"
	"math/rand"
	"slices"
	"testing"
)

func TestBitmap(t *testing.T) {
	var b Bitmap
	b.AddRange(3, 70)
	b.Add(200)
	b.RemoveRange(10, 12)
	b.Remove(69)
	if got, want := b.Cardinality(), uint64(67-2-1+1); got != want {
		t.Fatalf("cardinality %d, want %d", got, want)
	}
	var runs [][2]uint64
	for s, e := range b.Ranges() {
		runs = append(runs, [2]uint64{uint64(s), e})
	}
	want := [][2]uint64{{3, 10}, {12, 69}, {200, 201}}
	if !slices.Equal(runs, want) {
		t.Fatalf("ranges %v, want %v", runs, want)
	}
	if b.CardinalityInRange(0, 13) != 8 || b.CardinalityInRange(60, 300) != 10 {
		t.Fatalf("cardinality in range: %d %d", b.CardinalityInRange(0, 13), b.CardinalityInRange(60, 300))
	}
	if !b.Contains(200) || b.Contains(69) || b.Contains(1000) {
		t.Fatal("contains")
	}
	c := b.Clone()
	c.AndNot(&b)
	if !c.IsEmpty() {
		t.Fatal("x andnot x is not empty")
	}
	u := Or(&b, &Bitmap{})
	if u.Cardinality() != b.Cardinality() {
		t.Fatal("or")
	}
	// A range ending exactly on a word boundary, and one spanning several words.
	var d Bitmap
	d.AddRange(0, 64)
	d.AddRange(128, 320)
	runs = nil
	for s, e := range d.Ranges() {
		runs = append(runs, [2]uint64{uint64(s), e})
	}
	if !slices.Equal(runs, [][2]uint64{{0, 64}, {128, 320}}) {
		t.Fatalf("word-boundary ranges %v", runs)
	}
}

func TestLayerID(t *testing.T) {
	id := NewLayerID()
	got, err := ParseLayerID(id.String())
	if err != nil || got != id {
		t.Fatalf("round trip %s: %s, %v", id, got, err)
	}
	if !id.IsStored() || Empty.IsStored() || Base.IsStored() {
		t.Fatal("IsStored")
	}
	if _, err := ParseLayerID("not-a-uuid"); err == nil {
		t.Fatal("parsed garbage")
	}
}

func TestMergeMappings(t *testing.T) {
	a, b := NewLayerID(), NewLayerID()
	base := []LayerMap{{Offset: 0, Length: 10 * PageSize, Layer: a}}
	diff := []LayerMap{
		{Offset: 2 * PageSize, Length: 2 * PageSize, Layer: b, LayerOffset: 0},
		{Offset: 8 * PageSize, Length: 4 * PageSize, Layer: b, LayerOffset: 2 * PageSize},
	}
	got := MergeMappings(base, diff)
	want := []LayerMap{
		{Offset: 0, Length: 2 * PageSize, Layer: a},
		{Offset: 2 * PageSize, Length: 2 * PageSize, Layer: b},
		{Offset: 4 * PageSize, Length: 4 * PageSize, Layer: a, LayerOffset: 4 * PageSize},
		{Offset: 8 * PageSize, Length: 4 * PageSize, Layer: b, LayerOffset: 2 * PageSize},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("merge:\n got %v\nwant %v", got, want)
	}
	// Normalize joins only storage-contiguous runs of one layer; Empty runs always join.
	n := NormalizeMappings([]LayerMap{
		{Offset: 0, Length: PageSize, Layer: a, LayerOffset: 0},
		{Offset: PageSize, Length: PageSize, Layer: a, LayerOffset: PageSize},
		{Offset: 2 * PageSize, Length: PageSize, Layer: a, LayerOffset: 5 * PageSize},
		{Offset: 3 * PageSize, Length: PageSize, Layer: Empty, LayerOffset: 0},
		{Offset: 4 * PageSize, Length: PageSize, Layer: Empty, LayerOffset: 77 * PageSize},
	})
	if len(n) != 3 || n[0].Length != 2*PageSize || n[2].Length != 2*PageSize {
		t.Fatalf("normalize: %v", n)
	}
}

// model is a file as a test sees it: its bytes per generation, and each layer's packed object.
type model struct {
	t       *testing.T
	objects map[LayerID][]byte
	base    []byte
}

// resolve reads the whole file through h, the way the store's read path does.
func (m *model) resolve(h *Header) []byte {
	out := make([]byte, h.Metadata.Size)
	for off := int64(0); off < int64(h.Metadata.Size); {
		lm, err := h.GetShiftedMapping(off)
		if err != nil {
			m.t.Fatalf("mapping at %d: %v", off, err)
		}
		n := int64(lm.Length)
		switch lm.Layer {
		case Empty:
		case Base:
			copy(out[off:off+n], m.base[lm.LayerOffset:])
		default:
			obj, ok := m.objects[lm.Layer]
			if !ok {
				m.t.Fatalf("layer %s has no object", lm.Layer)
			}
			if _, ok := h.Layers[lm.Layer]; !ok {
				m.t.Fatalf("header has no entry for layer %s", lm.Layer)
			}
			copy(out[off:off+n], obj[lm.LayerOffset:lm.LayerOffset+lm.Length])
		}
		off += n
	}
	return out
}

// generation applies random writes (some of them zeros) to cur and returns the next header and
// the new content, packing the layer the way the store does (DiffMetadataBuilder).
func (m *model) generation(r *rand.Rand, parent *Header, cur []byte, writes int) (*Header, []byte) {
	next := bytes.Clone(cur)
	touched := NewBitmap()
	blocks := len(cur) / PageSize
	for range writes {
		start := r.Intn(blocks)
		for j := 0; j < 1+r.Intn(6) && start+j < blocks; j++ {
			b := next[(start+j)*PageSize : (start+j+1)*PageSize]
			if r.Intn(5) == 0 {
				clear(b)
			} else {
				r.Read(b)
			}
			touched.Add(uint32(start + j))
		}
	}
	builder := NewDiffMetadataBuilder(PageSize)
	var obj bytes.Buffer
	for s, e := range touched.Ranges() {
		for i := int64(s); i < int64(e); i++ {
			if err := builder.Process(next[i*PageSize:(i+1)*PageSize], &obj, i*PageSize); err != nil {
				m.t.Fatal(err)
			}
		}
	}
	layer := NewLayerID()
	h, err := builder.Build().ToDiffHeader(parent, layer)
	if err != nil {
		m.t.Fatal(err)
	}
	m.objects[layer] = obj.Bytes()
	h.SetLayer(layer, LayerData{Size: int64(obj.Len())})
	return h, next
}

func TestMultiGenerationDiffs(t *testing.T) {
	const size = 8 << 20
	r := rand.New(rand.NewSource(1))
	for _, start := range []string{"zero", "base"} {
		t.Run(start, func(t *testing.T) {
			m := &model{t: t, objects: map[LayerID][]byte{}}
			var h *Header
			var err error
			cur := make([]byte, size)
			if start == "zero" {
				h, err = NewZero(size, PageSize)
			} else {
				m.base = make([]byte, size)
				r.Read(m.base)
				cur = bytes.Clone(m.base)
				h, err = NewBase(size, PageSize)
			}
			if err != nil {
				t.Fatal(err)
			}
			for gen := 1; gen <= 12; gen++ {
				h, cur = m.generation(r, h, cur, 40)
				if h.Metadata.Generation != uint64(gen) {
					t.Fatalf("generation %d, want %d", h.Metadata.Generation, gen)
				}
				if !bytes.Equal(m.resolve(h), cur) {
					t.Fatalf("generation %d reads differently from the model", gen)
				}
				// What the store keeps is what a reader on another node gets back.
				data, err := Serialize(h)
				if err != nil {
					t.Fatal(err)
				}
				h2, err := Deserialize(data)
				if err != nil {
					t.Fatalf("generation %d: %v", gen, err)
				}
				if !bytes.Equal(m.resolve(h2), cur) {
					t.Fatalf("generation %d reads differently after a round trip", gen)
				}
				if !slices.Equal(withoutEmptyOffsets(h2.Mapping.Slice()), withoutEmptyOffsets(h.Mapping.Slice())) {
					t.Fatalf("generation %d mapping changed in a round trip", gen)
				}
				h = h2
			}
			// Every stored layer the last header references is one the test made, its own last.
			layers := h.StoredLayers()
			if len(layers) == 0 || layers[len(layers)-1] != h.Metadata.Layer {
				t.Fatalf("stored layers %v, own %s", layers, h.Metadata.Layer)
			}
			for _, id := range layers {
				if _, ok := m.objects[id]; !ok {
					t.Fatalf("unknown layer %s", id)
				}
			}
		})
	}
}

// withoutEmptyOffsets clears the storage offsets of Empty ranges, which mean nothing and are not
// stored.
func withoutEmptyOffsets(ms []LayerMap) []LayerMap {
	for i := range ms {
		if ms[i].Layer == Empty {
			ms[i].LayerOffset = 0
		}
	}
	return ms
}

func TestProvisionalHeader(t *testing.T) {
	const size = 1 << 20
	parent, _ := NewZero(size, PageSize)
	dirty := NewBitmap()
	dirty.AddRange(3, 9)
	layer := NewLayerID()
	d := NewDiffMetadata(PageSize, dirty, nil)
	ph, err := d.ToProvisionalDiffHeader(parent, layer, "/local/mem")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ph.GetShiftedMapping(5 * PageSize)
	if err != nil || m.Layer != layer || m.LayerOffset != 5*PageSize {
		t.Fatalf("provisional mapping %+v, %v: want identity offsets", m, err)
	}
	if ph.Local[layer] != "/local/mem" || !ph.IsProvisional() {
		t.Fatalf("provisional header's local layers: %v", ph.Local)
	}
	if _, err := Serialize(ph); err == nil {
		t.Fatal("serialized a provisional header")
	}
	dh, err := d.ToDiffHeader(parent, layer)
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := dh.GetShiftedMapping(5 * PageSize); m.LayerOffset != 2*PageSize {
		t.Fatalf("durable mapping %+v: want packed offsets", m)
	}
	// The durable header can't be stored until it knows its own layer.
	if _, err := Serialize(dh); err == nil {
		t.Fatal("serialized a header with no entry for its own layer")
	}
}

func TestFrameTableTrimAndRoundTrip(t *testing.T) {
	ft := NewFrameTable(CompressionZstd, []FrameSize{{U: 100, C: 10}, {U: 100, C: 20}, {U: 100, C: 30}, {U: 50, C: 5}})
	if r, err := ft.LocateUncompressed(150); err != nil || r.Offset != 100 || r.Length != 100 {
		t.Fatalf("locate U: %v %v", r, err)
	}
	if r, err := ft.LocateCompressed(200); err != nil || r.Offset != 30 || r.Length != 30 {
		t.Fatalf("locate C: %v %v", r, err)
	}
	if _, err := ft.LocateCompressed(150); err == nil {
		t.Fatal("located a mid-frame offset")
	}
	tr := ft.TrimToRanges([]Range{{Offset: 120, Length: 10}, {Offset: 310, Length: 5}})
	if tr.NumFrames() != 2 {
		t.Fatalf("trimmed to %d frames", tr.NumFrames())
	}
	var buf bytes.Buffer
	if err := tr.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	back, err := DeserializeFrameTable(&buf)
	if err != nil || back.NumFrames() != 2 || back.CompressionType() != CompressionZstd {
		t.Fatalf("round trip: %v %v", back, err)
	}
	if _, err := back.LocateUncompressed(250); err == nil {
		t.Fatal("located an offset in a trimmed gap")
	}
}

func TestDeserializeRejectsGarbage(t *testing.T) {
	h, _ := NewZero(1<<20, PageSize)
	data, err := Serialize(h)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{data[:10], append(bytes.Clone(data[:len(data)-3]), 1, 2, 3)} {
		if _, err := Deserialize(bad); err == nil {
			t.Fatal("deserialized garbage")
		}
	}
	flagged := bytes.Clone(data)
	flagged[metadataSize] = flagIncomplete
	if _, err := Deserialize(flagged); err == nil {
		t.Fatal("deserialized an incomplete header")
	}
}
