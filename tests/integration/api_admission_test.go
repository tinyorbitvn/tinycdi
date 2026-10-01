//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacev1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/api"
	"github.com/tinyorbitvn/tinycdi/internal/api/loginstate"
	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// fakeApplier stands in for the Kubernetes side (the suite wires the real
// Workspace CR writer behind the same contract). It dedups creates by
// deterministic request ID and counts runtimes so tests can prove
// exactly-once convergence.
type fakeApplier struct {
	mu       sync.Mutex
	ws       map[string]*fakeWorkspace
	byReqID  map[string]string
	gates    map[string]chan struct{}
	gateHit  map[string]bool
	failOnce map[string]bool
}

type fakeWorkspace struct {
	desired  string
	runtimes int
	kinds    []provisioning.IntentKind
}

func newFakeApplier() *fakeApplier {
	return &fakeApplier{
		ws:       map[string]*fakeWorkspace{},
		byReqID:  map[string]string{},
		gates:    map[string]chan struct{}{},
		gateHit:  map[string]bool{},
		failOnce: map[string]bool{},
	}
}

func (f *fakeApplier) Apply(ctx context.Context, in provisioning.Intent) error {
	f.mu.Lock()
	gate, gated := f.gates[string(in.WorkspaceUID)]
	if gated {
		f.gateHit[string(in.WorkspaceUID)] = true
	}
	f.mu.Unlock()
	if gated {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
		f.mu.Lock()
		delete(f.gates, string(in.WorkspaceUID))
		f.mu.Unlock()
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnce[string(in.WorkspaceUID)] {
		delete(f.failOnce, string(in.WorkspaceUID))
		return errors.New("fake applier: injected failure")
	}
	w := f.ws[string(in.WorkspaceUID)]
	switch in.Kind {
	case provisioning.IntentCreate:
		if prevUID, dup := f.byReqID[in.RequestID]; dup {
			if prevUID != string(in.WorkspaceUID) {
				return fmt.Errorf("request %s replayed for different workspace", in.RequestID)
			}
			return nil // replayed create: stored result, no second runtime
		}
		if w != nil {
			return fmt.Errorf("second create for workspace %s", in.WorkspaceUID)
		}
		f.ws[string(in.WorkspaceUID)] = &fakeWorkspace{
			desired:  "Running",
			runtimes: 1,
			kinds:    []provisioning.IntentKind{in.Kind},
		}
		f.byReqID[in.RequestID] = string(in.WorkspaceUID)
		return nil
	case provisioning.IntentStart:
		if w == nil {
			return fmt.Errorf("start before create for %s", in.WorkspaceUID)
		}
		w.desired = "Running"
	case provisioning.IntentStop:
		if w == nil {
			return fmt.Errorf("stop before create for %s", in.WorkspaceUID)
		}
		w.desired = "Stopped"
	case provisioning.IntentDelete:
		if w == nil {
			return fmt.Errorf("delete before create for %s", in.WorkspaceUID)
		}
		w.desired = "Deleted"
	default:
		return fmt.Errorf("unknown intent kind %q", in.Kind)
	}
	w.kinds = append(w.kinds, in.Kind)
	return nil
}

func (f *fakeApplier) desired(uid string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if w := f.ws[uid]; w != nil {
		return w.desired
	}
	return ""
}

func (f *fakeApplier) runtimeCount(uid string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if w := f.ws[uid]; w != nil {
		return w.runtimes
	}
	return 0
}

func (f *fakeApplier) kinds(uid string) []provisioning.IntentKind {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.ws[uid]
	if w == nil {
		return nil
	}
	return append([]provisioning.IntentKind{}, w.kinds...)
}

func (f *fakeApplier) workspaceCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ws)
}

// setGate makes the next Apply for uid block until the returned channel is
// closed.
func (f *fakeApplier) setGate(uid string) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan struct{})
	f.gates[uid] = ch
	return ch
}

// blocked reports whether an Apply call is currently parked on uid's gate.
func (f *fakeApplier) blocked(uid string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gateHit[uid]
}

