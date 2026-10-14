package provisioning_test

// Contract tests — post-restart recovery and
// quota settlement (design §8); they pin the behaviour recovery.go
// implements.
//
// Asserted contract:
//   - a held reservation is released only with positive proof the runtime
//     is gone — observer "still present" AND observer errors free nothing;
//   - a workspace whose create intent never dispatched settles via
//     ProofNeverCreated;
//   - a restart-resumed pass re-delivers pending outbox intents and marks
//     them dispatched only after a successful apply;
//   - a poison intent is retried at most MaxIntentApplyAttempts and then
//     quarantined — no infinite loop attempting to spawn a new runtime.
//
// The pg harness mirrors internal/broker/main_test.go (own container,
// per-test database, embedded migrations), lazily started so the pure
// in-memory tests in this package still run without docker.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// pgRecImage is the locked postgres candidate digest from
// docs/compatibility.md (data_plane.postgresql).
const pgRecImage = "docker.io/library/postgres:18.0@sha256:41fc5342eefba6cc2ccda736aaf034bbbb7c3df0fdb81516eba1ba33f360162c"
const pgRecContainer = "tcdi-it-w3t8red-pg"

var (
	pgRecOnce    sync.Once
	pgRecErr     error
	pgRecAdmin   string
	pgRecStarted atomic.Bool
	pgRecCounter atomic.Int64
)

// TestMain removes the lazily-started postgres container at process end.
// Pure in-memory tests run regardless of docker availability.
func TestMain(m *testing.M) {
	code := m.Run()
	if pgRecStarted.Load() {
		_ = exec.Command("docker", "rm", "-f", pgRecContainer).Run()
	}
	os.Exit(code)
}

func startRecoveryPG() {
	if _, err := exec.LookPath("docker"); err != nil {
		pgRecErr = errors.New("docker not found")
		return
	}
	_ = exec.Command("docker", "rm", "-f", pgRecContainer).Run()

	var pwBytes [16]byte
	if _, err := rand.Read(pwBytes[:]); err != nil {
		pgRecErr = err
		return
	}
	pw := hex.EncodeToString(pwBytes[:])

	out, err := exec.Command("docker", "run", "-d",
		"--name", pgRecContainer,
		"-e", "POSTGRES_PASSWORD="+pw,
		"-e", "POSTGRES_DB=postgres",
		"-p", "127.0.0.1::5432",
		pgRecImage).CombinedOutput()
	if err != nil {
		pgRecErr = fmt.Errorf("docker run postgres: %v\n%s", err, out)
		return
	}
	pgRecStarted.Store(true)

	portOut, err := exec.Command("docker", "port", pgRecContainer, "5432").Output()
	if err != nil {
		pgRecErr = fmt.Errorf("docker port: %v", err)
		return
	}
	var host string
	for _, line := range strings.Split(strings.TrimSpace(string(portOut)), "\n") {
		line = strings.TrimSpace(line)
		if i := strings.LastIndex(line, "->"); i >= 0 {
			line = strings.TrimSpace(line[i+2:])
		}
		if strings.HasPrefix(line, "127.0.0.1:") {
			host = line
			break
		}
	}
	if host == "" {
		pgRecErr = fmt.Errorf("no 127.0.0.1 port mapping: %q", portOut)
		return
	}
	pgRecAdmin = fmt.Sprintf("postgres://postgres:%s@%s/postgres?sslmode=disable&pool_max_conns=8", pw, host)

	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		db, err := store.Open(ctx, pgRecAdmin)
		cancel()
		if err == nil {
			db.Close()
			return
		}
		if time.Now().After(deadline) {
			pgRecErr = fmt.Errorf("postgres not ready: %v", err)
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// recoveryDB lazily starts the shared container and returns a fresh
// migrated database; skips the test when docker/postgres is unavailable.
func recoveryDB(t *testing.T) *store.DB {
	t.Helper()
	pgRecOnce.Do(startRecoveryPG)
	if pgRecErr != nil {
		t.Skipf("pg-backed recovery tests unavailable: %v", pgRecErr)
	}
	ctx := context.Background()
	admin, err := store.Open(ctx, pgRecAdmin)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	name := fmt.Sprintf("rec_%d_%d", time.Now().UnixNano()%1_000_000, pgRecCounter.Add(1))
	if _, err := admin.Pool().Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatalf("create database: %v", err)
	}
	dbURL := strings.Replace(pgRecAdmin, "/postgres?", "/"+name+"?", 1)
	db, err := store.Open(ctx, dbURL)
	if err != nil {
		admin.Close()
		t.Fatalf("connect %s: %v", name, err)
	}
	if err := db.Migrate(ctx); err != nil {
		db.Close()
		admin.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Pool().Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		admin.Close()
	})
	return db
}

