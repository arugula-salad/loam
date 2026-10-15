package db

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arugula-salad/loam/pkg/api"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Postgres is the Store in service.
type Postgres struct {
	pool *pgxpool.Pool
}

// Open connects to url and applies every migration not yet applied.
// RuntimeParams in cfg (say, a search_path) apply to every connection; tests
// use that for a schema of their own.
func Open(ctx context.Context, url string, params map[string]string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	for k, v := range params {
		cfg.ConnConfig.RuntimeParams[k] = v
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	p := &Postgres{pool: pool}
	if err := p.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

func (p *Postgres) Close() { p.pool.Close() }

// migrate applies migrations/NNNN_*.sql in order, each in its own transaction
// with its version recorded, under an advisory lock so that two loam-api
// processes starting at once (a rollout) don't both apply one.
func (p *Postgres) migrate(ctx context.Context) error {
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(7070)`); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(7070)`)
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	for _, name := range names {
		base := strings.TrimPrefix(name, "migrations/")
		v, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: no version prefix", base)
		}
		var done bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, v).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, v)
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", base, err)
		}
	}
	return nil
}

// pgErr maps Postgres's answers to the Store's errors.
func pgErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "23505": // unique_violation
			return ErrExists
		case "23001": // restrict_violation: deleting a project its sandboxes still reference (ON DELETE RESTRICT)
			if pe.ConstraintName == "sandboxes_project_fkey" {
				return ErrNotEmpty
			}
		case "23503": // foreign_key_violation
			if pe.ConstraintName == "sandboxes_project_fkey" && strings.Contains(pe.Message, "update or delete") {
				return ErrNotEmpty
			}
			return ErrNotFound
		}
	}
	return err
}

func (p *Postgres) CreateProject(ctx context.Context, pr api.Project) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO projects (name, created_at) VALUES ($1, $2)`, pr.Name, pr.CreatedAt)
	return pgErr(err)
}

func (p *Postgres) Project(ctx context.Context, name string) (api.Project, error) {
	var pr api.Project
	err := p.pool.QueryRow(ctx, `SELECT name, created_at FROM projects WHERE name = $1`, name).Scan(&pr.Name, &pr.CreatedAt)
	return pr, pgErr(err)
}

func (p *Postgres) Projects(ctx context.Context) ([]api.Project, error) {
	rows, err := p.pool.Query(ctx, `SELECT name, created_at FROM projects ORDER BY created_at, name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (api.Project, error) {
		var pr api.Project
		return pr, r.Scan(&pr.Name, &pr.CreatedAt)
	})
}

func (p *Postgres) DeleteProject(ctx context.Context, name string) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM projects WHERE name = $1`, name)
	if err != nil {
		return pgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) CreateKey(ctx context.Context, k Key) error {
	scope := k.Scope
	if scope == "" {
		scope = api.ScopeFull
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO api_keys (id, project, name, hash, created_at, scope) VALUES ($1, $2, $3, $4, $5, $6)`,
		k.ID, k.Project, k.Name, k.Hash, k.CreatedAt, scope)
	return pgErr(err)
}

func scanKey(r pgx.Row) (Key, error) {
	var k Key
	return k, r.Scan(&k.ID, &k.Project, &k.Name, &k.Hash, &k.CreatedAt, &k.Scope)
}

