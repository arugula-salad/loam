# Agent protocol: loam-nodelet ↔ loam-agent

`loam-agent` is PID 1 in every VM, booted from the initramfs the nodelet ships; it mounts the
root disk, does what an init must (reap, mount `/proc` and friends, bring up `eth0` from the
kernel command line, set the clock and hostname) and serves HTTP/1.1 on **vsock port 1024**.
PID 1 itself only sets up and reaps; it runs a copy of itself (`loam-agent serve`) as the server
and restarts it if it dies.
Only the host can open a vsock stream to a guest, so the agent trusts every stream it accepts:
there is no token, no handshake and no claim. It never listens on TCP.

## The sandbox sub-API

Everything under `/v1/sandboxes/{sandbox}/` in `docs/API.md`, except `checkpoints`, is served
by the agent at the same path with that prefix removed: `/exec`, `/exec/sessions/...`,
`/exec/run`, `/fs/...`, `/ports`, `/ports/watch`, `/ports/{port}/connect`, `/services/...`,
`/tasks/...`.
Same query parameters, bodies, frames, status codes and error shape. loam-api adds nothing but
authorization and the wake, so a behaviour the e2e suite checks is the agent's.

`/ports/{port}/connect` is how loam-api also serves port URLs: it carries HTTP to the port over
that stream. Before the upgrade it starts the service whose `http_port` is that port if it isn't
running (and then gives it up to 10 s to listen); with nothing listening it answers 502
`port_closed` at once, and loam-api, which retries for up to 10 s, passes that on as the URL's
answer. Port *settings* (`access`) are loam-api's and never reach the agent.

## Internal routes (from the nodelet only)

The nodelet calls these; loam-api never forwards anything under `/internal/` to the agent.

