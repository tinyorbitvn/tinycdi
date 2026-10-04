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

/** The template's clipboard policy, as published by GET /v1/templates. */
export type ClipboardPolicy = "Disabled" | "Send" | "Receive" | "Bidirectional";

// Stream-owner tab id (FX-R31, R-V3c). The page mints one random 128-bit
// id per PAGE INSTANCE and keeps it in memory only — never in
// sessionStorage, which browsers COPY on "duplicate tab" and
// reopen-closed-tab: a stored id would be shared by the copy and both
// tabs would claim the same owner, defeating the check entirely.
// A reload mints a new id; this instance's frame claims record it, and
// until a claim lands the page withholds its 'elsewhere' verdict (see
// observe() in SessionPage). The id travels to the broker inside the
// KasmVNC client's `path` URL setting (path=websockify?tcdi_tab=<id>):
// the client rebuilds its websocket URL from that setting on every
// connect and retry, so every stream claim from this frame carries the
// same id — including the re-claims a backend restart or rollout
// triggers. GET /connection then reports the claim's owner, and an epoch
// advance is no longer mistaken for a foreign tab.
let tabId: string | undefined;

function mintTabId(): string {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

/**
 * This page instance's stream-owner id: a 32-char lowercase hex string
 * (128 bits, crypto.getRandomValues), minted once per page load. Not
 * persisted anywhere — persistence is precisely what a duplicated tab
 * shares.
 */
export function sessionTabId(): string {
  if (tabId === undefined) tabId = mintTabId();
  return tabId;
}

/** The stream-claim parameter the id travels in on the websocket URL. */
export const STREAM_TAB_PARAM = "tcdi_tab";

/** Options the workspace's policy gives the embedded client. */
export interface SessionEmbedOpts {
  /**
   * The workspace template's clipboard policy. `undefined` = not yet known:
   * least privilege applies until the template resolves.
   */
  clipboardPolicy?: ClipboardPolicy | undefined;
}

/**
 * Which clipboard directions the policy allows, in the KasmVNC client's
 * terms: `up` is client→workspace (`Send`), `down` is workspace→client
 * (`Receive`). Anything not listed stays off.
 */
export function clipboardDirections(policy: ClipboardPolicy | undefined): {
  up: boolean;
  down: boolean;
} {
  switch (policy) {
    case "Bidirectional":
      return { up: true, down: true };
    case "Send":
      return { up: true, down: false };
    case "Receive":
      return { up: false, down: true };
    default:
      return { up: false, down: false };
  }
}

// clipboard_seamless mirrors the client's own non-embed default: on for
// Chrome-family browsers, off on Firefox and Safari — upstream disables it
// there itself (the Firefox "Paste" overlay, Safari's missing
// navigator.clipboard.read), so the portal only turns it on where the
// client would have.
function seamlessClipboardOK(): boolean {
  const ua = navigator.userAgent;
  if (/firefox/i.test(ua)) return false;
  return !(ua.includes("Safari") && !ua.includes("Chrome"));
}

/**
 * URL the session iframe is pointed at to (re)load the desktop client. The
 * KasmVNC client treats a page inside an iframe as an embedded widget
 * (`window.self !== window.top`) and, unless `show_control_bar` is set,
 * forces `resize=off`, `enable_webp=false` and every clipboard direction
 * off (FX-R18, client source: initSetting block in ui-*.js). Every URL
 * setting wins over an initSetting, so the portal re-asserts tab-mode
 * behaviour it wants:
 *
 * - `resize=remote` — the remote screen tracks the frame size; without it a
 *   frame larger than the remote shows large dark regions.
 * - `enable_webp` — same codec offer as a top-level tab.
 * - `idle_disconnect=1440` (minutes, i.e. 24 h): the client's own idle cut
 *   (default 20 min) would bounce the frame to disconnected.html well
 *   before the platform lifecycle does. Idle policy belongs to the
 *   workspace template, not to a second, unsynchronized client timer.
 * - `clipboard_up`/`clipboard_down`/`clipboard_seamless` — per the
 *   workspace's clipboard policy, on top of the server-side DLP policy
 *   (which today denies all directions; client flags gate the client half).
 *   `clipboard_seamless` only where the browser supports it, like the
 *   client's own non-embed default.
 *
 * `show_control_bar` stays unset deliberately: it would restore ALL
 * non-embed defaults including the Kasm control bar — a second settings
 * UI inside the portal's own toolbar (V3.24 decision).
 *
 * `path` carries this tab's stream-owner id (FX-R31): the client builds
 * its websocket URL from the `path` setting, so
 * `path=websockify?tcdi_tab=<id>` lands the id on every stream claim this
 * frame makes — first connect and every retry alike.
 */
export function sessionFrameUrl(
  workspaceId: string,
  sessionDomain: string,
  opts: SessionEmbedOpts = {},
): string {
  const { up, down } = clipboardDirections(opts.clipboardPolicy);
  const q = new URLSearchParams({
    resize: "remote",
    enable_webp: "true",
    idle_disconnect: "1440",
    clipboard_up: String(up),
    clipboard_down: String(down),
  });
  if ((up || down) && seamlessClipboardOK()) q.set("clipboard_seamless", "true");
  q.set("path", `websockify?${STREAM_TAB_PARAM}=${sessionTabId()}`);
  return `${sessionOrigin(workspaceId, sessionDomain)}/?${q}`;
}

/** Sandbox flags on the session iframe (D13). Navigation and dialogs stay out. */
export const SESSION_FRAME_SANDBOX =
  "allow-scripts allow-same-origin allow-forms allow-pointer-lock";

/**
 * Features delegated to the session frame, least privilege on top of the
 * server-side clipboard policy: `Send` (client→workspace) needs the frame
 * to READ the local clipboard (`clipboard-read`), `Receive` needs WRITE
 * (`clipboard-write`); `Bidirectional` gets both, `Disabled` neither.
 * Fullscreen and `keyboard-map` always: the client maps non-US layouts
 * through getLayoutMap() (without it a German Ctrl+Z becomes Ctrl+Y on the
 * remote — wrong keys, V3.24 layout check).
 */
export function sessionFrameFeatures(policy: ClipboardPolicy | undefined): readonly string[] {
  const { up, down } = clipboardDirections(policy);
  return [
    ...(up ? ["clipboard-read"] : []),
    ...(down ? ["clipboard-write"] : []),
    "fullscreen",
    "keyboard-map",
  ];
}

/**
 * The iframe's allow attribute. Each feature names the workspace's session
 * origin explicitly: the frame has no src attribute (the launch form POST
 * navigates it), so the bare-feature default, 'src', resolves to the portal's
 * own origin and delegated nothing to the session — clipboard, fullscreen and
 * getLayoutMap() were all refused inside the frame (FX-R22). Empty until the
 * session domain is known, when nothing can be launched anyway.
 */
export function sessionFrameAllow(
  workspaceId: string,
  sessionDomain: string,
  opts: SessionEmbedOpts = {},
): string {
  if (sessionDomain === "") return "";
  const origin = sessionOrigin(workspaceId, sessionDomain);
  return sessionFrameFeatures(opts.clipboardPolicy)
    .map((feature) => `${feature} ${origin}`)
    .join("; ");
}

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
  // The redemption 303 is the navigation that loads the desktop client, so
  // this tab's owner id rides the POST's query (never the ticket — that
  // stays in the body) for the gateway to re-assert on the redirect.
  const action = new URL(ticket.launchUrl);
  action.searchParams.set(STREAM_TAB_PARAM, sessionTabId());
  const form = document.createElement("form");
  form.method = "POST";
  form.action = action.toString();
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
  /**
   * The confirmed lease, or "" while provisional: the tab minted a launch
   * ticket and never observed the lease it created (a reload landing inside
   * the ~seconds between ticket POST and the first connected report). A
   * provisional marker accepts whichever lease is live on remount — the
   * ticket only ever creates a lease for this tab.
   */
  leaseRef: string;
  /** The confirmed stream epoch, or -1 while provisional. */
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
    if (typeof leaseRef !== "string") return null;
    if (typeof streamEpoch !== "number" || !Number.isInteger(streamEpoch)) return null;
    // Provisional ("" + -1) or confirmed (ref + epoch >= 0); anything else
    // is corrupt.
    if (leaseRef === "") return streamEpoch === -1 ? { leaseRef, streamEpoch } : null;
    return streamEpoch >= 0 ? { leaseRef, streamEpoch } : null;
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