func (p *Postgres) Keys(ctx context.Context, project string) ([]Key, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, project, name, hash, created_at, scope FROM api_keys WHERE project = $1 ORDER BY created_at, id`, project)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Key, error) { return scanKey(r) })
}

func (p *Postgres) KeyByHash(ctx context.Context, hash []byte) (Key, error) {
	k, err := scanKey(p.pool.QueryRow(ctx, `SELECT id, project, name, hash, created_at, scope FROM api_keys WHERE hash = $1`, hash))
	return k, pgErr(err)
}

func (p *Postgres) DeleteKey(ctx context.Context, project, key string) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM api_keys WHERE project = $1 AND (id = $2 OR name = $2)`, project, key)
	if err != nil {
		return pgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

const sandboxCols = `id, name, project, template, state, error, node, cpus, memory_mib, disk_gib, env, labels,
	idle_timeout_s, idle_action, deadline_at, deadline_action, protected, http_port,
	next_checkpoint, next_auto, created_at, updated_at, last_active_at, snapshot, next_snapshot, network, secrets, image`

func scanSandbox(r pgx.Row) (Sandbox, error) {
	var s Sandbox
	var env, labels, network []byte
	var dlAt *time.Time
	var dlAction *string
	err := r.Scan(&s.ID, &s.Name, &s.Project, &s.Template, &s.State, &s.Error, &s.Node,
		&s.Resources.CPUs, &s.Resources.MemoryMiB, &s.Resources.DiskGiB, &env, &labels,
		&s.Idle.TimeoutS, &s.Idle.Action, &dlAt, &dlAction, &s.Protected, &s.HTTPPort,
		&s.NextCheckpoint, &s.NextAuto, &s.CreatedAt, &s.UpdatedAt, &s.LastActiveAt, &s.Snapshot, &s.NextSnapshot,
		&network, &s.Secrets, &s.Image)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(network, &s.Network); err != nil {
		return s, err
	}
	if err := json.Unmarshal(env, &s.Env); err != nil {
		return s, err
	}
	if err := json.Unmarshal(labels, &s.Labels); err != nil {
		return s, err
	}
	if dlAt != nil {
		s.Deadline = &api.Deadline{At: dlAt.UTC(), Action: deref(dlAction)}
	}
	s.CreatedAt, s.UpdatedAt, s.LastActiveAt = s.CreatedAt.UTC(), s.UpdatedAt.UTC(), s.LastActiveAt.UTC()
	return s.Clone(), nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// sandboxArgs are s's columns in sandboxCols order.
func sandboxArgs(s Sandbox) []any {
	env, _ := json.Marshal(cloneMap(s.Env))
	labels, _ := json.Marshal(cloneMap(s.Labels))
	network, _ := json.Marshal(CloneNetwork(s.Network))
	var dlAt *time.Time
	var dlAction *string
	if s.Deadline != nil {
		dlAt, dlAction = &s.Deadline.At, &s.Deadline.Action
	}
	return []any{s.ID, s.Name, s.Project, s.Template, s.State, s.Error, s.Node,
		s.Resources.CPUs, s.Resources.MemoryMiB, s.Resources.DiskGiB, env, labels,
		s.Idle.TimeoutS, s.Idle.Action, dlAt, dlAction, s.Protected, s.HTTPPort,
		s.NextCheckpoint, s.NextAuto, s.CreatedAt, s.UpdatedAt, s.LastActiveAt, s.Snapshot, s.NextSnapshot,
		network, append([]string{}, s.Secrets...), s.Image}
}

// checkSecrets is ErrNoSecret when s names a secret its project doesn't have.
// It holds a share lock on the secrets it names until tx ends, so a
// DeleteSecret (which locks the row for update, then looks for sandboxes
// naming it) either sees this sandbox or deletes the secret before this
// check, never between it and the write.
func checkSecrets(ctx context.Context, tx pgx.Tx, s Sandbox) error {
	if len(s.Secrets) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT name FROM secrets WHERE project = $1 AND name = ANY($2) FOR SHARE`, s.Project, s.Secrets)
	if err != nil {
		return err
	}
	have, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, name := range s.Secrets {
		if !slices.Contains(have, name) {
			return fmt.Errorf("%w: %q", ErrNoSecret, name)
		}
	}
	return nil
}

func (p *Postgres) CreateSandbox(ctx context.Context, s Sandbox) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if err := checkSecrets(ctx, tx, s); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO sandboxes (`+sandboxCols+`) VALUES
			($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28)`,
			sandboxArgs(s)...)
		return pgErr(err)
	})
}

func (p *Postgres) Sandbox(ctx context.Context, id string) (Sandbox, error) {
	s, err := scanSandbox(p.pool.QueryRow(ctx, `SELECT `+sandboxCols+` FROM sandboxes WHERE id = $1`, id))
	return s, pgErr(err)
}

func (p *Postgres) SandboxByName(ctx context.Context, project, name string) (Sandbox, error) {
	s, err := scanSandbox(p.pool.QueryRow(ctx, `SELECT `+sandboxCols+` FROM sandboxes WHERE project = $1 AND name = $2`, project, name))
	return s, pgErr(err)
}

func (p *Postgres) Sandboxes(ctx context.Context, project string) ([]Sandbox, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+sandboxCols+` FROM sandboxes WHERE $1 = '' OR project = $1 ORDER BY created_at, id`, project)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Sandbox, error) { return scanSandbox(r) })
}

