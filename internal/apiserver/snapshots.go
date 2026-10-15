package apiserver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

// Snapshot records (docs/NODE.md, "Phase 2: the snapshot store"). Every
// snapshot a nodelet makes for loam-api is recorded: a checkpoint's
// (<sandbox>.<checkpoint>), a pause's or a stop's (<sandbox>.@<n>), and one a
// nodelet made on its own while draining, which loam-api learns of from the
// VM's "snapshot". A record carries the layers the snapshot references, which
// is what keeps layer objects in the store alive (gc.go), and whether it is
// stored, which is what lets the sandbox wake on another node.
//
// A sandbox's record names its newest snapshot (db.Sandbox.Snapshot), the one
// a wake elsewhere would use. These advance it: a pause, a stop, a memory
// checkpoint, a disk-only checkpoint of a stopped sandbox, a restore (to the
// checkpoint restored) and a create from a snapshot (a fork: the source's
// checkpoint). A disk-only checkpoint of a running or paused sandbox doesn't:
// waking from it would lose memory a newer snapshot or the VM still has.

// snapshotID is a checkpoint's snapshot (docs/NODE.md, "Snapshots").
func snapshotID(sandbox, checkpoint string) string { return sandbox + "." + checkpoint }

// allocSnapshot takes the name of the sandbox's next pause or stop snapshot;
// the counter only grows, so a name is never reused in the shared store.
func (s *Server) allocSnapshot(ctx context.Context, id string) (string, error) {
	var name string
	_, err := s.store.UpdateSandbox(ctx, id, func(r *db.Sandbox) error {
		r.NextSnapshot++
		name = fmt.Sprintf("%s.@%d", id, r.NextSnapshot)
		return nil
	})
	return name, err
}

// beginSnapshot records a snapshot loam-api is about to ask sb's node for, as
// pending. Until the nodelet answers with the snapshot's own layers, the
// record holds the layers of the snapshot it will be a diff on (the
// sandbox's newest), so nothing the new one shares is collected meanwhile.
func (s *Server) beginSnapshot(ctx context.Context, sb db.Sandbox, id, kind, checkpoint string, memory bool) error {
	rec := db.Snapshot{ID: id, Sandbox: sb.ID, Kind: kind, Checkpoint: checkpoint, Node: sb.Node,
		Parent: sb.Snapshot, Memory: memory, State: db.SnapPending, CreatedAt: s.now()}
	if sb.Snapshot != "" {
		if p, err := s.store.Snapshot(ctx, sb.Snapshot); err == nil {
			rec.Layers = p.Layers
		}
	}
	_, err := s.store.CreateSnapshot(ctx, rec)
	return err
}

// finishSnapshot makes a record ready, from the nodelet's account of it when
// there is one. Stored only ever goes from false to true: a snapshot event
// may have come before the answer.
func (s *Server) finishSnapshot(ctx context.Context, id string, ns *nodeapi.Snapshot) (db.Snapshot, error) {
	return s.store.UpdateSnapshot(ctx, id, func(r *db.Snapshot) error {
		r.State = db.SnapReady
		if ns != nil {
			fillSnapshot(r, *ns)
		}
		return nil
	})
}

func fillSnapshot(r *db.Snapshot, ns nodeapi.Snapshot) {
	r.Parent, r.Memory, r.SizeBytes = ns.Parent, ns.Memory, ns.SizeBytes
	if len(ns.Layers) > 0 {
		r.Layers = ns.Layers
	}
	r.Stored = r.Stored || ns.Stored
}

// abandonSnapshot marks a record whose call failed. The nodelet may have made
// the snapshot all the same, so it is deleted like garbage (gc.go).
func (s *Server) abandonSnapshot(ctx context.Context, id string) {
	_, err := s.store.UpdateSnapshot(context.WithoutCancel(ctx), id, func(r *db.Snapshot) error {
		if r.State != db.SnapPending {
			return errNoChange
		}
		r.State = db.SnapAbandoned
		return nil
	})
	if err == nil {
		s.kickGC()
	}
}

// pointTo makes snap the sandbox's newest snapshot.
func (s *Server) pointTo(ctx context.Context, id, snap string) (db.Sandbox, error) {
	return s.store.UpdateSandbox(context.WithoutCancel(ctx), id, func(r *db.Sandbox) error {
		r.Snapshot = snap
		return nil
	})
}

// settleSnapshot is the outcome of a pause or a stop that named snapshot
// want and got a VM naming got (its newest snapshot): want, unless the VM was
// in that state already and kept the snapshot it had. Records are finished,
// abandoned or adopted to match, and the sandbox points at the VM's.
func (s *Server) settleSnapshot(ctx context.Context, cl *nodeapi.Client, sb db.Sandbox, want, got string) db.Sandbox {
	if got == "" {
		got = want // a nodelet that doesn't say: take it as asked
	}
	if got != want {
		s.abandonSnapshot(ctx, want)
	}
	if err := s.adoptSnapshot(ctx, cl, sb, got); err != nil {
		s.log.Warn("could not record a snapshot", "sandbox", sb.ID, "snapshot", got, "err", err)
		return sb
	}
	if cur, err := s.pointTo(ctx, sb.ID, got); err == nil {
		return cur
	}
	return sb
}

