// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package operator

// envtest coverage for the workspaces.cdi.tinyorbit.vn/v1 CRD contract:
// v1 is the only served+storage version, spec.sessionPolicy is a required
// value (admission rejects its absence), sessionPolicy.fileTransfer is a
// closed enum, and WorkspaceTemplate.spec.fileTransferPolicy defaults to
// Disabled.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var (
	crdv1TplGVR = schema.GroupVersionResource{Group: "workspaces.cdi.tinyorbit.vn", Version: "v1", Resource: "workspacetemplates"}
	crdv1NS     = "crdv1-check"
)

// crdv1Workspace is a schema-valid Workspace; policy == nil omits
// spec.sessionPolicy entirely.
func crdv1Workspace(name string, policy map[string]any) *unstructured.Unstructured {
	spec := map[string]any{
		"templateRef":       map[string]any{"name": "tpl"},
		"ownerSubject":      map[string]any{"issuer": "https://idp.example.com", "subject": "user-1"},
		"desiredState":      "Stopped",
		"dataPolicy":        "Ephemeral",
		"runtimeGeneration": int64(0),
		"intentRevision":    int64(1),
	}
	if policy != nil {
		spec["sessionPolicy"] = policy
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "workspaces.cdi.tinyorbit.vn/v1",
		"kind":       "Workspace",
		"metadata":   map[string]any{"name": name, "namespace": crdv1NS},
		"spec":       spec,
	}}
}

func TestCRDv1_SessionPolicyRequiredAndFileTransferDefault(t *testing.T) {
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

	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dyn.Resource(nsGVR).Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": crdv1NS},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("namespace: %v", err)
	}

	// A WorkspaceTemplate created without spec.fileTransferPolicy reads
	// back with the CRD default Disabled.
	tpl := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "workspaces.cdi.tinyorbit.vn/v1",
		"kind":       "WorkspaceTemplate",
		"metadata":   map[string]any{"name": "tpl", "namespace": crdv1NS},
		"spec": map[string]any{
			"revision":   "2026-10-a",
			"runtime":    "LinuxContainer",
			"experience": "Browser",
			"linux": map[string]any{
				"image": "registry.local/tinycdi/browser@sha256:" +
					strings.Repeat("a", 64),
			},
			"resources":       map[string]any{"cpu": "2", "memory": "4Gi", "storage": "20Gi"},
			"bootDeadline":    "5m",
			"networkProfile":  "Isolated",
			"clipboardPolicy": "Disabled",
			"lifecycle": map[string]any{
				"idleTimeout":       "30m",
				"disconnectTimeout": "10m",
				"maxDuration":       "8h",
				"dataPolicy":        "Ephemeral",
			},
		},
	}}
	created, err := dyn.Resource(crdv1TplGVR).Namespace(crdv1NS).Create(ctx, tpl, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create template: %v", err)
	}
	if ft, found, _ := unstructured.NestedString(created.Object, "spec", "fileTransferPolicy"); !found || ft != "Disabled" {
		t.Fatalf("spec.fileTransferPolicy = %q (found=%v), want defaulted Disabled", ft, found)
	}

	// A Workspace without spec.sessionPolicy is rejected at admission and
	// the error names the missing field.
	if _, err := wsClient(dyn, crdv1NS).Create(ctx, crdv1Workspace("ws-nopol", nil), metav1.CreateOptions{}); err == nil {
		t.Fatal("workspace without sessionPolicy was accepted")
	} else if !strings.Contains(err.Error(), "sessionPolicy") {
		t.Fatalf("rejection does not name sessionPolicy: %v", err)
	}

	// An out-of-enum sessionPolicy.fileTransfer is rejected.
	bad := map[string]any{"clipboard": "Disabled", "fileTransfer": "Sideways"}
	if _, err := wsClient(dyn, crdv1NS).Create(ctx, crdv1Workspace("ws-sideways", bad), metav1.CreateOptions{}); err == nil {
		t.Fatal("workspace with fileTransfer=Sideways was accepted")
	} else if !strings.Contains(err.Error(), "fileTransfer") {
		t.Fatalf("enum rejection does not name fileTransfer: %v", err)
	}

	// The CRD serves and stores exactly one version: v1.
	extCli, err := apiextensionsclient.NewForConfig(restCfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"workspaces.workspaces.cdi.tinyorbit.vn",
		"workspacetemplates.workspaces.cdi.tinyorbit.vn",
	} {
		crd, err := extCli.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		if len(crd.Spec.Versions) != 1 {
			t.Fatalf("%s versions = %+v, want exactly one", name, crd.Spec.Versions)
		}
		v := crd.Spec.Versions[0]
		if v.Name != "v1" || !v.Served || !v.Storage {
			t.Fatalf("%s version = %+v, want {v1 served storage}", name, v)
		}
	}
}
