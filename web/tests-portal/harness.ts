// Shared constants and fixtures for the real-binary e2e harness
// (tests-portal). Two host layouts run the same spec bodies:
//
//   mode          portal origin                       session domain
//   lax           https://portal.tcdi.localhost:4174  session.tcdi.localhost:4312
//   partitioned   https://portal.tcdi.localhost:4174  session.tcdi-other.localhost:4313
//
// `lax` keeps portal and session on one registrable domain (SameSite=Lax
// session cookie); `partitioned` puts the session domain on a different
// site so the cookie must be SameSite=None; Partitioned (D16). Every
// *.localhost name resolves to loopback in both the browser and the test
// runner, so no /etc/hosts or DNS setup is needed.
//
// Layout (all started by serve.ts):
//   :4174  https portal router -> frontend binary (SPA) for paths outside
//          /v1 and /_control, mock API for the rest. The router rewrites
//          Me.sessionDomain and LaunchTicket.launchUrl onto the active
//          mode's domain and registers minted tickets with the fake
//          broker — the split-mode backend has no API of its own.
//   :4175  https "evil" origin used by the frame-ancestors spec.
//   :4176  http harness control (loopback only): mode switch, backend
//          restart, desktop behaviours, broker reset.
//   :4184/:4185  https real frontend binaries, one per mode (each gets its
//          own -session-domain so its CSP frame-src names the right
//          wildcard).
//   :4312/:4313  https real backend session listeners (split mode), lax on
//          :4312 and partitioned on :4313.
//   :4320  http contract mock API (tests/mock-api).
//   :4390  https fake KasmVNC upstream (what the gateway proxies to).
//   :4391  https+mTLS fake broker (internal contract: redeem/renew/target/
//          revoke/activity).

import { test as base, expect, type APIRequestContext, type Frame, type Page } from "@playwright/test";

export type HarnessMode = "lax" | "partitioned";

export const PORTAL_PORT = Number(process.env.TCDI_E2E_PORTAL_PORT ?? 4174);
export const EVIL_PORT = Number(process.env.TCDI_E2E_EVIL_PORT ?? 4175);
export const CONTROL_PORT = Number(process.env.TCDI_E2E_CONTROL_PORT ?? 4176);
export const MOCK_API_PORT = Number(process.env.TCDI_E2E_MOCK_PORT ?? 4320);
export const UPSTREAM_PORT = Number(process.env.TCDI_E2E_UPSTREAM_PORT ?? 4390);
export const BROKER_PORT = Number(process.env.TCDI_E2E_BROKER_PORT ?? 4391);
export const FRONTEND_PORTS: Record<HarnessMode, number> = {
  lax: Number(process.env.TCDI_E2E_FRONTEND_LAX_PORT ?? 4184),
  partitioned: Number(process.env.TCDI_E2E_FRONTEND_PARTITIONED_PORT ?? 4185),
};

export const PORTAL_ORIGIN = `https://portal.tcdi.localhost:${PORTAL_PORT}`;
export const EVIL_ORIGIN = `https://evil.tcdi.localhost:${EVIL_PORT}`;
export const MOCK_API = `http://127.0.0.1:${MOCK_API_PORT}`;
export const HARNESS_CONTROL = `http://127.0.0.1:${CONTROL_PORT}`;

// Session domain (host:port) per mode — the value the backend gets via
// -session-domain and /v1/me returns as sessionDomain.
export const SESSION_DOMAINS: Record<HarnessMode, string> = {
  lax: `session.tcdi.localhost:${process.env.TCDI_E2E_SESSION_LAX_PORT ?? 4312}`,
  partitioned: `session.tcdi-other.localhost:${process.env.TCDI_E2E_SESSION_PARTITIONED_PORT ?? 4313}`,
};

