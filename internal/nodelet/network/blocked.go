package network

import (
	"net"
	"net/netip"
)

// blockedPrefixes is blocked, parsed.
var blockedPrefixes = func() []netip.Prefix {
	out := make([]netip.Prefix, len(blocked))
	for i, s := range blocked {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}()

// NonPublic reports whether a is anything other than a public IPv4 address.
func NonPublic(a netip.Addr) bool {
	a = a.Unmap()
	if !a.Is4() {
		return true // guests have no IPv6, so nothing legitimate arrives as v6
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Blocked is NonPublic plus the pod's own addresses. The proxy dials from the
// pod, so its connections skip the forward chain that keeps VMs out of the
// pod network (and the pod's NetworkPolicy lets the pod reach loam-api and
// the snapshot store): this check is all there is for them. Interfaces are
// read on every call because they change.
func Blocked(a netip.Addr) bool {
	if NonPublic(a) {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return true
	}
	for _, ifa := range addrs {
		if n, ok := ifa.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap() == a.Unmap() {
				return true
			}
		}
	}
	return false
}
