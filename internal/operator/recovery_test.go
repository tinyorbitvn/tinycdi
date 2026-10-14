package operator

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
	tcdiruntime "github.com/tinyorbitvn/tinycdi/internal/runtime"
)

func readyWorkspace(t *testing.T, name string, mutate func(*cdiv1.Workspace)) (*WorkspaceReconciler, types.NamespacedName, *cdiv1.Workspace, func()) {
	t.Helper()
	env, c := startEnv(t)
	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-"+name, nil)
	ws := newWorkspace(t, c, ns, name, "tpl-"+name, mutate)
	key := types.NamespacedName{Name: ws.Name, Namespace: ns}
	r := newReconciler(c)
	reconcile(t, r, key)
	markPodReady(t, c, thePod(t, c, ns, ws.UID))
	reconcile(t, r, key)
	if got := getWorkspace(t, c, key); got.Status.Phase != cdiv1.WorkspacePhaseReady {
		t.Fatalf("setup: phase %q, want Ready", got.Status.Phase)
	}
	return r, key, ws, func() { _ = env.Stop() }
}

func TestRecovery_DeletedPodGoesRuntimeLostWithoutRebuild(t *testing.T) {
	r, key, ws, stop := readyWorkspace(t, "ws-lost", nil)
	defer stop()
	ctx := context.Background()
	pod := thePod(t, r.Client, key.Namespace, ws.UID)
	if err := r.Client.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, key)
	got := getWorkspace(t, r.Client, key)
	if got.Status.Phase != cdiv1.WorkspacePhaseFailed || degradedReasonOf(got) != cdiv1.FailureRuntimeLost {
		t.Fatalf("phase=%q reason=%q, want Failed/RuntimeLost", got.Status.Phase, degradedReasonOf(got))
	}
	reconcile(t, r, key)
	pods := &corev1.PodList{}
	if err := r.Client.List(ctx, pods); err != nil {
		t.Fatal(err)
	}
	for _, p := range pods.Items {
		if p.Namespace == key.Namespace && p.DeletionTimestamp == nil {
			t.Fatalf("pod %s rebuilt inside the lost generation", p.Name)
		}
	}
}

func TestRecovery_NewGenerationAfterRuntimeLostComesBackReady(t *testing.T) {
	r, key, ws, stop := readyWorkspace(t, "ws-recover", nil)
	defer stop()
	ctx := context.Background()
	old := thePod(t, r.Client, key.Namespace, ws.UID)
	if err := r.Client.Delete(ctx, old); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, key)
	cur := getWorkspace(t, r.Client, key)
	cur.Spec.RuntimeGeneration = 2 // what the applier writes for RestartWorkspace's start intent
	cur.Spec.IntentRevision = 2
	if err := r.Client.Update(ctx, cur); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, key)
	pod := thePod(t, r.Client, key.Namespace, ws.UID)
	if pod.UID == old.UID {
		t.Fatal("recovery reused the lost incarnation")
	}
	markPodReady(t, r.Client, pod)
	reconcile(t, r, key)
	got := getWorkspace(t, r.Client, key)
	if got.Status.Phase != cdiv1.WorkspacePhaseReady || got.Status.ObservedRuntimeGeneration != 2 {
		t.Fatalf("phase=%q gen=%d, want Ready/2", got.Status.Phase, got.Status.ObservedRuntimeGeneration)
	}
	if degradedReasonOf(got) != "" {
		t.Fatalf("Degraded still %q after recovery", degradedReasonOf(got))
	}
}

