// The sandbox sub-API: every route under /v1/sandboxes/{sandbox}/ that the
// agent also serves inside the sandbox on /run/loam/api.sock, without the
// prefix (docs/API.md, "Inside a sandbox"). One class serves both, so code
// written against it runs outside with a key or inside with none.

import type { Duplex } from "node:stream";
import { ExecStream, startStream, type AttachOptions, type ExecOptions } from "./exec.js";
import type { BodySource, RequestOptions, Transport } from "./http.js";
import { FileWatch, PortWatch, portStream } from "./streams.js";
import type {
  Checkpoint,
  CheckpointList,
  CreateCheckpoint,
  ExecResult,
  ExecRun,
  IdentityToken,
  Port,
  PortAccess,
  PortList,
  PortSetting,
  PortSettingList,
  Stats,
  PutTask,
  Sandbox,
  Service,
  ServiceList,
  ServiceSpec,
  Session,
  SessionList,
  Stat,
  StatList,
  Task,
  TaskList,
} from "./types.js";

/** query builds a query string, leaving out what is undefined, false or empty. */
export function query(params: Record<string, string | number | boolean | string[] | undefined>): URLSearchParams {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === false || v === "") continue;
    if (Array.isArray(v)) for (const s of v) q.append(k, s);
    else q.set(k, String(v));
  }
  return q;
}

/** envList is env as sorted K=V pairs, so requests are reproducible. */
function envList(env: Record<string, string> | undefined): string[] {
  return Object.entries(env ?? {})
    .map(([k, v]) => `${k}=${v}`)
    .sort();
}

/** FsOptions go with every file operation. */
export interface FsOptions extends RequestOptions {
  /** Run the operation as this user, whose permissions then apply. Without it, file operations run as root. */
  user?: string;
}

export interface WriteFileOptions extends FsOptions {
  /** The mode of a file it creates: 0o644 or "0644". */
  mode?: number | string;
  /** Create missing parent directories. */
  mkdir?: boolean;
}

export interface MkdirOptions extends FsOptions {
  parents?: boolean;
  mode?: number | string;
}

export interface RecursiveOptions extends FsOptions {
  recursive?: boolean;
}

/** RunOptions are the one-shot exec's request; `stdin` is sent as base64 for you. */
export interface RunOptions extends RequestOptions {
  cmd: string[];
  stdin?: string | Uint8Array;
  cwd?: string;
  env?: Record<string, string>;
  user?: string;
  /** Default 60, at most 3600. */
  timeout_s?: number;
}

/** RunResult is the one-shot exec's answer with its output decoded. */
export interface RunResult {
  exit_code: number;
  stdout: Buffer;
  stderr: Buffer;
  timed_out: boolean;
  /** Output beyond 16 MiB per stream was cut. */
  truncated: boolean;
}

export interface LogOptions extends RequestOptions {
  /** Keep the stream open for new output until it is cancelled or the signal aborts. */
  follow?: boolean;
  /** The last N lines; left out, all that is kept. */
  tail?: number;
}

function octal(mode: number | string | undefined): string | undefined {
  if (mode === undefined) return undefined;
  return typeof mode === "number" ? (mode & 0o7777).toString(8).padStart(4, "0") : mode;
}

/**
 * SandboxApi is one sandbox's sub-API: exec, files, ports, services, tasks
 * and checkpoints. Get one from LoamClient.sandbox, or LoamClient.inside from
 * within a sandbox.
 */
export class SandboxApi {
  /** The sandbox's ID or name ("" inside the sandbox, where it is implicit). */
  readonly sandbox: string;
  protected t: Transport;
  private prefix: string;

  /** @internal */
  constructor(t: Transport, sandbox: string, prefix: string) {
    this.t = t;
    this.sandbox = sandbox;
    this.prefix = prefix;
  }

  protected path(...parts: (string | number)[]): string {
    return this.prefix + parts.map((p) => "/" + encodeURIComponent(String(p))).join("");
  }

  // Exec.

  /**
   * exec starts cmd and resolves once the session has started. The output
   * flows to o.stdout and o.stderr until wait() settles.
   */
  async exec(o: ExecOptions): Promise<ExecStream> {
    if (!o.cmd || o.cmd.length === 0) throw new Error("exec: no command");
    const q = query({
      cmd: o.cmd,
      tty: o.tty,
      cols: o.cols && o.rows ? o.cols : undefined,
      rows: o.cols && o.rows ? o.rows : undefined,
      cwd: o.cwd,
      env: envList(o.env),
      user: o.user,
      detachable: o.detachable,
    });
    const ws = await this.t.dial(this.path("exec"), q, o.signal);
    return startStream(ws, "started", o);
  }

