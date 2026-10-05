// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

// Real-Postgres tests for `backend post-restore` — one per step plus
// idempotency and the freeze guard, on the shared pg_test.go harness.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// fakeCRSource is a liveCRSource the tests drive directly.
type fakeCRSource struct {
	crs []liveCR
	err error
}

func (f fakeCRSource) List(context.Context) ([]liveCR, error) { return f.crs, f.err }

// seedSession inserts a live session row bound to the current epoch.
func seedSession(t *testing.T, db *store.DB, id string) {
	t.Helper()
	_, err := db.Pool().Exec(context.Background(), `
		INSERT INTO sessions (id, issuer, subject, tenant_id, created_at, last_seen_at, epoch)
		VALUES ($1, 'iss', 'sub', 'tenant-a', now(), now(),
			(SELECT value FROM platform_meta WHERE key = 'session_epoch'))`, id)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

// seedLease inserts an 'active' connection_lease row.
func seedLease(t *testing.T, db *store.DB, id, wsUID string) {
	t.Helper()
	_, err := db.Pool().Exec(context.Background(), `
		INSERT INTO connection_lease
			(id, workspace_id, tenant_id, principal_subject, runtime_generation,
			 runtime_uid, fencing_version, gateway_id, state, expires_at)
		VALUES ($1, $2, 'tenant-a', 'iss|sub', 1, 'rt-1', 1, 'backend', 'active', now() + interval '30 seconds')`,
		id, wsUID)
	if err != nil {
		t.Fatalf("seed lease: %v", err)
	}
}

// seedTicket inserts an unconsumed, unrevoked launch_ticket row.
func seedTicket(t *testing.T, db *store.DB, hash byte, wsUID string) {
	t.Helper()
	_, err := db.Pool().Exec(context.Background(), `
		INSERT INTO launch_ticket
			(ticket_hash, workspace_id, tenant_id, principal_subject,
			 runtime_generation, runtime_uid, audience, request_id, expires_at)
		VALUES (decode(md5($1::text), 'hex'),
			$2, 'tenant-a', 'iss|sub', 1, 'rt-1', 'aud', 'req-t', now() + interval '60 seconds')`,
		fmt.Sprintf("t%d", hash), wsUID)
	if err != nil {
		t.Fatalf("seed ticket: %v", err)
	}
}

// seedWorkspaceFull inserts a workspace row with explicit intent fields.
func seedWorkspaceFull(t *testing.T, db *store.DB, wsUID, desired string, gen, rev int64) {
	t.Helper()
	seedWorkspace(t, db, "tenant-a", "sub", wsUID)
	_, err := db.Pool().Exec(context.Background(), `
		UPDATE workspaces SET desired_state=$2, runtime_generation=$3, intent_revision=$4
		 WHERE id=$1`, wsUID, desired, gen, rev)
	if err != nil {
		t.Fatalf("seed workspace fields: %v", err)
	}
}

func scalarInt(t *testing.T, db *store.DB, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.Pool().QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("scalar %q: %v", q, err)
	}
	return n
}

func scalarStr(t *testing.T, db *store.DB, q string, args ...any) string {
	t.Helper()
	var s string
	if err := db.Pool().QueryRow(context.Background(), q, args...).Scan(&s); err != nil {
		t.Fatalf("scalar %q: %v", q, err)
	}
	return s
}

var testTenants = provisioning.TenantNamespaces{"tenant-a": "ns-a"}

func TestPostRestoreDryRunMakesNoWrites(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	seedWorkspaceFull(t, db, "ws_aaaa", "Running", 1, 1)
	seedSession(t, db, "sess-1")
	seedLease(t, db, "lease-1", "ws_aaaa")
	seedTicket(t, db, 0x11, "ws_aaaa")
	epochBefore := scalarStr(t, db, `SELECT value FROM platform_meta WHERE key='session_epoch'`)

	var out, errOut bytes.Buffer
	code := postRestore(ctx, db, fakeCRSource{crs: []liveCR{{
		uid: "ws_aaaa", namespace: "ns-a", name: "ws-aaaa",
		desiredState: "Running", runtimeGeneration: 1, intentRevision: 1,
	}}}, nil, testTenants, false, &out, &errOut, testLog())
	if code != 0 {
		t.Fatalf("dry-run exit %d: %s", code, errOut.String())
	}
	for _, want := range []string{
		"sessions to rotate (session_epoch): 1",
		"active leases to revoke:            1",
		"unconsumed tickets to deny:         1",
		"0 rows behind",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("dry-run plan missing %q:\n%s", want, out.String())
		}
	}
	if got := scalarStr(t, db, `SELECT value FROM platform_meta WHERE key='session_epoch'`); got != epochBefore {
		t.Fatal("dry-run rotated the session epoch")
	}
	if got := scalarInt(t, db, `SELECT count(*) FROM connection_lease WHERE state='active'`); got != 1 {
		t.Fatal("dry-run revoked a lease")
	}
	if got := scalarInt(t, db, `SELECT count(*) FROM launch_ticket WHERE consumed_at IS NULL AND revoked_at IS NULL`); got != 1 {
		t.Fatal("dry-run denied a ticket")
	}
}

