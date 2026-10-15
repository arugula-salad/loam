//go:build e2e

package e2e

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/loam/pkg/api"
)

// TestIdentityToken gets a token from inside a sandbox, on the guest socket,
// and verifies it as a relying party outside the cluster would: discovery
// from the API's /.well-known, the key set it names, the key the token's kid
// picks, then the claims (docs/API.md, "Identity").
func TestIdentityToken(t *testing.T) {
	c := newClient(t)
	ctx := testCtx(t, 5*time.Minute)
	who, err := c.Whoami(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sb := newSandbox(t, ctx, c, "id", api.CreateSandbox{Labels: map[string]string{"case": "identity"}})

	var tok api.IdentityToken
	out := sh(t, ctx, c, sb.Name, `curl -sf --unix-socket /run/loam/api.sock 'http://loam/identity/token?audience=test'`)
	if err := json.Unmarshal([]byte(out), &tok); err != nil || tok.Token == "" {
		t.Fatalf("token: %v: %q", err, out)
	}
	if d := time.Until(tok.ExpiresAt); d < 8*time.Minute || d > 12*time.Minute {
		t.Fatalf("expires_at %v is not about ten minutes away", tok.ExpiresAt)
	}

	var disc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	getJSON(t, ctx, strings.TrimRight(e2eURL(), "/")+"/.well-known/openid-configuration", &disc)
	var set struct {
		Keys []struct{ Kty, Alg, Kid, N, E string } `json:"keys"`
	}
	getJSON(t, ctx, disc.JWKSURI, &set)

	parts := strings.Split(tok.Token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", tok.Token)
	}
	dec := func(s string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("base64url: %v", err)
		}
		return b
	}
	var head struct{ Alg, Kid string }
	json.Unmarshal(dec(parts[0]), &head)
	if head.Alg != "RS256" {
		t.Fatalf("alg %q", head.Alg)
	}
	var pub *rsa.PublicKey
	for _, k := range set.Keys {
		if k.Kid == head.Kid && k.Kty == "RSA" {
			pub = &rsa.PublicKey{N: new(big.Int).SetBytes(dec(k.N)), E: int(new(big.Int).SetBytes(dec(k.E)).Int64())}
		}
	}
	if pub == nil {
		t.Fatalf("kid %q is not published at %s", head.Kid, disc.JWKSURI)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], dec(parts[2])); err != nil {
		t.Fatalf("signature: %v", err)
	}

	var cl struct {
		Iss, Sub, Aud, Jti, Project, Sandbox, Template string
		SandboxName                                    string `json:"sandbox_name"`
		Iat, Nbf, Exp                                  int64
		Labels                                         map[string]string
	}
	if err := json.Unmarshal(dec(parts[1]), &cl); err != nil {
		t.Fatal(err)
	}
	switch {
	case cl.Iss != disc.Issuer:
		t.Fatalf("iss %q, discovery says %q", cl.Iss, disc.Issuer)
	case cl.Sub != "project:"+who.Project.Name+":sandbox:"+sb.ID:
		t.Fatalf("sub %q", cl.Sub)
	case cl.Aud != "test" || cl.Sandbox != sb.ID || cl.SandboxName != sb.Name || cl.Project != who.Project.Name ||
		cl.Template != sb.Template || cl.Labels["case"] != "identity" || cl.Jti == "":
		t.Fatalf("claims %+v", cl)
	case cl.Exp != tok.ExpiresAt.Unix() || cl.Exp-cl.Iat != 600 || cl.Nbf > time.Now().Add(time.Minute).Unix():
		t.Fatalf("times: iat %d nbf %d exp %d", cl.Iat, cl.Nbf, cl.Exp)
	}

	// Without an audience the API refuses.
	if code := shCode(t, ctx, c, sb.Name, "", `curl -sf --unix-socket /run/loam/api.sock http://loam/identity/token`); code == 0 {
		t.Fatal("a token without an audience")
	}
}

// getJSON fetches a public document, without the suite's key.
func getJSON(t *testing.T, ctx context.Context, u string, out any) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d (an issuer needs --identity-keys)", u, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(fmt.Errorf("GET %s: %w", u, err))
	}
}
