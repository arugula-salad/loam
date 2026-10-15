package apiserver

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/arugula-salad/loam/pkg/api"
)

// Images on create (docs/API.md, "image"): a sandbox made from an OCI image
// rather than a template.

// defaultImageResources are an image sandbox's resources when it gives none:
// the base template's, so that the two start alike.
var defaultImageResources = api.Resources{CPUs: 2, MemoryMiB: 2048, DiskGiB: 20}

// imageRE is <repository>@sha256:<digest>: a registry host (with a port if it
// has one), then path components of lowercase letters, digits and the
// separators the distribution spec allows. A tag has no place in it: every
// node must build the same disk from the reference, and a tag can move
// between one node's pull and the next's.
var imageRE = regexp.MustCompile(`^([a-z0-9.-]+(?::[0-9]+)?(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)+)@sha256:[0-9a-f]{64}$`)

// registryRE is an --allowed-registries entry: a registry host, and
// optionally a path under it.
var registryRE = regexp.MustCompile(`^[a-z0-9.-]+(?::[0-9]+)?(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)

// ParseAllowedRegistries reads --allowed-registries: comma-separated
// repository prefixes (ghcr.io/arugula-salad), each matching itself and the
// repositories under it, never a sibling that merely starts with the same
// letters (ghcr.io/arugula-salad-evil).
func ParseAllowedRegistries(v string) ([]string, error) {
	var out []string
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSuffix(strings.TrimSpace(p), "/")
		if p == "" {
			continue
		}
		if !registryRE.MatchString(p) {
			return nil, fmt.Errorf("allowed registry %q: want a registry host and optionally a path under it, like ghcr.io/arugula-salad", p)
		}
		out = append(out, p)
	}
	return out, nil
}

// checkImage checks an image a create names: a digest reference to a
// repository under one of the allowed prefixes. A deployment that allows none
// can't make sandboxes from images at all.
func (s *Server) checkImage(ref string) error {
	if len(s.opts.AllowedRegistries) == 0 {
		return unsupported("this deployment allows no image registries, so sandboxes are made from templates only")
	}
	m := imageRE.FindStringSubmatch(ref)
	if m == nil {
		return invalid("image %q: want <repository>@sha256:<64 hex digits>, never a tag", ref)
	}
	repo := m[1]
	for _, p := range s.opts.AllowedRegistries {
		if repo == p || strings.HasPrefix(repo, p+"/") {
			return nil
		}
	}
	return invalid("image %q: its repository is not under an allowed registry (%s)", ref, strings.Join(s.opts.AllowedRegistries, ", "))
}
