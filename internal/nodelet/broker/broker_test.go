package broker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/internal/nodelet/network"
	"github.com/arugula-salad/loam/pkg/api"
)

// testCA is a fresh broker CA.
func testCA(t *testing.T) *CA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "loam broker CA (test)"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	ca, err := NewCA(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}))
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// seen is a request as the origin got it.
type seen struct {
	method, target, proto string
	header                http.Header
	remote                string
}

// origin is a TLS host (example.com, the name httptest's certificate is
// for) that records what it is sent.
type origin struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []seen
	// release lets /stream send its second half.
	release chan struct{}
}

func newOrigin(t *testing.T) *origin {
	t.Helper()
	o := &origin{release: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		o.reqs = append(o.reqs, seen{r.Method, r.RequestURI, r.Proto, r.Header.Clone(), r.RemoteAddr})
		o.mu.Unlock()
		io.Copy(io.Discard, r.Body)
		switch {
		case r.URL.Path == "/echo":
			// The worst origin: every header value back, in the body.
			var b strings.Builder
			for k, vs := range r.Header {
				fmt.Fprintf(&b, "%s: %s\n", k, strings.Join(vs, ","))
			}
			io.WriteString(w, b.String())
		case r.URL.Path == "/echo-json":
			// The value as JSON writes it, with "/" escaped.
			v := strings.ReplaceAll(r.Header.Get("Authorization"), "/", `\/`)
			io.WriteString(w, `{"auth":"`+v+`"}`)
		case r.URL.Path == "/echo-header":
			w.Header().Set("X-Seen", r.Header.Get("Authorization"))
		case r.URL.Path == "/redirect":
			http.Redirect(w, r, "https://elsewhere.example/", http.StatusFound)
		case r.URL.Path == "/gzip":
			w.Header().Set("Content-Encoding", "gzip")
			io.WriteString(w, "not really gzip")
		case r.URL.Path == "/stream":
			// A stream of unknown length whose value arrives split over
			// two writes, the second after the guest has the first.
			v := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			io.WriteString(w, "before "+v[:5])
			w.(http.Flusher).Flush()
			<-o.release
			io.WriteString(w, v[5:]+" after")
		case r.URL.Path == "/chunks":
			io.WriteString(w, "one ")
			w.(http.Flusher).Flush()
			<-o.release
			io.WriteString(w, "two")
		case r.URL.Path == "/ws":
			if r.Header.Get("Upgrade") != "websocket" {
				http.Error(w, "no upgrade", 400)
				return
			}
			c, rw, err := http.NewResponseController(w).Hijack()
			if err != nil {
				return
			}
			defer c.Close()
			rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			rw.Flush()
			buf := make([]byte, 64)
			n, _ := rw.Read(buf)
			c.Write(append([]byte("echo:"), buf[:n]...))
		default:
			io.WriteString(w, "ok")
		}
	})
	o.srv = httptest.NewUnstartedServer(mux)
	o.srv.EnableHTTP2 = true
	o.srv.StartTLS()
	t.Cleanup(o.srv.Close)
	return o
}

func (o *origin) requests() []seen {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]seen(nil), o.reqs...)
}

func (o *origin) last(t *testing.T) seen {
	t.Helper()
	r := o.requests()
	if len(r) == 0 {
		t.Fatal("the origin got nothing")
	}
	return r[len(r)-1]
}

// harness is a broker for VM sb-a's connections to example.com, which a
// guest client reaches through a listener, as the proxy would hand them over.
type harness struct {
	o      *origin
	b      *Broker
	logs   *logBuf
	client *http.Client
	h1     *http.Client // HTTP/1.1 only
	mu     sync.Mutex
	pol    *network.Policy
}

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func newHarness(t *testing.T, secrets ...nodeapi.PolicySecret) *harness {
	t.Helper()
	return newHarnessRoots(t, nil, secrets...)
}

// newHarnessRoots is newHarness with the broker checking hosts against
// roots; nil is the roots that trust the origin.
func newHarnessRoots(t *testing.T, roots *x509.CertPool, secrets ...nodeapi.PolicySecret) *harness {
	t.Helper()
	h := &harness{o: newOrigin(t), logs: &logBuf{}}
	h.setSecrets(t, secrets...)
	ca := testCA(t)
	if roots == nil {
		roots = x509.NewCertPool()
		roots.AddCert(h.o.srv.Certificate())
	}
	originAddr := h.o.srv.Listener.Addr().String()
	h.b = &Broker{CA: ca, Log: slog.New(slog.NewTextHandler(h.logs, nil)), RootCAs: roots,
		Dial: func(ctx context.Context, name string, port int) (net.Conn, error) {
			if name != "example.com" || port != 443 {
				return nil, fmt.Errorf("dialed %s:%d", name, port)
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", originAddr)
		}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				h.b.Serve(c, "sb-a", "example.com", h.policy)
			}()
		}
	}()
	guestRoots := x509.NewCertPool()
	guestRoots.AppendCertsFromPEM([]byte(ca.PEM()))
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", ln.Addr().String())
	}
	h.client = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{DialContext: dial, ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{RootCAs: guestRoots}}}
	h.h1 = &http.Client{Timeout: 10 * time.Second,
		Transport: &http.Transport{DialContext: dial, TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}, TLSClientConfig: &tls.Config{RootCAs: guestRoots}}}
	return h
}

