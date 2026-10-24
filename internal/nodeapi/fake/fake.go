// Package fake is an in-memory nodelet (docs/NODE.md) with a fake agent behind
// its agent stream, so loam-api's tests run without KVM. Its VMs are records:
// a "disk" is a map of files, a snapshot a copy of it. The agent answers a small
// subset of the sandbox sub-API (docs/AGENT.md): /exec/run and the /exec
// WebSocket for a few commands, /fs/file, /ports and /ports/{port}/connect to
// handlers a test registers with Listen. Every call that changes something is
// logged (Calls), so a test can check the order loam-api made them in.
//
// Phase 2 (docs/NODE.md, "Phase 2: the snapshot store"): several fake nodelets
// can share one Store, the snapshot store. Every snapshot gets a layer of its
// own ("L-<snapshot id>") on top of the layers of the VM it was taken of, and
// is uploaded at once (stored, with a snapshot event) unless HoldUploads holds
// the node's uploads back. Down makes a node unreachable; Drain does what a
// nodelet does on SIGTERM.
//
// Phase 3 (docs/NODE.md, "Phase 3: the network boundary"): every create,
// resume and restore's policy, and every PUT /v1/vms/{id}/policy, is held as
// the VM's while it runs (Policy) and forgotten when it pauses or stops, as a
// nodelet does. A node has the broker CA unless SetBroker says otherwise, and
// refuses a policy with secrets without it.
package fake

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arugula-salad/loam/internal/netpolicy"
	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

// Nodelet is one fake node. Set its exported fields before serving it.
type Nodelet struct {
	Name      string
	Token     string
	MemoryMiB int
	// Keepalive is how often an idle event stream gets a comment line, as a
	// real nodelet's does. 0 is never.
	Keepalive time.Duration

	mu        sync.Mutex
	ready     bool
	broker    bool
	down      bool
	downCh    chan struct{} // closed when the node goes down, ending its event streams
	stalled   bool          // event streams stay open but send nothing (Stall)
	store     *Store
	hold      bool
	uploads   []string // local snapshots waiting for their upload, oldest first
	drains    int
	vms       map[string]*vm
	snaps     map[string]*snapshot // local copies
	calls     []string
	subs      map[chan event]bool
	failNext  map[string]error // by call kind ("resume", "create", ...), once
	sessionNo int
	// atBoot are the ports a fresh boot starts something on (StartAtBoot).
	atBoot map[int]func(vm string) http.Handler
}

type event struct {
	kind string
	data any
}

type vm struct {
	nodeapi.VM
	files    map[string][]byte
	wake     nodeapi.Wake
	activity nodeapi.Activity
	ports    map[int]http.Handler
	// policy is the VM's policy while it runs; nil otherwise.
	policy *nodeapi.Policy
	// layers are the layers the VM's state is a diff on: its newest
	// snapshot's, or the one it was made from.
	layers []string
}

type snapshot struct {
	nodeapi.Snapshot
	files map[string][]byte
}

func (s *snapshot) clone() *snapshot {
	c := *s
	c.Layers = append([]string(nil), s.Layers...)
	c.files = maps.Clone(s.files)
	return &c
}

// Store is the snapshot store several fake nodelets share: stored snapshots
// and layer objects.
type Store struct {
	mu     sync.Mutex
	snaps  map[string]*snapshot
	layers map[string]bool
}

func NewStore() *Store { return &Store{snaps: map[string]*snapshot{}, layers: map[string]bool{}} }

// HasSnapshot reports whether snapshot id is in the store.
func (st *Store) HasSnapshot(id string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	_, ok := st.snaps[id]
	return ok
}

// Layers lists the layer objects in the store.
func (st *Store) Layers() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []string
	for l := range st.layers {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// Snapshots lists the snapshots in the store.
func (st *Store) Snapshots() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []string
	for id := range st.snaps {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (st *Store) get(id string) *snapshot {
	st.mu.Lock()
	defer st.mu.Unlock()
	if s, ok := st.snaps[id]; ok {
		return s.clone()
	}
	return nil
}

func (st *Store) put(s *snapshot) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.snaps[s.ID] = s.clone()
	for _, l := range s.Layers {
		st.layers[l] = true
	}
}

// New is a ready node called name, taking token, with memMiB for VMs, and a
// store of its own.
func New(name, token string, memMiB int) *Nodelet {
	return &Nodelet{Name: name, Token: token, MemoryMiB: memMiB, ready: true, broker: true, store: NewStore(), downCh: make(chan struct{}),
		vms: map[string]*vm{}, snaps: map[string]*snapshot{}, subs: map[chan event]bool{}, failNext: map[string]error{}}
}

// WithStore makes the node use st, the store it shares with other nodes.
func (n *Nodelet) WithStore(st *Store) *Nodelet {
	n.store = st
	return n
}

// Store is the node's snapshot store.
func (n *Nodelet) Store() *Store { return n.store }

// HoldUploads holds the node's uploads back (true) until FinishUploads, so its
// new snapshots stay unstored.
func (n *Nodelet) HoldUploads(hold bool) {
	n.mu.Lock()
	n.hold = hold
	n.mu.Unlock()
}

// FinishUploads stores every snapshot whose upload was held back.
func (n *Nodelet) FinishUploads() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, id := range n.uploads {
		if s, ok := n.snaps[id]; ok {
			n.storeLocked(s)
		}
	}
	n.uploads = nil
}

