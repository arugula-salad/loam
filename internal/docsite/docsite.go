// Package docsite renders loam's user docs, docs/primer.md and
// docs/reference/*.md, as HTML pages, and serves them at /docs on the API's
// host (loam-api wires it in). Every page is rendered once, when the site is
// built; a request only looks one up.
//
// The Markdown is GitHub's (tables, fenced code, autolinks, task lists, and
// GitHub's heading anchors), so the pages read the same here as on GitHub.
// Links between the pages are written as links between the files
// (reference/exec.md, ../primer.md) and become links between the routes; a
// relative link to anything else in the repository, a contract say, goes to
// the file on GitHub.
package docsite

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// Prefix is where the site is served.
const Prefix = "/docs"

// RepoBlob is where a relative link to a file that is not a page goes: the
// file on GitHub, at main.
const RepoBlob = "https://github.com/arugula-salad/loam/blob/main/"

// ReferenceOrder is the order of the reference pages in the navigation: the
// order a reader meets the things they describe. A page not named here
// follows them, by name.
var ReferenceOrder = []string{
	"index", "sandboxes", "exec", "files", "ports", "stats", "services", "tasks", "checkpoints",
	"network", "secrets", "identity", "events", "templates", "inside", "e2b",
}

// csp allows the page its own inline style and nothing else: there is no
// script, no font and nothing fetched from anywhere.
const csp = "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:"

// page is one rendered page.
type page struct {
	// Route is the path it is served at.
	Route string
	// Src is the Markdown file it came from, relative to docs/; empty for
	// the landing page, which is made here.
	Src   string
	Title string

	html []byte
	etag string
}

// Site is the rendered docs.
type Site struct {
	pages map[string]*page
}

// navLink is a link in the navigation.
type navLink struct {
	Route, Title string
	Current      bool
}

// New renders the user docs in fsys, which is laid out as docs/ is: primer.md
// and reference/*.md. It serves what is there: a missing page is a page the
// navigation doesn't list, not an error, so loam builds and runs before the
// pages are written. A page that doesn't start with a "# Title" is titled after its file
// instead: a missing heading is the docs' mistake, and TestEmbedded catches it in CI; it must
// not stop loam-api from serving the API.
func New(fsys fs.FS) (*Site, error) {
	var pages []*page
	if _, err := fs.Stat(fsys, "primer.md"); err == nil {
		pages = append(pages, &page{Route: Prefix + "/primer", Src: "primer.md"})
	}
	refs, err := fs.Glob(fsys, "reference/*.md")
	if err != nil {
		return nil, err
	}
	slices.SortFunc(refs, func(a, b string) int {
		ra, rb := refRank(a), refRank(b)
		if ra != rb {
			return ra - rb
		}
		return strings.Compare(a, b)
	})
	for _, src := range refs {
		pages = append(pages, &page{Route: refRoute(src), Src: src})
	}

	routes := map[string]string{} // source path in the repository → route
	sources := map[string][]byte{}
	for _, p := range pages {
		raw, err := fs.ReadFile(fsys, p.Src)
		if err != nil {
			return nil, err
		}
		title, ok := firstHeading(raw)
		if !ok {
			title = strings.TrimSuffix(path.Base(p.Src), ".md")
		}
		p.Title = title
		sources[p.Src] = raw
		routes[path.Join("docs", p.Src)] = p.Route
	}

	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	)
	s := &Site{pages: map[string]*page{}}
	for _, p := range pages {
		raw := sources[p.Src]
		ctx := parser.NewContext(parser.WithIDs(&githubIDs{seen: map[string]bool{}}))
		doc := md.Parser().Parse(text.NewReader(raw), parser.WithContext(ctx))
		rewriteLinks(doc, p.Src, routes)
		var body bytes.Buffer
		if err := md.Renderer().Render(&body, raw, doc); err != nil {
			return nil, fmt.Errorf("docs/%s: %w", p.Src, err)
		}
		if err := s.add(p, template.HTML(body.String()), pages); err != nil {
			return nil, err
		}
	}

	landing := &page{Route: Prefix, Title: "loam docs"}
	var body bytes.Buffer
	if err := landingTmpl.Execute(&body, pages); err != nil {
		return nil, err
	}
	if err := s.add(landing, template.HTML(body.String()), pages); err != nil {
		return nil, err
	}
	return s, nil
}

