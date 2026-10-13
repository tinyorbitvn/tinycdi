# TinyCDI v1.0.0 public release: full-breaking rework

Date: 2026-10-13. Status: design approved in conversation, pending written-spec
review. Owner: advisor. Execution method: chosen at the plan stage.

## 1. Intent

TinyCDI has run only as an internal test on the RKE2 dev cluster. There are no
external users. The product owner wants the public face of the project to be a
clean **v1.0.0 stable** release. It gathers every open roadmap item into one
release and drops all compatibility with earlier internal builds.

What the owner decided:

- One spec covers everything left on the roadmap (section 3).
- Breaking changes are unrestricted. Nothing is upgraded in place: no upgrade
  path, no CRD conversion, no API versioning shim. The dev install is pruned
  and reinstalled from scratch, and the dev database may be wiped (a pg_dump is
  kept as evidence).
- The release ships from the same repository, `tinyorbitvn/tinycdi`. Before the
  first v1.0.0 release candidate, the full history is archived privately. Main
  is then force-pushed as one root commit, and every old tag, release and GHCR
  version is deleted.
- The HTTP API keeps the `/v1` prefix and is redesigned in place.
- A full security review of the whole product (the depth of the earlier v1.0
  review) gates GA.
- The v1.2.0 GA is abandoned. Its tag was deleted on 2026-10-13; the release
  run had failed on a withdrawn Chromium pin and produced no artefacts. Dev
  keeps running v1.2.0-rc.2 until v1.0.0-rc.1 replaces it.
- After the reset, the docs describe only v1.0.0.

Assumptions (confirmed by section approval):

- The in-repo portal (`web/`) is the only API client.
- Workers are Devin agents run through Orca. Each PR gets its own reviewer, and
  the advisor merges with merge commits, keeping main green between merges.

Out of scope:

- An external independent security review (the owner arranges it separately).
- The governance flip (required approvals, `prevent_self_review`), which waits
  for a second maintainer.
- Multi-architecture images, new runtimes, and any IdP change.

## 2. Success criteria (release gates)

1. CI is green on main, and the image baseline tests pass, including the size
   limits.
2. The fresh-install e2e passes on kind and again on the dev cluster (flow in
   §7.5).
3. The full security review closes with 0 open P0/P1. Each P2 is either fixed
   or accepted with an owner and a reason.
4. The browser checklist passes on dev.
5. The public repo history starts at the new root commit. The only tags,
   releases and GHCR versions left are the v1.0.0 set (rc and final) plus the
   new runtime train.
6. The docs describe only v1.0.0. The docs gate in §7.7 passes.

## 3. Scope inventory

| ID | Item | Source |
|---|---|---|
| W0.1 | Squash migrations into one baseline schema | new (no upgrade path) |
| W0.2 | CRD `workspaces.cdi.tinyorbit.vn/v1`, `v1alpha1` removed | roadmap "later 1.x" |
| W0.3 | `/v1` API redesign (paging, ETag, error envelope, `:action`) | client-side paging limit; If-Match quirk |
| W0.4 | Portal on the new API | follows W0.3 |
| W0.5 | Chart 1.0.0: deprecated values/env removed; `ClusterOnly` restricted by default | SR-2-F8 deferral |
| W0.6 | Chromium 155 pin + kasm catalog on `kasmweb/chrome` | failed v1.2.0 release build |
| F1 | Start recovers a `Failed` workspace whose pod was deleted | v1.2.0 known limitation |
| F2 | First cold connect stall | v1.2.0 known limitation |
| F3 | `session-policy:apply` restarts affected running workspaces | v1.2.0 known limitation |
| F4 | File transfer + admin toggle (ADR 0010 T11) | v1.3 plan |
| F5 | Admin session visibility + sign a user out (T7) | UI proposal P3 |
| F6 | Audit log viewer (T8) | UI proposal P3 |
| F7 | Overview capacity/health (T9) | UI proposal P3 |
| F8 | Tenant user directory | v1.2.0 known limitation |
| C1 | Remove compatibility shims | follows W0 |
| C2 | Docs rewritten for v1.0.0 only; orphan/init docs removed | owner |
| C3 | ADRs rewritten as current state | owner |
| C4 | Threat model + test inventory renumbered for the new architecture | input to R1 |
| R1–R6 | Review, archive, reset, runtime train, rc, GA | release |

