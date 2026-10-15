# Templates

A template is a named starting point for a sandbox: a guest image and default resources. They are
set up by your loam operator; there is no API to build one. To start from an image of your own,
use `image` at create ([Sandboxes](sandboxes.md#images)); to start from a sandbox you prepared,
checkpoint it and [fork](checkpoints.md#forks) it.

| Method and path | |
|---|---|
| `GET /v1/templates` | → `{"templates": [{"name", "image", "resources"}]}` |

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" "$LOAM_API_URL/v1/templates"
{"templates":[{"name":"base","image":"ghcr.io/arugula-salad/loam-guest@sha256:a0a0513ee4a4223deca9edab3fe9a93297c5ca87ef04a9709bf6e4f3fe06fc76","resources":{"cpus":2,"memory_mib":2048,"disk_gib":20}},{"name":"e2b","image":"ghcr.io/arugula-salad/loam-e2b@sha256:2d3516ea7948ab2216f5f35f8d82f2a9220e11dff75539ab330477460cbbfd14","resources":{"cpus":2,"memory_mib":1024,"disk_gib":20}}]}
$ loam templates
NAME  IMAGE                                                                                                     CPUS  MEM_MIB  DISK_GIB
base  ghcr.io/arugula-salad/loam-guest@sha256:a0a0513ee4a4223deca9edab3fe9a93297c5ca87ef04a9709bf6e4f3fe06fc76  2     2048     20
e2b   ghcr.io/arugula-salad/loam-e2b@sha256:2d3516ea7948ab2216f5f35f8d82f2a9220e11dff75539ab330477460cbbfd14    2     1024     20
```

## The templates here

**`base`**, the default: Ubuntu 24.04 with Python 3.12 (and `pip`), Node 24 (and `npm`), git,
curl, wget, jq, sudo and the usual command-line tools. The user `loam` (uid 1000, passwordless
`sudo`, home `/home/loam`) runs commands and services by default; `root` is there too. `/tmp` is a
tmpfs (memory), so it survives a pause but not a stop.

**`e2b`**: the image the [E2B adapter](e2b.md) uses, E2B's `base` reshaped for loam: Ubuntu 24.04,
Node 22, Python, the user `user` (uid 1000, home `/home/user`) for E2B's SDKs and `loam` (uid
1001) for loam's own exec, and E2B's in-guest daemon `envd` as a service. You can make loam
sandboxes from it like any template.

A template's resources are the defaults; give `resources` at create to change them. A template's
image sets the sandbox's environment beneath its `env` (`DEBIAN_FRONTEND=noninteractive` and
`LANG=C.UTF-8` in `base`).
