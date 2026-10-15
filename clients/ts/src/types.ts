// The types of loam's public API (docs/API.md), as they are on the wire: the
// same names and the same snake_case fields as pkg/api's JSON. The doc is the
// contract; pkg/api and these follow it. Times are RFC 3339 strings, as sent.

/** Project owns sandboxes and API keys. */
export interface Project {
  name: string;
  /** The project's URL domains (docs/API.md, "Ports"), from the operator's --domains file. */
  domains: string[];
  created_at: string;
}

/** Key is an API key. `secret` is set only in the answer that created it. */
export interface Key {
  id: string;
  name: string;
  project: string;
  secret?: string;
  created_at: string;
}

export interface Whoami {
  project: Project;
  key: Key;
}

/** ErrorBody is the body of every error answer. */
export interface ErrorBody {
  error: ErrorDetail;
}

export interface ErrorDetail {
  code: string;
  message: string;
}

/** The error codes clients rely on (docs/API.md, "Errors"). */
export const Codes = {
  Unauthorized: "unauthorized",
  NotFound: "not_found",
  AlreadyExists: "already_exists",
  Invalid: "invalid",
  Unsupported: "unsupported",
  Capacity: "capacity",
  SandboxUnavailable: "sandbox_unavailable",
  Conflict: "conflict",
  PermissionDenied: "permission_denied",
  ProjectNotEmpty: "project_not_empty",
  PortClosed: "port_closed",
} as const;

export type SandboxState = "creating" | "running" | "paused" | "stopped" | "error";
export type IdleAction = "pause" | "stop" | "none";
export type DeadlineAction = "delete" | "pause" | "stop";

export interface Resources {
  cpus: number;
  memory_mib: number;
  disk_gib: number;
}

export interface Idle {
  /** Defaults to 300; the minimum is 30. */
  timeout_s: number;
  action: IdleAction;
}

export interface Deadline {
  /** RFC 3339. */
  at: string;
  action: DeadlineAction;
}

export interface Sandbox {
  id: string;
  name: string;
  project: string;
  /** "" when the sandbox was made from an image. */
  template: string;
  /** The digest reference the sandbox was made from, or "" for a template. */
  image: string;
  state: SandboxState;
  error?: string;
  resources: Resources;
  env: Record<string, string>;
  labels: Record<string, string>;
  idle: Idle;
  deadline: Deadline | null;
  protected: boolean;
  http_port: number;
  network: Network;
  secrets: string[];
  /** The sandbox's own URL, which goes to http_port. */
  url: string;
  /** The same, under each of its project's URL domains. */
  urls: string[];
  created_at: string;
  updated_at: string;
  last_active_at: string;
}

/** From names a checkpoint to start a sandbox from (a fork). */
export interface From {
  sandbox: string;
  checkpoint: string;
}

export interface CreateSandbox {
  name?: string;
  /** Defaults to "base". Excludes `image`. */
  template?: string;
  /** `<repository>@sha256:<digest>` from an allowed registry, never a tag. Excludes `template`. */
  image?: string;
  resources?: Resources;
  env?: Record<string, string>;
  labels?: Record<string, string>;
  idle?: Partial<Idle>;
  deadline?: Deadline;
  protected?: boolean;
  http_port?: number;
  /** Left out, a fork takes its source's, and anything else none. */
  network?: Network;
  /** Left out, a fork takes its source's, and anything else none. */
  secrets?: string[];
  /** A fork: template and resources then come from the source and may not be given. */
  from?: From;
}

/**
 * UpdateSandbox is a PATCH: fields left out are left alone. `deadline: null`
 * clears the deadline. `env`, `labels`, `network` and `secrets` replace the
 * whole value; in `idle`, a field left out keeps its current value.
 */
export interface UpdateSandbox {
  env?: Record<string, string>;
  labels?: Record<string, string>;
  idle?: Partial<Idle>;
  deadline?: Deadline | null;
  protected?: boolean;
  http_port?: number;
  network?: Network;
  secrets?: string[];
}

export interface SandboxList {
  sandboxes: Sandbox[];
}

/** Exec stream frame bytes (docs/API.md, "Exec"): the first byte of a binary frame. */
export const FrameByte = {
  Stdin: 0,
  Stdout: 1,
  Stderr: 2,
  StdinEOF: 4,
} as const;

/** ExecMessage is a text frame on an exec or attach socket, either way. */
export interface ExecMessage {
  type: "started" | "attached" | "exit" | "error" | "resize" | "signal" | string;
  session?: string;
  pid?: number;
  offset?: number;
  code?: number;
  cols?: number;
  rows?: number;
  signal?: string;
  error?: ErrorDetail;
}

export interface Session {
  id: string;
  cmd: string[];
  tty: boolean;
  pid: number;
  started_at: string;
  exited: boolean;
  exit_code?: number;
  attached: boolean;
}

export interface SessionList {
  sessions: Session[];
}

/** ExecRun is the one-shot exec's request as sent: `stdin` is base64. */
export interface ExecRun {
  cmd: string[];
  stdin?: string;
  cwd?: string;
  env?: Record<string, string>;
  user?: string;
  /** Default 60, at most 3600. */
  timeout_s?: number;
}

