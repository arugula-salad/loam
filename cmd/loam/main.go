// Command loam is the CLI for loam's public API (docs/API.md), built on
// pkg/client. It replaces the Sprites `sprite` CLI for our use.
//
// It uses the standard flag package rather than a CLI framework: the command
// tree is two levels deep, and the one thing flag lacks, flags after
// positional arguments, is a few lines (parseArgs).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/arugula-salad/loam/pkg/client"
)

func main() {
	// No signal context: exec and attach forward SIGINT and SIGTERM to the
	// remote process, and everything else may simply die of them.
	a := &app{ctx: context.Background(), stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv}
	os.Exit(a.main(os.Args[1:]))
}

// app is one invocation: its I/O, its environment and the global flags.
type app struct {
	ctx            context.Context
	stdin          io.Reader
	stdout, stderr io.Writer
	getenv         func(string) string

	// The global flags, which every command takes.
	json       bool
	url, token string
}

type command struct {
	name    string
	args    string
	summary string
	run     func(a *app, args []string) error
	subs    []*command
}

// exitError carries a remote process's exit code out of exec and attach.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func commands() []*command {
	return []*command{
		{name: "login", args: "[--url URL] [--admin]", summary: "store the API URL and a key (read from stdin) in the user config", run: cmdLogin},
		{name: "whoami", summary: "show the key's project", run: cmdWhoami},
		{name: "create", args: "[name] [flags]", summary: "create a sandbox and wait for it to run", run: cmdCreate},
		{name: "ls", args: "[--label k=v] [--state S]", summary: "list sandboxes", run: cmdLs},
		{name: "info", args: "<sandbox>", summary: "show a sandbox", run: cmdInfo},
		{name: "update", args: "<sandbox> [flags]", summary: "change env, labels, idle rule, deadline, protection, http_port, egress rules or secrets", run: cmdUpdate},
		{name: "rm", args: "<sandbox>...", summary: "delete sandboxes and their checkpoints", run: cmdRm},
		{name: "pause", args: "<sandbox>...", summary: "pause (memory and disk kept)", run: lifecycleCmd("pause")},
		{name: "stop", args: "<sandbox>...", summary: "stop (disk kept; the next wake boots fresh)", run: lifecycleCmd("stop")},
		{name: "wake", args: "<sandbox>...", summary: "wake a paused or stopped sandbox", run: lifecycleCmd("wake")},
		{name: "templates", summary: "list templates", run: cmdTemplates},
		{name: "exec", args: "<sandbox> [flags] [--] cmd...", summary: "run a command, streaming; exits with its code", run: cmdExec},
		{name: "console", args: "<sandbox> [--user U]", summary: "an interactive login shell (exec -t)", run: cmdConsole},
		{name: "run", args: "<sandbox> [flags] [--] cmd...", summary: "one-shot exec: run to completion, then print the output", run: cmdRun},
		{name: "attach", args: "<sandbox> <session> [--offset N]", summary: "attach to a session, replaying its output", run: cmdAttach},
		{name: "sessions", args: "<sandbox>", summary: "list exec sessions", run: cmdSessions},
		{name: "kill", args: "<sandbox> <session> [--signal S]", summary: "signal a session", run: cmdKill},
		{name: "cp", args: "<src> <dst>", summary: "copy files and directories; a sandbox path is <sandbox>:/abs/path", run: cmdCp},
		{name: "ports", args: "<sandbox> [--watch]", summary: "list (or watch) listening ports", run: cmdPorts},
		{name: "port-forward", args: "<sandbox> <local>:<remote>...", summary: "forward local TCP ports into the sandbox", run: cmdPortForward},
		{name: "url", args: "<sandbox> [--port N] [--public|--private]", summary: "print (and set the access of) a sandbox or port URL", run: cmdURL},
		{name: "stats", args: "<sandbox> [--days N]", summary: "traffic at the sandbox's URLs per day: requests, statuses and visitors", run: cmdStats},
		{name: "services", summary: "manage services", subs: []*command{
			{name: "ls", args: "<sandbox>", summary: "list services", run: cmdServicesLs},
			{name: "get", args: "<sandbox> <name>", summary: "show a service", run: cmdServicesGet},
			{name: "put", args: "<sandbox> <name> [flags] [--] cmd...", summary: "create or replace a service", run: cmdServicesPut},
			{name: "rm", args: "<sandbox> <name>", summary: "stop and remove a service", run: cmdServicesRm},
			{name: "start", args: "<sandbox> <name>", summary: "start a service", run: serviceActionCmd("start")},
			{name: "stop", args: "<sandbox> <name>", summary: "stop a service", run: serviceActionCmd("stop")},
			{name: "restart", args: "<sandbox> <name>", summary: "restart a service", run: serviceActionCmd("restart")},
			{name: "logs", args: "<sandbox> <name> [-f] [--tail N]", summary: "print a service's output", run: cmdServicesLogs},
		}},
		{name: "checkpoint", summary: "manage checkpoints", subs: []*command{
			{name: "create", args: "<sandbox> [--comment C] [--disk-only]", summary: "take a checkpoint", run: cmdCheckpointCreate},
			{name: "ls", args: "<sandbox>", summary: "list checkpoints, oldest first", run: cmdCheckpointLs},
			{name: "restore", args: "<sandbox> <checkpoint>", summary: "restore (the current state is kept as auto-N first)", run: cmdCheckpointRestore},
			{name: "rm", args: "<sandbox> <checkpoint>", summary: "delete a checkpoint", run: cmdCheckpointRm},
		}},
		{name: "fork", args: "<sandbox> <checkpoint> [name]", summary: "a new sandbox from a checkpoint", run: cmdFork},
		{name: "events", args: "[--sandbox S]", summary: "stream the project's events", run: cmdEvents},
		{name: "secret", summary: "manage project secrets, which sandboxes use without holding them", subs: []*command{
			{name: "put", args: "<name> --host H... [--header H [--format F]] [--placeholder-prefix P] [--value V]", summary: "create or replace a secret; the value is read from stdin unless --value", run: cmdSecretPut},
			{name: "ls", summary: "list secrets (never their values)", run: cmdSecretLs},
			{name: "get", args: "<name>", summary: "show a secret (never its value)", run: cmdSecretGet},
			{name: "rm", args: "<name>...", summary: "delete secrets no sandbox uses", run: cmdSecretRm},
		}},
		{name: "network", summary: "egress rules", subs: []*command{
			{name: "defaults", summary: "list what --allow-defaults allows", run: cmdNetworkDefaults},
		}},
		{name: "admin", summary: "projects and keys (needs the admin token)", subs: []*command{
			{name: "project", summary: "manage projects", subs: []*command{
				{name: "create", args: "<name>", summary: "create a project", run: cmdProjectCreate},
				{name: "ls", summary: "list projects", run: cmdProjectLs},
				{name: "rm", args: "<name>", summary: "delete an empty project", run: cmdProjectRm},
			}},
			{name: "key", summary: "manage keys", subs: []*command{
				{name: "create", args: "<project> <name>", summary: "create a key; its secret is shown once", run: cmdKeyCreate},
				{name: "ls", args: "<project>", summary: "list a project's keys", run: cmdKeyLs},
				{name: "rm", args: "<project> <key>", summary: "delete a key", run: cmdKeyRm},
			}},
		}},
	}
}

