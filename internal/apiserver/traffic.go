package apiserver

import (
	"bufio"
	"context"
	"crypto/sha256"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/metrics"
	"github.com/arugula-salad/loam/pkg/api"
)

// Traffic at the sandboxes' URLs (docs/API.md, "Stats"). Every request to a
// port URL passes through servePortURL, so loam-api sees all of an app's
// traffic without the app doing anything, and counts it whether the sandbox
// was awake or not. Each request is counted three ways: the Prometheus
// metrics on the metrics listener, one access log line, and the day's counts
// and set of visitors in the store, which GET .../stats reads. The store's
// share is buffered here and flushed every trafficFlushEvery, before a stats
// answer, and at shutdown.

const (
	trafficFlushEvery = 30 * time.Second
	// trafficKeptDays is how long the store keeps a day's traffic, and the
	// longest window a stats request may ask for.
	trafficKeptDays  = 90
	statsDefaultDays = 7
	dayFormat        = "2006-01-02"
)

// portMetrics are loam-api's Prometheus series.
type portMetrics struct {
	reg        *metrics.Registry
	requests   *metrics.CounterVec
	duration   *metrics.HistogramVec
	bytes      *metrics.CounterVec
	wsOpen     *metrics.GaugeVec
	flushFails *metrics.CounterVec
}

func (s *Server) newMetrics() *portMetrics {
	reg := &metrics.Registry{}
	m := &portMetrics{
		reg: reg,
		requests: reg.Counter("loam_port_requests_total",
			"Requests to sandboxes' URLs, by status class (2xx..5xx) or websocket for an upgrade.",
			"project", "sandbox", "code"),
		duration: reg.Histogram("loam_port_request_duration_seconds",
			"Time to answer a request to a sandbox's URL, a wake included; WebSockets are not in it.",
			[]float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}, "project", "sandbox"),
		bytes: reg.Counter("loam_port_response_bytes_total",
			"Body bytes sent in answer to requests to sandboxes' URLs, WebSockets not included.",
			"project", "sandbox"),
		wsOpen: reg.Gauge("loam_port_websockets_open",
			"WebSockets open to sandboxes' URLs.", "project", "sandbox"),
		flushFails: reg.Counter("loam_traffic_flush_failures_total",
			"Flushes of the traffic counts to the store that failed; the counts are kept and tried again."),
	}
	reg.GaugeFunc("loam_sandboxes", "Sandboxes by project and state.", s.sandboxesByState, "project", "state")
	return m
}

// sandboxesByState is loam_sandboxes, read from the store at scrape time.
func (s *Server) sandboxesByState() []metrics.Sample {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sbs, err := s.store.Sandboxes(ctx, "")
	if err != nil {
		s.log.Warn("metrics: listing sandboxes failed", "err", err)
		return nil
	}
	n := map[[2]string]float64{}
	for _, sb := range sbs {
		n[[2]string{sb.Project, sb.State}]++
	}
	out := make([]metrics.Sample, 0, len(n))
	for k, v := range n {
		out = append(out, metrics.Sample{Values: []string{k[0], k[1]}, Value: v})
	}
	return out
}

// Metrics serves the Prometheus page. It is for the metrics listener, never
// the public one: the page names every project's sandboxes.
func (s *Server) Metrics() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", metrics.ContentType)
		s.metrics.reg.Write(w)
	})
}

// forgetMetrics drops a deleted sandbox's series, so that sandboxes coming
// and going don't grow the page for ever.
func (s *Server) forgetMetrics(sb db.Sandbox) {
	match := map[string]string{"project": sb.Project, "sandbox": sb.Name}
	s.metrics.requests.Delete(match)
	s.metrics.duration.Delete(match)
	s.metrics.bytes.Delete(match)
	s.metrics.wsOpen.Delete(match)
}

