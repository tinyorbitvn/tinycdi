import { describe, expect, it } from "vitest";
import { createHash } from "node:crypto";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";

// The Orbit 2.9 vendored stylesheets and the --tc-* alias layer are loaded as
// text; these tests pin the import order, the alias-only rule and the vendored
// file hashes recorded in src/design/orbit/README.md.

const web = process.cwd(); // vitest runs with cwd = web/
const read = (p: string) => readFileSync(join(web, p), "utf8");

// Binding Orbit 2.9 load order. "./base.css" and "../design/design.css" are
// inserted by T3.2 at the marked slot, so the test asserts ordering of the
// pinned sequence rather than the complete list.
const EXPECTED_ORDER = [
  "../design/orbit/css/fonts.css",
  "../design/orbit/css/tokens.css",
  "../design/orbit/css/expressive-tokens.css",
  "../design/orbit/css/extended-colors.css",
  "../design/orbit/css/companion-palettes.css",
  "./tokens.css",
  "./base.css",
  "../design/design.css",
  "../design/orbit/css/component-language.css",
  "../design/orbit/css/navy-night.css",
  "../design/orbit/css/palette-system.css",
];

describe("Orbit vendored tokens", () => {
  it("index.css import order", () => {
    const css = read("src/styles/index.css");
    const code = css.replace(/\/\*[\s\S]*?\*\//g, ""); // the T3.2 slot comment names imports
    const imports = [...code.matchAll(/@import\s+["']([^"']+)["']/g)].map((m) => m[1]);
    for (const i of imports) expect(EXPECTED_ORDER).toContain(i);
    const positions = EXPECTED_ORDER.filter((e) => imports.includes(e)).map((e) =>
      imports.indexOf(e),
    );
    expect(positions).toEqual([...positions].sort((a, b) => a - b));
    expect(imports.at(-1)).toBe("../design/orbit/css/palette-system.css");
    // Until T3.2 fills it, the app-CSS slot must stay marked between the alias
    // layer and component-language.
    if (!imports.includes("./base.css")) {
      expect(css).toContain("T3.2 inserts");
      expect(css.indexOf('T3.2 inserts')).toBeGreaterThan(css.indexOf('"./tokens.css"'));
      expect(css.indexOf("T3.2 inserts")).toBeLessThan(
        css.indexOf("component-language.css"),
      );
    }
  });

  it("alias layer has no literals", () => {
    const css = read("src/styles/tokens.css");
    expect(css).not.toMatch(/#[0-9a-fA-F]{3,8}\b/);
    expect(css).not.toContain("rgb(");
    expect(css).not.toContain("hsl(");
    const decls = [...css.matchAll(/--tc-color-[a-z0-9-]+\s*:\s*([^;]+);/g)].map((m) => m[1].trim());
    expect(decls.length).toBeGreaterThan(0);
    for (const value of decls) expect(value).toMatch(/^var\(--to-[a-z0-9-]+\)$/);
  });

  it("no --tc colour defined elsewhere", () => {
    const offenders: string[] = [];
    const walk = (dir: string) => {
      for (const name of readdirSync(dir)) {
        const p = join(dir, name);
        if (statSync(p).isDirectory()) {
          walk(p);
        } else if (/\.(css|tsx?|html)$/.test(name)) {
          const text = readFileSync(p, "utf8");
          if (/--tc-color-[a-z0-9-]+\s*:/.test(text)) offenders.push(relative(web, p));
        }
      }
    };
    walk(join(web, "src"));
    expect(offenders).toEqual(["src/styles/tokens.css"]);
  });

  it("fonts are local", () => {
    const css = read("src/design/orbit/css/fonts.css");
    const urls = [...css.matchAll(/url\((["']?)([^"')]+)\1\)/g)].map((m) => m[2]);
    expect(urls.length).toBeGreaterThan(0);
    for (const u of urls) {
      expect(u).toMatch(/^\.\.\/fonts\//);
      expect(u).not.toContain("http");
    }
  });

  it("vendored files match manifest", () => {
    const readme = read("src/design/orbit/README.md");
    const cssDir = join(web, "src/design/orbit/css");
    for (const name of readdirSync(cssDir).filter((n) => n.endsWith(".css") && n !== "fonts.css")) {
      const sha = createHash("sha256").update(readFileSync(join(cssDir, name))).digest("hex");
      expect(readme, `README.md records sha256 of css/${name}`).toContain(sha);
    }
  });
});
