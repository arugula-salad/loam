// Package apiserver is loam-api: the public API of docs/API.md over the records
// in internal/db and the nodelets of docs/NODE.md. It owns authorization,
// placement, the lifecycle rules, the events and the port URLs; the sandbox
// sub-API it forwards to the agent unchanged.
//
// Phase 1 runs a single replica. The in-flight counts that keep a sandbox
// awake, the per-sandbox transition locks, the event bus and the lifecycle
// loop all live in this process's memory, so a second replica would pause
// sandboxes the first is serving. See runtime.go and lifecycle.go.
package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/arugula-salad/loam/docs"
	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/docsite"
	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

// Options configures a Server.
type Options struct {
	Store     db.Store
	Nodes     *Nodes
	Templates Templates
	// Domain is the parent of the port URLs: <name>--<project>.<Domain>.
	Domain string
	// URLDomains are the projects' own URL domains (domains.go): a
	// sandbox N of the project is also N.<domain>.
	URLDomains Domains
	// AllowedRegistries are the repository prefixes a sandbox's image may
	// come from (images.go). None refuses images.
	AllowedRegistries []string
	AdminToken        string
	// NodeToken is what nodelets take from loam-api and what the guest host
	// channel (/internal/guest/*) must carry.
	NodeToken string
	// PublicURL is the API's base URL as clients outside reach it
	// (https://api.<domain>): the identity tokens' issuer.
	PublicURL string
	// IdentityKeys sign the sandboxes' identity tokens (identity.go): the
	// first signs, all are published. None answers the identity routes 501.
	IdentityKeys []IdentityKey
	Log          *slog.Logger
	// Now is the clock; tests move it. Nil is time.Now.
	Now func() time.Time
	// PortWait is how long a port URL waits for a listener before 502
	// port_closed (docs/API.md, "Ports"). 0 is 10 s.
	PortWait time.Duration
	// LifecycleEvery is the lifecycle loop's period. 0 is a second.
	LifecycleEvery time.Duration
	// GuestCheckpointDelay is how long a memory checkpoint asked for from
	// inside waits after answering before it pauses the VM (guest.go). 0 is 250 ms.
	GuestCheckpointDelay time.Duration
	// NodeLostAfter is how long a node must be unreachable before its
	// sandboxes may wake elsewhere (docs/NODE.md, "A lost node"). 0 is 2 minutes.
	NodeLostAfter time.Duration
	// EventStreamIdle is how long a nodelet's event stream may send nothing,
	// not even a keepalive (every 15 s), before loam-api drops it and
	// reconnects, reconciling as it does. A frozen nodelet's connection stays
	// open (docs/NODE.md, "Events"). 0 is 45 s.
	EventStreamIdle time.Duration
	// GCEvery is the period of the snapshot and layer collection (gc.go),
	// which also runs whenever something frees a snapshot. 0 is 30 s.
	GCEvery time.Duration
	// SecretKey seals project secrets (secrets.go): SecretKeySize bytes.
	// Without one, the secret routes and a sandbox naming secrets are
	// unsupported.
	SecretKey []byte
	// NoAccessLog leaves out the log line per request to a port URL
	// (traffic.go). The counts and metrics are kept either way.
	NoAccessLog bool
}

// Server is loam-api.
type Server struct {
	opts  Options
	store db.Store
	nodes *Nodes
	log   *slog.Logger
	mux   *http.ServeMux
	bus   *bus
	// sealer seals secret values; nil without a secret key.
	sealer *sealer
	// traffic and metrics count the requests to port URLs (traffic.go).
	traffic *trafficBuf
	metrics *portMetrics

	// started is when this process started looking at nodes: a node it has
	// never heard from is lost NodeLostAfter after it.
	started time.Time
	gcKick  chan struct{}

	mu          sync.Mutex
	rts         map[string]*sbrt
	watchers    map[string]bool
	storedEarly map[string]storedEarly
}

func New(opts Options) *Server {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.PortWait == 0 {
		opts.PortWait = 10 * time.Second
	}
	if opts.LifecycleEvery == 0 {
		opts.LifecycleEvery = time.Second
	}
	if opts.GuestCheckpointDelay == 0 {
		opts.GuestCheckpointDelay = 250 * time.Millisecond
	}
	if opts.EventStreamIdle == 0 {
		opts.EventStreamIdle = 45 * time.Second
	}
	if opts.NodeLostAfter == 0 {
		opts.NodeLostAfter = 2 * time.Minute
	}
	if opts.GCEvery == 0 {
		opts.GCEvery = 30 * time.Second
	}
	s := &Server{opts: opts, store: opts.Store, nodes: opts.Nodes, log: opts.Log, bus: newBus(),
		rts: map[string]*sbrt{}, watchers: map[string]bool{}, gcKick: make(chan struct{}, 1),
		storedEarly: map[string]storedEarly{}, traffic: newTrafficBuf()}
	s.metrics = s.newMetrics()
	if opts.SecretKey != nil {
		sl, err := newSealer(opts.SecretKey)
		if err != nil {
			// The caller checks the key (ParseSecretKey); a bad one here is a bug.
			panic(err)
		}
		s.sealer = sl
	}
	s.started = s.now()
	if s.nodes != nil && s.nodes.Now == nil {
		s.nodes.Now = s.now
	}
	s.routes()
	return s
}

