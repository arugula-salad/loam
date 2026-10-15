package network

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

func TestAddressing(t *testing.T) {
	cases := []struct {
		i              int
		addr, tap, mac string
	}{
		{2, "10.240.0.2", "lt2", "06:00:0a:f0:00:02"},
		{258, "10.240.1.2", "lt258", "06:00:0a:f0:01:02"},
		{MaxIndex, "10.240.255.254", "lt65534", "06:00:0a:f0:ff:fe"},
	}
	for _, c := range cases {
		if got := Addr(c.i).String(); got != c.addr {
			t.Errorf("Addr(%d) = %s", c.i, got)
		}
		if got := Tap(c.i); got != c.tap || len(got) > 15 {
			t.Errorf("Tap(%d) = %s", c.i, got)
		}
		if got := MAC(c.i); got != c.mac {
			t.Errorf("MAC(%d) = %s", c.i, got)
		}
	}
	if Prefix(7) != "10.240.0.7/16" {
		t.Errorf("Prefix(7) = %s", Prefix(7))
	}
	if i, err := FreeIndex(map[int]bool{2: true, 3: true, 5: true}); i != 4 || err != nil {
		t.Errorf("FreeIndex = %d %v", i, err)
	}
	full := map[int]bool{}
	for i := MinIndex; i <= MaxIndex; i++ {
		full[i] = true
	}
	if _, err := FreeIndex(full); !errors.Is(err, ErrFull) {
		t.Errorf("full: %v", err)
	}
}

// The rules keep VMs off everything but the public internet, and the
// nodelet's own ports.
func TestRuleset(t *testing.T) {
	r := Ruleset()
	for _, want := range []string{
		"table inet loam",
		"delete table inet loam",
		"10.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16", "224.0.0.0/4",
		`iifname "loam0" ip daddr @blocked4 drop`,
		`iifname "loam0" meta nfproto ipv6 drop`,
		`iifname "loam0" ip daddr 10.240.0.1 meta l4proto { tcp, udp } th dport 53 accept`,
		`iifname "loam0" drop comment "VMs may not talk to the nodelet"`,
		`oifname "loam0" drop`,
		`ip saddr 10.240.0.0/16 oifname != "loam0" masquerade`,
		// The proxy: restricted VMs' TCP, and the 443 of VMs with secrets.
		`iifname "loam0" ip daddr 10.240.0.1 return`,
		`iifname "loam0" ip saddr @restricted4 meta l4proto tcp redirect to :7880`,
		`iifname "loam0" ip saddr @https4 tcp dport 443 redirect to :7880`,
		`iifname "loam0" ip daddr 10.240.0.1 tcp dport 7880 ct status dnat accept`,
		`iifname "loam0" ip saddr @restricted4 drop`,
		// No VM sends from another's address.
		`table bridge loam`,
		`iifname "lt*" ether type ip iifname . ip saddr != @vmsrc drop`,
		`iifname "lt*" ether type arp iifname . arp saddr ip != @vmsrc drop`,
	} {
		if !strings.Contains(r, want) {
			t.Errorf("ruleset lacks %q", want)
		}
	}
	// A restricted VM's connection from before it was restricted is a reply
	// to nothing the proxy knows: it must not get past as one.
	fwd := r[strings.Index(r, "chain forward"):]
	if strings.Index(fwd, "@restricted4 drop") > strings.Index(fwd, "ct state established,related accept") {
		t.Error("established connections of a restricted VM are forwarded")
	}
}

// A VM's policy puts its address in one set or none, in one transaction.
func TestSetPolicy(t *testing.T) {
	var calls []call
	n := &Network{Enforcer: NewEnforcer(quiet), Run: func(_ context.Context, stdin, name string, args ...string) error {
		calls = append(calls, call{stdin, name + " " + strings.Join(args, " ")})
		return nil
	}}
	ctx := context.Background()
	restricted := mustCompile(t, nodeapi.Policy{Egress: []api.Rule{{Domain: "github.com", Action: "allow"}}})
	if err := n.SetPolicy(ctx, 7, "sb-a", restricted); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].args != "nft -f -" || !strings.HasSuffix(calls[0].stdin, "add element inet loam restricted4 { 10.240.0.7 }\n") ||
		!strings.Contains(calls[0].stdin, "delete element inet loam https4 { 10.240.0.7 }") {
		t.Errorf("restricted: %+v", calls)
	}
	if id, p, ok := n.Enforcer.Lookup(Addr(7)); !ok || id != "sb-a" || p != restricted {
		t.Errorf("enforcer: %s %v %v", id, p, ok)
	}
	calls = nil
	secrets := mustCompile(t, nodeapi.Policy{Secrets: []nodeapi.PolicySecret{{Name: "T", Value: "v", Hosts: []string{"api.github.com"}, Placeholder: "__loam_t__"}}})
	n.SetPolicy(ctx, 7, "sb-a", secrets)
	if !strings.HasSuffix(calls[0].stdin, "add element inet loam https4 { 10.240.0.7 }\n") {
		t.Errorf("secrets only: %q", calls[0].stdin)
	}
	calls = nil
	n.SetPolicy(ctx, 7, "sb-a", mustCompile(t, nodeapi.Policy{}))
	if !strings.HasSuffix(calls[0].stdin, "delete element inet loam https4 { 10.240.0.7 }\n") {
		t.Errorf("unrestricted: %q", calls[0].stdin)
	}
	// An nft failure leaves the old policy with the enforcer.
	n.Run = func(context.Context, string, string, ...string) error { return errors.New("nft: no") }
	if err := n.SetPolicy(ctx, 7, "sb-a", restricted); err == nil {
		t.Error("a failed nft was a success")
	}
	if _, p, _ := n.Enforcer.Lookup(Addr(7)); p.Restricted() {
		t.Error("the enforcer took a policy the kernel didn't")
	}
	n.ClearPolicy(7)
	if _, _, ok := n.Enforcer.Lookup(Addr(7)); ok {
		t.Error("a cleared VM is still known")
	}
}

