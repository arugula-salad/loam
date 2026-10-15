// LoamClient talks to one loam-api as one key: projects and keys (with the
// admin token), sandboxes, templates, secrets, network defaults and events.
// A sandbox's own routes are on LoamClient.sandbox's SandboxApi.

import { Transport, type RequestOptions, type RetryOptions } from "./http.js";
import { InsideSandbox, SandboxApi, linked, query } from "./sandbox.js";
import { EventStream } from "./streams.js";
import type {
  CreateSandbox,
  Key,
  Network,
  Project,
  PutSecret,
  Rule,
  Sandbox,
  SandboxList,
  SandboxState,
  Secret,
  SecretList,
  Template,
  TemplateList,
  UpdateSandbox,
  Whoami,
} from "./types.js";

/** INSIDE_SOCKET is where the agent serves the sandbox sub-API, inside a sandbox. */
export const INSIDE_SOCKET = "/run/loam/api.sock";

export interface ClientOptions {
  /** The API's base URL, without /v1, such as https://api.loam.example.com. Required. */
  url: string;
  /** The API key (or the admin token, for the admin routes). Required. */
  token: string;
  /**
   * Retry a `capacity` 503 after its Retry-After. Off by default: nothing is
   * retried unless asked.
   */
  retryCapacity?: boolean | RetryOptions;
  /** A fetch to use in place of the global one. */
  fetch?: typeof fetch;
}

export interface ListOptions extends RequestOptions {
  /** Every label must match. */
  labels?: Record<string, string>;
  state?: SandboxState;
}

/** ForkOptions are a fork's own settings; template and resources come from the source. */
export type ForkOptions = Omit<CreateSandbox, "from" | "template" | "image" | "resources"> & RequestOptions;

export interface EventsOptions extends RequestOptions {
  /** Narrow the stream to one sandbox (its ID or name). */
  sandbox?: string;
}

export class LoamClient {
  /** The API's base URL. */
  readonly url: string;
  private t: Transport;

  constructor(o: ClientOptions) {
    if (!o.url) {
      // There is no default loam: every one is someone's own.
      throw new Error("loam: no url (the API's base URL, such as https://api.loam.example.com)");
    }
    if (!o.token) {
      // Refused rather than a client that answers 401 to everything.
      throw new Error("loam: no token");
    }
    this.url = o.url.replace(/\/+$/, "");
    this.t = new Transport({ url: this.url, token: o.token, fetch: o.fetch, retryCapacity: o.retryCapacity });
  }

  /**
   * fromEnv returns a client configured by LOAM_API_URL and LOAM_TOKEN.
   * Either missing is an error.
   */
  static fromEnv(o: Omit<ClientOptions, "url" | "token"> = {}): LoamClient {
    const url = process.env.LOAM_API_URL;
    if (!url) throw new Error("LOAM_API_URL is not set (the API's base URL, such as https://api.loam.example.com)");
    const token = process.env.LOAM_TOKEN;
    if (!token) throw new Error("LOAM_TOKEN is not set");
    return new LoamClient({ ...o, url, token });
  }

  /**
   * inside returns the sandbox sub-API as the agent serves it to processes
   * inside a sandbox, on its Unix socket. It needs no key.
   */
  static inside(o: { socketPath?: string; retryCapacity?: boolean | RetryOptions } = {}): InsideSandbox {
    const t = new Transport({ url: "http://loam", socketPath: o.socketPath ?? INSIDE_SOCKET, retryCapacity: o.retryCapacity });
    return new InsideSandbox(t, "", "");
  }

  /** sandbox returns the sub-API of one sandbox, by ID or name. It makes no request. */
  sandbox(sandbox: string): SandboxApi {
    if (!sandbox) throw new Error("loam: no sandbox");
    return new SandboxApi(this.t, sandbox, `/v1/sandboxes/${encodeURIComponent(sandbox)}`);
  }

  // Projects and keys. These need the admin token as the client's token.

  createProject(name: string, o: RequestOptions = {}): Promise<Project> {
    return this.t.json<Project>("POST", "/v1/projects", { body: { name }, signal: o.signal });
  }

  async listProjects(o: RequestOptions = {}): Promise<Project[]> {
    const l = await this.t.json<{ projects: Project[] }>("GET", "/v1/projects", o);
    return l.projects;
  }

  /** deleteProject is a 409 `project_not_empty` while the project has sandboxes. */
  async deleteProject(name: string, o: RequestOptions = {}): Promise<void> {
    await this.t.json("DELETE", `/v1/projects/${encodeURIComponent(name)}`, o);
  }

  /** createKey makes a key in project; its `secret` is set only in this answer. */
  createKey(project: string, name: string, o: RequestOptions = {}): Promise<Key> {
    return this.t.json<Key>("POST", `/v1/projects/${encodeURIComponent(project)}/keys`, { body: { name }, signal: o.signal });
  }

  async listKeys(project: string, o: RequestOptions = {}): Promise<Key[]> {
    const l = await this.t.json<{ keys: Key[] }>("GET", `/v1/projects/${encodeURIComponent(project)}/keys`, o);
    return l.keys;
  }

  async deleteKey(project: string, key: string, o: RequestOptions = {}): Promise<void> {
    await this.t.json("DELETE", `/v1/projects/${encodeURIComponent(project)}/keys/${encodeURIComponent(key)}`, o);
  }

  /** whoami names the key's project and the key itself. */
  whoami(o: RequestOptions = {}): Promise<Whoami> {
    return this.t.json<Whoami>("GET", "/v1/whoami", o);
  }

  // Sandboxes.

