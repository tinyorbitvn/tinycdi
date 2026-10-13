import { t, formatDateTime as formatDateTimeIntl, formatNumber } from "../i18n";
import type { MessageKey } from "../i18n";
import type { Owner, QuotaAmounts } from "./api";

// Compact age: "45s", "12m", "3h 5m", "4d 2h". Future timestamps (clock skew)
// read as "just now" rather than a negative age.
export function formatAge(iso: string, now: number = Date.now()): string {
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return "—";
  const s = Math.floor((now - then) / 1000);
  if (s < 5) return t("admin.time.justNow");
  if (s < 60) return t("admin.time.s", { n: s });
  const m = Math.floor(s / 60);
  if (m < 60) return t("admin.time.m", { n: m });
  const h = Math.floor(m / 60);
  if (h < 24) return m % 60 ? t("admin.time.hm", { h, m: m % 60 }) : t("admin.time.h", { n: h });
  const d = Math.floor(h / 24);
  return h % 24 ? t("admin.time.dh", { d, h: h % 24 }) : t("admin.time.d", { n: d });
}

// "5m ago" / "just now" for prose.
export function formatAgo(iso: string, now: number = Date.now()): string {
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return "—";
  if (Math.floor((now - then) / 1000) < 5) return t("admin.time.justNow");
  return t("admin.time.ago", { age: formatAge(iso, now) });
}

export function formatDateTime(iso: string): string {
  const tms = Date.parse(iso);
  return Number.isNaN(tms) ? "—" : formatDateTimeIntl(tms);
}

export type QuotaKey = keyof QuotaAmounts;

export const QUOTA_KEYS: readonly QuotaKey[] = [
  "workspaces",
  "runningWorkspaces",
  "cpuMillicores",
  "memoryMib",
  "storageGib",
];

export const QUOTA_LABEL_KEYS: Record<QuotaKey, MessageKey> = {
  workspaces: "admin.quota.amount.workspaces",
  runningWorkspaces: "admin.quota.amount.runningWorkspaces",
  cpuMillicores: "admin.quota.amount.cpuMillicores",
  memoryMib: "admin.quota.amount.memoryMib",
  storageGib: "admin.quota.amount.storageGib",
};

export function quotaLabel(key: QuotaKey): string {
  return t(QUOTA_LABEL_KEYS[key]);
}

export function formatQuota(key: QuotaKey, value: number): string {
  switch (key) {
    case "cpuMillicores":
      return t("admin.unit.vcpu", { n: trim(value / 1000) });
    case "memoryMib":
      return value >= 1024 ? t("admin.unit.gib", { n: trim(value / 1024) }) : t("admin.unit.mib", { n: value });
    case "storageGib":
      return value >= 1024 ? t("admin.unit.tib", { n: trim(value / 1024) }) : t("admin.unit.gib", { n: value });
    default:
      return formatNumber(value);
  }
}

// Lifecycle timeouts ("2h", "30m", "45s") for the template catalog.
export function formatSeconds(s: number): string {
  if (s % 3600 === 0) return t("admin.time.h", { n: s / 3600 });
  if (s % 60 === 0) return t("admin.time.m", { n: s / 60 });
  return t("admin.time.s", { n: s });
}

function trim(n: number): string {
  return Number.isInteger(n) ? String(n) : n.toFixed(1).replace(/\.0$/, "");
}

export function usagePercent(used: number, limit: number): number {
  if (limit <= 0) return used > 0 ? 100 : 0;
  return Math.round((used / limit) * 100);
}

export function ownerLabel(owner: Owner | undefined): string {
  return owner ? owner.displayName || owner.subject : "—";
}
