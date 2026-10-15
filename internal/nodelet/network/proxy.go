package network

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/arugula-salad/loam/pkg/api"
)

// Proxy is the transparent TCP proxy on the gateway (docs/NODE.md, "Phase 3:
// the network boundary"): every TCP connection of a restricted VM, and the
// 443 of a VM with secrets, is redirected here. On 80 and 443 it reads the
// name the connection is for (the Host header, the ClientHello's SNI),
// checks it, and dials an address of that name it resolves itself, so a name
// that shares an allowed name's address stays out. On any other port it
// admits only an address the VM's allowed lookups produced.
type Proxy struct {
	Enforcer *Enforcer
	Log      *slog.Logger
	// OrigDst recovers where the VM was connecting before the kernel
	// redirected it here.
	OrigDst func(*net.TCPConn) (netip.AddrPort, error)
	// Blocked is checked for every address the proxy dials, whatever the
	// policy. Its connections leave from the pod, so the forward chain that
	// keeps VMs out of private ranges never sees them: this check is all
	// there is.
	Blocked func(netip.Addr) bool
	// Resolve returns a name's IPv4 addresses, asked of the public resolvers,
	// never the pod's (which would answer cluster names).
	Resolve func(ctx context.Context, name string) ([]netip.Addr, error)
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	// Broker, when the nodelet has the broker CA, takes connections to a
	// secret's host on 443.
	Broker Broker
}

// Broker ends the TLS of a VM's connection to name, one of its secrets'
// hosts, and brokers the requests on it until the connection ends. c replays
// the ClientHello the proxy read. policy is the VM's current policy, nil
// once it no longer runs.
type Broker interface {
	Serve(c net.Conn, vm, name string, policy func() *Policy)
}

// NewProxy is a proxy that resolves names with upstreams (host:port).
func NewProxy(e *Enforcer, upstreams []string, log *slog.Logger) *Proxy {
	d := &net.Dialer{Timeout: 10 * time.Second}
	return &Proxy{Enforcer: e, Log: log, OrigDst: OriginalDst, Blocked: Blocked,
		Resolve: Resolver(upstreams), Dial: d.DialContext}
}

// Resolver looks names up with the resolvers at upstreams, in turn.
func Resolver(upstreams []string) func(ctx context.Context, name string) ([]netip.Addr, error) {
	var mu sync.Mutex
	next := 0
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		mu.Lock()
		up := upstreams[next%len(upstreams)]
		next++
		mu.Unlock()
		return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, network, up)
	}}
	return func(ctx context.Context, name string) ([]netip.Addr, error) {
		// Fully qualified, so the pod's search domains are never tried.
		return r.LookupNetIP(ctx, "ip4", strings.TrimSuffix(name, ".")+".")
	}
}

// helloTimeout bounds how long a client has to send what the proxy reads
// before it decides: the ClientHello or the request head.
const helloTimeout = 10 * time.Second

// maxRequestHead bounds the HTTP request head the proxy reads on port 80.
const maxRequestHead = 16 << 10

func (p *Proxy) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			// Typically EMFILE. Back off instead of spinning; existing flows are unaffected.
			p.Log.Error("egress proxy accept failed", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go p.handle(c.(*net.TCPConn))
	}
}

// conn is one connection being decided.
type conn struct {
	p      *Proxy
	in     *net.TCPConn
	src    netip.Addr
	dst    netip.AddrPort
	vm     string
	policy *Policy
}

func (p *Proxy) handle(in *net.TCPConn) {
	defer in.Close()
	c := &conn{p: p, in: in, src: in.RemoteAddr().(*net.TCPAddr).AddrPort().Addr().Unmap()}
	var ok bool
	if c.vm, c.policy, ok = p.Enforcer.Lookup(c.src); !ok {
		c.refuse("", "source is not a running VM")
		return
	}
	dst, err := p.OrigDst(in)
	if err != nil {
		// Not a redirected connection: something dialed the proxy port directly.
		c.refuse("", "no original destination: "+err.Error())
		return
	}
	c.dst = dst
	if p.Enforcer.full(c.src) {
		c.refuse("", "too many concurrent connections")
		return
	}
	port := int(dst.Port())
	switch {
	case port == 443:
		c.tls()
	case port == 80 && c.policy.Restricted():
		c.http()
	case c.policy.Restricted():
		via := p.Enforcer.learnt(c.src, dst.Addr(), port)
		if via == "" {
			c.refuse("", "address was not resolved from a name allowed on this port")
			return
		}
		c.spliceTo(via, nil, nil)
	default:
		// Unrestricted, here for a moment while its policy changes.
		c.spliceTo("", nil, nil)
	}
}

