package nodelet

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

type entry struct {
	name, link string
	typ        byte
	body       string
}

func tarOf(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Linkname: e.link, Mode: 0o644, Size: int64(len(e.body)), ModTime: time.Unix(1, 0)}
		if e.typ == tar.TypeDir {
			h.Mode = 0o700
			h.Size = 0
		}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(e.body))
	}
	tw.Close()
	return buf.Bytes()
}

func readTar(t *testing.T, path string) (names []string, bodies map[string]string, modes map[string]int64) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	bodies, modes = map[string]string{}, map[string]int64{}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		b, _ := io.ReadAll(tr)
		bodies[h.Name] = string(b)
		modes[h.Name] = h.Mode
	}
}

// What mutate.Extract streams (top layer first) is reordered for mke2fs:
// directories first, parents before children, made up where missing; hard
// links after their targets; the image's metadata added.
func TestOrderTar(t *testing.T) {
	in := tarOf(t, []entry{
		{name: "usr/bin/tool", typ: tar.TypeReg, body: "tool"},
		{name: "usr/bin/alias", typ: tar.TypeLink, link: "usr/bin/tool2"},
		{name: "./usr/", typ: tar.TypeDir},
		{name: "usr/bin/tool2", typ: tar.TypeReg, body: "tool2"},
		{name: "etc/deep/nested/file", typ: tar.TypeReg, body: "x"},
		{name: "usr/", typ: tar.TypeDir}, // a lower layer's copy: the first one wins
		{name: "bin", typ: tar.TypeSymlink, link: "usr/bin"},
		{name: "./", typ: tar.TypeDir},
	})
	dst := filepath.Join(t.TempDir(), "out.tar")
	if err := orderTar(bytes.NewReader(in), dst, []byte(`{"ref":"x"}`)); err != nil {
		t.Fatal(err)
	}
	names, bodies, modes := readTar(t, dst)
	want := []string{"etc/", "etc/deep/", "etc/deep/nested/", "etc/loam/", "usr/", "usr/bin/",
		"usr/bin/tool", "usr/bin/tool2", "etc/deep/nested/file", "bin", "usr/bin/alias", ImageMetaPath}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Errorf("order\n got %v\nwant %v", names, want)
	}
	if modes["usr/"] != 0o700 || modes["etc/"] != 0o755 {
		t.Errorf("modes: usr/ %o (from the image), etc/ %o (made up)", modes["usr/"], modes["etc/"])
	}
	if bodies["usr/bin/tool2"] != "tool2" || bodies[ImageMetaPath] != `{"ref":"x"}` {
		t.Errorf("bodies: %v", bodies)
	}
}

