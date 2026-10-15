package apiserver

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

// Locking, after sandpit's engine (engine/lifecycle.go):
//
//  1. sbrt.mu, one sandbox's transition lock, is outermost. It is held across
//     slow work: a nodelet call that boots, pauses, snapshots or restores. Every
//     transition takes it and reads the record afresh under it, so a decision
//     taken on a list read earlier (the lifecycle loop's) is taken again.
//  2. At most one sbrt.mu is held at a time. Background passes (the idle rule,
//     deadlines, reconciliation) use TryLock and treat a held lock as "busy,
//     look again next time" rather than queue behind a transition.
//  3. sbrt.useMu, s.mu and the store's own locking are leaves.
//
// This is all in memory, which is why phase 1 runs a single replica: a second
// loam-api would neither see this one's in-flight calls nor be ordered against
// its transitions.

// sbrt is the in-memory state of one sandbox.
type sbrt struct {
	mu sync.Mutex

	useMu    sync.Mutex
	inflight int
	lastUse  time.Time
	// calls are the forwarded calls and streams in progress, so a transition
	// that takes the VM away (pause, stop, restore, delete) can end them; the
	// cause is what a WebSocket client's close says (forward.go).
	calls map[*context.CancelCauseFunc]struct{}
}

// errSandboxGone is why a call was ended by a transition: the sandbox went
// away under it (docs/API.md, "Exec": close 1011, no exit frame).
var errSandboxGone = errors.New("the sandbox went away under the call")

func (s *Server) rt(id string) *sbrt {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt, ok := s.rts[id]
	if !ok {
		// A sandbox this process hasn't served yet gets a fresh idle period:
		// after a restart, nothing knows how long it has really been idle.
		rt = &sbrt{lastUse: s.now(), calls: map[*context.CancelCauseFunc]struct{}{}}
		s.rts[id] = rt
	}
	return rt
}

func (s *Server) forgetRT(id string) {
	s.mu.Lock()
	delete(s.rts, id)
	s.mu.Unlock()
}

func (rt *sbrt) begin() {
	rt.useMu.Lock()
	rt.inflight++
	rt.useMu.Unlock()
}

func (rt *sbrt) end(now time.Time) {
	rt.useMu.Lock()
	rt.inflight--
	rt.lastUse = now
	rt.useMu.Unlock()
}

// touch counts as use without holding: a wake is the start of an idle period.
func (rt *sbrt) touch(now time.Time) {
	rt.useMu.Lock()
	rt.lastUse = now
	rt.useMu.Unlock()
}

// idleFor is how long since the last call ended, and false while one is in flight.
func (rt *sbrt) idleFor(now time.Time) (time.Duration, bool) {
	rt.useMu.Lock()
	defer rt.useMu.Unlock()
	if rt.inflight > 0 {
		return 0, false
	}
	return now.Sub(rt.lastUse), true
}

// track registers a call to be ended by the next transition; the returned
// func unregisters it.
func (rt *sbrt) track(cancel context.CancelCauseFunc) func() {
	rt.useMu.Lock()
	rt.calls[&cancel] = struct{}{}
	rt.useMu.Unlock()
	return func() {
		rt.useMu.Lock()
		delete(rt.calls, &cancel)
		rt.useMu.Unlock()
	}
}

// endCalls ends every forwarded call on the sandbox. Called under rt.mu by a
// transition that takes the VM away.
func (rt *sbrt) endCalls() {
	rt.useMu.Lock()
	defer rt.useMu.Unlock()
	for c := range rt.calls {
		(*c)(errSandboxGone)
	}
	rt.calls = map[*context.CancelCauseFunc]struct{}{}
}

// opContext is the context a transition runs a nodelet call in: not the
// client's, because a client that hangs up mid-boot must not leave the record
// and the VM disagreeing.
func opContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
}

