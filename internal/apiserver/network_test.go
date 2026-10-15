package apiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/arugula-salad/loam/internal/netpolicy"
	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/internal/nodeapi/fake"
	"github.com/arugula-salad/loam/pkg/api"
)

const ghValue = "ghp_0123456789abcdef"

// putGH puts the GITHUB_TOKEN secret the tests use.
func (h *harness) putGH(key, value string) api.Secret {
	h.t.Helper()
	var sec api.Secret
	h.must(200, "PUT", "/v1/secrets/GITHUB_TOKEN", key, api.PutSecret{Value: value, Hosts: []string{"api.github.com", "github.com"}}, &sec)
	return sec
}

// policy is the policy sandbox id's VM holds on node, failing when it holds none.
func policyOn(t *testing.T, node *fake.Nodelet, id string) nodeapi.Policy {
	t.Helper()
	p, ok := node.Policy(id)
	if !ok {
		t.Fatalf("%s holds no policy for %s", node.Name, id)
	}
	return p
}

func TestSealer(t *testing.T) {
	for _, bad := range []string{"", "not base64!", "AAAA"} {
		if _, err := ParseSecretKey(bad); err == nil {
			t.Errorf("ParseSecretKey(%q) took it", bad)
		}
	}
	key, err := ParseSecretKey(" " + strings.Repeat("A", 43) + "=\n")
	if err != nil || len(key) != SecretKeySize {
		t.Fatalf("a good key: %v", err)
	}
	sl, err := newSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	sealed := sl.seal("jake", "GITHUB_TOKEN", []byte(ghValue))
	if bytes.Contains(sealed, []byte(ghValue)) {
		t.Fatal("the sealed value holds the value")
	}
	if again := sl.seal("jake", "GITHUB_TOKEN", []byte(ghValue)); bytes.Equal(again, sealed) {
		t.Fatal("two seals of one value are the same: the nonce isn't random")
	}
	if got, err := sl.open("jake", "GITHUB_TOKEN", sealed); err != nil || string(got) != ghValue {
		t.Fatalf("open: %q %v", got, err)
	}
	// A sealed value opens only where it was sealed, and only as it was.
	for _, c := range []struct{ project, name string }{{"other", "GITHUB_TOKEN"}, {"jake", "OTHER"}, {"jak", "eGITHUB_TOKEN"}} {
		if _, err := sl.open(c.project, c.name, sealed); err == nil {
			t.Errorf("opened as %s/%s", c.project, c.name)
		}
	}
	for i := range sealed {
		tampered := bytes.Clone(sealed)
		tampered[i] ^= 1
		if _, err := sl.open("jake", "GITHUB_TOKEN", tampered); err == nil {
			t.Fatalf("opened with byte %d flipped", i)
		}
	}
	if _, err := sl.open("jake", "GITHUB_TOKEN", sealed[:5]); err == nil {
		t.Fatal("opened a stub")
	}
	other, _ := newSealer(bytes.Repeat([]byte{1}, SecretKeySize))
	if _, err := other.open("jake", "GITHUB_TOKEN", sealed); err == nil {
		t.Fatal("opened under another key")
	}
}

