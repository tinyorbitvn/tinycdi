import { defineConfig, devices } from "@playwright/test";
import { PORTAL_ORIGIN } from "./tests-portal/harness.ts";

// Portal-CSP harness — serves the BUILT SPA through the real Go
// portal binary (build/portal) with its real security headers, backed by
// the contract mock on a second local HTTPS origin as the session host
// (see tests-portal/serve.ts). The mock's /v1/launch enforces the real
// gateway's ADR-0004 origin gate against the portal origin, so
// this suite also catches a Referrer-Policy regression that nulls the
// launch POST's Origin. Needs a Go toolchain and openssl.
export default defineConfig({
  testDir: "./tests-portal",
  testMatch: "**/*.spec.ts",
  timeout: 30_000,
  workers: 1,
  retries: 0,
  reporter: [["list"]],
  use: {
    baseURL: PORTAL_ORIGIN,
    ignoreHTTPSErrors: true, // self-signed cert minted by tests-portal/serve.ts
    trace: "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: {
    command:
      "node --disable-warning=ExperimentalWarning tests-portal/serve.ts",
    url: `${PORTAL_ORIGIN}/healthz`,
    ignoreHTTPSErrors: true,
    reuseExistingServer: !process.env.CI,
    timeout: 120_000,
  },
});
