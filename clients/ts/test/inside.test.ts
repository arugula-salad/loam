import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, before, describe, test } from "node:test";
import { LoamClient, type InsideSandbox } from "../src/index.js";
import { Fake, sandbox, sendJSON } from "./fake.js";

// Inside a sandbox the agent serves the sub-API on a Unix socket, without the
// /v1/sandboxes/{sandbox} prefix and without a key.
describe("inside a sandbox", () => {
  const dir = mkdtempSync(join(tmpdir(), "loam-ts-"));
  const sock = join(dir, "api.sock");
  const fake = new Fake();
  let in_: InsideSandbox;
  before(async () => {
    fake.auth = false;
    await fake.listen(sock);
    in_ = LoamClient.inside({ socketPath: sock });
    fake
      .on("GET", "/self", (r, res) => sendJSON(res, 200, sandbox({ labels: { auth: String(r.headers.authorization ?? "none") } })))
      .on("GET", "/identity/token", (r, res) => sendJSON(res, 200, { token: `jwt-for-${r.query.get("audience")}-${r.query.get("ttl_s")}`, expires_at: "t" }))
      .on("POST", "/checkpoints", (_r, res) => sendJSON(res, 201, { id: "v2", sandbox: "sb-1", comment: "", memory: true, auto: false, created_at: "" }))
      .on("PUT", "/fs/file", (r, res) => sendJSON(res, 200, { path: r.query.get("path"), name: "f", type: "file", size: r.body.length, mode: "0644", mtime: "" }))
      .on("GET", "/fs/file", (_r, res) => {
        res.writeHead(200);
        res.end("from the socket");
      });
    fake.ws("/exec", async (_r, ws) => {
      ws.text({ type: "started", session: "s-9", pid: 1 });
      ws.binary(1, "hi");
      ws.text({ type: "exit", code: 0 });
      ws.close();
    });
  });
  after(async () => {
    await fake.close();
    rmSync(dir, { recursive: true, force: true });
  });

  test("self, without a key", async () => {
    const s = await in_.self();
    assert.equal(s.name, "scratch");
    assert.equal(s.labels.auth, "none");
  });

  test("identity token", async () => {
    const t = await in_.identityToken("sts.amazonaws.com", { ttl_s: 900 });
    assert.equal(t.token, "jwt-for-sts.amazonaws.com-900");
  });

  test("checkpoint and files on the unprefixed paths", async () => {
    assert.equal((await in_.createCheckpoint()).id, "v2");
    assert.equal((await in_.writeFile("/tmp/f", new Blob(["abc"]).stream())).size, 3);
    assert.equal((await in_.readFile("/tmp/f")).toString(), "from the socket");
  });

  test("exec over the socket", async () => {
    const out: Buffer[] = [];
    const p = await in_.exec({ cmd: ["echo", "hi"], stdout: (b) => out.push(b) });
    assert.equal(await p.wait(), 0);
    assert.equal(Buffer.concat(out).toString(), "hi");
  });
});
