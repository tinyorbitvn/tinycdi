import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { StatusPill } from "../../../src/design/Badge";
import { Table } from "../../../src/design/Table";
import { declarations, mediaBlocks, readWeb, topLevel } from "./css";

const design = () => topLevel(readWeb("src/design/design.css"));

describe("badge (Orbit tag metrics)", () => {
  it("never stretches and uses tag metrics with a tone border", () => {
    const css = design();
    const b = declarations(css, ".tc-badge");
    expect(b).toMatchObject({
      width: "max-content",
      "border-radius": "var(--tc-radius-sm)",
      "font-size": "var(--tc-text-2xs)",
      "font-weight": "var(--tc-weight-semibold)",
      padding: "var(--tc-space-1) var(--tc-space-2)",
      border: "1px solid var(--tc-badge-border)",
    });
    expect(b["min-height"]).toBeUndefined();
    expect(declarations(css, ".tc-status-pill")["border-radius"]).toBeUndefined();
    for (const tone of ["info", "success", "warning", "danger"]) {
      expect(declarations(css, `.tc-badge--${tone}`)["--tc-badge-border"], tone).toBe(
        `var(--tc-color-${tone}-border)`,
      );
    }
  });

  it("a status pill is an inline badge span (structure the CSS relies on)", () => {
    render(<StatusPill phase="Ready" />);
    const pill = screen.getByText("Ready").closest(".tc-badge")!;
    expect(pill.tagName).toBe("SPAN");
    expect(pill).toHaveClass("tc-badge", "tc-badge--success", "tc-status-pill");
  });
});

describe("table polish", () => {
  it("has no first-column seam, Orbit header type and inherited sort-button case", () => {
    const css = design();
    expect(declarations(css, ".tc-table tbody th").background).toBe("transparent");
    expect(declarations(css, ".tc-table thead th")).toMatchObject({
      background: "var(--tc-color-table-head)",
      "font-size": "var(--tc-text-2xs)",
      "font-weight": "var(--tc-weight-semibold)",
      "text-transform": "uppercase",
    });
    expect(declarations(css, ".tc-table__sort")).toMatchObject({
      "text-transform": "inherit",
      "letter-spacing": "inherit",
    });
    expect(declarations(css, ".tc-table")["font-size"]).toBe("var(--tc-text-sm)");
    expect(declarations(css, ".tc-table__sub")).toMatchObject({
      "font-size": "var(--tc-text-xs)",
      color: "var(--tc-color-text-muted)",
    });
  });

  it("tints hovered rows and keeps cell links plain until hover/focus", () => {
    const css = design();
    const hover = ".tc-table tbody tr:not(.tc-table__empty-row):not(.tc-table__skeleton-row):hover";
    expect(declarations(css, `${hover} > *`).background).toBe("var(--tc-color-row-hover)");
    // Empty-state and skeleton rows never tint.
    expect(declarations(css, ".tc-table tbody tr:hover > *")).toEqual({});
    expect(declarations(css, ".tc-table a")["text-decoration"]).toBe("none");
    expect(declarations(css, ".tc-table a:hover")["text-decoration"]).toBe("underline");
    expect(declarations(css, ".tc-table a:focus-visible")["text-decoration"]).toBe("underline");
  });

  it("empty-state and skeleton rows carry the classes the hover rule skips", () => {
    const cols = [{ key: "id", header: "ID" }];
    const { container, rerender } = render(
      <Table caption="rows" columns={cols} rows={[] as { id: string }[]} rowKey={(r) => r.id} />,
    );
    expect(container.querySelector("tbody tr")).toHaveClass("tc-table__empty-row");
    rerender(<Table caption="rows" columns={cols} rows={[] as { id: string }[]} rowKey={(r) => r.id} loading />);
    for (const tr of container.querySelectorAll("tbody tr")) expect(tr).toHaveClass("tc-table__skeleton-row");
  });

  it("a stacked row tints as one block, not cell by cell", () => {
    const narrow = mediaBlocks(readWeb("src/design/design.css"), "(max-width: 47.99rem)").join("\n");
    const hover = ".tc-table--stack-md tbody tr:not(.tc-table__empty-row):not(.tc-table__skeleton-row):hover";
    expect(declarations(narrow, hover).background).toBe("var(--tc-color-row-hover)");
    expect(declarations(narrow, `${hover} > *`).background).toBe("transparent");
  });

  it("an empty table hides its header and drops its frame", () => {
    const css = design();
    expect(declarations(css, ".tc-table--empty thead").display).toBe("none");
    expect(declarations(css, ".tc-table-wrap--empty")).toMatchObject({ border: "0", background: "transparent" });
  });
});

describe("links, dialog, empty state, tabs", () => {
  it("links are underlined only in prose", () => {
    const base = topLevel(readWeb("src/styles/base.css"));
    expect(declarations(base, "a")["text-decoration"]).toBe("none");
    for (const sel of [".tc-prose a", ".tc-alert a", "p a", ".tc-page__description a", ".tc-section__description a"]) {
      expect(declarations(base, sel)["text-decoration"], sel).toBe("underline");
    }
  });

  it("the dialog scrim is a translucent navy and dark panels get a visible border", () => {
    const css = design();
    expect(declarations(css, ".tc-overlay__backdrop").background).toBe(
      "color-mix(in srgb, var(--tc-color-overlay) 55%, transparent)",
    );
    expect(declarations(css, '[data-theme="dark"] .tc-overlay__panel')["border-color"]).toBe(
      "var(--tc-color-border-strong)",
    );
  });

  it("empty states use a solid subtle border", () => {
    expect(declarations(design(), ".tc-empty").border).toBe("1px solid var(--tc-color-border)");
  });

  it("tabs snap and fade at the edges on narrow screens", () => {
    const raw = readWeb("src/design/design.css");
    expect(declarations(design(), ".tc-tabs__list")["scroll-snap-type"]).toBe("x proximity");
    expect(declarations(design(), ".tc-tabs__tab")["scroll-snap-align"]).toBe("start");
    const narrow = mediaBlocks(raw, "(max-width: 47.99rem)").join("\n");
    expect(declarations(narrow, ".tc-tabs__list")["mask-image"]).toContain("currentColor");
  });
});

describe("catalog cards", () => {
  it("align the dl term column across rows and stretch to equal height", () => {
    const css = topLevel(readWeb("src/design/design.css"));
    expect(declarations(css, ".tc-dl--fit")["grid-template-columns"]).toBe("minmax(6rem, auto) minmax(0, 1fr)");
    expect(declarations(css, ".tc-dl--fit .tc-dl__row")["grid-template-columns"]).toBe("subgrid");
    expect(declarations(css, ".tc-grid > .tc-card").height).toBe("100%");
  });
});
