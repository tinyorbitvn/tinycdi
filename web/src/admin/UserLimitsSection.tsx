import { useCallback, useEffect, useState } from "react";
import { Alert, Badge, Button, Dialog, Input, Pagination, Section, Spinner, Stat, Table, useServerTable } from "../design";
import type { Column } from "../design/Table";
import { t } from "../i18n";
import { useApi } from "../api/context";
import { isPortalApiError } from "../api/errors";
import {
  deleteAdminUserLimit,
  fetchAdminUserLimitDefault,
  listAdminUserLimitPage,
  putAdminUserLimit,
  putAdminUserLimitDefault,
  type AdminUserLimitDefault,
  type AdminUserLimitEntry,
  type Tagged,
} from "./api";
import { ApiErrorAlert } from "./ApiErrorAlert";
import { useLoader } from "../app/me";

// Editing target: the tenant default (with the ETag it was read under), or
// one principal's override (the entry carries its own `etag`).
export type UserLimitEditTarget = { kind: "default"; value: Tagged<AdminUserLimitDefault> } | AdminUserLimitEntry;

function isDefaultTarget(
  target: UserLimitEditTarget,
): target is { kind: "default"; value: Tagged<AdminUserLimitDefault> } {
  return "kind" in target && target.kind === "default";
}

// A blank field means "clear": no override (the tenant default applies) or
// no default (unlimited). Any other value must be a whole number >= 0.
function parseLimit(s: string): number | null | "invalid" {
  const v = s.trim();
  if (v === "") return null;
  const n = Number(v);
  if (!Number.isInteger(n) || n < 0) return "invalid";
  return n;
}

// Writes one edit under the target's ETag. An override cleared to blank is
// a DELETE (nothing to delete when the principal has no override row); a
// value is a PUT, "*" creating a row that does not exist yet.
async function saveLimit(
  api: ReturnType<typeof useApi>,
  tenant: string,
  target: UserLimitEditTarget,
  limit: number | null,
): Promise<void> {
  if (isDefaultTarget(target)) {
    await putAdminUserLimitDefault(api, tenant, limit, target.value.etag);
    return;
  }
  if (limit !== null) {
    await putAdminUserLimit(api, tenant, target.ownerRef, limit, target.etag);
  } else if (target.etag) {
    await deleteAdminUserLimit(api, tenant, target.ownerRef, target.etag);
  }
}

// Exported so the admin Users page edits a principal's limit through the
// same dialog — one form, one parse rule, one write path. A 412 (someone
// else wrote first) keeps the dialog open: onSaved reloads, the parent
// hands in the fresh target, and the field shows the current value.
export function LimitDialog({
  tenant,
  target,
  onClose,
  onSaved,
}: {
  tenant: string;
  target: UserLimitEditTarget;
  onClose: () => void;
  onSaved: () => void;
}) {
  const api = useApi();
  const isDefault = isDefaultTarget(target);
  const current = isDefault ? target.value.limit : target.limit;
  const [value, setValue] = useState("");
  const [busy, setBusy] = useState(false);
  const [saveError, setSaveError] = useState<unknown>(null);
  const [invalid, setInvalid] = useState(false);

  useEffect(() => {
    setValue(current == null ? "" : String(current));
    setInvalid(false);
  }, [target, current]);

  async function save(limit: number | null) {
    setBusy(true);
    setSaveError(null);
    try {
      await saveLimit(api, tenant, target, limit);
      onSaved();
      onClose();
    } catch (e) {
      setSaveError(e);
      if (isPortalApiError(e) && e.code === "PRECONDITION_FAILED") onSaved();
    } finally {
      setBusy(false);
    }
  }

  function onSave() {
    const parsed = parseLimit(value);
    if (parsed === "invalid") {
      setInvalid(true);
      return;
    }
    void save(parsed);
  }

  return (
    <Dialog
      open
      onClose={() => {
        if (!busy) onClose();
      }}
      size="sm"
      dismissible={!busy}
      title={
        isDefault
          ? t("admin.userLimits.edit.defaultTitle")
          : t("admin.userLimits.edit.userTitle", { user: target.displayName })
      }
      footer={
        <>
          <Button onClick={onClose} disabled={busy}>
            {t("common.cancel")}
          </Button>
          <Button variant="primary" loading={busy} onClick={onSave}>
            {t("admin.userLimits.edit.save")}
          </Button>
        </>
      }
    >
      <p className="tc-admin-muted">
        {isDefault
          ? t("admin.userLimits.edit.descriptionDefault")
          : t("admin.userLimits.edit.descriptionUser")}
      </p>
      <Input
        label={t("admin.userLimits.edit.field")}
        name="user-limit"
        type="number"
        min={0}
        step={1}
        value={value}
        onChange={(e) => setValue(e.target.value)}
      />
      {invalid ? <Alert tone="warning">{t("admin.userLimits.edit.invalid")}</Alert> : null}
      <ApiErrorAlert error={saveError} />
    </Dialog>
  );
}

