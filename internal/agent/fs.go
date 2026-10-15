package agent

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/arugula-salad/loam/pkg/api"
)

// The file API works on the sandbox's disk directly. Paths are absolute;
// operations run as root unless the call names a user, and then as that
// user, permissions and all (asUser).

func (s *Server) registerFS(mux *http.ServeMux) {
	mux.HandleFunc("GET /fs/file", s.fsRead)
	mux.HandleFunc("PUT /fs/file", s.fsWrite)
	mux.HandleFunc("DELETE /fs/file", s.fsDelete)
	mux.HandleFunc("GET /fs/stat", s.fsStat)
	mux.HandleFunc("GET /fs/list", s.fsList)
	mux.HandleFunc("POST /fs/mkdir", s.fsMkdir)
	mux.HandleFunc("POST /fs/rename", s.fsRename)
	mux.HandleFunc("GET /fs/tar", s.fsTarGet)
	mux.HandleFunc("PUT /fs/tar", s.fsTarPut)
	mux.HandleFunc("GET /fs/watch", s.fsWatch)
}

var errInvalid = errors.New("invalid")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errInvalid, fmt.Sprintf(format, args...))
}

// fsFail answers err from a file operation with the right status and code.
func fsFail(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "io_error"
	switch {
	case errors.Is(err, errInvalid):
		status, code = http.StatusBadRequest, api.CodeInvalid
		err = errors.New(strings.TrimPrefix(err.Error(), errInvalid.Error()+": "))
	case errors.Is(err, errUnsupportedUser):
		status, code = http.StatusNotImplemented, api.CodeUnsupported
	case errors.Is(err, fs.ErrNotExist):
		status, code = http.StatusNotFound, api.CodeNotFound
	case errors.Is(err, fs.ErrPermission):
		status, code = http.StatusForbidden, "permission_denied"
	case errors.Is(err, syscall.ENOTEMPTY): // before ErrExist, which it also matches
		status, code = http.StatusConflict, api.CodeConflict
	case errors.Is(err, fs.ErrExist):
		status, code = http.StatusConflict, api.CodeAlreadyExists
	case errors.Is(err, syscall.EISDIR), errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.EXDEV):
		status, code = http.StatusBadRequest, api.CodeInvalid
	}
	writeErr(w, status, code, err.Error())
}

// absPath checks a path from a call: it must be absolute.
func absPath(p string) (string, error) {
	if p == "" {
		return "", invalidf("path is required")
	}
	if !filepath.IsAbs(p) {
		return "", invalidf("path %q is not absolute", p)
	}
	return filepath.Clean(p), nil
}

// fsCall resolves what every file route takes: ?path= and ?user=.
func (s *Server) fsCall(r *http.Request) (string, *account, error) {
	p, err := absPath(r.URL.Query().Get("path"))
	if err != nil {
		return "", nil, err
	}
	a, err := s.fsAccount(r.URL.Query().Get("user"))
	if err != nil {
		if errors.Is(err, errUnsupportedUser) {
			return "", nil, err
		}
		return "", nil, invalidf("%v", err)
	}
	return p, a, nil
}

// parseMode reads an octal mode ("0644"), def when empty.
func parseMode(v string, def fs.FileMode) (fs.FileMode, error) {
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseUint(v, 8, 32)
	if err != nil || n > 0o7777 {
		return 0, invalidf("invalid mode %q", v)
	}
	return unixMode(uint32(n)), nil
}

// unixMode turns permission and set-id bits into an fs.FileMode.
func unixMode(n uint32) fs.FileMode {
	m := fs.FileMode(n & 0o777)
	if n&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if n&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if n&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	return m
}

// statOf describes p without following a final symlink.
func statOf(p string) (api.Stat, error) {
	info, err := os.Lstat(p)
	if err != nil {
		return api.Stat{}, err
	}
	return statFor(p, info), nil
}

