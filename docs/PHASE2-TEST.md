# Phase 2 on two nodes: the test

The acceptance test run on the reference deployment (a three-node k3s cluster with Garage as the
store) when phase 2 landed. The node names are generalized to node-a and node-b, and the commands
assume Garage in the namespace `garage` and SSH to the nodes; adapt both to your cluster. The
numbers in `docs/PLAN.md` come from this run.

What the lead runs once phase 2 has landed (store, nodelet and api deployed, the store's bucket
and keys made, `docs/INSTALL.md`), to check `docs/PLAN.md`'s done-when on node-a
and node-b. Each section says what it proves and what passing looks like. Run them in order; each
leaves the cluster as it found it.

Nothing here touches node-a's or node-b's other workloads: "taking a node away" is done to the
nodelet (its label, or freezing its process), never with `kubectl drain` (which skips DaemonSet
pods anyway) or by stopping k3s (on the reference deployment both nodes are etcd members, and
node-b holds a Garage node).

## 0. Setup and preflight

```bash
export LOAM_API_URL=https://api.loam.example.com
export LOAM_ADMIN_TOKEN=...            # the deployment's admin token
LABEL=loam.example.com/node            # the label that puts a nodelet on a node (docs/INSTALL.md)
go build -o /tmp/loam ./cmd/loam && L=/tmp/loam

$L admin project create p2test
export LOAM_TOKEN=$($L admin key create p2test lead)   # prints the secret on stdout

# Where each sandbox is, as loam-api records it (the public API doesn't say).
where() { kubectl -n loam exec loam-pg-1 -c postgres -- psql -d loam -At -F' ' \
  -c "select name, state, node from sandboxes where project = 'p2test' order by name"; }

# A nodelet's own view (GET /v1/node, docs/NODE.md). The policy admits only loam-api, so through
# a port-forward, which enters the pod directly.
NODE_TOKEN=$(kubectl -n loam get secret loam-node-token -o jsonpath='{.data.LOAM_NODE_TOKEN}' | base64 -d)
nodelet() {  # nodelet <node> <path>
  local pod; pod=$(kubectl -n loam get pod -l app=loam-nodelet --field-selector spec.nodeName=$1 -o name)
  kubectl -n loam port-forward $pod 17070:7070 >/dev/null & local pf=$!; sleep 2
  curl -s -H "Authorization: Bearer $NODE_TOKEN" localhost:17070$2; kill $pf; }

GAR="kubectl -n garage exec garage-3 -- /garage"
```

Preflight, all must hold:

```bash
kubectl -n loam get pods -l app=loam-nodelet -o wide          # one Running on node-a, one on node-b
kubectl get nodes -o custom-columns=N:.metadata.name,UFFD:'.status.allocatable.loam\.arugula\.io/userfaultfd'   # the device plugin's name for /dev/userfaultfd
ssh node-b 'findmnt -no FSTYPE /bulk/loam; xfs_info /bulk/loam | grep -o reflink=1'   # xfs, reflink=1
kubectl -n loam get secret loam-store -o jsonpath='{.data}' | jq 'keys'               # AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
for n in node-a node-b; do nodelet $n /v1/node | jq '{name, memory_mib, memory_mib_allocated, ready}'; done
$GAR bucket info loam-snapshots | grep -E '^(Size|Objects)'   # note both: the GC check compares against them
kubectl -n loam logs ds/loam-nodelet --all-containers --prefix | grep -i -E 'userfaultfd|store|reflink' | tail
```

A process whose state lives only in memory, to tell a memory restore from a reboot: a counter in
`/dev/shm` (tmpfs, gone on any fresh boot) written every second by a process whose PID must not
change.

```bash
mark() {  # mark <sandbox>: start the counter
  $L run $1 -- sh -c 'setsid sh -c "i=0; while :; do i=\$((i+1)); echo \$i > /dev/shm/n; sleep 1; done" >/dev/null 2>&1 < /dev/null & echo $! > /dev/shm/pid'; }
check() {  # check <sandbox>: same PID, still counting
  $L run $1 -- sh -c 'p=$(cat /dev/shm/pid); kill -0 $p && a=$(cat /dev/shm/n) && sleep 2 && b=$(cat /dev/shm/n) && echo "pid $p alive, $a -> $b"'; }
```

