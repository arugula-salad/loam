import assert from "node:assert/strict";
import { after, before, describe, test } from "node:test";
import { Readable } from "node:stream";
import {
  Codes,
  LoamClient,
  LoamError,
  errorCode,
  isCode,
  isNotFound,
  portURL,
  portURLs,
  type Sandbox,
} from "../src/index.js";
import { Fake, TOKEN, apiError, sandbox, sendJSON } from "./fake.js";

async function text(s: ReadableStream<Uint8Array>): Promise<string> {
  return new Response(s).text();
}

describe("errors", () => {
  const fake = new Fake();
  let c: LoamClient;
  before(async () => {
    await fake.listen();
    c = new LoamClient({ url: fake.url + "/", token: TOKEN });
    fake
      .on("GET", "/v1/sandboxes/nope", (_r, res) => apiError(res, 404, "not_found", "no sandbox nope"))
      .on("POST", "/v1/sandboxes", (_r, res) => apiError(res, 503, "capacity", "full", { "Retry-After": "7" }))
      .on("GET", "/v1/templates", (_r, res) => {
        res.writeHead(502, { "Content-Type": "text/plain" });
        res.end("bad gateway\n");
      });
  });
  after(() => fake.close());

  test("the API's shape becomes a LoamError", async () => {
    const err = await c.getSandbox("nope").catch((e: unknown) => e);
    assert.ok(err instanceof LoamError);
    assert.equal(err.status, 404);
    assert.equal(err.code, "not_found");
    assert.equal(err.message, "no sandbox nope");
    assert.ok(err.isCode(Codes.NotFound));
    assert.ok(err.isCode("conflict", "not_found"));
    assert.ok(!err.isCode("conflict"));
    assert.ok(isNotFound(err));
    assert.ok(isCode(err, "not_found"));
    assert.equal(errorCode(err), "not_found");
    assert.equal(errorCode(new Error("x")), "");
  });

  test("capacity carries Retry-After and is not retried by default", async () => {
    const before = fake.requests.length;
    const err = await c.createSandbox({}).catch((e: unknown) => e);
    assert.ok(isCode(err, "capacity"));
    assert.equal(err.status, 503);
    assert.equal(err.retryAfter, 7);
    assert.equal(fake.requests.length, before + 1);
  });

  test("a body that is not the shape still becomes a LoamError", async () => {
    const err = await c.listTemplates().catch((e: unknown) => e);
    assert.ok(err instanceof LoamError);
    assert.equal(err.status, 502);
    assert.equal(err.code, "http_502");
    assert.equal(err.message, "bad gateway");
  });

  test("a bad key is unauthorized", async () => {
    const bad = new LoamClient({ url: fake.url, token: "wrong" });
    const err = await bad.whoami().catch((e: unknown) => e);
    assert.ok(isCode(err, Codes.Unauthorized));
  });

  test("no token is refused up front", () => {
    assert.throws(() => new LoamClient({ url: fake.url, token: "" }), /no token/);
    const saved = { url: process.env.LOAM_API_URL, token: process.env.LOAM_TOKEN };
    process.env.LOAM_API_URL = fake.url;
    delete process.env.LOAM_TOKEN;
    try {
      assert.throws(() => LoamClient.fromEnv(), /LOAM_TOKEN is not set/);
    } finally {
      restoreEnv(saved);
    }
  });

  test("no URL is refused up front: there is no default loam", () => {
    assert.throws(() => new LoamClient({ url: "", token: TOKEN }), /no url/);
    const saved = { url: process.env.LOAM_API_URL, token: process.env.LOAM_TOKEN };
    delete process.env.LOAM_API_URL;
    process.env.LOAM_TOKEN = TOKEN;
    try {
      assert.throws(() => LoamClient.fromEnv(), /LOAM_API_URL is not set/);
      process.env.LOAM_API_URL = fake.url + "/";
      assert.equal(LoamClient.fromEnv().url, fake.url);
    } finally {
      restoreEnv(saved);
    }
  });
});

