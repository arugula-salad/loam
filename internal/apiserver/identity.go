package apiserver

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/pkg/api"
)

// Sandbox identity (docs/API.md, "Identity"): loam-api is an OpenID Connect
// issuer, and a sandbox asks it for a token over the host channel. The host
// channel already makes the VM the identity (guest.go), so a sandbox can only
// ever get a token for itself; nothing in the request says who it is.
//
// The tokens are plain RS256 JWTs, which the standard library signs well
// enough that a JWT dependency would only add surface.

const (
	identityTTLDefault = 600 * time.Second
	identityTTLMax     = 3600 * time.Second
	// minIdentityKeyBits refuses keys verifiers such as AWS STS refuse too.
	minIdentityKeyBits = 2048
)

// IdentityKey is one signing key and its key ID.
type IdentityKey struct {
	// KID is the RFC 7638 thumbprint of the public key, so the same key
	// always has the same ID, whichever file or order it comes from.
	KID string
	Key *rsa.PrivateKey
}

// LoadIdentityKeys reads RSA private keys (PKCS #1 or PKCS #8 PEM) from
// paths, each a file or a directory whose regular files are read in name
// order. The first key signs; all of them are published, so a key being
// rotated out goes after the new one and stays while tokens it signed can
// still be live (an hour). A file that holds anything but RSA keys of at
// least 2048 bits is an error, not skipped: a key the operator meant to
// publish and that silently isn't would break verifiers an hour later.
func LoadIdentityKeys(paths []string) ([]IdentityKey, error) {
	var files []string
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() {
			files = append(files, p)
			continue
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return nil, err
		}
		var names []string
		for _, e := range entries {
			// Kubernetes mounts a Secret's keys as symlinks into a
			// ..data directory, which itself starts with a dot.
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			full := filepath.Join(p, e.Name())
			if st, err := os.Stat(full); err == nil && st.Mode().IsRegular() {
				names = append(names, full)
			}
		}
		sort.Strings(names)
		files = append(files, names...)
	}
	var keys []IdentityKey
	seen := map[string]bool{}
	for _, f := range files {
		ks, err := parseIdentityKeys(f)
		if err != nil {
			return nil, err
		}
		for _, k := range ks {
			if !seen[k.KID] {
				seen[k.KID] = true
				keys = append(keys, k)
			}
		}
	}
	if len(paths) > 0 && len(keys) == 0 {
		return nil, fmt.Errorf("no identity keys in %s", strings.Join(paths, ", "))
	}
	return keys, nil
}

func parseIdentityKeys(file string) ([]IdentityKey, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var keys []IdentityKey
	for {
		var b *pem.Block
		b, raw = pem.Decode(raw)
		if b == nil {
			break
		}
		var k any
		switch b.Type {
		case "RSA PRIVATE KEY":
			k, err = x509.ParsePKCS1PrivateKey(b.Bytes)
		case "PRIVATE KEY":
			k, err = x509.ParsePKCS8PrivateKey(b.Bytes)
		default:
			return nil, fmt.Errorf("%s: a %q block; identity keys are RSA private keys", file, b.Type)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("%s: a %T; identity keys are RSA (RS256)", file, k)
		}
		if bits := rk.N.BitLen(); bits < minIdentityKeyBits {
			return nil, fmt.Errorf("%s: an RSA key of %d bits; at least %d are needed", file, bits, minIdentityKeyBits)
		}
		keys = append(keys, IdentityKey{KID: jwkThumbprint(&rk.PublicKey), Key: rk})
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s: no PEM keys", file)
	}
	return keys, nil
}

var b64 = base64.RawURLEncoding

// jwk is an RSA public key as a JWK (RFC 7517).
type jwk struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	KID string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func rsaE(pub *rsa.PublicKey) string { return b64.EncodeToString(big.NewInt(int64(pub.E)).Bytes()) }

// jwkThumbprint is RFC 7638's: the SHA-256 of the required members in
// lexical order with no whitespace. Base64url values never need escaping.
func jwkThumbprint(pub *rsa.PublicKey) string {
	sum := sha256.Sum256([]byte(`{"e":"` + rsaE(pub) + `","kty":"RSA","n":"` + b64.EncodeToString(pub.N.Bytes()) + `"}`))
	return b64.EncodeToString(sum[:])
}

