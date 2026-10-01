# Security Policy

TinyCDI runs other people's desktops. A defect in the API, gateway, broker
or operator can expose one user's workspace, credentials or network
position to another. This file sets out how to report a vulnerability and
the hardening rules that contributors and operators must follow.

## Supported versions

| Version | Supported |
|---|---|
| `main` (unreleased) | Yes. Fixes land here first. |
| Latest `0.x` minor release (images + `oci://ghcr.io/tinyorbitvn/charts/tinycdi`) | Yes. Security fixes ship as a new patch release. |
| Older `0.x` minors | No. Upgrade to the latest minor. |

While the project is pre-1.0, only the most recent minor release line gets
security fixes. Deploy images by digest (`images.*.digest`), not by tag:
a fix ships as a new release with new digests.

## Reporting a vulnerability

**Do not open a public issue, discussion or pull request for a security
problem.**

Report it privately through GitHub's private vulnerability reporting:
**<https://github.com/tinyorbitvn/tinycdi/security/advisories/new>**
(repository **Security** tab → **Report a vulnerability**). This opens a
draft GitHub Security Advisory that only you and the maintainers can see.

Please include:

- the affected component (`api`, `gateway`, `broker`, `operator`,
  `portal`, runtime image, Helm chart, CI) and the version, commit or
  image digest;
- the trust boundary you crossed (see the threat model below) and the
  attacker position you assumed: unauthenticated, an authenticated user,
  a tenant controlling their own workspace, or a cluster-namespace user;
- reproduction steps or a proof of concept. Keep it minimal, use your own
  test deployment, and never use other people's data.

Never include real credentials, session cookies, launch tickets or
customer data in a report. Redact them.

### Disclosure timeline

| Step | Target |
|---|---|
| Acknowledge the report | 3 business days |
| Triage: severity (CVSS v3.1) and affected versions | 7 calendar days |
| Fix or mitigation for Critical/High | 30 days |
| Fix or mitigation for Medium/Low | 90 days |
| Publish the GitHub Security Advisory (CVE requested through GitHub) | When the fix ships, or at 90 days, whichever comes first, unless both sides agree to an extension |

We credit reporters in the advisory unless they ask us not to. Good-faith
research that follows this policy is welcome: stay within your own
deployment and accounts, do not degrade service for others, and do not
access, modify or keep other users' data.

## Threat model in one paragraph

Browsers authenticate to the **portal/API** with OIDC (server-side
session, `__Host-` cookie, synchronizer CSRF token, Origin allowlist). The
portal mints a one-use, hash-stored, 60 s **launch ticket**, which the
browser POSTs cross-site to the **session gateway**. The gateway redeems it
over an internal **mTLS broker API** (ADR 0003). That yields a fenced
**lease**, renewed every 10 s, and the gateway fails closed within 30 s if
renewal stops. The gateway then reverse-proxies HTTP/WebSocket traffic to
the workspace **runtime pod**, injecting per-workspace credentials and
pinning the per-workspace TLS certificate. The **operator** creates the
runtime pods, Secrets, Services and NetworkPolicies in managed namespaces.
Treat every runtime pod as **tenant-controlled**: the user has a shell
inside it, can read its credentials and can replace the server it runs.
Containers share the host kernel, so namespaces are not a boundary between
mutually hostile tenants (see `docs/architecture.md` §6).

Background: [architecture](docs/architecture.md) ·
[ADR 0003 internal broker API](docs/adr/0003-gateway-broker-internal-api.md) ·
[ADR 0004 launch origin policy](docs/adr/0004-launch-origin-policy.md) ·
[vulnerability policy (proposal)](docs/security/vulnerability-policy.md) ·
[release provenance](docs/security/provenance.md) ·
[node profiles](deploy/node-profiles/README.md).

## Hardening rules

The rules below are **mandatory**. Each one names where it is enforced
today. **Not yet enforced** means the rule still applies but nothing
automated checks it; reviewers must check it by hand, and adding the
missing guard is welcome work. Changing a rule needs a security review
and an update to this file.

### A. Contributors

**Authentication and authorization**

1. Every `/v1` route except `GET /v1/login` and `GET /v1/auth/callback`
   is wrapped in `RequireAuth`, and every state-changing route is also
   wrapped in `RequireCSRF`.
   *Enforced:* `TestUnauthenticatedRequestRejected`,
   `TestCreateConnection_RequiresAuthAndCSRF`, `TestData_CSRFRequired`,
   `TestData_Unauthenticated`.