func setQuota(t *testing.T, db *store.DB, tenant string, limit provisioning.ResourceVector) {
	t.Helper()
	if err := db.WithTx(context.Background(), func(tx store.Tx) error {
		return provisioning.SetQuota(context.Background(), tx, tenant, limit)
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}
}

func heldSlots(t *testing.T, db *store.DB, tenant string) int64 {
	t.Helper()
	used, err := provisioning.HeldUsage(context.Background(), db.Pool(), tenant)
	if err != nil {
		t.Fatalf("held usage: %v", err)
	}
	return used.RunningSlots
}

// TestQuotaConcurrentRequests: quota running=10, 100 concurrent creates ->
// at most 10 reservations succeed; retry with the same idempotency key
// does not increase the count; same key with a different body conflicts.
func TestQuotaConcurrentRequests(t *testing.T) {
	db := newDB(t)
	svc := provisioning.NewService(db)
	tenant := "tenant-quota"
	limit := provisioning.ResourceVector{RunningSlots: 10, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 1 << 40}
	setQuota(t, db, tenant, limit)

	req := provisioning.CreateRequest{
		OwnerIssuer:  "issuer",
		OwnerSubject: "sub-1",
		Name:         "w1",
		Template:     provisioning.TemplateInfo{ID: "tpl_linuxdesktop", Name: "linux", Revision: 1, Runtime: "LinuxContainer", Experience: "Desktop"},
		Vector:       provisioning.ResourceVector{RunningSlots: 1, CPUMillis: 500, MemoryBytes: 1 << 30, DiskBytes: 1 << 30},
		DesiredState: "Stopped",
		DataPolicy:   "Ephemeral",
	}

	const n = 100
	var wg sync.WaitGroup
	var okCount, quotaCount, otherErr atomic.Int64
	results := make([]provisioning.WorkspaceRecord, n)
	errs := make([]error, n)
	keys := make([]string, n)
	for i := 0; i < n; i++ {
		keys[i] = fmt.Sprintf("create-%d", i)
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			r2 := req
			r2.Name = fmt.Sprintf("w-%d", i)
			res, err := svc.CreateWorkspace(ctx, tenant, keys[i], r2, provisioning.RequestHash(r2))
			results[i], errs[i] = res, err
			switch {
			case err == nil:
				okCount.Add(1)
			case provisioning.IsQuotaExceeded(err):
				quotaCount.Add(1)
			default:
				otherErr.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if otherErr.Load() != 0 {
		for _, err := range errs {
			if err != nil && !provisioning.IsQuotaExceeded(err) {
				t.Fatalf("unexpected create error: %v", err)
			}
		}
	}
	if got := okCount.Load(); got != 10 {
		t.Fatalf("successful creates = %d, want 10", got)
	}
	if got := quotaCount.Load(); got != 90 {
		t.Fatalf("quota rejections = %d, want 90", got)
	}
	if held := heldSlots(t, db, tenant); held != 10 {
		t.Fatalf("held running slots = %d, want 10", held)
	}

	// Retry a winning key with the same body: stored result, no extra slot.
	var res provisioning.WorkspaceRecord
	var key string
	var winReq provisioning.CreateRequest
	for i := 0; i < n; i++ {
		if errs[i] == nil {
			res, key = results[i], keys[i]
			winReq = req
			winReq.Name = fmt.Sprintf("w-%d", i)
			break
		}
	}
	again, err := svc.CreateWorkspace(context.Background(), tenant, key, winReq, provisioning.RequestHash(winReq))
	if err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if !again.Replayed || again.ID != res.ID || again.RequestID != res.RequestID {
		t.Fatalf("retry returned %+v, want stored %+v", again, res)
	}
	if held := heldSlots(t, db, tenant); held != 10 {
		t.Fatalf("held slots after replay = %d, want 10", held)
	}

	// Same key, different body -> conflict; count unchanged.
	diff := winReq
	diff.Vector.CPUMillis = 600
	_, err = svc.CreateWorkspace(context.Background(), tenant, key, diff, provisioning.RequestHash(diff))
	if !provisioning.IsIdempotencyConflict(err) {
		t.Fatalf("same key different body: got %v, want idempotency conflict", err)
	}
	if held := heldSlots(t, db, tenant); held != 10 {
		t.Fatalf("held slots after conflict = %d, want 10", held)
	}
}

// TestOutboxCrashRecovery: a crash after the DB commit but before Apply,
// and a crash after Apply but before the dispatch ack, both converge to
// exactly one applied runtime. Quota stays held until runtime absence is
// proven.
func TestOutboxCrashRecovery(t *testing.T) {
	db := newDB(t)
	svc := provisioning.NewService(db)
	outbox := provisioning.NewOutbox(db)
	fake := newFakeApplier()
	consumer := provisioning.NewConsumer(fake)
	tenant := "tenant-crash"
	setQuota(t, db, tenant, provisioning.ResourceVector{RunningSlots: 100, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 1 << 40})
	req := provisioning.CreateRequest{
		OwnerIssuer:  "issuer",
		OwnerSubject: "sub-2",
		Name:         "w2",
		Template:     provisioning.TemplateInfo{ID: "tpl_linuxdesktop", Name: "linux", Revision: 1, Runtime: "LinuxContainer", Experience: "Desktop"},
		Vector:       provisioning.ResourceVector{RunningSlots: 1, CPUMillis: 500, MemoryBytes: 1 << 30, DiskBytes: 1 << 30},
		DesiredState: "Stopped",
		DataPolicy:   "Ephemeral",
	}
	ctx := context.Background()

	// Phase A: commit (workspace + reservation + intent), then "crash"
	// before any dispatcher exists.
	req.Name = "crash-a"
	resA, err := svc.CreateWorkspace(ctx, tenant, "crash-a", req, provisioning.RequestHash(req))
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	pending, err := outbox.PendingIntents(ctx, provisioning.PlatformID(resA.ID))
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending after commit = %v/%v, want 1 intent", pending, err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() { runDone <- provisioning.NewDispatcher(outbox, consumer).Run(runCtx) }()
	eventually(t, "A applied", 15*time.Second, func() bool { return fake.desired(resA.ID) == "Running" })
	cancel()
	<-runDone
	if n := fake.runtimeCount(resA.ID); n != 1 {
		t.Fatalf("A runtimes = %d, want exactly 1", n)
	}
	if pending, _ := outbox.PendingIntents(ctx, provisioning.PlatformID(resA.ID)); len(pending) != 0 {
		t.Fatalf("A intent still pending after apply")
	}

	// Phase B: crash after Apply but before the intent is marked
	// dispatched. The hook is the crash point; it also cancels the run.
	req.Name = "crash-b"
	resB, err := svc.CreateWorkspace(ctx, tenant, "crash-b", req, provisioning.RequestHash(req))
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	crashCtx, crashCancel := context.WithCancel(ctx)
	var hooked atomic.Bool
	crashDisp := provisioning.NewDispatcher(outbox, consumer,
		provisioning.WithAfterApplyHook(func(in provisioning.Intent) error {
			if string(in.WorkspaceUID) == resB.ID {
				hooked.Store(true)
				crashCancel()
				return errors.New("simulated crash before ack")
			}
			return nil
		}))
	crashDone := make(chan error, 1)
	go func() { crashDone <- crashDisp.Run(crashCtx) }()
	eventually(t, "B applied before crash", 15*time.Second, func() bool {
		return hooked.Load() && fake.desired(resB.ID) == "Running"
	})
	<-crashDone
	if pending, _ := outbox.PendingIntents(ctx, provisioning.PlatformID(resB.ID)); len(pending) != 1 {
		t.Fatalf("B intent should still be pending after crash")
	}

	// Restart the dispatcher: the intent is redelivered, the consumer
	// drops it (revision <= lastApplied) and it gets acked. No second
	// runtime may appear.
	reCtx, reCancel := context.WithCancel(ctx)
	reDone := make(chan error, 1)
	go func() { reDone <- provisioning.NewDispatcher(outbox, consumer).Run(reCtx) }()
	eventually(t, "B intent re-acked", 15*time.Second, func() bool {
		p, _ := outbox.PendingIntents(context.Background(), provisioning.PlatformID(resB.ID))
		return len(p) == 0
	})
	if n := fake.runtimeCount(resB.ID); n != 1 {
		t.Fatalf("B runtimes = %d after recovery, want exactly 1", n)
	}
	if consumer.Dropped() < 1 {
		t.Fatalf("consumer should have dropped the redelivered intent")
	}

	// Phase C: a failed request that never proved runtime absence keeps
	// its reservation. ws3 commits reservation+intent but the runtime
	// never existed (dispatcher not running yet for it is irrelevant:
	// proof is what matters).
	req.Name = "crash-c"
	resC, err := svc.CreateWorkspace(ctx, tenant, "crash-c", req, provisioning.RequestHash(req))
	if err != nil {
		t.Fatalf("create C: %v", err)
	}
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.Release(ctx, tx, tenant, resC.ID, provisioning.ProofUnspecified)
	}); !errors.Is(err, provisioning.ErrProofRequired) {
		t.Fatalf("release without proof: got %v, want ErrProofRequired", err)
	}
	if held := heldSlots(t, db, tenant); held != 3 {
		t.Fatalf("held slots = %d, want 3 (A,B,C all held)", held)
	}
	// The intent is still pending: the runtime provably never existed, so
	// ProofNeverCreated legitimizes the release.
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.Release(ctx, tx, tenant, resC.ID, provisioning.ProofNeverCreated)
	}); err != nil {
		t.Fatalf("release with proof: %v", err)
	}
	if held := heldSlots(t, db, tenant); held != 2 {
		t.Fatalf("held slots after proven release = %d, want 2", held)
	}
	// Second release of the same reservation is an idempotent no-op.
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.Release(ctx, tx, tenant, resC.ID, provisioning.ProofNeverCreated)
	}); err != nil {
		t.Fatalf("double release: %v", err)
	}

	reCancel()
	<-reDone
}

