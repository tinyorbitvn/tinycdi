import { useState } from "react";
import { Alert, Badge, Button, Card, Dialog, Grid, Input, Meter, Section, Spinner, Table } from "../design";
import type { Column } from "../design/Table";
import { IconChevronDown, IconChevronUp, IconRefresh } from "../design/icons";
import { cx } from "../design/cx";
import { t } from "../i18n";
import type { ApiClient } from "../api/client";
import { useApi } from "../api/context";
import {
  fetchAdminQuota,
  putAdminQuota,
  type AdminQuotaLimits,
  type AdminQuotaView,
  type QuotaAmounts,
  type QuotaSource,
  type UserUsage,
} from "./api";
import {
  QUOTA_KEYS,
  formatQuota,
  quotaLabel,
  usageLevel,
  usagePercent,
  type QuotaKey,
} from "./format";
import { loadMe, useLoader } from "../app/me";
import { ApiErrorAlert } from "./ApiErrorAlert";
import { AdminLayout } from "./AdminLayout";

// A workspace-count limit of 0 in a configured quota means "no limit" — never
// a meter against zero. A tenant without a quota row is not unlimited: it is
// refused every create (see QuotaMeters).
export function isUnlimited(k: QuotaKey, limit: number | undefined): boolean {
  return limit === undefined || (k === "workspaces" && limit === 0);
}

export function QuotaMeters({
  configured,
  limits,
  usage,
}: {
  configured: boolean;
  limits: Partial<QuotaAmounts> | undefined;
  usage: QuotaAmounts;
}) {
  if (!configured) {
    return <Alert tone="warning">{t("admin.quota.notConfigured")}</Alert>;
  }
  return (
    <Grid gap={4} min="sm">
      {QUOTA_KEYS.map((k) => {
        const limit = limits?.[k];
        if (limit === undefined || isUnlimited(k, limit)) {
          return (
            <div key={k} className="tc-meter">
              <div className="tc-meter__header">
                <span className="tc-meter__label">{quotaLabel(k)}</span>
                <span className="tc-meter__value">
                  {t("admin.quota.noLimit", { used: formatQuota(k, usage[k]) })}
                </span>
              </div>
            </div>
          );
        }
        return (
          <Meter
            key={k}
            label={quotaLabel(k)}
            value={usage[k]}
            max={limit}
            valueText={t("admin.quota.meter", {
              used: formatQuota(k, usage[k]),
              limit: formatQuota(k, limit),
              pct: usagePercent(usage[k], limit),
            })}
          />
        );
      })}
    </Grid>
  );
}

function UsageCell({ k, value, limit }: { k: QuotaKey; value: number; limit: number | undefined }) {
  if (limit === undefined || isUnlimited(k, limit)) return <>{formatQuota(k, value)}</>;
  const level = usageLevel(value, limit);
  return (
    <span className="tc-admin-usage">
      <span>
        {formatQuota(k, value)} <span className="tc-admin-muted">/ {formatQuota(k, limit)}</span>
      </span>
      {level === "full" ? (
        <Badge tone="danger">{t("admin.quota.atLimit")}</Badge>
      ) : level === "warn" ? (
        <Badge tone="warning">{usagePercent(value, limit)}%</Badge>
      ) : null}
    </span>
  );
}

export type UsageSortKey = "user" | QuotaKey;

export interface UsageSort {
  key: UsageSortKey;
  dir: 1 | -1;
}

// Default order: heaviest users first — running workspaces, then storage.
export const DEFAULT_USAGE_SORT: UsageSort = { key: "runningWorkspaces", dir: -1 };

export function sortedUsers(users: readonly UserUsage[], sort: UsageSort): UserUsage[] {
  const dir = sort.dir;
  return [...users].sort((a, b) => {
    const primary =
      sort.key === "user"
        ? a.displayName.localeCompare(b.displayName)
        : a.usage[sort.key] - b.usage[sort.key];
    return (
      primary * dir ||
      b.usage.runningWorkspaces - a.usage.runningWorkspaces ||
      a.displayName.localeCompare(b.displayName) ||
      a.subject.localeCompare(b.subject)
    );
  });
}

