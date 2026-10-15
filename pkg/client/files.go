package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/gorilla/websocket"

	"github.com/arugula-salad/loam/pkg/api"
)

// FSOption adjusts a file operation. Not every option means something to
// every route; the server refuses what it can't honour.
type FSOption func(url.Values)

// AsUser runs the operation as user, whose permissions then apply. Without
// it, file operations run as root.
func AsUser(user string) FSOption { return func(q url.Values) { q.Set("user", user) } }

// Mode is the mode of a file WriteFile creates.
func Mode(m os.FileMode) FSOption {
	return func(q url.Values) { q.Set("mode", fmt.Sprintf("%04o", m.Perm())) }
}

// MakeParents makes WriteFile create missing parent directories.
func MakeParents() FSOption { return func(q url.Values) { q.Set("mkdir", "true") } }

// Recursive makes Remove delete a directory tree, and WatchFiles watch one.
func Recursive() FSOption { return func(q url.Values) { q.Set("recursive", "true") } }

func fsQuery(path string, opts []FSOption) url.Values {
	q := url.Values{}
	if path != "" {
		q.Set("path", path)
	}
	for _, o := range opts {
		o(q)
	}
	return q
}

// OpenFile streams a file's bytes; the caller closes the reader.
func (c *Client) OpenFile(ctx context.Context, sandbox, path string, opts ...FSOption) (io.ReadCloser, error) {
	resp, err := c.request(ctx, http.MethodGet, sbPath(sandbox, "fs", "file"), fsQuery(path, opts), nil, "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// ReadFile reads a whole file.
func (c *Client) ReadFile(ctx context.Context, sandbox, path string, opts ...FSOption) ([]byte, error) {
	r, err := c.OpenFile(ctx, sandbox, path, opts...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// WriteFile writes body to path, replacing what was there.
func (c *Client) WriteFile(ctx context.Context, sandbox, path string, body io.Reader, opts ...FSOption) (*api.Stat, error) {
	resp, err := c.request(ctx, http.MethodPut, sbPath(sandbox, "fs", "file"), fsQuery(path, opts), body, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var st api.Stat
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, fmt.Errorf("write %s: decoding the answer: %w", path, err)
	}
	return &st, nil
}

// Remove deletes a file, or with Recursive a directory tree.
func (c *Client) Remove(ctx context.Context, sandbox, path string, opts ...FSOption) error {
	return c.do(ctx, http.MethodDelete, sbPath(sandbox, "fs", "file"), fsQuery(path, opts), nil, nil)
}

func (c *Client) Stat(ctx context.Context, sandbox, path string, opts ...FSOption) (*api.Stat, error) {
	var st api.Stat
	return &st, c.do(ctx, http.MethodGet, sbPath(sandbox, "fs", "stat"), fsQuery(path, opts), nil, &st)
}

// List lists a directory.
func (c *Client) List(ctx context.Context, sandbox, path string, opts ...FSOption) ([]api.Stat, error) {
	var l api.StatList
	return l.Entries, c.do(ctx, http.MethodGet, sbPath(sandbox, "fs", "list"), fsQuery(path, opts), nil, &l)
}

func (c *Client) Mkdir(ctx context.Context, sandbox string, req api.Mkdir, opts ...FSOption) (*api.Stat, error) {
	var st api.Stat
	return &st, c.do(ctx, http.MethodPost, sbPath(sandbox, "fs", "mkdir"), fsQuery("", opts), req, &st)
}

func (c *Client) Rename(ctx context.Context, sandbox, from, to string, opts ...FSOption) (*api.Stat, error) {
	var st api.Stat
	return &st, c.do(ctx, http.MethodPost, sbPath(sandbox, "fs", "rename"), fsQuery("", opts), api.Rename{From: from, To: to}, &st)
}

// GetTar streams a tar of the directory at path, its entries relative to it.
func (c *Client) GetTar(ctx context.Context, sandbox, path string, opts ...FSOption) (io.ReadCloser, error) {
	resp, err := c.request(ctx, http.MethodGet, sbPath(sandbox, "fs", "tar"), fsQuery(path, opts), nil, "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// PutTar extracts the tar body into the directory at path.
func (c *Client) PutTar(ctx context.Context, sandbox, path string, body io.Reader, opts ...FSOption) error {
	resp, err := c.request(ctx, http.MethodPut, sbPath(sandbox, "fs", "tar"), fsQuery(path, opts), body, "application/x-tar")
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// FileWatch is a stream of changes under a path.
type FileWatch struct {
	ws   *websocket.Conn
	stop func() bool
}

// WatchFiles watches path (with Recursive, everything under it).
func (c *Client) WatchFiles(ctx context.Context, sandbox, path string, opts ...FSOption) (*FileWatch, error) {
	ws, err := c.dial(ctx, sbPath(sandbox, "fs", "watch"), fsQuery(path, opts))
	if err != nil {
		return nil, err
	}
	return &FileWatch{ws: ws, stop: context.AfterFunc(ctx, func() { ws.Close() })}, nil
}

// Next blocks for the next change. io.EOF means the server ended the watch.
func (w *FileWatch) Next() (api.WatchEvent, error) {
	for {
		mt, data, err := w.ws.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				return api.WatchEvent{}, io.EOF
			}
			return api.WatchEvent{}, err
		}
		if mt != websocket.TextMessage {
			continue
		}
		var ev api.WatchEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return api.WatchEvent{}, fmt.Errorf("watch: bad frame %q: %w", data, err)
		}
		return ev, nil
	}
}

func (w *FileWatch) Close() error {
	w.stop()
	return w.ws.Close()
}
