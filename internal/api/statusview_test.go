package api

// Coverage for the API-side Workspace CR status projection (design review):
//
//   - merge/freshness unit tests: no external deps, run anywhere;
//   - envtest + pg end-to-end: create via the real API + real provisioning
//     service (Postgres), set CR status in a real apiserver, and prove GET
//     reflects phase + ConnectionReady inside the 15 s freshness bound —
//     and that a stalled informer can never report Ready.
//
// The pg harness mirrors internal/broker/main_test.go: lazily started
// pinned container unique to this test binary, per-test database, removed
// in TestMain. Tests skip cleanly without docker or envtest assets.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeStatusView returns scripted ObservedStatus per workspace UID.
type fakeStatusView struct {
	mu    sync.Mutex
	byUID map[string]ObservedStatus
	err   error
	calls []string // uids asked for, in order
}

func (f *fakeStatusView) set(uid string, obs ObservedStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byUID == nil {
		f.byUID = map[string]ObservedStatus{}
	}
	f.byUID[uid] = obs
}

func (f *fakeStatusView) WorkspaceStatus(_ context.Context, _, uid string) (ObservedStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, uid)
	if f.err != nil {
		return ObservedStatus{}, f.err
	}
	return f.byUID[uid], nil
}

func readyConditions() []workspaceCondition {
	return []workspaceCondition{
		{Type: "Admitted", Status: "True", Reason: "TemplateResolved"},
		{Type: "StorageReady", Status: "True", Reason: "Ready"},
		{Type: "RuntimeReady", Status: "True", Reason: "Ready"},
		{Type: "ConnectionReady", Status: "True", Reason: "Ready"},
	}
}

// ---------------------------------------------------------------------------
// mergeObservedStatus — pure projection rules
// ---------------------------------------------------------------------------

func testRecord(phase string) *provisioning.WorkspaceRecord {
	return &provisioning.WorkspaceRecord{
		ID: "ws_merge0001", TenantID: "tenant-a", Name: "w",
		Phase: phase, DesiredState: "Running", DataPolicy: "Ephemeral",
	}
}

func TestMergeObserved_FreshReady(t *testing.T) {
	v := recordToView(testRecord("Pending"))
	mergeObservedStatus(&v, testRecord("Pending"), ObservedStatus{
		Fresh: true, Found: true, Phase: "Ready",
		Conditions: readyConditions(),
	})
	if v.Phase != "Ready" {
		t.Fatalf("phase=%q, want Ready", v.Phase)
	}
	var conn *workspaceCondition
	for i := range v.Conditions {
		if v.Conditions[i].Type == "ConnectionReady" {
			conn = &v.Conditions[i]
		}
	}
	if conn == nil || conn.Status != "True" {
		t.Fatalf("ConnectionReady=%+v, want True", conn)
	}
}

func TestMergeObserved_FreshFailed(t *testing.T) {
	rec := testRecord("Provisioning")
	v := recordToView(rec)
	mergeObservedStatus(&v, rec, ObservedStatus{
		Fresh: true, Found: true, Phase: "Failed",
		FailureReason: "BootDeadlineExceeded",
		Conditions: []workspaceCondition{
			{Type: "RuntimeReady", Status: "False", Reason: "BootDeadlineExceeded"},
			{Type: "Degraded", Status: "True", Reason: "BootDeadlineExceeded"},
		},
	})
	if v.Phase != "Failed" || v.FailureReason != "BootDeadlineExceeded" {
		t.Fatalf("phase=%q reason=%q, want Failed/BootDeadlineExceeded", v.Phase, v.FailureReason)
	}
}

func TestMergeObserved_CRMissingUsesDB(t *testing.T) {
	rec := testRecord("Provisioning")
	v := recordToView(rec)
	mergeObservedStatus(&v, rec, ObservedStatus{Fresh: true})
	if v.Phase != "Provisioning" || len(v.Conditions) != 0 {
		t.Fatalf("phase=%q conds=%v, want DB Provisioning with no conditions", v.Phase, v.Conditions)
	}
}

