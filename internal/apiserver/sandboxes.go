package apiserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

// Defaults docs/API.md gives a sandbox.
const (
	defaultTemplate    = "base"
	defaultIdleTimeout = 300
	minIdleTimeout     = 30
	defaultHTTPPort    = 8080
)

// sandboxURL is the sandbox's own URL, which goes to its http_port.
func (s *Server) sandboxURL(sb db.Sandbox) string {
	return fmt.Sprintf("https://%s--%s.%s", sb.Name, sb.Project, s.opts.Domain)
}

func (s *Server) portURL(sb db.Sandbox, port int) string {
	return fmt.Sprintf("https://%d--%s--%s.%s", port, sb.Name, sb.Project, s.opts.Domain)
}

// domainURLs are the sandbox's URL under each of its project's URL domains.
func (s *Server) domainURLs(sb db.Sandbox) []string {
	out := []string{}
	for _, d := range s.opts.URLDomains.of(sb.Project) {
		out = append(out, fmt.Sprintf("https://%s.%s", sb.Name, d))
	}
	return out
}

func (s *Server) view(sb db.Sandbox) api.Sandbox {
	sb = sb.Clone()
	return api.Sandbox{ID: sb.ID, Name: sb.Name, Project: sb.Project, Template: sb.Template, Image: sb.Image, State: sb.State,
		Error: sb.Error, Resources: sb.Resources, Env: sb.Env, Labels: sb.Labels, Idle: sb.Idle,
		Deadline: sb.Deadline, Protected: sb.Protected, HTTPPort: sb.HTTPPort, Network: sb.Network,
		Secrets: sb.Secrets, URL: s.sandboxURL(sb), URLs: s.domainURLs(sb),
		CreatedAt: sb.CreatedAt, UpdatedAt: sb.UpdatedAt, LastActiveAt: sb.LastActiveAt}
}

// lookup finds {sandbox}, an ID or a name, in the caller's project. Another
// project's sandbox is not found, never forbidden (docs/API.md).
func (s *Server) lookup(ctx context.Context, c caller, ref string) (db.Sandbox, error) {
	if idRE.MatchString(ref) {
		sb, err := s.store.Sandbox(ctx, ref)
		if err == nil && sb.Project == c.project.Name {
			return sb, nil
		}
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return sb, err
		}
	}
	sb, err := s.store.SandboxByName(ctx, c.project.Name, ref)
	if errors.Is(err, db.ErrNotFound) {
		return sb, notFound("sandbox")
	}
	return sb, err
}

func checkIdle(in *api.Idle) (api.Idle, error) {
	idle := api.Idle{TimeoutS: defaultIdleTimeout, Action: api.ActionPause}
	if in == nil {
		return idle, nil
	}
	if in.TimeoutS != 0 {
		idle.TimeoutS = in.TimeoutS
	}
	if in.Action != "" {
		idle.Action = in.Action
	}
	if idle.TimeoutS < minIdleTimeout {
		return idle, invalid("idle.timeout_s must be at least %d", minIdleTimeout)
	}
	switch idle.Action {
	case api.ActionPause, api.ActionStop, api.ActionNone:
	default:
		return idle, invalid("idle.action must be pause, stop or none")
	}
	return idle, nil
}

func (s *Server) checkDeadline(d *api.Deadline) error {
	if d == nil {
		return nil
	}
	switch d.Action {
	case api.ActionDelete, api.ActionPause, api.ActionStop:
	default:
		return invalid("deadline.action must be delete, pause or stop")
	}
	if !d.At.After(s.now()) {
		return invalid("deadline.at must be in the future")
	}
	return nil
}

func checkEnv(env map[string]string) error {
	for k := range env {
		if !envKeyRE.MatchString(k) {
			return invalid("env name %q is not a valid variable name", k)
		}
	}
	return nil
}

func checkLabels(labels map[string]string) error {
	for k, v := range labels {
		if k == "" || len(k) > 63 || strings.ContainsAny(k, "=,") || len(v) > 255 {
			return invalid("label %q: a key is 1 to 63 characters without '=' or ',', a value at most 255", k)
		}
	}
	return nil
}

func checkPort(p int) error {
	if p < 1 || p > 65535 {
		return invalid("a port is 1 to 65535")
	}
	return nil
}

