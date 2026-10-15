// Progress-area strings: the lifecycle step panel on the workspace list,
// detail and session pages (V3.27 PR-B). Owned by T-V3.27; append-only.
export default {
  "progress.create.title": "Creating {name}",
  "progress.start.title": "Starting {name}",
  "progress.stop.title": "Stopping {name}",
  "progress.delete.title": "Deleting {name}",

  "progress.step.accepted": "Request accepted",
  "progress.step.queued": "Waiting for its turn",
  "progress.step.scheduling": "Scheduling the start",
  "progress.step.disk": "Preparing the disk",
  "progress.step.diskAttach": "Attaching your disk",
  "progress.step.machine": "Finding a machine",
  "progress.step.machinePrepare": "Preparing the machine",
  "progress.step.imagePull": "Downloading the desktop image",
  "progress.step.desktop": "Getting the desktop ready",
  "progress.step.desktopStart": "Starting the desktop",
  "progress.step.connect": "Ready to connect",
  "progress.step.shutdown": "Shutting down",
  "progress.step.stopped": "Stopped",
  "progress.step.sessions": "Closing sessions",
  "progress.step.runtime": "Removing the desktop",
  "progress.step.data": "Taking care of your data",
  "progress.step.finishing": "Finishing up",
  "progress.step.working": "Working on it",

  "progress.state.done": "done",
  "progress.state.active": "in progress",
  "progress.state.waiting": "waiting",
  "progress.state.failed": "failed",
  "progress.state.skipped": "skipped",

  "progress.step.of": "step {current} of {total}",
  "progress.step.elapsed": "for {elapsed}",
  "progress.step.reason": "Status: {reason}",

  "progress.slow.machine": "No machine has room for it right now. It keeps trying.",
  "progress.slow.imagePull":
    "The desktop image is still downloading. First starts can take a few minutes.",
  "progress.slow.drain": "Waiting for open sessions to close (up to 45 seconds).",
  "progress.slow.generic": "This is taking longer than usual.",

  "progress.failed.imagePull":
    "The desktop image couldn't be downloaded. Try starting again; if it repeats, contact an administrator.",
  "progress.failed.deadline": "The workspace didn't become ready in time.",
  "progress.failed.cleanup": "A cleanup step failed and will be retried.",
  "progress.failed.generic": "This step failed.",

  "progress.notice.retry": "The platform hit an error and is retrying.",

  "progress.refresh.retrying": "Can't refresh progress, retrying…",
  "progress.delayed": "Status delayed. Showing the last known step.",
  "progress.stalled":
    "Still waiting. Nothing is wrong on your side. Contact an administrator.",

  "progress.deleted.toast": "'{name}' was deleted.",
  "progress.deleted.retainedLink": "Your disk is in Retained data",
  "progress.deleted.announce": "{name} was deleted.",
} as const;
