// Playwright webServer for the portal-CSP regression spec (v0.2): serves
// the BUILT SPA (web/dist) through the REAL frontend binary
// (build/frontend) over HTTPS — real security headers — behind a thin
// edge shim on the portal origin that mirrors the production Ingress: /v1
// goes to the API (the contract mock on :4320, http), everything else to
// the frontend. The contract mock also listens on :4312 over TLS as the
// session listener, publishing `tcdi.localhost:4312` as the session domain
// — browsers resolve ws-<label>.tcdi.localhost to loopback, so the
// per-workspace launch URL reaches it like a real wildcard DNS would.
//
// Requires a Go toolchain and openssl on PATH. This process must stay alive
// for the suite; Playwright kills it after the run, and we reap the
// children it spawned.

import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import { mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import http from "node:http";
import https from "node:https";
import {
  PORTAL_PORT,
  API_PORT,
  SESSION_PORT,
  FRONTEND_PORT,
  SESSION_DOMAIN,
  SESSION_ORIGIN,
  PORTAL_ORIGIN,
  MOCK_API,
} from "./harness.ts";

const WEB_DIR = path.resolve(import.meta.dirname, "..");
const REPO_ROOT = path.resolve(WEB_DIR, "..");

const children: ChildProcess[] = [];
const servers: (http.Server | https.Server)[] = [];
function shutdown(code: number): never {
  for (const c of children) c.kill("SIGKILL");
  for (const s of servers) s.close();
  process.exit(code);
}
process.on("SIGTERM", () => shutdown(0));
process.on("SIGINT", () => shutdown(0));
process.on("exit", () => {
  for (const c of children) c.kill("SIGKILL");
});

function runSync(cmd: string, args: string[], cwd: string, what: string, quiet = false) {
  const r = spawnSync(cmd, args, { cwd, stdio: quiet ? "pipe" : "inherit" });
  if (r.error || r.status !== 0) {
    console.error(`${what} failed: ${cmd} ${args.join(" ")}`, r.error ?? "");
    if (quiet) console.error(String(r.stdout ?? "") + String(r.stderr ?? ""));
    shutdown(1);
  }
}

function spawnLogged(cmd: string, args: string[], opts: object, what: string): ChildProcess {
  const child = spawn(cmd, args, { stdio: ["ignore", "inherit", "inherit"], ...opts });
  children.push(child);
  child.on("exit", (code) => {
    console.error(`${what} exited (${code}) — tearing down`);
    shutdown(code ?? 1);
  });
  return child;
}

async function waitFor(url: string, what: string, timeoutMs = 60_000) {
  const get = url.startsWith("https:") ? https.get : http.get;
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const ok = await new Promise<boolean>((resolve) => {
      const req = get
        .call(null, url, { rejectUnauthorized: false }, (res) => {
          res.resume();
          resolve(res.statusCode === 200);
        })
        .on("error", () => resolve(false));
      req.setTimeout(2_000, () => req.destroy());
    });
    if (ok) return;
    if (Date.now() > deadline) {
      console.error(`timed out waiting for ${what} at ${url}`);
      shutdown(1);
    }
    await new Promise((r) => setTimeout(r, 250));
  }
}

// 1. Fresh SPA bundle — the CSP is sized to this build, so a stale dist
//    would test the wrong artifact.
runSync("npm", ["run", "build"], WEB_DIR, "vite build");

// 2. Real frontend binary (the SPA host; it owns the security headers).
const workDir = mkdtempSync(path.join(tmpdir(), "tcdi-portal-e2e-"));
const frontendBin = path.join(workDir, "frontend");
runSync("go", ["build", "-o", frontendBin, "./build/frontend"], REPO_ROOT, "go build ./build/frontend");

