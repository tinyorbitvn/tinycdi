package provisioning_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

type revokeCall struct {
	ws  string
	gen int64
}

func runningWorkspace(t *testing.T, db *store.DB, svc *provisioning.Service, tenant, name string) provisioning.WorkspaceRecord {
	t.Helper()
	ctx := context.Background()
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.SetQuota(ctx, tx, tenant, provisioning.ResourceVector{
			RunningSlots: 4, CPUMillis: 8000, MemoryBytes: 16 << 30, DiskBytes: 40 << 30})
	}); err != nil {
		t.Fatal(err)
	}
	req := provisioning.CreateRequest{
		OwnerIssuer: "iss", OwnerSubject: "sub", Name: name,
		Template: provisioning.TemplateInfo{ID: "tpl_x", Name: "x", Runtime: "linux", Experience: "desktop"},
		Vector:   quotaVec, DesiredState: "Running", DataPolicy: "Retain",
	}
	rec, err := svc.CreateWorkspace(ctx, tenant, "key-"+name, req, provisioning.RequestHash(req))
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func generationOf(t *testing.T, db *store.DB, ws string) int64 {
	t.Helper()
	var g int64
	if err := db.Pool().QueryRow(context.Background(), `SELECT runtime_generation FROM workspaces WHERE id = $1`, ws).Scan(&g); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestRestartWorkspace_BumpsGenerationRevokesOldAndAppendsStart(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	var calls []revokeCall
	svc := provisioning.NewService(db).WithGenerationRevoker(func(_ context.Context, _ store.Tx, ws string, gen int64) error {
		calls = append(calls, revokeCall{ws, gen})
		return nil
	})
	rec := runningWorkspace(t, db, svc, "tenant-r", "r1")
	g0 := generationOf(t, db, rec.ID)
	out, err := svc.RestartWorkspace(ctx, "tenant-r", "iss|sub", "iss|sub", rec.ID, "key-restart-1", g0, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if out.DesiredState != "Running" || generationOf(t, db, rec.ID) != g0+1 {
		t.Fatalf("record %+v gen %d, want Running and %d", out, generationOf(t, db, rec.ID), g0+1)
	}
	if len(calls) != 1 || calls[0] != (revokeCall{rec.ID, g0}) {
		t.Fatalf("revocations = %+v, want the old generation %d", calls, g0)
	}
	var start *provisioning.Intent
	pending := mustPending(t, db, rec.ID)
	for i := range pending {
		if pending[i].Kind == provisioning.IntentStart {
			start = &pending[i]
		}
	}
	if start == nil || start.RuntimeGeneration != g0+1 || start.DesiredState != "Running" {
		t.Fatalf("start intent = %+v", start)
	}
}

func TestRestartWorkspace_KeyReplaysAcrossRestartAndPlainStart(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	revokes := 0
	svc := provisioning.NewService(db).WithGenerationRevoker(func(context.Context, store.Tx, string, int64) error {
		revokes++
		return nil
	})
	rec := runningWorkspace(t, db, svc, "tenant-k", "k1")
	g0 := generationOf(t, db, rec.ID)
	if _, err := svc.RestartWorkspace(ctx, "tenant-k", "iss|sub", "iss|sub", rec.ID, "key-replay-01", g0, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	// The same request retried (a lost response). The CR has left Failed by
	// now, so the API routes it to the plain start path: the stored result
	// must come back, not an IDEMPOTENCY_CONFLICT, and nothing bumps again.
	again, err := svc.SignalWorkspace(ctx, "tenant-k", "iss|sub", "iss|sub", rec.ID, "key-replay-01", provisioning.IntentStart, []byte("{}"))
	if err != nil || !again.Replayed {
		t.Fatalf("plain start replay = %+v, %v; want the recorded result", again, err)
	}
	again, err = svc.RestartWorkspace(ctx, "tenant-k", "iss|sub", "iss|sub", rec.ID, "key-replay-01", g0, []byte("{}"))
	if err != nil || !again.Replayed {
		t.Fatalf("restart replay = %+v, %v; want the recorded result", again, err)
	}
	if g := generationOf(t, db, rec.ID); g != g0+1 || revokes != 1 {
		t.Fatalf("generation %d revokes %d after replays, want %d and 1", g, revokes, g0+1)
	}
}

func TestRestartWorkspace_GenerationFenced(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	var mu sync.Mutex
	revokes := 0
	svc := provisioning.NewService(db).WithGenerationRevoker(func(context.Context, store.Tx, string, int64) error {
		mu.Lock()
		revokes++
		mu.Unlock()
		return nil
	})
	rec := runningWorkspace(t, db, svc, "tenant-f", "f1")
	g0 := generationOf(t, db, rec.ID)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ { // a double click: two different idempotency keys, same observed generation
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.RestartWorkspace(ctx, "tenant-f", "iss|sub", "iss|sub", rec.ID, "key-dbl-"+string(rune('a'+i)), g0, []byte("{}"))
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("restart: %v", err)
		}
	}
	if g := generationOf(t, db, rec.ID); g != g0+1 || revokes != 1 {
		t.Fatalf("generation %d revokes %d, want exactly one bump to %d", g, revokes, g0+1)
	}
}

func TestRestartWorkspace_RefusesStoppedAndForeignAndRollsBackOnRevokeError(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	failing := errors.New("revoke failed")
	fail := false
	svc := provisioning.NewService(db).WithGenerationRevoker(func(context.Context, store.Tx, string, int64) error {
		if fail {
			return failing
		}
		return nil
	})
	rec := runningWorkspace(t, db, svc, "tenant-s", "s1")
	g0 := generationOf(t, db, rec.ID)
	if _, err := svc.RestartWorkspace(ctx, "tenant-s", "iss|eve", "iss|eve", rec.ID, "key-foreign", g0, []byte("{}")); !errors.Is(err, provisioning.ErrWorkspaceNotFound) {
		t.Fatalf("foreign caller: %v, want ErrWorkspaceNotFound", err)
	}
	fail = true
	if _, err := svc.RestartWorkspace(ctx, "tenant-s", "iss|sub", "iss|sub", rec.ID, "key-rb", g0, []byte("{}")); !errors.Is(err, failing) {
		t.Fatalf("revoke error: %v", err)
	}
	if generationOf(t, db, rec.ID) != g0 {
		t.Fatal("a failed revoke must roll back the generation bump")
	}
	fail = false
	if _, err := svc.SignalWorkspace(ctx, "tenant-s", "iss|sub", "", rec.ID, "key-s-stop", provisioning.IntentStop, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RestartWorkspace(ctx, "tenant-s", "iss|sub", "iss|sub", rec.ID, "key-s-r", generationOf(t, db, rec.ID), []byte("{}")); !errors.Is(err, provisioning.ErrNotRunning) {
		t.Fatalf("stopped: %v, want ErrNotRunning", err)
	}
}

func TestListRunningWorkspaces_TenantScopedRunningOnly(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	svc := provisioning.NewService(db)
	a := runningWorkspace(t, db, svc, "tenant-l", "l1")
	b := runningWorkspace(t, db, svc, "tenant-l", "l2")
	runningWorkspace(t, db, svc, "tenant-other", "o1")
	if _, err := svc.SignalWorkspace(ctx, "tenant-l", "iss|sub", "", b.ID, "key-l-stop", provisioning.IntentStop, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	got, err := svc.ListRunningWorkspaces(ctx, "tenant-l", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("running = %+v, want only %s", got, a.ID)
	}
	if want := generationOf(t, db, a.ID); got[0].RuntimeGeneration != want {
		t.Fatalf("RuntimeGeneration = %d, want %d", got[0].RuntimeGeneration, want)
	}
	capped, _ := svc.ListRunningWorkspaces(ctx, "tenant-l", 0)
	if len(capped) != 0 {
		t.Fatal("limit 0 returns nothing")
	}
}
