import { useEffect, useRef, type RefObject } from "react";
import type { components } from "../api/generated/schema";
import { sessionOrigin, submitLaunch, type LaunchTicket } from "./launch";

/**
 * Passive connection watch (D15). While the session is live the portal
 * polls `GET /v1/workspaces/{id}/connection` — the endpoint is passive, so
 * polling never slides the idle timer (P4/D18) — and repairs the frame:
 *
 * - `disconnected`/`stale` with `leaseActive: true`: the lease and its
 *   session cookie still work, so the frame is simply reloaded to the
 *   workspace origin, with bounded backoff (one pending reload at a time;
 *   a poll never re-arms it). When every step has run and the lease is
 *   still active but not streaming, `exhausted` is reported — but only
 *   after the last reload had its own interval to take effect.
 * - `leaseActive: false` (or `none`): the lease is gone; a fresh ticket is
 *   requested and submitted into the frame, at most MAX_AUTO_RELAUNCH
 *   times per 5 minutes. Past that, `exhausted` is reported and the page
 *   shows the disconnected state with a manual Reconnect and the
 *   open-in-new-tab fallback.
 *
 * There is deliberately no postMessage channel: this poll is the only
 * signal, and it pauses entirely while the page is hidden.
 */

export const RECONNECT_BACKOFF_MS = [1000, 2000, 4000, 8000, 15000] as const;
export const MAX_AUTO_RELAUNCH = 2; // per 5 minutes
export const CONNECTION_POLL_MS = 5_000;
export const AUTO_RELAUNCH_WINDOW_MS = 5 * 60_000;

/** GET /v1/workspaces/{id}/connection response body. */
export type ConnectionStatus = components["schemas"]["ConnectionStatus"];

export type WatchEvent =
  /** The frame was navigated to the workspace origin (lease-active reload). */
  | { type: "frame-navigated" }
  /** A fresh launch ticket was submitted into the frame. */
  | { type: "relaunched" }
  /** The auto-relaunch budget is spent: show the disconnected state. */
  | { type: "exhausted" }
  /** The status poll itself failed (network, 401, 404). */
  | { type: "poll-error"; error: unknown }
  /** Requesting a relaunch ticket failed. */
  | { type: "relaunch-error"; error: unknown };

export interface ConnectionWatchOptions {
  workspaceId: string;
  /** host[:port] from /v1/me; the watch idles while empty. */
  sessionDomain: string;
  /** Browsing-context name of the session iframe. */
  frameName: string;
  frame: RefObject<HTMLIFrameElement | null>;
  /** Poll only while the session is live (status "connected"). */
  active: boolean;
  fetchStatus: () => Promise<ConnectionStatus>;
  /** Sees every successful poll before the watch acts on it. */
  onObserve?: ((status: ConnectionStatus) => void) | undefined;
  /** Mint a launch ticket (POST /v1/workspaces/{id}/connections). */
  requestTicket: () => Promise<LaunchTicket>;
  onEvent: (ev: WatchEvent) => void;
  pollIntervalMs?: number | undefined;
}

export function useConnectionWatch(options: ConnectionWatchOptions): void {
  // Callbacks arrive inline; keep the latest in a ref so the interval is
  // stable across renders.
  const opts = useRef(options);
  useEffect(() => {
    opts.current = options;
  });

  const backoffStep = useRef(0);
  const relaunches = useRef<number[]>([]);
  const exhausted = useRef(false);
  const reloadTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const pollIntervalMs = options.pollIntervalMs ?? CONNECTION_POLL_MS;
  const enabled = options.active && options.sessionDomain !== "";

  useEffect(() => {
    if (!enabled) return;
    backoffStep.current = 0;
    exhausted.current = false;
    // Generation token for this run of the effect: a poll or ticket request
    // that settles after the watch was disabled or unmounted must not act.
    let stopped = false;
    let inflight = false;
    // Earliest moment the lease-active path may give up: the last reload
    // gets its step's full interval, not just the time to the next poll.
    let exhaustAfter = 0;

    const cancelReload = () => {
      clearTimeout(reloadTimer.current);
      reloadTimer.current = undefined;
    };

    const reloadFrame = () => {
      // The backoff advances only when a reload actually runs, so polls that
      // find a reload already pending cannot burn through the budget.
      reloadTimer.current = undefined;
      const interval = RECONNECT_BACKOFF_MS[backoffStep.current] ?? 0;
      backoffStep.current += 1;
      if (backoffStep.current >= RECONNECT_BACKOFF_MS.length) exhaustAfter = Date.now() + interval;
      const el = opts.current.frame.current;
      if (!el) return;
      // The session cookie on this host is bound to the live lease, so a
      // plain navigation to the workspace origin resumes the desktop.
      el.src = sessionOrigin(opts.current.workspaceId, opts.current.sessionDomain);
      opts.current.onEvent({ type: "frame-navigated" });
    };

    const relaunch = async () => {
      relaunches.current.push(Date.now());
      try {
        const ticket = await opts.current.requestTicket();
        if (stopped) return;
        submitLaunch(
          ticket,
          opts.current.frameName,
          opts.current.workspaceId,
          opts.current.sessionDomain,
        );
        opts.current.onEvent({ type: "relaunched" });
      } catch (e) {
        if (stopped) return;
        opts.current.onEvent({ type: "relaunch-error", error: e });
      }
    };

    const exhaust = () => {
      exhausted.current = true;
      cancelReload();
      opts.current.onEvent({ type: "exhausted" });
    };

    const handle = (status: ConnectionStatus) => {
      if (status.state === "connected") {
        cancelReload();
        backoffStep.current = 0;
        exhausted.current = false;
        return;
      }
      if (exhausted.current) return;
      if (status.leaseActive) {
        // A reload is already scheduled: let it run instead of re-arming it
        // on every poll (the poll is faster than the later backoff steps).
        if (reloadTimer.current !== undefined) return;
        if (backoffStep.current >= RECONNECT_BACKOFF_MS.length) {
          // Every step ran and the lease is still active but not streaming;
          // the last reload may still be settling.
          if (Date.now() < exhaustAfter) return;
          exhaust();
          return;
        }
        reloadTimer.current = setTimeout(reloadFrame, RECONNECT_BACKOFF_MS[backoffStep.current]);
        return;
      }
      // The lease is gone: recover through a fresh ticket, bounded per
      // window so a dead session cannot mint tickets forever.
      const now = Date.now();
      relaunches.current = relaunches.current.filter((t) => now - t < AUTO_RELAUNCH_WINDOW_MS);
      if (relaunches.current.length >= MAX_AUTO_RELAUNCH) {
        exhaust();
        return;
      }
      cancelReload();
      void relaunch();
    };

    const tick = async () => {
      if (document.visibilityState === "hidden") return;
      if (inflight) return;
      inflight = true;
      try {
        const status = await opts.current.fetchStatus();
        if (!stopped) {
          opts.current.onObserve?.(status);
          handle(status);
        }
      } catch (e) {
        if (!stopped) opts.current.onEvent({ type: "poll-error", error: e });
      } finally {
        inflight = false;
      }
    };

    const interval = setInterval(() => void tick(), pollIntervalMs);
    return () => {
      stopped = true;
      clearInterval(interval);
      cancelReload();
    };
  }, [enabled, pollIntervalMs]);
}
