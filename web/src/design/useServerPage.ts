import { useCallback, useEffect, useRef, useState } from "react";
import type { TableSortState } from "./Table";
import { t } from "../i18n";

// Server-side paging for /v1 collections: every list takes `cursor` and
// `sort` and answers {items, nextCursor} (nextCursor absent on the last
// page). The hook holds the rows loaded so far in server order; next()
// appends the following page, reset(sort) starts over under a new sort, and
// refresh() re-reads the loaded range in place (polling, after an action).

export interface ServerPage<T> {
  items: T[];
  nextCursor?: string | undefined;
}

export type FetchServerPage<T> = (cursor?: string, sort?: string) => Promise<ServerPage<T>>;

export interface UseServerPageOptions<T = unknown> {
  /** Initial `sort` parameter (an allowlisted field, `-` prefix = descending). */
  sort: string;
  /** When this value changes the list restarts from the first page (a filter changed). */
  key?: unknown;
  /**
   * Live lists: the delay (ms) before the next refresh() given the loaded
   * rows, or null to stop. Paused while the tab is hidden (refreshed on
   * return); consecutive failures back off up to 30 s.
   */
  poll?: (items: readonly T[]) => number | null;
}

export interface ServerPageResult<T> {
  /** Rows loaded so far, in server order. */
  items: T[];
  /** True while a page request is in flight. */
  loading: boolean;
  /** True once the first page since the last reset has answered. */
  loaded: boolean;
  /** The last request's failure; loaded rows stay. */
  error: unknown;
  /** True when the server reported another page. */
  hasNext: boolean;
  /** The sort the loaded rows are in. */
  sort: string;
  /** Appends the next page; a no-op without one or while loading. */
  next: () => void;
  /** Clears the rows and reloads the first page, under `sort` when given. */
  reset: (sort?: string) => void;
  /** Re-reads as many pages as are loaded and swaps them in without clearing. */
  refresh: () => void;
  /** Dismisses the current error (rows stay). */
  clearError: () => void;
}

