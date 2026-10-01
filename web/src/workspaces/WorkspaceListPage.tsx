import { useEffect, useState } from "react";
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
    const t = setInterval(() => void load(), pollIntervalMs);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, [api, pollIntervalMs]);

  return (
    <main>
      <h1>Workspaces</h1>
      <nav>
        <Link to="/workspaces/new">New workspace</Link> ·{" "}
        <Link to="/templates">Template catalog</Link> ·{" "}
        <Link to="/data">Retained data</Link>
      </nav>
      <ErrorBanner error={error} onDismiss={() => setError(null)} />
      {!workspaces ? (
        <p aria-busy="true">Loading…</p>
      ) : workspaces.length === 0 ? (
        <p>No workspaces yet.</p>
      ) : (
        <table aria-label="workspaces">
          <thead>
            <tr>
              <th>Name</th>
              <th>Template</th>
              <th>Phase</th>
              <th>Desired</th>
              <th>Data policy</th>
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
                    <small> (waiting for ConnectionReady)</small>
                  ) : null}
                </td>
                <td>{w.desiredState}</td>
                <td>{w.dataPolicy}</td>
                <td>
                  <Link to={`/workspaces/${w.id}`}>Manage</Link>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </main>
  );
}
