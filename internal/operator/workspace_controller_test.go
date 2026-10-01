// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// envtest coverage for the Workspace controller (design §4/§5).
//
// TestReconcileLinuxIdempotent proves, against a real apiserver+etcd:
//   - level-based convergence: N reconciles -> exactly one Pod/Service/Secret/
//     NetworkPolicy per Workspace, all named/labeled with the Workspace UID;
//   - a same-named object owned by a different UID is never adopted;
//   - a Workspace under deletion never gets a runtime;
//   - spec.intentRevision <= status.lastAppliedIntentRevision is ignored and
//     can never flip desiredState back (replayed stale intent);
//   - Pod delete/recreate produces a new status.runtimeUID under the same
//     observedRuntimeGeneration;
//   - a fresh reconciler instance (controller restart) resumes mid-provisioning;
//   - boot deadline from the template flips the Workspace to Failed.
//
// Run: KUBEBUILDER_ASSETS=<repo>/bin/k8s/1.37.0-linux-amd64 \
//      go test ./internal/operator/ -run TestReconcileLinuxIdempotent -v

package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

var (
	testScheme = runtime.NewScheme()
)

func init() {
	_ = clientgoscheme.AddToScheme(testScheme)
	_ = workspacesv1alpha1.AddToScheme(testScheme)
}

// startEnv boots envtest once per test process with the project CRDs.
func startEnv(t *testing.T) (*envtest.Environment, client.Client) {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		assets = filepath.Join("..", "..", "bin", "k8s", "1.37.0-linux-amd64")
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return env, c
}

func newReconciler(c client.Client) *WorkspaceReconciler {
	return &WorkspaceReconciler{
		Client:  c,
		Scheme:  testScheme,
		Backend: linux.New(c, linux.Options{}),
	}
}

func reconcile(t *testing.T, r *WorkspaceReconciler, key types.NamespacedName) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile %s: %v", key, err)
	}
	return res
}

func newNamespace(t *testing.T, c client.Client) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		GenerateName: "w2t3-",
	}}
	if err := c.Create(context.Background(), ns); err != nil {
		t.Fatalf("namespace: %v", err)
	}
	return ns.Name
}

func newTemplate(t *testing.T, c client.Client, ns, name string, mutate func(*workspacesv1alpha1.WorkspaceTemplate)) {
	t.Helper()
	tpl := &workspacesv1alpha1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: workspacesv1alpha1.WorkspaceTemplateSpec{
			Revision:   "2026-09-a",
			Runtime:    workspacesv1alpha1.RuntimeLinuxContainer,
			Experience: workspacesv1alpha1.ExperienceDesktop,
			Linux: &workspacesv1alpha1.LinuxRuntimeSpec{
				Image: "tcdi/linux-desktop@sha256:" + fmt.Sprintf("%064x", 1),
			},
			Resources: workspacesv1alpha1.ResourceProfile{
				CPU:     resource.MustParse("500m"),
				Memory:  resource.MustParse("512Mi"),
				Storage: resource.MustParse("1Gi"),
			},
			BootDeadline:    metav1.Duration{Duration: 5 * time.Minute},
			NetworkProfile:  workspacesv1alpha1.NetworkProfileIsolated,
			ClipboardPolicy: workspacesv1alpha1.ClipboardDisabled,
			Lifecycle: workspacesv1alpha1.LifecycleDefaults{
				IdleTimeout:       metav1.Duration{Duration: 30 * time.Minute},
				DisconnectTimeout: metav1.Duration{Duration: 10 * time.Minute},
				MaxDuration:       metav1.Duration{Duration: 8 * time.Hour},
				DataPolicy:        workspacesv1alpha1.DataPolicyEphemeral,
			},
		},
	}
	if mutate != nil {
		mutate(tpl)
	}
	if err := c.Create(context.Background(), tpl); err != nil {
		t.Fatalf("template: %v", err)
	}
}

