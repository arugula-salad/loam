# Installing loam

loam installs from a kustomize base, `deploy/base`, plus an overlay of your own that sets your
domain and your snapshot bucket and adds an ingress. `deploy/examples/basic` is a working overlay to
copy. This page is the whole path: what the cluster needs, the install, then a project, a key and
the E2B SDK pointed at it.

## What you need

| Need | Why | Notes |
|---|---|---|
| **x86_64 nodes with KVM** | every sandbox is a Firecracker microVM | Bare metal, or a cloud instance with nested virtualization (`/dev/kvm` must exist on the node). arm64 isn't built. |
| **Linux 6.1 or newer** on those nodes | `/dev/userfaultfd`, for restoring a snapshot's memory lazily | Pods can't call `userfaultfd(2)` under the RuntimeDefault seccomp profile; the device is the way in. |
| **A device plugin** exposing `/dev/kvm`, `/dev/net/tun` and `/dev/userfaultfd` | the nodelet is not privileged, so the kubelet has to hand it exactly these devices | Below: squat's generic-device-plugin. |
| **A directory on each node, XFS with reflinks** | VM disks, snapshots and caches are reflink clones of each other | `/var/lib/loam` by default. An XFS image file on another filesystem works (below). |
| **An S3-compatible bucket** | every pause and checkpoint is a snapshot in it, which any node can wake | AWS S3, MinIO, Garage, R2... Path-style works. |
| **Postgres** | loam-api's records: projects, keys, sandboxes, snapshots | Postgres 18 is what loam is tested on. loam-api creates its schema. |
| **Wildcard DNS and TLS** | the API and every sandbox's URLs are names under your domain | `*.loam.example.com` and `*.e2b.loam.example.com`, each with a wildcard certificate. |
| **An ingress controller** | to reach loam-api | Any; it must allow long-lived connections and WebSockets. |

Kubernetes itself: loam is developed on k3s 1.36 (Linux 7.0 nodes); it uses nothing beyond Deployments,
DaemonSets, NetworkPolicies and extended resources.

The names below assume your domain is `loam.example.com`: the API at `https://api.loam.example.com`,
sandbox URLs at `https://<name>--<project>.loam.example.com`, and E2B's at `*.e2b.loam.example.com`.

## 1. The nodes

### The device plugin

[squat/generic-device-plugin](https://github.com/squat/generic-device-plugin) advertises any
device file as an extended resource. With `--domain=loam.arugula.io` its resources are the names
the base asks for: `loam.arugula.io/kvm`, `loam.arugula.io/tun` and `loam.arugula.io/userfaultfd`.

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: generic-device-plugin
  namespace: kube-system
spec:
  selector:
    matchLabels: {app: generic-device-plugin}
  template:
    metadata:
      labels: {app: generic-device-plugin}
    spec:
      priorityClassName: system-node-critical
      containers:
        - name: generic-device-plugin
          image: ghcr.io/squat/generic-device-plugin:0.2.0
          args:
            - --domain=loam.arugula.io
            # count: how many pods on a node may hold the device at once (the nodelet takes one)
            - '--device={"name": "kvm", "groups": [{"count": 10, "paths": [{"path": "/dev/kvm"}]}]}'
            - '--device={"name": "tun", "groups": [{"count": 10, "paths": [{"path": "/dev/net/tun"}]}]}'
            - '--device={"name": "userfaultfd", "groups": [{"count": 10, "paths": [{"path": "/dev/userfaultfd"}]}]}'
          securityContext:
            runAsUser: 0
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: [ALL]}
          volumeMounts:
            - {name: device-plugins, mountPath: /var/lib/kubelet/device-plugins}
            - {name: dev, mountPath: /dev, readOnly: true}
      volumes:
        - {name: device-plugins, hostPath: {path: /var/lib/kubelet/device-plugins, type: Directory}}
        - {name: dev, hostPath: {path: /dev, type: Directory}}