func TestMergeObserved_StaleNeverReady(t *testing.T) {
	rec := testRecord("Pending")
	v := recordToView(rec)
	// Last known was Ready — stale must still not claim it.
	mergeObservedStatus(&v, rec, ObservedStatus{
		Fresh: false, Found: true, Phase: "Ready",
		Conditions: readyConditions(),
		ObservedAt: time.Now().Add(-20 * time.Second),
	})
	if v.Phase == "Ready" {
		t.Fatal("stale view claimed Ready")
	}
	if v.Phase != "Pending" {
		t.Fatalf("phase=%q, want DB fallback Pending", v.Phase)
	}
	var stale, conn *workspaceCondition
	for i := range v.Conditions {
		switch v.Conditions[i].Type {
		case "ConnectionReady":
			conn = &v.Conditions[i]
		case "Degraded":
			stale = &v.Conditions[i]
		}
	}
	if conn != nil && conn.Status == "True" {
		t.Fatal("stale view kept ConnectionReady=True")
	}
	if stale == nil || stale.Reason != ReasonObservedStale {
		t.Fatalf("no StatusStale marker: %+v", v.Conditions)
	}
}

func TestMergeObserved_StaleKeepsLastKnownNonReady(t *testing.T) {
	rec := testRecord("Provisioning")
	v := recordToView(rec)
	mergeObservedStatus(&v, rec, ObservedStatus{
		Fresh: false, Found: true, Phase: "Failed",
		FailureReason: "BootDeadlineExceeded",
		Conditions: []workspaceCondition{
			{Type: "RuntimeReady", Status: "False", Reason: "BootDeadlineExceeded"},
		},
		ObservedAt: time.Now().Add(-20 * time.Second),
	})
	if v.Phase != "Failed" {
		t.Fatalf("phase=%q, want last known Failed", v.Phase)
	}
	if v.FailureReason != "BootDeadlineExceeded" {
		t.Fatalf("failureReason=%q, want BootDeadlineExceeded", v.FailureReason)
	}
	var stale *workspaceCondition
	for i := range v.Conditions {
		if v.Conditions[i].Reason == ReasonObservedStale {
			stale = &v.Conditions[i]
		}
	}
	if stale == nil {
		t.Fatalf("no StatusStale marker: %+v", v.Conditions)
	}
}

func TestMergeObserved_StaleNoSnapshot(t *testing.T) {
	rec := testRecord("Provisioning")
	v := recordToView(rec)
	mergeObservedStatus(&v, rec, ObservedStatus{Fresh: false, ObservedAt: time.Now()})
	if v.Phase != "Provisioning" {
		t.Fatalf("phase=%q, want DB Provisioning", v.Phase)
	}
	if len(v.Conditions) != 1 || v.Conditions[0].Reason != ReasonObservedStale {
		t.Fatalf("conds=%+v, want single StatusStale marker", v.Conditions)
	}
}

// ---------------------------------------------------------------------------
// K8sStatusView freshness bookkeeping — no informer needed
// ---------------------------------------------------------------------------

func TestK8sStatusView_UnsyncedIsStale(t *testing.T) {
	sv := &K8sStatusView{
		now: time.Now, maxStale: time.Second,
		lastKnown: map[string]ObservedStatus{},
	}
	obs, err := sv.WorkspaceStatus(context.Background(), "tenant-a", "ws_x")
	if err != nil {
		t.Fatal(err)
	}
	if obs.Fresh || obs.Found {
		t.Fatalf("unsynced view = %+v, want stale+unfound", obs)
	}
}

func TestK8sStatusView_FakeClockStaleness(t *testing.T) {
	now := time.Now()
	sv := &K8sStatusView{
		now: func() time.Time { return now }, maxStale: 15 * time.Second,
		lastKnown: map[string]ObservedStatus{},
	}
	sv.MarkSynced()
	// Remember a fresh Ready snapshot, then jump past the freshness budget.
	sv.remember("ws_x", ObservedStatus{
		Found: true, Phase: "Ready", Conditions: readyConditions(),
	})
	now = now.Add(16 * time.Second)
	obs, err := sv.WorkspaceStatus(context.Background(), "tenant-a", "ws_x")
	if err != nil {
		t.Fatal(err)
	}
	if obs.Fresh {
		t.Fatal("aged informer still fresh")
	}
	if obs.Phase != "Ready" {
		t.Fatalf("snapshot phase=%q, want last known Ready", obs.Phase)
	}
}

