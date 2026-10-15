// loam-nodelet runs the Firecracker VMs of one node and serves the node
// protocol (docs/NODE.md) to loam-api. It runs as a DaemonSet pod with
// /dev/kvm, /dev/net/tun, NET_ADMIN and a hostPath for --data; the VMs'
// network lives in the pod's own network namespace.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/internal/nodelet"
	"github.com/arugula-salad/loam/internal/nodelet/broker"
	"github.com/arugula-salad/loam/internal/nodelet/cgroup"
	"github.com/arugula-salad/loam/internal/nodelet/firecracker"
	"github.com/arugula-salad/loam/internal/nodelet/network"
	"github.com/arugula-salad/loam/internal/snapstore"
)

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	// The UFFD helper: this binary again, one per VM whose memory comes from
	// the store (internal/nodelet/pager.go).
	if len(os.Args) > 1 && os.Args[1] == "uffd-serve" {
		if err := nodelet.RunPager(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "loam-nodelet uffd-serve:", err)
			os.Exit(1)
		}
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("loam-nodelet", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	var (
		data        = flag.String("data", "/var/lib/loam", "data directory (VMs, snapshots, image cache); must support reflinks")
		memory      = flag.Int("memory", 0, "MiB this node hands to running VMs (default: three quarters of the node's RAM)")
		nodeName    = flag.String("node-name", os.Getenv("NODE_NAME"), "this node's name (default $NODE_NAME)")
		tokenFile   = flag.String("token-file", "", "file holding the node token (default: $LOAM_NODE_TOKEN)")
		apiURL      = flag.String("api-url", "", "loam-api's base URL, for the guests' host channel (e.g. http://loam-api.loam.svc:8080)")
		dnsUpstream = flag.String("dns-upstream", "1.1.1.1,9.9.9.9", "public resolvers the VMs' DNS forwarder asks")
		kernel      = flag.String("kernel", "/usr/share/loam/vmlinux", "guest kernel")
		initramfs   = flag.String("initramfs", "/usr/share/loam/initramfs.cpio.gz", "the agent's initramfs")
		fcBin       = flag.String("firecracker", "/usr/local/bin/firecracker", "Firecracker binary")
		listen      = flag.String("listen", ":"+strconv.Itoa(nodeapi.Port), "address for the node protocol")
		cgroups     = flag.Bool("cgroups", true, "run each Firecracker in its own cgroup with limits, where the pod allows it (a writable cgroup2, or CAP_SYS_ADMIN in a private cgroup namespace); without, VMs share the pod's cgroup")
		drain       = flag.Duration("drain-timeout", 280*time.Second, "on SIGTERM, how long to spend pausing running VMs (keep it under the pod's termination grace period)")
		cacheAge    = flag.Duration("image-cache-max-age", 7*24*time.Hour, "drop a cached template image unused for this long")
		cacheGiB    = flag.Int("image-cache-max-gib", 20, "keep the template image cache under this many GiB on disk, dropping the least recently used")
		devCopy     = flag.Bool("dev-allow-copy", false, "development only: allow a data directory without reflinks (every clone a full copy)")
		storeURL    = flag.String("store-url", os.Getenv("LOAM_STORE_URL"), "the snapshot store (default $LOAM_STORE_URL): s3://bucket?endpoint=...&s3ForcePathStyle=true&region=..., credentials in $AWS_ACCESS_KEY_ID and $AWS_SECRET_ACCESS_KEY; or file:///dir")
		chunkAge    = flag.Duration("chunk-cache-max-age", 7*24*time.Hour, "drop a cached chunk of the snapshot store unused for this long")
		brokerCA    = flag.String("broker-ca", os.Getenv("LOAM_BROKER_CA"), "directory holding the broker CA's tls.crt and tls.key (the Secret loam-broker-ca; default $LOAM_BROKER_CA); without it, VMs with secrets are refused")
		chunkGiB    = flag.Int("chunk-cache-max-gib", 20, "keep the snapshot store's chunk cache under this many GiB, dropping the least recently used")
	)
	flag.Parse()
	if *nodeName == "" {
		return errors.New("--node-name (or $NODE_NAME) is required")
	}
	if *apiURL == "" {
		log.Warn("no --api-url: guests' host channel requests will be answered 503")
	}
	token := os.Getenv("LOAM_NODE_TOKEN")
	os.Unsetenv("LOAM_NODE_TOKEN") // not for the processes the nodelet starts
	if *tokenFile != "" {
		tok, err := os.ReadFile(*tokenFile)
		if err != nil {
			return fmt.Errorf("node token: %w", err)
		}
		token = string(tok)
	}
	if token = strings.TrimSpace(token); token == "" {
		return errors.New("no node token: set $LOAM_NODE_TOKEN or --token-file")
	}
	for _, f := range []string{*kernel, *initramfs, *fcBin} {
		if _, err := os.Stat(f); err != nil {
			return err
		}
	}
	if *memory == 0 {
		*memory = memTotalMiB() * 3 / 4
	}
	if *memory <= 0 {
		return errors.New("--memory is required where /proc/meminfo can't say")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	host := firecracker.Host{Firecracker: *fcBin, Kernel: *kernel, Initramfs: *initramfs}
	var cgRoot *cgroup.Root
	// Best effort: an unprivileged pod can't make cgroups, and the pod is the
	// jail in phase 1 (docs/NODE.md, "Confinement").
	switch {
	case !*cgroups:
		log.Warn("per-VM cgroups off (--cgroups=false): every VM shares the nodelet's cgroup, bounded only by the pod's limits")
	default:
		var err error
		if cgRoot, err = cgroup.Open(""); err != nil {
			log.Warn("per-VM cgroups unavailable: every VM shares the nodelet's cgroup, bounded only by the pod's limits", "reason", err)
			cgRoot = nil
		} else {
			host.Cgroups = cgRoot
			log.Info("per-VM cgroups on")
		}
	}

	enforcer := network.NewEnforcer(log)
	nw := &network.Network{Run: network.Exec, Enforcer: enforcer}
	if err := nw.Setup(ctx); err != nil {
		return fmt.Errorf("VM network: %w", err)
	}
	upstreams := network.Upstreams(*dnsUpstream)
	if len(upstreams) == 0 {
		return errors.New("--dns-upstream names no resolver")
	}
	dns := &network.DNS{Enforcer: enforcer, Upstreams: upstreams, Log: log}
	if _, stopDNS, err := dns.Start(net.JoinHostPort(network.Gateway.String(), "53")); err != nil {
		return fmt.Errorf("DNS forwarder: %w", err)
	} else {
		defer stopDNS()
	}
	// The egress proxy (docs/NODE.md, "Phase 3: the network boundary"), which
	// the nftables rules send restricted VMs' TCP to.
	proxy := network.NewProxy(enforcer, upstreams, log)
	caPEM := ""
	if *brokerCA != "" {
		ca, err := broker.LoadCA(*brokerCA)
		if err != nil {
			return fmt.Errorf("--broker-ca: %w", err)
		}
		proxy.Broker = &broker.Broker{CA: ca, Log: log, Dial: proxy.DialName}
		caPEM = ca.PEM()
		log.Info("brokering secrets with the broker CA", "dir", *brokerCA)
	} else {
		log.Warn("no --broker-ca: VMs with secrets are refused")
	}
	proxyLn, err := net.Listen("tcp4", net.JoinHostPort(network.Gateway.String(), strconv.Itoa(network.ProxyPort)))
	if err != nil {
		return fmt.Errorf("egress proxy: %w", err)
	}
	defer proxyLn.Close()
	go proxy.Serve(proxyLn)

	images, err := nodelet.NewImageCache(*data+"/images", log)
	if err != nil {
		return err
	}
	if *storeURL == "" {
		return errors.New("no snapshot store: set $LOAM_STORE_URL or --store-url")
	}
	// The chunk cache is on the data volume, shared with the pagers.
	cacheDir := *data + "/store"
	store, err := snapstore.Open(*storeURL, snapstore.Options{CacheDir: cacheDir})
	if err != nil {
		return fmt.Errorf("snapshot store: %w", err)
	}
	defer store.Close()
	pagers := &nodelet.ProcessPagers{Command: nodelet.SelfPager, StoreURL: *storeURL, CacheDir: cacheDir, Log: log}
	n, err := nodelet.New(nodelet.Options{Data: *data, MemoryMiB: *memory, NodeName: *nodeName, Version: version,
		Token: token, APIURL: *apiURL, DevAllowCopy: *devCopy, Store: store, Pagers: pagers, BrokerCA: caPEM, Log: log},
		nodelet.Firecracker{Host: host}, nw, images)
	if err != nil {
		return err
	}
	go func() {
		for {
			images.Evict(*cacheAge, int64(*cacheGiB)<<30)
			if removed, left, err := store.Evict(*chunkAge, int64(*chunkGiB)<<30); err != nil {
				log.Warn("chunk cache eviction", "err", err)
			} else if removed > 0 {
				log.Info("chunk cache evicted", "removed", removed, "left_mib", left>>20)
			}
			// Local copies of snapshots deleted through another node while
			// this one was away (docs/NODE.md, "Local copies").
			if err := n.ReconcileStored(ctx); err != nil {
				log.Warn("could not check the local snapshots against the store", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Hour):
			}
		}
	}()
	// Only now: a leaf that still held an orphaned VMM could not be removed,
	// and New reaped them.
	if cgRoot != nil {
		cgRoot.Sweep()
	}
	if *devCopy {
		log.Warn("--dev-allow-copy is set; this nodelet is not fit for production")
	}

	srv := &http.Server{Addr: *listen, Handler: n.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("loam-nodelet serving", "addr", *listen, "node", *nodeName, "version", version, "memory_mib", *memory, "data", *data)
	select {
	case err := <-errc:
		n.Shutdown()
		return err
	case <-ctx.Done():
	}
	// Pause what runs, so that a roll or a drain loses no awake sandbox. The
	// server keeps answering meanwhile: loam-api sees the VMs go paused.
	log.Info("draining: pausing running VMs and storing their snapshots", "timeout", *drain)
	dctx, cancel := context.WithTimeout(context.Background(), *drain)
	defer cancel()
	n.Drain(dctx)
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	srv.Shutdown(sctx)
	return nil
}

func memTotalMiB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	for sc := bufio.NewScanner(f); sc.Scan(); {
		if fields := strings.Fields(sc.Text()); len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, _ := strconv.Atoi(fields[1])
			return kb / 1024
		}
	}
	return 0
}
