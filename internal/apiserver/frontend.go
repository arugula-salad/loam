package apiserver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

// A front end in loam-api's process (internal/e2b, docs/E2B.md) is a client
// of this API like any other: it serves its own protocol and makes loam's
// calls on ServeHTTP, so every rule of docs/API.md is this server's to apply.
// What it can't do over the API is here: act for a project without its key
// (a call that arrives with a credential of the front end's own, such as an
// envd access token), reach a port as a byte stream without a WebSocket of its
// own, and see when a VM last booted.

// Error is a refusal as docs/API.md answers it ("Errors"), for a front end
// to put in its own protocol's terms.
type Error struct {
	Status  int
	Code    string
	Message string
	// RetryAfter is seconds, for capacity; 0 is none.
	RetryAfter int
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// public is err as an *Error when it is one of this API's refusals, and as
// it is otherwise.
func public(err error) error {
	var e *apiErr
	if errors.As(err, &e) {
		return &Error{Status: e.status, Code: e.code, Message: e.msg, RetryAfter: e.retryAfter}
	}
	return err
}

// projectCtx carries the project a front end acts for. Only code in this
// process can put it in a request's context; nothing a client sends can.
type projectCtx struct{}

// AsProject is ctx for a call on ServeHTTP that acts as the project, as its
// key would. The project must exist.
func (s *Server) AsProject(ctx context.Context, project string) (context.Context, error) {
	if _, err := s.store.Project(ctx, project); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return ctx, public(notFound("project"))
		}
		return ctx, err
	}
	return context.WithValue(ctx, projectCtx{}, project), nil
}

// ProjectOfKey is the project a key belongs to: a front end's own
// authentication (E2B's X-API-Key) with loam's keys.
func (s *Server) ProjectOfKey(ctx context.Context, key string) (string, error) {
	c, err := s.keyCaller((&http.Request{}).WithContext(ctx), key)
	if err != nil {
		return "", public(err)
	}
	return c.project.Name, nil
}

// frontendCaller is the caller AsProject put in r's context, if any.
func (s *Server) frontendCaller(r *http.Request) (caller, bool, error) {
	name, ok := r.Context().Value(projectCtx{}).(string)
	if !ok {
		return caller{}, false, nil
	}
	p, err := s.store.Project(r.Context(), name)
	if err != nil {
		return caller{}, true, unauthorized()
	}
	return caller{project: p}, true, nil
}

// sealedAD binds a front end's sealed value to the sandbox it is kept for, so
// that one copied into another row doesn't open; its prefix keeps it apart
// from a project secret's (secretAD). A sandbox ID holds no NUL.
func sealedAD(sandbox string) []byte {
	return []byte("loam frontend credential\x00" + sandbox)
}

// CanSeal is whether Seal can: whether this deployment has a secret key.
func (s *Server) CanSeal() bool { return s.sealer != nil }

// Seal seals a credential a front end keeps for a sandbox (envd's access
// token) with loam-api's secret key, as project secrets are sealed. Without
// a secret key it is unsupported: a credential that can't be kept sealed
// isn't kept.
func (s *Server) Seal(sandbox string, value []byte) ([]byte, error) {
	if s.sealer == nil {
		return nil, public(unsupported("this deployment has no secret key, so it can't keep a sandbox's credentials"))
	}
	return s.sealer.sealWith(sealedAD(sandbox), value), nil
}

// Open opens what Seal sealed for the same sandbox.
func (s *Server) Open(sandbox string, sealed []byte) ([]byte, error) {
	if s.sealer == nil {
		return nil, public(unsupported("this deployment has no secret key, so it can't keep a sandbox's credentials"))
	}
	v, err := s.sealer.openWith(sealedAD(sandbox), sealed)
	if err != nil {
		// Never the value in the message, only that it couldn't be had.
		return nil, errors.New("a sealed credential can't be opened (was the secret key changed, or the row moved?)")
	}
	return v, nil
}

// ErrPortClosed is DialPort's answer when nothing listens on the port.
var ErrPortClosed = errors.New("nothing listens on the port")