describe("capacity retry (opt-in)", () => {
  const fake = new Fake();
  let calls = 0;
  before(async () => {
    await fake.listen();
    fake.on("POST", "/v1/sandboxes", (r, res) => {
      calls++;
      if (calls < 3) return apiError(res, 503, "capacity", "full", { "Retry-After": "0" });
      sendJSON(res, 201, sandbox({ name: JSON.parse(r.body.toString()).name }));
    });
    fake.on("GET", "/v1/sandboxes/busy", (_r, res) => apiError(res, 503, "sandbox_unavailable", "could not wake"));
  });
  after(() => fake.close());

  test("retries capacity after Retry-After, with the same body", async () => {
    const c = new LoamClient({ url: fake.url, token: TOKEN, retryCapacity: true });
    const s = await c.createSandbox({ name: "again" });
    assert.equal(s.name, "again");
    assert.equal(calls, 3);
    assert.deepEqual(
      fake.requests.filter((r) => r.path === "/v1/sandboxes").map((r) => r.body.toString()),
      Array(3).fill('{"name":"again"}'),
    );
  });

  test("gives up after its attempts", async () => {
    calls = -10;
    const c = new LoamClient({ url: fake.url, token: TOKEN, retryCapacity: { attempts: 2 } });
    const err = await c.createSandbox({ name: "x" }).catch((e: unknown) => e);
    assert.ok(isCode(err, "capacity"));
    assert.equal(calls, -8);
  });

  test("retries nothing else", async () => {
    const c = new LoamClient({ url: fake.url, token: TOKEN, retryCapacity: true });
    const n = fake.requests.length;
    const err = await c.getSandbox("busy").catch((e: unknown) => e);
    assert.ok(isCode(err, "sandbox_unavailable"));
    assert.equal(fake.requests.length, n + 1);
  });
});