// upload stores s now, or queues it while uploads are held.
func (n *Nodelet) upload(s *snapshot) {
	if n.hold {
		n.uploads = append(n.uploads, s.ID)
		return
	}
	n.storeLocked(s)
}

func (n *Nodelet) storeLocked(s *snapshot) {
	s.Stored = true
	n.store.put(s)
	n.publishLocked(event{nodeapi.EventSnapshot, s.Snapshot})
}

// Down makes the node unreachable (true): every request fails at the
// connection and its event streams end. Down(false) brings it back as it was.
func (n *Nodelet) Down(down bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if down && !n.down {
		close(n.downCh)
	} else if !down && n.down {
		n.downCh = make(chan struct{})
	}
	n.down = down
}

// Drain is what a nodelet does on SIGTERM (docs/NODE.md, "Restarts"): it stops
// being ready, says so on its event stream, and pauses every running VM into
// a snapshot of its own naming, telling its event stream.
func (n *Nodelet) Drain() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ready = false
	n.publishLocked(event{nodeapi.EventNode, nodeapi.Node{Name: n.Name, Version: "fake", MemoryMiB: n.MemoryMiB}})
	ids := slices.Sorted(maps.Keys(n.vms))
	for _, id := range ids {
		m := n.vms[id]
		if m.State != nodeapi.VMRunning {
			continue
		}
		n.drains++
		s := n.makeSnapshot(m, fmt.Sprintf("%s.@drain-%d", id, n.drains), true)
		n.logf("drain %s %s", id, s.ID)
		m.State, m.policy = nodeapi.VMPaused, nil
		n.publishLocked(event{nodeapi.EventVM, m.VM})
		n.upload(s)
	}
}

// makeSnapshot takes snapshot id of m: its files, the layers m's state is a
// diff on and a layer of its own. A snapshot the VM would wake from elsewhere
// (one with memory, or the disk of a stopped VM) becomes the VM's newest.
func (n *Nodelet) makeSnapshot(m *vm, id string, memory bool) *snapshot {
	s := &snapshot{Snapshot: nodeapi.Snapshot{ID: id, VM: m.ID, Memory: memory, Template: m.Template,
		Resources: m.Resources, SizeBytes: 1 << 20, CreatedAt: time.Now().UTC(), Parent: m.Snapshot,
		Layers: append(append([]string(nil), m.layers...), "L-"+id)}, files: maps.Clone(m.files)}
	n.snaps[id] = s
	if memory || m.State == nodeapi.VMStopped {
		m.Snapshot, m.layers = id, s.Layers
	}
	return s
}

// findSnapshot is a local copy, else the store's.
func (n *Nodelet) findSnapshot(id string) *snapshot {
	if s, ok := n.snaps[id]; ok {
		return s
	}
	return n.store.get(id)
}

// Calls is the log of calls so far, as "kind args": "create sb-x",
// "snapshot sb-x sb-x.v1 memory", "agent sb-x POST /exec/run", ...
func (n *Nodelet) Calls() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.calls...)
}

// ResetCalls empties the log.
func (n *Nodelet) ResetCalls() {
	n.mu.Lock()
	n.calls = nil
	n.mu.Unlock()
}

func (n *Nodelet) logf(format string, args ...any) {
	n.calls = append(n.calls, fmt.Sprintf(format, args...))
}

// SetReady sets what GET /v1/node says of readiness.
func (n *Nodelet) SetReady(ready bool) {
	n.mu.Lock()
	n.ready = ready
	n.mu.Unlock()
}

// SetBroker sets whether the node has the broker CA, which GET /v1/node
// says and a policy with secrets needs.
func (n *Nodelet) SetBroker(broker bool) {
	n.mu.Lock()
	n.broker = broker
	n.mu.Unlock()
}

// Policy is the policy VM id holds now; ok is false when the node doesn't
// have the VM or it isn't running, so holds none.
func (n *Nodelet) Policy(id string) (p nodeapi.Policy, ok bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	m, ok := n.vms[id]
	if !ok || m.policy == nil {
		return p, false
	}
	return clonePolicy(*m.policy), true
}

