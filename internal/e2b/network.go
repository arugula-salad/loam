package e2b

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/arugula-salad/loam/internal/netpolicy"
	"net"
	"net/http"
	"sort"
	"strings"

	"github.com/arugula-salad/loam/pkg/api"
)

// E2B's egress options become loam's egress rules (docs/API.md, "Network"),
// which the sandbox's node enforces outside the guest. E2B lists addresses,
// CIDRs and host names to deny (denyOut) and exceptions to them (allowOut,
// which wins over denyOut); loam's rules are names only, decided by the most
// specific, and a sandbox with any rule reaches nothing else. So:
//
//   - allow_internet_access: false, or 0.0.0.0/0 (the SDKs' ALL_TRAFFIC) in
//     denyOut, is {"domain": "*", "action": "deny"}: nothing, then each name
//     in allowOut allowed on loam's default ports (80 and 443), and each name
//     in denyOut denied;
//   - without that, allowOut makes exceptions to nothing and limits nothing,
//     and a name in denyOut is refused: loam can't allow everything but a
//     name, since a restricted sandbox reaches only what its rules name, and
//     only over TCP;
//   - an address or a CIDR (but ALL_TRAFFIC) is refused anywhere: loam's
//     rules are names, which is what keeps another name on a shared address
//     out;
//   - 0.0.0.0/0 in allowOut is everything, as allowOut wins in E2B;
//   - a denied name that an allowed one covers is refused, since E2B would
//     allow it where loam, deciding by specificity, would deny it.

// allTraffic is the SDKs' ALL_TRAFFIC.
const allTraffic = "0.0.0.0/0"

// networkReq is E2B's network options (SandboxNetworkConfig), the parts this
// adapter knows; anything else in it that isn't null is refused (unknown).
type networkReq struct {
	Rules              map[string][]headerTransform `json:"rules"`
	AllowOut           []string                     `json:"allowOut"`
	DenyOut            []string                     `json:"denyOut"`
	AllowPublicTraffic *bool                        `json:"allowPublicTraffic"`

	unknown []string
}

func (n *networkReq) UnmarshalJSON(b []byte) error {
	type plain networkReq
	if err := json.Unmarshal(b, (*plain)(n)); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	n.unknown = unknownFields(all, "allowOut", "denyOut", "allowPublicTraffic", "rules")
	return nil
}

// unknownFields are the keys of all that aren't known and aren't null.
func unknownFields(all map[string]json.RawMessage, known ...string) []string {
	var out []string
next:
	for k, v := range all {
		for _, kn := range known {
			if k == kn {
				continue next
			}
		}
		if s := strings.TrimSpace(string(v)); s != "null" {
			out = append(out, k)
		}
	}
	return out
}

// isAddress is an IP address or a CIDR.
func isAddress(s string) bool {
	if net.ParseIP(s) != nil {
		return true
	}
	_, _, err := net.ParseCIDR(s)
	return err == nil
}

// covers is whether the loam rule domain a matches the name or pattern d.
func covers(a, d string) bool {
	return a == d || a == "*" || (strings.HasPrefix(a, "*.") && strings.HasSuffix(d, a[1:]))
}

// egressOf is the loam rules for E2B's egress options; nil is none, the
// whole internet. An error is an option loam can't enforce as E2B means it.
func egressOf(allowInternet *bool, n *networkReq) ([]api.Rule, error) {
	blanket := allowInternet != nil && !*allowInternet
	allowAll := false
	var denies, allows []string
	if n != nil {
		if len(n.unknown) > 0 {
			return nil, fmt.Errorf("network.%s is not supported by this server", strings.Join(n.unknown, ", network."))
		}
		for _, d := range n.DenyOut {
			switch {
			case d == allTraffic:
				blanket = true
			case isAddress(d):
				return nil, fmt.Errorf("network.denyOut %q: this server's egress rules are host names, not addresses (only %s, all traffic, is taken)", d, allTraffic)
			default:
				denies = append(denies, strings.ToLower(d))
			}
		}
		for _, a := range n.AllowOut {
			switch {
			case a == allTraffic:
				allowAll = true
			case isAddress(a):
				return nil, fmt.Errorf("network.allowOut %q: this server's egress rules are host names, not addresses", a)
			default:
				allows = append(allows, strings.ToLower(a))
			}
		}
	}
	if !blanket || allowAll {
		if len(denies) > 0 && !allowAll {
			return nil, fmt.Errorf("network.denyOut %q: this server can deny names only in a sandbox that denies all other traffic (denyOut %s, or allow_internet_access false)", denies[0], allTraffic)
		}
		return nil, nil
	}
	for _, d := range denies {
		for _, a := range allows {
			if covers(a, d) {
				return nil, fmt.Errorf("network: allowOut %q covers denyOut %q; E2B lets the allow win, this server the more specific rule, so the two can't be given together", a, d)
			}
		}
	}
	rules := []api.Rule{{Domain: "*", Action: api.RuleDeny}}
	for _, d := range denies {
		rules = append(rules, api.Rule{Domain: d, Action: api.RuleDeny})
	}
	for _, a := range allows {
		rules = append(rules, api.Rule{Domain: a, Action: api.RuleAllow})
	}
	return rules, nil
}

// The E2B rules array is applied in order within a single domain; the last
// assignment of a header wins. Domain selection happens in the broker.
type headerTransform struct {
	Transform struct {
		Headers map[string]string `json:"headers"`
	} `json:"transform"`
}

func (t *headerTransform) UnmarshalJSON(b []byte) error {
	type plain headerTransform
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode((*plain)(t))
}
func headersOf(n *networkReq) ([]api.HeaderRule, error) {
	if n == nil {
		return nil, nil
	}
	out := []api.HeaderRule{}
	for domain, rules := range n.Rules {
		r := api.HeaderRule{Domain: strings.ToLower(domain), Headers: map[string]string{}}
		for _, rule := range rules {
			for k, v := range rule.Transform.Headers {
				r.Headers[http.CanonicalHeaderKey(k)] = v
			}
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out, netpolicy.ValidateHeaders(out)
}
