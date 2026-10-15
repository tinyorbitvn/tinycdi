// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning_test

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// FX-R25: only a Pod is a live runtime incarnation. A stopped Retain
// workspace's home volume, and a stray default home left by the pre-FX-R20
// attach race, are disk and never block the absence proof.
func TestK8sRuntimeObserver_VolumesNeverBlockAbsenceProof(t *testing.T) {
	ctx := context.Background()
	const ns, platformUID = "ns-a", "ws_attached01"
	tenants := provisioning.TenantNamespaces{"tenant-a": ns}
	cr := obsCR(provisioning.WorkspaceCRName(platformUID), ns, platformUID, "cr-uid-a")
	retained := obsPVC("ws-old-home", ns, "cr-uid-a", true)
	stray := obsPVC("ws-cr-uid-a-home", ns, "cr-uid-a", false)

	c := fake.NewClientBuilder().WithScheme(observerScheme(t)).WithObjects(cr, retained, stray).Build()
	if gone, err := provisioning.NewK8sRuntimeObserver(c, tenants).RuntimeGone(ctx, platformUID); err != nil || !gone {
		t.Fatalf("retained claim + stray home, no pod: gone=%v err=%v, want true", gone, err)
	}

	pod := obsPod("ws-cr-uid-a", ns, "cr-uid-a", provisioning.WorkspaceCRName(platformUID))
	c = fake.NewClientBuilder().WithScheme(observerScheme(t)).WithObjects(cr, retained, stray, pod).Build()
	if gone, err := provisioning.NewK8sRuntimeObserver(c, tenants).RuntimeGone(ctx, platformUID); err != nil || gone {
		t.Fatalf("same volumes with a pod: gone=%v err=%v, want false", gone, err)
	}
}