func (s *Server) now() time.Time { return s.opts.Now().UTC() }

func (s *Server) routes() {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })

	m.HandleFunc("POST /v1/projects", s.admin(s.createProject))
	m.HandleFunc("GET /v1/projects", s.admin(s.listProjects))
	m.HandleFunc("DELETE /v1/projects/{project}", s.admin(s.deleteProject))
	m.HandleFunc("POST /v1/projects/{project}/keys", s.admin(s.createKey))
	m.HandleFunc("GET /v1/projects/{project}/keys", s.admin(s.listKeys))
	m.HandleFunc("DELETE /v1/projects/{project}/keys/{key}", s.admin(s.deleteKey))
	m.HandleFunc("GET /v1/whoami", s.keyed(s.whoami))

	m.HandleFunc("GET /v1/templates", s.keyed(s.listTemplates))
	m.HandleFunc("GET /v1/network/defaults", s.keyed(s.networkDefaults))

	m.HandleFunc("PUT /v1/secrets/{name}", s.keyed(s.keepsSecrets(s.putSecret)))
	m.HandleFunc("GET /v1/secrets", s.keyed(s.keepsSecrets(s.listSecrets)))
	m.HandleFunc("GET /v1/secrets/{name}", s.keyed(s.keepsSecrets(s.getSecret)))
	m.HandleFunc("DELETE /v1/secrets/{name}", s.keyed(s.keepsSecrets(s.deleteSecret)))
	m.HandleFunc("GET /v1/events", s.keyed(s.events))

	m.HandleFunc("POST /v1/sandboxes", s.keyed(s.createSandbox))
	m.HandleFunc("GET /v1/sandboxes", s.keyed(s.listSandboxes))
	m.HandleFunc("GET /v1/sandboxes/{sandbox}", s.keyed(s.getSandbox))
	m.HandleFunc("PATCH /v1/sandboxes/{sandbox}", s.keyed(s.patchSandbox))
	m.HandleFunc("DELETE /v1/sandboxes/{sandbox}", s.keyed(s.deleteSandbox))
	m.HandleFunc("POST /v1/sandboxes/{sandbox}/pause", s.keyed(s.pauseSandbox))
	m.HandleFunc("POST /v1/sandboxes/{sandbox}/stop", s.keyed(s.stopSandbox))
	m.HandleFunc("POST /v1/sandboxes/{sandbox}/wake", s.keyed(s.wakeSandbox))

	m.HandleFunc("POST /v1/sandboxes/{sandbox}/checkpoints", s.keyed(s.createCheckpoint))
	m.HandleFunc("GET /v1/sandboxes/{sandbox}/checkpoints", s.keyed(s.listCheckpoints))
	m.HandleFunc("GET /v1/sandboxes/{sandbox}/checkpoints/{checkpoint}", s.keyed(s.getCheckpoint))
	m.HandleFunc("DELETE /v1/sandboxes/{sandbox}/checkpoints/{checkpoint}", s.keyed(s.deleteCheckpoint))
	m.HandleFunc("POST /v1/sandboxes/{sandbox}/checkpoints/{checkpoint}/restore", s.keyed(s.restoreCheckpoint))

	// Port settings are loam-api's and never reach the agent (docs/AGENT.md).
	m.HandleFunc("PUT /v1/sandboxes/{sandbox}/ports/{port}", s.keyed(s.setPort))
	m.HandleFunc("GET /v1/sandboxes/{sandbox}/ports/settings", s.keyed(s.listPortSettings))
	// Traffic at the sandbox's URLs, as loam-api's proxy counted it (traffic.go).
	m.HandleFunc("GET /v1/sandboxes/{sandbox}/stats", s.keyed(s.stats))

	// Everything else under a sandbox is the agent's (forward.go).
	m.HandleFunc("/v1/sandboxes/{sandbox}/{rest...}", s.keyed(s.forward))

	m.HandleFunc("GET /internal/guest/self", s.guest(s.guestSelf))
	m.HandleFunc("POST /internal/guest/checkpoints", s.guest(s.guestCheckpoint))
	m.HandleFunc("GET /internal/guest/identity/token", s.guest(s.guestIdentityToken))

	// The OpenID Connect issuer's documents, without a key (identity.go).
	m.HandleFunc("GET /.well-known/openid-configuration", s.openIDConfiguration)
	m.HandleFunc("GET /.well-known/jwks.json", s.jwks)

	// The user docs, public, on the API's host only: ServeHTTP has given
	// sandbox hosts their sandboxes before the mux sees anything. They are
	// rendered here, at startup, rather than on the first reader's request.
	site()
	m.HandleFunc("GET /docs", s.docs)
	m.HandleFunc("GET /docs/", s.docs)
	s.mux = m
}

