# Phase 3 on two nodes: the test

The acceptance test run on the reference deployment (a three-node k3s cluster) when phase 3
landed, with the node names generalized to node-a and node-b.

What the lead runs once phase 3 has landed (contract, api, nodelet and identity deployed, with
the broker CA and identity key of `docs/INSTALL.md`), to check `docs/PLAN.md`'s
done-when on node-a and node-b. Each section says what it proves and what passing looks like. Run
them in order; each leaves the cluster as it found it, except the secret and sandboxes that
"Cleanup" removes.

The settings go through the API with `curl`, so this doesn't depend on the CLI's flags; the
CLI runs commands inside sandboxes. Run it from a terminal: `loam run` sends whatever is on its
stdin when that isn't a terminal, so from a script (or an agent's shell) give it `</dev/null`, or
it waits for an EOF that never comes.

## 0. Setup and preflight

```bash
export LOAM_API_URL=https://api.loam.example.com
export LOAM_ADMIN_TOKEN=...            # the deployment's admin token
LABEL=loam.example.com/node            # the label that puts a nodelet on a node (docs/INSTALL.md)
go build -o /tmp/loam ./cmd/loam && L=/tmp/loam

$L admin project create p3test
export LOAM_TOKEN=$($L admin key create p3test lead)

api() {  # api <method> <path> [json]
  curl -sS -X $1 -H "Authorization: Bearer $LOAM_TOKEN" -H 'Content-Type: application/json' \
    ${3:+-d "$3"} "$LOAM_API_URL$2"; echo; }
where() { kubectl -n loam exec loam-pg-1 -c postgres -- psql -d loam -At -F' ' \
  -c "select name, state, node from sandboxes where project = 'p3test' order by name"; }
NODE_TOKEN=$(kubectl -n loam get secret loam-node-token -o jsonpath='{.data.LOAM_NODE_TOKEN}' | base64 -d)
nodelet() {  # nodelet <node> <path>
  local pod; pod=$(kubectl -n loam get pod -l app=loam-nodelet --field-selector spec.nodeName=$1 -o name)
  kubectl -n loam port-forward $pod 17070:7070 >/dev/null & local pf=$!; sleep 2
  curl -s -H "Authorization: Bearer $NODE_TOKEN" localhost:17070$2; kill $pf; }
```

Preflight, all must hold:

```bash
for n in node-a node-b; do nodelet $n /v1/node | jq '{name, ready, broker}'; done    # ready and broker true on both
curl -s $LOAM_API_URL/.well-known/openid-configuration | jq '{issuer, jwks_uri}'   # https://api.loam.example.com
curl -s $LOAM_API_URL/.well-known/jwks.json | jq '.keys | length'                  # 1 (2 mid-rotation)
api GET /v1/network/defaults | jq '.egress | length'                               # the defaults list
kubectl -n loam logs ds/loam-nodelet --all-containers --prefix | grep -i -E 'broker|proxy|policy' | tail
```

## 1. Egress: names, ports and the CDN

Proves: a restricted sandbox resolves and reaches only what its rules allow, by name on 80 and
443 and by port elsewhere; a name sharing an allowed name's address stays out; UDP doesn't leave.

```bash
api POST /v1/sandboxes '{"name": "fenced", "idle": {"action": "none"}, "network": {"egress": [
  {"domain": "pypi.org", "action": "allow"},
  {"domain": "github.com", "action": "allow", "ports": [22, 443]}]}}' | jq .network
in() { $L run fenced -- sh -c "$1"; }

in 'getent hosts pypi.org && getent hosts github.com'                    # both resolve
in 'getent hosts example.com || echo refused'                           # refused
in 'curl -sS -o /dev/null -w "%{http_code}\n" https://pypi.org/simple/'  # 200
in 'curl -sS -m 5 https://example.com; echo "exit $?"'                  # fails: not resolvable
# The CDN: pypi.org and www.python.org are both on Fastly. Aim www.python.org at pypi.org's address.
in 'ip=$(getent hosts pypi.org | head -1 | cut -d" " -f1); curl -sS -m 5 --resolve www.python.org:443:$ip https://www.python.org; echo "exit $?"'   # fails: the SNI isn't allowed
in 'curl -sS -m 5 -H "Host: www.python.org" http://pypi.org/; echo'     # 403 with the error shape
# The proxy accepts every connection before it decides, so `nc -z` always "succeeds": read
# what comes back instead. A refused connection is closed with nothing sent.
in 'timeout 8 nc -w5 github.com 22 </dev/null | head -c 20; echo'      # SSH-2.0-…
in 'timeout 8 nc -w5 github.com 2222 </dev/null | head -c 20; echo'    # nothing: a port no rule names
in 'timeout 8 nc -w5 pypi.org 22 </dev/null | head -c 20; echo'        # nothing: pypi.org's rule has 80 and 443
in 'nc -zuw3 1.1.1.1 53; dig +time=2 +tries=1 @1.1.1.1 github.com; echo "exit $?"'   # times out: no UDP out
in 'curl -sS -m 5 -6 https://pypi.org; echo "exit $?"'                  # fails: no IPv6
```