// ---------------------------------------------------------------------------
// Handler-level projection via fake StatusView (no kube, no pg)
// ---------------------------------------------------------------------------

func newStatusEnv(t *testing.T, be workspaceBackend, sv StatusView) *testEnv {
	t.Helper()
	return newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants(),
		func(h *WorkspaceHandler) { h.WithStatusView(sv) })
}

func createWorkspace(t *testing.T, env *testEnv, sess, csrf *http.Cookie, name, key string) WorkspaceView {
	t.Helper()
	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
		fmt.Sprintf(`{"name":%q,"templateRef":"tpl_linuxdesktop","desiredState":"Running"}`, name),
		map[string]string{"Idempotency-Key": key})
	view := decodeBody[WorkspaceView](t, r)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create %s: status=%d view=%+v", name, r.StatusCode, view)
	}
	return view
}

func getWorkspace(t *testing.T, env *testEnv, sess, csrf *http.Cookie, id string) (WorkspaceView, map[string]any) {
	t.Helper()
	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/workspaces/"+id, "", nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status=%d", id, r.StatusCode)
	}
	var raw map[string]any
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	r.Body.Close()
	b, _ := json.Marshal(raw)
	var v WorkspaceView
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode view: %v", err)
	}
	return v, raw
}

func findCond(v WorkspaceView, typ string) *workspaceCondition {
	for i := range v.Conditions {
		if v.Conditions[i].Type == typ {
			return &v.Conditions[i]
		}
	}
	return nil
}

func TestWorkspaceView_ReadyProjection(t *testing.T) {
	be := newFakeBackend()
	sv := &fakeStatusView{}
	env := newStatusEnv(t, be, sv)
	sess, csrf := login(t, env, "user-a")

	created := createWorkspace(t, env, sess, csrf, "rdesk", "key-sv-ready01")
	sv.set(created.ID, ObservedStatus{
		Fresh: true, Found: true, Phase: "Ready", Conditions: readyConditions(),
		ObservedAt: time.Now(),
	})

	v, raw := getWorkspace(t, env, sess, csrf, created.ID)
	if v.Phase != "Ready" {
		t.Fatalf("phase=%q, want Ready (CR projection)", v.Phase)
	}
	if c := findCond(v, "ConnectionReady"); c == nil || c.Status != "True" {
		t.Fatalf("ConnectionReady=%+v, want True", c)
	}
	// No control-plane internals may leak into the public view.
	for _, k := range []string{"serviceRef", "runtimeUID", "runtimeGeneration",
		"observedRuntimeGeneration", "namespace", "uid"} {
		if _, ok := raw[k]; ok {
			t.Fatalf("internal field %q exposed in view: %v", k, raw)
		}
	}
}

func TestWorkspaceView_FailedProjection(t *testing.T) {
	be := newFakeBackend()
	sv := &fakeStatusView{}
	env := newStatusEnv(t, be, sv)
	sess, csrf := login(t, env, "user-a")

	created := createWorkspace(t, env, sess, csrf, "fdesk", "key-sv-failed1")
	sv.set(created.ID, ObservedStatus{
		Fresh: true, Found: true, Phase: "Failed", FailureReason: "BootDeadlineExceeded",
		Conditions: []workspaceCondition{
			{Type: "RuntimeReady", Status: "False", Reason: "BootDeadlineExceeded"},
			{Type: "Degraded", Status: "True", Reason: "BootDeadlineExceeded"},
		},
		ObservedAt: time.Now(),
	})

	v, _ := getWorkspace(t, env, sess, csrf, created.ID)
	if v.Phase != "Failed" || v.FailureReason != "BootDeadlineExceeded" {
		t.Fatalf("phase=%q reason=%q, want Failed/BootDeadlineExceeded", v.Phase, v.FailureReason)
	}
}