// The collection sorts on ownerRef only (the `issuer|sub` reference).
const USER_LIMIT_SORT_FIELDS = { user: "ownerRef" } as const;

// What the dialog is open on: the default, or an override by ownerRef —
// resolved against the latest rows each render, so a reload after a 412
// hands the dialog the fresh value and ETag.
type Editing = "default" | { ownerRef: string };

// UserLimitsSection is the per-principal half of the admin quota page: the
// tenant default and each user's override, backed by
// /v1/admin/tenants/{tenant}/user-limits. It loads independently so a
// failure here never hides the tenant meters.
export function UserLimitsSection({ tenant }: { tenant: string }) {
  const api = useApi();
  const def = useLoader((a) => fetchAdminUserLimitDefault(a, tenant), `admin-user-limit-default:${tenant}`);
  const load = useCallback(
    (cursor?: string, sort?: string) => listAdminUserLimitPage(api, tenant, cursor, sort),
    [api, tenant],
  );
  const list = useServerTable(load, {
    fields: USER_LIMIT_SORT_FIELDS,
    initial: { key: "user", direction: "ascending" },
    key: tenant,
  });
  const [editing, setEditing] = useState<Editing | null>(null);

  const columns: Column<AdminUserLimitEntry>[] = [
    {
      key: "user",
      header: t("admin.quota.column.user"),
      rowHeader: true,
      sortable: true,
      render: (u) => (
        <span className="tc-admin-cell-stack">
          <span>{u.displayName}</span>
          <code className="tc-admin-muted">{u.subject}</code>
        </span>
      ),
    },
    {
      key: "running",
      header: t("admin.userLimits.column.running"),
      align: "end",
      render: (u) => u.running,
    },
    {
      key: "limit",
      header: t("admin.userLimits.column.limit"),
      align: "end",
      render: (u) =>
        u.limit == null ? (
          <Badge tone="neutral">{t("admin.userLimits.inherit")}</Badge>
        ) : (
          u.limit
        ),
    },
    {
      key: "effective",
      header: t("admin.userLimits.column.effective"),
      align: "end",
      render: (u) => (u.effective == null ? t("admin.userLimits.unlimited") : u.effective),
    },
    {
      key: "actions",
      header: "",
      align: "end",
      render: (u) => (
        <Button size="sm" variant="secondary" onClick={() => setEditing({ ownerRef: u.ownerRef })}>
          {t("admin.userLimits.edit.action")}
        </Button>
      ),
    },
  ];

  const target: UserLimitEditTarget | null =
    editing === null
      ? null
      : editing === "default"
        ? def.data
          ? { kind: "default", value: def.data }
          : null
        : (list.items.find((u) => u.ownerRef === editing.ownerRef) ?? null);

  const reloadAll = () => {
    def.reload();
    list.refresh();
  };

  return (
    <Section
      title={t("admin.userLimits.title")}
      description={t("admin.userLimits.description")}
      actions={
        <Button size="sm" variant="secondary" disabled={!def.data} onClick={() => setEditing("default")}>
          {t("admin.userLimits.editDefault")}
        </Button>
      }
    >
      <div className="tc-stat-grid tc-userlimits-default">
        <Stat
          label={t("admin.userLimits.default")}
          value={def.data ? (def.data.limit == null ? t("admin.userLimits.unlimited") : def.data.limit) : null}
        />
      </div>
      <ApiErrorAlert error={def.error} onRetry={def.reload} />
      <ApiErrorAlert error={list.error} onRetry={list.refresh} />
      {list.loaded ? (
        <>
          <Table
            caption={t("admin.userLimits.caption")}
            columns={columns}
            rows={list.items}
            rowKey={(u) => u.ownerRef}
            sort={list.tableSort}
            onSortChange={list.onSortChange}
            density="compact"
            empty={t("admin.userLimits.empty")}
          />
          <Pagination
            loaded={list.items.length}
            hasNext={list.hasNext}
            loading={list.loading}
            onLoadMore={list.next}
            label={t("common.pagination.label", { name: t("admin.userLimits.caption") })}
          />
        </>
      ) : list.loading ? (
        <Spinner label={t("admin.userLimits.loading")} />
      ) : null}
      {target !== null ? (
        <LimitDialog tenant={tenant} target={target} onClose={() => setEditing(null)} onSaved={reloadAll} />
      ) : null}
    </Section>
  );
}
