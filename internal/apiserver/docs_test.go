package apiserver

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/arugula-salad/loam/docs"
	"github.com/arugula-salad/loam/pkg/api"
)

// The drift test: every public route loam serves is in the reference
// (docs/reference/*.md), so a route added without its page fails here. The
// routes are read from the code that registers them: loam-api's own (/v1/...
// and /.well-known/...), the agent's sandbox sub-API, which loam-api forwards
// under /v1/sandboxes/{sandbox}, and the three routes only the guest socket
// has. /internal/, /healthz and /docs are not the API.

// route is a method and a path pattern as registered.
type route struct{ method, path string }

func (r route) String() string { return r.method + " " + r.path }

// registered reads the method-and-path patterns passed to Handle and
// HandleFunc in the package in dir, with the function each is in.
func registered(t *testing.T, dir string) map[route]string {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[route]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
					return true
				}
				lit, ok := call.Args[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				pattern, _ := strconv.Unquote(lit.Value)
				method, path, ok := strings.Cut(pattern, " ")
				if ok && strings.HasPrefix(path, "/") {
					out[route{method, path}] = fn.Name.Name
				}
				return true
			})
		}
	}
	return out
}

// publicRoutes are the routes the reference must have.
func publicRoutes(t *testing.T) []route {
	t.Helper()
	var out []route
	for r := range registered(t, ".") {
		if strings.HasPrefix(r.path, "/v1/") || strings.HasPrefix(r.path, "/.well-known/") {
			out = append(out, r)
		}
	}
	for r, fn := range registered(t, "../agent") {
		switch {
		case strings.HasPrefix(r.path, "/internal/"):
		case fn == "GuestHandler":
			out = append(out, r)
		default:
			out = append(out, route{r.method, "/v1/sandboxes/{sandbox}" + r.path})
		}
	}
	slices.SortFunc(out, func(a, b route) int { return strings.Compare(a.path+" "+a.method, b.path+" "+b.method) })
	return out
}

// TestPublicRoutesFound guards the extraction: a refactor that hides the
// routes from it must fail here, not let the drift test pass on nothing.
func TestPublicRoutesFound(t *testing.T) {
	rs := publicRoutes(t)
	for _, want := range []route{
		{"POST", "/v1/sandboxes"},
		{"GET", "/v1/whoami"},
		{"GET", "/.well-known/jwks.json"},
		{"GET", "/v1/sandboxes/{sandbox}/exec"},
		{"PUT", "/v1/sandboxes/{sandbox}/fs/file"},
		{"POST", "/v1/sandboxes/{sandbox}/services/{name}/restart"},
		{"GET", "/self"},
		{"GET", "/identity/token"},
	} {
		if !slices.Contains(rs, want) {
			t.Errorf("%s is not among the public routes: %v", want, rs)
		}
	}
	for _, r := range rs {
		if strings.Contains(r.path, "/internal/") || strings.HasPrefix(r.path, "/docs") {
			t.Errorf("%s is not public", r)
		}
	}
}

// param is a path parameter as registered ({sandbox}, {rest...}) or as a page
// writes it, which may be a choice of literals: {start\|stop\|restart}.
const param = `\{[^{}/\s]*\}`

// routeRe matches a route's path in a page: any name for a parameter, and a
// literal segment either as itself or as one of a choice in braces.
func routeRe(path string) *regexp.Regexp {
	var b strings.Builder
	for i, seg := range strings.Split(path, "/") {
		if i > 0 {
			b.WriteString("/")
		}
		switch {
		case strings.HasPrefix(seg, "{"):
			b.WriteString(param)
		case seg == "":
		default:
			q := regexp.QuoteMeta(seg)
			b.WriteString(`(?:` + q + `|\{[^{}/\s]*\b` + q + `\b[^{}/\s]*\})`)
		}
	}
	return regexp.MustCompile(b.String())
}

// mentions says whether text has path as a whole path: not the start of a
// longer one (/v1/sandboxes in /v1/sandboxes/{sandbox}) and not the end of one
// under a parameter (/checkpoints in /v1/sandboxes/{sandbox}/checkpoints).
func mentions(text, path string) bool {
	for _, m := range routeRe(path).FindAllStringIndex(text, -1) {
		if m[0] > 0 && text[m[0]-1] == '}' {
			continue
		}
		if m[1] < len(text) && strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./{", rune(text[m[1]])) {
			continue
		}
		return true
	}
	return false
}

