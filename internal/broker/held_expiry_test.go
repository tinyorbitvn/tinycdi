// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package broker_test

// A workspace on a template hold (the operator's Degraded/TemplateRevisionGone
// or TemplateInvalid hold — status.templateSnapshot never recorded) keeps its
// Ready phase while its operator-owned pod serves, so the broker's running
// projection keeps it under lifecycle enforcement — with the held policy:
// the STRICTER of the resolved template lifecycle and the chart defaults.
// These tests pin enforcement at that stricter cap: a default-policy
// workspace at the same age keeps running where the held one already stops.
// Before the operator kept held-with-pod workspaces Ready, they vanished
// from the running set entirely and no cap applied at all.

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/operator"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

// heldPolicy is the TimeoutPolicy shape a held workspace resolves to when
// its live template is stricter than the chart defaults on every cap.
var heldPolicy = broker.TimeoutPolicy{
	IdleTimeout:       90 * time.Second,
	DisconnectTimeout: time.Minute,
	MaxDuration:       2 * time.Hour,
}

// report feeds one activity event to both workspaces' leases.
func report(t *testing.T, b *broker.Broker, heldLease string, heldFence broker.Fence,
	ctlLease string, ctlFence broker.Fence, ev broker.ActivityEvent) {
	t.Helper()
	for _, r := range []struct {
		lease string
		fence broker.Fence
	}{{heldLease, heldFence}, {ctlLease, ctlFence}} {
		if err := b.ReportActivity(ctx, gwA, r.lease, r.fence, ev); err != nil {
			t.Fatalf("ReportActivity(%s): %v", ev.Type, err)
		}
	}
}

// A held workspace's tighter caps — not the chart defaults — drive its
// idle, disconnect and max-duration stops. The default-policy control at
// the same age produces no intent.
func TestHeldWorkspace_EnforcedAtStricterCaps(t *testing.T) {
	t.Run("idle timeout", func(t *testing.T) {
		db, b, clock, src := setup(t)
		p := broker.NewExpiryPlanner(b)
		heldLease, heldFence := seedActivityWorkspace(t, db, src, clock, "ws-held", 1, "rt-h", 1)
		ctlLease, ctlFence := seedActivityWorkspace(t, db, src, clock, "ws-ctrl", 1, "rt-c", 1)
		report(t, b, heldLease, heldFence, ctlLease, ctlFence, input())

		started := clock.Now()
		run := []broker.RunningWorkspace{
			running("ws-held", 1, started, heldPolicy),
			running("ws-ctrl", 1, started, broker.DefaultTimeoutPolicy),
		}

		clock.Advance(heldPolicy.IdleTimeout - time.Second)
		got, err := p.Scan(ctx, run)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("stop emitted %v before the held idle cap", got)
		}

		clock.Advance(time.Second)
		got, err = p.Scan(ctx, run)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		wantIntents(t, got, []broker.StopIntent{{
			WorkspaceUID: "ws-held", RuntimeGeneration: 1,
			Reason: broker.StopReasonIdleTimeout,
		}})
	})

	t.Run("disconnect timeout", func(t *testing.T) {
		db, b, clock, src := setup(t)
		p := broker.NewExpiryPlanner(b)
		heldLease, heldFence := seedActivityWorkspace(t, db, src, clock, "ws-held", 1, "rt-h", 1)
		ctlLease, ctlFence := seedActivityWorkspace(t, db, src, clock, "ws-ctrl", 1, "rt-c", 1)
		report(t, b, heldLease, heldFence, ctlLease, ctlFence, input())
		report(t, b, heldLease, heldFence, ctlLease, ctlFence,
			broker.ActivityEvent{Type: broker.ActivityDisconnect})

		run := []broker.RunningWorkspace{
			running("ws-held", 1, clock.Now(), heldPolicy),
			running("ws-ctrl", 1, clock.Now(), broker.DefaultTimeoutPolicy),
		}

		clock.Advance(heldPolicy.DisconnectTimeout - time.Second)
		got, err := p.Scan(ctx, run)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("disconnect stop fired %v inside the held grace window", got)
		}

		clock.Advance(time.Second)
		got, err = p.Scan(ctx, run)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		wantIntents(t, got, []broker.StopIntent{{
			WorkspaceUID: "ws-held", RuntimeGeneration: 1,
			Reason: broker.StopReasonDisconnectTimeout,
		}})
	})

	t.Run("max duration wins over fresh input", func(t *testing.T) {
		db, b, clock, src := setup(t)
		p := broker.NewExpiryPlanner(b)
		heldLease, heldFence := seedActivityWorkspace(t, db, src, clock, "ws-held", 1, "rt-h", 1)
		ctlLease, ctlFence := seedActivityWorkspace(t, db, src, clock, "ws-ctrl", 1, "rt-c", 1)
		report(t, b, heldLease, heldFence, ctlLease, ctlFence, input())

		started := clock.Now()
		run := []broker.RunningWorkspace{
			running("ws-held", 1, started, heldPolicy),
			running("ws-ctrl", 1, started, broker.DefaultTimeoutPolicy),
		}

		// Fresh input just before the held cap: idle can never fire for
		// either workspace, so only the absolute cap decides.
		clock.Advance(heldPolicy.MaxDuration - 30*time.Second)
		report(t, b, heldLease, heldFence, ctlLease, ctlFence, input())
		got, err := p.Scan(ctx, run)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("stop emitted %v before the held max-duration cap", got)
		}

		clock.Advance(30 * time.Second)
		got, err = p.Scan(ctx, run)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		wantIntents(t, got, []broker.StopIntent{{
			WorkspaceUID: "ws-held", RuntimeGeneration: 1,
			Reason: broker.StopReasonMaxDuration,
		}})
	})
}

