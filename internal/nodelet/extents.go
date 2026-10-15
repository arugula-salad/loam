package nodelet

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/arugula-salad/loam/internal/snapstore"
)

// What a snapshot's disk layer holds is what the VM's disk changed since its
// parent: the template, for a VM's first snapshot, or the disk of the snapshot
// it last paused into, was restored from or stopped as. The nodelet keeps that
// parent's disk next to the VM's as a reflink clone (parentDisk), so the two
// files share every block the guest hasn't written since, and the changed
// ranges are those where the files' extent maps differ: a block the guest
// wrote was copied on write to a new physical block in the VM's disk only.
// That is FIEMAP on both files, compared by physical address. It reads no
// data, and unlike the extents' SHARED flag it isn't fooled by a third clone
// (a checkpoint's copy shares the VM's new blocks too).
//
// Where the volume has no reflinks (--dev-allow-copy: every clone is a copy
// with blocks of its own) or no FIEMAP (tmpfs), the files are compared by
// content instead: right, only slower.

// changedExtents returns the ranges where path may differ from base (both of
// the same size), comparing extent maps if shared says the two can share
// blocks. It errs towards more: a range it can't tell about is changed.
func changedExtents(path, base string, shared bool) ([]snapstore.Extent, error) {
	if !shared {
		return compareContents(path, base)
	}
	a, errA := fiemap(path)
	b, errB := fiemap(base)
	if errA == nil && errB == nil {
		return compareMaps(a, b), nil
	}
	if errA != nil && !errors.Is(errA, errNoFiemap) {
		return nil, errA
	}
	if errB != nil && !errors.Is(errB, errNoFiemap) {
		return nil, errB
	}
	return compareContents(path, base)
}

type extent struct {
	logical, physical, length int64
	// opaque: the physical address says nothing comparable (delayed
	// allocation, inline or encoded data).
	opaque, unwritten bool
}

var errNoFiemap = errors.New("FIEMAP is not supported here")

const (
	fsIocFiemap      = 0xc020660b
	fiemapFlagSync   = 0x1
	fiemapExtentLast = 0x1
	// UNKNOWN, DELALLOC, ENCODED, DATA_ENCRYPTED, NOT_ALIGNED, DATA_INLINE, DATA_TAIL
	fiemapOpaque         = 0x2 | 0x4 | 0x8 | 0x80 | 0x100 | 0x200 | 0x400
	fiemapExtentUnwrit   = 0x800
	fiemapExtentsPerCall = 512
)

type fiemapHeader struct {
	Start, Length                uint64
	Flags, Mapped, Count, Unused uint32
}

type fiemapExtent struct {
	Logical, Physical, Length uint64
	_                         [2]uint64
	Flags                     uint32
	_                         [3]uint32
}

// fiemap returns path's extent map, its dirty pages flushed first so every
// extent has its physical address.
func fiemap(path string) ([]extent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []extent
	buf := make([]byte, int(unsafe.Sizeof(fiemapHeader{}))+fiemapExtentsPerCall*int(unsafe.Sizeof(fiemapExtent{})))
	hdr := (*fiemapHeader)(unsafe.Pointer(&buf[0]))
	exts := unsafe.Slice((*fiemapExtent)(unsafe.Pointer(&buf[unsafe.Sizeof(fiemapHeader{})])), fiemapExtentsPerCall)
	for start := uint64(0); ; {
		clear(buf)
		*hdr = fiemapHeader{Start: start, Length: ^uint64(0) - start, Flags: fiemapFlagSync, Count: fiemapExtentsPerCall}
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), fsIocFiemap, uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
			if errno == unix.EOPNOTSUPP || errno == unix.ENOTTY || errno == unix.EINVAL {
				return nil, fmt.Errorf("%w (%s: %v)", errNoFiemap, path, errno)
			}
			return nil, fmt.Errorf("FIEMAP %s: %w", path, errno)
		}
		if hdr.Mapped == 0 {
			return out, nil
		}
		last := false
		for _, e := range exts[:hdr.Mapped] {
			out = append(out, extent{logical: int64(e.Logical), physical: int64(e.Physical), length: int64(e.Length),
				opaque: e.Flags&fiemapOpaque != 0, unwritten: e.Flags&fiemapExtentUnwrit != 0})
			last = last || e.Flags&fiemapExtentLast != 0
			start = e.Logical + e.Length
		}
		if last {
			return out, nil
		}
	}
}