`check` passing means the processes came back; a fresh boot fails it (no `/dev/shm/pid`).

## 1. Paused on node-a, woken on node-b

Proves: a pause uploads a full snapshot; a wake on a node without it restores the disk eagerly
and memory lazily through userfaultfd, with the processes.

```bash
$L create mover --idle-action none
where                                    # mover running <node>; if node-b, swap node-a and node-b below
mark mover; check mover
$L pause mover
$L info mover --json | jq .state         # paused
# Uploaded? The bucket grows by about the guest's touched memory plus the disk's diff.
$GAR bucket info loam-snapshots | grep -E '^(Size|Objects)'

# Take node-a's nodelet away (its VMs are paused already, so the SIGTERM has nothing to do).
kubectl label node node-a loam.arugula.io/node-
kubectl -n loam wait --for=delete pod -l app=loam-nodelet --field-selector spec.nodeName=node-a --timeout=360s
$L wake mover
where                                    # mover running node-b
check mover                              # pid alive, counting on from where it stopped
kubectl -n loam logs $(kubectl -n loam get pod -l app=loam-nodelet --field-selector spec.nodeName=node-b -o name) | grep -i -E 'restore|uffd|userfaultfd' | tail

kubectl label node node-a loam.arugula.io/node=true
kubectl -n loam rollout status ds/loam-nodelet
```

Pass: `where` shows node-b, `check` passes, the nodelet's log has no `userfaultfd` errors (an
`EPERM` there means the device isn't reaching the pod: the userfaultfd resource in its resources,
`ls -l /dev/userfaultfd` in it).

Then the way back, which proves a second pause uploads only a diff:

```bash
$GAR bucket info loam-snapshots | grep '^Size'     # before
$L pause mover
$GAR bucket info loam-snapshots | grep '^Size'     # grew by far less than the first pause
kubectl label node node-b loam.arugula.io/node-
kubectl -n loam wait --for=delete pod -l app=loam-nodelet --field-selector spec.nodeName=node-b --timeout=360s
$L wake mover; where; check mover                  # running node-a, same pid
kubectl label node node-b loam.arugula.io/node=true
kubectl -n loam rollout status ds/loam-nodelet
```

## 2. Drain: awake sandboxes move

Proves: on SIGTERM the nodelet pauses every running VM and finishes the uploads inside the grace
period, and they come back elsewhere with their processes.

```bash
for s in d1 d2 d3; do $L create $s --idle-action none; mark $s; done
where                                    # note which are on node-a; drain that node (node-a below)
kubectl label node node-a loam.arugula.io/node-
time kubectl -n loam wait --for=delete pod -l app=loam-nodelet --field-selector spec.nodeName=node-a --timeout=360s
                                         # well under 300 s; at 300 the kubelet kills it mid-upload
where                                    # the ones from node-a: paused (or running node-b if loam-api moves them itself)
for s in d1 d2 d3; do check $s; done     # wakes each on node-b if needed; every one passes
where                                    # all running node-b
kubectl label node node-a loam.arugula.io/node=true
kubectl -n loam rollout status ds/loam-nodelet
```

Pass: every `check` passes. Fail modes: a sandbox `stopped` or freshly booted (the upload didn't
finish: compare the time with `--drain-timeout`), or `error`.

## 3. A lost node

Proves: a node unreachable for 2 minutes has its paused and stopped sandboxes wake elsewhere
from their stored snapshots; when it returns, its now-stale VMs are deleted at reconcile.

The node is "lost" by freezing its nodelet process (SIGSTOP): loam-api can't reach it, nothing
pauses, and k3s, etcd and Garage on that node are untouched. Here node-b is lost; the sandboxes
must be on node-b first (repeat section 1's label dance to put them there if they aren't).

