package network

import (
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"testing"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

var (
	quiet    = slog.New(slog.NewTextHandler(io.Discard, nil))
	loopback = netip.MustParseAddr("127.0.0.1")
)

func mustCompile(t *testing.T, p nodeapi.Policy) *Policy {
	t.Helper()
	c, err := Compile(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func allow(domain string, ports ...int) api.Rule {
	return api.Rule{Domain: domain, Action: api.RuleAllow, Ports: ports}
}

func secret(name string, hosts ...string) nodeapi.PolicySecret {
	return nodeapi.PolicySecret{Name: name, Value: "value-of-" + name, Hosts: hosts, Placeholder: "__loam_" + strings.ToLower(name) + "__"}
}

func TestCompile(t *testing.T) {
	p := mustCompile(t, nodeapi.Policy{
		Egress:  []api.Rule{allow("github.com"), allow("*.example.com", 22)},
		Secrets: []nodeapi.PolicySecret{secret("GH", "api.github.com/repos/", "*.githubusercontent.com")},
	})
	if !p.Restricted() || p.set() != setRestricted {
		t.Errorf("restricted: %v %s", p.Restricted(), p.set())
	}
	for _, c := range []struct {
		name string
		port int
		want bool
	}{
		{"github.com", 443, true}, {"github.com", 22, false}, {"git.example.com", 22, true}, {"git.example.com", 443, false},
		// A secret's hosts are allowed on 443, and only there.
		{"api.github.com", 443, true}, {"api.github.com", 80, false}, {"raw.githubusercontent.com", 443, true},
		{"example.org", 443, false},
	} {
		if got := p.Allows(c.name, c.port); got != c.want {
			t.Errorf("Allows(%s, %d) = %v", c.name, c.port, got)
		}
	}
	if !p.Brokers("api.github.com") || !p.Brokers("raw.githubusercontent.com") || p.Brokers("githubusercontent.com") || p.Brokers("github.com") {
		t.Error("Brokers")
	}
	h := p.Secrets()[0].Hosts[0]
	if h.Name != "api.github.com" || h.Path != "/repos/" || !h.MatchPath("/repos/x") || h.MatchPath("/user") {
		t.Errorf("host %+v", h)
	}

	// Secrets alone don't restrict: only their 443 goes to the proxy.
	s := mustCompile(t, nodeapi.Policy{Secrets: []nodeapi.PolicySecret{secret("GH", "api.github.com")}})
	if s.Restricted() || s.set() != setHTTPS || !s.Allows("anything.org", 25) {
		t.Errorf("secrets only: %v %s", s.Restricted(), s.set())
	}
	if mustCompile(t, nodeapi.Policy{}).set() != "" {
		t.Error("an empty policy is in a set")
	}

	for name, bad := range map[string]nodeapi.Policy{
		"an address":          {Egress: []api.Rule{allow("10.0.0.1")}},
		"a bad action":        {Egress: []api.Rule{{Domain: "github.com", Action: "permit"}}},
		"a bad secret name":   {Secrets: []nodeapi.PolicySecret{secret("gh", "api.github.com")}},
		"a value with CRLF":   {Secrets: []nodeapi.PolicySecret{{Name: "GH", Value: "a\r\nb", Hosts: []string{"x.com"}, Placeholder: "__loam_gh__"}}},
		"a weak placeholder":  {Secrets: []nodeapi.PolicySecret{{Name: "GH", Value: "v", Hosts: []string{"x.com"}, Placeholder: "token"}}},
		"no hosts":            {Secrets: []nodeapi.PolicySecret{secret("GH")}},
		"a host with a port":  {Secrets: []nodeapi.PolicySecret{secret("GH", "x.com:8443")}},
		"a bad inject header": {Secrets: []nodeapi.PolicySecret{withInject(secret("GH", "x.com"), "x api key", "{value}")}},
		"a format without":    {Secrets: []nodeapi.PolicySecret{withInject(secret("GH", "x.com"), "x-api-key", "Bearer")}},
		"a format twice":      {Secrets: []nodeapi.PolicySecret{withInject(secret("GH", "x.com"), "x-api-key", "{value}{value}")}},
		"a name twice":        {Secrets: []nodeapi.PolicySecret{secret("GH", "x.com"), secret("GH", "y.com")}},
	} {
		if _, err := Compile(bad); err == nil {
			t.Errorf("%s compiled", name)
		}
	}
	// An empty value is a secret loam-api couldn't get, not a bad one: the
	// broker refuses requests that need it.
	if _, err := Compile(nodeapi.Policy{Secrets: []nodeapi.PolicySecret{{Name: "GH", Hosts: []string{"x.com"}, Placeholder: "sk-__loam_gh__"}}}); err != nil {
		t.Errorf("an empty value: %v", err)
	}
}

func withInject(s nodeapi.PolicySecret, header, format string) nodeapi.PolicySecret {
	s.Inject = &api.Inject{Header: header, Format: format}
	return s
}

// A flow is judged by the name that authorised it and its port.
func TestPermits(t *testing.T) {
	p := mustCompile(t, nodeapi.Policy{Egress: []api.Rule{allow("github.com", 22, 443)}})
	if !p.permits("github.com", 22) || p.permits("github.com", 80) || p.permits("", 443) {
		t.Error("restricted")
	}
	var none *Policy
	if !none.permits("", 25) || !mustCompile(t, nodeapi.Policy{}).permits("", 25) {
		t.Error("unrestricted")
	}
}
