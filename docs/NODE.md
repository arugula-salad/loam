# Node protocol: loam-api ↔ loam-nodelet

loam-nodelet runs the VMs of one node. loam-api is its only client. Go types, the client and the
stream helpers: `internal/nodeapi`. A VM's ID is its sandbox's ID.

## Reaching a nodelet

The nodelets are a DaemonSet behind the headless Service `loam-nodelet` (port 7070, HTTP).
loam-api finds them by resolving the Service and asking each `GET /v1/node`, and reaches the
nodelet of node `N` by resolving again every time it needs it (a cache of a few seconds at most):
a record stores the node's *name*, never an address. Every request carries
`Authorization: Bearer <node token>` (the Secret `loam-node-token`); anything else is 401.

## The node

`GET /v1/node` → `Node`:

```json
{"name": "node-a", "version": "…", "cpus": 32, "memory_mib": 65536, "memory_mib_allocated": 6144,
 "disk_gib_free": 250, "vms": 3, "ready": true, "broker": true}
```

`memory_mib` is what the nodelet may hand to VMs (its `--memory` flag), not the node's RAM.
`memory_mib_allocated` sums the `resources.memory_mib` of its running VMs: admission reserves
the ceiling, because the balloon is an economy, not a boundary.
`broker` is whether the nodelet has the broker CA, without which it can't run a VM with secrets
("Phase 3: the network boundary").

## VMs

```json
{"id": "sb-…", "state": "running", "template": "base", "resources": {…}, "booted_at": "…", "ip": "10.240.0.7"}
```

States: `running`, `paused` (a memory snapshot on this node, no VM process), `stopped` (disk
only), `error` (`"error": "<message>"`). A VM the nodelet doesn't have answers 404 `not_found`.

| Route | Body → answer |
|---|---|
| `GET /v1/vms` | → `{"vms": [VM]}` |
| `GET /v1/vms/{id}` | → `VM` |
| `POST /v1/vms` | `CreateVM` → 201 `VM` (`running`), after the agent has answered `/internal/wake`. 409 if the ID exists. 503 `capacity` if the memory doesn't fit. |
| `POST /v1/vms/{id}/resume` | `{"wake": Wake, "policy": Policy}` → `VM` (`running`). From `paused`: restores memory and disk, processes intact. From `stopped`: a fresh boot on the disk. On `running`: hands the agent the new `wake` and answers. A paused VM that can't be restored stays `paused`; one that was restored but whose agent didn't answer the wake is `stopped` (its guest ran, so the memory no longer matches the disk), and the error says so. |
| `POST /v1/vms/{id}/pause` | → `VM` (`paused`). Asks the agent `/internal/presuspend` first, then inflates the balloon, snapshots and exits the VM. |
| `POST /v1/vms/{id}/stop` | → `VM` (`stopped`). Graceful poweroff through the agent, killed after 10 s. |
| `DELETE /v1/vms/{id}` | 204. The VM, its disk and its pause snapshot. Not its checkpoints. |
| `POST /v1/vms/{id}/snapshots` | `{"id": "<snapshot id>", "memory": bool}` → 201 `Snapshot`. Memory: pause briefly, snapshot memory and disk, resume. Disk only: a reflink copy of the disk while it runs (the agent `sync`s first). The VM may be `running`, `paused` or `stopped`; memory needs `running` or `paused`. |
| `POST /v1/vms/{id}/restore` | `{"snapshot": "<id>", "wake": Wake, "policy": Policy}` → `VM` (`running`). Replaces the VM's disk (and memory, if the snapshot has it) with the snapshot's, ending the old VM process. |
| `GET /v1/vms/{id}/agent` | stream, see below |
| `GET /v1/vms/{id}/activity` | → `Activity` from the agent: `{"idle_ms","sessions_attached","tasks","busy"}`. Doesn't wake a paused VM: 409 `conflict`. |

`CreateVM`:

```json
{"id": "sb-…", "template": {"name": "base", "image": "ghcr.io/…@sha256:…"},
 "resources": {"cpus": 2, "memory_mib": 2048, "disk_gib": 20},
 "from": {"snapshot": "sb-other.v3"},
 "wake": Wake, "policy": Policy}
```

