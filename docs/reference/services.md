# Services

A service is a process loam keeps running for you. It starts at every boot of the sandbox, is
restarted when it crashes (with a growing pause between tries), and survives both kinds of sleep:
a pause keeps it running in the memory snapshot, and after a stop it starts again on the next
boot. Use services for what a sandbox should always be running: its web server, a database, a
worker. A command you start with exec is gone after a stop; a service isn't.

Service definitions are stored on the sandbox's disk (`/var/lib/loam/services/<name>.json`), so
they travel with checkpoints and forks. Output goes to rotating logs on the disk (8 MiB, two old
files kept).

| Method and path | |
|---|---|
| `GET /v1/sandboxes/{sandbox}/services` | → `{"services": [Service]}` |
| `PUT /v1/sandboxes/{sandbox}/services/{name}` | create or replace, and start → `Service` |
| `GET /v1/sandboxes/{sandbox}/services/{name}` | → `Service` |
| `DELETE /v1/sandboxes/{sandbox}/services/{name}` | stop it and remove it → `204` |
| `POST /v1/sandboxes/{sandbox}/services/{name}/{start\|stop\|restart}` | → `Service` |
| `GET /v1/sandboxes/{sandbox}/services/{name}/logs?follow=false&tail=N&wake=true` | `text/plain`, stdout and stderr interleaved. Following (`follow=true`) is not activity: it wakes the sandbox but doesn't keep it from going idle, and a pause ends it. `wake=false`: a sandbox that isn't running is 409 `conflict`, not woken (for a log shipper) |

## The definition

`PUT .../services/{name}`:

| Field | |
|---|---|
| `cmd` | argv; required |
| `cwd` | working directory |
| `env` | `{"K": "V"}` on top of the sandbox's env |
| `user` | default `loam` |
| `needs` | names of services to start first |
| `http_port` | the port this service serves; a request to that port's URL starts the service if it isn't running |

The answer, and `GET`, is the definition plus its state: `state` (`running`, `starting`,
`stopped`, `crashed`), `pid`, `restarts` and `started_at`. `stopped` is a service you stopped; it
stays stopped until you start it, a request reaches its `http_port`, or the next boot.

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -H 'Content-Type: application/json' -X PUT \
    "$LOAM_API_URL/v1/sandboxes/svc-demo/services/ticker" \
    -d '{"cmd":["sh","-c","while true; do date; sleep 5; done"],"env":{"TZ":"UTC"}}'
{"name":"ticker","cmd":["sh","-c","while true; do date; sleep 5; done"],"env":{"TZ":"UTC"},"state":"running","pid":296,"restarts":0,"started_at":"2026-10-12T09:18:51.448059501Z"}
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -H 'Content-Type: application/json' -X PUT \
    "$LOAM_API_URL/v1/sandboxes/svc-demo/services/after" -d '{"cmd":["sleep","infinity"],"needs":["ticker"],"user":"root"}'
{"name":"after","cmd":["sleep","infinity"],"user":"root","needs":["ticker"],"state":"running","pid":299,"restarts":0,"started_at":"2026-10-12T09:18:51.476035191Z"}
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -H 'Content-Type: application/json' -X PUT \
    "$LOAM_API_URL/v1/sandboxes/svc-demo/services/bad" -d '{"cmd":["true"],"needs":["nope"]}'
{"error":{"code":"invalid","message":"needs unknown service \"nope\""}}
```

With the CLI, flags come before `--` and the command after it:

```console
$ loam services put svc-demo web --http-port 8080 --cwd /home/loam/site -- python3 -m http.server 8080
name:      web
state:     running
cmd:       python3 -m http.server 8080
pid:       295
restarts:  0
started:   2026-10-12T09:18:51Z
cwd:       /home/loam/site
http_port: 8080
$ loam services ls svc-demo
NAME    STATE    PID  RESTARTS  HTTP_PORT  CMD
after   running  299  0                    sleep infinity
ticker  running  296  0                    sh -c while true; do date; sleep 5; done
web     running  295  0         8080       python3 -m http.server 8080
```

`loam services put` takes `--cwd`, `--env K=V`, `--user`, `--needs` (repeatable or
comma-separated) and `--http-port`.

## A service as the sandbox's site

A service with `http_port` N is started by a request to port N's URL when it isn't running, and
the sandbox's own URL goes to the sandbox's `http_port`. Make the two the same and the sandbox's
URL always answers, even after you stop the service or the sandbox:

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -X POST "$LOAM_API_URL/v1/sandboxes/svc-demo/services/web/stop"
{"name":"web","cmd":["python3","-m","http.server","8080"],"cwd":"/home/loam/site","http_port":8080,"state":"stopped","restarts":0}
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" https://svc-demo--docs.loam.example.com/
<h1>hello from loam</h1>
$ loam services get svc-demo web | grep state
state:     running
```

Access follows the port's setting either way ([Ports and URLs](ports.md#access-private-and-public)).

## Start, stop, restart, delete

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -X POST "$LOAM_API_URL/v1/sandboxes/svc-demo/services/ticker/restart"
$ loam services stop svc-demo ticker
$ loam services start svc-demo ticker
$ curl -sS -w '%{http_code}\n' -H "Authorization: Bearer $LOAM_TOKEN" -X DELETE "$LOAM_API_URL/v1/sandboxes/svc-demo/services/after"
204
$ loam services rm svc-demo ticker
```

Services come back after a stop, with new PIDs:

```console
$ loam stop svc-demo && loam wake svc-demo && loam services ls svc-demo
svc-demo	stopped
svc-demo	running
NAME    STATE    PID  RESTARTS  HTTP_PORT  CMD
after   running  292  0                    sleep infinity
ticker  running  291  0                    sh -c while true; do date; sleep 5; done
web     running  293  0         8080       python3 -m http.server 8080
```

A service that keeps failing ends up `crashed`:

```console
$ curl -sS ... -X PUT "$LOAM_API_URL/v1/sandboxes/svc-demo/services/crashy" -d '{"cmd":["sh","-c","echo boom; exit 1"]}'
$ loam services get svc-demo crashy
name:      crashy
state:     crashed
cmd:       sh -c echo boom; exit 1
restarts:  3
```

## Logs

`GET .../services/{name}/logs` answers the service's output as text, stdout and stderr together.
`tail=N` gives the last N lines; `follow=true` keeps the answer open and streams new output.

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" "$LOAM_API_URL/v1/sandboxes/svc-demo/services/ticker/logs?tail=2"
Mon Oct 12 09:18:51 UTC 2026
$ loam services logs svc-demo ticker -f
```

Programs that buffer their output when it isn't a terminal (Python, for one) show nothing until the
buffer fills; run them unbuffered (`python3 -u`, `PYTHONUNBUFFERED=1`).

## Go and TypeScript

```go
svc, err := c.PutService(ctx, "svc-demo", "web", api.ServiceSpec{
	Cmd: []string{"python3", "-m", "http.server", "8080"}, Cwd: "/home/loam/site", HTTPPort: 8080,
})
logs, err := c.ServiceLogs(ctx, "svc-demo", "web", client.LogOptions{Tail: 100}) // an io.ReadCloser
```

```ts
const box = loam.sandbox('svc-demo')
await box.putService('web', { cmd: ['python3', '-m', 'http.server', '8080'], cwd: '/home/loam/site', http_port: 8080 })
await box.restartService('web')   // also startService, stopService, deleteService, listServices, getService
console.log(await new Response(await box.serviceLogs('web', { tail: 100 })).text())
```
