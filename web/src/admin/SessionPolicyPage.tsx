import { useEffect, useRef, useState } from "react";
import { Alert, Button, Card, Section, Select, Spinner, Table, useTableSort } from "../design";
import type { Column } from "../design/Table";
import { IconRefresh } from "../design/icons";
import { t, type MessageKey } from "../i18n";
import type { ApiClient } from "../api/client";
import { useApi } from "../api/context";
import { isPortalApiError } from "../api/errors";
import {
  fetchAdminSessionPolicy,
  listTemplates,
  putAdminSessionPolicy,
  type AdminSessionPolicyView,
  type AdminTemplateView,
  type ClipboardCeiling,
} from "./api";
import { clipboardPolicyLabel } from "../templates/format";
import { loadMe, useLoader } from "../app/me";
import { ApiErrorAlert } from "./ApiErrorAlert";
import { AdminLayout } from "./AdminLayout";

const CEILINGS: readonly ClipboardCeiling[] = [
  "FollowTemplate",
  "Disabled",
  "Send",
  "Receive",
  "Bidirectional",
];

const CEILING_LABEL: Record<ClipboardCeiling, MessageKey> = {
  FollowTemplate: "admin.policy.option.followTemplate",
  Disabled: "templates.clipboard.disabled",
  Send: "templates.clipboard.send",
  Receive: "templates.clipboard.receive",
  Bidirectional: "templates.clipboard.bidirectional",
};

// meetClipboard resolves the direction lattice the backend enforces
// (ADR 0010 §1): the effective policy is the meet of the template's own
// clipboard policy and the tenant ceiling — Disabled is bottom,
// Bidirectional is top, the single directions are incomparable
// (Send ∩ Receive = Disabled) — and a ceiling can only narrow.
export function meetClipboard(template: string, ceiling: string): string {
  switch (ceiling) {
    case "Disabled":
      return "Disabled";
    case "Send":
    case "Receive":
      return template === ceiling || template === "Bidirectional" ? ceiling : "Disabled";
    default:
      // FollowTemplate, Bidirectional — or an unrecognised value, which
      // narrows nothing.
      return template;
  }
}

interface PolicyData {
  policy: AdminSessionPolicyView;
  templates: AdminTemplateView[];
}

async function loadPolicy(api: ApiClient): Promise<PolicyData> {
  const me = await loadMe(api);
  const [policy, templates] = await Promise.all([
    fetchAdminSessionPolicy(api, me.tenant),
    listTemplates(api),
  ]);
  return { policy, templates: templates.items };
}

function SourceNote({ source }: { source: AdminSessionPolicyView["source"] }) {
  return (
    <p>
      <span className="tc-admin-muted">
        {source === "TenantRow"
          ? t("admin.policy.source.tenantRow")
          : t("admin.policy.source.followTemplate")}
      </span>
    </p>
  );
}

