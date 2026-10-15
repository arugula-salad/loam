# Getting started with loam

loam runs your code in sandboxes. A sandbox is a small Linux virtual machine (a Firecracker
microVM, Ubuntu 24.04 with Python, Node, git and the usual tools) that you create with one call
and then use over an HTTP API: run commands in it, read and write its files, reach the servers it
runs at their own HTTPS URLs. A sandbox that nobody uses for a while goes to sleep, and wakes on
the next request with its memory, its processes and its disk as they were. A sleeping
sandbox holds no CPU or memory, and you never have to think about which machine it is on.

This page takes you from a key to a working sandbox in a few minutes, then tours the rest. The
[API reference](reference/index.md) has every route in detail.

## Before you start

**A key.** loam keys belong to a project, and whoever runs your loam deployment makes them with
its admin token (if that's you, see the [install guide](INSTALL.md)). A key looks like `loam_`
and 32 letters and digits. Everything you create with it belongs to its project, and it
never sees another project's sandboxes. Treat it like a password.

**Two environment variables.** The CLI, the Go and TypeScript clients and every `curl` example in
these docs read them:

```sh
export LOAM_API_URL=https://api.loam.example.com
export LOAM_TOKEN=loam_...        # your key
```

**The CLI.** `loam` is a single Go binary (Go 1.27.1 or later to build it):

```sh
go install github.com/arugula-salad/loam/cmd/loam@latest
```

From a checkout, `go build -o ~/.local/bin/loam ./cmd/loam` does the same. Instead of the two
variables you can store the URL and key once with `loam login` (it reads the key from stdin).
Check that it works:

```console
$ loam whoami
project docs, key docs-writer (key-mgnnkl5dvw)
```

Every command takes `--json` for machine-readable output, and `loam <command> --help` lists its
flags.

## Your first sandbox

### Create it

```console
$ loam create scratch
name:        scratch
id:          sb-2kjiso7vff5b
project:     docs
template:    base
state:       running
resources:   2 cpus, 2048 MiB, 20 GiB disk
url:         https://scratch--docs.loam.example.com
http_port:   8080
idle:        pause after 300s
network:     unrestricted
created:     2026-10-12T09:11:46Z
last_active: 2026-10-12T09:11:46Z
```

That took under half a second. The sandbox has a name you chose (`scratch`) and an ID loam chose
(`sb-2kjiso7vff5b`); every command and route takes either. Leave the name out and loam makes one
up, like `rustic-meadow-7517`.

### Run a command

```console
$ loam run scratch -- sh -c 'id; pwd; nproc' </dev/null
uid=1000(loam) gid=1000(loam) groups=1000(loam)
/home/loam
2
```

Commands run as the user `loam` (which has passwordless `sudo`) in `/home/loam`, unless you give
`--user root` or `--cwd`. `loam run` runs the command to completion and then prints its output and
exits with its exit code. `loam exec` streams the output as it comes, and `loam console` gives you
an interactive shell:

```console
$ loam exec scratch -- sh -c 'echo out; echo err >&2; exit 3' </dev/null; echo "exit=$?"
err
out
exit=3
```

The `</dev/null` matters in scripts: both commands send their own standard input to the command
when it isn't a terminal, so without it a command in a script would read the script's input.

### Write and read files

```console
$ echo '<h1>hello from loam</h1>' > index.html
$ loam cp --user loam index.html scratch:/home/loam/site/index.html
$ loam cp scratch:/home/loam/site/index.html back.html && cat back.html
<h1>hello from loam</h1>
```

A sandbox path is written `<sandbox>:/absolute/path`; `loam cp` copies directories too. File
operations run as root unless you give `--user`, so without `--user loam` the file would belong to
root.

### Run a web server and open its URL

Start a server in a session that outlives the command (`-d` prints the session's ID and leaves it
running):

```console
$ loam exec -d scratch -- python3 -m http.server 8080 --directory /home/loam/site </dev/null
s-rhgpddrfzk
$ loam ports scratch
PORT  ADDRESS  PID  SESSION
8080  0.0.0.0  309  s-rhgpddrfzk
$ loam url scratch
https://scratch--docs.loam.example.com
```

The sandbox's URL goes to its `http_port`, 8080 unless you choose another, and every other port
has one too: `https://3000--scratch--docs.loam.example.com` is port 3000. URLs are private
until you say otherwise: they want your key.

```console
$ curl https://scratch--docs.loam.example.com/
{"error":{"code":"unauthorized","message":"this port is private: send the project's key as a bearer token, or visit with ?loam_token=<key>"}}
$ curl -H "Authorization: Bearer $LOAM_TOKEN" https://scratch--docs.loam.example.com/
<h1>hello from loam</h1>
```

In a browser, open the URL once with `?loam_token=<your key>` on the end: loam sets a cookie for
that host and redirects to the bare URL.

### Make it public

```console
$ loam url scratch --public
https://scratch--docs.loam.example.com
$ curl https://scratch--docs.loam.example.com/
<h1>hello from loam</h1>
```

`loam url scratch --private` closes it again; `--port N` does the same for one port.

### Pause it, wake it, and see that nothing was lost

Start something that keeps state in memory, a counter that writes to a file every second:

```console
$ loam exec -d scratch -- python3 -c '
import time
n = 0
while True:
    n += 1
    open("/home/loam/count", "w").write(str(n))
    time.sleep(1)
' </dev/null
s-x6r4c6zelj
$ loam run scratch -- sh -c 'cat /home/loam/count; echo; pgrep -a python3' </dev/null
4
309 python3 -m http.server 8080 --directory /home/loam/site
318 python3 -c  import time n = 0 ...
$ loam pause scratch
scratch	paused
```

A paused sandbox keeps its memory and its disk and uses no CPU. You don't have to wake it
yourself: any request does, a command, a file read, a visit to its URL.

```console
$ curl -s -o /dev/null -w '%{http_code}\n' https://scratch--docs.loam.example.com/
200
$ loam run scratch -- sh -c 'cat /home/loam/count; echo; pgrep -a python3' </dev/null
4
309 python3 -m http.server 8080 --directory /home/loam/site
318 python3 -c  import time n = 0 ...
```

Same processes, same PIDs, and the counter carries on from 4: for the sandbox, no time passed.
loam pauses a sandbox by itself after 300 seconds without use (see "Idle" below).

`loam stop` is the other kind of sleep: it keeps only the disk, and the next wake boots fresh.

```console
$ loam stop scratch && loam wake scratch
scratch	stopped
scratch	running
$ loam run scratch -- sh -c 'cat /home/loam/count; echo; pgrep -a python3 || echo "no processes"' </dev/null
4
no processes
```

The file is still there; the processes are gone. Things you want running after every boot belong
in a service (below).

### Checkpoint and fork

A checkpoint is a snapshot you can go back to, or start new sandboxes from. By default it holds
memory too, so processes come back running.

```console
$ loam checkpoint create scratch --comment 'site up, state v1'
v1
$ loam checkpoint ls scratch
ID  KIND    AUTO  CREATED              COMMENT
v1  memory  no    2026-10-12 05:17:36  site up, state v1
```

A fork is a new sandbox from a checkpoint, with its own name and URL:

```console
$ loam fork scratch v1 scratch-copy
name:        scratch-copy
id:          sb-y6sgfql7k3ra
state:       running
url:         https://scratch-copy--docs.loam.example.com
...
$ loam run scratch-copy -- pgrep -a python3 </dev/null
314 python3 -m http.server 8080 --directory /home/loam/site
```

`loam checkpoint restore scratch v1` puts `scratch` itself back to `v1` (keeping its state just
before as an automatic checkpoint, `auto-1`, in case you change your mind).

### Clean up

```console
$ loam rm scratch scratch-copy
```

Deleting a sandbox deletes its checkpoints too. That's the whole loop: create, use, let it sleep,
delete.

## A tour of the rest

### Services: things that start at boot

A service is a supervised process: it starts at every boot, is restarted if it crashes, and
survives a pause (in memory) and a stop (it starts again on the next boot). A service with
`--http-port` is started by a request to that port's URL if it isn't running, so a sandbox whose
site is a service serves its URL even after a stop.

```console
$ loam services put svc-demo web --http-port 8080 --cwd /home/loam/site -- python3 -m http.server 8080
name:      web
state:     running
cmd:       python3 -m http.server 8080
...
$ loam services ls svc-demo
NAME    STATE    PID  RESTARTS  HTTP_PORT  CMD
web     running  295  0         8080       python3 -m http.server 8080
$ loam services logs svc-demo web --tail 20
```

More in [Services](reference/services.md).

### Idle, and tasks to keep a sandbox awake

A sandbox is idle when no request is in flight, no exec session is attached, and nothing in it
holds it awake; after `idle.timeout_s` (300 by default, 30 at least) of that, loam applies
`idle.action`: `pause` (the default), `stop`, or `none` (never sleep by itself).

```sh
loam create builder --idle 30m --idle-action stop
loam update builder --idle-action none
```

A long job that runs on its own (a build started with `exec -d`, a crawler) isn't "use" in that
sense. Hold the sandbox awake with a **task**, which expires (an hour at most) so that a client
that crashed can't hold it forever; put it again to hold longer.

```sh
curl -X PUT -H "Authorization: Bearer $LOAM_TOKEN" -H 'Content-Type: application/json' \
  "$LOAM_API_URL/v1/sandboxes/builder/tasks/build" -d '{"expire_s": 600}'
```

From inside the sandbox it's the same call on the API socket (`/tasks/build`). See
[Tasks](reference/tasks.md).

### Egress rules

A sandbox reaches the internet and nothing else: never the machine it runs on, the cluster or
another sandbox. Egress rules narrow that to the names you allow, enforced outside the VM:

```console
$ loam create egress-demo --allow-defaults --allow postman-echo.com --deny gist.github.com
$ loam run egress-demo -- curl -sS -o /dev/null -w '%{http_code}\n' https://api.github.com/ </dev/null
200
$ loam run egress-demo -- curl -sS https://example.com/ </dev/null
curl: (6) Could not resolve host: example.com
```

`--allow-defaults` is loam's list of common development hosts (package registries, GitHub and
GitLab, container registries, the major AI APIs; `loam network defaults` shows it). An allow opens
ports 80 and 443 unless you name others (`--allow github.com:22,443`). A restricted sandbox
speaks only TCP over IPv4. `loam update` with rules replaces the whole list, and
`loam update --unrestricted` drops it. The rules and how they decide are in
[Network](reference/network.md).