```

Check a node once it has rolled:

```bash
kubectl get node <node> -o jsonpath='{.status.allocatable}' | jq 'with_entries(select(.key | startswith("loam.arugula.io/")))'
# {"loam.arugula.io/kvm": "10", "loam.arugula.io/tun": "10", "loam.arugula.io/userfaultfd": "10"}
```

A device missing from a node (no `/dev/userfaultfd` on an older kernel, say) is simply not
advertised there, and the nodelet stays `Pending` with `Insufficient loam.arugula.io/...`.

If you already run a device plugin under another domain, keep it and change the three resource
names in your overlay (the base's `deploy/base/nodelet/daemonset.yaml`, `resources`).

### The data directory: XFS with reflinks

The nodelet refuses a data directory without reflinks. On a node whose disk is already XFS with
`reflink=1` (the default since xfsprogs 5.1), a directory is enough:

```bash
xfs_info / | grep -o reflink=1      # or the filesystem /var/lib/loam is on
sudo mkdir -p /var/lib/loam
```

Otherwise, an XFS image file, sparse, loop-mounted at boot:

```bash
sudo apt-get install -y xfsprogs
sudo mkdir -p /srv/loam /var/lib/loam
sudo truncate -s 200G /srv/loam/loam.xfs            # sparse: takes space only as it fills
sudo mkfs.xfs -m reflink=1 /srv/loam/loam.xfs
echo '/srv/loam/loam.xfs /var/lib/loam xfs loop,noatime 0 0' | sudo tee -a /etc/fstab
sudo mount /var/lib/loam
xfs_info /var/lib/loam | grep -o reflink=1
```

Mount it before the nodelet starts on that node, or the kubelet creates an empty directory on the
root filesystem and the nodelet crash-loops. To keep it somewhere else, change the `data` volume's
`hostPath` in your overlay.

### The label

The nodelet runs on nodes labelled `loam.arugula.io/node=true`. Label each node once the device
plugin advertises its devices and its data directory is mounted:

```bash
kubectl label node <node> loam.arugula.io/node=true
```

## 2. Storage

**The snapshot bucket.** Create a bucket (`loam-snapshots` here) and a key that may read, write
and delete in it and nothing else. loam keeps one object per snapshot layer and deletes layers no
snapshot needs any more. Its URL goes in your overlay as `LOAM_STORE_URL`:

```
s3://<bucket>?endpoint=<https://s3 endpoint>&s3ForcePathStyle=true&region=<region>
```

For AWS S3, drop `endpoint` and `s3ForcePathStyle` and give the bucket's region. If the store is
inside the cluster or on a private address, the base's NetworkPolicy (the nodelets may reach public
addresses only) refuses it: add an egress rule for it, as `deploy/examples/basic/store-egress.yaml`
does for MinIO.

**Postgres.** A database and a role that owns it; loam-api creates and migrates its tables at
start. Its `postgresql://` URL goes in the Secret `loam-database`, key `uri`. With CloudNativePG,
a `Cluster` with `bootstrap.initdb: {database: loam, owner: loam}` generates exactly that in its
`<cluster>-app` Secret: point loam-api at it with a patch of `LOAM_DATABASE_URL`'s `secretKeyRef`.
The base's NetworkPolicy lets loam-api reach TCP 5432 anywhere; narrow it in your overlay.

## 3. Secrets

The base reads seven Secrets in namespace `loam`. `deploy/examples/basic/secrets.example.yaml`
shows their shape; these commands create them without the values touching a file in the repo:

