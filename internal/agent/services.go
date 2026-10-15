package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/arugula-salad/loam/pkg/api"
)

// Services are supervised processes. Their definitions live on the sandbox's
// disk, so they survive a stop and travel with checkpoints; the supervisor
// starts them all, in dependency order, at the first wake of every boot. A
// pause keeps them running: they are part of the memory snapshot.

var (
	errServiceNotFound = errors.New("service not found")
	errServiceConflict = errors.New("service conflict")
	serviceNameRE      = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)
)

const (
	defaultStopTimeout = 10 * time.Second
	maxRestartBackoff  = 30 * time.Second
	stableRunTime      = 10 * time.Second // a run this long resets the crash backoff
)

// Service output (stdout and stderr through one pipe, so their order is
// kept) is appended to <state>/logs/services/<name>.log, on the sandbox's
// disk. Rotation by size, with a bounded number of rotations, caps a
// service's output at MaxBytes*(Keep+1) bytes however chatty it is, without
// asking it to cooperate.
//
// The default is generous per file and shallow in history, because what people
// actually read is the last few minutes of a service that just misbehaved, and
// they read it through the logs API, which reads the live file.
const (
	defaultLogMaxBytes = 8 << 20 // 8 MiB live, 24 MiB per service in total
	defaultLogKeep     = 2
	maxLogKeep         = 20 // a ceiling on the configured depth: the point is to bound the disk
)

// LogRotation bounds one service's log. It is read once, when the agent starts,
// from <state>/logrotate.json ({"max_bytes":8388608,"keep":2}), so a sandbox
// that wants a longer window says so on its own disk and the setting travels
// with its checkpoints like the service definitions do.
type LogRotation struct {
	// MaxBytes rotates the live log once it has grown past this. 0 disables
	// rotation entirely, which is the explicit opt-out: the log grows unbounded again.
	MaxBytes int64 `json:"max_bytes"`
	// Keep is how many rotated files (<name>.log.1 .. .log.N, 1 newest) are kept
	// beside the live one. 0 throws the old log away at each rotation.
	Keep int `json:"keep"`
}

// logFileRE matches a service's live log and its rotations: "<name>.log[.<n>]".
var logFileRE = regexp.MustCompile(`^(.+)\.log(?:\.[0-9]+)?$`)

// loadLogRotation prefers the defaults to anything it cannot make sense of: a
// truncated or hand-edited file must not be a way to end up unbounded again.
func loadLogRotation(stateDir string) LogRotation {
	r := LogRotation{MaxBytes: defaultLogMaxBytes, Keep: defaultLogKeep}
	b, err := os.ReadFile(filepath.Join(stateDir, "logrotate.json"))
	if err != nil {
		return r
	}
	var got struct {
		MaxBytes *int64 `json:"max_bytes"`
		Keep     *int   `json:"keep"`
	}
	if json.Unmarshal(b, &got) != nil {
		return r
	}
	if got.MaxBytes != nil && *got.MaxBytes >= 0 {
		r.MaxBytes = *got.MaxBytes
	}
	if got.Keep != nil && *got.Keep >= 0 {
		r.Keep = min(*got.Keep, maxLogKeep)
	}
	return r
}

// serviceDef is a definition as stored on disk.
type serviceDef struct {
	Name string `json:"name"`
	api.ServiceSpec
}

type service struct {
	def         serviceDef
	cmd         *exec.Cmd
	pid         int
	startedAt   *time.Time
	restarts    int
	wantRunning bool
	gen         int // bumped on every start/stop so stale restart timers and exit handlers no-op
	quickFails  int
	exited      chan struct{} // closed when the current process has been reaped
	followers   map[chan string]struct{}
	logFile     *os.File
	logBytes    int64 // what logFile holds, so rotation needs no stat per line
}

// Supervisor runs the services.
type Supervisor struct {
	srv     *Server
	defsDir string // definitions; on the sandbox's disk
	logsDir string // service output; on the sandbox's disk
	runDir  string // pid files; tmpfs, so it empties on a fresh boot

	mu       sync.Mutex
	services map[string]*service
	logRot   LogRotation
}

// newSupervisor loads the definitions; nothing starts until startAll.
func newSupervisor(srv *Server, defsDir, logsDir, runDir string) *Supervisor {
	sv := &Supervisor{srv: srv, defsDir: defsDir, logsDir: logsDir, runDir: runDir,
		services: map[string]*service{}, logRot: loadLogRotation(filepath.Dir(defsDir))}
	for _, d := range []string{defsDir, logsDir, runDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			logf("services: %v", err)
		}
	}
	entries, _ := os.ReadDir(defsDir)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(defsDir, e.Name()))
		if err != nil {
			continue
		}
		var def serviceDef
		if json.Unmarshal(b, &def) != nil || def.Name == "" {
			logf("service definition %s: unreadable; ignored", e.Name())
			continue
		}
		sv.services[def.Name] = newService(def)
	}
	return sv
}

