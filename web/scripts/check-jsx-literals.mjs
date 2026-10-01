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

const problems = files.flatMap(checkFile);
for (const problem of problems) console.log(problem);
if (problems.length > 0) {
  console.error(
    `${problems.length} literal string(s) found — move user-visible text into src/i18n/en.ts`,
  );
  process.exit(1);
}
