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
}
