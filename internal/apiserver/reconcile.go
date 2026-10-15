package apiserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

// Reconciliation: the records are the truth of what should exist, the
// nodelets of what does. loam-api follows each nodelet's event stream
// (GET /v1/events: a VM's state changed other than by a request, say it
// crashed or its node drained; a snapshot reached the store), and on every
// (re)connect lists its VMs (GET /v1/vms) and asks after its unstored
// snapshots to catch up on what it missed. The stream is opened first and
// read after the list, so nothing falls between the two.
//
// A VM whose sandbox's record names another node is stale: the sandbox moved
// while this node was away. It is deleted, as is a VM with a tombstone (its
// sandbox was deleted while its node was away). A VM with no record and no
// tombstone is only logged: deleting what loam-api doesn't know of is not a
// thing to do on a guess.

// watchNodes starts a follower for every nodelet not followed yet, deletes
// the tombstoned VMs of the nodes that answer, and lets checkNodes look for
// lost ones. A follower runs until ctx ends, reconnecting by name, so a
// nodelet that restarts with a new address is found again.
func (s *Server) watchNodes(ctx context.Context) {
	nodes, err := s.nodes.List(ctx)
	if err != nil {
		s.log.Warn("listing nodelets failed", "err", err)
		return
	}
	s.mu.Lock()
	for _, nd := range nodes {
		if !s.watchers[nd.Name] {
			s.watchers[nd.Name] = true
			go s.followNode(ctx, nd.Name)
		}
	}
	s.mu.Unlock()
	for _, nd := range nodes {
		s.buryTombstones(ctx, nd.Name, s.nodes.client(nd.Addr))
	}
	s.checkNodes(ctx, nodes)
}

