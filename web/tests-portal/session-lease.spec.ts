import type { Frame, Page } from "@playwright/test";
import {
  DESKTOP_MARKER,
  PORTAL_ORIGIN,
  expect,
  frameTabId,
  login,
  openStream,
  openSession,
  resetState,
  seedReadyWorkspace,
  sessionFrame,
  test,
  waitForDesktopFrame,
  workspaceOrigin,
} from "./harness.ts";

// session-lease: the session view only resumes a lease it provably held and
// steps aside when another tab streams on the same lease. Runs in both
// cookie modes.
const WS_A = "ws_e2e0001aabbcc";
const MARKER_PREFIX = "tcdi.session.owned.";

// The view records the lease it saw connected in the tab's sessionStorage;
// wait for that so a reload (or a duplicated tab) has something to resume.
async function waitForMarker(page: Page): Promise<void> {
  await expect
    .poll(
      () =>
        page.evaluate(
          (prefix) => Object.keys(sessionStorage).some((k) => k.startsWith(prefix)),
          MARKER_PREFIX,
        ),
      { timeout: 15_000 },
    )
    .toBe(true);
}

const statusBadge = (page: Page) => page.locator(".tc-session__status");

async function render(frame: Frame | undefined): Promise<string | null> {
  if (!frame) return null;
  return frame.locator("body").getAttribute("data-render", { timeout: 2_000 }).catch(() => null);
}

test("takeover from another browser, then reload: the dialog shows, not the old desktop", async ({
  page,
  browser,
  request,
  harnessMode,
}) => {
  await resetState(request, harnessMode);
  await seedReadyWorkspace(request, WS_A);
  await login(page);
  const origin = workspaceOrigin(harnessMode, WS_A);
  await openSession(page, harnessMode, WS_A);
  await waitForMarker(page);

  // A second browser (own cookie jar and tab storage) takes the session over.
  const other = await browser.newContext({ ignoreHTTPSErrors: true, baseURL: PORTAL_ORIGIN });
  const page2 = await other.newPage();
  await login(page2);
  await page2.goto(`/workspaces/${encodeURIComponent(WS_A)}/session`);
  await page2.getByRole("button", { name: "Take over session", exact: true }).click();
  await waitForDesktopFrame(page2, origin);

  // The first tab's marker names a lease that is gone. A reload must ask,
  // not resume: no desktop frame, not "Connected".
  await page.reload();
  await expect(
    page.getByRole("button", { name: "Take over session", exact: true }),
  ).toBeVisible({ timeout: 15_000 });
  await expect(statusBadge(page)).not.toContainText("Connected");
  const f = sessionFrame(page, origin);
  if (f) {
    expect(await f.locator("body").innerText().catch(() => "")).not.toContain(DESKTOP_MARKER);
  }
  await other.close();
});

test("duplicated tab: one shows the desktop, the other 'open in another tab', neither loops", async ({
  page,
  context,
  request,
  harnessMode,
}) => {
  await resetState(request, harnessMode);
  await seedReadyWorkspace(request, WS_A);
  await login(page);
  const origin = workspaceOrigin(harnessMode, WS_A);
  await openSession(page, harnessMode, WS_A);
  await waitForMarker(page);
  await expect(statusBadge(page)).toContainText("Connected");

  // "Duplicate tab": a new tab in the same browser profile (same cookies)
  // that starts with a copy of the first tab's sessionStorage — exactly
  // what browsers hand a duplicated tab. The stream-owner id itself is
  // memory-only (R-V3c), so the duplicate mints its OWN id; the copied
  // marker only lets it resume onto the lease, like a reload would.
  const stored = await page.evaluate(() => JSON.stringify({ ...sessionStorage }));
  const dup = await context.newPage();
  await dup.goto("/");
  await dup.evaluate((json) => {
    for (const [k, v] of Object.entries(JSON.parse(json) as Record<string, string>)) {
      sessionStorage.setItem(k, v);
    }
  }, stored);
  await dup.goto(`/workspaces/${encodeURIComponent(WS_A)}/session`);

  // The duplicate resumes: while its claim is pending a foreign owner is
  // "our claim hasn't landed yet", never 'elsewhere' — it navigates, and
  // its frame's claim records its own instance's tab id.
  const dupFrame = await waitForDesktopFrame(dup, origin);
  const dupTabId = frameTabId(dupFrame.url());
  await openStream(request, WS_A, dupTabId);

  await expect(dup.getByText("This session is open in another tab")).toHaveCount(0);
  await expect(statusBadge(dup)).toContainText("Connected", { timeout: 15_000 });
  // 'Elsewhere' lands on the tab that LOST the stream — a differing owner
  // id with no claim of its own pending.
  await expect(page.getByText("This session is open in another tab")).toBeVisible({
    timeout: 15_000,
  });
  await expect(page.getByRole("button", { name: "Use here" })).toBeVisible();

  // Neither tab reloads its frame or flips state while both sit there.
  const renders = async () => [await render(sessionFrame(page, origin)), await render(sessionFrame(dup, origin))];
  const before = await renders();
  await page.waitForTimeout(12_000);
  expect(await renders()).toEqual(before);
  await expect(page.getByText("This session is open in another tab")).toBeVisible();
  await expect(statusBadge(dup)).toContainText("Connected");
});

test("reload: the page reconnects under its fresh tab id — no 'elsewhere' flash", async ({
  page,
  request,
  harnessMode,
}) => {
  await resetState(request, harnessMode);
  await seedReadyWorkspace(request, WS_A);
  await login(page);
  const origin = workspaceOrigin(harnessMode, WS_A);
  await openSession(page, harnessMode, WS_A);
  await waitForMarker(page);
  await expect(statusBadge(page)).toContainText("Connected");

  // The reload mints a NEW owner id (memory-only, R-V3c). Until this page
  // instance's own claim lands, the lease still shows the previous id —
  // a foreign owner that must NOT verdict 'elsewhere' while the resume's
  // claim is pending (an early verdict is the reload flash).
  await page.reload();
  const foreign = "deadbeefdeadbeefdeadbeefdeadbeef";
  await openStream(request, WS_A, foreign);

  const frame = await waitForDesktopFrame(page, origin);
  expect(await page.getByText("This session is open in another tab").count()).toBe(0);

  // The reloaded frame's claim records the fresh id — the page is 'ours'
  // again without ever showing the dialog.
  await openStream(request, WS_A, frameTabId(frame.url()));
  await expect(statusBadge(page)).toContainText("Connected", { timeout: 15_000 });
  expect(await page.getByText("This session is open in another tab").count()).toBe(0);
});