// observed is the ResponseWriter of a request to a port URL: it notes the
// status, the body's size, and whether the connection was taken over for a
// WebSocket. ReverseProxy hijacks for an upgrade and writes the 101 itself,
// so a hijack is what says "upgraded"; Unwrap gives ResponseController the
// rest (flushing) of the writer underneath.
type observed struct {
	http.ResponseWriter
	status    int
	bytes     int64
	upgraded  bool
	onUpgrade func()
}

func (o *observed) WriteHeader(code int) {
	// 1xx other than 101 are interim answers, not the status.
	if o.status == 0 && (code >= 200 || code == http.StatusSwitchingProtocols) {
		o.status = code
	}
	o.ResponseWriter.WriteHeader(code)
}

func (o *observed) Write(p []byte) (int, error) {
	if o.status == 0 {
		o.status = http.StatusOK
	}
	n, err := o.ResponseWriter.Write(p)
	o.bytes += int64(n)
	return n, err
}

func (o *observed) Unwrap() http.ResponseWriter { return o.ResponseWriter }

func (o *observed) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(o.ResponseWriter).Hijack()
	if err == nil {
		o.upgraded, o.status = true, http.StatusSwitchingProtocols
		if o.onUpgrade != nil {
			o.onUpgrade()
		}
	}
	return c, rw, err
}

// observePort wraps a port URL's request: the returned writer is the one to
// answer with, and done, deferred, counts the request when it is over.
// private reports whether the port turned out private (unknown counts as
// private: nobody's a visitor of a port that couldn't be looked up).
func (s *Server) observePort(w http.ResponseWriter, r *http.Request, sb db.Sandbox, port int) (*observed, func(private *bool)) {
	start := time.Now()
	ob := &observed{ResponseWriter: w}
	ob.onUpgrade = func() { s.metrics.wsOpen.Add(1, sb.Project, sb.Name) }
	return ob, func(private *bool) {
		if ob.upgraded {
			// Not Add: the sandbox may have been deleted, its series with it.
			s.metrics.wsOpen.AddExisting(-1, sb.Project, sb.Name)
		}
		s.countRequest(r, sb, port, ob, *private, time.Since(start))
	}
}

func (s *Server) countRequest(r *http.Request, sb db.Sandbox, port int, ob *observed, private bool, took time.Duration) {
	status := ob.status
	if status == 0 {
		status = http.StatusOK // what net/http sends for a handler that wrote nothing
	}
	// The project's key: a private port's every answered request carries it
	// (anything else was refused), a public port's only if it was sent.
	key := status != http.StatusUnauthorized && status != http.StatusForbidden &&
		(private || r.URL.Query().Has(tokenParam) || s.carriesKey(r, sb))
	ua := r.UserAgent()

	code := "websocket"
	if !ob.upgraded {
		code = strconv.Itoa(status/100) + "xx"
		s.metrics.duration.Observe(took.Seconds(), sb.Project, sb.Name)
		s.metrics.bytes.Add(float64(ob.bytes), sb.Project, sb.Name)
	}
	s.metrics.requests.Inc(sb.Project, sb.Name, code)

	var visitor string
	if !key && status < 400 && !machineUA(ua) {
		visitor = clientIP(r) + "\x00" + ua
	}
	s.traffic.add(sb.ID, s.now().Format(dayFormat), status, ob.upgraded, ob.bytes, key, visitor)

	if !s.opts.NoAccessLog {
		s.log.Info("port request", "project", sb.Project, "sandbox", sb.Name, "port", port,
			"host", r.Host, "method", r.Method, "path", r.URL.Path, "status", status,
			"duration_ms", took.Milliseconds(), "bytes", ob.bytes, "websocket", ob.upgraded,
			"key", key, "user_agent", ua)
	}
}

// carriesKey says whether a request to a public port carries the project's
// key, as a bearer token or the loam_token cookie: the owner, not a visitor.
// The request may be over, so its context's end doesn't stop the lookup.
func (s *Server) carriesKey(r *http.Request, sb db.Sandbox) bool {
	r = r.WithContext(context.WithoutCancel(r.Context()))
	owns := func(secret string) bool {
		c, err := s.keyCaller(r, secret)
		return err == nil && c.project.Name == sb.Project
	}
	if tok := bearer(r); tok != "" && owns(tok) {
		return true
	}
	ck, err := r.Cookie(tokenCookie)
	return err == nil && owns(ck.Value)
}