func TestWorkspaceView_MissingCRUsesDBPhase(t *testing.T) {
	be := newFakeBackend()
	sv := &fakeStatusView{}
	env := newStatusEnv(t, be, sv)
	sess, csrf := login(t, env, "user-a")

	created := createWorkspace(t, env, sess, csrf, "mdesk", "key-sv-missing")
	sv.set(created.ID, ObservedStatus{Fresh: true, Found: false, ObservedAt: time.Now()})

	v, _ := getWorkspace(t, env, sess, csrf, created.ID)
	if v.Phase != "Pending" || len(v.Conditions) != 0 {
		t.Fatalf("phase=%q conds=%v, want DB Pending + no conditions", v.Phase, v.Conditions)
	}
}

func TestWorkspaceView_StaleCannotBeReady(t *testing.T) {
	be := newFakeBackend()
	sv := &fakeStatusView{}
	env := newStatusEnv(t, be, sv)
	sess, csrf := login(t, env, "user-a")

	created := createWorkspace(t, env, sess, csrf, "sdesk", "key-sv-stale01")
	// Stale informer with a last-known Ready snapshot.
	sv.set(created.ID, ObservedStatus{
		Fresh: false, Found: true, Phase: "Ready", Conditions: readyConditions(),
		ObservedAt: time.Now().Add(-30 * time.Second),
	})

	v, _ := getWorkspace(t, env, sess, csrf, created.ID)
	if v.Phase == "Ready" {
		t.Fatal("stale informer reported Ready")
	}
	if c := findCond(v, "ConnectionReady"); c != nil && c.Status == "True" {
		t.Fatal("stale informer kept ConnectionReady=True")
	}
	if c := findCond(v, "Degraded"); c == nil || c.Reason != ReasonObservedStale {
		t.Fatalf("no StatusStale marker: %+v", v.Conditions)
	}
}

func TestWorkspaceView_StatusErrorFailsClosed(t *testing.T) {
	be := newFakeBackend()
	sv := &fakeStatusView{err: errors.New("cache exploded")}
	env := newStatusEnv(t, be, sv)
	sess, csrf := login(t, env, "user-a")

	created := createWorkspace(t, env, sess, csrf, "edesk", "key-sv-err0001")
	v, _ := getWorkspace(t, env, sess, csrf, created.ID)
	if v.Phase == "Ready" {
		t.Fatal("status backend error reported Ready")
	}
	if c := findCond(v, "Degraded"); c == nil || c.Reason != ReasonObservedStale {
		t.Fatalf("no StatusStale marker on error: %+v", v.Conditions)
	}
}

func TestWorkspaceList_ProjectsPhases(t *testing.T) {
	be := newFakeBackend()
	sv := &fakeStatusView{}
	env := newStatusEnv(t, be, sv)
	sess, csrf := login(t, env, "user-a")

	a := createWorkspace(t, env, sess, csrf, "list-a", "key-sv-list-a1")
	b := createWorkspace(t, env, sess, csrf, "list-b", "key-sv-list-b1")
	sv.set(a.ID, ObservedStatus{
		Fresh: true, Found: true, Phase: "Ready", Conditions: readyConditions(),
		ObservedAt: time.Now(),
	})
	sv.set(b.ID, ObservedStatus{Fresh: true, Found: false, ObservedAt: time.Now()})

	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/workspaces", "", nil)
	list := decodeBody[WorkspaceList](t, r)
	r.Body.Close()
	if len(list.Items) != 2 {
		t.Fatalf("list items=%d, want 2", len(list.Items))
	}
	phases := map[string]string{}
	for _, it := range list.Items {
		phases[it.ID] = it.Phase
	}
	if phases[a.ID] != "Ready" || phases[b.ID] != "Pending" {
		t.Fatalf("list phases=%v, want %s=Ready %s=Pending", phases, a.ID, b.ID)
	}
}

