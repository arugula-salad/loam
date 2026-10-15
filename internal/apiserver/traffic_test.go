package apiserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arugula-salad/loam/pkg/api"
)

const browserUA = "Mozilla/5.0 (X11; Linux x86_64) Firefox/140.0"

// visit is a request to a port URL as Traefik would hand it on: from the
// client at ip, with its User-Agent.
func (h *harness) visit(host, path, ip, ua, token string) int {
	h.t.Helper()
	req, _ := http.NewRequest("GET", h.ts.URL+path, nil)
	req.Host = host
	req.Header.Set("X-Forwarded-For", ip)
	req.Header.Set("User-Agent", ua)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// Traffic at a sandbox's URLs is counted per day, visitors among it, and
// served by GET .../stats and the metrics page.
func TestStats(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	sb := h.create(key, api.CreateSandbox{Name: "web"})
	h.node.Listen(sb.ID, 8080, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "hello")
	}))
	host := "web--jake." + testDomain

	// Private (the default): the owner's requests are the key's, and the
	// refused one is a 4xx; nobody is a visitor.
	if got := h.visit(host, "/", "203.0.113.1", browserUA, key); got != 200 {
		t.Fatalf("private with the key: %d", got)
	}
	if got := h.visit(host, "/", "203.0.113.2", browserUA, ""); got != 401 {
		t.Fatalf("private without the key: %d", got)
	}

	h.must(200, "PUT", "/v1/sandboxes/web/ports/8080", key, map[string]string{"access": "public"}, nil)
	h.visit(host, "/", "203.0.113.10", browserUA, "")
	h.visit(host, "/about", "203.0.113.10", browserUA, "")   // the same visitor again
	h.visit(host, "/", "203.0.113.11", browserUA, "")        // another
	h.visit(host, "/", "203.0.113.11", browserUA+" x", "")   // another browser at the same address
	h.visit(host, "/missing", "203.0.113.12", browserUA, "") // an error answer isn't a visit
	h.visit(host, "/", "203.0.113.13", "Googlebot/2.1", "")  // a crawler isn't a visitor
	h.visit(host, "/", "203.0.113.14", "", "")               // nor is a request without a User-Agent
	h.visit(host, "/", "203.0.113.15", browserUA, key)       // the owner, on a public port

	var st api.Stats
	h.must(200, "GET", "/v1/sandboxes/web/stats", key, nil, &st)
	if st.Sandbox != sb.ID || st.Name != "web" || len(st.Days) != statsDefaultDays {
		t.Fatalf("stats: %+v", st)
	}
	today := st.Days[len(st.Days)-1]
	if today.Day != "2026-10-11" || st.Days[0].Day != "2026-10-05" {
		t.Fatalf("window: %s to %s", st.Days[0].Day, today.Day)
	}
	want := api.Traffic{Requests: 10, Status2xx: 8, Status4xx: 2, BytesSent: today.BytesSent, KeyRequests: 2}
	if today.Traffic != want || today.Visitors != 3 || today.BytesSent < 8*5 {
		t.Fatalf("today: %+v visitors %d, want %+v and 3", today.Traffic, today.Visitors, want)
	}
	if st.Total != today.Traffic || st.VisitorDays != 3 {
		t.Fatalf("total: %+v, visitor days %d", st.Total, st.VisitorDays)
	}

	// The next day: the same client is a new day's visitor, and the window moves.
	h.clock.Advance(24 * time.Hour)
	h.visit(host, "/", "203.0.113.10", browserUA, "")
	h.must(200, "GET", "/v1/sandboxes/web/stats?days=2", key, nil, &st)
	if len(st.Days) != 2 || st.Days[0].Day != "2026-10-11" || st.Days[1].Day != "2026-10-12" ||
		st.Days[1].Visitors != 1 || st.Days[1].Requests != 1 || st.Total.Requests != 11 || st.VisitorDays != 4 {
		t.Fatalf("two days: %+v", st)
	}
	h.errCode(400, api.CodeInvalid, "GET", "/v1/sandboxes/web/stats?days=0", key, nil)
	h.errCode(400, api.CodeInvalid, "GET", "/v1/sandboxes/web/stats?days=91", key, nil)
	other := h.project("eve")
	h.errCode(404, api.CodeNotFound, "GET", "/v1/sandboxes/web/stats", other, nil)

	// The metrics page.
	page := h.metricsPage()
	for _, line := range []string{
		`loam_port_requests_total{project="jake",sandbox="web",code="2xx"} 9`,
		`loam_port_requests_total{project="jake",sandbox="web",code="4xx"} 2`,
		`loam_port_request_duration_seconds_count{project="jake",sandbox="web"} 11`,
		`loam_sandboxes{project="jake",state="running"} 1`,
	} {
		if !strings.Contains(page, line+"\n") {
			t.Errorf("metrics page lacks %s:\n%s", line, page)
		}
	}

	// A deleted sandbox's series go with it.
	h.must(204, "DELETE", "/v1/sandboxes/web", key, nil, nil)
	if page := h.metricsPage(); strings.Contains(page, `sandbox="web"`) {
		t.Fatalf("the deleted sandbox is still on the page:\n%s", page)
	}
}

