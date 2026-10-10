// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// Package runtime defines the contract between the Workspace operator and the
// runtime backends (Linux Pod, Windows KubeVirt VM). Implementations live in
// subpackages (e.g. internal/runtime/linux); this file is only the interface.
//
// No credentials ever cross this boundary: runtime auth material is delivered
// to the runtime via mounted Secrets the operator creates separately, and
// broker tickets are issued by the broker, never by the backend.
package runtime

import (
	"context"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
)

// Observation is the backend's report of the current runtime incarnation for
// a Workspace. Every field refers to the incarnation identified by
// RuntimeUID under spec.runtimeGeneration == RuntimeGeneration.
//
// A nil/zero field means "not yet observed"; the operator decides phase and
// conditions from the combination.
type Observation struct {
	// StorageReady reports all required volumes bound and attached.
	StorageReady bool

	// RuntimeReady reports the runtime itself is up (display server / RDP
	// endpoint answering inside the runtime).
	RuntimeReady bool

	// ConnectionReady reports the streaming endpoint is reachable on the
	// internal Service.
	ConnectionReady bool

	// ServiceRef is the internal Service fronting this incarnation. Nil
	// while no Service exists. Never a client-facing endpoint.
	ServiceRef *workspacesv1alpha1.ServiceReference

	// RuntimeUID is the UID of the current incarnation (Pod UID or VMI UID).
	// A new incarnation — after crash, reschedule, or recreate inside the
	// same generation — carries a new RuntimeUID, which is what fences
	// tickets/leases/activity bound to the previous one. Empty while no
	// incarnation exists.
	RuntimeUID string

	// RuntimeGeneration is the spec.runtimeGeneration this observation
	// describes. The operator drops observations for older generations.
	RuntimeGeneration int64

	// Reason is a short machine-readable explanation of the current state
	// (e.g. "Provisioning", "PodFailed", "ImagePullBackOff"), surfaced into
	// condition reasons.
	Reason string
}

// Backend is the runtime adapter the operator drives. All methods must be
// idempotent and safe to call repeatedly; the operator owns retry/backoff.
//
// Implementations take pointer objects — never deep copies — so status
// fields observed by the backend are the objects the caller passes.
type Backend interface {
	// Ensure converges the runtime for ws toward the spec captured by tpl.
	// It is called for desiredState=Running and returns the observation for
	// the incarnation it is converging.
	Ensure(ctx context.Context, ws *workspacesv1alpha1.Workspace, tpl *workspacesv1alpha1.WorkspaceTemplate) (Observation, error)

	// Observe reports the current incarnation without changing anything.
	// Called for periodic freshness and while desiredState != Running.
	Observe(ctx context.Context, ws *workspacesv1alpha1.Workspace) (Observation, error)

	// Stop terminates the current incarnation but keeps data allowed by
	// dataPolicy (home PVC, retained disk). It is not Delete.
	Stop(ctx context.Context, ws *workspacesv1alpha1.Workspace) error

	// DeleteRuntime removes the runtime and its ephemeral child objects
	// (Service, secrets, scratch). Persistent data is handled by the
	// retention flow, not here.
	DeleteRuntime(ctx context.Context, ws *workspacesv1alpha1.Workspace) error

	// PodMatchesTemplate reports whether the workspace's current runtime
	// incarnation exists, is owned by ws (controller ownerRef), and
	// provably was built from tpl — either it carries a matching
	// template-hash stamp, or (incarnations built before the stamp
	// existed) its spec equals a fresh build from tpl field-for-field.
	// It is the upgrade-adoption proof for template snapshots recorded
	// before status.templateSnapshot existed; a false answer means the
	// record may NOT be trusted and convergence must re-snapshot.
	PodMatchesTemplate(ctx context.Context, ws *workspacesv1alpha1.Workspace, tpl *workspacesv1alpha1.WorkspaceTemplate) (bool, error)

	// PodTemplateIdentity returns the template-revision identity the
	// runtime incarnation pod was stamped with — operator-written pod
	// metadata, never user input. Owned is false when no pod exists or
	// the pod is not operator-owned for ws; the caller then treats the
	// workspace as having no proving incarnation.
	PodTemplateIdentity(ctx context.Context, ws *workspacesv1alpha1.Workspace) (PodTemplateIdentity, error)

	// StampPodTemplateIdentity backfills the template-identity stamps
	// onto the workspace's operator-owned incarnation pod after its
	// provenance was proven by another means (the pre-stamp upgrade
	// path) — later reconciles and upgrades read the identity directly.
	// No-op when no owned pod exists.
	StampPodTemplateIdentity(ctx context.Context, ws *workspacesv1alpha1.Workspace, tpl *workspacesv1alpha1.WorkspaceTemplate) error
}

// PodTemplateIdentity is the stamped template-revision identity of a
// workspace's runtime incarnation pod.
type PodTemplateIdentity struct {
	// Owned is true only when the pod exists and is operator-owned for
	// the workspace (controller ownerRef to the Workspace UID plus the
	// backend's managed label set for the live generation).
	Owned bool
	// Name is the WorkspaceTemplate object name the pod was built from.
	// Empty means the pod predates the stamp — its revision cannot be
	// proven.
	Name string
	// Revision is spec.revision of the template the pod was built from.
	Revision string
}
