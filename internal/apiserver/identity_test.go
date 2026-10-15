package apiserver

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

const testIssuer = "https://api.loam.test"

// Generating RSA keys is slow enough to share them across the tests.
var (
	testKeysOnce sync.Once
	testKeys     [2]*rsa.PrivateKey
)

func identityTestKeys(t *testing.T) [2]*rsa.PrivateKey {
	testKeysOnce.Do(func() {
		for i := range testKeys {
			k, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				panic(err)
			}
			testKeys[i] = k
		}
	})
	return testKeys
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// RFC 7638, section 3.1: the thumbprint of the RFC 7517 example key.
func TestJWKThumbprintVector(t *testing.T) {
	n, err := base64.RawURLEncoding.DecodeString("0vx7agoebGcQSuuPiLJXZptN9nndrQmbXEps2aiAFbWhM78LhWx4cbbfAAtVT86zwu1RK7aPFFxuhDR1L6tSoc_BJECPebWKRXjBZCiFV4n3oknjhMstn64tZ_2W-5JsGY4Hc5n9yBXArwl93lqt7_RN5w6Cf0h4QyQ5v-65YGjQR0_FDW2QvzqY368QQMicAtaSqzs8KJZgnYb9c7d0zgdAZHzu6qMQvRL5hajrn1n91CbOpbISD08qNLyrdkt-bFTWhAI4vMQFh6WeZu0fM4lFd2NcRwr3XPksINHaQ-G_xBniIqbw0Ls1jF44-csFCur-kEgU8awapJzKnqDKgw")
	if err != nil {
		t.Fatal(err)
	}
	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: 65537}
	if got, want := jwkThumbprint(pub), "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs"; got != want {
		t.Fatalf("thumbprint %s, want %s", got, want)
	}
}

