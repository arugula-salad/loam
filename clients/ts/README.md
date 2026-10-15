# @arugula-salad/loam

The TypeScript client for loam's public API. `docs/API.md` is the contract; this client follows
it, as the Go client (`pkg/client`) does, and its types mirror `pkg/api` field for field, in the
wire's snake_case.

It needs Node 20 or later and has no dependencies. It is Node-only: an API key does not belong
in a browser.

## Install

Each `ts-v*` tag publishes the package to npm (`@arugula-salad/loam`), and each tag's
GitHub release carries `loam.mjs`, the whole client in one ES module file. There are two ways to
use it.

**Vendor `loam.mjs`** (the simplest, and the right choice for a plain `.mjs` service with no
`package.json` and no `npm install`). Put the file next to your code and commit it. Nothing
needs a token at build time:

```sh
gh release download ts-v0.1.0 --repo arugula-salad/loam --pattern loam.mjs --dir lobby/ --clobber
```

```js
import { LoamClient } from './loam.mjs'
```

To update, download a newer tag's file. Its first line names its version.

**Install the package** from npm.

```sh
npm install @arugula-salad/loam
```

## Use

```ts
import { LoamClient, isCode } from '@arugula-salad/loam'

const loam = new LoamClient({ url: 'https://api.loam.example.com', token: process.env.LOAM_TOKEN! })
// or: LoamClient.fromEnv(), from LOAM_API_URL and LOAM_TOKEN
```

The client has the routes that aren't under one sandbox: projects and keys (with the admin
token), `whoami`, sandboxes, templates, secrets, network defaults and events.
`loam.sandbox(idOrName)` gives the rest, the sandbox's sub-API: exec, files, ports, services,
tasks and checkpoints. Every method takes an optional last argument with an `AbortSignal`
(`{ signal }`).

### Create a sandbox

Create answers once the sandbox is running.

```ts
// From a template (default "base").
const sb = await loam.createSandbox({
  name: 'scratch',
  template: 'base',
  labels: { app: 'arcade-games' },
  idle: { timeout_s: 300, action: 'pause' },
  env: { NODE_ENV: 'production' },
})

// From an OCI image, by digest (never a tag), from a registry loam-api allows.
await loam.createSandbox({ image: 'ghcr.io/arugula-salad/game@sha256:…', http_port: 5173 })

// A fork: a new sandbox from a checkpoint of another, its disk and (if the checkpoint has it) memory.
await loam.fork('game-template', 'v3', { name: 'remix-42', labels: { app: 'arcade-games' } })

// Restricted egress, and a secret the sandbox uses without holding it.
await loam.createSandbox({
  network: { egress: [{ include: 'defaults' }, { domain: 'api.anthropic.com', action: 'allow' }] },
  secrets: ['ANTHROPIC_API_KEY'],
})

await loam.listSandboxes({ labels: { app: 'arcade-games' }, state: 'running' })
await loam.updateSandbox('scratch', { deadline: null, protected: true }) // null clears the deadline
await loam.pause('scratch'); await loam.wake('scratch'); await loam.stop('scratch')
await loam.deleteSandbox('scratch')
```

### Run a command

```ts
const box = loam.sandbox('scratch')
const r = await box.run({ cmd: ['sh', '-c', 'wc -c'], stdin: 'hello', timeout_s: 30 })
r.exit_code          // 0
r.stdout.toString()  // "5\n" (stdout and stderr are Buffers, decoded from the API's base64)
```

### Exec, streaming

`exec` resolves once the session has started; output goes to the sinks (a function, or anything
with `write()`, such as `process.stdout`) until `wait()` settles.

```ts
const p = await box.exec({
  cmd: ['bash'],
  tty: true, cols: 80, rows: 24,
  cwd: '/home/loam', env: { TERM: 'xterm-256color' }, user: 'loam',
  stdout: process.stdout,       // with a TTY, all output is stdout
  detachable: true,             // outlives this socket: attach to it again later
})
await p.write('ls -la\n')
await p.resize(120, 40)
await p.signal('INT')           // to the process group; names as in `kill -l`
await p.closeStdin()            // EOF
const code = await p.wait()     // 128+signal when killed
```

