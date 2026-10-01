package broker_test

// envtest coverage for K8sBindingSource : a real apiserver +
// informer cache proves the projection Workspace status -> RuntimeBinding
// and the freshness gate (unsynced cache refuses, synced cache projects
// phase/generation/runtimeUID/serviceRef with a fresh ObservedAt).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	"github.com/tinyorbitvn/tinycdi/internal/broker"
)

func envtestAssets(t *testing.T) string {
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

func TestK8sBindingSource_Projection(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := workspacesv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory: envtestAssets(t),
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
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "w3brk-"}}
	if err := kc.Create(context.Background(), ns); err != nil {
		t.Fatalf("namespace: %v", err)
	}

	// An informer that never starts must answer ErrFreshness, not data.
	dead, err := crcache.New(restCfg, crcache.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	deadSrc, err := broker.NewK8sBindingSource(context.Background(), dead)
	if err != nil {
		t.Fatalf("dead source: %v", err)
	}
	if _, err := deadSrc.CurrentBinding(context.Background(), "ws-x"); !errors.Is(err, broker.ErrFreshness) {
		t.Fatalf("unsynced binding = %v, want ErrFreshness", err)
	}

	// Live informer cache restricted to the managed namespace. The resync
	// doubles as the liveness heartbeat (minimum allowed is 1 s, well under
	// the 15 s freshness budget).
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
	src, err := broker.NewK8sBindingSource(context.Background(), kcache)
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	cctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = kcache.Start(cctx) }()
	if !kcache.WaitForCacheSync(cctx) {
		t.Fatal("cache did not sync")
	}
	src.MarkSynced()

	if _, err := src.CurrentBinding(context.Background(), "ws-nope"); !errors.Is(err, broker.ErrNotFound) {
		t.Fatalf("unknown uid = %v, want ErrNotFound", err)
	}

	wsUID := broker.PlatformID("ws_binding01")
	ws := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-binding01",
			Namespace: ns.Name,
			Labels: map[string]string{
				"workspaces.cdi.tinyorbit.vn/workspace-uid": string(wsUID),
				"workspaces.cdi.tinyorbit.vn/tenant":        "tenant-a",
			},
		},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			TemplateRef:       workspacesv1alpha1.TemplateReference{Name: "tpl"},
			OwnerSubject:      workspacesv1alpha1.OwnerSubject{Issuer: "https://idp.example", Subject: "alice"},
			DesiredState:      workspacesv1alpha1.DesiredStateRunning,
			DataPolicy:        workspacesv1alpha1.DataPolicyEphemeral,
			RuntimeGeneration: 4,
			IntentRevision:    1,
		},
	}
	if err := kc.Create(context.Background(), ws); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	ws.Status = workspacesv1alpha1.WorkspaceStatus{
		Phase:                     workspacesv1alpha1.WorkspacePhaseReady,
		ObservedRuntimeGeneration: 4,
		RuntimeUID:                "rt-uid-4",
		ServiceRef:                &workspacesv1alpha1.ServiceReference{Name: "ws-binding01", Port: 8443},
	}
	if err := kc.Status().Update(context.Background(), ws); err != nil {
		t.Fatalf("status update: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	var last error
	var lastB broker.RuntimeBinding
	for {
		b, err := src.CurrentBinding(context.Background(), wsUID)
		if err == nil {
			lastB = b
			if b.Phase == "Ready" && b.RuntimeGeneration == 4 && b.RuntimeUID == "rt-uid-4" &&
				b.Namespace == ns.Name && b.ServiceName == "ws-binding01" && b.ServicePort == 8443 &&
				b.TenantID == "tenant-a" && b.OwnerSubject == "https://idp.example|alice" &&
				b.CRUID == ws.UID && string(b.CRUID) != string(wsUID) {
				if time.Since(b.ObservedAt) > broker.MaxBindingAge {
					t.Fatalf("ObservedAt stale: %v", b.ObservedAt)
				}
				return
			}
			last = fmt.Errorf("incomplete projection: %+v", b)
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			t.Fatalf("binding never appeared: %v (last=%+v)", last, lastB)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