| Route | |
|---|---|
| `POST /internal/wake` | `Wake` (`docs/NODE.md`). At every boot and every resume, before the VM is reported running. Sets the clock (a resumed guest's clock is stale), hostname, `eth0` from `network` when given, the env every new process gets, the default user, the broker CA from `ca` (below); then starts services on a fresh boot, or tells running sessions nothing on a resume. Answers 204 once done. The wake is kept in `/run/loam/wake.json` (tmpfs), so a server process that PID 1 restarts takes it up again. |
| `POST /internal/presuspend` | Before a pause or a memory snapshot. Runs the executables in `/var/lib/loam/hooks/presuspend.d` in name order, as root, 5 s for all of them together; then syncs and drops the page cache (`drop_caches`, `compact_memory`) so the balloon can reclaim it. 204. |
| `POST /internal/sync` | `sync` all filesystems; before a disk-only snapshot. 204. |
| `GET /internal/activity` | → `Activity`: `{"idle_ms": N, "sessions_attached": N, "tasks": N, "busy": bool}`. `idle_ms` is since the last byte in or out of an exec session, a file operation, a port stream or the last wake, on the monotonic clock (time paused doesn't count). A live task (`docs/API.md`, "Tasks") makes `busy` true; nothing else does. |
| `POST /internal/poweroff` | Stop services (SIGTERM, 5 s, SIGKILL), sync, power off. |

## The guest's API socket

The agent serves the sandbox sub-API to processes in the VM on `/run/loam/api.sock` (mode 0666),
without the prefix, as above. Three routes there are not the agent's own: `GET /self`,
`POST /checkpoints` and `GET /identity/token` go to loam-api through the host channel (`docs/NODE.md`, "The guest's host
channel": the agent connects to host vsock port 1025 and sends the request as is). For
`POST /checkpoints` with memory, the agent answers its caller as soon as loam-api has accepted
the request, before the pause, so the answer isn't lost in the snapshot. The agent relays the
answer the moment it arrives, so this needs loam-api to send its whole answer (`201` and the
`Checkpoint`) before it asks the nodelet for the snapshot: an answer that only comes after the
resume would be lost in a sandbox later restored from that checkpoint.

## The broker CA

A wake's `ca` is the root certificate the sandbox's node signs brokered hosts' certificates with
(`docs/API.md`, "Secrets"). The agent writes it to `/etc/loam/ca.pem` and puts it, between
`# loam broker CA` marker lines it replaces in place, at the end of each system bundle the disk
has: `/etc/ssl/certs/ca-certificates.crt` (Debian, Ubuntu, Alpine, Arch),
`/etc/pki/tls/certs/ca-bundle.crt` (Fedora, RHEL) and `/etc/ssl/cert.pem`. Every new process
gets `NODE_EXTRA_CA_CERTS=/etc/loam/ca.pem`; and, where a bundle was found, `SSL_CERT_FILE`,
`REQUESTS_CA_BUNDLE` and `CARGO_HTTP_CAINFO` pointing at it, and `UV_NATIVE_TLS=1`, unless the
image or the sandbox sets them. A package update that rewrites a bundle loses the root until the
next wake. A wake without `ca` removes nothing: a root once installed is harmless, as only the
nodes hold its key. A `ca` that isn't PEM certificates is refused, 400 `invalid`, before anything
else in the wake is applied. A bundle that is a symlink is written through to the file it names,
once however many names it has.

## Users and processes

- The guest image has a user `loam` (uid 1000, passwordless sudo) and `root`. Exec, services and
  file operations default to `loam` / `loam` / `root` (see `docs/API.md`); the wake's `user`
  replaces `loam` as the default for exec and services. Any account on the disk may be named; one
  that doesn't exist is 400 `invalid`.
- A file operation with `user=` runs with that user's filesystem uid, gid and groups (set on one
  thread for the operation), so the kernel's permission checks are that user's.
- A process gets: the broker CA's variables, then the image's environment, then the sandbox's
  `env` from the last wake (with its secrets' placeholders), then the call's `env`; `HOME`, `USER`, `LOGNAME`, `SHELL` for its user; `TERM=xterm-256color` with a TTY.
  `PATH` defaults to the usual system directories and ends with the user's `~/.local/bin`.
- Every exec session is its own process group and session; signals go to the group.

## The guest disk

What the agent reads from and keeps on the sandbox's disk (which is what travels in snapshots):

| Path | |
|---|---|
| `/etc/loam/image.json` | `{"env": ["K=V", ...], "entrypoint"?, "cmd"?, "workdir"?, "user"?}`: the template image's config `Env`, `Entrypoint`, `Cmd`, `WorkingDir` and `User`. The nodelet writes it when it flattens the image; absent means none. `HOME`, `USER`, `LOGNAME`, `SHELL`, `HOSTNAME` and `TERM` in `env` are ignored (the agent sets them per user). The agent reads only `env`: loam runs nothing the image says to run, and the rest is there for a client that makes the image's program a service (`GET /v1/sandboxes/{sandbox}/files?path=/etc/loam/image.json`). |
| `/var/lib/loam/services/<name>.json` | Service definitions. An image may ship some, which then start at every boot like any other (the `e2b` template's envd, `docs/E2B.md`). |
| `/var/lib/loam/logs/services/<name>.log[.N]` | Service output, rotated at 8 MiB, 2 kept; `/var/lib/loam/logrotate.json` (`{"max_bytes","keep"}`) changes that. |
| `/var/lib/loam/hooks/presuspend.d/` | Executables run before a pause or a memory snapshot. |

On tmpfs, gone at every boot: `/run/loam/api.sock` (the guest API), `/run/loam/wake.json`, and
the services' pid files in `/run/loam/services`.

## Ports

Found by polling `/proc/net/tcp` and `/proc/net/tcp6` every 500 ms (there is no unprivileged
event for a LISTEN), attributed to a session by walking `/proc/<pid>/fd` for the socket inode and
up the parent chain to a session leader. The agent needs `CAP_SYS_PTRACE` for that, which as
root in its own VM it has (without it, under gVisor, the walk found nothing).

## Booting: what the nodelet provides

- **Initramfs**: `scripts/build-initramfs.sh` → `dist/initramfs.cpio.gz` (gzip, newc, reproducible),
  holding only the static agent as `/init`. Firecracker's `boot-source.initrd_path`.
- **Root disk**: an ext4 block device, the first drive (`/dev/vda`), with `is_root_device: false`:
  the agent mounts it and pivots to it itself.
- **Kernel command line**: Firecracker's usual `console=ttyS0 reboot=k panic=1 pci=off` (`reboot=k`
  matters: poweroff is a reset through the keyboard controller, which makes Firecracker exit, as
  it has no ACPI poweroff; `i8042.noaux i8042.nomux i8042.dumbkbd` keep the probe quiet), plus the
  agent's own parameters:

  | Parameter | |
  |---|---|
  | `loam.root=/dev/vda` | The disk to mount as `/`. Without it the agent stays on the initramfs, which is only good for debugging. |
  | `loam.ip=10.240.0.7/16` | `eth0`'s IPv4 address with its prefix length. Without it `eth0` stays down until a wake carries `network`. |
  | `loam.gw=10.240.0.1` | The default route. |
  | `loam.dns=10.240.0.1` | Resolvers for `/etc/resolv.conf`, comma-separated. |
  | `loam.hostname=scratch` | Optional; the wake sets the hostname anyway. |

  `eth0`'s MAC is Firecracker's `guest_mac`; the agent changes it only when a wake's
  `network.mac` differs (a memory snapshot resumed on another VM).
- **vsock**: Firecracker's `vsock` device, any guest CID. The nodelet reaches the
  agent through the device's Unix socket (`uds_path`) by writing `CONNECT 1024\n` and reading
  `OK <port>\n`; from then on the socket is the stream. The guest's host channel arrives as a
  connection on `<uds_path>_1025`, which the nodelet must listen on.
- `scripts/boot-test.sh` boots exactly this under Firecracker in a container and checks it.

## Carried from sandpit

The agent started as the guest agent of sandpit, the single-host Firecracker sandbox system loam
replaced. Carried over: the session engine (replay buffer, backpressure on the reader, process groups), the port watcher,
the services supervisor (`needs`, crash backoff, rotating logs, definitions on the guest disk so
they travel with snapshots), tasks, the file API and the inotify cap. Not carried: the TCP and
upgrade listeners, `ClaimListener`, `ReverseHost`, `/control`, the Sprites and envd framings,
checkpoint mounts and the network policy file (phase 3 enforces policy outside the guest); also
the privileges policy (capability bounding, `no_new_privs`, the workload memory cgroup), the
in-guest `sprite-env` CLI and its setuid sudo stand-in (the guest image has a real sudo), system
services declared by the image, lease-expiry hooks, and service event reports to the host.