// The full projection path on a real apiserver: an operator reconcile that
// lands the workspace on a template hold while its pod keeps running must
// still show up in K8sRunningSource.RunningWorkspaces — Ready, generation
// pinned, held policy applied. On the pre-fix operator the hold wrote
// Pending and the running set dropped the workspace: no idle, disconnect
// or max-duration enforcement at all.
func TestK8sRunningSource_OperatorHeldWorkspaceStaysEnforced(t *testing.T) {
	kc, scheme, restCfg, ns := brokerEnv(t)
	ctx := context.Background()

	// The template the pod was built under — strict lifecycle caps, and
	// (after the pod is seeded) an out-of-policy seccomp annotation that
	// lands the workspace on the content-invalid hold.
	tpl := &cdiv1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name: "fam-held", Namespace: ns,
			Labels:      map[string]string{provisioning.LabelCatalogName: "fam-held"},
			Annotations: map[string]string{linux.AnnotationSeccompProfile: "localhost/browser-sandbox"},
		},
		Spec: cdiv1.WorkspaceTemplateSpec{
			Revision: "2026-10-a", Runtime: cdiv1.RuntimeLinuxContainer,
			Experience: cdiv1.ExperienceDesktop,
			Linux: &cdiv1.LinuxRuntimeSpec{
				Image: "tcdi/linux-desktop@sha256:" + fmt.Sprintf("%064x", 9),
			},
			Resources: cdiv1.ResourceProfile{
				CPU: resource.MustParse("500m"), Memory: resource.MustParse("512Mi"),
				Storage: resource.MustParse("1Gi"),
			},
			BootDeadline:    metav1.Duration{Duration: 5 * time.Minute},
			NetworkProfile:  cdiv1.NetworkProfileIsolated,
			ClipboardPolicy: cdiv1.ClipboardDisabled,
			Lifecycle: cdiv1.LifecycleDefaults{
				IdleTimeout:       metav1.Duration{Duration: 90 * time.Second},
				DisconnectTimeout: metav1.Duration{Duration: time.Minute},
				MaxDuration:       metav1.Duration{Duration: 2 * time.Hour},
				DataPolicy:        cdiv1.DataPolicyEphemeral,
			},
		},
	}
	if err := kc.Create(ctx, tpl); err != nil {
		t.Fatal(err)
	}
	ws := &cdiv1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ws-held-op", Namespace: ns,
			Labels: map[string]string{provisioning.LabelWorkspaceUID: "ws_heldop"},
		},
		Spec: cdiv1.WorkspaceSpec{
			TemplateRef:       cdiv1.TemplateReference{Name: "fam-held"},
			OwnerSubject:      cdiv1.OwnerSubject{Issuer: "https://idp.example", Subject: "alice"},
			DesiredState:      cdiv1.DesiredStateRunning,
			DataPolicy:        cdiv1.DataPolicyEphemeral,
			RuntimeGeneration: 1, IntentRevision: 1,
			SessionPolicy: cdiv1.SessionPolicySpec{
				Clipboard:    cdiv1.ClipboardDisabled,
				FileTransfer: cdiv1.FileTransferDisabled,
			},
		},
	}
	if err := kc.Create(ctx, ws); err != nil {
		t.Fatal(err)
	}

	// Seed the running incarnation through the real backend — stamped,
	// owned pod + service, kubelet-reported Ready — then invalidate the
	// live template's content.
	backend := linux.New(kc, linux.Options{})
	if _, err := backend.Ensure(ctx, ws, tpl); err != nil {
		t.Fatalf("seed incarnation: %v", err)
	}
	pod := &corev1.Pod{}
	if err := kc.Get(ctx, client.ObjectKey{Namespace: ns, Name: linux.PodName(ws.UID)}, pod); err != nil {
		t.Fatalf("seeded pod: %v", err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue, Reason: "Ready"},
	}
	if err := kc.Status().Update(ctx, pod); err != nil {
		t.Fatalf("mark pod serving: %v", err)
	}
	live := &cdiv1.WorkspaceTemplate{}
	if err := kc.Get(ctx, client.ObjectKey{Namespace: ns, Name: "fam-held"}, live); err != nil {
		t.Fatal(err)
	}
	live.Annotations[linux.AnnotationSeccompProfile] = "unconfined"
	if err := kc.Update(ctx, live); err != nil {
		t.Fatalf("invalidate template content: %v", err)
	}

	r := &operator.WorkspaceReconciler{Client: kc, Scheme: scheme, Backend: backend}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ws)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &cdiv1.Workspace{}
	if err := kc.Get(ctx, client.ObjectKeyFromObject(ws), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != cdiv1.WorkspacePhaseReady {
		t.Fatalf("held workspace with a live pod: phase=%q, want Ready", got.Status.Phase)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, cdiv1.ConditionDegraded)
	if cond == nil || cond.Status != metav1.ConditionTrue ||
		cond.Reason != operator.ReasonTemplateInvalid {
		t.Fatalf("want Degraded/TemplateInvalid, got %+v", got.Status.Conditions)
	}

	src := brokerSource(t, scheme, restCfg, ns)
	var running []broker.RunningWorkspace
	deadline := time.Now().Add(15 * time.Second)
	for {
		var err error
		if running, err = src.RunningWorkspaces(ctx); err != nil {
			t.Fatal(err)
		}
		if len(running) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(running) != 1 {
		t.Fatalf("running = %+v, want the held workspace", running)
	}
	rw := running[0]
	if rw.WorkspaceUID != "ws_heldop" || rw.RuntimeGeneration != 1 || rw.StartedAt.IsZero() {
		t.Fatalf("held projection = %+v", rw)
	}
	if rw.Policy != heldPolicy {
		t.Fatalf("held policy = %+v, want the stricter caps %+v", rw.Policy, heldPolicy)
	}
}
