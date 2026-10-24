// Package e2b is loam's E2B adapter (docs/E2B.md): the official, unmodified
// E2B SDKs (Python and JS) create, use, pause, resume and kill sandboxes
// against loam-api as they do against hosted E2B. It is sandpit's E2B front
// end (frontend/e2b), moved from sandpit's engine onto loam's records.
//
// It serves two planes, on the hosts of its domain (Handler):
//
//   - the control plane, E2B's REST API (/v2/sandboxes, /sandboxes/{id}, ...)
//     on api.<domain>, authenticated by X-API-Key, which is a loam project
//     key (control.go);
//   - the envd plane: requests for a sandbox, routed the way E2B's sandbox
//     proxy routes them (by a <port>-<id>.<domain> Host, by the
//     E2b-Sandbox-Id and E2b-Sandbox-Port headers that E2B_SANDBOX_URL mode
//     sends, or by the signature on a signed file URL) and carried to that
//     port in the guest over loam's port stream (envd.go). Port 49983 is
//     envd, E2B's own in-guest daemon, which the e2b template's image starts
//     at every boot as a loam service (images/e2b); any other port is the
//     user's.
//
// An E2B sandbox is a loam sandbox in the key's project, made through loam's
// own API in this process (loam.go), so it is a sandbox like any other there
// too. What E2B knows beyond loam's record (its timeout and pause, envd's
// access token) is the adapter's record, db.E2BSandbox; its metadata are its
// labels under "e2b/". envd is handed its token, env vars and default user by
// POST /init after every fresh boot of the VM, before anything else of E2B's
// reaches it (ensureInit).
package e2b

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/arugula-salad/loam/internal/db"
)

// Defaults that hosted E2B has and the SDKs rely on.
const (
	// EnvdPort is where envd listens in the guest (images/e2b/envd.json).
	EnvdPort = 49983
	// EnvdVersion is the envd the e2b image is built with (images/e2b).
	// The SDKs gate features on the version reported at create; 0.9.0 is past
	// every gate they have.
	EnvdVersion = "0.9.0"
	// defaultTimeout is POST /v2/sandboxes' and connect's (SandboxTimeoutDefaultV2).
	defaultTimeout = 300 * time.Second
	// defaultTimeoutV1 is the deprecated POST /sandboxes'.
	defaultTimeoutV1 = 15 * time.Second
	// clientID is deprecated but required by the spec; hosted always returns this.
	clientID = "6532622b"
	// baseAlias is E2B's default template: what Sandbox.create() asks for
	// when it names none.
	baseAlias   = "base"
	defaultUser = "user"
	defaultHome = "/home/user"
	// LabelPrefix keeps an E2B sandbox's metadata apart from the labels
	// loam's own API gives it: metadata key k is label "e2b/k".
	LabelPrefix = "e2b/"
)

// Options configures the adapter.
type Options struct {
	// Domain is the E2B domain, the SDKs' E2B_DOMAIN: the control plane is
	// api.<Domain>, a sandbox's port P is <P>-<id>.<Domain>. It is reported
	// as every sandbox's `domain`, so getHost(port) points here; with a port
	// (e2b.localhost:7901, a local loam-api), getHost carries it too.
	Domain string
	// Template is the loam template E2B's "base" is made from (the e2b
	// template, whose image runs envd).
	Template string
	// Templates are E2B templates beyond base, by the name the SDKs send
	// (without a tag); "base" is refused, being Template's.
	Templates map[string]Template
	// MaxTimeout is the longest timeout a sandbox may be given (hosted: 1 h
	// on the hobby tier, 24 h on pro). 0 is 24 h.
	MaxTimeout time.Duration
	// InitWait is how long envd's /init is retried after a fresh boot, while
	// envd starts. 0 is 30 s.
	InitWait time.Duration
	Log      *slog.Logger
	// Now is the clock; tests move it. Nil is time.Now.
	Now func() time.Time
}

// Template is an E2B template beyond base: the loam template it is made
// from, whose image must run envd as the e2b template's does (images/e2b-
// fountain builds on it), and the user envd runs commands as when a request
// names none. Hosted E2B takes that user, and the working directory, from the
// template's build; here the working directory is the user's home, which a
// process of another user may not be able to enter (homes are 0750).
type Template struct {
	Loam string
	// User is the default user; "" is base's, user.
	User string
}

// defaultsOf is the default user and working directory envd is given for a
// sandbox of the E2B template named.
func (f *Frontend) defaultsOf(name string) (user, workdir string) {
	t, ok := f.opts.Templates[name]
	if !ok || t.User == "" {
		return defaultUser, defaultHome
	}
	if t.User == "root" {
		return "root", "/root"
	}
	return t.User, "/home/" + t.User
}

