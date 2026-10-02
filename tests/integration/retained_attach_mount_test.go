// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

//go:build integration

package integration

// FX-R20: attaching a retained disk must mount THAT disk. The attach used to
// create the consuming Workspace CR bare and stamp the retained-pvc
// annotations in a second write; a running operator reconciled in between
// and built the pod on a brand-new empty default claim, while the database
// said the retained record was Attached.
//
// Driven against real PostgreSQL and the envtest apiserver with the
// production apply chain (RetainedApplier over K8sApplier) AND the
// production WorkspaceReconciler running in a manager, so the create/patch
// window is exercised exactly as it is in the cluster.
//
// Run: go test -tags=integration ./tests/integration -run TestFXR20 -count=50

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/api"
	"github.com/tinyorbitvn/tinycdi/internal/operator"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

const fxr20Tenant = "tenant-fx-r20"

// laggyClient models the production latency between the applier's CR create
// and its follow-up work (a PostgreSQL round trip plus apiserver hops
// across the network): the applier's Workspace read waits first. The delay
// cycles 0..450ms per run so successive runs land the applier's second
// write at different points of the operator's first reconcile — the fix
// must hold for every one of them.
type laggyClient struct {
	client.Client
	delay time.Duration
}

var fxr20Runs atomic.Int64

