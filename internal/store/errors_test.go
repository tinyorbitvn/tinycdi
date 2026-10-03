// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeNetErr stands in for the refused/reset dial errors a dead Postgres
// produces without needing a socket.
type fakeNetErr struct{ timeout bool }

func (e fakeNetErr) Error() string   { return "fake net error" }
func (e fakeNetErr) Timeout() bool   { return e.timeout }
func (e fakeNetErr) Temporary() bool { return true }

func TestIsTransient(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"server-reported error is not an outage", &pgconn.PgError{Severity: "ERROR", Code: "23505", Message: "duplicate key"}, false},
		{"admin shutdown is the outage signature", &pgconn.PgError{Severity: "FATAL", Code: "57P01", Message: "terminating connection due to administrator command"}, true},
		{"crash shutdown", &pgconn.PgError{Severity: "FATAL", Code: "57P02", Message: "terminating connection due to server crash"}, true},
		{"no rows is a verdict not an outage", pgx.ErrNoRows, false},
		{"plain bug", errors.New("decode mismatch"), false},
		{"client cancel is not an outage", context.Canceled, false},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"conn closed", pgconn.ErrConnClosed, true},
		{"EOF", io.EOF, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"net timeout", fakeNetErr{timeout: true}, true},
		{"net reset", fmt.Errorf("read: %w", fakeNetErr{}), true},
		{"wrapped deadline", fmt.Errorf("query: %w", context.DeadlineExceeded), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransient(tc.err); got != tc.want {
				t.Fatalf("IsTransient(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsTransient_ConnectError exercises the real outage shape: a refused
// dial surfaces as *pgconn.ConnectError.
func TestIsTransient_ConnectError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgconn.Connect(ctx, "postgres://postgres:x@127.0.0.1:1/postgres")
	if err == nil {
		conn.Close(ctx)
		t.Fatal("connect to a dead port unexpectedly succeeded")
	}
	var connErr *pgconn.ConnectError
	if !errors.As(err, &connErr) {
		t.Fatalf("refused dial did not surface as ConnectError: %T %v", err, err)
	}
	if !IsTransient(err) {
		t.Fatalf("IsTransient(ConnectError) = false for %v", err)
	}
}

var _ net.Error = fakeNetErr{}
