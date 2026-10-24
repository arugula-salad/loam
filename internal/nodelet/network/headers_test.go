package network

import (
	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
	"testing"
)

func TestHeaderRulesDoNotGrantAccessAndSelectOneDomain(t *testing.T) {
	p, err := Compile(nodeapi.Policy{Egress: []api.Rule{{Domain: "*", Action: "deny"}}, RequestHeaders: []nodeapi.HeaderRule{
		{Domain: "*.example.com", Headers: map[string]string{"X-Broad": "b"}},
		{Domain: "*.sub.example.com", Headers: map[string]string{"X-Narrow": "n"}},
		{Domain: "api.sub.example.com", Headers: map[string]string{}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Allows("api.sub.example.com", 443) || p.Resolves("api.sub.example.com") {
		t.Fatal("headers expanded access")
	}
	if !p.NeedsBroker() || !p.Brokers("api.sub.example.com") {
		t.Fatal("missing broker")
	}
	r, ok := p.HeadersFor("api.sub.example.com")
	if !ok || len(r.Headers) != 0 {
		t.Fatal("exact empty rule did not mask wildcards")
	}
	r, ok = p.HeadersFor("other.sub.example.com")
	if !ok || r.Headers["X-Narrow"] != "n" || len(r.Headers) != 1 {
		t.Fatal("wildcard rules merged")
	}
	if _, ok = p.HeadersFor("example.com"); ok {
		t.Fatal("wildcard matched root domain")
	}
}
