import { useMemo, useState } from "react";
import { cx } from "./cx";
import { t } from "../i18n";
import { IconChevronLeft, IconChevronRight } from "./icons";

/** Page size the hook defaults to; callers can offer these alternatives. */
export const DEFAULT_PAGE_SIZE = 50;
export const DEFAULT_PAGE_SIZES: readonly number[] = [25, 50, 100];

/** Page count for `total` items at `pageSize`; always at least 1. */
export function pageCount(total: number, pageSize: number): number {
  return Math.max(1, Math.ceil(Math.max(0, total) / Math.max(1, pageSize)));
}

/** Clamps a 1-based page into [1, pages] (e.g. when rows shrink under it). */
export function clampPage(page: number, pages: number): number {
  return Math.min(Math.max(1, Math.trunc(page)), Math.max(1, pages));
}

export type PageItem = number | "ellipsis";

// Numbered-page strip with an ellipsis on each side once the range leaves
// a gap — "1 … 5 6 7 … 20". At 7 pages or fewer everything fits ungapped.
export function pageItems(page: number, pages: number): PageItem[] {
  const p = clampPage(page, pages);
  if (pages <= 7) return Array.from({ length: pages }, (_, i) => i + 1);
  const set = new Set<number>([1, pages, p - 1, p, p + 1]);
  const nums = [...set].filter((n) => n >= 1 && n <= pages).sort((a, b) => a - b);
  const items: PageItem[] = [];
  for (let i = 0; i < nums.length; i++) {
    const prev = nums[i - 1];
    const cur = nums[i];
    if (prev !== undefined && cur - prev > 1) {
      // A gap of exactly one page is cheaper to show than to hide.
      if (cur - prev === 2) items.push(prev + 1);
      else items.push("ellipsis");
    }
    items.push(cur);
  }
  return items;
}

export interface UsePaginationOptions {
  /** Initial page size (default 50). */
  pageSize?: number;
  /** Page-size choices offered by the returned props (default 25/50/100). */
  pageSizes?: readonly number[];
  /** When this value changes the page resets to 1 (e.g. a filter changed). */
  resetKey?: unknown;
}

export interface UsePaginationResult<T> {
  /** Current page, clamped into range — safe after rows shrink. */
  page: number;
  pageSize: number;
  /** Total rows across all pages. */
  total: number;
  /** Total pages, always >= 1. */
  pages: number;
  /** The slice of `rows` for the current page. */
  pageRows: T[];
  setPage: (page: number) => void;
  setPageSize: (size: number) => void;
  /** Spread onto <Pagination>; `label` is still required there. */
  props: Pick<
    PaginationProps,
    "total" | "page" | "pageSize" | "pageSizes" | "onPageChange" | "onPageSizeChange"
  >;
}

// Client-side paging over an already-fetched list (listAll caps at 4000 —
// server-side params stay out of scope). The page is a derived-clamped
// value, so a list shrinking under the current page (a purge, a narrower
// filter) can never leave the view on an out-of-range page.
export function usePagination<T>(
  rows: readonly T[],
  options: UsePaginationOptions = {},
): UsePaginationResult<T> {
  const { resetKey } = options;
  const [rawPage, setRawPage] = useState(1);
  const [pageSize, setPageSizeRaw] = useState(options.pageSize ?? DEFAULT_PAGE_SIZE);
  const [prevKey, setPrevKey] = useState(resetKey);
  if (prevKey !== resetKey) {
    // Render-phase reset (React's derive-state-from-props pattern): a filter
    // change brings the page back to 1 in the same commit as the new rows.
    setPrevKey(resetKey);
    setRawPage(1);
  }

  const total = rows.length;
  const pages = pageCount(total, pageSize);
  const page = clampPage(rawPage, pages);
  const pageRows = useMemo(
    () => rows.slice((page - 1) * pageSize, page * pageSize),
    [rows, page, pageSize],
  );

  const setPageSize = (size: number) => {
    setPageSizeRaw(size);
    setRawPage(1);
  };

  return {
    page,
    pageSize,
    total,
    pages,
    pageRows,
    setPage: setRawPage,
    setPageSize,
    props: {
      total,
      page,
      pageSize,
      pageSizes: options.pageSizes ?? DEFAULT_PAGE_SIZES,
      onPageChange: setRawPage,
      onPageSizeChange: setPageSize,
    },
  };
}

export interface PaginationProps {
  /** Total items across all pages. */
  total: number;
  /** Current 1-based page (clamped into range). */
  page: number;
  /** Items per page. */
  pageSize: number;
  onPageChange: (page: number) => void;
  /** Choices for the page-size select; needs `onPageSizeChange` to render. */
  pageSizes?: readonly number[];
  onPageSizeChange?: (size: number) => void;
  /** Accessible name of the <nav> landmark. */
  label: string;
  className?: string;
}

// Orbit 2.9 contract (project/components index.d.ts): a pages/onChange
// pager. This takes the same shape, controlled, plus the page-size select
// and the "Showing a–b of n" status the admin tables need. Page-number
// controls only render once there is more than one page.
export function Pagination({
  total,
  page: rawPage,
  pageSize,
  onPageChange,
  pageSizes,
  onPageSizeChange,
  label,
  className,
}: PaginationProps) {
  if (total === 0) return null;
  const pages = pageCount(total, pageSize);
  const page = clampPage(rawPage, pages);
  const from = (page - 1) * pageSize + 1;
  const to = Math.min(page * pageSize, total);

  return (
    <nav className={cx("tc-pagination", className)} aria-label={label}>
      <p className="tc-pagination__status" aria-live="polite">
        {t("common.pagination.status", { from, to, total })}
      </p>
      <div className="tc-pagination__controls">
        {pageSizes !== undefined && pageSizes.length > 1 && onPageSizeChange ? (
          <label className="tc-pagination__size">
            <span>{t("common.pagination.rowsPerPage")}</span>
            <select
              className="tc-input tc-pagination__size-select"
              value={pageSize}
              onChange={(e) => onPageSizeChange(Number(e.target.value))}
            >
              {pageSizes.map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </select>
          </label>
        ) : null}
        {pages > 1 ? (
          <ul className="tc-pagination__pages">
            <li>
              <button
                type="button"
                className="tc-pagination__button tc-pagination__step"
                disabled={page === 1}
                onClick={() => onPageChange(page - 1)}
              >
                <IconChevronLeft size={14} aria-hidden="true" />
                {t("common.pagination.previous")}
              </button>
            </li>
            {pageItems(page, pages).map((item, i) =>
              item === "ellipsis" ? (
                <li key={`gap-${i}`}>
                  <span className="tc-pagination__ellipsis" aria-hidden="true">
                    …
                  </span>
                </li>
              ) : (
                <li key={item}>
                  <button
                    type="button"
                    className={cx(
                      "tc-pagination__button tc-pagination__page",
                      item === page && "tc-pagination__page--current",
                    )}
                    aria-label={t("common.pagination.page", { n: item })}
                    {...(item === page ? { "aria-current": "page" as const } : {})}
                    onClick={() => onPageChange(item)}
                  >
                    {item}
                  </button>
                </li>
              ),
            )}
            <li>
              <button
                type="button"
                className="tc-pagination__button tc-pagination__step"
                disabled={page === pages}
                onClick={() => onPageChange(page + 1)}
              >
                {t("common.pagination.next")}
                <IconChevronRight size={14} aria-hidden="true" />
              </button>
            </li>
          </ul>
        ) : null}
      </div>
    </nav>
  );
}
