import { useEffect, useState } from "react";
import { Alert, Badge, Button, Checkbox, Dialog, Spinner, Table } from "../design";
import type { Column } from "../design/Table";
import { t, type MessageKey } from "../i18n";
import { useApi } from "../api/context";
import { applyAdminSessionPolicy, type ApplySessionPolicyResult } from "./api";
import { ApiErrorAlert } from "./ApiErrorAlert";

type Row = ApplySessionPolicyResult["affected"][number];

const RESULT_LABEL: Record<Row["result"], MessageKey> = {
  pending: "admin.policy.apply.result.pending",
  restarted: "admin.policy.apply.result.restarted",
  skipped_active: "admin.policy.apply.result.skipped_active",
  skipped_moved: "admin.policy.apply.result.skipped_moved",
  failed: "admin.policy.apply.result.failed",
};

export function ApplyPolicyDialog({
  tenant,
  etag,
  policyText,
  onClose,
}: {
  tenant: string;
  etag: string | undefined;
  policyText: (p: Row["applied"]) => string;
  onClose: () => void;
}) {
  const api = useApi();
  const [data, setData] = useState<ApplySessionPolicyResult | null>(null);
  const [done, setDone] = useState(false);
  const [includeActive, setIncludeActive] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  // The dry run: what the confirmed call would restart. The dialog mounts
  // fresh on every open (the page renders it conditionally).
  useEffect(() => {
    let cancelled = false;
    applyAdminSessionPolicy(api, tenant, { dryRun: true }, etag)
      .then((r) => {
        if (!cancelled) setData(r);
      })
      .catch((e) => {
        if (!cancelled) setError(e);
      });
    return () => {
      cancelled = true;
    };
  }, [api, tenant, etag]);

  async function confirm() {
    setBusy(true);
    setError(null);
    try {
      setData(await applyAdminSessionPolicy(api, tenant, { dryRun: false, includeActive }, etag));
      setDone(true);
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  }

  const rows = data?.affected ?? [];
  const restarted = rows.filter((r) => r.result === "restarted").length;
  const columns: Column<Row>[] = [
    { key: "workspace", header: t("admin.policy.apply.column.workspace"), rowHeader: true, render: (r) => r.name },
    { key: "owner", header: t("admin.policy.apply.column.owner"), render: (r) => r.owner.displayName },
    {
      key: "change",
      header: t("admin.policy.apply.column.change"),
      render: (r) => (
        <>
          {policyText(r.applied)} → {policyText(r.effective)}{" "}
          {r.live ? <Badge tone="warning">{t("admin.policy.apply.live")}</Badge> : null}
        </>
      ),
    },
    { key: "result", header: t("admin.policy.apply.column.result"), render: (r) => t(RESULT_LABEL[r.result]) },
  ];

  return (
    <Dialog
      open
      onClose={() => {
        if (!busy) onClose();
      }}
      dismissible={!busy}
      size="lg"
      title={t("admin.policy.apply.title")}
      footer={
        <>
          <Button onClick={onClose} disabled={busy}>
            {t("admin.policy.apply.close")}
          </Button>
          {data && rows.length > 0 && !done ? (
            <Button variant="primary" loading={busy} onClick={() => void confirm()}>
              {t("admin.policy.apply.confirm")}
            </Button>
          ) : null}
        </>
      }
    >
      <ApiErrorAlert error={error} />
      {!data && !error ? <Spinner label={t("admin.policy.apply.loading")} /> : null}
      {data && rows.length === 0 ? <p>{t("admin.policy.apply.none")}</p> : null}
      {data && rows.length > 0 ? (
        <>
          <p className="tc-admin-muted">{t("admin.policy.apply.description")}</p>
          {data.truncated ? <Alert tone="warning">{t("admin.policy.apply.truncated")}</Alert> : null}
          <Table
            caption={t("admin.policy.apply.caption")}
            columns={columns}
            rows={rows}
            rowKey={(r) => r.workspaceId}
            density="compact"
          />
          {done ? (
            <p role="status">
              {t("admin.policy.apply.summary", { restarted, skipped: rows.length - restarted })}
            </p>
          ) : (
            <Checkbox
              label={t("admin.policy.apply.includeActive")}
              checked={includeActive}
              onChange={(e) => setIncludeActive(e.target.checked)}
            />
          )}
        </>
      ) : null}
    </Dialog>
  );
}
