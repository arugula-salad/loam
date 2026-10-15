package agent

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/arugula-salad/loam/pkg/api"
)

func (s *Server) registerServices(mux *http.ServeMux) {
	mux.HandleFunc("GET /services", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, api.ServiceList{Services: s.services.list()})
	})
	mux.HandleFunc("GET /services/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.serviceAnswer(w, http.StatusOK)(s.services.get(r.PathValue("name")))
	})
	mux.HandleFunc("PUT /services/{name}", func(w http.ResponseWriter, r *http.Request) {
		var spec api.ServiceSpec
		if !decodeBody(w, r, 1<<20, &spec) {
			return
		}
		s.serviceAnswer(w, http.StatusOK)(s.services.define(r.PathValue("name"), spec))
	})
	mux.HandleFunc("DELETE /services/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.services.remove(r.PathValue("name")); err != nil {
			serviceErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /services/{name}/start", func(w http.ResponseWriter, r *http.Request) {
		s.serviceAnswer(w, http.StatusOK)(s.services.start(r.PathValue("name")))
	})
	mux.HandleFunc("POST /services/{name}/stop", func(w http.ResponseWriter, r *http.Request) {
		s.serviceAnswer(w, http.StatusOK)(s.services.stop(r.PathValue("name")))
	})
	mux.HandleFunc("POST /services/{name}/restart", func(w http.ResponseWriter, r *http.Request) {
		s.serviceAnswer(w, http.StatusOK)(s.services.restart(r.PathValue("name")))
	})
	mux.HandleFunc("GET /services/{name}/logs", s.serviceLogs)
}

func (s *Server) serviceAnswer(w http.ResponseWriter, status int) func(api.Service, error) {
	return func(svc api.Service, err error) {
		if err != nil {
			serviceErr(w, err)
			return
		}
		writeJSON(w, status, svc)
	}
}

func serviceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errServiceNotFound):
		writeErr(w, http.StatusNotFound, api.CodeNotFound, err.Error())
	case errors.Is(err, errServiceConflict):
		writeErr(w, http.StatusConflict, api.CodeConflict, strings.TrimPrefix(err.Error(), errServiceConflict.Error()+": "))
	case errors.Is(err, errUnsupportedUser):
		writeErr(w, http.StatusNotImplemented, api.CodeUnsupported, err.Error())
	default:
		writeErr(w, http.StatusBadRequest, api.CodeInvalid, err.Error())
	}
}

// serviceLogs is the live log as text/plain, stdout and stderr interleaved:
// the last ?tail=N lines (all of it by default), then, with ?follow=true,
// what the service prints next until the caller goes or the service is deleted.
func (s *Server) serviceLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	follow, err := parseBool(q.Get("follow"), false)
	if err != nil {
		writeErr(w, http.StatusBadRequest, api.CodeInvalid, "follow must be true or false")
		return
	}
	tail := -1
	if v := q.Get("tail"); v != "" {
		if tail, err = strconv.Atoi(v); err != nil || tail < 0 {
			writeErr(w, http.StatusBadRequest, api.CodeInvalid, "tail must be a non-negative integer")
			return
		}
	}
	lines, ch, cancel, err := s.services.follow(r.PathValue("name"), tail, follow)
	if err != nil {
		serviceErr(w, err)
		return
	}
	defer cancel()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	for _, l := range lines {
		io.WriteString(w, l)
	}
	if ch == nil {
		return
	}
	fl, _ := w.(http.Flusher)
	if fl != nil {
		fl.Flush()
	}
	for {
		select {
		case l, ok := <-ch:
			if !ok {
				return
			}
			io.WriteString(w, l)
			if fl != nil {
				fl.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}
