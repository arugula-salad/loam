package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/loam/pkg/api"
)

func TestParseArgs(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		maxPos int
		pos    string
		tty    bool
		env    string
	}{
		{[]string{"sb", "ls", "-la"}, 1, "sb ls -la", false, ""},
		{[]string{"-t", "sb", "--", "bash", "-l"}, 1, "sb bash -l", true, ""},
		{[]string{"--env", "A=1", "sb", "git", "log", "--", "f"}, 1, "sb git log -- f", false, "A=1"},
		{[]string{"name", "--tty", "--env=B=2"}, -1, "name", true, "B=2"},
		{[]string{"a", "--", "-b"}, -1, "a -b", false, ""},
		{[]string{"-", "sb:/x"}, -1, "- sb:/x", false, ""},
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		tty := fs.Bool("tty", false, "")
		fs.BoolVar(tty, "t", false, "")
		env := kvFlag{}
		fs.Var(env, "env", "")
		pos, err := parseArgs(fs, tc.args, tc.maxPos)
		if err != nil || strings.Join(pos, " ") != tc.pos || *tty != tc.tty || env.String() != tc.env {
			t.Errorf("%q: pos=%q tty=%v env=%q err=%v", tc.args, pos, *tty, env.String(), err)
		}
	}
}

func TestParsers(t *testing.T) {
	for in, want := range map[string]int{"512": 512, "2g": 2048, "1GiB": 1024, "768m": 768} {
		if got, err := parseMiB(in); err != nil || got != want {
			t.Errorf("parseMiB(%q) = %d, %v", in, got, err)
		}
	}
	if _, err := parseMiB("lots"); err == nil {
		t.Error("parseMiB accepted nonsense")
	}
	for in, want := range map[string]forward{
		"8080":              {"127.0.0.1:8080", 8080},
		"9000:80":           {"127.0.0.1:9000", 80},
		"0.0.0.0:9000:5432": {"0.0.0.0:9000", 5432},
	} {
		if got, err := parseForward(in); err != nil || got != want {
			t.Errorf("parseForward(%q) = %+v, %v", in, got, err)
		}
	}
	if _, err := parseForward("x:y"); err == nil {
		t.Error("parseForward accepted nonsense")
	}
	if e := parseCpEnd("my-box:/home/loam"); e.sandbox != "my-box" || e.path != "/home/loam" {
		t.Errorf("remote: %+v", e)
	}
	for _, local := range []string{"./a:b", "/abs", "rel/path", "C:/x", "box:rel"} {
		if e := parseCpEnd(local); e.sandbox != "" {
			t.Errorf("%q parsed as remote: %+v", local, e)
		}
	}
	if u := portURL("https://box--jake.loam.test", 3000); u != "https://3000--box--jake.loam.test" {
		t.Errorf("portURL: %s", u)
	}
}

