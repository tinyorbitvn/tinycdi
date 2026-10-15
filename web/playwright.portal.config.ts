import { defineConfig, devices } from "@playwright/test";
import { PORTAL_ORIGIN, type HarnessMode } from "./tests-portal/harness.ts";

// Real-binary e2e harness — serve.ts builds ./cmd/backend and
// ./build/frontend into web/.e2e-bin and runs the real frontend plus the
// real backend session listener in split mode against a fake broker (mTLS)
// and a fake KasmVNC upstream. Two projects exercise both cookie modes
// (D16): the lax project keeps portal and session on tcdi.localhost, the
// partitioned project serves sessions from tcdi-other.localhost.
// Needs a Go toolchain and openssl.
export default defineConfig<{ harnessMode: HarnessMode }>({
  testDir: "./tests-portal",
  testMatch: "**/*.spec.ts",
  timeout: 60_000,
  workers: 1,
  retries: 0,
  reporter: [["list"]],
  use: {
    baseURL: PORTAL_ORIGIN,
    ignoreHTTPSErrors: true, // self-signed cert minted by tests-portal/serve.ts
    trace: "retain-on-failure",
  },
  projects: [
    {
      name: "lax",
      use: { ...devices["Desktop Chrome"], harnessMode: "lax" },
    },
    {
      name: "partitioned",
      use: { ...devices["Desktop Chrome"], harnessMode: "partitioned" },
    },
  ],
  webServer: {
    command:
      "node --disable-warning=ExperimentalWarning tests-portal/serve.ts",
    url: "http://127.0.0.1:4176/healthz",
    ignoreHTTPSErrors: true,
    reuseExistingServer: !process.env.CI,
    timeout: 300_000,
  },
});