// site is the user docs, rendered once per process however many Servers
// there are.
var site = sync.OnceValue(func() *docsite.Site {
	st, err := docsite.New(docs.FS)
	if err != nil {
		// The pages are embedded and docsite's tests render them; one that
		// won't render here is a bug.
		panic(err)
	}
	return st
})

func (s *Server) docs(w http.ResponseWriter, r *http.Request) {
	if !site().Serve(w, r) {
		s.writeErr(w, notFound("page"))
	}
}

// ServeHTTP dispatches by host first: a sandbox's URL under loam's domain
// (ports.go) or under a project's URL domain (domains.go) goes to the
// sandbox, and only a host that is neither reaches the API. Traefik forwards
// every path on sandbox hosts, /internal/ included, so the API's routes must
// never answer on one.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := requestHost(r)
	if t, ok := s.portTarget(host); ok {
		s.servePortURL(w, r, t)
		return
	}
	if t, matched, ok := s.domainTarget(host); matched {
		if !ok {
			s.writeErr(w, notFound("sandbox"))
			return
		}
		s.servePortURL(w, r, t)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// Run runs the lifecycle loop, the nodelet watchers, the collection of
// snapshots and layers and the traffic flushes until ctx ends.
func (s *Server) Run(ctx context.Context) {
	go s.runGC(ctx)
	go s.runTraffic(ctx)
	s.runLifecycle(ctx)
}

// apiErr is an error answer (docs/API.md, "Errors").
type apiErr struct {
	status     int
	code       string
	msg        string
	retryAfter int
}

func (e *apiErr) Error() string { return e.code + ": " + e.msg }

func errf(status int, code, format string, args ...any) *apiErr {
	return &apiErr{status: status, code: code, msg: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) *apiErr {
	return errf(http.StatusBadRequest, api.CodeInvalid, format, args...)
}

func notFound(what string) *apiErr {
	return errf(http.StatusNotFound, api.CodeNotFound, "%s not found", what)
}

func conflict(format string, args ...any) *apiErr {
	return errf(http.StatusConflict, api.CodeConflict, format, args...)
}

func capacityErr(msg string) *apiErr {
	return &apiErr{status: http.StatusServiceUnavailable, code: api.CodeCapacity, msg: msg, retryAfter: 30}
}

func unavailable(format string, args ...any) *apiErr {
	return errf(http.StatusServiceUnavailable, api.CodeSandboxUnavailable, format, args...)
}

// nodeErr is what a nodelet's failure means to a client: capacity is capacity,
// anything else is a sandbox that couldn't be reached or moved.
func nodeErr(err error) *apiErr {
	var e *nodeapi.Err
	if errors.As(err, &e) {
		switch e.Code {
		case api.CodeCapacity:
			return capacityErr("the node can't take the sandbox now: " + e.Message)
		case api.CodeConflict:
			return conflict("%s", e.Message)
		}
		return unavailable("the node refused: %s", e.Message)
	}
	return unavailable("the node could not be reached: %v", err)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) writeErr(w http.ResponseWriter, err error) {
	var e *apiErr
	switch {
	case errors.As(err, &e):
	case errors.Is(err, db.ErrNotFound):
		e = notFound("record")
	case errors.Is(err, db.ErrExists):
		e = errf(http.StatusConflict, api.CodeAlreadyExists, "it already exists")
	case errors.Is(err, db.ErrNoSecret):
		e = invalid("secrets: %v", err)
	case errors.Is(err, context.Canceled):
		return // the client went away; nobody to answer
	default:
		s.log.Error("internal error", "err", err)
		e = errf(http.StatusInternalServerError, "internal", "%v", err)
	}
	if e.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.retryAfter))
	}
	writeJSON(w, e.status, api.Error{Error: api.ErrorDetail{Code: e.code, Message: e.msg}})
}

// decode reads a JSON body strictly: a field the API doesn't know is refused,
// not dropped, since an option nobody honours is worse than an error.
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid("bad request body: %v", err)
	}
	return nil
}
