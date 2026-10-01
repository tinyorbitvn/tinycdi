// Playwright webServer for the real-binary portal e2e harness.
//
// Builds ./cmd/backend and ./build/frontend into web/.e2e-bin/ (gitignored),
// then runs the real frontend plus the real backend session listener in
// split mode (-broker-url mTLS, no app/internal listeners) against two
// in-process fakes: a broker speaking the internal mTLS contract and a
// KasmVNC-like upstream that requires the injected Basic credentials and
// serves the fake desktop page.
//
// Two host layouts run at once (one backend + one frontend per mode):
//   lax         session.tcdi.localhost:4312    (same site as the portal)
//   partitioned session.tcdi-other.localhost:4313 (different site, D16)
// The https router on portal.tcdi.localhost:4174 is the "edge": /v1 and
// /_control go to the contract mock API, everything else to the active
// mode's frontend binary. It also rewrites Me.sessionDomain and
// LaunchTicket.launchUrl onto the active session domain and hands each
// minted ticket to the fake broker — that is exactly what the real API +
// broker pair does, the split-mode backend just has no API listener.
//
// Loopback control API (http://127.0.0.1:4176, never on a public name):
//   POST /mode            {mode: "lax"|"partitioned"}  — active site
//   POST /backend/restart {mode} — kill + respawn that backend, reply when up
//   POST /desktop         {topNav?: bool, popup?: bool} — fake upstream page behaviour
//   POST /broker/reset    — drop every ticket/lease the fake broker holds
//
// Requires a Go toolchain, openssl and the Playwright browsers. This
// process must stay alive for the suite; Playwright kills it after the run
// and we reap the children it spawned.

import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import http from "node:http";
import https from "node:https";
import {
  BROKER_PORT,
  CONTROL_PORT,
  EVIL_PORT,
  FRONTEND_PORTS,
  MOCK_API,
  MOCK_API_PORT,
  PORTAL_PORT,
  PORTAL_ORIGIN,
  SESSION_DOMAINS,
  UPSTREAM_PORT,
  workspaceLabel,
  type HarnessMode,
} from "./harness.ts";

const WEB_DIR = path.resolve(import.meta.dirname, "..");
const REPO_ROOT = path.resolve(WEB_DIR, "..");
const BIN_DIR = path.join(WEB_DIR, ".e2e-bin");
const DIST_DIR = path.join(WEB_DIR, "dist");
const CERT_FILE = path.join(BIN_DIR, "tls.crt");
const KEY_FILE = path.join(BIN_DIR, "tls.key");
const TOKEN_FILE = path.join(BIN_DIR, "control-token");

// SANs on the single self-signed certificate. It doubles as CA
// (basicConstraints CA:TRUE) so one file is the server cert, the mTLS
// client cert and the trust bundle for both directions.
const CERT_SANS = [
  "DNS:portal.tcdi.localhost",
  "DNS:evil.tcdi.localhost",
  "DNS:session.tcdi.localhost",
  "DNS:*.session.tcdi.localhost",
  "DNS:session.tcdi-other.localhost",
  "DNS:*.session.tcdi-other.localhost",
  "DNS:upstream.tcdi.localhost",
  "DNS:localhost",
  "IP:127.0.0.1",
].join(",");

const UPSTREAM_USER = "kasm-user";
const UPSTREAM_PASS = "kasm-pass";
const UPSTREAM_AUTH =
  "Basic " + Buffer.from(`${UPSTREAM_USER}:${UPSTREAM_PASS}`).toString("base64");

// ---------------------------------------------------------------------------
// process lifecycle
// ---------------------------------------------------------------------------

