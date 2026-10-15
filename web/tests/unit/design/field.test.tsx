import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { Input } from "../../../src/design/Field";
import { IconSearch } from "../../../src/design/icons";

// Toolbar/filter language: a leading search icon plus a clear affordance
// that keeps focus in the field (UX-8).
describe("design/Field Input affixes", () => {
  it("renders the leading icon as a decorative ornament", () => {
    const { container } = render(<Input label="Filter" icon={<IconSearch />} />);
    const ornament = container.querySelector(".tc-input-affixed__icon");
    expect(ornament).not.toBeNull();
    expect(ornament).toHaveAttribute("aria-hidden", "true");
    expect(ornament!.querySelector("svg")).not.toBeNull();
  });

  it("shows the clear button only while the input has a value", () => {
    const { rerender } = render(
      <Input label="Filter" value="" onChange={() => {}} onClear={() => {}} />,
    );
    expect(screen.queryByRole("button", { name: "Clear" })).not.toBeInTheDocument();
    rerender(<Input label="Filter" value="abc" onChange={() => {}} onClear={() => {}} />);
    expect(screen.getByRole("button", { name: "Clear" })).toBeInTheDocument();
  });

  it("clear calls onClear and keeps focus in the input", () => {
    const onClear = vi.fn();
    render(<Input label="Filter" value="abc" onChange={() => {}} onClear={onClear} />);
    fireEvent.click(screen.getByRole("button", { name: "Clear" }));
    expect(onClear).toHaveBeenCalledOnce();
    expect(screen.getByLabelText("Filter")).toHaveFocus();
  });

  it("tracks emptiness on uncontrolled inputs too", () => {
    render(<Input label="Filter" onClear={() => {}} />);
    expect(screen.queryByRole("button", { name: "Clear" })).not.toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Filter"), { target: { value: "x" } });
    expect(screen.getByRole("button", { name: "Clear" })).toBeInTheDocument();
  });
});