func (p *Postgres) UpdateSandbox(ctx context.Context, id string, change func(*Sandbox) error) (Sandbox, error) {
	var out Sandbox
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		s, err := scanSandbox(tx.QueryRow(ctx, `SELECT `+sandboxCols+` FROM sandboxes WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return pgErr(err)
		}
		named := s.Secrets
		if err := change(&s); err != nil {
			return err
		}
		s.ID = id
		if !slices.Equal(named, s.Secrets) {
			if err := checkSecrets(ctx, tx, s); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE sandboxes SET (`+sandboxCols+`) =
			($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28)
			WHERE id = $1`, sandboxArgs(s)...)
		if err != nil {
			return pgErr(err)
		}
		out = s.Clone()
		return nil
	})
	return out, err
}

func (p *Postgres) DeleteSandbox(ctx context.Context, id string) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM sandboxes WHERE id = $1`, id)
	if err != nil {
		return pgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) CreateCheckpoint(ctx context.Context, c api.Checkpoint) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO checkpoints (sandbox, id, comment, memory, auto, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		c.Sandbox, c.ID, c.Comment, c.Memory, c.Auto, c.CreatedAt)
	return pgErr(err)
}

const checkpointCols = `sandbox, id, comment, memory, auto, created_at`

func scanCheckpoint(r pgx.Row) (api.Checkpoint, error) {
	var c api.Checkpoint
	err := r.Scan(&c.Sandbox, &c.ID, &c.Comment, &c.Memory, &c.Auto, &c.CreatedAt)
	c.CreatedAt = c.CreatedAt.UTC()
	return c, err
}

func (p *Postgres) Checkpoint(ctx context.Context, sandbox, id string) (api.Checkpoint, error) {
	c, err := scanCheckpoint(p.pool.QueryRow(ctx, `SELECT `+checkpointCols+` FROM checkpoints WHERE sandbox = $1 AND id = $2`, sandbox, id))
	return c, pgErr(err)
}

func (p *Postgres) Checkpoints(ctx context.Context, sandbox string) ([]api.Checkpoint, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+checkpointCols+` FROM checkpoints WHERE sandbox = $1 ORDER BY seq`, sandbox)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (api.Checkpoint, error) { return scanCheckpoint(r) })
}

func (p *Postgres) DeleteCheckpoint(ctx context.Context, sandbox, id string) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM checkpoints WHERE sandbox = $1 AND id = $2`, sandbox, id)
	if err != nil {
		return pgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

const snapshotCols = `id, sandbox, kind, checkpoint, node, parent, layers, memory, stored, state, size_bytes, created_at, seq`

func scanSnapshot(r pgx.Row) (Snapshot, error) {
	var s Snapshot
	err := r.Scan(&s.ID, &s.Sandbox, &s.Kind, &s.Checkpoint, &s.Node, &s.Parent, &s.Layers,
		&s.Memory, &s.Stored, &s.State, &s.SizeBytes, &s.CreatedAt, &s.Seq)
	s.CreatedAt = s.CreatedAt.UTC()
	return s.Clone(), err
}

func (p *Postgres) collectSnapshots(ctx context.Context, sql string, args ...any) ([]Snapshot, error) {
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Snapshot, error) { return scanSnapshot(r) })
}

// addLayers adds a record's layers to the layer set, in the record's transaction.
func addLayers(ctx context.Context, tx pgx.Tx, layers []string) error {
	if len(layers) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO layers (id) SELECT unnest($1::text[]) ON CONFLICT DO NOTHING`, layers)
	return err
}

func (p *Postgres) CreateSnapshot(ctx context.Context, s Snapshot) (Snapshot, error) {
	s = s.Clone()
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO snapshots (id, sandbox, kind, checkpoint, node, parent, layers, memory, stored, state, size_bytes, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING seq`,
			s.ID, s.Sandbox, s.Kind, s.Checkpoint, s.Node, s.Parent, s.Layers, s.Memory, s.Stored, s.State, s.SizeBytes, s.CreatedAt).Scan(&s.Seq)
		if err != nil {
			return pgErr(err)
		}
		return addLayers(ctx, tx, s.Layers)
	})
	return s, err
}