func clonePolicy(p nodeapi.Policy) nodeapi.Policy {
	b, _ := json.Marshal(p)
	var out nodeapi.Policy
	json.Unmarshal(b, &out)
	return out
}

// checkPolicy is what a nodelet refuses of a policy: rules it can't compile,
// and secrets without the broker CA.
func (n *Nodelet) checkPolicy(w http.ResponseWriter, p nodeapi.Policy) bool {
	if _, err := netpolicy.Compile(p.Egress); err != nil {
		writeErr(w, 400, "invalid", err.Error())
		return false
	}
	if (len(p.Secrets) > 0 || len(p.RequestHeaders) > 0) && !n.broker {
		writeErr(w, 501, "unsupported", "this node has no broker CA")
		return false
	}
	return true
}

// Fail makes the next call of kind (create, resume, pause, stop, snapshot,
// restore, policy, delete-layer) answer err (a *nodeapi.Err gives its status and
// code). A nil err takes a failure back.
func (n *Nodelet) Fail(kind string, err *nodeapi.Err) {
	n.mu.Lock()
	if err == nil {
		delete(n.failNext, kind)
	} else {
		n.failNext[kind] = err
	}
	n.mu.Unlock()
}

func (n *Nodelet) takeFail(kind string) error {
	err := n.failNext[kind]
	delete(n.failNext, kind)
	return err
}

// VM is a copy of a VM's state and the last Wake it got; ok is false when the
// node doesn't have it.
func (n *Nodelet) VM(id string) (v nodeapi.VM, w nodeapi.Wake, ok bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	m, ok := n.vms[id]
	if !ok {
		return v, w, false
	}
	return m.VM, m.wake, true
}

// HasSnapshot reports whether the node holds a local copy of snapshot id.
func (n *Nodelet) HasSnapshot(id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.snaps[id]
	return ok
}

// VMs lists the IDs of the node's VMs.
func (n *Nodelet) VMs() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Sorted(maps.Keys(n.vms))
}

// File is a file on a VM's disk.
func (n *Nodelet) File(id, path string) ([]byte, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if m, ok := n.vms[id]; ok {
		b, ok := m.files[path]
		return b, ok
	}
	return nil, false
}

// SetActivity sets what the VM's agent reports at /internal/activity.
func (n *Nodelet) SetActivity(id string, a nodeapi.Activity) {
	n.mu.Lock()
	if m, ok := n.vms[id]; ok {
		m.activity = a
	}
	n.mu.Unlock()
}

// Listen makes port in VM id answer with h, as if a process listened on it.
// A nil h closes it.
func (n *Nodelet) Listen(id string, port int, h http.Handler) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if m, ok := n.vms[id]; ok {
		if h == nil {
			delete(m.ports, port)
		} else {
			m.ports[port] = h
		}
	}
}

// StartAtBoot makes port answer, in every VM from its next fresh boot on,
// with what start returns for it, as a service the image starts at every
// boot would (envd, in the e2b template). Every fresh boot (a create, a
// restore, a resume of a stopped VM) calls start again, so what the handler
// kept is lost as a restarted process's would be; a resume from a pause
// keeps the handler it had.
func (n *Nodelet) StartAtBoot(port int, start func(vm string) http.Handler) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.atBoot == nil {
		n.atBoot = map[int]func(string) http.Handler{}
	}
	n.atBoot[port] = start
}

// startAtBootLocked starts what StartAtBoot asks for in m, freshly booted.
func (n *Nodelet) startAtBootLocked(m *vm) {
	for port, start := range n.atBoot {
		m.ports[port] = start(m.ID)
	}
}

// Crash puts a VM in the error state, as a VM process dying would, and tells
// the event stream.
func (n *Nodelet) Crash(id, message string) {
	n.mu.Lock()
	m, ok := n.vms[id]
	if ok {
		m.State, m.Error, m.policy = nodeapi.VMError, message, nil
	}
	if ok {
		n.publishLocked(event{nodeapi.EventVM, m.VM})
	}
	n.mu.Unlock()
}

// PauseQuietly pauses a VM without telling the event stream, as a nodelet
// draining on SIGTERM does while loam-api's stream to it is down.
func (n *Nodelet) PauseQuietly(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if m, ok := n.vms[id]; ok {
		m.State, m.policy = nodeapi.VMPaused, nil
	}
}

// Forget drops a VM without telling anyone, as a nodelet that lost its data would.
func (n *Nodelet) Forget(id string) {
	n.mu.Lock()
	delete(n.vms, id)
	n.mu.Unlock()
}

func (n *Nodelet) publishLocked(e event) {
	for ch := range n.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, api.Error{Error: api.ErrorDetail{Code: code, Message: msg}})
}

func writeFail(w http.ResponseWriter, err error) {
	if e, ok := err.(*nodeapi.Err); ok {
		writeErr(w, e.Status, e.Code, e.Message)
		return
	}
	writeErr(w, 500, "internal", err.Error())
}

