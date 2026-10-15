// Derived from github.com/e2b-dev/infra, packages/orchestrator/pkg/sandbox/uffd/userfaultfd/
// userfaultfd.go, deferred.go and prefault.go, and block/prefetch_tracker.go, at commit cf536c2
// (Copyright 2023 FoundryLabs, Inc., Apache-2.0, see ../LICENSE-e2b). Modified for loam: upstream
// Firecracker registers guest memory for missing faults only, so the write-protect paths (sync
// WP resolves, the CoW export window, WP-armed installs) are dropped and pages install plainly;
// no metrics, a context instead of the exit pipe wrapper, a prefetch list in place of E2B's
// prefetcher.

// Package uffd serves a Firecracker VM's memory lazily from a snapshot: Firecracker hands its
// userfaultfd over a unix socket (Accept), and Serve answers each page fault with the page read
// from the store. It must run in a process other than the one whose memory it serves (in loam,
// a helper process the nodelet starts per restore): a Go thread blocked in a fault the same
// process should serve can't be stopped for the garbage collector, and the process deadlocks.
package uffd

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"

	"github.com/arugula-salad/loam/internal/snapstore/block"
	"github.com/arugula-salad/loam/internal/snapstore/header"
)

const maxRequestsInProgress = 4096

const (
	// sliceMaxRetries is how many times a failed source read is retried before the fault is
	// given up on.
	sliceMaxRetries     = 3
	sliceRetryBaseDelay = 50 * time.Millisecond
	sliceRetryMaxDelay  = 500 * time.Millisecond
)

// PageSource is where page contents come from: the snapshot's memory file through the store.
type PageSource interface {
	ReadAt(ctx context.Context, p []byte, off int64) (int, error)
}

// Handler serves one VM's page faults.
type Handler struct {
	fd       int
	src      PageSource
	regions  mapping
	pageSize uintptr
	tracker  *block.Tracker

	// settle guards the tracker against a REMOVE batch: workers take RLock for the
	// lookup, install, record sequence; the batch takes Lock, so a worker can't record an
	// install over a page the batch just recorded removed.
	settle sync.RWMutex

	wg errgroup.Group

	// wakeupPipe wakes the poll loop after a worker defers a fault, so the deferred fault isn't
	// stranded until the next unrelated event.
	wakeupPipe [2]int
	exitPipe   [2]int

	faultMu    sync.Mutex
	faultSeen  map[int64]bool
	faultOrder []int64

	closeOnce sync.Once
	closed    bool
}

// New makes a handler for a userfaultfd and its regions (from Accept). All regions must share a
// page size: 4 KiB, or 2 MiB with hugepages.
func New(fd int, regions []Region, src PageSource) (*Handler, error) {
	if len(regions) == 0 {
		return nil, errors.New("memory mapping has no regions")
	}
	pageSize := regions[0].PageSize
	if pageSize != header.PageSize && pageSize != header.HugepageSize {
		return nil, fmt.Errorf("unsupported page size: %d", pageSize)
	}
	for _, r := range regions[1:] {
		if r.PageSize != pageSize {
			return nil, fmt.Errorf("region page size mismatch: %d != %d", r.PageSize, pageSize)
		}
	}
	h := &Handler{fd: fd, src: src, regions: regions, pageSize: pageSize, tracker: block.NewTracker(), faultSeen: map[int64]bool{}}
	if err := unix.Pipe2(h.wakeupPipe[:], unix.O_NONBLOCK|unix.O_CLOEXEC); err != nil {
		return nil, err
	}
	if err := unix.Pipe2(h.exitPipe[:], unix.O_NONBLOCK|unix.O_CLOEXEC); err != nil {
		unix.Close(h.wakeupPipe[0])
		unix.Close(h.wakeupPipe[1])
		return nil, err
	}
	h.wg.SetLimit(maxRequestsInProgress)
	return h, nil
}

// PageSize is the guest's page size, the unit of every offset the handler reports.
func (h *Handler) PageSize() int64 { return int64(h.pageSize) }

