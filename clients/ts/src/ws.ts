// A small RFC 6455 WebSocket client over node:http, for the exec, attach,
// file watch and port connect streams.
//
// It is here rather than Node's global WebSocket for three reasons. The
// contract answers a refused upgrade with the API's error shape
// (docs/API.md, "Errors": not_found, port_closed, capacity, ...), and the
// WHATWG WebSocket only ever says "error" and closes with 1006, so the code
// would be lost. The sandbox's own socket (/run/loam/api.sock) is a Unix
// socket, which the global WebSocket can't dial. And Node 20 has no global
// WebSocket at all. The alternative is the `ws` package; this is the part of it
// loam needs: client frames, fragmentation, ping/pong and the close handshake,
// without extensions or subprotocols.

import { createHash, randomBytes } from "node:crypto";
import * as http from "node:http";
import * as https from "node:https";
import type { Socket } from "node:net";
import { errorFromBody } from "./errors.js";

const GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11";

/** The largest message accepted from the server; more closes with 1009. */
const MAX_MESSAGE = 64 << 20;

/** Past this many unread messages the socket stops reading until they are taken. */
const HIGH_WATER = 64;

const OP_CONT = 0x0;
const OP_TEXT = 0x1;
const OP_BINARY = 0x2;
const OP_CLOSE = 0x8;
const OP_PING = 0x9;
const OP_PONG = 0xa;

export interface WsMessage {
  binary: boolean;
  data: Buffer;
}

export interface WsDialOptions {
  headers?: Record<string, string>;
  /** Dial this Unix socket instead of the URL's host. */
  socketPath?: string;
  signal?: AbortSignal;
  handshakeTimeoutMs?: number;
}

/**
 * dialWebSocket opens a WebSocket on url (ws: or wss:). A refused handshake
 * (any answer but 101) rejects with a LoamError read from its body.
 */
export function dialWebSocket(url: URL, opts: WsDialOptions = {}): Promise<WsConn> {
  return new Promise((resolve, reject) => {
    const key = randomBytes(16).toString("base64");
    const secure = url.protocol === "wss:" || url.protocol === "https:";
    const mod = secure ? https : http;
    let settled = false;
    const fail = (err: unknown) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      opts.signal?.removeEventListener("abort", onAbort);
      req.destroy();
      reject(err);
    };
    const onAbort = () => fail(opts.signal?.reason ?? new Error("aborted"));
    const req = mod.request({
      protocol: secure ? "https:" : "http:",
      hostname: url.hostname.replace(/^\[(.*)\]$/, "$1"),
      port: url.port || (secure ? 443 : 80),
      socketPath: opts.socketPath,
      path: url.pathname + url.search,
      method: "GET",
      headers: {
        ...opts.headers,
        Connection: "Upgrade",
        Upgrade: "websocket",
        "Sec-WebSocket-Key": key,
        "Sec-WebSocket-Version": "13",
      },
    });
    const timer = setTimeout(
      () => fail(new Error(`websocket: no handshake answer from ${url.host || opts.socketPath} in ${(opts.handshakeTimeoutMs ?? 45_000) / 1000}s`)),
      opts.handshakeTimeoutMs ?? 45_000,
    );
    if (opts.signal?.aborted) return onAbort();
    opts.signal?.addEventListener("abort", onAbort, { once: true });
    req.on("error", fail);
    req.on("response", (res) => {
      // Not upgraded: the body is the API's error.
      const chunks: Buffer[] = [];
      let size = 0;
      res.on("data", (c: Buffer) => {
        if (size < 64 << 10) chunks.push(c);
        size += c.length;
      });
      res.on("error", fail);
      res.on("end", () => {
        const ra = res.headers["retry-after"];
        fail(errorFromBody(res.statusCode ?? 0, res.statusMessage ?? "", typeof ra === "string" ? ra : null, Buffer.concat(chunks).toString("utf8")));
      });
    });
    req.on("upgrade", (res, socket: Socket, head: Buffer) => {
      const want = createHash("sha1").update(key + GUID).digest("base64");
      if (res.headers["sec-websocket-accept"] !== want) {
        socket.destroy();
        return fail(new Error("websocket: the server's Sec-WebSocket-Accept is wrong"));
      }
      if (settled) {
        socket.destroy();
        return;
      }
      settled = true;
      clearTimeout(timer);
      opts.signal?.removeEventListener("abort", onAbort);
      const conn = new WsConn(socket, head);
      if (opts.signal) {
        // The signal outlives the handshake: aborting it later drops the connection.
        const drop = () => conn.destroy();
        opts.signal.addEventListener("abort", drop, { once: true });
        conn.closed.then(() => opts.signal?.removeEventListener("abort", drop));
      }
      resolve(conn);
    });
    req.end();
  });
}

