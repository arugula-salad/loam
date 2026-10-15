package network

import (
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

// fakeBroker records the connections it is handed and what they replay.
type fakeBroker struct {
	mu    sync.Mutex
	names []string
	hello []byte
	pol   *Policy
}

func (b *fakeBroker) Serve(c net.Conn, vm, name string, policy func() *Policy) {
	buf := make([]byte, 4096)
	c.SetReadDeadline(time.Now().Add(time.Second))
	n, _ := c.Read(buf)
	b.mu.Lock()
	b.names, b.hello, b.pol = append(b.names, vm+" "+name), buf[:n], policy()
	b.mu.Unlock()
	c.Write([]byte("brokered"))
}

// A secret's host on 443 goes to the broker, with the ClientHello replayed,
// whether the VM is restricted or not; other names don't.
func TestProxyHandsSecretHostsToTheBroker(t *testing.T) {
	for _, egress := range [][]api.Rule{nil, {allow("github.com")}} {
		pol := nodeapi.Policy{Egress: egress, Secrets: []nodeapi.PolicySecret{secret("GH", "api.github.com")}}
		h := newProxy(t, pol, "140.82.112.5:443")
		b := &fakeBroker{}
		h.p.Broker = b
		hello := goHello(t, "API.github.com")
		if got, err := roundTrip(t, h.addr, hello); err != nil || got != "brokered" {
			t.Errorf("restricted %v: %q %v", egress != nil, got, err)
		}
		b.mu.Lock()
		if len(b.names) != 1 || b.names[0] != "sb-a api.github.com" || string(b.hello) != string(hello) || len(b.pol.Secrets()) != 1 {
			t.Errorf("broker got %v, %d bytes of hello", b.names, len(b.hello))
		}
		b.mu.Unlock()
		if d := h.dials(); len(d) != 0 {
			t.Errorf("dialed %v", d)
		}
		h.mu.Lock()
		h.names["github.com"] = []netip.Addr{netip.MustParseAddr("140.82.112.5")}
		h.mu.Unlock()
		other := goHello(t, "github.com")
		if got, _ := roundTrip(t, h.addr, other); got != "echo:"+string(other) {
			t.Errorf("a name without secrets went to the broker: %q", got)
		}
	}
}
