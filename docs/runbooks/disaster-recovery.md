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
| 3 | Postgres loss | Restore DB dump; rebuild retained inventory from PVC metadata; reconcile quota (backup-restore.md §4–6). **If the cluster kept running** (CRs and runtime pods still live), follow "Postgres-only restore onto a live cluster" below instead — the fencing assumptions the rebuild procedure relies on do not hold there. |
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
   Cadence: one pass at every leader acquisition, then every
   `-recovery-interval` (`<=0` = the single startup pass only); quota
   settlement is primarily event-driven off the Workspace informer and
   the tick is the fallback. A reservation whose runtime absence is never
   proven is held indefinitely by design — quota is released only on
   positive proof — so a row stuck `held` means the runtime is unproven,
   not that recovery stopped.
9. **Access reset.** All pre-loss tickets and leases are invalid by
   construction — they fence on (workspaceUID, runtimeGeneration,
   runtimeUID) of incarnations that no longer exist. Users log in fresh
   and reconnect. Announce the reconnect storm.

Then run the full post-restore invariant checklist in
`docs/runbooks/backup-restore.md` (retained rows ↔ PVCs 1:1, no pre-restore
ticket/lease redeeming, held disk quota exact, outbox replays in revision
order with no double runtimes).

## Postgres outage (failover) — behaviour and measured numbers

Tier 3 above covers Postgres **loss** (data recovery). A Postgres
**outage** — restart, failover, network cut — is an availability event,
not a DR event: no data is lost and the platform heals itself when the
database returns. Fail-closed behaviour was measured in the v0.3 outage
drill (`tests/integration/postgres_outage_test.go`, `docker stop`/`start`
of the Postgres container — all connections severed at once, the
shared-fate failover shape):

| Outage | Open streams | Control-plane API | After recovery |
|---|---|---|---|
| **10 s** (shorter than the revoke deadline) | WebSocket stayed open and echoed through the outage and after recovery — no reconnect needed | `GET /v1/workspaces` → **503 `UNAVAILABLE`** in ~1 ms (bound: ≤ 5 s, never a hang) | Lease renewals resume; session unaffected |
| **45 s** (longer than the deadline) | Socket closed **30.1 s** after the outage began — `session closed reason="renew_deadline"` (30.4 s after the last counted lease renew) | same 503 `UNAVAILABLE`, immediate | Lease row had expired (lease TTL 30 s) → reconnect takes the **re-launch** path: new ticket, fresh stream |

Semantics, for operators planning DB maintenance or running a failover:

- The session gateway survives broker/store failures only for
  `-revoke-deadline` (default **30 s**) measured from the last lease renew
  that actually landed. A failover that completes inside ~30 s keeps live
  desktop streams; **a failover longer than 30 s drops every stream by
  design** — fail closed beats serving an unverifiable session.
- While the DB is down the app API answers `503 UNAVAILABLE` fast (~1 ms
  measured) instead of hanging or answering wrong — callers should retry,
  not fail.
- After the DB returns, streams that survived (short outage) continue
  untouched. Streams that were killed: if the lease row is still active
  the existing session cookie re-opens a stream; once the lease has
  expired (TTL 30 s), the user goes back through the ticket/launch flow —
  measured path in the drill.
- No operator action is needed on recovery: renewals resume on the next
  tick and the lease/outbox machinery settles itself.

## Postgres-only restore onto a live cluster

The procedures so far assume a rebuild: fresh release, fresh gateway,
dead runtimes. A different failure is **Postgres alone rolling back** —
the DB is lost, corrupted or reverted while the cluster keeps running,
and the only dump is *older* than the live world. Restoring it is not a
rebuild: the fencing that makes restored tickets and leases harmless on
a fresh release (dead `runtimeUID`s, a foreign gateway audience) does
**not** hold — the workspaces, runtimes and gateway the dump remembers
are still alive. This section is the restore-semantics contract for that
case and was verified in the kind drill below.

### What an older dump resurrects or regresses

