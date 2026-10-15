package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/arugula-salad/loam/internal/apiserver"
	"github.com/arugula-salad/loam/pkg/api"
)

// The adapter is a client of loam's API (docs/API.md) in the same process: it
// makes its calls on loam-api's handler, as the sandbox's project, so every
// rule of loam's (names, labels, deadlines, the network, the wake) is loam's
// to apply and an E2B sandbox is a loam sandbox like any other.

// call makes one call on loam's API as project. body, when not nil, is sent as
// JSON; out, when not nil, is decoded from a successful answer. A refusal is
// an *apiserver.Error.
func (f *Frontend) call(ctx context.Context, project, method, path string, body, out any) error {
	ctx, err := f.loam.AsProject(ctx, project)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	// The host is no domain of loam-api's, so the request is the API's, never
	// a port URL's.
	req, err := http.NewRequestWithContext(ctx, method, "http://loam-api"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := &recorder{header: http.Header{}}
	f.loam.ServeHTTP(rec, req)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	if rec.status >= 300 {
		var e api.Error
		if json.Unmarshal(rec.body.Bytes(), &e) != nil || e.Error.Code == "" {
			return &apiserver.Error{Status: rec.status, Code: "internal", Message: fmt.Sprintf("loam answered %d: %.200s", rec.status, rec.body.String())}
		}
		retry, _ := strconv.Atoi(rec.header.Get("Retry-After"))
		return &apiserver.Error{Status: rec.status, Code: e.Error.Code, Message: e.Error.Message, RetryAfter: retry}
	}
	if out != nil {
		return json.Unmarshal(rec.body.Bytes(), out)
	}
	return nil
}

// recorder is the answer to one in-process call.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}
func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

// codeOf is the docs/API.md code of a refusal of loam's, or "".
func codeOf(err error) string {
	var e *apiserver.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// sandboxOf is the loam sandbox id, as project sees it.
func (f *Frontend) sandboxOf(ctx context.Context, project, id string) (api.Sandbox, error) {
	var sb api.Sandbox
	err := f.call(ctx, project, http.MethodGet, "/v1/sandboxes/"+id, nil, &sb)
	return sb, err
}
