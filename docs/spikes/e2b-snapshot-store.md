# Spike: E2B's snapshot store (phase 0, 2026-10-11)

e2b-dev/infra at cf536c2. Program and run output kept outside the repo; findings below.

## Verdict: copy and trim the packages, don't import them

Verified by running, with E2B's packages unmodified: a base memfile plus a diff layer with
headers, uploaded (zstd, lz4, raw) to a temporary Garage bucket over path-style S3, read back by
offset through E2B's read path (header mapping → diff → mmap chunk cache → shared chunk dir →
S3) byte-identical, served lazily to another process through E2B's UFFD handler, and a
copy-on-write overlay on top exported as a generation-2 diff header.

Importing it as a module is not worth it: 810–1006 non-stdlib packages (GCS, Azure,
LaunchDarkly, OTel, gRPC, redis, client-go, pgx), a 75 MB binary, no published versions, heavy
churn (27 commits to storage in 3 months), cgo, feature-flag and config coupling, and pause
depends on E2B's private Firecracker fork for the dirty-memory set.

Copy about 12k lines (Apache-2.0: keep LICENSE, mark changed files): S3 and local backends only,
static config instead of feature flags, no-op telemetry, no peer client, `x/sys/unix` instead of
the cgo constants. Keep the header format, frame tables, Chunker/Cache/Overlay and the UFFD serve
loop. The dirty-page source for pause is ours.

Running E2B wholesale: still no (their Firecracker fork and kernels, Redis, ClickHouse, Nomad/k8s
discovery, 4096 NBD devices per node, envd in every guest).

## Shape for loam's phase 2 snapshot store

1. **A flattened header per generation** (E2B's): offset → (layer, offset in layer) plus Empty ranges. One header GET per resume, on any node; long chains cost nothing at read time.
2. **One object per layer**, dirty blocks packed, **2 MiB independently compressed frames** (zstd level 1: 64 MiB → 0.97 MB in the test, lz4 1.23 MB), the frame table in the header. A miss is one ranged GET with streaming decompression.
3. **No chunk content addressing yet.** Dedup comes from ancestors; GC is refcounting layer IDs from headers' build maps, which fits Postgres.
4. **Two node cache tiers**: a sparse mmap file per layer with fetch coalescing, and a node-local chunk directory evicted by TTL and disk pressure. Measured: a second "node" with a warm chunk dir made 0 S3 fetches (43 ms vs 1.9 s cold through a port-forward).
5. **Provisional headers**: resume from the local copy right after a pause while the upload runs, then swap in the durable header.
6. **A prefetch list** recorded from a template's first boot, replayed on resume.
7. **Firecracker stays upstream**: UFFD backend for lazy memory; the dirty set from diff snapshots (`track_dirty_pages`, scan the sparse file with SEEK_DATA) or a pagemap scan.

## Requirements found

- **userfaultfd**: `WP_ASYNC` needs Linux ≥ 6.7; `EVENT_REMOVE` with the balloon. Unprivileged `USER_MODE_ONLY` doesn't catch KVM's faults, so Firecracker needs the privilege (`vm.unprivileged_userfaultfd=1`, `CAP_SYS_PTRACE`, or `/dev/userfaultfd`). The handler must run in a different process from the memory owner (in-process it deadlocks the Go GC).
- **Rootfs copy-on-write transport**: NBD (the kernel module with `nbds_max`, a host change on every node) or vhost-user-blk, or a local reflink copy without lazy disk loading. Decide separately.
- **Chunk cache** on a reflink filesystem (one node had XFS with reflinks; the others needed one).
- **Garage** works unchanged: `s3://<bucket>?endpoint=…&s3ForcePathStyle=true&region=<region>`.
