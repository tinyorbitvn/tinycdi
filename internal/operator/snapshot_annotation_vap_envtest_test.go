// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package operator

// envtest coverage for the optional ValidatingAdmissionPolicy sample in
// config/admissionpolicy/policy.yaml (S36): applied verbatim against a
// real apiserver it must DENY create/update of Workspace objects that add,
// change or remove the operator-owned
// workspaces.cdi.tinyorbit.vn/template-snapshot mirror annotation, unless
// the caller is the operator ServiceAccount — and stay inert outside the
// managed namespaces. The chart renders the same expressions with the
// release-derived operator identity (deploy/helm/chart_admissionpolicy_test.go).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	apimachyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	vapOperatorUser = "system:serviceaccount:platform-system:platform-controller-manager"
	vapWriterUser   = "system:serviceaccount:platform-system:backend"
	vapManagedNS    = "tinycdi-tenant-a"
	vapUnmanagedNS  = "not-managed"
	vapAnnotKey     = "workspaces.cdi.tinyorbit.vn/template-snapshot"
	vapAnnotValue   = `{"name":"tpl","uid":"u","revision":"r1","specHash":"h","spec":{}}`
)

var (
	vapGVR   = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicies"}
	vapbGVR  = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicybindings"}
	wsVAPGVR = schema.GroupVersionResource{Group: "workspaces.cdi.tinyorbit.vn", Version: "v1alpha1", Resource: "workspaces"}
	nsGVR    = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
)

func vapEnvtestAssets(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("KUBEBUILDER_ASSETS"); dir != "" {
		return dir
	}
	matches, _ := filepath.Glob(filepath.Join("..", "..", "bin", "k8s", "*-linux-amd64"))
	if len(matches) == 0 {
		t.Skip("KUBEBUILDER_ASSETS unset and no bin/k8s/*-linux-amd64 found")
	}
	return matches[len(matches)-1]
}

// vapWorkspace is a schema-valid Workspace; withAnnot stamps the guarded
// template-snapshot annotation.
func vapWorkspace(name, ns string, withAnnot bool) *unstructured.Unstructured {
	md := map[string]any{"name": name, "namespace": ns}
	if withAnnot {
		md["annotations"] = map[string]any{vapAnnotKey: vapAnnotValue}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "workspaces.cdi.tinyorbit.vn/v1alpha1",
		"kind":       "Workspace",
		"metadata":   md,
		"spec": map[string]any{
			"templateRef":       map[string]any{"name": "tpl"},
			"ownerSubject":      map[string]any{"issuer": "https://idp.example.com", "subject": "user-1"},
			"desiredState":      "Stopped",
			"dataPolicy":        "Ephemeral",
			"runtimeGeneration": int64(0),
			"intentRevision":    int64(1),
		},
	}}
}

// impersonated returns a dynamic client acting as the given identity —
// the policy keys on request.userInfo.username. The system:masters group
// carries the request past RBAC to the admission chain (envtest grants no
// role bindings); it does not influence the CEL username check.
func impersonated(t *testing.T, cfg *rest.Config, username string) dynamic.Interface {
	t.Helper()
	c := rest.CopyConfig(cfg)
	c.Impersonate = rest.ImpersonationConfig{
		UserName: username,
		Groups:   []string{"system:masters", "system:authenticated"},
	}
	d, err := dynamic.NewForConfig(c)
	if err != nil {
		t.Fatalf("impersonated client %s: %v", username, err)
	}
	return d
}

func wsClient(d dynamic.Interface, ns string) dynamic.ResourceInterface {
	return d.Resource(wsVAPGVR).Namespace(ns)
}

func createWS(d dynamic.Interface, ws *unstructured.Unstructured) error {
	_, err := wsClient(d, ws.GetNamespace()).Create(context.Background(), ws, metav1.CreateOptions{})
	return err
}

// vapDenied reports whether err is the policy's denial — identified by the
// rendered message, which names the annotation — and not an unrelated
// rejection (schema, missing namespace).
func vapDenied(err error) bool {
	if err == nil {
		return false
	}
	return (apierrors.IsInvalid(err) || apierrors.IsForbidden(err)) &&
		strings.Contains(err.Error(), "template-snapshot")
}

// sampleDocs decodes every manifest document of
// config/admissionpolicy/policy.yaml — the same files a kubectl apply -k
// ships. Comment-only chunks (the header) decode to an empty object.
func sampleDocs(t *testing.T) []*unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "admissionpolicy", "policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var out []*unstructured.Unstructured
	for i, doc := range strings.Split(string(raw), "\n---\n") {
		obj := &unstructured.Unstructured{}
		if err := apimachyaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
			t.Fatalf("policy.yaml doc %d: %v", i, err)
		}
		if obj.GetKind() == "" {
			continue
		}
		out = append(out, obj)
	}
	return out
}

func applySample(t *testing.T, admin dynamic.Interface) {
	t.Helper()
	for _, obj := range sampleDocs(t) {
		var gvr schema.GroupVersionResource
		switch obj.GetKind() {
		case "ValidatingAdmissionPolicy":
			gvr = vapGVR
		case "ValidatingAdmissionPolicyBinding":
			gvr = vapbGVR
		default:
			t.Fatalf("policy.yaml: unexpected kind %q", obj.GetKind())
		}
		if _, err := admin.Resource(gvr).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
			t.Fatalf("apply %s %s: %v", obj.GetKind(), obj.GetName(), err)
		}
	}
}

