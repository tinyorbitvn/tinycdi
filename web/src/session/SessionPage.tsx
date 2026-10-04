import { useCallback, useEffect, useReducer, useRef, useState, type ReactNode, type RefObject } from "react";
import { t, formatTime } from "../i18n";
import { formatDuration } from "../templates/format";
import { phaseLabelKey } from "../workspaces/helpers";
import { useApi } from "../api/context";
import { newIdempotencyKey, unwrap } from "../api/client";
import { isPortalApiError } from "../api/errors";
import { LifecycleProgress } from "../progress/LifecycleProgress";
import { opPollMs, startInFlight, withJitter } from "../progress/derive";
import { useMe } from "../app/me";
import { defaultLoginRedirect } from "../auth/AuthGate";
import type { components } from "../api/generated/schema";
import { Link, navigate } from "../lib/router";
import { Badge, type Tone } from "../design/Badge";
import { Button, buttonClass } from "../design/Button";
import { Spinner } from "../design/Spinner";
import {
  IconArrowLeft,
  IconClipboard,
  IconClock,
  IconExternalLink,
  IconMaximize,
  IconMinimize,
  IconRefresh,
} from "../design/icons";
import {
  SESSION_FRAME_SANDBOX,
  assertLaunchTarget,
  clearSessionOwned,
  markSessionOwned,
  readSessionMarker,
  sessionFrameName,
  sessionLabel,
  sessionFrameAllow,
  sessionFrameUrl,
  sessionTabId,
  submitLaunch,
  type SessionEmbedOpts,
  type SessionMarker,
} from "./launch";
import {
  initialSessionState,
  isLive,
  sessionReducer,
  type SessionState,
  type SessionStatus,
} from "./state";
import { useConnectionWatch, type ConnectionStatus, type WatchEvent } from "./useConnectionWatch";
import "./session.css";

type WorkspaceView = components["schemas"]["WorkspaceView"];
type TemplateView = components["schemas"]["TemplateView"];
type LaunchMode = "frame" | "tab";

/** Remaining running time below which the portal warns the user. */
const END_WARNING_MS = 10 * 60_000;

/** Cadence of the confirmation poll while a resumed session reconnects. */
const RESUME_POLL_MS = 1_000;

/**
 * What a /connection report says about the stream this tab is looking at:
 * `ours` (record it), `stale` (an older stream than the one we opened, keep
 * waiting), `elsewhere` (a newer stream on our lease: another tab opened it)
 * or `unknown` (not connected, or no lease/stream identity to compare).
 */
type Observation = "ours" | "stale" | "elsewhere" | "unknown";

const isUnauthenticated = (e: unknown) => isPortalApiError(e) && e.code === "UNAUTHENTICATED";

export interface SessionPageProps {
  workspaceId: string;
  /** How long the frame may take to load before the new-tab fallback is offered. */
  loadTimeoutMs?: number;
  /** Connection-status poll cadence while the session is live (D15). */
  pollIntervalMs?: number;
  /**
   * How long a resumed session (own live lease, reloaded page) may take to
   * report `connected` before the page falls back to a fresh ticket.
   */
  resumeTimeoutMs?: number;
  /** Navigates to the portal login (401). Injectable for tests. */
  onSignIn?: () => void;
}