// refuse ends the connection with a reset, so the guest sees a refusal rather
// than an empty reply.
func (c *conn) refuse(name, reason string) {
	c.p.Log.Warn("egress denied", "vm", c.vm, "src", c.src, "dst", c.dst, "domain", name, "reason", reason)
	c.in.SetLinger(0)
}

// tls decides a connection to port 443 by its ClientHello.
func (c *conn) tls() {
	c.in.SetReadDeadline(time.Now().Add(helloTimeout))
	hello, raw, err := readClientHello(c.in)
	c.in.SetReadDeadline(time.Time{})
	if !c.policy.Restricted() {
		// Here for its secrets: anything that isn't a brokered name goes
		// where it was going, as it would have without them.
		if err == nil && validName(hello.serverName) && c.brokers(hello.serverName) {
			c.broker(canonicalName(hello.serverName), raw)
			return
		}
		c.spliceTo("", raw, nil)
		return
	}
	switch {
	case err != nil:
		c.refuse("", "not a TLS ClientHello")
	case hello.serverName == "" || !validName(hello.serverName):
		c.refuse(hello.serverName, "no server name")
	case !c.policy.Allows(hello.serverName, 443):
		c.refuse(hello.serverName, "not allowed by the network policy")
	case c.brokers(hello.serverName):
		c.broker(canonicalName(hello.serverName), raw)
	default:
		name := canonicalName(hello.serverName)
		c.spliceTo(name, raw, &name)
	}
}

// brokers reports whether name is one of the VM's secrets' hosts, which the
// broker takes.
func (c *conn) brokers(name string) bool {
	return c.p.Broker != nil && c.policy.Brokers(name)
}

// broker hands the connection to the broker, tracked like any other so a
// policy change that no longer allows name closes it.
func (c *conn) broker(name string, hello []byte) {
	untrack, ok := c.p.Enforcer.track(c.src, c.vm, name, 443, c.in)
	if !ok {
		c.refuse(name, "the policy changed while connecting")
		return
	}
	defer untrack()
	src, vm := c.src, c.vm
	c.p.Broker.Serve(&replayConn{TCPConn: c.in, r: io.MultiReader(bytes.NewReader(hello), c.in)}, vm, name,
		func() *Policy { return c.p.Enforcer.Policy(src, vm) })
}

// replayConn is a connection whose first bytes, already read, are read again.
type replayConn struct {
	*net.TCPConn
	r io.Reader
}

