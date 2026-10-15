package apiserver

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/pkg/api"
)

// caller is who a request comes from: a project's key.
type caller struct {
	key     db.Key
	project api.Project
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func unauthorized() *apiErr {
	return errf(http.StatusUnauthorized, api.CodeUnauthorized, "a valid API key is required")
}

func tokenEqual(got, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// keyCaller resolves a project key. Only the key's SHA-256 is stored, so the
// lookup is by hash; a string that isn't shaped like a key isn't looked up.
func (s *Server) keyCaller(r *http.Request, secret string) (caller, error) {
	if !keyRE.MatchString(secret) {
		return caller{}, unauthorized()
	}
	k, err := s.store.KeyByHash(r.Context(), hashKey(secret))
	if errors.Is(err, db.ErrNotFound) {
		return caller{}, unauthorized()
	} else if err != nil {
		return caller{}, err
	}
	p, err := s.store.Project(r.Context(), k.Project)
	if err != nil {
		return caller{}, unauthorized()
	}
	return caller{key: k, project: p}, nil
}

// admin guards the project and key routes: the admin token and nothing else.
func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !tokenEqual(bearer(r), s.opts.AdminToken) {
			s.writeErr(w, errf(http.StatusUnauthorized, api.CodeUnauthorized, "the admin token is required"))
			return
		}
		h(w, r)
	}
}

// keyed guards everything else: a project's key, or a front end in this
// process acting for a project (frontend.go). The admin token is not a
// project and has no sandboxes.
func (s *Server) keyed(h func(http.ResponseWriter, *http.Request, caller)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, inProcess, err := s.frontendCaller(r)
		if !inProcess {
			c, err = s.keyCaller(r, bearer(r))
		}
		if err != nil {
			s.writeErr(w, err)
			return
		}
		if c.key.Scope == api.ScopeRead && ((r.Method != http.MethodGet && r.Method != http.MethodHead) || websocket.IsWebSocketUpgrade(r)) {
			s.writeErr(w, errf(http.StatusForbidden, api.CodePermissionDenied, "this key may only read"))
			return
		}
		h(w, r, c)
	}
}
