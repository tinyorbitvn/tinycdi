import type { APIRequestContext, Page } from "@playwright/test";
import {
  DESKTOP_MARKER,
  expect,
  login,
  openSession,
  resetState,
  seedReadyWorkspace,
  sessionFrame,
  setConnectionStatus,
  takeoverDialogCount,
  test,
  waitForDesktopFrame,
  watchConsole,
  workspaceOrigin,
  type HarnessMode,
} from "./harness.ts";

// session-embed: the portal's in-portal session view launches a ticket into
// the session frame and the real backend serves the fake desktop page on the
// workspace's own host. Runs once per site layout:
//   [lax]         portal + session on tcdi.localhost (SameSite=Lax cookie)
//   [partitioned] session on tcdi-other.localhost (SameSite=None; Partitioned, D16)
const WS_A = "ws_e2e0001aabbcc";
const WS_B = "ws_e2e0002aabbcc";

async function embedRun(fixtures: {
  page: Page;
  request: APIRequestContext;
  harnessMode: HarnessMode;
}) {
  const { page, request, harnessMode } = fixtures;
  const violations = watchConsole(page);
  await resetState(request, harnessMode);
  await seedReadyWorkspace(request, WS_A);
  await login(page);

  const origin = workspaceOrigin(harnessMode, WS_A);
  const frame = await openSession(page, harnessMode, WS_A);
  await setConnectionStatus(request, WS_A, { state: "connected", leaseActive: true });

  // The frame settled on the workspace's own session host and shows the
  // fake desktop the real gateway proxied out of the fake upstream.
  expect(frame.url()).toBe(`${origin}/`);
  await expect(frame.locator("h1")).toContainText(DESKTOP_MARKER);

  expect(violations, "CSP violations on portal or session origin").toEqual([]);
}

test("lax", async ({ page, request, harnessMode }) => {
  test.skip(harnessMode !== "lax", "runs in the lax project");
  await embedRun({ page, request, harnessMode });
});

test("partitioned", async ({ page, request, harnessMode }) => {
  test.skip(harnessMode !== "partitioned", "runs in the partitioned project");
  await embedRun({ page, request, harnessMode });
});

// D10: two workspaces launched in one browser profile both stay connected —
// each session cookie is host-only on its own ws-* host.
test("two workspaces", async ({ page, context, request, harnessMode }) => {
  await resetState(request, harnessMode);
  await seedReadyWorkspace(request, WS_A);
  await seedReadyWorkspace(request, WS_B);
  await login(page);

  const page2 = await context.newPage();
  const frameA = await openSession(page, harnessMode, WS_A);
  const frameB = await openSession(page2, harnessMode, WS_B);
  for (const id of [WS_A, WS_B]) {
    await setConnectionStatus(request, id, { state: "connected", leaseActive: true });
  }

  await expect(frameA.locator("h1")).toContainText(DESKTOP_MARKER);
  await expect(frameB.locator("h1")).toContainText(DESKTOP_MARKER);

  // Both frames must still be alive after 30 s — the shared-profile rule of
  // D10 — and again after each portal tab reloads.
  await page.waitForTimeout(30_000);
  const originA = workspaceOrigin(harnessMode, WS_A);
  const originB = workspaceOrigin(harnessMode, WS_B);
  await expect(sessionFrame(page, originA)!.locator("h1")).toContainText(DESKTOP_MARKER);
  await expect(sessionFrame(page2, originB)!.locator("h1")).toContainText(DESKTOP_MARKER);

  await page.reload();
  await page2.reload();
  await waitForDesktopFrame(page, originA);
  await waitForDesktopFrame(page2, originB);
});

// Reload (F5) of the session tab on a live session of our own: the view
// resumes the frame with the existing cookie instead of minting a ticket, so
// the take-over dialog never shows. Runs in both cookie modes (lax and
// partitioned) because the resume depends on the session cookie surviving
// the reload.
test("reload of the session tab shows the desktop again without the take-over dialog", async ({
  page,
  request,
  harnessMode,
}) => {
  await resetState(request, harnessMode);
  await seedReadyWorkspace(request, WS_A);
  await login(page);

  const origin = workspaceOrigin(harnessMode, WS_A);
  await openSession(page, harnessMode, WS_A);
  await setConnectionStatus(request, WS_A, { state: "connected", leaseActive: true });

  const tickets: string[] = [];
  page.on("request", (r) => {
    if (r.method() === "POST" && /\/v1\/workspaces\/[^/]+\/connections$/.test(r.url())) {
      tickets.push(r.url());
    }
  });

  await page.reload();
  const frame = await waitForDesktopFrame(page, origin);
  await expect(frame.locator("h1")).toContainText(DESKTOP_MARKER);
  expect(await takeoverDialogCount(page)).toBe(0);
  expect(tickets, "resume must not request a ticket").toEqual([]);
});
