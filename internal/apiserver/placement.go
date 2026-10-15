package apiserver

import (
	"context"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

// Placement across nodes (docs/NODE.md, "Phase 2: the snapshot store").
//
// A wake goes to the sandbox's own node when that node is ready and has room:
// its VM is there, paused or stopped, and resumes from local disk. Otherwise,
// if the sandbox's newest snapshot is stored, it moves: any ready node with
// room creates the VM under the sandbox's own ID from that snapshot, and the
// VM on the old node is deleted, now if the node answers, else when it comes
// back (a tombstone, and reconciliation of VMs whose record names another
// node). Else it can't be woken: capacity when the node is merely full,
// sandbox_unavailable otherwise.
//
// A node that doesn't answer is waited for (Nodes.Wait) rather than left at
// once: a nodelet that restarts is gone for seconds, and the VM it has is the
// fastest wake. A node is gone, and its sandboxes move without waiting, when
// it has not answered for NodeLostAfter (lost), or when it was draining the
// last time it answered (its VMs were paused into stored snapshots as it
// went). A lost node's running sandboxes are also set paused or stopped,
// per their newest snapshot, so that the next call wakes them elsewhere;
// whatever they did after that snapshot is lost, and the event says so.

// nodeLost says whether node, which isn't answering now, has not answered for
// NodeLostAfter (counted from this process's start for one never heard from).
func (s *Server) nodeLost(node string) bool {
	since := s.started
	if sn, ok := s.nodes.lastSeen(node); ok && sn.at.After(since) {
		since = sn.at
	}
	return s.now().Sub(since) >= s.opts.NodeLostAfter
}

// nodeGone says whether node, which isn't answering now, can be left: it is
// lost, or it was draining when it last answered.
func (s *Server) nodeGone(node string) bool {
	if sn, ok := s.nodes.lastSeen(node); ok && !sn.ready {
		return true
	}
	return s.nodeLost(node)
}

// wakeLocked brings a paused, stopped or failed sandbox to running, on its
// node or another.
func (s *Server) wakeLocked(ctx context.Context, rt *sbrt, sb db.Sandbox) (db.Sandbox, error) {
	switch sb.State {
	case api.StateRunning:
		return sb, nil
	case api.StateCreating:
		return sb, unavailable("the sandbox is still being created")
	}
	ctx, cancel := opContext(ctx)
	defer cancel()
	nodes, err := s.nodes.List(ctx)
	if err != nil {
		return sb, unavailable("the nodes can't be listed: %v", err)
	}
	home := findNode(nodes, sb.Node)
	if home == nil && !s.nodeGone(sb.Node) {
		if _, err := s.nodes.ClientWait(ctx, sb.Node); err == nil {
			if nodes, err = s.nodes.List(ctx); err != nil {
				return sb, unavailable("the nodes can't be listed: %v", err)
			}
			home = findNode(nodes, sb.Node)
		}
		if home == nil {
			return sb, unavailable("its node %q can't be reached", sb.Node)
		}
	}
	if err := checkBrokers(nodes, sb); err != nil {
		return sb, err
	}
	from := sb.State
	homeHasVM := home != nil
	if home != nil {
		cl := s.nodes.client(home.Addr)
		// A node without the broker CA can't run a sandbox that was given
		// secrets while it slept there: it moves, like one whose node is full.
		if home.fits(sb.Resources.MemoryMiB) && home.canRun(sb) {
			wake, pol, err := s.bootOf(ctx, sb)
			if err != nil {
				return sb, err
			}
			_, err = cl.Resume(ctx, sb.ID, nodeapi.Resume{Wake: wake, Policy: pol})
			if err == nil {
				rt.touch(s.now())
				s.log.Info("sandbox woken", "sandbox", sb.ID, "from", from)
				return s.setState(ctx, sb, api.StateRunning, "", map[string]any{"from": from})
			}
			s.log.Warn("wake on its node failed", "sandbox", sb.ID, "node", sb.Node, "from", from, "err", err)
			switch {
			case nodeapi.IsCode(err, api.CodeNotFound):
				homeHasVM = false // its node lost it: start it from its snapshot, there or anywhere
			case nodeapi.IsCode(err, api.CodeCapacity), nodeapi.IsCode(err, codeNotReady):
			default:
				e := policyErr(err)
				if e.code == api.CodeInvalid || e.code == api.CodeUnsupported {
					return sb, e
				}
				return sb, unavailable("the sandbox could not be woken: %s", e.msg)
			}
		} else if vm, err := cl.VM(ctx, sb.ID); err == nil && vm.State == nodeapi.VMRunning {
			// The node runs it although the record says otherwise (an answer
			// lost on the way): never start a second copy elsewhere.
			rt.touch(s.now())
			return s.setState(ctx, sb, api.StateRunning, "", map[string]any{"from": from})
		}
	}
	candidates := nodes
	if homeHasVM {
		candidates = without(nodes, sb.Node)
	}
	return s.moveLocked(ctx, rt, sb, candidates, home != nil)
}

// codeNotReady is a nodelet refusing a create or a resume while it drains.
const codeNotReady = "not_ready"

// moveLocked wakes sb on one of candidates from its newest snapshot, which
// must be stored. homeAnswers says whether its old node answers, so its VM
// there can be deleted now.
func (s *Server) moveLocked(ctx context.Context, rt *sbrt, sb db.Sandbox, candidates []NodeInfo, homeAnswers bool) (db.Sandbox, error) {
	snap, ok := s.newestSnapshot(ctx, sb)
	switch {
	case !ok:
		if homeAnswers {
			return sb, capacityErr("its node has no room for it now, and it has no snapshot to wake from elsewhere")
		}
		return sb, unavailable("its node %q is gone, and it has no snapshot to wake from elsewhere", sb.Node)
	case !snap.Stored:
		if homeAnswers {
			return sb, capacityErr("its node has no room for it now, and its newest snapshot isn't stored yet")
		}
		return sb, unavailable("its node %q is gone, and its newest snapshot never reached the store", sb.Node)
	}
	target := placePreferring(runnable(candidates, sb), sb.Resources.MemoryMiB, sb.Node)
	if target == "" {
		return sb, capacityErr("no node has room for the sandbox now")
	}
	wake, pol, err := s.bootOf(ctx, sb)
	if err != nil {
		return sb, err
	}
	cl := s.nodes.client(findNode(candidates, target).Addr)
	req := nodeapi.CreateVM{ID: sb.ID, Template: s.templateOf(sb), Resources: sb.Resources,
		From: &nodeapi.FromSnapshot{Snapshot: snap.ID}, Wake: wake, Policy: pol}
	_, err = cl.Create(ctx, req)
	if nodeapi.IsCode(err, api.CodeAlreadyExists) && target != sb.Node {
		// A VM left there by an earlier stay, which the record doesn't name.
		if derr := cl.Delete(ctx, sb.ID); derr == nil || nodeapi.IsCode(derr, api.CodeNotFound) {
			_, err = cl.Create(ctx, req)
		}
	}
	if err != nil {
		s.log.Warn("wake on another node failed", "sandbox", sb.ID, "node", target, "snapshot", snap.ID, "err", err)
		e := policyErr(err)
		switch e.code {
		case api.CodeCapacity, api.CodeInvalid, api.CodeUnsupported:
		default:
			e = unavailable("the sandbox could not be woken on another node: %s", e.msg)
		}
		return sb, e
	}
	old, from := sb.Node, sb.State
	now := s.now()
	cur, err := s.store.UpdateSandbox(context.WithoutCancel(ctx), sb.ID, func(r *db.Sandbox) error {
		r.Node, r.State, r.Error, r.UpdatedAt, r.LastActiveAt = target, api.StateRunning, "", now, now
		return nil
	})
	if err != nil {
		// The record can't say where it is: take the new VM away again rather
		// than run one the records don't know of.
		cl.Delete(context.WithoutCancel(ctx), sb.ID)
		return sb, err
	}
	rt.touch(now)
	s.log.Info("sandbox moved", "sandbox", sb.ID, "from_node", old, "to_node", target, "snapshot", snap.ID, "was", from)
	s.emit(cur, "sandbox.running", map[string]any{"from": from, "moved": true})
	if old != target {
		s.dropOldVM(ctx, old, sb.ID, homeAnswers)
	}
	return cur, nil
}

// dropOldVM deletes a moved sandbox's VM on the node it left, or leaves a
// tombstone for reconciliation to delete it when the node is back.
func (s *Server) dropOldVM(ctx context.Context, node, id string, answers bool) {
	if answers {
		if cl, err := s.nodes.Client(ctx, node); err == nil {
			err := cl.Delete(ctx, id)
			if err == nil || nodeapi.IsCode(err, api.CodeNotFound) {
				return
			}
		}
	}
	if err := s.store.AddTombstone(context.WithoutCancel(ctx), node, id); err != nil {
		s.log.Error("could not leave a tombstone; the old VM stays until its node reconnects", "sandbox", id, "node", node, "err", err)
	}
}

// templateOf is what a node builds the sandbox's disk from: its template's
// image, or the image it was made from, under no template name (docs/NODE.md).
func (s *Server) templateOf(sb db.Sandbox) nodeapi.Template {
	if sb.Image != "" {
		return nodeapi.Template{Image: sb.Image}
	}
	t := nodeapi.Template{Name: sb.Template}
	if tpl, ok := s.opts.Templates[sb.Template]; ok {
		t.Image = tpl.Image
	}
	return t
}

// checkNodes looks at every node a sandbox record names: a running sandbox
// whose node is lost is set paused or stopped per its newest stored snapshot,
// so that the next call wakes it elsewhere. One with no stored snapshot is
// left as it is: it can't be woken anywhere until its node is back.
func (s *Server) checkNodes(ctx context.Context, nodes []NodeInfo) {
	all, err := s.store.Sandboxes(ctx, "")
	if err != nil {
		return
	}
	for _, sb := range all {
		if sb.State != api.StateRunning || findNode(nodes, sb.Node) != nil || !s.nodeLost(sb.Node) {
			continue
		}
		s.loseRunning(ctx, sb)
	}
}

func (s *Server) loseRunning(ctx context.Context, sb db.Sandbox) {
	rt := s.rt(sb.ID)
	if !rt.mu.TryLock() {
		return
	}
	defer rt.mu.Unlock()
	cur, err := s.store.Sandbox(ctx, sb.ID)
	if err != nil || cur.State != api.StateRunning || cur.Node != sb.Node {
		return
	}
	snap, ok := s.newestSnapshot(ctx, cur)
	if !ok || !snap.Stored {
		s.log.Debug("a running sandbox's node is lost and it has no stored snapshot to wake from", "sandbox", cur.ID, "node", cur.Node)
		return
	}
	state := api.StatePaused
	if !snap.Memory {
		state = api.StateStopped
	}
	rt.endCalls()
	s.log.Warn("sandbox's node is lost; it will wake elsewhere from its newest snapshot", "sandbox", cur.ID, "node", cur.Node, "snapshot", snap.ID, "state", state)
	s.setState(ctx, cur, state, "", map[string]any{"reason": "node_lost", "snapshot_at": snap.CreatedAt,
		"lost": "its node stopped answering: what it did after snapshot_at is lost, and it wakes from there"})
}
