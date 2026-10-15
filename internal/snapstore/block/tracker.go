// Derived from github.com/e2b-dev/infra, packages/orchestrator/pkg/sandbox/block/tracker.go at
// commit cf536c2 (Copyright 2023 FoundryLabs, Inc., Apache-2.0, see ../LICENSE-e2b). Modified
// for loam: roaring replaced by header.Bitmap; the write-protect promotions (MarkWritten,
// MarkExported) dropped, since loam's dirty set comes from Firecracker, not from write-protect
// faults.

package block

import (
	"sync"

	"github.com/arugula-salad/loam/internal/snapstore/header"
)

type State uint8

const (
	// NotPresent: fall back to the previous layer.
	NotPresent State = iota
	// Dirty: this layer holds materialized data.
	Dirty
	// Zero: known-zero and installed (an in-place zero page); no need to consult the previous
	// layer.
	Zero
	// Removed: known-zero but not installed (the mapping was zapped by a UFFD REMOVE, the
	// balloon's madvise). Distinct from Zero so a later fault knows to install zeros rather than
	// the source's content.
	Removed
	// Clean: installed with content identical to the source. Present like Dirty, but excluded
	// from Export: a snapshot diff can inherit the page from the parent layer.
	Clean
)

func (s State) String() string {
	switch s {
	case NotPresent:
		return "not_present"
	case Dirty:
		return "dirty"
	case Zero:
		return "zero"
	case Removed:
		return "removed"
	case Clean:
		return "clean"
	default:
		return "unknown"
	}
}

// Tracker records a state per block. The state bitmaps are disjoint.
type Tracker struct {
	mu                          sync.RWMutex
	dirty, zero, removed, clean *header.Bitmap
}

func NewTracker() *Tracker {
	return &Tracker{dirty: header.NewBitmap(), zero: header.NewBitmap(), removed: header.NewBitmap(), clean: header.NewBitmap()}
}

// SetRange sets state for indices in [start, end).
func (t *Tracker) SetRange(start, end uint32, state State) {
	if end <= start {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s, e := uint64(start), uint64(end)
	target := t.bitmapFor(state)
	for _, bm := range []*header.Bitmap{t.dirty, t.zero, t.removed, t.clean} {
		if bm == target {
			bm.AddRange(s, e)
		} else {
			bm.RemoveRange(s, e)
		}
	}
}

// MarkInstalled records a read-side install (Zero or Clean) of block idx, but only if it is
// NotPresent or Removed: an installer suspended between its UFFDIO_COPY and this call must not
// downgrade a page a concurrent write fault already made Dirty.
func (t *Tracker) MarkInstalled(idx uint32, state State) {
	if state != Zero && state != Clean {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch t.stateOf(idx) {
	case Removed:
		t.removed.Remove(idx)
		t.bitmapFor(state).Add(idx)
	case NotPresent:
		t.bitmapFor(state).Add(idx)
	}
}

func (t *Tracker) bitmapFor(state State) *header.Bitmap {
	switch state {
	case Dirty:
		return t.dirty
	case Zero:
		return t.zero
	case Removed:
		return t.removed
	case Clean:
		return t.clean
	default:
		return nil
	}
}

func (t *Tracker) Get(idx uint32) State {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.stateOf(idx)
}

func (t *Tracker) stateOf(idx uint32) State {
	switch {
	case t.dirty.Contains(idx):
		return Dirty
	case t.zero.Contains(idx):
		return Zero
	case t.removed.Contains(idx):
		return Removed
	case t.clean.Contains(idx):
		return Clean
	default:
		return NotPresent
	}
}

// Present reports whether every index in [start, end) has been observed (is not NotPresent).
func (t *Tracker) Present(start, end uint32) bool {
	if end <= start {
		return end == start
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, e := uint64(start), uint64(end)
	return t.dirty.CardinalityInRange(s, e)+t.zero.CardinalityInRange(s, e)+
		t.removed.CardinalityInRange(s, e)+t.clean.CardinalityInRange(s, e) == e-s
}

// Export returns copies of the dirty and empty sets. Zero and Removed blocks both read back as
// zeros, so they make up the empty set. Clean blocks are in neither: their content matches the
// source, so a snapshot diff inherits them from the parent layer.
func (t *Tracker) Export() (dirty, empty *header.Bitmap) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.dirty.Clone(), header.Or(t.zero, t.removed)
}

// Zeroed returns the blocks that read as zeros: Zero and Removed.
func (t *Tracker) Zeroed() *header.Bitmap {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return header.Or(t.zero, t.removed)
}
