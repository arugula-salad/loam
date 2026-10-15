package apiserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

// The sandbox sub-API is the agent's (docs/AGENT.md): loam-api checks the key,
// wakes the sandbox, holds it awake for the call, and forwards the request
// unchanged over the nodelet's agent stream, one stream per request.

// agentAreas are the first path segments the agent serves. Anything else
// under a sandbox is not found here, without waking it; /internal/ above all
// is the nodelet's and never forwarded.
var agentAreas = map[string]bool{"exec": true, "fs": true, "ports": true, "services": true, "tasks": true}

// logsPath is a service's log, the one read that may decline to wake.
var logsPath = regexp.MustCompile(`^/services/[^/]+/logs$`)

func (s *Server) forward(w http.ResponseWriter, r *http.Request, c caller) {
	// The path the agent sees: the rest after the sandbox, cleaned, so that no
	// spelling of it ("exec/../internal/wake") reaches what isn't forwarded.
	rest := path.Clean("/" + r.PathValue("rest"))
	area, _, _ := strings.Cut(strings.TrimPrefix(rest, "/"), "/")
	if !agentAreas[area] {
		s.writeErr(w, notFound("route"))
		return
	}
	sb, err := s.lookup(r.Context(), c, r.PathValue("sandbox"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	// A service's log is what a log shipper reads (docs/API.md, "Services"): it
	// may ask not to wake a sandbox that isn't running (?wake=false), and
	// following is not activity, so a reader doesn't keep an idle one awake.
	logs := logsPath.MatchString(rest)
	if logs && r.URL.Query().Get("wake") == "false" && sb.State != api.StateRunning {
		s.writeErr(w, conflict("sandbox %s is %s, and wake=false leaves it so", sb.Name, sb.State))
		return
	}
	cur, release, err := s.acquire(r.Context(), sb)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	// Watching ports is not activity (docs/API.md, "Ports"): the watch wakes
	// the sandbox but doesn't hold it, and a pause ends it like any stream.
	if rest == "/ports/watch" || (logs && r.URL.Query().Get("follow") == "true") {
		release()
	} else {
		defer release()
	}
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	defer s.rt(cur.ID).track(cancel)()
	r = r.WithContext(ctx)
	if websocket.IsWebSocketUpgrade(r) {
		s.relayWS(w, r, cur, rest)
		return
	}
	s.proxyHTTP(w, r, cur, rest)
}

// agentDial is a dialer of sb's agent stream, resolving its node, from the
// record, on each dial. A VM its record says is running but its node says
// isn't (the nodelet paused it while draining, and loam-api hasn't heard yet)
// is woken, on that node or another, and dialled again, rather than failing
// the call.
func (s *Server) agentDial(sb db.Sandbox) func(ctx context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		node := sb.Node
		if cur, err := s.store.Sandbox(ctx, sb.ID); err == nil {
			node = cur.Node
		}
		cl, err := s.nodes.ClientWait(ctx, node)
		if err != nil {
			return nil, err
		}
		c, err := cl.DialAgent(ctx, sb.ID)
		if !nodeapi.IsCode(err, api.CodeConflict) {
			return c, err
		}
		if herr := s.resumeBehindBack(ctx, sb.ID); herr != nil {
			return nil, err
		}
		cur, rerr := s.store.Sandbox(ctx, sb.ID)
		if rerr != nil {
			return nil, err
		}
		if cl, err = s.nodes.ClientWait(ctx, cur.Node); err != nil {
			return nil, err
		}
		return cl.DialAgent(ctx, sb.ID)
	}
}

// resumeBehindBack resumes the VM of a sandbox whose record says running
// while its node has it paused or stopped. The record stays running, so the
// calls in flight on it (this one among them) carry on.
func (s *Server) resumeBehindBack(ctx context.Context, id string) error {
	rt := s.rt(id)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	sb, err := s.store.Sandbox(ctx, id)
	if err != nil {
		return err
	}
	if sb.State != api.StateRunning {
		return fmt.Errorf("sandbox is %s", sb.State)
	}
	cl, err := s.nodeClient(ctx, sb)
	if err != nil {
		return err
	}
	vm, err := cl.VM(ctx, id)
	if err != nil {
		return err
	}
	switch vm.State {
	case nodeapi.VMRunning:
		return nil
	case nodeapi.VMPaused, nodeapi.VMStopped:
	default:
		return fmt.Errorf("its VM is %s", vm.State)
	}
	octx, cancel := opContext(ctx)
	defer cancel()
	wake, pol, err := s.bootOf(octx, sb)
	if err != nil {
		return err
	}
	if _, err := cl.Resume(octx, id, nodeapi.Resume{Wake: wake, Policy: pol}); err != nil {
		if !nodeapi.IsCode(err, codeNotReady) && !nodeapi.IsCode(err, api.CodeCapacity) {
			s.log.Warn("resuming a VM its node had paused failed", "sandbox", id, "was", vm.State, "err", err)
			return err
		}
		// The node won't take it back (it is draining, or full): record the
		// VM as it is, its snapshot with it, and wake it the long way, on
		// another node if need be. The calls in flight stay: they are waiting
		// for this wake.
		s.applyVM(ctx, cl, id, &vm, false)
		if sb, err = s.store.Sandbox(ctx, id); err != nil {
			return err
		}
		_, err := s.wakeLocked(ctx, rt, sb)
		return err
	}
	rt.touch(s.now())
	s.log.Info("resumed a VM its node had paused behind loam-api's back", "sandbox", id, "was", vm.State)
	return nil
}

// proxyHTTP forwards a plain HTTP call, streaming either way (SSE, tar, logs).
func (s *Server) proxyHTTP(w http.ResponseWriter, r *http.Request, sb db.Sandbox, rest string) {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "agent"
			pr.Out.URL.Path, pr.Out.URL.RawPath = rest, ""
			pr.Out.Host = "agent"
			// The key is ours; the agent trusts the stream and needs nothing else.
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
		},
		Transport:     nodeapi.AgentTransport(s.agentDial(sb)),
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(context.Cause(r.Context()), errSandboxGone) {
				return // a transition ended it; the connection just closes
			}
			s.writeErr(w, unavailable("the sandbox's agent could not be reached: %v", err))
		},
	}
	rp.ServeHTTP(w, r)
}

