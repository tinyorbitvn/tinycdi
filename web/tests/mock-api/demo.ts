// Demo tenant for the dev server (createMockApi({ demo: true }), i.e.
// MOCK_DEMO=1 tests/mock-api/server.ts). Synthetic, contract-shaped records
// that exercise every card state the portal renders: connectable, starting,
// stopping, failed, stopped with retained data, and an ephemeral browser.
// Tests never load these — they seed exactly what they assert on.

import {
  READY_CONDITIONS,
  TEMPLATE_BROWSER,
  TEMPLATE_LINUX,
  type ConditionFixture,
  type RetainedFixture,
  type TemplateFixture,
  type WorkspaceEventFixture,
  type WorkspaceFixture,
} from "./fixtures.ts";

export const DEMO_TEMPLATES: TemplateFixture[] = [
  {
    id: "tpl_01J4ZE2K8VQW5R7T3N9P1M6X4D",
    name: "dev-workstation",
    description: "Ubuntu workstation with VS Code, Git and container tooling for in-cluster development",
    revision: 12,
    runtime: "LinuxContainer",
    experience: "Desktop",
    resources: { cpuMillicores: 8000, memoryMib: 16384, storageGib: 50 },
    lifecycleDefaults: { idleTimeoutSeconds: 3600, disconnectGraceSeconds: 900, maxRunningSeconds: 43200 },
    dataPolicyDefault: "Retain",
    clipboardPolicy: "Enabled",
    networkProfile: "ClusterOnly",
    publishedAt: "2026-09-12T08:30:00Z",
  },
  {
    id: "tpl_01J4ZF6P3XRB8T2W7K5N9Q1H3C",
    name: "windows-office",
    description: "Windows desktop VM with an office suite for line-of-business apps",
    revision: 4,
    runtime: "WindowsVM",
    experience: "Desktop",
    resources: { cpuMillicores: 4000, memoryMib: 8192, storageGib: 64 },
    lifecycleDefaults: { idleTimeoutSeconds: 1800, disconnectGraceSeconds: 600, maxRunningSeconds: 28800 },
    dataPolicyDefault: "Retain",
    clipboardPolicy: "Enabled",
    networkProfile: "InternetOnly",
    publishedAt: "2026-09-18T14:00:00Z",
  },
  {
    id: "tpl_01J4ZG9R5YTC1V4X8M2P6S3K7B",
    name: "isolated-analysis",
    description: "Air-gapped desktop for handling sensitive datasets — no egress, clipboard off",
    revision: 2,
    runtime: "LinuxContainer",
    experience: "Desktop",
    resources: { cpuMillicores: 4000, memoryMib: 16384, storageGib: 100 },
    lifecycleDefaults: { idleTimeoutSeconds: 900, disconnectGraceSeconds: 300, maxRunningSeconds: 14400 },
    dataPolicyDefault: "Ephemeral",
    clipboardPolicy: "Disabled",
    networkProfile: "Isolated",
    publishedAt: "2026-09-25T09:15:00Z",
  },
];

const [DEV, WIN] = DEMO_TEMPLATES;

function summary(t: TemplateFixture): WorkspaceFixture["template"] {
  return { id: t.id, name: t.name, revision: t.revision, runtime: t.runtime, experience: t.experience };
}

function at(conditions: ConditionFixture[], time: string): ConditionFixture[] {
  return conditions.map((c) => ({ ...c, lastTransitionTime: time }));
}

// Stored demo records carry a placeholder owner; the mock replaces it
// with the tenancy owner at serve time (see admin.ts ownerOf).
const DEMO_OWNER: WorkspaceFixture["owner"] = { subject: "user-alice", displayName: "Alice A" };

