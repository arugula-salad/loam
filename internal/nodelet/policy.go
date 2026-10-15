package nodelet

import (
	"context"
	"net/http"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/internal/nodelet/network"
	"github.com/arugula-salad/loam/pkg/api"
)

// compilePolicy checks a VM's policy (docs/NODE.md, "Phase 3: the network
// boundary"): one that doesn't compile is invalid, and one with secrets is
// unsupported on a nodelet without the broker CA, which could only send them
// on without their values or not at all.
func (n *Nodelet) compilePolicy(p nodeapi.Policy) (*network.Policy, error) {
	pol, err := network.Compile(p)
	if err != nil {
		return nil, invalid("policy: %v", err)
	}
	if len(pol.Secrets()) > 0 && !n.broker() {
		return nil, errorf(http.StatusNotImplemented, api.CodeUnsupported, "node %s has no broker CA, so it can't run a VM with secrets", n.opts.NodeName)
	}
	return pol, nil
}

// SetPolicy replaces a running VM's policy (docs/NODE.md, PUT
// /v1/vms/{id}/policy). A VM that isn't running gets its policy at its next
// resume, so this one is a conflict: the nodelet holds no policy for it.
func (n *Nodelet) SetPolicy(ctx context.Context, id string, p nodeapi.Policy) error {
	pol, err := n.compilePolicy(p)
	if err != nil {
		return err
	}
	v, err := n.lock(id)
	if err != nil {
		return err
	}
	defer v.mu.Unlock()
	if v.m == nil {
		return conflict("VM %s is %s: it gets its policy when it resumes", id, n.recordOf(v).State)
	}
	return n.replacePolicy(ctx, v, pol)
}

// replacePolicy puts pol in place for v, which is running. The caller holds
// v.mu. If it can't be, the VM is stopped: it would otherwise run with less
// than its policy.
func (n *Nodelet) replacePolicy(ctx context.Context, v *vm, pol *network.Policy) error {
	if !v.tap {
		return nil // no NIC: nothing to restrict
	}
	err := n.net.SetPolicy(ctx, n.recordOf(v).NetIndex, v.id, pol)
	if err == nil {
		return nil
	}
	n.log.Error("a running VM's new policy could not be put in place; the VM is stopped", "vm", v.id, "err", err)
	n.kill(v)
	n.update(v, func(r *record) { r.State, r.BootedAt, r.MemParent = nodeapi.VMStopped, nil, false })
	n.events.publish(event{nodeapi.EventVM, n.viewOf(v)})
	return unavailable("the new policy of %s could not be put in place, so it was stopped: %v", v.id, err)
}

// broker reports whether this nodelet can broker secrets.
func (n *Nodelet) broker() bool { return n.opts.BrokerCA != "" }
