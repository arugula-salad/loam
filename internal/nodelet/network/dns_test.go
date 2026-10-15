package network

import (
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

// fakeUpstream is a resolver that answers from a table, over UDP and TCP,
// and counts what it was asked.
type fakeUpstream struct {
	addr string
	mu   sync.Mutex
	// records by question name (with the trailing dot) and type.
	records map[string][]dnsmessage.Resource
	asked   []string
	// truncate answers over UDP with TC set.
	truncate bool
}

func newUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	pc, ln, err := listenUDPAndTCP("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close(); ln.Close() })
	u := &fakeUpstream{addr: pc.LocalAddr().String(), records: map[string][]dnsmessage.Resource{}}
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(u.answer(buf[:n], true), from)
		}
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				q, err := readTCPMessage(c)
				if err == nil {
					writeTCPMessage(c, u.answer(q, false))
				}
			}()
		}
	}()
	return u
}

func (u *fakeUpstream) answer(q []byte, udp bool) []byte {
	var m dnsmessage.Message
	if m.Unpack(q) != nil || len(m.Questions) != 1 {
		return nil
	}
	qn := m.Questions[0]
	u.mu.Lock()
	u.asked = append(u.asked, qn.Name.String()+" "+qn.Type.String())
	rrs := u.records[qn.Name.String()]
	trunc := u.truncate && udp
	u.mu.Unlock()
	r := dnsmessage.Message{Header: dnsmessage.Header{ID: m.Header.ID, Response: true, RecursionAvailable: true}, Questions: m.Questions}
	if trunc {
		r.Header.Truncated = true
	} else {
		for _, rr := range rrs {
			if rr.Header.Type == qn.Type || rr.Header.Type == dnsmessage.TypeCNAME {
				r.Answers = append(r.Answers, rr)
			}
		}
	}
	b, _ := r.Pack()
	return b
}

func (u *fakeUpstream) add(name string, rr ...dnsmessage.Resource) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.records[name] = append(u.records[name], rr...)
}

func (u *fakeUpstream) questions() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.asked...)
}

func aRR(name, addr string, ttl uint32) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: ttl},
		Body:   &dnsmessage.AResource{A: netip.MustParseAddr(addr).As4()},
	}
}

func cnameRR(name, target string) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET, TTL: 300},
		Body:   &dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName(target)},
	}
}

// dnsHarness is a forwarder with the VM at 127.0.0.1 under policy, and its upstream.
type dnsHarness struct {
	e  *Enforcer
	up *fakeUpstream
	d  *DNS
}

func newDNS(t *testing.T, p nodeapi.Policy) *dnsHarness {
	t.Helper()
	h := &dnsHarness{e: NewEnforcer(quiet), up: newUpstream(t)}
	h.e.Set(loopback, "sb-a", mustCompile(t, p))
	h.d = &DNS{Enforcer: h.e, Upstreams: []string{h.up.addr}, Log: quiet, Blocked: NonPublic, Timeout: time.Second}
	return h
}

// ask sends a query from 127.0.0.1 (as the VM) and returns the answer.
func (h *dnsHarness) ask(t *testing.T, name string, typ dnsmessage.Type, udp bool) dnsmessage.Message {
	t.Helper()
	q := dnsmessage.Message{Header: dnsmessage.Header{ID: 0x4242, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET}}}
	b, _ := q.Pack()
	resp := h.d.answer(loopback, b, udp)
	var m dnsmessage.Message
	if err := m.Unpack(resp); err != nil {
		t.Fatalf("answer to %s: %v", name, err)
	}
	if m.Header.ID != 0x4242 || !m.Header.Response {
		t.Errorf("answer header %+v", m.Header)
	}
	return m
}

func addrs(m dnsmessage.Message) []string {
	var out []string
	for _, rr := range m.Answers {
		if a, ok := rr.Body.(*dnsmessage.AResource); ok {
			out = append(out, netip.AddrFrom4(a.A).String())
		}
	}
	return out
}

var restrictedGitHub = nodeapi.Policy{Egress: []api.Rule{allow("github.com"), allow("git.example.com", 22)}}

func TestDNSAllowedNameIsAnsweredAndRemembered(t *testing.T) {
	h := newDNS(t, restrictedGitHub)
	h.up.add("github.com.", aRR("github.com.", "140.82.112.3", 60))
	m := h.ask(t, "github.com.", dnsmessage.TypeA, true)
	if m.Header.RCode != dnsmessage.RCodeSuccess || strings.Join(addrs(m), ",") != "140.82.112.3" {
		t.Fatalf("%v %v", m.Header.RCode, addrs(m))
	}
	gh := netip.MustParseAddr("140.82.112.3")
	if via := h.e.learnt(loopback, gh, 443); via != "github.com" {
		t.Errorf("learnt on 443: %q", via)
	}
	// Learnt, but the name isn't allowed on 22.
	if via := h.e.learnt(loopback, gh, 22); via != "" {
		t.Errorf("learnt on 22: %q", via)
	}
}

