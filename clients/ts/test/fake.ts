// A fake loam-api for the tests: plain routes, WebSocket routes with a
// server-side frame codec of its own (written apart from src/ws.ts, so the
// two don't share a mistake), and the key check every request goes through.

import { createHash } from "node:crypto";
import * as http from "node:http";
import type { AddressInfo, Socket } from "node:net";

export const TOKEN = "loam_test";

export interface FakeReq {
  method: string;
  path: string;
  query: URLSearchParams;
  headers: http.IncomingHttpHeaders;
  body: Buffer;
}

export type Handler = (req: FakeReq, res: http.ServerResponse) => void | Promise<void>;
export type WsHandler = (req: FakeReq, ws: ServerWs) => void | Promise<void>;

export function sendJSON(res: http.ServerResponse, status: number, v: unknown, headers: Record<string, string> = {}): void {
  const b = JSON.stringify(v);
  res.writeHead(status, { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(b), ...headers });
  res.end(b);
}

export function apiError(res: http.ServerResponse, status: number, code: string, message: string, headers: Record<string, string> = {}): void {
  sendJSON(res, status, { error: { code, message } }, headers);
}

/** RefuseUpgrade, thrown by a WsHandler before it touches the socket, answers the upgrade with an error. */
export class RefuseUpgrade {
  constructor(
    readonly status: number,
    readonly code: string,
    readonly message: string,
  ) {}
}

export class Fake {
  readonly requests: FakeReq[] = [];
  private routes = new Map<string, Handler>();
  private wsRoutes = new Map<string, WsHandler>();
  private server: http.Server;
  private sockets = new Set<Socket>();
  url = "";
  auth = true;

  constructor() {
    this.server = http.createServer((req, res) => void this.serve(req, res));
    this.server.on("connection", (s: Socket) => {
      this.sockets.add(s);
      s.on("close", () => this.sockets.delete(s));
    });
    this.server.on("upgrade", (req, socket: Socket, head: Buffer) => void this.upgrade(req, socket, head));
  }

  on(method: string, path: string, h: Handler): this {
    this.routes.set(`${method} ${path}`, h);
    return this;
  }

  ws(path: string, h: WsHandler): this {
    this.wsRoutes.set(path, h);
    return this;
  }

  async listen(socketPath?: string): Promise<this> {
    await new Promise<void>((r) => (socketPath ? this.server.listen(socketPath, r) : this.server.listen(0, "127.0.0.1", r)));
    if (!socketPath) this.url = `http://127.0.0.1:${(this.server.address() as AddressInfo).port}`;
    return this;
  }

  async close(): Promise<void> {
    for (const s of this.sockets) s.destroy();
    await new Promise((r) => this.server.close(r));
  }

  last(): FakeReq {
    return this.requests[this.requests.length - 1]!;
  }

  private async read(req: http.IncomingMessage): Promise<FakeReq> {
    const chunks: Buffer[] = [];
    for await (const c of req) chunks.push(c as Buffer);
    const u = new URL(req.url ?? "/", "http://fake");
    const r = { method: req.method ?? "GET", path: u.pathname, query: u.searchParams, headers: req.headers, body: Buffer.concat(chunks) };
    this.requests.push(r);
    return r;
  }

  private authorized(req: http.IncomingMessage): boolean {
    return !this.auth || req.headers.authorization === `Bearer ${TOKEN}`;
  }

  private async serve(req: http.IncomingMessage, res: http.ServerResponse): Promise<void> {
    const r = await this.read(req);
    if (!this.authorized(req)) return apiError(res, 401, "unauthorized", "no key");
    const h = this.routes.get(`${r.method} ${r.path}`);
    if (!h) return apiError(res, 404, "not_found", `no route ${r.method} ${r.path}`);
    try {
      await h(r, res);
    } catch (err) {
      if (!res.headersSent) apiError(res, 500, "internal", String(err));
    }
  }

  private async upgrade(req: http.IncomingMessage, socket: Socket, head: Buffer): Promise<void> {
    const u = new URL(req.url ?? "/", "http://fake");
    const r: FakeReq = { method: "GET", path: u.pathname, query: u.searchParams, headers: req.headers, body: Buffer.alloc(0) };
    this.requests.push(r);
    const refuse = (status: number, code: string, message: string) => {
      const b = JSON.stringify({ error: { code, message } });
      socket.end(
        `HTTP/1.1 ${status} ${http.STATUS_CODES[status]}\r\nContent-Type: application/json\r\nContent-Length: ${Buffer.byteLength(b)}\r\nConnection: close\r\n\r\n${b}`,
      );
    };
    if (!this.authorized(req)) return refuse(401, "unauthorized", "no key");
    const h = this.wsRoutes.get(r.path);
    if (!h) return refuse(404, "not_found", `no route ${r.path}`);
    const ws = new ServerWs(socket, head);
    const accept = createHash("sha1")
      .update(String(req.headers["sec-websocket-key"]) + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")
      .digest("base64");
    try {
      // The handler may refuse before the 101 goes out.
      await h(r, ws.deferUntilAccepted(() =>
        socket.write(`HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`),
      ));
    } catch (err) {
      if (err instanceof RefuseUpgrade && !ws.accepted) return refuse(err.status, err.code, err.message);
      socket.destroy();
    }
  }
}

export interface ServerMsg {
  binary: boolean;
  data: Buffer;
}

/** ServerWs is the server's side of a WebSocket: it reads masked frames and writes plain ones. */
export class ServerWs {
  accepted = false;
  /** The close code the client sent, once it has. */
  clientClose?: number;
  private socket: Socket;
  private buf: Buffer;
  private queue: ServerMsg[] = [];
  private waiters: ((m: ServerMsg | null) => void)[] = [];
  private ended = false;
  private accept?: () => void;

