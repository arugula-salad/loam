package apiserver

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

func get(t *testing.T, url, accept string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

// TestLandingPage: a browser at the API's root gets the landing page; every
// other client, and every other path, gets what it got before.
func TestLandingPage(t *testing.T) {
	h := newHarness(t)

	const browser = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
	code, hdr, body := get(t, h.ts.URL+"/", browser)
	if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "text/html") || !bytes.Equal(body, landingPage) {
		t.Fatalf("browser at /: %d %q, %d bytes", code, hdr.Get("Content-Type"), len(body))
	}
	if !strings.Contains(hdr.Get("Vary"), "Accept") {
		t.Errorf("Vary %q, want Accept", hdr.Get("Vary"))
	}
	for _, want := range []string{"https://github.com/arugula-salad/loam", "kubectl apply -k"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("the page has no %q", want)
		}
	}
	if bytes.Contains(body, []byte("TODO")) {
		t.Error("the page still has a TODO")
	}

	// Not a browser: the root's 404, unchanged.
	for _, accept := range []string{"", "*/*", "application/json"} {
		if code, _, body := get(t, h.ts.URL+"/", accept); code != 404 || bytes.Contains(body, []byte("<html")) {
			t.Errorf("Accept %q at /: %d %q, want the plain 404", accept, code, body)
		}
	}
	// Only the root: another unknown path is still a 404 for a browser too.
	if code, _, body := get(t, h.ts.URL+"/nope", browser); code != 404 || bytes.Contains(body, []byte("<html")) {
		t.Errorf("browser at /nope: %d", code)
	}
	// The API and the docs are untouched.
	if code, _, body := get(t, h.ts.URL+"/healthz", browser); code != 200 || string(body) != "ok\n" {
		t.Errorf("/healthz: %d %q", code, body)
	}
	if code, _, _ := get(t, h.ts.URL+"/v1/whoami", browser); code != 401 {
		t.Errorf("/v1/whoami without a key: %d, want 401", code)
	}
	// A sandbox's host never sees the page: ServeHTTP gives it to the sandbox first.
	req, _ := http.NewRequest("GET", h.ts.URL+"/", nil)
	req.Host = "web--proj." + testDomain
	req.Header.Set("Accept", browser)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if bytes.Equal(b, landingPage) {
		t.Error("a sandbox host was served the landing page")
	}
}
