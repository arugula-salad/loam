package apiserver

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/arugula-salad/loam/pkg/api"
)

func withDomains(o *Options) {
	o.URLDomains = Domains{"games.example": "arcade", "play.example": "arcade", "ghost.example": "ghost"}
}

// A project's URL domains reach its sandboxes by name, with the same access
// rules as the port URLs under loam's domain, and nothing else.
func TestURLDomains(t *testing.T) {
	h := newClusterWith(t, withDomains, nodeSpec{"geek", 8192})
	key := h.project("arcade")
	other := h.project("jake")
	sb := h.create(key, api.CreateSandbox{Name: "web"})
	h.node.Listen(sb.ID, 8080, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "app at "+r.Host+r.URL.Path)
	}))
	h.node.Listen(sb.ID, 3000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "three") }))

	if want := []string{"https://web.games.example", "https://web.play.example"}; !slices.Equal(sb.URLs, want) {
		t.Fatalf("urls: %v, want %v", sb.URLs, want)
	}
	if mine := h.create(other, api.CreateSandbox{Name: "web"}); mine.URLs == nil || len(mine.URLs) != 0 {
		t.Fatalf("a project without domains: urls %#v", mine.URLs)
	}

	// N.<domain> is the http_port, private by default.
	host := "web.games.example"
	for _, tok := range []string{"", other} {
		if status, _, _ := h.callHost(host, "GET", "/", tok, nil); status != 401 {
			t.Fatalf("private with %q: %d", tok, status)
		}
	}
	if status, body, _ := h.callHost(host, "GET", "/hello", key, nil); status != 200 || string(body) != "app at "+host+"/hello" {
		t.Fatalf("with the key: %d %q", status, body)
	}
	// Every path is the sandbox's: the API's own routes never answer here.
	if status, body, _ := h.callHost(host, "GET", "/internal/guest/self", key, nil); status != 200 || string(body) != "app at "+host+"/internal/guest/self" {
		t.Fatalf("/internal/ on a sandbox host: %d %q", status, body)
	}
	if status, body, _ := h.callHost("WEB.Play.Example.:443", "GET", "/", key, nil); status != 200 || string(body) != "app at WEB.Play.Example.:443/" {
		t.Fatalf("the second domain, oddly spelled: %d %q", status, body)
	}

	// The cookie is the host's own: set without a Domain, so it goes to no other.
	status, _, hdr := h.callHost(host, "GET", "/page?a=1&loam_token="+url.QueryEscape(key), "", nil)
	ck := hdr.Get("Set-Cookie")
	if status != 302 || hdr.Get("Location") != "/page?a=1" || !strings.Contains(ck, "loam_token="+key) || strings.Contains(strings.ToLower(ck), "domain=") {
		t.Fatalf("token visit: %d %v", status, hdr)
	}
	req, _ := http.NewRequest("GET", h.ts.URL+"/", nil)
	req.Host = host
	req.AddCookie(&http.Cookie{Name: "loam_token", Value: key})
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("with the cookie: %v %v", resp, err)
	}
	resp.Body.Close()
	if status, _, _ := h.callHost(host, "GET", "/?loam_token="+url.QueryEscape(other), "", nil); status != 401 {
		t.Fatalf("another project's token: %d", status)
	}

	// <port>--N.<domain> is any port; a public one needs no key.
	if status, body, _ := h.callHost("3000--web.games.example", "GET", "/", key, nil); status != 200 || string(body) != "three" {
		t.Fatalf("port 3000: %d %q", status, body)
	}
	if status, _, _ := h.callHost("3000--web.games.example", "GET", "/", "", nil); status != 401 {
		t.Fatalf("private port 3000 without a key: %d", status)
	}
	h.must(200, "PUT", "/v1/sandboxes/web/ports/3000", key, map[string]string{"access": "public"}, nil)
	if status, _, _ := h.callHost("3000--web.games.example", "GET", "/", "", nil); status != 200 {
		t.Fatalf("public port 3000: %d", status)
	}

	// A host under a domain that names no sandbox is not found, never the API.
	for _, bad := range []string{"nope.games.example", "a.web.games.example", "games.example", "0--web.games.example",
		"03000--web.games.example", "x--web.games.example", "web--arcade.games.example", "-web.games.example"} {
		status, _, _ := h.callHost(bad, "GET", "/healthz", key, nil)
		if status != 404 {
			t.Errorf("%s: %d, want 404", bad, status)
		}
	}
	// Another project's sandbox of the same name is not reachable under arcade's domain.
	h.node.ResetCalls()
	h.callHost("web.games.example", "GET", "/", other, nil)
	for _, c := range h.node.Calls() {
		if strings.Contains(c, h.get(other, "web").ID) {
			t.Fatalf("jake's sandbox was reached through arcade's domain: %s", c)
		}
	}

	// A domain whose project doesn't exist yet matches nothing until it does.
	if status, _, _ := h.callHost("web.ghost.example", "GET", "/", "", nil); status != 404 {
		t.Fatalf("a domain without its project: %d", status)
	}
	gk := h.project("ghost")
	g := h.create(gk, api.CreateSandbox{Name: "web"})
	h.must(200, "PUT", "/v1/sandboxes/web/ports/8080", gk, map[string]string{"access": "public"}, nil)
	h.node.Listen(g.ID, 8080, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ghost") }))
	if status, body, _ := h.callHost("web.ghost.example", "GET", "/", "", nil); status != 200 || string(body) != "ghost" {
		t.Fatalf("once the project exists: %d %q", status, body)
	}

	// The projects show their domains.
	var list struct {
		Projects []api.Project `json:"projects"`
	}
	h.must(200, "GET", "/v1/projects", adminToken, nil, &list)
	got := map[string][]string{}
	for _, p := range list.Projects {
		got[p.Name] = p.Domains
	}
	if !slices.Equal(got["arcade"], []string{"games.example", "play.example"}) || got["jake"] == nil || len(got["jake"]) != 0 ||
		!slices.Equal(got["ghost"], []string{"ghost.example"}) {
		t.Fatalf("projects: %+v", list)
	}
	var who api.Whoami
	h.must(200, "GET", "/v1/whoami", key, nil, &who)
	if !slices.Equal(who.Project.Domains, []string{"games.example", "play.example"}) {
		t.Fatalf("whoami: %+v", who)
	}
	// loam's own hosts are as they were.
	if status, _, _ := h.callHost("api."+testDomain, "GET", "/healthz", "", nil); status != 200 {
		t.Fatalf("the API host: %d", status)
	}
}