// ---------------------------------------------------------------------------
// pg + envtest harness
// ---------------------------------------------------------------------------

// svPgImage is the locked postgres candidate digest from
// docs/compatibility.md (data_plane.postgresql).
const svPgImage = "docker.io/library/postgres:18.0@sha256:41fc5342eefba6cc2ccda736aaf034bbbb7c3df0fdb81516eba1ba33f360162c"

var (
	svPgOnce      sync.Once
	svPgStartErr  error
	svPgAdminDSN  string
	svPgContainer string
	svDBCounter   atomic.Int64
)

func TestMain(m *testing.M) {
	code := m.Run()
	if svPgContainer != "" {
		_ = exec.Command("docker", "rm", "-f", svPgContainer).Run()
	}
	os.Exit(code)
}

func svStartPostgres() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("docker not found")
	}
	var pwBytes [16]byte
	if _, err := rand.Read(pwBytes[:]); err != nil {
		return err
	}
	pw := hex.EncodeToString(pwBytes[:])
	var sfx [4]byte
	if _, err := rand.Read(sfx[:]); err != nil {
		return err
	}
	svPgContainer = fmt.Sprintf("tcdi-it-w5view-pg-%d-%s", os.Getpid(), hex.EncodeToString(sfx[:]))
	if out, err := exec.Command("docker", "run", "-d",
		"--name", svPgContainer,
		"-e", "POSTGRES_PASSWORD="+pw,
		"-e", "POSTGRES_DB=postgres",
		"-p", "127.0.0.1::5432",
		svPgImage).CombinedOutput(); err != nil {
		return fmt.Errorf("docker run postgres: %v\n%s", err, out)
	}
	portOut, err := exec.Command("docker", "port", svPgContainer, "5432").Output()
	if err != nil {
		return fmt.Errorf("docker port: %w", err)
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
		return fmt.Errorf("no 127.0.0.1 port mapping for postgres: %q", portOut)
	}
	svPgAdminDSN = fmt.Sprintf("postgres://postgres:%s@%s/postgres?sslmode=disable&pool_max_conns=16", pw, host)
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		db, err := store.Open(ctx, svPgAdminDSN)
		cancel()
		if err == nil {
			db.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("postgres did not become ready: %w", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func svEnsurePostgres(t *testing.T) {
	t.Helper()
	svPgOnce.Do(func() { svPgStartErr = svStartPostgres() })
	if svPgStartErr != nil {
		if strings.Contains(svPgStartErr.Error(), "docker not found") {
			t.Skipf("postgres test container unavailable: %v", svPgStartErr)
		}
		t.Fatalf("postgres test container: %v", svPgStartErr)
	}
}

// svNewDB creates a fresh migrated database inside the shared container.
func svNewDB(t *testing.T) *store.DB {
	t.Helper()
	svEnsurePostgres(t)
	ctx := context.Background()
	admin, err := store.Open(ctx, svPgAdminDSN)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	name := fmt.Sprintf("w5v_%d_%d", time.Now().UnixNano()%1_000_000, svDBCounter.Add(1))
	if _, err := admin.Pool().Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatalf("create database: %v", err)
	}
	dbURL := strings.Replace(svPgAdminDSN, "/postgres?", "/"+name+"?", 1)
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

func svEnvtestAssets(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("KUBEBUILDER_ASSETS"); dir != "" {
		return dir
	}
	matches, _ := filepath.Glob(filepath.Join("..", "..", "bin", "k8s", "*-linux-amd64"))
	if len(matches) == 0 {
		t.Skip("KUBEBUILDER_ASSETS unset and no bin/k8s/*-linux-amd64 found")
	}
	return matches[0]
}

// ---------------------------------------------------------------------------
// Full-stack: API + real Service (pg) + real informer (envtest)
// ---------------------------------------------------------------------------

func TestStatusProjection_EndToEnd(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := workspacesv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory: svEnvtestAssets(t),
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	restCfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	kc, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "w5view-"}}
	if err := kc.Create(ctx, ns); err != nil {
		t.Fatalf("namespace: %v", err)
	}

	resync := time.Second
	kcache, err := crcache.New(restCfg, crcache.Options{
		Scheme:     scheme,
		SyncPeriod: &resync,
		DefaultNamespaces: map[string]crcache.Config{
			ns.Name: {},
		},
	})
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	sv, err := NewK8sStatusView(ctx, kcache)
	if err != nil {
		t.Fatalf("status view: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go func() { _ = kcache.Start(cctx) }()
	if !kcache.WaitForCacheSync(cctx) {
		t.Fatal("cache did not sync")
	}
	sv.MarkSynced()

	// Real provisioning service on real Postgres; quota row needed for create.
	db := svNewDB(t)
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.SetQuota(ctx, tx, "tenant-a", provisioning.ResourceVector{
			RunningSlots: 4, CPUMillis: 16000, MemoryBytes: 64 << 30, DiskBytes: 1 << 40,
		})
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	svc := provisioning.NewService(db)

	apiEnv := newWorkspaceEnv(t, svc, defaultCatalog(),
		StaticTenantResolver{"tenant-a": ns.Name},
		func(h *WorkspaceHandler) { h.WithStatusView(sv) })
	sess, csrf := login(t, apiEnv, "user-a")

	created := createWorkspace(t, apiEnv, sess, csrf, "e2e-desk", "key-sv-e2e-0001")
	if created.Phase != "Pending" {
		t.Fatalf("fresh create phase=%q, want Pending", created.Phase)
	}

	// The CR the dispatcher would have applied, then operator-observed
	// status: Ready with all conditions True.
	cr := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      provisioning.WorkspaceCRName(provisioning.PlatformID(created.ID)),
			Namespace: ns.Name,
			Labels: map[string]string{
				provisioning.LabelWorkspaceUID: created.ID,
				provisioning.LabelTenant:       "tenant-a",
			},
		},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			TemplateRef: workspacesv1alpha1.TemplateReference{Name: "linuxdesktop"},
			OwnerSubject: workspacesv1alpha1.OwnerSubject{
				Issuer: "https://issuer.test", Subject: "user-a",
			},
			DesiredState:      workspacesv1alpha1.DesiredStateRunning,
			DataPolicy:        workspacesv1alpha1.DataPolicyEphemeral,
			RuntimeGeneration: 1,
			IntentRevision:    1,
		},
	}
	if err := kc.Create(ctx, cr); err != nil {
		t.Fatalf("create CR: %v", err)
	}
	setCRStatus := func(phase workspacesv1alpha1.WorkspacePhase, conds []metav1.Condition) {
		t.Helper()
		var cur workspacesv1alpha1.Workspace
		if err := kc.Get(ctx, client.ObjectKeyFromObject(cr), &cur); err != nil {
			t.Fatalf("get CR: %v", err)
		}
		cur.Status.Phase = phase
		cur.Status.Conditions = conds
		if err := kc.Status().Update(ctx, &cur); err != nil {
			t.Fatalf("status update: %v", err)
		}
	}
	now := metav1.NewTime(time.Now())
	setCRStatus(workspacesv1alpha1.WorkspacePhaseReady, []metav1.Condition{
		{Type: workspacesv1alpha1.ConditionAdmitted, Status: metav1.ConditionTrue,
			Reason: "TemplateResolved", LastTransitionTime: now},
		{Type: workspacesv1alpha1.ConditionStorageReady, Status: metav1.ConditionTrue,
			Reason: "Ready", LastTransitionTime: now},
		{Type: workspacesv1alpha1.ConditionRuntimeReady, Status: metav1.ConditionTrue,
			Reason: "Ready", LastTransitionTime: now},
		{Type: workspacesv1alpha1.ConditionConnectionReady, Status: metav1.ConditionTrue,
			Reason: "Ready", LastTransitionTime: now},
		{Type: workspacesv1alpha1.ConditionDegraded, Status: metav1.ConditionFalse,
			Reason: "Nominal", LastTransitionTime: now},
	})

	// GET must reflect the CR inside the 15 s freshness bound.
	var v WorkspaceView
	deadline := time.Now().Add(15 * time.Second)
	for {
		v, _ = getWorkspace(t, apiEnv, sess, csrf, created.ID)
		if v.Phase == "Ready" && findCond(v, "ConnectionReady") != nil &&
			findCond(v, "ConnectionReady").Status == "True" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("phase=%q conds=%+v — CR Ready never projected within 15s", v.Phase, v.Conditions)
		}
		time.Sleep(200 * time.Millisecond)
	}
	// No internal refs in the wire view.
	_, raw := getWorkspace(t, apiEnv, sess, csrf, created.ID)
	for _, k := range []string{"serviceRef", "runtimeUID", "runtimeGeneration",
		"observedRuntimeGeneration", "namespace"} {
		if _, ok := raw[k]; ok {
			t.Fatalf("internal field %q exposed: %v", k, raw)
		}
	}

	// CR goes Failed -> view reports Failed with the operator's reason.
	setCRStatus(workspacesv1alpha1.WorkspacePhaseFailed, []metav1.Condition{
		{Type: workspacesv1alpha1.ConditionRuntimeReady, Status: metav1.ConditionFalse,
			Reason: "BootDeadlineExceeded", LastTransitionTime: now},
		{Type: workspacesv1alpha1.ConditionConnectionReady, Status: metav1.ConditionFalse,
			Reason: "Provisioning", LastTransitionTime: now},
		{Type: workspacesv1alpha1.ConditionDegraded, Status: metav1.ConditionTrue,
			Reason: "BootDeadlineExceeded", LastTransitionTime: now},
	})
	deadline = time.Now().Add(15 * time.Second)
	for {
		v, _ = getWorkspace(t, apiEnv, sess, csrf, created.ID)
		if v.Phase == "Failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("phase=%q — CR Failed never projected", v.Phase)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if v.FailureReason != "BootDeadlineExceeded" {
		t.Fatalf("failureReason=%q, want BootDeadlineExceeded", v.FailureReason)
	}

	// A second workspace with no CR at all keeps its DB intent phase.
	orphan := createWorkspace(t, apiEnv, sess, csrf, "e2e-orphan", "key-sv-e2e-0002")
	v, _ = getWorkspace(t, apiEnv, sess, csrf, orphan.ID)
	if v.Phase != "Pending" || len(v.Conditions) != 0 {
		t.Fatalf("CR-missing phase=%q conds=%v, want DB Pending + no conditions", v.Phase, v.Conditions)
	}

	// List endpoint shows projected phases for both.
	r := doReq(t, apiEnv, sess, csrf, http.MethodGet, "/v1/workspaces", "", nil)
	list := decodeBody[WorkspaceList](t, r)
	r.Body.Close()
	phases := map[string]string{}
	for _, it := range list.Items {
		phases[it.ID] = it.Phase
	}
	if phases[created.ID] != "Failed" || phases[orphan.ID] != "Pending" {
		t.Fatalf("list phases=%v, want Failed + Pending", phases)
	}

	// Stale informer: advance the view's clock past the freshness budget.
	realNow := time.Now()
	sv.now = func() time.Time { return realNow.Add(30 * time.Second) }
	v, _ = getWorkspace(t, apiEnv, sess, csrf, created.ID)
	if v.Phase == "Ready" {
		t.Fatal("stale informer reported Ready")
	}
	if c := findCond(v, "ConnectionReady"); c != nil && c.Status == "True" {
		t.Fatal("stale informer kept ConnectionReady=True")
	}
	if c := findCond(v, "Degraded"); c == nil || c.Reason != ReasonObservedStale {
		t.Fatalf("no StatusStale marker while stale: %+v", v.Conditions)
	}
	// Last known was Failed (non-Ready): the stale view may keep it.
	if v.Phase != "Failed" {
		t.Fatalf("stale phase=%q, want last known Failed", v.Phase)
	}
}
