# loam API, v1

Base URL: `https://api.loam.example.com` (configurable as `LOAM_API_URL`). JSON in and out
unless a route says otherwise. Go types for every body: `pkg/api`.

The user docs are HTML on the same host, without a key: `GET /docs` (where to start),
`/docs/primer`, and the reference at `/docs/reference/` and `/docs/reference/{page}`, made from
`docs/primer.md` and `docs/reference/*.md`. Not on a sandbox's host, where every path is the
sandbox's.

## Authentication and projects

Every request carries `Authorization: Bearer <key>`. A key belongs to one **project**, and
everything a key creates belongs to its project; a key never sees another project's sandboxes
(they answer 404, not 403). Keys look like `loam_<32 base32 chars>`; only their SHA-256 is stored.

The **admin token** (`LOAM_ADMIN_TOKEN` in loam-api's environment) is the only way to manage projects and keys:

| Route | Body → answer |
|---|---|
| `POST /v1/projects` | `{"name"}` → `Project`. `name` is a DNS label (`[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?`, no `--`). |
| `GET /v1/projects` | → `{"projects": [Project]}` |
| `DELETE /v1/projects/{project}` | 409 `project_not_empty` while it has sandboxes |
| `POST /v1/projects/{project}/keys` | `{"name", "scope"?}` → `Key` with `secret`, shown only here. `scope`: `full` (the default) may do anything in the project; `read` may only read: GET and HEAD, no WebSocket (exec and port connections are upgrades of a GET); anything else is 403 `permission_denied`. For a reader such as a log shipper. |
| `GET /v1/projects/{project}/keys` | → `{"keys": [Key]}` (no secrets) |
| `DELETE /v1/projects/{project}/keys/{key}` | |

`GET /v1/whoami` (any key) → `{"project": Project, "key": Key}`.

## Errors

Every error answer is `{"error": {"code": "<snake_case>", "message": "<sentence>"}}` with a
fitting status. Codes the clients rely on: `unauthorized` 401, `not_found` 404,
`already_exists` 409, `invalid` 400, `unsupported` 501 (a feature this deployment can't
enforce), `capacity` 503 (no node can take the sandbox; carries `Retry-After`),
`sandbox_unavailable` 503 (it could not be woken), `conflict` 409 (a state that forbids the
call, e.g. deleting a `protected` sandbox), `permission_denied` 403 (a file operation the
`user` may not do), `port_closed` 502. Every error uses this shape, including a refused
WebSocket upgrade.

## Sandboxes

```json
{
  "id": "sb-7k2q9xw4m3ab",
  "name": "scratch",
  "project": "jake",
  "template": "base",
  "image": "",
  "state": "running",
  "resources": {"cpus": 2, "memory_mib": 2048, "disk_gib": 20},
  "env": {"FOO": "bar"},
  "labels": {"owner": "ci"},
  "idle": {"timeout_s": 300, "action": "pause"},
  "deadline": null,
  "protected": false,
  "http_port": 8080,
  "network": {"egress": [{"domain": "github.com", "action": "allow", "ports": [22, 443]}]},
  "secrets": ["GITHUB_TOKEN"],
  "url": "https://scratch--jake.loam.example.com",
  "urls": ["https://scratch.games.example.com"],
  "created_at": "2026-10-11T18:00:00Z",
  "updated_at": "2026-10-11T18:00:00Z",
  "last_active_at": "2026-10-11T18:00:00Z"
}
```

- `id`: `sb-` and 12 lowercase base32 characters. `name`: a DNS label of at most 32 characters, unique in the project, with no `--`; generated (`<adjective>-<noun>-<4 digits>`) when omitted. Every route takes either as `{sandbox}`. A name and its project together are at most 54 characters, so that `<port>--<name>--<project>` stays one DNS label.
- `labels`: string to string. A key is 1 to 63 characters without `=` or `,`; a value is at most 255 characters. Anything else is `invalid`.
- `state`: `creating`, `running`, `paused`, `stopped`, `error`. `deadline`: `null` or `{"at": RFC 3339, "action": "delete"|"pause"|"stop"}`.
- `idle.action`: `pause` (default), `stop` or `none`. `idle.timeout_s` defaults to 300; the minimum is 30.
- `env`: given to every process the sandbox runs. Stored, and handed to the agent at every wake; never baked into a snapshot.
- `url`: the sandbox's own URL, which goes to `http_port` (default 8080). See "Ports". `urls`: the same, under each of its project's URL domains (see "URL domains").
- `image`: the image the sandbox was made from, when it was made from one rather than a template (`template` is then `""`).
- `network`: what the sandbox may reach (see "Network"). `{"egress": []}`, the default, is the whole internet.
- `secrets`: the names of project secrets the sandbox uses (see "Secrets"). Each is in its env as a placeholder; the value never enters the sandbox.

| Route | Body → answer |
|---|---|
| `POST /v1/sandboxes` | `CreateSandbox` → 201 `Sandbox`, once it is `running` (create waits for the boot). |
| `GET /v1/sandboxes` | `?label=k=v` (repeatable) `&state=` → `{"sandboxes": [Sandbox]}` |
| `GET /v1/sandboxes/{sandbox}` | → `Sandbox` |
| `PATCH /v1/sandboxes/{sandbox}` | any of `env`, `labels`, `idle`, `deadline`, `protected`, `http_port`, `network`, `secrets` → `Sandbox`. `env` takes effect at the next wake and for new processes started after it. `network` and `secrets` replace the whole value and take effect at once on an awake sandbox (a secret's placeholder reaches its env at the next wake, like `env`); a change its node refuses is not kept, and one its node didn't answer is kept and answered `sandbox_unavailable`. `"deadline": null` clears the deadline; leaving it out leaves it. `env` and `labels` replace the whole map. In `idle`, a field left out keeps its current value. |
| `DELETE /v1/sandboxes/{sandbox}` | 204. 409 `conflict` when `protected`. Deletes its checkpoints too. |
| `POST /v1/sandboxes/{sandbox}/pause` | → `Sandbox` (`paused`) |
| `POST /v1/sandboxes/{sandbox}/stop` | → `Sandbox` (`stopped`) |
| `POST /v1/sandboxes/{sandbox}/wake` | → `Sandbox` (`running`) |

`CreateSandbox`: `{"name"?, "template"? ("base"), "image"?, "resources"?, "env"?, "labels"?, "idle"?,
"deadline"?, "protected"?, "http_port"?, "network"?, "secrets"?, "from"?: {"sandbox", "checkpoint"}}`. With `from`,
the new sandbox starts from that checkpoint (a fork): its disk, and its memory when the
checkpoint has memory; `template` and `resources` then come from the source and may not be given.
A fork takes its source's `network` and `secrets` unless it gives its own.

`image` makes the sandbox from an OCI image instead of a template: `<repository>@sha256:<digest>`,
from a registry loam-api allows (its `--allowed-registries`, `ghcr.io/arugula-salad` here), never
a tag, since every node must build the same disk from it. It and `template` exclude each other;
`resources` default to `{"cpus": 2, "memory_mib": 2048, "disk_gib": 20}`. Anything else is
`invalid`; a deployment that allows no registry answers 501 `unsupported`. A fork keeps its
source's image. The image's config `Env` becomes the sandbox's environment beneath its `env`, as a
template's does; its entrypoint and command are not run (services are how a sandbox runs
something at boot), but they, its working directory and its user are recorded in
`/etc/loam/image.json` for a client that makes them a service (`docs/AGENT.md`, "The guest disk").

**Waking.** Every route under `/v1/sandboxes/{sandbox}/` other than `pause`, `stop` and the
record routes (reading and deleting checkpoints, port settings) wakes the sandbox first and holds it awake until the call ends (a stream: until it
closes). A paused sandbox wakes with its processes; a stopped one boots fresh on its disk.

## Exec

### Streaming: `GET /v1/sandboxes/{sandbox}/exec` (WebSocket)

Query: `cmd` (repeatable, argv; required), `tty` (`true`/`false`), `cols`, `rows`, `cwd`,
`env` (repeatable `K=V`, on top of the sandbox's), `user` (default `loam`; `root` allowed),
`detachable` (`true`: the session outlives the socket; see attach).

Frames:

| Direction | Frame | Meaning |
|---|---|---|
| client → | binary, first byte `0` | stdin bytes (the rest of the frame) |
| client → | binary, single byte `4` | stdin EOF |
| client → | text `{"type":"resize","cols":N,"rows":N}` | TTY size |
| client → | text `{"type":"signal","signal":"TERM"}` | to the process group; names as in `kill -l` |
| → client | text `{"type":"started","session":"s-…","pid":N}` | first frame |
| → client | binary, first byte `1` / `2` | stdout / stderr bytes (TTY: all output is `1`) |
| → client | text `{"type":"exit","code":N}` | last frame; `code` is 128+signal when killed. The socket then closes normally. |
| → client | text `{"type":"error","error":{"code","message"}}` | the command could not start; then close |

If the sandbox goes away under a session (deleted, restored, paused, a memory checkpoint taken,
its node lost), the socket closes with status 1011 and no `exit` frame. Clients must treat that
as "exit unknown", not success.

### Sessions

| Route | |
|---|---|
| `GET /v1/sandboxes/{sandbox}/exec/sessions` | → `{"sessions": [Session]}`: `{"id","cmd","tty","pid","started_at","exited","exit_code"?,"attached"}` |
| `GET /v1/sandboxes/{sandbox}/exec/sessions/{session}` (WebSocket) | attach. `?offset=N` replays output from byte N of the session's buffer (default: everything kept, up to 1 MiB per session), then live. The offset counts stdout and stderr together, in the order they were produced. An offset older than what is kept starts at the oldest kept byte. Same frames; the first is `{"type":"attached","session","offset"}`, where `offset` (always present, 0 included) is where the replay actually starts. |
| `POST /v1/sandboxes/{sandbox}/exec/sessions/{session}/signal` | `{"signal"}` |

A session that is not `detachable` ends (SIGHUP to its process group) when its socket closes.
A detachable one keeps running; an exited session, detachable or not, stays listed for 10
minutes with its output. Pausing a sandbox (by request or the idle rule) closes every exec
socket with 1011; the sessions are in the memory snapshot, so a detachable one can be attached
again after the wake. A memory checkpoint closes every exec socket with 1011 too, and the
sessions that aren't detachable end with their sockets; a detachable one runs on and can be
attached again at once (see "Checkpoints").

### One-shot: `POST /v1/sandboxes/{sandbox}/exec/run`

`{"cmd": [argv], "stdin"?: "<base64>", "cwd"?, "env"?: {}, "user"?, "timeout_s"? (default 60, max 3600)}`
→ `{"exit_code": N, "stdout": "<base64>", "stderr": "<base64>", "timed_out": bool}`. Output
beyond 16 MiB per stream is cut, with `"truncated": true`.

## Files

Paths are absolute inside the sandbox. File operations run as root unless `user` is given.

| Route | |
|---|---|
| `GET /v1/sandboxes/{sandbox}/fs/file?path=` | raw bytes; `Content-Type: application/octet-stream` |
| `PUT /v1/sandboxes/{sandbox}/fs/file?path=&mode=0644&mkdir=true` | raw body; → `Stat` |
| `DELETE /v1/sandboxes/{sandbox}/fs/file?path=&recursive=false` | 204. A directory that isn't empty, without `recursive=true`, is 409 `conflict`. |
| `GET /v1/sandboxes/{sandbox}/fs/stat?path=` | → `Stat`: `{"path","name","type":"file"|"dir"|"symlink"|"other","size","mode":"0644","mtime","target"?}` |
| `GET /v1/sandboxes/{sandbox}/fs/list?path=` | → `{"entries": [Stat]}` |
| `POST /v1/sandboxes/{sandbox}/fs/mkdir` | `{"path","parents"?,"mode"?}` → `Stat` |
| `POST /v1/sandboxes/{sandbox}/fs/rename` | `{"from","to"}` → `Stat` |
| `GET /v1/sandboxes/{sandbox}/fs/tar?path=` | a tar of the directory, streamed (`application/x-tar`); entry names are relative to `path` (`a/b.txt`, not `/home/loam/dir/a/b.txt` or `dir/a/b.txt`) |
| `PUT /v1/sandboxes/{sandbox}/fs/tar?path=` | extracts the body (a tar, names relative to `path`) into the directory, creating it and its parents if missing → 204. A name that leaves `path` (`..`, or through a symlink) is `invalid`. |
| `GET /v1/sandboxes/{sandbox}/fs/watch?path=&recursive=false` (WebSocket) | text frames `{"type":"create"|"write"|"remove"|"rename"|"chmod","path"}` |

`user=` (a query parameter on every route above, including the ones with JSON bodies) runs the
operation as that user; permissions apply, and a refusal is 403 `permission_denied`. A missing
path is 404 `not_found`.

## Ports

| Route | |
|---|---|
| `GET /v1/sandboxes/{sandbox}/ports` | → `{"ports": [Port]}`: `{"port","address","pid","session"?}`; listeners on every address. |
| `GET /v1/sandboxes/{sandbox}/ports/watch` | Server-Sent Events: `event: opened` / `event: closed`, `data: Port`. Watching is not activity. |
| `GET /v1/sandboxes/{sandbox}/ports/{port}/connect` (WebSocket) | a raw TCP stream to `127.0.0.1:{port}` in the sandbox, in binary frames both ways. Nothing listening is 502 `port_closed`, before the upgrade. |
| `PUT /v1/sandboxes/{sandbox}/ports/{port}` | `{"access": "private"|"public"}` → `{"port","access","url"}` |
| `GET /v1/sandboxes/{sandbox}/ports/settings` | → `{"ports": [{"port","access","url"}]}`: only the ports whose access was set. Every port has a URL of the form below whether or not it was set. |

**URLs.** `https://<name>--<project>.<domain>` goes to `http_port`;
`https://<port>--<name>--<project>.<domain>` to any port. Plain HTTP and WebSocket, over TLS
terminated by Traefik. A port is `private` by default: its URL needs the project's key
(`Authorization: Bearer`, or a `loam_token` cookie set by `?loam_token=<key>` on first visit).
`public` opens it to anyone. A request to a sleeping sandbox wakes it. A request to a port
nothing listens on answers 502 `port_closed` after waiting up to 10 s for a listener.

**URL domains.** The operator gives a project domains of its own (loam-api's `--domains` file,
`{"domains": [{"domain": "games.example.com", "project": "arcade"}]}`, with a wildcard certificate
and route for each in the deploy). A sandbox named N in that project is then also
`https://N.<domain>` (its `http_port`) and `https://<port>--N.<domain>`, with the same access
rules; the `loam_token` cookie is set per host. A domain belongs to one project, and may not
lie under loam's domain or another project's, nor they under it. Every host under a URL domain is
a sandbox's: the domain itself, and a host there that names no sandbox of its project, are 404
`not_found`, never the API. `GET /v1/projects` and `whoami` show a project's `domains`.

## Stats

loam-api counts every request to a sandbox's URLs (both forms, under loam's domain and a URL
domain) as it proxies it, whether the sandbox was awake or not; the app does nothing.

`GET /v1/sandboxes/{sandbox}/stats?days=N` → `Stats`: `{"sandbox", "name", "days": [StatsDay],
"total": Traffic, "visitor_days"}`. `days` (default 7, at most 90) is the window: today (UTC)
and the days before it, oldest first, every day present, zeros on a day without traffic.
`Traffic` is `{"requests", "status_2xx", "status_3xx", "status_4xx", "status_5xx",
"websockets", "bytes_sent", "key_requests"}`; a `StatsDay` is a `Traffic` with `"day"`
(`2006-01-02`) and `"visitors"`. A read key may ask.

- `requests` is every request; a WebSocket upgrade counts in `websockets` and in no status
  class. The status is what the client got, loam-api's own answers included (401 on a private
  port, 502 `port_closed`). `bytes_sent` is response bodies, WebSockets not included.
