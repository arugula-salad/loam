# Events

A stream of what happens to your project's sandboxes: created, paused, woken, deleted,
checkpointed. Useful for dashboards and for reacting to sandboxes that sleep or wake on their own.

| Method and path | |
|---|---|
| `GET /v1/events?sandbox=` | Server-Sent Events for the key's project; `sandbox` (an ID or a name) narrows it to one |

Each event is an `event: <type>` line and a `data:` line holding
`{"type", "sandbox", "at", "detail"?}`, where `sandbox` is the sandbox's ID.

```console
$ curl -sS -N -H "Authorization: Bearer $LOAM_TOKEN" "$LOAM_API_URL/v1/events"
event: sandbox.paused
data: {"type":"sandbox.paused","sandbox":"sb-x4cytxhpcior","at":"2026-10-12T09:19:18.35128952Z","detail":{"reason":"request"}}

event: sandbox.running
data: {"type":"sandbox.running","sandbox":"sb-x4cytxhpcior","at":"2026-10-12T09:19:18.564084847Z","detail":{"from":"paused"}}

event: checkpoint.created
data: {"type":"checkpoint.created","sandbox":"sb-x4cytxhpcior","at":"2026-10-12T09:19:19.054822957Z","detail":{"auto":false,"checkpoint":"v1","memory":true}}

event: checkpoint.deleted
data: {"type":"checkpoint.deleted","sandbox":"sb-x4cytxhpcior","at":"2026-10-12T09:19:19.079862957Z","detail":{"checkpoint":"v1"}}
```

```console
$ loam events --sandbox scratch
05:25:33  sandbox.paused         sb-2kjiso7vff5b
05:25:33  sandbox.running        sb-2kjiso7vff5b
```

## Types

| Type | `detail` |
|---|---|
| `sandbox.created` | |
| `sandbox.running` | `from`: the state it woke from; `moved: true` when the wake moved it to another node |
| `sandbox.paused` | `reason`: `request`, `idle`, `deadline`, `node_lost` or `vm_lost` (below), or `reconciled` (loam found the VM so on its node) |
| `sandbox.stopped` | `reason`, as for paused |
| `sandbox.deleted` | `reason`: `request`, `deadline`, or the error code of a create that failed (`capacity`, `invalid`, `unsupported`) |
| `sandbox.error` | `reason` `reconciled` when loam found the VM failed or gone with no snapshot to go back to; none when the first boot failed |
| `checkpoint.created` | `checkpoint`, `memory`, `auto` |
| `checkpoint.restored` | `checkpoint`, and `auto`: the automatic checkpoint kept before |
| `checkpoint.deleted` | `checkpoint` |

**Lost nodes.** If the node a running sandbox is on is lost, loam puts the sandbox back to its
latest snapshot: it becomes `sandbox.paused` (or `sandbox.stopped`) with `detail.reason`
`node_lost` (`vm_lost` when the node lost just the VM) and `detail.snapshot_at`. It wakes from that
snapshot, and what it did after `snapshot_at` is lost.

## Best effort

Events are best effort: one can be missed (a dropped connection, a restart of loam-api), and
there is no replay. The API is the state: a client that reconnects should read what it cares about
(`GET /v1/sandboxes`) and then follow events again.

## Go and TypeScript

```go
evs, err := c.Events(ctx, "") // or a sandbox's ID or name
if err != nil {
	log.Fatal(err)
}
defer evs.Close()
for {
	ev, err := evs.Next()
	if err != nil {
		break
	}
	fmt.Println(ev.Type, ev.Sandbox, ev.Detail)
}
```

```ts
const events = await loam.events({ sandbox: 'scratch' })  // or every sandbox
for await (const ev of events) {
  if (ev.detail?.reason === 'node_lost') console.log(ev.sandbox, 'woke from the snapshot at', ev.detail.snapshot_at)
}
events.close()
```
