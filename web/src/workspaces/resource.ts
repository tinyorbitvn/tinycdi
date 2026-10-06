import { useCallback, useEffect, useRef, useState } from "react";

// Data-fetching primitive for the workspace area. The public API has no
// change stream (SSE/watch), so live status is polling — adaptive (fast
// while something is transitional, slow when idle), paused while the tab is
// hidden, refreshed on return, and backed off on errors.

export interface Resource<T> {
  data: T | undefined;
  error: unknown;
  /** True until the first load settles. */
  loading: boolean;
  refresh: () => Promise<void>;
  /**
   * Local (optimistic) update. Bumps the epoch so a poll that started
   * before the update cannot overwrite it with stale data.
   */
  mutate: (fn: (prev: T | undefined) => T | undefined) => void;
  clearError: () => void;
}

/** Poll delay after a load: ms, or null to stop polling. */
export type PollInterval<T> = number | null | ((data: T | undefined) => number | null);

const MAX_BACKOFF_MS = 30_000;

// The `background` flag tells the loader whether the call is a scheduled
// poll tick (true → the request carries POLL_HEADERS and cannot slide the
// portal idle window) or user-facing activity (false → mount loads,
// manual refresh, return-to-visible reloads all slide normally).
export function useResource<T>(
  load: (background: boolean) => Promise<T>,
  interval: PollInterval<T> = null,
): Resource<T> {
  const [data, setData] = useState<T | undefined>(undefined);
  const [error, setError] = useState<unknown>(null);
  const [loading, setLoading] = useState(true);
  const epoch = useRef(0);
  const dataRef = useRef<T | undefined>(undefined);
  const failures = useRef(0);
  // A server-asked delay (429 Retry-After) beats the local backoff once.
  const retryAfterMs = useRef<number | null>(null);
  const intervalRef = useRef(interval);
  useEffect(() => {
    intervalRef.current = interval;
  }, [interval]);
  // Re-arms a stopped poll from refresh(): the interval effect owns the
  // timer, so a refresh that discovers transitional data can resume
  // polling. Only fires when no tick is currently scheduled — an armed
  // poll is untouched, so always-on consumers are unchanged.
  const scheduleRef = useRef<() => void>(() => {});

  const run = useCallback(
    async (background: boolean) => {
      const started = epoch.current;
      try {
        const d = await load(background);
        if (started !== epoch.current) return;
        dataRef.current = d;
        failures.current = 0;
        retryAfterMs.current = null;
        setData(d);
        setError(null);
      } catch (e) {
        if (started !== epoch.current) return;
        failures.current += 1;
        const ra = (e as { retryAfterMs?: unknown }).retryAfterMs;
        retryAfterMs.current = typeof ra === "number" && Number.isFinite(ra) ? ra : null;
        setError(e);
      } finally {
        if (started === epoch.current) setLoading(false);
      }
    },
    [load],
  );

  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    let stopped = false;
    let armed = false;
    // A new loader (e.g. a different workspace id) starts from scratch.
    epoch.current += 1;
    dataRef.current = undefined;
    setData(undefined);
    setLoading(true);
    setError(null);

    const nextDelay = (): number | null => {
      const iv = intervalRef.current;
      const base = typeof iv === "function" ? iv(dataRef.current) : iv;
      if (base === null) return null;
      if (retryAfterMs.current !== null) return retryAfterMs.current;
      return failures.current > 0 ? Math.min(base * 2 ** failures.current, MAX_BACKOFF_MS) : base;
    };
    const schedule = () => {
      if (stopped) return;
      clearTimeout(timer);
      armed = false;
      const delay = nextDelay();
      if (delay === null || document.visibilityState === "hidden") return;
      timer = setTimeout(() => void tick(true), delay);
      armed = true;
    };
    const tick = async (background: boolean) => {
      await run(background);
      schedule();
    };
    const onVisible = () => {
      // Returning to a hidden tab is the user showing up — the reload
      // counts as activity; only the timer-driven ticks are background.
      if (document.visibilityState === "visible") void tick(false);
      else {
        clearTimeout(timer);
        armed = false;
      }
    };
    scheduleRef.current = () => {
      if (!armed) schedule();
    };
    void tick(false);
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      stopped = true;
      epoch.current += 1;
      clearTimeout(timer);
      scheduleRef.current = () => {};
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [run]);

  const mutate = useCallback((fn: (prev: T | undefined) => T | undefined) => {
    epoch.current += 1;
    const next = fn(dataRef.current);
    dataRef.current = next;
    setData(next);
    setLoading(false);
  }, []);

  const refresh = useCallback(() => {
    epoch.current += 1;
    return run(false).then(() => scheduleRef.current());
  }, [run]);

  const clearError = useCallback(() => setError(null), []);

  return { data, error, loading, refresh, mutate, clearError };
}