// Loam is what the adapter needs of loam-api (*apiserver.Server): its API, in
// this process, and the few things a front end can't do over it (the
// apiserver's frontend.go).
type Loam interface {
	RefreshE2BSecrets(context.Context, string) error
	http.Handler
	AsProject(ctx context.Context, project string) (context.Context, error)
	ProjectOfKey(ctx context.Context, key string) (string, error)
	DialPort(ctx context.Context, id string, port int) (net.Conn, error)
	BootedAt(ctx context.Context, id string) (time.Time, error)
	// Seal and Open keep envd's access token sealed under loam-api's secret
	// key, bound to the sandbox's ID; CanSeal is whether there is one.
	CanSeal() bool
	TrafficToken(sandbox string) (string, error)
	Seal(sandbox string, value []byte) ([]byte, error)
	Open(sandbox string, sealed []byte) ([]byte, error)
}

// token is the sandbox's envd access token, opened.
func (f *Frontend) token(row db.E2BSandbox) (string, error) {
	b, err := f.loam.Open(row.Sandbox, row.SealedToken)
	return string(b), err
}

// Store is where the adapter keeps its records (db.Store).
type Store interface {
	CreateE2BTemplate(context.Context, db.E2BTemplate) error
	E2BTemplates(context.Context, string) ([]db.E2BTemplate, error)
	UpdateE2BTemplate(context.Context, string, string, func(*db.E2BTemplate) error) (db.E2BTemplate, error)
	DeleteE2BTemplate(context.Context, string, string) error
	E2BSecrets(context.Context, string) ([]db.E2BSecret, error)
	PutE2BSecret(context.Context, db.E2BSecret, bool) (db.E2BSecret, error)
	DeleteE2BSecret(context.Context, string, string) error
	CreateE2B(ctx context.Context, e db.E2BSandbox) error
	E2B(ctx context.Context, sandbox string) (db.E2BSandbox, error)
	E2Bs(ctx context.Context, project string) ([]db.E2BSandbox, error)
	UpdateE2B(ctx context.Context, sandbox string, change func(*db.E2BSandbox) error) (db.E2BSandbox, error)
}

// Frontend is the E2B API on loam.
type Frontend struct {
	opts Options
	// host is the domain without a port: what a request's Host is matched
	// against, where the domain may carry the port of a server reached
	// without a proxy in front (local development).
	host    string
	loam    Loam
	store   Store
	log     *slog.Logger
	control http.Handler

	// locks serializes the control-plane transitions of one sandbox (pause,
	// connect, timeout, kill) against each other, so that, say, a connect and
	// a pause racing cannot leave the record saying the opposite of the VM.
	locks sync.Map // id -> *sync.Mutex
	// initLocks serializes envd's /init per sandbox; inited is the boot
	// (BootedAt) each sandbox's envd was last initialized in.
	initLocks sync.Map // id -> *sync.Mutex
	mu        sync.Mutex
	inited    map[string]time.Time
}

// New is the adapter on loam.
func New(opts Options, loam Loam, store Store) *Frontend {
	if opts.MaxTimeout <= 0 {
		opts.MaxTimeout = 24 * time.Hour
	}
	if opts.InitWait <= 0 {
		opts.InitWait = 30 * time.Second
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	opts.Domain = strings.ToLower(strings.TrimSuffix(opts.Domain, "."))
	f := &Frontend{opts: opts, host: hostOnly(opts.Domain), loam: loam, store: store, log: opts.Log.With("api", "e2b"),
		inited: map[string]time.Time{}}
	f.control = f.controlPlane()
	return f
}

func (f *Frontend) now() time.Time { return f.opts.Now().UTC() }

// lock serializes control-plane transitions on one sandbox.
func lock(m *sync.Map, id string) func() {
	v, _ := m.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// Handler serves the adapter's hosts (api.<domain>, <domain> and
// <label>.<domain>) and hands every other request to next, loam's own API.
// Which plane a request on the adapter's hosts is for is decided the way
// E2B's own proxy decides it (route).
func (f *Frontend) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !f.ours(r.Host) {
			next.ServeHTTP(w, r)
			return
		}
		if t, ok := f.route(r); ok {
			f.serveSandbox(w, r, t)
			return
		}
		f.control.ServeHTTP(w, r)
	})
}

// ours is whether host is the E2B domain or one label under it.
func (f *Frontend) ours(host string) bool {
	if f.host == "" {
		return false
	}
	host = hostOnly(host)
	if host == f.host {
		return true
	}
	label, ok := strings.CutSuffix(host, "."+f.host)
	return ok && label != "" && !strings.Contains(label, ".")
}

// hostOnly is a Host without its port, lowercased, without a trailing dot.
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}
