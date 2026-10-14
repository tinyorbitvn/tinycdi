# Versioning

TinyCDI follows [semantic versioning](https://semver.org). Releases are
`MAJOR.MINOR.PATCH`. A pre-release carries a `-rc.N` suffix: it is never
marked latest, and its images get no `:latest` tag. A minor release adds
features within the contract below; a patch release carries bug and
security fixes.

## The 1.x contract — breaking changes only in a new major

- **The `/v1` HTTP API**, as described by
  [`internal/api/openapi.yaml`](../internal/api/openapi.yaml), with the
  conventions of [ADR 0002](adr/0002-api-conventions.md). A minor release
  may add operations, fields and enum values, so clients must tolerate
  unknown fields and values. It never removes an existing one or changes
  its meaning.
- **The Helm chart values**
  ([chart README](../deploy/helm/tinycdi/README.md)). A value is deprecated
  for at least one minor release, with a documented replacement, before a
  major release removes it.
- **The CRDs** `workspaces.cdi.tinyorbit.vn/v1` (`Workspace`,
  `WorkspaceTemplate`). Create and edit `Workspace` objects only through
  the API. The CRD schema follows the same additive rule as the API.

## Not covered by the contract

- The internal mTLS broker API on the backend's `:9443` listener, which
  the operator calls ([ADR 0003](adr/0003-gateway-broker-internal-api.md)).
  Both components ship in the same release and are upgraded together.
- Prometheus metric names and labels, and the log and audit field layout.
  These are best effort; the release notes list every change.
- Container image internals: the filesystem layout, entrypoints and
  tooling inside the published `tinycdi-*` images.

## Upgrading within 1.x

Read the release notes of the target release first. Then take the backup
set described in [backup-restore.md](runbooks/backup-restore.md). A
downgrade is a restore from that backup: the backend applies database
migrations when it starts, and it refuses to start on a database that
holds a migration it does not embed, for example one written by a newer
release.

Helm never upgrades the objects in `crds/`, so diff and apply the CRDs
yourself after you review the diff. Only additive schema changes are safe
to apply under running objects. Pin images by digest
(`images.*.digest`), never by tag, and verify them first with the
`cosign verify` command in
[provenance.md](security/provenance.md#verification-side-what-a-consumer-runs).
Then render and diff the release, upgrade it, and watch the rollouts. The
order with the least user impact is CRDs, then backend, then operator,
then frontend. Seeded templates diff as a create and delete pair of
`<name>-<hash8>` revision objects, never as an in-place change.

```bash
# 1. CRDs (Helm does not upgrade crds/)
kubectl diff -f deploy/helm/tinycdi/crds/
kubectl apply -f deploy/helm/tinycdi/crds/      # only after you review the diff

# 2. Render and diff, then upgrade
helm template tinycdi oci://ghcr.io/tinyorbitvn/charts/tinycdi --version <1.x.y> \
  -n tinycdi-system -f my-values.yaml | kubectl diff -f -
helm upgrade tinycdi oci://ghcr.io/tinyorbitvn/charts/tinycdi --version <1.x.y> \
  -n tinycdi-system -f my-values.yaml

# 3. Watch the rollouts in order
for d in backend operator frontend; do
  kubectl -n tinycdi-system rollout status "deployment/$d"
done
```

**Session impact.** With the default `backend.replicas: 2` and
`maxUnavailable: 0`, a rollout replaces one backend pod at a time. A
client whose stream drops reconnects with the same cookie inside its live
lease; users must relaunch only when every backend replica is down for
longer than the 30-second lease TTL. A stopping backend fails readiness,
waits `-drain-propagation-delay` (default 5 s), then drains its streams
and keeps serving for `-drain-window` (default 8 s). The frontend drains
for `frontend.drainDelay` (default 8 s), so page loads land on a live
replica.

**Argo CD.** Apply `crds/` first, outside the application or in an
earlier sync wave. Then hard-refresh the application
(`argocd app get <app> --hard-refresh`) before it syncs objects that use
new fields: Argo CD diffs against the CRD schemas it has cached.

**Post-checks.** `tinycdi_lease_failures_total` is back to its baseline,
`tinycdi_quota_drift` is 0, and one create → connect → delete round trip
succeeds.

## Supported versions

See [SECURITY.md](../SECURITY.md#supported-versions).
