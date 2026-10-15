//go:build e2e

package e2e

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/loam/pkg/api"
	"github.com/arugula-salad/loam/pkg/client"
)

func TestExecStream(t *testing.T) {
	c := newClient(t)
	ctx := testCtx(t, 5*time.Minute)
	sb := newSandbox(t, ctx, c, "exec", api.CreateSandbox{})

	run := func(t *testing.T, o client.ExecOptions) (int, string, string, error) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		o.Stdout, o.Stderr = &stdout, &stderr
		s, err := c.Exec(ctx, sb.Name, o)
		if err != nil {
			return -1, "", "", err
		}
		code, err := s.Wait()
		return code, stdout.String(), stderr.String(), err
	}

	t.Run("output", func(t *testing.T) {
		code, out, _, err := run(t, client.ExecOptions{Cmd: []string{"echo", "hello", "world"}})
		if err != nil || code != 0 || out != "hello world\n" {
			t.Fatalf("code=%d out=%q err=%v", code, out, err)
		}
	})

	t.Run("exit code and stderr", func(t *testing.T) {
		code, out, errOut, err := run(t, client.ExecOptions{Cmd: []string{"sh", "-c", "echo oops >&2; exit 42"}})
		if err != nil || code != 42 || out != "" || errOut != "oops\n" {
			t.Fatalf("code=%d out=%q stderr=%q err=%v", code, out, errOut, err)
		}
	})

	t.Run("stdin env cwd user", func(t *testing.T) {
		code, out, _, err := run(t, client.ExecOptions{
			Cmd: []string{"sh", "-c", `cat; echo " $FOO $(pwd) $(id -un)"`},
			Env: map[string]string{"FOO": "bar"}, Cwd: "/tmp",
			IO: client.IO{Stdin: strings.NewReader("piped")},
		})
		if err != nil || code != 0 || out != "piped bar /tmp loam\n" {
			t.Fatalf("code=%d out=%q err=%v", code, out, err)
		}
		code, out, _, err = run(t, client.ExecOptions{Cmd: []string{"id", "-un"}, User: "root"})
		if err != nil || code != 0 || out != "root\n" {
			t.Fatalf("as root: code=%d out=%q err=%v", code, out, err)
		}
	})

	t.Run("large output is complete and ordered", func(t *testing.T) {
		code, out, _, err := run(t, client.ExecOptions{Cmd: []string{"seq", "1", "200000"}})
		if err != nil || code != 0 {
			t.Fatalf("code=%d err=%v", code, err)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 200000 || lines[0] != "1" || lines[199999] != "200000" {
			t.Fatalf("got %d lines, last %q", len(lines), lines[len(lines)-1])
		}
	})

	t.Run("tty with size and resize", func(t *testing.T) {
		code, out, _, err := run(t, client.ExecOptions{Cmd: []string{"sh", "-c", "test -t 0 && test -t 1 && stty size && echo $TERM"}, TTY: true, Cols: 120, Rows: 40})
		if err != nil || code != 0 || !strings.Contains(out, "40 120") || !strings.Contains(out, "xterm-256color") {
			t.Fatalf("code=%d out=%q err=%v", code, out, err)
		}

		var buf syncBuffer
		s, err := c.Exec(ctx, sb.Name, client.ExecOptions{Cmd: []string{"sh", "-c", "echo ready; read x; stty size"}, TTY: true, Cols: 80, Rows: 24, IO: client.IO{Stdout: &buf}})
		if err != nil {
			t.Fatal(err)
		}
		buf.waitFor(t, "ready", 10*time.Second)
		if err := s.Resize(100, 30); err != nil {
			t.Fatal(err)
		}
		s.Write([]byte("go\r"))
		if code, err := s.Wait(); err != nil || code != 0 || !strings.Contains(buf.String(), "30 100") {
			t.Fatalf("after resize: code=%d out=%q err=%v", code, buf.String(), err)
		}
	})

	t.Run("signal over the stream", func(t *testing.T) {
		s, err := c.Exec(ctx, sb.Name, client.ExecOptions{Cmd: []string{"sleep", "300"}})
		if err != nil {
			t.Fatal(err)
		}
		if s.Session == "" || s.PID == 0 {
			t.Fatalf("started frame: %+v", s)
		}
		eventually(t, 10*time.Second, "session listed", func() error {
			ss, err := c.Sessions(ctx, sb.Name)
			if err != nil {
				return err
			}
			for _, x := range ss {
				if x.ID == s.Session && !x.Exited && x.PID == s.PID && strings.Join(x.Cmd, " ") == "sleep 300" {
					return nil
				}
			}
			return fmt.Errorf("sessions: %+v", ss)
		})
		if err := s.Signal("TERM"); err != nil {
			t.Fatal(err)
		}
		if code, err := s.Wait(); err != nil || code != 143 {
			t.Fatalf("after SIGTERM: code=%d err=%v", code, err)
		}
	})

	t.Run("signal by session", func(t *testing.T) {
		s, err := c.Exec(ctx, sb.Name, client.ExecOptions{Cmd: []string{"sleep", "300"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.SignalSession(ctx, sb.Name, s.Session, "KILL"); err != nil {
			t.Fatal(err)
		}
		if code, err := s.Wait(); err != nil || code != 137 {
			t.Fatalf("after SIGKILL: code=%d err=%v", code, err)
		}
	})

	t.Run("a command that can't start", func(t *testing.T) {
		_, err := c.Exec(ctx, sb.Name, client.ExecOptions{Cmd: []string{"/no/such/command"}})
		var e *client.Error
		if !errors.As(err, &e) {
			t.Fatalf("want an error frame, got %v", err)
		}
	})

	t.Run("a hung-up session ends", func(t *testing.T) {
		s, err := c.Exec(ctx, sb.Name, client.ExecOptions{Cmd: []string{"sleep", "300"}})
		if err != nil {
			t.Fatal(err)
		}
		pid := s.PID
		s.Close()
		eventually(t, 10*time.Second, "SIGHUP ends a non-detachable session", func() error {
			if shCode(t, ctx, c, sb.Name, "root", fmt.Sprintf("kill -0 %d", pid)) == 0 {
				return fmt.Errorf("pid %d still alive", pid)
			}
			return nil
		})
	})
}

func TestAttach(t *testing.T) {
	c := newClient(t)
	ctx := testCtx(t, 5*time.Minute)
	sb := newSandbox(t, ctx, c, "attach", api.CreateSandbox{})

	var first syncBuffer
	s, err := c.Exec(ctx, sb.Name, client.ExecOptions{
		Cmd:        []string{"sh", "-c", `echo one; echo two; read x; echo "got $x"; exit 5`},
		Detachable: true, IO: client.IO{Stdout: &first},
	})
	if err != nil {
		t.Fatal(err)
	}
	session := s.Session
	first.waitFor(t, "two\n", 10*time.Second)
	s.Close() // detach

	ss, err := c.Sessions(ctx, sb.Name)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range ss {
		if x.ID == session {
			found = true
			if x.Exited || x.Attached {
				t.Fatalf("after detach: %+v", x)
			}
		}
	}
	if !found {
		t.Fatalf("detached session %s not listed: %+v", session, ss)
	}

	// Everything kept is replayed.
	var replay syncBuffer
	a, err := c.Attach(ctx, sb.Name, session, client.AttachOptions{IO: client.IO{Stdout: &replay}})
	if err != nil {
		t.Fatal(err)
	}
	if a.Session != session || a.Offset != 0 {
		t.Fatalf("attached frame: %+v", a)
	}
	replay.waitFor(t, "one\ntwo\n", 10*time.Second)
	a.Close()

	// From an offset, then live: stdin reaches the process and its exit code comes back.
	off := int64(len("one\n"))
	var tail syncBuffer
	a, err = c.Attach(ctx, sb.Name, session, client.AttachOptions{Offset: &off, IO: client.IO{Stdout: &tail}})
	if err != nil {
		t.Fatal(err)
	}
	if a.Offset != off {
		t.Fatalf("attached at %d, asked for %d", a.Offset, off)
	}
	tail.waitFor(t, "two\n", 10*time.Second)
	a.Write([]byte("it\n"))
	code, err := a.Wait()
	if err != nil || code != 5 || tail.String() != "two\ngot it\n" {
		t.Fatalf("code=%d err=%v out=%q", code, err, tail.String())
	}

	// An exited session is kept for its output.
	ss, _ = c.Sessions(ctx, sb.Name)
	for _, x := range ss {
		if x.ID == session && (!x.Exited || x.ExitCode == nil || *x.ExitCode != 5) {
			t.Fatalf("exited session: %+v", x)
		}
	}
}

func TestExecRun(t *testing.T) {
	c := newClient(t)
	ctx := testCtx(t, 5*time.Minute)
	sb := newSandbox(t, ctx, c, "run", api.CreateSandbox{})

	r, err := c.Run(ctx, sb.Name, api.ExecRun{Cmd: []string{"sh", "-c", `cat; echo " $FOO $(pwd) $(id -un)"; echo err >&2; exit 3`},
		Stdin: []byte("in"), Env: map[string]string{"FOO": "bar"}, Cwd: "/tmp"})
	if err != nil || r.ExitCode != 3 || string(r.Stdout) != "in bar /tmp loam\n" || string(r.Stderr) != "err\n" || r.TimedOut {
		t.Fatalf("run: %+v %v", r, err)
	}
	r, err = c.Run(ctx, sb.Name, api.ExecRun{Cmd: []string{"id", "-u"}, User: "root"})
	if err != nil || string(r.Stdout) != "0\n" {
		t.Fatalf("as root: %+v %v", r, err)
	}
	start := time.Now()
	r, err = c.Run(ctx, sb.Name, api.ExecRun{Cmd: []string{"sleep", "60"}, TimeoutS: 2})
	if err != nil || !r.TimedOut || time.Since(start) > 30*time.Second {
		t.Fatalf("timeout: %+v %v after %s", r, err, time.Since(start))
	}
}
