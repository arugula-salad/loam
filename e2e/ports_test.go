//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/loam/pkg/api"
	"github.com/arugula-salad/loam/pkg/client"
)

func TestPorts(t *testing.T) {
	c := newClient(t)
	ctx := testCtx(t, 5*time.Minute)
	sb := newSandbox(t, ctx, c, "ports", api.CreateSandbox{})
	installServer(t, ctx, c, sb.Name)

	wctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	w, err := c.WatchPorts(wctx, sb.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	events := make(chan client.PortEvent, 64)
	go func() {
		defer close(events)
		for {
			ev, err := w.Next()
			if err != nil {
				return
			}
			events <- ev
		}
	}()
	next := func(typ string, port int) api.Port {
		t.Helper()
		timeout := time.After(20 * time.Second)
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					t.Fatalf("the watch ended waiting for %s %d", typ, port)
				}
				if ev.Type == typ && ev.Port.Port == port {
					return ev.Port
				}
			case <-timeout:
				t.Fatalf("no %s event for port %d", typ, port)
			}
		}
	}

	s, err := c.Exec(ctx, sb.Name, client.ExecOptions{Cmd: []string{"/usr/local/bin/e2e-server", "-port", "7123"}})
	if err != nil {
		t.Fatal(err)
	}
	opened := next("opened", 7123)
	if opened.Session != s.Session || opened.PID == 0 {
		t.Fatalf("opened %+v, want session %s", opened, s.Session)
	}

	ps, err := c.Ports(ctx, sb.Name)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range ps {
		if p.Port == 7123 && p.Session == s.Session {
			found = true
		}
	}
	if !found {
		t.Fatalf("ports: %+v", ps)
	}

	t.Run("connect", func(t *testing.T) {
		conn, err := c.ConnectPort(ctx, sb.Name, 7123)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(20 * time.Second))
		fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || !strings.HasPrefix(string(b), "e2e-server 7123 ") {
			t.Fatalf("through connect: %s %q", resp.Status, b)
		}
	})

	t.Run("closed when the listener goes", func(t *testing.T) {
		s.Signal("TERM")
		s.Wait()
		closed := next("closed", 7123)
		if closed.Port != 7123 {
			t.Fatalf("closed %+v", closed)
		}
	})
}

func TestPortURLs(t *testing.T) {
	c := newClient(t)
	ctx := testCtx(t, 5*time.Minute)
	sb := newSandbox(t, ctx, c, "urls", api.CreateSandbox{HTTPPort: 7124})
	if sb.URL == "" || sb.HTTPPort != 7124 {
		t.Fatalf("sandbox record: url %q http_port %d", sb.URL, sb.HTTPPort)
	}
	installServer(t, ctx, c, sb.Name)
	startServer(t, ctx, c, sb.Name, 7124)
	startServer(t, ctx, c, sb.Name, 7125)
	hc := urlClient()

	t.Run("private sandbox URL", func(t *testing.T) {
		if code, body := fetch(t, hc, sb.URL, nil); code != http.StatusUnauthorized {
			t.Fatalf("without a key: %d %q", code, body)
		}
		if code, body := fetch(t, hc, sb.URL, bearer()); code != 200 || !strings.HasPrefix(body, "e2e-server 7124 ") {
			t.Fatalf("with a key: %d %q", code, body)
		}
		if err := wsEcho(t, sb.URL, bearer()); err != nil {
			t.Fatalf("WebSocket with a key: %v", err)
		}
		if err := wsEcho(t, sb.URL, nil); err == nil {
			t.Fatal("WebSocket without a key got through")
		}
	})

	t.Run("token cookie", func(t *testing.T) {
		jarred := urlClient()
		if code, body := fetch(t, jarred, sb.URL+"/?loam_token="+e2eToken(), nil); code != 200 || !strings.HasPrefix(body, "e2e-server 7124 ") {
			t.Fatalf("first visit with ?loam_token: %d %q", code, body)
		}
		if code, body := fetch(t, jarred, sb.URL+"/again", nil); code != 200 {
			t.Fatalf("second visit on the cookie: %d %q", code, body)
		}
	})

	t.Run("public port URL", func(t *testing.T) {
		set, err := c.SetPortAccess(ctx, sb.Name, 7125, "public")
		if err != nil || set.Access != "public" || set.URL == "" {
			t.Fatalf("set public: %+v %v", set, err)
		}
		settings, err := c.PortSettings(ctx, sb.Name)
		if err != nil || len(settings) == 0 {
			t.Fatalf("settings: %+v %v", settings, err)
		}
		if code, body := fetch(t, hc, set.URL, nil); code != 200 || !strings.HasPrefix(body, "e2e-server 7125 ") {
			t.Fatalf("public URL without a key: %d %q", code, body)
		}
		if err := wsEcho(t, set.URL, nil); err != nil {
			t.Fatalf("public WebSocket: %v", err)
		}
		if _, err := c.SetPortAccess(ctx, sb.Name, 7125, "private"); err != nil {
			t.Fatal(err)
		}
		if code, _ := fetch(t, hc, set.URL, nil); code != http.StatusUnauthorized {
			t.Fatalf("private again, without a key: %d", code)
		}
		if code, _ := fetch(t, hc, set.URL, bearer()); code != 200 {
			t.Fatalf("private again, with a key: %d", code)
		}
	})

	t.Run("nothing listening", func(t *testing.T) {
		set, err := c.SetPortAccess(ctx, sb.Name, 7126, "public")
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		code, body := fetch(t, hc, set.URL, nil)
		if code != http.StatusBadGateway || !strings.Contains(body, api.CodePortClosed) {
			t.Fatalf("closed port: %d %q after %s", code, body, time.Since(start))
		}
	})

	t.Run("a request wakes a paused sandbox", func(t *testing.T) {
		if _, err := c.Pause(ctx, sb.Name); err != nil {
			t.Fatal(err)
		}
		if code, body := fetch(t, hc, sb.URL, bearer()); code != 200 || !strings.HasPrefix(body, "e2e-server 7124 ") {
			t.Fatalf("after pause: %d %q", code, body)
		}
		if got, _ := c.GetSandbox(ctx, sb.Name); got.State != api.StateRunning {
			t.Fatalf("state after the request: %s", got.State)
		}
	})
}
