import { test, expect, type APIRequestContext, type Page } from "@playwright/test";
import { MOCK_API, PORTAL_ORIGIN, SESSION_ORIGIN, SESSION_PORT } from "./harness.ts";

// Regression tests: the built SPA served by the real
// portal binary must complete the whole launch round-trip — the
// cross-site form POST reaches the session origin (CSP form-action) AND
// the session cookie rides the POST→302 redirect into the desktop (the
// cookie must be SameSite=Lax; Strict would not be sent). The portal runs
// its real headers on https://localhost:4174 and the mocked session
// origin lives on https://127.0.0.1:4312 — different sites, the
// production shape.
//
// The origin leg: the mock session origin enforces the
// gateway's ADR-0004 launch gate (Origin must equal the portal origin
// when Sec-Fetch-Site is present), so the launch now also proves the
// portal's Referrer-Policy sends a real Origin — and the second test
// replays the original defect (no-referrer -> Origin: null -> 403
// bad_origin) as a negative control.

const SEED_WS = "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E";

const READY_CONDITIONS = [
  { type: "Admitted", status: "True", reason: "QuotaReserved", lastTransitionTime: "2026-09-30T10:00:00Z" },
  { type: "StorageReady", status: "True", reason: "VolumeBound", lastTransitionTime: "2026-09-30T10:01:00Z" },
  { type: "RuntimeReady", status: "True", reason: "RuntimeUp", lastTransitionTime: "2026-09-30T10:02:00Z" },
  { type: "ConnectionReady", status: "True", reason: "StreamEndpointUp", lastTransitionTime: "2026-09-30T10:02:30Z" },
];

async function login(page: Page) {
  await page.goto("/v1/login?returnTo=/");
  await page.getByRole("button", { name: "Log in with SSO" }).click();
  await page.waitForURL("/");
  await expect(page.getByRole("heading", { name: "Workspaces" })).toBeVisible();
}

async function seedReady(request: APIRequestContext) {
  const res = await request.post(`${MOCK_API}/_control/workspaces/${SEED_WS}`, {
    data: { phase: "Ready", desiredState: "Running", conditions: READY_CONDITIONS },
  });
  expect(res.ok()).toBeTruthy();
}

test.beforeEach(async ({ request }) => {
  const res = await request.post(`${MOCK_API}/_control/reset`);
  expect(res.ok()).toBeTruthy();
});

test("portal CSP form-action allows the launch POST to the session origin", async ({
  page,
  context,
  request,
}) => {
  // CSP violations surface as console errors on the page that owns the
  // blocked form — collect them on every page in the context.
  const cspViolations: string[] = [];
  const watch = (p: Page) =>
    p.on("console", (m) => {
      if (/content security policy|form-action/i.test(m.text())) cspViolations.push(m.text());
    });
  watch(page);
  context.on("page", watch);

  await seedReady(request);

  await login(page);
  await page.goto(`/workspaces/${SEED_WS}`);
  await expect(page.getByRole("button", { name: "Connect" })).toBeEnabled({ timeout: 15_000 });

  const [popup] = await Promise.all([
    context.waitForEvent("page"),
    page.getByRole("button", { name: "Connect" }).click(),
  ]);
  await popup.waitForLoadState("load");

  // The launch POST reached the session origin and redeemed the ticket:
  // the browser followed the mock's 302 into the desktop page, and the
  // mock recorded the ticket in the POST body.
  expect(popup.url()).toContain(`:${SESSION_PORT}/desktop/`);
  const launches = await (await request.get(`${MOCK_API}/_control/launchRequests`)).json();
  expect(launches.requests).toHaveLength(1);
  expect(new URLSearchParams(launches.requests[0].rawBody).get("ticket")).toBeTruthy();

  // The launch POST must carry Origin=<portal origin> — the mock
  // enforces the ADR-0004 gate like the real gateway, so an Origin that
  // is "null"/absent (what Referrer-Policy: no-referrer produces) would
  // have failed the launch above; assert the header explicitly too.
  expect(launches.requests[0].headers.origin).toBe(PORTAL_ORIGIN);
  expect(launches.requests[0].headers["sec-fetch-site"]).toBe("cross-site");

  // The session cookie rode the cross-site POST→302 redirect: the mock
  // desktop route is cookie-gated like the real gateway, so "desktop
  // session" — not "unauthorized" — proves the cookie was sent.
  await expect(popup.locator("h1")).toContainText("desktop session");

  expect(cspViolations).toEqual([]);
});