/** WsConn is an open WebSocket, client side. */
export class WsConn {
  /** The close status the server sent: 1005 for none, 1006 when the connection just broke. */
  closeCode?: number;
  closeReason = "";
  /** Resolves once the connection is gone. */
  readonly closed: Promise<void>;

  private socket: Socket;
  private chunks: Buffer[] = [];
  private buffered = 0;
  private fragments: Buffer[] = [];
  private fragmentOp = 0;
  private fragmentSize = 0;
  private queue: WsMessage[] = [];
  private waiters: ((m: WsMessage | null) => void)[] = [];
  private ended = false;
  private closeSent = false;
  private resolveClosed!: () => void;

  constructor(socket: Socket, head: Buffer) {
    this.socket = socket;
    this.closed = new Promise((r) => (this.resolveClosed = r));
    socket.setNoDelay(true);
    socket.on("data", (c: Buffer) => this.onData(c));
    socket.on("error", () => this.finish(1006));
    socket.on("close", () => this.finish(1006));
    if (head.length > 0) this.onData(head);
  }

  /** next resolves with the next message, or null once the connection has closed. */
  next(): Promise<WsMessage | null> {
    const m = this.queue.shift();
    if (m) {
      if (this.queue.length < HIGH_WATER / 2 && this.socket.isPaused()) this.socket.resume();
      return Promise.resolve(m);
    }
    if (this.ended) return Promise.resolve(null);
    return new Promise((r) => this.waiters.push(r));
  }

  /** send writes one message; it resolves once the frame is handed to the socket. */
  send(data: Buffer | string, binary = typeof data !== "string"): Promise<void> {
    const payload = typeof data === "string" ? Buffer.from(data, "utf8") : data;
    return this.write(binary ? OP_BINARY : OP_TEXT, payload);
  }

  /**
   * close starts the close handshake and resolves when the connection is gone:
   * when the server answers, or after a second without an answer.
   */
  async close(code = 1000, reason = ""): Promise<void> {
    if (!this.ended && !this.closeSent) {
      const r = Buffer.from(reason, "utf8");
      const payload = Buffer.alloc(2 + r.length);
      payload.writeUInt16BE(code, 0);
      r.copy(payload, 2);
      this.closeSent = true;
      await this.write(OP_CLOSE, payload).catch(() => {});
    }
    const timer = setTimeout(() => this.destroy(), 1000);
    await this.closed;
    clearTimeout(timer);
  }

  /** destroy drops the connection without a close handshake. */
  destroy(): void {
    this.socket.destroy();
    this.finish(1006);
  }

  private write(op: number, payload: Buffer): Promise<void> {
    if (this.ended || (this.closeSent && op !== OP_CLOSE)) {
      return Promise.reject(new Error("websocket: the connection is closed"));
    }
    const len = payload.length;
    let header: Buffer;
    if (len < 126) {
      header = Buffer.alloc(6);
      header[1] = 0x80 | len;
    } else if (len < 65536) {
      header = Buffer.alloc(8);
      header[1] = 0x80 | 126;
      header.writeUInt16BE(len, 2);
    } else {
      header = Buffer.alloc(14);
      header[1] = 0x80 | 127;
      header.writeBigUInt64BE(BigInt(len), 2);
    }
    header[0] = 0x80 | op;
    // Every client frame is masked (RFC 6455, 5.3).
    const mask = randomBytes(4);
    mask.copy(header, header.length - 4);
    const masked = Buffer.allocUnsafe(len);
    for (let i = 0; i < len; i++) masked[i] = payload[i]! ^ mask[i & 3]!;
    return new Promise((resolve, reject) => {
      this.socket.write(Buffer.concat([header, masked]), (err) => (err ? reject(err) : resolve()));
    });
  }

  private onData(chunk: Buffer): void {
    this.chunks.push(chunk);
    this.buffered += chunk.length;
    for (;;) {
      const frame = this.readFrame();
      if (!frame) return;
      this.onFrame(frame.fin, frame.op, frame.payload);
      if (this.ended) return;
    }
  }

  /** peek returns a buffer that starts with the first n buffered bytes, joining chunks only when it must. */
  private peek(n: number): Buffer {
    if (this.chunks[0]!.length < n) {
      this.chunks = [Buffer.concat(this.chunks)];
    }
    return this.chunks[0]!;
  }

