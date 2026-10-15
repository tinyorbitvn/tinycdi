import { describe, expect, it } from "vitest";
import { spawnSync } from "node:child_process";
import { readdirSync, readFileSync } from "node:fs";
import { join, resolve, sep } from "node:path";
import { en } from "../../../src/i18n/en";
import { t } from "../../../src/i18n";

// vitest runs with cwd = web/ (the vitest.config.ts root). Under jsdom
// import.meta.url is an http:// URL, so resolve paths from cwd instead.
const webRoot = process.cwd();
const srcRoot = join(webRoot, "src");
const i18nDir = join(srcRoot, "i18n") + sep;

// All source text outside the catalog itself: a catalog key counts as used
// when it appears as a string literal somewhere in web/src.
function sourceCorpus(dir: string): string {
  let out = "";
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) {
      if (path + sep !== i18nDir) out += sourceCorpus(path);
    } else {
      out += readFileSync(path, "utf8");
    }
  }
  return out;
}

describe("i18n catalog", () => {
  it("t: substitutes params", () => {
    expect(t("workspaces.list.count", { n: 3 })).toBe("3 workspaces");
  });

  it("t: missing param throws in tests", () => {
    expect(() => t("workspaces.list.count")).toThrow();
    expect(() => t("workspaces.list.count", {})).toThrow();
    expect(() => t("workspaces.list.count", { other: 1 })).toThrow();
  });

  it("catalog: no emoji", () => {
    for (const [key, value] of Object.entries(en)) {
      expect(value, key).not.toMatch(/\p{Extended_Pictographic}/u);
    }
  });

  it("catalog: no unused keys", () => {
    const corpus = sourceCorpus(srcRoot);
    for (const key of Object.keys(en)) {
      expect(corpus, `unused key: ${key}`).toContain(`"${key}"`);
    }
  });

  it(
    "lint:strings",
    () => {
      const script = join(webRoot, "scripts", "check-jsx-literals.mjs");
      const clean = spawnSync(process.execPath, [script], {
        cwd: webRoot,
        encoding: "utf8",
      });
      expect(clean.status, clean.stdout + clean.stderr).toBe(0);

      const fixture = resolve(
        webRoot,
        "tests/unit/i18n/fixtures/jsx-literal.tsx",
      );
      const dirty = spawnSync(process.execPath, [script, fixture], {
        cwd: webRoot,
        encoding: "utf8",
      });
      expect(dirty.status, dirty.stdout + dirty.stderr).toBe(1);
    },
    // Spawning node + the TypeScript parser twice exceeds the 5s default.
    30_000,
  );
});
