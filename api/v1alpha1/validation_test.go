// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package v1alpha1_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// These tests run against a real envtest apiserver (kube-apiserver + etcd,
// k8s 1.37.0 binaries under bin/k8s via `make envtest` / KUBEBUILDER_ASSETS)
// so that CRD schema and CEL x-kubernetes-validations are actually enforced.
//
// KUBEBUILDER_ASSETS must point at the envtest binaries, e.g.:
//   KUBEBUILDER_ASSETS="$(./bin/setup-envtest use 1.37.x --bin-dir ./bin -p path)" go test ./api/...

var (
	dyn    dynamic.Interface
	crdCli apiextensionsclient.Interface

	tplGVR = schema.GroupVersionResource{Group: "workspaces.cdi.tinyorbit.vn", Version: "v1alpha1", Resource: "workspacetemplates"}
	wsGVR  = schema.GroupVersionResource{Group: "workspaces.cdi.tinyorbit.vn", Version: "v1alpha1", Resource: "workspaces"}
	crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
)

func TestMain(m *testing.M) {
	testEnv := &envtest.Environment{
		CRDInstallOptions: envtest.CRDInstallOptions{
			Paths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
		},
	}
	// Honor KUBEBUILDER_ASSETS if set; otherwise auto-detect the envtest
	// bundle installed under repo bin/ by `make envtest`.
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		matches, _ := filepath.Glob(filepath.Join("..", "..", "bin", "k8s", "*-linux-amd64"))
		if len(matches) == 0 {
			panic("no envtest binaries: run `make envtest` or set KUBEBUILDER_ASSETS")
		}
		abs, err := filepath.Abs(matches[len(matches)-1])
		if err != nil {
			panic(err)
		}
		testEnv.BinaryAssetsDirectory = abs
	}
	cfg, err := testEnv.Start()
	if err != nil {
		panic("envtest start (set KUBEBUILDER_ASSETS, e.g. via `make test`): " + err.Error())
	}
	dyn, err = dynamic.NewForConfig(cfg)
	if err != nil {
		panic(err)
	}
	crdCli, err = apiextensionsclient.NewForConfig(cfg)
	if err != nil {
		panic(err)
	}
	code := m.Run()
	_ = testEnv.Stop()
	os.Exit(code)
}

const testNS = "default"

func digest() string { return strings.Repeat("a", 64) }

func linuxTemplate(name string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "workspaces.cdi.tinyorbit.vn/v1alpha1",
		"kind":       "WorkspaceTemplate",
		"metadata":   map[string]interface{}{"name": name, "namespace": testNS},
		"spec": map[string]interface{}{
			"revision":   "2026-09-a",
			"runtime":    "LinuxContainer",
			"experience": "Browser",
			"linux": map[string]interface{}{
				"image": "registry.local/tinycdi/browser@sha256:" + digest(),
				"browserPolicy": map[string]interface{}{
					"allowedDomains": []interface{}{"*.corp.example"},
				},
			},
			"resources":       map[string]interface{}{"cpu": "2", "memory": "4Gi", "storage": "20Gi"},
			"bootDeadline":    "5m",
			"networkProfile":  "InternetOnly",
			"clipboardPolicy": "Receive",
			"lifecycle": map[string]interface{}{
				"idleTimeout":       "30m",
				"disconnectTimeout": "10m",
				"maxDuration":       "8h",
				"dataPolicy":        "Ephemeral",
			},
		},
	}
}

func windowsTemplate(name string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "workspaces.cdi.tinyorbit.vn/v1alpha1",
		"kind":       "WorkspaceTemplate",
		"metadata":   map[string]interface{}{"name": name, "namespace": testNS},
		"spec": map[string]interface{}{
			"revision":   "2026-09-win1",
			"runtime":    "WindowsVM",
			"experience": "Desktop",
			"windows": map[string]interface{}{
				"sourcePVCRef":   map[string]interface{}{"name": "golden-win11"},
				"sourceChecksum": "sha256:" + digest(),
			},
			"resources":       map[string]interface{}{"cpu": "4", "memory": "8Gi", "storage": "64Gi"},
			"bootDeadline":    "15m",
			"networkProfile":  "ClusterOnly",
			"clipboardPolicy": "Bidirectional",
			"lifecycle": map[string]interface{}{
				"idleTimeout":       "1h",
				"disconnectTimeout": "15m",
				"maxDuration":       "12h",
				"dataPolicy":        "Retain",
			},
		},
	}
}

