import { readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";

// Tiny CSS reader for contract tests: enough to pull the declarations of a
// selector out of our own stylesheets (no nesting beyond @media).

export const WEB = process.cwd(); // vitest runs with cwd = web/
export const readWeb = (p: string): string => readFileSync(join(WEB, p), "utf8");

/** Every .css under web/src except the vendored Orbit sheets, web-relative. */
export function appCssFiles(): string[] {
  const out: string[] = [];
  const orbit = join("src", "design", "orbit");
  const walk = (dir: string) => {
    for (const e of readdirSync(dir, { withFileTypes: true })) {
      const p = join(dir, e.name);
      if (e.isDirectory()) {
        if (relative(WEB, p) !== orbit) walk(p);
      } else if (e.name.endsWith(".css")) {
        out.push(relative(WEB, p));
      }
    }
  };
  walk(join(WEB, "src"));
  return out.sort();
}

export function stripComments(css: string): string {
  return css.replace(/\/\*[\s\S]*?\*\//g, "");
}

function blockEnd(src: string, open: number): number {
  let depth = 1;
  let j = open + 1;
  while (depth > 0 && j < src.length) {
    if (src[j] === "{") depth++;
    else if (src[j] === "}") depth--;
    j++;
  }
  return j;
}

/** The stylesheet without any @-block (@media, @keyframes, @supports…). */
export function topLevel(css: string): string {
  const src = stripComments(css);
  let out = "";
  let i = 0;
  while (i < src.length) {
    if (src[i] === "@") {
      const open = src.indexOf("{", i);
      const semi = src.indexOf(";", i);
      if (open === -1 || (semi !== -1 && semi < open)) {
        i = semi === -1 ? src.length : semi + 1;
        continue;
      }
      i = blockEnd(src, open);
      continue;
    }
    out += src[i++];
  }
  return out;
}

/** Bodies of every `@media <query> { … }` block, in file order. */
export function mediaBlocks(css: string, query: string): string[] {
  const src = stripComments(css);
  const needle = `@media ${query}`;
  const out: string[] = [];
  let i = 0;
  while ((i = src.indexOf(needle, i)) !== -1) {
    const open = src.indexOf("{", i);
    const end = blockEnd(src, open);
    out.push(src.slice(open + 1, end - 1));
    i = end;
  }
  return out;
}

/** Merged declarations of every rule whose selector list contains `selector` exactly. */
export function declarations(css: string, selector: string): Record<string, string> {
  const decls: Record<string, string> = {};
  for (const m of stripComments(css).matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    const selectors = m[1]!.split(",").map((s) => s.trim());
    if (!selectors.includes(selector)) continue;
    for (const d of m[2]!.split(";")) {
      const k = d.indexOf(":");
      if (k < 0) continue;
      decls[d.slice(0, k).trim()] = d.slice(k + 1).trim().replace(/\s+/g, " ");
    }
  }
  return decls;
}