describe("sandboxes and the rest of the record routes", () => {
  const fake = new Fake();
  let c: LoamClient;
  before(async () => {
    await fake.listen();
    c = new LoamClient({ url: fake.url, token: TOKEN });
    const echo = (status: number) => (r: { body: Buffer }, res: import("node:http").ServerResponse) =>
      sendJSON(res, status, sandbox(r.body.length ? JSON.parse(r.body.toString()) : {}));
    fake
      .on("POST", "/v1/sandboxes", echo(201))
      .on("GET", "/v1/sandboxes", (_r, res) => sendJSON(res, 200, { sandboxes: [sandbox()] }))
      .on("PATCH", "/v1/sandboxes/scratch", echo(200))
      .on("DELETE", "/v1/sandboxes/scratch", (_r, res) => {
        res.writeHead(204);
        res.end();
      })
      .on("POST", "/v1/sandboxes/scratch/pause", (_r, res) => sendJSON(res, 200, sandbox({ state: "paused" })))
      .on("POST", "/v1/sandboxes/scratch/stop", (_r, res) => sendJSON(res, 200, sandbox({ state: "stopped" })))
      .on("POST", "/v1/sandboxes/scratch/wake", (_r, res) => sendJSON(res, 200, sandbox()))
      .on("GET", "/v1/sandboxes/a%2Fb", (_r, res) => sendJSON(res, 200, sandbox({ name: "a/b" })))
      .on("GET", "/v1/templates", (_r, res) => sendJSON(res, 200, { templates: [{ name: "base", image: "ghcr.io/x@sha256:1", resources: { cpus: 2, memory_mib: 2048, disk_gib: 20 } }] }))
      .on("GET", "/v1/whoami", (_r, res) => sendJSON(res, 200, { project: { name: "jake", domains: [], created_at: "" }, key: { id: "k1", name: "ci", project: "jake", created_at: "" } }))
      .on("POST", "/v1/projects", (r, res) => sendJSON(res, 200, { name: JSON.parse(r.body.toString()).name, domains: [], created_at: "" }))
      .on("GET", "/v1/projects", (_r, res) => sendJSON(res, 200, { projects: [] }))
      .on("POST", "/v1/projects/jake/keys", (r, res) => sendJSON(res, 200, { id: "k2", name: JSON.parse(r.body.toString()).name, project: "jake", secret: "loam_x", created_at: "" }))
      .on("GET", "/v1/projects/jake/keys", (_r, res) => sendJSON(res, 200, { keys: [] }))
      .on("DELETE", "/v1/projects/jake/keys/k2", (_r, res) => {
        res.writeHead(204);
        res.end();
      })
      .on("DELETE", "/v1/projects/jake", (_r, res) => apiError(res, 409, "project_not_empty", "it has sandboxes"))
      .on("GET", "/v1/network/defaults", (_r, res) => sendJSON(res, 200, { egress: [{ domain: "github.com", action: "allow", ports: [80, 443] }] }))
      .on("PUT", "/v1/secrets/GITHUB_TOKEN", (r, res) => {
        const b = JSON.parse(r.body.toString());
        sendJSON(res, 200, { name: "GITHUB_TOKEN", hosts: b.hosts, placeholder: "__loam_github_token__", inject: b.inject ?? null, created_at: "", updated_at: "" });
      })
      .on("GET", "/v1/secrets", (_r, res) => sendJSON(res, 200, { secrets: [] }))
      .on("GET", "/v1/secrets/GITHUB_TOKEN", (_r, res) => sendJSON(res, 200, { name: "GITHUB_TOKEN", hosts: [], placeholder: "", inject: null, created_at: "", updated_at: "" }))
      .on("DELETE", "/v1/secrets/GITHUB_TOKEN", (_r, res) => apiError(res, 409, "conflict", "a sandbox names it"));
  });
  after(() => fake.close());

  test("create from a template, an image, and a fork", async () => {
    await c.createSandbox({ name: "scratch", labels: { owner: "arcade" }, idle: { timeout_s: 60 } });
    assert.deepEqual(JSON.parse(fake.last().body.toString()), { name: "scratch", labels: { owner: "arcade" }, idle: { timeout_s: 60 } });
    assert.equal(fake.last().headers["content-type"], "application/json");
    assert.equal(fake.last().headers.authorization, `Bearer ${TOKEN}`);

    const img = "ghcr.io/arugula-salad/game@sha256:" + "a".repeat(64);
    const s = await c.createSandbox({ image: img, network: { egress: [{ include: "defaults" }] }, secrets: ["GITHUB_TOKEN"] });
    assert.equal(s.image, img);
    assert.deepEqual(JSON.parse(fake.last().body.toString()), { image: img, network: { egress: [{ include: "defaults" }] }, secrets: ["GITHUB_TOKEN"] });

    await c.fork("game-template", "v3", { name: "remix-1", labels: { game: "1" } });
    assert.deepEqual(JSON.parse(fake.last().body.toString()), { name: "remix-1", labels: { game: "1" }, from: { sandbox: "game-template", checkpoint: "v3" } });
  });

  test("list filters by labels (sorted) and state", async () => {
    const l = await c.listSandboxes({ labels: { z: "1", a: "b=c" }, state: "running" });
    assert.equal(l.length, 1);
    assert.deepEqual(fake.last().query.getAll("label"), ["a=b=c", "z=1"]);
    assert.equal(fake.last().query.get("state"), "running");
  });

  test("update sends null to clear a deadline and leaves out the rest", async () => {
    await c.updateSandbox("scratch", { deadline: null, protected: true });
    assert.equal(fake.last().method, "PATCH");
    assert.equal(fake.last().body.toString(), '{"deadline":null,"protected":true}');
  });

  test("lifecycle and delete", async () => {
    assert.equal((await c.pause("scratch")).state, "paused");
    assert.equal((await c.stop("scratch")).state, "stopped");
    assert.equal((await c.wake("scratch")).state, "running");
    assert.equal(await c.deleteSandbox("scratch"), undefined);
  });

  test("names are escaped in paths", async () => {
    assert.equal((await c.getSandbox("a/b")).name, "a/b");
  });

  test("templates, whoami, projects and keys", async () => {
    assert.equal((await c.listTemplates())[0]!.name, "base");
    assert.equal((await c.whoami()).project.name, "jake");
    assert.equal((await c.createProject("arcade")).name, "arcade");
    assert.deepEqual(await c.listProjects(), []);
    assert.equal((await c.createKey("jake", "lobby")).secret, "loam_x");
    assert.deepEqual(await c.listKeys("jake"), []);
    await c.deleteKey("jake", "k2");
    assert.ok(isCode(await c.deleteProject("jake").catch((e: unknown) => e), "project_not_empty"));
  });

  test("network defaults and secrets", async () => {
    assert.equal((await c.networkDefaults())[0]!.domain, "github.com");
    const s = await c.putSecret("GITHUB_TOKEN", { value: "ghp_x", hosts: ["api.github.com"], inject: { header: "Authorization", format: "Bearer {value}" } });
    assert.equal(s.placeholder, "__loam_github_token__");
    assert.deepEqual(s.inject, { header: "Authorization", format: "Bearer {value}" });
    assert.deepEqual(await c.listSecrets(), []);
    assert.equal((await c.getSecret("GITHUB_TOKEN")).name, "GITHUB_TOKEN");
    assert.ok(isCode(await c.deleteSecret("GITHUB_TOKEN").catch((e: unknown) => e), "conflict"));
  });
});

