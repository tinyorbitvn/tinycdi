import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { Pagination } from "../../../src/design/Pagination";

// The footer of a server-paged table: a count of loaded rows and, while the
// server reports another page, a "Load more" control.
describe("design/Pagination (load more)", () => {
  it("renders a named nav landmark with the loaded count and Load more", () => {
    const onLoadMore = vi.fn();
    render(<Pagination loaded={50} hasNext onLoadMore={onLoadMore} label="Rows" />);
    const nav = screen.getByRole("navigation", { name: "Rows" });
    expect(within(nav).getByText("Showing the first 50")).toBeInTheDocument();
    fireEvent.click(within(nav).getByRole("button", { name: "Load more" }));
    expect(onLoadMore).toHaveBeenCalledTimes(1);
  });

  it("drops the control on the last page", () => {
    render(<Pagination loaded={7} hasNext={false} onLoadMore={() => {}} label="Rows" />);
    expect(screen.getByText("Showing all 7")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Load more" })).not.toBeInTheDocument();
  });

  it("disables Load more while a page is in flight", () => {
    render(<Pagination loaded={50} hasNext loading onLoadMore={() => {}} label="Rows" />);
    expect(screen.getByRole("button", { name: "Load more" })).toBeDisabled();
  });

  it("renders nothing for an empty, complete list", () => {
    const { container } = render(<Pagination loaded={0} hasNext={false} onLoadMore={() => {}} label="Rows" />);
    expect(container).toBeEmptyDOMElement();
  });
});
