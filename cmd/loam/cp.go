package main

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/arugula-salad/loam/pkg/client"
)

// remoteSpec matches <sandbox>:/abs/path. A local path with a colon in it can
// be written ./like:this.
var remoteSpec = regexp.MustCompile(`^([a-z0-9][a-z0-9-]*):(/.*)$`)

type cpEnd struct {
	sandbox, path string // sandbox empty: local
}

func parseCpEnd(s string) cpEnd {
	if m := remoteSpec.FindStringSubmatch(s); m != nil {
		return cpEnd{sandbox: m[1], path: m[2]}
	}
	return cpEnd{path: s}
}

func cmdCp(a *app, args []string) error {
	fs := a.flags("loam cp")
	user := fs.String("user", "", "do the sandbox side as this user (default root), whose permissions then apply")
	pos, err := parseArgs(fs, args, -1)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errUsage
	}
	src, dst := parseCpEnd(pos[0]), parseCpEnd(pos[1])
	var opts []client.FSOption
	if *user != "" {
		opts = append(opts, client.AsUser(*user))
	}
	switch {
	case src.sandbox == "" && dst.sandbox == "":
		return errors.New("cp: one side must be in a sandbox (<sandbox>:/path)")
	case src.sandbox != "" && dst.sandbox != "":
		return errors.New("cp: copying between sandboxes is not supported; copy through a local directory")
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	if dst.sandbox != "" {
		return a.upload(c, src.path, dst, opts)
	}
	return a.download(c, src, dst.path, opts)
}

// upload copies a local file or directory (or stdin, "-") into a sandbox. Like
// cp, a destination that is an existing directory receives the source by its name.
func (a *app) upload(c *client.Client, src string, dst cpEnd, opts []client.FSOption) error {
	target := dst.path
	if src == "-" {
		_, err := c.WriteFile(a.ctx, dst.sandbox, target, a.stdin, append(opts, client.MakeParents())...)
		return err
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if st, err := c.Stat(a.ctx, dst.sandbox, target, opts...); err == nil && st.Type == "dir" {
		target = path.Join(target, filepath.Base(filepath.Clean(src)))
	} else if err != nil && !client.IsNotFound(err) {
		return err
	}
	if !info.IsDir() {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = c.WriteFile(a.ctx, dst.sandbox, target, f, append(opts, client.Mode(info.Mode()), client.MakeParents())...)
		return err
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeTar(pw, src)) }()
	err = c.PutTar(a.ctx, dst.sandbox, target, pr, opts...)
	pr.CloseWithError(err)
	return err
}

// download copies a sandbox file or directory to a local path (or a file to stdout, "-").
func (a *app) download(c *client.Client, src cpEnd, dst string, opts []client.FSOption) error {
	st, err := c.Stat(a.ctx, src.sandbox, src.path, opts...)
	if err != nil {
		return err
	}
	if dst == "-" {
		if st.Type == "dir" {
			return errors.New("cp: a directory can't go to stdout")
		}
		r, err := c.OpenFile(a.ctx, src.sandbox, src.path, opts...)
		if err != nil {
			return err
		}
		defer r.Close()
		_, err = io.Copy(a.stdout, r)
		return err
	}
	target := dst
	if info, err := os.Stat(dst); err == nil && info.IsDir() {
		target = filepath.Join(dst, path.Base(src.path))
	}
	if st.Type != "dir" {
		r, err := c.OpenFile(a.ctx, src.sandbox, src.path, opts...)
		if err != nil {
			return err
		}
		defer r.Close()
		mode := fs.FileMode(0o644)
		if m, err := strconv.ParseUint(st.Mode, 8, 32); err == nil {
			mode = fs.FileMode(m).Perm()
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, r); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
	r, err := c.GetTar(a.ctx, src.sandbox, src.path, opts...)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	return extractTar(r, target)
}

// writeTar writes the tree under dir as a tar whose entries are relative to
// it, which is what PUT fs/tar extracts into its path.
func writeTar(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		} else if !info.IsDir() && !info.Mode().IsRegular() {
			return nil // sockets, devices, fifos: nothing to copy
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			h.Name += "/"
		}
		// Owners on this machine mean nothing in the sandbox.
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// extractTar extracts r into dir. It goes through an os.Root, so neither a
// "../" name nor a symlink in the archive can write outside dir.
func extractTar(r io.Reader, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "/"))
		if name == "." {
			continue
		}
		if name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("tar entry %q leaves the directory", h.Name)
		}
		mode := fs.FileMode(h.Mode).Perm()
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return err
			}
			root.Chmod(name, mode|0o700)
		case tar.TypeReg:
			if d := path.Dir(name); d != "." {
				if err := root.MkdirAll(d, 0o755); err != nil {
					return err
				}
			}
			f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if d := path.Dir(name); d != "." {
				if err := root.MkdirAll(d, 0o755); err != nil {
					return err
				}
			}
			root.Remove(name)
			if err := root.Symlink(h.Linkname, name); err != nil {
				return err
			}
		default:
			// Hard links, devices and fifos are not copied.
		}
	}
}
