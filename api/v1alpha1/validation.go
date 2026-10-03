// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package v1alpha1

// Shared enum types and condition/phase constants for the workspaces API.
// Values are validated at the apiserver by CRD schema (Enum) and CEL
// (x-kubernetes-validations); the constants here keep controllers and tests
// on one spelling.

// RuntimeType selects the workload the operator creates.
// +kubebuilder:validation:Enum=LinuxContainer;WindowsVM
type RuntimeType string

const (
	// RuntimeLinuxContainer runs a Linux desktop/browser in a Pod.
	RuntimeLinuxContainer RuntimeType = "LinuxContainer"
	// RuntimeWindowsVM runs a Windows VM via KubeVirt.
	RuntimeWindowsVM RuntimeType = "WindowsVM"
)

// ExperienceType selects the client-facing experience.
// +kubebuilder:validation:Enum=Desktop;Browser
type ExperienceType string

const (
	// ExperienceDesktop is a full desktop session.
	ExperienceDesktop ExperienceType = "Desktop"
	// ExperienceBrowser is a single locked-down browser window.
	ExperienceBrowser ExperienceType = "Browser"
)

// DesiredState is the lifecycle intent recorded in Workspace.spec.
// +kubebuilder:validation:Enum=Running;Stopped
type DesiredState string

const (
	// DesiredStateRunning requests a running runtime.
	DesiredStateRunning DesiredState = "Running"
	// DesiredStateStopped requests the runtime be stopped but the workspace
	// (and data allowed by dataPolicy) kept.
	DesiredStateStopped DesiredState = "Stopped"
)

// ImageUpdatePolicy selects how a workspace picks up newer published
// revisions of its template family.
// +kubebuilder:validation:Enum=OnStart;Pinned
type ImageUpdatePolicy string

const (
	// ImageUpdateOnStart moves the workspace to the newest published
	// revision of its template family on every Stopped -> Running start.
	ImageUpdateOnStart ImageUpdatePolicy = "OnStart"
	// ImageUpdatePinned keeps the workspace on its recorded template
	// revision across starts; only a delete + recreate moves it.
	ImageUpdatePinned ImageUpdatePolicy = "Pinned"
)

// DataPolicy controls what happens to workspace data on Stop/Delete.
// +kubebuilder:validation:Enum=Ephemeral;Retain
type DataPolicy string

const (
	// DataPolicyEphemeral destroys workspace data when the workspace is
	// deleted (browser sessions, scratch-only workspaces).
	DataPolicyEphemeral DataPolicy = "Ephemeral"
	// DataPolicyRetain moves workspace data into the retained inventory on
	// delete; a separate purge removes it.
	DataPolicyRetain DataPolicy = "Retain"
)

// NetworkProfile selects the runtime egress policy.
// +kubebuilder:validation:Enum=InternetOnly;ClusterOnly;Isolated
type NetworkProfile string

const (
	// NetworkProfileInternetOnly allows egress to the Internet plus cluster
	// DNS and the bootstrap API; pod/service/node/metadata CIDRs are denied.
	NetworkProfileInternetOnly NetworkProfile = "InternetOnly"
	// NetworkProfileClusterOnly restricts egress to in-cluster destinations.
	NetworkProfileClusterOnly NetworkProfile = "ClusterOnly"
	// NetworkProfileIsolated denies all egress except cluster DNS.
	NetworkProfileIsolated NetworkProfile = "Isolated"
)

// RuntimeAdapter selects an injected runtime adapter for a LinuxContainer
// template: a shim the operator delivers into the pod via an initContainer
// so a foreign workspace image can satisfy the TinyCDI runtime contract
// without modification. "" runs the image's own entrypoint (the tcdi/*
// images); "kasm" runs unmodified kasmweb/* images via build/kasm-adapter
// (docs/kasm-images.md).
// +kubebuilder:validation:Enum="";kasm
type RuntimeAdapter string

