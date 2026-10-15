package e2b

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/arugula-salad/loam/internal/apiserver"
	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/pkg/api"
)

// The control plane: E2B's REST API, as the SDKs use it (docs/E2B.md,
// "Control plane"). Shapes, status codes and messages are hosted E2B's, from
// its OpenAPI spec, its API server's handlers and the golden traces in
// sandpit's e2e/providers/e2b; what each call does is a call on loam's API.

func (f *Frontend) controlPlane() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "Health check successful")
	})
	mux.HandleFunc("POST /v2/sandboxes", f.auth(func(w http.ResponseWriter, r *http.Request, p string) { f.create(w, r, p, defaultTimeout) }))
	mux.HandleFunc("POST /sandboxes", f.auth(func(w http.ResponseWriter, r *http.Request, p string) { f.create(w, r, p, defaultTimeoutV1) }))
	mux.HandleFunc("GET /v2/sandboxes", f.auth(f.list))
	mux.HandleFunc("GET /sandboxes", f.auth(f.listV1))
	mux.HandleFunc("GET /sandboxes/{id}", f.auth(f.sandbox(f.get)))
	mux.HandleFunc("DELETE /sandboxes/{id}", f.auth(f.sandbox(f.kill)))
	mux.HandleFunc("POST /sandboxes/{id}/timeout", f.auth(f.sandbox(f.setTimeout)))
	mux.HandleFunc("POST /sandboxes/{id}/refreshes", f.auth(f.sandbox(f.refresh)))
	mux.HandleFunc("POST /sandboxes/{id}/pause", f.auth(f.sandbox(f.pause)))
	mux.HandleFunc("POST /v2/sandboxes/{id}/connect", f.auth(f.sandbox(f.connect)))
	// The unversioned path is the same call: older clients, and E2B's own REST
	// spec before v2, name it so (Fountain's adapter among them).
	mux.HandleFunc("POST /sandboxes/{id}/connect", f.auth(f.sandbox(f.connect)))
	mux.HandleFunc("POST /sandboxes/{id}/resume", f.auth(f.sandbox(f.resume)))
	mux.HandleFunc("PUT /sandboxes/{id}/network", f.auth(f.sandbox(f.updateNetwork)))
	mux.HandleFunc("GET /sandboxes/{id}/metrics", f.auth(f.sandbox(f.metrics)))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "Not found: this E2B-compatible server does not implement "+r.Method+" "+r.URL.Path)
	})
	return mux
}

// writeErr is E2B's error body, {"code": <status>, "message": ...}. The SDKs
// map errors by status and show the message.
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"code": status, "message": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// auth checks X-API-Key against loam's keys: the key's project is whose
// sandboxes the call sees, as on loam's own API.
func (f *Frontend) auth(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		project, err := "", errors.New("no key")
		if key != "" {
			project, err = f.loam.ProjectOfKey(r.Context(), key)
		}
		if err != nil {
			if c := codeOf(err); c != "" && c != api.CodeUnauthorized {
				f.fail(w, "", err)
				return
			}
			writeErr(w, http.StatusUnauthorized, "Invalid API key: use a loam project key (loam_...) as E2B_API_KEY")
			return
		}
		h(w, r, project)
	}
}

// notFound is hosted E2B's answer for an ID that is not a sandbox of ours.
func notFound(w http.ResponseWriter, id string) {
	writeErr(w, http.StatusNotFound, fmt.Sprintf("Sandbox %q doesn't exist or you don't have access to it", id))
}

// fail answers a refusal of loam's in E2B's terms. The SDKs map by status:
// 401 AuthenticationError, 404 NotFound, 429 RateLimitError (retried with an
// integer Retry-After), anything else a SandboxError with the message.
func (f *Frontend) fail(w http.ResponseWriter, id string, err error) {
	var e *apiserver.Error
	if !errors.As(err, &e) {
		if errors.Is(err, context.Canceled) {
			return
		}
		f.log.Error("e2b call failed", "sandbox", id, "err", err)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	switch e.Code {
	case api.CodeNotFound:
		if id != "" {
			notFound(w, id)
			return
		}
		writeErr(w, http.StatusNotFound, e.Message)
	case api.CodeUnauthorized:
		writeErr(w, http.StatusUnauthorized, e.Message)
	case api.CodeCapacity:
		// No node has room: hosted's answer for too many sandboxes, which the
		// SDKs retry after Retry-After.
		if e.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
		}
		writeErr(w, http.StatusTooManyRequests, "No capacity for the sandbox now: "+e.Message)
	default:
		writeErr(w, e.Status, e.Message)
	}
}

