package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/arugula-salad/loam/pkg/api"
	"github.com/arugula-salad/loam/pkg/client"
)

func cmdLogin(a *app, args []string) error {
	fs := a.flags("loam login")
	admin := fs.Bool("admin", false, "store the admin token instead of a key")
	if _, err := parseArgs(fs, args, 0); err != nil {
		return err
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	u, err := a.apiURL(cfg)
	if err != nil {
		return err
	}
	tok := a.token
	if tok == "" {
		what := "API key"
		if *admin {
			what = "admin token"
		}
		if tok, err = a.readSecret(what + " for " + u + ": "); err != nil {
			return err
		}
	}
	if tok == "" {
		return errors.New("no token given")
	}
	c := client.New(u, tok)
	var msg string
	if *admin {
		ps, err := c.ListProjects(a.ctx)
		if err != nil {
			return fmt.Errorf("checking the admin token: %w", err)
		}
		cfg.AdminToken = tok
		msg = fmt.Sprintf("admin token for %s stored (%d projects)", u, len(ps))
	} else {
		w, err := c.Whoami(a.ctx)
		if err != nil {
			return fmt.Errorf("checking the key: %w", err)
		}
		cfg.Token = tok
		msg = fmt.Sprintf("logged in to %s as key %q of project %s", u, w.Key.Name, w.Project.Name)
	}
	cfg.URL = u
	p, err := a.saveConfig(cfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stderr, "%s (%s)\n", msg, p)
	return nil
}

// readSecret reads a line from stdin, without echo when it is a terminal.
func (a *app) readSecret(prompt string) (string, error) {
	if f, ok := a.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(a.stderr, prompt)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(a.stderr)
		return strings.TrimSpace(string(b)), err
	}
	line, err := bufio.NewReader(a.stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func cmdWhoami(a *app, args []string) error {
	fs := a.flags("loam whoami")
	if _, err := parseArgs(fs, args, 0); err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	w, err := c.Whoami(a.ctx)
	if err != nil {
		return err
	}
	return a.output(w, func() {
		fmt.Fprintf(a.stdout, "project %s, key %s (%s)\n", w.Project.Name, w.Key.Name, w.Key.ID)
		if len(w.Project.Domains) > 0 {
			fmt.Fprintf(a.stdout, "URL domains: %s\n", strings.Join(w.Project.Domains, ", "))
		}
	})
}

// listOrDash is a list for a table cell: comma-separated, or "-" when empty.
func listOrDash(l []string) string {
	if len(l) == 0 {
		return "-"
	}
	return strings.Join(l, ",")
}

// madeFrom is what a sandbox was made from, for a table: its template, or
// its image's repository (the digest is in info).
func madeFrom(s api.Sandbox) string {
	if s.Image != "" {
		repo, _, _ := strings.Cut(s.Image, "@")
		return repo
	}
	return s.Template
}

// deadlineFlags are --deadline and --deadline-action, shared by create and update.
type deadlineFlags struct{ at, action string }

func (d *deadlineFlags) register(fs interface {
	StringVar(*string, string, string, string)
}) {
	fs.StringVar(&d.at, "deadline", "", `a deadline: a duration from now ("2h") or an RFC 3339 time; "none" clears it (update)`)
	fs.StringVar(&d.action, "deadline-action", "", "what happens at the deadline: delete (default), pause or stop")
}

// value is the deadline, or nil when none was asked for.
func (d *deadlineFlags) value(now time.Time) (*api.Deadline, error) {
	if d.at == "" {
		if d.action != "" {
			return nil, errors.New("--deadline-action needs --deadline")
		}
		return nil, nil
	}
	dl := &api.Deadline{Action: d.action}
	if dl.Action == "" {
		dl.Action = api.ActionDelete
	}
	if dur, err := time.ParseDuration(d.at); err == nil {
		dl.At = now.Add(dur).UTC().Truncate(time.Second)
	} else if t, err := time.Parse(time.RFC3339, d.at); err == nil {
		dl.At = t
	} else {
		return nil, fmt.Errorf("bad --deadline %q: want a duration or an RFC 3339 time", d.at)
	}
	return dl, nil
}

// idleValue is the idle rule from --idle and --idle-action, or nil when neither was given.
func idleValue(timeout time.Duration, action string) (*api.Idle, error) {
	if timeout == 0 && action == "" {
		return nil, nil
	}
	if timeout == 0 {
		// The rule is set whole; keep the documented default timeout.
		timeout = 300 * time.Second
	}
	if timeout%time.Second != 0 {
		return nil, fmt.Errorf("--idle %s: whole seconds only", timeout)
	}
	if action == "" {
		action = api.ActionPause
	}
	return &api.Idle{TimeoutS: int(timeout / time.Second), Action: action}, nil
}

func cmdCreate(a *app, args []string) error {
	fs := a.flags("loam create")
	var req api.CreateSandbox
	env, labels := kvFlag{}, kvFlag{}
	var cpus, disk int
	var memory, idleAction string
	var idle time.Duration
	var dl deadlineFlags
	fs.StringVar(&req.Template, "template", "", "the template (default base)")
	fs.StringVar(&req.Image, "image", "", "an OCI image to make it from instead of a template, <repository>@sha256:<digest>")
	fs.IntVar(&cpus, "cpus", 0, "vCPUs")
	fs.StringVar(&memory, "memory", "", "memory, in MiB or with a unit (2g)")
	fs.IntVar(&disk, "disk", 0, "disk size in GiB")
	fs.Var(env, "env", "K=V given to every process (repeatable)")
	fs.Var(labels, "label", "k=v label (repeatable)")
	fs.DurationVar(&idle, "idle", 0, "idle timeout (default 5m, minimum 30s)")
	fs.StringVar(&idleAction, "idle-action", "", "pause (default), stop or none")
	fs.BoolVar(&req.Protected, "protected", false, "refuse deletion until unprotected")
	fs.IntVar(&req.HTTPPort, "http-port", 0, "the port the sandbox URL goes to")
	dl.register(fs)
	var egress egressFlags
	egress.register(fs, false)
	var secrets listFlag
	fs.Var(&secrets, "secret", "a project secret the sandbox uses, in its env as a placeholder (repeatable)")
	pos, err := parseArgs(fs, args, -1)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return errUsage
	}
	if req.Network, err = egress.value(); err != nil {
		return err
	}
	if len(secrets) > 0 {
		names := []string(secrets)
		req.Secrets = &names
	}
	if len(pos) == 1 {
		req.Name = pos[0]
	}
	if cpus > 0 || memory != "" || disk > 0 {
		req.Resources = &api.Resources{CPUs: cpus, DiskGiB: disk}
		if memory != "" {
			if req.Resources.MemoryMiB, err = parseMiB(memory); err != nil {
				return err
			}
		}
	}
	if len(env) > 0 {
		req.Env = env
	}
	if len(labels) > 0 {
		req.Labels = labels
	}
	if req.Idle, err = idleValue(idle, idleAction); err != nil {
		return err
	}
	if req.Deadline, err = dl.value(time.Now()); err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	sb, err := c.CreateSandbox(a.ctx, req)
	if err != nil {
		return err
	}
	return a.output(sb, func() { a.printSandbox(sb) })
}

func cmdLs(a *app, args []string) error {
	fs := a.flags("loam ls")
	labels := kvFlag{}
	var o client.ListOptions
	fs.Var(labels, "label", "only sandboxes with this k=v label (repeatable)")
	fs.StringVar(&o.State, "state", "", "only sandboxes in this state")
	if pos, err := parseArgs(fs, args, -1); err != nil {
		return err
	} else if len(pos) > 0 {
		return errUsage
	}
	o.Labels = labels
	c, err := a.client()
	if err != nil {
		return err
	}
	l, err := c.ListSandboxes(a.ctx, o)
	if err != nil {
		return err
	}
	if l == nil {
		l = []api.Sandbox{}
	}
	return a.output(l, func() {
		rows := make([][]string, 0, len(l))
		for _, s := range l {
			rows = append(rows, []string{s.Name, s.ID, s.State, madeFrom(s), strconv.Itoa(s.Resources.CPUs),
				strconv.Itoa(s.Resources.MemoryMiB), sortedKV(s.Labels), age(s.CreatedAt)})
		}
		a.table([]string{"NAME", "ID", "STATE", "TEMPLATE", "CPUS", "MEM_MIB", "LABELS", "AGE"}, rows)
	})
}

// age is how long ago t was, short.
func age(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// positionals parses a command that takes exactly n positionals and only the global flags.
func positionals(a *app, name string, args []string, n int) ([]string, error) {
	fs := a.flags(name)
	pos, err := parseArgs(fs, args, -1)
	if err != nil {
		return nil, err
	}
	if len(pos) != n {
		return nil, errUsage
	}
	return pos, nil
}

func cmdInfo(a *app, args []string) error {
	pos, err := positionals(a, "loam info", args, 1)
	if err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	sb, err := c.GetSandbox(a.ctx, pos[0])
	if err != nil {
		return err
	}
	return a.output(sb, func() { a.printSandbox(sb) })
}

func (a *app) printSandbox(s *api.Sandbox) {
	rows := [][2]string{
		{"name", s.Name}, {"id", s.ID}, {"project", s.Project},
	}
	if s.Image != "" {
		rows = append(rows, [2]string{"image", s.Image})
	} else {
		rows = append(rows, [2]string{"template", s.Template})
	}
	rows = append(rows, [2]string{"state", s.State})
	if s.Error != "" {
		rows = append(rows, [2]string{"error", s.Error})
	}
	rows = append(rows,
		[2]string{"resources", fmt.Sprintf("%d cpus, %d MiB, %d GiB disk", s.Resources.CPUs, s.Resources.MemoryMiB, s.Resources.DiskGiB)},
		[2]string{"url", s.URL},
	)
	for _, u := range s.URLs {
		rows = append(rows, [2]string{"url", u})
	}
	rows = append(rows,
		[2]string{"http_port", strconv.Itoa(s.HTTPPort)},
		[2]string{"idle", fmt.Sprintf("%s after %ds", s.Idle.Action, s.Idle.TimeoutS)},
	)
	if s.Deadline != nil {
		rows = append(rows, [2]string{"deadline", fmt.Sprintf("%s at %s", s.Deadline.Action, s.Deadline.At.Format(time.RFC3339))})
	}
	if s.Protected {
		rows = append(rows, [2]string{"protected", "yes"})
	}
	if len(s.Env) > 0 {
		rows = append(rows, [2]string{"env", sortedKV(s.Env)})
	}
	if len(s.Labels) > 0 {
		rows = append(rows, [2]string{"labels", sortedKV(s.Labels)})
	}
	rows = append(rows, [2]string{"network", networkString(s.Network)})
	if len(s.Secrets) > 0 {
		rows = append(rows, [2]string{"secrets", strings.Join(s.Secrets, ",")})
	}
	rows = append(rows,
		[2]string{"created", s.CreatedAt.Format(time.RFC3339)},
		[2]string{"last_active", s.LastActiveAt.Format(time.RFC3339)},
	)
	for _, r := range rows {
		fmt.Fprintf(a.stdout, "%-12s %s\n", r[0]+":", r[1])
	}
}

func cmdUpdate(a *app, args []string) error {
	fs := a.flags("loam update")
	env, labels := kvFlag{}, kvFlag{}
	var unsetEnv, unsetLabel listFlag
	var idle time.Duration
	var idleAction, protected string
	httpPort := -1
	var dl deadlineFlags
	fs.Var(env, "env", "set K=V in the env (repeatable); processes started after this and later wakes see it")
	fs.Var(&unsetEnv, "unset-env", "remove K from the env (repeatable)")
	fs.Var(labels, "label", "set a k=v label (repeatable)")
	fs.Var(&unsetLabel, "unset-label", "remove a label (repeatable)")
	fs.DurationVar(&idle, "idle", 0, "idle timeout")
	fs.StringVar(&idleAction, "idle-action", "", "pause, stop or none")
	fs.StringVar(&protected, "protected", "", "true or false")
	fs.IntVar(&httpPort, "http-port", -1, "the port the sandbox URL goes to")
	dl.register(fs)
	var egress egressFlags
	egress.register(fs, true)
	var secrets, unsetSecret listFlag
	fs.Var(&secrets, "secret", "add a project secret the sandbox uses (repeatable)")
	fs.Var(&unsetSecret, "unset-secret", "stop using a project secret (repeatable)")
	pos, err := parseArgs(fs, args, -1)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errUsage
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	var req api.UpdateSandbox
	if req.Network, err = egress.value(); err != nil {
		return err
	}
	// env, labels and secrets are replaced whole by a PATCH, so merge into the current ones.
	if len(env)+len(unsetEnv)+len(labels)+len(unsetLabel)+len(secrets)+len(unsetSecret) > 0 || (idle != 0) != (idleAction != "") {
		cur, err := c.GetSandbox(a.ctx, pos[0])
		if err != nil {
			return err
		}
		if len(env)+len(unsetEnv) > 0 {
			req.Env = merge(cur.Env, env, unsetEnv)
		}
		if len(labels)+len(unsetLabel) > 0 {
			req.Labels = merge(cur.Labels, labels, unsetLabel)
		}
		if len(secrets)+len(unsetSecret) > 0 {
			names := mergeList(cur.Secrets, secrets, unsetSecret)
			req.Secrets = &names
		}
		if idle == 0 && idleAction != "" {
			idle = time.Duration(cur.Idle.TimeoutS) * time.Second
		} else if idle != 0 && idleAction == "" {
			idleAction = cur.Idle.Action
		}
	}
	if req.Idle, err = idleValue(idle, idleAction); err != nil {
		return err
	}
	if dl.at == "none" {
		var none *api.Deadline
		req.Deadline = &none
	} else if d, err := dl.value(time.Now()); err != nil {
		return err
	} else if d != nil {
		req.Deadline = &d
	}
	if protected != "" {
		b, err := strconv.ParseBool(protected)
		if err != nil {
			return fmt.Errorf("--protected %q: want true or false", protected)
		}
		req.Protected = &b
	}
	if httpPort >= 0 {
		req.HTTPPort = &httpPort
	}
	sb, err := c.UpdateSandbox(a.ctx, pos[0], req)
	if err != nil {
		return err
	}
	return a.output(sb, func() { a.printSandbox(sb) })
}

// merge is cur with set applied and unset removed; never nil, so a PATCH
// that empties a map sends {} rather than leaving it alone.
func merge(cur, set map[string]string, unset []string) map[string]string {
	m := map[string]string{}
	for k, v := range cur {
		m[k] = v
	}
	for k, v := range set {
		m[k] = v
	}
	for _, k := range unset {
		delete(m, k)
	}
	return m
}

// mergeList is cur with add appended (once each) and unset removed; never nil.
func mergeList(cur, add, unset []string) []string {
	out := []string{}
	for _, v := range append(append([]string{}, cur...), add...) {
		if !slices.Contains(out, v) && !slices.Contains(unset, v) {
			out = append(out, v)
		}
	}
	return out
}

func cmdRm(a *app, args []string) error {
	fs := a.flags("loam rm")
	pos, err := parseArgs(fs, args, -1)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return errUsage
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	var errs []error
	for _, s := range pos {
		if err := c.DeleteSandbox(a.ctx, s); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s, err))
			continue
		}
		if !a.json {
			fmt.Fprintln(a.stdout, s)
		}
	}
	return errors.Join(errs...)
}

