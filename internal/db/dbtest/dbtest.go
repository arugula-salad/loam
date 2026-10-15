// Package dbtest gives a test a Store: Postgres in a schema of its own when
// LOAM_TEST_DATABASE_URL is set, and Memory otherwise, so `go test ./...`
// needs no database and a run with one covers both. Use the Postgres major
// version the cluster runs (CNPG's loam-pg: 18): error codes differ between
// versions, and 17 reports a violated ON DELETE RESTRICT as a plain foreign
// key violation where 18 reports restrict_violation.
//
//	docker run --rm -d -p 127.0.0.1:55432:5432 -e POSTGRES_PASSWORD=t postgres:18
//	LOAM_TEST_DATABASE_URL='postgres://postgres:t@127.0.0.1:55432/postgres?sslmode=disable' go test ./...
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/arugula-salad/loam/internal/db"
)

// EnvURL names the Postgres the tests may use. Every test gets a fresh schema
// in it, dropped at the end.
const EnvURL = "LOAM_TEST_DATABASE_URL"

// Postgres is a Store in a new schema of the database at $LOAM_TEST_DATABASE_URL,
// or a skip when it isn't set.
func Postgres(t testing.TB) db.Store {
	t.Helper()
	url := os.Getenv(EnvURL)
	if url == "" {
		t.Skip(EnvURL + " not set")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	st, err := db.Open(ctx, url, map[string]string{"search_path": schema})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		st.Close()
		if c, err := pgx.Connect(ctx, url); err == nil {
			c.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
			c.Close(ctx)
		}
	})
	return st
}

// Store is Postgres when the env var is set, else Memory.
func Store(t testing.TB) db.Store {
	t.Helper()
	if os.Getenv(EnvURL) != "" {
		return Postgres(t)
	}
	return db.NewMemory()
}
