package db_test

import (
	"context"
	"errors"
	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/db/dbtest"
	"github.com/arugula-salad/loam/pkg/api"
	"testing"
	"time"
)

func TestE2BTemplateStore(t *testing.T) {
	for _, kind := range []string{"memory", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			var st db.Store = db.NewMemory()
			if kind == "postgres" {
				st = dbtest.Postgres(t)
			}
			ctx := context.Background()
			if err := st.CreateProject(ctx, api.Project{Name: "p", CreatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			row := db.E2BTemplate{ID: "tpl-a", Project: "p", Name: "workspace", Builds: map[string]db.E2BBuild{"b": {ID: "b", Status: "waiting"}}, Tags: map[string]string{}, Files: map[string]int64{"f": 7}}
			if err := st.CreateE2BTemplate(ctx, row); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateE2BTemplate(ctx, row); !errors.Is(err, db.ErrExists) {
				t.Fatalf("duplicate: %v", err)
			}
			row.Files["f"] = 9
			rows, err := st.E2BTemplates(ctx, "p")
			if err != nil || len(rows) != 1 || rows[0].Files["f"] != 7 {
				t.Fatalf("round trip: %+v %v", rows, err)
			}
			if rows, _ := st.E2BTemplates(ctx, "q"); len(rows) != 0 {
				t.Fatal("cross-project listing")
			}
			_, err = st.UpdateE2BTemplate(ctx, "q", "tpl-a", func(*db.E2BTemplate) error { return nil })
			if !errors.Is(err, db.ErrNotFound) {
				t.Fatal("cross-project update", err)
			}
			boom := errors.New("boom")
			_, err = st.UpdateE2BTemplate(ctx, "p", "tpl-a", func(t *db.E2BTemplate) error { t.Tags["v1"] = "b"; return boom })
			if !errors.Is(err, boom) {
				t.Fatal(err)
			}
			rows, _ = st.E2BTemplates(ctx, "p")
			if len(rows[0].Tags) != 0 {
				t.Fatal("failed update was committed")
			}
			_, err = st.UpdateE2BTemplate(ctx, "p", "tpl-a", func(t *db.E2BTemplate) error { t.Tags["v1"] = "b"; return nil })
			if err != nil {
				t.Fatal(err)
			}
			rows, _ = st.E2BTemplates(ctx, "p")
			if rows[0].Tags["v1"] != "b" {
				t.Fatal("update lost")
			}
			if err := st.DeleteE2BTemplate(ctx, "q", "tpl-a"); !errors.Is(err, db.ErrNotFound) {
				t.Fatal("cross-project delete", err)
			}
			if err := st.DeleteE2BTemplate(ctx, "p", "tpl-a"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
