# Inside a sandbox

Processes in a sandbox can use loam's API about their own sandbox without a key, on a Unix socket:

```
/run/loam/api.sock
```

Being inside is the credential: the socket is only reachable from within the VM, and it only ever
speaks for that one sandbox. It is open to every user in the sandbox (mode `0666`).

## What it serves

The sandbox's own sub-API, at the same paths without the `/v1/sandboxes/{sandbox}` prefix, with
the same parameters, bodies, frames and errors: so `/exec/run`, `/exec`, `/exec/sessions/...`,
`/fs/...`, `/ports/...`, `/services/...` and `/tasks/...` all work, as described on their pages.
The host name in the URL is ignored; these docs write `http://loam`.

Three routes go out to loam-api for the sandbox itself:

| Method and path | |
|---|---|
| `GET /self` | → this sandbox's `Sandbox` record ([Sandboxes](sandboxes.md)) |
| `POST /checkpoints` | `{"comment"?, "memory"?}` → `201` `Checkpoint`: checkpoint this sandbox ([Checkpoints](checkpoints.md)) |
| `GET /identity/token` | `?audience=&ttl_s=` → `{"token", "expires_at"}` ([Identity](identity.md)) |

The rest of the checkpoint routes (list, get, restore, delete), the lifecycle routes (pause, stop,
delete) and anything under `/v1/` are not served here (`404`); use a project key from outside for
those.

## Examples

All of these run inside the sandbox (here through `loam run`, but any process works):

```console
$ curl -sS --unix-socket /run/loam/api.sock http://loam/self | jq -c '{id,name,state}'
{"id":"sb-2kjiso7vff5b","name":"scratch","state":"running"}
$ curl -sS --unix-socket /run/loam/api.sock 'http://loam/fs/list?path=/home/loam/site'
{"entries":[{"path":"/home/loam/site/index.html","name":"index.html","type":"file","size":25,"mode":"0664","mtime":"2026-10-12T09:15:46.143559219Z"}]}
$ curl -sS --unix-socket /run/loam/api.sock http://loam/exec/run -d '{"cmd":["hostname"]}' | jq -r .stdout | base64 -d
scratch
$ curl -sS --unix-socket /run/loam/api.sock -X PUT http://loam/tasks/inside -d '{"expire_s":60}'
{"name":"inside","started_at":"2026-10-12T09:21:26.915260002Z","expires_at":"2026-10-12T09:22:26.915260002Z"}
$ curl -sS --unix-socket /run/loam/api.sock 'http://loam/identity/token?audience=sts.amazonaws.com'
{"token":"eyJhbGciOiJSUzI1NiIs...","expires_at":"2026-10-12T09:36:26Z"}
$ curl -sS --unix-socket /run/loam/api.sock http://loam/checkpoints -d '{"comment":"from inside"}'
{"id":"v3","sandbox":"sb-2kjiso7vff5b","comment":"from inside","memory":true,"auto":false,"created_at":"2026-10-12T09:21:26.829389423Z"}
$ curl -sS --unix-socket /run/loam/api.sock http://loam/v1/sandboxes
{"error":{"code":"not_found","message":"no such route: GET /v1/sandboxes"}}
```

### Checkpointing yourself

`POST /checkpoints` answers as soon as loam has accepted it, **before** the sandbox pauses for the
snapshot, so the answer isn't lost inside the snapshot (a sandbox later restored from that
checkpoint would otherwise wake up still waiting for it). The checkpoint shows in the list once the
snapshot is taken, about a second later. The calling process is paused with everything else and
carries on afterwards (unless it runs in an exec session from outside that isn't detachable: a
memory checkpoint ends those, as it closes their sockets); in a restore or fork of the checkpoint, it carries on from just after the
answer.

That makes "prepare, then checkpoint" a one-liner at the end of a setup script:

```sh
apt-get install -y ... && npm ci && \
curl -sS --unix-socket /run/loam/api.sock http://loam/checkpoints -d '{"comment":"deps installed"}'
```

### From TypeScript

The TypeScript client talks to the socket with `LoamClient.inside()`, and the single-file
`loam.mjs` runs on the `base` template's Node:

```js
import { LoamClient } from './loam.mjs'

const me = LoamClient.inside() // /run/loam/api.sock; no key
const self = await me.self()
console.log('I am', self.name, self.id, self.state)
const { token, expires_at } = await me.identityToken('sts.amazonaws.com')
console.log('token for sts.amazonaws.com until', expires_at, token.slice(0, 20) + '...')
console.log((await me.run({ cmd: ['uptime'] })).stdout.toString().trim())
```

```console
$ loam cp --user loam loam.mjs scratch:/home/loam/loam.mjs
$ loam cp --user loam inside.mjs scratch:/home/loam/inside.mjs
$ loam run scratch -- node /home/loam/inside.mjs </dev/null
I am scratch sb-2kjiso7vff5b running
token for sts.amazonaws.com until 2026-10-12T09:34:01Z eyJhbGciOiJSUzI1NiIs...
09:24:01 up 7 min,  0 user,  load average: 0.00, 0.00, 0.00
```

`me.createCheckpoint()` checkpoints the sandbox, and the rest of the sub-API (`exec`, files,
services, tasks) is there as on `loam.sandbox(...)`.

## Other things the sandbox gives its processes

- The environment: the sandbox's `env` (with [secret](secrets.md) placeholders), plus the image's,
  and the broker CA's variables (`NODE_EXTRA_CA_CERTS=/etc/loam/ca.pem`, `SSL_CERT_FILE`,
  `REQUESTS_CA_BUNDLE`, `CARGO_HTTP_CAINFO`, `UV_NATIVE_TLS=1`).
- The hostname: the sandbox's name.
- `/var/lib/loam/services/<name>.json`: the [service](services.md) definitions, on the disk.
- `/var/lib/loam/hooks/presuspend.d/`: executables put here run as root before every pause or
  memory checkpoint (5 seconds for all of them together); the place to flush or close what
  shouldn't be snapshotted mid-write.
