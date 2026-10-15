import { useCallback, useEffect, useRef, useState } from "react";
import {
  Alert,
  Button,
  Cluster,
  DescriptionList,
  EmptyState,
  IconButton,
  IconClipboard,
  Page,
  useToast,
} from "../design";
import { Link, navigate } from "../lib/router";
import { shortId } from "./format";
import { useApi } from "../api/context";
import { ErrorBanner } from "../workspaces/ErrorBanner";
import { useResource } from "../workspaces/resource";
import { t, formatDateTime } from "../i18n";
import { runtimeLabel } from "../templates/format";
import { AttachDialog } from "./AttachDialog";
import { PurgeDialog } from "./PurgeDialog";
import { canAttach, canPurge, DataStateBadge, ownerLabel, PURGING_POLL_MS } from "./DataListPage";
import { isTenantAdmin, useMeLoaded } from "../app/me";
import {
  getRetainedData,
  type ScopedRetainedData,
  type WorkspaceView,
} from "./api";

export function DataDetailPage({
  dataId,
  pollIntervalMs,
}: {
  dataId: string;
  pollIntervalMs?: number;
}) {
  const api = useApi();
  const me = useMeLoaded();
  const admin = isTenantAdmin(me.data);
  const { toast } = useToast();
  async function copyId(id: string) {
    try {
      await navigator.clipboard.writeText(id);
      toast({ tone: "success", title: t("data.detail.copied") });
    } catch {
      // Clipboard blocked (permissions / insecure context): the id stays
      // visible and selectable in the list, so there is nothing to recover.
    }
  }
  const load = useCallback(() => getRetainedData(api, dataId), [api, dataId]);
  // Poll while the record is Purging (same cadence as the list); a Purged
  // record either stops being returned (404 -> null) or reports Purged —
  // either way the poll stops.
  const detail = useResource(load, (d) =>
    d?.state === "Purging" ? (pollIntervalMs ?? PURGING_POLL_MS) : null,
  );

  // The last record actually seen: once a Purging record vanishes the
  // detail view shows the Purged state (no actions) rather than the
  // not-found empty state — the disappearance IS the purge completing.
  const lastSeen = useRef<ScopedRetainedData | null>(null);
  useEffect(() => {
    if (detail.data) lastSeen.current = detail.data;
  }, [detail.data]);

  const record =
    detail.data ??
    (lastSeen.current ? { ...lastSeen.current, state: "Purged" as const } : null);
  const [attaching, setAttaching] = useState(false);
  const [purging, setPurging] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);

  function onAttached(ws: WorkspaceView) {
    setAttaching(false);
    navigate(`/workspaces/${ws.id}`);
  }

  function onPurged() {
    setPurging(false);
    setNotice(t("data.purge.scheduled"));
    void detail.refresh();
  }

  if (detail.loading && record === null) {
    return <Page title={t("data.detail.title", { id: shortId(dataId) })} back={{ to: "/data", label: t("data.detail.back") }} />;
  }

  if (detail.error || record === null) {
    return (
      <Page
        title={t("data.detail.title", { id: shortId(dataId) })}
        back={{ to: "/data", label: t("data.detail.back") }}
      >
        {detail.error && !(detail.data === null) ? (
          <ErrorBanner error={detail.error} onRetry={() => void detail.refresh()} />
        ) : (
          <EmptyState
            title={t("data.detail.notFound.title")}
            description={t("data.detail.notFound.description")}
            action={<Link to="/data">{t("data.detail.back")}</Link>}
          />
        )}
      </Page>
    );
  }

  const r = record!;
  return (
    <Page
      title={r.sourceWorkspaceName || shortId(r.id)}
      back={{ to: "/data", label: t("data.detail.back") }}
      actions={
        canAttach(r) || canPurge(r) ? (
          <Cluster gap={2}>
            {canAttach(r) ? (
              <Button variant="primary" onClick={() => setAttaching(true)}>
                {t("data.action.attach")}
              </Button>
            ) : null}
            {canPurge(r) ? (
              <Button variant="danger-ghost" onClick={() => setPurging(true)}>
                {t("data.action.purge")}
              </Button>
            ) : null}
          </Cluster>
        ) : undefined
      }
    >
      {notice ? (
        <Alert tone="success" onDismiss={() => setNotice(null)}>
          {notice}
        </Alert>
      ) : null}
      <DescriptionList
        items={[
          {
            term: t("data.detail.field.id"),
            detail: (
              <Cluster gap={2} wrap={false}>
                <code>{r.id}</code>
                <IconButton
                  size="sm"
                  label={t("data.detail.copyId")}
                  icon={<IconClipboard />}
                  onClick={() => void copyId(r.id)}
                />
              </Cluster>
            ),
          },
          { term: t("data.detail.field.state"), detail: <DataStateBadge state={r.state} /> },
          { term: t("data.detail.field.source"), detail: r.sourceWorkspaceName },
          { term: t("data.detail.field.runtime"), detail: runtimeLabel(r.runtime) },
          { term: t("data.detail.field.size"), detail: t("data.list.sizeGib", { n: r.sizeGib }) },
          {
            term: t("data.detail.field.retainedAt"),
            detail: formatDateTime(r.retainedAt),
          },
          ...(admin && r.owner
            ? [{ term: t("data.detail.field.owner"), detail: ownerLabel(r.owner) }]
            : []),
          ...(r.consumingWorkspaceId
            ? [
                {
                  term: t("data.detail.field.consuming"),
                  detail: (
                    <Link to={`/workspaces/${r.consumingWorkspaceId}`}>
                      {r.consumingWorkspaceId}
                    </Link>
                  ),
                },
              ]
            : []),
        ]}
      />
      {attaching ? (
        <AttachDialog record={r} onClose={() => setAttaching(false)} onAttached={onAttached} />
      ) : null}
      {purging ? (
        <PurgeDialog record={r} onClose={() => setPurging(false)} onPurged={onPurged} />
      ) : null}
    </Page>
  );
}