const children = new Set<ChildProcess>();
let exiting = false;
function shutdown(code: number): never {
  if (exiting) process.exit(code);
  exiting = true;
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

// spawnChild runs a supervised child. onDown, when set, is called on
// unexpected exit instead of tearing the suite down (the session-reconnect
// spec kills and restarts backends through the control endpoint).
function spawnChild(
  what: string,
  cmd: string,
  args: string[],
  opts: { cwd?: string; env?: NodeJS.ProcessEnv; onDown?: () => void } = {},
): ChildProcess {
  const child = spawn(cmd, args, {
    stdio: ["ignore", "inherit", "inherit"],
    cwd: opts.cwd,
    env: opts.env,
  });
  children.add(child);
  let reported = false;
  const down = (why: string, code: number | null) => {
    children.delete(child);
    if (exiting || reported) return;
    reported = true;
    if (opts.onDown) {
      console.error(`${what} ${why} (${code}) — restartable`);
      opts.onDown();
    } else {
      console.error(`${what} ${why} (${code}) — tearing down`);
      shutdown(code ?? 1);
    }
  };
  child.on("exit", (code) => down("exited", code));
  child.on("error", (err) => down(`spawn error: ${err.message}`, 1));
  return child;
}

function requestRaw(opts: {
  url: string;
  method?: string;
  headers?: Record<string, string>;
  body?: string | Buffer;
  tls?: boolean;
  rejectUnauthorized?: boolean;
  cert?: string;
  key?: string;
  ca?: string;
}): Promise<{ status: number; headers: http.IncomingHttpHeaders; raw: Buffer }> {
  return new Promise((resolve, reject) => {
    const u = new URL(opts.url);
    const mod = u.protocol === "https:" ? https : http;
    const req = mod.request(
      u,
      {
        method: opts.method ?? "GET",
        headers: opts.headers,
        rejectUnauthorized: opts.rejectUnauthorized ?? false,
        cert: opts.cert,
        key: opts.key,
        ca: opts.ca,
      },
      (res) => {
        const chunks: Buffer[] = [];
        res.on("data", (c: Buffer) => chunks.push(c));
        res.on("end", () => resolve({ status: res.statusCode ?? 0, headers: res.headers, raw: Buffer.concat(chunks) }));
      },
    );
    req.on("error", reject);
    req.setTimeout(4_000, () => req.destroy(new Error("timeout")));
    if (opts.body !== undefined) req.write(opts.body);
    req.end();
  });
}

// waitFor polls until fn returns true (any HTTP response counts as "up" for
// process liveness — the session listener answers 401/421 on wrong hosts
// and that still proves the listener).
async function waitFor(what: string, fn: () => Promise<boolean>, timeoutMs = 90_000) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    try {
      if (await fn()) return;
    } catch {
      /* not up yet */
    }
    if (Date.now() > deadline) {
      console.error(`timed out waiting for ${what}`);
      shutdown(1);
    }
    await new Promise((r) => setTimeout(r, 250));
  }
}

const responds = async (url: string) => {
  try {
    await requestRaw({ url });
    return true;
  } catch {
    return false;
  }
};

// ---------------------------------------------------------------------------
// build + certificates
// ---------------------------------------------------------------------------

// TCDI_E2E_NO_BINS=1 runs only the in-process fakes and the mock (no go
// builds, no binary children) — a development/debug knob for exercising the
// harness plumbing before the backend/frontend binaries exist.
const NO_BINS = process.env.TCDI_E2E_NO_BINS === "1";

mkdirSync(BIN_DIR, { recursive: true });
if (!NO_BINS) {
  runSync("npm", ["run", "build"], WEB_DIR, "vite build");
  runSync("go", ["build", "-o", path.join(BIN_DIR, "backend"), "./cmd/backend"], REPO_ROOT, "go build ./cmd/backend");
  runSync("go", ["build", "-o", path.join(BIN_DIR, "frontend"), "./build/frontend"], REPO_ROOT, "go build ./build/frontend");
}
runSync(
  "openssl",
  [
    "req", "-x509", "-newkey", "rsa:2048", "-nodes",
    "-keyout", KEY_FILE, "-out", CERT_FILE,
    "-subj", "/CN=tcdi-e2e",
    "-addext", `subjectAltName=${CERT_SANS}`,
    "-addext", "basicConstraints=critical,CA:TRUE",
    "-addext", "extendedKeyUsage=serverAuth,clientAuth",
    "-days", "1",
  ],
  BIN_DIR,
  "openssl self-signed cert",
  true,
);
writeFileSync(TOKEN_FILE, "e2e-control-token\n", { mode: 0o600 });

const cert = readFileSync(CERT_FILE);
const key = readFileSync(KEY_FILE);
const tlsOpts = { cert, key };

// ---------------------------------------------------------------------------
// fake KasmVNC upstream (what the session listener proxies to)
// ---------------------------------------------------------------------------

const upstreamBehavior = { topNav: false, popup: false };
let upstreamRenders = 0;

