// Package network gives VMs their network inside the nodelet pod's own network
// namespace (docs/NODE.md, "Inside the nodelet"): the bridge loam0 with the
// gateway address, one tap per VM on it, nftables rules that NAT VMs out of the
// pod and keep them off everything but the public internet, a DNS forwarder
// on the gateway, and the egress proxy beside it that enforces a VM's policy
// (docs/NODE.md, "Phase 3: the network boundary"; enforcer.go has the
// state they share). Nothing here touches the host's network: the pod's
// namespace is the only one the nodelet can see.
//
// Addresses come from an index: VM n (2 <= n < 65535) is 10.240.<n/256>.<n%256>,
// its tap is lt<n>, and its MAC is derived from its address.
package network

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	// Bridge is the VMs' bridge in the pod's namespace.
	Bridge = "loam0"
	// Table is the nftables table the nodelet owns, in the inet family.
	Table     = "loam"
	tapPrefix = "lt"
)

// Net is the VMs' network and Gateway the bridge's address, which is also
// where the DNS forwarder listens.
var (
	Net     = netip.MustParsePrefix("10.240.0.0/16")
	Gateway = netip.MustParseAddr("10.240.0.1")
)

// MinIndex and MaxIndex bound a VM's network index: 0 is the network, 1 the
// gateway and 65535 the broadcast address.
const (
	MinIndex = 2
	MaxIndex = 65534
)

// Addr is the address of the VM with index i.
func Addr(i int) netip.Addr {
	b := Net.Addr().As4()
	return netip.AddrFrom4([4]byte{b[0], b[1], byte(i >> 8), byte(i)})
}

// Tap is the tap device of the VM with index i.
func Tap(i int) string { return tapPrefix + strconv.Itoa(i) }

// GatewayMAC is the bridge's MAC, the same on every node and every start. A
// guest keeps the gateway's MAC in its ARP cache across a pause, so a sandbox
// that wakes on another node, or after its nodelet restarted, would send to a
// MAC nobody has until the entry went stale, about half a minute without a
// network. Left to itself a bridge takes a random MAC, or its lowest port's,
// which changes as taps come and go under running VMs.
const GatewayMAC = "06:00:0a:f0:00:01"

// MAC is the guest MAC of the VM with index i: locally administered, and
// unique on the bridge because the address is.
func MAC(i int) string {
	a := Addr(i).As4()
	return fmt.Sprintf("06:00:%02x:%02x:%02x:%02x", a[0], a[1], a[2], a[3])
}

// Prefix is the VM's address with the network's length, as the guest configures it.
func Prefix(i int) string { return netip.PrefixFrom(Addr(i), Net.Bits()).String() }

// blocked is every IPv4 range that is not the public internet: private (the pod
// network, the LAN), CGNAT (the tailnet), link-local (cloud metadata),
// loopback, multicast and the reserved blocks. VMs may not send to any of it.
var blocked = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4",
}

// ProxyPort is where the egress proxy listens on the gateway (proxy.go).
const ProxyPort = 7880

