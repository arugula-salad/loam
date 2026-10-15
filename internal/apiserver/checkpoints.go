package apiserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

// Checkpoints are snapshots (docs/NODE.md, "Snapshots"), named <sandbox
// id>.<checkpoint id>, with their records here and a snapshot record each
// (snapshots.go). IDs are v1, v2, ... for the ones a client takes and auto-1,
// ... for the ones a restore takes first; only the newest autoKeep automatic
// ones are kept. Deleting a checkpoint deletes its record; its snapshot goes
// at the next collection (gc.go), unless a sandbox still starts from it.
const autoKeep = 5

// allocCheckpoint takes the next ID of its kind; the counters only grow.
func (s *Server) allocCheckpoint(ctx context.Context, id string, auto bool) (string, error) {
	var cpID string
	_, err := s.store.UpdateSandbox(ctx, id, func(r *db.Sandbox) error {
		if auto {
			r.NextAuto++
			cpID = fmt.Sprintf("auto-%d", r.NextAuto)
		} else {
			r.NextCheckpoint++
			cpID = fmt.Sprintf("v%d", r.NextCheckpoint)
		}
		return nil
	})
	return cpID, err
}

// snapshotLocked takes checkpoint cp of sb on its node and records it.
func (s *Server) snapshotLocked(ctx context.Context, cl *nodeapi.Client, sb db.Sandbox, cp api.Checkpoint) (api.Checkpoint, error) {
	id := snapshotID(sb.ID, cp.ID)
	if err := s.beginSnapshot(ctx, sb, id, db.SnapCheckpoint, cp.ID, cp.Memory); err != nil {
		return cp, err
	}
	// A memory snapshot of a running VM ends every stream into its guest:
	// Firecracker resets the guest's vsock when it saves the device, and the
	// guest drops its connections without telling the host (the nodelet cuts
	// what is left of them). So the calls end here first, with 1011, as a
	// pause ends them (docs/API.md, "Checkpoints"), rather than hang.
	if cp.Memory && sb.State == api.StateRunning {
		s.rt(sb.ID).endCalls()
	}
	ns, err := cl.Snapshot(ctx, sb.ID, nodeapi.CreateSnapshot{ID: id, Memory: cp.Memory})
	if err != nil {
		s.abandonSnapshot(ctx, id)
		return cp, nodeErr(err)
	}
	if _, err := s.finishSnapshot(ctx, id, &ns); err != nil {
		return cp, err
	}
	if err := s.store.CreateCheckpoint(ctx, cp); err != nil {
		// The snapshot is there but no checkpoint holds it: the collection
		// deletes it rather than leak it.
		s.kickGC()
		return cp, err
	}
	if advances(sb.State, cp.Memory) {
		s.pointTo(ctx, sb.ID, id)
	}
	s.emit(sb, "checkpoint.created", map[string]any{"checkpoint": cp.ID, "memory": cp.Memory, "auto": cp.Auto})
	return cp, nil
}