function desktopPage(): string {
  const scripts: string[] = [];
  if (upstreamBehavior.topNav) {
    scripts.push(
      `try { window.top.location = "https://example.invalid/"; } catch (e) {}`,
      `document.body.dataset.topnav = "attempted";`,
    );
  }
  if (upstreamBehavior.popup) {
    scripts.push(
      `try { window.open("https://example.invalid/", "_blank"); } catch (e) {}`,
      `document.body.dataset.popup = "attempted";`,
    );
  }
  const script = scripts.length ? `<script>${scripts.join("\n")}</script>` : "";
  // data-render counts upstream documents so the reconnect spec can prove a
  // fresh load happened rather than the stale frame sitting still.
  return `<!doctype html><html><head><title>fake KasmVNC upstream</title></head>` +
    `<body data-render="${++upstreamRenders}"><h1 data-testid="desktop">desktop session</h1>` +
    `${script}</body></html>`;
}

const upstream = https.createServer(tlsOpts, (req, res) => {
  if (req.headers.authorization !== UPSTREAM_AUTH) {
    res.writeHead(401, { "content-type": "text/plain" });
    res.end("upstream requires the injected credentials");
    return;
  }
  res.writeHead(200, { "content-type": "text/html" });
  res.end(desktopPage());
});
upstream.listen(UPSTREAM_PORT, "127.0.0.1");

// ---------------------------------------------------------------------------
// fake broker: the internal mTLS contract the split-mode backend speaks
// ---------------------------------------------------------------------------

interface FakeLease {
  leaseId: string;
  workspaceUID: string;
  tenantID: string;
  principalSubject: string;
  runtimeGeneration: number;
  runtimeUID: string;
  fencingVersion: number;
  gatewayID: string;
  expiresAt: string;
}

const brokerState = {
  seq: 0,
  tickets: new Map<string, { workspaceUID: string; used: boolean }>(),
  leases: new Map<string, FakeLease>(),
  revoked: new Set<string>(),
};

function brokerReset() {
  brokerState.tickets.clear();
  brokerState.leases.clear();
  brokerState.revoked.clear();
}

// The router calls this for every ticket the mock API mints — the real
// broker learns the same binding when the API creates the lease.
function brokerRegisterTicket(ticket: string, workspaceUID: string) {
  brokerState.tickets.set(ticket, { workspaceUID, used: false });
}

function apiErr(res: http.ServerResponse, status: number, code: string, message: string) {
  const body = JSON.stringify({ code, message, retryable: false, requestId: `req_fake_${++brokerState.seq}` });
  res.writeHead(status, { "content-type": "application/json" });
  res.end(body);
}

const LEASES_RE = /^\/internal\/v1\/broker\/leases\/([^/]+)\/(renew|target|revoke|activity)$/;

const broker = https.createServer(
  {
    ...tlsOpts,
    // Real mTLS: the backend must present the client certificate.
    ca: [cert],
    requestCert: true,
    rejectUnauthorized: true,
  },
  (req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => {
      let body: { ticket?: string } = {};
      try {
        body = JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}");
      } catch {
        /* treated as empty */
      }
      const expiry = () => new Date(Date.now() + 60_000).toISOString();

      if (req.method === "POST" && req.url === "/internal/v1/broker/redeem") {
        const t = brokerState.tickets.get(body.ticket ?? "");
        if (!t || t.used) {
          // Same (status, code) the real broker emits for a dead ticket —
          // the client maps it to broker.ErrTicketInvalid.
          apiErr(res, 401, "UNAUTHENTICATED", "ticket invalid, expired or already redeemed");
          return;
        }
        t.used = true;
        const lease: FakeLease = {
          leaseId: `lease_${++brokerState.seq}`,
          workspaceUID: t.workspaceUID,
          tenantID: "tenant-e2e",
          principalSubject: "e2e|user",
          runtimeGeneration: 1,
          runtimeUID: `rt_${++brokerState.seq}`,
          fencingVersion: 1,
          gatewayID: "tcdi-e2e",
          expiresAt: expiry(),
        };
        brokerState.leases.set(lease.leaseId, lease);
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify(lease));
        return;
      }

      const m = req.url?.match(LEASES_RE);
      if (!m) {
        apiErr(res, 404, "NOT_FOUND", `no route ${req.method} ${req.url}`);
        return;
      }
      const lease = brokerState.leases.get(m[1]);
      if (!lease || brokerState.revoked.has(m[1])) {
        apiErr(res, 410, "NOT_FOUND", "lease gone");
        return;
      }
      switch (m[2]) {
        case "renew":
          lease.fencingVersion += 1;
          lease.expiresAt = expiry();
          res.writeHead(200, { "content-type": "application/json" });
          res.end(JSON.stringify(lease));
          return;
        case "target":
          res.writeHead(200, { "content-type": "application/json" });
          res.end(
            JSON.stringify({
              upstreamURL: `https://127.0.0.1:${UPSTREAM_PORT}`,
              tlsServerName: "upstream.tcdi.localhost",
              // Contract: caPEM is a []byte on the wire (base64 of the PEM).
              caPEM: cert.toString("base64"),
              username: UPSTREAM_USER,
              password: UPSTREAM_PASS,
              protocol: "kasmvnc",
            }),
          );
          return;
        case "revoke":
          brokerState.revoked.add(m[1]);
          res.writeHead(204);
          res.end();
          return;
        case "activity":
          res.writeHead(204);
          res.end();
          return;
      }
    });
  },
);
broker.listen(BROKER_PORT, "127.0.0.1");

