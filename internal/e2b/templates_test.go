package e2b

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/loam/internal/db"
)

func TestTemplateBuildIsolationAndRecovery(t *testing.T) {
	fx := newFixture(t)
	r := fx.do("POST", "/v3/templates", fx.key, map[string]any{"name": "my-template", "tags": []string{"release"}})
	if r.Code != 202 {
		t.Fatal(r.Code, r.String())
	}
	var ids struct {
		Template string `json:"templateID"`
		Build    string `json:"buildID"`
	}
	if err := json.Unmarshal(r.Body, &ids); err != nil {
		t.Fatal(err)
	}
	status := "/templates/" + ids.Template + "/builds/" + ids.Build + "/status"
	if r = fx.do("GET", status, fx.other, nil); r.Code != 404 {
		t.Fatal("cross-project status", r.Code)
	}
	if r = fx.do("DELETE", "/templates/"+ids.Template, fx.other, nil); r.Code != 404 {
		t.Fatal("cross-project delete", r.Code)
	}
	if _, _, err := fx.f.resolveBuild(context.Background(), "p", "my-template:release"); err != db.ErrNotFound {
		t.Fatal("unbuilt tag published", err)
	}
	_, err := fx.store.UpdateE2BTemplate(context.Background(), "p", ids.Template, func(t *db.E2BTemplate) error {
		b := t.Builds[ids.Build]
		b.Status = "building"
		t.Builds[ids.Build] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	fx.clock.Advance(templateBuildTimeout + 2*time.Minute)
	r = fx.do("GET", status, fx.key, nil)
	if r.Code != 200 || !strings.Contains(r.String(), `"status":"error"`) || !strings.Contains(r.String(), "interrupted") {
		t.Fatal("lost build did not expire", r.Code, r.String())
	}
	if _, _, err = fx.f.resolveBuild(context.Background(), "p", "my-template:release"); err != db.ErrNotFound {
		t.Fatal("failed tag published", err)
	}
	if r = fx.do("DELETE", "/templates/"+ids.Template, fx.key, nil); r.Code != 204 {
		t.Fatal(r.Code, r.String())
	}
}

func TestTemplateArchiveBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind byte
		ok   bool
	}{
		{"./", tar.TypeDir, true}, {"hello.txt", tar.TypeReg, true}, {"nested/hello.txt", tar.TypeReg, true},
		{"../escape", tar.TypeReg, false}, {"/absolute", tar.TypeReg, false},
		{"nested/../../escape", tar.TypeReg, false}, {"link", tar.TypeSymlink, false},
		{"hardlink", tar.TypeLink, false}, {"device", tar.TypeChar, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			if err := tw.WriteHeader(&tar.Header{Name: tc.name, Typeflag: tc.kind, Mode: 0644}); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			_, err := archiveFiles(buf.Bytes())
			if (err == nil) != tc.ok {
				t.Fatalf("archive accepted=%v: %v", err == nil, err)
			}
		})
	}
}

func TestPinnedBuildImage(t *testing.T) {
	digest := "@sha256:" + strings.Repeat("a", 64)
	for in, want := range map[string]string{
		"ubuntu:24.04" + digest:                             "docker.io/library/ubuntu" + digest,
		"docker.io/ubuntu" + digest:                         "docker.io/library/ubuntu" + digest,
		"team/image:tag" + digest:                           "docker.io/team/image" + digest,
		"registry.example.com:5000/team/image:tag" + digest: "registry.example.com:5000/team/image" + digest,
	} {
		got, err := pinnedBuildImage(in)
		if err != nil || got != want {
			t.Errorf("%q => %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := pinnedBuildImage("ubuntu:latest"); err == nil {
		t.Fatal("mutable image accepted")
	}
}

func TestTemplateBuildPublishesOnlySuccess(t *testing.T) {
	fx := newFixture(t)
	request := func() (string, string) {
		t.Helper()
		r := fx.do("POST", "/v3/templates", fx.key, map[string]any{"name": "versioned", "tags": []string{"stable"}})
		if r.Code != 202 {
			t.Fatal(r.Code, r.String())
		}
		ids := map[string]string{}
		var raw map[string]any
		if err := json.Unmarshal(r.Body, &raw); err != nil {
			t.Fatal(err)
		}
		ids["templateID"], ids["buildID"] = raw["templateID"].(string), raw["buildID"].(string)
		return ids["templateID"], ids["buildID"]
	}
	wait := func(id, bid, status string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			row, err := fx.f.findTemplate(context.Background(), "p", id)
			if err != nil {
				t.Fatal(err)
			}
			b := row.Builds[bid]
			if b.Status == status {
				return
			}
			if b.Status == "error" && status != "error" {
				t.Fatal(b.Error)
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("build did not reach", status)
	}
	id, first := request()
	path := "/v2/templates/" + id + "/builds/"
	r := fx.do("POST", path+first, fx.key, map[string]any{"fromTemplate": "base"})
	if r.Code != 202 {
		t.Fatal(r.Code, r.String())
	}
	wait(id, first, "ready")
	_, ready, err := fx.f.resolveBuild(context.Background(), "p", "versioned:stable")
	if err != nil || ready.ID != first || ready.Checkpoint == "" {
		t.Fatal(ready, err)
	}
	if r = fx.do("POST", path+first, fx.key, map[string]any{"fromTemplate": "base"}); r.Code != 409 {
		t.Fatal("duplicate start", r.Code, r.String())
	}
	_, second := request()
	r = fx.do("POST", path+second, fx.key, map[string]any{"fromImage": "ubuntu:unpinned"})
	if r.Code != 202 {
		t.Fatal(r.Code, r.String())
	}
	wait(id, second, "error")
	_, ready, err = fx.f.resolveBuild(context.Background(), "p", "versioned:stable")
	if err != nil || ready.ID != first {
		t.Fatal("failed rebuild changed tag", ready, err)
	}
	r = fx.do("POST", "/templates/tags", fx.key, map[string]any{"target": "versioned:stable", "tags": []string{"promoted"}})
	if r.Code != 201 {
		t.Fatal(r.Code, r.String())
	}
	_, ready, err = fx.f.resolveBuild(context.Background(), "p", "versioned:promoted")
	if err != nil || ready.ID != first {
		t.Fatal("tag assignment", ready, err)
	}
	if r = fx.do("DELETE", "/templates/"+id, fx.key, nil); r.Code != 204 {
		t.Fatal(r.Code, r.String())
	}
}
