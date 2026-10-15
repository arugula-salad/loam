# Network

A sandbox reaches the internet and nothing else: never the node it runs on, the cluster, the
local network, or another sandbox. Inside that, a sandbox's `network.egress` rules narrow what it
may reach to the names you allow. The rules are enforced outside the VM, on its node, so nothing
in the sandbox (root included) can widen them.

Nothing reaches *into* a sandbox over the network either: its ports are reached through loam
([Ports and URLs](ports.md)), which works whatever the egress rules say.

| Method and path | |
|---|---|
| `GET /v1/network/defaults` | → `{"egress": [Rule]}`: the hosts `{"include": "defaults"}` stands for |

The rules themselves are the sandbox's `network` field, set at create and changed with
`PATCH /v1/sandboxes/{sandbox}` ([Sandboxes](sandboxes.md)).

## Rules

```json
{"egress": [
  {"include": "defaults"},
  {"domain": "*.githubusercontent.com", "action": "allow"},
  {"domain": "github.com", "action": "allow", "ports": [22, 443]},
  {"domain": "gist.github.com", "action": "deny"}
]}
```

| Field | |
|---|---|
| `domain` | An exact name (`github.com`); `*.` and a name (`*.github.com`: every name under it, not `github.com` itself); or `*` (every name). Lowercase ASCII. An IP address is not a name and is `invalid`. |
| `action` | `allow` or `deny`. |
| `ports` | Allow only: the TCP ports the rule opens; default `[80, 443]`. A deny covers every port and takes none. |
| `include` | `"defaults"`: loam's list of common development hosts (package registries, GitHub and GitLab, container registries, the major AI APIs), each on ports 80 and 443. |

At most 1000 rules, counting `defaults` expanded. `{"egress": []}`, the default, is the whole
internet.

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" "$LOAM_API_URL/v1/network/defaults" | jq -c '.egress | length, .[0:3]'
56
[{"domain":"github.com","action":"allow"},{"domain":"*.github.com","action":"allow"},{"domain":"*.githubusercontent.com","action":"allow"}]
$ loam network defaults | head -4
allow github.com
allow *.github.com
allow *.githubusercontent.com
allow *.githubassets.com
```

### How a connection is decided

A connection to name N on port P is allowed when, among the rules that cover port P, the most
specific one that matches N is an allow:

1. an exact name beats the longest matching `*.` suffix, which beats `*`, whatever their order in
   the list;
2. an allow and a deny on the same `domain` are a deny;
3. no match is a deny.

A name resolves (DNS) when it is allowed on some port. So with the rules above:
`raw.githubusercontent.com:443` is allowed (by `*.githubusercontent.com`), `gist.github.com` is
denied on every port (the exact deny beats `*.github.com` from the defaults), `github.com:22` is
allowed, and `example.com` doesn't even resolve.

## What a restricted sandbox can do

Any rule at all makes the sandbox **restricted**:

- **TCP over IPv4 only.** UDP (QUIC included: clients fall back to TCP), ICMP and IPv6 are
  dropped.
- **DNS** goes to the node's resolver, which answers allowed names and refuses the rest
  (`REFUSED`, so `Could not resolve host`), and never answers with a private address.
- **Ports 80 and 443 are checked by name on every connection**: the `Host` header, or the TLS
  server name (SNI), must be allowed on that port, and the connection goes to an address the node
  resolves for that name itself, wherever the sandbox aimed it. A name that shares an allowed
  name's address (a CDN) stays out, and a TLS connection without a server name is refused.
- **Other ports** have no name to read, so a connection is allowed to an address the sandbox
  resolved, within the DNS answer's TTL, from a name allowed on that port.
- **A refused connection** is closed at once (a reset); on port 80 it first gets a `403` in the
  usual error shape.
- **Changing the rules** on an awake sandbox applies at once, and closes its connections the new
  rules don't allow.

Here it is, in a sandbox made with `--allow-defaults --allow postman-echo.com --deny gist.github.com`:

```console
$ loam create egress-demo --allow-defaults --allow postman-echo.com --deny gist.github.com --json | jq -c .network
{"egress":[{"include":"defaults"},{"domain":"postman-echo.com","action":"allow"},{"domain":"gist.github.com","action":"deny"}]}
$ loam run egress-demo -- curl -sS -o /dev/null -w '%{http_code}\n' https://api.github.com/ </dev/null
200
$ loam run egress-demo -- curl -sS https://example.com/ </dev/null
curl: (6) Could not resolve host: example.com
$ loam run egress-demo -- curl -sS https://gist.github.com/ </dev/null
curl: (6) Could not resolve host: gist.github.com
$ loam run egress-demo -- curl -sS https://1.1.1.1/ </dev/null
curl: (35) Recv failure: Connection reset by peer
$ loam run egress-demo -- curl -sS -i --resolve example.com:80:93.184.215.14 http://example.com/ </dev/null
HTTP/1.1 403 Forbidden
Content-Type: application/json
Connection: close

