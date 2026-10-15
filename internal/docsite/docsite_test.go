package docsite

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/arugula-salad/loam/docs"
)

// testDocs is docs/ in the shape the user docs have, small.
var testDocs = fstest.MapFS{
	"API.md": {Data: []byte("# The contract\n\nNot served.\n")},
	"primer.md": {Data: []byte("# Primer\n\n" +
		"Start with [exec](reference/exec.md#streaming-get-v1sandboxessandboxexec), " +
		"[the reference](reference/index.md), [the contract](API.md#errors), " +
		"[elsewhere](https://example.com/a.md), [here](#a-table) and <script>alert(1)</script>.\n\n" +
		"## A table\n\n| Route | What |\n|---|---|\n| `GET /v1/whoami` | the key |\n\n" +
		"```sh\ncurl -s $LOAM/v1/whoami\n```\n\n" +
		"## A table\n\n- [x] done\n- [ ] not yet\n")},
	"reference/index.md": {Data: []byte("# Reference\n\nBack to [the primer](../primer.md).\n")},
	"reference/zeta.md":  {Data: []byte("# Zeta\n\nA page the order doesn't name.\n")},
	"reference/exec.md": {Data: []byte("# Exec\n\n" +
		"## Streaming: `GET /v1/sandboxes/{sandbox}/exec`\n\n" +
		"See [files](files.md) and [the deploy](../../deploy/base/api.yaml).\n")},
}

func rendered(t *testing.T, s *Site, route string) string {
	t.Helper()
	p, ok := s.pages[route]
	if !ok {
		t.Fatalf("no page at %s", route)
	}
	return string(p.html)
}

func TestRender(t *testing.T) {
	s, err := New(testDocs)
	if err != nil {
		t.Fatal(err)
	}
	primer := rendered(t, s, "/docs/primer")
	for _, want := range []string{
		"<title>Primer · loam docs</title>",
		`<table>`, `<th>Route</th>`, `<td><code>GET /v1/whoami</code></td>`,
		`<pre><code class="language-sh">curl -s $LOAM/v1/whoami`,
		`href="/docs/reference/exec#streaming-get-v1sandboxessandboxexec"`,
		`href="/docs/reference/"`,
		`href="` + RepoBlob + `docs/API.md#errors"`,
		`href="https://example.com/a.md"`,
		`href="#a-table"`,
		`<h2 id="a-table">`, `<h2 id="a-table-1">`,
		`<input checked="" disabled="" type="checkbox"`,
		`<a href="/docs/primer" aria-current="page">Primer</a>`,
	} {
		if !strings.Contains(primer, want) {
			t.Errorf("the primer has no %s", want)
		}
	}
	if strings.Contains(primer, "<script") {
		t.Error("raw HTML in the Markdown reached the page")
	}

	exec := rendered(t, s, "/docs/reference/exec")
	for _, want := range []string{
		`<h2 id="streaming-get-v1sandboxessandboxexec">`,
		// files.md is not a page in testDocs, so it is only a file.
		`href="` + RepoBlob + `docs/reference/files.md"`,
		`href="` + RepoBlob + `deploy/base/api.yaml"`,
	} {
		if !strings.Contains(exec, want) {
			t.Errorf("exec has no %s", want)
		}
	}
	if idx := rendered(t, s, "/docs/reference/"); !strings.Contains(idx, `href="/docs/primer"`) {
		t.Error("../primer.md was not rewritten")
	}

	// The navigation: the primer, then the reference in ReferenceOrder, then
	// the pages it doesn't name.
	landing := rendered(t, s, "/docs")
	order := []string{`href="/docs/primer"`, `href="/docs/reference/"`, `href="/docs/reference/exec"`, `href="/docs/reference/zeta"`}
	last := -1
	for _, o := range order {
		i := strings.Index(landing, o)
		if i <= last {
			t.Fatalf("navigation out of order at %s:\n%s", o, landing)
		}
		last = i
	}
	if strings.Contains(landing, "API.md") || strings.Contains(landing, "The contract") {
		t.Error("a contract is in the navigation")
	}
}

// A page without a "# Title" still renders, titled after its file: loam-api
// renders the docs at startup and must not fail on them.
func TestNoTitle(t *testing.T) {
	s, err := New(fstest.MapFS{"reference/x.md": {Data: []byte("Text first.\n\n# Title\n")}})
	if err != nil {
		t.Fatal(err)
	}
	if p := s.pages[Prefix+"/reference/x"]; p == nil || p.Title != "x" {
		t.Fatalf("page %+v", p)
	}
}

// TestEmbedded renders the docs loam is built with, and holds each page to
// starting with its "# Title", which New no longer does.
func TestEmbedded(t *testing.T) {
	if _, err := New(docs.FS); err != nil {
		t.Fatal(err)
	}
	srcs, _ := fs.Glob(docs.FS, "reference/*.md")
	if _, err := fs.Stat(docs.FS, "primer.md"); err == nil {
		srcs = append(srcs, "primer.md")
	}
	for _, src := range srcs {
		raw, err := fs.ReadFile(docs.FS, src)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := firstHeading(raw); !ok {
			t.Errorf("docs/%s: no \"# Title\" on its first line", src)
		}
	}
}

func TestServe(t *testing.T) {
	s, err := New(testDocs)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string, hdr ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		if !s.Serve(w, r) {
			w.Code = 0
		}
		return w
	}
	w := get("/docs/reference/exec")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<h1") {
		t.Fatalf("exec: %d", w.Code)
	}
	h := w.Header()
	for k, v := range map[string]string{
		"Content-Type":            "text/html; charset=utf-8",
		"Cache-Control":           "public, max-age=300",
		"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:",
	} {
		if h.Get(k) != v {
			t.Errorf("%s: %q", k, h.Get(k))
		}
	}
	etag := h.Get("ETag")
	if len(etag) < 10 || etag[0] != '"' {
		t.Fatalf("ETag %q", etag)
	}
	if w := get("/docs/reference/exec", "If-None-Match", etag); w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatalf("with the ETag: %d", w.Code)
	}
	if w := get("/docs/reference/exec", "If-None-Match", `"other"`); w.Code != 200 {
		t.Fatalf("with another ETag: %d", w.Code)
	}
	if get("/docs/primer").Header().Get("ETag") == etag {
		t.Fatal("two pages, one ETag")
	}
	for path, to := range map[string]string{"/docs/": "/docs", "/docs/primer/": "/docs/primer", "/docs/reference": "/docs/reference/"} {
		if w := get(path); w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != to {
			t.Errorf("%s: %d to %q", path, w.Code, w.Header().Get("Location"))
		}
	}
	for _, path := range []string{"/docs/reference/nope", "/docs/API", "/docs/reference/exec.md"} {
		if w := get(path); w.Code != 0 {
			t.Errorf("%s: %d, want no page", path, w.Code)
		}
	}
}

func TestGitHubIDs(t *testing.T) {
	g := &githubIDs{seen: map[string]bool{}}
	for in, want := range map[string]string{
		"Streaming: `GET /v1/sandboxes/{sandbox}/exec` (WebSocket)": "streaming-get-v1sandboxessandboxexec-websocket",
		"snake_case and Ünïcode":                                    "snake_case-and-ünïcode",
		"!!!":                                                       "section",
	} {
		if got := string(g.Generate([]byte(in), 0)); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	if got := string(g.Generate([]byte("!!!"), 0)); got != "section-1" {
		t.Errorf("a repeat: %q", got)
	}
}