func (n *Nodelet) allocated() int {
	sum := 0
	for _, m := range n.vms {
		if m.State == nodeapi.VMRunning {
			sum += m.Resources.MemoryMiB
		}
	}
	return sum
}

func (n *Nodelet) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	down := n.down
	n.mu.Unlock()
	if down {
		// Unreachable: the connection drops without an answer.
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				c.Close()
				return
			}
		}
		panic(http.ErrAbortHandler)
	}
	if r.Header.Get("Authorization") != "Bearer "+n.Token {
		writeErr(w, 401, "unauthorized", "bad node token")
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/node", n.node)
	mux.HandleFunc("GET /v1/vms", n.list)
	mux.HandleFunc("GET /v1/vms/{id}", n.get)
	mux.HandleFunc("POST /v1/vms", n.create)
	mux.HandleFunc("POST /v1/vms/{id}/resume", n.resume)
	mux.HandleFunc("PUT /v1/vms/{id}/policy", n.setPolicy)
	mux.HandleFunc("POST /v1/vms/{id}/pause", n.transition("pause", nodeapi.VMPaused))
	mux.HandleFunc("POST /v1/vms/{id}/stop", n.transition("stop", nodeapi.VMStopped))
	mux.HandleFunc("GET /v1/snapshots/{id}", n.getSnapshot)
	mux.HandleFunc("DELETE /v1/layers/{layer}", n.deleteLayer)
	mux.HandleFunc("DELETE /v1/vms/{id}", n.delete)
	mux.HandleFunc("POST /v1/vms/{id}/snapshots", n.snapshot)
	mux.HandleFunc("POST /v1/vms/{id}/restore", n.restore)
	mux.HandleFunc("GET /v1/vms/{id}/agent", n.agent)
	mux.HandleFunc("GET /v1/vms/{id}/activity", n.activity)
	mux.HandleFunc("GET /v1/snapshots", n.snapshots)
	mux.HandleFunc("DELETE /v1/snapshots/{id}", n.deleteSnapshot)
	mux.HandleFunc("GET /v1/events", n.events)
	mux.ServeHTTP(w, r)
}

func (n *Nodelet) node(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	defer n.mu.Unlock()
	writeJSON(w, 200, nodeapi.Node{Name: n.Name, Version: "fake", CPUs: 8, MemoryMiB: n.MemoryMiB,
		MemoryMiBAllocated: n.allocated(), DiskGiBFree: 100, VMs: len(n.vms), Ready: n.ready, Broker: n.broker})
}

func (n *Nodelet) list(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := nodeapi.VMList{VMs: []nodeapi.VM{}}
	for _, m := range n.vms {
		out.VMs = append(out.VMs, m.VM)
	}
	sort.Slice(out.VMs, func(i, j int) bool { return out.VMs[i].ID < out.VMs[j].ID })
	writeJSON(w, 200, out)
}

func (n *Nodelet) get(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	defer n.mu.Unlock()
	m, ok := n.vms[r.PathValue("id")]
	if !ok {
		writeErr(w, 404, "not_found", "no such VM")
		return
	}
	writeJSON(w, 200, m.VM)
}

func (n *Nodelet) boot(m *vm, wake nodeapi.Wake, pol nodeapi.Policy) {
	// A resume from a pause keeps booted_at, as a nodelet's does.
	if m.State != nodeapi.VMPaused || m.BootedAt == nil {
		now := time.Now().UTC()
		m.BootedAt = &now
	}
	m.State, m.Error, m.wake = nodeapi.VMRunning, "", wake
	m.activity = nodeapi.Activity{}
	p := clonePolicy(pol)
	m.policy = &p
}

func (n *Nodelet) create(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.CreateVM
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid", err.Error())
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if req.From != nil {
		n.logf("create %s from %s", req.ID, req.From.Snapshot)
	} else {
		n.logf("create %s", req.ID)
	}
	if err := n.takeFail("create"); err != nil {
		writeFail(w, err)
		return
	}
	if !n.ready {
		writeErr(w, 503, "not_ready", "the node is draining")
		return
	}
	if !n.checkPolicy(w, req.Policy) {
		return
	}
	if _, ok := n.vms[req.ID]; ok {
		writeErr(w, 409, "already_exists", "VM exists")
		return
	}
	if n.allocated()+req.Resources.MemoryMiB > n.MemoryMiB {
		writeErr(w, 503, "capacity", "the memory doesn't fit")
		return
	}
	m := &vm{VM: nodeapi.VM{ID: req.ID, Template: req.Template, Resources: req.Resources, IP: "10.240.0.2"},
		files: map[string][]byte{}, ports: map[int]http.Handler{}}
	if req.From != nil {
		s := n.findSnapshot(req.From.Snapshot)
		if s == nil {
			writeErr(w, 404, "not_found", "no such snapshot here or stored")
			return
		}
		if s.Template != req.Template || s.Resources != req.Resources {
			writeErr(w, 400, "invalid", "template and resources must match the snapshot's")
			return
		}
		m.files = maps.Clone(s.files)
		m.Snapshot, m.layers = s.ID, append([]string(nil), s.Layers...)
	}
	n.boot(m, req.Wake, req.Policy)
	n.startAtBootLocked(m)
	n.vms[req.ID] = m
	writeJSON(w, 201, m.VM)
}