// clientIP is the address Traefik saw the request come from: the last
// X-Forwarded-For entry, which Traefik appends (and the only one, since it
// replaces what an untrusted client sends). Without one (a request straight
// to loam-api), the connection's own.
func clientIP(r *http.Request) string {
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		last := xff[len(xff)-1]
		if i := strings.LastIndexByte(last, ','); i >= 0 {
			last = last[i+1:]
		}
		if ip := strings.TrimSpace(last); ip != "" {
			return ip
		}
	}
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// machineUAs are User-Agents that aren't people: crawlers, link previews,
// uptime probes (Prometheus's blackbox exporter among them) and HTTP
// libraries. Their requests are counted; they are never visitors.
var machineUAs = []string{
	"bot", "crawl", "spider", "slurp", "preview", "blackbox", "uptime", "monitor", "probe",
	"headless", "curl/", "wget/", "python-requests", "python-urllib", "go-http-client", "okhttp", "axios",
	"node-fetch", "undici",
}

func machineUA(ua string) bool {
	if ua == "" {
		return true
	}
	ua = strings.ToLower(ua)
	return slices.ContainsFunc(machineUAs, func(m string) bool { return strings.Contains(ua, m) })
}

// visitorHash is what the store keeps of a visitor: the day's salt, the
// sandbox and the client, hashed, so that a visitor counts once per sandbox
// per day and the address can't be read back once the salt is gone.
func visitorHash(salt []byte, sandbox, client string) []byte {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(sandbox))
	h.Write([]byte{0})
	h.Write([]byte(client))
	return h.Sum(nil)[:16]
}

// trafficBuf is the traffic not yet in the store, by sandbox ID and day.
// Visitors are kept as the client (address and User-Agent) until the flush
// hashes them, so that the request path never waits on the store for a salt.
type trafficBuf struct {
	mu   sync.Mutex
	days map[[2]string]*trafficDay
	// flushing is held for a whole flush, so that two (the loop's and a stats
	// request's) don't interleave their read-modify-write of the buffer.
	flushing sync.Mutex
}

type trafficDay struct {
	api.Traffic
	visitors map[string]bool
}

func newTrafficBuf() *trafficBuf { return &trafficBuf{days: map[[2]string]*trafficDay{}} }

func (b *trafficBuf) add(sandbox, day string, status int, upgraded bool, bytes int64, key bool, visitor string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	d := b.day(sandbox, day)
	d.Requests++
	switch {
	case upgraded:
		d.WebSockets++
	case status >= 500:
		d.Status5xx++
	case status >= 400:
		d.Status4xx++
	case status >= 300:
		d.Status3xx++
	default:
		d.Status2xx++
	}
	d.BytesSent += bytes
	if key {
		d.KeyRequests++
	}
	if visitor != "" {
		d.visitors[visitor] = true
	}
}

func (b *trafficBuf) day(sandbox, day string) *trafficDay {
	k := [2]string{sandbox, day}
	d := b.days[k]
	if d == nil {
		d = &trafficDay{visitors: map[string]bool{}}
		b.days[k] = d
	}
	return d
}

// putBack returns a failed flush's days to the buffer, to go with the next.
func (b *trafficBuf) putBack(taken map[[2]string]*trafficDay) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, t := range taken {
		d := b.day(k[0], k[1])
		d.Requests += t.Requests
		d.Status2xx += t.Status2xx
		d.Status3xx += t.Status3xx
		d.Status4xx += t.Status4xx
		d.Status5xx += t.Status5xx
		d.WebSockets += t.WebSockets
		d.BytesSent += t.BytesSent
		d.KeyRequests += t.KeyRequests
		for v := range t.visitors {
			d.visitors[v] = true
		}
	}
}

