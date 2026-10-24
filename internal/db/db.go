// Package db holds loam-api's records: projects, API keys, sandboxes, their
// checkpoints and port settings, the event log, (phase 2) every snapshot
// with the layers it references, the set of layers in the store, and the
// tombstones of VMs to delete on nodes that were away, (phase 3) each
// project's secrets, and (phase 4) the E2B adapter's records. Postgres is the
// store in service (Postgres, migrations applied at start); Memory is the same
// contract in memory, for tests and for running loam-api without a database.
// Both pass the same conformance test (store_test.go), so a handler test
// against Memory says something about Postgres too.
//
// The database is the truth (docs/ARCHITECTURE.md): a sandbox's state here is
// what loam-api last knew of its VM, and the nodelet's is reconciled against it.
package db

import (
	"context"
	"errors"
	"maps"
	"time"

	"github.com/arugula-salad/loam/pkg/api"
)

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
	// ErrNotEmpty is a project that still has sandboxes.
	ErrNotEmpty = errors.New("project has sandboxes")
	// ErrInUse is a layer that a snapshot record references, or a secret
	// that a sandbox names.
	ErrInUse = errors.New("still referenced")
	// ErrNoSecret is a sandbox naming a secret its project doesn't have. The
	// store checks it as it writes the sandbox, in the same transaction, so
	// that a secret can't be deleted between the check and the write.
	ErrNoSecret = errors.New("no such secret")
)

// Key is an API key as stored: its secret only as a SHA-256.
type Key struct {
	ID        string
	Project   string
	Name      string
	Hash      []byte
	CreatedAt time.Time
	// Scope is api.ScopeFull or api.ScopeRead.
	Scope string
}

// Sandbox is a sandbox's record: what docs/API.md shows of it, without the URL
// (which is derived), plus where it runs and its checkpoint counters.
type Sandbox struct {
	ID       string
	Name     string
	Project  string
	Template string
	// Image is the digest the sandbox was made from when it was made from
	// an image rather than a template (Template is then ""). A fork keeps
	// its source's.
	Image     string
	State     string
	Error     string
	Resources api.Resources
	Env       map[string]string
	Labels    map[string]string
	Idle      api.Idle
	Deadline  *api.Deadline
	Protected bool
	HTTPPort  int
	// Network is the rules as given, includes unexpanded; Secrets the names
	// of the project secrets the sandbox uses (docs/API.md).
	Network      api.Network
	Secrets      []string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastActiveAt time.Time

	// Node is the name of the node whose nodelet holds the VM; never an address
	// (docs/NODE.md, "Reaching a nodelet").
	Node string
	// NextCheckpoint and NextAuto number v<n> and auto-<n> checkpoints. They
	// only grow, so an ID is never reused after a delete.
	NextCheckpoint int
	NextAuto       int
	// Snapshot is the newest snapshot of the sandbox's VM (a Snapshot
	// record's ID), the one a wake on another node would use; "" until it has
	// one. It may be another sandbox's (a fork starts from its source's
	// checkpoint) and it may not be stored yet. NextSnapshot numbers the
	// <id>.@<n> snapshots its pauses and stops make.
	Snapshot     string
	NextSnapshot int
}

// Clone is a deep copy, so a caller can't change a record through a map.
func (s Sandbox) Clone() Sandbox {
	s.Env = cloneMap(s.Env)
	s.Labels = cloneMap(s.Labels)
	if s.Deadline != nil {
		d := *s.Deadline
		s.Deadline = &d
	}
	s.Network = CloneNetwork(s.Network)
	s.Secrets = append([]string{}, s.Secrets...)
	return s
}

// CloneNetwork is a deep copy whose Egress is never nil, so that it is shown
// and stored as [] rather than null.
func CloneNetwork(n api.Network) api.Network {
	out := api.Network{Egress: make([]api.Rule, len(n.Egress))}
	for _, r := range n.RequestHeaders {
		r.Headers = maps.Clone(r.Headers)
		out.RequestHeaders = append(out.RequestHeaders, r)
	}
	for i, r := range n.Egress {
		if r.Ports != nil {
			r.Ports = append([]int{}, r.Ports...)
		}
		out.Egress[i] = r
	}
	return out
}