- `key_requests` carried the project's key: every answered request to a private port, and a
  request to a public one with the key as a bearer token, the `loam_token` cookie or
  `?loam_token=`.
- `visitors` is the day's distinct clients: a client is its address (the last
  `X-Forwarded-For` entry, which Traefik sets) and its `User-Agent`. A request is a visit when
  it got an answer below 400 (101 included) without the project's key and its User-Agent
  isn't empty or a known machine (crawlers, link previews, uptime probes, HTTP libraries such
  as curl). A client is only known within a day: the store keeps a hash of a salt, the
  sandbox and the client, and deletes the salt when the day is over. So `visitor_days`, the
  days' `visitors` added up, counts one person on three days three times. Two people behind
  one address with the same browser are one visitor.
- Kept 90 days. Today's count includes requests up to the call (it writes the buffered counts
  first). A deleted sandbox's traffic goes with it.

loam-api also serves Prometheus metrics on a listener of its own (`--metrics-listen`, `:9090`,
`/metrics`), never on the public one: `loam_port_requests_total{project,sandbox,code}` (`code`
`2xx`..`5xx` or `websocket`), `loam_port_request_duration_seconds{project,sandbox}` (WebSockets
not included), `loam_port_response_bytes_total`, `loam_port_websockets_open`,
`loam_sandboxes{project,state}` and `loam_traffic_flush_failures_total`. With `--access-log`
(the default) it logs a line per request: project, sandbox, port, host, method, path, status,
duration, bytes, WebSocket, key, User-Agent; never the client's address.

