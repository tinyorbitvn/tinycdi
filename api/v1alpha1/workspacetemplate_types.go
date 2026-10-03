// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// BrowserPolicy carries the admin-set restrictions applied to a Browser
// experience: which destinations the in-session browser may reach. The end
// user cannot widen this set.
type BrowserPolicy struct {
	// allowedDomains is the allowlist of DNS domains the session browser may
	// reach. Empty means the network profile alone decides.
	// +optional
	AllowedDomains []string `json:"allowedDomains,omitempty"`

	// deniedDomains is subtracted from the allowlist.
	// +optional
	DeniedDomains []string `json:"deniedDomains,omitempty"`
}

// LinuxRuntimeSpec configures a LinuxContainer template. The image is pinned
// by digest; a tag-only reference is rejected because it is mutable.
//
// +kubebuilder:validation:XValidation:rule="!has(self.sessionCmd) || self.adapter == 'kasm'",message="sessionCmd is only valid with adapter=kasm"
// +kubebuilder:validation:XValidation:rule="self.adapter != 'kasm' || !has(self.command) || size(self.command) == 0",message="command must be empty with adapter=kasm; the adapter supplies the container command"
type LinuxRuntimeSpec struct {
	// image is the OCI reference for the runtime image, digest-pinned:
	// <repo>@sha256:<64 hex>. Tags without a digest are rejected.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern="^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)*@sha256:[0-9a-f]{64}$"
	Image string `json:"image"`

	// command is the fixed container command/args chosen by the admin. End
	// users can never supply or override a container command. Forbidden with
	// adapter=kasm — the adapter supplies the container command.
	// +optional
	Command []string `json:"command,omitempty"`

	// adapter selects an injected runtime adapter for the image: "kasm"
	// runs UNMODIFIED kasmweb/* workspace images behind the adapter scripts
	// the operator delivers via an initContainer (docs/kasm-images.md).
	// Requires the operator to be configured with --kasm-adapter-image;
	// empty keeps the image's own entrypoint contract (the tcdi/* images).
	// +optional
	// +kubebuilder:default=""
	Adapter RuntimeAdapter `json:"adapter,omitempty"`

	// sessionCmd is the session payload command the adapter runs inside the
	// desktop session (exported to the runtime as TCDI_SESSION_CMD). For
	// Chromium-family kasmweb images it MUST point past the image's
	// --no-sandbox wrapper scripts at the real binary — per docs/kasm-images.md
	// every cataloged image needs a wrapper audit. Only valid with
	// adapter=kasm; empty runs the image's default desktop session.
	// +optional
	// +kubebuilder:validation:Pattern="^[ -~]{1,512}$"
	SessionCmd string `json:"sessionCmd,omitempty"`

	// browserPolicy restricts the in-session browser when experience is
	// Browser.
	// +optional
	BrowserPolicy *BrowserPolicy `json:"browserPolicy,omitempty"`

	// hostUsers sets the pod's hostUsers field: false runs the runtime in a
	// user namespace so its root is not node root. Unset leaves the pod
	// field nil — the operator's default applies when configured. Opt-in in
	// v0.2; it becomes the default only after the R1 spike proves
	// KasmVNC + the Chromium sandbox + PVC on RKE2 (design §3.7, D26).
	// +optional
	HostUsers *bool `json:"hostUsers,omitempty"`
}

// PlacementSpec controls where runtime pods are scheduled. Admin-only,
// immutable with the spec. Per field the precedence is: template field →
// legacy annotation (nodeSelector only — the
// workspaces.cdi.tinyorbit.vn/node-selector annotation is deprecated and
// ships for one release) → operator default. A field the template sets
// REPLACES the default; lists/maps are never merged.
type PlacementSpec struct {
	// nodeSelector pins runtime pods to nodes carrying these labels.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations let runtime pods land on tainted nodes (the dedicated
	// workspace pool). Capped at 16 — a scheduling hint, not a policy
	// surface.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// runtimeClassName selects the node container runtime handler (e.g.
	// gvisor). Passed through to pod.spec.runtimeClassName; v0.2 does not
	// maintain a tested handler matrix (design §3.7).
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	RuntimeClassName *string `json:"runtimeClassName,omitempty"`
}

