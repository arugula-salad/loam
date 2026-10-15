package broker

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arugula-salad/loam/internal/nodelet/network"
	"github.com/arugula-salad/loam/pkg/api"
)

// Broker terminates the TLS of a VM's connections to its secrets' hosts and
// brokers each request on them.
type Broker struct {
	CA  *CA
	Log *slog.Logger
	// Dial connects to name on port at an address the node resolved itself,
	// never a blocked one (network.Proxy.DialName).
	Dial func(ctx context.Context, name string, port int) (net.Conn, error)
	// RootCAs verifies the hosts; nil is the system's roots.
	RootCAs *x509.CertPool

	once      sync.Once
	transport *http.Transport
}

// handshakeTimeout bounds the guest's side of the TLS handshake.
const handshakeTimeout = 10 * time.Second

// smallBody is the largest answer with a known length that is read whole
// before its head goes back, so a value in it is a 502 rather than a cut
// stream. A longer one, or one of unknown length (a stream), is searched as
// it goes, and cut where a value appears.
const smallBody = 64 << 10

func (b *Broker) init() {
	b.once.Do(func() {
		b.transport = &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				p, err := strconv.Atoi(port)
				if err != nil {
					return nil, err
				}
				return b.Dial(ctx, host, p)
			},
			TLSClientConfig:     &tls.Config{RootCAs: b.RootCAs},
			TLSHandshakeTimeout: 10 * time.Second,
			ForceAttemptHTTP2:   true,
			// Every request on a connection of its own. A pooled connection the
			// origin closed while idle would have to be redialed and the request
			// sent again, and a streamed body can't be: dialing per request is
			// always correct.
			DisableKeepAlives: true,
			// The broker asks for an uncompressed answer itself, so it can read it.
			DisableCompression:     true,
			MaxResponseHeaderBytes: 1 << 20,
		}
	})
}

// Serve ends the TLS of c, a VM's connection to name on 443 that the proxy
// has read the ClientHello of (and replays), and brokers the requests on it
// until it ends. policy is the VM's current policy, asked for every request,
// so a changed secret is used from the next one; nil once the VM no longer
// runs.
func (b *Broker) Serve(c net.Conn, vm, name string, policy func() *network.Policy) {
	b.init()
	cert, err := b.CA.Leaf(name)
	if err != nil {
		b.Log.Error("broker: no leaf", "vm", vm, "host", name, "err", err)
		return
	}
	tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{*cert}, NextProtos: []string{"h2", "http/1.1"}})
	hctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	err = tc.HandshakeContext(hctx)
	cancel()
	if err != nil {
		b.Log.Info("broker: the guest's TLS handshake failed", "vm", vm, "host", name, "err", err)
		return
	}
	// net/http serves the one connection, HTTP/2 or HTTP/1.1 as the guest
	// negotiated. It returns once the connection has ended (or been hijacked
	// for an upgrade); handlers still running are waited for, since the
	// caller closes c when Serve returns.
	var handlers sync.WaitGroup
	done := make(chan struct{})
	var doneOnce sync.Once
	h := &handler{b: b, vm: vm, name: name, policy: policy}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlers.Add(1)
			defer handlers.Done()
			h.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
		ErrorLog:          log.New(io.Discard, "", 0),
		ConnState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateClosed || s == http.StateHijacked {
				doneOnce.Do(func() { close(done) })
			}
		},
	}
	srv.Serve(&oneListener{c: tc, done: done})
	handlers.Wait()
}

// oneListener hands out one connection, then waits for it to be done.
type oneListener struct {
	c    net.Conn
	done chan struct{}
	used bool
}

