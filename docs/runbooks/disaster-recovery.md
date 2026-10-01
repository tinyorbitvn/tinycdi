# Runbook — Disaster recovery

Design: `docs/architecture.md` §5. Extends `docs/runbooks/backup-restore.md` (the
consistency rules and post-restore invariants — read it first) into a full
loss-and-rebuild procedure, including the **copy-based** volume backup
needed when the cluster has no `VolumeSnapshotClass`.

Scope reminder: this platform has no HA. DR protects **data** (Postgres,
CRs, PVCs), not availability. Live sessions are never migrated — after any
restore, all tickets and leases are dead and every user reconnects with a
fresh login (expected, not an outage).

## Failure tiers and what each costs

| Tier | Event | Recovery |
|---|---|---|
| 1 | Component pod/node loss (api, operator, gateway, portal, one worker) | Kubernetes reschedules; streams drop and users reconnect within the lease window or re-ticket. No restore needed. |
| 2 | Platform namespace or release loss | `helm install` again + re-create the values-referenced Secrets from escrow; CRs/PVCs in managed namespaces survive (chart keeps them — install.md §Uninstall). |
| 3 | Postgres loss | Restore DB dump; rebuild retained inventory from PVC metadata; reconcile quota (backup-restore.md §4–6). |
| 4 | Whole cluster loss | Full rebuild below — CRDs + chart + Secrets + CRs + volumes + DB. |
| 5 | Storage backend loss | Only the off-cluster volume backups below save the data. |

RPO/RTO targets — measured in a restore drill (single fresh
release on the reference cluster):

- **RPO** = the timestamp of the last coordinated backup set (DB dump +
  CR export + PVC copies inside one quiesce window; ~1 min at small scale).
  There is no continuous replication — anything after the dump is lost.
- **RTO (small-scale, single release):** ≈20 minutes wall-clock end to
  end — fresh release install ~5 min, restore script (CRs + PVCs + DB +
  data copy) ~5 min once correct, verification ~10 min. Scale by PVC
  count and object count; a real outage adds decision time. These are
  measured drill numbers, not an SLA.

## The backup set you must hold off-cluster

Everything in `backup-restore.md` §"What must be backed up together", as one
coordinated, timestamped set:

1. **Postgres dump** — `pg_dump` (or PITR) of the platform DB.
2. **CR export** — all Workspace + WorkspaceTemplate manifests per managed
   namespace:
   `kubectl get workspaces,workspacetemplates -A -o yaml`.
3. **Volume backups** — see below; no snapshots assumed.
4. **Secrets escrow** — every Secret the values file references
   (OIDC client, DB DSN, mTLS chain, gateway/portal TLS, control token).
   The chart never owns them; if they die with the cluster, so does your
   mTLS identity.
5. **The values file used at install** and the chart version — you cannot
   rebuild what you cannot reproduce.

### Copy-based PVC backup (no VolumeSnapshotClass assumed)

When the cluster has no `VolumeSnapshotClass`, use a reader Job per PVC:

```bash
# Stream a volume's contents to external storage. Runs as a Job mounting
# the target PVC read-only; write to an off-cluster sink (S3/NFS/host path
# on a backup node — NOT the same Ceph pool you are protecting).
kubectl -n <tenant-ns> apply -f - <<'EOF'
apiVersion: batch/v1
kind: Job
metadata: {name: pvc-backup-<pvc-name>}
spec:
  template:
    spec:
      restartPolicy: Never
      containers:
      - name: copy
        image: <pinned-toolbox-image>
        command: ["sh","-c","cd /src && tar cf - . | gzip > /backup/<pvc-uid>.tar.gz"]
        volumeMounts:
        - {name: src, mountPath: /src, readOnly: true}
        - {name: dst, mountPath: /backup}
      volumes:
      - name: src
        persistentVolumeClaim: {claimName: <pvc-name>, readOnly: true}
      - name: dst   # NFS/S3-fuse/hostPath to off-cluster storage
        ...
EOF
```

Record the PVC **UID** (`kubectl get pvc <name> -o jsonpath='{.metadata.uid}'`)
in the backup manifest — dataset identity is PVC UID + workspace UID, never
the PVC name (`docs/runbooks/retained-data.md`). Quiesce first: stop the
workspace (crash-consistent is the ceiling; a running-workspace copy can
tear mid-write — backup-restore.md §"Application consistency"). Retained
(disconnected) disks are safe to copy any time.

On a cluster **with** a VolumeSnapshotClass, CSI snapshots may replace the
copy step — but they are still only crash-consistent and must follow the
same quiesce rules.

## Full-cluster-loss restore (tier 4)