{"error":{"code":"permission_denied","message":"example.com:80 is not allowed by the network policy"}}
$ loam run egress-demo -- curl -sS --resolve example.com:443:140.82.114.3 https://example.com/ </dev/null
curl: (35) Recv failure: Connection reset by peer
```

The last two aim an allowed name's address (or any address) with a name that isn't allowed; the
name decides. Ports other than 80 and 443 need a rule that names them:

```console
$ loam run egress-demo -- python3 -c 'import socket; s=socket.create_connection(("github.com",22),timeout=6); print(s.recv(40))' </dev/null
ConnectionResetError: [Errno 104] Connection reset by peer
$ loam update egress-demo --allow github.com:22,443 | grep network
network:     allow github.com:22,443
$ loam run egress-demo -- python3 -c 'import socket; s=socket.create_connection(("github.com",22),timeout=6); print(s.recv(40))' </dev/null
b'SSH-2.0-3bc2e72\r\n'
```

## Setting the rules

At create, and with `PATCH`, which replaces the whole list:

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" -H 'Content-Type: application/json' -X PATCH \
    "$LOAM_API_URL/v1/sandboxes/egress-demo" \
    -d '{"network":{"egress":[{"include":"defaults"},{"domain":"github.com","action":"allow","ports":[22,443]},{"domain":"example.com","action":"allow"}]}}' \
    | jq -c .network
{"egress":[{"include":"defaults"},{"domain":"github.com","action":"allow","ports":[22,443]},{"domain":"example.com","action":"allow"}]}
$ curl -sS ... -d '{"network":{"egress":[{"domain":"1.2.3.4","action":"allow"}]}}'
{"error":{"code":"invalid","message":"network.egress: rule 0: invalid domain \"1.2.3.4\": an address is not a name"}}
$ curl -sS ... -d '{"network":{"egress":[{"domain":"x.com","action":"deny","ports":[443]}]}}'
{"error":{"code":"invalid","message":"network.egress: rule 0: a deny covers every port and takes no ports"}}
```

The CLI builds the list from flags, in order: `--allow-defaults` first, then each `--allow DOMAIN`
or `--allow DOMAIN:PORT,PORT` and `--deny DOMAIN`. On `loam update` those flags replace the whole
list (give all the rules you want, not just the new one), and `--unrestricted` empties it:

```console
$ loam update egress-demo --allow-defaults --allow postman-echo.com | grep network
network:     include defaults, allow postman-echo.com
$ loam update egress-demo --unrestricted | grep network
network:     unrestricted
```

A [fork](checkpoints.md#forks) keeps its source's rules unless it gives its own. A project
[secret](secrets.md)'s hosts are allowed on port 443 in a restricted sandbox that uses it, as if
by a rule.

In Go, `api.Network{Egress: []api.Rule{{Include: "defaults"}, {Domain: "github.com", Action: api.RuleAllow, Ports: []int{22, 443}}}}`
on `CreateSandbox` or `UpdateSandbox`; in TypeScript, `network: { egress: [...] }` the same way.
