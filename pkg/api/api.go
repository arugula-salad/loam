// Package api holds the types of loam's public API (docs/API.md): what loam-api
// serves, the agent serves for the sandbox sub-API, and the CLI and the e2e
// suite send. The doc is the contract; these follow it.
package api

import (
	"bytes"
	"encoding/json"
	"time"
)

// Project owns sandboxes and API keys.
type Project struct {
	Name string `json:"name"`
	// Domains are the project's URL domains (docs/API.md, "Ports"), from
	// the operator's --domains file.
	Domains   []string  `json:"domains"`
	CreatedAt time.Time `json:"created_at"`
}

// Key is an API key. Secret is set only in the answer that created it.
type Key struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Project   string    `json:"project"`
	Scope     string    `json:"scope"`
	Secret    string    `json:"secret,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// A key's scope: full (the default) may do anything in its project; read may
// only read, with GET and HEAD and no WebSocket (exec and port connections
// are WebSocket upgrades of a GET).
const (
	ScopeFull = "full"
	ScopeRead = "read"
)

type Whoami struct {
	Project Project `json:"project"`
	Key     Key     `json:"key"`
}

// Error is the body of every error answer.
type Error struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// The error codes clients rely on (docs/API.md, "Errors").
const (
	CodeUnauthorized       = "unauthorized"
	CodeNotFound           = "not_found"
	CodeAlreadyExists      = "already_exists"
	CodeInvalid            = "invalid"
	CodeUnsupported        = "unsupported"
	CodeCapacity           = "capacity"
	CodeSandboxUnavailable = "sandbox_unavailable"
	CodeConflict           = "conflict"
	CodeProjectNotEmpty    = "project_not_empty"
	CodePortClosed         = "port_closed"
	CodePermissionDenied   = "permission_denied"
)

// Sandbox states.
const (
	StateCreating = "creating"
	StateRunning  = "running"
	StatePaused   = "paused"
	StateStopped  = "stopped"
	StateError    = "error"
)

// Idle and deadline actions.
const (
	ActionPause  = "pause"
	ActionStop   = "stop"
	ActionDelete = "delete"
	ActionNone   = "none"
)

type Resources struct {
	CPUs      int `json:"cpus"`
	MemoryMiB int `json:"memory_mib"`
	DiskGiB   int `json:"disk_gib"`
}

type Idle struct {
	TimeoutS int    `json:"timeout_s"`
	Action   string `json:"action"`
}

type Deadline struct {
	At     time.Time `json:"at"`
	Action string    `json:"action"`
}

type Sandbox struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Project      string            `json:"project"`
	Template     string            `json:"template"`
	Image        string            `json:"image"`
	State        string            `json:"state"`
	Error        string            `json:"error,omitempty"`
	Resources    Resources         `json:"resources"`
	Env          map[string]string `json:"env"`
	Labels       map[string]string `json:"labels"`
	Idle         Idle              `json:"idle"`
	Deadline     *Deadline         `json:"deadline"`
	Protected    bool              `json:"protected"`
	HTTPPort     int               `json:"http_port"`
	Network      Network           `json:"network"`
	Secrets      []string          `json:"secrets"`
	URL          string            `json:"url"`
	URLs         []string          `json:"urls"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	LastActiveAt time.Time         `json:"last_active_at"`
}

// From names a checkpoint to start a sandbox from (a fork).
type From struct {
	Sandbox    string `json:"sandbox"`
	Checkpoint string `json:"checkpoint"`
}

type CreateSandbox struct {
	Name     string `json:"name,omitempty"`
	Template string `json:"template,omitempty"`
	// Image is a digest reference, in place of Template.
	Image     string            `json:"image,omitempty"`
	Resources *Resources        `json:"resources,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	Idle      *Idle             `json:"idle,omitempty"`
	Deadline  *Deadline         `json:"deadline,omitempty"`
	Protected bool              `json:"protected,omitempty"`
	HTTPPort  int               `json:"http_port,omitempty"`
	// Network and Secrets left out take a fork's source's, or none.
	Network *Network  `json:"network,omitempty"`
	Secrets *[]string `json:"secrets,omitempty"`
	From    *From     `json:"from,omitempty"`
}

// UpdateSandbox is a PATCH: nil fields are left alone. Deadline is a pointer
// to a pointer so that `"deadline": null` clears it.
type UpdateSandbox struct {
	Env       map[string]string `json:"env,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	Idle      *Idle             `json:"idle,omitempty"`
	Deadline  **Deadline        `json:"deadline,omitempty"`
	Protected *bool             `json:"protected,omitempty"`
	HTTPPort  *int              `json:"http_port,omitempty"`
	Network   *Network          `json:"network,omitempty"`
	Secrets   *[]string         `json:"secrets,omitempty"`
}

