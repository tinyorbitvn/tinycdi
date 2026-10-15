import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { ComponentType } from "react";
import * as navIcons from "../../../src/design/nav-icons";
import type { IconProps } from "../../../src/design/icons";
import { ApiProvider } from "../../../src/api/context";
import { AppShell, BrandingProvider } from "../../../src/app/shell";
import { MeProvider, type Me } from "../../../src/app/me";
import { ThemeProvider } from "../../../src/app/theme";
import { LocaleProvider } from "../../../src/app/locale";
import { ToastProvider } from "../../../src/design";
import { DEFAULT_BRANDING } from "../../../src/app/branding";
import type { RouteArea } from "../../../src/app/routes";

// The sidebar duotone set (Orbit 2.9 "Dashboard icons") shares the outline
// icons' svg + aria contract, plus a low-opacity currentColor underlay that
// makes the style duotone: a 0.16 fill layer on most glyphs, a 0.15 thick
// stroke under the "attachment" outline — exactly as the source artwork.
const exported = Object.entries(navIcons).filter(
  ([name, v]) => name.startsWith("NavIcon") && typeof v === "function",
) as Array<[string, ComponentType<IconProps>]>;

// Glyphs whose underlay is a 0.16 fill layer — "attachment" is the one
// exception (stroked underlay, pinned below).
const fillUnderlay = exported.filter(([name]) => name !== "NavIconAttachment");

// Geometry pinned byte-exact to the design-system source (Orbit 2.9
// Dashboard duotone "attachment", fetched 2026-10-12). The source's
// role/aria-label/title are stripped — the wrapper owns the aria contract.
const ATTACHMENT_UNDERLAY_D = "m7 13 7-7a5 5 0 0 1 7 7l-9 9";
const ATTACHMENT_OUTLINE_D =
  "m8 13 6-6a3 3 0 0 1 4 4l-8 8a5 5 0 0 1-7-7l9-9a2 2 0 0 1 3 3l-9 9a1 1 0 0 0 2 2l8-8";

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

  it.each(fillUnderlay)("%s carries a 0.16-opacity fill layer (duotone)", (_name, Icon) => {
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

  it("NavIconAttachment matches the source duotone geometry byte-exact", () => {
    const { container } = render(<navIcons.NavIconAttachment />);
    const paths = Array.from(container.querySelectorAll("path"));
    expect(paths.map((p) => p.getAttribute("d"))).toEqual([
      ATTACHMENT_UNDERLAY_D,
      ATTACHMENT_OUTLINE_D,
    ]);
    const underlay = paths[0];
    expect(underlay).toHaveAttribute("stroke", "currentColor");
    expect(underlay).toHaveAttribute("stroke-width", "5");
    expect(underlay).toHaveAttribute("opacity", "0.15");
  });

  it("the /data nav item renders the attachment glyph", async () => {
    const areas: RouteArea[] = [
      {
        name: "workspaces",
        owns: () => true,
        load: async () => ({
          routes: [{ path: "/*", title: "Home", render: () => <div /> }],
        }),
      },
    ];
    const me: Me = { subject: "u1", displayName: "Ada Admin", tenant: "acme", roles: [] };
    render(
      <ApiProvider>
        <ThemeProvider>
          <BrandingProvider load={async () => DEFAULT_BRANDING}>
            <LocaleProvider>
              <ToastProvider>
                <MeProvider load={async () => me}>
                  <AppShell areas={areas} />
                </MeProvider>
              </ToastProvider>
            </LocaleProvider>
          </BrandingProvider>
        </ThemeProvider>
      </ApiProvider>,
    );
    const link = await screen.findByRole("link", { name: "Retained data" });
    expect(link).toHaveAttribute("href", "/data");
    const ds = Array.from(link.querySelectorAll("path")).map((p) => p.getAttribute("d"));
    expect(ds).toEqual([ATTACHMENT_UNDERLAY_D, ATTACHMENT_OUTLINE_D]);
  });
});