  /**
   * createSandbox answers once the sandbox is running: create waits for the
   * boot. From a template (`template`, default "base"), an image (`image`, a
   * digest reference) or a checkpoint (`from`, a fork).
   */
  createSandbox(req: CreateSandbox = {}, o: RequestOptions = {}): Promise<Sandbox> {
    return this.t.json<Sandbox>("POST", "/v1/sandboxes", { body: req, signal: o.signal });
  }

  /**
   * fork starts a new sandbox from a checkpoint of another: its disk, and its
   * memory when the checkpoint has memory. It takes the source's network and
   * secrets unless given its own.
   */
  fork(sandbox: string, checkpoint: string, o: ForkOptions = {}): Promise<Sandbox> {
    const { signal, ...rest } = o;
    return this.createSandbox({ ...rest, from: { sandbox, checkpoint } }, { signal });
  }

  async listSandboxes(o: ListOptions = {}): Promise<Sandbox[]> {
    const labels = Object.entries(o.labels ?? {})
      .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
      .map(([k, v]) => `${k}=${v}`);
    const l = await this.t.json<SandboxList>("GET", "/v1/sandboxes", { query: query({ label: labels, state: o.state }), signal: o.signal });
    return l.sandboxes;
  }

  getSandbox(sandbox: string, o: RequestOptions = {}): Promise<Sandbox> {
    return this.t.json<Sandbox>("GET", sbPath(sandbox), o);
  }

  /**
   * updateSandbox patches the record: fields left out are left alone, and
   * `deadline: null` clears the deadline. A new env reaches processes started
   * after it and every later wake; network and secrets apply at once.
   */
  updateSandbox(sandbox: string, patch: UpdateSandbox, o: RequestOptions = {}): Promise<Sandbox> {
    return this.t.json<Sandbox>("PATCH", sbPath(sandbox), { body: patch, signal: o.signal });
  }

  /** deleteSandbox deletes the sandbox and its checkpoints; a protected one is a `conflict`. */
  async deleteSandbox(sandbox: string, o: RequestOptions = {}): Promise<void> {
    await this.t.json("DELETE", sbPath(sandbox), o);
  }

  pause(sandbox: string, o: RequestOptions = {}): Promise<Sandbox> {
    return this.t.json<Sandbox>("POST", sbPath(sandbox) + "/pause", o);
  }

  stop(sandbox: string, o: RequestOptions = {}): Promise<Sandbox> {
    return this.t.json<Sandbox>("POST", sbPath(sandbox) + "/stop", o);
  }

  wake(sandbox: string, o: RequestOptions = {}): Promise<Sandbox> {
    return this.t.json<Sandbox>("POST", sbPath(sandbox) + "/wake", o);
  }

  async listTemplates(o: RequestOptions = {}): Promise<Template[]> {
    const l = await this.t.json<TemplateList>("GET", "/v1/templates", o);
    return l.templates;
  }

  // Network and secrets. A deployment without a secret key answers the secret
  // routes with the code `unsupported`.

  /** networkDefaults is the rule list {"include": "defaults"} stands for. */
  async networkDefaults(o: RequestOptions = {}): Promise<Rule[]> {
    const n = await this.t.json<Network>("GET", "/v1/network/defaults", o);
    return n.egress;
  }

  /**
   * putSecret creates or replaces a project secret. A replaced value reaches
   * the awake sandboxes that use it before this resolves. The answer never
   * holds the value, and nothing can read it back.
   */
  putSecret(name: string, req: PutSecret, o: RequestOptions = {}): Promise<Secret> {
    return this.t.json<Secret>("PUT", `/v1/secrets/${encodeURIComponent(name)}`, { body: req, signal: o.signal });
  }

  getSecret(name: string, o: RequestOptions = {}): Promise<Secret> {
    return this.t.json<Secret>("GET", `/v1/secrets/${encodeURIComponent(name)}`, o);
  }

  async listSecrets(o: RequestOptions = {}): Promise<Secret[]> {
    const l = await this.t.json<SecretList>("GET", "/v1/secrets", o);
    return l.secrets;
  }

  /** deleteSecret is a `conflict` while a sandbox names the secret. */
  async deleteSecret(name: string, o: RequestOptions = {}): Promise<void> {
    await this.t.json("DELETE", `/v1/secrets/${encodeURIComponent(name)}`, o);
  }

  // Events.

  /**
   * events streams the project's events (best effort: after a reconnect,
   * read the state from the API). Iterate it; close() ends it.
   */
  async events(o: EventsOptions = {}): Promise<EventStream> {
    const abort = linked(o.signal);
    const res = await this.t.request("GET", "/v1/events", { query: query({ sandbox: o.sandbox }), signal: abort.signal });
    return new EventStream(res.body ?? new Blob([]).stream(), abort);
  }
}

function sbPath(sandbox: string): string {
  return `/v1/sandboxes/${encodeURIComponent(sandbox)}`;
}

/**
 * portURL is the URL of a port of a sandbox: `https://<port>--<name>--<project>.<domain>`
 * from its own URL, or `https://<port>--<name>.<domain>` from one of its `urls`.
 * Every port has one, private (the project's key needed) unless set public.
 */
export function portURL(sandboxURL: string | Pick<Sandbox, "url">, port: number): string {
  const u = new URL(typeof sandboxURL === "string" ? sandboxURL : sandboxURL.url);
  u.hostname = `${port}--${u.hostname}`;
  return u.toString().replace(/\/$/, "");
}

/** portURLs is portURL for the sandbox's own URL and each of its project's URL domains. */
export function portURLs(sandbox: Pick<Sandbox, "url" | "urls">, port: number): string[] {
  return [sandbox.url, ...(sandbox.urls ?? [])].filter(Boolean).map((u) => portURL(u, port));
}