export const DEMO_WORKSPACES: WorkspaceFixture[] = [
  {
    id: "ws_01J4ZH1A2B3C4D5E6F7G8H9J0K",
    name: "design-review",
    owner: DEMO_OWNER,
    template: summary(TEMPLATE_LINUX),
    phase: "Ready",
    desiredState: "Running",
    dataPolicy: "Retain",
    conditions: at(READY_CONDITIONS, "2026-10-01T08:02:00Z"),
    createdAt: "2026-09-28T07:45:00Z",
    updatedAt: "2026-10-01T08:02:00Z",
  },
  {
    id: "ws_01J4ZH2B3C4D5E6F7G8H9J0K1M",
    name: "quick-browse",
    owner: DEMO_OWNER,
    template: summary(TEMPLATE_BROWSER),
    phase: "Ready",
    desiredState: "Running",
    dataPolicy: "Ephemeral",
    conditions: at(READY_CONDITIONS, "2026-10-01T09:10:00Z"),
    createdAt: "2026-10-01T09:09:00Z",
    updatedAt: "2026-10-01T09:10:00Z",
  },
  {
    id: "ws_01J4ZH3C4D5E6F7G8H9J0K1M2N",
    name: "kernel-dev",
    owner: DEMO_OWNER,
    template: summary(DEV),
    phase: "Provisioning",
    desiredState: "Running",
    dataPolicy: "Retain",
    conditions: [
      { type: "Admitted", status: "True", reason: "QuotaReserved", lastTransitionTime: "2026-10-01T09:20:00Z" },
      { type: "StorageReady", status: "True", reason: "VolumeBound", lastTransitionTime: "2026-10-01T09:20:20Z" },
      {
        type: "RuntimeReady",
        status: "False",
        reason: "ImagePulling",
        message: "pulling runtime image (1.8 GiB)",
        lastTransitionTime: "2026-10-01T09:20:25Z",
      },
    ],
    createdAt: "2026-09-30T16:00:00Z",
    updatedAt: "2026-10-01T09:20:25Z",
  },
  {
    id: "ws_01J4ZH4D5E6F7G8H9J0K1M2N3P",
    name: "finance-office",
    owner: DEMO_OWNER,
    template: summary(WIN),
    phase: "Failed",
    desiredState: "Running",
    dataPolicy: "Retain",
    failureReason: "RuntimeStartTimeout: the Windows VM did not report ready within 10m",
    conditions: [
      { type: "Admitted", status: "True", reason: "QuotaReserved", lastTransitionTime: "2026-10-01T07:00:00Z" },
      { type: "StorageReady", status: "True", reason: "VolumeBound", lastTransitionTime: "2026-10-01T07:00:40Z" },
      {
        type: "RuntimeReady",
        status: "False",
        reason: "StartTimeout",
        message: "guest agent did not report ready within 10m",
        lastTransitionTime: "2026-10-01T07:11:00Z",
      },
      {
        type: "Degraded",
        status: "True",
        reason: "RuntimeStartTimeout",
        message: "start can be retried; disk is intact",
        lastTransitionTime: "2026-10-01T07:11:00Z",
      },
    ],
    createdAt: "2026-09-22T10:30:00Z",
    updatedAt: "2026-10-01T07:11:00Z",
  },
  {
    id: "ws_01J4ZH5E6F7G8H9J0K1M2N3P4Q",
    name: "build-agent",
    owner: DEMO_OWNER,
    template: summary(DEV),
    phase: "Stopping",
    desiredState: "Stopped",
    dataPolicy: "Retain",
    conditions: at(READY_CONDITIONS, "2026-10-01T09:00:00Z"),
    createdAt: "2026-09-25T12:00:00Z",
    updatedAt: "2026-10-01T09:25:00Z",
  },
];

// Transitional demo workspaces are re-stamped at reset so the demo
// controller visibly walks them forward a few seconds after load.
export const DEMO_TRANSITIONAL = new Set(["ws_01J4ZH3C4D5E6F7G8H9J0K1M2N", "ws_01J4ZH5E6F7G8H9J0K1M2N3P4Q"]);

export const DEMO_EVENTS: Record<string, WorkspaceEventFixture[]> = {
  ws_01J4ZH1A2B3C4D5E6F7G8H9J0K: [
    { type: "Normal", reason: "SessionStarted", message: "Interactive session started", lastTimestamp: "2026-10-01T08:05:00Z" },
    { type: "Normal", reason: "RuntimeReady", message: "Runtime is up; stream endpoint ready", lastTimestamp: "2026-10-01T08:02:00Z" },
    { type: "Normal", reason: "Starting", message: "Start requested", lastTimestamp: "2026-10-01T08:00:00Z" },
  ],
  ws_01J4ZH3C4D5E6F7G8H9J0K1M2N: [
    {
      type: "Normal",
      reason: "Pulling",
      message: "Pulling runtime image for dev-workstation@12",
      count: 3,
      firstTimestamp: "2026-10-01T09:20:25Z",
      lastTimestamp: "2026-10-01T09:21:10Z",
    },
    { type: "Normal", reason: "VolumeBound", message: "Persistent volume bound (50 GiB)", lastTimestamp: "2026-10-01T09:20:20Z" },
    { type: "Normal", reason: "Admitted", message: "Quota reserved: 8 CPU, 16 GiB memory, 50 GiB storage", lastTimestamp: "2026-10-01T09:20:00Z" },
  ],
  ws_01J4ZH4D5E6F7G8H9J0K1M2N3P: [
    {
      type: "Warning",
      reason: "RuntimeStartTimeout",
      message: "Windows VM did not report ready within 10m; disk retained",
      lastTimestamp: "2026-10-01T07:11:00Z",
    },
    {
      type: "Warning",
      reason: "GuestAgentUnreachable",
      message: "guest agent probe failed",
      count: 12,
      firstTimestamp: "2026-10-01T07:02:00Z",
      lastTimestamp: "2026-10-01T07:10:30Z",
    },
    { type: "Normal", reason: "Starting", message: "Start requested", lastTimestamp: "2026-10-01T07:00:00Z" },
  ],
  ws_01J4ZH5E6F7G8H9J0K1M2N3P4Q: [
    { type: "Normal", reason: "Stopping", message: "Stop requested", lastTimestamp: "2026-10-01T09:25:00Z" },
  ],
};

export const DEMO_RETAINED: RetainedFixture[] = [
  {
    id: "rd_01J4ZJ1K2L3M4N5P6Q7R8S9T0V",
    state: "Retained",
    owner: DEMO_OWNER,
    sizeGib: 64,
    runtime: "WindowsVM",
    sourceWorkspaceName: "q3-reporting",
    retainedAt: "2026-09-20T17:40:00Z",
    purgeConfirmationNonce: "nonce-demo-1",
  },
  {
    id: "rd_01J4ZJ2L3M4N5P6Q7R8S9T0V1W",
    state: "Retained",
    owner: DEMO_OWNER,
    sizeGib: 50,
    runtime: "LinuxContainer",
    sourceWorkspaceName: "thesis-workstation",
    retainedAt: "2026-09-26T11:05:00Z",
    purgeConfirmationNonce: "nonce-demo-2",
  },
];
