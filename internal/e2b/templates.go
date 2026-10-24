package e2b

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/pkg/api"
)

var templateNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

func buildID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func (f *Frontend) templateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /templates/{template}/files/{hash}", f.auth(f.buildFile))
	mux.HandleFunc("PUT /templates/{template}/files/{hash}", f.auth(f.buildFile))
	mux.HandleFunc("POST /v3/templates", f.auth(f.requestBuild))
	mux.HandleFunc("POST /v2/templates/{template}/builds/{build}", f.auth(f.startBuild))
	mux.HandleFunc("GET /templates/{template}/builds/{build}/status", f.auth(f.buildStatus))
	mux.HandleFunc("GET /templates/{template}/builds/{build}/logs", f.auth(f.buildLogs))
	mux.HandleFunc("GET /templates/{template}", f.auth(f.getTemplate))
	mux.HandleFunc("DELETE /templates/{template}", f.auth(f.deleteTemplate))
	mux.HandleFunc("GET /templates/{template}/{action}", f.auth(func(w http.ResponseWriter, r *http.Request, p string) {
		if r.PathValue("template") == "aliases" {
			r.SetPathValue("alias", r.PathValue("action"))
			f.templateAlias(w, r, p)
			return
		}
		if r.PathValue("action") == "tags" {
			f.templateTags(w, r, p)
			return
		}
		writeErr(w, 404, "template route not found")
	}))
	mux.HandleFunc("POST /templates/tags", f.auth(f.assignTags))
}

func (f *Frontend) findTemplate(ctx context.Context, project, ref string) (db.E2BTemplate, error) {
	all, err := f.store.E2BTemplates(ctx, project)
	if err != nil {
		return db.E2BTemplate{}, err
	}
	for _, t := range all {
		if t.ID == ref || t.Name == ref {
			return f.expireBuilds(ctx, t)
		}
	}
	return db.E2BTemplate{}, db.ErrNotFound
}

// A crashed API cannot resume a build command safely. The source sandbox has
// its own deletion deadline; expire the durable record after that deadline and
// a cleanup grace period so clients do not poll a lost worker indefinitely.
func (f *Frontend) expireBuilds(ctx context.Context, t db.E2BTemplate) (db.E2BTemplate, error) {
	stale := false
	for _, b := range t.Builds {
		if b.Status == "building" && f.now().Sub(b.UpdatedAt) > templateBuildTimeout+time.Minute {
			stale = true
		}
	}
	if !stale {
		return t, nil
	}
	return f.store.UpdateE2BTemplate(ctx, t.Project, t.ID, func(t *db.E2BTemplate) error {
		for id, b := range t.Builds {
			if b.Status == "building" && f.now().Sub(b.UpdatedAt) > templateBuildTimeout+time.Minute {
				b.Status = "error"
				b.Error = "build worker interrupted or exceeded its time limit; submit a new build"
				b.UpdatedAt = f.now()
				b.FinishedAt = &b.UpdatedAt
				t.Builds[id] = b
			}
		}
		return nil
	})
}

func (f *Frontend) resolveBuild(ctx context.Context, project, ref string) (db.E2BTemplate, db.E2BBuild, error) {
	name, tag, _ := strings.Cut(ref, ":")
	if tag == "" {
		tag = "default"
	}
	t, err := f.findTemplate(ctx, project, name)
	if err != nil {
		return t, db.E2BBuild{}, err
	}
	b, ok := t.Builds[t.Tags[tag]]
	if !ok || b.Status != "ready" {
		return t, b, db.ErrNotFound
	}
	return t, b, nil
}
func (f *Frontend) templateError(w http.ResponseWriter, err error) {
	if errors.Is(err, db.ErrNotFound) {
		writeErr(w, 404, "template or build not found")
		return
	}
	if errors.Is(err, db.ErrExists) {
		writeErr(w, 409, "template or build already exists")
		return
	}
	f.fail(w, "", err)
}