  /** attach joins a session again: its kept output is replayed from the offset, then it is live. */
  async attach(session: string, o: AttachOptions = {}): Promise<ExecStream> {
    const ws = await this.t.dial(this.path("exec", "sessions", session), query({ offset: o.offset }), o.signal);
    return startStream(ws, "attached", o);
  }

  /** sessions lists the exec sessions, running and recently exited. */
  async sessions(o: RequestOptions = {}): Promise<Session[]> {
    const l = await this.t.json<SessionList>("GET", this.path("exec", "sessions"), o);
    return l.sessions;
  }

  /** signalSession signals a session's process group without a stream to it. */
  async signalSession(session: string, signal: string, o: RequestOptions = {}): Promise<void> {
    await this.t.json("POST", this.path("exec", "sessions", session, "signal"), { body: { signal }, signal: o.signal });
  }

  /** run is the one-shot exec: it answers when the command has exited (or timed out) with all of its output. */
  async run(o: RunOptions): Promise<RunResult> {
    const body: ExecRun = { cmd: o.cmd };
    if (o.stdin !== undefined) body.stdin = Buffer.from(o.stdin).toString("base64");
    if (o.cwd) body.cwd = o.cwd;
    if (o.env) body.env = o.env;
    if (o.user) body.user = o.user;
    if (o.timeout_s) body.timeout_s = o.timeout_s;
    const r = await this.t.json<ExecResult>("POST", this.path("exec", "run"), { body, signal: o.signal });
    return {
      exit_code: r.exit_code,
      stdout: Buffer.from(r.stdout ?? "", "base64"),
      stderr: Buffer.from(r.stderr ?? "", "base64"),
      timed_out: !!r.timed_out,
      truncated: !!r.truncated,
    };
  }

  // Files. Paths are absolute inside the sandbox.

  /** openFile streams a file's bytes. */
  async openFile(path: string, o: FsOptions = {}): Promise<ReadableStream<Uint8Array>> {
    const res = await this.t.request("GET", this.path("fs", "file"), { query: query({ path, user: o.user }), signal: o.signal });
    return res.body ?? new Blob([]).stream();
  }

  /** readFile reads a whole file. */
  async readFile(path: string, o: FsOptions = {}): Promise<Buffer> {
    const res = await this.t.request("GET", this.path("fs", "file"), { query: query({ path, user: o.user }), signal: o.signal });
    return Buffer.from(await res.arrayBuffer());
  }

  /** writeFile writes data to path, replacing what was there. */
  async writeFile(path: string, data: BodySource, o: WriteFileOptions = {}): Promise<Stat> {
    const res = await this.t.request("PUT", this.path("fs", "file"), {
      query: query({ path, mode: octal(o.mode), mkdir: o.mkdir, user: o.user }),
      body: data,
      contentType: "application/octet-stream",
      signal: o.signal,
    });
    return (await res.json()) as Stat;
  }

  /** remove deletes a file, or with `recursive` a directory tree. */
  async remove(path: string, o: RecursiveOptions = {}): Promise<void> {
    await this.t.json("DELETE", this.path("fs", "file"), { query: query({ path, recursive: o.recursive, user: o.user }), signal: o.signal });
  }

  stat(path: string, o: FsOptions = {}): Promise<Stat> {
    return this.t.json<Stat>("GET", this.path("fs", "stat"), { query: query({ path, user: o.user }), signal: o.signal });
  }

  /** list lists a directory. */
  async list(path: string, o: FsOptions = {}): Promise<Stat[]> {
    const l = await this.t.json<StatList>("GET", this.path("fs", "list"), { query: query({ path, user: o.user }), signal: o.signal });
    return l.entries;
  }

  mkdir(path: string, o: MkdirOptions = {}): Promise<Stat> {
    const body: { path: string; parents?: boolean; mode?: string } = { path };
    if (o.parents) body.parents = true;
    if (o.mode !== undefined) body.mode = octal(o.mode);
    return this.t.json<Stat>("POST", this.path("fs", "mkdir"), { query: query({ user: o.user }), body, signal: o.signal });
  }