// TestIntentRevisionOrdering: create→start→stop→delete serialize as
// revisions 1..4; delayed or replayed lower revisions cannot flip
// desiredState back or re-create a runtime; delete closes the stream; two
// workspaces dispatch in parallel; a replayed create with the same
// deterministic request ID returns the stored result.
func TestIntentRevisionOrdering(t *testing.T) {
	db := newDB(t)
	svc := provisioning.NewService(db)
	outbox := provisioning.NewOutbox(db)
	fake := newFakeApplier()
	consumer := provisioning.NewConsumer(fake)
	tenant := "tenant-order"
	setQuota(t, db, tenant, provisioning.ResourceVector{RunningSlots: 100, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 1 << 40})
	req := provisioning.CreateRequest{
		OwnerIssuer:  "issuer",
		OwnerSubject: "sub-3",
		Name:         "w3",
		Template:     provisioning.TemplateInfo{ID: "tpl_linuxdesktop", Name: "linux", Revision: 1, Runtime: "LinuxContainer", Experience: "Desktop"},
		Vector:       provisioning.ResourceVector{RunningSlots: 1, CPUMillis: 500, MemoryBytes: 1 << 30, DiskBytes: 1 << 30},
		DesiredState: "Stopped",
		DataPolicy:   "Ephemeral",
	}
	ctx := context.Background()

	req.Name = "order-a"
	origReq := req
	res, err := svc.CreateWorkspace(ctx, tenant, "order-1", origReq, provisioning.RequestHash(origReq))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	uid := res.ID
	if res.Revision != 1 {
		t.Fatalf("create revision = %d, want 1", res.Revision)
	}
	var wantRev uint64 = 2
	for _, k := range []provisioning.IntentKind{provisioning.IntentStart, provisioning.IntentStop, provisioning.IntentDelete} {
		sigRec, err := svc.SignalWorkspace(ctx, tenant, req.OwnerIssuer+"|"+req.OwnerSubject, req.OwnerIssuer+"|"+req.OwnerSubject, uid, "", k, nil)
		if err != nil {
			t.Fatalf("signal %s: %v", k, err)
		}
		if sigRec.Revision != wantRev {
			t.Fatalf("%s revision = %d, want %d", k, sigRec.Revision, wantRev)
		}
		wantRev++
	}
	// The outbox intent carries the deterministic request ID.
	pending, err := outbox.PendingIntents(ctx, provisioning.PlatformID(uid))
	if err != nil || len(pending) != 4 {
		t.Fatalf("pending intents = %v/%v, want 4", pending, err)
	}
	if pending[0].RequestID != res.RequestID {
		t.Fatalf("intent request_id %q != stored %q", pending[0].RequestID, res.RequestID)
	}
	if _, err := svc.SignalWorkspace(ctx, tenant, req.OwnerIssuer+"|"+req.OwnerSubject, req.OwnerIssuer+"|"+req.OwnerSubject, uid, "", provisioning.IntentStart, nil); !errors.Is(err, provisioning.ErrWorkspaceClosed) {
		t.Fatalf("start after delete: got %v, want ErrWorkspaceClosed", err)
	}

	// Two other workspaces dispatch in parallel: B is gated inside Apply,
	// C must still converge while B is blocked.
	req.Name = "order-b"
	resB, err := svc.CreateWorkspace(ctx, tenant, "order-b", req, provisioning.RequestHash(req))
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	req.Name = "order-c"
	resC, err := svc.CreateWorkspace(ctx, tenant, "order-c", req, provisioning.RequestHash(req))
	if err != nil {
		t.Fatalf("create C: %v", err)
	}
	gate := fake.setGate(resB.ID)

	runCtx, cancel := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() { runDone <- provisioning.NewDispatcher(outbox, consumer).Run(runCtx) }()
	defer func() { cancel(); <-runDone }()

	eventually(t, "B apply blocked", 15*time.Second, func() bool { return fake.blocked(resB.ID) })
	eventually(t, "C applied while B blocked", 15*time.Second, func() bool {
		return fake.desired(resC.ID) == "Running"
	})
	close(gate)

	eventually(t, "A deleted + stream closed", 15*time.Second, func() bool {
		return fake.desired(uid) == "Deleted" && consumer.Closed(provisioning.PlatformID(uid))
	})
	gotKinds := fake.kinds(uid)
	wantKinds := []provisioning.IntentKind{
		provisioning.IntentCreate, provisioning.IntentStart,
		provisioning.IntentStop, provisioning.IntentDelete,
	}
	if fmt.Sprint(gotKinds) != fmt.Sprint(wantKinds) {
		t.Fatalf("applied order = %v, want %v", gotKinds, wantKinds)
	}
	if rev, ok := consumer.LastApplied(provisioning.PlatformID(uid)); !ok || rev != 4 {
		t.Fatalf("lastApplied = %d/%v, want 4", rev, ok)
	}
	if n := fake.runtimeCount(uid); n != 1 {
		t.Fatalf("runtimes = %d, want exactly 1", n)
	}

	// Replayed/delayed lower revisions are dropped: desiredState does not
	// flip back and no runtime is re-created.
	drops := consumer.Dropped()
	stale := provisioning.Intent{WorkspaceUID: provisioning.PlatformID(uid), Revision: 2, Kind: provisioning.IntentStart, RequestID: res.RequestID}
	if err := consumer.Apply(ctx, stale); err != nil {
		t.Fatalf("stale apply: %v", err)
	}
	if fake.desired(uid) != "Deleted" {
		t.Fatalf("stale intent flipped desiredState to %q", fake.desired(uid))
	}
	if consumer.Dropped() <= drops {
		t.Fatalf("stale intent was not dropped")
	}
	// Stream is closed: even a higher revision is dropped after delete.
	if err := consumer.Apply(ctx, provisioning.Intent{
		WorkspaceUID: provisioning.PlatformID(uid), Revision: 99, Kind: provisioning.IntentStart, RequestID: res.RequestID,
	}); err != nil {
		t.Fatalf("post-close apply: %v", err)
	}
	if fake.desired(uid) != "Deleted" || fake.runtimeCount(uid) != 1 {
		t.Fatalf("post-close intent mutated workspace")
	}

	// Replay create with the same idempotency key -> stored result, no
	// new intent, no new runtime.
	replay, err := svc.CreateWorkspace(ctx, tenant, "order-1", origReq, provisioning.RequestHash(origReq))
	if err != nil {
		t.Fatalf("replay create: %v", err)
	}
	if !replay.Replayed || replay.ID != uid || replay.RequestID != res.RequestID || replay.Revision != 1 {
		t.Fatalf("replay = %+v, want stored result for %s", replay, uid)
	}
	eventually(t, "B running after gate", 15*time.Second, func() bool {
		return fake.desired(resB.ID) == "Running"
	})
	if n := fake.workspaceCount(); n != 3 {
		t.Fatalf("fake workspaces = %d, want 3 (A,B,C)", n)
	}
}

