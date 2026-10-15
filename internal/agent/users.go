package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// An account is a user on the sandbox's disk that a process or a file
// operation runs as.
type account struct {
	name     string
	uid, gid uint32
	groups   []uint32
	home     string
	shell    string
	self     bool // the agent's own account under RunAsSelf: nothing is switched
}

// errUnsupportedUser is a user an agent that isn't root can't switch to.
var errUnsupportedUser = errors.New("this agent is not root and runs everything as itself")

const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// defaultUser is the account processes run as when a call names none: the
// wake's, else loam.
func (s *Server) defaultUser() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wake.User != "" {
		return s.wake.User
	}
	return DefaultUser
}

// account resolves name ("" is the default user) for a process.
func (s *Server) account(name string) (*account, error) {
	if s.cfg.RunAsSelf {
		return selfAccount(name)
	}
	if name == "" {
		name = s.defaultUser()
	}
	return lookupAccount(name)
}

// fsAccount resolves the user of a file operation: root, unless the call
// names one. nil means root, with nothing to switch.
func (s *Server) fsAccount(name string) (*account, error) {
	if s.cfg.RunAsSelf {
		return selfAccount(name)
	}
	if name == "" || name == "root" {
		return nil, nil
	}
	return lookupAccount(name)
}

func selfAccount(name string) (*account, error) {
	u, err := user.Current()
	if err != nil {
		return nil, err
	}
	if name != "" && name != u.Username {
		return nil, fmt.Errorf("user %q: %w", name, errUnsupportedUser)
	}
	a := &account{name: u.Username, home: u.HomeDir, self: true}
	a.shell = passwdShell(u.Username)
	return a, nil
}

func lookupAccount(name string) (*account, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("no user %q in this sandbox", name)
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	a := &account{name: u.Username, uid: uint32(uid), gid: uint32(gid), home: u.HomeDir, shell: passwdShell(u.Username)}
	if gids, err := u.GroupIds(); err == nil {
		for _, g := range gids {
			if n, err := strconv.ParseUint(g, 10, 32); err == nil {
				a.groups = append(a.groups, uint32(n))
			}
		}
	}
	if a.home == "" {
		a.home = "/"
	}
	return a, nil
}

// credential is what a child process is started with; nil keeps the agent's.
func (a *account) credential() *syscall.Credential {
	if a == nil || a.self {
		return nil
	}
	return &syscall.Credential{Uid: a.uid, Gid: a.gid, Groups: a.groups}
}

// passwdShell is the account's login shell from /etc/passwd (os/user doesn't
// say), else bash or sh if the disk has one.
func passwdShell(name string) string {
	if f, err := os.Open("/etc/passwd"); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Split(sc.Text(), ":")
			if len(fields) == 7 && fields[0] == name && fields[6] != "" {
				if executable(fields[6]) {
					return fields[6]
				}
				break
			}
		}
	}
	for _, sh := range []string{"/bin/bash", "/usr/bin/bash", "/bin/sh", "/usr/bin/sh"} {
		if executable(sh) {
			return sh
		}
	}
	return "/bin/sh"
}

func executable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

// imageEnv is the template image's environment, which the nodelet writes to
// /etc/loam/image.json ({"env": ["K=V", ...]}, the image config's Env) when it
// flattens the image: PATH additions such as /usr/local/go/bin, and what the
// image's own tools expect. Less what the agent decides per user.
func (s *Server) imageEnv() []string {
	b, err := os.ReadFile(s.cfg.ImageMeta)
	if err != nil {
		return nil
	}
	var meta struct {
		Env []string `json:"env"`
	}
	if json.Unmarshal(b, &meta) != nil {
		return nil
	}
	var out []string
	for _, kv := range meta.Env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		switch k {
		case "HOME", "USER", "LOGNAME", "SHELL", "HOSTNAME", "TERM":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// sandboxEnv is the sandbox's env from the last wake, as KEY=value sorted by key.
func (s *Server) sandboxEnv() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.wake.Env))
	for k := range s.wake.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+s.wake.Env[k])
	}
	return out
}

// processEnv is a new process's environment (docs/AGENT.md, "Users and
// processes"): the user's own variables, then the broker CA's, the image's
// environment, the sandbox's env from the last wake and the call's env,
// later ones winning.
// PATH ends with the user's ~/.local/bin, after the system directories so it
// cannot shadow them: tools installed there (an npm or pip --user install)
// are found by name.
func (s *Server) processEnv(a *account, tty bool, call []string) []string {
	localBin := filepath.Join(a.home, ".local", "bin")
	env := []string{
		"PATH=" + defaultPath + ":" + localBin,
		"HOME=" + a.home, "USER=" + a.name, "LOGNAME=" + a.name, "SHELL=" + a.shell,
		"LANG=C.UTF-8",
	}
	if tty {
		env = append(env, "TERM=xterm-256color")
	}
	env = append(env, s.caEnv()...)
	for _, kv := range s.imageEnv() {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok && !strings.Contains(":"+v+":", ":"+localBin+":") {
			kv += ":" + localBin
		}
		env = append(env, kv)
	}
	env = append(env, s.sandboxEnv()...)
	return append(env, call...)
}

// envValue returns the last value of key in env, as exec does.
func envValue(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], key+"="); ok {
			return v
		}
	}
	return ""
}

// lookPath finds file on the PATH the command will run with, which is not
// the agent's own.
func lookPath(file string, env []string) (string, error) {
	if strings.Contains(file, "/") {
		return file, nil
	}
	path := envValue(env, "PATH")
	if path == "" {
		path = defaultPath
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		if p := filepath.Join(dir, file); executable(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("executable %q not found in PATH", file)
}

// envList turns a map into KEY=value, sorted, checking the keys.
func envList(m map[string]string) ([]string, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return nil, fmt.Errorf("invalid environment variable name %q", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out, nil
}