func TestSecretRoutes(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	other := h.project("other")

	good := api.PutSecret{Value: ghValue, Hosts: []string{"api.github.com"}}
	with := func(f func(*api.PutSecret)) api.PutSecret {
		p := good
		f(&p)
		return p
	}
	for name, body := range map[string]api.PutSecret{
		"lower":                 good,
		"9LIVES":                good,
		strings.Repeat("A", 65): good,
		"EMPTY":                 with(func(p *api.PutSecret) { p.Value = "" }),
		"BIG":                   with(func(p *api.PutSecret) { p.Value = strings.Repeat("x", 8<<10+1) }),
		"NEWLINE":               with(func(p *api.PutSecret) { p.Value = "a\nb" }),
		"CR":                    with(func(p *api.PutSecret) { p.Value = "a\rb" }),
		"NUL":                   with(func(p *api.PutSecret) { p.Value = "a\x00b" }),
		"NO_HOSTS":              with(func(p *api.PutSecret) { p.Hosts = nil }),
		"IP_HOST":               with(func(p *api.PutSecret) { p.Hosts = []string{"10.0.0.1"} }),
		"PORT_HOST":             with(func(p *api.PutSecret) { p.Hosts = []string{"api.github.com:443"} }),
		"STAR_HOST":             with(func(p *api.PutSecret) { p.Hosts = []string{"*"} }),
		"UPPER_HOST":            with(func(p *api.PutSecret) { p.Hosts = []string{"API.github.com"} }),
		"QUERY_HOST":            with(func(p *api.PutSecret) { p.Hosts = []string{"api.github.com/x?y"} }),
		"BAD_HEADER":            with(func(p *api.PutSecret) { p.Inject = &api.Inject{Header: "x api", Format: "{value}"} }),
		"HOST_HEADER":           with(func(p *api.PutSecret) { p.Inject = &api.Inject{Header: "Host", Format: "{value}"} }),
		"NO_VALUE":              with(func(p *api.PutSecret) { p.Inject = &api.Inject{Header: "x-api-key", Format: "Bearer"} }),
		"TWO_VALUES":            with(func(p *api.PutSecret) { p.Inject = &api.Inject{Header: "x-api-key", Format: "{value}{value}"} }),
		"FORMAT_NEWLINE":        with(func(p *api.PutSecret) { p.Inject = &api.Inject{Header: "x-api-key", Format: "{value}\nx: y"} }),
		"PREFIX_SPACE":          with(func(p *api.PutSecret) { p.PlaceholderPrefix = "sk ant" }),
	} {
		h.errCode(400, api.CodeInvalid, "PUT", "/v1/secrets/"+name, key, body)
	}
	h.errCode(400, api.CodeInvalid, "PUT", "/v1/secrets/X", key, map[string]any{"value": "v", "hosts": []string{"a.com"}, "extra": 1})

	status, raw, _ := h.call("PUT", "/v1/secrets/GITHUB_TOKEN", key, api.PutSecret{Value: ghValue,
		Hosts: []string{"api.github.com/repos/", "*.github.com"}, PlaceholderPrefix: "ghp_"})
	var sec api.Secret
	if status != 200 || json.Unmarshal(raw, &sec) != nil || bytes.Contains(raw, []byte(ghValue)) {
		t.Fatalf("put: %d %s", status, raw)
	}
	if sec.Name != "GITHUB_TOKEN" || sec.Placeholder != "ghp___loam_github_token__" || sec.Inject != nil ||
		strings.Join(sec.Hosts, " ") != "api.github.com/repos/ *.github.com" || !sec.CreatedAt.Equal(h.clock.Now()) {
		t.Fatalf("put: %+v", sec)
	}
	// Stored sealed, never as it is.
	rec, err := h.store.Secret(context.Background(), "jake", "GITHUB_TOKEN")
	if err != nil || bytes.Contains(rec.Value, []byte(ghValue)) {
		t.Fatalf("stored: %v", err)
	}

	h.must(200, "PUT", "/v1/secrets/ANTHROPIC_API_KEY", key, api.PutSecret{Value: "sk-1", Hosts: []string{"api.anthropic.com"},
		Inject: &api.Inject{Header: "x-api-key", Format: "{value}"}}, nil)
	var l api.SecretList
	h.must(200, "GET", "/v1/secrets", key, nil, &l)
	if len(l.Secrets) != 2 || l.Secrets[0].Name != "ANTHROPIC_API_KEY" || l.Secrets[0].Inject == nil || l.Secrets[1].Name != "GITHUB_TOKEN" {
		t.Fatalf("list: %+v", l)
	}
	h.must(200, "GET", "/v1/secrets", other, nil, &l)
	if len(l.Secrets) != 0 {
		t.Fatalf("another project sees %+v", l)
	}
	h.errCode(404, api.CodeNotFound, "GET", "/v1/secrets/GITHUB_TOKEN", other, nil)
	h.errCode(404, api.CodeNotFound, "DELETE", "/v1/secrets/GITHUB_TOKEN", other, nil)
	_, raw, _ = h.call("GET", "/v1/secrets/GITHUB_TOKEN", key, nil)
	if bytes.Contains(raw, []byte(ghValue)) || !bytes.Contains(raw, []byte(`"inject":null`)) {
		t.Fatalf("get: %s", raw)
	}

	// Replacing keeps created_at.
	h.clock.Advance(60e9)
	sec = h.putGH(key, "ghp_new")
	if sec.UpdatedAt.Equal(sec.CreatedAt) || sec.Placeholder != "__loam_github_token__" {
		t.Fatalf("replaced: %+v", sec)
	}

	// Deleting one a sandbox names is a conflict, until it doesn't.
	h.create(key, api.CreateSandbox{Name: "box", Secrets: &[]string{"GITHUB_TOKEN"}})
	h.errCode(409, api.CodeConflict, "DELETE", "/v1/secrets/GITHUB_TOKEN", key, nil)
	h.must(200, "PATCH", "/v1/sandboxes/box", key, map[string]any{"secrets": []string{}}, nil)
	h.must(204, "DELETE", "/v1/secrets/GITHUB_TOKEN", key, nil, nil)
	h.errCode(404, api.CodeNotFound, "GET", "/v1/secrets/GITHUB_TOKEN", key, nil)
}

