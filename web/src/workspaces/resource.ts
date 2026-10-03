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

export function useResource<T>(load: () => Promise<T>, interval: PollInterval<T> = null): Resource<T> {
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

  const run = useCallback(async () => {
    const started = epoch.current;
    try {
      const d = await load();
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
  }, [load]);

  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    let stopped = false;
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
      const delay = nextDelay();
      if (delay === null || document.visibilityState === "hidden") return;
      timer = setTimeout(() => void tick(), delay);
    };
    const tick = async () => {
      await run();
      schedule();
    };
    const onVisible = () => {
      if (document.visibilityState === "visible") void tick();
      else clearTimeout(timer);
    };
    void tick();
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      stopped = true;
      epoch.current += 1;
      clearTimeout(timer);
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
    return run();
  }, [run]);

  const clearError = useCallback(() => setError(null), []);

  return { data, error, loading, refresh, mutate, clearError };
}
