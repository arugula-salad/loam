package e2b

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/arugula-salad/loam/internal/apiserver"
	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/pkg/api"
)

// The envd plane: everything the SDK sends to a sandbox rather than to the
// API, carried into the guest over loam's port stream (no guest network is
// involved), plus the /init that prepares envd after every fresh boot.

// target is a sandbox port a request is for.
type target struct {
	id   string
	port int
	// unmatched is a signed file URL that matched no sandbox's token.
	unmatched bool
	// badPort is a port that is not a number from 1 to 65535.
	badPort bool
}

// portHost is the left-most label of a sandbox port's Host, <port>-<id>, as
// E2B's proxy parses it (shared/pkg/proxy/host.go): the port is everything
// before the first dash, so an ID may hold dashes, as loam's (sb-...) do.
var portHost = regexp.MustCompile(`^([0-9]+)-([a-z0-9][a-z0-9-]*)$`)

// route decides whether r is for a sandbox, the way E2B's proxy does: a Host
// of <port>-<id>.<domain>; else the E2b-Sandbox-Id / E2b-Sandbox-Port
// headers (what E2B_SANDBOX_URL mode sends with every envd call); else, for a
// signed /files URL (which under E2B_SANDBOX_URL names no sandbox at all), the
// sandbox whose access token the signature was made with. Anything else is
// the control plane.
func (f *Frontend) route(r *http.Request) (target, bool) {
	if label, ok := strings.CutSuffix(hostOnly(r.Host), "."+f.host); ok {
		if m := portHost.FindStringSubmatch(label); m != nil {
			return withPort(m[2], m[1]), true
		}
	}
	if id := r.Header.Get("E2b-Sandbox-Id"); id != "" {
		port := r.Header.Get("E2b-Sandbox-Port")
		if port == "" {
			port = strconv.Itoa(EnvdPort)
		}
		return withPort(id, port), true
	}
	if r.URL.Path == "/files" && r.URL.Query().Has("signature") {
		if id, ok := f.bySignature(r); ok {
			return target{id: id, port: EnvdPort}, true
		}
		return target{unmatched: true}, true
	}
	return target{}, false
}

// withPort is a target for port as a client wrote it, as a number: the dial
// and the check for envd's internal endpoints must see the same port, or
// "049983" (or anything carrying more than digits) would slip past the check
// and still reach envd.
func withPort(id, port string) target {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return target{id: id, badPort: true}
	}
	return target{id: id, port: n}
}

// fileSignature is envd's: v1_ and the unpadded base64 SHA-256 of
// "<path>:<read|write>:<user or ”>:<token>[:<expiration>]"
// (envd internal/api/auth.go; the SDKs' sandbox/signature.ts).
func fileSignature(path, op, user, token string, exp string) string {
	parts := []string{path, op, user, token}
	if exp != "" {
		parts = append(parts, exp)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, ":")))
	return "v1_" + base64.RawStdEncoding.EncodeToString(sum[:])
}

func fileOp(method string) string {
	return map[string]string{http.MethodGet: "read", http.MethodHead: "read", http.MethodPost: "write"}[method]
}

// bySignature finds the sandbox a signed /files URL was signed for, by
// recomputing the signature with each one's token: under E2B_SANDBOX_URL the
// URL carries nothing else that names it. envd checks the signature (and its
// expiration) again itself.
func (f *Frontend) bySignature(r *http.Request) (string, bool) {
	q := r.URL.Query()
	op := fileOp(r.Method)
	if op == "" {
		return "", false
	}
	sig := q.Get("signature")
	exp := q.Get("signature_expiration")
	if exp != "" {
		if _, err := strconv.ParseInt(exp, 10, 64); err != nil {
			return "", false
		}
	}
	rows, err := f.store.E2Bs(r.Context(), "")
	if err != nil {
		f.log.Warn("signed URL not routed: the records can't be read", "err", err)
		return "", false
	}
	now := f.now()
	for _, row := range rows {
		if paused(row, now) && !row.AutoResume {
			continue
		}
		token, err := f.token(row)
		if err != nil {
			f.log.Warn("a sandbox's token can't be opened", "sandbox", row.Sandbox, "err", err)
			continue
		}
		want := fileSignature(q.Get("path"), op, q.Get("username"), token, exp)
		if subtle.ConstantTimeCompare([]byte(want), []byte(sig)) == 1 {
			return row.Sandbox, true
		}
	}
	return "", false
}

