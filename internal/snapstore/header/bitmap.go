package header

import (
	"iter"
	"math/bits"
)

// Bitmap is a set of block indices. E2B uses roaring bitmaps here; loam's files are bounded
// (a 16 GiB memory file is 4 Mi pages, 512 KiB of plain bits), so a dense bitset does the same
// job without the dependency. The zero value is an empty set, and the methods take the set by
// pointer and grow it as needed.
type Bitmap struct {
	words []uint64
}

// NewBitmap returns an empty set.
func NewBitmap() *Bitmap { return &Bitmap{} }

func (b *Bitmap) grow(end uint64) {
	if need := int((end + 63) / 64); need > len(b.words) {
		b.words = append(b.words, make([]uint64, need-len(b.words))...)
	}
}

// Add adds index i.
func (b *Bitmap) Add(i uint32) { b.AddRange(uint64(i), uint64(i)+1) }

// Remove removes index i.
func (b *Bitmap) Remove(i uint32) { b.RemoveRange(uint64(i), uint64(i)+1) }

// Contains reports whether index i is in the set.
func (b *Bitmap) Contains(i uint32) bool {
	w := int(i / 64)
	return w < len(b.words) && b.words[w]&(1<<(i%64)) != 0
}

// AddRange adds every index in [start, end).
func (b *Bitmap) AddRange(start, end uint64) {
	if end <= start {
		return
	}
	b.grow(end)
	b.setRange(start, end, true)
}

// RemoveRange removes every index in [start, end).
func (b *Bitmap) RemoveRange(start, end uint64) {
	end = min(end, uint64(len(b.words))*64)
	if end <= start {
		return
	}
	b.setRange(start, end, false)
}

func (b *Bitmap) setRange(start, end uint64, set bool) {
	for start < end {
		w, bit := start/64, start%64
		n := min(64-bit, end-start)
		mask := ^uint64(0)
		if n < 64 {
			mask = ((1 << n) - 1) << bit
		}
		if set {
			b.words[w] |= mask
		} else {
			b.words[w] &^= mask
		}
		start += n
	}
}

// Cardinality is the number of indices in the set.
func (b *Bitmap) Cardinality() uint64 {
	var n int
	for _, w := range b.words {
		n += bits.OnesCount64(w)
	}
	return uint64(n)
}

// CardinalityInRange is the number of indices in [start, end) that are in the set.
func (b *Bitmap) CardinalityInRange(start, end uint64) uint64 {
	end = min(end, uint64(len(b.words))*64)
	var n uint64
	for start < end {
		w, bit := start/64, start%64
		c := min(64-bit, end-start)
		word := b.words[w] >> bit
		if c < 64 {
			word &= (1 << c) - 1
		}
		n += uint64(bits.OnesCount64(word))
		start += c
	}
	return n
}

// IsEmpty reports whether the set has no indices.
func (b *Bitmap) IsEmpty() bool {
	for _, w := range b.words {
		if w != 0 {
			return false
		}
	}
	return true
}

// Ranges yields the runs of consecutive indices in ascending order as (start, end), end
// exclusive: the shape of roaring's Ranges, which the mapping code was written against.
func (b *Bitmap) Ranges() iter.Seq2[uint32, uint64] {
	return func(yield func(uint32, uint64) bool) {
		n := uint64(len(b.words)) * 64
		for i := uint64(0); i < n; {
			s, ok := b.next(i, true)
			if !ok {
				return
			}
			e, _ := b.next(s, false)
			if !yield(uint32(s), e) {
				return
			}
			i = e
		}
	}
}

// next returns the first index at or after i whose membership is set, and whether there is
// one. Looking for a clear index always succeeds: past the last word every index is clear.
func (b *Bitmap) next(i uint64, set bool) (uint64, bool) {
	for w := i / 64; w < uint64(len(b.words)); w++ {
		word := b.words[w]
		if !set {
			word = ^word
		}
		if w == i/64 {
			word &= ^uint64(0) << (i % 64)
		}
		if word != 0 {
			return w*64 + uint64(bits.TrailingZeros64(word)), true
		}
	}
	return uint64(len(b.words)) * 64, !set
}

// Clone returns a copy of the set.
func (b *Bitmap) Clone() *Bitmap {
	return &Bitmap{words: append([]uint64(nil), b.words...)}
}

// Or adds every index of o to b.
func (b *Bitmap) Or(o *Bitmap) {
	b.grow(uint64(len(o.words)) * 64)
	for i, w := range o.words {
		b.words[i] |= w
	}
}

// AndNot removes every index of o from b.
func (b *Bitmap) AndNot(o *Bitmap) {
	for i := range min(len(b.words), len(o.words)) {
		b.words[i] &^= o.words[i]
	}
}

// Or returns the union of a and c.
func Or(a, c *Bitmap) *Bitmap {
	out := a.Clone()
	out.Or(c)
	return out
}
