package e2b

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/pkg/api"
)

var fileHashRE = regexp.MustCompile(`^[a-f0-9]{64}$`)

const maxBuildFile = 64 << 20

func (f *Frontend) buildFile(w http.ResponseWriter, r *http.Request, project string) {
	t, err := f.findTemplate(r.Context(), project, r.PathValue("template"))
	if err != nil {
		f.templateError(w, err)
		return
	}
	hash := r.PathValue("hash")
	if !fileHashRE.MatchString(hash) {
		writeErr(w, 400, "invalid file hash")
		return
	}
	if r.Method == "GET" {
		if _, ok := t.Files[hash]; ok {
			writeJSON(w, 200, map[string]any{"present": true})
			return
		}
		scheme := "https"
		if r.TLS == nil && (strings.HasPrefix(r.Host, "localhost") || strings.Contains(r.Host, ".localhost")) {
			scheme = "http"
		}
		// Use the configured API host rather than reflecting an arbitrary Host header.
		host := "api." + f.opts.Domain
		if scheme == "http" {
			host = f.opts.Domain
		}
		writeJSON(w, 200, map[string]any{"present": false, "url": scheme + "://" + host + "/templates/" + t.ID + "/files/" + hash, "headers": map[string]string{"X-API-Key": r.Header.Get("X-API-Key")}})
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBuildFile))
	if err != nil {
		writeErr(w, 413, "build file archive exceeds 64 MiB")
		return
	}
	if _, err = archiveFiles(raw); err != nil {
		writeErr(w, 400, "invalid build archive: "+err.Error())
		return
	}
	_, err = f.store.UpdateE2BTemplate(r.Context(), project, t.ID, func(t *db.E2BTemplate) error {
		total := len(raw)
		for key, b := range t.Files {
			if key != hash {
				total += len(b)
			}
		}
		if total > 256<<20 {
			return fmt.Errorf("template uploads exceed 256 MiB")
		}
		if t.Files == nil {
			t.Files = map[string][]byte{}
		}
		t.Files[hash] = raw
		return nil
	})
	if err != nil {
		f.templateError(w, err)
		return
	}
	w.WriteHeader(200)
}

type archiveFile struct {
	Name      string
	Data      []byte
	Mode      int64
	Directory bool
}

// Unpack in memory, never onto the API host's filesystem. Each member is
// subsequently written through the sandbox's own file API.
func archiveFiles(raw []byte) ([]archiveFile, error) {
	var reader io.Reader = bytes.NewReader(raw)
	if len(raw) > 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		gz, err := gzip.NewReader(reader)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		reader = gz
	}
	tr := tar.NewReader(reader)
	out := []archiveFile{}
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimPrefix(h.Name, "./")
		if name == "" && h.Typeflag == tar.TypeDir && h.Name == "./" {
			name = "."
		}
		if name == "" || path.IsAbs(name) || name == ".." || strings.HasPrefix(path.Clean(name), "../") {
			return nil, fmt.Errorf("archive path must be relative")
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			return nil, fmt.Errorf("archive links and special files are not supported")
		}
		total += h.Size
		if h.Size < 0 || total > 256<<20 || len(out) > 10000 {
			return nil, fmt.Errorf("expanded archive exceeds limits")
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out = append(out, archiveFile{Name: path.Clean(name), Data: data, Mode: h.Mode & 0777, Directory: h.Typeflag == tar.TypeDir})
	}
	return out, nil
}

func (f *Frontend) copyBuildFiles(ctx context.Context, project, id, cwd string, step templateStep, files map[string][]byte) error {
	if len(step.Args) < 2 {
		return fmt.Errorf("COPY requires source and destination")
	}
	raw, ok := files[step.FilesHash]
	if !ok {
		return fmt.Errorf("COPY upload not found")
	}
	entries, err := archiveFiles(raw)
	if err != nil {
		return err
	}
	source, dest := path.Clean(step.Args[0]), step.Args[1]
	if strings.ContainsAny(source, "*?[") {
		return fmt.Errorf("COPY glob sources are not supported; use a directory or exact file")
	}
	if !path.IsAbs(dest) {
		dest = path.Join(cwd, dest)
	}
	sourceDir := false
	for _, file := range entries {
		if file.Name == source && file.Directory {
			sourceDir = true
		}
	}
	destDir := strings.HasSuffix(step.Args[1], "/")
	if !destDir {
		var out api.ExecResult
		if err := f.call(ctx, project, "POST", "/v1/sandboxes/"+id+"/exec/run", api.ExecRun{Cmd: []string{"/bin/sh", "-c", "test -d " + shellQuote(dest)}, User: "root"}, &out); err != nil {
			return err
		}
		destDir = out.ExitCode == 0
	}
	for _, file := range entries {
		suffix := path.Base(file.Name)
		if sourceDir {
			if source == "." {
				suffix = file.Name
				if suffix == "." {
					continue
				}
			} else {
				if file.Name != source && !strings.HasPrefix(file.Name, source+"/") {
					return fmt.Errorf("COPY archive member is outside its source directory")
				}
				suffix = strings.TrimPrefix(strings.TrimPrefix(file.Name, source), "/")
			}
			if suffix == "" {
				continue
			}
		}
		target := dest
		if sourceDir || destDir || len(entries) > 1 {
			target = path.Join(dest, suffix)
		}
		if file.Directory {
			var out api.ExecResult
			if err = f.call(ctx, project, "POST", "/v1/sandboxes/"+id+"/exec/run", api.ExecRun{Cmd: []string{"mkdir", "-p", target}, User: "root"}, &out); err != nil {
				return err
			}
			if out.ExitCode != 0 {
				return fmt.Errorf("COPY cannot create directory")
			}
			continue
		}
		mode := fmt.Sprintf("%04o", file.Mode)
		if len(step.Args) > 3 && step.Args[3] != "" {
			mode = step.Args[3]
		}
		query := url.Values{"path": {target}, "mode": {mode}, "mkdir": {"true"}}
		if _, err = f.rawCall(ctx, project, "PUT", "/v1/sandboxes/"+id+"/fs/file?"+query.Encode(), file.Data); err != nil {
			return err
		}
		if len(step.Args) > 2 && step.Args[2] != "" {
			var out api.ExecResult
			if err = f.call(ctx, project, "POST", "/v1/sandboxes/"+id+"/exec/run", api.ExecRun{Cmd: []string{"chown", step.Args[2], target}, User: "root"}, &out); err != nil {
				return err
			}
			if out.ExitCode != 0 {
				return fmt.Errorf("COPY cannot set owner")
			}
		}
	}
	return nil
}
