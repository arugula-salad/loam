//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/loam/pkg/api"
	"github.com/arugula-salad/loam/pkg/client"
)

// The phase 3 API surface (docs/API.md, "Network" and "Secrets"): the
// records, and what a sandbox's env holds. That the node enforces them is
// docs/PHASE3-TEST.md's.

// uniqueSecret is a secret name for this test: an env name, never reused.
func uniqueSecret(tag string) string {
	var b [5]byte
	rand.Read(b[:])
	return "E2E_" + tag + "_" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])
}

// putSecret puts a secret for this test, deleting it when the test ends; a
// deployment without a secret key skips the test.
func putSecret(t *testing.T, ctx context.Context, c *client.Client, name string, req api.PutSecret) *api.Secret {
	t.Helper()
	s, err := c.PutSecret(ctx, name, req)
	if client.ErrorCode(err) == api.CodeUnsupported {
		t.Skip("this deployment has no secret key")
	}
	if err != nil {
		t.Fatalf("put %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := c.DeleteSecret(ctx, name); err != nil && !client.IsNotFound(err) {
			t.Errorf("cleanup: delete secret %s: %v", name, err)
		}
	})
	return s
}

func TestSecrets(t *testing.T) {
	c := newClient(t)
	ctx := testCtx(t, 5*time.Minute)
	name := uniqueSecret("TOKEN")
	value := "e2e-value-" + strings.ToLower(name)
	placeholder := "__loam_" + strings.ToLower(name) + "__"

	s := putSecret(t, ctx, c, name, api.PutSecret{Value: value, Hosts: []string{"api.github.com", "*.example.com/v1/"}})
	if s.Name != name || s.Placeholder != placeholder || s.Inject != nil || len(s.Hosts) != 2 {
		t.Fatalf("put: %+v", s)
	}
	got, err := c.GetSecret(ctx, name)
	if err != nil || got.Placeholder != placeholder {
		t.Fatalf("get: %+v %v", got, err)
	}
	l, err := c.ListSecrets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range l {
		found = found || x.Name == name
	}
	if !found {
		t.Fatalf("list: %s missing", name)
	}
	_, err = c.PutSecret(ctx, name, api.PutSecret{Value: "a\nb", Hosts: []string{"api.github.com"}})
	if client.ErrorCode(err) != api.CodeInvalid {
		t.Fatalf("a value with a newline: %v", err)
	}

	// The sandbox's env holds the placeholder; nothing it can read holds the value.
	sb := newSandbox(t, ctx, c, "secret", api.CreateSandbox{Secrets: &[]string{name}})
	if strings.Join(sb.Secrets, ",") != name {
		t.Fatalf("created: %+v", sb.Secrets)
	}
	if out := sh(t, ctx, c, sb.Name, "printenv "+name); out != placeholder+"\n" {
		t.Fatalf("printenv: %q", out)
	}
	if out := sh(t, ctx, c, sb.Name, fmt.Sprintf("grep -rsl -- %q /proc/1/environ /proc/self/environ /etc /home 2>/dev/null || true", value)); out != "" {
		t.Fatalf("the value is inside the sandbox: %s", out)
	}
	// A replaced secret keeps its placeholder.
	if _, err := c.PutSecret(ctx, name, api.PutSecret{Value: value + "-2", Hosts: []string{"api.github.com"}}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if out := sh(t, ctx, c, sb.Name, "printenv "+name); out != placeholder+"\n" {
		t.Fatalf("printenv after the replace: %q", out)
	}

	// Refused: a secret the project lacks, and one that shadows env.
	_, err = c.CreateSandbox(ctx, api.CreateSandbox{Name: uniqueName("nosec"), Secrets: &[]string{name + "_NOPE"}})
	if client.ErrorCode(err) != api.CodeInvalid {
		t.Fatalf("a missing secret: %v", err)
	}
	_, err = c.CreateSandbox(ctx, api.CreateSandbox{Name: uniqueName("shadow"), Secrets: &[]string{name}, Env: map[string]string{name: "x"}})
	if client.ErrorCode(err) != api.CodeInvalid {
		t.Fatalf("a secret shadowing env: %v", err)
	}

	// Named, it can't be deleted; unnamed, it can.
	if err := c.DeleteSecret(ctx, name); client.ErrorCode(err) != api.CodeConflict {
		t.Fatalf("delete while named: %v", err)
	}
	none := []string{}
	up, err := c.UpdateSandbox(ctx, sb.Name, api.UpdateSandbox{Secrets: &none})
	if err != nil || len(up.Secrets) != 0 {
		t.Fatalf("patch: %+v %v", up, err)
	}
	if err := c.DeleteSecret(ctx, name); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.GetSecret(ctx, name); !client.IsNotFound(err) {
		t.Fatalf("get after delete: %v", err)
	}
}

func TestNetworkRecord(t *testing.T) {
	c := newClient(t)
	ctx := testCtx(t, 8*time.Minute)

	defaults, err := c.NetworkDefaults(ctx)
	if err != nil || len(defaults) == 0 {
		t.Fatalf("defaults: %d %v", len(defaults), err)
	}
	plain := newSandbox(t, ctx, c, "open", api.CreateSandbox{})
	if plain.Network.Egress == nil || len(plain.Network.Egress) != 0 || plain.Secrets == nil {
		t.Fatalf("a plain sandbox's boundary: %+v %+v", plain.Network, plain.Secrets)
	}

	rules := []api.Rule{{Include: "defaults"}, {Domain: "example.com", Action: api.RuleAllow, Ports: []int{443, 8443}},
		{Domain: "gist.github.com", Action: api.RuleDeny}}
	sb := newSandbox(t, ctx, c, "egress", api.CreateSandbox{Network: &api.Network{Egress: rules}})
	if fmt.Sprint(sb.Network.Egress) != fmt.Sprint(rules) {
		t.Fatalf("created: %+v", sb.Network)
	}
	_, err = c.CreateSandbox(ctx, api.CreateSandbox{Name: uniqueName("badrule"), Network: &api.Network{Egress: []api.Rule{{Domain: "10.0.0.1", Action: api.RuleAllow}}}})
	if client.ErrorCode(err) != api.CodeInvalid {
		t.Fatalf("an address as a rule: %v", err)
	}

	// A fork keeps its source's rules unless it gives its own.
	if _, err := c.CreateCheckpoint(ctx, sb.Name, api.CreateCheckpoint{}); err != nil {
		t.Fatal(err)
	}
	cps, err := c.ListCheckpoints(ctx, sb.Name)
	if err != nil || len(cps) == 0 {
		t.Fatalf("checkpoints: %v", err)
	}
	fork, err := c.Fork(ctx, sb.Name, cps[len(cps)-1].ID, uniqueName("fork"))
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	cleanup(t, c, fork.Name)
	if fmt.Sprint(fork.Network.Egress) != fmt.Sprint(rules) {
		t.Fatalf("fork: %+v", fork.Network)
	}

	// PATCH replaces the list, on an awake sandbox and a paused one.
	narrow := []api.Rule{{Domain: "example.com", Action: api.RuleAllow}}
	up, err := c.UpdateSandbox(ctx, sb.Name, api.UpdateSandbox{Network: &api.Network{Egress: narrow}})
	if err != nil || fmt.Sprint(up.Network.Egress) != fmt.Sprint(narrow) {
		t.Fatalf("patch awake: %+v %v", up, err)
	}
	if _, err := c.Pause(ctx, sb.Name); err != nil {
		t.Fatal(err)
	}
	up, err = c.UpdateSandbox(ctx, sb.Name, api.UpdateSandbox{Network: &api.Network{Egress: []api.Rule{}}})
	if err != nil || len(up.Network.Egress) != 0 || up.State != api.StatePaused {
		t.Fatalf("patch paused: %+v %v", up, err)
	}
	if w, err := c.Wake(ctx, sb.Name); err != nil || w.State != api.StateRunning {
		t.Fatalf("wake: %+v %v", w, err)
	}
}