| Restored state | What the dump brings back | Verdict |
|---|---|---|
| `sessions` | Rows exactly as of the dump — **including sessions deleted by sign-out after the dump** (S17). The old portal cookie is valid again until the idle/absolute cap. | **Resurrects.** The row is digest-keyed and cryptographically indistinguishable from a live one; nothing marks it "deleted after dump". Fix: rotate `platform_meta.session_epoch` after loading the dump — every restored row fails its next read. Verified in the drill: a signed-out cookie returned `200` after a naive restore, `401` after rotation. |
| `connection_lease` | `'active'` leases as of the dump — **including leases revoked after it** — with their `session_digest` bindings. | **Resurrects inside the lease TTL.** `LeaseBySession` resolves cookie digest → `'active'` lease; renew and re-attach never consult the portal session, and the bound `(runtimeUID, runtimeGeneration, fencingVersion)` still matches — the runtime is alive. The drill restored a dump ~15 s old and the signed-out gateway cookie served `200` until `expires_at` lapsed. A restore landing inside the TTL (or a stream renewing inside it) extends this indefinitely — the lease self-heals only by expiry. Fix: revoke every restored lease unconditionally, below. |
| `launch_ticket` | Unconsumed tickets re-arm; tickets revoked after the dump revive. | Fenced, mostly: redemption re-checks the portal-session digest (S17 second barrier) and fails closed under a rotated epoch, and tickets die at `expires_at` (60 s). Tickets minted without a portal-session digest have no such barrier, and a fast restore lands inside either TTL. Fix: deny every unconsumed ticket unconditionally, below. |
| Fencing counters (`fencing_version`, `stream_epoch`) | Regress to dump values. | Safe. `fencing_version` is `MAX+1` per workspace inside the restored table — self-consistent; renew/claim re-validate against the live informer binding, so a recreated runtime stays fenced. |
| `workspaces` rows vs live CRs | Dump-time `desired_state`, `runtime_generation`, `intent_revision`, `phase`; CRs created after the dump have no row at all. | **Diverges — the real cost.** See "Workspace reconciliation" below. |
| `outbox_intent` | `dispatched_at` regresses; intents applied after the dump are undispatched again. | Safe by fencing: re-delivery hits CRs whose `applied-intent` already names the revision, and the operator drops `intentRevision <= applied.revision`. Pending rows replay by design. |
| `quota_reservation` | `held` rows for workloads deleted after the dump (phantom usage); missing rows for workloads created after it (invisible usage). | Phantoms release on proven runtime absence in one recovery pass. Invisible usage under-counts until reconciled — keep admission closed until the workspace reconcile finishes (the freeze covers this). |
| `retained_data` | Rows for PVCs deleted after the dump; missing rows for retained PVCs created after it. | `RetainedSync` imports the missing and counts the stale as `MissingVolumes` — it never deletes; purge stale rows per runbook `retained-data.md`. |
| `rate_limit_window` | Minute-bucketed counters at dump state. | Harmless: a rollback under-counts at most the in-flight minute; expired rows are swept on the 15-minute retention. `TRUNCATE rate_limit_window` is acceptable but buys nothing the bound does not already give. |
| `idempotency`, `workspace_activity`, `workspace_revocation`, `tenant_quota` | Dump-time copies. | A retried create collides on `workspaces.request_id` (conflict, not double-create). Activity counters skew idle/disconnect timers one cycle. A post-dump operator-stop revocation row is lost, but the stopped phase still blocks tickets. Quota limit edits after the dump revert — re-apply them. |

### Procedure

Do this on a change window; it is a platform freeze, not a rebuild.
Steps 1–3 are the parts that differ from backup-restore.md.

1. **Freeze the platform.** Scale `backend` and `operator` to 0 in the
   release namespace and wait for the pods to exit. `backend` is the app
   API, the session gateway and the outbox dispatcher in one Deployment —
   scaling it stops logins, lease renewals, ticket redemption and intent
   dispatch; every open stream dies at its revoke deadline (~30 s) and
   every lease at its TTL. The operator may stay up if you prefer, but
   scaling it too means nothing mutates CRs mid-restore (it comes back
   at step 4 — orphan deletion needs its finalizer, which calls the
   backend control surface).
2. **Load the dump** exactly as backup-restore.md step 3: drop and
   recreate schema `public`, replay the `pg_dump --no-owner` output.
   `store.Migrate` fills any schema gap when the api comes back.
