import { useEffect, useRef, type RefObject } from "react";
import type { components } from "../api/generated/schema";
import { sessionFrameUrl, sessionTabId, submitLaunch, type LaunchTicket } from "./launch";

/**
 * Passive connection watch (D15). While the session is live the portal
 * polls `GET /v1/workspaces/{id}/connection` — the endpoint is passive, so
 * polling never slides the idle timer (P4/D18) — and repairs the frame:
 *
 * - `disconnected`/`stale` with `leaseActive: true`: the lease and its
 *   session cookie still work. The KasmVNC client inside the frame gets
 *   IN_FRAME_RETRY_MS to re-claim the stream on its own websocket
 *   (reconnect=true in the frame URL, FX-R32) — the page stays out of the
 *   way so a rollout's mass reconnect lands as cheap websocket retries,
 *   not sixty client-document reloads at once. Only when no reconnect was
 *   observed for longer than that window does the watch re-navigate the
 *   frame to the workspace origin, then keeps re-navigating on
 *   RECONNECT_BACKOFF_MS spacing: one pending reload at a time, and the
 *   spacing is long enough that a nav->claim in flight is never aborted by
 *   the next step (the rc.3 herd). When every step has run and the lease
 *   is still active but not streaming, `exhausted` is reported — but only
 *   after the last reload had its own interval to take effect.
 * - `streamOwnerTab` set and different: the stream was fenced by another
 *   tab — not lost. The page's observation layer owns the verdict
 *   ("elsewhere"); the watch never navigates into a foreign claim, so the
 *   frame cannot fight the other owner for the stream.
 * - `leaseActive: false` (or `none`): the lease is gone; a fresh ticket is
 *   requested and submitted into the frame, at most MAX_AUTO_RELAUNCH
 *   times per 5 minutes. Past that, `exhausted` is reported and the page
 *   shows the disconnected state with a manual Reconnect and the
 *   open-in-new-tab fallback.
 *
 * There is deliberately no postMessage channel: this poll is the only
 * signal, and it pauses entirely while the page is hidden.
 */

// How long the frame's own KasmVNC retry gets to reclaim the stream before
// the watch re-navigates the frame itself (FX-R32): long enough for the
// client's retry + the broker claim to show on the poll, short enough that
// a real loss still meets the after-rollout reconnect gates.
export const IN_FRAME_RETRY_MS = 5_000;
// Spacing between frame re-navigations once the in-frame window ran out.
// The ladder's SHAPE is the outer bound (advisor MINOR-2): at most
// length-many attempts, and each nav's claim gets the last step — 20 s —
// as its evidence budget before the next nav may fire.
export const RECONNECT_BACKOFF_MS = [10_000, 15_000, 20_000] as const;
// A re-navigation is a claim attempt: after a nav starts, the watch waits
// for the poll to show a NEW stream epoch (the claim landed — stop
// re-navving) or this outer bound to elapse. 20 s covers the 5-10 s
// nav->claim measured at fleet scale on rc.3, so a slow-but-live claim is
// never aborted by the next reload.
export const CLAIM_EVIDENCE_MS = RECONNECT_BACKOFF_MS[RECONNECT_BACKOFF_MS.length - 1];
// Jitter added to the FIRST re-navigation only: a fleet whose in-frame
// windows all expire on the same poll tick must not re-navigate in
// lockstep.
export const FIRST_RENAV_JITTER_MS = 3_000;
export const MAX_AUTO_RELAUNCH = 2; // per 5 minutes
export const CONNECTION_POLL_MS = 5_000;
export const AUTO_RELAUNCH_WINDOW_MS = 5 * 60_000;
// Minimum spacing between relaunch ticket mints: a successful mint's
// "relaunched" dispatch takes a render to disable this watch, and a
// straggler lease-gone poll in that gap must not mint again (two mints
// back-to-back burn the whole budget). Real retries still get through —
// a live relaunch means the next poll finds the lease active, and a
// genuinely dead one outlives the gate.
export const RELAUNCH_RETRY_MS = 1_000;

/** GET /v1/workspaces/{id}/connection response body. */
export type ConnectionStatus = components["schemas"]["ConnectionStatus"];

export type WatchEvent =
  /** The stream is down while the lease is live; recovery is in flight. */
  | { type: "recovering" }
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
  /**
   * How long the frame's own retry gets before the watch re-navigates;
   * defaults to IN_FRAME_RETRY_MS. Injectable for tests.
   */
  inFrameRetryMs?: number | undefined;
  /**
   * The first re-navigation's jitter delay; defaults to a uniform random
   * value in [0, FIRST_RENAV_JITTER_MS). Injectable for tests.
   */
  reNavJitter?: (() => number) | undefined;
  /**
   * Resolves the URL a lease-active frame reload points the iframe at;
   * defaults to sessionFrameUrl with the current policy options. A function
   * so the reload picks up a clipboard policy resolved after mount.
   */
  frameUrl?: (() => string) | undefined;
}

