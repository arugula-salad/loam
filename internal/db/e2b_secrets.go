package db

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// E2BSecret keeps only encrypted material. Public metadata is selected by the
// adapter; the sealed value is never serialized as a public secret response.
type E2BSecret struct {
	ID        string            `json:"id"`
	Project   string            `json:"project"`
	Name      string            `json:"name"`
	Version   int               `json:"version"`
	Metadata  map[string]string `json:"metadata"`
	CreatedAt time.Time         `json:"createdAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
	Sealed    []byte            `json:"sealed"`
}

func (s E2BSecret) Clone() E2BSecret {
	b, _ := json.Marshal(s)
	var out E2BSecret
	json.Unmarshal(b, &out)
	return out
}
func (m *Memory) E2BSecrets(_ context.Context, project string) ([]E2BSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []E2BSecret{}
	for _, s := range m.e2bSecrets {
		if s.Project == project {
			out = append(out, s.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (m *Memory) PutE2BSecret(_ context.Context, s E2BSecret, create bool) (E2BSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[s.Project]; !ok {
		return E2BSecret{}, ErrNotFound
	}
	if m.e2bSecrets == nil {
		m.e2bSecrets = map[string]E2BSecret{}
	}
	old, exists := m.e2bSecrets[s.ID]
	if create {
		for _, v := range m.e2bSecrets {
			if v.ID == s.ID || v.Project == s.Project && v.Name == s.Name {
				return E2BSecret{}, ErrExists
			}
		}
		s.Version = 1
	} else {
		if !exists || old.Project != s.Project {
			return E2BSecret{}, ErrNotFound
		}
		s.Name, s.CreatedAt, s.Version = old.Name, old.CreatedAt, old.Version+1
		if s.Metadata == nil {
			s.Metadata = old.Metadata
		}
	}
	m.e2bSecrets[s.ID] = s.Clone()
	return s.Clone(), nil
}
func (m *Memory) DeleteE2BSecret(_ context.Context, project, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.e2bSecrets[id]
	if !ok || s.Project != project {
		return ErrNotFound
	}
	delete(m.e2bSecrets, id)
	return nil
}
func (p *Postgres) E2BSecrets(ctx context.Context, project string) ([]E2BSecret, error) {
	rows, err := p.pool.Query(ctx, `SELECT data FROM e2b_secrets WHERE project=$1 ORDER BY id`, project)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (E2BSecret, error) {
		var b []byte
		var s E2BSecret
		err := r.Scan(&b)
		if err == nil {
			err = json.Unmarshal(b, &s)
		}
		return s, err
	})
}
func (p *Postgres) PutE2BSecret(ctx context.Context, s E2BSecret, create bool) (E2BSecret, error) {
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if create {
			s.Version = 1
		} else {
			var b []byte
			var old E2BSecret
			if err := tx.QueryRow(ctx, `SELECT data FROM e2b_secrets WHERE project=$1 AND id=$2 FOR UPDATE`, s.Project, s.ID).Scan(&b); err != nil {
				return pgErr(err)
			}
			if err := json.Unmarshal(b, &old); err != nil {
				return err
			}
			s.Name, s.CreatedAt, s.Version = old.Name, old.CreatedAt, old.Version+1
			if s.Metadata == nil {
				s.Metadata = old.Metadata
			}
		}
		b, err := json.Marshal(s)
		if err != nil {
			return err
		}
		if create {
			_, err = tx.Exec(ctx, `INSERT INTO e2b_secrets(id,project,name,data) VALUES($1,$2,$3,$4)`, s.ID, s.Project, s.Name, b)
		} else {
			_, err = tx.Exec(ctx, `UPDATE e2b_secrets SET data=$3 WHERE project=$1 AND id=$2`, s.Project, s.ID, b)
		}
		return pgErr(err)
	})
	return s, err
}
func (p *Postgres) DeleteE2BSecret(ctx context.Context, project, id string) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM e2b_secrets WHERE project=$1 AND id=$2`, project, id)
	if err != nil {
		return pgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
