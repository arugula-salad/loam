package cgroup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLimitFiles(t *testing.T) {
	got := LimitFiles(Limits{VCPUs: 2, MemMiB: 1024})
	want := [][2]string{
		{"cpu.max", "300000 100000"},
		{"pids.max", "40"},
		{"memory.max", "1543503872"},  // 1024 + 192 + 256 MiB
		{"memory.high", "1275068416"}, // 1024 + 192 MiB
	}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s = %s, want %s", got[i][0], got[i][1], want[i][1])
		}
	}
}

// A leaf gets its limits written; on a plain directory the files are just files.
func TestCreateLeaf(t *testing.T) {
	r := &Root{vms: t.TempDir()}
	g, err := r.Create("sb-one", Limits{VCPUs: 1, MemMiB: 256})
	if err != nil {
		t.Fatal(err)
	}
	if g.FD() < 0 {
		t.Error("no directory fd")
	}
	b, _ := os.ReadFile(filepath.Join(r.vms, "sb-one", "memory.high"))
	if string(b) != "469762048" {
		t.Errorf("memory.high %q", b)
	}
	g.Close()
	if g.FD() != -1 {
		t.Error("Close kept the fd")
	}
	var nilLeaf *Leaf
	nilLeaf.Close()
	nilLeaf.Remove()
}