func TestDNSRefusesNamesNotAllowedWithoutAskingUpstream(t *testing.T) {
	h := newDNS(t, restrictedGitHub)
	if m := h.ask(t, "evil.example.org.", dnsmessage.TypeA, true); m.Header.RCode != dnsmessage.RCodeRefused || len(m.Answers) != 0 {
		t.Errorf("%v", m.Header.RCode)
	}
	if q := h.up.questions(); len(q) != 0 {
		t.Errorf("upstream was asked %v", q)
	}
}

func TestDNSUnknownSourceIsRefused(t *testing.T) {
	h := newDNS(t, nodeapi.Policy{})
	h.e.Remove(loopback)
	if m := h.ask(t, "github.com.", dnsmessage.TypeA, true); m.Header.RCode != dnsmessage.RCodeRefused {
		t.Errorf("%v", m.Header.RCode)
	}
}

// AAAA, HTTPS and SVCB are answered empty, ANY is refused, none of them asked upstream.
func TestDNSTypesARestrictedVMDoesNotGet(t *testing.T) {
	h := newDNS(t, restrictedGitHub)
	for _, typ := range []dnsmessage.Type{dnsmessage.TypeAAAA, dnsmessage.TypeHTTPS, dnsmessage.TypeSVCB} {
		if m := h.ask(t, "github.com.", typ, true); m.Header.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 0 {
			t.Errorf("%v: %v %d", typ, m.Header.RCode, len(m.Answers))
		}
	}
	if m := h.ask(t, "github.com.", dnsmessage.TypeALL, true); m.Header.RCode != dnsmessage.RCodeRefused {
		t.Errorf("ANY: %v", m.Header.RCode)
	}
	if q := h.up.questions(); len(q) != 0 {
		t.Errorf("upstream was asked %v", q)
	}
}

// DNS rebinding: an allowed name answered with a private address loses it.
func TestDNSPrivateAnswersAreStripped(t *testing.T) {
	h := newDNS(t, restrictedGitHub)
	h.up.add("github.com.", aRR("github.com.", "10.43.0.10", 60), aRR("github.com.", "169.254.169.254", 60), aRR("github.com.", "140.82.112.3", 60))
	m := h.ask(t, "github.com.", dnsmessage.TypeA, true)
	if got := strings.Join(addrs(m), ","); got != "140.82.112.3" {
		t.Errorf("answers %s", got)
	}
	if via := h.e.learnt(loopback, netip.MustParseAddr("10.43.0.10"), 443); via != "" {
		t.Error("a private answer was learnt")
	}
}

// A CNAME chain ends at a name the policy never mentions; its addresses are
// learnt under the name asked.
func TestDNSCNAMEChainIsLearntUnderTheQuestion(t *testing.T) {
	h := newDNS(t, restrictedGitHub)
	h.up.add("github.com.", cnameRR("github.com.", "lb.cdn.example."), aRR("lb.cdn.example.", "140.82.112.4", 60))
	m := h.ask(t, "github.com.", dnsmessage.TypeA, true)
	if len(m.Answers) != 2 {
		t.Fatalf("answers %+v", m.Answers)
	}
	if via := h.e.learnt(loopback, netip.MustParseAddr("140.82.112.4"), 443); via != "github.com" {
		t.Errorf("learnt %q", via)
	}
}

func TestDNSLearntAddressesExpire(t *testing.T) {
	h := newDNS(t, restrictedGitHub)
	clock := fakeClock(h.e)
	h.up.add("git.example.com.", aRR("git.example.com.", "203.0.113.7", 30))
	h.ask(t, "git.example.com.", dnsmessage.TypeA, true)
	dst := netip.MustParseAddr("203.0.113.7")
	if h.e.learnt(loopback, dst, 22) != "git.example.com" {
		t.Fatal("not learnt")
	}
	clock(30*time.Second + ttlGrace - time.Second)
	if h.e.learnt(loopback, dst, 22) == "" {
		t.Error("expired within its grace")
	}
	clock(2 * time.Second)
	if h.e.learnt(loopback, dst, 22) != "" {
		t.Error("still learnt past its TTL and grace")
	}
}

// What one VM learnt is its own.
func TestDNSLearntIsPerVM(t *testing.T) {
	h := newDNS(t, restrictedGitHub)
	other := netip.MustParseAddr("10.240.0.9")
	h.e.Set(other, "sb-b", mustCompile(t, restrictedGitHub))
	h.up.add("github.com.", aRR("github.com.", "140.82.112.3", 60))
	h.ask(t, "github.com.", dnsmessage.TypeA, true)
	if h.e.learnt(other, netip.MustParseAddr("140.82.112.3"), 443) != "" {
		t.Error("another VM learnt it too")
	}
}

