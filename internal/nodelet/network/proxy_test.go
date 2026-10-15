package network

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

// echoServer answers each connection with "echo:" + everything it read, which
// it can only do after seeing EOF: the reply arriving proves half-close works.
func echoServer(t *testing.T) (addr string, accepted *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	accepted = new(atomic.Int32)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() {
				defer c.Close()
				b, _ := io.ReadAll(c)
				c.Write(append([]byte("echo:"), b...))
			}()
		}
	}()
	return ln.Addr().String(), accepted
}

// proxyHarness is a proxy whose VM is 127.0.0.1 (the test's clients), whose
// "original destination" is dst, whose resolver answers from names, and
// whose dialer sends every connection to the echo server, recording where
// it was meant to go.
type proxyHarness struct {
	e     *Enforcer
	p     *Proxy
	addr  string
	dst   netip.AddrPort
	names map[string][]netip.Addr

	mu     sync.Mutex
	dialed []string
}

func newProxy(t *testing.T, pol nodeapi.Policy, dst string) *proxyHarness {
	t.Helper()
	echo, _ := echoServer(t)
	h := &proxyHarness{e: NewEnforcer(quiet), dst: netip.MustParseAddrPort(dst), names: map[string][]netip.Addr{}}
	h.e.Set(loopback, "sb-a", mustCompile(t, pol))
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	h.addr = ln.Addr().String()
	h.p = &Proxy{Enforcer: h.e, Log: quiet, Blocked: NonPublic,
		OrigDst: func(*net.TCPConn) (netip.AddrPort, error) { h.mu.Lock(); defer h.mu.Unlock(); return h.dst, nil },
		Resolve: func(_ context.Context, name string) ([]netip.Addr, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if a, ok := h.names[name]; ok {
				return a, nil
			}
			return nil, errors.New("no such host")
		},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			h.mu.Lock()
			h.dialed = append(h.dialed, addr)
			h.mu.Unlock()
			return (&net.Dialer{}).DialContext(ctx, network, echo)
		},
	}
	go h.p.Serve(ln)
	return h
}

func (h *proxyHarness) setDst(dst string) {
	h.mu.Lock()
	h.dst = netip.MustParseAddrPort(dst)
	h.mu.Unlock()
}

func (h *proxyHarness) dials() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.dialed...)
}

