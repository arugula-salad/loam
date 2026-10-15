// Package cgroup puts each Firecracker process in its own cgroup v2 leaf under
// the nodelet pod's cgroup, with CPU, memory and pid limits derived from the
// VM's shape. The pod is the jail in phase 1 (docs/NODE.md, "Inside the
// nodelet"); the leaves keep one VM from taking the whole pod's share.
//
// In a pod with its own cgroup namespace the nodelet sees its cgroup as the
// root of the hierarchy, holding its own processes. cgroup v2 lets a cgroup
// with processes have no controllers enabled for its children, so Open first
// moves the nodelet's processes into a leaf of their own ("nodelet"), then
// enables the controllers, and keeps the VMs under "vms".
package cgroup

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Per-VM limits. Everything is derived from the VM's shape, so the resources
// loam-api asked for drive the host limits through the same numbers that size
// the guest.
const (
	// cpuPeriodUs is the cgroup v2 default scheduling period.
	cpuPeriodUs = 100000
	// cpuSlackCores is CPU the VMM's own threads get on top of the guest's
	// vCPUs: virtio I/O, the API server and the vsock muxer are not vCPUs, and
	// capping the lot at exactly vcpu_count throttles disk-heavy guests.
	cpuSlackCores = 1
	// memHeadroomMiB covers Firecracker itself (~30 MiB) plus page cache for the
	// VM's disk on top of the guest's RAM, and memHardSlackMiB sits between
	// memory.high and memory.max. Reclaim (high) rather than an OOM kill (max)
	// has to be what a busy VM hits: an OOM kill of the VMM destroys the
	// guest's memory state, and a snapshot writes the whole RAM through page
	// cache charged to this cgroup.
	memHeadroomMiB   = 192
	memHardSlackMiB  = 256
	pidsBase         = 32
	pidsPerVCPU      = 4
	cgroupRemoveTry  = 20
	cgroupRemoveWait = 25 * time.Millisecond
)

var controllers = []string{"cpu", "memory", "pids"}

// Limits is one VM's share.
type Limits struct {
	VCPUs  int
	MemMiB int // guest RAM; the cgroup adds headroom for the VMM on top
}

// Root is the subtree holding one leaf per running VMM.
type Root struct {
	vms string
}

// Leaf is one VM's cgroup. Its fd goes to clone3(CLONE_INTO_CGROUP) so the VMM
// is born inside the limits rather than moved into them afterwards.
type Leaf struct {
	path string
	fd   int
}

// Open prepares the nodelet's cgroup for VM leaves, or says why it cannot.
// mount is the cgroup2 mount, "" to find it.
func Open(mount string) (*Root, error) {
	if mount == "" {
		var err error
		if mount, err = cgroup2Mount(); err != nil {
			return nil, err
		}
	}
	rel, err := ownCgroup()
	if err != nil {
		return nil, err
	}
	own := filepath.Join(mount, rel)
	if err := writable(mount, own); err != nil {
		return nil, err
	}
	if missing := missing(filepath.Join(own, "cgroup.controllers")); len(missing) > 0 {
		return nil, fmt.Errorf("cgroup %s lacks the %s controllers", own, strings.Join(missing, ","))
	}
	// Our own processes leave for a leaf, unless the subtree is already set up
	// (a nodelet restarted in the same cgroup, which it then shares with that
	// leaf's other members).
	if filepath.Base(own) != "nodelet" || !hasSibling(own, "vms") {
		if err := evacuate(own, filepath.Join(own, "nodelet")); err != nil {
			return nil, err
		}
	} else {
		own = filepath.Dir(own)
	}
	if err := os.WriteFile(filepath.Join(own, "cgroup.subtree_control"), []byte("+cpu +memory +pids"), 0o644); err != nil {
		return nil, fmt.Errorf("enable controllers in %s: %w", own, err)
	}
	vms := filepath.Join(own, "vms")
	if err := os.Mkdir(vms, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(vms, "cgroup.subtree_control"), []byte("+cpu +memory +pids"), 0o644); err != nil {
		return nil, fmt.Errorf("enable controllers in %s: %w", vms, err)
	}
	return &Root{vms: vms}, nil
}

func hasSibling(dir, name string) bool {
	_, err := os.Stat(filepath.Join(filepath.Dir(dir), name))
	return err == nil
}

// writable makes sure the cgroup tree can be written. A container runtime
// mounts it read-only; in a private cgroup namespace a nodelet with
// CAP_SYS_ADMIN may remount it read-write, which reaches only its own subtree.
func writable(mount, own string) error {
	var st unix.Statfs_t
	if err := unix.Statfs(mount, &st); err != nil {
		return err
	}
	if st.Flags&unix.ST_RDONLY == 0 {
		return nil
	}
	if own != mount {
		return fmt.Errorf("%s is read-only and the nodelet is not at the root of a private cgroup namespace", mount)
	}
	if err := unix.Mount("", mount, "", unix.MS_REMOUNT|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC|unix.MS_RELATIME, ""); err != nil {
		return fmt.Errorf("%s is read-only and remounting it failed (the nodelet needs CAP_SYS_ADMIN, or --cgroups=false): %w", mount, err)
	}
	return nil
}