func (p *Postgres) Snapshot(ctx context.Context, id string) (Snapshot, error) {
	s, err := scanSnapshot(p.pool.QueryRow(ctx, `SELECT `+snapshotCols+` FROM snapshots WHERE id = $1`, id))
	return s, pgErr(err)
}

func (p *Postgres) SnapshotsOf(ctx context.Context, sandbox string) ([]Snapshot, error) {
	return p.collectSnapshots(ctx, `SELECT `+snapshotCols+` FROM snapshots WHERE sandbox = $1 ORDER BY seq`, sandbox)
}

func (p *Postgres) SnapshotsOn(ctx context.Context, node string) ([]Snapshot, error) {
	return p.collectSnapshots(ctx, `SELECT `+snapshotCols+` FROM snapshots WHERE node = $1 ORDER BY seq`, node)
}

func (p *Postgres) UpdateSnapshot(ctx context.Context, id string, change func(*Snapshot) error) (Snapshot, error) {
	var out Snapshot
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		s, err := scanSnapshot(tx.QueryRow(ctx, `SELECT `+snapshotCols+` FROM snapshots WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return pgErr(err)
		}
		seq := s.Seq
		if err := change(&s); err != nil {
			return err
		}
		s.ID, s.Seq = id, seq
		s = s.Clone() // a nil Layers is an empty array, not NULL
		_, err = tx.Exec(ctx, `UPDATE snapshots SET (sandbox, kind, checkpoint, node, parent, layers, memory, stored, state, size_bytes, created_at) =
			($2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) WHERE id = $1`,
			id, s.Sandbox, s.Kind, s.Checkpoint, s.Node, s.Parent, s.Layers, s.Memory, s.Stored, s.State, s.SizeBytes, s.CreatedAt)
		if err != nil {
			return pgErr(err)
		}
		if err := addLayers(ctx, tx, s.Layers); err != nil {
			return err
		}
		out = s.Clone()
		return nil
	})
	return out, err
}

func (p *Postgres) DeleteSnapshot(ctx context.Context, id string) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM snapshots WHERE id = $1`, id)
	if err != nil {
		return pgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GarbageSnapshots is the rule in db.go, the same as Memory's.
func (p *Postgres) GarbageSnapshots(ctx context.Context) ([]Snapshot, error) {
	return p.collectSnapshots(ctx, `SELECT `+snapshotCols+` FROM snapshots s
		WHERE s.state <> 'pending'
		  AND NOT EXISTS (SELECT 1 FROM sandboxes b WHERE b.snapshot = s.id)
		  AND (s.state = 'abandoned'
		    OR (s.kind = 'checkpoint'
		        AND NOT EXISTS (SELECT 1 FROM checkpoints c WHERE c.sandbox = s.sandbox AND c.id = s.checkpoint))
		    OR (s.kind <> 'checkpoint'
		        AND (NOT EXISTS (SELECT 1 FROM sandboxes b WHERE b.id = s.sandbox)
		          OR EXISTS (SELECT 1 FROM snapshots n
		                     WHERE n.sandbox = s.sandbox AND n.stored AND n.state = 'ready' AND n.seq > s.seq))))
		ORDER BY s.seq`)
}

func (p *Postgres) Layers(ctx context.Context) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT id FROM layers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (p *Postgres) UnreferencedLayers(ctx context.Context) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT l.id FROM layers l
		WHERE NOT EXISTS (SELECT 1 FROM snapshots s WHERE s.layers @> ARRAY[l.id]) ORDER BY l.id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// DeleteLayer locks the layer's row and checks it is unreferenced in one
// transaction.
func (p *Postgres) DeleteLayer(ctx context.Context, id string) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var x string
		if err := tx.QueryRow(ctx, `SELECT id FROM layers WHERE id = $1 FOR UPDATE`, id).Scan(&x); err != nil {
			return pgErr(err)
		}
		var used bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM snapshots WHERE layers @> ARRAY[$1::text])`, id).Scan(&used); err != nil {
			return err
		}
		if used {
			return ErrInUse
		}
		_, err := tx.Exec(ctx, `DELETE FROM layers WHERE id = $1`, id)
		return err
	})
}

func (p *Postgres) AddLayers(ctx context.Context, ids []string) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error { return addLayers(ctx, tx, ids) })
}

func (p *Postgres) AddTombstone(ctx context.Context, node, vm string) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO vm_tombstones (node, vm) VALUES ($1, $2) ON CONFLICT DO NOTHING`, node, vm)
	return pgErr(err)
}