func TestPostRestoreApplyRotatesRevokesDenies(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	seedWorkspaceFull(t, db, "ws_aaaa", "Running", 1, 1)
	seedSession(t, db, "sess-1")
	seedLease(t, db, "lease-1", "ws_aaaa")
	seedTicket(t, db, 0x22, "ws_aaaa")
	epochBefore := scalarStr(t, db, `SELECT value FROM platform_meta WHERE key='session_epoch'`)

	var out, errOut bytes.Buffer
	code := postRestore(ctx, db, fakeCRSource{crs: []liveCR{{
		uid: "ws_aaaa", namespace: "ns-a", name: "ws-aaaa",
		desiredState: "Running", runtimeGeneration: 1, intentRevision: 1,
	}}}, nil, testTenants, true, &out, &errOut, testLog())
	if code != 0 {
		t.Fatalf("apply exit %d: %s", code, errOut.String())
	}
	if got := scalarStr(t, db, `SELECT value FROM platform_meta WHERE key='session_epoch'`); got == epochBefore {
		t.Fatal("epoch was not rotated")
	}
	if got := scalarInt(t, db,
		`SELECT count(*) FROM sessions WHERE epoch = (SELECT value FROM platform_meta WHERE key='session_epoch')`); got != 0 {
		t.Fatalf("%d sessions still live at the new epoch", got)
	}
	if got := scalarInt(t, db, `SELECT count(*) FROM connection_lease WHERE state='active'`); got != 0 {
		t.Fatalf("%d leases still active", got)
	}
	if got := scalarStr(t, db, `SELECT state FROM connection_lease WHERE id='lease-1'`); got != "revoked" {
		t.Fatalf("lease state %q", got)
	}
	if got := scalarInt(t, db, `SELECT count(*) FROM launch_ticket WHERE consumed_at IS NULL AND revoked_at IS NULL`); got != 0 {
		t.Fatalf("%d tickets still armed", got)
	}
}

func TestPostRestoreApplyIdempotent(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	seedWorkspaceFull(t, db, "ws_aaaa", "Running", 1, 1)
	seedLease(t, db, "lease-1", "ws_aaaa")
	seedTicket(t, db, 0x33, "ws_aaaa")
	src := fakeCRSource{crs: []liveCR{{
		uid: "ws_aaaa", namespace: "ns-a", name: "ws-aaaa",
		desiredState: "Stopped", runtimeGeneration: 2, intentRevision: 4,
	}}}

	for i := 0; i < 2; i++ {
		var out, errOut bytes.Buffer
		if code := postRestore(ctx, db, src, nil, testTenants, true, &out, &errOut, testLog()); code != 0 {
			t.Fatalf("apply run %d exit %d: %s", i, code, errOut.String())
		}
	}
	var rev, gen int64
	var desired string
	if err := db.Pool().QueryRow(ctx,
		`SELECT desired_state, runtime_generation, intent_revision FROM workspaces WHERE id='ws_aaaa'`).
		Scan(&desired, &gen, &rev); err != nil {
		t.Fatal(err)
	}
	if rev != 4 || gen != 2 || desired != "Stopped" {
		t.Fatalf("row not aligned: rev=%d gen=%d desired=%s", rev, gen, desired)
	}
	if got := scalarInt(t, db, `SELECT count(*) FROM connection_lease WHERE state='active'`); got != 0 {
		t.Fatal("second run left an active lease")
	}
}

func TestPostRestoreAlignsDivergedRow(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	seedWorkspaceFull(t, db, "ws_aaaa", "Running", 1, 1)
	seedWorkspaceFull(t, db, "ws_bbbb", "Running", 3, 3)
	src := fakeCRSource{crs: []liveCR{
		{uid: "ws_aaaa", namespace: "ns-a", name: "ws-aaaa",
			desiredState: "Stopped", runtimeGeneration: 2, intentRevision: 4},
		// ws_bbbb has no CR: ghost. ws_cccc is an orphan CR.
		{uid: "ws_cccc", namespace: "ns-a", name: "ws-cccc",
			desiredState: "Running", runtimeGeneration: 1, intentRevision: 1},
	}}

	var out, errOut bytes.Buffer
	if code := postRestore(ctx, db, src, nil, testTenants, true, &out, &errOut, testLog()); code != 0 {
		t.Fatalf("apply exit %d: %s", code, errOut.String())
	}
	var rev int64
	var desired string
	if err := db.Pool().QueryRow(ctx,
		`SELECT desired_state, intent_revision FROM workspaces WHERE id='ws_aaaa'`).
		Scan(&desired, &rev); err != nil {
		t.Fatal(err)
	}
	if rev != 4 || desired != "Stopped" {
		t.Fatalf("diverged row not aligned: rev=%d desired=%s", rev, desired)
	}
	if !strings.Contains(out.String(), "1 rows behind, 1 ghost rows") ||
		!strings.Contains(out.String(), "1 orphan CRs") {
		t.Fatalf("reconcile counts missing:\n%s", out.String())
	}
	// Ghost row untouched.
	if got := scalarInt(t, db, `SELECT count(*) FROM workspaces WHERE id='ws_bbbb' AND state='active'`); got != 1 {
		t.Fatal("ghost row was modified")
	}
}

