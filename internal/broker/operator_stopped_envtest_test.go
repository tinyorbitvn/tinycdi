// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package broker_test

// FX-R24: envtest coverage for K8sRunningSource.OperatorStoppedWorkspaces —
// only a CR whose operator flipped the CURRENT intent to Stopped is listed.

import (
	"context"
	"encoding/json"
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
	"github.com/tinyorbitvn/tinycdi/internal/operator"
)

func TestK8sRunningSource_OperatorStopped(t *testing.T) {
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
		t.Fatal(err)
	}
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "fxr24-"}}
	if err := kc.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}

	mk := func(name string, desired workspacesv1alpha1.DesiredState, specRev int64, applied operator.AppliedIntent, phase workspacesv1alpha1.WorkspacePhase) {
		t.Helper()
		raw, _ := json.Marshal(applied)
		ws := &workspacesv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: ns.Name,
				Labels:      map[string]string{"workspaces.cdi.tinyorbit.vn/workspace-uid": "ws_" + name},
				Annotations: map[string]string{operator.AnnotationAppliedIntent: string(raw)},
			},
			Spec: workspacesv1alpha1.WorkspaceSpec{
				TemplateRef:       workspacesv1alpha1.TemplateReference{Name: "tpl"},
				OwnerSubject:      workspacesv1alpha1.OwnerSubject{Issuer: "https://idp.example", Subject: "alice"},
				DesiredState:      desired,
				DataPolicy:        workspacesv1alpha1.DataPolicyRetain,
				RuntimeGeneration: 2,
				IntentRevision:    specRev,
			},
		}
		if err := kc.Create(ctx, ws); err != nil {
			t.Fatal(err)
		}
		ws.Status = workspacesv1alpha1.WorkspaceStatus{Phase: phase, LastAppliedIntentRevision: applied.Revision}
		if err := kc.Status().Update(ctx, ws); err != nil {
			t.Fatal(err)
		}
	}
	stopped := operator.AppliedIntent{Revision: 3, DesiredState: workspacesv1alpha1.DesiredStateStopped, RuntimeGeneration: 2}
	running := operator.AppliedIntent{Revision: 3, DesiredState: workspacesv1alpha1.DesiredStateRunning, RuntimeGeneration: 2}

	// stuck: the operator's backstop flipped the applied intent to Stopped.
	mk("stuck", workspacesv1alpha1.DesiredStateRunning, 3, stopped, workspacesv1alpha1.WorkspacePhaseStopped)
	// stopped by the user: spec Stopped, nothing to reconcile.
	mk("byuser", workspacesv1alpha1.DesiredStateStopped, 3, stopped, workspacesv1alpha1.WorkspacePhaseStopped)
	// healthy: running.
	mk("healthy", workspacesv1alpha1.DesiredStateRunning, 3, running, workspacesv1alpha1.WorkspacePhaseReady)
	// a newer start (rev 4) the operator has not applied yet.
	mk("newstart", workspacesv1alpha1.DesiredStateRunning, 4, stopped, workspacesv1alpha1.WorkspacePhaseStopped)

	resync := time.Second
	kcache, err := crcache.New(restCfg, crcache.Options{
		Scheme: scheme, SyncPeriod: &resync,
		DefaultNamespaces: map[string]crcache.Config{ns.Name: {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go func() { _ = kcache.Start(cctx) }()
	if !kcache.WaitForCacheSync(cctx) {
		t.Fatal("cache did not sync")
	}

	src := broker.NewK8sRunningSource(kcache)
	var got []broker.OperatorStopped
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err = src.OperatorStoppedWorkspaces(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(got) != 1 || got[0].WorkspaceUID != "ws_stuck" || got[0].RuntimeGeneration != 2 || got[0].IntentRevision != 3 {
		t.Fatalf("OperatorStopped = %+v, want exactly ws_stuck gen 2 rev 3", got)
	}
}
