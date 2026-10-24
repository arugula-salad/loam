package e2b

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/arugula-salad/loam/internal/db"
)

func (f *Frontend) secretRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /secrets", f.auth(f.putSecret))
	mux.HandleFunc("POST /secrets/{secret}", f.auth(f.putSecret))
	mux.HandleFunc("GET /secrets", f.auth(f.listSecrets))
	mux.HandleFunc("GET /secrets/{secret}", f.auth(f.getSecret))
	mux.HandleFunc("DELETE /secrets/{secret}", f.auth(f.deleteSecret))
}
func secretInfo(s db.E2BSecret) map[string]any {
	meta := s.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	return map[string]any{"secretID": s.ID, "name": s.Name, "currentVersion": s.Version, "metadata": meta, "createdAt": s.CreatedAt, "updatedAt": s.UpdatedAt}
}
func (f *Frontend) findSecret(ctx context.Context, project, ref string) (db.E2BSecret, error) {
	all, err := f.store.E2BSecrets(ctx, project)
	if err != nil {
		return db.E2BSecret{}, err
	}
	for _, s := range all {
		if s.ID == ref || s.Name == ref {
			return s, nil
		}
	}
	return db.E2BSecret{}, db.ErrNotFound
}
func (f *Frontend) secretError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrNotFound):
		writeErr(w, 404, "secret not found")
	case errors.Is(err, db.ErrExists):
		writeErr(w, 409, "secret name already exists")
	default:
		f.fail(w, "", err)
	}
}
func (f *Frontend) putSecret(w http.ResponseWriter, r *http.Request, project string) {
	var req struct {
		Name     string            `json:"name"`
		Value    string            `json:"value"`
		Metadata map[string]string `json:"metadata"`
	}
	if !readBody(w, r, &req) {
		return
	}
	create := r.PathValue("secret") == ""
	if len(req.Value) == 0 || len(req.Value) > 8192 || strings.ContainsAny(req.Value, "\r\n\x00") {
		writeErr(w, 400, "secret value must be 1–8192 bytes without CR, LF, or NUL")
		return
	}
	if len(req.Metadata) > 64 {
		writeErr(w, 400, "at most 64 metadata entries")
		return
	}
	for k, v := range req.Metadata {
		if len(k) > 128 || len(v) > 1024 {
			writeErr(w, 400, "metadata entry is too long")
			return
		}
	}
	if !f.loam.CanSeal() {
		writeErr(w, 501, "this deployment has no secret key")
		return
	}
	now := f.now()
	rec := db.E2BSecret{Project: project, Name: req.Name, ID: "sec-" + newAccessToken(), CreatedAt: now, UpdatedAt: now, Metadata: req.Metadata}
	if create {
		if req.Name == "" || len(req.Name) > 256 || strings.ContainsAny(req.Name, "{}") || strings.ContainsFunc(req.Name, unicode.IsControl) {
			writeErr(w, 400, "invalid secret name")
			return
		}
	} else {
		old, err := f.findSecret(r.Context(), project, r.PathValue("secret"))
		if err != nil {
			f.secretError(w, err)
			return
		}
		if req.Name != "" && req.Name != old.Name {
			writeErr(w, 400, "secret names cannot be changed")
			return
		}
		rec.ID, rec.Name = old.ID, old.Name
	}
	sealed, err := f.loam.Seal("e2b-secret:"+project+":"+rec.ID, []byte(req.Value))
	if err != nil {
		f.secretError(w, err)
		return
	}
	rec.Sealed = sealed
	rec, err = f.store.PutE2BSecret(r.Context(), rec, create)
	if err != nil {
		f.secretError(w, err)
		return
	}
	if err = f.loam.RefreshE2BSecrets(r.Context(), project); err != nil {
		writeErr(w, 503, "secret stored, but an awake sandbox could not confirm the new policy; retry the update")
		return
	}
	status := 200
	if create {
		status = 201
	}
	writeJSON(w, status, secretInfo(rec))
}
func (f *Frontend) getSecret(w http.ResponseWriter, r *http.Request, project string) {
	rec, err := f.findSecret(r.Context(), project, r.PathValue("secret"))
	if err != nil {
		f.secretError(w, err)
		return
	}
	writeJSON(w, 200, secretInfo(rec))
}
func (f *Frontend) deleteSecret(w http.ResponseWriter, r *http.Request, project string) {
	rec, err := f.findSecret(r.Context(), project, r.PathValue("secret"))
	if err != nil {
		f.secretError(w, err)
		return
	}
	if err = f.store.DeleteE2BSecret(r.Context(), project, rec.ID); err != nil {
		f.secretError(w, err)
		return
	}
	if err = f.loam.RefreshE2BSecrets(r.Context(), project); err != nil {
		writeErr(w, 503, "secret deleted, but an awake sandbox could not confirm revocation")
		return
	}
	w.WriteHeader(204)
}
func (f *Frontend) listSecrets(w http.ResponseWriter, r *http.Request, project string) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeErr(w, 400, "limit must be 1–100")
			return
		}
		limit = n
	}
	all, err := f.store.E2BSecrets(r.Context(), project)
	if err != nil {
		f.secretError(w, err)
		return
	}
	out := []any{}
	cursor := r.URL.Query().Get("nextToken")
	for _, rec := range all {
		if rec.ID <= cursor {
			continue
		}
		if len(out) == limit {
			w.Header().Set("X-Next-Token", fmt.Sprint(out[len(out)-1].(map[string]any)["secretID"]))
			break
		}
		out = append(out, secretInfo(rec))
	}
	writeJSON(w, 200, out)
}