func TestSnapshotAnnotationVAP_Enforced(t *testing.T) {
	env := &envtest.Environment{
		BinaryAssetsDirectory: vapEnvtestAssets(t),
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	restCfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx := context.Background()

	admin, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{vapManagedNS, vapUnmanagedNS} {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Namespace",
			"metadata": map[string]any{"name": ns},
		}}
		if _, err := admin.Resource(nsGVR).Create(ctx, u, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create namespace %s: %v", ns, err)
		}
	}
	applySample(t, admin)

	op := impersonated(t, restCfg, vapOperatorUser)
	wr := impersonated(t, restCfg, vapWriterUser)

	// The binding activates asynchronously — probe with a forbidden write
	// until the denial lands instead of racing the informer.
	err = wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 60*time.Second, true,
		func(ctx context.Context) (bool, error) {
			err := createWS(wr, vapWorkspace("vap-probe", vapManagedNS, true))
			if vapDenied(err) {
				return true, nil
			}
			if err == nil {
				_ = wsClient(admin, vapManagedNS).Delete(ctx, "vap-probe", metav1.DeleteOptions{})
				return false, nil
			}
			return false, err
		})
	if err != nil {
		t.Fatalf("policy never enforced (or probe failed unexpectedly): %v", err)
	}

	// Non-operator writer: create carrying the annotation is denied...
	if err := createWS(wr, vapWorkspace("w-deny-create", vapManagedNS, true)); !vapDenied(err) {
		t.Errorf("writer create with annotation: want VAP denial, got %v", err)
	}
	// ...allowed in a namespace the policy does not match...
	if err := createWS(wr, vapWorkspace("w-unmanaged", vapUnmanagedNS, true)); err != nil {
		t.Errorf("writer create with annotation in unmanaged namespace should pass (matchConditions), got %v", err)
	}
	// ...and allowed inside it when the annotation is absent.
	if err := createWS(wr, vapWorkspace("w-clean", vapManagedNS, false)); err != nil {
		t.Errorf("writer create without annotation should pass, got %v", err)
	}

	// Operator: create carrying the annotation is allowed; that object
	// doubles as the change/remove fixture below.
	if err := createWS(op, vapWorkspace("w-op-annot", vapManagedNS, true)); err != nil {
		t.Errorf("operator create with annotation should pass, got %v", err)
	}

	// UPDATE diffs the annotation oldObject vs object — add, change and
	// remove by a non-operator are all denied.
	clean, err := wsClient(admin, vapManagedNS).Get(ctx, "w-clean", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	add := clean.DeepCopy()
	_ = unstructured.SetNestedField(add.Object, vapAnnotValue, "metadata", "annotations", vapAnnotKey)
	if _, err := wsClient(wr, vapManagedNS).Update(ctx, add, metav1.UpdateOptions{}); !vapDenied(err) {
		t.Errorf("writer update adding annotation: want VAP denial, got %v", err)
	}

	annotated, err := wsClient(admin, vapManagedNS).Get(ctx, "w-op-annot", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	change := annotated.DeepCopy()
	_ = unstructured.SetNestedField(change.Object, `{"name":"forged"}`, "metadata", "annotations", vapAnnotKey)
	if _, err := wsClient(wr, vapManagedNS).Update(ctx, change, metav1.UpdateOptions{}); !vapDenied(err) {
		t.Errorf("writer update changing annotation: want VAP denial, got %v", err)
	}
	remove := annotated.DeepCopy()
	_ = unstructured.SetNestedMap(remove.Object, map[string]any{"example.com/unrelated": "1"}, "metadata", "annotations")
	if _, err := wsClient(wr, vapManagedNS).Update(ctx, remove, metav1.UpdateOptions{}); !vapDenied(err) {
		t.Errorf("writer update removing annotation: want VAP denial, got %v", err)
	}

	// A non-operator update that leaves the annotation alone is fine.
	other := clean.DeepCopy()
	_ = unstructured.SetNestedField(other.Object, "1", "metadata", "annotations", "example.com/unrelated")
	if _, err := wsClient(wr, vapManagedNS).Update(ctx, other, metav1.UpdateOptions{}); err != nil {
		t.Errorf("writer update of unrelated annotation should pass, got %v", err)
	}

	// The operator may add, change or remove it — including removing the
	// annotation entirely from its own mirrored object.
	opEdit, err := wsClient(admin, vapManagedNS).Get(ctx, "w-op-annot", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(opEdit.Object, `{"name":"tpl2"}`, "metadata", "annotations", vapAnnotKey)
	if _, err := wsClient(op, vapManagedNS).Update(ctx, opEdit, metav1.UpdateOptions{}); err != nil {
		t.Errorf("operator update of the annotation should pass, got %v", err)
	}
}

// TestSnapshotAnnotationVAP_SampleAPIVersion pins the sample to the GA
// admissionregistration.k8s.io/v1 surface (Kubernetes >= 1.30) the chart
// renders.
func TestSnapshotAnnotationVAP_SampleAPIVersion(t *testing.T) {
	for i, obj := range sampleDocs(t) {
		if obj.GetAPIVersion() != admissionv1.SchemeGroupVersion.String() {
			t.Errorf("doc %d (%s %s) apiVersion = %s, want %s",
				i, obj.GetKind(), obj.GetName(), obj.GetAPIVersion(), admissionv1.SchemeGroupVersion)
		}
	}
}