## 4. W0: foundation

W0.1 → W0.2 → W0.3 → W0.4 run in order. W0.5 and W0.6 run in parallel with
W0.2–W0.4.

### W0.1 Baseline schema

- Replace `internal/store/migrations/001…024` (23 SQL files, schema 24) with
  `0001_baseline.sql`.
  - Generate it from `pg_dump --schema-only` of a database migrated through
    024, then hand-clean the result.
  - Columns that allow NULL only for legacy rows become `NOT NULL`: the
    session-digest and claim-id columns read by `internal/broker/{tickets,
    sessions,activity,connection_state}.go`.
  - Drop columns and tables that only served compatibility.
- The baseline also creates the tables W1 needs, so W1 adds no migrations:
  - `audit_event`: `id`, `tenant`, `actor_ref` (pseudonymized as in
    `observability.md`), `action`, `target_kind`, `target_id`, `outcome`,
    `details jsonb`, `created_at`. Indexed on `(tenant, created_at desc)`.
  - `session_file_transfer`: the columns from ADR 0010 T11.
- The migrator stays for migrations that come after 1.0. Its version table
  restarts at 1.
- Tests:
  - The schema after `0001` matches a committed snapshot.
  - The migrator applies on an empty database and is idempotent.
- The migration-specific e2e steps (restored-dump migrate, rollback drill) are
  removed. There is nothing to migrate from.

### W0.2 CRD `v1`

- `api/v1alpha1` → `api/v1` (group `workspaces.cdi.tinyorbit.vn`) for `Workspace` and `WorkspaceTemplate`. The
  operator serves and stores `v1` only.
- Removed from the schema and code:
  - the chart's leftover legacy `templates[].nodeSelector` rejection (the
    annotation path itself is already gone from code);
  - the session-policy snapshot fallback (`buildPod` reads `spec.sessionPolicy`
    only; the backend always writes it);
  - the legacy catalog-name label fallback in `snapshotStale` and
    `resolveTemplate` (`internal/operator/workspace_controller.go`).
- `podSpecBuiltEqual`'s `DeprecatedServiceAccount` normalization stays. It
  normalizes a Kubernetes core field and is not a TinyCDI legacy path.
- `spec.sessionPolicy` carries a live `fileTransfer` channel.
  `WorkspaceTemplate` gains `spec.fileTransferPolicy` (default `Disabled`).
- Regenerate the deepcopy code and `config/crd/bases`. Update the RBAC markers.
- Tests: envtest covers create, validation, defaulting and status for both
  kinds.

### W0.3 API `/v1` redesign

Write the contract first: replace `internal/api/openapi.yaml`, then the
handlers. A new ADR (§6, C3) records these conventions:

- **Lists.** Every list operation accepts `limit` (default 50, max 500),
  `cursor` (opaque) and `sort` (an allowlisted field with `-` for descending).
  It returns `{items, nextCursor}`. Cursor stability means no duplicates and
  no gaps under concurrent inserts, ordered by `(sortKey, id)`. Applies to
  workspaces, templates, data, the admin user-limits, users, sessions and
  audit.
- **Concurrency.** Mutable admin resources return a strong quoted `ETag`.
  PUT/PATCH/DELETE on them require `If-Match`:
  - missing → 428 `precondition_required`;
  - stale → 412 `precondition_failed`;
  - `If-Match: *` is accepted only when the resource does not exist.
  This replaces the unquoted opaque `version` token. Quota, user-limits
  (which today has no precondition) and session-policy all adopt it.
- **Errors.** One envelope, `{"error": {"code", "message", "details"}}`. `code`
  is a closed enum shared with the portal.
