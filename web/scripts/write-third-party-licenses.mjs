// Writes dist/THIRD_PARTY_LICENSES.txt: every production dependency of the
// portal bundle with its declared license and license text. Run after
// `vite build` (chained in the `build` script). No external tools needed —
// the package-lock marks dev entries, so production deps are exact.
import { readFileSync, readdirSync, writeFileSync, existsSync } from "node:fs";
import { join } from "node:path";

const root = new URL("..", import.meta.url).pathname;
const lock = JSON.parse(readFileSync(join(root, "package-lock.json"), "utf8"));

const prod = Object.entries(lock.packages ?? {})
  .filter(([k, v]) => k.startsWith("node_modules/") && !v.dev)
  .map(([k]) => k.slice("node_modules/".length))
  .sort();

const LICENSE_FILES = ["LICENSE", "LICENSE.md", "LICENSE.txt", "LICENCE", "COPYING", "COPYING.md"];

const blocks = [];
for (const name of prod) {
  const dir = join(root, "node_modules", name);
  const pkg = JSON.parse(readFileSync(join(dir, "package.json"), "utf8"));
  let text = "";
  for (const f of LICENSE_FILES) {
    if (existsSync(join(dir, f))) {
      text = readFileSync(join(dir, f), "utf8").trim();
      break;
    }
  }
  blocks.push(
    `---\n${pkg.name} ${pkg.version}\nLicense: ${pkg.license ?? "see repository"}\n` +
      (pkg.repository ? `Repository: ${typeof pkg.repository === "string" ? pkg.repository : pkg.repository.url}\n` : "") +
      (text ? `\n${text}\n` : ""),
  );
}

// Vendored assets: the Orbit System 2.9 stylesheets, the WOFF2 font subsets
// and the Tabler icon set ship in the bundle but are not npm dependencies.
// Their license texts live in src/design/orbit/LICENSES/ (provenance and
// SHA-256 in src/design/orbit/README.md).
const vendored = [
  "---\nTinyOrbit Orbit System 2.9 stylesheets (src/design/orbit/css/)\n" +
    "License: TinyOrbit design-system asset; ships with this project.\n",
];
const licensesDir = join(root, "src/design/orbit/LICENSES");
const VENDORED_COMPONENT = {
  "manrope-LICENSE.txt":
    "Manrope Variable font, WOFF2 subsets (src/design/orbit/fonts/manrope-*.woff2)\n" +
    "Source: @fontsource-variable/manrope 5.3.0\nLicense: OFL-1.1",
  "jetbrains-LICENSE.txt":
    "JetBrains Mono font, WOFF2 subsets (src/design/orbit/fonts/jetbrains-mono-*.woff2)\n" +
    "Source: @fontsource/jetbrains-mono 5.3.0\nLicense: OFL-1.1",
  "LICENSE-Tabler.txt":
    "Tabler Icons 3.48.0 outline set (@tabler/icons), vendored as inline SVG components\n" +
    "License: MIT",
};
if (existsSync(licensesDir)) {
  for (const f of readdirSync(licensesDir).sort()) {
    const text = readFileSync(join(licensesDir, f), "utf8").trim();
    vendored.push(`---\n${VENDORED_COMPONENT[f] ?? f}\n\n${text}\n`);
  }
}

const header =
  "TinyCDI portal (tinycdi-portal) — third-party licenses\n" +
  `Generated at build time from package-lock.json (production dependencies only).\n` +
  `The portal itself is MIT-licensed; ${prod.length} production packages ship in dist/.\n` +
  "The TinyOrbit name, wordmark and mark are trademarks of TinyOrbit and are not\n" +
  "covered by the MIT licence (see TRADEMARKS.md at the repository root).\n";

writeFileSync(
  join(root, "dist", "THIRD_PARTY_LICENSES.txt"),
  header + "\n" + blocks.join("\n") + "\nVendored assets\n" + vendored.join("\n"),
);
console.log(`wrote dist/THIRD_PARTY_LICENSES.txt (${prod.length} packages)`);