2. Owner and tenant come **only** from the verified `Principal`, never
   from the body, query or headers. Another user's or tenant's resource
   returns `404`, so the response never reveals that it exists.
   *Enforced:* `TestOwnerDerivedFromPrincipalNotBody`,
   `TestCreateWorkspaceOwnerFromPrincipal`, `TestWorkspaceOwnership`,
   `TestUnknownTenantForbidden`, `TestSEC21_IdempotencyPrincipalScoped`
   (idempotent replays too).
3. A request field that references an existing resource (for example
   `retainedDataRef`, `templateRef`) is resolved through the same
   owner-scoped, state-checked, transactional path as that resource's
   own endpoint. Appliers re-verify the claim **before** they mutate
   anything in Kubernetes.
   *Enforced:* `TestCreateRetainedDataRef_RoutesThroughAttach`,
   `TestCreateRetainedDataRef_ErrorMapping`,
   `TestSEC01_CreateRejectsBareRef`, `TestSEC01_UnclaimedRecordNeverMutated`
   (integration).
4. OIDC uses the authorization-code flow with PKCE S256. `state` is
   single-use **and bound to the initiating browser** (a short-lived
   `__Host-` cookie). The nonce is verified, and go-oidc checks issuer
   and audience. ID and access tokens are never stored or logged.
   *Enforced:* `TestCallbackRejects*` (issuer, audience, expiry, nonce,
   unknown/replayed state), `TestCallbackRequiresLoginCookie`,
   `TestCallbackRejectsWrongLoginCookie`, `TestLoginCookieAttributes`,
   `TestLoginSendsPKCES256`. An optional login gate (`-required-groups`,
   chart `oidc.requiredGroups`) admits only principals whose verified
   `groups` claim contains a listed group **verbatim**: no path matching,
   and a missing or malformed claim is denied with a 403 and no session.
   *Enforced:* `TestRequiredGroups*`, `TestNewAuthenticatorRejectsEmptyRequiredGroup`,
   `TestGroupListValidation`, `TestHardeningOIDCRequiredGroups`.
5. Sessions use opaque IDs of at least 256 bits, rotated at login.
   Cookies are `__Host-`, `Secure`, `HttpOnly` and have no `Domain`.
   Idle and absolute expiry are enforced server-side and bound to the
   session epoch. The database stores only a hash of the session ID
   (and only a MAC of the CSRF token).
   *Enforced:* `TestSessionFixationPrevented`,
   `TestSessionIdleAndAbsoluteExpiry`, `TestOIDCLoginFlowSucceeds`,
   `TestSessionKeyIsDigest`, `TestCSRFTokenMACIsKeyedBySessionID`,
   `TestPGSessionStore` (integration).
6. Unauthenticated endpoints use bounded memory and no global O(n) work
   per request. Anything stored before authentication has a cap and a
   TTL.
   *Enforced:* `TestPendingLoginsAreBounded`, `TestPendingSweepDropsExpired`.
   A flood can still evict in-flight logins once the cap is reached, so
   rate-limit `/v1/login` at the edge (§B.2).

**CSRF and Origin**

7. Nothing changes state on `GET`, `HEAD` or `OPTIONS`. A cookie-
   authenticated mutation needs the synchronizer token, compared in
   constant time against the server-side copy, and must pass
   `RequireTrustedOrigin`.
   *Enforced:* `TestCSRF*`, `TestOriginAllowlist`,
   `TestOriginMultipleHeadersRejected`, `TestCSRFChainForgedOriginRejected`.