// workspaceLabel mirrors sessionhost.Label / the web sessionLabel helper:
// platform ID "ws_<hex>" -> DNS label "ws-<hex>".
export function workspaceLabel(workspaceId: string): string {
  return workspaceId.replaceAll("_", "-");
}

export function workspaceHost(mode: HarnessMode, workspaceId: string): string {
  return `${workspaceLabel(workspaceId)}.${SESSION_DOMAINS[mode]}`;
}

export function workspaceOrigin(mode: HarnessMode, workspaceId: string): string {
  return `https://${workspaceHost(mode, workspaceId)}`;
}

// Marker text every fake-desktop page serves (tests-portal/serve.ts).
export const DESKTOP_MARKER = "desktop session";

// Unbuffered debug sink — Playwright batches test stdout until the test
// ends, which hides timing. TCDI_E2E_DEBUG=1 writes to this file instead.
import { appendFileSync } from "node:fs";
const DBG_ON = process.env.TCDI_E2E_DEBUG === "1";
const DBG_FILE = process.env.TCDI_E2E_DEBUG_FILE ?? "/tmp/t26-e2e-debug.log";
function dbg(msg: string) {
  if (DBG_ON) appendFileSync(DBG_FILE, `${new Date().toISOString()} ${msg}\n`);
}

// Extend the base test with a per-project harnessMode option; projects in
// playwright.portal.config.ts set it through `use`.
export const test = base.extend<{ harnessMode: HarnessMode }>({
  harnessMode: ["lax", { option: true }],
});

export { expect };

// ---------------------------------------------------------------------------
// spec-side helpers
// ---------------------------------------------------------------------------

// Conditions mirroring the contract mock's "ready to connect" fixture.
const READY_CONDITIONS = [
  { type: "Admitted", status: "True", reason: "QuotaReserved", lastTransitionTime: "2026-09-30T10:00:00Z" },
  { type: "StorageReady", status: "True", reason: "VolumeBound", lastTransitionTime: "2026-09-30T10:01:00Z" },
  { type: "RuntimeReady", status: "True", reason: "RuntimeUp", lastTransitionTime: "2026-09-30T10:02:00Z" },
  { type: "ConnectionReady", status: "True", reason: "StreamEndpointUp", lastTransitionTime: "2026-09-30T10:02:30Z" },
];

// resetState clears the mock API and the fake broker, then selects the
// session site this test runs against.
export async function resetState(request: APIRequestContext, mode: HarnessMode) {
  for (const [url, data] of [
    [`${MOCK_API}/_control/reset`, {}],
    [`${HARNESS_CONTROL}/broker/reset`, {}],
    [`${HARNESS_CONTROL}/desktop`, { topNav: false, popup: false }],
    [`${HARNESS_CONTROL}/mode`, { mode }],
  ] as const) {
    const res = await request.post(url, { data });
    expect(res.ok(), `control call ${url}`).toBeTruthy();
  }
}

// seedReadyWorkspace upserts a Ready/Running workspace in the mock API. The
// id must be a valid sessionhost label base ("ws_" + [a-z0-9]{8,60}) — the
// real gateway derives the session host label from it.
export async function seedReadyWorkspace(request: APIRequestContext, id: string) {
  const res = await request.put(`${MOCK_API}/_control/workspaces/${id}`, {
    data: { phase: "Ready", desiredState: "Running", conditions: READY_CONDITIONS },
  });
  expect(res.ok(), `seed workspace ${id}`).toBeTruthy();
}

// setConnectionStatus scripts the mock's GET /v1/workspaces/{id}/connection
// answer (the session area's scriptable endpoint) for the reconnect spec.
export async function setConnectionStatus(
  request: APIRequestContext,
  workspaceId: string,
  status: {
    state: string;
    leaseActive: boolean;
    leaseRef?: string;
    streamEpoch?: number;
    streamOwnerTab?: string;
  },
) {
  const res = await request.post(`${MOCK_API}/_control/session/connection`, {
    data: { workspaceId, ...status },
  });
  expect(res.ok(), "script /connection status").toBeTruthy();
}

