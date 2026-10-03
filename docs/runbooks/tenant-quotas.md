# Runbook — Tenant quotas

Tenant quotas are **declarative**: you state each tenant's limits in the
chart values and the platform keeps the `tenant_quota` rows in Postgres in
step. No SQL is needed for normal operations.

## Where quotas come from

Each `managedNamespaces[]` entry may carry a `quota` block:

```yaml
managedNamespaces:
  - name: tinycdi-tenant-a        # namespace the platform manages
    tenant: tenant-a              # tenant id (OIDC-derived)
    quota:
      runningWorkspaces: 12       # whole number >= 0 — concurrent Running slots
      cpu: "16"                   # cores or millicores: "16", "1.5", "500m"
      memory: 64Gi                # Kubernetes quantity
      storage: 200Gi              # retained + active disks, Kubernetes quantity
```

The chart renders the quota blocks as the backend flag `-tenant-quotas`
(JSON). At startup the **singleton leader** — the replica holding the
Postgres leader lock — upserts exactly those tenants' rows; replicas
starting together write once, and the pass is idempotent (an identical
row is not rewritten).

Key properties:

- **Declared values win.** For a tenant with a `quota` block, the chart
  value overwrites a hand-edited row at the next backend start. Change
  `values.yaml`, not the database.
- **Listed tenants only.** Removing a `quota` block (or omitting one)
  leaves the existing row untouched — declarative config adds and updates
  rows; it never deletes them. To retire a tenant's quota entirely, delete
  its `tenant_quota` row after removing the block.
- **Lowering is safe.** A limit below current usage is accepted: running
  workspaces keep running and keep their reservations; new creates are
  refused until usage drops under the limit.
- **Validation.** `values.schema.json` rejects negative or unparsable
  quantities at `helm template` time; the backend refuses to start on a
  malformed `-tenant-quotas`.

Apply a change with `helm upgrade` — the backend rolls and the new leader
upserts on start. Verify in the leader's log:

```bash
kubectl -n <release-ns> logs -l app.kubernetes.io/name=backend --tail=-1 | grep "tenant quotas applied"
```

## Reading quota state

- `GET /v1/quota` returns the caller's tenant snapshot: `configured`
  (false = no quota row — **not** "unlimited"; creates are refused),
  `limits`, `usage`, and a per-user `users` breakdown. Tenant
  administrators see every user; regular users see only their own row.
- Metrics (with `backend.metrics.enabled` — see `observability.md`):
  `tinycdi_workspaces_running{tenant}` vs `tinycdi_workspaces_reserved{tenant}`
  for the live picture, and `tinycdi_quota_drift{tenant}` which should
  stay `0` — a non-zero value means reservations and actual usage
  disagree; investigate before raising limits.

## What users see when quota refuses

| Situation | API answer | Retryable? | Meaning |
|---|---|---|---|
| Tenant has no quota row | `409 QUOTA_NOT_CONFIGURED` | no | admission fails closed; an administrator must declare a quota (chart `quota` block). The user-facing message says exactly that |
| Over a configured limit | `409 QUOTA_EXHAUSTED` | no | genuine headroom shortage — free resources or raise the limit |
| Over limit only because of teardown-pending holds | `409 QUOTA_EXHAUSTED` + `details.reason: release_pending` + `Retry-After` | **yes** | a deleted or stopped workspace still holds the reservation pending the runtime-absence proof; the release is usually within seconds (event-driven settle) and at most ~30 s (the recovery tick is the catch-all), so the portal can retry automatically |

Quota accounting detail: compute (`runningWorkspaces`, `cpu`, `memory`) is
held while a runtime incarnation exists — from start intent until the pod
is proven gone. Disk (`storage`) is held while the volume exists. A
**Stopped** workspace under the Retain data policy releases compute and
keeps only a disk-only hold; Start re-reserves the vector it was admitted
with. Retained disks keep their storage quota until purge.

## Operations checklist

- **New tenant onboarded:** add the `managedNamespaces[]` entry with a
  `quota` block — without it every create fails `QUOTA_NOT_CONFIGURED`.
  Install/upgrade NOTES print a warning for every entry missing `quota`.
- **Tenant reports `QUOTA_EXHAUSTED`:** check `GET /v1/quota` usage vs
  limits, and whether refusals carry `release_pending` (transient — no
  action; the hold usually clears within seconds, at most ~30 s).
  Otherwise raise the block or have
  the tenant free workspaces.
- **Quota row looks wrong:** the next backend start rewrites declared
  tenants from values — fix the values, not the row. For undeclared
  leftovers, delete the `tenant_quota` row directly.
- **Sizing:** keep the sum of quotas inside what the cluster and any
  namespace `ResourceQuota` can actually schedule — see `capacity.md`.
