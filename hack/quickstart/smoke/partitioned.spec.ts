import * as https from "node:https";
import { execFileSync } from "node:child_process";
import { expect, test, type Page, type Response } from "@playwright/test";

// Partitioned-cookie smoke (B6-PART). Runs against a quickstart installed
// with hack/quickstart/values-partitioned.yaml — backend.sessionCookieMode:
// partitioned, so the gateway mints the session-host cookie as
// `SameSite=None; Secure; Partitioned` (CHIPS) instead of `SameSite=Lax`.
//
// Browser assumption: a Chromium build that honours the Partitioned
// attribute — Chrome/Edge >= 118 (CHIPS shipped in 118); the pinned
// @playwright/test 1.63 bundles Chromium 153 (playwright-core
// browsers.json), headless shell included (same network stack and cookie
// store as the headed binary). Firefox/WebKit are not usable here.
//
// The quickstart's portal and session hosts sit on one registrable domain
// (same-site), so this variant proves the partitioned mode end to end —
// attributes minted on real Set-Cookie responses, the browser storing the
// cookie partitioned, in-frame reconnect after a backend rollout (session
// rehydrate via the cookie digest on a replica that never saw it), lease
// revocation making the old cookie worthless on the session listener, and
// logout destroying the portal session. It does not reproduce a cross-site
// deployment; that is the mode's production use, not what kind can fake
// with one base domain.

const USER = process.env.TCDI_QS_USER ?? "demo";
const PASSWORD = process.env.TCDI_QS_PASSWORD ?? "tcdi-demo-dev-only";
const SESSION_DOMAIN =
  process.env.TCDI_QS_SESSION_DOMAIN ??
  `session.${process.env.TCDI_QS_DOMAIN ?? "tcdi.localtest.me"}`;
const SYSTEM_NAMESPACE = process.env.TCDI_QS_SYSTEM_NAMESPACE ?? "tinycdi-system";
const SESSION_COOKIE = "__Host-tcdi_session";

async function login(page: Page, user = USER): Promise<Response> {
  // The OIDC callback response carries the portal session cookie.
  const callback = page.waitForResponse(
    (r) => /\/v1\/auth\/callback/.test(r.url()) && r.request().method() === "GET",
  );
  await page.goto("/");
  await expect(page.locator("#username")).toBeVisible();
  await expect(page).toHaveURL(/^https:\/\/keycloak\./);
  await page.locator("#username").fill(user);
  await page.locator("#password").fill(PASSWORD);
  await page.locator("#kc-login").click();
  await expect(page.getByRole("heading", { name: "Workspaces", exact: true })).toBeVisible();
  return callback;
}

async function createBrowserWorkspace(page: Page, name: string): Promise<string> {
  await page.getByRole("link", { name: "New workspace" }).click();
  await page.waitForURL("**/workspaces/new");
  await page.locator('input[name="name"]').fill(name);
  const browserTemplate = page
    .locator('select[name="template"] option', { hasText: /browser/i })
    .first();
  await expect(browserTemplate).toBeAttached();
  await page
    .locator('select[name="template"]')
    .selectOption((await browserTemplate.getAttribute("value")) ?? "");
  await page.getByRole("button", { name: "Create workspace" }).click();
  await page.waitForURL(/\/workspaces\/ws_/);
  return decodeURIComponent(new URL(page.url()).pathname.split("/").pop() ?? "");
}

async function waitForConnect(page: Page) {
  const connect = page.getByRole("link", { name: "Connect" });
  await expect(connect).toBeVisible({ timeout: 10 * 60_000 });
  return connect;
}

function kubectlIn(ns: string, ...args: string[]): string {
  return execFileSync("kubectl", ["-n", ns, ...args], {
    encoding: "utf8",
    timeout: 300_000,
  }).trim();
}

async function workspacePhase(page: Page, id: string): Promise<string> {
  return page.evaluate(async (wid) => {
    const r = await fetch(`/v1/workspaces/${encodeURIComponent(wid)}`);
    return r.ok ? ((await r.json()) as { phase?: string }).phase ?? "?" : `http-${r.status}`;
  }, id);
}

