package apiserver

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// URL domains (docs/API.md, "URL domains"): the operator gives a project
// hostnames of its own, under which its sandbox named N is N.<domain> and its
// ports <port>--N.<domain>. They are the operator's, like templates: loam-api's
// --domains file, with a certificate and a route for each in the deploy.

// Domains maps a URL domain to the project it belongs to.
type Domains map[string]string

// domainFile is the --domains file (YAML, or JSON, which is YAML):
//
//	domains:
//	  - domain: apps.example.com
//	    project: apps
type domainFile struct {
	Domains []struct {
		Domain  string `yaml:"domain"`
		Project string `yaml:"project"`
	} `yaml:"domains"`
}

// dnsLabelRE is one label of a host name.
var dnsLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// validDomain says whether d is a lowercase DNS name of at least two labels,
// with room under it for the longest sandbox label ("<port>--<name>.").
func validDomain(d string) bool {
	labels := strings.Split(d, ".")
	if len(labels) < 2 || len(d)+5+2+32+1 > 253 {
		return false
	}
	for _, l := range labels {
		if !dnsLabelRE.MatchString(l) {
			return false
		}
	}
	return true
}

// under says whether host is d or a name under it.
func under(host, d string) bool { return host == d || strings.HasSuffix(host, "."+d) }

// LoadDomains reads a --domains file. Each domain belongs to one project (a
// project may have several), and none may be loam's own --domain, or one of
// the others, or lie under either: a host must name one sandbox, never two
// depending on which suffix is tried first. A project that doesn't exist
// yet is no error; its domain matches nothing until it does.
func LoadDomains(path, loamDomain string) (Domains, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f domainFile
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	// An empty file is an empty list, so that the deploy can carry the
	// mechanism before it carries a domain.
	if err := dec.Decode(&f); err != nil && len(bytes.TrimSpace(raw)) > 0 {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := Domains{}
	for _, e := range f.Domains {
		d := strings.TrimSuffix(e.Domain, ".")
		if !validDomain(d) {
			return nil, fmt.Errorf("%s: domain %q is not a lowercase DNS name of at least two labels", path, e.Domain)
		}
		if !validName(e.Project) {
			return nil, fmt.Errorf("%s: domain %q: project %q is not a project name", path, d, e.Project)
		}
		if loamDomain != "" && (under(d, loamDomain) || under(loamDomain, d)) {
			return nil, fmt.Errorf("%s: domain %q overlaps loam's own domain %q", path, d, loamDomain)
		}
		for o := range out {
			if under(d, o) || under(o, d) {
				return nil, fmt.Errorf("%s: domain %q overlaps %q", path, d, o)
			}
		}
		out[d] = e.Project
	}
	return out, nil
}

// of is a project's domains, sorted.
func (ds Domains) of(project string) []string {
	out := []string{}
	for d, p := range ds {
		if p == project {
			out = append(out, d)
		}
	}
	slices.Sort(out)
	return out
}

// domainTarget is what a host under a project's URL domain names:
// N.<domain> is sandbox N's http_port, <port>--N.<domain> any port. matched
// says the host is under one of the domains at all, so that a host there that
// names no sandbox is answered as one (not found), never by the API.
func (s *Server) domainTarget(host string) (t portTarget, matched, ok bool) {
	for d, project := range s.opts.URLDomains {
		if !under(host, d) {
			continue
		}
		label, found := strings.CutSuffix(host, "."+d)
		if !found || strings.Contains(label, ".") {
			return portTarget{}, true, false
		}
		t.project, t.name = project, label
		if p, name, two := strings.Cut(label, "--"); two {
			n, err := strconv.Atoi(p)
			if err != nil || checkPort(n) != nil || strconv.Itoa(n) != p {
				return portTarget{}, true, false
			}
			t.port, t.name = n, name
		}
		return t, true, validName(t.name)
	}
	return portTarget{}, false, false
}
