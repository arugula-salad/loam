package client

import (
	"context"
	"net/http"
	"net/url"
	"sort"

	"github.com/arugula-salad/loam/pkg/api"
)

// Projects and keys. These need the admin token as the client's Token.

func (c *Client) CreateProject(ctx context.Context, name string) (*api.Project, error) {
	var p api.Project
	return &p, c.do(ctx, http.MethodPost, "/v1/projects", nil, map[string]string{"name": name}, &p)
}

func (c *Client) ListProjects(ctx context.Context) ([]api.Project, error) {
	var l struct {
		Projects []api.Project `json:"projects"`
	}
	return l.Projects, c.do(ctx, http.MethodGet, "/v1/projects", nil, nil, &l)
}

func (c *Client) DeleteProject(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/projects/"+url.PathEscape(name), nil, nil, nil)
}

// CreateKey makes a key in project; its Secret is set only in this answer.
func (c *Client) CreateKey(ctx context.Context, project, name string) (*api.Key, error) {
	var k api.Key
	return &k, c.do(ctx, http.MethodPost, "/v1/projects/"+url.PathEscape(project)+"/keys", nil, map[string]string{"name": name}, &k)
}

func (c *Client) ListKeys(ctx context.Context, project string) ([]api.Key, error) {
	var l struct {
		Keys []api.Key `json:"keys"`
	}
	return l.Keys, c.do(ctx, http.MethodGet, "/v1/projects/"+url.PathEscape(project)+"/keys", nil, nil, &l)
}

func (c *Client) DeleteKey(ctx context.Context, project, key string) error {
	return c.do(ctx, http.MethodDelete, "/v1/projects/"+url.PathEscape(project)+"/keys/"+url.PathEscape(key), nil, nil, nil)
}

// Whoami names the key's project and the key itself.
func (c *Client) Whoami(ctx context.Context) (*api.Whoami, error) {
	var w api.Whoami
	return &w, c.do(ctx, http.MethodGet, "/v1/whoami", nil, nil, &w)
}

// Sandboxes.

// CreateSandbox answers once the sandbox is running: create waits for the boot.
func (c *Client) CreateSandbox(ctx context.Context, req api.CreateSandbox) (*api.Sandbox, error) {
	var s api.Sandbox
	return &s, c.do(ctx, http.MethodPost, "/v1/sandboxes", nil, req, &s)
}

// Fork starts a new sandbox from a checkpoint of another; name may be empty
// for a generated one.
func (c *Client) Fork(ctx context.Context, sandbox, checkpoint, name string) (*api.Sandbox, error) {
	return c.CreateSandbox(ctx, api.CreateSandbox{Name: name, From: &api.From{Sandbox: sandbox, Checkpoint: checkpoint}})
}

// ListOptions filter ListSandboxes; every label must match.
type ListOptions struct {
	Labels map[string]string
	State  string
}

func (c *Client) ListSandboxes(ctx context.Context, o ListOptions) ([]api.Sandbox, error) {
	q := url.Values{}
	keys := make([]string, 0, len(o.Labels))
	for k := range o.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		q.Add("label", k+"="+o.Labels[k])
	}
	if o.State != "" {
		q.Set("state", o.State)
	}
	var l api.SandboxList
	return l.Sandboxes, c.do(ctx, http.MethodGet, "/v1/sandboxes", q, nil, &l)
}

func (c *Client) GetSandbox(ctx context.Context, sandbox string) (*api.Sandbox, error) {
	var s api.Sandbox
	return &s, c.do(ctx, http.MethodGet, sbPath(sandbox), nil, nil, &s)
}

// UpdateSandbox patches the record; a new env reaches processes started after
// it and every later wake.
func (c *Client) UpdateSandbox(ctx context.Context, sandbox string, req api.UpdateSandbox) (*api.Sandbox, error) {
	var s api.Sandbox
	return &s, c.do(ctx, http.MethodPatch, sbPath(sandbox), nil, req, &s)
}

// DeleteSandbox deletes the sandbox and its checkpoints; a protected one is a conflict.
func (c *Client) DeleteSandbox(ctx context.Context, sandbox string) error {
	return c.do(ctx, http.MethodDelete, sbPath(sandbox), nil, nil, nil)
}

func (c *Client) Pause(ctx context.Context, sandbox string) (*api.Sandbox, error) {
	return c.lifecycle(ctx, sandbox, "pause")
}

func (c *Client) Stop(ctx context.Context, sandbox string) (*api.Sandbox, error) {
	return c.lifecycle(ctx, sandbox, "stop")
}

func (c *Client) Wake(ctx context.Context, sandbox string) (*api.Sandbox, error) {
	return c.lifecycle(ctx, sandbox, "wake")
}

func (c *Client) lifecycle(ctx context.Context, sandbox, verb string) (*api.Sandbox, error) {
	var s api.Sandbox
	return &s, c.do(ctx, http.MethodPost, sbPath(sandbox, verb), nil, nil, &s)
}

func (c *Client) ListTemplates(ctx context.Context) ([]api.Template, error) {
	var l api.TemplateList
	return l.Templates, c.do(ctx, http.MethodGet, "/v1/templates", nil, nil, &l)
}

// Checkpoints.

func (c *Client) CreateCheckpoint(ctx context.Context, sandbox string, req api.CreateCheckpoint) (*api.Checkpoint, error) {
	var cp api.Checkpoint
	return &cp, c.do(ctx, http.MethodPost, sbPath(sandbox, "checkpoints"), nil, req, &cp)
}

// ListCheckpoints answers oldest first.
func (c *Client) ListCheckpoints(ctx context.Context, sandbox string) ([]api.Checkpoint, error) {
	var l api.CheckpointList
	return l.Checkpoints, c.do(ctx, http.MethodGet, sbPath(sandbox, "checkpoints"), nil, nil, &l)
}

func (c *Client) GetCheckpoint(ctx context.Context, sandbox, checkpoint string) (*api.Checkpoint, error) {
	var cp api.Checkpoint
	return &cp, c.do(ctx, http.MethodGet, sbPath(sandbox, "checkpoints", url.PathEscape(checkpoint)), nil, nil, &cp)
}

func (c *Client) DeleteCheckpoint(ctx context.Context, sandbox, checkpoint string) error {
	return c.do(ctx, http.MethodDelete, sbPath(sandbox, "checkpoints", url.PathEscape(checkpoint)), nil, nil, nil)
}

// RestoreCheckpoint keeps the current state as an automatic checkpoint first,
// and ends every exec session and port stream on the sandbox.
func (c *Client) RestoreCheckpoint(ctx context.Context, sandbox, checkpoint string) (*api.Sandbox, error) {
	var s api.Sandbox
	return &s, c.do(ctx, http.MethodPost, sbPath(sandbox, "checkpoints", url.PathEscape(checkpoint), "restore"), nil, nil, &s)
}