func TestCreateLsInfo(t *testing.T) {
	f := newFake(t)
	code, out, errOut := f.cli(t, "", "create", "box", "--cpus", "4", "--memory", "4g", "--env", "A=1", "--label", "owner=me",
		"--idle", "45s", "--idle-action", "stop", "--deadline", "2h", "--http-port", "3000")
	if code != 0 || !strings.Contains(out, "name:        box") {
		t.Fatalf("create: %d %q %q", code, out, errOut)
	}
	var req api.CreateSandbox
	json.Unmarshal([]byte(f.bodies["POST /v1/sandboxes"]), &req)
	if req.Name != "box" || req.Resources.CPUs != 4 || req.Resources.MemoryMiB != 4096 || req.Env["A"] != "1" ||
		req.Labels["owner"] != "me" || *req.Idle != (api.Idle{TimeoutS: 45, Action: "stop"}) || req.HTTPPort != 3000 ||
		req.Deadline.Action != "delete" || time.Until(req.Deadline.At) < 119*time.Minute {
		t.Fatalf("create sent %s", f.bodies["POST /v1/sandboxes"])
	}

	code, out, _ = f.cli(t, "", "ls")
	if code != 0 || !regexp.MustCompile(`(?m)^NAME +ID +STATE.*\nbox +sb-a{12} +running +base +2 +2048 +owner=me +1h$`).MatchString(out) {
		t.Fatalf("ls: %d\n%s", code, out)
	}
	code, out, _ = f.cli(t, "", "ls", "--json")
	var l []api.Sandbox
	if code != 0 || json.Unmarshal([]byte(out), &l) != nil || len(l) != 1 || l[0].Name != "box" {
		t.Fatalf("ls --json: %d %s", code, out)
	}
	code, out, _ = f.cli(t, "", "info", "--json", "box")
	var sb api.Sandbox
	if code != 0 || json.Unmarshal([]byte(out), &sb) != nil || sb.URL != "https://box--jake.loam.test" {
		t.Fatalf("info --json: %d %s", code, out)
	}
	code, _, errOut = f.cli(t, "", "info", "nope")
	if code != 1 || !strings.Contains(errOut, "sandbox not found (not_found, 404)") {
		t.Fatalf("info of a missing sandbox: %d %q", code, errOut)
	}
	code, out, _ = f.cli(t, "", "pause", "box")
	if code != 0 || out != "box\tpaused\n" {
		t.Fatalf("pause: %d %q", code, out)
	}
}

func TestCreateFromImage(t *testing.T) {
	f := newFake(t)
	img := "ghcr.io/arugula-salad/app@sha256:" + strings.Repeat("a", 64)
	code, out, errOut := f.cli(t, "", "create", "game", "--image", img)
	if code != 0 || !regexp.MustCompile(`(?m)^image: +`+regexp.QuoteMeta(img)+`$`).MatchString(out) ||
		!regexp.MustCompile(`(?m)^url: +https://game--jake\.loam\.test\n^url: +https://game\.games\.test$`).MatchString(out) ||
		strings.Contains(out, "template:") {
		t.Fatalf("create: %d %q %q", code, out, errOut)
	}
	var req api.CreateSandbox
	json.Unmarshal([]byte(f.bodies["POST /v1/sandboxes"]), &req)
	if req.Image != img || req.Template != "" {
		t.Fatalf("create sent %s", f.bodies["POST /v1/sandboxes"])
	}
	code, out, _ = f.cli(t, "", "ls")
	if code != 0 || !regexp.MustCompile(`(?m)^game +sb-a{12} +running +ghcr\.io/arugula-salad/app +2 `).MatchString(out) {
		t.Fatalf("ls: %d\n%s", code, out)
	}
}

func TestUpdateMergesEnv(t *testing.T) {
	f := newFake(t)
	f.cli(t, "", "create", "box", "--env", "A=1", "--env", "B=2")
	code, _, errOut := f.cli(t, "", "update", "box", "--env", "C=3", "--unset-env", "A", "--idle-action", "none")
	if code != 0 {
		t.Fatalf("update: %d %s", code, errOut)
	}
	var u api.UpdateSandbox
	json.Unmarshal([]byte(f.bodies["PATCH /v1/sandboxes/box"]), &u)
	if len(u.Env) != 2 || u.Env["B"] != "2" || u.Env["C"] != "3" || *u.Idle != (api.Idle{TimeoutS: 300, Action: "none"}) || u.Labels != nil {
		t.Fatalf("update sent %s", f.bodies["PATCH /v1/sandboxes/box"])
	}
}