func (a *app) main(args []string) int {
	err := a.dispatch(commands(), "loam", args)
	var ee exitError
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.As(err, &ee):
		return ee.code
	case errors.Is(err, errUsage):
		return 2
	default:
		fmt.Fprintln(a.stderr, "loam:", err)
		return 1
	}
}

var errUsage = errors.New("usage")

func (a *app) dispatch(cmds []*command, path string, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		a.usage(cmds, path, a.stdout)
		if len(args) == 0 && path != "loam" {
			return errUsage
		}
		return nil
	}
	for _, c := range cmds {
		if c.name != args[0] {
			continue
		}
		if c.subs != nil {
			return a.dispatch(c.subs, path+" "+c.name, args[1:])
		}
		err := c.run(a, args[1:])
		if errors.Is(err, errUsage) {
			fmt.Fprintf(a.stderr, "usage: %s %s %s\n", path, c.name, c.args)
		}
		return err
	}
	fmt.Fprintf(a.stderr, "%s: unknown command %q\n", path, args[0])
	a.usage(cmds, path, a.stderr)
	return errUsage
}

func (a *app) usage(cmds []*command, path string, w io.Writer) {
	fmt.Fprintf(w, "usage: %s <command> [args]\n\n", path)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range cmds {
		args := c.args
		if c.subs != nil {
			args = "<command> ..."
		}
		fmt.Fprintf(tw, "  %s %s\t%s\n", c.name, args, c.summary)
	}
	tw.Flush()
	if path == "loam" {
		fmt.Fprint(w, `
Every command takes --json (machine-readable output), --url and --token.
Configuration: --url/--token, then LOAM_API_URL/LOAM_TOKEN, then the file "loam login" writes.
Admin commands use LOAM_ADMIN_TOKEN, then the stored admin token, then the key.
`)
	}
}