function SortHeader({
  label,
  columnKey,
  sort,
  onSort,
}: {
  label: string;
  columnKey: UsageSortKey;
  sort: UsageSort;
  onSort: (key: UsageSortKey) => void;
}) {
  const active = sort.key === columnKey;
  return (
    <button
      type="button"
      className={cx("tc-admin-sort", active && "tc-admin-sort--active")}
      aria-pressed={active}
      onClick={() => onSort(columnKey)}
    >
      {label}
      {active ? (
        sort.dir === 1 ? (
          <IconChevronUp size={14} aria-hidden="true" />
        ) : (
          <IconChevronDown size={14} aria-hidden="true" />
        )
      ) : null}
    </button>
  );
}

// The limits row's owner: config rows are read-only here — only the
// platform configuration may change them — api/none rows are writable.
function SourceBadge({ source }: { source: QuotaSource }) {
  switch (source) {
    case "config":
      return <Badge tone="info">{t("admin.quota.source.config")}</Badge>;
    case "api":
      return <Badge tone="neutral">{t("admin.quota.source.api")}</Badge>;
    default:
      return <Badge tone="warning">{t("admin.quota.source.none")}</Badge>;
  }
}

interface EditFields {
  runningWorkspaces: string;
  cpu: string;
  memory: string;
  storage: string;
}

// The edit dialog speaks in the units the meters render — vCPU for CPU,
// GiB for memory and storage — and converts back to the contract's
// millicores/MiB on save.
function fieldsOf(quota: AdminQuotaView): EditFields {
  const l = quota.limits;
  return {
    runningWorkspaces: l ? String(l.runningWorkspaces) : "",
    cpu: l ? String(l.cpuMillicores / 1000) : "",
    memory: l ? String(l.memoryMib / 1024) : "",
    storage: l ? String(l.storageGib) : "",
  };
}

function parseFields(f: EditFields): AdminQuotaLimits | null {
  const num = (s: string) => (s.trim() === "" ? NaN : Number(s));
  const slots = num(f.runningWorkspaces);
  const cpuMillis = num(f.cpu) * 1000;
  const memoryMib = num(f.memory) * 1024;
  const storageGib = num(f.storage);
  const ints = [slots, cpuMillis, memoryMib, storageGib];
  if (ints.some((n) => !Number.isInteger(n) || n < 0)) return null;
  return {
    runningWorkspaces: slots,
    cpuMillicores: cpuMillis,
    memoryMib,
    storageGib,
  };
}

