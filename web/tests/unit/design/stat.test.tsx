import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Stat, StatList } from "../../../src/design/Stat";
import { declarations, readWeb, topLevel } from "./css";

describe("design/Stat", () => {
  it("is a group named by its label, with the label above the value", () => {
    render(<Stat label="Live sessions" value={2} hint="last 24 h" />);
    const group = screen.getByRole("group", { name: "Live sessions" });
    const [label, value, hint] = Array.from(group.children);
    expect(label).toHaveClass("tc-stat__label");
    expect(value).toHaveClass("tc-stat__value");
    expect(value).toHaveTextContent("2");
    expect(hint).toHaveClass("tc-stat__hint");
    expect(hint).toHaveTextContent("last 24 h");
  });

  it("renders zero and a tone class", () => {
    render(<Stat label="Stuck deletions" value={0} tone="warning" />);
    const group = screen.getByRole("group", { name: "Stuck deletions" });
    expect(group).toHaveClass("tc-stat", "tc-stat--warning");
    expect(within(group).getByText("0")).toHaveClass("tc-stat__value");
  });

  it("renders an action below the value", () => {
    render(<Stat label="Stuck" value={3} action={<a href="/admin/workspaces">Review</a>} />);
    const group = screen.getByRole("group", { name: "Stuck" });
    expect(within(group).getByRole("link", { name: "Review" }).parentElement).toHaveClass("tc-stat__action");
  });

  it("StatList renders key/value rows, or its empty text", () => {
    const { rerender } = render(
      <StatList label="Running by template" items={[{ key: "a", term: "chrome-r1", value: 2 }]} empty="Nothing running" />,
    );
    const group = screen.getByRole("group", { name: "Running by template" });
    const term = within(group).getByText("chrome-r1");
    expect(term.tagName).toBe("DT");
    expect(term.nextElementSibling).toHaveTextContent("2");
    rerender(<StatList label="Running by template" items={[]} empty="Nothing running" />);
    expect(screen.getByText("Nothing running")).toHaveClass("tc-stat__hint");
  });

  it("uses the spec type scale: label 12px/600 muted, value 28px/600, hint 12px", () => {
    const css = topLevel(readWeb("src/design/design.css"));
    expect(declarations(css, ".tc-stat__label")).toMatchObject({
      "font-size": "var(--tc-text-xs)",
      "font-weight": "var(--tc-weight-semibold)",
      color: "var(--tc-color-text-muted)",
    });
    expect(declarations(css, ".tc-stat__value")).toMatchObject({
      "font-size": "var(--tc-text-2xl)",
      "font-weight": "var(--tc-weight-semibold)",
    });
    expect(declarations(css, ".tc-stat__hint")["font-size"]).toBe("var(--tc-text-xs)");
  });
});