func TestExec(t *testing.T) {
	f := newFake(t)
	f.cli(t, "", "create", "box")
	code, out, errOut := f.cli(t, "", "exec", "box", "echo", "-n", "hi")
	if code != 0 || out != "-n hi\n" || errOut != "done\n" {
		t.Fatalf("exec echo: %d %q %q", code, out, errOut)
	}
	if code, _, _ := f.cli(t, "", "exec", "box", "--", "exit", "7"); code != 7 {
		t.Fatalf("exit code passthrough: %d", code)
	}
	if code, out, _ := f.cli(t, "piped in", "exec", "--env", "X=1", "--cwd", "/tmp", "box", "cat"); code != 0 || out != "piped in" {
		t.Fatalf("stdin: %d %q", code, out)
	}
	if code, out, _ := f.cli(t, "", "exec", "-d", "box", "sleep", "1"); code != 0 || out != "s-1\n" {
		t.Fatalf("detach: %d %q", code, out)
	}
	if !strings.Contains(strings.Join(f.requests, "\n"), "cmd=cat&cwd=%2Ftmp&env=X%3D1") {
		t.Fatalf("exec query: %s", f.requests)
	}
	if !strings.Contains(strings.Join(f.requests, "\n"), "cmd=sleep&cmd=1&detachable=true") {
		t.Fatalf("detach query: %s", f.requests)
	}
}

func TestRunOneShot(t *testing.T) {
	f := newFake(t)
	f.cli(t, "", "create", "box")
	code, out, errOut := f.cli(t, "abc", "run", "box", "--timeout", "1500ms", "--", "cat")
	if code != 0 || out != "abc" || errOut != "on stderr\n" {
		t.Fatalf("run: %d %q %q", code, out, errOut)
	}
	var req api.ExecRun
	json.Unmarshal([]byte(f.bodies["POST /v1/sandboxes/box/exec/run"]), &req)
	if req.TimeoutS != 2 || string(req.Stdin) != "abc" {
		t.Fatalf("run sent %s", f.bodies["POST /v1/sandboxes/box/exec/run"])
	}
	if code, _, _ := f.cli(t, "", "run", "box", "exit", "4"); code != 4 {
		t.Fatalf("run exit code: %d", code)
	}
}

func TestCp(t *testing.T) {
	f := newFake(t)
	f.cli(t, "", "create", "box")
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "proj", "sub"), 0o755)
	os.WriteFile(filepath.Join(src, "proj", "a.txt"), []byte("A"), 0o600)
	os.WriteFile(filepath.Join(src, "proj", "sub", "b.sh"), []byte("#!/bin/sh"), 0o755)
	os.Symlink("a.txt", filepath.Join(src, "proj", "link"))
	os.MkdirAll(filepath.Join(f.root, "home", "loam"), 0o755)

	// A directory into an existing directory lands under its own name.
	if code, _, errOut := f.cli(t, "", "cp", filepath.Join(src, "proj"), "box:/home/loam"); code != 0 {
		t.Fatalf("cp up: %d %s", code, errOut)
	}
	if b, _ := os.ReadFile(filepath.Join(f.root, "home/loam/proj/sub/b.sh")); string(b) != "#!/bin/sh" {
		t.Fatalf("uploaded tree: %q", b)
	}
	if l, _ := os.Readlink(filepath.Join(f.root, "home/loam/proj/link")); l != "a.txt" {
		t.Fatalf("uploaded symlink: %q", l)
	}
	// A single file to a new name keeps its mode.
	if code, _, errOut := f.cli(t, "", "cp", filepath.Join(src, "proj", "sub", "b.sh"), "box:/home/loam/run.sh"); code != 0 {
		t.Fatalf("cp file up: %d %s", code, errOut)
	}
	if info, err := os.Stat(filepath.Join(f.root, "home/loam/run.sh")); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("uploaded file: %v %v", info, err)
	}
	// And back down, to a new directory.
	dst := filepath.Join(t.TempDir(), "copy")
	if code, _, errOut := f.cli(t, "", "cp", "box:/home/loam/proj", dst); code != 0 {
		t.Fatalf("cp down: %d %s", code, errOut)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(b) != "A" {
		t.Fatalf("downloaded tree: %q", b)
	}
	if info, _ := os.Stat(filepath.Join(dst, "sub", "b.sh")); info == nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("downloaded mode: %v", info)
	}
	// Stdin up, stdout down.
	if code, _, _ := f.cli(t, "from stdin", "cp", "-", "box:/tmp/new/x"); code != 0 {
		t.Fatal("cp - up")
	}
	if code, out, errOut := f.cli(t, "", "cp", "box:/tmp/new/x", "-"); code != 0 || out != "from stdin" {
		t.Fatalf("cp down to stdout: %d %q %s", code, out, errOut)
	}
	if code, _, _ := f.cli(t, "", "cp", "a", "b"); code != 1 {
		t.Fatal("local to local should fail")
	}
}

