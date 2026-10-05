# TinyCDI documentation

- [architecture.md](architecture.md) — platform design (original design
  doc, kept for reference) with the current component overview; decisions
  are tracked in `adr/`
- [images.md](images.md) — runtime image contract: endpoints, credentials,
  healthcheck, pinned inputs
- [lifecycle-reasons.md](lifecycle-reasons.md) — workspace condition reasons
  behind the portal's lifecycle progress
- [compatibility.md](compatibility.md) — pinned dependency/toolchain
  versions and digests
- [development.md](development.md) — repo layout, build & test commands,
  local image builds, screenshot regeneration
- [quickstart.md](quickstart.md) — TinyCDI on a throwaway kind cluster in
  one command (dev only)
- [branding.md](branding.md) — operator branding: ConfigMap layout, worked
  example, trademark note

## Runbooks

- [runbooks/install.md](runbooks/install.md) — install from the Helm chart
- [runbooks/upgrade.md](runbooks/upgrade.md) — upgrade ordering and rollback
- [runbooks/observability.md](runbooks/observability.md) — metrics,
  dashboards and alerts: enabling them and what each alert means
- [runbooks/tenant-quotas.md](runbooks/tenant-quotas.md) — declarative
  tenant quotas, refusal codes, day-2 operations
- [runbooks/retained-data.md](runbooks/retained-data.md) — Retain policy,
  re-attach and purge flows
- [runbooks/backup-restore.md](runbooks/backup-restore.md) — backup and restore
- [runbooks/disaster-recovery.md](runbooks/disaster-recovery.md) — DR procedure
- [runbooks/capacity.md](runbooks/capacity.md) — sizing and capacity planning
- [runbooks/stuck-finalizer.md](runbooks/stuck-finalizer.md) — workspace
  teardown finalizer recovery

## Releases

- [releases/v0.3.0.md](releases/v0.3.0.md) — v0.3.0 release notes
  (source for the GitHub release)
- [releases/v0.3.1.md](releases/v0.3.1.md) — v0.3.1 release notes
- [releases/v0.3.2.md](releases/v0.3.2.md) — v0.3.2 release notes
- [releases/v0.4.0.md](releases/v0.4.0.md) — v0.4 release notes
  (in progress; assembled ahead of the tag)

## Security

- [../SECURITY.md](../SECURITY.md) — security policy, private reporting,
  mandatory hardening rules
- [security/vulnerability-policy.md](security/vulnerability-policy.md) —
  vulnerability handling policy
- [security/provenance.md](security/provenance.md) — release signing, SBOMs
  and SLSA provenance verification
- [security/threat-model.md](security/threat-model.md) — v1.0 security
  review preparation: assets, actors, trust boundaries, current controls
- [security/test-inventory.md](security/test-inventory.md) — security-relevant
  tests and CI scanners with paths

## ADRs

- [adr/0001-runtime-streaming.md](adr/0001-runtime-streaming.md) — runtime
  streaming protocol choice
- [adr/0002-public-api.md](adr/0002-public-api.md) — public API surface
- [adr/0003-gateway-broker-internal-api.md](adr/0003-gateway-broker-internal-api.md) —
  gateway ↔ broker internal API
- [adr/0004-launch-origin-policy.md](adr/0004-launch-origin-policy.md) —
  launch origin enforcement
- [adr/0005-backend-frontend-operator.md](adr/0005-backend-frontend-operator.md) —
  v0.2: three components and per-workspace session hosts (accepted)
- [adr/0006-rate-limit-state.md](adr/0006-rate-limit-state.md) —
  rate-limiter state placement: per-replica vs Postgres-backed options
  (proposed — decision pending for v0.4)
- [adr/0008-kasmvnc-bruteforce.md](adr/0008-kasmvnc-bruteforce.md) —
  KasmVNC endpoint brute-force posture: reachability and credential
  channel analysis closing threat-model S18 (accepted — no build)

## Elsewhere in the repo

- [../deploy/helm/tinycdi/README.md](../deploy/helm/tinycdi/README.md) —
  Helm chart reference (values, topology, node profiles)
- [../deploy/node-profiles/README.md](../deploy/node-profiles/README.md) —
  seccomp/AppArmor node profiles for browser workspaces
- [../.github/README.md](../.github/README.md) — CI/release pipeline notes
