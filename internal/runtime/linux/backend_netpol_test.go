// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// Unit coverage for the per-workspace boundary NetworkPolicy (SEC-02,
// SEC-29): the InternetOnly egress rule must always subtract the built-in
// private/reserved excepts (a workspace user must never reach RFC1918,
// CGNAT, loopback, link-local/metadata or multicast ranges through the
// "internet" allow), the stored policy must be reconciled when the
// desired spec drifts, and runtime ingress must be scoped to gateway
// pods in the platform namespace — not "any namespace".
package linux

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
)

// netPolFor runs Ensure against a fake client and returns the converged
// boundary NetworkPolicy.
func netPolFor(t *testing.T, opts Options, ws *workspacesv1alpha1.Workspace, tpl *workspacesv1alpha1.WorkspaceTemplate) (*networkingv1.NetworkPolicy, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(backendScheme(t)).Build()
	if _, err := New(c, opts).Ensure(context.Background(), ws, tpl); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	np := &networkingv1.NetworkPolicy{}
	if err := c.Get(context.Background(),
		client.ObjectKey{Name: NetPolName(ws.UID), Namespace: ws.Namespace}, np); err != nil {
		t.Fatalf("networkpolicy not created: %v", err)
	}
	return np, c
}

func internetExcepts(t *testing.T, opts Options) []string {
	t.Helper()
	ws := testWorkspace()
	tpl := testTemplate(nil)
	tpl.Spec.NetworkProfile = workspacesv1alpha1.NetworkProfileInternetOnly
	np, _ := netPolFor(t, opts, ws, tpl)
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == "0.0.0.0/0" {
				return peer.IPBlock.Except
			}
		}
	}
	t.Fatalf("no 0.0.0.0/0 ipBlock egress rule on InternetOnly policy: %+v", np.Spec.Egress)
	return nil
}

// SEC-02: private/reserved ranges are always subtracted from the
// InternetOnly allow, even with zero operator configuration — an empty
// --internet-except-cidrs must never expose the corporate/attached
// networks.
func TestInternetOnlyBuiltinExceptsAlwaysApplied(t *testing.T) {
	except := internetExcepts(t, Options{})
	for _, want := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
		"169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16",
		"224.0.0.0/4", "240.0.0.0/4",
	} {
		found := false
		for _, e := range except {
			if e == want {
				found = true
			}
		}
		if !found {
			t.Errorf("builtin except %s missing from %v", want, except)
		}
	}
}

// Cluster CIDRs the admin declares via --internet-except-cidrs are added
// on top of the built-ins; duplicates collapse.
func TestInternetOnlyExceptsMergeConfigured(t *testing.T) {
	except := internetExcepts(t, Options{
		InternetExceptCIDRs: []string{"10.0.0.0/8", "192.0.2.0/24", "203.0.113.0/24"},
	})
	seen := map[string]int{}
	for _, e := range except {
		seen[e]++
	}
	for _, want := range []string{"10.0.0.0/8", "192.0.2.0/24", "203.0.113.0/24"} {
		if seen[want] == 0 {
			t.Errorf("configured except %s missing from %v", want, except)
		}
	}
	if seen["10.0.0.0/8"] != 1 {
		t.Errorf("10.0.0.0/8 duplicated: %v", except)
	}
}

// The explicit opt-out removes only the built-ins; configured excepts
// still apply. Without it the default must never ship "internet minus
// nothing".
func TestInternetOnlyBuiltinExceptsOptOut(t *testing.T) {
	except := internetExcepts(t, Options{
		InternetExceptCIDRs:         []string{"203.0.113.0/24"},
		DisableBuiltinEgressExcepts: true,
	})
	for _, e := range except {
		for _, builtin := range builtinEgressExcepts {
			if e == builtin {
				t.Errorf("builtin except %s present despite opt-out: %v", builtin, except)
			}
		}
	}
	if len(except) != 1 || except[0] != "203.0.113.0/24" {
		t.Fatalf("except = %v, want only [203.0.113.0/24]", except)
	}
}

// SEC-02: the boundary policy is level-based — when the desired spec
// drifts (operator restarted with new excepts, template profile changed)
// the stored object is updated, not just adopted.
func TestNetPolReconcilesSpecDrift(t *testing.T) {
	ws := testWorkspace()
	tpl := testTemplate(nil)
	tpl.Spec.NetworkProfile = workspacesv1alpha1.NetworkProfileInternetOnly

	_, c := netPolFor(t, Options{}, ws, tpl)

	if _, err := New(c, Options{
		InternetExceptCIDRs: []string{"198.51.100.0/24"},
	}).Ensure(context.Background(), ws, tpl); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}

	np := &networkingv1.NetworkPolicy{}
	if err := c.Get(context.Background(),
		client.ObjectKey{Name: NetPolName(ws.UID), Namespace: ws.Namespace}, np); err != nil {
		t.Fatal(err)
	}
	var except []string
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == "0.0.0.0/0" {
				except = peer.IPBlock.Except
			}
		}
	}
	found := false
	for _, e := range except {
		if e == "198.51.100.0/24" {
			found = true
		}
	}
	if !found {
		t.Fatalf("existing NetworkPolicy was not reconciled to new excepts: %v", except)
	}
}