- **Actions.** Custom methods use `:verb` paths. `net/http.ServeMux` cannot
  match `{id}:verb` inside one segment, so a small dispatcher in `internal/api`
  registers `POST /v1/workspaces/{ref}` and splits `ref` on the last `:`.
  Paths: `POST
  /v1/workspaces/{id}:start`, `:stop`, `/v1/data/{id}:attach`, `:purge`,
  `/v1/admin/tenants/{t}/session-policy:apply`.
- **Unchanged contracts:** cookies (`__Host-` only), CSRF (`X-CSRF-Token`),
  PKCE login, rate limits and tenant isolation. These are security behaviour,
  not API style.

Routes: today's 22 paths are kept in meaning but renamed to the conventions
above. W1 adds the new paths in §5. There is no compatibility alias.

Tests:

- Contract tests are generated from the OpenAPI document for every operation.
- Paging property tests.
- ETag/If-Match matrix.
- Error-envelope conformance.
- A negative authorization test for every admin route (non-admin, other
  tenant, missing CSRF).

### W0.4 Portal on the new API

- Regenerate `web/src/api/generated/schema.d.ts`.
- Rework `web/src/api/client.ts` and `errors.ts` for the error envelope and
  ETag handling.
- Tables switch from client-side 4000-row paging to server paging and sorting.
  The visuals are unchanged.
- Playwright smoke on every page.

### W0.5 Chart and config 1.0.0

- Chart version `1.0.0`.
- Remove every deprecated value. The chart README gets a "removed values" table
  that the public release notes copy.
- `runtime.networkProfiles.clusterOnly` defaults to restricted egress. With no
  configured selectors, the operator confines ClusterOnly egress to namespaces
  labelled `workspaces.cdi.tinyorbit.vn/tenant` (DNS is unaffected; external
  IPs stay denied). The chart value
  `runtime.networkProfiles.clusterOnly.unrestricted: true` opts back into
  any-pod egress. Replace `TestClusterOnlyEgressDeprecationNotice` with a test
  that pins the restricted default.
- Remove the CI `upgrade` job together with `hack/quickstart/upgrade-test.sh`
  and `smoke/upgrade.spec.ts`. There is no previous release to upgrade from.
- Remove legacy env names from `internal/backend/config.go`
  (`TCDI_GW_CONTROL_TOKEN`, `TCDI_LOGIN_KEY_FILES`) and their chart rendering.
- Tests: the chart Go tests, helm lint, and the render-guard tests.

### W0.6 Chromium pin and kasm catalog

- Build `build/browser` with `CHROMIUM_APT_VERSION=155.0.8059.39-1~deb12u1`.
  Already proven: it builds, the packages install at 155, and trivy
  HIGH/CRITICAL with ignore-unfixed reports 0. Reuse local branch
  `fix/chromium-155-pin`, commit `6ec1564`.
- `kasmweb/chromium` rolling-weekly still bundles Chromium 150, below the
  desktop floor of 151 (pin − 4). Replace it with `kasmweb/chrome`
  1.19.0-rolling-weekly (`sha256:4999d22efdde2ae7e432c6eb969685e578498c83491e6ef8a0853adc1324003b`,
  Chrome 154, trivy 0), class `browser`, floor 154.
- The adapter and `TestKasmAdapterChromium` assume the `/usr/bin/chromium-orig`
  wrapper layout, and fail 7 of 8 subtests on the chrome image. Make the
  adapter detect the browser binary and wrapper per image, then run the
  contract suite against every catalog entry. No gate is weakened.
- `TestLinuxRuntimeBrowserBaseline` has about 380 KB of headroom. Re-baseline
  the size limit to the measured size plus a stated margin, with the reason
  recorded next to the limit.

## 5. W1: features

These run in parallel after W0, one PR each. Every item includes tests, en+vi
strings, Playwright coverage for its UI, and threat-model rows for any new
write surface.

### F1 Recover a `Failed` workspace

- **Problem.** When a workspace's runtime pod is deleted, the workspace stays
  `Failed` for good. The `runtimeGeneration` fence never rebuilds a pod that
  was removed underneath it.
