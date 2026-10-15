// Package netpolicy is a sandbox's egress rules (docs/API.md, "Network"):
// checking them, expanding their includes, and deciding a name and port
// against them. loam-api uses it to refuse a bad rule list before it is
// stored; the nodelet, to enforce one. Copied from sandpit's rules.go and given
// ports.
package netpolicy

import (
	"fmt"
	"slices"
	"strings"

	"github.com/arugula-salad/loam/pkg/api"
)

// MaxRules bounds a policy so a client cannot make every DNS query expensive.
const MaxRules = 1000

// DefaultPorts are the ports an allow rule without ports opens.
var DefaultPorts = []int{80, 443}

// Policy is a compiled rule list. The zero value and nil both mean
// unrestricted: the whole internet.
type Policy struct {
	exact       map[string]*entry // host
	wild        map[string]*entry // "example.com" for a "*.example.com" rule
	all         *entry            // the "*" rule, if any
	ports       []int             // every port some allow rule opens, sorted
	restrictive bool
}

// entry is every rule on one domain pattern. A deny covers every port and
// beats an allow on the same pattern, as two disagreeing rules did in sandpit.
type entry struct {
	deny  bool
	allow map[int]bool
}

// covers reports whether the pattern has a rule that says anything about port.
func (e *entry) covers(port int) bool { return e != nil && (e.deny || e.allow[port]) }

// Validate checks rules as loam-api stores them, includes unexpanded.
func Validate(rules []api.Rule) error {
	_, err := Compile(rules)
	return err
}

// Expand replaces each include with the rules it stands for, which is how
// loam-api hands a policy to a nodelet: every nodelet then enforces the same
// list whatever its own version's defaults.
func Expand(rules []api.Rule) ([]api.Rule, error) {
	if err := Validate(rules); err != nil {
		return nil, err
	}
	out := make([]api.Rule, 0, len(rules))
	for _, r := range rules {
		if r.Include != "" {
			out = append(out, Defaults()...)
			continue
		}
		r.Domain = canonical(r.Domain)
		out = append(out, r)
	}
	return out, nil
}

// Defaults is what {"include": "defaults"} expands to.
func Defaults() []api.Rule {
	out := make([]api.Rule, len(defaultDomains))
	for i, d := range defaultDomains {
		out[i] = api.Rule{Domain: d, Action: api.RuleAllow}
	}
	return out
}

// Compile validates rules and builds the matcher. Rule order carries no
// meaning ("the most specific wins"), so none is kept.
func Compile(rules []api.Rule) (*Policy, error) {
	if len(rules) > MaxRules {
		return nil, fmt.Errorf("too many rules (%d, at most %d)", len(rules), MaxRules)
	}
	p := &Policy{exact: map[string]*entry{}, wild: map[string]*entry{}, restrictive: len(rules) > 0}
	for i, r := range rules {
		if r.Include != "" {
			if r.Domain != "" || r.Action != "" || r.Ports != nil {
				return nil, fmt.Errorf("rule %d: an include has nothing else", i)
			}
			if r.Include != "defaults" {
				return nil, fmt.Errorf("rule %d: unknown include %q (only \"defaults\" exists)", i, r.Include)
			}
			for _, d := range defaultDomains {
				p.add(d, false, DefaultPorts)
			}
			continue
		}
		if r.Action != api.RuleAllow && r.Action != api.RuleDeny {
			return nil, fmt.Errorf("rule %d: action must be \"allow\" or \"deny\"", i)
		}
		d := canonical(r.Domain)
		if err := validDomain(d); err != nil {
			return nil, fmt.Errorf("rule %d: %w", i, err)
		}
		ports := r.Ports
		if r.Action == api.RuleDeny {
			if ports != nil {
				return nil, fmt.Errorf("rule %d: a deny covers every port and takes no ports", i)
			}
		} else if ports == nil {
			ports = DefaultPorts
		} else if len(ports) == 0 {
			return nil, fmt.Errorf("rule %d: an allow with ports needs at least one", i)
		}
		for _, port := range ports {
			if port < 1 || port > 65535 {
				return nil, fmt.Errorf("rule %d: port %d is not a TCP port", i, port)
			}
		}
		p.add(d, r.Action == api.RuleDeny, ports)
	}
	slices.Sort(p.ports)
	p.ports = slices.Compact(p.ports)
	return p, nil
}

// AllowHTTPS adds an allow on 443 for each host, as a secret's hosts are
// allowed in a restricted sandbox (docs/API.md, "Secrets"). A host may carry a
// path, which egress doesn't see. It changes nothing in an unrestricted
// policy: secrets never make a sandbox restricted.
func (p *Policy) AllowHTTPS(hosts []string) error {
	if !p.Restrictive() {
		return nil
	}
	for _, h := range hosts {
		h, _, _ = strings.Cut(h, "/")
		h = canonical(h)
		if err := validDomain(h); err != nil {
			return err
		}
		p.add(h, false, []int{443})
	}
	slices.Sort(p.ports)
	p.ports = slices.Compact(p.ports)
	return nil
}

