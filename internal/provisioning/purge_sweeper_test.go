// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning_test

// FX-R27 — PurgeSweeper finalizer strip contract:
//   - the pvc-protection strip is a merge patch, never Update: the
//     retained-data Role grants get/list/patch/delete only, so an
//     apiserver that forbids Update (fake-client interceptor answering
//     403 like the rendered backend Role does) must not stall the purge;
//   - the strip runs ONLY on a terminating, verifiably-detached volume:
//     a consumer pod appearing while the PVC already carries a
//     deletionTimestamp keeps pvc-protection, and the record stays
//     Purging until the consumer is gone.

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// forbidPVCUpdateClient simulates the rendered retained-data Role: every
// Update on a PVC answers Forbidden, so a sweep only converges when the
// finalizer strip is a patch.
func forbidPVCUpdateClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(retainedApplyScheme(t)).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
					return apierrors.NewForbidden(
						schema.GroupResource{Resource: "persistentvolumeclaims"},
						obj.GetName(), errors.New("update not granted"))
				}
				return c.Update(ctx, obj)
			},
		}).Build()
}

// purgingRecord drives one retained record to Purging and returns it.
func purgingRecord(t *testing.T, db *store.DB, tenant, pvcNS, pvcName, pvcUID string) provisioning.RetainedRecord {
	t.Helper()
	ctx := context.Background()
	st := provisioning.NewRetainedStore(db)
	seedHeldWorkspace(t, db, tenant, "ws_"+pvcName, quotaVec)
	rec, err := st.ImportRetained(ctx, provisioning.RetainedDiskInfo{
		TenantID: tenant, Owner: "iss|sub-1", SourceWorkspaceID: "ws_" + pvcName,
		Runtime: "LinuxContainer", SizeBytes: 1 << 30,
		PVCNamespace: pvcNS, PVCName: pvcName, PVCUID: pvcUID,
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	got, err := st.ReadRetained(ctx, tenant, "iss|sub-1", "iss|sub-1", rec.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := st.PurgeRetained(ctx, tenant, "iss|sub-1", "iss|sub-1", rec.ID,
		got.PurgeNonce, "", nil); err != nil {
		t.Fatalf("purge: %v", err)
	}
	return rec
}

func retainedState(t *testing.T, st *provisioning.RetainedStore, tenant, id string) provisioning.RetainedDataState {
	t.Helper()
	rec, err := st.GetRetained(context.Background(), tenant, id)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	return rec.State
}

// consumerPod mounts claimName — the live consumer pvc-protection guards.
func consumerPod(ns, claimName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "consumer-" + claimName, Namespace: ns},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "busybox"}},
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName},
				},
			}},
		},
	}
}

// TestPurgeSweeper_StripsProtectionWithPatch: a retained volume held only
// by pvc-protection is deleted and the record completes on ONE sweep —
// even though the Role-equivalent client forbids Update on PVCs.
func TestPurgeSweeper_StripsProtectionWithPatch(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	const tenant, ns, name = "tenant-fx27a", "ns-it", "pvc-fx27a"
	rec := purgingRecord(t, db, tenant, ns, name, "uid-"+name)
	st := provisioning.NewRetainedStore(db)

	pvc := retainedPVC(ns, name, "cruid-fx27a", "ws-"+name, nil)
	pvc.Finalizers = []string{"kubernetes.io/pvc-protection"}
	kc := forbidPVCUpdateClient(t, pvc)

	provisioning.NewPurgeSweeper(st, kc, nil).SweepOnce(ctx)

	got := &corev1.PersistentVolumeClaim{}
	if err := kc.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, got); !apierrors.IsNotFound(err) {
		t.Fatalf("pvc still present after sweep (err=%v) — the strip must be a patch", err)
	}
	if s := retainedState(t, st, tenant, rec.ID); s != provisioning.RetainedStatePurged {
		t.Fatalf("record = %s, want Purged", s)
	}
}

// TestPurgeSweeper_ConsumerKeepsProtection: pvc-protection is never
// stripped while a consumer mounts the volume — not even when the PVC is
// already terminating. The record stays Purging (retried by the sweep)
// and completes once the consumer is gone.
func TestPurgeSweeper_ConsumerKeepsProtection(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	const tenant, ns, name = "tenant-fx27b", "ns-it", "pvc-fx27b"
	rec := purgingRecord(t, db, tenant, ns, name, "uid-"+name)
	st := provisioning.NewRetainedStore(db)

	pvc := retainedPVC(ns, name, "cruid-fx27b", "ws-"+name, nil)
	pvc.Finalizers = []string{"kubernetes.io/pvc-protection"}
	pod := consumerPod(ns, name)
	kc := forbidPVCUpdateClient(t, pvc, pod)

	// The volume is already terminating when the sweep reaches it.
	cur := &corev1.PersistentVolumeClaim{}
	if err := kc.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, cur); err != nil {
		t.Fatalf("pvc: %v", err)
	}
	if err := kc.Delete(ctx, cur); err != nil {
		t.Fatalf("pre-delete pvc: %v", err)
	}

	sweeper := provisioning.NewPurgeSweeper(st, kc, nil)
	sweeper.SweepOnce(ctx)

	got := &corev1.PersistentVolumeClaim{}
	if err := kc.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, got); err != nil {
		t.Fatalf("pvc must survive while a consumer mounts it: %v", err)
	}
	if len(got.Finalizers) != 1 || got.Finalizers[0] != "kubernetes.io/pvc-protection" {
		t.Fatalf("finalizers = %v, want pvc-protection kept", got.Finalizers)
	}
	if s := retainedState(t, st, tenant, rec.ID); s != provisioning.RetainedStatePurging {
		t.Fatalf("record = %s, want Purging while a consumer mounts the volume", s)
	}

	// Consumer gone: the next sweep patches the finalizer away and the
	// purge completes.
	if err := kc.Delete(ctx, pod); err != nil {
		t.Fatalf("delete consumer pod: %v", err)
	}
	sweeper.SweepOnce(ctx)
	if err := kc.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, got); !apierrors.IsNotFound(err) {
		t.Fatalf("pvc still present after consumer left (err=%v)", err)
	}
	if s := retainedState(t, st, tenant, rec.ID); s != provisioning.RetainedStatePurged {
		t.Fatalf("record = %s, want Purged after consumer left", s)
	}
}
