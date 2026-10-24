package network

import (
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// ttlGrace is how long past a record's TTL the address stays usable: clients
// cache answers a little longer than they should, and a connection attempt can
// trail the lookup that authorised it.
const ttlGrace = 60 * time.Second

// maxLearned bounds the remembered addresses per VM. At the bound expired
// ones are swept; if none have expired, new addresses are not learned (and so
// not reachable on a port without a name) rather than letting a guest grow
// the table without limit.
const maxLearned = 4096

// maxFlows bounds one VM's concurrent proxied connections: each costs the
// nodelet two file descriptors, and they are shared with every other VM.
const maxFlows = 2048

// Enforcer holds what the DNS forwarder and the proxy share: each running
// VM's policy, keyed by the address the VM is known by, the addresses its
// allowed lookups produced, and its live proxied connections.
type Enforcer struct {
	log *slog.Logger
	now func() time.Time

	mu  sync.Mutex
	vms map[netip.Addr]*vmState
}

type vmState struct {
	id     string
	policy *Policy
	// learned maps a destination to the names that resolved to it and when
	// each stops counting. Names are kept so a policy change can revoke
	// precisely.
	learned map[netip.Addr]map[string]time.Time
	flows   map[*flow]struct{}
}

// flow is one live proxied connection, the name that authorised it ("" for
// none) and the port it went to.
type flow struct {
	via  string
	port int
	conn io.Closer
}

func NewEnforcer(log *slog.Logger) *Enforcer {
	return &Enforcer{log: log, now: time.Now, vms: map[netip.Addr]*vmState{}}
}

// Set installs the policy of the VM id at src. Addresses learned through
// names the new policy no longer resolves are forgotten, and connections it
// no longer allows are closed, so tightening a policy takes effect on live
// traffic.
func (e *Enforcer) Set(src netip.Addr, id string, p *Policy) {
	e.mu.Lock()
	vs := e.vms[src]
	var doomed []*flow
	if vs != nil && vs.id != id {
		// Another VM now owns this address; nothing of the old one carries over.
		for f := range vs.flows {
			doomed = append(doomed, f)
		}
		vs = nil
	}
	if vs == nil {
		vs = &vmState{id: id, learned: map[netip.Addr]map[string]time.Time{}, flows: map[*flow]struct{}{}}
		e.vms[src] = vs
	}
	oldPolicy := vs.policy
	vs.policy = p
	for dst, names := range vs.learned {
		for n := range names {
			if !p.Resolves(n) {
				delete(names, n)
			}
		}
		if len(names) == 0 {
			delete(vs.learned, dst)
		}
	}
	for f := range vs.flows {
		if !p.permits(f.via, f.port) || (f.port == 443 && oldPolicy.Brokers(f.via) != p.Brokers(f.via)) {
			doomed = append(doomed, f)
			delete(vs.flows, f)
		}
	}
	e.mu.Unlock()
	// Closed outside the lock: each close makes its handler come back to untrack.
	for _, f := range doomed {
		e.log.Info("egress connection closed by a policy change", "vm", id, "domain", f.via, "port", f.port)
		f.conn.Close()
	}
}

// Remove forgets the VM at src and closes its connections.
func (e *Enforcer) Remove(src netip.Addr) {
	e.mu.Lock()
	var doomed []*flow
	if vs := e.vms[src]; vs != nil {
		for f := range vs.flows {
			doomed = append(doomed, f)
		}
	}
	delete(e.vms, src)
	e.mu.Unlock()
	for _, f := range doomed {
		f.conn.Close()
	}
}

// Lookup returns the ID and policy of the VM at src; ok is false for an
// address no running VM owns.
func (e *Enforcer) Lookup(src netip.Addr) (id string, p *Policy, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	vs := e.vms[src]
	if vs == nil {
		return "", nil, false
	}
	return vs.id, vs.policy, true
}

// record remembers that an allowed lookup of name by the VM at src returned dst.
func (e *Enforcer) record(src netip.Addr, name string, dst netip.Addr, ttl time.Duration) {
	name = canonicalName(name)
	e.mu.Lock()
	defer e.mu.Unlock()
	vs := e.vms[src]
	if vs == nil || !vs.policy.Resolves(name) { // the policy may have changed while upstream answered
		return
	}
	now := e.now()
	if vs.learned[dst] == nil {
		if len(vs.learned) >= maxLearned {
			for d, names := range vs.learned {
				for n, exp := range names {
					if now.After(exp) {
						delete(names, n)
					}
				}
				if len(names) == 0 {
					delete(vs.learned, d)
				}
			}
		}
		if len(vs.learned) >= maxLearned {
			e.log.Warn("egress address table full; not learning more", "vm", vs.id, "domain", name)
			return
		}
		vs.learned[dst] = map[string]time.Time{}
	}
	vs.learned[dst][name] = now.Add(ttl + ttlGrace)
}

// learnt returns a name, allowed on port, that the VM at src resolved to dst
// and whose answer hasn't expired; "" if there is none.
func (e *Enforcer) learnt(src, dst netip.Addr, port int) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	vs := e.vms[src]
	if vs == nil {
		return ""
	}
	now := e.now()
	for n, exp := range vs.learned[dst] {
		if now.Before(exp) && vs.policy.Allows(n, port) {
			return n
		}
	}
	return ""
}

// full reports whether the VM at src has as many proxied connections as it may.
func (e *Enforcer) full(src netip.Addr) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	vs := e.vms[src]
	return vs != nil && len(vs.flows) >= maxFlows
}

// track registers a live connection so a later policy change can close it.
// It checks again, under the lock, that the VM's policy (which may have
// changed while the proxy was dialing) permits it, and that the VM is still
// the one that asked.
func (e *Enforcer) track(src netip.Addr, id, via string, port int, conn io.Closer) (untrack func(), ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	vs := e.vms[src]
	if vs == nil || vs.id != id || len(vs.flows) >= maxFlows || !vs.policy.permits(via, port) {
		return nil, false
	}
	f := &flow{via: via, port: port, conn: conn}
	vs.flows[f] = struct{}{}
	return func() {
		e.mu.Lock()
		delete(vs.flows, f)
		e.mu.Unlock()
	}, true
}

// Policy is the current policy of VM id at src, nil if it no longer runs
// there. The broker asks for every request, so a changed secret is used from
// the next one.
func (e *Enforcer) Policy(src netip.Addr, id string) *Policy {
	e.mu.Lock()
	defer e.mu.Unlock()
	if vs := e.vms[src]; vs != nil && vs.id == id {
		return vs.policy
	}
	return nil
}

func canonicalName(name string) string {
	return strings.TrimSuffix(strings.ToLower(name), ".")
}
