// The transport under every route: fetch for plain requests, dialWebSocket for
// the streams, and the API's error shape for anything that isn't 2xx.

import * as http from "node:http";
import { Readable } from "node:stream";
import { Codes } from "./types.js";
import { LoamError, errorFromBody } from "./errors.js";
import { dialWebSocket, type WsConn } from "./ws.js";

/** What a request body may be: bytes, text, or a stream of bytes (web or Node). */
export type BodySource =
  | string
  | Uint8Array
  | ArrayBuffer
  | Blob
  | ReadableStream<Uint8Array>
  | AsyncIterable<Uint8Array | string>;

/**
 * RetryOptions turn on the one retry the contract documents: a `capacity`
 * 503 (no node can take the sandbox), after its Retry-After. Nothing else is
 * retried, and a request whose body is a stream is never retried, since the
 * stream can't be read twice.
 */
export interface RetryOptions {
  /** Tries in all, the first included. Default 5. */
  attempts?: number;
  /** The most seconds to spend waiting across the retries. Default 120. */
  maxWaitS?: number;
}

export interface TransportOptions {
  url: string;
  token?: string;
  socketPath?: string;
  fetch?: typeof fetch;
  retryCapacity?: boolean | RetryOptions;
}

export interface RequestOptions {
  signal?: AbortSignal;
}

export type Query = URLSearchParams | undefined;

export class Transport {
  readonly url: string;
  readonly socketPath?: string;
  private token?: string;
  private fetchImpl: typeof fetch;
  private retry?: Required<RetryOptions>;

  constructor(o: TransportOptions) {
    this.url = o.url.replace(/\/+$/, "");
    this.token = o.token;
    this.socketPath = o.socketPath;
    this.fetchImpl = o.fetch ?? (o.socketPath ? unixFetch(o.socketPath) : (globalThis.fetch as typeof fetch));
    if (typeof this.fetchImpl !== "function") {
      throw new Error("loam: this runtime has no fetch; Node 20 or later is needed");
    }
    if (o.retryCapacity) {
      const r = o.retryCapacity === true ? {} : o.retryCapacity;
      this.retry = { attempts: r.attempts ?? 5, maxWaitS: r.maxWaitS ?? 120 };
    }
  }

  private headers(contentType?: string): Record<string, string> {
    const h: Record<string, string> = {};
    if (this.token) h["Authorization"] = `Bearer ${this.token}`;
    if (contentType) h["Content-Type"] = contentType;
    return h;
  }

  /**
   * request sends one request and answers with the response when it is 2xx;
   * anything else is read into a LoamError and thrown.
   */
  async request(
    method: string,
    path: string,
    o: { query?: Query; body?: BodySource; contentType?: string; signal?: AbortSignal } = {},
  ): Promise<Response> {
    const url = this.url + path + (o.query && [...o.query.keys()].length > 0 ? `?${o.query}` : "");
    const { body, stream } = normalizeBody(o.body);
    return this.withRetry(!stream, o.signal, async () => {
      const init: RequestInit & { duplex?: "half" } = {
        method,
        headers: this.headers(o.body === undefined ? undefined : o.contentType),
        signal: o.signal,
      };
      if (body !== undefined) init.body = body as RequestInit["body"];
      if (stream) init.duplex = "half";
      const res = await this.fetchImpl(url, init);
      if (res.status >= 200 && res.status < 300) return res;
      throw await readError(res);
    });
  }

  /** json sends `body` as JSON (when given) and decodes the answer (undefined for 204). */
  async json<T>(method: string, path: string, o: { query?: Query; body?: unknown; signal?: AbortSignal } = {}): Promise<T> {
    const res = await this.request(method, path, {
      query: o.query,
      body: o.body === undefined ? undefined : JSON.stringify(o.body),
      contentType: "application/json",
      signal: o.signal,
    });
    if (res.status === 204) {
      await res.body?.cancel();
      return undefined as T;
    }
    const text = await res.text();
    if (text === "") return undefined as T;
    try {
      return JSON.parse(text) as T;
    } catch (err) {
      throw new Error(`${method} ${path}: decoding the answer: ${(err as Error).message}`);
    }
  }