// Without a secret key the secret routes, and a sandbox that would use
// secrets, are unsupported; everything else works.
func TestNoSecretKey(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	h.putGH(key, ghValue)
	h.create(key, api.CreateSandbox{Name: "src", Secrets: &[]string{"GITHUB_TOKEN"}})
	h.must(201, "POST", "/v1/sandboxes/src/checkpoints", key, map[string]any{}, nil)
	h.srv.sealer = nil

	h.errCode(501, api.CodeUnsupported, "PUT", "/v1/secrets/X", key, api.PutSecret{Value: "v", Hosts: []string{"a.com"}})
	h.errCode(501, api.CodeUnsupported, "GET", "/v1/secrets", key, nil)
	h.errCode(501, api.CodeUnsupported, "GET", "/v1/secrets/GITHUB_TOKEN", key, nil)
	h.errCode(501, api.CodeUnsupported, "DELETE", "/v1/secrets/GITHUB_TOKEN", key, nil)
	h.errCode(501, api.CodeUnsupported, "POST", "/v1/sandboxes", key, api.CreateSandbox{Secrets: &[]string{"GITHUB_TOKEN"}})
	// A fork inherits its source's secrets, so it is unsupported too, unless it gives none.
	h.errCode(501, api.CodeUnsupported, "POST", "/v1/sandboxes", key, api.CreateSandbox{From: &api.From{Sandbox: "src", Checkpoint: "v1"}})
	h.create(key, api.CreateSandbox{Name: "plain", From: &api.From{Sandbox: "src", Checkpoint: "v1"}, Secrets: &[]string{}})
	h.errCode(501, api.CodeUnsupported, "PATCH", "/v1/sandboxes/plain", key, map[string]any{"secrets": []string{"GITHUB_TOKEN"}})
	// A sandbox that has secrets keeps its record routes, but can't wake
	// without the key that opens them.
	h.must(200, "PATCH", "/v1/sandboxes/src", key, map[string]any{"env": map[string]string{"A": "1"}}, nil)
	h.must(200, "POST", "/v1/sandboxes/src/pause", key, nil, nil)
	h.errCode(501, api.CodeUnsupported, "POST", "/v1/sandboxes/src/wake", key, nil)
	h.create(key, api.CreateSandbox{Name: "free", Network: &api.Network{Egress: []api.Rule{{Domain: "github.com", Action: "allow"}}}})
}

