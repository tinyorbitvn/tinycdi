import { useEffect, useState } from "react";
import { t } from "../i18n";
import { useApi } from "../api/context";
import { unwrap } from "../api/client";
import { Link } from "../lib/router";
import type { WorkspaceView } from "./helpers";
import { isConnectable } from "./helpers";
import { PhaseBadge } from "./StatusBits";
import { ErrorBanner } from "./ErrorBanner";

export function WorkspaceListPage({ pollIntervalMs = 2000 }: { pollIntervalMs?: number }) {
  const api = useApi();
  const [workspaces, setWorkspaces] = useState<WorkspaceView[] | null>(null);
  const [error, setError] = useState<unknown>(null);

  useEffect(() => {
    let cancelled = false;
    async function load() {
      try {
        const res = unwrap(await api.GET("/v1/workspaces", {}));
        if (!cancelled) {
          setWorkspaces(res.items);
          setError(null);
        }
      } catch (e) {
        if (!cancelled) setError(e);
      }
    }
    void load();
    const timer = setInterval(() => void load(), pollIntervalMs);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [api, pollIntervalMs]);

  return (
    <main>
      <h1>{t("workspaces.list.title")}</h1>
      <nav>
        <Link to="/workspaces/new">{t("nav.newWorkspace")}</Link> ·{" "}
        <Link to="/templates">{t("nav.templates")}</Link> ·{" "}
        <Link to="/data">{t("nav.data")}</Link>
      </nav>
      <ErrorBanner error={error} onDismiss={() => setError(null)} />
      {!workspaces ? (
        <p aria-busy="true">{t("workspaces.list.loading")}</p>
      ) : workspaces.length === 0 ? (
        <p>{t("workspaces.list.empty")}</p>
      ) : (
        <table aria-label={t("workspaces.list.count", { n: workspaces.length })}>
          <thead>
            <tr>
              <th>{t("workspaces.list.col.name")}</th>
              <th>{t("workspaces.list.col.template")}</th>
              <th>{t("workspaces.list.col.phase")}</th>
              <th>{t("workspaces.list.col.desired")}</th>
              <th>{t("workspaces.list.col.dataPolicy")}</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {workspaces.map((w) => (
              <tr key={w.id}>
                <td>
                  <Link to={`/workspaces/${w.id}`}>{w.name}</Link>
                </td>
                <td>
                  {w.template.name}@{w.template.revision}
                </td>
                <td>
                  <PhaseBadge phase={w.phase} />
                  {isConnectable(w) ? null : w.phase === "Ready" ? (
                    <small> {t("workspaces.list.waitingConnection")}</small>
                  ) : null}
                </td>
                <td>{w.desiredState}</td>
                <td>{w.dataPolicy}</td>
                <td>
                  <Link to={`/workspaces/${w.id}`}>{t("workspaces.list.manage")}</Link>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </main>
  );
}