func statFor(p string, info fs.FileInfo) api.Stat {
	st := api.Stat{Path: p, Name: filepath.Base(p), Size: info.Size(), MTime: info.ModTime().UTC()}
	switch {
	case info.Mode().IsRegular():
		st.Type = "file"
	case info.IsDir():
		st.Type, st.Size = "dir", 0
	case info.Mode()&fs.ModeSymlink != 0:
		st.Type = "symlink"
		st.Target, _ = os.Readlink(p)
	default:
		st.Type = "other"
	}
	perm := uint32(info.Mode().Perm())
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		perm = sys.Mode & 0o7777
	}
	st.Mode = fmt.Sprintf("%04o", perm)
	return st
}

func (s *Server) fsRead(w http.ResponseWriter, r *http.Request) {
	p, a, err := s.fsCall(r)
	if err != nil {
		fsFail(w, err)
		return
	}
	s.touch()
	err = asUser(a, func() error {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.IsDir() {
			return invalidf("%s is a directory", p)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if info.Mode().IsRegular() {
			w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		}
		w.WriteHeader(http.StatusOK)
		io.Copy(w, f)
		return nil
	})
	if err != nil {
		fsFail(w, err)
	}
	s.touch()
}

// fsWrite writes beside the target and renames over it, so readers never
// see a partial file. Replacing a file keeps its owner.
func (s *Server) fsWrite(w http.ResponseWriter, r *http.Request) {
	p, a, err := s.fsCall(r)
	if err != nil {
		fsFail(w, err)
		return
	}
	q := r.URL.Query()
	perm, err := parseMode(q.Get("mode"), 0o644)
	if err != nil {
		fsFail(w, err)
		return
	}
	mkdir, err := parseBool(q.Get("mkdir"), false)
	if err != nil {
		fsFail(w, invalidf("mkdir must be true or false"))
		return
	}
	s.touch()
	var st api.Stat
	err = asUser(a, func() error {
		dir := filepath.Dir(p)
		if mkdir {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		tmp, err := os.CreateTemp(dir, ".loam-write-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		_, err = io.Copy(tmp, r.Body)
		if err == nil {
			err = tmp.Chmod(perm)
		}
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if old, err := os.Stat(p); err == nil {
			if old.IsDir() {
				return invalidf("%s is a directory", p)
			}
			if sys, ok := old.Sys().(*syscall.Stat_t); ok && a == nil {
				os.Chown(tmp.Name(), int(sys.Uid), int(sys.Gid))
			}
		}
		if err := os.Rename(tmp.Name(), p); err != nil {
			return err
		}
		st, err = statOf(p)
		return err
	})
	s.touch()
	if err != nil {
		fsFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) fsDelete(w http.ResponseWriter, r *http.Request) {
	p, a, err := s.fsCall(r)
	if err != nil {
		fsFail(w, err)
		return
	}
	recursive, err := parseBool(r.URL.Query().Get("recursive"), false)
	if err != nil {
		fsFail(w, invalidf("recursive must be true or false"))
		return
	}
	if p == "/" {
		fsFail(w, invalidf("refusing to delete /"))
		return
	}
	s.touch()
	err = asUser(a, func() error {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if recursive {
			return os.RemoveAll(p)
		}
		if info.IsDir() {
			// os.Remove would try unlink then rmdir; say plainly what is wrong.
			if err := syscall.Rmdir(p); err != nil {
				if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
					return fmt.Errorf("%s is a directory and not empty; use recursive=true: %w", p, syscall.ENOTEMPTY)
				}
				return &fs.PathError{Op: "rmdir", Path: p, Err: err}
			}
			return nil
		}
		return os.Remove(p)
	})
	s.touch()
	if err != nil {
		fsFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) fsStat(w http.ResponseWriter, r *http.Request) {
	p, a, err := s.fsCall(r)
	if err != nil {
		fsFail(w, err)
		return
	}
	var st api.Stat
	if err := asUser(a, func() (err error) { st, err = statOf(p); return }); err != nil {
		fsFail(w, err)
		return
	}
	s.touch()
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) fsList(w http.ResponseWriter, r *http.Request) {
	p, a, err := s.fsCall(r)
	if err != nil {
		fsFail(w, err)
		return
	}
	entries := []api.Stat{}
	err = asUser(a, func() error {
		des, err := os.ReadDir(p)
		if err != nil {
			return err
		}
		for _, de := range des {
			if info, err := de.Info(); err == nil {
				entries = append(entries, statFor(filepath.Join(p, de.Name()), info))
			}
		}
		return nil
	})
	if err != nil {
		fsFail(w, err)
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	s.touch()
	writeJSON(w, http.StatusOK, api.StatList{Entries: entries})
}

// fsMkdir creates a directory with mode (default 0755). With parents, missing
// parents are created too and an existing directory is not an error (mkdir
// -p); without, the parent must exist and the directory must not (409).
func (s *Server) fsMkdir(w http.ResponseWriter, r *http.Request) {
	var req api.Mkdir
	if !decodeBody(w, r, 1<<16, &req) {
		return
	}
	p, err := absPath(req.Path)
	if err != nil {
		fsFail(w, err)
		return
	}
	perm, err := parseMode(req.Mode, 0o755)
	if err != nil {
		fsFail(w, err)
		return
	}
	a, err := s.fsAccount(r.URL.Query().Get("user"))
	if err != nil {
		fsFail(w, invalidf("%v", err))
		return
	}
	var st api.Stat
	err = asUser(a, func() error {
		_, statErr := os.Stat(p)
		existed := statErr == nil
		var err error
		if req.Parents {
			err = os.MkdirAll(p, perm)
		} else {
			err = os.Mkdir(p, perm)
		}
		if err == nil && !existed {
			// Mkdir's mode is filtered by the umask; the caller asked for this one.
			err = os.Chmod(p, perm)
		}
		if err != nil {
			return err
		}
		st, err = statOf(p)
		return err
	})
	s.touch()
	if err != nil {
		fsFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) fsRename(w http.ResponseWriter, r *http.Request) {
	var req api.Rename
	if !decodeBody(w, r, 1<<16, &req) {
		return
	}
	from, err := absPath(req.From)
	if err != nil {
		fsFail(w, err)
		return
	}
	to, err := absPath(req.To)
	if err != nil {
		fsFail(w, err)
		return
	}
	a, err := s.fsAccount(r.URL.Query().Get("user"))
	if err != nil {
		fsFail(w, invalidf("%v", err))
		return
	}
	var st api.Stat
	err = asUser(a, func() error {
		if err := os.Rename(from, to); err != nil {
			return err
		}
		var err error
		st, err = statOf(to)
		return err
	})
	s.touch()
	if err != nil {
		fsFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// fsTarGet streams a tar of a directory, names relative to it. Sockets and
// devices are left out; symlinks are stored as symlinks.
func (s *Server) fsTarGet(w http.ResponseWriter, r *http.Request) {
	p, a, err := s.fsCall(r)
	if err != nil {
		fsFail(w, err)
		return
	}
	s.touch()
	started := false
	err = asUser(a, func() error {
		info, err := os.Stat(p)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return invalidf("%s is not a directory", p)
		}
		root, err := os.OpenRoot(p)
		if err != nil {
			return err
		}
		defer root.Close()
		w.Header().Set("Content-Type", "application/x-tar")
		w.WriteHeader(http.StatusOK)
		started = true
		tw := tar.NewWriter(&touchWriter{w: w, touch: s.touch})
		err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // vanished or unreadable while walking: leave it out
			}
			if name == "." {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			link := ""
			if info.Mode()&fs.ModeSymlink != 0 {
				if link, err = root.Readlink(name); err != nil {
					return nil
				}
			} else if !info.Mode().IsRegular() && !info.IsDir() {
				return nil
			}
			hdr, err := tar.FileInfoHeader(info, link)
			if err != nil {
				return nil
			}
			hdr.Name = name
			if info.IsDir() {
				hdr.Name += "/"
			}
			hdr.Uname, hdr.Gname = "", ""
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				f, err := root.Open(name)
				if err != nil {
					return err
				}
				// The header promised Size bytes; a file that shrank meanwhile is padded.
				n, err := io.Copy(tw, io.LimitReader(f, hdr.Size))
				f.Close()
				if err == nil && n < hdr.Size {
					_, err = io.CopyN(tw, zeros{}, hdr.Size-n)
				}
				if err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		return tw.Close()
	})
	if err != nil && !started {
		fsFail(w, err)
	}
	s.touch()
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

type touchWriter struct {
	w     io.Writer
	touch func()
}

func (t *touchWriter) Write(p []byte) (int, error) {
	t.touch()
	return t.w.Write(p)
}

// fsTarPut extracts a tar into a directory (created if missing). Extraction
// goes through os.Root, so no entry, however named and whatever symlinks it
// meets, lands outside the directory. As root, ownership comes from the
// archive; as a user, everything belongs to that user.
func (s *Server) fsTarPut(w http.ResponseWriter, r *http.Request) {
	p, a, err := s.fsCall(r)
	if err != nil {
		fsFail(w, err)
		return
	}
	s.touch()
	err = asUser(a, func() error {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return err
		}
		root, err := os.OpenRoot(p)
		if err != nil {
			return err
		}
		defer root.Close()
		tr := tar.NewReader(r.Body)
		type dirTime struct {
			name string
			t    time.Time
		}
		var dirs []dirTime
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return invalidf("reading the tar: %v", err)
			}
			s.touch()
			name := path.Clean(hdr.Name)
			if name == "." {
				continue
			}
			if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
				return invalidf("tar entry %q is outside the directory", hdr.Name)
			}
			if d := path.Dir(name); d != "." {
				if err := root.MkdirAll(d, 0o755); err != nil {
					return err
				}
			}
			mode := unixMode(uint32(hdr.Mode))
			switch hdr.Typeflag {
			case tar.TypeDir:
				if err := root.MkdirAll(name, mode.Perm()); err != nil {
					return err
				}
				dirs = append(dirs, dirTime{name, hdr.ModTime})
			case tar.TypeReg:
				root.Remove(name) // replace, don't write through, a symlink in the way
				f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_EXCL, mode.Perm())
				if err != nil {
					return err
				}
				_, err = io.Copy(f, tr)
				if cerr := f.Close(); err == nil {
					err = cerr
				}
				if err != nil {
					return err
				}
				root.Chtimes(name, hdr.ModTime, hdr.ModTime)
			case tar.TypeSymlink:
				root.Remove(name)
				if err := root.Symlink(hdr.Linkname, name); err != nil {
					return err
				}
			case tar.TypeLink:
				root.Remove(name)
				target := path.Clean(hdr.Linkname)
				if path.IsAbs(target) || target == ".." || strings.HasPrefix(target, "../") {
					return invalidf("tar hard link %q points outside the directory", hdr.Linkname)
				}
				if err := root.Link(target, name); err != nil {
					return err
				}
			default:
				continue // devices, fifos: not something a sandbox's API should create
			}
			if a == nil {
				root.Lchown(name, hdr.Uid, hdr.Gid)
			}
			// The mode after the owner: a chown clears setuid and setgid, so set
			// first they would be lost.
			if hdr.Typeflag == tar.TypeDir || hdr.Typeflag == tar.TypeReg {
				root.Chmod(name, mode)
			}
		}
		// Last, since writing into a directory moves its mtime.
		for _, d := range dirs {
			root.Chtimes(d.name, d.t, d.t)
		}
		return nil
	})
	s.touch()
	if err != nil {
		if isEscape(err) {
			err = invalidf("a tar entry goes outside the directory: %v", err)
		}
		fsFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// isEscape is os.Root refusing a path that leaves it (through "..", or a
// symlink pointing out). The error is unexported, so it is known by its text.
func isEscape(err error) bool {
	return err != nil && strings.Contains(err.Error(), "path escapes from parent")
}
