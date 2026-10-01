# Backup & restore

Design: `docs/architecture.md` §5. Covers control-plane backup and the restore
drill. Scope: API DB (Postgres), Workspace/WorkspaceTemplate CR manifests,
and persistent volumes (PVCs / retained disks).

## What must be backed up together

The platform state is split across three stores that are only consistent
as a set:

| Store | Contents | Backup |
|---|---|---|
| API DB (Postgres) | workspaces, quota reservations, outbox intents, idempotency keys, sessions, launch tickets, connection leases, `retained_data` records | `pg_dump` / PITR base backup |
| Cluster manifests | Workspace + WorkspaceTemplate CRs (spec is the fencing record: `intentRevision`, `runtimeGeneration`), operator annotations (`applied-intent`, `template-snapshot`, `finalizer-progress`) | CR export (`kubectl get ... -o yaml` or etcd-level backup) |
| Volumes | workspace PVCs, retained disks, Windows VM/DataVolume disks | CSI `VolumeSnapshot`s / storage-backend snapshots |

**They must be coordinated.** A DB snapshot taken before a CR change and
restored together with a later volume snapshot can disagree about who owns
a disk. Take the volume snapshots and the DB dump inside the same planned
window, quiescing writes if the tooling allows; record the snapshot set
id and timestamp in the change record.

## Application consistency — read this before trusting a snapshot

**CSI snapshots are NOT automatically application-consistent.** A
`VolumeSnapshot` is crash-consistent at best: it is equivalent to pulling
the power cord. Filesystem and guest state may be incomplete.

- **Linux workspaces:** home PVCs are acceptable crash-consistent for
  typical desktop data, but anything with its own write ordering
  (databases inside the workspace, dev tools mid-write) can be corrupted.
  Prefer snapshotting while the workspace is **Stopped**, or quiesce the
  workload first.
- **Windows VMs:** NTFS + registry + in-flight updates make
  crash-consistent snapshots materially riskier. Snapshot only after a
  clean guest shutdown (Stop → wait for VMI termination), or use a
  storage integration that requests guest quiescence (VSS/fsfreeze).
  Never treat a running-VM snapshot as a restore point you can promise.
- The `retained_data` inventory is the exception that is easy to get
  right: retained disks are detached — snapshot them freely.

## Restore procedure (test namespace)

Do the drill on a scratch cluster/namespace before you need it in anger.
This procedure was run end-to-end against a fresh release on a
throwaway namespace; the steps below carry the corrections that drill
surfaced.

1. **Restore CRs first.** Apply the WorkspaceTemplate + Workspace
   manifests, then the volume objects' claims, into the target
   namespaces. Verify the API group/version matches the installed CRDs.
   Two corrections from the drill:
   - Strip operator reconcile bookkeeping from exported Workspace
     manifests — `workspaces.cdi.tinyorbit.vn/applied-intent`,
     `incarnation-start` and `finalizer-progress`. The strip keeps the
     park-until-PVC ordering intact: the operator only adopts
     `spec.intentRevision` greater than the recorded applied revision, so
     a restored `applied-intent` would override the Stopped park and boot
     the workspace before its data is in place. (Older releases also
     latched terminal `Failed/BootDeadlineExceeded` here — the restored
     `appliedAt` put the boot deadline in the past; current releases
     anchor the deadline to the runtime incarnation's first observation,
     so a stale
     `appliedAt` can no longer fail a converging workspace. The strip is
     still correct for the intent fencing.) Keep `template-snapshot` —
     it is the immutable image pin the workspace was admitted under.
   - A bare `spec.desiredState` patch is **ignored** by the operator:
     it only adopts intents with `spec.intentRevision` greater than the
     recorded `applied-intent.revision`. To re-drive a restored
     workspace, patch `intentRevision` past the applied revision
     together with `desiredState`, and bump the DB row's
     `intent_revision` to match so later API-driven intents stay ahead.
   - Park `dataPolicy: Retain` workspaces as `desiredState: Stopped`
     until their home PVC exists **and** is filled — the home claim is
     named `ws-<new CR UID>-home`, so it can only be pre-created after
     the CR is applied. Ephemeral workspaces need no PVC (emptyDir).
