// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package linux

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func cond(t corev1.PodConditionType, s corev1.ConditionStatus, reason string) corev1.PodCondition {
	return corev1.PodCondition{Type: t, Status: s, Reason: reason}
}

func waiting(reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}}
}

// TestPodReason_Matrix pins the machine-readable reason the progress UI
// reads (V3.27 G2): ContainerCreating is split into pod preparation (sandbox,
// network, volumes) and image pull through the kubelet's
// PodReadyToStartContainers condition; an init container's pull failure is
// reported instead of the generic PodInitializing; every other token is
// unchanged.
func TestPodReason_Matrix(t *testing.T) {
	now := metav1.NewTime(time.Now())
	tests := []struct {
		name string
		pod  corev1.Pod
		want string
	}{
		{"no status yet", corev1.Pod{}, "Provisioning"},
		{"terminating", corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now}}, "Terminating"},
		{"failed", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed}}, "PodFailed"},
		{"exited", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}, "PodExited"},
		{"unschedulable", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{cond(corev1.PodScheduled, corev1.ConditionFalse, "Unschedulable")}}}, "Unschedulable"},
		{"unschedulable without reason", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{cond(corev1.PodScheduled, corev1.ConditionFalse, "")}}}, "Unschedulable"},
		{"scheduled, kubelet has not reported the sandbox", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				cond(corev1.PodScheduled, corev1.ConditionTrue, ""),
				cond(corev1.PodReadyToStartContainers, corev1.ConditionFalse, ""),
			}}}, "PreparingPod"},
		{"scheduled, no kubelet condition at all", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{cond(corev1.PodScheduled, corev1.ConditionTrue, "")}}}, "Provisioning"},
		{"creating, sandbox not ready", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending,
			Conditions:        []corev1.PodCondition{cond(corev1.PodReadyToStartContainers, corev1.ConditionFalse, "")},
			ContainerStatuses: []corev1.ContainerStatus{waiting("ContainerCreating")}}}, "PreparingPod"},
		{"creating, sandbox ready: pulling", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending,
			Conditions:        []corev1.PodCondition{cond(corev1.PodReadyToStartContainers, corev1.ConditionTrue, "")},
			ContainerStatuses: []corev1.ContainerStatus{waiting("ContainerCreating")}}}, "PullingImage"},
		{"creating on a kubelet without the condition", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{waiting("ContainerCreating")}}}, "ContainerCreating"},
		{"pull backoff", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending,
			Conditions:        []corev1.PodCondition{cond(corev1.PodReadyToStartContainers, corev1.ConditionTrue, "")},
			ContainerStatuses: []corev1.ContainerStatus{waiting("ImagePullBackOff")}}}, "ImagePullBackOff"},
		{"crash loop", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{waiting("CrashLoopBackOff")}}}, "CrashLoopBackOff"},
		{"init container running", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{waiting("PodInitializing")}}}, "PodInitializing"},
		{"init container pull failure beats PodInitializing", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{waiting("ImagePullBackOff")},
			ContainerStatuses:     []corev1.ContainerStatus{waiting("PodInitializing")}}}, "ImagePullBackOff"},
		{"init container pulling", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending,
			Conditions:            []corev1.PodCondition{cond(corev1.PodReadyToStartContainers, corev1.ConditionTrue, "")},
			InitContainerStatuses: []corev1.ContainerStatus{waiting("ContainerCreating")},
			ContainerStatuses:     []corev1.ContainerStatus{waiting("PodInitializing")}}}, "PullingImage"},
		{"running, probe failing", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}, "NotReady"},
		{"ready", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{cond(corev1.PodReady, corev1.ConditionTrue, "")}}}, "Ready"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := podReason(&tc.pod); got != tc.want {
				t.Fatalf("podReason = %q, want %q", got, tc.want)
			}
		})
	}
}
