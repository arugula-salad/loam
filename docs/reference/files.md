# Files

Read and write a sandbox's files without running a command. Paths are absolute. Every file
operation runs as **root** unless you give `user=` (a query parameter on every route here,
including the ones with JSON bodies); with a user, that user's permissions apply, and a refusal is
`403 permission_denied`. A missing path is `404 not_found`.

| Method and path | |
|---|---|
| `GET /v1/sandboxes/{sandbox}/fs/file?path=` | read a file: raw bytes, `application/octet-stream` |
| `PUT /v1/sandboxes/{sandbox}/fs/file?path=&mode=0644&mkdir=true` | write a file from the raw body → `Stat` |
| `DELETE /v1/sandboxes/{sandbox}/fs/file?path=&recursive=false` | delete a file or directory → `204` |
| `GET /v1/sandboxes/{sandbox}/fs/stat?path=` | → `Stat` |
| `GET /v1/sandboxes/{sandbox}/fs/list?path=` | → `{"entries": [Stat]}` |
| `POST /v1/sandboxes/{sandbox}/fs/mkdir` | `{"path", "parents"?, "mode"?}` → `Stat` |
| `POST /v1/sandboxes/{sandbox}/fs/rename` | `{"from", "to"}` → `Stat` |
| `GET /v1/sandboxes/{sandbox}/fs/tar?path=` | a directory as a tar, streamed |
| `PUT /v1/sandboxes/{sandbox}/fs/tar?path=` | extract a tar into a directory → `204` |
| `GET /v1/sandboxes/{sandbox}/fs/watch?path=&recursive=false` | WebSocket of changes |

A `Stat`:

```json
{"path": "/home/loam/app/link.py", "name": "link.py", "type": "symlink", "size": 22,
 "mode": "0777", "mtime": "2026-10-12T09:18:03.006765912Z", "target": "/home/loam/app/main.py"}
```

`type` is `file`, `dir`, `symlink` or `other`; `mode` is octal permissions; `target` only for a
symlink. Stat doesn't follow symlinks.

The examples use:

```sh
U="$LOAM_API_URL/v1/sandboxes/scratch"
A="Authorization: Bearer $LOAM_TOKEN"
```

## Write a file

`PUT .../fs/file?path=` with the file's bytes as the body. `mode` (octal, default `0644` for a new
file) sets its permissions; `mkdir=true` creates missing parent directories, without which a
missing parent is `404 not_found`. The new contents replace the file at once: a reader sees the old
file or the new one, never half of it.

```console
$ curl -sS -H "$A" -X PUT --data-binary 'print("hi")' "$U/fs/file?path=/home/loam/app/main.py&mode=0644&mkdir=true&user=loam"
{"path":"/home/loam/app/main.py","name":"main.py","type":"file","size":11,"mode":"0644","mtime":"2026-10-12T09:18:02.902765908Z"}
$ curl -sS -H "$A" -X PUT --data-binary @local.bin "$U/fs/file?path=/srv/local.bin"
```

Use `--data-binary`, not `-d`, which would strip newlines. Written as root (no `user=`), the file
belongs to root.

## Read a file

```console
$ curl -sS -H "$A" "$U/fs/file?path=/home/loam/app/main.py"
print("hi")
$ curl -sS -H "$A" "$U/fs/file?path=/etc/shadow&user=loam"
{"error":{"code":"permission_denied","message":"open /etc/shadow: permission denied"}}
```

## Stat, list, mkdir, rename, delete

```console
$ curl -sS -H "$A" "$U/fs/stat?path=/home/loam/app"
{"path":"/home/loam/app","name":"app","type":"dir","size":0,"mode":"0755","mtime":"2026-10-12T09:18:02.902765908Z"}
$ curl -sS -H "$A" "$U/fs/list?path=/home/loam/app"
{"entries":[{"path":"/home/loam/app/main.py","name":"main.py","type":"file","size":11,"mode":"0644","mtime":"2026-10-12T09:18:02.902765908Z"}]}
$ curl -sS -H "$A" -H 'Content-Type: application/json' "$U/fs/mkdir" -d '{"path":"/home/loam/app/a/b","parents":true,"mode":"0755"}'
{"path":"/home/loam/app/a/b","name":"b","type":"dir","size":0,"mode":"0755","mtime":"2026-10-12T09:18:02.970765911Z"}
$ curl -sS -H "$A" -H 'Content-Type: application/json' "$U/fs/rename" -d '{"from":"/home/loam/app/a","to":"/home/loam/app/c"}'
{"path":"/home/loam/app/c","name":"c","type":"dir","size":0,"mode":"0755","mtime":"2026-10-12T09:18:02.970765911Z"}
$ curl -sS -H "$A" "$U/fs/stat?path=/nope"
{"error":{"code":"not_found","message":"lstat /nope: no such file or directory"}}
```

`DELETE .../fs/file` removes a file or an empty directory; a directory with something in it needs
`recursive=true`, or it is `409 conflict`:

