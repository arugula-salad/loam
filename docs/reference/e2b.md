# E2B

loam speaks E2B's API as well as its own, so code written for E2B's official SDKs (Python
`e2b` and JavaScript `e2b`, unmodified) creates, uses, pauses, resumes and kills sandboxes on
loam. It's checked against both SDKs at 2.52.0.

## Setup

Two environment variables, with your loam project key as the E2B key:

```sh
export E2B_DOMAIN=e2b.loam.example.com
export E2B_API_KEY=$LOAM_TOKEN     # loam_...
```

| | |
|---|---|
| Control plane | `https://api.e2b.loam.example.com` |
| A sandbox's port P | `https://<P>-<sandbox id>.e2b.loam.example.com` |
| Key | `X-API-Key: loam_...` (the SDKs send it) |

```js
import { Sandbox } from 'e2b'

const sbx = await Sandbox.create({ timeoutMs: 5 * 60_000, metadata: { owner: 'docs' } })
console.log('sandbox', sbx.sandboxId)

const r = await sbx.commands.run('echo hello from $(hostname); id -un')
console.log(r.exitCode, r.stdout.trim())

await sbx.files.write('/home/user/hello.txt', 'hi\n')
console.log('read:', (await sbx.files.read('/home/user/hello.txt')).trim())

await sbx.commands.run('python3 -m http.server 3000', { background: true })
await new Promise((r) => setTimeout(r, 1500))
const host = sbx.getHost(3000)
console.log('host', host, (await fetch(`https://${host}/`)).status)

await sbx.pause()
const again = await Sandbox.connect(sbx.sandboxId)
console.log('after resume:', (await again.commands.run('cat /home/user/hello.txt')).stdout.trim())

try {
  await Sandbox.create('some-other-template')
} catch (e) {
  console.log('refused:', e.constructor.name, e.message)
}

await again.kill()
```

```console
$ node e2b.mjs
sandbox sb-nlucxvii5em3
0 hello from rustic-meadow-7517
user
read: hi
host 3000-sb-nlucxvii5em3.e2b.loam.example.com 200
after resume: hi
refused: SandboxError 404: template 'some-other-template' not found
```

Python is the same: `Sandbox.create()`, `sbx.commands.run(...)`, `sbx.files.write(...)`, and so on,
with the same two variables.

## An E2B sandbox is a loam sandbox

The adapter makes ordinary loam sandboxes in your key's project, through loam's own rules, so
`loam ls` and the native API see them and can act on them:

| E2B | loam |
|---|---|
| sandbox ID | the loam sandbox ID, `sb-…` |
| template `base` (the only one) | the loam template `e2b` |
| `metadata` k: v | the label `e2b/k`: v |
| `envVars` | the sandbox's `env` |
| `timeout` | the sandbox's `deadline`, action `delete` (with `autoPause`, `pause`); `idle` is `none`, so an E2B sandbox runs until its timeout |
| `pause()` / `connect()` | loam's pause (or stop, with `autoPauseMemory: false`) and wake |
| `allow_internet_access`, `network.allowOut` / `denyOut` | the sandbox's [egress rules](network.md) |

A sandbox the adapter didn't make isn't an E2B sandbox: E2B's API answers 404 for it and doesn't
list it.

## What works

Create (`Sandbox.create`, with `timeout`, `metadata`, `envVars`, `autoPause`, `autoPauseMemory`,
`autoResume`, `secure`, `allow_internet_access`, `network`), `connect` and resume, `pause`,
`kill`, `setTimeout`, `getInfo`, `list` (with metadata and state filters, paged), `getMetrics` (one
current sample), and everything the SDK does in the sandbox through envd: commands (streaming,
background, stdin, PTY, signals), files (read, write, list, watch, signed upload and download
URLs), and `getHost(port)` for a sandbox's ports. Processes survive a pause and are listed after
the resume.

## What's refused

Answered `404 Not found: this E2B-compatible server does not implement …`: templates and their
build API (`/v3/templates`, `/templates/…`), snapshots and `fork`, volumes, sandbox logs and
events, `GET /sandboxes/metrics`, teams and API key management.

Refused at create with a `400` that names the field: any template but `base`, `mcp`, `iam`,
volume mounts, egress proxies and transform rules (`network.rules`, `network.egressProxy`), IP
addresses and CIDRs in `allowOut`/`denyOut` (except `0.0.0.0/0`, "all traffic"),
`secure: false`, and a `denyOut` name that an `allowOut` wildcard
covers.

```console
$ curl -sS -H "X-API-Key: $LOAM_TOKEN" https://api.e2b.loam.example.com/v3/templates
{"code":404,"message":"Not found: this E2B-compatible server does not implement GET /v3/templates"}
$ curl -sS -H "X-API-Key: $LOAM_TOKEN" -H 'Content-Type: application/json' \
    https://api.e2b.loam.example.com/v2/sandboxes -d '{"templateID":"base","mcp":{}}'
{"code":400,"message":"mcp: not supported by this server"}
$ curl -sS -H "X-API-Key: nope" https://api.e2b.loam.example.com/v2/sandboxes
{"code":401,"message":"Invalid API key: use a loam project key (loam_...) as E2B_API_KEY"}
```

loam's own refusals come through in E2B's terms: no node with room is a `429` with `Retry-After`
(the SDKs retry it), something loam finds invalid (a label, an env name) a `400` with loam's
message, a sandbox that can't be woken a `503`.

## Egress

E2B's options become loam [egress rules](network.md), which are names, decided by the most
specific:

- `allow_internet_access: false`, or `0.0.0.0/0` in `denyOut`, denies everything; then each
  `allowOut` name is allowed (on ports 80 and 443) and each `denyOut` name denied.
- Without that, `allowOut` limits nothing, and a name in `denyOut` is refused: loam can't allow
  everything except a name.
- Addresses and CIDRs (other than `0.0.0.0/0`) are refused.

`Sandbox.updateNetwork` (`PUT /sandboxes/{id}/network`) replaces the rules and applies them at
once. The SDK's own traffic to envd and to your ports comes in through loam, not the network, so
it works whatever the rules.

## Differences from hosted E2B

- **Ports are public by default.** `https://<port>-<id>.e2b.loam.example.com` needs no credential, as on
  hosted E2B. The same port through loam's own URL (`https://<port>--<name>--<project>.loam.example.com`)
  keeps loam's access setting, private by default.
