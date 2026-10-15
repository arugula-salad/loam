//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/loam/pkg/api"
	"github.com/arugula-salad/loam/pkg/client"
)

// startSleeper starts a detachable `sleep` that records its PID, detaches,
// and returns the PID.
func startSleeper(t *testing.T, c *client.Client, ctx context.Context, sandbox string) int {
	t.Helper()
	s, err := c.Exec(ctx, sandbox, client.ExecOptions{Cmd: []string{"sleep", "3600"}, Detachable: true})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	return s.PID
}

// alive reports whether pid is a running sleep in the sandbox (not a reused PID).
func alive(t *testing.T, c *client.Client, ctx context.Context, sandbox string, pid int) bool {
	t.Helper()
	return shCode(t, ctx, c, sandbox, "root", fmt.Sprintf("grep -q '^sleep' /proc/%d/cmdline", pid)) == 0
}

// bootID tells boots apart: it changes on a fresh boot and survives a memory restore.
func bootID(t *testing.T, c *client.Client, ctx context.Context, sandbox string) string {
	t.Helper()
	return strings.TrimSpace(sh(t, ctx, c, sandbox, "cat /proc/sys/kernel/random/boot_id"))
}

func TestCheckpoints(t *testing.T) {
	c := newClient(t)
	ctx := testCtx(t, 10*time.Minute)
	sb := newSandbox(t, ctx, c, "cp", api.CreateSandbox{})

	sh(t, ctx, c, sb.Name, "echo before > ~/state")
	pid := startSleeper(t, c, ctx, sb.Name)
	boot := bootID(t, c, ctx, sb.Name)

	mem, err := c.CreateCheckpoint(ctx, sb.Name, api.CreateCheckpoint{Comment: "known good"})
	if err != nil || !mem.Memory || mem.Auto || mem.Comment != "known good" || mem.ID != "v1" {
		t.Fatalf("memory checkpoint: %+v %v", mem, err)
	}
	if got, _ := c.GetSandbox(ctx, sb.Name); got.State != api.StateRunning {
		t.Fatalf("after a memory checkpoint the sandbox is %s", got.State)
	}

	// A disk-only checkpoint doesn't pause: the process goes on.
	sh(t, ctx, c, sb.Name, "echo disk > ~/state")
	f := false
	disk, err := c.CreateCheckpoint(ctx, sb.Name, api.CreateCheckpoint{Memory: &f})
	if err != nil || disk.Memory || disk.ID != "v2" {
		t.Fatalf("disk checkpoint: %+v %v", disk, err)
	}
	if !alive(t, c, ctx, sb.Name, pid) {
		t.Fatal("a disk-only checkpoint stopped a process")
	}

	if l, err := c.ListCheckpoints(ctx, sb.Name); err != nil || len(l) != 2 || l[0].ID != "v1" || l[1].ID != "v2" {
		t.Fatalf("list: %+v %v", l, err)
	}

	t.Run("memory restore resumes processes", func(t *testing.T) {
		sh(t, ctx, c, sb.Name, "echo after > ~/state; sudo rm -f /usr/bin/tee")
		sh(t, ctx, c, sb.Name, fmt.Sprintf("sudo kill %d", pid))
		// A stream open across the restore ends without an exit status.
		open, err := c.Exec(ctx, sb.Name, client.ExecOptions{Cmd: []string{"sleep", "300"}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.RestoreCheckpoint(ctx, sb.Name, mem.ID)
		if err != nil || got.State != api.StateRunning {
			t.Fatalf("restore: %+v %v", got, err)
		}
		if _, err := open.Wait(); !errors.Is(err, client.ErrExitUnknown) {
			t.Fatalf("a stream across the restore: %v", err)
		}
		if out := sh(t, ctx, c, sb.Name, "cat ~/state; test -x /usr/bin/tee && echo tee-is-back"); out != "before\ntee-is-back\n" {
			t.Fatalf("after restore: %q", out)
		}
		if !alive(t, c, ctx, sb.Name, pid) {
			t.Fatal("the process from the checkpoint isn't running")
		}
		if b := bootID(t, c, ctx, sb.Name); b != boot {
			t.Fatalf("a memory restore booted again: %s, was %s", b, boot)
		}
	})

	t.Run("the state before a restore is kept", func(t *testing.T) {
		l, err := c.ListCheckpoints(ctx, sb.Name)
		if err != nil {
			t.Fatal(err)
		}
		var auto *api.Checkpoint
		for i := range l {
			if l[i].Auto {
				auto = &l[i]
			}
		}
		if auto == nil || !strings.HasPrefix(auto.ID, "auto-") {
			t.Fatalf("no automatic checkpoint: %+v", l)
		}
	})

	t.Run("disk restore boots fresh", func(t *testing.T) {
		if _, err := c.RestoreCheckpoint(ctx, sb.Name, disk.ID); err != nil {
			t.Fatal(err)
		}
		if out := sh(t, ctx, c, sb.Name, "cat ~/state"); out != "disk\n" {
			t.Fatalf("after a disk restore: %q", out)
		}
		if alive(t, c, ctx, sb.Name, pid) {
			t.Fatal("a process survived a disk-only restore")
		}
		if b := bootID(t, c, ctx, sb.Name); b == boot {
			t.Fatal("a disk-only restore didn't boot fresh")
		}
	})

	t.Run("fork from memory", func(t *testing.T) {
		name := uniqueName("fork")
		fk, err := c.Fork(ctx, sb.Name, mem.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		cleanup(t, c, name)
		if fk.State != api.StateRunning || fk.Name != name || fk.ID == sb.ID {
			t.Fatalf("fork: %+v", fk)
		}
		if out := sh(t, ctx, c, name, "cat ~/state"); out != "before\n" {
			t.Fatalf("fork's disk: %q", out)
		}
		if !alive(t, c, ctx, name, pid) {
			t.Fatal("the fork from a memory checkpoint lost its processes")
		}
		// The fork is its own sandbox.
		sh(t, ctx, c, name, "echo forked > ~/state")
		if out := sh(t, ctx, c, sb.Name, "cat ~/state"); out != "disk\n" {
			t.Fatalf("the fork wrote into its source: %q", out)
		}
	})

	t.Run("fork from disk", func(t *testing.T) {
		name := uniqueName("fork")
		if _, err := c.Fork(ctx, sb.Name, disk.ID, name); err != nil {
			t.Fatal(err)
		}
		cleanup(t, c, name)
		if out := sh(t, ctx, c, name, "cat ~/state"); out != "disk\n" {
			t.Fatalf("fork's disk: %q", out)
		}
		if alive(t, c, ctx, name, pid) {
			t.Fatal("a fork from a disk checkpoint has processes")
		}
	})

	t.Run("a fork may not choose its template or resources", func(t *testing.T) {
		_, err := c.CreateSandbox(ctx, api.CreateSandbox{Name: uniqueName("fork"), Template: "base", From: &api.From{Sandbox: sb.Name, Checkpoint: mem.ID}})
		if client.ErrorCode(err) != api.CodeInvalid {
			t.Fatalf("fork with a template: %v", err)
		}
	})

	t.Run("checkpoint from inside", func(t *testing.T) {
		out := sh(t, ctx, c, sb.Name, `curl -sf --unix-socket /run/loam/api.sock -X POST -H 'Content-Type: application/json' -d '{"comment":"inside"}' http://loam/checkpoints`)
		if !strings.Contains(out, `"comment":"inside"`) {
			t.Fatalf("POST /checkpoints from inside: %q", out)
		}
		eventually(t, 30*time.Second, "the inside checkpoint listed", func() error {
			l, err := c.ListCheckpoints(ctx, sb.Name)
			if err != nil {
				return err
			}
			for _, cp := range l {
				if cp.Comment == "inside" {
					return nil
				}
			}
			return fmt.Errorf("%+v", l)
		})
	})

	t.Run("delete", func(t *testing.T) {
		if err := c.DeleteCheckpoint(ctx, sb.Name, disk.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetCheckpoint(ctx, sb.Name, disk.ID); !client.IsNotFound(err) {
			t.Fatalf("after delete: %v", err)
		}
		if _, err := c.RestoreCheckpoint(ctx, sb.Name, disk.ID); err == nil {
			t.Fatal("restored a deleted checkpoint")
		}
	})
}
