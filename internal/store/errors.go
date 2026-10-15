// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"io"
	"net"

	"github.com/jackc/pgx/v5/pgconn"
)

// IsTransient reports whether err means PostgreSQL did not answer: a
// transport-level failure (refused or reset connect, a pooled connection
// that died before delivering an answer, a deadline) rather than a
// server-reported error or a client-side decode bug. API surfaces map it to
// 503 UNAVAILABLE — the request never reached a store verdict, so a
// retryable 5xx is the honest answer and the fail-closed rule is kept.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	// A PgError means the server evaluated the statement — a real
	// (permanent) failure such as a constraint violation. The exception is
	// the connection/shutdown class the server emits while it dies:
	// 57P01-57P03 arrive on pooled connections mid-outage and mean "the
	// server went away", not "your statement was wrong".
	if pgErr := pgErrorOf(err); pgErr != nil {
		switch pgErr.Code {
		case "57P01", // admin_shutdown
			"57P02", // crash_shutdown
			"57P03", // cannot_connect_now
			"08006": // connection_failure
			return true
		}
		return false
	}
	var connErr *pgconn.ConnectError
	switch {
	case errors.As(err, &connErr),
		errors.Is(err, pgconn.ErrConnClosed),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, io.EOF),
		errors.Is(err, io.ErrUnexpectedEOF),
		pgconn.SafeToRetry(err),
		pgconn.Timeout(err):
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// pgErrorOf extracts the server-reported error, if any.
func pgErrorOf(err error) *pgconn.PgError {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr
	}
	return nil
}