func workspace(name string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "workspaces.cdi.tinyorbit.vn/v1alpha1",
		"kind":       "Workspace",
		"metadata":   map[string]interface{}{"name": name, "namespace": testNS},
		"spec": map[string]interface{}{
			"templateRef":       map[string]interface{}{"name": "tpl"},
			"ownerSubject":      map[string]interface{}{"issuer": "https://idp.example.com", "subject": "user-123"},
			"desiredState":      "Running",
			"dataPolicy":        "Ephemeral",
			"runtimeGeneration": int64(1),
			"intentRevision":    int64(1),
		},
	}
}

func create(t *testing.T, gvr schema.GroupVersionResource, obj map[string]interface{}) (*unstructured.Unstructured, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return dyn.Resource(gvr).Namespace(testNS).Create(ctx, &unstructured.Unstructured{Object: obj}, metav1.CreateOptions{})
}

func update(t *testing.T, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return dyn.Resource(gvr).Namespace(testNS).Update(ctx, obj, metav1.UpdateOptions{})
}

func get(t *testing.T, gvr schema.GroupVersionResource, name string) *unstructured.Unstructured {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	obj, err := dyn.Resource(gvr).Namespace(testNS).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s/%s: %v", gvr.Resource, name, err)
	}
	return obj
}

// live returns the actual nested map inside obj (not a copy, unlike
// unstructured.NestedMap), creating intermediates as needed, so mutations
// apply in place.
func live(obj map[string]interface{}, keys ...string) map[string]interface{} {
	cur := obj
	for _, k := range keys {
		next, _ := cur[k].(map[string]interface{})
		if next == nil {
			next = map[string]interface{}{}
			cur[k] = next
		}
		cur = next
	}
	return cur
}

// mutate applies fn to the spec of a deep copy of base.
func mutate(base map[string]interface{}, fn func(spec map[string]interface{})) map[string]interface{} {
	out := (&unstructured.Unstructured{Object: base}).DeepCopy().Object
	fn(live(out, "spec"))
	return out
}

func TestWorkspaceTemplateCreateValidation(t *testing.T) {
	cases := []struct {
		name    string
		obj     map[string]interface{}
		wantErr string // substring of the apiserver error; empty means accept
	}{
		{name: "accept linux browser template", obj: linuxTemplate("tpl-acc-linux")},
		{name: "accept windows desktop template", obj: windowsTemplate("tpl-acc-windows")},
		{
			name: "reject unsupported runtime",
			obj: mutate(linuxTemplate("tpl-rej-runtime"), func(s map[string]interface{}) {
				s["runtime"] = "SolarisZone"
			}),
			wantErr: "Unsupported value",
		},
		{
			name: "reject unsupported experience",
			obj: mutate(linuxTemplate("tpl-rej-experience"), func(s map[string]interface{}) {
				s["experience"] = "VR"
			}),
			wantErr: "Unsupported value",
		},
		{
			name: "reject tag-only image (no digest)",
			obj: mutate(linuxTemplate("tpl-rej-tag"), func(s map[string]interface{}) {
				l := live(s, "linux")
				l["image"] = "registry.local/tinycdi/browser:latest"
			}),
			wantErr: "sha256",
		},
		{
			name: "reject truncated digest",
			obj: mutate(linuxTemplate("tpl-rej-shortdigest"), func(s map[string]interface{}) {
				l := live(s, "linux")
				l["image"] = "registry.local/tinycdi/browser@sha256:abc"
			}),
			wantErr: "sha256",
		},
		{
			name: "reject linux runtime without linux block",
			obj: mutate(linuxTemplate("tpl-rej-nolinux"), func(s map[string]interface{}) {
				delete(s, "linux")
			}),
			wantErr: "linux",
		},
		{
			name: "reject windows block on linux runtime",
			obj: mutate(linuxTemplate("tpl-rej-extra"), func(s map[string]interface{}) {
				s["windows"] = map[string]interface{}{
					"sourcePVCRef":   map[string]interface{}{"name": "x"},
					"sourceChecksum": "sha256:" + digest(),
				}
			}),
			wantErr: "windows",
		},
		{
			name: "reject windows runtime without windows block",
			obj: mutate(windowsTemplate("tpl-rej-nowindows"), func(s map[string]interface{}) {
				delete(s, "windows")
			}),
			wantErr: "windows",
		},
		{
			name: "reject cross-namespace disk reference",
			obj: mutate(windowsTemplate("tpl-rej-xns"), func(s map[string]interface{}) {
				w := live(s, "windows")
				ref := live(w, "sourcePVCRef")
				ref["namespace"] = "other-tenant"
			}),
			wantErr: "namespace",
		},
		{
			name: "reject windows template without checksum",
			obj: mutate(windowsTemplate("tpl-rej-nocksum"), func(s map[string]interface{}) {
				w := live(s, "windows")
				delete(w, "sourceChecksum")
			}),
			wantErr: "sourceChecksum",
		},
		{
			name: "reject bad networkProfile",
			obj: mutate(linuxTemplate("tpl-rej-net"), func(s map[string]interface{}) {
				s["networkProfile"] = "Yolo"
			}),
			wantErr: "Unsupported value",
		},
		{
			name: "accept kasm adapter with sessionCmd",
			obj: mutate(linuxTemplate("tpl-acc-kasm"), func(s map[string]interface{}) {
				l := live(s, "linux")
				l["adapter"] = "kasm"
				l["sessionCmd"] = "/usr/bin/chromium-orig --start-maximized"
			}),
		},
		{
			name: "accept kasm adapter without sessionCmd",
			obj: mutate(linuxTemplate("tpl-acc-kasmdesk"), func(s map[string]interface{}) {
				live(s, "linux")["adapter"] = "kasm"
			}),
		},
		{
			name: "reject unsupported adapter",
			obj: mutate(linuxTemplate("tpl-rej-adapter"), func(s map[string]interface{}) {
				live(s, "linux")["adapter"] = "docker"
			}),
			wantErr: "Unsupported value",
		},
		{
			name: "reject sessionCmd without adapter",
			obj: mutate(linuxTemplate("tpl-rej-cmd-noadapter"), func(s map[string]interface{}) {
				live(s, "linux")["sessionCmd"] = "xterm"
			}),
			wantErr: "sessionCmd is only valid with adapter=kasm",
		},
		{
			name: "reject sessionCmd with empty adapter",
			obj: mutate(linuxTemplate("tpl-rej-cmd-emptyadapter"), func(s map[string]interface{}) {
				l := live(s, "linux")
				l["adapter"] = ""
				l["sessionCmd"] = "xterm"
			}),
			wantErr: "sessionCmd",
		},
		{
			name: "reject command with kasm adapter",
			obj: mutate(linuxTemplate("tpl-rej-kasmcmd"), func(s map[string]interface{}) {
				l := live(s, "linux")
				l["adapter"] = "kasm"
				l["command"] = []interface{}{"/bin/sh"}
			}),
			wantErr: "command must be empty with adapter=kasm",
		},
		{
			name: "reject sessionCmd with newline",
			obj: mutate(linuxTemplate("tpl-rej-cmd-nl"), func(s map[string]interface{}) {
				l := live(s, "linux")
				l["adapter"] = "kasm"
				l["sessionCmd"] = "xterm\nrm -rf /"
			}),
			wantErr: "sessionCmd",
		},
		{
			name: "reject oversized sessionCmd",
			obj: mutate(linuxTemplate("tpl-rej-cmd-big"), func(s map[string]interface{}) {
				l := live(s, "linux")
				l["adapter"] = "kasm"
				l["sessionCmd"] = strings.Repeat("x", 513)
			}),
			wantErr: "sessionCmd",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := create(t, tplGVR, tc.obj)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected accept, got reject: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected reject containing %q, got accept", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("rejected but for wrong reason; want substring %q in: %v", tc.wantErr, err)
			}
		})
	}
}