// issuer is the API's public base URL without a trailing slash: verifiers
// compare iss with the discovery document's issuer byte for byte.
func (s *Server) issuer() string { return strings.TrimRight(s.opts.PublicURL, "/") }

func (s *Server) noIdentity() *apiErr {
	return errf(http.StatusNotImplemented, api.CodeUnsupported, "this deployment has no identity signing key")
}

// openIDConfiguration is GET /.well-known/openid-configuration, without a key.
func (s *Server) openIDConfiguration(w http.ResponseWriter, r *http.Request) {
	if len(s.opts.IdentityKeys) == 0 {
		s.writeErr(w, s.noIdentity())
		return
	}
	iss := s.issuer()
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                iss,
		"jwks_uri":                              iss + "/.well-known/jwks.json",
		"response_types_supported":              []string{"id_token"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"claims_supported": []string{"iss", "sub", "aud", "iat", "nbf", "exp", "jti",
			"project", "sandbox", "sandbox_name", "template", "labels"},
	})
}

// jwks is GET /.well-known/jwks.json, without a key: every loaded key, the
// signing one first.
func (s *Server) jwks(w http.ResponseWriter, r *http.Request) {
	if len(s.opts.IdentityKeys) == 0 {
		s.writeErr(w, s.noIdentity())
		return
	}
	keys := make([]jwk, 0, len(s.opts.IdentityKeys))
	for _, k := range s.opts.IdentityKeys {
		pub := &k.Key.PublicKey
		keys = append(keys, jwk{Kty: "RSA", Use: "sig", Alg: "RS256", KID: k.KID,
			N: b64.EncodeToString(pub.N.Bytes()), E: rsaE(pub)})
	}
	// Verifiers cache the set; five minutes bounds how long one goes without
	// a key just added.
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

// identityClaims is the token's payload (docs/API.md, "Identity").
type identityClaims struct {
	Iss         string            `json:"iss"`
	Sub         string            `json:"sub"`
	Aud         string            `json:"aud"`
	Iat         int64             `json:"iat"`
	Nbf         int64             `json:"nbf"`
	Exp         int64             `json:"exp"`
	Jti         string            `json:"jti"`
	Project     string            `json:"project"`
	Sandbox     string            `json:"sandbox"`
	SandboxName string            `json:"sandbox_name"`
	Template    string            `json:"template"`
	Labels      map[string]string `json:"labels"`
}

// guestIdentityToken is GET /identity/token from inside the sandbox.
func (s *Server) guestIdentityToken(w http.ResponseWriter, r *http.Request, sb db.Sandbox) {
	if len(s.opts.IdentityKeys) == 0 {
		s.writeErr(w, s.noIdentity())
		return
	}
	q := r.URL.Query()
	aud := q.Get("audience")
	if aud == "" {
		s.writeErr(w, invalid("audience is required"))
		return
	}
	ttl := identityTTLDefault
	if v := q.Get("ttl_s"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || time.Duration(n)*time.Second > identityTTLMax {
			s.writeErr(w, invalid("ttl_s must be a number of seconds from 1 to %d", int(identityTTLMax/time.Second)))
			return
		}
		ttl = time.Duration(n) * time.Second
	}
	now := s.now().Truncate(time.Second)
	exp := now.Add(ttl)
	var jti [16]byte
	rand.Read(jti[:])
	labels := sb.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	tok, err := signRS256(s.opts.IdentityKeys[0], identityClaims{
		Iss: s.issuer(), Sub: "project:" + sb.Project + ":sandbox:" + sb.ID, Aud: aud,
		Iat: now.Unix(), Nbf: now.Unix(), Exp: exp.Unix(), Jti: hex.EncodeToString(jti[:]),
		Project: sb.Project, Sandbox: sb.ID, SandboxName: sb.Name, Template: sb.Template, Labels: labels,
	})
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.IdentityToken{Token: tok, ExpiresAt: exp})
}

// signRS256 makes a compact JWS (RFC 7515) of claims.
func signRS256(k IdentityKey, claims any) (string, error) {
	head, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": k.KID})
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := b64.EncodeToString(head) + "." + b64.EncodeToString(body)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(nil, k.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", errors.New("sign the identity token: " + err.Error())
	}
	return input + "." + b64.EncodeToString(sig), nil
}
