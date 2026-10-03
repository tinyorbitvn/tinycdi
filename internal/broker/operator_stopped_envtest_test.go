// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package broker_test

// FX-R24: envtest coverage for K8sRunningSource.OperatorStoppedWorkspaces —
// only a CR whose operator flipped the CURRENT intent to Stopped is listed.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/operator"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// brokerEnv starts an envtest apiserver and returns a client and a namespace.
func brokerEnv(t *testing.T) (client.Client, *runtime.Scheme, *rest.Config, string) {
	t.Helper()
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
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "fxr24-"}}
	if err := kc.Create(context.Background(), ns); err != nil {
		t.Fatal(err)
	}
	return kc, scheme, restCfg, ns.Name
}

// brokerSource starts an informer cache over ns and returns the source on it.
func brokerSource(t *testing.T, scheme *runtime.Scheme, restCfg *rest.Config, ns string) *broker.K8sRunningSource {
	t.Helper()
	resync := time.Second
	kcache, err := crcache.New(restCfg, crcache.Options{
		Scheme: scheme, SyncPeriod: &resync,
		DefaultNamespaces: map[string]crcache.Config{ns: {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = kcache.Start(cctx) }()
	if !kcache.WaitForCacheSync(cctx) {
		t.Fatal("cache did not sync")
	}
	return broker.NewK8sRunningSource(kcache)
}

func TestK8sRunningSource_OperatorStopped(t *testing.T) {
	kc, scheme, restCfg, nsName := brokerEnv(t)
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}

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
	// applied Running at the spec's own revision: nothing stopped.
	mk("appliedrunning", workspacesv1alpha1.DesiredStateRunning, 3, running, workspacesv1alpha1.WorkspacePhaseStopped)
	// applied Stopped for another runtime generation than the spec's.
	other := stopped
	other.RuntimeGeneration = 1
	mk("othergen", workspacesv1alpha1.DesiredStateRunning, 3, other, workspacesv1alpha1.WorkspacePhaseStopped)
	// a newer start (rev 4) the operator has not applied yet.
	mk("newstart", workspacesv1alpha1.DesiredStateRunning, 4, stopped, workspacesv1alpha1.WorkspacePhaseStopped)

	src := brokerSource(t, scheme, restCfg, ns.Name)
	var got []broker.OperatorStopped
	var err error
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
	// Let the informer deliver the remaining objects, then judge the set.
	time.Sleep(2 * time.Second)
	if got, err = src.OperatorStoppedWorkspaces(ctx); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].WorkspaceUID != "ws_stuck" || got[0].RuntimeGeneration != 2 || got[0].IntentRevision != 3 {
		t.Fatalf("OperatorStopped = %+v, want exactly ws_stuck gen 2 rev 3", got)
	}
}

// A catalog base name resolves to the newest revision for the lifecycle
// policy, as the operator resolves it — not to the platform defaults.
func TestK8sRunningSource_PolicyFromCatalogRevision(t *testing.T) {
	kc, scheme, restCfg, ns := brokerEnv(t)
	ctx := context.Background()

	tpl := &workspacesv1alpha1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name: "linux-desktop-e795139d", Namespace: ns,
			Labels: map[string]string{provisioning.LabelCatalogName: "linux-desktop"},
		},
		Spec: workspacesv1alpha1.WorkspaceTemplateSpec{
			Revision: "r1", Runtime: workspacesv1alpha1.RuntimeLinuxContainer,
			Experience: workspacesv1alpha1.ExperienceDesktop,
			Linux:      &workspacesv1alpha1.LinuxRuntimeSpec{Image: "tcdi/linux-desktop@sha256:" + fmt.Sprintf("%064x", 1)},
			Resources: workspacesv1alpha1.ResourceProfile{
				CPU: resource.MustParse("500m"), Memory: resource.MustParse("512Mi"), Storage: resource.MustParse("1Gi")},
			BootDeadline:    metav1.Duration{Duration: 5 * time.Minute},
			NetworkProfile:  workspacesv1alpha1.NetworkProfileIsolated,
			ClipboardPolicy: workspacesv1alpha1.ClipboardDisabled,
			Lifecycle: workspacesv1alpha1.LifecycleDefaults{
				IdleTimeout:       metav1.Duration{Duration: 20 * time.Minute},
				DisconnectTimeout: metav1.Duration{Duration: 5 * time.Minute},
				MaxDuration:       metav1.Duration{Duration: 4 * time.Hour},
				DataPolicy:        workspacesv1alpha1.DataPolicyEphemeral,
			},
		},
	}
	if err := kc.Create(ctx, tpl); err != nil {
		t.Fatal(err)
	}
	ws := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pol", Namespace: ns,
			Labels: map[string]string{"workspaces.cdi.tinyorbit.vn/workspace-uid": "ws_pol"},
		},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			TemplateRef:       workspacesv1alpha1.TemplateReference{Name: "linux-desktop"}, // base name
			OwnerSubject:      workspacesv1alpha1.OwnerSubject{Issuer: "https://idp.example", Subject: "alice"},
			DesiredState:      workspacesv1alpha1.DesiredStateRunning,
			DataPolicy:        workspacesv1alpha1.DataPolicyEphemeral,
			RuntimeGeneration: 1, IntentRevision: 1,
		},
	}
	if err := kc.Create(ctx, ws); err != nil {
		t.Fatal(err)
	}
	started := metav1.Now()
	ws.Status = workspacesv1alpha1.WorkspaceStatus{Phase: workspacesv1alpha1.WorkspacePhaseReady, StartedAt: &started}
	if err := kc.Status().Update(ctx, ws); err != nil {
		t.Fatal(err)
	}

	src := brokerSource(t, scheme, restCfg, ns)
	var got []broker.RunningWorkspace
	deadline := time.Now().Add(10 * time.Second)
	for {
		var err error
		if got, err = src.RunningWorkspaces(ctx); err != nil {
			t.Fatal(err)
		}
		if len(got) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(got) != 1 {
		t.Fatalf("running = %+v, want one workspace", got)
	}
	want := broker.TimeoutPolicy{IdleTimeout: 20 * time.Minute, DisconnectTimeout: 5 * time.Minute, MaxDuration: 4 * time.Hour}
	if got[0].Policy != want {
		t.Fatalf("policy = %+v, want the template's %+v (not the platform defaults)", got[0].Policy, want)
	}
}

