import { useMemo, useState } from "react";
import { Alert, Button, Cluster, ConfirmDialog, Dialog, Input, Select, StatusPill, Table } from "../design";
import type { Column } from "../design/Table";
import { IconRefresh, IconStop, IconTrash } from "../design/icons";
import { useApi } from "../api/context";
import { newIdempotencyKey, unwrap } from "../api/client";
import { Link } from "../lib/router";
import { t } from "../i18n";
import { phaseLabelKey } from "../workspaces/helpers";
import { listScopedWorkspaces, type ScopedWorkspace, type WorkspacePhase } from "./api";
import { formatAge, formatDateTime, ownerLabel } from "./format";
import { useLoader } from "./hooks";
import { ApiErrorAlert } from "./ApiErrorAlert";
import { AdminLayout } from "./AdminLayout";

const PHASES: WorkspacePhase[] = [
  "Pending",
  "Provisioning",
  "Ready",
  "Stopping",
  "Stopped",
  "Failed",
  "Terminating",
];

// Stop is offered while the runtime is (or is becoming) active; delete for
// anything not already being deleted. The API re-checks and answers
// INVALID_STATE on a race.
export function canStop(ws: ScopedWorkspace): boolean {
  return ws.desiredState === "Running" && ws.phase !== "Stopping" && ws.phase !== "Terminating";
}

export function canDelete(ws: ScopedWorkspace): boolean {
  return ws.phase !== "Terminating";
}

export function matchesFilter(ws: ScopedWorkspace, text: string): boolean {
  const q = text.trim().toLowerCase();
  if (!q) return true;
  return [ws.name, ws.id, ws.template.name, ws.owner?.displayName ?? "", ws.owner?.subject ?? ""].some(
    (v) => v.toLowerCase().includes(q),
  );
}

type Pending = { kind: "stop" | "delete"; ws: ScopedWorkspace };

