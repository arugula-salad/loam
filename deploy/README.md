# Deploying loam

[docs/INSTALL.md](../docs/INSTALL.md) is the guide: what a cluster needs, and every step from the
device plugin to your first API key.

| Path | What |
|---|---|
| `base/` | the kustomize base: namespace `loam`, loam-api (Deployment, Service), the nodelet (DaemonSet, headless Service, PriorityClass), NetworkPolicies, and the `loam-templates` and `loam-domains` ConfigMaps. Nothing in it is tied to one cluster; `deploy_test.go` keeps it so |
| `base/nodelet/` | the nodelet DaemonSet alone, with its image pin, so an overlay can instantiate it again for nodes of another size |
| `base/pinned-source` | the source key the pinned digests were built from; written only by the pin |
| `examples/basic/` | an overlay that works on a plain cluster: your domain, your S3 bucket, an Ingress, and the Secrets' shapes (`secrets.example.yaml`) |
| `components/e2b-fountain/` | an optional component adding a second E2B template (`images/e2b-fountain`) |

## Image pins

`.github/workflows/images.yml` builds the images and pins their digests into `base/` (and the
templates of `components/`) **on the pull request's branch**, so the commit that merges carries
the code, its `base/` changes (a new loam-api flag, say) and the images built from them, and
whatever applies `base/` from `main` gets them as one revision. `base/pinned-source` records the
source key the pins were built from (`scripts/source-key.sh`); every pin rewrites it, so a PR
pinned before another one merged conflicts there: rebase it, and its rerun builds and pins on
top. A push to `main` whose sources are already pinned builds nothing; one that isn't builds and
pins on `main` itself.

Merge a PR that touches `cmd/`, `internal/`, `pkg/`, `images/` or the user docs only once its
`deploy(loam): pin images for #N` commit is its head; until then its images aren't pinned.

To follow `main` from your own overlay, point it at `base/` at a commit, not at a branch:
`https://github.com/arugula-salad/loam//deploy/base?ref=<commit>`.