// UnmarshalJSON keeps `"deadline": null` apart from no deadline field:
// encoding/json alone sets the outer pointer to nil for both. It is strict, as
// loam-api is with every body: a field it doesn't know is an error.
func (u *UpdateSandbox) UnmarshalJSON(b []byte) error {
	type plain UpdateSandbox
	var p plain
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if d, ok := raw["deadline"]; ok && string(bytes.TrimSpace(d)) == "null" {
		var none *Deadline
		p.Deadline = &none
	}
	*u = UpdateSandbox(p)
	return nil
}

type SandboxList struct {
	Sandboxes []Sandbox `json:"sandboxes"`
}

// Exec stream frame bytes (docs/API.md, "Exec"): the first byte of a binary frame.
const (
	StreamStdin    byte = 0
	StreamStdout   byte = 1
	StreamStderr   byte = 2
	StreamStdinEOF byte = 4
)

// ExecMessage is a text frame on an exec or attach socket, either way.
type ExecMessage struct {
	Type    string       `json:"type"` // started, attached, exit, error, resize, signal
	Session string       `json:"session,omitempty"`
	PID     int          `json:"pid,omitempty"`
	Offset  int64        `json:"offset,omitempty"`
	Code    *int         `json:"code,omitempty"`
	Cols    int          `json:"cols,omitempty"`
	Rows    int          `json:"rows,omitempty"`
	Signal  string       `json:"signal,omitempty"`
	Error   *ErrorDetail `json:"error,omitempty"`
}

// ExecAttached is the first frame on an attach socket. It is ExecMessage's
// "attached" with its offset always present, 0 included: ExecMessage leaves
// out what is empty, and the contract promises the offset.
type ExecAttached struct {
	Type    string `json:"type"` // attached
	Session string `json:"session"`
	Offset  int64  `json:"offset"`
}

type Session struct {
	ID        string    `json:"id"`
	Cmd       []string  `json:"cmd"`
	TTY       bool      `json:"tty"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Exited    bool      `json:"exited"`
	ExitCode  *int      `json:"exit_code,omitempty"`
	Attached  bool      `json:"attached"`
}

type SessionList struct {
	Sessions []Session `json:"sessions"`
}

type Signal struct {
	Signal string `json:"signal"`
}

// ExecRun and ExecResult are the one-shot exec. Byte fields are base64 in JSON.
type ExecRun struct {
	Cmd      []string          `json:"cmd"`
	Stdin    []byte            `json:"stdin,omitempty"`
	Cwd      string            `json:"cwd,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	User     string            `json:"user,omitempty"`
	TimeoutS int               `json:"timeout_s,omitempty"`
}

type ExecResult struct {
	ExitCode  int    `json:"exit_code"`
	Stdout    []byte `json:"stdout"`
	Stderr    []byte `json:"stderr"`
	TimedOut  bool   `json:"timed_out"`
	Truncated bool   `json:"truncated,omitempty"`
}

type Stat struct {
	Path   string    `json:"path"`
	Name   string    `json:"name"`
	Type   string    `json:"type"` // file, dir, symlink, other
	Size   int64     `json:"size"`
	Mode   string    `json:"mode"` // octal, "0644"
	MTime  time.Time `json:"mtime"`
	Target string    `json:"target,omitempty"`
}

type StatList struct {
	Entries []Stat `json:"entries"`
}

type Mkdir struct {
	Path    string `json:"path"`
	Parents bool   `json:"parents,omitempty"`
	Mode    string `json:"mode,omitempty"`
}

type Rename struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type WatchEvent struct {
	Type string `json:"type"` // create, write, remove, rename, chmod
	Path string `json:"path"`
}

type Port struct {
	Port    int    `json:"port"`
	Address string `json:"address"`
	PID     int    `json:"pid"`
	Session string `json:"session,omitempty"`
}

type PortList struct {
	Ports []Port `json:"ports"`
}

// PortSetting is loam-api's, never the agent's.
type PortSetting struct {
	Port   int    `json:"port"`
	Access string `json:"access"` // private, public
	URL    string `json:"url"`
}

type PortSettingList struct {
	Ports []PortSetting `json:"ports"`
}

// Traffic is what loam-api counted at a sandbox's URLs (docs/API.md, "Stats").
// Requests is every request, WebSockets included; the status classes count
// the answers that weren't upgrades.
type Traffic struct {
	Requests    int64 `json:"requests"`
	Status2xx   int64 `json:"status_2xx"`
	Status3xx   int64 `json:"status_3xx"`
	Status4xx   int64 `json:"status_4xx"`
	Status5xx   int64 `json:"status_5xx"`
	WebSockets  int64 `json:"websockets"`
	BytesSent   int64 `json:"bytes_sent"`
	KeyRequests int64 `json:"key_requests"` // carrying the project's key: you, not your visitors
}