func (n *Nodelet) resume(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.Resume
	json.NewDecoder(r.Body).Decode(&req)
	id := r.PathValue("id")
	n.mu.Lock()
	defer n.mu.Unlock()
	n.logf("resume %s", id)
	if err := n.takeFail("resume"); err != nil {
		writeFail(w, err)
		return
	}
	m, ok := n.vms[id]
	if !ok {
		writeErr(w, 404, "not_found", "no such VM")
		return
	}
	if !n.ready && m.State != nodeapi.VMRunning {
		writeErr(w, 503, "not_ready", "the node is draining")
		return
	}
	if m.State != nodeapi.VMRunning && n.allocated()+m.Resources.MemoryMiB > n.MemoryMiB {
		writeErr(w, 503, "capacity", "the memory doesn't fit")
		return
	}
	if !n.checkPolicy(w, req.Policy) {
		return
	}
	if m.State == nodeapi.VMRunning {
		p := clonePolicy(req.Policy)
		m.wake, m.policy = req.Wake, &p
	} else {
		fresh := m.State != nodeapi.VMPaused
		if fresh {
			m.BootedAt = nil
		}
		n.boot(m, req.Wake, req.Policy)
		if fresh {
			n.startAtBootLocked(m)
		}
	}
	writeJSON(w, 200, m.VM)
}

func (n *Nodelet) setPolicy(w http.ResponseWriter, r *http.Request) {
	var p nodeapi.Policy
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, 400, "invalid", err.Error())
		return
	}
	id := r.PathValue("id")
	n.mu.Lock()
	defer n.mu.Unlock()
	n.logf("policy %s", id)
	if err := n.takeFail("policy"); err != nil {
		writeFail(w, err)
		return
	}
	m, ok := n.vms[id]
	if !ok {
		writeErr(w, 404, "not_found", "no such VM")
		return
	}
	if m.State != nodeapi.VMRunning {
		writeErr(w, 409, "conflict", "the VM isn't running; it gets its policy at its next resume")
		return
	}
	if !n.checkPolicy(w, p) {
		return
	}
	m.policy = &p
	w.WriteHeader(204)
}

// transition is pause and stop: each makes the snapshot named in the body,
// unless the VM is in its target state already (then it answers the VM as it
// is, naming the snapshot it has).
func (n *Nodelet) transition(kind, to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req nodeapi.PauseVM
		json.NewDecoder(r.Body).Decode(&req)
		id := r.PathValue("id")
		n.mu.Lock()
		defer n.mu.Unlock()
		n.logf("%s %s %s", kind, id, req.Snapshot)
		if err := n.takeFail(kind); err != nil {
			writeFail(w, err)
			return
		}
		m, ok := n.vms[id]
		if !ok {
			writeErr(w, 404, "not_found", "no such VM")
			return
		}
		if m.State == to {
			writeJSON(w, 200, m.VM)
			return
		}
		if kind == "pause" && m.State != nodeapi.VMRunning {
			writeErr(w, 409, "conflict", "only a running VM pauses")
			return
		}
		if req.Snapshot == "" {
			writeErr(w, 400, "invalid", "snapshot is required")
			return
		}
		if n.findSnapshot(req.Snapshot) != nil {
			writeErr(w, 409, "already_exists", "snapshot exists")
			return
		}
		m.State, m.Error, m.policy = to, "", nil
		s := n.makeSnapshot(m, req.Snapshot, kind == "pause")
		n.upload(s)
		writeJSON(w, 200, m.VM)
	}
}

func (n *Nodelet) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n.mu.Lock()
	defer n.mu.Unlock()
	n.logf("delete %s", id)
	if _, ok := n.vms[id]; !ok {
		writeErr(w, 404, "not_found", "no such VM")
		return
	}
	delete(n.vms, id)
	w.WriteHeader(204)
}