// ---------------------------------------------------------------------------
// evil origin (frame-ancestors spec): a foreign page embedding a session host
// ---------------------------------------------------------------------------

const evil = https.createServer(tlsOpts, (req, res) => {
  const u = new URL(req.url ?? "/", "https://evil.tcdi.localhost");
  res.writeHead(200, { "content-type": "text/html" });
  if (u.pathname === "/frame.html") {
    const target = u.searchParams.get("target") ?? "about:blank";
    res.end(
      `<!doctype html><html><body><h1>evil origin</h1>` +
        `<iframe src="${target.replaceAll('"', "&quot;")}"></iframe></body></html>`,
    );
    return;
  }
  res.end("<!doctype html><html><body><h1>evil origin</h1></body></html>");
});
evil.listen(EVIL_PORT, "127.0.0.1");

// ---------------------------------------------------------------------------
// portal router ("the edge"): /v1 + /_control -> mock API, rest -> frontend
// ---------------------------------------------------------------------------

let activeMode: HarnessMode = "lax";
const sessionDomain = () => SESSION_DOMAINS[activeMode];

const CONN_RE = /^\/v1\/workspaces\/([^/]+)\/connections$/;

// The contract mock names its portal cookie "tcdi_session"; the real API
// ships "__Host-tcdi_session" (D17). The edge presents the real name to
// the browser — Secure added, Path=/ already, no Domain — and translates
// it back on the way in, so the cookie surface this suite observes is the
// production one.
const MOCK_COOKIE = "tcdi_session";
const HOST_COOKIE = "__Host-tcdi_session";

function inboundCookie(header: string | undefined): string | undefined {
  if (header === undefined) return undefined;
  return header.replaceAll(`${HOST_COOKIE}=`, `${MOCK_COOKIE}=`);
}

function rewriteSetCookie(h: http.IncomingHttpHeaders): void {
  const sc = h["set-cookie"];
  if (!sc) return;
  const list = Array.isArray(sc) ? sc : [sc];
  h["set-cookie"] = list.map((c) =>
    c.startsWith(`${MOCK_COOKIE}=`)
      ? `${HOST_COOKIE}${c.slice(MOCK_COOKIE.length)}; Secure`
      : c,
  );
}

function pipeUpstream(
  target: { protocol: "http:" | "https:"; port: number },
  req: http.IncomingMessage,
  res: http.ServerResponse,
) {
  const mod = target.protocol === "https:" ? https : http;
  const up = mod.request(
    {
      host: "127.0.0.1",
      port: target.port,
      path: req.url,
      method: req.method,
      headers: req.headers,
      rejectUnauthorized: false,
    },
    (upRes) => {
      res.writeHead(upRes.statusCode ?? 502, upRes.headers);
      upRes.pipe(res);
    },
  );
  up.on("error", () => {
    res.writeHead(502);
    res.end("harness: upstream unreachable");
  });
  req.pipe(up);
}