Then tightening it while a connection is open:

```bash
$L exec fenced -- sh -c 'nc github.com 22; echo "nc ended $?"' </dev/null &   # prints the SSH banner and waits
sleep 3
api PATCH /v1/sandboxes/fenced '{"network": {"egress": [{"domain": "pypi.org", "action": "allow"}]}}' | jq .network
wait                                                                     # "nc ended": the connection was closed
in 'getent hosts github.com || echo refused'                            # refused
```

Pass: every line as commented. An unrestricted sandbox is unchanged:
`$L create open; $L run open -- sh -c 'curl -sS -o /dev/null -w "%{http_code}\n" https://example.com; dig +short @1.1.1.1 example.com'`
gives 200 and an address.

## 2. Brokering

Proves: a sandbox uses a credential it never holds; the value goes only to the secret's hosts;
a reflected value is cut off; a changed value reaches an awake sandbox.

The test token is the lead's own GitHub token, read-only use; delete the secret afterwards
(Cleanup).

```bash
api PUT /v1/secrets/GITHUB_TOKEN "$(jq -n --arg v "$(gh auth token)" \
  '{value: $v, hosts: ["api.github.com"]}')" | jq                            # no value in the answer
api PUT /v1/secrets/ECHO_TOKEN '{"value": "s3cr3t-echo-value-0123456789", "hosts": ["httpbin.org"]}' | jq .placeholder
api POST /v1/sandboxes '{"name": "broker", "idle": {"action": "none"}, "secrets": ["GITHUB_TOKEN", "ECHO_TOKEN"]}' | jq .secrets
in() { $L run broker -- sh -c "$1"; }

in 'echo $GITHUB_TOKEN'                                                  # __loam_github_token__
in 'curl -sS -H "Authorization: Bearer $GITHUB_TOKEN" https://api.github.com/user | jq -r .login'   # your login
in 'curl -sS -H "Authorization: token $GITHUB_TOKEN" https://api.github.com/user | jq -r .login'    # the same
in 'curl -sS https://postman-echo.com/headers -H "X-T: $GITHUB_TOKEN" | jq -r ".headers[\"x-t\"]"' # __loam_github_token__: not a secret host
in 'curl -sS -o /dev/null -w "%{http_code}\n" https://httpbin.org/headers -H "X-T: $ECHO_TOKEN"'    # 502: the answer held the value
in 'curl -sS -o /dev/null -w "%{http_code}\n" https://httpbin.org/get'                               # 200: no value, nothing cut
in 'grep -l -a -r "$(echo czNjcjN0LWVjaG8tdmFsdWUtMDEyMzQ1Njc4OQ== | base64 -d)" /proc/*/environ /etc /home 2>/dev/null; echo done'   # done, and nothing listed
in 'ls /etc/loam/ca.pem && env | grep -E "NODE_EXTRA_CA_CERTS|SSL_CERT_FILE"'                    # the CA and its variables
```

A changed value, without a wake:

```bash
api PUT /v1/secrets/GITHUB_TOKEN '{"value": "ghp_not_a_real_token", "hosts": ["api.github.com"]}' >/dev/null
in 'curl -sS -o /dev/null -w "%{http_code}\n" -H "Authorization: Bearer $GITHUB_TOKEN" https://api.github.com/user'   # 401
api PUT /v1/secrets/GITHUB_TOKEN "$(jq -n --arg v "$(gh auth token)" '{value: $v, hosts: ["api.github.com"]}')" >/dev/null
in 'curl -sS -H "Authorization: Bearer $GITHUB_TOKEN" https://api.github.com/user | jq -r .login'   # your login again
api DELETE /v1/secrets/ECHO_TOKEN                                        # 409 conflict: broker names it
```

