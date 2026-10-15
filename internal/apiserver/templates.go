package apiserver

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"sort"

	"go.yaml.in/yaml/v3"

	"github.com/arugula-salad/loam/pkg/api"
)

// Templates are the operator's (docs/API.md, "Templates"): loam-api's
// --templates file, by name. Phase 1 has no API to build one.
type Templates map[string]api.Template

// templateFile is the --templates file:
//
//	templates:
//	  - name: base
//	    image: ghcr.io/arugula-salad/loam-base@sha256:...
//	    resources: {cpus: 2, memory_mib: 2048, disk_gib: 20}
type templateFile struct {
	Templates []struct {
		Name      string `yaml:"name"`
		Image     string `yaml:"image"`
		Resources struct {
			CPUs      int `yaml:"cpus"`
			MemoryMiB int `yaml:"memory_mib"`
			DiskGiB   int `yaml:"disk_gib"`
		} `yaml:"resources"`
	} `yaml:"templates"`
}

// LoadTemplates reads a --templates file. Every template needs a name, an
// image and whole resources: they are the defaults a create fills in.
func LoadTemplates(path string) (Templates, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f templateFile
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := Templates{}
	for _, t := range f.Templates {
		res := api.Resources{CPUs: t.Resources.CPUs, MemoryMiB: t.Resources.MemoryMiB, DiskGiB: t.Resources.DiskGiB}
		if !validName(t.Name) || t.Image == "" {
			return nil, fmt.Errorf("%s: template %q needs a name (a DNS label) and an image", path, t.Name)
		}
		if err := checkResources(res); err != nil {
			return nil, fmt.Errorf("%s: template %q: %v", path, t.Name, err)
		}
		if _, dup := out[t.Name]; dup {
			return nil, fmt.Errorf("%s: template %q twice", path, t.Name)
		}
		out[t.Name] = api.Template{Name: t.Name, Image: t.Image, Resources: res}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no templates", path)
	}
	return out, nil
}

// Resource bounds a sandbox may ask for. The node's own memory is the real
// limit; these catch a typo before it reaches one.
const (
	maxCPUs      = 32
	minMemoryMiB = 128
	maxMemoryMiB = 256 << 10
	maxDiskGiB   = 2048
)

func checkResources(r api.Resources) error {
	switch {
	case r.CPUs < 1 || r.CPUs > maxCPUs:
		return fmt.Errorf("cpus must be 1 to %d", maxCPUs)
	case r.MemoryMiB < minMemoryMiB || r.MemoryMiB > maxMemoryMiB:
		return fmt.Errorf("memory_mib must be %d to %d", minMemoryMiB, maxMemoryMiB)
	case r.DiskGiB < 1 || r.DiskGiB > maxDiskGiB:
		return fmt.Errorf("disk_gib must be 1 to %d", maxDiskGiB)
	}
	return nil
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request, _ caller) {
	out := []api.Template{}
	for _, t := range s.opts.Templates {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, api.TemplateList{Templates: out})
}