func TestExtractTarStaysInside(t *testing.T) {
	var buf bytes.Buffer
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ok"), []byte("x"), 0o644)
	writeTar(&buf, dir)
	if err := extractTar(bytes.NewReader(buf.Bytes()), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	// A symlink out of the directory, then a file through it.
	evil := t.TempDir()
	var tb bytes.Buffer
	tw := tarWriter(&tb)
	tw.symlink("esc", evil)
	tw.file("esc/pwned", "x")
	tw.Close()
	if err := extractTar(bytes.NewReader(tb.Bytes()), t.TempDir()); err == nil {
		t.Fatal("wrote through a symlink out of the directory")
	}
	if _, err := os.Stat(filepath.Join(evil, "pwned")); err == nil {
		t.Fatal("file landed outside")
	}
}

func TestLogin(t *testing.T) {
	f := newFake(t)
	cfg := filepath.Join(t.TempDir(), "loam", "config.json")
	env := map[string]string{"LOAM_CONFIG": cfg}
	var stdout, stderr bytes.Buffer
	a := &app{ctx: context.Background(), stdin: strings.NewReader("loam_stored\n"), stdout: &stdout, stderr: &stderr, getenv: func(k string) string { return env[k] }}
	if code := a.main([]string{"login", "--url", f.srv.URL}); code != 0 {
		t.Fatalf("login: %d %s", code, stderr.String())
	}
	info, err := os.Stat(cfg)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config: %v %v", info, err)
	}
	b, _ := os.ReadFile(cfg)
	var c config
	if json.Unmarshal(b, &c) != nil || c.Token != "loam_stored" || c.URL != f.srv.URL {
		t.Fatalf("config: %s", b)
	}
	// A later command uses what was stored.
	a = &app{ctx: context.Background(), stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr, getenv: func(k string) string { return env[k] }}
	stdout.Reset()
	if code := a.main([]string{"whoami"}); code != 0 || !strings.Contains(stdout.String(), "project jake") {
		t.Fatalf("whoami after login: %d %q %q", code, stdout.String(), stderr.String())
	}
}

func TestURLAndKeys(t *testing.T) {
	f := newFake(t)
	f.cli(t, "", "create", "box")
	if code, out, _ := f.cli(t, "", "url", "box"); code != 0 || out != "https://box--jake.loam.test\n" {
		t.Fatalf("url: %d %q", code, out)
	}
	if code, out, _ := f.cli(t, "", "url", "box", "--port", "3000"); code != 0 || out != "https://3000--box--jake.loam.test\n" {
		t.Fatalf("url --port: %d %q", code, out)
	}
	code, out, _ := f.cli(t, "", "url", "box", "--port", "4000", "--public", "--json")
	var s api.PortSetting
	if code != 0 || json.Unmarshal([]byte(out), &s) != nil || s.Access != "public" || s.Port != 4000 {
		t.Fatalf("url --public: %d %q", code, out)
	}
	if code, out, errOut := f.cli(t, "", "admin", "key", "create", "jake", "ci"); code != 0 || out != "loam_SECRET\n" || !strings.Contains(errOut, "not shown again") {
		t.Fatalf("key create: %d %q %q", code, out, errOut)
	}
}

