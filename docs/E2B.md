# The E2B adapter

loam-api speaks E2B's API too, so that the official, unmodified E2B SDKs (Python and JS) create,
use, pause, resume and kill sandboxes on loam as they do on hosted E2B. It began as the E2B front
end of sandpit, the single-host Firecracker system loam replaced, and now runs on loam's records:
`internal/e2b`, wired into loam-api by `--e2b-domain`. This is its contract; where it and the code disagree, one of them is a bug.

The yardstick is the SDK conformance run in `e2e/e2b/` ("Conformance"): both SDKs, pinned,
through create, exec with streaming, background processes, stdin and PTY, files, signed
upload and download URLs, a sandbox port, set-timeout, metrics, list, pause and resume, and kill.

## Reaching it

| | |
|---|---|
| Domain | loam-api's `--e2b-domain` (`LOAM_E2B_DOMAIN`), say `e2b.loam.example.com`, with a wildcard certificate and route for it (`docs/INSTALL.md`, and the example in `deploy/examples/basic/`) |
| Control plane | `https://api.<domain>` (also `https://<domain>`) |
| A sandbox's port P | `https://<P>-<id>.<domain>`; envd is P = 49983 |
| Key | `X-API-Key`: a loam project key (`loam_…`) |

So the SDKs need only:

```sh
E2B_DOMAIN=e2b.loam.example.com
E2B_API_KEY=loam_...
```

The SDKs take the control plane to be `https://api.${E2B_DOMAIN}` and a sandbox's envd to be
`https://49983-<id>.<sandbox domain>`, where the sandbox domain is the `domain` the API answered
(the adapter always gives it) or `E2B_DOMAIN`. `E2B_SANDBOX_URL` (marked internal in the SDKs,
honoured by both) works too, pointed at either control host: envd calls are then routed by the
`E2b-Sandbox-Id` and `E2b-Sandbox-Port` headers every SDK envd call carries, and signed file URLs,
which then name no sandbox, by their signature (below).

loam-api serves the adapter on the domain and on single labels under it only; every other host
is loam's own API and port URLs, untouched. The adapter takes no other host, so loam's
`<port>--<name>--<project>.<loam domain>` URLs and these never meet.

## What an E2B sandbox is in loam

An ordinary loam sandbox in the key's project, made by the adapter through loam's own API in the
same process (an E2B call is loam calls, with every loam rule applied), so it is visible and
usable through loam's API and CLI as well:

| E2B | loam |
|---|---|
| sandbox ID | the loam sandbox ID (`sb-` and 12 base32 characters). E2B's proxy and the SDKs take the left-most host label as `<port>-<id>`, the port being everything before the first dash, so the dash in `sb-` routes; nothing in either SDK parses an ID. |
| template `base` (`base:default` too) | loam's template named by `--e2b-template` (`e2b`): E2B's base image with envd (`images/e2b`). The response's `templateID` and `alias` are both `base`. |
| any other template | one of `--e2b-templates` (`name=loam-template[:user],...`), each made from a loam template whose image runs envd as the e2b template's does, `user` being envd's default user (`/init`'s `defaultUser`, with that user's home as `defaultWorkdir`; `user` and `/home/user` when not given), as a hosted template's build sets them; for example `fountain`, from `e2b-fountain` as `sprite` (`images/e2b-fountain`: the e2b image with one E2B client's expected layout, a `sprite` user and the agent CLIs). A request for another user still runs in the default working directory unless it names one, as on hosted E2B, so it must be one that user can enter. `templateID` and `alias` are the name; the template filter of a listing matches it. Only the `default` tag exists, and a loam template's own name is no E2B template. |
| `metadata` k: v | label `e2b/k`: v. Labels loam's API gives the sandbox are not metadata unless they carry the prefix. A key loam's label rules refuse (over 59 characters, `=` or `,`) is a 400. |
| `envVars` | the sandbox's `env`, which loam hands every process its agent starts, and envd gets at `/init` |
| `timeout` | the sandbox's `deadline`, action `delete` (with `autoPause`: `pause`, or `stop` with `autoPauseMemory: false`). `idle` is `none`: an E2B sandbox runs until its timeout. |
| pause / connect | loam's pause (or stop) and wake |
| `allow_internet_access`, `network` | `network.egress` ("Network") |
| everything else E2B knows | the adapter's record, `e2b_sandboxes` (`db.E2BSandbox`): envd's access token (sealed, below), the create's timeout, `startedAt`, `endAt`, `autoPause`, `autoPauseMemory`, `autoResume` and E2B's paused. It is deleted with the sandbox, however that is deleted. |