// envdAuthorized reports whether r carries what envd will ask of it: the
// sandbox's access token as X-Access-Token, or an unexpired /files signature
// made with it. envd checks again itself, but only once the sandbox is awake;
// checking here first means traffic without either can't wake a sleeping
// sandbox, or keep a running one from pausing. User ports are not checked:
// they are public, as on hosted E2B.
func envdAuthorized(r *http.Request, token string, now time.Time) bool {
	if tok := r.Header.Get("X-Access-Token"); tok != "" {
		return subtle.ConstantTimeCompare([]byte(tok), []byte(token)) == 1
	}
	q := r.URL.Query()
	sig := q.Get("signature")
	op := fileOp(r.Method)
	if sig == "" || op == "" || r.URL.Path != "/files" {
		return false
	}
	exp := q.Get("signature_expiration")
	if exp != "" {
		n, err := strconv.ParseInt(exp, 10, 64)
		if err != nil || now.Unix() > n {
			return false
		}
	}
	want := fileSignature(q.Get("path"), op, q.Get("username"), token, exp)
	return subtle.ConstantTimeCompare([]byte(want), []byte(sig)) == 1
}

// envdInternal are envd's endpoints for the orchestrator, never served through
// E2B's public proxy. /init in particular is ours to call (ensureInit).
var envdInternal = map[string]bool{"/init": true, "/freeze": true, "/unfreeze": true,
	"/fsfreeze": true, "/fsthaw": true, "/collapse": true, "/upgrade": true}

// sandboxNotFound is the proxy's answer for a sandbox it cannot route to, as
// hosted E2B's proxy gives it (shared/pkg/proxy/template): 502, which the
// SDK's isRunning() reads as "not running".
func sandboxNotFound(w http.ResponseWriter, id string) {
	writeJSON(w, http.StatusBadGateway, map[string]any{"sandboxId": id, "message": "The sandbox was not found", "code": http.StatusBadGateway})
}

// serveSandbox carries r to t's port in the guest, waking a sandbox loam has
// asleep (it is still running as far as E2B is concerned), and holding it
// awake until the response is done, which for a process or watcher stream is
// as long as the stream stays open.
func (f *Frontend) serveSandbox(w http.ResponseWriter, r *http.Request, t target) {
	if t.unmatched {
		writeErr(w, http.StatusUnauthorized, "invalid signature: it matches no running sandbox's access token")
		return
	}
	if t.badPort {
		writeErr(w, http.StatusBadRequest, "invalid sandbox port")
		return
	}
	// The internal-endpoint check below and envd's own router must agree on
	// the path, so it is cleaned once here and forwarded as cleaned.
	if clean := path.Clean("/" + r.URL.Path); clean != r.URL.Path {
		r.URL.Path, r.URL.RawPath = clean, ""
	}
	row, err := f.store.E2B(r.Context(), t.id)
	if err != nil {
		sandboxNotFound(w, t.id)
		return
	}
	now := f.now()
	asleep := paused(row, now)
	if asleep && !row.AutoResume {
		sandboxNotFound(w, t.id)
		return
	}
	envd := t.port == EnvdPort
	if envd && envdInternal[r.URL.Path] {
		writeErr(w, http.StatusNotFound, "not found: "+r.URL.Path+" is envd's internal API")
		return
	}
	if envd {
		token, err := f.token(row)
		if err != nil {
			f.log.Error("a sandbox's token can't be opened", "sandbox", t.id, "err", err)
			sandboxNotFound(w, t.id)
			return
		}
		if !envdAuthorized(r, token, now) {
			writeErr(w, http.StatusUnauthorized, "unauthorized access, please provide a valid access token or method signing if supported")
			return
		}
	}
	if asleep {
		// autoResume: traffic resumes it, with the timeout it was created with.
		if row, err = f.autoResume(r.Context(), row); err != nil {
			f.log.Warn("could not resume a sandbox for its traffic", "sandbox", t.id, "err", err)
			if codeOf(err) == api.CodeCapacity {
				f.fail(w, t.id, err)
				return
			}
			sandboxNotFound(w, t.id)
			return
		}
	}
	// A Connect stream's request is read while its response is being written.
	http.NewResponseController(w).EnableFullDuplex()
	serveStream(w, r, f.sandboxProxy(row, t.port), "the connection to sandbox "+t.id+" ended before the stream completed")
}

