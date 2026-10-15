//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/arugula-salad/loam/pkg/api"
	"github.com/arugula-salad/loam/pkg/client"
)

func TestPauseStopWake(t *testing.T) {
	c := newClient(t)
	ctx := testCtx(t, 8*time.Minute)
	sb := newSandbox(t, ctx, c, "life", api.CreateSandbox{})

	t.Run("get and list", func(t *testing.T) {
		got, err := c.GetSandbox(ctx, sb.ID)
		if err != nil || got.Name != sb.Name {
			t.Fatalf("get by id: %+v %v", got, err)
		}
		l, err := c.ListSandboxes(ctx, client.ListOptions{Labels: map[string]string{"e2e": "true"}, State: api.StateRunning})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, s := range l {
			found = found || s.ID == sb.ID
			if s.State != api.StateRunning || s.Labels["e2e"] != "true" {
				t.Fatalf("list filter let through %+v", s)
			}
		}
		if !found {
			t.Fatalf("list: %s missing", sb.Name)
		}
	})

	sh(t, ctx, c, sb.Name, "echo kept > ~/file")
	pid := startSleeper(t, c, ctx, sb.Name)

	t.Run("processes and files survive pause", func(t *testing.T) {
		p, err := c.Pause(ctx, sb.Name)
		if err != nil || p.State != api.StatePaused {
			t.Fatalf("pause: %+v %v", p, err)
		}
		if got, _ := c.GetSandbox(ctx, sb.Name); got.State != api.StatePaused {
			t.Fatalf("a record read woke the sandbox: %s", got.State)
		}
		w, err := c.Wake(ctx, sb.Name)
		if err != nil || w.State != api.StateRunning {
			t.Fatalf("wake: %+v %v", w, err)
		}
		if !alive(t, c, ctx, sb.Name, pid) {
			t.Fatal("the process didn't survive the pause")
		}
		if out := sh(t, ctx, c, sb.Name, "cat ~/file"); out != "kept\n" {
			t.Fatalf("file after pause: %q", out)
		}
	})

	t.Run("a call wakes a paused sandbox", func(t *testing.T) {
		if _, err := c.Pause(ctx, sb.Name); err != nil {
			t.Fatal(err)
		}
		if out := sh(t, ctx, c, sb.Name, "cat ~/file"); out != "kept\n" {
			t.Fatalf("exec on a paused sandbox: %q", out)
		}
		if got, _ := c.GetSandbox(ctx, sb.Name); got.State != api.StateRunning {
			t.Fatalf("after the call: %s", got.State)
		}
	})

	t.Run("files survive stop, processes don't", func(t *testing.T) {
		s, err := c.Stop(ctx, sb.Name)
		if err != nil || s.State != api.StateStopped {
			t.Fatalf("stop: %+v %v", s, err)
		}
		if _, err := c.Wake(ctx, sb.Name); err != nil {
			t.Fatal(err)
		}
		if alive(t, c, ctx, sb.Name, pid) {
			t.Fatal("a process survived a stop")
		}
		if out := sh(t, ctx, c, sb.Name, "cat ~/file"); out != "kept\n" {
			t.Fatalf("file after stop: %q", out)
		}
	})

	t.Run("a call wakes a stopped sandbox", func(t *testing.T) {
		if _, err := c.Stop(ctx, sb.Name); err != nil {
			t.Fatal(err)
		}
		if out := sh(t, ctx, c, sb.Name, "cat ~/file"); out != "kept\n" {
			t.Fatalf("exec on a stopped sandbox: %q", out)
		}
	})

	t.Run("protected", func(t *testing.T) {
		yes := true
		if u, err := c.UpdateSandbox(ctx, sb.Name, api.UpdateSandbox{Protected: &yes}); err != nil || !u.Protected {
			t.Fatalf("protect: %+v %v", u, err)
		}
		if err := c.DeleteSandbox(ctx, sb.Name); client.ErrorCode(err) != api.CodeConflict {
			t.Fatalf("delete of a protected sandbox: %v", err)
		}
		no := false
		if _, err := c.UpdateSandbox(ctx, sb.Name, api.UpdateSandbox{Protected: &no}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		if err := c.DeleteSandbox(ctx, sb.Name); err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetSandbox(ctx, sb.Name); !client.IsNotFound(err) {
			t.Fatalf("after delete: %v", err)
		}
	})
}

func TestIdlePause(t *testing.T) {
	c := newClient(t)
	ctx := testCtx(t, 6*time.Minute)
	sb := newSandbox(t, ctx, c, "idle", api.CreateSandbox{Idle: &api.Idle{TimeoutS: 30, Action: api.ActionPause}})
	if sb.Idle.TimeoutS != 30 || sb.Idle.Action != api.ActionPause {
		t.Fatalf("idle rule: %+v", sb.Idle)
	}
	pid := startSleeper(t, c, ctx, sb.Name)
	last := time.Now()

	var paused time.Time
	eventually(t, 3*time.Minute, "idle pause", func() error {
		got, err := c.GetSandbox(ctx, sb.Name) // a record read is not activity
		if err != nil {
			return err
		}
		if got.State != api.StatePaused {
			return fmt.Errorf("state %s", got.State)
		}
		paused = time.Now()
		return nil
	})
	if idle := paused.Sub(last); idle < 30*time.Second {
		t.Fatalf("paused after %s idle, before the 30 s timeout", idle)
	}
	if !alive(t, c, ctx, sb.Name, pid) {
		t.Fatal("the idle pause lost a process")
	}

	t.Run("a minimum below 30s is refused", func(t *testing.T) {
		_, err := c.UpdateSandbox(ctx, sb.Name, api.UpdateSandbox{Idle: &api.Idle{TimeoutS: 5, Action: api.ActionPause}})
		if client.ErrorCode(err) != api.CodeInvalid {
			t.Fatalf("idle 5s: %v", err)
		}
	})
}

func TestEnv(t *testing.T) {
	c := newClient(t)
	ctx := testCtx(t, 6*time.Minute)
	sb := newSandbox(t, ctx, c, "env", api.CreateSandbox{Env: map[string]string{"FOO": "one"}})
	echo := `echo "$FOO $BAR"`

	if out := sh(t, ctx, c, sb.Name, echo); out != "one \n" {
		t.Fatalf("env at create: %q", out)
	}
	// A session started before the change keeps its environment.
	var old syncBuffer
	s, err := c.Exec(ctx, sb.Name, client.ExecOptions{Cmd: []string{"sh", "-c", `read x; echo "$FOO"`}, IO: client.IO{Stdout: &old}})
	if err != nil {
		t.Fatal(err)
	}
	u, err := c.UpdateSandbox(ctx, sb.Name, api.UpdateSandbox{Env: map[string]string{"FOO": "two", "BAR": "x"}})
	if err != nil || u.Env["FOO"] != "two" {
		t.Fatalf("patch env: %+v %v", u, err)
	}
	if out := sh(t, ctx, c, sb.Name, echo); out != "two x\n" {
		t.Fatalf("new process after PATCH: %q", out)
	}
	s.Write([]byte("\n"))
	if _, err := s.Wait(); err != nil || old.String() != "one\n" {
		t.Fatalf("old session: %q %v", old.String(), err)
	}
	// The call's env goes on top.
	r, err := c.Run(ctx, sb.Name, api.ExecRun{Cmd: []string{"sh", "-c", echo}, Env: map[string]string{"FOO": "three"}})
	if err != nil || string(r.Stdout) != "three x\n" {
		t.Fatalf("call env: %+v %v", r, err)
	}
	for _, step := range []struct {
		name string
		f    func(context.Context, string) (*api.Sandbox, error)
	}{{"pause", c.Pause}, {"stop", c.Stop}} {
		if _, err := step.f(ctx, sb.Name); err != nil {
			t.Fatal(err)
		}
		if out := sh(t, ctx, c, sb.Name, echo); out != "two x\n" {
			t.Fatalf("env after %s and wake: %q", step.name, out)
		}
	}
}

func TestEvents(t *testing.T) {
	c := newClient(t)
	ctx := testCtx(t, 4*time.Minute)
	sb := newSandbox(t, ctx, c, "events", api.CreateSandbox{})

	ectx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	es, err := c.Events(ectx, sb.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	seen := make(chan string, 64)
	go func() {
		defer close(seen)
		for {
			ev, err := es.Next()
			if err != nil {
				if err != io.EOF && ectx.Err() == nil {
					t.Logf("events: %v", err)
				}
				return
			}
			if ev.Sandbox == sb.ID || ev.Sandbox == sb.Name {
				seen <- ev.Type
			}
		}
	}()
	time.Sleep(time.Second)
	if _, err := c.Pause(ctx, sb.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Wake(ctx, sb.Name); err != nil {
		t.Fatal(err)
	}
	f := false
	if _, err := c.CreateCheckpoint(ctx, sb.Name, api.CreateCheckpoint{Memory: &f}); err != nil {
		t.Fatal(err)
	}
	want := []string{"sandbox.paused", "sandbox.running", "checkpoint.created"}
	var got []string
	for typ := range seen {
		got = append(got, typ)
		if containsInOrder(got, want) {
			return
		}
	}
	t.Fatalf("events %v, want %v in order", got, want)
}

// containsInOrder reports whether want appears in got in order, not necessarily adjacent.
func containsInOrder(got, want []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}