func (h *harness) policy() *network.Policy {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pol
}

func (h *harness) setSecrets(t *testing.T, secrets ...nodeapi.PolicySecret) {
	t.Helper()
	p, err := network.Compile(nodeapi.Policy{Secrets: secrets})
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.pol = p
	h.mu.Unlock()
}

const ghValue = "ghp_s3cr3t/value:123"

func gh(hosts ...string) nodeapi.PolicySecret {
	if len(hosts) == 0 {
		hosts = []string{"example.com"}
	}
	return nodeapi.PolicySecret{Name: "GH", Value: ghValue, Hosts: hosts, Placeholder: "__loam_gh__"}
}

func (h *harness) get(t *testing.T, c *http.Client, target string, hdr ...string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", "https://example.com"+target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// TestMITMInjectsCredentials: the value goes where the placeholder was, in
// a header, and the guest's other headers go as they are.
func TestSubstitutesInHeaders(t *testing.T) {
	h := newHarness(t, gh())
	resp, body := h.get(t, h.client, "/x", "Authorization", "Bearer __loam_gh__", "X-Other", "kept", "Proxy-Authorization", "dropped")
	if resp.StatusCode != 200 || body != "ok" || resp.ProtoMajor != 2 {
		t.Fatalf("%s %q", resp.Status, body)
	}
	got := h.o.last(t)
	if got.header.Get("Authorization") != "Bearer "+ghValue || got.header.Get("X-Other") != "kept" || got.header.Get("Proxy-Authorization") != "" {
		t.Errorf("origin saw %v", got.header)
	}
	// The broker asks for an uncompressed answer, whatever the guest asked.
	if got.header.Get("Accept-Encoding") != "identity" {
		t.Errorf("Accept-Encoding %q", got.header.Get("Accept-Encoding"))
	}
}

// A placeholder in the path and the query goes in byte for byte, nothing
// percent-encoded (TestMITMSubstitutionPath / Query).
func TestSubstitutesInTheTarget(t *testing.T) {
	h := newHarness(t, gh())
	h.get(t, h.h1, "/bot__loam_gh__/send?key=__loam_gh__&x=1")
	if got := h.o.last(t).target; got != "/bot"+ghValue+"/send?key="+ghValue+"&x=1" {
		t.Errorf("origin saw %q", got)
	}
	// Over HTTP/2 too.
	h.get(t, h.client, "/a/__loam_gh__")
	if got := h.o.last(t); got.target != "/a/"+ghValue || got.proto != "HTTP/1.1" && got.proto != "HTTP/2.0" {
		t.Errorf("origin saw %+v", got)
	}
}

// A client that puts the token in a URL's userinfo sends it in a Basic
// credential, which is decoded, substituted and encoded again.
func TestSubstitutesInBasic(t *testing.T) {
	h := newHarness(t, gh())
	req, _ := http.NewRequest("GET", "https://example.com/repo.git/info/refs", nil)
	req.SetBasicAuth("x-access-token", "__loam_gh__")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	r := &http.Request{Header: h.o.last(t).header}
	if u, p, ok := r.BasicAuth(); !ok || u != "x-access-token" || p != ghValue {
		t.Errorf("origin saw %q %q", u, p)
	}
}

// inject sets the header from its format, replacing the guest's.
func TestInjectsAHeader(t *testing.T) {
	s := gh()
	s.Inject = &api.Inject{Header: "x-api-key", Format: "Key {value}"}
	h := newHarness(t, s)
	h.get(t, h.client, "/x", "X-Api-Key", "whatever the guest sent")
	if got := h.o.last(t).header.Values("X-Api-Key"); len(got) != 1 || got[0] != "Key "+ghValue {
		t.Errorf("origin saw %q", got)
	}
}

// A secret's path prefix limits where it goes; elsewhere the placeholder
// goes as it is, which is worth nothing.
func TestPathPrefix(t *testing.T) {
	h := newHarness(t, gh("example.com/repos/"))
	h.get(t, h.client, "/repos/x", "Authorization", "Bearer __loam_gh__")
	if got := h.o.last(t).header.Get("Authorization"); got != "Bearer "+ghValue {
		t.Errorf("under the prefix: %q", got)
	}
	h.get(t, h.client, "/user", "Authorization", "Bearer __loam_gh__")
	if got := h.o.last(t).header.Get("Authorization"); got != "Bearer __loam_gh__" {
		t.Errorf("outside it: %q", got)
	}
}

// A secret the node has no value for is a 502, and the request is never
// sent (ErrCredentialMissing).
func TestMissingValueIs502(t *testing.T) {
	s := gh()
	s.Value = ""
	h := newHarness(t, s)
	resp, body := h.get(t, h.client, "/x", "Authorization", "Bearer __loam_gh__")
	var e api.Error
	json.Unmarshal([]byte(body), &e)
	if resp.StatusCode != http.StatusBadGateway || e.Error.Code != CodeBrokerFailed {
		t.Errorf("%s %q", resp.Status, body)
	}
	// One that doesn't use it goes.
	if resp, _ := h.get(t, h.client, "/y"); resp.StatusCode != 200 {
		t.Errorf("a request without the placeholder: %s", resp.Status)
	}
	if n := len(h.o.requests()); n != 1 {
		t.Errorf("the origin got %d requests", n)
	}
	// The same for one that injects.
	s.Inject = &api.Inject{Header: "x-api-key", Format: "{value}"}
	h.setSecrets(t, s)
	if resp, _ := h.get(t, h.client, "/z"); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("inject without a value: %s", resp.Status)
	}
}