// Faults returns the memory-file offsets of the pages the guest faulted in, in the order it
// first touched them: a prefetch list for the next restore of the same snapshot (or of its
// template's first boot). Pages prefetched are not in it.
func (h *Handler) Faults() []int64 {
	h.faultMu.Lock()
	defer h.faultMu.Unlock()
	return append([]int64(nil), h.faultOrder...)
}

// Zeroed returns, as page indices, the pages that read as zeros now: the ones the guest gave
// back (UFFD REMOVE: the balloon's madvise), and the ones installed as zeros since. Firecracker's
// dirty log doesn't include a page the guest gave back, so a snapshot taken of this VM would
// otherwise keep its old content; pass these as LayerSpec.Zero. A page in it the guest wrote
// since is in the dirty log, and the dirty log wins.
func (h *Handler) Zeroed() *header.Bitmap { return h.tracker.Zeroed() }

// Serve answers faults until ctx is done, or a fault can't be served (the source failed after
// retries, or an ioctl failed): then the guest thread is stuck on its page and the VM must be
// stopped. It returns nil when ctx ends it.
func (h *Handler) Serve(ctx context.Context) error {
	var deferred deferredFaults
	stop := context.AfterFunc(ctx, func() { unix.Write(h.exitPipe[1], []byte{0}) })
	defer stop()
	// Workers may still run when an error path returns early: drain them so none outlives
	// Serve and races Close. Then wake any fault still deferred: the kernel never redelivers it,
	// so its thread would otherwise have no retry signal until the userfaultfd closes.
	defer func() {
		h.wg.Wait()
		for _, pf := range deferred.drain() {
			wake(h.fd, uintptr(pf.Address), h.pageSize)
		}
	}()
	pollFds := []unix.PollFd{
		{Fd: int32(h.fd), Events: unix.POLLIN},
		{Fd: int32(h.exitPipe[0]), Events: unix.POLLIN},
		{Fd: int32(h.wakeupPipe[0]), Events: unix.POLLIN},
	}
	// failed carries the first worker error to the loop, which then stops.
	failed := make(chan error, 1)
	for {
		if _, err := unix.Poll(pollFds, -1); err != nil {
			if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
				continue
			}
			return fmt.Errorf("poll: %w", err)
		}
		select {
		case err := <-failed:
			return err
		default:
		}
		if pollFds[1].Revents&unix.POLLIN != 0 {
			return h.wg.Wait()
		}
		if pollFds[2].Revents&unix.POLLIN != 0 {
			drain(h.wakeupPipe[0])
		}
		var pagefaults []uffdPagefault
		if pollFds[0].Revents&unix.POLLIN != 0 {
			removes, pfs, err := h.readEvents()
			if err != nil {
				return err
			}
			if len(removes) > 0 {
				h.settle.Lock()
				for _, rm := range removes {
					// rm.Start and rm.End are page-aligned for the registered range, so the
					// half-open page range lines up exactly.
					startOff, err := h.regions.offset(uintptr(rm.Start))
					if err != nil {
						continue
					}
					startIdx := uint32(startOff / int64(h.pageSize))
					h.tracker.SetRange(startIdx, startIdx+uint32((rm.End-rm.Start)/uint64(h.pageSize)), block.Removed)
				}
				h.settle.Unlock()
			}
			pagefaults = pfs
		} else if pollFds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return fmt.Errorf("userfaultfd poll error (revents %#x)", pollFds[0].Revents)
		}
		pagefaults = append(deferred.drain(), pagefaults...)
		for _, pf := range pagefaults {
			if pf.Flags&(UFFD_PAGEFAULT_FLAG_MINOR|UFFD_PAGEFAULT_FLAG_WP) != 0 {
				return fmt.Errorf("unexpected fault flags %#x: the memory is registered for missing faults only", pf.Flags)
			}
			addr := uintptr(pf.Address)
			offset, err := h.regions.offset(addr)
			if err != nil {
				return err
			}
			h.wg.Go(func() error {
				err := h.serveFault(ctx, pf, addr, offset, &deferred)
				if ctx.Err() != nil {
					// Serve is ending: a read it cut short is not a failure.
					return nil
				}
				if err != nil {
					select {
					case failed <- err:
					default:
					}
					unix.Write(h.wakeupPipe[1], []byte{1})
				}
				return err
			})
		}
	}
}

