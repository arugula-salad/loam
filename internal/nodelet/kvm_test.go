//go:build kvm

package nodelet

// The VM mechanics on real Firecracker with loam's agent: boot from an
// image, pause, resume, snapshot, restore, fork, stop and a fresh boot, the
// network rules and the host channel. It needs root, /dev/kvm and
// /dev/net/tun, and changes the network namespace it runs in, so it runs
// only inside the nodelet's own image, never on a host: scripts/kvm-test.sh.
// Environment:
//
//	LOAM_KVM_KERNEL, LOAM_KVM_INITRAMFS, LOAM_KVM_FIRECRACKER  default: the image's paths
//	LOAM_KVM_IMAGE      the template image (default docker.io/library/ubuntu:24.04)
//	LOAM_KVM_IMAGE_TAR  a `docker save` of the template instead (images/guest's
//	                    loam-guest:dev), served from a registry inside the test
//	LOAM_KVM_DATA       the data directory (default a temporary one; reflinks if it has them)

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/internal/nodelet/broker"
	"github.com/arugula-salad/loam/internal/nodelet/cgroup"
	"github.com/arugula-salad/loam/internal/nodelet/firecracker"
	"github.com/arugula-salad/loam/internal/nodelet/network"
	"github.com/arugula-salad/loam/internal/snapstore"
	"github.com/arugula-salad/loam/pkg/api"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/gorilla/websocket"
)

// kvmBrokerCA is a broker CA made for the test, as loam-broker-ca would be.
func kvmBrokerCA() (*broker.CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "loam broker CA (kvm test)"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return broker.NewCA(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}))
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// kvm is what every KVM test shares: the VMs' network (one bridge, one DNS
// forwarder), Firecracker, and the template image. Made once per process.
var kvm struct {
	once  sync.Once
	err   error
	host  firecracker.Host
	nw    *network.Network
	proxy *network.Proxy
	caPEM string
	image string
	log   *slog.Logger
}

func kvmSetup(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("needs root in a container: scripts/kvm-test.sh")
	}
	kvm.once.Do(func() {
		kvm.log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
		ctx := context.Background()
		kvm.host = firecracker.Host{
			Firecracker: env("LOAM_KVM_FIRECRACKER", "/usr/local/bin/firecracker"),
			Kernel:      env("LOAM_KVM_KERNEL", "/usr/share/loam/vmlinux"),
			Initramfs:   env("LOAM_KVM_INITRAMFS", "/usr/share/loam/initramfs.cpio.gz"),
		}
		if cg, err := cgroup.Open(""); err != nil {
			kvm.log.Info("per-VM cgroups unavailable, running without", "err", err)
		} else {
			kvm.host.Cgroups = cg
		}
		enforcer := network.NewEnforcer(kvm.log)
		kvm.nw = &network.Network{Run: network.Exec, Enforcer: enforcer}
		if kvm.err = kvm.nw.Setup(ctx); kvm.err != nil {
			return
		}
		upstreams := network.Upstreams("1.1.1.1,9.9.9.9")
		dns := &network.DNS{Enforcer: enforcer, Upstreams: upstreams, Log: kvm.log}
		if _, _, kvm.err = dns.Start(network.Gateway.String() + ":53"); kvm.err != nil {
			return
		}
		kvm.proxy = network.NewProxy(enforcer, upstreams, kvm.log)
		ln, err := net.Listen("tcp4", fmt.Sprintf("%s:%d", network.Gateway, network.ProxyPort))
		if err != nil {
			kvm.err = err
			return
		}
		go kvm.proxy.Serve(ln)
		var ca *broker.CA
		if ca, kvm.err = kvmBrokerCA(); kvm.err != nil {
			return
		}
		kvm.proxy.Broker = &broker.Broker{CA: ca, Log: kvm.log, Dial: kvm.proxy.DialName}
		kvm.caPEM = ca.PEM()
		kvm.image = env("LOAM_KVM_IMAGE", "docker.io/library/ubuntu:24.04")
		if tarPath := os.Getenv("LOAM_KVM_IMAGE_TAR"); tarPath != "" {
			reg := httptest.NewServer(registry.New(registry.Logger(stdlog.New(io.Discard, "", 0))))
			img, err := tarball.ImageFromPath(tarPath, nil)
			if err != nil {
				kvm.err = err
				return
			}
			kvm.image = strings.TrimPrefix(reg.URL, "http://") + "/loam/guest:dev"
			ref, _ := name.ParseReference(kvm.image)
			kvm.err = remote.Write(ref, img)
		}
	})
	if kvm.err != nil {
		t.Fatal(kvm.err)
	}
}

