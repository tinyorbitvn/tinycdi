// lint:strings — fails when a .tsx file under web/src (outside src/i18n/)
// carries a user-visible literal: a JsxText node containing a letter, or a
// string literal passed to aria-label, title, placeholder or alt. Those
// strings belong in the message catalog (src/i18n/en.ts, D33). Positional
// args are extra files or directories to check instead of the default scan.
import ts from "typescript";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

const webRoot = fileURLToPath(new URL("..", import.meta.url));
const srcRoot = join(webRoot, "src");
const i18nDir = join(srcRoot, "i18n") + sep;
const CHECKED_ATTRS = new Set(["aria-label", "title", "placeholder", "alt"]);
const LETTER = /\p{L}/u;

function collect(path, out = []) {
  if (statSync(path).isDirectory()) {
    for (const entry of readdirSync(path)) collect(join(path, entry), out);
  } else if (path.endsWith(".tsx")) {
    out.push(path);
  }
  return out;
}

function checkFile(path) {
  const source = ts.createSourceFile(
    path,
    readFileSync(path, "utf8"),
    ts.ScriptTarget.Latest,
    true,
    ts.ScriptKind.TSX,
  );
  const problems = [];
  const visit = (node) => {
    let what = null;
    if (ts.isJsxText(node) && LETTER.test(node.getText(source))) {
      what = `JSX text ${JSON.stringify(node.getText(source).trim())}`;
    } else if (
      ts.isJsxAttribute(node) &&
      CHECKED_ATTRS.has(node.name.getText(source)) &&
      node.initializer !== undefined &&
      ts.isStringLiteral(node.initializer)
    ) {
      what = `attribute ${node.name.getText(source)}=${node.initializer.getText(source)}`;
    }
    if (what !== null) {
      const { line, character } = source.getLineAndCharacterOfPosition(
        node.getStart(source),
      );
      problems.push(`${path}:${line + 1}:${character + 1}: literal string ${what}`);
    }
    node.forEachChild(visit);
  };
  visit(source);
  return problems;
}

const args = process.argv.slice(2).map((a) => resolve(a));
const roots = args.length > 0 ? args : [srcRoot];
const files = roots
  .flatMap((root) => collect(root))
  .filter((file) => !file.startsWith(i18nDir));

// Catalog parity (E11): every locale directory under src/i18n must carry
// the same key set as en/, per area file. Missing keys are reported as
// "missing", keys with no English counterpart as "extra". Only runs in the
// default full scan — positional args mean a targeted literal check.
const KEY_LINE = /^\s*"((?:[^"\\]|\\.)+)"\s*:/gm;
function catalogKeys(file) {
  return new Set(
    [...readFileSync(file, "utf8").matchAll(KEY_LINE)].map((m) => m[1]),
  );
}

const catalogProblems = [];
if (args.length === 0) {
  const enDir = join(i18nDir, "en");
  const areas = readdirSync(enDir).filter((f) => f.endsWith(".ts"));
  for (const entry of readdirSync(i18nDir, { withFileTypes: true })) {
    if (!entry.isDirectory() || entry.name === "en") continue;
    for (const area of areas) {
      const enKeys = catalogKeys(join(enDir, area));
      const localeFile = join(i18nDir, entry.name, area);
      let localeKeys;
      try {
        localeKeys = catalogKeys(localeFile);
      } catch {
        catalogProblems.push(`${localeFile}: missing area file`);
        continue;
      }
      const missing = [...enKeys].filter((k) => !localeKeys.has(k));
      const extra = [...localeKeys].filter((k) => !enKeys.has(k));
      if (missing.length > 0) {
        catalogProblems.push(`${localeFile}: missing keys: ${missing.join(", ")}`);
      }
      if (extra.length > 0) {
        catalogProblems.push(`${localeFile}: extra keys: ${extra.join(", ")}`);
      }
    }
  }
}

const problems = files.flatMap(checkFile);
for (const problem of [...problems, ...catalogProblems]) console.log(problem);
if (problems.length > 0 || catalogProblems.length > 0) {
  console.error(
    `${problems.length + catalogProblems.length} problem(s) found — move user-visible text into src/i18n/en.ts and keep locale catalogs in key parity`,
  );
  process.exit(1);
}