type call struct {
	stdin string
	args  string
}

// The bridge has the same MAC on every node and every start, so a guest's
// ARP entry for its gateway is still right after a move or a restart; and it
// is the MAC index 1 would have, which no VM gets.
func TestSetupBridgeMAC(t *testing.T) {
	var calls []string
	n := &Network{Run: func(_ context.Context, stdin, name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	}}
	if err := n.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(calls, "ip link add loam0 address 06:00:0a:f0:00:01 type bridge") {
		t.Errorf("no bridge with the fixed MAC: %q", calls)
	}
	if GatewayMAC != MAC(1) {
		t.Errorf("GatewayMAC %s, MAC(1) %s", GatewayMAC, MAC(1))
	}
}

func TestAddTap(t *testing.T) {
	var calls []call
	n := &Network{Run: func(_ context.Context, stdin, name string, args ...string) error {
		calls = append(calls, call{stdin, name + " " + strings.Join(args, " ")})
		return nil
	}}
	tap, err := n.AddTap(context.Background(), 7)
	if err != nil || tap != "lt7" {
		t.Fatalf("%s %v", tap, err)
	}
	want := []string{
		"ip link delete lt7",
		"ip tuntap add dev lt7 mode tap",
		"ip link set lt7 master loam0",
		"ip link set dev lt7 type bridge_slave isolated on",
		"ip link set lt7 up",
	}
	for i, w := range want {
		if i >= len(calls) || calls[i].args != w {
			t.Errorf("call %d: %+v, want %q", i, calls, w)
			break
		}
	}
	if len(calls) != 6 || calls[5].args != "nft -f -" || !strings.HasSuffix(calls[5].stdin, "add element bridge loam vmsrc { \"lt7\" . 10.240.0.7 }\n") {
		t.Errorf("the tap's source address: %+v", calls[len(calls)-1])
	}
	if _, err := n.AddTap(context.Background(), 1); err == nil {
		t.Error("a tap for the gateway's index")
	}
	// A failure part way removes the tap again.
	calls = nil
	n.Run = func(_ context.Context, stdin, name string, args ...string) error {
		calls = append(calls, call{stdin, name + " " + strings.Join(args, " ")})
		if strings.Contains(strings.Join(args, " "), "master") {
			return errors.New("no bridge")
		}
		return nil
	}
	if _, err := n.AddTap(context.Background(), 8); err == nil || calls[len(calls)-1].args != "ip link delete lt8" {
		t.Errorf("failed tap: %v %+v", err, calls)
	}
}

// The forwarder relays queries and answers as they are, over UDP and TCP,
// and moves on to the next upstream when one doesn't answer.
func TestDNSForwarder(t *testing.T) {
	// An upstream that answers every query with the query plus "!".
	pc, ln, err := listenUDPAndTCP("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	defer ln.Close()
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(append(append([]byte{}, buf[:n]...), '!'), from)
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
					writeTCPMessage(c, append(q, '?'))
				}
			}()
		}
	}()
	// The first upstream is a closed port.
	dead, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	deadAddr := dead.LocalAddr().String()
	dead.Close()
	// The test's queries come from 127.0.0.1, an unrestricted VM here.
	e := NewEnforcer(quiet)
	e.Set(loopback, "sb-a", nil)
	d := &DNS{Enforcer: e, Upstreams: []string{deadAddr, pc.LocalAddr().String()}, Timeout: 300 * time.Millisecond, Log: quiet}
	bound, stop, err := d.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	c, err := net.Dial("udp", bound)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write([]byte("\x12\x34query"))
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "\x12\x34query!" {
		t.Errorf("udp answer %q %v", buf[:n], err)
	}

	tc, err := net.Dial("tcp", bound)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	tc.SetDeadline(time.Now().Add(3 * time.Second))
	writeTCPMessage(tc, []byte("\x00\x01tcp"))
	var l [2]byte
	io.ReadFull(tc, l[:])
	msg := make([]byte, binary.BigEndian.Uint16(l[:]))
	io.ReadFull(tc, msg)
	if string(msg) != "\x00\x01tcp?" {
		t.Errorf("tcp answer %q", msg)
	}
}

func TestUpstreams(t *testing.T) {
	got := Upstreams(" 1.1.1.1, 9.9.9.9:5353,,")
	if strings.Join(got, " ") != "1.1.1.1:53 9.9.9.9:5353" {
		t.Errorf("%v", got)
	}
}