8. On the session origin, only `POST /v1/launch` accepts the
   portal-origin allowlist (ADR 0004). WebSocket upgrades and every other
   route require `Origin` to equal the public origin. A rejected launch
   never consumes the ticket. Because the launch POST is cross-site:
   - the portal CSP `form-action` lists the session origin;
   - the portal `Referrer-Policy` must keep the `Origin` header (use
     `strict-origin`). With `no-referrer` or `same-origin`, browsers
     send `Origin: null` and every launch is rejected;
   - the gateway session cookie is `__Host-`, `Secure`, `HttpOnly`,
     `SameSite=Lax`. A `Strict` cookie is never sent on the redirect
     that follows the cross-site POST.

   *Enforced:* `TestLaunch_*` (incl. `TestLaunch_SetsHostOnlyCookie`),
   `TestUpgrade_OriginEnforced`, `TestUpgrade_PortalOriginRejected`,
   `TestPortalCSP`, `TestSecurityHeaders` (`Referrer-Policy: strict-origin`),
   `TestPortalSessionOrigin`, and `web/tests-portal/portal-csp.spec.ts`
   (the real portal binary against a session mock that applies the
   gateway's Origin gate, with a `no-referrer` negative control).

**Tickets, leases and the internal broker**

9. Launch tickets:
   - come from `crypto/rand`, at least 256 bits;
   - are persisted only as a SHA-256 hash;
   - have a TTL of 60 s or less;
   - are consumed on the first redemption attempt;
   - are bound to (workspace, tenant, owner, runtimeGeneration,
     runtimeUID, audience);
   - never appear in URLs, logs, metrics or audit records.

   *Enforced:* `TestIssueTicket_*`, `TestRedeemTicket_*`,
   `TestLaunch_TicketInQueryRejected`, `TestCreateConnection_NoTicketInLogs`,
   `TestCreateConnection_IssuesTicket` (`Cache-Control: no-store`). The
   SPA posts a ticket only to the configured session origin
   (`TestNormalizeSessionOrigin`, `web/tests/unit/connect.test.tsx`).
10. Leases:
    - at most one is active per workspace (DB unique index);
    - each is fenced by generation + runtimeUID + fencingVersion and
      bound to one gateway identity;
    - renewal happens at least every 10 s, and the session fails closed
      within 30 s;
    - the broker refuses issue and renew when its observed state is
      older than 15 s.

    *Enforced:* `TestRenewLease_*`, `TestLease_Stale*`,
    `TestTakeover_FencesOldLeaseFirst`, `TestSingleActiveLeaseAcrossGateways`,
    `TestRevoke_ClosesOpenStream`, `TestRenewLease_StaleObservationFailsClosed`.
11. The broker API is served only on the internal mTLS listener (TLS 1.3,
    client certificate required) and never on a public mux. Operator and
    gateway identities are disjoint.
    *Enforced:* `TestNoClientCert_401`, `TestWrongClientCA_Rejected`,
    `TestSPIFFESAN_*`, `TestActivity_OperatorIdentityForbidden`,
    `TestOperatorEndpoints_GatewayForbidden`.

**Gateway proxy**

12. The upstream target and its credentials come only from the broker.
    The gateway strips the client's `Authorization` and `Cookie` headers.
    Upstream TLS is verified against the pinned per-workspace
    certificate. `InsecureSkipVerify` never appears in non-test code.
    *Enforced:* `TestProxy_ArbitraryTargetRejected`,
    `TestProxy_StripsClientAuth`, `TestUpstreamTLS_RequiresPinnedCA`.
    `InsecureSkipVerify` ban: *not yet enforced* by a linter.
13. Every runtime response is tenant-controlled:
    - the path allowlist is evaluated on the normalized path (no
      dot-segments or encoded separators);
    - upstream `Set-Cookie`, `Service-Worker-Allowed` and similar
      origin-wide headers are dropped;
    - the gateway adds its own CSP, `nosniff` and HSTS.

    *Enforced:* `TestProxy_ManagementPathsDenied`,
    `TestProxy_DotSegmentsRejected`, `TestProxy_ForwardsCleanedPath`,
    `TestProxy_HostileUpstreamHeadersDropped`,
    `TestProxy_ServiceWorkerScriptFetchRejected`,
    `TestProxy_SecurityHeadersEverywhere`. Runtime HTML/JS still runs on
    the shared session origin (the KasmVNC client needs inline scripts),
    so serving the pinned client assets from the gateway remains
    *not yet enforced*.

**Input validation and data**

14. Every JSON body is read through `http.MaxBytesReader` with
    `DisallowUnknownFields`. IDs must match anchored patterns. Errors use
    the stable error model and expose no SQL or Kubernetes internals.
    *Enforced:* `TestData_BadIDs`, `TestErrorCodesMatchOpenAPI`,
    `TestCreateWorkspaceErrorMapping`, `TestCreateDecoderErrorNotEchoed`,
    `TestDataDecoderErrorNotEchoed`, `TestSignalBodyValidated`,
    `TestListWorkspaces_BadPageToken`.
15. SQL goes only through pgx placeholders. Request input is never
    interpolated; the only formatted values are integer `LIMIT`s.
    *Not yet enforced* (code review only).
16. End users never supply images, commands, PodSpec fragments, hostPath
    or privileges. Templates are admin-only, their images are
    digest-pinned and their specs are CEL-immutable. The operator
    re-validates whatever it builds a Pod from.
    *Enforced:* CRD CEL + `api/v1alpha1/validation_test.go`,
    `TestImageDigestPinning`, `TestForgedSnapshot*`
    (`internal/operator/snapshot_verify_test.go`: hash, digest-only
    image, CEL invariants, live-template match). A snapshot written by a
    Workspace writer that names a template which no longer exists is
    still trusted on hash + invariants. Operator-authenticated snapshots
    (status or signature): *not yet enforced.*
17. Idempotency keys are scoped to the principal and the operation, and
    a replay never bypasses authorization.
    *Enforced:* `TestSEC21_IdempotencyPrincipalScoped` (integration).
    Keys expire after 24 h (hourly prune in `cmd/api`).

**Logging and secrets**

18. Never log, or emit to metrics or audit, any of: tickets, session IDs,
    cookies, `Authorization`, OIDC code/state/nonce/verifier, runtime
    credentials, raw query strings or request bodies. Audit actors are
    pseudonymous (`observability.ActorRef`).
    *Enforced:* `TestAuditLogNeverContainsSecrets`, `TestNoSecretsInLogs`,
    `TestAuditRedactsSecretLikeFields`, `TestWorkspaceHandlerNoSecretsInLogs`,
    `TestLogsNeverContainRawSubject`, `TestNoLeaseIDsInLogs`.
19. Secrets reach a process only as a mounted file or as `secretKeyRef`
    env. They never go in command-line flags, `values.yaml`, CR
    spec/status, ConfigMaps, images or git.
    *Enforced:* `TestNoPlaintextSecrets`, `TestSecretsReferencedByName`.
    `.gitignore` and `.dockerignore` exclude `.env*`, `secret/`, keys and
    kubeconfigs. *Not yet enforced:* secret scanning (a CI job or GitHub
    push protection), `**/` patterns in `.dockerignore`, and rejecting
    literal secret values in `extraEnv`.

**Crypto and TLS**

20. Every token and key comes from `crypto/rand`. Public edges use TLS
    1.2 or later; the internal listener uses TLS 1.3. Internal mTLS uses
    a dedicated CA. No workload ever mounts a CA private key: CA volumes
    project `ca.crt` only, via `items:`.
    The api refuses to start unless PostgreSQL TLS verifies the server
    (`verify-ca`/`verify-full`, from the DSN or `PGSSLMODE`), unless a
    dev-only flag is set.
    *Enforced:* TLS minimums in code; `TestCAVolumesProjectOnlyCACrt`
    (every CA volume, including the DB CA), `TestDatabaseTLSWiring`,
    `TestHardeningDatabaseTLSGate`, `TestCheckDatabaseTLS`,
    `TestCheckDatabaseTLSMalformedDSN`, `TestPGConnHonoursSSEnv`.
    *Not yet enforced:*
    - a CA lifetime longer than its leaves;
    - certificate hot-reload (pods load certificates once at startup, so
      restart them after every rotation, including edge certificates);
    - a schema check that a CA key name is never a private key.

**Kubernetes**

21. RBAC is namespaced Roles and RoleBindings, only in the release and
    managed namespaces. There is no ClusterRoleBinding, wildcard,
    `escalate`/`bind`/`impersonate` or `pods/exec`. Each component gets
    only the verbs it uses; for example, the operator gets no Secret or
    Pod access in the release namespace, and the gateway gets no
    Kubernetes access at all.
    *Enforced:* `TestNoClusterRoleBindings`,
    `TestRoleBindingsOnlyInAllowedNamespaces`,
    `TestOperatorClusterRoleBoundPerManagedNamespace` (no release-namespace
    binding), `TestOperatorWatchNamespacesExcludesRelease`,
    `TestLeaderElectionRoleLeasesOnly`, `TestGatewayServiceAccountLockedDown`,
    `TestKustomizeRBACNoClusterRoleBinding`. A generic ban on wildcard /
    `escalate` / `bind` / `impersonate` / `pods/exec` verbs:
    *not yet enforced* (holds today by inspection).
22. Pod security:
    - `runAsNonRoot`, `allowPrivilegeEscalation: false`, drop `ALL`;
    - seccomp `RuntimeDefault` or a validated `Localhost` profile, never
      `Unconfined`;
    - `readOnlyRootFilesystem` on platform pods;
    - `automountServiceAccountToken: false` unless the pod calls the
      apiserver;
    - CPU, memory and ephemeral-storage limits.

    The only privileged object is the opt-in node-profile installer,
    which runs in its own namespace.
    *Enforced:* `TestEveryContainerHardened` (Deployments),
    `TestRuntimeServiceAccountAutomountFalse`,
    `TestPodAutomountServiceAccountTokenFalse`, `TestPodEphemeralStorageBounds`,
    `TestNodeProfilesOffByDefault`, `TestNodeProfilesDaemonSet` (privileged
    initContainer only; no RBAC in the installer namespace),
    `TestNodeProfilesInstallerNamespaceGuards`, `TestSchemaHardening`,
    `TestHardeningInstallerNamespaceGuard` (rejects the release, managed,
    `default` and `kube-*` namespaces), `TestHardeningManagedEnforcePrivilegedGated`,
    `TestHardeningDevGateBypasses` (extraArgs, capability lists, Unconfined,
    seLinuxOptions, root groups, hostPath volumes: all need
    `dev.enabled`), `TestHardeningVerifierLeastPrivilege`,
    `TestHardeningVerifierRealSysfsProof`.
    *Not yet enforced:*
    - seccomp and read-only-root assertions on every platform pod;
    - `readOnlyRootFilesystem` and `hostUsers: false` on runtime pods;
    - `extraEnv` equivalents of gated flags (`TCDI_DEV_*`, `PG*`,
      `POD_NAMESPACE`);
    - `localhost/<name>` profile names, which are checked only by
      pattern (allowlist `tinycdi-browser`);
    - the installer verifier still shares `hostPID`.
    Keep `extraArgs`, `extraEnv` and `extraVolumes` empty.
23. NetworkPolicy:
    - default-deny ingress and egress in the release and managed
      namespaces;
    - runtime ingress only from gateway pods, on TCP 8443;
    - runtime egress = DNS + the template's profile;
    - `InternetOnly` always excludes RFC 1918, 100.64.0.0/10, loopback,
      link-local/metadata, multicast/reserved and the cluster CIDRs;
    - generated policies are reconciled when they drift;
    - an empty peer list fails the render and never means "any".

    *Enforced:* `TestDefaultDenyNetworkPolicyEverywhere`,
    `TestGatewayEgressToRuntimeNamespaces`, `TestAPIBrokerReachableFromOperator`,
    `TestInternetOnlyBuiltinExceptsAlwaysApplied`,
    `TestInternetOnlyExceptsMergeConfigured`, `TestNetPolReconcilesSpecDrift`,
    `TestNetPolReconcileStillRefusesForeign`,
    `TestIngressPeerScopedToGatewayNamespace`,
    `TestIngressPeerDefaultsToWorkspaceNamespace`, `TestParseExceptCIDRs`,
    `TestNetworkPolicyEmptyPeersFailClosed`, `TestMetricsEmptyPeersFailClosed`,
    `TestMetricsNotOnPublicGatewayService`, `TestInternetOnlyRequiresClusterCIDRs`,
    `TestOperatorPodNamespaceEnv`, `TestHardeningDevGateBypasses`
    (`--disable-builtin-egress-excepts` needs `dev.enabled`). Rejecting
    flags that override chart-set ones (a later `--internet-except-cidrs`
    or `--gateway-namespace`), and guarding `managedNamespaces` against
    system namespaces: *not yet enforced*. Keep `extraArgs` empty.

**Images and supply chain**

24. Every `uses:` is pinned to a full 40-character commit SHA.
    `permissions:` are least-privilege per workflow and per job.
    Untrusted input reaches the shell only through `env:`. Untrusted code
    never runs under `pull_request_target` or `workflow_run`.
    *Enforced:*
    - the `ci.yml` job "workflow lint (actionlint + yamllint + zizmor)"
      (hash-pinned tools; zizmor fails on tag-pinned actions, template
      injection, dangerous triggers and excessive permissions);
    - "workflow policy tests (.github)" (`.github/tests/*.test.sh`,
      including the release-version validation);
    - Dependabot for actions.

    These become merge gates only once `.github/scripts/setup-repo-protection.sh`
    has applied branch protection.
25. Every `FROM` is pinned by `@sha256`, and every download is
    sha256-verified. Final images run as a numeric non-root UID with
    setuid/setgid bits removed.
    *Enforced (partly):* sha256-pinned downloads of KasmVNC, helm, syft,
    trivy, cosign, actionlint and the PyPI wheels; the BuildKit image is
    digest-pinned (checked by `release-topology.test.sh`); the Makefile
    fails closed on unpinned checksums. All `FROM` lines are pinned,
    images run as numeric UIDs and setuid bits are stripped, but no CI
    test asserts any of that: *not yet enforced*.
26. Publishing happens only after every gate passes, in this order:
    1. build and push by digest only (no tags);
    2. vulnerability scan, in a job with no publish credentials, no
       id-token and an explicit `--config`/`--ignorefile`;
    3. sign and attest the scanned digests (cosign signature, SBOM,
       provenance);
    4. only then promote the tags.

    The publish job consumes only allowlisted artifacts, one expected
    artifact per name, never merged. Releases run only from a protected
    environment, and the released chart carries image digests.
    `.trivyignore` exceptions carry a reason, an approver and an expiry of
    at most 30 days, and an expired exception fails the **scan gate**.
    *Enforced:* digest-only push, scan before promotion, scan jobs without
    secrets or id-token, sign/attest before promotion, chart digest
    resolved from the registry and signed, a digest-stamped chart, the
    allowlist step, and exception expiry inside the scan jobs.
    Publish inputs are downloaded by exact artifact name and validated
    by `.github/scripts/collect-publish-inputs.sh`: exact folder set, the
    expected file per folder, strict refs, SBOM JSON, and chart digests
    equal to the refs. The `images.yml` promote job validates refs with
    `validate-image-refs.sh`. When a GPL runtime image is released, the
    KasmVNC corresponding-source bundle is required among the release
    assets. Checked by `.github/tests/release-topology.test.sh`,
    `.github/tests/check-trivyignore.test.sh` and
    `.github/tests/publish-inputs.test.sh`. Dispatches are rehearsals
    only; a real publish needs a `v*` tag.
    *Repository settings* (applied by
    `.github/scripts/setup-repo-protection.sh --apply` right after the
    repository goes public and **before** any tag or dispatch; its plan is
    checked by `.github/tests/setup-repo-protection.test.sh`):
    - the protected `release` environment with a `v*` tag policy;
    - the `v*` tag ruleset (bypass: repository admins only);
    - branch protection on `main`.
27. `govulncheck`, `npm audit --omit=dev`, dependency review and secret
    scanning gate CI. Thresholds follow the
    [vulnerability policy](docs/security/vulnerability-policy.md).
    *Enforced:* "govulncheck (Go vuln scan)" (v1.8.0, called
    vulnerabilities fail the job), `npm audit --omit=dev --audit-level=high`
    in "portal ui", a weekly scheduled rerun, Dependabot, and
    "chromium apt-pin freshness".
    *Repository settings* (applied by `setup-repo-protection.sh --apply`):
    - dependency review (`CODE_SCANNING_ENABLED`);
    - secret scanning and push protection;
    - private vulnerability reporting;
    - required checks via branch protection.

    They are not in force until the script has run.

### B. Operators: deployment checklist

1. **OIDC (Keycloak):**
   - confidential client with PKCE S256 and the exact redirect URI
     `https://<portalHost>/v1/auth/callback`;
   - `tenant_id` comes from an admin-controlled mapper (a hard-coded
     claim or a group mapping), **never** from a user-editable attribute;
   - the `groups` claim is emitted without full paths, so `tenant-admin`
     matches;
   - **gate who may log in** with `oidc.requiredGroups` (e.g.
     `[platform-admins]`). Every account that passes gets a desktop with
     Internet egress inside your network. The match is verbatim, so emit
     the `groups` claim **without** full paths (`full.path: false`). The
     gate applies at login: removing someone from the group takes effect
     when their session ends (up to the absolute timeout);
   - the client secret is supplied via `oidc.existingSecret`.
2. **Edges:**
   - portal and session hosts are on **different registrable domains**
     (the chart only checks that the hostnames differ);
   - TLS on both edges (the chart enforces Ingress TLS and an HTTPRoute
     `sectionName`; point it at the HTTPS listener);
   - HSTS: the portal and gateway already send it;
   - rate limits on `/v1/login` and `/v1/launch`;
   - if a CDN/WAF proxies the portal, it terminates TLS and sees session
     cookies and launch tickets. Require strict origin TLS, never cache
     `/v1/*`, and make the origin reachable only through the CDN.
3. **Database:**
   - `database.tls.mode: verify-full` (the default) with
     `database.tls.caSecret` for a private CA (the api gets
     `PGSSLMODE`/`PGSSLROOTCERT`). A DSN `sslmode=` overrides
     `database.tls.mode`; the api refuses to start with a non-verifying
     mode, so keep `sslmode=` out of the DSN, or set it to `verify-full`;
   - a dedicated role;
   - `database.allowedPeers` set explicitly: the default is a deny-all
     placeholder, and an empty list fails the render;
   - the DB is unreachable from tenant namespaces;
   - backups are encrypted;
   - after a restore, rotate the session epoch
     ([backup-restore](docs/runbooks/backup-restore.md)).
4. **Internal mTLS:**
   - a dedicated CA, never a cluster-wide shared issuer: every cert the
     CA signs is a valid gateway or operator identity;
   - the CA Secrets you reference contain only `ca.crt`;
   - `api.operatorCN` is unique;
   - certificates rotate through cert-manager `duration`/`renewBefore`.
     Give the CA a longer lifetime than its leaves. Pods read
     certificates only at startup, so restart api, gateway, operator and
     portal after a renewal (or use a reloader).
5. **Egress:**
   - the operator always excludes RFC 1918, `100.64.0.0/10`, loopback,
     link-local/metadata and multicast/reserved ranges from `InternetOnly`;
   - set `operator.clusterCIDRs` to your pod, service and node CIDRs. It
     is required when you seed `InternetOnly` templates, and it matters
     for any range outside RFC 1918;
   - keep `extraArgs`/`extraEnv` empty on every component: the chart
     rejects known weakening flags unless `dev.enabled`, but cannot catch
     every override;
   - prefer the `Isolated` profile where the use case allows it;
   - an FQDN allowlist needs an egress proxy.
6. **Namespaces:**
   - release namespace PSS `enforce=baseline` (`warn=restricted`);
     managed namespaces `restricted`;
   - never label the release namespace `privileged`;
   - `networkPolicy.enabled=true` with explicit `apiServerPeers`,
     `dnsPeers` and monitoring peers.
7. **Node profiles:**
   - install the seccomp and AppArmor profiles out-of-band
     ([node profiles](deploy/node-profiles/README.md)), or with the
     installer **in a namespace of its own** (`nodeProfiles.install.namespace`;
     the chart rejects the release, managed, `kube-*` and `default`
     namespaces). The installer runs a privileged pod on every matched
     node; scope it with `nodeProfiles.install.nodeSelector` to the
     nodes that run browser workspaces;
   - never run browser templates `Unconfined`;
   - run all workspace templates on dedicated, promptly patched nodes
     that host no platform or cluster-infrastructure workloads
     (cert-manager, operators, storage, CI runners). A container escape
     from a tenant shell must not land next to cluster-wide credentials.
8. **Images and upgrades:**
   - pin `images.*.digest` in your GitOps values;
   - verify signatures against the **exact** release identity, never a
     regex that also matches branches:
     `cosign verify --certificate-oidc-issuer https://token.actions.githubusercontent.com --certificate-identity https://github.com/tinyorbitvn/tinycdi/.github/workflows/release.yml@refs/tags/vX.Y.Z <image>@sha256:<digest>`.
     Provenance, when published, can also be checked with
     `gh attestation verify oci://<image>@sha256:<digest> --owner tinyorbitvn --signer-workflow tinyorbitvn/tinycdi/.github/workflows/release.yml --source-ref refs/tags/vX.Y.Z`;
   - follow [upgrade](docs/runbooks/upgrade.md) and diff CRDs before
     applying them.
9. **Secret rotation:**
   - rotate the OIDC client secret, the DB credentials and the internal
     certs on a schedule;
   - leave `gateway.controlToken` unset unless you need it;
   - to revoke all sessions, rotate the session epoch;
   - runtime credentials are per-workspace and regenerated when the
     workspace is recreated.
10. **Observability:**
    - keep metrics off public Services;
    - restrict access to API and gateway logs, which contain OIDC
      subjects and lease IDs;
    - alert on spikes of denied `launch.redeem` events.
11. **Clipboard and other in-desktop restrictions are UX defaults, not
    DLP.** The workspace user controls their own runtime.