```bash
kubectl create namespace loam
d=$(mktemp -d)

kubectl -n loam create secret generic loam-database --from-literal=uri='postgresql://loam:<password>@<host>:5432/loam?sslmode=require'
kubectl -n loam create secret generic loam-admin-token --from-literal=LOAM_ADMIN_TOKEN="$(openssl rand -hex 32)"
kubectl -n loam create secret generic loam-node-token  --from-literal=LOAM_NODE_TOKEN="$(openssl rand -hex 32)"
kubectl -n loam create secret generic loam-secret-key  --from-literal=LOAM_SECRET_KEY="$(openssl rand -base64 32)"

openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out $d/id.pem
kubectl -n loam create secret generic loam-identity-key --from-file=identity.pem=$d/id.pem

openssl req -x509 -new -nodes -newkey ec -pkeyopt ec_paramgen_curve:P-256 -days 7300 \
  -subj "/CN=loam broker CA" -addext basicConstraints=critical,CA:TRUE \
  -addext keyUsage=critical,keyCertSign,cRLSign -keyout $d/ca.key -out $d/ca.crt
kubectl -n loam create secret generic loam-broker-ca --from-file=tls.crt=$d/ca.crt --from-file=tls.key=$d/ca.key

kubectl -n loam create secret generic loam-store \
  --from-literal=AWS_ACCESS_KEY_ID='<key id>' --from-literal=AWS_SECRET_ACCESS_KEY='<secret>'

rm -r $d
```

| Secret | Read by | What it is |
|---|---|---|
| `loam-database` | loam-api | Postgres URL (`uri`) |
| `loam-admin-token` | loam-api | the admin token: creates projects and keys, nothing else |
| `loam-node-token` | loam-api, nodelets | what they present to each other; both must hold the same one |
| `loam-secret-key` | loam-api | seals project secrets in Postgres (AES-256-GCM). **Changing it makes every stored secret unreadable.** |
| `loam-identity-key` | loam-api | signs sandbox identity tokens, published at `/.well-known/jwks.json` |
| `loam-broker-ca` | nodelets | the credential broker's CA, which every sandbox trusts. Its key never leaves the nodelets. |
| `loam-store` | nodelets | the snapshot bucket's key |

Keep a copy of `loam-secret-key`, `loam-identity-key` and `loam-broker-ca` somewhere safe: the
first is the only way to read stored secrets back. If you keep secrets in a store of your own
(External Secrets, Sealed Secrets, SOPS), produce the same names and keys from it instead.

loam-api and the nodelets read their tokens at start: after changing one, restart them
(`kubectl -n loam rollout restart deploy/loam-api ds/loam-nodelet`). Restarting a nodelet pauses
every awake sandbox on its node first.

## 4. DNS and TLS

Two wildcard DNS records pointing at your ingress:

```
*.loam.example.com       A/CNAME  <your ingress>
*.e2b.loam.example.com   A/CNAME  <your ingress>
```

and two wildcard certificates, since `*.loam.example.com` doesn't cover a name two labels under it:
one for `*.loam.example.com` (Secret `loam-tls`) and one for `*.e2b.loam.example.com` (Secret
`loam-e2b-tls`). Wildcards need a DNS-01 challenge. With cert-manager and a ClusterIssuer that has
a DNS-01 solver for your DNS provider:

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: {name: loam-tls, namespace: loam}
spec:
  secretName: loam-tls
  issuerRef: {name: <your issuer>, kind: ClusterIssuer}
  dnsNames: ["loam.example.com", "*.loam.example.com"]
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: {name: loam-e2b-tls, namespace: loam}
spec:
  secretName: loam-e2b-tls
  issuerRef: {name: <your issuer>, kind: ClusterIssuer}
  dnsNames: ["e2b.loam.example.com", "*.e2b.loam.example.com"]
```

A wildcard record covers names under it only while no record exists at the name itself: if you
ever create a record at `loam.example.com`, keep `*.loam.example.com` beside it.

## 5. The overlay, and apply

```bash
git clone https://github.com/arugula-salad/loam
cp -r loam/deploy/examples/basic my-loam
```

In `my-loam`:

1. `kustomization.yaml`: point `resources` at the base. From a copy outside the repo, use a pinned
   remote base: `https://github.com/arugula-salad/loam//deploy/base?ref=<commit>`. Its image
   digests are the ones built from that commit.
2. `domain.yaml`: your `--domain` and `--e2b-domain` (the second value of each pair).
3. `store.yaml`: your `LOAM_STORE_URL`; `store-egress.yaml`: your store's pods, or delete it (and
   its entry in `kustomization.yaml`) if the store is on the internet.