describe("the sandbox sub-API's plain routes", () => {
  const fake = new Fake();
  let c: LoamClient;
  const files = new Map<string, Buffer>();
  before(async () => {
    await fake.listen();
    c = new LoamClient({ url: fake.url, token: TOKEN });
    const sb = "/v1/sandboxes/scratch";
    const stat = (path: string, size = 0) => ({ path, name: path.split("/").pop(), type: "file", size, mode: "0644", mtime: "2026-10-11T18:00:00Z" });
    fake
      .on("POST", `${sb}/exec/run`, (r, res) => {
        const b = JSON.parse(r.body.toString());
        const stdin = Buffer.from(b.stdin ?? "", "base64");
        sendJSON(res, 200, { exit_code: 3, stdout: Buffer.concat([Buffer.from("got "), stdin]).toString("base64"), stderr: Buffer.from([0xff, 0]).toString("base64"), timed_out: false });
      })
      .on("GET", `${sb}/exec/sessions`, (_r, res) => sendJSON(res, 200, { sessions: [{ id: "s-1", cmd: ["sh"], tty: true, pid: 9, started_at: "", exited: true, exit_code: 0, attached: false }] }))
      .on("POST", `${sb}/exec/sessions/s-1/signal`, (_r, res) => {
        res.writeHead(204);
        res.end();
      })
      .on("PUT", `${sb}/fs/file`, (r, res) => {
        files.set(r.query.get("path")!, r.body);
        sendJSON(res, 200, stat(r.query.get("path")!, r.body.length));
      })
      .on("GET", `${sb}/fs/file`, (r, res) => {
        const b = files.get(r.query.get("path")!);
        if (!b) return apiError(res, 404, "not_found", "no such file");
        if (r.query.get("user") === "nobody") return apiError(res, 403, "permission_denied", "nope");
        res.writeHead(200, { "Content-Type": "application/octet-stream" });
        res.end(b);
      })
      .on("DELETE", `${sb}/fs/file`, (_r, res) => {
        res.writeHead(204);
        res.end();
      })
      .on("GET", `${sb}/fs/stat`, (r, res) => sendJSON(res, 200, stat(r.query.get("path")!)))
      .on("GET", `${sb}/fs/list`, (r, res) => sendJSON(res, 200, { entries: [stat(r.query.get("path") + "/a")] }))
      .on("POST", `${sb}/fs/mkdir`, (r, res) => sendJSON(res, 200, { ...stat(JSON.parse(r.body.toString()).path), type: "dir" }))
      .on("POST", `${sb}/fs/rename`, (r, res) => sendJSON(res, 200, stat(JSON.parse(r.body.toString()).to)))
      .on("PUT", `${sb}/fs/tar`, (r, res) => {
        files.set("tar:" + r.query.get("path"), r.body);
        res.writeHead(204);
        res.end();
      })
      .on("GET", `${sb}/fs/tar`, (r, res) => {
        res.writeHead(200, { "Content-Type": "application/x-tar" });
        res.end(files.get("tar:" + r.query.get("path")));
      })
      .on("GET", `${sb}/ports`, (_r, res) => sendJSON(res, 200, { ports: [{ port: 8080, address: "0.0.0.0", pid: 7 }] }))
      .on("PUT", `${sb}/ports/3000`, (r, res) =>
        sendJSON(res, 200, { port: 3000, access: JSON.parse(r.body.toString()).access, url: "https://3000--scratch--jake.loam.example.com" }),
      )
      .on("GET", `${sb}/stats`, (_r, res) => sendJSON(res, 200, {
        sandbox: "sb-1", name: "scratch", days: [{ day: "2026-10-13", requests: 3, status_2xx: 3, status_3xx: 0, status_4xx: 0, status_5xx: 0, websockets: 0, bytes_sent: 30, key_requests: 0, visitors: 2 }],
        total: { requests: 3, status_2xx: 3, status_3xx: 0, status_4xx: 0, status_5xx: 0, websockets: 0, bytes_sent: 30, key_requests: 0 }, visitor_days: 2,
      }))
      .on("GET", `${sb}/ports/settings`, (_r, res) => sendJSON(res, 200, { ports: [{ port: 3000, access: "public", url: "https://3000--scratch--jake.loam.example.com" }] }))
      .on("PUT", `${sb}/services/web`, (r, res) => sendJSON(res, 200, { name: "web", ...JSON.parse(r.body.toString()), state: "starting", restarts: 0 }))
      .on("GET", `${sb}/services`, (_r, res) => sendJSON(res, 200, { services: [] }))
      .on("GET", `${sb}/services/web`, (_r, res) => sendJSON(res, 200, { name: "web", cmd: ["npm", "start"], state: "running", pid: 4, restarts: 1 }))
      .on("POST", `${sb}/services/web/restart`, (_r, res) => sendJSON(res, 200, { name: "web", cmd: ["npm", "start"], state: "starting", restarts: 2 }))
      .on("POST", `${sb}/services/web/start`, (_r, res) => sendJSON(res, 200, { name: "web", cmd: [], state: "running", restarts: 0 }))
      .on("POST", `${sb}/services/web/stop`, (_r, res) => sendJSON(res, 200, { name: "web", cmd: [], state: "stopped", restarts: 0 }))
      .on("DELETE", `${sb}/services/web`, (_r, res) => {
        res.writeHead(204);
        res.end();
      })
      .on("GET", `${sb}/services/web/logs`, (_r, res) => {
        res.writeHead(200, { "Content-Type": "text/plain" });
        res.end("listening on 8080\n");
      })
      .on("PUT", `${sb}/tasks/build`, (r, res) => sendJSON(res, 200, { name: "build", started_at: "a", expires_at: String(JSON.parse(r.body.toString()).expire_s) }))
      .on("GET", `${sb}/tasks`, (_r, res) => sendJSON(res, 200, { tasks: [] }))
      .on("GET", `${sb}/tasks/build`, (_r, res) => sendJSON(res, 200, { name: "build", started_at: "a", expires_at: "b" }))
      .on("DELETE", `${sb}/tasks/build`, (_r, res) => {
        res.writeHead(204);
        res.end();
      })
      .on("POST", `${sb}/checkpoints`, (r, res) => sendJSON(res, 201, { id: "v1", sandbox: "sb-1", comment: JSON.parse(r.body.toString()).comment ?? "", memory: JSON.parse(r.body.toString()).memory ?? true, auto: false, created_at: "" }))
      .on("GET", `${sb}/checkpoints`, (_r, res) => sendJSON(res, 200, { checkpoints: [] }))
      .on("GET", `${sb}/checkpoints/v1`, (_r, res) => sendJSON(res, 200, { id: "v1", sandbox: "sb-1", comment: "", memory: true, auto: false, created_at: "" }))
      .on("DELETE", `${sb}/checkpoints/v1`, (_r, res) => {
        res.writeHead(204);
        res.end();
      })
      .on("POST", `${sb}/checkpoints/v1/restore`, (_r, res) => sendJSON(res, 200, sandbox()));
  });
  after(() => fake.close());

  test("run sends stdin as base64 and decodes the output", async () => {
    const r = await c.sandbox("scratch").run({ cmd: ["cat"], stdin: "hello", env: { A: "1" }, timeout_s: 5 });
    assert.equal(r.exit_code, 3);
    assert.equal(r.stdout.toString(), "got hello");
    assert.deepEqual([...r.stderr], [0xff, 0]);
    assert.equal(r.timed_out, false);
    assert.equal(r.truncated, false);
    assert.deepEqual(JSON.parse(fake.last().body.toString()), { cmd: ["cat"], stdin: "aGVsbG8=", env: { A: "1" }, timeout_s: 5 });
  });

  test("sessions and signalling one", async () => {
    const sb = c.sandbox("scratch");
    assert.equal((await sb.sessions())[0]!.exit_code, 0);
    await sb.signalSession("s-1", "TERM");
    assert.equal(fake.last().body.toString(), '{"signal":"TERM"}');
  });

  test("files: write, read, stat, list, mkdir, rename, remove, as a user", async () => {
    const sb = c.sandbox("scratch");
    const bytes = Buffer.from([0, 1, 2, 255, 254]);
    const st = await sb.writeFile("/home/loam/a.bin", bytes, { mode: 0o600, mkdir: true });
    assert.equal(st.size, 5);
    assert.equal(fake.last().query.get("mode"), "0600");
    assert.equal(fake.last().query.get("mkdir"), "true");
    assert.equal(fake.last().headers["content-type"], "application/octet-stream");
    assert.deepEqual(await sb.readFile("/home/loam/a.bin"), bytes);
    assert.equal(await text(await sb.openFile("/home/loam/a.bin")), bytes.toString());
    assert.ok(isCode(await sb.readFile("/home/loam/a.bin", { user: "nobody" }).catch((e: unknown) => e), "permission_denied"));
    assert.ok(isNotFound(await sb.readFile("/nope").catch((e: unknown) => e)));

    await sb.writeFile("/s.txt", "text", { mode: "0644" });
    assert.equal(fake.last().query.get("mode"), "0644");
    assert.equal(files.get("/s.txt")!.toString(), "text");

    assert.equal((await sb.stat("/x")).path, "/x");
    assert.equal((await sb.list("/d"))[0]!.path, "/d/a");
    assert.equal((await sb.mkdir("/d/e", { parents: true, mode: 0o755, user: "loam" })).type, "dir");
    assert.equal(fake.last().query.get("user"), "loam");
    assert.deepEqual(JSON.parse(fake.last().body.toString()), { path: "/d/e", parents: true, mode: "0755" });
    assert.equal((await sb.rename("/a", "/b")).path, "/b");
    await sb.remove("/d", { recursive: true });
    assert.equal(fake.last().query.get("recursive"), "true");
  });

  test("files: tar out and in, streamed", async () => {
    const sb = c.sandbox("scratch");
    const tar = Buffer.alloc(3 * 512, 7);
    // A Node stream, a web stream and plain bytes all go up.
    await sb.putTar("/home/loam/app", Readable.from([tar.subarray(0, 512), tar.subarray(512)]));
    assert.equal(fake.last().headers["content-type"], "application/x-tar");
    assert.deepEqual(files.get("tar:/home/loam/app"), tar);
    await sb.putTar("/web", new Blob([tar]).stream());
    assert.deepEqual(files.get("tar:/web"), tar);
    const got = Buffer.from(await new Response(await sb.getTar("/home/loam/app")).arrayBuffer());
    assert.deepEqual(got, tar);
  });

  test("ports, port access and URLs", async () => {
    const sb = c.sandbox("scratch");
    assert.equal((await sb.ports())[0]!.port, 8080);
    const s = await sb.setPortAccess(3000, "public");
    assert.equal(s.access, "public");
    assert.equal((await sb.portSettings())[0]!.port, 3000);
    const st = await sb.stats({ days: 1 });
    assert.equal(st.days[0]!.visitors, 2);
    assert.equal(fake.last().path, "/v1/sandboxes/scratch/stats");
    assert.equal(fake.last().query.get("days"), "1");

    const rec = sandbox() as unknown as Sandbox;
    assert.equal(portURL(rec, 3000), "https://3000--scratch--jake.loam.example.com");
    assert.equal(portURL("https://scratch.apps.example.com", 5173), "https://5173--scratch.apps.example.com");
    assert.deepEqual(portURLs(rec, 3000), ["https://3000--scratch--jake.loam.example.com", "https://3000--scratch.apps.example.com"]);
  });

  test("services", async () => {
    const sb = c.sandbox("scratch");
    const s = await sb.putService("web", { cmd: ["npm", "start"], cwd: "/app", http_port: 8080, needs: ["db"] });
    assert.equal(s.state, "starting");
    assert.deepEqual(JSON.parse(fake.last().body.toString()), { cmd: ["npm", "start"], cwd: "/app", http_port: 8080, needs: ["db"] });
    assert.deepEqual(await sb.listServices(), []);
    assert.equal((await sb.getService("web")).pid, 4);
    assert.equal((await sb.restartService("web")).restarts, 2);
    assert.equal((await sb.startService("web")).state, "running");
    assert.equal((await sb.stopService("web")).state, "stopped");
    assert.equal(await text(await sb.serviceLogs("web", { tail: 10 })), "listening on 8080\n");
    assert.equal(fake.last().query.get("tail"), "10");
    assert.equal(fake.last().query.get("follow"), null);
    await sb.deleteService("web");
  });

  test("tasks", async () => {
    const sb = c.sandbox("scratch");
    assert.equal((await sb.putTask("build", { expire_s: 600 })).expires_at, "600");
    assert.deepEqual(await sb.listTasks(), []);
    assert.equal((await sb.getTask("build")).name, "build");
    await sb.deleteTask("build");
  });

  test("checkpoints", async () => {
    const sb = c.sandbox("scratch");
    const cp = await sb.createCheckpoint({ comment: "before upgrade", memory: false });
    assert.equal(cp.comment, "before upgrade");
    assert.equal(cp.memory, false);
    assert.deepEqual(await sb.listCheckpoints(), []);
    assert.equal((await sb.getCheckpoint("v1")).id, "v1");
    assert.equal((await sb.restoreCheckpoint("v1")).state, "running");
    await sb.deleteCheckpoint("v1");
  });
});

function restoreEnv(saved: { url?: string; token?: string }): void {
  if (saved.url === undefined) delete process.env.LOAM_API_URL;
  else process.env.LOAM_API_URL = saved.url;
  if (saved.token === undefined) delete process.env.LOAM_TOKEN;
  else process.env.LOAM_TOKEN = saved.token;
}
