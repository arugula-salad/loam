import assert from "node:assert/strict";
import { test } from "node:test";
import { Fake, TOKEN, apiError } from "./fake.js";

// dist/loam.mjs, the one-file build for vendoring, is the same client: same
// exports, and it works. (npm test builds it first.)
const dist = new URL("../../dist/", import.meta.url);

test("loam.mjs exports what index.js does, and works", async () => {
  const index = await import(new URL("index.js", dist).href);
  const one = await import(new URL("loam.mjs", dist).href);
  assert.deepEqual(Object.keys(one).sort(), Object.keys(index).sort());

  const fake = await new Fake().listen();
  fake.on("GET", "/v1/sandboxes/x", (_r, res) => apiError(res, 404, "not_found", "no sandbox x"));
  try {
    const c = new one.LoamClient({ url: fake.url, token: TOKEN });
    const err = await c.getSandbox("x").catch((e: unknown) => e);
    assert.ok(err instanceof one.LoamError);
    assert.ok(one.isNotFound(err));
  } finally {
    await fake.close();
  }
});
