import type { components } from "../api/generated/schema";

export type LaunchTicket = components["schemas"]["LaunchTicket"];

// Form field name carrying the ticket in the POST body to the session
// origin. Never a query parameter: the ticket must not appear in URLs,
// history, referers or logs (ADR 0001/0002).
export const TICKET_FIELD = "ticket";

/** Portal route hosting the in-portal session view. */
export function sessionPath(workspaceId: string): string {
  return `/workspaces/${encodeURIComponent(workspaceId)}/session`;
}

/**
 * Browsing-context name of the session iframe. The launch form targets it
 * by name, so the gateway's 303 after redemption lands inside the frame.
 */
export function sessionFrameName(workspaceId: string): string {
  return `tcdi-session-${workspaceId}`;
}

/**
 * Workspace ID -> session host label (internal/sessionhost.Label):
 * "ws_<suffix>" becomes "ws-<suffix>". "_" is invalid in a DNS label.
 */
export function sessionLabel(workspaceId: string): string {
  return workspaceId.replaceAll("_", "-").toLowerCase();
}

/**
 * Origin of the workspace's own session host:
 * https://ws-<suffix>.<sessionDomain>, where sessionDomain is the
 * host[:port] published by GET /v1/me (D9, P5).
 */
export function sessionOrigin(workspaceId: string, sessionDomain: string): string {
  return `https://${sessionLabel(workspaceId)}.${sessionDomain}`;
}

/** Sandbox flags on the session iframe (D13). Navigation and dialogs stay out. */
export const SESSION_FRAME_SANDBOX =
  "allow-scripts allow-same-origin allow-forms allow-pointer-lock";

/** Permissions delegated to the session frame (clipboard + fullscreen). */
export const SESSION_FRAME_ALLOW = "clipboard-read; clipboard-write; fullscreen";

// SEC-26/D9: the ticket is bearer-equivalent, so its destination is pinned
// to the workspace's own host under the session domain /v1/me published.
// A launchUrl on any other origin — a foreign domain, another workspace's
// host, a sub-label — is refused rather than submitted.
export function assertLaunchTarget(
  ticket: LaunchTicket,
  workspaceId: string,
  sessionDomain: string,
): string {
  let origin: string;
  try {
    origin = new URL(ticket.launchUrl).origin;
  } catch {
    throw new Error("malformed launchUrl in connection response");
  }
  if (sessionDomain === "") {
    throw new Error("no session domain configured; refusing to launch");
  }
  const expected = sessionOrigin(workspaceId, sessionDomain);
  if (origin !== expected) {
    throw new Error(
      `refusing to POST launch ticket to ${origin}: expected ${expected}`,
    );
  }
  return expected;
}

/**
 * POSTs the ticket to the session origin with `target` set to a browsing
 * context: the session iframe's name, or `_blank` for a new tab. The ticket
 * lives only in this transient form's POST body — never in a URL, history
 * entry or web storage.
 */
export function submitLaunch(
  ticket: LaunchTicket,
  target: string,
  workspaceId: string,
  sessionDomain: string,
): void {
  assertLaunchTarget(ticket, workspaceId, sessionDomain);
  const form = document.createElement("form");
  form.method = "POST";
  form.action = ticket.launchUrl;
  form.target = target;
  if (target === "_blank") form.rel = "noopener";
  form.hidden = true;
  const input = document.createElement("input");
  input.type = "hidden";
  input.name = TICKET_FIELD;
  input.value = ticket.ticket;
  form.append(input);
  document.body.append(form);
  try {
    form.submit();
  } finally {
    form.remove();
  }
}

/** Fallback launch: opens the desktop in a new tab. */
export function launchInNewTab(
  ticket: LaunchTicket,
  workspaceId: string,
  sessionDomain: string,
): void {
  submitLaunch(ticket, "_blank", workspaceId, sessionDomain);
}