/** ExecResult is the one-shot exec's answer as sent: `stdout` and `stderr` are base64. */
export interface ExecResult {
  exit_code: number;
  stdout: string;
  stderr: string;
  timed_out: boolean;
  truncated?: boolean;
}

export type FileType = "file" | "dir" | "symlink" | "other";

export interface Stat {
  path: string;
  name: string;
  type: FileType;
  size: number;
  /** Octal, "0644". */
  mode: string;
  mtime: string;
  target?: string;
}

export interface StatList {
  entries: Stat[];
}

export interface Mkdir {
  path: string;
  parents?: boolean;
  mode?: string;
}

export interface Rename {
  from: string;
  to: string;
}

export interface WatchEvent {
  type: "create" | "write" | "remove" | "rename" | "chmod";
  path: string;
}

export interface Port {
  port: number;
  address: string;
  pid: number;
  session?: string;
}

export interface PortList {
  ports: Port[];
}

export type PortAccess = "private" | "public";

/** PortSetting is loam-api's, never the agent's. */
export interface PortSetting {
  port: number;
  access: PortAccess;
  url: string;
}

export interface PortSettingList {
  ports: PortSetting[];
}

/**
 * Traffic is what loam-api counted at a sandbox's URLs (docs/API.md, "Stats").
 * `requests` includes WebSocket upgrades, which are in no status class.
 */
export interface Traffic {
  requests: number;
  status_2xx: number;
  status_3xx: number;
  status_4xx: number;
  status_5xx: number;
  websockets: number;
  bytes_sent: number;
  /** Requests carrying the project's key: you, not your visitors. */
  key_requests: number;
}

/** StatsDay is one day (UTC, `2006-01-02`) of a sandbox's traffic. */
export interface StatsDay extends Traffic {
  day: string;
  /** The day's distinct visitors, by address and User-Agent. */
  visitors: number;
}

/** Stats is GET /v1/sandboxes/{sandbox}/stats: every day of the window, today last. */
export interface Stats {
  sandbox: string;
  name: string;
  days: StatsDay[];
  total: Traffic;
  /** The days' visitors added up: a visitor on three days counts three times. */
  visitor_days: number;
}

export interface ServiceSpec {
  cmd: string[];
  cwd?: string;
  env?: Record<string, string>;
  user?: string;
  needs?: string[];
  http_port?: number;
}

export type ServiceState = "running" | "starting" | "stopped" | "crashed";

export interface Service extends ServiceSpec {
  name: string;
  state: ServiceState;
  pid?: number;
  restarts: number;
  started_at?: string;
}

export interface ServiceList {
  services: Service[];
}

export interface Checkpoint {
  id: string;
  sandbox: string;
  comment: string;
  memory: boolean;
  auto: boolean;
  created_at: string;
}

export interface CreateCheckpoint {
  comment?: string;
  /** Default true. A memory checkpoint pauses the sandbox for about a second. */
  memory?: boolean;
}

export interface CheckpointList {
  checkpoints: Checkpoint[];
}

export interface Template {
  name: string;
  image: string;
  resources: Resources;
}

export interface TemplateList {
  templates: Template[];
}

export type EventType =
  | "sandbox.created"
  | "sandbox.running"
  | "sandbox.paused"
  | "sandbox.stopped"
  | "sandbox.deleted"
  | "sandbox.error"
  | "checkpoint.created"
  | "checkpoint.restored"
  | "checkpoint.deleted";

export interface Event {
  type: EventType | string;
  /** The sandbox's ID. */
  sandbox: string;
  at: string;
  /** For example `reason` (`node_lost`, `vm_lost`), `snapshot_at`, `moved`. */
  detail?: Record<string, unknown>;
}

/** Task is a keep-awake hold on a sandbox (docs/API.md, "Tasks"). */
export interface Task {
  name: string;
  started_at: string;
  expires_at: string;
}

export interface TaskList {
  tasks: Task[];
}

/** PutTask creates or refreshes a task. `expire_s` defaults to, and is at most, 3600. */
export interface PutTask {
  expire_s?: number;
}

/** Network is what a sandbox may reach (docs/API.md, "Network"). No rules is the whole internet. */
export interface Network {
  egress: Rule[];
}

/** Rule is one egress rule: a domain with an action, or an include. */
export interface Rule {
  domain?: string;
  action?: "allow" | "deny";
  /** Allow only; default [80, 443]. */
  ports?: number[];
  include?: "defaults";
}

/** Secret is a project secret as the API shows it: never its value. */
export interface Secret {
  name: string;
  hosts: string[];
  placeholder: string;
  inject: Inject | null;
  created_at: string;
  updated_at: string;
}

/**
 * Inject sets a header from a secret on every request to its hosts, in place
 * of substituting its placeholder. `format` holds "{value}" once.
 */
export interface Inject {
  header: string;
  format: string;
}

export interface PutSecret {
  value: string;
  hosts: string[];
  inject?: Inject;
  placeholder_prefix?: string;
}

export interface SecretList {
  secrets: Secret[];
}

/** IdentityToken is GET /identity/token's answer, inside a sandbox. */
export interface IdentityToken {
  token: string;
  expires_at: string;
}