func (f *Frontend) requestBuild(w http.ResponseWriter, r *http.Request, project string) {
	var req struct {
		Name   string   `json:"name"`
		Alias  string   `json:"alias"`
		Tags   []string `json:"tags"`
		CPU    int      `json:"cpuCount"`
		Memory int      `json:"memoryMB"`
		Disk   int      `json:"minFreeDiskMb"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if req.Name == "" {
		req.Name = req.Alias
	}
	if req.Name == "" {
		req.Name = "template-" + buildID()
	}
	name, tag, _ := strings.Cut(req.Name, ":")
	if !templateNameRE.MatchString(name) {
		writeErr(w, 400, "invalid template name")
		return
	}
	if _, _, static := f.templateOf(name); static {
		writeErr(w, 409, "operator templates cannot be rebuilt")
		return
	}
	if tag != "" {
		req.Tags = append(req.Tags, tag)
	}
	if len(req.Tags) == 0 {
		req.Tags = []string{"default"}
	}
	for _, tag := range req.Tags {
		if !templateNameRE.MatchString(tag) {
			writeErr(w, 400, "invalid template tag")
			return
		}
	}
	if req.Disk != 0 {
		writeErr(w, 400, "minFreeDiskMb: explicit free-disk growth is not supported")
		return
	}
	if req.CPU == 0 {
		req.CPU = 2
	}
	if req.Memory == 0 {
		req.Memory = 1024
	}
	if req.CPU < 1 || req.CPU > 32 || req.Memory < 128 || req.Memory > 256<<10 || req.Disk < 0 || req.Disk > 1024*1024 {
		writeErr(w, 400, "invalid template resources")
		return
	}
	defer lock(&f.locks, "template-name:"+project+":"+name)()
	t, err := f.findTemplate(r.Context(), project, name)
	if errors.Is(err, db.ErrNotFound) {
		t = db.E2BTemplate{ID: "tpl-" + buildID(), Name: name, Project: project, CreatedAt: f.now(), Builds: map[string]db.E2BBuild{}, Tags: map[string]string{}}
		err = f.store.CreateE2BTemplate(r.Context(), t)
	}
	if err != nil {
		f.templateError(w, err)
		return
	}
	now := f.now()
	b := db.E2BBuild{ID: buildID(), Status: "waiting", Resources: api.Resources{CPUs: req.CPU, MemoryMiB: req.Memory, DiskGiB: 20 + (req.Disk+1023)/1024}, Tags: req.Tags, CreatedAt: now, UpdatedAt: now}
	_, err = f.store.UpdateE2BTemplate(r.Context(), project, t.ID, func(t *db.E2BTemplate) error { t.Builds[b.ID] = b; return nil })
	if err != nil {
		f.templateError(w, err)
		return
	}
	writeJSON(w, 202, map[string]any{"templateID": t.ID, "buildID": b.ID, "names": []string{name}, "aliases": []string{name}, "tags": req.Tags, "public": false})
}

func (f *Frontend) requestedBuild(w http.ResponseWriter, r *http.Request, project string) (db.E2BTemplate, db.E2BBuild, bool) {
	t, err := f.findTemplate(r.Context(), project, r.PathValue("template"))
	if err != nil {
		f.templateError(w, err)
		return t, db.E2BBuild{}, false
	}
	b, ok := t.Builds[r.PathValue("build")]
	if !ok {
		f.templateError(w, db.ErrNotFound)
	}
	return t, b, ok
}
func (f *Frontend) buildStatus(w http.ResponseWriter, r *http.Request, project string) {
	t, b, ok := f.requestedBuild(w, r, project)
	if !ok {
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("logsOffset"))
	if offset < 0 || offset > len(b.Logs) {
		writeErr(w, 400, "invalid logsOffset")
		return
	}
	entries := append([]db.E2BBuildLog{}, b.Logs[offset:]...)
	out := map[string]any{"templateID": t.ID, "buildID": b.ID, "status": b.Status, "logs": []string{}, "logEntries": entries}
	if b.Error != "" {
		out["reason"] = map[string]any{"message": b.Error}
	}
	writeJSON(w, 200, out)
}
func (f *Frontend) buildLogs(w http.ResponseWriter, r *http.Request, project string) {
	_, b, ok := f.requestedBuild(w, r, project)
	if !ok {
		return
	}
	logs := []any{}
	if b.Error != "" {
		logs = append(logs, map[string]any{"timestamp": b.UpdatedAt, "level": "error", "message": b.Error})
	}
	writeJSON(w, 200, map[string]any{"logs": logs})
}
func (f *Frontend) getTemplate(w http.ResponseWriter, r *http.Request, project string) {
	t, err := f.findTemplate(r.Context(), project, r.PathValue("template"))
	if err != nil {
		f.templateError(w, err)
		return
	}
	builds := []any{}
	for _, b := range t.Builds {
		builds = append(builds, map[string]any{"buildID": b.ID, "status": b.Status, "createdAt": b.CreatedAt, "updatedAt": b.UpdatedAt, "finishedAt": b.FinishedAt, "cpuCount": b.Resources.CPUs, "memoryMB": b.Resources.MemoryMiB, "diskSizeMB": b.Resources.DiskGiB * 1024, "envdVersion": EnvdVersion})
	}
	writeJSON(w, 200, map[string]any{"templateID": t.ID, "names": []string{t.Name}, "aliases": []string{t.Name}, "public": false, "builds": builds})
}
func (f *Frontend) templateTags(w http.ResponseWriter, r *http.Request, project string) {
	t, err := f.findTemplate(r.Context(), project, r.PathValue("template"))
	if err != nil {
		f.templateError(w, err)
		return
	}
	out := []any{}
	for tag, id := range t.Tags {
		out = append(out, map[string]any{"tag": tag, "buildID": id, "createdAt": t.Builds[id].UpdatedAt})
	}
	writeJSON(w, 200, out)
}
func (f *Frontend) assignTags(w http.ResponseWriter, r *http.Request, project string) {
	var req struct {
		Target string   `json:"target"`
		Tags   []string `json:"tags"`
	}
	if !readBody(w, r, &req) {
		return
	}
	for _, tag := range req.Tags {
		if !templateNameRE.MatchString(tag) {
			writeErr(w, 400, "invalid template tag")
			return
		}
	}
	t, build, err := f.resolveBuild(r.Context(), project, req.Target)
	if err != nil {
		f.templateError(w, err)
		return
	}
	_, err = f.store.UpdateE2BTemplate(r.Context(), project, t.ID, func(t *db.E2BTemplate) error {
		b, ok := t.Builds[build.ID]
		if !ok || b.Status != "ready" {
			return db.ErrNotFound
		}
		for _, tag := range req.Tags {
			t.Tags[tag] = b.ID
		}
		return nil
	})
	if err != nil {
		f.templateError(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"templateID": t.ID, "buildID": build.ID, "tags": req.Tags})
}
func (f *Frontend) deleteTemplate(w http.ResponseWriter, r *http.Request, project string) {
	t, err := f.findTemplate(r.Context(), project, r.PathValue("template"))
	if err != nil {
		f.templateError(w, err)
		return
	}
	defer lock(&f.locks, "template:"+t.ID)()
	t, err = f.findTemplate(r.Context(), project, t.ID)
	if err != nil {
		f.templateError(w, err)
		return
	}
	for _, b := range t.Builds {
		if b.Status == "building" {
			writeErr(w, 409, "template has a running build")
			return
		}
	}
	for _, b := range t.Builds {
		if b.Sandbox != "" {
			if err := f.call(r.Context(), project, "PATCH", "/v1/sandboxes/"+b.Sandbox, map[string]any{"protected": false}, nil); err != nil && codeOf(err) != api.CodeNotFound {
				f.fail(w, b.Sandbox, err)
				return
			}
			if err := f.call(r.Context(), project, "DELETE", "/v1/sandboxes/"+b.Sandbox, nil, nil); err != nil && codeOf(err) != api.CodeNotFound {
				f.fail(w, b.Sandbox, err)
				return
			}
		}
	}
	if err = f.store.DeleteE2BTemplate(r.Context(), project, t.ID); err != nil {
		f.templateError(w, err)
		return
	}
	w.WriteHeader(204)
}

type templateStep struct {
	Type      string   `json:"type"`
	Args      []string `json:"args"`
	FilesHash string   `json:"filesHash"`
	Force     bool     `json:"force"`
}
type buildRequest struct {
	FromImage    string         `json:"fromImage"`
	FromTemplate string         `json:"fromTemplate"`
	Registry     any            `json:"fromImageRegistry"`
	Start        string         `json:"startCmd"`
	Ready        string         `json:"readyCmd"`
	Steps        []templateStep `json:"steps"`
	Force        bool           `json:"force"`
}

func (f *Frontend) startBuild(w http.ResponseWriter, r *http.Request, project string) {
	t, b, ok := f.requestedBuild(w, r, project)
	if !ok {
		return
	}
	var req buildRequest
	if !readBody(w, r, &req) {
		return
	}
	if (req.FromImage == "") == (req.FromTemplate == "") {
		writeErr(w, 400, "exactly one of fromImage or fromTemplate is required")
		return
	}
	if req.Registry != nil {
		writeErr(w, 400, "fromImageRegistry: configure registry credentials on the nodelet")
		return
	}
	for _, step := range req.Steps {
		if !slices.Contains([]string{"RUN", "ENV", "USER", "WORKDIR", "COPY"}, step.Type) || len(step.Args) == 0 {
			writeErr(w, 400, fmt.Sprintf("unsupported or empty build step %s", step.Type))
			return
		}
	}
	defer lock(&f.locks, "template:"+t.ID)()
	updated, err := f.store.UpdateE2BTemplate(r.Context(), project, t.ID, func(t *db.E2BTemplate) error {
		cur := t.Builds[b.ID]
		if cur.Status != "waiting" {
			return db.ErrExists
		}
		cur.Status = "building"
		cur.UpdatedAt = f.now()
		t.Builds[b.ID] = cur
		return nil
	})
	if err != nil {
		f.templateError(w, err)
		return
	}
	go f.runBuild(updated, updated.Builds[b.ID], req)
	w.WriteHeader(202)
}

const templateBuildTimeout = 20 * time.Minute

func (f *Frontend) templateAlias(w http.ResponseWriter, r *http.Request, project string) {
	t, err := f.findTemplate(r.Context(), project, r.PathValue("alias"))
	if err != nil {
		f.templateError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"templateID": t.ID, "alias": t.Name})
}
