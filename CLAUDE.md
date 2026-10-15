# CLAUDE.md

Guidance for coding agents (and people) working in this repository. `CONTRIBUTING.md` has the
same rules in short.

loam runs sandboxes on Kubernetes: Firecracker microVMs placed on warm worker pods ("nodelets"),
records in Postgres, snapshots in S3-compatible storage that let a sandbox sleep and wake on any
node. Read `docs/ARCHITECTURE.md` first, then the contract for the part you touch.

## Contracts

- `docs/API.md`: the public API (`pkg/api` holds its Go types). The CLI, the Go and TypeScript clients and the e2e suite are written against it.
- `docs/NODE.md`: loam-api ↔ loam-nodelet (`internal/nodeapi` holds the types, the client and the stream helpers).
- `docs/AGENT.md`: loam-nodelet ↔ loam-agent over vsock, and the agent's HTTP API, which is the sandbox sub-API of `docs/API.md` minus its prefix.
- `docs/E2B.md`: the E2B adapter in loam-api (`internal/e2b`), what of E2B's API it serves and how it maps onto loam.
- `docs/STORE.md`: the snapshot store (`internal/snapstore`).

The contracts are the source of truth. If one is wrong or missing something, change the doc and
its Go types in the same PR, say so in the PR description, and keep the change as small as the
fix needs. Never let code and contract drift apart.

The user docs, `docs/primer.md` and `docs/reference/*.md`, are embedded in loam-api and served
at `/docs` (`docs/embed.go`, `internal/docsite`). Each page starts with its `# Title`. A change to
user-visible behaviour updates them in the same PR.

## Layout

| Path | What |
|---|---|
| `cmd/loam-api`, `internal/apiserver`, `internal/db` | The API server, placement, lifecycle, Postgres and its migrations |
| `cmd/loam-nodelet`, `internal/nodelet` | The per-node worker: Firecracker, the VMs' network, storage, the credential broker |
| `cmd/loam-agent`, `internal/agent` | PID 1 in every VM, reached over vsock |
| `cmd/loam`, `pkg/client`, `pkg/api` | The CLI, the Go client and the API's types |
| `internal/snapstore` | The snapshot store, derived from e2b-dev/infra (see `NOTICE`, and keep the per-file headers) |
| `internal/e2b` | The E2B adapter |
| `clients/ts` | The TypeScript client |
| `images/` | Container images: api, nodelet (with Firecracker and the guest kernel), guest root filesystems |
| `deploy/` | The Kustomize base and an example overlay (`docs/INSTALL.md`) |
| `e2e/` | The end-to-end suite against a running loam, and the E2B SDK conformance run (`e2e/e2b`) |
| `scripts/` | The initramfs build, the KVM test runner, boot tests |

## Build and test

```sh
make build        # every binary into bin/
make check        # go vet and go test ./...: what CI runs
make kvm-test     # the nodelet on real Firecracker, in a container with /dev/kvm
cd clients/ts && npm ci && npm test
```

- `go test ./...` must pass without root, KVM or a network. Anything that needs them is behind a build tag: `kvm` (`scripts/kvm-test.sh`), `garage` (a real S3 bucket, `docs/STORE.md`) or `e2e` (a running loam, `e2e/e2e_test.go`).
- The store's SQL is tested against Postgres when `LOAM_TEST_DATABASE_URL` is set, as CI does. Without it only the in-memory store is tested.
- Never `sudo` on a shared host. Anything that needs root or KVM runs in a container shaped like the nodelet pod: `docker run --rm --device /dev/kvm --device /dev/net/tun --device /dev/userfaultfd --cap-add NET_ADMIN ...`, as `scripts/kvm-test.sh` does.
- Code is `gofmt` clean, which CI checks.

## Conventions

- Go 1.27, module `github.com/arugula-salad/loam`. Standard library first. A new dependency needs a reason in its PR.
- Comments are full sentences that say why, at the density of the surrounding code. No TODO markers: write down what is missing in the doc that owns it.
- Refuse what you can't enforce: an option the code can't honour is an error, never silently dropped.
- Errors over the API use the shape in `docs/API.md` ("Errors").
- A bug fix comes with a test that fails without it.
- Nothing deployment-specific in code defaults or docs: use `example.com` hosts in examples, and put a deployment's settings in its overlay.

## Git

- One PR per piece of work, squash-merged.
- Commit messages: a short subject in the imperative, a body that says why.
