package agent

import (
	"encoding/binary"
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

func TestNetlinkEncoding(t *testing.T) {
	a := nlAttr(unix.IFLA_ADDRESS, net.HardwareAddr{6, 0, 10, 240, 0, 7})
	if len(a) != 12 || binary.NativeEndian.Uint16(a[0:2]) != 10 || binary.NativeEndian.Uint16(a[2:4]) != unix.IFLA_ADDRESS {
		t.Fatalf("attr % x", a)
	}
	m := nlMessage(unix.RTM_NEWADDR, unix.NLM_F_CREATE, []byte{1, 2, 3, 4})
	if binary.NativeEndian.Uint32(m[0:4]) != uint32(len(m)) || binary.NativeEndian.Uint16(m[4:6]) != unix.RTM_NEWADDR ||
		binary.NativeEndian.Uint16(m[6:8]) != unix.NLM_F_CREATE|unix.NLM_F_REQUEST|unix.NLM_F_ACK {
		t.Fatalf("message % x", m)
	}
}
