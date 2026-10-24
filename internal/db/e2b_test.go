package db_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/db/dbtest"
	"github.com/arugula-salad/loam/pkg/api"
)

func TestMemoryE2B(t *testing.T)   { e2bConformance(t, db.NewMemory()) }
func TestPostgresE2B(t *testing.T) { e2bConformance(t, dbtest.Postgres(t)) }

func e2bConformance(t *testing.T, st db.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	is := func(err, want error) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Fatalf("got %v, want %v", err, want)
		}
	}
	must(st.CreateProject(ctx, api.Project{Name: "p", CreatedAt: now}))
	must(st.CreateProject(ctx, api.Project{Name: "q", CreatedAt: now}))
	for _, s := range []struct{ id, project string }{{"sb-a", "p"}, {"sb-b", "p"}, {"sb-c", "q"}} {
		must(st.CreateSandbox(ctx, db.Sandbox{ID: s.id, Name: s.id, Project: s.project, Template: "e2b", State: api.StateRunning,
			Idle: api.Idle{TimeoutS: 300, Action: api.ActionNone}, HTTPPort: 8080, CreatedAt: now, UpdatedAt: now, LastActiveAt: now}))
	}
	row := func(id, project string) db.E2BSandbox {
		return db.E2BSandbox{Sandbox: id, Project: project, Template: "base", SealedToken: []byte("sealed-" + id), TimeoutS: 300,
			StartedAt: now, EndAt: now.Add(5 * time.Minute), KeepMemory: true, PrivateTraffic: id == "sb-a"}
	}

	// Only for a sandbox that exists, and once.
	is(st.CreateE2B(ctx, row("sb-nope", "p")), db.ErrNotFound)
	must(st.CreateE2B(ctx, row("sb-a", "p")))
	is(st.CreateE2B(ctx, row("sb-a", "p")), db.ErrExists)
	must(st.CreateE2B(ctx, row("sb-c", "q")))
	_, err := st.E2B(ctx, "sb-b")
	is(err, db.ErrNotFound)

	got, err := st.E2B(ctx, "sb-a")
	must(err)
	if !reflect.DeepEqual(got, row("sb-a", "p")) {
		t.Fatalf("got %+v", got)
	}
	all, err := st.E2Bs(ctx, "")
	must(err)
	mine, err := st.E2Bs(ctx, "p")
	must(err)
	if len(all) != 2 || all[0].Sandbox != "sb-a" || all[1].Sandbox != "sb-c" || len(mine) != 1 || mine[0].Sandbox != "sb-a" {
		t.Fatalf("all %+v, p's %+v", all, mine)
	}

	later := now.Add(time.Hour)
	got, err = st.UpdateE2B(ctx, "sb-a", func(e *db.E2BSandbox) error {
		e.Paused, e.EndAt, e.Sandbox = true, later, "sb-other"
		return nil
	})
	must(err)
	if !got.Paused || !got.EndAt.Equal(later) || got.Sandbox != "sb-a" {
		t.Fatalf("update: %+v", got)
	}
	boom := errors.New("boom")
	_, err = st.UpdateE2B(ctx, "sb-a", func(e *db.E2BSandbox) error { e.Paused = false; return boom })
	is(err, boom)
	if got, _ := st.E2B(ctx, "sb-a"); !got.Paused {
		t.Fatal("a failed update was kept")
	}
	_, err = st.UpdateE2B(ctx, "sb-b", func(*db.E2BSandbox) error { return nil })
	is(err, db.ErrNotFound)

	// The row goes with its sandbox.
	must(st.DeleteSandbox(ctx, "sb-a"))
	_, err = st.E2B(ctx, "sb-a")
	is(err, db.ErrNotFound)
}
