package network

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// DNS is the VMs' resolver on the gateway. It knows each VM by the query's
// source address (Enforcer). An unrestricted VM's queries go to public
// resolvers as they are, and their answers come back as they are: the
// nftables rules already keep that VM from whatever private address an
// answer names. A restricted VM's queries are read: a name its policy doesn't
// resolve is refused, IPv6 and private answers are stripped, and every
// address it is answered is remembered for the proxy (docs/NODE.md, "Phase
// 3: the network boundary"). A source no running VM owns is refused.
type DNS struct {
	Enforcer  *Enforcer
	Upstreams []string // host:port, tried in order
	Log       *slog.Logger
	// Blocked filters a restricted VM's answers: an allowed name that
	// resolves to a private address (DNS rebinding) must not become a way
	// into the pod's networks.
	Blocked func(netip.Addr) bool
	Timeout time.Duration // per upstream attempt; 0 means 2s
}

const maxDNSMessage = 65535

// Upstreams turns the --dns-upstream flag ("1.1.1.1,9.9.9.9") into dialable addresses.
func Upstreams(list string) []string {
	var out []string
	for _, s := range strings.Split(list, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(s); err != nil {
			s = net.JoinHostPort(s, "53")
		}
		out = append(out, s)
	}
	return out
}

func (d *DNS) timeout() time.Duration {
	if d.Timeout == 0 {
		return 2 * time.Second
	}
	return d.Timeout
}

// Start serves on addr over UDP and TCP until stop is called and reports the
// UDP address it bound (the TCP listener takes the same port).
func (d *DNS) Start(addr string) (bound string, stop func(), err error) {
	if d.Blocked == nil {
		d.Blocked = Blocked
	}
	pc, ln, err := listenUDPAndTCP(addr)
	if err != nil {
		return "", nil, err
	}
	go d.serveUDP(pc)
	go d.serveTCP(ln)
	return pc.LocalAddr().String(), func() { pc.Close(); ln.Close() }, nil
}

// listenUDPAndTCP binds UDP and TCP on the same address. The TCP listener takes
// the port UDP got, so addr may use port 0 (in tests). The UDP and TCP port
// spaces are separate, so the port the kernel picked for UDP can already be held
// on the TCP side (another listener, or the local end of an outgoing or
// TIME_WAIT connection); with port 0 we just pick again.
func listenUDPAndTCP(addr string) (net.PacketConn, net.Listener, error) {
	ap, perr := netip.ParseAddrPort(addr)
	retry := perr == nil && ap.Port() == 0
	for attempt := 0; ; attempt++ {
		pc, err := net.ListenPacket("udp4", addr)
		if err != nil {
			return nil, nil, err
		}
		ln, err := net.Listen("tcp4", pc.LocalAddr().String())
		if err == nil {
			return pc, ln, nil
		}
		pc.Close()
		if !retry || !errors.Is(err, syscall.EADDRINUSE) || attempt >= 20 {
			return nil, nil, err
		}
	}
}

func (d *DNS) serveUDP(pc net.PacketConn) {
	for {
		buf := make([]byte, maxDNSMessage)
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		src, ok := netip.AddrFromSlice(from.(*net.UDPAddr).IP)
		if !ok {
			continue
		}
		go func(q []byte) {
			if resp := d.answer(src.Unmap(), q, true); resp != nil {
				pc.WriteTo(resp, from)
			}
		}(buf[:n])
	}
}

func (d *DNS) serveTCP(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go d.handleTCP(c)
	}
}

func (d *DNS) handleTCP(c net.Conn) {
	defer c.Close()
	src := c.RemoteAddr().(*net.TCPAddr).AddrPort().Addr().Unmap()
	for {
		c.SetDeadline(time.Now().Add(10 * time.Second))
		q, err := readTCPMessage(c)
		if err != nil {
			return
		}
		resp := d.answer(src, q, false)
		if resp == nil {
			return
		}
		if err := writeTCPMessage(c, resp); err != nil {
			return
		}
	}
}

