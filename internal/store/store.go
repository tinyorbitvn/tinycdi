// Package store wraps pgx with the control-plane schema: pooled
// connections, transaction helpers and the embedded migration runner.
package store

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxAppNameLen is Postgres' application_name limit (NAMEDATALEN-1); the
// server silently truncates beyond it, so composed names are cut here
// first to control where the cut lands.
const maxAppNameLen = 63

// Tx is the transaction handle accepted by quota/outbox/idempotency
// operations so each multi-write request stays atomic.
type Tx = pgx.Tx

// DB is a pooled PostgreSQL handle.
type DB struct {
	pool *pgxpool.Pool
}

// Open connects to PostgreSQL at url and verifies the connection.
func Open(ctx context.Context, url string) (*DB, error) {
	return OpenWithAppName(ctx, url, "")
}

// OpenWithAppName connects like Open, stamping every pool connection's
// application_name so pg_stat_activity can tell components apart (the
// post-restore tool relies on it to spot live backends). A name already
// set in the DSN is composed as a suffix — "appName/dsn-name" — so the
// binary's identity leads (prefix matchers such as the post-restore guard
// still see it) while a per-caller tag survives; the restart drill uses
// that tag to sever one replica's connections. Postgres keeps only the
// first 63 bytes of application_name, so the composed name is truncated
// there (on a rune boundary), keeping the appName prefix whole.
func OpenWithAppName(ctx context.Context, url, appName string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("store: parse/connect %w", err)
	}
	if appName != "" {
		if cfg.ConnConfig.RuntimeParams == nil {
			cfg.ConnConfig.RuntimeParams = map[string]string{}
		}
		if dsnName := cfg.ConnConfig.RuntimeParams["application_name"]; dsnName != "" {
			appName += "/" + dsnName
		}
		if len(appName) > maxAppNameLen {
			n := maxAppNameLen
			for n > 0 && !utf8.RuneStart(appName[n]) {
				n--
			}
			appName = appName[:n]
		}
		cfg.ConnConfig.RuntimeParams["application_name"] = appName
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: parse/connect %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping %w", err)
	}
	return &DB{pool: pool}, nil
}

// Close releases the pool.
func (d *DB) Close() { d.pool.Close() }

// Pool exposes the underlying pool for single-statement queries.
func (d *DB) Pool() *pgxpool.Pool { return d.pool }

// WithTx runs fn inside a transaction, committing on success and rolling
// back on error or panic.
func (d *DB) WithTx(ctx context.Context, fn func(Tx) error) (err error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit %w", err)
	}
	return nil
}