// StatsDay is one day (UTC) of a sandbox's traffic.
type StatsDay struct {
	Day string `json:"day"` // 2006-01-02
	Traffic
	// Visitors is the day's distinct visitors: clients, by address and
	// User-Agent, that got an answer below 400 without the project's key.
	Visitors int64 `json:"visitors"`
}

// Stats is GET /v1/sandboxes/{sandbox}/stats: every day of the window, oldest
// first, today last.
type Stats struct {
	Sandbox string     `json:"sandbox"` // the ID
	Name    string     `json:"name"`
	Days    []StatsDay `json:"days"`
	Total   Traffic    `json:"total"`
	// VisitorDays is the days' Visitors added up: a visitor is only known
	// within a day, so one who came on three days counts three times.
	VisitorDays int64 `json:"visitor_days"`
}

type ServiceSpec struct {
	Cmd      []string          `json:"cmd"`
	Cwd      string            `json:"cwd,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	User     string            `json:"user,omitempty"`
	Needs    []string          `json:"needs,omitempty"`
	HTTPPort int               `json:"http_port,omitempty"`
}

type Service struct {
	Name string `json:"name"`
	ServiceSpec
	State     string     `json:"state"` // running, starting, stopped, crashed
	PID       int        `json:"pid,omitempty"`
	Restarts  int        `json:"restarts"`
	StartedAt *time.Time `json:"started_at,omitempty"`
}

type ServiceList struct {
	Services []Service `json:"services"`
}

type Checkpoint struct {
	ID        string    `json:"id"`
	Sandbox   string    `json:"sandbox"`
	Comment   string    `json:"comment"`
	Memory    bool      `json:"memory"`
	Auto      bool      `json:"auto"`
	CreatedAt time.Time `json:"created_at"`
}

type CreateCheckpoint struct {
	Comment string `json:"comment,omitempty"`
	Memory  *bool  `json:"memory,omitempty"`
}

type CheckpointList struct {
	Checkpoints []Checkpoint `json:"checkpoints"`
}

type Template struct {
	Name      string    `json:"name"`
	Image     string    `json:"image"`
	Resources Resources `json:"resources"`
}

type TemplateList struct {
	Templates []Template `json:"templates"`
}

type Event struct {
	Type    string         `json:"type"`
	Sandbox string         `json:"sandbox"`
	At      time.Time      `json:"at"`
	Detail  map[string]any `json:"detail,omitempty"`
}

// Task is a keep-awake hold on a sandbox (docs/API.md, "Tasks").
type Task struct {
	Name      string    `json:"name"`
	StartedAt time.Time `json:"started_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type TaskList struct {
	Tasks []Task `json:"tasks"`
}

// PutTask creates or refreshes a task. ExpireS defaults to, and is at most, 3600.
type PutTask struct {
	ExpireS int `json:"expire_s,omitempty"`
}

// Network is what a sandbox may reach (docs/API.md, "Network"). No rules is
// the whole internet.
// HeaderRule replaces request headers on the most specific matching HTTPS host.
// Values can reference E2B project secrets with ${e2b.secrets.NAME}.
type HeaderRule struct {
	Domain  string            `json:"domain"`
	Headers map[string]string `json:"headers"`
}

type Network struct {
	RequestHeaders []HeaderRule `json:"request_headers,omitempty"`
	Egress         []Rule       `json:"egress"`
}

// Rule is one egress rule: a domain with an action, or an include.
type Rule struct {
	Domain  string `json:"domain,omitempty"`
	Action  string `json:"action,omitempty"`  // allow, deny
	Ports   []int  `json:"ports,omitempty"`   // allow only; default 80 and 443
	Include string `json:"include,omitempty"` // "defaults"
}

// Rule actions.
const (
	RuleAllow = "allow"
	RuleDeny  = "deny"
)

// Secret is a project secret as the API shows it: never its value.
type Secret struct {
	Name        string    `json:"name"`
	Hosts       []string  `json:"hosts"`
	Placeholder string    `json:"placeholder"`
	Inject      *Inject   `json:"inject"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Inject sets a header from a secret on every request to its hosts, in
// place of substituting its placeholder. Format holds "{value}" once.
type Inject struct {
	Header string `json:"header"`
	Format string `json:"format"`
}

type PutSecret struct {
	Value             string   `json:"value"`
	Hosts             []string `json:"hosts"`
	Inject            *Inject  `json:"inject,omitempty"`
	PlaceholderPrefix string   `json:"placeholder_prefix,omitempty"`
}

type SecretList struct {
	Secrets []Secret `json:"secrets"`
}

// IdentityToken is GET /identity/token's answer, inside a sandbox.
type IdentityToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}
