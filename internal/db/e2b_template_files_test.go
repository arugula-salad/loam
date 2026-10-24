package db_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/db/dbtest"
	"github.com/arugula-salad/loam/pkg/api"
)

type brokenUpload struct{}

func (brokenUpload) Read([]byte) (int, error) { return 0, errors.New("upload interrupted") }

func TestTemplateFileStorage(t *testing.T) {
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
			if err := st.CreateE2BTemplate(ctx, db.E2BTemplate{ID: "tpl", Project: "p", Name: "files"}); err != nil {
				t.Fatal(err)
			}
			size := 3 << 20
			count := 1
			if kind == "postgres" && os.Getenv("LOAM_TEST_LARGE_UPLOADS") != "" {
				size = 64 << 20
				count = 4
			}
			payload := make([]byte, size)
			rand.Read(payload)
			want := sha256.Sum256(payload)
			for n := 0; n < count; n++ {
				hash := string(rune('a' + n))
				if err := st.PutE2BTemplateFile(ctx, "p", "tpl", hash, bytes.NewReader(payload)); err != nil {
					t.Fatal(err)
				}
				r, err := st.OpenE2BTemplateFile(ctx, "p", "tpl", hash)
				if err != nil {
					t.Fatal(err)
				}
				h := sha256.New()
				_, err = io.Copy(h, r)
				r.Close()
				if err != nil || !bytes.Equal(h.Sum(nil), want[:]) {
					t.Fatal("file differs", err)
				}
			}
			if err := st.PutE2BTemplateFile(ctx, "q", "tpl", "a", bytes.NewReader(payload)); !errors.Is(err, db.ErrNotFound) {
				t.Fatal("cross-project write", err)
			}
			if _, err := st.OpenE2BTemplateFile(ctx, "q", "tpl", "a"); !errors.Is(err, db.ErrNotFound) {
				t.Fatal("cross-project read", err)
			}
			if err := st.PutE2BTemplateFile(ctx, "p", "tpl", "a", io.MultiReader(bytes.NewReader(payload[:2<<20]), brokenUpload{})); err == nil {
				t.Fatal("accepted truncated upload")
			}
			r, err := st.OpenE2BTemplateFile(ctx, "p", "tpl", "a")
			if err != nil {
				t.Fatal(err)
			}
			h := sha256.New()
			io.Copy(h, r)
			r.Close()
			if !bytes.Equal(h.Sum(nil), want[:]) {
				t.Fatal("failed upload replaced old file")
			}
			for n := 0; n < 30; n++ {
				row, err := st.UpdateE2BTemplate(ctx, "p", "tpl", func(t *db.E2BTemplate) error { t.Tags = map[string]string{"v1": "build"}; return nil })
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(row)
				if len(raw) > 1024 {
					t.Fatalf("archive leaked into metadata: %d bytes", len(raw))
				}
			}

			_, err = st.UpdateE2BTemplate(ctx, "p", "tpl", func(t *db.E2BTemplate) error {
				var used int64
				for _, n := range t.Files {
					used += n
				}
				t.Files["quota-reservation"] = (256 << 20) - used
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err = st.PutE2BTemplateFile(ctx, "p", "tpl", "overflow", bytes.NewReader([]byte{1})); err == nil {
				t.Fatal("retained upload quota not enforced")
			}
			if _, err = st.OpenE2BTemplateFile(ctx, "p", "tpl", "overflow"); !errors.Is(err, db.ErrNotFound) {
				t.Fatal("quota failure retained chunks", err)
			}
			if err := st.DeleteE2BTemplate(ctx, "p", "tpl"); err != nil {
				t.Fatal(err)
			}
			if _, err := st.OpenE2BTemplateFile(ctx, "p", "tpl", "a"); !errors.Is(err, db.ErrNotFound) {
				t.Fatal("deleted file remains", err)
			}
		})
	}
}
