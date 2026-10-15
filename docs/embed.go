// Package docs carries loam's documentation into the binaries that serve it:
// loam-api renders the user docs, primer.md and reference/*.md, at /docs
// (internal/docsite). The contracts beside them are for loam's developers and
// are not served.
package docs

import "embed"

// FS is this directory. The pattern is the whole directory, not the user docs
// by name, because go:embed fails the build on a pattern that matches nothing:
// naming primer.md or reference/ here would tie building loam to the pages
// existing. internal/docsite picks the pages it serves out of FS and serves
// what is there. The contracts and spikes come along too; they are small, and
// in the repository anyway.
//
//go:embed *
var FS embed.FS