func TestLoadDomains(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "d")
		os.WriteFile(p, []byte(s), 0o644)
		return p
	}
	ds, err := LoadDomains(write(`
domains:
  - domain: games.arugula.io
    project: arcade
  - domain: studio.arugula.io
    project: arcade
  - domain: widgets.wtf.
    project: jake
`), "loam.example.com")
	if err != nil || len(ds) != 3 || ds["widgets.wtf"] != "jake" || !slices.Equal(ds.of("arcade"), []string{"games.arugula.io", "studio.arugula.io"}) {
		t.Fatalf("%v %v", ds, err)
	}
	for _, empty := range []string{"", "\n", `{"domains": []}`, "domains: []"} {
		if ds, err := LoadDomains(write(empty), "loam.example.com"); err != nil || len(ds) != 0 {
			t.Errorf("%q: %v %v", empty, ds, err)
		}
	}
	if ds, err := LoadDomains(write(`{"domains": [{"domain": "games.arugula.io", "project": "arcade"}]}`), ""); err != nil || ds["games.arugula.io"] != "arcade" {
		t.Errorf("JSON: %v %v", ds, err)
	}
	for _, bad := range []string{
		"domains:\n  - {domain: games.arugula.io, project: a}\n  - {domain: games.arugula.io, project: b}", // twice
		"domains:\n  - {domain: arugula.io, project: a}\n  - {domain: games.arugula.io, project: b}",       // nested
		"domains:\n  - {domain: x.loam.example.com, project: a}",                                           // under loam's
		"domains:\n  - {domain: example.com, project: a}",                                                  // over loam's
		"domains:\n  - {domain: localhost, project: a}",                                                    // one label
		"domains:\n  - {domain: Games.arugula.io, project: a}",                                             // case
		"domains:\n  - {domain: '*.arugula.io', project: a}",                                               // a wildcard
		"domains:\n  - {domain: games.arugula.io, project: a--b}",                                          // not a project name
		"domains:\n  - {domain: games.arugula.io}",                                                         // no project
		"domains:\n  - {domain: games.arugula.io, project: a, tls: true}",                                  // unknown field
	} {
		if _, err := LoadDomains(write(bad), "loam.example.com"); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