export function SessionPolicyContent({
  data,
  onChanged,
}: {
  data: PolicyData;
  onChanged: () => void;
}) {
  const api = useApi();
  const { policy, templates } = data;
  const [ceiling, setCeiling] = useState<ClipboardCeiling>(policy.clipboard);
  const [busy, setBusy] = useState(false);
  const [saveError, setSaveError] = useState<unknown>(null);
  // The version the form's selection was read from — sent as If-Match so
  // a concurrent write 412s instead of being silently overwritten.
  const versionRef = useRef(policy.version);

  // The page reloads after every save and on precondition failures: reset
  // the selection and version to what the server just reported. The error
  // is deliberately NOT cleared here — a 412 needs its "reloaded, review
  // and retry" guidance to stay visible while the new values land.
  useEffect(() => {
    setCeiling(policy.clipboard);
    versionRef.current = policy.version;
  }, [policy]);

  async function save() {
    setBusy(true);
    setSaveError(null);
    try {
      await putAdminSessionPolicy(
        api,
        policy.tenant,
        { clipboard: ceiling, fileTransfer: { mode: "FollowTemplate", maxFileBytes: null } },
        versionRef.current,
      );
      onChanged();
    } catch (e) {
      setSaveError(e);
      // 412 PRECONDITION_FAILED: someone else wrote first — pull the
      // fresh values in so the retry is reviewed against them.
      if (isPortalApiError(e) && e.code === "PRECONDITION_FAILED") onChanged();
    } finally {
      setBusy(false);
    }
  }

  const columns: Column<AdminTemplateView>[] = [
    {
      key: "template",
      sortable: true,
      compare: (a, b) => a.name.localeCompare(b.name),
      header: t("admin.policy.preview.template"),
      rowHeader: true,
      render: (tpl) => tpl.name,
    },
    {
      key: "templatePolicy",
      sortable: true,
      compare: (a, b) => a.clipboardPolicy.localeCompare(b.clipboardPolicy),
      header: t("admin.policy.preview.templatePolicy"),
      render: (tpl) => clipboardPolicyLabel(tpl.clipboardPolicy),
    },
    {
      key: "ceiling",
      header: t("admin.policy.preview.ceiling"),
      render: () => t(CEILING_LABEL[ceiling]),
    },
    {
      key: "effective",
      sortable: true,
      compare: (a, b) =>
        meetClipboard(a.clipboardPolicy, ceiling).localeCompare(
          meetClipboard(b.clipboardPolicy, ceiling),
        ),
      header: t("admin.policy.preview.effective"),
      render: (tpl) => clipboardPolicyLabel(meetClipboard(tpl.clipboardPolicy, ceiling)),
    },
  ];
  const sort = useTableSort(columns, templates, { key: "template", direction: "ascending" });

  return (
    <>
      <Section
        title={t("admin.policy.ceiling.title")}
        description={t("admin.policy.ceiling.description")}
      >
        <SourceNote source={policy.source} />
        <div className="tc-admin-policy-fields">
          <Select
            label={t("admin.policy.clipboard.label")}
            hint={t("admin.policy.clipboard.hint")}
            name="clipboard-ceiling"
            value={ceiling}
            onChange={(e) => setCeiling(e.target.value as ClipboardCeiling)}
            options={CEILINGS.map((v) => ({ value: v, label: t(CEILING_LABEL[v]) }))}
          />
          <Select
            label={t("admin.policy.fileTransfer.label")}
            hint={t("admin.policy.fileTransfer.hint")}
            name="file-transfer-ceiling"
            value="FollowTemplate"
            disabled
            options={[{ value: "FollowTemplate", label: t(CEILING_LABEL.FollowTemplate) }]}
          />
        </div>
        <Alert tone="info">{t("admin.policy.appliesNote")}</Alert>
        <div>
          <Button
            variant="primary"
            loading={busy}
            disabled={ceiling === policy.clipboard}
            onClick={() => void save()}
          >
            {t("admin.policy.save")}
          </Button>
        </div>
        <ApiErrorAlert error={saveError} />
      </Section>
      <Section
        title={t("admin.policy.preview.title")}
        description={t("admin.policy.preview.description")}
      >
        <Table
          caption={t("admin.policy.preview.caption")}
          columns={columns}
          rows={sort.sortedRows}
          rowKey={(tpl) => tpl.id}
          sort={sort.sort}
          onSortChange={sort.onSortChange}
          density="compact"
          empty={t("admin.policy.preview.empty")}
        />
      </Section>
    </>
  );
}

export function SessionPolicyPage() {
  const q = useLoader(loadPolicy, "admin-session-policy");
  return (
    <AdminLayout
      title={t("admin.policy.title")}
      description={t("admin.policy.description")}
      actions={
        <Button icon={<IconRefresh />} onClick={q.reload} loading={q.loading && !!q.data}>
          {t("admin.action.refresh")}
        </Button>
      }
    >
      <ApiErrorAlert error={q.error} onRetry={q.reload} />
      {q.data ? (
        <SessionPolicyContent data={q.data} onChanged={q.reload} />
      ) : q.loading ? (
        <Card>
          <Spinner label={t("admin.policy.loading")} />
        </Card>
      ) : null}
    </AdminLayout>
  );
}
