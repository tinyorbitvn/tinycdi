import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Table, type Column } from "../../../src/design/Table";

type Row = { id: string; name: string };

const rows: Row[] = [{ id: "1", name: "alpha" }];

// aria-sort (WCAG table sorting): only sortable columns carry the
// attribute; "none" marks a sortable-but-unsorted header.
describe("design/Table aria-sort", () => {
  it("exposes aria-sort on sortable headers only", () => {
    const columns: Column<Row>[] = [
      { key: "name", header: "Name", sort: "ascending" },
      { key: "id", header: "ID", sort: "none" },
      { key: "plain", header: "Plain" },
    ];
    render(<Table caption="rows" columns={columns} rows={rows} rowKey={(r) => r.id} />);
    expect(screen.getByRole("columnheader", { name: "Name" })).toHaveAttribute(
      "aria-sort",
      "ascending",
    );
    expect(screen.getByRole("columnheader", { name: "ID" })).toHaveAttribute("aria-sort", "none");
    expect(screen.getByRole("columnheader", { name: "Plain" })).not.toHaveAttribute("aria-sort");
  });

  it("reflects a descending sort", () => {
    const columns: Column<Row>[] = [{ key: "name", header: "Name", sort: "descending" }];
    render(<Table caption="rows" columns={columns} rows={rows} rowKey={(r) => r.id} />);
    expect(screen.getByRole("columnheader", { name: "Name" })).toHaveAttribute(
      "aria-sort",
      "descending",
    );
  });
});