- **Design.**
  - `:start` on a `Failed` workspace with no pod, and with a recoverable
    reason, bumps `runtimeGeneration`. That gives a new `runtimeUID`, and the
    pod is rebuilt on the same home PVC.
  - Leases and tickets bound to the old generation are revoked and rejected.
  - Unrecoverable reasons stay `Failed` with an explicit
    `reason`/`lifecycle-reasons.md` entry: image pull denied by policy, PVC
    lost, validation failure.
- **Tests.** Envtest plus the e2e step "delete pod → start → home file present
  → old lease rejected".

### F2 Cold first connect

- **Spike first.** On kind, measure the timeline from Ready to ticket mint,
  iframe load and redeem against a just-Ready pod, and find the cause.
  Candidates:
  - Ready is reported before the session host listens;
  - the ticket expires while the iframe waits;
  - the gateway's upstream dial gives up too early.
- **Expected fix.**
  - Ready requires a successful probe of the session host's stream port.
  - The portal retries redeem with backoff inside the ticket's lifetime.
- **Gate.** Cold-connect p95 under 20 s across 20 e2e runs.

### F3 Apply a session policy now

- `POST /v1/admin/tenants/{t}/session-policy:apply`, in two phases:
  1. `dryRun=true` lists the running workspaces whose effective policy is
     looser than the stored ceiling.
  2. The confirmed call restarts them.
- Workspaces with a live session are restarted only when `includeActive=true`.
- Writes an audit event. Uses CSRF, If-Match on the policy ETag, and the
  tenant-admin role.
- UI: an "Apply now" flow on the session-policy page that shows the affected
  list.

### F4 File transfer (ADR 0010 T11)

Built as the ADR describes:

- The `fileTransferPolicy` template field is met against the tenant ceiling by
  the single resolver.
- **API.**
  - Mint with `POST /v1/workspaces/{id}/file-transfers` (direction, path,
    declared bytes).
  - Rows in `session_file_transfer` are single-use: `pending → claimed` is one
    conditional update. They expire, and the policy is checked again at claim.
- **`tcdi-xferd`** in `build/linux-base` and in the kasm-adapter contract:
  - owns `:8443` and proxies the desktop to loopback;
  - requires the per-workspace transfer credential;
  - is jailed to the transfer root;
  - hard-caps at `TCDI_XFER_MAX_BYTES`.
- **Gateway.**
  - `/xfer/` joins the upstream allowlist through `cleanedProxyPath`.
  - The claim-and-proxy leg runs the lease-binding check.
  - Bytes count against the cap and the per-workspace stream cap.
- **UI.** The session page gets upload and download, hidden when the resolved
  policy is `Disabled`. The session-policy page controls go live.
- **Tests.** ADR 0010 "Tests" list, plus the e2e round trip upload → download.

### F5 Admin sessions and sign-out (T7)

- `GET /v1/admin/tenants/{t}/sessions` lists live leases from the Postgres
  session directory. It is not replica-local.
- `POST /v1/admin/tenants/{t}/users/{ownerRef}/sessions:revoke-all` is the
  admin variant of the self route. It writes the audit event
  `admin.session.revoke`.
- UI: a sessions table on the user detail page with a "Sign out everywhere"
  action that asks for confirmation.

### F6 Audit viewer (T8)

- Writing audit events:
  - One function writes the existing stdout audit record and inserts into
    `audit_event`, in the same transaction as the audited mutation where one
    exists.
  - A failed insert fails the mutation closed.
- Retention is 90 days by default (chart value). A daily prune runs as a
  backend leader task.
- `GET /v1/admin/tenants/{t}/audit` filters by action, actor, target and time
  range, and pages by cursor. UI at `/admin/audit`.
- A new ADR records the store, retention and pseudonymization (§6, C3).

### F7 Overview (T9)

- `GET /v1/admin/tenants/{t}/overview` returns:
  - the live session count;
  - running workspaces per template;
  - provisioning p95 over 24 h;
  - the count of stuck finalizers.
- It is one aggregate query plus the operator's status counts, and it is
  cached for 15 s.
- UI: cards on the admin Overview page.

### F8 Tenant user directory

- `GET /v1/admin/tenants/{t}/users` reads from the principal table: everyone
  who has signed in to the tenant, with last-seen, workspace count and limit
  override.
