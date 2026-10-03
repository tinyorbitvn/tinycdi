// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning_test

// V3.3 (E3): the -image-block-after admission block on the start path —
// a start that cannot move to a fresher revision is refused with
// *ImageStaleError (the API maps it to 409 IMAGE_STALE) when the resolved
// revision's image is older than the limit. A missing imageBuiltAt never
// blocks; <=0 disables the block.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

const blockTestLimit = 45 * 24 * time.Hour

func daysAgoRFC(n int) string {
	return time.Now().UTC().Add(-time.Duration(n) * 24 * time.Hour).Format(time.RFC3339)
}

// blockWorkspace seeds a quota and a Stopped workspace recorded on the
// family's revision aaaa1111 whose image was built oldDays ago.
func blockWorkspace(t *testing.T, cat *fakeTemplateLookup, oldDays int, desired string) (*store.DB, *provisioning.Service, string) {
	t.Helper()
	db := recoveryDB(t)
	ctx := context.Background()
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.SetQuota(ctx, tx, familyTenant, provisioning.ResourceVector{
			RunningSlots: 4, CPUMillis: 8000, MemoryBytes: 16 << 30, DiskBytes: 64 << 30})
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	svc := provisioning.NewService(db).WithTemplateLookup(cat)
	res, err := svc.CreateWorkspace(ctx, familyTenant, "block-seed-001", provisioning.CreateRequest{
		OwnerIssuer: "iss", OwnerSubject: "sub", Name: "block-ws",
		Template: provisioning.TemplateInfo{
			ID: "tpl_linuxdesk-aaaa1111", Name: "linuxdesk", Revision: 1,
			RevisionLabel: "2026-10-a", Runtime: "LinuxContainer", Experience: "Desktop",
			ImageBuiltAt: daysAgoRFC(oldDays),
		},
		Vector: quotaVec, DesiredState: desired, DataPolicy: "Ephemeral",
	}, []byte("seed"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return db, svc, res.ID
}

func blockEntry(family, suffix, revLabel, policy string, oldDays int) provisioning.TemplateCatalogEntry {
	e := familyEntry(family, suffix, revLabel, policy)
	e.ImageBuiltAt = daysAgoRFC(oldDays)
	return e
}

// TestStart_MovesToFreshRevisionInsteadOfBlocking: the recorded revision's
// image is over the block limit but the family published a fresh revision
// — under imageUpdate=OnStart the start adopts it instead of blocking.
func TestStart_MovesToFreshRevisionInsteadOfBlocking(t *testing.T) {
	cat := newFakeTemplateLookup()
	cat.put(blockEntry("linuxdesk", "aaaa1111", "2026-10-a", "OnStart", 60))
	revB := blockEntry("linuxdesk", "bbbb2222", "2026-10-b", "OnStart", 2)
	cat.put(revB)
	_, svc, ws := blockWorkspace(t, cat, 60, "Stopped")
	svc.WithImageBlockAfter(blockTestLimit)

	res, err := svc.SignalWorkspace(context.Background(), familyTenant, "iss|sub", "", ws, "block-start-1", provisioning.IntentStart, []byte("{}"))
	if err != nil {
		t.Fatalf("start on a fresh newer revision must not block: %v", err)
	}
	if res.Template.ID != revB.ID {
		t.Fatalf("record template = %q, want the fresh revision %q", res.Template.ID, revB.ID)
	}
	if res.Template.ImageBuiltAt != revB.ImageBuiltAt {
		t.Fatalf("snapshot imageBuiltAt = %q, want the fresh revision's", res.Template.ImageBuiltAt)
	}
}

// TestStart_BlockedWhenPinnedAndStale: a workspace pinned to a stale
// revision cannot move to the fresh newest — the start is refused with
// *ImageStaleError and nothing (state, reservation, intent) is written.
func TestStart_BlockedWhenPinnedAndStale(t *testing.T) {
	cat := newFakeTemplateLookup()
	cat.put(blockEntry("linuxdesk", "aaaa1111", "2026-10-a", "Pinned", 60))
	cat.put(blockEntry("linuxdesk", "bbbb2222", "2026-10-b", "OnStart", 2))
	db, svc, ws := blockWorkspace(t, cat, 60, "Stopped")
	svc.WithImageBlockAfter(blockTestLimit)

	_, err := svc.SignalWorkspace(context.Background(), familyTenant, "iss|sub", "", ws, "block-start-2", provisioning.IntentStart, []byte("{}"))
	var stale *provisioning.ImageStaleError
	if !errors.As(err, &stale) {
		t.Fatalf("start error = %v, want *ImageStaleError", err)
	}
	if stale.AgeDays != 60 || stale.LimitDays != 45 {
		t.Fatalf("ImageStaleError = %+v, want 60 days over a 45-day limit", stale)
	}
	rec, gerr := svc.GetWorkspace(context.Background(), familyTenant, "", ws)
	if gerr != nil {
		t.Fatalf("get: %v", gerr)
	}
	if rec.DesiredState != "Stopped" || rec.Template.ID != "tpl_linuxdesk-aaaa1111" {
		t.Fatalf("blocked start changed the record: %+v", rec)
	}
	if n := len(pendingIntents(t, db, ws)); n != 1 {
		t.Fatalf("intents = %d, want only the create intent (no start recorded)", n)
	}
}

// TestStart_BlockedWhenNewestAlsoStale: even an OnStart move does not help
// when the family's newest revision is itself over the limit.
func TestStart_BlockedWhenNewestAlsoStale(t *testing.T) {
	cat := newFakeTemplateLookup()
	cat.put(blockEntry("linuxdesk", "aaaa1111", "2026-10-a", "OnStart", 60))
	cat.put(blockEntry("linuxdesk", "bbbb2222", "2026-10-b", "OnStart", 90))
	_, svc, ws := blockWorkspace(t, cat, 60, "Stopped")
	svc.WithImageBlockAfter(blockTestLimit)

	_, err := svc.SignalWorkspace(context.Background(), familyTenant, "iss|sub", "", ws, "block-start-3", provisioning.IntentStart, []byte("{}"))
	if !provisioning.IsImageStale(err) {
		t.Fatalf("start error = %v, want ImageStaleError", err)
	}
}

// TestStart_BlockedWithoutAnnotation is the E3 escape: a resolved revision
// with no image-built-at never blocks, and a malformed value does not
// either.
func TestStart_BlockedWithoutAnnotation(t *testing.T) {
	cat := newFakeTemplateLookup()
	cat.put(blockEntry("linuxdesk", "aaaa1111", "2026-10-a", "Pinned", 0))
	_, svc, ws := blockWorkspace(t, cat, 0, "Stopped")
	// The recorded revision object carries no annotation at all.
	cat.byID["tpl_linuxdesk-aaaa1111"].ImageBuiltAt = ""
	svc.WithImageBlockAfter(blockTestLimit)

	res, err := svc.SignalWorkspace(context.Background(), familyTenant, "iss|sub", "", ws, "block-start-4", provisioning.IntentStart, []byte("{}"))
	if err != nil {
		t.Fatalf("start without imageBuiltAt must not block: %v", err)
	}
	if res.DesiredState != "Running" {
		t.Fatalf("record = %+v, want Running", res)
	}
}

// TestBlockDisabled: -image-block-after=0 disables the block entirely — a
// stale pinned workspace starts, and a stale create is admitted.
func TestBlockDisabled(t *testing.T) {
	cat := newFakeTemplateLookup()
	cat.put(blockEntry("linuxdesk", "aaaa1111", "2026-10-a", "Pinned", 90))
	_, svc, ws := blockWorkspace(t, cat, 90, "Stopped")
	svc.WithImageBlockAfter(0)

	res, err := svc.SignalWorkspace(context.Background(), familyTenant, "iss|sub", "", ws, "block-start-5", provisioning.IntentStart, []byte("{}"))
	if err != nil {
		t.Fatalf("disabled block must never refuse: %v", err)
	}
	if res.DesiredState != "Running" {
		t.Fatalf("record = %+v, want Running", res)
	}

	// Same for the create path: with the block off a 90-day-old image is
	// admitted.
	if _, err := svc.CreateWorkspace(context.Background(), familyTenant, "block-seed-002", provisioning.CreateRequest{
		OwnerIssuer: "iss", OwnerSubject: "sub", Name: "block-ws-2",
		Template: provisioning.TemplateInfo{
			ID: "tpl_linuxdesk-aaaa1111", Name: "linuxdesk", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop", ImageBuiltAt: daysAgoRFC(90),
		},
		Vector: quotaVec, DesiredState: "Stopped", DataPolicy: "Ephemeral",
	}, []byte("seed2")); err != nil {
		t.Fatalf("disabled block must not refuse create: %v", err)
	}
}

// TestRunningWorkspaceUntouched: a workspace already Running on a stale
// image is never touched — a repeated start is a no-op, stop still works.
func TestRunningWorkspaceUntouched(t *testing.T) {
	cat := newFakeTemplateLookup()
	cat.put(blockEntry("linuxdesk", "aaaa1111", "2026-10-a", "Pinned", 90))
	// Block off while seeding a workspace that is already Running.
	_, svc, ws := blockWorkspace(t, cat, 90, "Running")
	svc.WithImageBlockAfter(blockTestLimit)

	res, err := svc.SignalWorkspace(context.Background(), familyTenant, "iss|sub", "", ws, "block-start-6", provisioning.IntentStart, []byte("{}"))
	if err != nil {
		t.Fatalf("start on a Running workspace is a no-op and must not block: %v", err)
	}
	if res.DesiredState != "Running" {
		t.Fatalf("record = %+v, want still Running", res)
	}
	res, err = svc.SignalWorkspace(context.Background(), familyTenant, "iss|sub", "", ws, "block-stop-6", provisioning.IntentStop, []byte("{}"))
	if err != nil {
		t.Fatalf("stop on a stale running workspace must work: %v", err)
	}
	if res.DesiredState != "Stopped" {
		t.Fatalf("record = %+v, want Stopped", res)
	}
}