// openStream tells the mock a new stream opened on the workspace's current
// lease (its stream epoch advances; an optional streamOwnerTab lands on the
// same "write") — what the real gateway reports when a frame connects with
// the lease's session cookie.
export async function openStream(request: APIRequestContext, workspaceId: string, streamOwnerTab?: string) {
  const res = await request.post(`${MOCK_API}/_control/session/stream`, {
    data: { workspaceId, ...(streamOwnerTab !== undefined ? { streamOwnerTab } : {}) },
  });
  expect(res.ok(), "open a stream on the mock lease").toBeTruthy();
  return (await res.json()) as { leaseRef: string; streamEpoch: number };
}

// frameTabId reads this page instance's stream-owner id off the frame's
// `path` URL setting (path=websockify?tcdi_tab=<id>) — the same channel the
// real KasmVNC client sends it on with every websocket claim (FX-R31).
export function frameTabId(frameUrl: string): string {
  const path = new URL(frameUrl).searchParams.get("path") ?? "";
  const m = path.match(/tcdi_tab=([0-9a-f]{32})/);
  if (!m) throw new Error(`frame URL carries no tcdi_tab: ${frameUrl}`);
  return m[1];
}

// clearLease drops the mock's lease record for the workspace — the broker-
// side half of "the session is gone" in the reconnect spec (with it still
// present, the relaunch ticket mint would 409 CONNECTION_IN_USE).
export async function clearLease(request: APIRequestContext, workspaceId: string) {
  const res = await request.post(`${MOCK_API}/_control/lease`, {
    data: { workspaceId, active: false },
  });
  expect(res.ok(), "clear mock lease").toBeTruthy();
}

export async function setDesktopBehaviour(
  request: APIRequestContext,
  b: { topNav?: boolean; popup?: boolean },
) {
  const res = await request.post(`${HARNESS_CONTROL}/desktop`, { data: b });
  expect(res.ok()).toBeTruthy();
}

export async function restartBackend(request: APIRequestContext, mode: HarnessMode) {
  const res = await request.post(`${HARNESS_CONTROL}/backend/restart`, {
    data: { mode },
    timeout: 45_000,
  });
  expect(res.ok(), "backend restart").toBeTruthy();
}

// login drives the mock's fake-SSO round trip on the portal origin.
export async function login(page: Page) {
  await page.goto("/v1/login?returnTo=/");
  await page.getByRole("button", { name: "Log in with SSO" }).click();
  await page.waitForURL("/");
}

// The desktop frame is located by its origin — the DOM attributes of the
// portal's iframe are not part of this suite's contract.
export function sessionFrame(page: Page, origin: string): Frame | undefined {
  return page
    .frames()
    .find((f) => f.url().startsWith(origin) && !f.url().includes("/v1/launch"));
}

// takeoverDialogCount reports how many "Take over session" buttons the
// session view shows right now (count() is non-waiting, so it never blocks
// the poll loop below).
export async function takeoverDialogCount(page: Page): Promise<number> {
  return page
    .getByRole("button", { name: "Take over session", exact: true })
    .count()
    .catch(() => 0);
}

