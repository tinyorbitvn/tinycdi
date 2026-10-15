import { describe, expect, it } from "vitest";
import { appCssFiles, declarations, readWeb, stripComments, topLevel } from "./css";

// Spec §3.1/§3.2 + §6: the design layer contract the Orbit polish pass
// promises. Branding overrides (docs/branding.md) rely on the last three
// guards: app CSS never reads --to-* directly and tokens.css declares on
// :root only, so an operator's later `:root { --tc-… }` always wins.

const NEW_TOKENS = [
  "--tc-text-2xs",
  "--tc-control-font-size",
  "--tc-control-font-weight",
  "--tc-color-stage",
  "--tc-color-danger-strong",
  "--tc-color-danger-strong-hover",
  "--tc-color-on-danger",
  "--tc-color-meter-track",
  "--tc-color-meter-warning",
  "--tc-color-meter-danger",
  "--tc-color-table-head",
  "--tc-color-row-hover",
];

const design = () => topLevel(readWeb("src/design/design.css"));

describe("design layer contract", () => {
  it("--tc-space-5 is gone everywhere", () => {
    for (const f of appCssFiles()) expect(readWeb(f), f).not.toContain("--tc-space-5");
    expect(readWeb("src/design/design.css")).not.toContain(".tc-gap-5");
  });

  it("no hex colour literal outside the vendored Orbit sheets", () => {
    for (const f of appCssFiles()) expect(readWeb(f), f).not.toMatch(/#[0-9a-fA-F]{3,8}\b/);
  });

  it("only styles/tokens.css reads --to-* (branding overrides on --tc-* always win)", () => {
    for (const f of appCssFiles()) {
      if (f !== "src/styles/tokens.css") expect(readWeb(f), f).not.toContain("var(--to-");
    }
  });

  it("tokens.css declares on :root only", () => {
    const selectors = [...stripComments(readWeb("src/styles/tokens.css")).matchAll(/([^{}]+)\{/g)].map((m) =>
      m[1]!.trim(),
    );
    expect(selectors).toEqual([":root"]);
  });

  it("new tokens are defined in tokens.css and documented for branding", () => {
    const tokens = readWeb("src/styles/tokens.css");
    const branding = readWeb("../docs/branding.md");
    for (const name of NEW_TOKENS) {
      expect(tokens, name).toMatch(new RegExp(`${name}\\s*:`));
      expect(branding, name).toContain(`\`${name}\``);
    }
  });

  it("buttons use the Orbit control type: 12px/600, lg keeps 14px", () => {
    const css = design();
    expect(declarations(css, ".tc-button")).toMatchObject({
      "font-size": "var(--tc-control-font-size)",
      "font-weight": "var(--tc-control-font-weight)",
    });
    expect(declarations(css, ".tc-button--sm")["font-size"]).toBeUndefined();
    expect(declarations(css, ".tc-button--lg")["font-size"]).toBe("var(--tc-text-md)");
  });

  it("field label 12px/600; hint and error 11px/400", () => {
    const css = design();
    expect(declarations(css, ".tc-field__label")).toMatchObject({
      "font-size": "var(--tc-text-xs)",
      "font-weight": "var(--tc-weight-semibold)",
    });
    for (const sel of [".tc-field__hint", ".tc-field__error"]) {
      expect(declarations(css, sel), sel).toMatchObject({
        "font-size": "var(--tc-text-2xs)",
        "font-weight": "var(--tc-weight-regular)",
      });
    }
  });

  it("dl dt and dd share 13px; dt is muted 600", () => {
    const css = design();
    expect(declarations(css, ".tc-dl dt")).toMatchObject({
      "font-size": "var(--tc-text-sm)",
      "font-weight": "var(--tc-weight-semibold)",
      color: "var(--tc-color-text-muted)",
    });
    expect(declarations(css, ".tc-dl dd")["font-size"]).toBe("var(--tc-text-sm)");
  });

  it("cards have no shadow; interactive hover changes the border only", () => {
    const css = design();
    expect(declarations(css, ".tc-card")["box-shadow"]).toBeUndefined();
    const hover = declarations(css, ".tc-card--interactive:hover");
    expect(hover["box-shadow"]).toBeUndefined();
    expect(hover["border-color"]).toBe("var(--tc-color-border-strong)");
  });

  it("danger buttons are solid with white text; danger-ghost is outlined", () => {
    const css = design();
    expect(declarations(css, ".tc-button--danger")).toMatchObject({
      background: "var(--tc-color-danger-strong)",
      color: "var(--tc-color-on-danger)",
    });
    expect(
      declarations(css, '.tc-button--danger:hover:not(:disabled):not([aria-disabled="true"])').background,
    ).toBe("var(--tc-color-danger-strong-hover)");
    expect(declarations(css, ".tc-button--danger-ghost")).toMatchObject({
      background: "transparent",
      "border-color": "var(--tc-color-danger-border)",
      color: "var(--tc-color-danger-text)",
    });
  });

  it("meters use full-strength status colours on a visible track", () => {
    const css = design();
    expect(declarations(css, ".tc-meter__bar").background).toBe("var(--tc-color-meter-track)");
    expect(declarations(css, ".tc-meter__bar::-webkit-meter-bar").background).toBe("var(--tc-color-meter-track)");
    expect(declarations(css, ".tc-meter--warning .tc-meter__bar")["--tc-meter-fill"]).toBe(
      "var(--tc-color-meter-warning)",
    );
    expect(declarations(css, ".tc-meter--danger .tc-meter__bar")["--tc-meter-fill"]).toBe(
      "var(--tc-color-meter-danger)",
    );
  });

  it("dark mode has a flat canvas (no purple page glow)", () => {
    const base = topLevel(readWeb("src/styles/base.css"));
    // navy-night.css (loaded later) sets the gradient on
    // `html[data-theme="dark"] body`; the override must out-rank it.
    expect(readWeb("src/design/orbit/css/navy-night.css")).toContain('html[data-theme="dark"] body{');
    expect(declarations(base, ':root[data-theme="dark"] body')["background-image"]).toBe("none");
  });

  it("the skip link has no shadow and is clipped until focused", () => {
    const app = topLevel(readWeb("src/app/app.css"));
    const skip = declarations(app, ".tc-skip-link");
    expect(skip["box-shadow"]).toBeUndefined();
    expect(skip.transform).toBeUndefined();
    expect(skip["clip-path"]).toBe("inset(50%)");
    expect(declarations(app, ".tc-skip-link:focus")["clip-path"]).toBe("none");
  });

  it("the fullscreen session stage uses the stage token", () => {
    const session = topLevel(readWeb("src/session/session.css"));
    expect(declarations(session, ".tc-session__stage:fullscreen").background).toBe("var(--tc-color-stage)");
  });
});