export function QuotaEditDialog({
  quota,
  open,
  onClose,
  onSaved,
}: {
  quota: AdminQuotaView;
  open: boolean;
  onClose: () => void;
  onSaved: () => void;
}) {
  const api = useApi();
  const [fields, setFields] = useState<EditFields>(() => fieldsOf(quota));
  const [busy, setBusy] = useState(false);
  const [saveError, setSaveError] = useState<unknown>(null);
  const [invalid, setInvalid] = useState(false);

  function set(k: keyof EditFields) {
    return (e: React.ChangeEvent<HTMLInputElement>) =>
      setFields((f) => ({ ...f, [k]: e.target.value }));
  }

  async function save() {
    const limits = parseFields(fields);
    if (!limits) {
      setInvalid(true);
      return;
    }
    setInvalid(false);
    setBusy(true);
    setSaveError(null);
    try {
      await putAdminQuota(api, quota.tenant, limits);
      onSaved();
      onClose();
    } catch (e) {
      setSaveError(e);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog
      open={open}
      onClose={() => {
        if (!busy) onClose();
      }}
      size="sm"
      dismissible={!busy}
      title={t("admin.quota.edit.title")}
      footer={
        <>
          <Button onClick={onClose} disabled={busy}>
            {t("admin.quota.edit.cancel")}
          </Button>
          <Button variant="primary" loading={busy} onClick={() => void save()}>
            {t("admin.quota.edit.save")}
          </Button>
        </>
      }
    >
      <p className="tc-admin-muted">{t("admin.quota.edit.description")}</p>
      <Grid gap={3} min="xs">
        <Input
          label={t("admin.quota.edit.field.runningWorkspaces")}
          name="quota-running"
          type="number"
          min={0}
          step={1}
          required
          value={fields.runningWorkspaces}
          onChange={set("runningWorkspaces")}
        />
        <Input
          label={t("admin.quota.edit.field.cpu")}
          name="quota-cpu"
          type="number"
          min={0}
          step="any"
          required
          value={fields.cpu}
          onChange={set("cpu")}
        />
        <Input
          label={t("admin.quota.edit.field.memory")}
          name="quota-memory"
          type="number"
          min={0}
          step="any"
          required
          value={fields.memory}
          onChange={set("memory")}
        />
        <Input
          label={t("admin.quota.edit.field.storage")}
          name="quota-storage"
          type="number"
          min={0}
          step={1}
          required
          value={fields.storage}
          onChange={set("storage")}
        />
      </Grid>
      {invalid ? <Alert tone="warning">{t("admin.quota.edit.invalid")}</Alert> : null}
      <ApiErrorAlert error={saveError} />
    </Dialog>
  );
}

export function QuotaContent({
  quota,
  onChanged,
}: {
  quota: AdminQuotaView;
  onChanged: () => void;
}) {
  const [sort, setSort] = useState<UsageSort>(DEFAULT_USAGE_SORT);
  const [editing, setEditing] = useState(false);
  const writable = quota.source !== "config";

  function onSort(key: UsageSortKey) {
    setSort((s) =>
      s.key === key ? { key, dir: s.dir === 1 ? -1 : 1 } : { key, dir: key === "user" ? 1 : -1 },
    );
  }

  const columns: Column<UserUsage>[] = [
    {
      key: "user",
      header: (
        <SortHeader label={t("admin.quota.column.user")} columnKey="user" sort={sort} onSort={onSort} />
      ),
      rowHeader: true,
      render: (u) => (
        <span className="tc-admin-cell-stack">
          <span>{u.displayName}</span>
          <code className="tc-admin-muted">{u.subject}</code>
        </span>
      ),
    },
    ...QUOTA_KEYS.map(
      (k): Column<UserUsage> => ({
        key: k,
        header: <SortHeader label={quotaLabel(k)} columnKey={k} sort={sort} onSort={onSort} />,
        align: "end",
        hideOnMobile: k === "memoryMib" || k === "cpuMillicores",
        render: (u) => <UsageCell k={k} value={u.usage[k]} limit={quota.limits?.[k]} />,
      }),
    ),
  ];

  return (
    <>
      <Section
        title={t("admin.quota.limits.title")}
        description={t("admin.quota.limits.description", { tenant: quota.tenant })}
        actions={
          writable ? (
            <Button onClick={() => setEditing(true)}>{t("admin.quota.edit.action")}</Button>
          ) : undefined
        }
      >
        <p>
          <SourceBadge source={quota.source} />{" "}
          <span className="tc-admin-muted">
            {quota.source === "config"
              ? t("admin.quota.source.configNote")
              : quota.source === "api"
                ? t("admin.quota.source.apiNote")
                : t("admin.quota.source.noneNote")}
          </span>
        </p>
        <QuotaMeters configured={quota.configured} limits={quota.limits} usage={quota.usage} />
      </Section>
      <Section title={t("admin.quota.users.title")}>
        <Table
          caption={t("admin.quota.users.caption")}
          columns={columns}
          rows={sortedUsers(quota.users, sort)}
          rowKey={(u) => u.subject}
          density="compact"
          empty={t("admin.quota.users.empty")}
        />
      </Section>
      <QuotaEditDialog
        quota={quota}
        open={editing}
        onClose={() => setEditing(false)}
        onSaved={onChanged}
      />
    </>
  );
}

async function loadAdminQuota(api: ApiClient): Promise<AdminQuotaView> {
  const me = await loadMe(api);
  return fetchAdminQuota(api, me.tenant);
}

export function QuotaPage() {
  const q = useLoader(loadAdminQuota, "admin-quota");
  return (
    <AdminLayout
      title={t("admin.quota.title")}
      description={t("admin.quota.description")}
      actions={
        <Button
          icon={<IconRefresh />}
          onClick={q.reload}
          loading={q.loading && !!q.data}
        >
          {t("admin.action.refresh")}
        </Button>
      }
    >
      <ApiErrorAlert error={q.error} onRetry={q.reload} />
      {q.data ? (
        <QuotaContent quota={q.data} onChanged={q.reload} />
      ) : q.loading ? (
        <Card>
          <Spinner label={t("admin.quota.loading")} />
        </Card>
      ) : null}
    </AdminLayout>
  );
}