func newWorkspace(t *testing.T, c client.Client, ns, name, tplName string, mutate func(*workspacesv1alpha1.Workspace)) *workspacesv1alpha1.Workspace {
	t.Helper()
	ws := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			TemplateRef:       workspacesv1alpha1.TemplateReference{Name: tplName},
			OwnerSubject:      workspacesv1alpha1.OwnerSubject{Issuer: "https://issuer.test", Subject: "sub-1"},
			DesiredState:      workspacesv1alpha1.DesiredStateRunning,
			DataPolicy:        workspacesv1alpha1.DataPolicyEphemeral,
			RuntimeGeneration: 1,
			IntentRevision:    1,
		},
	}
	if mutate != nil {
		mutate(ws)
	}
	if err := c.Create(context.Background(), ws); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	return ws
}

func getWorkspace(t *testing.T, c client.Client, key types.NamespacedName) *workspacesv1alpha1.Workspace {
	t.Helper()
	ws := &workspacesv1alpha1.Workspace{}
	if err := c.Get(context.Background(), key, ws); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	return ws
}

func listOwned(t *testing.T, c client.Client, ns string, uid types.UID, list client.ObjectList, kind string) int {
	t.Helper()
	if err := c.List(context.Background(), list,
		client.InNamespace(ns),
		client.MatchingLabels{linux.LabelWorkspaceUID: string(uid)}); err != nil {
		t.Fatalf("list %s: %v", kind, err)
	}
	switch l := list.(type) {
	case *corev1.PodList:
		return len(l.Items)
	case *corev1.ServiceList:
		return len(l.Items)
	case *corev1.SecretList:
		return len(l.Items)
	case *networkingv1.NetworkPolicyList:
		return len(l.Items)
	case *corev1.PersistentVolumeClaimList:
		return len(l.Items)
	}
	return -1
}

func countChildren(t *testing.T, c client.Client, ns string, uid types.UID) (pods, svcs, secrets, netpols int) {
	t.Helper()
	return listOwned(t, c, ns, uid, &corev1.PodList{}, "pods"),
		listOwned(t, c, ns, uid, &corev1.ServiceList{}, "services"),
		listOwned(t, c, ns, uid, &corev1.SecretList{}, "secrets"),
		listOwned(t, c, ns, uid, &networkingv1.NetworkPolicyList{}, "netpols")
}

// markPodReady simulates the kubelet: Pod scheduled + Ready condition true
// (readinessProbe exec of the runtime healthcheck passing).
func markPodReady(t *testing.T, c client.Client, pod *corev1.Pod) {
	t.Helper()
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, Reason: "Scheduled"},
		{Type: corev1.PodReady, Status: corev1.ConditionTrue, Reason: "Ready"},
	}
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("mark pod ready: %v", err)
	}
}

func thePod(t *testing.T, c client.Client, ns string, uid types.UID) *corev1.Pod {
	t.Helper()
	pods := &corev1.PodList{}
	if err := c.List(context.Background(), pods, client.InNamespace(ns),
		client.MatchingLabels{linux.LabelWorkspaceUID: string(uid)}); err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("want exactly 1 pod, got %d", len(pods.Items))
	}
	return &pods.Items[0]
}

func condition(ws *workspacesv1alpha1.Workspace, typ string) *metav1.Condition {
	for i := range ws.Status.Conditions {
		if ws.Status.Conditions[i].Type == typ {
			return &ws.Status.Conditions[i]
		}
	}
	return nil
}

