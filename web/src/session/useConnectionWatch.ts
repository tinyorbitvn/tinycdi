import { useEffect, useRef, type RefObject } from "react";
import { sessionOrigin, submitLaunch, type LaunchTicket } from "./launch";

/**
 * Passive connection watch (D15). While the session is live the portal
 * polls `GET /v1/workspaces/{id}/connection` — the endpoint is passive, so
 * polling never slides the idle timer (P4/D18) — and repairs the frame:
 *
 * - `disconnected`/`stale` with `leaseActive: true`: the lease and its
 *   session cookie still work, so the frame is simply reloaded to the
 *   workspace origin, with bounded backoff.
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

/** GET /v1/workspaces/{id}/connection response body (T2.3). */
export interface ConnectionStatus {
  state: "none" | "connected" | "disconnected" | "stale";
  leaseActive: boolean;
  lastRenewedAt?: string;
}

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
  const inflight = useRef(false);
  const reloadTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const pollIntervalMs = options.pollIntervalMs ?? CONNECTION_POLL_MS;
  const enabled = options.active && options.sessionDomain !== "";

  useEffect(() => {
    if (!enabled) return;
    backoffStep.current = 0;
    exhausted.current = false;

    const cancelReload = () => {
      clearTimeout(reloadTimer.current);
      reloadTimer.current = undefined;
    };

    const reloadFrame = () => {
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
        submitLaunch(
          ticket,
          opts.current.frameName,
          opts.current.workspaceId,
          opts.current.sessionDomain,
        );
        opts.current.onEvent({ type: "relaunched" });
      } catch (e) {
        opts.current.onEvent({ type: "relaunch-error", error: e });
      }
    };

    const handle = (status: ConnectionStatus) => {
      if (status.state === "connected") {
        backoffStep.current = 0;
        exhausted.current = false;
        return;
      }
      if (exhausted.current) return;
      if (status.leaseActive) {
        cancelReload();
        const step = Math.min(backoffStep.current, RECONNECT_BACKOFF_MS.length - 1);
        backoffStep.current = step + 1;
        reloadTimer.current = setTimeout(reloadFrame, RECONNECT_BACKOFF_MS[step]);
        return;
      }
      // The lease is gone: recover through a fresh ticket, bounded per
      // window so a dead session cannot mint tickets forever.
      const now = Date.now();
      relaunches.current = relaunches.current.filter((t) => now - t < AUTO_RELAUNCH_WINDOW_MS);
      if (relaunches.current.length >= MAX_AUTO_RELAUNCH) {
        exhausted.current = true;
        cancelReload();
        opts.current.onEvent({ type: "exhausted" });
        return;
      }
      void relaunch();
    };

    const tick = async () => {
      if (document.visibilityState === "hidden") return;
      if (inflight.current) return;
      inflight.current = true;
      try {
        handle(await opts.current.fetchStatus());
      } catch (e) {
        opts.current.onEvent({ type: "poll-error", error: e });
      } finally {
        inflight.current = false;
      }
    };

    const interval = setInterval(() => void tick(), pollIntervalMs);
    return () => {
      clearInterval(interval);
      cancelReload();
    };
  }, [enabled, pollIntervalMs]);
}
