import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The portal is served on the portal origin alongside the API, so /v1 is a
// same-origin path. In dev, proxy it to the mock API (tests/mock-api).
export default defineConfig({
  plugins: [react()],
  // No inlined assets: fonts, images and scripts stay separate files so the
  // strict CSP (default-src 'self') never has to allow data: URLs.
  build: { assetsInlineLimit: 0 },
  server: {
    proxy: {
      "/v1": {
        target: process.env.MOCK_API_ORIGIN ?? "http://127.0.0.1:4310",
        changeOrigin: false,
      },
    },
  },
});