func (s *Server) nodeClient(ctx context.Context, sb db.Sandbox) (*nodeapi.Client, error) {
	cl, err := s.nodes.ClientWait(ctx, sb.Node)
	if err != nil {
		return nil, unavailable("its node %q can't be reached: %v", sb.Node, err)
	}
	return cl, nil
}

// setState stores a state the VM is now in, and says so on the event stream.
func (s *Server) setState(ctx context.Context, sb db.Sandbox, state, msg string, detail map[string]any) (db.Sandbox, error) {
	now := s.now()
	cur, err := s.store.UpdateSandbox(context.WithoutCancel(ctx), sb.ID, func(r *db.Sandbox) error {
		r.State, r.Error, r.UpdatedAt = state, msg, now
		if state == api.StateRunning {
			r.LastActiveAt = now
		}
		return nil
	})
	if err != nil {
		return sb, err
	}
	if msg != "" {
		if detail == nil {
			detail = map[string]any{}
		}
		detail["error"] = msg
	}
	s.emit(cur, "sandbox."+state, detail)
	return cur, nil
}

// acquire wakes a sandbox if it needs waking and holds it awake until release:
// every sub-API call does this for its whole length (docs/ARCHITECTURE.md,
// "Lifecycle"). sb is a record read before any lock; acquire reads it again.
func (s *Server) acquire(ctx context.Context, sb db.Sandbox) (db.Sandbox, func(), error) {
	rt := s.rt(sb.ID)
	rt.mu.Lock()
	cur, err := s.store.Sandbox(ctx, sb.ID)
	if err != nil {
		rt.mu.Unlock()
		if errors.Is(err, db.ErrNotFound) {
			return cur, nil, notFound("sandbox")
		}
		return cur, nil, err
	}
	if cur.State != api.StateRunning {
		if cur, err = s.wakeLocked(ctx, rt, cur); err != nil {
			rt.mu.Unlock()
			return cur, nil, err
		}
	}
	rt.begin()
	rt.mu.Unlock()
	var once sync.Once
	return cur, func() {
		once.Do(func() {
			now := s.now()
			rt.end(now)
			s.noteActive(cur.ID, now)
		})
	}, nil
}

// noteActive keeps last_active_at roughly current without a write per call.
func (s *Server) noteActive(id string, now time.Time) {
	s.store.UpdateSandbox(context.Background(), id, func(r *db.Sandbox) error {
		if now.Sub(r.LastActiveAt) < 10*time.Second {
			return errNoChange
		}
		r.LastActiveAt = now
		return nil
	})
}

var errNoChange = errors.New("no change")

// pauseLocked pauses a running sandbox. One already paused is left alone.
func (s *Server) pauseLocked(ctx context.Context, rt *sbrt, sb db.Sandbox, reason string) (db.Sandbox, error) {
	switch sb.State {
	case api.StatePaused:
		return sb, nil
	case api.StateRunning:
	default:
		return sb, conflict("a %s sandbox can't be paused", sb.State)
	}
	ctx, cancel := opContext(ctx)
	defer cancel()
	cl, err := s.nodeClient(ctx, sb)
	if err != nil {
		return sb, err
	}
	rt.endCalls()
	if sb, err = s.snapshotTransition(ctx, cl, sb, db.SnapPause, nodeapi.VMPaused); err != nil {
		return sb, err
	}
	s.log.Info("sandbox paused", "sandbox", sb.ID, "reason", reason, "snapshot", sb.Snapshot)
	return s.setState(ctx, sb, api.StatePaused, "", map[string]any{"reason": reason})
}

