import { useState } from "react";
import {
  Alert,
  Button,
  Cluster,
  DescriptionList,
  EmptyState,
  Page,
} from "../design";
import { Link, navigate } from "../lib/router";
import { isPortalApiError } from "../api/errors";
import { t } from "../i18n";
import { AttachDialog } from "./AttachDialog";
import { PurgeDialog } from "./PurgeDialog";
import { canAttach, canPurge, DataStateBadge, ownerLabel } from "./DataListPage";
import {
  getRetainedData,
  isTenantAdmin,
  useLoader,
  useMe,
  type WorkspaceView,
} from "./api";

export function DataDetailPage({ dataId }: { dataId: string }) {
  const me = useMe();
  const admin = isTenantAdmin(me.data);
  const detail = useLoader((a) => getRetainedData(a, dataId), `data:${dataId}`);

  const record = detail.data ?? null;
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
    detail.reload();
  }

  if (detail.loading && !detail.data) {
    return <Page title={t("data.detail.title", { id: dataId })} />;
  }

  if (detail.error || (detail.data === null)) {
    return (
      <Page
        title={t("data.detail.title", { id: dataId })}
        eyebrow={<Link to="/data">{t("data.detail.back")}</Link>}
      >
        {detail.error && !(detail.data === null) ? (
          <Alert
            tone="danger"
            title={isPortalApiError(detail.error) ? detail.error.code : undefined}
            actions={<Button onClick={detail.reload}>{t("data.error.retry")}</Button>}
          >
            {detail.error instanceof Error ? detail.error.message : String(detail.error)}
          </Alert>
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
      title={t("data.detail.title", { id: r.id })}
      eyebrow={<Link to="/data">{t("data.detail.back")}</Link>}
      actions={
        canAttach(r) || canPurge(r) ? (
          <Cluster gap={2}>
            {canAttach(r) ? (
              <Button variant="primary" onClick={() => setAttaching(true)}>
                {t("data.action.attach")}
              </Button>
            ) : null}
            {canPurge(r) ? (
              <Button variant="danger" onClick={() => setPurging(true)}>
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
          { term: t("data.detail.field.id"), detail: r.id },
          { term: t("data.detail.field.state"), detail: <DataStateBadge state={r.state} /> },
          { term: t("data.detail.field.source"), detail: r.sourceWorkspaceName },
          { term: t("data.detail.field.runtime"), detail: r.runtime },
          { term: t("data.detail.field.size"), detail: t("data.list.sizeGib", { n: r.sizeGib }) },
          {
            term: t("data.detail.field.retainedAt"),
            detail: new Date(r.retainedAt).toLocaleString(),
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