func TestNetworkAndSecretsOnCreate(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	h.putGH(key, ghValue)

	// A bare sandbox shows an empty boundary, not nulls, and its VM gets an
	// empty policy.
	_, raw, _ := h.call("POST", "/v1/sandboxes", key, api.CreateSandbox{Name: "bare"})
	if !bytes.Contains(raw, []byte(`"network":{"egress":[]}`)) || !bytes.Contains(raw, []byte(`"secrets":[]`)) {
		t.Fatalf("bare: %s", raw)
	}
	bare := h.get(key, "bare")
	if p := policyOn(t, h.node, bare.ID); p.Egress == nil || p.Secrets == nil || len(p.Egress)+len(p.Secrets) != 0 {
		t.Fatalf("bare policy: %+v", p)
	}

	rules := []api.Rule{{Include: "defaults"}, {Domain: "Example.COM", Action: "allow", Ports: []int{22, 443}}, {Domain: "gist.github.com", Action: "deny"}}
	sb := h.create(key, api.CreateSandbox{Name: "box", Env: map[string]string{"FOO": "bar"},
		Network: &api.Network{Egress: rules}, Secrets: &[]string{"GITHUB_TOKEN"}})
	// Stored and shown as given, includes unexpanded.
	if len(sb.Network.Egress) != 3 || sb.Network.Egress[0].Include != "defaults" || sb.Network.Egress[1].Domain != "Example.COM" ||
		strings.Join(sb.Secrets, ",") != "GITHUB_TOKEN" {
		t.Fatalf("created: %+v", sb)
	}
	if got := h.get(key, "box"); fmt.Sprint(got.Network) != fmt.Sprint(sb.Network) {
		t.Fatalf("get: %+v", got.Network)
	}
	// The node gets the rules expanded, and the secret with its value.
	p := policyOn(t, h.node, sb.ID)
	defaults := netpolicy.Defaults()
	if len(p.Egress) != len(defaults)+2 || fmt.Sprint(p.Egress[0]) != fmt.Sprint(defaults[0]) || p.Egress[len(defaults)].Domain != "example.com" ||
		fmt.Sprint(p.Egress[len(defaults)].Ports) != "[22 443]" || p.Egress[len(defaults)+1].Action != "deny" {
		t.Fatalf("egress: %+v", p.Egress)
	}
	if len(p.Secrets) != 1 || p.Secrets[0].Name != "GITHUB_TOKEN" || p.Secrets[0].Value != ghValue || p.Secrets[0].Inject != nil ||
		p.Secrets[0].Placeholder != "__loam_github_token__" || strings.Join(p.Secrets[0].Hosts, " ") != "api.github.com github.com" {
		t.Fatalf("secrets: %+v", p.Secrets)
	}
	// The wake has the placeholder, never the value; the record's env is the client's.
	_, wake, _ := h.node.VM(sb.ID)
	if wake.Env["GITHUB_TOKEN"] != "__loam_github_token__" || wake.Env["FOO"] != "bar" {
		t.Fatalf("wake env: %+v", wake.Env)
	}
	if _, ok := h.get(key, "box").Env["GITHUB_TOKEN"]; ok {
		t.Fatal("the placeholder leaked into the record's env")
	}

	// Refused before anything reaches a node.
	h.resetCalls()
	for _, req := range []api.CreateSandbox{
		{Network: &api.Network{Egress: []api.Rule{{Domain: "10.0.0.1", Action: "allow"}}}},
		{Network: &api.Network{Egress: []api.Rule{{Domain: "a.com", Action: "permit"}}}},
		{Network: &api.Network{Egress: []api.Rule{{Domain: "a.com", Action: "deny", Ports: []int{22}}}}},
		{Network: &api.Network{Egress: []api.Rule{{Include: "everything"}}}},
		{Secrets: &[]string{"NOPE"}},
		{Secrets: &[]string{"lower"}},
		{Secrets: &[]string{"GITHUB_TOKEN", "GITHUB_TOKEN"}},
		{Secrets: &[]string{"GITHUB_TOKEN"}, Env: map[string]string{"GITHUB_TOKEN": "mine"}},
	} {
		h.errCode(400, api.CodeInvalid, "POST", "/v1/sandboxes", key, req)
	}
	// An include that would take the list over the limit once expanded.
	many := []api.Rule{{Include: "defaults"}}
	for i := len(many); i < netpolicy.MaxRules; i++ {
		many = append(many, api.Rule{Domain: fmt.Sprintf("h%d.example.com", i), Action: "allow"})
	}
	h.errCode(400, api.CodeInvalid, "POST", "/v1/sandboxes", key, api.CreateSandbox{Network: &api.Network{Egress: many}})
	if c := h.callsOf("geek"); len(c) != 0 {
		t.Fatalf("a refused create reached the node: %v", c)
	}
	// An env that would shadow a secret is refused on PATCH too.
	h.errCode(400, api.CodeInvalid, "PATCH", "/v1/sandboxes/box", key, map[string]any{"env": map[string]string{"GITHUB_TOKEN": "x"}})

	var defs api.Network
	h.must(200, "GET", "/v1/network/defaults", key, nil, &defs)
	if fmt.Sprint(defs.Egress) != fmt.Sprint(defaults) {
		t.Fatalf("defaults: %+v", defs)
	}
}

