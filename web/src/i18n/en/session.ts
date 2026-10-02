// Session-area strings: the in-portal session view (toolbar, frame overlays,
// clipboard hint, lifecycle notice). Owned by T2.5; append-only.
export default {
  "session.page.ariaLabel": "Desktop session: {name}",

  "session.toolbar.back": "Back to dashboard",
  "session.toolbar.controls": "Session controls",
  "session.toolbar.reconnect": "Reconnect",
  "session.toolbar.openInNewTab": "Open in new tab",
  "session.toolbar.fullscreen": "Full screen",
  "session.toolbar.exitFullscreen": "Exit full screen",

  "session.status.loading": "Loading",
  "session.status.notReady": "Not ready",
  "session.status.requesting": "Connecting",
  "session.status.inUse": "In use elsewhere",
  "session.status.connecting": "Connecting",
  "session.status.connected": "Connected",
  "session.status.disconnected": "Disconnected",
  "session.status.ended": "Ended",
  "session.status.blocked": "Blocked",
  "session.status.external": "In another tab",
  "session.status.error": "Error",

  "session.frame.title": "Desktop: {name}",
  "session.frame.keyboardHint":
    "Keyboard input goes to the desktop while it has focus. Use your browser's shortcuts or click outside the desktop to leave it.",

  "session.progress.loading": "Loading workspace…",
  "session.progress.requesting": "Requesting a secure session…",
  "session.progress.connecting": "Connecting to your desktop…",
  "session.progress.newTabHint": "Taking a while?",
  "session.progress.newTabAction": "Open it in a new tab",

  "session.inUse.title": "This workspace is open somewhere else",
  "session.inUse.body":
    "Another session is connected to this workspace. Taking over disconnects it.",
  "session.inUse.takeover": "Take over session",

  "session.notReady.title": "This workspace isn't ready to connect",
  "session.notReady.fallback": "Start the workspace and try again.",

  "session.disconnected.title": "Connection lost",
  "session.disconnected.offline":
    "Your device went offline. Reconnect once your network is back.",
  "session.disconnected.unreachable":
    "The portal can't reach the service right now. Reconnect to try again.",
  "session.disconnected.exhausted":
    "The session could not be recovered automatically. Reconnect to try again.",
  "session.disconnected.reconnect": "Reconnect",

  "session.ended.title": "Session ended",
  "session.ended.stopped":
    "The workspace was stopped. Start it again from the dashboard to reconnect.",
  "session.ended.failed": "The workspace failed. Open its details to see what happened.",
  "session.ended.deleted": "The workspace was deleted.",
  "session.ended.unknown": "The workspace is no longer running.",

  "session.blocked.title": "The desktop can't be shown inside the portal",
  "session.blocked.timeout":
    "The session didn't load in time — your browser or network may be blocking embedded sessions.",
  "session.blocked.policy": "Your browser blocked the embedded session.",
  "session.blocked.newTabHint": "You can open it in a new tab instead.",
  "session.blocked.retry": "Try again here",

  "session.external.title": "Session opened in a new tab",
  "session.external.body": "Bringing it back here disconnects the other tab.",
  "session.external.showHere": "Show it here instead",

  "session.error.title": "Couldn't start the session",
  "session.error.retry": "Retry",
  "session.error.requestId": "Request ID: {id}",
  "session.error.generic": "Something went wrong.",
  "session.error.api.invalidState":
    "The workspace is not accepting connections right now — it may be starting or stopping.",
  "session.error.api.forbidden": "You are not allowed to connect to this workspace.",
  "session.error.api.notFound": "This workspace does not exist (or is not visible to you).",
  "session.error.api.rateLimited": "Too many connection attempts. Wait a moment and retry.",
  "session.error.api.unauthenticated":
    "Your sign-in expired. Reload the page to sign in again.",
  "session.error.api.csrfFailed": "Your session token expired. Reload the page and try again.",
  "session.error.api.fallback":
    "The service could not start the session; it is safe to retry.",

  "session.overlay.workspaceDetails": "Workspace details",

  "session.clipboard.label": "Clipboard",
  "session.clipboard.disabled":
    "Clipboard sharing is disabled for this workspace by policy.",
  "session.clipboard.hint1":
    "Copy and paste between this computer and the desktop with the usual shortcuts while the desktop has focus.",
  "session.clipboard.hint2":
    "Your browser may ask to allow clipboard access for the session — allow it to paste from this computer.",

  "session.lifecycle.stopsAfter": "Stops after {duration} idle",
  "session.lifecycle.endsBy": "ends by {time}",
  "session.lifecycle.warn":
    "Ends in {minutes} min — save your work.",

  "session.blocker.stopped": "The workspace is stopped. Start it from the dashboard first.",
  "session.blocker.phase": "The workspace is {phase}. Try again once it is ready.",
  "session.blocker.connection": "The desktop's streaming endpoint is not ready yet.",
} as const;
