import {
  DESKTOP_MARKER,
  clearLease,
  expect,
  frameTabId,
  login,
  openSession,
  openStream,
  resetState,
  restartBackend,
  seedReadyWorkspace,
  sessionFrame,
  setConnectionStatus,
  test,
  workspaceOrigin,
} from "./harness.ts";

// session-reconnect: kill the split-mode backend mid-session. There is no
// rehydration in split mode (P6), so the portal must notice via the
// connection watch (no postMessage exists, D15), mint a fresh ticket and
// submit it into the frame — the desktop page comes back by itself.
const WS_A = "ws_e2e0001aabbcc";

test("backend restart mid-session relaunches the frame", async ({
  page,
  request,
  harnessMode,
}) => {
  await resetState(request, harnessMode);
  await seedReadyWorkspace(request, WS_A);
  await login(page);

  const origin = workspaceOrigin(harnessMode, WS_A);
  const frame = await openSession(page, harnessMode, WS_A);
  await setConnectionStatus(request, WS_A, { state: "connected", leaseActive: true });
  const renderBefore = await frame.locator("body").getAttribute("data-render");
  expect(renderBefore).toBeTruthy();

  // Kill and respawn the backend for this site; the loopback control call
  // returns once the new listener answers.
  await restartBackend(request, harnessMode);

  // The broker still holds the lease row shape the API reports: from the
  // portal's view the session died with the backend, the lease is gone.
  // Drop the mock's lease record too, or the relaunch ticket mint would
  // answer 409 CONNECTION_IN_USE.
  await clearLease(request, WS_A);
  await setConnectionStatus(request, WS_A, { state: "disconnected", leaseActive: false });

  // Within 15 s and without a click the session view must have re-launched:
  // a new ticket redeemed on the restarted backend and a FRESH upstream
  // document (render counter advanced) inside the same frame.
  await expect
    .poll(
      async () => {
        const f = sessionFrame(page, origin);
        if (!f) return false;
        try {
          const render = await f.locator("body").getAttribute("data-render", { timeout: 500 });
          const h1 = await f.locator("h1").innerText({ timeout: 500 });
          return h1.includes(DESKTOP_MARKER) && Number(render) > Number(renderBefore);
        } catch {
          return false;
        }
      },
      { timeout: 15_000 },
    )
    .toBe(true);
});

// FX-R32: a stream drop while the lease is still live must NOT re-navigate
// the frame — the KasmVNC client inside it gets the in-frame window to
// re-claim the stream on its own websocket. The poll going quiet->connected
// without a navigation is what the advisor's rollout drill needs at N=60.
test("stream drop with a live lease resumes without a frame navigation", async ({
  page,
  request,
  harnessMode,
}) => {
  await resetState(request, harnessMode);
  await seedReadyWorkspace(request, WS_A);
  await login(page);

  const origin = workspaceOrigin(harnessMode, WS_A);
  const frame = await openSession(page, harnessMode, WS_A);
  const renderBefore = await frame.locator("body").getAttribute("data-render");
  const tabId = frameTabId(frame.url());

  // Every subframe navigation after this point is a breach of the
  // in-frame-reconnect contract.
  const navs: string[] = [];
  page.on("framenavigated", (f) => {
    if (f !== page.mainFrame()) navs.push(f.url());
  });

  // The stream dies but the lease stays live, owner id still ours — what a
  // drained backend pod reports.
  await setConnectionStatus(request, WS_A, {
    state: "disconnected",
    leaseActive: true,
    streamOwnerTab: tabId,
  });
  await expect(page.getByText("Reconnecting")).toBeVisible({ timeout: 10_000 });
  // Inside the in-frame window nothing may navigate the frame.
  await page.waitForTimeout(2_000);
  expect(navs).toHaveLength(0);

  // The frame's own retry re-claims the stream (same tab id, epoch +1):
  // the poll reports connected and the page settles on its own.
  const stream = await openStream(request, WS_A, tabId);
  await setConnectionStatus(request, WS_A, {
    state: "connected",
    leaseActive: true,
    leaseRef: stream.leaseRef,
    streamEpoch: stream.streamEpoch,
    streamOwnerTab: tabId,
  });
  await expect(page.getByText("Connected")).toBeVisible({ timeout: 10_000 });

  // Well past the would-be navigation deadline: still no navigation, and
  // the desktop document was never reloaded.
  await page.waitForTimeout(6_000);
  expect(navs).toHaveLength(0);
  const f = sessionFrame(page, origin);
  expect(f).toBeTruthy();
  await expect(f!.locator("body")).toHaveAttribute("data-render", renderBefore!);
  await expect(f!.locator("h1")).toContainText(DESKTOP_MARKER);
});

// FX-R32: when the in-frame window runs out (the client's retry never
// lands), the watch re-navigates the frame exactly once — bounded backoff
// keeps a nav->claim in flight instead of aborting it every second.
test("a lease-active drop past the in-frame window reloads the frame once", async ({
  page,
  request,
  harnessMode,
}) => {
  await resetState(request, harnessMode);
  await seedReadyWorkspace(request, WS_A);
  await login(page);

  const origin = workspaceOrigin(harnessMode, WS_A);
  const frame = await openSession(page, harnessMode, WS_A);
  const renderBefore = await frame.locator("body").getAttribute("data-render");
  const tabId = frameTabId(frame.url());
  const navs: string[] = [];
  page.on("framenavigated", (f) => {
    if (f !== page.mainFrame()) navs.push(f.url());
  });

  await setConnectionStatus(request, WS_A, {
    state: "disconnected",
    leaseActive: true,
    streamOwnerTab: tabId,
  });
  await expect(page.getByText("Reconnecting")).toBeVisible({ timeout: 10_000 });

  // The window is ~5 s after the first loss report: the reload comes later
  // than that and exactly once.
  const f2 = sessionFrame(page, origin)!;
  await expect
    .poll(
      async () =>
        Number(await f2.locator("body").getAttribute("data-render", { timeout: 500 }).catch(() => null)),
      { timeout: 20_000 },
    )
    .toBeGreaterThan(Number(renderBefore));
  expect(navs.length).toBeGreaterThanOrEqual(1);
  const navCount = navs.length;
  await page.waitForTimeout(6_000);
  expect(navs.length).toBe(navCount);

  // The reloaded client claims the stream: connected again.
  const stream = await openStream(request, WS_A, tabId);
  await setConnectionStatus(request, WS_A, {
    state: "connected",
    leaseActive: true,
    leaseRef: stream.leaseRef,
    streamEpoch: stream.streamEpoch,
    streamOwnerTab: tabId,
  });
  await expect(page.getByText("Connected")).toBeVisible({ timeout: 10_000 });
});