  rename(from: string, to: string, o: FsOptions = {}): Promise<Stat> {
    return this.t.json<Stat>("POST", this.path("fs", "rename"), { query: query({ user: o.user }), body: { from, to }, signal: o.signal });
  }

  /** getTar streams a tar of the directory at path, its entry names relative to it. */
  async getTar(path: string, o: FsOptions = {}): Promise<ReadableStream<Uint8Array>> {
    const res = await this.t.request("GET", this.path("fs", "tar"), { query: query({ path, user: o.user }), signal: o.signal });
    return res.body ?? new Blob([]).stream();
  }

  /**
   * putTar extracts a tar (names relative to path) into the directory at
   * path, creating it and its parents if missing.
   */
  async putTar(path: string, tar: BodySource, o: FsOptions = {}): Promise<void> {
    const res = await this.t.request("PUT", this.path("fs", "tar"), {
      query: query({ path, user: o.user }),
      body: tar,
      contentType: "application/x-tar",
      signal: o.signal,
    });
    await res.body?.cancel();
  }

  /** watchFiles watches path (with `recursive`, everything under it). */
  async watchFiles(path: string, o: RecursiveOptions = {}): Promise<FileWatch> {
    const ws = await this.t.dial(this.path("fs", "watch"), query({ path, recursive: o.recursive, user: o.user }), o.signal);
    return new FileWatch(ws);
  }

  // Ports.

  /** ports lists what listens in the sandbox, on every address. */
  async ports(o: RequestOptions = {}): Promise<Port[]> {
    const l = await this.t.json<PortList>("GET", this.path("ports"), o);
    return l.ports;
  }

  /** watchPorts streams listeners opening and closing. Watching is not activity. */
  async watchPorts(o: RequestOptions = {}): Promise<PortWatch> {
    const abort = linked(o.signal);
    const res = await this.t.request("GET", this.path("ports", "watch"), { signal: abort.signal });
    return new PortWatch(res.body ?? new Blob([]).stream(), abort);
  }

  /**
   * connectPort opens a TCP stream to 127.0.0.1:port in the sandbox, carried
   * over a WebSocket. Nothing listening is a LoamError `port_closed`. The
   * connection holds the sandbox awake while it is open.
   */
  async connectPort(port: number, o: RequestOptions = {}): Promise<Duplex> {
    const ws = await this.t.dial(this.path("ports", port, "connect"), undefined, o.signal);
    return portStream(ws);
  }

  /** setPortAccess makes a port's URL "private" (the project's key needed) or "public". */
  setPortAccess(port: number, access: PortAccess, o: RequestOptions = {}): Promise<PortSetting> {
    return this.t.json<PortSetting>("PUT", this.path("ports", port), { body: { access }, signal: o.signal });
  }

  /** portSettings lists the ports whose access has been set. */
  async portSettings(o: RequestOptions = {}): Promise<PortSetting[]> {
    const l = await this.t.json<PortSettingList>("GET", this.path("ports", "settings"), o);
    return l.ports;
  }

  /** stats is the sandbox's traffic at its URLs over the last `days` days (default 7, at most 90). */
  async stats(o: RequestOptions & { days?: number } = {}): Promise<Stats> {
    return this.t.json<Stats>("GET", this.path("stats"), { query: query({ days: o.days }), signal: o.signal });
  }

  // Services.

  async listServices(o: RequestOptions = {}): Promise<Service[]> {
    const l = await this.t.json<ServiceList>("GET", this.path("services"), o);
    return l.services;
  }

  /** putService creates or replaces a service definition. */
  putService(name: string, spec: ServiceSpec, o: RequestOptions = {}): Promise<Service> {
    return this.t.json<Service>("PUT", this.path("services", name), { body: spec, signal: o.signal });
  }

  getService(name: string, o: RequestOptions = {}): Promise<Service> {
    return this.t.json<Service>("GET", this.path("services", name), o);
  }

  /** deleteService stops a service and removes its definition. */
  async deleteService(name: string, o: RequestOptions = {}): Promise<void> {
    await this.t.json("DELETE", this.path("services", name), o);
  }

  startService(name: string, o: RequestOptions = {}): Promise<Service> {
    return this.t.json<Service>("POST", this.path("services", name, "start"), o);
  }

