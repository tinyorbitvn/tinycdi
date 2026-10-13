import { useCallback, useMemo, useState, type ReactNode } from "react";
import {
  Alert,
  Button,
  Cluster,
  ConfirmDialog,
  Dialog,
  IconButton,
  Input,
  Menu,
  Pagination,
  Select,
  StatusPill,
  Table,
  useServerTable,
} from "../design";
import type { Column } from "../design/Table";
import type { MenuItem } from "../design";
import {
  IconDeviceDesktop,
  IconDots,
  IconPlayerPlay,
  IconPlayerStop,
  IconPower,
  IconRefresh,
  IconSearch,
  IconTrash,
} from "../design/icons";
import { useApi } from "../api/context";
import { newIdempotencyKey, unwrap } from "../api/client";
import { Link, navigate } from "../lib/router";
import { t } from "../i18n";
import { isConnectable, phaseLabelKey, sessionPath } from "../workspaces/helpers";
import {
  listScopedWorkspacePage,
  restartWorkspace,
  startWorkspace,
  stopWorkspace,
  RestartAbortedError,
  RestartTimeoutError,
  type RestartStep,
  type ScopedWorkspace,
  type WorkspacePhase,
} from "./api";
import { formatAge, formatDateTime, ownerLabel } from "./format";
import { UNRECOVERABLE_FAILURES } from "../workspaces/reasons";
import { useMeLoaded } from "../app/me";
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

// Start is offered for the two terminal phases the API accepts (Failed only
// when a new generation can fix its reason — F1); restart is
// stop -> wait Stopped -> start and only makes sense while the runtime is
// (or is becoming) active. Stop/delete follow the same phase gates the
// API re-checks (it answers INVALID_STATE on a race).
export function canStart(ws: ScopedWorkspace): boolean {
  if (ws.phase === "Failed") return !UNRECOVERABLE_FAILURES.has(ws.failureReason ?? "");
  return ws.phase === "Stopped";
}

export function canStop(ws: ScopedWorkspace): boolean {
  return ws.desiredState === "Running" && ws.phase !== "Stopping" && ws.phase !== "Terminating";
}

// A Failed workspace is already not running — Start is the retry verb, so
// Restart is reserved for rows that actually have a runtime to bounce.
export function canRestart(ws: ScopedWorkspace): boolean {
  return canStop(ws) && ws.phase !== "Failed";
}

export function canDelete(ws: ScopedWorkspace): boolean {
  return ws.phase !== "Terminating";
}

// Connect is owner-only (advisor Q1): a tenant admin never gets a session
// on another user's workspace — no takeover in v1.2.
export function canConnect(ws: ScopedWorkspace, viewerSubject: string | undefined): boolean {
  return !!viewerSubject && ws.owner?.subject === viewerSubject && isConnectable(ws);
}

export function matchesFilter(ws: ScopedWorkspace, text: string): boolean {
  const q = text.trim().toLowerCase();
  if (!q) return true;
  return [ws.name, ws.id, ws.template.name, ws.owner?.displayName ?? "", ws.owner?.subject ?? ""].some(
    (v) => v.toLowerCase().includes(q),
  );
}

type Pending = { kind: "start" | "stop" | "restart" | "delete"; ws: ScopedWorkspace };

// Static keys so the unused-key catalog check can grep each one.
// Columns the server sorts on (GET /v1/workspaces `sort`).
const WORKSPACE_SORT_FIELDS = { name: "name", age: "createdAt" } as const;

const NOTICE_KEYS = {
  start: "admin.workspaces.notice.start",
  stop: "admin.workspaces.notice.stop",
  restart: "admin.workspaces.notice.restart",
  delete: "admin.workspaces.notice.delete",
} as const;

interface RowAction {
  id: string;
  label: string;
  ariaLabel: string;
  icon: ReactNode;
  danger?: boolean;
  disabled?: boolean;
  onSelect: () => void;
}