// Secret is a project secret as stored: its value sealed (Value is the
// ciphertext, never the secret itself; loam-api seals and opens it).
type Secret struct {
	Project     string
	Name        string
	Hosts       []string
	Placeholder string
	Inject      *api.Inject
	Value       []byte
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Clone is a deep copy.
func (s Secret) Clone() Secret {
	s.Hosts = append([]string{}, s.Hosts...)
	s.Value = append([]byte{}, s.Value...)
	if s.Inject != nil {
		in := *s.Inject
		s.Inject = &in
	}
	return s
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Snapshot kinds: what made the snapshot.
const (
	SnapCheckpoint = "checkpoint"
	SnapPause      = "pause"
	SnapStop       = "stop"
)

// Snapshot record states. A record is written pending before loam-api asks a
// nodelet for the snapshot, so that the layers it shares with its parent are
// referenced while the nodelet makes it, and becomes ready with the nodelet's
// answer, or abandoned when the call failed (it may exist all the same, so it
// is deleted like garbage).
const (
	SnapPending   = "pending"
	SnapReady     = "ready"
	SnapAbandoned = "abandoned"
)

// Snapshot is a snapshot loam-api asked a nodelet for, or learnt of from one
// (a pause while draining): every checkpoint's, pause's and stop's. Its layers
// are what keeps layer objects in the store alive (docs/NODE.md, "Phase 2").
type Snapshot struct {
	ID string
	// Sandbox is the sandbox whose VM it was taken of. No foreign key: a
	// snapshot outlives its sandbox while a fork still starts from it.
	Sandbox string
	Kind    string
	// Checkpoint is the checkpoint ID of a SnapCheckpoint snapshot.
	Checkpoint string
	// Node is the node that made it: where it is on local disk, and where its
	// upload runs until it is stored.
	Node      string
	Parent    string
	Layers    []string
	Memory    bool
	Stored    bool
	State     string
	SizeBytes int64
	CreatedAt time.Time
	// Seq orders records by creation; the store assigns it.
	Seq int64
}

// Clone is a deep copy.
func (s Snapshot) Clone() Snapshot {
	s.Layers = append([]string{}, s.Layers...)
	return s
}

// Event is an api.Event and the project it belongs to.
type Event struct {
	Project string
	api.Event
}

// PortSetting is one port's access, as set by PUT .../ports/{port}.
type PortSetting struct {
	Sandbox string
	Port    int
	Access  string
}

// Traffic is one sandbox's traffic on one day (UTC, 2006-01-02), as loam-api's
// port proxy counts it (docs/API.md, "Stats"). Written, it is added to the
// day's counts and NewVisitors to the day's set of visitor hashes; read,
// Visitors is the size of that set.
type Traffic struct {
	Sandbox string
	Day     string
	api.Traffic
	NewVisitors [][]byte
	Visitors    int64
}

// Store is every record loam-api keeps. Methods answer ErrNotFound for a
// missing record and ErrExists for a duplicate name or ID.
type Store interface {
	CreateE2BTemplate(context.Context, E2BTemplate) error
	E2BTemplates(context.Context, string) ([]E2BTemplate, error)
	UpdateE2BTemplate(context.Context, string, string, func(*E2BTemplate) error) (E2BTemplate, error)
	DeleteE2BTemplate(context.Context, string, string) error
	E2BSecrets(context.Context, string) ([]E2BSecret, error)
	PutE2BSecret(context.Context, E2BSecret, bool) (E2BSecret, error)
	DeleteE2BSecret(context.Context, string, string) error
	CreateProject(ctx context.Context, p api.Project) error
	Project(ctx context.Context, name string) (api.Project, error)
	Projects(ctx context.Context) ([]api.Project, error)
	// DeleteProject is ErrNotEmpty while the project has sandboxes. Its keys
	// and secrets go with it.
	DeleteProject(ctx context.Context, name string) error

	CreateKey(ctx context.Context, k Key) error
	Keys(ctx context.Context, project string) ([]Key, error)
	KeyByHash(ctx context.Context, hash []byte) (Key, error)
	// DeleteKey takes the key's ID or its name.
	DeleteKey(ctx context.Context, project, key string) error

	// CreateSandbox and UpdateSandbox are ErrNoSecret when the record names a
	// secret its project doesn't have.
	CreateSandbox(ctx context.Context, s Sandbox) error
	Sandbox(ctx context.Context, id string) (Sandbox, error)
	SandboxByName(ctx context.Context, project, name string) (Sandbox, error)
	// Sandboxes lists a project's sandboxes, oldest first; project "" lists all.
	Sandboxes(ctx context.Context, project string) ([]Sandbox, error)
	// UpdateSandbox applies change to the current record atomically (in
	// Postgres, under a row lock) and stores it. An error from change aborts
	// the update and is returned as it is.
	UpdateSandbox(ctx context.Context, id string, change func(*Sandbox) error) (Sandbox, error)
	// DeleteSandbox removes the record, its checkpoints' records and its port settings.
	DeleteSandbox(ctx context.Context, id string) error

	CreateCheckpoint(ctx context.Context, c api.Checkpoint) error
	Checkpoint(ctx context.Context, sandbox, id string) (api.Checkpoint, error)
	// Checkpoints lists a sandbox's checkpoints, oldest first.
	Checkpoints(ctx context.Context, sandbox string) ([]api.Checkpoint, error)
	DeleteCheckpoint(ctx context.Context, sandbox, id string) error

	// CreateSnapshot stores a new snapshot record and returns it with its Seq.
	// Its layers are added to the layer set.
	CreateSnapshot(ctx context.Context, s Snapshot) (Snapshot, error)
	Snapshot(ctx context.Context, id string) (Snapshot, error)
	// SnapshotsOf lists the snapshots taken of a sandbox, SnapshotsOn the
	// ones a node made; oldest first.
	SnapshotsOf(ctx context.Context, sandbox string) ([]Snapshot, error)
	SnapshotsOn(ctx context.Context, node string) ([]Snapshot, error)
	// UpdateSnapshot applies change atomically, as UpdateSandbox does; new
	// layers are added to the layer set.
	UpdateSnapshot(ctx context.Context, id string, change func(*Snapshot) error) (Snapshot, error)
	DeleteSnapshot(ctx context.Context, id string) error
	// GarbageSnapshots lists the snapshot records nothing needs any more, oldest
	// first. A record is kept while it is pending, while a sandbox's Snapshot
	// names it, and otherwise: a checkpoint's while its checkpoint exists; a
	// pause's or stop's while its sandbox exists and has no newer stored
	// snapshot (a newer one supersedes it). An abandoned record is garbage
	// unless a sandbox names it.
	GarbageSnapshots(ctx context.Context) ([]Snapshot, error)
	// Layers lists the layer set: every layer a record has referenced and
	// that hasn't been deleted since. UnreferencedLayers is the part of it no
	// snapshot record references.
	Layers(ctx context.Context) ([]string, error)
	UnreferencedLayers(ctx context.Context) ([]string, error)
	// DeleteLayer removes a layer from the set; ErrInUse if a record
	// references it. Garbage collection removes a layer from the set before it
	// deletes the object, and adds it back (AddLayers) if that fails: a crash in
	// between leaks an object rather than deleting one a record still needs.
	DeleteLayer(ctx context.Context, id string) error
	AddLayers(ctx context.Context, ids []string) error

	// Tombstones are VMs of deleted or moved sandboxes that couldn't be
	// deleted when they were found (the node didn't answer, or the sandbox
	// was in a transition); reconciliation deletes them on a later pass.
	AddTombstone(ctx context.Context, node, vm string) error
	Tombstones(ctx context.Context, node string) ([]string, error)
	DeleteTombstone(ctx context.Context, node, vm string) error

	SetPort(ctx context.Context, p PortSetting) error
	Ports(ctx context.Context, sandbox string) ([]PortSetting, error)

	// PutSecret creates or replaces a secret, keeping the CreatedAt of the
	// one it replaces, and returns it as stored. ErrNotFound when the project
	// doesn't exist.
	PutSecret(ctx context.Context, s Secret) (Secret, error)
	Secret(ctx context.Context, project, name string) (Secret, error)
	// Secrets lists a project's secrets by name.
	Secrets(ctx context.Context, project string) ([]Secret, error)
	// DeleteSecret is ErrInUse while a sandbox of the project names it.
	DeleteSecret(ctx context.Context, project, name string) error

	// The E2B adapter's records (docs/E2B.md), one per sandbox it made,
	// deleted with the sandbox. CreateE2B is ErrNotFound without the sandbox;
	// E2Bs lists a project's, by sandbox ID, and project "" lists all.
	CreateE2B(ctx context.Context, e E2BSandbox) error
	E2B(ctx context.Context, sandbox string) (E2BSandbox, error)
	E2Bs(ctx context.Context, project string) ([]E2BSandbox, error)
	UpdateE2B(ctx context.Context, sandbox string, change func(*E2BSandbox) error) (E2BSandbox, error)

	AddEvent(ctx context.Context, e Event) error
	// Events lists a project's events at or after since, oldest first.
	Events(ctx context.Context, project string, since time.Time) ([]Event, error)
	PruneEvents(ctx context.Context, before time.Time) error

	// AddTraffic adds each record to its sandbox's day, in one transaction. A
	// record whose sandbox no longer exists is dropped: its traffic went with it.
	AddTraffic(ctx context.Context, ts []Traffic) error
	// TrafficOf lists a sandbox's days from since on (2006-01-02), oldest
	// first; a day without traffic has no record.
	TrafficOf(ctx context.Context, sandbox, since string) ([]Traffic, error)
	// PruneTraffic deletes the days before before.
	PruneTraffic(ctx context.Context, before string) error
	// VisitorSalt is day's salt for the visitor hashes, made on the first ask.
	// Earlier days' salts are deleted as it is made, so a past day's hashes
	// can't be recomputed.
	VisitorSalt(ctx context.Context, day string) ([]byte, error)

	Close()
}
