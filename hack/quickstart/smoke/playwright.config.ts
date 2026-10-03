import { defineConfig, devices } from "@playwright/test";

// Quickstart smoke (hack/quickstart/up.sh must have finished). Everything
// under *.<domain> resolves to loopback on the public DNS; the host-resolver
// rule keeps the run independent of the network's DNS. The cluster uses a
// self-made CA, so certificate errors are ignored here (up.sh already
// verified the chain with curl --cacert).
const domain = process.env.TCDI_QS_DOMAIN ?? "tcdi.localtest.me";

export default defineConfig({
  testDir: ".",
  testMatch: "smoke.spec.ts",
  // Image pulls and a cold desktop boot dominate the first workspace.
  timeout: 15 * 60_000,
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