// waitForDesktopFrame polls until a frame on `origin` serves the fake
// desktop marker. The session view resumes or launches by itself, so no
// click is ever needed: if the "Take over session" dialog shows up, the
// session view asked the user to take a live session over from themselves,
// and the test fails instead of papering over it by clicking through.
export async function waitForDesktopFrame(
  page: Page,
  origin: string,
  timeoutMs = 30_000,
): Promise<Frame> {
  // A frame mid-navigation (the launch POST -> 303 -> document swap) can
  // leave locator/evaluate calls pending for far longer than our poll
  // cadence — race every probe against a hard deadline so the loop always
  // keeps moving.
  const probeFrame = (f: Frame) =>
    Promise.race([
      f.evaluate(
        (m) => (document.body?.innerText ?? "").includes(m),
        DESKTOP_MARKER,
      ),
      new Promise<boolean>((res) => setTimeout(() => res(false), 2_500)),
    ]).catch(() => false);

  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const frame = sessionFrame(page, origin);
    if (frame) {
      if (await probeFrame(frame)) return frame;
      dbg(`frame ${frame.url()} not ready`);
    } else {
      dbg(`no frame on ${origin}; frames=${JSON.stringify(page.frames().map((f) => f.url()))}`);
    }
    expect(await takeoverDialogCount(page), "unexpected take-over dialog").toBe(0);
    if (Date.now() > deadline) break;
    await page.waitForTimeout(300);
  }
  const frame = sessionFrame(page, origin);
  expect(frame, `session frame on ${origin}`).toBeTruthy();
  await expect(frame!.locator("h1")).toContainText(DESKTOP_MARKER, { timeout: 5_000 });
  return frame!;
}

// openSession navigates to the in-portal session view and waits for the
// frame on the workspace's own host to serve the fake desktop page.
export async function openSession(
  page: Page,
  mode: HarnessMode,
  workspaceId: string,
): Promise<Frame> {
  const origin = workspaceOrigin(mode, workspaceId);
  if (DBG_ON) {
    page.on("console", (m) => {
      if (m.type() === "error" || m.type() === "warning")
        dbg(`[console.${m.type()}] ${m.text().slice(0, 200)}`);
    });
    page.on("pageerror", (e) => dbg(`[pageerror] ${String(e).slice(0, 200)}`));
    page.on("requestfailed", (r) =>
      dbg(`[reqfail] ${r.url()} ${r.failure()?.errorText}`));
    page.on("response", (r) => {
      if (r.status() >= 400) dbg(`[http ${r.status()}] ${r.url()}`);
    });
    page.on("request", (r) => {
      const u = r.url();
      if (u.includes("session.tcdi") || u.includes("/connections"))
        dbg(`[req ${r.method()}] ${u}`);
    });
  }
  await page.goto(`/workspaces/${encodeURIComponent(workspaceId)}/session`);
  dbg(`openSession: page loaded ${page.url()}`);
  return waitForDesktopFrame(page, origin);
}

// watchConsole collects Content-Security-Policy violation console messages
// for a page and every page/frame later added to its context.
export function watchConsole(page: Page): string[] {
  const violations: string[] = [];
  const onConsole = (m: { text: () => string }) => {
    if (/content security policy/i.test(m.text())) violations.push(m.text());
  };
  page.on("console", onConsole);
  page.context().on("page", (p) => p.on("console", onConsole));
  return violations;
}

// Chromium's own advisory about the pinned frame sandbox (D13): the desktop
// client needs allow-scripts and allow-same-origin together. It is a warning
// from the browser about the portal's deliberate choice, not an app error.
const KNOWN_SANDBOX_WARNING =
  "An iframe which has both allow-scripts and allow-same-origin for its sandbox attribute can escape its sandboxing.";

// watchConsoleErrors collects every console error/warning and uncaught page
// error from a page and any page/frame later added to its context, so an
// embedded session load can be held to a zero-noise console (FX-R22). The one
// known D13 sandbox warning above is the only message let through.
export function watchConsoleErrors(page: Page): string[] {
  const seen: string[] = [];
  const onConsole = (m: { type: () => string; text: () => string }) => {
    if (m.type() !== "error" && m.type() !== "warning") return;
    if (m.text() === KNOWN_SANDBOX_WARNING) return;
    seen.push(`${m.type()}: ${m.text()}`);
  };
  const onPage = (p: Page) => {
    p.on("console", onConsole);
    p.on("pageerror", (e) => seen.push(`pageerror: ${e.message}`));
  };
  onPage(page);
  page.context().on("page", onPage);
  return seen;
}
