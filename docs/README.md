# TinyCDI documentation

## Getting started

- [quickstart.md](quickstart.md) — TinyCDI on a throwaway kind cluster in
  one command (dev only)
- [runbooks/install.md](runbooks/install.md) — install from the Helm chart
- [versioning.md](versioning.md) — the versioning contract: semantic
  versioning, what it covers, and how upgrades behave

## Using and operating

- [runbooks/audit.md](runbooks/audit.md) — the stored audit log: what is
  recorded, retention, and how to read it
- [runbooks/backup-restore.md](runbooks/backup-restore.md) — backup and restore
- [runbooks/capacity.md](runbooks/capacity.md) — sizing and capacity
  planning, with the measured load numbers
- [runbooks/disaster-recovery.md](runbooks/disaster-recovery.md) — DR procedure
- [runbooks/mtls-cert-revocation.md](runbooks/mtls-cert-revocation.md) —
  revoke one internal mTLS certificate (ADR 0008 serial blocklist)
- [runbooks/observability.md](runbooks/observability.md) — metrics,
  dashboards and alerts: enabling them and what each alert means
- [runbooks/retained-data.md](runbooks/retained-data.md) — Retain policy,
  re-attach and purge flows
- [runbooks/session-policy.md](runbooks/session-policy.md) — the tenant
  session data-transfer ceilings (clipboard and file transfer) — toggling
  them, when they take effect, verifying
- [runbooks/stuck-finalizer.md](runbooks/stuck-finalizer.md) — workspace
  teardown finalizer recovery
- [runbooks/tenant-quotas.md](runbooks/tenant-quotas.md) — declarative
  tenant quotas, per-user limits, the user directory, refusal codes,
  day-2 operations

## Reference

- [architecture.md](architecture.md) — the components of an install and
  where each lives in the repo
- [compatibility.md](compatibility.md) — pinned dependency/toolchain
  versions and digests
- [images.md](images.md) — runtime image contract: endpoints, credentials,
  healthcheck, pinned inputs
- [kasm-images.md](kasm-images.md) — running unmodified `kasmweb/*`
  workspace images as `LinuxContainer` runtimes
- [lifecycle-reasons.md](lifecycle-reasons.md) — workspace condition reasons
  behind the portal's lifecycle progress
- [branding.md](branding.md) — operator branding: ConfigMap layout, worked
  example, trademark note
- [development.md](development.md) — repo layout, build & test commands,
  local image builds, screenshot regeneration
- [../deploy/node-profiles/README.md](../deploy/node-profiles/README.md) —
  seccomp/AppArmor node profiles for browser workspaces

## Security

- [../SECURITY.md](../SECURITY.md) — security policy, private reporting,
  mandatory hardening rules
- [security/threat-model.md](security/threat-model.md) — assets, actors,
  trust boundaries, current controls
- [security/test-inventory.md](security/test-inventory.md) — security-relevant
  tests and CI scanners with paths
- [security/provenance.md](security/provenance.md) — release signing, SBOMs
  and SLSA provenance verification
- [security/vulnerability-policy.md](security/vulnerability-policy.md) —
  vulnerability handling policy

## Decisions

- [adr/0001-runtime-streaming.md](adr/0001-runtime-streaming.md) —
  KasmVNC over WebSocket/TLS behind the session gateway; 60 s launch
  tickets and incarnation-fenced leases
- [adr/0002-api-conventions.md](adr/0002-api-conventions.md) —
  API conventions: resources, custom methods, paging, errors, ETags, idempotency
- [adr/0003-gateway-broker-internal-api.md](adr/0003-gateway-broker-internal-api.md) —
  internal mTLS broker API: lease routes for gateways, revoke/drain for
  the operator CN
- [adr/0004-launch-origin-policy.md](adr/0004-launch-origin-policy.md) —
  launch POST origin policy: portal-origin allowlist plus fetch metadata;
  the ticket never travels in the query
- [adr/0005-backend-frontend-operator.md](adr/0005-backend-frontend-operator.md) —
  three components (backend, frontend, operator) and per-workspace
  session hosts
- [adr/0006-rate-limit-state.md](adr/0006-rate-limit-state.md) —
  Postgres-backed shared fixed-minute windows for the login/launch limits,
  local buckets as ceiling and floor
- [adr/0007-sign-out-everywhere.md](adr/0007-sign-out-everywhere.md) —
  principal-scoped revocation of all portal sessions, leases and tickets
- [adr/0008-mtls-cert-revocation.md](adr/0008-mtls-cert-revocation.md) —
  per-certificate revocation on the internal mTLS listener: a polled
  serial blocklist
- [adr/0009-session-data-transfer-policy.md](adr/0009-session-data-transfer-policy.md) —
  tenant ceilings for clipboard and file transfer, and the in-pod
  transfer helper
- [adr/0010-audit-store-and-retention.md](adr/0010-audit-store-and-retention.md) —
  the durable audit store, written in the mutation's transaction, with
  pseudonymous actors and 90-day retention

## Releases

- [releases/v1.0.2.md](releases/v1.0.2.md) — what 1.0.2 fixes (closing a
  session tab frees the workspace), how to upgrade from 1.0.1
- [releases/v1.0.1.md](releases/v1.0.1.md) — what 1.0.1 changes (portal and
  admin console polish), how to upgrade from 1.0.0, known issues
- [releases/v1.0.0.md](releases/v1.0.0.md) — what 1.0.0 contains, how to
  install it, known limitations
