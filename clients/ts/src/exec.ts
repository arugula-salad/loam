// Streaming exec and attach (docs/API.md, "Exec"): the frames, stdin, the TTY
// size, signals, and the exit status, or the lack of one.

import { ExitUnknownError, LoamError } from "./errors.js";
import { FrameByte, type ExecMessage } from "./types.js";
import type { WsConn } from "./ws.js";

/**
 * Sink takes a stream's output: a function, or anything with write(), such as
 * process.stdout or a Writable. Output with no sink is dropped.
 */
export type Sink = ((chunk: Buffer) => void) | { write(chunk: Buffer): unknown };

/**
 * StdinSource is copied to stdin and then closed (EOF sent). Leave stdin out
 * to write with ExecStream.write yourself.
 */
export type StdinSource = string | Uint8Array | ReadableStream<Uint8Array> | AsyncIterable<Uint8Array | string>;

/** StreamIO wires a stream to the caller. */
export interface StreamIO {
  stdin?: StdinSource;
  stdout?: Sink;
  stderr?: Sink;
  /** Aborting it drops the socket, as close() does. */
  signal?: AbortSignal;
}

/** ExecOptions describe a streaming exec. */
export interface ExecOptions extends StreamIO {
  /** argv; required. */
  cmd: string[];
  tty?: boolean;
  /** The TTY's initial size. */
  cols?: number;
  rows?: number;
  cwd?: string;
  /** On top of the sandbox's env. */
  env?: Record<string, string>;
  /** Default `loam`; `root` is allowed. */
  user?: string;
  /** The session outlives the socket; attach to it again with attach(). */
  detachable?: boolean;
}

/** AttachOptions describe an attach. */
export interface AttachOptions extends StreamIO {
  /** Replay from this byte of the session's output; left out, everything kept. */
  offset?: number;
}

/**
 * ExecStream is a running exec session, from exec() or attach(). Output flows
 * to the sinks until wait() settles.
 */
export class ExecStream {
  /** The session's ID. */
  readonly session: string;
  /** Its process (exec only; 0 for an attach). */
  readonly pid: number;
  /** Where an attach's replay began (0 for an exec). */
  readonly offset: number;

  private ws: WsConn;
  private io: StreamIO;
  private code?: number;
  private err?: Error;
  private readonly finished: Promise<void>;

  /** @internal Use SandboxApi.exec or SandboxApi.attach. */
  constructor(ws: WsConn, first: ExecMessage, io: StreamIO) {
    this.ws = ws;
    this.io = io;
    this.session = first.session ?? "";
    this.pid = first.pid ?? 0;
    this.offset = first.offset ?? 0;
    this.finished = this.read();
    if (io.stdin !== undefined) this.copyStdin(io.stdin);
  }

  /** write sends stdin bytes. */
  async write(data: string | Uint8Array): Promise<void> {
    const b = typeof data === "string" ? Buffer.from(data, "utf8") : data;
    if (b.length === 0) return;
    const frame = Buffer.allocUnsafe(1 + b.length);
    frame[0] = FrameByte.Stdin;
    frame.set(b, 1);
    await this.ws.send(frame, true);
  }

  /** closeStdin sends EOF on stdin. */
  closeStdin(): Promise<void> {
    return this.ws.send(Buffer.from([FrameByte.StdinEOF]), true);
  }

  /** resize sets the TTY's size. */
  resize(cols: number, rows: number): Promise<void> {
    return this.ws.send(JSON.stringify({ type: "resize", cols, rows } satisfies ExecMessage), false);
  }

  /**
   * signal signals the session's process group; name is as in `kill -l`
   * ("TERM", "INT", "KILL").
   */
  signal(name: string): Promise<void> {
    return this.ws.send(JSON.stringify({ type: "signal", signal: name } satisfies ExecMessage), false);
  }

  /** done resolves when the stream has ended, however it ended. */
  get done(): Promise<void> {
    return this.finished;
  }

  /**
   * wait resolves with the exit code once the stream ends; a killed process
   * exits 128+signal. Without an exit frame it rejects with ExitUnknownError,
   * and with a LoamError when the server sent an error frame.
   */
  async wait(): Promise<number> {
    await this.finished;
    if (this.err) throw this.err;
    if (this.code === undefined) throw new ExitUnknownError("");
    return this.code;
  }

  /**
   * close closes the socket: a detachable session goes on without it, any
   * other is hung up (SIGHUP to its process group).
   */
  async close(): Promise<void> {
    await this.ws.close(1000);
    await this.finished;
  }

  private async read(): Promise<void> {
    for (;;) {
      const m = await this.ws.next();
      if (m === null) break;
      if (m.binary) {
        if (m.data.length === 0) continue;
        const sink = m.data[0] === FrameByte.Stdout ? this.io.stdout : m.data[0] === FrameByte.Stderr ? this.io.stderr : undefined;
        if (sink) emit(sink, m.data.subarray(1));
        continue;
      }
      let msg: ExecMessage;
      try {
        msg = JSON.parse(m.data.toString("utf8")) as ExecMessage;
      } catch {
        continue;
      }
      if (msg.type === "exit" && typeof msg.code === "number") {
        this.code = msg.code;
      } else if (msg.type === "error" && msg.error) {
        this.err = new LoamError(500, msg.error.code, msg.error.message);
      }
    }
    if (this.code !== undefined || this.err) return;
    const cc = this.ws.closeCode;
    this.err =
      cc === 1011
        ? new ExitUnknownError("the sandbox went away", cc)
        : new ExitUnknownError(cc === 1006 || cc === undefined ? "the connection broke" : `closed with ${cc}`, cc);
  }

  private async copyStdin(src: StdinSource): Promise<void> {
    try {
      if (typeof src === "string" || src instanceof Uint8Array) {
        await this.write(src);
      } else if (typeof (src as ReadableStream).getReader === "function") {
        const reader = (src as ReadableStream<Uint8Array>).getReader();
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          await this.write(value);
        }
      } else {
        for await (const chunk of src as AsyncIterable<Uint8Array | string>) await this.write(chunk);
      }
      await this.closeStdin();
    } catch {
      // The stream ended under the copy; wait() says how.
    }
  }
}

function emit(sink: Sink, chunk: Buffer): void {
  // A copy: the sink may keep the chunk past the next frame.
  const c = Buffer.from(chunk);
  if (typeof sink === "function") sink(c);
  else sink.write(c);
}

/**
 * startStream reads the first frame, which must be want ("started" or
 * "attached") or an error, and then hands the socket to an ExecStream.
 */
export async function startStream(ws: WsConn, want: "started" | "attached", io: StreamIO): Promise<ExecStream> {
  const m = await ws.next();
  const fail = (err: Error) => {
    ws.destroy();
    return Promise.reject(err);
  };
  if (m === null) return fail(new Error(`exec: the socket closed before the first frame (${ws.closeCode})`));
  let first: ExecMessage;
  try {
    if (m.binary) throw new Error();
    first = JSON.parse(m.data.toString("utf8")) as ExecMessage;
  } catch {
    return fail(new Error(`exec: the first frame is not a "${want}" message`));
  }
  if (first.type === "error" && first.error) {
    return fail(new LoamError(400, first.error.code, first.error.message));
  }
  if (first.type !== want) return fail(new Error(`exec: first frame "${first.type}", want "${want}"`));
  return new ExecStream(ws, first, io);
}
