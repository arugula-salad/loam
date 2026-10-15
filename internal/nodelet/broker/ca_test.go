package broker

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/internal/nodelet/network"
)

// A leaf is for its name, chains to the root, and lives a week; it is
// cached, made again a day before it ends, and the cache is bounded.
func TestLeaves(t *testing.T) {
	ca := testCA(t)
	now := time.Now()
	ca.now = func() time.Time { return now }
	l, err := ca.Leaf("api.github.com")
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(l.Certificate[0])
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(ca.PEM()))
	if _, err := cert.Verify(x509.VerifyOptions{DNSName: "api.github.com", Roots: roots, CurrentTime: now}); err != nil {
		t.Errorf("the leaf doesn't verify: %v", err)
	}
	if d := cert.NotAfter.Sub(now); d <= leafLife-time.Second || d > leafLife { // a certificate's times are in seconds
		t.Errorf("life %v", d)
	}
	if again, _ := ca.Leaf("api.github.com"); again != l {
		t.Error("not cached")
	}
	now = now.Add(leafLife - leafRenew + time.Minute)
	if renewed, _ := ca.Leaf("api.github.com"); renewed == l {
		t.Error("a leaf with less than a day left was handed out")
	}
	for i := range maxLeaves + 10 {
		ca.Leaf(fmt.Sprintf("h%d.example.com", i))
	}
	if ca.lru.Len() != maxLeaves || len(ca.leaves) != maxLeaves {
		t.Errorf("cache holds %d", ca.lru.Len())
	}
	if _, ok := ca.leaves["api.github.com"]; ok {
		t.Error("the least recently used leaf was kept")
	}
}

func TestLoadCA(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadCA(dir); err == nil {
		t.Error("loaded nothing")
	}
	// A certificate that isn't a CA's is refused.
	leafCA := testCA(t)
	l, _ := leafCA.Leaf("x.example.com")
	os.WriteFile(filepath.Join(dir, "tls.crt"), pemCert(l.Certificate[0]), 0o600)
	os.WriteFile(filepath.Join(dir, "tls.key"), pemKey(t, l.PrivateKey), 0o600)
	if _, err := LoadCA(dir); err == nil || !strings.Contains(err.Error(), "not a CA") {
		t.Errorf("a leaf as the CA: %v", err)
	}
}

// The scanner holds back what could start a value, so one split across
// reads is caught before any of it is released; everything else passes
// unchanged.
func TestScanner(t *testing.T) {
	ns := needles([]network.Secret{{Value: "ab/cd"}})
	if len(ns) != 2 || string(ns[1]) != `ab\/cd` {
		t.Fatalf("needles %q", ns)
	}
	read := func(body string) (string, error) {
		s := &scanner{body: io.NopCloser(iotest.OneByteReader(strings.NewReader(body))), resp: &http.Response{}, needles: ns}
		var got []byte
		buf := make([]byte, 3)
		for {
			n, err := s.Read(buf)
			got = append(got, buf[:n]...)
			if err != nil {
				if err == io.EOF {
					err = nil
				}
				return string(got), err
			}
		}
	}
	for _, clean := range []string{"", "hello", "ab/c", "xxab", "ab\\/c ab/ ab/c"} {
		if got, err := read(clean); err != nil || got != clean {
			t.Errorf("%q: %q %v", clean, got, err)
		}
	}
	for _, dirty := range []string{"ab/cd", "xx ab/cd yy", `{"t":"ab\/cd"}`} {
		got, err := read(dirty)
		if err != errReflected || strings.Contains(got, "ab") {
			t.Errorf("%q: released %q, %v", dirty, got, err)
		}
	}
	// Trailers are searched at the end.
	resp := &http.Response{Trailer: http.Header{"X-T": {"ab/cd"}}}
	s := &scanner{body: io.NopCloser(strings.NewReader("fine")), resp: resp, needles: ns}
	if _, err := io.ReadAll(s); err != errReflected {
		t.Errorf("trailer: %v", err)
	}
}

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func pemKey(t *testing.T, key any) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func TestApplicable(t *testing.T) {
	p, err := network.Compile(nodeapi.Policy{Secrets: []nodeapi.PolicySecret{
		{Name: "A", Value: "a", Hosts: []string{"*.github.com"}, Placeholder: "__a__"},
		{Name: "B", Value: "b", Hosts: []string{"api.github.com/repos/"}, Placeholder: "__b__"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	names := func(name, path string) string {
		var out []string
		for _, s := range applicable(p.Secrets(), name, path) {
			out = append(out, s.Name)
		}
		return strings.Join(out, ",")
	}
	for _, c := range [][3]string{
		{"api.github.com", "/repos/x", "A,B"}, {"api.github.com", "/user", "A"},
		{"github.com", "/", ""}, {"raw.github.com", "/repos/x", "A"},
	} {
		if got := names(c[0], c[1]); got != c[2] {
			t.Errorf("%s%s: %q, want %q", c[0], c[1], got, c[2])
		}
	}
}