func TestReconcileLinuxIdempotent(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()

	t.Run("ten reconciles converge to one pod and service", func(t *testing.T) {
		ns := newNamespace(t, c)
		newTemplate(t, c, ns, "tpl-basic", nil)
		ws := newWorkspace(t, c, ns, "ws-basic", "tpl-basic", nil)
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}
		r := newReconciler(c)

		for i := 0; i < 10; i++ {
			reconcile(t, r, key)
		}

		pods, svcs, secrets, netpols := countChildren(t, c, ns, ws.UID)
		if pods != 1 || svcs != 1 || secrets != 1 || netpols != 1 {
			t.Fatalf("children = %d pods, %d svcs, %d secrets, %d netpols; want 1 each",
				pods, svcs, secrets, netpols)
		}

		pod := thePod(t, c, ns, ws.UID)
		if pod.Name != linux.PodName(ws.UID) {
			t.Fatalf("pod name %q, want %q", pod.Name, linux.PodName(ws.UID))
		}
		// Security contract: non-root, drop ALL, no privesc, RuntimeDefault seccomp,
		// readiness probe exec healthcheck, /dev/shm bounded emptyDir Memory.
		if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.RunAsNonRoot == nil ||
			!*pod.Spec.SecurityContext.RunAsNonRoot {
			t.Fatalf("pod must runAsNonRoot")
		}
		if pod.Spec.SecurityContext.SeccompProfile == nil ||
			pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Fatalf("pod must default to RuntimeDefault seccomp")
		}
		ctr := pod.Spec.Containers[0]
		if ctr.SecurityContext == nil || ctr.SecurityContext.AllowPrivilegeEscalation == nil ||
			*ctr.SecurityContext.AllowPrivilegeEscalation {
			t.Fatalf("container must forbid privilege escalation")
		}
		if len(ctr.SecurityContext.Capabilities.Drop) != 1 ||
			ctr.SecurityContext.Capabilities.Drop[0] != "ALL" {
			t.Fatalf("container must drop ALL capabilities, got %v", ctr.SecurityContext.Capabilities.Drop)
		}
		if ctr.ReadinessProbe == nil || ctr.ReadinessProbe.Exec == nil {
			t.Fatalf("readinessProbe must be exec healthcheck")
		}
		var shm *corev1.Volume
		for i := range pod.Spec.Volumes {
			if pod.Spec.Volumes[i].Name == "dshm" {
				shm = &pod.Spec.Volumes[i]
			}
		}
		if shm == nil || shm.EmptyDir == nil || shm.EmptyDir.Medium != corev1.StorageMediumMemory ||
			shm.EmptyDir.SizeLimit == nil {
			t.Fatalf("/dev/shm must be a bounded emptyDir Memory volume")
		}

		// Service selector is pinned to the workspace UID.
		svcList := &corev1.ServiceList{}
		if err := c.List(context.Background(), svcList, client.InNamespace(ns),
			client.MatchingLabels{linux.LabelWorkspaceUID: string(ws.UID)}); err != nil {
			t.Fatal(err)
		}
		if svcList.Items[0].Spec.Selector[linux.LabelWorkspaceUID] != string(ws.UID) {
			t.Fatalf("service selector must include workspace UID: %v", svcList.Items[0].Spec.Selector)
		}

		// Secret: per-workspace runtime credential files, never logged/statused.
		sec := &corev1.Secret{}
		if err := c.Get(context.Background(),
			types.NamespacedName{Name: linux.SecretName(ws.UID), Namespace: ns}, sec); err != nil {
			t.Fatalf("runtime secret: %v", err)
		}
		for _, k := range []string{"username", "password", "tls.crt", "tls.key"} {
			if len(sec.Data[k]) == 0 {
				t.Fatalf("secret missing key %q", k)
			}
		}

		// Not Ready while the kubelet has not reported readiness.
		wsNow := getWorkspace(t, c, key)
		if wsNow.Status.Phase == workspacesv1alpha1.WorkspacePhaseReady {
			t.Fatalf("phase=Ready without a passing readiness probe")
		}
		if cond := condition(wsNow, workspacesv1alpha1.ConditionAdmitted); cond == nil ||
			cond.Status != metav1.ConditionTrue {
			t.Fatalf("Admitted condition missing/false: %+v", cond)
		}

		// Readiness probe passes -> Ready, status carries incarnation identity.
		markPodReady(t, c, pod)
		reconcile(t, r, key)
		wsNow = getWorkspace(t, c, key)
		if wsNow.Status.Phase != workspacesv1alpha1.WorkspacePhaseReady {
			t.Fatalf("phase=%q after ready pod, want Ready (conds=%v)",
				wsNow.Status.Phase, wsNow.Status.Conditions)
		}
		if wsNow.Status.RuntimeUID != string(pod.UID) {
			t.Fatalf("runtimeUID=%q want pod UID %q", wsNow.Status.RuntimeUID, pod.UID)
		}
		if wsNow.Status.ObservedRuntimeGeneration != 1 {
			t.Fatalf("observedRuntimeGeneration=%d want 1", wsNow.Status.ObservedRuntimeGeneration)
		}
		if wsNow.Status.ObservedGeneration != wsNow.Generation {
			t.Fatalf("observedGeneration=%d want %d", wsNow.Status.ObservedGeneration, wsNow.Generation)
		}
		if cond := condition(wsNow, workspacesv1alpha1.ConditionRuntimeReady); cond == nil ||
			cond.Status != metav1.ConditionTrue {
			t.Fatalf("RuntimeReady condition not true: %+v", cond)
		}
	})

	t.Run("foreign uid object is not adopted", func(t *testing.T) {
		ns := newNamespace(t, c)
		newTemplate(t, c, ns, "tpl-foreign", nil)
		ws := newWorkspace(t, c, ns, "ws-foreign", "tpl-foreign", nil)
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}
		r := newReconciler(c)

		// Squat the deterministic child name with a foreign object before the
		// controller ever runs.
		foreign := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:      linux.PodName(ws.UID),
			Namespace: ns,
			Labels:    map[string]string{"app": "someone-else"},
		}, Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "x", Image: "busybox"}},
		}}
		if err := c.Create(context.Background(), foreign); err != nil {
			t.Fatalf("foreign pod: %v", err)
		}

		for i := 0; i < 3; i++ {
			reconcile(t, r, key)
		}

		got := &corev1.Pod{}
		if err := c.Get(context.Background(),
			types.NamespacedName{Name: foreign.Name, Namespace: ns}, got); err != nil {
			t.Fatalf("foreign pod get: %v", err)
		}
		if got.UID != foreign.UID {
			t.Fatalf("foreign pod was replaced (uid %s -> %s)", foreign.UID, got.UID)
		}
		if len(got.OwnerReferences) != 0 {
			t.Fatalf("foreign pod adopted: ownerRefs=%v", got.OwnerReferences)
		}
		if got.Labels["app"] != "someone-else" || got.Spec.Containers[0].Image != "busybox" {
			t.Fatalf("foreign pod was mutated")
		}
		wsNow := getWorkspace(t, c, key)
		if cond := condition(wsNow, workspacesv1alpha1.ConditionDegraded); cond == nil ||
			cond.Status != metav1.ConditionTrue {
			t.Fatalf("expected Degraded=True on name conflict, conds=%v", wsNow.Status.Conditions)
		}
	})

	t.Run("deletion timestamp prevents runtime creation", func(t *testing.T) {
		ns := newNamespace(t, c)
		newTemplate(t, c, ns, "tpl-del", nil)
		ws := newWorkspace(t, c, ns, "ws-del", "tpl-del", func(ws *workspacesv1alpha1.Workspace) {
			// the applier's platform workspace-id label — teardown keys
			// broker seams by it
			ws.Labels = map[string]string{provisioning.LabelWorkspaceUID: "ws_it_del"}
		})
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}

		// Attach the operator finalizer first (as if an earlier reconcile saw the
		// object), then delete so the object lingers in Terminating.
		if err := c.Get(context.Background(), key, ws); err != nil {
			t.Fatal(err)
		}
		ws.Finalizers = append(ws.Finalizers, FinalizerRuntimeCleanup)
		if err := c.Update(context.Background(), ws); err != nil {
			t.Fatal(err)
		}
		if err := c.Delete(context.Background(), ws); err != nil {
			t.Fatal(err)
		}

		r := newReconciler(c)
		reconcile(t, r, key)

		pods, svcs, secrets, netpols := countChildren(t, c, ns, ws.UID)
		if pods+svcs+secrets+netpols != 0 {
			t.Fatalf("runtime children created under deletionTimestamp")
		}
		// Finalizer released -> apiserver removes the object.
		if err := c.Get(context.Background(), key, &workspacesv1alpha1.Workspace{}); err == nil {
			t.Fatalf("workspace still present after finalizer removal")
		}
	})

	t.Run("stale intent revision is ignored", func(t *testing.T) {
		ns := newNamespace(t, c)
		newTemplate(t, c, ns, "tpl-stale", nil)
		ws := newWorkspace(t, c, ns, "ws-stale", "tpl-stale", nil)
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}
		r := newReconciler(c)

		reconcile(t, r, key)
		pod := thePod(t, c, ns, ws.UID)
		markPodReady(t, c, pod)
		reconcile(t, r, key)

		// Apply intent rev 2: Stop.
		wsNow := getWorkspace(t, c, key)
		wsNow.Spec.DesiredState = workspacesv1alpha1.DesiredStateStopped
		wsNow.Spec.IntentRevision = 2
		if err := c.Update(context.Background(), wsNow); err != nil {
			t.Fatalf("stop intent: %v", err)
		}
		for i := 0; i < 5; i++ {
			reconcile(t, r, key)
		}
		wsNow = getWorkspace(t, c, key)
		if wsNow.Status.Phase != workspacesv1alpha1.WorkspacePhaseStopped {
			t.Fatalf("phase=%q want Stopped", wsNow.Status.Phase)
		}
		if pods := listOwned(t, c, ns, ws.UID, &corev1.PodList{}, "pods"); pods != 0 {
			t.Fatalf("pod still present after stop intent")
		}

		// Replay a stale intent (rev 3 < lastApplied 5). The API alone writes
		// spec; here we simulate the operator having already applied a newer
		// intent stream by lifting lastAppliedIntentRevision + applied-intent
		// record, then presenting the stale spec.
		wsNow = getWorkspace(t, c, key)
		wsNow.Status.LastAppliedIntentRevision = 5
		if err := c.Status().Update(context.Background(), wsNow); err != nil {
			t.Fatalf("bump lastApplied: %v", err)
		}
		wsNow = getWorkspace(t, c, key)
		applied, _ := json.Marshal(AppliedIntent{
			Revision:          5,
			DesiredState:      workspacesv1alpha1.DesiredStateStopped,
			RuntimeGeneration: 1,
			DataPolicy:        workspacesv1alpha1.DataPolicyEphemeral,
		})
		wsNow.Annotations[AnnotationAppliedIntent] = string(applied)
		if err := c.Update(context.Background(), wsNow); err != nil {
			t.Fatalf("applied annotation: %v", err)
		}
		wsNow = getWorkspace(t, c, key)
		wsNow.Spec.DesiredState = workspacesv1alpha1.DesiredStateRunning
		wsNow.Spec.IntentRevision = 3
		if err := c.Update(context.Background(), wsNow); err != nil {
			t.Fatalf("stale spec: %v", err)
		}

		for i := 0; i < 5; i++ {
			reconcile(t, r, key)
		}
		wsNow = getWorkspace(t, c, key)
		if pods := listOwned(t, c, ns, ws.UID, &corev1.PodList{}, "pods"); pods != 0 {
			t.Fatalf("stale intent resurrected a pod")
		}
		if wsNow.Status.Phase != workspacesv1alpha1.WorkspacePhaseStopped {
			t.Fatalf("stale intent reversed desiredState, phase=%q", wsNow.Status.Phase)
		}
		if wsNow.Status.LastAppliedIntentRevision != 5 {
			t.Fatalf("lastAppliedIntentRevision=%d regressed, want 5",
				wsNow.Status.LastAppliedIntentRevision)
		}
	})

	t.Run("pod replacement keeps generation and changes runtimeUID", func(t *testing.T) {
		ns := newNamespace(t, c)
		newTemplate(t, c, ns, "tpl-repl", nil)
		ws := newWorkspace(t, c, ns, "ws-repl", "tpl-repl", nil)
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}
		r := newReconciler(c)

		reconcile(t, r, key)
		pod1 := thePod(t, c, ns, ws.UID)
		markPodReady(t, c, pod1)
		reconcile(t, r, key)
		wsNow := getWorkspace(t, c, key)
		uid1 := wsNow.Status.RuntimeUID
		if uid1 == "" || uid1 != string(pod1.UID) {
			t.Fatalf("runtimeUID=%q want %q", uid1, pod1.UID)
		}

		// Template snapshot is immutable for the running generation: deleting
		// the template must not stop convergence.
		if err := c.Delete(context.Background(), &workspacesv1alpha1.WorkspaceTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "tpl-repl", Namespace: ns},
		}); err != nil {
			t.Fatalf("delete template: %v", err)
		}

		if err := c.Delete(context.Background(), pod1); err != nil {
			t.Fatalf("delete pod: %v", err)
		}
		reconcile(t, r, key)
		pod2 := thePod(t, c, ns, ws.UID)
		if pod2.UID == pod1.UID {
			t.Fatalf("pod was not actually recreated")
		}
		markPodReady(t, c, pod2)
		reconcile(t, r, key)
		wsNow = getWorkspace(t, c, key)
		if wsNow.Status.RuntimeUID == uid1 || wsNow.Status.RuntimeUID != string(pod2.UID) {
			t.Fatalf("runtimeUID=%q want new %q", wsNow.Status.RuntimeUID, pod2.UID)
		}
		if wsNow.Status.ObservedRuntimeGeneration != 1 {
			t.Fatalf("observedRuntimeGeneration=%d moved, want 1",
				wsNow.Status.ObservedRuntimeGeneration)
		}
	})

	t.Run("controller restart resumes mid-provisioning", func(t *testing.T) {
		ns := newNamespace(t, c)
		newTemplate(t, c, ns, "tpl-restart", nil)
		ws := newWorkspace(t, c, ns, "ws-restart", "tpl-restart", nil)
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}

		// First reconcile by instance A.
		reconcile(t, newReconciler(c), key)

		// "Restart": a brand-new reconciler instance continues from persisted
		// cluster state only.
		rB := newReconciler(c)
		for i := 0; i < 5; i++ {
			reconcile(t, rB, key)
		}
		pods, svcs, _, _ := countChildren(t, c, ns, ws.UID)
		if pods != 1 || svcs != 1 {
			t.Fatalf("after restart: %d pods %d svcs, want 1/1", pods, svcs)
		}
		pod := thePod(t, c, ns, ws.UID)
		markPodReady(t, c, pod)
		reconcile(t, rB, key)
		wsNow := getWorkspace(t, c, key)
		if wsNow.Status.Phase != workspacesv1alpha1.WorkspacePhaseReady {
			t.Fatalf("phase=%q after restart, want Ready", wsNow.Status.Phase)
		}
	})

	t.Run("boot deadline marks workspace failed", func(t *testing.T) {
		ns := newNamespace(t, c)
		newTemplate(t, c, ns, "tpl-deadline", func(tpl *workspacesv1alpha1.WorkspaceTemplate) {
			tpl.Spec.BootDeadline = metav1.Duration{Duration: time.Millisecond}
		})
		ws := newWorkspace(t, c, ns, "ws-deadline", "tpl-deadline", nil)
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}
		r := newReconciler(c)

		reconcile(t, r, key) // applies intent, starts the deadline clock
		time.Sleep(10 * time.Millisecond)
		reconcile(t, r, key)

		wsNow := getWorkspace(t, c, key)
		if wsNow.Status.Phase != workspacesv1alpha1.WorkspacePhaseFailed {
			t.Fatalf("phase=%q want Failed", wsNow.Status.Phase)
		}
		cond := condition(wsNow, workspacesv1alpha1.ConditionDegraded)
		if cond == nil || cond.Status != metav1.ConditionTrue ||
			cond.Reason != ReasonBootDeadlineExceeded {
			t.Fatalf("want Degraded=True reason=%q, got %+v",
				ReasonBootDeadlineExceeded, cond)
		}
	})
}