func (p *Postgres) Tombstones(ctx context.Context, node string) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT vm FROM vm_tombstones WHERE node = $1 ORDER BY vm`, node)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (p *Postgres) DeleteTombstone(ctx context.Context, node, vm string) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM vm_tombstones WHERE node = $1 AND vm = $2`, node, vm)
	if err != nil {
		return pgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) SetPort(ctx context.Context, ps PortSetting) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO port_settings (sandbox, port, access) VALUES ($1, $2, $3)
		ON CONFLICT (sandbox, port) DO UPDATE SET access = EXCLUDED.access`, ps.Sandbox, ps.Port, ps.Access)
	return pgErr(err)
}

func (p *Postgres) Ports(ctx context.Context, sandbox string) ([]PortSetting, error) {
	rows, err := p.pool.Query(ctx, `SELECT sandbox, port, access FROM port_settings WHERE sandbox = $1 ORDER BY port`, sandbox)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PortSetting, error) {
		var ps PortSetting
		return ps, r.Scan(&ps.Sandbox, &ps.Port, &ps.Access)
	})
}

const secretCols = `project, name, hosts, placeholder, inject, value, created_at, updated_at`

func scanSecret(r pgx.Row) (Secret, error) {
	var s Secret
	var inject []byte
	if err := r.Scan(&s.Project, &s.Name, &s.Hosts, &s.Placeholder, &inject, &s.Value, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return s, err
	}
	if inject != nil {
		if err := json.Unmarshal(inject, &s.Inject); err != nil {
			return s, err
		}
	}
	s.CreatedAt, s.UpdatedAt = s.CreatedAt.UTC(), s.UpdatedAt.UTC()
	return s.Clone(), nil
}

func (p *Postgres) PutSecret(ctx context.Context, s Secret) (Secret, error) {
	var inject any // NULL for none
	if s.Inject != nil {
		b, err := json.Marshal(s.Inject)
		if err != nil {
			return s, err
		}
		inject = b
	}
	out, err := scanSecret(p.pool.QueryRow(ctx, `INSERT INTO secrets (`+secretCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (project, name) DO UPDATE SET hosts = EXCLUDED.hosts, placeholder = EXCLUDED.placeholder,
			inject = EXCLUDED.inject, value = EXCLUDED.value, updated_at = EXCLUDED.updated_at
		RETURNING `+secretCols,
		s.Project, s.Name, append([]string{}, s.Hosts...), s.Placeholder, inject, s.Value, s.CreatedAt, s.UpdatedAt))
	return out, pgErr(err)
}

func (p *Postgres) Secret(ctx context.Context, project, name string) (Secret, error) {
	s, err := scanSecret(p.pool.QueryRow(ctx, `SELECT `+secretCols+` FROM secrets WHERE project = $1 AND name = $2`, project, name))
	return s, pgErr(err)
}

func (p *Postgres) Secrets(ctx context.Context, project string) ([]Secret, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+secretCols+` FROM secrets WHERE project = $1 ORDER BY name`, project)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Secret, error) { return scanSecret(r) })
}

// DeleteSecret locks the secret's row, then looks for a sandbox naming it;
// checkSecrets is the other half.
func (p *Postgres) DeleteSecret(ctx context.Context, project, name string) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var x string
		if err := tx.QueryRow(ctx, `SELECT name FROM secrets WHERE project = $1 AND name = $2 FOR UPDATE`, project, name).Scan(&x); err != nil {
			return pgErr(err)
		}
		var used bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sandboxes WHERE project = $1 AND secrets @> ARRAY[$2::text])`, project, name).Scan(&used); err != nil {
			return err
		}
		if used {
			return ErrInUse
		}
		_, err := tx.Exec(ctx, `DELETE FROM secrets WHERE project = $1 AND name = $2`, project, name)
		return err
	})
}

func (p *Postgres) AddEvent(ctx context.Context, e Event) error {
	var detail []byte
	if e.Detail != nil {
		var err error
		if detail, err = json.Marshal(e.Detail); err != nil {
			return err
		}
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO events (project, sandbox, type, at, detail) VALUES ($1, $2, $3, $4, $5)`,
		e.Project, e.Sandbox, e.Type, e.At, detail)
	return err
}

