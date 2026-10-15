import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Button, buttonClass } from "../../../src/design/Button";

describe("design/Button", () => {
  it("danger-ghost is a variant with its own class", () => {
    render(<Button variant="danger-ghost">Purge</Button>);
    expect(screen.getByRole("button", { name: "Purge" })).toHaveClass(
      "tc-button",
      "tc-button--danger-ghost",
      "tc-button--md",
    );
    expect(buttonClass("danger-ghost", "sm")).toBe("tc-button tc-button--danger-ghost tc-button--sm");
  });
});