// seedHeldWorkspace plants a tenant quota, a workspace row and a held
// reservation through the real Reserve path.
func seedHeldWorkspace(t *testing.T, db *store.DB, tenantID, wsUID string, v provisioning.ResourceVector) {
	t.Helper()
	ctx := context.Background()
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		if err := provisioning.SetQuota(ctx, tx, tenantID, provisioning.ResourceVector{
			RunningSlots: 5, CPUMillis: 8000, MemoryBytes: 1 << 34, DiskBytes: 1 << 40,
		}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO workspaces (id, tenant_id, owner_subject, request_id)
			VALUES ($1, $2, 'iss|sub', $3)`, wsUID, tenantID, "req-"+wsUID); err != nil {
			return err
		}
		return provisioning.Reserve(ctx, tx, tenantID, wsUID, v)
	}); err != nil {
		t.Fatalf("seed held workspace: %v", err)
	}
}

// fakeObserver answers RuntimeGone from the test.
type fakeObserver struct {
	gone bool
	err  error
}

func (f *fakeObserver) RuntimeGone(context.Context, provisioning.PlatformID) (bool, error) {
	return f.gone, f.err
}

var quotaVec = provisioning.ResourceVector{
	RunningSlots: 1, CPUMillis: 500, MemoryBytes: 1 << 30, DiskBytes: 1 << 32,
}

// TestRecovery_QuotaHeldUntilRuntimeProvenGone: reservation stays held
// while the runtime is present and while absence is unproven; only a
// positive "gone" releases it.
func TestRecovery_QuotaHeldUntilRuntimeProvenGone(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	seedHeldWorkspace(t, db, "tenant-a", "ws-1", quotaVec)

	// Runtime still present: nothing is released.
	rec := provisioning.NewRecovery(db, &fakeObserver{gone: false})
	if err := rec.SettleQuota(ctx, "tenant-a", "ws-1"); !errors.Is(err, provisioning.ErrRuntimeNotProvenGone) {
		t.Fatalf("SettleQuota with runtime present = %v, want ErrRuntimeNotProvenGone", err)
	}
	used, err := provisioning.HeldUsage(ctx, db.Pool(), "tenant-a")
	if err != nil {
		t.Fatalf("HeldUsage: %v", err)
	}
	if used != quotaVec {
		t.Fatalf("reservation was released while runtime present: %+v", used)
	}

	// Observer error (K8s/operator unreachable): still nothing freed —
	// uncertainty never frees quota.
	rec = provisioning.NewRecovery(db, &fakeObserver{err: errors.New("k8s api unavailable")})
	if err := rec.SettleQuota(ctx, "tenant-a", "ws-1"); !errors.Is(err, provisioning.ErrRuntimeNotProvenGone) {
		t.Fatalf("SettleQuota with observer error = %v, want ErrRuntimeNotProvenGone", err)
	}
	if used, _ = provisioning.HeldUsage(ctx, db.Pool(), "tenant-a"); used != quotaVec {
		t.Fatalf("reservation released on observer error: %+v", used)
	}

	// Runtime proven gone: the reservation releases.
	rec = provisioning.NewRecovery(db, &fakeObserver{gone: true})
	if err := rec.SettleQuota(ctx, "tenant-a", "ws-1"); err != nil {
		t.Fatalf("SettleQuota with proven absence: %v", err)
	}
	if used, _ = provisioning.HeldUsage(ctx, db.Pool(), "tenant-a"); used.RunningSlots != 0 {
		t.Fatalf("reservation still held after proven absence: %+v", used)
	}
}

// seedIntent appends one outbox intent for the workspace inside its own
// committed transaction.
func seedIntent(t *testing.T, db *store.DB, wsUID string, kind provisioning.IntentKind) {
	t.Helper()
	if err := db.WithTx(context.Background(), func(tx store.Tx) error {
		_, err := provisioning.AppendIntent(context.Background(), tx, provisioning.PlatformID(wsUID), kind)
		return err
	}); err != nil {
		t.Fatalf("append %s: %v", kind, err)
	}
}

// TestRecovery_NeverCreatedSettles: a workspace whose create intent never
// dispatched is settled via ProofNeverCreated — no runtime ever existed.
func TestRecovery_NeverCreatedSettles(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	seedHeldWorkspace(t, db, "tenant-a", "ws-1", quotaVec)

	// A pending create intent means the runtime layer never saw the
	// workspace: provable never_created.
	seedIntent(t, db, "ws-1", provisioning.IntentCreate)

	rec := provisioning.NewRecovery(db, &fakeObserver{gone: true})
	if err := rec.SettleQuota(ctx, "tenant-a", "ws-1"); err != nil {
		t.Fatalf("SettleQuota: %v", err)
	}
	var proof string
	if err := db.Pool().QueryRow(ctx,
		`SELECT release_proof FROM quota_reservation WHERE workspace_id='ws-1'`).Scan(&proof); err != nil {
		t.Fatalf("read release_proof: %v", err)
	}
	if proof != string(provisioning.ProofNeverCreated) {
		t.Fatalf("release_proof=%q want %q", proof, provisioning.ProofNeverCreated)
	}
}

// TestRecovery_RestartRedeliversPendingIntents: after an API crash with
// intents still pending, a recovery pass re-drives them in revision order
// through the applier — and only marks dispatched after a successful apply.
func TestRecovery_RestartRedeliversPendingIntents(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	seedHeldWorkspace(t, db, "tenant-a", "ws-1", quotaVec)

	seedIntent(t, db, "ws-1", provisioning.IntentCreate)
	seedIntent(t, db, "ws-1", provisioning.IntentStart)

	pending, err := provisioning.NewRecovery(db, &fakeObserver{}).PendingRecovery(ctx)
	if err != nil {
		t.Fatalf("PendingRecovery: %v", err)
	}
	if len(pending) != 1 || pending[0] != "ws-1" {
		t.Fatalf("PendingRecovery = %v, want [ws-1]", pending)
	}

	rec := &recordingApplier{}
	actions, err := provisioning.NewRecovery(db, &fakeObserver{}).Recover(ctx, rec)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(rec.intents) != 2 {
		t.Fatalf("recovery delivered %d intents, want 2", len(rec.intents))
	}
	if rec.intents[0].Kind != provisioning.IntentCreate || rec.intents[1].Kind != provisioning.IntentStart {
		t.Fatalf("intents delivered out of order: %+v", rec.intents)
	}
	foundRedeliver := false
	for _, a := range actions {
		if a.WorkspaceUID == "ws-1" && a.Kind == provisioning.ActionRedeliverIntent {
			foundRedeliver = true
		}
	}
	if !foundRedeliver {
		t.Fatalf("no redeliver action recorded: %+v", actions)
	}
}

// recordingApplier records delivered intents and counts every attempt; it
// can be set to fail permanently (a poison intent).
type recordingApplier struct {
	fail     bool
	attempts int
	intents  []provisioning.Intent
}

func (r *recordingApplier) Apply(_ context.Context, in provisioning.Intent) error {
	r.attempts++
	if r.fail {
		return errors.New("injected apply failure")
	}
	r.intents = append(r.intents, in)
	return nil
}

// TestRecovery_PoisonIntentQuarantined: an intent that keeps failing is
// retried a bounded number of times and then quarantined — recovery must
// never loop forever trying to create a runtime. Once quarantined the
// applier is never invoked again for that intent.
func TestRecovery_PoisonIntentQuarantined(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	seedHeldWorkspace(t, db, "tenant-a", "ws-1", quotaVec)

	seedIntent(t, db, "ws-1", provisioning.IntentCreate)

	rec := &recordingApplier{fail: true}
	recovery := provisioning.NewRecovery(db, &fakeObserver{})
	recovery.MaxApplyAttempts = 3

	var sawQuarantine bool
	for i := 0; i < provisioning.MaxIntentApplyAttempts+3 && !sawQuarantine; i++ {
		actions, err := recovery.Recover(ctx, rec)
		if err != nil {
			t.Fatalf("Recover pass %d: %v", i, err)
		}
		for _, a := range actions {
			if a.Kind == provisioning.ActionQuarantineIntent {
				sawQuarantine = true
			}
		}
	}
	if !sawQuarantine {
		t.Fatalf("poison intent never quarantined after %d attempts",
			rec.attempts)
	}
	if rec.attempts == 0 {
		t.Fatal("poison intent quarantined without ever being attempted")
	}
	before := rec.attempts
	for i := 0; i < 3; i++ {
		if _, err := recovery.Recover(ctx, rec); err != nil {
			t.Fatalf("Recover after quarantine: %v", err)
		}
	}
	if rec.attempts != before {
		t.Fatalf("quarantined intent still retried: attempts %d -> %d",
			before, rec.attempts)
	}
}

// TestRecovery_APIRestartCreateDedup: an API crash between commit and
// response must not produce a second workspace on retry — recovery looks
// the deterministic request ID up before recreating.
func TestRecovery_APIRestartCreateDedup(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	svc := provisioning.NewService(db)

	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.SetQuota(ctx, tx, "tenant-a", provisioning.ResourceVector{
			RunningSlots: 5, CPUMillis: 8000, MemoryBytes: 1 << 34, DiskBytes: 1 << 40,
		})
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	req := provisioning.CreateRequest{
		OwnerIssuer: "https://issuer.test", OwnerSubject: "sub-1",
		Name:         "box",
		Template:     provisioning.TemplateInfo{ID: "tpl_x", Name: "x", Revision: 1, Runtime: "linux", Experience: "desktop"},
		Vector:       quotaVec,
		DesiredState: "Running",
		DataPolicy:   "Ephemeral",
	}
	first, err := svc.CreateWorkspace(ctx, "tenant-a", "idem-1", req, provisioning.RequestHash(req))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Simulated retry after a crash between commit and response: same
	// idempotency key must return the same record, never a duplicate.
	second, err := svc.CreateWorkspace(ctx, "tenant-a", "idem-1", req, provisioning.RequestHash(req))
	if err != nil {
		t.Fatalf("retry create: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("retry produced a new workspace %q vs %q", second.ID, first.ID)
	}
	var n int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM workspaces WHERE tenant_id='tenant-a'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("crash-retry created %d workspaces, want exactly 1", n)
	}

	// The recovered pass must re-drive the still-pending create intent —
	// missing behaviour until the implementation landed.
	rec := &recordingApplier{}
	if _, err := provisioning.NewRecovery(db, &fakeObserver{}).Recover(ctx, rec); err != nil {
		t.Fatalf("Recover after API-restart dedup: %v", err)
	}
	if len(rec.intents) != 1 || rec.intents[0].WorkspaceUID != provisioning.PlatformID(first.ID) {
		t.Fatalf("recovery delivered %+v, want the pending create for %s",
			rec.intents, first.ID)
	}
}

// TestSettleQuota_DeletedWorkspaceKeepsRetainedDiskHeld: settling a deleted
// workspace whose home was retained frees its compute and, in the SAME
// transaction, re-holds the retained disk — no later repair pass needed.
func TestSettleQuota_DeletedWorkspaceKeepsRetainedDiskHeld(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	const tenant = "tenant-rh"
	seedHeldWorkspace(t, db, tenant, "ws_rhsrc", quotaVec)
	if _, err := db.Pool().Exec(ctx, `UPDATE workspaces SET state = 'deleted' WHERE id = 'ws_rhsrc'`); err != nil {
		t.Fatal(err)
	}
	if _, err := provisioning.NewRetainedStore(db).ImportRetained(ctx, provisioning.RetainedDiskInfo{
		TenantID: tenant, Owner: "iss|sub-1", SourceWorkspaceID: "ws_rhsrc",
		Runtime: "LinuxContainer", SizeBytes: 3 << 30,
		PVCNamespace: "ns-it", PVCName: "pvc-rh", PVCUID: "uid-pvc-rh",
	}); err != nil {
		t.Fatalf("import: %v", err)
	}
	rec := provisioning.NewRecovery(db, &fakeObserver{gone: true})
	if err := rec.SettleQuota(ctx, tenant, "ws_rhsrc"); err != nil {
		t.Fatalf("settle: %v", err)
	}
	state, v, _, _ := resState(t, db, "ws_rhsrc")
	if state != "held" || v != (provisioning.ResourceVector{DiskBytes: 3 << 30}) {
		t.Fatalf("after settle: %s %+v, want held disk-only 3 GiB", state, v)
	}
	actions, err := rec.Recover(ctx, &recordingApplier{})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("recovery found work after an in-transaction settle: %+v", actions)
	}
}

// startDuringObserve runs fire once inside RuntimeGone — the window between
// the settle candidate check and the release transaction — and then still
// reports the runtime gone (the new pod is not created yet).
type startDuringObserve struct {
	fire func()
}

func (s *startDuringObserve) RuntimeGone(context.Context, provisioning.PlatformID) (bool, error) {
	if s.fire != nil {
		f := s.fire
		s.fire = nil
		f()
	}
	return true, nil
}

// settleRaceEnv seeds a tenant with RunningSlots 1 and a per-user limit of
// 1, creates one Running Ephemeral workspace for iss|sub, stops it and acks
// the stop intent. It returns the service, the workspace id and a create
// helper for a second workspace of the same user.
func settleRaceEnv(t *testing.T, tenant string) (*store.DB, *provisioning.Service, string, func(name string) error) {
	t.Helper()
	db := recoveryDB(t)
	ctx := context.Background()
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.SetQuota(ctx, tx, tenant, provisioning.ResourceVector{
			RunningSlots: 1, CPUMillis: 64000, MemoryBytes: 128 << 30, DiskBytes: 4 << 40})
	}); err != nil {
		t.Fatal(err)
	}
	ulSetDefault(t, db, tenant, i64(1))
	svc := provisioning.NewService(db)
	mk := func(name string) (provisioning.WorkspaceRecord, error) {
		return svc.CreateWorkspace(ctx, tenant, "key-"+name, provisioning.CreateRequest{
			OwnerIssuer: "iss", OwnerSubject: "sub", Name: name,
			Template: provisioning.TemplateInfo{ID: "t", Name: "tpl"},
			Vector:   quotaVec, DesiredState: "Running", DataPolicy: "Ephemeral",
		}, []byte(name))
	}
	ws1, err := mk("ws-1")
	if err != nil {
		t.Fatalf("create ws-1: %v", err)
	}
	if _, err := mk("ws-2"); err == nil {
		t.Fatal("control: a second running workspace must be refused before any settle")
	}
	if _, err := svc.SignalWorkspace(ctx, tenant, "iss|sub", "iss|sub", ws1.ID, "", provisioning.IntentStop, nil); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE outbox_intent SET dispatched_at = now()`); err != nil {
		t.Fatal(err)
	}
	create := func(name string) error { _, err := mk(name); return err }
	return db, svc, ws1.ID, create
}

