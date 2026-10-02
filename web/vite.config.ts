import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";

// Operator branding (/branding/tokens.css, served by the frontend) overrides
// the design tokens. Vite appends the bundled stylesheet after the links in
// index.html, which would put the override first and let the defaults win, so
// move the branding link to the end of <head> once the bundle link exists.
const BRANDING_LINK = /[ \t]*<link\b[^>]*\bhref="\/branding\/tokens\.css"[^>]*>[ \t]*\n?/;

function brandingLast(): Plugin {
  return {
    name: "tinycdi-branding-last",
    transformIndexHtml: {
      order: "post",
      handler(html) {
        const link = BRANDING_LINK.exec(html);
        if (!link) return html;
        return html.replace(BRANDING_LINK, "").replace("</head>", `${link[0].trimEnd()}\n  </head>`);
      },
    },
  };
}

// The portal is served on the portal origin alongside the API, so /v1 is a
// same-origin path. In dev, proxy it to the mock API (tests/mock-api).
export default defineConfig({
  plugins: [react(), brandingLast()],
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
