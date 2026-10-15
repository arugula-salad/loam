// The long-lived streams other than exec: the file watch (WebSocket), the
// ports watch and the event stream (Server-Sent Events), and a port
// connection (a TCP stream carried over a WebSocket).

import { Duplex } from "node:stream";
import { readSSE, type SSEMessage } from "./sse.js";
import type { Event, Port, WatchEvent } from "./types.js";
import type { WsConn } from "./ws.js";

/**
 * FileWatch is a stream of changes under a path. Iterate it; the iteration
 * ends when the server ends the watch or close() is called.
 */
export class FileWatch implements AsyncIterable<WatchEvent> {
  private ws: WsConn;

  /** @internal Use SandboxApi.watchFiles. */
  constructor(ws: WsConn) {
    this.ws = ws;
  }

  /** next resolves with the next change, or null once the watch has ended. */
  async next(): Promise<WatchEvent | null> {
    for (;;) {
      const m = await this.ws.next();
      if (m === null) {
        const cc = this.ws.closeCode;
        if (cc === 1000 || cc === 1005 || this.closing) return null;
        throw new Error(`watch: the stream broke (close status ${cc})`);
      }
      if (m.binary) continue;
      const text = m.data.toString("utf8");
      try {
        return JSON.parse(text) as WatchEvent;
      } catch {
        throw new Error(`watch: bad frame ${JSON.stringify(text)}`);
      }
    }
  }

  async *[Symbol.asyncIterator](): AsyncIterator<WatchEvent> {
    try {
      for (;;) {
        const ev = await this.next();
        if (ev === null) return;
        yield ev;
      }
    } finally {
      await this.close();
    }
  }

  private closing = false;

  close(): Promise<void> {
    this.closing = true;
    return this.ws.close(1000);
  }
}

/** SSEStream is the shared shape of the ports watch and the event stream. */
abstract class SSEStream<T> implements AsyncIterable<T> {
  private it: AsyncGenerator<SSEMessage>;
  private abort: AbortController;

  constructor(body: ReadableStream<Uint8Array>, abort: AbortController) {
    this.it = readSSE(body);
    this.abort = abort;
  }

  protected abstract decode(m: SSEMessage): T | undefined;

  /** next resolves with the next item, or null once the stream has ended. */
  async next(): Promise<T | null> {
    for (;;) {
      let r: IteratorResult<SSEMessage>;
      try {
        r = await this.it.next();
      } catch (err) {
        if (this.abort.signal.aborted) return null;
        throw err;
      }
      if (r.done) return null;
      const v = this.decode(r.value);
      if (v !== undefined) return v;
    }
  }

  async *[Symbol.asyncIterator](): AsyncIterator<T> {
    try {
      for (;;) {
        const v = await this.next();
        if (v === null) return;
        yield v;
      }
    } finally {
      this.close();
    }
  }

  close(): void {
    this.abort.abort();
    this.it.return(undefined).catch(() => {});
  }
}

/** PortEvent is a listener opening or closing. */
export interface PortEvent {
  type: "opened" | "closed";
  port: Port;
}

/**
 * PortWatch is the ports watch. Watching is not activity, so an open watch
 * doesn't keep the sandbox awake.
 */
export class PortWatch extends SSEStream<PortEvent> {
  protected decode(m: SSEMessage): PortEvent | undefined {
    if (m.event !== "opened" && m.event !== "closed") return undefined;
    try {
      return { type: m.event, port: JSON.parse(m.data) as Port };
    } catch {
      throw new Error(`ports watch: bad data ${JSON.stringify(m.data)}`);
    }
  }
}

/**
 * EventStream is the project's event stream. Events are best effort: after a
 * reconnect, read the state from the API.
 */
export class EventStream extends SSEStream<Event> {
  protected decode(m: SSEMessage): Event | undefined {
    let ev: Event;
    try {
      ev = JSON.parse(m.data) as Event;
    } catch {
      throw new Error(`events: bad data ${JSON.stringify(m.data)}`);
    }
    if (!ev.type) ev.type = m.event;
    return ev;
  }
}

/**
 * portStream is a Duplex over a port connection's WebSocket, whose binary
 * frames carry the bytes. The protocol has no half-close, so ending the
 * writable side closes both directions.
 */
export function portStream(ws: WsConn): Duplex {
  let reading = false;
  const d = new Duplex({
    read() {
      if (reading) return;
      reading = true;
      void (async () => {
        try {
          for (;;) {
            const m = await ws.next();
            if (m === null) {
              const cc = ws.closeCode;
              if (cc === 1000 || cc === 1001 || cc === 1005 || d.writableFinished) d.push(null);
              else d.destroy(new Error(`port connection: closed with ${cc}`));
              return;
            }
            if (!m.binary || m.data.length === 0) continue;
            if (!d.push(m.data)) return;
          }
        } finally {
          reading = false;
        }
      })();
    },
    write(chunk: Buffer, _enc, cb) {
      ws.send(chunk, true).then(() => cb(), cb);
    },
    final(cb) {
      ws.close(1000).then(() => cb(), cb);
    },
    destroy(err, cb) {
      ws.destroy();
      cb(err);
    },
  });
  return d;
}
