import { defineConfig, devices } from "@playwright/test";

// Partitioned-cookie smoke (B6-PART). Same kind quickstart as
// playwright.config.ts, installed with backend.sessionCookieMode:
// partitioned (TCDI_QS_VALUES_OVERLAY=hack/quickstart/values-partitioned.yaml).
//
// Browser assumption: the Partitioned attribute (CHIPS) needs a
// Chromium-based browser — shipped in Chromium 118. This @playwright/test
// pin bundles Chromium 153 (playwright-core browsers.json) and the default
// headless shell shares the headed build's network stack and cookie store,
// so CHIPS behaves identically. Do not switch this variant to
// Firefox/WebKit; their partitioned-cookie surface differs.
const domain = process.env.TCDI_QS_DOMAIN ?? "tcdi.localtest.me";

export default defineConfig({
  testDir: ".",
  testMatch: "partitioned.spec.ts",
  // A full lifecycle in one test: login, workspace boot, backend rollout,
  // stop/start, logout. Cold pod boots dominate.
  timeout: 20 * 60_000,
  expect: { timeout: 30_000 },
  workers: 1,
  retries: 0,
  reporter: [["list"]],
  outputDir: "test-results",
  use: {
    ...devices["Desktop Chrome"],
    baseURL: process.env.TCDI_QS_PORTAL_URL ?? `https://portal.${domain}`,
    ignoreHTTPSErrors: true,
    launchOptions: { args: [`--host-resolver-rules=MAP *.${domain} 127.0.0.1`] },
    actionTimeout: 30_000,
    navigationTimeout: 60_000,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
});
