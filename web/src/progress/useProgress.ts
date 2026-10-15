import { useEffect, useRef, useState } from "react";
import {
  createTracker,
  deriveProgress,
  serverNow,
  type ProgressModel,
  type ProgressTracker,
  type WorkspaceView,
} from "./derive";

export interface ProgressState {
  model: ProgressModel | null;
  /** Consecutive failed refreshes while a model is on screen. */
  refreshFailures: number;
}

/**
 * The progress model for one workspace row/panel. Owns the per-tab tracker
 * (the replica-lag latch) and the 1 s ticker that moves the elapsed counters
 * and slow thresholds — a local timer only, never a request (D18).
 */
export function useProgress(
  workspace: WorkspaceView | undefined,
  opts?: { refreshError?: unknown },
): ProgressState {
  const tracker = useRef<ProgressTracker>(createTracker());
  const [model, setModel] = useState<ProgressModel | null>(() =>
    workspace ? deriveProgress(workspace, tracker.current, serverNow()) : null,
  );
  const [tick, setTick] = useState(0);
  const [refreshFailures, setRefreshFailures] = useState(0);
  const lastError = useRef<unknown>(undefined);
  const wsId = workspace?.id;

  // A different workspace resets the latch (the tracker key also covers
  // intent epochs via updatedAt — this guards a swapped row).
  useEffect(() => {
    tracker.current = createTracker();
  }, [wsId]);

  useEffect(() => {
    setModel(workspace ? deriveProgress(workspace, tracker.current, serverNow()) : null);
  }, [workspace, tick]);

  const live = model !== null && model.terminal === null;
  useEffect(() => {
    if (!live) return;
    const t = setInterval(() => setTick((n) => n + 1), 1_000);
    return () => clearInterval(t);
  }, [live]);

  // Count consecutive refresh failures for the "can't refresh" escalation.
  const refreshError = opts?.refreshError;
  useEffect(() => {
    if (refreshError === undefined || refreshError === null) {
      lastError.current = null;
      setRefreshFailures(0);
    } else if (refreshError !== lastError.current) {
      lastError.current = refreshError;
      setRefreshFailures((n) => n + 1);
    }
  }, [refreshError]);

  return { model, refreshFailures };
}
