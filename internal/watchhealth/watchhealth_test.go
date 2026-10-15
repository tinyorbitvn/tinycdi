package watchhealth

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
)

// TestNewVersion_ResyncIsNoEvent: a periodic resync replays the store with
// an unchanged resource version and keeps firing while the watch is down, so
// only a new version counts as a delivered event.
func TestNewVersion_ResyncIsNoEvent(t *testing.T) {
	a := &cdiv1.Workspace{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "41"}}
	b := &cdiv1.Workspace{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "42"}}
	if NewVersion(a, a.DeepCopy()) {
		t.Fatal("resync replay counted as a delivered event")
	}
	if !NewVersion(a, b) {
		t.Fatal("new object version not counted as a delivered event")
	}
}

// TestWorkspaceMetadataVersions: the probe's API side lists metadata per
// watched namespace (or cluster-wide), keys by namespace/name, and surfaces
// a failing API read.
func TestWorkspaceMetadataVersions(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cdiv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ws := func(ns, name string) *cdiv1.Workspace {
		return &cdiv1.Workspace{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ws("ns-a", "one"), ws("ns-b", "two"), ws("ns-c", "three")).Build()
	ctx := context.Background()

	all, err := WorkspaceMetadataVersions(c, nil)(ctx)
	if err != nil || len(all) != 3 || all["ns-c/three"] == "" {
		t.Fatalf("cluster-wide = %v, %v; want 3 versioned entries", all, err)
	}
	some, err := WorkspaceMetadataVersions(c, []string{"ns-a", "ns-b"})(ctx)
	if err != nil || len(some) != 2 || some["ns-a/one"] != all["ns-a/one"] {
		t.Fatalf("namespaced = %v, %v; want ns-a and ns-b only", some, err)
	}
	broken := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("connection refused")
		},
	}).Build()
	if _, err := WorkspaceMetadataVersions(broken, []string{"ns-a"})(ctx); err == nil {
		t.Fatal("probe hid a failing list")
	}
}