// roundTrip sends msg, half-closes, and returns whatever comes back. A dial
// error is returned, not fatal: a refusing proxy resets at once, and on
// loopback that RST can land before the dialer has collected its connect.
func roundTrip(t *testing.T, proxy string, msg []byte) (string, error) {
	t.Helper()
	c, err := net.DialTimeout("tcp4", proxy, 2*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(msg); err != nil {
		return "", err
	}
	c.(*net.TCPConn).CloseWrite()
	b, err := io.ReadAll(c)
	return string(b), err
}

func refused(t *testing.T, got string, err error) {
	t.Helper()
	if err == nil && got != "" {
		t.Errorf("not refused: %q", got)
	}
}

var gitPolicy = nodeapi.Policy{Egress: []api.Rule{allow("github.com"), allow("git.example.com", 22), allow("*.example.org", 443)}}

// On a port without a name, an address the VM resolved from a name allowed there.
func TestProxyOtherPortNeedsALearntAddress(t *testing.T) {
	h := newProxy(t, gitPolicy, "203.0.113.7:22")
	got, err := roundTrip(t, h.addr, []byte("SSH-2.0"))
	refused(t, got, err)
	h.e.record(loopback, "git.example.com", netip.MustParseAddr("203.0.113.7"), time.Minute)
	big := strings.Repeat("x", 1<<20) // more than any socket buffer, so both directions really stream
	if got, err := roundTrip(t, h.addr, []byte(big)); err != nil || got != "echo:"+big {
		t.Fatalf("got %d bytes, %v", len(got), err)
	}
	if d := h.dials(); len(d) != 1 || d[0] != "203.0.113.7:22" {
		t.Errorf("dialed %v", d)
	}
}

func TestProxyOtherPortRefusesANameNotAllowedThere(t *testing.T) {
	h := newProxy(t, gitPolicy, "140.82.112.3:22")
	h.e.record(loopback, "github.com", netip.MustParseAddr("140.82.112.3"), time.Minute)
	got, err := roundTrip(t, h.addr, []byte("SSH-2.0"))
	refused(t, got, err)
}

func TestProxyOtherPortRefusesAnExpiredAddress(t *testing.T) {
	h := newProxy(t, gitPolicy, "203.0.113.7:22")
	clock := fakeClock(h.e)
	h.e.record(loopback, "git.example.com", netip.MustParseAddr("203.0.113.7"), time.Second)
	clock(time.Second + ttlGrace + time.Second)
	got, err := roundTrip(t, h.addr, []byte("SSH-2.0"))
	refused(t, got, err)
}

// On 443 the name is the ClientHello's, and the proxy dials an address it
// resolves for it, the one the guest aimed at when that is among them. The
// hello reaches the server byte for byte.
func TestProxyTLSDialsTheNameItResolves(t *testing.T) {
	h := newProxy(t, gitPolicy, "198.51.100.1:443")
	h.names["github.com"] = []netip.Addr{netip.MustParseAddr("140.82.112.3"), netip.MustParseAddr("140.82.112.4")}
	hello := goHello(t, "github.com")
	got, err := roundTrip(t, h.addr, hello)
	if err != nil || got != "echo:"+string(hello) {
		t.Fatalf("%q %v", got, err)
	}
	if d := h.dials(); len(d) != 1 || d[0] != "140.82.112.3:443" {
		t.Errorf("dialed %v (not where the guest aimed: it isn't the name's)", d)
	}
	h.setDst("140.82.112.4:443")
	roundTrip(t, h.addr, hello)
	if d := h.dials(); len(d) != 2 || d[1] != "140.82.112.4:443" {
		t.Errorf("dialed %v (where the guest aimed: it is one of the name's)", d)
	}
}

// A ClientHello with the ECH extension, which Chrome sends keyless on every
// connection, is judged by its outer name like any other.
func TestProxyTLSGreaseECH(t *testing.T) {
	h := newProxy(t, gitPolicy, "140.82.112.3:443")
	h.names["github.com"] = []netip.Addr{netip.MustParseAddr("140.82.112.3")}
	hello := handHello(sniExt("github.com"), ext(0xfe0d, []byte{0}))
	if got, err := roundTrip(t, h.addr, hello); err != nil || got != "echo:"+string(hello) {
		t.Fatalf("allowed outer name: %q %v", got, err)
	}
	got, err := roundTrip(t, h.addr, handHello(sniExt("gist.github.com"), ext(0xfe0d, []byte{0})))
	refused(t, got, err)
}

func TestProxyTLSRefusals(t *testing.T) {
	h := newProxy(t, gitPolicy, "140.82.112.3:443")
	h.names["github.com"] = []netip.Addr{netip.MustParseAddr("140.82.112.3")}
	h.names["private.example.org"] = []netip.Addr{netip.MustParseAddr("10.43.0.1")}
	for name, hello := range map[string][]byte{
		"a name not allowed":     goHello(t, "gist.github.com"),
		"no server name":         handHello(),
		"not TLS":                []byte("GET / HTTP/1.1\r\nHost: github.com\r\n\r\n"),
		"an address as the name": handHello(sniExt("140.82.112.3")),
		"a private answer":       goHello(t, "private.example.org"),
	} {
		got, err := roundTrip(t, h.addr, hello)
		if err == nil && got != "" {
			t.Errorf("%s: not refused: %q", name, got)
		}
	}
	if d := h.dials(); len(d) != 0 {
		t.Errorf("dialed %v", d)
	}
}

// On 80 the name is the Host header's; a refusal is a 403 in the API's shape.
func TestProxyHTTP(t *testing.T) {
	h := newProxy(t, gitPolicy, "198.51.100.1:80")
	h.names["github.com"] = []netip.Addr{netip.MustParseAddr("140.82.112.3")}
	req := "GET /x HTTP/1.1\r\nHost: github.com\r\n\r\nbody"
	if got, err := roundTrip(t, h.addr, []byte(req)); err != nil || got != "echo:"+req {
		t.Errorf("%q %v", got, err)
	}
	if d := h.dials(); len(d) != 1 || d[0] != "140.82.112.3:80" {
		t.Errorf("dialed %v", d)
	}
	got, err := roundTrip(t, h.addr, []byte("GET / HTTP/1.1\r\nHost: gist.github.com\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(got)), nil)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("%v %v", resp, err)
	}
	var e api.Error
	if json.NewDecoder(resp.Body).Decode(&e) != nil || e.Error.Code != CodeEgressDenied || !strings.Contains(e.Error.Message, "gist.github.com") {
		t.Errorf("body %+v", e)
	}
}

// An unrestricted VM here for its secrets has its 443 go where it was going,
// whatever it sends.
func TestProxyUnrestrictedGoesToTheOriginalDestination(t *testing.T) {
	h := newProxy(t, nodeapi.Policy{Secrets: []nodeapi.PolicySecret{secret("GH", "api.github.com")}}, "198.51.100.1:443")
	for _, msg := range [][]byte{goHello(t, "example.com"), handHello(), []byte("not tls at all")} {
		if got, err := roundTrip(t, h.addr, msg); err != nil || got != "echo:"+string(msg) {
			t.Errorf("%q %v", got, err)
		}
	}
	if d := h.dials(); len(d) != 3 || d[0] != "198.51.100.1:443" {
		t.Errorf("dialed %v", d)
	}
	// But never to a private address.
	h.setDst("10.43.0.1:443")
	got, err := roundTrip(t, h.addr, goHello(t, "example.com"))
	refused(t, got, err)
}

func TestProxyUnknownSourceIsRefused(t *testing.T) {
	h := newProxy(t, nodeapi.Policy{}, "198.51.100.1:443")
	h.e.Remove(loopback)
	got, err := roundTrip(t, h.addr, goHello(t, "example.com"))
	refused(t, got, err)
}

// A connection whose name the new policy no longer allows is closed; one it
// still allows carries on.
func TestProxyPolicyChangeClosesConnections(t *testing.T) {
	h := newProxy(t, gitPolicy, "203.0.113.7:22")
	h.e.record(loopback, "git.example.com", netip.MustParseAddr("203.0.113.7"), time.Minute)
	c, err := net.Dial("tcp4", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("hello"))
	waitFor(t, "the flow", func() bool { return h.flows() == 1 })
	h.e.Set(loopback, "sb-a", mustCompile(t, nodeapi.Policy{Egress: []api.Rule{allow("git.example.com", 22), allow("github.com")}}))
	if h.flows() != 1 {
		t.Error("a connection the new policy allows was closed")
	}
	h.e.Set(loopback, "sb-a", mustCompile(t, nodeapi.Policy{Egress: []api.Rule{allow("github.com")}}))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadAll(c); errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("the connection was not closed")
	}
	waitFor(t, "the flow to go", func() bool { return h.flows() == 0 })
}

// A VM that stops running loses its connections.
func TestProxyRemoveClosesConnections(t *testing.T) {
	h := newProxy(t, nodeapi.Policy{}, "198.51.100.1:443")
	c, err := net.Dial("tcp4", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write(goHello(t, "example.com"))
	waitFor(t, "the flow", func() bool { return h.flows() == 1 })
	h.e.Remove(loopback)
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var buf [1]byte
	if _, err := c.Read(buf[:]); err == nil {
		t.Error("read after the VM was removed")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Error("the connection was not closed")
	}
}

func (h *proxyHarness) flows() int {
	h.e.mu.Lock()
	defer h.e.mu.Unlock()
	if vs := h.e.vms[loopback]; vs != nil {
		return len(vs.flows)
	}
	return 0
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// Something that dialed the proxy's port itself has no original destination.
func TestOriginalDstWithoutRedirect(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, _ := net.Dial("tcp4", ln.Addr().String())
		if c != nil {
			time.Sleep(100 * time.Millisecond)
			c.Close()
		}
	}()
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Without conntrack there is no entry; with it, the destination is the
	// listener itself, a blocked address. Either way it goes nowhere.
	if dst, err := OriginalDst(c.(*net.TCPConn)); err == nil && !NonPublic(dst.Addr()) {
		t.Errorf("original destination %v", dst)
	}
}

func TestNonPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"10.240.0.1": true, "10.43.0.10": true, "100.100.100.100": true, "169.254.169.254": true,
		"172.16.5.4": true, "192.168.1.1": true, "127.0.0.1": true, "224.0.0.1": true, "::1": true,
		"140.82.112.3": false, "1.1.1.1": false, "::ffff:10.0.0.1": true,
	} {
		if got := NonPublic(netip.MustParseAddr(addr)); got != want {
			t.Errorf("NonPublic(%s) = %v", addr, got)
		}
	}
}

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{
		"github.com": true, "GitHub.com.": true, "a_b.example.org": true, "localhost": true,
		"": false, "1.2.3.4": false, "a..b": false, "a b.com": false, "x.com:443": false, "[::1]": false,
	} {
		if got := validName(name); got != want {
			t.Errorf("validName(%q) = %v", name, got)
		}
	}
}