const (
	// AdapterNone runs the image's own entrypoint — the default.
	AdapterNone RuntimeAdapter = ""
	// AdapterKasm runs unmodified kasmweb/* workspace images behind the
	// injected adapter (requires the operator's --kasm-adapter-image).
	AdapterKasm RuntimeAdapter = "kasm"
)

// ClipboardPolicy selects clipboard redirection between client and runtime.
// +kubebuilder:validation:Enum=Disabled;Send;Receive;Bidirectional
type ClipboardPolicy string

const (
	// ClipboardDisabled turns clipboard redirection off.
	ClipboardDisabled ClipboardPolicy = "Disabled"
	// ClipboardSend allows client -> workspace only.
	ClipboardSend ClipboardPolicy = "Send"
	// ClipboardReceive allows workspace -> client only.
	ClipboardReceive ClipboardPolicy = "Receive"
	// ClipboardBidirectional allows both directions.
	ClipboardBidirectional ClipboardPolicy = "Bidirectional"
)

// WorkspacePhase summarizes lifecycle position. Conditions carry the real
// state; phase is display/routing sugar.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Stopping;Stopped;Failed;Terminating
type WorkspacePhase string

const (
	WorkspacePhasePending      WorkspacePhase = "Pending"
	WorkspacePhaseProvisioning WorkspacePhase = "Provisioning"
	WorkspacePhaseReady        WorkspacePhase = "Ready"
	WorkspacePhaseStopping     WorkspacePhase = "Stopping"
	WorkspacePhaseStopped      WorkspacePhase = "Stopped"
	WorkspacePhaseFailed       WorkspacePhase = "Failed"
	WorkspacePhaseTerminating  WorkspacePhase = "Terminating"
)

// DataRole identifies which volume a DataReference points at.
// +kubebuilder:validation:Enum=Home;Boot;Scratch;Retained
type DataRole string

const (
	// DataRoleHome is the Linux per-user home PVC.
	DataRoleHome DataRole = "Home"
	// DataRoleBoot is the Windows boot disk clone.
	DataRoleBoot DataRole = "Boot"
	// DataRoleScratch is ephemeral scratch space.
	DataRoleScratch DataRole = "Scratch"
	// DataRoleRetained is a disk in the retained inventory attached to this
	// workspace.
	DataRoleRetained DataRole = "Retained"
)

// Workspace status condition types.
const (
	// ConditionAdmitted is true once the platform API has admitted the
	// workspace (quota reserved, template resolved).
	ConditionAdmitted = "Admitted"
	// ConditionStorageReady is true when all required volumes are bound and
	// attached.
	ConditionStorageReady = "StorageReady"
	// ConditionRuntimeReady is true when the runtime incarnation reports
	// ready (display server / RDP up).
	ConditionRuntimeReady = "RuntimeReady"
	// ConditionConnectionReady is true when the streaming endpoint is
	// reachable through the gateway path.
	ConditionConnectionReady = "ConnectionReady"
	// ConditionDegraded is true when the workspace is up but impaired
	// (e.g. runtime restarted, reconnect pending).
	ConditionDegraded = "Degraded"
)

// DigestPattern matches a digest-pinned OCI reference. The regex is embedded
// in the generated CRD via a kubebuilder Pattern marker on
// LinuxRuntimeSpec.Image; keep it in sync here for unit-test reuse and
// documentation.
const DigestPattern = `^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)*@sha256:[0-9a-f]{64}$`

// SessionCmdPattern constrains spec.linux.sessionCmd to a printable-ASCII
// command line (no control characters, no newlines; 1..512 chars) — the
// value lands verbatim in the runtime's session environment. The regex is
// embedded in the generated CRD via a kubebuilder Pattern marker on
// LinuxRuntimeSpec.SessionCmd; keep it in sync here for the operator's
// snapshot re-verification.
const SessionCmdPattern = `^[ -~]{1,512}$`
