import {
  DESKTOP_MARKER,
  EVIL_ORIGIN,
  PORTAL_ORIGIN,
  expect,
  login,
  openSession,
  resetState,
  seedReadyWorkspace,
  setConnectionStatus,
  setDesktopBehaviour,
  test,
  workspaceOrigin,
} from "./harness.ts";

// session-isolation: the session surface must not leak out of the portal's
// embedding rules — foreign frames refused by frame-ancestors (D14), no
// top-navigation or popups out of the sandboxed frame (D13), a session
// cookie replayed on a different workspace host is dead (D11), and the
// portal origin only ever carries __Host- cookies (D17).
const WS_A = "ws_e2e0001aabbcc";
const WS_B = "ws_e2e0002aabbcc";

test("foreign frame", async ({ page, context, request, harnessMode }) => {
  await resetState(request, harnessMode);
  await seedReadyWorkspace(request, WS_A);
  await login(page);
  const origin = workspaceOrigin(harnessMode, WS_A);
  await openSession(page, harnessMode, WS_A);

  // A page on a foreign origin frames the live workspace host. Every
  // session response carries frame-ancestors <portal origin>, so the
  // browser must refuse to render it.
  const evil = await context.newPage();
  const refusals: string[] = [];
  evil.on("console", (m) => {
    if (/frame-ancestors|refused to display|content security policy/i.test(m.text()))
      refusals.push(m.text());
  });
  await evil.goto(`${EVIL_ORIGIN}/frame.html?target=${encodeURIComponent(`${origin}/`)}`);

  await expect
    .poll(() => refusals.join("\n"), { timeout: 15_000 })
    .toMatch(/frame-ancestors/i);

  // And no usable session document ever rendered inside the foreign frame
  // — a refused frame shows the browser's error page, which has no h1 at
  // all, so assert on the body text rather than a missing element.
  const framed = evil.frames().filter((f) => f !== evil.mainFrame());
  for (const f of framed) {
    const body = await f.locator("body").innerText().catch(() => "");
    expect(body).not.toContain(DESKTOP_MARKER);
  }
});

test("top navigation", async ({ page, request, harnessMode }) => {
  await resetState(request, harnessMode);
  await setDesktopBehaviour(request, { topNav: true });
  await seedReadyWorkspace(request, WS_A);
  await login(page);

  const frame = await openSession(page, harnessMode, WS_A);
  // The fake desktop page tried window.top.location=… — its own DOM proves
  // the attempt ran; the sandbox (no allow-top-navigation) refused it.
  await expect(frame.locator("body")).toHaveAttribute("data-topnav", "attempted");
  await page.waitForTimeout(2_000);
  expect(page.url()).toContain(`${PORTAL_ORIGIN}/workspaces/`);
});

test("popup", async ({ page, context, request, harnessMode }) => {
  await resetState(request, harnessMode);
  await setDesktopBehaviour(request, { popup: true });
  await seedReadyWorkspace(request, WS_A);
  await login(page);

  // window.open from a frame without allow-popups must not create a page.
  const popups: unknown[] = [];
  context.on("page", (p) => popups.push(p));
  const frame = await openSession(page, harnessMode, WS_A);
  await expect(frame.locator("body")).toHaveAttribute("data-popup", "attempted");
  await page.waitForTimeout(2_000);
  expect(popups).toHaveLength(0);
});

test("cookie on other host", async ({ page, context, request, harnessMode }) => {
  await resetState(request, harnessMode);
  await seedReadyWorkspace(request, WS_A);
  await seedReadyWorkspace(request, WS_B);
  await login(page);
  await openSession(page, harnessMode, WS_A);
  await setConnectionStatus(request, WS_A, { state: "connected", leaseActive: true });

  const originA = workspaceOrigin(harnessMode, WS_A);
  const cookies = await context.cookies(originA);
  const sessionCookie = cookies.find((c) => c.name === "__Host-tcdi_session");
  expect(sessionCookie, "session cookie on A's host").toBeTruthy();

  // D11: replaying A's cookie on B's workspace host must not proxy — the
  // lease's workspaceUID does not match the host's label, so the session is
  // treated as absent (401).
  const res = await request.get(`${workspaceOrigin(harnessMode, WS_B)}/`, {
    headers: { cookie: `__Host-tcdi_session=${sessionCookie!.value}` },
  });
  expect(res.status()).toBe(401);
});

test("portal cookies", async ({ page, context, request, harnessMode }) => {
  await resetState(request, harnessMode);
  await login(page);

  // D17: every cookie on the portal origin carries the __Host- prefix —
  // tcdi_csrf and tcdi_session_origin are gone in v0.2.
  const cookies = await context.cookies(PORTAL_ORIGIN);
  expect(cookies.length).toBeGreaterThan(0);
  for (const c of cookies) {
    expect(c.name, `cookie ${c.name} must be __Host- prefixed`).toMatch(/^__Host-/);
  }
});