func TestDNSPolicyChangeForgetsWhatItNoLongerAllows(t *testing.T) {
	h := newDNS(t, restrictedGitHub)
	h.up.add("git.example.com.", aRR("git.example.com.", "203.0.113.7", 300))
	h.up.add("github.com.", aRR("github.com.", "140.82.112.3", 300))
	h.ask(t, "git.example.com.", dnsmessage.TypeA, true)
	h.ask(t, "github.com.", dnsmessage.TypeA, true)
	h.e.Set(loopback, "sb-a", mustCompile(t, nodeapi.Policy{Egress: []api.Rule{allow("github.com")}}))
	if h.e.learnt(loopback, netip.MustParseAddr("203.0.113.7"), 22) != "" {
		t.Error("an address learnt through a name no longer allowed is still learnt")
	}
	if h.e.learnt(loopback, netip.MustParseAddr("140.82.112.3"), 443) != "github.com" {
		t.Error("an address learnt through a name still allowed was forgotten")
	}
}

func TestDNSUpstreamFailureIsServfail(t *testing.T) {
	h := newDNS(t, restrictedGitHub)
	dead, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	h.d.Upstreams = []string{dead.LocalAddr().String()}
	dead.Close()
	h.d.Timeout = 200 * time.Millisecond
	if m := h.ask(t, "github.com.", dnsmessage.TypeA, true); m.Header.RCode != dnsmessage.RCodeServerFailure {
		t.Errorf("%v", m.Header.RCode)
	}
}

// A truncated upstream answer is asked again over TCP; an answer too big for
// the guest's UDP buffer goes back truncated, for it to ask over TCP.
func TestDNSTruncation(t *testing.T) {
	h := newDNS(t, restrictedGitHub)
	var many []dnsmessage.Resource
	for i := range 40 {
		many = append(many, aRR("github.com.", netip.AddrFrom4([4]byte{140, 82, 113, byte(i + 1)}).String(), 60))
	}
	h.up.add("github.com.", many...)
	h.up.mu.Lock()
	h.up.truncate = true
	h.up.mu.Unlock()
	m := h.ask(t, "github.com.", dnsmessage.TypeA, true)
	if !m.Header.Truncated || len(m.Answers) != 0 {
		t.Errorf("over UDP: truncated %v, %d answers", m.Header.Truncated, len(m.Answers))
	}
	// It was still learnt: the upstream answered in full over TCP.
	if h.e.learnt(loopback, netip.MustParseAddr("140.82.113.40"), 443) != "github.com" {
		t.Error("not learnt")
	}
	if m := h.ask(t, "github.com.", dnsmessage.TypeA, false); len(m.Answers) != 40 {
		t.Errorf("over TCP: %d answers", len(m.Answers))
	}
}

// An unrestricted VM's queries are relayed as they are, answers too.
func TestDNSUnrestrictedIsRelayed(t *testing.T) {
	h := newDNS(t, nodeapi.Policy{})
	h.up.add("anything.example.org.", aRR("anything.example.org.", "10.1.2.3", 60))
	m := h.ask(t, "anything.example.org.", dnsmessage.TypeA, true)
	if got := strings.Join(addrs(m), ","); got != "10.1.2.3" {
		t.Errorf("answers %s", got)
	}
}

// The forwarder over the wire, as a restricted VM uses it.
func TestDNSOverTheWire(t *testing.T) {
	h := newDNS(t, restrictedGitHub)
	h.up.add("github.com.", aRR("github.com.", "140.82.112.3", 60))
	bound, stop, err := h.d.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	q := dnsmessage.Message{Header: dnsmessage.Header{ID: 7, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("github.com."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
	b, _ := q.Pack()
	for _, network := range []string{"udp", "tcp"} {
		c, err := net.Dial(network, bound)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(3 * time.Second))
		var resp []byte
		if network == "udp" {
			c.Write(b)
			buf := make([]byte, 512)
			n, _ := c.Read(buf)
			resp = buf[:n]
		} else {
			writeTCPMessage(c, b)
			resp, _ = readTCPMessage(c)
		}
		c.Close()
		var m dnsmessage.Message
		if err := m.Unpack(resp); err != nil || strings.Join(addrs(m), ",") != "140.82.112.3" {
			t.Errorf("%s: %v %v", network, addrs(m), err)
		}
	}
}

// fakeClock makes e's clock one that moves only when the returned function
// says, by that much.
func fakeClock(e *Enforcer) func(time.Duration) {
	var mu sync.Mutex
	now := time.Now()
	e.mu.Lock()
	e.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	e.mu.Unlock()
	return func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
}
