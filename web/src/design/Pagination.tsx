import { cx } from "./cx";
import { t } from "../i18n";

export interface PaginationProps {
  /** Rows loaded so far. */
  loaded: number;
  /** True when the server reported another page. */
  hasNext: boolean;
  /** True while a page request is in flight (disables the control). */
  loading?: boolean;
  /** Loads the next page (useServerPage's next()). */
  onLoadMore: () => void;
  /** Accessible name of the <nav> landmark. */
  label: string;
  className?: string;
}

// Footer of a server-paged table: how many rows are loaded and, while the
// server reports another page, a "Load more" control that appends it. The
// collection is never fetched whole — each page is one request, in the
// order the table's sort asked the server for.
export function Pagination({ loaded, hasNext, loading = false, onLoadMore, label, className }: PaginationProps) {
  if (loaded === 0 && !hasNext) return null;
  return (
    <nav className={cx("tc-pagination", className)} aria-label={label}>
      <p className="tc-pagination__status" aria-live="polite">
        {hasNext
          ? t("common.pagination.loadedMore", { n: loaded })
          : t("common.pagination.loadedAll", { n: loaded })}
      </p>
      {hasNext ? (
        <div className="tc-pagination__controls">
          <button
            type="button"
            className="tc-pagination__button"
            disabled={loading}
            aria-busy={loading || undefined}
            onClick={onLoadMore}
          >
            {t("common.pagination.loadMore")}
          </button>
        </div>
      ) : null}
    </nav>
  );
}
