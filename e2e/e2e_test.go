//go:build e2e

// Package e2e drives a running loam (loam-api, a nodelet, the agent) through
// pkg/client, the way the CLI and every Go caller do. It is phase 1's
// done-when (docs/PLAN.md). Each test makes its own sandboxes and deletes them.
//
//	LOAM_E2E_URL=https://api.loam.example.com LOAM_E2E_TOKEN=loam_... \
//	  go test -tags e2e -count=1 -v ./e2e/
//
// Optional:
//
//	LOAM_E2E_ADMIN_TOKEN  the admin token; without it the project isolation test is skipped.
//	LOAM_E2E_INGRESS      host:port to dial for sandbox and port URLs instead of
//	                      resolving their names (Traefik on the LAN, say); TLS
//	                      still checks the URL's name.
//	LOAM_E2E_INSECURE=1   don't verify the port URLs' certificates.
//	LOAM_E2E_TEMPLATE     the template to create sandboxes from (default: the API's default).
//	LOAM_E2E_GOARCH       the sandboxes' architecture, for the helper server (default amd64).
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base32"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arugula-salad/loam/pkg/api"
	"github.com/arugula-salad/loam/pkg/client"
)

func e2eURL() string   { return os.Getenv("LOAM_E2E_URL") }
func e2eToken() string { return os.Getenv("LOAM_E2E_TOKEN") }

// newClient is a client for the suite's key, or a skip when none is set.
func newClient(t *testing.T) *client.Client {
	t.Helper()
	if e2eURL() == "" || e2eToken() == "" {
		t.Skip("LOAM_E2E_URL / LOAM_E2E_TOKEN not set")
	}
	return client.New(e2eURL(), e2eToken())
}

// testCtx bounds a test; a boot and a few snapshots fit easily.
func testCtx(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// uniqueName is a sandbox name: a DNS label, short, with no "--".
func uniqueName(tag string) string {
	var b [5]byte
	rand.Read(b[:])
	return "e2e-" + tag + "-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}

// newSandbox creates a sandbox for this test and deletes it when the test ends.
func newSandbox(t *testing.T, ctx context.Context, c *client.Client, tag string, req api.CreateSandbox) *api.Sandbox {
	t.Helper()
	if req.Name == "" {
		req.Name = uniqueName(tag)
	}
	if req.Template == "" && req.Image == "" && req.From == nil {
		req.Template = os.Getenv("LOAM_E2E_TEMPLATE")
	}
	if req.Labels == nil {
		req.Labels = map[string]string{}
	}
	req.Labels["e2e"] = "true"
	sb, err := c.CreateSandbox(ctx, req)
	if err != nil {
		t.Fatalf("create %s: %v", req.Name, err)
	}
	cleanup(t, c, sb.Name)
	if sb.State != api.StateRunning {
		t.Fatalf("create answered before the sandbox ran: %+v", sb)
	}
	return sb
}

// cleanup deletes a sandbox when the test ends, unprotecting it first.
func cleanup(t *testing.T, c *client.Client, name string) {
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		f := false
		c.UpdateSandbox(ctx, name, api.UpdateSandbox{Protected: &f})
		if err := c.DeleteSandbox(ctx, name); err != nil && !client.IsNotFound(err) {
			t.Errorf("cleanup: delete %s: %v", name, err)
		}
	})
}

// sh runs a bash script in the sandbox with the one-shot exec and returns its
// stdout; a non-zero exit fails the test.
func sh(t *testing.T, ctx context.Context, c *client.Client, sandbox, script string) string {
	t.Helper()
	return shAs(t, ctx, c, sandbox, "", script)
}

func shAs(t *testing.T, ctx context.Context, c *client.Client, sandbox, user, script string) string {
	t.Helper()
	r, err := c.Run(ctx, sandbox, api.ExecRun{Cmd: []string{"bash", "-c", script}, User: user, TimeoutS: 120})
	if err != nil {
		t.Fatalf("%s: %v", script, err)
	}
	if r.ExitCode != 0 || r.TimedOut {
		t.Fatalf("%s: exit %d (timed out %v)\nstdout: %s\nstderr: %s", script, r.ExitCode, r.TimedOut, r.Stdout, r.Stderr)
	}
	return string(r.Stdout)
}

// shCode runs a script and returns its exit code, whatever it is.
func shCode(t *testing.T, ctx context.Context, c *client.Client, sandbox, user, script string) int {
	t.Helper()
	r, err := c.Run(ctx, sandbox, api.ExecRun{Cmd: []string{"bash", "-c", script}, User: user, TimeoutS: 120})
	if err != nil {
		t.Fatalf("%s: %v", script, err)
	}
	return r.ExitCode
}

