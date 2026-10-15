import assert from "node:assert/strict";
import { after, before, describe, test } from "node:test";
import { ExitUnknownError, LoamClient, LoamError, isCode, type ExecMessage } from "../src/index.js";
import { Fake, RefuseUpgrade, TOKEN, sendJSON } from "./fake.js";

const sb = "/v1/sandboxes/scratch";

/** collect is a Sink that keeps what it is given. */
function collect(): { sink: (b: Buffer) => void; text: () => string; bytes: () => Buffer } {
  const parts: Buffer[] = [];
  return { sink: (b) => parts.push(b), text: () => Buffer.concat(parts).toString(), bytes: () => Buffer.concat(parts) };
}

describe("exec and attach", () => {
  const fake = new Fake();
  let c: LoamClient;
  const seen: { text: ExecMessage[]; stdin: Buffer[]; eof: boolean } = { text: [], stdin: [], eof: false };
  before(async () => {
    await fake.listen();
    c = new LoamClient({ url: fake.url, token: TOKEN });
    fake.ws(`${sb}/exec`, async (r, ws) => {
      const cmd = r.query.getAll("cmd");
      if (cmd[0] === "missing-sandbox") throw new RefuseUpgrade(404, "not_found", "no sandbox scratch");
      if (cmd[0] === "full") throw new RefuseUpgrade(503, "capacity", "no room");
      if (cmd[0] === "nope") {
        ws.text({ type: "error", error: { code: "invalid", message: "no such program: nope" } });
        return ws.close();
      }
      ws.text({ type: "started", session: "s-1", pid: 42 });
      if (cmd[0] === "vanish") {
        ws.binary(1, "partial");
        return ws.close(1011);
      }
      if (cmd[0] === "query") {
        ws.binary(1, JSON.stringify(Object.fromEntries([...new Set(r.query.keys())].map((k) => [k, r.query.getAll(k)]))));
        ws.text({ type: "exit", code: 0 });
        return ws.close();
      }
      if (cmd[0] === "big") {
        // A large frame (64-bit length) and a fragmented one.
        ws.binary(1, Buffer.alloc(200_000, "x"));
        ws.frame(0x2, Buffer.from([2, 0x65]), false);
        ws.frame(0x0, Buffer.from("rr"), false);
        ws.frame(0x9, Buffer.from("ping"));
        ws.frame(0x0, Buffer.from("!"), true);
        ws.text({ type: "exit", code: 0 });
        return ws.close();
      }
      // cat: echo stdin to stdout until EOF, record control messages, exit 7.
      for (;;) {
        const m = await ws.next();
        if (m === null) return;
        if (m.binary) {
          if (m.data[0] === 4 && m.data.length === 1) {
            seen.eof = true;
            break;
          }
          seen.stdin.push(m.data.subarray(1));
          ws.binary(1, m.data.subarray(1));
          ws.binary(2, "e:" + m.data.subarray(1).toString());
        } else {
          const msg = JSON.parse(m.data.toString()) as ExecMessage;
          seen.text.push(msg);
          if (msg.type === "signal" && msg.signal === "KILL") {
            ws.text({ type: "exit", code: 137 });
            return ws.close();
          }
        }
      }
      ws.text({ type: "exit", code: 7 });
      ws.close();
    });
    fake.ws(`${sb}/exec/sessions/s-1`, async (r, ws) => {
      const kept = "hello, world";
      const offset = Number(r.query.get("offset") ?? 0);
      ws.text({ type: "attached", session: "s-1", offset });
      ws.binary(1, kept.slice(offset));
      ws.text({ type: "exit", code: 0 });
      ws.close();
    });
    fake.ws(`${sb}/exec/sessions/s-2`, async (_r, ws) => {
      ws.text({ type: "attached", session: "s-2", offset: 0 });
      // Hold the socket until the client goes.
      while ((await ws.next()) !== null);
    });
  });
  after(() => fake.close());

  test("stdin, stdout, stderr, EOF and the exit code", async () => {
    const out = collect();
    const err = collect();
    const p = await c.sandbox("scratch").exec({ cmd: ["cat"], stdin: "one two", stdout: out.sink, stderr: err.sink });
    assert.equal(p.session, "s-1");
    assert.equal(p.pid, 42);
    assert.equal(await p.wait(), 7);
    assert.equal(out.text(), "one two");
    assert.equal(err.text(), "e:one two");
    assert.ok(seen.eof);
  });

  test("a TTY: write, resize, signal", async () => {
    seen.text = [];
    const out = collect();
    const p = await c.sandbox("scratch").exec({ cmd: ["bash"], tty: true, cols: 80, rows: 24, stdout: out.sink });
    await p.write("ls\n");
    await p.resize(120, 40);
    await p.signal("INT");
    await p.signal("KILL");
    assert.equal(await p.wait(), 137);
    assert.equal(out.text(), "ls\n");
    assert.deepEqual(seen.text, [
      { type: "resize", cols: 120, rows: 40 },
      { type: "signal", signal: "INT" },
      { type: "signal", signal: "KILL" },
    ]);
  });

  test("stdin from an async iterable, stdout to a writable", async () => {
    const chunks: string[] = [];
    const writable = { write: (b: Buffer) => chunks.push(b.toString()) };
    async function* gen() {
      yield "a";
      yield Buffer.from("b");
    }
    const p = await c.sandbox("scratch").exec({ cmd: ["cat"], stdin: gen(), stdout: writable });
    assert.equal(await p.wait(), 7);
    assert.equal(chunks.join(""), "ab");
  });

  test("the query carries argv, tty, size, cwd, env, user and detachable", async () => {
    const out = collect();
    const p = await c.sandbox("scratch").exec({
      cmd: ["query", "a b", "--x=1"],
      tty: true,
      cols: 100,
      rows: 30,
      cwd: "/app",
      env: { B: "2", A: "1=1" },
      user: "root",
      detachable: true,
      stdout: out.sink,
    });
    assert.equal(await p.wait(), 0);
    assert.deepEqual(JSON.parse(out.text()), {
      cmd: ["query", "a b", "--x=1"],
      tty: ["true"],
      cols: ["100"],
      rows: ["30"],
      cwd: ["/app"],
      env: ["A=1=1", "B=2"],
      user: ["root"],
      detachable: ["true"],
    });
  });

  test("large and fragmented frames", async () => {
    const out = collect();
    const err = collect();
    const p = await c.sandbox("scratch").exec({ cmd: ["big"], stdout: out.sink, stderr: err.sink });
    assert.equal(await p.wait(), 0);
    assert.equal(out.bytes().length, 200_000);
    assert.equal(err.text(), "err!");
  });

  test("a refused upgrade is a LoamError with the API's code", async () => {
    const err = await c.sandbox("scratch").exec({ cmd: ["missing-sandbox"] }).catch((e: unknown) => e);
    assert.ok(err instanceof LoamError, String(err));
    assert.equal(err.status, 404);
    assert.equal(err.code, "not_found");
    assert.equal(err.message, "no sandbox scratch");
    const full = await c.sandbox("scratch").exec({ cmd: ["full"] }).catch((e: unknown) => e);
    assert.ok(isCode(full, "capacity"));
  });

  test("an error first frame is a LoamError", async () => {
    const err = await c.sandbox("scratch").exec({ cmd: ["nope"] }).catch((e: unknown) => e);
    assert.ok(isCode(err, "invalid"));
    assert.match((err as Error).message, /no such program/);
  });

  test("1011 without an exit frame is exit unknown, never success", async () => {
    const out = collect();
    const p = await c.sandbox("scratch").exec({ cmd: ["vanish"], stdout: out.sink });
    const err = await p.wait().catch((e: unknown) => e);
    assert.ok(err instanceof ExitUnknownError);
    assert.equal(err.closeCode, 1011);
    assert.match(err.message, /went away/);
    assert.equal(out.text(), "partial");
  });

  test("no command is refused before dialling", async () => {
    await assert.rejects(c.sandbox("scratch").exec({ cmd: [] }), /no command/);
  });

  test("attach replays from an offset", async () => {
    const out = collect();
    const p = await c.sandbox("scratch").attach("s-1", { offset: 7, stdout: out.sink });
    assert.equal(p.offset, 7);
    assert.equal(await p.wait(), 0);
    assert.equal(out.text(), "world");
    assert.equal(fake.last().query.get("offset"), "7");

    const all = collect();
    const q = await c.sandbox("scratch").attach("s-1", { offset: 0, stdout: all.sink });
    assert.equal(await q.wait(), 0);
    assert.equal(all.text(), "hello, world");
    assert.equal(fake.last().query.get("offset"), "0");
  });

  test("close leaves the session, and an abort drops the socket", async () => {
    const p = await c.sandbox("scratch").attach("s-2");
    await p.close();
    assert.ok((await p.wait().catch((e: unknown) => e)) instanceof ExitUnknownError);

    const ac = new AbortController();
    const q = await c.sandbox("scratch").attach("s-2", { signal: ac.signal });
    ac.abort();
    assert.ok((await q.wait().catch((e: unknown) => e)) instanceof ExitUnknownError);
  });
});

