package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// parseArgs parses flags anywhere among the positional arguments, which
// flag.Parse alone doesn't. With maxPos >= 0, the positional after the first
// maxPos starts the rest, which is taken as it is: `loam exec sb -t ls -la`
// runs `ls -la` with a TTY. A "--" ends the flags too.
func parseArgs(fs *flag.FlagSet, args []string, maxPos int) ([]string, error) {
	var pos []string
	for len(args) > 0 {
		a := args[0]
		switch {
		case a == "--":
			return append(pos, args[1:]...), nil
		case strings.HasPrefix(a, "-") && a != "-":
			// Parse up to the next "--", which flag.Parse would otherwise eat.
			end := len(args)
			for i, s := range args {
				if s == "--" {
					end = i
					break
				}
			}
			if err := fs.Parse(args[:end]); err != nil {
				return nil, err
			}
			args = append(fs.Args(), args[end:]...)
		case maxPos >= 0 && len(pos) >= maxPos:
			return append(pos, args...), nil
		default:
			pos = append(pos, a)
			args = args[1:]
		}
	}
	return pos, nil
}

// kvFlag is a repeatable K=V flag.
type kvFlag map[string]string

func (f kvFlag) String() string { return sortedKV(f) }

func (f kvFlag) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("%q is not K=V", s)
	}
	f[k] = v
	return nil
}

// listFlag is a repeatable string flag that also takes comma-separated values.
type listFlag []string

func (f *listFlag) String() string { return strings.Join(*f, ",") }

func (f *listFlag) Set(s string) error {
	for _, v := range strings.Split(s, ",") {
		if v != "" {
			*f = append(*f, v)
		}
	}
	return nil
}

// parseMiB reads a memory size: a bare number is MiB; "m"/"mi"/"mib", "g"/"gi"/"gib" suffixes work too.
func parseMiB(s string) (int, error) {
	l := strings.ToLower(strings.TrimSpace(s))
	mult := 1
	for _, suf := range []struct {
		s string
		m int
	}{{"gib", 1024}, {"gi", 1024}, {"g", 1024}, {"mib", 1}, {"mi", 1}, {"m", 1}} {
		if strings.HasSuffix(l, suf.s) {
			l, mult = strings.TrimSuffix(l, suf.s), suf.m
			break
		}
	}
	n, err := strconv.Atoi(l)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("bad memory size %q (want MiB, or a size like 2g)", s)
	}
	return n * mult, nil
}

// config is what `loam login` stores.
type config struct {
	URL        string `json:"url,omitempty"`
	Token      string `json:"token,omitempty"`
	AdminToken string `json:"admin_token,omitempty"`
}

// configPath is $LOAM_CONFIG, or loam/config.json in the user config dir.
func (a *app) configPath() (string, error) {
	if p := a.getenv("LOAM_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "loam", "config.json"), nil
}

func (a *app) loadConfig() (config, error) {
	var c config
	p, err := a.configPath()
	if err != nil {
		return c, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", p, err)
	}
	return c, nil
}

// saveConfig writes the config 0600 (it holds keys), replacing it whole.
func (a *app) saveConfig(c config) (string, error) {
	p, err := a.configPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return "", err
	}
	// WriteFile keeps the mode of a file that exists; make sure.
	if err := os.Chmod(tmp, 0o600); err != nil {
		return "", err
	}
	return p, os.Rename(tmp, p)
}