func TestLoadIdentityKeys(t *testing.T) {
	keys := identityTestKeys(t)
	dir := t.TempDir()
	// A directory reads in name order, skipping dot files (a Secret's ..data).
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(keys[1])
	writePEM(t, filepath.Join(dir, "b.pem"), "PRIVATE KEY", pkcs8)
	writePEM(t, filepath.Join(dir, "a.pem"), "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(keys[0]))
	os.WriteFile(filepath.Join(dir, ".hidden"), []byte("not a key"), 0o600)
	got, err := LoadIdentityKeys([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Key.N.Cmp(keys[0].N) != 0 || got[1].Key.N.Cmp(keys[1].N) != 0 {
		t.Fatalf("keys out of order: %+v", got)
	}
	// The same key twice is published once; a file before a directory signs.
	got, err = LoadIdentityKeys([]string{filepath.Join(dir, "b.pem"), dir})
	if err != nil || len(got) != 2 || got[0].Key.N.Cmp(keys[1].N) != 0 {
		t.Fatalf("file then directory: %+v %v", got, err)
	}
	if none, err := LoadIdentityKeys(nil); err != nil || len(none) != 0 {
		t.Fatalf("no paths: %v %v", none, err)
	}

	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, _ := x509.MarshalPKCS8PrivateKey(ec)
	pubDER, _ := x509.MarshalPKIXPublicKey(&keys[0].PublicKey)
	bad := t.TempDir()
	for name, b := range map[string]struct {
		typ string
		der []byte
	}{
		"small.pem":  {"RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(small)},
		"ec.pem":     {"PRIVATE KEY", ecDER},
		"public.pem": {"PUBLIC KEY", pubDER},
	} {
		p := filepath.Join(bad, name)
		writePEM(t, p, b.typ, b.der)
		if _, err := LoadIdentityKeys([]string{p}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	empty := filepath.Join(bad, "empty.pem")
	os.WriteFile(empty, []byte("nothing here\n"), 0o600)
	if _, err := LoadIdentityKeys([]string{empty}); err == nil {
		t.Error("a file without keys was accepted")
	}
	if _, err := LoadIdentityKeys([]string{t.TempDir()}); err == nil {
		t.Error("an empty directory was accepted")
	}
}

func identityHarness(t *testing.T, signing ...*rsa.PrivateKey) *harness {
	return newClusterWith(t, func(o *Options) {
		o.PublicURL = testIssuer + "/"
		for _, k := range signing {
			o.IdentityKeys = append(o.IdentityKeys, IdentityKey{KID: jwkThumbprint(&k.PublicKey), Key: k})
		}
	}, nodeSpec{"geek", 8192})
}

// guestGet is a GET on the host channel as the nodelet sends it.
func (h *harness) guestGet(path, token, sandbox string) (int, []byte) {
	h.t.Helper()
	req, _ := http.NewRequest("GET", h.ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(nodeapi.HeaderSandbox, sandbox)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// verifyJWT checks a token the way a relying party does, from the published
// documents alone: discovery, then the JWKS it names, then the key the
// token's kid picks. It shares no code with the signer.
func verifyJWT(t *testing.T, h *harness, token string) (header map[string]any, claims map[string]any) {
	t.Helper()
	var disc struct {
		Issuer  string   `json:"issuer"`
		JWKSURI string   `json:"jwks_uri"`
		Algs    []string `json:"id_token_signing_alg_values_supported"`
	}
	h.must(200, "GET", "/.well-known/openid-configuration", "", nil, &disc)
	if disc.Issuer != testIssuer || disc.JWKSURI != testIssuer+"/.well-known/jwks.json" || len(disc.Algs) != 1 || disc.Algs[0] != "RS256" {
		t.Fatalf("discovery: %+v", disc)
	}
	var set struct {
		Keys []struct {
			Kty, Use, Alg, Kid, N, E string
		} `json:"keys"`
	}
	h.must(200, "GET", strings.TrimPrefix(disc.JWKSURI, testIssuer), "", nil, &set)

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a compact JWS: %q", token)
	}
	dec := func(s string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("base64url %q: %v", s, err)
		}
		return b
	}
	if err := json.Unmarshal(dec(parts[0]), &header); err != nil {
		t.Fatal(err)
	}
	if header["alg"] != "RS256" {
		t.Fatalf("header: %v", header)
	}
	var pub *rsa.PublicKey
	for _, k := range set.Keys {
		if k.Kid == header["kid"] {
			if k.Kty != "RSA" || k.Use != "sig" || k.Alg != "RS256" {
				t.Fatalf("jwk: %+v", k)
			}
			pub = &rsa.PublicKey{N: new(big.Int).SetBytes(dec(k.N)), E: int(new(big.Int).SetBytes(dec(k.E)).Int64())}
		}
	}
	if pub == nil {
		t.Fatalf("kid %v is not in the JWKS %+v", header["kid"], set)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], dec(parts[2])); err != nil {
		t.Fatalf("signature: %v", err)
	}
	if err := json.Unmarshal(dec(parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	return header, claims
}

func TestIdentityToken(t *testing.T) {
	keys := identityTestKeys(t)
	h := identityHarness(t, keys[0], keys[1])
	key := h.project("jake")
	sb := h.create(key, api.CreateSandbox{Name: "who", Labels: map[string]string{"team": "infra"}})

	// The JWKS publishes every key, the signing one first.
	var set struct {
		Keys []struct{ Kid string } `json:"keys"`
	}
	h.must(200, "GET", "/.well-known/jwks.json", "", nil, &set)
	if len(set.Keys) != 2 || set.Keys[0].Kid != jwkThumbprint(&keys[0].PublicKey) || set.Keys[1].Kid != jwkThumbprint(&keys[1].PublicKey) {
		t.Fatalf("jwks: %+v", set)
	}

	status, body := h.guestGet("/internal/guest/identity/token?audience=sts.amazonaws.com", nodeToken, sb.ID)
	if status != 200 {
		t.Fatalf("token: %d %s", status, body)
	}
	var tok api.IdentityToken
	if err := json.Unmarshal(body, &tok); err != nil {
		t.Fatal(err)
	}
	now := h.clock.Now()
	if !tok.ExpiresAt.Equal(now.Add(600 * time.Second)) {
		t.Fatalf("expires_at %v, want ten minutes after %v", tok.ExpiresAt, now)
	}
	header, claims := verifyJWT(t, h, tok.Token)
	if header["kid"] != jwkThumbprint(&keys[0].PublicKey) || header["typ"] != "JWT" {
		t.Fatalf("header: %v", header)
	}
	want := map[string]any{
		"iss": testIssuer, "sub": "project:jake:sandbox:" + sb.ID, "aud": "sts.amazonaws.com",
		"iat": float64(now.Unix()), "nbf": float64(now.Unix()), "exp": float64(now.Unix() + 600),
		"project": "jake", "sandbox": sb.ID, "sandbox_name": "who", "template": sb.Template,
	}
	for k, v := range want {
		if claims[k] != v {
			t.Errorf("claim %s = %v, want %v", k, claims[k], v)
		}
	}
	if l, _ := claims["labels"].(map[string]any); len(l) != 1 || l["team"] != "infra" {
		t.Errorf("labels: %v", claims["labels"])
	}
	if jti, _ := claims["jti"].(string); len(jti) < 16 {
		t.Errorf("jti: %v", claims["jti"])
	}
	_, body = h.guestGet("/internal/guest/identity/token?audience=sts.amazonaws.com", nodeToken, sb.ID)
	json.Unmarshal(body, &tok)
	if _, again := verifyJWT(t, h, tok.Token); again["jti"] == claims["jti"] {
		t.Error("two tokens with one jti")
	}

	// ttl_s: up to an hour, a whole number of seconds.
	_, body = h.guestGet("/internal/guest/identity/token?audience=a&ttl_s=3600", nodeToken, sb.ID)
	json.Unmarshal(body, &tok)
	if _, c := verifyJWT(t, h, tok.Token); c["exp"] != float64(now.Unix()+3600) || c["aud"] != "a" {
		t.Fatalf("an hour: %v", c)
	}
	for _, q := range []string{"", "?audience=", "?audience=a&ttl_s=3601", "?audience=a&ttl_s=0", "?audience=a&ttl_s=-5", "?audience=a&ttl_s=1.5", "?audience=a&ttl_s=x"} {
		status, body := h.guestGet("/internal/guest/identity/token"+q, nodeToken, sb.ID)
		var e api.Error
		json.Unmarshal(body, &e)
		if status != 400 || e.Error.Code != api.CodeInvalid {
			t.Errorf("%q: %d %s", q, status, body)
		}
	}

	// The host channel's rules: the node token, and a sandbox that exists.
	// Neither a project key nor the admin token gets a token, and the
	// public API has no such route.
	for _, tk := range []string{key, adminToken, ""} {
		if status, _ := h.guestGet("/internal/guest/identity/token?audience=a", tk, sb.ID); status != 401 {
			t.Errorf("token %q on the host channel: %d", tk, status)
		}
	}
	if status, _ := h.guestGet("/internal/guest/identity/token?audience=a", nodeToken, "sb-aaaaaaaaaaaa"); status != 404 {
		t.Errorf("unknown sandbox: %d", status)
	}
	h.errCode(404, api.CodeNotFound, "GET", "/v1/sandboxes/who/identity/token?audience=a", key, nil)
	h.must(204, "DELETE", "/v1/sandboxes/who", key, nil, nil)
	if status, _ := h.guestGet("/internal/guest/identity/token?audience=a", nodeToken, sb.ID); status != 404 {
		t.Errorf("deleted sandbox: %d", status)
	}
}

// Without a signing key the identity routes say so, rather than publishing
// an empty key set that would make every verifier fail obscurely.
func TestIdentityWithoutKey(t *testing.T) {
	h := identityHarness(t)
	key := h.project("jake")
	sb := h.create(key, api.CreateSandbox{Name: "who"})
	h.errCode(501, api.CodeUnsupported, "GET", "/.well-known/openid-configuration", "", nil)
	h.errCode(501, api.CodeUnsupported, "GET", "/.well-known/jwks.json", "", nil)
	status, body := h.guestGet("/internal/guest/identity/token?audience=a", nodeToken, sb.ID)
	var e api.Error
	json.Unmarshal(body, &e)
	if status != 501 || e.Error.Code != api.CodeUnsupported {
		t.Fatalf("token without a key: %d %s", status, body)
	}
}
