// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// KASM-3: a ResourceQuota that requires compute requests applies to
// initContainers too — the adapter init must carry requests so kasm pods
// are admitted in quota-governed namespaces. envtest runs a real
// apiserver with the ResourceQuota admission plugin, so this is an
// admission proof, not a shape assertion.
package linux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
)

func kasmEnvtestAssets(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("KUBEBUILDER_ASSETS"); dir != "" {
		return dir
	}
	matches, _ := filepath.Glob(filepath.Join("..", "..", "bin", "k8s", "*-linux-amd64"))
	if len(matches) == 0 {
		t.Skip("KUBEBUILDER_ASSETS unset and no bin/k8s/*-linux-amd64 found")
	}
	return matches[0]
}

func TestKasmAdapterInitAdmittedUnderQuota(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{BinaryAssetsDirectory: kasmEnvtestAssets(t)}
	restCfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	kc, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "kasmquota-"}}
	if err := kc.Create(ctx, ns); err != nil {
		t.Fatalf("namespace: %v", err)
	}
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "compute", Namespace: ns.Name},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("8"),
			corev1.ResourceMemory:           resource.MustParse("16Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
		}},
	}
	if err := kc.Create(ctx, quota); err != nil {
		t.Fatalf("quota: %v", err)
	}
	// envtest runs no kube-controller-manager, so the quota's usage is
	// never observed — and the admission plugin only enforces a quota
	// whose status has been initialized. Seed it the way the controller
	// would.
	quota.Status.Hard = quota.Spec.Hard
	quota.Status.Used = corev1.ResourceList{
		corev1.ResourceCPU:              resource.MustParse("0"),
		corev1.ResourceMemory:           resource.MustParse("0"),
		corev1.ResourceEphemeralStorage: resource.MustParse("0"),
	}
	if err := kc.Status().Update(ctx, quota); err != nil {
		t.Fatalf("quota status: %v", err)
	}

	tpl := testTemplate(nil)
	tpl.Spec.Linux.Adapter = workspacesv1alpha1.AdapterKasm
	ws := testWorkspace()
	ws.Namespace = ns.Name
	pod := buildPod(ws, tpl, nil, testAdapterImage)

	if err := kc.Create(ctx, pod); err != nil {
		t.Fatalf("kasm pod rejected under compute quota (init must carry requests): %v", err)
	}

	// Sanity: the quota really bites — the same pod without init
	// resources must be refused.
	bare := pod.DeepCopy()
	bare.Name += "-bare"
	bare.ResourceVersion = ""
	bare.Spec.InitContainers[0].Resources = corev1.ResourceRequirements{}
	err = kc.Create(ctx, bare)
	if err == nil {
		t.Fatal("pod with resource-less init container was admitted — quota is not enforcing")
	}
	if !strings.Contains(err.Error(), "failed quota") {
		t.Fatalf("expected a quota rejection, got: %v", err)
	}
}
