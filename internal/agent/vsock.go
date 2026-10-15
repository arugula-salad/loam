package agent

import (
	"context"
	"fmt"
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// AF_VSOCK streams with nothing but x/sys/unix. The sockets are nonblocking
// and wrapped in os.File, so the runtime's poller drives them: accept and
// read block a goroutine, not a thread, and Close interrupts them.

// VsockAddr is one end of a vsock stream.
type VsockAddr struct{ CID, Port uint32 }

func (a VsockAddr) Network() string { return "vsock" }
func (a VsockAddr) String() string  { return fmt.Sprintf("vsock:%d:%d", a.CID, a.Port) }

type vsockListener struct {
	f    *os.File
	rc   syscall.RawConn
	addr VsockAddr
}

// ListenVsock listens on port for streams from any CID (which, in a guest,
// means the host).
func ListenVsock(port uint32) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock bind %d: %w", port, err)
	}
	if err := unix.Listen(fd, 128); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock listen: %w", err)
	}
	f := os.NewFile(uintptr(fd), "vsock-listener")
	rc, err := f.SyscallConn()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &vsockListener{f: f, rc: rc, addr: VsockAddr{CID: unix.VMADDR_CID_ANY, Port: port}}, nil
}

func (l *vsockListener) Accept() (net.Conn, error) {
	var nfd int
	var sa unix.Sockaddr
	var aerr error
	err := l.rc.Read(func(fd uintptr) bool {
		nfd, sa, aerr = unix.Accept4(int(fd), unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
		return aerr != unix.EAGAIN
	})
	if err != nil {
		return nil, err
	}
	if aerr != nil {
		return nil, aerr
	}
	remote := VsockAddr{}
	if vm, ok := sa.(*unix.SockaddrVM); ok {
		remote = VsockAddr{CID: vm.CID, Port: vm.Port}
	}
	return newVsockConn(nfd, l.addr, remote), nil
}

func (l *vsockListener) Close() error   { return l.f.Close() }
func (l *vsockListener) Addr() net.Addr { return l.addr }

// vsockConn is a connected stream. os.File gives Read, Write, Close and the
// deadlines for a pollable descriptor; the rest is the net.Conn surface.
type vsockConn struct {
	*os.File
	local, remote VsockAddr
}

func newVsockConn(fd int, local, remote VsockAddr) *vsockConn {
	return &vsockConn{File: os.NewFile(uintptr(fd), "vsock"), local: local, remote: remote}
}

func (c *vsockConn) LocalAddr() net.Addr  { return c.local }
func (c *vsockConn) RemoteAddr() net.Addr { return c.remote }

// CloseWrite half-closes the stream.
func (c *vsockConn) CloseWrite() error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = unix.Shutdown(int(fd), unix.SHUT_WR) }); err != nil {
		return err
	}
	return serr
}

// DialVsock connects to port on cid. The connect itself blocks (vsock has its
// own connect timeout, 2 s by default); the stream is nonblocking after.
func DialVsock(ctx context.Context, cid, port uint32) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: cid, Port: port}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock connect %d:%d: %w", cid, port, err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}
	local := VsockAddr{Port: 0}
	if sa, err := unix.Getsockname(fd); err == nil {
		if vm, ok := sa.(*unix.SockaddrVM); ok {
			local = VsockAddr{CID: vm.CID, Port: vm.Port}
		}
	}
	return newVsockConn(fd, local, VsockAddr{CID: cid, Port: port}), nil
}
