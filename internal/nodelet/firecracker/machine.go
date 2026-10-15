// Package firecracker drives one Firecracker microVM per VM directory: a fresh
// boot, a full snapshot to files, a restore from them, the balloon, and vsock
// streams both ways. It knows nothing of loam's records; internal/nodelet
// decides what a pause or a checkpoint is made of.
package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/arugula-salad/loam/internal/nodelet/cgroup"
)

// File names inside a VM directory. Firecracker runs with the directory as its
// cwd and only ever sees these relative names for the disk and the vsock: that
// keeps unix socket paths under the 108-byte sun_path limit and makes memory
// snapshots relocatable, so one VM's snapshot restores as another's.
const (
	apiSock    = "fc.sock"
	VsockSock  = "v.sock"
	DiskFile   = "disk.ext4"
	ConsoleLog = "console.log"
	pidFile    = "fc.pid"
)

// GuestCID is every guest's vsock context ID. Each VM has its own vsock device
// and socket, so they never meet.
const GuestCID = 3

// Host is what every VM on the node shares.
type Host struct {
	Firecracker string
	Kernel      string
	Initramfs   string
	// Cgroups, when set, starts every VMM in its own leaf with limits derived
	// from its shape. nil runs Firecracker in the nodelet's own cgroup, bounded
	// only by the pod.
	Cgroups *cgroup.Root
}

// Config is one VM's shape.
type Config struct {
	ID     string // names the cgroup leaf
	Dir    string // VM directory; must be short (holds unix sockets)
	VCPUs  int
	MemMiB int
	// Tap and MAC give the VM its NIC; a VM without a tap boots with none.
	Tap, MAC string
	// BootArgs are appended to the kernel command line; they reach a fresh boot only.
	BootArgs []string
}

// Machine is a running Firecracker process.
type Machine struct {
	cfg  Config
	cmd  *exec.Cmd
	cg   *cgroup.Leaf
	http *http.Client

	exitOnce sync.Once
	exited   chan struct{}
}

// PidOf returns the VMM recorded in dir's pid file, if that process is still a
// Firecracker running there (pids get reused).
func PidOf(dir string) (int, bool) {
	b, err := os.ReadFile(filepath.Join(dir, pidFile))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false
	}
	exe, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	cwd, _ := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	return pid, strings.Contains(filepath.Base(exe), "firecracker") && cwd == dir
}

// ReapOrphan kills a Firecracker left behind in dir by a nodelet that died
// without cleaning up. Its memory state is unrecoverable, so the VM is stopped.
func ReapOrphan(dir string) {
	// Guard against pid reuse: only signal it if it really is a firecracker in this dir.
	if pid, ok := PidOf(dir); ok {
		syscall.Kill(pid, syscall.SIGKILL)
	}
	for _, f := range []string{pidFile, apiSock, VsockSock} {
		os.Remove(filepath.Join(dir, f))
	}
}

func launch(h Host, cfg Config) (*Machine, error) {
	for _, s := range []string{apiSock, VsockSock} {
		os.Remove(filepath.Join(cfg.Dir, s))
	}
	console, err := os.OpenFile(filepath.Join(cfg.Dir, ConsoleLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	defer console.Close()

	cmd := exec.Command(h.Firecracker, "--api-sock", apiSock, "--level", "Warning")
	cmd.Dir = cfg.Dir
	cmd.Stdout, cmd.Stderr = console, console
	// Own process group so a signal for the nodelet's group doesn't hit the VMs directly.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var leaf *cgroup.Leaf
	if h.Cgroups != nil {
		// Born inside its limits (clone3 with CLONE_INTO_CGROUP) rather than
		// moved into them after it has started allocating.
		if leaf, err = h.Cgroups.Create(cfg.ID, cgroup.Limits{VCPUs: cfg.VCPUs, MemMiB: cfg.MemMiB}); err != nil {
			return nil, fmt.Errorf("cgroup for %s: %w", cfg.ID, err)
		}
		cmd.SysProcAttr.UseCgroupFD = true
		cmd.SysProcAttr.CgroupFD = leaf.FD()
	}
	if err := cmd.Start(); err != nil {
		leaf.Remove()
		return nil, fmt.Errorf("start firecracker: %w", err)
	}
	// The cgroup itself outlives this fd; the VM is in it now.
	leaf.Close()
	// Without the pid file a VM orphaned by a crash of the nodelet is never reaped.
	if err := os.WriteFile(filepath.Join(cfg.Dir, pidFile), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		leaf.Remove()
		return nil, fmt.Errorf("write pid file: %w", err)
	}
	sock := filepath.Join(cfg.Dir, apiSock)
	m := &Machine{cfg: cfg, cmd: cmd, cg: leaf, exited: make(chan struct{}),
		http: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}},
	}
	go func() {
		cmd.Wait()
		leaf.Remove()
		m.exitOnce.Do(func() { close(m.exited) })
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			return m, nil
		}
		select {
		case <-m.exited:
			return nil, fmt.Errorf("firecracker exited during startup%s", ConsoleTail(cfg.Dir))
		default:
		}
		if time.Now().After(deadline) {
			m.Kill()
			return nil, errors.New("timed out waiting for firecracker API socket")
		}
		time.Sleep(time.Millisecond)
	}
}

