# API reference

loam's API is plain HTTP and JSON, with WebSockets for interactive streams and Server-Sent Events
for watches. Everything the CLI and the clients do goes through it. This page covers what every
route has in common; the pages below take one area each.

| Page | What it covers |
|---|---|
| [Sandboxes](sandboxes.md) | create, list, change, pause, stop, wake, delete; images |
| [Exec](exec.md) | running commands: one-shot, streaming over a WebSocket, sessions |
| [Files](files.md) | reading, writing, listing, tars, watching |
| [Ports and URLs](ports.md) | listening ports, port URLs and who may use them, raw TCP |
| [Stats](stats.md) | how much your app is used: requests, statuses and visitors per day |
| [Services](services.md) | supervised processes that start at boot |
| [Tasks](tasks.md) | holding a sandbox awake |
| [Checkpoints](checkpoints.md) | snapshots, restore, forks |
| [Network](network.md) | egress rules |
| [Secrets](secrets.md) | credentials a sandbox uses without holding them |
| [Identity](identity.md) | OpenID Connect tokens that prove which sandbox is calling |
| [Events](events.md) | a stream of what happens to your sandboxes |
| [Templates](templates.md) | what a sandbox can be made from |
| [Inside a sandbox](inside.md) | the API socket processes in a sandbox use |
| [E2B](e2b.md) | using E2B's SDKs against loam |

New to loam? Start with the [primer](../primer.md).

## Base URL

```
https://api.loam.example.com
```

Every route below is relative to it, and versioned under `/v1`. The CLI and the clients read it
from `LOAM_API_URL`. The examples assume:

```sh
export LOAM_API_URL=https://api.loam.example.com
export LOAM_TOKEN=loam_...        # your project key
```

## Authentication

Every request carries the key as a bearer token:

```sh
curl -H "Authorization: Bearer $LOAM_TOKEN" "$LOAM_API_URL/v1/whoami"
```

```json
{"project":{"name":"docs","domains":[],"created_at":"2026-10-12T09:09:01.831156Z"},
 "key":{"id":"key-mgnnkl5dvw","name":"docs-writer","project":"docs","created_at":"2026-10-12T09:09:01.914433Z"}}
```

| Route | Answer |
|---|---|
| `GET /v1/whoami` | `{"project": Project, "key": Key}`: the key's project (with its URL `domains`) and the key itself, never its secret |

A key belongs to one **project**. Everything it creates belongs to that project, and it never sees
another project's sandboxes or secrets: they answer `404`, exactly as if they didn't exist. Keys
look like `loam_` and 32 base32 characters; loam stores only their hash, so a lost key can't be
shown again, only replaced.

```console
$ loam whoami
project docs, key docs-writer (key-mgnnkl5dvw)
```

Projects and keys are made by the deployment's operator with the admin token, not with a project key. For
completeness, those routes are `POST /v1/projects`, `GET /v1/projects`,
`DELETE /v1/projects/{project}`, `POST /v1/projects/{project}/keys`,
`GET /v1/projects/{project}/keys` and `DELETE /v1/projects/{project}/keys/{key}`; a project key
gets `401` from them.

## Requests and answers

- Bodies are JSON (`Content-Type: application/json`) unless a route says otherwise (file contents
  and tars are raw bytes). An unknown field in a body is an error, not ignored:
  `{"nmae": "x"}` is `400 invalid` with `bad request body: json: unknown field "nmae"`.
- Answers are JSON, and lists come wrapped: `{"sandboxes": [...]}`, `{"sessions": [...]}`.
- Times are RFC 3339 in UTC (`2026-10-12T09:11:46.438209Z`).
- `{sandbox}` in a path is a sandbox's ID (`sb-2kjiso7vff5b`) or its name (`scratch`). Names are
  unique in a project, so both always mean one sandbox.
- Any route under `/v1/sandboxes/{sandbox}/` wakes a sleeping sandbox before it runs, and holds it
  awake until it ends, except `pause`, `stop`, and the routes that only read or change loam's
  records (listing, reading and deleting checkpoints, port settings). So you never have to wake a
  sandbox yourself.