// The in-portal session view: a full-height surface hosting the desktop in
// an iframe on the workspace's own session host (D9). Launch = ticket API,
// then a form POST whose `target` is the iframe, so the gateway's redirect
// after redemption lands inside it. There is no postMessage channel (D15):
// the passive /connection poll is the only signal, and it cannot slide the
// portal idle timer (P4/D18) — so no other polling runs while live.
export function SessionPage({
  workspaceId,
  loadTimeoutMs = 20_000,
  pollIntervalMs,
  resumeTimeoutMs = 10_000,
  onSignIn = defaultLoginRedirect,
}: SessionPageProps) {
  const api = useApi();
  const { me } = useMe();
  const sessionDomain = me?.sessionDomain ?? "";
  const [state, dispatch] = useReducer(sessionReducer, initialSessionState);
  const [workspace, setWorkspace] = useState<WorkspaceView | null>(null);
  const [pollFailures, setPollFailures] = useState(0);
  const [template, setTemplate] = useState<TemplateView | null>(null);
  const [frameKey, setFrameKey] = useState(0);
  const [fullscreen, setFullscreen] = useState(false);
  const frameRef = useRef<HTMLIFrameElement>(null);
  const stageRef = useRef<HTMLDivElement>(null);
  const actionRef = useRef<HTMLButtonElement>(null);
  const mounted = useRef(false);
  const started = useRef(false);
  const inflight = useRef(false);
  // A launch POST was submitted into the frame and its load is awaited.
  const armed = useRef(false);
  // The lease and stream epoch this tab last saw connected (also across a
  // reload, through the per-tab marker). It is what a resume is checked
  // against and what a newer stream on the same lease is measured from.
  const seen = useRef<SessionMarker | null>(readSessionMarker(workspaceId));
  // After this tab itself starts a stream (launch, resume, frame reload) the
  // next connected report is not "another tab": it is judged against what
  // was known before — a different lease, or a higher epoch, is ours.
  const pending = useRef<{ leaseRef: string; minEpoch: number } | null>(null);
  // The newest lease/stream the /connection poll has reported. Pending
  // baselines come from this, not only from the marker: the marker can lag
  // behind a stream that already claimed a higher epoch, and claiming that
  // stream as "ours" would make our own next claim look like another
  // tab's (backlog 2).
  const observed = useRef<{ leaseRef?: string | undefined; streamEpoch?: number | undefined }>(
    {},
  );
  // The watch is reloading the frame to recover the stream: keep the
  // desktop up but say "Reconnecting" instead of a stale "Connected".
  const [recovering, setRecovering] = useState(false);
  // This tab holds a live lease of its own: relaunches replace it instead of
  // asking for a takeover.
  const owned = useRef(seen.current !== null);
  const lastMode = useRef<LaunchMode>("frame");
  const loadTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  // Resume: the deadline for the confirmation poll and the poll itself.
  const resumeDeadline = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const resumePoll = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const frameName = sessionFrameName(workspaceId);

  // The workspace's clipboard policy, resolved once the template lands:
  // least privilege (no clipboard delegation) until then. Read through a
  // ref so callbacks (resume, the watch's reload) pick up a policy that
  // arrives after mount without changing their identity — a recreated
  // `loadWorkspace` would re-run the mount effect and clear the resume
  // timers mid-flight.
  const templateRef = useRef<TemplateView | null>(null);
  useEffect(() => {
    templateRef.current = template;
  }, [template]);
  const embedOpts = useCallback(
    (): SessionEmbedOpts => ({ clipboardPolicy: templateRef.current?.clipboardPolicy }),
    [],
  );

  const clearLoadTimer = () => {
    clearTimeout(loadTimer.current);
    loadTimer.current = undefined;
  };

  const clearResume = () => {
    clearTimeout(resumeDeadline.current);
    clearTimeout(resumePoll.current);
    resumeDeadline.current = undefined;
    resumePoll.current = undefined;
  };

  // The frame never fires `load` when the embed is refused or the network
  // swallows the navigation: every launch path (first, manual, watch-driven)
  // arms the same timer so the page ends in blocked/timeout, not "connecting"
  // forever.
  const armLoadTimer = useCallback(() => {
    clearTimeout(loadTimer.current);
    loadTimer.current = setTimeout(() => {
      dispatch({ type: "frame-blocked", reason: "timeout" });
    }, loadTimeoutMs);
  }, [loadTimeoutMs]);

  const remember = useCallback(
    (m: SessionMarker) => {
      seen.current = m;
      markSessionOwned(workspaceId, m);
    },
    [workspaceId],
  );

  const forget = useCallback(() => {
    seen.current = null;
    pending.current = null;
    owned.current = false;
    clearSessionOwned(workspaceId);
  }, [workspaceId]);

  // Arm pending against the observed baseline: the live lease and the
  // highest stream epoch reported on it, or the remembered marker's when
  // the poll has seen nothing newer. Only a report beyond that baseline is
  // "ours"; what is already there belongs to whoever connected before us.
  const armPending = useCallback(
    (base?: { leaseRef?: string | undefined; streamEpoch?: number | undefined }) => {
    const b = base ?? observed.current;
    const s = seen.current;
    if (b.leaseRef !== undefined && (s === null || s.leaseRef === "" || s.leaseRef !== b.leaseRef)) {
      pending.current = { leaseRef: b.leaseRef, minEpoch: b.streamEpoch ?? -1 };
      return;
    }
    pending.current = {
      leaseRef: b.leaseRef ?? s?.leaseRef ?? "",
      minEpoch: Math.max(b.streamEpoch ?? -1, s?.streamEpoch ?? -1),
    };
  }, []);

  const observe = useCallback(
    (s: ConnectionStatus): Observation => {
      const { leaseRef, streamEpoch } = s;
      if (s.state !== "connected" || !s.leaseActive) return "unknown";
      if (leaseRef === undefined || streamEpoch === undefined) return "unknown";
      // Ownership evidence settles it outright (FX-R31): the id lands on
      // the lease in the same write as the epoch, so it always names the
      // CURRENT stream's claimer. OUR id means the live stream is ours —
      // the epoch arithmetic below never sees two same-tab claims inside
      // one poll interval (a backend restart's re-claim) as a takeover.
      // A different id verdicts "elsewhere" only once this page instance's
      // own claim has had its turn: while a claim of ours is pending (the
      // provisional marker the resume/launch path armed), a foreign owner
      // just means our claim has not reached the broker yet — every reload
      // mints a fresh id that only arrives with the frame's next claim,
      // and an early verdict would flash "open in another tab" on each
      // reload (R-V3c). With nothing pending, the stream provably belongs
      // to a different tab. Absent (a legacy claim stored NULL) falls back
      // to the epochs.
      if (s.streamOwnerTab) {
        if (s.streamOwnerTab === sessionTabId()) {
          pending.current = null;
          owned.current = true;
          remember({ leaseRef, streamEpoch });
          return "ours";
        }
        if (pending.current !== null) return "unknown";
        remember({ leaseRef, streamEpoch });
        return "elsewhere";
      }
      const p = pending.current;
      if (p) {
        if (leaseRef !== p.leaseRef && p.leaseRef !== "") {
          // A different lease while we wait for our claim: neither ours
          // nor stale. The resume poll filters lease swaps on its own
          // before observe() ever runs; this branch stays for the other
          // callers (the connected baseline fetch, the watch), where a
          // pending-armed report on a different lease must not be claimed.
          return "unknown";
        }
        // streamEpoch 0 means the backend keeps no stream accounting
        // (split mode without a session directory): epochs cannot be
        // compared there, so a leaseRef match is confirmation enough
        // (backlog 3).
        if (streamEpoch === 0) {
          pending.current = null;
          owned.current = true;
          remember({ leaseRef, streamEpoch });
          return "ours";
        }
        if (leaseRef === p.leaseRef && streamEpoch <= p.minEpoch) return "stale";
        // Every claim advances the lease epoch by exactly one, so ours is
        // the very next epoch on the armed baseline — never more. A gap
        // means a stream we did not open claimed inside the window:
        // foreign (PR1b). Deliberately no +2 tolerance: a widened window
        // would absorb exactly the takeover we are trying to catch.
        // Residuals, accepted conservatively:
        //   (a) a foreign claim landing at exactly baseline+1 still reads
        //       "ours" once — indistinguishable from our own claim — then
        //       self-corrects: the next report past it flips the page to
        //       "elsewhere";
        //   (b) two claims of ours landing inside one poll interval (e.g.
        //       a reload racing itself) reads baseline+2 — a false
        //       "elsewhere" the user recovers via "Use here" rather than
        //       a silent takeover.
        // The provisional baseline (""/-1) stays exempt: its fresh
        // lease's first claim is ours by construction.
        if (p.leaseRef !== "" && streamEpoch > p.minEpoch + 1) {
          pending.current = null;
          remember({ leaseRef, streamEpoch });
          return "elsewhere";
        }
        pending.current = null;
        owned.current = true;
        remember({ leaseRef, streamEpoch });
        return "ours";
      }
      const prev = seen.current;
      if (prev && prev.leaseRef === leaseRef && streamEpoch > prev.streamEpoch) {
        // Remember the newer epoch: "Use here" must out-wait that stream.
        remember({ leaseRef, streamEpoch });
        return "elsewhere";
      }
      if (!prev || prev.leaseRef !== leaseRef) remember({ leaseRef, streamEpoch });
      return "ours";
    },
    [remember],
  );

  const fetchConnection = useCallback(async (): Promise<ConnectionStatus> => {
    const s = unwrap(
      await api.GET("/v1/workspaces/{workspaceId}/connection", {
        params: { path: { workspaceId } },
      }),
    );
    observed.current = { leaseRef: s.leaseRef, streamEpoch: s.streamEpoch };
    return s;
  }, [api, workspaceId]);

  // A 401 means the portal login is gone; every API call from here on fails
  // the same way, so say so instead of showing a stale badge or a generic error.
  const fail = useCallback((e: unknown) => {
    dispatch(isUnauthenticated(e) ? { type: "signed-out" } : { type: "failed", error: e });
  }, []);

  const launch = useCallback(
    async (mode: LaunchMode, takeover: boolean) => {
      if (inflight.current) return;
      inflight.current = true;
      lastMode.current = mode;
      clearLoadTimer();
      clearResume();
      armed.current = false;
      if (mode === "frame") dispatch({ type: "request" });
      try {
        const ticket = unwrap(
          await api.POST("/v1/workspaces/{workspaceId}/connections", {
            params: { path: { workspaceId } },
            body: { takeover },
          }),
        );
        if (!mounted.current) return;
        assertLaunchTarget(ticket, workspaceId, sessionDomain);
        owned.current = true;
        if (mode === "tab") {
          // The new tab holds the lease now; a reload of this one must not
          // try to resume it.
          forget();
          submitLaunch(ticket, "_blank", workspaceId, sessionDomain);
          // The new tab takes the lease over: drop the embedded desktop.
          setFrameKey((k) => k + 1);
          dispatch({ type: "external" });
          return;
        }
        // The ticket created a lease this tab has not seen yet: mark it
        // provisionally so a reload inside this window resumes instead of
        // asking to take over its own session (backlog 1). The pending
        // baseline accepts the first connected report — a fresh ticket can
        // only have made this tab's stream.
        remember({ leaseRef: "", streamEpoch: -1 });
        pending.current = { leaseRef: "", minEpoch: -1 };
        armed.current = true;
        submitLaunch(ticket, frameName, workspaceId, sessionDomain);
        dispatch({ type: "ticket" });
        armLoadTimer();
      } catch (e) {
        armed.current = false;
        if (!mounted.current) return;
        if (isPortalApiError(e) && e.code === "CONNECTION_IN_USE") dispatch({ type: "in-use" });
        else fail(e);
      } finally {
        inflight.current = false;
      }
    },
    [api, workspaceId, frameName, sessionDomain, armLoadTimer, fail, forget],
  );

  // Resume our own live session after a reload or an in-portal round trip:
  // the host-only session cookie is still valid, so pointing the frame at the
  // workspace origin brings the desktop back without a ticket (and without
  // the "in use - take over?" dialog against ourselves). It is attempted only
  // when /connection reports the very lease this tab last saw connected: a
  // different lease means someone took the session over, and the frame is not
  // loaded. The /connection poll confirms our new stream; if it does not
  // within resumeTimeoutMs the page asks for a ticket WITHOUT takeover, so a
  // still-held lease shows the dialog instead of being replaced silently.
  // Resolves true when it took over the start-up flow.
  const resume = useCallback(async (): Promise<boolean> => {
    const marker = seen.current;
    if (!marker) return false;
    let status: ConnectionStatus;
    try {
      status = await fetchConnection();
    } catch (e) {
      if (!isUnauthenticated(e)) return false;
      if (mounted.current) dispatch({ type: "signed-out" });
      return true;
    }
    if (!mounted.current) return true;
    const el = frameRef.current;
    if (!status.leaseActive || !el) return false;
    // The marker names the lease this tab last owned; "" is the provisional
    // marker a launch writes (accept the live lease — our ticket made it).
    // A different live leaseRef is a stale marker: this tab's lease is gone
    // — but the shared session cookie may still open a stream on the live
    // one (a duplicate tab of this browser took over or re-launched).
    // Resume by navigation either way and let the poll prove a NEW stream:
    // the stream already on the lease is never "ours" — only a claim beyond
    // the observed epoch is (backlog 2). When the cookie is not bound to
    // the live lease (another browser holds it), the frame opens no stream
    // and the deadline below falls back to a ticket without takeover.
    const own = marker.leaseRef !== "" && status.leaseRef === marker.leaseRef;
    if (!own) {
      forget();
    }

    clearResume();
    armed.current = false;
    pending.current = {
      leaseRef: status.leaseRef ?? "",
      minEpoch: own
        ? Math.max(status.streamEpoch ?? marker.streamEpoch, marker.streamEpoch)
        : (status.streamEpoch ?? -1),
    };
    dispatch({ type: "resume" });
    el.src = sessionFrameUrl(workspaceId, sessionDomain, embedOpts());
    resumeDeadline.current = setTimeout(() => {
      clearResume();
      void launch("frame", false);
    }, resumeTimeoutMs);
    const pollMs = Math.min(pollIntervalMs ?? RESUME_POLL_MS, RESUME_POLL_MS);
    const poll = async () => {
      try {
        const s = await fetchConnection();
        if (!mounted.current || resumeDeadline.current === undefined) return;
        if (!s.leaseActive) {
          // The lease vanished while we waited: nothing left to resume.
          clearResume();
          void launch("frame", false);
          return;
        }
        // A different lease appeared while we waited (the frame is on
        // `target`, the lease we decided to resume onto): taken over.
        const target = pending.current?.leaseRef;
        if (target !== undefined && target !== "" && s.leaseRef !== undefined && s.leaseRef !== target) {
          clearResume();
          forget();
          void launch("frame", false);
          return;
        }
        if (s.state === "connected") {
          const obs = observe(s);
          // Only OUR claim confirms the resume: a foreign stream claiming
          // a newer epoch inside the resume window ("elsewhere") is a
          // takeover, not our confirmation — show it, don't claim it.
          if (obs === "elsewhere") {
            clearResume();
            dispatch({ type: "elsewhere" });
            return;
          }
          if (obs === "ours") {
            clearResume();
            dispatch({ type: "resumed" });
            return;
          }
        }
      } catch (e) {
        if (!mounted.current || resumeDeadline.current === undefined) return;
        if (isUnauthenticated(e)) {
          clearResume();
          dispatch({ type: "signed-out" });
          return;
        }
      }
      resumePoll.current = setTimeout(() => void poll(), pollMs);
    };
    resumePoll.current = setTimeout(() => void poll(), pollMs);
    return true;
  }, [
    fetchConnection,
    launch,
    observe,
    embedOpts,
    forget,
    workspaceId,
    sessionDomain,
    resumeTimeoutMs,
    pollIntervalMs,
  ]);

  // "Use here": the same lease, so no ticket is needed — resume points this
  // tab's frame back at the session, which the other tab will then see as a
  // newer stream and step aside for.
  const useHere = useCallback(async () => {
    if (!(await resume()) && mounted.current) void launch("frame", false);
  }, [resume, launch]);

  const relaunch = useCallback(
    (mode: LaunchMode) => void launch(mode, owned.current),
    [launch],
  );

  // One-shot workspace lookup to refine a failed/expired session into an
  // "ended" reason — never on an interval, so it cannot slide the idle
  // timer (D18).
  const checkEnded = useCallback(async () => {
    try {
      const ws = unwrap(
        await api.GET("/v1/workspaces/{workspaceId}", { params: { path: { workspaceId } } }),
      );
      if (!mounted.current) return;
      setWorkspace(ws);
      const end = endReason(ws);
      if (end) dispatch({ type: "ended", reason: end });
    } catch (e) {
      if (!mounted.current) return;
      if (isUnauthenticated(e)) dispatch({ type: "signed-out" });
      else if (isPortalApiError(e) && e.code === "NOT_FOUND") {
        dispatch({ type: "ended", reason: "deleted" });
      }
    }
  }, [api, workspaceId]);

  // D15: the passive /connection poll is the disconnect signal. Lease still
  // active → reload the frame with bounded backoff; lease gone → relaunch
  // through a fresh ticket, at most MAX_AUTO_RELAUNCH per 5 minutes.
  useConnectionWatch({
    workspaceId,
    sessionDomain,
    frameName,
    frame: frameRef,
    active: state.status === "connected",
    pollIntervalMs,
    fetchStatus: fetchConnection,
    frameUrl: useCallback(
      () => sessionFrameUrl(workspaceId, sessionDomain, embedOpts()),
      [workspaceId, sessionDomain, embedOpts],
    ),
    onObserve: useCallback(
      (status: ConnectionStatus) => {
        if (status.state === "connected") setRecovering(false);
        if (observe(status) === "elsewhere") dispatch({ type: "elsewhere" });
      },
      [observe],
    ),
    requestTicket: useCallback(
      async () =>
        // The lease is gone, so no takeover is needed; if the poll raced a
        // still-live lease the 409 surfaces as relaunch-error and the next
        // window entry retries.
        unwrap(
          await api.POST("/v1/workspaces/{workspaceId}/connections", {
            params: { path: { workspaceId } },
            body: { takeover: false },
          }),
        ),
      [api, workspaceId],
    ),
    onEvent: useCallback(
      (ev: WatchEvent) => {
        switch (ev.type) {
          case "relaunched":
            // A fresh ticket made a lease we have not observed: same as a
            // launch — mark provisionally so a reload resumes it, and let
            // the first connected report be ours.
            remember({ leaseRef: "", streamEpoch: -1 });
            pending.current = { leaseRef: "", minEpoch: -1 };
            armed.current = true;
            dispatch({ type: "ticket" });
            armLoadTimer();
            break;
          case "exhausted":
            setRecovering(false);
            dispatch({ type: "offline", reason: "exhausted" });
            void checkEnded();
            break;
          case "relaunch-error":
            if (isUnauthenticated(ev.error)) {
              dispatch({ type: "signed-out" });
            } else if (isPortalApiError(ev.error) && ev.error.code === "NOT_FOUND") {
              dispatch({ type: "ended", reason: "deleted" });
            } else if (isPortalApiError(ev.error) && ev.error.code === "INVALID_STATE") {
              void checkEnded();
            }
            break;
          case "poll-error":
            if (isUnauthenticated(ev.error)) {
              dispatch({ type: "signed-out" });
            } else if (isPortalApiError(ev.error) && ev.error.code === "NOT_FOUND") {
              dispatch({ type: "ended", reason: "deleted" });
            }
            break;
          case "frame-navigated":
            // Our own reload opens a new stream: not another tab's. The
            // badge says "Reconnecting" until the poll sees it.
            armPending();
            setRecovering(true);
            break;
        }
      },
      [checkEnded, armLoadTimer, armPending, remember],
    ),
  });

  // Connectable workspace → the first-load launch path: resume our own
  // lease when there is one, else request a ticket. Used by the initial
  // load and by the starting-state poll when the workspace turns Ready.
  const beginConnect = useCallback(() => {
    // Resume first; request a ticket only when there is nothing to resume.
    void resume().then((resuming) => {
      if (!resuming && mounted.current) void launch("frame", false);
    });
  }, [resume, launch]);

  const loadWorkspace = useCallback(async () => {
    try {
      const ws = unwrap(
        await api.GET("/v1/workspaces/{workspaceId}", { params: { path: { workspaceId } } }),
      );
      if (!mounted.current) return;
      setWorkspace(ws);
      const blocker = connectBlocker(ws);
      // V3.27: a workspace mid start/create keeps polling on this page and
      // shows the step list ("starting"); stopped/failed/blocked stay on the
      // static not-ready overlay.
      dispatch(
        blocker
          ? { type: "workspace", connectable: false, reason: blocker, starting: startInFlight(ws) }
          : { type: "workspace", connectable: true },
      );
      if (!blocker) beginConnect();
      void loadTemplate(api, ws).then((tpl) => mounted.current && setTemplate(tpl));
    } catch (e) {
      if (mounted.current) fail(e);
    }
  }, [api, workspaceId, beginConnect, fail]);

  // Initial load + automatic first launch. Guarded so React StrictMode's
  // double effect run does not mint two tickets. The launch waits for
  // /v1/me: without the session domain the launch target cannot be pinned.
  useEffect(() => {
    mounted.current = true;
    if (!started.current && me !== null) {
      started.current = true;
      void loadWorkspace();
    }
    return () => {
      mounted.current = false;
      clearLoadTimer();
      clearResume();
    };
  }, [loadWorkspace, me]);

  // "starting" (V3.27): the workspace is mid start/create, so poll GET
  // /v1/workspaces/{id} — the step list needs phase + conditions, not the
  // /connection stream status. The poll stops when the view becomes
  // connectable (the usual launch path then runs exactly once), when the
  // intent settles (Failed/Stopped → not-ready), or on 401/404. It never
  // runs while connected (D15).
  useEffect(() => {
    if (state.status !== "starting") return;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let failures = 0;
    let delay = 1_000;
    const tick = async () => {
      try {
        const ws = unwrap(
          await api.GET("/v1/workspaces/{workspaceId}", { params: { path: { workspaceId } } }),
        );
        if (cancelled || !mounted.current) return;
        failures = 0;
        setPollFailures(0);
        setWorkspace(ws);
        const blocker = connectBlocker(ws);
        if (!blocker) {
          dispatch({ type: "workspace", connectable: true });
          beginConnect();
          return;
        }
        const starting = startInFlight(ws);
        dispatch({ type: "workspace", connectable: false, starting, reason: blocker });
        if (!starting) return;
        timer = setTimeout(
          () => void tick(),
          withJitter(opPollMs(Date.parse(ws.updatedAt))),
        );
      } catch (e) {
        if (cancelled || !mounted.current) return;
        if (isUnauthenticated(e)) {
          dispatch({ type: "signed-out" });
          return;
        }
        if (isPortalApiError(e) && e.code === "NOT_FOUND") {
          dispatch({ type: "ended", reason: "deleted" });
          return;
        }
        failures += 1;
        setPollFailures(failures);
        delay = Math.min(delay * 2 ** failures, 30_000);
        timer = setTimeout(() => void tick(), delay);
      }
    };
    timer = setTimeout(() => void tick(), delay);
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [state.status, api, workspaceId, beginConnect]);

  // The stopped (or failed) workspace on this page offers Start: post the
  // intent, then let the starting-state poll pick the workspace up. On an
  // INVALID_STATE race the reload re-derives the real position.
  const startWorkspace = useCallback(async () => {
    try {
      const ws = unwrap(
        await api.POST("/v1/workspaces/{workspaceId}/start", {
          params: {
            path: { workspaceId },
            header: { "Idempotency-Key": newIdempotencyKey() },
          },
        }),
      );
      if (!mounted.current) return;
      setWorkspace(ws);
      const blocker = connectBlocker(ws);
      dispatch(
        blocker
          ? { type: "workspace", connectable: false, reason: blocker, starting: startInFlight(ws) }
          : { type: "workspace", connectable: true },
      );
      if (!blocker) beginConnect();
    } catch (e) {
      if (!mounted.current) return;
      if (isPortalApiError(e) && e.code === "INVALID_STATE") void loadWorkspace();
      else fail(e);
    }
  }, [api, workspaceId, beginConnect, loadWorkspace, fail]);

  // The lease is no longer (or no longer only) ours: a reload must not try
  // to resume it.
  useEffect(() => {
    if (state.status === "ended" || state.status === "external") forget();
  }, [state.status, forget]);

  // "Reconnecting" only qualifies the live badge; leaving connected clears it.
  useEffect(() => {
    if (state.status !== "connected") setRecovering(false);
  }, [state.status]);

  // A freshly launched session has no baseline yet: take one right away
  // instead of after the first watch poll, so a reload in between still finds
  // its lease. (A resume already recorded one when it saw the stream.)
  useEffect(() => {
    if (state.status !== "connected" || pending.current === null) return;
    let cancelled = false;
    fetchConnection().then(
      (s) => {
        if (!cancelled && observe(s) === "elsewhere") dispatch({ type: "elsewhere" });
      },
      () => undefined, // the watch's own poll reports API failures
    );
    return () => {
      cancelled = true;
    };
  }, [state.status, fetchConnection, observe]);

  // The portal's own CSP refusing the frame (frame-src) or the launch POST
  // (form-action) is the one embedding failure the portal can observe
  // directly; a refusal on the session side (frame-ancestors) only shows up
  // as a frame that never loads, which the load timer catches.
  useEffect(() => {
    const onViolation = (e: SecurityPolicyViolationEvent) => {
      if (sessionDomain === "") return;
      let blockedHost: string;
      try {
        blockedHost = new URL(e.blockedURI).host;
      } catch {
        return;
      }
      if (blockedHost !== `${sessionLabel(workspaceId)}.${sessionDomain}`) return;
      if (e.effectiveDirective === "frame-src" || e.effectiveDirective === "child-src") {
        clearLoadTimer();
        armed.current = false;
        dispatch({ type: "frame-blocked", reason: "frame-src" });
      } else if (e.effectiveDirective === "form-action") {
        clearLoadTimer();
        armed.current = false;
        dispatch({
          type: "failed",
          error: new Error("The portal's security policy blocked the launch request."),
        });
      }
    };
    const onOffline = () => dispatch({ type: "offline", reason: "offline" });
    document.addEventListener("securitypolicyviolation", onViolation);
    window.addEventListener("offline", onOffline);
    return () => {
      document.removeEventListener("securitypolicyviolation", onViolation);
      window.removeEventListener("offline", onOffline);
    };
  }, [workspaceId, sessionDomain]);

  useEffect(() => {
    const onChange = () => setFullscreen(document.fullscreenElement === stageRef.current);
    document.addEventListener("fullscreenchange", onChange);
    return () => document.removeEventListener("fullscreenchange", onChange);
  }, []);

  // Keyboard focus follows the session: into the desktop once it is up, onto
  // the overlay's primary action whenever the user has to decide something.
  useEffect(() => {
    if (state.status === "connected") frameRef.current?.focus();
    else actionRef.current?.focus();
  }, [state.status]);

  function onFrameLoad() {
    if (!armed.current) return;
    // A fresh frame's initial about:blank is same-origin and readable; the
    // session document is cross-origin, so reading its location throws.
    try {
      if (frameRef.current?.contentWindow?.location.href === "about:blank") return;
    } catch {
      /* cross-origin: the session origin answered */
    }
    armed.current = false;
    clearLoadTimer();
    dispatch({ type: "frame-loaded" });
  }

  async function toggleFullscreen() {
    try {
      if (document.fullscreenElement) await document.exitFullscreen();
      else await stageRef.current?.requestFullscreen();
    } catch {
      /* refused (no user activation / policy): stay as we are */
    }
    frameRef.current?.focus();
  }

  const canFullscreen = typeof document !== "undefined" && document.fullscreenEnabled === true;
  const name = workspace?.name ?? workspaceId;
  const connected = state.status === "connected";
  const busy = state.status === "requesting" || state.status === "loading";
  const canLaunch = workspace !== null && state.status !== "not-ready" && !busy;

  return (
    <section className="tc-session" aria-label={t("session.page.ariaLabel", { name })}>
      <div className="tc-session__toolbar">
        <Link to="/" className={buttonClass("ghost", "sm")}>
          <IconArrowLeft aria-hidden="true" />
          <span className="tc-button__label">{t("session.toolbar.back")}</span>
        </Link>
        <h1 className="tc-session__title">{name}</h1>
        <StatusBadge status={state.status} recovering={recovering} />
        <LifecycleNotice workspace={workspace} template={template} live={isLive(state.status)} />
        <div className="tc-session__controls" role="group" aria-label={t("session.toolbar.controls")}>
          <span className="tc-session__printhint">{t("session.toolbar.printHint")}</span>
          <ClipboardHint template={template} />
          <Button
            size="sm"
            variant="ghost"
            icon={<IconRefresh aria-hidden="true" />}
            disabled={!canLaunch}
            onClick={() => relaunch("frame")}
          >
            {t("session.toolbar.reconnect")}
          </Button>
          <Button
            size="sm"
            variant="ghost"
            icon={<IconExternalLink aria-hidden="true" />}
            disabled={!canLaunch}
            onClick={() => relaunch("tab")}
          >
            {t("session.toolbar.openInNewTab")}
          </Button>
          <Button
            size="sm"
            variant="ghost"
            icon={fullscreen ? <IconMinimize aria-hidden="true" /> : <IconMaximize aria-hidden="true" />}
            aria-pressed={fullscreen}
            disabled={!canFullscreen}
            onClick={() => void toggleFullscreen()}
          >
            {fullscreen ? t("session.toolbar.exitFullscreen") : t("session.toolbar.fullscreen")}
          </Button>
        </div>
      </div>
      <div className="tc-session__stage" ref={stageRef} data-status={state.status}>
        <iframe
          key={frameKey}
          ref={frameRef}
          name={frameName}
          title={t("session.frame.title", { name })}
          className="tc-session__frame"
          sandbox={SESSION_FRAME_SANDBOX}
          allow={sessionFrameAllow(workspaceId, sessionDomain, {
            clipboardPolicy: template?.clipboardPolicy,
          })}
          aria-describedby="tc-session-keyboard-hint"
          inert={!connected}
          onLoad={onFrameLoad}
        />
        <p id="tc-session-keyboard-hint" className="tc-sr-only">
          {t("session.frame.keyboardHint")}
        </p>
        {connected ? null : (
          <Overlay
            state={state}
            workspaceId={workspaceId}
            actionRef={actionRef}
            onRetry={() => (workspace ? relaunch("frame") : void loadWorkspace())}
            onNewTab={() => relaunch("tab")}
            onTakeover={() => void launch(lastMode.current, true)}
            onUseHere={() => void useHere()}
            onSignIn={onSignIn}
            workspace={workspace}
            pollFailures={pollFailures}
            onStart={() => void startWorkspace()}
          />
        )}
      </div>
    </section>
  );
}

const STATUS_KEY: Record<SessionStatus, [Parameters<typeof t>[0], Tone]> = {
  loading: ["session.status.loading", "neutral"],
  "not-ready": ["session.status.notReady", "warning"],
  starting: ["session.status.starting", "info"],
  requesting: ["session.status.requesting", "info"],
  "in-use": ["session.status.inUse", "warning"],
  connecting: ["session.status.connecting", "info"],
  connected: ["session.status.connected", "success"],
  elsewhere: ["session.status.elsewhere", "warning"],
  disconnected: ["session.status.disconnected", "warning"],
  ended: ["session.status.ended", "neutral"],
  blocked: ["session.status.blocked", "danger"],
  external: ["session.status.external", "neutral"],
  "signed-out": ["session.status.signedOut", "warning"],
  error: ["session.status.error", "danger"],
};

function StatusBadge({ status, recovering }: { status: SessionStatus; recovering?: boolean }) {
  const [key, tone] = STATUS_KEY[status];
  const pulse = status === "requesting" || status === "connecting" || status === "starting";
  // A watch-driven frame reload keeps the session nominally connected while
  // the desktop stream is re-established: say so instead of a stale
  // "Connected" (T5.4).
  const reconnecting = status === "connected" && recovering === true;
  return (
    <span role="status" aria-live="polite" className="tc-session__status">
      <Badge tone={reconnecting ? "info" : tone} dot pulse={pulse || reconnecting}>
        {reconnecting ? t("session.status.reconnecting") : t(key)}
      </Badge>
    </span>
  );
}

function Overlay({
  state,
  workspaceId,
  actionRef,
  onRetry,
  onNewTab,
  onTakeover,
  onUseHere,
  onSignIn,
  workspace,
  pollFailures,
  onStart,
}: {
  state: SessionState;
  workspaceId: string;
  actionRef: RefObject<HTMLButtonElement | null>;
  onRetry: () => void;
  onNewTab: () => void;
  onTakeover: () => void;
  onUseHere: () => void;
  onSignIn: () => void;
  workspace: WorkspaceView | null;
  pollFailures: number;
  onStart: () => void;
}) {
  // A stopped or failed workspace can be started right here — no trip back
  // to the dashboard (V3.27).
  const offerStart =
    workspace !== null && (workspace.phase === "Stopped" || workspace.phase === "Failed");
  const back = (
    <Link to="/" className={buttonClass("secondary", "md")}>
      {t("session.toolbar.back")}
    </Link>
  );
  const details = (
    <Link to={`/workspaces/${encodeURIComponent(workspaceId)}`} className={buttonClass("ghost", "md")}>
      {t("session.overlay.workspaceDetails")}
    </Link>
  );
  switch (state.status) {
    case "loading":
    case "requesting":
    case "connecting":
      return (
        <OverlayPanel tone="progress" title={progressTitle(state.status)}>
          <Spinner size="lg" decorative />
          {state.status === "connecting" ? (
            <p className="tc-session__hint">
              {t("session.progress.newTabHint")}{" "}
              <button type="button" className="tc-session__link" onClick={onNewTab}>
                {t("session.progress.newTabAction")}
              </button>
            </p>
          ) : null}
        </OverlayPanel>
      );
    case "in-use":
      return (
        <OverlayPanel
          tone="dialog"
          title={t("session.inUse.title")}
          actions={
            <>
              <Button ref={actionRef} variant="primary" onClick={onTakeover}>
                {t("session.inUse.takeover")}
              </Button>
              <Button onClick={() => navigate("/")}>{t("common.cancel")}</Button>
            </>
          }
        >
          <p>{t("session.inUse.body")}</p>
        </OverlayPanel>
      );
    case "elsewhere":
      return (
        <OverlayPanel
          tone="dialog"
          title={t("session.elsewhere.title")}
          actions={
            <>
              <Button ref={actionRef} variant="primary" onClick={onUseHere}>
                {t("session.elsewhere.useHere")}
              </Button>
              {back}
            </>
          }
        >
          <p>{t("session.elsewhere.body")}</p>
        </OverlayPanel>
      );
    case "starting":
      // The lifecycle step panel, centred on the stage; the poll in the
      // page body keeps it fresh and hands off to the launch path the
      // moment the workspace turns connectable.
      return (
        <div className="tc-session__overlay" data-tone="progress">
          {workspace ? (
            <LifecycleProgress
              workspace={workspace}
              variant="overlay"
              refreshError={pollFailures > 0 ? pollFailures : undefined}
            />
          ) : (
            <Spinner size="lg" decorative />
          )}
        </div>
      );
    case "not-ready":
      return (
        <OverlayPanel
          tone="alert"
          title={t("session.notReady.title")}
          actions={
            <>
              {offerStart ? (
                <Button ref={actionRef} variant="primary" onClick={onStart}>
                  {workspace?.phase === "Failed"
                    ? t("workspaces.detail.action.retryStart")
                    : t("workspaces.detail.action.start")}
                </Button>
              ) : null}
              {details}
              {back}
            </>
          }
        >
          <p>{state.reason ?? t("session.notReady.fallback")}</p>
        </OverlayPanel>
      );
    case "disconnected":
      return (
        <OverlayPanel
          tone="alert"
          title={t("session.disconnected.title")}
          actions={
            <>
              <Button ref={actionRef} variant="primary" onClick={onRetry}>
                {t("session.disconnected.reconnect")}
              </Button>
              {back}
            </>
          }
        >
          <p>
            {state.reason === "offline"
              ? t("session.disconnected.offline")
              : state.reason === "exhausted"
                ? t("session.disconnected.exhausted")
                : t("session.disconnected.unreachable")}
          </p>
        </OverlayPanel>
      );
    case "ended":
      return (
        <OverlayPanel
          tone="alert"
          title={t("session.ended.title")}
          actions={
            <>
              {state.reason === "stopped" && offerStart ? (
                <Button ref={actionRef} variant="primary" onClick={onStart}>
                  {t("workspaces.detail.action.start")}
                </Button>
              ) : null}
              {back}
              {details}
            </>
          }
        >
          <p>{endedText(state.reason)}</p>
        </OverlayPanel>
      );
    case "blocked":
      return (
        <OverlayPanel
          tone="alert"
          title={t("session.blocked.title")}
          actions={
            <>
              <Button ref={actionRef} variant="primary" icon={<IconExternalLink aria-hidden="true" />} onClick={onNewTab}>
                {t("session.toolbar.openInNewTab")}
              </Button>
              <Button onClick={onRetry}>{t("session.blocked.retry")}</Button>
            </>
          }
        >
          <p>
            {state.reason === "timeout"
              ? t("session.blocked.timeout")
              : t("session.blocked.policy")}{" "}
            {t("session.blocked.newTabHint")}
          </p>
        </OverlayPanel>
      );
    case "external":
      return (
        <OverlayPanel
          tone="info"
          title={t("session.external.title")}
          actions={
            <>
              <Button ref={actionRef} variant="primary" onClick={onRetry}>
                {t("session.external.showHere")}
              </Button>
              {back}
            </>
          }
        >
          <p>{t("session.external.body")}</p>
        </OverlayPanel>
      );
    case "signed-out":
      return (
        <OverlayPanel
          tone="alert"
          title={t("session.signedOut.title")}
          actions={
            <Button ref={actionRef} variant="primary" onClick={onSignIn}>
              {t("session.signedOut.action")}
            </Button>
          }
        >
          <p>{t("session.signedOut.body")}</p>
        </OverlayPanel>
      );
    case "error":
      return (
        <OverlayPanel
          tone="alert"
          title={t("session.error.title")}
          actions={
            <>
              <Button ref={actionRef} variant="primary" icon={<IconRefresh aria-hidden="true" />} onClick={onRetry}>
                {t("session.error.retry")}
              </Button>
              {back}
            </>
          }
        >
          <ErrorText error={state.error} />
        </OverlayPanel>
      );
    case "connected":
      return null;
  }
}

function progressTitle(s: SessionStatus): string {
  if (s === "loading") return t("session.progress.loading");
  if (s === "requesting") return t("session.progress.requesting");
  return t("session.progress.connecting");
}

function endedText(reason: string | undefined): string {
  if (reason === "stopped") return t("session.ended.stopped");
  if (reason === "failed") return t("session.ended.failed");
  if (reason === "deleted") return t("session.ended.deleted");
  return t("session.ended.unknown");
}

function OverlayPanel({
  tone,
  title,
  children,
  actions,
}: {
  tone: "progress" | "dialog" | "alert" | "info";
  title: string;
  children?: ReactNode;
  actions?: ReactNode;
}) {
  const titleId = "tc-session-overlay-title";
  const role = tone === "dialog" ? "alertdialog" : tone === "alert" ? "alert" : "status";
  return (
    <div className="tc-session__overlay" data-tone={tone}>
      <div className="tc-session__panel" role={role} aria-labelledby={titleId}>
        <h2 id={titleId} className="tc-session__panel-title">
          {title}
        </h2>
        {children}
        {actions ? <div className="tc-session__actions">{actions}</div> : null}
      </div>
    </div>
  );
}

const API_ERROR_KEY: Partial<Record<string, Parameters<typeof t>[0]>> = {
  INVALID_STATE: "session.error.api.invalidState",
  FORBIDDEN: "session.error.api.forbidden",
  NOT_FOUND: "session.error.api.notFound",
  RATE_LIMITED: "session.error.api.rateLimited",
  UNAUTHENTICATED: "session.error.api.unauthenticated",
  CSRF_FAILED: "session.error.api.csrfFailed",
};

function ErrorText({ error }: { error: unknown }) {
  if (isPortalApiError(error)) {
    return (
      <>
        <p>{API_ERROR_KEY[error.code] ? t(API_ERROR_KEY[error.code]!) : t("session.error.api.fallback")}</p>
        {error.requestId ? (
          <p className="tc-session__meta">{t("session.error.requestId", { id: error.requestId })}</p>
        ) : null}
      </>
    );
  }
  return <p>{error instanceof Error ? error.message : t("session.error.generic")}</p>;
}

function LifecycleNotice({
  workspace,
  template,
  live,
}: {
  workspace: WorkspaceView | null;
  template: TemplateView | null;
  live: boolean;
}) {
  const [now, setNow] = useState(() => Date.now());
  const endsAt = workspace && template ? runningDeadline(workspace, template) : null;
  useEffect(() => {
    if (endsAt === null || !live) return;
    const t0 = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(t0);
  }, [endsAt, live]);
  if (!template) return null;
  const { idleTimeoutSeconds } = template.lifecycleDefaults;
  const remaining = endsAt === null ? null : endsAt - now;
  const warn = live && remaining !== null && remaining > 0 && remaining <= END_WARNING_MS;
  return (
    <span className="tc-session__notice" data-warn={warn || undefined}>
      <IconClock aria-hidden="true" />
      <span>
        {t("session.lifecycle.stopsAfter", { duration: formatDuration(idleTimeoutSeconds) })}
        {endsAt !== null ? ` · ${t("session.lifecycle.endsBy", { time: formatTime(endsAt) })}` : ""}
      </span>
      {warn ? (
        <span role="alert" className="tc-session__warning">
          {t("session.lifecycle.warn", { minutes: Math.max(1, Math.ceil(remaining / 60_000)) })}
        </span>
      ) : null}
    </span>
  );
}

function ClipboardHint({ template }: { template: TemplateView | null }) {
  const disabled = template?.clipboardPolicy === "Disabled";
  return (
    <details className="tc-session__clipboard">
      <summary className={buttonClass("ghost", "sm")}>
        <IconClipboard aria-hidden="true" />
        <span className="tc-button__label">{t("session.clipboard.label")}</span>
      </summary>
      <div className="tc-session__popover">
        {disabled ? (
          <p>{t("session.clipboard.disabled")}</p>
        ) : (
          <>
            <p>{t("session.clipboard.hint1")}</p>
            <p>{t("session.clipboard.hint2")}</p>
          </>
        )}
      </div>
    </details>
  );
}

// ---- helpers ----

/** Why the workspace cannot take a connection now, or null when it can. */
export function connectBlocker(ws: WorkspaceView): string | null {
  if (ws.desiredState !== "Running") return t("session.blocker.stopped");
  if (ws.phase !== "Ready") {
    return t("session.blocker.phase", { phase: t(phaseLabelKey(ws.phase)) });
  }
  const conn = ws.conditions.find((c) => c.type === "ConnectionReady");
  if (conn?.status !== "True") return t("session.blocker.connection");
  return null;
}

/** Why a live session's workspace is gone, or null while it still runs. */
export function endReason(ws: WorkspaceView): string | null {
  if (ws.phase === "Terminating") return "deleted";
  if (ws.phase === "Failed") return "failed";
  if (ws.desiredState === "Stopped" || ws.phase === "Stopping" || ws.phase === "Stopped") {
    return "stopped";
  }
  return null;
}

/**
 * Approximate end of the running incarnation: the RuntimeReady transition
 * plus the template's maxRunningSeconds. The API exposes no explicit
 * deadline, so this is a hint, not a promise.
 */
export function runningDeadline(ws: WorkspaceView, tpl: TemplateView): number | null {
  const ready = ws.conditions.find((c) => c.type === "RuntimeReady" && c.status === "True");
  const since = ready ? Date.parse(ready.lastTransitionTime) : NaN;
  if (Number.isNaN(since)) return null;
  return since + tpl.lifecycleDefaults.maxRunningSeconds * 1000;
}

async function loadTemplate(
  api: ReturnType<typeof useApi>,
  ws: WorkspaceView,
): Promise<TemplateView | null> {
  try {
    const list = unwrap(await api.GET("/v1/templates", {}));
    return list.items.find((tpl) => tpl.id === ws.template.id) ?? null;
  } catch {
    return null; // notices are best-effort
  }
}

