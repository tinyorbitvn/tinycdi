import type { Request } from "@playwright/test";
import { expect, resetState, test } from "./harness.ts";

// FX-R13 / FX-R13b: a first, unauthenticated visit to the portal — served by
// the REAL frontend binary with no branding directory — must leave the console
// completely empty and fail no request at all. Chrome itself logs any failed
// fetch (e.g. a 401 from GET /v1/me) as a console error that page script
// cannot suppress, so the SPA asks the passive GET /v1/session instead; it
// answers 200 {"authenticated": false} and the SPA navigates to the login flow
// without ever calling /v1/me.
//
// Before FX-R13: /branding/branding.json and /favicon.ico answered 404 and
// the first /v1 call was GET /v1/workspaces. Before FX-R13b: the probe was
// GET /v1/me and its 401 line had to be tolerated.

test("an unauthenticated load logs nothing to the console and fails no request", async ({
  page,
  request,
  harnessMode,
}) => {
  await resetState(request, harnessMode);

  const consoleLines: string[] = [];
  const pageErrors: string[] = [];
  const failed: string[] = [];
  const apiCalls: string[] = [];
  page.on("console", (m) =>
    consoleLines.push(`${m.type()}: ${m.text()} (${m.location().url})`),
  );
  page.on("pageerror", (e) => pageErrors.push(String(e)));
  page.on("requestfailed", (r: Request) =>
    failed.push(`${r.url()} ${r.failure()?.errorText ?? ""}`),
  );
  page.on("response", (r) => {
    const u = new URL(r.url());
    if (u.pathname.startsWith("/v1/")) apiCalls.push(`${r.request().method()} ${u.pathname}`);
    if (r.status() >= 400) failed.push(`${r.status()} ${r.request().method()} ${u.pathname}`);
  });

  await page.goto("/");
  // Unauthenticated: the SPA hands over to the login flow.
  await page.waitForURL(/\/v1\/login/);
  await page.waitForLoadState("networkidle");

  expect(apiCalls[0], "the passive session probe is the first /v1 call").toBe("GET /v1/session");
  expect(apiCalls).not.toContain("GET /v1/me");
  expect(apiCalls).not.toContain("GET /v1/workspaces");
  expect(failed, "failed requests").toEqual([]);
  expect(consoleLines, "console lines of any level").toEqual([]);
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
