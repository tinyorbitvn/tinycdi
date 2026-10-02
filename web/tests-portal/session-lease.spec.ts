import type { Frame, Page } from "@playwright/test";
import {
  DESKTOP_MARKER,
  PORTAL_ORIGIN,
  expect,
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
  // that starts with a copy of the first tab's sessionStorage.
  const stored = await page.evaluate(() => JSON.stringify({ ...sessionStorage }));
  const dup = await context.newPage();
  await dup.goto("/");
  await dup.evaluate((json) => {
    for (const [k, v] of Object.entries(JSON.parse(json) as Record<string, string>)) {
      sessionStorage.setItem(k, v);
    }
  }, stored);
  await dup.goto(`/workspaces/${encodeURIComponent(WS_A)}/session`);

  // The duplicate resumes with the shared cookie; its frame opens a stream
  // on the lease (what the gateway reports as a higher stream epoch).
  await waitForDesktopFrame(dup, origin);
  await openStream(request, WS_A);

  await expect(dup.getByText("This session is open in another tab")).toHaveCount(0);
  await expect(statusBadge(dup)).toContainText("Connected", { timeout: 15_000 });
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
