import { useCallback, useEffect, useRef, useState } from "react";
import type { ApiClient } from "../api/client";
import { useApi } from "../api/context";
import { fetchMe, type Me } from "./api";

export interface Loaded<T> {
  data: T | undefined;
  error: unknown;
  loading: boolean;
  reload: () => void;
}

// useLoader runs `load` on mount and whenever `key` changes; reload() re-runs
// it. A response that arrives after a newer request started is dropped, so a
// slow first page cannot overwrite a fresher reload.
export function useLoader<T>(load: (api: ApiClient) => Promise<T>, key: string): Loaded<T> {
  const api = useApi();
  const [data, setData] = useState<T | undefined>(undefined);
  const [error, setError] = useState<unknown>(null);
  const [loading, setLoading] = useState(true);
  const [tick, setTick] = useState(0);
  const loadRef = useRef(load);
  loadRef.current = load;

  useEffect(() => {
    let current = true;
    setLoading(true);
    loadRef.current(api).then(
      (d) => {
        if (!current) return;
        setData(d);
        setError(null);
        setLoading(false);
      },
      (e: unknown) => {
        if (!current) return;
        setError(e);
        setLoading(false);
      },
    );
    return () => {
      current = false;
    };
  }, [api, key, tick]);

  const reload = useCallback(() => setTick((t) => t + 1), []);
  return { data, error, loading, reload };
}

// One /v1/me request per API client: every admin/data page asks for the
// principal, and the answer only changes on re-login (a full page load).
const meCache = new WeakMap<ApiClient, Promise<Me>>();

export function loadMe(api: ApiClient): Promise<Me> {
  let p = meCache.get(api);
  if (!p) {
    p = fetchMe(api);
    // A failed probe is not cached, so a later page can retry.
    p.catch(() => meCache.delete(api));
    meCache.set(api, p);
  }
  return p;
}

export function useMe(): Loaded<Me> {
  return useLoader(loadMe, "me");
}