// handleApi proxies to the mock, rewriting the two session-domain fields
// onto the active site and registering minted tickets with the fake broker.
function handleApi(req: http.IncomingMessage, res: http.ServerResponse) {
  const chunks: Buffer[] = [];
  req.on("data", (c: Buffer) => chunks.push(c));
  req.on("end", () => {
    const raw = Buffer.concat(chunks);
    const up = http.request(
      {
        host: "127.0.0.1",
        port: MOCK_API_PORT,
        path: req.url,
        method: req.method,
        headers: (() => {
          const h = {
            ...req.headers,
            host: `127.0.0.1:${MOCK_API_PORT}`,
            "content-length": String(raw.length),
          };
          const c = inboundCookie(req.headers.cookie);
          if (c === undefined) delete h.cookie;
          else h.cookie = c;
          return h;
        })(),
      },
      (upRes) => {
        const connMatch = req.method === "POST" ? req.url?.match(CONN_RE) : null;
        const isMe = req.method === "GET" && (req.url === "/v1/me" || req.url?.startsWith("/v1/me?"));
        if (!connMatch && !isMe) {
          rewriteSetCookie(upRes.headers);
          res.writeHead(upRes.statusCode ?? 502, upRes.headers);
          upRes.pipe(res);
          return;
        }
        const body: Buffer[] = [];
        upRes.on("data", (c: Buffer) => body.push(c));
        upRes.on("end", () => {
          const buf = Buffer.concat(body);
          let out = buf;
          try {
            if ((upRes.statusCode ?? 0) >= 200 && (upRes.statusCode ?? 0) < 300) {
              const json = JSON.parse(buf.toString("utf8"));
              if (connMatch) {
                const wsId = connMatch[1];
                if (typeof json.ticket === "string") {
                  brokerRegisterTicket(json.ticket, wsId);
                }
                json.launchUrl = `https://${workspaceLabel(wsId)}.${sessionDomain()}/v1/launch`;
              } else {
                json.sessionDomain = sessionDomain();
              }
              out = Buffer.from(JSON.stringify(json), "utf8");
            }
          } catch {
            /* pass the body through untouched */
          }
          const headers = { ...upRes.headers } as http.IncomingHttpHeaders;
          rewriteSetCookie(headers);
          delete headers["content-length"];
          delete headers["transfer-encoding"];
          headers["content-length"] = String(out.length);
          res.writeHead(upRes.statusCode ?? 502, headers);
          res.end(out);
        });
      },
    );
    up.on("error", () => {
      res.writeHead(502);
      res.end("harness: mock api unreachable");
    });
    up.end(raw);
  });
}

const portalRouter = https.createServer(tlsOpts, (req, res) => {
  const p = new URL(req.url ?? "/", "https://portal.tcdi.localhost").pathname;
  if (p === "/v1/login" || p === "/v1/auth/callback" || p.startsWith("/v1/") || p.startsWith("/_control/")) {
    handleApi(req, res);
    return;
  }
  pipeUpstream({ protocol: "https:", port: FRONTEND_PORTS[activeMode] }, req, res);
});
portalRouter.listen(PORTAL_PORT, "127.0.0.1");

// ---------------------------------------------------------------------------
// children: mock API, two frontends, two split-mode backends
// ---------------------------------------------------------------------------

spawnChild("mock api", "node", ["--disable-warning=ExperimentalWarning", "tests/mock-api/server.ts"], {
  cwd: WEB_DIR,
  env: {
    ...process.env,
    MOCK_PORTAL_PORT: String(MOCK_API_PORT),
    MOCK_SESSION_PORT: "4392",
    MOCK_PORTAL_ORIGINS: PORTAL_ORIGIN,
  },
});

function spawnFrontend(mode: HarnessMode) {
  const port = FRONTEND_PORTS[mode];
  spawnChild(`frontend[${mode}]`, path.join(BIN_DIR, "frontend"), [
    `-listen=127.0.0.1:${port}`,
    `-web-root=${DIST_DIR}`,
    `-tls-cert=${CERT_FILE}`,
    `-tls-key=${KEY_FILE}`,
    `-session-domain=${SESSION_DOMAINS[mode]}`,
  ]);
}

function spawnBackend(mode: HarnessMode) {
  const port = SESSION_DOMAINS[mode].split(":")[1];
  const bin = path.join(BIN_DIR, "backend");
  const what = `backend[${mode}]`;
  const start = () =>
    spawnChild(
      what,
      bin,
      [
        // Split/test mode: session listener only — no app, internal or
        // metrics listener, no DB/OIDC/Kubernetes config.
        "-listen=",
        "-internal-listen=",
        "-metrics-listen=",
        `-session-listen=127.0.0.1:${port}`,
        `-session-tls-cert=${CERT_FILE}`,
        `-session-tls-key=${KEY_FILE}`,
        `-session-domain=${SESSION_DOMAINS[mode]}`,
        `-session-cookie-mode=${mode}`,
        `-session-control-hosts=${SESSION_DOMAINS[mode].split(":")[0]}`,
        `-portal-origin=${PORTAL_ORIGIN}`,
        `-gateway-id=tcdi-e2e-${mode}`,
        `-broker-url=https://127.0.0.1:${BROKER_PORT}`,
        `-broker-ca=${CERT_FILE}`,
        `-mtls-cert=${CERT_FILE}`,
        `-mtls-key=${KEY_FILE}`,
        `-upstream-ca=${CERT_FILE}`,
        `-control-token-file=${TOKEN_FILE}`,
      ],
      // The session-reconnect spec kills this child on purpose; an
      // unexpected exit is logged, not suite-fatal.
      { onDown: () => {} },
    );
  return start;
}

