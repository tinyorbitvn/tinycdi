# Stuck workspace finalizer — break-glass runbook

Design: `docs/architecture.md` §5. A Workspace CR carries the
`workspaces.cdi.tinyorbit.vn/runtime-cleanup` finalizer so deletion runs the
mandated teardown order. When teardown wedges, the object sits
`Terminating` forever. This runbook is the *explicitly destructive* escape
hatch — prefer fixing the wedge to forcing the drop.

## Teardown order (what the finalizer is doing)

```
block-connects → revoke-leases → drain-streams (≤45 s) →
stop-runtime → retention → cleanup → done
```

Progress is persisted on the object in the
`workspaces.cdi.tinyorbit.vn/finalizer-progress` annotation (JSON
`FinalizerProgress`). A blocked step sets the `Degraded` condition with a
reason (`RetentionPending`, `DrainTimedOut`, `CleanupRetry`, …) — read the
condition and the annotation **before** doing anything else:

```sh
kubectl -n <ns> get workspace <name> -o jsonpath='{.status.conditions}'
kubectl -n <ns> get workspace <name> -o jsonpath='{.metadata.annotations.workspaces\.tinycdi\.dev/finalizer-progress}'
```

## Step-by-step unblock

| Stuck at | Likely cause | Non-destructive fix |
|---|---|---|
| `revoke-leases` | broker unreachable | restore broker connectivity; step retries |
| `drain-streams` | gateway never closes streams | bounded at 45 s — if it persists past the budget, the finalizer proceeds anyway; if it does not, the operator is wedged, restart it |
| `stop-runtime` | runtime backend (KubeVirt/runtime) error | fix backend; verify Pod/VMI actually gone |
| `retention` | PVC/CSI errors, foreign PVC (`ErrForeignPVC`) | fix storage; verify the PVC's `workspace-uid` label matches the workspace UID |
| `cleanup` | leftover Service/Secret/PVC children | delete the named children manually; each is UID-labelled |

If a step keeps failing, the fix is in the failing dependency, not the
finalizer.

## Break-glass: force-removing the finalizer

Only when the workspace must be deleted and the blocked step cannot be
fixed (e.g. the storage backend is permanently gone):

```sh
kubectl -n <ns> patch workspace <name> --type=merge \
  -p '{"metadata":{"finalizers":[]}}'
```

**This skips every remaining teardown step. You are accepting the
consequences below — write down which step you bypassed and what data you
left behind.**

### Data consequences (by step skipped)

- **`retention` skipped, workspace was `dataPolicy: Retain`:** the home
  PVC survives but was never stamped into the retained inventory — it is
  an *unregistered orphan*. It still exists, still costs storage, and is
  invisible to `GET /v1/data`. To recover: label it
  `workspaces.cdi.tinyorbit.vn/data-retained=true` and set the
  `retained-tenant`/`retained-owner`/`retained-source-workspace`/
  `retained-runtime`/`retained-at` annotations by hand (see
  `retained-data.md`), then import it; or delete the PVC yourself if the
  data is disposable. A PVC left unlabeled is unowned garbage.
- **`retention` skipped, `Ephemeral`:** the data volume may still exist —
  deleting the namespace will take it, otherwise delete the PVC manually
  or storage leaks.
- **`cleanup` skipped:** Service, runtime Secret and NetworkPolicy objects
  for the workspace UID may remain. They are inert (no runtime behind
  them) but hold ports/names; delete by label
  `workspaces.cdi.tinyorbit.vn/workspace-uid=<uid>`.
- **`stop-runtime` skipped:** the Pod/VMI may still be RUNNING — verify
  and kill it manually, or you have an unaccounted workload consuming the
  freed quota reservation.

### Routing consequences

- **`block-connects`/`revoke-leases` skipped:** existing connection leases
  die with the runtime anyway (they bind runtimeUID, which is gone), but
  up to the lease TTL a gateway may still forward to a ghost. Expect a
  few minutes of failed connects; no security exposure — the lease stops
  renewing and expires.
- **Never** remove the finalizer to "speed up" a normal delete; the drain
  window exists so streams close cleanly.

### Quota consequence

The workspace's `quota_reservation` is released only on a runtime-absence
proof. Force-dropping the finalizer does not release it automatically —
confirm the runtime is really gone, then let the recovery path settle the
reservation (`ProofRuntimeAbsent`), or the tenant's quota permanently
shrinks by one slot.

## Prevention checklist

- Alert on `Degraded` with finalizer reasons and on workspaces stuck
  `Terminating` past a few minutes.
- Keep the broker/gateway/storage health dependencies observable — every
  stuck finalizer has a wedged seam behind it.
- Record every break-glass removal (who, which step, leftover objects) in
  the incident log.
