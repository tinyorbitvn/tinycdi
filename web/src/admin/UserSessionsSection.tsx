import { useCallback, useState } from "react";
import { Button, ConfirmDialog, Pagination, Section, Table, useServerPage } from "../design";
import type { Column } from "../design/Table";
import { useApi } from "../api/context";
import { t } from "../i18n";
import { ApiErrorAlert } from "./ApiErrorAlert";
import { listAdminSessionPage, revokeAdminUserSessions, type AdminLiveSession } from "./api";
import { formatDateTime } from "./format";

// The "Live sessions" section of the user detail page (F5): the user's live
// desktop leases, read from Postgres, and the confirmed "Sign out everywhere".
export function UserSessionsSection({
  tenant,
  ownerRef,
  displayName,
}: {
  tenant: string;
  ownerRef: string;
  displayName: string;
}) {
  const api = useApi();
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<unknown>(null);
  const [result, setResult] = useState<string | null>(null);
  const load = useCallback(
    (cursor?: string) => listAdminSessionPage(api, tenant, ownerRef, cursor),
    [api, tenant, ownerRef],
  );
  const list = useServerPage(load, { sort: "-createdAt", key: `${tenant}|${ownerRef}` });

  const columns: Column<AdminLiveSession>[] = [
    { key: "workspace", header: t("admin.sessions.column.workspace"), rowHeader: true, render: (s) => s.workspaceName || s.workspaceId },
    { key: "started", header: t("admin.sessions.column.started"), render: (s) => formatDateTime(s.createdAt) },
    {
      key: "renewed",
      header: t("admin.sessions.column.lastActive"),
      hideOnMobile: true,
      render: (s) => (s.lastRenewedAt ? formatDateTime(s.lastRenewedAt) : "—"),
    },
    { key: "ref", header: t("admin.sessions.column.ref"), hideOnMobile: true, render: (s) => <code>{s.leaseRef}</code> },
  ];

  async function signOut() {
    setBusy(true);
    setActionError(null);
    try {
      const r = await revokeAdminUserSessions(api, tenant, ownerRef);
      setConfirming(false);
      setResult(t("admin.sessions.done", { sessions: r.sessions, leases: r.leases }));
      list.reset();
    } catch (e) {
      setActionError(e);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Section
      title={t("admin.sessions.title")}
      actions={
        <Button size="sm" variant="danger" onClick={() => { setActionError(null); setConfirming(true); }}>
          {t("admin.sessions.signOutAll")}
        </Button>
      }
    >
      <ApiErrorAlert error={list.error} onRetry={list.refresh} />
      <Table
        caption={t("admin.sessions.caption")}
        columns={columns}
        rows={list.items}
        rowKey={(s) => s.leaseRef}
        density="compact"
        loading={!list.loaded && !list.error}
        empty={t("admin.sessions.empty")}
      />
      <Pagination
        loaded={list.items.length}
        hasNext={list.hasNext}
        loading={list.loading}
        onLoadMore={list.next}
        label={t("common.pagination.label", { name: t("admin.sessions.caption") })}
      />
      {result ? <p role="status">{result}</p> : null}
      <ConfirmDialog
        open={confirming}
        title={t("admin.sessions.confirm.title", { name: displayName })}
        confirmLabel={t("admin.sessions.signOutAll")}
        cancelLabel={t("common.cancel")}
        busy={busy}
        onConfirm={() => void signOut()}
        onCancel={() => setConfirming(false)}
      >
        <p>{t("admin.sessions.confirm.body")}</p>
        <ApiErrorAlert error={actionError} />
      </ConfirmDialog>
    </Section>
  );
}