3. **Kill all restored access and align intent rows — with the backend
   `post-restore` one-shot, before scaling back.** The backend binary
   carries a `post-restore` subcommand so the kill-steps cannot be skipped
   or partially applied by hand. `-apply` performs, in order:

   - `session_epoch` rotation (F5/S17): every restored session row is
     rejected on its next read, revoked or not — the defensive DDL keeps
     this working against dumps taken before migration 009;
   - `connection_lease` revoke: resurrected leases carry live
     `session_digest` bindings — the old gateway cookie rehydrates to them
     with no portal-session check;
   - `launch_ticket` deny for every unconsumed row;
   - `workspaces` alignment: every `state='active'` row whose live CR is
     ahead is set to the CR's `spec.desiredState`,
     `spec.runtimeGeneration`, `spec.intentRevision` (see step 5 for why
     that fence matters). Rows with no CR (ghosts) and CRs with no row
     (orphans) are counted and printed for step 5 — never deleted here.

   The subcommand is idempotent; `-apply` requires `-i-have-scaled-down`
   and additionally refuses while any backend replica holds the leader
   advisory lock — the flag is the operator's freeze acknowledgement, the
   lock probe the hard backstop (a serving replica set elects a leader
   within seconds). It logs one summary line per run. With no flag the
   command is a dry-run: the plan plus affected counts, no writes.

   Run it as a Job in the release namespace, reusing the backend pod
   template — the `backend` ServiceAccount's `backend-workspaces`
   RoleBindings give it read on Workspace CRs in the managed namespaces,
   and the labels below place the pod under the existing DB/apiserver
   egress NetworkPolicies. Deriving the manifest from
   `deploy/backend`'s pod template keeps image, DSN env and TLS mounts in
   sync; a static equivalent:

   ```yaml
   apiVersion: batch/v1
   kind: Job
   metadata: {name: tinycdi-post-restore, namespace: <release-ns>}
   spec:
     backoffLimit: 0
     template:
       metadata:
         labels:
           # under the backend DB-egress + apiserver-egress policies; do
           # NOT add app.kubernetes.io/instance — the backend Service must
           # never select this pod.
           app.kubernetes.io/name: backend
           cdi.tinyorbit.vn/needs-apiserver: "true"
       spec:
         serviceAccountName: backend
         restartPolicy: Never
         containers:
         - name: post-restore
           image: <same image as deploy/backend>
           # first run with [] (dry-run) to read the plan, then:
           args: ["post-restore", "-apply", "-i-have-scaled-down"]
           env:
             - name: TCDI_DATABASE_URL
               valueFrom:
                 secretKeyRef:
                   {name: <database.existingSecret>, key: <database.urlKey>}
             - {name: PGSSLMODE, value: "<database.tls.mode>"}
             - {name: PGSSLROOTCERT, value: "/etc/db-ca/ca.crt"}
             - {name: TCDI_TENANT_NAMESPACES, value: "<tenant=ns,...>"}
           volumeMounts:
             - {name: db-ca, mountPath: /etc/db-ca, readOnly: true}
         volumes:
           - name: db-ca
             secret: {secretName: <database.tls.caSecret.name>}
   ```

   The DB steps need no ServiceAccount token at all
   (`automountServiceAccountToken: false` is fine): without Kubernetes
   read access the tool still rotates/revokes/denies, then prints the
   per-workspace `UPDATE` — with the `kubectl … -o jsonpath` that fills
   each CR-derived value — and exits non-zero so the incomplete run is
   not silently green. `backend post-restore` alone is a dry-run usable
   in the same Job shape before `-apply`.

   This step is what turns "dead within one TTL, if you're lucky" into
   "dead deterministically". Skipping it is the unsafe restore the drill
   below demonstrates.
4. **Scale `operator` and `backend` back up.** With the kill-SQL landed,
   resurrected access is already dead — the reconcile below needs the
   platform running (orphan-CR deletion runs the operator's finalizer,
   which itself calls the backend control surface).