// Every resume, move, restore and fork hands its node the policy again: a
// nodelet forgets it whenever the VM stops running.
func TestPolicyAtEveryWake(t *testing.T) {
	h := newCluster(t, nodeSpec{"a", 8192}, nodeSpec{"b", 6144})
	key := h.project("jake")
	h.putGH(key, ghValue)
	rules := []api.Rule{{Domain: "github.com", Action: "allow"}}
	sb := h.create(key, api.CreateSandbox{Name: "x", Network: &api.Network{Egress: rules}, Secrets: &[]string{"GITHUB_TOKEN"}})
	a, b := h.fakes["a"], h.fakes["b"]
	check := func(node *fake.Nodelet, id, value string) {
		t.Helper()
		p := policyOn(t, node, id)
		if len(p.Egress) != 1 || p.Egress[0].Domain != "github.com" || len(p.Secrets) != 1 || p.Secrets[0].Value != value {
			t.Fatalf("policy on %s: %+v", node.Name, p)
		}
		if _, wake, _ := node.VM(id); wake.Env["GITHUB_TOKEN"] != "__loam_github_token__" {
			t.Fatalf("wake on %s: %+v", node.Name, wake.Env)
		}
	}
	check(a, sb.ID, ghValue)

	h.must(200, "POST", "/v1/sandboxes/x/pause", key, nil, nil)
	if _, ok := a.Policy(sb.ID); ok {
		t.Fatal("a paused VM holds a policy")
	}
	h.must(200, "POST", "/v1/sandboxes/x/wake", key, nil, nil)
	check(a, sb.ID, ghValue)

	// A checkpoint, then a restore of it.
	h.must(201, "POST", "/v1/sandboxes/x/checkpoints", key, map[string]any{}, nil)
	h.putGH(key, "ghp_second")
	check(a, sb.ID, "ghp_second") // replaced at once, while awake
	h.resetCalls()
	h.must(200, "POST", "/v1/sandboxes/x/checkpoints/v1/restore", key, nil, nil)
	if c := h.callsOf("a"); c[len(c)-1] != "restore "+sb.ID+" "+sb.ID+".v1" {
		t.Fatalf("calls: %v", c)
	}
	check(a, sb.ID, "ghp_second")

	// A fork takes its source's boundary, unless it gives its own.
	fork := h.create(key, api.CreateSandbox{Name: "f", From: &api.From{Sandbox: "x", Checkpoint: "v1"}})
	if len(fork.Network.Egress) != 1 || strings.Join(fork.Secrets, ",") != "GITHUB_TOKEN" {
		t.Fatalf("fork: %+v", fork)
	}
	r := h.record(fork.ID)
	check(h.fakes[r.Node], fork.ID, "ghp_second")
	open := h.create(key, api.CreateSandbox{Name: "g", From: &api.From{Sandbox: "x", Checkpoint: "v1"},
		Network: &api.Network{Egress: []api.Rule{}}, Secrets: &[]string{}})
	if p := policyOn(t, h.fakes[h.record(open.ID).Node], open.ID); len(p.Egress)+len(p.Secrets) != 0 {
		t.Fatalf("fork with its own boundary: %+v", p)
	}

	// A move to another node.
	h.must(200, "POST", "/v1/sandboxes/x/pause", key, nil, nil)
	a.SetReady(false)
	h.must(200, "POST", "/v1/sandboxes/x/wake", key, nil, nil)
	if r := h.record(sb.ID); r.Node != "b" {
		t.Fatalf("didn't move: %+v", r)
	}
	check(b, sb.ID, "ghp_second")
}

