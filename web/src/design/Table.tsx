import { useMemo, useState, type KeyboardEvent, type ReactNode } from "react";
import { cx } from "./cx";
import { IconChevronDown, IconChevronUp, IconSelector } from "./icons";

/** aria-sort value a sortable column currently holds ("none" = sortable, not sorted). */
export type TableSort = "ascending" | "descending" | "none";
export type SortDirection = Exclude<TableSort, "none">;

/** Active sort: the column key plus the direction it is sorted in. */
export interface TableSortState {
  key: string;
  direction: SortDirection;
}

export interface Column<T> {
  /** Stable column id. */
  key: string;
  header: ReactNode;
  /** Explicit aria-sort; sortable columns derive it from the table's `sort` prop instead. */
  sort?: TableSort;
  /**
   * Sortable column: the header renders a toggle button (click / Enter /
   * Space) and `compare` orders the rows. Wire `sort` + `onSortChange` on
   * the table — `useTableSort` does both in one call.
   */
  sortable?: boolean;
  /** Ascending-order comparator for a sortable column. */
  compare?: (a: T, b: T) => number;
  /**
   * Secondary order applied after `compare`; it is NOT flipped by the sort
   * direction, so use it for the deterministic tail of a sort (e.g. a name).
   */
  tiebreak?: (a: T, b: T) => number;
  /** Direction the first activation sorts to (default "ascending"). */
  sortStart?: SortDirection;
  /** Cell content; defaults to String(row[key]). */
  render?: (row: T) => ReactNode;
  align?: "start" | "end" | "center";
  /** Hide this column below the small breakpoint. */
  hideOnMobile?: boolean;
  /** Extra class on th/td (width presets etc.). */
  className?: string;
  /** Render as the row header (<th scope="row">). */
  rowHeader?: boolean;
}

/**
 * Orders `rows` by the sortable column named in `sort`. Array.prototype.sort
 * is stable, so rows the comparator treats as equal keep their source order.
 */
export function sortRows<T>(
  rows: readonly T[],
  columns: readonly Column<T>[],
  sort: TableSortState | null | undefined,
): T[] {
  const col = sort
    ? columns.find((c) => c.key === sort.key && c.sortable && c.compare !== undefined)
    : undefined;
  const compare = col?.compare;
  if (!col || !compare) return [...rows];
  const dir = sort!.direction === "ascending" ? 1 : -1;
  return [...rows].sort((a, b) => dir * compare(a, b) || (col.tiebreak?.(a, b) ?? 0));
}

export interface UseTableSortResult<T> {
  /** `rows` ordered by the current sort (or a copy of `rows` when unsorted). */
  sortedRows: T[];
  sort: TableSortState | null;
  /** Full next state — the table computes the toggle, callers just store it. */
  onSortChange: (next: TableSortState) => void;
}

// Sort state for a Table: pass the result's `sort`/`onSortChange` to the
// table and page over `sortedRows` — sorting always precedes pagination.
export function useTableSort<T>(
  columns: readonly Column<T>[],
  rows: readonly T[],
  initial?: TableSortState | null,
): UseTableSortResult<T> {
  const [sort, setSort] = useState<TableSortState | null>(initial ?? null);
  const sortedRows = useMemo(() => sortRows(rows, columns, sort), [rows, columns, sort]);
  return { sortedRows, sort, onSortChange: setSort };
}

export interface TableProps<T> {
  columns: Array<Column<T>>;
  rows: readonly T[];
  rowKey: (row: T) => string;
  /** Accessible name (rendered as a visually hidden caption unless `showCaption`). */
  caption: string;
  showCaption?: boolean;
  /** Rendered in place of the body when there are no rows. */
  empty?: ReactNode;
  /** Shows skeleton rows and sets aria-busy. */
  loading?: boolean;
  /** Current sort for `sortable` columns; drives aria-sort + the header icons. */
  sort?: TableSortState | null;
  /** Called with the full next sort state when a sortable header activates. */
  onSortChange?: (next: TableSortState) => void;
  /**
   * Makes rows activatable (click / Enter). Prefer putting a real link in the
   * row-header cell; this is a convenience for whole-row targets.
   */
  onRowClick?: (row: T) => void;
  /** Rows highlighted as selected. */
  isRowSelected?: (row: T) => boolean;
  density?: "comfortable" | "compact";
  className?: string;
}

