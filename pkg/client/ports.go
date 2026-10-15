package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arugula-salad/loam/pkg/api"
)

// Ports lists what listens in the sandbox, on every address.
func (c *Client) Ports(ctx context.Context, sandbox string) ([]api.Port, error) {
	var l api.PortList
	return l.Ports, c.do(ctx, http.MethodGet, sbPath(sandbox, "ports"), nil, nil, &l)
}

// PortEvent is a listener opening or closing.
type PortEvent struct {
	Type string // "opened" or "closed"
	Port api.Port
}

// PortWatch is the ports watch, an SSE stream. Watching is not activity, so
// an open watch doesn't keep the sandbox awake.
type PortWatch struct{ sse *sseReader }

func (c *Client) WatchPorts(ctx context.Context, sandbox string) (*PortWatch, error) {
	resp, err := c.request(ctx, http.MethodGet, sbPath(sandbox, "ports", "watch"), nil, nil, "")
	if err != nil {
		return nil, err
	}
	return &PortWatch{sse: newSSE(resp.Body)}, nil
}

// Next blocks for the next listener change; io.EOF when the stream ends.
func (w *PortWatch) Next() (PortEvent, error) {
	for {
		ev, data, err := w.sse.next()
		if err != nil {
			return PortEvent{}, err
		}
		if ev != "opened" && ev != "closed" {
			continue
		}
		var p api.Port
		if err := json.Unmarshal([]byte(data), &p); err != nil {
			return PortEvent{}, fmt.Errorf("ports watch: bad data %q: %w", data, err)
		}
		return PortEvent{Type: ev, Port: p}, nil
	}
}

func (w *PortWatch) Close() error { return w.sse.Close() }

// ConnectPort opens a TCP stream to 127.0.0.1:port in the sandbox, carried
// over a WebSocket. The connection holds the sandbox awake while it is open.
func (c *Client) ConnectPort(ctx context.Context, sandbox string, port int) (net.Conn, error) {
	ws, err := c.dial(ctx, sbPath(sandbox, "ports", strconv.Itoa(port), "connect"), nil)
	if err != nil {
		return nil, err
	}
	return &wsConn{ws: ws}, nil
}

// SetPortAccess makes a port's URL "private" (the project's key needed) or "public".
func (c *Client) SetPortAccess(ctx context.Context, sandbox string, port int, access string) (*api.PortSetting, error) {
	var s api.PortSetting
	return &s, c.do(ctx, http.MethodPut, sbPath(sandbox, "ports", strconv.Itoa(port)), nil, map[string]string{"access": access}, &s)
}

// PortSettings lists the ports whose access has been set.
func (c *Client) PortSettings(ctx context.Context, sandbox string) ([]api.PortSetting, error) {
	var l api.PortSettingList
	return l.Ports, c.do(ctx, http.MethodGet, sbPath(sandbox, "ports", "settings"), nil, nil, &l)
}

// Stats is a sandbox's traffic at its URLs over the last days days, today
// last (docs/API.md, "Stats"); 0 is the server's default, 7.
func (c *Client) Stats(ctx context.Context, sandbox string, days int) (*api.Stats, error) {
	q := url.Values{}
	if days > 0 {
		q.Set("days", strconv.Itoa(days))
	}
	var st api.Stats
	return &st, c.do(ctx, http.MethodGet, sbPath(sandbox, "stats"), q, nil, &st)
}

// wsConn is a net.Conn over a WebSocket whose binary frames carry the bytes.
// The protocol has no half-close, so Close ends both directions.
type wsConn struct {
	ws  *websocket.Conn
	rmu sync.Mutex
	r   io.Reader
	wmu sync.Mutex
}

func (c *wsConn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for {
		if c.r == nil {
			mt, r, err := c.ws.NextReader()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					return 0, io.EOF
				}
				return 0, err
			}
			if mt != websocket.BinaryMessage {
				continue
			}
			c.r = r
		}
		n, err := c.r.Read(p)
		if errors.Is(err, io.EOF) {
			c.r = nil
			if n == 0 {
				continue
			}
			err = nil
		}
		return n, err
	}
}

func (c *wsConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsConn) Close() error {
	c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	return c.ws.Close()
}

func (c *wsConn) LocalAddr() net.Addr  { return c.ws.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr { return c.ws.RemoteAddr() }

func (c *wsConn) SetDeadline(t time.Time) error {
	return errors.Join(c.ws.SetReadDeadline(t), c.ws.SetWriteDeadline(t))
}
func (c *wsConn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *wsConn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }
