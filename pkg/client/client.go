// Package client is a Go client for loam's public API (docs/API.md). Every
// route has a method; the streams (exec, attach, file and port watches, port
// connections, events) have types of their own. The CLI (cmd/loam) and the e2e
// suite are built on it, so what they check is what any Go caller gets.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arugula-salad/loam/pkg/api"
)

// Client talks to one loam-api as one key. It is safe for concurrent use.
type Client struct {
	// URL is the API's base URL, without /v1.
	URL string
	// Token is the API key (or the admin token, for the admin routes).
	Token string
	// HTTP carries every plain request. It must not have a Timeout: logs,
	// tars and event streams are long-lived; use contexts instead.
	HTTP *http.Client
	// Dialer opens the WebSockets (exec, attach, file watch, port connect).
	Dialer *websocket.Dialer
}

// New returns a client for the API at baseURL with the key token.
func New(baseURL, token string) *Client {
	return &Client{
		URL:    strings.TrimRight(baseURL, "/"),
		Token:  token,
		HTTP:   &http.Client{},
		Dialer: &websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: 45 * time.Second},
	}
}

// FromEnv returns a client configured by LOAM_API_URL and LOAM_TOKEN. Either
// missing is an error: there is no default loam to fall back on, and a client
// without a key would answer 401 to everything.
func FromEnv() (*Client, error) {
	u := os.Getenv("LOAM_API_URL")
	if u == "" {
		return nil, errors.New("LOAM_API_URL is not set (the API's base URL, such as https://api.loam.example.com)")
	}
	tok := os.Getenv("LOAM_TOKEN")
	if tok == "" {
		return nil, errors.New("LOAM_TOKEN is not set")
	}
	return New(u, tok), nil
}

// Error is an error answer from the API (docs/API.md, "Errors").
type Error struct {
	Status  int
	Code    string
	Message string
	// RetryAfter is the server's Retry-After, set on capacity errors.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s (%s, %d)", e.Message, e.Code, e.Status)
}

// ErrorCode is the API error code in err's chain, or "" when it has none.
func ErrorCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// IsNotFound reports whether err is the API's 404. Another project's sandbox
// answers this too: it is how isolation looks from outside.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && (e.Code == api.CodeNotFound || e.Status == http.StatusNotFound)
}

// readError turns a non-2xx answer into an *Error. A body that is not the
// documented shape (a proxy's error page, say) still becomes one, with the
// body as the message, so callers have a single type to look for.
func readError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &Error{Status: resp.StatusCode}
	var body api.Error
	if json.Unmarshal(raw, &body) == nil && body.Error.Code != "" {
		e.Code, e.Message = body.Error.Code, body.Error.Message
	} else {
		e.Code = "http_" + strconv.Itoa(resp.StatusCode)
		e.Message = strings.TrimSpace(string(raw))
		if e.Message == "" {
			e.Message = resp.Status
		}
	}
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			e.RetryAfter = time.Duration(n) * time.Second
		}
	}
	return e
}

// sbPath is the path of a route under a sandbox.
func sbPath(sandbox string, parts ...string) string {
	p := "/v1/sandboxes/" + url.PathEscape(sandbox)
	for _, s := range parts {
		p += "/" + s
	}
	return p
}

// request sends one request and returns the answer when it is 2xx; anything
// else is read into an *Error and the body closed.
func (c *Client) request(ctx context.Context, method, path string, q url.Values, body io.Reader, contentType string) (*http.Response, error) {
	u := c.URL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, readError(resp)
	}
	return resp, nil
}

// do sends in as JSON (when not nil) and decodes the answer into out (when not nil).
func (c *Client) do(ctx context.Context, method, path string, q url.Values, in, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	resp, err := c.request(ctx, method, path, q, body, ct)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNoContent {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decoding the answer: %w", method, path, err)
	}
	return nil
}

// dial opens a WebSocket on path. A refused handshake carries the API's
// error body, which becomes an *Error like any other answer.
func (c *Client) dial(ctx context.Context, path string, q url.Values) (*websocket.Conn, error) {
	u, err := url.Parse(c.URL + path)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.RawQuery = q.Encode()
	h := http.Header{"Authorization": {"Bearer " + c.Token}}
	ws, resp, err := c.Dialer.DialContext(ctx, u.String(), h)
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			defer resp.Body.Close()
			return nil, readError(resp)
		}
		return nil, err
	}
	return ws, nil
}