## Services

Supervised processes that start at every boot, restart on crash, and survive a pause (they are
part of the memory snapshot) and a stop (they start again on the next boot).

| Route | |
|---|---|
| `GET /v1/sandboxes/{sandbox}/services` | → `{"services": [Service]}` |
| `PUT /v1/sandboxes/{sandbox}/services/{name}` | `{"cmd": [argv], "cwd"?, "env"?, "user"?, "needs"?: [names], "http_port"?: N}` → `Service` |
| `GET /v1/sandboxes/{sandbox}/services/{name}` | → `Service`: the definition plus `{"state": "running"|"starting"|"stopped"|"crashed","pid"?,"restarts","started_at"?}` |
| `DELETE /v1/sandboxes/{sandbox}/services/{name}` | stops it and removes it |
| `POST /v1/sandboxes/{sandbox}/services/{name}/{start\|stop\|restart}` | → `Service` |
| `GET /v1/sandboxes/{sandbox}/services/{name}/logs?follow=false&tail=N&wake=true` | `text/plain`, stdout and stderr interleaved. Following (`follow=true`) is not activity: it wakes the sandbox but doesn't keep it from going idle, and a pause ends it. `wake=false`: a sandbox that isn't running is 409 `conflict`, not woken (for a log shipper) |

A service with `http_port` N is started, if it isn't running, by a request to port N's URL; the
sandbox's own URL goes to the sandbox's `http_port`, so the two match when a service is meant to
be the sandbox's site. Access follows the port's setting either way.

