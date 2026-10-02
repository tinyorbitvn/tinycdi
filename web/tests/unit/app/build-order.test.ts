import { mkdtempSync, readFileSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterAll, describe, expect, it } from "vitest";
import { build } from "vite";

const root = join(import.meta.dirname, "../../..");
const outDir = mkdtempSync(join(tmpdir(), "tcdi-build-order-"));

afterAll(() => rmSync(outDir, { recursive: true, force: true }));

// Operator branding (/branding/tokens.css) overrides the design tokens, so it
// has to be the LAST stylesheet in the built document: with equal specificity
// the later rule wins.
describe("built index.html", () => {
  it("links /branding/tokens.css after every bundled stylesheet", async () => {
    await build({ root, logLevel: "silent", build: { outDir, emptyOutDir: true, write: true } });
    expect(readdirSync(join(outDir, "assets")).some((f) => f.endsWith(".css"))).toBe(true);

    const html = readFileSync(join(outDir, "index.html"), "utf8");
    const sheets = [...html.matchAll(/<link\b[^>]*\brel="stylesheet"[^>]*>/g)].map((m) => m[0]);
    const hrefs = sheets.map((tag) => /\bhref="([^"]+)"/.exec(tag)?.[1] ?? "");
    const tokens = hrefs.indexOf("/branding/tokens.css");
    const lastBundled = hrefs.map((h, i) => (h.startsWith("/assets/") && h.endsWith(".css") ? i : -1)).reduce((a, b) => Math.max(a, b), -1);

    expect(tokens, `tokens.css link missing in ${hrefs.join(", ")}`).toBeGreaterThanOrEqual(0);
    expect(lastBundled, "no bundled stylesheet link").toBeGreaterThanOrEqual(0);
    expect(tokens, `stylesheet order: ${hrefs.join(", ")}`).toBeGreaterThan(lastBundled);
  }, 180_000);
});