// ---------------------------------------------------------------------------
// Postgres session store, real-apiserver outbox, HTTP surface
// ---------------------------------------------------------------------------

// pgSessionAdapter adapts store.SessionStore (its own record type) to
// api.SessionStore. The production wiring lives in internal/backend; this is the
// same conversion for tests.
type pgSessionAdapter struct{ s *store.SessionStore }

func (a *pgSessionAdapter) Save(ctx context.Context, sess *api.Session) error {
	return a.s.Save(ctx, &store.Session{
		ID:         sess.ID,
		Issuer:     sess.Principal.Issuer,
		Subject:    sess.Principal.Subject,
		TenantID:   sess.Principal.TenantID,
		Groups:     sess.Principal.Groups,
		CSRFToken:  sess.CSRFToken,
		CreatedAt:  sess.CreatedAt,
		LastSeenAt: sess.LastSeenAt,
		ExpiresAt:  sess.ExpiresAt,
	})
}

func (a *pgSessionAdapter) Get(ctx context.Context, id string) (*api.Session, error) {
	rec, err := a.s.Get(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrSessionNotFound) {
			return nil, api.ErrSessionNotFound
		}
		return nil, err
	}
	return &api.Session{
		ID: rec.ID,
		Principal: api.Principal{
			Issuer: rec.Issuer, Subject: rec.Subject,
			TenantID: rec.TenantID, Groups: rec.Groups,
		},
		CSRFToken:  rec.CSRFToken,
		CreatedAt:  rec.CreatedAt,
		LastSeenAt: rec.LastSeenAt,
		ExpiresAt:  rec.ExpiresAt,
	}, nil
}