func TestWorkspaceTemplateImmutable(t *testing.T) {
	if _, err := create(t, tplGVR, linuxTemplate("tpl-immutable")); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	for _, tc := range []struct {
		name string
		fn   func(spec map[string]interface{})
	}{
		{"image", func(s map[string]interface{}) {
			l := live(s, "linux")
			l["image"] = "registry.local/tinycdi/browser@sha256:" + strings.Repeat("b", 64)
		}},
		{"runtime", func(s map[string]interface{}) { s["runtime"] = "WindowsVM" }},
		{"revision", func(s map[string]interface{}) { s["revision"] = "2026-09-b" }},
		{"lifecycle", func(s map[string]interface{}) {
			l := live(s, "lifecycle")
			l["dataPolicy"] = "Retain"
		}},
	} {
		t.Run("reject spec mutation of "+tc.name, func(t *testing.T) {
			bad := get(t, tplGVR, "tpl-immutable")
			spec := live(bad.Object, "spec")
			tc.fn(spec)
			_, err := update(t, tplGVR, bad)
			if err == nil {
				t.Fatalf("mutation of %s accepted; template spec must be immutable", tc.name)
			}
			if !strings.Contains(err.Error(), "immutable") {
				t.Fatalf("rejected but for wrong reason; want 'immutable' in: %v", err)
			}
		})
	}
}