```console
$ curl -sS -H "$A" -X DELETE "$U/fs/file?path=/srv/up"
{"error":{"code":"conflict","message":"/srv/up is a directory and not empty; use recursive=true: directory not empty"}}
$ curl -sS -w '%{http_code}\n' -H "$A" -X DELETE "$U/fs/file?path=/srv/up&recursive=true"
204
```

## Tars

`GET .../fs/tar?path=` streams a directory as a tar (`application/x-tar`). Entry names are
relative to `path` (`a/b.txt`, not `/home/loam/dir/a/b.txt`), so the tar unpacks anywhere.

```console
$ curl -sS -H "$A" "$U/fs/tar?path=/home/loam/app" -o app.tar && tar tvf app.tar
drwxr-xr-x 0/0               0 2026-10-12 05:18 c/
drwxr-xr-x 0/0               0 2026-10-12 05:18 c/b/
lrwxrwxrwx 1000/1000         0 2026-10-12 05:18 link.py -> /home/loam/app/main.py
-rw-r--r-- 1000/1000        11 2026-10-12 05:18 main.py
```

`PUT .../fs/tar?path=` extracts the body (a tar, names relative to `path`) into the directory,
creating it and its parents if they're missing. A name that would land outside `path` (`..`, or
through a symlink) is refused with `400 invalid`.

```console
$ tar -C up -cf up.tar www
$ curl -sS -w '%{http_code}\n' -H "$A" -X PUT --data-binary @up.tar "$U/fs/tar?path=/srv/up"
204
$ curl -sS -H "$A" -X PUT --data-binary @evil.tar "$U/fs/tar?path=/srv/up"
{"error":{"code":"invalid","message":"tar entry \"../evil.txt\" is outside the directory"}}
```

Copying a directory from one sandbox to another is a `GET` piped into a `PUT`:

```sh
curl -sS -H "$A" "$LOAM_API_URL/v1/sandboxes/scratch/fs/tar?path=/home/loam/app" |
  curl -sS -H "$A" -X PUT --data-binary @- "$LOAM_API_URL/v1/sandboxes/scratch-copy/fs/tar?path=/home/loam/app"
```

## Watch

`GET .../fs/watch?path=&recursive=false`, a WebSocket. Each change is a text frame
`{"type": "create"|"write"|"remove"|"rename"|"chmod", "path"}`. `recursive=true` watches the
directories below too. Like every stream, an open watch holds the sandbox awake until you close
it.

```ts
const box = loam.sandbox('scratch')
const watch = await box.watchFiles('/home/loam/app')       // { recursive: true } for subdirectories
await box.run({ cmd: ['sh', '-c', 'echo x > /home/loam/app/new.txt'] })
for await (const ev of watch) { console.log(ev.type, ev.path); break }  // create /home/loam/app/new.txt
watch.close()
```

In Go, `c.WatchFiles(ctx, sandbox, path, client.Recursive())` returns a `*FileWatch` with
`Next()` and `Close()`.

## The CLI: `loam cp`

`loam cp <src> <dst>` copies files and directories either way; a sandbox path is written
`<sandbox>:/absolute/path`. Directories go as tars. `--user` does the sandbox side as that user.

```console
$ loam cp index.html scratch:/home/loam/site/index.html
$ loam cp scratch:/home/loam/site/index.html back.html
$ loam cp ./d scratch:/home/loam/d
$ loam run scratch -- find /home/loam/d </dev/null
/home/loam/d
/home/loam/d/sub
/home/loam/d/sub/b.txt
/home/loam/d/a.txt
$ loam cp --user loam index.html scratch:/home/loam/mine.html
$ loam run scratch -- ls -l /home/loam/mine.html </dev/null
-rw-rw-r-- 1 loam loam 25 Oct 12 09:18 /home/loam/mine.html
```

## Go and TypeScript

```go
_, err := c.WriteFile(ctx, "scratch", "/home/loam/notes.txt", strings.NewReader("remember the milk\n"),
	client.AsUser("loam"), client.MakeParents()) // also client.Mode(0o600)
b, err := c.ReadFile(ctx, "scratch", "/home/loam/notes.txt")
// also OpenFile (a stream), Stat, List, Mkdir, Rename, Remove (client.Recursive()), GetTar, PutTar
```

```ts
const box = loam.sandbox('scratch')
await box.writeFile('/home/loam/app/index.html', '<h1>hi</h1>\n', { mode: 0o644, mkdir: true, user: 'loam' })
const buf = await box.readFile('/home/loam/app/index.html')   // a Buffer
await box.stat('/home/loam'); await box.list('/home/loam')
await box.mkdir('/srv/a/b', { parents: true }); await box.rename('/srv/a', '/srv/c')
await box.remove('/srv/c', { recursive: true })
await loam.sandbox('scratch-copy').putTar('/home/loam/app', await box.getTar('/home/loam/app'))
```