## Tasks

A task holds the sandbox awake: while one is live, the idle rule doesn't pause or stop it. Every
task expires, so a client that crashed can't hold a sandbox for ever; to hold longer, put it again
before it runs out. Tasks count wall-clock time, a pause included.

| Route | |
|---|---|
| `GET /v1/sandboxes/{sandbox}/tasks` | → `{"tasks": [Task]}`: `{"name","started_at","expires_at"}`, live ones only |
| `PUT /v1/sandboxes/{sandbox}/tasks/{name}` | `{"expire_s"?}` (default and maximum 3600) → `Task`. Creates the task or moves its expiry. At most 64 live tasks (409 `conflict`). |
| `GET /v1/sandboxes/{sandbox}/tasks/{name}` | → `Task` |
| `DELETE /v1/sandboxes/{sandbox}/tasks/{name}` | 204 |

## Checkpoints

```json
{"id": "v3", "sandbox": "sb-…", "comment": "before upgrade", "memory": true, "auto": false, "created_at": "…"}
```

| Route | |
|---|---|
| `POST /v1/sandboxes/{sandbox}/checkpoints` | `{"comment"?, "memory"? (default true)}` → 201 `Checkpoint`. A memory checkpoint pauses the sandbox for about a second, and ends every stream and call on it as a pause does: exec sockets, port streams and `fs/watch` close with 1011, and every other call in flight, `ports/watch` among them, ends (taking the snapshot resets the guest's connections to the agent). Exec sessions that aren't detachable end with their sockets (SIGHUP); detachable ones, services and every other process run on. A disk-only checkpoint doesn't pause the sandbox and ends nothing. |
| `GET /v1/sandboxes/{sandbox}/checkpoints` | → `{"checkpoints": [Checkpoint]}`, oldest first |
| `GET /v1/sandboxes/{sandbox}/checkpoints/{checkpoint}` | → `Checkpoint` |
| `DELETE /v1/sandboxes/{sandbox}/checkpoints/{checkpoint}` | 204 |
| `POST /v1/sandboxes/{sandbox}/checkpoints/{checkpoint}/restore` | → `Sandbox`. The sandbox's current state is kept first as an automatic checkpoint (`auto-N`, the newest 5 kept). Restoring a memory checkpoint resumes its processes; a disk-only one boots fresh. Every exec session and port stream on the sandbox ends (1011). |

IDs are `v1`, `v2`, … per sandbox for checkpoints you take, `auto-1`, … for automatic ones.

## Templates

`GET /v1/templates` → `{"templates": [{"name","image","resources"}]}`. Phase 1 templates are
operator-configured (loam-api's `--templates` file); there is no API to build one.

## Network

A sandbox reaches the internet and nothing else: never its node, the cluster, the LAN or another
sandbox. `network.egress` narrows that to a list of rules, enforced outside the guest, on its
node; nothing in the sandbox can widen it.

```json
{"egress": [
  {"include": "defaults"},
  {"domain": "*.githubusercontent.com", "action": "allow"},
  {"domain": "github.com", "action": "allow", "ports": [22, 443]},
  {"domain": "gist.github.com", "action": "deny"}
]}
```

- `domain`: an exact name (`github.com`), `*.` and a name (`*.github.com`: every name under it,
  not the name itself), or `*` (every name). Lowercase ASCII; an IP address is not a name and is
  `invalid`, so a restricted sandbox reaches no address it didn't resolve.
- `action`: `allow` or `deny`. `ports` (allow only): the TCP ports the rule opens, default
  `[80, 443]`. A deny covers every port.
- `{"include": "defaults"}`: loam's list of common development hosts (package registries, GitHub
  and GitLab, container registries, the major AI APIs), each with the default ports.
  `GET /v1/network/defaults` → `{"egress": [Rule]}` shows it.
- At most 1000 rules, as given and with `defaults` expanded.

**Deciding.** A connection to name N on port P is allowed when, among the rules that cover P, the
most specific that matches N is an allow: an exact name beats the longest `*.` suffix, which
beats `*`, whatever their order; an allow and a deny on the same `domain` are a deny. No match is
a deny. A name resolves when it is allowed on some port.

**Enforcing.** An empty list (the default) is the whole internet, over TCP, UDP and ICMP. Any
rule makes the sandbox **restricted**:
- Only TCP leaves it, and only IPv4: UDP (QUIC included: clients fall back to TCP), ICMP and IPv6
  are dropped. DNS goes to its node's resolver, which answers names that are allowed and refuses
  the rest (`REFUSED`), and never answers a private address.
- On ports 80 and 443 the name is checked on every connection: the `Host` header, or the TLS
  ClientHello's server name, must be allowed on that port, and the connection goes to an address
  of that name that the node resolves itself, wherever the sandbox aimed it. So a name sharing an
  allowed name's address (a CDN) stays out. A TLS connection without a server name is refused.
  With an encrypted ClientHello (ECH) the outer name is checked: the node never answers the
  HTTPS records that carry ECH keys, so only a client given keys some other way can hide a name
  behind it, and only behind an ECH provider's public name (`cloudflare-ech.com`), which a rule
  would have to allow.
- On any other port there is no name to read: a connection is allowed to an address the sandbox
  resolved, within its TTL, from a name allowed on that port.
- A refused connection is closed at once; on port 80 it gets a `403` with the error shape
  (`permission_denied`) first.

Changing `network` on an awake sandbox applies at once and closes its connections that the new
rules don't allow.

## Secrets

A project's secrets are credentials its sandboxes use without holding them. The sandbox's env has
a **placeholder** where the value would be; on the way out of the sandbox, in a request to one of
the secret's hosts, its node puts the value in. The value reaches nothing else: not the sandbox's
memory, disk, snapshots or logs.

```json
{"name": "GITHUB_TOKEN", "hosts": ["api.github.com", "github.com"],
 "placeholder": "__loam_github_token__", "inject": null,
 "created_at": "…", "updated_at": "…"}
```

| Route | Body → answer |
|---|---|
| `PUT /v1/secrets/{name}` | `{"value", "hosts", "inject"?, "placeholder_prefix"?}` → `Secret`. Creates or replaces it; a replaced value reaches awake sandboxes at once. |
| `GET /v1/secrets` | → `{"secrets": [Secret]}` |
| `GET /v1/secrets/{name}` | → `Secret`. Never the value. |
| `DELETE /v1/secrets/{name}` | 204. 409 `conflict` while a sandbox names it. |

- `name`: an environment variable name, `[A-Z_][A-Z0-9_]*`, at most 64 characters. A sandbox
  whose `env` has the same name is `invalid`.
- `value`: 1 byte to 8 KiB, without CR, LF or NUL.
- `hosts` (at least one): where the value may go, over HTTPS (port 443) only: `api.github.com`, `*.github.com`, or
  either with a path prefix (`api.github.com/repos/`). A request that matches none goes out with
  the placeholder as it is, which is worth nothing. In a restricted sandbox the hosts are allowed
  on 443 as if by a rule.
- `placeholder`: `__loam_<name in lowercase>__`, after `placeholder_prefix` when given (for a
  client that checks its token's shape: `sk-ant-oat01-`; at most 64 printable ASCII characters,
  no spaces).
- `inject`: `null` (the default) puts the value wherever the placeholder appears in the request
  head (the path, the query and every header value, and inside an `Authorization: Basic`
  credential, decoded), byte for byte. `{"header": "x-api-key", "format": "{value}"}` instead
  sets that header on every request to the hosts, replacing any the sandbox sent; `format` holds
  `{value}` once (`"Bearer {value}"`). `header` is a header name, and not one that frames or
  routes the request (`Host`, `Content-Length`, `Transfer-Encoding`, `Connection`, `Upgrade`, …).
- A deployment without a secret key answers every secret route, and a sandbox that names
  secrets, `unsupported`. A sandbox with secrets runs only on a node with the broker CA: none at
  all is `unsupported`, none with room is `capacity`. Bodies and WebSocket messages are never changed.

What a brokered request can't do: follow a redirect for the sandbox (the sandbox sees it, and the
next request is matched afresh), or carry the value back. A response whose headers or body hold a
value (as is, or with `/` written `\/`) is cut off: `502` if its head hasn't been sent, else a
reset. To read the body, the node asks the host for an uncompressed answer (`Accept-Encoding:
identity`), and an answer encoded anyway is `502`. A request that needs a secret its node can't
get is `502`, never sent without it. The node's own `502`s carry the code `broker_failed`. A
request on a brokered connection whose `Host` isn't the connection's server name is `421`.