func TestPostRestoreRowAheadOfCRIsAnomaly(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	seedWorkspaceFull(t, db, "ws_aaaa", "Running", 5, 9)
	src := fakeCRSource{crs: []liveCR{{
		uid: "ws_aaaa", namespace: "ns-a", name: "ws-aaaa",
		desiredState: "Stopped", runtimeGeneration: 2, intentRevision: 4,
	}}}
	var out, errOut bytes.Buffer
	if code := postRestore(ctx, db, src, nil, testTenants, true, &out, &errOut, testLog()); code != 0 {
		t.Fatalf("apply exit %d: %s", code, errOut.String())
	}
	if got := scalarInt(t, db, `SELECT intent_revision FROM workspaces WHERE id='ws_aaaa'`); got != 9 {
		t.Fatal("row ahead of its CR was rewritten downwards")
	}
	if !strings.Contains(out.String(), "AHEAD") {
		t.Fatalf("anomaly not reported:\n%s", out.String())
	}
}

func TestPostRestoreRefusesWhileLeaderHeld(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	seedWorkspaceFull(t, db, "ws_aaaa", "Running", 1, 1)
	seedLease(t, db, "lease-1", "ws_aaaa")

	// Hold the leader lock on a dedicated pool connection, exactly like the
	// backend's singleton leader does.
	conn, err := db.Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, leaderLockKey).Scan(&ok); err != nil || !ok {
		t.Fatalf("take leader lock: %v held=%v", err, ok)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, leaderLockKey) }()

	var out, errOut bytes.Buffer
	code := postRestore(ctx, db, fakeCRSource{}, nil, testTenants, true, &out, &errOut, testLog())
	if code != 1 {
		t.Fatalf("expected refusal exit 1, got %d", code)
	}
	if !strings.Contains(errOut.String(), "leader lock") {
		t.Fatalf("refusal message missing: %s", errOut.String())
	}
	if got := scalarInt(t, db, `SELECT count(*) FROM connection_lease WHERE state='active'`); got != 1 {
		t.Fatal("refused run still revoked a lease")
	}
}

func TestPostRestoreNoKubeAccessPrintsSQL(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	seedWorkspaceFull(t, db, "ws_aaaa", "Running", 1, 1)

	var out, errOut bytes.Buffer
	code := postRestore(ctx, db, nil, errors.New("in-cluster config: no service account token"),
		testTenants, true, &out, &errOut, testLog())
	if code != postRestoreExitKubeUnavailable {
		t.Fatalf("expected exit %d, got %d", postRestoreExitKubeUnavailable, code)
	}
	for _, want := range []string{
		"UPDATE workspaces", "ws_aaaa", "intent_revision",
		"kubectl -n ns-a get workspace ws-aaaa",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("fallback SQL missing %q:\n%s", want, out.String())
		}
	}
}

func TestPostRestoreFlagValidation(t *testing.T) {
	getenv := func(string) string { return "" }
	if _, err := parsePostRestoreFlags([]string{"-apply"}, getenv); err == nil ||
		!strings.Contains(err.Error(), "i-have-scaled-down") {
		t.Fatalf("-apply without ack: %v", err)
	}
	if _, err := parsePostRestoreFlags([]string{"-apply", "-dry-run", "-i-have-scaled-down"}, getenv); err == nil {
		t.Fatal("-apply -dry-run accepted")
	}
	if _, err := parsePostRestoreFlags([]string{"-i-have-scaled-down"}, getenv); err == nil {
		t.Fatal("ack without -apply accepted")
	}
	if _, err := parsePostRestoreFlags(nil, getenv); err == nil {
		t.Fatal("missing -database-url accepted")
	}
	c, err := parsePostRestoreFlags([]string{"-database-url", "postgres://x"}, getenv)
	if err != nil || c.apply {
		t.Fatalf("default mode: %v %+v", err, c)
	}
}
