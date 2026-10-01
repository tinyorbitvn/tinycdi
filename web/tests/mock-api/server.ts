// Mock servers for Playwright: portal origin on 4310 (public API + login +
// _control) and session origin on 4311 (/v1/launch + /desktop/*). Both share
// one mock-api instance so tickets minted on the portal redeem on session.
//
// MOCK_SESSION_TLS_CERT + MOCK_SESSION_TLS_KEY switch the session listener
// to HTTPS (tests-portal: the portal CSP config only accepts an https
// session origin, so the launch target must be TLS there too).
//
// MOCK_PORTAL_ORIGINS (CSV) is the launch allowlist the mock session
// origin enforces on /v1/launch — the gateway's Config.PortalOrigins
// (ADR 0004). Default is the vite dev origin the playwright.config.ts
// suite launches from; tests-portal/serve.ts overrides it with the real
// portal binary's https origin. An empty list fails closed.

import fs from "node:fs";
import http from "node:http";
import https from "node:https";
import { createMockApi, type MockRequest } from "./handler.ts";

const PORTAL_PORT = Number(process.env.MOCK_PORTAL_PORT ?? 4310);
const SESSION_PORT = Number(process.env.MOCK_SESSION_PORT ?? 4311);
const SESSION_TLS_CERT = process.env.MOCK_SESSION_TLS_CERT;
const SESSION_TLS_KEY = process.env.MOCK_SESSION_TLS_KEY;
if (!SESSION_TLS_CERT !== !SESSION_TLS_KEY) {
  throw new Error("MOCK_SESSION_TLS_CERT and MOCK_SESSION_TLS_KEY must be set together");
}
const sessionOrigin = `${SESSION_TLS_CERT ? "https" : "http"}://127.0.0.1:${SESSION_PORT}`;
const portalOrigins = (process.env.MOCK_PORTAL_ORIGINS ?? "http://127.0.0.1:4173")
  .split(",")
  .map((s) => s.trim())
  .filter((s) => s !== "");

const api = createMockApi({ sessionOrigin, portalOrigins });

function toHeaders(h: http.IncomingHttpHeaders): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(h)) {
    if (typeof v === "string") out[k.toLowerCase()] = v;
    else if (Array.isArray(v)) out[k.toLowerCase()] = v.join("; ");
  }
  return out;
}

function handleRequest(req: http.IncomingMessage, res: http.ServerResponse) {
  const chunks: Buffer[] = [];
  req.on("data", (c: Buffer) => chunks.push(c));
  req.on("end", () => {
    const rawBody = Buffer.concat(chunks).toString("utf8");
    const url = new URL(req.url ?? "/", "http://localhost");
    let body: Record<string, unknown> | undefined;
    try {
      body = rawBody ? (JSON.parse(rawBody) as Record<string, unknown>) : undefined;
    } catch {
      body = undefined;
    }
    const mreq: MockRequest = {
      method: req.method ?? "GET",
      path: url.pathname,
      query: url.searchParams,
      headers: toHeaders(req.headers),
      rawBody,
      ...(body !== undefined ? { body } : {}),
    };
    const resp = api.handle(mreq);
    res.writeHead(resp.status, resp.headers);
    res.end(typeof resp.body === "string" ? resp.body : JSON.stringify(resp.body));
  });
}

const portal = http.createServer(handleRequest);
const session =
  SESSION_TLS_CERT && SESSION_TLS_KEY
    ? https.createServer(
        { cert: fs.readFileSync(SESSION_TLS_CERT), key: fs.readFileSync(SESSION_TLS_KEY) },
        handleRequest,
      )
    : http.createServer(handleRequest);

portal.listen(PORTAL_PORT, "127.0.0.1", () => {
  console.log(`mock portal api on http://127.0.0.1:${PORTAL_PORT}`);
});
session.listen(SESSION_PORT, "127.0.0.1", () => {
  console.log(`mock session origin on ${sessionOrigin}`);
});