// undocumented are the routes no page mentions.
func undocumented(routes []route, pages []string) []route {
	text := strings.Join(pages, "\n")
	var out []route
	for _, r := range routes {
		if !mentions(text, r.path) {
			out = append(out, r)
		}
	}
	return out
}

func TestMentions(t *testing.T) {
	pages := []string{
		"| `GET /v1/sandboxes/{id}/exec/sessions` | list |\n" +
			"| `POST /v1/sandboxes/{sandbox}/services/{name}/{start\\|stop\\|restart}` | → `Service` |\n" +
			"`GET /v1/sandboxes/{sandbox}/fs/file?path=` raw bytes\n" +
			"From inside: `curl --unix-socket /run/loam/api.sock http://loam/identity/token?audience=x`\n" +
			"and `POST /v1/sandboxes/{sandbox}/checkpoints`.\n",
	}
	for _, c := range []struct {
		path string
		want bool
	}{
		{"/v1/sandboxes/{sandbox}/exec/sessions", true},
		{"/v1/sandboxes/{sandbox}/services/{name}/start", true},
		{"/v1/sandboxes/{sandbox}/services/{name}/restart", true},
		{"/v1/sandboxes/{sandbox}/services/{name}/logs", false},
		{"/v1/sandboxes/{sandbox}/fs/file", true},
		{"/identity/token", true},
		{"/v1/sandboxes/{sandbox}/checkpoints", true},
		// Only as the start or the end of a longer path.
		{"/v1/sandboxes/{sandbox}/exec", false},
		{"/v1/sandboxes", false},
		{"/checkpoints", false},
		{"/v1/sandboxes/{sandbox}/fs", false},
	} {
		if got := mentions(pages[0], c.path); got != c.want {
			t.Errorf("%s: %v, want %v", c.path, got, c.want)
		}
	}
	got := undocumented([]route{{"GET", "/v1/sandboxes/{sandbox}/exec/sessions"}, {"GET", "/v1/events"}}, pages)
	if len(got) != 1 || got[0].path != "/v1/events" {
		t.Fatalf("undocumented: %v", got)
	}
}

// TestReferenceCoversRoutes is the drift test against the docs loam is built
// with.
func TestReferenceCoversRoutes(t *testing.T) {
	names, err := fs.Glob(docs.FS, "reference/*.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Skip("no docs/reference/*.md to check the routes against")
	}
	var pages []string
	for _, n := range names {
		raw, err := fs.ReadFile(docs.FS, n)
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, string(raw))
	}
	for _, r := range undocumented(publicRoutes(t), pages) {
		t.Errorf("%s is in no page of docs/reference/", r)
	}
}

func TestDocsRoutes(t *testing.T) {
	h := newHarness(t)
	status, body, hdr := h.call("GET", "/docs", "", nil)
	if status != 200 || hdr.Get("Content-Type") != "text/html; charset=utf-8" || !strings.Contains(string(body), "<h1>loam docs</h1>") {
		t.Fatalf("/docs: %d %v %.200s", status, hdr, body)
	}
	etag := hdr.Get("ETag")
	req, _ := http.NewRequest("GET", h.ts.URL+"/docs", nil)
	req.Header.Set("If-None-Match", etag)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("/docs with its ETag: %d", resp.StatusCode)
	}
	h.errCode(404, "not_found", "GET", "/docs/reference/no-such-page", "", nil)
	if status, _, _ := h.call("POST", "/docs", "", nil); status != http.StatusMethodNotAllowed {
		t.Fatalf("POST /docs: %d", status)
	}

	// A sandbox's host is the sandbox's, /docs included: here a private
	// port asked without a key.
	key := h.project("jake")
	h.create(key, api.CreateSandbox{Name: "web"})
	if status, body, _ := h.callHost("web--jake."+testDomain, "GET", "/docs", "", nil); status != 401 || strings.Contains(string(body), "<html") {
		t.Fatalf("/docs on a sandbox's host: %d %.200s", status, body)
	}
}
