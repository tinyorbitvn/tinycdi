import { fireEvent, render, screen } from "@testing-library/react";
import { createRef } from "react";
import { describe, expect, it, vi } from "vitest";
import { Form, FormActions } from "../../../src/design/Form";
import { declarations, mediaBlocks, readWeb, topLevel } from "./css";

describe("design/Form", () => {
  it("renders a grid form, forwards the ref and submits from FormActions", () => {
    const onSubmit = vi.fn((e: { preventDefault: () => void }) => e.preventDefault());
    const ref = createRef<HTMLFormElement>();
    render(
      <Form ref={ref} aria-label="filters" onSubmit={onSubmit}>
        <FormActions>
          <button type="submit">Apply</button>
        </FormActions>
      </Form>,
    );
    const form = screen.getByRole("form", { name: "filters" });
    expect(form).toHaveClass("tc-form");
    expect(form).not.toHaveClass("tc-form--2col");
    expect(ref.current).toBe(form);
    const apply = screen.getByRole("button", { name: "Apply" });
    expect(apply.parentElement).toHaveClass("tc-form__actions");
    fireEvent.click(apply);
    expect(onSubmit).toHaveBeenCalledOnce();
  });

  it("columns={2} adds the two-column class", () => {
    render(<Form aria-label="policy" columns={2} />);
    expect(screen.getByRole("form", { name: "policy" })).toHaveClass("tc-form", "tc-form--2col");
  });

  it("spaces fields by space-6 and fieldsets by space-4; two columns from 768px", () => {
    const raw = readWeb("src/design/design.css");
    const css = topLevel(raw);
    expect(declarations(css, ".tc-form").gap).toBe("var(--tc-space-6)");
    expect(declarations(css, ".tc-form").display).toBe("grid");
    expect(declarations(css, ".tc-form fieldset").gap).toBe("var(--tc-space-4)");
    expect(declarations(css, ".tc-form__actions")["flex-wrap"]).toBe("wrap");
    const wide = mediaBlocks(raw, "(min-width: 48rem)").join("\n");
    expect(declarations(wide, ".tc-form--2col")["grid-template-columns"]).toBe("repeat(2, minmax(0, 1fr))");
    expect(declarations(wide, ".tc-form--2col > .tc-form__actions")["grid-column"]).toBe("1 / -1");
    expect(declarations(wide, ".tc-form--2col > .tc-alert")["grid-column"]).toBe("1 / -1");
  });
});