// FlushTraffic writes the buffered traffic to the store. loam-api's main calls
// it once more after the listener has shut down.
func (s *Server) FlushTraffic(ctx context.Context) error {
	b := s.traffic
	b.flushing.Lock()
	defer b.flushing.Unlock()
	b.mu.Lock()
	taken := b.days
	b.days = map[[2]string]*trafficDay{}
	b.mu.Unlock()
	if len(taken) == 0 {
		return nil
	}
	// Oldest day first: asking for a day's salt deletes the earlier days',
	// so yesterday's last visitors are hashed before today's salt is made.
	keys := make([][2]string, 0, len(taken))
	for k := range taken {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b [2]string) int { return strings.Compare(a[1]+a[0], b[1]+b[0]) })
	salts := map[string][]byte{}
	out := make([]db.Traffic, 0, len(keys))
	for _, k := range keys {
		d := taken[k]
		t := db.Traffic{Sandbox: k[0], Day: k[1], Traffic: d.Traffic}
		if len(d.visitors) > 0 {
			salt, ok := salts[k[1]]
			if !ok {
				var err error
				if salt, err = s.store.VisitorSalt(ctx, k[1]); err != nil {
					b.putBack(taken)
					s.metrics.flushFails.Inc()
					return err
				}
				salts[k[1]] = salt
			}
			for v := range d.visitors {
				t.NewVisitors = append(t.NewVisitors, visitorHash(salt, k[0], v))
			}
		}
		out = append(out, t)
	}
	if err := s.store.AddTraffic(ctx, out); err != nil {
		b.putBack(taken)
		s.metrics.flushFails.Inc()
		return err
	}
	return nil
}

// runTraffic flushes the buffer every trafficFlushEvery until ctx ends.
func (s *Server) runTraffic(ctx context.Context) {
	t := time.NewTicker(trafficFlushEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.FlushTraffic(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("flushing traffic failed; kept for the next flush", "err", err)
			}
		}
	}
}

// stats is GET /v1/sandboxes/{sandbox}/stats?days=N.
func (s *Server) stats(w http.ResponseWriter, r *http.Request, c caller) {
	days := statsDefaultDays
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > trafficKeptDays {
			s.writeErr(w, invalid("days is 1 to %d", trafficKeptDays))
			return
		}
		days = n
	}
	sb, err := s.lookup(r.Context(), c, r.PathValue("sandbox"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	// Answer with what is stored even if the buffer couldn't be added to it:
	// the last half-minute missing beats no answer.
	if err := s.FlushTraffic(r.Context()); err != nil {
		s.log.Warn("flushing traffic for stats failed", "err", err)
	}
	today := s.now().Truncate(24 * time.Hour)
	first := today.AddDate(0, 0, -(days - 1))
	stored, err := s.store.TrafficOf(r.Context(), sb.ID, first.Format(dayFormat))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	byDay := map[string]db.Traffic{}
	for _, t := range stored {
		byDay[t.Day] = t
	}
	out := api.Stats{Sandbox: sb.ID, Name: sb.Name, Days: make([]api.StatsDay, 0, days)}
	for d := first; !d.After(today); d = d.AddDate(0, 0, 1) {
		day := d.Format(dayFormat)
		t := byDay[day]
		out.Days = append(out.Days, api.StatsDay{Day: day, Traffic: t.Traffic, Visitors: t.Visitors})
		out.Total.Requests += t.Requests
		out.Total.Status2xx += t.Status2xx
		out.Total.Status3xx += t.Status3xx
		out.Total.Status4xx += t.Status4xx
		out.Total.Status5xx += t.Status5xx
		out.Total.WebSockets += t.WebSockets
		out.Total.BytesSent += t.BytesSent
		out.Total.KeyRequests += t.KeyRequests
		out.VisitorDays += t.Visitors
	}
	writeJSON(w, http.StatusOK, out)
}
