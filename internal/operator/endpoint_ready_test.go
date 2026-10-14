// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package operator

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
)

// F2: a Ready pod is not a Ready workspace until the Service routes to it.
func TestReconcile_WaitingForEndpoint(t *testing.T) {
	env, c := startEnv(t)
	defer func() { _ = env.Stop() }()
	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-ep", nil)
	ws := newWorkspace(t, c, ns, "ws-ep", "tpl-ep", nil)
	key := types.NamespacedName{Name: ws.Name, Namespace: ns}
	r := newReconciler(c)

	reconcile(t, r, key)
	pod := thePod(t, c, ns, ws.UID)
	markPodReadyOnly(t, c, pod) // kubelet says Ready; no EndpointSlice yet
	res := reconcile(t, r, key)

	got := getWorkspace(t, c, key)
	if got.Status.Phase == cdiv1.WorkspacePhaseReady {
		t.Fatalf("phase Ready without a ready endpoint (conds=%v)", got.Status.Conditions)
	}
	if rt := condition(got, cdiv1.ConditionRuntimeReady); rt == nil || rt.Status != metav1.ConditionTrue {
		t.Fatalf("RuntimeReady = %+v, want True", rt)
	}
	cc := condition(got, cdiv1.ConditionConnectionReady)
	if cc == nil || cc.Status != metav1.ConditionFalse || cc.Reason != "WaitingForEndpoint" {
		t.Fatalf("ConnectionReady = %+v, want False/WaitingForEndpoint", cc)
	}
	if res.RequeueAfter != requeueEndpointWait {
		t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, requeueEndpointWait)
	}

	publishReadyEndpoint(t, c, pod)
	reconcile(t, r, key)
	if got = getWorkspace(t, c, key); got.Status.Phase != cdiv1.WorkspacePhaseReady {
		t.Fatalf("phase %q after the slice appeared, want Ready (conds=%v)", got.Status.Phase, got.Status.Conditions)
	}
}