export function WorkspacesAdminPage({ now }: { now?: number }) {
  const api = useApi();
  const me = useMeLoaded();
  const [phase, setPhase] = useState<WorkspacePhase | "">("");
  const [filter, setFilter] = useState("");
  const load = useCallback(
    (cursor?: string, sort?: string) => listScopedWorkspacePage(api, "tenant", phase || undefined, cursor, sort),
    [api, phase],
  );
  // Newest first (the server default); a phase change restarts the list.
  const list = useServerTable(load, {
    fields: WORKSPACE_SORT_FIELDS,
    initial: { key: "age", direction: "descending" },
    key: phase,
  });
  const [pending, setPending] = useState<Pending | null>(null);
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [restartStep, setRestartStep] = useState<RestartStep | null>(null);
  const [actionError, setActionError] = useState<unknown>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const viewerSubject = me.data?.subject;
  // The search narrows the rows loaded so far.
  const rows = useMemo(() => list.items.filter((ws) => matchesFilter(ws, filter)), [list.items, filter]);

  function open(kind: Pending["kind"], ws: ScopedWorkspace) {
    setPending({ kind, ws });
    setTyped("");
    setRestartStep(null);
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
      if (kind === "start") {
        await startWorkspace(api, ws.id);
      } else if (kind === "stop") {
        await stopWorkspace(api, ws.id);
      } else if (kind === "restart") {
        await restartWorkspace(api, ws.id, { onStep: setRestartStep });
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
        t(NOTICE_KEYS[kind], {
          name: ws.name,
          owner: ownerLabel(ws.owner),
        }),
      );
      setPending(null);
      list.refresh();
    } catch (e) {
      setActionError(e);
    } finally {
      setBusy(false);
    }
  }

  // Everything a row can do; rendered inline at <= 2 actions and folded
  // into the IconDots menu above that, so busy rows stay one control wide.
  function rowActions(ws: ScopedWorkspace): RowAction[] {
    const actions: RowAction[] = [];
    if (canConnect(ws, viewerSubject)) {
      actions.push({
        id: "connect",
        label: t("admin.workspaces.action.connect"),
        ariaLabel: t("admin.workspaces.action.connectLabel", { name: ws.name }),
        icon: <IconDeviceDesktop />,
        onSelect: () => navigate(sessionPath(ws.id)),
      });
    }
    if (canStart(ws)) {
      actions.push({
        id: "start",
        label: t("admin.workspaces.action.start"),
        ariaLabel: t("admin.workspaces.action.startLabel", { name: ws.name }),
        icon: <IconPlayerPlay />,
        onSelect: () => open("start", ws),
      });
    }
    if (canStop(ws)) {
      actions.push({
        id: "stop",
        label: t("admin.workspaces.action.stop"),
        ariaLabel: t("admin.workspaces.action.stopLabel", { name: ws.name }),
        icon: <IconPlayerStop />,
        onSelect: () => open("stop", ws),
      });
    }
    if (canRestart(ws)) {
      actions.push({
        id: "restart",
        label: t("admin.workspaces.action.restart"),
        ariaLabel: t("admin.workspaces.action.restartLabel", { name: ws.name }),
        icon: <IconPower />,
        onSelect: () => open("restart", ws),
      });
    }
    actions.push({
      id: "delete",
      label: t("admin.workspaces.action.delete"),
      ariaLabel: t("admin.workspaces.action.deleteLabel", { name: ws.name }),
      icon: <IconTrash />,
      danger: true,
      disabled: !canDelete(ws),
      onSelect: () => open("delete", ws),
    });
    return actions;
  }

  const ts = now ?? Date.now();
  const columns: Column<ScopedWorkspace>[] = [
    {
      key: "name",
      header: t("admin.workspaces.column.workspace"),
      rowHeader: true,
      sortable: true,
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
      sortable: true,
      sortStart: "descending",
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
      render: (ws) => {
        const actions = rowActions(ws);
        if (actions.length > 2) {
          const items: MenuItem[] = actions.map((a) => ({
            id: a.id,
            label: a.label,
            icon: a.icon,
            ...(a.danger ? { danger: true } : {}),
            ...(a.disabled ? { disabled: true } : {}),
            ...(a.danger ? { separatorBefore: true } : {}),
            onSelect: a.onSelect,
          }));
          return (
            <Menu
              align="end"
              trigger={(p) => (
                <IconButton
                  {...p}
                  size="sm"
                  label={t("admin.workspaces.action.menuLabel", { name: ws.name })}
                  icon={<IconDots />}
                />
              )}
              items={items}
            />
          );
        }
        return (
          <Cluster gap={2} justify="end" wrap={false}>
            {actions.map((a) => (
              <Button
                key={a.id}
                size="sm"
                variant={a.danger ? "danger" : "secondary"}
                icon={a.icon}
                disabled={a.disabled}
                aria-label={a.ariaLabel}
                onClick={a.onSelect}
              >
                {a.label}
              </Button>
            ))}
          </Cluster>
        );
      },
    },
  ];

  const deleteConfirmed = pending?.kind === "delete" && typed === pending.ws.name;
  const actingOn = pending ? ownerLabel(pending.ws.owner) : "";
  const restartProgressKey =
    restartStep === "stopping"
      ? ("admin.workspaces.restart.progress.stopping" as const)
      : restartStep === "waiting"
        ? ("admin.workspaces.restart.progress.waiting" as const)
        : ("admin.workspaces.restart.progress.starting" as const);

  return (
    <AdminLayout
      title={t("admin.workspaces.title")}
      description={t("admin.workspaces.description")}
      actions={
        <Button icon={<IconRefresh />} onClick={list.refresh} loading={list.loading && list.loaded}>
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
          icon={<IconSearch />}
          onClear={() => setFilter("")}
        />
        <Select
          label={t("admin.workspaces.phase.label")}
          value={phase}
          onChange={(e) => setPhase(e.target.value as WorkspacePhase | "")}
          options={[
            { value: "", label: t("admin.workspaces.phase.all") },
            ...PHASES.map((p) => ({ value: p, label: t(phaseLabelKey(p)) })),
          ]}
        />
      </Cluster>
      {notice ? (
        <Alert tone="success" onDismiss={() => setNotice(null)}>
          {notice}
        </Alert>
      ) : null}
      <ApiErrorAlert error={list.error} onRetry={list.refresh} />
      <Table
        caption={t("admin.workspaces.caption")}
        columns={columns}
        rows={rows}
        rowKey={(ws) => ws.id}
        sort={list.tableSort}
        onSortChange={list.onSortChange}
        loading={!list.loaded && !list.error}
        empty={
          filter || phase ? t("admin.workspaces.empty.filtered") : t("admin.workspaces.empty.all")
        }
      />
      {filter && list.hasNext ? (
        <p className="tc-admin-muted" role="status">
          {t("common.filter.partial", { n: list.items.length })}
        </p>
      ) : null}
      <Pagination
        loaded={list.items.length}
        hasNext={list.hasNext}
        loading={list.loading}
        onLoadMore={list.next}
        label={t("common.pagination.label", { name: t("admin.workspaces.caption") })}
      />

      <ConfirmDialog
        open={pending?.kind === "start"}
        title={
          pending ? t("admin.workspaces.start.title", { name: pending.ws.name }) : t("admin.workspaces.start.fallbackTitle")
        }
        confirmLabel={t("admin.workspaces.start.confirm")}
        cancelLabel={t("admin.workspaces.dialog.cancel")}
        destructive={false}
        busy={busy}
        onConfirm={() => void run()}
        onCancel={close}
      >
        {pending ? (
          <>
            <p className="tc-admin-muted">{t("admin.workspaces.actingAs", { owner: actingOn })}</p>
            <p>{t("admin.workspaces.start.body", { owner: actingOn })}</p>
            <ApiErrorAlert error={actionError} />
          </>
        ) : null}
      </ConfirmDialog>

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
            <p className="tc-admin-muted">{t("admin.workspaces.actingAs", { owner: actingOn })}</p>
            <p>
              {t("admin.workspaces.stop.body")}{" "}
              {pending.ws.dataPolicy === "Retain"
                ? t("admin.workspaces.stop.retain")
                : t("admin.workspaces.stop.ephemeral")}
            </p>
            <ApiErrorAlert error={actionError} />
          </>
        ) : null}
      </ConfirmDialog>

      <ConfirmDialog
        open={pending?.kind === "restart"}
        title={
          pending ? t("admin.workspaces.restart.title", { name: pending.ws.name }) : t("admin.workspaces.restart.fallbackTitle")
        }
        confirmLabel={t("admin.workspaces.restart.confirm")}
        cancelLabel={t("admin.workspaces.dialog.cancel")}
        destructive={false}
        busy={busy}
        onConfirm={() => void run()}
        onCancel={close}
      >
        {pending ? (
          <>
            <p className="tc-admin-muted">{t("admin.workspaces.actingAs", { owner: actingOn })}</p>
            <p>
              {t("admin.workspaces.restart.body")}{" "}
              {t("admin.workspaces.restart.quota", { owner: actingOn })}
            </p>
            {busy && restartStep ? (
              <p className="tc-admin-muted" aria-live="polite">
                {t(restartProgressKey, { name: pending.ws.name })}
              </p>
            ) : null}
            {actionError instanceof RestartTimeoutError ? (
              <Alert tone="danger">{t("admin.workspaces.restart.timeout")}</Alert>
            ) : actionError instanceof RestartAbortedError ? (
              <Alert tone="danger">{t("admin.workspaces.restart.aborted")}</Alert>
            ) : (
              <ApiErrorAlert error={actionError} />
            )}
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
            <p className="tc-admin-muted">{t("admin.workspaces.actingAs", { owner: actingOn })}</p>
            <p>
              {t("admin.workspaces.delete.body")}{" "}
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
