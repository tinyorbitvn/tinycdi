// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TemplateReference identifies the WorkspaceTemplate a Workspace was created
// from. Namespaced: a Workspace can only reference a template in its own
// namespace.
type TemplateReference struct {
	// name of the WorkspaceTemplate in the same namespace.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// OwnerSubject pins the OIDC identity that owns this Workspace. Issuer+subject
// are the immutable identity pair; no display name, email, or credential is
// stored here.
type OwnerSubject struct {
	// issuer is the OIDC issuer URL (the `iss` claim).
	// +required
	// +kubebuilder:validation:MinLength=1
	Issuer string `json:"issuer"`

	// subject is the OIDC subject (the `sub` claim).
	// +required
	// +kubebuilder:validation:MinLength=1
	Subject string `json:"subject"`
}

// ServiceReference is the operator-populated internal Service that fronts the
// running workspace runtime. It is internal-only: end users never connect to
// it directly; all traffic goes through the broker/gateway.
type ServiceReference struct {
	// name of the Service in the workspace's namespace.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// port on the Service that serves the streaming endpoint.
	// +optional
	Port int32 `json:"port,omitempty"`
}

// DataReference records a data volume the workspace uses or retains. Persistent
// volumes never carry an ownerReference to the Workspace; these refs are the
// platform's bookkeeping for retention and purge.
type DataReference struct {
	// role of the referenced volume.
	// +required
	Role DataRole `json:"role"`

	// name of the referenced object (e.g. PersistentVolumeClaim name).
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// WorkspaceSpec is managed exclusively by the platform API and operator
// service accounts. End users never write a Workspace CR directly; they act
// through the public API, which writes spec fields inside quota/outbox
// transactions. The CRD-level rules below are the last line of defense for
// when cluster admins use kubectl.
type WorkspaceSpec struct {
	// templateRef is the WorkspaceTemplate this workspace runs from. It is
	// fixed while the runtime is wanted Running; the platform API may
	// re-point it to a newer revision of the same template family only
	// while desiredState is Stopped (the CEL rule below).
	// +required
	TemplateRef TemplateReference `json:"templateRef"`

	// ownerSubject is the immutable OIDC identity of the workspace owner.
	// +required
	OwnerSubject OwnerSubject `json:"ownerSubject"`

	// desiredState is the user/platform intent: Running or Stopped.
	// +required
	DesiredState DesiredState `json:"desiredState"`

	// dataPolicy selects whether workspace data is destroyed (Ephemeral) or
	// moved to the retained inventory (Retain) when the workspace is deleted.
	// +required
	DataPolicy DataPolicy `json:"dataPolicy"`

	// runtimeGeneration is a monotonically increasing number scoped to this
	// workspace's UID. The API assigns a new value (in spec, inside the same
	// outbox transaction as the intent record) every time an intent is
	// accepted that moves the runtime to Running: create-with-Running or
	// start-from-Stopped. Stop/Delete fence the current generation; the next
	// start is issued a new one. It may be 0 only while the workspace has
	// never been asked to run.
	// +required
	// +kubebuilder:validation:Minimum=0
	RuntimeGeneration int64 `json:"runtimeGeneration"`

	// intentRevision serializes every lifecycle intent (create, start, stop,
	// delete, attach, purge) of this workspace. The API increments it in the
	// same transaction as the workspace record. The operator drops any
	// observed intentRevision <= status.lastAppliedIntentRevision.
	// +required
	// +kubebuilder:validation:Minimum=1
	IntentRevision int64 `json:"intentRevision"`
}

// WorkspaceStatus is operator-populated. Phase is only a summary; conditions
// and reasons are the operational truth.
type WorkspaceStatus struct {
	// observedGeneration is the latest metadata.generation the operator has
	// reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// observedRuntimeGeneration is the spec.runtimeGeneration the reported
	// runtime incarnation belongs to.
	// +optional
	ObservedRuntimeGeneration int64 `json:"observedRuntimeGeneration,omitempty"`

	// runtimeUID is the UID of the current runtime incarnation (Pod for
	// LinuxContainer, VMI for WindowsVM) belonging to
	// observedRuntimeGeneration. A generation may contain several
	// incarnations; a crash/reschedule produces a new runtimeUID which
	// automatically fences tickets, leases and activity bound to the old one.
	// +optional
	RuntimeUID string `json:"runtimeUID,omitempty"`

	// lastAppliedIntentRevision is the highest spec.intentRevision the
	// operator has applied. Lower revisions are ignored.
	// +optional
	LastAppliedIntentRevision int64 `json:"lastAppliedIntentRevision,omitempty"`

	// phase summarizes the lifecycle position. Condition reasons carry the
	// actionable detail.
	// +optional
	Phase WorkspacePhase `json:"phase,omitempty"`

	// conditions represent the current state of the Workspace resource.
	// Condition types: Admitted, StorageReady, RuntimeReady, ConnectionReady,
	// Degraded.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// serviceRef is the internal Service fronting the runtime, set when the
	// runtime exists. Not a connect endpoint for clients.
	// +optional
	ServiceRef *ServiceReference `json:"serviceRef,omitempty"`

	// dataRefs lists the volumes belonging to this workspace (home PVC,
	// retained disk attachment, Windows boot disk).
	// +listType=map
	// +listMapKey=role
	// +optional
	DataRefs []DataReference `json:"dataRefs,omitempty"`

	// startedAt is when the current runtime incarnation became Ready. It
	// belongs to that incarnation alone: the maxDuration cap is measured from
	// it, it is cleared once the incarnation ends (Stopping, Stopped or
	// Failed) and set again when the next start reaches Ready. It is not the
	// workspace's first start.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// stoppedAt is when the runtime last reached Stopped.
	// +optional
	StoppedAt *metav1.Time `json:"stoppedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Desired",type=string,JSONPath=".spec.desiredState"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="RuntimeGen",type=integer,JSONPath=".spec.runtimeGeneration"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Workspace is the Schema for the workspaces API.
type Workspace struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Workspace
	// +required
	// +kubebuilder:validation:XValidation:rule="self.templateRef == oldSelf.templateRef || oldSelf.desiredState == 'Stopped'",message="spec.templateRef may change only while desiredState is Stopped"
	// +kubebuilder:validation:XValidation:rule="self.ownerSubject == oldSelf.ownerSubject",message="spec.ownerSubject is immutable"
	// +kubebuilder:validation:XValidation:rule="self.dataPolicy == oldSelf.dataPolicy",message="spec.dataPolicy is immutable"
	// +kubebuilder:validation:XValidation:rule="self.runtimeGeneration >= oldSelf.runtimeGeneration",message="spec.runtimeGeneration must not decrease"
	// +kubebuilder:validation:XValidation:rule="self.intentRevision >= oldSelf.intentRevision",message="spec.intentRevision must not decrease"
	// +kubebuilder:validation:XValidation:rule="self.desiredState != 'Running' || self.runtimeGeneration >= 1",message="spec.runtimeGeneration must be >= 1 when spec.desiredState is Running"
	Spec WorkspaceSpec `json:"spec"`

	// status defines the observed state of Workspace
	// +optional
	Status WorkspaceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// WorkspaceList contains a list of Workspace.
type WorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Workspace `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Workspace{}, &WorkspaceList{})
		return nil
	})
}