func (s *Server) createSandbox(w http.ResponseWriter, r *http.Request, c caller) {
	var req api.CreateSandbox
	if err := decode(r, &req); err != nil {
		s.writeErr(w, err)
		return
	}
	sb, err := s.newRecord(r.Context(), c, req)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	sb, err = s.create(r.Context(), sb, req.From)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.view(sb))
}

// newRecord checks a create and fills in its defaults, and places it.
func (s *Server) newRecord(ctx context.Context, c caller, req api.CreateSandbox) (db.Sandbox, error) {
	now := s.now()
	sb := db.Sandbox{ID: newSandboxID(), Project: c.project.Name, Name: req.Name, State: api.StateCreating,
		Env: req.Env, Labels: req.Labels, Deadline: req.Deadline, Protected: req.Protected, HTTPPort: req.HTTPPort,
		CreatedAt: now, UpdatedAt: now, LastActiveAt: now}
	if sb.Name == "" {
		sb.Name = generateName()
	} else if !validName(sb.Name) {
		return sb, invalid("a sandbox name is a DNS label of at most 32 characters without \"--\"")
	}
	if len(sb.Name)+len(sb.Project) > maxHostLabel {
		return sb, invalid("the name and project together may be at most %d characters, so that the port URLs are valid host names", maxHostLabel)
	}
	var err error
	if sb.Idle, err = checkIdle(req.Idle); err != nil {
		return sb, err
	}
	if err := s.checkDeadline(req.Deadline); err != nil {
		return sb, err
	}
	if err := checkEnv(req.Env); err != nil {
		return sb, err
	}
	if err := checkLabels(req.Labels); err != nil {
		return sb, err
	}
	if sb.HTTPPort == 0 {
		sb.HTTPPort = defaultHTTPPort
	}
	if err := checkPort(sb.HTTPPort); err != nil {
		return sb, err
	}
	if req.Network != nil {
		sb.Network = *req.Network
	}
	if req.Secrets != nil {
		sb.Secrets = *req.Secrets
	}
	sb = sb.Clone()

	if req.From != nil {
		if req.Template != "" || req.Image != "" || req.Resources != nil {
			return sb, invalid("a fork takes its template or image and its resources from the checkpoint; don't give them")
		}
		src, err := s.lookup(ctx, c, req.From.Sandbox)
		if err != nil {
			return sb, err
		}
		// A fork keeps its source's boundary unless it gives its own: a
		// copy of a restricted sandbox is restricted too.
		if req.Network == nil {
			sb.Network = db.CloneNetwork(src.Network)
		}
		if req.Secrets == nil {
			sb.Secrets = append([]string{}, src.Secrets...)
		}
		if err := s.checkBoundary(sb); err != nil {
			return sb, err
		}
		if _, err := s.store.Checkpoint(ctx, src.ID, req.From.Checkpoint); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return sb, notFound("checkpoint")
			}
			return sb, err
		}
		sb.Template, sb.Image, sb.Resources = src.Template, src.Image, src.Resources
		sb.Snapshot = snapshotID(src.ID, req.From.Checkpoint)
		req.From.Sandbox = src.ID
		nodes, err := s.nodes.List(ctx)
		if err != nil {
			return sb, unavailable("the nodes can't be listed: %v", err)
		}
		if err := checkBrokers(nodes, sb); err != nil {
			return sb, err
		}
		// A fork goes to any node with room, its source's first. A checkpoint
		// that isn't stored yet is only on the node that took it.
		snapNode := src.Node
		rec, err := s.store.Snapshot(ctx, sb.Snapshot)
		if err == nil {
			snapNode = rec.Node
		}
		if err == nil && rec.Stored {
			sb.Node = placePreferring(runnable(nodes, sb), sb.Resources.MemoryMiB, src.Node)
		} else {
			sb.Node = place(runnable(only(nodes, snapNode), sb), sb.Resources.MemoryMiB)
		}
		if sb.Node == "" {
			return sb, capacityErr("no node that has the checkpoint has room for the fork now")
		}
		return sb, nil
	}

	if err := s.checkBoundary(sb); err != nil {
		return sb, err
	}
	if req.Image != "" {
		if req.Template != "" {
			return sb, invalid("give a template or an image, not both")
		}
		if err := s.checkImage(req.Image); err != nil {
			return sb, err
		}
		sb.Image, sb.Resources = req.Image, defaultImageResources
	} else {
		sb.Template = req.Template
		if sb.Template == "" {
			sb.Template = defaultTemplate
		}
		tpl, ok := s.opts.Templates[sb.Template]
		if !ok {
			return sb, invalid("no template %q", sb.Template)
		}
		sb.Resources = tpl.Resources
	}
	if req.Resources != nil {
		if req.Resources.CPUs != 0 {
			sb.Resources.CPUs = req.Resources.CPUs
		}
		if req.Resources.MemoryMiB != 0 {
			sb.Resources.MemoryMiB = req.Resources.MemoryMiB
		}
		if req.Resources.DiskGiB != 0 {
			sb.Resources.DiskGiB = req.Resources.DiskGiB
		}
	}
	if err := checkResources(sb.Resources); err != nil {
		return sb, invalid("%v", err)
	}
	nodes, err := s.nodes.List(ctx)
	if err != nil {
		return sb, unavailable("the nodes can't be listed: %v", err)
	}
	if err := checkBrokers(nodes, sb); err != nil {
		return sb, err
	}
	if sb.Node = place(runnable(nodes, sb), sb.Resources.MemoryMiB); sb.Node == "" {
		return sb, capacityErr("no node has room for the sandbox now")
	}
	return sb, nil
}