// ConsoleTail is the end of a VM's console log, for an error that needs it.
func ConsoleTail(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ConsoleLog))
	if err != nil || len(b) == 0 {
		return ""
	}
	if len(b) > 1500 {
		b = b[len(b)-1500:]
	}
	return "\n--- console ---\n" + string(b)
}

func (m *Machine) api(ctx context.Context, method, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://firecracker"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.http.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("firecracker %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

type obj = map[string]any

// Boot starts a VM fresh from the kernel and initramfs with dir's disk attached.
func Boot(ctx context.Context, h Host, cfg Config) (*Machine, error) {
	m, err := launch(h, cfg)
	if err != nil {
		return nil, err
	}
	args := append([]string{"console=ttyS0", "reboot=k", "panic=1", "pci=off", "quiet", "loglevel=3",
		"i8042.noaux", "i8042.nomux", "i8042.dumbkbd"}, cfg.BootArgs...)
	steps := []struct {
		path string
		body obj
	}{
		{"/boot-source", obj{"kernel_image_path": h.Kernel, "initrd_path": h.Initramfs, "boot_args": strings.Join(args, " ")}},
		{"/drives/root", obj{"drive_id": "root", "path_on_host": DiskFile, "is_root_device": false, "is_read_only": false}},
		// Dirty page tracking from the first instruction, so the snapshot after
		// a restore of this VM's first one can be a diff (Snapshot).
		{"/machine-config", obj{"vcpu_count": cfg.VCPUs, "mem_size_mib": cfg.MemMiB, "track_dirty_pages": true}},
		{"/vsock", obj{"guest_cid": GuestCID, "uds_path": VsockSock}},
		{"/entropy", obj{}},
		{"/balloon", balloonConfig()},
	}
	if cfg.Tap != "" {
		steps = append(steps, struct {
			path string
			body obj
		}{"/network-interfaces/eth0", obj{"iface_id": "eth0", "host_dev_name": cfg.Tap, "guest_mac": cfg.MAC}})
	}
	for _, s := range steps {
		if err := m.api(ctx, http.MethodPut, s.path, s.body); err != nil {
			m.Kill()
			return nil, err
		}
	}
	if err := m.api(ctx, http.MethodPut, "/actions", obj{"action_type": "InstanceStart"}); err != nil {
		m.Kill()
		return nil, err
	}
	return m, nil
}

// Memory is where a restored VM's memory comes from.
type Memory struct {
	// File is a full memory file (absolute path), mapped privately: it is never
	// written to and may be restored again, or deleted, while this VM runs.
	File string
	// Uffd is the unix socket of a userfaultfd handler that serves the memory
	// lazily (docs/STORE.md, "The UFFD helper process"). Firecracker connects to
	// it during the load and hands it the guest memory's regions and its
	// userfaultfd; the load returns once the handler has them.
	Uffd string
}

// Restore starts a VM from a snapshot's state file (absolute path) and its
// memory, and resumes it. The disk and vsock paths recorded in the state are
// relative, and resolve in cfg.Dir. Dirty page tracking is on, so the VM's
// next snapshot can be a diff against this one.
func Restore(ctx context.Context, h Host, cfg Config, state string, mem Memory) (*Machine, error) {
	backend := obj{"backend_type": "File", "backend_path": mem.File}
	if mem.Uffd != "" {
		backend = obj{"backend_type": "Uffd", "backend_path": mem.Uffd}
	}
	m, err := launch(h, cfg)
	if err != nil {
		return nil, err
	}
	body := obj{
		"snapshot_path":     state,
		"mem_backend":       backend,
		"track_dirty_pages": true,
		"resume_vm":         true,
	}
	if cfg.Tap != "" {
		body["network_overrides"] = []obj{{"iface_id": "eth0", "host_dev_name": cfg.Tap}}
	}
	if err := m.api(ctx, http.MethodPut, "/snapshot/load", body); err != nil {
		m.Kill()
		return nil, err
	}
	return m, nil
}

// Pause and Resume freeze and unfreeze the vCPUs.
func (m *Machine) Pause(ctx context.Context) error {
	return m.api(ctx, http.MethodPatch, "/vm", obj{"state": "Paused"})
}

func (m *Machine) Resume(ctx context.Context) error {
	return m.api(ctx, http.MethodPatch, "/vm", obj{"state": "Resumed"})
}

// Snapshot writes a snapshot of the paused VM to state and mem (absolute
// paths), unsynced: Firecracker would fsync the whole-RAM memory file, zeros
// and all, and the caller makes it sparse (CompactMemory) before syncing it.
// A full snapshot's memory file is the whole of guest memory. A diff's holds
// only the pages dirtied since the VM was restored or last snapshotted, at
// their own offsets, and holes elsewhere: there a hole means unchanged, not
// zero. Either resets the dirty log, so the next diff starts from this one.
func (m *Machine) Snapshot(ctx context.Context, state, mem string, diff bool) error {
	kind := "Full"
	if diff {
		kind = "Diff"
	}
	return m.api(ctx, http.MethodPut, "/snapshot/create", obj{
		"snapshot_type": kind, "snapshot_path": state, "mem_file_path": mem,
		"sync_snapshot_files": false,
	})
}

// Kill stops the VMM immediately. Guest state that was not synced is lost.
func (m *Machine) Kill() {
	if m.cmd.Process != nil {
		m.cmd.Process.Kill()
	}
	<-m.exited
	for _, f := range []string{pidFile, apiSock, VsockSock} {
		os.Remove(filepath.Join(m.cfg.Dir, f))
	}
}

// Pid is the VMM process.
func (m *Machine) Pid() int { return m.cmd.Process.Pid }

// Exited is closed when the VMM process ends (guest reboot or poweroff, crash, or Kill).
func (m *Machine) Exited() <-chan struct{} { return m.exited }

// ListenGuest accepts streams the guest opens to the host. Firecracker maps a
// guest connect() to CID 2 port N onto the unix socket "<uds_path>_N"; it looks
// the socket up per connection, so the listener only has to exist by the time
// the guest first uses it, and one made before a snapshot restore works the same.
// Closing the listener removes the socket file.
func ListenGuest(dir string, port uint32) (net.Listener, error) {
	path := filepath.Join(dir, fmt.Sprintf("%s_%d", VsockSock, port))
	os.Remove(path) // left behind by a nodelet that died
	return net.Listen("unix", path)
}

// Dial opens a stream to the guest's vsock port using Firecracker's
// host-initiated handshake: connect to the UDS, send "CONNECT <port>", expect "OK <n>".
func (m *Machine) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", filepath.Join(m.cfg.Dir, VsockSock))
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(5 * time.Second))
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		conn.Close()
		return nil, err
	}
	// Read the reply byte-wise: anything after the newline belongs to the caller.
	var line string
	for b := make([]byte, 1); !strings.HasSuffix(line, "\n") && len(line) < 64; {
		if _, err := conn.Read(b); err != nil {
			conn.Close()
			return nil, fmt.Errorf("vsock handshake: %w", err)
		}
		line += string(b)
	}
	if !strings.HasPrefix(line, "OK ") {
		conn.Close()
		return nil, fmt.Errorf("vsock handshake: %q", strings.TrimSpace(line))
	}
	conn.SetDeadline(time.Time{})
	return conn, nil
}

// CompactMemory turns the memory file a Snapshot wrote at tmp into the one a
// restore reads at dst: a sparse copy holding only what the guest was using
// (see sparseCopy), synced. When the copy cannot be made the original is synced
// and published instead, and only disk space is lost. Deleting the original
// unsynced drops its zeros before they reach the disk.
func CompactMemory(tmp, dst string) error {
	sparse := dst + ".sparse"
	if _, err := sparseCopy(tmp, sparse); err == nil {
		os.Remove(tmp)
		return os.Rename(sparse, dst)
	}
	if err := SyncFile(tmp); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// SyncFile fsyncs path.
func SyncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
