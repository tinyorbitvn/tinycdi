// Data-area strings: retained-disk list and purge dialog. Owned by T3.7
// after T3.3 seeds it; append-only.
export default {
  "data.list.title": "Retained data",
  "data.list.intro":
    "Disks kept after their workspace was deleted with dataPolicy Retain. Purge destroys a disk permanently — Delete only removes a workspace.",
  "data.list.loading": "Loading…",
  "data.list.empty": "No retained disks.",
  "data.list.label": "retained data",
  "data.list.col.id": "ID",
  "data.list.col.workspace": "From workspace",
  "data.list.col.runtime": "Runtime",
  "data.list.col.size": "Size",
  "data.list.col.state": "State",
  "data.list.col.retainedAt": "Retained at",
  "data.list.size": "{size} GiB",
  "data.purge.action": "Purge",
  "data.purge.label": "purge retained disk",
  "data.purge.warning":
    "Purging {id} ({size} GiB from {name}) destroys the disk permanently. Type {name} to confirm.",
  "data.purge.confirmLabel": "Confirmation",
  "data.purge.confirm": "Purge permanently",
  "data.purge.confirming": "Purging…",
} as const;