// checkBoundary checks a sandbox's network and secrets as it will be stored.
func (s *Server) checkBoundary(sb db.Sandbox) error {
	if err := checkNetwork(sb.Network); err != nil {
		return err
	}
	if len(sb.Secrets) > 0 && s.sealer == nil {
		return noSecretKey()
	}
	return checkSecretNames(sb.Secrets, sb.Env)
}

// create stores the record and boots its VM, holding the sandbox's lock
// throughout, and answers once it is running (docs/API.md). A node that turns
// out to be full drops the record again (capacity, try later); any other
// failure leaves it in the error state, to be looked at and deleted.
func (s *Server) create(ctx context.Context, sb db.Sandbox, from *api.From) (db.Sandbox, error) {
	rt := s.rt(sb.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if err := s.store.CreateSandbox(ctx, sb); err != nil {
		s.forgetRT(sb.ID)
		if errors.Is(err, db.ErrExists) {
			return sb, errf(http.StatusConflict, api.CodeAlreadyExists, "the project already has a sandbox named %q", sb.Name)
		}
		return sb, err
	}
	ctx, cancel := opContext(ctx)
	defer cancel()
	wake, pol, err := s.bootOf(ctx, sb)
	if err != nil {
		s.store.DeleteSandbox(ctx, sb.ID)
		s.forgetRT(sb.ID)
		return sb, err
	}
	s.emit(sb, "sandbox.created", nil)
	cl, err := s.nodeClient(ctx, sb)
	if err == nil {
		req := nodeapi.CreateVM{ID: sb.ID, Template: s.templateOf(sb), Resources: sb.Resources, Wake: wake, Policy: pol}
		if from != nil {
			req.From = &nodeapi.FromSnapshot{Snapshot: snapshotID(from.Sandbox, from.Checkpoint)}
		}
		_, err = cl.Create(ctx, req)
	}
	if err != nil {
		e, ok := err.(*apiErr)
		if !ok {
			e = policyErr(err)
		}
		// Capacity is for later; a policy the node refused is the client's
		// to change. Neither leaves a sandbox behind.
		switch e.code {
		case api.CodeCapacity, api.CodeInvalid, api.CodeUnsupported:
			s.store.DeleteSandbox(ctx, sb.ID)
			s.forgetRT(sb.ID)
			s.emit(sb, "sandbox.deleted", map[string]any{"reason": e.code})
			return sb, e
		}
		s.log.Warn("create failed", "sandbox", sb.ID, "node", sb.Node, "err", err)
		sb, _ = s.setState(ctx, sb, api.StateError, e.msg, nil)
		return sb, unavailable("the sandbox could not be created: %s", e.msg)
	}
	rt.touch(s.now())
	s.log.Info("sandbox created", "sandbox", sb.ID, "name", sb.Name, "project", sb.Project, "node", sb.Node)
	return s.setState(ctx, sb, api.StateRunning, "", nil)
}

func (s *Server) listSandboxes(w http.ResponseWriter, r *http.Request, c caller) {
	q := r.URL.Query()
	want := map[string]string{}
	for _, l := range q["label"] {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			s.writeErr(w, invalid("label filters are key=value"))
			return
		}
		want[k] = v
	}
	state := q.Get("state")
	all, err := s.store.Sandboxes(r.Context(), c.project.Name)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := api.SandboxList{Sandboxes: []api.Sandbox{}}
next:
	for _, sb := range all {
		if state != "" && sb.State != state {
			continue
		}
		for k, v := range want {
			if got, ok := sb.Labels[k]; !ok || got != v {
				continue next
			}
		}
		out.Sandboxes = append(out.Sandboxes, s.view(sb))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getSandbox(w http.ResponseWriter, r *http.Request, c caller) {
	sb, err := s.lookup(r.Context(), c, r.PathValue("sandbox"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.view(sb))
}

func (s *Server) patchSandbox(w http.ResponseWriter, r *http.Request, c caller) {
	var req api.UpdateSandbox
	if err := decode(r, &req); err != nil {
		s.writeErr(w, err)
		return
	}
	if err := checkEnv(req.Env); err != nil {
		s.writeErr(w, err)
		return
	}
	if err := checkLabels(req.Labels); err != nil {
		s.writeErr(w, err)
		return
	}
	if req.Deadline != nil {
		if err := s.checkDeadline(*req.Deadline); err != nil {
			s.writeErr(w, err)
			return
		}
	}
	if req.HTTPPort != nil {
		if err := checkPort(*req.HTTPPort); err != nil {
			s.writeErr(w, err)
			return
		}
	}
	if req.Network != nil {
		if err := checkNetwork(*req.Network); err != nil {
			s.writeErr(w, err)
			return
		}
	}
	// Without the key a sandbox can't be given secrets; one that has some
	// may still have its env changed, and is refused only at its next wake.
	if req.Secrets != nil && len(*req.Secrets) > 0 && s.sealer == nil {
		s.writeErr(w, noSecretKey())
		return
	}
	sb, err := s.lookup(r.Context(), c, r.PathValue("sandbox"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	rt := s.rt(sb.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	var prev db.Sandbox
	cur, err := s.store.UpdateSandbox(r.Context(), sb.ID, func(x *db.Sandbox) error {
		prev = x.Clone()
		if req.Env != nil {
			x.Env = req.Env
		}
		if req.Labels != nil {
			x.Labels = req.Labels
		}
		if req.Idle != nil {
			// A field left out keeps its current value, not the default: a PATCH
			// of the action alone mustn't reset the timeout.
			merged := x.Idle
			if req.Idle.TimeoutS != 0 {
				merged.TimeoutS = req.Idle.TimeoutS
			}
			if req.Idle.Action != "" {
				merged.Action = req.Idle.Action
			}
			idle, err := checkIdle(&merged)
			if err != nil {
				return err
			}
			x.Idle = idle
		}
		if req.Deadline != nil {
			x.Deadline = *req.Deadline
		}
		if req.Protected != nil {
			x.Protected = *req.Protected
		}
		if req.HTTPPort != nil {
			x.HTTPPort = *req.HTTPPort
		}
		if req.Network != nil {
			x.Network = db.CloneNetwork(*req.Network)
		}
		if req.Secrets != nil {
			x.Secrets = append([]string{}, *req.Secrets...)
		}
		if req.Env != nil || req.Secrets != nil {
			if err := checkSecretNames(x.Secrets, x.Env); err != nil {
				return err
			}
		}
		x.UpdatedAt = s.now()
		return nil
	})
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if cur.State != api.StateRunning {
		// A sleeping sandbox gets it all when it wakes.
		writeJSON(w, http.StatusOK, s.view(cur))
		return
	}
	ctx, cancel := opContext(r.Context())
	defer cancel()
	// A new network or new secrets apply to a running sandbox at once
	// (docs/API.md). One its node refuses, or that never reached it, is taken
	// back, so the record never claims a boundary the VM doesn't have; one
	// sent without an answer stays, and the client is told.
	if req.Network != nil || req.Secrets != nil {
		if err := s.pushPolicyLocked(ctx, cur); err != nil {
			var lost *unconfirmed
			if errors.As(err, &lost) {
				s.log.Warn("a running sandbox's node didn't answer its new policy", "sandbox", cur.ID, "err", err)
				s.writeErr(w, unavailable("the change is recorded, but the sandbox's node didn't confirm it; it applies at the next wake, or when the change is sent again"))
				return
			}
			s.revertPatch(ctx, prev)
			e, ok := err.(*apiErr)
			if !ok {
				e = policyErr(err)
			}
			s.writeErr(w, e)
			return
		}
	}
	// New env, and the placeholders of new secrets, reach a running
	// sandbox's new processes now: a resume of a running VM hands the agent
	// the new wake (docs/NODE.md). A sleeping one gets it when it wakes.
	if req.Env != nil || req.Secrets != nil {
		wake, pol, err := s.bootOf(ctx, cur)
		if err == nil {
			var cl *nodeapi.Client
			if cl, err = s.nodeClient(ctx, cur); err == nil {
				_, err = cl.Resume(ctx, cur.ID, nodeapi.Resume{Wake: wake, Policy: pol})
			}
		}
		if err != nil {
			s.log.Warn("could not hand a running sandbox its new env; it gets it at the next wake", "sandbox", cur.ID, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, s.view(cur))
}

// revertPatch puts back what a PATCH changed, after its node refused it.
func (s *Server) revertPatch(ctx context.Context, prev db.Sandbox) {
	_, err := s.store.UpdateSandbox(context.WithoutCancel(ctx), prev.ID, func(x *db.Sandbox) error {
		x.Env, x.Labels, x.Idle, x.Deadline, x.Protected = prev.Env, prev.Labels, prev.Idle, prev.Deadline, prev.Protected
		x.HTTPPort, x.Network, x.Secrets, x.UpdatedAt = prev.HTTPPort, prev.Network, prev.Secrets, prev.UpdatedAt
		return nil
	})
	if err != nil {
		s.log.Error("could not take back a change its node refused; the record says more than the VM has", "sandbox", prev.ID, "err", err)
	}
}

func (s *Server) deleteSandbox(w http.ResponseWriter, r *http.Request, c caller) {
	sb, err := s.lookup(r.Context(), c, r.PathValue("sandbox"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	rt := s.rt(sb.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if sb, err = s.store.Sandbox(r.Context(), sb.ID); err != nil {
		s.writeErr(w, notFound("sandbox"))
		return
	}
	if sb.Protected {
		s.writeErr(w, conflict("the sandbox is protected; unprotect it first"))
		return
	}
	if err := s.deleteLocked(r.Context(), rt, sb, "request"); err != nil {
		s.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// transition runs one of pause, stop or wake on request.
func (s *Server) transition(w http.ResponseWriter, r *http.Request, c caller,
	do func(context.Context, *sbrt, db.Sandbox) (db.Sandbox, error)) {
	sb, err := s.lookup(r.Context(), c, r.PathValue("sandbox"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	rt := s.rt(sb.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if sb, err = s.store.Sandbox(r.Context(), sb.ID); err != nil {
		s.writeErr(w, notFound("sandbox"))
		return
	}
	if sb, err = do(r.Context(), rt, sb); err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.view(sb))
}

func (s *Server) pauseSandbox(w http.ResponseWriter, r *http.Request, c caller) {
	s.transition(w, r, c, func(ctx context.Context, rt *sbrt, sb db.Sandbox) (db.Sandbox, error) {
		return s.pauseLocked(ctx, rt, sb, "request")
	})
}

func (s *Server) stopSandbox(w http.ResponseWriter, r *http.Request, c caller) {
	s.transition(w, r, c, func(ctx context.Context, rt *sbrt, sb db.Sandbox) (db.Sandbox, error) {
		return s.stopLocked(ctx, rt, sb, "request")
	})
}

func (s *Server) wakeSandbox(w http.ResponseWriter, r *http.Request, c caller) {
	s.transition(w, r, c, s.wakeLocked)
}
