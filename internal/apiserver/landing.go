package apiserver

import (
	_ "embed"
	"net/http"
	"strings"
)

// landingPage is what a browser sees at the API's root: what loam is and
// where to get it. Only a browser: a client that doesn't ask for HTML gets the
// 404 the root has always answered, so nothing that probes or scripts against
// the API sees a change.
//
//go:embed site/index.html
var landingPage []byte

func (s *Server) landing(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Accept")
	if !acceptsHTML(r) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(landingPage)
}

// acceptsHTML is whether the request's Accept names text/html, as a browser's
// navigation does. A missing Accept, */* alone (curl's), or application/json
// is not a browser.
func acceptsHTML(r *http.Request) bool {
	for _, v := range r.Header.Values("Accept") {
		for _, part := range strings.Split(v, ",") {
			mt, _, _ := strings.Cut(part, ";")
			if strings.EqualFold(strings.TrimSpace(mt), "text/html") {
				return true
			}
		}
	}
	return false
}
