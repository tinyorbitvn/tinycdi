package provisioning_test

import (
	"context"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func bootRows(t *testing.T, db *store.DB, wsID string) []int64 {
	t.Helper()
	rows, err := db.Pool().Query(context.Background(),
		`SELECT runtime_generation FROM workspace_boot WHERE workspace_id = $1 ORDER BY runtime_generation`, wsID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var gens []int64
	for rows.Next() {
		var g int64
		if err := rows.Scan(&g); err != nil {
			t.Fatal(err)
		}
		gens = append(gens, g)
	}
	return gens
}

func TestAppendIntent_RecordsBootRequestPerGeneration(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	const tenant = "tenant-boot"
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.SetQuota(ctx, tx, tenant, provisioning.ResourceVector{
			RunningSlots: 4, CPUMillis: 8000, MemoryBytes: 16 << 30, DiskBytes: 40 << 30})
	}); err != nil {
		t.Fatal(err)
	}
	svc := provisioning.NewService(db)
	req := provisioning.CreateRequest{
		OwnerIssuer: "iss", OwnerSubject: "sub", Name: "boot-ws",
		Template: provisioning.TemplateInfo{ID: "tpl_x", Name: "x", Runtime: "linux", Experience: "desktop"},
		Vector:   quotaVec, DesiredState: "Running", DataPolicy: "Ephemeral",
	}
	res, err := svc.CreateWorkspace(ctx, tenant, "key-boot-1", req, provisioning.RequestHash(req))
	if err != nil {
		t.Fatal(err)
	}
	if got := bootRows(t, db, res.ID); len(got) != 1 {
		t.Fatalf("after create: %v, want one boot row", got)
	}
	if _, err := svc.SignalWorkspace(ctx, tenant, "iss|sub", "", res.ID, "key-boot-stop", provisioning.IntentStop, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if got := bootRows(t, db, res.ID); len(got) != 1 {
		t.Fatalf("a stop must not record a boot: %v", got)
	}
	if _, err := svc.SignalWorkspace(ctx, tenant, "iss|sub", "", res.ID, "key-boot-start", provisioning.IntentStart, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	got := bootRows(t, db, res.ID)
	if len(got) != 2 || got[1] <= got[0] {
		t.Fatalf("after start: %v, want a second row for the bumped generation", got)
	}
}
