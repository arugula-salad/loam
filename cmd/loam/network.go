package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/arugula-salad/loam/pkg/api"
)

// egressFlags are --allow, --deny and --allow-defaults (and, on update,
// --unrestricted): a sandbox's egress rules, in the order given. The API
// takes the list whole, so on update they replace it rather than add to it.
type egressFlags struct {
	rules        []api.Rule
	defaults     bool
	unrestricted bool
}

// ruleFlag is one of --allow or --deny, adding to the shared list.
type ruleFlag struct {
	e      *egressFlags
	action string
}

func (f ruleFlag) String() string { return "" }

// Set reads DOMAIN, or for an allow DOMAIN:PORT,PORT... ("github.com:22,443").
func (f ruleFlag) Set(s string) error {
	r := api.Rule{Domain: s, Action: f.action}
	if domain, ports, ok := strings.Cut(s, ":"); ok {
		if f.action == api.RuleDeny {
			return fmt.Errorf("--deny %q: a deny covers every port and takes none", s)
		}
		r.Domain = domain
		for _, p := range strings.Split(ports, ",") {
			n, err := strconv.Atoi(p)
			if err != nil {
				return fmt.Errorf("--allow %q: %q is not a port", s, p)
			}
			r.Ports = append(r.Ports, n)
		}
	}
	if r.Domain == "" {
		return fmt.Errorf("--%s needs a domain", f.action)
	}
	f.e.rules = append(f.e.rules, r)
	return nil
}

func (e *egressFlags) register(fs interface {
	Var(flag.Value, string, string)
	BoolVar(*bool, string, bool, string)
}, update bool) {
	fs.Var(ruleFlag{e, api.RuleAllow}, "allow", `allow egress to a domain ("*.example.com", "*"), on 80 and 443 or the ports given ("github.com:22,443"); repeatable`)
	fs.Var(ruleFlag{e, api.RuleDeny}, "deny", "deny egress to a domain on every port; repeatable")
	fs.BoolVar(&e.defaults, "allow-defaults", false, "allow loam's common development hosts (see \"loam network defaults\")")
	if update {
		fs.BoolVar(&e.unrestricted, "unrestricted", false, "drop every egress rule: the whole internet")
	}
}

// value is the network the flags give, or nil when none was given.
func (e *egressFlags) value() (*api.Network, error) {
	if e.unrestricted {
		if e.defaults || len(e.rules) > 0 {
			return nil, errors.New("--unrestricted drops every rule; don't give rules with it")
		}
		return &api.Network{Egress: []api.Rule{}}, nil
	}
	if !e.defaults && len(e.rules) == 0 {
		return nil, nil
	}
	n := &api.Network{Egress: []api.Rule{}}
	if e.defaults {
		n.Egress = append(n.Egress, api.Rule{Include: "defaults"})
	}
	n.Egress = append(n.Egress, e.rules...)
	return n, nil
}

// ruleString is a rule as the flags write it.
func ruleString(r api.Rule) string {
	switch {
	case r.Include != "":
		return "include " + r.Include
	case len(r.Ports) > 0:
		ports := make([]string, len(r.Ports))
		for i, p := range r.Ports {
			ports[i] = strconv.Itoa(p)
		}
		return r.Action + " " + r.Domain + ":" + strings.Join(ports, ",")
	}
	return r.Action + " " + r.Domain
}

func networkString(n api.Network) string {
	if len(n.Egress) == 0 {
		return "unrestricted"
	}
	l := make([]string, len(n.Egress))
	for i, r := range n.Egress {
		l[i] = ruleString(r)
	}
	return strings.Join(l, ", ")
}

func cmdNetworkDefaults(a *app, args []string) error {
	if _, err := positionals(a, "loam network defaults", args, 0); err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	l, err := c.NetworkDefaults(a.ctx)
	if err != nil {
		return err
	}
	if l == nil {
		l = []api.Rule{}
	}
	return a.output(api.Network{Egress: l}, func() {
		for _, r := range l {
			fmt.Fprintln(a.stdout, ruleString(r))
		}
	})
}

