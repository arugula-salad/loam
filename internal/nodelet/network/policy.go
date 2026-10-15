package network

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/arugula-salad/loam/internal/netpolicy"
	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

// Policy is a VM's nodeapi.Policy, checked and compiled (docs/NODE.md,
// "Phase 3: the network boundary"): its egress rules, with its secrets' hosts
// allowed on 443, and its secrets. It is never changed once compiled: a new
// policy is a new Policy, so whoever holds one sees a consistent whole.
type Policy struct {
	rules   *netpolicy.Policy
	secrets []Secret
}

// Secret is one of a VM's secrets as the broker uses it.
type Secret struct {
	Name, Value, Placeholder string
	Hosts                    []Host
	// Inject, when set, sets a header on every request to the hosts instead
	// of substituting the placeholder.
	Inject *api.Inject
}

// Host is one of a secret's hosts: a name ("api.github.com") or a wildcard
// ("*.github.com", every name under it but not the name itself), and a path
// prefix ("/repos/"), "" for every path.
type Host struct {
	Name, Path string
}

// MatchName reports whether name is one h covers.
func (h Host) MatchName(name string) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if suffix, ok := strings.CutPrefix(h.Name, "*."); ok {
		return strings.HasSuffix(name, "."+suffix)
	}
	return name == h.Name
}

// MatchPath reports whether a request's path, as the client sent it, is one h
// covers.
func (h Host) MatchPath(path string) bool { return strings.HasPrefix(path, h.Path) }

// Limits on a secret (docs/API.md, "Secrets"). loam-api checks them before a
// secret is stored; the nodelet checks again, since it is what writes the
// value into requests.
const (
	maxSecretValue = 8 << 10
	maxSecretName  = 64
)

var (
	secretNameRE  = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	headerTokenRE = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	alnumRE       = regexp.MustCompile(`[A-Za-z0-9]`)
	boundaryRE    = regexp.MustCompile(`[^A-Za-z0-9_]`)
)

// Compile checks p and builds what the forwarder, the proxy and the broker
// decide with. Anything it can't make sense of is an error: the policy would
// otherwise be enforced as something other than what was asked.
func Compile(p nodeapi.Policy) (*Policy, error) {
	rules, err := netpolicy.Compile(p.Egress)
	if err != nil {
		return nil, err
	}
	out := &Policy{rules: rules}
	seen := map[string]bool{}
	for _, s := range p.Secrets {
		sec, err := compileSecret(s)
		if err != nil {
			return nil, fmt.Errorf("secret %s: %w", s.Name, err)
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("secret %s is named twice", s.Name)
		}
		seen[s.Name] = true
		hosts := make([]string, len(sec.Hosts))
		for i, h := range sec.Hosts {
			hosts[i] = h.Name
		}
		if err := rules.AllowHTTPS(hosts); err != nil {
			return nil, fmt.Errorf("secret %s: %w", s.Name, err)
		}
		out.secrets = append(out.secrets, sec)
	}
	return out, nil
}

func compileSecret(s nodeapi.PolicySecret) (Secret, error) {
	if len(s.Name) > maxSecretName || !secretNameRE.MatchString(s.Name) {
		return Secret{}, errors.New("its name must be [A-Z_][A-Z0-9_]*, at most 64 characters")
	}
	// An empty value is a secret loam-api couldn't get: a request that needs
	// it is refused (broker), never sent without it.
	if len(s.Value) > maxSecretValue || strings.ContainsAny(s.Value, "\r\n\x00") {
		return Secret{}, errors.New("its value must be at most 8 KiB, without CR, LF or NUL")
	}
	if !validPlaceholder(s.Placeholder) {
		return Secret{}, fmt.Errorf("placeholder %q is not distinctive enough to replace (at least 4 characters, a letter or digit, and __ at an end or a character outside [A-Za-z0-9_])", s.Placeholder)
	}
	if len(s.Hosts) == 0 {
		return Secret{}, errors.New("it has no hosts")
	}
	out := Secret{Name: s.Name, Value: s.Value, Placeholder: s.Placeholder}
	for _, h := range s.Hosts {
		if err := netpolicy.ValidHost(h); err != nil {
			return Secret{}, err
		}
		name, path, hasPath := strings.Cut(h, "/")
		host := Host{Name: name}
		if hasPath {
			host.Path = "/" + path
		}
		out.Hosts = append(out.Hosts, host)
	}
	if in := s.Inject; in != nil {
		if !headerTokenRE.MatchString(in.Header) {
			return Secret{}, fmt.Errorf("inject header %q is not a header name", in.Header)
		}
		if strings.Count(in.Format, "{value}") != 1 || strings.ContainsAny(in.Format, "\r\n\x00") {
			return Secret{}, errors.New(`inject format must hold "{value}" once, and no CR, LF or NUL`)
		}
		inj := *in
		out.Inject = &inj
	}
	return out, nil
}

// validPlaceholder is managoat's rule for a placeholder: substitution is a
// literal find and replace, so one that occurs in ordinary text ("id",
// "account_sid") would put the value somewhere nobody chose. loam's
// placeholders (__loam_<name>__, after an optional prefix) always pass.
func validPlaceholder(p string) bool {
	return len(p) >= 4 && alnumRE.MatchString(p) &&
		(strings.HasPrefix(p, "__") || strings.HasSuffix(p, "__") || boundaryRE.MatchString(p))
}

// Restricted reports whether the VM has egress rules: then only TCP leaves
// it, through the proxy, and its DNS is filtered.
func (p *Policy) Restricted() bool { return p != nil && p.rules.Restrictive() }

// Allows reports whether a connection to name on port is allowed.
func (p *Policy) Allows(name string, port int) bool { return p == nil || p.rules.Allows(name, port) }

// Resolves reports whether the forwarder answers name.
func (p *Policy) Resolves(name string) bool { return p == nil || p.rules.Resolves(name) }

// Secrets are the VM's secrets. The slice is the policy's own: not to be changed.
func (p *Policy) Secrets() []Secret {
	if p == nil {
		return nil
	}
	return p.secrets
}

// Brokers reports whether name is one of a secret's hosts, whose TLS the
// proxy terminates.
func (p *Policy) Brokers(name string) bool {
	for _, s := range p.Secrets() {
		for _, h := range s.Hosts {
			if h.MatchName(name) {
				return true
			}
		}
	}
	return false
}

// set is the nftables set the VM's address goes in (Ruleset): restricted4
// sends all its TCP to the proxy, https4 only its 443, for the broker.
func (p *Policy) set() string {
	switch {
	case p.Restricted():
		return setRestricted
	case len(p.Secrets()) > 0:
		return setHTTPS
	}
	return ""
}

// permits reports whether a proxied connection, authorized through name via
// to port, may go on under p. An empty via is one made while the VM was
// unrestricted, to wherever it aimed, which no restricted policy allows.
func (p *Policy) permits(via string, port int) bool {
	if !p.Restricted() {
		return true
	}
	return via != "" && p.Allows(via, port)
}