func lifecycleCmd(verb string) func(*app, []string) error {
	return func(a *app, args []string) error {
		fs := a.flags("loam " + verb)
		pos, err := parseArgs(fs, args, -1)
		if err != nil {
			return err
		}
		if len(pos) == 0 {
			return errUsage
		}
		c, err := a.client()
		if err != nil {
			return err
		}
		f := c.Pause
		switch verb {
		case "stop":
			f = c.Stop
		case "wake":
			f = c.Wake
		}
		var errs []error
		var done []*api.Sandbox
		for _, s := range pos {
			sb, err := f(a.ctx, s)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", s, err))
				continue
			}
			done = append(done, sb)
			if !a.json {
				fmt.Fprintf(a.stdout, "%s\t%s\n", sb.Name, sb.State)
			}
		}
		if a.json {
			if err := a.printJSON(done); err != nil {
				return err
			}
		}
		return errors.Join(errs...)
	}
}

func cmdTemplates(a *app, args []string) error {
	if _, err := positionals(a, "loam templates", args, 0); err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	l, err := c.ListTemplates(a.ctx)
	if err != nil {
		return err
	}
	return a.output(l, func() {
		var rows [][]string
		for _, t := range l {
			rows = append(rows, []string{t.Name, t.Image, strconv.Itoa(t.Resources.CPUs), strconv.Itoa(t.Resources.MemoryMiB), strconv.Itoa(t.Resources.DiskGiB)})
		}
		a.table([]string{"NAME", "IMAGE", "CPUS", "MEM_MIB", "DISK_GIB"}, rows)
	})
}

