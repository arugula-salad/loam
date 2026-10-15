package apiserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/pkg/api"
)

// Port access (docs/API.md, "Ports").
const (
	accessPrivate = "private"
	accessPublic  = "public"
	tokenCookie   = "loam_token"
	tokenParam    = "loam_token"
)

func (s *Server) portAccess(ctx context.Context, sb db.Sandbox, port int) (string, error) {
	ps, err := s.store.Ports(ctx, sb.ID)
	if err != nil {
		return "", err
	}
	for _, p := range ps {
		if p.Port == port {
			return p.Access, nil
		}
	}
	return accessPrivate, nil
}

// setPort is PUT .../ports/{port}. Port settings are records: loam-api's, never
// the agent's, so this doesn't wake the sandbox.
func (s *Server) setPort(w http.ResponseWriter, r *http.Request, c caller) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || checkPort(port) != nil {
		s.writeErr(w, invalid("a port is 1 to 65535"))
		return
	}
	var req struct {
		Access string `json:"access"`
	}
	if err := decode(r, &req); err != nil {
		s.writeErr(w, err)
		return
	}
	if req.Access != accessPrivate && req.Access != accessPublic {
		s.writeErr(w, invalid("access is private or public"))
		return
	}
	sb, err := s.lookup(r.Context(), c, r.PathValue("sandbox"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if err := s.store.SetPort(r.Context(), db.PortSetting{Sandbox: sb.ID, Port: port, Access: req.Access}); err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.PortSetting{Port: port, Access: req.Access, URL: s.portURL(sb, port)})
}

func (s *Server) listPortSettings(w http.ResponseWriter, r *http.Request, c caller) {
	sb, err := s.lookup(r.Context(), c, r.PathValue("sandbox"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	ps, err := s.store.Ports(r.Context(), sb.ID)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := api.PortSettingList{Ports: []api.PortSetting{}}
	for _, p := range ps {
		out.Ports = append(out.Ports, api.PortSetting{Port: p.Port, Access: p.Access, URL: s.portURL(sb, p.Port)})
	}
	writeJSON(w, http.StatusOK, out)
}

// portTarget is what a port URL's host names: <name>--<project>.<domain> is
// the sandbox's http_port, <port>--<name>--<project>.<domain> any port. Names
// and projects can't contain "--", so the split is unambiguous.
type portTarget struct {
	name, project string
	port          int // 0: the sandbox's http_port
}

func (s *Server) portTarget(host string) (portTarget, bool) {
	if s.opts.Domain == "" {
		return portTarget{}, false
	}
	label, ok := strings.CutSuffix(host, "."+s.opts.Domain)
	if !ok || strings.Contains(label, ".") || !strings.Contains(label, "--") {
		return portTarget{}, false
	}
	parts := strings.Split(label, "--")
	var t portTarget
	switch len(parts) {
	case 2:
		t.name, t.project = parts[0], parts[1]
	case 3:
		p, err := strconv.Atoi(parts[0])
		if err != nil || checkPort(p) != nil || strconv.Itoa(p) != parts[0] {
			return portTarget{}, false
		}
		t.port, t.name, t.project = p, parts[1], parts[2]
	default:
		return portTarget{}, false
	}
	return t, validName(t.name) && validName(t.project)
}

// requestHost is a request's host as the host dispatch compares it: without
// a port, lowercase, without a trailing dot.
func requestHost(r *http.Request) string {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// errPortClosed is the port gate giving up: the sandbox is up, nothing
// listens on the port.
var errPortClosed = errors.New("nothing listens on the port")

// servePortURL serves a sandbox's port URL: checks access, wakes the
// sandbox, and proxies HTTP (and WebSocket upgrades) to the port over the
// agent's /ports/{port}/connect stream.
func (s *Server) servePortURL(w http.ResponseWriter, r *http.Request, t portTarget) {
	ctx := r.Context()
	sb, err := s.store.SandboxByName(ctx, t.project, t.name)
	if err != nil {
		s.writeErr(w, notFound("sandbox"))
		return
	}
	port := t.port
	if port == 0 {
		port = sb.HTTPPort
	}
	ob, done := s.observePort(w, r, sb, port)
	w = ob
	private := true
	defer done(&private)
	access, err := s.portAccess(ctx, sb, port)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	private = access == accessPrivate
	usedHeader := false
	if access == accessPrivate {
		var ok bool
		if usedHeader, ok = s.portAuthorized(w, r, sb); !ok {
			return
		}
	}
	sb, release, err := s.acquire(ctx, sb)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	defer release()
	cctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	defer s.rt(sb.ID).track(cancel)()
	r = r.WithContext(cctx)

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", pr.In.Host
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Proto", "https") // TLS ends at Traefik
			if usedHeader {
				pr.Out.Header.Del("Authorization") // the project key is ours, not the app's
			}
			stripTokenCookie(pr.Out)
		},
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return s.dialPort(ctx, sb, port)
			},
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			switch {
			case errors.Is(context.Cause(r.Context()), errSandboxGone):
			case errors.Is(err, errPortClosed):
				s.writeErr(w, errf(http.StatusBadGateway, api.CodePortClosed, "nothing listens on port %d in the sandbox", port))
			default:
				s.writeErr(w, errf(http.StatusBadGateway, api.CodeSandboxUnavailable, "the request to port %d failed: %v", port, err))
			}
		},
	}
	rp.ServeHTTP(w, r)
}