func (n *Nodelet) snapshot(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.CreateSnapshot
	json.NewDecoder(r.Body).Decode(&req)
	id := r.PathValue("id")
	n.mu.Lock()
	defer n.mu.Unlock()
	kind := "disk"
	if req.Memory {
		kind = "memory"
	}
	n.logf("snapshot %s %s %s", id, req.ID, kind)
	if err := n.takeFail("snapshot"); err != nil {
		writeFail(w, err)
		return
	}
	m, ok := n.vms[id]
	if !ok {
		writeErr(w, 404, "not_found", "no such VM")
		return
	}
	if req.Memory && m.State != nodeapi.VMRunning && m.State != nodeapi.VMPaused {
		writeErr(w, 409, "conflict", "a memory snapshot needs a running or paused VM")
		return
	}
	if n.findSnapshot(req.ID) != nil {
		writeErr(w, 409, "already_exists", "snapshot exists")
		return
	}
	s := n.makeSnapshot(m, req.ID, req.Memory)
	n.upload(s)
	writeJSON(w, 201, s.Snapshot)
}

func (n *Nodelet) restore(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.Restore
	json.NewDecoder(r.Body).Decode(&req)
	id := r.PathValue("id")
	n.mu.Lock()
	defer n.mu.Unlock()
	n.logf("restore %s %s", id, req.Snapshot)
	if err := n.takeFail("restore"); err != nil {
		writeFail(w, err)
		return
	}
	m, ok := n.vms[id]
	if !ok {
		writeErr(w, 404, "not_found", "no such VM")
		return
	}
	s := n.findSnapshot(req.Snapshot)
	if s == nil {
		writeErr(w, 404, "not_found", "no such snapshot here or stored")
		return
	}
	if !n.checkPolicy(w, req.Policy) {
		return
	}
	m.files = maps.Clone(s.files)
	m.Snapshot, m.layers = s.ID, append([]string(nil), s.Layers...)
	m.ports = map[int]http.Handler{}
	m.BootedAt = nil
	n.boot(m, req.Wake, req.Policy)
	n.startAtBootLocked(m)
	writeJSON(w, 200, m.VM)
}

func (n *Nodelet) activity(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	defer n.mu.Unlock()
	m, ok := n.vms[r.PathValue("id")]
	if !ok {
		writeErr(w, 404, "not_found", "no such VM")
		return
	}
	if m.State != nodeapi.VMRunning {
		writeErr(w, 409, "conflict", "VM is not running")
		return
	}
	writeJSON(w, 200, m.activity)
}

func (n *Nodelet) snapshots(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := nodeapi.SnapshotList{Snapshots: []nodeapi.Snapshot{}}
	for _, s := range n.snaps {
		out.Snapshots = append(out.Snapshots, s.Snapshot)
	}
	sort.Slice(out.Snapshots, func(i, j int) bool { return out.Snapshots[i].ID < out.Snapshots[j].ID })
	writeJSON(w, 200, out)
}

func (n *Nodelet) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n.mu.Lock()
	defer n.mu.Unlock()
	n.logf("delete-snapshot %s", id)
	for _, m := range n.vms {
		if m.State == nodeapi.VMPaused && m.Snapshot == id {
			writeErr(w, 409, "conflict", "a VM here is paused on it")
			return
		}
	}
	_, local := n.snaps[id]
	n.store.mu.Lock()
	_, stored := n.store.snaps[id]
	delete(n.store.snaps, id)
	n.store.mu.Unlock()
	if !local && !stored {
		writeErr(w, 404, "not_found", "no such snapshot")
		return
	}
	delete(n.snaps, id)
	n.uploads = slices.DeleteFunc(n.uploads, func(u string) bool { return u == id })
	w.WriteHeader(204)
}

func (n *Nodelet) getSnapshot(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := n.findSnapshot(r.PathValue("id"))
	if s == nil {
		writeErr(w, 404, "not_found", "no such snapshot")
		return
	}
	writeJSON(w, 200, s.Snapshot)
}

func (n *Nodelet) deleteLayer(w http.ResponseWriter, r *http.Request) {
	l := r.PathValue("layer")
	n.mu.Lock()
	defer n.mu.Unlock()
	n.logf("delete-layer %s", l)
	if err := n.takeFail("delete-layer"); err != nil {
		writeFail(w, err)
		return
	}
	n.store.mu.Lock()
	defer n.store.mu.Unlock()
	if !n.store.layers[l] {
		writeErr(w, 404, "not_found", "no such layer")
		return
	}
	delete(n.store.layers, l)
	w.WriteHeader(204)
}

func (n *Nodelet) events(w http.ResponseWriter, r *http.Request) {
	ch := make(chan event, 64)
	n.mu.Lock()
	n.subs[ch] = true
	downCh := n.downCh
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		delete(n.subs, ch)
		n.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	fl := w.(http.Flusher)
	fl.Flush()
	var tick <-chan time.Time
	if n.Keepalive > 0 {
		t := time.NewTicker(n.Keepalive)
		defer t.Stop()
		tick = t.C
	}
	stalled := func() bool { n.mu.Lock(); defer n.mu.Unlock(); return n.stalled }
	for {
		select {
		case <-r.Context().Done():
			return
		case <-downCh:
			panic(http.ErrAbortHandler) // the connection drops
		case <-tick:
			if !stalled() {
				fmt.Fprint(w, ": keepalive\n\n")
				fl.Flush()
			}
		case e := <-ch:
			if stalled() {
				continue
			}
			b, _ := json.Marshal(e.data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.kind, b)
			fl.Flush()
		}
	}
}