export function useConnectionWatch(options: ConnectionWatchOptions): void {
  // Callbacks arrive inline; keep the latest in a ref so the interval is
  // stable across renders.
  const opts = useRef(options);
  useEffect(() => {
    opts.current = options;
  });

  const reloads = useRef(0);
  const relaunches = useRef<number[]>([]);
  const exhausted = useRef(false);
  const pollIntervalMs = options.pollIntervalMs ?? CONNECTION_POLL_MS;
  const inFrameRetryMs = options.inFrameRetryMs ?? IN_FRAME_RETRY_MS;
  const reNavJitter =
    options.reNavJitter ?? (() => Math.floor(Math.random() * FIRST_RENAV_JITTER_MS));
  const enabled = options.active && options.sessionDomain !== "";

  useEffect(() => {
    if (!enabled) return;
    reloads.current = 0;
    exhausted.current = false;
    // Generation token for this run of the effect: a poll or ticket request
    // that settles after the watch was disabled or unmounted must not act.
    let stopped = false;
    let inflight = false;
    // A lease-gone relaunch is async and fire-and-forget: a poll that finds
    // the lease still gone while its ticket request is in flight must not
    // mint a second one (two concurrent mints burn the relaunch budget and
    // the loser can exhaust it spuriously).
    let relaunchInflight = false;
    // First non-connected poll of this outage: the in-frame retry window
    // starts here, so a disconnect only costs a re-navigation once the
    // KasmVNC client has had its chance (FX-R32).
    let lossSince = 0;
    // The first re-navigation's jitter, drawn at outage start.
    let navJitter = 0;
    // The owner the latest poll reported: a reload that fires while the
    // stream was ours must not navigate into a claim another tab made since.
    let lastOwner: string | undefined;
    // The stream epoch the latest poll reported: a NEWER epoch mid-outage
    // is the evidence gate — a claim landed, so stop re-navigating.
    let lastEpoch: number | undefined;
    // When the last re-navigation started; its claim gets CLAIM_EVIDENCE_MS.
    let lastNavAt = 0;
    // A claim that already landed gets this long to reach connected; if it
    // dies before connecting the outage resumes instead of wedging.
    let claimUntil = 0;

    const reloadFrame = () => {
      const el = opts.current.frame.current;
      if (!el) return;
      if (lastOwner !== undefined && lastOwner !== sessionTabId()) return;
      // The attempt count advances only when a reload actually runs.
      reloads.current += 1;
      lastNavAt = Date.now();
      // The session cookie on this host is bound to the live lease, so a
      // plain navigation to the workspace origin resumes the desktop.
      el.src =
        opts.current.frameUrl?.() ??
        sessionFrameUrl(opts.current.workspaceId, opts.current.sessionDomain);
      opts.current.onEvent({ type: "frame-navigated" });
    };

    const relaunch = async () => {
      relaunches.current.push(Date.now());
      relaunchInflight = true;
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
      } finally {
        relaunchInflight = false;
      }
    };

    const exhaust = () => {
      exhausted.current = true;
      opts.current.onEvent({ type: "exhausted" });
    };

    const handle = (status: ConnectionStatus) => {
      const epoch = typeof status.streamEpoch === "number" ? status.streamEpoch : 0;
      if (status.state === "connected") {
        reloads.current = 0;
        lossSince = 0;
        lastOwner = undefined;
        lastEpoch = epoch;
        claimUntil = 0;
        exhausted.current = false;
        return;
      }
      if (exhausted.current) return;
      if (status.leaseActive) {
        const owner = status.streamOwnerTab;
        lastOwner = typeof owner === "string" && owner !== "" ? owner : undefined;
        // A foreign stream owner is a fence, not a loss: the observation
        // layer verdicts "elsewhere" and the frame must never navigate into
        // the other tab's claim — a reload's client would steal it back.
        if (lastOwner !== undefined && lastOwner !== sessionTabId()) return;
        // The evidence gate: an epoch newer than the outage's own means a
        // claim landed — the last navigation's or the frame's own retry's.
        // Stop re-navigating and let it come up; if that claim never turns
        // connected it is dead and the outage resumes after the same
        // budget instead of wedging.
        if (lastEpoch !== undefined && epoch > lastEpoch) {
          claimUntil = Date.now() + CLAIM_EVIDENCE_MS;
        }
        lastEpoch = epoch;
        if (lossSince === 0) {
          lossSince = Date.now();
          navJitter = reNavJitter();
          opts.current.onEvent({ type: "recovering" });
        }
        if (Date.now() < claimUntil) return;
        if (reloads.current >= RECONNECT_BACKOFF_MS.length) {
          // Every attempt ran and the lease is still active but not
          // streaming; the last claim gets its full evidence budget too.
          if (Date.now() - lastNavAt < CLAIM_EVIDENCE_MS) return;
          exhaust();
          return;
        }
        if (reloads.current === 0) {
          // The first repair is the frame's own retry: the poll only
          // re-navigates past the in-frame window plus the first-nav
          // jitter (a fleet that loses its streams together must not all
          // reload on the same tick).
          if (Date.now() - lossSince < inFrameRetryMs + navJitter) return;
          reloadFrame();
          return;
        }
        // Later attempts wait for the last one's claim evidence.
        if (Date.now() - lastNavAt < CLAIM_EVIDENCE_MS) return;
        reloadFrame();
        return;
      }
      // The lease is gone: recover through a fresh ticket, bounded per
      // window so a dead session cannot mint tickets forever. One mint in
      // flight at a time (relaunchInflight) and none inside RELAUNCH_RETRY_MS
      // of the last — the "relaunched" dispatch needs that beat to land.
      if (relaunchInflight) return;
      const now = Date.now();
      relaunches.current = relaunches.current.filter((t) => now - t < AUTO_RELAUNCH_WINDOW_MS);
      const last = relaunches.current[relaunches.current.length - 1];
      if (last !== undefined && now - last < RELAUNCH_RETRY_MS) return;
      if (relaunches.current.length >= MAX_AUTO_RELAUNCH) {
        exhaust();
        return;
      }
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
    };
  }, [enabled, pollIntervalMs, inFrameRetryMs, reNavJitter]);
}
