import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Page } from "../../../src/design/Layout";
import { declarations, readWeb, topLevel } from "./css";

describe("design/Page", () => {
  it("renders back as a chevron link in the eyebrow slot", () => {
    render(<Page title="Detail" back={{ to: "/data", label: "All data" }} />);
    const link = screen.getByRole("link", { name: "All data" });
    expect(link).toHaveAttribute("href", "/data");
    expect(link).toHaveClass("tc-backlink");
    expect(link.querySelector("svg")).not.toBeNull();
    expect(link.closest(".tc-page__eyebrow")).not.toBeNull();
  });

  it("keeps the eyebrow slot without back or eyebrow (constant header height)", () => {
    const { container } = render(<Page title="Plain" />);
    expect(container.querySelector(".tc-page__header > .tc-page__eyebrow")).not.toBeNull();
  });

  it("puts the actions on the title row", () => {
    render(<Page title="With actions" actions={<button type="button">Go</button>} />);
    const row = screen.getByRole("heading", { level: 1, name: "With actions" }).parentElement!;
    expect(row).toHaveClass("tc-page__titlerow");
    expect(within(row).getByRole("button", { name: "Go" })).toBeInTheDocument();
  });

  it("shows back and eyebrow text together", () => {
    render(<Page title="T" back={{ to: "/admin/users", label: "Back to users" }} eyebrow="Tenant acme" />);
    expect(screen.getByRole("link", { name: "Back to users" })).toBeInTheDocument();
    expect(screen.getByText("Tenant acme")).toHaveClass("tc-page__context");
  });

  it("aligns actions to the title baseline and caps description width at 72ch", () => {
    const css = topLevel(readWeb("src/design/design.css"));
    expect(declarations(css, ".tc-page__titlerow")["align-items"]).toBe("baseline");
    expect(declarations(css, ".tc-page__description")["max-width"]).toBe("72ch");
    expect(declarations(css, ".tc-section__description")["max-width"]).toBe("72ch");
    expect(declarations(css, ".tc-page__eyebrow")["min-height"]).toBe("var(--tc-space-6)");
  });
});