// adoptSnapshot makes sure snapshot id, which node cl has, has a ready
// record: one loam-api asked for is finished, one it didn't (a pause while
// draining) is recorded as the sandbox's. The nodelet's account fills it in.
// Without one, a pending record is finished as it is when the nodelet can't
// be asked (the snapshot event fills it in later), and abandoned when the
// nodelet says it has no such snapshot.
func (s *Server) adoptSnapshot(ctx context.Context, cl *nodeapi.Client, sb db.Sandbox, id string) error {
	var ns *nodeapi.Snapshot
	got, gerr := cl.GetSnapshot(ctx, id)
	if gerr == nil {
		ns = &got
	}
	rec, err := s.store.Snapshot(ctx, id)
	switch {
	case err == nil:
		switch {
		case nodeapi.IsCode(gerr, api.CodeNotFound):
			s.abandonSnapshot(ctx, id)
			return fmt.Errorf("the node has no snapshot %q", id)
		case rec.State == db.SnapReady && (ns == nil || ((rec.Stored || !ns.Stored) && len(rec.Layers) > 0)):
			return nil
		case rec.State == db.SnapAbandoned && ns == nil:
			return fmt.Errorf("snapshot %q was abandoned and its node can't be asked: %w", id, gerr)
		}
		_, err = s.finishSnapshot(ctx, id, ns)
		return err
	case !errors.Is(err, db.ErrNotFound):
		return err
	case ns == nil:
		return fmt.Errorf("the node has no snapshot %q: %w", id, gerr)
	}
	owner := ns.VM
	if owner == "" {
		owner = sb.ID
	}
	rec = db.Snapshot{ID: id, Sandbox: owner, Kind: db.SnapPause, Node: sb.Node, State: db.SnapReady, CreatedAt: ns.CreatedAt}
	if !ns.Memory {
		rec.Kind = db.SnapStop
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = s.now()
	}
	fillSnapshot(&rec, *ns)
	_, err = s.store.CreateSnapshot(ctx, rec)
	if errors.Is(err, db.ErrExists) {
		return nil
	}
	if err == nil {
		if early, ok := s.takeStoredEarly(id); ok {
			s.snapshotStored(ctx, early)
		}
	}
	return err
}

// snapshotStored is a nodelet's snapshot event: the snapshot is in the store.
// A pause or stop snapshot that is stored supersedes its sandbox's older
// ones, which the collection then deletes.
func (s *Server) snapshotStored(ctx context.Context, ns nodeapi.Snapshot) {
	_, err := s.store.UpdateSnapshot(ctx, ns.ID, func(r *db.Snapshot) error {
		if r.Stored && len(r.Layers) == len(ns.Layers) {
			return errNoChange
		}
		fillSnapshot(r, ns)
		r.Stored = true
		return nil
	})
	switch {
	case err == nil:
		s.kickGC()
	case errors.Is(err, db.ErrNotFound):
		// Not recorded yet: a node's own snapshot (a drain's) whose VM event
		// is still being handled. adoptSnapshot takes it from here.
		s.mu.Lock()
		s.storedEarly[ns.ID] = storedEarly{ns, s.now()}
		for id, e := range s.storedEarly {
			if s.now().Sub(e.at) > storedEarlyFor {
				delete(s.storedEarly, id)
			}
		}
		s.mu.Unlock()
	case !errors.Is(err, errNoChange):
		s.log.Warn("could not record a stored snapshot", "snapshot", ns.ID, "err", err)
	}
}

// storedEarly is a snapshot event for a snapshot with no record yet, kept
// for storedEarlyFor in case the record comes.
type storedEarly struct {
	ns nodeapi.Snapshot
	at time.Time
}

const storedEarlyFor = 10 * time.Minute

// takeStoredEarly is the stored event that came before snapshot id's record.
func (s *Server) takeStoredEarly(id string) (nodeapi.Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.storedEarly[id]
	delete(s.storedEarly, id)
	return e.ns, ok
}

// advances says whether a checkpoint becomes the sandbox's newest snapshot
// (see the top of this file).
func advances(state string, memory bool) bool {
	return memory || state == api.StateStopped
}

// newestSnapshot is the sandbox's newest snapshot's record; ok is false when
// it has none.
func (s *Server) newestSnapshot(ctx context.Context, sb db.Sandbox) (db.Snapshot, bool) {
	if sb.Snapshot == "" {
		return db.Snapshot{}, false
	}
	rec, err := s.store.Snapshot(ctx, sb.Snapshot)
	return rec, err == nil
}