// answer is the reply to the query q from src, or nil for none.
func (d *DNS) answer(src netip.Addr, q []byte, udp bool) []byte {
	id, policy, known := d.Enforcer.Lookup(src)
	if known && !policy.Restricted() {
		// Unrestricted: relayed as it is. A truncated UDP answer goes back as
		// it is too: the client retries over TCP, which is relayed over TCP.
		var resp []byte
		var err error
		if udp {
			resp, err = d.exchangeUDP(q)
		} else {
			resp, err = d.exchangeTCP(q)
		}
		if err != nil {
			d.Log.Debug("dns upstream failed", "vm", id, "err", err)
			return nil
		}
		return resp
	}

	var m dnsmessage.Message
	if err := m.Unpack(q); err != nil || m.Header.Response {
		return nil // not a query: nothing to answer
	}
	refuse := func() []byte { return reply(m, dnsmessage.RCodeRefused) }
	if !known || m.Header.OpCode != 0 || len(m.Questions) != 1 || m.Questions[0].Class != dnsmessage.ClassINET {
		return refuse()
	}
	qn := m.Questions[0]
	name := canonicalName(qn.Name.String())
	if !policy.Resolves(name) {
		d.Log.Info("egress dns refused", "vm", id, "domain", name, "type", qn.Type)
		return refuse()
	}
	switch qn.Type {
	case dnsmessage.TypeAAAA, dnsmessage.TypeHTTPS, dnsmessage.TypeSVCB:
		// The guest has no IPv6 when restricted, so an empty answer sends
		// clients straight to A instead of trying addresses that can only time
		// out. HTTPS and SVCB records carry address hints, which would skip
		// the filter below, and ECH keys, which would hide the name the proxy
		// has to read.
		return reply(m, dnsmessage.RCodeSuccess)
	case dnsmessage.TypeALL, dnsmessage.TypeAXFR, typeIXFR:
		return refuse()
	}

	// Asked with an ID of our own, not the guest's: upstream answers are
	// matched on it, and a guest that chose IDs could try to collide them.
	fwd := append([]byte(nil), q...)
	var rid [2]byte
	rand.Read(rid[:])
	copy(fwd, rid[:])
	resp, err := d.exchangeUDP(fwd)
	if err == nil && len(resp) > 2 && resp[2]&0x02 != 0 { // TC: ask again over TCP
		resp, err = d.exchangeTCP(fwd)
	}
	var r dnsmessage.Message
	if err == nil {
		err = r.Unpack(resp)
	}
	if err != nil {
		d.Log.Warn("egress dns upstream failed", "vm", id, "domain", name, "err", err)
		return reply(m, dnsmessage.RCodeServerFailure)
	}
	r.Header.ID = m.Header.ID
	r.Answers = d.filter(r.Answers, id, name)
	r.Authorities = d.filter(r.Authorities, id, name)
	r.Additionals = d.filter(r.Additionals, id, name)
	// Every A in the answer counts, whatever its owner name: a CNAME chain
	// ends at a name the policy never mentions, and it is the question that
	// was allowed.
	for _, rr := range r.Answers {
		if a, ok := rr.Body.(*dnsmessage.AResource); ok {
			d.Enforcer.record(src, name, netip.AddrFrom4(a.A), time.Duration(rr.Header.TTL)*time.Second)
		}
	}
	out, err := r.Pack()
	if err != nil {
		return reply(m, dnsmessage.RCodeServerFailure)
	}
	if udp && len(out) > udpSize(m) {
		// Too big for the guest's buffer: truncated, with nothing in it, so
		// the client asks again over TCP.
		t := dnsmessage.Message{Header: r.Header, Questions: r.Questions}
		t.Header.Truncated = true
		out, _ = t.Pack()
	}
	return out
}

// typeIXFR is an incremental zone transfer, which dnsmessage has no name for.
const typeIXFR dnsmessage.Type = 251