`stdin` may also be given up front (a string, bytes, a Node or web stream, any async iterable):
it is copied in and then closed. Without a TTY, stderr goes to `stderr`:

```ts
const q = await box.exec({ cmd: ['tar', 'x', '-C', '/srv'], stdin: fs.createReadStream('site.tar'), stderr: (b) => log(b.toString()) })
await q.wait()
```

`wait()` rejects with `ExitUnknownError` when the stream ended without an exit frame: the sandbox
went away under it (deleted, restored, its node lost; close status 1011) or the connection broke.
That is never success. It rejects with a `LoamError` when the server sent an error frame.

Sessions:

```ts
await box.sessions()                            // running and recently exited
const again = await box.attach(p.session, { offset: 0, stdout: process.stdout }) // replay, then live
await box.signalSession(p.session, 'TERM')
await again.close()  // a detachable session goes on; any other is hung up
```

### Files

Paths are absolute in the sandbox. Operations run as root unless `user` is given (then a refusal
is `permission_denied`).

```ts
await box.writeFile('/home/loam/app/index.html', '<h1>hi</h1>', { mode: 0o644, mkdir: true, user: 'loam' })
const buf = await box.readFile('/etc/os-release')        // Buffer
const stream = await box.openFile('/var/log/big.log')     // web ReadableStream
await box.stat('/home/loam'); await box.list('/home/loam')
await box.mkdir('/srv/a/b', { parents: true }); await box.rename('/srv/a', '/srv/c')
await box.remove('/srv/c', { recursive: true })

// A directory as a tar, out and in; entry names are relative to the path.
const tar = await box.getTar('/home/loam/app')
await loam.sandbox('remix-42').putTar('/home/loam/app', tar)
await box.putTar('/srv', fs.createReadStream('site.tar'))

for await (const ev of await box.watchFiles('/home/loam/app', { recursive: true })) {
  console.log(ev.type, ev.path)  // create, write, remove, rename, chmod
}
```

### Ports and URLs

Every port has a URL, private unless set public. A private URL needs the project's key
(`Authorization: Bearer`, or a `loam_token` cookie set by `?loam_token=<key>` on a first visit).
A request to a sleeping sandbox wakes it.

```ts
import { portURL, portURLs } from '@arugula-salad/loam'

sb.url                    // https://scratch--jake.loam.example.com  -> http_port
sb.urls                   // ["https://scratch.games.example.com"], one per project URL domain
portURL(sb, 3000)         // https://3000--scratch--jake.loam.example.com
portURLs(sb, 3000)        // the same under each domain: https://3000--scratch.games.example.com, …

const { url } = await box.setPortAccess(3000, 'public')  // or 'private'
await box.portSettings()  // only the ports whose access was set
await box.ports()         // what listens, on every address

for await (const ev of await box.watchPorts()) console.log(ev.type, ev.port.port) // opened / closed

// A raw TCP stream to 127.0.0.1:5432 in the sandbox, as a Node Duplex.
const conn = await box.connectPort(5432)  // nothing listening: LoamError port_closed
```

### Services

Supervised processes that start at every boot, restart on a crash, and survive a pause and a stop.

```ts
await box.putService('web', { cmd: ['npm', 'run', 'dev'], cwd: '/home/loam/app', http_port: 5173 })
await box.getService('web')       // { state: 'running', pid, restarts, … }
await box.restartService('web')   // also startService, stopService, deleteService, listServices
const logs = await box.serviceLogs('web', { tail: 100 })      // web ReadableStream of text
console.log(await new Response(logs).text())
```

### Tasks

A task holds the sandbox awake while it is live, and expires (at most an hour) so a crashed
client can't hold it forever. Put it again to hold longer.

```ts
await box.putTask('build', { expire_s: 600 })
await box.deleteTask('build')
```

### Checkpoints

