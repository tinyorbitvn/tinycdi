# Versioning — the 1.0 contract

From **v1.0.0** TinyCDI follows [semantic versioning](https://semver.org):
releases are `MAJOR.MINOR.PATCH`. A breaking change to a stable surface
ships only in a new major release; additive changes ship in minors;
bug and security fixes ship in patches.

## Stable surfaces — breaking changes only in a new major

- **The `/v1` HTTP API**, as described by the OpenAPI document
  ([`internal/api/openapi.yaml`](../internal/api/openapi.yaml)). Minor
  releases may add operations, fields and enum values — clients must
  tolerate additions — but never remove or change the meaning of an
  existing one.
- **The Helm chart values**
  ([reference](../deploy/helm/tinycdi/README.md)). A value is marked
  deprecated for at least one minor release, with a documented
  replacement, before it is removed.
- **The documented upgrade path** — from the previous `1.x` minor, and
  from `v0.5.1` to `1.0.0` — as described in
  [runbooks/upgrade.md](runbooks/upgrade.md).

## Not covered by the contract

- **The `Workspace`/`WorkspaceTemplate` CRD schema** stays at
  `v1alpha1`: an operator-internal contract, not a public API. Promotion
  to a stable version with conversion is planned for a later release.
  Do not create or edit `Workspace` objects directly except through the
  documented runbooks.
- **The internal mTLS control API** between operator and backend
  ([ADR 0003](adr/0003-gateway-broker-internal-api.md)) — an
  implementation detail between components that are deployed and
  upgraded together.
- **Prometheus metric names and the log/audit field layout** — best
  effort; changes are noted in the release notes.
- **Container image internals** — the filesystem layout, entrypoints and
  tooling inside the published `tinycdi-*` images.

## Supported versions

The support policy lives in
[SECURITY.md](../SECURITY.md#supported-versions): the latest `1.x`
minor is fully supported (security and bug fixes); `0.5.x` receives
security-only fixes (Critical/High) for 90 days after the v1.0.0 final
release; `0.4` and older are unsupported as of 1.0.0. The supported
upgrade path is `v0.5.1` → `1.0.0`.
