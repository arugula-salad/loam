package agent

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"

	"github.com/arugula-salad/loam/pkg/api"
)

// The guest API is what processes inside the sandbox reach on
// /run/loam/api.sock, with no token: being inside is the credential. It is
// the sandbox sub-API, as on vsock, without /internal; and three routes that
// are not the agent's own, GET /self, POST /checkpoints and GET
// /identity/token, which go to loam-api over the host channel (docs/NODE.md, "The guest's host channel"). The
// nodelet sets which sandbox is asking from the VM the stream came from, so
// nothing the guest sends can claim another.

// GuestHandler is the handler for the guest socket.
func (s *Server) GuestHandler() http.Handler {
	mux := http.NewServeMux()
	s.registerSubAPI(mux)
	mux.HandleFunc("/internal/", notFound)
	host := s.hostProxy()
	mux.Handle("GET /self", host)
	mux.Handle("POST /checkpoints", host)
	mux.Handle("GET /identity/token", host)
	return mux
}

// hostProxy relays a request unchanged to the host channel. Its answer is
// copied to the caller as soon as it arrives: for a memory checkpoint, loam-api
// answers before it pauses the VM, and the caller must have that answer
// before the snapshot is taken, or a sandbox restored from it would be
// waiting on a stream that no longer exists.
func (s *Server) hostProxy() http.Handler {
	if s.cfg.HostDial == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeErr(w, http.StatusNotImplemented, api.CodeUnsupported, "this agent has no host channel")
		})
	}
	dial := s.cfg.HostDial
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = "http", "loam", "loam"
		},
		Transport: &http.Transport{DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) }},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeErr(w, http.StatusBadGateway, api.CodeSandboxUnavailable, "the host channel: "+err.Error())
		},
	}
}

// ListenGuest creates the guest socket, replacing one left on disk, open to
// every user in the sandbox (0666): docs/AGENT.md.
func ListenGuest(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}