// Stall makes the open event streams go silent, keepalives and all, without
// closing, as a nodelet whose process is frozen (SIGSTOP) does: the kernel
// keeps its connections open.
func (n *Nodelet) Stall(stalled bool) {
	n.mu.Lock()
	n.stalled = stalled
	n.mu.Unlock()
}

// Subscribers is how many event streams are open, so a test can wait for
// loam-api to have connected before it crashes a VM.
func (n *Nodelet) Subscribers() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.subs)
}

func (n *Nodelet) agent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n.mu.Lock()
	m, ok := n.vms[id]
	running := ok && m.State == nodeapi.VMRunning
	n.mu.Unlock()
	if !ok {
		writeErr(w, 404, "not_found", "no such VM")
		return
	}
	if !running {
		writeErr(w, 409, "conflict", "VM is not running")
		return
	}
	c, err := nodeapi.AcceptUpgrade(w, r)
	if err != nil {
		return
	}
	// One HTTP exchange (or WebSocket) per stream, as loam-api speaks it.
	srv := &http.Server{Handler: &agent{n: n, id: id}}
	srv.Serve(newOneConnListener(c))
}

// oneConnListener hands out one connection, then blocks until it is closed,
// so http.Server.Serve returns once the stream is done.
type oneConnListener struct {
	c    net.Conn
	once sync.Once
	done chan struct{}
}

func newOneConnListener(c net.Conn) *oneConnListener {
	l := &oneConnListener{done: make(chan struct{})}
	l.c = &notifyConn{Conn: c, closed: l.done}
	return l
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.c })
	if c != nil {
		return c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.c.LocalAddr() }

type notifyConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *notifyConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
	return err
}

// agent is the fake guest agent of one VM.
type agent struct {
	n  *Nodelet
	id string
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func (a *agent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.n.mu.Lock()
	a.n.logf("agent %s %s %s", a.id, r.Method, r.URL.Path)
	m := a.n.vms[a.id]
	a.n.mu.Unlock()
	if m == nil {
		writeErr(w, 404, "not_found", "no such VM")
		return
	}
	switch p := r.URL.Path; {
	case r.Method == "POST" && p == "/exec/run":
		a.execRun(w, r, m)
	case r.Method == "GET" && p == "/exec":
		a.exec(w, r, m)
	case p == "/fs/file":
		a.file(w, r, m)
	case r.Method == "GET" && p == "/ports":
		a.n.mu.Lock()
		out := api.PortList{Ports: []api.Port{}}
		for port := range m.ports {
			out.Ports = append(out.Ports, api.Port{Port: port, Address: "0.0.0.0", PID: 42})
		}
		a.n.mu.Unlock()
		sort.Slice(out.Ports, func(i, j int) bool { return out.Ports[i].Port < out.Ports[j].Port })
		writeJSON(w, 200, out)
	case r.Method == "GET" && strings.HasPrefix(p, "/ports/") && strings.HasSuffix(p, "/connect"):
		a.connect(w, r, m, strings.TrimSuffix(strings.TrimPrefix(p, "/ports/"), "/connect"))
	default:
		writeErr(w, 404, "not_found", "the fake agent has no "+r.Method+" "+p)
	}
}

// run is the fake's whole command set: echo, cat, env, hostname, exit N.
func (a *agent) run(m *vm, cmd []string, stdin []byte, env map[string]string) (stdout, stderr []byte, code int) {
	if len(cmd) == 0 {
		return nil, []byte("no command\n"), 2
	}
	switch cmd[0] {
	case "echo":
		return []byte(strings.Join(cmd[1:], " ") + "\n"), nil, 0
	case "cat":
		if len(cmd) > 1 {
			a.n.mu.Lock()
			b, ok := m.files[cmd[1]]
			a.n.mu.Unlock()
			if !ok {
				return nil, []byte("cat: " + cmd[1] + ": No such file or directory\n"), 1
			}
			return b, nil, 0
		}
		return stdin, nil, 0
	case "env":
		a.n.mu.Lock()
		all := maps.Clone(m.wake.Env)
		a.n.mu.Unlock()
		if all == nil {
			all = map[string]string{}
		}
		maps.Copy(all, env)
		var lines []string
		for k, v := range all {
			lines = append(lines, k+"="+v)
		}
		sort.Strings(lines)
		return []byte(strings.Join(lines, "\n") + "\n"), nil, 0
	case "hostname":
		a.n.mu.Lock()
		h := m.wake.Hostname
		a.n.mu.Unlock()
		return []byte(h + "\n"), nil, 0
	case "exit":
		code, _ := strconv.Atoi(cmd[len(cmd)-1])
		return nil, nil, code
	}
	return nil, []byte(cmd[0] + ": command not found\n"), 127
}

func (a *agent) execRun(w http.ResponseWriter, r *http.Request, m *vm) {
	var req api.ExecRun
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Cmd) == 0 {
		writeErr(w, 400, "invalid", "cmd is required")
		return
	}
	out, errb, code := a.run(m, req.Cmd, req.Stdin, req.Env)
	writeJSON(w, 200, api.ExecResult{ExitCode: code, Stdout: out, Stderr: errb})
}