// snapshotTransition is the nodelet's half of a pause or a stop: each makes a
// snapshot of loam-api's naming (docs/NODE.md, "Phase 2"), recorded pending
// before the call and settled after it. A call that failed without an answer
// may have happened all the same: the VM says, if it can be asked.
func (s *Server) snapshotTransition(ctx context.Context, cl *nodeapi.Client, sb db.Sandbox, kind, to string) (db.Sandbox, error) {
	name, err := s.allocSnapshot(ctx, sb.ID)
	if err != nil {
		return sb, err
	}
	if err := s.beginSnapshot(ctx, sb, name, kind, "", kind == db.SnapPause); err != nil {
		return sb, err
	}
	var vm nodeapi.VM
	if kind == db.SnapPause {
		vm, err = cl.PauseInto(ctx, sb.ID, name)
	} else {
		vm, err = cl.StopInto(ctx, sb.ID, name)
	}
	if err != nil {
		var answer *nodeapi.Err
		if !errors.As(err, &answer) {
			if got, gerr := cl.VM(ctx, sb.ID); gerr == nil && got.State == to {
				vm, err = got, nil
			} else if gerr != nil {
				// Unknown: left pending, for reconciliation to settle.
				return sb, nodeErr(err)
			}
		}
		if err != nil {
			s.abandonSnapshot(ctx, name)
			return sb, nodeErr(err)
		}
	}
	return s.settleSnapshot(ctx, cl, sb, name, vm.Snapshot), nil
}

// stopLocked stops a sandbox, keeping its disk: from running, paused (its
// memory snapshot is dropped) or error. One already stopped is left alone.
func (s *Server) stopLocked(ctx context.Context, rt *sbrt, sb db.Sandbox, reason string) (db.Sandbox, error) {
	switch sb.State {
	case api.StateStopped:
		return sb, nil
	case api.StateCreating:
		return sb, conflict("the sandbox is still being created")
	}
	ctx, cancel := opContext(ctx)
	defer cancel()
	cl, err := s.nodeClient(ctx, sb)
	if err != nil {
		return sb, err
	}
	rt.endCalls()
	if sb, err = s.snapshotTransition(ctx, cl, sb, db.SnapStop, nodeapi.VMStopped); err != nil {
		return sb, err
	}
	s.log.Info("sandbox stopped", "sandbox", sb.ID, "reason", reason, "snapshot", sb.Snapshot)
	return s.setState(ctx, sb, api.StateStopped, "", map[string]any{"reason": reason})
}

// deleteLocked deletes a sandbox whole: its VM and its record, which takes
// its checkpoints' records with it. Their snapshots, and its pauses' and
// stops', go at the next collection (gc.go), unless a fork still starts from
// one. A node that no nodelet answers for has its VM deleted when it comes
// back (a tombstone); a nodelet that answers but fails keeps the record, so
// the delete can be tried again rather than leak a VM.
func (s *Server) deleteLocked(ctx context.Context, rt *sbrt, sb db.Sandbox, reason string) error {
	ctx, cancel := opContext(ctx)
	defer cancel()
	rt.endCalls()
	cl, err := s.nodes.Client(ctx, sb.Node)
	if err == nil {
		err = cl.Delete(ctx, sb.ID)
	}
	var answer *nodeapi.Err
	switch {
	case err == nil, nodeapi.IsCode(err, api.CodeNotFound):
	case errors.As(err, &answer):
		return nodeErr(err)
	case errors.Is(err, ErrNoNode) || ctx.Err() == nil:
		// No answer: the node is away. Its VM goes when it is back.
		if sb.Node != "" {
			if err := s.store.AddTombstone(ctx, sb.Node, sb.ID); err != nil {
				return err
			}
		}
	default:
		return unavailable("its node %q can't be reached: %v", sb.Node, err)
	}
	if err := s.store.DeleteSandbox(ctx, sb.ID); err != nil && !errors.Is(err, db.ErrNotFound) {
		return err
	}
	s.forgetRT(sb.ID)
	s.forgetMetrics(sb)
	s.log.Info("sandbox deleted", "sandbox", sb.ID, "reason", reason)
	s.emit(sb, "sandbox.deleted", map[string]any{"reason": reason})
	s.kickGC()
	return nil
}