export function WorkspacesAdminPage({ now }: { now?: number }) {
  const api = useApi();
  const [phase, setPhase] = useState<WorkspacePhase | "">("");
  const [filter, setFilter] = useState("");
  const list = useLoader((a) => listScopedWorkspaces(a, "tenant", phase || undefined), `ws:${phase}`);
  const [pending, setPending] = useState<Pending | null>(null);
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<unknown>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const rows = useMemo(
    () => (list.data?.items ?? []).filter((ws) => matchesFilter(ws, filter)),
    [list.data, filter],
  );

  function open(kind: Pending["kind"], ws: ScopedWorkspace) {
    setPending({ kind, ws });
    setTyped("");
    setActionError(null);
  }

  function close() {
    if (busy) return;
    setPending(null);
    setActionError(null);
  }

  async function run() {
    if (!pending) return;
    const { kind, ws } = pending;
    setBusy(true);
    setActionError(null);
    try {
      if (kind === "stop") {
        unwrap(
          await api.POST("/v1/workspaces/{workspaceId}/stop", {
            params: {
              path: { workspaceId: ws.id },
              header: { "Idempotency-Key": newIdempotencyKey() },
            },
          }),
        );
      } else {
        unwrap(
          await api.DELETE("/v1/workspaces/{workspaceId}", {
            params: {
              path: { workspaceId: ws.id },
              header: { "Idempotency-Key": newIdempotencyKey() },
            },
          }),
        );
      }
      setNotice(
        t(kind === "stop" ? "admin.workspaces.notice.stop" : "admin.workspaces.notice.delete", {
          name: ws.name,
          owner: ownerLabel(ws.owner),
        }),
      );
      setPending(null);
      list.reload();
    } catch (e) {
      setActionError(e);
    } finally {
      setBusy(false);
    }
  }

  const ts = now ?? Date.now();
  const columns: Column<ScopedWorkspace>[] = [
    {
      key: "name",
      header: t("admin.workspaces.column.workspace"),
      rowHeader: true,
      render: (ws) => (
        <span className="tc-admin-cell-stack">
          <Link to={`/workspaces/${ws.id}`}>{ws.name}</Link>
          <code className="tc-admin-muted">{ws.id}</code>
        </span>
      ),
    },
    {
      key: "owner",
      header: t("admin.workspaces.column.owner"),
      render: (ws) => ownerLabel(ws.owner),
    },
    {
      key: "template",
      header: t("admin.workspaces.column.template"),
      hideOnMobile: true,
      render: (ws) => `${ws.template.name} ${t("admin.templates.revision", { n: ws.template.revision })}`,
    },
    {
      key: "phase",
      header: t("admin.workspaces.column.phase"),
      render: (ws) => (
        <span className="tc-admin-cell-stack">
          <StatusPill phase={ws.phase} label={t(phaseLabelKey(ws.phase))} />
          {ws.phase === "Failed" && ws.failureReason ? (
            <span className="tc-admin-muted">{ws.failureReason}</span>
          ) : null}
        </span>
      ),
    },
    {
      key: "age",
      header: t("admin.workspaces.column.age"),
      align: "end",
      hideOnMobile: true,
      render: (ws) => (
        <time dateTime={ws.createdAt} title={formatDateTime(ws.createdAt)}>
          {formatAge(ws.createdAt, ts)}
        </time>
      ),
    },
    {
      key: "actions",
      header: <span className="tc-sr-only">{t("admin.workspaces.column.actions")}</span>,
      align: "end",
      render: (ws) => (
        <Cluster gap={2} justify="end" wrap={false}>
          <Button
            size="sm"
            icon={<IconStop />}
            disabled={!canStop(ws)}
            aria-label={t("admin.workspaces.action.stopLabel", { name: ws.name })}
            onClick={() => open("stop", ws)}
          >
            {t("admin.workspaces.action.stop")}
          </Button>
          <Button
            size="sm"
            variant="danger"
            icon={<IconTrash />}
            disabled={!canDelete(ws)}
            aria-label={t("admin.workspaces.action.deleteLabel", { name: ws.name })}
            onClick={() => open("delete", ws)}
          >
            {t("admin.workspaces.action.delete")}
          </Button>
        </Cluster>
      ),
    },
  ];

  const deleteConfirmed = pending?.kind === "delete" && typed === pending.ws.name;

  return (
    <AdminLayout
      title={t("admin.workspaces.title")}
      description={t("admin.workspaces.description")}
      actions={
        <Button icon={<IconRefresh />} onClick={list.reload} loading={list.loading && !!list.data}>
          {t("admin.action.refresh")}
        </Button>
      }
    >
      <Cluster gap={4} align="end" className="tc-admin-toolbar">
        <Input
          label={t("admin.workspaces.filter.label")}
          type="search"
          placeholder={t("admin.workspaces.filter.placeholder")}
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
        />
        <Select
          label={t("admin.workspaces.phase.label")}
          value={phase}
          onChange={(e) => setPhase(e.target.value as WorkspacePhase | "")}
          options={[
            { value: "", label: t("admin.workspaces.phase.all") },
            ...PHASES.map((p) => ({ value: p, label: p })),
          ]}
        />
      </Cluster>
      {notice ? (
        <Alert tone="success" onDismiss={() => setNotice(null)}>
          {notice}
        </Alert>
      ) : null}
      <ApiErrorAlert error={list.error} onRetry={list.reload} />
      {list.data?.truncated ? (
        <Alert tone="warning">
          {t("admin.workspaces.truncated", { n: list.data.items.length })}
        </Alert>
      ) : null}
      <Table
        caption={t("admin.workspaces.caption")}
        columns={columns}
        rows={rows}
        rowKey={(ws) => ws.id}
        loading={list.loading && !list.data}
        empty={
          filter || phase ? t("admin.workspaces.empty.filtered") : t("admin.workspaces.empty.all")
        }
      />
      <p className="tc-admin-muted" aria-live="polite">
        {list.data
          ? t("admin.workspaces.shown", { shown: rows.length, total: list.data.items.length })
          : ""}
      </p>

      <ConfirmDialog
        open={pending?.kind === "stop"}
        title={
          pending ? t("admin.workspaces.stop.title", { name: pending.ws.name }) : t("admin.workspaces.stop.fallbackTitle")
        }
        confirmLabel={t("admin.workspaces.stop.confirm")}
        cancelLabel={t("admin.workspaces.dialog.cancel")}
        destructive={false}
        busy={busy}
        onConfirm={() => void run()}
        onCancel={close}
      >
        {pending ? (
          <>
            <p>
              {t("admin.workspaces.stop.body", { owner: ownerLabel(pending.ws.owner) })}{" "}
              {pending.ws.dataPolicy === "Retain"
                ? t("admin.workspaces.stop.retain")
                : t("admin.workspaces.stop.ephemeral")}
            </p>
            <ApiErrorAlert error={actionError} />
          </>
        ) : null}
      </ConfirmDialog>

      <Dialog
        open={pending?.kind === "delete"}
        onClose={close}
        role="alertdialog"
        size="sm"
        dismissible={!busy}
        title={
          pending ? t("admin.workspaces.delete.title", { name: pending.ws.name }) : t("admin.workspaces.delete.fallbackTitle")
        }
        footer={
          <>
            <Button onClick={close} disabled={busy} data-autofocus>
              {t("admin.workspaces.dialog.cancel")}
            </Button>
            <Button variant="danger" loading={busy} disabled={!deleteConfirmed} onClick={() => void run()}>
              {t("admin.workspaces.delete.confirm")}
            </Button>
          </>
        }
      >
        {pending ? (
          <>
            <p>
              {t("admin.workspaces.delete.body", { owner: ownerLabel(pending.ws.owner) })}{" "}
              {pending.ws.dataPolicy === "Retain"
                ? t("admin.workspaces.delete.retain")
                : t("admin.workspaces.delete.ephemeral")}
            </p>
            <Input
              label={t("admin.workspaces.delete.typeConfirm", { name: pending.ws.name })}
              name="confirm-delete"
              autoComplete="off"
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
            />
            <ApiErrorAlert error={actionError} />
          </>
        ) : null}
      </Dialog>
    </AdminLayout>
  );
}
