// Session-view state machine. The desktop runs in a cross-origin iframe, so
// the portal cannot look inside it: every transition below is driven by
// what the portal itself observes — the ticket API, the frame's load event,
// the passive /connection poll, its own CSP violations, network status and
// the workspace's phase.

export type SessionStatus =
  | "loading" //       fetching the workspace
  | "not-ready" //     workspace exists but is not connectable
  | "starting" //      a start intent is in flight: progress + workspace poll (V3.27)
  | "requesting" //    launch ticket request in flight
  | "in-use" //        CONNECTION_IN_USE: offer takeover
  | "connecting" //    ticket POSTed into the frame, waiting for it to load
  | "connected" //     the frame loaded the session origin
  | "elsewhere" //     another tab opened a newer stream on our lease: offer "Use here"
  | "disconnected" //  the stream is down and auto-recovery is spent or impossible
  | "ended" //         the workspace stopped or disappeared
  | "blocked" //       the browser refused to embed the session: new-tab fallback
  | "external" //      the session was handed to a new tab
  | "signed-out" //    the portal login expired (401): the page cannot talk to the API
  | "error"; //        launch failed

export interface SessionState {
  status: SessionStatus;
  /** Failure behind `error` (an Error or PortalApiError). */
  error?: unknown;
  /** Short machine reason for `ended` / `blocked` / `not-ready` / `disconnected`. */
  reason?: string;
}

export type SessionEvent =
  /** `starting`: the workspace is mid start/create — keep polling it. */
  | { type: "workspace"; connectable: boolean; reason?: string; starting?: boolean }
  | { type: "request" }
  | { type: "ticket" }
  /** Resuming our own live lease: the frame was pointed at the session origin. */
  | { type: "resume" }
  /** The /connection poll confirmed the resumed stream. */
  | { type: "resumed" }
  | { type: "signed-out" }
  | { type: "in-use" }
  | { type: "failed"; error: unknown }
  | { type: "frame-loaded" }
  | { type: "frame-blocked"; reason: string }
  | { type: "offline"; reason: "offline" | "unreachable" | "exhausted" }
  | { type: "ended"; reason: string }
  | { type: "external" }
  /** /connection shows a newer stream on our lease than the one this tab opened. */
  | { type: "elsewhere" };

export const initialSessionState: SessionState = { status: "loading" };

/** States in which a desktop is (or is about to be) live in the frame. */
export function isLive(s: SessionStatus): boolean {
  return s === "connecting" || s === "connected";
}

export function sessionReducer(state: SessionState, ev: SessionEvent): SessionState {
  switch (ev.type) {
    case "workspace":
      // The initial load and the starting-state polls decide connectability;
      // everywhere else polls report through "ended" so a live session is
      // never reset by a poll (V3.27: starting polls keep landing here).
      if (
        state.status !== "loading" &&
        state.status !== "not-ready" &&
        state.status !== "starting"
      ) {
        return state;
      }
      if (ev.connectable) return { status: "requesting" };
      return ev.starting
        ? { status: "starting" }
        : { status: "not-ready", ...(ev.reason ? { reason: ev.reason } : {}) };
    case "request":
      return { status: "requesting" };
    case "ticket":
    case "resume":
      return { status: "connecting" };
    case "resumed":
      return state.status === "connecting" ? { status: "connected" } : state;
    case "signed-out":
      return { status: "signed-out" };
    case "in-use":
      return { status: "in-use" };
    case "failed":
      return { status: "error", error: ev.error };
    case "frame-loaded":
      // A slow frame that loads after the timeout fallback was offered still
      // wins: the desktop is there after all.
      return state.status === "connecting" || (state.status === "blocked" && state.reason === "timeout")
        ? { status: "connected" }
        : state;
    case "frame-blocked":
      return isLive(state.status) ? { status: "blocked", reason: ev.reason } : state;
    case "offline":
      // Coming back online does not resurrect the desktop by itself (the
      // stream socket is gone), so there is no "online" event: the user
      // reconnects explicitly.
      return isLive(state.status) ? { status: "disconnected", reason: ev.reason } : state;
    case "ended":
      return state.status === "external" || state.status === "loading"
        ? state
        : { status: "ended", reason: ev.reason };
    case "external":
      return { status: "external" };
    case "elsewhere":
      return state.status === "connected" ? { status: "elsewhere" } : state;
  }
}