TLS to a secret's hosts ends at the sandbox's node, with a certificate from loam's broker CA,
which every sandbox trusts (`docs/AGENT.md`); the node checks the host's own certificate. Every
other connection, TLS or not, is never decrypted.

## Identity

A sandbox can prove which sandbox it is to anything that accepts OpenID Connect tokens (AWS STS,
Vault, a service of your own), without holding a credential. loam-api is the issuer:

| Route | |
|---|---|
| `GET /.well-known/openid-configuration` | Discovery, without a key: `issuer` (the API's base URL), `jwks_uri`, `id_token_signing_alg_values_supported` (`RS256`), … |
| `GET /.well-known/jwks.json` | The public keys, without a key. A rotated-out key stays here while tokens it signed can be live. |

From inside the sandbox only (see "Inside a sandbox"): `GET /identity/token?audience=<aud>&ttl_s=600`
→ `{"token": "<JWT>", "expires_at": "…"}`. `audience` is required; `ttl_s` defaults to 600, at most 3600.
The token is RS256, with `iss`, `sub` (`project:<project>:sandbox:<id>`), `aud`, `iat`, `nbf`,
`exp`, `jti`, and `project`, `sandbox` (the ID), `sandbox_name`, `template` and `labels`; its
header's `kid` is the RFC 7638 thumbprint of the key that signed it. A missing `audience` or a
`ttl_s` that isn't whole seconds from 1 to 3600 is `invalid`. A deployment without a signing key
answers `unsupported`, on these routes and on `/.well-known`.

## Events

`GET /v1/events?sandbox=` Server-Sent Events for the key's project: `event: <type>`,
`data: Event` (`{"type","sandbox","at","detail"?}`; `sandbox` is the ID). Types: `sandbox.created`, `sandbox.running`,
`sandbox.paused`, `sandbox.stopped`, `sandbox.deleted`, `sandbox.error`,
`checkpoint.created`, `checkpoint.restored`, `checkpoint.deleted`. Best effort: the API is the
state, and a client that reconnects reads it. A running sandbox whose node was lost becomes
`sandbox.paused` (or `sandbox.stopped`) with `detail.reason` `node_lost` (`vm_lost` when its node
lost the VM) and `detail.snapshot_at`: it wakes from the snapshot taken then, and what it did after
is lost. A wake that moved the sandbox to another node says `detail.moved: true`.

## Inside a sandbox

The agent serves this same sandbox sub-API to processes inside the sandbox on
`/run/loam/api.sock` (paths without the `/v1/sandboxes/{sandbox}` prefix), so `curl --unix-socket
/run/loam/api.sock http://loam/fs/list?path=/` works from inside. Being inside is the
credential. Three routes there reach loam-api for the sandbox itself:
`GET /self` → `Sandbox`, `POST /checkpoints` (as above; it answers before the pause), and
`GET /identity/token` (see "Identity").

## E2B

A deployment with an E2B domain (loam-api's `--e2b-domain`, here `e2b.loam.example.com`) also
serves E2B's API there, for E2B's own SDKs, with a project's key as `E2B_API_KEY`: `docs/E2B.md`.
An E2B sandbox is a sandbox of the key's project like any other, made from the `e2b` template, its
E2B metadata in labels under `e2b/` and its timeout its `deadline`; this API sees and acts on it
as on any other.


### HTTPS request header transforms

`network.request_headers` is an optional array of `{domain, headers}`. Each header
value is literal or contains `${e2b.secrets.NAME}` references to the project's E2B
secret store. These transforms do not change egress permission or attach a native
secret to the guest. They require a broker-capable node and apply only to HTTPS
port 443. The most specific matching domain supplies the entire header map; exact
domains outrank `*.` wildcards. Native attached secrets retain their existing host
allowance and placeholder behavior.

Domains must be DNS names or leading wildcards, without paths. At most 128 rules,
64 headers per rule, and 16 KiB per header format are accepted. Duplicate domains,
invalid header names, CR/LF/NUL, and routing/framing headers are refused. Unknown
secret references cause matching requests to fail closed. Snapshot forks inherit
formats, and waking resolves current project secret values. Rules returned by the
API contain references rather than resolved credentials.
