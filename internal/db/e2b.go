package db

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// E2BSandbox is what the E2B adapter (internal/e2b, docs/E2B.md) knows of a
// sandbox it made, beyond the sandbox's own record: E2B's view of its
// lifecycle, and envd's access token. It is deleted with its sandbox.
type E2BSandbox struct {
	Sandbox string
	Project string
	// Template is the E2B template it was made from, as E2B names it.
	Template string
	// SealedToken is envd's access token, sealed with loam-api's secret key
	// and bound to the sandbox's ID (apiserver's Seal): never kept, logged
	// or shown in the clear by the store.
	SealedToken []byte
	// TimeoutS is the timeout it was created with, which a resume by its
	// traffic (AutoResume) gives it again.
	TimeoutS int
	// StartedAt is the create or the last resume; EndAt when its timeout
	// runs out, or the moment it was paused.
	StartedAt time.Time
	EndAt     time.Time
	// AutoPause pauses it at EndAt rather than deleting it: with its memory
	// when KeepMemory, else only its disk. AutoResume resumes a paused one
	// when traffic comes for it.
	AutoPause  bool
	KeepMemory bool
	AutoResume bool
	// Paused is E2B's paused, by a pause call, until a connect.
	Paused bool
}

// Clone is a deep copy.
func (e E2BSandbox) Clone() E2BSandbox {
	e.SealedToken = append([]byte{}, e.SealedToken...)
	return e
}

func (m *Memory) CreateE2B(_ context.Context, e E2BSandbox) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sandboxes[e.Sandbox]; !ok {
		return ErrNotFound
	}
	if _, ok := m.e2b[e.Sandbox]; ok {
		return ErrExists
	}
	m.e2b[e.Sandbox] = e.Clone()
	return nil
}

func (m *Memory) E2B(_ context.Context, id string) (E2BSandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.e2b[id]
	if !ok {
		return e, ErrNotFound
	}
	return e.Clone(), nil
}

func (m *Memory) E2Bs(_ context.Context, project string) ([]E2BSandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []E2BSandbox
	for _, e := range m.e2b {
		if project == "" || e.Project == project {
			out = append(out, e.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sandbox < out[j].Sandbox })
	return out, nil
}

func (m *Memory) UpdateE2B(_ context.Context, id string, change func(*E2BSandbox) error) (E2BSandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.e2b[id]
	if !ok {
		return e, ErrNotFound
	}
	e = e.Clone()
	if err := change(&e); err != nil {
		return E2BSandbox{}, err
	}
	e.Sandbox = id
	m.e2b[id] = e.Clone()
	return e, nil
}

const e2bCols = `sandbox, project, template, sealed_token, timeout_s, started_at, end_at, auto_pause, keep_memory, auto_resume, paused`

func e2bArgs(e E2BSandbox) []any {
	return []any{e.Sandbox, e.Project, e.Template, e.SealedToken, e.TimeoutS, e.StartedAt, e.EndAt,
		e.AutoPause, e.KeepMemory, e.AutoResume, e.Paused}
}

func scanE2B(r pgx.Row) (E2BSandbox, error) {
	var e E2BSandbox
	err := r.Scan(&e.Sandbox, &e.Project, &e.Template, &e.SealedToken, &e.TimeoutS, &e.StartedAt, &e.EndAt,
		&e.AutoPause, &e.KeepMemory, &e.AutoResume, &e.Paused)
	e.StartedAt, e.EndAt = e.StartedAt.UTC(), e.EndAt.UTC()
	return e, err
}

// CreateE2B is ErrNotFound when the sandbox doesn't exist (the foreign key).
func (p *Postgres) CreateE2B(ctx context.Context, e E2BSandbox) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO e2b_sandboxes (`+e2bCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`, e2bArgs(e)...)
	return pgErr(err)
}

func (p *Postgres) E2B(ctx context.Context, id string) (E2BSandbox, error) {
	e, err := scanE2B(p.pool.QueryRow(ctx, `SELECT `+e2bCols+` FROM e2b_sandboxes WHERE sandbox = $1`, id))
	return e, pgErr(err)
}

func (p *Postgres) E2Bs(ctx context.Context, project string) ([]E2BSandbox, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+e2bCols+` FROM e2b_sandboxes WHERE $1 = '' OR project = $1 ORDER BY sandbox`, project)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (E2BSandbox, error) { return scanE2B(r) })
}

func (p *Postgres) UpdateE2B(ctx context.Context, id string, change func(*E2BSandbox) error) (E2BSandbox, error) {
	var out E2BSandbox
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		e, err := scanE2B(tx.QueryRow(ctx, `SELECT `+e2bCols+` FROM e2b_sandboxes WHERE sandbox = $1 FOR UPDATE`, id))
		if err != nil {
			return pgErr(err)
		}
		if err := change(&e); err != nil {
			return err
		}
		e.Sandbox = id
		if _, err := tx.Exec(ctx, `UPDATE e2b_sandboxes SET (`+e2bCols+`) = ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			WHERE sandbox = $1`, e2bArgs(e)...); err != nil {
			return pgErr(err)
		}
		out = e
		return nil
	})
	return out, err
}
