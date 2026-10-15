package agent

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/arugula-salad/loam/pkg/api"
)

func q(kv ...string) string {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Add(kv[i], kv[i+1])
	}
	return "?" + v.Encode()
}

func TestFileRoundTrip(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a", "b", "f.txt")

	// Without mkdir the parent must exist.
	h.wantStatus(h.do("PUT", "/fs/file"+q("path", p), "x", nil), 404, api.CodeNotFound)
	var st api.Stat
	h.wantStatus(h.do("PUT", "/fs/file"+q("path", p, "mode", "0640", "mkdir", "true"), "hello", &st), 200, "")
	if st.Path != p || st.Name != "f.txt" || st.Type != "file" || st.Size != 5 || st.Mode != "0640" || st.MTime.IsZero() {
		t.Fatalf("stat after write %+v", st)
	}
	resp := h.do("GET", "/fs/file"+q("path", p), nil, nil)
	h.wantStatus(resp, 200, "")
	if b, _ := io.ReadAll(resp.Body); string(b) != "hello" || resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("read %q %s", b, resp.Header.Get("Content-Type"))
	}
	h.wantStatus(h.do("GET", "/fs/stat"+q("path", p), nil, &st), 200, "")
	if st.Type != "file" || st.Size != 5 {
		t.Fatalf("stat %+v", st)
	}

	os.Symlink("f.txt", filepath.Join(dir, "a", "b", "link"))
	var list api.StatList
	h.wantStatus(h.do("GET", "/fs/list"+q("path", filepath.Join(dir, "a", "b")), nil, &list), 200, "")
	if len(list.Entries) != 2 || list.Entries[0].Name != "f.txt" || list.Entries[1].Type != "symlink" || list.Entries[1].Target != "f.txt" {
		t.Fatalf("list %+v", list)
	}

	h.wantStatus(h.do("POST", "/fs/mkdir", api.Mkdir{Path: filepath.Join(dir, "m"), Mode: "0700"}, &st), 200, "")
	if st.Type != "dir" || st.Mode != "0700" {
		t.Fatalf("mkdir %+v", st)
	}
	h.wantStatus(h.do("POST", "/fs/mkdir", api.Mkdir{Path: filepath.Join(dir, "m")}, nil), 409, api.CodeAlreadyExists)
	h.wantStatus(h.do("POST", "/fs/mkdir", api.Mkdir{Path: filepath.Join(dir, "m"), Parents: true}, nil), 200, "")
	h.wantStatus(h.do("POST", "/fs/mkdir", api.Mkdir{Path: filepath.Join(dir, "x", "y")}, nil), 404, api.CodeNotFound)

	to := filepath.Join(dir, "m", "g.txt")
	h.wantStatus(h.do("POST", "/fs/rename", api.Rename{From: p, To: to}, &st), 200, "")
	if st.Path != to || st.Size != 5 {
		t.Fatalf("rename %+v", st)
	}
	h.wantStatus(h.do("GET", "/fs/file"+q("path", p), nil, nil), 404, api.CodeNotFound)

	h.wantStatus(h.do("DELETE", "/fs/file"+q("path", filepath.Join(dir, "m")), nil, nil), 409, api.CodeConflict)
	h.wantStatus(h.do("DELETE", "/fs/file"+q("path", filepath.Join(dir, "m"), "recursive", "true"), nil, nil), 204, "")
	if exists(to) {
		t.Fatal("recursive delete left the file")
	}

	// Paths are absolute; a user this agent can't be is refused.
	h.wantStatus(h.do("GET", "/fs/stat"+q("path", "rel/x"), nil, nil), 400, api.CodeInvalid)
	h.wantStatus(h.do("GET", "/fs/stat"+q("path", dir, "user", "someone-else"), nil, nil), 501, api.CodeUnsupported)
	h.wantStatus(h.do("GET", "/fs/file"+q("path", dir), nil, nil), 400, api.CodeInvalid)
	h.wantStatus(h.do("DELETE", "/fs/file"+q("path", "/"), nil, nil), 400, api.CodeInvalid)
	h.wantStatus(h.do("POST", "/fs/mkdir", `{"path":"/x","nope":true}`, nil), 400, api.CodeInvalid)
}