A sandbox the adapter didn't make is not an E2B sandbox, even by ID: the E2B API answers 404 for
it, and doesn't list it. Another project's sandbox is 404 too, as on loam's API.

**The access token** is envd's, 32 random bytes in hex, made at create. It is a credential: never
logged, never in a label, an event or the sandbox's env. The SDK needs it back on create, connect
and get, as hosted gives it, so it is kept whole, but sealed: AES-256-GCM under loam-api's secret
key (`LOAM_SECRET_KEY`, the key project secrets are sealed with), a fresh nonce each time, with
the sandbox's ID (and a prefix no project secret's has) as additional data, so a ciphertext
copied into another sandbox's row, or changed at all, doesn't open. Opened only to answer the
project's keys through the E2B API, to check a request's token or signature, and to send
`/init`. A sandbox whose token doesn't open is refused: its envd traffic gets the proxy's 502,
its `GET` a 500, never a token that isn't its own. A deployment without a secret key can't keep
tokens sealed, so it answers every E2B create 501 (a sandbox's credentials can't be kept safe).

## Control plane

E2B's REST API as the SDKs use it. Shapes, status codes and messages are hosted E2B's (its OpenAPI
spec, its API server's handlers and traces recorded against hosted E2B). Every error body is E2B's
`{"code": <status>, "message": "…"}`; the SDKs map by status: 401 `AuthenticationError`, 404 the
call's not-found (`false` for kill), 429 `RateLimitError` (retried after an integer
`Retry-After`), anything else a `SandboxError` showing the message.

| Route | |
|---|---|
| `POST /v2/sandboxes` | `NewSandboxV2` → 201 `Sandbox` (`alias`, `clientID` `6532622b`, `domain`, `envdAccessToken`, `envdVersion` `0.9.0`, `sandboxID`, `templateID`), once envd has taken its `/init`. Timeout default 300 s, at least 1 s, at most `--e2b-max-timeout` (24 h). |
| `POST /sandboxes` | Deprecated create: the same, default timeout 15 s. |
| `GET /sandboxes/{id}` | 200 `SandboxDetail`: `cpuCount`, `memoryMB`, `diskSizeMB` from the sandbox's resources, `startedAt`, `endAt`, `state`, `metadata`, `lifecycle` (`onTimeout` `kill` or `pause`, `autoResume`), `envdAccessToken`, `volumeMounts` (`[]` while running, absent while paused). |
| `DELETE /sandboxes/{id}` | 204: loam's delete. 404 afterwards for every route. |
| `POST /sandboxes/{id}/timeout` | `{"timeout": s}` → 204: `endAt` and the deadline become now + timeout (it can shorten). 404 while paused. |
| `POST /sandboxes/{id}/refreshes` | Deprecated: `{"duration": s}` extends the timeout, never shortens it. |
| `POST /sandboxes/{id}/pause` | `{}` → 204: loam's pause, a memory snapshot; `{"memory": false}`: loam's stop, the disk only. The deadline is cleared first, then the record marked paused (`endAt` the pause time), then the VM paused, so a timeout can't kill it mid-pause and traffic arriving meanwhile is refused rather than waking it. A failed pause undoes both. 409 `Error pausing sandbox - sandbox '<id>' is already paused`. |
| `POST /v2/sandboxes/{id}/connect`, `POST /sandboxes/{id}/connect` | `{"timeout"?}` (default 300 s). Paused: loam's wake, envd's `/init` if the wake was a fresh boot, `startedAt` now, `endAt` now + timeout → 201 `Sandbox`. Running: the timeout only extended → 200. |
| `POST /sandboxes/{id}/resume` | Deprecated connect (default 15 s), 409 on a running sandbox. |
| `PUT /sandboxes/{id}/network` | `{"allow_internet_access"?, "allowOut"?, "denyOut"?}` → 204: the sandbox's egress rules replaced, as at create; loam applies them at once to a running sandbox, closing what they no longer allow. |
| `GET /sandboxes/{id}/metrics` | `[SandboxMetric]`: one sample, envd's own `/metrics`, for a sandbox awake in loam; `[]` otherwise (asking doesn't wake it). `start` and `end` are ignored: there is no history. |
| `GET /v2/sandboxes` | `[ListedSandbox]` (no token, no lifecycle), the project's E2B sandboxes in `state` (`running,paused` by default), filtered by `metadata` (`k=v&k2=v2`, URL-encoded, exact), `template`, `startedAfter`; newest first or `order=asc`; pages of `limit` (1 to 100), the next page's cursor in `X-Next-Token`; `X-Total-Running` when running ones are asked for. |
| `GET /sandboxes` | Deprecated list: running only, unpaged. |
| `GET /health` | `Health check successful` |

