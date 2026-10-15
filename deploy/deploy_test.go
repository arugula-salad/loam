// Package deploy holds no code: deploy/base is a kustomize base, applied by whoever installs loam
// (docs/INSTALL.md). These tests keep it installable by anyone and keep its pinned files in step.
package deploy

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestBaseIsGeneric: nothing in the base or the examples names one particular cluster, its
// secret store, ingress controller or operators. An installation's own names belong in its
// overlay, outside this repo.
func TestBaseIsGeneric(t *testing.T) {
	banned := []string{
		"inevitable.fyi", "home-cloud", "infisical", "garage", "geek", "basil", "fireball",
		"games.arugula.io", "studio.arugula.io", "traefik.io", "cert-manager.io", "cnpg.io",
		"/bulk/", "fountain",
	}
	for _, dir := range []string{"base", "examples"} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			text := strings.ToLower(string(b))
			for _, s := range banned {
				if strings.Contains(text, s) {
					t.Errorf("%s names %q: that belongs in an installation's own overlay", path, s)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

type templateFile struct {
	Templates []struct {
		Name  string `json:"name"`
		Image string `json:"image"`
	} `json:"templates"`
}

func templates(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f templateFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	out := map[string]string{}
	for _, tm := range f.Templates {
		out[tm.Name] = tm.Image
	}
	return out
}

var pinned = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// TestTemplatesAgree: a component that replaces the base's templates.json carries the base's
// templates at the same digests (scripts/pin-images.sh pins both), so opting into one never
// rolls an installation back to older guests.
func TestTemplatesAgree(t *testing.T) {
	base := templates(t, "base/templates.json")
	for _, name := range []string{"base", "e2b"} {
		if !pinned.MatchString(base[name]) {
			t.Errorf("base/templates.json: %s is %q, want an image pinned by digest", name, base[name])
		}
	}
	comps, _ := filepath.Glob("components/*/templates.json")
	for _, c := range comps {
		got := templates(t, c)
		for name, image := range base {
			if got[name] != image {
				t.Errorf("%s: %s is %q, the base's is %q", c, name, got[name], image)
			}
		}
		for name, image := range got {
			if !pinned.MatchString(image) {
				t.Errorf("%s: %s is %q, want an image pinned by digest", c, name, image)
			}
		}
	}
}

// TestImagesPinned: the base's own images are pinned by digest, where scripts/pin-images.sh
// looks for them.
func TestImagesPinned(t *testing.T) {
	for path, image := range map[string]string{
		"base/kustomization.yaml":         "ghcr.io/arugula-salad/loam-api",
		"base/nodelet/kustomization.yaml": "ghcr.io/arugula-salad/loam-nodelet",
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		re := regexp.MustCompile(`newName: ` + regexp.QuoteMeta(image) + `\n\s+digest: sha256:[0-9a-f]{64}\n`)
		if !re.Match(b) {
			t.Errorf("%s: no `newName: %s` followed by its digest", path, image)
		}
	}
}
