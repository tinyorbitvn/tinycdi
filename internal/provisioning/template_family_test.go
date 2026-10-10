// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning_test

// V3.1 (E1/E2): a Stopped -> Running start re-points a workspace at the
// newest published revision of its template family when the recorded
// revision says imageUpdate=OnStart. The row snapshot and the outbox
// intent move in one transaction; the applier writes spec.templateRef on
// the CR.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacev1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

const familyTenant = "tenant-fam"

// fakeTemplateLookup implements provisioning.TemplateLookup against
// in-memory catalog entries; a missing entry means the revision object is
// gone (nil, nil), a missing family answers ErrTemplateNotFound.
type fakeTemplateLookup struct {
	byID    map[string]*provisioning.TemplateCatalogEntry
	newest  map[string]*provisioning.TemplateCatalogEntry
	newestE error // when set, NewestInFamily fails (catalog read error)
	getErr  error // when set, Get fails
}

func newFakeTemplateLookup() *fakeTemplateLookup {
	return &fakeTemplateLookup{
		byID:   map[string]*provisioning.TemplateCatalogEntry{},
		newest: map[string]*provisioning.TemplateCatalogEntry{},
	}
}

func (f *fakeTemplateLookup) put(e provisioning.TemplateCatalogEntry) {
	e2 := e
	f.byID[e.ID] = &e2
	f.newest[e.Name] = &e2
}

func (f *fakeTemplateLookup) Get(_ context.Context, _, id string) (*provisioning.TemplateCatalogEntry, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.byID[id], nil
}

func (f *fakeTemplateLookup) NewestInFamily(_ context.Context, _, family string) (provisioning.TemplateCatalogEntry, error) {
	if f.newestE != nil {
		return provisioning.TemplateCatalogEntry{}, f.newestE
	}
	e, ok := f.newest[family]
	if !ok {
		return provisioning.TemplateCatalogEntry{}, provisioning.ErrTemplateNotFound
	}
	return *e, nil
}

// familyEntry builds a catalog entry for a revision object
// "<family>-<suffix>" of family.
func familyEntry(family, suffix, revLabel, policy string) provisioning.TemplateCatalogEntry {
	return provisioning.TemplateCatalogEntry{
		ID:                "tpl_" + family + "-" + suffix,
		Name:              family,
		RevisionLabel:     revLabel,
		Runtime:           "LinuxContainer",
		Experience:        "Desktop",
		DiskBytes:         5 << 30,
		DataPolicyDefault: "Ephemeral",
		ImageUpdate:       policy,
		ImageBuiltAt:      "2026-10-01T00:00:00Z",
	}
}