// exec is the streaming exec: `cat` echoes stdin to stdout until EOF; `sleep`
// runs until the socket closes; anything else is run as by /exec/run.
func (a *agent) exec(w http.ResponseWriter, r *http.Request, m *vm) {
	cmd := r.URL.Query()["cmd"]
	if len(cmd) == 0 {
		writeErr(w, 400, "invalid", "cmd is required")
		return
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	a.n.mu.Lock()
	a.n.sessionNo++
	sid := fmt.Sprintf("s-%d", a.n.sessionNo)
	a.n.mu.Unlock()
	ws.WriteJSON(api.ExecMessage{Type: "started", Session: sid, PID: 100})
	exit := func(code int) {
		ws.WriteJSON(api.ExecMessage{Type: "exit", Code: &code})
		ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		ws.ReadMessage() // the peer's close
	}
	switch cmd[0] {
	case "cat":
		for {
			kind, msg, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.BinaryMessage || len(msg) == 0 {
				continue
			}
			switch msg[0] {
			case api.StreamStdin:
				ws.WriteMessage(websocket.BinaryMessage, append([]byte{api.StreamStdout}, msg[1:]...))
			case api.StreamStdinEOF:
				exit(0)
				return
			}
		}
	case "sleep":
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	default:
		out, errb, code := a.run(m, cmd, nil, nil)
		if len(out) > 0 {
			ws.WriteMessage(websocket.BinaryMessage, append([]byte{api.StreamStdout}, out...))
		}
		if len(errb) > 0 {
			ws.WriteMessage(websocket.BinaryMessage, append([]byte{api.StreamStderr}, errb...))
		}
		exit(code)
	}
}

func (a *agent) file(w http.ResponseWriter, r *http.Request, m *vm) {
	path := r.URL.Query().Get("path")
	if !strings.HasPrefix(path, "/") {
		writeErr(w, 400, "invalid", "path must be absolute")
		return
	}
	var body []byte
	if r.Method == "PUT" {
		var err error
		if body, err = io.ReadAll(r.Body); err != nil {
			return
		}
	}
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	switch r.Method {
	case "GET":
		b, ok := m.files[path]
		if !ok {
			writeErr(w, 404, "not_found", "no such file")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(b)
	case "PUT":
		b := body
		m.files[path] = b
		writeJSON(w, 200, api.Stat{Path: path, Name: path[strings.LastIndex(path, "/")+1:], Type: "file", Size: int64(len(b)), Mode: "0644", MTime: time.Now().UTC()})
	case "DELETE":
		delete(m.files, path)
		w.WriteHeader(204)
	default:
		writeErr(w, 405, "invalid", "method not allowed")
	}
}

// connect is /ports/{port}/connect: a WebSocket of binary frames to whatever
// Listen put on the port, carried over an in-memory pipe. Nothing listening
// is 502 port_closed, before the upgrade (docs/API.md, "Ports").
func (a *agent) connect(w http.ResponseWriter, r *http.Request, m *vm, portStr string) {
	port, err := strconv.Atoi(portStr)
	if err != nil {
		writeErr(w, 400, "invalid", "bad port")
		return
	}
	a.n.mu.Lock()
	h := m.ports[port]
	a.n.mu.Unlock()
	if h == nil {
		writeErr(w, 502, api.CodePortClosed, fmt.Sprintf("nothing listens on port %d", port))
		return
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	app, guest := net.Pipe()
	go (&http.Server{Handler: h}).Serve(newOneConnListener(app))
	go func() {
		buf := make([]byte, 32<<10)
		for {
			k, err := guest.Read(buf)
			if k > 0 {
				if ws.WriteMessage(websocket.BinaryMessage, buf[:k]) != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		ws.Close()
	}()
	for {
		kind, msg, err := ws.ReadMessage()
		if err != nil {
			break
		}
		if kind == websocket.BinaryMessage {
			if _, err := guest.Write(msg); err != nil {
				break
			}
		}
	}
	guest.Close()
	ws.Close()
}