// compareMaps returns the ranges where two extent maps differ: data in one
// and a hole in the other, or data at different physical addresses.
func compareMaps(a, b []extent) []snapstore.Extent {
	var cuts []int64
	for _, m := range [][]extent{a, b} {
		for _, e := range m {
			cuts = append(cuts, e.logical, e.logical+e.length)
		}
	}
	sort.Slice(cuts, func(i, j int) bool { return cuts[i] < cuts[j] })
	at := func(m []extent, off int64) *extent {
		i := sort.Search(len(m), func(i int) bool { return m[i].logical+m[i].length > off })
		if i < len(m) && m[i].logical <= off {
			return &m[i]
		}
		return nil
	}
	var out []snapstore.Extent
	for i := 0; i+1 < len(cuts); i++ {
		lo, hi := cuts[i], cuts[i+1]
		if lo == hi {
			continue
		}
		ea, eb := at(a, lo), at(b, lo)
		same := ea == nil && eb == nil
		if ea != nil && eb != nil && !ea.opaque && !eb.opaque && ea.unwritten == eb.unwritten {
			same = ea.physical+(lo-ea.logical) == eb.physical+(lo-eb.logical)
		}
		if same {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Off+out[n-1].Len == lo {
			out[n-1].Len += hi - lo
		} else {
			out = append(out, snapstore.Extent{Off: lo, Len: hi - lo})
		}
	}
	return out
}

// compareContents returns the pages where path and base differ, reading only
// where either has data.
func compareContents(path, base string) ([]snapstore.Extent, error) {
	da, err := snapstore.DataExtents(path)
	if err != nil {
		return nil, err
	}
	db, err := snapstore.DataExtents(base)
	if err != nil {
		return nil, err
	}
	fa, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fa.Close()
	fb, err := os.Open(base)
	if err != nil {
		return nil, err
	}
	defer fb.Close()
	const chunk = 1 << 20
	ba, bb := make([]byte, chunk), make([]byte, chunk)
	var out []snapstore.Extent
	for _, r := range unionExtents(da, db) {
		for off := r.Off; off < r.Off+r.Len; off += chunk {
			n := min(chunk, r.Off+r.Len-off)
			if err := readFull(fa, ba[:n], off); err != nil {
				return nil, err
			}
			if err := readFull(fb, bb[:n], off); err != nil {
				return nil, err
			}
			for p := int64(0); p < n; p += pageSize {
				e := min(p+pageSize, n)
				if bytes.Equal(ba[p:e], bb[p:e]) {
					continue
				}
				if k := len(out); k > 0 && out[k-1].Off+out[k-1].Len == off+p {
					out[k-1].Len += e - p
				} else {
					out = append(out, snapstore.Extent{Off: off + p, Len: e - p})
				}
			}
		}
	}
	return out, nil
}

const pageSize = 4096

// readFull reads len(p) bytes at off; what lies past the end reads as zeros.
func readFull(f *os.File, p []byte, off int64) error {
	n, err := f.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	clear(p[n:])
	return nil
}

// unionExtents merges two sorted extent lists into sorted, disjoint ranges,
// each rounded out to whole pages.
func unionExtents(lists ...[]snapstore.Extent) []snapstore.Extent {
	var all []snapstore.Extent
	for _, l := range lists {
		for _, e := range l {
			lo := e.Off &^ (pageSize - 1)
			hi := (e.Off + e.Len + pageSize - 1) &^ (pageSize - 1)
			all = append(all, snapstore.Extent{Off: lo, Len: hi - lo})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Off < all[j].Off })
	var out []snapstore.Extent
	for _, e := range all {
		if k := len(out); k > 0 && e.Off <= out[k-1].Off+out[k-1].Len {
			out[k-1].Len = max(out[k-1].Len, e.Off+e.Len-out[k-1].Off)
			continue
		}
		out = append(out, e)
	}
	return out
}

// subtractExtents returns the parts of a not in b (both sorted and disjoint).
func subtractExtents(a, b []snapstore.Extent) []snapstore.Extent {
	var out []snapstore.Extent
	j := 0
	for _, e := range a {
		lo, hi := e.Off, e.Off+e.Len
		for j < len(b) && b[j].Off+b[j].Len <= lo {
			j++
		}
		for k := j; k < len(b) && b[k].Off < hi; k++ {
			if b[k].Off > lo {
				out = append(out, snapstore.Extent{Off: lo, Len: b[k].Off - lo})
			}
			lo = max(lo, b[k].Off+b[k].Len)
		}
		if lo < hi {
			out = append(out, snapstore.Extent{Off: lo, Len: hi - lo})
		}
	}
	return out
}

// overlay writes the ranges es of src over dst, at the same offsets: a diff
// snapshot's pages over a copy of its parent's whole memory make this one's.
func overlay(dst, src string, es []snapstore.Extent) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	for _, e := range es {
		for off := e.Off; off < e.Off+e.Len; {
			n := min(int64(len(buf)), e.Off+e.Len-off)
			if err := readFull(in, buf[:n], off); err != nil {
				out.Close()
				return err
			}
			if _, err := out.WriteAt(buf[:n], off); err != nil {
				out.Close()
				return err
			}
			off += n
		}
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// fileSize is path's apparent size.
func fileSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}