func TestUsage(t *testing.T) {
	f := newFake(t)
	if code, _, errOut := f.cli(t, "", "frobnicate"); code != 2 || !strings.Contains(errOut, `unknown command "frobnicate"`) {
		t.Fatalf("unknown: %d %q", code, errOut)
	}
	if code, _, errOut := f.cli(t, "", "info"); code != 2 || !strings.Contains(errOut, "usage: loam info <sandbox>") {
		t.Fatalf("missing arg: %d %q", code, errOut)
	}
	if code, out, _ := f.cli(t, "", "help"); code != 0 || !strings.Contains(out, "port-forward") {
		t.Fatalf("help: %d", code)
	}
	if code, out, _ := f.cli(t, "", "services"); code != 2 || !strings.Contains(out, "restart") {
		t.Fatalf("services alone: %d %q", code, out)
	}
}

func TestPortForward(t *testing.T) {
	f := newFake(t)
	f.cli(t, "", "create", "box")
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	done := make(chan int)
	go func() {
		env := map[string]string{"LOAM_API_URL": f.srv.URL, "LOAM_TOKEN": "loam_test", "LOAM_CONFIG": filepath.Join(t.TempDir(), "c")}
		a := &app{ctx: ctx, stdin: strings.NewReader(""), stdout: io.Discard, stderr: pw, getenv: func(k string) string { return env[k] }}
		done <- a.main([]string{"port-forward", "box", "0:5432"})
		pw.Close()
	}()
	line, err := bufio.NewReader(pr).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	go io.Copy(io.Discard, pr)
	m := regexp.MustCompile(`forwarding (\S+) -> box:5432`).FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("no listener: %q", line)
	}
	conn, err := net.Dial("tcp", m[1])
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("hello"))
	buf := make([]byte, 64)
	n, _ := io.ReadAtLeast(conn, buf, len("5432:hello"))
	if string(buf[:n]) != "5432:hello" {
		t.Fatalf("through the forward: %q", buf[:n])
	}
	conn.Close()
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("port-forward exited %d", code)
	}
}

type testTar struct{ *tar.Writer }

func tarWriter(w io.Writer) testTar { return testTar{tar.NewWriter(w)} }

func (t testTar) symlink(name, target string) {
	t.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777})
}

func (t testTar) file(name, body string) {
	t.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(body)), Mode: 0o644})
	t.Write([]byte(body))
}

func TestEgressAndSecretFlags(t *testing.T) {
	f := newFake(t)
	code, out, errOut := f.cli(t, "", "create", "box", "--allow-defaults", "--allow", "github.com:22,443", "--allow", "*.example.com",
		"--deny", "gist.github.com", "--secret", "GITHUB_TOKEN", "--secret", "A,B")
	if code != 0 {
		t.Fatalf("create: %d %s", code, errOut)
	}
	var req api.CreateSandbox
	json.Unmarshal([]byte(f.bodies["POST /v1/sandboxes"]), &req)
	if req.Network == nil || networkString(*req.Network) != "include defaults, allow github.com:22,443, allow *.example.com, deny gist.github.com" ||
		req.Secrets == nil || strings.Join(*req.Secrets, " ") != "GITHUB_TOKEN A B" {
		t.Fatalf("create sent %s", f.bodies["POST /v1/sandboxes"])
	}
	if !strings.Contains(out, "network:     include defaults, allow github.com:22,443") || !strings.Contains(out, "secrets:     GITHUB_TOKEN,A,B") {
		t.Fatalf("create printed:\n%s", out)
	}
	// Without the flags, nothing is sent: the API's default, or a fork's source's.
	f.cli(t, "", "create", "plain")
	if b := f.bodies["POST /v1/sandboxes"]; strings.Contains(b, "network") || strings.Contains(b, "secrets") {
		t.Fatalf("plain create sent %s", b)
	}
	for _, bad := range [][]string{{"--deny", "a.com:22"}, {"--allow", "a.com:ssh"}, {"--allow", ":22"}} {
		if code, _, _ := f.cli(t, "", append([]string{"create", "x"}, bad...)...); code == 0 {
			t.Errorf("create %q was taken", bad)
		}
	}

	// update replaces the rules whole, and merges secrets like env.
	code, _, errOut = f.cli(t, "", "update", "box", "--allow", "pypi.org", "--secret", "C", "--unset-secret", "A")
	if code != 0 {
		t.Fatalf("update: %d %s", code, errOut)
	}
	var u api.UpdateSandbox
	json.Unmarshal([]byte(f.bodies["PATCH /v1/sandboxes/box"]), &u)
	if u.Network == nil || networkString(*u.Network) != "allow pypi.org" || u.Secrets == nil || strings.Join(*u.Secrets, " ") != "GITHUB_TOKEN B C" {
		t.Fatalf("update sent %s", f.bodies["PATCH /v1/sandboxes/box"])
	}
	f.cli(t, "", "update", "box", "--unrestricted")
	if b := f.bodies["PATCH /v1/sandboxes/box"]; b != `{"network":{"egress":[]}}`+"\n" && b != `{"network":{"egress":[]}}` {
		t.Fatalf("--unrestricted sent %s", b)
	}
	if code, _, _ := f.cli(t, "", "update", "box", "--unrestricted", "--allow", "a.com"); code == 0 {
		t.Fatal("--unrestricted with a rule was taken")
	}

	code, out, _ = f.cli(t, "", "network", "defaults")
	if code != 0 || out != "allow github.com\nallow *.npmjs.org\n" {
		t.Fatalf("network defaults: %d %q", code, out)
	}
}