var upgrader = websocket.Upgrader{
	// Sub-API sockets authenticate with the bearer key, which a browser can't
	// attach to a WebSocket, so the origin is not what guards them.
	CheckOrigin: func(*http.Request) bool { return true },
}

// relayWS forwards a WebSocket (exec, attach, fs/watch, port connect) message
// by message rather than as bytes, so that loam-api can close the client's
// side with 1011 when the sandbox goes away under it: deleted, restored,
// paused, or its stream to the agent lost (docs/API.md, "Exec").
func (s *Server) relayWS(w http.ResponseWriter, r *http.Request, sb db.Sandbox, rest string) {
	dialer := websocket.Dialer{
		NetDialContext:   func(ctx context.Context, _, _ string) (net.Conn, error) { return s.agentDial(sb)(ctx) },
		HandshakeTimeout: 30 * time.Second,
		Subprotocols:     websocket.Subprotocols(r),
	}
	u := "ws://agent" + rest
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	agent, resp, err := dialer.DialContext(r.Context(), u, nil)
	if err != nil {
		if resp != nil {
			// The agent refused the socket (a missing cmd, say): its answer is
			// the answer, as for any other call.
			defer resp.Body.Close()
			for _, h := range []string{"Content-Type", "Retry-After"} {
				if v := resp.Header.Get(h); v != "" {
					w.Header().Set(h, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			io.Copy(w, io.LimitReader(resp.Body, 1<<20))
			return
		}
		s.writeErr(w, unavailable("the sandbox's agent could not be reached: %v", err))
		return
	}
	defer agent.Close()
	var hdr http.Header
	if p := agent.Subprotocol(); p != "" {
		hdr = http.Header{"Sec-Websocket-Protocol": {p}}
	}
	client, err := upgrader.Upgrade(w, r, hdr)
	if err != nil {
		return
	}
	defer client.Close()
	relay(r.Context(), client, agent)
}

// relay copies messages both ways between a client and the agent until one
// side closes or ctx ends. A close from either side is passed on with its code.
// The agent's side ending without a close (its stream died), or ctx ending
// because a transition took the sandbox away, closes the client with 1011.
func relay(ctx context.Context, client, agent *websocket.Conn) {
	type end struct {
		fromAgent bool
		err       error
	}
	ends := make(chan end, 2)
	pump := func(dst, src *websocket.Conn, fromAgent bool) {
		for {
			kind, rd, err := src.NextReader()
			if err != nil {
				ends <- end{fromAgent, err}
				return
			}
			wr, err := dst.NextWriter(kind)
			if err != nil {
				ends <- end{!fromAgent, err}
				return
			}
			if _, err := io.Copy(wr, rd); err != nil {
				ends <- end{fromAgent, err}
				return
			}
			if err := wr.Close(); err != nil {
				ends <- end{!fromAgent, err}
				return
			}
		}
	}
	go pump(agent, client, false)
	go pump(client, agent, true)

	closeWith := func(c *websocket.Conn, code int, text string) {
		c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, text), time.Now().Add(time.Second))
	}
	var first end
	select {
	case first = <-ends:
	case <-ctx.Done():
		closeWith(client, websocket.CloseInternalServerErr, "the sandbox went away")
		closeWith(agent, websocket.CloseGoingAway, "")
		return
	}
	var ce *websocket.CloseError
	normal := errors.As(first.err, &ce) && ce.Code != websocket.CloseAbnormalClosure
	switch {
	case first.fromAgent && normal:
		closeWith(client, ce.Code, ce.Text)
	case first.fromAgent:
		closeWith(client, websocket.CloseInternalServerErr, "the sandbox's agent stream ended")
	case normal:
		closeWith(agent, ce.Code, ce.Text)
	default:
		closeWith(agent, websocket.CloseGoingAway, "")
	}
	// Give the other side a moment to answer the close, then drop both.
	select {
	case <-ends:
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
	}
}