func (p *Postgres) Events(ctx context.Context, project string, since time.Time) ([]Event, error) {
	rows, err := p.pool.Query(ctx, `SELECT project, sandbox, type, at, detail FROM events WHERE project = $1 AND at >= $2 ORDER BY id`, project, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
		var e Event
		var detail []byte
		if err := r.Scan(&e.Project, &e.Sandbox, &e.Type, &e.At, &detail); err != nil {
			return e, err
		}
		e.At = e.At.UTC()
		if detail != nil {
			if err := json.Unmarshal(detail, &e.Detail); err != nil {
				return e, err
			}
		}
		return e, nil
	})
}

func (p *Postgres) PruneEvents(ctx context.Context, before time.Time) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM events WHERE at < $1`, before)
	return err
}

func (p *Postgres) AddTraffic(ctx context.Context, ts []Traffic) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		for _, t := range ts {
			_, err := tx.Exec(ctx, `INSERT INTO sandbox_traffic
				(sandbox, day, requests, status_2xx, status_3xx, status_4xx, status_5xx, websockets, bytes_sent, key_requests)
				SELECT $1, $2::date, $3, $4, $5, $6, $7, $8, $9, $10 WHERE EXISTS (SELECT 1 FROM sandboxes WHERE id = $1)
				ON CONFLICT (sandbox, day) DO UPDATE SET
					requests = sandbox_traffic.requests + EXCLUDED.requests,
					status_2xx = sandbox_traffic.status_2xx + EXCLUDED.status_2xx,
					status_3xx = sandbox_traffic.status_3xx + EXCLUDED.status_3xx,
					status_4xx = sandbox_traffic.status_4xx + EXCLUDED.status_4xx,
					status_5xx = sandbox_traffic.status_5xx + EXCLUDED.status_5xx,
					websockets = sandbox_traffic.websockets + EXCLUDED.websockets,
					bytes_sent = sandbox_traffic.bytes_sent + EXCLUDED.bytes_sent,
					key_requests = sandbox_traffic.key_requests + EXCLUDED.key_requests`,
				t.Sandbox, t.Day, t.Requests, t.Status2xx, t.Status3xx, t.Status4xx, t.Status5xx, t.WebSockets, t.BytesSent, t.KeyRequests)
			if err != nil {
				return pgErr(err)
			}
			if len(t.NewVisitors) == 0 {
				continue
			}
			_, err = tx.Exec(ctx, `INSERT INTO sandbox_visitors (sandbox, day, visitor)
				SELECT $1, $2::date, v FROM unnest($3::bytea[]) AS v WHERE EXISTS (SELECT 1 FROM sandboxes WHERE id = $1)
				ON CONFLICT DO NOTHING`, t.Sandbox, t.Day, t.NewVisitors)
			if err != nil {
				return pgErr(err)
			}
		}
		return nil
	})
}

func (p *Postgres) TrafficOf(ctx context.Context, sandbox, since string) ([]Traffic, error) {
	rows, err := p.pool.Query(ctx, `SELECT t.sandbox, to_char(t.day, 'YYYY-MM-DD'), t.requests, t.status_2xx, t.status_3xx,
		t.status_4xx, t.status_5xx, t.websockets, t.bytes_sent, t.key_requests,
		(SELECT count(*) FROM sandbox_visitors v WHERE v.sandbox = t.sandbox AND v.day = t.day)
		FROM sandbox_traffic t WHERE t.sandbox = $1 AND t.day >= $2::date ORDER BY t.day`, sandbox, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Traffic, error) {
		var t Traffic
		return t, r.Scan(&t.Sandbox, &t.Day, &t.Requests, &t.Status2xx, &t.Status3xx, &t.Status4xx, &t.Status5xx,
			&t.WebSockets, &t.BytesSent, &t.KeyRequests, &t.Visitors)
	})
}

func (p *Postgres) PruneTraffic(ctx context.Context, before string) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM sandbox_visitors WHERE day < $1::date`, before); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM sandbox_traffic WHERE day < $1::date`, before)
		return err
	})
}

func (p *Postgres) VisitorSalt(ctx context.Context, day string) ([]byte, error) {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	var out []byte
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO visitor_salts (day, salt) VALUES ($1::date, $2) ON CONFLICT DO NOTHING`, day, salt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM visitor_salts WHERE day < $1::date`, day); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT salt FROM visitor_salts WHERE day = $1::date`, day).Scan(&out)
	})
	return out, err
}