func (s *Server) followNode(ctx context.Context, name string) {
	backoff := time.Second
	for ctx.Err() == nil {
		connected, err := s.followOnce(ctx, name)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = time.Second
		}
		s.log.Info("nodelet event stream ended; reconnecting", "node", name, "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// followOnce follows node name's event stream until it ends. connected says
// whether it got as far as reconciling, so the next reconnect starts quick.
func (s *Server) followOnce(ctx context.Context, name string) (connected bool, err error) {
	cl, err := s.nodes.Client(ctx, name)
	if err != nil {
		return false, err
	}
	// No client timeout: the stream is meant to stay open. It is dropped when
	// it says nothing, not even a keepalive, for EventStreamIdle: a frozen
	// nodelet's connection stays open, and only a reconnect reconciles what
	// changed meanwhile.
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := time.AfterFunc(s.opts.EventStreamIdle, cancel)
	defer idle.Stop()
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, "http://"+cl.Addr+"/v1/events", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+cl.Token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, errors.New("nodelet events: " + resp.Status)
	}
	vms, err := cl.VMs(ctx)
	if err != nil {
		return false, err
	}
	s.reconcileNode(ctx, name, cl, vms)
	s.catchUpSnapshots(ctx, name, cl)
	s.buryTombstones(ctx, name, cl)

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var event string
	var data strings.Builder
	for sc.Scan() {
		idle.Reset(s.opts.EventStreamIdle)
		line := sc.Text()
		switch {
		case line == "":
			switch event {
			case nodeapi.EventVM:
				var vm nodeapi.VM
				if json.Unmarshal([]byte(data.String()), &vm) == nil && vm.ID != "" {
					go s.vmChanged(ctx, name, vm.ID)
				}
			case nodeapi.EventNode:
				var nd nodeapi.Node
				if json.Unmarshal([]byte(data.String()), &nd) == nil {
					s.nodes.markSeen(name, nd.Ready)
				}
			case nodeapi.EventSnapshot:
				var sn nodeapi.Snapshot
				if json.Unmarshal([]byte(data.String()), &sn) == nil && sn.ID != "" && sn.Stored {
					s.snapshotStored(ctx, sn)
				}
			}
			event = ""
			data.Reset()
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if sctx.Err() != nil && ctx.Err() == nil {
		return true, fmt.Errorf("nothing from the nodelet for %s", s.opts.EventStreamIdle)
	}
	return true, sc.Err()
}

// reconcileNode brings the records of sandboxes on node into line with the
// VMs it has, and deletes its stale VMs. A sandbox in a transition is
// skipped: the transition writes its own outcome.
func (s *Server) reconcileNode(ctx context.Context, node string, cl *nodeapi.Client, vms []nodeapi.VM) {
	byID := map[string]*nodeapi.VM{}
	for i := range vms {
		byID[vms[i].ID] = &vms[i]
	}
	all, err := s.store.Sandboxes(ctx, "")
	if err != nil {
		return
	}
	known := map[string]bool{}
	for _, sb := range all {
		known[sb.ID] = true
		if sb.Node != node {
			if byID[sb.ID] != nil {
				s.deleteStale(ctx, node, cl, sb.ID)
			}
			continue
		}
		rt := s.rt(sb.ID)
		if !rt.mu.TryLock() {
			continue
		}
		s.applyVM(ctx, cl, sb.ID, byID[sb.ID], true)
		rt.mu.Unlock()
	}
	tombs, _ := s.store.Tombstones(ctx, node)
	for _, t := range tombs {
		known[t] = true
	}
	for _, vm := range vms {
		if !known[vm.ID] {
			s.log.Warn("the node has a VM with no sandbox record", "node", node, "vm", vm.ID, "state", vm.State)
		}
	}
}

// deleteStale deletes node's VM of sandbox id if the record, read again under
// the sandbox's lock, names another node: the sandbox moved while node was
// away, and the copy here is out of date.
//
// What it can't do now it leaves a tombstone for: a sandbox in a transition
// (it may be moving here right now), or a delete that failed. The look
// that found the VM is a reconnect's, or an event's, and while the stream
// stays up neither comes again, so without one the stale copy would outlive
// the busy moment by as long as the stream does. buryTombstones takes it up
// on every watchNodes pass, and drops it without a delete if the sandbox has
// moved here by then.
func (s *Server) deleteStale(ctx context.Context, node string, cl *nodeapi.Client, id string) {
	rt := s.rt(id)
	if !rt.mu.TryLock() {
		s.leaveTombstone(ctx, node, id, "it is in a transition")
		return
	}
	defer rt.mu.Unlock()
	sb, err := s.store.Sandbox(ctx, id)
	if err != nil || sb.Node == node {
		return
	}
	if err := cl.Delete(ctx, id); err != nil && !nodeapi.IsCode(err, api.CodeNotFound) {
		s.leaveTombstone(ctx, node, id, err.Error())
		return
	}
	s.log.Info("deleted a stale VM: its sandbox is on another node", "node", node, "vm", id, "sandbox_node", sb.Node)
}

func (s *Server) leaveTombstone(ctx context.Context, node, id, why string) {
	if err := s.store.AddTombstone(context.WithoutCancel(ctx), node, id); err != nil {
		s.log.Error("could not leave a tombstone; the stale VM stays until its node reconnects", "node", node, "vm", id, "err", err)
		return
	}
	s.log.Info("could not delete a stale VM yet; left a tombstone for it", "node", node, "vm", id, "why", why)
}

// buryTombstones deletes the VMs of deleted or moved sandboxes on node, which
// answers now. A tombstone whose sandbox is back on node (it moved back) is
// dropped without a delete.
func (s *Server) buryTombstones(ctx context.Context, node string, cl *nodeapi.Client) {
	tombs, err := s.store.Tombstones(ctx, node)
	if err != nil {
		return
	}
	for _, id := range tombs {
		rt := s.rt(id)
		if !rt.mu.TryLock() {
			continue
		}
		sb, err := s.store.Sandbox(ctx, id)
		if err == nil && sb.Node == node {
			s.store.DeleteTombstone(ctx, node, id)
			rt.mu.Unlock()
			continue
		}
		err = cl.Delete(ctx, id)
		rt.mu.Unlock()
		if err != nil && !nodeapi.IsCode(err, api.CodeNotFound) {
			s.log.Warn("could not delete a tombstoned VM; it will be tried again", "node", node, "vm", id, "err", err)
			continue
		}
		s.store.DeleteTombstone(ctx, node, id)
		s.log.Info("deleted the VM of a sandbox deleted or moved while its node was away", "node", node, "vm", id)
	}
}

// catchUpSnapshots asks node after the snapshots it made that loam-api
// hasn't heard are stored: their events may have been missed. A pending
// record whose sandbox has no transition running is one whose call loam-api
// never heard the end of: the node says whether it exists.
func (s *Server) catchUpSnapshots(ctx context.Context, node string, cl *nodeapi.Client) {
	recs, err := s.store.SnapshotsOn(ctx, node)
	if err != nil {
		return
	}
	for _, rec := range recs {
		if rec.Stored || rec.State == db.SnapAbandoned {
			continue
		}
		if rec.State == db.SnapPending {
			s.settlePending(ctx, cl, rec)
			continue
		}
		if ns, err := cl.GetSnapshot(ctx, rec.ID); err == nil && ns.Stored {
			s.snapshotStored(ctx, ns)
		}
	}
}

func (s *Server) settlePending(ctx context.Context, cl *nodeapi.Client, rec db.Snapshot) {
	rt := s.rt(rec.Sandbox)
	if !rt.mu.TryLock() {
		return
	}
	defer rt.mu.Unlock()
	ns, err := cl.GetSnapshot(ctx, rec.ID)
	switch {
	case err == nil:
		s.finishSnapshot(ctx, rec.ID, &ns)
	case nodeapi.IsCode(err, api.CodeNotFound):
		s.abandonSnapshot(ctx, rec.ID)
	}
}

// vmChanged handles one event from a nodelet: it waits for any transition in
// progress, then asks the nodelet for the VM afresh, because by then the
// event may be stale.
func (s *Server) vmChanged(ctx context.Context, node, id string) {
	sb, err := s.store.Sandbox(ctx, id)
	if err != nil {
		return
	}
	// A fresh list, not the cache: it is also how loam-api sees a node that
	// is draining (placement.go, nodeGone).
	nodes, err := s.nodes.List(ctx)
	if err != nil {
		return
	}
	nd := findNode(nodes, node)
	if nd == nil {
		return
	}
	cl := s.nodes.client(nd.Addr)
	if sb.Node != node {
		s.deleteStale(ctx, node, cl, id)
		return
	}
	rt := s.rt(id)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if sb, err = s.store.Sandbox(ctx, id); err != nil || sb.Node != node {
		return
	}
	vm, err := cl.VM(ctx, id)
	switch {
	case err == nil:
		s.applyVM(ctx, cl, id, &vm, true)
	case nodeapi.IsCode(err, api.CodeNotFound):
		s.applyVM(ctx, cl, id, nil, true)
	}
}

// applyVM sets a record's state from its VM's, under the sandbox's lock, and
// records a newest snapshot of the VM's that loam-api didn't ask for (a pause
// while draining). No VM at all is an error, unless the sandbox has a stored
// snapshot to wake from: then it is paused or stopped per that snapshot, and
// wakes from it (docs/NODE.md, "A lost node"). endCalls says whether a state
// other than running ends the calls in flight; a caller that is about to wake
// the sandbox for one of them says no.
func (s *Server) applyVM(ctx context.Context, cl *nodeapi.Client, id string, vm *nodeapi.VM, endCalls bool) {
	sb, err := s.store.Sandbox(ctx, id)
	if err != nil {
		return
	}
	if vm != nil && vm.Snapshot != "" && vm.Snapshot != sb.Snapshot {
		if rec, err := s.store.Snapshot(ctx, vm.Snapshot); errors.Is(err, db.ErrNotFound) || (err == nil && rec.State != db.SnapReady) {
			if s.adoptSnapshot(ctx, cl, sb, vm.Snapshot) == nil {
				if cur, err := s.pointTo(ctx, id, vm.Snapshot); err == nil {
					sb = cur
					s.log.Info("recorded a snapshot its node made on its own", "sandbox", id, "snapshot", vm.Snapshot)
				}
			}
		}
	}
	state, msg := api.StateError, "its node has no VM for it"
	detail := map[string]any{"reason": "reconciled"}
	if vm != nil {
		state, msg = vm.State, vm.Error
		switch state {
		case nodeapi.VMRunning, nodeapi.VMPaused, nodeapi.VMStopped:
			msg = ""
		default:
			state = api.StateError
			if msg == "" {
				msg = "its VM failed"
			}
		}
	} else if snap, ok := s.newestSnapshot(ctx, sb); ok && snap.Stored && sb.State != api.StateCreating {
		state, msg = api.StatePaused, ""
		if !snap.Memory {
			state = api.StateStopped
		}
		if sb.State == api.StateRunning {
			detail = map[string]any{"reason": "vm_lost", "snapshot_at": snap.CreatedAt,
				"lost": "its node lost its VM: what it did after snapshot_at is lost, and it wakes from there"}
		}
	}
	if sb.State == state && sb.Error == msg {
		return
	}
	if sb.State == api.StateCreating && vm == nil {
		msg = "it was being created when loam-api lost track of it"
	}
	s.log.Warn("sandbox state reconciled from its node", "sandbox", id, "was", sb.State, "now", state, "error", msg)
	if state != api.StateRunning && endCalls {
		s.rt(id).endCalls()
	}
	s.setState(ctx, sb, state, msg, detail)
}
