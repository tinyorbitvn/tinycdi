import { spawnSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

// Spec §5: the README embeds themed captures produced by
// web/scripts/screenshots.mjs; this pins README ⇄ files ⇄ generator.
const WEB = process.cwd(); // vitest runs with cwd = web/
const ROOT = join(WEB, "..");
const README = readFileSync(join(ROOT, "README.md"), "utf8");
const SHOTS = "docs/images/screenshots";
const PORTAL = [
  "portal-workspaces",
  "portal-create",
  "portal-detail",
  "admin-overview",
  "admin-session-policy",
  "admin-audit",
];
const RUNTIME = ["session-desktop", "session-browser"];
const escape = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

describe("README screenshots", () => {
  it.each(PORTAL)("%s is a <picture> with existing light and dark captures", (stem) => {
    const dark = `${SHOTS}/${stem}-dark.png`;
    const light = `${SHOTS}/${stem}-light.png`;
    expect(README).toContain(`<source media="(prefers-color-scheme: dark)" srcset="${dark}">`);
    expect(README).toMatch(new RegExp(`<img src="${escape(light)}" alt="[^"]+">`));
    expect(existsSync(join(ROOT, dark)), dark).toBe(true);
    expect(existsSync(join(ROOT, light)), light).toBe(true);
  });

  it.each(RUNTIME)("%s is one capture used for both schemes", (stem) => {
    expect(README).toContain(`${SHOTS}/${stem}.png`);
    expect(existsSync(join(ROOT, SHOTS, `${stem}.png`))).toBe(true);
  });

  it("the unthemed portal captures are gone", () => {
    for (const f of ["portal-data.png", "portal-workspaces.png", "portal-create.png", "portal-detail.png"]) {
      expect(existsSync(join(ROOT, SHOTS, f)), f).toBe(false);
      expect(README).not.toContain(`${SHOTS}/${f}`);
    }
  });

  it("the generator lists exactly the README files", () => {
    const all = spawnSync(process.execPath, ["scripts/screenshots.mjs", "--list"], { cwd: WEB, encoding: "utf8", timeout: 15_000 });
    expect(all.status).toBe(0);
    expect(all.stdout.trim().split("\n")).toEqual([
      ...PORTAL.flatMap((s) => [`${s}-light.png`, `${s}-dark.png`]),
      ...RUNTIME.map((s) => `${s}.png`),
    ]);
    const portal = spawnSync(process.execPath, ["scripts/screenshots.mjs", "--list", "--only", "portal"], {
      cwd: WEB,
      encoding: "utf8",
      timeout: 15_000,
    });
    expect(portal.stdout.trim().split("\n")).toHaveLength(12);
    const bad = spawnSync(process.execPath, ["scripts/screenshots.mjs", "--only", "nope"], { cwd: WEB, encoding: "utf8", timeout: 15_000 });
    expect(bad.status).not.toBe(0);
  });

  it("Features cover the v1.0.0 surfaces", () => {
    for (const s of ["Admin console", "Session policy", "Audit log", "Sign out everywhere", "hack/quickstart/up.sh"]) {
      expect(README, s).toContain(s);
    }
  });
});
