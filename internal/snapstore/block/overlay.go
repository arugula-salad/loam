// Derived from github.com/e2b-dev/infra, packages/orchestrator/pkg/sandbox/block/overlay.go at
// commit cf536c2 (Copyright 2023 FoundryLabs, Inc., Apache-2.0, see ../LICENSE-e2b). Modified
// for loam: the swap/seal/fold machinery for background exports dropped; one writable cache.

package block

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/arugula-salad/loam/internal/snapstore/header"
)

// Overlay is a copy-on-write view of a read-only device: writes land in the cache, reads take a
// block from the cache if it holds it and from the device otherwise. ExportDiff turns the
// writes into the next layer.
type Overlay struct {
	device    ReadonlyDevice
	cache     *Cache
	blockSize int64
}

// NewOverlay layers cache, which must be the device's size and block size, over device.
func NewOverlay(device ReadonlyDevice, cache *Cache) (*Overlay, error) {
	if cache.Size() != device.Size() || cache.BlockSize() != device.BlockSize() {
		return nil, fmt.Errorf("overlay cache is %d bytes in blocks of %d, device %d in %d",
			cache.Size(), cache.BlockSize(), device.Size(), device.BlockSize())
	}
	return &Overlay{device: device, cache: cache, blockSize: device.BlockSize()}, nil
}

// ReadAt reads block-aligned p at off.
func (o *Overlay) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if int64(len(p))%o.blockSize != 0 || off%o.blockSize != 0 {
		return 0, fmt.Errorf("misaligned read: len=%d off=%d block=%d", len(p), off, o.blockSize)
	}
	for blockOff := int64(0); blockOff < int64(len(p)); blockOff += o.blockSize {
		buf := p[blockOff : blockOff+o.blockSize]
		_, err := o.cache.ReadAt(buf, off+blockOff)
		if err == nil {
			continue
		}
		if !errors.As(err, &BytesNotAvailableError{}) {
			return int(blockOff), fmt.Errorf("error reading from cache: %w", err)
		}
		if _, err := o.device.ReadAt(ctx, buf, off+blockOff); err != nil {
			return int(blockOff), fmt.Errorf("error reading from device: %w", err)
		}
	}
	return len(p), nil
}

func (o *Overlay) WriteAt(p []byte, off int64) (int, error) { return o.cache.WriteAt(p, off) }

func (o *Overlay) WriteZeroesAt(off, length int64) (int, error) {
	return o.cache.WriteZeroesAt(off, length)
}

func (o *Overlay) Size() int64      { return o.device.Size() }
func (o *Overlay) BlockSize() int64 { return o.blockSize }

// ExportDiff writes the overlay's dirty blocks to out, packed, without detaching the cache: the
// overlay stays usable. Compose the result over the device's header with ToDiffHeader.
func (o *Overlay) ExportDiff(out *os.File) (*header.DiffMetadata, error) {
	return o.cache.ExportToDiff(out)
}

// Close closes (and removes) the cache.
func (o *Overlay) Close() error { return o.cache.Close() }