2. **Restore volumes** from the coordinated snapshot set into PVCs with
   the **same UIDs** where the storage layer allows; where UIDs change,
   re-stamp `workspaces.cdi.tinyorbit.vn/workspace-uid` +
   `data-retained` metadata so dataset identity (PVC UID + workspace UID)
   is re-pinned to the restored objects — never by name. Drill corrections:
   - **Strip binding annotations** on re-created claims —
     `pv.kubernetes.io/bind-completed`, `bound-by-controller`,
     `volume.*-provisioner`, `selected-node`. Copying `bind-completed`
     without the original `volumeName` leaves the claim `Lost` while the
     provisioner orphans the PV it just made (observed; PVs must be
     deleted by hand).
   - Copy data into the restored claims *before* starting the workspace —
     a restore-helper pod still holding the claim blocks the workspace's
     RWO attach (`FailedAttachVolume ... already used by pod`).
   - If the restore target runs a **different OIDC issuer**, re-map
     `retained-owner` annotations and the DB
     `workspaces.owner_subject` / `retained_data.owner_subject`
     (`<new-issuer>|<old-sub>`) — both the workspace list and the
     retained inventory are owner-scoped.
3. **Restore the API DB.** Replay migrations if the target schema is
   older (`store.Migrate` is idempotent, versioned via
   `schema_migrations`). On a live target release, scale `api` to 0
   first, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`, load the
   dump, then scale back — `pg_dump --no-owner` output loads cleanly
   this way (verified in the drill).
4. **Rebuild the retained inventory** from PVC metadata:
   `RetentionInventory.List` over each tenant namespace, then
   `ImportRetained` (idempotent on `(pvc_namespace, pvc_uid)`). Check
   `Unregistered`/`MissingVolumes` for drift. Delete `retained_data`
   rows whose `pvc_namespace` sits outside the release's managed
   namespaces — they describe foreign claims the sweep can never see
   (RetainedSync re-imports the in-scope claims from PVC metadata).
5. **Invalidate all access from before the restore — automatically.** All
   launch tickets and connection leases minted before the restore are
   invalid: tickets redeem only at the gateway whose `audience` (session
   host:port) they were issued for — a different release's gateway
   answers `invalid_ticket` — and every restored lease binds a
   `runtimeUID` that no longer exists, so it can never attach a stream.
   **Sessions die too.** Every session row binds to
   `platform_meta.session_epoch` (migration 009). The restore procedure
   rotates the epoch after loading the dump
   (`INSERT ... ON CONFLICT DO UPDATE`, same
   statement as `store.DB.RotateSessionEpoch`), so every restored session
   carries the old epoch and fails with `UNAUTHENTICATED` — a session
   revoked after the backup cannot be resurrected by the restore.
   Communicate a **re-login**, then reconnect via fresh ticket.
   Rotation is automatic in restore.sh; if you restore by hand, run
   `SELECT`/`UPDATE` on `platform_meta` per that script before scaling
   the api back up.
6. **Reconcile quota** — after DB restore, compare `quota_reservation`
   `held` rows against observed runtimes (recovery path) and against
   `retained_data` rows for disk bytes. The drill observed the api recovery
   loop releasing `held` reservations for restored workspaces while
   their runtimes did not yet exist — let a sweep complete before
   re-driving intents, or expect released-until-next-acquire. (Fixed —
   `Start` re-acquires the released reservation in the
   same transaction as the start intent and fails `QUOTA_EXHAUSTED` when
   there is no headroom, so a stop/start cycle can no longer exceed the
   running quota.)

## Post-restore invariants to verify

- [ ] `retained_data` rows ↔ retained-labelled PVCs match 1:1 in both
      directions (no `Unregistered`, no `MissingVolumes`).
- [ ] No pre-restore ticket redeems; no pre-restore lease renews.
- [ ] First boot of an attached retained disk re-enrolls the runtime with
      new credentials.
- [ ] Held disk quota equals the sum of live + retained disks exactly
      once.
- [ ] The dispatcher replays undispatched outbox intents in revision
      order; no workspace gets two runtimes.