  stopService(name: string, o: RequestOptions = {}): Promise<Service> {
    return this.t.json<Service>("POST", this.path("services", name, "stop"), o);
  }

  restartService(name: string, o: RequestOptions = {}): Promise<Service> {
    return this.t.json<Service>("POST", this.path("services", name, "restart"), o);
  }

  /**
   * serviceLogs streams a service's stdout and stderr, interleaved, as text.
   * With `follow` the stream stays open for new output.
   */
  async serviceLogs(name: string, o: LogOptions = {}): Promise<ReadableStream<Uint8Array>> {
    const res = await this.t.request("GET", this.path("services", name, "logs"), {
      query: query({ follow: o.follow, tail: o.tail && o.tail > 0 ? o.tail : undefined }),
      signal: o.signal,
    });
    return res.body ?? new Blob([]).stream();
  }

  // Tasks: keep-awake holds that expire.

  async listTasks(o: RequestOptions = {}): Promise<Task[]> {
    const l = await this.t.json<TaskList>("GET", this.path("tasks"), o);
    return l.tasks;
  }

  /** putTask creates the task or moves its expiry; `expire_s` defaults to, and is at most, 3600. */
  putTask(name: string, req: PutTask = {}, o: RequestOptions = {}): Promise<Task> {
    return this.t.json<Task>("PUT", this.path("tasks", name), { body: req, signal: o.signal });
  }

  getTask(name: string, o: RequestOptions = {}): Promise<Task> {
    return this.t.json<Task>("GET", this.path("tasks", name), o);
  }

  async deleteTask(name: string, o: RequestOptions = {}): Promise<void> {
    await this.t.json("DELETE", this.path("tasks", name), o);
  }

  // Checkpoints.

  /** createCheckpoint takes a checkpoint; a memory one (the default) pauses the sandbox for about a second. */
  createCheckpoint(req: CreateCheckpoint = {}, o: RequestOptions = {}): Promise<Checkpoint> {
    return this.t.json<Checkpoint>("POST", this.path("checkpoints"), { body: req, signal: o.signal });
  }

  /** listCheckpoints answers oldest first. */
  async listCheckpoints(o: RequestOptions = {}): Promise<Checkpoint[]> {
    const l = await this.t.json<CheckpointList>("GET", this.path("checkpoints"), o);
    return l.checkpoints;
  }

  getCheckpoint(checkpoint: string, o: RequestOptions = {}): Promise<Checkpoint> {
    return this.t.json<Checkpoint>("GET", this.path("checkpoints", checkpoint), o);
  }

  async deleteCheckpoint(checkpoint: string, o: RequestOptions = {}): Promise<void> {
    await this.t.json("DELETE", this.path("checkpoints", checkpoint), o);
  }

  /**
   * restoreCheckpoint keeps the current state as an automatic checkpoint
   * first, and ends every exec session and port stream on the sandbox (1011).
   */
  restoreCheckpoint(checkpoint: string, o: RequestOptions = {}): Promise<Sandbox> {
    return this.t.json<Sandbox>("POST", this.path("checkpoints", checkpoint, "restore"), o);
  }
}

/**
 * InsideSandbox is the sub-API as the agent serves it to processes inside the
 * sandbox, on /run/loam/api.sock. Being inside is the credential. Besides the
 * sub-API, three routes reach loam-api for the sandbox itself: self(),
 * createCheckpoint() (it answers before the pause) and identityToken().
 */
export class InsideSandbox extends SandboxApi {
  /** self is the sandbox's own record. */
  self(o: RequestOptions = {}): Promise<Sandbox> {
    return this.t.json<Sandbox>("GET", "/self", o);
  }

  /**
   * identityToken is an OpenID Connect token that proves which sandbox this
   * is, for `audience`. `ttl_s` defaults to 600, at most 3600.
   */
  identityToken(audience: string, o: RequestOptions & { ttl_s?: number } = {}): Promise<IdentityToken> {
    return this.t.json<IdentityToken>("GET", "/identity/token", { query: query({ audience, ttl_s: o.ttl_s }), signal: o.signal });
  }
}

/** linked is an AbortController that also aborts when signal does. */
export function linked(signal?: AbortSignal): AbortController {
  const c = new AbortController();
  if (signal) {
    if (signal.aborted) c.abort(signal.reason);
    else signal.addEventListener("abort", () => c.abort(signal.reason), { once: true });
  }
  return c;
}
