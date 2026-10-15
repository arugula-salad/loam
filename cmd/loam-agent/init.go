package main

import (
	"log"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/arugula-salad/loam/internal/agent"
	"github.com/arugula-salad/loam/internal/nodeapi"
)

// bootParams returns the loam.* parameters on the kernel command line
// (docs/AGENT.md, "Kernel command line").
func bootParams() map[string]string {
	out := map[string]string{}
	b, _ := os.ReadFile("/proc/cmdline")
	for _, f := range strings.Fields(string(b)) {
		if k, v, ok := strings.Cut(f, "="); ok && strings.HasPrefix(k, "loam.") {
			out[strings.TrimPrefix(k, "loam.")] = v
		}
	}
	return out
}

func mount(src, dst, fstype string, flags uintptr, data string) {
	os.MkdirAll(dst, 0o755)
	if err := unix.Mount(src, dst, fstype, flags, data); err != nil && err != unix.EBUSY {
		log.Printf("mount %s: %v", dst, err)
	}
}

// attachConsole points stdio at the serial console. The initramfs has no
// /dev/console node, so the kernel starts /init with nothing open.
func attachConsole() {
	f, err := os.OpenFile("/dev/console", os.O_RDWR, 0)
	if err != nil {
		return
	}
	for fd := 0; fd < 3; fd++ {
		unix.Dup2(int(f.Fd()), fd)
	}
}

// pivotToDisk mounts the sandbox's disk (loam.root) and makes it the root,
// the same dance as switch_root. PID 1 keeps running from the initramfs copy
// of this binary, so the disk never needs to contain it, and a disk restored
// from an old checkpoint still gets the agent the nodelet ships now.
func pivotToDisk() map[string]string {
	mount("proc", "/proc", "proc", 0, "")
	p := bootParams()
	unix.Unmount("/proc", 0)
	mount("devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID, "mode=0755")
	attachConsole()
	dev := p["root"]
	if dev == "" {
		log.Print("no loam.root on the kernel command line; staying on the initramfs")
		return p
	}
	os.MkdirAll("/newroot", 0o755)
	var err error
	for i := 0; i < 200; i++ { // the block device can show up a moment after /init starts
		if err = unix.Mount(dev, "/newroot", "ext4", 0, ""); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		log.Fatalf("mount %s: %v", dev, err)
	}
	if err := unix.Mount("/dev", "/newroot/dev", "", unix.MS_MOVE, ""); err != nil {
		log.Printf("move /dev: %v", err)
	}
	must := func(what string, err error) {
		if err != nil {
			log.Fatalf("%s: %v", what, err)
		}
	}
	must("chdir", unix.Chdir("/newroot"))
	must("move root", unix.Mount(".", "/", "", unix.MS_MOVE, ""))
	must("chroot", unix.Chroot("."))
	must("chdir /", unix.Chdir("/"))
	return p
}

func runInit() {
	const nsd = unix.MS_NOSUID | unix.MS_NODEV
	p := pivotToDisk()
	mount("proc", "/proc", "proc", nsd|unix.MS_NOEXEC, "")
	mount("sysfs", "/sys", "sysfs", nsd|unix.MS_NOEXEC, "")
	mount("devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID, "mode=0755")
	mount("devpts", "/dev/pts", "devpts", unix.MS_NOSUID|unix.MS_NOEXEC, "gid=5,mode=0620,ptmxmode=0666")
	mount("tmpfs", "/dev/shm", "tmpfs", nsd, "mode=1777")
	mount("tmpfs", "/run", "tmpfs", nsd, "mode=0755")
	mount("tmpfs", "/tmp", "tmpfs", nsd, "mode=1777")
	mount("cgroup2", "/sys/fs/cgroup", "cgroup2", nsd|unix.MS_NOEXEC, "")
	for _, l := range [][2]string{{"/proc/self/fd", "/dev/fd"}, {"/proc/self/fd/0", "/dev/stdin"},
		{"/proc/self/fd/1", "/dev/stdout"}, {"/proc/self/fd/2", "/dev/stderr"}} {
		os.Symlink(l[0], l[1])
	}
	// /dev/ptmx must be the devpts one, so a pty opened through it lands in /dev/pts.
	os.Remove("/dev/ptmx")
	os.Symlink("pts/ptmx", "/dev/ptmx")

	// The wake sets the hostname again, and the clock; a hostname from the
	// command line just makes the first seconds of the boot less anonymous.
	h := p["hostname"]
	if h == "" {
		h = "loam"
	}
	unix.Sethostname([]byte(h))
	agent.WriteHostFiles(h)

	if err := agent.LoopbackUp(); err != nil {
		log.Printf("lo: %v", err)
	}
	if p["ip"] != "" {
		n := &nodeapi.WakeNetwork{IP: p["ip"], Gateway: p["gw"]}
		if p["dns"] != "" {
			n.DNS = strings.Split(p["dns"], ",")
		}
		if err := agent.ConfigureEth0(n); err != nil {
			log.Printf("eth0: %v", err)
		}
	}

	supervise([]string{"loam-agent", "serve"}, []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"})
}

// supervise runs the server (argv, env) and reaps everything, restarting the
// server whenever it is the one that died.
func supervise(argv, env []string) {
	for {
		proc, err := os.StartProcess("/proc/self/exe", argv, &os.ProcAttr{
			Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
			Env:   env,
		})
		if err != nil {
			log.Printf("start server: %v", err)
			time.Sleep(time.Second)
			continue
		}
		// Reap everything; orphans are reparented to us. Restart the server if it is the one that died.
		for {
			var ws unix.WaitStatus
			pid, err := unix.Wait4(-1, &ws, 0, nil)
			if err == unix.EINTR {
				continue
			}
			if err != nil || pid == proc.Pid {
				break
			}
		}
		log.Printf("server exited; restarting")
		time.Sleep(200 * time.Millisecond)
	}
}
