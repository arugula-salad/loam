//go:build e2e

package e2e

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/loam/pkg/api"
	"github.com/arugula-salad/loam/pkg/client"
)

// A sandbox made from an image rather than a template (docs/API.md, "image"):
// the guest image's own digest, taken from the template that uses it, since
// it is the one image every deployment's registry allowlist must admit.
func TestCreateFromImage(t *testing.T) {
	c := newClient(t)
	ctx := testCtx(t, 8*time.Minute)
	want := os.Getenv("LOAM_E2E_TEMPLATE")
	if want == "" {
		want = "base"
	}
	tpls, err := c.ListTemplates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var image string
	for _, tpl := range tpls {
		if tpl.Name == want {
			image = tpl.Image
		}
	}
	if !strings.Contains(image, "@sha256:") {
		t.Skipf("template %q has no digest image (%q) to create from", want, image)
	}

	sb := newSandbox(t, ctx, c, "img", api.CreateSandbox{Image: image})
	if sb.Image != image || sb.Template != "" || sb.Resources != (api.Resources{CPUs: 2, MemoryMiB: 2048, DiskGiB: 20}) {
		t.Fatalf("created: %+v", sb)
	}
	sh(t, ctx, c, sb.Name, "echo kept > ~/file")

	// A stop and a wake boot it fresh on its own disk; the record still says
	// what it was made from.
	if _, err := c.Stop(ctx, sb.Name); err != nil {
		t.Fatal(err)
	}
	if w, err := c.Wake(ctx, sb.Name); err != nil || w.Image != image {
		t.Fatalf("wake: %+v %v", w, err)
	}
	if out := sh(t, ctx, c, sb.Name, "cat ~/file"); out != "kept\n" {
		t.Fatalf("after a fresh boot: %q", out)
	}

	// A fork keeps the image.
	cp, err := c.CreateCheckpoint(ctx, sb.Name, api.CreateCheckpoint{})
	if err != nil {
		t.Fatal(err)
	}
	fork := newSandbox(t, ctx, c, "imgfork", api.CreateSandbox{From: &api.From{Sandbox: sb.Name, Checkpoint: cp.ID}})
	if fork.Image != image || fork.Template != "" {
		t.Fatalf("fork: %+v", fork)
	}
	if out := sh(t, ctx, c, fork.Name, "cat ~/file"); out != "kept\n" {
		t.Fatalf("the fork's disk: %q", out)
	}

	// A tag, or an image and a template together, is refused.
	repo, _, _ := strings.Cut(image, "@")
	for _, req := range []api.CreateSandbox{
		{Name: uniqueName("tag"), Image: repo + ":latest"},
		{Name: uniqueName("both"), Image: image, Template: want},
	} {
		if _, err := c.CreateSandbox(ctx, req); client.ErrorCode(err) != api.CodeInvalid {
			cleanup(t, c, req.Name)
			t.Errorf("%+v: %v, want invalid", req, err)
		}
	}
}