// WindowsSourcePVCRef names the sealed source PVC the template clones from.
// The PVC must live in the same namespace as the template (MVP); the
// namespace field must be omitted so a cross-namespace reference cannot be
// expressed.
type WindowsSourcePVCRef struct {
	// name of the sealed source PVC in the template's namespace.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// namespace must be omitted. The source PVC is always resolved in the
	// template's own namespace; setting this field is rejected.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// WindowsRuntimeSpec configures a WindowsVM template.
type WindowsRuntimeSpec struct {
	// sourcePVCRef is the sealed, golden-image PVC to clone per workspace.
	// +required
	SourcePVCRef WindowsSourcePVCRef `json:"sourcePVCRef"`

	// sourceChecksum is the content checksum of the sealed source, recorded
	// by the admin at seal time and used to detect a re-sealed or swapped
	// source.
	// +required
	// +kubebuilder:validation:Pattern="^sha256:[0-9a-f]{64}$"
	SourceChecksum string `json:"sourceChecksum"`
}

// ResourceProfile is the admin-sized compute/storage envelope for workspaces
// created from the template.
type ResourceProfile struct {
	// cpu request/limit for the runtime.
	// +required
	CPU resource.Quantity `json:"cpu"`

	// memory request/limit for the runtime.
	// +required
	Memory resource.Quantity `json:"memory"`

	// storage is the size of the per-workspace data volume (Linux home PVC or
	// Windows boot disk clone).
	// +required
	Storage resource.Quantity `json:"storage"`
}

// LifecycleDefaults are the admin-set defaults a workspace inherits.
type LifecycleDefaults struct {
	// idleTimeout is how long an unused (no input) session may run before the
	// platform stops it.
	// +required
	IdleTimeout metav1.Duration `json:"idleTimeout"`

	// disconnectTimeout is the grace period after the last interactive
	// connection drops before the runtime is stopped.
	// +required
	DisconnectTimeout metav1.Duration `json:"disconnectTimeout"`

	// maxDuration is the hard cap on a single Running generation.
	// +required
	MaxDuration metav1.Duration `json:"maxDuration"`

	// dataPolicy is the default data policy for workspaces created from this
	// template.
	// +required
	DataPolicy DataPolicy `json:"dataPolicy"`

	// imageUpdate selects how workspaces created from this template pick up
	// newer published revisions of the same template family: OnStart moves a
	// workspace to the newest revision on every Stopped -> Running start;
	// Pinned keeps it on its recorded revision.
	// +optional
	// +kubebuilder:validation:Enum=OnStart;Pinned
	// +kubebuilder:default=OnStart
	ImageUpdate ImageUpdatePolicy `json:"imageUpdate,omitempty"`
}

// WorkspaceTemplateSpec describes one immutable template revision. Templates
// are published by admins only; a new revision is a new object (or a bumped
// revision field in a fresh object), never an in-place edit. Spec is fully
// immutable after create — enforced by CEL below.
type WorkspaceTemplateSpec struct {
	// revision is an admin-assigned identifier for this immutable template
	// revision (e.g. "2026-09-a"). Immutable with the rest of spec.
	// +required
	// +kubebuilder:validation:MinLength=1
	Revision string `json:"revision"`

	// runtime selects the runtime kind.
	// +required
	Runtime RuntimeType `json:"runtime"`

	// experience selects the client experience.
	// +required
	Experience ExperienceType `json:"experience"`

	// linux holds the LinuxContainer configuration. Required iff
	// runtime=LinuxContainer, forbidden otherwise.
	// +optional
	Linux *LinuxRuntimeSpec `json:"linux,omitempty"`

	// windows holds the WindowsVM configuration. Required iff
	// runtime=WindowsVM, forbidden otherwise.
	// +optional
	Windows *WindowsRuntimeSpec `json:"windows,omitempty"`

	// resources is the compute/storage profile for the runtime.
	// +required
	Resources ResourceProfile `json:"resources"`

	// bootDeadline bounds how long a workspace may take to become Ready
	// before it is failed.
	// +required
	BootDeadline metav1.Duration `json:"bootDeadline"`

	// networkProfile selects the permitted egress profile for the runtime.
	// +required
	NetworkProfile NetworkProfile `json:"networkProfile"`

	// clipboardPolicy selects clipboard redirection between client and
	// workspace.
	// +required
	ClipboardPolicy ClipboardPolicy `json:"clipboardPolicy"`

	// lifecycle carries the default idle/disconnect/max-duration and data
	// policy for workspaces from this template.
	// +required
	Lifecycle LifecycleDefaults `json:"lifecycle"`

	// placement controls where runtime pods are scheduled: node selector,
	// tolerations, runtime class. Admin-only and immutable with the spec.
	// +optional
	Placement *PlacementSpec `json:"placement,omitempty"`
}

// WorkspaceTemplateStatus is a minimal status: templates are data objects,
// not reconciled workloads.
type WorkspaceTemplateStatus struct {
	// observedGeneration is the latest metadata.generation seen by a
	// consumer (informational).
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Runtime",type=string,JSONPath=".spec.runtime"
// +kubebuilder:printcolumn:name="Experience",type=string,JSONPath=".spec.experience"
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=".spec.revision"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="self.spec == oldSelf.spec",message="spec is immutable; publish a new revision object instead"
// +kubebuilder:validation:XValidation:rule="(self.spec.runtime == 'LinuxContainer' && has(self.spec.linux) && !has(self.spec.windows)) || (self.spec.runtime == 'WindowsVM' && has(self.spec.windows) && !has(self.spec.linux))",message="spec.linux is required iff spec.runtime=LinuxContainer, spec.windows iff WindowsVM; the other block is forbidden"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.windows) || !has(self.spec.windows.sourcePVCRef) || !has(self.spec.windows.sourcePVCRef.namespace)",message="spec.windows.sourcePVCRef.namespace must be omitted; the source PVC is always resolved in the template's namespace (cross-namespace references are not allowed)"

// WorkspaceTemplate is the Schema for the workspacetemplates API.
type WorkspaceTemplate struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of WorkspaceTemplate
	// +required
	Spec WorkspaceTemplateSpec `json:"spec"`

	// status defines the observed state of WorkspaceTemplate
	// +optional
	Status WorkspaceTemplateStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// WorkspaceTemplateList contains a list of WorkspaceTemplate.
type WorkspaceTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []WorkspaceTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &WorkspaceTemplate{}, &WorkspaceTemplateList{})
		return nil
	})
}