// autoResume resumes a sandbox paused with autoResume, as a connect would.
func (f *Frontend) autoResume(ctx context.Context, row db.E2BSandbox) (db.E2BSandbox, error) {
	defer lock(&f.locks, row.Sandbox)()
	cur, err := f.store.E2B(ctx, row.Sandbox)
	if err != nil {
		return row, apiNotFound(err)
	}
	if !paused(cur, f.now()) {
		return cur, nil
	}
	return f.wake(context.WithoutCancel(ctx), cur, time.Duration(cur.TimeoutS)*time.Second)
}

// sandboxProxy is the reverse proxy to one port of a sandbox. It streams both
// ways, flushing every write, and passes everything through as it is: the
// access token, the SDK's encodings, the Host. Its dial is loam's port
// stream, which wakes the sandbox and holds it; envd is initialized in the
// boot the dial found before the request goes over it.
func (f *Frontend) sandboxProxy(row db.E2BSandbox, port int) *httputil.ReverseProxy {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		c, err := f.loam.DialPort(ctx, row.Sandbox, port)
		if err != nil {
			return nil, err
		}
		if port == EnvdPort {
			if err := f.ensureInit(ctx, row); err != nil {
				c.Close()
				return nil, err
			}
		}
		return c, nil
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "sandbox"
			pr.Out.Host = pr.In.Host
		},
		Transport:     &http.Transport{DisableKeepAlives: true, DisableCompression: true, DialContext: dial},
		FlushInterval: -1,
		// A stream cut off by a pause is reported here; serveStream ends it.
		ErrorLog: slog.NewLogLogger(f.log.Handler(), slog.LevelDebug),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			f.log.Debug("sandbox port unreachable", "sandbox", row.Sandbox, "port", port, "err", err)
			switch c := codeOf(err); {
			case errors.Is(err, apiserver.ErrPortClosed):
				writeJSON(w, http.StatusBadGateway, map[string]any{"sandboxId": row.Sandbox, "port": port, "code": http.StatusBadGateway,
					"message": "The sandbox is running but port is not open"})
			case c == api.CodeCapacity:
				f.fail(w, row.Sandbox, err)
			default:
				sandboxNotFound(w, row.Sandbox)
			}
		},
	}
}

// Connect streams (docs/E2B.md, "envd"): a server stream is a series of
// envelopes, [flags u8][length u32 BE][message], and ends with one flagged
// end-stream (0x02) whose message is {} or {"error": ...}.
const (
	connectStream    = "application/connect+"
	connectEndStream = 0x02
)

// serveStream runs proxy and makes sure a Connect stream it relays ends with
// an end-stream envelope even when the guest goes away mid-stream (a pause
// ends every port stream of the sandbox): it then ends it with an
// "unavailable" error, as hosted E2B's proxy does, rather than letting the
// client see a stream cut off. A response that is not a Connect stream, or one
// cut inside an envelope, is left cut off.
func serveStream(w http.ResponseWriter, r *http.Request, proxy http.Handler, message string) {
	sw := &streamWriter{ResponseWriter: w}
	defer func() {
		rec := recover()
		if rec != nil {
			if err, ok := rec.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}
		}
		if !sw.endStream(message) && rec != nil {
			panic(rec)
		}
	}()
	proxy.ServeHTTP(sw, r)
}

// streamWriter follows the envelopes of a Connect stream as they are relayed.
type streamWriter struct {
	http.ResponseWriter
	stream bool // a 200 with a Connect stream content type
	env    envelopes
}

// Unwrap lets http.ResponseController reach Flush and the full-duplex switch.
func (w *streamWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *streamWriter) WriteHeader(status int) {
	w.stream = status == http.StatusOK && strings.HasPrefix(w.Header().Get("Content-Type"), connectStream)
	w.ResponseWriter.WriteHeader(status)
}

