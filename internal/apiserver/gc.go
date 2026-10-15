package apiserver

import (
	"context"
	"errors"
	"time"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

// The collection of snapshots and layers (docs/NODE.md, "Phase 2: the
// snapshot store"). Snapshots are deleted when no record needs them
// (db.Store.GarbageSnapshots says which); a layer is deleted from the store
// once no snapshot record references it. Both are idempotent, and anything
// that fails is left for the next pass, which runs every GCEvery and whenever
// something kicks it (a delete, a stored snapshot).
//
// A snapshot is deleted through the node that made it, whose copy is local
// and whose upload may still run: a nodelet cancels an upload before it
// answers the delete, so the snapshot's own layer isn't written after it is
// collected. Only when that node is gone (lost, or drained and gone) is a
// stored snapshot deleted through any other node, which deletes the store's
// copy; an unstored one waits for its node. A layer is deleted through any
// ready node: they share the store.
//
// Single replica, as the rest of loam-api's background work: two collectors
// would only race each other's deletes, which are idempotent, but there is no
// need for them.

func (s *Server) kickGC() {
	select {
	case s.gcKick <- struct{}{}:
	default:
	}
}

func (s *Server) runGC(ctx context.Context) {
	tick := time.NewTicker(s.opts.GCEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-s.gcKick:
		}
		s.collect(ctx)
	}
}

// collect is one pass: garbage snapshots, then unreferenced layers.
func (s *Server) collect(ctx context.Context) {
	garbage, err := s.store.GarbageSnapshots(ctx)
	if err != nil {
		s.log.Warn("collection skipped: the snapshot records can't be read", "err", err)
		return
	}
	nodes, err := s.nodes.List(ctx)
	if err != nil {
		s.log.Warn("collection skipped: the nodes can't be listed", "err", err)
		return
	}
	for _, g := range garbage {
		cl := s.snapshotDeleter(nodes, g)
		if cl == nil {
			continue
		}
		if err := cl.DeleteSnapshot(ctx, g.ID); err != nil && !nodeapi.IsCode(err, api.CodeNotFound) {
			s.log.Warn("could not delete a snapshot; it will be tried again", "snapshot", g.ID, "node", g.Node, "err", err)
			continue
		}
		if err := s.store.DeleteSnapshot(ctx, g.ID); err != nil && !errors.Is(err, db.ErrNotFound) {
			s.log.Warn("could not delete a snapshot's record", "snapshot", g.ID, "err", err)
			continue
		}
		s.log.Info("snapshot deleted", "snapshot", g.ID, "kind", g.Kind, "stored", g.Stored)
	}
	s.collectLayers(ctx, nodes)
}

// snapshotDeleter is the client to delete g through, or nil to wait.
func (s *Server) snapshotDeleter(nodes []NodeInfo, g db.Snapshot) *nodeapi.Client {
	if nd := findNode(nodes, g.Node); nd != nil {
		return s.nodes.client(nd.Addr)
	}
	if !g.Stored || !s.nodeGone(g.Node) {
		return nil
	}
	return anyReady(s, nodes)
}

func anyReady(s *Server, nodes []NodeInfo) *nodeapi.Client {
	for _, nd := range nodes {
		if nd.Ready {
			return s.nodes.client(nd.Addr)
		}
	}
	return nil
}

// collectLayers deletes the layers no record references. Each is taken out of
// the layer set first, under the store's check that nothing references it,
// and put back if the nodelet can't delete it: a crash in between leaks an
// object rather than deleting one a record needs.
func (s *Server) collectLayers(ctx context.Context, nodes []NodeInfo) {
	layers, err := s.store.UnreferencedLayers(ctx)
	if err != nil || len(layers) == 0 {
		return
	}
	cl := anyReady(s, nodes)
	if cl == nil {
		return
	}
	for _, l := range layers {
		if err := s.store.DeleteLayer(ctx, l); err != nil {
			continue // referenced again, or gone already
		}
		if err := cl.DeleteLayer(ctx, l); err != nil && !nodeapi.IsCode(err, api.CodeNotFound) {
			s.log.Warn("could not delete a layer; it will be tried again", "layer", l, "err", err)
			if err := s.store.AddLayers(context.WithoutCancel(ctx), []string{l}); err != nil {
				s.log.Error("a layer left the set without being deleted; it leaks", "layer", l, "err", err)
			}
			continue
		}
		s.log.Info("layer deleted", "layer", l)
	}
}
