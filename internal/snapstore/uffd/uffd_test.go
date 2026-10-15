package uffd_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/arugula-salad/loam/internal/snapstore"
	"github.com/arugula-salad/loam/internal/snapstore/header"
	"github.com/arugula-salad/loam/internal/snapstore/storage"
	"github.com/arugula-salad/loam/internal/snapstore/uffd"
)

const (
	childEnv  = "SNAPSTORE_UFFD_CHILD"
	skipCode  = 3
	page      = header.PageSize
	memSize   = 8 << 20
	freedPage = 7 // the page the child gives back (madvise), which must then read as zeros
)

func TestMain(m *testing.M) {
	if os.Getenv(childEnv) != "" {
		os.Exit(child())
	}
	os.Exit(m.Run())
}

// newUffd makes the child's userfaultfd as Firecracker 1.17 does, through /dev/userfaultfd. The
// device is often root-only on a development host, and only there does the test fall back to
// the syscall: a container's seccomp (containerd's RuntimeDefault) refuses it, and snapstore
// itself never makes the call. Both are user-mode-only, which needs no privilege and serves
// this process's own faults (not KVM's: Firecracker needs the privilege, see docs/STORE.md).
func newUffd() (int, error) {
	fd, err := uffd.Open(uffd.UFFD_FEATURE_EVENT_REMOVE, true)
	if err == nil {
		return fd, nil
	}
	r, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, unix.O_CLOEXEC|unix.O_NONBLOCK|uffd.UFFD_USER_MODE_ONLY, 0, 0)
	if errno != 0 {
		return -1, fmt.Errorf("%v; userfaultfd(2): %v", err, errno)
	}
	if err := uffd.API(int(r), uffd.UFFD_FEATURE_EVENT_REMOVE); err != nil {
		unix.Close(int(r))
		return -1, err
	}
	return int(r), nil
}

// child plays Firecracker: it owns the memory, registers it with a userfaultfd, hands both to
// the handler over the socket, and then touches the memory.
func child() int {
	sock, expected := os.Getenv("SOCK"), os.Getenv("EXPECTED")
	want, err := os.ReadFile(expected)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fd, err := newUffd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "no userfaultfd:", err)
		return skipCode
	}
	mem, err := unix.Mmap(-1, 0, memSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	addr := uintptr(unsafe.Pointer(&mem[0]))
	if err := uffd.Register(fd, addr, memSize); err != nil {
		fmt.Fprintln(os.Stderr, "no userfaultfd:", err)
		return skipCode
	}
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// Firecracker's handshake: the regions as JSON (both page-size fields, as it sends them)
	// and the userfaultfd as SCM_RIGHTS, in one message.
	regions, _ := json.Marshal([]map[string]any{{
		"base_host_virt_addr": addr, "size": memSize, "offset": 0, "page_size": page, "page_size_kib": page,
	}})
	if _, _, err := conn.WriteMsgUnix(regions, unix.UnixRights(fd), nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	unix.Close(fd)
	mismatches := 0
	r := rand.New(rand.NewSource(99))
	for range 1000 {
		off := r.Intn(memSize/page) * page
		if !bytes.Equal(mem[off:off+page], want[off:off+page]) {
			mismatches++
		}
	}
	// A write fault on a page not yet touched, then give a touched page back: it must read as
	// zeros from then on, not the snapshot's content.
	mem[3*page+5] ^= 0xff
	want[3*page+5] ^= 0xff
	_ = mem[freedPage*page]
	if err := unix.Madvise(mem[freedPage*page:(freedPage+1)*page], unix.MADV_DONTNEED); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	clear(want[freedPage*page : (freedPage+1)*page])
	for off := 0; off < memSize; off += page {
		if !bytes.Equal(mem[off:off+page], want[off:off+page]) {
			mismatches++
		}
	}
	if mismatches > 0 {
		fmt.Fprintf(os.Stderr, "%d mismatched pages\n", mismatches)
		return 1
	}
	return 0
}

func TestServeFirecrackerHandshake(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	// The memory file, stored as a snapshot layer and read back through the store, as the
	// nodelet's UFFD helper does.
	want := make([]byte, memSize)
	r := rand.New(rand.NewSource(1))
	for i := 0; i < memSize/page; i++ {
		if i%5 != 0 {
			r.Read(want[i*page : i*page+64])
		}
	}
	mem := filepath.Join(work, "mem")
	os.WriteFile(mem, want, 0o600)
	b, _ := storage.NewFS(filepath.Join(work, "bucket"))
	s, err := snapstore.New(b, snapstore.Options{CacheDir: filepath.Join(work, "cache")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	parent, _ := header.NewZero(memSize, page)
	pl, _ := s.NewLayer(snapstore.LayerSpec{Layer: header.NewLayerID(), Kind: snapstore.Memory, Path: mem, Size: memSize,
		Dirty: []snapstore.Extent{{Off: 0, Len: memSize}}})
	h, err := pl.Upload(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	file, err := s.OpenFile(h, snapstore.Memory, "")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	// sun_path is 108 bytes: keep the socket's path short.
	sock := filepath.Join(os.TempDir(), "uffd-"+strconv.Itoa(os.Getpid())+".sock")
	os.Remove(sock)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(sock)
	defer l.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), childEnv+"=1", "SOCK="+sock, "EXPECTED="+mem)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(waited) }()

	// A child that can't make a userfaultfd exits without connecting: stop waiting for it.
	actx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	go func() {
		select {
		case <-waited:
			cancel()
		case <-actx.Done():
		}
	}()
	hs, err := uffd.Accept(actx, l)
	if err != nil {
		<-waited
		if isSkip(waitErr) {
			t.Skipf("the kernel refuses an unprivileged userfaultfd here: %s", stderr.String())
		}
		t.Fatalf("accept: %v (child: %v: %s)", err, waitErr, stderr.String())
	}
	if len(hs.Regions) != 1 || hs.Regions[0].PageSize != page || hs.Regions[0].Size != memSize {
		t.Fatalf("regions %+v", hs.Regions)
	}
	handler, err := uffd.New(hs.FD, hs.Regions, file)
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()
	sctx, stop := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- handler.Serve(sctx) }()
	go handler.Prefetch(sctx, []int64{100 * page, 101 * page, 102 * page}, 2)

	select {
	case <-waited:
		if waitErr != nil {
			t.Fatalf("child: %v: %s", waitErr, stderr.String())
		}
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		t.Fatal("child hung: a fault went unanswered")
	}
	stop()
	if err := <-served; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if n := len(handler.Faults()); n < 500 {
		t.Fatalf("recorded %d faults", n)
	}
	if !handler.Zeroed().Contains(freedPage) {
		t.Fatal("the page the guest gave back is not recorded as zeros")
	}
}

func isSkip(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == skipCode
}