func (h *harness) metricsPage() string {
	h.t.Helper()
	rec := httptest.NewRecorder()
	h.srv.Metrics().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

// A WebSocket through a port URL is counted as one, with an open gauge while
// it lasts, and its visitor counts like any other.
func TestStatsWebSocket(t *testing.T) {
	h := newHarness(t)
	key := h.project("jake")
	sb := h.create(key, api.CreateSandbox{Name: "game", HTTPPort: 9000})
	h.must(200, "PUT", "/v1/sandboxes/game/ports/9000", key, map[string]string{"access": "public"}, nil)
	up := websocket.Upgrader{}
	h.node.Listen(sb.ID, 9000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			kind, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			c.WriteMessage(kind, msg)
		}
	}))
	hdr := http.Header{"Host": {"game--jake." + testDomain}, "User-Agent": {browserUA}, "X-Forwarded-For": {"198.51.100.7"}}
	ws, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.ts.URL, "http")+"/play", hdr)
	if err != nil {
		t.Fatalf("dial: %v %v", err, resp)
	}
	ws.WriteMessage(websocket.TextMessage, []byte("hi"))
	if _, msg, err := ws.ReadMessage(); err != nil || string(msg) != "hi" {
		t.Fatalf("echo: %q %v", msg, err)
	}
	if page := h.metricsPage(); !strings.Contains(page, `loam_port_websockets_open{project="jake",sandbox="game"} 1`) {
		t.Fatalf("no open WebSocket on the page:\n%s", page)
	}
	ws.Close()

	var st api.Stats
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.must(200, "GET", "/v1/sandboxes/game/stats?days=1", key, nil, &st)
		if st.Total.WebSockets == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond) // the proxy counts it once both ends have closed
	}
	if st.Total.Requests != 1 || st.Total.WebSockets != 1 || st.Total.Status2xx != 0 || st.VisitorDays != 1 {
		t.Fatalf("stats: %+v", st)
	}
	if page := h.metricsPage(); !strings.Contains(page, `loam_port_websockets_open{project="jake",sandbox="game"} 0`) ||
		!strings.Contains(page, `loam_port_requests_total{project="jake",sandbox="game",code="websocket"} 1`) {
		t.Fatalf("after close:\n%s", page)
	}
}

func TestClientIP(t *testing.T) {
	for _, c := range []struct {
		xff  []string
		want string
	}{
		{nil, "192.0.2.1"},
		{[]string{"203.0.113.5"}, "203.0.113.5"},
		{[]string{"10.0.0.1, 203.0.113.5"}, "203.0.113.5"},
		{[]string{"10.0.0.1", "203.0.113.6 "}, "203.0.113.6"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "192.0.2.1:4000"
		for _, v := range c.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := clientIP(r); got != c.want {
			t.Errorf("%v: %q, want %q", c.xff, got, c.want)
		}
	}
}
