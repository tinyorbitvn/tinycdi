import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, test } from "vitest";

// D31: components never take or emit a style attribute/prop — geometry lives
// in the stylesheets only.
const SRC = join(import.meta.dirname, "..", "..", "..", "src");

function* walk(dir: string): Generator<string> {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, entry.name);
    if (entry.isDirectory()) yield* walk(p);
    else if (entry.name.endsWith(".tsx")) yield p;
  }
}

describe("design system", () => {
  test("design: no style props", () => {
    const offenders: string[] = [];
    for (const file of walk(SRC)) {
      const source = readFileSync(file, "utf8");
      if (/\bstyle\s*=/.test(source)) offenders.push(file);
    }
    expect(offenders).toEqual([]);
  });
});
