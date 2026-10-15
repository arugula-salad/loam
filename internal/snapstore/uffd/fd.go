// Derived from github.com/e2b-dev/infra, packages/orchestrator/pkg/sandbox/uffd/userfaultfd/fd.go
// at commit cf536c2 (Copyright 2023 FoundryLabs, Inc., Apache-2.0, see ../LICENSE-e2b), whose
// constants follow github.com/loopholelabs/userfaultfd-go (pkg/constants/cgo.go, Apache-2.0).
// Modified for loam: no cgo; the ioctl numbers and structs are written out from
// <linux/userfaultfd.h> as Go constants, and write-protect support is dropped.

package uffd

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// From <linux/userfaultfd.h>. The ioctl numbers are _IOWR/_IOR(0xAA, nr, struct) for amd64 and
// arm64 alike.
const (
	UFFD_API = 0xAA

	UFFD_USER_MODE_ONLY = 1

	UFFD_EVENT_PAGEFAULT = 0x12
	UFFD_EVENT_REMOVE    = 0x15

	UFFD_PAGEFAULT_FLAG_WRITE = 1 << 0
	UFFD_PAGEFAULT_FLAG_WP    = 1 << 1
	UFFD_PAGEFAULT_FLAG_MINOR = 1 << 2

	UFFD_FEATURE_EVENT_REMOVE = 1 << 3

	UFFDIO_REGISTER_MODE_MISSING = 1 << 0

	UFFDIO_API      = 0xc018aa3f // _IOWR(0xAA, 0x3F, struct uffdio_api)
	UFFDIO_REGISTER = 0xc020aa00 // _IOWR(0xAA, 0x00, struct uffdio_register)
	UFFDIO_WAKE     = 0x8010aa02 // _IOR(0xAA, 0x02, struct uffdio_range)
	UFFDIO_COPY     = 0xc028aa03 // _IOWR(0xAA, 0x03, struct uffdio_copy)
	UFFDIO_ZEROPAGE = 0xc020aa04 // _IOWR(0xAA, 0x04, struct uffdio_zeropage)

	// USERFAULTFD_IOC_NEW is /dev/userfaultfd's ioctl that makes a userfaultfd: _IO(0xAA, 0x00).
	// Where seccomp refuses the userfaultfd(2) syscall (containerd's RuntimeDefault does), the
	// device is how a process gets one; Firecracker 1.17 tries it first.
	USERFAULTFD_IOC_NEW = 0xaa00
)

type uffdioAPI struct {
	API      uint64
	Features uint64
	Ioctls   uint64
}

type uffdioRange struct {
	Start uint64
	Len   uint64
}

type uffdioRegister struct {
	Range  uffdioRange
	Mode   uint64
	Ioctls uint64
}

type uffdioCopy struct {
	Dst  uint64
	Src  uint64
	Len  uint64
	Mode uint64
	Copy int64
}

type uffdioZeropage struct {
	Range    uffdioRange
	Mode     uint64
	Zeropage int64
}

// uffdMsg is struct uffd_msg: an event byte, padding, and a 24-byte union.
type uffdMsg struct {
	Event uint8
	_     [7]uint8
	Arg   [24]byte
}

type uffdPagefault struct {
	Flags   uint64
	Address uint64
	Ptid    uint32
	_       uint32
}

type uffdRemove struct {
	Start uint64
	End   uint64
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// Open makes a userfaultfd through /dev/userfaultfd (never the syscall: see
// USERFAULTFD_IOC_NEW), non-blocking and close-on-exec, and negotiates the API with features.
// userModeOnly limits it to faults from user space, which needs no privilege but cannot serve
// KVM's: it is for tests that play Firecracker's part.
func Open(features uint64, userModeOnly bool) (int, error) {
	dev, err := unix.Open("/dev/userfaultfd", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open /dev/userfaultfd: %w", err)
	}
	defer unix.Close(dev)
	flags := unix.O_CLOEXEC | unix.O_NONBLOCK
	if userModeOnly {
		flags |= UFFD_USER_MODE_ONLY
	}
	r, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(dev), USERFAULTFD_IOC_NEW, uintptr(flags))
	if errno != 0 {
		return -1, fmt.Errorf("USERFAULTFD_IOC_NEW: %w", errno)
	}
	fd := int(r)
	if err := API(fd, features); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// API negotiates the userfaultfd API on a new fd.
func API(fd int, features uint64) error {
	api := uffdioAPI{API: UFFD_API, Features: features}
	if err := ioctl(fd, UFFDIO_API, unsafe.Pointer(&api)); err != nil {
		return fmt.Errorf("UFFDIO_API: %w", err)
	}
	return nil
}

// Register registers [start, start+length) for missing faults, as Firecracker does with guest
// memory.
func Register(fd int, start, length uintptr) error {
	reg := uffdioRegister{Range: uffdioRange{Start: uint64(start), Len: uint64(length)}, Mode: UFFDIO_REGISTER_MODE_MISSING}
	if err := ioctl(fd, UFFDIO_REGISTER, unsafe.Pointer(&reg)); err != nil {
		return fmt.Errorf("UFFDIO_REGISTER: %w", err)
	}
	return nil
}

// copyPage installs data at the page holding addr and wakes the faulting thread.
func copyPage(fd int, addr, pagesize uintptr, data []byte) error {
	if len(data) == 0 {
		return errors.New("cannot copy from an empty buffer")
	}
	// UFFDIO_COPY hides src as an integer, so keep data pinned while the kernel reads it.
	var pinner runtime.Pinner
	pinner.Pin(&data[0])
	defer pinner.Unpin()
	cpy := uffdioCopy{
		Dst: uint64(addr &^ (pagesize - 1)),
		Src: uint64(uintptr(unsafe.Pointer(&data[0]))),
		Len: uint64(pagesize),
	}
	if err := ioctl(fd, UFFDIO_COPY, unsafe.Pointer(&cpy)); err != nil {
		return err
	}
	return classifyCopyResult(cpy.Copy, int64(pagesize))
}

// classifyCopyResult turns UFFDIO_COPY's copy field into an error. The kernel encodes a negated
// errno on failure (most commonly -EAGAIN when mmap_changing is set), and a short positive count
// when the copy was preempted mid-page (hugetlb). Both partial outcomes surface as EAGAIN; the
// caller defers the fault and retries it, the kernel does not redeliver it.
func classifyCopyResult(bytesCopied, pagesize int64) error {
	if bytesCopied < 0 {
		return unix.Errno(-bytesCopied)
	}
	if bytesCopied != pagesize {
		return unix.EAGAIN
	}
	return nil
}

// zeroPage installs a page of zeros at the page holding addr. Only for 4 KiB pages: hugetlbfs
// has no UFFDIO_ZEROPAGE, so a huge page is zeroed by copying zeros.
func zeroPage(fd int, addr, pagesize uintptr) error {
	z := uffdioZeropage{Range: uffdioRange{Start: uint64(addr &^ (pagesize - 1)), Len: uint64(pagesize)}}
	if err := ioctl(fd, UFFDIO_ZEROPAGE, unsafe.Pointer(&z)); err != nil {
		return err
	}
	if z.Zeropage < 0 {
		return unix.Errno(-z.Zeropage)
	}
	if z.Zeropage != int64(pagesize) {
		return unix.EAGAIN
	}
	return nil
}

func wake(fd int, addr, pagesize uintptr) error {
	r := uffdioRange{Start: uint64(addr &^ (pagesize - 1)), Len: uint64(pagesize)}
	return ioctl(fd, UFFDIO_WAKE, unsafe.Pointer(&r))
}
