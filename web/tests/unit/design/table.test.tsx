import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { useState } from "react";
import {
  sortRows,
  Table,
  useTableSort,
  type Column,
  type TableSortState,
} from "../../../src/design/Table";

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

type NumRow = { id: string; n: number };

const sortableColumns: Column<NumRow>[] = [
  { key: "id", header: "ID", sortable: true, compare: (a, b) => a.id.localeCompare(b.id) },
  { key: "n", header: "N", sortable: true, sortStart: "descending", compare: (a, b) => a.n - b.n },
];

function SortableTable({ initial }: { initial?: TableSortState }) {
  const data: NumRow[] = [
    { id: "b", n: 2 },
    { id: "a", n: 2 },
    { id: "c", n: 1 },
  ];
  const sort = useTableSort(sortableColumns, data, initial ?? null);
  return (
    <Table
      caption="nums"
      columns={sortableColumns}
      rows={sort.sortedRows}
      rowKey={(r) => r.id}
      sort={sort.sort}
      onSortChange={sort.onSortChange}
    />
  );
}

const bodyOrder = () =>
  within(screen.getByRole("table", { name: "nums" }))
    .getAllByRole("row")
    .slice(1)
    .map((r) => within(r).getAllByRole("cell")[0].textContent);

// Sortable columns: the header button toggles direction (and a new column
// starts at its sortStart), aria-sort tracks the state, and equal keys keep
// their source order — the sort is stable.
describe("design/Table sortable columns", () => {
  it("sortRows is stable and honours direction and tiebreak", () => {
    const data: NumRow[] = [
      { id: "b", n: 2 },
      { id: "a", n: 2 },
      { id: "c", n: 1 },
    ];
    expect(sortRows(data, sortableColumns, null).map((r) => r.id)).toEqual(["b", "a", "c"]);
    // Ascending by n: c first; the two n=2 rows keep source order (b before a).
    expect(
      sortRows(data, sortableColumns, { key: "n", direction: "ascending" }).map((r) => r.id),
    ).toEqual(["c", "b", "a"]);
    // Descending flips the primary only — the tied pair still keeps source order.
    expect(
      sortRows(data, sortableColumns, { key: "n", direction: "descending" }).map((r) => r.id),
    ).toEqual(["b", "a", "c"]);
    // tiebreak breaks primary ties and is NOT flipped by the direction.
    const tied: Column<NumRow>[] = [
      {
        key: "n",
        header: "N",
        sortable: true,
        compare: (a, b) => a.n - b.n,
        tiebreak: (a, b) => a.id.localeCompare(b.id),
      },
    ];
    expect(
      sortRows(data, tied, { key: "n", direction: "descending" }).map((r) => r.id),
    ).toEqual(["a", "b", "c"]);
    // A column marked sortable but carrying no compare leaves order untouched.
    const noCmp: Column<NumRow>[] = [{ key: "id", header: "ID", sortable: true }];
    expect(sortRows(data, noCmp, { key: "id", direction: "ascending" }).map((r) => r.id)).toEqual(
      ["b", "a", "c"],
    );
  });

  it("click toggles a column through start direction then flips", () => {
    render(<SortableTable />);
    const header = screen.getByRole("columnheader", { name: "N" });
    expect(header).toHaveAttribute("aria-sort", "none");

    // First activation honours sortStart: descending.
    fireEvent.click(screen.getByRole("button", { name: "N" }));
    expect(header).toHaveAttribute("aria-sort", "descending");
    expect(bodyOrder()).toEqual(["b", "a", "c"]);

    // Same column flips to ascending.
    fireEvent.click(screen.getByRole("button", { name: "N" }));
    expect(header).toHaveAttribute("aria-sort", "ascending");
    expect(bodyOrder()).toEqual(["c", "b", "a"]);
  });

  it("activating another column switches the sorted header", () => {
    render(<SortableTable />);
    fireEvent.click(screen.getByRole("button", { name: "ID" }));
    expect(screen.getByRole("columnheader", { name: "ID" })).toHaveAttribute(
      "aria-sort",
      "ascending",
    );
    expect(screen.getByRole("columnheader", { name: "N" })).toHaveAttribute("aria-sort", "none");
    expect(bodyOrder()).toEqual(["a", "b", "c"]);
  });

  it("the sort button is keyboard-focusable (Enter/Space activate natively)", () => {
    render(<SortableTable />);
    const button = screen.getByRole("button", { name: "ID" });
    button.focus();
    expect(button).toHaveFocus();
    fireEvent.click(button);
    expect(bodyOrder()).toEqual(["a", "b", "c"]);
    // Focus stays on the header control through the re-sort.
    expect(screen.getByRole("button", { name: "ID" })).toHaveFocus();
  });

  it("initial sort applies immediately", () => {
    render(<SortableTable initial={{ key: "id", direction: "descending" }} />);
    expect(bodyOrder()).toEqual(["c", "b", "a"]);
    expect(screen.getByRole("columnheader", { name: "ID" })).toHaveAttribute(
      "aria-sort",
      "descending",
    );
  });

  it("non-sortable columns render no button and no aria-sort", () => {
    const cols: Column<NumRow>[] = [{ key: "id", header: "ID" }];
    render(
      <Table caption="t" columns={cols} rows={[]} rowKey={(r) => r.id} />,
    );
    expect(screen.getByRole("columnheader", { name: "ID" })).not.toHaveAttribute("aria-sort");
    expect(screen.queryByRole("button", { name: "ID" })).not.toBeInTheDocument();
  });

  it("a controlled sort drives the same wiring without the hook", () => {
    function Controlled() {
      const [sort, setSort] = useState<TableSortState | null>(null);
      return (
        <Table
          caption="nums"
          columns={sortableColumns}
          rows={sortRows([{ id: "b", n: 2 }, { id: "a", n: 1 }], sortableColumns, sort)}
          rowKey={(r) => r.id}
          sort={sort}
          onSortChange={setSort}
        />
      );
    }
    render(<Controlled />);
    fireEvent.click(screen.getByRole("button", { name: "ID" }));
    expect(screen.getByRole("columnheader", { name: "ID" })).toHaveAttribute(
      "aria-sort",
      "ascending",
    );
  });
});

describe("design/Table empty", () => {
  const cols: Column<Row>[] = [{ key: "name", header: "Name" }];

  it("an empty table is marked empty and shows the text in place of rows", () => {
    const { container } = render(
      <Table caption="rows" columns={cols} rows={[] as Row[]} rowKey={(r) => r.id} empty="Nothing here" />,
    );
    expect(container.querySelector(".tc-table-wrap")).toHaveClass("tc-table-wrap--empty");
    expect(screen.getByRole("table", { name: "rows" })).toHaveClass("tc-table--empty");
    expect(screen.getByText("Nothing here")).toHaveClass("tc-table__empty-text");
  });

  it("an element empty state is rendered as given", () => {
    render(
      <Table
        caption="rows"
        columns={cols}
        rows={[] as Row[]}
        rowKey={(r) => r.id}
        empty={<div data-testid="custom-empty">Custom</div>}
      />,
    );
    expect(screen.getByTestId("custom-empty")).toBeInTheDocument();
  });

  it("a loading table is not marked empty", () => {
    const { container } = render(
      <Table caption="rows" columns={cols} rows={[] as Row[]} rowKey={(r) => r.id} loading />,
    );
    expect(container.querySelector(".tc-table-wrap")).not.toHaveClass("tc-table-wrap--empty");
  });
});