```bash
$L create l-paused --idle-action none;  mark l-paused;  $L pause l-paused
$L create l-stopped --idle-action none; $L run l-stopped -- sh -c 'echo kept > /var/tmp/disk-marker'; $L stop l-stopped
$L create l-running --idle-action none; mark l-running
where                                    # all three on node-b; l-paused paused, l-stopped stopped, l-running running
sleep 30                                 # let the uploads finish (each snapshot's "stored")

ssh node-b sudo pkill -STOP -x loam-nodelet
sleep 150                                # past loam-api's 2 minutes
kubectl -n loam get pod -l app=loam-nodelet --field-selector spec.nodeName=node-b   # RESTARTS unchanged
# (The liveness probe is a TCP connect, which the kernel still accepts for a stopped process. If
# the kubelet restarted the container anyway, the node came back early: start this section again.)
$L wake l-paused;  where; check l-paused                              # running node-a, processes back
$L wake l-stopped; $L run l-stopped -- cat /var/tmp/disk-marker     # "kept": the disk came over
$L wake l-running; where                 # running node-a, from its newest stored snapshot (or 409 if it has none: say which)

ssh node-b sudo pkill -CONT -x loam-nodelet
sleep 60                                 # reconcile
nodelet node-b /v1/vms | jq '[.vms[].id]' # none of the three
where                                    # all three still on node-a
```

Pass: the three wake on node-a, `check l-paused` passes, the disk marker is there, and node-b's
nodelet no longer has their VMs after it comes back. If `l-running` had no stored snapshot,
loam-api must refuse it with an error rather than boot it fresh as if nothing was lost.

## 4. Fork placement

Proves: a fork lands on the node with room, fetching the checkpoint from the store when it isn't
local.

```bash
$L create origin --idle-action none; mark origin
$L checkpoint create origin                       # v1, a memory checkpoint
where                                             # origin's node: call it A, the other B
# Fill A: base sandboxes are 2 GiB, --memory is 16 GiB, so create until A's memory_mib_allocated
# reaches memory_mib (placement may put some on B; keep going until A is full).
for i in $(seq 1 8); do $L create fill-$i --idle-action none; done
where
nodelet <A> /v1/node | jq '{memory_mib, memory_mib_allocated}'   # equal, or within 2048
$L fork origin v1 forked
where                                             # forked running on B
check forked                                      # origin's counter, same pid number, in the fork
```

Pass: `forked` is on B and `check` passes. Then a fork with room on A lands where loam-api's
placement prefers (A has the snapshot locally): delete two `fill-*`, fork again (`forked2`), and
note which node it took.

## 5. Garbage: deleting sandboxes frees their layers

```bash
$L ls --json | jq -r '.[].name' | xargs $L rm
where                                             # nothing
sleep 120                                         # loam-api's layer GC (docs/NODE.md, DELETE /v1/layers)
$GAR bucket info loam-snapshots | grep -E '^(Size|Objects)'
```

Pass: `Size` and `Objects` back to (about) the numbers from preflight: every layer the test made
is gone. Anything left: list it (`kubectl -n garage exec garage-3 -- /garage bucket inspect-object
loam-snapshots <key>`, or any S3 client with the nodelets' store key) and match the names against the sandboxes deleted.

## 6. The phase 1 suite still passes

```bash
LOAM_E2E_URL=$LOAM_API_URL LOAM_E2E_TOKEN=$LOAM_TOKEN LOAM_E2E_ADMIN_TOKEN=$LOAM_ADMIN_TOKEN \
  go test -tags e2e -count=1 -v ./e2e/
```

With two nodelets its sandboxes land on either; it should not care.

## Cleanup

```bash
$L ls --json | jq -r '.[].name' | xargs -r $L rm
$L admin key rm p2test lead && $L admin project rm p2test
kubectl get nodes -L loam.arugula.io/node     # true on node-a and node-b
ssh node-b 'pgrep -x loam-nodelet | xargs -r ps -o stat= -p'   # not T (stopped)
```
