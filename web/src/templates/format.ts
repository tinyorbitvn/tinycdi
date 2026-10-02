import { t, type MessageKey } from "../i18n";
import type { DataPolicy, NetworkProfile, RuntimeKind, ExperienceKind, TemplateView } from "./types";

// Every user-visible label comes from the message catalog (D33); the maps
// below bind contract enum values to message keys.

/** "4 vCPU", "0.5 vCPU". */
export function formatCpu(millicores: number): string {
  const cores = millicores / 1000;
  return t("templates.format.cpu", { n: Number.isInteger(cores) ? cores : cores.toFixed(1) });
}

/** MiB → "8 GiB" / "512 MiB". */
export function formatMemory(mib: number): string {
  if (mib >= 1024) {
    const gib = mib / 1024;
    return t("templates.format.gib", { n: Number.isInteger(gib) ? gib : gib.toFixed(1) });
  }
  return t("templates.format.mib", { n: mib });
}

export function formatStorage(gib: number): string {
  return t("templates.format.gib", { n: gib });
}

/** "4 vCPU · 8 GiB RAM · 20 GiB disk". */
export function formatResources(r: TemplateView["resources"]): string {
  return t("templates.format.resources", {
    cpu: formatCpu(r.cpuMillicores),
    memory: formatMemory(r.memoryMib),
    storage: formatStorage(r.storageGib),
  });
}

/** Seconds → "30 min", "8 h", "1 h 30 min". */
export function formatDuration(seconds: number): string {
  if (seconds <= 0) return t("templates.format.duration.none");
  const h = Math.floor(seconds / 3600);
  const m = Math.round((seconds % 3600) / 60);
  if (h && m) return t("templates.format.duration.hm", { h, m });
  if (h) return t("templates.format.duration.h", { h });
  return t("templates.format.duration.m", { m });
}

const RUNTIME_KEY: Record<RuntimeKind, MessageKey> = {
  LinuxContainer: "templates.runtime.linuxContainer",
  WindowsVM: "templates.runtime.windowsVm",
};

const EXPERIENCE_KEY: Record<ExperienceKind, MessageKey> = {
  Desktop: "templates.experience.desktop",
  Browser: "templates.experience.browser",
};

const NETWORK_PROFILE_KEYS: Record<NetworkProfile, { label: MessageKey; description: MessageKey }> = {
  InternetOnly: {
    label: "templates.network.internetOnly.label",
    description: "templates.network.internetOnly.description",
  },
  ClusterOnly: {
    label: "templates.network.clusterOnly.label",
    description: "templates.network.clusterOnly.description",
  },
  Isolated: {
    label: "templates.network.isolated.label",
    description: "templates.network.isolated.description",
  },
};

const DATA_POLICY_KEYS: Record<DataPolicy, { label: MessageKey; description: MessageKey }> = {
  Retain: {
    label: "templates.dataPolicy.retain.label",
    description: "templates.dataPolicy.retain.description",
  },
  Ephemeral: {
    label: "templates.dataPolicy.ephemeral.label",
    description: "templates.dataPolicy.ephemeral.description",
  },
};

export function runtimeLabel(runtime: RuntimeKind): string {
  return t(RUNTIME_KEY[runtime]);
}

export function experienceLabel(experience: ExperienceKind): string {
  return t(EXPERIENCE_KEY[experience]);
}

export function networkProfileLabel(profile: NetworkProfile): string {
  return t(NETWORK_PROFILE_KEYS[profile].label);
}

export function networkProfileDescription(profile: NetworkProfile): string {
  return t(NETWORK_PROFILE_KEYS[profile].description);
}

export function dataPolicyLabel(policy: DataPolicy): string {
  return t(DATA_POLICY_KEYS[policy].label);
}

export function dataPolicyDescription(policy: DataPolicy): string {
  return t(DATA_POLICY_KEYS[policy].description);
}

const CLIPBOARD_KEY: Record<string, MessageKey> = {
  Disabled: "templates.clipboard.disabled",
  Send: "templates.clipboard.send",
  Receive: "templates.clipboard.receive",
  Bidirectional: "templates.clipboard.bidirectional",
};

/** Label for the template clipboard policy (CRD enum: Disabled|Send|Receive|Bidirectional). */
export function clipboardPolicyLabel(policy: string): string {
  const key = CLIPBOARD_KEY[policy];
  return key ? t(key) : policy;
}

/** Short date for build/publish timestamps ("Sep 30, 2026"). */
export function formatDate(iso: string): string {
  return new Date(iso).toLocaleDateString(undefined, { dateStyle: "medium" });
}

export function formatDateTime(iso: string): string {
  return new Date(iso).toLocaleString();
}
