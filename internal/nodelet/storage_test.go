package nodelet

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A clone has the source's contents, and keeps its holes as holes, by
// reflink where the volume has them and by sparse copy where copies are allowed.
func TestClone(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	f, _ := os.Create(src)
	f.Truncate(64 << 20)
	f.WriteAt([]byte("head"), 0)
	f.WriteAt([]byte("middle"), 32<<20)
	f.Close()
	reflinks := ProbeReflink(dir)
	t.Logf("%s supports reflinks: %v", dir, reflinks)

	dst := filepath.Join(dir, "dst")
	err := cloner{}.clone(src, dst)
	switch {
	case reflinks && err != nil:
		t.Fatalf("reflink clone: %v", err)
	case !reflinks && !errors.Is(err, errNoReflink):
		t.Fatalf("clone without reflinks or copies: %v", err)
	}
	if !reflinks && fileExists(dst+".tmp") {
		t.Error("a refused clone left its temporary file")
	}
	if err := (cloner{allowCopy: true}).clone(src, dst); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(src)
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, want) {
		t.Fatal("the clone's contents differ")
	}
	if a := allocated(dst); a > 1<<20 {
		t.Errorf("a 64 MiB file with 10 bytes of data cloned into %d bytes", a)
	}
}

// The data directory must support reflinks, unless the dev flag says otherwise.
func TestNewRefusesNoReflink(t *testing.T) {
	dir := t.TempDir()
	if ProbeReflink(dir) {
		t.Skipf("%s supports reflinks", dir)
	}
	_, err := New(Options{Data: dir, MemoryMiB: 1024, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, &fakeVMM{agent: &fakeAgent{}}, nil, nil)
	if err == nil {
		t.Fatal("a nodelet started on a volume without reflinks")
	}
}

func TestWriteFileAtomic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := writeFileAtomic(p, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(p, []byte("two")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "two" || fileExists(p+".tmp") {
		t.Errorf("got %q", b)
	}
	var st syscall.Stat_t
	syscall.Stat(p, &st)
}
