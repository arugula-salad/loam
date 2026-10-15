package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/arugula-salad/loam/pkg/api"
)

// Network and secrets (docs/API.md, "Network" and "Secrets"). A deployment
// without a secret key answers the secret routes with ErrorCode "unsupported".

// NetworkDefaults is the rule list {"include": "defaults"} stands for.
func (c *Client) NetworkDefaults(ctx context.Context) ([]api.Rule, error) {
	var n api.Network
	return n.Egress, c.do(ctx, http.MethodGet, "/v1/network/defaults", nil, nil, &n)
}

// PutSecret creates or replaces a project secret. A replaced value reaches
// the awake sandboxes that use it before this returns. The answer never holds
// the value, and nothing can read it back.
func (c *Client) PutSecret(ctx context.Context, name string, req api.PutSecret) (*api.Secret, error) {
	var s api.Secret
	return &s, c.do(ctx, http.MethodPut, "/v1/secrets/"+url.PathEscape(name), nil, req, &s)
}

func (c *Client) GetSecret(ctx context.Context, name string) (*api.Secret, error) {
	var s api.Secret
	return &s, c.do(ctx, http.MethodGet, "/v1/secrets/"+url.PathEscape(name), nil, nil, &s)
}

func (c *Client) ListSecrets(ctx context.Context) ([]api.Secret, error) {
	var l api.SecretList
	return l.Secrets, c.do(ctx, http.MethodGet, "/v1/secrets", nil, nil, &l)
}

// DeleteSecret is a conflict while a sandbox names the secret.
func (c *Client) DeleteSecret(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/secrets/"+url.PathEscape(name), nil, nil, nil)
}
