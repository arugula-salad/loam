// Package nodeapi is the node protocol (docs/NODE.md): the types loam-api and
// loam-nodelet exchange, loam-api's client, and the upgrade both sides of the
// agent stream use.
package nodeapi

import (
	"time"

	"github.com/arugula-salad/loam/pkg/api"
)

// Port is where a nodelet serves the node protocol.
const Port = 7070

// VM states.
const (
	VMRunning = "running"
	VMPaused  = "paused"
	VMStopped = "stopped"
	VMError   = "error"
)

type Node struct {
	Name               string `json:"name"`
	Version            string `json:"version"`
	CPUs               int    `json:"cpus"`
	MemoryMiB          int    `json:"memory_mib"`
	MemoryMiBAllocated int    `json:"memory_mib_allocated"`
	DiskGiBFree        int    `json:"disk_gib_free"`
	VMs                int    `json:"vms"`
	Ready              bool   `json:"ready"`
	// Broker is whether the nodelet has the broker CA, without which it
	// refuses a policy with secrets.
	Broker bool `json:"broker"`
}

type Template struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

type VM struct {
	ID        string        `json:"id"`
	State     string        `json:"state"`
	Error     string        `json:"error,omitempty"`
	Template  Template      `json:"template"`
	Resources api.Resources `json:"resources"`
	BootedAt  *time.Time    `json:"booted_at,omitempty"`
	IP        string        `json:"ip,omitempty"`
	// Snapshot is the VM's newest snapshot, the one a move or a wake on
	// another node would use (docs/NODE.md, "Phase 2: the snapshot store"):
	// "" until it has one.
	Snapshot string `json:"snapshot,omitempty"`
}

type VMList struct {
	VMs []VM `json:"vms"`
}

// Wake is what the agent gets at every boot and resume (docs/AGENT.md,
// /internal/wake), and only then: nothing of it is in a snapshot that the
// next wake doesn't overwrite.
type Wake struct {
	Sandbox  string            `json:"sandbox"`
	Name     string            `json:"name"`
	Project  string            `json:"project"`
	Hostname string            `json:"hostname"`
	Env      map[string]string `json:"env"`
	User     string            `json:"user"`
	Time     time.Time         `json:"time"`
	// Network is eth0's configuration, which the agent converges on at every
	// wake. A fresh boot already has it from the kernel command line; a memory
	// snapshot resumed on another VM (a fork) still has its source's.
	Network *WakeNetwork `json:"network,omitempty"`
	// CA is the broker CA's root certificate (PEM), which the agent installs.
	// The nodelet sets it, like Network.
	CA string `json:"ca,omitempty"`
}

// WakeNetwork is a VM's eth0: its address with prefix length
// ("10.240.0.7/16"), the gateway, resolvers, and its MAC, which must match
// the one the nodelet gave Firecracker.
type WakeNetwork struct {
	IP      string   `json:"ip"`
	Gateway string   `json:"gateway"`
	DNS     []string `json:"dns,omitempty"`
	MAC     string   `json:"mac,omitempty"`
}

type FromSnapshot struct {
	Snapshot string `json:"snapshot"`
}

type CreateVM struct {
	ID        string        `json:"id"`
	Template  Template      `json:"template"`
	Resources api.Resources `json:"resources"`
	From      *FromSnapshot `json:"from,omitempty"`
	Wake      Wake          `json:"wake"`
	Policy    Policy        `json:"policy"`
}

// Policy is what a VM may reach and the secrets it uses (docs/NODE.md,
// "Phase 3: the network boundary"). The nodelet holds it in memory, only
// while the VM runs. Egress has its includes expanded by loam-api.
type Policy struct {
	Egress  []api.Rule     `json:"egress"`
	Secrets []PolicySecret `json:"secrets"`
}

// PolicySecret is a secret with its value, which only the nodelet's proxy
// ever sees.
type PolicySecret struct {
	Name        string      `json:"name"`
	Value       string      `json:"value"`
	Hosts       []string    `json:"hosts"`
	Placeholder string      `json:"placeholder"`
	Inject      *api.Inject `json:"inject"`
}

// PauseVM and StopVM name the snapshot a pause or a stop makes; loam-api
// chooses it (<sandbox>.@<n>).
type PauseVM struct {
	Snapshot string `json:"snapshot"`
}

type StopVM struct {
	Snapshot string `json:"snapshot"`
}

type Resume struct {
	Wake   Wake   `json:"wake"`
	Policy Policy `json:"policy"`
}

type CreateSnapshot struct {
	ID     string `json:"id"`
	Memory bool   `json:"memory"`
}

type Restore struct {
	Snapshot string `json:"snapshot"`
	Wake     Wake   `json:"wake"`
	Policy   Policy `json:"policy"`
}

type Snapshot struct {
	ID        string        `json:"id"`
	VM        string        `json:"vm"`
	Memory    bool          `json:"memory"`
	Template  Template      `json:"template"`
	Resources api.Resources `json:"resources"`
	SizeBytes int64         `json:"size_bytes"`
	CreatedAt time.Time     `json:"created_at"`
	// Parent is the snapshot this one is a diff against, or "".
	Parent string `json:"parent"`
	// Layers is every layer the snapshot's headers reference, its own last.
	// A layer outlives the snapshot that made it while any other references it.
	Layers []string `json:"layers"`
	// Stored is true once the snapshot and its layers are all in the store,
	// so any node can restore it.
	Stored bool `json:"stored"`
}

type SnapshotList struct {
	Snapshots []Snapshot `json:"snapshots"`
}

// Event types on a nodelet's GET /v1/events.
const (
	EventVM       = "vm"       // data: VM
	EventSnapshot = "snapshot" // data: Snapshot, once it is stored
	EventNode     = "node"     // data: Node, when it stops being ready (a drain)
)

// Activity is the agent's /internal/activity, passed through.
type Activity struct {
	IdleMS           int64 `json:"idle_ms"`
	SessionsAttached int   `json:"sessions_attached"`
	Tasks            int   `json:"tasks"`
	Busy             bool  `json:"busy"`
}

// Headers on the guest's host channel (docs/NODE.md).
const (
	HeaderSandbox = "X-Loam-Sandbox"
	// UpgradeAgent is the Upgrade token of the agent stream.
	UpgradeAgent = "loam-agent"
)
