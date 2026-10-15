package apiserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

// The guest's host channel (docs/NODE.md): the agent calls loam-api for its
// own sandbox through the nodelet, which forwards to /internal/guest/<path>
// with the node token and X-Loam-Sandbox set from the VM the request came
// from. Being inside is the credential, so the sandbox is whichever the
// nodelet names.

func (s *Server) guest(h func(http.ResponseWriter, *http.Request, db.Sandbox)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !tokenEqual(bearer(r), s.opts.NodeToken) {
			s.writeErr(w, errf(http.StatusUnauthorized, api.CodeUnauthorized, "the node token is required"))
			return
		}
		sb, err := s.store.Sandbox(r.Context(), r.Header.Get(nodeapi.HeaderSandbox))
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				err = notFound("sandbox")
			}
			s.writeErr(w, err)
			return
		}
		h(w, r, sb)
	}
}

// guestSelf is GET /self from inside: the sandbox's own record.
func (s *Server) guestSelf(w http.ResponseWriter, r *http.Request, sb db.Sandbox) {
	writeJSON(w, http.StatusOK, s.view(sb))
}

// guestCheckpoint is POST /checkpoints from inside. A disk-only checkpoint
// doesn't pause the VM, so it is taken before the answer, as from outside. A
// memory one is answered first and taken after: the snapshot captures the
// guest mid-call, and an answer the guest hasn't received by then would be
// lost in every restore of it (docs/AGENT.md). The checkpoint's ID is taken
// before the answer so the guest knows what it will be called; the record
// appears once the snapshot exists. The delay gives the answer time to cross
// the nodelet and the vsock before the VM pauses.
func (s *Server) guestCheckpoint(w http.ResponseWriter, r *http.Request, sb db.Sandbox) {
	var req api.CreateCheckpoint
	if err := decode(r, &req); err != nil {
		s.writeErr(w, err)
		return
	}
	memory := req.Memory == nil || *req.Memory
	if !memory {
		cp, err := s.checkpoint(r.Context(), sb, req.Comment, false, "")
		if err != nil {
			s.writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, cp)
		return
	}
	id, err := s.allocCheckpoint(r.Context(), sb.ID, false)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, api.Checkpoint{ID: id, Sandbox: sb.ID, Comment: req.Comment, Memory: true, CreatedAt: s.now()})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		time.Sleep(s.opts.GuestCheckpointDelay)
		ctx := context.Background()
		cur, release, err := s.acquire(ctx, sb)
		if err == nil {
			_, err = s.checkpoint(ctx, cur, req.Comment, true, id)
			release()
		}
		if err != nil {
			s.log.Warn("a checkpoint asked for from inside failed", "sandbox", sb.ID, "checkpoint", id, "err", err)
		}
	}()
}