func (h *Handler) serveFault(ctx context.Context, pf uffdPagefault, addr uintptr, offset int64, deferred *deferredFaults) error {
	h.settle.RLock()
	defer h.settle.RUnlock()
	idx := uint32(offset / int64(h.pageSize))
	var src PageSource
	switch h.tracker.Get(idx) {
	case block.Dirty, block.Clean:
		// Installed already: a concurrent worker or a prefetch won, and its install woke the
		// thread. Pages must not be swappable for this to hold; only a REMOVE takes a page out
		// of these states, and it records Removed.
		return nil
	case block.Zero, block.Removed:
		// The guest freed the page; it reads as zeros, never the snapshot's old content.
	default:
		src = h.src
	}
	write := pf.Flags&UFFD_PAGEFAULT_FLAG_WRITE != 0
	outcome, err := h.install(ctx, addr, offset, src)
	if err != nil {
		return err
	}
	switch outcome {
	case installed, alreadyPresent:
		switch {
		case write:
			h.tracker.SetRange(idx, idx+1, block.Dirty)
		case src != nil:
			h.tracker.MarkInstalled(idx, block.Clean)
		default:
			h.tracker.MarkInstalled(idx, block.Zero)
		}
		h.faultMu.Lock()
		if !h.faultSeen[offset] {
			h.faultSeen[offset] = true
			h.faultOrder = append(h.faultOrder, offset)
		}
		h.faultMu.Unlock()
	case deferredFault:
		deferred.push(pf)
		unix.Write(h.wakeupPipe[1], []byte{1})
	}
	return nil
}

type outcome uint8

const (
	installed outcome = iota
	// alreadyPresent: the page was mapped already (EEXIST), by a concurrent worker or a
	// prefetch.
	alreadyPresent
	// deferredFault: a soft failure (EAGAIN); retry later.
	deferredFault
	// discarded: no install and no point retrying (ESRCH: the faulting thread is gone).
	discarded
)

// install puts the page at addr in place: from src, or zeros when src is nil.
func (h *Handler) install(ctx context.Context, addr uintptr, offset int64, src PageSource) (outcome, error) {
	var err error
	switch {
	case src == nil && h.pageSize == header.PageSize:
		err = zeroPage(h.fd, addr, h.pageSize)
	case src == nil:
		err = copyPage(h.fd, addr, h.pageSize, header.EmptyHugePage[:h.pageSize])
	default:
		b := make([]byte, h.pageSize)
		var dataErr error
		for attempt := range sliceMaxRetries + 1 {
			var n int
			n, dataErr = src.ReadAt(ctx, b, offset)
			if dataErr == nil && n != len(b) {
				dataErr = fmt.Errorf("short read at %d: got %d, want %d", offset, n, len(b))
			}
			if dataErr == nil || attempt == sliceMaxRetries || ctx.Err() != nil {
				break
			}
			delay := min(sliceRetryBaseDelay<<attempt, sliceRetryMaxDelay)
			select {
			case <-ctx.Done():
			case <-time.After(delay + time.Duration(rand.Int63n(int64(delay)/2))):
			}
		}
		if dataErr != nil {
			return discarded, fmt.Errorf("read page at %d: %w", offset, dataErr)
		}
		err = copyPage(h.fd, addr, h.pageSize, b)
	}
	switch {
	case err == nil:
		return installed, nil
	case errors.Is(err, unix.EEXIST):
		// Mapped already. Wake in case the install that won didn't.
		wake(h.fd, addr, h.pageSize)
		return alreadyPresent, nil
	case errors.Is(err, unix.ESRCH):
		return discarded, nil
	case errors.Is(err, unix.EAGAIN):
		// mmap_changing (a concurrent madvise, a REMOVE in flight) or a partial copy: the
		// caller defers the fault; the kernel does not redeliver it.
		return deferredFault, nil
	default:
		return discarded, fmt.Errorf("install page at %d: %w", offset, err)
	}
}

