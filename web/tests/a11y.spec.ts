import { test, expect, type Page, type APIRequestContext } from "@playwright/test";
import { AxeBuilder } from "@axe-core/playwright";

// Accessibility gate (D34): axe against every portal route in both themes —
// zero `serious` or `critical` violations — plus a keyboard sweep proving
// every interactive element on /workspaces is Tab-reachable with a visible
// focus ring. Runs against the contract mock (tests/mock-api) on :4310.

const MOCK = process.env.PW_MOCK_API ?? "http://127.0.0.1:4310";
const SEED_WS = "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E";

const READY_CONDITIONS = [
  { type: "Admitted", status: "True", reason: "QuotaReserved", params: { cpuMillicores: "4000", memoryMiB: "8192", storageGiB: "20" }, lastTransitionTime: "2026-09-30T10:00:00Z" },
  { type: "StorageReady", status: "True", reason: "VolumeBound", params: { sizeGiB: "20" }, lastTransitionTime: "2026-09-30T10:01:00Z" },
  { type: "RuntimeReady", status: "True", reason: "RuntimeUp", lastTransitionTime: "2026-09-30T10:02:00Z" },
  { type: "ConnectionReady", status: "True", reason: "StreamEndpointUp", lastTransitionTime: "2026-09-30T10:02:30Z" },
];

// Every route the shell renders (D34): workspaces, catalog, session view,
// admin, retained data. Each is also a smoke test of the page on the /v1
// API: it must render without an error callout (a contract drift — paths,
// paging, error envelope — surfaces as a danger alert).
const ROUTES = [
  "/workspaces",
  "/workspaces/new",
  "/templates",
  `/workspaces/${SEED_WS}`,
  `/workspaces/${SEED_WS}/session`,
  "/admin",
  "/admin/quota",
  "/admin/session-policy",
  "/admin/templates",
  "/admin/workspaces",
  "/admin/data",
  "/admin/users",
  "/admin/audit",
  "/admin/users/aHR0cHM6Ly9pZHAuaW52YWxpZHx1c2VyLTAxSjRaREdSQw",
  "/data",
  "/data/rd_01J4Z9W2PFK8G4TQ3M7H1R5N0A",
] as const;

const THEMES = ["light", "dark"] as const;

async function login(page: Page, heading = "Workspaces") {
  await page.goto("/v1/login?returnTo=/");
  await page.getByRole("button", { name: "Log in with SSO" }).click();
  await page.waitForURL("/");
  await expect(
    page.getByRole("heading", { name: heading, exact: true }),
  ).toBeVisible();
}

// Runs the sweep as a tenant admin so the admin routes render their real
// content instead of the redirect.
async function seed(request: APIRequestContext) {
  const res = await request.post(`${MOCK}/_control/reset`);
  expect(res.ok()).toBeTruthy();
  const admin = await request.post(`${MOCK}/_control/admin/me`, {
    data: { roles: ["user", "tenant-admin"] },
  });
  expect(admin.ok()).toBeTruthy();
  const peers = await request.post(`${MOCK}/_control/admin/seed`);
  expect(peers.ok()).toBeTruthy();
  const ws = await request.post(`${MOCK}/_control/workspaces/${SEED_WS}`, {
    data: { phase: "Ready", desiredState: "Running", conditions: READY_CONDITIONS },
  });
  expect(ws.ok()).toBeTruthy();
}

async function settle(page: Page) {
  await page.waitForLoadState("networkidle");
  await expect(page.locator("main")).toBeVisible();
}

for (const theme of THEMES) {
  test.describe(`theme: ${theme}`, () => {
    test.beforeEach(async ({ page, request, context }) => {
      // Force the theme through the OS preference and the stored override,
      // matching what theme-init.js resolves before first paint.
      await page.emulateMedia({ colorScheme: theme });
      await context.addInitScript(
        (t) => window.localStorage.setItem("tcdi.theme", t),
        theme,
      );
      await seed(request);
      await login(page);
    });

    for (const route of ROUTES) {
      test(`axe: ${route} has no serious/critical violations`, async ({ page }) => {
        await page.goto(route);
        await settle(page);
        await expect(page.locator(".tc-alert--danger"), `${route} renders without an API error`).toHaveCount(0);
        const results = await new AxeBuilder({ page }).analyze();
        const bad = results.violations.filter(
          (v) => v.impact === "serious" || v.impact === "critical",
        );
        expect(
          bad,
          `${route} [${theme}] violations: ${JSON.stringify(
            bad.map((v) => ({ id: v.id, impact: v.impact, nodes: v.nodes.length })),
          )}`,
        ).toEqual([]);
      });
    }
  });
}

// E11: the same axe gate runs for the Vietnamese UI on the two busiest
// routes. navigator.language=vi-VN is the detection path — no stored
// preference is set, so the portal picks it up on its own.
const VI_ROUTES = [
  "/workspaces",
  "/admin",
  "/admin/session-policy",
  "/admin/data",
  "/admin/users",
] as const;

