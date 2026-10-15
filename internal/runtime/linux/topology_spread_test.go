// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// Unit coverage for Options.TopologySpread (TOPO-1): enabled, every
// runtime pod carries one SOFT topologySpreadConstraint (maxSkew 1,
// kubernetes.io/hostname, ScheduleAnyway) whose selector matches the
// pod's own labels — i.e. the tenant namespace's runtime pods, since a
// constraint only counts same-namespace pods. Disabled (zero value) the
// pod spec is unchanged; placement fields are orthogonal either way.
package linux

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
)

func TestTopologySpread_Enabled(t *testing.T) {
	pod := ensurePodSpecFor(t, testTemplate(nil), Options{TopologySpread: true})
	tsc := pod.Spec.TopologySpreadConstraints
	if len(tsc) != 1 {
		t.Fatalf("topologySpreadConstraints = %v, want exactly one", tsc)
	}
	c := tsc[0]
	if c.MaxSkew != 1 {
		t.Errorf("maxSkew = %d, want 1", c.MaxSkew)
	}
	if c.TopologyKey != corev1.LabelHostname {
		t.Errorf("topologyKey = %q, want %q", c.TopologyKey, corev1.LabelHostname)
	}
	if c.WhenUnsatisfiable != corev1.ScheduleAnyway {
		t.Errorf("whenUnsatisfiable = %q, want ScheduleAnyway — the constraint must never block "+
			"scheduling (retained-PVC reattach, single-node pool)", c.WhenUnsatisfiable)
	}
	if c.LabelSelector == nil {
		t.Fatal("labelSelector is nil — the constraint would count nothing")
	}
	sel, err := metav1.LabelSelectorAsSelector(c.LabelSelector)
	if err != nil {
		t.Fatalf("labelSelector does not parse: %v", err)
	}
	if !sel.Matches(k8slabels.Set(pod.Labels)) {
		t.Fatalf("labelSelector %v does not match the pod's own labels %v — "+
			"it must count this namespace's runtime pods", c.LabelSelector, pod.Labels)
	}
	// The selector must not widen to non-runtime children (data pods,
	// gateway pods) — role=runtime only.
	if sel.Matches(k8slabels.Set{LabelRole: RoleGateway, LabelPartOf: PartOf}) {
		t.Fatalf("labelSelector %v must not match gateway pods", c.LabelSelector)
	}
}

func TestTopologySpread_DisabledLeavesPodSpecUntouched(t *testing.T) {
	for _, opts := range []Options{{}, {TopologySpread: false}} {
		pod := ensurePodSpecFor(t, testTemplate(nil), opts)
		if pod.Spec.TopologySpreadConstraints != nil {
			t.Fatalf("disabled: topologySpreadConstraints = %v, want unset", pod.Spec.TopologySpreadConstraints)
		}
	}
}

// Spread composes with placement rather than replacing it: the constraint
// only ranks the nodes the selector/tolerations already admit.
func TestTopologySpread_KeepsPlacement(t *testing.T) {
	opts := defaultPlacementOptions()
	opts.TopologySpread = true
	pod := ensurePodSpecFor(t, testTemplate(nil), opts)
	if !reflect.DeepEqual(pod.Spec.NodeSelector, map[string]string{"pool": "default"}) {
		t.Fatalf("nodeSelector = %v, want operator default kept", pod.Spec.NodeSelector)
	}
	if len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Key != "default-taint" {
		t.Fatalf("tolerations = %v, want operator default kept", pod.Spec.Tolerations)
	}
	if len(pod.Spec.TopologySpreadConstraints) != 1 {
		t.Fatalf("topologySpreadConstraints = %v, want the one spread constraint", pod.Spec.TopologySpreadConstraints)
	}
}
