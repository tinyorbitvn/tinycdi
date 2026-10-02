package store

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrateLockKey is the session advisory lock that serialises Migrate across
// replicas ("tcdi" + 0x01). The backend's leader lock uses a different key.
const migrateLockKey int64 = 0x7463_6469_0001

// Migrate applies embedded migrations in filename order. Each migration
// runs in its own transaction and is recorded in schema_migrations. Replicas
// start together, so the whole call runs on one dedicated connection holding
// a session advisory lock: concurrent callers queue and find every version
// already applied.
func (d *DB) Migrate(ctx context.Context) (err error) {
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("store: migrate connection %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockKey); err != nil {
		return fmt.Errorf("store: migrate lock %w", err)
	}
	defer func() {
		// Unlock on a fresh context: ctx may already be cancelled. If the
		// unlock fails the connection is closed instead of being returned to
		// the pool, which releases the session lock.
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, uerr := conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, migrateLockKey); uerr != nil {
			_ = conn.Conn().Close(uctx)
		}
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    int PRIMARY KEY,
			name       text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("store: schema_migrations %w", err)
	}

	names, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("store: read migrations dir %w", err)
	}
	var files []string
	for _, e := range names {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	for _, name := range files {
		version, err := migrationVersion(name)
		if err != nil {
			return err
		}
		var applied bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`,
			version).Scan(&applied); err != nil {
			return fmt.Errorf("store: check migration %s %w", name, err)
		}
		if applied {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("store: read %s %w", name, err)
		}
		if err := applyMigration(ctx, conn.Conn(), name, version, string(body)); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, conn *pgx.Conn, name string, version int, body string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin %w", err)
	}
	if _, err := tx.Exec(ctx, body); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("store: apply %s %w", name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`,
		version, name); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit %w", err)
	}
	return nil
}

func migrationVersion(name string) (int, error) {
	prefix, _, ok := strings.Cut(name, "_")
	if !ok {
		return 0, fmt.Errorf("store: bad migration name %q", name)
	}
	v, err := strconv.Atoi(prefix)
	if err != nil {
		return 0, fmt.Errorf("store: bad migration version %q %w", name, err)
	}
	return v, nil
}
