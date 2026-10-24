// Package agent is loam-agent, the runtime inside every sandbox VM: exec
// sessions, files, ports, services, tasks and activity, served as the sandbox
// sub-API of docs/API.md (without its /v1/sandboxes/{sandbox} prefix), plus
// the /internal routes the nodelet calls (docs/AGENT.md).
//
// Only the host can open a vsock stream to a guest, so the agent trusts every
// stream it accepts: there is no token, no handshake and no claim. The same
// handler, minus /internal, serves processes inside the VM on the guest API
// socket.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

// Ports on vsock (docs/AGENT.md, docs/NODE.md).
const (
	// AgentPort is where the agent serves HTTP; the nodelet dials it.
	AgentPort = 1024
	// HostPort is the host channel: the guest dials the host (CID 2) on it.
	HostPort = 1025
	// HostCID is the vsock address of the host, seen from a guest.
	HostCID = 2
)

// Defaults for a guest; tests point them at temporary directories.
const (
	DefaultStateDir = "/var/lib/loam" // on the sandbox's disk: services, their logs, hooks
	DefaultRunDir   = "/run/loam"     // tmpfs: pid files, the last wake, the guest API socket
	DefaultUser     = "loam"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  64 * 1024,
	WriteBufferSize: 64 * 1024,
	CheckOrigin:     func(*http.Request) bool { return true },
	// A failed handshake answers in the error shape too.
	Error: func(w http.ResponseWriter, _ *http.Request, status int, reason error) {
		writeErr(w, status, api.CodeInvalid, reason.Error())
	},
}

// System is what the agent does to the guest as a whole. The real one
// (NewSystem) is root's; tests use a fake, since none of it can be done on a
// developer's host.
type System interface {
	SetClock(time.Time) error
	SetHostname(string) error
	// SetSandboxID refreshes the identity visible to snapshotted processes.
	SetSandboxID(string) error
	// ConfigureNetwork converges eth0 on n (a memory snapshot forked onto a
	// new VM wakes with its source's address).
	ConfigureNetwork(n *nodeapi.WakeNetwork) error
	// DropCaches syncs and gives the page cache back, so the balloon can
	// reclaim it before a snapshot.
	DropCaches()
	Sync()
	Poweroff()
}

// Config is what New needs.
type Config struct {
	StateDir string
	RunDir   string
	System   System
	// HostDial opens a stream on the host channel; nil leaves /self,
	// /checkpoints and /identity/token on the guest socket answering 501.
	HostDial func(ctx context.Context) (net.Conn, error)
	// RunAsSelf runs every process and file operation as the agent's own
	// account, for an agent that is not root (tests on a host). A request
	// naming another user is refused, since it can't be honoured.
	RunAsSelf bool
	// ImageMeta is the image's environment file; empty means /etc/loam/image.json.
	ImageMeta string
	// TrustRoot is the directory the broker CA's paths are under (ca.go):
	// "/" in a guest, empty meaning that; elsewhere for an agent on a host
	// and for tests, which must not touch the host's trust store.
	TrustRoot string
}

// Server is the agent.
type Server struct {
	cfg      Config
	sessions *Manager
	services *Supervisor
	tasks    taskTable
	ports    *portWatcher

	mu        sync.Mutex
	wake      nodeapi.Wake
	woken     bool // a wake has been applied since this process started
	startedSv bool
}

// New builds the agent. If an earlier agent process on this boot recorded a
// wake (the server crashed and PID 1 restarted it), that wake is taken up
// again and the services start at once: no new wake will come until the next
// resume.
func New(cfg Config) *Server {
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir
	}
	if cfg.RunDir == "" {
		cfg.RunDir = DefaultRunDir
	}
	if cfg.TrustRoot == "" {
		cfg.TrustRoot = "/"
	}
	if cfg.ImageMeta == "" {
		cfg.ImageMeta = "/etc/loam/image.json"
	}
	s := &Server{cfg: cfg}
	s.sessions = newManager()
	s.ports = &portWatcher{mgr: s.sessions}
	s.services = newSupervisor(s, filepath.Join(cfg.StateDir, "services"), filepath.Join(cfg.StateDir, "logs", "services"),
		filepath.Join(cfg.RunDir, "services"))
	if b, err := os.ReadFile(s.wakePath()); err == nil {
		var w nodeapi.Wake
		if json.Unmarshal(b, &w) == nil {
			s.wake, s.woken = w, true
			s.startServices()
		}
	}
	return s
}

func (s *Server) wakePath() string { return filepath.Join(s.cfg.RunDir, "wake.json") }

// Close stops what the agent supervises; for tests.
func (s *Server) Close() {
	s.services.stopAll(time.Second)
	s.sessions.killAll()
}

// Handler is the vsock surface: the sandbox sub-API and /internal.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerSubAPI(mux)
	mux.HandleFunc("POST /internal/wake", s.handleWake)
	mux.HandleFunc("POST /internal/presuspend", s.handlePresuspend)
	mux.HandleFunc("POST /internal/sync", s.handleSync)
	mux.HandleFunc("GET /internal/activity", s.handleActivity)
	mux.HandleFunc("POST /internal/poweroff", s.handlePoweroff)
	return mux
}

// registerSubAPI mounts the sandbox sub-API (docs/API.md without the
// /v1/sandboxes/{sandbox} prefix), which both sockets serve.
func (s *Server) registerSubAPI(mux *http.ServeMux) {
	s.registerExec(mux)
	s.registerFS(mux)
	s.registerPorts(mux)
	s.registerServices(mux)
	s.registerTasks(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, api.CodeNotFound, "no such route: "+r.Method+" "+r.URL.Path)
	})
}

// writeErr answers with the error shape of docs/API.md.
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, api.Error{Error: api.ErrorDetail{Code: code, Message: msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// decodeBody reads a JSON body of at most limit bytes, refusing unknown
// fields: a misspelt option would otherwise be silently dropped.
func decodeBody(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, api.CodeInvalid, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// parseBool is strict: anything but true/false (or absent, which is def) is an error.
func parseBool(v string, def bool) (bool, error) {
	if v == "" {
		return def, nil
	}
	return strconv.ParseBool(v)
}

// touch records activity: a byte in or out of an exec session, a file
// operation, a port stream (docs/AGENT.md, /internal/activity).
func (s *Server) touch() { s.sessions.touch() }

func logf(format string, args ...any) { log.Printf(format, args...) }
