import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import {
  Alert,
  Badge,
  Button,
  Cluster,
  DescriptionList,
  Page,
  Section,
  Spinner,
  Table,
  useToast,
} from "../design";
import type { Column } from "../design/Table";
import { IconArrowLeft } from "../design/icons";
import { t } from "../i18n";
import { useApi } from "../api/context";
import { newIdempotencyKey, unwrap } from "../api/client";
import { isPortalApiError } from "../api/errors";
import { navigate, Link } from "../lib/router";
import { resolveTemplate } from "../templates/family";
import { useTemplates } from "../templates/useTemplates";
import {
  dataPolicyDescription,
  dataPolicyLabel,
  experienceLabel,
  formatDate,
  formatDateTime,
  networkProfileDescription,
  networkProfileLabel,
  runtimeLabel,
} from "../templates/format";
import { getRetainedData, type ScopedRetainedData } from "../data/api";
import { useBranding } from "../app/shell";
import { DEFAULT_BRANDING } from "../app/branding";
import { getWorkspace, listWorkspaceEvents } from "./api";
import type { WorkspaceEvent, WorkspaceView } from "./helpers";
import { blockingReason, desiredLabel } from "./helpers";
import { ReasonText, reasonMessageKey } from "./reasons";
import { useResource } from "./resource";
import { ConnectButton } from "./ConnectButton";
import { ConditionsTable, PhasePill } from "./StatusBits";
import { DeleteWorkspaceButton } from "./DeleteWorkspaceButton";
import { ErrorBanner } from "./ErrorBanner";
import { LifecycleProgress } from "../progress/LifecycleProgress";
import { workspacePollMs, withJitter } from "../progress/derive";

const TERMINAL = new Set(["Stopped", "Failed"]);
const IDLE_MS = 8000;

interface DetailData {
  workspace: WorkspaceView;
  events: WorkspaceEvent[];
  /** The retained disk this workspace consumes, when it has one. */
  retained?: ScopedRetainedData | null;
}

// Operation cadence while a lifecycle intent is in flight (fast when young,
// backing off with age); the idle 8 s otherwise.
function pollDelay(d: DetailData | undefined): number {
  return withJitter(workspacePollMs(d?.workspace, IDLE_MS));
}

// The failure field reads in plain language for the causes we can name;
// the raw token stays as secondary text (and the hover title) so the detail
// is never lost. Unknown causes render raw only — never hidden.
const FAILURE_COPY: Record<string, Parameters<typeof t>[0]> = {
  BootDeadlineExceeded: "progress.failed.deadline",
  ImagePullBackOff: "progress.failed.imagePull",
  ErrImagePull: "progress.failed.imagePull",
  CrashLoopBackOff: "progress.notice.retry",
};

function failureDetail(reason: string): ReactNode {
  const token = reason.split(":")[0].trim();
  const key = FAILURE_COPY[token] ?? reasonMessageKey(token);
  if (!key) return reason;
  return (
    <>
      {t(key)}{" "}
      <span className="tc-field__hint" title={reason}>
        {reason}
      </span>
    </>
  );
}

const EVENT_TONE = { Normal: "neutral", Warning: "warning" } as const;

function eventTypeLabel(ev: WorkspaceEvent): string {
  return t(
    ev.type === "Warning" ? "workspaces.detail.events.type.warning" : "workspaces.detail.events.type.normal",
  );
}

function EventsTable({ events }: { events: WorkspaceEvent[] }) {
  const columns: Column<WorkspaceEvent>[] = [
    {
      key: "type",
      header: t("workspaces.conditions.col.type"),
      render: (ev) => (
        <Badge tone={EVENT_TONE[ev.type] ?? "neutral"}>{eventTypeLabel(ev)}</Badge>
      ),
    },
    { key: "reason", header: t("workspaces.conditions.col.reason"), rowHeader: true },
    {
      key: "message",
      header: t("workspaces.conditions.col.message"),
      render: (ev) => (
        <ReasonText reason={ev.reason} detail={ev.message} params={ev.params} />
      ),
    },
    {
      key: "count",
      header: "",
      align: "end",
      render: (ev) => (ev.count && ev.count > 1 ? t("workspaces.detail.events.count", { n: ev.count }) : ""),
    },
    {
      key: "lastTimestamp",
      header: t("workspaces.conditions.col.since"),
      render: (ev) => (ev.lastTimestamp ? formatDateTime(ev.lastTimestamp) : ""),
    },
  ];
  return (
    <Table
      columns={columns}
      rows={events}
      rowKey={(ev) => ev.id}
      caption={t("workspaces.detail.events.title")}
      empty={t("workspaces.detail.events.empty")}
    />
  );
}