## Errors

Every error, from every route and from a refused WebSocket upgrade, has the same shape:

```json
{"error": {"code": "not_found", "message": "sandbox not found"}}
```

`code` is for programs, `message` for people. The codes:

| Code | Status | Meaning |
|---|---|---|
| `invalid` | 400 | The request is wrong: a bad name, a missing field, a value out of range. The message says what. |
| `unauthorized` | 401 | No key, or not a valid one. Also a private port URL without the key. |
| `permission_denied` | 403 | A file operation the given `user` may not do; a connection the sandbox's egress rules refuse (on port 80). |
| `not_found` | 404 | No such sandbox, checkpoint, file, session, task, service or secret in your project. |
| `already_exists` | 409 | The name is taken (a sandbox name in the project). |
| `conflict` | 409 | The thing's state forbids the call: deleting a protected sandbox, deleting a secret a sandbox uses, signalling a session that has exited, deleting a directory that isn't empty without `recursive=true`. |
| `unsupported` | 501 | A feature this deployment can't enforce, refused rather than ignored. |
| `port_closed` | 502 | Nothing listens on the port (after waiting up to 10 s, for a port URL). |
| `broker_failed` | 502 | From inside a sandbox only: a brokered request whose answer would have carried a secret's value ([Secrets](secrets.md)). |
| `capacity` | 503 | No node has room for the sandbox now. Carries `Retry-After` (seconds): wait that long and try again. |
| `sandbox_unavailable` | 503 | The sandbox could not be woken. |

```console
$ curl -sS -i "$LOAM_API_URL/v1/sandboxes"
HTTP/2 401
content-type: application/json

{"error":{"code":"unauthorized","message":"a valid API key is required"}}
```

```console
$ curl -sS -i -H "Authorization: Bearer $LOAM_TOKEN" -H 'Content-Type: application/json' \
    "$LOAM_API_URL/v1/sandboxes" -d '{"resources":{"memory_mib":200000}}'
HTTP/2 503
retry-after: 30

{"error":{"code":"capacity","message":"no node has room for the sandbox now"}}
```

In Go, `client.ErrorCode(err)` gives the code and `client.IsNotFound(err)` tests for `not_found`;
`*client.Error` has the status, message and `RetryAfter`. In TypeScript every error is a
`LoamError` with `status`, `code` and `message`, and `isNotFound(err)` and `err.isCode(...)` test
it.

## Streams

Three kinds of route stream rather than answer once:

- **WebSockets** for interactive, two-way streams: exec and attach ([Exec](exec.md)), file
  watches ([Files](files.md)), raw TCP to a port ([Ports](ports.md)). Authenticate the upgrade
  request with the same `Authorization` header. A refused upgrade answers with an ordinary HTTP
  error in the shape above, before any WebSocket exists. If the sandbox goes away under a stream
  (deleted, paused, restored), the socket closes with status `1011`.
- **Server-Sent Events** (`text/event-stream`) for watches: port listeners
  ([Ports](ports.md#watch-listeners)) and the project's events ([Events](events.md)). Each event is
  an `event: <type>` line and a `data: <JSON>` line. `curl -N` shows them as they come.
- **Plain chunked bodies** for big or long things: file downloads, tars, `follow`ed service logs.

`curl` can't speak WebSocket, so the WebSocket examples use the CLI, the clients, or a short Node
program (Node 22 and later have a `WebSocket` that takes headers).

## Clients

| | |
|---|---|
| CLI | `loam`, in `cmd/loam`. `go install github.com/arugula-salad/loam/cmd/loam@latest` (see the [primer](../primer.md#before-you-start)). |
| Go | `github.com/arugula-salad/loam/pkg/client`, with the body types in `pkg/api`. `client.FromEnv()` reads `LOAM_API_URL` and `LOAM_TOKEN`. |
| TypeScript | `@arugula-salad/loam` on npm, or the single file `loam.mjs` from the `ts-v*` releases. Node 20 or later. `LoamClient.fromEnv()`. |
| E2B SDKs | Python and JavaScript, unmodified: [E2B](e2b.md). |
