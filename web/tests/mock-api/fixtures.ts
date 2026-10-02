// Fixtures derived from internal/api/openapi.yaml component examples.
// No secrets — all values are synthetic contract-shaped records.

import type { components } from "../../src/api/generated/schema";

// networkProfile mirrors api/v1alpha1 NetworkProfile; it is additive to
// TemplateView (requested for the public API) and optional here so the
// fixtures type-check against both the current and the extended schema.
export type NetworkProfileFixture = "InternetOnly" | "ClusterOnly" | "Isolated";
export type TemplateFixture = components["schemas"]["TemplateView"] & {
  networkProfile?: NetworkProfileFixture;
};
export type ConditionFixture = components["schemas"]["WorkspaceCondition"];
export type WorkspaceFixture = components["schemas"]["WorkspaceView"];
export type RetainedFixture = components["schemas"]["RetainedDataView"];
// GET /v1/workspaces/{id}/events item (Kubernetes-style event summary).
export interface WorkspaceEventFixture {
  type: "Normal" | "Warning";
  reason: string;
  message: string;
  count?: number;
  firstTimestamp?: string;
  lastTimestamp: string;
}

export const TEMPLATE_LINUX: TemplateFixture = {
  id: "tpl_01J4ZB3N1RXD7P2V8W5K0H6Q4M",
  name: "linux-firefox-desktop",
  description: "Ubuntu desktop with Firefox, KasmVNC streaming",
  revision: 7,
  runtime: "LinuxContainer",
  experience: "Desktop",
  resources: { cpuMillicores: 4000, memoryMib: 8192, storageGib: 20 },
  lifecycleDefaults: {
    idleTimeoutSeconds: 1800,
    disconnectGraceSeconds: 600,
    maxRunningSeconds: 28800,
  },
  dataPolicyDefault: "Retain",
  clipboardPolicy: "Enabled",
  networkProfile: "InternetOnly",
  publishedAt: "2026-09-01T00:00:00Z",
};

export const TEMPLATE_BROWSER: TemplateFixture = {
  id: "tpl_01J4ZC7M2QXW8P3V9H5K1N6R4T",
  name: "linux-chromium-browser",
  description: "Ephemeral Chromium browser profile",
  revision: 3,
  runtime: "LinuxContainer",
  experience: "Browser",
  resources: { cpuMillicores: 2000, memoryMib: 4096, storageGib: 10 },
  lifecycleDefaults: {
    idleTimeoutSeconds: 900,
    disconnectGraceSeconds: 300,
    maxRunningSeconds: 14400,
  },
  dataPolicyDefault: "Ephemeral",
  clipboardPolicy: "Disabled",
  networkProfile: "Isolated",
  publishedAt: "2026-09-01T00:00:00Z",
};

export const WORKSPACE_STOPPED: WorkspaceFixture = {
  id: "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E",
  name: "research-desktop",
  owner: { subject: "user-01J4ZDADA", displayName: "Ada Lovelace" },
  template: {
    id: TEMPLATE_LINUX.id,
    name: TEMPLATE_LINUX.name,
    revision: TEMPLATE_LINUX.revision,
    runtime: TEMPLATE_LINUX.runtime,
    experience: TEMPLATE_LINUX.experience,
  },
  phase: "Stopped",
  conditions: [
    {
      type: "Admitted",
      status: "True",
      reason: "QuotaReserved",
      lastTransitionTime: "2026-09-30T10:00:00Z",
    },
    {
      type: "ConnectionReady",
      status: "False",
      reason: "RuntimeStopped",
      message: "runtime is not running",
      lastTransitionTime: "2026-09-30T10:05:00Z",
    },
  ],
  desiredState: "Stopped",
  dataPolicy: "Retain",
  createdAt: "2026-09-30T09:00:00Z",
  updatedAt: "2026-09-30T10:05:00Z",
};

export const RETAINED_DISK: RetainedFixture = {
  id: "rd_01J4Z9W2PFK8G4TQ3M7H1R5N0A",
  state: "Retained",
  owner: { subject: "user-01J4ZDADA", displayName: "Ada Lovelace" },
  sizeGib: 20,
  runtime: "LinuxContainer",
  sourceWorkspaceName: "old-desktop",
  retainedAt: "2026-09-29T12:00:00Z",
  purgeConfirmationNonce: "nonce-initial",
};

export const SEED_EVENTS: WorkspaceEventFixture[] = [
  {
    type: "Normal",
    reason: "Stopped",
    message: "Runtime stopped; disk retained",
    lastTimestamp: "2026-09-30T10:05:00Z",
  },
  {
    type: "Normal",
    reason: "Admitted",
    message: "Quota reserved: 4 CPU, 8 GiB memory, 20 GiB storage",
    lastTimestamp: "2026-09-30T10:00:00Z",
  },
];

export function makeWorkspace(overrides: Partial<WorkspaceFixture> = {}): WorkspaceFixture {
  const base = structuredClone(WORKSPACE_STOPPED);
  return { ...base, ...overrides };
}

export const READY_CONDITIONS: ConditionFixture[] = [
  {
    type: "Admitted",
    status: "True",
    reason: "QuotaReserved",
    lastTransitionTime: "2026-09-30T10:00:00Z",
  },
  {
    type: "StorageReady",
    status: "True",
    reason: "VolumeBound",
    lastTransitionTime: "2026-09-30T10:01:00Z",
  },
  {
    type: "RuntimeReady",
    status: "True",
    reason: "RuntimeUp",
    lastTransitionTime: "2026-09-30T10:02:00Z",
  },
  {
    type: "ConnectionReady",
    status: "True",
    reason: "StreamEndpointUp",
    lastTransitionTime: "2026-09-30T10:02:30Z",
  },
];

export function readyWorkspace(overrides: Partial<WorkspaceFixture> = {}): WorkspaceFixture {
  return makeWorkspace({
    phase: "Ready",
    desiredState: "Running",
    conditions: structuredClone(READY_CONDITIONS),
    ...overrides,
  });
}
