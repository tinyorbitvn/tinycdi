import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { ComponentType } from "react";
import * as navIcons from "../../../src/design/nav-icons";
import type { IconProps } from "../../../src/design/icons";

// The sidebar duotone set (Orbit 2.9 "Dashboard icons") shares the outline
// icons' svg + aria contract, plus a 0.16 currentColor fill layer that
// makes the style duotone.
const exported = Object.entries(navIcons).filter(
  ([name, v]) => name.startsWith("NavIcon") && typeof v === "function",
) as Array<[string, ComponentType<IconProps>]>;

describe("design/nav-icons", () => {
  it("exports the sidebar set", () => {
    expect(exported.length).toBeGreaterThanOrEqual(4);
  });

  it.each(exported)("%s renders the shared svg contract", (_name, Icon) => {
    const { container } = render(<Icon />);
    const svg = container.querySelector("svg");
    expect(svg).not.toBeNull();
    expect(svg).toHaveAttribute("viewBox", "0 0 24 24");
    expect(svg).toHaveAttribute("fill", "none");
    expect(svg).toHaveAttribute("stroke", "currentColor");
    expect(svg).toHaveAttribute("stroke-width", "1.75");
    expect(svg).toHaveAttribute("aria-hidden", "true");
    expect(svg).not.toHaveAttribute("role");
    expect(svg!.querySelector("title")).toBeNull();
  });

  it.each(exported)("%s carries a 0.16-opacity fill layer (duotone)", (_name, Icon) => {
    const { container } = render(<Icon />);
    const layer = container.querySelector('[fill-opacity="0.16"]');
    expect(layer).not.toBeNull();
    expect(layer).toHaveAttribute("fill", "currentColor");
    expect(layer).toHaveAttribute("stroke", "none");
  });

  it("a titled icon exposes role=img, aria-label and a <title>", () => {
    const { container } = render(<navIcons.NavIconVps title="workspaces" />);
    const svg = container.querySelector("svg");
    expect(svg).toHaveAttribute("role", "img");
    expect(svg).toHaveAttribute("aria-label", "workspaces");
    expect(svg).not.toHaveAttribute("aria-hidden");
    expect(svg!.querySelector("title")?.textContent).toBe("workspaces");
  });
});