func cmdFork(a *app, args []string) error {
	fs := a.flags("loam fork")
	pos, err := parseArgs(fs, args, -1)
	if err != nil {
		return err
	}
	if len(pos) < 2 || len(pos) > 3 {
		return errUsage
	}
	name := ""
	if len(pos) == 3 {
		name = pos[2]
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	sb, err := c.Fork(a.ctx, pos[0], pos[1], name)
	if err != nil {
		return err
	}
	return a.output(sb, func() { a.printSandbox(sb) })
}

func cmdEvents(a *app, args []string) error {
	fs := a.flags("loam events")
	sandbox := fs.String("sandbox", "", "only this sandbox's events")
	if pos, err := parseArgs(fs, args, -1); err != nil {
		return err
	} else if len(pos) > 0 {
		return errUsage
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	es, err := c.Events(a.ctx, *sandbox)
	if err != nil {
		return err
	}
	defer es.Close()
	for {
		ev, err := es.Next()
		if err != nil {
			if errors.Is(err, io.EOF) || a.ctx.Err() != nil {
				return nil
			}
			return err
		}
		if a.json {
			// One object per line, so it pipes into jq as a stream.
			b, _ := jsonLine(ev)
			a.stdout.Write(b)
			continue
		}
		fmt.Fprintf(a.stdout, "%s  %-22s %s\n", ev.At.Local().Format(time.TimeOnly), ev.Type, ev.Sandbox)
	}
}

func cmdURL(a *app, args []string) error {
	fs := a.flags("loam url")
	port := fs.Int("port", 0, "a port's URL instead of the sandbox's (which goes to http_port)")
	public := fs.Bool("public", false, "open the URL to anyone")
	private := fs.Bool("private", false, "require the project's key for the URL")
	pos, err := parseArgs(fs, args, -1)
	if err != nil {
		return err
	}
	if len(pos) != 1 || (*public && *private) {
		return errUsage
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	sb, err := c.GetSandbox(a.ctx, pos[0])
	if err != nil {
		return err
	}
	p := *port
	if p == 0 {
		p = sb.HTTPPort
	}
	access := ""
	switch {
	case *public:
		access = "public"
	case *private:
		access = "private"
	}
	var setting *api.PortSetting
	if access != "" {
		if setting, err = c.SetPortAccess(a.ctx, sb.Name, p, access); err != nil {
			return err
		}
	} else {
		settings, err := c.PortSettings(a.ctx, sb.Name)
		if err != nil {
			return err
		}
		for _, s := range settings {
			if s.Port == p {
				setting = &s
				break
			}
		}
	}
	out := api.PortSetting{Port: p, Access: "private"}
	if setting != nil {
		out = *setting
	}
	switch {
	case *port == 0:
		// The sandbox's own URL, which goes to http_port under that port's access.
		out.URL = sb.URL
	case out.URL == "":
		out.URL = portURL(sb.URL, p)
	}
	return a.output(out, func() { fmt.Fprintln(a.stdout, out.URL) })
}

// portURL derives https://<port>--<name>--<project>.<domain> from the
// sandbox URL https://<name>--<project>.<domain>, for a port whose access was
// never set (and so has no setting to carry its URL).
func portURL(sandboxURL string, port int) string {
	u, err := url.Parse(sandboxURL)
	if err != nil || u.Host == "" {
		return ""
	}
	u.Host = strconv.Itoa(port) + "--" + u.Host
	return u.String()
}