export function WorkspaceDetailPage({
  workspaceId,
  pollIntervalMs,
}: {
  workspaceId: string;
  pollIntervalMs?: number;
}) {
  const api = useApi();
  const templates = useTemplates();
  const [busy, setBusy] = useState<string | null>(null);
  const [actionError, setActionError] = useState<unknown>(null);

  const load = useCallback(
    async (background: boolean): Promise<DetailData> => {
      const [workspace, events] = await Promise.all([
        getWorkspace(api, workspaceId, background),
        listWorkspaceEvents(api, workspaceId, background),
      ]);
      // The retained disk this workspace mounts: named so the user can tell
      // where its home came from (T5.4). Best effort — the record may be
      // gone or not visible to this caller.
      const retained = workspace.retainedDataRef
        ? await getRetainedData(api, workspace.retainedDataRef, background).catch(() => null)
        : null;
      return { workspace, events, retained };
    },
    [api, workspaceId],
  );
  const detail = useResource(load, pollIntervalMs ?? pollDelay);
  const { toast } = useToast();
  // A delete "completes" when the API drops the row (FX-R19 hides finalised
  // workspaces): the page stays on the teardown progress until the GET
  // 404s. Set either by this page's own Delete or by a row that arrived
  // already Terminating.
  const deleteSeen = useRef(false);
  const deletedDone = useRef(false);

  useEffect(() => {
    if (detail.data?.workspace.phase === "Terminating") deleteSeen.current = true;
    const e = detail.error;
    if (
      deletedDone.current ||
      !deleteSeen.current ||
      !isPortalApiError(e) ||
      e.httpStatus !== 404
    ) {
      return;
    }
    deletedDone.current = true;
    const gone = detail.data?.workspace;
    toast({
      tone: "success",
      title: t("progress.deleted.toast", { name: gone?.name ?? workspaceId }),
      ...(gone?.dataPolicy === "Retain"
        ? {
            action: {
              label: t("progress.deleted.retainedLink"),
              onClick: () => navigate("/data"),
            },
          }
        : {}),
    });
    navigate("/");
  }, [detail.error, detail.data, toast, workspaceId]);

  async function act(kind: "start" | "stop") {
    const ws = detail.data?.workspace;
    if (!ws) return;
    setBusy(kind);
    setActionError(null);
    try {
      if (kind === "start") {
        unwrap(
          await api.POST("/v1/workspaces/{workspaceId}/start", {
            params: {
              path: { workspaceId: ws.id },
              header: { "Idempotency-Key": newIdempotencyKey() },
            },
          }),
        );
      } else {
        unwrap(
          await api.POST("/v1/workspaces/{workspaceId}/stop", {
            params: { path: { workspaceId: ws.id } },
          }),
        );
      }
      await detail.refresh();
    } catch (e) {
      setActionError(e);
    } finally {
      setBusy(null);
    }
  }

  const ws = detail.data?.workspace;
  const template = ws ? resolveTemplate(templates.data, ws.template) : undefined;
  const error = actionError ?? detail.error;
  const branding = useBranding();

  // The route title is shared with the list; once the workspace is known
  // the tab should name it (T5.4). The cleanup leaves the neutral product
  // title so a workspace name never outlives its page — the shell's own
  // route-title effect overwrites it in the same commit either way.
  const wsName = ws?.name;
  const productName = branding?.productName ?? DEFAULT_BRANDING.productName;
  useEffect(() => {
    if (!wsName) return;
    document.title = `${wsName} · ${productName}`;
    return () => {
      document.title = productName;
    };
  }, [wsName, productName]);

  if (!ws) {
    return (
      <Page
        title={t("workspaces.detail.loading")}
        eyebrow={
          <Link to="/">
            <IconArrowLeft size={14} aria-hidden="true" /> {t("nav.allWorkspaces")}
          </Link>
        }
      >
        <ErrorBanner error={error} onRetry={() => void detail.refresh()} onDismiss={detail.clearError} />
        {error ? null : <Spinner label={t("workspaces.detail.loading")} />}
      </Page>
    );
  }

  const blocker = blockingReason(ws);
  const canStart = ws.phase === "Stopped" || ws.phase === "Failed";
  const canStop = ws.desiredState === "Running" && !TERMINAL.has(ws.phase);
  // Connect needs a running workspace; on a stopped one the Start action is
  // the way forward, so the Connect control is not offered (T5.4).
  const canOfferConnect = ws.desiredState === "Running" && ws.phase !== "Terminating";

  const fields = [
    {
      term: t("workspaces.detail.field.template"),
      detail:
        ws.template.revision > 0
          ? t("workspaces.detail.template", {
              name: ws.template.name,
              revision: ws.template.revision,
              runtime: runtimeLabel(ws.template.runtime),
              experience: experienceLabel(ws.template.experience),
            })
          : // Revision 0 is "no published revision yet", not a real rev (T5.4).
            t("workspaces.detail.templateNoRevision", {
              name: ws.template.name,
              runtime: runtimeLabel(ws.template.runtime),
              experience: experienceLabel(ws.template.experience),
            }),
    },
    ...(ws.retainedDataRef
      ? [
          {
            term: t("workspaces.detail.field.dataDisk"),
            detail: detail.data?.retained
              ? t("workspaces.detail.dataDisk.retained", {
                  source: detail.data.retained.sourceWorkspaceName,
                  id: ws.retainedDataRef,
                })
              : ws.retainedDataRef,
          },
        ]
      : []),
    ...(ws.owner
      ? [
          {
            term: t("workspaces.detail.field.owner"),
            detail:
              ws.owner.displayName && ws.owner.displayName !== ws.owner.subject
                ? `${ws.owner.displayName} (${ws.owner.subject})`
                : ws.owner.subject,
          },
        ]
      : []),
    { term: t("workspaces.detail.field.desired"), detail: desiredLabel(ws) },
    {
      term: t("workspaces.detail.field.dataPolicy"),
      detail: `${dataPolicyLabel(ws.dataPolicy)} — ${dataPolicyDescription(ws.dataPolicy)}`,
    },
    ...(template?.networkProfile
      ? [
          {
            term: t("workspaces.detail.field.networkProfile"),
            detail: `${networkProfileLabel(template.networkProfile)} — ${networkProfileDescription(template.networkProfile)}`,
          },
        ]
      : []),
    ...(ws.imageBuiltAt
      ? [{ term: t("workspaces.detail.field.imageBuilt"), detail: formatDateTime(ws.imageBuiltAt) }]
      : []),
    ...(ws.failureReason
      ? [{ term: t("workspaces.detail.field.failure"), detail: failureDetail(ws.failureReason) }]
      : []),
    { term: t("workspaces.detail.field.created"), detail: formatDateTime(ws.createdAt) },
    { term: t("workspaces.detail.field.updated"), detail: formatDateTime(ws.updatedAt) },
    { term: t("workspaces.detail.field.id"), detail: ws.id },
  ];

  return (
    <Page
      eyebrow={
        <Link to="/">
          <IconArrowLeft size={14} aria-hidden="true" /> {t("nav.allWorkspaces")}
        </Link>
      }
      title={
        <Cluster gap={3}>
          {ws.name} <PhasePill phase={ws.phase} />
        </Cluster>
      }
      actions={
        <Cluster gap={2}>
          {canOfferConnect ? <ConnectButton workspace={ws} /> : null}
          {canStart ? (
            <Button variant="secondary" loading={busy === "start"} disabled={busy !== null} onClick={() => void act("start")}>
              {busy === "start"
                ? t("workspaces.detail.action.starting")
                : ws.phase === "Failed"
                  ? t("workspaces.detail.action.retryStart")
                  : t("workspaces.detail.action.start")}
            </Button>
          ) : null}
          {ws.phase !== "Terminating" &&
          (canStop || ws.phase === "Ready" || ws.phase === "Provisioning") ? (
            <Button
              variant="secondary"
              loading={busy === "stop"}
              disabled={busy !== null || ws.desiredState === "Stopped"}
              onClick={() => void act("stop")}
            >
              {busy === "stop" ? t("workspaces.detail.action.stopping") : t("workspaces.detail.action.stop")}
            </Button>
          ) : null}
          <DeleteWorkspaceButton
            workspace={ws}
            onDeleteStarted={() => {
              deleteSeen.current = true;
              void detail.refresh();
            }}
          />
        </Cluster>
      }
    >
      <ErrorBanner error={error} onRetry={() => void detail.refresh()} onDismiss={() => { setActionError(null); detail.clearError(); }} />
      {blocker ? (
        <Alert tone="info" title={t("workspaces.detail.connectStatus")}>
          {t("workspaces.detail.connectUnavailable", { reason: t(blocker.key, blocker.params) })}
        </Alert>
      ) : null}
      {ws.imageStale === true ? (
        <Alert tone="warning" title={t("workspaces.detail.stale.badge")}>
          {ws.imageBuiltAt
            ? t("workspaces.detail.stale.body", { date: formatDate(ws.imageBuiltAt) })
            : t("workspaces.detail.stale.bodyUnknown")}
        </Alert>
      ) : null}
      {ws.updateAvailable === true && (ws.phase === "Stopped" || ws.phase === "Failed") ? (
        <Alert tone="info" title={t("workspaces.detail.update.title")}>
          {t("workspaces.detail.update.body")}
        </Alert>
      ) : null}
      <LifecycleProgress
        workspace={ws}
        variant="full"
        refreshError={detail.error}
        onRetry={() => void detail.refresh()}
      />
      <Section>
        <DescriptionList items={fields} />
      </Section>
      <Section title={t("workspaces.detail.events.title")}>
        <EventsTable events={detail.data?.events ?? []} />
      </Section>
      <Section title={t("workspaces.detail.conditions.title")}>
        <ConditionsTable workspace={ws} />
      </Section>
    </Page>
  );
}