- The admin Users page switches to it, and the "not a tenant directory"
  limitation goes away.

## 6. W2: cleanup and docs

These run in parallel after W1.

### C1 Compatibility shims

- Remove:
  - the legacy NULL digest/claim branches (broker, gateway);
  - the principal-wide touch path;
  - the legacy env names, catalog labels and snapshot fallback, if any are
    left after W0;
  - comments that point at internal versions ("until v1.3", "pre-v1").
- Gate: a `.github/tests` script that greps `legacy|deprecated|v1alpha1` in Go,
  TS and chart sources against a reviewed allowlist. Each allowlist entry must
  carry a reason. Upstream quirks such as KasmVNC's `Sec-WebSocket-Origin` are
  allowed.

### C2 Docs for v1.0.0 only

- **Delete:**
  - `docs/releases/v0.3.0…v1.2.0.md` (9 files);
  - `docs/runbooks/upgrade.md`;
  - the old `docs/security/review-v1.0.md`, which R1 replaces;
  - this spec and any plan under `docs/superpowers/`. Before R3 their content
    moves into ADRs and the release notes.
- **Add:**
  - `docs/releases/v1.0.0.md`: features, install, known limitations;
  - `docs/runbooks/audit.md`;
  - the file-transfer section in `docs/runbooks/session-policy.md`.
- **Rewrite without internal version history:**
  - `docs/versioning.md`: the 1.x contract covers `/v1`, chart values and CRD
    `v1`; internal mTLS and metrics stay outside it;
  - `compatibility.md`, `images.md` (with the `/xfer/` + `tcdi-xferd` runtime
    contract), `kasm-images.md`, `quickstart.md`, `runbooks/install.md`;
  - the chart README, `.github/README.md`, `docs/README.md`;
  - `SECURITY.md`: the supported version is 1.x.
- **Orphans:** `CONTRIBUTING.md` has no inbound link. Link it from `README.md`;
  do not delete it.
- **Gates:**
  - A docs link checker.
  - A grep gate that rejects `v0\.[0-9]`, `v1\.[12]`, `rc\.[0-9]` and PR
    references (`#[0-9]{2,3}`) in `docs/`, the root `*.md` and the chart
    README, with an allowlist for real external version strings.

### C3 ADRs as current state

- Rewrite 0001–0010 to describe the current design:
  - each status line reads `Accepted`;
  - release and PR history is removed.
- Restructure:
  - 0002 (public API) is replaced by the new ADR "API conventions" (W0.3).
  - The content of 0008 (KasmVNC brute-force analysis) moves into the threat
    model, and the file is deleted.
  - New ADR "Audit store and retention" (F6).
- Renumber so the set has no gaps.

### C4 Threat model and test inventory

- Rebuild `docs/security/threat-model.md` and `test-inventory.md` for the
  v1.0.0 architecture.
- Drop the rows that only concerned removed shims and the "fixed in vX" prose.
- Add rows for:
  - the `tcdi-xferd` boundary and the `/xfer/` proxy leg;
  - admin revoke, audit read, `:apply` and overview.
- This is R1's input.

## 7. Testing strategy

1. **Store.** Baseline schema snapshot; `NOT NULL` constraints; the
   `session_file_transfer` state machine; audit retention prune.
2. **API.** OpenAPI-generated contract tests; paging properties;
   ETag/If-Match; the error envelope; CSRF and tenant-admin negatives on every
   admin route; rate-limit behaviour unchanged.
3. **Operator.** Envtest for CRD `v1`; F1 recovery; Ready only after the
   session-host probe.
4. **Runtime.**
   - `tcdi-xferd` jail, credential and cap;
   - the entrypoint render contract (shellcheck-clean);
   - the kasm adapter contract on every catalog entry;
   - image baseline sizes with reasons recorded.
5. **E2E on kind, fresh install.** Login → create → connect (record the cold
   p95) → clipboard round trip → upload/download → stop/start → delete pod →
   recover → retained data and attach → admin (sessions, revoke, audit,
   overview, users, `:apply`). Teardown is proven: namespaces gone, 0 PVs.