export function useServerPage<T>(
  fetchPage: FetchServerPage<T>,
  opts: UseServerPageOptions<T>,
): ServerPageResult<T> {
  const [items, setItems] = useState<T[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<unknown>(null);
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const [sort, setSort] = useState(opts.sort);
  const [loaded, setLoaded] = useState(false);

  const fetchRef = useRef(fetchPage);
  fetchRef.current = fetchPage;
  // Every reset/refresh bumps the generation; a response from an older
  // generation is dropped so a slow page never lands under a newer sort.
  const gen = useRef(0);
  const busy = useRef(false);
  const sortRef = useRef(opts.sort);
  const cursorRef = useRef<string | undefined>(undefined);
  const pagesRef = useRef(0);
  // Consecutive refresh failures, for the poll backoff.
  const failures = useRef(0);

  const loadFirst = useCallback((nextSort: string) => {
    const g = ++gen.current;
    sortRef.current = nextSort;
    cursorRef.current = undefined;
    pagesRef.current = 0;
    busy.current = true;
    setSort(nextSort);
    setLoaded(false);
    setItems([]);
    setCursor(undefined);
    setError(null);
    setLoading(true);
    fetchRef.current(undefined, nextSort).then(
      (page) => {
        if (g !== gen.current) return;
        busy.current = false;
        failures.current = 0;
        pagesRef.current = 1;
        cursorRef.current = page.nextCursor || undefined;
        setItems(page.items);
        setCursor(cursorRef.current);
        setLoaded(true);
        setLoading(false);
      },
      (e: unknown) => {
        if (g !== gen.current) return;
        busy.current = false;
        failures.current += 1;
        setError(e);
        setLoading(false);
      },
    );
  }, []);

  useEffect(() => {
    loadFirst(sortRef.current);
    return () => {
      gen.current += 1; // unmount or key change: drop in-flight answers
    };
  }, [opts.key, loadFirst]);

  const next = useCallback(() => {
    const at = cursorRef.current;
    if (busy.current || at === undefined) return;
    const g = gen.current;
    busy.current = true;
    setError(null);
    setLoading(true);
    fetchRef.current(at, sortRef.current).then(
      (page) => {
        if (g !== gen.current) return;
        busy.current = false;
        pagesRef.current += 1;
        cursorRef.current = page.nextCursor || undefined;
        setItems((prev) => [...prev, ...page.items]);
        setCursor(cursorRef.current);
        setLoading(false);
      },
      (e: unknown) => {
        if (g !== gen.current) return;
        busy.current = false;
        setError(e);
        setLoading(false);
      },
    );
  }, []);

  const reset = useCallback((nextSort?: string) => loadFirst(nextSort ?? sortRef.current), [loadFirst]);

  const refresh = useCallback(() => {
    const g = ++gen.current;
    const want = Math.max(1, pagesRef.current);
    const s = sortRef.current;
    busy.current = true;
    setLoading(true);
    void (async () => {
      const rows: T[] = [];
      let at: string | undefined;
      let pages = 0;
      try {
        do {
          const page = await fetchRef.current(at, s);
          if (g !== gen.current) return;
          rows.push(...page.items);
          at = page.nextCursor || undefined;
          pages += 1;
        } while (at !== undefined && pages < want);
      } catch (e) {
        if (g !== gen.current) return;
        busy.current = false;
        failures.current += 1;
        setError(e);
        setLoading(false);
        return;
      }
      busy.current = false;
      failures.current = 0;
      pagesRef.current = pages;
      cursorRef.current = at;
      setItems(rows);
      setCursor(at);
      setError(null);
      setLoaded(true);
      setLoading(false);
    })();
  }, []);

  // Live lists re-read the loaded range on a timer chosen from the rows.
  const pollRef = useRef(opts.poll);
  pollRef.current = opts.poll;
  const hiddenDue = useRef(false);
  useEffect(() => {
    const poll = pollRef.current;
    // A failed first page keeps polling too (with backoff), so a live list
    // recovers on its own once the API answers again.
    if (!poll || loading || (!loaded && !error)) return;
    const base = poll(items);
    if (base === null) return;
    const delay = error ? Math.min(base * 2 ** failures.current, MAX_POLL_BACKOFF_MS) : base;
    const timer = setTimeout(() => {
      if (typeof document !== "undefined" && document.visibilityState === "hidden") {
        hiddenDue.current = true;
        return;
      }
      refresh();
    }, delay);
    return () => clearTimeout(timer);
  }, [items, loading, loaded, error, refresh]);
  useEffect(() => {
    if (typeof document === "undefined") return;
    const onVisible = () => {
      if (document.visibilityState === "visible" && hiddenDue.current) {
        hiddenDue.current = false;
        refresh();
      }
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => document.removeEventListener("visibilitychange", onVisible);
  }, [refresh]);

  const clearError = useCallback(() => setError(null), []);

  return { items, loading, loaded, error, hasNext: cursor !== undefined, sort, next, reset, refresh, clearError };
}

const MAX_POLL_BACKOFF_MS = 30_000;

export interface UseServerTableOptions<T> extends Omit<UseServerPageOptions<T>, "sort"> {
  /** Column key → server sort field; only these columns may be `sortable`. */
  fields: Readonly<Record<string, string>>;
  /** The column sort the table opens with (its field must be in `fields`). */
  initial: TableSortState;
}

export interface ServerTableResult<T> extends ServerPageResult<T> {
  /** Spread onto <Table> as sort/onSortChange: header clicks re-query the server. */
  tableSort: TableSortState;
  onSortChange: (next: TableSortState) => void;
}

// A table over a server collection: the column sort drives the `sort`
// parameter (header clicks reset the list under the new order) and rows
// render in server order — no client-side re-sort of a partial list.
export function useServerTable<T>(
  fetchPage: FetchServerPage<T>,
  opts: UseServerTableOptions<T>,
): ServerTableResult<T> {
  const { fields, initial, ...rest } = opts;
  const [tableSort, setTableSort] = useState<TableSortState>(initial);
  const list = useServerPage(fetchPage, { ...rest, sort: serverSortParam(initial, fields) ?? "" });
  const { reset } = list;
  const onSortChange = useCallback(
    (next: TableSortState) => {
      const param = serverSortParam(next, fields);
      if (param === undefined) return;
      setTableSort(next);
      reset(param);
    },
    [fields, reset],
  );
  return { ...list, tableSort, onSortChange };
}

/** Maps a column sort onto the server's `sort` parameter (`-` = descending). */
export function serverSortParam(sort: TableSortState, fields: Readonly<Record<string, string>>): string | undefined {
  const field = fields[sort.key];
  if (field === undefined) return undefined;
  return sort.direction === "descending" ? `-${field}` : field;
}

/** Page size for scanCollection: the contract maximum, fewest round trips. */
export const SCAN_PAGE_LIMIT = 500;

/**
 * Hard cap on the pages one scanCollection call reads (SCAN_PAGE_LIMIT rows
 * each, so 10 000 rows). A scan that still has a next cursor after this many
 * pages is rejected rather than truncated: an aggregate view must never show
 * a partial count as a total, and a backend that keeps minting fresh cursors
 * cannot pin the tab.
 */
export const SCAN_MAX_PAGES = 20;

/** A scanCollection call hit SCAN_MAX_PAGES, or the server repeated a cursor. */
export class CollectionTooLargeError extends Error {
  readonly pages: number;
  constructor(pages: number) {
    super(t("common.collectionTooLarge", { n: pages * SCAN_PAGE_LIMIT }));
    this.name = "CollectionTooLargeError";
    this.pages = pages;
  }
}

// Reads a whole collection, bounded by SCAN_MAX_PAGES. Only for views that
// aggregate across the collection because the API has no aggregate or
// owner/id filter (overview counts, per-template and per-user tallies, a
// lookup by id); tables and lists page with useServerPage instead. Rejects
// with CollectionTooLargeError instead of returning a truncated result.
export async function scanCollection<T>(
  fetchPage: FetchServerPage<T>,
  sort?: string,
  maxPages: number = SCAN_MAX_PAGES,
): Promise<T[]> {
  const rows: T[] = [];
  const seen = new Set<string>();
  let at: string | undefined;
  for (let pages = 0; ; ) {
    const page = await fetchPage(at, sort);
    pages++;
    rows.push(...page.items);
    at = page.nextCursor || undefined;
    if (at === undefined) return rows;
    if (pages >= maxPages || seen.has(at)) throw new CollectionTooLargeError(pages);
    seen.add(at);
  }
}
