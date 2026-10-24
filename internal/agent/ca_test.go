package agent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

func testRoot(t *testing.T, cn string) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

const systemRoots = "# a system root\n-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWithCA(t *testing.T) {
	block := caMarker + "\nNEW\n" + caMarker + "\n"
	for _, c := range []struct{ name, in, want string }{
		{"empty", "", block},
		{"appended", "A\n", "A\n" + block},
		{"no final newline", "A", "A\n" + block},
		{"replaced in place", "A\n" + caMarker + "\nOLD\n" + caMarker + "\nB\n", "A\n" + block + "B\n"},
		{"CRLF markers", "A\r\n" + caMarker + "\r\nOLD\r\n" + caMarker + "\r\nB\r\n", "A\r\n" + block + "B\r\n"},
		{"a second block goes", caMarker + "\nOLD\n" + caMarker + "\nB\n" + caMarker + "\nOLDER\n" + caMarker + "\n", block + "B\n"},
		{"an open marker runs to the end", "A\n" + caMarker + "\nOLD\nB\n", "A\n" + block},
		{"already there", "A\n" + block, "A\n" + block},
	} {
		if got := string(withCA([]byte(c.in), []byte("NEW\n"))); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestWakeInstallsCA(t *testing.T) {
	h := newHarness(t)
	root := h.srv.cfg.TrustRoot
	debian := filepath.Join(root, "etc/ssl/certs/ca-certificates.crt")
	mustWrite(t, debian, systemRoots, 0o644)
	// Alpine's /etc/ssl/cert.pem is a link to the Debian-named bundle: one file, one root.
	if err := os.Symlink("certs/ca-certificates.crt", filepath.Join(root, "etc/ssl/cert.pem")); err != nil {
		t.Fatal(err)
	}

	// Before any CA, no process gets its variables.
	if env := h.env(); env["NODE_EXTRA_CA_CERTS"] != "" || env["SSL_CERT_FILE"] != "" {
		t.Fatalf("env without a CA: %v", env)
	}

	first := testRoot(t, "loam broker one")
	h.wantStatus(h.do("POST", "/internal/wake", nodeapi.Wake{Sandbox: "sb-x", CA: first}, nil), 204, "")
	if got := read(t, filepath.Join(root, caFile)); got != first {
		t.Fatalf("ca.pem: %q", got)
	}
	want := systemRoots + caMarker + "\n" + first + caMarker + "\n"
	if got := read(t, debian); got != want {
		t.Fatalf("bundle:\n%s\nwant:\n%s", got, want)
	}
	if fi, err := os.Lstat(filepath.Join(root, "etc/ssl/cert.pem")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced by a file: %v %v", fi, err)
	}
	if fi, _ := os.Stat(debian); fi.Mode().Perm() != 0o644 {
		t.Fatalf("bundle mode %o", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(root, "etc/pki")); !os.IsNotExist(err) {
		t.Fatalf("a bundle the disk didn't have was made: %v", err)
	}

	// Waking again with the same root changes nothing, not even the file.
	before, _ := os.Stat(debian)
	time.Sleep(10 * time.Millisecond)
	h.wantStatus(h.do("POST", "/internal/wake", nodeapi.Wake{Sandbox: "sb-x", CA: first}, nil), 204, "")
	if after, _ := os.Stat(debian); !after.ModTime().Equal(before.ModTime()) || read(t, debian) != want {
		t.Fatal("an unchanged root rewrote the bundle")
	}

	// A package update appends after it; a new root replaces the old where it is.
	os.WriteFile(debian, []byte(read(t, debian)+"# added later\n"), 0o644)
	second := testRoot(t, "loam broker two")
	h.wantStatus(h.do("POST", "/internal/wake", nodeapi.Wake{Sandbox: "sb-x", CA: second}, nil), 204, "")
	got := read(t, debian)
	if want := systemRoots + caMarker + "\n" + second + caMarker + "\n# added later\n"; got != want {
		t.Fatalf("bundle after a new root:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, strings.TrimSpace(first)) || read(t, filepath.Join(root, caFile)) != second {
		t.Fatal("the old root is still there")
	}

	// A wake without a CA removes nothing.
	h.wantStatus(h.do("POST", "/internal/wake", nodeapi.Wake{Sandbox: "sb-x"}, nil), 204, "")
	if read(t, debian) != got {
		t.Fatal("a wake without a CA changed the bundle")
	}

	// Something that isn't a certificate is refused, and nothing changes.
	for _, bad := range []string{"not pem", "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n",
		"-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n", second + "trailing junk"} {
		h.wantStatus(h.do("POST", "/internal/wake", nodeapi.Wake{Sandbox: "sb-x", CA: bad}, nil), 400, api.CodeInvalid)
	}
	if read(t, debian) != got {
		t.Fatal("a refused CA changed the bundle")
	}
}

// E2B applications discover this path independently of envd's environment.
func TestWakeInstallsE2BCA(t *testing.T) {
	h := newHarness(t)
	path := filepath.Join(h.srv.cfg.TrustRoot, "/usr/local/share/ca-certificates/e2b-ca.crt")
	first := testRoot(t, "first")
	second := testRoot(t, "second")
	for _, cert := range []string{first, first, second} {
		h.wantStatus(h.do("POST", "/internal/wake", nodeapi.Wake{Sandbox: "sb-x", CA: cert}, nil), 204, "")
		if got := read(t, path); got != cert {
			t.Fatalf("E2B CA: %q, want %q", got, cert)
		}
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o644 {
			t.Fatalf("E2B CA permissions: %v %v", fi, err)
		}
	}
	// Repair a removed certificate on the next wake, without any secrets.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	h.wantStatus(h.do("POST", "/internal/wake", nodeapi.Wake{Sandbox: "sb-x", CA: second}, nil), 204, "")
	h.wantStatus(h.do("POST", "/internal/wake", nodeapi.Wake{Sandbox: "sb-x"}, nil), 204, "")
	h.wantStatus(h.do("POST", "/internal/wake", nodeapi.Wake{Sandbox: "sb-x", CA: "invalid"}, nil), 400, api.CodeInvalid)
	if got := read(t, path); got != second {
		t.Fatalf("E2B CA changed: %q", got)
	}
}

// The CA's variables sit beneath the image's, the sandbox's and the call's.
func TestCAEnvOrder(t *testing.T) {
	h := newHarness(t)
	root := h.srv.cfg.TrustRoot
	// Only the Fedora bundle: it is the one the variables name.
	fedora := filepath.Join(root, "etc/pki/tls/certs/ca-bundle.crt")
	mustWrite(t, fedora, systemRoots, 0o644)
	mustWrite(t, h.state+"/image.json", `{"env":["SSL_CERT_FILE=/from/image","CARGO_HTTP_CAINFO=/from/image"]}`, 0o644)
	h.wantStatus(h.do("POST", "/internal/wake", nodeapi.Wake{Sandbox: "sb-x", CA: testRoot(t, "r"),
		Env: map[string]string{"CARGO_HTTP_CAINFO": "/from/sandbox", "UV_NATIVE_TLS": "0"}}, nil), 204, "")
	env := h.env("UV_NATIVE_TLS=")
	want := map[string]string{
		"NODE_EXTRA_CA_CERTS": filepath.Join(root, caFile),
		"REQUESTS_CA_BUNDLE":  fedora,
		"SSL_CERT_FILE":       "/from/image",
		"CARGO_HTTP_CAINFO":   "/from/sandbox",
		"UV_NATIVE_TLS":       "",
	}
	for k, v := range want {
		if got, ok := env[k]; !ok || got != v {
			t.Errorf("%s=%q (set %v), want %q", k, got, ok, v)
		}
	}

	// Without a bundle there is only the file of its own.
	os.Remove(fedora)
	env = h.env()
	if env["NODE_EXTRA_CA_CERTS"] != filepath.Join(root, caFile) || env["SSL_CERT_FILE"] != "/from/image" || env["REQUESTS_CA_BUNDLE"] != "" {
		t.Fatalf("env without a bundle: %v", env)
	}
}

// env runs env in the sandbox with the call's variables and returns what it printed.
func (h *harness) env(call ...string) map[string]string {
	h.t.Helper()
	ce := map[string]string{}
	for _, kv := range call {
		k, v, _ := strings.Cut(kv, "=")
		ce[k] = v
	}
	var res api.ExecResult
	h.wantStatus(h.do("POST", "/exec/run", api.ExecRun{Cmd: []string{"env"}, Env: ce}, &res), 200, "")
	out := map[string]string{}
	for _, l := range strings.Split(string(res.Stdout), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			out[k] = v
		}
	}
	return out
}