async function waitPhase(page: Page, id: string, phase: string, timeoutMs = 5 * 60_000) {
  await expect
    .poll(() => workspacePhase(page, id), {
      timeout: timeoutMs,
      intervals: [3_000],
      message: `${id} -> ${phase}`,
    })
    .toBe(phase);
}

async function runningUsage(page: Page): Promise<number> {
  return page.evaluate(async () => {
    const r = await fetch("/v1/quota");
    if (!r.ok) return -1;
    const body = (await r.json()) as { usage?: { runningWorkspaces?: number } };
    return body.usage?.runningWorkspaces ?? -1;
  });
}

// ---- Set-Cookie assertions ---------------------------------------------------

interface ParsedCookie {
  name: string;
  value: string;
  attrs: Record<string, string | true>;
}

function parseSetCookie(header: string): ParsedCookie {
  const [pair, ...attrs] = header.split(";").map((s) => s.trim());
  const [name, ...rest] = pair.split("=");
  const out: Record<string, string | true> = {};
  for (const a of attrs) {
    const [k, ...v] = a.split("=");
    out[k.trim().toLowerCase()] = v.length ? v.join("=").trim() : true;
  }
  return { name, value: rest.join("="), attrs: out };
}

async function mintedSessionCookie(resp: Response): Promise<ParsedCookie> {
  const headers = await resp.headerValues("set-cookie");
  const raw = headers.find((h) => h.startsWith(`${SESSION_COOKIE}=`));
  expect(raw, `Set-Cookie for ${SESSION_COOKIE} on ${resp.url()}`).toBeTruthy();
  const cookie = parseSetCookie(raw!);
  expect(cookie.name).toBe(SESSION_COOKIE);
  expect(cookie.value, "cookie value").not.toBe("");
  return cookie;
}

function expectHostOnlySecure(cookie: ParsedCookie, sameSite: string, partitioned: boolean) {
  expect(cookie.attrs.secure, "Secure").toBe(true);
  expect(cookie.attrs.httponly, "HttpOnly").toBe(true);
  expect(cookie.attrs.path, "Path=/").toBe("/");
  expect(cookie.attrs.samesite, "SameSite").toBe(sameSite);
  expect(cookie.attrs.domain, "__Host- forbids a Domain attribute").toBeUndefined();
  if (partitioned) {
    expect(cookie.attrs.partitioned, "Partitioned (CHIPS)").toBe(true);
  } else {
    expect(cookie.attrs.partitioned, "no Partitioned").toBeUndefined();
  }
}

// Raw HTTPS GET to a session host with an explicit Cookie header — the
// cookie is host-only and partitioned, so no browser path can replay it
// onto a host of our choosing; the point is the listener's own verdict.
function probeSessionHost(host: string, cookieValue: string): Promise<number> {
  return new Promise((resolve, reject) => {
    const req = https.request(
      {
        host: "127.0.0.1",
        port: 443,
        servername: host,
        path: "/",
        method: "GET",
        headers: { Host: host, Cookie: `${SESSION_COOKIE}=${cookieValue}` },
        // The quickstart CA is self-made; the browser side already ignores
        // cert errors (playwright config) and up.sh verified the chain.
        rejectUnauthorized: false,
        timeout: 15_000,
      },
      (res) => {
        res.resume();
        res.on("end", () => resolve(res.statusCode ?? 0));
      },
    );
    req.on("timeout", () => req.destroy(new Error(`probe to ${host} timed out`)));
    req.on("error", reject);
    req.end();
  });
}

// -----------------------------------------------------------------------------

