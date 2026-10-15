package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/arugula-salad/loam/pkg/api"
)

func (c *Client) ListServices(ctx context.Context, sandbox string) ([]api.Service, error) {
	var l api.ServiceList
	return l.Services, c.do(ctx, http.MethodGet, sbPath(sandbox, "services"), nil, nil, &l)
}

// PutService creates or replaces a service definition.
func (c *Client) PutService(ctx context.Context, sandbox, name string, spec api.ServiceSpec) (*api.Service, error) {
	var s api.Service
	return &s, c.do(ctx, http.MethodPut, sbPath(sandbox, "services", url.PathEscape(name)), nil, spec, &s)
}

func (c *Client) GetService(ctx context.Context, sandbox, name string) (*api.Service, error) {
	var s api.Service
	return &s, c.do(ctx, http.MethodGet, sbPath(sandbox, "services", url.PathEscape(name)), nil, nil, &s)
}

// DeleteService stops a service and removes its definition.
func (c *Client) DeleteService(ctx context.Context, sandbox, name string) error {
	return c.do(ctx, http.MethodDelete, sbPath(sandbox, "services", url.PathEscape(name)), nil, nil, nil)
}

func (c *Client) StartService(ctx context.Context, sandbox, name string) (*api.Service, error) {
	return c.serviceAction(ctx, sandbox, name, "start")
}

func (c *Client) StopService(ctx context.Context, sandbox, name string) (*api.Service, error) {
	return c.serviceAction(ctx, sandbox, name, "stop")
}

func (c *Client) RestartService(ctx context.Context, sandbox, name string) (*api.Service, error) {
	return c.serviceAction(ctx, sandbox, name, "restart")
}

func (c *Client) serviceAction(ctx context.Context, sandbox, name, verb string) (*api.Service, error) {
	var s api.Service
	return &s, c.do(ctx, http.MethodPost, sbPath(sandbox, "services", url.PathEscape(name), verb), nil, nil, &s)
}

// LogOptions select a service's logs. Tail 0 means all that is kept.
type LogOptions struct {
	Follow bool
	Tail   int
}

// ServiceLogs streams a service's stdout and stderr, interleaved. With
// Follow the stream stays open for new output until ctx ends.
func (c *Client) ServiceLogs(ctx context.Context, sandbox, name string, o LogOptions) (io.ReadCloser, error) {
	q := url.Values{}
	if o.Follow {
		q.Set("follow", "true")
	}
	if o.Tail > 0 {
		q.Set("tail", strconv.Itoa(o.Tail))
	}
	resp, err := c.request(ctx, http.MethodGet, sbPath(sandbox, "services", url.PathEscape(name), "logs"), q, nil, "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// EventStream is the project's event stream. Events are best effort: after
// a reconnect, read the state from the API.
type EventStream struct{ sse *sseReader }

// Events streams the project's events; sandbox, when set, narrows it to one.
func (c *Client) Events(ctx context.Context, sandbox string) (*EventStream, error) {
	q := url.Values{}
	if sandbox != "" {
		q.Set("sandbox", sandbox)
	}
	resp, err := c.request(ctx, http.MethodGet, "/v1/events", q, nil, "")
	if err != nil {
		return nil, err
	}
	return &EventStream{sse: newSSE(resp.Body)}, nil
}

// Next blocks for the next event; io.EOF when the stream ends.
func (s *EventStream) Next() (api.Event, error) {
	name, data, err := s.sse.next()
	if err != nil {
		return api.Event{}, err
	}
	var ev api.Event
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return api.Event{}, fmt.Errorf("events: bad data %q: %w", data, err)
	}
	if ev.Type == "" {
		ev.Type = name
	}
	return ev, nil
}

func (s *EventStream) Close() error { return s.sse.Close() }