// sandbox looks up {id} as an E2B sandbox of the caller's project: one the
// adapter made. Any other sandbox, the project's own loam sandboxes
// included, isn't one, even by ID.
func (f *Frontend) sandbox(h func(http.ResponseWriter, *http.Request, string, db.E2BSandbox)) func(http.ResponseWriter, *http.Request, string) {
	return func(w http.ResponseWriter, r *http.Request, project string) {
		id := r.PathValue("id")
		row, err := f.store.E2B(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) || (err == nil && row.Project != project) {
			notFound(w, id)
			return
		} else if err != nil {
			f.fail(w, id, err)
			return
		}
		h(w, r, project, row)
	}
}

// readBody decodes an optional JSON body into v: an empty body leaves v as it is.
func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err == nil && len(strings.TrimSpace(string(b))) > 0 {
		err = json.Unmarshal(b, v)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Error when parsing request: "+err.Error())
		return false
	}
	return true
}

// timeoutOf is a request's timeout in seconds, or def; ok is false (and the
// answer written) for one out of range. A deadline must be in the future, so
// the shortest is a second.
func (f *Frontend) timeoutOf(w http.ResponseWriter, secs *int, def time.Duration) (time.Duration, bool) {
	if secs == nil {
		return def, true
	}
	d := time.Duration(*secs) * time.Second
	if *secs < 1 {
		writeErr(w, http.StatusBadRequest, "Timeout must be at least 1 second")
		return 0, false
	}
	if d > f.opts.MaxTimeout {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("Timeout cannot be greater than %s", f.opts.MaxTimeout))
		return 0, false
	}
	return d, true
}

// The wire shapes (OpenAPI components.schemas).

// sandboxJSON is Sandbox: the answer to a create or a connect.
type sandboxJSON struct {
	Alias           string `json:"alias,omitempty"`
	ClientID        string `json:"clientID"`
	Domain          string `json:"domain,omitempty"`
	EnvdAccessToken string `json:"envdAccessToken,omitempty"`
	EnvdVersion     string `json:"envdVersion"`
	SandboxID       string `json:"sandboxID"`
	TemplateID      string `json:"templateID"`
}

