import { useEffect, useState } from "react";
import { Alert, Badge, Button, Dialog, Input, Section, Spinner, Table } from "../design";
import type { Column } from "../design/Table";
import { t } from "../i18n";
import { useApi } from "../api/context";
import {
  fetchAdminUserLimits,
  putAdminUserLimit,
  putAdminUserLimitDefault,
  type AdminUserLimitEntry,
} from "./api";
import { ApiErrorAlert } from "./ApiErrorAlert";
import { useLoader } from "../app/me";

// Editing target: the tenant default, or one principal's override.
type EditTarget = "default" | AdminUserLimitEntry;

// A blank field means "clear": no override (the tenant default applies) or
// no default (unlimited). Any other value must be a whole number >= 0.
function parseLimit(s: string): number | null | "invalid" {
  const v = s.trim();
  if (v === "") return null;
  const n = Number(v);
  if (!Number.isInteger(n) || n < 0) return "invalid";
  return n;
}

function LimitDialog({
  tenant,
  target,
  onClose,
  onSaved,
}: {
  tenant: string;
  target: EditTarget;
  onClose: () => void;
  onSaved: () => void;
}) {
  const api = useApi();
  const isDefault = target === "default";
  const [value, setValue] = useState("");
  const [busy, setBusy] = useState(false);
  const [saveError, setSaveError] = useState<unknown>(null);
  const [invalid, setInvalid] = useState(false);

  useEffect(() => {
    const cur = isDefault ? null : (target as AdminUserLimitEntry).limit;
    setValue(cur == null ? "" : String(cur));
    setInvalid(false);
    setSaveError(null);
  }, [target, isDefault]);

  const entry = isDefault ? null : (target as AdminUserLimitEntry);

  async function save(limit: number | null) {
    setBusy(true);
    setSaveError(null);
    try {
      if (isDefault) {
        await putAdminUserLimitDefault(api, tenant, limit);
      } else {
        await putAdminUserLimit(api, tenant, entry!.ownerRef, limit);
      }
      onSaved();
      onClose();
    } catch (e) {
      setSaveError(e);
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
          : t("admin.userLimits.edit.userTitle", { user: entry!.displayName })
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

// UserLimitsSection is the per-principal half of the admin quota page: the
// tenant default and each user's override, backed by
// /v1/admin/tenants/{tenant}/user-limits (v0.5). It loads independently so
// a failure here never hides the tenant meters.
export function UserLimitsSection({ tenant }: { tenant: string }) {
  const q = useLoader((api) => fetchAdminUserLimits(api, tenant), "admin-user-limits");
  const [editing, setEditing] = useState<EditTarget | null>(null);

  const columns: Column<AdminUserLimitEntry>[] = [
    {
      key: "user",
      header: t("admin.quota.column.user"),
      rowHeader: true,
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
        <Button size="sm" variant="secondary" onClick={() => setEditing(u)}>
          {t("admin.userLimits.edit.action")}
        </Button>
      ),
    },
  ];

  return (
    <Section
      title={t("admin.userLimits.title")}
      description={t("admin.userLimits.description")}
      actions={
        <Button size="sm" variant="secondary" onClick={() => setEditing("default")}>
          {t("admin.userLimits.editDefault")}
        </Button>
      }
    >
      <p>
        <span className="tc-admin-muted">{t("admin.userLimits.default")}</span>{" "}
        {q.data?.default == null ? t("admin.userLimits.unlimited") : q.data.default}
      </p>
      <ApiErrorAlert error={q.error} onRetry={q.reload} />
      {q.data ? (
        <Table
          caption={t("admin.userLimits.caption")}
          columns={columns}
          rows={q.data.users}
          rowKey={(u) => u.ownerRef}
          density="compact"
          empty={t("admin.userLimits.empty")}
        />
      ) : q.loading ? (
        <Spinner label={t("admin.userLimits.loading")} />
      ) : null}
      {editing !== null && q.data ? (
        <LimitDialog
          tenant={tenant}
          target={editing}
          onClose={() => setEditing(null)}
          onSaved={q.reload}
        />
      ) : null}
    </Section>
  );
}
