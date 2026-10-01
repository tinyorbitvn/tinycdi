# TinyCDI documentation

- [architecture.md](architecture.md) — platform design (original design
  doc, kept for reference) with the current component overview; decisions
  are tracked in `adr/`
- [images.md](images.md) — runtime image contract: endpoints, credentials,
  healthcheck, pinned inputs
- [compatibility.md](compatibility.md) — pinned dependency/toolchain
  versions and digests
- [development.md](development.md) — repo layout, build & test commands,
  local image builds, screenshot regeneration

## Runbooks

- [runbooks/install.md](runbooks/install.md) — install from the Helm chart
- [runbooks/upgrade.md](runbooks/upgrade.md) — upgrade ordering and rollback
- [runbooks/retained-data.md](runbooks/retained-data.md) — Retain policy,
  re-attach and purge flows
- [runbooks/backup-restore.md](runbooks/backup-restore.md) — backup and restore
- [runbooks/disaster-recovery.md](runbooks/disaster-recovery.md) — DR procedure
- [runbooks/capacity.md](runbooks/capacity.md) — sizing and capacity planning
- [runbooks/stuck-finalizer.md](runbooks/stuck-finalizer.md) — workspace
  teardown finalizer recovery

## Security

- [../SECURITY.md](../SECURITY.md) — security policy, private reporting,
  mandatory hardening rules
- [security/vulnerability-policy.md](security/vulnerability-policy.md) —
  vulnerability handling policy
- [security/provenance.md](security/provenance.md) — release signing, SBOMs
  and SLSA provenance verification

## ADRs

- [adr/0001-runtime-streaming.md](adr/0001-runtime-streaming.md) — runtime
  streaming protocol choice
- [adr/0002-public-api.md](adr/0002-public-api.md) — public API surface
- [adr/0003-gateway-broker-internal-api.md](adr/0003-gateway-broker-internal-api.md) —
  gateway ↔ broker internal API
- [adr/0004-launch-origin-policy.md](adr/0004-launch-origin-policy.md) —
  launch origin enforcement
- [adr/0005-backend-frontend-operator.md](adr/0005-backend-frontend-operator.md) —
  v0.2 target: three components and per-workspace session hosts (proposed)

## Elsewhere in the repo

- [../deploy/helm/tinycdi/README.md](../deploy/helm/tinycdi/README.md) —
  Helm chart reference (values, topology, node profiles)
- [../deploy/node-profiles/README.md](../deploy/node-profiles/README.md) —
  seccomp/AppArmor node profiles for browser workspaces
- [../.github/README.md](../.github/README.md) — CI/release pipeline notes