  private consume(n: number): void {
    this.buffered -= n;
    while (n > 0) {
      const c = this.chunks[0]!;
      if (c.length <= n) {
        this.chunks.shift();
        n -= c.length;
      } else {
        this.chunks[0] = c.subarray(n);
        n = 0;
      }
    }
  }

  private readFrame(): { fin: boolean; op: number; payload: Buffer } | null {
    if (this.buffered < 2) return null;
    let b = this.peek(2);
    const fin = (b[0]! & 0x80) !== 0;
    const op = b[0]! & 0x0f;
    const masked = (b[1]! & 0x80) !== 0;
    let len = b[1]! & 0x7f;
    let off = 2;
    if (len === 126) {
      if (this.buffered < 4) return null;
      b = this.peek(4);
      len = b.readUInt16BE(2);
      off = 4;
    } else if (len === 127) {
      if (this.buffered < 10) return null;
      b = this.peek(10);
      const big = b.readBigUInt64BE(2);
      if (big > BigInt(MAX_MESSAGE)) {
        this.fail(1009, "message too big");
        return null;
      }
      len = Number(big);
      off = 10;
    }
    const maskOff = off;
    if (masked) off += 4;
    if (this.buffered < off + len) return null;
    b = this.peek(off + len);
    const payload = Buffer.from(b.subarray(off, off + len));
    if (masked) {
      for (let i = 0; i < len; i++) payload[i]! ^= b[maskOff + (i & 3)]!;
    }
    this.consume(off + len);
    return { fin, op, payload };
  }

  private onFrame(fin: boolean, op: number, payload: Buffer): void {
    switch (op) {
      case OP_CLOSE: {
        const code = payload.length >= 2 ? payload.readUInt16BE(0) : 1005;
        this.closeReason = payload.length > 2 ? payload.subarray(2).toString("utf8") : "";
        this.closeCode = code;
        if (!this.closeSent) {
          // Echo the close, then the server ends the TCP connection (RFC 6455, 7.1.1).
          this.closeSent = true;
          const echo = Buffer.alloc(2);
          echo.writeUInt16BE(code === 1005 ? 1000 : code, 0);
          this.write(OP_CLOSE, echo).catch(() => {});
        }
        this.socket.end();
        this.finish(code);
        return;
      }
      case OP_PING:
        this.write(OP_PONG, payload).catch(() => {});
        return;
      case OP_PONG:
        return;
      case OP_TEXT:
      case OP_BINARY:
        if (this.fragments.length > 0) return this.fail(1002, "a new message inside a fragmented one");
        if (fin) return this.deliver(op, payload);
        this.fragmentOp = op;
        this.fragments = [payload];
        this.fragmentSize = payload.length;
        return;
      case OP_CONT:
        if (this.fragments.length === 0) return this.fail(1002, "a continuation with nothing to continue");
        this.fragments.push(payload);
        this.fragmentSize += payload.length;
        if (this.fragmentSize > MAX_MESSAGE) return this.fail(1009, "message too big");
        if (fin) {
          const data = Buffer.concat(this.fragments);
          this.fragments = [];
          this.fragmentSize = 0;
          this.deliver(this.fragmentOp, data);
        }
        return;
      default:
        this.fail(1002, `unknown opcode ${op}`);
    }
  }

  private deliver(op: number, data: Buffer): void {
    const m = { binary: op === OP_BINARY, data };
    const w = this.waiters.shift();
    if (w) return w(m);
    this.queue.push(m);
    if (this.queue.length >= HIGH_WATER) this.socket.pause();
  }

  /** fail closes the connection over a protocol error on the server's side. */
  private fail(code: number, reason: string): void {
    if (!this.closeSent) {
      const r = Buffer.from(reason, "utf8");
      const payload = Buffer.alloc(2 + r.length);
      payload.writeUInt16BE(code, 0);
      r.copy(payload, 2);
      this.closeSent = true;
      this.write(OP_CLOSE, payload).catch(() => {});
    }
    this.socket.end();
    setTimeout(() => this.socket.destroy(), 1000).unref();
    this.finish(code);
  }

  private finish(code: number): void {
    if (this.closeCode === undefined) this.closeCode = code;
    if (this.ended) return;
    this.ended = true;
    // Messages already read stay readable; the socket has nothing more to give.
    if (this.socket.isPaused()) this.socket.resume();
    for (const w of this.waiters.splice(0)) w(null);
    this.resolveClosed();
  }
}
