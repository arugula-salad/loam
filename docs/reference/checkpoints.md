# Checkpoints

A checkpoint is a snapshot of a sandbox you can go back to (restore) or start new sandboxes from
(fork). A **memory** checkpoint (the default) holds the sandbox's memory and disk, so processes
come back running where they were; a **disk-only** one holds the disk, and comes back as a fresh
boot on it.

```json
{"id": "v1", "sandbox": "sb-2kjiso7vff5b", "comment": "site up, state v1", "memory": true,
 "auto": false, "created_at": "2026-10-12T09:17:36.094927Z"}
```

IDs are `v1`, `v2`, … per sandbox for checkpoints you take (never reused), and `auto-1`, … for the
automatic ones loam keeps before a restore.

| Method and path | |
|---|---|
| `POST /v1/sandboxes/{sandbox}/checkpoints` | `{"comment"?, "memory"?}` → `201` `Checkpoint` |
| `GET /v1/sandboxes/{sandbox}/checkpoints` | → `{"checkpoints": [Checkpoint]}`, oldest first |
| `GET /v1/sandboxes/{sandbox}/checkpoints/{checkpoint}` | → `Checkpoint` |
| `DELETE /v1/sandboxes/{sandbox}/checkpoints/{checkpoint}` | → `204` |
| `POST /v1/sandboxes/{sandbox}/checkpoints/{checkpoint}/restore` | → `Sandbox` |

Listing, reading and deleting checkpoints don't wake the sandbox. Deleting a sandbox deletes its
checkpoints.

## Take a checkpoint

`memory` defaults to `true`. A memory checkpoint pauses the sandbox for about a second while it is
taken; a disk-only one doesn't pause it. Like a pause, a memory checkpoint ends every stream on
the sandbox: exec sockets, port streams and file watches close with status 1011, and other
calls in flight (a port watch among them) end. Exec sessions that aren't detachable end with
their sockets (they get `SIGHUP`); detachable sessions, services and every other process carry
on, and a detachable session can be attached again as soon as the checkpoint is taken. A
disk-only checkpoint ends nothing.

```console
$ loam checkpoint create scratch --comment 'site up, state v1'
v1
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -H 'Content-Type: application/json' \
    "$LOAM_API_URL/v1/sandboxes/scratch/checkpoints" -d '{"comment":"disk only","memory":false}'
{"id":"v2","sandbox":"sb-2kjiso7vff5b","comment":"disk only","memory":false,"auto":false,"created_at":"2026-10-12T09:17:37.727794283Z"}
```

`loam checkpoint create --disk-only` takes a disk-only one. A sandbox can also checkpoint itself
from inside ([Inside a sandbox](inside.md)).

## List, get, delete

```console
$ loam checkpoint ls scratch
ID  KIND    AUTO  CREATED              COMMENT
v1  memory  no    2026-10-12 05:17:36  site up, state v1
v2  disk    no    2026-10-12 05:17:37  disk only
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" "$LOAM_API_URL/v1/sandboxes/scratch/checkpoints/v1"
{"id":"v1","sandbox":"sb-2kjiso7vff5b","comment":"site up, state v1","memory":true,"auto":false,"created_at":"2026-10-12T09:17:36.094927Z"}
$ curl -sS -w '%{http_code}\n' -H "Authorization: Bearer $LOAM_TOKEN" -X DELETE "$LOAM_API_URL/v1/sandboxes/scratch/checkpoints/v2"
204
$ loam checkpoint rm scratch auto-1
```

(The CLI shows times in your local time zone.) A checkpoint that doesn't exist, or no longer does,
is `404 not_found`.

## Restore

`POST .../checkpoints/{checkpoint}/restore` puts the sandbox back to the checkpoint. First loam
keeps its current state as an automatic memory checkpoint (`auto-N`; the newest 5 are kept), so a
restore can be undone. Restoring a memory checkpoint resumes its processes; a disk-only one boots
fresh. Every exec session and port stream on the sandbox ends (status 1011).

```console
$ loam run scratch -- sh -c 'echo v2 > /home/loam/state.txt; pkill -f http.server' </dev/null
$ loam checkpoint restore scratch v1
scratch	running
$ loam run scratch -- sh -c 'cat /home/loam/state.txt; pgrep -a python3' </dev/null
v1
314 python3 -m http.server 8080 --directory /home/loam/site
$ loam checkpoint ls scratch
ID      KIND    AUTO  CREATED              COMMENT
v1      memory  no    2026-10-12 05:17:36  site up, state v1
v2      disk    no    2026-10-12 05:17:37  disk only
auto-1  memory  yes   2026-10-12 05:17:38  before restoring v1
```

```sh
curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -X POST "$LOAM_API_URL/v1/sandboxes/scratch/checkpoints/v1/restore"
```

## Forks

A fork is a new sandbox made from a checkpoint: its disk, and its memory when the checkpoint has
memory, so a fork of a memory checkpoint starts with the source's processes running. It is
`POST /v1/sandboxes` with `from` ([Sandboxes](sandboxes.md#create-a-sandbox)):

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -H 'Content-Type: application/json' "$LOAM_API_URL/v1/sandboxes" \
    -d '{"name":"scratch-disk","from":{"sandbox":"scratch","checkpoint":"v2"}}' | jq -c '{id,name,state,template}'
{"id":"sb-liinyza2erqn","name":"scratch-disk","state":"running","template":"base"}
$ loam fork scratch v1 scratch-copy
name:        scratch-copy
id:          sb-y6sgfql7k3ra
state:       running
url:         https://scratch-copy--docs.loam.example.com
...
$ loam run scratch-copy -- pgrep -a python3 </dev/null
314 python3 -m http.server 8080 --directory /home/loam/site
```

- `template`, `image` and `resources` come from the source and may not be given (`400 invalid`).
- `network` and `secrets` come from the source unless you give your own, so a fork of a restricted
  sandbox is restricted too. Everything else (`env`, `labels`, `idle`, `http_port`, …) is the
  fork's own, as for any create.
- The fork is independent: changing or deleting the source, or the checkpoint, doesn't touch it.
- A fork starts in about the time a wake takes, so one prepared checkpoint can be the start of
  many sandboxes.

```console
$ curl -sS ... "$LOAM_API_URL/v1/sandboxes" -d '{"template":"base","from":{"sandbox":"scratch","checkpoint":"v2"}}'
{"error":{"code":"invalid","message":"a fork takes its template or image and its resources from the checkpoint; don't give them"}}
```

## Go and TypeScript

```go
cp, err := c.CreateCheckpoint(ctx, "scratch", api.CreateCheckpoint{Comment: "before upgrade"})
_, err = c.RestoreCheckpoint(ctx, "scratch", cp.ID)
fork, err := c.Fork(ctx, "scratch", cp.ID, "scratch-2")
```

```ts
const box = loam.sandbox('scratch')
const cp = await box.createCheckpoint({ comment: 'before upgrade' })   // memory: true by default
await box.restoreCheckpoint(cp.id)
await loam.fork('scratch', cp.id, { name: 'scratch-2', labels: { owner: 'docs' } })
await box.deleteCheckpoint(cp.id)
```
