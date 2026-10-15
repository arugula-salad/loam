package snapstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"

	"github.com/arugula-salad/loam/internal/snapstore/header"
	"github.com/arugula-salad/loam/internal/snapstore/storage"
)

// Extent is a byte range [Off, Off+Len) of a file.
type Extent struct {
	Off, Len int64
}

// DataExtents returns the extents of path that hold data (SEEK_DATA/SEEK_HOLE): for a memory
// file Firecracker wrote, the pages it wrote. A diff snapshot's file holds exactly the pages
// dirtied since the last snapshot or restore; a full snapshot made sparse holds every non-zero
// page.
func DataExtents(path string) ([]Extent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var out []Extent
	fd := int(f.Fd())
	for off := int64(0); off < fi.Size(); {
		data, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("seek data in %s: %w", path, err)
		}
		hole, err := unix.Seek(fd, data, unix.SEEK_HOLE)
		if err != nil {
			return nil, fmt.Errorf("seek hole in %s: %w", path, err)
		}
		out = append(out, Extent{data, hole - data})
		off = hole
	}
	return out, nil
}

// BitmapExtents turns a set of indices of units of unit bytes into extents: the UFFD handler's
// Zeroed pages (unit: its PageSize) into LayerSpec.Zero.
func BitmapExtents(b *header.Bitmap, unit int64) []Extent {
	var out []Extent
	for s, e := range b.Ranges() {
		out = append(out, Extent{int64(s) * unit, (int64(e) - int64(s)) * unit})
	}
	return out
}

// blocks turns extents into the set of PageSize blocks they touch, rounding outward.
func blocks(es []Extent, size int64) (*header.Bitmap, error) {
	b := header.NewBitmap()
	for _, e := range es {
		if e.Off < 0 || e.Len < 0 || e.Off+e.Len > size {
			return nil, fmt.Errorf("extent %d+%d outside a file of %d bytes", e.Off, e.Len, size)
		}
		if e.Len > 0 {
			b.AddRange(uint64(e.Off/header.PageSize), uint64((e.Off+e.Len+header.PageSize-1)/header.PageSize))
		}
	}
	return b, nil
}

// LayerSpec is a new layer of one file: what a pause, stop or checkpoint changed in it.
type LayerSpec struct {
	// Layer names the generation. A snapshot's memory and disk layers share it.
	Layer header.LayerID
	Kind  Kind
	// Path is the file the changed blocks are read from, each at its own offset: Firecracker's
	// memory file (a diff snapshot's, or a full one's), or the VM's disk. It must stay unchanged
	// until Upload returns, and while a provisional header reads it.
	Path string
	// Size is the file's size, which must be its parent's. Path may be shorter (sparse files
	// are); what it lacks reads as zeros.
	Size int64
	// Dirty are the ranges to take from Path; every other block is unchanged from the parent.
	// A block in them that reads all zeros is recorded Empty and not stored.
	Dirty []Extent
	// Zero are ranges known to read as zeros now without reading them (the UFFD handler's
	// Zeroed pages: the balloon gave them back). Dirty wins where they overlap.
	Zero []Extent
}

// PendingLayer is a layer from the moment its blocks are known to the moment it is stored.
type PendingLayer struct {
	s     *Store
	spec  LayerSpec
	dirty *header.Bitmap
	zero  *header.Bitmap
}

// NewLayer prepares a layer. It reads nothing yet.
func (s *Store) NewLayer(spec LayerSpec) (*PendingLayer, error) {
	if !spec.Kind.valid() {
		return nil, fmt.Errorf("unknown file kind %q", spec.Kind)
	}
	if !spec.Layer.IsStored() {
		return nil, fmt.Errorf("layer id %s is reserved", spec.Layer)
	}
	if spec.Size <= 0 || spec.Size%header.PageSize != 0 {
		return nil, fmt.Errorf("size %d is not a positive multiple of %d", spec.Size, header.PageSize)
	}
	dirty, err := blocks(spec.Dirty, spec.Size)
	if err != nil {
		return nil, err
	}
	zero, err := blocks(spec.Zero, spec.Size)
	if err != nil {
		return nil, err
	}
	zero.AndNot(dirty)
	return &PendingLayer{s: s, spec: spec, dirty: dirty, zero: zero}, nil
}