func (a *pgSessionAdapter) Peek(ctx context.Context, id string) (*api.Session, error) {
	rec, err := a.s.Peek(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrSessionNotFound) {
			return nil, api.ErrSessionNotFound
		}
		return nil, err
	}
	return &api.Session{
		ID: rec.ID,
		Principal: api.Principal{
			Issuer: rec.Issuer, Subject: rec.Subject,
			TenantID: rec.TenantID, Groups: rec.Groups,
		},
		CSRFToken:  rec.CSRFToken,
		CreatedAt:  rec.CreatedAt,
		LastSeenAt: rec.LastSeenAt,
		ExpiresAt:  rec.ExpiresAt,
	}, nil
}

func (a *pgSessionAdapter) TouchPrincipal(ctx context.Context, principal string) (int64, error) {
	return a.s.TouchPrincipal(ctx, principal)
}

func (a *pgSessionAdapter) Delete(ctx context.Context, id string) error {
	return a.s.Delete(ctx, id)
}

func TestPGSessionStore(t *testing.T) {
	db := newDB(t)
	ss := store.NewSessionStore(db, 60*time.Second)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	sess := &store.Session{
		ID: "sess-1", Issuer: "iss", Subject: "sub", TenantID: "tenant-a",
		Groups: []string{"g1"}, CSRFToken: "csrf-1",
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := ss.Save(ctx, sess); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := ss.Get(ctx, "sess-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// SEC-27: the raw CSRF token is never persisted — Get returns the
	// session-ID-keyed HMAC instead (mirrors store.csrfTokenMAC).
	mac := hmac.New(sha256.New, []byte("sess-1"))
	mac.Write([]byte("tcdi-csrf-token\x00"))
	mac.Write([]byte("csrf-1"))
	wantCSRF := hex.EncodeToString(mac.Sum(nil))
	if got.CSRFToken != wantCSRF || got.TenantID != "tenant-a" || len(got.Groups) != 1 {
		t.Fatalf("session mismatch: %+v", got)
	}
	// ...and the row itself must carry digests, not usable credentials.
	idSum := sha256.Sum256([]byte("sess-1"))
	var rowID, rowCSRF string
	if err := db.Pool().QueryRow(ctx,
		`SELECT id, csrf_token FROM sessions WHERE id = $1`,
		hex.EncodeToString(idSum[:])).Scan(&rowID, &rowCSRF); err != nil {
		t.Fatalf("digest-keyed lookup: %v", err)
	}
	if rowCSRF != wantCSRF || rowCSRF == "csrf-1" {
		t.Fatalf("csrf_token persisted in usable form: %q", rowCSRF)
	}
	var rawRows int
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM sessions WHERE id = 'sess-1'`).Scan(&rawRows); err != nil {
		t.Fatal(err)
	}
	if rawRows != 0 {
		t.Fatal("session row keyed by raw session ID")
	}
	if !got.LastSeenAt.After(sess.LastSeenAt) && !got.LastSeenAt.Equal(sess.LastSeenAt) {
		t.Fatalf("last seen not slid: %v", got.LastSeenAt)
	}

	// absolute expiry
	sess.ID, sess.ExpiresAt = "sess-expired", now.Add(-time.Hour)
	if err := ss.Save(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Get(ctx, "sess-expired"); !errors.Is(err, store.ErrSessionNotFound) {
		t.Fatalf("expired get: %v", err)
	}

	// idle expiry
	short := store.NewSessionStore(db, 30*time.Millisecond)
	sess.ID, sess.ExpiresAt = "sess-idle", now.Add(time.Hour)
	if err := short.Save(ctx, sess); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if _, err := short.Get(ctx, "sess-idle"); !errors.Is(err, store.ErrSessionNotFound) {
		t.Fatalf("idle get: %v", err)
	}

	if err := ss.Delete(ctx, "sess-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Get(ctx, "sess-1"); !errors.Is(err, store.ErrSessionNotFound) {
		t.Fatalf("deleted get: %v", err)
	}
	if _, err := ss.Get(ctx, "never-existed"); !errors.Is(err, store.ErrSessionNotFound) {
		t.Fatalf("missing get: %v", err)
	}
}

// TestSessionEpochRotation covers defect F5: every session record binds to
// platform_meta.session_epoch at Save; rotating the epoch (the restore
// procedure's job) invalidates every pre-rotation session — including ones
// revoked after the backup was taken — while sessions saved under the
// current epoch keep working across plain restarts.
func TestSessionEpochRotation(t *testing.T) {
	db := newDB(t)
	ss := store.NewSessionStore(db, 60*time.Second)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	sess := &store.Session{
		ID: "sess-epoch", Issuer: "iss", Subject: "sub", TenantID: "tenant-a",
		Groups: []string{"g1"}, CSRFToken: "csrf-epoch",
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := ss.Save(ctx, sess); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := ss.Get(ctx, "sess-epoch"); err != nil {
		t.Fatalf("same-epoch get must succeed: %v", err)
	}

	// A row carried over from a dump written under a different epoch —
	// or one predating the epoch column — is rejected. The row is keyed by
	// the session-ID digest (SEC-27) so the epoch check is what rejects it.
	oldEpochKey := sha256.Sum256([]byte("sess-old-epoch"))
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO sessions (id, issuer, subject, tenant_id, groups, csrf_token,
			created_at, last_seen_at, expires_at, epoch)
		VALUES ($1, 'iss', 'sub', 'tenant-a', '[]', 'csrf-old',
			now(), now(), now() + interval '1 hour', 'pre-restore-epoch')`,
		hex.EncodeToString(oldEpochKey[:])); err != nil {
		t.Fatalf("insert old-epoch row: %v", err)
	}
	if _, err := ss.Get(ctx, "sess-old-epoch"); !errors.Is(err, store.ErrSessionNotFound) {
		t.Fatalf("old-epoch get: %v, want ErrSessionNotFound", err)
	}

	// Rotate: the current session dies too — a restored dump can never
	// resurrect sessions that were valid when the backup ran.
	if err := db.RotateSessionEpoch(ctx); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, err := ss.Get(ctx, "sess-epoch"); !errors.Is(err, store.ErrSessionNotFound) {
		t.Fatalf("post-rotation get: %v, want ErrSessionNotFound", err)
	}

	// A fresh login under the new epoch works — and survives "restarts"
	// (a new store instance over the same DB).
	ss2 := store.NewSessionStore(db, 60*time.Second)
	sess.ID = "sess-new-epoch"
	if err := ss2.Save(ctx, sess); err != nil {
		t.Fatalf("save new epoch: %v", err)
	}
	if _, err := ss2.Get(ctx, "sess-new-epoch"); err != nil {
		t.Fatalf("new-epoch get must succeed: %v", err)
	}
}

// staticCatalog serves one template for tenant-a in integration tests.
type staticCatalog struct{}

func (staticCatalog) Resolve(_ context.Context, tenantID, id string) (api.TemplateEntry, error) {
	if tenantID != "tenant-a" || id != "tpl_linuxdesktop" {
		return api.TemplateEntry{}, api.ErrTemplateNotFound
	}
	return api.TemplateEntry{
		ID: "tpl_linuxdesktop", Name: "linux", Revision: 1,
		Runtime: "LinuxContainer", Experience: "Desktop",
		CPUMillis: 500, MemoryMiB: 1024, StorageGiB: 1,
		IdleTimeoutSeconds: 1800, DisconnectGraceSeconds: 600, MaxRunningSeconds: 28800,
		DataPolicyDefault: "Ephemeral", ClipboardPolicy: "Disabled",
		PublishedAt: time.Now().UTC(),
	}, nil
}

func (staticCatalog) List(_ context.Context, tenantID, _, _ string, _ int) ([]api.TemplateEntry, string, error) {
	e, err := staticCatalog{}.Resolve(context.Background(), tenantID, "tpl_linuxdesktop")
	if err != nil {
		return nil, "", nil
	}
	return []api.TemplateEntry{e}, "", nil
}

// httpEnv is the full public API stack over real Postgres: PG session store,
// OIDC login via oidctest, real provisioning.Service, and an optional
// dispatcher applying intents to envtest via K8sApplier.
type httpEnv struct {
	issuer  *oidctest.Issuer
	server  *httptest.Server
	logs    *bytes.Buffer
	auth    *api.Authenticator
	svc     *provisioning.Service
	k8s     client.Client
	tenants provisioning.TenantNamespaces
}

// mustLoginSealer builds the login-state sealer the test Authenticator uses.
func mustLoginSealer(t *testing.T) *loginstate.Sealer {
	t.Helper()
	s, err := loginstate.NewSealer(bytes.Repeat([]byte{0x1a}, 32))
	if err != nil {
		t.Fatalf("loginstate.NewSealer: %v", err)
	}
	return s
}

func newHTTPEnv(t *testing.T, db *store.DB, dispatch bool) *httpEnv {
	t.Helper()
	iss, err := oidctest.NewIssuer()
	if err != nil {
		t.Fatalf("oidctest: %v", err)
	}
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, nil))
	sessions := &pgSessionAdapter{s: store.NewSessionStore(db, 30*time.Minute)}
	authn, err := api.NewAuthenticator(context.Background(), api.AuthConfig{
		Issuer:      iss.URL(),
		ClientID:    iss.ClientID,
		RedirectURL: "https://portal.test/auth/callback",
		LoginSealer: mustLoginSealer(t),
	}, sessions, logger)
	if err != nil {
		t.Fatalf("authenticator: %v", err)
	}
	tenants := provisioning.TenantNamespaces{"tenant-a": "ns-e2e", "tenant-b": "ns-b"}
	svc := provisioning.NewService(db)
	h := api.NewWorkspaceHandler(svc, staticCatalog{}, tenants)
	th := api.NewTemplateHandler(staticCatalog{}, tenants)
	mux := http.NewServeMux()
	mux.Handle("/auth/login", http.HandlerFunc(authn.LoginHandler))
	mux.Handle("/auth/callback", http.HandlerFunc(authn.CallbackHandler))
	api.MountWorkspaceRoutes(mux, authn, h, th)
	srv := httptest.NewServer(api.RequestID(api.Audit(logger)(mux)))

	env := &httpEnv{issuer: iss, server: srv, logs: logBuf, auth: authn, svc: svc, k8s: k8sClient, tenants: tenants}
	if dispatch {
		outbox := provisioning.NewOutbox(db)
		disp := provisioning.NewDispatcher(outbox,
			provisioning.NewK8sApplier(k8sClient, tenants),
			provisioning.WithPollInterval(10*time.Millisecond))
		dctx, dcancel := context.WithCancel(context.Background())
		go func() { _ = disp.Run(dctx) }()
		t.Cleanup(dcancel)
	}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return env
}

