package linux

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
)

func retainWorkspace(observedGen int64) *cdiv1.Workspace {
	ws := testWorkspace()
	ws.Spec.DataPolicy = cdiv1.DataPolicyRetain
	ws.Status.ObservedRuntimeGeneration = observedGen
	return ws
}

func TestEnsure_RetainHomeMissingAfterFirstBootIsLost(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(backendScheme(t)).Build()
	_, err := New(c, Options{}).Ensure(context.Background(), retainWorkspace(1), testTemplate(nil))
	if !errors.Is(err, ErrHomeVolumeLost) {
		t.Fatalf("Ensure = %v, want ErrHomeVolumeLost", err)
	}
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(context.Background(), pvcs); err != nil || len(pvcs.Items) != 0 {
		t.Fatalf("a fresh empty home was created: %d claims, err %v", len(pvcs.Items), err)
	}
}

func TestEnsure_RetainFirstBootCreatesTheHome(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(backendScheme(t)).Build()
	if _, err := New(c, Options{}).Ensure(context.Background(), retainWorkspace(0), testTemplate(nil)); err != nil {
		t.Fatalf("first boot: %v", err)
	}
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(context.Background(), pvcs); err != nil || len(pvcs.Items) != 1 {
		t.Fatalf("first boot must create the home: %d claims, err %v", len(pvcs.Items), err)
	}
}

func TestWaitingMessage(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}}, // no message: skipped
		ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull", Message: "denied by policy"}}}},
	}}
	if got := waitingMessage(pod); got != "denied by policy" {
		t.Fatalf("waitingMessage = %q", got)
	}
	if got := waitingMessage(&corev1.Pod{}); got != "" {
		t.Fatalf("waitingMessage of an empty pod = %q", got)
	}
}
