package provisioning_test

// Tests for K8sRuntimeObserver — the production proof-of-absence oracle
// Recovery consults before releasing held quota (design §8). Fake-client
// tests cover the decision table; an envtest+pg test proves the wired
// settle path: a held reservation is released only once the runtime
// children are gone, and never on K8s API errors.
//
// Container/dep rules: uses the shared provisioning_test pg harness
// (recoveryDB) and bin/k8s envtest assets; creates no docker resources of
// its own.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	workspacev1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

func observerScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := workspacev1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func obsCR(name, ns, platformUID, crUID string) *workspacev1alpha1.Workspace {
	return &workspacev1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       types.UID(crUID),
			Labels: map[string]string{
				provisioning.LabelWorkspaceUID: platformUID,
				provisioning.LabelTenant:       "tenant-a",
			},
		},
		Spec: workspacev1alpha1.WorkspaceSpec{
			TemplateRef:    workspacev1alpha1.TemplateReference{Name: "tpl"},
			OwnerSubject:   workspacev1alpha1.OwnerSubject{Issuer: "i", Subject: "s"},
			DesiredState:   workspacev1alpha1.DesiredStateStopped,
			DataPolicy:     workspacev1alpha1.DataPolicyEphemeral,
			IntentRevision: 1,
		},
	}
}

func obsPod(name, ns, crUID, crName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels: map[string]string{
				linux.LabelWorkspaceUID:  crUID,
				linux.LabelWorkspaceName: crName,
				linux.LabelRole:          linux.RoleRuntime,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "rt", Image: "rt:latest"}},
		},
	}
}

func obsPVC(name, ns, crUID string, retained bool) *corev1.PersistentVolumeClaim {
	l := map[string]string{linux.LabelWorkspaceUID: crUID}
	if retained {
		l[linux.LabelDataRetained] = "true"
	}
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: ns, Labels: l,
	}}
}

func TestK8sRuntimeObserver_DecisionTable(t *testing.T) {
	ctx := context.Background()
	scheme := observerScheme(t)
	tenants := provisioning.TenantNamespaces{"tenant-a": "ns-a", "tenant-b": "ns-b"}
	const uid = "ws_obsv0001"
	crName := provisioning.WorkspaceCRName(uid)

	// No CR at all -> runtime is proven gone.
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	obs := provisioning.NewK8sRuntimeObserver(c, tenants)
	gone, err := obs.RuntimeGone(ctx, uid)
	if err != nil || !gone {
		t.Fatalf("no CR: gone=%v err=%v, want true/nil", gone, err)
	}

	// CR exists but has no children -> gone.
	cr := obsCR(crName, "ns-a", uid, "cr-uid-1")
	c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()
	obs = provisioning.NewK8sRuntimeObserver(c, tenants)
	gone, err = obs.RuntimeGone(ctx, uid)
	if err != nil || !gone {
		t.Fatalf("CR without children: gone=%v err=%v, want true/nil", gone, err)
	}

	// A Pod carrying the CR-UID label -> not gone.
	pod := obsPod("ws-cr-uid-1", "ns-a", "cr-uid-1", crName)
	c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr, pod).Build()
	obs = provisioning.NewK8sRuntimeObserver(c, tenants)
	gone, err = obs.RuntimeGone(ctx, uid)
	if err != nil || gone {
		t.Fatalf("live pod: gone=%v err=%v, want false/nil", gone, err)
	}

	// A PVC is disk, not compute: an unlabelled (not yet retained) home
	// volume never blocks the absence proof (FX-R25).
	c = fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cr, obsPVC("ws-cr-uid-1-home", "ns-a", "cr-uid-1", false)).Build()
	obs = provisioning.NewK8sRuntimeObserver(c, tenants)
	gone, err = obs.RuntimeGone(ctx, uid)
	if err != nil || !gone {
		t.Fatalf("unretained pvc: gone=%v err=%v, want true/nil", gone, err)
	}

	// A retained PVC is data inventory, not compute -> gone.
	c = fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cr, obsPVC("ws-cr-uid-1-home", "ns-a", "cr-uid-1", true)).Build()
	obs = provisioning.NewK8sRuntimeObserver(c, tenants)
	gone, err = obs.RuntimeGone(ctx, uid)
	if err != nil || !gone {
		t.Fatalf("retained pvc: gone=%v err=%v, want true/nil", gone, err)
	}

	// CR deleted but a GC straggler remains, detectable by the
	// deterministic workspace-name label -> not gone.
	stray := obsPod("ws-stale", "ns-b", "some-other-uid", crName)
	c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(stray).Build()
	obs = provisioning.NewK8sRuntimeObserver(c, tenants)
	gone, err = obs.RuntimeGone(ctx, uid)
	if err != nil || gone {
		t.Fatalf("stray pod after CR delete: gone=%v err=%v, want false/nil", gone, err)
	}

	// Two same-named CRs in different managed namespaces -> ambiguous ->
	// error, i.e. not proven gone.
	cr2 := obsCR(crName, "ns-b", uid, "cr-uid-2")
	c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr, cr2).Build()
	obs = provisioning.NewK8sRuntimeObserver(c, tenants)
	gone, err = obs.RuntimeGone(ctx, uid)
	if err == nil || gone {
		t.Fatalf("duplicate CR: gone=%v err=%v, want false/err", gone, err)
	}
}

// errListClient fails every List — the observer must return an error
// (which Recovery maps to "not proven gone").
type errListClient struct{ client.Client }

