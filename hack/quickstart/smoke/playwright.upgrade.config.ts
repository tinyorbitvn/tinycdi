import { defineConfig } from "@playwright/test";
import base from "./playwright.config";

// N-1 -> N upgrade spec (hack/quickstart/upgrade-test.sh). Same cluster and
// browser settings as the quickstart smoke; a different testMatch so
// `npx playwright test` in the quickstart CI job still runs only
// smoke.spec.ts. One test drives seed -> live-upgrade -> verify, so the
// timeout covers the whole arc.
export default defineConfig({
  ...base,
  testMatch: "upgrade.spec.ts",
  timeout: 40 * 60_000,
});