**Create's options.** Honoured: `templateID`, `timeout`, `metadata`, `envVars`, `autoPause`,
`autoPauseMemory`, `autoResume`, `secure: true`, `allow_internet_access`, `network.allowOut`,
`network.denyOut`, `network.allowPublicTraffic: true`. Any other field that isn't null is a 400
naming it (`mcp`, `iam`, `volumeMounts`, `network.rules`, `network.egressProxy`, …), as are
`secure: false` (every sandbox here has an access token), `network.allowPublicTraffic: false`, and
the SDKs' own constraints (`autoResume` or `autoPauseMemory` without `autoPause`, `autoResume`
with `autoPauseMemory: false`). An unknown template is 404 `template '<name>' not found`, as hosted.

**Loam's refusals** come through in E2B's terms: a key that isn't one, 401; no node with room
(`capacity`), 429 with loam's `Retry-After`; anything loam finds `invalid` (a label, an env name,
a network rule), 400 with loam's message; a sandbox that can't be woken, 503; a deployment without a secret key, 501 for every create.

**State.** E2B's state is the adapter's, not loam's: `paused` once paused through the API (until a
connect), or once the timeout of an `autoPause` sandbox has passed (loam's deadline pauses it);
`running` otherwise. A running E2B sandbox may be asleep in loam (its node drained, loam's API
paused it): its next traffic wakes it, as any call on a loam sandbox does. A sandbox E2B calls
paused that loam's API woke stays paused to E2B, refusing traffic, until a connect.

**autoResume.** Traffic for a paused `autoResume` sandbox, once authorized as below, resumes it
as a connect would, with the timeout it was created with; then the request goes through.

## Network

E2B's egress options become loam's egress rules (docs/API.md, "Network"), enforced by the
sandbox's node outside the guest. E2B denies addresses, CIDRs and names (`denyOut`) with
exceptions that win over them (`allowOut`); loam's rules are names, decided by the most specific,
and a sandbox with any rule reaches only what they allow. So:

- `allow_internet_access: false`, or `0.0.0.0/0` (the SDKs' `ALL_TRAFFIC`) in `denyOut`, is
  `{"domain": "*", "action": "deny"}`, then each `denyOut` name denied and each `allowOut` name
  allowed (on loam's default ports, 80 and 443, which is where E2B's own name filtering works).
- Without that, `allowOut` makes exceptions to nothing and limits nothing; a name in `denyOut` is
  refused, since loam can't allow everything but a name (a restricted sandbox is TCP to what its
  rules allow, nothing else).
- An IP address or a CIDR, other than `ALL_TRAFFIC`, is refused anywhere: loam's rules are names,
  which is what keeps another name on a shared address out. `ALL_TRAFFIC` in `allowOut` is
  everything, since E2B's allow wins.
- A denied name an allowed one covers (`allowOut *.x.com` with `denyOut api.x.com`) is refused:
  E2B would allow it, loam would deny it.

A restricted sandbox's processes reach envd and its user ports all the same: those come in over
loam's port stream, not the network. envd's polling of 169.254.169.254 (it looks for E2B's
metadata service for 60 s after every `/init`, `-isnotfc` or not) never leaves the VM: loam drops
link-local traffic from every VM, and a restricted one's port 80 goes to the node's proxy, which
refuses an address. envd only logs that it gave up.

## The data plane

Everything the SDKs send a sandbox rather than the API goes over loam's port stream to the port
in the guest (the agent's `/ports/{port}/connect`): no guest network is involved, so it works the
same in a restricted sandbox. A request is for a sandbox, decided as E2B's proxy decides it:

1. a Host of `<port>-<id>.<domain>`;
2. else the headers `E2b-Sandbox-Id` and `E2b-Sandbox-Port` (default 49983), on either control host;
3. else a signed `GET`/`POST /files?…&signature=…` (an upload or download URL under
   `E2B_SANDBOX_URL`, which names no sandbox): the sandbox whose access token the signature was
   made with, found by computing it with each one's (envd's algorithm: `v1_` and the unpadded
   base64 SHA-256 of `<path>:<read|write>:<user>:<token>[:<expiration>]`). One that matches none
   is 401. A paused sandbox's URLs match only with `autoResume`.

Anything else on the adapter's hosts is the control plane.

- **Ports are numbers.** 1 to 65535, normalized (`049983` is 49983); anything else is 400 before
  anything is dialled.
- **Auth before wake**. A request to envd must carry the sandbox's token as
  `X-Access-Token`, or a `/files` signature made with it that hasn't expired, or it is 401 and the
  sandbox is not woken: envd checks again itself, but only once the sandbox is awake, so checking
  first keeps traffic without a credential from waking a sleeping sandbox or keeping a running
  one up. The path is cleaned once, before the check, and forwarded as cleaned.
- **envd's internal API** (`/init`, `/freeze`, `/unfreeze`, `/fsfreeze`, `/fsthaw`, `/collapse`,
  `/upgrade`) is 404 from outside, however the path or port is spelled. `/init` is the adapter's.
