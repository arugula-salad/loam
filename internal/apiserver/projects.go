package apiserver

import (
	"errors"
	"net/http"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/pkg/api"
)

func keyView(k db.Key) api.Key {
	return api.Key{ID: k.ID, Name: k.Name, Project: k.Project, Scope: k.Scope, CreatedAt: k.CreatedAt}
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(r, &req); err != nil {
		s.writeErr(w, err)
		return
	}
	if !validName(req.Name) {
		s.writeErr(w, invalid("a project name is a DNS label of at most 32 characters without \"--\""))
		return
	}
	p := api.Project{Name: req.Name, CreatedAt: s.now()}
	if err := s.store.CreateProject(r.Context(), p); err != nil {
		if errors.Is(err, db.ErrExists) {
			err = errf(http.StatusConflict, api.CodeAlreadyExists, "project %q already exists", req.Name)
		}
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.projectView(p))
}

// projectView is a project as the API shows it: with its URL domains, which
// are the operator's file's, not records.
func (s *Server) projectView(p api.Project) api.Project {
	p.Domains = s.opts.URLDomains.of(p.Name)
	return p
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	ps, err := s.store.Projects(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if ps == nil {
		ps = []api.Project{}
	}
	for i := range ps {
		ps[i] = s.projectView(ps[i])
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": ps})
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	switch err := s.store.DeleteProject(r.Context(), r.PathValue("project")); {
	case errors.Is(err, db.ErrNotEmpty):
		s.writeErr(w, errf(http.StatusConflict, api.CodeProjectNotEmpty, "the project still has sandboxes"))
	case errors.Is(err, db.ErrNotFound):
		s.writeErr(w, notFound("project"))
	case err != nil:
		s.writeErr(w, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
	}
	if err := decode(r, &req); err != nil {
		s.writeErr(w, err)
		return
	}
	if req.Name == "" || len(req.Name) > 64 {
		s.writeErr(w, invalid("a key needs a name of 1 to 64 characters"))
		return
	}
	if req.Scope == "" {
		req.Scope = api.ScopeFull
	}
	if req.Scope != api.ScopeFull && req.Scope != api.ScopeRead {
		s.writeErr(w, invalid("a key's scope is full or read"))
		return
	}
	project := r.PathValue("project")
	secret := newKeySecret()
	k := db.Key{ID: newKeyID(), Project: project, Name: req.Name, Hash: hashKey(secret), CreatedAt: s.now(), Scope: req.Scope}
	if err := s.store.CreateKey(r.Context(), k); err != nil {
		switch {
		case errors.Is(err, db.ErrNotFound):
			err = notFound("project")
		case errors.Is(err, db.ErrExists):
			err = errf(http.StatusConflict, api.CodeAlreadyExists, "the project already has a key named %q", req.Name)
		}
		s.writeErr(w, err)
		return
	}
	v := keyView(k)
	v.Secret = secret
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if _, err := s.store.Project(r.Context(), project); err != nil {
		s.writeErr(w, notFound("project"))
		return
	}
	ks, err := s.store.Keys(r.Context(), project)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := []api.Key{}
	for _, k := range ks {
		out = append(out, keyView(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteKey(r.Context(), r.PathValue("project"), r.PathValue("key")); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			err = notFound("key")
		}
		s.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) whoami(w http.ResponseWriter, r *http.Request, c caller) {
	writeJSON(w, http.StatusOK, api.Whoami{Project: s.projectView(c.project), Key: keyView(c.key)})
}
