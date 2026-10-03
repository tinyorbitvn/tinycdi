import { useState } from "react";
import { Alert, Badge, Button, Card, Grid, Meter, Section, Spinner, Table } from "../design";
import type { Column } from "../design/Table";
import { IconChevronDown, IconChevronUp, IconRefresh } from "../design/icons";
import { cx } from "../design/cx";
import { t } from "../i18n";
import { fetchQuota, type QuotaAmounts, type QuotaView, type UserUsage } from "./api";
import {
  QUOTA_KEYS,
  formatQuota,
  quotaLabel,
  usageLevel,
  usagePercent,
  type QuotaKey,
} from "./format";
import { useLoader } from "../app/me";
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

export function QuotaContent({ quota }: { quota: QuotaView }) {
  const [sort, setSort] = useState<UsageSort>(DEFAULT_USAGE_SORT);

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
        render: (u) => <UsageCell k={k} value={u.usage[k]} limit={quota.userLimits?.[k]} />,
      }),
    ),
  ];

  return (
    <>
      <Section
        title={t("admin.quota.limits.title")}
        description={t("admin.quota.limits.description", { tenant: quota.tenant })}
      >
        <QuotaMeters configured={quota.configured} limits={quota.limits} usage={quota.usage} />
      </Section>
      {quota.userLimits ? (
        <Section title={t("admin.quota.userLimits.title")} headingLevel={3}>
          <p className="tc-admin-muted">
            {t("admin.quota.userLimits.body", {
              limits: QUOTA_KEYS.filter((k) => !isUnlimited(k, quota.userLimits![k]))
                .map((k) => `${formatQuota(k, quota.userLimits![k])} ${quotaLabel(k).toLowerCase()}`)
                .join(", "),
            })}
          </p>
        </Section>
      ) : null}
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
    </>
  );
}

export function QuotaPage() {
  const q = useLoader(fetchQuota, "quota");
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
        <QuotaContent quota={q.data} />
      ) : q.loading ? (
        <Card>
          <Spinner label={t("admin.quota.loading")} />
        </Card>
      ) : null}
    </AdminLayout>
  );
}
