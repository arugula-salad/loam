# Ports and URLs

Every port in a sandbox has an HTTPS URL, and the sandbox itself has one that goes to its
`http_port`. URLs are private (they want your key) until you make them public. You can also see
what listens, watch listeners come and go, and open a raw TCP stream to any port.

| Method and path | |
|---|---|
| `GET /v1/sandboxes/{sandbox}/ports` | what listens → `{"ports": [Port]}` |
| `GET /v1/sandboxes/{sandbox}/ports/watch` | listeners opening and closing, Server-Sent Events |
| `GET /v1/sandboxes/{sandbox}/ports/{port}/connect` | a raw TCP stream to the port, WebSocket |
| `PUT /v1/sandboxes/{sandbox}/ports/{port}` | `{"access": "private"\|"public"}` → `{"port", "access", "url"}` |
| `GET /v1/sandboxes/{sandbox}/ports/settings` | → `{"ports": [{"port", "access", "url"}]}` |

## URLs

| URL | Goes to |
|---|---|
| `https://<name>--<project>.loam.example.com` | the sandbox's `http_port` (8080 unless you set it) |
| `https://<port>--<name>--<project>.loam.example.com` | that port |

So the sandbox `scratch` in the project `docs` is `https://scratch--docs.loam.example.com`, and
its port 3000 is `https://3000--scratch--docs.loam.example.com`. They carry plain HTTP and
WebSockets, over TLS that loam terminates; the server in the sandbox speaks plain HTTP. A server
listening only on `127.0.0.1` is reachable too.

- **Waking.** A request to a sleeping sandbox wakes it.
- **Nothing listening.** loam waits up to 10 seconds for something to listen on the port, then
  answers `502 port_closed`:

  ```console
  $ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" https://3000--scratch--docs.loam.example.com/
  {"error":{"code":"port_closed","message":"nothing listens on port 3000 in the sandbox"}}
  ```

- **Services.** A request to the port of a [service](services.md) with that `http_port` starts the
  service if it isn't running.

`loam url` prints them:

```console
$ loam url scratch
https://scratch--docs.loam.example.com
$ loam url scratch --port 3000
https://3000--scratch--docs.loam.example.com
```

In TypeScript, `sb.url`, `sb.urls`, and `portURL(sb, 3000)` / `portURLs(sb, 3000)` build them.

### Access: private and public

Every port is **private** by default: its URL needs the project's key, either as
`Authorization: Bearer <key>` or as a `loam_token` cookie. Opening a URL once with
`?loam_token=<key>` sets that cookie for the host (`HttpOnly`, `Secure`, `SameSite=Lax`) and
redirects to the same URL without the key, which is how you open a private URL in a browser.

```console
$ curl -sS https://scratch--docs.loam.example.com/
{"error":{"code":"unauthorized","message":"this port is private: send the project's key as a bearer token, or visit with ?loam_token=<key>"}}
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" https://scratch--docs.loam.example.com/
<h1>hello from loam</h1>
$ curl -sS -i "https://scratch--docs.loam.example.com/?loam_token=$LOAM_TOKEN"
HTTP/2 302
location: /
set-cookie: loam_token=<key>; Path=/; HttpOnly; Secure; SameSite=Lax
```

**Public** opens a port to anyone. The sandbox's own URL follows the setting of its `http_port`.

`PUT /v1/sandboxes/{sandbox}/ports/{port}` sets it:

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -H 'Content-Type: application/json' -X PUT \
    "$LOAM_API_URL/v1/sandboxes/scratch/ports/3000" -d '{"access":"public"}'
{"port":3000,"access":"public","url":"https://3000--scratch--docs.loam.example.com"}
$ loam url scratch --public                    # the http_port
$ loam url scratch --port 3000 --private
```

`GET /v1/sandboxes/{sandbox}/ports/settings` lists the ports whose access was set (every other
port is private). Neither route wakes the sandbox, and a port needn't be listening to be set.

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" "$LOAM_API_URL/v1/sandboxes/scratch/ports/settings"
{"ports":[{"port":3000,"access":"public","url":"https://3000--scratch--docs.loam.example.com"},{"port":8080,"access":"public","url":"https://8080--scratch--docs.loam.example.com"}]}
```

```ts
const { url } = await loam.sandbox('scratch').setPortAccess(3000, 'public')
await loam.sandbox('scratch').portSettings()
```

### URL domains

The operator can give a project domains of its own (say `games.example.com`, with its DNS and
certificate set up for it). Then a sandbox named N in that project is also `https://N.<domain>`
(its `http_port`) and `https://<port>--N.<domain>`, with the same access rules; the `loam_token`
cookie is set per host. The sandbox's `urls` lists them, and `loam whoami` and
`GET /v1/whoami` show the project's `domains`. Every host under such a domain is a sandbox's: the
domain itself, or a name there that is no sandbox of the project, is `404 not_found`.

## List what listens

`GET /v1/sandboxes/{sandbox}/ports` → `{"ports": [{"port", "address", "pid", "session"?}]}`, every
TCP listener on every address, with the session that started it when there is one.

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" "$LOAM_API_URL/v1/sandboxes/scratch/ports"
{"ports":[{"port":8080,"address":"0.0.0.0","pid":314,"session":"s-2dh5hyezuv"}]}
$ loam ports scratch
PORT  ADDRESS  PID  SESSION
8080  0.0.0.0  309  s-rhgpddrfzk
```

## Watch listeners

`GET /v1/sandboxes/{sandbox}/ports/watch`, Server-Sent Events: first an `opened` event for every
port already listening, then `opened` and `closed` as they change. Listeners are found by polling
every half second. Watching is not activity: it doesn't keep the sandbox from going idle.

```console
$ curl -sS -N -H "Authorization: Bearer $LOAM_TOKEN" "$LOAM_API_URL/v1/sandboxes/scratch/ports/watch"
event: opened
data: {"port":8080,"address":"0.0.0.0","pid":314,"session":"s-2dh5hyezuv"}

event: opened
data: {"port":3000,"address":"0.0.0.0","pid":327,"session":"s-lhg4wr5syu"}

event: closed
data: {"port":3000,"address":"0.0.0.0","pid":327,"session":"s-lhg4wr5syu"}
```

`loam ports scratch --watch` prints the same; `box.watchPorts()` in TypeScript and `c.WatchPorts`
in Go give the events.

## Connect to a port

`GET /v1/sandboxes/{sandbox}/ports/{port}/connect`, a WebSocket carrying a raw TCP stream to
`127.0.0.1:{port}` in the sandbox, in binary frames both ways. It works for any protocol (a
database, SSH, a debugger) and in a sandbox with any egress rules, since it doesn't use the
sandbox's network. Nothing listening is `502 port_closed`, before the upgrade:

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" --http1.1 -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
    -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
    "$LOAM_API_URL/v1/sandboxes/scratch/ports/3000/connect"
{"error":{"code":"port_closed","message":"nothing listens on port 3000"}}
```

The CLI's `port-forward` listens locally and connects each connection through this route:

```console
$ loam port-forward scratch 18080:8080 &
forwarding 127.0.0.1:18080 -> scratch:8080
$ curl -sS http://127.0.0.1:18080/
<h1>hello from loam</h1>
```

In Go, `c.ConnectPort(ctx, "scratch", 5432)` returns a `net.Conn`; in TypeScript,
`box.connectPort(5432)` a Node `Duplex`.

A pause, a memory checkpoint, a stop, a restore or a delete ends every port stream (status 1011).