// flags returns a FlagSet with the global flags registered.
func (a *app) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	fs.BoolVar(&a.json, "json", false, "print JSON")
	fs.StringVar(&a.url, "url", "", "the API URL (default: $LOAM_API_URL, then the stored one)")
	fs.StringVar(&a.token, "token", "", "the API key (default: $LOAM_TOKEN, then the stored one)")
	return fs
}

// client is a client for the user's key.
func (a *app) client() (*client.Client, error) {
	cfg, _ := a.loadConfig()
	u, err := a.apiURL(cfg)
	if err != nil {
		return nil, err
	}
	tok := firstNonEmpty(a.token, a.getenv("LOAM_TOKEN"), cfg.Token)
	if tok == "" {
		return nil, errors.New(`no API key: run "loam login", or set LOAM_TOKEN`)
	}
	return client.New(u, tok), nil
}

// adminClient is a client for the admin token.
func (a *app) adminClient() (*client.Client, error) {
	cfg, _ := a.loadConfig()
	u, err := a.apiURL(cfg)
	if err != nil {
		return nil, err
	}
	tok := firstNonEmpty(a.token, a.getenv("LOAM_ADMIN_TOKEN"), cfg.AdminToken, a.getenv("LOAM_TOKEN"), cfg.Token)
	if tok == "" {
		return nil, errors.New(`no admin token: run "loam login --admin", or set LOAM_ADMIN_TOKEN`)
	}
	return client.New(u, tok), nil
}

// apiURL is the API's base URL: --url, then LOAM_API_URL, then the one "loam
// login" stored. There is no default: a loam is always someone's own.
func (a *app) apiURL(cfg config) (string, error) {
	u := firstNonEmpty(a.url, a.getenv("LOAM_API_URL"), cfg.URL)
	if u == "" {
		return "", errors.New(`no API URL: pass --url, set LOAM_API_URL, or run "loam login --url https://api.<your loam's domain>"`)
	}
	return u, nil
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// printJSON prints v indented.
func (a *app) printJSON(v any) error {
	enc := json.NewEncoder(a.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// jsonLine is v as one line of JSON.
func jsonLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	return append(b, '\n'), err
}

// table prints rows under a header, aligned.
func (a *app) table(header []string, rows [][]string) {
	tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

// output prints v as JSON with --json, otherwise calls human.
func (a *app) output(v any, human func()) error {
	if a.json {
		return a.printJSON(v)
	}
	human()
	return nil
}

func sortedKV(m map[string]string) string {
	l := make([]string, 0, len(m))
	for k, v := range m {
		l = append(l, k+"="+v)
	}
	sort.Strings(l)
	return strings.Join(l, ",")
}