// Ruleset is the nftables tables, applied as one transaction (the leading
// add+delete makes re-applying them replace each table whole).
//
// What the inet table says, chain by chain:
//   - prerouting (nat): a restricted VM's TCP, and the port 443 of a VM with
//     secrets and no rules, goes to the proxy on the gateway (a redirect, so
//     the proxy reads where it was going with SO_ORIGINAL_DST). Traffic to
//     the gateway itself (DNS) is left as it is.
//   - input: from the bridge, the DNS forwarder, ping on the gateway,
//     connections the prerouting chain redirected to the proxy (never one a
//     VM opened to the proxy's port itself), and replies to what the nodelet
//     itself opened; nothing else. The node protocol on :7070 is on the pod's
//     own interface, and a VM must not reach it.
//   - forward: from the bridge, a restricted VM's packets first: everything
//     it may send went to the gateway (DNS, the proxy), so whatever is left
//     (UDP, QUIC, ICMP, a connection from before it was restricted) is
//     dropped. Then replies, then a drop of every non-public destination
//     (which includes other VMs, routed back through the gateway: the bridge
//     ports are isolated at layer 2 too) and of IPv6; the rest of the
//     internet is allowed. To the bridge, only replies: nothing in the pod
//     network opens a connection to a VM.
//   - postrouting: VMs leave the pod as the pod's address.
//
// The sets restricted4 and https4 hold the addresses of the VMs whose policy
// says so (SetPolicy); everything about a VM's policy beyond which set it is
// in is the proxy's and the DNS forwarder's.
//
// The bridge table keeps each VM to its own address. The sets above, the
// forwarder and the proxy all know a VM by its source address, so a guest
// that sent from another VM's address would be judged by that VM's policy:
// it couldn't read the answers, but a DNS query or a UDP packet carries data
// out on its own. vmsrc pairs each tap with its address (AddTap), and a frame
// from a tap whose IPv4 or ARP sender isn't its own is dropped before it
// reaches the bridge.
func Ruleset() string {
	return fmt.Sprintf(`table inet %[1]s
delete table inet %[1]s
table inet %[1]s {
  set blocked4 {
    type ipv4_addr; flags interval
    elements = { %[2]s }
  }
  set restricted4 {
    type ipv4_addr
  }
  set https4 {
    type ipv4_addr
  }
  chain prerouting {
    type nat hook prerouting priority dstnat; policy accept;
    iifname "%[3]s" ip daddr %[4]s return
    iifname "%[3]s" ip saddr @restricted4 meta l4proto tcp redirect to :%[6]d
    iifname "%[3]s" ip saddr @https4 tcp dport 443 redirect to :%[6]d
  }
  chain input {
    type filter hook input priority filter; policy accept;
    iifname "%[3]s" ct state established,related accept
    iifname "%[3]s" ip daddr %[4]s meta l4proto { tcp, udp } th dport 53 accept
    iifname "%[3]s" ip daddr %[4]s tcp dport %[6]d ct status dnat accept
    iifname "%[3]s" ip daddr %[4]s icmp type echo-request accept
    iifname "%[3]s" drop comment "VMs may not talk to the nodelet"
  }
  chain forward {
    type filter hook forward priority filter; policy accept;
    iifname "%[3]s" ip saddr @restricted4 drop comment "a restricted VM leaves only through the proxy"
    iifname "%[3]s" ct state established,related accept
    iifname "%[3]s" ip daddr @blocked4 drop comment "no pod network, LAN, tailnet or other VMs"
    iifname "%[3]s" meta nfproto ipv6 drop
    oifname "%[3]s" ct state established,related accept
    oifname "%[3]s" drop comment "nothing opens a connection to a VM"
  }
  chain postrouting {
    type nat hook postrouting priority srcnat; policy accept;
    ip saddr %[5]s oifname != "%[3]s" masquerade
  }
}
table bridge %[1]s
delete table bridge %[1]s
table bridge %[1]s {
  set vmsrc {
    type ifname . ipv4_addr
  }
  chain prerouting {
    type filter hook prerouting priority filter; policy accept;
    iifname "%[7]s*" ether type ip iifname . ip saddr != @vmsrc drop comment "a VM sends from its own address"
    iifname "%[7]s*" ether type arp iifname . arp saddr ip != @vmsrc drop
  }
}
`, Table, strings.Join(blocked, ", "), Bridge, Gateway, Net, ProxyPort, tapPrefix)
}

// Runner runs a command; injected so tests need neither root nor iproute2.
type Runner func(ctx context.Context, stdin string, name string, args ...string) error

// Exec runs commands for real.
func Exec(ctx context.Context, stdin string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Network is the VMs' network in this namespace.
type Network struct {
	Run Runner
	// Enforcer holds each running VM's policy for the DNS forwarder and the
	// proxy; SetPolicy puts it there.
	Enforcer *Enforcer
}

// Setup makes the bridge and the rules from scratch: whatever a previous
// nodelet left in this namespace (taps of VMs that died with it) is removed.
func (n *Network) Setup(ctx context.Context) error {
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); err == nil && strings.TrimSpace(string(b)) != "1" {
		if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil {
			return fmt.Errorf("net.ipv4.ip_forward is 0 in the pod and cannot be set (%v); set it with the pod's sysctls", err)
		}
	}
	entries, _ := os.ReadDir("/sys/class/net")
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tapPrefix) {
			if _, err := strconv.Atoi(strings.TrimPrefix(e.Name(), tapPrefix)); err == nil {
				n.Run(ctx, "", "ip", "link", "delete", e.Name())
			}
		}
	}
	n.Run(ctx, "", "ip", "link", "delete", Bridge) // absent on a first start
	gw := netip.PrefixFrom(Gateway, Net.Bits()).String()
	for _, args := range [][]string{
		{"link", "add", Bridge, "address", GatewayMAC, "type", "bridge"},
		{"addr", "add", gw, "dev", Bridge},
		{"link", "set", Bridge, "up"},
	} {
		if err := n.Run(ctx, "", "ip", args...); err != nil {
			return err
		}
	}
	return n.Run(ctx, Ruleset(), "nft", "-f", "-")
}