5. **Reconcile `workspaces` against the live CRs** — in both directions:
   - **CR exists, row does not** (created after the dump): an orphan —
     running, unmetered, invisible to the API. Default: delete it,
     `kubectl delete workspace <cr-name> -n <tenant-ns>`; the operator
     finalizer runs teardown and a `Retain` disk survives via the
     retained path. Adopt only if you must keep the workload: insert the
     `workspaces` row (fresh `request_id`), a `held` `quota_reservation`
     matching the running vector, and the CR's intent revision.
   - **Row exists, CR does not** (deleted after the dump): a ghost —
     listed by the API, holding quota it cannot spend. Re-delete through
     the API (`DELETE /v1/workspaces/{id}` — the applier tolerates a
     missing CR), or mark the row `state='deleted'` in SQL.
   - **Both exist, intents diverged** (any start/stop/delete after the
     dump): the CR's `spec.intentRevision` and applied-intent revision
     are ahead of the restored row's `intent_revision`, and the operator
     adopts only `intentRevision > applied.revision`. Intents the API
     writes afterwards land at or under the applied revision and are
     **silently ignored** — the drill hit this: the restored row claimed
     `Running` over a `Stopped` CR, so `start` was refused by the API's
     own desired-state check. The step-3 Job already aligned every
     diverged row to its live CR (`desired_state`/`runtime_generation`/
     `intent_revision` from the CR's `spec`); when the Job ran without
     Kubernetes access it printed the equivalent `UPDATE` per row — apply
     those statements now, reading the values off the CRs:

     ```sql
     UPDATE workspaces
        SET desired_state      = '<CR spec.desiredState>',
            runtime_generation = <CR spec.runtimeGeneration>,
            intent_revision    = <CR spec.intentRevision>
      WHERE id = '<ws_...>';
     ```

     The dump-side desired state is *not* forced onto the cluster — the
     live CR is the truth for workload state; the row is aligned to
     describe it. A stopped-after-dump workspace stays stopped; users
     re-drive from there (each new intent is adoptable once the row no
     longer trails the CR).
6. **Rebuild retained inventory + reconcile quota** as in the rebuild
   path (steps 7–8): `RetainedSync`/`ImportRetained` for PVC-side truth,
   one recovery pass for held reservations, `tinycdi_quota_drift` at 0
   before declaring the restore done.
7. **Tell users:** every session died — log in again, then reconnect;
   work on the platform between dump and freeze is gone or reverted (see
   below).

### What users experience

- **Forced re-login, always.** Restored portal cookies fail
  `UNAUTHENTICATED` the moment the epoch rotates — that is the fix
  working, not breakage.
- **Every stream is dead.** There is no live-restore path; reconnect
  lands on the normal ticket/launch flow.
- **Post-dump changes are lost on the DB side and kept on the cluster
  side.** Sessions, sign-outs, quota edits and retained inventory revert
  to the dump; CR-side facts (a workspace stopped after the dump, a new
  workspace's pod) persist — the reconcile above aligns the record with
  them rather than rewinding workloads.
- **One benign wedge if step 5 is skipped:** intents on a diverged
  workspace are silently dropped until its `intent_revision` passes the
  CR's applied revision — a user-visible "button does nothing", not a
  security hole.

### Invariant checklist — live-cluster restore

Run after step 6, in addition to the rebuild list in backup-restore.md:

- [ ] `sessions` joined to `platform_meta.session_epoch` shows **zero**
      rows at the current epoch (`SELECT count(*) FROM sessions WHERE
      epoch = (SELECT value FROM platform_meta WHERE key='session_epoch')`).
- [ ] Zero `connection_lease` rows `state='active'` and zero
      `launch_ticket` rows `consumed_at IS NULL AND revoked_at IS NULL`
      predating the restore.
- [ ] `kubectl get workspaces -A` and `SELECT id FROM workspaces WHERE
      state='active'` agree on the workspace set — no orphans, no ghosts.
- [ ] Every `workspaces.intent_revision` >= the matching CR's
      applied-intent revision.
- [ ] `quota_reservation` `held` rows ↔ running runtimes + retained disks
      exactly once; `tinycdi_quota_drift` = 0.
- [ ] Fresh login → ticket → redeem → stream works on a live workspace.

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

A second drill (v0.5, kind quickstart — `hack/quickstart`) exercised the
**live-cluster** path: `pg_dump` while a portal session held a running
workspace with an active lease, then sign-out (S17 revoke) + a workspace
stop + a new workspace, then the dump loaded back over the running
release.

Observed, and now covered by "Postgres-only restore onto a live
cluster":

- **Naive restore resurrects a signed-out session.** With the dump
  loaded and no epoch rotation, the signed-out portal cookie answered
  `200` on `/v1/me` (idle window intact). After the epoch rotation in
  the procedure: `401`.
- **Naive restore resurrects a revoked lease inside its TTL.** The
  restored `connection_lease` row came back `state='active'` with its
  `session_digest`; the old gateway cookie served `200` on the session
  host until `expires_at` lapsed (~15 s of residual TTL observed). The
  unconditional `state='revoked'` update removes the window entirely.
- **Intent fence wedges on divergence.** The workspace stopped after
  the dump had CR `applied-intent` revision 2 vs restored row revision
  1: `POST /start` was refused by the API's own desired-state check
  ("Running" over a stopped CR) and, once bumped past, the next intent
  (revision 3) adopted and booted normally. The reconcile step exists
  precisely for this.
- **Orphan CR reconcile works.** The post-dump workspace had no
  `workspaces` row; deleting its CR ran the operator finalizer cleanly.
- Post-procedure: fresh login, ticket issue, redemption (303) and
  workspace list all healthy; held quota matched the running runtime;
  no resurrected sessions, leases or tickets.
