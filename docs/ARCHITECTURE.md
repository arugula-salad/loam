# loam architecture

A sandbox is a record, not a pod. A few long-lived pods (one **nodelet** per KVM node) hold the
capacity; a sandbox runs on one only while it is awake, as a Firecracker microVM. An idle
sandbox becomes a snapshot and wakes on the next request. Clients never see which node it is on.

The design draws on an earlier single-host Firecracker sandbox system that loam replaced, on
running sandboxes as Kubernetes pods, and on E2B's snapshot store (`docs/STORE.md`).

```
clients ──► loam-api ──(node protocol, docs/NODE.md)──► loam-nodelet (one per KVM node)
  (API key)   │  Postgres                                 │  Firecracker VMs, taps, storage
              │                                           │
wildcard ─► Traefik ─► loam-api (port URLs)               └─vsock─► loam-agent (PID 1 in each VM)
hosts                                                        (docs/AGENT.md)
```

## Components

| Binary | Runs as | Job |
|---|---|---|
| `loam-api` (`cmd/loam-api`, `internal/apiserver`) | Deployment in namespace `loam`, Postgres from CNPG | The public API (`docs/API.md`), authorization, placement, lifecycle rules (idle, deadline), events, the port URLs and the traffic through them (`docs/API.md`, "Stats": per-day counts and visitors in Postgres, Prometheus metrics on a listener of its own, an access log line per request). Owns every record. With `--e2b-domain`, also E2B's API (`internal/e2b`, `docs/E2B.md`): a client of loam's own API in the same process, so E2B sandboxes are loam sandboxes. |
| `loam-nodelet` (`cmd/loam-nodelet`, `internal/nodelet`) | DaemonSet on the nodes labelled for it (`docs/INSTALL.md`), with `/dev/kvm`, `NET_ADMIN` and a hostPath for its data | Runs the VMs of its node: boot, pause, snapshot, restore, stop, delete; their network inside its own pod network namespace; their disks; the image cache. Knows nothing about projects or API keys. |
| `loam-agent` (`cmd/loam-agent`, `internal/agent`) | PID 1 in every VM, from the initramfs | Exec sessions, files, ports, services, activity. Reached only over vsock. |
| `loam` (`cmd/loam`) | A user's machine | The CLI over `docs/API.md`. |

## Principles

- **Sandboxes are not pods.** Waking a sandbox never waits on the Kubernetes scheduler or etcd.
- **Every state is a snapshot.** A paused sandbox, a checkpoint and a fork are snapshots; a snapshot says whether it holds memory.
- **Refuse what you can't enforce.**
- **The network is the boundary.** A VM reaches the internet and nothing else: not the node, not the pod network, not the LAN (phase 1). Allowlists and credential brokering (phase 3) are enforced outside the guest.
- **One agent, one protocol.** The agent's HTTP API *is* the sandbox sub-API; loam-api checks authorization and forwards it unchanged.
- **The database is the truth.** loam-api's Postgres holds every record. The nodelet's state is a cache of what it runs, reconciled against the records.
- **Resolve, don't remember, addresses.** loam-api reaches a nodelet by node name through the headless Service every time, never by a stored IP (a pod-based design that remembered worker IPs taught this one).

## Lifecycle

States: `creating`, `running`, `paused` (memory and disk kept; wakes with processes intact),
`stopped` (disk only; wakes with a fresh boot on the same disk), `error`; deletion removes the
record. Rules: an idle rule (timeout and action `pause`, `stop` or `none`), a deadline (time and
action `delete`, `pause` or `stop`) and `protected`.

Any call on a sandbox's sub-API wakes it: loam-api *acquires* the sandbox (wakes it if needed,
counts the use) for the length of the call. A sandbox is idle when no call is in flight, no exec
session is attached, the agent reports no activity, and the idle timeout has passed.

## Phases

1. **One node, native API**: loam-api, Postgres, one nodelet, the agent on
   vsock, the CLI, the e2e suite. Snapshots are local to the node, so a sandbox always wakes
   where it slept.
2. **Many nodes**: snapshot layers in Garage, lazy restore, a layer cache, placement across nodes, pause on drain.
3. **The network boundary**: DNS and SNI allowlists, credential brokering, per-sandbox identity.
4. **E2B adapter and migration** of the reference deployment's workloads: docs/PLAN.md, "Phase 4".
   The adapter's contract is docs/E2B.md.

## Not in loam

The vendor front ends but E2B (`docs/E2B.md`); agent transports other than vsock; the CLAIM handshake and reverse
host channel (vsock is host-only, and secrets arrive at every wake); the chunked backup tier
(snapshots are the durable copy from phase 2); custom domains and ACME (cert-manager and
Traefik); the web UI; Landlock confinement and the host network install (the pod is the
confinement, and the nodelet sets up its VMs' network inside its own network namespace);
memory-balloon autoscale in phase 1 (the balloon itself stays: it makes snapshots small).