func TestPatchBoundary(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	h.putGH(key, ghValue)
	sb := h.create(key, api.CreateSandbox{Name: "box", Env: map[string]string{"FOO": "bar"}})

	// On a running sandbox, at once: the policy, then the wake for the placeholder.
	h.resetCalls()
	var got api.Sandbox
	h.must(200, "PATCH", "/v1/sandboxes/box", key, map[string]any{
		"network": api.Network{Egress: []api.Rule{{Include: "defaults"}}}, "secrets": []string{"GITHUB_TOKEN"}}, &got)
	wantCalls(t, h.callsOf("geek"), "policy "+sb.ID, "resume "+sb.ID)
	if len(got.Network.Egress) != 1 || got.Network.Egress[0].Include != "defaults" || strings.Join(got.Secrets, ",") != "GITHUB_TOKEN" {
		t.Fatalf("patched: %+v", got)
	}
	p := policyOn(t, h.node, sb.ID)
	if len(p.Egress) != len(netpolicy.Defaults()) || len(p.Secrets) != 1 || p.Secrets[0].Value != ghValue {
		t.Fatalf("policy: %+v", p)
	}
	if _, wake, _ := h.node.VM(sb.ID); wake.Env["GITHUB_TOKEN"] != "__loam_github_token__" || wake.Env["FOO"] != "bar" {
		t.Fatalf("wake: %+v", wake.Env)
	}
	// The network alone needs no wake.
	h.resetCalls()
	h.must(200, "PATCH", "/v1/sandboxes/box", key, map[string]any{"network": map[string]any{"egress": []any{}}}, nil)
	wantCalls(t, h.callsOf("geek"), "policy "+sb.ID)
	if p := policyOn(t, h.node, sb.ID); len(p.Egress) != 0 || len(p.Secrets) != 1 {
		t.Fatalf("policy: %+v", p)
	}
	// Anything else, nothing at all.
	h.resetCalls()
	h.must(200, "PATCH", "/v1/sandboxes/box", key, map[string]any{"labels": map[string]string{"a": "b"}}, nil)
	wantCalls(t, h.callsOf("geek"))

	// The node refusing it takes it back.
	h.node.Fail("policy", &nodeapi.Err{Status: 501, ErrorDetail: api.ErrorDetail{Code: api.CodeUnsupported, Message: "no broker CA"}})
	h.errCode(501, api.CodeUnsupported, "PATCH", "/v1/sandboxes/box", key, map[string]any{
		"network": api.Network{Egress: []api.Rule{{Domain: "x.com", Action: "allow"}}}, "secrets": []string{}, "labels": map[string]string{}})
	if got := h.get(key, "box"); len(got.Network.Egress) != 0 || len(got.Secrets) != 1 || got.Labels["a"] != "b" {
		t.Fatalf("not taken back: %+v", got)
	}
	// Rules that don't compile and secrets the project lacks change nothing.
	h.errCode(400, api.CodeInvalid, "PATCH", "/v1/sandboxes/box", key, map[string]any{"network": map[string]any{"egress": []any{map[string]any{"domain": "x.com"}}}})
	h.errCode(400, api.CodeInvalid, "PATCH", "/v1/sandboxes/box", key, map[string]any{"secrets": []string{"NOPE"}})

	// A sleeping one gets it at its wake, and nothing reaches its node before.
	h.must(200, "POST", "/v1/sandboxes/box/pause", key, nil, nil)
	h.resetCalls()
	h.must(200, "PATCH", "/v1/sandboxes/box", key, map[string]any{"network": api.Network{Egress: []api.Rule{{Domain: "a.com", Action: "allow"}}}, "secrets": []string{}}, nil)
	wantCalls(t, h.callsOf("geek"))
	h.must(200, "POST", "/v1/sandboxes/box/wake", key, nil, nil)
	if p := policyOn(t, h.node, sb.ID); len(p.Egress) != 1 || p.Egress[0].Domain != "a.com" || len(p.Secrets) != 0 {
		t.Fatalf("policy after the wake: %+v", p)
	}
	if _, wake, _ := h.node.VM(sb.ID); wake.Env["GITHUB_TOKEN"] != "" {
		t.Fatalf("a dropped secret's placeholder is still in the env: %+v", wake.Env)
	}
}

