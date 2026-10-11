import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";
import type { ComponentType } from "react";
import * as icons from "../../../src/design/icons";
import type { IconProps } from "../../../src/design/icons";

// The portal icon set is Tabler Icons 3.48.0 outline artwork at the Orbit
// 2.9 manifest names. Every export must share the same svg contract: 24×24
// viewBox, 1.75 round strokes, currentColor — and the aria contract
// (decorative unless titled). The PINNED table locks a sample of `d`
// strings to the upstream artwork so hand-drawn geometry cannot come back.

const exported = Object.entries(icons).filter(
  ([name, v]) => name.startsWith("Icon") && typeof v === "function",
) as Array<[string, ComponentType<IconProps>]>;

describe("design/icons", () => {
  it("exports a non-empty set", () => {
    expect(exported.length).toBeGreaterThan(20);
  });

  it.each(exported)("%s renders the shared svg contract", (_name, Icon) => {
    const { container } = render(<Icon />);
    const svg = container.querySelector("svg");
    expect(svg).not.toBeNull();
    expect(svg).toHaveAttribute("viewBox", "0 0 24 24");
    expect(svg).toHaveAttribute("fill", "none");
    expect(svg).toHaveAttribute("stroke", "currentColor");
    expect(svg).toHaveAttribute("stroke-width", "1.75");
    expect(svg).toHaveAttribute("stroke-linecap", "round");
    expect(svg).toHaveAttribute("stroke-linejoin", "round");
    expect(svg).toHaveAttribute("aria-hidden", "true");
    expect(svg).not.toHaveAttribute("role");
    expect(svg!.querySelector("title")).toBeNull();
  });

  it("a titled icon exposes role=img, aria-label and a <title>", () => {
    const { container } = render(<icons.IconCheck title="done" />);
    const svg = container.querySelector("svg");
    expect(svg).toHaveAttribute("role", "img");
    expect(svg).toHaveAttribute("aria-label", "done");
    expect(svg).not.toHaveAttribute("aria-hidden");
    expect(svg!.querySelector("title")?.textContent).toBe("done");
  });

  // Exact Tabler 3.48.0 outline geometry (icons/outline/<name>.svg in the
  // published tarball) — pinned so the artwork cannot drift or regress to
  // the old hand-drawn set.
  const PINNED: Array<[ComponentType<IconProps>, string[]]> = [
    [icons.IconTriangleAlert, [
      "M12 9v4",
      "M10.363 3.591l-8.106 13.534a1.914 1.914 0 0 0 1.636 2.871h16.214a1.914 1.914 0 0 0 1.636 -2.87l-8.106 -13.536a1.914 1.914 0 0 0 -3.274 0",
      "M12 16h.01",
    ]],
    [icons.IconWorld, [
      "M3 12a9 9 0 1 0 18 0a9 9 0 0 0 -18 0",
      "M3.6 9h16.8",
      "M3.6 15h16.8",
      "M11.5 3a17 17 0 0 0 0 18",
      "M12.5 3a17 17 0 0 1 0 18",
    ]],
    [icons.IconPlayerPlay, ["M7 4v16l13 -8l-13 -8"]],
    [icons.IconCircleCheck, [
      "M3 12a9 9 0 1 0 18 0a9 9 0 1 0 -18 0",
      "M9 12l2 2l4 -4",
    ]],
    [icons.IconDots, [
      "M4 12a1 1 0 1 0 2 0a1 1 0 1 0 -2 0",
      "M11 12a1 1 0 1 0 2 0a1 1 0 1 0 -2 0",
      "M18 12a1 1 0 1 0 2 0a1 1 0 1 0 -2 0",
    ]],
    [icons.IconShieldCheck, [
      "M11.46 20.846a12 12 0 0 1 -7.96 -14.846a12 12 0 0 0 8.5 -3a12 12 0 0 0 8.5 3a12 12 0 0 1 -.09 7.06",
      "M15 19l2 2l4 -4",
    ]],
  ];

  it.each(PINNED)("%s keeps verbatim Tabler 3.48.0 geometry", (Icon, paths) => {
    const { container } = render(<Icon />);
    const rendered = [...container.querySelectorAll("svg path")].map((p) =>
      p.getAttribute("d"),
    );
    expect(rendered).toEqual(paths);
  });
});