func (c laggyClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*workspacesv1alpha1.Workspace); ok {
		time.Sleep(c.delay)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// startOperator runs the production reconciler (cached client, owned-object
// watches, bounded backoff) against the envtest apiserver, restricted to ns.
func startOperator(t *testing.T, ns string) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := workspacesv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	mgr, err := ctrl.NewManager(testEnvConfig, ctrl.Options{
		Scheme:         scheme,
		Metrics:        metricsserver.Options{BindAddress: "0"},
		Cache:          cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
		Controller:     config.Controller{SkipNameValidation: ptr.To(true)},
		LeaderElection: false,
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	r := &operator.WorkspaceReconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		Backend: linux.New(mgr.GetClient(), linux.Options{}),
	}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("setup reconciler: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = mgr.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	// The informers must be live before the attach is applied: the race is
	// the operator reacting to the CR create while the applier is still
	// working.
	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()
	if !mgr.GetCache().WaitForCacheSync(syncCtx) {
		t.Fatal("operator cache did not sync")
	}
}

func fxr20Template(t *testing.T, ns, name string) {
	t.Helper()
	tpl := &workspacesv1alpha1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: workspacesv1alpha1.WorkspaceTemplateSpec{
			Revision:   "2026-10-a",
			Runtime:    workspacesv1alpha1.RuntimeLinuxContainer,
			Experience: workspacesv1alpha1.ExperienceDesktop,
			Linux: &workspacesv1alpha1.LinuxRuntimeSpec{
				Image: "tcdi/linux-desktop@sha256:" + fmt.Sprintf("%064x", 1),
			},
			Resources: workspacesv1alpha1.ResourceProfile{
				CPU:     resource.MustParse("500m"),
				Memory:  resource.MustParse("512Mi"),
				Storage: resource.MustParse("1Gi"),
			},
			BootDeadline:    metav1.Duration{Duration: 5 * time.Minute},
			NetworkProfile:  workspacesv1alpha1.NetworkProfileIsolated,
			ClipboardPolicy: workspacesv1alpha1.ClipboardDisabled,
			Lifecycle: workspacesv1alpha1.LifecycleDefaults{
				IdleTimeout:       metav1.Duration{Duration: 30 * time.Minute},
				DisconnectTimeout: metav1.Duration{Duration: 10 * time.Minute},
				MaxDuration:       metav1.Duration{Duration: 8 * time.Hour},
				DataPolicy:        workspacesv1alpha1.DataPolicyRetain,
			},
		},
	}
	if err := k8sClient.Create(context.Background(), tpl); err != nil {
		t.Fatalf("template: %v", err)
	}
}

// TestFXR20_AttachMountsRetainedVolume: the operator is running while the
// attach intent is applied. The consuming workspace's pod must mount the
// retained PVC, and no default home claim may be created for it.
func TestFXR20_AttachMountsRetainedVolume(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	ns := newRetainedNamespace(t)
	setQuota(t, db, fxr20Tenant, provisioning.ResourceVector{
		RunningSlots: 100, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 1 << 40,
	})
	svc := provisioning.NewService(db)
	rstore := provisioning.NewRetainedStore(db)

	pvc := newRetainedPVC(t, ns, types.UID("old-cr-uid"), "old-desktop", true)
	rec, err := rstore.ImportRetained(ctx, api.RetainedDiskInfo{
		PVCNamespace: ns, PVCName: pvc.Name, PVCUID: string(pvc.UID), TenantID: fxr20Tenant,
		Owner: retOwner, SourceWorkspaceID: "ws_fxr20src001",
		SourceWorkspaceName: "old-desktop", Runtime: "LinuxContainer", SizeBytes: 20 << 30,
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	req := attachReq()
	fxr20Template(t, ns, req.Template.Name)
	ws, err := svc.AttachRetained(ctx, fxr20Tenant, retOwner, retOwner, rec.ID, "k-fxr20", req, []byte("a"))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	pending, err := provisioning.NewOutbox(db).PendingIntents(ctx, provisioning.PlatformID(ws.ID))
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending intents = %v (err %v), want 1 create", pending, err)
	}

	startOperator(t, ns)
	tmap := provisioning.TenantNamespaces{fxr20Tenant: ns}
	lag := laggyClient{Client: k8sClient, delay: time.Duration(fxr20Runs.Add(1)%10) * 50 * time.Millisecond}
	applier := provisioning.NewRetainedApplier(provisioning.NewK8sApplier(k8sClient, tmap), lag, tmap, rstore)
	t.Logf("applier second-write delay: %s", lag.delay)
	if err := applier.Apply(ctx, pending[0]); err != nil {
		t.Fatalf("apply attach: %v", err)
	}

	cr := workspaceCRByUID(t, ws.ID)
	if cr == nil {
		t.Fatal("consuming workspace CR not created")
	}
	podKey := client.ObjectKey{Namespace: ns, Name: linux.PodName(cr.UID)}
	pod := &corev1.Pod{}
	// A pod must eventually exist: the attach converges, it is not refused.
	eventually(t, "consuming pod", 30*time.Second, func() bool {
		return k8sClient.Get(ctx, podKey, pod) == nil
	})

	// The pod's home claim is the retained volume, never a fresh one.
	var claims []string
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			claims = append(claims, v.PersistentVolumeClaim.ClaimName)
		}
	}
	if len(claims) != 1 || claims[0] != pvc.Name {
		t.Fatalf("pod mounts claims %v, want exactly the retained PVC %q", claims, pvc.Name)
	}

	// No default home claim exists for the consuming workspace.
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := k8sClient.List(ctx, pvcs, client.InNamespace(ns)); err != nil {
		t.Fatalf("list pvcs: %v", err)
	}
	for _, p := range pvcs.Items {
		if p.Name == linux.PVCName(cr.UID) {
			t.Fatalf("stray default home claim %q created for the attached workspace", p.Name)
		}
	}
	if len(pvcs.Items) != 1 {
		t.Fatalf("namespace holds %d PVCs, want only the retained one", len(pvcs.Items))
	}

	// The retained volume itself is the same object, untouched but for the
	// consumer relabel (UID unchanged, not deleted, still retained-marked).
	got := &corev1.PersistentVolumeClaim{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), got); err != nil {
		t.Fatalf("retained pvc: %v", err)
	}
	if got.UID != pvc.UID || !got.DeletionTimestamp.IsZero() || got.Labels[operator.LabelDataRetained] != "true" {
		t.Fatalf("retained pvc changed: uid %s->%s deleting=%v labels=%v",
			pvc.UID, got.UID, !got.DeletionTimestamp.IsZero(), got.Labels)
	}
}
