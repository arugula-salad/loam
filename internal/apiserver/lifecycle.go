package apiserver

import (
	"context"
	"time"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/pkg/api"
)

// The lifecycle rules, after sandpit's (engine/lifecycle_rules.go,
// engine/leases.go, docs/lifecycle.md):
//
//   - Idle: a running sandbox with no call in flight for idle.timeout_s, whose
//     agent also reports no attached session, no task, nothing busy and no
//     byte through a session, file operation or port stream for as long, is
//     paused or stopped as idle.action says; `none` leaves it running. The
//     rule is read from the record on every pass, so a PATCH reaches a running
//     sandbox within a tick.
//   - Deadline: once deadline.at has passed, unless protected, the action is
//     taken whatever the sandbox is doing. pause and stop then clear the
//     deadline (it is spent, so waking the sandbox doesn't stop it again a
//     tick later); delete is a delete. Protection holds any action off without
//     forgetting the deadline, and clearing it hands the sandbox straight back.
//     The loop runs once at start, so a deadline that passed while loam-api
//     was down has still passed.
//
// Each pass reads the record list without a lock and decides again on a fresh
// record under the sandbox's transition lock, taken with TryLock: a sandbox in
// the middle of a transition is looked at again on the next tick.
//
// Single replica (phase 1): in-flight counts are this process's, so this loop
// must be the only one.

// nodeWatchEvery is how often the set of nodelets is looked at for new ones
// to follow, tombstoned VMs to delete (reconcile.go) and lost nodes
// (placement.go).
const nodeWatchEvery = 10 * time.Second

// eventsKept is how long the events table keeps an event.
const eventsKept = 24 * time.Hour

func (s *Server) runLifecycle(ctx context.Context) {
	s.watchNodes(ctx)
	s.lifecycleTick(ctx)
	tick := time.NewTicker(s.opts.LifecycleEvery)
	defer tick.Stop()
	lastWatch, lastPrune := time.Now(), time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		s.lifecycleTick(ctx)
		if time.Since(lastWatch) >= nodeWatchEvery {
			s.watchNodes(ctx)
			lastWatch = time.Now()
		}
		if time.Since(lastPrune) >= time.Hour {
			if err := s.store.PruneEvents(ctx, s.now().Add(-eventsKept)); err != nil {
				s.log.Warn("pruning events failed", "err", err)
			}
			before := s.now().AddDate(0, 0, -trafficKeptDays).Format(dayFormat)
			if err := s.store.PruneTraffic(ctx, before); err != nil {
				s.log.Warn("pruning traffic failed", "err", err)
			}
			lastPrune = time.Now()
		}
	}
}

// lifecycleTick is one pass of the idle and deadline rules over every sandbox.
func (s *Server) lifecycleTick(ctx context.Context) {
	all, err := s.store.Sandboxes(ctx, "")
	if err != nil {
		s.log.Warn("lifecycle pass skipped: the records can't be read", "err", err)
		return
	}
	now := s.now()
	for _, sb := range all {
		if deadlinePassed(sb, now) {
			s.passDeadline(ctx, sb)
			continue
		}
		if sb.State == api.StateRunning {
			s.applyIdle(ctx, sb, now)
		}
	}
}

func deadlinePassed(sb db.Sandbox, now time.Time) bool {
	return sb.Deadline != nil && !sb.Protected && !now.Before(sb.Deadline.At)
}

// applyIdle pauses or stops sb if it has been idle for its timeout. The cheap
// test (no call in flight here for that long) comes first; only then is the
// agent asked.
func (s *Server) applyIdle(ctx context.Context, sb db.Sandbox, now time.Time) {
	if sb.Idle.Action == api.ActionNone {
		return
	}
	timeout := time.Duration(sb.Idle.TimeoutS) * time.Second
	rt := s.rt(sb.ID)
	if idle, ok := rt.idleFor(now); !ok || idle < timeout {
		return
	}
	actx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cl, err := s.nodes.Client(actx, sb.Node)
	if err != nil {
		return
	}
	act, err := cl.Activity(actx, sb.ID)
	if err != nil || act.SessionsAttached > 0 || act.Tasks > 0 || act.Busy ||
		time.Duration(act.IdleMS)*time.Millisecond < timeout {
		return
	}
	if !rt.mu.TryLock() {
		return
	}
	defer rt.mu.Unlock()
	// Again under the lock, on a fresh record: a call may have begun, the
	// rule may have changed, a transition may have run.
	cur, err := s.store.Sandbox(ctx, sb.ID)
	if err != nil || cur.State != api.StateRunning {
		return
	}
	if idle, ok := rt.idleFor(s.now()); !ok || idle < time.Duration(cur.Idle.TimeoutS)*time.Second {
		return
	}
	switch cur.Idle.Action {
	case api.ActionPause:
		_, err = s.pauseLocked(ctx, rt, cur, "idle")
	case api.ActionStop:
		_, err = s.stopLocked(ctx, rt, cur, "idle")
	default:
		return
	}
	if err != nil {
		s.log.Warn("idle action failed; the sandbox is left running", "sandbox", cur.ID, "action", cur.Idle.Action, "err", err)
	}
}

// passDeadline takes a passed deadline's action.
func (s *Server) passDeadline(ctx context.Context, sb db.Sandbox) {
	rt := s.rt(sb.ID)
	if !rt.mu.TryLock() {
		return
	}
	defer rt.mu.Unlock()
	cur, err := s.store.Sandbox(ctx, sb.ID)
	if err != nil || !deadlinePassed(cur, s.now()) {
		return
	}
	action := cur.Deadline.Action
	s.log.Info("deadline passed", "sandbox", cur.ID, "at", cur.Deadline.At, "action", action)
	switch action {
	case api.ActionDelete:
		if err := s.deleteLocked(ctx, rt, cur, "deadline"); err != nil {
			s.log.Error("deleting a sandbox at its deadline failed; it will be tried again", "sandbox", cur.ID, "err", err)
		}
		return
	case api.ActionPause:
		if cur.State == api.StateRunning {
			if _, err := s.pauseLocked(ctx, rt, cur, "deadline"); err != nil {
				// A deadline acts whatever the guest is doing: one that can't be
				// paused is stopped instead, rather than run past its deadline.
				s.log.Warn("pause at the deadline failed; stopping it instead", "sandbox", cur.ID, "err", err)
				if _, err := s.stopLocked(ctx, rt, cur, "deadline"); err != nil {
					s.log.Error("stop at the deadline failed; it will be tried again", "sandbox", cur.ID, "err", err)
					return
				}
			}
		}
	case api.ActionStop:
		if cur.State == api.StateRunning || cur.State == api.StatePaused {
			if _, err := s.stopLocked(ctx, rt, cur, "deadline"); err != nil {
				s.log.Error("stop at the deadline failed; it will be tried again", "sandbox", cur.ID, "err", err)
				return
			}
		}
	}
	// Spent: cleared, so a wake isn't undone on the next tick.
	s.store.UpdateSandbox(ctx, cur.ID, func(r *db.Sandbox) error {
		r.Deadline, r.UpdatedAt = nil, s.now()
		return nil
	})
}