// An image is pulled from a registry, flattened and written to ext4, once per
// digest and size; a pinned reference needs no registry once cached.
func TestImageCache(t *testing.T) {
	if !mke2fsReadsTar() {
		t.Skip("no mke2fs that reads tar (e2fsprogs 1.47.1 or newer); the nodelet image has one")
	}
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	layer := func(entries []entry) v1.Layer {
		b := tarOf(t, entries)
		l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	img, err := mutate.AppendLayers(empty.Image,
		layer([]entry{{name: "etc/", typ: tar.TypeDir}, {name: "etc/os-release", typ: tar.TypeReg, body: "ID=test"}, {name: "etc/gone", typ: tar.TypeReg, body: "x"}}),
		layer([]entry{{name: "etc/.wh.gone", typ: tar.TypeReg}, {name: "etc/motd", typ: tar.TypeReg, body: "hi"}}))
	if err != nil {
		t.Fatal(err)
	}
	img, _ = mutate.Config(img, v1.Config{Env: []string{"PATH=/usr/bin", "LANG=C.UTF-8"}, User: "loam",
		Entrypoint: []string{"/usr/bin/tool"}, Cmd: []string{"serve", "--port", "8080"}, WorkingDir: "/srv"})
	img, _ = mutate.ConfigFile(img, func() *v1.ConfigFile { c, _ := img.ConfigFile(); c.OS, c.Architecture = "linux", "amd64"; return c }())
	ref, _ := name.ParseReference(host + "/loam/base:1")
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	digest, _ := img.Digest()

	dir := t.TempDir()
	c, err := NewImageCache(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	disk, pinned, err := c.Disk(context.Background(), host+"/loam/base:1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := host + "/loam/base@" + digest.String(); pinned != want {
		t.Errorf("pinned %s, want %s", pinned, want)
	}
	// Another node builds the same disk, byte for byte: snapshots store only
	// their difference from it.
	other, err := NewImageCache(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	disk2, _, err := other.Disk(context.Background(), pinned, 1)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := fileSum(t, disk), fileSum(t, disk2); a != b {
		t.Error("two builds of one image differ")
	}
	if fi, err := os.Stat(disk); err != nil || fi.Size() != 1<<30 || !strings.Contains(disk, digest.Hex) {
		t.Fatalf("disk %s: %v %v", disk, fi, err)
	}
	if out, err := exec.Command("debugfs", "-R", "cat /etc/motd", disk).Output(); err == nil && string(out) != "hi" {
		t.Errorf("/etc/motd on the disk: %q", out)
	}
	if out, err := exec.Command("debugfs", "-R", "ls -p /etc", disk).Output(); err == nil && strings.Contains(string(out), "gone") {
		t.Error("a whited-out file is on the disk")
	}
	if out, err := exec.Command("debugfs", "-R", "cat /"+ImageMetaPath, disk).Output(); err == nil {
		var meta ImageMeta
		if json.Unmarshal(out, &meta) != nil || len(meta.Env) != 2 || meta.Env[1] != "LANG=C.UTF-8" ||
			len(meta.Entrypoint) != 1 || meta.Entrypoint[0] != "/usr/bin/tool" || len(meta.Cmd) != 3 || meta.Cmd[2] != "8080" ||
			meta.Workdir != "/srv" || meta.User != "loam" {
			t.Errorf("image metadata on the disk: %s", out)
		}
	}

	// Pinned by digest, a cached image is found with the registry gone.
	srv.Close()
	again, _, err := c.Disk(context.Background(), host+"/loam/base@"+digest.String(), 1)
	if err != nil || again != disk {
		t.Errorf("pinned and cached: %s %v", again, err)
	}
	if _, _, err := c.Disk(context.Background(), "not a reference!", 1); code(err) != "invalid" {
		t.Errorf("bad reference: %v", err)
	}
}

func fileSum(t *testing.T, path string) [32]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return [32]byte(h.Sum(nil))
}

// mke2fsReadsTar reports whether the host's mke2fs is 1.47.1 or newer, the
// first to take a tar for -d. CI runners and older hosts have an older one.
func mke2fsReadsTar() bool {
	out, err := exec.Command("mke2fs", "-V").CombinedOutput()
	if err != nil {
		return false
	}
	var a, b, c int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "mke2fs %d.%d.%d", &a, &b, &c); err != nil {
		return false
	}
	return a > 1 || a == 1 && (b > 47 || b == 47 && c >= 1)
}

// Evict drops what is past its age, then the least recently used until the
// cache fits, and never anything used within the floor.
func TestImageCacheEvict(t *testing.T) {
	dir := t.TempDir()
	c, err := NewImageCache(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	write := func(name string, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, 1<<20), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, now.Add(-age), now.Add(-age))
	}
	write("old.tar", 10*24*time.Hour)
	write("old-20g.ext4", 10*24*time.Hour)
	write("lru-20g.ext4", 3*time.Hour)
	write("mid-20g.ext4", 2*time.Hour)
	write("fresh-20g.ext4", time.Minute)
	write("notes.txt", 30*24*time.Hour) // not the cache's

	removed, err := c.Evict(7*24*time.Hour, 2<<20) // room for two of the 1 MiB files
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(removed)
	if want := []string{"lru-20g.ext4", "old-20g.ext4", "old.tar"}; !slices.Equal(removed, want) {
		t.Errorf("removed %v, want %v", removed, want)
	}
	for _, keep := range []string{"mid-20g.ext4", "fresh-20g.ext4", "notes.txt"} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("%s: %v", keep, err)
		}
	}

	// Within the floor nothing goes, however full.
	if removed, _ := c.Evict(0, 0); len(removed) != 1 || removed[0] != "mid-20g.ext4" {
		t.Errorf("at zero limits removed %v, want only mid-20g.ext4 (fresh is within the floor)", removed)
	}
}
