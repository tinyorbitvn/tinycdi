// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package broker_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func mintRow(t *testing.T, db *store.DB, id, ws, direction string, gen int64, rtUID string, expires time.Time) {
	t.Helper()
	iss, sub, _ := strings.Cut(alice.Owner(), "|")
	if err := store.InsertFileTransfer(context.Background(), db, store.FileTransfer{
		ID: id, WorkspaceID: ws, Tenant: "tenant-a", PrincipalIssuer: iss, PrincipalSubject: sub,
		RuntimeGeneration: gen, RuntimeUID: rtUID, Direction: direction, Path: "a.txt",
		DeclaredBytes: 10, ResolvedPolicy: "Bidirectional", ExpiresAt: expires,
	}); err != nil {
		t.Fatal(err)
	}
}

func policyMax(mode string, max int64) broker.TransferPolicyResolver {
	return func(context.Context, string, string) (broker.TransferPolicy, error) {
		return broker.TransferPolicy{Mode: mode, MaxBytes: max}, nil
	}
}

func policy(mode string) broker.TransferPolicyResolver { return policyMax(mode, 1<<20) }

func rowStatus(t *testing.T, db *store.DB, id string) string {
	t.Helper()
	var s string
	if err := db.Pool().QueryRow(context.Background(), `SELECT status FROM session_file_transfer WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// auditRows counts audit_event rows of one action and outcome.
func auditRows(t *testing.T, db *store.DB, action, outcome string) int {
	t.Helper()
	var n int
	if err := db.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM audit_event WHERE action = $1 AND outcome = $2`, action, outcome).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestClaimTransfer_HappyPathIsSingleUse(t *testing.T) {
	db, b, clock, src := setup(t)
	b.WithTransferPolicy(policy("Bidirectional"))
	lease, _ := seedActivityWorkspace(t, db, src, clock, "ws_xf1", 1, "uid-1", 1)
	mintRow(t, db, "xf_one", "ws_xf1", "upload", 1, "uid-1", clock.Now().Add(time.Minute))
	g, err := b.ClaimTransfer(context.Background(), gwA, lease, "xf_one", "PUT")
	if err != nil || g.Direction != "upload" || g.Path != "a.txt" || g.MaxBytes != 10 {
		t.Fatalf("grant = %+v, %v; upload cap is min(declared, policy)", g, err)
	}
	if _, err := b.ClaimTransfer(context.Background(), gwA, lease, "xf_one", "PUT"); !errors.Is(err, broker.ErrTransferInvalid) {
		t.Fatalf("second claim: %v, want ErrTransferInvalid", err)
	}
	if err := b.CompleteTransfer(context.Background(), gwA, lease, "xf_one", 10, true); err != nil {
		t.Fatal(err)
	}
	var details string
	_ = db.Pool().QueryRow(context.Background(),
		`SELECT details::text FROM audit_event WHERE action = 'file.transfer.upload' AND details->>'phase' = 'complete'`).Scan(&details)
	if rowStatus(t, db, "xf_one") != "completed" || auditRows(t, db, "file.transfer.upload", "success") != 2 ||
		!strings.Contains(details, `"bytes": "10"`) || !strings.Contains(details, `"declared": "10"`) {
		t.Fatalf("status %q, success audit rows %d, complete details %s; want completed, claim+complete with declared and transferred bytes",
			rowStatus(t, db, "xf_one"), auditRows(t, db, "file.transfer.upload", "success"), details)
	}
}

func TestClaimTransfer_PolicyTightenedAfterMint(t *testing.T) {
	db, b, clock, src := setup(t)
	b.WithTransferPolicy(policy("Download")) // the ceiling moved to Download after an upload was minted
	lease, _ := seedActivityWorkspace(t, db, src, clock, "ws_xf2", 1, "uid-1", 1)
	mintRow(t, db, "xf_up", "ws_xf2", "upload", 1, "uid-1", clock.Now().Add(time.Minute))
	if _, err := b.ClaimTransfer(context.Background(), gwA, lease, "xf_up", "PUT"); !errors.Is(err, broker.ErrTransferDenied) {
		t.Fatalf("claim: %v, want ErrTransferDenied", err)
	}
	if got := rowStatus(t, db, "xf_up"); got != "failed" {
		t.Fatalf("status %q, want failed (the denial is recorded, the row is spent)", got)
	}
	if n := auditRows(t, db, "file.transfer.upload", "denied"); n != 1 {
		t.Fatalf("denied audit rows = %d, want 1 (a refused transfer is as visible as a completed one)", n)
	}

	// The size ceiling shrank below the declared size: same refusal.
	b.WithTransferPolicy(policyMax("Bidirectional", 5))
	mintRow(t, db, "xf_big", "ws_xf2", "upload", 1, "uid-1", clock.Now().Add(time.Minute))
	if _, err := b.ClaimTransfer(context.Background(), gwA, lease, "xf_big", "PUT"); !errors.Is(err, broker.ErrTransferDenied) {
		t.Fatalf("claim over the tightened size: %v, want ErrTransferDenied", err)
	}
}

func TestClaimTransfer_RejectsMismatches(t *testing.T) {
	db, b, clock, src := setup(t)
	b.WithTransferPolicy(policy("Bidirectional"))
	lease, _ := seedActivityWorkspace(t, db, src, clock, "ws_xf3", 2, "uid-2", 1)
	seedActivityWorkspace(t, db, src, clock, "ws_other", 1, "uid-o", 1)
	mintRow(t, db, "xf_method", "ws_xf3", "download", 2, "uid-2", clock.Now().Add(time.Minute))
	mintRow(t, db, "xf_oldgen", "ws_xf3", "upload", 1, "uid-1", clock.Now().Add(time.Minute))
	mintRow(t, db, "xf_foreign", "ws_other", "upload", 1, "uid-o", clock.Now().Add(time.Minute))
	mintRow(t, db, "xf_expired", "ws_xf3", "upload", 2, "uid-2", clock.Now().Add(-time.Second))
	for id, method := range map[string]string{"xf_method": "PUT", "xf_oldgen": "PUT", "xf_foreign": "PUT", "xf_expired": "PUT", "xf_missing": "PUT"} {
		if _, err := b.ClaimTransfer(context.Background(), gwA, lease, id, method); !errors.Is(err, broker.ErrTransferInvalid) {
			t.Errorf("%s: %v, want ErrTransferInvalid", id, err)
		}
	}
	// A mismatch does not spend the row (the wrong-method or stale-incarnation
	// caller is not the grant's holder), and a foreign workspace's id is not
	// ours to audit.
	if rowStatus(t, db, "xf_method") != "pending" || rowStatus(t, db, "xf_foreign") != "pending" {
		t.Fatal("a mismatched claim spent the row")
	}
	if n := auditRows(t, db, "file.transfer.download", "denied") + auditRows(t, db, "file.transfer.upload", "denied"); n != 2 {
		t.Fatalf("denied audit rows = %d, want 2 (xf_method, xf_oldgen)", n)
	}
}

func TestClaimFileTransfer_OnePerWorkspaceConcurrent(t *testing.T) {
	db, b, clock, src := setup(t)
	b.WithTransferPolicy(policy("Bidirectional"))
	lease, _ := seedActivityWorkspace(t, db, src, clock, "ws_xf4", 1, "uid-1", 1)
	mintRow(t, db, "xf_a", "ws_xf4", "upload", 1, "uid-1", clock.Now().Add(time.Minute))
	mintRow(t, db, "xf_b", "ws_xf4", "upload", 1, "uid-1", clock.Now().Add(time.Minute))
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, id := range []string{"xf_a", "xf_b"} {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			_, errs[i] = b.ClaimTransfer(context.Background(), gwA, lease, id, "PUT")
		}(i, id)
	}
	wg.Wait()
	ok, busy := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, broker.ErrTransferBusy):
			busy++
		default:
			t.Fatalf("unexpected: %v", err)
		}
	}
	if ok != 1 || busy != 1 {
		t.Fatalf("ok=%d busy=%d, want exactly one stream per workspace", ok, busy)
	}
	if auditRows(t, db, "file.transfer.upload", "success") != 1 || auditRows(t, db, "file.transfer.upload", "denied") != 1 {
		t.Fatal("want one success and one denied (busy) audit row")
	}
}

