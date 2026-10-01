import { existsSync, readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, test } from "vitest";

// Every var(--tc-*) a component or shell stylesheet uses must be defined
// somewhere in web/src — in styles/tokens.css (the T3.1 alias layer) or as a
// scoped custom property inside a component rule (e.g. --tc-badge-bg). The
// alias layer lands with T3.1; until it is merged this test self-skips.
const SRC = join(import.meta.dirname, "..", "..", "..", "src");
const TOKENS = join(SRC, "styles", "tokens.css");

function* walk(dir: string): Generator<string> {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, entry.name);
    if (entry.isDirectory()) yield* walk(p);
    else if (entry.name.endsWith(".css")) yield p;
  }
}

function definedTokens(files: string[]): Set<string> {
  const defined = new Set<string>();
  for (const file of files) {
    for (const m of readFileSync(file, "utf8").matchAll(/(--tc-[a-z0-9-]+)\s*:/gi)) {
      defined.add(m[1]!.toLowerCase());
    }
  }
  return defined;
}

function usedTokens(files: string[]): Set<string> {
  const used = new Set<string>();
  for (const file of files) {
    for (const m of readFileSync(file, "utf8").matchAll(/var\(\s*(--tc-[a-z0-9-]+)/gi)) {
      used.add(m[1]!.toLowerCase());
    }
  }
  return used;
}

describe("token references", () => {
  test.runIf(existsSync(TOKENS))("every var(--tc-*) used is defined", () => {
    const files = [...walk(SRC)];
    const defined = definedTokens(files);
    const missing = [...usedTokens(files)].filter((tok) => !defined.has(tok));
    expect(missing).toEqual([]);
  });
});