// DialPort is a byte stream to port in sandbox id, which the caller has
// authorized itself. It is a call on the sandbox like any other: the sandbox
// is woken if it sleeps and held awake until the stream is closed, and a
// transition that takes the VM away (a pause, a stop, a delete) closes it.
// Nothing listening is ErrPortClosed at once, without the wait a port URL
// gives a listener.
func (s *Server) DialPort(ctx context.Context, id string, port int) (net.Conn, error) {
	if err := checkPort(port); err != nil {
		return nil, public(err)
	}
	sb, err := s.store.Sandbox(ctx, id)
	if errors.Is(err, db.ErrNotFound) {
		return nil, public(notFound("sandbox"))
	} else if err != nil {
		return nil, err
	}
	sb, release, err := s.acquire(ctx, sb)
	if err != nil {
		return nil, public(err)
	}
	// The stream outlives ctx (the dial's): it ends when it is closed, or
	// when a transition ends the sandbox's calls.
	held, cancel := context.WithCancelCause(context.Background())
	untrack := s.rt(sb.ID).track(cancel)
	c, closed, err := s.connectPort(ctx, sb, port)
	if err != nil {
		untrack()
		cancel(nil)
		release()
		if closed {
			return nil, ErrPortClosed
		}
		return nil, public(unavailable("port %d of the sandbox can't be reached: %v", port, err))
	}
	stop := context.AfterFunc(held, func() { c.Close() })
	return &heldConn{Conn: c, done: func() {
		stop()
		untrack()
		cancel(nil)
		release()
	}}, nil
}

// heldConn is a port stream that holds its sandbox awake until it is closed.
type heldConn struct {
	net.Conn
	once sync.Once
	done func()
}

func (c *heldConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.done)
	return err
}

// BootedAt is when the VM of sandbox id last booted fresh, by its node's
// account (docs/NODE.md, VM.booted_at): a resume from a memory snapshot on
// the same VM keeps it, anything that starts the guest's processes afresh
// moves it. The zero time is a VM that isn't running. A front end that keeps
// state in a process the guest starts at boot (envd) sets it up again when
// this changes.
func (s *Server) BootedAt(ctx context.Context, id string) (time.Time, error) {
	sb, err := s.store.Sandbox(ctx, id)
	if errors.Is(err, db.ErrNotFound) {
		return time.Time{}, public(notFound("sandbox"))
	} else if err != nil {
		return time.Time{}, err
	}
	cl, err := s.nodes.Client(ctx, sb.Node)
	if err != nil {
		return time.Time{}, public(unavailable("its node %q can't be reached: %v", sb.Node, err))
	}
	vm, err := cl.VM(ctx, id)
	if err != nil {
		return time.Time{}, public(nodeErr(err))
	}
	if vm.State != nodeapi.VMRunning || vm.BootedAt == nil {
		return time.Time{}, nil
	}
	return *vm.BootedAt, nil
}

// TrafficToken derives a stable credential for a sandbox's private E2B ports.
// A separate key keeps traffic authentication apart from secret encryption.
func (s *Server) TrafficToken(sandbox string) (string, error) {
	if s.sealer == nil {
		return "", public(unsupported("this deployment has no secret key"))
	}
	mac := hmac.New(sha256.New, s.sealer.trafficKey)
	mac.Write([]byte(sandbox))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// RefreshE2BSecrets resolves current header credentials on each awake sandbox.
// Sleeping sandboxes receive the current values at their next wake.
func (s *Server) RefreshE2BSecrets(ctx context.Context, project string) error {
	all, err := s.store.Sandboxes(ctx, project)
	if err != nil {
		return err
	}
	var failures []error
	for _, sb := range all {
		if sb.State != api.StateRunning || len(sb.Network.RequestHeaders) == 0 {
			continue
		}
		rt := s.rt(sb.ID)
		rt.mu.Lock()
		cur, e := s.store.Sandbox(ctx, sb.ID)
		if e == nil {
			e = s.pushPolicyLocked(ctx, cur)
		}
		rt.mu.Unlock()
		if e != nil && !errors.Is(e, db.ErrNotFound) {
			failures = append(failures, e)
		}
	}
	return errors.Join(failures...)
}