func TestCompleteTransfer_BoundToLeaseWorkspaceAndSurvivesRevoke(t *testing.T) {
	db, b, clock, src := setup(t)
	b.WithTransferPolicy(policy("Bidirectional"))
	lease, _ := seedActivityWorkspace(t, db, src, clock, "ws_xf5", 1, "uid-1", 1)
	other, _ := seedActivityWorkspace(t, db, src, clock, "ws_xf6", 1, "uid-6", 1)
	mintRow(t, db, "xf_c", "ws_xf5", "download", 1, "uid-1", clock.Now().Add(time.Minute))
	if _, err := b.ClaimTransfer(context.Background(), gwA, lease, "xf_c", "GET"); err != nil {
		t.Fatal(err)
	}
	if err := b.CompleteTransfer(context.Background(), gwA, other, "xf_c", 5, true); !errors.Is(err, broker.ErrTransferInvalid) {
		t.Fatalf("complete from another workspace's lease: %v, want ErrTransferInvalid", err)
	}
	// The stream was authorized at claim: a revoke mid-stream must still let
	// the row close with its byte count.
	if err := b.RevokeLease(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	if err := b.CompleteTransfer(context.Background(), gwA, lease, "xf_c", 5, false); err != nil {
		t.Fatalf("complete after revoke: %v", err)
	}
	if err := b.CompleteTransfer(context.Background(), gwA, lease, "xf_c", 5, true); !errors.Is(err, broker.ErrTransferInvalid) {
		t.Fatalf("second complete: %v, want ErrTransferInvalid", err)
	}
	if rowStatus(t, db, "xf_c") != "failed" || auditRows(t, db, "file.transfer.download", "failure") != 1 {
		t.Fatal("an ok=false completion must record status failed and a failure audit row")
	}
}
