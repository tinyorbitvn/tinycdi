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

	// Create needs IfMatchCreate (literal "*").
	if err := s.SetCeiling(ctx, "tenant-a", store.SessionPolicyWrite{Clipboard: strptr("Disabled")}, "anything"); !errors.Is(err, store.ErrSessionPolicyVersionMismatch) {
		t.Fatalf("create without *: %v", err)
	}
	if err := s.SetCeiling(ctx, "tenant-a", store.SessionPolicyWrite{Clipboard: strptr("Disabled")}, store.IfMatchCreate); err != nil {
		t.Fatalf("create: %v", err)
	}
	c, err := s.Ceiling(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if !c.HasRow || c.Clipboard == nil || *c.Clipboard != "Disabled" || c.Version == "" {
		t.Fatalf("created ceiling: %+v", c)
	}

	// Update needs the reported version; a stale one is 412-able.
	if err := s.SetCeiling(ctx, "tenant-a", store.SessionPolicyWrite{Clipboard: strptr("Send")}, "stale"); !errors.Is(err, store.ErrSessionPolicyVersionMismatch) {
		t.Fatalf("stale update: %v", err)
	}
	if err := s.SetCeiling(ctx, "tenant-a", store.SessionPolicyWrite{Clipboard: strptr("Send")}, store.IfMatchCreate); !errors.Is(err, store.ErrSessionPolicyVersionMismatch) {
		t.Fatalf("* is not a bypass on an existing row: %v", err)
	}
	if err := s.SetCeiling(ctx, "tenant-a", store.SessionPolicyWrite{Clipboard: strptr("Send")}, c.Version); err != nil {
		t.Fatalf("update: %v", err)
	}
	c2, err := s.Ceiling(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if c2.Clipboard == nil || *c2.Clipboard != "Send" || c2.Version == c.Version {
		t.Fatalf("updated ceiling: %+v (want Send, fresh version)", c2)
	}

	// An all-FollowTemplate write deletes the row.
	if err := s.SetCeiling(ctx, "tenant-a", store.SessionPolicyWrite{}, c2.Version); err != nil {
		t.Fatalf("delete-on-defaults: %v", err)
	}
	c3, err := s.Ceiling(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if c3.HasRow {
		t.Fatalf("all-FollowTemplate write must delete the row: %+v", c3)
	}
}

func TestSessionPolicy_MultiTenantIsolation(t *testing.T) {
	db := newDB(t)
	s := store.NewSessionPolicyStore(db)
	ctx := context.Background()

	if err := s.SetCeiling(ctx, "tenant-a", store.SessionPolicyWrite{Clipboard: strptr("Receive")}, store.IfMatchCreate); err != nil {
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

// Migration 024: the launch_ticket.session_policy column exists and a row
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
			 request_id, expires_at, session_policy)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now(), $9)`,
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