6. **UI.** Playwright on every page; en/vi string parity; no console or CSP
   errors.
7. **Docs.** The link check, the version-string grep gate and the shim grep
   gate.

## 8. W3: release

1. **R1: full security review.**
   - A dedicated Opus xhigh security reviewer audits the whole product against
     the C4 threat model.
   - Each finding is fixed in its own PR with a security test.
   - Report: `docs/security/review-v1.0.md`.
   - Gate: 0 open P0/P1.
2. **R2: archive.**
   - Push every branch and tag to the private remote `archive`
     (`tinyorbitvn/tinycdi-archive`).
   - Check that the commit and tag counts match. Go on only if they do.
3. **R3: reset the public repo.**
   - Delete every GitHub release and tag (`v0.1.0…v1.2.0-rc.2`, `runtime-*`).
   - Squash main into one root commit and force-push it, using the ruleset
     bypass.
   - Delete the remaining branches. Check that `git rev-list --count main` is 1.
   - Caveats: GitHub keeps `refs/pull/*` of closed PRs, and old SHAs stay
     reachable by URL until GitHub garbage-collects them. Purging needs a
     GitHub Support request; the owner decides whether to file one.
4. **R4: runtime train.**
   - Run `runtime-images.yml` on the root commit, which gives a new
     `runtime-YYYY.MM.DD` train.
   - Repin the quickstart, which checks pins against the signed
     `runtime-images.json`.
5. **R5: `v1.0.0-rc.N`.**
   - Tag it. The release workflow signs and publishes, and a watcher approves
     the `release` environment gate.
   - Run the fresh-install e2e on kind (§7.5).
   - Then reinstall dev:
     1. pg_dump the dev database as evidence;
     2. remove the Argo app `tinycdi`, its namespaces, the `v1alpha1` CRDs, the
        workspace PVCs and the CNPG `tinycdi-db` cluster;
     3. commit GitOps with chart `1.0.0-rc.N` and values limited to keys that
        still exist;
     4. let Argo install from scratch.
   - Verify: Synced/Healthy, the anonymous smoke round, the dev e2e, and the
     browser checklist (the owner provides a login or test accounts).
   - Fixes land as normal PRs on top of the root commit, followed by the next
     rc.
6. **R6: `v1.0.0`.**
   - Tag the passing rc commit, then push the GitOps final bump and verify it
     live.
   - **GHCR cleanup:** delete every package version outside the v1.0.0 digest
     set and the new train. This waits until after the dev reinstall, because
     dev runs rc.2 digests until then.
   - Write the release body. Clean up worktrees and agents.

R3 deliberately comes before R5. The new `v1.0.0-rc.1` and `v1.0.0` tags must
not collide with the deleted tags of the same name, and the cosign
certificate identities have to bind to refs in the reset repo.

## 9. Execution

| Wave | Items | Concurrency |
|---|---|---|
| W0 | 0.1 → 0.2 → 0.3 → 0.4; 0.5 and 0.6 alongside | up to 3 |
| W1 | F1–F8 (F2 starts with its spike) | up to 8 |
| W2 | C1–C4 | up to 4 |
| W3 | R1 → R6 | sequential, with fix PRs in parallel inside R1 and R5 |

- Concurrency stays at most 10 Devin workers.
- Every PR has a reviewer gate. The advisor merges.

## 10. Risks

| Risk | Mitigation |
|---|---|
| The W0.3 redesign touches every handler and page, so W1 starts late | Contract first: W1 workers code against the merged OpenAPI while W0.4 lands |
| Kasm upstream lags Debian Chromium again | The catalog floor rule stays; the adapter supports both layouts; a catalog may hold only `kasmweb/chrome` |
| The force-push loses something needed | R2 archive verified before R3; PR refs and the archive remote keep a full copy |
| Dev reinstall deletes data | Internal test only; pg_dump kept; the owner has authorized it |
| The full review finds many issues late | C4 threat model lands early in W2; the reviewer can start on W0/W1 code while W2 runs |
| Image size creeps past the baseline | W0.6 re-baselines with a margin; the trivy and size gates stay |
