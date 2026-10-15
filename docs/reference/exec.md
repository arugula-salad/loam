# Exec

Three ways to run a command in a sandbox:

- **One-shot** (`POST .../exec/run`): plain JSON, runs to completion, answers with the exit code
  and the output. The simplest; good for short commands.
- **Streaming** (`GET .../exec`, a WebSocket): output as it is written, stdin, a TTY, signals.
- **Sessions**: every streaming command is a session you can list, signal and, if it was started
  detachable, attach to again later with its output replayed.

Commands run as the user `loam` (uid 1000, passwordless `sudo`, home `/home/loam`) unless you
give `user` (`root`, or any account on the sandbox's disk). Each gets the sandbox's `env`, then
the call's `env` on top, and `HOME`, `USER`, `LOGNAME` and `SHELL` for its user. The command is
argv, not a shell line: use `["sh", "-c", "..."]` for pipes and variables.

| Method and path | |
|---|---|
| `POST /v1/sandboxes/{sandbox}/exec/run` | one-shot |
| `GET /v1/sandboxes/{sandbox}/exec` | streaming, WebSocket |
| `GET /v1/sandboxes/{sandbox}/exec/sessions` | list sessions |
| `GET /v1/sandboxes/{sandbox}/exec/sessions/{session}` | attach, WebSocket |
| `POST /v1/sandboxes/{sandbox}/exec/sessions/{session}/signal` | signal a session |

## One-shot

`POST /v1/sandboxes/{sandbox}/exec/run`

| Field | |
|---|---|
| `cmd` | argv; required |
| `stdin` | base64; given to the command, then EOF |
| `cwd` | working directory; default the user's home |
| `env` | `{"K": "V"}` on top of the sandbox's |
| `user` | default `loam` |
| `timeout_s` | default 60, at most 3600; the command is killed after it |

Answer: `{"exit_code", "stdout", "stderr", "timed_out"}`, with stdout and stderr base64. Output
beyond 16 MiB per stream is cut, and the answer then has `"truncated": true`. A command killed by
a signal has `exit_code` 128 + the signal (137 for `KILL`).

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -H 'Content-Type: application/json' \
    "$LOAM_API_URL/v1/sandboxes/scratch/exec/run" \
    -d "{\"cmd\":[\"sh\",\"-c\",\"wc -c; echo to-stderr >&2; exit 2\"],\"stdin\":\"$(printf hello | base64)\",\"cwd\":\"/tmp\",\"env\":{\"X\":\"1\"},\"timeout_s\":30}"
{"exit_code":2,"stdout":"NQo=","stderr":"dG8tc3RkZXJyCg==","timed_out":false}
```

`jq` decodes the output:

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -H 'Content-Type: application/json' \
    "$LOAM_API_URL/v1/sandboxes/scratch/exec/run" -d '{"cmd":["sh","-c","echo hi; echo err >&2"]}' \
    | jq -r '.stdout, .stderr | @base64d'
hi

err

$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -H 'Content-Type: application/json' \
    "$LOAM_API_URL/v1/sandboxes/scratch/exec/run" -d '{"cmd":["sleep","10"],"timeout_s":1}'
{"exit_code":137,"stdout":"","stderr":"","timed_out":true}
```

Errors are about starting the command, not its exit code (a non-zero exit is a `200`):

```console
$ curl -sS ... -d '{"cmd":["nope"]}'
{"error":{"code":"invalid","message":"executable \"nope\" not found in PATH"}}
$ curl -sS ... -d '{"cmd":["id"],"user":"nobody2"}'
{"error":{"code":"invalid","message":"no user \"nobody2\" in this sandbox"}}
$ curl -sS ... -d '{"cmd":["true"],"timeout_s":4000}'
{"error":{"code":"invalid","message":"timeout_s must be between 1 and 3600"}}
```

The CLI's `run` is this route. It sends its own stdin when stdin isn't a terminal, so give
`</dev/null` in scripts; it prints stdout and stderr and exits with the command's code.

```console
$ echo hello | loam run scratch -- wc -c
6
$ loam run scratch --user root --cwd /etc --env A=1 --timeout 5m -- sh -c 'id -un; pwd' </dev/null
root
/etc
```

Go and TypeScript:

```go
res, err := c.Run(ctx, "scratch", api.ExecRun{Cmd: []string{"sh", "-c", "echo hello from $(hostname)"}})
// res.ExitCode, res.Stdout ([]byte, decoded), res.Stderr, res.TimedOut
```

```ts
const r = await loam.sandbox('scratch').run({ cmd: ['sh', '-c', 'wc -c'], stdin: 'hello', timeout_s: 30 })
r.exit_code          // 0
r.stdout.toString()  // "5\n": stdout and stderr are Buffers
```

## Streaming

`GET /v1/sandboxes/{sandbox}/exec`, upgraded to a WebSocket, with the `Authorization` header on
the upgrade request.

Query parameters:

| Parameter | |
|---|---|
| `cmd` | argv, one parameter per argument, in order (`?cmd=sh&cmd=-c&cmd=echo%20hi`); required |
| `tty` | `true` for a pseudo-terminal; default `false` |
| `cols`, `rows` | the TTY's first size |
| `cwd` | working directory |
| `env` | `K=V`, repeatable, on top of the sandbox's env |
| `user` | default `loam` |
| `detachable` | `true`: the session keeps running when the socket closes, and can be attached again |

### Frames

Binary frames carry bytes with a one-byte stream tag in front; text frames carry JSON control
messages.

| Direction | Frame | Meaning |
|---|---|---|
| client → server | binary, first byte `0`, then bytes | stdin |
| client → server | binary, the single byte `4` | stdin EOF |
| client → server | text `{"type":"resize","cols":N,"rows":N}` | the TTY's size |
| client → server | text `{"type":"signal","signal":"TERM"}` | a signal to the command's process group; names as in `kill -l`, without `SIG` |
| server → client | text `{"type":"started","session":"s-…","pid":N}` | always the first frame once the command runs |
| server → client | binary, first byte `1`, then bytes | stdout (with a TTY, all output) |
| server → client | binary, first byte `2`, then bytes | stderr (never with a TTY) |
| server → client | text `{"type":"exit","code":N}` | the last frame; `code` is 128 + signal when killed. The socket then closes normally (1000). |
| server → client | text `{"type":"error","error":{"code","message"}}` | the command could not start (instead of `started`); then the socket closes |

If the sandbox goes away under the session (paused, a memory checkpoint taken, deleted, restored
to a checkpoint, its node lost), the socket closes with status **1011** and no `exit` frame. Treat that as "exit unknown",
never as success. Every client below reports it as an error (`client.ErrExitUnknown` in Go,
`ExitUnknownError` in TypeScript, exit status 255 from the CLI).

A refused upgrade (no key, no such sandbox) is an ordinary HTTP error in the usual shape, before
any WebSocket:

```console
$ curl -sS -i --http1.1 -H 'Connection: Upgrade' -H 'Upgrade: websocket' -H 'Sec-WebSocket-Version: 13' \
    -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' "$LOAM_API_URL/v1/sandboxes/scratch/exec?cmd=id"
HTTP/1.1 401 Unauthorized
Content-Type: application/json

{"error":{"code":"unauthorized","message":"a valid API key is required"}}
```

### A raw client

curl can't speak WebSocket. This is the protocol by hand, in Node 22 or later with no packages: it
sends a line of stdin and EOF, and prints every frame it gets.

```js
// ws-exec.mjs
const base = process.env.LOAM_API_URL.replace(/^http/, 'ws')
const q = new URLSearchParams()
for (const a of ['sh', '-c', 'read name; echo "hello, $name"; echo oops >&2; exit 7']) q.append('cmd', a)
const ws = new WebSocket(`${base}/v1/sandboxes/scratch/exec?${q}`, {
  headers: { Authorization: `Bearer ${process.env.LOAM_TOKEN}` },
})
ws.binaryType = 'arraybuffer'
ws.onmessage = (ev) => {
  if (typeof ev.data === 'string') {
    console.log('text  ', ev.data)
    const msg = JSON.parse(ev.data)
    if (msg.type === 'started') {
      ws.send(new Uint8Array([0, ...new TextEncoder().encode('loam\n')])) // stdin
      ws.send(new Uint8Array([4]))                                       // stdin EOF
    }
    return
  }
  const b = new Uint8Array(ev.data)
  console.log('binary', b[0] === 1 ? 'stdout' : 'stderr', JSON.stringify(new TextDecoder().decode(b.subarray(1))))
}
ws.onclose = (ev) => console.log('close ', ev.code)
```

```console
$ node ws-exec.mjs
text   {"type":"started","session":"s-5u6pz7c26b","pid":295}
binary stdout "hello, loam\n"
binary stderr "oops\n"
text   {"type":"exit","code":7}
close  1000
```

The same client with other parameters shows the other frames:

| Query | Frames |
|---|---|
| `cmd=no-such-command` | `{"type":"error","error":{"code":"invalid","message":"executable \"no-such-command\" not found in PATH"}}`, close 1000 |
| `cmd=sleep&cmd=30`, then `{"type":"signal","signal":"TERM"}` | `started`, `{"type":"exit","code":143}` |
| `cmd=sh&cmd=-c&cmd=stty size; tty; echo err >&2&tty=true&cols=100&rows=30` | `started`, stdout `"30 100\r\n"`, `"/dev/pts/0\r\n"`, `"err\r\n"` (all on stream 1), `exit` 0 |
| `cmd=id&user=root&cwd=/etc&env=A=1` | stdout `"uid=0(root) gid=0(root) groups=0(root)\n"` |

### The CLI

`loam exec` streams; `loam console` is `exec -t` of a login shell, for interactive use.

```console
$ loam exec scratch -- sh -c 'echo out; echo err >&2; exit 3' </dev/null; echo "exit=$?"
err
out
exit=3
$ loam exec -t scratch -- bash                  # a TTY, sized and resized with your terminal
$ loam exec -d scratch -- python3 -m http.server 8080 </dev/null   # detached: prints the session ID
s-rhgpddrfzk
$ loam console scratch --user root
```

| Flag | |
|---|---|
| `-t`, `--tty` | allocate a TTY (raw mode on your terminal) |
| `-i`, `--stdin` | forward stdin even when it is a terminal (otherwise a terminal on stdin gets EOF) |
| `-d`, `--detach` | start a detachable session, print its ID, and return |
| `--detachable` | the session outlives this connection; with a TTY, `Ctrl-]` detaches |
| `--cwd`, `--env K=V`, `--user` | as the query parameters |

Without a TTY the CLI passes `INT`, `TERM`, `HUP` and `QUIT` on to the remote process group, so
Ctrl-C stops the remote command. It forwards stdin whenever stdin isn't a terminal; give
`</dev/null` in scripts.

### Go and TypeScript

```go
s, err := c.Exec(ctx, "scratch", client.ExecOptions{
	Cmd: []string{"sh", "-c", "for i in 1 2 3; do echo tick $i; sleep 1; done; exit 4"},
	IO:  client.IO{Stdout: os.Stdout, Stderr: os.Stderr}, // Stdin: an io.Reader, copied then closed
})
if err != nil {
	log.Fatal(err)
}
code, err := s.Wait() // 4, nil; client.ErrExitUnknown if the sandbox went away under it
```

`Stream` also has `Write` (stdin), `CloseStdin`, `Resize`, `Signal` and `Close`.

```ts
const p = await loam.sandbox('scratch').exec({
  cmd: ['sh', '-c', 'echo out; echo err >&2; exit 3'],
  stdout: process.stdout,   // a function, or anything with write()
  stderr: process.stderr,
})
await p.wait()              // 3; rejects with ExitUnknownError on a 1011 close
// also: p.write(data), p.closeStdin(), p.resize(cols, rows), p.signal('INT'), p.session
```

## Sessions

Every streaming command is a session, `s-` and 10 characters. A session that isn't detachable
ends (`SIGHUP` to its process group) when its socket closes. A detachable one keeps running with
nobody attached. Either way, an exited session stays listed for 10 minutes, with its output.
Pausing the sandbox closes every session's socket with 1011, but the sessions are in the memory
snapshot: a detachable one can be attached again after the wake. A memory checkpoint closes every
session's socket with 1011 too; the ones that aren't detachable end with their sockets, and a
detachable one runs on and can be attached again at once. A stop or a restore to a disk-only
checkpoint ends them all.

Each session keeps the last 1 MiB of its output (stdout and stderr together, in the order written)
for replay.

### List sessions

`GET /v1/sandboxes/{sandbox}/exec/sessions` → `{"sessions": [Session]}`; a session is
`{"id", "cmd", "tty", "pid", "started_at", "exited", "exit_code"?, "attached"}`.

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" "$LOAM_API_URL/v1/sandboxes/scratch/exec/sessions" | jq -c '.sessions[]'
{"id":"s-5u6pz7c26b","cmd":["sh","-c","read name; echo \"hello, $name\"; echo oops >&2; exit 7"],"tty":false,"pid":295,"started_at":"2026-10-12T09:16:36.951557736Z","exited":true,"exit_code":7,"attached":false}
{"id":"s-p6mjlxcp7w","cmd":["sh","-c","for i in 1 2 3; do echo tick $i; sleep 1; done; sleep 600"],"tty":false,"pid":307,"started_at":"2026-10-12T09:17:05.497372569Z","exited":false,"attached":false}
$ loam sessions scratch
ID            PID  TTY  ATTACHED  STATUS    AGE  CMD
s-bj4vew4m37  303  no   no        exited 3  3m   sh -c echo out; echo err >&2; exit 3
s-rhgpddrfzk  309  no   no        running   1s   python3 -m http.server 8080 --directory /home/loam/site
```

### Attach

`GET /v1/sandboxes/{sandbox}/exec/sessions/{session}`, a WebSocket with the same frames as exec.
The first frame is `{"type":"attached","session":"s-…","offset":N}` instead of `started`; then the
replay, then live output, then `exit` (at once, for a session that has already exited). `offset`
is always there, 0 included.

`?offset=N` replays from byte N of the session's output (counting stdout and stderr together);
the default replays everything kept. An offset older than what is kept starts at the oldest kept
byte, and `offset` in the first frame says where the replay really starts. A plain `GET` without
the upgrade is `400 invalid`.

```console
$ loam exec -d scratch -- sh -c 'for i in 1 2 3; do echo tick $i; sleep 1; done; sleep 600' </dev/null
s-p6mjlxcp7w
$ loam attach scratch s-p6mjlxcp7w --offset 7
tick 2
tick 3
```

(`tick 1\n` is the first 7 bytes.) Closing an attach leaves a detachable session running, but
remember that the CLI passes Ctrl-C on to a non-TTY session: interrupting `loam attach` stops the
command.

```go
s, err := c.Attach(ctx, "scratch", "s-p6mjlxcp7w", client.AttachOptions{IO: client.IO{Stdout: os.Stdout}})
```

```ts
const again = await loam.sandbox('scratch').attach('s-p6mjlxcp7w', { offset: 0, stdout: process.stdout })
```

### Signal a session

`POST /v1/sandboxes/{sandbox}/exec/sessions/{session}/signal` with `{"signal": "TERM"}` (names as
in `kill -l`), to the session's process group → `204`. `409 conflict` once it has exited,
`404 not_found` for a session the sandbox doesn't have.

```console
$ curl -sS -i -H "Authorization: Bearer $LOAM_TOKEN" -H 'Content-Type: application/json' \
    "$LOAM_API_URL/v1/sandboxes/scratch/exec/sessions/s-krbiocuwm6/signal" -d '{"signal":"KILL"}'
HTTP/2 204
$ loam kill scratch s-krbiocuwm6 --signal INT     # default TERM
$ curl -sS ... "$LOAM_API_URL/v1/sandboxes/scratch/exec/sessions/s-p6mjlxcp7w/signal" -d '{"signal":"KILL"}'
{"error":{"code":"conflict","message":"the session has exited"}}
```
