import type { WorkspaceView } from "./helpers";

export function PhaseBadge({ phase }: { phase: WorkspaceView["phase"] }) {
  return <span className={`phase phase-${phase.toLowerCase()}`}>{phase}</span>;
}

export function ConditionsTable({ workspace }: { workspace: WorkspaceView }) {
  if (workspace.conditions.length === 0) {
    return <p>No conditions reported yet.</p>;
  }
  return (
    <table aria-label="conditions">
      <thead>
        <tr>
          <th>Type</th>
          <th>Status</th>
          <th>Reason</th>
          <th>Message</th>
          <th>Since</th>
        </tr>
      </thead>
      <tbody>
        {workspace.conditions.map((c) => (
          <tr key={c.type}>
            <td>{c.type}</td>
            <td>{c.status}</td>
            <td>{c.reason}</td>
            <td>{c.message ?? ""}</td>
            <td>{new Date(c.lastTransitionTime).toLocaleString()}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