// A value that can't be written into a request target is refused there.
func TestUnsafeValueInTarget(t *testing.T) {
	s := gh()
	s.Value = "has a space"
	h := newHarness(t, s)
	if resp, _ := h.get(t, h.h1, "/a/__loam_gh__"); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("%s", resp.Status)
	}
	// In a header a space is ordinary.
	h.get(t, h.client, "/b", "Authorization", "Bearer __loam_gh__")
	if got := h.o.last(t).header.Get("Authorization"); got != "Bearer has a space" {
		t.Errorf("%q", got)
	}
}

// A redirect goes back to the guest; the next request is matched afresh.
func TestRedirectsAreNotFollowed(t *testing.T) {
	h := newHarness(t, gh())
	resp, _ := h.get(t, h.client, "/redirect", "Authorization", "Bearer __loam_gh__")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "https://elsewhere.example/" {
		t.Errorf("%s %v", resp.Status, resp.Header)
	}
	if n := len(h.o.requests()); n != 1 {
		t.Errorf("the origin got %d requests", n)
	}
}

// An answer that holds a value is cut: a 502 before its head is sent.
func TestReflectedValueIs502(t *testing.T) {
	h := newHarness(t, gh())
	for _, path := range []string{"/echo", "/echo-json", "/echo-header"} {
		resp, body := h.get(t, h.client, path, "Authorization", "Bearer __loam_gh__")
		if resp.StatusCode != http.StatusBadGateway || strings.Contains(body, "s3cr3t") || strings.Contains(fmt.Sprint(resp.Header), "s3cr3t") {
			t.Errorf("%s: %s %v %q", path, resp.Status, resp.Header, body)
		}
	}
	// Any of the VM's values, whichever secret the request used.
	other := nodeapi.PolicySecret{Name: "OTHER", Value: "other-value-42", Hosts: []string{"other.example"}, Placeholder: "__loam_other__"}
	h.setSecrets(t, gh(), other)
	if resp, _ := h.get(t, h.client, "/echo", "X-Leak", "other-value-42"); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("another secret's value: %s", resp.Status)
	}
	// What can't be read can't be searched.
	if resp, _ := h.get(t, h.client, "/gzip"); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("an encoded answer: %s", resp.Status)
	}
	if !strings.Contains(h.logs.String(), "holds a secret") || strings.Contains(h.logs.String(), "s3cr3t") {
		t.Errorf("logs: %s", h.logs)
	}
}

// In a stream, the value split over two writes is caught before its first
// half reaches the guest, and the stream is cut.
func TestReflectedValueInAStreamIsCut(t *testing.T) {
	h := newHarness(t, gh())
	req, _ := http.NewRequest("GET", "https://example.com/stream", nil)
	req.Header.Set("Authorization", "Bearer __loam_gh__")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf) // "before " arrives; "ghp_s" waits, a candidate
	close(h.o.release)
	rest, err := io.ReadAll(resp.Body)
	got := string(buf[:n]) + string(rest)
	if err == nil {
		t.Errorf("the stream ended cleanly: %q", got)
	}
	if strings.Contains(got, "ghp_s") || strings.Contains(got, "s3cr3t") {
		t.Errorf("the guest got %q", got)
	}
}