func (e *httpEnv) loginUser(t *testing.T, subject, tenant string) (sess, csrf *http.Cookie) {
	t.Helper()
	e.issuer.Subject = subject
	e.issuer.TenantID = tenant
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(e.server.URL + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	loc := resp.Header.Get("Location")
	var loginCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == e.auth.LoginCookieName() {
			loginCookie = c
		}
	}
	resp.Body.Close()
	resp2, err := client.Get(loc)
	if err != nil {
		t.Fatal(err)
	}
	cbLoc := resp2.Header.Get("Location")
	resp2.Body.Close()
	cbReq, _ := http.NewRequest(http.MethodGet,
		e.server.URL+"/auth/callback?"+strings.SplitN(cbLoc, "?", 2)[1], nil)
	if loginCookie != nil {
		cbReq.AddCookie(loginCookie) // SEC-03: the pending state is browser-bound
	}
	resp3, err := client.Do(cbReq)
	if err != nil {
		t.Fatal(err)
	}
	cookies := resp3.Cookies()
	resp3.Body.Close()
	for _, c := range cookies {
		if c.Name == e.auth.SessionCookieName() {
			sess = c
		}
		if c.Name == e.auth.CSRFCookieName() {
			csrf = c
		}
	}
	if sess == nil || csrf == nil {
		t.Fatalf("login missing cookies")
	}
	return
}

