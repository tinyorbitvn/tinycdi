import { useCallback, useMemo, useState } from "react";
import {
  Alert,
  Badge,
  Button,
  type Column,
  EmptyState,
  Page,
  Select,
  Table,
  type Tone,
  VisuallyHidden,
} from "../design";
import { Link, navigate } from "../lib/router";
import { useApi } from "../api/context";
import { isPortalApiError } from "../api/errors";
import { useResource } from "../workspaces/resource";
import { t, formatDateTime, type MessageKey } from "../i18n";
import { runtimeLabel } from "../templates/format";
import { AttachDialog } from "./AttachDialog";
import { PurgeDialog } from "./PurgeDialog";
import { isTenantAdmin, useMeLoaded } from "../app/me";
import {
  listRetainedData,
  type RetainedDataState,
  type Scope,
  type ScopedRetainedData,
  type WorkspaceView,
} from "./api";

// Status is never conveyed by colour alone: every state renders its label
// next to the tone dot, and transitional states pulse.
export const STATE_TONES: Record<RetainedDataState, { tone: Tone; pulse: boolean }> = {
  Retained: { tone: "neutral", pulse: false },
  Attaching: { tone: "info", pulse: true },
  Attached: { tone: "success", pulse: false },
  Purging: { tone: "warning", pulse: true },
  Purged: { tone: "neutral", pulse: false },
};

export const STATE_LABEL_KEYS: Record<RetainedDataState, MessageKey> = {
  Retained: "data.state.retained",
  Attaching: "data.state.attaching",
  Attached: "data.state.attached",
  Purging: "data.state.purging",
  Purged: "data.state.purged",
};

export function DataStateBadge({ state }: { state: RetainedDataState }) {
  const meta = STATE_TONES[state] ?? { tone: "neutral" as Tone, pulse: false };
  return (
    <Badge tone={meta.tone} dot pulse={meta.pulse}>
      {t(STATE_LABEL_KEYS[state] ?? "data.state.retained")}
    </Badge>
  );
}

export function canAttach(rec: Pick<ScopedRetainedData, "state">): boolean {
  return rec.state === "Retained";
}

export function canPurge(rec: Pick<ScopedRetainedData, "state">): boolean {
  return rec.state === "Retained";
}

export function ownerLabel(owner: { subject: string; displayName: string } | undefined): string {
  return owner ? owner.displayName || owner.subject : "—";
}

/** Poll cadence while a record waits for its purge sweep (FX-R29). */
export const PURGING_POLL_MS = 3_000;

// Poll only while a row is Purging: the sweep usually lands inside a
// minute and a Purged record drops out of the list (the API excludes it),
// so once nothing is Purging the poll stops entirely. Error backoff and
// the hidden-tab pause come from useResource.
function purgePollDelay(
  data: { items: ScopedRetainedData[] } | undefined,
  override: number | undefined,
): number | null {
  return data?.items.some((r) => r.state === "Purging")
    ? (override ?? PURGING_POLL_MS)
    : null;
}

export function DataListPage({ pollIntervalMs }: { pollIntervalMs?: number }) {
  const api = useApi();
  const me = useMeLoaded();
  const admin = isTenantAdmin(me.data);
  const [scope, setScope] = useState<Scope>("mine");
  const effectiveScope: Scope = admin ? scope : "mine";
  const load = useCallback(
    (background: boolean) => listRetainedData(api, effectiveScope, background),
    [api, effectiveScope],
  );
  const list = useResource(load, (data) => purgePollDelay(data, pollIntervalMs));

  const [attaching, setAttaching] = useState<ScopedRetainedData | null>(null);
  const [purging, setPurging] = useState<ScopedRetainedData | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const columns = useMemo<Column<ScopedRetainedData>[]>(() => {
    const cols: Column<ScopedRetainedData>[] = [
      {
        key: "id",
        header: t("data.list.col.id"),
        rowHeader: true,
        render: (r) => <Link to={`/data/${r.id}`}>{r.id}</Link>,
      },
      { key: "source", header: t("data.list.col.source"), render: (r) => r.sourceWorkspaceName },
    ];
    if (effectiveScope === "tenant") {
      cols.push({
        key: "owner",
        header: t("data.list.col.owner"),
        render: (r) => ownerLabel(r.owner),
      });
    }
    cols.push(
      {
        key: "runtime",
        header: t("data.list.col.runtime"),
        hideOnMobile: true,
        render: (r) => runtimeLabel(r.runtime),
      },
      {
        key: "size",
        header: t("data.list.col.size"),
        align: "end",
        render: (r) => t("data.list.sizeGib", { n: r.sizeGib }),
      },
      {
        key: "state",
        header: t("data.list.col.state"),
        render: (r) => <DataStateBadge state={r.state} />,
      },
      {
        key: "retainedAt",
        header: t("data.list.col.retainedAt"),
        hideOnMobile: true,
        render: (r) => formatDateTime(r.retainedAt),
      },
      {
        key: "actions",
        header: <VisuallyHidden>{t("data.list.col.actions")}</VisuallyHidden>,
        render: (r) => (
          <>
            {canAttach(r) ? (
              <Button size="sm" onClick={() => setAttaching(r)}>
                {t("data.action.attach")}
              </Button>
            ) : null}
            {canPurge(r) ? (
              <Button
                size="sm"
                variant="danger"
                onClick={() => setPurging(r)}
              >
                {t("data.action.purge")}
              </Button>
            ) : null}
          </>
        ),
      },
    );
    return cols;
  }, [effectiveScope]);

  function onAttached(ws: WorkspaceView) {
    setAttaching(null);
    navigate(`/workspaces/${ws.id}`);
  }

  function onPurged() {
    setPurging(null);
    setNotice(t("data.purge.scheduled"));
    // refresh() re-arms the stopped poll, so the now-Purging row is
    // followed until the sweep drops it from the list.
    void list.refresh();
  }

  const items = list.data?.items;

  return (
    <Page
      title={t("data.list.title")}
      description={t("data.list.description")}
      actions={
        admin ? (
          <Select
            label={t("data.list.scope.label")}
            value={effectiveScope}
            onChange={(e) => setScope(e.target.value as Scope)}
            options={[
              { value: "mine", label: t("data.list.scope.mine") },
              { value: "tenant", label: t("data.list.scope.tenant") },
            ]}
          />
        ) : undefined
      }
    >
      {notice ? (
        <Alert tone="success" onDismiss={() => setNotice(null)}>
          {notice}
        </Alert>
      ) : null}
      {list.error ? (
        <Alert
          tone="danger"
          title={isPortalApiError(list.error) ? list.error.code : undefined}
          actions={<Button onClick={() => void list.refresh()}>{t("data.error.retry")}</Button>}
        >
          {list.error instanceof Error ? list.error.message : String(list.error)}
        </Alert>
      ) : null}
      {list.data?.truncated ? (
        <Alert tone="warning">{t("data.list.truncated", { n: items?.length ?? 0 })}</Alert>
      ) : null}
      <Table
        caption={t("data.list.caption")}
        columns={columns}
        rows={items ?? []}
        rowKey={(r) => r.id}
        loading={list.loading && !list.data}
        empty={
          <EmptyState
            title={t("data.list.empty.title")}
            description={t("data.list.empty.description")}
          />
        }
      />
      {attaching ? (
        <AttachDialog
          record={attaching}
          onClose={() => setAttaching(null)}
          onAttached={onAttached}
        />
      ) : null}
      {purging ? (
        <PurgeDialog
          record={purging}
          onClose={() => setPurging(null)}
          onPurged={onPurged}
        />
      ) : null}
    </Page>
  );
}