// detailJSON is SandboxDetail (GET /sandboxes/{id}) and, without the token and
// the lifecycle, ListedSandbox (GET /v2/sandboxes).
type detailJSON struct {
	Alias           string            `json:"alias,omitempty"`
	ClientID        string            `json:"clientID"`
	CPUCount        int               `json:"cpuCount"`
	DiskSizeMB      int               `json:"diskSizeMB"`
	Domain          string            `json:"domain,omitempty"`
	EndAt           string            `json:"endAt"`
	EnvdAccessToken string            `json:"envdAccessToken,omitempty"`
	EnvdVersion     string            `json:"envdVersion"`
	Lifecycle       *lifecycleJSON    `json:"lifecycle,omitempty"`
	MemoryMB        int               `json:"memoryMB"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	SandboxID       string            `json:"sandboxID"`
	StartedAt       string            `json:"startedAt"`
	State           string            `json:"state"`
	TemplateID      string            `json:"templateID"`
	// VolumeMounts is [] for a running sandbox and absent for a paused one, as hosted.
	VolumeMounts *[]any `json:"volumeMounts,omitempty"`
}

type lifecycleJSON struct {
	AutoResume bool   `json:"autoResume"`
	OnTimeout  string `json:"onTimeout"`
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// paused is the sandbox's E2B state: paused through the API, or by its
// timeout passing with autoPause (which loam's deadline acts on, pausing the
// sandbox, without telling the adapter). A sandbox E2B calls running may be
// asleep in loam (its node drained, say): its traffic wakes it.
func paused(row db.E2BSandbox, now time.Time) bool {
	return row.Paused || (row.AutoPause && !now.Before(row.EndAt))
}

func stateOf(row db.E2BSandbox, now time.Time) string {
	if paused(row, now) {
		return "paused"
	}
	return "running"
}

// deadlineAction is what loam does at the timeout: delete, or with autoPause
// pause (stop, a disk-only pause, without its memory).
func deadlineAction(row db.E2BSandbox) string {
	switch {
	case !row.AutoPause:
		return api.ActionDelete
	case row.KeepMemory:
		return api.ActionPause
	}
	return api.ActionStop
}

// metadataOf is an E2B sandbox's metadata: its labels under LabelPrefix.
func metadataOf(labels map[string]string) map[string]string {
	var out map[string]string
	for k, v := range labels {
		if name, ok := strings.CutPrefix(k, LabelPrefix); ok {
			if out == nil {
				out = map[string]string{}
			}
			out[name] = v
		}
	}
	return out
}

func (f *Frontend) sandboxJSON(row db.E2BSandbox, token string) sandboxJSON {
	return sandboxJSON{Alias: row.Template, ClientID: clientID, Domain: f.opts.Domain, EnvdAccessToken: token,
		EnvdVersion: EnvdVersion, SandboxID: row.Sandbox, TemplateID: row.Template}
}

// detailOf is GET /sandboxes/{id}'s answer, with token, or a listing's, without.
func (f *Frontend) detailOf(sb api.Sandbox, row db.E2BSandbox, token string, now time.Time, listed bool) detailJSON {
	d := detailJSON{Alias: row.Template, ClientID: clientID, CPUCount: sb.Resources.CPUs, DiskSizeMB: sb.Resources.DiskGiB * 1024,
		Domain: f.opts.Domain, EndAt: stamp(row.EndAt), EnvdVersion: EnvdVersion, MemoryMB: sb.Resources.MemoryMiB,
		Metadata: metadataOf(sb.Labels), SandboxID: row.Sandbox, StartedAt: stamp(row.StartedAt), State: stateOf(row, now),
		TemplateID: row.Template}
	if d.State == "running" {
		d.VolumeMounts = &[]any{}
	}
	if !listed {
		d.EnvdAccessToken = token
		onTimeout := "kill"
		if row.AutoPause {
			onTimeout = "pause"
		}
		d.Lifecycle = &lifecycleJSON{AutoResume: row.AutoResume, OnTimeout: onTimeout}
	}
	return d
}

// createReq is NewSandboxV2, the parts of it this server honours. Anything
// else that isn't null is refused (unknown): an option nobody honours is
// worse than an error.
type createReq struct {
	TemplateID      string            `json:"templateID"`
	Timeout         *int              `json:"timeout"`
	Metadata        map[string]string `json:"metadata"`
	EnvVars         map[string]string `json:"envVars"`
	AutoPause       *bool             `json:"autoPause"`
	AutoPauseMemory *bool             `json:"autoPauseMemory"`
	AutoResume      *struct {
		Enabled bool `json:"enabled"`
	} `json:"autoResume"`
	// Secure is envd's access token, which every sandbox here has.
	Secure              *bool       `json:"secure"`
	AllowInternetAccess *bool       `json:"allow_internet_access"`
	Network             *networkReq `json:"network"`

	unknown []string
}

func (c *createReq) UnmarshalJSON(b []byte) error {
	type plain createReq
	if err := json.Unmarshal(b, (*plain)(c)); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	c.unknown = unknownFields(all, "templateID", "timeout", "metadata", "envVars", "autoPause", "autoPauseMemory",
		"autoResume", "secure", "allow_internet_access", "network")
	sort.Strings(c.unknown)
	return nil
}

// templateOf resolves a template name or ID to the E2B template's name and
// the loam template it is made from: base, loam's e2b template (E2B's base
// image with envd, images/e2b), or one of Options.Templates. Only the default
// tag exists; no template here has builds to tag.
func (f *Frontend) templateOf(ref string) (id, loam string, ok bool) {
	if ref == "" {
		ref = baseAlias
	}
	name, tag, _ := strings.Cut(ref, ":")
	if tag != "" && tag != "default" {
		return "", "", false
	}
	if name == baseAlias {
		return baseAlias, f.opts.Template, true
	}
	if t, ok := f.opts.Templates[name]; ok {
		return name, t.Loam, true
	}
	return "", "", false
}

// newAccessToken is a fresh envd access token.
func newAccessToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (f *Frontend) create(w http.ResponseWriter, r *http.Request, project string, def time.Duration) {
	var req createReq
	if !readBody(w, r, &req) {
		return
	}
	tmpl, loamTmpl, ok := f.templateOf(req.TemplateID)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("template '%s' not found", req.TemplateID))
		return
	}
	if len(req.unknown) > 0 {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("%s: not supported by this server", strings.Join(req.unknown, ", ")))
		return
	}
	if req.Secure != nil && !*req.Secure {
		writeErr(w, http.StatusBadRequest, "secure: false is not supported by this server: every sandbox's envd has an access token")
		return
	}
	autoPause := req.AutoPause != nil && *req.AutoPause
	keepMemory := req.AutoPauseMemory == nil || *req.AutoPauseMemory
	autoResume := req.AutoResume != nil && req.AutoResume.Enabled
	switch {
	case req.AutoPauseMemory != nil && !autoPause:
		writeErr(w, http.StatusBadRequest, "autoPauseMemory is only allowed with autoPause")
		return
	case autoResume && !autoPause:
		writeErr(w, http.StatusBadRequest, "autoResume can only be enabled with autoPause")
		return
	case autoResume && !keepMemory:
		writeErr(w, http.StatusBadRequest, "autoResume can't be enabled with autoPauseMemory false")
		return
	}
	rules, err := egressOf(req.AllowInternetAccess, req.Network)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	timeout, ok := f.timeoutOf(w, req.Timeout, def)
	if !ok {
		return
	}
	// envd's token is kept sealed under loam-api's secret key, or not at all.
	if !f.loam.CanSeal() {
		writeErr(w, http.StatusNotImplemented, "this deployment has no secret key (LOAM_SECRET_KEY), so it can't keep envd's access tokens: E2B sandboxes are unsupported")
		return
	}
	now := f.now()
	token := newAccessToken()
	row := db.E2BSandbox{Project: project, Template: tmpl, TimeoutS: int(timeout / time.Second),
		StartedAt: now, EndAt: now.Add(timeout), AutoPause: autoPause, KeepMemory: keepMemory, AutoResume: autoResume}
	labels := map[string]string{}
	for k, v := range req.Metadata {
		labels[LabelPrefix+k] = v
	}
	// E2B sandboxes are never paused for being idle: they run until their
	// timeout, and then die (or pause, with autoPause).
	create := api.CreateSandbox{Template: loamTmpl, Env: req.EnvVars, Labels: labels,
		Idle: &api.Idle{Action: api.ActionNone}, Deadline: &api.Deadline{At: row.EndAt, Action: deadlineAction(row)}}
	if rules != nil {
		create.Network = &api.Network{Egress: rules}
	}
	var sb api.Sandbox
	if err := f.call(r.Context(), project, http.MethodPost, "/v1/sandboxes", create, &sb); err != nil {
		f.fail(w, "", err)
		return
	}
	row.Sandbox = sb.ID
	// From here a failure takes the sandbox away again: an E2B create either
	// answers a sandbox whose envd has its token, or leaves nothing.
	ctx := context.WithoutCancel(r.Context())
	sealed, err := f.loam.Seal(sb.ID, []byte(token))
	if err == nil {
		row.SealedToken = sealed
		err = f.store.CreateE2B(ctx, row)
	}
	if err != nil {
		f.call(ctx, project, http.MethodDelete, "/v1/sandboxes/"+sb.ID, nil, nil)
		f.fail(w, "", err)
		return
	}
	if err := f.ensureInit(ctx, row); err != nil {
		f.log.Error("envd did not take /init; the sandbox is deleted", "sandbox", sb.ID, "err", err)
		f.call(ctx, project, http.MethodDelete, "/v1/sandboxes/"+sb.ID, nil, nil)
		writeErr(w, http.StatusInternalServerError, "Failed to start sandbox: "+err.Error())
		return
	}
	f.log.Info("sandbox created", "sandbox", sb.ID, "project", project, "template", tmpl, "timeout", timeout)
	writeJSON(w, http.StatusCreated, f.sandboxJSON(row, token))
}

func (f *Frontend) get(w http.ResponseWriter, r *http.Request, project string, row db.E2BSandbox) {
	sb, err := f.sandboxOf(r.Context(), project, row.Sandbox)
	if err != nil {
		f.fail(w, row.Sandbox, err)
		return
	}
	token, err := f.token(row)
	if err != nil {
		f.fail(w, row.Sandbox, err)
		return
	}
	writeJSON(w, http.StatusOK, f.detailOf(sb, row, token, f.now(), false))
}

func (f *Frontend) kill(w http.ResponseWriter, r *http.Request, project string, row db.E2BSandbox) {
	defer lock(&f.locks, row.Sandbox)()
	if err := f.call(r.Context(), project, http.MethodDelete, "/v1/sandboxes/"+row.Sandbox, nil, nil); err != nil {
		f.fail(w, row.Sandbox, err)
		return
	}
	f.locks.Delete(row.Sandbox)
	f.forgetInit(row.Sandbox)
	f.log.Info("sandbox killed", "sandbox", row.Sandbox)
	w.WriteHeader(http.StatusNoContent)
}

// current is the sandbox's record as it is now, under its lock.
func (f *Frontend) current(ctx context.Context, w http.ResponseWriter, row db.E2BSandbox) (db.E2BSandbox, bool) {
	cur, err := f.store.E2B(ctx, row.Sandbox)
	if err != nil {
		f.fail(w, row.Sandbox, apiNotFound(err))
		return cur, false
	}
	return cur, true
}

// apiNotFound is a missing record as loam's not_found.
func apiNotFound(err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return &apiserver.Error{Status: http.StatusNotFound, Code: api.CodeNotFound, Message: "sandbox not found"}
	}
	return err
}

// setDeadline moves the sandbox's end to end: loam's deadline, which is what
// acts on it, then the record. change, if not nil, changes the record too.
func (f *Frontend) setDeadline(ctx context.Context, row db.E2BSandbox, end time.Time, change func(*db.E2BSandbox)) (db.E2BSandbox, error) {
	next := row
	next.EndAt = end
	if change != nil {
		change(&next)
	}
	patch := map[string]any{"deadline": api.Deadline{At: end, Action: deadlineAction(next)}}
	if err := f.call(ctx, row.Project, http.MethodPatch, "/v1/sandboxes/"+row.Sandbox, patch, nil); err != nil {
		return row, err
	}
	return f.store.UpdateE2B(ctx, row.Sandbox, func(e *db.E2BSandbox) error {
		e.EndAt = end
		if change != nil {
			change(e)
		}
		return nil
	})
}

func (f *Frontend) setTimeout(w http.ResponseWriter, r *http.Request, project string, row db.E2BSandbox) {
	var req struct {
		Timeout *int `json:"timeout"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if req.Timeout == nil {
		writeErr(w, http.StatusBadRequest, "Error when parsing request: timeout is required")
		return
	}
	timeout, ok := f.timeoutOf(w, req.Timeout, 0)
	if !ok {
		return
	}
	defer lock(&f.locks, row.Sandbox)()
	row, ok = f.current(r.Context(), w, row)
	if !ok {
		return
	}
	if paused(row, f.now()) {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("Sandbox %q is paused; resume it with connect first", row.Sandbox))
		return
	}
	if _, err := f.setDeadline(r.Context(), row, f.now().Add(timeout), nil); err != nil {
		f.fail(w, row.Sandbox, apiNotFound(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// refresh is the deprecated keep-alive: it extends the timeout, never shortens it.
func (f *Frontend) refresh(w http.ResponseWriter, r *http.Request, project string, row db.E2BSandbox) {
	var req struct {
		Duration *int `json:"duration"`
	}
	if !readBody(w, r, &req) {
		return
	}
	d, ok := f.timeoutOf(w, req.Duration, defaultTimeout)
	if !ok {
		return
	}
	defer lock(&f.locks, row.Sandbox)()
	// Decide on the record as it is now: a pause that landed while this
	// waited for the lock cleared the deadline, and must not get one back.
	row, ok = f.current(r.Context(), w, row)
	if !ok {
		return
	}
	if end := f.now().Add(d); !paused(row, f.now()) && end.After(row.EndAt) {
		if _, err := f.setDeadline(r.Context(), row, end, nil); err != nil {
			f.fail(w, row.Sandbox, apiNotFound(err))
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// pause pauses the sandbox: by default a memory snapshot, from which a
// connect resumes it with its processes (loam's pause); with {"memory":
// false} only its disk, so a connect boots it afresh (loam's stop). A paused
// sandbox has no deadline.
func (f *Frontend) pause(w http.ResponseWriter, r *http.Request, project string, row db.E2BSandbox) {
	var req struct {
		Memory *bool `json:"memory"`
	}
	if !readBody(w, r, &req) {
		return
	}
	defer lock(&f.locks, row.Sandbox)()
	row, ok := f.current(r.Context(), w, row)
	if !ok {
		return
	}
	if paused(row, f.now()) {
		writeErr(w, http.StatusConflict, fmt.Sprintf("Error pausing sandbox - sandbox '%s' is already paused", row.Sandbox))
		return
	}
	ctx := context.WithoutCancel(r.Context())
	// The deadline goes first: a sandbox on its way to a pause must not be
	// killed by a timeout that passes while it pauses.
	if err := f.call(ctx, project, http.MethodPatch, "/v1/sandboxes/"+row.Sandbox, map[string]any{"deadline": nil}, nil); err != nil {
		f.fail(w, row.Sandbox, err)
		return
	}
	// Then it is marked paused, before the VM is paused: traffic that arrives
	// in between must be refused, not wake the VM back up behind the pause
	// (it would then run with no deadline and no idle rule, for good).
	now := f.now()
	if _, err := f.store.UpdateE2B(ctx, row.Sandbox, func(e *db.E2BSandbox) error { e.Paused, e.EndAt = true, now; return nil }); err != nil {
		f.fail(w, row.Sandbox, apiNotFound(err))
		return
	}
	err := f.sleep(ctx, project, row.Sandbox, req.Memory == nil || *req.Memory)
	if err != nil {
		f.log.Error("pause failed", "sandbox", row.Sandbox, "err", err)
		// Undo both: the sandbox is still running, with its old timeout.
		f.store.UpdateE2B(ctx, row.Sandbox, func(e *db.E2BSandbox) error { e.Paused, e.EndAt = false, row.EndAt; return nil })
		f.call(ctx, project, http.MethodPatch, "/v1/sandboxes/"+row.Sandbox,
			map[string]any{"deadline": api.Deadline{At: row.EndAt, Action: deadlineAction(row)}}, nil)
		f.fail(w, row.Sandbox, err)
		return
	}
	f.log.Info("sandbox paused", "sandbox", row.Sandbox, "memory", req.Memory == nil || *req.Memory)
	w.WriteHeader(http.StatusNoContent)
}

// sleep pauses (memory) or stops the loam sandbox. One loam has asleep
// already is left as it is, unless its memory is to go.
func (f *Frontend) sleep(ctx context.Context, project, id string, memory bool) error {
	sb, err := f.sandboxOf(ctx, project, id)
	if err != nil {
		return err
	}
	switch {
	case sb.State == api.StateStopped, memory && sb.State == api.StatePaused:
		return nil
	case memory:
		return f.call(ctx, project, http.MethodPost, "/v1/sandboxes/"+id+"/pause", nil, nil)
	}
	return f.call(ctx, project, http.MethodPost, "/v1/sandboxes/"+id+"/stop", nil, nil)
}

// connect resumes a paused sandbox (201), with its timeout reset to the one
// asked for (default 300 s) as hosted does, or extends a running one's (200):
// connecting never shortens a running sandbox's timeout.
func (f *Frontend) connect(w http.ResponseWriter, r *http.Request, project string, row db.E2BSandbox) {
	var req struct {
		Timeout *int `json:"timeout"`
	}
	if !readBody(w, r, &req) {
		return
	}
	timeout, ok := f.timeoutOf(w, req.Timeout, defaultTimeout)
	if !ok {
		return
	}
	f.resumeOrExtend(w, r, row, timeout, false)
}

// resume is the deprecated POST /sandboxes/{id}/resume: connect, but 409 for a
// sandbox that is already running.
func (f *Frontend) resume(w http.ResponseWriter, r *http.Request, project string, row db.E2BSandbox) {
	var req struct {
		Timeout *int `json:"timeout"`
	}
	if !readBody(w, r, &req) {
		return
	}
	timeout, ok := f.timeoutOf(w, req.Timeout, defaultTimeoutV1)
	if !ok {
		return
	}
	f.resumeOrExtend(w, r, row, timeout, true)
}

func (f *Frontend) resumeOrExtend(w http.ResponseWriter, r *http.Request, row db.E2BSandbox, timeout time.Duration, onlyPaused bool) {
	defer lock(&f.locks, row.Sandbox)()
	row, ok := f.current(r.Context(), w, row)
	if !ok {
		return
	}
	token, err := f.token(row)
	if err != nil {
		f.fail(w, row.Sandbox, err)
		return
	}
	now := f.now()
	if !paused(row, now) {
		if onlyPaused {
			writeErr(w, http.StatusConflict, fmt.Sprintf("Sandbox %s is already running", row.Sandbox))
			return
		}
		if end := now.Add(timeout); end.After(row.EndAt) {
			if row, err = f.setDeadline(r.Context(), row, end, nil); err != nil {
				f.fail(w, row.Sandbox, apiNotFound(err))
				return
			}
		}
		writeJSON(w, http.StatusOK, f.sandboxJSON(row, token))
		return
	}
	row, err = f.wake(context.WithoutCancel(r.Context()), row, timeout)
	if err != nil {
		f.fail(w, row.Sandbox, err)
		return
	}
	writeJSON(w, http.StatusCreated, f.sandboxJSON(row, token))
}

// wake resumes a paused sandbox, under its lock: loam's wake (its memory
// back, or a fresh boot on its disk), envd's /init if the boot was fresh, and
// a new timeout from now.
func (f *Frontend) wake(ctx context.Context, row db.E2BSandbox, timeout time.Duration) (db.E2BSandbox, error) {
	if err := f.call(ctx, row.Project, http.MethodPost, "/v1/sandboxes/"+row.Sandbox+"/wake", nil, nil); err != nil {
		return row, err
	}
	if err := f.ensureInit(ctx, row); err != nil {
		return row, fmt.Errorf("envd did not take /init after the resume: %w", err)
	}
	now := f.now()
	row, err := f.setDeadline(ctx, row, now.Add(timeout), func(e *db.E2BSandbox) { e.Paused, e.StartedAt = false, now })
	if err != nil {
		return row, apiNotFound(err)
	}
	f.log.Info("sandbox resumed", "sandbox", row.Sandbox, "timeout", timeout)
	return row, nil
}

// updateNetwork is PUT /sandboxes/{id}/network: new egress options, as at
// create, which loam applies at once to a running sandbox (closing what they
// no longer allow) and at the next wake to a paused one.
func (f *Frontend) updateNetwork(w http.ResponseWriter, r *http.Request, project string, row db.E2BSandbox) {
	var req struct {
		AllowInternetAccess *bool `json:"allow_internet_access"`
		networkReq
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err == nil {
		err = json.Unmarshal(b, &req.networkReq)
	}
	if err == nil {
		var all map[string]json.RawMessage
		if err = json.Unmarshal(b, &all); err == nil {
			if raw, ok := all["allow_internet_access"]; ok {
				err = json.Unmarshal(raw, &req.AllowInternetAccess)
			}
			req.unknown = unknownFields(all, "allowOut", "denyOut", "allow_internet_access")
		}
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Error when parsing request: "+err.Error())
		return
	}
	rules, err := egressOf(req.AllowInternetAccess, &req.networkReq)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if rules == nil {
		rules = []api.Rule{}
	}
	defer lock(&f.locks, row.Sandbox)()
	patch := map[string]any{"network": api.Network{Egress: rules}}
	if err := f.call(r.Context(), project, http.MethodPatch, "/v1/sandboxes/"+row.Sandbox, patch, nil); err != nil {
		f.fail(w, row.Sandbox, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// metricJSON is SandboxMetric.
type metricJSON struct {
	CPUCount      int     `json:"cpuCount"`
	CPUUsedPct    float64 `json:"cpuUsedPct"`
	DiskTotal     int64   `json:"diskTotal"`
	DiskUsed      int64   `json:"diskUsed"`
	MemCache      int64   `json:"memCache"`
	MemTotal      int64   `json:"memTotal"`
	MemUsed       int64   `json:"memUsed"`
	Timestamp     string  `json:"timestamp"`
	TimestampUnix int64   `json:"timestampUnix"`
}

// metrics is one sample, envd's own /metrics, for a sandbox that is awake;
// [] otherwise (hosted answers [] too until its first sample). There is no
// history: start and end are accepted and ignored. Asking doesn't wake it.
func (f *Frontend) metrics(w http.ResponseWriter, r *http.Request, project string, row db.E2BSandbox) {
	out := []metricJSON{}
	if !paused(row, f.now()) {
		if sb, err := f.sandboxOf(r.Context(), project, row.Sandbox); err == nil && sb.State == api.StateRunning {
			if s, err := f.sampleEnvd(r.Context(), row); err == nil {
				out = append(out, s)
			} else {
				f.log.Debug("no metrics from envd", "sandbox", row.Sandbox, "err", err)
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (f *Frontend) sampleEnvd(ctx context.Context, row db.E2BSandbox) (metricJSON, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := f.ensureInit(ctx, row); err != nil {
		return metricJSON{}, err
	}
	var s struct {
		TS         int64   `json:"ts"`
		CPUCount   int     `json:"cpu_count"`
		CPUUsedPct float64 `json:"cpu_used_pct"`
		MemTotal   int64   `json:"mem_total"`
		MemUsed    int64   `json:"mem_used"`
		MemCache   int64   `json:"mem_cache"`
		DiskUsed   int64   `json:"disk_used"`
		DiskTotal  int64   `json:"disk_total"`
	}
	token, err := f.token(row)
	if err != nil {
		return metricJSON{}, err
	}
	if err := f.envdCall(ctx, row.Sandbox, http.MethodGet, "/metrics", token, nil, &s); err != nil {
		return metricJSON{}, err
	}
	t := time.Unix(s.TS, 0).UTC()
	return metricJSON{CPUCount: s.CPUCount, CPUUsedPct: s.CPUUsedPct, DiskTotal: s.DiskTotal, DiskUsed: s.DiskUsed,
		MemCache: s.MemCache, MemTotal: s.MemTotal, MemUsed: s.MemUsed,
		Timestamp: t.Format(time.RFC3339), TimestampUnix: s.TS}, nil
}

// listed is one sandbox in a listing, with what it is sorted and filtered by.
type listed struct {
	sb  api.Sandbox
	row db.E2BSandbox
}

// sandboxes is every E2B sandbox of the project.
func (f *Frontend) sandboxes(ctx context.Context, project string) ([]listed, error) {
	rows, err := f.store.E2Bs(ctx, project)
	if err != nil {
		return nil, err
	}
	var all api.SandboxList
	if err := f.call(ctx, project, http.MethodGet, "/v1/sandboxes", nil, &all); err != nil {
		return nil, err
	}
	byID := map[string]api.Sandbox{}
	for _, sb := range all.Sandboxes {
		byID[sb.ID] = sb
	}
	var out []listed
	for _, row := range rows {
		if sb, ok := byID[row.Sandbox]; ok {
			out = append(out, listed{sb, row})
		}
	}
	return out, nil
}

// parseMetadata reads the metadata filter: a URL-encoded k=v&k2=v2 string,
// as the SDKs send it and the API server parses it (utils.parseFilters).
func parseMetadata(q string) (map[string]string, error) {
	q, err := url.QueryUnescape(q)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, kv := range strings.Split(q, "&") {
		parts := strings.Split(kv, "=")
		if len(parts) != 2 {
			return nil, errors.New("invalid key value pair in query")
		}
		k, err1 := url.QueryUnescape(parts[0])
		v, err2 := url.QueryUnescape(parts[1])
		if err1 != nil || err2 != nil {
			return nil, errors.New("invalid escape in query")
		}
		out[k] = v
	}
	return out, nil
}

func matches(sb api.Sandbox, filter map[string]string) bool {
	for k, v := range filter {
		if got, ok := sb.Labels[LabelPrefix+k]; !ok || got != v {
			return false
		}
	}
	return true
}

// A page cursor is the last item's start time and ID, base64url, as hosted's
// (utils.ParseCursor) are; the SDKs only pass it back.
func cursorOf(l listed) string {
	return base64.URLEncoding.EncodeToString([]byte(l.row.StartedAt.UTC().Format(time.RFC3339Nano) + "__" + l.row.Sandbox))
}

func parseCursor(tok string) (time.Time, string, error) {
	b, err := base64.URLEncoding.DecodeString(tok)
	if err != nil {
		return time.Time{}, "", err
	}
	ts, id, ok := strings.Cut(string(b), "__")
	if !ok {
		return time.Time{}, "", errors.New("malformed cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	return t, id, err
}

// list is GET /v2/sandboxes: running and paused sandboxes (or the states
// asked for), filtered by metadata, template and start time, newest first (or
// oldest, with order=asc), in pages of limit (default and most 100) with the
// next page's cursor in X-Next-Token, absent on the last.
func (f *Frontend) list(w http.ResponseWriter, r *http.Request, project string) {
	q := r.URL.Query()
	states := []string{"running", "paused"}
	if s := q.Get("state"); s != "" {
		states = strings.Split(s, ",")
		for _, st := range states {
			if st != "running" && st != "paused" {
				writeErr(w, http.StatusBadRequest, fmt.Sprintf("Invalid state: %q", st))
				return
			}
		}
	}
	var filter map[string]string
	if s := q.Get("metadata"); s != "" {
		var err error
		if filter, err = parseMetadata(s); err != nil {
			writeErr(w, http.StatusBadRequest, "Error parsing metadata")
			return
		}
	}
	limit := 100
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 100 {
			writeErr(w, http.StatusBadRequest, "limit must be between 1 and 100")
			return
		}
		limit = n
	}
	asc := false
	switch q.Get("order") {
	case "", "desc":
	case "asc":
		asc = true
	default:
		writeErr(w, http.StatusBadRequest, "Invalid order parameter: want asc or desc")
		return
	}
	var after time.Time
	if s := q.Get("startedAfter"); s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "Invalid startedAfter: want an RFC 3339 time")
			return
		}
		after = t
	}
	var template string
	if s := q.Get("template"); s != "" {
		var ok bool
		if template, _, ok = f.templateOf(s); !ok {
			if slices.Contains(states, "running") {
				w.Header().Set("X-Total-Running", "0")
			}
			writeJSON(w, http.StatusOK, []detailJSON{})
			return
		}
	}
	var curT time.Time
	var curID string
	if tok := q.Get("nextToken"); tok != "" {
		var err error
		if curT, curID, err = parseCursor(tok); err != nil {
			writeErr(w, http.StatusBadRequest, "Invalid next token")
			return
		}
	}
	all, err := f.sandboxes(r.Context(), project)
	if err != nil {
		f.fail(w, "", err)
		return
	}
	now := f.now()
	var items []listed
	running := 0
	for _, l := range all {
		st := stateOf(l.row, now)
		if !slices.Contains(states, st) || !matches(l.sb, filter) || (!after.IsZero() && !l.row.StartedAt.After(after)) ||
			(template != "" && l.row.Template != template) {
			continue
		}
		if st == "running" {
			running++
		}
		items = append(items, l)
	}
	if slices.Contains(states, "running") {
		w.Header().Set("X-Total-Running", strconv.Itoa(running))
	}
	less := func(a, b listed) bool { // a comes before b
		if !a.row.StartedAt.Equal(b.row.StartedAt) {
			return a.row.StartedAt.After(b.row.StartedAt) != asc
		}
		return (a.row.Sandbox > b.row.Sandbox) != asc
	}
	sort.Slice(items, func(i, j int) bool { return less(items[i], items[j]) })
	if curID != "" {
		mark := listed{row: db.E2BSandbox{Sandbox: curID, StartedAt: curT}}
		i := sort.Search(len(items), func(i int) bool { return less(mark, items[i]) })
		items = items[i:]
	}
	if len(items) > limit {
		items = items[:limit]
		w.Header().Set("X-Next-Token", cursorOf(items[limit-1]))
	}
	out := make([]detailJSON, len(items))
	for i, l := range items {
		out[i] = f.detailOf(l.sb, l.row, "", now, true)
	}
	writeJSON(w, http.StatusOK, out)
}

// listV1 is the deprecated GET /sandboxes: running sandboxes, unpaged.
func (f *Frontend) listV1(w http.ResponseWriter, r *http.Request, project string) {
	var filter map[string]string
	if s := r.URL.Query().Get("metadata"); s != "" {
		var err error
		if filter, err = parseMetadata(s); err != nil {
			writeErr(w, http.StatusBadRequest, "Error parsing metadata")
			return
		}
	}
	all, err := f.sandboxes(r.Context(), project)
	if err != nil {
		f.fail(w, "", err)
		return
	}
	now := f.now()
	out := []detailJSON{}
	for _, l := range all {
		if stateOf(l.row, now) == "running" && matches(l.sb, filter) {
			out = append(out, f.detailOf(l.sb, l.row, "", now, true))
		}
	}
	writeJSON(w, http.StatusOK, out)
}
