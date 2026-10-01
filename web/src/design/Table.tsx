import type { KeyboardEvent, ReactNode } from "react";
import { cx } from "./cx";

export interface Column<T> {
  /** Stable column id. */
  key: string;
  header: ReactNode;
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
  onRowClick,
  isRowSelected,
  density = "comfortable",
  className,
}: TableProps<T>) {
  const cellClass = (c: Column<T>) =>
    cx(`tc-table__cell--${c.align ?? "start"}`, c.hideOnMobile && "tc-table__cell--hide-sm", c.className);

  return (
    <div className={cx("tc-table-wrap", className)}>
      <table className={cx("tc-table", `tc-table--${density}`)} aria-busy={loading || undefined}>
        <caption className={showCaption ? "tc-table__caption" : "tc-sr-only"}>{caption}</caption>
        <thead>
          <tr>
            {columns.map((c) => (
              <th key={c.key} scope="col" className={cellClass(c)}>
                {c.header}
              </th>
            ))}
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