4. `ingress.yaml`: your hosts, your `ingressClassName`, and your controller's timeouts. Streams and
   WebSockets (exec, logs, events, port connections) stay open for minutes or hours.
5. Optionally, a patch for the nodelet's size. The base gives VMs 16 GiB per node
   (`--memory=16384`, a 17 GiB request and a 20 GiB limit on the pod); set all three to fit your
   nodes ("Sizing", below).

Then:

```bash
kubectl kustomize my-loam | less      # read what you are about to apply
kubectl apply -k my-loam
kubectl -n loam get pods -o wide      # loam-api, and a loam-nodelet per labelled node, Running
curl -s https://api.loam.example.com/healthz   # ok
```

`https://api.loam.example.com/docs` serves the user docs, the same pages as https://loam.agentsh.it/docs/.

## 6. A project, a key, and the E2B SDK

The CLI:

```bash
go install github.com/arugula-salad/loam/cmd/loam@latest

ADMIN=$(kubectl -n loam get secret loam-admin-token -o jsonpath='{.data.LOAM_ADMIN_TOKEN}' | base64 -d)
loam login --admin --url https://api.loam.example.com --token "$ADMIN"
loam admin project create dev
loam admin key create dev laptop          # prints the key (loam_...) once
loam login --url https://api.loam.example.com      # paste that key
loam create hello --template base
loam exec hello -- uname -a
```

The CLI and the clients take the API's URL from `--url`, `LOAM_API_URL` or what `loam login`
stored, and the key from `--token`, `LOAM_TOKEN` or the same place. There is no default URL.

The E2B SDKs, unmodified (docs/E2B.md has what the adapter serves):

```bash
export E2B_DOMAIN=e2b.loam.example.com
export E2B_API_KEY=loam_...               # a key from above
```

```python
from e2b import Sandbox

sbx = Sandbox.create()
print(sbx.commands.run("uname -a").stdout)
sbx.pause()
```

## Sizing

A DaemonSet has one pod spec, so every node the nodelet runs on gets the same `--memory`. For
nodes of another size, instantiate the base's DaemonSet again under another name, for nodes with
another label value, with an extra selector label so the two never claim each other's pods. In
your overlay, a directory `nodelet-small/` with:

```yaml
# nodelet-small/kustomization.yaml
resources:
  - <the base>/nodelet           # e.g. ../../../deploy/base/nodelet, or the remote base's /nodelet
nameSuffix: -small
patches:
  - path: size.yaml
  - target: {kind: DaemonSet}
    patch: |-
      - op: replace
        path: /spec/template/spec/containers/0/args/2
        value: --memory=4096
```

```yaml
# nodelet-small/size.yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: loam-nodelet
  labels: {loam.arugula.io/size: small}
spec:
  selector:
    matchLabels: {loam.arugula.io/size: small}
  template:
    metadata:
      labels: {loam.arugula.io/size: small}
    spec:
      nodeSelector: {loam.arugula.io/node: small}
      containers:
        - name: loam-nodelet
          resources:
            requests: {cpu: 500m, memory: 4608Mi}
            limits: {memory: 5632Mi}
```

and `- nodelet-small` in the overlay's `resources`. Its pods keep `app: loam-nodelet`, so the
Service and the NetworkPolicies cover them. Label a node `loam.arugula.io/node=small` to move it;
its old nodelet drains (pausing its sandboxes into the store) and the new one wakes them on the
next call. A DaemonSet's selector can't change, so pick the size label before the first apply.

## Upgrading

Move the base's `ref` to a newer commit and apply. loam-api is replaced (a few seconds without the
API); each nodelet rolls one node at a time, pausing that node's awake sandboxes into the store
first, and they wake on the next call.

## What it doesn't do yet

- loam-api runs as one replica.
- The VMs on a node share their nodelet pod's cgroup and memory limit.
- Backups: Postgres and the bucket are yours to back up. The snapshots are useless without the
  records that name them, so back up both, or neither.