`template.name` is `""` for a sandbox made from an image rather than a template; the nodelet
treats the two alike, by `image`. Without `from`, the disk is a reflink clone of the template's root filesystem (built from
`image` on first use and cached by digest: pulled, flattened to ext4, grown to `disk_gib`), and
the VM boots fresh. With `from`, the disk (and memory, if the snapshot has it) comes from the
snapshot, which must be on this node in phase 1 (any stored snapshot in phase 2, below); `template` and `resources` must match the
snapshot's, which it records.

`Wake` is what the agent gets at every boot and resume, and only then (`docs/AGENT.md`). Its
`env` is the sandbox's with its secrets' placeholders added; never a secret's value:

```json
{"sandbox": "sb-…", "name": "scratch", "project": "jake", "hostname": "scratch",
 "env": {"FOO": "bar"}, "user": "loam", "time": "2026-10-11T18:00:00.123Z"}
```

loam-api leaves `network` out; the nodelet fills it in on every wake it hands the agent:
`{"ip": "10.240.0.7/16", "gateway": "10.240.0.1", "dns": ["10.240.0.1"], "mac": "06:00:0a:f0:00:07"}`.
A fresh boot has the same from the kernel command line (`docs/AGENT.md`), but a memory snapshot
restored onto another VM (a fork, or a restore after the VM got a new address) wakes with its
source's address and MAC, which the agent replaces with these. The nodelet also sets `time` as
it sends each wake (the moment the guest's clock is set to), whatever loam-api put there, and
`ca`, the broker CA's root certificate (PEM) when it has one, which the agent installs.

## Snapshots

`{"id", "vm", "memory", "template", "resources", "size_bytes", "created_at"}` (and, in phase 2,
`"parent"`, `"layers"`, `"stored"`). IDs are chosen by loam-api: `<sandbox id>.<checkpoint id>`
(`sb-7k2q9xw4m3ab.v3`) for checkpoints, `<sandbox id>.@<n>` for pauses and stops (phase 2), and
`<sandbox id>.@drain-<unix ms>` by the nodelet for its own pauses while draining.

| Route | |
|---|---|
| `GET /v1/snapshots` | → `{"snapshots": [Snapshot]}` |
| `DELETE /v1/snapshots/{id}` | 204 |

A snapshot outlives its VM: deleting a sandbox's VM leaves its checkpoints until loam-api deletes
them. In phase 1 snapshots live in the nodelet's data directory, as reflink clones of the disk and
the memory file with holes kept. Phase 2 moves them to object storage.

## Phase 2: the snapshot store

Phase 2 makes every snapshot durable in object storage, so a sandbox can wake on any node. What
changes from the sections above; where they disagree, this wins.

**The store.** The Garage bucket `loam-snapshots`. Only nodelets hold its key (env
`LOAM_STORE_URL`, for Garage in the cluster something like `s3://loam-snapshots?endpoint=http://s3.garage.svc.cluster.local:3900&s3ForcePathStyle=true&region=garage`,
plus `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`); loam-api never touches it. The format is
E2B's, copied and trimmed into `internal/snapstore` (`docs/spikes/e2b-snapshot-store.md`,
written up in `docs/STORE.md`): one object per **layer** (the blocks that changed since its
parent, packed, in independently compressed 2 MiB zstd frames), and per **snapshot** a flattened
header per file (memory, disk) mapping every offset to (layer, offset in layer) or zero, plus
`meta.json` (template, resources, memory or not, Firecracker's vmstate). Reading a snapshot
never walks a chain of parents.

**Every snapshot is stored**: checkpoints, and now pauses and stops too.

| Route | Change |
|---|---|
| `POST /v1/vms/{id}/pause` | Body `{"snapshot": "<id>"}`, named by loam-api (`<sandbox>.@<n>`). The answer comes once the snapshot is on local disk; the upload continues. A VM that is paused already makes no new snapshot: the answer is the VM as it is, its `snapshot` naming the one it is paused on. |
| `POST /v1/vms/{id}/stop` | Body `{"snapshot": "<id>"}`: a disk-only snapshot of the stopped disk, stored the same way (from `paused` too, of the paused disk). A VM that is stopped already makes none, as for pause. |
| `POST /v1/vms/{id}/snapshots` | Unchanged, and now uploaded too. |
| `POST /v1/vms` | `from.snapshot` may be any stored snapshot, on this node or not. A memory snapshot created under the sandbox's **own** ID is how a sandbox **moves**: its processes resume on this node. Under another ID it is a fork, as before. |
| `POST /v1/vms/{id}/restore` | `snapshot` may likewise be any stored snapshot. |
| `GET /v1/snapshots/{id}` | New: → `Snapshot`, from this node's local copy or the store's; 404 when neither has it. |
| `DELETE /v1/snapshots/{id}` | Deletes the snapshot's header and meta, locally and in the store, through any nodelet. Not its layers. An upload in flight is cancelled before the answer, so nothing of it is written after. 409 `conflict` while a VM on this node needs it (paused on it, or running with memory still served from it); 404 when neither copy exists. |
| `DELETE /v1/vms/{id}` | The VM and its local files. Never a stored snapshot, its pause snapshot included: loam-api deletes snapshots by name, because a sandbox that moved still wakes from one its old node made. |
| `DELETE /v1/layers/{layer}` | New: deletes a layer object; 404 when there is none. loam-api calls it once no snapshot it knows of references the layer (any nodelet will do; they share the store). |

`Snapshot` gains `"parent"` (the snapshot it is a diff against, or `""`), `"layers"` (every layer
its headers reference, its own last) and `"stored"` (`true` once the upload is done: its header,
its meta and every layer it references are in the store, its parent's included, so any node can
restore it). Layer IDs are the nodelet's to choose: opaque, unique in the store, and safe in a URL
path segment. A snapshot's `layers` are known, and final, when it is created, before the upload.

A stored snapshot comes as an event, `event: snapshot`, `data: Snapshot` (with `stored: true`),
once per snapshot, whoever asked for it. `VM` gains `"snapshot"`: the **ID** of its newest
snapshot, the one a move or a wake elsewhere would use (`""` until it has one). These make a
snapshot the VM's newest: a pause, a stop, a memory snapshot, a disk-only snapshot of a stopped VM,
a create from a snapshot (that one) and a restore (the one restored). A disk-only snapshot of a
running or paused VM doesn't: waking from it would lose memory the VM still has. loam-api keeps
the same rule in its records.

**Diffs.** A pause after a resume uploads only what changed: memory from Firecracker's dirty
page tracking (`track_dirty_pages` and a diff snapshot), disk from the ranges where the VM's
disk and a reflink clone of its parent's disk (kept beside it) map to different physical
blocks (FIEMAP on both; the `SHARED` flag alone can't tell, since a checkpoint's clone shares
the VM's new blocks too). A generation's parent is the VM's newest snapshot (`VM.snapshot`);
its upload waits for the parent's. A sandbox's first pause uploads its whole memory and its
disk's diff against the template, which every node builds byte for byte the same (a filesystem
UUID and hash seed derived from the image digest and size, a fixed clock), so a disk is stored
as its difference from it. A VM made before phase 2 stores its disk whole, once.

**Same-node wakes.** A snapshot whose whole memory is on this node (every one of a VM that
booted here, or was restored here from a local copy) restores by mapping that file, as in
phase 1. A VM whose memory came lazily from the store pauses into a diff that is not whole on
its own: its wake, here too, waits for that snapshot's upload and serves the memory from the
store.

**Restoring elsewhere.** On a node without the snapshot: the template disk is built or taken
from the image cache, the snapshot's disk layers are fetched and applied to a clone of it before
the VM starts (they are small), and memory is served lazily by a userfaultfd handler in a process
of its own (not the nodelet's: a fault inside the handler's own process deadlocks Go). Chunks are
cached under `--data` and evicted like images. Firecracker needs a userfaultfd that handles
kernel faults on guest memory, which the host's `vm.unprivileged_userfaultfd=0` reserves for
`CAP_SYS_PTRACE`. In a pod that capability isn't enough (containerd's RuntimeDefault seccomp
profile refuses `userfaultfd(2)` whatever the capabilities), so the pod gets the device
`/dev/userfaultfd` instead, through a device plugin (`docs/INSTALL.md`), which
Firecracker uses first when it exists: no capability, no host sysctl, no custom seccomp profile.