func TestSecretPut(t *testing.T) {
	f := newFake(t)
	// The value comes from stdin, less its newline, and is never printed.
	code, out, errOut := f.cli(t, "ghp_secret\n", "secret", "put", "GITHUB_TOKEN", "--host", "api.github.com,github.com")
	if code != 0 || strings.Contains(out, "ghp_secret") || !strings.Contains(out, "placeholder: __loam_github_token__") {
		t.Fatalf("put: %d %q %q", code, out, errOut)
	}
	var req api.PutSecret
	json.Unmarshal([]byte(f.bodies["PUT /v1/secrets/GITHUB_TOKEN"]), &req)
	if req.Value != "ghp_secret" || strings.Join(req.Hosts, " ") != "api.github.com github.com" || req.Inject != nil {
		t.Fatalf("put sent %+v", req)
	}
	code, out, _ = f.cli(t, "", "secret", "put", "ANTHROPIC_API_KEY", "--host", "api.anthropic.com", "--value", "sk-1",
		"--header", "x-api-key", "--placeholder-prefix", "sk-ant-")
	json.Unmarshal([]byte(f.bodies["PUT /v1/secrets/ANTHROPIC_API_KEY"]), &req)
	if code != 0 || req.Value != "sk-1" || req.Inject == nil || *req.Inject != (api.Inject{Header: "x-api-key", Format: "{value}"}) ||
		req.PlaceholderPrefix != "sk-ant-" || !strings.Contains(out, "inject:      x-api-key: {value}") {
		t.Fatalf("put with a header: %d %q, sent %+v", code, out, req)
	}
	if code, _, errOut := f.cli(t, "", "secret", "put", "X", "--host", "a.com"); code == 0 || !strings.Contains(errOut, "no value") {
		t.Fatalf("put without a value: %d %q", code, errOut)
	}
	if code, _, _ := f.cli(t, "v", "secret", "put", "X", "--host", "a.com", "--format", "Bearer {value}"); code == 0 {
		t.Fatal("--format without --header was taken")
	}
}

func TestStats(t *testing.T) {
	f := newFake(t)
	f.cli(t, "", "create", "web")
	code, out, errOut := f.cli(t, "", "stats", "web", "--days", "2")
	if code != 0 || !regexp.MustCompile(`(?m)^DAY +VISITORS +REQUESTS.*\n2026-10-12 +0 +0 .*\n2026-10-13 +9 +40 +38 +0 +2 +0 +0 +3.0MiB +0\ntotal +9\* +40 `).MatchString(out) {
		t.Fatalf("stats: %d %q\n%s", code, errOut, out)
	}
	if got := f.requests[len(f.requests)-1]; got != "GET /v1/sandboxes/web/stats?days=2" {
		t.Fatalf("asked %s", got)
	}
}
