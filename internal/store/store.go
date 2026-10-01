// Package store wraps pgx with the control-plane schema: pooled
// connections, transaction helpers and the embedded migration runner.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Tx is the transaction handle accepted by quota/outbox/idempotency
// operations so each multi-write request stays atomic.
type Tx = pgx.Tx

// DB is a pooled PostgreSQL handle.
type DB struct {
	pool *pgxpool.Pool
}

// Open connects to PostgreSQL at url and verifies the connection.
func Open(ctx context.Context, url string) (*DB, error) {
	pool, err := pgxpool.New(ctx, url)
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