describe("watches, port connections and events", () => {
  const fake = new Fake();
  let c: LoamClient;
  before(async () => {
    await fake.listen();
    c = new LoamClient({ url: fake.url, token: TOKEN });
    fake.ws(`${sb}/fs/watch`, async (r, ws) => {
      ws.text({ type: "create", path: r.query.get("path") + "/a" });
      ws.text({ type: "write", path: r.query.get("path") + "/a" });
      ws.text({ type: "remove", path: r.query.get("recursive") ?? "flat" });
      ws.close(1000);
    });
    fake.ws(`${sb}/ports/5432/connect`, async (_r, ws) => {
      // An echo server, upper-cased.
      for (;;) {
        const m = await ws.next();
        if (m === null) return;
        ws.frame(0x2, Buffer.from(m.data.toString().toUpperCase()));
      }
    });
    fake.ws(`${sb}/ports/9/connect`, async () => {
      throw new RefuseUpgrade(502, "port_closed", "nothing listens on 9");
    });
    fake.on("GET", `${sb}/ports/watch`, (_r, res) => {
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      res.write(": keep-alive\n\n");
      res.write('event: opened\ndata: {"port":3000,"address":"0.0.0.0","pid":5}\n\n');
      res.write("event: ignored\ndata: {}\n\n");
      res.write('event: closed\r\ndata: {"port":3000,"address":"0.0.0.0",\r\ndata: "pid":5}\r\n\r\n');
      res.end();
    });
    fake.on("GET", "/v1/events", (r, res) => {
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      res.write(`event: sandbox.paused\ndata: {"type":"sandbox.paused","sandbox":"${r.query.get("sandbox")}","at":"t","detail":{"reason":"node_lost","snapshot_at":"s"}}\n\n`);
      res.write('event: sandbox.running\ndata: {"sandbox":"sb-2","at":"t"}\n\n');
      // Then it stays open, as the real stream does.
    });
    fake.on("GET", "/v1/sandboxes/x", (_r, res) => sendJSON(res, 200, {}));
  });
  after(() => fake.close());

  test("file watch", async () => {
    const w = await c.sandbox("scratch").watchFiles("/home/loam", { recursive: true });
    const got = [];
    for await (const ev of w) got.push(ev);
    assert.deepEqual(got, [
      { type: "create", path: "/home/loam/a" },
      { type: "write", path: "/home/loam/a" },
      { type: "remove", path: "true" },
    ]);
  });

  test("port connection is a duplex stream", async () => {
    const s = await c.sandbox("scratch").connectPort(5432);
    s.write("select 1;");
    const [chunk] = (await new Promise((r) => s.once("data", (d) => r([d])))) as Buffer[];
    assert.equal(chunk!.toString(), "SELECT 1;");
    s.end();
    await new Promise((r) => s.once("close", r));
  });

  test("nothing listening is port_closed, before the upgrade", async () => {
    const err = await c.sandbox("scratch").connectPort(9).catch((e: unknown) => e);
    assert.ok(isCode(err, "port_closed"));
    assert.equal((err as LoamError).status, 502);
  });

  test("ports watch", async () => {
    const w = await c.sandbox("scratch").watchPorts();
    const got = [];
    for await (const ev of w) got.push(ev);
    assert.deepEqual(got, [
      { type: "opened", port: { port: 3000, address: "0.0.0.0", pid: 5 } },
      { type: "closed", port: { port: 3000, address: "0.0.0.0", pid: 5 } },
    ]);
  });

  test("events, and closing an open stream", async () => {
    const ev = await c.events({ sandbox: "sb-1" });
    const first = await ev.next();
    assert.equal(first?.type, "sandbox.paused");
    assert.equal(first?.sandbox, "sb-1");
    assert.equal(first?.detail?.reason, "node_lost");
    const second = await ev.next();
    // No type in the data: the event's name fills it in.
    assert.equal(second?.type, "sandbox.running");
    const pending = ev.next();
    ev.close();
    assert.equal(await pending, null);
  });

  test("the fake's frames are what a standard WebSocket client reads", { skip: typeof globalThis.WebSocket !== "function" }, async () => {
    fake.auth = false;
    try {
      const url = fake.url.replace("http:", "ws:") + `${sb}/fs/watch?path=/w`;
      const ws = new WebSocket(url);
      const got: string[] = [];
      await new Promise<void>((resolve, reject) => {
        ws.onmessage = (e) => got.push(String(e.data));
        ws.onclose = () => resolve();
        ws.onerror = () => reject(new Error("websocket error"));
      });
      assert.equal(got.length, 3);
      assert.deepEqual(JSON.parse(got[0]!), { type: "create", path: "/w/a" });
    } finally {
      fake.auth = true;
    }
  });
});

describe("every stream sends the key", () => {
  test("exec with a bad key is unauthorized", async () => {
    const fake = await new Fake().listen();
    try {
      const c = new LoamClient({ url: fake.url, token: "wrong" });
      const err = await c.sandbox("scratch").exec({ cmd: ["cat"] }).catch((e: unknown) => e);
      assert.ok(isCode(err, "unauthorized"));
    } finally {
      await fake.close();
    }
  });
});
