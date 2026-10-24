package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/arugula-salad/loam/internal/apiserver"
	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/pkg/api"
)

// Builds run inside ordinary project sandboxes. No user command or Dockerfile
// instruction runs in loam-api or on a nodelet's host filesystem.
func (f *Frontend) runBuild(t db.E2BTemplate, b db.E2BBuild, req buildRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), templateBuildTimeout)
	defer cancel()
	var source string
	err := func() error {
		create := api.CreateSandbox{Resources: &b.Resources, Idle: &api.Idle{Action: api.ActionNone}, Deadline: &api.Deadline{At: f.now().Add(templateBuildTimeout), Action: api.ActionDelete}, Labels: map[string]string{"e2b-build": b.ID}}
		if req.FromImage != "" {
			image, err := pinnedBuildImage(req.FromImage)
			if err != nil {
				return err
			}
			create.Image = image
		} else if _, native, ok := f.templateOf(req.FromTemplate); ok {
			create.Template = native
		} else {
			_, parent, err := f.resolveBuild(ctx, t.Project, req.FromTemplate)
			if err != nil {
				return err
			}
			if b.Resources != parent.Resources {
				return fmt.Errorf("a build from a snapshot must use its source resources")
			}
			create.Resources = nil
			create.From = &api.From{Sandbox: parent.Sandbox, Checkpoint: parent.Checkpoint}
			create.Env = parent.Env
		}
		var sb api.Sandbox
		if err := f.call(ctx, t.Project, "POST", "/v1/sandboxes", create, &sb); err != nil {
			return err
		}
		source = sb.ID
		if _, err := f.store.UpdateE2BTemplate(ctx, t.Project, t.ID, func(t *db.E2BTemplate) error {
			cur := t.Builds[b.ID]
			cur.Sandbox = source
			t.Builds[b.ID] = cur
			return nil
		}); err != nil {
			return err
		}
		env := sb.Env
		if env == nil {
			env = map[string]string{}
		}
		user, cwd := "root", "/"
		run := func(command, as string) error {
			var out api.ExecResult
			err := f.call(ctx, t.Project, "POST", "/v1/sandboxes/"+source+"/exec/run", api.ExecRun{Cmd: []string{"/bin/sh", "-c", command}, User: as, Cwd: cwd, Env: env, TimeoutS: 600}, &out)
			if err != nil {
				return err
			}
			f.appendBuildLog(ctx, t, b.ID, string(out.Stdout)+string(out.Stderr))
			if out.ExitCode != 0 || out.TimedOut {
				return fmt.Errorf("build command failed (exit %d): %.4096s", out.ExitCode, out.Stderr)
			}
			return nil
		}
		for i, step := range req.Steps {
			f.appendBuildLog(ctx, t, b.ID, fmt.Sprintf("Step %d: %s", i+1, step.Type))
			switch step.Type {
			case "RUN":
				as := user
				if len(step.Args) > 1 && step.Args[1] != "" {
					as = step.Args[1]
				}
				if err := run(step.Args[0], as); err != nil {
					return err
				}
			case "COPY":
				if err := f.copyBuildFiles(ctx, t.Project, source, cwd, step, t.Files); err != nil {
					return err
				}
			case "USER":
				user = step.Args[0]
			case "WORKDIR":
				cwd = step.Args[0]
				if !strings.HasPrefix(cwd, "/") {
					return fmt.Errorf("WORKDIR must be absolute")
				}
				saved := cwd
				cwd = "/"
				err := run("mkdir -p -- "+shellQuote(saved), "root")
				cwd = saved
				if err != nil {
					return err
				}
			case "ENV":
				if len(step.Args)%2 != 0 {
					return fmt.Errorf("ENV needs name/value pairs")
				}
				values := map[string]string{}
				for i := 0; i < len(step.Args); i += 2 {
					// Match E2B's ENV interpolation inside the guest. Escape
					// command substitutions while preserving shell variables.
					escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", "$(", "\\$(").Replace(step.Args[i+1])
					var out api.ExecResult
					if err := f.call(ctx, t.Project, "POST", "/v1/sandboxes/"+source+"/exec/run", api.ExecRun{Cmd: []string{"/bin/sh", "-c", `printf '%s' "` + escaped + `"`}, User: "root", Env: env, TimeoutS: 5}, &out); err != nil {
						return err
					}
					if out.ExitCode != 0 || out.TimedOut {
						return fmt.Errorf("cannot evaluate ENV %s", step.Args[i])
					}
					values[step.Args[i]] = string(out.Stdout)
				}
				for k, v := range values {
					env[k] = v
				}
			}
		}
		if err := f.call(ctx, t.Project, "PATCH", "/v1/sandboxes/"+source, map[string]any{"env": env}, nil); err != nil {
			return err
		}
		if req.FromImage != "" {
			if err := f.installEnvd(ctx, t.Project, source); err != nil {
				return err
			}
		}
		if req.Start != "" {
			service := api.ServiceSpec{Cmd: []string{"/bin/sh", "-c", req.Start}, User: user, Cwd: cwd, Env: env}
			if err := f.call(ctx, t.Project, "PUT", "/v1/sandboxes/"+source+"/services/e2b-template-start", service, nil); err != nil {
				return err
			}
			if err := f.call(ctx, t.Project, "POST", "/v1/sandboxes/"+source+"/services/e2b-template-start/start", nil, nil); err != nil {
				return err
			}
		}
		if req.Ready != "" {
			readyCtx, stop := context.WithTimeout(ctx, 2*time.Minute)
			defer stop()
			for {
				var out api.ExecResult
				err := f.call(readyCtx, t.Project, "POST", "/v1/sandboxes/"+source+"/exec/run", api.ExecRun{Cmd: []string{"/bin/sh", "-c", req.Ready}, User: user, Cwd: cwd, Env: env, TimeoutS: 5}, &out)
				if err == nil && out.ExitCode == 0 && !out.TimedOut {
					break
				}
				select {
				case <-readyCtx.Done():
					return fmt.Errorf("template ready check did not pass within two minutes")
				case <-time.After(250 * time.Millisecond):
				}
			}
		}
		var cp api.Checkpoint
		if err := f.call(ctx, t.Project, "POST", "/v1/sandboxes/"+source+"/checkpoints", api.CreateCheckpoint{Comment: "E2B template build " + b.ID}, &cp); err != nil {
			return err
		}
		if err := f.call(ctx, t.Project, "POST", "/v1/sandboxes/"+source+"/pause", nil, nil); err != nil {
			return err
		}
		if err := f.call(ctx, t.Project, "PATCH", "/v1/sandboxes/"+source, map[string]any{"deadline": nil, "protected": true}, nil); err != nil {
			return err
		}
		now := f.now()
		_, err := f.store.UpdateE2BTemplate(ctx, t.Project, t.ID, func(t *db.E2BTemplate) error {
			cur := t.Builds[b.ID]
			cur.Status = "ready"
			cur.Checkpoint = cp.ID
			cur.Env = env
			cur.User, cur.Workdir = user, cwd
			cur.UpdatedAt = now
			cur.FinishedAt = &now
			t.Builds[b.ID] = cur
			for _, tag := range b.Tags {
				t.Tags[tag] = b.ID
			}
			return nil
		})
		return err
	}()
	if err == nil {
		return
	}
	cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
	defer stop()
	if source != "" {
		f.call(cleanup, t.Project, "PATCH", "/v1/sandboxes/"+source, map[string]any{"protected": false}, nil)
		if e := f.call(cleanup, t.Project, "DELETE", "/v1/sandboxes/"+source, nil, nil); e != nil {
			f.log.Error("failed to delete a failed build sandbox", "sandbox", source, "err", e)
		}
	}
	_, saveErr := f.store.UpdateE2BTemplate(cleanup, t.Project, t.ID, func(t *db.E2BTemplate) error {
		cur := t.Builds[b.ID]
		cur.Status = "error"
		cur.Error = err.Error()
		cur.UpdatedAt = f.now()
		cur.FinishedAt = &cur.UpdatedAt
		t.Builds[b.ID] = cur
		return nil
	})
	if saveErr != nil {
		f.log.Error("failed to record template build failure", "template", t.ID, "build", b.ID, "err", saveErr)
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// installEnvd takes the operator's pinned envd from a temporary base sandbox,
// never from a user-supplied URL. Registry pulls and binary extraction stay
// inside the nodelet's existing image and filesystem boundaries.
func (f *Frontend) installEnvd(ctx context.Context, project, target string) error {
	var donor api.Sandbox
	if err := f.call(ctx, project, "POST", "/v1/sandboxes", api.CreateSandbox{Template: f.opts.Template, Deadline: &api.Deadline{At: f.now().Add(templateBuildTimeout), Action: api.ActionDelete}}, &donor); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := f.call(cleanup, project, "DELETE", "/v1/sandboxes/"+donor.ID, nil, nil); err != nil {
			f.log.Error("failed to delete envd donor", "sandbox", donor.ID, "err", err)
		}
	}()
	binary, err := f.rawCall(ctx, project, "GET", "/v1/sandboxes/"+donor.ID+"/fs/file?path=/usr/bin/envd", nil)
	if err != nil {
		return err
	}
	if len(binary) == 0 || len(binary) > 64<<20 {
		return fmt.Errorf("the operator template has an invalid envd binary")
	}
	if _, err = f.rawCall(ctx, project, "PUT", "/v1/sandboxes/"+target+"/fs/file?path="+url.QueryEscape("/usr/bin/envd")+"&mode=0755&mkdir=true", binary); err != nil {
		return err
	}
	var out api.ExecResult
	// Debian/Ubuntu images provide useradd; Alpine images provide adduser.
	command := "id user >/dev/null 2>&1 || { if command -v useradd >/dev/null; then useradd -m -s /bin/sh user; else adduser -D -s /bin/sh user; fi; }"
	if err = f.call(ctx, project, "POST", "/v1/sandboxes/"+target+"/exec/run", api.ExecRun{Cmd: []string{"/bin/sh", "-c", command}, User: "root"}, &out); err != nil {
		return err
	}
	if out.ExitCode != 0 {
		return fmt.Errorf("cannot create E2B default user in image")
	}
	spec := api.ServiceSpec{Cmd: []string{"/usr/bin/envd", "-isnotfc", "-port", "49983", "-no-cgroups"}, User: "root"}
	if err = f.call(ctx, project, "PUT", "/v1/sandboxes/"+target+"/services/envd", spec, nil); err != nil {
		return err
	}
	return f.call(ctx, project, "POST", "/v1/sandboxes/"+target+"/services/envd/start", nil, nil)
}
func (f *Frontend) rawCall(ctx context.Context, project, method, path string, body []byte) ([]byte, error) {
	ctx, err := f.loam.AsProject(ctx, project)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://loam-api"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	rec := &recorder{header: http.Header{}}
	f.loam.ServeHTTP(rec, req)
	if rec.status >= 300 {
		var e api.Error
		if json.Unmarshal(rec.body.Bytes(), &e) != nil {
			return nil, fmt.Errorf("loam answered %d", rec.status)
		}
		return nil, &apiserver.Error{Status: rec.status, Code: e.Error.Code, Message: e.Error.Message}
	}
	return rec.body.Bytes(), nil
}

// A Dockerfile may carry a tag beside its digest or use Docker Hub shorthand.
// Normalize those without resolving a mutable tag or bypassing registry policy.
func pinnedBuildImage(ref string) (string, error) {
	repo, digest, ok := strings.Cut(ref, "@")
	if !ok || !strings.HasPrefix(digest, "sha256:") {
		return "", fmt.Errorf("fromImage must pin a sha256 digest")
	}
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	first, _, slash := strings.Cut(repo, "/")
	if !slash {
		repo = "docker.io/library/" + repo
	} else if !strings.ContainsAny(first, ".:") && first != "localhost" {
		repo = "docker.io/" + repo
	}
	repo = strings.Replace(repo, "index.docker.io/", "docker.io/", 1)
	if strings.HasPrefix(repo, "docker.io/") && strings.Count(repo, "/") == 1 {
		repo = strings.Replace(repo, "docker.io/", "docker.io/library/", 1)
	}
	return repo + "@" + digest, nil
}

// Bound retained output per build; logs are project scoped like its source.
func (f *Frontend) appendBuildLog(ctx context.Context, t db.E2BTemplate, id, message string) {
	if message == "" {
		return
	}
	_, err := f.store.UpdateE2BTemplate(ctx, t.Project, t.ID, func(t *db.E2BTemplate) error {
		b := t.Builds[id]
		size := 0
		for _, entry := range b.Logs {
			size += len(entry.Message)
		}
		if size >= 64<<10 {
			return nil
		}
		if len(message) > (64<<10)-size {
			message = message[:(64<<10)-size]
		}
		b.Logs = append(b.Logs, db.E2BBuildLog{Timestamp: f.now(), Level: "info", Message: message})
		t.Builds[id] = b
		return nil
	})
	if err != nil {
		f.log.Error("failed to save build log", "template", t.ID, "build", id, "err", err)
	}
}
