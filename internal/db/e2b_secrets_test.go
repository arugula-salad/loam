package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/db/dbtest"
	"github.com/arugula-salad/loam/pkg/api"
)

func TestMemoryE2BSecrets(t *testing.T)   { e2bSecrets(t, db.NewMemory()) }
func TestPostgresE2BSecrets(t *testing.T) { e2bSecrets(t, dbtest.Postgres(t)) }
func e2bSecrets(t *testing.T, st db.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, name := range []string{"p", "q"} {
		if err := st.CreateProject(ctx, api.Project{Name: name, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	rec := db.E2BSecret{ID: "sec-one", Project: "p", Name: "key", CreatedAt: now, UpdatedAt: now, Sealed: []byte("ciphertext"), Metadata: map[string]string{"a": "b"}}
	got, err := st.PutE2BSecret(ctx, rec, true)
	if err != nil || got.Version != 1 {
		t.Fatal(got, err)
	}
	rec.Sealed[0] = 'X'
	rec.Metadata["a"] = "changed"
	all, err := st.E2BSecrets(ctx, "p")
	if err != nil || len(all) != 1 || string(all[0].Sealed) != "ciphertext" || all[0].Metadata["a"] != "b" {
		t.Fatal("not a deep copy", all, err)
	}
	duplicate := got
	duplicate.ID = "sec-two"
	if _, err := st.PutE2BSecret(ctx, duplicate, true); !errors.Is(err, db.ErrExists) {
		t.Fatal("duplicate name", err)
	}
	other := got
	other.Project = "q"
	if _, err := st.PutE2BSecret(ctx, other, false); !errors.Is(err, db.ErrNotFound) {
		t.Fatal("cross-project mutation", err)
	}
	if err := st.DeleteE2BSecret(ctx, "q", got.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatal("cross-project deletion", err)
	}
	got.Metadata = nil
	got.Sealed = []byte("rotated")
	got, err = st.PutE2BSecret(ctx, got, false)
	if err != nil || got.Version != 2 || got.Metadata["a"] != "b" {
		t.Fatal("rotation", got, err)
	}
	if err := st.DeleteProject(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	all, err = st.E2BSecrets(ctx, "p")
	if err != nil || len(all) != 0 {
		t.Fatal("project deletion retained secrets", all, err)
	}
}