// TestBootDeadlineIncarnationAnchor covers defect F2: a restored Workspace
// carries an applied-intent record whose appliedAt predates the backup, so
// a boot deadline anchored at appliedAt is already in the past and the
// first reconcile latches Failed while the recreated runtime converges.
// The deadline is anchored at max(appliedAt, first observation of the
// current runtime incarnation) instead.
func TestBootDeadlineIncarnationAnchor(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()

	staleApplied := map[string]string{}
	raw, _ := json.Marshal(AppliedIntent{
		Revision:          1,
		DesiredState:      workspacesv1alpha1.DesiredStateRunning,
		RuntimeGeneration: 1,
		DataPolicy:        workspacesv1alpha1.DataPolicyEphemeral,
		AppliedAt:         metav1.NewTime(time.Now().Add(-2 * time.Hour)),
	})
	staleApplied[AnnotationAppliedIntent] = string(raw)

	t.Run("restored CR with stale appliedAt converges to Ready", func(t *testing.T) {
		ns := newNamespace(t, c)
		newTemplate(t, c, ns, "tpl-rst", func(tpl *workspacesv1alpha1.WorkspaceTemplate) {
			tpl.Spec.BootDeadline = metav1.Duration{Duration: 400 * time.Millisecond}
		})
		ws := newWorkspace(t, c, ns, "ws-rst", "tpl-rst", func(ws *workspacesv1alpha1.Workspace) {
			ws.Annotations = staleApplied
		})
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}
		r := newReconciler(c)

		// First reconcile: incarnation appears; deadline must be anchored
		// to it — NOT the 2h-stale appliedAt — so the workspace provisions
		// instead of instantly latching Failed/BootDeadlineExceeded.
		reconcile(t, r, key)
		wsNow := getWorkspace(t, c, key)
		if wsNow.Status.Phase == workspacesv1alpha1.WorkspacePhaseFailed {
			t.Fatalf("restored workspace latched Failed (phase=%v, conds=%v)",
				wsNow.Status.Phase, wsNow.Status.Conditions)
		}
		if wsNow.Status.Phase != workspacesv1alpha1.WorkspacePhaseProvisioning &&
			wsNow.Status.Phase != workspacesv1alpha1.WorkspacePhasePending {
			t.Fatalf("phase=%q want Provisioning while runtime boots", wsNow.Status.Phase)
		}
		pod := thePod(t, c, ns, ws.UID)
		markPodReady(t, c, pod)
		reconcile(t, r, key)
		wsNow = getWorkspace(t, c, key)
		if wsNow.Status.Phase != workspacesv1alpha1.WorkspacePhaseReady {
			t.Fatalf("phase=%q want Ready", wsNow.Status.Phase)
		}
		// Well past appliedAt+bootDeadline now — must stay Ready.
		time.Sleep(450 * time.Millisecond)
		reconcile(t, r, key)
		wsNow = getWorkspace(t, c, key)
		if wsNow.Status.Phase != workspacesv1alpha1.WorkspacePhaseReady {
			t.Fatalf("phase=%q must stay Ready past stale-appliedAt deadline", wsNow.Status.Phase)
		}
	})

	t.Run("genuinely stuck boot still fails after deadline", func(t *testing.T) {
		ns := newNamespace(t, c)
		newTemplate(t, c, ns, "tpl-stuck", func(tpl *workspacesv1alpha1.WorkspaceTemplate) {
			tpl.Spec.BootDeadline = metav1.Duration{Duration: 300 * time.Millisecond}
		})
		ws := newWorkspace(t, c, ns, "ws-stuck", "tpl-stuck", func(ws *workspacesv1alpha1.Workspace) {
			ws.Annotations = staleApplied
		})
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}
		r := newReconciler(c)

		reconcile(t, r, key) // incarnation observed; deadline = incarnation start + 300ms
		wsNow := getWorkspace(t, c, key)
		if wsNow.Status.Phase == workspacesv1alpha1.WorkspacePhaseFailed {
			t.Fatalf("incarnation window must start at first observation, not stale appliedAt")
		}
		time.Sleep(400 * time.Millisecond)
		reconcile(t, r, key) // pod never ready -> deadline exceeded
		wsNow = getWorkspace(t, c, key)
		if wsNow.Status.Phase != workspacesv1alpha1.WorkspacePhaseFailed {
			t.Fatalf("phase=%q want Failed after boot deadline", wsNow.Status.Phase)
		}
		if cond := condition(wsNow, workspacesv1alpha1.ConditionDegraded); cond == nil ||
			cond.Status != metav1.ConditionTrue || cond.Reason != ReasonBootDeadlineExceeded {
			t.Fatalf("want Degraded=True reason=%q, got %+v", ReasonBootDeadlineExceeded, cond)
		}
	})
}

