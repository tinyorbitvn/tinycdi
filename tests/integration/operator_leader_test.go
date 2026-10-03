// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

//go:build integration

// E4: the operator ships highly available — two replicas behind Lease
// leader election. These tests run TWO production reconcilers against the
// envtest apiserver competing for one lease: the standby must take over
// when the leader stops, and a running pair must still converge a
// workspace exactly once.
//
// Run: go test -tags=integration ./tests/integration -run 'TestOperator_' -count=1

package integration

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/operator"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

// leaderLeaseID is the production leader-election ID from cmd/operator.
const leaderLeaseID = "b6b73984.cdi.tinyorbit.vn"

// startLeaderOperator runs the production reconciler in a leader-electing
// manager: it competes for the leaseNS/leaderLeaseID Lease and reconciles
// only ns (matching --watch-namespaces / --leader-election-namespace).
// Short lease timings keep the failover inside the 30 s test budget while
// exercising the same resourcelock path the Deployment uses
// (LeaderElectionReleaseOnCancel stays off, as in production — a crashed
// leader's lease expires, it is not handed over).
// Returns the manager and its stop func.
func startLeaderOperator(t *testing.T, ns, leaseNS string) (ctrl.Manager, func()) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := workspacesv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	mgr, err := ctrl.NewManager(testEnvConfig, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		Cache:                   cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
		Controller:              config.Controller{SkipNameValidation: ptr.To(true)},
		LeaderElection:          true,
		LeaderElectionID:        leaderLeaseID,
		LeaderElectionNamespace: leaseNS,
		LeaseDuration:           ptr.To(5 * time.Second),
		RenewDeadline:           ptr.To(4 * time.Second),
		RetryPeriod:             ptr.To(1 * time.Second),
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
	stop := func() {
		cancel()
		<-done
	}
	t.Cleanup(stop)
	return mgr, stop
}

// leaderWorkspace creates a minimal Running-intent Workspace CR — the same
// shape the API's K8sApplier writes — in ns against template tplName.
func leaderWorkspace(t *testing.T, ns, name, tplName string) *workspacesv1alpha1.Workspace {
	t.Helper()
	ws := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			TemplateRef:       workspacesv1alpha1.TemplateReference{Name: tplName},
			OwnerSubject:      workspacesv1alpha1.OwnerSubject{Issuer: "it", Subject: "sub-" + name},
			DesiredState:      workspacesv1alpha1.DesiredStateRunning,
			DataPolicy:        workspacesv1alpha1.DataPolicyEphemeral,
			RuntimeGeneration: 1,
			IntentRevision:    1,
		},
	}
	if err := k8sClient.Create(context.Background(), ws); err != nil {
		t.Fatalf("workspace %s: %v", name, err)
	}
	return ws
}

// workspaceAdmitted polls until the Workspace's Admitted condition is True.
func workspaceAdmitted(t *testing.T, ns, name string, timeout time.Duration) {
	t.Helper()
	eventually(t, "Admitted=True on "+ns+"/"+name, timeout, func() bool {
		var ws workspacesv1alpha1.Workspace
		if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &ws); err != nil {
			return false
		}
		c := meta.FindStatusCondition(ws.Status.Conditions, workspacesv1alpha1.ConditionAdmitted)
		return c != nil && c.Status == metav1.ConditionTrue
	})
}

// TestOperator_LeaderFailover: two managers compete for the lease; the
// leader is stopped and a Workspace created afterwards must still reach
// Admitted=True within 30 s — the standby took the lease and reconciled.
func TestOperator_LeaderFailover(t *testing.T) {
	ns := newRetainedNamespace(t)
	leaseNS := newRetainedNamespace(t)
	fxr20Template(t, ns, "desktop")

	mgrA, stopA := startLeaderOperator(t, ns, leaseNS)
	mgrB, stopB := startLeaderOperator(t, ns, leaseNS)

	// Wait for a leader, then stop it: its pod is gone.
	var leaderStop func()
	select {
	case <-mgrA.Elected():
		leaderStop = stopA
	case <-mgrB.Elected():
		leaderStop = stopB
	case <-time.After(30 * time.Second):
		t.Fatal("no manager acquired leadership within 30s")
	}
	leaderStop()

	// The standby must acquire the expired lease and reconcile a new
	// workspace inside the 30 s availability budget (E4).
	leaderWorkspace(t, ns, "ws-failover", "desktop")
	workspaceAdmitted(t, ns, "ws-failover", 30*time.Second)
}

// TestOperator_NoDoubleReconcile: with two managers only the leader
// reconciles — one workspace produces exactly one runtime pod.
func TestOperator_NoDoubleReconcile(t *testing.T) {
	ns := newRetainedNamespace(t)
	leaseNS := newRetainedNamespace(t)
	fxr20Template(t, ns, "desktop")

	mgrA, _ := startLeaderOperator(t, ns, leaseNS)
	mgrB, _ := startLeaderOperator(t, ns, leaseNS)

	select {
	case <-mgrA.Elected():
	case <-mgrB.Elected():
	case <-time.After(30 * time.Second):
		t.Fatal("no manager acquired leadership within 30s")
	}

	ws := leaderWorkspace(t, ns, "ws-once", "desktop")
	workspaceAdmitted(t, ns, "ws-once", 30*time.Second)

	// Exactly one pod, owned by this workspace — a non-leader that also
	// reconciled would have raced a second create.
	pods := &corev1.PodList{}
	if err := k8sClient.List(context.Background(), pods,
		client.InNamespace(ns),
		client.MatchingLabels{"workspaces.cdi.tinyorbit.vn/workspace-uid": string(ws.UID)}); err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("pods for workspace = %d, want exactly 1", len(pods.Items))
	}
	if pods.Items[0].Name != linux.PodName(ws.UID) {
		t.Fatalf("pod name = %q, want %q", pods.Items[0].Name, linux.PodName(ws.UID))
	}
}
