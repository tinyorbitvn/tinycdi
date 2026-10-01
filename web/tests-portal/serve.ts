// Playwright webServer for the portal-CSP regression spec: serves the
// BUILT SPA (web/dist) through the REAL Go frontend binary (build/frontend)
// over HTTPS — real security headers — backed by the contract mock
// (tests/mock-api) as the API on :4320 (http) and as the session origin on
// :4312 (https; the frontend only accepts an https -session-origin).
//
// The frontend deliberately answers /v1/* with 404 (the edge routes the API
// to the backend by path), so a tiny TLS front on PORTAL_PORT plays the edge
// here: /v1/* goes to the mock, everything else to the frontend.
//
// Requires a Go toolchain and openssl on PATH. This process must stay alive
// for the suite; Playwright kills it after the run, and we reap the
// children it spawned.

import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import fs, { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import http from "node:http";
import https from "node:https";
import { PORTAL_PORT, API_PORT, SESSION_PORT, SESSION_ORIGIN, PORTAL_ORIGIN, MOCK_API } from "./harness.ts";

const WEB_DIR = path.resolve(import.meta.dirname, "..");
const REPO_ROOT = path.resolve(WEB_DIR, "..");

const children: ChildProcess[] = [];
function shutdown(code: number): never {
  for (const c of children) c.kill("SIGKILL");
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

// 2. Real frontend binary.
const workDir = mkdtempSync(path.join(tmpdir(), "tcdi-portal-e2e-"));
const frontendBin = path.join(workDir, "frontend");
runSync("go", ["build", "-o", frontendBin, "./build/frontend"], REPO_ROOT, "go build ./build/frontend");

// 3. Self-signed cert for 127.0.0.1 — used by BOTH the portal and the mock
//    session origin (Playwright runs with ignoreHTTPSErrors).
const tlsCert = path.join(workDir, "tls.crt");
const tlsKey = path.join(workDir, "tls.key");
runSync(
  "openssl",
  [
    "req", "-x509", "-newkey", "rsa:2048", "-nodes",
    "-keyout", tlsKey, "-out", tlsCert,
    "-subj", "/CN=127.0.0.1",
    "-addext", "subjectAltName=IP:127.0.0.1,DNS:localhost",
    "-days", "1",
  ],
  workDir,
  "openssl self-signed cert",
  true, // keygen progress dots go to stderr — silence unless it fails
);

// 4. Contract mock: API on :4320 (the portal proxies /v1 to it over plain
//    http — server-side hop, the browser never sees it), session origin on
//    :4312 over TLS so it is a valid -session-origin.
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
      // The mock session origin enforces the gateway's launch origin
      // allowlist (ADR 0004): only this portal origin may redeem.
      MOCK_PORTAL_ORIGINS: PORTAL_ORIGIN,
    },
  },
  "mock api",
);

// 5. Frontend binary with its real headers — form-action must carry the
//    session origin for launches to succeed. It binds a loopback port and
//    the edge shim below publishes PORTAL_PORT.
const FRONT_PORT = PORTAL_PORT + 1000;
spawnLogged(
  frontendBin,
  [
    `-listen=127.0.0.1:${FRONT_PORT}`,
    `-web-root=${path.join(WEB_DIR, "dist")}`,
    `-tls-cert=${tlsCert}`,
    `-tls-key=${tlsKey}`,
    `-session-origin=${SESSION_ORIGIN}`,
  ],
  {},
  "frontend",
);

// 6. Edge shim on the portal origin: /v1/* goes to the mock API (the
//    backend's stand-in), everything else to the frontend — the same
//    path-based split the production edge applies.
const edge = https.createServer(
  { cert: fs.readFileSync(tlsCert), key: fs.readFileSync(tlsKey) },
  (req, res) => {
    const isApi = req.url === "/v1" || req.url?.startsWith("/v1/");
    const upstream = isApi
      ? { mod: http, port: API_PORT }
      : { mod: https, port: FRONT_PORT };
    const preq = upstream.mod.request(
      {
        host: "127.0.0.1",
        port: upstream.port,
        path: req.url,
        method: req.method,
        headers: req.headers,
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
  },
);
edge.listen(PORTAL_PORT, "127.0.0.1");

await waitFor(`${MOCK_API}/_control/health`, "mock api");
await waitFor(`https://127.0.0.1:${PORTAL_PORT}/healthz`, "portal");
console.log(
  `portal e2e harness up: portal https://127.0.0.1:${PORTAL_PORT} -> api :${API_PORT}, session ${SESSION_ORIGIN}`,
);