func (p *PendingLayer) checkParent(parent *header.Header) error {
	if int64(parent.Metadata.Size) != p.spec.Size {
		return fmt.Errorf("parent is %d bytes, the layer's file %d", parent.Metadata.Size, p.spec.Size)
	}
	return nil
}

// Provisional returns the next generation's header over parent (durable or itself provisional),
// reading this layer's blocks straight from Path: at once, with no I/O. Open it with OpenFile to
// serve the file before the upload is done; it can't be stored.
func (p *PendingLayer) Provisional(parent *header.Header) (*header.Header, error) {
	if err := p.checkParent(parent); err != nil {
		return nil, err
	}
	d := header.NewDiffMetadata(header.PageSize, p.dirty, p.zero)
	return d.ToProvisionalDiffHeader(parent, p.spec.Layer, p.spec.Path)
}

// Upload stores the layer and returns the next generation's durable header over parent, which
// must be durable (stored, or about to be: not provisional). It reads the dirty blocks from Path,
// records the all-zero ones Empty, packs the rest in order into one object of independently
// compressed frames (layers/<layer>/<kind>[.zst]) and composes the header, which knows the new
// layer's size, checksum and frame table. A layer with no non-zero blocks stores no object.
func (p *PendingLayer) Upload(ctx context.Context, parent *header.Header) (*header.Header, error) {
	if err := p.checkParent(parent); err != nil {
		return nil, err
	}
	if parent.IsProvisional() {
		return nil, errors.New("upload over a provisional parent: upload the parent first")
	}
	src, err := os.Open(p.spec.Path)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	builder := header.NewDiffMetadataBuilder(header.PageSize)
	for s, e := range p.zero.Ranges() {
		for i := uint64(s); i < e; i++ {
			builder.MarkEmpty(int64(i) * header.PageSize)
		}
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(packBlocks(src, p.dirty, builder, pw)) }()
	key := layerKeyFor(p.spec.Layer, p.spec.Kind, p.s.compress.Type)
	res, err := storage.UploadFramed(ctx, p.s.b, key, pr, p.s.compress)
	pr.CloseWithError(errors.New("upload ended"))
	if err != nil {
		return nil, fmt.Errorf("upload layer %s %s: %w", p.spec.Layer, p.spec.Kind, err)
	}
	h, err := builder.Build().ToDiffHeader(parent, p.spec.Layer)
	if err != nil {
		return nil, err
	}
	if res.Size == 0 {
		// Nothing but zeros: the header maps no block to the layer, so no object is needed.
		if err := p.s.b.Delete(ctx, key); err != nil {
			return nil, err
		}
		return h, nil
	}
	h.SetLayer(p.spec.Layer, header.LayerData{Size: res.Size, Checksum: res.Checksum, Frames: res.Frames})
	return h, nil
}

// packBlocks writes the non-zero blocks of src that dirty names to out, in order, classifying
// each through the builder.
func packBlocks(src *os.File, dirty *header.Bitmap, builder *header.DiffMetadataBuilder, out io.Writer) error {
	buf := make([]byte, 1<<20)
	for s, e := range dirty.Ranges() {
		for off, end := int64(s)*header.PageSize, int64(e)*header.PageSize; off < end; {
			chunk := buf[:min(int64(len(buf)), end-off)]
			n, err := src.ReadAt(chunk, off)
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			clear(chunk[n:])
			for i := 0; i < len(chunk); i += header.PageSize {
				if err := builder.Process(chunk[i:i+header.PageSize], out, off+int64(i)); err != nil {
					return err
				}
			}
			off += int64(len(chunk))
		}
	}
	return nil
}
