package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/arugula-salad/loam/pkg/api"
	"github.com/arugula-salad/loam/pkg/client"
)

func cmdServicesLs(a *app, args []string) error {
	pos, err := positionals(a, "loam services ls", args, 1)
	if err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	l, err := c.ListServices(a.ctx, pos[0])
	if err != nil {
		return err
	}
	if l == nil {
		l = []api.Service{}
	}
	return a.output(l, func() {
		var rows [][]string
		for _, s := range l {
			port := ""
			if s.HTTPPort > 0 {
				port = strconv.Itoa(s.HTTPPort)
			}
			pid := ""
			if s.PID > 0 {
				pid = strconv.Itoa(s.PID)
			}
			rows = append(rows, []string{s.Name, s.State, pid, strconv.Itoa(s.Restarts), port, strings.Join(s.Cmd, " ")})
		}
		a.table([]string{"NAME", "STATE", "PID", "RESTARTS", "HTTP_PORT", "CMD"}, rows)
	})
}

func (a *app) printService(s *api.Service) {
	fmt.Fprintf(a.stdout, "%-10s %s\n", "name:", s.Name)
	fmt.Fprintf(a.stdout, "%-10s %s\n", "state:", s.State)
	fmt.Fprintf(a.stdout, "%-10s %s\n", "cmd:", strings.Join(s.Cmd, " "))
	if s.PID > 0 {
		fmt.Fprintf(a.stdout, "%-10s %d\n", "pid:", s.PID)
	}
	fmt.Fprintf(a.stdout, "%-10s %d\n", "restarts:", s.Restarts)
	if s.StartedAt != nil {
		fmt.Fprintf(a.stdout, "%-10s %s\n", "started:", s.StartedAt.Format(time.RFC3339))
	}
	if s.Cwd != "" {
		fmt.Fprintf(a.stdout, "%-10s %s\n", "cwd:", s.Cwd)
	}
	if s.User != "" {
		fmt.Fprintf(a.stdout, "%-10s %s\n", "user:", s.User)
	}
	if len(s.Env) > 0 {
		fmt.Fprintf(a.stdout, "%-10s %s\n", "env:", sortedKV(s.Env))
	}
	if len(s.Needs) > 0 {
		fmt.Fprintf(a.stdout, "%-10s %s\n", "needs:", strings.Join(s.Needs, ","))
	}
	if s.HTTPPort > 0 {
		fmt.Fprintf(a.stdout, "%-10s %d\n", "http_port:", s.HTTPPort)
	}
}

func cmdServicesGet(a *app, args []string) error {
	pos, err := positionals(a, "loam services get", args, 2)
	if err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	s, err := c.GetService(a.ctx, pos[0], pos[1])
	if err != nil {
		return err
	}
	return a.output(s, func() { a.printService(s) })
}

func cmdServicesPut(a *app, args []string) error {
	fs := a.flags("loam services put")
	var spec api.ServiceSpec
	env := kvFlag{}
	var needs listFlag
	fs.StringVar(&spec.Cwd, "cwd", "", "working directory")
	fs.Var(env, "env", "K=V (repeatable)")
	fs.StringVar(&spec.User, "user", "", "run as this user (default loam)")
	fs.Var(&needs, "needs", "services to start first (repeatable or comma-separated)")
	fs.IntVar(&spec.HTTPPort, "http-port", 0, "make this the sandbox's HTTP service, on this port")
	pos, err := parseArgs(fs, args, 2)
	if err != nil {
		return err
	}
	if len(pos) < 3 {
		return errUsage
	}
	spec.Cmd = pos[2:]
	if len(env) > 0 {
		spec.Env = env
	}
	spec.Needs = needs
	c, err := a.client()
	if err != nil {
		return err
	}
	s, err := c.PutService(a.ctx, pos[0], pos[1], spec)
	if err != nil {
		return err
	}
	return a.output(s, func() { a.printService(s) })
}

func cmdServicesRm(a *app, args []string) error {
	pos, err := positionals(a, "loam services rm", args, 2)
	if err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	return c.DeleteService(a.ctx, pos[0], pos[1])
}

func serviceActionCmd(verb string) func(*app, []string) error {
	return func(a *app, args []string) error {
		pos, err := positionals(a, "loam services "+verb, args, 2)
		if err != nil {
			return err
		}
		c, err := a.client()
		if err != nil {
			return err
		}
		f := c.StartService
		switch verb {
		case "stop":
			f = c.StopService
		case "restart":
			f = c.RestartService
		}
		s, err := f(a.ctx, pos[0], pos[1])
		if err != nil {
			return err
		}
		return a.output(s, func() { fmt.Fprintf(a.stdout, "%s\t%s\n", s.Name, s.State) })
	}
}

func cmdServicesLogs(a *app, args []string) error {
	fs := a.flags("loam services logs")
	var o client.LogOptions
	fs.BoolVar(&o.Follow, "f", false, "keep printing new output")
	fs.BoolVar(&o.Follow, "follow", false, "keep printing new output")
	fs.IntVar(&o.Tail, "tail", 0, "only the last N lines")
	pos, err := parseArgs(fs, args, -1)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errUsage
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	r, err := c.ServiceLogs(a.ctx, pos[0], pos[1], o)
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = io.Copy(a.stdout, r)
	return err
}

