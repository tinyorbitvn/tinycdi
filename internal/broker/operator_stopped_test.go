// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package broker_test

// FX-R24: a stop the operator applied on its own (its max-duration backstop)
// must reach the workspaces row, fenced so it can never race a newer intent.

import (
	"context"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// operatorStoppedSource is a RunningSource that also reports operator stops.
type operatorStoppedSource struct {
	staticRunning
	stopped []broker.OperatorStopped
}

func (s operatorStoppedSource) OperatorStoppedWorkspaces(context.Context) ([]broker.OperatorStopped, error) {
	return s.stopped, nil
}

// seedStuckRow shapes the row of a start the operator stopped by itself:
// Running/Provisioning at generation gen with intents 1..rev dispatched.
func seedStuckRow(t *testing.T, db *store.DB, wsUID string, gen uint64, rev int) {
	t.Helper()
	ctx := context.Background()
	seedWorkspace(t, db, "tenant-a", alice.Owner(), wsUID)
	if _, err := db.Pool().Exec(ctx,
		`UPDATE workspaces SET desired_state='Running', runtime_generation=$2, phase='Provisioning', intent_revision=$3 WHERE id=$1`,
		wsUID, int64(gen), int64(rev)); err != nil {
		t.Fatalf("shape row: %v", err)
	}
	for r := 1; r <= rev; r++ {
		if _, err := db.Pool().Exec(ctx, `
			INSERT INTO outbox_intent (workspace_id, tenant_id, revision, kind, request_id, dispatched_at)
			VALUES ($1, 'tenant-a', $2, 'create', $3, now())`,
			wsUID, int64(r), wsUID+"-"+string(rune('a'+r))); err != nil {
			t.Fatalf("seed intent %d: %v", r, err)
		}
	}
}

func rowState(t *testing.T, db *store.DB, wsUID string) (desired, phase string, stops int) {
	t.Helper()
	ctx := context.Background()
	if err := db.Pool().QueryRow(ctx,
		`SELECT desired_state, phase FROM workspaces WHERE id=$1`, wsUID).Scan(&desired, &phase); err != nil {
		t.Fatalf("row: %v", err)
	}
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM outbox_intent WHERE workspace_id=$1 AND kind='stop'`, wsUID).Scan(&stops); err != nil {
		t.Fatalf("stops: %v", err)
	}
	return
}

func TestSweep_OperatorStoppedFlipsRow(t *testing.T) {
	db, b, _, _ := setup(t)
	p := broker.NewExpiryPlanner(b)
	seedStuckRow(t, db, "ws-op", 2, 3)
	src := operatorStoppedSource{stopped: []broker.OperatorStopped{{WorkspaceUID: "ws-op", RuntimeGeneration: 2, IntentRevision: 3}}}

	n, err := p.Sweep(context.Background(), src)
	if err != nil || n != 1 {
		t.Fatalf("Sweep = %d, %v; want 1 stop emitted", n, err)
	}
	desired, phase, stops := rowState(t, db, "ws-op")
	if desired != "Stopped" || phase != "Stopping" || stops != 1 {
		t.Fatalf("row = %s/%s with %d stop intents; want Stopped/Stopping with 1", desired, phase, stops)
	}
	// Idempotent: the row is Stopped now, a second sweep emits nothing.
	if n, err := p.Sweep(context.Background(), src); err != nil || n != 0 {
		t.Fatalf("second Sweep = %d, %v; want 0", n, err)
	}
}

func TestSweep_OperatorStoppedNeverRacesNewerIntent(t *testing.T) {
	ctx := context.Background()

	t.Run("a newer outbox revision wins", func(t *testing.T) {
		db, b, _, _ := setup(t)
		p := broker.NewExpiryPlanner(b)
		seedStuckRow(t, db, "ws-new", 3, 4) // start intent rev 4 not yet seen by the CR (rev 3)
		src := operatorStoppedSource{stopped: []broker.OperatorStopped{{WorkspaceUID: "ws-new", RuntimeGeneration: 3, IntentRevision: 3}}}
		if n, err := p.Sweep(ctx, src); err != nil || n != 0 {
			t.Fatalf("Sweep = %d, %v; want 0", n, err)
		}
		if desired, _, stops := rowState(t, db, "ws-new"); desired != "Running" || stops != 0 {
			t.Fatalf("row desired=%s stops=%d; the newer start must stand", desired, stops)
		}
	})

	t.Run("a restarted generation wins", func(t *testing.T) {
		db, b, _, _ := setup(t)
		p := broker.NewExpiryPlanner(b)
		seedStuckRow(t, db, "ws-gen", 3, 3) // row already at gen 3
		src := operatorStoppedSource{stopped: []broker.OperatorStopped{{WorkspaceUID: "ws-gen", RuntimeGeneration: 2, IntentRevision: 3}}}
		if n, err := p.Sweep(ctx, src); err != nil || n != 0 {
			t.Fatalf("Sweep = %d, %v; want 0", n, err)
		}
		if desired, _, stops := rowState(t, db, "ws-gen"); desired != "Running" || stops != 0 {
			t.Fatalf("row desired=%s stops=%d; the restarted generation must stand", desired, stops)
		}
	})

	t.Run("a row already stopped by the user is left alone", func(t *testing.T) {
		db, b, _, _ := setup(t)
		p := broker.NewExpiryPlanner(b)
		seedStuckRow(t, db, "ws-usr", 2, 3)
		if _, err := db.Pool().Exec(ctx, `UPDATE workspaces SET desired_state='Stopped', phase='Stopping' WHERE id='ws-usr'`); err != nil {
			t.Fatal(err)
		}
		src := operatorStoppedSource{stopped: []broker.OperatorStopped{{WorkspaceUID: "ws-usr", RuntimeGeneration: 2, IntentRevision: 3}}}
		if n, err := p.Sweep(ctx, src); err != nil || n != 0 {
			t.Fatalf("Sweep = %d, %v; want 0", n, err)
		}
	})
}
