package agent

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

// The /internal routes are the nodelet's (docs/AGENT.md); loam-api never
// forwards anything under /internal/ to the agent, and the guest socket
// doesn't serve them.

const (
	presuspendHookTimeout = 5 * time.Second
	poweroffGrace         = 5 * time.Second
)

// handleWake applies a Wake: at every boot and every resume, before the VM
// is reported running. The clock first: a resumed guest's is stale, and
// everything after it (task expiry, logs) wants the real time. Services
// start at the first wake of a boot; a resume finds them running.
func (s *Server) handleWake(w http.ResponseWriter, r *http.Request) {
	var wk nodeapi.Wake
	if !decodeBody(w, r, 1<<20, &wk) {
		return
	}
	// A CA that isn't certificates is refused before anything changes.
	if wk.CA != "" {
		if _, err := checkCA(wk.CA); err != nil {
			writeErr(w, http.StatusBadRequest, api.CodeInvalid, err.Error())
			return
		}
	}
	sys := s.cfg.System
	if !wk.Time.IsZero() {
		if err := sys.SetClock(wk.Time); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "set the clock: "+err.Error())
			return
		}
	}
	if wk.Hostname != "" {
		if err := sys.SetHostname(wk.Hostname); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "set the hostname: "+err.Error())
			return
		}
	}
	if wk.Network != nil {
		if err := sys.ConfigureNetwork(wk.Network); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "configure eth0: "+err.Error())
			return
		}
	}
	if wk.CA != "" {
		if err := s.installCA(wk.CA); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "install the broker CA: "+err.Error())
			return
		}
	}
	// Recorded on tmpfs, so a server process restarted by PID 1 takes it up
	// again (New) and a fresh boot starts without it.
	if b, err := json.Marshal(wk); err == nil {
		tmp := s.wakePath() + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			os.Rename(tmp, s.wakePath())
		}
	}
	s.mu.Lock()
	s.wake, s.woken = wk, true
	s.mu.Unlock()
	// The wake itself is activity: the idle window starts now, not at the
	// last thing that happened before the sandbox slept.
	s.touch()
	s.startServices()
	w.WriteHeader(http.StatusNoContent)
}

// startServices starts the services once per agent process.
func (s *Server) startServices() {
	s.mu.Lock()
	first := !s.startedSv
	s.startedSv = true
	s.mu.Unlock()
	if first {
		s.services.startAll()
	}
}

// handlePresuspend runs before a pause or a memory snapshot: the executables
// in <state>/hooks/presuspend.d, in name order, as root, with 5 s for all of
// them together; then it gives the page cache back (which the balloon then
// reclaims, so the snapshot is small) and syncs.
func (s *Server) handlePresuspend(w http.ResponseWriter, r *http.Request) {
	s.runHooks(filepath.Join(s.cfg.StateDir, "hooks", "presuspend.d"), presuspendHookTimeout)
	s.cfg.System.DropCaches()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) runHooks(dir string, budget time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	deadline := time.Now().Add(budget)
	acct, err := s.account("root")
	if err != nil {
		acct, _ = s.account("")
	}
	if acct == nil {
		return
	}
	env := s.processEnv(acct, false, nil)
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.Mode()&0o111 == 0 {
			continue
		}
		left := time.Until(deadline)
		if left <= 0 {
			logf("presuspend: out of time before %s", e.Name())
			return
		}
		cmd := &exec.Cmd{Path: p, Args: []string{p}, Env: env, Dir: "/",
			SysProcAttr: &syscall.SysProcAttr{Credential: acct.credential(), Setsid: true}}
		if err := cmd.Start(); err != nil {
			logf("presuspend hook %s: %v", e.Name(), err)
			continue
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				logf("presuspend hook %s: %v", e.Name(), err)
			}
		case <-time.After(left):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
			logf("presuspend hook %s: killed after the 5 s budget", e.Name())
			return
		}
	}
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	s.cfg.System.Sync()
	w.WriteHeader(http.StatusNoContent)
}

// handleActivity is what loam-api's idle rule reads. idle_ms is measured on
// the monotonic clock, which stands still while the VM is paused.
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	tasks := len(s.tasks.live())
	last, attached := s.sessions.activity()
	writeJSON(w, http.StatusOK, nodeapi.Activity{
		IdleMS:           time.Since(last).Milliseconds(),
		SessionsAttached: attached,
		Tasks:            tasks,
		Busy:             tasks > 0,
	})
}

// handlePoweroff answers first, then stops services (SIGTERM, 5 s, SIGKILL),
// syncs and powers off.
func (s *Server) handlePoweroff(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
	go func() {
		s.services.stopAll(poweroffGrace)
		s.cfg.System.Sync()
		s.cfg.System.Poweroff()
	}()
}

// notFound is for the guest socket's /internal.
func notFound(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotFound, api.CodeNotFound, "no such route: "+r.Method+" "+r.URL.Path)
}
