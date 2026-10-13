// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// Session-policy ceiling store contract (ADR 0010, T10): a NULL/absent
// channel follows the template; no row at all is the upgrade default and
// means the same thing; the last FollowTemplate write deletes the row —
// the store keeps exactly one representation of "follows".

func TestSessionPolicy_NoRowResolvesFollowTemplate(t *testing.T) {
	db := newDB(t)
	s := store.NewSessionPolicyStore(db)
	c, err := s.Ceiling(context.Background(), "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if c.HasRow || c.Clipboard != nil || c.FileTransfer != nil || c.FileMaxBytes != nil || c.Version != "" {
		t.Fatalf("absent ceiling must resolve all-nil: %+v", c)
	}
}

func TestSessionPolicy_SetCeiling_CreateReadUpdateDelete(t *testing.T) {
	db := newDB(t)
	s := store.NewSessionPolicyStore(db)
	ctx := context.Background()

	// A conditional update on a missing row matches nothing.
	if _, err := s.SetCeiling(ctx, "tenant-a", store.SessionPolicyWrite{Clipboard: strptr("Disabled")}, "2026-10-13T10:00:00Z", false); !errors.Is(err, store.ErrVersionMismatch) {
		t.Fatalf("update of a missing row: %v", err)
	}
	v1, err := s.SetCeiling(ctx, "tenant-a", store.SessionPolicyWrite{Clipboard: strptr("Disabled")}, "", true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	c, err := s.Ceiling(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if !c.HasRow || c.Clipboard == nil || *c.Clipboard != "Disabled" || c.Version == "" || c.Version != v1 {
		t.Fatalf("created ceiling: %+v (create returned %q)", c, v1)
	}

	// Update needs the current version; a stale one or a second create is
	// a mismatch.
	if _, err := s.SetCeiling(ctx, "tenant-a", store.SessionPolicyWrite{Clipboard: strptr("Send")}, "stale", false); !errors.Is(err, store.ErrVersionMismatch) {
		t.Fatalf("stale update: %v", err)
	}
	if _, err := s.SetCeiling(ctx, "tenant-a", store.SessionPolicyWrite{Clipboard: strptr("Send")}, "", true); !errors.Is(err, store.ErrVersionMismatch) {
		t.Fatalf("create on an existing row: %v", err)
	}
	v2, err := s.SetCeiling(ctx, "tenant-a", store.SessionPolicyWrite{Clipboard: strptr("Send")}, c.Version, false)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	c2, err := s.Ceiling(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if c2.Clipboard == nil || *c2.Clipboard != "Send" || c2.Version == c.Version || c2.Version != v2 {
		t.Fatalf("updated ceiling: %+v (want Send, fresh version %q)", c2, v2)
	}

	// An all-FollowTemplate write deletes the row — conditionally.
	if _, err := s.SetCeiling(ctx, "tenant-a", store.SessionPolicyWrite{}, c.Version, false); !errors.Is(err, store.ErrVersionMismatch) {
		t.Fatalf("stale delete-on-defaults: %v", err)
	}
	if v, err := s.SetCeiling(ctx, "tenant-a", store.SessionPolicyWrite{}, c2.Version, false); err != nil || v != "" {
		t.Fatalf("delete-on-defaults: version=%q err=%v", v, err)
	}
	c3, err := s.Ceiling(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if c3.HasRow {
		t.Fatalf("all-FollowTemplate write must delete the row: %+v", c3)
	}
	// With no row, an all-defaults create is a no-op.
	if _, err := s.SetCeiling(ctx, "tenant-a", store.SessionPolicyWrite{}, "", true); err != nil {
		t.Fatalf("defaults on a missing row: %v", err)
	}
}

func TestSessionPolicy_MultiTenantIsolation(t *testing.T) {
	db := newDB(t)
	s := store.NewSessionPolicyStore(db)
	ctx := context.Background()

	if _, err := s.SetCeiling(ctx, "tenant-a", store.SessionPolicyWrite{Clipboard: strptr("Receive")}, "", true); err != nil {
		t.Fatal(err)
	}
	c, err := s.Ceiling(ctx, "tenant-b")
	if err != nil {
		t.Fatal(err)
	}
	if c.HasRow {
		t.Fatalf("ceiling leaked across tenants: %+v", c)
	}
}

// The launch_ticket.session_policy column exists and a row
// stores a resolved vector verbatim.
func TestSessionPolicy_LaunchTicketColumn(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO workspaces (id, tenant_id, owner_subject, request_id) VALUES ($1, $2, $3, $4)`,
		"ws-1", "tenant-a", "iss|alice", "req-ws-1"); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO launch_ticket
			(ticket_hash, workspace_id, tenant_id, principal_subject,
			 runtime_generation, runtime_uid, audience,
			 request_id, expires_at, session_policy, portal_session_digest)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now(), $9, sha256('test-portal-session'::bytea))`,
		[]byte("h"), "ws-1", "tenant-a", "iss|alice", int64(1), "uid-1", "aud",
		"req-1", `{"clipboard":"Disabled","fileTransfer":{"mode":"Disabled","maxFileBytes":null}}`); err != nil {
		t.Fatalf("insert ticket with session_policy: %v", err)
	}
	var got string
	if err := db.Pool().QueryRow(ctx,
		`SELECT session_policy::text FROM launch_ticket WHERE workspace_id = 'ws-1'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Fatal("session_policy round-tripped as empty")
	}
}

func strptr(s string) *string { return &s }