The value isn't in a snapshot. Take a memory checkpoint, then look for both values in the VM's
files and the checkpoint's on its node (`/var/lib/loam` in the pod). Search for the *whole*
value: the guest ran commands holding parts of it (the `grep` above), and those are in its
memory. The placeholder is the control: it must be found, or the scan proves nothing.

```bash
$L checkpoint create broker
sb=$(api GET /v1/sandboxes/broker | jq -r .id); node=$(where | awk '$1=="broker"{print $3}')
pod=$(kubectl -n loam get pod -l app=loam-nodelet --field-selector spec.nodeName=$node -o name)
printf '%s\n%s\n' s3cr3t-echo-value-0123456789 "$(gh auth token)" |
  kubectl -n loam exec -i $pod -- sh -c "cat > /tmp/n; grep -l -a -F -f /tmp/n /var/lib/loam/vms/$sb/* /var/lib/loam/snapshots/$sb.v1/* 2>/dev/null; rm -f /tmp/n; echo done"
                                                         # done, nothing listed
kubectl -n loam exec $pod -- grep -c -a -F __loam_github_token__ /var/lib/loam/snapshots/$sb.v1/snap.mem   # more than 0
```

Pass: every line as commented.

## 3. Identity

Proves: a sandbox gets a token that names it, and it verifies from outside against the keys
loam-api publishes.

```bash
in 'curl -sS --unix-socket /run/loam/api.sock "http://loam/identity/token?audience=sts.amazonaws.com"' > /tmp/tok.json
jq -r .token /tmp/tok.json | cut -d. -f2 | tr '_-' '/+' | base64 -d 2>/dev/null | jq '{iss, sub, aud, sandbox_name}'
                                                  # sub project:p3test:sandbox:sb-…, sandbox_name broker
in 'curl -sS --unix-socket /run/loam/api.sock "http://loam/identity/token"'   # 400 invalid: no audience
go test -tags e2e -run Identity ./e2e                                         # verifies through discovery and the JWKS
```

Pass: the claims name the sandbox and the e2e test passes.

## 4. Across a pause, a move and a fork

Proves: the policy and secrets come back with every wake, on either node, and a fork takes them.

```bash
mover=$(where | awk '$1=="broker"{print $3}')                      # broker's node; take it away
$L pause broker fenced
kubectl label node $mover loam.arugula.io/node-
kubectl -n loam wait --for=delete pod -l app=loam-nodelet --field-selector spec.nodeName=$mover --timeout=360s
$L run broker -- sh -c 'curl -sS -H "Authorization: Bearer $GITHUB_TOKEN" https://api.github.com/user | jq -r .login'   # your login
$L run fenced -- sh -c 'curl -sS -m 5 https://example.com; echo "exit $?"'                                            # still fails
where                                                              # both on the other node
kubectl label node $mover loam.arugula.io/node=true
kubectl -n loam rollout status ds/loam-nodelet

$L checkpoint create fenced
$L fork fenced v1 fenced2
api GET /v1/sandboxes/fenced2 | jq '.network'                      # fenced's rules
$L run fenced2 -- sh -c 'getent hosts example.com || echo refused' # refused
```

Pass: as commented. The first call after the move works at once: every node's bridge has the
same MAC, so the guest's ARP entry for its gateway is still right (#28; before it, the first
call after a move timed out resolving).

## 5. Phases 1 and 2 still pass

```bash
go test -tags e2e ./e2e                       # the phase 1 suite, and the new network, secrets and identity tests
```

and `docs/PHASE2-TEST.md`, sections 1 and 2 (a move and a drain), on sandboxes created with a
policy and a secret, which carry them across.

## Cleanup

```bash
for s in fenced fenced2 broker open; do api DELETE /v1/sandboxes/$s; done
api DELETE /v1/secrets/GITHUB_TOKEN; api DELETE /v1/secrets/ECHO_TOKEN
$L admin project rm p3test
```