// AddTap creates VM i's tap on the bridge. Its port is isolated: it can reach
// the bridge (the gateway) but no other port.
func (n *Network) AddTap(ctx context.Context, i int) (string, error) {
	if i < MinIndex || i > MaxIndex {
		return "", fmt.Errorf("network index %d out of range", i)
	}
	tap := Tap(i)
	n.Run(ctx, "", "ip", "link", "delete", tap) // left by a VM that died uncleanly
	for _, args := range [][]string{
		{"tuntap", "add", "dev", tap, "mode", "tap"},
		{"link", "set", tap, "master", Bridge},
		{"link", "set", "dev", tap, "type", "bridge_slave", "isolated", "on"},
		{"link", "set", tap, "up"},
	} {
		if err := n.Run(ctx, "", "ip", args...); err != nil {
			n.Run(ctx, "", "ip", "link", "delete", tap)
			return "", err
		}
	}
	// Its address is the only one it may send from (Ruleset, the bridge table).
	if err := n.Run(ctx, srcElements(i, true), "nft", "-f", "-"); err != nil {
		n.Run(ctx, "", "ip", "link", "delete", tap)
		return "", err
	}
	return tap, nil
}

// srcElements is the nft script that leaves VM i's pair in vmsrc (add) or
// takes it out. Adding the element first makes the delete succeed whether it
// was there or not, so one a VM that died uncleanly left changes nothing.
func srcElements(i int, add bool) string {
	el := fmt.Sprintf("{ %q . %s }", Tap(i), Addr(i))
	s := fmt.Sprintf("add element bridge %[1]s vmsrc %[2]s\ndelete element bridge %[1]s vmsrc %[2]s\n", Table, el)
	if add {
		s += fmt.Sprintf("add element bridge %s vmsrc %s\n", Table, el)
	}
	return s
}

// RemoveTap deletes VM i's tap.
func (n *Network) RemoveTap(i int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n.Run(ctx, "", "ip", "link", "delete", Tap(i))
	n.Run(ctx, srcElements(i, false), "nft", "-f", "-")
}

// SetPolicy puts p in place for VM i, which id now owns: the set its address
// is in, then the policy the forwarder and the proxy apply. The caller does
// this before the VM's guest runs, so it never runs with less than p; on a
// running VM it replaces what it had, and the connections p no longer allows
// are closed. An error leaves the VM's previous policy in place.
func (n *Network) SetPolicy(ctx context.Context, i int, id string, p *Policy) error {
	if err := n.Run(ctx, policyElements(i, p.set()), "nft", "-f", "-"); err != nil {
		return fmt.Errorf("the network policy of %s: %w", id, err)
	}
	n.Enforcer.Set(Addr(i), id, p)
	return nil
}

// ClearPolicy forgets VM i's policy, once its guest no longer runs, and
// closes whatever the proxy still carried for it.
func (n *Network) ClearPolicy(i int) {
	n.Enforcer.Remove(Addr(i))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n.Run(ctx, policyElements(i, ""), "nft", "-f", "-")
}

// The sets a VM's address may be in, by its policy (Ruleset).
const (
	setRestricted = "restricted4"
	setHTTPS      = "https4"
)

// policyElements is the nft script that leaves VM i's address in set
// (setRestricted or setHTTPS) and in no other, or in none for "". It is one
// transaction: a VM moving from one set to the other is never in neither.
func policyElements(i int, set string) string {
	var b strings.Builder
	for _, s := range []string{setRestricted, setHTTPS} {
		fmt.Fprintf(&b, "add element inet %[1]s %[2]s { %[3]s }\ndelete element inet %[1]s %[2]s { %[3]s }\n", Table, s, Addr(i))
	}
	if set != "" {
		fmt.Fprintf(&b, "add element inet %s %s { %s }\n", Table, set, Addr(i))
	}
	return b.String()
}

// ErrFull is a node whose every network index is taken.
var ErrFull = errors.New("no free network address")

// FreeIndex is the lowest index not in used.
func FreeIndex(used map[int]bool) (int, error) {
	for i := MinIndex; i <= MaxIndex; i++ {
		if !used[i] {
			return i, nil
		}
	}
	return 0, ErrFull
}
