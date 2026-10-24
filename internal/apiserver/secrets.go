package apiserver

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/netpolicy"
	"github.com/arugula-salad/loam/pkg/api"
)

// Project secrets (docs/API.md, "Secrets"). A value is sealed with AES-256-GCM
// under loam-api's secret key before it reaches the store, and opened only to
// build a policy for a nodelet (network.go). It is never logged, never
// answered, and never part of a wake: the sandbox gets the placeholder.

// SecretKeySize is the length of loam-api's secret key: AES-256.
const SecretKeySize = 32

// ParseSecretKey reads the secret key as loam-api's environment carries it:
// 32 bytes, base64.
func ParseSecretKey(s string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("the secret key is not base64: %w", err)
	}
	if len(key) != SecretKeySize {
		return nil, fmt.Errorf("the secret key is %d bytes, not %d", len(key), SecretKeySize)
	}
	return key, nil
}

// sealer seals and opens secret values.
type sealer struct {
	aead       cipher.AEAD
	trafficKey []byte
}

func newSealer(key []byte) (*sealer, error) {
	if len(key) != SecretKeySize {
		return nil, fmt.Errorf("the secret key is %d bytes, not %d", len(key), SecretKeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	trafficKey, err := hkdf.Key(sha256.New, key, nil, "loam e2b traffic token", 32)
	if err != nil {
		return nil, err
	}
	return &sealer{aead: aead, trafficKey: trafficKey}, nil
}

// secretAD binds a sealed value to its row: one copied to another project or
// name doesn't open. Neither a project nor a secret name holds a NUL.
func secretAD(project, name string) []byte {
	return []byte("loam secret\x00" + project + "\x00" + name)
}

// seal is a random nonce and the sealed value after it. A random 96-bit
// nonce is safe for far more seals than a project's secrets will ever see.
func (s *sealer) seal(project, name string, value []byte) []byte {
	return s.sealWith(secretAD(project, name), value)
}

func (s *sealer) open(project, name string, sealed []byte) ([]byte, error) {
	return s.openWith(secretAD(project, name), sealed)
}

// sealWith and openWith are seal and open with the additional data given,
// which binds the sealed value to where it is kept.
func (s *sealer) sealWith(ad, value []byte) []byte {
	nonce := make([]byte, s.aead.NonceSize(), s.aead.NonceSize()+len(value)+s.aead.Overhead())
	rand.Read(nonce)
	return s.aead.Seal(nonce, nonce, value, ad)
}

func (s *sealer) openWith(ad, sealed []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("sealed value too short")
	}
	return s.aead.Open(nil, sealed[:n], sealed[n:], ad)
}

func unsupported(format string, args ...any) *apiErr {
	return errf(http.StatusNotImplemented, api.CodeUnsupported, format, args...)
}

func noSecretKey() *apiErr {
	return unsupported("this deployment has no secret key, so it can't keep secrets")
}

const (
	maxSecretValue = 8 << 10
	maxSecretName  = 64
)

var (
	secretNameRE = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	// placeholderPrefixRE is printable ASCII without a space: a placeholder is
	// in the sandbox's env and, byte for byte, in a request head.
	placeholderPrefixRE = regexp.MustCompile(`^[!-~]{0,64}$`)
	// headerTokenRE is an HTTP field name (RFC 9110, "token").
	headerTokenRE = regexp.MustCompile("^[!#$%&'*+\\-.^_`|~0-9A-Za-z]+$")
)

// frameHeaders are headers a secret can't set: they say how the request is
// framed or where it goes, which the broker decides, not the secret.
var frameHeaders = []string{"host", "content-length", "transfer-encoding", "connection", "upgrade", "te",
	"trailer", "keep-alive", "proxy-connection"}

func validSecretName(name string) bool {
	return len(name) <= maxSecretName && secretNameRE.MatchString(name)
}

// placeholder is what stands for a secret in its sandboxes' env.
func placeholder(prefix, name string) string {
	return prefix + "__loam_" + strings.ToLower(name) + "__"
}

func checkPutSecret(name string, req api.PutSecret) error {
	if !validSecretName(name) {
		return invalid("a secret's name is an environment variable name, [A-Z_][A-Z0-9_]*, at most %d characters", maxSecretName)
	}
	switch {
	case req.Value == "":
		// An empty value would be found in every answer the broker scans.
		return invalid("value is required")
	case len(req.Value) > maxSecretValue:
		return invalid("value is at most %d bytes", maxSecretValue)
	case strings.ContainsAny(req.Value, "\r\n\x00"):
		return invalid("value may not hold CR, LF or NUL")
	}
	if len(req.Hosts) == 0 {
		return invalid("hosts is required: the value goes nowhere else")
	}
	for _, h := range req.Hosts {
		if err := netpolicy.ValidHost(h); err != nil {
			return invalid("hosts: %v", err)
		}
	}
	if !placeholderPrefixRE.MatchString(req.PlaceholderPrefix) {
		return invalid("placeholder_prefix is at most 64 printable ASCII characters, without spaces")
	}
	if in := req.Inject; in != nil {
		if !headerTokenRE.MatchString(in.Header) {
			return invalid("inject.header %q is not a header name", in.Header)
		}
		if slices.Contains(frameHeaders, strings.ToLower(in.Header)) {
			return invalid("inject.header %q frames or routes the request, which a secret can't set", in.Header)
		}
		if strings.Count(in.Format, "{value}") != 1 {
			return invalid(`inject.format holds "{value}" exactly once`)
		}
		if strings.ContainsAny(in.Format, "\r\n\x00") {
			return invalid("inject.format may not hold CR, LF or NUL")
		}
	}
	return nil
}

func secretView(s db.Secret) api.Secret {
	s = s.Clone()
	return api.Secret{Name: s.Name, Hosts: s.Hosts, Placeholder: s.Placeholder, Inject: s.Inject,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}
}

// keepsSecrets guards the secret routes: without a key, they are unsupported.
func (s *Server) keepsSecrets(h func(http.ResponseWriter, *http.Request, caller)) func(http.ResponseWriter, *http.Request, caller) {
	return func(w http.ResponseWriter, r *http.Request, c caller) {
		if s.sealer == nil {
			s.writeErr(w, noSecretKey())
			return
		}
		h(w, r, c)
	}
}

// putSecret creates or replaces a secret, then hands the sandboxes that name
// it and are awake their new policy before answering, so that a client that
// replaced a leaked value knows when it is no longer used.
func (s *Server) putSecret(w http.ResponseWriter, r *http.Request, c caller) {
	name := r.PathValue("name")
	var req api.PutSecret
	if err := decode(r, &req); err != nil {
		s.writeErr(w, err)
		return
	}
	if err := checkPutSecret(name, req); err != nil {
		s.writeErr(w, err)
		return
	}
	now := s.now()
	rec, err := s.store.PutSecret(r.Context(), db.Secret{Project: c.project.Name, Name: name, Hosts: req.Hosts,
		Placeholder: placeholder(req.PlaceholderPrefix, name), Inject: req.Inject,
		Value: s.sealer.seal(c.project.Name, name, []byte(req.Value)), CreatedAt: now, UpdatedAt: now})
	if err != nil {
		s.writeErr(w, err)
		return
	}
	s.log.Info("secret put", "project", c.project.Name, "secret", name)
	s.pushSecret(r.Context(), c.project.Name, name)
	writeJSON(w, http.StatusOK, secretView(rec))
}

// pushSecret hands every awake sandbox of project that names secret its
// policy anew. One that can't be reached keeps the old value until its next
// wake; that is logged, not answered, since the secret itself was stored.
func (s *Server) pushSecret(ctx context.Context, project, secret string) {
	all, err := s.store.Sandboxes(ctx, project)
	if err != nil {
		s.log.Warn("could not list the sandboxes a secret reaches", "project", project, "secret", secret, "err", err)
		return
	}
	for _, sb := range all {
		if sb.State != api.StateRunning || !slices.Contains(sb.Secrets, secret) {
			continue
		}
		rt := s.rt(sb.ID)
		rt.mu.Lock()
		cur, err := s.store.Sandbox(ctx, sb.ID)
		if err == nil {
			err = s.pushPolicyLocked(ctx, cur)
		}
		rt.mu.Unlock()
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			s.log.Warn("could not hand an awake sandbox a replaced secret; it gets it at its next wake", "sandbox", sb.ID, "secret", secret, "err", err)
		}
	}
}

func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request, c caller) {
	l, err := s.store.Secrets(r.Context(), c.project.Name)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := api.SecretList{Secrets: []api.Secret{}}
	for _, sec := range l {
		out.Secrets = append(out.Secrets, secretView(sec))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getSecret(w http.ResponseWriter, r *http.Request, c caller) {
	sec, err := s.store.Secret(r.Context(), c.project.Name, r.PathValue("name"))
	if errors.Is(err, db.ErrNotFound) {
		err = notFound("secret")
	}
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, secretView(sec))
}

func (s *Server) deleteSecret(w http.ResponseWriter, r *http.Request, c caller) {
	switch err := s.store.DeleteSecret(r.Context(), c.project.Name, r.PathValue("name")); {
	case errors.Is(err, db.ErrNotFound):
		s.writeErr(w, notFound("secret"))
	case errors.Is(err, db.ErrInUse):
		s.writeErr(w, conflict("a sandbox names the secret; take it out of the sandbox's secrets first"))
	case err != nil:
		s.writeErr(w, err)
	default:
		s.log.Info("secret deleted", "project", c.project.Name, "secret", r.PathValue("name"))
		w.WriteHeader(http.StatusNoContent)
	}
}
