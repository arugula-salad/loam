# Secrets

A project secret is a credential your sandboxes use without holding it: an API key, a token, a
password. The sandbox's environment has a **placeholder** where the value would be. When a request
leaves the sandbox for one of the secret's hosts, over HTTPS, the sandbox's node puts the real
value in on the way out. The value never reaches the sandbox's memory, disk, snapshots or logs, so
code in the sandbox (yours, a dependency's, an agent's) can use the credential but can't read it,
print it or send it anywhere else.

```json
{"name": "ECHO_PASSWORD", "hosts": ["postman-echo.com/basic-auth"],
 "placeholder": "__loam_echo_password__", "inject": null,
 "created_at": "2026-10-12T09:20:56.703666Z", "updated_at": "2026-10-12T09:20:56.703666Z"}
```

| Method and path | Body → answer |
|---|---|
| `PUT /v1/secrets/{name}` | `{"value", "hosts", "inject"?, "placeholder_prefix"?}` → `Secret`: creates or replaces it |
| `GET /v1/secrets` | → `{"secrets": [Secret]}` |
| `GET /v1/secrets/{name}` | → `Secret`, never the value |
| `DELETE /v1/secrets/{name}` | → `204`; `409 conflict` while a sandbox uses it |

Secrets belong to the project; a sandbox uses one by naming it in its `secrets`
([Sandboxes](sandboxes.md)).

## The fields

| Field | |
|---|---|
| `name` | An environment variable name, `[A-Z_][A-Z0-9_]*`, at most 64 characters. It is the variable the placeholder appears in. A sandbox can't also have it in its `env`. |
| `value` | 1 byte to 8 KiB, without CR, LF or NUL. Write-only: nothing ever shows it again. |
| `hosts` | At least one: where the value may go, over HTTPS (port 443) only. `api.github.com`, `*.github.com`, or either with a path prefix (`api.github.com/repos/`). |
| `placeholder` | Made by loam: `__loam_<name in lowercase>__`, after `placeholder_prefix` when you give one. |
| `placeholder_prefix` | For clients that check a token's shape before sending it (`sk-ant-oat01-`): at most 64 printable ASCII characters, no spaces. |
| `inject` | `null` (the default): the value replaces the placeholder wherever it appears in the request head. Or `{"header": "x-api-key", "format": "{value}"}`: set that header on every request to the hosts. See below. |

Replacing a secret's value reaches awake sandboxes at once.

## How the value gets in

With `inject: null`, the node replaces the placeholder byte for byte wherever it appears in the
request **head**: the path, the query, every header value, and inside an
`Authorization: Basic` credential (decoded, replaced, encoded again). So code that builds its
request from the environment as usual just works: `Authorization: Bearer $GITHUB_TOKEN`,
`curl -u user:$PASSWORD`, `?api_key=$KEY`.

With `inject: {"header": H, "format": F}`, every request to the hosts gets header `H` set to `F`
with `{value}` replaced (`"Bearer {value}"`), replacing any `H` the sandbox sent; the placeholder
isn't needed anywhere. `H` can't be a header that frames or routes the request (`Host`,
`Content-Length`, `Transfer-Encoding`, `Connection`, `Upgrade`, …).

Bodies and WebSocket messages are never changed, so a placeholder in a JSON body goes out as it is.

To do this, TLS to a secret's hosts ends at the node, with a certificate from loam's broker CA,
which every sandbox trusts: the agent installs it in the system bundles and sets
`NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, `CARGO_HTTP_CAINFO` and
`UV_NATIVE_TLS=1` for every process. The node checks the real host's certificate itself. A client
that pins certificates, or brings its own CA list and ignores those variables, won't trust the
node. Every other connection, TLS or not, is never decrypted.

The broker root is also available at E2B's path,
`/usr/local/share/ca-certificates/e2b-ca.crt`, refreshed on every wake even without secrets.
Applications that launch Node themselves can set `NODE_EXTRA_CA_CERTS` to that path;
Node reads extra roots at process startup. Rebuild older memory-snapshot templates to
pick up the agent that installs this path.

## A worked example

postman-echo.com's `/basic-auth` answers `{"authenticated":true}` to the user `postman` with the
password `password`, so it shows brokering with a value that's harmless to show.

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -H 'Content-Type: application/json' -X PUT \
    "$LOAM_API_URL/v1/secrets/ECHO_PASSWORD" -d '{"value":"password","hosts":["postman-echo.com/basic-auth"]}'
{"name":"ECHO_PASSWORD","hosts":["postman-echo.com/basic-auth"],"placeholder":"__loam_echo_password__","inject":null,"created_at":"2026-10-12T09:20:56.703666Z","updated_at":"2026-10-12T09:20:56.703666Z"}
$ loam create secret-demo --secret ECHO_PASSWORD --json | jq -c '{name,secrets}'
{"name":"secret-demo","secrets":["ECHO_PASSWORD"]}
$ loam run secret-demo -- sh -c 'env | grep ECHO_' </dev/null
ECHO_PASSWORD=__loam_echo_password__
$ loam run secret-demo -- sh -c 'curl -sS -u "postman:$ECHO_PASSWORD" https://postman-echo.com/basic-auth' </dev/null
{"authenticated":true}
```