**Drain.** On SIGTERM, stop being ready, say so on the event stream (`event: node`, the Node
with `"ready": false`), pause every running VM and wait for the uploads, inside the grace
period. A pause the nodelet makes on its own is named by it,
`<sandbox>.@drain-<unix milliseconds>`, and is told like any change not asked for: `event: vm`
(`paused`, its `snapshot` naming the new one), then `event: snapshot` when it is stored. A
nodelet that isn't ready refuses `POST /v1/vms`, a restore, and a resume of a VM that isn't
running with 503 `not_ready`. Pause and stop name their snapshot: without one they are 400
`invalid`, and with one that exists on this node 409 `already_exists`. loam-api records the drain's snapshots from the events, and once the node no longer
answers (having last answered not ready) wakes those sandboxes elsewhere at the next call, without
waiting the two minutes below.

**A lost node.** loam-api treats a sandbox whose node has been unreachable for 2 minutes as
movable if its newest snapshot is stored: the next call wakes it on another node from that
snapshot, and anything after it (the rest of a running VM's life) is lost. A running one is set
`paused` (or `stopped`, for a disk-only snapshot) when its node is found lost, with an event that
says so. When the old node comes back, its VM for that sandbox is deleted at reconcile (the record
names another node), as is the VM of a sandbox deleted while its node was away. What this can't
recover: a drain whose events loam-api missed, on a node that never comes back. loam-api doesn't
know the drain's snapshot, so the sandbox wakes from its older newest snapshot, if that one is
stored.

**Local copies.** A nodelet that finds a local copy of a stored snapshot whose store copy is gone
(deleted through another node while it was away) deletes the local copy. An unstored local
snapshot it keeps, and uploads; loam-api deletes it through this node when nothing needs it.

## Phase 3: the network boundary

A sandbox's `network` and `secrets` (`docs/API.md`, "Network" and "Secrets") are enforced by the
nodelet of the node it runs on, in its own network namespace, outside the guest.

**Policy.** Every create, resume and restore carries the VM's `Policy`, and a running VM's can be
replaced:

```json
{"egress": [{"domain": "github.com", "action": "allow", "ports": [22, 443]}],
 "secrets": [{"name": "GITHUB_TOKEN", "value": "ghp_…", "hosts": ["api.github.com"],
              "placeholder": "__loam_github_token__", "inject": null}]}
```

`egress` is the sandbox's rules with `{"include": "defaults"}` already expanded, so every nodelet
enforces the same list whatever its version; `secrets` is every secret the sandbox names, values
included. loam-api expands and validates both; a nodelet that can't compile them answers 400
`invalid`, and one without the broker CA answers a policy with secrets 501 `unsupported`.

| Route | Body → answer |
|---|---|
| `PUT /v1/vms/{id}/policy` | `Policy` → 204. The VM must be `running` (409 `conflict` otherwise: it gets its policy at its next resume). Applies at once: connections the new rules don't allow are closed, and a changed secret is used from the next request. A running VM whose new policy can't be put in place is stopped (an `event: vm` says so), and the answer is 503 `sandbox_unavailable`; so is a running VM's resume. |

The nodelet holds a policy in memory only, and only while its VM runs: never in `--data`, a
snapshot or anything it sends the agent. A VM that pauses or stops forgets it; a nodelet that
restarts has none, which is right, because its VMs aren't running either. A create, resume or
restore whose policy can't be put in place fails, and the VM doesn't run: a VM never runs with
less than its policy.

**Egress.** The nftables table gains, per restricted VM (one with any rule), its address in a set
that sends its TCP to the nodelet's proxy (`10.240.0.1:7880`, a redirect), its DNS to the
forwarder as before, and drops everything else it sends. A VM with secrets but no rules has only
its port 443 sent to the proxy. The forwarder finds the VM by the query's source address,
answers `REFUSED` for a name the policy doesn't allow (and for `ANY` and zone transfers), empty
for `AAAA`, `HTTPS` and `SVCB` (their address hints would skip the filter, their ECH keys would
hide the name the proxy reads; a ClientHello with the ECH extension is judged by its outer name,
as Chrome sends one, keyless, on every connection), strips private addresses from answers, and remembers each address it answered the VM, with its TTL and the
name it was for, up to 4096 per VM. The proxy reads the connection's original destination;
on 80 and 443 it reads the `Host` header or the ClientHello's SNI, checks the name, and dials
an address it resolves itself (the original destination when that is among them), checked
against the blocked ranges, because the proxy's own connection leaves from the pod and the
forward chain never sees it. On other ports it dials the original destination if the VM learnt
it from a name allowed on that port. A new policy drops what the VM learnt from names it no
longer allows and closes the connections they carried. At most 2048 connections per VM.

**Brokering.** On 443, a server name that matches a secret's host is terminated in the proxy
with a leaf certificate for that name, signed by the broker CA; every other connection is
spliced on as it is. Inside, each request (HTTP/1.1 or HTTP/2, as the guest negotiated) goes out
on a connection of its own to the host, with full certificate checks, no pooling (a pooled
connection the origin closed would make a streamed body unreplayable), no redirects followed,
`Accept-Encoding: identity` asked for, the placeholder replaced or the header set, and the
answer streamed back through a scan for the values of the VM's secrets. An answer of known
length up to 64 KiB is read whole before its head goes back, so a value in it is a `502`; a
longer or streamed one is scanned as it goes, holding back only what could be the start of a
value, and reset where one appears. A WebSocket upgrade is rewritten like any request, then
spliced. Logs carry the method, host, path and status, never a query or a header value. The CA is
the Secret `loam-broker-ca` (`tls.crt`, `tls.key`, a long-lived root, whose certificate must be a
CA's), mounted into every nodelet (`--broker-ca <dir>`, or `$LOAM_BROKER_CA`), so a sandbox that
moves keeps trusting it; leaves (ECDSA P-256) are made on demand and cached (1024, by name, a
7-day life, made again a day before they end). A nodelet started with `--broker-ca` that can't
load it doesn't start; one without the flag runs with `broker: false`.

## The agent stream

`GET /v1/vms/{id}/agent` with `Connection: Upgrade` and `Upgrade: loam-agent` answers
`101 Switching Protocols`; from then on the connection is a byte stream to the agent's vsock
port 1024, on which loam-api speaks HTTP/1.1 (one request, or a WebSocket, per stream). The VM
must be `running`: anything else is 409 `conflict` before the upgrade. loam-api forwards the
sandbox sub-API over these streams unchanged (`nodeapi.AgentTransport`).

A stream lasts no longer than the VM's memory does: a pause or a stop ends it with the VM's
process, and a memory snapshot of a running VM closes every stream open into it, since taking it
resets the guest's vsock connections without a word to the host. A disk-only snapshot leaves them.

## Events

`GET /v1/events` is Server-Sent Events: `event: vm`, `data: VM` whenever a VM's state changes
other than by a request (it crashed, the guest powered off, the node drained), and in phase 2
`event: snapshot`, `data: Snapshot` when a snapshot is stored, and `event: node`, `data: Node`
when a drain starts. loam-api reconciles its records
from these and, on (re)connect, from `GET /v1/vms` and `GET /v1/snapshots/{id}` of the snapshots
it has not heard are stored. The stream carries a comment line every 15 s when idle; loam-api
drops a stream that has said nothing for 45 s and reconnects, since a frozen nodelet's connection
stays open and only a reconnect reconciles what changed while it was silent.

## Restarts

The nodelet keeps its VMs' records in `--data`, so a new nodelet pod finds the VMs the old one
had: paused ones still `paused`, stopped ones `stopped`, and running ones `stopped` (their
memory went with the old pod's processes). On SIGTERM it pauses every running VM, in parallel,
before it exits (`--drain-timeout`, default 280 s, inside the pod's 300 s grace), so rolling the
DaemonSet loses no awake sandbox; one that can't be paused in time is stopped. Uploads the
old nodelet didn't finish, the new one resumes, parents first. What phase 1 left is kept: a VM
paused into its own directory resumes from it (and has no `snapshot`), and a checkpoint without
layers is stored whole, as one layer.

## The guest's host channel

The agent can call loam-api for its own sandbox (`GET /self`, `POST /checkpoints`, `GET /identity/token`; `docs/API.md`,
"Inside a sandbox") by connecting from the guest to host vsock port 1025. The nodelet accepts
those and forwards each HTTP request to loam-api at `/internal/guest/<path>` with the headers
`Authorization: Bearer <node token>` and `X-Loam-Sandbox: <id>`. The VM is the identity: the
nodelet sets the header from which VM's socket the request came in on, and drops any copy the
guest sent.

## Inside the nodelet

- **Network.** Every VM has a tap on the bridge `loam0` (10.240.0.0/16, gateway 10.240.0.1, MAC
  `06:00:0a:f0:00:01` on every node, so a sandbox that wakes elsewhere still has its gateway's MAC) in the
  nodelet pod's own network namespace, NATed out of the pod. nftables in that namespace drops VM
  traffic to private, link-local, CGNAT (Tailscale) and multicast ranges and between VMs, and
  allows the rest of the internet. Each tap may send only from its VM's address (IPv4 and ARP),
  since the forwarder, the proxy and the rules all know a VM by its source address. The pod's NetworkPolicy is the second layer. DNS for VMs is a
  forwarder on 10.240.0.1:53 to public resolvers (`--dns-upstream`, default `1.1.1.1,9.9.9.9`),
  which applies a restricted VM's policy, as does the proxy on 10.240.0.1:7880 ("Phase 3: the
  network boundary"). Nothing here touches the host's network.
- **Storage.** `--data` (a hostPath such as `/bulk/loam`) must support reflinks (XFS with
  `reflink=1`, btrfs); the nodelet checks at start and refuses otherwise. On an ext4 node, an
  XFS image file mounted loop-back works (`docs/INSTALL.md`).
- **Images.** Template images are pulled with go-containerregistry (no podman; credentials from
  the default keychain, i.e. `$DOCKER_CONFIG/config.json`), flattened, and written to ext4 with
  `mke2fs -d`, once per digest and `disk_gib` (the filesystem is made at full size, so no VM
  spends its first boot zeroing inode tables into its disk). The image's config `Env` goes to
  `/etc/loam/image.json` on the disk (`docs/AGENT.md`, "The guest disk"). Every use refreshes a
  cached file's mtime; hourly, files unused for `--image-cache-max-age` (7 days) go, then the least
  recently used until the cache is under `--image-cache-max-gib` (20), never one used in the last
  hour. VMs are unaffected: each disk is a reflink clone of its own. The kernel and the agent's initramfs ship in the nodelet's own
  container image, so updating the agent is rolling the nodelet; a sandbox picks up the new agent
  at its next fresh boot.
- **Confinement.** Firecracker's seccomp filters on; no jailer in phase 1 (the pod is the jail).
  Each Firecracker process also gets its own cgroup under the pod's (CPU, memory and pid limits
  from its resources) where the pod lets the nodelet make one: a writable cgroup2, or
  `CAP_SYS_ADMIN` in a private cgroup namespace, which an unprivileged pod has neither of. Without
  that, the nodelet says so in its log at start and the VMs share the pod's cgroup, bounded only by
  the pod's limits; admission still holds them to `--memory`.


### Resolved request headers

`Policy.request_headers` carries `{domain, headers, unavailable?}` rules resolved by
loam-api. `Policy.sensitive` carries the referenced secret values for the broker's
response reflection guard. Both remain in nodelet memory; neither reaches the agent
or stored VM snapshots. Rules require the broker CA, do not add egress allowances,
and select only the most specific matching domain. An unavailable rule refuses its
requests. The nodelet validates header names, domains and values independently.
Adding/removing interception closes existing proxied connections whose broker route
changes; existing brokered connections read the current policy for every request.

## Guest firewall support

The shipped x86_64 guest kernel includes nftables inet filtering and UID-based
loopback rules (`meta skuid`), including TCP-reset rejection, plus iptables owner
matching. These are built in, independent of guest modules. Guest root controls
these rules; the nodelet's external network policy still applies. Kernel sources,
configuration provenance and validation are recorded in `images/nodelet/KERNEL.md`.
A fresh boot uses the new kernel after a nodelet rollout; a memory resume keeps
its snapshotted kernel. Rebuild running templates when upgrading kernel features.