func (e errListClient) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("simulated k8s api failure")
}

func TestK8sRuntimeObserver_APIError(t *testing.T) {
	ctx := context.Background()
	scheme := observerScheme(t)
	const uid = "ws_obsv0002"
	cr := obsCR(provisioning.WorkspaceCRName(uid), "ns-a", uid, "cr-uid-9")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()
	obs := provisioning.NewK8sRuntimeObserver(errListClient{c},
		provisioning.TenantNamespaces{"tenant-a": "ns-a"})
	gone, err := obs.RuntimeGone(ctx, uid)
	if err == nil || gone {
		t.Fatalf("api error: gone=%v err=%v, want false/err", gone, err)
	}
}

// ---------------------------------------------------------------------------
// envtest + pg: held reservation released only on proven absence
// ---------------------------------------------------------------------------

func obsEnvtestAssets(t *testing.T) string {
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

// TestRecovery_SettleWithRealObserver: seed a stopped workspace with a held
// reservation; with a live pod the pass holds quota; once the pod is gone
// the pass releases it. A k8s error in between holds too.
func TestRecovery_SettleWithRealObserver(t *testing.T) {
	scheme := observerScheme(t)
	env := &envtest.Environment{
		BinaryAssetsDirectory: obsEnvtestAssets(t),
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
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "w5obs-"}}
	if err := kc.Create(ctx, ns); err != nil {
		t.Fatalf("namespace: %v", err)
	}
	tenants := provisioning.TenantNamespaces{"tenant-a": ns.Name}

	db := recoveryDB(t)
	const uid = "ws_settle01"
	seedHeldWorkspace(t, db, "tenant-a", uid, quotaVec)

	// The workspace CR + a running-runtime pod (label = CR UID).
	crName := provisioning.WorkspaceCRName(uid)
	cr := &workspacev1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      crName,
			Namespace: ns.Name,
			Labels: map[string]string{
				provisioning.LabelWorkspaceUID: uid,
				provisioning.LabelTenant:       "tenant-a",
			},
		},
		Spec: workspacev1alpha1.WorkspaceSpec{
			TemplateRef:    workspacev1alpha1.TemplateReference{Name: "tpl"},
			OwnerSubject:   workspacev1alpha1.OwnerSubject{Issuer: "i", Subject: "s"},
			DesiredState:   workspacev1alpha1.DesiredStateStopped,
			DataPolicy:     workspacev1alpha1.DataPolicyEphemeral,
			IntentRevision: 1,
		},
	}
	if err := kc.Create(ctx, cr); err != nil {
		t.Fatalf("create CR: %v", err)
	}
	crUID := string(cr.UID)
	pod := obsPod("ws-"+crUID, ns.Name, crUID, crName)
	if err := kc.Create(ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	rec := provisioning.NewRecovery(db, provisioning.NewK8sRuntimeObserver(kc, tenants))
	applier := provisioning.NewK8sApplier(kc, tenants)

	// PendingRecovery sees the held reservation on the terminal workspace.
	pend, err := rec.PendingRecovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range pend {
		if p == uid {
			found = true
		}
	}
	if !found {
		t.Fatalf("PendingRecovery=%v, want %s present", pend, uid)
	}

	// Pass 1: pod alive -> hold.
	actions, err := rec.Recover(ctx, applier)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	assertAction(t, actions, uid, provisioning.ActionHoldQuota)
	if used, _ := provisioning.HeldUsage(ctx, db.Pool(), "tenant-a"); used != quotaVec {
		t.Fatalf("released while pod alive: %+v", used)
	}

	// K8s API failure mid-flight: a wrapped client that errors -> hold.
	recErr := provisioning.NewRecovery(db,
		provisioning.NewK8sRuntimeObserver(errListClient{kc}, tenants))
	if err := recErr.SettleQuota(ctx, "tenant-a", uid); !errors.Is(err, provisioning.ErrRuntimeNotProvenGone) {
		t.Fatalf("settle on api error = %v, want ErrRuntimeNotProvenGone", err)
	}
	if used, _ := provisioning.HeldUsage(ctx, db.Pool(), "tenant-a"); used != quotaVec {
		t.Fatalf("released on api error: %+v", used)
	}

	// Pod deleted -> absence proven -> released.
	if err := kc.Delete(ctx, pod); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	actions, err = rec.Recover(ctx, applier)
	if err != nil {
		t.Fatalf("recover after pod delete: %v", err)
	}
	assertAction(t, actions, uid, provisioning.ActionReleaseQuota)
	if used, _ := provisioning.HeldUsage(ctx, db.Pool(), "tenant-a"); used.RunningSlots != 0 {
		t.Fatalf("reservation still held after runtime gone: %+v", used)
	}

	// A fresh Recovery instance (restart) sees nothing pending for it.
	rec2 := provisioning.NewRecovery(db, provisioning.NewK8sRuntimeObserver(kc, tenants))
	pend, err = rec2.PendingRecovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pend {
		if p == uid {
			t.Fatalf("uid %s still pending after release", uid)
		}
	}
}

func assertAction(t *testing.T, actions []provisioning.RecoveryAction, uid provisioning.PlatformID, kind provisioning.RecoveryActionKind) {
	t.Helper()
	for _, a := range actions {
		if a.WorkspaceUID == uid && a.Kind == kind {
			return
		}
	}
	t.Fatalf("actions=%+v, want %s for %s", actions, kind, uid)
}