// TestMain runs the pager when the nodelet under test starts one (the test
// binary stands in for loam-nodelet's uffd-serve).
func TestMain(m *testing.M) {
	if os.Getenv("LOAM_KVM_PAGER") == "1" {
		if err := RunPager(os.Args[1:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "uffd-serve:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func selfPager(args []string) *exec.Cmd {
	cmd := exec.Command("/proc/self/exe", args...)
	cmd.Env = append(os.Environ(), "LOAM_KVM_PAGER=1")
	return cmd
}

// kvmNode is a nodelet called name on data, with the store at storeURL.
func kvmNode(t *testing.T, name, data, storeURL, apiURL string) (*Nodelet, *nodeapi.Client, *snapstore.Store) {
	t.Helper()
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	reflinks := ProbeReflink(data)
	t.Logf("node %s: data %s, reflinks %v", name, data, reflinks)
	images, err := NewImageCache(filepath.Join(data, "images"), kvm.log)
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(data, "store")
	st, err := snapstore.Open(storeURL, snapstore.Options{CacheDir: cache})
	if err != nil {
		t.Fatal(err)
	}
	n, err := New(Options{Data: data, MemoryMiB: 4096, NodeName: name, Token: "tok", APIURL: apiURL,
		DevAllowCopy: !reflinks, Store: st, BrokerCA: kvm.caPEM, Pagers: &ProcessPagers{Command: selfPager, StoreURL: storeURL, CacheDir: cache},
		Log: kvm.log.With("node", name)}, Firecracker{Host: kvm.host}, kvm.nw, images)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(n.Handler())
	t.Cleanup(func() { srv.Close(); n.Shutdown(); st.Close() })
	return n, &nodeapi.Client{Addr: strings.TrimPrefix(srv.URL, "http://"), Token: "tok"}, st
}

func TestKVM(t *testing.T) {
	kvmSetup(t)
	ctx := context.Background()
	data := env("LOAM_KVM_DATA", "")
	if data == "" {
		data = t.TempDir()
	}
	host := kvm.host
	// loam-api, as the host channel sees it.
	guestCalls := make(chan *http.Request, 8)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		guestCalls <- r
		io.WriteString(w, `{"ok":true}`)
	}))
	defer apiSrv.Close()
	_, cl, _ := kvmNode(t, "kvm", filepath.Join(data, "single"), "file://"+filepath.Join(data, "single-store"), apiSrv.URL)
	srv := struct{ URL string }{"http://" + cl.Addr}

	// agent makes one request to VM id's agent through the node protocol's agent stream.
	agent := func(id, method, path, body string) (int, string) {
		t.Helper()
		hc := &http.Client{Transport: nodeapi.AgentTransport(func(ctx context.Context) (net.Conn, error) { return cl.DialAgent(ctx, id) }), Timeout: 30 * time.Second}
		req, _ := http.NewRequest(method, "http://agent"+path, strings.NewReader(body))
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("%s %s on %s: %v", method, path, id, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	read := func(id, path string) string {
		t.Helper()
		code, b := agent(id, "GET", "/fs/file?path="+url.QueryEscape(path), "")
		if code != 200 {
			return fmt.Sprintf("<%d>", code)
		}
		return b
	}
	write := func(id, path, body string) {
		t.Helper()
		if code, b := agent(id, "PUT", "/fs/file?mkdir=true&path="+url.QueryEscape(path), body); code != 200 {
			t.Fatalf("write %s on %s: %d %s", path, id, code, b)
		}
	}
	// run runs a command in VM id as root and returns its exit code and output.
	run := func(id string, cmd ...string) (int, string) {
		t.Helper()
		body, _ := json.Marshal(api.ExecRun{Cmd: cmd, User: "root", TimeoutS: 20})
		code, b := agent(id, "POST", "/exec/run", string(body))
		var res api.ExecResult
		if code != 200 || json.Unmarshal([]byte(b), &res) != nil {
			t.Fatalf("exec %v on %s: %d %s", cmd, id, code, b)
		}
		return res.ExitCode, string(res.Stdout) + string(res.Stderr)
	}
	timed := func(what string, f func() (nodeapi.VM, error)) nodeapi.VM {
		t.Helper()
		start := time.Now()
		v, err := f()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		t.Logf("%-28s %-8s %v", what, v.State, time.Since(start).Round(time.Millisecond))
		return v
	}
	wake := func(id string) nodeapi.Wake {
		return nodeapi.Wake{Sandbox: id, Name: id, Hostname: id, User: "root", Env: map[string]string{"FOO": "bar"}}
	}

	image := kvm.image
	res := api.Resources{CPUs: 2, MemoryMiB: 1024, DiskGiB: 4}
	v := timed("create (pull + build + boot)", func() (nodeapi.VM, error) {
		return cl.Create(ctx, nodeapi.CreateVM{ID: "sb-kvm1", Template: nodeapi.Template{Name: "base", Image: image}, Resources: res, Wake: wake("sb-kvm1")})
	})
	if v.State != nodeapi.VMRunning || v.IP != "10.240.0.2" {
		t.Fatalf("created %+v", v)
	}
	if code, out := run("sb-kvm1", "sh", "-c", "hostname; echo $FOO; cat /proc/cmdline"); code != 0 || !strings.HasPrefix(out, "sb-kvm1\nbar\n") {
		t.Fatalf("exec: %d %q", code, out)
	}
	if host.Cgroups != nil {
		// In a private cgroup namespace the nodelet's subtree is the root.
		procs, err := os.ReadFile("/sys/fs/cgroup/vms/sb-kvm1/cgroup.procs")
		max, _ := os.ReadFile("/sys/fs/cgroup/vms/sb-kvm1/memory.max")
		if err != nil || len(strings.TrimSpace(string(procs))) == 0 {
			t.Errorf("the VM's Firecracker is not in its cgroup: %q %v", procs, err)
		}
		t.Logf("VM cgroup: pids %s, memory.max %s", strings.TrimSpace(string(procs)), strings.TrimSpace(string(max)))
	}
	write("sb-kvm1", "/run/mem-marker", "in memory") // /run is a tmpfs in the guest
	write("sb-kvm1", "/root/disk-marker", "on disk")

	t.Run("guest uid firewall", func(t *testing.T) {
		code, out := run("sb-kvm1", "python3", "-c", `import errno, os, socket, subprocess, sys
sockets = []
for family, host in [(socket.AF_INET, "127.0.0.1"), (socket.AF_INET6, "::1")]:
    for port in [881, 1881]:
        s = socket.socket(family)
        if family == socket.AF_INET6: s.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 1)
        s.bind((host, port)); s.listen(16); sockets.append(s)
rule = 'table inet loam_uid_test { chain out { type filter hook output priority 0; policy accept; oif "lo" meta skuid 1002 meta l4proto tcp tcp dport { 1-1023 } reject with tcp reset; }; }\n'
subprocess.run(["nft", "-f", "-"], input=rule, text=True, check=True)
def probe(uid, host, port, blocked):
    child = os.fork()
    if child == 0:
        os.setgid(uid); os.setuid(uid)
        try:
            s = socket.create_connection((host, port), 2); s.close()
            os._exit(1 if blocked else 0)
        except OSError as e:
            os._exit(0 if blocked and e.errno == errno.ECONNREFUSED else 1)
    _, status = os.waitpid(child, 0)
    assert os.waitstatus_to_exitcode(status) == 0, (uid, host, port, blocked)
try:
    for host in ["127.0.0.1", "::1"]:
        probe(1002, host, 881, True)
        probe(1000, host, 881, False)
        probe(1002, host, 1881, False)
finally:
    subprocess.run(["nft", "delete", "table", "inet", "loam_uid_test"], check=True)
for host in ["127.0.0.1", "::1"]: probe(1002, host, 881, False)
print("uid firewall: IPv4/IPv6 rejection, other users/ports, and removal passed")
`)
		if code != 0 {
			t.Fatalf("guest uid firewall: %d %s", code, out)
		}
		t.Log(out)
	})

	t.Run("network", func(t *testing.T) {
		reach := func(host, port string) bool {
			code, _ := run("sb-kvm1", "timeout", "5", "bash", "-c", "exec 3<>/dev/tcp/"+host+"/"+port)
			return code == 0
		}
		dial := func(host, port string) int {
			if reach(host, port) {
				return http.StatusSwitchingProtocols
			}
			return http.StatusBadGateway
		}
		if code := dial("1.1.1.1", "443"); code != http.StatusSwitchingProtocols {
			t.Errorf("the internet from the guest: %d", code)
		}
		if code := dial("one.one.one.one", "443"); code != http.StatusSwitchingProtocols {
			t.Errorf("a name from the guest, through the DNS forwarder: %d", code)
		}
		if code := dial(network.Gateway.String(), "7070"); code == http.StatusSwitchingProtocols {
			t.Error("the guest reached the nodelet's port on the gateway")
		}
		podIP := ""
		if addrs, err := net.InterfaceAddrs(); err == nil {
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && ipn.IP.String() != network.Gateway.String() {
					podIP = ipn.IP.String()
				}
			}
		}
		if podIP != "" {
			// Something listening on every address of the pod, which the
			// guest must not reach by the pod's own address.
			ln, err := net.Listen("tcp4", "0.0.0.0:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					c.Close()
				}
			}()
			port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
			if c, err := net.DialTimeout("tcp", podIP+":"+port, time.Second); err != nil {
				t.Fatalf("the listener isn't reachable at %s even from the pod: %v", podIP, err)
			} else {
				c.Close()
			}
			if code := dial(podIP, port); code == http.StatusSwitchingProtocols {
				t.Errorf("the guest reached the pod's own address %s", podIP)
			}
			if code := dial(network.Gateway.String(), port); code == http.StatusSwitchingProtocols {
				t.Errorf("the guest reached the pod on the gateway's address")
			}
		}
	})

	// docs/NODE.md, "Phase 3: the network boundary", on a running VM.
	t.Run("egress", func(t *testing.T) {
		ok := func(cmd string) bool {
			code, out := run("sb-kvm1", "timeout", "15", "bash", "-c", cmd)
			t.Logf("%s: exit %d %s", cmd, code, strings.TrimSpace(out))
			return code == 0
		}
		policy := nodeapi.Policy{Egress: []api.Rule{
			{Domain: "one.one.one.one", Action: api.RuleAllow, Ports: []int{80, 443, 853}},
		}}
		if err := cl.SetPolicy(ctx, "sb-kvm1", policy); err != nil {
			t.Fatal(err)
		}
		defer cl.SetPolicy(ctx, "sb-kvm1", nodeapi.Policy{})
		if !ok("curl -sS -o /dev/null https://one.one.one.one/") {
			t.Error("an allowed name over HTTPS")
		}
		if !ok("curl -sS -o /dev/null http://one.one.one.one/") {
			t.Error("an allowed name over HTTP")
		}
		if !ok("dig +time=3 +tries=1 example.com | grep -q 'status: REFUSED'") {
			t.Error("a name not allowed was not REFUSED")
		}
		if ok("curl -sS -o /dev/null --max-time 5 --resolve example.com:443:1.1.1.1 https://example.com/") {
			t.Error("a name not allowed, on an allowed name's address")
		}
		if !ok("curl -sS --max-time 5 -o /dev/null -w '%{http_code}' --resolve example.com:80:1.1.1.1 http://example.com/ | grep -q 403") {
			t.Error("a Host not allowed on 80 got no 403")
		}
		if !ok("exec 3<>/dev/tcp/one.one.one.one/853") {
			t.Error("a port a rule names, at an address the VM resolved")
		}
		// The proxy accepts every redirected connection and resets one it
		// refuses, so the refusal shows on the first read, or already at
		// connect when the reset beats the handshake's last ACK.
		if !ok("{ exec 3<>/dev/tcp/9.9.9.9/853 && sleep 1 && cat <&3; } 2>&1 | grep -q reset") {
			t.Error("a port a rule names, at an address the VM never resolved")
		}
		if ok("dig +time=2 +tries=1 @1.1.1.1 one.one.one.one") {
			t.Error("UDP left a restricted VM")
		}
		// From another VM's address: neither its packets nor ARP for that
		// address get past the tap.
		ok("ip addr add 10.240.0.99/16 dev eth0")
		if ok("dig +time=2 +tries=1 -b 10.240.0.99 @1.1.1.1 one.one.one.one") {
			t.Error("a restricted VM got out from an address not its own")
		}
		ok("ip addr del 10.240.0.99/16 dev eth0")

		if err := cl.SetPolicy(ctx, "sb-kvm1", nodeapi.Policy{}); err != nil {
			t.Fatal(err)
		}
		if !ok("dig +time=3 +tries=1 @1.1.1.1 example.com | grep -q 'status: NOERROR'") {
			t.Error("unrestricted again, UDP is still dropped")
		}
	})

	// A brokered secret, against an echo host on the internet. The guest
	// trusts the broker CA from its wake. Where the secret applies the echo
	// carries the value back, so the answer is cut (502); where it doesn't,
	// the placeholder goes as it is.
	t.Run("broker", func(t *testing.T) {
		const value = "loam-kvm-value-7a1b"
		pol := nodeapi.Policy{Secrets: []nodeapi.PolicySecret{{Name: "TOKEN", Value: value,
			Hosts: []string{"postman-echo.com/get"}, Placeholder: "__loam_token__"}}}
		if err := cl.SetPolicy(ctx, "sb-kvm1", pol); err != nil {
			t.Fatal(err)
		}
		defer cl.SetPolicy(ctx, "sb-kvm1", nodeapi.Policy{})
		// The agent installed the wake's root in the system bundle
		// (docs/AGENT.md, "The broker CA"), so curl trusts the broker as is.
		if got := read("sb-kvm1", "/etc/loam/ca.pem"); got != string(kvm.caPEM) {
			t.Fatalf("/etc/loam/ca.pem: %q", got)
		}
		code, out := run("sb-kvm1", "timeout", "20", "curl", "-sS", "-H", "X-T: __loam_token__", "https://postman-echo.com/headers")
		if code != 0 || !strings.Contains(out, "__loam_token__") || strings.Contains(out, value) {
			t.Errorf("outside the secret's path: %d %s", code, out)
		}
		code, out = run("sb-kvm1", "timeout", "20", "curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "-H", "X-T: __loam_token__", "https://postman-echo.com/get")
		if code != 0 || out != "502" {
			t.Errorf("the value echoed back: %d %q", code, out)
		}
		if _, env := run("sb-kvm1", "sh", "-c", "env; cat /proc/1/environ"); strings.Contains(env, value) {
			t.Error("the value is in the guest")
		}
	})

	t.Run("egress header rules", func(t *testing.T) {
		pol := nodeapi.Policy{RequestHeaders: []nodeapi.HeaderRule{{Domain: "postman-echo.com", Headers: map[string]string{"X-Loam-Transform": "loam-header-test-715"}}}}
		if err := cl.SetPolicy(ctx, "sb-kvm1", pol); err != nil {
			t.Fatal(err)
		}
		defer cl.SetPolicy(ctx, "sb-kvm1", nodeapi.Policy{})
		code, out := run("sb-kvm1", "timeout", "20", "curl", "-sS", "-H", "X-Loam-Transform: guest-value", "https://postman-echo.com/headers")
		if code != 0 || !strings.Contains(out, "loam-header-test-715") || strings.Contains(out, "guest-value") {
			t.Fatalf("header transform: %d %s", code, out)
		}
		pol.Sensitive = []string{"loam-header-test-715"}
		if err := cl.SetPolicy(ctx, "sb-kvm1", pol); err != nil {
			t.Fatal(err)
		}
		code, out = run("sb-kvm1", "timeout", "20", "curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "https://postman-echo.com/headers")
		if code != 0 || out != "502" {
			t.Fatalf("transformed secret reflection: %d %q", code, out)
		}
	})

	t.Run("host channel", func(t *testing.T) {
		// A process in the guest asks for its sandbox through the agent's API
		// socket, which goes out the host channel to "loam-api".
		code, out := run("sb-kvm1", "sh", "-c", "curl -s --unix-socket /run/loam/api.sock http://loam/self || python3 -c 'import socket;s=socket.socket(socket.AF_UNIX);s.connect(\"/run/loam/api.sock\");s.sendall(b\"GET /self HTTP/1.0\\r\\nHost: loam\\r\\n\\r\\n\");print(s.recv(4096).decode())'")
		if code != 0 || !strings.Contains(out, `{"ok":true}`) {
			t.Errorf("GET /self from the guest: %d %q", code, out)
		}
		select {
		case r := <-guestCalls:
			if r.Header.Get(nodeapi.HeaderSandbox) != "sb-kvm1" || r.URL.Path != "/internal/guest/self" || r.Header.Get("Authorization") != "Bearer tok" {
				t.Errorf("forwarded %s %s %v", r.Method, r.URL, r.Header)
			}
			t.Logf("the guest called loam-api: %s %s as %s", r.Method, r.URL.Path, r.Header.Get(nodeapi.HeaderSandbox))
		case <-time.After(5 * time.Second):
			t.Error("no request from the guest reached loam-api")
		}
	})

	timed("pause", func() (nodeapi.VM, error) { return cl.PauseInto(ctx, "sb-kvm1", "sb-kvm1.@1") })
	dir := filepath.Join(data, "single", "snapshots", "sb-kvm1.@1")
	t.Logf("pause snapshot: memory %d MiB allocated of %d MiB, state %d KiB",
		allocated(filepath.Join(dir, snapMem))>>20, res.MemoryMiB, allocated(filepath.Join(dir, snapState))>>10)
	if _, err := cl.DialAgent(ctx, "sb-kvm1"); !nodeapi.IsCode(err, api.CodeConflict) {
		t.Errorf("agent stream to a paused VM: %v", err)
	}
	timed("resume from pause", func() (nodeapi.VM, error) { return cl.Resume(ctx, "sb-kvm1", nodeapi.Resume{Wake: wake("sb-kvm1")}) })
	if got := read("sb-kvm1", "/run/mem-marker"); got != "in memory" {
		t.Errorf("after pause and resume, the guest's memory says %q", got)
	}

	snap := func(id string, mem bool) {
		timed("snapshot "+id, func() (nodeapi.VM, error) {
			s, err := cl.Snapshot(ctx, "sb-kvm1", nodeapi.CreateSnapshot{ID: id, Memory: mem})
			if err == nil {
				t.Logf("snapshot %s: %d MiB", id, s.SizeBytes>>20)
			}
			return nodeapi.VM{State: "-"}, err
		})
	}
	// Exec sockets open across a memory snapshot: the snapshot resets the
	// guest's vsock, and the nodelet cuts the streams it left half-open, so
	// each socket ends rather than hang. The session that isn't detachable
	// ends with its socket; the detachable one runs on and can be attached.
	execWS := func(query string) (*websocket.Conn, string) {
		t.Helper()
		d := websocket.Dialer{NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return cl.DialAgent(ctx, "sb-kvm1") }}
		ws, _, err := d.Dial("ws://agent/exec?"+query, nil)
		if err != nil {
			t.Fatalf("exec socket %s: %v", query, err)
		}
		var m api.ExecMessage
		if err := ws.ReadJSON(&m); err != nil || m.Type != "started" {
			t.Fatalf("exec socket %s: %+v %v", query, m, err)
		}
		return ws, m.Session
	}
	plain, plainID := execWS("cmd=sleep&cmd=600&user=root")
	detached, detachedID := execWS("cmd=sleep&cmd=600&user=root&detachable=true")
	snap("sb-kvm1.v1", true)
	for _, ws := range []*websocket.Conn{plain, detached} {
		ws.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, _, err := ws.ReadMessage(); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("an exec socket across a memory snapshot: %v, want it ended", err)
		}
		ws.Close()
	}
	if code, _ := run("sb-kvm1", "true"); code != 0 {
		t.Error("the VM did not run on after a memory snapshot")
	}
	var sessions struct {
		Sessions []struct {
			ID     string `json:"id"`
			Exited bool   `json:"exited"`
		} `json:"sessions"`
	}
	if code, b := agent("sb-kvm1", "GET", "/exec/sessions", ""); code != 200 || json.Unmarshal([]byte(b), &sessions) != nil {
		t.Fatalf("sessions: %d %s", code, b)
	}
	exited := map[string]bool{}
	for _, s := range sessions.Sessions {
		exited[s.ID] = s.Exited
	}
	if e, ok := exited[plainID]; ok && !e {
		t.Errorf("the session that isn't detachable outlived its socket: %+v", sessions)
	}
	if e, ok := exited[detachedID]; !ok || e {
		t.Errorf("the detachable session didn't outlive the snapshot: %+v", sessions)
	}
	run("sb-kvm1", "pkill", "sleep")
	write("sb-kvm1", "/run/mem-marker", "changed")
	write("sb-kvm1", "/root/disk-marker", "changed")
	snap("sb-kvm1.v2", false)

	timed("restore memory snapshot", func() (nodeapi.VM, error) {
		return cl.Restore(ctx, "sb-kvm1", nodeapi.Restore{Snapshot: "sb-kvm1.v1", Wake: wake("sb-kvm1")})
	})
	if got, disk := read("sb-kvm1", "/run/mem-marker"), read("sb-kvm1", "/root/disk-marker"); got != "in memory" || disk != "on disk" {
		t.Errorf("after restoring v1: memory %q, disk %q", got, disk)
	}
	timed("restore disk snapshot", func() (nodeapi.VM, error) {
		return cl.Restore(ctx, "sb-kvm1", nodeapi.Restore{Snapshot: "sb-kvm1.v2", Wake: wake("sb-kvm1")})
	})
	if got, disk := read("sb-kvm1", "/run/mem-marker"), read("sb-kvm1", "/root/disk-marker"); got == "changed" || disk != "changed" {
		t.Errorf("after restoring v2 (disk only, a fresh boot): memory %q, disk %q", got, disk)
	}

	fork := timed("fork from memory snapshot", func() (nodeapi.VM, error) {
		return cl.Create(ctx, nodeapi.CreateVM{ID: "sb-kvm2", From: &nodeapi.FromSnapshot{Snapshot: "sb-kvm1.v1"}, Wake: wake("sb-kvm2")})
	})
	if got := read("sb-kvm2", "/run/mem-marker"); got != "in memory" || fork.IP == v.IP {
		t.Errorf("fork: memory %q, IP %s", got, fork.IP)
	}
	for _, id := range []string{"sb-kvm1", "sb-kvm2"} {
		if got := read(id, "/run/e2b/.E2B_SANDBOX_ID"); got != id {
			t.Errorf("%s identity: %q", id, got)
		}
	}
	// The fork's memory had sb-kvm1's address; the wake moved it to its own.
	if code, out := run("sb-kvm2", "ip", "-4", "-o", "addr", "show", "eth0"); code != 0 || !strings.Contains(out, fork.IP+"/16") {
		t.Errorf("the fork's eth0: %q, want %s", out, fork.IP)
	}
	if !t.Run("fork network", func(t *testing.T) {
		if code, out := run("sb-kvm2", "timeout", "5", "bash", "-c", "exec 3<>/dev/tcp/1.1.1.1/443"); code != 0 {
			t.Errorf("the fork can't reach the internet: %q", out)
		}
	}) {
		t.Log("fork network failed")
	}

	timed("stop", func() (nodeapi.VM, error) { return cl.StopInto(ctx, "sb-kvm1", "sb-kvm1.@2") })
	timed("resume from stopped", func() (nodeapi.VM, error) { return cl.Resume(ctx, "sb-kvm1", nodeapi.Resume{Wake: wake("sb-kvm1")}) })
	if got, disk := read("sb-kvm1", "/run/mem-marker"), read("sb-kvm1", "/root/disk-marker"); !strings.HasPrefix(got, "<") || disk != "changed" {
		t.Errorf("after a fresh boot: memory %q, disk %q", got, disk)
	}

	// A guest that powers itself off is reported stopped on the event stream.
	t.Run("events", func(t *testing.T) {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/events", nil)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		agent("sb-kvm2", "POST", "/internal/poweroff", "")
		lines := make(chan string, 4)
		go func() {
			br := bufio.NewReader(resp.Body)
			for {
				l, err := br.ReadString('\n')
				if err != nil {
					return
				}
				lines <- strings.TrimSpace(l)
			}
		}()
		deadline := time.After(20 * time.Second)
		for {
			select {
			case l := <-lines:
				if strings.Contains(l, `"id":"sb-kvm2"`) && strings.Contains(l, `"state":"stopped"`) {
					t.Logf("event: %s", l)
					return
				}
			case <-deadline:
				t.Fatal("no event for a guest that powered off")
			}
		}
	})

	if err := cl.Delete(ctx, "sb-kvm1"); err != nil {
		t.Error(err)
	}
	if err := cl.Delete(ctx, "sb-kvm2"); err != nil {
		t.Error(err)
	}
	if node, _ := cl.Node(ctx); node.MemoryMiBAllocated != 0 || node.VMs != 0 {
		t.Errorf("node at the end: %+v", node)
	}
	snaps, _ := cl.Snapshots(ctx)
	t.Logf("%d snapshots outlive their VM", len(snaps))
}
