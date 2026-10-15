package agent

import (
	"encoding/binary"
	"errors"
	"net"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
)

// The few rtnetlink requests the agent makes to configure eth0: set a link
// up (and its MAC), replace an address, delete the others, replace the
// default route. Four message shapes are not worth a netlink library.

var nlSeq atomic.Uint32

// nlAttr is one rtattr: length, type, then the value padded to 4 bytes.
func nlAttr(typ uint16, value []byte) []byte {
	l := 4 + len(value)
	b := make([]byte, (l+3)&^3)
	binary.NativeEndian.PutUint16(b[0:2], uint16(l))
	binary.NativeEndian.PutUint16(b[2:4], typ)
	copy(b[4:], value)
	return b
}

// nlMessage is a netlink header for body.
func nlMessage(typ, flags uint16, body []byte) []byte {
	b := make([]byte, unix.SizeofNlMsghdr, unix.SizeofNlMsghdr+len(body))
	binary.NativeEndian.PutUint32(b[0:4], uint32(unix.SizeofNlMsghdr+len(body)))
	binary.NativeEndian.PutUint16(b[4:6], typ)
	binary.NativeEndian.PutUint16(b[6:8], flags|unix.NLM_F_REQUEST|unix.NLM_F_ACK)
	binary.NativeEndian.PutUint32(b[8:12], nlSeq.Add(1))
	return append(b, body...)
}

// nlRequest sends one request and waits for its acknowledgement.
func nlRequest(msg []byte) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	if err := unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	seq := binary.NativeEndian.Uint32(msg[8:12])
	buf := make([]byte, 1<<16)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return err
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if m.Header.Seq != seq || m.Header.Type != unix.NLMSG_ERROR {
				continue
			}
			if len(m.Data) < 4 {
				return errors.New("short netlink error")
			}
			if errno := int32(binary.NativeEndian.Uint32(m.Data[0:4])); errno != 0 {
				return syscall.Errno(-errno)
			}
			return nil
		}
	}
}

// linkSet sets a link up or down and, with mac, its address.
func linkSet(index int, up bool, mac net.HardwareAddr) error {
	body := make([]byte, unix.SizeofIfInfomsg)
	body[0] = unix.AF_UNSPEC
	binary.NativeEndian.PutUint32(body[4:8], uint32(index))
	var flags uint32
	if up {
		flags = unix.IFF_UP
	}
	binary.NativeEndian.PutUint32(body[8:12], flags)
	binary.NativeEndian.PutUint32(body[12:16], unix.IFF_UP)
	if mac != nil {
		body = append(body, nlAttr(unix.IFLA_ADDRESS, mac)...)
	}
	return nlRequest(nlMessage(unix.RTM_NEWLINK, 0, body))
}

func ifaddrmsg(index int, prefix int) []byte {
	b := make([]byte, unix.SizeofIfAddrmsg)
	b[0] = unix.AF_INET
	b[1] = byte(prefix)
	b[3] = unix.RT_SCOPE_UNIVERSE
	binary.NativeEndian.PutUint32(b[4:8], uint32(index))
	return b
}

func addrReplace(index int, ip net.IP, prefix int) error {
	body := ifaddrmsg(index, prefix)
	body = append(body, nlAttr(unix.IFA_LOCAL, ip)...)
	body = append(body, nlAttr(unix.IFA_ADDRESS, ip)...)
	return nlRequest(nlMessage(unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_REPLACE, body))
}

// flushOtherAddrs deletes every IPv4 address on the link but keep.
func flushOtherAddrs(index int, keep net.IP) error {
	rib, err := syscall.NetlinkRIB(unix.RTM_GETADDR, unix.AF_INET)
	if err != nil {
		return err
	}
	msgs, err := syscall.ParseNetlinkMessage(rib)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		if m.Header.Type != unix.RTM_NEWADDR || len(m.Data) < unix.SizeofIfAddrmsg {
			continue
		}
		if int(binary.NativeEndian.Uint32(m.Data[4:8])) != index {
			continue
		}
		attrs, err := syscall.ParseNetlinkRouteAttr(&m)
		if err != nil {
			continue
		}
		for _, a := range attrs {
			if a.Attr.Type != unix.IFA_LOCAL || len(a.Value) != 4 || net.IP(a.Value).Equal(keep) {
				continue
			}
			body := ifaddrmsg(index, int(m.Data[1]))
			body = append(body, nlAttr(unix.IFA_LOCAL, a.Value)...)
			if err := nlRequest(nlMessage(unix.RTM_DELADDR, 0, body)); err != nil && !errors.Is(err, unix.EADDRNOTAVAIL) {
				return err
			}
		}
	}
	return nil
}

func defaultRouteReplace(index int, gw net.IP) error {
	body := make([]byte, unix.SizeofRtMsg)
	body[0] = unix.AF_INET
	body[4] = unix.RT_TABLE_MAIN
	body[5] = unix.RTPROT_BOOT
	body[6] = unix.RT_SCOPE_UNIVERSE
	body[7] = unix.RTN_UNICAST
	body = append(body, nlAttr(unix.RTA_GATEWAY, gw)...)
	oif := make([]byte, 4)
	binary.NativeEndian.PutUint32(oif, uint32(index))
	body = append(body, nlAttr(unix.RTA_OIF, oif)...)
	return nlRequest(nlMessage(unix.RTM_NEWROUTE, unix.NLM_F_CREATE|unix.NLM_F_REPLACE, body))
}
