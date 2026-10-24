package e2b

import (
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strings"
)

var browserAgent = regexp.MustCompile(`(?i)mozilla|chrome|safari|firefox|edge|opera|msie`)

// trafficHTML follows E2B's proxy negotiation: script requests get JSON even
// when their user agent is a browser's. Navigation falls back to the user agent.
func trafficHTML(r *http.Request) bool {
	accept := strings.ToLower(r.Header.Get("Accept"))
	if strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/html") {
		return false
	}
	switch strings.ToLower(r.Header.Get("Sec-Fetch-Mode")) {
	case "cors", "no-cors", "same-origin", "websocket":
		return false
	case "navigate":
	default:
		if r.Header.Get("X-Requested-With") != "" {
			return false
		}
	}
	return browserAgent.MatchString(r.UserAgent())
}

func trafficDenied(w http.ResponseWriter, r *http.Request, id string, missing bool) {
	message := "Sandbox is secured with traffic access token. Provided token in header 'e2b-traffic-access-token' is invalid"
	if missing {
		message = "Sandbox is secured with traffic access token. Token header 'e2b-traffic-access-token' is missing"
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	if trafficHTML(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, "<!doctype html><html lang=\"en\"><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width\"><title>Private sandbox · loam</title><body><main><h1>Private sandbox</h1><p>%s</p><p>Sandbox: <code>%s</code></p></main></body></html>", html.EscapeString(message), html.EscapeString(id))
		return
	}
	writeJSON(w, http.StatusForbidden, map[string]any{"sandboxId": id, "message": message, "code": http.StatusForbidden})
}
