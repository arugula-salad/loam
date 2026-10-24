package e2b

import (
	"context"
	"strings"
	"testing"

	"github.com/arugula-salad/loam/internal/nodelet/network"
)

func TestSecretLifecycleAndEgressPolicy(t *testing.T) {
	fx := newFixture(t)
	fx.node.SetBroker(true)
	value := "write-only-test-value-123"
	r := fx.do("POST", "/secrets", fx.key, map[string]any{"name": "inference-key", "value": value, "metadata": map[string]string{"purpose": "inference"}})
	if r.Code != 201 || strings.Contains(r.String(), value) {
		t.Fatal(r.Code, r.String())
	}
	info := decode[map[string]any](t, r)
	sid := info["secretID"].(string)
	if info["currentVersion"] != float64(1) {
		t.Fatal(info)
	}
	if r = fx.do("GET", "/secrets/"+sid, fx.other, nil); r.Code != 404 {
		t.Fatal("cross-project read", r.Code)
	}
	if r = fx.do("POST", "/secrets/"+sid, fx.other, map[string]any{"value": "other-value"}); r.Code != 404 {
		t.Fatal("cross-project update", r.Code)
	}
	records, err := fx.store.E2BSecrets(context.Background(), "p")
	if err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	if strings.Contains(string(records[0].Sealed), value) {
		t.Fatal("plaintext secret stored")
	}
	id := fx.create(map[string]any{"allow_internet_access": false, "network": map[string]any{"rules": map[string]any{"api.example.com": []any{map[string]any{"transform": map[string]any{"headers": map[string]string{"Authorization": "Bearer ${e2b.secrets.inference-key}", "X-Literal": "yes"}}}}}}})
	pol, ok := fx.node.Policy(id)
	if !ok || len(pol.RequestHeaders) != 1 {
		t.Fatal("no header policy", pol)
	}
	if pol.RequestHeaders[0].Headers["Authorization"] != "Bearer "+value {
		t.Fatal("reference not resolved")
	}
	compiled, err := network.Compile(pol)
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Allows("api.example.com", 443) {
		t.Fatal("transform granted network access")
	}
	sb := fx.sandbox(id)
	for _, v := range sb.Env {
		if strings.Contains(v, value) {
			t.Fatal("secret in guest env")
		}
	}
	raw := fx.do("GET", "/sandboxes/"+id, fx.key, nil).String()
	if strings.Contains(raw, value) {
		t.Fatal("secret in sandbox response")
	}
	r = fx.do("POST", "/secrets/inference-key", fx.key, map[string]any{"value": "rotated-secret-value-456"})
	if r.Code != 200 || decode[map[string]any](t, r)["currentVersion"] != float64(2) {
		t.Fatal(r.Code, r.String())
	}
	pol, _ = fx.node.Policy(id)
	if pol.RequestHeaders[0].Headers["Authorization"] != "Bearer rotated-secret-value-456" {
		t.Fatal("rotation not pushed")
	}
	r = fx.do("DELETE", "/secrets/"+sid, fx.key, nil)
	if r.Code != 204 {
		t.Fatal(r.Code, r.String())
	}
	pol, _ = fx.node.Policy(id)
	if !pol.RequestHeaders[0].Unavailable {
		t.Fatal("deletion did not revoke")
	}
	if r = fx.do("GET", "/secrets", fx.key, nil); r.Code != 200 || r.String() != "[]\n" {
		t.Fatal(r.Code, r.String())
	}
}