func cmdCheckpointCreate(a *app, args []string) error {
	fs := a.flags("loam checkpoint create")
	var req api.CreateCheckpoint
	diskOnly := fs.Bool("disk-only", false, "disk only: no pause, and a restore boots fresh")
	fs.StringVar(&req.Comment, "comment", "", "a note")
	pos, err := parseArgs(fs, args, -1)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errUsage
	}
	if *diskOnly {
		f := false
		req.Memory = &f
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	cp, err := c.CreateCheckpoint(a.ctx, pos[0], req)
	if err != nil {
		return err
	}
	return a.output(cp, func() { fmt.Fprintln(a.stdout, cp.ID) })
}

func cmdCheckpointLs(a *app, args []string) error {
	pos, err := positionals(a, "loam checkpoint ls", args, 1)
	if err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	l, err := c.ListCheckpoints(a.ctx, pos[0])
	if err != nil {
		return err
	}
	if l == nil {
		l = []api.Checkpoint{}
	}
	return a.output(l, func() {
		var rows [][]string
		for _, cp := range l {
			kind := "disk"
			if cp.Memory {
				kind = "memory"
			}
			rows = append(rows, []string{cp.ID, kind, yesNo(cp.Auto), cp.CreatedAt.Local().Format(time.DateTime), cp.Comment})
		}
		a.table([]string{"ID", "KIND", "AUTO", "CREATED", "COMMENT"}, rows)
	})
}

func cmdCheckpointRestore(a *app, args []string) error {
	pos, err := positionals(a, "loam checkpoint restore", args, 2)
	if err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	sb, err := c.RestoreCheckpoint(a.ctx, pos[0], pos[1])
	if err != nil {
		return err
	}
	return a.output(sb, func() { fmt.Fprintf(a.stdout, "%s\t%s\n", sb.Name, sb.State) })
}

func cmdCheckpointRm(a *app, args []string) error {
	pos, err := positionals(a, "loam checkpoint rm", args, 2)
	if err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	return c.DeleteCheckpoint(a.ctx, pos[0], pos[1])
}

func cmdProjectCreate(a *app, args []string) error {
	pos, err := positionals(a, "loam admin project create", args, 1)
	if err != nil {
		return err
	}
	c, err := a.adminClient()
	if err != nil {
		return err
	}
	p, err := c.CreateProject(a.ctx, pos[0])
	if err != nil {
		return err
	}
	return a.output(p, func() { fmt.Fprintln(a.stdout, p.Name) })
}

func cmdProjectLs(a *app, args []string) error {
	if _, err := positionals(a, "loam admin project ls", args, 0); err != nil {
		return err
	}
	c, err := a.adminClient()
	if err != nil {
		return err
	}
	l, err := c.ListProjects(a.ctx)
	if err != nil {
		return err
	}
	if l == nil {
		l = []api.Project{}
	}
	return a.output(l, func() {
		var rows [][]string
		for _, p := range l {
			rows = append(rows, []string{p.Name, p.CreatedAt.Local().Format(time.DateTime), listOrDash(p.Domains)})
		}
		a.table([]string{"NAME", "CREATED", "DOMAINS"}, rows)
	})
}

func cmdProjectRm(a *app, args []string) error {
	pos, err := positionals(a, "loam admin project rm", args, 1)
	if err != nil {
		return err
	}
	c, err := a.adminClient()
	if err != nil {
		return err
	}
	return c.DeleteProject(a.ctx, pos[0])
}

func cmdKeyCreate(a *app, args []string) error {
	pos, err := positionals(a, "loam admin key create", args, 2)
	if err != nil {
		return err
	}
	c, err := a.adminClient()
	if err != nil {
		return err
	}
	k, err := c.CreateKey(a.ctx, pos[0], pos[1])
	if err != nil {
		return err
	}
	return a.output(k, func() {
		fmt.Fprintln(a.stdout, k.Secret)
		fmt.Fprintf(a.stderr, "key %s (%s) in project %s; the secret is not shown again\n", k.Name, k.ID, k.Project)
	})
}

func cmdKeyLs(a *app, args []string) error {
	pos, err := positionals(a, "loam admin key ls", args, 1)
	if err != nil {
		return err
	}
	c, err := a.adminClient()
	if err != nil {
		return err
	}
	l, err := c.ListKeys(a.ctx, pos[0])
	if err != nil {
		return err
	}
	if l == nil {
		l = []api.Key{}
	}
	return a.output(l, func() {
		var rows [][]string
		for _, k := range l {
			rows = append(rows, []string{k.ID, k.Name, k.CreatedAt.Local().Format(time.DateTime)})
		}
		a.table([]string{"ID", "NAME", "CREATED"}, rows)
	})
}

func cmdKeyRm(a *app, args []string) error {
	pos, err := positionals(a, "loam admin key rm", args, 2)
	if err != nil {
		return err
	}
	c, err := a.adminClient()
	if err != nil {
		return err
	}
	return c.DeleteKey(a.ctx, pos[0], pos[1])
}
