// Derived from github.com/e2b-dev/infra, packages/orchestrator/pkg/sandbox/uffd/memory/region.go,
// memory/mapping.go and uffd.go (the handshake) at commit cf536c2 (Copyright 2023 FoundryLabs,
// Inc., Apache-2.0, see ../LICENSE-e2b). Modified for loam: one package, both of Firecracker's
// page-size fields read, the listener's lifetime left to the caller.

package uffd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
)

// Region maps a region of guest memory, at Offset in the memory file, to where Firecracker has
// it in its address space. The JSON is Firecracker's UFFD handshake (GuestRegionUffdMapping in
// src/vmm/src/persist.rs): Firecracker sends a list of these with the userfaultfd.
type Region struct {
	BaseHostVirtAddr uintptr `json:"base_host_virt_addr"`
	Size             uintptr `json:"size"`
	Offset           uintptr `json:"offset"`
	// PageSize is in bytes. Firecracker sends it as page_size, and as the deprecated
	// page_size_kib, which despite its name is in bytes too; older versions only the latter.
	PageSize uintptr `json:"page_size"`
}

func (r *Region) UnmarshalJSON(b []byte) error {
	var v struct {
		BaseHostVirtAddr uintptr  `json:"base_host_virt_addr"`
		Size             uintptr  `json:"size"`
		Offset           uintptr  `json:"offset"`
		PageSize         *uintptr `json:"page_size"`
		PageSizeKiB      *uintptr `json:"page_size_kib"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*r = Region{BaseHostVirtAddr: v.BaseHostVirtAddr, Size: v.Size, Offset: v.Offset}
	switch {
	case v.PageSize != nil:
		r.PageSize = *v.PageSize
	case v.PageSizeKiB != nil:
		r.PageSize = *v.PageSizeKiB
	default:
		return errors.New("memory region without a page size")
	}
	return nil
}

func (r Region) endHostVirtAddr() uintptr { return r.BaseHostVirtAddr + r.Size }

// offsetOf returns the memory-file offset of a host address in the region.
func (r Region) offsetOf(addr uintptr) int64 { return int64(addr - r.BaseHostVirtAddr + r.Offset) }

// mapping translates between host addresses and memory-file offsets.
type mapping []Region

func (m mapping) offset(addr uintptr) (int64, error) {
	for _, r := range m {
		if addr >= r.BaseHostVirtAddr && addr < r.endHostVirtAddr() {
			return r.offsetOf(addr), nil
		}
	}
	return 0, fmt.Errorf("host address %#x not in any region", addr)
}

func (m mapping) hostAddr(off int64) (uintptr, error) {
	for _, r := range m {
		if off >= int64(r.Offset) && off < int64(r.Offset+r.Size) {
			return uintptr(off) + r.BaseHostVirtAddr - r.Offset, nil
		}
	}
	return 0, fmt.Errorf("offset %d not in any region", off)
}

// Handshake is what Firecracker hands over when it connects: its userfaultfd for the guest
// memory, and where that memory is.
type Handshake struct {
	FD      int
	Regions []Region
}

// Accept waits (until ctx is done) for Firecracker to connect to l and reads the handshake: one
// message carrying the regions as JSON and the userfaultfd as SCM_RIGHTS. Firecracker connects
// during PUT /snapshot/load with mem_backend {"backend_type": "Uffd", "backend_path": <socket>},
// and the load doesn't finish until the handler serves the faults it takes. Newer Firecrackers
// may pass a second fd (a guest_memfd); it is closed, loam doesn't use it.
func Accept(ctx context.Context, l *net.UnixListener) (*Handshake, error) {
	stop := context.AfterFunc(ctx, func() { l.SetDeadline(time.Now()) })
	defer stop()
	conn, err := l.AcceptUnix()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("accept firecracker: %w", err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 64<<10)
	oob := make([]byte, syscall.CmsgSpace(4*4))
	n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, fmt.Errorf("read handshake: %w", err)
	}
	var fds []int
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, fmt.Errorf("parse handshake control message: %w", err)
	}
	for i := range msgs {
		got, err := syscall.ParseUnixRights(&msgs[i])
		if err == nil {
			fds = append(fds, got...)
		}
	}
	closeFds := func() {
		for _, fd := range fds {
			syscall.Close(fd)
		}
	}
	if len(fds) == 0 {
		return nil, errors.New("handshake carried no userfaultfd")
	}
	var regions []Region
	if err := json.Unmarshal(buf[:n], &regions); err != nil {
		closeFds()
		return nil, fmt.Errorf("parse handshake regions: %w", err)
	}
	for _, fd := range fds[1:] {
		syscall.Close(fd)
	}
	return &Handshake{FD: fds[0], Regions: regions}, nil
}