func (w *streamWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	if w.stream {
		w.env.advance(b[:n])
	}
	return n, err
}

func (w *streamWriter) Flush() { http.NewResponseController(w.ResponseWriter).Flush() }

// endStream closes off a Connect stream that has not ended, at an envelope
// boundary. It reports whether the response is now complete: true for a
// stream that ended by itself or was ended here.
func (w *streamWriter) endStream(message string) bool {
	if !w.stream {
		return false
	}
	if w.env.ended {
		return true
	}
	if !w.env.atBoundary() {
		return false
	}
	w.ResponseWriter.Write(endFrame(message))
	http.NewResponseController(w.ResponseWriter).Flush()
	return true
}

// endFrame is an end-stream envelope carrying an "unavailable" error, the
// one Connect clients take for the peer going away.
func endFrame(message string) []byte {
	msg, _ := json.Marshal(map[string]any{"error": map[string]string{"code": "unavailable", "message": message}})
	b := make([]byte, 5, 5+len(msg))
	b[0] = connectEndStream
	binary.BigEndian.PutUint32(b[1:], uint32(len(msg)))
	return append(b, msg...)
}

// envelopes tracks where a stream of envelopes is: inside a header, inside a
// message, or between two, and whether the end-stream one went by.
type envelopes struct {
	hdr   [5]byte
	hdrN  int
	left  int
	flags byte
	ended bool
}

func (e *envelopes) advance(b []byte) {
	for len(b) > 0 {
		if e.left > 0 {
			n := min(len(b), e.left)
			e.left -= n
			b = b[n:]
			if e.left == 0 && e.flags&connectEndStream != 0 {
				e.ended = true
			}
			continue
		}
		n := copy(e.hdr[e.hdrN:], b)
		e.hdrN += n
		b = b[n:]
		if e.hdrN == len(e.hdr) {
			e.hdrN = 0
			e.flags = e.hdr[0]
			e.left = int(binary.BigEndian.Uint32(e.hdr[1:]))
			if e.left == 0 && e.flags&connectEndStream != 0 {
				e.ended = true
			}
		}
	}
}

func (e *envelopes) atBoundary() bool { return e.hdrN == 0 && e.left == 0 }

// envdError is envd answering with a status that is not a success.
type envdError struct {
	status int
	body   string
}

func (e *envdError) Error() string { return fmt.Sprintf("envd answered %d: %s", e.status, e.body) }