test("partitioned cookie mode: CHIPS session cookie across the session lifecycle", async ({
  page,
  context,
}) => {
  test.setTimeout(20 * 60_000);

  // Guard: this spec is meaningless against a lax install.
  const backendArgs = kubectlIn(
    SYSTEM_NAMESPACE,
    "get", "deploy", "backend",
    "-o", "jsonpath={.spec.template.spec.containers[*].args}",
  );
  expect(backendArgs, "install must set -session-cookie-mode=partitioned").toContain(
    "-session-cookie-mode=partitioned",
  );

  // Launch tickets the portal mints (POST /v1/workspaces/<id>/connections):
  // reconnects inside a live lease never mint one.
  const tickets: string[] = [];
  page.on("request", (r) => {
    if (r.method() === "POST" && /\/v1\/workspaces\/[^/]+\/connections$/.test(r.url())) {
      tickets.push(r.url());
    }
  });

  // ---- 1. login: the PORTAL cookie keeps its lax shape ----------------------
  const callbackResp = await login(page);
  const portalCookie = await mintedSessionCookie(callbackResp);
  // backend.sessionCookieMode covers the session-host cookie only; the
  // portal session cookie is always SameSite=Lax and must not be relaxed.
  expectHostOnlySecure(portalCookie, "Lax", false);

  // ---- 2. create + Connect: the launch 303 mints a Partitioned cookie -------
  const workspaceId = await createBrowserWorkspace(page, `part-${Date.now().toString(36)}`);
  const connect = await waitForConnect(page);
  const launchRespP = page.waitForResponse(
    (r) => /\/v1\/launch/.test(r.url()) && r.request().method() === "POST",
    { timeout: 120_000 },
  );
  await connect.click();
  await page.waitForURL(`**/workspaces/${encodeURIComponent(workspaceId)}/session`);
  const launchCookie = await mintedSessionCookie(await launchRespP);
  expectHostOnlySecure(launchCookie, "None", true);

  // The embedded session: frame on the workspace's session host, then the
  // streaming client, then Connected.
  const label = workspaceId.replaceAll("_", "-").toLowerCase();
  const sessionHost = `${label}.${SESSION_DOMAIN}`;
  const sessionOrigin = `https://${sessionHost}`;
  await expect
    .poll(
      () =>
        page
          .frames()
          .some((f) => f.url().startsWith(`${sessionOrigin}/`) && !f.url().includes("/v1/launch")),
      { timeout: 120_000, message: `session frame on ${sessionOrigin}` },
    )
    .toBe(true);
  const frame = page
    .frames()
    .find((f) => f.url().startsWith(`${sessionOrigin}/`) && !f.url().includes("/v1/launch"));
  expect(frame, "session frame").toBeDefined();
  await expect(frame!.locator("canvas:visible").first()).toBeVisible({ timeout: 120_000 });
  const status = page.locator(".tc-session__status");
  await expect(status).toHaveText("Connected", { timeout: 120_000 });

  // Chromium stored the cookie partitioned (CHIPS): sameSite None, secure,
  // httpOnly, and a partition key bound to the embedding top-level site.
  const stored = (await context.cookies(sessionOrigin)).find((c) => c.name === SESSION_COOKIE);
  expect(stored, "session cookie stored by the browser").toBeDefined();
  expect(stored!.sameSite).toBe("None");
  expect(stored!.secure).toBe(true);
  expect(stored!.httpOnly).toBe(true);
  expect(stored!.partitionKey, "CHIPS partition key").toMatch(/^https:\/\//);

  // GET /v1/workspaces/<id>/connection — the passive connection read the
  // portal polls (P4): state, leaseRef (sha256 prefix, not the lease id)
  // and streamEpoch.
  const connectionState = () =>
    page.evaluate(async (wid) => {
      const r = await fetch(`/v1/workspaces/${encodeURIComponent(wid)}/connection`);
      return r.ok
        ? ((await r.json()) as {
            state?: string;
            leaseActive?: boolean;
            leaseRef?: string;
            streamEpoch?: number;
          })
        : null;
    }, workspaceId);
  const leaseBefore = await connectionState();
  expect(leaseBefore?.state, "session connected before the rollout").toBe("connected");
  expect(leaseBefore?.leaseRef, "live lease before the rollout").toBeTruthy();

  // ---- 3. backend rollout -> in-frame reconnect via session rehydrate -------
  // The partitioned cookie must reach the session host inside the portal
  // frame on EVERY retry; a replica that never saw it resolves its SHA-256
  // digest to the still-live lease (internal/gateway/rehydrate.go). The
  // badge alone is too weak to prove the reconnect — it can still read
  // Connected while the old pod drains — so wait for streamEpoch to
  // advance on the SAME leaseRef: the new replica's stream claim (P3).
  const ticketsBeforeRollout = tickets.length;
  kubectlIn(SYSTEM_NAMESPACE, "rollout", "restart", "deploy/backend");
  kubectlIn(SYSTEM_NAMESPACE, "rollout", "status", "deploy/backend", "--timeout=300s");
  await expect
    .poll(
      async () => {
        const st = await connectionState();
        return (
          st?.state === "connected" &&
          st.leaseRef === leaseBefore?.leaseRef &&
          (st.streamEpoch ?? 0) > (leaseBefore?.streamEpoch ?? -1)
        );
      },
      {
        timeout: 5 * 60_000,
        intervals: [3_000],
        message: "in-frame reconnect: same lease, newer stream epoch",
      },
    )
    .toBe(true);
  await expect(status).toHaveText("Connected", { timeout: 60_000 });
  expect(tickets.length, "reconnect mints no launch ticket").toBe(ticketsBeforeRollout);

  // ---- 4. stop -> the session listener rejects the cookie -------------------
  await page.goto(`/workspaces/${encodeURIComponent(workspaceId)}`);
  await page.getByRole("button", { name: "Stop", exact: true }).click();
  await waitPhase(page, workspaceId, "Stopped");
  // A stop does not revoke the lease (revocation is a finalizer step on
  // delete); the gateway session dies when the renew sees the runtime
  // binding go stale, and the lease then lapses inside its sliding TTL
  // (~30 s). Wait for /connection to report it gone — the session page
  // must not be visited while a nominal lease row still exists, or its
  // resume path would try the dead cookie and fall into the takeover
  // dialog.
  await expect
    .poll(async () => (await connectionState())?.state === "none", {
      timeout: 2 * 60_000,
      intervals: [3_000],
      message: "lease lapsed after stop",
    })
    .toBe(true);
  // The dead lease's digest resolves to nothing and the listener answers
  // 401 — the cookie minted for this workspace's first session is done.
  expect(
    await probeSessionHost(sessionHost, launchCookie.value),
    "session listener rejects the cookie once its lease is dead",
  ).toBe(401);

  // start -> a fresh session mints a fresh partitioned cookie. The
  // compute slot is freed asynchronously (recovery pass proves the runtime
  // gone, ~30 s cadence) — wait for it before restarting.
  await expect
    .poll(() => runningUsage(page), {
      timeout: 120_000,
      intervals: [3_000],
      message: "stopped workspace frees its running slot",
    })
    .toBe(0);
  await page.goto(`/workspaces/${encodeURIComponent(workspaceId)}`);
  await page.getByRole("button", { name: "Start", exact: true }).click();
  await waitPhase(page, workspaceId, "Ready");
  const relaunchRespP = page.waitForResponse(
    (r) => /\/v1\/launch/.test(r.url()) && r.request().method() === "POST",
    { timeout: 120_000 },
  );
  await page.goto(`/workspaces/${encodeURIComponent(workspaceId)}/session`);
  const relaunchCookie = await mintedSessionCookie(await relaunchRespP);
  expectHostOnlySecure(relaunchCookie, "None", true);
  await expect(status).toHaveText("Connected", { timeout: 5 * 60_000 });

  // ---- 5. logout -> portal session gone; old cookie stays rejected ----------
  await page.locator("button:has(.tc-topbar__user)").click();
  await page.getByRole("menuitem", { name: "Sign out" }).click();
  // RP-initiated logout lands back on the public signed-out page.
  await page.waitForURL("**/signed-out", { timeout: 60_000 });
  expect(
    await page.evaluate(async () => (await fetch("/v1/me")).status),
    "GET /v1/me after logout",
  ).toBe(401);
  // The cookie minted at the first Connect stays dead (its lease lapsed
  // when the workspace stopped); replayed against a sibling workspace host
  // it is also rejected — a session cookie never crosses hosts (D11 host
  // binding).
  expect(await probeSessionHost(sessionHost, launchCookie.value)).toBe(401);
  const sibling = `${label.slice(0, -1)}${label.endsWith("0") ? "1" : "0"}.${SESSION_DOMAIN}`;
  expect(await probeSessionHost(sibling, launchCookie.value)).toBe(401);
});
