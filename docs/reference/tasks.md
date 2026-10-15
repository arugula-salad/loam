# Tasks

A sandbox goes to sleep when it's idle: no call in flight, no exec session attached, and nothing
used for `idle.timeout_s`. A job that runs on its own (a build started with `exec -d`, a crawler,
a training run) doesn't count as use, so the sandbox would pause under it. A **task** holds the
sandbox awake: while one is live, the idle rule doesn't pause or stop it.

Every task expires, at most an hour after it was put, so a client that crashed can't hold a
sandbox forever. To hold longer, put it again before it runs out; each put moves the expiry.
Expiry counts wall-clock time, a pause included. A task doesn't stop an explicit pause or stop, or
a deadline.

| Method and path | |
|---|---|
| `GET /v1/sandboxes/{sandbox}/tasks` | → `{"tasks": [Task]}`, live ones only |
| `PUT /v1/sandboxes/{sandbox}/tasks/{name}` | `{"expire_s"?}` → `Task`: creates the task or moves its expiry |
| `GET /v1/sandboxes/{sandbox}/tasks/{name}` | → `Task` |
| `DELETE /v1/sandboxes/{sandbox}/tasks/{name}` | → `204` |

A `Task` is `{"name", "started_at", "expires_at"}`. `name` is yours to choose, 1 to 128 bytes
(URL-encode it in the path). `expire_s` is 1 to 3600, default 3600. A sandbox has at most 64 live
tasks (`409 conflict` beyond). An expired task is gone: `GET` answers `404`.

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -H 'Content-Type: application/json' -X PUT \
    "$LOAM_API_URL/v1/sandboxes/svc-demo/tasks/build" -d '{"expire_s":600}'
{"name":"build","started_at":"2026-10-12T09:19:14.063953276Z","expires_at":"2026-10-12T09:29:14.063953276Z"}
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -X PUT "$LOAM_API_URL/v1/sandboxes/svc-demo/tasks/deploy"
{"name":"deploy","started_at":"2026-10-12T09:19:14.103378915Z","expires_at":"2026-10-12T10:19:14.103378915Z"}
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" "$LOAM_API_URL/v1/sandboxes/svc-demo/tasks"
{"tasks":[{"name":"build","started_at":"2026-10-12T09:19:14.063953276Z","expires_at":"2026-10-12T09:29:14.063953276Z"},{"name":"deploy","started_at":"2026-10-12T09:19:14.103378915Z","expires_at":"2026-10-12T10:19:14.103378915Z"}]}
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" "$LOAM_API_URL/v1/sandboxes/svc-demo/tasks/build"
{"name":"build","started_at":"2026-10-12T09:19:14.063953276Z","expires_at":"2026-10-12T09:29:14.063953276Z"}
$ curl -sS -w '%{http_code}\n' -H "Authorization: Bearer $LOAM_TOKEN" -X DELETE "$LOAM_API_URL/v1/sandboxes/svc-demo/tasks/build"
204
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -H 'Content-Type: application/json' -X PUT \
    "$LOAM_API_URL/v1/sandboxes/svc-demo/tasks/x" -d '{"expire_s":7200}'
{"error":{"code":"invalid","message":"expire_s must be between 1 and 3600; refresh the task to hold longer"}}
```

There is no CLI command for tasks. The TypeScript client has them:

```ts
const box = loam.sandbox('svc-demo')
await box.putTask('build', { expire_s: 600 })
// ... refresh with putTask again before it expires, for longer jobs
await box.deleteTask('build')
await box.listTasks(); await box.getTask('build')
```

The Go client has no task methods yet; send the request with `net/http`.

## From inside the sandbox

The job itself can hold its sandbox awake, with no key, on the [API socket](inside.md):

```sh
curl -sS --unix-socket /run/loam/api.sock -X PUT http://loam/tasks/build -d '{"expire_s":600}'
# ... the work, refreshing the task every few minutes ...
curl -sS --unix-socket /run/loam/api.sock -X DELETE http://loam/tasks/build
```

```console
$ loam run scratch -- curl -sS --unix-socket /run/loam/api.sock -X PUT http://loam/tasks/inside -d '{"expire_s":60}' </dev/null
{"name":"inside","started_at":"2026-10-12T09:21:26.915260002Z","expires_at":"2026-10-12T09:22:26.915260002Z"}
```

Tasks live in the sandbox's memory: they survive a pause, and a stop or a restore ends them.
