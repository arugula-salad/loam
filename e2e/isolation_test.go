//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/loam/pkg/api"
	"github.com/arugula-salad/loam/pkg/client"
)

// TestProjectIsolation makes a second project with its own key and checks
// that key sees none of the first project's sandboxes: every route answers
// 404, as if they didn't exist.
func TestProjectIsolation(t *testing.T) {
	c := newClient(t)
	adminToken := os.Getenv("LOAM_E2E_ADMIN_TOKEN")
	if adminToken == "" {
		t.Skip("LOAM_E2E_ADMIN_TOKEN not set")
	}
	ctx := testCtx(t, 5*time.Minute)
	admin := client.New(e2eURL(), adminToken)

	project := uniqueName("iso")
	if _, err := admin.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	key, err := admin.CreateKey(ctx, project, "e2e")
	if err != nil || !strings.HasPrefix(key.Secret, "loam_") {
		t.Fatalf("key: %+v %v", key, err)
	}
	other := client.New(e2eURL(), key.Secret)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if l, _ := other.ListSandboxes(ctx, client.ListOptions{}); l != nil {
			for _, s := range l {
				other.DeleteSandbox(ctx, s.Name)
			}
		}
		admin.DeleteKey(ctx, project, key.ID)
		// By now the project is empty, so this must succeed.
		if err := admin.DeleteProject(ctx, project); err != nil {
			t.Errorf("cleanup: delete project %s: %v", project, err)
		}
	})

	keys, err := admin.ListKeys(ctx, project)
	if err != nil || len(keys) != 1 || keys[0].Secret != "" {
		t.Fatalf("list keys (no secrets): %+v %v", keys, err)
	}
	if w, err := other.Whoami(ctx); err != nil || w.Project.Name != project {
		t.Fatalf("whoami: %+v %v", w, err)
	}

	sb := newSandbox(t, ctx, c, "iso", api.CreateSandbox{})
	for _, ref := range []string{sb.Name, sb.ID} {
		if _, err := other.GetSandbox(ctx, ref); !client.IsNotFound(err) {
			t.Fatalf("get %s from another project: %v", ref, err)
		}
		if _, err := other.Run(ctx, ref, api.ExecRun{Cmd: []string{"true"}}); !client.IsNotFound(err) {
			t.Fatalf("exec in %s from another project: %v", ref, err)
		}
		if _, err := other.ReadFile(ctx, ref, "/etc/hostname"); !client.IsNotFound(err) {
			t.Fatalf("read a file in %s from another project: %v", ref, err)
		}
		if _, err := other.Exec(ctx, ref, client.ExecOptions{Cmd: []string{"true"}}); !client.IsNotFound(err) {
			t.Fatalf("streaming exec in %s from another project: %v", ref, err)
		}
		if err := other.DeleteSandbox(ctx, ref); !client.IsNotFound(err) {
			t.Fatalf("delete %s from another project: %v", ref, err)
		}
		if _, err := other.Pause(ctx, ref); !client.IsNotFound(err) {
			t.Fatalf("pause %s from another project: %v", ref, err)
		}
	}
	if l, err := other.ListSandboxes(ctx, client.ListOptions{}); err != nil || len(l) != 0 {
		t.Fatalf("list from another project: %+v %v", l, err)
	}
	if _, err := other.ListProjects(ctx); client.ErrorCode(err) != api.CodeUnauthorized {
		t.Fatalf("a project key on an admin route: %v", err)
	}
	// Names are per project: the same name is free in the other one.
	twin, err := other.CreateSandbox(ctx, api.CreateSandbox{Name: sb.Name})
	if err != nil {
		t.Fatalf("the same name in another project: %v", err)
	}
	if twin.ID == sb.ID || twin.Project != project {
		t.Fatalf("twin: %+v", twin)
	}
	if err := admin.DeleteProject(ctx, project); client.ErrorCode(err) != api.CodeProjectNotEmpty {
		t.Fatalf("delete of a project with a sandbox: %v", err)
	}
	if err := other.DeleteSandbox(ctx, twin.Name); err != nil {
		t.Fatal(err)
	}
}

// TestInGuestAPI uses /run/loam/api.sock from inside a sandbox.
func TestInGuestAPI(t *testing.T) {
	c := newClient(t)
	ctx := testCtx(t, 5*time.Minute)
	sb := newSandbox(t, ctx, c, "guest", api.CreateSandbox{})
	curl := `curl -sf --unix-socket /run/loam/api.sock `

	if out := sh(t, ctx, c, sb.Name, "stat -c '%a' /run/loam/api.sock"); out != "666\n" {
		t.Fatalf("socket mode: %q", out)
	}

	var l api.StatList
	out := sh(t, ctx, c, sb.Name, curl+"'http://loam/fs/list?path=/'")
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		t.Fatalf("fs/list: %v: %q", err, out)
	}
	found := false
	for _, e := range l.Entries {
		found = found || (e.Name == "etc" && e.Type == "dir")
	}
	if !found {
		t.Fatalf("fs/list of /: %+v", l.Entries)
	}

	var self api.Sandbox
	out = sh(t, ctx, c, sb.Name, curl+"http://loam/self")
	if err := json.Unmarshal([]byte(out), &self); err != nil || self.ID != sb.ID || self.Name != sb.Name || self.State != api.StateRunning {
		t.Fatalf("GET /self: %+v %v: %q", self, err, out)
	}

	// A write through the socket is the same file API, user and all.
	sh(t, ctx, c, sb.Name, curl+"-X PUT --data-binary inside 'http://loam/fs/file?path=/home/loam/from-inside&user=loam'")
	if b, err := c.ReadFile(ctx, sb.Name, "/home/loam/from-inside"); err != nil || string(b) != "inside" {
		t.Fatalf("file written from inside, read from outside: %q %v", b, err)
	}
}