  constructor(socket: Socket, head: Buffer) {
    this.socket = socket;
    this.buf = Buffer.from(head);
    socket.on("data", (c: Buffer) => {
      this.buf = Buffer.concat([this.buf, c]);
      this.parse();
    });
    socket.on("close", () => this.end());
    socket.on("error", () => this.end());
  }

  deferUntilAccepted(accept: () => void): this {
    this.accept = accept;
    return this;
  }

  private ensureAccepted(): void {
    if (this.accepted) return;
    this.accepted = true;
    this.accept?.();
    if (this.buf.length) this.parse();
  }

  private parse(): void {
    if (!this.accepted) return;
    for (;;) {
      if (this.buf.length < 2) return;
      const op = this.buf[0]! & 0x0f;
      const fin = (this.buf[0]! & 0x80) !== 0;
      if ((this.buf[1]! & 0x80) === 0) throw new Error("fake: an unmasked client frame");
      let len = this.buf[1]! & 0x7f;
      let off = 2;
      if (len === 126) {
        if (this.buf.length < 4) return;
        len = this.buf.readUInt16BE(2);
        off = 4;
      } else if (len === 127) {
        if (this.buf.length < 10) return;
        len = Number(this.buf.readBigUInt64BE(2));
        off = 10;
      }
      if (this.buf.length < off + 4 + len) return;
      const mask = this.buf.subarray(off, off + 4);
      const data = Buffer.alloc(len);
      for (let i = 0; i < len; i++) data[i] = this.buf[off + 4 + i]! ^ mask[i % 4]!;
      this.buf = this.buf.subarray(off + 4 + len);
      if (!fin) throw new Error("fake: the client fragmented a message");
      if (op === 0x8) {
        this.clientClose = data.length >= 2 ? data.readUInt16BE(0) : 1005;
        this.frame(0x8, data.subarray(0, 2));
        this.socket.end();
        this.end();
        return;
      }
      if (op === 0x9) {
        this.frame(0xa, data);
        continue;
      }
      if (op === 0xa) continue;
      const m = { binary: op === 0x2, data };
      const w = this.waiters.shift();
      if (w) w(m);
      else this.queue.push(m);
    }
  }

  private end(): void {
    if (this.ended) return;
    this.ended = true;
    for (const w of this.waiters.splice(0)) w(null);
  }

  next(): Promise<ServerMsg | null> {
    this.ensureAccepted();
    const m = this.queue.shift();
    if (m) return Promise.resolve(m);
    if (this.ended) return Promise.resolve(null);
    return new Promise((r) => this.waiters.push(r));
  }

  /** frame writes one frame; `fin` false starts or continues a fragmented message. */
  frame(op: number, data: Buffer, fin = true): void {
    this.ensureAccepted();
    const len = data.length;
    let h: Buffer;
    if (len < 126) {
      h = Buffer.from([0, len]);
    } else if (len < 65536) {
      h = Buffer.alloc(4);
      h[1] = 126;
      h.writeUInt16BE(len, 2);
    } else {
      h = Buffer.alloc(10);
      h[1] = 127;
      h.writeBigUInt64BE(BigInt(len), 2);
    }
    h[0] = (fin ? 0x80 : 0) | op;
    this.socket.write(Buffer.concat([h, data]));
  }

  text(v: unknown): void {
    this.frame(0x1, Buffer.from(typeof v === "string" ? v : JSON.stringify(v)));
  }

  binary(first: number, data: Buffer | string): void {
    this.frame(0x2, Buffer.concat([Buffer.from([first]), Buffer.from(data)]));
  }

  close(code = 1000): void {
    this.ensureAccepted();
    const b = Buffer.alloc(2);
    b.writeUInt16BE(code, 0);
    this.frame(0x8, b);
    this.socket.end();
  }
}

/** sandbox is a Sandbox record for answers. */
export function sandbox(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: "sb-7k2q9xw4m3ab",
    name: "scratch",
    project: "jake",
    template: "base",
    image: "",
    state: "running",
    resources: { cpus: 2, memory_mib: 2048, disk_gib: 20 },
    env: {},
    labels: {},
    idle: { timeout_s: 300, action: "pause" },
    deadline: null,
    protected: false,
    http_port: 8080,
    network: { egress: [] },
    secrets: [],
    url: "https://scratch--jake.loam.example.com",
    urls: ["https://scratch.apps.example.com"],
    created_at: "2026-10-11T18:00:00Z",
    updated_at: "2026-10-11T18:00:00Z",
    last_active_at: "2026-10-11T18:00:00Z",
    ...over,
  };
}
