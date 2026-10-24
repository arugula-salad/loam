package db

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/arugula-salad/loam/pkg/api"
	"github.com/jackc/pgx/v5"
)

// E2BTemplate owns versioned build snapshots in one project. Tags only point
// at completed builds, so a failed rebuild cannot replace a working template.
type E2BTemplate struct {
	ID        string              `json:"id"`
	Project   string              `json:"project"`
	Name      string              `json:"name"`
	CreatedAt time.Time           `json:"created_at"`
	Files     map[string][]byte   `json:"files,omitempty"`
	Builds    map[string]E2BBuild `json:"builds"`
	Tags      map[string]string   `json:"tags"`
}

type E2BBuildLog struct {
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
}

type E2BBuild struct {
	Logs       []E2BBuildLog     `json:"logs,omitempty"`
	User       string            `json:"user,omitempty"`
	Workdir    string            `json:"workdir,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	ID         string            `json:"id"`
	Status     string            `json:"status"`
	Resources  api.Resources     `json:"resources"`
	Tags       []string          `json:"tags"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
	FinishedAt *time.Time        `json:"finished_at,omitempty"`
	Sandbox    string            `json:"sandbox,omitempty"`
	Checkpoint string            `json:"checkpoint,omitempty"`
	Error      string            `json:"error,omitempty"`
}

func (t E2BTemplate) Clone() E2BTemplate {
	raw, _ := json.Marshal(t)
	var out E2BTemplate
	json.Unmarshal(raw, &out)
	return out
}

func (m *Memory) CreateE2BTemplate(_ context.Context, t E2BTemplate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[t.Project]; !ok {
		return ErrNotFound
	}
	for _, old := range m.e2bTemplates {
		if old.ID == t.ID || old.Project == t.Project && old.Name == t.Name {
			return ErrExists
		}
	}
	if m.e2bTemplates == nil {
		m.e2bTemplates = map[string]E2BTemplate{}
	}
	m.e2bTemplates[t.ID] = t.Clone()
	return nil
}
func (m *Memory) E2BTemplates(_ context.Context, project string) ([]E2BTemplate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []E2BTemplate{}
	for _, t := range m.e2bTemplates {
		if project == "" || t.Project == project {
			out = append(out, t.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (m *Memory) UpdateE2BTemplate(_ context.Context, project, id string, change func(*E2BTemplate) error) (E2BTemplate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.e2bTemplates[id]
	if !ok || old.Project != project {
		return E2BTemplate{}, ErrNotFound
	}
	t := old.Clone()
	if err := change(&t); err != nil {
		return E2BTemplate{}, err
	}
	t.ID, t.Project, t.Name = old.ID, old.Project, old.Name
	m.e2bTemplates[id] = t.Clone()
	return t, nil
}
func (m *Memory) DeleteE2BTemplate(_ context.Context, project, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.e2bTemplates[id]
	if !ok || t.Project != project {
		return ErrNotFound
	}
	delete(m.e2bTemplates, id)
	return nil
}
func (p *Postgres) CreateE2BTemplate(ctx context.Context, t E2BTemplate) error {
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `INSERT INTO e2b_templates (id,project,name,data) VALUES ($1,$2,$3,$4)`, t.ID, t.Project, t.Name, raw)
	return pgErr(err)
}
func (p *Postgres) E2BTemplates(ctx context.Context, project string) ([]E2BTemplate, error) {
	rows, err := p.pool.Query(ctx, `SELECT data FROM e2b_templates WHERE $1='' OR project=$1 ORDER BY id`, project)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (E2BTemplate, error) {
		var raw []byte
		var t E2BTemplate
		err := r.Scan(&raw)
		if err == nil {
			err = json.Unmarshal(raw, &t)
		}
		return t, err
	})
}
func (p *Postgres) UpdateE2BTemplate(ctx context.Context, project, id string, change func(*E2BTemplate) error) (E2BTemplate, error) {
	var out E2BTemplate
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT data FROM e2b_templates WHERE project=$1 AND id=$2 FOR UPDATE`, project, id).Scan(&raw); err != nil {
			return pgErr(err)
		}
		var t E2BTemplate
		if err := json.Unmarshal(raw, &t); err != nil {
			return err
		}
		name := t.Name
		if err := change(&t); err != nil {
			return err
		}
		t.ID, t.Project, t.Name = id, project, name
		raw, err := json.Marshal(t)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE e2b_templates SET data=$3 WHERE project=$1 AND id=$2`, project, id, raw); err != nil {
			return pgErr(err)
		}
		out = t
		return nil
	})
	return out, err
}
func (p *Postgres) DeleteE2BTemplate(ctx context.Context, project, id string) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM e2b_templates WHERE project=$1 AND id=$2`, project, id)
	if err != nil {
		return pgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
