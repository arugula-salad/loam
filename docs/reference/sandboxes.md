# Sandboxes

A sandbox is a Firecracker microVM with its own disk, memory and URL. It is a record in loam
first: it runs on some node only while it's awake, and sleeps as a snapshot the rest of the time.
You never see which node; you see its state.

## The Sandbox object

```json
{
  "id": "sb-2kjiso7vff5b",
  "name": "scratch",
  "project": "docs",
  "template": "base",
  "image": "",
  "state": "running",
  "resources": {"cpus": 2, "memory_mib": 2048, "disk_gib": 20},
  "env": {},
  "labels": {},
  "idle": {"timeout_s": 300, "action": "pause"},
  "deadline": null,
  "protected": false,
  "http_port": 8080,
  "network": {"egress": []},
  "secrets": [],
  "url": "https://scratch--docs.loam.example.com",
  "urls": [],
  "created_at": "2026-10-12T09:11:46.438209Z",
  "updated_at": "2026-10-12T09:11:46.845706Z",
  "last_active_at": "2026-10-12T09:11:46.845706Z"
}
```

| Field | |
|---|---|
| `id` | `sb-` and 12 lowercase base32 characters. Never changes. |
| `name` | A DNS label, unique in the project: lowercase letters, digits and `-`, at most 32 characters, starting and ending with a letter or digit, without `--`. The name and the project's name together are at most 54 characters, so that `<port>--<name>--<project>` stays one DNS label. Generated as `<adjective>-<noun>-<4 digits>` (`nimble-pine-6075`) when you leave it out. |
| `template`, `image` | What it was made from: a template's name, or an image digest with `template` `""`. See [Templates](templates.md) and [Images](#images). |
| `state` | `creating`, `running`, `paused`, `stopped` or `error`; see [States](#states). |
| `resources` | vCPUs, memory in MiB, disk in GiB. Defaults come from the template. Within 1 to 32 CPUs, 128 MiB to 256 GiB of memory, 1 to 2048 GiB of disk, and what a node has room for. |
| `env` | Environment variables every process in the sandbox gets. Stored by loam and handed to the sandbox at every wake; never written into a snapshot. Names are `[A-Za-z_][A-Za-z0-9_]*`. |
| `labels` | Your own key/value tags, for filtering lists. A key is 1 to 63 characters without `=` or `,`; a value at most 255. |
| `idle` | When the sandbox sleeps by itself: after `timeout_s` seconds idle (default 300, at least 30), `action` (`pause`, the default, `stop`, or `none`). |
| `deadline` | `null`, or `{"at": <RFC 3339>, "action": "delete"\|"pause"\|"stop"}`: what happens at a set time. |
| `protected` | While `true`, delete is refused (`409 conflict`). |
| `http_port` | The port the sandbox's own URL goes to; default 8080. |
| `network` | Egress rules; `{"egress": []}`, the default, is the whole internet. See [Network](network.md). |
| `secrets` | Names of project secrets the sandbox uses; each is in its env as a placeholder. See [Secrets](secrets.md). |
| `url` | `https://<name>--<project>.loam.example.com`, which goes to `http_port`. See [Ports and URLs](ports.md). |
| `urls` | The same under each of the project's own URL domains, if the operator gave it any. |
| `last_active_at` | When it was last used. |

## States

| State | What it means | How it wakes |
|---|---|---|
| `creating` | Booting for the first time. Create answers only once it's `running`. | |
| `running` | Awake on a node. | |
| `paused` | Memory and disk kept as a snapshot; uses no CPU or memory. | With every process as it was, usually in well under a second. |
| `stopped` | Disk only. | With a fresh boot on the same disk: files kept, processes gone, services started again. |
| `error` | Something went wrong that loam couldn't recover; delete it. | |

Any call on a sandbox's sub-API (exec, files, ports, services, tasks, its URLs) wakes it, so you
rarely call wake yourself. A sandbox is **idle** when no call is in flight, no exec session is
attached, no [task](tasks.md) is live, and nothing in it has been used for `idle.timeout_s`; then
its `idle.action` applies. A `deadline` applies at its time whatever the sandbox is doing.

## Routes

| Method and path | Body → answer |
|---|---|
| `POST /v1/sandboxes` | `CreateSandbox` → `201` `Sandbox`, once it is running |
| `GET /v1/sandboxes` | `?label=k=v` (repeatable) `&state=` → `{"sandboxes": [Sandbox]}` |
| `GET /v1/sandboxes/{sandbox}` | → `Sandbox` |
| `PATCH /v1/sandboxes/{sandbox}` | `UpdateSandbox` → `Sandbox` |
| `DELETE /v1/sandboxes/{sandbox}` | → `204` |
| `POST /v1/sandboxes/{sandbox}/pause` | → `Sandbox` (`paused`) |
| `POST /v1/sandboxes/{sandbox}/stop` | → `Sandbox` (`stopped`) |
| `POST /v1/sandboxes/{sandbox}/wake` | → `Sandbox` (`running`) |

### Create a sandbox

`POST /v1/sandboxes`

| Field | |
|---|---|
| `name` | Optional; generated when left out. |
| `template` | A template's name; default `base`. |
| `image` | An image digest instead of a template; see [Images](#images). |
| `resources` | `{"cpus", "memory_mib", "disk_gib"}`; any left out (or 0) come from the template. |
| `env`, `labels`, `idle`, `deadline`, `protected`, `http_port`, `network`, `secrets` | As in the object above. |
| `from` | `{"sandbox", "checkpoint"}`: a fork, a new sandbox from a checkpoint. See [Checkpoints](checkpoints.md#forks). |

Create waits for the boot and answers `201` with the sandbox `running`, typically in half a second.

```sh
curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -H 'Content-Type: application/json' \
  "$LOAM_API_URL/v1/sandboxes" \
  -d '{"name":"curl-demo","labels":{"owner":"docs"},"env":{"GREETING":"hello"},"idle":{"timeout_s":120}}'
```

```json
{"id":"sb-zgteynauc2g2","name":"curl-demo","project":"docs","template":"base","image":"","state":"running",
 "resources":{"cpus":2,"memory_mib":2048,"disk_gib":20},"env":{"GREETING":"hello"},"labels":{"owner":"docs"},
 "idle":{"timeout_s":120,"action":"pause"},"deadline":null,"protected":false,"http_port":8080,
 "network":{"egress":[]},"secrets":[],"url":"https://curl-demo--docs.loam.example.com","urls":[],
 "created_at":"2026-10-12T09:12:09.859096Z","updated_at":"2026-10-12T09:12:10.108637364Z",
 "last_active_at":"2026-10-12T09:12:10.108637364Z"}
```

```sh
loam create curl-demo --label owner=docs --env GREETING=hello --idle 2m
loam create big --cpus 4 --memory 8g --disk 50
loam create site --http-port 3000 --deadline 2h --deadline-action stop
```

Errors: `400 invalid` for a bad name, label, env name, idle or deadline (`deadline.at` must be in
the future), unknown template, or both `template` and `image`; `409 already_exists` for a name
the project already has; `503 capacity` (with `Retry-After`) when no node has room;
`501 unsupported` for secrets on a deployment without a secret key.

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -H 'Content-Type: application/json' \
    "$LOAM_API_URL/v1/sandboxes" -d '{"name":"curl-demo"}'
{"error":{"code":"already_exists","message":"the project already has a sandbox named \"curl-demo\""}}
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -H 'Content-Type: application/json' \
    "$LOAM_API_URL/v1/sandboxes" -d '{"idle":{"timeout_s":10}}'
{"error":{"code":"invalid","message":"idle.timeout_s must be at least 30"}}
```

Go:

```go
sb, err := c.CreateSandbox(ctx, api.CreateSandbox{
	Name:   "go-demo",
	Labels: map[string]string{"owner": "docs"},
	Idle:   &api.Idle{TimeoutS: 120},
})
```

TypeScript:

```ts
const sb = await loam.createSandbox({ name: 'ts-demo', labels: { owner: 'docs' }, idle: { timeout_s: 120 } })
```

### Images

`image` makes the sandbox from an OCI image of your own instead of a template:
`<repository>@sha256:<digest>`, from a registry loam allows (on this deployment,
`ghcr.io/arugula-salad`), never a tag, because every node must build the same disk from it.

- `resources` default to 2 CPUs, 2048 MiB and 20 GiB.
- The image's `Env` becomes the sandbox's environment, beneath its `env`.
- Its entrypoint and command are **not** run; define a [service](services.md) for what should run
  at boot. An image may also ship service definitions in `/var/lib/loam/services/<name>.json`,
  which start at every boot like any other.
- The first sandbox from an image waits while a node pulls it and builds a disk (about 50
  seconds for the 1 GiB `e2b` image); later ones from the same digest start in under a second.
- A fork keeps its source's image.

```console
$ loam create image-demo --cpus 1 --memory 1g \
    --image ghcr.io/arugula-salad/loam-e2b@sha256:2d3516ea7948ab2216f5f35f8d82f2a9220e11dff75539ab330477460cbbfd14
name:        image-demo
id:          sb-lda6ljxett5u
image:       ghcr.io/arugula-salad/loam-e2b@sha256:2d3516ea7948ab2216f5f35f8d82f2a9220e11dff75539ab330477460cbbfd14
state:       running
resources:   1 cpus, 1024 MiB, 20 GiB disk
...
```

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -H 'Content-Type: application/json' \
    "$LOAM_API_URL/v1/sandboxes" -d '{"image":"ghcr.io/arugula-salad/loam-guest:latest"}'
{"error":{"code":"invalid","message":"image \"ghcr.io/arugula-salad/loam-guest:latest\": want <repository>@sha256:<64 hex digits>, never a tag"}}
```

An image from another registry is `invalid` too; a deployment that allows no registry answers
`501 unsupported`.

### List sandboxes

`GET /v1/sandboxes?label=k=v&state=running`. `label` may repeat (all must match); `state` is one
of the states above.

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" "$LOAM_API_URL/v1/sandboxes?label=owner=docs&state=running" \
    | jq -c '.sandboxes[] | {id,name,state,labels}'
{"id":"sb-zgteynauc2g2","name":"curl-demo","state":"running","labels":{"owner":"docs"}}
```

```console
$ loam ls
NAME       ID               STATE    TEMPLATE  CPUS  MEM_MIB  LABELS                 AGE
scratch    sb-2kjiso7vff5b  paused   base      2     2048                            4m
curl-demo  sb-zgteynauc2g2  paused   base      2     2048     owner=docs,stage=test  4m
$ loam ls --label owner=docs --state running
```

### Get a sandbox

`GET /v1/sandboxes/{sandbox}`, by ID or name. Reading it doesn't wake it.

```sh
curl -sS -H "Authorization: Bearer $LOAM_TOKEN" "$LOAM_API_URL/v1/sandboxes/scratch"
loam info scratch
```

### Change a sandbox

`PATCH /v1/sandboxes/{sandbox}` with any of `env`, `labels`, `idle`, `deadline`, `protected`,
`http_port`, `network`, `secrets`. What you leave out stays as it is.

- `env` and `labels` replace the whole map. A new `env` reaches processes started after the
  change, and the whole sandbox at its next wake; processes already running keep theirs.
- `idle`: a field you leave out keeps its value (`{"timeout_s": 600}` keeps the action).
- `deadline`: an object sets it, `null` clears it, leaving it out leaves it.
- `network` and `secrets` replace the whole value and apply at once to an awake sandbox (a new
  secret's placeholder reaches the env like `env` does).

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -H 'Content-Type: application/json' -X PATCH \
    "$LOAM_API_URL/v1/sandboxes/curl-demo" \
    -d '{"labels":{"owner":"docs","stage":"test"},"deadline":{"at":"2026-10-13T00:00:00Z","action":"delete"},"protected":true}' \
    | jq -c '{labels,deadline,protected}'
{"labels":{"owner":"docs","stage":"test"},"deadline":{"at":"2026-10-13T00:00:00Z","action":"delete"},"protected":true}
```

The CLI's `update` changes one key at a time rather than replacing maps:

```sh
loam update scratch --env GREETING=hi --unset-env OLD --label team=docs --unset-label stage
loam update scratch --idle 10m --idle-action stop
loam update scratch --deadline 2h --deadline-action pause   # "none" clears it
loam update scratch --protected true
loam update scratch --http-port 3000
```

Its egress flags (`--allow`, `--deny`, `--allow-defaults`) are the exception: given at all, they
replace the sandbox's whole rule list; `--unrestricted` empties it. `--secret` and
`--unset-secret` add and remove one secret.

Errors: as for create, plus `sandbox_unavailable` when a `network` or `secrets` change couldn't
reach the sandbox's node (it is kept, and applies at the next wake). A change the node refuses
is not kept.

### Delete a sandbox

`DELETE /v1/sandboxes/{sandbox}` → `204`. Deletes its disk, snapshots and checkpoints, and ends
every stream to it. A protected sandbox is `409 conflict`:

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -X DELETE "$LOAM_API_URL/v1/sandboxes/curl-demo"
{"error":{"code":"conflict","message":"the sandbox is protected; unprotect it first"}}
$ loam update curl-demo --protected false && loam rm curl-demo
```

### Pause, stop, wake

`POST /v1/sandboxes/{sandbox}/pause`, `/stop` and `/wake` answer the sandbox in its new state.
Each is a no-op on a sandbox already in that state. Pausing a stopped sandbox is `409 conflict`
(it has no memory to keep).

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -X POST "$LOAM_API_URL/v1/sandboxes/builder/pause" | jq -c '{state}'
{"state":"paused"}
$ loam stop builder && loam wake builder
builder	stopped
builder	running
```

Pausing (by request or by the idle rule) closes every exec session's socket and port stream with
status `1011`; the processes themselves are in the snapshot and carry on after the wake. Detachable
sessions can be attached again ([Exec](exec.md#sessions)).

In Go: `c.Pause`, `c.Stop`, `c.Wake`, `c.DeleteSandbox`. In TypeScript: `loam.pause`,
`loam.stop`, `loam.wake`, `loam.deleteSandbox`.