  /** dial opens a WebSocket on path; a refused upgrade throws its LoamError. */
  dial(path: string, query: Query, signal?: AbortSignal): Promise<WsConn> {
    const u = new URL(this.url + path);
    u.protocol = u.protocol === "https:" ? "wss:" : "ws:";
    if (query) u.search = query.toString();
    return this.withRetry(true, signal, () =>
      dialWebSocket(u, { headers: this.headers(), socketPath: this.socketPath, signal }),
    );
  }

  private async withRetry<T>(replayable: boolean, signal: AbortSignal | undefined, attempt: () => Promise<T>): Promise<T> {
    let waited = 0;
    for (let n = 1; ; n++) {
      try {
        return await attempt();
      } catch (err) {
        const r = this.retry;
        if (!r || !replayable || n >= r.attempts || !(err instanceof LoamError) || err.code !== Codes.Capacity) throw err;
        const delay = err.retryAfter ?? 1;
        if (waited + delay > r.maxWaitS) throw err;
        waited += delay;
        await sleep(delay * 1000, signal);
      }
    }
  }
}

async function readError(res: Response): Promise<LoamError> {
  let raw = "";
  try {
    raw = await res.text();
  } catch {
    // The body broke off: the status is what there is.
  }
  return errorFromBody(res.status, res.statusText, res.headers.get("retry-after"), raw.slice(0, 64 << 10));
}

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason);
    const t = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(t);
      reject(signal?.reason);
    };
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

/** normalizeBody makes a body fetch takes, and says whether it is a stream (never retried). */
function normalizeBody(b: BodySource | undefined): { body?: string | Uint8Array | Blob | ReadableStream<Uint8Array>; stream: boolean } {
  if (b === undefined) return { stream: false };
  if (typeof b === "string" || b instanceof Uint8Array || b instanceof Blob) return { body: b, stream: false };
  if (b instanceof ArrayBuffer) return { body: new Uint8Array(b), stream: false };
  if (typeof (b as ReadableStream).getReader === "function") return { body: b as ReadableStream<Uint8Array>, stream: true };
  if (typeof (b as AsyncIterable<unknown>)[Symbol.asyncIterator] === "function") {
    const r = b instanceof Readable ? b : Readable.from(b as AsyncIterable<Uint8Array | string>);
    return { body: Readable.toWeb(r) as unknown as ReadableStream<Uint8Array>, stream: true };
  }
  throw new TypeError("loam: a body is a string, bytes, a Blob or a stream of bytes");
}

/**
 * unixFetch is enough of fetch to reach an HTTP server on a Unix socket: the
 * agent's /run/loam/api.sock, inside a sandbox. Node's fetch can't dial one
 * without undici's Agent, which isn't exposed without the undici package.
 */
export function unixFetch(socketPath: string): typeof fetch {
  return ((input: string | URL, init: RequestInit = {}) =>
    new Promise<Response>((resolve, reject) => {
      const u = new URL(input.toString());
      const req = http.request({
        socketPath,
        path: u.pathname + u.search,
        method: init.method ?? "GET",
        headers: { Host: u.host, ...(init.headers as Record<string, string> | undefined) },
        signal: init.signal ?? undefined,
      });
      req.on("error", reject);
      req.on("response", (res) => {
        const headers = new Headers();
        for (const [k, v] of Object.entries(res.headers)) {
          if (v !== undefined) headers.set(k, Array.isArray(v) ? v.join(", ") : v);
        }
        const status = res.statusCode ?? 0;
        const empty = status === 204 || status === 205 || status === 304 || init.method === "HEAD";
        if (empty) res.resume();
        resolve(
          new Response(empty ? null : (Readable.toWeb(res) as unknown as ReadableStream<Uint8Array>), {
            status,
            statusText: res.statusMessage,
            headers,
          }),
        );
      });
      const body = init.body;
      if (body === undefined || body === null) {
        req.end();
      } else if (typeof body === "string" || body instanceof Uint8Array) {
        req.end(body);
      } else if (body instanceof Blob) {
        body.arrayBuffer().then((ab) => req.end(Buffer.from(ab)), reject);
      } else if (typeof (body as ReadableStream).getReader === "function") {
        const src = Readable.fromWeb(body as unknown as import("node:stream/web").ReadableStream<Uint8Array>);
        src.on("error", (err) => req.destroy(err));
        src.pipe(req);
      } else {
        reject(new TypeError("loam: unsupported body over a Unix socket"));
      }
    })) as typeof fetch;
}