func reservationState(t *testing.T, db *store.DB, wsID string) string {
	t.Helper()
	var st string
	if err := db.Pool().QueryRow(context.Background(),
		`SELECT state FROM quota_reservation WHERE workspace_id = $1`, wsID).Scan(&st); err != nil {
		t.Fatalf("reservation state: %v", err)
	}
	return st
}

// TestSettleQuota_StartRaceKeepsReservation (R1-01): a :start that commits
// while the settle pass is proving absence keeps the reservation held, so a
// second workspace over the user limit and the tenant quota is refused.
func TestSettleQuota_StartRaceKeepsReservation(t *testing.T) {
	const tenant = "tenant-race"
	ctx := context.Background()
	db, svc, ws1, create := settleRaceEnv(t, tenant)
	obs := &startDuringObserve{fire: func() {
		if _, err := svc.SignalWorkspace(ctx, tenant, "iss|sub", "iss|sub", ws1, "", provisioning.IntentStart, nil); err != nil {
			t.Errorf("racing start: %v", err)
		}
	}}
	trig := provisioning.NewSettleTrigger()
	w := &provisioning.SettleWorker{DB: db, Trigger: trig, Rec: provisioning.NewRecovery(db, obs)}
	trig.Enqueue(provisioning.PlatformID(ws1))
	w.DrainOnce(ctx)

	if st := reservationState(t, db, ws1); st != "held" {
		t.Fatalf("reservation of the restarted workspace = %q, want held", st)
	}
	if err := create("ws-2b"); err == nil {
		t.Fatal("BYPASS: a second workspace was admitted over user limit 1 and tenant RunningSlots 1")
	}

	// The direct call reports the race as an unproven absence.
	if err := provisioning.NewRecovery(db, &fakeObserver{gone: true}).SettleQuota(ctx, tenant, provisioning.PlatformID(ws1)); !errors.Is(err, provisioning.ErrRuntimeNotProvenGone) {
		t.Fatalf("SettleQuota on a workspace wanted Running = %v, want ErrRuntimeNotProvenGone", err)
	}

	// A stop-then-start-then-stop inside the window moves the generation:
	// desired is Stopped again, but the absence proof is for an older
	// generation, so the hold stays.
	if _, err := svc.SignalWorkspace(ctx, tenant, "iss|sub", "iss|sub", ws1, "", provisioning.IntentStop, nil); err != nil {
		t.Fatalf("stop again: %v", err)
	}
	obs2 := &startDuringObserve{fire: func() {
		for _, k := range []provisioning.IntentKind{provisioning.IntentStart, provisioning.IntentStop} {
			if _, err := svc.SignalWorkspace(ctx, tenant, "iss|sub", "iss|sub", ws1, "", k, nil); err != nil {
				t.Errorf("racing %s: %v", k, err)
			}
		}
	}}
	err := provisioning.NewRecovery(db, obs2).SettleQuota(ctx, tenant, provisioning.PlatformID(ws1))
	if !errors.Is(err, provisioning.ErrSettleRaced) {
		t.Fatalf("SettleQuota across a generation move = %v, want ErrSettleRaced", err)
	}
	if st := reservationState(t, db, ws1); st != "held" {
		t.Fatalf("reservation after a generation move = %q, want held", st)
	}
}