func (e *httpEnv) do(t *testing.T, sess, csrf *http.Cookie, method, path, body string, hdrs map[string]string) (*http.Response, string) {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req, _ := http.NewRequest(method, e.server.URL+path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if sess != nil {
		req.AddCookie(sess)
	}
	if csrf != nil {
		req.Header.Set(e.auth.CSRFHeader(), csrf.Value)
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// TestAPIEndToEnd: real OIDC login -> create -> CR lands in envtest;
// cross-user reads are invisible; quota exhaustion maps to QUOTA_EXHAUSTED
// and creates no CR; audit logs carry no secrets.
func TestAPIEndToEnd(t *testing.T) {
	if k8sClient == nil {
		t.Skip("no envtest assets (KUBEBUILDER_ASSETS)")
	}
	db := newDB(t)
	env := newHTTPEnv(t, db, true)
	setQuota(t, db, "tenant-a", provisioning.ResourceVector{RunningSlots: 1, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 1 << 40})

	sessA, csrfA := env.loginUser(t, "user-a", "tenant-a")
	resp, body := env.do(t, sessA, csrfA, http.MethodPost, "/v1/workspaces",
		`{"name":"a-box","templateRef":"tpl_linuxdesktop","desiredState":"Running"}`,
		map[string]string{"Idempotency-Key": "e2e-a-1000"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, body)
	}
	var created api.WorkspaceView
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	if created.Phase != "Pending" && created.Phase != "Provisioning" {
		t.Fatalf("phase=%q", created.Phase)
	}

	// The dispatcher applies the intent to envtest: exactly one CR.
	crName := provisioning.WorkspaceCRName(provisioning.PlatformID(created.ID))
	eventually(t, "workspace CR exists", 15*time.Second, func() bool {
		return k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "ns-e2e", Name: crName},
			&workspacev1alpha1.Workspace{}) == nil
	})

	// Idempotent replay: same key + same body -> same stored result.
	resp, body2 := env.do(t, sessA, csrfA, http.MethodPost, "/v1/workspaces",
		`{"name":"a-box","templateRef":"tpl_linuxdesktop","desiredState":"Running"}`,
		map[string]string{"Idempotency-Key": "e2e-a-1000"})
	var replay api.WorkspaceView
	_ = json.Unmarshal([]byte(body2), &replay)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", resp.StatusCode, body2)
	}
	if replay.ID != created.ID {
		t.Fatalf("replay id=%s want %s", replay.ID, created.ID)
	}

	// Quota exhausted: running=1 already held; a second create is refused
	// with QUOTA_EXHAUSTED and must not produce a CR.
	resp, body = env.do(t, sessA, csrfA, http.MethodPost, "/v1/workspaces",
		`{"name":"a-box-2","templateRef":"tpl_linuxdesktop","desiredState":"Running"}`,
		map[string]string{"Idempotency-Key": "e2e-a-2000"})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "QUOTA_EXHAUSTED") {
		t.Fatalf("quota status=%d body=%s", resp.StatusCode, body)
	}
	time.Sleep(200 * time.Millisecond)
	if n := countWorkspaceCRs(t, "ns-e2e"); n != 1 {
		t.Fatalf("CRs in ns-e2e = %d, want 1 (quota rejection must not create)", n)
	}

	// user-b same tenant: foreign workspace invisible.
	sessB, csrfB := env.loginUser(t, "user-b", "tenant-a")
	resp, _ = env.do(t, sessB, csrfB, http.MethodGet, "/v1/workspaces/"+created.ID, "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("B get A's ws: status=%d want 404", resp.StatusCode)
	}
	resp, body = env.do(t, sessB, csrfB, http.MethodGet, "/v1/workspaces", "", nil)
	if resp.StatusCode != 200 || strings.Contains(body, created.ID) {
		t.Fatalf("B list leaked A's workspace: %s", body)
	}
	resp, _ = env.do(t, sessB, csrfB, http.MethodPost, "/v1/workspaces/"+created.ID+"/stop", "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("B stop A's ws: status=%d want 404", resp.StatusCode)
	}

	// No session/CSRF/token material in logs.
	logs := env.logs.String()
	for _, leak := range []string{sessA.Value, csrfA.Value, sessB.Value, env.issuer.LastIDToken()} {
		if leak != "" && strings.Contains(logs, leak) {
			t.Fatalf("log contains sensitive value")
		}
	}
}

