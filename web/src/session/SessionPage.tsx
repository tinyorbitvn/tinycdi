import { useCallback, useEffect, useReducer, useRef, useState, type ReactNode, type RefObject } from "react";
import { t } from "../i18n";
import { useApi } from "../api/context";
import { unwrap } from "../api/client";
import { isPortalApiError } from "../api/errors";
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
  submitLaunch,
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
  // This tab holds a live lease of its own: relaunches replace it instead of
  // asking for a takeover.
  const owned = useRef(seen.current !== null);
  const lastMode = useRef<LaunchMode>("frame");
  const loadTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  // Resume: the deadline for the confirmation poll and the poll itself.
  const resumeDeadline = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const resumePoll = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const frameName = sessionFrameName(workspaceId);

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
    clearSessionOwned(workspaceId);
  }, [workspaceId]);

  const armPending = useCallback(() => {
    pending.current = seen.current
      ? { leaseRef: seen.current.leaseRef, minEpoch: seen.current.streamEpoch }
      : { leaseRef: "", minEpoch: -1 };
  }, []);

  const observe = useCallback(
    (s: ConnectionStatus): Observation => {
      const { leaseRef, streamEpoch } = s;
      if (s.state !== "connected" || !s.leaseActive) return "unknown";
      if (leaseRef === undefined || streamEpoch === undefined) return "unknown";
      const p = pending.current;
      if (p) {
        if (leaseRef === p.leaseRef && streamEpoch <= p.minEpoch) return "stale";
        pending.current = null;
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

  const fetchConnection = useCallback(
    async (): Promise<ConnectionStatus> =>
      unwrap(
        await api.GET("/v1/workspaces/{workspaceId}/connection", {
          params: { path: { workspaceId } },
        }),
      ),
    [api, workspaceId],
  );

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
        armPending();
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
    [api, workspaceId, frameName, sessionDomain, armLoadTimer, armPending, fail, forget],
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
    if (status.leaseRef !== marker.leaseRef) {
      // Not the lease this tab had: the marker proves nothing any more.
      forget();
      owned.current = false;
      return false;
    }

    clearResume();
    armed.current = false;
    armPending();
    dispatch({ type: "resume" });
    el.src = sessionFrameUrl(workspaceId, sessionDomain);
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
        if (s.leaseRef !== undefined && s.leaseRef !== marker.leaseRef) {
          // Taken over while we waited.
          clearResume();
          forget();
          owned.current = false;
          void launch("frame", false);
          return;
        }
        if (s.state === "connected" && observe(s) !== "stale") {
          clearResume();
          dispatch({ type: "resumed" });
          return;
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
    armPending,
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
    onObserve: useCallback(
      (status: ConnectionStatus) => {
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
            armPending();
            armed.current = true;
            dispatch({ type: "ticket" });
            armLoadTimer();
            break;
          case "exhausted":
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
            // Our own reload opens a new stream: not another tab's.
            armPending();
            break;
        }
      },
      [checkEnded, armLoadTimer, armPending],
    ),
  });

  const loadWorkspace = useCallback(async () => {
    try {
      const ws = unwrap(
        await api.GET("/v1/workspaces/{workspaceId}", { params: { path: { workspaceId } } }),
      );
      if (!mounted.current) return;
      setWorkspace(ws);
      const blocker = connectBlocker(ws);
      dispatch(
        blocker
          ? { type: "workspace", connectable: false, reason: blocker }
          : { type: "workspace", connectable: true },
      );
      if (!blocker) {
        // Resume first; request a ticket only when there is nothing to resume.
        void resume().then((resuming) => {
          if (!resuming && mounted.current) void launch("frame", false);
        });
      }
      void loadTemplate(api, ws).then((tpl) => mounted.current && setTemplate(tpl));
    } catch (e) {
      if (mounted.current) fail(e);
    }
  }, [api, workspaceId, launch, resume, fail]);

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

  // The lease is no longer (or no longer only) ours: a reload must not try
  // to resume it.
  useEffect(() => {
    if (state.status === "ended" || state.status === "external") forget();
  }, [state.status, forget]);

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
        <StatusBadge status={state.status} />
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
          allow={sessionFrameAllow(workspaceId, sessionDomain)}
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
          />
        )}
      </div>
    </section>
  );
}

const STATUS_KEY: Record<SessionStatus, [Parameters<typeof t>[0], Tone]> = {
  loading: ["session.status.loading", "neutral"],
  "not-ready": ["session.status.notReady", "warning"],
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

function StatusBadge({ status }: { status: SessionStatus }) {
  const [key, tone] = STATUS_KEY[status];
  const pulse = status === "requesting" || status === "connecting";
  return (
    <span role="status" aria-live="polite" className="tc-session__status">
      <Badge tone={tone} dot pulse={pulse}>
        {t(key)}
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
}: {
  state: SessionState;
  workspaceId: string;
  actionRef: RefObject<HTMLButtonElement | null>;
  onRetry: () => void;
  onNewTab: () => void;
  onTakeover: () => void;
  onUseHere: () => void;
  onSignIn: () => void;
}) {
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
    case "not-ready":
      return (
        <OverlayPanel
          tone="alert"
          title={t("session.notReady.title")}
          actions={
            <>
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
        {endsAt !== null ? ` · ${t("session.lifecycle.endsBy", { time: formatClock(endsAt) })}` : ""}
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
    return t("session.blocker.phase", { phase: ws.phase.toLowerCase() });
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

export function formatDuration(seconds: number): string {
  if (seconds < 3600) return `${Math.max(1, Math.round(seconds / 60))} min`;
  const h = Math.floor(seconds / 3600);
  const m = Math.round((seconds % 3600) / 60);
  return m ? `${h} h ${m} min` : `${h} h`;
}

function formatClock(ms: number): string {
  return new Date(ms).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}