// TestSettleQuota_StoppedWorkspaceStillReleases (Review Focus 3): the row
// lock must not leak quota — a workspace that stays Stopped is released on
// the settle pass, and a deleted one too.
func TestSettleQuota_StoppedWorkspaceStillReleases(t *testing.T) {
	const tenant = "tenant-settle-ok"
	ctx := context.Background()
	db, svc, ws1, create := settleRaceEnv(t, tenant)
	trig := provisioning.NewSettleTrigger()
	w := &provisioning.SettleWorker{DB: db, Trigger: trig, Rec: provisioning.NewRecovery(db, &fakeObserver{gone: true})}
	trig.Enqueue(provisioning.PlatformID(ws1))
	w.DrainOnce(ctx)
	if st := reservationState(t, db, ws1); st != "released" {
		t.Fatalf("stopped workspace reservation = %q, want released", st)
	}
	if err := create("ws-3"); err != nil {
		t.Fatalf("create after a proven stop: %v", err)
	}

	// Deleted: the new workspace is deleted, settles and frees its slot.
	var ws3 string
	if err := db.Pool().QueryRow(ctx, `SELECT id FROM workspaces WHERE name = 'ws-3'`).Scan(&ws3); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SignalWorkspace(ctx, tenant, "iss|sub", "iss|sub", ws3, "", provisioning.IntentDelete, nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE outbox_intent SET dispatched_at = now()`); err != nil {
		t.Fatal(err)
	}
	if err := provisioning.NewRecovery(db, &fakeObserver{gone: true}).SettleQuota(ctx, tenant, provisioning.PlatformID(ws3)); err != nil {
		t.Fatalf("settle deleted: %v", err)
	}
	if st := reservationState(t, db, ws3); st != "released" {
		t.Fatalf("deleted workspace reservation = %q, want released", st)
	}
}
