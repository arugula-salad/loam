package apiserver

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/netpolicy"
	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

// The network boundary (docs/API.md, "Network" and "Secrets"; docs/NODE.md,
// "Phase 3"). loam-api keeps a sandbox's rules as given and the names of its
// secrets; its nodelet enforces them. Every create, resume and restore hands
// the nodelet the sandbox's policy, its rules expanded and its secrets'
// values opened, and a change reaches an awake sandbox at once
// (pushPolicyLocked).

// checkNetwork refuses a rule list the nodelet couldn't enforce. The list is
// checked as given (at most netpolicy.MaxRules) and expanded too, as the
// nodelet gets it, so that an include can't take it over the limit there.
func checkNetwork(n api.Network) error {
	if err := netpolicy.Validate(n.Egress); err != nil {
		return invalid("network.egress: %v", err)
	}
	exp, err := netpolicy.Expand(n.Egress)
	if err == nil {
		err = netpolicy.Validate(exp)
	}
	if err != nil {
		return invalid("network.egress, with its includes expanded: %v", err)
	}
	return nil
}

// checkSecretNames checks a sandbox's secrets against its env: each a secret
// name, once, and none an env name, since the secret's placeholder is put in
// the env under it. Whether each exists the store checks as it writes the
// record (db.ErrNoSecret).
func checkSecretNames(names []string, env map[string]string) error {
	seen := map[string]bool{}
	for _, n := range names {
		switch {
		case !validSecretName(n):
			return invalid("secrets: %q is not a secret name", n)
		case seen[n]:
			return invalid("secrets: %q is named twice", n)
		}
		if _, ok := env[n]; ok {
			return invalid("secrets: %q is also in env; the secret's placeholder goes in the env under its name", n)
		}
		seen[n] = true
	}
	return nil
}

// policyOf is sb's policy for its nodelet: its rules with their includes
// expanded, and every secret it names with its value. It fails rather than
// leave a secret out: a VM never runs with less than its policy.
func (s *Server) policyOf(ctx context.Context, sb db.Sandbox) (nodeapi.Policy, error) {
	egress, err := netpolicy.Expand(sb.Network.Egress)
	if err != nil {
		return nodeapi.Policy{}, invalid("the sandbox's network.egress: %v", err)
	}
	if egress == nil {
		egress = []api.Rule{}
	}
	pol := nodeapi.Policy{Egress: egress, Secrets: []nodeapi.PolicySecret{}}
	if len(sb.Secrets) > 0 && s.sealer == nil {
		return pol, noSecretKey()
	}
	for _, name := range sb.Secrets {
		rec, err := s.store.Secret(ctx, sb.Project, name)
		if err != nil {
			return pol, fmt.Errorf("secret %s: %w", name, err)
		}
		value, err := s.sealer.open(sb.Project, name, rec.Value)
		if err != nil {
			// The value is never in the message, only that it couldn't be had.
			return pol, fmt.Errorf("secret %s can't be opened (was the secret key changed?)", name)
		}
		pol.Secrets = append(pol.Secrets, nodeapi.PolicySecret{Name: name, Value: string(value), Hosts: rec.Hosts,
			Placeholder: rec.Placeholder, Inject: rec.Inject})
	}
	return pol, nil
}

// wakeOf is what the agent gets at every boot and resume (docs/NODE.md): the
// sandbox's env with each of its secrets' placeholders under the secret's
// name, never a value.
func (s *Server) wakeOf(sb db.Sandbox, pol nodeapi.Policy) nodeapi.Wake {
	env := maps.Clone(sb.Env)
	if env == nil {
		env = map[string]string{}
	}
	for _, sec := range pol.Secrets {
		env[sec.Name] = sec.Placeholder
	}
	return nodeapi.Wake{Sandbox: sb.ID, Name: sb.Name, Project: sb.Project, Hostname: sb.Name,
		Env: env, User: "loam", Time: s.now()}
}

// bootOf is a create's, a resume's or a restore's wake and policy together.
func (s *Server) bootOf(ctx context.Context, sb db.Sandbox) (nodeapi.Wake, nodeapi.Policy, error) {
	pol, err := s.policyOf(ctx, sb)
	if err != nil {
		return nodeapi.Wake{}, pol, err
	}
	return s.wakeOf(sb, pol), pol, nil
}

// pushPolicyLocked hands an awake sandbox's VM its policy now, under the
// sandbox's transition lock. A VM that isn't running (409) gets it at its
// next resume, which reads the record afresh; so does a sandbox that isn't
// running here.
func (s *Server) pushPolicyLocked(ctx context.Context, sb db.Sandbox) error {
	if sb.State != api.StateRunning {
		return nil
	}
	ctx, cancel := opContext(ctx)
	defer cancel()
	pol, err := s.policyOf(ctx, sb)
	if err != nil {
		return err
	}
	cl, err := s.nodeClient(ctx, sb)
	if err != nil {
		return err
	}
	err = cl.SetPolicy(ctx, sb.ID, pol)
	var answer *nodeapi.Err
	switch {
	case err == nil, nodeapi.IsCode(err, api.CodeConflict):
		return nil
	case errors.As(err, &answer):
		return err
	}
	return &unconfirmed{err}
}

// unconfirmed is a policy sent to a nodelet that didn't answer: it may or
// may not be in place.
type unconfirmed struct{ err error }

func (u *unconfirmed) Error() string { return "the node didn't answer: " + u.err.Error() }
func (u *unconfirmed) Unwrap() error { return u.err }

// policyErr is what a nodelet's refusal of a policy means to a client. Its
// invalid and unsupported are the client's to fix, not the sandbox's being
// unavailable.
func policyErr(err error) *apiErr {
	var e *nodeapi.Err
	if errors.As(err, &e) {
		switch e.Code {
		case api.CodeInvalid:
			return invalid("the node refused the policy: %s", e.Message)
		case api.CodeUnsupported:
			return unsupported("the node can't enforce the policy: %s", e.Message)
		}
	}
	return nodeErr(err)
}

// canRun says whether the node can run sb: one with secrets needs a node
// with the broker CA.
func (nd NodeInfo) canRun(sb db.Sandbox) bool { return len(sb.Secrets) == 0 || nd.Broker }

// runnable keeps the nodes that can run sb.
func runnable(nodes []NodeInfo, sb db.Sandbox) []NodeInfo {
	if len(sb.Secrets) == 0 {
		return nodes
	}
	var out []NodeInfo
	for _, nd := range nodes {
		if nd.Broker {
			out = append(out, nd)
		}
	}
	return out
}

// checkBrokers is unsupported when sb has secrets and none of nodes (every
// node that answers) has the broker CA: no node could run it, now or after a
// wait, which capacity would promise.
func checkBrokers(nodes []NodeInfo, sb db.Sandbox) error {
	if len(sb.Secrets) > 0 && len(runnable(nodes, sb)) == 0 {
		return unsupported("no node has the broker CA, so none can run a sandbox with secrets")
	}
	return nil
}

func (s *Server) networkDefaults(w http.ResponseWriter, r *http.Request, c caller) {
	writeJSON(w, http.StatusOK, api.Network{Egress: netpolicy.Defaults()})
}
