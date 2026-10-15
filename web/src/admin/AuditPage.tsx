import { useCallback, useState, type FormEvent } from "react";
import { Badge, Button, Input, Pagination, Table, useServerPage, type Tone } from "../design";
import type { Column } from "../design/Table";
import { IconRefresh } from "../design/icons";
import { t, type MessageKey } from "../i18n";
import { useMeLoaded } from "../app/me";
import { useApi } from "../api/context";
import { AdminLayout } from "./AdminLayout";
import { ApiErrorAlert } from "./ApiErrorAlert";
import { formatDateTime } from "./format";
import { listAdminAuditPage, type AdminAuditEntry, type AuditFilter } from "./api";

const OUTCOME_TONE: Record<AdminAuditEntry["outcome"], Tone> = {
  success: "success",
  denied: "warning",
  failure: "danger",
};

const OUTCOME_LABEL: Record<AdminAuditEntry["outcome"], MessageKey> = {
  success: "admin.audit.outcome.success",
  denied: "admin.audit.outcome.denied",
  failure: "admin.audit.outcome.failure",
};

// A datetime-local value ("2026-10-13T09:30") is local time; the API wants RFC 3339.
function toIso(local: string): string | undefined {
  if (!local) return undefined;
  const d = new Date(local);
  return Number.isNaN(d.getTime()) ? undefined : d.toISOString();
}

export function AuditPage() {
  const api = useApi();
  const me = useMeLoaded();
  const tenant = me.data?.tenant;
  const [draft, setDraft] = useState({ action: "", actor: "", target: "", since: "", until: "" });
  // The filter the list is loaded with; changes only on Apply / Clear.
  const [applied, setApplied] = useState<AuditFilter>({});
  const load = useCallback(
    (cursor?: string) =>
      tenant === undefined
        ? Promise.resolve({ items: [] as AdminAuditEntry[] })
        : listAdminAuditPage(api, tenant, applied, cursor),
    [api, tenant, applied],
  );
  const list = useServerPage(load, { sort: "-createdAt", key: JSON.stringify([tenant, applied]) });

  const apply = (e: FormEvent) => {
    e.preventDefault();
    setApplied({
      ...(draft.action.trim() ? { action: draft.action.trim() } : {}),
      ...(draft.actor.trim() ? { actor: draft.actor.trim() } : {}),
      ...(draft.target.trim() ? { target: draft.target.trim() } : {}),
      ...(toIso(draft.since) ? { since: toIso(draft.since)! } : {}),
      ...(toIso(draft.until) ? { until: toIso(draft.until)! } : {}),
    });
  };
  const clear = () => {
    setDraft({ action: "", actor: "", target: "", since: "", until: "" });
    setApplied({});
  };

  const columns: Column<AdminAuditEntry>[] = [
    { key: "time", header: t("admin.audit.column.time"), rowHeader: true,
      render: (r) => <time dateTime={r.createdAt}>{formatDateTime(r.createdAt)}</time> },
    { key: "actor", header: t("admin.audit.column.actor"), hideOnMobile: true, render: (r) => <code>{r.actor}</code> },
    { key: "action", header: t("admin.audit.column.action"), render: (r) => <code>{r.action}</code> },
    { key: "target", header: t("admin.audit.column.target"),
      render: (r) => (r.targetId ? <code>{r.targetId}</code> : "—") },
    { key: "outcome", header: t("admin.audit.column.outcome"),
      render: (r) => <Badge tone={OUTCOME_TONE[r.outcome]}>{t(OUTCOME_LABEL[r.outcome])}</Badge> },
    { key: "details", header: t("admin.audit.column.details"), hideOnMobile: true,
      render: (r) => Object.entries(r.details).map(([k, v]) => `${k}=${v}`).join(", ") || "—" },
  ];

  return (
    <AdminLayout
      title={t("admin.audit.title")}
      description={t("admin.audit.description")}
      actions={<Button icon={<IconRefresh />} onClick={list.refresh} loading={list.loading && list.loaded}>{t("admin.action.refresh")}</Button>}
    >
      <form className="tc-admin-filters" onSubmit={apply}>
        <Input label={t("admin.audit.filter.action")} value={draft.action} onChange={(e) => setDraft({ ...draft, action: e.target.value })} />
        <Input label={t("admin.audit.filter.actor")} hint={t("admin.audit.filter.actorHint")} value={draft.actor} onChange={(e) => setDraft({ ...draft, actor: e.target.value })} />
        <Input label={t("admin.audit.filter.target")} value={draft.target} onChange={(e) => setDraft({ ...draft, target: e.target.value })} />
        <Input label={t("admin.audit.filter.since")} type="datetime-local" value={draft.since} onChange={(e) => setDraft({ ...draft, since: e.target.value })} />
        <Input label={t("admin.audit.filter.until")} type="datetime-local" value={draft.until} onChange={(e) => setDraft({ ...draft, until: e.target.value })} />
        <Button type="submit" variant="primary">{t("admin.audit.filter.apply")}</Button>
        <Button onClick={clear}>{t("admin.audit.filter.clear")}</Button>
      </form>
      <ApiErrorAlert error={me.error ?? list.error} onRetry={me.error ? me.reload : list.refresh} />
      <Table
        caption={t("admin.audit.caption")}
        columns={columns}
        rows={list.items}
        rowKey={(r) => r.id}
        loading={(tenant === undefined || !list.loaded) && !list.error && !me.error}
        density="compact"
        empty={t("admin.audit.empty")}
      />
      <Pagination
        loaded={list.items.length}
        hasNext={list.hasNext}
        loading={list.loading}
        onLoadMore={list.next}
        label={t("common.pagination.label", { name: t("admin.audit.caption") })}
      />
    </AdminLayout>
  );
}