// evacuate moves every process in from into the leaf to.
func evacuate(from, to string) error {
	if err := os.Mkdir(to, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	b, err := os.ReadFile(filepath.Join(from, "cgroup.procs"))
	if err != nil {
		return err
	}
	for _, pid := range strings.Fields(string(b)) {
		// A process may exit between the read and the move; that is not an error.
		if err := os.WriteFile(filepath.Join(to, "cgroup.procs"), []byte(pid), 0o644); err != nil && !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("move pid %s to %s: %w", pid, to, err)
		}
	}
	return nil
}

// cgroup2Mount finds where cgroup2 is mounted (/sys/fs/cgroup on any current host).
func cgroup2Mount() (string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	defer f.Close()
	for sc := bufio.NewScanner(f); sc.Scan(); {
		// ... <mountpoint> ... - <fstype> <source> <opts>
		fields := strings.Fields(sc.Text())
		for i, fl := range fields {
			if fl == "-" && i+1 < len(fields) && fields[i+1] == "cgroup2" && i >= 4 {
				return fields[4], nil
			}
		}
	}
	return "", errors.New("no cgroup2 mount")
}

// ownCgroup is this process's cgroup path, relative to the mount.
func ownCgroup() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		// cgroup v2 is always the "0::<path>" entry.
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", errors.New("not in a cgroup v2 hierarchy")
}

// missing reports which controllers a cgroup.controllers file lacks.
func missing(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return controllers
	}
	have := map[string]bool{}
	for _, c := range strings.Fields(string(b)) {
		have[c] = true
	}
	var out []string
	for _, c := range controllers {
		if !have[c] {
			out = append(out, c)
		}
	}
	return out
}

// LimitFiles is what a leaf for lim writes, in the order it writes them:
// memory.high must not be left above a lower memory.max.
func LimitFiles(lim Limits) [][2]string {
	return [][2]string{
		{"cpu.max", fmt.Sprintf("%d %d", (lim.VCPUs+cpuSlackCores)*cpuPeriodUs, cpuPeriodUs)},
		{"pids.max", strconv.Itoa(pidsBase + pidsPerVCPU*lim.VCPUs)},
		{"memory.max", strconv.Itoa((lim.MemMiB + memHeadroomMiB + memHardSlackMiB) << 20)},
		{"memory.high", strconv.Itoa((lim.MemMiB + memHeadroomMiB) << 20)},
	}
}

// Create makes the leaf for one VM and writes its limits. The caller must
// Close it once the child is started and Remove it when the VMM exits.
func (r *Root) Create(id string, lim Limits) (*Leaf, error) {
	path := filepath.Join(r.vms, id)
	if err := os.Mkdir(path, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	g := &Leaf{path: path, fd: -1}
	for _, kv := range LimitFiles(lim) {
		if err := os.WriteFile(filepath.Join(path, kv[0]), []byte(kv[1]), 0o644); err != nil {
			g.Remove()
			return nil, fmt.Errorf("cgroup %s: %w", kv[0], err)
		}
	}
	fd, err := unix.Open(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		g.Remove()
		return nil, fmt.Errorf("cgroup open %s: %w", path, err)
	}
	g.fd = fd
	return g, nil
}

// Sweep removes leaves left by a nodelet that died without cleaning up. A leaf
// whose VMM is still running cannot be removed and is left alone, so orphaned
// VMMs must be reaped first.
func (r *Root) Sweep() {
	entries, _ := os.ReadDir(r.vms)
	for _, e := range entries {
		if e.IsDir() {
			removeCgroup(filepath.Join(r.vms, e.Name()))
		}
	}
}

// FD is the directory fd for SysProcAttr.CgroupFD.
func (g *Leaf) FD() int { return g.fd }

// Close releases the directory fd; the cgroup stays until Remove. Safe on nil.
func (g *Leaf) Close() {
	if g != nil && g.fd >= 0 {
		unix.Close(g.fd)
		g.fd = -1
	}
}

// Remove deletes the leaf. Safe on nil.
func (g *Leaf) Remove() {
	if g == nil {
		return
	}
	g.Close()
	removeCgroup(g.path)
}

// removeCgroup rmdirs a cgroup, waiting out the EBUSY window while the kernel
// finishes reaping a process that has just been killed.
func removeCgroup(path string) bool {
	for i := 0; i < cgroupRemoveTry; i++ {
		if err := os.Remove(path); err == nil || errors.Is(err, os.ErrNotExist) {
			return true
		}
		time.Sleep(cgroupRemoveWait)
	}
	return false
}