// Replacing a secret reaches every awake sandbox that names it, and only those.
func TestSecretReplaceReachesAwakeSandboxes(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	h.putGH(key, ghValue)
	h.must(200, "PUT", "/v1/secrets/OTHER", key, api.PutSecret{Value: "o", Hosts: []string{"o.com"}}, nil)
	awake := h.create(key, api.CreateSandbox{Name: "awake", Secrets: &[]string{"GITHUB_TOKEN", "OTHER"}})
	asleep := h.create(key, api.CreateSandbox{Name: "asleep", Secrets: &[]string{"GITHUB_TOKEN"}})
	h.create(key, api.CreateSandbox{Name: "unnamed", Secrets: &[]string{"OTHER"}})
	h.must(200, "POST", "/v1/sandboxes/asleep/pause", key, nil, nil)

	h.resetCalls()
	h.putGH(key, "ghp_rotated")
	wantCalls(t, h.callsOf("geek"), "policy "+awake.ID)
	p := policyOn(t, h.node, awake.ID)
	if len(p.Secrets) != 2 || p.Secrets[0].Value != "ghp_rotated" || p.Secrets[1].Value != "o" {
		t.Fatalf("policy: %+v", p)
	}
	h.must(200, "POST", "/v1/sandboxes/asleep/wake", key, nil, nil)
	if p := policyOn(t, h.node, asleep.ID); p.Secrets[0].Value != "ghp_rotated" {
		t.Fatalf("woken: %+v", p)
	}
}

// A sandbox with secrets runs only where the broker CA is: on a broker node
// with room, else capacity; with no broker node at all, unsupported.
func TestBrokerPlacement(t *testing.T) {
	h := newCluster(t, nodeSpec{"big", 16384}, nodeSpec{"small", 4096})
	key := h.project("jake")
	h.putGH(key, ghValue)
	big, small := h.fakes["big"], h.fakes["small"]
	big.SetBroker(false)

	plain := h.create(key, api.CreateSandbox{Name: "plain"})
	if r := h.record(plain.ID); r.Node != "big" {
		t.Fatalf("a sandbox without secrets went to %s", r.Node)
	}
	sec := h.create(key, api.CreateSandbox{Name: "sec", Secrets: &[]string{"GITHUB_TOKEN"}})
	if r := h.record(sec.ID); r.Node != "small" {
		t.Fatalf("a sandbox with secrets went to %s", r.Node)
	}
	// small is full now (4096 of 4096 with a second).
	h.create(key, api.CreateSandbox{Name: "sec2", Secrets: &[]string{"GITHUB_TOKEN"}})
	h.errCode(503, api.CodeCapacity, "POST", "/v1/sandboxes", key, api.CreateSandbox{Secrets: &[]string{"GITHUB_TOKEN"}})
	small.SetBroker(false)
	h.errCode(501, api.CodeUnsupported, "POST", "/v1/sandboxes", key, api.CreateSandbox{Secrets: &[]string{"GITHUB_TOKEN"}})
	small.SetBroker(true)

	// Given secrets while it ran on a node without the CA: refused, and kept as it was.
	h.errCode(501, api.CodeUnsupported, "PATCH", "/v1/sandboxes/plain", key, map[string]any{"secrets": []string{"GITHUB_TOKEN"}})
	if got := h.get(key, "plain"); len(got.Secrets) != 0 {
		t.Fatalf("kept: %+v", got)
	}
	// Given them while it slept there: it wakes on a node with the CA.
	h.must(204, "DELETE", "/v1/sandboxes/sec2", key, nil, nil)
	h.must(200, "POST", "/v1/sandboxes/plain/pause", key, nil, nil)
	h.must(200, "PATCH", "/v1/sandboxes/plain", key, map[string]any{"secrets": []string{"GITHUB_TOKEN"}}, nil)
	h.must(200, "POST", "/v1/sandboxes/plain/wake", key, nil, nil)
	if r := h.record(plain.ID); r.Node != "small" {
		t.Fatalf("woke on %s", r.Node)
	}
	if p := policyOn(t, small, plain.ID); len(p.Secrets) != 1 {
		t.Fatalf("policy: %+v", p)
	}
}
