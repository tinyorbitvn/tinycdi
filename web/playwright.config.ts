import { defineConfig, devices } from "@playwright/test";

// Portal UI harness — runs the Vite dev server plus the contract mock
// (tests/mock-api): portal API on :4310, session origin on :4311.
export default defineConfig({
  testDir: "./tests",
  testMatch: "**/*.spec.ts",
  timeout: 30_000,
  workers: 1,
  retries: 0,
  reporter: [["list"]],
  use: {
    baseURL: "http://127.0.0.1:4173",
    trace: "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: [
    {
      command:
        "node --disable-warning=ExperimentalWarning tests/mock-api/server.ts",
      url: "http://127.0.0.1:4310/_control/health",
      reuseExistingServer: !process.env.CI,
      timeout: 30_000,
    },
    {
      command: "npm run dev -- --port 4173 --strictPort --host 127.0.0.1",
      url: "http://127.0.0.1:4173/",
      reuseExistingServer: !process.env.CI,
      timeout: 60_000,
    },
  ],
});