func TestTarRoundTrip(t *testing.T) {
	h := newHarness(t)
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "top.txt"), "top", 0o644)
	mustWrite(t, filepath.Join(src, "sub", "deep.sh"), "#!/bin/sh\n", 0o755)
	os.Symlink("top.txt", filepath.Join(src, "ln"))

	resp := h.do("GET", "/fs/tar"+q("path", src), nil, nil)
	h.wantStatus(resp, 200, "")
	archive, _ := io.ReadAll(resp.Body)
	var names []string
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, hdr.Name)
	}
	sort.Strings(names)
	want := []string{"ln", "sub/", "sub/deep.sh", "top.txt"}
	if len(names) != len(want) {
		t.Fatalf("tar names %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("tar names %v, want %v", names, want)
		}
	}

	dst := filepath.Join(t.TempDir(), "new")
	h.wantStatus(h.do("PUT", "/fs/tar"+q("path", dst), archive, nil), 204, "")
	if b, _ := os.ReadFile(filepath.Join(dst, "top.txt")); string(b) != "top" {
		t.Fatalf("top.txt %q", b)
	}
	if fi, err := os.Stat(filepath.Join(dst, "sub", "deep.sh")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("deep.sh %v %v", fi, err)
	}
	if l, _ := os.Readlink(filepath.Join(dst, "ln")); l != "top.txt" {
		t.Fatalf("ln -> %q", l)
	}

	// An entry that climbs out of the directory is refused, and nothing lands outside.
	var evil bytes.Buffer
	tw := tar.NewWriter(&evil)
	tw.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	tw.Write([]byte("x"))
	tw.Close()
	h.wantStatus(h.do("PUT", "/fs/tar"+q("path", dst), evil.Bytes(), nil), 400, api.CodeInvalid)
	if exists(filepath.Join(filepath.Dir(dst), "escape")) {
		t.Fatal("tar entry escaped")
	}
	// So is one written through a symlink that points out.
	os.Symlink("/", filepath.Join(dst, "out"))
	evil.Reset()
	tw = tar.NewWriter(&evil)
	tw.WriteHeader(&tar.Header{Name: "out/tmp/loam-escape-test", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	tw.Write([]byte("x"))
	tw.Close()
	resp = h.do("PUT", "/fs/tar"+q("path", dst), evil.Bytes(), nil)
	if exists("/tmp/loam-escape-test") {
		os.Remove("/tmp/loam-escape-test")
		t.Fatal("extraction followed a symlink out")
	}
	h.wantStatus(resp, 400, api.CodeInvalid)
	// And an absolute name.
	evil.Reset()
	tw = tar.NewWriter(&evil)
	tw.WriteHeader(&tar.Header{Name: "/etc/loam-abs", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	tw.Write([]byte("x"))
	tw.Close()
	h.wantStatus(h.do("PUT", "/fs/tar"+q("path", dst), evil.Bytes(), nil), 400, api.CodeInvalid)
	// PUT creates the directory and its parents.
	deep := filepath.Join(t.TempDir(), "a", "b", "c")
	h.wantStatus(h.do("PUT", "/fs/tar"+q("path", deep), archive, nil), 204, "")
	if !exists(filepath.Join(deep, "top.txt")) {
		t.Fatal("no extraction into new parents")
	}
}

func TestWatch(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	conn := h.dial("/fs/watch", url.Values{"path": {dir}, "recursive": {"true"}})
	time.Sleep(50 * time.Millisecond)
	mustWrite(t, filepath.Join(dir, "sub", "f"), "x", 0o644)
	seen := map[string]bool{}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for !seen["create "+filepath.Join(dir, "sub", "f")] {
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("after %v: %v", seen, err)
		}
		var ev api.WatchEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			t.Fatal(err)
		}
		seen[ev.Type+" "+ev.Path] = true
	}
	if !seen["create "+filepath.Join(dir, "sub")] {
		t.Fatalf("no create for the new directory: %v", seen)
	}
	h.wantStatus(h.do("GET", "/fs/watch"+q("path", "/nonexistent-loam"), nil, nil), 400, api.CodeInvalid) // not a WebSocket
}
