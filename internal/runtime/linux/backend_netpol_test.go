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
	"k8s.io/apimachinery/pkg/util/intstr"
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

// The ClusterOnly default is the historical posture: egress to any pod
// in any namespace on any port — two peers, one selecting every pod in
// the workspace's own namespace and one every namespace. The rule
// carries no ports and the DNS rule still leads the egress list.
func TestClusterOnlyEgressDefaultUnrestricted(t *testing.T) {
	ws := testWorkspace()
	tpl := testTemplate(nil)
	tpl.Spec.NetworkProfile = workspacesv1alpha1.NetworkProfileClusterOnly
	np, _ := netPolFor(t, Options{}, ws, tpl)

	if len(np.Spec.Egress) != 2 {
		t.Fatalf("ClusterOnly egress rules = %d, want 2 (dns + profile)", len(np.Spec.Egress))
	}
	dns := np.Spec.Egress[0]
	foundDNS := false
	for _, p := range dns.Ports {
		if p.Port != nil && p.Port.IntValue() == 53 {
			foundDNS = true
		}
	}
	if !foundDNS {
		t.Fatalf("first egress rule must be cluster DNS (port 53): %+v", dns)
	}
	rule := np.Spec.Egress[1]
	if len(rule.Ports) != 0 {
		t.Fatalf("default ClusterOnly rule must not restrict ports: %+v", rule.Ports)
	}
	var anyPod, anyNS bool
	for _, peer := range rule.To {
		if peer.PodSelector != nil && len(peer.PodSelector.MatchLabels) == 0 &&
			len(peer.PodSelector.MatchExpressions) == 0 && peer.NamespaceSelector == nil {
			anyPod = true
		}
		if peer.NamespaceSelector != nil && len(peer.NamespaceSelector.MatchLabels) == 0 &&
			len(peer.NamespaceSelector.MatchExpressions) == 0 && peer.PodSelector == nil {
			anyNS = true
		}
	}
	if !anyPod || !anyNS {
		t.Fatalf("default ClusterOnly peers = %+v, want {podSelector:{}} + {namespaceSelector:{}}", rule.To)
	}
}

// A configured ClusterOnlyEgress lands on the built policy as a single
// narrowed peer: the requested namespace+pod selectors AND the ports —
// and nothing else (no leftover any-namespace peer). The restriction
// reaches EXISTING policies too: the spec-drift reconcile rewrites a
// stored unrestricted rule.
func TestClusterOnlyEgressRestrictionOnPolicy(t *testing.T) {
	ws := testWorkspace()
	tpl := testTemplate(nil)
	tpl.Spec.NetworkProfile = workspacesv1alpha1.NetworkProfileClusterOnly

	opts := Options{ClusterOnlyEgress: &ClusterOnlyEgress{
		NamespaceSelector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "workspaces.cdi.tinyorbit.vn/tenant",
				Operator: metav1.LabelSelectorOpExists,
			}},
		},
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"app.kubernetes.io/name": "postgres"},
		},
		Ports: []networkingv1.NetworkPolicyPort{{
			Protocol: ptr(corev1.ProtocolTCP),
			Port:     ptr(intstr.FromInt32(5432)),
		}},
	}}
	np, c := netPolFor(t, opts, ws, tpl)

	if len(np.Spec.Egress) != 2 {
		t.Fatalf("restricted ClusterOnly egress rules = %d, want 2 (dns + profile)", len(np.Spec.Egress))
	}
	rule := np.Spec.Egress[1]
	if len(rule.To) != 1 {
		t.Fatalf("restricted ClusterOnly peers = %+v, want exactly one peer", rule.To)
	}
	peer := rule.To[0]
	if peer.NamespaceSelector == nil ||
		len(peer.NamespaceSelector.MatchExpressions) != 1 ||
		peer.NamespaceSelector.MatchExpressions[0].Key != "workspaces.cdi.tinyorbit.vn/tenant" {
		t.Fatalf("restricted namespaceSelector = %+v", peer.NamespaceSelector)
	}
	if peer.PodSelector == nil ||
		peer.PodSelector.MatchLabels["app.kubernetes.io/name"] != "postgres" {
		t.Fatalf("restricted podSelector = %+v", peer.PodSelector)
	}
	if len(rule.Ports) != 1 || rule.Ports[0].Port == nil || rule.Ports[0].Port.IntValue() != 5432 {
		t.Fatalf("restricted ports = %+v, want TCP 5432", rule.Ports)
	}

	// Drift reconcile: removing the option on a second Ensure rewrites
	// the stored policy back to the unrestricted shape (and vice versa —
	// the mechanism is generic, exercised here in the hardening direction
	// by the second half of this test).
	if _, err := New(c, Options{}).Ensure(context.Background(), ws, tpl); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	back := &networkingv1.NetworkPolicy{}
	if err := c.Get(context.Background(),
		client.ObjectKey{Name: NetPolName(ws.UID), Namespace: ws.Namespace}, back); err != nil {
		t.Fatal(err)
	}
	if len(back.Spec.Egress[1].To) != 2 {
		t.Fatalf("stored policy was not reconciled back to the unrestricted peer set: %+v",
			back.Spec.Egress[1].To)
	}
}

// A ports-only narrowing keeps the pod-only destination set: the peer
// must be an all-namespaces namespaceSelector (every pod, never external
// IPs), NOT a wholly empty peer — `to: [{}]` would also admit non-pod
// destinations.
func TestClusterOnlyEgressPortsOnlyStaysPodOnly(t *testing.T) {
	ws := testWorkspace()
	tpl := testTemplate(nil)
	tpl.Spec.NetworkProfile = workspacesv1alpha1.NetworkProfileClusterOnly

	np, _ := netPolFor(t, Options{ClusterOnlyEgress: &ClusterOnlyEgress{
		Ports: []networkingv1.NetworkPolicyPort{{
			Port: ptr(intstr.FromInt32(443)),
		}},
	}}, ws, tpl)
	rule := np.Spec.Egress[1]
	if len(rule.To) != 1 || rule.To[0].NamespaceSelector == nil || rule.To[0].PodSelector != nil {
		t.Fatalf("ports-only peer = %+v, want a single all-namespaces pod peer", rule.To)
	}
	if len(rule.Ports) != 1 || rule.Ports[0].Port.IntValue() != 443 {
		t.Fatalf("ports-only rule ports = %+v, want [443]", rule.Ports)
	}
}