// add puts p in its shell, with every page in the navigation, and keeps it.
func (s *Site) add(p *page, body template.HTML, pages []*page) error {
	var nav []navLink
	for _, q := range pages {
		nav = append(nav, navLink{Route: q.Route, Title: q.Title, Current: q == p})
	}
	var out bytes.Buffer
	err := shellTmpl.Execute(&out, struct {
		Title string
		Nav   []navLink
		Body  template.HTML
		Home  bool
	}{p.Title, nav, body, p.Route == Prefix})
	if err != nil {
		return fmt.Errorf("%s: %w", p.Route, err)
	}
	p.html = out.Bytes()
	sum := sha256.Sum256(p.html)
	p.etag = `"` + hex.EncodeToString(sum[:16]) + `"`
	s.pages[p.Route] = p
	return nil
}

// Serve serves the page at the request's path and says whether there was
// one; the caller answers a path that is not a page, in its own error shape.
// The pages never change while the process runs, so the ETag is the page's
// hash and a matching If-None-Match is 304 (http.ServeContent does the
// comparing); five minutes of caching lets a deploy show up soon enough.
func (s *Site) Serve(w http.ResponseWriter, r *http.Request) bool {
	p, ok := s.pages[r.URL.Path]
	if !ok {
		// /docs/ and /docs/reference are a slash away from a page.
		alt := strings.TrimSuffix(r.URL.Path, "/")
		if alt == r.URL.Path {
			alt += "/"
		}
		if _, ok := s.pages[alt]; !ok {
			return false
		}
		http.Redirect(w, r, alt, http.StatusMovedPermanently)
		return true
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "public, max-age=300")
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("ETag", p.etag)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(p.html))
	return true
}

func refRoute(src string) string {
	name := strings.TrimSuffix(path.Base(src), ".md")
	if name == "index" {
		return Prefix + "/reference/"
	}
	return Prefix + "/reference/" + name
}

func refRank(src string) int {
	if i := slices.Index(ReferenceOrder, strings.TrimSuffix(path.Base(src), ".md")); i >= 0 {
		return i
	}
	return len(ReferenceOrder)
}

// firstHeading is the text of a page's "# Title", which must be its first
// non-blank line.
func firstHeading(raw []byte) (string, bool) {
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		title, ok := strings.CutPrefix(line, "# ")
		title = strings.TrimSpace(strings.ReplaceAll(title, "`", ""))
		return title, ok && title != ""
	}
	return "", false
}

// rewriteLinks points the relative links of the page at src (relative to
// docs/) at the routes of the pages they name, and at GitHub for any other
// file in the repository. A link with a scheme, a host, an absolute path or
// only a fragment is left as written.
func rewriteLinks(doc ast.Node, src string, routes map[string]string) {
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch l := n.(type) {
		case *ast.Link:
			l.Destination = []byte(rewriteLink(string(l.Destination), src, routes))
		case *ast.Image:
			l.Destination = []byte(rewriteLink(string(l.Destination), src, routes))
		}
		return ast.WalkContinue, nil
	})
}

func rewriteLink(dest, src string, routes map[string]string) string {
	u, err := url.Parse(dest)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Path == "" || strings.HasPrefix(u.Path, "/") {
		return dest
	}
	target := path.Join("docs", path.Dir(src), u.Path)
	if strings.HasPrefix(target, "../") || target == ".." {
		return dest // outside the repository: nothing to point it at
	}
	out, ok := routes[target]
	if !ok {
		out = RepoBlob + target
	}
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		out += "#" + u.EscapedFragment()
	}
	return out
}

// githubIDs makes heading IDs as GitHub does, so that an anchor written
// against the page on GitHub works here too: lower case, letters, digits, '-'
// and '_' kept, a space a '-', anything else dropped, and a repeat numbered.
// goldmark's own turns '_' into '-' and drops letters outside ASCII.
type githubIDs struct {
	seen map[string]bool
}

func (g *githubIDs) Generate(value []byte, kind ast.NodeKind) []byte {
	var b strings.Builder
	for _, r := range strings.TrimSpace(string(value)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			b.WriteRune(unicode.ToLower(r))
		case r == ' ':
			b.WriteByte('-')
		}
	}
	id := b.String()
	if id == "" {
		id = "section"
	}
	out := id
	for i := 1; g.seen[out]; i++ {
		out = fmt.Sprintf("%s-%d", id, i)
	}
	g.seen[out] = true
	return []byte(out)
}

func (g *githubIDs) Put(value []byte) { g.seen[string(value)] = true }