// checkpoint takes a checkpoint of a sandbox the caller has acquired (so it
// is running), under its transition lock.
func (s *Server) checkpoint(ctx context.Context, sb db.Sandbox, comment string, memory bool, cpID string) (api.Checkpoint, error) {
	rt := s.rt(sb.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	ctx, cancel := opContext(ctx)
	defer cancel()
	cur, err := s.store.Sandbox(ctx, sb.ID)
	if err != nil {
		return api.Checkpoint{}, notFound("sandbox")
	}
	cl, err := s.nodeClient(ctx, cur)
	if err != nil {
		return api.Checkpoint{}, err
	}
	if cpID == "" {
		if cpID, err = s.allocCheckpoint(ctx, cur.ID, false); err != nil {
			return api.Checkpoint{}, err
		}
	}
	cp := api.Checkpoint{ID: cpID, Sandbox: cur.ID, Comment: comment, Memory: memory, CreatedAt: s.now()}
	return s.snapshotLocked(ctx, cl, cur, cp)
}

// createCheckpoint wakes the sandbox first, as every route under it but the
// record routes does (docs/API.md, "Waking"); a memory checkpoint then pauses
// it for about a second on the node and resumes it.
func (s *Server) createCheckpoint(w http.ResponseWriter, r *http.Request, c caller) {
	var req api.CreateCheckpoint
	if err := decode(r, &req); err != nil {
		s.writeErr(w, err)
		return
	}
	memory := req.Memory == nil || *req.Memory
	sb, err := s.lookup(r.Context(), c, r.PathValue("sandbox"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	sb, release, err := s.acquire(r.Context(), sb)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	defer release()
	cp, err := s.checkpoint(r.Context(), sb, req.Comment, memory, "")
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, cp)
}

func (s *Server) listCheckpoints(w http.ResponseWriter, r *http.Request, c caller) {
	sb, err := s.lookup(r.Context(), c, r.PathValue("sandbox"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	cps, err := s.store.Checkpoints(r.Context(), sb.ID)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if cps == nil {
		cps = []api.Checkpoint{}
	}
	writeJSON(w, http.StatusOK, api.CheckpointList{Checkpoints: cps})
}

func (s *Server) findCheckpoint(ctx context.Context, sb db.Sandbox, id string) (api.Checkpoint, error) {
	cp, err := s.store.Checkpoint(ctx, sb.ID, id)
	if errors.Is(err, db.ErrNotFound) {
		return cp, notFound("checkpoint")
	}
	return cp, err
}

func (s *Server) getCheckpoint(w http.ResponseWriter, r *http.Request, c caller) {
	sb, err := s.lookup(r.Context(), c, r.PathValue("sandbox"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	cp, err := s.findCheckpoint(r.Context(), sb, r.PathValue("checkpoint"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cp)
}

// dropCheckpointLocked deletes a checkpoint's record; the collection deletes
// its snapshot.
func (s *Server) dropCheckpointLocked(ctx context.Context, sb db.Sandbox, cpID string) error {
	if err := s.store.DeleteCheckpoint(ctx, sb.ID, cpID); err != nil && !errors.Is(err, db.ErrNotFound) {
		return err
	}
	s.emit(sb, "checkpoint.deleted", map[string]any{"checkpoint": cpID})
	s.kickGC()
	return nil
}

func (s *Server) deleteCheckpoint(w http.ResponseWriter, r *http.Request, c caller) {
	sb, err := s.lookup(r.Context(), c, r.PathValue("sandbox"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	cp, err := s.findCheckpoint(r.Context(), sb, r.PathValue("checkpoint"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	rt := s.rt(sb.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if err := s.dropCheckpointLocked(r.Context(), sb, cp.ID); err != nil {
		s.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// restoreCheckpoint keeps the sandbox's current state as an automatic
// checkpoint first, then replaces it with the checkpoint's. Every call and
// stream on the sandbox ends (1011). Restoring a memory checkpoint resumes its
// processes; a disk-only one boots fresh.
func (s *Server) restoreCheckpoint(w http.ResponseWriter, r *http.Request, c caller) {
	sb, err := s.lookup(r.Context(), c, r.PathValue("sandbox"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	target, err := s.findCheckpoint(r.Context(), sb, r.PathValue("checkpoint"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	rt := s.rt(sb.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	ctx, cancel := opContext(r.Context())
	defer cancel()
	if sb, err = s.store.Sandbox(ctx, sb.ID); err != nil {
		s.writeErr(w, notFound("sandbox"))
		return
	}
	if sb.State == api.StateCreating {
		s.writeErr(w, conflict("the sandbox is still being created"))
		return
	}
	// The checkpoint must be where the sandbox's node can restore it: on that
	// node, or stored.
	targetID := snapshotID(sb.ID, target.ID)
	if rec, err := s.store.Snapshot(ctx, targetID); err == nil && !rec.Stored && rec.Node != sb.Node {
		s.writeErr(w, unavailable("the checkpoint is on another node and not stored yet; try again shortly"))
		return
	}
	cl, err := s.nodeClient(ctx, sb)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	// Its policy before anything changes: a restore can't run without it.
	wake, pol, err := s.bootOf(ctx, sb)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	// The current state first. Memory when there is a VM or a pause snapshot
	// to take it from; the disk alone when it is stopped or failed. If even
	// that can't be taken, the restore is refused: it would lose the state the
	// contract promises to keep.
	autoID, err := s.allocCheckpoint(ctx, sb.ID, true)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	memory := sb.State == api.StateRunning || sb.State == api.StatePaused
	auto := api.Checkpoint{ID: autoID, Sandbox: sb.ID, Comment: "before restoring " + target.ID,
		Memory: memory, Auto: true, CreatedAt: s.now()}
	if _, err := s.snapshotLocked(ctx, cl, sb, auto); err != nil {
		s.writeErr(w, unavailable("the current state could not be kept, so nothing was restored: %v", err))
		return
	}
	rt.endCalls()
	if _, err := cl.Restore(ctx, sb.ID, nodeapi.Restore{Snapshot: targetID, Wake: wake, Policy: pol}); err != nil {
		s.log.Warn("restore failed", "sandbox", sb.ID, "checkpoint", target.ID, "err", err)
		s.writeErr(w, policyErr(err))
		return
	}
	rt.touch(s.now())
	if sb, err = s.pointTo(ctx, sb.ID, targetID); err != nil {
		s.writeErr(w, err)
		return
	}
	s.emit(sb, "checkpoint.restored", map[string]any{"checkpoint": target.ID, "auto": auto.ID})
	if sb, err = s.setState(ctx, sb, api.StateRunning, "", map[string]any{"restored": target.ID}); err != nil {
		s.writeErr(w, err)
		return
	}
	s.pruneAutoLocked(ctx, sb)
	writeJSON(w, http.StatusOK, s.view(sb))
}

// pruneAutoLocked keeps the newest autoKeep automatic checkpoints.
func (s *Server) pruneAutoLocked(ctx context.Context, sb db.Sandbox) {
	cps, err := s.store.Checkpoints(ctx, sb.ID)
	if err != nil {
		return
	}
	var autos []string
	for _, cp := range cps {
		if cp.Auto {
			autos = append(autos, cp.ID)
		}
	}
	for len(autos) > autoKeep {
		if err := s.dropCheckpointLocked(ctx, sb, autos[0]); err != nil {
			s.log.Warn("could not prune an automatic checkpoint", "sandbox", sb.ID, "checkpoint", autos[0], "err", err)
			return
		}
		autos = autos[1:]
	}
}
