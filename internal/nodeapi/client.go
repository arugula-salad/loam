package nodeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/arugula-salad/loam/pkg/api"
)

// Client talks to one nodelet. Addr is resolved by the caller from the node's
// name each time it builds one (docs/NODE.md, "Reaching a nodelet"): a Client
// is cheap and short-lived.
type Client struct {
	Addr  string // host:port
	Token string
	HTTP  *http.Client
}

// Err is an error answer from a nodelet.
type Err struct {
	Status int
	api.ErrorDetail
}

func (e *Err) Error() string { return fmt.Sprintf("nodelet: %d %s: %s", e.Status, e.Code, e.Message) }

// IsCode reports whether err is a nodelet answer with code.
func IsCode(err error, code string) bool {
	e, ok := err.(*Err)
	return ok && e.Code == code
}

func readError(resp *http.Response) error {
	var body api.Error
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(raw, &body) != nil || body.Error.Code == "" {
		body.Error = api.ErrorDetail{Code: "nodelet_error", Message: string(raw)}
	}
	return &Err{Status: resp.StatusCode, ErrorDetail: body.Error}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+c.Addr+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return readError(resp)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) Node(ctx context.Context) (Node, error) {
	var n Node
	return n, c.do(ctx, http.MethodGet, "/v1/node", nil, &n)
}

func (c *Client) VMs(ctx context.Context) ([]VM, error) {
	var l VMList
	return l.VMs, c.do(ctx, http.MethodGet, "/v1/vms", nil, &l)
}

func (c *Client) VM(ctx context.Context, id string) (VM, error) {
	var v VM
	return v, c.do(ctx, http.MethodGet, "/v1/vms/"+id, nil, &v)
}

func (c *Client) Create(ctx context.Context, req CreateVM) (VM, error) {
	var v VM
	return v, c.do(ctx, http.MethodPost, "/v1/vms", req, &v)
}

func (c *Client) Resume(ctx context.Context, id string, req Resume) (VM, error) {
	var v VM
	return v, c.do(ctx, http.MethodPost, "/v1/vms/"+id+"/resume", req, &v)
}

// SetPolicy replaces a running VM's policy (docs/NODE.md, "Phase 3").
func (c *Client) SetPolicy(ctx context.Context, id string, p Policy) error {
	return c.do(ctx, http.MethodPut, "/v1/vms/"+id+"/policy", p, nil)
}

// Pause and Stop are phase 1's calls, without a body. loam-api makes
// PauseInto and StopInto's.
func (c *Client) Pause(ctx context.Context, id string) (VM, error) {
	var v VM
	return v, c.do(ctx, http.MethodPost, "/v1/vms/"+id+"/pause", nil, &v)
}

func (c *Client) Stop(ctx context.Context, id string) (VM, error) {
	var v VM
	return v, c.do(ctx, http.MethodPost, "/v1/vms/"+id+"/stop", nil, &v)
}

// PauseInto pauses VM id into snapshot (docs/NODE.md, "Phase 2"). The
// answer's Snapshot names the snapshot the VM is paused on, which is not
// snapshot when the VM was paused already.
func (c *Client) PauseInto(ctx context.Context, id, snapshot string) (VM, error) {
	var v VM
	return v, c.do(ctx, http.MethodPost, "/v1/vms/"+id+"/pause", PauseVM{Snapshot: snapshot}, &v)
}

// StopInto stops VM id, keeping its disk as snapshot.
func (c *Client) StopInto(ctx context.Context, id, snapshot string) (VM, error) {
	var v VM
	return v, c.do(ctx, http.MethodPost, "/v1/vms/"+id+"/stop", StopVM{Snapshot: snapshot}, &v)
}

func (c *Client) Delete(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/vms/"+id, nil, nil)
}

func (c *Client) Snapshot(ctx context.Context, vm string, req CreateSnapshot) (Snapshot, error) {
	var s Snapshot
	return s, c.do(ctx, http.MethodPost, "/v1/vms/"+vm+"/snapshots", req, &s)
}

func (c *Client) Restore(ctx context.Context, vm string, req Restore) (VM, error) {
	var v VM
	return v, c.do(ctx, http.MethodPost, "/v1/vms/"+vm+"/restore", req, &v)
}

func (c *Client) Snapshots(ctx context.Context) ([]Snapshot, error) {
	var l SnapshotList
	return l.Snapshots, c.do(ctx, http.MethodGet, "/v1/snapshots", nil, &l)
}

func (c *Client) GetSnapshot(ctx context.Context, id string) (Snapshot, error) {
	var s Snapshot
	return s, c.do(ctx, http.MethodGet, "/v1/snapshots/"+id, nil, &s)
}

// DeleteLayer deletes a layer object from the store; any nodelet will do.
func (c *Client) DeleteLayer(ctx context.Context, layer string) error {
	return c.do(ctx, http.MethodDelete, "/v1/layers/"+url.PathEscape(layer), nil, nil)
}

func (c *Client) DeleteSnapshot(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/snapshots/"+id, nil, nil)
}

func (c *Client) Activity(ctx context.Context, id string) (Activity, error) {
	var a Activity
	return a, c.do(ctx, http.MethodGet, "/v1/vms/"+id+"/activity", nil, &a)
}

// DialAgent opens the agent stream of VM id (docs/NODE.md, "The agent stream").
func (c *Client) DialAgent(ctx context.Context, id string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", c.Addr)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	s, err := DialUpgrade(conn, id, c.Token)
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return s, nil
}

// Addr builds host:port for a nodelet address without a port.
func Addr(host string) string { return net.JoinHostPort(host, strconv.Itoa(Port)) }
