// Public surface of the session area for the rest of the portal.
export { SessionPage, type SessionPageProps } from "./SessionPage";
export {
  assertLaunchTarget,
  launchInNewTab,
  sessionFrameName,
  sessionLabel,
  sessionOrigin,
  sessionPath,
  submitLaunch,
  sessionFrameFeatures,
  sessionFrameAllow,
  SESSION_FRAME_SANDBOX,
  TICKET_FIELD,
  type LaunchTicket,
} from "./launch";
export {
  CONNECTION_POLL_MS,
  MAX_AUTO_RELAUNCH,
  RECONNECT_BACKOFF_MS,
  useConnectionWatch,
  type ConnectionStatus,
  type WatchEvent,
} from "./useConnectionWatch";