// A streamed answer streams (TestMITMForwardStreamsChunksPromptly).
func TestStreams(t *testing.T) {
	h := newHarness(t, gh())
	resp, err := h.client.Get("https://example.com/chunks")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 16)
	n, err := resp.Body.Read(buf)
	if err != nil || string(buf[:n]) != "one " {
		t.Fatalf("first chunk %q %v", buf[:n], err)
	}
	close(h.o.release)
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "two" {
		t.Errorf("rest %q", rest)
	}
}

// Every request goes out on a connection of its own, however many the guest
// sends on one (TestMITMForwardKeepalivePersistsAcrossRequests).
func TestNoPooling(t *testing.T) {
	h := newHarness(t, gh())
	for range 3 {
		h.get(t, h.client, "/x", "Authorization", "Bearer __loam_gh__")
	}
	reqs := h.o.requests()
	remotes := map[string]bool{}
	for _, r := range reqs {
		remotes[r.remote] = true
		if r.header.Get("Authorization") != "Bearer "+ghValue {
			t.Errorf("request without its value: %v", r.header)
		}
	}
	if len(reqs) != 3 || len(remotes) != 3 {
		t.Errorf("%d requests on %d connections", len(reqs), len(remotes))
	}
}

// A changed secret is used from the next request, on the same connection.
func TestChangedSecret(t *testing.T) {
	h := newHarness(t, gh())
	h.get(t, h.client, "/x", "Authorization", "Bearer __loam_gh__")
	s := gh()
	s.Value = "ghp_rotated"
	h.setSecrets(t, s)
	h.get(t, h.client, "/x", "Authorization", "Bearer __loam_gh__")
	if got := h.o.last(t).header.Get("Authorization"); got != "Bearer ghp_rotated" {
		t.Errorf("after the change: %q", got)
	}
}

// A request for another host on the connection is refused: its secrets were
// chosen for the server name.
func TestHostMustBeTheServerName(t *testing.T) {
	h := newHarness(t, gh())
	req, _ := http.NewRequest("GET", "https://example.com/x", nil)
	req.Host = "evil.example"
	req.Header.Set("Authorization", "Bearer __loam_gh__")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest || len(h.o.requests()) != 0 {
		t.Errorf("%s, origin got %d", resp.Status, len(h.o.requests()))
	}
}

// The host's certificate is checked (TestMITMUpstreamCertUntrusted).
func TestUntrustedHostIs502(t *testing.T) {
	h := newHarnessRoots(t, x509.NewCertPool(), gh())
	if resp, _ := h.get(t, h.client, "/x"); resp.StatusCode != http.StatusBadGateway || len(h.o.requests()) != 0 {
		t.Errorf("%s", resp.Status)
	}
}

// A WebSocket upgrade carries the value like any request; its frames are
// spliced as they are (TestMITMWebSocketInjectsCredentialsAndPipesFrames).
func TestWebSocket(t *testing.T) {
	h := newHarness(t, gh())
	req, _ := http.NewRequest("GET", "https://example.com/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Authorization", "Bearer __loam_gh__")
	resp, err := h.h1.Transport.RoundTrip(req) // a Client's timeout would hide the upgraded body's Write
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %q", resp.Status, b)
	}
	rwc := resp.Body.(io.ReadWriteCloser)
	defer rwc.Close()
	rwc.Write([]byte("__loam_gh__ frame"))
	got, _ := io.ReadAll(bufio.NewReader(rwc))
	if string(got) != "echo:__loam_gh__ frame" {
		t.Errorf("frames %q", got)
	}
	if a := h.o.last(t).header.Get("Authorization"); a != "Bearer "+ghValue {
		t.Errorf("upgrade request %q", a)
	}
}

// Logs carry the method, host, path and status; never a query or a header value.
func TestLogs(t *testing.T) {
	h := newHarness(t, gh())
	h.get(t, h.client, "/logged/path?token=__loam_gh__&q=secretquery", "Authorization", "Bearer __loam_gh__")
	l := h.logs.String()
	if !strings.Contains(l, "path=/logged/path") || !strings.Contains(l, "status=200") || !strings.Contains(l, "method=GET") {
		t.Errorf("logs lack the request: %s", l)
	}
	for _, never := range []string{"secretquery", "__loam_gh__", "s3cr3t", "Bearer"} {
		if strings.Contains(l, never) {
			t.Errorf("logs hold %q: %s", never, l)
		}
	}
}