func (h *Handler) readEvents() ([]uffdRemove, []uffdPagefault, error) {
	var msg uffdMsg
	buf := unsafe.Slice((*byte)(unsafe.Pointer(&msg)), unsafe.Sizeof(msg))
	var removes []uffdRemove
	var pagefaults []uffdPagefault
	for {
		n, err := unix.Read(h.fd, buf)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EAGAIN) || n == 0 {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read userfaultfd: %w", err)
		}
		switch msg.Event {
		case UFFD_EVENT_PAGEFAULT:
			pagefaults = append(pagefaults, *(*uffdPagefault)(unsafe.Pointer(&msg.Arg[0])))
		case UFFD_EVENT_REMOVE:
			removes = append(removes, *(*uffdRemove)(unsafe.Pointer(&msg.Arg[0])))
		default:
			return nil, nil, fmt.Errorf("unexpected userfaultfd event %#x", msg.Event)
		}
	}
	return removes, pagefaults, nil
}

// Prefetch installs the pages at offsets (memory-file offsets, in order) ahead of the guest,
// with a few workers, until it runs out or ctx ends. A page the guest already faulted in, or
// freed, is skipped. Run it beside Serve; it returns when done.
func (h *Handler) Prefetch(ctx context.Context, offsets []int64, workers int) {
	next := make(chan int64)
	var wg sync.WaitGroup
	for range max(workers, 1) {
		wg.Go(func() {
			for off := range next {
				h.prefault(ctx, off)
			}
		})
	}
	for _, off := range offsets {
		select {
		case next <- off - off%int64(h.pageSize):
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(next)
	wg.Wait()
}

func (h *Handler) prefault(ctx context.Context, offset int64) {
	addr, err := h.regions.hostAddr(offset)
	if err != nil {
		return
	}
	idx := uint32(offset / int64(h.pageSize))
	if h.tracker.Get(idx) != block.NotPresent {
		return
	}
	// Read outside the lock: a slow fetch must not hold up a REMOVE batch.
	b := make([]byte, h.pageSize)
	if n, err := h.src.ReadAt(ctx, b, offset); err != nil || n != len(b) {
		return
	}
	h.settle.RLock()
	defer h.settle.RUnlock()
	if h.closed || h.tracker.Get(idx) != block.NotPresent {
		return
	}
	if copyPage(h.fd, addr, h.pageSize, b) == nil {
		h.tracker.MarkInstalled(idx, block.Clean)
	}
}

// Close closes the userfaultfd and the handler's pipes. Call it after Serve returned.
func (h *Handler) Close() error {
	var err error
	h.closeOnce.Do(func() {
		h.settle.Lock()
		defer h.settle.Unlock()
		h.closed = true
		for _, fd := range []int{h.wakeupPipe[0], h.wakeupPipe[1], h.exitPipe[0], h.exitPipe[1]} {
			unix.Close(fd)
		}
		err = unix.Close(h.fd)
	})
	return err
}

func drain(fd int) {
	var buf [64]byte
	for {
		if _, err := unix.Read(fd, buf[:]); err != nil {
			return
		}
	}
}

// deferredFaults collects faults that hit EAGAIN, to retry on the next poll iteration. A page
// faulted by several threads is retried once; if one of them was a write, the retained fault is
// upgraded to a write so the retry records the page dirty.
type deferredFaults struct {
	mu     sync.Mutex
	pf     []uffdPagefault
	byAddr map[uint64]int
}

func (d *deferredFaults) push(pf uffdPagefault) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.byAddr == nil {
		d.byAddr = map[uint64]int{}
	}
	if i, ok := d.byAddr[pf.Address]; ok {
		d.pf[i].Flags |= pf.Flags & UFFD_PAGEFAULT_FLAG_WRITE
		return
	}
	d.byAddr[pf.Address] = len(d.pf)
	d.pf = append(d.pf, pf)
}

func (d *deferredFaults) drain() []uffdPagefault {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.pf
	d.pf, d.byAddr = nil, nil
	return out
}
