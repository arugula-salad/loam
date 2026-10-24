package agent

import (
	"errors"
	"github.com/arugula-salad/loam/internal/nodeapi"
	"os"
	"path/filepath"
	"testing"
)

func TestSandboxIdentityFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "e2b")
	for _, id := range []string{"sb-source", "sb-source", "sb-fork"} {
		if err := writeSandboxID(dir, id); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, ".E2B_SANDBOX_ID")
		b, err := os.ReadFile(p)
		if err != nil || string(b) != id {
			t.Fatalf("identity %q: %q %v", id, b, err)
		}
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != 0o644 {
			t.Fatalf("identity permissions: %v %v", info, err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("left temporary files: %v", entries)
	}
}

func TestWakeRefreshesSandboxIdentity(t *testing.T) {
	h := newHarness(t)
	for _, id := range []string{"sb-source", "sb-source", "sb-fork"} {
		h.wantStatus(h.do("POST", "/internal/wake", nodeapi.Wake{Sandbox: id}, nil), 204, "")
		if h.sys.sandboxID != id {
			t.Fatalf("stale identity: %s", h.sys.sandboxID)
		}
	}
	h.sys.identityErr = errors.New("unwritable identity")
	h.wantStatus(h.do("POST", "/internal/wake", nodeapi.Wake{Sandbox: "sb-new"}, nil), 500, "internal")
	if h.srv.wake.Sandbox != "sb-fork" {
		t.Fatal("failed wake was recorded")
	}
}