// A foreign-owned policy is still never adopted — reconcile drift does
// not weaken the ownership check.
func TestNetPolReconcileStillRefusesForeign(t *testing.T) {
	ws := testWorkspace()
	tpl := testTemplate(nil)
	c := fake.NewClientBuilder().WithScheme(backendScheme(t)).WithObjects(
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
			Name: NetPolName(ws.UID), Namespace: ws.Namespace,
			Labels: map[string]string{LabelWorkspaceUID: "another-uid"},
		}},
	).Build()
	if _, err := New(c, Options{}).Ensure(context.Background(), ws, tpl); err == nil {
		t.Fatalf("Ensure must fail on foreign-owned NetworkPolicy")
	}
}

// SEC-29: runtime ingress is scoped to gateway pods in the platform
// namespace — a pod labeled role=gateway in an arbitrary namespace must
// not satisfy the peer.
func TestIngressPeerScopedToGatewayNamespace(t *testing.T) {
	ws := testWorkspace()
	np, _ := netPolFor(t, Options{GatewayNamespace: "tcdi-system"}, ws, testTemplate(nil))
	peer := np.Spec.Ingress[0].From[0]
	if peer.NamespaceSelector == nil ||
		peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "tcdi-system" {
		t.Fatalf("ingress namespaceSelector = %+v, want kubernetes.io/metadata.name=tcdi-system",
			peer.NamespaceSelector)
	}
	if peer.PodSelector == nil ||
		peer.PodSelector.MatchLabels[LabelRole] != RoleGateway {
		t.Fatalf("ingress podSelector = %+v, want role=gateway", peer.PodSelector)
	}
}

// Without a configured gateway namespace the peer narrows to the
// workspace's own namespace (fail closed — never "any namespace").
func TestIngressPeerDefaultsToWorkspaceNamespace(t *testing.T) {
	ws := testWorkspace()
	np, _ := netPolFor(t, Options{}, ws, testTemplate(nil))
	peer := np.Spec.Ingress[0].From[0]
	if peer.NamespaceSelector == nil || len(peer.NamespaceSelector.MatchLabels) == 0 {
		t.Fatalf("ingress peer must never use an empty namespaceSelector (any namespace)")
	}
	if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != ws.Namespace {
		t.Fatalf("default ingress namespace = %+v, want the workspace namespace %q",
			peer.NamespaceSelector.MatchLabels, ws.Namespace)
	}
}

// SEC-11: the runtime pod carries ephemeral-storage request+limit, and
// every disk-backed emptyDir (Ephemeral home, /tmp) has a sizeLimit.
func TestPodEphemeralStorageBounds(t *testing.T) {
	pod := ensurePodSpec(t, nil)
	ctr := pod.Spec.Containers[0]
	for _, list := range []corev1.ResourceList{ctr.Resources.Requests, ctr.Resources.Limits} {
		q, ok := list[corev1.ResourceEphemeralStorage]
		if !ok || q.Sign() <= 0 {
			t.Fatalf("ephemeral-storage missing/non-positive in %v", list)
		}
	}
	var home, tmp *corev1.Volume
	for i := range pod.Spec.Volumes {
		switch pod.Spec.Volumes[i].Name {
		case "home":
			home = &pod.Spec.Volumes[i]
		case "tmp":
			tmp = &pod.Spec.Volumes[i]
		}
	}
	if home == nil || home.EmptyDir == nil || home.EmptyDir.SizeLimit == nil {
		t.Fatalf("ephemeral home must be a size-limited emptyDir: %+v", home)
	}
	// Home is bounded by the template's declared storage size (1Gi in
	// testTemplate).
	if home.EmptyDir.SizeLimit.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Fatalf("home sizeLimit = %v, want 1Gi (template storage)", home.EmptyDir.SizeLimit)
	}
	if tmp == nil || tmp.EmptyDir == nil || tmp.EmptyDir.SizeLimit == nil {
		t.Fatalf("/tmp must be a size-limited emptyDir: %+v", tmp)
	}
}

// Retain keeps the PVC home — the pod's ephemeral budget then only has
// to cover scratch, and there is no emptyDir home at all.
func TestPodEphemeralStorageBoundsRetain(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(backendScheme(t)).Build()
	ws := testWorkspace()
	ws.Spec.DataPolicy = workspacesv1alpha1.DataPolicyRetain
	if _, err := New(c, Options{}).Ensure(context.Background(), ws, testTemplate(nil)); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	pod := &corev1.Pod{}
	if err := c.Get(context.Background(),
		client.ObjectKey{Name: PodName(ws.UID), Namespace: ws.Namespace}, pod); err != nil {
		t.Fatal(err)
	}
	for _, v := range pod.Spec.Volumes {
		if v.Name == "home" && v.EmptyDir != nil {
			t.Fatalf("retain home must be the PVC, not emptyDir")
		}
	}
	ctr := pod.Spec.Containers[0]
	if q := ctr.Resources.Limits[corev1.ResourceEphemeralStorage]; q.Sign() <= 0 {
		t.Fatalf("ephemeral-storage limit missing on retain pod: %v", ctr.Resources.Limits)
	}
}

// SEC-30: no apiserver token is ever mounted into the runtime.
func TestPodAutomountServiceAccountTokenFalse(t *testing.T) {
	pod := ensurePodSpec(t, nil)
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatalf("automountServiceAccountToken must be explicitly false")
	}
	if pod.Spec.EnableServiceLinks == nil || *pod.Spec.EnableServiceLinks {
		t.Fatalf("enableServiceLinks must be explicitly false")
	}
}
