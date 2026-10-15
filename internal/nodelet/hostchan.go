package nodelet

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/internal/nodelet/firecracker"
	"github.com/arugula-salad/loam/pkg/api"
)

// The guest's host channel (docs/NODE.md): the agent connects from the guest
// to host vsock port 1025, which Firecracker turns into a connection to the
// unix socket v.sock_1025 in the VM's directory. Each VM has its own listener,
// and the listener is the identity: every request that arrives on it goes to
// loam-api's /internal/guest/<path> with X-Loam-Sandbox set to that VM, and
// whatever the guest put in that header, or in Authorization, is dropped.

type guestChan struct {
	srv *http.Server
}

// openGuest listens on v's host channel. It lives exactly as long as the VM
// process: opened before a boot or restore, closed by cleanup.
func (n *Nodelet) openGuest(v *vm) (*guestChan, error) {
	ln, err := firecracker.ListenGuest(v.dir, HostPort)
	if err != nil {
		return nil, err
	}
	g := &guestChan{srv: &http.Server{Handler: n.guestHandler(v.id), ReadHeaderTimeout: 10 * time.Second}}
	go g.srv.Serve(ln)
	return g, nil
}

// close is safe on nil. Closing the listener also unlinks its socket file.
func (g *guestChan) close() {
	if g != nil {
		g.srv.Close()
	}
}

// guestHandler forwards one VM's host channel to loam-api.
func (n *Nodelet) guestHandler(id string) http.Handler {
	base, err := url.Parse(n.opts.APIURL)
	if n.opts.APIURL == "" || err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, &Error{Status: http.StatusServiceUnavailable, Code: api.CodeSandboxUnavailable,
				Message: "this nodelet has no loam-api to forward the host channel to"})
		})
	}
	// Not JoinPath: on a URL without a path it makes a relative one.
	base.Path = strings.TrimSuffix(base.Path, "/") + "/internal/guest"
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(base)
			pr.Out.Header.Del(nodeapi.HeaderSandbox)
			pr.Out.Header.Set(nodeapi.HeaderSandbox, id)
			pr.Out.Header.Set("Authorization", "Bearer "+n.opts.Token)
		},
		// Streamed answers (a watch, say) reach the guest as they come.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			n.log.Warn("host channel: loam-api unreachable", "vm", id, "err", err)
			writeError(w, &Error{Status: http.StatusBadGateway, Code: api.CodeSandboxUnavailable, Message: "loam-api is unreachable: " + err.Error()})
		},
	}
}
