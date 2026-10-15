// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// TestReadRetained_Scope: the single-record read behind GET /v1/data/{id}
// honours tenant and owner scope exactly like the list and carries a nonce.
func TestReadRetained_Scope(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	st := provisioning.NewRetainedStore(db)
	seedWorkspaceRow(t, db, "ws_srcread01", "tenant-it", "iss|sub-1", "src-read")
	rec, err := st.ImportRetained(ctx, provisioning.RetainedDiskInfo{
		TenantID: "tenant-it", Owner: "iss|sub-1", SourceWorkspaceID: "ws_srcread01",
		Runtime: "LinuxContainer", SizeBytes: 2 << 30,
		PVCNamespace: "ns-it", PVCName: "pvc-read", PVCUID: "uid-pvc-read",
	})
	if err != nil {
		t.Fatalf("ImportRetained: %v", err)
	}

	got, err := st.ReadRetained(ctx, "tenant-it", "iss|sub-1", "iss|sub-1", rec.ID)
	if err != nil || got.ID != rec.ID || got.PurgeNonce == "" {
		t.Fatalf("owner read = %+v err=%v, want the record with a nonce", got, err)
	}
	if _, err := st.ReadRetained(ctx, "tenant-it", "iss|admin", "", rec.ID); err != nil {
		t.Fatalf("tenant-admin read (ownerScope \"\"): %v", err)
	}
	if _, err := st.ReadRetained(ctx, "tenant-it", "iss|sub-2", "iss|sub-2", rec.ID); !errors.Is(err, provisioning.ErrRetainedNotFound) {
		t.Fatalf("other owner: err=%v, want ErrRetainedNotFound", err)
	}
	if _, err := st.ReadRetained(ctx, "tenant-other", "iss|admin", "", rec.ID); !errors.Is(err, provisioning.ErrRetainedNotFound) {
		t.Fatalf("other tenant: err=%v, want ErrRetainedNotFound", err)
	}
}
