import type { APIRequestContext, Page } from "@playwright/test";
import {
  expect,
  login,
  MOCK_API,
  resetState,
  seedReadyWorkspace,
  test,
  waitForDesktopFrame,
  watchConsole,
  workspaceOrigin,
  type HarnessMode,
} from "./harness.ts";

// Portal CSP gate (D31): every v0.2 route, served by the REAL frontend
// binary (web/dist through build/frontend with the production security
// headers), must produce zero `securitypolicyviolation` events. The
// listener is installed by init script so it catches violations from the
// very first navigation; console-reported CSP messages are collected too.
// Runs under both harness projects (lax and partitioned session sites) —
// the CSP frame-src/form-action wildcard names the mode's domain.
//
// The session route is the load-bearing case: the launch form POSTs to
// <label>.<sessionDomain> (form-action) and the reply loads in the iframe
// (frame-src). The test requires the fake desktop to actually render, so a
// silently blocked launch cannot masquerade as "no violations".

const WS = "ws_csp0001aabbcc";

// The v0.2 shell routes (same set a11y.spec.ts sweeps).
const ROUTES = [
  "/workspaces",
  "/workspaces/new",
  `/workspaces/${WS}`,
  `/workspaces/${WS}/session`,
  "/admin",
  "/admin/quota",
  "/admin/templates",
  "/admin/workspaces",
  "/data",
] as const;

// Violations the page itself reports (bubbling securitypolicyviolation
// events), installed before any document script runs.
async function armViolationCollector(page: Page) {
  await page.addInitScript(() => {
    const bag: string[] = [];
    (window as unknown as { __cspViolations: string[] }).__cspViolations = bag;
    document.addEventListener("securitypolicyviolation", (e) => {
      const ev = e as SecurityPolicyViolationEvent;
      bag.push(
        `${ev.effectiveDirective} blocked ${ev.blockedURI} on ${ev.sourceFile ?? "?"}:${ev.lineNumber ?? 0}`,
      );
    });
  });
}

async function pageViolations(page: Page): Promise<string[]> {
  return page.evaluate(
    () =>
      (window as unknown as { __cspViolations?: string[] }).__cspViolations ??
      [],
  );
}

// Seed everything the sweep needs: the caller's Ready workspace plus the
// tenant-admin role and other tenants' records so the admin and data
// routes render their real content.
async function seed(request: APIRequestContext) {
  const admin = await request.post(`${MOCK_API}/_control/admin/me`, {
    data: { roles: ["user", "tenant-admin"] },
  });
  expect(admin.ok(), "admin role grant").toBeTruthy();
  const peers = await request.post(`${MOCK_API}/_control/admin/seed`);
  expect(peers.ok(), "admin seed").toBeTruthy();
  await seedReadyWorkspace(request, WS);
}

async function cspSweep(fixtures: {
  page: Page;
  request: APIRequestContext;
  harnessMode: HarnessMode;
}) {
  const { page, request, harnessMode } = fixtures;
  const consoleViolations = watchConsole(page);
  await armViolationCollector(page);
  await resetState(request, harnessMode);
  await seed(request);
  await login(page);
  await expect(page.getByRole("heading", { name: "Workspaces" })).toBeVisible();

  for (const route of ROUTES) {
    await page.evaluate(
      () =>
        ((window as unknown as { __cspViolations: string[] }).__cspViolations =
          []),
    );
    consoleViolations.length = 0;
    await page.goto(route);
    if (route.endsWith("/session")) {
      // The session view auto-launches: the ticket POST and the frame
      // navigation to the workspace's own host are the two directives
      // (form-action, frame-src) a broken portal CSP would trip.
      await waitForDesktopFrame(page, workspaceOrigin(harnessMode, WS));
    } else {
      await page.waitForLoadState("networkidle");
      await expect(page.locator("main").first()).toBeVisible();
    }
    expect(
      await pageViolations(page),
      `securitypolicyviolation events on ${route} [${harnessMode}]`,
    ).toEqual([]);
    expect(
      consoleViolations,
      `console CSP reports on ${route} [${harnessMode}]`,
    ).toEqual([]);
  }
}

test("every route reports zero securitypolicyviolation events", async ({
  page,
  request,
  harnessMode,
}) => {
  await cspSweep({ page, request, harnessMode });
});