// A running workspace's expiry budget comes from the template snapshot the
// operator recorded at admit — never from a template revision published
// afterwards. The broker owns expiry decisions and must plan on the same
// contract the operator's backstop uses (V3.25).
func TestK8sRunningSource_PolicyFromRecordedSnapshot(t *testing.T) {
	kc, scheme, restCfg, ns := brokerEnv(t)
	ctx := context.Background()

	// The republished catalog revision carries different lifecycle caps;
	// under the old resolver-based read the planner would have used these.
	tpl := &workspacesv1alpha1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name: "linux-desktop-ffff9999", Namespace: ns,
			Labels: map[string]string{provisioning.LabelCatalogName: "linux-desktop"},
		},
		Spec: workspacesv1alpha1.WorkspaceTemplateSpec{
			Revision: "r2", Runtime: workspacesv1alpha1.RuntimeLinuxContainer,
			Experience: workspacesv1alpha1.ExperienceDesktop,
			Linux:      &workspacesv1alpha1.LinuxRuntimeSpec{Image: "tcdi/linux-desktop@sha256:" + fmt.Sprintf("%064x", 2)},
			Resources: workspacesv1alpha1.ResourceProfile{
				CPU: resource.MustParse("500m"), Memory: resource.MustParse("512Mi"), Storage: resource.MustParse("1Gi")},
			BootDeadline:    metav1.Duration{Duration: 5 * time.Minute},
			NetworkProfile:  workspacesv1alpha1.NetworkProfileIsolated,
			ClipboardPolicy: workspacesv1alpha1.ClipboardDisabled,
			Lifecycle: workspacesv1alpha1.LifecycleDefaults{
				IdleTimeout:       metav1.Duration{Duration: 90 * time.Minute},
				DisconnectTimeout: metav1.Duration{Duration: 45 * time.Minute},
				MaxDuration:       metav1.Duration{Duration: 24 * time.Hour},
				DataPolicy:        workspacesv1alpha1.DataPolicyEphemeral,
			},
		},
	}
	if err := kc.Create(ctx, tpl); err != nil {
		t.Fatal(err)
	}

	// The admitted snapshot (the annotation the operator writes at first
	// admit) records the lifecycle the workspace actually runs under.
	snapJSON, err := json.Marshal(struct {
		Spec workspacesv1alpha1.WorkspaceTemplateSpec `json:"spec"`
	}{Spec: workspacesv1alpha1.WorkspaceTemplateSpec{
		Lifecycle: workspacesv1alpha1.LifecycleDefaults{
			IdleTimeout:       metav1.Duration{Duration: 7 * time.Minute},
			DisconnectTimeout: metav1.Duration{Duration: 2 * time.Minute},
			MaxDuration:       metav1.Duration{Duration: time.Hour},
			DataPolicy:        workspacesv1alpha1.DataPolicyEphemeral,
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ws := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "snap-pol", Namespace: ns,
			Labels:      map[string]string{"workspaces.cdi.tinyorbit.vn/workspace-uid": "ws_snap-pol"},
			Annotations: map[string]string{operator.AnnotationTemplateSnapshot: string(snapJSON)},
		},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			TemplateRef:       workspacesv1alpha1.TemplateReference{Name: "linux-desktop"},
			OwnerSubject:      workspacesv1alpha1.OwnerSubject{Issuer: "https://idp.example", Subject: "alice"},
			DesiredState:      workspacesv1alpha1.DesiredStateRunning,
			DataPolicy:        workspacesv1alpha1.DataPolicyEphemeral,
			RuntimeGeneration: 1, IntentRevision: 1,
		},
	}
	if err := kc.Create(ctx, ws); err != nil {
		t.Fatal(err)
	}
	started := metav1.Now()
	ws.Status = workspacesv1alpha1.WorkspaceStatus{Phase: workspacesv1alpha1.WorkspacePhaseReady, StartedAt: &started}
	if err := kc.Status().Update(ctx, ws); err != nil {
		t.Fatal(err)
	}

	src := brokerSource(t, scheme, restCfg, ns)
	var got []broker.RunningWorkspace
	deadline := time.Now().Add(10 * time.Second)
	for {
		var err error
		if got, err = src.RunningWorkspaces(ctx); err != nil {
			t.Fatal(err)
		}
		if len(got) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(got) != 1 {
		t.Fatalf("running = %+v, want one workspace", got)
	}
	want := broker.TimeoutPolicy{IdleTimeout: 7 * time.Minute, DisconnectTimeout: 2 * time.Minute, MaxDuration: time.Hour}
	if got[0].Policy != want {
		t.Fatalf("policy = %+v, want the recorded snapshot's %+v, not the republished revision", got[0].Policy, want)
	}
}
