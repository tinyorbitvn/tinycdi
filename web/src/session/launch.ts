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

/** sessionDomain naming a loopback listener (mock e2e / dev harness). */
function isLoopbackDomain(sessionDomain: string): boolean {
  const host = sessionDomain.split(":")[0] ?? "";
  return (
    host === "localhost" ||
    host.endsWith(".localhost") ||
    host === "127.0.0.1" ||
    host === "::1" ||
    host === "[::1]"
  );
}

// SEC-26/D9: the ticket is bearer-equivalent, so its destination is pinned
// to the workspace's own host under the session domain /v1/me published.
// A launchUrl on any other host — a foreign domain, another workspace's
// host, a sub-label — is refused rather than submitted. The real backend
// always emits https (sessionhost.Domain.Origin); http launch URLs are
// accepted only on loopback session domains, where the contract mock and
// the e2e harness listen without TLS.
export function assertLaunchTarget(
  ticket: LaunchTicket,
  workspaceId: string,
  sessionDomain: string,
): string {
  let url: URL;
  try {
    url = new URL(ticket.launchUrl);
  } catch {
    throw new Error("malformed launchUrl in connection response");
  }
  if (sessionDomain === "") {
    throw new Error("no session domain configured; refusing to launch");
  }
  const expectedHost = `${sessionLabel(workspaceId)}.${sessionDomain}`;
  const schemeOK =
    url.protocol === "https:" ||
    (url.protocol === "http:" && isLoopbackDomain(sessionDomain));
  if (url.host !== expectedHost || !schemeOK) {
    throw new Error(
      `refusing to POST launch ticket to ${url.origin}: expected ${sessionOrigin(workspaceId, sessionDomain)}`,
    );
  }
  return url.origin;
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

// Ownership marker. The session cookie is HttpOnly and lives on another
// origin, so the portal cannot ask whether *this browser* holds the live
// lease. A tab remembers which lease (leaseRef) and which stream (epoch) it
// last saw connected, for the lifetime of the tab (survives F5 and in-portal
// navigation, not a new tab or another browser). A remount resumes only if
// /connection still reports the same leaseRef: the marker alone, or the
// workspace merely being "connected", proves nothing about who holds the
// lease. The value holds the public lease reference and an integer, never a
// ticket, cookie or lease ID.
const OWNED_KEY_PREFIX = "tcdi.session.owned.";

export interface SessionMarker {
  leaseRef: string;
  streamEpoch: number;
}

export function markSessionOwned(workspaceId: string, marker: SessionMarker): void {
  try {
    sessionStorage.setItem(OWNED_KEY_PREFIX + workspaceId, JSON.stringify(marker));
  } catch {
    /* storage unavailable: the page falls back to a ticket request */
  }
}

export function readSessionMarker(workspaceId: string): SessionMarker | null {
  try {
    const raw = sessionStorage.getItem(OWNED_KEY_PREFIX + workspaceId);
    if (raw === null) return null;
    const v: unknown = JSON.parse(raw);
    if (typeof v !== "object" || v === null) return null;
    const { leaseRef, streamEpoch } = v as Partial<SessionMarker>;
    if (typeof leaseRef !== "string" || leaseRef === "") return null;
    if (typeof streamEpoch !== "number" || !Number.isInteger(streamEpoch)) return null;
    return { leaseRef, streamEpoch };
  } catch {
    return null;
  }
}

export function clearSessionOwned(workspaceId: string): void {
  try {
    sessionStorage.removeItem(OWNED_KEY_PREFIX + workspaceId);
  } catch {
    /* nothing stored */
  }
}