### Secrets

A project secret is a credential your sandboxes use without ever holding it. The sandbox's
environment has a **placeholder** where the value would be; when a request leaves the sandbox for
one of the secret's hosts, over HTTPS, loam puts the real value in on the way out.

Here with a harmless one: postman-echo.com's `/basic-auth` answers `{"authenticated":true}` to
the user `postman` with the password `password`.

```console
$ printf 'password' | loam secret put ECHO_PASSWORD --host postman-echo.com/basic-auth
name:        ECHO_PASSWORD
placeholder: __loam_echo_password__
hosts:       postman-echo.com/basic-auth
...
$ loam create secret-demo --secret ECHO_PASSWORD
$ loam run secret-demo -- sh -c 'echo $ECHO_PASSWORD' </dev/null
__loam_echo_password__
$ loam run secret-demo -- sh -c 'curl -sS -u "postman:$ECHO_PASSWORD" https://postman-echo.com/basic-auth' </dev/null
{"authenticated":true}
```

The request reached the host with the real password, though nothing in the sandbox (its memory,
its disk, its snapshots) ever had it. A real one is the same: `GITHUB_TOKEN` with
`--host api.github.com`, and `curl -H "Authorization: Bearer $GITHUB_TOKEN" https://api.github.com/user`. What brokering can't do: carry a value back (an answer that
contains the value is cut off with a `502`), work over anything but HTTPS to the named hosts, or
reach a host you didn't list (there the placeholder goes out as it is, worth nothing). See
[Secrets](reference/secrets.md).

