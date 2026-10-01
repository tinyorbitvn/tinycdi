import {
  DESKTOP_MARKER,
  expect,
  login,
  openSession,
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
