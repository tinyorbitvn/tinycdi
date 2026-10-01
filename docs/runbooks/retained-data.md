# Retained data — operations runbook

Design: `docs/architecture.md` §5. Covers the retained-disk inventory behind
`GET /v1/data`, `POST /v1/data/{id}/attach`, `POST /v1/data/{id}/purge`.

## Model

A workspace with `dataPolicy: Retain` moves its persistent volume into the
retained inventory on **Delete** (the finalizer's `retention` step).
`Ephemeral` volumes are destroyed instead.

- **Source of truth is PVC metadata**, not the API DB. The operator stamps
  `workspaces.cdi.tinyorbit.vn/data-retained=true` plus annotations carrying
  tenant, owner (`iss|sub`), source workspace and runtime
  (`internal/operator/retention.go`). The `retained_data` table
  (migration `007`) is the transactional index; it can be rebuilt from PVC
  metadata after an API DB loss (see `backup-restore.md`).
- **Dataset identity = PVC UID + source workspace UID.** PVC names are
  reused and must never be trusted as identity.
- **State machine:** `Retained → Attaching → Attached` and
  `Retained → Purging → Purged`. Transitions are conditional updates in
  the same transaction as the quota reservation and outbox intent, so
  attach and purge mutually exclude each other — a disk has at most one
  consumer.
- The request path **never deletes persistent data on its own**. `Purge`
  only moves the record to `Purging`; the control-plane purge sweeper
  (`PurgeSweeper`, running alongside the API against the cluster) deletes
  the volume — and only after verifying the PVC UID still matches the
  record and no live consumer (pod mount or consuming Workspace CR)
  references it. The completion proof advances the record to `Purged` and
  releases the disk quota. `RetentionInventory.Purge` is the same
  verified-detach delete exposed operator-side.

## Quota

- A retained disk's `disk_bytes` stay `held` in `quota_reservation` until
  the record reaches `Purged` — a disk you still owe storage for counts.
- On attach the held disk reservation is re-keyed to the consuming
  workspace: counted once, never twice.
- Attach reserves compute for the new workspace normally; without
  headroom the API returns `409 QUOTA_EXHAUSTED` and the disk stays
  `Retained`.

## Routine operations

### List a user's retained disks

```
GET /v1/data            # owner scope; tenant admins see the tenant
```

Each record carries a fresh `purgeConfirmationNonce` bound to the record,
the caller and the current state epoch. It is short-lived and single-use —
a stale or consumed nonce is `400 INVALID_REQUEST`.

### Attach a disk to a new workspace

```
POST /v1/data/{rd_id}/attach   (Idempotency-Key required)
{"name": "restored-desktop", "templateRef": "tpl_..."}
```

- Only the record owner (or a tenant admin) may attach; foreign ids are
  `404`.
- `POST /v1/workspaces` with `retainedDataRef` is the **same claimed
  path**, not a shortcut: it runs `AttachRetained` with the caller's
  owner/tenant-admin scope, requires state `Retained`, takes the
  `Retained → Attaching` claim and moves the disk quota atomically. A
  record the caller cannot see returns `404`; the plain create path
  refuses a bare `retainedDataRef`, and the applier re-verifies the claim
  (state, consuming workspace, owner) before touching any CR or PVC.
- `templateRef` runtime must match the disk's runtime
  (`422 INVALID_TEMPLATE` on mismatch).
- The new workspace is a **new runtime identity**: new
  `runtimeGeneration`, new enrollment token. The deleted workspace's
  credentials, tickets and leases never come back; the first boot
  re-enrolls.
- If the new workspace is deleted before the attach completes, the disk
  returns to `Retained` — it is never garbage-collected by association.

### Purge a disk

```
POST /v1/data/{rd_id}/purge    (confirmationNonce required)
{"confirmationNonce": "..."}
```

- Nonce from the caller's latest `GET /v1/data`; bound to record +
  principal + state epoch. Replay of a consumed nonce → `400`.
- Only `Retained` records purge; `Attaching`/`Attached` →
  `409 INVALID_STATE` (delete the consuming workspace first).
- The nonce MAC key is the single-row `retained_nonce_key` table
  (migration `008`), shared by all API replicas. Rotating it — deleting
  the row so a fresh key is generated — invalidates every outstanding
  nonce; users must re-read `GET /v1/data` for fresh ones. That is the
  sanctioned way to abort all pending purge confirmations at once.
- The record reports `Purging` until the sweeper confirms the volume is
  gone. Do not promise the user instant destruction.

## Drift / orphan detection

Orphans run in **both directions** — check both:

| Symptom | Meaning | Action |
|---|---|---|
| PVC has `data-retained` label but no `retained_data` row | API DB lost rows, or import was missed | Re-import via the inventory (`RetentionInventory.Unregistered` surfaces these; `ImportRetained` is idempotent on PVC UID) |
| `retained_data` row points at a PVC UID that does not exist | volume deleted out-of-band | `RetentionInventory.MissingVolumes`; investigate storage backend before marking the record `Purged` |
| Record `Attaching`/`Attached` with no consuming workspace | mid-attach delete missed the rollback | `ReturnToRetained` returns it; verify the PVC is still labelled for the source workspace UID |

Detection surfaces: operator `RetentionInventory` (`Unregistered`,
`MissingVolumes`) and the `RetainedPVCUIDs` cross-check; the reconciliation
loop should emit metrics/alerts for non-empty results (alerting wiring is tracked separately —
dashboards).

## Tenant namespace decommission

A namespace holding retained disks **must not** be deleted as if it were
an empty workspace namespace:

1. Run `RetentionInventory.DecommissionCheck(namespace)`. Non-empty →
   stop. Contact owners: attach elsewhere or purge each disk first.
2. Only when the inventory is empty proceed with namespace deletion.
3. Skipping the check destroys retained user data with the namespace and
   leaves `retained_data` rows pointing at volumes that no longer exist.

## Failure modes

| Symptom | Likely cause | Fix |
|---|---|---|
| Record stuck `Attaching`/`Attached`, consuming workspace gone | `ReturnToRetained` not delivered | Re-deliver via the record's `consuming_workspace_id`; the transition is safe to repeat |
| Record stuck `Purging` | the sweeper could not delete the PVC (CSI down, or the volume still shows a live consumer) | fix storage / detach the consumer; the sweep retries; quota stays held until completion — do not force `Purged` while the volume exists |
| `409 INVALID_STATE` on attach | disk already claimed or purging | `GET /v1/data` for the current state; check `consumingWorkspaceId` |
| `400 INVALID_REQUEST` on purge | stale/consumed nonce | re-`GET /v1/data` for a fresh nonce |
