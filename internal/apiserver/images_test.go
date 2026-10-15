package apiserver

import (
	"strings"
	"testing"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

var testImage = "ghcr.io/arugula-salad/app@sha256:" + strings.Repeat("ab", 32)

func allowArugula(o *Options) { o.AllowedRegistries = []string{"ghcr.io/arugula-salad"} }

// A sandbox made from an image: no template, the image's digest on every
// create the nodes get (fresh, a fork, a move), its resources defaulted.
func TestCreateFromImage(t *testing.T) {
	h := newClusterWith(t, allowArugula, nodeSpec{"a", 8192}, nodeSpec{"b", 8192})
	key := h.project("jake")
	sb := h.create(key, api.CreateSandbox{Name: "img", Image: testImage})
	if sb.Template != "" || sb.Image != testImage || sb.Resources != defaultImageResources {
		t.Fatalf("created: %+v", sb)
	}
	want := nodeapi.Template{Image: testImage}
	node := h.record(sb.ID).Node
	if vm, _, ok := h.fakes[node].VM(sb.ID); !ok || vm.Template != want {
		t.Fatalf("the node's VM: %+v", vm)
	}
	if got := h.get(key, "img"); got.Image != testImage || got.Template != "" {
		t.Fatalf("read back: %+v", got)
	}
	// A template sandbox shows no image.
	if tpl := h.create(key, api.CreateSandbox{Name: "tpl"}); tpl.Image != "" || tpl.Template != "base" {
		t.Fatalf("template sandbox: %+v", tpl)
	}
	small := h.create(key, api.CreateSandbox{Name: "small", Image: testImage, Resources: &api.Resources{MemoryMiB: 512}})
	if small.Resources != (api.Resources{CPUs: 2, MemoryMiB: 512, DiskGiB: 20}) {
		t.Fatalf("resources: %+v", small.Resources)
	}

	// A fork keeps the image, and may not name one.
	h.must(201, "POST", "/v1/sandboxes/img/checkpoints", key, map[string]any{}, nil)
	fork := h.create(key, api.CreateSandbox{Name: "fork", From: &api.From{Sandbox: "img", Checkpoint: "v1"}})
	if fork.Image != testImage || fork.Template != "" {
		t.Fatalf("fork: %+v", fork)
	}
	if vm, _, _ := h.fakes[h.record(fork.ID).Node].VM(fork.ID); vm.Template != want {
		t.Fatalf("the fork's VM: %+v", vm)
	}
	h.errCode(400, api.CodeInvalid, "POST", "/v1/sandboxes", key,
		api.CreateSandbox{Name: "fork2", Image: testImage, From: &api.From{Sandbox: "img", Checkpoint: "v1"}})

	// A move rebuilds it from its snapshot under the same nameless template;
	// the node refuses one that names another (the fake checks, as the real
	// nodelet does).
	h.must(200, "POST", "/v1/sandboxes/img/pause", key, nil, nil)
	h.fakes[node].SetReady(false)
	h.exec(key, "img", "true")
	moved := h.record(sb.ID)
	if moved.Node == node {
		t.Fatalf("didn't move: %+v", moved)
	}
	if vm, _, _ := h.fakes[moved.Node].VM(sb.ID); vm.Template != want {
		t.Fatalf("the moved VM: %+v", vm)
	}
}

func TestImageRefusals(t *testing.T) {
	h := newClusterWith(t, allowArugula, nodeSpec{"geek", 8192})
	key := h.project("jake")
	digest := "@sha256:" + strings.Repeat("0", 64)
	for _, img := range []string{
		"ghcr.io/arugula-salad/app:1",                                  // a tag
		"ghcr.io/arugula-salad/app:1" + digest,                         // a tag and a digest
		"ghcr.io/arugula-salad/app@sha256:abc",                         // a short digest
		"ghcr.io/arugula-salad/App" + digest,                           // not lowercase
		"docker.io/library/alpine" + digest,                            // another registry
		"ghcr.io/arugula-salad-evil/app" + digest,                      // a lookalike prefix
		"ghcr.io/arugula-salad/app" + digest + "/x",                    // trailing junk
		"https://ghcr.io/arugula-salad/app" + digest,                   // a URL
		"ghcr.io/arugula-salad/../other/app" + digest,                  // path tricks
		"ghcr.io/arugula-salad/app@sha512:" + strings.Repeat("0", 128), // only sha256
	} {
		status, raw, _ := h.call("POST", "/v1/sandboxes", key, api.CreateSandbox{Image: img})
		if status != 400 || !strings.Contains(string(raw), api.CodeInvalid) {
			t.Errorf("%s: %d %s, want 400 invalid", img, status, raw)
		}
	}
	// A prefix matches itself as well as what is under it.
	h.create(key, api.CreateSandbox{Image: "ghcr.io/arugula-salad" + digest})
	h.errCode(400, api.CodeInvalid, "POST", "/v1/sandboxes", key, api.CreateSandbox{Template: "base", Image: testImage})

	// A deployment that allows no registry has no images at all.
	none := newHarness(t)
	k := none.project("jake")
	none.errCode(501, api.CodeUnsupported, "POST", "/v1/sandboxes", k, api.CreateSandbox{Image: testImage})
}

func TestParseAllowedRegistries(t *testing.T) {
	got, err := ParseAllowedRegistries(" ghcr.io/arugula-salad/ , registry.local:5000,,")
	if err != nil || strings.Join(got, " ") != "ghcr.io/arugula-salad registry.local:5000" {
		t.Fatalf("%q %v", got, err)
	}
	if got, err := ParseAllowedRegistries(""); err != nil || len(got) != 0 {
		t.Fatalf("empty: %q %v", got, err)
	}
	for _, bad := range []string{"https://ghcr.io", "GHCR.io", "ghcr.io/a b", "ghcr.io//x"} {
		if _, err := ParseAllowedRegistries(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
