//go:build e2e

package e2e

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/loam/pkg/api"
	"github.com/arugula-salad/loam/pkg/client"
)

func TestServices(t *testing.T) {
	c := newClient(t)
	ctx := testCtx(t, 8*time.Minute)
	sb := newSandbox(t, ctx, c, "svc", api.CreateSandbox{HTTPPort: 7130})
	installServer(t, ctx, c, sb.Name)

	waitService := func(t *testing.T, name string, ok func(*api.Service) bool) *api.Service {
		t.Helper()
		var s *api.Service
		eventually(t, 30*time.Second, "service "+name, func() error {
			var err error
			if s, err = c.GetService(ctx, sb.Name, name); err != nil {
				return err
			}
			if !ok(s) {
				return fmt.Errorf("%+v", s)
			}
			return nil
		})
		return s
	}
	running := func(s *api.Service) bool { return s.State == "running" && s.PID > 0 }

	spec := api.ServiceSpec{Cmd: []string{"/usr/local/bin/e2e-server", "-port", "7130"}, Env: map[string]string{"SVC": "1"}, HTTPPort: 7130}
	put, err := c.PutService(ctx, sb.Name, "web", spec)
	if err != nil || put.Name != "web" || put.HTTPPort != 7130 {
		t.Fatalf("put: %+v %v", put, err)
	}
	web := waitService(t, "web", running)
	waitListening(t, ctx, c, sb.Name, 7130)

	t.Run("dependencies", func(t *testing.T) {
		if _, err := c.PutService(ctx, sb.Name, "db", api.ServiceSpec{Cmd: []string{"sleep", "infinity"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.PutService(ctx, sb.Name, "app", api.ServiceSpec{Cmd: []string{"sleep", "infinity"}, Needs: []string{"db"}}); err != nil {
			t.Fatal(err)
		}
		waitService(t, "app", running)
		waitService(t, "db", running)
		l, err := c.ListServices(ctx, sb.Name)
		if err != nil || len(l) != 3 {
			t.Fatalf("list: %+v %v", l, err)
		}
	})

	t.Run("logs", func(t *testing.T) {
		r, err := c.ServiceLogs(ctx, sb.Name, "web", client.LogOptions{Tail: 10})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r)
		r.Close()
		if !strings.Contains(string(b), "listening on :7130") {
			t.Fatalf("logs: %q", b)
		}
	})

	t.Run("restart on crash", func(t *testing.T) {
		sh(t, ctx, c, sb.Name, fmt.Sprintf("sudo kill -9 %d", web.PID))
		after := waitService(t, "web", func(s *api.Service) bool { return running(s) && s.Restarts >= web.Restarts+1 && s.PID != web.PID })
		web = after
		waitListening(t, ctx, c, sb.Name, 7130)
	})

	t.Run("restart", func(t *testing.T) {
		if _, err := c.RestartService(ctx, sb.Name, "web"); err != nil {
			t.Fatal(err)
		}
		web = waitService(t, "web", func(s *api.Service) bool { return running(s) && s.PID != web.PID })
		waitListening(t, ctx, c, sb.Name, 7130)
	})

	t.Run("survives pause", func(t *testing.T) {
		if _, err := c.Pause(ctx, sb.Name); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Wake(ctx, sb.Name); err != nil {
			t.Fatal(err)
		}
		s, err := c.GetService(ctx, sb.Name, "web")
		if err != nil || !running(s) || s.PID != web.PID || s.Restarts != web.Restarts {
			t.Fatalf("after pause: %+v %v, before %+v", s, err, web)
		}
	})

	t.Run("started by a request to the URL", func(t *testing.T) {
		if _, err := c.StopService(ctx, sb.Name, "web"); err != nil {
			t.Fatal(err)
		}
		waitService(t, "web", func(s *api.Service) bool { return s.State == "stopped" })
		code, body := fetch(t, urlClient(), sb.URL, bearer())
		if code != http.StatusOK || !strings.HasPrefix(body, "e2e-server 7130 ") {
			t.Fatalf("sandbox URL with the service stopped: %d %q", code, body)
		}
		web = waitService(t, "web", running)
	})

	t.Run("survives stop", func(t *testing.T) {
		if _, err := c.Stop(ctx, sb.Name); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Wake(ctx, sb.Name); err != nil {
			t.Fatal(err)
		}
		waitService(t, "web", running)
		waitService(t, "app", running)
		waitListening(t, ctx, c, sb.Name, 7130)
	})

	t.Run("start stop delete", func(t *testing.T) {
		if _, err := c.StopService(ctx, sb.Name, "app"); err != nil {
			t.Fatal(err)
		}
		waitService(t, "app", func(s *api.Service) bool { return s.State == "stopped" })
		if _, err := c.StartService(ctx, sb.Name, "app"); err != nil {
			t.Fatal(err)
		}
		waitService(t, "app", running)
		if err := c.DeleteService(ctx, sb.Name, "app"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetService(ctx, sb.Name, "app"); !client.IsNotFound(err) {
			t.Fatalf("after delete: %v", err)
		}
	})
}