// envdCall makes one HTTP request to envd in the sandbox, over loam's port
// stream.
func (f *Frontend) envdCall(ctx context.Context, id, method, path, token string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://envd"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Access-Token", token)
	}
	t := &http.Transport{DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return f.loam.DialPort(ctx, id, EnvdPort) }}
	resp, err := t.RoundTrip(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &envdError{resp.StatusCode, strings.TrimSpace(string(b))}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// initRetryWait is how long /init waits between attempts while envd starts.
const initRetryWait = 50 * time.Millisecond

// initRequest is envd's POST /init body (docs/E2B.md, "envd's /init").
type initRequest struct {
	AccessToken    string            `json:"accessToken"`
	EnvVars        map[string]string `json:"envVars,omitempty"`
	DefaultUser    string            `json:"defaultUser"`
	DefaultWorkdir string            `json:"defaultWorkdir"`
	Timestamp      string            `json:"timestamp"`
}

// forgetInit drops what is known of a sandbox's envd, when it is deleted.
func (f *Frontend) forgetInit(id string) {
	f.mu.Lock()
	delete(f.inited, id)
	f.mu.Unlock()
	f.initLocks.Delete(id)
}

// ensureInit prepares envd in the sandbox's current boot, before anything
// else of E2B's reaches it: POST /init with the sandbox's access token, env
// vars, default user and the time. A fresh boot (a create, a wake from a stop
// or a disk-only pause, a restore, a move without memory, a nodelet that
// restarted under a running VM) starts envd with no token, answering anyone
// who reaches its port, so this has to happen first; a resume from a memory
// snapshot finds envd as it was, token and all. Which of the two a wake was
// is the VM's booted_at (loam's BootedAt), which only a fresh boot moves: so
// /init runs once per boot, on the first E2B call that finds a new one. A
// second /init in a boot (loam-api restarted and forgot) carries the same
// token and is harmless.
func (f *Frontend) ensureInit(ctx context.Context, row db.E2BSandbox) error {
	booted, err := f.loam.BootedAt(ctx, row.Sandbox)
	if err != nil {
		return err
	}
	if f.initedIn(row.Sandbox, booted) {
		return nil
	}
	defer lock(&f.initLocks, row.Sandbox)()
	if f.initedIn(row.Sandbox, booted) {
		return nil
	}
	sb, err := f.sandboxOf(ctx, row.Project, row.Sandbox)
	if err != nil {
		return err
	}
	env, err := f.initEnv(ctx, row, sb)
	if err != nil {
		return err
	}
	token, err := f.token(row)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, f.opts.InitWait)
	defer cancel()
	start := time.Now()
	user, workdir := f.defaultsOf(row.Template)
	for {
		req := initRequest{AccessToken: token, EnvVars: env, DefaultUser: user,
			DefaultWorkdir: workdir, Timestamp: f.now().Format(time.RFC3339Nano)}
		err = f.envdCall(ctx, row.Sandbox, http.MethodPost, "/init", "", req, nil)
		if err == nil {
			break
		}
		var ee *envdError
		if errors.As(err, &ee) && ee.status == http.StatusUnauthorized {
			// Something in the guest got to envd first and set a token of its own:
			// whatever answers on 49983 now is not ours to hand the SDK.
			return fmt.Errorf("envd refused /init (another token was set first): %w", err)
		}
		if c := codeOf(err); c != "" && c != api.CodeSandboxUnavailable {
			return err // the sandbox is gone, or can't be woken: no use waiting
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("envd did not take /init in %s: %w", f.opts.InitWait, err)
		case <-time.After(initRetryWait):
		}
	}
	// The boot is the one that /init went to, unless the VM booted again in
	// between, which the next call finds.
	f.mu.Lock()
	f.inited[row.Sandbox] = booted
	f.mu.Unlock()
	f.log.Info("envd initialized", "sandbox", row.Sandbox, "booted_at", booted, "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// initedIn is whether the sandbox's envd was initialized in the boot that
// began at booted. A VM that isn't running (zero) never is.
func (f *Frontend) initedIn(id string, booted time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.inited[id]
	return ok && !booted.IsZero() && at.Equal(booted)
}

// Where the broker CA is in the e2b image (docs/AGENT.md, "The broker CA"):
// envd gives the processes it starts only PATH from its own environment, so
// what the agent puts in every process's environment has to come by /init.
const (
	caFile   = "/etc/loam/ca.pem"
	caBundle = "/etc/ssl/certs/ca-certificates.crt"
)

// initEnv is the environment envd gives every process it starts: what hosted
// envd puts there (envd main.go, and host/mmds.go from the orchestrator's
// metadata), which -isnotfc leaves out or false; then the sandbox's env, with
// its secrets' placeholders and, when it has secrets, the broker CA's
// variables, as loam's agent gives its own processes.
func (f *Frontend) initEnv(ctx context.Context, row db.E2BSandbox, sb api.Sandbox) (map[string]string, error) {
	env := map[string]string{"E2B_SANDBOX": "true", "E2B_SANDBOX_ID": row.Sandbox, "E2B_TEMPLATE_ID": row.Template}
	if len(sb.Secrets) > 0 {
		var list api.SecretList
		if err := f.call(ctx, row.Project, http.MethodGet, "/v1/secrets", nil, &list); err != nil {
			return nil, err
		}
		for _, s := range list.Secrets {
			for _, name := range sb.Secrets {
				if s.Name == name {
					env[name] = s.Placeholder
				}
			}
		}
		env["NODE_EXTRA_CA_CERTS"] = caFile
		for _, k := range []string{"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CARGO_HTTP_CAINFO"} {
			env[k] = caBundle
		}
		env["UV_NATIVE_TLS"] = "1"
	}
	maps.Copy(env, sb.Env)
	return env, nil
}
