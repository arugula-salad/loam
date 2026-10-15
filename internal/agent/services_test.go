package agent

import (
	"bufio"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/loam/pkg/api"
)

func (h *harness) service(name string) api.Service {
	h.t.Helper()
	var svc api.Service
	h.wantStatus(h.do("GET", "/services/"+name, nil, &svc), 200, "")
	return svc
}

func (h *harness) logs(name, query string) string {
	h.t.Helper()
	resp := h.do("GET", "/services/"+name+"/logs"+query, nil, nil)
	h.wantStatus(resp, 200, "")
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestServiceLifecycle(t *testing.T) {
	h := newHarness(t)
	var svc api.Service
	spec := api.ServiceSpec{Cmd: []string{"sh", "-c", "echo out; echo err >&2; echo $GREETING; exec sleep 60"},
		Env: map[string]string{"GREETING": "hi"}}
	h.wantStatus(h.do("PUT", "/services/web", spec, &svc), 200, "")
	if svc.Name != "web" || svc.State != "running" || svc.PID == 0 || svc.StartedAt == nil || svc.Env["GREETING"] != "hi" {
		t.Fatalf("after put %+v", svc)
	}
	waitFor(t, "log lines", func() bool { return strings.Count(h.logs("web", ""), "\n") == 3 })
	if got := h.logs("web", ""); got != "out\nerr\nhi\n" {
		t.Fatalf("logs %q", got)
	}
	if got := h.logs("web", "?tail=1"); got != "hi\n" {
		t.Fatalf("tail=1 %q", got)
	}
	var list api.ServiceList
	h.do("GET", "/services", nil, &list)
	if len(list.Services) != 1 {
		t.Fatalf("list %+v", list)
	}

	svc = api.Service{}
	h.wantStatus(h.do("POST", "/services/web/stop", nil, &svc), 200, "")
	if svc.State != "stopped" || svc.PID != 0 {
		t.Fatalf("after stop %+v", svc)
	}
	h.wantStatus(h.do("POST", "/services/web/start", nil, &svc), 200, "")
	if svc.State != "running" {
		t.Fatalf("after start %+v", svc)
	}
	pid := svc.PID
	h.wantStatus(h.do("POST", "/services/web/restart", nil, &svc), 200, "")
	if svc.State != "running" || svc.PID == pid {
		t.Fatalf("after restart %+v (was pid %d)", svc, pid)
	}

	h.wantStatus(h.do("DELETE", "/services/web", nil, nil), 204, "")
	h.wantStatus(h.do("GET", "/services/web", nil, nil), 404, api.CodeNotFound)
	if exists(h.state + "/logs/services/web.log") {
		t.Fatal("the deleted service's log is still there")
	}
}

func TestServiceCrashRestarts(t *testing.T) {
	h := newHarness(t)
	h.wantStatus(h.do("PUT", "/services/flaky", api.ServiceSpec{Cmd: []string{"sh", "-c", "echo up; exit 1"}}, nil), 200, "")
	waitFor(t, "a restart", func() bool { return h.service("flaky").Restarts >= 1 })
	if s := h.service("flaky"); s.State != "running" && s.State != "crashed" {
		t.Fatalf("state %q", s.State)
	}
	if !strings.Contains(h.logs("flaky", ""), "loam-agent: exited with code 1") {
		t.Fatalf("logs %q", h.logs("flaky", ""))
	}
}

func TestServiceValidation(t *testing.T) {
	h := newHarness(t)
	h.wantStatus(h.do("PUT", "/services/a", api.ServiceSpec{}, nil), 400, api.CodeInvalid)
	h.wantStatus(h.do("PUT", "/services/bad name", api.ServiceSpec{Cmd: []string{"true"}}, nil), 400, api.CodeInvalid)
	h.wantStatus(h.do("PUT", "/services/a", api.ServiceSpec{Cmd: []string{"sleep", "60"}, Needs: []string{"ghost"}}, nil), 400, api.CodeInvalid)
	h.wantStatus(h.do("PUT", "/services/a", api.ServiceSpec{Cmd: []string{"sleep", "60"}, HTTPPort: 8080}, nil), 200, "")
	h.wantStatus(h.do("PUT", "/services/b", api.ServiceSpec{Cmd: []string{"sleep", "60"}, HTTPPort: 8081}, nil), 409, api.CodeConflict)
	h.wantStatus(h.do("PUT", "/services/b", api.ServiceSpec{Cmd: []string{"sleep", "60"}, Needs: []string{"a"}}, nil), 200, "")
	h.wantStatus(h.do("PUT", "/services/a", api.ServiceSpec{Cmd: []string{"sleep", "60"}, Needs: []string{"b"}}, nil), 400, api.CodeInvalid)
	h.wantStatus(h.do("PUT", "/services/c", api.ServiceSpec{Cmd: []string{"true"}, User: "nobody-else"}, nil), 501, api.CodeUnsupported)
	h.wantStatus(h.do("PUT", "/services/c", `{"cmd":["true"],"restart":"always"}`, nil), 400, api.CodeInvalid)
	h.wantStatus(h.do("POST", "/services/ghost/start", nil, nil), 404, api.CodeNotFound)
}

func TestServicesComeBackAtBoot(t *testing.T) {
	h := newHarness(t)
	h.wantStatus(h.do("PUT", "/services/db", api.ServiceSpec{Cmd: []string{"sleep", "60"}}, nil), 200, "")
	h.wantStatus(h.do("PUT", "/services/app", api.ServiceSpec{Cmd: []string{"sleep", "60"}, Needs: []string{"db"}}, nil), 200, "")
	h.srv.Close()

	// A fresh boot: the disk (state) is kept, /run is not.
	os.RemoveAll(h.run)
	h2 := newHarnessIn(t, h.state, h.run)
	if s := h2.service("app"); s.State != "stopped" {
		t.Fatalf("before the wake %+v", s)
	}
	h2.wantStatus(h2.do("POST", "/internal/wake", `{"sandbox":"sb-x"}`, nil), 204, "")
	for _, n := range []string{"db", "app"} {
		if s := h2.service(n); s.State != "running" {
			t.Fatalf("%s after the wake: %+v", n, s)
		}
	}
	if s := h2.service("app"); len(s.Needs) != 1 || s.Needs[0] != "db" {
		t.Fatalf("definition %+v", s)
	}
}

func TestServiceLogsFollow(t *testing.T) {
	h := newHarness(t)
	h.wantStatus(h.do("PUT", "/services/ticker", api.ServiceSpec{Cmd: []string{"sh", "-c", "echo one; sleep 0.3; echo two; sleep 60"}}, nil), 200, "")
	waitFor(t, "first line", func() bool { return h.logs("ticker", "") == "one\n" })
	resp, err := http.Get(h.ts.URL + "/services/ticker/logs?follow=true")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	var got []string
	done := make(chan struct{})
	go func() {
		for sc.Scan() {
			if got = append(got, sc.Text()); len(got) == 2 {
				break
			}
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("followed %v", got)
	}
	if got[0] != "one" || got[1] != "two" {
		t.Fatalf("followed %v", got)
	}
}

func TestLogRotation(t *testing.T) {
	h := newHarness(t)
	h.srv.services.logRot = LogRotation{MaxBytes: 64, Keep: 1}
	h.wantStatus(h.do("PUT", "/services/chatty", api.ServiceSpec{Cmd: []string{"sh", "-c",
		"i=0; while [ $i -lt 50 ]; do echo line-$i; i=$((i+1)); done; exec sleep 60"}}, nil), 200, "")
	waitFor(t, "the last line", func() bool { return strings.Contains(h.logs("chatty", ""), "line-49") })
	if exists(h.state + "/logs/services/chatty.log.2") {
		t.Fatal("kept more rotations than asked")
	}
	if !exists(h.state + "/logs/services/chatty.log.1") {
		t.Fatal("no rotation")
	}
	fi, _ := os.Stat(h.state + "/logs/services/chatty.log")
	if fi.Size() > 64 {
		t.Fatalf("live log %d bytes", fi.Size())
	}
}