### Identity tokens

A sandbox can prove which sandbox it is to anything that accepts OpenID Connect tokens (AWS, GCP,
Vault, your own service), without holding a credential:

```console
$ loam run scratch -- curl -sS --unix-socket /run/loam/api.sock \
    'http://loam/identity/token?audience=sts.amazonaws.com' </dev/null
{"token":"eyJhbGciOiJSUzI1NiIs...","expires_at":"2026-10-12T09:36:26Z"}
```

The token's subject is `project:docs:sandbox:sb-2kjiso7vff5b`, and it carries the sandbox's
name and labels. [Identity](reference/identity.md) shows how to verify one and how to trade it for
AWS credentials.

### Images on create

Besides the templates (`loam templates`: `base`, and `e2b` for the E2B adapter), a sandbox can be
made from an OCI image of your own, named by digest (never a tag) in a registry loam allows (here
`ghcr.io/arugula-salad`):

```sh
loam create game --image ghcr.io/arugula-salad/game@sha256:<digest> --http-port 5173
```

The image's environment becomes the sandbox's; its entrypoint is not run (use a service for
that). The first sandbox from a new image takes a while, since the node pulls the image and
builds a disk from it (about 50 seconds for the `e2b` image); later ones start from that disk. See
[Sandboxes](reference/sandboxes.md#images).

### Inside a sandbox

Processes in a sandbox reach the same API about themselves on a Unix socket, with no key: being
inside is the credential.

```sh
curl --unix-socket /run/loam/api.sock http://loam/self                      # this sandbox's record
curl --unix-socket /run/loam/api.sock -X POST http://loam/checkpoints -d '{}'  # checkpoint myself
curl --unix-socket /run/loam/api.sock 'http://loam/fs/list?path=/home/loam'
```

See [Inside a sandbox](reference/inside.md).

### Using loam from code

Go, with `github.com/arugula-salad/loam/pkg/client`:

```go
c, err := client.FromEnv() // LOAM_API_URL and LOAM_TOKEN
if err != nil {
	log.Fatal(err)
}
sb, err := c.CreateSandbox(ctx, api.CreateSandbox{Name: "go-demo", Idle: &api.Idle{TimeoutS: 120}})
if err != nil {
	log.Fatal(err)
}
res, err := c.Run(ctx, sb.ID, api.ExecRun{Cmd: []string{"sh", "-c", "echo hello from $(hostname)"}})
if err != nil {
	log.Fatal(err)
}
fmt.Printf("exit %d: %s", res.ExitCode, res.Stdout) // exit 0: hello from go-demo
```

TypeScript (Node 20 or later), with the `@arugula-salad/loam` package or the single file
`loam.mjs` from the `ts-v*` GitHub releases:

```ts
import { LoamClient } from './loam.mjs'

const loam = LoamClient.fromEnv()
const sb = await loam.createSandbox({ name: 'ts-demo', idle: { timeout_s: 120 } })
const box = loam.sandbox(sb.id)
const r = await box.run({ cmd: ['sh', '-c', 'wc -c'], stdin: 'hello' })
console.log(r.exit_code, r.stdout.toString()) // 0 "5\n"
await box.writeFile('/home/loam/app/index.html', '<h1>hi</h1>\n', { mkdir: true, user: 'loam' })
```

Every reference page has more, in curl first and in the clients where they help.

### The E2B SDKs

loam also speaks E2B's API, so code written for E2B's official SDKs (Python and JavaScript) runs
here unchanged, with two environment variables:

```sh
export E2B_DOMAIN=e2b.loam.example.com
export E2B_API_KEY=$LOAM_TOKEN     # your loam key
```

An E2B sandbox is an ordinary loam sandbox in your project, so `loam ls` shows it too. What works
and what doesn't is in [E2B](reference/e2b.md).

## Limits and gotchas

- **Idle pause.** A sandbox pauses after 300 seconds without use unless you change `idle`. A
  paused sandbox wakes on any request, usually in well under a second, but anything that was
  talking to it over a socket was cut off (next point).
- **A pause closes exec sockets.** `loam exec` attached to a sandbox that pauses ends with
  "the session ended without an exit status" (exit 255); the process itself is in the snapshot
  and carries on after the wake. Start long jobs with `exec -d` and look at them with
  `loam attach` or a service's logs, and hold the sandbox awake with a task.
- **Ctrl-C reaches the command.** Without a TTY, `loam exec` and `loam attach` pass `INT`,
  `TERM`, `HUP` and `QUIT` on to the remote process, so interrupting `loam attach` stops the
  process you attached to. With a TTY (`exec -t`, `console`), `Ctrl-]` detaches from a detachable
  session.
- **stdin in scripts.** `loam run` and `loam exec` forward their standard input when it isn't a
  terminal. Give `</dev/null` to commands in scripts and CI.
- **Names and URLs.** A name is a DNS label: lowercase letters, digits and `-`, at most 32
  characters, no `--`, and with the project name at most 54 characters in all. The URL of port P
  of sandbox N in project J is `https://P--N--J.loam.example.com`. Renaming isn't possible;
  fork to a new name instead.
- **Restricted egress is TCP only.** With any egress rule, UDP (QUIC included), ICMP and IPv6 are
  dropped, an IP address can't be allowed (only names), and HTTP and HTTPS are checked by name on
  every connection, so a name sharing an allowed name's CDN address stays out.
- **`cp` and file routes run as root** unless you give a user, so files you upload belong to root.
- **`/tmp` is memory.** It survives a pause and is empty after a stop or a disk-only restore.
- **Environment changes** (`loam update --env`) reach new processes and the next wake, not
  processes already running.
- **One-shot output** (`loam run`, `exec/run`) is capped at 16 MiB per stream and 60 seconds by
  default (`--timeout`, at most an hour); use `exec` for more.
- **Capacity.** When no node has room the API answers `503 capacity` with `Retry-After`; wait and
  try again.

## Where to go next

- [API reference](reference/index.md): authentication, errors, and every route.
- [Exec](reference/exec.md) for the streaming protocol, if you're writing your own client.
- [Ports and URLs](reference/ports.md), [Network](reference/network.md) and
  [Secrets](reference/secrets.md) for what a sandbox can reach and who can reach it.
- The docs are served at https://api.loam.example.com/docs.