// eventually retries f until it succeeds or d passes.
func eventually(t *testing.T, d time.Duration, what string, f func() error) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		err := f()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: still not true after %s: %v", what, d, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// syncBuffer is a bytes.Buffer safe to fill from a stream while a test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitFor waits until the buffer contains want.
func (s *syncBuffer) waitFor(t *testing.T, want string, d time.Duration) {
	t.Helper()
	eventually(t, d, fmt.Sprintf("output containing %q", want), func() error {
		if strings.Contains(s.String(), want) {
			return nil
		}
		return fmt.Errorf("output so far %q", s.String())
	})
}

var (
	serverOnce sync.Once
	serverPath string
	serverErr  error
)

// serverBinary builds testdata/e2eserver for the sandboxes' architecture.
func serverBinary(t *testing.T) string {
	t.Helper()
	serverOnce.Do(func() {
		dir, err := os.MkdirTemp("", "loam-e2e-")
		if err != nil {
			serverErr = err
			return
		}
		serverPath = filepath.Join(dir, "e2e-server")
		arch := os.Getenv("LOAM_E2E_GOARCH")
		if arch == "" {
			arch = "amd64"
		}
		cmd := exec.Command("go", "build", "-trimpath", "-o", serverPath, "./testdata/e2eserver")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
		if out, err := cmd.CombinedOutput(); err != nil {
			serverErr = fmt.Errorf("building the helper server: %v\n%s", err, out)
		}
	})
	if serverErr != nil {
		t.Fatal(serverErr)
	}
	return serverPath
}

// installServer uploads the helper server to /usr/local/bin/e2e-server.
func installServer(t *testing.T, ctx context.Context, c *client.Client, sandbox string) {
	t.Helper()
	f, err := os.Open(serverBinary(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := c.WriteFile(ctx, sandbox, "/usr/local/bin/e2e-server", f, client.Mode(0o755), client.MakeParents()); err != nil {
		t.Fatalf("upload the helper server: %v", err)
	}
}

// startServer runs the helper server on port in a detachable session, waits
// until it listens, and detaches: an attached session would hold the sandbox
// awake. It returns the session's ID.
func startServer(t *testing.T, ctx context.Context, c *client.Client, sandbox string, port int) string {
	t.Helper()
	s, err := c.Exec(ctx, sandbox, client.ExecOptions{Cmd: []string{"/usr/local/bin/e2e-server", "-port", fmt.Sprint(port)}, Detachable: true})
	if err != nil {
		t.Fatalf("start the helper server: %v", err)
	}
	waitListening(t, ctx, c, sandbox, port)
	s.Close()
	return s.Session
}

func waitListening(t *testing.T, ctx context.Context, c *client.Client, sandbox string, port int) {
	t.Helper()
	eventually(t, 20*time.Second, fmt.Sprintf("port %d listening", port), func() error {
		ps, err := c.Ports(ctx, sandbox)
		if err != nil {
			return err
		}
		for _, p := range ps {
			if p.Port == port {
				return nil
			}
		}
		return fmt.Errorf("ports: %+v", ps)
	})
}

// ingressDial dials LOAM_E2E_INGRESS instead of the URL's host when it is set.
func ingressDial(ctx context.Context, network, addr string) (net.Conn, error) {
	if in := os.Getenv("LOAM_E2E_INGRESS"); in != "" {
		addr = in
	}
	return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
}

func urlTLS() *tls.Config {
	return &tls.Config{InsecureSkipVerify: os.Getenv("LOAM_E2E_INSECURE") == "1"}
}

// urlClient fetches sandbox and port URLs, keeping cookies.
func urlClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Transport: &http.Transport{DialContext: ingressDial, TLSClientConfig: urlTLS()},
		Jar:       jar,
		Timeout:   60 * time.Second,
		// The first visit with ?loam_token= may redirect to drop the token from the URL.
		CheckRedirect: func(*http.Request, []*http.Request) error { return nil },
	}
}

// fetch GETs a URL with optional headers and returns the status and body.
func fetch(t *testing.T, hc *http.Client, u string, h http.Header) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range h {
		req.Header[k] = v
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func bearer() http.Header { return http.Header{"Authorization": {"Bearer " + e2eToken()}} }

// wsEcho opens a WebSocket on a sandbox or port URL's /ws and checks it echoes.
func wsEcho(t *testing.T, u string, h http.Header) error {
	t.Helper()
	d := websocket.Dialer{NetDialContext: ingressDial, TLSClientConfig: urlTLS(), HandshakeTimeout: 30 * time.Second}
	wsURL := "ws" + strings.TrimPrefix(strings.TrimRight(u, "/"), "http") + "/ws"
	ws, resp, err := d.Dial(wsURL, h)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial %s: %v (%s)", wsURL, err, resp.Status)
		}
		return fmt.Errorf("dial %s: %v", wsURL, err)
	}
	defer ws.Close()
	if err := ws.WriteMessage(websocket.TextMessage, []byte("marco")); err != nil {
		return err
	}
	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, data, err := ws.ReadMessage()
	if err != nil {
		return err
	}
	if string(data) != "marco" {
		return fmt.Errorf("echo answered %q", data)
	}
	return nil
}