// TestTemplateCatalogFallback covers the F1 follow-through: a Workspace
// whose spec.templateRef.name is a catalog base name resolves to the
// newest published immutable revision ("<name>-<hash8>" carrying the
// workspaces.cdi.tinyorbit.vn/catalog-name label) when no object bears the
// exact name — e.g. a stopped workspace created before an upgrade that
// swapped the seeded template objects.
func TestTemplateCatalogFallback(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-fb-aaaa0000", func(tpl *workspacesv1alpha1.WorkspaceTemplate) {
		tpl.Labels = map[string]string{provisioning.LabelCatalogName: "tpl-fb"}
	})
	// The workspace references the bare base name — no such object exists.
	ws := newWorkspace(t, c, ns, "ws-fb", "tpl-fb", nil)
	key := types.NamespacedName{Name: ws.Name, Namespace: ns}
	r := newReconciler(c)

	reconcile(t, r, key)
	wsNow := getWorkspace(t, c, key)
	if cond := condition(wsNow, workspacesv1alpha1.ConditionDegraded); cond != nil &&
		cond.Status == metav1.ConditionTrue && cond.Reason == ReasonTemplateNotFound {
		t.Fatalf("base-name templateRef must resolve via catalog-name label, got TemplateNotFound")
	}
	pod := thePod(t, c, ns, ws.UID)
	markPodReady(t, c, pod)
	reconcile(t, r, key)
	wsNow = getWorkspace(t, c, key)
	if wsNow.Status.Phase != workspacesv1alpha1.WorkspacePhaseReady {
		t.Fatalf("phase=%q want Ready via fallback-resolved template", wsNow.Status.Phase)
	}
}

