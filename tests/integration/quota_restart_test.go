//go:build integration

package integration

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// goneObserver is a RuntimeObserver that always proves absence — the
// equivalent of the operator inventory reporting the workload deleted.
// startHash is the request-body fingerprint the signal calls carry; any
// fixed non-empty value works — replays must present the identical hash.
var startHash = provisioning.RequestHash("start")

type goneObserver struct{}

func (goneObserver) RuntimeGone(context.Context, provisioning.PlatformID) (bool, error) {
	return true, nil
}

// markDispatched flips every pending outbox intent of a workspace to
// dispatched so Recovery.SettleQuota must prove absence through the
// observer (ProofRuntimeAbsent) instead of the never-created shortcut.
func markDispatched(t *testing.T, db *store.DB, wsID string) {
	t.Helper()
	if _, err := db.Pool().Exec(context.Background(), `
		UPDATE outbox_intent SET dispatched_at = now()
		WHERE workspace_id = $1 AND dispatched_at IS NULL`, wsID); err != nil {
		t.Fatalf("mark dispatched %s: %v", wsID, err)
	}
}

// createStopped creates a workspace with desiredState=Stopped and releases
// its reservation the way the recovery pass does once runtime absence is
// proven (its create intent never dispatched -> ProofNeverCreated). The
// workspace then exists without holding quota — the state a stopped
// workspace settles into.
func createStopped(t *testing.T, svc *provisioning.Service, rec *provisioning.Recovery, db *store.DB, tenant, name string) provisioning.WorkspaceRecord {
	t.Helper()
	ctx := context.Background()
	req := provisioning.CreateRequest{
		OwnerIssuer:  "issuer",
		OwnerSubject: "sub-quota",
		Name:         name,
		Template:     provisioning.TemplateInfo{ID: "tpl_linuxdesktop", Name: "linux", Revision: 1, Runtime: "LinuxContainer", Experience: "Desktop"},
		Vector:       provisioning.ResourceVector{RunningSlots: 1, CPUMillis: 500, MemoryBytes: 1 << 30, DiskBytes: 1 << 30},
		DesiredState: "Stopped",
		DataPolicy:   "Ephemeral",
	}
	res, err := svc.CreateWorkspace(ctx, tenant, "create-"+name, req, provisioning.RequestHash(req))
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if err := rec.SettleQuota(ctx, tenant, provisioning.PlatformID(res.ID)); err != nil {
		t.Fatalf("settle %s: %v", name, err)
	}
	return res
}

// TestStartReacquiresQuota covers defect F7: POST
// /v1/workspaces/{id}/start on a stopped workspace must re-acquire the
// running-quota reservation that Stop + proven runtime absence released,
// in the same transaction as the start intent.
//
//   - quota=1, A running (holds), B stopped (released): start B fails
//     QUOTA_EXHAUSTED and records no intent.
//   - stop A alone does not free the slot — release needs absence proof;
//     start B still fails until the reservation is actually released.
//   - after A's runtime is proven gone, start B succeeds and re-holds.
//   - replaying the same start Idempotency-Key returns the stored result
//     without re-reserving.
func TestStartReacquiresQuota(t *testing.T) {
	db := newDB(t)
	svc := provisioning.NewService(db)
	rec := provisioning.NewRecovery(db, goneObserver{})
	tenant := "tenant-f7"
	ctx := context.Background()
	setQuota(t, db, tenant, provisioning.ResourceVector{RunningSlots: 1, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 1 << 40})
	owner := "issuer|sub-quota"

	// B exists stopped with its reservation already released.
	resB := createStopped(t, svc, rec, db, tenant, "f7-b")
	if held := heldSlots(t, db, tenant); held != 0 {
		t.Fatalf("held=%d after settling B, want 0", held)
	}

	// A running occupies the single slot.
	reqA := provisioning.CreateRequest{
		OwnerIssuer: "issuer", OwnerSubject: "sub-quota", Name: "f7-a",
		Template:     provisioning.TemplateInfo{ID: "tpl_linuxdesktop", Name: "linux", Revision: 1, Runtime: "LinuxContainer", Experience: "Desktop"},
		Vector:       provisioning.ResourceVector{RunningSlots: 1, CPUMillis: 500, MemoryBytes: 1 << 30, DiskBytes: 1 << 30},
		DesiredState: "Running", DataPolicy: "Ephemeral",
	}
	resA, err := svc.CreateWorkspace(ctx, tenant, "create-f7-a", reqA, provisioning.RequestHash(reqA))
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	markDispatched(t, db, resA.ID)
	if held := heldSlots(t, db, tenant); held != 1 {
		t.Fatalf("held=%d want 1 (A)", held)
	}

	// start B must fail: no headroom — and the failure must not leave a
	// start intent behind (same-transaction guarantee).
	if _, err := svc.SignalWorkspace(ctx, tenant, owner, owner, resB.ID, "start-b-1", provisioning.IntentStart, startHash); !provisioning.IsQuotaExceeded(err) {
		t.Fatalf("start B over quota: got %v, want QuotaExceededError", err)
	}
	if st, err := svc.GetWorkspace(ctx, tenant, owner, resB.ID); err != nil || st.DesiredState != "Stopped" {
		t.Fatalf("B after failed start: desired=%q err=%v, want Stopped", st.DesiredState, err)
	}

	// stop A: the reservation stays held until runtime absence is proven —
	// start B still fails in the window between intent and proof.
	if _, err := svc.SignalWorkspace(ctx, tenant, owner, owner, resA.ID, "stop-a-1", provisioning.IntentStop, startHash); err != nil {
		t.Fatalf("stop A: %v", err)
	}
	if _, err := svc.SignalWorkspace(ctx, tenant, owner, owner, resB.ID, "start-b-2", provisioning.IntentStart, startHash); !provisioning.IsQuotaExceeded(err) {
		t.Fatalf("start B after stop (pre-proof): got %v, want QuotaExceededError", err)
	}

	// Recovery proves A's runtime gone and releases its reservation.
	if err := rec.SettleQuota(ctx, tenant, provisioning.PlatformID(resA.ID)); err != nil {
		t.Fatalf("settle A: %v", err)
	}
	if held := heldSlots(t, db, tenant); held != 0 {
		t.Fatalf("held=%d after settling A, want 0", held)
	}

	// Now start B succeeds and re-holds the slot.
	st, err := svc.SignalWorkspace(ctx, tenant, owner, owner, resB.ID, "start-b-3", provisioning.IntentStart, startHash)
	if err != nil {
		t.Fatalf("start B after proof: %v", err)
	}
	if st.DesiredState != "Running" || st.Phase != "Provisioning" {
		t.Fatalf("B after start: %+v, want Running/Provisioning", st)
	}
	if held := heldSlots(t, db, tenant); held != 1 {
		t.Fatalf("held=%d after start B, want 1", held)
	}

	// Replay the same idempotency key: stored result, no double reserve.
	again, err := svc.SignalWorkspace(ctx, tenant, owner, owner, resB.ID, "start-b-3", provisioning.IntentStart, startHash)
	if err != nil || !again.Replayed {
		t.Fatalf("replayed start: err=%v replayed=%v", err, again.Replayed)
	}
	if held := heldSlots(t, db, tenant); held != 1 {
		t.Fatalf("held=%d after replayed start, want 1", held)
	}
}