// familyWorkspace seeds a tenant quota and a Stopped workspace created from
// revision A of family linuxdesk.
func familyWorkspace(t *testing.T, cat provisioning.TemplateLookup) (*store.DB, *provisioning.Service, string) {
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
	res, err := svc.CreateWorkspace(ctx, familyTenant, "fam-seed-0001", provisioning.CreateRequest{
		OwnerIssuer: "iss", OwnerSubject: "sub", Name: "fam-ws",
		Template: provisioning.TemplateInfo{
			ID: "tpl_linuxdesk-aaaa1111", Name: "linuxdesk", Revision: 1,
			RevisionLabel: "2026-10-a", Runtime: "LinuxContainer", Experience: "Desktop",
			ImageBuiltAt: "2026-09-01T00:00:00Z",
		},
		Vector: quotaVec, DesiredState: "Stopped", DataPolicy: "Ephemeral",
	}, []byte("seed"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return db, svc, res.ID
}

func pendingIntents(t *testing.T, db *store.DB, wsID string) []provisioning.Intent {
	t.Helper()
	intents, err := provisioning.NewOutbox(db).PendingIntents(context.Background(), provisioning.PlatformID(wsID))
	if err != nil {
		t.Fatalf("pending intents: %v", err)
	}
	return intents
}

func runtimeGeneration(t *testing.T, db *store.DB, wsID string) int64 {
	t.Helper()
	var gen int64
	if err := db.Pool().QueryRow(context.Background(),
		`SELECT runtime_generation FROM workspaces WHERE id = $1`, wsID).Scan(&gen); err != nil {
		t.Fatalf("runtime_generation: %v", err)
	}
	return gen
}

// applyPending drains the workspace's outbox into a fake-client-backed
// K8sApplier and returns the resulting Workspace CR.
func applyPending(t *testing.T, db *store.DB, wsID string) *workspacev1alpha1.Workspace {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	applier := provisioning.NewK8sApplier(c, provisioning.TenantNamespaces{familyTenant: "ns-fam"})
	for _, in := range pendingIntents(t, db, wsID) {
		if err := applier.Apply(context.Background(), in); err != nil {
			t.Fatalf("apply rev %d (%s): %v", in.Revision, in.Kind, err)
		}
	}
	ws := &workspacev1alpha1.Workspace{}
	if err := c.Get(context.Background(),
		client.ObjectKey{Namespace: "ns-fam", Name: provisioning.WorkspaceCRName(provisioning.PlatformID(wsID))}, ws); err != nil {
		t.Fatalf("get CR: %v", err)
	}
	return ws
}

// TestStart_OnStartMovesToNewestRevision: a Stopped workspace on revision A
// whose family published B starts on B — the row snapshot, the outbox
// intent and the applied CR's spec.templateRef all name B, and
// runtimeGeneration advances exactly once.
func TestStart_OnStartMovesToNewestRevision(t *testing.T) {
	cat := newFakeTemplateLookup()
	revA := familyEntry("linuxdesk", "aaaa1111", "2026-10-a", "OnStart")
	revB := familyEntry("linuxdesk", "bbbb2222", "2026-10-b", "OnStart")
	revB.ImageBuiltAt = "2026-10-02T00:00:00Z"
	cat.put(revA)
	cat.put(revB)
	db, svc, ws := familyWorkspace(t, cat)
	ctx := context.Background()

	genBefore := runtimeGeneration(t, db, ws)
	res, err := svc.SignalWorkspace(ctx, familyTenant, "iss|sub", "", ws, "fam-start-01", provisioning.IntentStart, []byte("{}"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if res.Template.ID != revB.ID || res.Template.RevisionLabel != "2026-10-b" {
		t.Fatalf("record template = %+v, want revision B %q", res.Template, revB.ID)
	}
	if got := runtimeGeneration(t, db, ws); got != genBefore+1 {
		t.Fatalf("runtime_generation = %d, want %d", got, genBefore+1)
	}
	intents := pendingIntents(t, db, ws)
	start := intents[len(intents)-1]
	if start.Kind != provisioning.IntentStart || start.Spec.TemplateName != "linuxdesk-bbbb2222" {
		t.Fatalf("start intent spec = %+v, want TemplateName linuxdesk-bbbb2222", start.Spec)
	}
	cr := applyPending(t, db, ws)
	if cr.Spec.TemplateRef.Name != "linuxdesk-bbbb2222" {
		t.Fatalf("CR templateRef = %q, want linuxdesk-bbbb2222", cr.Spec.TemplateRef.Name)
	}
	if cr.Spec.RuntimeGeneration != genBefore+1 {
		t.Fatalf("CR runtimeGeneration = %d, want %d", cr.Spec.RuntimeGeneration, genBefore+1)
	}
}

// TestStart_PinnedStays: imageUpdate=Pinned on the recorded revision keeps
// templateRef on revision A; the start still runs (generation bumps once).
func TestStart_PinnedStays(t *testing.T) {
	cat := newFakeTemplateLookup()
	cat.put(familyEntry("linuxdesk", "aaaa1111", "2026-10-a", "Pinned"))
	cat.put(familyEntry("linuxdesk", "bbbb2222", "2026-10-b", "OnStart"))
	db, svc, ws := familyWorkspace(t, cat)

	genBefore := runtimeGeneration(t, db, ws)
	res, err := svc.SignalWorkspace(context.Background(), familyTenant, "iss|sub", "", ws, "fam-start-02", provisioning.IntentStart, []byte("{}"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if res.Template.ID != "tpl_linuxdesk-aaaa1111" || res.DesiredState != "Running" {
		t.Fatalf("record = %+v, want pinned revision A running", res)
	}
	if got := runtimeGeneration(t, db, ws); got != genBefore+1 {
		t.Fatalf("runtime_generation = %d, want %d", got, genBefore+1)
	}
	intents := pendingIntents(t, db, ws)
	start := intents[len(intents)-1]
	if start.Kind != provisioning.IntentStart || start.Spec.TemplateName != "" {
		t.Fatalf("pinned start must not carry a template re-point, got %+v", start.Spec)
	}
	cr := applyPending(t, db, ws)
	if cr.Spec.TemplateRef.Name != "linuxdesk" {
		t.Fatalf("CR templateRef = %q, want unchanged family ref linuxdesk", cr.Spec.TemplateRef.Name)
	}
}

// TestStart_AlreadyNewestNoChange: the workspace is already on the newest
// revision — the start bumps the generation once and writes no templateRef
// change.
func TestStart_AlreadyNewestNoChange(t *testing.T) {
	cat := newFakeTemplateLookup()
	cat.put(familyEntry("linuxdesk", "aaaa1111", "2026-10-a", "OnStart"))
	db, svc, ws := familyWorkspace(t, cat)

	genBefore := runtimeGeneration(t, db, ws)
	res, err := svc.SignalWorkspace(context.Background(), familyTenant, "iss|sub", "", ws, "fam-start-03", provisioning.IntentStart, []byte("{}"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if res.Template.ID != "tpl_linuxdesk-aaaa1111" {
		t.Fatalf("record template = %+v, want unchanged revision A", res.Template)
	}
	if got := runtimeGeneration(t, db, ws); got != genBefore+1 {
		t.Fatalf("runtime_generation = %d, want %d", got, genBefore+1)
	}
	intents := pendingIntents(t, db, ws)
	start := intents[len(intents)-1]
	if start.Spec.TemplateName != "" {
		t.Fatalf("no template move expected, intent spec = %+v", start.Spec)
	}
	if len(intents) != 2 {
		t.Fatalf("intents = %d, want exactly create+start", len(intents))
	}
}

// TestStart_ConcurrentIdempotent: two Starts with one idempotency key
// produce one revision change and one generation bump — the second is a
// stored replay.
func TestStart_ConcurrentIdempotent(t *testing.T) {
	cat := newFakeTemplateLookup()
	cat.put(familyEntry("linuxdesk", "aaaa1111", "2026-10-a", "OnStart"))
	cat.put(familyEntry("linuxdesk", "bbbb2222", "2026-10-b", "OnStart"))
	db, svc, ws := familyWorkspace(t, cat)
	ctx := context.Background()

	genBefore := runtimeGeneration(t, db, ws)
	res1, err := svc.SignalWorkspace(ctx, familyTenant, "iss|sub", "", ws, "fam-idem-key", provisioning.IntentStart, []byte("{}"))
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	res2, err := svc.SignalWorkspace(ctx, familyTenant, "iss|sub", "", ws, "fam-idem-key", provisioning.IntentStart, []byte("{}"))
	if err != nil {
		t.Fatalf("replayed start: %v", err)
	}
	if !res2.Replayed || res2.Template.ID != res1.Template.ID {
		t.Fatalf("second start not a replay of the first: %+v", res2)
	}
	if got := runtimeGeneration(t, db, ws); got != genBefore+1 {
		t.Fatalf("runtime_generation = %d, want %d (one bump for two starts)", got, genBefore+1)
	}
	if n := len(pendingIntents(t, db, ws)); n != 2 {
		t.Fatalf("intents = %d, want exactly create+start (one revision change)", n)
	}
}

// TestFamily_DeletedOldRevision: the recorded revision object was deleted
// by a chart upgrade — the start adopts the newest published revision and
// the returned record resolves the live template again.
func TestFamily_DeletedOldRevision(t *testing.T) {
	cat := newFakeTemplateLookup()
	// byID has no tpl_linuxdesk-aaaa1111: the chart upgrade deleted it.
	cat.put(familyEntry("linuxdesk", "bbbb2222", "2026-10-b", "OnStart"))
	db, svc, ws := familyWorkspace(t, cat)

	res, err := svc.SignalWorkspace(context.Background(), familyTenant, "iss|sub", "", ws, "fam-start-04", provisioning.IntentStart, []byte("{}"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if res.Template.ID != "tpl_linuxdesk-bbbb2222" || res.Template.Name != "linuxdesk" ||
		res.Template.Runtime == "" || res.Template.RevisionLabel != "2026-10-b" {
		t.Fatalf("record did not adopt the newest revision: %+v", res.Template)
	}
	cr := applyPending(t, db, ws)
	if cr.Spec.TemplateRef.Name != "linuxdesk-bbbb2222" {
		t.Fatalf("CR templateRef = %q, want linuxdesk-bbbb2222", cr.Spec.TemplateRef.Name)
	}
}

// TestStart_CatalogErrorKeepsRecorded: a catalog read failure (not a clean
// NotFound) never fails the start — the workspace starts on its recorded
// revision exactly as a pinned one would, with a WARN logged.
func TestStart_CatalogErrorKeepsRecorded(t *testing.T) {
	cat := newFakeTemplateLookup()
	cat.newestE = errors.New("apiserver unreachable")
	db, svc, ws := familyWorkspace(t, cat)

	genBefore := runtimeGeneration(t, db, ws)
	res, err := svc.SignalWorkspace(context.Background(), familyTenant, "iss|sub", "", ws, "fam-start-05", provisioning.IntentStart, []byte("{}"))
	if err != nil {
		t.Fatalf("catalog error must not fail the start: %v", err)
	}
	if res.Template.ID != "tpl_linuxdesk-aaaa1111" || res.DesiredState != "Running" {
		t.Fatalf("record = %+v, want recorded revision running", res)
	}
	if got := runtimeGeneration(t, db, ws); got != genBefore+1 {
		t.Fatalf("runtime_generation = %d, want %d", got, genBefore+1)
	}
	intents := pendingIntents(t, db, ws)
	if start := intents[len(intents)-1]; start.Spec.TemplateName != "" {
		t.Fatalf("degraded start must not carry a re-point, got %+v", start.Spec)
	}

	// Same for a failure reading the recorded revision after the family
	// resolved.
	cat2 := newFakeTemplateLookup()
	cat2.put(familyEntry("linuxdesk", "bbbb2222", "2026-10-b", "OnStart"))
	cat2.getErr = errors.New("leader election")
	db2, svc2, ws2 := familyWorkspace(t, cat2)
	res, err = svc2.SignalWorkspace(context.Background(), familyTenant, "iss|sub", "", ws2, "fam-start-06", provisioning.IntentStart, []byte("{}"))
	if err != nil {
		t.Fatalf("recorded-revision lookup error must not fail the start: %v", err)
	}
	if res.Template.ID != "tpl_linuxdesk-aaaa1111" {
		t.Fatalf("record = %+v, want recorded revision", res)
	}
	intents = pendingIntents(t, db2, ws2)
	if start := intents[len(intents)-1]; start.Spec.TemplateName != "" {
		t.Fatalf("degraded start must not carry a re-point, got %+v", start.Spec)
	}
}

// intentReason reads the recorded reason of the workspace's newest intent
// — the cause the events endpoint curates (TemplateUpdateSkipped for a
// guard-refused family re-point).
func intentReason(t *testing.T, svc *provisioning.Service, wsID string) (string, provisioning.IntentKind) {
	t.Helper()
	hist, err := svc.IntentHistory(context.Background(), familyTenant, provisioning.PlatformID(wsID))
	if err != nil || len(hist) == 0 {
		t.Fatalf("intent history: %v (n=%d)", err, len(hist))
	}
	return hist[0].Reason, hist[0].Kind
}

// TestUpdate_SkippedWhenStorageSmaller (V3.2/E2): the newest family
// revision asks for a smaller disk — the start keeps the recorded
// revision (row, intent and CR templateRef all unchanged) and records the
// storage-smaller reason on the start intent for the curated
// TemplateUpdateSkipped event.
func TestUpdate_SkippedWhenStorageSmaller(t *testing.T) {
	cat := newFakeTemplateLookup()
	revA := familyEntry("linuxdesk", "aaaa1111", "2026-10-a", "OnStart")
	revB := familyEntry("linuxdesk", "bbbb2222", "2026-10-b", "OnStart")
	revB.DiskBytes = revA.DiskBytes - (1 << 30)
	cat.put(revA)
	cat.put(revB)
	db, svc, ws := familyWorkspace(t, cat)

	res, err := svc.SignalWorkspace(context.Background(), familyTenant, "iss|sub", "", ws, "fam-skip-01", provisioning.IntentStart, []byte("{}"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if res.Template.ID != revA.ID || res.DesiredState != "Running" {
		t.Fatalf("record = %+v, want recorded revision A running", res)
	}
	intents := pendingIntents(t, db, ws)
	start := intents[len(intents)-1]
	if start.Kind != provisioning.IntentStart || start.Spec.TemplateName != "" {
		t.Fatalf("skipped start must not carry a re-point, got %+v", start.Spec)
	}
	reason, kind := intentReason(t, svc, ws)
	if kind != provisioning.IntentStart || reason != provisioning.SkipReasonStorageSmaller {
		t.Fatalf("newest intent = %s reason %q, want start/%s",
			kind, reason, provisioning.SkipReasonStorageSmaller)
	}
	cr := applyPending(t, db, ws)
	if cr.Spec.TemplateRef.Name != "linuxdesk" {
		t.Fatalf("CR templateRef = %q, want unchanged linuxdesk", cr.Spec.TemplateRef.Name)
	}
}

// TestUpdate_SkippedWhenDataPolicyDiffers (V3.2/E2): the newest revision
// declares a different data policy — the start keeps the recorded revision
// and records data-policy-changed on the start intent.
func TestUpdate_SkippedWhenDataPolicyDiffers(t *testing.T) {
	cat := newFakeTemplateLookup()
	revA := familyEntry("linuxdesk", "aaaa1111", "2026-10-a", "OnStart")
	revB := familyEntry("linuxdesk", "bbbb2222", "2026-10-b", "OnStart")
	revB.DataPolicyDefault = "Retain"
	cat.put(revA)
	cat.put(revB)
	db, svc, ws := familyWorkspace(t, cat)

	res, err := svc.SignalWorkspace(context.Background(), familyTenant, "iss|sub", "", ws, "fam-skip-02", provisioning.IntentStart, []byte("{}"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if res.Template.ID != revA.ID || res.DesiredState != "Running" {
		t.Fatalf("record = %+v, want recorded revision A running", res)
	}
	reason, kind := intentReason(t, svc, ws)
	if kind != provisioning.IntentStart || reason != provisioning.SkipReasonDataPolicyChanged {
		t.Fatalf("newest intent = %s reason %q, want start/%s",
			kind, reason, provisioning.SkipReasonDataPolicyChanged)
	}
	cr := applyPending(t, db, ws)
	if cr.Spec.TemplateRef.Name != "linuxdesk" {
		t.Fatalf("CR templateRef = %q, want unchanged linuxdesk", cr.Spec.TemplateRef.Name)
	}
}

// TestUpdate_SkippedReasonMatrix (V3.2/E2): each guard dimension reports
// its own recorded reason; a compatible move records none.
func TestUpdate_SkippedReasonMatrix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(e *provisioning.TemplateCatalogEntry)
		want   string
	}{
		{"runtime-changed", func(e *provisioning.TemplateCatalogEntry) { e.Runtime = "WindowsVM" }, provisioning.SkipReasonRuntimeChanged},
		{"experience-changed", func(e *provisioning.TemplateCatalogEntry) { e.Experience = "Browser" }, provisioning.SkipReasonExperienceChanged},
		{"data-policy-changed", func(e *provisioning.TemplateCatalogEntry) { e.DataPolicyDefault = "Retain" }, provisioning.SkipReasonDataPolicyChanged},
		{"storage-smaller", func(e *provisioning.TemplateCatalogEntry) { e.DiskBytes -= 1 << 30 }, provisioning.SkipReasonStorageSmaller},
		{"network-profile-changed", func(e *provisioning.TemplateCatalogEntry) { e.NetworkProfile = "InternetOnly" }, provisioning.SkipReasonNetworkProfileChanged},
		{"clipboard-policy-changed", func(e *provisioning.TemplateCatalogEntry) { e.ClipboardPolicy = "Bidirectional" }, provisioning.SkipReasonClipboardPolicyChanged},
		{"adapter-changed", func(e *provisioning.TemplateCatalogEntry) { e.Adapter = "kasm" }, provisioning.SkipReasonAdapterChanged},
		{"host-users-changed", func(e *provisioning.TemplateCatalogEntry) {
			hu := false
			e.HostUsers = &hu
		}, provisioning.SkipReasonHostUsersChanged},
		{"placement-changed", func(e *provisioning.TemplateCatalogEntry) {
			e.Placement = &workspacev1alpha1.PlacementSpec{NodeSelector: map[string]string{"workload": "runtime"}}
		}, provisioning.SkipReasonPlacementChanged},
		{"seccomp-profile-changed", func(e *provisioning.TemplateCatalogEntry) {
			e.SeccompProfile = "localhost/profiles/chromium-userns.json"
		}, provisioning.SkipReasonSeccompProfileChanged},
		{"apparmor-profile-changed", func(e *provisioning.TemplateCatalogEntry) {
			e.AppArmorProfile = "localhost/tinycdi-browser"
		}, provisioning.SkipReasonAppArmorProfileChanged},
		{"compatible", func(e *provisioning.TemplateCatalogEntry) { e.DiskBytes += 1 << 30 }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat := newFakeTemplateLookup()
			revA := familyEntry("linuxdesk", "aaaa1111", "2026-10-a", "OnStart")
			revB := familyEntry("linuxdesk", "bbbb2222", "2026-10-b", "OnStart")
			tc.mutate(&revB)
			cat.put(revA)
			cat.put(revB)
			_, svc, ws := familyWorkspace(t, cat)

			res, err := svc.SignalWorkspace(context.Background(), familyTenant, "iss|sub", "", ws, "fam-skip-mx", provisioning.IntentStart, []byte("{}"))
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			reason, _ := intentReason(t, svc, ws)
			if reason != tc.want {
				t.Fatalf("recorded reason = %q, want %q", reason, tc.want)
			}
			if tc.want == "" && res.Template.ID != revB.ID {
				t.Fatalf("compatible start must move to %s, record = %+v", revB.ID, res.Template)
			}
			if tc.want != "" && res.Template.ID != revA.ID {
				t.Fatalf("skipped start must stay on %s, record = %+v", revA.ID, res.Template)
			}
		})
	}
}

// famRevision builds a published WorkspaceTemplate revision object of
// family in ns-fam — the shape templateEntry reads every E2 guard
// dimension from.
func famRevision(family, suffix, revLabel string, created time.Time,
	mutate func(*workspacev1alpha1.WorkspaceTemplate)) *workspacev1alpha1.WorkspaceTemplate {
	tpl := &workspacev1alpha1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:              family + "-" + suffix,
			Namespace:         "ns-fam",
			Labels:            map[string]string{provisioning.LabelCatalogName: family},
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: workspacev1alpha1.WorkspaceTemplateSpec{
			Revision:   revLabel,
			Runtime:    workspacev1alpha1.RuntimeLinuxContainer,
			Experience: workspacev1alpha1.ExperienceDesktop,
			Linux: &workspacev1alpha1.LinuxRuntimeSpec{
				Image: "registry.example/tcdi/linux-desktop@sha256:" +
					"0000000000000000000000000000000000000000000000000000000000000000",
			},
			ClipboardPolicy: workspacev1alpha1.ClipboardDisabled,
			NetworkProfile:  workspacev1alpha1.NetworkProfileIsolated,
			Lifecycle: workspacev1alpha1.LifecycleDefaults{
				ImageUpdate: workspacev1alpha1.ImageUpdateOnStart,
			},
		},
	}
	if mutate != nil {
		mutate(tpl)
	}
	return tpl
}

// TestUpdate_SkippedOnSecurityFieldDrift (v1.0/SR-3-F6): the E2 guard
// covers the security-relevant revision fields end to end — read off the
// real WorkspaceTemplate objects by the catalog, not a hand-built entry.
// A published successor that changes the egress boundary, clipboard
// policy, runtime adapter, hostUsers, placement or the confinement
// annotations is never adopted silently: the start stays on the recorded
// revision and the skip reason names the field; a revision that only
// freshens the image/revision label still flows.
func TestUpdate_SkippedOnSecurityFieldDrift(t *testing.T) {
	hostUsers := false
	base := time.Now().Add(-2 * time.Hour)
	for i, tc := range []struct {
		name   string
		mutate func(*workspacev1alpha1.WorkspaceTemplate)
		want   string
	}{
		{"image-only", nil, ""},
		{"network-profile", func(tpl *workspacev1alpha1.WorkspaceTemplate) {
			tpl.Spec.NetworkProfile = workspacev1alpha1.NetworkProfileInternetOnly
		}, provisioning.SkipReasonNetworkProfileChanged},
		{"clipboard-policy", func(tpl *workspacev1alpha1.WorkspaceTemplate) {
			tpl.Spec.ClipboardPolicy = workspacev1alpha1.ClipboardBidirectional
		}, provisioning.SkipReasonClipboardPolicyChanged},
		{"adapter", func(tpl *workspacev1alpha1.WorkspaceTemplate) {
			tpl.Spec.Linux.Adapter = workspacev1alpha1.AdapterKasm
		}, provisioning.SkipReasonAdapterChanged},
		{"host-users", func(tpl *workspacev1alpha1.WorkspaceTemplate) {
			tpl.Spec.Linux.HostUsers = &hostUsers
		}, provisioning.SkipReasonHostUsersChanged},
		{"placement", func(tpl *workspacev1alpha1.WorkspaceTemplate) {
			tpl.Spec.Placement = &workspacev1alpha1.PlacementSpec{
				NodeSelector: map[string]string{"workload": "runtime"},
			}
		}, provisioning.SkipReasonPlacementChanged},
		{"seccomp-profile", func(tpl *workspacev1alpha1.WorkspaceTemplate) {
			tpl.Annotations = map[string]string{
				linux.AnnotationSeccompProfile: "localhost/profiles/chromium-userns.json",
			}
		}, provisioning.SkipReasonSeccompProfileChanged},
		{"apparmor-profile", func(tpl *workspacev1alpha1.WorkspaceTemplate) {
			tpl.Annotations = map[string]string{
				linux.AnnotationAppArmorProfile: "localhost/tinycdi-browser",
			}
		}, provisioning.SkipReasonAppArmorProfileChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			revA := famRevision("linuxdesk", "aaaa1111", "2026-10-a", base, nil)
			revB := famRevision("linuxdesk", "bbbb2222", "2026-10-b", base.Add(time.Hour), tc.mutate)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(revA, revB).Build()
			cat := provisioning.NewK8sTemplateCatalog(c,
				provisioning.TenantNamespaces{familyTenant: "ns-fam"})
			_, svc, ws := familyWorkspace(t, cat)

			res, err := svc.SignalWorkspace(context.Background(), familyTenant, "iss|sub", "",
				ws, fmt.Sprintf("fam-sec-%d", i), provisioning.IntentStart, []byte("{}"))
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			reason, _ := intentReason(t, svc, ws)
			if reason != tc.want {
				t.Fatalf("recorded reason = %q, want %q", reason, tc.want)
			}
			if tc.want == "" && res.Template.ID != "tpl_linuxdesk-bbbb2222" {
				t.Fatalf("compatible start must move to revision B, record = %+v", res.Template)
			}
			if tc.want != "" && res.Template.ID != "tpl_linuxdesk-aaaa1111" {
				t.Fatalf("skipped start must stay on revision A, record = %+v", res.Template)
			}
		})
	}
}

// TestCatalogNewestInFamily: NewestInFamily resolves the newest published
// revision of a catalog-name family and singletons by object name;
// unknown families answer ErrTemplateNotFound.
func TestCatalogNewestInFamily(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	old := catalogTmpl("linuxdesk1-aaaa0000", "linuxdesk1", "w6a-1", t0)
	cur := catalogTmpl("linuxdesk1-bbbb1111", "linuxdesk1", "w6a-2", t0.Add(30*time.Minute))
	cur.Spec.Lifecycle.ImageUpdate = workspacev1alpha1.ImageUpdatePinned
	solo := catalogTmpl("admintpl", "", "1", t0)

	scheme := runtime.NewScheme()
	if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(old, cur, solo).Build()
	cat := provisioning.NewK8sTemplateCatalog(c, provisioning.TenantNamespaces{"tenant-a": "ns-a"})
	ctx := context.Background()

	e, err := cat.NewestInFamily(ctx, "tenant-a", "linuxdesk1")
	if err != nil {
		t.Fatalf("newest in family: %v", err)
	}
	if e.ID != "tpl_linuxdesk1-bbbb1111" || e.RevisionLabel != "w6a-2" ||
		e.ImageUpdate != string(workspacev1alpha1.ImageUpdatePinned) {
		t.Fatalf("newest = %+v, want revision bbbb1111 pinned", e)
	}
	e, err = cat.NewestInFamily(ctx, "tenant-a", "admintpl")
	if err != nil || e.ID != "tpl_admintpl" {
		t.Fatalf("singleton family: %v/%v", e, err)
	}
	if _, err = cat.NewestInFamily(ctx, "tenant-a", "gone"); err == nil {
		t.Fatal("unknown family must answer ErrTemplateNotFound")
	}
}

// TestK8sApplierStartRepointsTemplateRef: a start intent carrying a
// template name re-points spec.templateRef while the CR is Stopped and
// refreshes the image-built-at annotation.
func TestK8sApplierStartRepointsTemplateRef(t *testing.T) {
	c, applier := newFakeK8sApplier(t)
	ctx := context.Background()
	uid := provisioning.PlatformID("ws_aaaabbbbccccdddd0000111122223333")
	in := createIntent(uid, "req-1", 1)
	in.DesiredState = "Stopped"
	in.RuntimeGeneration = 0
	in.Spec.ImageBuiltAt = "2026-09-01T00:00:00Z"
	if err := applier.Apply(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := applier.Apply(ctx, provisioning.Intent{
		WorkspaceUID: uid, TenantID: "tenant-a", Revision: 2,
		Kind: provisioning.IntentStart, DesiredState: "Running", RuntimeGeneration: 1,
		Spec: provisioning.IntentSpec{TemplateName: "tmpl-linux-2", ImageBuiltAt: "2026-10-02T00:00:00Z"},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	ws := getWorkspace(t, c, "ns-a", provisioning.WorkspaceCRName(uid))
	if ws.Spec.TemplateRef.Name != "tmpl-linux-2" {
		t.Fatalf("templateRef = %q, want tmpl-linux-2", ws.Spec.TemplateRef.Name)
	}
	if ws.Annotations[provisioning.AnnotationWorkspaceImageBuiltAt] != "2026-10-02T00:00:00Z" {
		t.Fatalf("image-built-at annotation = %q", ws.Annotations[provisioning.AnnotationWorkspaceImageBuiltAt])
	}
}

// TestK8sApplierRepointRefusedWhileRunning: a start intent carrying a
// re-point that finds the CR wanted Running is refused with the typed
// error — the CR keeps every field and a replayed delivery refuses the
// same way, so the intent stays undispatched for the recovery/quarantine
// path instead of silently diverging.
func TestK8sApplierRepointRefusedWhileRunning(t *testing.T) {
	c, applier := newFakeK8sApplier(t)
	ctx := context.Background()
	uid := provisioning.PlatformID("ws_aaaabbbbccccdddd0000111122223333")
	// The CR is wanted Running out of band (e.g. an admin write).
	in := createIntent(uid, "req-1", 1)
	if err := applier.Apply(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}
	repoint := provisioning.Intent{
		WorkspaceUID: uid, TenantID: "tenant-a", Revision: 2,
		Kind: provisioning.IntentStart, DesiredState: "Running", RuntimeGeneration: 2,
		Spec: provisioning.IntentSpec{TemplateName: "tmpl-linux-3"},
	}
	if err := applier.Apply(ctx, repoint); !errors.Is(err, provisioning.ErrTemplateRepointBlocked) {
		t.Fatalf("apply on Running CR = %v, want ErrTemplateRepointBlocked", err)
	}
	ws := getWorkspace(t, c, "ns-a", provisioning.WorkspaceCRName(uid))
	if ws.Spec.TemplateRef.Name != "tmpl-linux" || ws.Spec.IntentRevision != 1 {
		t.Fatalf("refused apply changed the CR: %+v", ws.Spec)
	}
	// Replay (the intent was never marked dispatched): refuses identically,
	// still no CR write.
	if err := applier.Apply(ctx, repoint); !errors.Is(err, provisioning.ErrTemplateRepointBlocked) {
		t.Fatalf("replayed apply = %v, want ErrTemplateRepointBlocked", err)
	}
	ws = getWorkspace(t, c, "ns-a", provisioning.WorkspaceCRName(uid))
	if ws.Spec.TemplateRef.Name != "tmpl-linux" || ws.Spec.IntentRevision != 1 {
		t.Fatalf("replayed apply changed the CR: %+v", ws.Spec)
	}
}