Order matters — later steps assume earlier ones exist:

1. **Cluster baseline.** New cluster at a supported version
   (`docs/compatibility.md`); CNI enforcing NetworkPolicy;
   StorageClass for PVCs; node profile pair rolled to browser-capable
   workers (the node-profile rollout condition in `docs/compatibility.md`).
2. **CRDs + chart.** `kubectl apply -f deploy/helm/tinycdi/crds/`,
   then `helm install` with the escrowed values
   (`docs/runbooks/install.md`) into a fresh release namespace. Do **not**
   seed `templates[]` until the DB restore decides which workspaces exist.
3. **Secrets.** Re-create every Secret from escrow in the release namespace.
   A lost mTLS CA means a full re-issue of the internal chain — gateway and
   operator cannot reach the broker until it is done.
4. **Tenant namespaces + CRs.** Create the managed namespaces (or let the
   chart recreate them — labels/PSS must match), then apply the CR export.
5. **Volumes.** Restore each PVC from its copy/snapshot, then re-stamp
   metadata where UIDs changed:
   `workspaces.cdi.tinyorbit.vn/workspace-uid` and the `data-retained`
   annotations must re-pin to the restored objects
   (backup-restore.md step 2).
6. **Postgres.** Restore the dump; `store.Migrate` is idempotent and
   fills any schema gap between the dump and the running api.
7. **Retained inventory.** `RetentionInventory.List` + `ImportRetained`
   rebuild `retained_data` from PVC metadata; check for `Unregistered` /
   `MissingVolumes` drift.
8. **Quota reconcile.** Compare held `quota_reservation` rows against
   observed runtimes and `retained_data` — the api recovery loop
   (`-recovery-interval`, `internal/provisioning/recovery.go`) settles
   them; `tinycdi_quota_drift` should read 0 before opening to users.
9. **Access reset.** All pre-loss tickets and leases are invalid by
   construction — they fence on (workspaceUID, runtimeGeneration,
   runtimeUID) of incarnations that no longer exist. Users log in fresh
   and reconnect. Announce the reconnect storm.

Then run the full post-restore invariant checklist in
`docs/runbooks/backup-restore.md` (retained rows ↔ PVCs 1:1, no pre-restore
ticket/lease redeeming, held disk quota exact, outbox replays in revision
order with no double runtimes).

## Restore drill — executed

A release drill executed tier 3 + the volume path on a
separate fresh release (own namespaces, own OIDC issuer, own
NodePorts): pg_dump restore + CR re-apply + copy-based PVC restore with
checksums, against a live release which was never touched.

Verified post-restore: restored Workspace CRs reconcile to
Ready; the Retain workspace's home marker sha256 matches the backup
manifest byte-for-byte; the retained inventory imported 3 disks from PVC
metadata and one attached+read back with matching file checksum; quota
reservations reconciled; pre-restore launch tickets get
`invalid_ticket` at the gateway (audience is the session
host:port — tickets minted for the old session host cannot redeem at
the new one); restored leases bind dead `runtimeUID`s; a fresh login +
fresh ticket redeems 303 and streams; the outbox replay produced no
duplicate runtimes.

Drill findings now folded into the procedure above / in
backup-restore.md:
- Strip `applied-intent`/`incarnation-start`/`finalizer-progress` from
  exported Workspace manifests — the strip preserves the
  park-until-PVC intent ordering (a stale applied record would override
  the Stopped park). The stale-`appliedAt` → instant
  `Failed/BootDeadlineExceeded` failure itself is fixed: the boot
  deadline anchors to the runtime incarnation's first observation.
- The operator only adopts `spec.intentRevision > applied.revision` —
  re-driving a restored workspace needs an `intentRevision` bump plus a
  matching `workspaces.intent_revision` bump in the DB.
- Never copy `pv.kubernetes.io/bind-*` annotations onto re-created
  claims (claim goes `Lost`, PV orphaned).
- Restored workspaces keep their original lifecycle clock — both
  drill workspaces were stopped by their own 30 m idle expiry ~30 min
  after their pre-backup last activity. Expected, not a bug.
- Portal sessions die on restore: every session binds to
  `platform_meta.session_epoch` and `restore.sh` rotates the epoch after
  loading the dump — restored cookies fail `UNAUTHENTICATED` and users
  re-login. Tickets and leases die by construction (audience +
  runtimeUID fencing). Announce "re-login then reconnect".
- Teardown order: delete Workspace CRs (finalizers need the operator)
  BEFORE `helm uninstall` removes the operator — or strip the
  `runtime-cleanup` finalizer during a wholesale namespace teardown
  (as the drill did).