- **A server on `127.0.0.1` is reachable**; hosted E2B needs `0.0.0.0`.
- `templateID` and `alias` are both `base`. `envdVersion` is `0.9.0`.
- Resources are the `e2b` template's: 2 CPUs, 1024 MB, 20 GB of disk.
- Metrics are one sample read when asked, `[]` for a sandbox asleep in loam; there is no history.
- The hostname is the loam sandbox's name (`rustic-meadow-7517`), not `e2b`.
- A timeout is a loam deadline, acted on within a second of `endAt`; a sandbox killed by its
  timeout is deleted, disk and all. A paused sandbox has no timeout and is kept until killed.
- A pause is loam's: the memory snapshot can wake on any node, with its processes.
- No `X-RateLimit-*` headers; nothing is rate limited but capacity.

The commands run as `user` (uid 1000, home `/home/user`, passwordless `sudo`), as on E2B. Using
the same sandbox through loam's own API, exec runs as `loam` (uid 1001) unless you give `user`.

## Identity after a snapshot

A running process can read its current sandbox ID from `/run/e2b/.E2B_SANDBOX_ID`.
The agent refreshes this file at every create, resume, restore and fork, before the
VM is reported running. Reopen the file when reading it, or watch `/run/e2b` for
replacement. A restored process's environment still contains its original values.

## Private ports

Create with `network: { allowPublicTraffic: false }` in JavaScript (Python:
`network={"allow_public_traffic": False}`). The SDK exposes `trafficAccessToken`
(`traffic_access_token` in Python). Send it on requests to user ports:

```sh
curl -H "e2b-traffic-access-token: $TRAFFIC_TOKEN" https://8000-sb-example.e2b.loam.example.com
```

A missing or incorrect token gets 403 without waking the sandbox. envd still uses its
own access token. Public access is fixed at create; network updates can repeat the same
setting but cannot change it. The traffic token survives pause/resume and changes if the
operator rotates `LOAM_SECRET_KEY`.

## Build a template

Use the official E2B Template SDK with a digest-pinned base image or an existing
template. Dockerfiles support RUN, ENV, USER, WORKDIR, and COPY through the SDK's
parser. The base registry must be allowed by the operator; per-build registry
credentials and disk-growth requests are not supported.

```js
const template = Template({fileContextPath: '.'})
  .fromTemplate('base')
  .copy('server.js', '/home/user/server.js')
  .setUser('user')
  .setWorkdir('/home/user')
  .setStartCmd('node server.js', 'curl -f http://localhost:3000/health')
await Template.build(template, 'my-server', {tags: ['v1'], cpuCount: 2, memoryMB: 1024})
const sandbox = await Sandbox.create('my-server:v1')
```

The build snapshots the running server after its ready check. Each create resumes
its own copy. Tags change only when a build succeeds, so a failed rebuild leaves
the previous version available. Templates are private to the API key's project.
Builds have a 20-minute limit and readiness checks a two-minute limit.

## Keep inference keys outside the sandbox

```js
import {Secret, Sandbox} from 'e2b'
await Secret.create('inference-key', process.env.INFERENCE_KEY)
const sandbox = await Sandbox.create({
  network: {
    denyOut: ['0.0.0.0/0'],
    allowOut: ['api.example.com'],
    rules: {'api.example.com': [{transform: {headers: {
      Authorization: `Bearer ${Secret.fill('inference-key')}`
    }}}]}
  }
})
```

The HTTPS broker fills the stored value into outgoing requests on port 443. The
sandbox receives no raw key. Header rules do not allow a blocked destination.
Rotate with `Secret.update('inference-key', newValue)`; delete with `Secret.destroy`.
Deleted or unavailable values cause matching requests to fail. Secret reads return
metadata and version only. Exact domains override wildcard rules entirely.

COPY uploads are streamed through private temporary files and stored separately
from build status/log metadata as bounded binary chunks. The limits remain 64 MiB
compressed per archive, 256 MiB expanded, and 256 MiB retained per template.
Transient database failures trigger retries of failed-build status and sandbox
cleanup. Upgrading from inline upload storage clears only the upload cache; the
SDK reuploads missing archives, while completed templates remain usable.

The guest kernel supports nftables UID-based loopback filtering, including inet
`meta skuid` rules with TCP-reset rejection. Install the `nft` userspace tool in
custom images and apply rules as root. After a kernel upgrade, rebuild running
templates: existing memory snapshots retain their old kernel.
