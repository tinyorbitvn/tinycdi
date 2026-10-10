// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning

import (
	"context"
	"errors"
	"log/slog"
	"reflect"

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
	// The security-relevant dimensions: a newer family revision may
	// freshen the image/version but must never silently move the
	// workspace's exposure boundary — the egress profile, clipboard
	// redirection, the runtime adapter, the user-namespace contract,
	// pod placement, or the confinement profiles.
	SkipReasonNetworkProfileChanged  = "network-profile-changed"
	SkipReasonClipboardPolicyChanged = "clipboard-policy-changed"
	SkipReasonAdapterChanged         = "adapter-changed"
	SkipReasonHostUsersChanged       = "host-users-changed"
	SkipReasonPlacementChanged       = "placement-changed"
	SkipReasonSeccompProfileChanged  = "seccomp-profile-changed"
	SkipReasonAppArmorProfileChanged = "apparmor-profile-changed"
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
// resolvedBuiltAt is the image-built-at value of the revision the start
// would actually run on — the newest revision's when the start moves or
// the workspace already sits on it, else the recorded revision's live
// annotation (falling back to the create-time snapshot when the revision
// object is unreadable). The E3 stale-image block judges that value.
//
// The bool return marks a workspace that can never move to a fresher
// revision — imageUpdate Pinned or a move the compatibility guard refused
// — so the stale-image refusal can say an administrator must publish a
// fresh image.
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
func startTemplateTarget(ctx context.Context, cat TemplateLookup, tenantID string, rec *WorkspaceRecord, log *slog.Logger) (*TemplateInfo, string, string, string, bool, error) {
	if log == nil {
		log = slog.Default()
	}
	family := rec.Template.Name
	if family == "" {
		return nil, "", "", rec.Template.ImageBuiltAt, false, nil
	}
	newest, err := cat.NewestInFamily(ctx, tenantID, family)
	if err != nil {
		if !errors.Is(err, ErrTemplateNotFound) {
			log.Warn("template family lookup failed; starting on recorded revision",
				"workspace", rec.ID, "family", family, "error", err)
		}
		return nil, "", "", rec.Template.ImageBuiltAt, false, nil
	}
	if newest.ID == "" {
		return nil, "", "", rec.Template.ImageBuiltAt, false, nil
	}
	if newest.ID == rec.Template.ID {
		// Already on the newest published revision; the resolved revision
		// is the live object, not the create-time snapshot.
		return nil, "", "", newest.ImageBuiltAt, false, nil
	}
	cur, err := cat.Get(ctx, tenantID, rec.Template.ID)
	if err != nil {
		log.Warn("recorded template revision lookup failed; starting on recorded revision",
			"workspace", rec.ID, "template", rec.Template.ID, "error", err)
		return nil, "", "", rec.Template.ImageBuiltAt, false, nil
	}
	if cur != nil {
		if workspacev1alpha1.ImageUpdatePolicy(cur.ImageUpdate) == workspacev1alpha1.ImageUpdatePinned {
			// Pinned: the workspace can never move — the stale-image
			// refusal must say an admin must publish a fresh image.
			return nil, "", "", cur.ImageBuiltAt, true, nil
		}
		if reason := revisionIncompatible(cur, &newest); reason != "" {
			log.Info("template update skipped by compatibility guard",
				"workspace", rec.ID, "family", family,
				"from", rec.Template.ID, "to", newest.ID, "reason", reason)
			// Guard-refused: the workspace can never move either.
			return nil, "", reason, cur.ImageBuiltAt, true, nil
		}
	}
	name, ok := TemplateCRName(newest.ID)
	if !ok {
		builtAt := rec.Template.ImageBuiltAt
		if cur != nil {
			builtAt = cur.ImageBuiltAt
		}
		return nil, "", "", builtAt, false, nil
	}
	return &TemplateInfo{
		ID:            newest.ID,
		Name:          newest.Name,
		Revision:      newest.Revision,
		RevisionLabel: newest.RevisionLabel,
		Runtime:       newest.Runtime,
		Experience:    newest.Experience,
		ImageBuiltAt:  newest.ImageBuiltAt,
	}, name, "", newest.ImageBuiltAt, false, nil
}

// revisionIncompatible is the E2 guard: a start may move a workspace only
// to a revision with the same runtime, experience, data policy and
// security-relevant settings, whose storage is not smaller than the
// recorded revision's. It returns "" when compatible, else the
// SkipReason* token naming the first failing dimension — the curated
// TemplateUpdateSkipped event's cause.
//
// Every security-relevant dimension is an exact-match gate — any change
// is incompatible, even one that looks like a tightening
// (InternetOnly→Isolated): an admin publishing a new revision expresses
// intent for NEW workspaces; silently altering an existing workspace's
// exposure boundary — in either direction — is never what a start was
// admitted under. The egress profile and clipboard policy govern the
// data boundary, adapter/hostUsers the sandbox contract, placement where
// the pod lands, and the profile annotations which node-loaded
// confinement the container runs under.
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
	case cur.NetworkProfile != next.NetworkProfile:
		return SkipReasonNetworkProfileChanged
	case cur.ClipboardPolicy != next.ClipboardPolicy:
		return SkipReasonClipboardPolicyChanged
	case cur.Adapter != next.Adapter:
		return SkipReasonAdapterChanged
	case !reflect.DeepEqual(cur.HostUsers, next.HostUsers):
		return SkipReasonHostUsersChanged
	case !reflect.DeepEqual(cur.Placement, next.Placement):
		return SkipReasonPlacementChanged
	case cur.SeccompProfile != next.SeccompProfile:
		return SkipReasonSeccompProfileChanged
	case cur.AppArmorProfile != next.AppArmorProfile:
		return SkipReasonAppArmorProfileChanged
	}
	return ""
}
