import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { Tabs } from "../../../src/design/Tabs";

const tabs = [
  { id: "a", label: "Alpha" },
  { id: "b", label: "Beta" },
  { id: "c", label: "Gamma", disabled: true },
  { id: "d", label: "Delta" },
];

// WAI-ARIA tabs: roving tabindex, arrow keys move+activate, disabled tabs
// are skipped; the admin section switcher relies on this contract.
describe("design/Tabs", () => {
  it("gives the selected tab tabindex 0 and the rest -1", () => {
    render(<Tabs label="sections" tabs={tabs} defaultValue="a" />);
    expect(screen.getByRole("tab", { name: "Alpha" })).toHaveAttribute("tabindex", "0");
    expect(screen.getByRole("tab", { name: "Beta" })).toHaveAttribute("tabindex", "-1");
    expect(screen.getByRole("tab", { name: "Delta" })).toHaveAttribute("tabindex", "-1");
  });

  it("arrow keys move selection and focus, skipping disabled tabs", () => {
    const onChange = vi.fn();
    render(<Tabs label="sections" tabs={tabs} defaultValue="a" onChange={onChange} />);
    const tablist = screen.getByRole("tablist", { name: "sections" });
    screen.getByRole("tab", { name: "Alpha" }).focus();

    fireEvent.keyDown(tablist, { key: "ArrowRight" });
    expect(onChange).toHaveBeenCalledWith("b");
    expect(screen.getByRole("tab", { name: "Beta" })).toHaveFocus();
    expect(screen.getByRole("tab", { name: "Beta" })).toHaveAttribute("aria-selected", "true");

    // Gamma is disabled: the next ArrowRight lands on Delta.
    fireEvent.keyDown(tablist, { key: "ArrowRight" });
    expect(onChange).toHaveBeenLastCalledWith("d");
    expect(screen.getByRole("tab", { name: "Delta" })).toHaveFocus();

    // Wrap-around forward and back.
    fireEvent.keyDown(tablist, { key: "ArrowRight" });
    expect(screen.getByRole("tab", { name: "Alpha" })).toHaveFocus();
    fireEvent.keyDown(tablist, { key: "ArrowLeft" });
    expect(screen.getByRole("tab", { name: "Delta" })).toHaveFocus();
  });

  it("Home and End jump to the first/last enabled tab", () => {
    render(<Tabs label="sections" tabs={tabs} defaultValue="b" />);
    const tablist = screen.getByRole("tablist", { name: "sections" });
    screen.getByRole("tab", { name: "Beta" }).focus();
    fireEvent.keyDown(tablist, { key: "End" });
    expect(screen.getByRole("tab", { name: "Delta" })).toHaveFocus();
    fireEvent.keyDown(tablist, { key: "Home" });
    expect(screen.getByRole("tab", { name: "Alpha" })).toHaveFocus();
  });

  it("sets aria-controls only when the tab renders a panel", () => {
    render(
      <Tabs
        label="sections"
        tabs={[
          { id: "p", label: "Panelled", content: <div>panel body</div> },
          { id: "n", label: "Nav" },
        ]}
        defaultValue="p"
      />,
    );
    const panelled = screen.getByRole("tab", { name: "Panelled" });
    const panel = screen.getByRole("tabpanel");
    expect(panelled).toHaveAttribute("aria-controls", panel.id);
    expect(screen.getByRole("tab", { name: "Nav" })).not.toHaveAttribute("aria-controls");
  });
});

describe("design/Tabs scroll into view", () => {
  it("scrolls the selected tab into view without smooth behaviour (reduced motion safe)", () => {
    const seen: Element[] = [];
    const spy = vi.fn(function (this: Element, _arg?: boolean | ScrollIntoViewOptions) {
      seen.push(this);
    });
    const original = HTMLElement.prototype.scrollIntoView;
    HTMLElement.prototype.scrollIntoView = spy as unknown as HTMLElement["scrollIntoView"];
    try {
      render(<Tabs tabs={tabs} label="Sections" defaultValue="d" />);
      expect(spy).toHaveBeenCalledWith({ block: "nearest", inline: "nearest" });
      expect(seen[0]).toBe(screen.getByRole("tab", { name: "Delta" }));

      fireEvent.keyDown(screen.getByRole("tab", { name: "Delta" }), { key: "Home" });
      expect(seen[seen.length - 1]).toBe(screen.getByRole("tab", { name: "Alpha" }));
      for (const call of spy.mock.calls) expect(call[0]).not.toHaveProperty("behavior");
    } finally {
      HTMLElement.prototype.scrollIntoView = original;
    }
  });
});