// 3. Self-signed cert — used by the edge shim, the frontend and the mock
//    session listener (Playwright runs with ignoreHTTPSErrors). The SANs
//    cover the session domain wildcard so the cert is plausible, though
//    the suite never verifies it.
const tlsCert = path.join(workDir, "tls.crt");
const tlsKey = path.join(workDir, "tls.key");
const sessionDomainHost = SESSION_DOMAIN.split(":")[0];
runSync(
  "openssl",
  [
    "req", "-x509", "-newkey", "rsa:2048", "-nodes",
    "-keyout", tlsKey, "-out", tlsCert,
    "-subj", "/CN=localhost",
    "-addext",
    `subjectAltName=IP:127.0.0.1,DNS:localhost,DNS:${sessionDomainHost},DNS:*.${sessionDomainHost}`,
    "-days", "1",
  ],
  workDir,
  "openssl self-signed cert",
  true, // keygen progress dots go to stderr — silence unless it fails
);

// 4. Contract mock: API on :4320 (the shim proxies /v1 to it over plain
//    http — server-side hop, the browser never sees it), session listener
//    on :4312 over TLS. sessionDomain is what /v1/me publishes and what
//    launchUrl hosts are built under (ws-<label>.tcdi.localhost:4312).
spawnLogged(
  "node",
  ["--disable-warning=ExperimentalWarning", "tests/mock-api/server.ts"],
  {
    cwd: WEB_DIR,
    env: {
      ...process.env,
      MOCK_PORTAL_PORT: String(API_PORT),
      MOCK_SESSION_PORT: String(SESSION_PORT),
      MOCK_SESSION_TLS_CERT: tlsCert,
      MOCK_SESSION_TLS_KEY: tlsKey,
      MOCK_SESSION_DOMAIN: SESSION_DOMAIN,
      // The mock session origin enforces the gateway's launch origin
      // allowlist (ADR 0004): only this portal origin may redeem.
      MOCK_PORTAL_ORIGINS: PORTAL_ORIGIN,
    },
  },
  "mock api",
);

// 5. Frontend binary with its real headers — the session domain lands in
//    CSP as the https wildcard for frame-src and form-action (D14).
spawnLogged(
  frontendBin,
  [
    `-listen=127.0.0.1:${FRONTEND_PORT}`,
    `-web-root=${path.join(WEB_DIR, "dist")}`,
    `-tls-cert=${tlsCert}`,
    `-tls-key=${tlsKey}`,
    `-session-domain=${SESSION_DOMAIN}`,
  ],
  {},
  "frontend",
);

// 6. Edge shim on the portal origin: exactly what the production Ingress
//    does — /v1/* to the API, everything else to the frontend — so the SPA
//    and the API share one origin and the frontend's headers reach the
//    browser untouched.
const edge = https.createServer({ cert: readFileSync(tlsCert), key: readFileSync(tlsKey) }, (req, res) => {
  const toApi = (req.url ?? "/") === "/v1" || (req.url ?? "").startsWith("/v1/");
  const upstream = toApi
    ? { mod: http, port: API_PORT }
    : { mod: https, port: FRONTEND_PORT };
  const preq = upstream.mod.request(
    {
      host: "127.0.0.1",
      port: upstream.port,
      path: req.url,
      method: req.method,
      headers: { ...req.headers, host: `127.0.0.1:${upstream.port}` },
      rejectUnauthorized: false,
    },
    (pres) => {
      res.writeHead(pres.statusCode ?? 502, pres.headers);
      pres.pipe(res);
    },
  );
  preq.on("error", () => {
    if (!res.headersSent) res.writeHead(502);
    res.end();
  });
  req.pipe(preq);
});
servers.push(edge);
edge.listen(PORTAL_PORT);

await waitFor(`${MOCK_API}/_control/health`, "mock api");
await waitFor(`https://127.0.0.1:${FRONTEND_PORT}/healthz`, "frontend");
await waitFor(`${PORTAL_ORIGIN}/healthz`, "edge shim");
console.log(
  `portal e2e harness up: portal ${PORTAL_ORIGIN} -> api :${API_PORT} + frontend :${FRONTEND_PORT}, session *.${SESSION_DOMAIN} (${SESSION_ORIGIN})`,
);
