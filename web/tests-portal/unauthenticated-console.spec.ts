import type { ConsoleMessage, Request } from "@playwright/test";
import { expect, resetState, test } from "./harness.ts";

// FX-R13: a first, unauthenticated visit to the portal — served by the REAL
// frontend binary with no branding directory — must leave the console clean
// and must not fail any request except the expected 401 of the session
// probe (GET /v1/me), which the SPA treats as "signed out" and answers by
// navigating to the login flow.
//
// Before the fix: /branding/branding.json and /favicon.ico answered 404 and
// the first /v1 call was GET /v1/workspaces.

// The browser itself logs "Failed to load resource: ... 401" for every 4xx
// fetch, whatever the page does with it — only that line about /v1/me is
// tolerated, never an error the app logs itself.
function isExpectedProbeLog(m: ConsoleMessage): boolean {
  return (
    /status of 401/.test(m.text()) &&
    new URL(m.location().url || "http://x/").pathname === "/v1/me"
  );
}

test("an unauthenticated load logs no console errors and fails only the /v1/me 401", async ({
  page,
  request,
  harnessMode,
}) => {
  await resetState(request, harnessMode);

  const consoleErrors: string[] = [];
  const pageErrors: string[] = [];
  const failed: string[] = [];
  const apiCalls: string[] = [];
  page.on("console", (m) => {
    if ((m.type() === "error" || m.type() === "warning") && !isExpectedProbeLog(m))
      consoleErrors.push(`${m.type()}: ${m.text()} (${m.location().url})`);
  });
  page.on("pageerror", (e) => pageErrors.push(String(e)));
  page.on("requestfailed", (r: Request) =>
    failed.push(`${r.url()} ${r.failure()?.errorText ?? ""}`),
  );
  page.on("response", (r) => {
    const u = new URL(r.url());
    if (u.pathname.startsWith("/v1/")) apiCalls.push(`${r.request().method()} ${u.pathname}`);
    if (r.status() >= 400 && !(u.pathname === "/v1/me" && r.status() === 401))
      failed.push(`${r.status()} ${r.request().method()} ${u.pathname}`);
  });

  await page.goto("/");
  // Unauthenticated: the SPA hands over to the login flow.
  await page.waitForURL(/\/v1\/login/);
  await page.waitForLoadState("networkidle");

  expect(apiCalls[0], "the session probe is the first /v1 call").toBe("GET /v1/me");
  expect(apiCalls).not.toContain("GET /v1/workspaces");
  expect(failed, "failed requests").toEqual([]);
  expect(consoleErrors, "console errors/warnings").toEqual([]);
  expect(pageErrors, "uncaught page errors").toEqual([]);
});

test("the portal answers branding.json and the favicon without errors", async ({
  request,
  harnessMode,
}) => {
  await resetState(request, harnessMode);
  const branding = await request.get("/branding/branding.json");
  expect(branding.status()).toBe(200);
  expect(branding.headers()["content-type"]).toContain("application/json");
  expect(await branding.json()).toEqual({});

  for (const [path, type] of [
    ["/favicon.ico", /image\/(x-icon|vnd\.microsoft\.icon)/],
    ["/favicon.svg", /image\/svg\+xml/],
  ] as const) {
    const res = await request.get(path);
    expect(res.status(), path).toBe(200);
    expect(res.headers()["content-type"], path).toMatch(type);
  }
});
