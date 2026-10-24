package db

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
)

const templateFileLimit = 64 << 20
const templateFilesLimit = 256 << 20
const templateChunkSize = 1 << 20

func retainTemplateFile(t *E2BTemplate, hash string, size int64) error {
	total := size
	for key, n := range t.Files {
		if key != hash {
			total += n
		}
	}
	if size > templateFileLimit || total > templateFilesLimit {
		return fmt.Errorf("template upload exceeds storage limits")
	}
	if t.Files == nil {
		t.Files = map[string]int64{}
	}
	t.Files[hash] = size
	return nil
}

func (m *Memory) PutE2BTemplateFile(ctx context.Context, project, id, hash string, r io.Reader) error {
	raw, err := io.ReadAll(io.LimitReader(r, templateFileLimit+1))
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.e2bTemplates[id]
	if !ok || t.Project != project {
		return ErrNotFound
	}
	t = t.Clone()
	if err = retainTemplateFile(&t, hash, int64(len(raw))); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if m.e2bTemplateFiles == nil {
		m.e2bTemplateFiles = map[string]map[string][]byte{}
	}
	if m.e2bTemplateFiles[id] == nil {
		m.e2bTemplateFiles[id] = map[string][]byte{}
	}
	m.e2bTemplateFiles[id][hash] = raw
	m.e2bTemplates[id] = t
	return nil
}
func (m *Memory) OpenE2BTemplateFile(_ context.Context, project, id, hash string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.e2bTemplates[id]
	if !ok || t.Project != project {
		return nil, ErrNotFound
	}
	raw, ok := m.e2bTemplateFiles[id][hash]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

// Lock only the template metadata, and send bounded binary chunks to Postgres.
// Logs/status queries never read or rewrite these rows. A failed upload rolls
// back its chunks and quota accounting together, preserving the prior file.
func (p *Postgres) PutE2BTemplateFile(ctx context.Context, project, id, hash string, r io.Reader) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT data FROM e2b_templates WHERE project=$1 AND id=$2 FOR UPDATE`, project, id).Scan(&raw); err != nil {
			return pgErr(err)
		}
		var t E2BTemplate
		if err := json.Unmarshal(raw, &t); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM e2b_template_file_chunks WHERE template=$1 AND hash=$2`, id, hash); err != nil {
			return err
		}
		buf := make([]byte, templateChunkSize)
		var size int64
		for seq := 0; ; seq++ {
			n, err := io.ReadFull(r, buf)
			if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
				return err
			}
			size += int64(n)
			if e := retainTemplateFile(&t, hash, size); e != nil {
				return e
			}
			if n > 0 || seq == 0 {
				if _, e := tx.Exec(ctx, `INSERT INTO e2b_template_file_chunks (template,hash,sequence,data) VALUES ($1,$2,$3,$4)`, id, hash, seq, buf[:n]); e != nil {
					return e
				}
			}
			if err != nil {
				break
			}
		}
		raw, err := json.Marshal(t)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE e2b_templates SET data=$3 WHERE project=$1 AND id=$2`, project, id, raw)
		return err
	})
}

func (p *Postgres) OpenE2BTemplateFile(ctx context.Context, project, id, hash string) (io.ReadCloser, error) {
	rows, err := p.pool.Query(ctx, `SELECT c.data FROM e2b_template_file_chunks c JOIN e2b_templates t ON t.id=c.template WHERE t.project=$1 AND c.template=$2 AND c.hash=$3 ORDER BY c.sequence`, project, id, hash)
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		rows.Close()
		if rows.Err() != nil {
			return nil, rows.Err()
		}
		return nil, ErrNotFound
	}
	var data []byte
	if err = rows.Scan(&data); err != nil {
		rows.Close()
		return nil, err
	}
	return &templateFileReader{rows: rows, chunk: bytes.NewReader(data)}, nil
}

type templateFileReader struct {
	rows  pgx.Rows
	chunk *bytes.Reader
}

func (r *templateFileReader) Read(p []byte) (int, error) {
	for {
		n, err := r.chunk.Read(p)
		if err != io.EOF {
			return n, err
		}
		if !r.rows.Next() {
			if err = r.rows.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		var data []byte
		if err = r.rows.Scan(&data); err != nil {
			return 0, err
		}
		r.chunk = bytes.NewReader(data)
	}
}
func (r *templateFileReader) Close() error { r.rows.Close(); return nil }