func TestWorkspaceCreateValidation(t *testing.T) {
	cases := []struct {
		name    string
		obj     map[string]interface{}
		wantErr string
	}{
		{name: "accept running workspace", obj: workspace("ws-acc-running")},
		{
			name: "accept stopped workspace with generation 0",
			obj: mutate(workspace("ws-acc-stopped"), func(s map[string]interface{}) {
				s["desiredState"] = "Stopped"
				s["runtimeGeneration"] = int64(0)
			}),
		},
		{
			name: "reject unsupported desiredState",
			obj: mutate(workspace("ws-rej-ds"), func(s map[string]interface{}) {
				s["desiredState"] = "Frozen"
			}),
			wantErr: "Unsupported value",
		},
		{
			name: "reject unsupported dataPolicy",
			obj: mutate(workspace("ws-rej-dp"), func(s map[string]interface{}) {
				s["dataPolicy"] = "Forever"
			}),
			wantErr: "Unsupported value",
		},
		{
			name: "reject Running with runtimeGeneration 0",
			obj: mutate(workspace("ws-rej-gen0"), func(s map[string]interface{}) {
				s["runtimeGeneration"] = int64(0)
			}),
			wantErr: "runtimeGeneration",
		},
		{
			name: "reject missing ownerSubject",
			obj: mutate(workspace("ws-rej-noowner"), func(s map[string]interface{}) {
				delete(s, "ownerSubject")
			}),
			wantErr: "ownerSubject",
		},
		{
			name: "reject missing templateRef name",
			obj: mutate(workspace("ws-rej-notpl"), func(s map[string]interface{}) {
				s["templateRef"] = map[string]interface{}{}
			}),
			wantErr: "name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := create(t, wsGVR, tc.obj)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected accept, got reject: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected reject containing %q, got accept", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("rejected but for wrong reason; want substring %q in: %v", tc.wantErr, err)
			}
		})
	}
}

func TestWorkspaceUpdateValidation(t *testing.T) {
	if _, err := create(t, wsGVR, workspace("ws-upd")); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	for _, tc := range []struct {
		name    string
		fn      func(spec map[string]interface{})
		wantErr string // empty = accept
	}{
		{"reject ownerSubject mutation", func(s map[string]interface{}) {
			o := live(s, "ownerSubject")
			o["subject"] = "mallory"
		}, "ownerSubject"},
		{"reject templateRef mutation", func(s map[string]interface{}) {
			r := live(s, "templateRef")
			r["name"] = "other-template"
		}, "templateRef"},
		{"reject dataPolicy mutation", func(s map[string]interface{}) {
			s["dataPolicy"] = "Retain"
		}, "dataPolicy"},
		{"reject runtimeGeneration decrease", func(s map[string]interface{}) {
			s["runtimeGeneration"] = int64(0)
		}, "runtimeGeneration"},
		{"reject intentRevision decrease", func(s map[string]interface{}) {
			s["intentRevision"] = int64(0)
		}, "intentRevision"},
		{"accept stop intent bumping revisions", func(s map[string]interface{}) {
			s["desiredState"] = "Stopped"
			s["intentRevision"] = int64(2)
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := get(t, wsGVR, "ws-upd")
			spec := live(bad.Object, "spec")
			tc.fn(spec)
			_, err := update(t, wsGVR, bad)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected accept, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected reject containing %q, got accept", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("rejected but for wrong reason; want substring %q in: %v", tc.wantErr, err)
			}
		})
	}

	t.Run("accept start with new generation and revision", func(t *testing.T) {
		next := get(t, wsGVR, "ws-upd")
		spec := live(next.Object, "spec")
		spec["desiredState"] = "Running"
		spec["runtimeGeneration"] = int64(2)
		spec["intentRevision"] = int64(3)
		if _, err := update(t, wsGVR, next); err != nil {
			t.Fatalf("expected accept, got: %v", err)
		}
	})
}

// TestWorkspaceRejectsRawPodSpec proves there is no way to smuggle a raw
// PodSpec into a Workspace: the CRD has preserveUnknownFields=false
// (structural schema), so spec.podSpec is silently pruned, never stored.
func TestWorkspaceRejectsRawPodSpec(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	crd, err := crdCli.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, "workspaces.workspaces.cdi.tinyorbit.vn", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get workspace CRD: %v", err)
	}
	if crd.Spec.PreserveUnknownFields {
		t.Fatal("CRD must not set preserveUnknownFields=true")
	}

	obj := mutate(workspace("ws-podspec"), func(s map[string]interface{}) {
		s["podSpec"] = map[string]interface{}{
			"containers": []interface{}{map[string]interface{}{
				"name":  "escape",
				"image": "registry.local/evil@sha256:" + digest(),
			}},
		}
	})
	if _, err := create(t, wsGVR, obj); err != nil {
		t.Fatalf("create with extra field should be accepted-and-pruned, got: %v", err)
	}
	got := get(t, wsGVR, "ws-podspec")
	if _, found, _ := unstructured.NestedMap(got.Object, "spec", "podSpec"); found {
		t.Fatal("spec.podSpec survived round-trip; structural-schema pruning failed")
	}
}