To a host the secret doesn't name, the placeholder goes out as it is:

```console
$ loam run secret-demo -- sh -c 'curl -sS -H "X-Token: $ECHO_PASSWORD" https://httpbin.org/headers | grep X-Token' </dev/null
    "X-Token": "__loam_echo_password__"
```

The CLI reads the value from stdin, so it stays out of your shell history (`--value` exists, but
then it's in the history):

```console
$ printf 'demo-key-123' | loam secret put ECHO_KEY --host postman-echo.com --header x-api-key
name:        ECHO_KEY
placeholder: __loam_echo_key__
hosts:       postman-echo.com
inject:      x-api-key: {value}
created:     2026-10-12T09:20:56Z
updated:     2026-10-12T09:20:56Z
$ loam secret put PREFIXED --host postman-echo.com --placeholder-prefix sk-demo- --value abc --json | jq -r .placeholder
sk-demo-__loam_prefixed__
$ loam secret ls
NAME           PLACEHOLDER                HOSTS                        HEADER     UPDATED
ECHO_KEY       __loam_echo_key__          postman-echo.com             x-api-key  0s
ECHO_PASSWORD  __loam_echo_password__     postman-echo.com/basic-auth  -          0s
PREFIXED       sk-demo-__loam_prefixed__  postman-echo.com             -          0s
$ loam secret get ECHO_KEY
$ loam secret rm PREFIXED
```

`--header H` with `--format F` (default `{value}`) is `inject`. Add and remove secrets on a
sandbox with `loam update --secret NAME` and `--unset-secret NAME`; a new placeholder reaches
processes started after the change.

## What brokering can't do

- **Carry the value back.** An answer whose headers or body hold the value (as is, or with `/`
  written `\/`) is cut off: a `502` if its head hasn't been sent yet, else a reset. So an echo
  service, which repeats your request, can't be used with a secret:

  ```console
  $ loam run secret-demo -- curl -sS https://postman-echo.com/headers </dev/null    # with ECHO_KEY in use
  {"error":{"code":"broker_failed","message":"the answer holds a secret's value"}}
  ```

  To read the body, the node asks the host for an uncompressed answer
  (`Accept-Encoding: identity`); an answer compressed anyway is a `502` too.
- **Follow redirects for you.** The sandbox sees the redirect, and its next request is matched
  afresh: a redirect to a host the secret doesn't name gets the placeholder.
- **Go anywhere but HTTPS on 443 to the named hosts.** Plain HTTP, other ports and other hosts get
  the placeholder.
- **Change bodies or WebSocket messages.**
- **Go out without the value.** A request that needs a secret the node can't get is a `502`, never
  sent with the placeholder in its place. The node's own `502`s carry the code `broker_failed`. A
  request on a brokered connection whose `Host` isn't the connection's TLS server name is `421`.

In a restricted sandbox ([Network](network.md)), a secret's hosts are allowed on port 443 as if by
a rule.

## Errors

```console
$ curl -sS ... -X PUT "$LOAM_API_URL/v1/secrets/bad-name" -d '{"value":"x","hosts":["a.com"]}'
{"error":{"code":"invalid","message":"a secret's name is an environment variable name, [A-Z_][A-Z0-9_]*, at most 64 characters"}}
$ curl -sS ... -X PUT "$LOAM_API_URL/v1/secrets/X1" -d '{"value":"x","hosts":["a.com"],"inject":{"header":"Host","format":"{value}"}}'
{"error":{"code":"invalid","message":"inject.header \"Host\" frames or routes the request, which a secret can't set"}}
$ curl -sS ... -X DELETE "$LOAM_API_URL/v1/secrets/ECHO_KEY"
{"error":{"code":"conflict","message":"a sandbox names the secret; take it out of the sandbox's secrets first"}}
$ curl -sS ... -X PATCH "$LOAM_API_URL/v1/sandboxes/secret-demo" -d '{"env":{"ECHO_KEY":"x"}}'
{"error":{"code":"invalid","message":"secrets: \"ECHO_KEY\" is also in env; the secret's placeholder goes in the env under its name"}}
```

A deployment without a secret key answers every secret route, and any sandbox that names secrets,
`501 unsupported`; a sandbox with secrets needs a node with the broker CA, and is `503 capacity`
when none has room.

## Go and TypeScript

```go
_, err := c.PutSecret(ctx, "ANTHROPIC_API_KEY", api.PutSecret{
	Value: os.Getenv("ANTHROPIC_API_KEY"), Hosts: []string{"api.anthropic.com"},
	Inject: &api.Inject{Header: "x-api-key", Format: "{value}"},
})
```

```ts
await loam.putSecret('GITHUB_TOKEN', { value: process.env.GITHUB_TOKEN!, hosts: ['api.github.com', 'github.com'] })
await loam.updateSandbox('scratch', { secrets: ['GITHUB_TOKEN'] })
await loam.listSecrets()   // never the values
```