const backendStarters: Record<HarnessMode, () => ChildProcess> = {
  lax: spawnBackend("lax"),
  partitioned: spawnBackend("partitioned"),
};

async function restartBackend(mode: HarnessMode): Promise<void> {
  const port = SESSION_DOMAINS[mode].split(":")[1];
  for (const c of [...children]) {
    if (c.spawnargs.includes(`-session-listen=127.0.0.1:${port}`)) {
      c.kill("SIGKILL");
      await new Promise((r) => setTimeout(r, 300));
    }
  }
  backendStarters[mode]();
  await waitFor(`backend[${mode}] restart`, () => responds(`https://127.0.0.1:${port}/`), 30_000);
}

// ---------------------------------------------------------------------------
// loopback control API
// ---------------------------------------------------------------------------

// harnessReady flips once every child has answered its health probe —
// Playwright's webServer gate polls /healthz, so tests must not start
// before the binaries are listening.
let harnessReady = false;

const control = http.createServer((req, res) => {
  const chunks: Buffer[] = [];
  req.on("data", (c: Buffer) => chunks.push(c));
  req.on("end", () => {
    let body: Record<string, unknown> = {};
    try {
      body = JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}");
    } catch {
      /* empty */
    }
    const reply = (status: number, v: unknown) => {
      res.writeHead(status, { "content-type": "application/json" });
      res.end(JSON.stringify(v));
    };
    if (req.method === "GET" && req.url === "/healthz") {
      return reply(harnessReady ? 200 : 503, { ready: harnessReady });
    }
    if (req.method === "POST" && req.url === "/mode") {
      const m = body.mode;
      if (m !== "lax" && m !== "partitioned") return reply(400, { error: "mode must be lax|partitioned" });
      activeMode = m;
      return reply(200, { mode: activeMode });
    }
    if (req.method === "POST" && req.url === "/desktop") {
      upstreamBehavior.topNav = body.topNav === true;
      upstreamBehavior.popup = body.popup === true;
      return reply(200, upstreamBehavior);
    }
    if (req.method === "POST" && req.url === "/broker/reset") {
      brokerReset();
      return reply(200, { reset: true });
    }
    if (req.method === "POST" && req.url === "/backend/restart") {
      const m = body.mode;
      if (m !== "lax" && m !== "partitioned") return reply(400, { error: "mode must be lax|partitioned" });
      if (NO_BINS) return reply(503, { error: "TCDI_E2E_NO_BINS: no binaries running" });
      restartBackend(m)
        .then(() => reply(200, { restarted: m }))
        .catch((e) => reply(500, { error: String(e) }));
      return;
    }
    reply(404, { error: `no control route ${req.method} ${req.url}` });
  });
});
control.listen(CONTROL_PORT, "127.0.0.1");

// Start the two frontends, then the two backends, then gate readiness.
if (!NO_BINS) {
  for (const mode of ["lax", "partitioned"] as const) spawnFrontend(mode);
  backendStarters.lax();
  backendStarters.partitioned();
}

const ready = async () => {
  await waitFor("mock api", () => responds(`${MOCK_API}/_control/health`));
  if (!NO_BINS) {
    await waitFor("frontend[lax]", () => responds(`https://127.0.0.1:${FRONTEND_PORTS.lax}/healthz`));
    await waitFor("frontend[partitioned]", () => responds(`https://127.0.0.1:${FRONTEND_PORTS.partitioned}/healthz`));
    await waitFor("backend[lax]", () => responds(`https://127.0.0.1:${SESSION_DOMAINS.lax.split(":")[1]}/`));
    await waitFor("backend[partitioned]", () =>
      responds(`https://127.0.0.1:${SESSION_DOMAINS.partitioned.split(":")[1]}/`),
    );
  }
  console.log(
    `portal e2e harness up: ${PORTAL_ORIGIN} -> frontend {:${FRONTEND_PORTS.lax},:${FRONTEND_PORTS.partitioned}} + mock :${MOCK_API_PORT}; ` +
      `backends ${SESSION_DOMAINS.lax} / ${SESSION_DOMAINS.partitioned} -> broker :${BROKER_PORT} -> upstream :${UPSTREAM_PORT}`,
  );
  harnessReady = true;
};
void ready();