// TestStartConcurrentNeverExceedsQuota: N released workspaces racing start
// under a quota of K — exactly K may re-hold; the rest get
// QuotaExceededError. The tenant_quota row lock serializes admission.
func TestStartConcurrentNeverExceedsQuota(t *testing.T) {
	db := newDB(t)
	svc := provisioning.NewService(db)
	rec := provisioning.NewRecovery(db, goneObserver{})
	tenant := "tenant-f7-conc"
	ctx := context.Background()
	setQuota(t, db, tenant, provisioning.ResourceVector{RunningSlots: 2, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 1 << 40})
	owner := "issuer|sub-quota"

	const n = 6
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		ids[i] = createStopped(t, svc, rec, db, tenant, fmt.Sprintf("f7c-%d", i)).ID
	}
	if held := heldSlots(t, db, tenant); held != 0 {
		t.Fatalf("held=%d want 0 after settling all", held)
	}

	var wg sync.WaitGroup
	var okCount, quotaCount, otherErr atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			_, err := svc.SignalWorkspace(c, tenant, owner, owner, ids[i], fmt.Sprintf("start-c-%d", i), provisioning.IntentStart, startHash)
			switch {
			case err == nil:
				okCount.Add(1)
			case provisioning.IsQuotaExceeded(err):
				quotaCount.Add(1)
			default:
				otherErr.Add(1)
				t.Errorf("start %d: unexpected error %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if okCount.Load() != 2 || quotaCount.Load() != int64(n-2) {
		t.Fatalf("starts: ok=%d quota=%d other=%d, want ok=2 quota=%d",
			okCount.Load(), quotaCount.Load(), otherErr.Load(), n-2)
	}
	if held := heldSlots(t, db, tenant); held != 2 {
		t.Fatalf("held=%d want 2", held)
	}
}

// TestStartOnRunningIsIdempotent: a start on an already-Running workspace
// with a held reservation is a no-op — it must not touch quota or append
// an intent.
func TestStartOnRunningIsIdempotent(t *testing.T) {
	db := newDB(t)
	svc := provisioning.NewService(db)
	tenant := "tenant-f7-noop"
	ctx := context.Background()
	setQuota(t, db, tenant, provisioning.ResourceVector{RunningSlots: 5, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 1 << 40})
	owner := "issuer|sub-quota"

	req := provisioning.CreateRequest{
		OwnerIssuer: "issuer", OwnerSubject: "sub-quota", Name: "f7-noop",
		Template:     provisioning.TemplateInfo{ID: "tpl_linuxdesktop", Name: "linux", Revision: 1, Runtime: "LinuxContainer", Experience: "Desktop"},
		Vector:       provisioning.ResourceVector{RunningSlots: 1, CPUMillis: 500, MemoryBytes: 1 << 30, DiskBytes: 1 << 30},
		DesiredState: "Running", DataPolicy: "Ephemeral",
	}
	res, err := svc.CreateWorkspace(ctx, tenant, "create-noop", req, provisioning.RequestHash(req))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	st, err := svc.SignalWorkspace(ctx, tenant, owner, owner, res.ID, "start-noop", provisioning.IntentStart, startHash)
	if err != nil {
		t.Fatalf("start on running: %v", err)
	}
	if st.DesiredState != "Running" {
		t.Fatalf("desired=%q want Running", st.DesiredState)
	}
	if held := heldSlots(t, db, tenant); held != 1 {
		t.Fatalf("held=%d want 1 (no double reserve)", held)
	}
	var intents int
	if err := db.Pool().QueryRow(ctx, `
		SELECT count(*) FROM outbox_intent WHERE workspace_id = $1`, res.ID).Scan(&intents); err != nil {
		t.Fatal(err)
	}
	if intents != 1 {
		t.Fatalf("intents=%d want 1 (create only; start was a no-op)", intents)
	}
}

// The handler-level mapping (QuotaExceededError -> 409 QUOTA_EXHAUSTED on
// POST /v1/workspaces/{id}/start) is covered in internal/api
// (workspaces_handlers_test.go: TestStartQuotaExhaustedMapping).