func TestRecovery_UnrecoverableReasonsFailImmediatelyAndStick(t *testing.T) {
	env, c := startEnv(t)
	defer func() { _ = env.Stop() }()
	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-hv", nil)
	ws := newWorkspace(t, c, ns, "ws-hv", "tpl-hv", func(w *cdiv1.Workspace) { w.Spec.DataPolicy = cdiv1.DataPolicyRetain })
	key := types.NamespacedName{Name: ws.Name, Namespace: ns}
	now := time.Now()
	r := newReconciler(c)
	r.Now = func() time.Time { return now }
	reconcile(t, r, key)
	markPodReady(t, c, thePod(t, c, ns, ws.UID))
	reconcile(t, r, key) // envtest has no volume controller: the claim never binds, the pod was still observed (observedRuntimeGeneration 1)
	// The home claim is deleted underneath the workspace, then a new generation starts.
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(context.Background(), pvcs); err != nil {
		t.Fatal(err)
	}
	for i := range pvcs.Items {
		if pvcs.Items[i].Namespace == ns {
			pvcs.Items[i].Finalizers = nil // no kube-controller-manager here to release pvc-protection
			if err := c.Update(context.Background(), &pvcs.Items[i]); err != nil {
				t.Fatal(err)
			}
			if err := c.Delete(context.Background(), &pvcs.Items[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	cur := getWorkspace(t, c, key)
	cur.Spec.RuntimeGeneration = 2
	cur.Spec.IntentRevision = 2
	if err := c.Update(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, key)
	got := getWorkspace(t, c, key)
	if got.Status.Phase != cdiv1.WorkspacePhaseFailed || degradedReasonOf(got) != cdiv1.FailureHomeVolumeLost {
		t.Fatalf("phase=%q reason=%q, want Failed/HomeVolumeLost at once", got.Status.Phase, degradedReasonOf(got))
	}
	now = now.Add(FailedCleanupDelay + time.Minute)
	reconcile(t, r, key)
	if got := getWorkspace(t, c, key); degradedReasonOf(got) != cdiv1.FailureHomeVolumeLost {
		t.Fatalf("cleanup overwrote the unrecoverable reason with %q", degradedReasonOf(got))
	}
	if err := c.List(context.Background(), pvcs); err != nil {
		t.Fatal(err)
	}
	for _, p := range pvcs.Items {
		if p.Namespace == ns && p.DeletionTimestamp == nil {
			t.Fatalf("a fresh empty home %s was created", p.Name)
		}
	}
}

func TestRecovery_ImagePolicyDeniedOnlyFromPolicyPullMessages(t *testing.T) {
	env, c := startEnv(t)
	defer func() { _ = env.Stop() }()
	for _, tc := range []struct {
		name, msg string
		denied    bool
	}{
		{"policy", "rpc error: failed to pull: 403 Forbidden: image rejected by policy", true},
		{"missing credentials stays a retried pull", "pull access denied, repository does not exist or may require authorization", false},
		{"network error stays a retried pull", "dial tcp 10.0.0.1:443: connection refused", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := newNamespace(t, c)
			newTemplate(t, c, ns, "tpl-ip", nil)
			ws := newWorkspace(t, c, ns, "ws-ip", "tpl-ip", nil)
			key := types.NamespacedName{Name: ws.Name, Namespace: ns}
			r := newReconciler(c)
			reconcile(t, r, key)
			pod := thePod(t, c, ns, ws.UID)
			pod.Status.Phase = corev1.PodPending
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "desktop",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "ErrImagePull", Message: tc.msg}},
			}}
			if err := c.Status().Update(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
			reconcile(t, r, key) // well before the 5 minute boot deadline
			got := getWorkspace(t, c, key)
			if tc.denied {
				if got.Status.Phase != cdiv1.WorkspacePhaseFailed || degradedReasonOf(got) != cdiv1.FailureImagePolicyDenied {
					t.Fatalf("phase=%q reason=%q, want Failed/ImagePolicyDenied", got.Status.Phase, degradedReasonOf(got))
				}
				return
			}
			if got.Status.Phase != cdiv1.WorkspacePhaseProvisioning || degradedReasonOf(got) != "" {
				t.Fatalf("phase=%q reason=%q, want Provisioning without Degraded", got.Status.Phase, degradedReasonOf(got))
			}
		})
	}
}

func TestAdmissionImageDenied(t *testing.T) {
	forbidden := func(msg string) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "ws-x", errors.New(msg))
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"image-policy webhook", forbidden(`admission webhook "policy.sigstore.dev" denied the request: image ghcr.io/x@sha256:00 has no matching signatures`), true},
		{"validating admission policy", forbidden(`ValidatingAdmissionPolicy 'images' with binding 'images' denied request: image must come from the internal registry`), true},
		{"quota", forbidden(`exceeded quota: tenant, requested: pods=1, used: pods=10, limited: pods=10`), false},
		{"pod security", forbidden(`violates PodSecurity "restricted:latest": allowPrivilegeEscalation != false`), false},
		{"not a forbidden error", errors.New(`admission webhook "x" denied the request: image`), false},
	}
	for _, tc := range cases {
		if got := admissionImageDenied(tc.err); got != tc.want {
			t.Errorf("%s: admissionImageDenied = %v, want %v", tc.name, got, tc.want)
		}
	}
	if !imagePolicyDenied(tcdiruntime.Observation{Reason: "ImagePullBackOff", Message: "rejected by policy"}) ||
		imagePolicyDenied(tcdiruntime.Observation{Reason: "Provisioning", Message: "rejected by policy"}) {
		t.Error("imagePolicyDenied must key on a pull-failure reason AND a policy message")
	}
}
