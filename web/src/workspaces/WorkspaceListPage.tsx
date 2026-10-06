import { useCallback, useEffect, useRef, useState } from "react";
import { buttonClass, EmptyState, Page, Spinner, StatusPill, Table } from "../design";
import type { Column } from "../design/Table";
import { IconGrid, IconPlus } from "../design/icons";
import { t } from "../i18n";
import { useApi } from "../api/context";
import { Link } from "../lib/router";
import { listWorkspaces } from "./api";
import type { WorkspaceView } from "./helpers";
import { desiredLabel, isConnectable, phaseLabelKey, templateRefLabel } from "./helpers";
import { useResource } from "./resource";
import { ErrorBanner } from "./ErrorBanner";
import { dataPolicyLabel } from "../templates/format";
import { LifecycleProgress } from "../progress/LifecycleProgress";
import { deriveProgress, opPollMs, serverNow, withJitter } from "../progress/derive";

const IDLE_MS = 10_000;

/**
 * Poll fast while any workspace runs an operation — the cadence follows the
 * youngest operation's age (fresh intents poll fastest) — slow when all are
 * settled. Failed is terminal, not busy.
 */
function pollDelay(items: WorkspaceView[] | undefined): number {
  const now = serverNow();
  let best = Infinity;
  for (const w of items ?? []) {
    const m = deriveProgress(w, undefined, now);
    if (m && m.terminal === null) best = Math.min(best, opPollMs(m.startedAtMs, now));
  }
  return best === Infinity ? IDLE_MS : withJitter(best);
}

const COLUMNS: Column<WorkspaceView>[] = [
  {
    key: "name",
    header: t("workspaces.list.col.name"),
    rowHeader: true,
    render: (w) => <Link to={`/workspaces/${w.id}`}>{w.name}</Link>,
  },
  {
    key: "template",
    header: t("workspaces.list.col.template"),
    hideOnMobile: true,
    render: (w) => templateRefLabel(w.template),
  },
  {
    key: "phase",
    header: t("workspaces.list.col.phase"),
    render: (w) => (
      <>
        <StatusPill phase={w.phase} label={t(phaseLabelKey(w.phase))} />{" "}
        <LifecycleProgress workspace={w} variant="compact" />
        {!isConnectable(w) && w.phase === "Ready" ? (
          <small>{t("workspaces.list.waitingConnection")}</small>
        ) : null}
      </>
    ),
  },
  {
    key: "desiredState",
    header: t("workspaces.list.col.desired"),
    hideOnMobile: true,
    render: (w) => desiredLabel(w),
  },
  {
    key: "dataPolicy",
    header: t("workspaces.list.col.dataPolicy"),
    hideOnMobile: true,
    render: (w) => dataPolicyLabel(w.dataPolicy),
  },
  {
    key: "manage",
    header: "",
    align: "end",
    render: (w) => <Link to={`/workspaces/${w.id}`}>{t("workspaces.list.manage")}</Link>,
  },
];

export function WorkspaceListPage({ pollIntervalMs }: { pollIntervalMs?: number }) {
  const api = useApi();
  const load = useCallback(() => listWorkspaces(api), [api]);
  const list = useResource(load, pollIntervalMs ?? pollDelay);
  const [announcement, setAnnouncement] = useState<string | null>(null);

  // A row in "Deleting" that disappears is the expected end of a delete —
  // announce it politely rather than letting the row vanish silently.
  const items = list.data ?? [];
  const terminating = useRef<Map<string, string>>(new Map());
  useEffect(() => {
    const current = new Map<string, string>();
    for (const w of items) if (w.phase === "Terminating") current.set(w.id, w.name);
    for (const name of [...terminating.current.entries()]
      .filter(([id]) => !current.has(id))
      .map(([, n]) => n)) {
      setAnnouncement(t("progress.deleted.announce", { name }));
    }
    terminating.current = current;
  }, [items]);

  return (
    <Page
      title={t("workspaces.list.title")}
      actions={
        <>
          <Link to="/templates" className={buttonClass("secondary")}>
            <IconGrid size={16} /> {t("nav.templates")}
          </Link>
          <Link to="/workspaces/new" className={buttonClass("primary")}>
            <IconPlus size={16} /> {t("nav.newWorkspace")}
          </Link>
        </>
      }
    >
      <ErrorBanner error={list.error} onRetry={() => void list.refresh()} onDismiss={list.clearError} />
      <div role="status" className="tc-sr-only">
        {announcement}
      </div>
      {list.loading && !list.data ? (
        <Spinner label={t("workspaces.list.loading")} />
      ) : items.length === 0 ? (
        <EmptyState
          icon={<IconGrid size={24} />}
          title={t("workspaces.list.empty")}
          description={t("workspaces.list.emptyBody")}
          action={
            <Link to="/templates" className={buttonClass("primary")}>
              {t("workspaces.list.emptyCta")}
            </Link>
          }
        />
      ) : (
        <>
          <p className="tc-field__hint">{t("workspaces.list.count", { n: items.length })}</p>
          <Table
            columns={COLUMNS}
            rows={items}
            rowKey={(w) => w.id}
            caption={t("workspaces.list.label")}
            empty={t("workspaces.list.empty")}
          />
        </>
      )}
    </Page>
  );
}
