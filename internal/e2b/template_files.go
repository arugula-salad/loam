package e2b

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"

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
	file, err := os.CreateTemp("", "loam-template-upload-*")
	if err != nil {
		f.templateError(w, err)
		return
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err = io.Copy(file, http.MaxBytesReader(w, r.Body, maxBuildFile)); err != nil {
		writeErr(w, 413, "build file archive exceeds 64 MiB")
		return
	}
	if _, err = file.Seek(0, 0); err != nil {
		f.templateError(w, err)
		return
	}
	if err = walkArchive(file, func(archiveFile, io.Reader) error { return nil }); err != nil {
		writeErr(w, 400, "invalid build archive: "+err.Error())
		return
	}
	if _, err = file.Seek(0, 0); err != nil {
		f.templateError(w, err)
		return
	}
	err = f.store.PutE2BTemplateFile(r.Context(), project, t.ID, hash, file)
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

// walkArchive validates every member and streams its payload to visit. Archive
// names are never used on the API host filesystem.
func walkArchive(input io.Reader, visit func(archiveFile, io.Reader) error) error {
	buffered := bufio.NewReader(input)
	var reader io.Reader = buffered
	magic, _ := buffered.Peek(2)
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(buffered)
		if err != nil {
			return err
		}
		defer gz.Close()
		reader = gz
	}
	tr := tar.NewReader(reader)
	var total int64
	for count := 0; ; count++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(h.Name, "./")
		if name == "" && h.Typeflag == tar.TypeDir && h.Name == "./" {
			name = "."
		}
		if name == "" || path.IsAbs(name) || path.Clean(name) == ".." || strings.HasPrefix(path.Clean(name), "../") {
			return fmt.Errorf("archive path must be relative")
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			return fmt.Errorf("archive links and special files are not supported")
		}
		if h.Size < 0 || h.Size > (256<<20)-total || count >= 10000 {
			return fmt.Errorf("expanded archive exceeds limits")
		}
		total += h.Size
		if err = visit(archiveFile{Name: path.Clean(name), Mode: h.Mode & 0777, Directory: h.Typeflag == tar.TypeDir}, tr); err != nil {
			return err
		}
		if _, err = io.Copy(io.Discard, tr); err != nil {
			return err
		}
	}
	// Drain gzip to verify its checksum too, rather than stopping at tar EOF.
	n, err := io.Copy(io.Discard, io.LimitReader(reader, (1<<20)+1))
	if n > 1<<20 {
		return fmt.Errorf("excessive archive trailer")
	}
	return err
}

// archiveFiles is used by small archive conformance tests.
func archiveFiles(raw []byte) ([]archiveFile, error) {
	var out []archiveFile
	err := walkArchive(bytes.NewReader(raw), func(file archiveFile, r io.Reader) error {
		data, err := io.ReadAll(r)
		file.Data = data
		out = append(out, file)
		return err
	})
	return out, err
}

func (f *Frontend) copyBuildFiles(ctx context.Context, project, id, cwd string, step templateStep, template string) error {
	if len(step.Args) < 2 {
		return fmt.Errorf("COPY requires source and destination")
	}
	input, err := f.store.OpenE2BTemplateFile(ctx, project, template, step.FilesHash)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp("", "loam-template-copy-*")
	if err != nil {
		input.Close()
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	_, err = io.Copy(file, input)
	input.Close()
	if err != nil {
		return err
	}
	if _, err = file.Seek(0, 0); err != nil {
		return err
	}
	var entries []archiveFile
	if err = walkArchive(file, func(entry archiveFile, _ io.Reader) error { entries = append(entries, entry); return nil }); err != nil {
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
	if _, err = file.Seek(0, 0); err != nil {
		return err
	}
	return walkArchive(file, func(file archiveFile, payload io.Reader) error {
		suffix := path.Base(file.Name)
		if sourceDir {
			if source == "." {
				suffix = file.Name
				if suffix == "." {
					return nil
				}
			} else {
				if file.Name != source && !strings.HasPrefix(file.Name, source+"/") {
					return fmt.Errorf("COPY archive member is outside its source directory")
				}
				suffix = strings.TrimPrefix(strings.TrimPrefix(file.Name, source), "/")
			}
			if suffix == "" {
				return nil
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
			return nil
		}
		mode := fmt.Sprintf("%04o", file.Mode)
		if len(step.Args) > 3 && step.Args[3] != "" {
			mode = step.Args[3]
		}
		query := url.Values{"path": {target}, "mode": {mode}, "mkdir": {"true"}}
		if _, err = f.rawStreamCall(ctx, project, "PUT", "/v1/sandboxes/"+id+"/fs/file?"+query.Encode(), payload); err != nil {
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
		return nil
	})
}
