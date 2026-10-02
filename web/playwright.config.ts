import { defineConfig, devices } from "@playwright/test";

// Portal UI harness — runs the Vite dev server plus the contract mock
// (tests/mock-api): portal API on :4310, session origin on :4311.
// Ports are env-overridable so parallel checkouts can run the suite without
// reuseExistingServer cross-talk resetting each other's mock state.
const PORTAL_PORT = process.env.PW_PORTAL_PORT ?? "4173";
const MOCK_API_PORT = process.env.MOCK_PORTAL_PORT ?? "4310";
const MOCK_SESSION_PORT = process.env.MOCK_SESSION_PORT ?? "4311";

export default defineConfig({
  testDir: "./tests",
  testMatch: "**/*.spec.ts",
  timeout: 30_000,
  workers: 1,
  retries: 0,
  reporter: [["list"]],
  use: {
    baseURL: `http://127.0.0.1:${PORTAL_PORT}`,
    trace: "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: [
    {
      command: `MOCK_PORTAL_PORT=${MOCK_API_PORT} MOCK_SESSION_PORT=${MOCK_SESSION_PORT} MOCK_PORTAL_ORIGINS=http://127.0.0.1:${PORTAL_PORT} node --disable-warning=ExperimentalWarning tests/mock-api/server.ts`,
      url: `http://127.0.0.1:${MOCK_API_PORT}/_control/health`,
      reuseExistingServer: !process.env.CI,
      timeout: 30_000,
    },
    {
      command: `MOCK_API_ORIGIN=http://127.0.0.1:${MOCK_API_PORT} npm run dev -- --port ${PORTAL_PORT} --strictPort --host 127.0.0.1`,
      url: `http://127.0.0.1:${PORTAL_PORT}/`,
      reuseExistingServer: !process.env.CI,
      timeout: 60_000,
    },
  ],
});
