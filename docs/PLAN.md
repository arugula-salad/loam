# Phase 1: one node, native API

**Done 2026-10-11.** loam runs on the reference deployment (a three-node home k3s cluster), with
a nodelet on one node, and the e2e suite passes against it, 14 of 14. Fixes
found by the live run: Postgres 18's restrict_violation (#9), and a call resuming a VM its
nodelet paused while draining (#10). Measured on that node's XFS data directory (reflinks): pause 320 ms, resume
from pause 29 ms, memory checkpoint 369 ms, disk checkpoint 4 ms, fork 28 ms.

Done when an e2e suite for `docs/API.md` passes against loam on one node, covering what the e2e
suite of sandpit (the single-host Firecracker system loam replaces) covers today: exec (streaming, TTY, attach with replay, one-shot), files, ports and
port URLs, services, checkpoints (memory and disk, restore, fork), pause/stop/wake, idle pause.

## Workstreams

Each is one branch and one or more PRs; the contracts in `docs/` are shared and change only by PR.

| Stream | Owns | Builds from |
|---|---|---|
| **agent** | `cmd/loam-agent`, `internal/agent`, `images/guest/` (the guest root filesystem image), `scripts/build-initramfs.sh` | sandpit's guest agent and base image |
| **nodelet** | `cmd/loam-nodelet`, `internal/nodelet`, `images/nodelet/` (its container image with Firecracker, the kernel and the agent's initramfs) | sandpit's Firecracker engine, VMM wrapper, network and OCI image code |
| **api** | `cmd/loam-api`, `internal/apiserver`, `internal/db` (Postgres, migrations), `internal/nodeapi/fake` | sandpit's record model and lifecycle rules |
| **cli** | `cmd/loam`, `e2e/` | `docs/API.md` |
| **deploy** | the Kustomize deploy, `.github/workflows/images.yml`, and the reference deployment's cluster-side changes | sandpit's manifests |

Integration (the lead): merge order agent → nodelet → api → deploy → cli/e2e, and the end-to-end run on the cluster.


# Phase 2: many nodes

**Done 2026-10-12.** Store (#16), api (#15) and nodelet (#17) landed together, and
`docs/PHASE2-TEST.md` passed on two nodes, every section. The run found four bugs, each
fixed with a test that fails without it:
- **A quick drain wasn't heard (#18).** A nodelet with nothing running drained between two of
  loam-api's polls, so its paused sandboxes waited two minutes. Drains are now announced on the
  event stream.
- **A draining nodelet left DNS (#19).** It dropped out of DNS at SIGTERM, so loam-api couldn't
  record its drain pauses. Fixed with `publishNotReadyAddresses`.
- **A frozen nodelet's stream stayed open (#20).** Its event stream was open but silent, so it
  was never reconciled after the node came back. A silent stream is now dropped after 45 s, and
  the reconnect backoff resets after a good stream.
- **A deleted parent snapshot's upload ran on (#21).** It kept going after its child was
  deleted and wrote a layer loam-api had already collected.

Measured on the cluster (base template, 2 GiB):

| What | Time |
|---|---|
| Wake on the other node, memory loaded lazily | 0.42 s |
| Diff pause after a move | 2 MB uploaded (vs 16 MB for the first) |
| Drain of a node with two awake sandboxes | 13 s |
| Wake after a drain | about 1 s |
| Wake on the other node after a lost node | 5.8 s |
| Fork on the other node | 0.52 s |
| Fork on the node with the checkpoint | 83 ms |

The phase 1 e2e still passes, 14 of 14, and afterwards the bucket and the layer records are
empty.

Done when, on the cluster with nodelets on two nodes: a sandbox paused on one node wakes on
the other with its processes; a node drain moves every awake sandbox without losing a process; a
node taken away has its paused and stopped sandboxes wake elsewhere; a fork lands on whichever node
has room; deleting sandboxes frees their layers in the store; and the phase 1 e2e suite still
passes. Contract: `docs/NODE.md`, "Phase 2: the snapshot store".

| Stream | Owns |
|---|---|
| **store** | `internal/snapstore` (E2B's header, storage, block and UFFD code, copied and trimmed: S3 and filesystem backends, static config, no telemetry, no peers, no cgo), `docs/STORE.md`, tests against a filesystem backend and, behind a tag, Garage |
| **nodelet** | uploads on pause/stop/checkpoint, dirty-page and extent diffs, restore from the store (eager disk layers, lazy memory), the UFFD handler process, drain with uploads, layer deletion, chunk cache eviction |
| **api** | snapshot names for pauses and stops, layer refcounts and GC, placement on wake (the node that has it, else any with room), moving a sandbox, lost nodes, reconcile of VMs whose record names another node |
| **infra** | Garage bucket and key, the nodelet's store env and `/dev/userfaultfd`, an XFS image file for the second node (its disk is ext4), the second node labelled a loam node, a two-node test |


# Phase 3: the network boundary

**Done 2026-10-12.** Contract, api, identity and nodelet landed together (#27), and
`docs/PHASE3-TEST.md` passed on two nodes, every section; the phase 1 e2e and the new
network, secrets and identity tests pass, 17 of 17. Found by the run, each fixed:
- **A moved sandbox had no network for half a minute (#28).** Each node's bridge had its own
  random MAC, so a guest woken elsewhere sent to its old gateway's MAC until its ARP entry went
  stale. `loam0` now has the same MAC everywhere.
- **The identity e2e test checked a label the suite overwrites** (fixed with the test doc).
- **The test doc**: `nc -z` always succeeds against a transparent proxy (read a banner instead),
  and a snapshot scan must look for a secret's whole value, since the guest's own commands hold
  parts of it.

Also found before the run: Chrome sends the ECH extension on every connection, so refusing it
refused Chrome; an ECH ClientHello is judged by its outer name (#27).

A sandbox reaches only what its project lets it, uses credentials it never holds, and can prove
who it is. Everything is enforced in the nodelet, outside the guest; the guest is never trusted
to apply its own policy.

Done when, on two nodes:
- A sandbox with an egress allowlist reaches the names it allows, gets REFUSED for others, can't
  reach a name it doesn't allow on an allowed name's CDN address, reaches a non-web port only
  where a rule names it, and has its open connections cut when a PATCH tightens the policy.
- A sandbox with `GITHUB_TOKEN` brokered gets 200 from `curl -H "Authorization: Bearer
  $GITHUB_TOKEN" https://api.github.com/user`, while its env, `/proc`, its disk and its memory
  snapshot hold only the placeholder, and an echo host it sends the placeholder to sees the
  placeholder. Changing the secret reaches an awake sandbox without a wake.
- A sandbox's identity token verifies, from outside the cluster, against loam-api's published keys.
- Policy, secrets and identity hold across pause and wake, a move to the other node, and a fork.
- The phase 1 e2e and `docs/PHASE2-TEST.md` still pass; `docs/PHASE3-TEST.md` is the run.

## Decided

**Egress.** `network.egress` on a sandbox is a list of rules, sandpit's model (its `netpolicy`:
exact names, `*.example.com`, `*`, `include: defaults`, deny beside allow, the most specific
wins), plus ports: a bare name allows 80 and 443, and `name:22` allows port 22 too. No rules is
the whole internet, as in phase 1. A fork inherits its source's rules. A restricted sandbox is
TCP only: UDP (QUIC included), ICMP and IPv6 are dropped. DNS goes through the nodelet's
forwarder, which refuses names the policy doesn't allow, strips private answers, and remembers
what it answered each VM. TCP goes to a transparent proxy in the nodelet: on 80 and 443 it reads
the `Host` header or the ClientHello's SNI, checks that name, resolves it itself and dials the
answer, so another name on a shared CDN address stays out and a sandbox that moved nodes needs no
DNS it already cached; a ClientHello without SNI, or with ECH, is refused. Other ports reach only
addresses the VM resolved from a name whose rule names that port. The nodelet owns its network
namespace, so it writes nftables itself; sandpit's privileged network helper does not come over.

**Brokering.** Transparent MITM, scoped to brokered hosts, ported from an earlier credential
broker by the same author (its parity tests are the checklist). A project
secret (`PUT /v1/secrets/{name}`, in the key's project, never readable back) has
a name, a value, the hosts it may go to (`host[:port][/path]`) and how it goes in: substituted
for its placeholder in the request head, byte for byte, or set as a header from a template. The
value is encrypted in Postgres under a key from the deployment's secrets. A sandbox names the secrets it uses;
its env gets the placeholder (`__loam_<name>__`, with an optional prefix for CLIs that check
their token's shape), loam-api hands the values to the nodelet with each wake and on change, and
the nodelet keeps them in memory while the VM is awake: never on its disk, in a snapshot or in
the agent. The VM is the session (its tap address), so there is no proxy token. Only a
brokered host's TLS is terminated, with a leaf from a CA root kept in the deployment's secrets (unlike
the earlier broker's, not derived from a seed: Go's ECDSA signatures aren't deterministic, so a derived
root would differ in its bytes between nodelets), the same on every nodelet; every other host is spliced on its SNI. Carried from the earlier broker: a
missing secret is a 502, never an unauthenticated request; no redirects followed; no pooled
origin connections; the path logged, never the query; bodies and WebSocket frames untouched.
The agent installs the root at every wake, in the OS store and for `NODE_EXTRA_CA_CERTS`,
`SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, `CARGO_HTTP_CAINFO` and `UV_NATIVE_TLS`.

**Identity.** loam-api is an OIDC issuer (`/.well-known/openid-configuration` and its keys,
signed with a key from the deployment's secrets, rotated by `kid`). Inside a sandbox, `GET /identity/token?audience=`
on `/run/loam/api.sock` goes over the host channel and returns a short-lived token for
`project:<project>:sandbox:<id>` with its name, template and labels. The host channel already
makes the VM the identity, so a sandbox can only get its own token.

| PR | Owns |
|---|---|
| **contract** | `docs/API.md` (`network`, secrets, `/identity/token`), `docs/NODE.md` (policy and secrets on create and wake, changing them on an awake VM, the CA), `docs/AGENT.md` (the root at wake, the identity route), and their types in `pkg/api` and `internal/nodeapi` |
| **egress** | sandpit's netpolicy copied into `internal/nodelet/network` and cut down, the DNS policy, the proxy with name checks and ports; the records, PATCH and the CLI |
| **identity** | the issuer and its keys, `/internal/guest/identity/token`, the agent's route, a verifier test; independent of the others |
| **brokering** | the secret store and its encryption, delivery to nodelets, the MITM in the proxy (HTTP/1.1 and h2), the CA and leaf cache, placeholders, the agent's trust store |
| **deploy and test** | the broker CA, the identity signing key and the secret key; `/.well-known` through Traefik; `docs/PHASE3-TEST.md` run on two nodes |

Order: contract, then egress (brokering sits in its proxy) and identity in parallel, then
brokering, then deploy and the test.


# Phase 4: off sandpit

**Done 2026-10-12.** Every caller on the reference deployment runs on loam:
- **A set of browser games:** all 17, byte-exact, on their own URL domain, in project `games`.
- **Hosted dev boxes:** every box on its own URL domain, in project `studio`.
- **A CI runner** in place of the old one.
- **E2B:** an E2B endpoint where the official SDKs pass the conformance run, 26 of 26.
- **loam-pg:** backed up to Garage, and a restore drill matched every table.
- **sandpit:** every instance stopped and disabled, and the third node runs a small nodelet.

Found by the migrations, each fixed:
- **loam-api never forwarded a sandbox's tasks (#39).**
- **A tar extract dropped setuid and setgid (#37).**
- **The client's release step needed gh (#36).**
- **A moved sandbox had no network for half a minute** (phase 3's #28).

sandpit, the single-host Firecracker system loam replaced, ran as three instances, and its
callers spoke its Sprites-style API. Phase 4 moves each onto loam's own API, brings E2B back as an
adapter for the people who use its SDKs, and stops sandpit.

Done when, on the cluster:
- the game and dev-box apps run their sandboxes on loam, on their own hostnames, with their
  users' links and passkeys working as before;
- E2B's official Python and JS SDKs pass the E2B conformance run (`e2e/e2b`, 13 steps each)
  against loam;
- monitoring watches loam, and the sandboxes worth keeping are copied over;
- sandpit is stopped, its third node runs a nodelet, and loam-pg is backed up;
- the phase 1 to 3 suites still pass.

## Decided

**The callers move to loam's API**, not to a Sprites front end (the vendor front ends stay out,
`docs/ARCHITECTURE.md`), through a small TypeScript client, `@arugula-salad/loam`, since every
caller is Node. Each migration is a PR in its app's repo.

**Images on create.** `CreateSandbox` takes `image` (a digest, from registries loam-api's
`--allowed-registries` names) in place of `template`; the nodelet already pulls, flattens and
caches by digest. Template sandboxes (a dev box, a game) stay what they are: a sandbox with a
checkpoint, forked with `from`.

**Hostnames.** A project has URL domains, the operator's like templates (loam-api's `--domains`
file): a sandbox named N in it is also `https://N.<domain>`, and its ports
`https://<port>--N.<domain>`. The deploy carries a wildcard cert-manager Certificate (DNS-01) and
a Traefik route per domain; loam carries no ACME code and loam-api never writes to Kubernetes.
The callers filter by exact labels, which loam has; they change their label scheme to fit
rather than loam growing prefix filters. Custom domains per sandbox stay out.

**Lobbies are not sandboxes.** An app's lobby, the thing that creates its sandboxes, runs as a
cluster pod with a project key: loam's in-sandbox API gains no create.

**E2B is E2B's own envd** in the guest: the `e2b` template's image ships envd and a loam service
definition that starts it at every boot, and the adapter in loam-api (`docs/E2B.md`) calls envd's
`/init` with the sandbox's token once per boot of its VM (the VM's `booted_at`), before any E2B
traffic reaches it. The adapter is a client of loam's API in the same process, so an E2B sandbox
is a loam sandbox in the key's project: `timeout` is a deadline, `metadata` labels under `e2b/`,
pause and connect are pause (or stop) and wake, and IDs are loam's. It proxies the data plane
(`<port>-<id>.<e2b domain>`, and the SDK's `E2B_SANDBOX_URL` headers) through the port stream.
What loam can now enforce, it does: `allow_internet_access: false` and `denyOut` of all traffic
become egress rules, with `allowOut` names allowed; CIDR rules stay refused. sandpit's adapter
comments and its 18 unit tests come with the code, and both SDKs pass the conformance run against
a local loam-api and nodelet in a container.

**Existing sandboxes are recreated.** Apps redeploy; sandboxes worth keeping are copied by a
one-off script (a tar of the disk out of sandpit, into a new sandbox through `fs/tar`). Memory
and processes don't move.

| PR | Owns |
|---|---|
| **contract** | `docs/API.md` (`image` on create, project URL domains), `docs/NODE.md` (images by digest), their types |
| **api** | images and the registry allowlist, URL domains (loam-api's `--domains` file, and a Certificate and route per domain in the deploy) |
| **e2b** | `docs/E2B.md` (the adapter's contract), the adapter (`internal/e2b`), envd in the guest image and its `/init` at every wake, the SDK conformance run in `e2e/e2b` |
| **client** | `@arugula-salad/loam` (TypeScript, in `clients/ts`) |
| **migrations** | each app, one by one, then the E2B endpoint and monitoring |
| **infra** | loam-pg backups (CNPG to Garage), the third node as a loam node, stopping sandpit |

Order: contract; then api, e2b and client in parallel; then the migrations one by one, each
checked live before the next; then the decommission.