- **User ports are public**, as on hosted E2B: `<port>-<id>.<domain>` needs no credential. This
  is E2B's default, not loam's: the same port through loam's own URL
  (`<port>--<name>--<project>.<loam domain>`) keeps loam's access setting, private by default.
- **A paused sandbox** (E2B's paused) answers the proxy's 502 `{"sandboxId", "message": "The
  sandbox was not found", "code": 502}`, which the SDKs read as not running; so does an unknown
  ID. A port nothing listens on answers 502 `The sandbox is running but port is not open` at once.
- **Streams.** Everything is passed through as it is (the token, the SDK's encodings, the Host),
  both ways, flushed on every write, full duplex (a Connect stream's request is read while its
  response is written). A call holds the sandbox awake for as long as it lasts. A pause (or stop,
  or delete) ends every port stream of the sandbox; a Connect server stream (`application/connect+…`,
  envelopes `[flags u8][length u32 BE][message]`) cut at an envelope boundary is closed with an
  end-stream envelope carrying `{"error": {"code": "unavailable", "message": "the connection to
  sandbox <id> ended before the stream completed"}}`, as hosted's proxy does, rather than left
  cut off. The processes themselves survive a memory pause and are listed after the resume.

## envd in the guest

The `e2b` template's image (`images/e2b/Containerfile`) is loam's guest image reshaped like E2B's
`base`: Ubuntu 24.04, user `user` (uid 1000, passwordless sudo, home `/home/user`), `loam`
(uid 1001) for loam's own API, Node 22, Python with `pip` that just works, `/code`, hostname file
`e2b`; and envd, E2B's in-guest daemon, built from a pinned commit of `github.com/e2b-dev/infra`
(`92197909`, Apache-2.0, **envd 0.9.0**, past every SDK feature gate) into a static binary at
`/usr/bin/envd`. The SDKs gate features on the `envdVersion` the API reports, so the adapter
reports the image's: moving the pin moves `EnvdVersion` in `internal/e2b` with it.

**Started at every boot.** The image ships `/var/lib/loam/services/envd.json`, a loam service
definition (`docs/AGENT.md`, "The guest disk"): `/usr/bin/envd -isnotfc -port 49983 -no-cgroups`,
as root. loam-agent starts every service at the first wake of each boot, restarts a crashed one,
and keeps it through a pause (it is in the memory snapshot); it shows in the sandbox's
`services` like any other. `-isnotfc`: envd isn't on E2B's Firecracker hosts, so it takes its
token by `/init` rather than from E2B's metadata service. `-no-cgroups`: envd would otherwise make
cgroups of its own beside the agent's.

**envd's `/init`.** After a fresh boot envd has no token and answers anyone who reaches its port,
with root as its default user. The adapter hands it the sandbox's:

```
POST /init (over the port stream to 49983, no auth header)
{"accessToken": "<token>", "defaultUser": "user", "defaultWorkdir": "/home/user",
 "envVars": {...}, "timestamp": "<RFC 3339 now>"}                            → 204
```

`envVars` is what envd gives every process it starts, which takes only `PATH` from envd's own
environment: `E2B_SANDBOX=true`, `E2B_SANDBOX_ID`, `E2B_TEMPLATE_ID` (what hosted envd sets), the
sandbox's `env`, and, for a sandbox that names loam secrets, their placeholders and the broker
CA's variables (`NODE_EXTRA_CA_CERTS=/etc/loam/ca.pem`, `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE` and
`CARGO_HTTP_CAINFO` at Ubuntu's bundle, `UV_NATIVE_TLS=1`), as loam's agent gives its own
processes. A change to the env reaches envd at its next `/init`, which is the next fresh boot.

**When.** Once per boot of the VM, before anything else of E2B's reaches envd in it:

- The boot is the VM's `booted_at` on its node (docs/NODE.md, `VM`), which loam-api reads for the
  adapter (`BootedAt`). A fresh boot moves it: a create, a wake from a stop or a disk-only pause,
  a restore, a move to another node, a VM its nodelet restarted under. A resume from a memory
  snapshot on the same node doesn't, and envd there still has its token, defaults and processes.
- Every request for port 49983 (and the adapter's own calls to envd: metrics) dials the port
  first, which wakes the sandbox if it sleeps, then compares `booted_at` with the boot the
  adapter last initialized envd in, and calls `/init` if they differ, before sending the request.
  A create and a connect that wakes a sandbox do the same before they answer, so the SDK's first
  call finds envd ready, as hosted's create returns only once envd answers.
- `/init` is retried for up to 30 s while nothing listens on 49983 (envd starting). envd answering
  401 means something in the guest set a token first: whatever answers there isn't the adapter's
  to hand the SDK, so the call fails (a create is then deleted; a request gets the proxy's 502).
- The boots are remembered in loam-api's memory, so a restarted loam-api calls `/init` once more
  in each sandbox's current boot. envd takes a second `/init` that carries the same token (it
  steps the clock, nothing else), so that is harmless.

**Exposure before `/init`.** envd listens on every address of the guest. Before its `/init` it is
reachable by the sandbox's own processes and through loam's API by the project's keys (port
connect to 49983), never from the network (loam lets nothing reach a VM). A loam call that wakes
an E2B sandbox fresh leaves envd waiting for the adapter's next call; anything in that window
that sets a token of its own makes the adapter refuse the sandbox, as above. An envd that crashes
and is restarted by the agent within a boot has lost its token too; it is set again only at the
next fresh boot (or loam-api restart), and until then envd answers without one.

## Not here

Answered 404 (`Not found: this E2B-compatible server does not implement …`): templates and their
build API (`/v3/templates`, `/templates/…`), snapshots and `fork` (`/sandboxes/{id}/fork`,
`/sandboxes/{id}/snapshots`, `/snapshots`), volumes, sandbox logs and events,
`GET /sandboxes/metrics`, teams and API key management. Refused at create (above): templates
other than `base`, `mcp`, `iam`, volumes, egress proxies and transform rules, CIDR rules,
`allowPublicTraffic: false` and its `e2b-traffic-access-token`, `secure: false`.

## Differences from hosted E2B

Measured against hosted E2B with the conformance probes.

- `templateID` is `base`, not hosted's resolved template ID; `alias` is `base` on both.
- `envdVersion` is `0.9.0` (hosted's `base` runs 0.6.10).
- `domain` is set in create, connect and get (hosted leaves it out), so `getHost(port)` points here
  whatever `E2B_DOMAIN` says.
- `cpuCount`, `memoryMB`, `diskSizeMB` are the e2b template's resources (2, 1024, 20480), not
  hosted's (2, 512, 23301); `memTotal` in metrics is the guest's, a little under that.
- Metrics are one sample read from envd when asked, `[]` for a sandbox asleep in loam; no history.
- No `X-RateLimit-*` headers. Nothing is rate limited but loam's capacity, which answers 429.
- Unary envd answers carry envd's `Content-Length` where hosted's proxy chunks them; the bodies
  are the same.
- Timeouts are loam's deadlines, acted on within a second of `endAt`. A sandbox killed by its
  timeout is deleted, disk and all, as hosted.
- A pause is loam's: a memory snapshot in the store, which wakes on any node with its processes;
  a paused sandbox has no timeout and is kept until killed, as hosted.
- The hostname is the loam sandbox's name (loam's agent sets it at every wake), not `e2b`.
- The marker file `/run/e2b/.E2B_SANDBOX` says `false` (`-isnotfc`); `E2B_SANDBOX=true` is in
  the commands' environment, as hosted.
- Port traffic reaches a guest port on `127.0.0.1` (loam's port stream), so a server listening
  only on loopback is reachable, where hosted needs `0.0.0.0` (or envd's socat forwarder).
- The 401 for a bad key names loam's keys rather than E2B's dashboard.

## Conformance

`e2e/e2b/run.sh` runs both official SDKs (pinned: `e2e/e2b/requirements.txt`,
`e2e/e2b/package.json`), 13 steps each, against a deployment, then a sweep for leaked sandboxes:

```sh
E2B_DOMAIN=e2b.loam.example.com E2B_API_KEY=loam_... e2e/e2b/run.sh
```

It needs python3 and node; the venv lives in `${XDG_CACHE_HOME:-~/.cache}/loam/e2b-probe` and
`node_modules` beside the script. `ONLY=py|js` runs one SDK, `E2B_PROBE_PAUSE=0` skips pause and
resume. Its exit status is the number of failures. The unit tests (`internal/e2b`) run the
adapter on a real loam-api over the fake nodelet with a fake envd.

**Locally**, without a cluster: a nodelet in a container shaped like its pod, a registry beside
it for the e2b image, and loam-api on the host in plain HTTP, with a domain that carries its port
(the adapter matches hosts without it and reports it with it, so `getHost` reaches the server):

```sh
scripts/build-initramfs.sh
docker build -f images/nodelet/Containerfile -t loam-nodelet:dev .
docker build -f images/e2b/Containerfile -t loam-e2b:dev images/e2b
docker network create loam-dev
docker run -d --name loam-registry --network loam-dev -p 127.0.0.1:5011:5000 registry:3
docker tag loam-e2b:dev localhost:5011/loam-e2b:dev && docker push localhost:5011/loam-e2b:dev   # its digest
docker run -d --name loam-nodelet --network loam-dev --device /dev/kvm --device /dev/net/tun \
  --device /dev/userfaultfd --cap-add NET_ADMIN -e LOAM_NODE_TOKEN=node \
  loam-nodelet:dev --data /data --dev-allow-copy --node-name local --memory 6144 --store-url file:///data/store
# templates.json: "e2b" with image <registry's container address>:5000/loam-e2b@<digest>
# (a private address: the nodelet pulls from it over plain HTTP)
LOAM_ADMIN_TOKEN=admin LOAM_NODE_TOKEN=node go run ./cmd/loam-api --listen 127.0.0.1:7901 \
  --database-url memory --nodelet local=<nodelet's container address>:7070 --templates templates.json \
  --domain loam.localhost --e2b-domain e2b.localhost:7901
# a project and a key through the admin token (docs/API.md), then:
E2B_DOMAIN=e2b.localhost:7901 E2B_API_URL=http://api.e2b.localhost:7901 \
  E2B_SANDBOX_URL=http://api.e2b.localhost:7901 E2B_PROBE_PORT_SCHEME=http \
  E2B_API_KEY=loam_... e2e/e2b/run.sh
```

Run that way on one KVM host (2026-10-12), both SDKs passed every step, 26 of 26, in Firecracker VMs of
the e2b image; so did egress (`allow_internet_access: false`, `denyOut` of all traffic with
`allowOut` names, and `update_network`) and the refusal of a CIDR. The same run passes against the
reference deployment, a three-node k3s cluster.

## Reference: the SDKs

From a survey of the SDKs (2.52.0) and E2B's infra (`92197909`), the parts the adapter depends
on.

- **Transport.** envd speaks the Connect protocol with the JSON codec: unary calls are
  `POST /<package>.<Service>/<Method>` with `Content-Type: application/json`; server streams
  (`process.Process/Start`, `Connect`, `filesystem.Filesystem/WatchDir`) are
  `application/connect+json` envelopes, the last flagged end-stream (`0x02`) with `{}` or
  `{"error": …}`. Plain HTTP besides: `GET /health` (204), `GET`/`POST /files` (download and
  multipart or octet-stream upload), `GET /metrics`, `GET /envs`. Every envd call carries
  `E2b-Sandbox-Id`, `E2b-Sandbox-Port: 49983` and `X-Access-Token`; a user is chosen with
  `Authorization: Basic base64("<user>:")`, else envd's default from `/init`.
  `Connect-Timeout-Ms` on `Start` is the process's lifetime (60 s by default in the SDKs).
- **Version gates** on `envdVersion`: < 0.1.0 refused; 0.1.4 recursive watch; 0.3.0 `stdin: false`;
  0.4.0 default user from envd; 0.5.2 `CloseStdin`; 0.5.7 octet-stream upload; 0.6.2 file metadata;
  0.6.3 `include_entry` in watch events; 0.6.4 network mounts.
- **Retries.** 3 on 429 (only with an integer `Retry-After`), 502, 503 and connection failures; a
  create is not retried on 502.
- **HTTP/2** is negotiated by ALPN on TLS (Traefik); loam-api sees HTTP/1.1.