export function Table<T>({
  columns,
  rows,
  rowKey,
  caption,
  showCaption = false,
  empty,
  loading = false,
  sort = null,
  onSortChange,
  onRowClick,
  isRowSelected,
  density = "comfortable",
  className,
}: TableProps<T>) {
  const cellClass = (c: Column<T>) =>
    cx(`tc-table__cell--${c.align ?? "start"}`, c.hideOnMobile && "tc-table__cell--hide-sm", c.className);

  // Click/Enter/Space on a header button: same column flips direction, a new
  // column starts at its declared `sortStart` (ascending by default).
  const activateSort = (c: Column<T>) => {
    if (!c.sortable) return;
    if (sort?.key === c.key) {
      onSortChange?.({
        key: c.key,
        direction: sort.direction === "ascending" ? "descending" : "ascending",
      });
    } else {
      onSortChange?.({ key: c.key, direction: c.sortStart ?? "ascending" });
    }
  };

  return (
    <div className={cx("tc-table-wrap", className)}>
      <table className={cx("tc-table", `tc-table--${density}`)} aria-busy={loading || undefined}>
        <caption className={showCaption ? "tc-table__caption" : "tc-sr-only"}>{caption}</caption>
        <thead>
          <tr>
            {columns.map((c) => {
              const sorted = sort?.key === c.key ? sort.direction : undefined;
              const ariaSort = c.sortable ? (sorted ?? "none") : c.sort;
              return (
                <th
                  key={c.key}
                  scope="col"
                  className={cellClass(c)}
                  {...(ariaSort !== undefined ? { "aria-sort": ariaSort } : {})}
                >
                  {c.sortable ? (
                    <button
                      type="button"
                      className={cx("tc-table__sort", sorted && "tc-table__sort--active")}
                      aria-pressed={sorted !== undefined}
                      onClick={() => activateSort(c)}
                    >
                      {c.header}
                      {sorted === "ascending" ? (
                        <IconChevronUp size={14} aria-hidden="true" />
                      ) : sorted === "descending" ? (
                        <IconChevronDown size={14} aria-hidden="true" />
                      ) : (
                        <IconSelector size={14} aria-hidden="true" />
                      )}
                    </button>
                  ) : (
                    c.header
                  )}
                </th>
              );
            })}
          </tr>
        </thead>
        <tbody>
          {loading ? (
            Array.from({ length: 3 }, (_, i) => (
              <tr key={`sk-${i}`} className="tc-table__skeleton-row">
                {columns.map((c) => (
                  <td key={c.key} className={cellClass(c)}>
                    <span className="tc-skeleton tc-skeleton--text tc-w-75" aria-hidden="true" />
                  </td>
                ))}
              </tr>
            ))
          ) : rows.length === 0 ? (
            <tr>
              <td colSpan={columns.length} className="tc-table__empty">
                {empty ?? "Nothing to show."}
              </td>
            </tr>
          ) : (
            rows.map((row) => {
              const key = rowKey(row);
              const selected = isRowSelected?.(row) ?? false;
              const activate = onRowClick ? () => onRowClick(row) : undefined;
              return (
                <tr
                  key={key}
                  className={cx(onRowClick && "tc-table__row--clickable", selected && "tc-table__row--selected")}
                  {...(selected ? { "aria-selected": true } : {})}
                  {...(activate
                    ? {
                        onClick: activate,
                        tabIndex: 0,
                        onKeyDown: (e: KeyboardEvent<HTMLTableRowElement>) => {
                          if (e.target === e.currentTarget && (e.key === "Enter" || e.key === " ")) {
                            e.preventDefault();
                            activate();
                          }
                        },
                      }
                    : {})}
                >
                  {columns.map((c) => {
                    const content = c.render
                      ? c.render(row)
                      : String((row as Record<string, unknown>)[c.key] ?? "");
                    return c.rowHeader ? (
                      <th key={c.key} scope="row" className={cellClass(c)}>
                        {content}
                      </th>
                    ) : (
                      <td key={c.key} className={cellClass(c)}>
                        {content}
                      </td>
                    );
                  })}
                </tr>
              );
            })
          )}
        </tbody>
      </table>
    </div>
  );
}