func (p *Policy) add(domain string, deny bool, ports []int) {
	var e *entry
	switch {
	case domain == "*":
		if p.all == nil {
			p.all = &entry{allow: map[int]bool{}}
		}
		e = p.all
	case strings.HasPrefix(domain, "*."):
		if e = p.wild[domain[2:]]; e == nil {
			e = &entry{allow: map[int]bool{}}
			p.wild[domain[2:]] = e
		}
	default:
		if e = p.exact[domain]; e == nil {
			e = &entry{allow: map[int]bool{}}
			p.exact[domain] = e
		}
	}
	if deny {
		e.deny = true
		return
	}
	for _, port := range ports {
		e.allow[port] = true
		p.ports = append(p.ports, port)
	}
}

// Restrictive reports whether the policy can refuse anything: whether it has
// any rule at all.
func (p *Policy) Restrictive() bool { return p != nil && p.restrictive }

// Allows reports whether a connection to name on port is allowed: among the
// rules that cover port, the most specific that matches name decides. An
// exact rule beats a subdomain wildcard, a longer wildcard beats a shorter
// one, and "*" comes last. "*.example.com" covers subdomains only, not
// example.com itself.
func (p *Policy) Allows(name string, port int) bool {
	if !p.Restrictive() {
		return true
	}
	name = canonical(name)
	if e := p.exact[name]; e.covers(port) {
		return !e.deny
	}
	for rest := name; ; {
		i := strings.IndexByte(rest, '.')
		if i < 0 {
			break
		}
		rest = rest[i+1:]
		if e := p.wild[rest]; e.covers(port) {
			return !e.deny
		}
	}
	if p.all.covers(port) {
		return !p.all.deny
	}
	return false
}

// Resolves reports whether the DNS forwarder answers name: whether it is
// allowed on some port.
func (p *Policy) Resolves(name string) bool {
	if !p.Restrictive() {
		return true
	}
	for _, port := range p.ports {
		if p.Allows(name, port) {
			return true
		}
	}
	return false
}

func canonical(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

// ValidHost checks one of a secret's hosts: a name or "*." and a name, with an
// optional path prefix, and no port (a secret goes to 443 only).
func ValidHost(h string) error {
	name, path, hasPath := strings.Cut(h, "/")
	if hasPath && strings.ContainsAny(path, " ?#") {
		return fmt.Errorf("host %q: a path prefix has no spaces, query or fragment", h)
	}
	if name == "*" || name != canonical(name) {
		return fmt.Errorf("host %q: use a lowercase name or \"*.\" and a name", h)
	}
	return validDomain(name)
}

func validDomain(d string) error {
	if d == "*" {
		return nil
	}
	host := strings.TrimPrefix(d, "*.")
	if host == "" || len(host) > 253 {
		return fmt.Errorf("invalid domain %q", d)
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("invalid domain %q", d)
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				// Also rejects a "*" anywhere but as the whole leading label,
				// a port, and an IPv6 address.
				return fmt.Errorf("invalid domain %q: use an exact name, \"*.example.com\" or \"*\"", d)
			}
		}
	}
	// No top-level domain is all digits, so this is an IPv4 address: rules
	// name hosts, and a restricted sandbox reaches no address it didn't resolve.
	if isNumericTLD(host) {
		return fmt.Errorf("invalid domain %q: an address is not a name", d)
	}
	return nil
}

func isNumericTLD(host string) bool {
	tld := host[strings.LastIndexByte(host, '.')+1:]
	return strings.Trim(tld, "0123456789") == ""
}

// defaultDomains is what {"include": "defaults"} expands to: sandpit's list, our
// reading of "common development domains, GitHub, npm, PyPI, Docker Hub, and the
// major AI APIs among them".
var defaultDomains = []string{
	"github.com", "*.github.com", "*.githubusercontent.com", "*.githubassets.com", "ghcr.io", "*.ghcr.io",
	"gitlab.com", "*.gitlab.com", "bitbucket.org", "*.bitbucket.org",
	"npmjs.org", "*.npmjs.org", "npmjs.com", "*.npmjs.com", "yarnpkg.com", "*.yarnpkg.com", "nodejs.org", "*.nodejs.org",
	"pypi.org", "*.pypi.org", "pythonhosted.org", "*.pythonhosted.org",
	"docker.io", "*.docker.io", "docker.com", "*.docker.com",
	"crates.io", "*.crates.io", "rust-lang.org", "*.rust-lang.org",
	"golang.org", "*.golang.org", "go.dev", "*.go.dev", "pkg.go.dev",
	"rubygems.org", "*.rubygems.org", "hex.pm", "*.hex.pm", "packagist.org", "*.packagist.org",
	"maven.org", "*.maven.org",
	"ubuntu.com", "*.ubuntu.com", "debian.org", "*.debian.org",
	"api.anthropic.com", "api.openai.com", "generativelanguage.googleapis.com",
	"api.mistral.ai", "api.cohere.com", "api.groq.com", "openrouter.ai",
	"huggingface.co", "*.huggingface.co",
}