// filter drops the records a restricted VM must not be handed: IPv6, SVCB and
// HTTPS (see answer), and private addresses.
func (d *DNS) filter(rrs []dnsmessage.Resource, id, name string) []dnsmessage.Resource {
	out := rrs[:0]
	for _, rr := range rrs {
		switch b := rr.Body.(type) {
		case *dnsmessage.AAAAResource, *dnsmessage.HTTPSResource, *dnsmessage.SVCBResource:
			continue
		case *dnsmessage.AResource:
			if a := netip.AddrFrom4(b.A); d.Blocked(a) {
				d.Log.Warn("egress dns answer stripped", "vm", id, "domain", name, "addr", a, "reason", "non-public address")
				continue
			}
		case *dnsmessage.UnknownResource:
			if b.Type == dnsmessage.TypeHTTPS || b.Type == dnsmessage.TypeSVCB {
				continue
			}
		}
		out = append(out, rr)
	}
	return out
}

// reply is an answer to q with no records and rcode.
func reply(q dnsmessage.Message, rcode dnsmessage.RCode) []byte {
	m := dnsmessage.Message{
		Header: dnsmessage.Header{ID: q.Header.ID, Response: true, OpCode: q.Header.OpCode,
			RecursionDesired: q.Header.RecursionDesired, RecursionAvailable: true, RCode: rcode},
		Questions: q.Questions,
	}
	b, err := m.Pack()
	if err != nil {
		return nil
	}
	return b
}

// udpSize is the largest UDP answer q's sender takes: its EDNS buffer, or 512.
func udpSize(q dnsmessage.Message) int {
	for _, rr := range q.Additionals {
		if rr.Header.Type == dnsmessage.TypeOPT && int(rr.Header.Class) > 512 {
			return int(rr.Header.Class)
		}
	}
	return 512
}

// exchangeUDP asks each upstream in turn and returns the first answer.
func (d *DNS) exchangeUDP(q []byte) ([]byte, error) {
	err := errors.New("no upstream resolvers configured")
	for _, up := range d.Upstreams {
		var c net.Conn
		if c, err = net.DialTimeout("udp", up, d.timeout()); err != nil {
			continue
		}
		c.SetDeadline(time.Now().Add(d.timeout()))
		if _, err = c.Write(q); err == nil {
			buf := make([]byte, maxDNSMessage)
			var n int
			// Skip anything that isn't the answer to this query's ID.
			for {
				if n, err = c.Read(buf); err != nil || n < 2 || len(q) < 2 || (buf[0] == q[0] && buf[1] == q[1]) {
					break
				}
			}
			if err == nil && n >= 2 {
				c.Close()
				return buf[:n], nil
			}
		}
		c.Close()
	}
	return nil, err
}

func (d *DNS) exchangeTCP(q []byte) ([]byte, error) {
	err := errors.New("no upstream resolvers configured")
	for _, up := range d.Upstreams {
		var c net.Conn
		if c, err = net.DialTimeout("tcp", up, d.timeout()); err != nil {
			continue
		}
		c.SetDeadline(time.Now().Add(d.timeout()))
		var resp []byte
		if err = writeTCPMessage(c, q); err == nil {
			resp, err = readTCPMessage(c)
		}
		c.Close()
		if err == nil {
			return resp, nil
		}
	}
	return nil, err
}

func readTCPMessage(r io.Reader) ([]byte, error) {
	var l [2]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return nil, err
	}
	msg := make([]byte, binary.BigEndian.Uint16(l[:]))
	_, err := io.ReadFull(r, msg)
	return msg, err
}

func writeTCPMessage(w io.Writer, msg []byte) error {
	if len(msg) > maxDNSMessage {
		return errors.New("dns message too long")
	}
	b := make([]byte, 2+len(msg))
	binary.BigEndian.PutUint16(b, uint16(len(msg)))
	copy(b[2:], msg)
	_, err := w.Write(b)
	return err
}