test.describe("locale: vi", () => {
  test.use({ locale: "vi-VN" });

  for (const theme of THEMES) {
    test.describe(`theme: ${theme}`, () => {
      test.beforeEach(async ({ page, request, context }) => {
        await page.emulateMedia({ colorScheme: theme });
        await context.addInitScript(
          (t) => window.localStorage.setItem("tcdi.theme", t),
          theme,
        );
        await seed(request);
        await login(page, "Workspace");
      });

      for (const route of VI_ROUTES) {
        test(`axe [vi]: ${route} has no serious/critical violations`, async ({ page }) => {
          await page.goto(route);
          await settle(page);
          await expect(page.locator("html")).toHaveAttribute("lang", "vi");
          const results = await new AxeBuilder({ page }).analyze();
          const bad = results.violations.filter(
            (v) => v.impact === "serious" || v.impact === "critical",
          );
          expect(
            bad,
            `${route} [vi, ${theme}] violations: ${JSON.stringify(
              bad.map((v) => ({ id: v.id, impact: v.impact, nodes: v.nodes.length })),
            )}`,
          ).toEqual([]);
        });
      }
    });
  }
});

test.describe("keyboard", () => {
  test("skip link: clipped until the first Tab, then visible without a shadow", async ({ page, request }) => {
    await seed(request);
    await login(page);
    await page.goto("/workspaces");
    await settle(page);
    const skip = page.locator(".tc-skip-link");
    expect(await skip.evaluate((el) => getComputedStyle(el).clipPath)).not.toBe("none");
    await page.keyboard.press("Tab");
    await expect(skip).toBeFocused();
    const style = await skip.evaluate((el) => {
      const s = getComputedStyle(el);
      return { clip: s.clipPath, shadow: s.boxShadow };
    });
    expect(style).toEqual({ clip: "none", shadow: "none" });
  });

  test("every interactive element on /workspaces is Tab-reachable with a visible focus ring", async ({
    page,
    request,
  }) => {
    await seed(request);
    await login(page);
    await page.goto("/workspaces");
    await settle(page);

    const interactive = page
      .locator(
        [
          "a[href]",
          "button",
          "input",
          "select",
          "textarea",
          "summary",
          '[role="button"]',
          '[role="link"]',
          '[role="tab"]',
          '[role="menuitem"]',
          '[role="switch"]',
          '[role="checkbox"]',
          '[contenteditable="true"]',
          "[tabindex]",
        ].join(", "),
      )
      .filter({ visible: true });

    const count = await interactive.count();
    expect(count).toBeGreaterThan(0);

    // Tag each candidate so the Tab sweep can recognise it when focused.
    const probed: string[] = [];
    for (let i = 0; i < count; i++) {
      const el = interactive.nth(i);
      const usable = await el.evaluate((node, idx) => {
        const e = node as HTMLElement;
        if (e.getAttribute("tabindex") === "-1") return false;
        if ("disabled" in e && (e as HTMLButtonElement).disabled) return false;
        if (e.getAttribute("aria-disabled") === "true") return false;
        e.setAttribute("data-a11y-probe", String(idx));
        return true;
      }, i);
      if (usable) probed.push(String(i));
    }

    const focused = new Map<string, { ring: boolean }>();
    const maxTabs = probed.length + 12;
    for (let press = 0; press < maxTabs; press++) {
      await page.keyboard.press("Tab");
      const state = await page.evaluate(() => {
        const el = document.activeElement as HTMLElement | null;
        if (!el || el === document.body || el === document.documentElement) {
          return { probe: null as string | null, ring: false };
        }
        const cs = getComputedStyle(el);
        const outline =
          cs.outlineStyle !== "none" &&
          cs.outlineStyle !== "hidden" &&
          parseFloat(cs.outlineWidth) > 0;
        const ring = outline || (cs.boxShadow !== "none" && cs.boxShadow !== "");
        return { probe: el.getAttribute("data-a11y-probe"), ring };
      });
      if (state.probe !== null && !focused.has(state.probe)) {
        focused.set(state.probe, { ring: state.ring });
      }
      // Stop early once everything has been seen.
      if (focused.size >= probed.length) break;
    }

    const unfocused = probed.filter((id) => !focused.has(id));
    expect(unfocused, "interactive elements never reached by Tab").toEqual([]);

    const noRing = [...focused.entries()].filter(([, v]) => !v.ring).map(([id]) => id);
    expect(noRing, "focused elements without a visible focus ring").toEqual([]);
  });
});

test.describe("locale: vi at 390px", () => {
  test.use({ locale: "vi-VN", viewport: { width: 390, height: 844 } });

  test("admin overview: Stat cards, tabs and buttons fit without horizontal scroll", async ({ page, request }) => {
    await seed(request);
    await login(page, "Workspace");
    await page.goto("/admin");
    await settle(page);
    await expect(page.locator("html")).toHaveAttribute("lang", "vi");
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    expect(overflow).toBeLessThanOrEqual(0);
    const stats = page.locator(".tc-stat");
    expect(await stats.count()).toBeGreaterThan(0);
    for (const stat of await stats.all()) {
      const box = await stat.boundingBox();
      expect(box!.x + box!.width).toBeLessThanOrEqual(390);
    }
    for (const button of await page.locator(".tc-page__actions .tc-button").all()) {
      const box = await button.boundingBox();
      expect(box!.x + box!.width).toBeLessThanOrEqual(390);
    }
  });
});