// TestOutboxCrashRecoveryEnvtest is the crash-recovery scenario with the
// real apiserver: crash after DB commit before CR create, and after CR
// create before ack, converge to exactly one Workspace CR.
func TestOutboxCrashRecoveryEnvtest(t *testing.T) {
	if k8sClient == nil {
		t.Skip("no envtest assets (KUBEBUILDER_ASSETS)")
	}
	db := newDB(t)
	tenants := provisioning.TenantNamespaces{"tenant-crash": "ns-crash"}
	svc := provisioning.NewService(db)
	outbox := provisioning.NewOutbox(db)
	applier := provisioning.NewK8sApplier(k8sClient, tenants)
	setQuota(t, db, "tenant-crash", provisioning.ResourceVector{RunningSlots: 100, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 1 << 40})
	req := provisioning.CreateRequest{
		OwnerIssuer:  "issuer",
		OwnerSubject: "sub-crash",
		Name:         "crash-ws",
		Template:     provisioning.TemplateInfo{ID: "tpl_linuxdesktop", Name: "linux", Revision: 1, Runtime: "LinuxContainer", Experience: "Desktop"},
		Vector:       provisioning.ResourceVector{RunningSlots: 1, CPUMillis: 500, MemoryBytes: 1 << 30, DiskBytes: 1 << 30},
		DesiredState: "Running",
		DataPolicy:   "Ephemeral",
	}
	ctx := context.Background()

	// Crash after DB commit, before any dispatcher exists.
	req.Name = "crash-et-a"
	resA, err := svc.CreateWorkspace(ctx, "tenant-crash", "et-a", req, provisioning.RequestHash(req))
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- provisioning.NewDispatcher(outbox, applier).Run(runCtx) }()
	eventually(t, "A CR created", 15*time.Second, func() bool {
		return workspaceCRByUID(t, resA.ID) != nil
	})
	cancel()
	<-done

	// Crash after CR create, before the outbox ack.
	req.Name = "crash-et-b"
	resB, err := svc.CreateWorkspace(ctx, "tenant-crash", "et-b", req, provisioning.RequestHash(req))
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	crashCtx, crashCancel := context.WithCancel(ctx)
	var hooked atomic.Bool
	crashDisp := provisioning.NewDispatcher(outbox, applier,
		provisioning.WithAfterApplyHook(func(in provisioning.Intent) error {
			if string(in.WorkspaceUID) == resB.ID {
				hooked.Store(true)
				crashCancel()
				return errors.New("simulated crash before ack")
			}
			return nil
		}))
	crashDone := make(chan error, 1)
	go func() { crashDone <- crashDisp.Run(crashCtx) }()
	eventually(t, "B CR created before crash", 15*time.Second, func() bool {
		return hooked.Load() && workspaceCRByUID(t, resB.ID) != nil
	})
	<-crashDone
	if pending, _ := outbox.PendingIntents(ctx, provisioning.PlatformID(resB.ID)); len(pending) != 1 {
		t.Fatalf("B intent should be pending after crash")
	}

	// Restart: redelivery hits AlreadyExists with the same request ID ->
	// success, and the intent is acked. Still exactly one CR.
	reCtx, reCancel := context.WithCancel(ctx)
	reDone := make(chan error, 1)
	go func() { reDone <- provisioning.NewDispatcher(outbox, applier).Run(reCtx) }()
	eventually(t, "B intent re-acked", 15*time.Second, func() bool {
		p, _ := outbox.PendingIntents(context.Background(), provisioning.PlatformID(resB.ID))
		return len(p) == 0
	})
	reCancel()
	<-reDone
	for _, id := range []string{resA.ID, resB.ID} {
		if cr := workspaceCRByUID(t, id); cr == nil {
			t.Fatalf("no CR for %s after recovery", id)
		}
	}

	// Replay create with the same idempotency key returns the stored
	// result; no new CR.
	replay, err := svc.CreateWorkspace(ctx, "tenant-crash", "et-b", req, provisioning.RequestHash(req)) // req.Name still crash-et-b
	if err != nil {
		t.Fatalf("replay create: %v", err)
	}
	if !replay.Replayed || replay.ID != resB.ID {
		t.Fatalf("replay = %+v, want stored %s", replay, resB.ID)
	}
	if n := countWorkspaceCRs(t, "ns-crash"); n != 2 {
		t.Fatalf("CRs after replay = %d, want 2", n)
	}

	// The CR carries the fencing numbers and request ID label.
	var cr workspacev1alpha1.Workspace
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: "ns-crash", Name: provisioning.WorkspaceCRName(provisioning.PlatformID(resB.ID))}, &cr); err != nil {
		t.Fatal(err)
	}
	if cr.Spec.IntentRevision != 1 || cr.Spec.RuntimeGeneration != 1 {
		t.Fatalf("CR spec fencing wrong: %+v", cr.Spec)
	}
	if cr.Spec.OwnerSubject.Subject != "sub-crash" {
		t.Fatalf("CR owner wrong: %+v", cr.Spec.OwnerSubject)
	}
}
