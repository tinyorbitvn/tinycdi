import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import {
  clampPage,
  pageCount,
  pageItems,
  Pagination,
  usePagination,
} from "../../../src/design/Pagination";

// Page-strip math: pages<=7 shows every number; wider ranges keep
// first/last plus a window around the current page with ellipsis gaps.
describe("design/Pagination math", () => {
  it("pageCount clamps to at least one page", () => {
    expect(pageCount(0, 50)).toBe(1);
    expect(pageCount(1, 50)).toBe(1);
    expect(pageCount(50, 50)).toBe(1);
    expect(pageCount(51, 50)).toBe(2);
    expect(pageCount(137, 50)).toBe(3);
  });

  it("clampPage keeps a page inside [1, pages]", () => {
    expect(clampPage(1, 4)).toBe(1);
    expect(clampPage(9, 4)).toBe(4);
    expect(clampPage(-3, 4)).toBe(1);
    expect(clampPage(0, 1)).toBe(1);
  });

  it("pageItems lists every page when 7 or fewer fit", () => {
    expect(pageItems(1, 7)).toEqual([1, 2, 3, 4, 5, 6, 7]);
    expect(pageItems(7, 7)).toEqual([1, 2, 3, 4, 5, 6, 7]);
  });

  it("pageItems ellipsizes the tail at the start of a long range", () => {
    expect(pageItems(1, 20)).toEqual([1, 2, "ellipsis", 20]);
    expect(pageItems(2, 20)).toEqual([1, 2, 3, "ellipsis", 20]);
  });

  it("pageItems ellipsizes both sides mid-range", () => {
    expect(pageItems(10, 20)).toEqual([1, "ellipsis", 9, 10, 11, "ellipsis", 20]);
  });

  it("pageItems ellipsizes the head near the end", () => {
    expect(pageItems(20, 20)).toEqual([1, "ellipsis", 19, 20]);
    expect(pageItems(19, 20)).toEqual([1, "ellipsis", 18, 19, 20]);
  });

  it("pageItems prefers the single missing page over a one-page gap", () => {
    expect(pageItems(4, 9)).toEqual([1, 2, 3, 4, 5, "ellipsis", 9]);
    expect(pageItems(6, 9)).toEqual([1, "ellipsis", 5, 6, 7, 8, 9]);
  });

  it("pageItems clamps an out-of-range current page", () => {
    expect(pageItems(25, 20)).toEqual([1, "ellipsis", 19, 20]);
    expect(pageItems(-2, 20)).toEqual([1, 2, "ellipsis", 20]);
  });
});

describe("design/Pagination component", () => {
  it("renders a named nav landmark with the status text", () => {
    render(
      <Pagination
        total={137}
        page={2}
        pageSize={50}
        onPageChange={() => {}}
        label="Tenant workspaces pages"
      />,
    );
    const nav = screen.getByRole("navigation", { name: "Tenant workspaces pages" });
    expect(within(nav).getByText("Showing 51–100 of 137")).toBeInTheDocument();
  });

  it("marks the current page with aria-current and disables the ends", () => {
    render(<Pagination total={30} page={1} pageSize={10} onPageChange={() => {}} label="Rows" />);
    expect(screen.getByRole("button", { name: "Page 1" })).toHaveAttribute(
      "aria-current",
      "page",
    );
    expect(screen.getByRole("button", { name: "Page 3" })).not.toHaveAttribute("aria-current");
    expect(screen.getByRole("button", { name: "Previous" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Next" })).toBeEnabled();
  });

  it("fires onPageChange for next, previous and numbered pages", () => {
    const onPageChange = vi.fn();
    render(<Pagination total={300} page={2} pageSize={50} onPageChange={onPageChange} label="Rows" />);
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(onPageChange).toHaveBeenLastCalledWith(3);
    fireEvent.click(screen.getByRole("button", { name: "Previous" }));
    expect(onPageChange).toHaveBeenLastCalledWith(1);
    fireEvent.click(screen.getByRole("button", { name: "Page 6" }));
    expect(onPageChange).toHaveBeenLastCalledWith(6);
  });

  it("offers the page-size select and resets through onPageSizeChange", () => {
    const onPageSizeChange = vi.fn();
    render(
      <Pagination
        total={300}
        page={2}
        pageSize={50}
        onPageChange={() => {}}
        pageSizes={[25, 50, 100]}
        onPageSizeChange={onPageSizeChange}
        label="Rows"
      />,
    );
    const select = screen.getByRole("combobox", { name: "Rows per page" });
    expect(select).toHaveValue("50");
    fireEvent.change(select, { target: { value: "25" } });
    expect(onPageSizeChange).toHaveBeenCalledWith(25);
  });

  it("hides the page strip on a single page but keeps the status", () => {
    render(<Pagination total={3} page={1} pageSize={50} onPageChange={() => {}} label="Rows" />);
    expect(screen.queryByRole("button", { name: "Next" })).not.toBeInTheDocument();
    expect(screen.getByText("Showing 1–3 of 3")).toBeInTheDocument();
  });

  it("renders nothing when there are no rows", () => {
    const { container } = render(
      <Pagination total={0} page={1} pageSize={50} onPageChange={() => {}} label="Rows" />,
    );
    expect(container).toBeEmptyDOMElement();
  });
});

// The hook: slices rows for the current page, resets to page 1 when the
// resetKey changes (filters), and clamps the page when rows shrink.
describe("design/usePagination", () => {
  function Harness({ rows, filter }: { rows: number[]; filter: string }) {
    const pg = usePagination(rows, { pageSize: 10, resetKey: filter });
    return (
      <Pagination {...pg.props} pageSizes={[10, 25]} label="Items" />
    );
  }

  it("slices the requested page and changes page size", () => {
    const rows = Array.from({ length: 30 }, (_, i) => i);
    render(<Harness rows={rows} filter="" />);
    expect(screen.getByText("Showing 1–10 of 30")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Page 2" }));
    expect(screen.getByText("Showing 11–20 of 30")).toBeInTheDocument();
    fireEvent.change(screen.getByRole("combobox", { name: "Rows per page" }), {
      target: { value: "25" },
    });
    expect(screen.getByText("Showing 1–25 of 30")).toBeInTheDocument();
  });

  it("resets to page 1 when the resetKey changes", () => {
    const rows = Array.from({ length: 30 }, (_, i) => i);
    const { rerender } = render(<Harness rows={rows} filter="" />);
    fireEvent.click(screen.getByRole("button", { name: "Page 3" }));
    expect(screen.getByText("Showing 21–30 of 30")).toBeInTheDocument();
    rerender(<Harness rows={rows.slice(0, 5)} filter="abc" />);
    expect(screen.getByText("Showing 1–5 of 5")).toBeInTheDocument();
  });

  it("clamps the current page when rows shrink under it", () => {
    const rows = Array.from({ length: 30 }, (_, i) => i);
    const { rerender } = render(<Harness rows={rows} filter="k" />);
    fireEvent.click(screen.getByRole("button", { name: "Page 3" }));
    expect(screen.getByText("Showing 21–30 of 30")).toBeInTheDocument();
    // Same filter (no reset): the list shrinking to 12 rows pulls page 3
    // back to the new last page instead of stranding the view.
    rerender(<Harness rows={rows.slice(0, 12)} filter="k" />);
    expect(screen.getByText("Showing 11–12 of 12")).toBeInTheDocument();
  });
});