test("a no-referrer portal sends Origin: null and the launch is rejected", async ({
  page,
  context,
  request,
}) => {
  // Negative control for the defect: replay the old
  // Referrer-Policy: no-referrer by rewriting ONLY the workspace
  // document's response header (the document that owns the launch form).
  // Everything else stays real — real portal binary, real SPA, real
  // browser fetch-metadata. The browser then derives Origin: null on the
  // cross-site launch POST, and the ADR-0004 gate must reject it.
  await page.route(`${PORTAL_ORIGIN}/workspaces/${SEED_WS}`, async (route) => {
    const resp = await route.fetch();
    await route.fulfill({
      response: resp,
      headers: { ...resp.headers(), "referrer-policy": "no-referrer" },
    });
  });

  await seedReady(request);

  await login(page);
  await page.goto(`/workspaces/${SEED_WS}`);
  await expect(page.getByRole("button", { name: "Connect" })).toBeEnabled({ timeout: 15_000 });

  const [popup, launchResp] = await Promise.all([
    context.waitForEvent("page"),
    context.waitForEvent(
      "response",
      (r) => r.url().endsWith("/v1/launch") && r.request().method() === "POST",
    ),
    page.getByRole("button", { name: "Connect" }).click(),
  ]);

  // Rejected exactly like the real gateway: 403 {"error":"bad_origin"},
  // no redirect into the desktop.
  expect(launchResp.status()).toBe(403);
  expect(await launchResp.json()).toEqual({ error: "bad_origin" });
  await popup.waitForLoadState("load");
  expect(popup.url()).toContain(`:${SESSION_PORT}/v1/launch`);

  // The browser sent what no-referrer produces: an unattributable Origin
  // on a fetch-metadata-labelled cross-site POST.
  const launches = await (await request.get(`${MOCK_API}/_control/launchRequests`)).json();
  expect(launches.requests).toHaveLength(1);
  const recorded = launches.requests[0];
  expect(recorded.headers["sec-fetch-site"]).toBe("cross-site");
  expect(recorded.headers.origin === undefined || recorded.headers.origin === "null").toBeTruthy();

  // The gate ran before redemption: the same ticket still redeems with a
  // proper Origin (the real gateway's "rejected launch never consumes the
  // ticket" rule).
  const ticket = new URLSearchParams(recorded.rawBody).get("ticket");
  expect(ticket).toBeTruthy();
  const retry = await request.post(`${SESSION_ORIGIN}/v1/launch`, {
    headers: { Origin: PORTAL_ORIGIN, "Sec-Fetch-Site": "cross-site" },
    form: { ticket: ticket! },
    maxRedirects: 0,
  });
  expect(retry.status()).toBe(302);
});

test("session origin launch gate rejects unattributable browser POSTs (ADR 0004)", async ({
  request,
}) => {
  // Mock-level contract check of the gate itself, mirroring the gateway's
  // TestLaunch_* origin cases (internal/gateway/launch_origin_test.go):
  // fetch metadata present requires an allowlisted Origin.
  for (const headers of [
    { "Sec-Fetch-Site": "cross-site" }, // no Origin
    { "Sec-Fetch-Site": "cross-site", Origin: "null" },
    { "Sec-Fetch-Site": "same-site", Origin: "https://evil.example" },
  ]) {
    const res = await request.post(`${SESSION_ORIGIN}/v1/launch`, {
      headers,
      form: { ticket: "bogus" },
      maxRedirects: 0,
    });
    expect(res.status()).toBe(403);
    expect(await res.json()).toEqual({ error: "bad_origin" });
  }

  // An unrecognized fetch-site value is bad_fetch_site.
  const badSite = await request.post(`${SESSION_ORIGIN}/v1/launch`, {
    headers: { "Sec-Fetch-Site": "none", Origin: PORTAL_ORIGIN },
    form: { ticket: "bogus" },
    maxRedirects: 0,
  });
  expect(badSite.status()).toBe(403);
  expect(await badSite.json()).toEqual({ error: "bad_fetch_site" });

  // An allowlisted Origin passes the gate and reaches ticket validation
  // (which rejects this bogus ticket with the API-shape error, not
  // bad_origin) — the check order proves the gate ran first.
  const gated = await request.post(`${SESSION_ORIGIN}/v1/launch`, {
    headers: { "Sec-Fetch-Site": "cross-site", Origin: PORTAL_ORIGIN },
    form: { ticket: "bogus" },
    maxRedirects: 0,
  });
  expect(gated.status()).toBe(403);
  expect((await gated.json()).code).toBe("FORBIDDEN");

  // A non-browser client (neither header) is allowed through the gate.
  const nonBrowser = await request.post(`${SESSION_ORIGIN}/v1/launch`, {
    form: { ticket: "bogus" },
    maxRedirects: 0,
  });
  expect(nonBrowser.status()).toBe(403);
  expect((await nonBrowser.json()).code).toBe("FORBIDDEN");
});