// TestTemplateRejectedAppArmor covers defect I-10: a WorkspaceTemplate
// carrying an out-of-policy workspaces.cdi.tinyorbit.vn/apparmor-profile value
// (anything but "localhost/<name>" or "runtime/default") is rejected by the
// backend before any runtime child exists; the Workspace surfaces
// Degraded=True reason=TemplateRejected instead of silently running under
// the containerd default AppArmor profile.
func TestTemplateRejectedAppArmor(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-badaa", func(tpl *workspacesv1alpha1.WorkspaceTemplate) {
		tpl.Annotations = map[string]string{
			linux.AnnotationAppArmorProfile: "unconfined",
		}
	})
	ws := newWorkspace(t, c, ns, "ws-badaa", "tpl-badaa", nil)
	key := types.NamespacedName{Name: ws.Name, Namespace: ns}
	r := newReconciler(c)

	reconcile(t, r, key)
	reconcile(t, r, key)

	wsNow := getWorkspace(t, c, key)
	cond := condition(wsNow, workspacesv1alpha1.ConditionDegraded)
	if cond == nil || cond.Status != metav1.ConditionTrue ||
		cond.Reason != ReasonTemplateRejected {
		t.Fatalf("want Degraded=True reason=%q, got %+v", ReasonTemplateRejected, cond)
	}
	if got := listOwned(t, c, ns, ws.UID, &corev1.PodList{}, "pods"); got != 0 {
		t.Fatalf("rejected template must create no pod, got %d", got)
	}
}
