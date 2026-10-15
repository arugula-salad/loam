package nodeapi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// DialUpgrade turns c, a connection to a nodelet, into the agent stream of VM
// id: it sends the upgrade request and reads the 101. Anything else is an
// error carrying the nodelet's answer.
func DialUpgrade(c net.Conn, id, token string) (net.Conn, error) {
	fmt.Fprintf(c, "GET /v1/vms/%s/agent HTTP/1.1\r\nHost: nodelet\r\nAuthorization: Bearer %s\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", id, token, UpgradeAgent)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, fmt.Errorf("agent stream to %s: %w", id, err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		return nil, readError(resp)
	}
	return &bufferedConn{Conn: c, r: br}, nil
}

// AcceptUpgrade is the nodelet's half: it checks the request is an agent
// upgrade, hijacks it, and answers 101. The caller then splices the returned
// connection to the agent's vsock stream.
func AcceptUpgrade(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
	if r.Header.Get("Upgrade") != UpgradeAgent {
		return nil, errors.New("not an agent upgrade")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("hijack unsupported")
	}
	c, buf, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: "+UpgradeAgent+"\r\n\r\n"); err != nil {
		c.Close()
		return nil, err
	}
	if buf.Reader.Buffered() > 0 {
		return &bufferedConn{Conn: c, r: buf.Reader}, nil
	}
	return c, nil
}

// Splice copies both ways until either side closes, then closes both.
func Splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.SetReadDeadline(time.Now())
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
	a.Close()
	b.Close()
}

// AgentTransport carries HTTP to a VM's agent, a fresh stream per request:
// nothing is kept alive across a pause.
func AgentTransport(dial func(ctx context.Context) (net.Conn, error)) *http.Transport {
	return &http.Transport{
		DisableKeepAlives: true,
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
	}
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