func (c *replayConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// http decides a restricted VM's connection to port 80 by its request's Host.
func (c *conn) http() {
	var raw bytes.Buffer
	c.in.SetReadDeadline(time.Now().Add(helloTimeout))
	req, err := http.ReadRequest(bufio.NewReader(io.TeeReader(io.LimitReader(c.in, maxRequestHead), &raw)))
	c.in.SetReadDeadline(time.Time{})
	host := ""
	if err == nil {
		host = req.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
	}
	switch {
	case err != nil:
		c.refuse("", "not an HTTP request")
	case !validName(host):
		c.forbid(host, "no Host name")
	case !c.policy.Allows(host, 80):
		c.forbid(host, "not allowed by the network policy")
	default:
		name := canonicalName(host)
		c.spliceTo(name, raw.Bytes(), &name)
	}
}

// forbid answers a refused HTTP request with a 403 in the API's error shape
// (docs/API.md, "Network").
func (c *conn) forbid(host, reason string) {
	c.p.Log.Warn("egress denied", "vm", c.vm, "src", c.src, "dst", c.dst, "domain", host, "reason", reason)
	body, _ := json.Marshal(api.Error{Error: api.ErrorDetail{Code: CodeEgressDenied,
		Message: fmt.Sprintf("%s:80 is %s", host, reason)}})
	c.in.SetWriteDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c.in, "HTTP/1.1 403 Forbidden\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
}

// CodeEgressDenied is the error code of a refused request on port 80.
const CodeEgressDenied = "permission_denied"

// spliceTo dials onward, replays what the proxy read (head), and splices. A
// name means dial that name, resolved here; nil means the original
// destination. via is the name that authorised the connection, "" for none.
func (c *conn) spliceTo(via string, head []byte, name *string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var out net.Conn
	var err error
	if name != nil {
		out, err = c.p.dialName(ctx, *name, int(c.dst.Port()), c.dst.Addr())
	} else if c.p.Blocked(c.dst.Addr()) {
		c.refuse(via, "non-public or pod address")
		return
	} else {
		out, err = c.p.Dial(ctx, "tcp4", c.dst.String())
	}
	if err != nil {
		c.p.Log.Info("egress connect failed", "vm", c.vm, "dst", c.dst, "domain", via, "err", err)
		c.in.SetLinger(0)
		return
	}
	defer out.Close()
	untrack, ok := c.p.Enforcer.track(c.src, c.vm, via, int(c.dst.Port()), closers{c.in, out})
	if !ok {
		c.refuse(via, "the policy changed while connecting")
		return
	}
	defer untrack()
	if len(head) > 0 {
		if _, err := out.Write(head); err != nil {
			return
		}
	}
	splice(c.in, out)
}

// DialName connects to name on port at an address resolved here, never a
// blocked one.
func (p *Proxy) DialName(ctx context.Context, name string, port int) (net.Conn, error) {
	return p.dialName(ctx, name, port, netip.Addr{})
}

// dialName is DialName preferring prefer, where the guest aimed, when it is
// among the name's addresses: the guest may have picked one for a reason
// (the address it was answered, a connection it already has there).
func (p *Proxy) dialName(ctx context.Context, name string, port int, prefer netip.Addr) (net.Conn, error) {
	addrs, err := p.Resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	var ok []netip.Addr
	for _, a := range addrs {
		a = a.Unmap()
		if p.Blocked(a) {
			continue
		}
		if a == prefer {
			ok = append([]netip.Addr{a}, ok...)
		} else {
			ok = append(ok, a)
		}
	}
	if len(ok) == 0 {
		return nil, fmt.Errorf("%s has no public IPv4 address", name)
	}
	for _, a := range ok {
		var c net.Conn
		if c, err = p.Dial(ctx, "tcp4", netip.AddrPortFrom(a, uint16(port)).String()); err == nil {
			return c, nil
		}
	}
	return nil, err
}

// validName reports whether a server name or Host is a DNS name the policy
// can decide: not an address (a restricted sandbox reaches no address it
// didn't resolve), nothing but letters, digits, '-', '_' and dots.
func validName(name string) bool {
	name = strings.TrimSuffix(name, ".")
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			ch := label[i] | 0x20
			if !(ch >= 'a' && ch <= 'z' || label[i] >= '0' && label[i] <= '9' || label[i] == '-' || label[i] == '_') {
				return false
			}
		}
	}
	_, err := strconv.Atoi(name[strings.LastIndexByte(name, '.')+1:])
	return err != nil // no top-level domain is all digits: that is an address
}

type closers []io.Closer

func (c closers) Close() error {
	for _, x := range c {
		x.Close()
	}
	return nil
}

// splice copies both ways until both directions have ended. One side finishing
// its writes (FIN) is passed on as a half-close, not a teardown: the other
// direction may still have a response in flight.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	pipe := func(dst, src net.Conn) {
		defer wg.Done()
		_, err := io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok && err == nil {
			cw.CloseWrite()
			return
		}
		// A broken direction cannot be half-closed meaningfully; end the whole flow.
		dst.Close()
		src.Close()
	}
	wg.Add(2)
	go pipe(a, b)
	go pipe(b, a)
	wg.Wait()
}

// OriginalDst reads SO_ORIGINAL_DST, the pre-redirect destination conntrack
// keeps for a NATed connection.
func OriginalDst(c *net.TCPConn) (netip.AddrPort, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}
	var dst netip.AddrPort
	var serr error
	err = raw.Control(func(fd uintptr) {
		// There is no typed getsockopt for a sockaddr_in. IPv6Mreq is simply a
		// struct of at least that size whose leading bytes are addressable.
		var mreq *unix.IPv6Mreq
		if mreq, serr = unix.GetsockoptIPv6Mreq(int(fd), unix.SOL_IP, unix.SO_ORIGINAL_DST); serr != nil {
			return
		}
		b := mreq.Multiaddr // sockaddr_in: family[0:2] port[2:4] addr[4:8]
		dst = netip.AddrPortFrom(netip.AddrFrom4([4]byte(b[4:8])), uint16(b[2])<<8|uint16(b[3]))
	})
	if err == nil {
		err = serr
	}
	if err == nil && !dst.IsValid() {
		err = fmt.Errorf("invalid original destination")
	}
	return dst, err
}