// portAuthorized checks a private port's credential: the project's key as a
// bearer token or in the loam_token cookie. ?loam_token=<key> sets the cookie
// and redirects to the same URL without it, so the key doesn't stay in the
// address bar or reach the app. usedHeader says the Authorization header was
// the credential, which is then not forwarded.
func (s *Server) portAuthorized(w http.ResponseWriter, r *http.Request, sb db.Sandbox) (usedHeader, ok bool) {
	owns := func(secret string) bool {
		c, err := s.keyCaller(r, secret)
		return err == nil && c.project.Name == sb.Project
	}
	if tok := r.URL.Query().Get(tokenParam); tok != "" {
		if !owns(tok) {
			s.writeErr(w, unauthorized())
			return false, false
		}
		http.SetCookie(w, &http.Cookie{Name: tokenCookie, Value: tok, Path: "/", HttpOnly: true, Secure: true,
			SameSite: http.SameSiteLaxMode})
		u := *r.URL
		q := u.Query()
		q.Del(tokenParam)
		u.RawQuery = q.Encode()
		u.Scheme, u.Host = "", ""
		http.Redirect(w, r, u.RequestURI(), http.StatusFound)
		return false, false
	}
	if tok := bearer(r); tok != "" && owns(tok) {
		return true, true
	}
	if ck, err := r.Cookie(tokenCookie); err == nil && owns(ck.Value) {
		return false, true
	}
	s.writeErr(w, errf(http.StatusUnauthorized, api.CodeUnauthorized,
		"this port is private: send the project's key as a bearer token, or visit with ?loam_token=<key>"))
	return false, false
}

func stripTokenCookie(r *http.Request) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name != tokenCookie {
			r.AddCookie(c)
		}
	}
}

// dialPort opens a byte stream to port in the sandbox: the agent stream, a
// WebSocket to /ports/{port}/connect on it, and that WebSocket's binary frames
// as a net.Conn. Nothing listening yet is retried for up to PortWait (a
// sandbox just woken has its app still starting), then errPortClosed.
func (s *Server) dialPort(ctx context.Context, sb db.Sandbox, port int) (net.Conn, error) {
	deadline := time.Now().Add(s.opts.PortWait)
	backoff := 100 * time.Millisecond
	for {
		c, closed, err := s.connectPort(ctx, sb, port)
		if err == nil {
			return c, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		left := time.Until(deadline)
		if left <= 0 {
			if closed {
				return nil, errPortClosed
			}
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(min(backoff, left)):
		}
		backoff = min(backoff*2, 500*time.Millisecond)
	}
}

// connectPort is one attempt; closed says the agent answered that nothing
// listens (or any refusal of the upgrade, which is retried the same way).
func (s *Server) connectPort(ctx context.Context, sb db.Sandbox, port int) (net.Conn, bool, error) {
	dialer := websocket.Dialer{
		NetDialContext:   func(ctx context.Context, _, _ string) (net.Conn, error) { return s.agentDial(sb)(ctx) },
		HandshakeTimeout: 10 * time.Second,
	}
	ws, resp, err := dialer.DialContext(ctx, fmt.Sprintf("ws://agent/ports/%d/connect", port), nil)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
			return nil, true, fmt.Errorf("port %d: %d", port, resp.StatusCode)
		}
		return nil, false, err
	}
	return &wsConn{ws: ws}, false, nil
}

// wsConn is a WebSocket's binary frames as a byte stream.
type wsConn struct {
	ws *websocket.Conn
	r  io.Reader

	// gorilla/websocket allows one writer at a time, and the proxy's copy goroutines can write
	// concurrently.
	writeMu   sync.Mutex
	closeOnce sync.Once
}

func (c *wsConn) Read(p []byte) (int, error) {
	for {
		if c.r == nil {
			kind, r, err := c.ws.NextReader()
			if err != nil {
				var ce *websocket.CloseError
				if errors.As(err, &ce) && ce.Code == websocket.CloseNormalClosure {
					return 0, io.EOF
				}
				return 0, err
			}
			if kind != websocket.BinaryMessage {
				continue
			}
			c.r = r
		}
		n, err := c.r.Read(p)
		if err == io.EOF {
			c.r = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (c *wsConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsConn) Close() error {
	c.closeOnce.Do(func() {
		c.ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	})
	return c.ws.Close()
}

func (c *wsConn) LocalAddr() net.Addr  { return c.ws.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr { return c.ws.RemoteAddr() }
func (c *wsConn) SetDeadline(t time.Time) error {
	c.ws.SetReadDeadline(t)
	return c.ws.SetWriteDeadline(t)
}
func (c *wsConn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *wsConn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }
