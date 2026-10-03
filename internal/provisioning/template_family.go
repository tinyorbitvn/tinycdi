// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning

import (
	"context"
	"errors"
	"log/slog"

	workspacev1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
)

// TemplateLookup is the catalog read surface the start path consults to
// re-point a workspace onto the newest published revision of its template
// family (E1). *K8sTemplateCatalog implements it.
type TemplateLookup interface {
	// Get resolves one public template ID ("tpl_<name>"); nil, nil when
	// the revision object is gone.
	Get(ctx context.Context, tenantID, id string) (*TemplateCatalogEntry, error)
	// NewestInFamily resolves the newest published revision of a template
	// family (catalog name); ErrTemplateNotFound when the family has no
	// live revision.
	NewestInFamily(ctx context.Context, tenantID, family string) (TemplateCatalogEntry, error)
}

// TemplateUpdateSkipped reasons recorded on a start intent whose family
// re-point was refused by the E2 compatibility guard. The events endpoint
// curates them into the TemplateUpdateSkipped event; the values are part
// of the event contract (one of these tokens, verbatim).
const (
	SkipReasonRuntimeChanged    = "runtime-changed"
	SkipReasonExperienceChanged = "experience-changed"
	SkipReasonDataPolicyChanged = "data-policy-changed"
	SkipReasonStorageSmaller    = "storage-smaller"
)

// startTemplateTarget decides whether a Stopped -> Running start moves the
// workspace to a newer published revision of its template family. It
// returns the row's new TemplateInfo snapshot and the revision object name
// spec.templateRef must point at, or nil when the workspace keeps its
// recorded revision — because the family no longer resolves, the workspace
// is already on the newest revision, the recorded revision pins the image,
// or the newest revision fails the compatibility guard (E2). A guard-
// refused move also returns the SkipReason* token the caller records on
// the start intent so the workspace events can name it; every other
// keep-recorded outcome returns an empty reason.
//
// The imageUpdate policy is read from the workspace's recorded revision;
// an unreadable revision (deleted by a chart upgrade, or a stale row)
// falls back to the OnStart default so a workspace whose template object
// vanished adopts the newest published revision instead of wedging.
//
// A catalog read failure (not a clean NotFound) never fails the user's
// start: it logs a warning and keeps the recorded revision for that start
// — the same outcome as a pinned template — because a template lookup is
// advisory, not part of the lifecycle contract the intent was admitted
// under.
func startTemplateTarget(ctx context.Context, cat TemplateLookup, tenantID string, rec *WorkspaceRecord, log *slog.Logger) (*TemplateInfo, string, string, error) {
	if log == nil {
		log = slog.Default()
	}
	family := rec.Template.Name
	if family == "" {
		return nil, "", "", nil
	}
	newest, err := cat.NewestInFamily(ctx, tenantID, family)
	if err != nil {
		if errors.Is(err, ErrTemplateNotFound) {
			return nil, "", "", nil
		}
		log.Warn("template family lookup failed; starting on recorded revision",
			"workspace", rec.ID, "family", family, "error", err)
		return nil, "", "", nil
	}
	if newest.ID == "" || newest.ID == rec.Template.ID {
		return nil, "", "", nil // already on the newest published revision
	}
	cur, err := cat.Get(ctx, tenantID, rec.Template.ID)
	if err != nil {
		log.Warn("recorded template revision lookup failed; starting on recorded revision",
			"workspace", rec.ID, "template", rec.Template.ID, "error", err)
		return nil, "", "", nil
	}
	if cur != nil {
		if workspacev1alpha1.ImageUpdatePolicy(cur.ImageUpdate) == workspacev1alpha1.ImageUpdatePinned {
			return nil, "", "", nil
		}
		if reason := revisionIncompatible(cur, &newest); reason != "" {
			log.Info("template update skipped by compatibility guard",
				"workspace", rec.ID, "family", family,
				"from", rec.Template.ID, "to", newest.ID, "reason", reason)
			return nil, "", reason, nil
		}
	}
	name, ok := TemplateCRName(newest.ID)
	if !ok {
		return nil, "", "", nil
	}
	return &TemplateInfo{
		ID:            newest.ID,
		Name:          newest.Name,
		Revision:      newest.Revision,
		RevisionLabel: newest.RevisionLabel,
		Runtime:       newest.Runtime,
		Experience:    newest.Experience,
		ImageBuiltAt:  newest.ImageBuiltAt,
	}, name, "", nil
}

// revisionIncompatible is the E2 guard: a start may move a workspace only
// to a revision with the same runtime, experience and data policy, whose
// storage is not smaller than the recorded revision's. It returns "" when
// compatible, else the SkipReason* token naming the first failing
// dimension — the curated TemplateUpdateSkipped event's cause.
func revisionIncompatible(cur, next *TemplateCatalogEntry) string {
	switch {
	case cur.Runtime != next.Runtime:
		return SkipReasonRuntimeChanged
	case cur.Experience != next.Experience:
		return SkipReasonExperienceChanged
	case cur.DataPolicyDefault != next.DataPolicyDefault:
		return SkipReasonDataPolicyChanged
	case next.DiskBytes < cur.DiskBytes:
		return SkipReasonStorageSmaller
	}
	return ""
}