func newService(def serviceDef) *service {
	return &service{def: def, followers: map[chan string]struct{}{}}
}

// startAll starts every service, after clearing out what a previous agent
// process left running, and drops logs no definition owns.
func (sv *Supervisor) startAll() {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	sv.sweepOrphanLogsLocked()
	// Every stale process first: starting a service starts what it needs,
	// whose fresh pid file a later killStale would otherwise take for stale.
	for _, name := range sv.sortedNames() {
		sv.killStale(name)
	}
	for _, name := range sv.sortedNames() {
		sv.startLocked(name, map[string]bool{})
	}
}

func (sv *Supervisor) logPath(n string) string { return filepath.Join(sv.logsDir, n+".log") }
func (sv *Supervisor) pidPath(n string) string { return filepath.Join(sv.runDir, n+".pid") }
func (sv *Supervisor) defPath(n string) string { return filepath.Join(sv.defsDir, n+".json") }

// rotationPath is where the nth-newest rotated log lives: <name>.log.1 is the
// one just retired, .log.2 the one before it, and so on up to Keep.
func (sv *Supervisor) rotationPath(name string, n int) string {
	return sv.logPath(name) + "." + strconv.Itoa(n)
}

// openLogLocked attaches a service to its log file, picking up a log that
// earlier runs left behind (the file is appended to across restarts, so its
// size carries over) and rotating straight away if that is already over the limit.
func (sv *Supervisor) openLogLocked(s *service) error {
	f, err := os.OpenFile(sv.logPath(s.def.Name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	s.logFile, s.logBytes = f, 0
	if st, err := f.Stat(); err == nil {
		s.logBytes = st.Size()
	}
	if sv.logRot.MaxBytes > 0 && s.logBytes >= sv.logRot.MaxBytes {
		sv.rotateLocked(s)
	}
	return nil
}

// rotateLocked retires the live log and starts a new one. It renames rather
// than truncates on purpose: a reader that already has the file open (the logs
// API, a shell inside the sandbox) keeps reading the inode it opened, sees a
// complete file and never a half-erased one. The service's own handle is
// swapped here under sv.mu, which is the lock every writer holds, so no line
// is written to a file that has just been rotated away.
func (sv *Supervisor) rotateLocked(s *service) {
	name := s.def.Name
	if s.logFile != nil {
		s.logFile.Close()
		s.logFile = nil
	}
	if keep := sv.logRot.Keep; keep <= 0 {
		os.Remove(sv.logPath(name)) // no history wanted: the old log goes
	} else {
		os.Remove(sv.rotationPath(name, keep)) // the oldest falls off the end
		for i := keep - 1; i >= 1; i-- {
			os.Rename(sv.rotationPath(name, i), sv.rotationPath(name, i+1))
		}
		os.Rename(sv.logPath(name), sv.rotationPath(name, 1))
	}
	f, err := os.OpenFile(sv.logPath(name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return // logging stops until the next start; the service itself is untouched
	}
	s.logFile, s.logBytes = f, 0
}

// removeLogsLocked deletes a service's live log and every rotation of it: a
// deleted service's logs go with it, since nothing can read them back.
func (sv *Supervisor) removeLogsLocked(name string) {
	entries, _ := os.ReadDir(sv.logsDir)
	for _, e := range entries {
		if m := logFileRE.FindStringSubmatch(e.Name()); m != nil && m[1] == name {
			os.Remove(filepath.Join(sv.logsDir, e.Name()))
		}
	}
}

// sweepOrphanLogsLocked drops logs that belong to no definition (one removed
// by hand): they are unreachable through the API and pure occupancy on the
// sandbox's disk.
func (sv *Supervisor) sweepOrphanLogsLocked() {
	entries, err := os.ReadDir(sv.logsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		m := logFileRE.FindStringSubmatch(e.Name())
		if m == nil || e.IsDir() {
			continue
		}
		if _, live := sv.services[m[1]]; !live {
			os.Remove(filepath.Join(sv.logsDir, e.Name()))
		}
	}
}

func (sv *Supervisor) sortedNames() []string {
	names := make([]string, 0, len(sv.services))
	for n := range sv.services {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// killStale cleans up a process group left by a previous agent process (the
// server restarted without the guest rebooting), which would otherwise hold
// the service's ports.
func (sv *Supervisor) killStale(name string) {
	b, err := os.ReadFile(sv.pidPath(name))
	if err != nil {
		return
	}
	os.Remove(sv.pidPath(name))
	if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
		syscall.Kill(-pid, syscall.SIGKILL)
	}
}

func (sv *Supervisor) viewLocked(s *service) api.Service {
	out := api.Service{Name: s.def.Name, ServiceSpec: s.def.ServiceSpec, PID: s.pid, Restarts: s.restarts}
	switch {
	case s.cmd != nil:
		out.State, out.StartedAt = "running", s.startedAt
	case s.wantRunning:
		out.State = "crashed" // exited on its own, or failed to launch; a restart is scheduled
	default:
		out.State = "stopped"
	}
	return out
}

func (sv *Supervisor) list() []api.Service {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	out := []api.Service{}
	for _, n := range sv.sortedNames() {
		out = append(out, sv.viewLocked(sv.services[n]))
	}
	return out
}

func (sv *Supervisor) get(name string) (api.Service, error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	s, ok := sv.services[name]
	if !ok {
		return api.Service{}, errServiceNotFound
	}
	return sv.viewLocked(s), nil
}

// define creates or replaces a definition and starts it; a replaced service
// is stopped first.
func (sv *Supervisor) define(name string, spec api.ServiceSpec) (api.Service, error) {
	if !serviceNameRE.MatchString(name) {
		return api.Service{}, fmt.Errorf("invalid service name %q", name)
	}
	if len(spec.Cmd) == 0 || spec.Cmd[0] == "" {
		return api.Service{}, errors.New("cmd is required")
	}
	if spec.HTTPPort < 0 || spec.HTTPPort > 65535 {
		return api.Service{}, errors.New("http_port must be 1-65535")
	}
	if spec.Cwd != "" && !filepath.IsAbs(spec.Cwd) {
		return api.Service{}, errors.New("cwd must be absolute")
	}
	if _, err := envList(spec.Env); err != nil {
		return api.Service{}, err
	}
	if _, err := sv.srv.account(spec.User); err != nil {
		return api.Service{}, err
	}
	def := serviceDef{Name: name, ServiceSpec: spec}
	sv.mu.Lock()
	defer sv.mu.Unlock()
	for _, need := range spec.Needs {
		if _, ok := sv.services[need]; !ok || need == name {
			return api.Service{}, fmt.Errorf("needs unknown service %q", need)
		}
	}
	if spec.HTTPPort != 0 {
		for n, o := range sv.services {
			if n != name && o.def.HTTPPort != 0 {
				return api.Service{}, fmt.Errorf("%w: service %s already has the HTTP port", errServiceConflict, n)
			}
		}
	}
	old, existed := sv.services[name]
	if existed {
		prev := old.def
		old.def = def
		cycle := sv.hasCycle(name, map[string]bool{})
		old.def = prev
		if cycle {
			return api.Service{}, errors.New("needs would form a dependency cycle")
		}
	}
	b, _ := json.MarshalIndent(def, "", "  ")
	if err := os.WriteFile(sv.defPath(name)+".tmp", b, 0o644); err != nil {
		return api.Service{}, err
	}
	if err := os.Rename(sv.defPath(name)+".tmp", sv.defPath(name)); err != nil {
		return api.Service{}, err
	}
	s := old
	if existed {
		sv.stopLocked(old, defaultStopTimeout)
		old.def = def
		old.restarts = 0
	} else {
		s = newService(def)
		sv.services[name] = s
	}
	sv.startLocked(name, map[string]bool{})
	return sv.viewLocked(s), nil
}

func (sv *Supervisor) hasCycle(name string, path map[string]bool) bool {
	if path[name] {
		return true
	}
	path[name] = true
	defer delete(path, name)
	if s, ok := sv.services[name]; ok {
		for _, need := range s.def.Needs {
			if sv.hasCycle(need, path) {
				return true
			}
		}
	}
	return false
}

func (sv *Supervisor) remove(name string) error {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	s, ok := sv.services[name]
	if !ok {
		return errServiceNotFound
	}
	// A service another one needs can go too: deleting and re-creating it is
	// how a definition is replaced, and its dependants keep running. Until a
	// service of that name is created again, starting a dependant skips it.
	sv.stopLocked(s, defaultStopTimeout)
	if s.logFile != nil {
		s.logFile.Close()
		s.logFile, s.logBytes = nil, 0
	}
	for ch := range s.followers {
		close(ch)
	}
	s.followers = map[chan string]struct{}{}
	delete(sv.services, name)
	sv.removeLogsLocked(name)
	return os.Remove(sv.defPath(name))
}

func (sv *Supervisor) start(name string) (api.Service, error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	s, ok := sv.services[name]
	if !ok {
		return api.Service{}, errServiceNotFound
	}
	sv.startLocked(name, map[string]bool{})
	return sv.viewLocked(s), nil
}

func (sv *Supervisor) stop(name string) (api.Service, error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	s, ok := sv.services[name]
	if !ok {
		return api.Service{}, errServiceNotFound
	}
	sv.stopLocked(s, defaultStopTimeout)
	return sv.viewLocked(s), nil
}

func (sv *Supervisor) restart(name string) (api.Service, error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	s, ok := sv.services[name]
	if !ok {
		return api.Service{}, errServiceNotFound
	}
	sv.stopLocked(s, defaultStopTimeout)
	sv.startLocked(name, map[string]bool{})
	return sv.viewLocked(s), nil
}

// ensureHTTP starts the service that declares port as its http_port, if it
// isn't running: a request to the sandbox URL starts it (docs/API.md). It
// says whether it started one.
func (sv *Supervisor) ensureHTTP(port int) (started bool) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	for n, s := range sv.services {
		if s.def.HTTPPort == port && s.cmd == nil {
			sv.startLocked(n, map[string]bool{})
			started = true
		}
	}
	return started
}

// startLocked starts name after everything it needs. It is a no-op for a live
// service. A process that fails to launch is not an error here: it shows up in
// the service's state and is retried with backoff.
func (sv *Supervisor) startLocked(name string, visiting map[string]bool) {
	s := sv.services[name]
	if s == nil || visiting[name] {
		return
	}
	visiting[name] = true
	for _, need := range s.def.Needs {
		sv.startLocked(need, visiting)
	}
	if s.cmd != nil {
		return
	}
	s.wantRunning = true
	s.gen++
	sv.spawnLocked(s)
}

func (sv *Supervisor) spawnLocked(s *service) {
	acct, err := sv.srv.account(s.def.User)
	var cmd *exec.Cmd
	var outR, outW *os.File
	if err == nil {
		call, _ := envList(s.def.Env) // checked when defined
		env := sv.srv.processEnv(acct, false, call)
		dir := s.def.Cwd
		if dir == "" {
			dir = acct.home
		}
		var path string
		if path, err = lookPath(s.def.Cmd[0], env); err == nil {
			cmd = &exec.Cmd{Path: path, Args: s.def.Cmd, Env: env, Dir: dir,
				SysProcAttr: &syscall.SysProcAttr{Credential: acct.credential(), Setsid: true}}
		}
	}
	if err == nil {
		outR, outW, err = os.Pipe()
	}
	if err == nil && s.logFile == nil {
		err = sv.openLogLocked(s)
	}
	if err == nil {
		cmd.Stdout, cmd.Stderr = outW, outW
		err = cmd.Start()
	}
	if outW != nil {
		outW.Close()
	}
	if err != nil {
		if outR != nil {
			outR.Close()
		}
		s.pid = 0
		s.quickFails++
		sv.logLineLocked(s, "loam-agent: "+err.Error())
		sv.scheduleRestartLocked(s)
		return
	}

	now := time.Now().UTC()
	s.cmd, s.exited, s.pid, s.startedAt = cmd, make(chan struct{}), cmd.Process.Pid, &now
	os.WriteFile(sv.pidPath(s.def.Name), []byte(strconv.Itoa(s.pid)), 0o644)

	var readers sync.WaitGroup
	readers.Add(1)
	go sv.pumpLines(s, outR, &readers)
	go sv.reap(s, cmd, s.gen, s.exited, now, &readers)
}

func (sv *Supervisor) pumpLines(s *service, r *os.File, wg *sync.WaitGroup) {
	defer wg.Done()
	defer r.Close()
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			sv.mu.Lock()
			sv.logLineLocked(s, strings.TrimSuffix(line, "\n"))
			sv.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// logLineLocked appends a line to the service's log and hands it to followers.
// Service output deliberately does not count as activity: a service's work is
// not a reason for the sandbox to stay awake (a task is).
func (sv *Supervisor) logLineLocked(s *service, text string) {
	line := text + "\n"
	if s.logFile != nil {
		// Rotate before the line that would cross the limit, never in the middle
		// of one, and never after it: the live log is what the logs API reads,
		// so it should always hold the newest output rather than start out empty.
		if sv.logRot.MaxBytes > 0 && s.logBytes > 0 && s.logBytes+int64(len(line)) > sv.logRot.MaxBytes {
			sv.rotateLocked(s)
		}
		if s.logFile != nil { // a rotation that could not reopen leaves nothing to write to
			n, _ := io.WriteString(s.logFile, line)
			s.logBytes += int64(n)
		}
	}
	for ch := range s.followers {
		select {
		case ch <- line:
		default:
			// A follower this far behind is let go rather than stalling the service.
			delete(s.followers, ch)
			close(ch)
		}
	}
}

func (sv *Supervisor) reap(s *service, cmd *exec.Cmd, gen int, exited chan struct{}, started time.Time, readers *sync.WaitGroup) {
	err := cmd.Wait()
	drained := make(chan struct{})
	go func() { readers.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(outputDrainTimeout):
	}
	code := exitCode(cmd, err)

	sv.mu.Lock()
	defer sv.mu.Unlock()
	defer close(exited)
	os.Remove(sv.pidPath(s.def.Name))
	if s.cmd == cmd {
		s.cmd, s.pid = nil, 0
	}
	if gen != s.gen || !s.wantRunning {
		return // stopped or replaced on purpose
	}
	// It exited on its own: that is a crash, however clean the exit code.
	if time.Since(started) >= stableRunTime {
		s.quickFails = 0
	} else {
		s.quickFails++
	}
	s.restarts++
	sv.logLineLocked(s, fmt.Sprintf("loam-agent: exited with code %d", code))
	sv.scheduleRestartLocked(s)
}

// scheduleRestartLocked restarts a service that had been up for a while
// immediately, and backs off 1s, 2s, 4s... one that keeps dying.
func (sv *Supervisor) scheduleRestartLocked(s *service) {
	var delay time.Duration
	if s.quickFails > 0 {
		delay = min(time.Second<<min(s.quickFails-1, 5), maxRestartBackoff)
	}
	gen := s.gen
	time.AfterFunc(delay, func() {
		sv.mu.Lock()
		defer sv.mu.Unlock()
		if s.gen == gen && s.wantRunning && s.cmd == nil && sv.services[s.def.Name] == s {
			sv.spawnLocked(s)
		}
	})
}

// stopLocked is sticky: the service stays down until started again. It
// releases the lock while waiting for the process to die.
func (sv *Supervisor) stopLocked(s *service, timeout time.Duration) {
	s.wantRunning = false
	s.gen++
	if s.cmd == nil {
		return
	}
	pid, exited := s.cmd.Process.Pid, s.exited
	syscall.Kill(-pid, syscall.SIGTERM)
	sv.mu.Unlock()
	select {
	case <-exited:
	case <-time.After(timeout):
		syscall.Kill(-pid, syscall.SIGKILL)
		<-exited
	}
	sv.mu.Lock()
}

// stopAll stops every service at once: SIGTERM, then SIGKILL for those still
// running after timeout. For poweroff.
func (sv *Supervisor) stopAll(timeout time.Duration) {
	sv.mu.Lock()
	var waits []chan struct{}
	var pids []int
	for _, s := range sv.services {
		s.wantRunning = false
		s.gen++
		if s.cmd != nil {
			pids = append(pids, s.cmd.Process.Pid)
			waits = append(waits, s.exited)
			syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
		}
	}
	sv.mu.Unlock()
	deadline := time.After(timeout)
	for i, ch := range waits {
		select {
		case <-ch:
		case <-deadline:
			for _, pid := range pids[i:] {
				syscall.Kill(-pid, syscall.SIGKILL)
			}
			for _, ch := range waits[i:] {
				<-ch
			}
			return
		}
	}
}

// follow returns the last tail lines of the live log (all of it for tail <
// 0) and, if follow, a channel of the lines that come after, with no gap or
// repeat between the two: both are taken under the lock every writer holds.
func (sv *Supervisor) follow(name string, tail int, follow bool) (lines []string, ch chan string, cancel func(), err error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	s, ok := sv.services[name]
	if !ok {
		return nil, nil, nil, errServiceNotFound
	}
	if f, err := os.Open(sv.logPath(name)); err == nil {
		br := bufio.NewReaderSize(f, 64*1024)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				if lines = append(lines, line); tail >= 0 && len(lines) > tail {
					lines = lines[1:]
				}
			}
			if err != nil {
				break
			}
		}
		f.Close()
	}
	if !follow {
		return lines, nil, func() {}, nil
	}
	ch = make(chan string, 1024)
	s.followers[ch] = struct{}{}
	return lines, ch, func() {
		sv.mu.Lock()
		if _, ok := s.followers[ch]; ok {
			delete(s.followers, ch)
			close(ch)
		}
		sv.mu.Unlock()
	}, nil
}
