package agent

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/arugula-salad/loam/internal/nodeapi"
)

// guestSystem is the System of a real guest, where the agent is root and
// PID 1's child.
type guestSystem struct{}

// NewSystem is the System of a real guest.
func NewSystem() System { return guestSystem{} }

func (guestSystem) SetClock(t time.Time) error {
	tv := unix.NsecToTimeval(t.UnixNano())
	return unix.Settimeofday(&tv)
}

// SetHostname sets the kernel's hostname and the files that name it: sudo
// and friends expect the hostname to resolve.
func (guestSystem) SetHostname(h string) error {
	if err := unix.Sethostname([]byte(h)); err != nil {
		return err
	}
	WriteHostFiles(h)
	return nil
}

// WriteHostFiles writes /etc/hostname and /etc/hosts for hostname h.
func WriteHostFiles(h string) {
	// WriteFile keeps an existing file's mode; an image may ship odd ones.
	os.WriteFile("/etc/hostname", []byte(h+"\n"), 0o644)
	os.Chmod("/etc/hostname", 0o644)
	os.WriteFile("/etc/hosts", []byte("127.0.0.1\tlocalhost\n127.0.1.1\t"+h+"\n"+
		"::1\tlocalhost ip6-localhost ip6-loopback\n"), 0o644)
}

func (guestSystem) ConfigureNetwork(n *nodeapi.WakeNetwork) error { return ConfigureEth0(n) }

func (guestSystem) DropCaches() {
	unix.Sync()
	os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0o200)
	os.WriteFile("/proc/sys/vm/compact_memory", []byte("1"), 0o200)
}

func (guestSystem) Sync() { unix.Sync() }

// Poweroff resets the machine: with reboot=k on the kernel command line that
// goes through the keyboard controller, which makes Firecracker exit cleanly
// (it has no ACPI poweroff).
func (guestSystem) Poweroff() {
	unix.Sync()
	if err := unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART); err != nil {
		logf("reboot: %v", err)
	}
}

// ConfigureEth0 converges eth0 on n: the MAC, the one IPv4 address, the
// default route and the resolvers. It is idempotent, so it runs at boot from
// the kernel command line and again at every wake.
func ConfigureEth0(n *nodeapi.WakeNetwork) error {
	ip, ipnet, err := net.ParseCIDR(n.IP)
	if err != nil || ip.To4() == nil {
		return fmt.Errorf("ip %q: want an IPv4 address with its prefix length", n.IP)
	}
	gw := net.ParseIP(n.Gateway)
	if n.Gateway != "" && (gw == nil || gw.To4() == nil) {
		return fmt.Errorf("gateway %q: not an IPv4 address", n.Gateway)
	}
	eth, err := net.InterfaceByName("eth0")
	if err != nil {
		return err
	}
	if n.MAC != "" {
		mac, err := net.ParseMAC(n.MAC)
		if err != nil {
			return fmt.Errorf("mac %q: %w", n.MAC, err)
		}
		if eth.HardwareAddr.String() != mac.String() {
			// The address of a link can only change while it is down.
			if err := linkSet(eth.Index, false, nil); err != nil {
				return fmt.Errorf("eth0 down: %w", err)
			}
			if err := linkSet(eth.Index, false, mac); err != nil {
				return fmt.Errorf("eth0 mac: %w", err)
			}
		}
	}
	// The old address goes before the new one comes: in the same subnet the
	// new one would be its secondary, and deleting a primary address takes
	// its secondaries with it.
	if err := flushOtherAddrs(eth.Index, ip.To4()); err != nil {
		return fmt.Errorf("eth0 old addresses: %w", err)
	}
	ones, _ := ipnet.Mask.Size()
	if err := addrReplace(eth.Index, ip.To4(), ones); err != nil {
		return fmt.Errorf("eth0 address: %w", err)
	}
	if err := linkSet(eth.Index, true, nil); err != nil {
		return fmt.Errorf("eth0 up: %w", err)
	}
	if gw != nil {
		if err := defaultRouteReplace(eth.Index, gw.To4()); err != nil {
			return fmt.Errorf("default route: %w", err)
		}
	}
	if len(n.DNS) > 0 {
		var b strings.Builder
		for _, s := range n.DNS {
			b.WriteString("nameserver " + s + "\n")
		}
		os.Remove("/etc/resolv.conf") // often a dangling systemd-resolved symlink
		if err := os.WriteFile("/etc/resolv.conf", []byte(b.String()), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// LoopbackUp brings lo up.
func LoopbackUp() error {
	lo, err := net.InterfaceByName("lo")
	if err != nil {
		return err
	}
	return linkSet(lo.Index, true, nil)
}
