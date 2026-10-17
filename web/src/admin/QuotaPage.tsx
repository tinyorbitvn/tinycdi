import { useState } from "react";
import {
  Alert,
  Badge,
  Button,
  Card,
  Cluster,
  Dialog,
  Grid,
  Input,
  Meter,
  Section,
  Spinner,
} from "../design";
import { IconRefresh } from "../design/icons";
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
  type Tagged,
} from "./api";
import { isPortalApiError } from "../api/errors";
import {
  QUOTA_KEYS,
  formatQuota,
  quotaLabel,
  usagePercent,
  type QuotaKey,
} from "./format";
import { loadMe, useLoader } from "../app/me";
import { ApiErrorAlert } from "./ApiErrorAlert";
import { AdminLayout } from "./AdminLayout";
import { UserLimitsSection } from "./UserLimitsSection";

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
    <div className="tc-meter-grid">
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
    </div>
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
  quota: Tagged<AdminQuotaView>;
  open: boolean;
  onClose: () => void;
  onSaved: () => void;
}) {
  const api = useApi();
  const [fields, setFields] = useState<EditFields>(() => fieldsOf(quota));
  const [busy, setBusy] = useState(false);
  const [saveError, setSaveError] = useState<unknown>(null);
  const [invalid, setInvalid] = useState(false);
  // The dialog stays mounted while closed, so state must be reset when it
  // reopens or when the quota changed underneath it — otherwise a reopen
  // shows (and would re-save) stale limits. The reset runs in the render
  // that shows the new props: a post-commit effect could land after an
  // edit made right as a reload painted and silently undo it. While open,
  // the fields are therefore always read from `quota`, so quota.etag is the
  // If-Match that makes a concurrent write 412 instead of being overwritten.
  const [shown, setShown] = useState({ open, quota });
  if (shown.open !== open || shown.quota !== quota) {
    setShown({ open, quota });
    if (open) {
      setFields(fieldsOf(quota));
      setInvalid(false);
      setSaveError(null);
    }
  }

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
      await putAdminQuota(api, quota.tenant, limits, quota.etag);
      onSaved();
      onClose();
    } catch (e) {
      setSaveError(e);
      // 412 PRECONDITION_FAILED: someone else wrote first — pull the fresh
      // values in so the retry is reviewed against them.
      if (isPortalApiError(e) && e.code === "PRECONDITION_FAILED") onSaved();
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
  quota: Tagged<AdminQuotaView>;
  onChanged: () => void;
}) {
  const [editing, setEditing] = useState(false);
  const writable = quota.source !== "config";

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
        <Cluster gap={2} align="center" className="tc-quota-source">
          <SourceBadge source={quota.source} />
          <span className="tc-admin-muted">
            {quota.source === "config"
              ? t("admin.quota.source.configNote")
              : quota.source === "api"
                ? t("admin.quota.source.apiNote")
                : t("admin.quota.source.noneNote")}
          </span>
        </Cluster>
        <QuotaMeters configured={quota.configured} limits={quota.limits} usage={quota.usage} />
      </Section>
      <UserLimitsSection tenant={quota.tenant} />
      <QuotaEditDialog
        quota={quota}
        open={editing}
        onClose={() => setEditing(false)}
        onSaved={onChanged}
      />
    </>
  );
}

async function loadAdminQuota(api: ApiClient): Promise<Tagged<AdminQuotaView>> {
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
