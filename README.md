# loam

Sandboxes on Kubernetes, with an E2B-compatible API.

loam runs each sandbox as a Firecracker microVM, and keeps each one as a record in Postgres. A
sandbox runs on a long-lived worker pod (a nodelet, one per KVM node) only while it is in use.
When it goes idle, loam snapshots its memory and disk to S3-compatible storage and frees the
node, and the next request wakes it on whichever node has room, with its processes as they were.
You drive it through a native HTTP API, the `loam` CLI, Go and TypeScript clients, or the
official E2B SDKs, unmodified.

Project page: https://loam.inevitable.fyi

## Why

E2B has a good API for agent sandboxes. Its own self-hosted setup runs on Nomad, Consul and
Terraform. The Kubernetes projects that speak E2B's API run each sandbox as a pod, which ties
every wake to the scheduler and keeps idle sandboxes holding memory. loam is for running
Firecracker sandboxes on the Kubernetes cluster you already have, where a sleeping sandbox is a
snapshot in object storage and costs nothing on any node.

## Numbers

Measured on the reference deployment, a three-node home k3s cluster on x86 bare metal with
Garage as the snapshot store, using the base template (2 vCPUs, 2 GiB).

| What | Result |
|---|---|
| Resume a paused sandbox on the same node | 29 ms |
| Wake on another node, memory loaded lazily | 0.42 s |
| Fork from a memory checkpoint | 28 ms |
| Pause, memory and disk | 320 ms |
| Drain a node with two awake sandboxes | 13 s |
| Official E2B SDKs, Python and JS | 26 of 26 conformance steps |

The methodology is in the repo's test write-ups. [docs/PLAN.md](docs/PLAN.md) has the results
of each phase, [docs/PHASE2-TEST.md](docs/PHASE2-TEST.md) and
[docs/PHASE3-TEST.md](docs/PHASE3-TEST.md) are the multi-node and network boundary runs, and
[docs/E2B.md](docs/E2B.md#conformance) describes the SDK conformance suite in `e2e/e2b`.

## Quickstart

With a loam deployment that serves the E2B adapter, point the E2B SDK at it and use a loam
project key as the E2B key.

```sh
pip install e2b
export E2B_DOMAIN=e2b.loam.example.com
export E2B_API_KEY=loam_...
```

```python
from e2b import Sandbox

sbx = Sandbox.create()
print(sbx.commands.run("uname -a").stdout)
sbx.pause()      # a snapshot in the store, the VM gone from its node
sbx.connect()    # wakes it, on any node, with its processes
sbx.kill()
```

The native CLI does the same and more (exec sessions, port URLs, checkpoints, forks, egress
rules, brokered secrets).

```sh
go install github.com/arugula-salad/loam/cmd/loam@latest
export LOAM_API_URL=https://api.loam.example.com LOAM_TOKEN=loam_...
loam create scratch
loam run scratch -- uname -a
loam pause scratch
loam run scratch -- uptime     # wakes it first
```

To run loam yourself, start with [docs/INSTALL.md](docs/INSTALL.md). You need x86_64 nodes with
KVM, Postgres, an S3-compatible bucket, an XFS data directory with reflinks on each node, and
wildcard DNS and TLS for the sandboxes' URLs. To try it on one machine without a cluster, see
"Locally" in [docs/E2B.md](docs/E2B.md#conformance).

## Docs

- [docs/INSTALL.md](docs/INSTALL.md) for requirements and installing on a cluster
- [docs/primer.md](docs/primer.md) to get from a key to a working sandbox
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the components and the principles behind them
- [docs/API.md](docs/API.md), the native API contract, with a user reference in [docs/reference](docs/reference/index.md)
- [docs/E2B.md](docs/E2B.md), what of E2B's API the adapter serves and how it differs from hosted E2B
- [docs/STORE.md](docs/STORE.md), the snapshot store
- [docs/NODE.md](docs/NODE.md) and [docs/AGENT.md](docs/AGENT.md), the internal protocols

## Status and limitations

loam is young. It was built in October 2026 and runs one deployment in production so far.

- loam-api runs as a single replica, so restarting it briefly interrupts the API and the
  sandboxes' URLs.
- The nodelet runs unprivileged, and in that mode its VMs share the pod's cgroup. There are no
  per-VM CPU or memory limits beyond admission against the nodelet's `--memory`.
- x86_64 with KVM only.
- Linux 6.1 or later on the nodes, for `/dev/userfaultfd`.
- The E2B adapter has gaps. There is no template build API, no E2B snapshot or fork endpoints, no
  volumes, no sandbox logs or metrics history, and egress rules take names only (CIDR rules are
  refused). The full list is in [docs/E2B.md](docs/E2B.md#not-here).

## How this was built

loam was written mostly by AI coding agents (Claude Code), working from Jake Gaylor's design and
under his review. It replaced his earlier single-host Firecracker sandbox system and runs his own
workloads in production, including browser games, hosted dev boxes, CI and an E2B endpoint.

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md). Report vulnerabilities privately as described in
[SECURITY.md](SECURITY.md).

## License

Apache License 2.0, in [LICENSE](LICENSE). The snapshot store is derived from
[e2b-dev/infra](https://github.com/e2b-dev/infra), also Apache-2.0. See [NOTICE](NOTICE).
