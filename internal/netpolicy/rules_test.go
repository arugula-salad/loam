package netpolicy

import (
	"testing"

	"github.com/arugula-salad/loam/pkg/api"
)

func allow(d string, ports ...int) api.Rule {
	return api.Rule{Domain: d, Action: api.RuleAllow, Ports: ports}
}
func deny(d string) api.Rule { return api.Rule{Domain: d, Action: api.RuleDeny} }

func TestAllows(t *testing.T) {
	p, err := Compile([]api.Rule{
		allow("*.githubusercontent.com"),
		allow("github.com", 22, 443),
		deny("gist.github.com"),
		allow("*.github.com"),
		allow("db.example.com", 5432),
		allow("*.example.com"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		port int
		want bool
	}{
		{"raw.githubusercontent.com", 443, true},
		{"raw.githubusercontent.com", 80, true},
		{"raw.githubusercontent.com", 22, false},
		{"githubusercontent.com", 443, false}, // a wildcard is subdomains only
		{"github.com", 22, true},
		{"github.com", 443, true},
		{"github.com", 80, false}, // its ports replace the default
		{"GitHub.com.", 443, true},
		{"gist.github.com", 443, false}, // exact deny beats the wildcard
		{"api.github.com", 443, true},
		{"api.github.com", 22, false},
		// Among the rules that cover a port, the most specific decides: the
		// exact rule says nothing about 443, so the wildcard does.
		{"db.example.com", 5432, true},
		{"db.example.com", 443, true},
		{"www.example.com", 5432, false},
		{"example.org", 443, false}, // no match is a deny
	} {
		if got := p.Allows(c.name, c.port); got != c.want {
			t.Errorf("Allows(%q, %d) = %v, want %v", c.name, c.port, got, c.want)
		}
	}
	for name, want := range map[string]bool{"github.com": true, "gist.github.com": false, "db.example.com": true, "example.org": false} {
		if p.Resolves(name) != want {
			t.Errorf("Resolves(%q) = %v", name, !want)
		}
	}
}

func TestStarAndDenies(t *testing.T) {
	p, _ := Compile([]api.Rule{allow("*"), deny("evil.com"), allow("evil.com")})
	if !p.Restrictive() || p.Allows("evil.com", 443) || !p.Allows("good.com", 443) || p.Allows("good.com", 22) {
		t.Fatal("* with a deny")
	}
	none, _ := Compile(nil)
	if none.Restrictive() || !none.Allows("anything.com", 25) || !(*Policy)(nil).Allows("x.com", 1) {
		t.Fatal("no rules is the whole internet")
	}
	all, _ := Compile([]api.Rule{deny("*")})
	if all.Allows("github.com", 443) || all.Resolves("github.com") {
		t.Fatal("deny * still allowed something")
	}
}

func TestInvalid(t *testing.T) {
	for _, r := range []api.Rule{
		{Domain: "github.com"},
		{Domain: "github.com", Action: "maybe"},
		allow("1.2.3.4"),
		allow("github.com:22"),
		allow("*github.com"),
		allow("a.*.com"),
		allow("github.com", 0),
		allow("github.com", 70000),
		{Domain: "github.com", Action: api.RuleAllow, Ports: []int{}},
		{Domain: "evil.com", Action: api.RuleDeny, Ports: []int{22}},
		{Include: "everything"},
		{Include: "defaults", Domain: "x.com"},
		allow("fe80::1"),
	} {
		if err := Validate([]api.Rule{r}); err == nil {
			t.Errorf("%+v was accepted", r)
		}
	}
	if err := Validate(make([]api.Rule, MaxRules+1)); err == nil {
		t.Error("too many rules accepted")
	}
}

func TestExpandAndHTTPS(t *testing.T) {
	rules, err := Expand([]api.Rule{{Include: "defaults"}, allow("Example.COM")})
	if err != nil || len(rules) != len(defaultDomains)+1 || rules[len(rules)-1].Domain != "example.com" {
		t.Fatalf("expand: %v %v", err, rules)
	}
	p, _ := Compile(rules)
	if !p.Allows("pypi.org", 443) || p.Allows("pypi.org", 22) {
		t.Fatal("defaults")
	}

	p, _ = Compile([]api.Rule{allow("pypi.org"), deny("evil.com")})
	if err := p.AllowHTTPS([]string{"api.github.com/repos/", "*.openai.com", "evil.com"}); err != nil {
		t.Fatal(err)
	}
	if !p.Allows("api.github.com", 443) || p.Allows("api.github.com", 80) || !p.Allows("x.openai.com", 443) || p.Allows("evil.com", 443) {
		t.Fatal("a secret's hosts")
	}
	open, _ := Compile(nil)
	open.AllowHTTPS([]string{"api.github.com"})
	if open.Restrictive() {
		t.Fatal("a secret made a sandbox restricted")
	}
}

func TestValidHost(t *testing.T) {
	for h, ok := range map[string]bool{
		"api.github.com": true, "*.github.com": true, "api.github.com/repos/": true,
		"*": false, "API.github.com": false, "api.github.com:443": false, "1.2.3.4": false, "a.com/x?y": false,
	} {
		if (ValidHost(h) == nil) != ok {
			t.Errorf("ValidHost(%q) ok = %v", h, !ok)
		}
	}
}