```ts
const cp = await box.createCheckpoint({ comment: 'before upgrade' })   // memory: true by default
await box.listCheckpoints()                                           // oldest first
await box.restoreCheckpoint(cp.id)    // keeps the current state as auto-N first; ends every stream
await loam.fork('scratch', cp.id, { name: 'scratch-2' })
await box.deleteCheckpoint(cp.id)
```

### Events

Server-Sent Events for the key's project. Best effort: the API is the state, and a client that
reconnects reads it.

```ts
const events = await loam.events({ sandbox: 'scratch' })  // or every sandbox
for await (const ev of events) {
  // sandbox.created / running / paused / stopped / deleted / error, checkpoint.created / restored / deleted
  if (ev.detail?.reason === 'node_lost') console.log(ev.sandbox, 'woke from the snapshot at', ev.detail.snapshot_at)
}
events.close()
```

### Secrets and the network

```ts
await loam.putSecret('GITHUB_TOKEN', { value: 'ghp_…', hosts: ['api.github.com', 'github.com'] })
await loam.putSecret('ANTHROPIC_API_KEY', {
  value: 'sk-ant-…', hosts: ['api.anthropic.com'],
  inject: { header: 'x-api-key', format: '{value}' },
})
await loam.listSecrets()                 // never the values
await loam.updateSandbox('scratch', { secrets: ['GITHUB_TOKEN'] })
await loam.networkDefaults()            // what { include: 'defaults' } stands for
```

### Inside a sandbox

Processes in a sandbox reach the same sub-API on `/run/loam/api.sock`, with no key: being
inside is the credential.

```ts
const me = LoamClient.inside()                 // socketPath defaults to /run/loam/api.sock
await me.self()                                // this sandbox's record
await me.identityToken('sts.amazonaws.com')   // an OIDC token proving which sandbox this is
await me.createCheckpoint()                    // answers before the pause
await me.run({ cmd: ['uptime'] })              // and the rest of the sub-API, exec and files included
```

## Errors

Every error answer becomes a `LoamError` with `status`, `code` (the API's snake_case code) and
`message`. A body that isn't the API's shape (a proxy's error page) still becomes one, with the
code `http_<status>`. A refused WebSocket upgrade is the same.

```ts
import { LoamError, Codes, isCode, isNotFound } from '@arugula-salad/loam'

try {
  await loam.getSandbox('nope')
} catch (err) {
  if (isNotFound(err)) { /* also another project's sandbox: isolation looks like 404 */ }
  if (err instanceof LoamError && err.isCode(Codes.Capacity, Codes.SandboxUnavailable)) { /* … */ }
}
```

**Retries.** None, except the one the contract documents, and only when asked: a `capacity`
503 (no node can take the sandbox) carries `Retry-After`, and with `retryCapacity` the client
waits that long and tries again.

```ts
const loam = new LoamClient({ token, retryCapacity: { attempts: 5, maxWaitS: 120 } }) // or `true`
```

A request whose body is a stream is never retried, since the stream can't be read twice.

## Why not Node's WebSocket

The streams (exec, attach, file watch, port connect) use a small WebSocket client of the
package's own (`src/ws.ts`) over `node:http`, not Node's global `WebSocket`: a refused upgrade
answers with the API's error shape, and the standard `WebSocket` can't show it (it only says
"error" and closes with 1006), so the code would be lost; the in-sandbox API is on a Unix socket,
which the standard `WebSocket` can't dial; and Node 20 has no global `WebSocket`. The `ws`
package would also do; this is the part of it loam needs.

## Develop

```sh
cd clients/ts
npm ci
npm test        # builds dist/ (and dist/loam.mjs), then runs the tests against a fake API
npm run build
```

The tests need no network: `test/fake.ts` is a fake loam-api with a WebSocket server of its own.

Publishing: bump `version` in `package.json` in a PR, merge, then tag main with `ts-v<version>`;
`.github/workflows/client-ts.yml` publishes the package and makes the release with `loam.mjs`.