func (l *oneListener) Accept() (net.Conn, error) {
	if !l.used {
		l.used = true
		return l.c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneListener) Close() error   { return nil }
func (l *oneListener) Addr() net.Addr { return l.c.LocalAddr() }

// handler brokers the requests of one connection to name.
type handler struct {
	b      *Broker
	vm     string
	name   string
	policy func() *network.Policy
}

// brokered is what a request carries from ServeHTTP to the reverse proxy's
// hooks: the secrets it uses, and the values the answer is searched for.
type brokered struct {
	secrets []network.Secret
	target  string
	needles [][]byte
	err     string
}

type brokeredKey struct{}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	path, _, _ := strings.Cut(r.RequestURI, "?")
	sw := &statusWriter{ResponseWriter: w}
	defer func() {
		// The path and never the query, which can hold a credential the
		// broker never brokered (a signed URL is one); no header values.
		h.b.Log.Info("brokered request", "vm", h.vm, "method", r.Method, "host", h.name, "path", path,
			"status", sw.status, "took", time.Since(start).Round(time.Millisecond))
	}()
	host := r.Host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		host = hh
	}
	if !strings.EqualFold(strings.TrimSuffix(host, "."), h.name) {
		// The connection was checked, and its secrets chosen, for the
		// server name; a request for another host on it is neither.
		refuse(sw, http.StatusMisdirectedRequest, "this connection is for "+h.name)
		return
	}
	pol := h.policy()
	if pol == nil {
		refuse(sw, http.StatusBadGateway, "the sandbox no longer runs on this node")
		return
	}
	target := r.RequestURI
	if !strings.HasPrefix(target, "/") {
		// An absolute-form target (or "*"): what it names is r.URL.
		target = r.URL.RequestURI()
		path = r.URL.EscapedPath()
	}
	secrets := applicable(pol.Secrets(), h.name, path)
	if why := check(secrets, target, r.Header); why != "" {
		h.b.Log.Warn("brokered request refused", "vm", h.vm, "host", h.name, "path", path, "reason", why)
		refuse(sw, http.StatusBadGateway, why)
		return
	}
	bk := &brokered{secrets: secrets, target: target, needles: needles(pol.Secrets())}
	rp := &httputil.ReverseProxy{
		Rewrite:        h.rewrite,
		Transport:      h.b.transport,
		FlushInterval:  -1, // a streamed answer streams
		ModifyResponse: h.inspect,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, errReflected) {
				h.b.Log.Warn("brokered answer cut: it holds a secret's value", "vm", h.vm, "host", h.name, "path", path)
				refuse(w, http.StatusBadGateway, "the answer holds a secret's value")
				return
			}
			h.b.Log.Info("brokered request failed", "vm", h.vm, "host", h.name, "path", path, "err", err)
			refuse(w, http.StatusBadGateway, "the request to "+h.name+" failed")
		},
		ErrorLog: log.New(io.Discard, "", 0),
	}
	rp.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), brokeredKey{}, bk)))
}

// rewrite makes the request that goes to the host: the same, to the server
// name, with its secrets put in and no compression asked for. The reverse
// proxy has dropped the hop-by-hop headers already.
func (h *handler) rewrite(pr *httputil.ProxyRequest) {
	bk := pr.In.Context().Value(brokeredKey{}).(*brokered)
	target := rewrite(bk.secrets, bk.target, pr.Out.Header)
	u := &url.URL{Scheme: "https", Host: h.name}
	if path, query, ok := strings.Cut(target, "?"); strings.HasPrefix(path, "//") {
		// Opaque would be read as an authority; the absolute form says the same.
		u.Opaque, u.RawQuery, u.ForceQuery = "//"+h.name+path, query, ok && query == ""
	} else {
		u.Opaque, u.RawQuery, u.ForceQuery = path, query, ok && query == ""
	}
	pr.Out.URL = u
	pr.Out.Host = h.name
	pr.Out.Header.Set("Accept-Encoding", "identity")
}

// inspect searches the answer's head for the VM's values, and sets its body
// to be searched as it goes.
func (h *handler) inspect(resp *http.Response) error {
	bk := resp.Request.Context().Value(brokeredKey{}).(*brokered)
	if len(bk.needles) == 0 {
		return nil
	}
	if headHolds(resp.Status, resp.Header, bk.needles) {
		return errReflected
	}
	if resp.StatusCode == http.StatusSwitchingProtocols {
		return nil // the frames after an upgrade are spliced, never read
	}
	for _, ce := range resp.Header.Values("Content-Encoding") {
		for _, c := range strings.Split(ce, ",") {
			if c = strings.TrimSpace(strings.ToLower(c)); c != "" && c != "identity" {
				// Asked for identity, answered otherwise: what can't be read
				// can't be searched.
				return errors.New("the host answered with Content-Encoding " + c)
			}
		}
	}
	if resp.ContentLength >= 0 && resp.ContentLength <= smallBody {
		body, err := io.ReadAll(io.LimitReader(resp.Body, smallBody+1))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if holds(body, bk.needles) || headHolds("", resp.Trailer, bk.needles) {
			return errReflected
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return nil
	}
	resp.Body = &scanner{body: resp.Body, resp: resp, needles: bk.needles}
	return nil
}

// CodeBrokerFailed is the error code of a brokered request the node answers
// 502 itself (docs/API.md, "Secrets").
const CodeBrokerFailed = "broker_failed"

// refuse answers with the API's error shape.
func refuse(w http.ResponseWriter, status int, msg string) {
	code := CodeBrokerFailed
	if status == http.StatusMisdirectedRequest {
		code = api.CodeInvalid
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(api.Error{Error: api.ErrorDetail{Code: code, Message: msg}})
}

// statusWriter notes the status for the log. Unwrap lets the reverse proxy
// reach the connection's flusher and hijacker through it.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