func cmdSecretPut(a *app, args []string) error {
	fs := a.flags("loam secret put")
	var hosts listFlag
	var req api.PutSecret
	var value, header, format string
	fs.Var(&hosts, "host", `where the value may go, over HTTPS: "api.github.com", "*.github.com", with an optional path prefix; repeatable, or comma-separated`)
	fs.StringVar(&value, "value", "", "the value (default: read from stdin; a flag ends up in shell history)")
	fs.StringVar(&header, "header", "", "set this header on every request to the hosts, instead of substituting the placeholder")
	fs.StringVar(&format, "format", "", `the --header value, holding "{value}" once (default "{value}"; "Bearer {value}")`)
	fs.StringVar(&req.PlaceholderPrefix, "placeholder-prefix", "", `put before the placeholder, for a client that checks its token's shape ("sk-ant-oat01-")`)
	pos, err := parseArgs(fs, args, -1)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errUsage
	}
	req.Hosts = hosts
	switch {
	case header != "":
		if format == "" {
			format = "{value}"
		}
		req.Inject = &api.Inject{Header: header, Format: format}
	case format != "":
		return errors.New("--format needs --header")
	}
	if value == "" {
		if value, err = a.readValue("value of " + pos[0] + ": "); err != nil {
			return err
		}
	}
	req.Value = value
	c, err := a.client()
	if err != nil {
		return err
	}
	s, err := c.PutSecret(a.ctx, pos[0], req)
	if err != nil {
		return err
	}
	return a.output(s, func() { a.printSecret(s) })
}

// readValue reads a secret's value from stdin: without echo when it is a
// terminal, else all of it, less one trailing newline (echo's, or a file's).
func (a *app) readValue(prompt string) (string, error) {
	if f, ok := a.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(a.stderr, prompt)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(a.stderr)
		return string(b), err
	}
	b, err := io.ReadAll(io.LimitReader(a.stdin, 1<<20))
	if err != nil {
		return "", err
	}
	b = bytes.TrimSuffix(b, []byte("\n"))
	b = bytes.TrimSuffix(b, []byte("\r"))
	if len(b) == 0 {
		return "", errors.New("no value on stdin")
	}
	return string(b), nil
}

func (a *app) printSecret(s *api.Secret) {
	rows := [][2]string{{"name", s.Name}, {"placeholder", s.Placeholder}, {"hosts", strings.Join(s.Hosts, ",")}}
	if s.Inject != nil {
		rows = append(rows, [2]string{"inject", s.Inject.Header + ": " + s.Inject.Format})
	}
	rows = append(rows, [2]string{"created", s.CreatedAt.Format(time.RFC3339)}, [2]string{"updated", s.UpdatedAt.Format(time.RFC3339)})
	for _, r := range rows {
		fmt.Fprintf(a.stdout, "%-12s %s\n", r[0]+":", r[1])
	}
}

func cmdSecretLs(a *app, args []string) error {
	if _, err := positionals(a, "loam secret ls", args, 0); err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	l, err := c.ListSecrets(a.ctx)
	if err != nil {
		return err
	}
	if l == nil {
		l = []api.Secret{}
	}
	return a.output(l, func() {
		var rows [][]string
		for _, s := range l {
			inject := "-"
			if s.Inject != nil {
				inject = s.Inject.Header
			}
			rows = append(rows, []string{s.Name, s.Placeholder, strings.Join(s.Hosts, ","), inject, age(s.UpdatedAt)})
		}
		a.table([]string{"NAME", "PLACEHOLDER", "HOSTS", "HEADER", "UPDATED"}, rows)
	})
}

func cmdSecretGet(a *app, args []string) error {
	pos, err := positionals(a, "loam secret get", args, 1)
	if err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	s, err := c.GetSecret(a.ctx, pos[0])
	if err != nil {
		return err
	}
	return a.output(s, func() { a.printSecret(s) })
}

func cmdSecretRm(a *app, args []string) error {
	fs := a.flags("loam secret rm")
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
	for _, name := range pos {
		if err := c.DeleteSecret(a.ctx, name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if !a.json {
			fmt.Fprintln(a.stdout, name)
		}
	}
	return errors.Join(errs...)
}
