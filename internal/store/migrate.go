package store

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies embedded migrations in filename order. Each migration
// runs in its own transaction and is recorded in schema_migrations.
func (d *DB) Migrate(ctx context.Context) error {
	if _, err := d.pool.Exec(ctx, `
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
		if err := d.pool.QueryRow(ctx,
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
		if err := d.WithTx(ctx, func(tx Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return fmt.Errorf("store: apply %s %w", name, err)
			}
			_, err := tx.Exec(ctx,
				`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`,
				version, name)
			return err
		}); err != nil {
			return err
		}
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
