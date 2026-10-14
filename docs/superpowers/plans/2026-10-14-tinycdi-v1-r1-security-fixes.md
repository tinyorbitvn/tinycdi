# TinyCDI v1.0.0 — R1 Security Review Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use sdd-orca:subagent-driven-development (recommended) or sdd-orca:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close every finding of the v1.0.0 security review (`docs/security/review-v1.0.md`, R1-01 to R1-41: 9 P2, 32 P3). Fix each one in code where the repo can fix it, and pin it with a named security test that fails before the fix. Record the few residuals the repo cannot remove as owner-accepted. Finish by bringing the review report, the threat model and the test inventory up to the post-fix state (spec §2 gate 3, §8 R1).

**Architecture:** 30 tasks run in order on one branch, one commit each, in one PR. Tasks 1–5 cover quota and audit (provisioning, admin API). Tasks 6–11 cover the app listener: auth, headers, contract and input handling. Tasks 12–16 cover the session listener: broker tickets, the gateway proxy and the backend listeners. Tasks 17–21 cover the workspace pod: KasmVNC, the PID bound, NetworkPolicy, profiles and the CRD. Tasks 22–25 cover the internal mTLS listener and the chart guards. Tasks 26–29 cover the supply chain: train, release and CI gates. Task 30 updates the report, the threat-model register and the test inventory. Each task carries its finding's attack, required fix and security test, so an implementer needs no other document.

**Tech Stack:** Go 1.26, pgx v5, controller-runtime + envtest, Postgres 18 test container (`postgres:18.0`), Helm v4.1.3, React + TypeScript + vitest, KasmVNC 1.5.0 in `build/linux-base` and the kasm adapter. Gate scripts use bash, GNU grep/awk/sed and python3 (standard library; plus PyYAML, from the runner's system `python3-yaml`, only in Task 28's `publish-gating.test.sh` and `setup-repo-protection.test.sh` block, which print `FAIL` when the import fails). The CI runner is `ubuntu-24.04`, which has python3 but no ripgrep.

**Spec:** `docs/superpowers/specs/2026-10-13-tinycdi-v1-public-design.md`, §2 gate 3 ("0 open P0/P1; each P2 fixed or accepted with an owner and a reason") and §8 R1 ("each finding is fixed in its own PR with a security test"; this plan runs as one PR with one commit per finding or per finding group, per Decision 2).

**Codebase:** branch `v1/r1-security-review`, HEAD `98c7f54` = `origin/main` `351029a` + the committed report. Every `file:line` below was re-checked at `98c7f54`. If a line moved, find it again by symbol.

## Decisions

These are owner decisions (defaults the owner reviews). Each gives the decision, the reason, and the cost if it is wrong.

1. **Fix every P2 and every P3.** A finding is "accepted (owner)" only where a fix is infeasible inside this repo or contradicts an existing owner decision. Exactly one part is accepted: the node-level enforcement in R1-07. The kubelet's `podPidsLimit` lives in node configuration that this repo does not ship, so the product can require and check it but cannot set it. R1-07's in-repo parts are fixed (Task 18), and the residual goes into the release notes' known limitations and the threat model. R1-27 is fixed: the docs are corrected and the desktop leg drops `Content-Disposition`. The in-pod channel itself stays the existing owner-accepted "In-pod trust" limitation. R1-29 is fixed: the operator refuses the userns profile pair unless the pod runs with `hostUsers: false`, and the docs state the trade-off. R1-37 is fixed through the release gate (Task 29). Merges stay non-gated by the kasm scan, per the existing owner decision behind `setup-repo-protection.sh` (network-dependent jobs do not gate merges). CI run 38586060306 on `351029a` shows `kasm adapter contract + catalog scan` green, so gating releases on it does not fail today. — Reason: spec §2 gate 3, and the owner asked for every finding fixed. — Cost if wrong: about 30 commits in one PR; a review round is longer.
2. **One commit per finding where practical.** Tightly coupled or same-file findings share a task: R1-16+22+23 (Task 4), R1-10+11+12 (Task 7), R1-17+21 (Task 10), R1-18+19 (Task 11), R1-28+35 (Task 19), R1-30+31 (Task 22), R1-32+33 (Task 23), R1-15+36 (Task 25), R1-38+39+40 (Task 28), R1-37+41 (Task 29). R1-09 spans two tasks (26, 27). Every finding keeps its own named security test, which fails before the fix. A doc-only finding gets a docs assertion in `.github/tests/security-doc-claims.test.sh` (Global Constraints) and says so. Task numbers are integers 1..30. — Reason: sdd-run maps tasks by ordinal; the brief's suggested grouping. — Cost if wrong: none; regrouping is mechanical.
3. **R1-02: operator-declared per-tenant quota ceiling.** The chart gains `managedNamespaces[].quotaCeiling` (the same four keys as `quota`). It renders into a new backend flag, `-tenant-quota-ceilings`, with the same JSON shape as `-tenant-quotas`. A tenant-admin `PUT /v1/admin/tenants/{t}/quota` above the ceiling is refused, and so is any tenant-admin quota write when no ceiling is declared (fail closed). A config-managed tenant (with a `quota` block) stays read-only, as today. — Reason: the operator must own the upper bound. — Cost if wrong: operators who relied on delegating quota to tenant admins must now declare a ceiling.
4. **R1-07: `podPidsLimit` is an install requirement, checked by preflight. The in-pod warning and the operator condition are dropped.** `docs/runbooks/install.md` requires a kubelet `podPidsLimit` on workspace nodes. `hack/preflight/preflight.sh` reads every workspace node's `podPidsLimit` from the kubelet `configz` endpoint with the admin's credentials, and WARNs when it is unset or `-1`. `hack/quickstart/kind-config.yaml` sets it, so CI runs bounded. The brief's in-pod check (the entrypoint reads `/sys/fs/cgroup/pids.max`) cannot work. Inside a container with a private cgroup namespace, that file shows the container's own cgroup, not the pod-level cgroup where kubelet applies `podPidsLimit`. This was checked on the planning host: under a parent slice with `pids.max=123`, a `--cgroupns=private` container read `pids.max=38381` (systemd's per-scope default). An operator condition would need `nodes/proxy` RBAC, which reaches the kubelet API of every node, so it is not added. — Reason: a warning that is wrong in both directions is worse than none. — Cost if wrong: an install without the kubelet setting gets no in-product signal; preflight is the check.
5. **R1-08: `WorkspaceTemplate.spec.linux.browserPolicy` is removed** (CRD field, types, deepcopy, samples, docs, and any portal or OpenAPI mention). The release notes say per-template URL filtering is not offered in 1.0. — Reason: an unenforced restriction in a public CRD is worse than none; enforcing it is a feature. — Cost if wrong: re-adding the field later is additive.
6. **R1-28: the default ClusterOnly egress peer admits only namespaces that carry the workspace's own tenant label value.** The operator reads the tenant from the Workspace CR's `workspaces.cdi.tinyorbit.vn/tenant` label (set by the API; the chart puts the same value on each managed Namespace), so no Namespace read or new RBAC is needed; a CR without the label is limited to its own namespace. A tenant with several managed namespaces keeps reaching all of them. Cross-tenant reach needs the explicit `clusterOnlyEgress` selector or `unrestricted`. — Reason: least privilege. — Cost if wrong: a deployment that relied on cross-tenant ClusterOnly reach must set the selector.
7. **R1-03: local per-client unknown-cookie lookup limiter on the app listener**, the same shape as the gateway's `SessionLookupLimiter` (keyed by `ratelimit.ClientKey`, local, never shared). Only lookups that miss spend the budget. The opportunistic `DELETE` leaves the session read path, and expired rows are pruned by the leader's existing sweep. — Reason: the brief's fix. — Cost if wrong: a NAT with many expired cookies sees anonymous answers on the probe for a minute.
8. **The last task (30) updates `docs/security/review-v1.0.md`.** Each finding's status becomes `fixed (<commit subject>)` or, for the R1-07 node-level part, `fixed (<subject>); node-level enforcement accepted (owner)`. The task restores the links that W2 turned into plain text (`README.md` Status, `SECURITY.md`, `.github/README.md` environments) to `docs/security/review-v1.0.md`, and checks release.yml's review line with `.github/tests/release-notes-review-status.test.sh`. It brings the threat-model register rows the findings touched to the post-fix control state (Partial/Gap → Verified where fixed), adds the new rows, and adds every new test to `docs/security/test-inventory.md`. — Reason: owner. — Cost if wrong: none.
9. **Verify command** (the workflow's whole-branch check):
   `go build ./... && go vet ./... && GOFLAGS=-timeout=40m make test && go test -count=1 ./deploy/helm/ && KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path --bin-dir "$PWD/bin")" go test -tags=integration -count=1 -skip '^TestLinuxRuntime(ReadinessAndHome|FirefoxFallback|Desktop|BrowserBaseline|ClipboardParamsNotOverridable)$|^TestKasmAdapter' ./tests/integration/... && (cd web && npm ci && npm run typecheck && npm run lint:strings && npm run test:unit && npm run build) && for t in .github/tests/*.test.sh; do bash "$t" || exit 1; done`
   `GOFLAGS=-timeout=40m` is there because `make test` runs `internal/operator` (about 10 minutes, the `go test` default timeout) without `-timeout`. `TestLinuxRuntimeClipboardParamsNotOverridable` (Task 17) needs the built runtime image, like the other skipped `TestLinuxRuntime*` tests. No CI job runs `TestLinuxRuntime*` (`ci.yml`'s cluster-free integration job skips them; Task 17 adds the new name to that skip regex); they run locally with the images built. The `kasm-contract` job runs the adapter subtest `TestKasmAdapterChromium/ClipboardParamsNotOverridable`. Task 17 also adds a cheap local check (`.github/tests/clipboard-policy.test.sh`). — Reason: owner. — Cost if wrong: none.

## Plan rulings

These are the planner's rulings, made against the code at `98c7f54`. Each gives the ruling, the reason, and the cost if it is wrong.

1. **Threat-model edits are split.** A task whose finding is a doc-accuracy issue, or whose fix makes a sentence false, edits that prose (and its register row's Residual cell) itself. Task 30 owns the register rows' Control and Pinned-by cells for code fixes, the new rows, and the test inventory. — Reason: each task stays self-contained, and the register is updated once with every test name final. — Cost if wrong: a row is briefly stale between commits on the branch; nothing ships in between.
2. **New threats get appended register rows TM-51 onward (Task 30)**, not rows inserted in boundary order. Inserting would renumber TM-NN references across the repo. — Reason: `threat-model-refs.test.sh` requires gap-free order only. — Cost if wrong: the new rows sit out of boundary order.
3. **R1-05 uses a server-level `ReadTimeout` on the session listener** (a package variable, 30 s, so tests can shorten it). `MaxHeaderBytes` drops to 64 KiB. No hijack code is needed: `net/http` already clears a connection's deadlines when a handler hijacks it (`hijackLocked`), so an admitted WebSocket is unaffected. The transfer leg keeps its existing per-request deadline extension. Task 14 corrects the `listeners.go` comment and `TestSessionServerTimeouts`, which claim the opposite. — Reason: it also bounds the post-handler body drain and every path outside the gateway handler. — Cost if wrong: a long non-hijacked request that does not extend its deadline is cut at the bound (Review Focus 1 pins the two legitimate long reads).
4. **R1-13 enforces a file-name allowlist** in `serveBranding`, with the names `docs/branding.md` documents. The wording of TM-05 then stays true. — Reason: fixing the code beats weakening the claim. — Cost if wrong: an operator's extra file in the branding dir is not served.
5. **R1-21 is fixed in the docs.** TM-17 says the portal runs a dry run first and the API requires an explicit `dryRun` flag. No server-side dry-run token is added. — Reason: `:apply` is within the tenant admin's privilege, and a token adds server state for no boundary gain. — Cost if wrong: a scripted confirm can still skip review; the owner can ask for the token later.
6. **R1-39: the required checks are every `ci.yml` job except a documented exclusion list of network-dependent jobs:** `kasm adapter contract + catalog scan`, `quickstart (kind up.sh + portal smoke)` and `partitioned (kind up.sh + CHIPS cookie smoke)`. `go fuzz (auth/session parsing boundaries)` and `soak harness (npm ci, tsc, unit + dry-run)` become required. The new test asserts `contexts == ci.yml job names − exclusions`. Applying the script to the repo (`--apply`) is an owner step after the public-repo reset. — Reason: the existing owner rule is "every job except network-dependent ones". — Cost if wrong: a flaky required check blocks a merge.
7. **R1-32: new backend flag `-internal-gateway-cns`** (comma list, empty by default, env `TCDI_INTERNAL_GATEWAY_CNS`). The chart does not render it, because the gateway is in-process. — Reason: the brief's fix. — Cost if wrong: a split-mode gateway must be listed explicitly.
8. **Commit subjects are fixed by this plan** (each task's last step), because Task 30 quotes them in the report. An implementer must not reword a subject.

## Global Constraints

- No upgrade path. Never add a compatibility alias, fallback branch, dual read or migration for data or objects written by earlier builds. Full breaking changes are allowed (spec §1).
- Schema edits go into `internal/store/migrations/0001_baseline.sql`. After any edit, regenerate the snapshot with `TCDI_UPDATE_SNAPSHOT=1 go test ./internal/store/ -run TestBaseline_SchemaMatchesSnapshot -count=1`, then rerun without the variable.
- After any edit under `api/v1/` (comments included, because they render into CRD descriptions), run `make manifests generate sync-chart-crds`. The chart's `crds/` directory must equal `config/crd/bases` byte for byte.
- After any change that alters the default chart render (templates, CRDs, values), regenerate the golden from `deploy/helm/`: `${TCDI_HELM:-helm} template tcdi ./tinycdi --namespace tcdi-system --include-crds -f tinycdi/ci/minimal-values.yaml > testdata/default-render.golden.yaml` (`deploy/helm/chart_defaultrender_test.go`).
- After any edit to `internal/api/openapi.yaml`, run `cd web && npm run generate`. A new API error code needs its portal string (`npm run lint:strings`).
- Docs are English and describe only v1.0.0. They carry no internal version history (`v0.x`, `v1.1`, `v1.2`, `rc.N`), no PR or issue numbers, and no review-tag prose ("fixed in", "since", "no longer", "previously", "R1-NN"). Finding ids appear only in `docs/security/review-v1.0.md` and in the `docs/security/test-inventory.md` section Task 30 adds, where they are the point.
- Every fact a doc states must be checkable in the code. Keep the machine-edited anchors that W2 listed byte-compatible in substance (`docs/images.md` `- Chromium:` / `- Firefox ESR` lines, `docs/compatibility.md` `current ARG` line and `| Firefox ESR fallback |` row, digest-pinned `kasmweb/*` refs, `.github/README.md` lines matching `cosign verify-blob .*runtime-images\.json`, `runtime-images\.yml@refs/heads/main`, `TRAIN_ATTESTATIONS_ENABLED`; `docs/security/provenance.md` lines matching `pullable|digest-addressable` and `never (signed|tagged)|unsigned`).
- `.github/tests` scripts start with `#!/usr/bin/env bash` and `set -uo pipefail`, set `ROOT="$(cd "$(dirname "$0")/../.." && pwd)"`, print `FAIL: …` per failure, and end with `exit "$fails"`. They use only GNU grep, awk, sed, git and python3, with no network and no `rg`. The `workflow-policy` CI job runs every `.github/tests/*.test.sh`, so a new script needs no workflow edit.
- **`.github/tests/security-doc-claims.test.sh`** (Task 10 creates it; Tasks 16, 18, 20, 25 and 29 append to it) pins corrected security claims in prose. Its helpers:
  - `need <finding> <repo-relative file> <ERE>`: the file, flattened to one line with runs of whitespace squeezed to one space, must match the ERE (`grep -qE`).
  - `absent <finding> <file> <ERE>`: the flattened file must not match.
  - `need_row <finding> <TM-NN> <ERE>`: the threat-model register row `| TM-NN | …` must match.

  To append, insert the new lines directly above the final `exit "$fails"` line.
- Tests that need Postgres start `postgres:18.0` through the package's own helper. They skip without Docker, and **a skip is not a pass**. Run them on a host with Docker.
- A test that needs a built runtime image (`tests/integration/linux_runtime_test.go`, `kasm_adapter_test.go`) is in the verify command's skip list and in `ci.yml`'s cluster-free integration `-skip` regex. CI's `kasm-contract` job runs `TestKasmAdapterChromium`; no CI job runs `TestLinuxRuntime*`, which run locally with `TCDI_IT_BUILD=1`. Each such task also gives a local check that runs without images.
- Do not edit `docs/superpowers/**` or any tag. Never push. `.github/workflows/*.yml` may be edited only by Tasks 26–29, plus one exception: Task 17 adds its image-bound test name to the `-skip` regex (and its comment) of `ci.yml`'s cluster-free integration job, so that job does not run a test that needs a built image.
- Each task's security test must fail before its fix. Run it, and see it fail for the stated reason, before you write the fix.
- Commit messages are conventional and use the exact subject the task gives. Every commit ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Focused commands per task are given in its steps. At the end of every task, `for t in .github/tests/*.test.sh; do bash "$t" || exit 1; done` must pass.

## Review Focus

1. **A long legitimate read on the session listener after the R1-05 bound.** This covers a KasmVNC WebSocket that streams for hours and a file-transfer upload that takes longer than the bound. Expected: both survive; only a stalled request phase is cut. Pinned in Task 14: `TestSessionListener_AdmittedStreamOutlivesReadBound` and `TestSessionListener_TransferUploadOutlivesReadBound`.
2. **A WebSocket upgrade (`101 Switching Protocols`) through the 1xx-suppressing writer of R1-04.** Expected: 101 still switches, and every frame flows; only 100 and 102–199 are dropped. Pinned in Task 13: `TestProxy_WebSocketUpgradeSurvives1xxGuard`.
3. **A legitimately stopped or deleted workspace after the R1-01 settle lock.** Expected: its reservation is still released on the next settle pass, so quota does not leak. Only a workspace that is wanted Running again in a new generation keeps its hold. Pinned in Task 1: `TestSettleQuota_StoppedWorkspaceStillReleases`.
4. **A tenant admin writing a quota exactly at the ceiling, or lowering one below current usage (R1-02).** Expected: 200 for both. Only a value above the ceiling, or any write without a declared ceiling, is refused. Pinned in Task 3: `TestAdminQuota_PutAtCeilingAccepted`.
5. **Many real users behind one NAT after the R1-03 limiter.** Expected: valid sessions never spend the unknown-cookie budget, so 50 signed-in users on one client key are never throttled. Only misses count. Pinned in Task 6: `TestAppListener_ValidSessionsBehindOneClientNotThrottled`.

---

## Task index

| # | Title | Tier | Findings | Commit subject |
|---|---|---|---|---|
| 1 | Settle releases a reservation only under the workspace row lock | design | R1-01 | `fix(quota): settle releases a reservation only under the workspace row lock` |
| 2 | Exact reservation vector; a revision move re-reserves | design | R1-20 | `fix(quota): reserve exact template bytes and re-reserve on a revision move` |
| 3 | Operator-declared tenant quota ceilings | design | R1-02 | `feat(quota): operator-declared tenant quota ceilings bound tenant-admin writes` |
| 4 | Audit hygiene: bounded targets, route-pattern request log, in-tx single-statement writes | integrate | R1-16, R1-22, R1-23 | `fix(audit): bound audit targets, log route patterns, audit single-statement writes in-tx` |
| 5 | Retained PVC label carries the CR name | transcribe | R1-24 | `fix(retained): label retained PVCs with the workspace CR name` |
| 6 | App-listener unknown-cookie lookup limiter | design | R1-03 | `fix(auth): bound unknown-cookie store lookups per client on the app listener` |
| 7 | /v1 browser headers; cookie and ID-token contract | integrate | R1-10, R1-11, R1-12 | `fix(api): pin browser security headers on /v1 and correct the cookie and id-token contract` |
| 8 | Login with a valid session keeps it | integrate | R1-14 | `fix(auth): a login started with a valid session keeps it` |
| 9 | Branding serves documented names only | transcribe | R1-13 | `fix(frontend): serve only the documented branding file names` |
| 10 | Admin-role revocation latency and the :apply contract (docs) | transcribe | R1-17, R1-21 | `docs(security): state admin-role revocation latency and the :apply dry-run contract` |
| 11 | Foreign-owner tickets 404; NUL filters and cursors 400 | integrate | R1-18, R1-19 | `fix(api): foreign-owner tickets answer 404 and NUL filters answer 400` |
| 12 | Every refused ticket redemption consumes the ticket | integrate | R1-25 | `fix(broker): every refused ticket redemption consumes the ticket` |
| 13 | Upstream 1xx cannot strip pinned session headers | design | R1-04 | `fix(gateway): upstream 1xx responses cannot strip pinned session headers` |
| 14 | Session listener read bound | design | R1-05 | `fix(backend): bound request reads on the session listener` |
| 15 | Health on control hosts only; control token length and failure limit | integrate | R1-26 | `fix(backend): health only on control hosts; control token length and failure limit` |
| 16 | In-pod trust covers the desktop leg | integrate | R1-27 | `docs(security): in-pod trust covers the desktop leg; drop upstream Content-Disposition` |
| 17 | KasmVNC refuses runtime clipboard overrides | integrate | R1-06 | `fix(runtime): KasmVNC refuses runtime overrides of clipboard parameters` |
| 18 | podPidsLimit install requirement and preflight check | integrate | R1-07 | `docs(install): require a kubelet podPidsLimit on workspace nodes and check it in preflight` |
| 19 | ClusterOnly egress in-tenant; NetworkPolicy before the pod | integrate | R1-28, R1-35 | `fix(runtime): ClusterOnly egress stays in-tenant and the NetworkPolicy precedes the pod` |
| 20 | Userns browser profile pair requires hostUsers false | integrate | R1-29 | `fix(runtime): the userns browser profile pair requires hostUsers false` |
| 21 | Remove WorkspaceTemplate browserPolicy | integrate | R1-08 | `fix(crd)!: remove the unenforced WorkspaceTemplate browserPolicy` |
| 22 | CA rotation severs removed-CA conns; canonical blocklist issuers | design | R1-30, R1-31 | `fix(mtls): CA rotation severs removed-CA connections; blocklist matches issuers canonically` |
| 23 | Gateway role opt-in by CN; handshake and role refusals counted | design | R1-32, R1-33 | `fix(mtls): gateway role is opt-in by CN; count handshake and role refusals` |
| 24 | extraArgs/extraEnv cannot override rendered flags | design | R1-34 | `fix(chart): extraArgs and extraEnv cannot override any chart-rendered flag` |
| 25 | Trusted-proxy guidance; dead podSecurity value removed | integrate | R1-15, R1-36 | `fix(chart): narrow trusted-proxy guidance and remove podSecurity.platformEnforce` |
| 26 | Train: build time carried to the manifest; stale scan refused before signing | design | R1-09 (train) | `ci(runtime): carry build time into the train manifest and refuse a stale scan before signing` |
| 27 | Release: runtime builtAt stamped; stale scan refused before signing | design | R1-09 (release) | `ci(release): stamp runtime builtAt and refuse a stale scan before signing` |
| 28 | Publish gating per job; required checks; unsigned chart never left tagged | integrate | R1-38, R1-39, R1-40 | `ci: pin publish gating per job, codify required checks, never leave an unsigned chart tagged` |
| 29 | Kasm catalog scan gates releases; no shared cache for main images | integrate | R1-37, R1-41 | `ci: gate releases on the kasm catalog scan and build main images without the shared cache` |
| 30 | Review status, threat-model register, test inventory, links | integrate | all | `docs(security): record R1 fix status in the review, threat model and test inventory` |

Task order: 1 → 30. Task 10 creates `.github/tests/security-doc-claims.test.sh`, which Tasks 16, 18, 20, 25 and 29 append to. Tasks 3 and 4 both edit `internal/api/adminquota.go`. Tasks 6, 7, 14 and 15 edit `internal/backend/{wire.go,listeners.go}`. Tasks 13 and 16 edit `responsePolicy` in `internal/gateway/proxy.go`. Tasks 19, 20 and 21 edit `internal/runtime/linux/backend.go`. Tasks 22 and 23 edit `internal/broker/httpapi/server.go` and `internal/backend/listeners.go`. Find code by symbol, not by line, after an earlier task has run.

---

### Task 1: Settle releases a reservation only under the workspace row lock (R1-01) — tier: design

**Finding (R1-01, final severity P2, TM-09).** `SettleQuota` proves runtime absence (`RuntimeGone`) outside any transaction. In a separate transaction it then calls `Release`, without locking the `workspaces` row and without re-checking `desired_state`, `state` or `runtime_generation`. The attack: an end user stops an Ephemeral workspace, and when its pod disappears sends `POST /v1/workspaces/{id}:start`. If the start commits between the absence proof and the release, `reserveForStart -> Reserve` sees the still-`held` row (`held == v`, `quota.go:209`) and treats it as an idempotent retry. `RuntimeGone` is still true, because the new pod does not exist yet. `Release` then flips the reservation to `released`. The workspace runs with no reservation, so it counts against neither the per-user limit nor the tenant quota until its next stop. Repeating the cycle lets a user run more workspaces than allowed. The Retain branch (`convertToDiskOnly`) already re-checks under the row lock; the Ephemeral branch does not.
Required fix: in the `SettleQuota` transaction, `SELECT desired_state, state, runtime_generation FROM workspaces WHERE id=$1 FOR UPDATE`, re-apply the settle predicate (`state='deleted' OR desired_state='Stopped'`), require the generation observed at candidate selection, and skip otherwise. As defense in depth, put the same predicate into `Release`'s UPDATE. Invariant: a reservation is released only while its workspace is deleted or wanted Stopped, in the same generation.
Security test: `TestSettleQuota_StartRaceKeepsReservation` in `internal/provisioning/recovery_test.go`. An observer hook commits a `:start` inside `RuntimeGone`. The test asserts that the reservation stays `held` and that a second create over the limit is refused.

Brief claims re-checked at `98c7f54`: `internal/provisioning/recovery.go:225-262` (`SettleQuota`; `Release` call at `:258`), `internal/provisioning/quota.go:368-381` (`Release`), `quota.go:209` (`state == "held" && held == v` idempotent branch) and `internal/provisioning/service.go:765` (`reserveForStart`) are all accurate.

**Files:**
- Modify: `internal/provisioning/recovery.go:19` (sentinel block), `:219-263` (`SettleQuota`)
- Modify: `internal/provisioning/quota.go:364-381` (`Release`)
- Modify: `tests/integration/api_admission_test.go:408-424` (Phase C of `TestOutboxCrashRecovery`; see Step 5)
- Modify test: `internal/provisioning/retain_stop_quota_test.go:187-189` (`TestRetainStop_StartedBeforeConversionKeepsCompute`; see Step 4)
- Test: `internal/provisioning/recovery_test.go` (append)

**Interfaces:**
- Consumes: `provisioning.NewRecovery(db, obs)`, `(*Recovery).SettleQuota(ctx, tenantID string, uid PlatformID) error`, `provisioning.NewSettleTrigger()`, `(*SettleWorker).DrainOnce(ctx)`, `(*Service).SignalWorkspace(...)`, and the test helpers `recoveryDB` and `quotaVec` (`recovery_test.go:129,198`) and `ulSetDefault` and `i64` (`userlimit_test.go:27,65`). All are unchanged.
- Produces:
  - `var provisioning.ErrSettleRaced = fmt.Errorf("%w: the workspace was started again during the absence check", ErrRuntimeNotProvenGone)`. `errors.Is(err, ErrRuntimeNotProvenGone)` holds for it, so `SettleWorker.DrainOnce` (`settle_events.go:172`) and the recovery pass (`recovery.go:449-462`) already treat it as "absence unproven, keep held" with no caller change.
  - `Release(ctx, tx, tenantID, workspaceID string, proof AbsenceProof) error`: same signature. It now returns `ErrSettleRaced` and writes nothing when the reservation is held and its workspace row is `state='active' AND desired_state='Running'`.
  - New test type `startDuringObserve` in package `provisioning_test` (`recovery_test.go`).

- [ ] **Step 1: Write the failing tests**

Append to `internal/provisioning/recovery_test.go`. The imports it needs (`context`, `errors`, `testing`, `provisioning`, `store`) are already in the file's import block.

```go
// startDuringObserve runs fire once inside RuntimeGone — the window between
// the settle candidate check and the release transaction — and then still
// reports the runtime gone (the new pod is not created yet).
type startDuringObserve struct {
	fire func()
}

func (s *startDuringObserve) RuntimeGone(context.Context, provisioning.PlatformID) (bool, error) {
	if s.fire != nil {
		f := s.fire
		s.fire = nil
		f()
	}
	return true, nil
}

// settleRaceEnv seeds a tenant with RunningSlots 1 and a per-user limit of
// 1, creates one Running Ephemeral workspace for iss|sub, stops it and acks
// the stop intent. It returns the service, the workspace id and a create
// helper for a second workspace of the same user.
func settleRaceEnv(t *testing.T, tenant string) (*store.DB, *provisioning.Service, string, func(name string) error) {
	t.Helper()
	db := recoveryDB(t)
	ctx := context.Background()
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.SetQuota(ctx, tx, tenant, provisioning.ResourceVector{
			RunningSlots: 1, CPUMillis: 64000, MemoryBytes: 128 << 30, DiskBytes: 4 << 40})
	}); err != nil {
		t.Fatal(err)
	}
	ulSetDefault(t, db, tenant, i64(1))
	svc := provisioning.NewService(db)
	mk := func(name string) (provisioning.WorkspaceRecord, error) {
		return svc.CreateWorkspace(ctx, tenant, "key-"+name, provisioning.CreateRequest{
			OwnerIssuer: "iss", OwnerSubject: "sub", Name: name,
			Template: provisioning.TemplateInfo{ID: "t", Name: "tpl"},
			Vector:   quotaVec, DesiredState: "Running", DataPolicy: "Ephemeral",
		}, []byte(name))
	}
	ws1, err := mk("ws-1")
	if err != nil {
		t.Fatalf("create ws-1: %v", err)
	}
	if _, err := mk("ws-2"); err == nil {
		t.Fatal("control: a second running workspace must be refused before any settle")
	}
	if _, err := svc.SignalWorkspace(ctx, tenant, "iss|sub", "iss|sub", ws1.ID, "", provisioning.IntentStop, nil); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE outbox_intent SET dispatched_at = now()`); err != nil {
		t.Fatal(err)
	}
	create := func(name string) error { _, err := mk(name); return err }
	return db, svc, ws1.ID, create
}

func reservationState(t *testing.T, db *store.DB, wsID string) string {
	t.Helper()
	var st string
	if err := db.Pool().QueryRow(context.Background(),
		`SELECT state FROM quota_reservation WHERE workspace_id = $1`, wsID).Scan(&st); err != nil {
		t.Fatalf("reservation state: %v", err)
	}
	return st
}

// TestSettleQuota_StartRaceKeepsReservation (R1-01): a :start that commits
// while the settle pass is proving absence keeps the reservation held, so a
// second workspace over the user limit and the tenant quota is refused.
func TestSettleQuota_StartRaceKeepsReservation(t *testing.T) {
	const tenant = "tenant-race"
	ctx := context.Background()
	db, svc, ws1, create := settleRaceEnv(t, tenant)
	obs := &startDuringObserve{fire: func() {
		if _, err := svc.SignalWorkspace(ctx, tenant, "iss|sub", "iss|sub", ws1, "", provisioning.IntentStart, nil); err != nil {
			t.Errorf("racing start: %v", err)
		}
	}}
	trig := provisioning.NewSettleTrigger()
	w := &provisioning.SettleWorker{DB: db, Trigger: trig, Rec: provisioning.NewRecovery(db, obs)}
	trig.Enqueue(provisioning.PlatformID(ws1))
	w.DrainOnce(ctx)

	if st := reservationState(t, db, ws1); st != "held" {
		t.Fatalf("reservation of the restarted workspace = %q, want held", st)
	}
	if err := create("ws-2b"); err == nil {
		t.Fatal("BYPASS: a second workspace was admitted over user limit 1 and tenant RunningSlots 1")
	}

	// The direct call reports the race as an unproven absence.
	if err := provisioning.NewRecovery(db, &fakeObserver{gone: true}).SettleQuota(ctx, tenant, provisioning.PlatformID(ws1)); !errors.Is(err, provisioning.ErrRuntimeNotProvenGone) {
		t.Fatalf("SettleQuota on a workspace wanted Running = %v, want ErrRuntimeNotProvenGone", err)
	}

	// A stop-then-start-then-stop inside the window moves the generation:
	// desired is Stopped again, but the absence proof is for an older
	// generation, so the hold stays.
	if _, err := svc.SignalWorkspace(ctx, tenant, "iss|sub", "iss|sub", ws1, "", provisioning.IntentStop, nil); err != nil {
		t.Fatalf("stop again: %v", err)
	}
	obs2 := &startDuringObserve{fire: func() {
		for _, k := range []provisioning.IntentKind{provisioning.IntentStart, provisioning.IntentStop} {
			if _, err := svc.SignalWorkspace(ctx, tenant, "iss|sub", "iss|sub", ws1, "", k, nil); err != nil {
				t.Errorf("racing %s: %v", k, err)
			}
		}
	}}
	err := provisioning.NewRecovery(db, obs2).SettleQuota(ctx, tenant, provisioning.PlatformID(ws1))
	if !errors.Is(err, provisioning.ErrSettleRaced) {
		t.Fatalf("SettleQuota across a generation move = %v, want ErrSettleRaced", err)
	}
	if st := reservationState(t, db, ws1); st != "held" {
		t.Fatalf("reservation after a generation move = %q, want held", st)
	}
}

// TestSettleQuota_StoppedWorkspaceStillReleases (Review Focus 3): the row
// lock must not leak quota — a workspace that stays Stopped is released on
// the settle pass, and a deleted one too.
func TestSettleQuota_StoppedWorkspaceStillReleases(t *testing.T) {
	const tenant = "tenant-settle-ok"
	ctx := context.Background()
	db, svc, ws1, create := settleRaceEnv(t, tenant)
	trig := provisioning.NewSettleTrigger()
	w := &provisioning.SettleWorker{DB: db, Trigger: trig, Rec: provisioning.NewRecovery(db, &fakeObserver{gone: true})}
	trig.Enqueue(provisioning.PlatformID(ws1))
	w.DrainOnce(ctx)
	if st := reservationState(t, db, ws1); st != "released" {
		t.Fatalf("stopped workspace reservation = %q, want released", st)
	}
	if err := create("ws-3"); err != nil {
		t.Fatalf("create after a proven stop: %v", err)
	}

	// Deleted: the new workspace is deleted, settles and frees its slot.
	var ws3 string
	if err := db.Pool().QueryRow(ctx, `SELECT id FROM workspaces WHERE name = 'ws-3'`).Scan(&ws3); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SignalWorkspace(ctx, tenant, "iss|sub", "iss|sub", ws3, "", provisioning.IntentDelete, nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE outbox_intent SET dispatched_at = now()`); err != nil {
		t.Fatal(err)
	}
	if err := provisioning.NewRecovery(db, &fakeObserver{gone: true}).SettleQuota(ctx, tenant, provisioning.PlatformID(ws3)); err != nil {
		t.Fatalf("settle deleted: %v", err)
	}
	if st := reservationState(t, db, ws3); st != "released" {
		t.Fatalf("deleted workspace reservation = %q, want released", st)
	}
}
```

- [ ] **Step 2: Run them and see them fail**

Run: `go test ./internal/provisioning/ -run 'TestSettleQuota_(StartRaceKeepsReservation|StoppedWorkspaceStillReleases)' -count=1 -v 2>&1 | grep -E '^(=== RUN|--- |\s+recovery_test)' | head -20`

Expected: the package fails to compile with `undefined: provisioning.ErrSettleRaced`. To see the behavioral failure first, temporarily replace `provisioning.ErrSettleRaced` with `provisioning.ErrRuntimeNotProvenGone` and rerun. `TestSettleQuota_StartRaceKeepsReservation` then fails with `reservation of the restarted workspace = "released", want held`, and `TestSettleQuota_StoppedWorkspaceStillReleases` passes. Restore the name afterwards. Docker is required; a skip is not a pass.

- [ ] **Step 3: Implement the lock and the predicate**

In `internal/provisioning/recovery.go`, replace line 19 (`var ErrRuntimeNotProvenGone = …`) with:

```go
var ErrRuntimeNotProvenGone = errors.New("provisioning: runtime absence not proven")

// ErrSettleRaced: the workspace was started again (or its runtime
// generation moved) between the absence proof and the release. It wraps
// ErrRuntimeNotProvenGone, so every settle caller keeps the hold and
// retries on the next pass.
var ErrSettleRaced = fmt.Errorf("%w: the workspace was started again during the absence check", ErrRuntimeNotProvenGone)
```

Replace the whole `SettleQuota` function (doc comment included, lines 219-263) with:

```go
// SettleQuota releases the workspace's held reservation if and only if the
// observer proves the runtime is gone AND the workspace is still deleted or
// wanted Stopped in the runtime generation that was current before the
// proof. Proof is mapped to AbsenceProof: observed-absent ->
// ProofRuntimeAbsent; a workspace whose create intent never dispatched ->
// ProofNeverCreated. Any observer error or still-present report returns
// ErrRuntimeNotProvenGone and releases nothing. The release transaction
// locks the workspaces row and re-checks the settle predicate and the
// generation, so a :start that commits during the proof keeps its
// reservation (ErrSettleRaced). It is idempotent: an already-released
// reservation is a no-op.
func (r *Recovery) SettleQuota(ctx context.Context, tenantID string, workspaceUID PlatformID) error {
	// The generation the absence proof is about. A row that does not exist
	// can never be started again; -1 marks it.
	gen := int64(-1)
	if err := r.db.Pool().QueryRow(ctx,
		`SELECT runtime_generation FROM workspaces WHERE id = $1`, workspaceUID).
		Scan(&gen); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("settle: read generation %s: %w", workspaceUID, err)
	}
	// A never-dispatched create intent is the strongest absence proof: no
	// runtime was ever materialized.
	pendingCreate, err := r.pendingCreateIntent(ctx, workspaceUID)
	if err != nil {
		return err
	}
	proof := ProofRuntimeAbsent
	if pendingCreate {
		proof = ProofNeverCreated
	} else {
		if r.obs == nil {
			return ErrRuntimeNotProvenGone
		}
		gone, err := r.obs.RuntimeGone(ctx, workspaceUID)
		if err != nil || !gone {
			// Ambiguity never frees quota.
			return ErrRuntimeNotProvenGone
		}
	}
	return r.db.WithTx(ctx, func(tx store.Tx) error {
		var policy, state, desired string
		var curGen int64
		err := tx.QueryRow(ctx, `
			SELECT data_policy, state, desired_state, runtime_generation
			FROM workspaces WHERE id = $1 FOR UPDATE`,
			workspaceUID).Scan(&policy, &state, &desired, &curGen)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// No row: nothing can start it again; settle on the proof.
		case err != nil:
			return fmt.Errorf("settle: lock workspace %s: %w", workspaceUID, err)
		case state != "deleted" && desired != "Stopped":
			// Started again after the candidate was picked: its new
			// runtime needs this reservation.
			return ErrSettleRaced
		case curGen != gen:
			// Stopped again, but in a newer generation than the absence
			// proof: that generation's runtime is not proven gone.
			return ErrSettleRaced
		}
		if state == "active" && policy == "Retain" {
			// A stopped Retain workspace keeps its volume: compute is free
			// on the absence proof, the home disk stays held on the same
			// reservation and a start re-acquires the compute.
			_, err := convertToDiskOnly(ctx, tx, tenantID, string(workspaceUID))
			return err
		}
		if err := Release(ctx, tx, tenantID, string(workspaceUID), proof); err != nil {
			return err
		}
		// Compute is free; a retained disk the workspace left behind keeps
		// counting until it is purged (design: retained quota holds to purge).
		return holdRetainedDisk(ctx, tx, tenantID, string(workspaceUID))
	})
}
```

`recovery.go` already imports `errors`, `fmt`, `pgx` and `store`. Check with `go build ./internal/provisioning/`.

In `internal/provisioning/quota.go`, replace `Release` (doc comment and body, lines 364-381) with:

```go
// Release frees a held reservation and drops any stored restart vector (a
// disk-only hold that is released belongs to a deleted workspace). proof must
// evidence that the runtime no longer consumes compute; ProofUnspecified is
// rejected so a failed request or HTTP timeout never silently frees quota.
// A reservation whose workspace is active and wanted Running is never
// released (ErrSettleRaced): the release predicate is part of the UPDATE, so
// it holds even for a caller that skipped SettleQuota's row lock.
func Release(ctx context.Context, tx store.Tx, tenantID, workspaceID string, proof AbsenceProof) error {
	if !proof.Valid() {
		return ErrProofRequired
	}
	tag, err := tx.Exec(ctx, `
		UPDATE quota_reservation qr
		SET state = 'released', release_proof = $3, released_at = now(),
		    restart_slots = NULL, restart_cpu_millis = NULL, restart_memory_bytes = NULL
		WHERE qr.workspace_id = $1 AND qr.tenant_id = $2 AND qr.state = 'held'
		  AND NOT EXISTS (
		      SELECT 1 FROM workspaces w
		      WHERE w.id = qr.workspace_id AND w.state = 'active' AND w.desired_state = 'Running')`,
		workspaceID, tenantID, string(proof))
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	// Nothing released: either already released (idempotent no-op) or the
	// predicate kept a held row for a workspace wanted Running.
	var blocked bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM quota_reservation
		               WHERE workspace_id = $1 AND tenant_id = $2 AND state = 'held')`,
		workspaceID, tenantID).Scan(&blocked); err != nil {
		return fmt.Errorf("release: %w", err)
	}
	if blocked {
		return ErrSettleRaced
	}
	return nil
}
```

- [ ] **Step 4: Run the focused tests**

Run: `go test ./internal/provisioning/ -run 'TestSettleQuota_|TestRecovery_|TestUserLimit|TestRetainStop|TestReleasePending|TestPurge_' -count=1 2>&1 | tail -5`
Expected: `ok  	github.com/tinyorbitvn/tinycdi/internal/provisioning`. Every other existing `SettleQuota` caller in the tests runs on a workspace that is Stopped or deleted (`recovery_test.go:212-267,460`, `userlimit_test.go:248,278`, `release_pending_test.go:88`), or on a seeded row whose `desired_state` defaults to `Stopped` (`seedHeldWorkspace`).

One existing test settles a workspace wanted Running and must change: `TestRetainStop_StartedBeforeConversionKeepsCompute` (`internal/provisioning/retain_stop_quota_test.go:183-191`) stops, then starts, then calls `SettleQuota` and requires `nil`. After the fix that call returns `ErrSettleRaced`. Add `"errors"` to that file's import block and replace lines 187-189:

```go
	if err := e.rec.SettleQuota(context.Background(), retainTenant, provisioning.PlatformID(e.ws)); err != nil {
		t.Fatalf("SettleQuota on a running workspace: %v", err)
	}
```

with:

```go
	if err := e.rec.SettleQuota(context.Background(), retainTenant, provisioning.PlatformID(e.ws)); !errors.Is(err, provisioning.ErrRuntimeNotProvenGone) {
		t.Fatalf("SettleQuota on a running workspace = %v, want ErrRuntimeNotProvenGone (ErrSettleRaced)", err)
	}
```

The `e.want("running again", quotaVec)` line after it stays.

- [ ] **Step 5: Fix the one direct `Release` caller that releases a Running workspace**

`tests/integration/api_admission_test.go` Phase C (lines 392-424) creates `resC` with the test's `req` and calls `provisioning.Release` directly with `ProofNeverCreated`. `TestOutboxCrashRecovery`'s `req` is `DesiredState: "Stopped"` (line 317), so the guard does not block it. Confirm this and change nothing else:

Run: `KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path --bin-dir "$PWD/bin")" go test -tags=integration -count=1 -run 'TestOutboxCrashRecovery|TestQuota|TestIntentRevisionOrdering|TestStartAfterStop' ./tests/integration/ 2>&1 | tail -5`
Expected: `ok`. If a test fails with `ErrSettleRaced` (`the workspace was started again during the absence check`), it releases the reservation of a workspace wanted Running. That is exactly the bug. Make that test stop the workspace (`UPDATE workspaces SET desired_state = 'Stopped' WHERE id = $1`) before its release, with the comment `// settle frees only a workspace that is deleted or wanted Stopped (R1-01)`.

- [ ] **Step 6: Whole package and gates**

Run: `go test ./internal/provisioning/ -count=1 2>&1 | tail -3 && go vet ./internal/provisioning/ && for t in .github/tests/*.test.sh; do bash "$t" >/dev/null || echo "FAIL $t"; done`
Expected: `ok`, no vet output, no `FAIL` line.

- [ ] **Step 7: Commit**

```bash
git add internal/provisioning/recovery.go internal/provisioning/quota.go internal/provisioning/recovery_test.go internal/provisioning/retain_stop_quota_test.go tests/integration/api_admission_test.go
git commit -m "fix(quota): settle releases a reservation only under the workspace row lock" -m "R1-01: SettleQuota locks the workspaces row, re-checks deleted/wanted-Stopped and the runtime generation seen before the absence proof; Release carries the same predicate. Pinned by TestSettleQuota_StartRaceKeepsReservation and TestSettleQuota_StoppedWorkspaceStillReleases." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```
(`git add` of an unchanged file is a no-op, so listing the integration test is safe whether or not Step 5 changed it.)

### Task 2: Exact reservation vector; a revision move re-reserves (R1-20) — tier: design

**Finding (R1-20, final severity P3, TM-09).** The quota reservation vector under-counts what actually runs, in two ways:
- The API builds the vector from display units. `MemoryBytes: tpl.MemoryMiB << 20` and `DiskBytes: tpl.StorageGiB << 30` truncate, so storage below 1 GiB (for example `512Mi`) reserves 0 disk bytes.
- `reserveForStart` re-uses the stored vector when an OnStart start re-points a workspace at a newer template revision with larger resources. The operator re-snapshots and sizes the pod from the new revision, but the reservation keeps the old vector.

The tenant quota is then under-counted. Templates are operator-controlled, so a user cannot choose this, but quota stops being a reliable bound.
Required fix: reserve exact bytes (`TemplateCatalogEntry.Vector()`). When a start changes revision, re-reserve the new vector inside the same transaction under the `tenant_quota` lock, or refuse the move when resources grow beyond headroom.
Security test: `TestStart_RevisionMoveReservesNewVector` in `internal/provisioning/template_family_test.go`.

Ruling for this task: a start that moves revision re-reserves the new revision's exact vector under the `tenant_quota` row lock. A move that does not fit the tenant's headroom refuses the whole start with the ordinary `QuotaExceededError` (API `409 QUOTA_EXHAUSTED`). The transaction rolls back, so the workspace stays on its recorded revision and stays Stopped.

Brief claims re-checked at `98c7f54`: `internal/api/workspaces.go:539-540` and `internal/api/data.go:404-405` (the truncating shifts), `internal/provisioning/service.go:765` (`reserveForStart`) and `internal/provisioning/template_family.go:172` (`revisionIncompatible` ignores CPU and memory) are accurate. `TemplateCatalogEntry.Vector()` is at `internal/provisioning/k8sapplier.go:659`.

**Files:**
- Modify: `internal/api/workspaces.go:71-110` (`TemplateEntry` gains `MemoryBytes`), `:536-541` (create vector)
- Modify: `internal/api/data.go:401-406` (attach vector)
- Modify: `internal/backend/wire.go:1117-1129` (`catalogEntry` sets `MemoryBytes`)
- Modify: `internal/provisioning/template_family.go:86` (`startTemplateTarget` return type), `:138-154` (final return)
- Modify: `internal/provisioning/service.go:529-541` (start path), `:558` (call), `:752-785` (`reserveForStart`); new func `regrowHeld` after `reserveForStart`
- Modify tests: `internal/api/workspaces_handlers_test.go:224-230` (`fakeCatalog.Resolve`), `tests/integration/api_admission_test.go:782-788` (`staticCatalog.Resolve`)
- Test: `internal/provisioning/template_family_test.go` (append), `internal/api/workspaces_handlers_test.go` (append)

**Interfaces:**
- Consumes: `provisioning.TemplateCatalogEntry.Vector() ResourceVector` (`k8sapplier.go:659`), `Reserve`, `reacquireCompute`, `HeldUsage`, `newQuotaExceeded`, `ResourceVector.minus` and `Exceeds` (`quota.go`), and the test helpers `newFakeTemplateLookup`, `(*fakeTemplateLookup).put`, `familyEntry`, `familyWorkspace`, `familyTenant` (`template_family_test.go:30-115`) and `newFakeBackend`, `newWorkspaceEnv`, `defaultTenants`, `login`, `doReq` (`workspaces_handlers_test.go`).
- Produces:
  - `api.TemplateEntry.MemoryBytes int64`: untruncated memory, set from `TemplateCatalogEntry.MemoryBytes`. `StorageBytes` already exists. Every create or attach vector uses `MemoryBytes`/`StorageBytes`, never the MiB/GiB view fields.
  - `type startMove struct { Info TemplateInfo; Vector ResourceVector }` (package-private). `startTemplateTarget` returns `*startMove` as its first value instead of `*TemplateInfo`.
  - `func reserveForStart(ctx context.Context, tx store.Tx, tenantID, workspaceID string, want *ResourceVector) error`: `want` is the moved-to revision's exact vector, or nil when the start keeps its revision.
  - `func regrowHeld(ctx context.Context, tx store.Tx, tenantID, workspaceID string, held, want ResourceVector) error`.

- [ ] **Step 1: Write the failing provisioning test**

Append to `internal/provisioning/template_family_test.go`. Its import block already has `context`, `errors`, `testing`, `provisioning` and `store`.

```go
// TestStart_RevisionMoveReservesNewVector (R1-20): a start that re-points
// the workspace at a larger revision re-reserves that revision's exact
// vector; a move beyond the tenant's headroom refuses the start and leaves
// the workspace and its reservation untouched.
func TestStart_RevisionMoveReservesNewVector(t *testing.T) {
	ctx := context.Background()
	newCatalog := func(nextCPU int64) (*fakeTemplateLookup, provisioning.TemplateCatalogEntry) {
		cat := newFakeTemplateLookup()
		a := familyEntry("linuxdesk", "aaaa1111", "2026-10-a", "OnStart")
		a.CPUMillis, a.MemoryBytes = 500, 1<<30
		cat.put(a)
		b := familyEntry("linuxdesk", "bbbb2222", "2026-10-b", "OnStart")
		b.CPUMillis, b.MemoryBytes, b.DiskBytes = nextCPU, 4<<30+4096, 8<<30+512<<20
		cat.put(b) // newest of the family
		return cat, b
	}
	reservation := func(db *store.DB, ws string) provisioning.ResourceVector {
		t.Helper()
		var v provisioning.ResourceVector
		if err := db.Pool().QueryRow(ctx, `
			SELECT running_slots, cpu_millis, memory_bytes, disk_bytes
			FROM quota_reservation WHERE workspace_id = $1`, ws).
			Scan(&v.RunningSlots, &v.CPUMillis, &v.MemoryBytes, &v.DiskBytes); err != nil {
			t.Fatalf("reservation: %v", err)
		}
		return v
	}

	t.Run("move re-reserves the exact new vector", func(t *testing.T) {
		cat, b := newCatalog(2000)
		db, svc, ws := familyWorkspace(t, cat)
		res, err := svc.SignalWorkspace(ctx, familyTenant, "iss|sub", "", ws, "fam-r120-01", provisioning.IntentStart, []byte("{}"))
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		if res.Template.ID != b.ID {
			t.Fatalf("start did not move: template %s, want %s", res.Template.ID, b.ID)
		}
		if got, want := reservation(db, ws), b.Vector(); got != want {
			t.Fatalf("reservation after the move = %+v, want the new revision's exact vector %+v", got, want)
		}
	})

	t.Run("move beyond headroom refuses the start", func(t *testing.T) {
		cat, _ := newCatalog(9000) // familyWorkspace's tenant allows 8000 millicores
		db, svc, ws := familyWorkspace(t, cat)
		before := reservation(db, ws)
		_, err := svc.SignalWorkspace(ctx, familyTenant, "iss|sub", "", ws, "fam-r120-02", provisioning.IntentStart, []byte("{}"))
		if !provisioning.IsQuotaExceeded(err) {
			t.Fatalf("start onto a revision beyond headroom = %v, want a quota refusal", err)
		}
		if after := reservation(db, ws); after != before {
			t.Fatalf("refused start moved the reservation %+v -> %+v", before, after)
		}
		var tplID, desired string
		if err := db.Pool().QueryRow(ctx, `SELECT template->>'id', desired_state FROM workspaces WHERE id = $1`, ws).
			Scan(&tplID, &desired); err != nil {
			t.Fatal(err)
		}
		if tplID != "tpl_linuxdesk-aaaa1111" || desired != "Stopped" {
			t.Fatalf("refused start left template=%s desired=%s, want the recorded revision and Stopped", tplID, desired)
		}
	})
}
```

The workspace row stores the template snapshot in `workspaces.template` (jsonb); `TemplateInfo.ID` is tagged `json:"id"` (`service.go:157`). The seeded reservation is `quotaVec` = `{1, 500, 1<<30, 1<<32}` (`recovery_test.go:198`). Revision b passes `revisionIncompatible` (`template_family.go:172`): only its CPU, memory and a larger disk differ.

- [ ] **Step 2: Write the failing API test**

Append to `internal/api/workspaces_handlers_test.go`:

```go
// exactCatalog serves one template whose memory and storage are not whole
// MiB/GiB — the reservation must carry the exact bytes (R1-20).
type exactCatalog struct{ fakeCatalog }

func (c *exactCatalog) Resolve(ctx context.Context, tenantID, id string) (TemplateEntry, error) {
	e, err := c.fakeCatalog.Resolve(ctx, tenantID, id)
	if err != nil {
		return e, err
	}
	e.MemoryMiB, e.MemoryBytes = 1536, 1536<<20+4096
	e.StorageGiB, e.StorageBytes = 0, 512<<20
	return e, nil
}

func TestCreateWorkspace_ReservesExactTemplateBytes(t *testing.T) {
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, &exactCatalog{*defaultCatalog()}, defaultTenants())
	sess, csrf := login(t, env, "user-a")
	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
		`{"name":"exact-bytes","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-exact-1000"})
	r.Body.Close()
	if r.StatusCode != http.StatusCreated || len(be.gotCreate) != 1 {
		t.Fatalf("create status=%d creates=%d", r.StatusCode, len(be.gotCreate))
	}
	v := be.gotCreate[0].Vector
	if v.MemoryBytes != 1536<<20+4096 || v.DiskBytes != 512<<20 {
		t.Fatalf("reserved memory=%d disk=%d, want the exact template bytes %d / %d", v.MemoryBytes, v.DiskBytes, 1536<<20+4096, 512<<20)
	}
}
```

- [ ] **Step 3: Run both and see them fail**

Run: `go test ./internal/provisioning/ -run TestStart_RevisionMoveReservesNewVector -count=1 2>&1 | grep -E -- '--- (FAIL|PASS)|reservation after|want a quota' | head`
Expected: `move re-reserves the exact new vector` fails with `reservation after the move = {RunningSlots:1 CPUMillis:500 MemoryBytes:1073741824 DiskBytes:4294967296}, want … {RunningSlots:1 CPUMillis:2000 …}`. `move beyond headroom refuses the start` fails with `want a quota refusal` (the start succeeds today). Docker is required.

Run: `go test ./internal/api/ -run TestCreateWorkspace_ReservesExactTemplateBytes -count=1 2>&1 | tail -3`
Expected: compile failure `e.MemoryBytes undefined (type TemplateEntry has no field or method MemoryBytes)`.

- [ ] **Step 4: Exact bytes on the API path**

`internal/api/workspaces.go`, in `TemplateEntry` (after `MemoryMiB int64`, line 82), add:

```go
	// MemoryBytes is the untruncated memory size; MemoryMiB is the same
	// value rounded down for the public view. Reservations use the bytes.
	MemoryBytes int64
```

Replace the create vector (lines 536-541) with:

```go
	vector := provisioning.ResourceVector{
		RunningSlots: 1,
		CPUMillis:    tpl.CPUMillis,
		MemoryBytes:  tpl.MemoryBytes,
		DiskBytes:    tpl.StorageBytes,
	}
```

`internal/api/data.go:401-406`: in the attach `Vector:` literal, use `MemoryBytes: tpl.MemoryBytes,` and `DiskBytes: tpl.StorageBytes,`.

`internal/backend/wire.go`, `catalogEntry` (line 1127): after `MemoryMiB: e.MemoryBytes >> 20,` add `MemoryBytes: e.MemoryBytes,`.

Test catalogs that feed creates must now carry the bytes:
- `internal/api/workspaces_handlers_test.go:227`: make the line `CPUMillis: 2000, MemoryMiB: 4096, MemoryBytes: 4096 << 20, StorageGiB: 20, StorageBytes: 20 << 30,`.
- `tests/integration/api_admission_test.go:785`: make it `CPUMillis: 500, MemoryMiB: 1024, MemoryBytes: 1024 << 20, StorageGiB: 1, StorageBytes: 1 << 30,`.

Then check that no other create path still reads the view fields: `rg -n 'MemoryMiB *<<|StorageGiB *<<' internal tests --glob '*.go'` should print nothing.

- [ ] **Step 5: A revision move re-reserves**

`internal/provisioning/template_family.go`: above `startTemplateTarget`, add:

```go
// startMove is the revision a start moves to: the row snapshot to record
// and the exact quota vector the new revision runs with.
type startMove struct {
	Info   TemplateInfo
	Vector ResourceVector
}
```

Change the signature (line 86) to return `(*startMove, string, string, string, bool, error)`. Every early `return nil, …` stays as it is. Rewrite the final return (lines 138-154) so the literal is wrapped:

```go
	return &startMove{
		Info: TemplateInfo{
			ID:            newest.ID,
			Name:          newest.Name,
			Revision:      newest.Revision,
			RevisionLabel: newest.RevisionLabel,
			Runtime:       newest.Runtime,
			Experience:    newest.Experience,
			ImageBuiltAt:  newest.ImageBuiltAt,
			// The guard already refuses a move whose clipboard or
			// file-transfer policy differs (SkipReasonClipboardPolicyChanged,
			// SkipReasonFileTransferPolicyChanged), so the snapshot here
			// equals the recorded revision's — carried explicitly since the
			// row stores this struct.
			ClipboardPolicy:    newest.ClipboardPolicy,
			FileTransferPolicy: newest.FileTransferPolicy,
		},
		Vector: newest.Vector(),
	}, name, "", newest.ImageBuiltAt, false, nil
```

Keep every field the current literal sets. If the HEAD literal carries more fields than the list above, keep them inside `Info`.

`internal/provisioning/service.go`, start path (lines 518-541): declare `var want *ResourceVector` next to `var pinned bool`. In the `if next != nil {` block, write:

```go
					if next != nil {
						rec.Template = next.Info
						want = &next.Vector
						sigSpec = &IntentSpec{
							TemplateName: objName,
							ImageBuiltAt: next.Info.ImageBuiltAt,
						}
					} else {
```

At line 558, make the call `reserveForStart(ctx, tx, tenantID, wsID, want)`. Extend the comment above it with: "A start that moved revision re-reserves the new revision's exact vector here, so quota never counts the old revision's resources for the new pod; a move that does not fit refuses the start."

Replace `reserveForStart` (doc comment and body, lines 752-785) with:

```go
// reserveForStart re-acquires the running-quota reservation a stopped
// workspace gave up, sized for the revision the start runs on. want is the
// moved-to revision's exact vector, or nil when the start keeps its
// revision. Three shapes exist:
//   - released row (Ephemeral): re-held through Reserve with want, or with
//     the vector the original admission granted when want is nil;
//   - disk-only hold (stopped Retain, see convertToDiskOnly): the admitted
//     compute sits in restart_*; it (or want's compute) is added back to the
//     same row and the stored vector cleared. The disk stays as held — it is
//     the existing volume;
//   - still-held row (the stop's absence proof has not landed yet): kept as
//     is, or regrown to want under the tenant_quota lock.
//
// A workspace with no reservation row at all cannot have passed admission —
// that is an internal inconsistency, not a grant of zero quota.
func reserveForStart(ctx context.Context, tx store.Tx, tenantID, workspaceID string, want *ResourceVector) error {
	var v ResourceVector
	var state string
	var rs, rc, rm *int64
	err := tx.QueryRow(ctx, `
		SELECT running_slots, cpu_millis, memory_bytes, disk_bytes, state,
		       restart_slots, restart_cpu_millis, restart_memory_bytes
		FROM quota_reservation WHERE workspace_id = $1`, workspaceID).
		Scan(&v.RunningSlots, &v.CPUMillis, &v.MemoryBytes, &v.DiskBytes, &state, &rs, &rc, &rm)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("start: workspace %s has no reservation row to re-acquire", workspaceID)
	}
	if err != nil {
		return fmt.Errorf("start: read reservation %w", err)
	}
	switch {
	case state == "held" && rs != nil && rc != nil && rm != nil:
		compute := ResourceVector{RunningSlots: *rs, CPUMillis: *rc, MemoryBytes: *rm}
		if want != nil {
			compute = ResourceVector{RunningSlots: want.RunningSlots, CPUMillis: want.CPUMillis, MemoryBytes: want.MemoryBytes}
		}
		return reacquireCompute(ctx, tx, tenantID, workspaceID, compute)
	case state == "held" && want != nil && *want != v:
		return regrowHeld(ctx, tx, tenantID, workspaceID, v, *want)
	case state != "held" && want != nil:
		return Reserve(ctx, tx, tenantID, workspaceID, *want)
	}
	return Reserve(ctx, tx, tenantID, workspaceID, v)
}

// regrowHeld replaces a still-held reservation's vector with want: a start
// moved the workspace to a revision with other resources before the stop's
// release landed. It serializes on the tenant_quota row like Reserve and
// judges want against the tenant's usage without this reservation. The disk
// never shrinks: the larger of the held and wanted bytes is kept.
func regrowHeld(ctx context.Context, tx store.Tx, tenantID, workspaceID string, held, want ResourceVector) error {
	var limit ResourceVector
	err := tx.QueryRow(ctx, `
		SELECT max_running_slots, max_cpu_millis, max_memory_bytes, max_disk_bytes
		FROM tenant_quota WHERE tenant_id = $1 FOR UPDATE`, tenantID).
		Scan(&limit.RunningSlots, &limit.CPUMillis, &limit.MemoryBytes, &limit.DiskBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoQuota
	}
	if err != nil {
		return fmt.Errorf("regrow: lock quota %w", err)
	}
	if held.DiskBytes > want.DiskBytes {
		want.DiskBytes = held.DiskBytes
	}
	used, err := HeldUsage(ctx, tx, tenantID)
	if err != nil {
		return fmt.Errorf("regrow: usage %w", err)
	}
	base := used.minus(held)
	if dim := want.Exceeds(base, limit); dim != "" {
		return newQuotaExceeded(tenantID, dim, want, base, limit)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE quota_reservation
		SET running_slots = $3, cpu_millis = $4, memory_bytes = $5, disk_bytes = $6
		WHERE workspace_id = $1 AND tenant_id = $2 AND state = 'held'`,
		workspaceID, tenantID, want.RunningSlots, want.CPUMillis, want.MemoryBytes, want.DiskBytes); err != nil {
		return fmt.Errorf("regrow: %w", err)
	}
	return nil
}
```

`service.go` already imports `errors`, `fmt` and `pgx`. `ErrNoQuota` is the sentinel `Reserve` returns (`quota.go`). Run `go build ./internal/provisioning/ ./internal/api/ ./internal/backend/` and expect no output.

- [ ] **Step 6: Run the focused tests**

Run: `go test ./internal/provisioning/ -run 'TestStart_RevisionMove|TestTemplateFamily|TestFamily|TestStart|TestRestart|TestRetainStop|TestSettleQuota' -count=1 2>&1 | tail -3 && go test ./internal/api/ -run 'TestCreateWorkspace|TestTemplateUpdate|TestData' -count=1 2>&1 | tail -3`
Expected: both `ok`.

Run: `go test ./internal/provisioning/ ./internal/api/ ./internal/backend/ -count=1 2>&1 | tail -4`
Expected: three `ok` lines.

- [ ] **Step 7: Integration and gates**

Run: `KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path --bin-dir "$PWD/bin")" go test -tags=integration -count=1 -run 'Quota|Admission|Template|Attach|Retain' ./tests/integration/ 2>&1 | tail -3 && for t in .github/tests/*.test.sh; do bash "$t" >/dev/null || echo "FAIL $t"; done`
Expected: `ok`, no `FAIL` line.

- [ ] **Step 8: Commit**

```bash
git add internal/api/workspaces.go internal/api/data.go internal/backend/wire.go internal/provisioning/template_family.go internal/provisioning/service.go internal/provisioning/template_family_test.go internal/api/workspaces_handlers_test.go tests/integration/api_admission_test.go
git commit -m "fix(quota): reserve exact template bytes and re-reserve on a revision move" -m "R1-20: create and attach reserve TemplateCatalogEntry bytes, not truncated MiB/GiB; a start that re-points a workspace at a new revision re-reserves that revision's vector under the tenant_quota lock and refuses the start when it does not fit. Pinned by TestStart_RevisionMoveReservesNewVector and TestCreateWorkspace_ReservesExactTemplateBytes." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 3: Operator-declared tenant quota ceilings (R1-02) — tier: design

**Finding (R1-02, final severity P2, new B2 surface; understates TM-44).** A tenant admin of a tenant whose `managedNamespaces[]` entry has no `quota` block (the schema allows this; NOTES only warns) sends `PUT /v1/admin/tenants/<own>/quota` with `If-Match: *` and `{"runningWorkspaces":9223372036854775807,"cpuMillicores":9223372036854775807,...}`. The fail-closed `QUOTA_NOT_CONFIGURED` default becomes an effectively unlimited quota. `namespaceQuota` is off by default, and TM-44 says "only the API-layer tenant quota bounds workspaces". That bound is now writable by the tenant admin, so the tenant can consume the shared cluster's capacity and starve other tenants. The docs present omitting the block as "fail closed until an administrator declares a quota" without saying that a tenant admin can lift it without limit.
Required fix: the operator must own the upper bound. Add an operator-declared ceiling per tenant (`-tenant-quota-ceilings`) and refuse a tenant-admin PUT above it. With no declared ceiling, refuse tenant-admin quota writes. Document it in the threat model (TM-44 residual) and the chart README, and have NOTES recommend `namespaceQuota`.
Security tests: `TestAdminQuota_PutAboveOperatorCeilingRefused` and `TestAdminQuota_UndeclaredTenantCreateRefused` in `internal/api/adminquota_test.go`. A tenant-admin PUT above the ceiling, or with no ceiling, must not answer 200 and must not write the row. Review Focus 4 adds `TestAdminQuota_PutAtCeilingAccepted`: a write exactly at the ceiling, and one that lowers the quota below usage, both answer 200.

Decision D3 is binding:
- Chart key `managedNamespaces[].quotaCeiling` has the same four keys as `quota`.
- It renders as the backend flag `-tenant-quota-ceilings` (same JSON shape as `-tenant-quotas`; env `TCDI_TENANT_QUOTA_CEILINGS`).
- A tenant-admin PUT above the ceiling is refused, and so is any tenant-admin quota write when no ceiling is declared.
- A tenant with a `quota` block stays read-only (`QUOTA_MANAGED_BY_CONFIG`).
- An entry may carry `quota` or `quotaCeiling`, not both: the schema refuses both on one entry, and the backend refuses a tenant listed in both flags.

The refusal is a new code, `409 QUOTA_ABOVE_CEILING`, which is not retryable. The message names the request field that is over the ceiling, or says that no ceiling is declared.

Brief claims re-checked at `98c7f54`: `internal/api/adminquota.go:304-352` (`Put`), `:128-160` (`toVector`, bounds `MaxInt64`), `:309` (only config-managed tenants refused), `deploy/helm/tinycdi/values.schema.json:50-62` (`quota` optional per entry) and `docs/security/threat-model.md:723` (TM-44) are accurate.

**Files:**
- Modify: `internal/api/adminquota.go:162-199` (view type, handler field, `WithQuotaCeilings`), `:242-261` (`view`), `:296-352` (`Put` doc and body)
- Modify: `internal/api/errors.go:32`, `:60`, `:81` (new code)
- Modify: `internal/api/openapi.yaml` (~869-881 PUT description, ~2012/2033 ErrorCode table and enum, ~2490-2515 `AdminQuotaView`)
- Modify: `internal/backend/config.go:117`, `:292-293`, `:425-427`
- Modify: `internal/backend/wire.go:742-749`
- Modify: `deploy/helm/tinycdi/templates/_helpers.tpl:74-86` (new define after it), `:431` (extraEnv deny list)
- Modify: `deploy/helm/tinycdi/templates/backend.yaml:146-151`
- Modify: `deploy/helm/tinycdi/values.schema.json:43-62` (items), `:540` (`$defs`)
- Modify: `deploy/helm/tinycdi/values.yaml:108-141`, `deploy/helm/tinycdi/templates/NOTES.txt:33-42`
- Modify: `web/src/api/errors.ts:72`, `web/src/workspaces/ErrorBanner.tsx:15`, `web/src/admin/ApiErrorAlert.tsx:16`, `web/src/i18n/en/admin.ts:376`, `web/src/i18n/vi/admin.ts:376`, `web/tests/unit/api/errors.test.ts:32`, then regenerate `web/src/api/generated/schema.d.ts`
- Modify docs: `deploy/helm/tinycdi/README.md:97`, `:182`, `:754-783`; `docs/runbooks/install.md:262-312`; `docs/runbooks/tenant-quotas.md:7-50`, `:133-140`; `docs/runbooks/observability.md:200`; `docs/adr/0002-api-conventions.md:98`; `docs/security/threat-model.md:210-219`, `:723` (Residual cell only)
- Test: `internal/api/adminquota_test.go`, `internal/api/errors_test.go:118`, `internal/api/tenantadmin_test.go:82`, `internal/api/auditroutes_test.go:244`, `internal/backend/tenantquota_test.go`, `deploy/helm/chart_tenantquota_test.go`, `deploy/helm/chart_extraenv_test.go:42`, `:99`

**Interfaces:**
- Consumes: `provisioning.ParseTenantQuotas` (`tenantquota.go:63`, `[]TenantQuota{TenantID, Limits}`), `ResourceVector.Exceeds(used, limit) string` (`quota.go:47`, returns `runningSlots`/`cpuMillis`/`memoryBytes`/`diskBytes`), and the test helpers `newAdminQuotaEnv`, `loginAdmin`, `doReq`, `decodeErr`, `decodeBody`, `requireErr`, `requireValid`, `adminQuotaPath`, `adminQuotaBody`, `IfMatchCreate`, `fakeAdminQuotaSource`, `quotaValues`, `renderArgs`, `renderErrArgs`, `renderNotes`, `deployment`, `firstContainerArgs`, `mergedArgs`, `noEnv`, `envMap`, `goodTenantQuotas`.
- Produces:
  - Backend flag `-tenant-quota-ceilings` / env `TCDI_TENANT_QUOTA_CEILINGS` (`Config.TenantQuotaCeilings string`), with the same JSON as `-tenant-quotas`. The chart renders it from `managedNamespaces[].quotaCeiling`, so it is chart-managed: `TCDI_TENANT_QUOTA_CEILINGS` joins the extraEnv deny list, and any extraArgs deny list (Task 24) must treat `-tenant-quota-ceilings` as rendered.
  - `api.CodeQuotaAboveCeiling ErrorCode = "QUOTA_ABOVE_CEILING"`: 409, not retryable.
  - `func (h *AdminQuotaHandler) WithQuotaCeilings(c map[string]provisioning.ResourceVector) *AdminQuotaHandler`.
  - `AdminQuotaView.ceiling` (optional, schema `AdminQuotaLimits`): the declared ceiling in display units.
  - Chart define `tinycdi.tenantQuotaCeilings`, chart schema `$defs/tenantQuota`.

- [ ] **Step 1: Write the failing API tests**

In `internal/api/adminquota_test.go`, add `"math"` to the import block. After `adminQuotaBody` (line 134), add:

```go
// testQuotaCeilings is the operator ceiling newAdminQuotaEnv declares for
// tenant-a: far above every body the other tests send, so only the
// ceiling tests see it bite.
var testQuotaCeilings = map[string]provisioning.ResourceVector{
	"tenant-a": {RunningSlots: 1000, CPUMillis: 1_000_000, MemoryBytes: 1 << 40, DiskBytes: 1 << 50},
}

// withCeilings replaces the default ceilings (nil = none declared).
func withCeilings(c map[string]provisioning.ResourceVector) func(*AdminQuotaHandler) {
	return func(h *AdminQuotaHandler) { h.WithQuotaCeilings(c) }
}

// ceilingAtBody is the operator ceiling that equals adminQuotaBody.
var ceilingAtBody = map[string]provisioning.ResourceVector{
	"tenant-a": {RunningSlots: 4, CPUMillis: 8000, MemoryBytes: 16384 << 20, DiskBytes: 200 << 30},
}
```

In `newAdminQuotaEnv` (line 112), make the constructor line:

```go
	h := NewAdminQuotaHandler(src, defaultTenants(), managed).WithQuotaCeilings(testQuotaCeilings)
```

In `adminQuotaViewJSON` (line 125), add the field `Ceiling *quotaAmountsJSON `json:"ceiling"``.

Append:

```go
// TestAdminQuota_PutAboveOperatorCeilingRefused (R1-02): a tenant admin
// cannot raise the tenant quota past the operator-declared ceiling — one
// dimension above it is 409 QUOTA_ABOVE_CEILING and nothing is stored.
func TestAdminQuota_PutAboveOperatorCeilingRefused(t *testing.T) {
	for name, body := range map[string]string{
		"running": `{"runningWorkspaces":5,"cpuMillicores":8000,"memoryMib":16384,"storageGib":200}`,
		"cpu":     `{"runningWorkspaces":4,"cpuMillicores":8001,"memoryMib":16384,"storageGib":200}`,
		"memory":  `{"runningWorkspaces":4,"cpuMillicores":8000,"memoryMib":16385,"storageGib":200}`,
		"storage": `{"runningWorkspaces":4,"cpuMillicores":8000,"memoryMib":16384,"storageGib":201}`,
		"maxint": fmt.Sprintf(`{"runningWorkspaces":%d,"cpuMillicores":%d,"memoryMib":%d,"storageGib":%d}`,
			int64(math.MaxInt64), int64(math.MaxInt64), int64(math.MaxInt64>>20), int64(math.MaxInt64>>30)),
	} {
		t.Run(name, func(t *testing.T) {
			src := &fakeAdminQuotaSource{}
			env := newAdminQuotaEnv(t, src, nil, withCeilings(ceilingAtBody))
			sess, csrf := loginAdmin(t, env, "admin-a")
			r := doReq(t, env, sess, csrf, http.MethodPut, adminQuotaPath, body,
				map[string]string{"If-Match": IfMatchCreate})
			requireErr(t, r, http.StatusConflict, CodeQuotaAboveCeiling, name+" above the ceiling")
			if src.setCall != 0 {
				t.Fatalf("a PUT above the ceiling reached SetLimits %d times", src.setCall)
			}
		})
	}
}

// TestAdminQuota_UndeclaredTenantCreateRefused (R1-02): with no ceiling
// declared for the tenant, a tenant admin can neither create the quota row
// (the fail-closed QUOTA_NOT_CONFIGURED default stays) nor change an
// existing API-managed row.
func TestAdminQuota_UndeclaredTenantCreateRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		src     *fakeAdminQuotaSource
		ifMatch func(*fakeAdminQuotaSource) string
	}{
		"create": {&fakeAdminQuotaSource{}, func(*fakeAdminQuotaSource) string { return IfMatchCreate }},
		"update": {&fakeAdminQuotaSource{hasRow: true, limits: store.QuotaAmounts{RunningSlots: 1}},
			func(s *fakeAdminQuotaSource) string { return ETagOf(s.currentVersion()) }},
	} {
		t.Run(name, func(t *testing.T) {
			env := newAdminQuotaEnv(t, tc.src, nil, withCeilings(nil))
			sess, csrf := loginAdmin(t, env, "admin-a")
			r := doReq(t, env, sess, csrf, http.MethodPut, adminQuotaPath, adminQuotaBody,
				map[string]string{"If-Match": tc.ifMatch(tc.src)})
			e := decodeErr(t, r)
			if r.StatusCode != http.StatusConflict || e.Code != CodeQuotaAboveCeiling {
				t.Fatalf("status=%d code=%q, want 409 QUOTA_ABOVE_CEILING", r.StatusCode, e.Code)
			}
			if e.Retryable {
				t.Fatal("QUOTA_ABOVE_CEILING must not be retryable")
			}
			if tc.src.setCall != 0 {
				t.Fatalf("a PUT without a declared ceiling reached SetLimits %d times", tc.src.setCall)
			}
		})
	}
}

// TestAdminQuota_PutAtCeilingAccepted (R1-02): the ceiling is inclusive — a
// write exactly at it is stored — and lowering the quota below current
// usage under a ceiling is still accepted. The view reports the ceiling.
func TestAdminQuota_PutAtCeilingAccepted(t *testing.T) {
	src := &fakeAdminQuotaSource{}
	env := newAdminQuotaEnv(t, src, nil, withCeilings(ceilingAtBody))
	sess, csrf := loginAdmin(t, env, "admin-a")

	r := doReq(t, env, sess, csrf, http.MethodPut, adminQuotaPath, adminQuotaBody,
		map[string]string{"If-Match": IfMatchCreate})
	v := decodeBody[adminQuotaViewJSON](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("PUT at the ceiling: status=%d, want 200", r.StatusCode)
	}
	if v.Ceiling == nil || v.Ceiling.RunningWorkspaces != 4 || v.Ceiling.CPUMillicores != 8000 ||
		v.Ceiling.MemoryMib != 16384 || v.Ceiling.StorageGib != 200 {
		t.Fatalf("view ceiling = %+v, want the declared ceiling in display units", v.Ceiling)
	}

	src.mu.Lock()
	src.usage = store.QuotaAmounts{RunningSlots: 3, CPUMillis: 6000, MemoryBytes: 12 << 30, DiskBytes: 150 << 30}
	src.mu.Unlock()
	r = doReq(t, env, sess, csrf, http.MethodPut, adminQuotaPath,
		`{"runningWorkspaces":1,"cpuMillicores":1000,"memoryMib":1024,"storageGib":10}`,
		map[string]string{"If-Match": ETagOf(src.currentVersion())})
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("lowering below usage under a ceiling: status=%d, want 200", r.StatusCode)
	}
	if src.setCall != 2 {
		t.Fatalf("SetLimits calls = %d, want 2", src.setCall)
	}
}
```

In `TestAdminQuota_Contract` (line 543), append a third case:

```go
	requireValid(t, "AdminQuotaView", adminQuotaView{
		Tenant: "acme", Configured: true, Source: QuotaSourceAPI,
		Limits: &limits, Usage: usage,
		Ceiling: &quotaCeilingView{RunningWorkspaces: 8, CPUMillicores: 32000, MemoryMib: 65536, StorageGib: 400},
	})
```

In `internal/api/errors_test.go:118`, add `CodeQuotaAboveCeiling: http.StatusConflict,` after the `CodeQuotaManagedByConfig` entry.

The other two handlers built outside `newAdminQuotaEnv` must declare a ceiling, or their quota PUTs start failing:
- `internal/api/tenantadmin_test.go:82`: `NewAdminQuotaHandler(fx.aqs, defaultTenants(), nil).WithQuotaCeilings(testQuotaCeilings)`.
- `internal/api/auditroutes_test.go:244`: `NewAdminQuotaHandler(&fakeAdminQuotaSource{}, tenants, nil).WithQuotaCeilings(testQuotaCeilings).WithAuditSink(sink)`.

- [ ] **Step 2: Write the failing backend and chart tests**

Append to `internal/backend/tenantquota_test.go`:

```go
// TestParseFlags_TenantQuotaCeilings (R1-02): -tenant-quota-ceilings /
// TCDI_TENANT_QUOTA_CEILINGS parse like -tenant-quotas; a malformed value,
// or a tenant listed in both flags, fails startup naming the flag.
func TestParseFlags_TenantQuotaCeilings(t *testing.T) {
	c, err := ParseFlags(append(mergedArgs(), "-tenant-quota-ceilings", goodTenantQuotas), noEnv)
	if err != nil || c.TenantQuotaCeilings != goodTenantQuotas {
		t.Fatalf("flag: TenantQuotaCeilings=%q err=%v", c.TenantQuotaCeilings, err)
	}
	c, err = ParseFlags(mergedArgs(), envMap(map[string]string{"TCDI_TENANT_QUOTA_CEILINGS": goodTenantQuotas}))
	if err != nil || c.TenantQuotaCeilings != goodTenantQuotas {
		t.Fatalf("env: TenantQuotaCeilings=%q err=%v", c.TenantQuotaCeilings, err)
	}
	_, err = ParseFlags(append(mergedArgs(), "-tenant-quota-ceilings", `[{"tenant":"tenant-a","runningWorkspaces":1}]`), noEnv)
	if err == nil || !strings.Contains(err.Error(), "-tenant-quota-ceilings") {
		t.Fatalf("malformed: err=%v, want an error naming -tenant-quota-ceilings", err)
	}
	_, err = ParseFlags(append(mergedArgs(), "-tenant-quotas", goodTenantQuotas, "-tenant-quota-ceilings", goodTenantQuotas), noEnv)
	if err == nil || !strings.Contains(err.Error(), "-tenant-quota-ceilings") || !strings.Contains(err.Error(), "tenant-a") {
		t.Fatalf("tenant in both flags: err=%v, want an error naming the flag and tenant-a", err)
	}
}
```

Append to `deploy/helm/chart_tenantquota_test.go`:

```go
func tenantQuotaCeilingsArg(t *testing.T, valuesFile string) (string, bool) {
	t.Helper()
	dep := deployment(renderArgs(t, "-f", valuesFile), "backend")
	if dep == nil {
		t.Fatal("no backend Deployment rendered")
	}
	for _, a := range firstContainerArgs(dep) {
		if v, ok := strings.CutPrefix(a, "-tenant-quota-ceilings="); ok {
			return v, true
		}
	}
	return "", false
}

// TestTenantQuotaCeilingsRenderFlag (R1-02): quotaCeiling entries render
// -tenant-quota-ceilings; quota entries stay in -tenant-quotas only.
func TestTenantQuotaCeilingsRenderFlag(t *testing.T) {
	vf := quotaValues(t, `
  - name: tcdi-a
    tenant: tenant-a
    quotaCeiling: {runningWorkspaces: 20, cpu: "32", memory: 128Gi, storage: 1Ti}
  - name: tcdi-b
    tenant: tenant-b
    quota: {runningWorkspaces: 2, cpu: "2", memory: 4Gi, storage: 20Gi}
`)
	raw, ok := tenantQuotaCeilingsArg(t, vf)
	if !ok {
		t.Fatal("backend args carry no -tenant-quota-ceilings")
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("-tenant-quota-ceilings is not JSON: %v\n%s", err, raw)
	}
	want := map[string]any{"tenant": "tenant-a", "runningWorkspaces": 20.0, "cpu": "32", "memory": "128Gi", "storage": "1Ti"}
	if len(got) != 1 || len(got[0]) != len(want) {
		t.Fatalf("-tenant-quota-ceilings = %s, want exactly tenant-a's ceiling", raw)
	}
	for k, v := range want {
		if got[0][k] != v {
			t.Errorf("ceiling %s = %v (%T), want %v", k, got[0][k], got[0][k], v)
		}
	}
	if q, ok := tenantQuotasArg(t, vf); !ok || strings.Contains(q, "tenant-a") {
		t.Errorf("-tenant-quotas = %q ok=%v, want tenant-b only", q, ok)
	}
}

func TestTenantQuotaCeilingsFlagAbsentByDefault(t *testing.T) {
	if v, ok := tenantQuotaCeilingsArg(t, filepath.Join("tinycdi", "ci", "minimal-values.yaml")); ok {
		t.Fatalf("minimal values rendered -tenant-quota-ceilings=%s", v)
	}
}

// TestSchemaRejectsBadQuotaCeilings (R1-02): quotaCeiling takes the quota
// block's validation, and one entry cannot carry both blocks.
func TestSchemaRejectsBadQuotaCeilings(t *testing.T) {
	for name, entry := range map[string]string{
		"unparsable cpu": "\n  - name: tcdi-a\n    tenant: tenant-a\n    quotaCeiling: {runningWorkspaces: 1, cpu: lots, memory: 1Gi, storage: 1Gi}\n",
		"missing field":  "\n  - name: tcdi-a\n    tenant: tenant-a\n    quotaCeiling: {runningWorkspaces: 1, cpu: \"1\", memory: 1Gi}\n",
		"both blocks": "\n  - name: tcdi-a\n    tenant: tenant-a\n    quota: {runningWorkspaces: 1, cpu: \"1\", memory: 1Gi, storage: 1Gi}\n" +
			"    quotaCeiling: {runningWorkspaces: 2, cpu: \"2\", memory: 2Gi, storage: 2Gi}\n",
	} {
		out := renderErrArgs(t, "-f", quotaValues(t, entry))
		if !strings.Contains(out, "managedNamespaces") {
			t.Errorf("%s: render failed but the error does not point at managedNamespaces:\n%s", name, out)
		}
	}
}
```

In `TestTenantQuotasNotesWarnForTenantsWithoutQuota` (line 108), after the existing `QUOTA_NOT_CONFIGURED` check, add:

```go
	if !strings.Contains(notes, "quotaCeiling") || !strings.Contains(notes, "namespaceQuota") {
		t.Errorf("NOTES must point at quotaCeiling and recommend namespaceQuota, got:\n%s", notes)
	}
```

In `deploy/helm/chart_extraenv_test.go`, add `"TCDI_TENANT_QUOTA_CEILINGS",` after `"TCDI_TENANT_QUOTAS",` (line 42), and add `{"backend", "TCDI_TENANT_QUOTA_CEILINGS", "managedNamespaces"},` after the `TCDI_TENANT_QUOTAS` case (line 99).

- [ ] **Step 3: Run them and see them fail**

Run: `go test ./internal/api/ -run 'TestAdminQuota_' -count=1 2>&1 | tail -3`
Expected: compile failure, `undefined: CodeQuotaAboveCeiling` (and `WithQuotaCeilings`, `quotaCeilingView`).

Run: `go test ./internal/backend/ -run TestParseFlags_TenantQuotaCeilings -count=1 2>&1 | tail -3`
Expected: compile failure, `c.TenantQuotaCeilings undefined`.

Run: `go test ./deploy/helm/ -run 'TestTenantQuotaCeilings|TestSchemaRejectsBadQuotaCeilings|TestTenantQuotasNotesWarn|ExtraEnv' -count=1 2>&1 | grep -E -- '--- FAIL' | head`
Expected: `TestTenantQuotaCeilingsRenderFlag` (the schema refuses the unknown key `quotaCeiling`), `TestTenantQuotasNotesWarnForTenantsWithoutQuota` (no `quotaCeiling`/`namespaceQuota` in NOTES) and the extraEnv case for `TCDI_TENANT_QUOTA_CEILINGS` fail. `TestSchemaRejectsBadQuotaCeilings` already passes, because the unknown key is refused today; after the fix it guards the shared quota schema and the one-block rule.

- [ ] **Step 4: The error code**

`internal/api/errors.go`:
- After line 32, add `CodeQuotaAboveCeiling    ErrorCode = "QUOTA_ABOVE_CEILING"     // 409`.
- In `AllErrorCodes`, add `CodeQuotaAboveCeiling,` after `CodeQuotaManagedByConfig,`.
- In `HTTPStatus`, add `CodeQuotaAboveCeiling` to the 409 case list after `CodeQuotaManagedByConfig`.

Run `gofmt -w internal/api/errors.go`. `Retryable` stays false for it, because it is not in the retryable list.

- [ ] **Step 5: The handler**

`internal/api/adminquota.go`. After `adminQuotaView`'s `Usage` field, add:

```go
	// Ceiling is the operator-declared upper bound on tenant-admin writes
	// (-tenant-quota-ceilings); absent when none is declared, and then the
	// quota cannot be written through this API at all.
	Ceiling *quotaCeilingView `json:"ceiling,omitempty"`
```

Below the `adminQuotaView` type, add:

```go
// quotaCeilingView is a declared ceiling in display units (openapi
// AdminQuotaLimits); byte amounts round down like every quota view.
type quotaCeilingView struct {
	RunningWorkspaces int64 `json:"runningWorkspaces"`
	CPUMillicores     int64 `json:"cpuMillicores"`
	MemoryMib         int64 `json:"memoryMib"`
	StorageGib        int64 `json:"storageGib"`
}

// ceilingFields names, per ResourceVector.Exceeds dimension, the request
// field a QUOTA_ABOVE_CEILING refusal points at.
var ceilingFields = map[string]string{
	"runningSlots": "runningWorkspaces",
	"cpuMillis":    "cpuMillicores",
	"memoryBytes":  "memoryMib",
	"diskBytes":    "storageGib",
}
```

Extend the `AdminQuotaHandler` doc comment with: "ceilings holds the operator-declared upper bounds from -tenant-quota-ceilings: a write above its tenant's ceiling, or for a tenant without one, is refused with QUOTA_ABOVE_CEILING." Add the field `ceilings map[string]provisioning.ResourceVector` after `managed`. After `WithAuditSink`, add:

```go
// WithQuotaCeilings sets the operator-declared per-tenant ceilings. A nil
// or empty map declares none, so every tenant-admin quota write is refused.
func (h *AdminQuotaHandler) WithQuotaCeilings(c map[string]provisioning.ResourceVector) *AdminQuotaHandler {
	h.ceilings = c
	return h
}
```

In `view`, before `return out`, add:

```go
	if c, ok := h.ceilings[tenantID]; ok {
		out.Ceiling = &quotaCeilingView{
			RunningWorkspaces: c.RunningSlots,
			CPUMillicores:     c.CPUMillis,
			MemoryMib:         c.MemoryBytes >> 20,
			StorageGib:        c.DiskBytes >> 30,
		}
	}
```

In `Put`, after the four `auditSetDetail` lines and before `cur, err := h.source.Report(...)`, add:

```go
	// The operator owns the upper bound: a tenant admin may write any
	// quota up to the declared ceiling (inclusive) and nothing without one,
	// so the fail-closed QUOTA_NOT_CONFIGURED default cannot be lifted
	// without bound.
	ceiling, ok := h.ceilings[tenant]
	if !ok {
		writeError(w, r, CodeQuotaAboveCeiling,
			"no quota ceiling is declared for this tenant; the platform operator must declare one before the quota can be set here")
		return
	}
	if dim := v.Exceeds(provisioning.ResourceVector{}, ceiling); dim != "" {
		writeError(w, r, CodeQuotaAboveCeiling,
			ceilingFields[dim]+" is above the quota ceiling the platform operator declared for this tenant")
		return
	}
```

Replace the `Put` doc comment's first sentence with: "Put handles PUT /v1/admin/tenants/{tenant}/quota: it replaces the tenant's limits, provided the tenant is not declared in -tenant-quotas (config-managed rows answer 409 QUOTA_MANAGED_BY_CONFIG), the limits stay within the tenant's -tenant-quota-ceilings entry (409 QUOTA_ABOVE_CEILING above it or without one) and If-Match names the current ETag …". Keep the rest of the comment as it is.

Run: `go test ./internal/api/ -run 'TestAdminQuota_|TestErrorCode|TestTenantAdmin|TestAudit_' -count=1 2>&1 | tail -3`
Expected: `TestErrorCodes…` (the openapi enum check in `errors_test.go`) and `TestAdminQuota_Contract` (its new `ceiling` case; `AdminQuotaView` is `additionalProperties: false` until Step 6) still fail until Step 6; every other `TestAdminQuota_*` passes.

- [ ] **Step 6: OpenAPI and portal**

`internal/api/openapi.yaml`:
- In the `putAdminTenantQuota` description, after "must be changed through the platform configuration.", insert: "For any other tenant the limits must stay within the ceiling the platform operator declared for it (`managedNamespaces[].quotaCeiling`, backend flag `-tenant-quota-ceilings`); a value above it, or any write for a tenant without a declared ceiling, answers `409 QUOTA_ABOVE_CEILING`."
- In the ErrorCode description table, after the `QUOTA_MANAGED_BY_CONFIG` row, add: ``| `QUOTA_ABOVE_CEILING` | 409 | false | a tenant-admin quota write exceeds the ceiling the platform operator declared for the tenant (`-tenant-quota-ceilings`), or no ceiling is declared so the quota cannot be set through the API; the message names the field |``.
- In the `ErrorCode` enum, add `- QUOTA_ABOVE_CEILING` after `- QUOTA_MANAGED_BY_CONFIG`.
- In `AdminQuotaView.properties`, after `usage`, add:

```yaml
        ceiling:
          $ref: "#/components/schemas/AdminQuotaLimits"
```

  Append to the `AdminQuotaView` description: "`ceiling` is the operator-declared upper bound on writes through this API, in display units (rounded down); it is absent when none is declared, and then `PUT` answers `QUOTA_ABOVE_CEILING`."

Web (`cd web`):
- `src/api/errors.ts` `CODE_TABLE`: add `QUOTA_ABOVE_CEILING: true,` after `QUOTA_MANAGED_BY_CONFIG: true,`.
- `tests/unit/api/errors.test.ts`: the same line after `QUOTA_MANAGED_BY_CONFIG: true,`.
- `src/workspaces/ErrorBanner.tsx` `GUIDANCE_KEYS`: `QUOTA_ABOVE_CEILING: "errors.code.generic",`.
- `src/admin/ApiErrorAlert.tsx` `GUIDANCE`: `QUOTA_ABOVE_CEILING: "admin.errors.quotaAboveCeiling",`.
- `src/i18n/en/admin.ts`, after the `quotaManagedByConfig` entry:

```ts
  "admin.errors.quotaAboveCeiling":
    "These limits are above the ceiling the platform operator set for this tenant, or no ceiling is set. Choose lower limits or ask the platform operator.",
```

- `src/i18n/vi/admin.ts`, same position:

```ts
  "admin.errors.quotaAboveCeiling":
    "Các giới hạn này vượt mức trần mà nhà vận hành nền tảng đặt cho tenant, hoặc chưa có mức trần nào. Hãy chọn giới hạn thấp hơn hoặc liên hệ nhà vận hành nền tảng.",
```

Run: `(cd web && npm run generate && npm run typecheck && npm run lint:strings && npx vitest run tests/unit/api/errors.test.ts tests/unit/admin 2>&1 | tail -5) && go test ./internal/api/ -count=1 2>&1 | tail -3`
Expected: generate rewrites `src/api/generated/schema.d.ts` (it now contains `QUOTA_ABOVE_CEILING` and `ceiling?:`), typecheck and lint print no errors, vitest passes, Go `ok`.

- [ ] **Step 7: Backend flag and wiring**

`internal/backend/config.go`. After `TenantQuotas` (line 117), add:

```go
	TenantQuotaCeilings  string // JSON, same shape; bounds tenant-admin quota writes
```

After the `-tenant-quotas` flag (lines 292-293), add:

```go
	fs.StringVar(&c.TenantQuotaCeilings, "tenant-quota-ceilings", envOr(getenv, "TCDI_TENANT_QUOTA_CEILINGS", ""),
		"operator ceilings on tenant-admin quota writes as JSON [{tenant,runningWorkspaces,cpu,memory,storage}]; a tenant-admin quota write above its tenant's ceiling, or for a tenant without one, is refused (env TCDI_TENANT_QUOTA_CEILINGS)")
```

Replace lines 425-427 (the `-tenant-quotas` check) with:

```go
	if err := validateTenantQuotaFlags(c.TenantQuotas, c.TenantQuotaCeilings); err != nil {
		return err
	}
```

Add below `validate`:

```go
// validateTenantQuotaFlags parses -tenant-quotas and -tenant-quota-ceilings.
// A tenant may appear in only one: a declared quota is read-only, so a
// ceiling on it would never apply.
func validateTenantQuotaFlags(quotas, ceilings string) error {
	q, err := provisioning.ParseTenantQuotas(quotas)
	if err != nil {
		return fmt.Errorf("-tenant-quotas: %w", err)
	}
	c, err := provisioning.ParseTenantQuotas(ceilings)
	if err != nil {
		return fmt.Errorf("-tenant-quota-ceilings: %w", err)
	}
	declared := make(map[string]bool, len(q))
	for _, e := range q {
		declared[e.TenantID] = true
	}
	for _, e := range c {
		if declared[e.TenantID] {
			return fmt.Errorf("-tenant-quota-ceilings: tenant %q also has a quota in -tenant-quotas, which is read-only; declare one or the other", e.TenantID)
		}
	}
	return nil
}
```

`internal/backend/wire.go`, in `newAppHandler`, after the `managedQuotas` loop (line 747), add:

```go
	// -tenant-quota-ceilings bounds tenant-admin quota writes; a tenant
	// without a declared ceiling cannot have its quota set through the API.
	declaredCeilings, err := provisioning.ParseTenantQuotas(cfg.TenantQuotaCeilings)
	if err != nil {
		return fmt.Errorf("-tenant-quota-ceilings: %w", err)
	}
	quotaCeilings := make(map[string]provisioning.ResourceVector, len(declaredCeilings))
	for _, q := range declaredCeilings {
		quotaCeilings[q.TenantID] = q.Limits
	}
```

Change the handler construction to:

```go
	adminQuotaHandler := api.NewAdminQuotaHandler(api.NewAdminQuotaSource(db), tenants, managedQuotas).
		WithAuditSink(appAudit).
		WithQuotaCeilings(quotaCeilings)
```

Run: `go build ./... && go test ./internal/backend/ -run 'TestParseFlags' -count=1 2>&1 | tail -3`
Expected: `ok`.

- [ ] **Step 8: Chart**

`deploy/helm/tinycdi/templates/_helpers.tpl`, after the `tinycdi.tenantQuotas` define (line 86):

```
{{/* JSON array for the backend -tenant-quota-ceilings flag: one entry per
     managedNamespaces entry that carries a quotaCeiling block. Empty when no
     entry declares one — every tenant-admin quota write is then refused. */}}
{{- define "tinycdi.tenantQuotaCeilings" -}}
{{- $ceilings := list -}}
{{- range .Values.managedNamespaces -}}
{{- if .quotaCeiling -}}
{{- $ceilings = append $ceilings (dict "tenant" .tenant "runningWorkspaces" .quotaCeiling.runningWorkspaces "cpu" .quotaCeiling.cpu "memory" .quotaCeiling.memory "storage" .quotaCeiling.storage) -}}
{{- end -}}
{{- end -}}
{{- if $ceilings }}{{ toJson $ceilings }}{{ end -}}
{{- end }}
```

In the extraEnv deny dict (line 431), add the line `"TCDI_TENANT_QUOTA_CEILINGS" "managedNamespaces[].quotaCeiling"` after the `TCDI_TENANT_QUOTAS` line.

`templates/backend.yaml`, after the `-tenant-quotas` block (line 151):

```yaml
            {{- $tenantQuotaCeilings := include "tinycdi.tenantQuotaCeilings" . }}
            {{- if $tenantQuotaCeilings }}
            # managedNamespaces[].quotaCeiling: the highest quota a tenant
            # admin may set through the admin quota API.
            - {{ printf "-tenant-quota-ceilings=%s" $tenantQuotaCeilings | quote }}
            {{- end }}
```

`values.schema.json`:
- Move the `quota` object (lines 50-62, `type` through the closing brace of `properties`) into `$defs` as `"tenantQuota": {…}`, with a `"description": "Tenant quota amounts: runningWorkspaces, cpu (cores or millicores), memory and storage (byte quantities)."`.
- In the item properties, replace the inline object with `"quota": {"$ref": "#/$defs/tenantQuota"},` and add `"quotaCeiling": {"$ref": "#/$defs/tenantQuota"},`.
- In the `items` object (next to `"required": ["name", "tenant"]`), add `"not": {"required": ["quota", "quotaCeiling"]},`.

`values.yaml` comment (lines 108-137): after the "Values below current usage are accepted…" lines, add:

```yaml
# quotaCeiling (per entry, OPTIONAL; same four keys as quota, not together
# with quota) lets the tenant's admins set the quota themselves through the
# admin quota API, up to these amounts. It renders as -tenant-quota-ceilings.
# Without quota or quotaCeiling, tenant admins cannot set a quota at all and
# every create is refused with QUOTA_NOT_CONFIGURED until an operator
# declares one.
```

In the commented example after line 141, add `  #   # or, to delegate the quota to tenant admins:` and `  #   # quotaCeiling: {runningWorkspaces: 20, cpu: "32", memory: 128Gi, storage: 400Gi}` under the `quota:` line.

`templates/NOTES.txt` (lines 37-41): make the warning body:

```
Tenant quotas: WARNING — no managedNamespaces[].quota for tenant(s)
  {{ join ", " $unquoted }}
  Workspace creates for them are refused with QUOTA_NOT_CONFIGURED (409)
  unless a tenant_quota row already exists. Set quota: {runningWorkspaces,
  cpu, memory, storage} on each entry, or quotaCeiling (same keys) to let
  the tenant's admins set a quota up to it — see install.md "Tenant
  quotas". Also consider managedNamespaces[].namespaceQuota, which bounds
  the namespace in the apiserver independently of the platform quota.
```

Regenerate the golden (Global Constraints command) and confirm `git diff --stat deploy/helm/testdata/` is empty: the minimal values declare neither block. Then run:

Run: `go test ./deploy/helm/ -count=1 2>&1 | tail -3`
Expected: `ok`.

- [ ] **Step 9: Docs**

- `deploy/helm/tinycdi/README.md:97` (`managedNamespaces[]` row): `{name, tenant, quota?, quotaCeiling?, namespaceQuota?}`, and after the `quota` clause add "; `quotaCeiling` instead lets the tenant's admins set the quota up to it".
- README extraEnv table (line 182): add the row ``| `TCDI_TENANT_QUOTA_CEILINGS` | `managedNamespaces[].quotaCeiling` |``.
- README "Tenant quotas" (754-783): append the paragraph:

  > **Tenant-admin quota writes.** A tenant administrator can set the tenant's quota through the portal (`PUT /v1/admin/tenants/{tenant}/quota`) only when the entry declares a `quotaCeiling` block (same four keys as `quota`; an entry carries one or the other). The chart renders those blocks as `-tenant-quota-ceilings`. A write above the ceiling, or for a tenant with neither block, is refused with `409 QUOTA_ABOVE_CEILING`, so a tenant left without a quota stays fail-closed (`QUOTA_NOT_CONFIGURED`) until the operator acts. The ceiling bounds the API-layer quota only; `namespaceQuota` (below) bounds the namespace in the apiserver.

- `docs/runbooks/install.md` "Tenant quotas": after the YAML block, add a bullet **Delegating to tenant admins.**, with the content of the README paragraph above, and in the "Listed tenants only" bullet change "print a warning for every tenant without a `quota` block" to "print a warning for every tenant without a `quota` block (a `quotaCeiling`-only tenant has no row until its admin sets one)".
- `docs/runbooks/tenant-quotas.md` "Where quotas come from": add the subsection `### Tenant-admin quotas under a ceiling` with the YAML

  ```yaml
  managedNamespaces:
    - name: tinycdi-tenant-b
      tenant: tenant-b
      quotaCeiling:
        runningWorkspaces: 20
        cpu: "32"
        memory: 128Gi
        storage: 400Gi
  ```

  and the text: "The tenant's admins set the quota on the portal's Quota page, up to these amounts. A write above the ceiling answers `409 QUOTA_ABOVE_CEILING` and names the field. A tenant with neither `quota` nor `quotaCeiling` cannot have its quota set through the API at all. The backend refuses to start when a tenant is in both `-tenant-quotas` and `-tenant-quota-ceilings`." In the Operations checklist "New tenant onboarded" bullet, add "or a `quotaCeiling` block to let the tenant's admins size it".
- `docs/runbooks/observability.md:200`: change "and `QUOTA_MANAGED_BY_CONFIG` rejections" to "`QUOTA_MANAGED_BY_CONFIG` and `QUOTA_ABOVE_CEILING` rejections".
- `docs/adr/0002-api-conventions.md:98`: after the `QUOTA_MANAGED_BY_CONFIG` row, add ``  | `QUOTA_ABOVE_CEILING` | 409 | no |``.
- `docs/security/threat-model.md` "Quotas" paragraph (210-219): after the first sentence, add "A tenant admin can write the tenant quota (`PUT /v1/admin/tenants/{tenant}/quota`) only up to the operator-declared ceiling (`managedNamespaces[].quotaCeiling`, `-tenant-quota-ceilings`); with no ceiling declared the write is refused (`QUOTA_ABOVE_CEILING`), and a tenant with a declared `quota` is read-only (`QUOTA_MANAGED_BY_CONFIG`)."
- `docs/security/threat-model.md:723` (TM-44 Residual cell only): "Off by default: without it, only the API-layer tenant quota bounds workspaces, and that quota is set by the platform configuration (`quota`) or bounded by its `quotaCeiling`." Task 30 adds the TM-51 row.

Run: `rg -n 'quotaCeiling|QUOTA_ABOVE_CEILING' docs deploy/helm/tinycdi/README.md | wc -l`
Expected: at least 8.

- [ ] **Step 10: Whole packages and gates**

Run: `go vet ./internal/api/ ./internal/backend/ && go test ./internal/api/ ./internal/backend/ ./deploy/helm/ -count=1 2>&1 | tail -4 && KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path --bin-dir "$PWD/bin")" go test -tags=integration -count=1 -run 'Quota' ./tests/integration/ 2>&1 | tail -3 && (cd web && npm run test:unit 2>&1 | tail -3) && for t in .github/tests/*.test.sh; do bash "$t" >/dev/null || echo "FAIL $t"; done`
Expected: three Go `ok` lines plus the integration `ok`, the vitest summary passes, no `FAIL` line.

- [ ] **Step 11: Commit**

```bash
git add internal/api/adminquota.go internal/api/adminquota_test.go internal/api/errors.go internal/api/errors_test.go internal/api/openapi.yaml internal/api/tenantadmin_test.go internal/api/auditroutes_test.go internal/backend/config.go internal/backend/wire.go internal/backend/tenantquota_test.go deploy/helm/tinycdi deploy/helm/chart_tenantquota_test.go deploy/helm/chart_extraenv_test.go deploy/helm/testdata web/src web/tests/unit/api/errors.test.ts docs/runbooks/install.md docs/runbooks/tenant-quotas.md docs/runbooks/observability.md docs/adr/0002-api-conventions.md docs/security/threat-model.md
git commit -m "feat(quota): operator-declared tenant quota ceilings bound tenant-admin writes" -m "R1-02: managedNamespaces[].quotaCeiling renders -tenant-quota-ceilings; a tenant-admin quota PUT above its tenant's ceiling, or for a tenant without one, answers 409 QUOTA_ABOVE_CEILING and stores nothing. Config-managed quotas stay read-only. Pinned by TestAdminQuota_PutAboveOperatorCeilingRefused, TestAdminQuota_UndeclaredTenantCreateRefused and TestAdminQuota_PutAtCeilingAccepted." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 4: Audit hygiene: bounded targets, route-pattern request log, in-tx single-statement writes (R1-16, R1-22, R1-23) — tier: integrate

**Finding (R1-16, final severity P3, TM-13).** The audit actor and the TM-13 audit target are pseudonymised (`ActorRef`). But the stdout `http_request` record for `GET /v1/admin/tenants/{t}/users/{ownerRef}`, the user-limit PUT/DELETE and admin `sessions:revoke-all` contains the target user's raw subject. Anyone reading the log pipeline can de-pseudonymise who was inspected or signed out.
Required fix: log the route template (`r.Pattern`, already used for metrics) instead of `r.URL.Path` in `AuditWithSink`.
Security test: `TestAudit_RequestLogNeverContainsRawOwnerRef` in `internal/api/auditroutes_test.go`.

**Finding (R1-22, final severity P3, TM-50).** `audited()` persists the raw path value as `target_id` before validation. A signed-in user sends refused requests with an attacker-chosen path value, such as `DELETE /v1/workspaces/<60 KB>` (the CSRF refusal is itself audited), and each one writes a ~60 KB `audit_event` row. A `%00` or `%ff` in the segment makes the insert fail (SQLSTATE 22021): the request answers 500, the refused attempt has no durable row and the persist-failure alarm fires.
Required fix: sanitize `TargetUID` and detail values before persisting: replace invalid UTF-8 and NUL, and cap the length. Invariant: no request-controlled byte sequence can make the audit insert fail or exceed a fixed row size.
Security tests: `TestAudited_TargetSanitizedAndBounded` in `internal/api/auditstore_test.go` and a Postgres-backed `TestAudit_InsertSanitizedTarget` in `internal/store/audit_test.go`.

**Finding (R1-23, final severity P3, TM-50).** TM-50 and ADR 0010 say the row is persisted "in the audited transaction", so the mutation and its record commit or roll back together, with only Kubernetes-only mutations excepted. But the quota PUT and every user-limit PUT/DELETE autocommit outside any transaction, so the `TxHook` never runs. If the non-tx audit insert that follows fails, the client gets 500 while the new quota or limit stays in force with no durable record.
Required fix: run `adminQuotaStore.SetLimits` and `UserLimitStore.write` (and, for consistency, the ticket insert) inside `db.WithTx`, so the hook inserts the audit row in the same transaction.
Security test: `TestAdminUserLimit_AuditFailureRollsBackWrite`, Postgres-backed, with a real `DurableAuditSink` whose store fails the insert. After a PUT that answers 500, `tenant_user_limit_default` and `tenant_quota` are unchanged. The test lives in `internal/api/adminuserlimit_test.go`, which has the shared Postgres container (`svNewDB`) and both route environments; `internal/backend` has neither.

Rulings for this task:
- **Request log.** Both the `http_request` slog record and the `http.request` audit event carry `route`, which is `r.Pattern` without its method prefix, or `unmatched`. Neither carries `path` any more.
- **Sanitizer.** A value over 256 bytes keeps its first 200 bytes, cut on a rune boundary, followed by `…sha256:` and the first 16 hex digits of the raw value's SHA-256. The result is at most 226 bytes, stays readable (a long file-transfer `path` is still recognisable), and distinct long values stay distinct.
- **Where it applies.** The sanitizer runs in two layers: in `audited()`, so the stdout record matches the stored row, and in `store.InsertAudit`/`AmendAudit`, so every writer is covered. `ListAudit`'s `TargetID` filter is sanitized the same way, so a filter on a long target still matches its stored row.

Brief claims re-checked at `98c7f54`: `internal/api/middleware.go:436` (`"path", r.URL.Path`; the audit event repeats it at `:460`), `internal/api/auditroutes.go:247-253` (target taken before validation), `internal/store/audit.go:46` (insert), `internal/api/adminquota.go:65`/`:76`, `internal/store/userlimit.go:103-105`, `internal/broker/tickets.go:428` and `internal/store/store.go:99-109` are accurate. `internal/backend/wire.go:913` wraps every app request in `api.Audit`. `RequireTrustedOrigin` and `InstrumentHTTP` pass the same `*http.Request` down, so `r.Pattern` set by the mux is readable in the audit middleware after `next` returns.

**Files:**
- Modify: `internal/api/middleware.go:400-405` (`InstrumentHTTP` uses the new helper), `:433-462` (`AuditWithSink`)
- Modify: `internal/store/audit.go:1-10` (imports), `:44-76` (`InsertAudit`, `AmendAudit`), `:113` (`ListAudit` target filter); new `SanitizeAuditText`
- Modify: `internal/api/auditroutes.go:278-281` (`event()`)
- Modify: `internal/api/adminquota.go:55-93` (`SetLimits`)
- Modify: `internal/store/userlimit.go:101-125` (`write`, `delete`)
- Modify: `internal/broker/tickets.go:428-438` (ticket insert)
- Modify test comment: `internal/api/auditstore_test.go:98`
- Modify test: `internal/api/adminquota_test.go:519,533` (`TestAdminQuota_Audit` reads `route`, not `path`; Step 4)
- Modify docs: `docs/runbooks/observability.md:189`, `:224-227`; `docs/adr/0010-audit-store-and-retention.md:33-35`
- Test: `internal/api/auditroutes_test.go`, `internal/api/auditstore_test.go`, `internal/store/audit_test.go`, `internal/api/adminuserlimit_test.go` (append)

**Interfaces:**
- Consumes: `AuditWithSink(log, sink)`, `RequestID`, `audited`, `routeAdminUserGet`, `routeWorkspaceDelete`, `captureSink`, `fakeDurable`, `plainSink`, `NewDurableAuditSink`, `AuditStore`, `svNewDB`, `newUserLimitsEnv`, `newAdminQuotaEnv`, `NewAdminUserLimitSource`, `NewAdminQuotaSource`, `loginAdmin`, `doReq`, `userLimitDefaultPath`, `adminQuotaPath`, `adminQuotaBody`, `IfMatchCreate`, `newDB` (store tests), `DB.WithTx`.
- Produces:
  - `store.AuditTextMax = 256` and `func store.SanitizeAuditText(s string) string`.
  - `func routePattern(r *http.Request) string` (package `api`, unexported).
  - The log field `route` replaces `path` in the `http_request` record and in the `http.request` event details.
  - `SetLimits`, `UserLimitStore.write`/`delete` and the ticket insert now run in `DB.WithTx`, so a `store.TxHook` in the context fires on them.
- Depends on Task 3: `newAdminQuotaEnv` declares a ceiling by default, and `adminQuotaBody` is within it.

- [ ] **Step 1: Write the failing tests**

`internal/api/auditroutes_test.go`: add `"net/url"` to the imports and append:

```go
// TestAudit_RequestLogNeverContainsRawOwnerRef (R1-16): the per-request
// http_request record and http.request event name the matched route
// template, so an owner-addressed admin route never logs the target's raw
// issuer|sub.
func TestAudit_RequestLogNeverContainsRawOwnerRef(t *testing.T) {
	const subject = "f81d4fae-7dec-11d0-a765-00a0c91e6bf6"
	ownerRef := "https://kc.example/realms/r|" + subject
	logBuf := &bytes.Buffer{}
	sink := &captureSink{}
	mux := http.NewServeMux()
	mux.Handle(routeAdminUserGet, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	h := RequestID(AuditWithSink(slog.New(slog.NewJSONHandler(logBuf, nil)), sink)(mux))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/admin/tenants/tenant-a/users/"+url.PathEscape(ownerRef), nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want the handler's 404 (route matched)", rec.Code)
	}
	out := logBuf.String() + sink.raw()
	if strings.Contains(out, subject) {
		t.Fatalf("request log carries the raw owner subject:\n%s", out)
	}
	const route = `"route":"/v1/admin/tenants/{tenant}/users/{ownerRef}"`
	if !strings.Contains(logBuf.String(), route) {
		t.Fatalf("http_request record lacks %s:\n%s", route, logBuf.String())
	}
	if ev := sink.get(); len(ev) != 1 || ev[0].Details["route"] != "/v1/admin/tenants/{tenant}/users/{ownerRef}" {
		t.Fatalf("http.request event details = %+v, want the route template", ev)
	}
}
```

`internal/api/auditstore_test.go`: add `"strings"` and `"unicode/utf8"` to the imports and append:

```go
// TestAudited_TargetSanitizedAndBounded (R1-22): a request-controlled
// target — 60 KB, NUL, invalid UTF-8 — reaches the durable store as valid
// UTF-8 within store.AuditTextMax, so the insert cannot fail and the row
// cannot bloat.
func TestAudited_TargetSanitizedAndBounded(t *testing.T) {
	for name, seg := range map[string]string{
		"long":    strings.Repeat("A", 60000),
		"nul":     "ws_a%00b",
		"badutf8": "ws_a%ffb",
	} {
		t.Run(name, func(t *testing.T) {
			sink := &fakeDurable{}
			mux := http.NewServeMux()
			mux.Handle(routeWorkspaceDelete, audited(sink, routeWorkspaceDelete,
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					writeError(w, r, CodeInvalidRequest, "invalid workspace id")
				})))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/workspaces/"+seg, nil))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want the handler's 400 (the audit row persisted)", rec.Code)
			}
			if len(sink.persisted) != 1 {
				t.Fatalf("persisted %d rows, want 1", len(sink.persisted))
			}
			for _, got := range []string{sink.persisted[0].TargetUID, sink.stdout[0].TargetUID} {
				if len(got) > store.AuditTextMax || !utf8.ValidString(got) || strings.ContainsRune(got, 0) {
					t.Fatalf("target %q (len %d) is not bounded, valid UTF-8 without NUL", got[:min(len(got), 40)], len(got))
				}
			}
			if name == "long" && (!strings.HasPrefix(sink.persisted[0].TargetUID, "AAAA") || !strings.Contains(sink.persisted[0].TargetUID, "sha256:")) {
				t.Fatalf("long target = %q, want its prefix plus a sha256 marker", sink.persisted[0].TargetUID)
			}
		})
	}
}
```

Fix the comment in `TestAudited_NoTxMutationPersistFailureFailsClosed` (line 100): `// a K8s-only or single-statement mutation` becomes `// a K8s-only mutation (no transaction to share)`.

`internal/store/audit_test.go`: add `"strings"` and `"unicode/utf8"` to the imports and append:

```go
// TestAudit_InsertSanitizedTarget (R1-22): request-controlled audit text
// with NUL, invalid UTF-8 or 60 KB inserts and amends cleanly, valid and
// bounded, and the target filter finds the long row by its raw value.
func TestAudit_InsertSanitizedTarget(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	long := strings.Repeat("A", 60000)
	for name, raw := range map[string]string{"nul": "a\x00b", "badutf8": "a\xffb", "long": long} {
		id, err := store.InsertAudit(ctx, db.Pool(), store.AuditRecord{
			Tenant: "t-san", ActorRef: "oidc:aa", Action: "workspace.delete", TargetKind: "workspace",
			TargetID: raw, Outcome: "failure", RequestID: raw, Details: map[string]string{"path": raw},
		})
		if err != nil {
			t.Fatalf("%s: insert %v", name, err)
		}
		if err := db.AmendAudit(ctx, id, raw, "failure", "INVALID_REQUEST", map[string]string{"path": raw}); err != nil {
			t.Fatalf("%s: amend %v", name, err)
		}
	}
	rows, err := db.ListAudit(ctx, store.AuditFilter{Tenant: "t-san"}, 10, nil)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows=%d err=%v, want 3", len(rows), err)
	}
	for _, r := range rows {
		for _, v := range []string{r.TargetID, r.RequestID, r.Details["path"]} {
			if len(v) > 256 || !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
				t.Fatalf("stored value %q (len %d) not bounded valid UTF-8", v[:min(len(v), 40)], len(v))
			}
		}
	}
	hit, err := db.ListAudit(ctx, store.AuditFilter{Tenant: "t-san", TargetID: long}, 10, nil)
	if err != nil || len(hit) != 1 {
		t.Fatalf("filter by the raw long target: rows=%d err=%v, want 1", len(hit), err)
	}
}
```

`internal/api/adminuserlimit_test.go`: append:

```go
// failingAuditStore is an AuditStore whose writes fail: an audit-store
// outage during the request.
type failingAuditStore struct{}

func (failingAuditStore) InsertAudit(context.Context, store.Tx, store.AuditRecord) (int64, error) {
	return 0, errors.New("audit store down")
}

func (failingAuditStore) AmendAudit(context.Context, int64, string, string, string, map[string]string) error {
	return errors.New("audit store down")
}

// TestAdminUserLimit_AuditFailureRollsBackWrite (R1-23): an admin quota or
// user-limit write commits only together with its durable audit row — when
// the audit insert fails, the request answers 5xx and the row is absent.
func TestAdminUserLimit_AuditFailureRollsBackWrite(t *testing.T) {
	ctx := context.Background()
	rows := func(t *testing.T, db *store.DB, table string) int {
		t.Helper()
		var n int
		if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE tenant_id = 'tenant-a'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	sink := func() *DurableAuditSink { return NewDurableAuditSink(&plainSink{}, failingAuditStore{}, nil, nil) }

	t.Run("user-limit default", func(t *testing.T) {
		db := svNewDB(t)
		env := newUserLimitsEnv(t, NewAdminUserLimitSource(db), sink())
		sess, csrf := loginAdmin(t, env, "admin-a")
		r := doReq(t, env, sess, csrf, http.MethodPut, userLimitDefaultPath, `{"limit":3}`,
			map[string]string{"If-Match": IfMatchCreate})
		r.Body.Close()
		if r.StatusCode < 500 {
			t.Fatalf("status=%d, want 5xx when the audit insert fails", r.StatusCode)
		}
		if n := rows(t, db, "tenant_user_limit_default"); n != 0 {
			t.Fatalf("tenant_user_limit_default has %d rows after a failed audit, want 0", n)
		}
	})
	t.Run("tenant quota", func(t *testing.T) {
		db := svNewDB(t)
		env := newAdminQuotaEnv(t, NewAdminQuotaSource(db), nil,
			func(h *AdminQuotaHandler) { h.WithAuditSink(sink()) })
		sess, csrf := loginAdmin(t, env, "admin-a")
		r := doReq(t, env, sess, csrf, http.MethodPut, adminQuotaPath, adminQuotaBody,
			map[string]string{"If-Match": IfMatchCreate})
		r.Body.Close()
		if r.StatusCode < 500 {
			t.Fatalf("status=%d, want 5xx when the audit insert fails", r.StatusCode)
		}
		if n := rows(t, db, "tenant_quota"); n != 0 {
			t.Fatalf("tenant_quota has %d rows after a failed audit, want 0", n)
		}
	})
}
```

`adminuserlimit_test.go` already imports `context`, `errors`, `net/http` and `store`.

- [ ] **Step 2: Run them and see them fail**

Run: `go test ./internal/api/ -run 'TestAudit_RequestLogNeverContainsRawOwnerRef|TestAudited_TargetSanitizedAndBounded|TestAdminUserLimit_AuditFailureRollsBackWrite' -count=1 2>&1 | grep -E -- '--- (FAIL|SKIP)|undefined' | head`
Expected: compile failure `undefined: store.AuditTextMax`. With that line commented out, the three tests fail as follows:
- `TestAudit_RequestLogNeverContainsRawOwnerRef`: "request log carries the raw owner subject".
- `TestAudited_TargetSanitizedAndBounded/long`: target not bounded.
- `TestAdminUserLimit_AuditFailureRollsBackWrite`: "has 1 rows after a failed audit". Docker is required; a SKIP is not a pass.

Run: `go test ./internal/store/ -run TestAudit_InsertSanitizedTarget -count=1 2>&1 | tail -3`
Expected: FAIL on the first `nul` or `badutf8` insert (the cases are a map, so the order varies) with `ERROR: invalid byte sequence for encoding "UTF8"` (SQLSTATE 22021).

- [ ] **Step 3: The sanitizer (R1-22)**

`internal/store/audit.go`: add `"crypto/sha256"`, `"encoding/hex"`, `"strings"` and `"unicode/utf8"` to the imports. After `detailsJSON`, add:

```go
// AuditTextMax bounds one request-controlled audit value — a target id,
// request id or detail value — in bytes.
const AuditTextMax = 256

// auditTextKeep is how much of an oversized value is kept readable.
const auditTextKeep = 200

// SanitizeAuditText makes a request-controlled value safe to persist: NUL
// and invalid UTF-8 become U+FFFD (Postgres text rejects both), and a value
// longer than AuditTextMax keeps its first auditTextKeep bytes followed by
// "…sha256:" and 16 hex digits of the raw value's digest — so no request
// can make the insert fail or grow the row past a fixed size, and distinct
// long values stay distinct.
func SanitizeAuditText(s string) string {
	out := s
	if !utf8.ValidString(out) {
		out = strings.ToValidUTF8(out, "\uFFFD")
	}
	if strings.IndexByte(out, 0) >= 0 {
		out = strings.ReplaceAll(out, "\x00", "\uFFFD")
	}
	if len(out) <= AuditTextMax {
		return out
	}
	cut := auditTextKeep
	for cut > 0 && !utf8.RuneStart(out[cut]) {
		cut--
	}
	sum := sha256.Sum256([]byte(s))
	return out[:cut] + "…sha256:" + hex.EncodeToString(sum[:8])
}

func sanitizeAuditDetails(d map[string]string) map[string]string {
	if d == nil {
		return nil
	}
	out := make(map[string]string, len(d))
	for k, v := range d {
		out[SanitizeAuditText(k)] = SanitizeAuditText(v)
	}
	return out
}
```

Then:
- In `InsertAudit`, make the first line `details, err := detailsJSON(sanitizeAuditDetails(r.Details))`. In the argument list, replace `r.TargetID` with `SanitizeAuditText(r.TargetID)` and `r.RequestID` with `SanitizeAuditText(r.RequestID)`.
- In `AmendAudit`, make the first line `raw, err := detailsJSON(sanitizeAuditDetails(details))` and pass `SanitizeAuditText(targetID)` as `$2`.
- In `ListAudit` (line 113), make the filter `add("target_id = $%d", SanitizeAuditText(f.TargetID))`.

`internal/api/auditroutes.go`, in `event()` (lines 278-281): replace the return with

```go
			details := make(map[string]string, len(ra.details))
			for k, v := range ra.details {
				details[k] = store.SanitizeAuditText(v)
			}
			if len(details) == 0 {
				details = nil
			}
			return observability.AuditEvent{
				Actor: actorOrAnonymous(actor), Action: action, TargetUID: store.SanitizeAuditText(ra.target), Tenant: tenant,
				RequestID: RequestIDFromContext(ctx), Outcome: outcome, ErrorCode: errCode, Details: details,
			}
```

Add a line to the `audited` doc comment: "The target and detail values are sanitized (store.SanitizeAuditText) so the stdout record and the stored row agree, and no request-controlled value can fail the insert."

- [ ] **Step 4: Route template in the request log (R1-16)**

`internal/api/middleware.go`. Above `InstrumentHTTP`, add:

```go
// routePattern is the matched ServeMux pattern without its method prefix,
// or "unmatched". Logs and metrics name routes by it — never by the raw
// path, whose parameters can carry an owner's issuer|sub.
func routePattern(r *http.Request) string {
	route := strings.TrimPrefix(r.Pattern, r.Method+" ")
	if route == "" {
		return "unmatched"
	}
	return route
}
```

In `InstrumentHTTP`, replace the three `route :=` lines with `route := routePattern(r)`.

In `AuditWithSink`:
- the slog attrs pair `"path", r.URL.Path,` becomes `"route", routePattern(r),`;
- the event details map entry `"path": r.URL.Path,` becomes `"route": routePattern(r),`.

One existing test reads the old key: `TestAdminQuota_Audit` (`internal/api/adminquota_test.go:533`). Replace `` !strings.Contains(logs, `"path":"`+adminQuotaPath+`"`) || `` with `` !strings.Contains(logs, `"route":"/v1/admin/tenants/{tenant}/quota"`) || ``, and in its doc comment (line 519) change "method+path+status" to "method+route+status".

Extend the redaction-rules comment above `Audit` with: "- never log the raw path — path values can carry an owner's issuer|sub; the record names the matched route template."

- [ ] **Step 5: Single-statement writes run in the audited transaction (R1-23)**

`internal/api/adminquota.go`, replace the body of `SetLimits` (lines 63-92) with:

```go
	var since time.Time
	if !create {
		var err error
		if since, err = time.Parse(time.RFC3339Nano, expectedVersion); err != nil {
			return "", store.ErrVersionMismatch
		}
	}
	// One statement, run in a transaction so the audited route's TxHook
	// inserts the audit row with it: the limit and its record commit or
	// roll back together (ADR 0010).
	var at time.Time
	err := s.db.WithTx(ctx, func(tx store.Tx) error {
		if create {
			return tx.QueryRow(ctx, `
				INSERT INTO tenant_quota (tenant_id, max_running_slots, max_cpu_millis, max_memory_bytes, max_disk_bytes, updated_at)
				VALUES ($1, $2, $3, $4, $5, clock_timestamp())
				ON CONFLICT (tenant_id) DO NOTHING
				RETURNING updated_at`,
				tenantID, v.RunningSlots, v.CPUMillis, v.MemoryBytes, v.DiskBytes).Scan(&at)
		}
		return tx.QueryRow(ctx, `
			UPDATE tenant_quota SET
				max_running_slots = $2, max_cpu_millis = $3,
				max_memory_bytes  = $4, max_disk_bytes = $5,
				updated_at        = clock_timestamp()
			WHERE tenant_id = $1 AND updated_at = $6
			RETURNING updated_at`,
			tenantID, v.RunningSlots, v.CPUMillis, v.MemoryBytes, v.DiskBytes, since).Scan(&at)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", store.ErrVersionMismatch
		}
		return "", err
	}
	return store.FormatVersion(at), nil
```

Append to the `SetLimits` doc comment: "The statement runs in DB.WithTx so an audited request's TxHook persists the audit row in the same transaction."

`internal/store/userlimit.go`, replace `write` and `delete` (lines 101-125) with:

```go
// write runs a conditional INSERT/UPDATE ... RETURNING updated_at in a
// transaction — so an audited request's TxHook commits the audit row with
// it — and maps no returned row to a failed precondition.
func (s *UserLimitStore) write(ctx context.Context, sql string, args ...any) (string, error) {
	var at time.Time
	err := s.db.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&at)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrVersionMismatch
		}
		return "", err
	}
	return FormatVersion(at), nil
}

// delete runs a conditional DELETE in a transaction (see write); zero rows
// means the precondition failed.
func (s *UserLimitStore) delete(ctx context.Context, sql string, args ...any) error {
	return s.db.WithTx(ctx, func(tx Tx) error {
		tag, err := tx.Exec(ctx, sql, args...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrVersionMismatch
		}
		return nil
	})
}
```

`internal/broker/tickets.go:428-438`, wrap the insert:

```go
	// In a transaction so the connection route's audit TxHook commits the
	// audit row with the ticket (ADR 0010).
	if err := b.db.WithTx(ctx, func(tx store.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO launch_ticket
				(ticket_hash, workspace_id, tenant_id, principal_subject,
				 runtime_generation, runtime_uid, audience, takeover,
				 request_id, expires_at, clipboard_policy, session_policy,
				 portal_session_digest)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			ticketHash(token), workspaceUID, tenantID, owner,
			int64(binding.RuntimeGeneration), binding.RuntimeUID,
			b.audience, takeover, requestID(ctx), expires, clipboard,
			resolved, portalSessionDigest(portalSessionID))
		return err
	}); err != nil {
		return Ticket{}, fmt.Errorf("broker: insert ticket: %w", err)
	}
```

Task 12 (R1-25) also edits `tickets.go`, in the redeem path (`:490-608`). The two hunks do not overlap.

Run: `go build ./... && go test ./internal/api/ -run 'TestAudit|TestAudited|TestAdminUserLimit|TestAdminQuota|TestUserLimit|TestInstrumentHTTP|TestAuditCoverage' -count=1 2>&1 | tail -3 && go test ./internal/store/ ./internal/broker/ -count=1 2>&1 | tail -3`
Expected: three `ok` lines.

- [ ] **Step 6: Docs**

- `docs/runbooks/observability.md:189` (`targetUid` row): append "; request-controlled values here and in `details` are stored and logged as valid UTF-8 (NUL and invalid bytes become U+FFFD) of at most 256 bytes — a longer value keeps its first 200 bytes followed by `…sha256:` and 16 hex digits of the full value's digest".
- `docs/runbooks/observability.md:224-227`: after "…the per-request `http_request` log record that every HTTP call produces.", add: "That record, and the matching `http.request` event, names the matched route template in `route` (for example `/v1/admin/tenants/{tenant}/users/{ownerRef}`, or `unmatched`), never the raw path, so path values such as an owner's `issuer|sub` stay out of the log."
- `docs/adr/0010-audit-store-and-retention.md`, in the Pseudonymization bullet after "(`observability.RedactDetails`).": add "Request-controlled values (target id, request id, detail values) are stored as valid UTF-8 of at most 256 bytes (`store.SanitizeAuditText`), so no request can fail the insert or grow a row without bound."

ADR 0010 "One function, fail closed" and the TM-50 Control cell now hold for the admin quota and user-limit writes, so no edit is needed there. Task 30 owns the register cells.

- [ ] **Step 7: Whole packages and gates**

Run: `go vet ./internal/api/ ./internal/store/ ./internal/broker/ && go test ./internal/api/ ./internal/store/ ./internal/broker/ ./internal/backend/ -count=1 2>&1 | tail -5 && KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path --bin-dir "$PWD/bin")" go test -tags=integration -count=1 -run 'Audit|Quota|Ticket|Launch' ./tests/integration/ 2>&1 | tail -3 && for t in .github/tests/*.test.sh; do bash "$t" >/dev/null || echo "FAIL $t"; done`
Expected: four Go `ok` lines, the integration `ok`, no `FAIL` line.

- [ ] **Step 8: Commit**

```bash
git add internal/api/middleware.go internal/api/auditroutes.go internal/api/adminquota.go internal/api/adminquota_test.go internal/store/audit.go internal/store/userlimit.go internal/broker/tickets.go internal/api/auditroutes_test.go internal/api/auditstore_test.go internal/api/adminuserlimit_test.go internal/store/audit_test.go docs/runbooks/observability.md docs/adr/0010-audit-store-and-retention.md
git commit -m "fix(audit): bound audit targets, log route patterns, audit single-statement writes in-tx" -m "R1-16: the http_request record and http.request event carry the route template, never the raw path. R1-22: store.SanitizeAuditText makes target, request id and detail values valid UTF-8 within 256 bytes in audited() and the store. R1-23: the admin quota and user-limit writes and the ticket insert run in DB.WithTx so the audit TxHook commits the row with them. Pinned by TestAudit_RequestLogNeverContainsRawOwnerRef, TestAudited_TargetSanitizedAndBounded, TestAudit_InsertSanitizedTarget and TestAdminUserLimit_AuditFailureRollsBackWrite." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 5: Retained PVC label carries the CR name (R1-24) — tier: transcribe

**Finding (R1-24, final severity P3, new B2/B6 provisioning).** On attach, the applier writes `in.Spec.WorkspaceName` into the PVC's `workspace-name` label. That value is the user-chosen display name (`service.go:315`, `retained.go:644`; up to 128 chars). The linux backend stamps the CR name `ws-<hex>` there instead. Two problems follow:
- A name of 64-128 chars is not a valid label value, so the patch fails and the user's own attach retries forever (self-DoS).
- After a database loss, the importer derives `SourceWorkspaceID` from this label. A user who names a workspace `ws-<another workspace's hex id>` gets their retained disk's quota attributed to that workspace id.
Required fix: write the CR name (`WorkspaceCRName(workspaceUID)`) to the label, as the linux backend does.
Security test: `TestRetainedAttach_LabelIsCRName` in `internal/provisioning/retained_applier_test.go`. A 100-char display name attaches, and the label equals `ws-<hex>`.

Brief claims re-checked at `98c7f54`:
- `internal/provisioning/retained_applier.go:173` (`pvc.Labels[linux.LabelWorkspaceName] = in.Spec.WorkspaceName`) is accurate.
- `internal/provisioning/retained_sync.go:177-181` (the importer takes `SourceWorkspaceID` from the label, requiring only the `ws-` prefix) is accurate.
- `internal/runtime/linux/backend.go:470` (`LabelWorkspaceName: ws.Name`) is accurate.
- `internal/api/workspaces.go:360` (`workspaceNamePattern` admits `ws-…` names up to 128 chars) is accurate.
- In `applyAttach`, `ws` is the consuming CR, fetched by `WorkspaceCRName(in.WorkspaceUID)` (`retained_applier.go:136-141`), so `ws.Name` is the CR name.

**Files:**
- Modify: `internal/provisioning/retained_applier.go:169-173`
- Modify test: `internal/provisioning/retained_applier_test.go:139-142` (the existing label assertion expects the display name)
- Test: `internal/provisioning/retained_applier_test.go` (append)

**Interfaces:**
- Consumes: `provisioning.WorkspaceCRName`, `NewRetainedApplier`, `NewK8sApplier`, `NewRetainedStore`, `ImportRetained`, `AttachRequest`, `NewOutbox(db).PendingIntents`, `PlatformID`, and the test helpers `recoveryDB`, `seedWorkspaceRow`, `retainedPVC`, `retainedApplyScheme`, `quotaVec`.
- Produces: the invariant that a retained PVC's `workspaces.cdi.tinyorbit.vn/workspace-name` label is always the consuming CR's name (`ws-<hex>`), never user text. No new identifiers.

- [ ] **Step 1: Write the failing test and fix the existing assertion**

Append to `internal/provisioning/retained_applier_test.go`:

```go
// TestRetainedAttach_LabelIsCRName (R1-24): the retargeted PVC's
// workspace-name label is the consuming CR's name, never the user's display
// name — a 100-char name (not a valid label value) attaches, and a name
// shaped like another workspace's CR name cannot steer the post-DB-loss
// importer.
func TestRetainedAttach_LabelIsCRName(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	rstore := provisioning.NewRetainedStore(db)
	svc := provisioning.NewService(db)
	const tenant = "tenant-r124"
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.SetQuota(ctx, tx, tenant, provisioning.ResourceVector{
			RunningSlots: 10, CPUMillis: 1 << 20, MemoryBytes: 1 << 40, DiskBytes: 1 << 40})
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	seedWorkspaceRow(t, db, "ws_r124src01", tenant, "iss|sub-1", "src-r124")
	rec, err := rstore.ImportRetained(ctx, provisioning.RetainedDiskInfo{
		TenantID: tenant, Owner: "iss|sub-1", SourceWorkspaceID: "ws_r124src01",
		Runtime: "LinuxContainer", SizeBytes: 2 << 30,
		PVCNamespace: "ns-r124", PVCName: "pvc-r124", PVCUID: "uid-pvc-r124",
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	// 100 chars, a valid workspace name, and shaped like a CR name.
	displayName := "ws-" + strings.Repeat("b", 97)
	ws, err := svc.AttachRetained(ctx, tenant, "iss|sub-1", "iss|sub-1", rec.ID, "k-r124",
		provisioning.AttachRequest{
			Name:     displayName,
			Template: provisioning.TemplateInfo{ID: "tpl_t", Name: "tmpl", Runtime: "LinuxContainer"},
			Vector:   quotaVec, DesiredState: "Stopped",
		}, []byte("a"))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	pending, err := provisioning.NewOutbox(db).PendingIntents(ctx, provisioning.PlatformID(ws.ID))
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending intents = %v (err %v), want 1 create", pending, err)
	}

	pvc := retainedPVC("ns-r124", "pvc-r124", "cruid-src", "ws-r124src01", nil)
	kc := fake.NewClientBuilder().WithScheme(retainedApplyScheme(t)).WithObjects(pvc).Build()
	tmap := provisioning.TenantNamespaces{tenant: "ns-r124"}
	applier := provisioning.NewRetainedApplier(provisioning.NewK8sApplier(kc, tmap), kc, tmap, rstore)
	if err := applier.Apply(ctx, pending[0]); err != nil {
		t.Fatalf("apply create: %v", err)
	}

	got := &corev1.PersistentVolumeClaim{}
	if err := kc.Get(ctx, client.ObjectKey{Namespace: "ns-r124", Name: "pvc-r124"}, got); err != nil {
		t.Fatalf("pvc: %v", err)
	}
	want := provisioning.WorkspaceCRName(pending[0].WorkspaceUID)
	if label := got.Labels["workspaces.cdi.tinyorbit.vn/workspace-name"]; label != want {
		t.Fatalf("workspace-name label = %q, want the consuming CR name %q", label, want)
	}
	if !strings.HasPrefix(want, "ws-") || len(want) > 63 {
		t.Fatalf("CR name %q is not a valid ws-<hex> label value", want)
	}
}
```

In `TestRetainedApplierSameRevisionTwice` (lines 139-142), the existing assertion expects the display name. Change it to:

```go
	if gotPVC.Labels["workspaces.cdi.tinyorbit.vn/workspace-uid"] != string(cr.UID) ||
		gotPVC.Labels["workspaces.cdi.tinyorbit.vn/workspace-name"] != provisioning.WorkspaceCRName(create.WorkspaceUID) {
		t.Fatalf("pvc labels after replay = %v, want consumer binding", gotPVC.Labels)
	}
```

The file already imports `context`, `strings`, `testing`, `corev1`, `client`, `fake`, `provisioning` and `store`.

- [ ] **Step 2: Run them and see them fail**

Run: `go test ./internal/provisioning/ -run 'TestRetainedAttach_LabelIsCRName|TestRetainedApplierSameRevisionTwice' -count=1 2>&1 | grep -E -- '--- FAIL|label = |labels after' | head`
Expected:
- `TestRetainedAttach_LabelIsCRName` fails with `workspace-name label = "ws-bbbb…", want the consuming CR name "ws-…"`.
- `TestRetainedApplierSameRevisionTwice` fails with `pvc labels after replay = …workspace-name:consumer-b10r…`.
Docker is required; a SKIP is not a pass.

- [ ] **Step 3: Write the CR name**

`internal/provisioning/retained_applier.go`, lines 169-173. Replace the comment and the two label lines with:

```go
	// Both consumer labels name the CR, never the platform id or the
	// user's display name — matching what the linux backend stamps and
	// what its claim checks enforce. The workspace-name label must be the
	// CR name (ws-<hex>): it is a valid label value whatever the display
	// name, and the post-DB-loss importer derives the source workspace id
	// from it (retained_sync.go), so user text there could misattribute
	// the disk.
	pvc.Labels[linux.LabelWorkspaceUID] = string(ws.UID)
	pvc.Labels[linux.LabelWorkspaceName] = ws.Name
```

- [ ] **Step 4: Run the focused tests**

Run: `go test ./internal/provisioning/ -run 'TestRetained' -count=1 2>&1 | tail -3`
Expected: `ok`.

- [ ] **Step 5: Whole package and gates**

Run: `go vet ./internal/provisioning/ && go test ./internal/provisioning/ -count=1 2>&1 | tail -3 && KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path --bin-dir "$PWD/bin")" go test -tags=integration -count=1 -run 'Retain' ./tests/integration/ 2>&1 | tail -3 && for t in .github/tests/*.test.sh; do bash "$t" >/dev/null || echo "FAIL $t"; done`
Expected: no vet output, `ok`, the integration `ok`, no `FAIL` line.

No docs change: no published document describes the label's value. Task 30 adds the register row.

- [ ] **Step 6: Commit**

```bash
git add internal/provisioning/retained_applier.go internal/provisioning/retained_applier_test.go
git commit -m "fix(retained): label retained PVCs with the workspace CR name" -m "R1-24: attach wrote the user's display name into the workspace-name label; a long name failed the patch forever and a ws-shaped name could steer the post-DB-loss importer. The label now carries the consuming CR's name, as the linux backend stamps it. Pinned by TestRetainedAttach_LabelIsCRName." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 6: App-listener unknown-cookie lookup limiter (R1-03) — tier: design

**Finding (R1-03, P2, TM-06).**
- **Attack.** An unauthenticated Internet client sends `GET /v1/session`, `/v1/me`, `/v1/workspaces` or any other `/v1` route. Each request carries a fresh random `__Host-tcdi_session` value. Each request costs Postgres a primary-key SELECT plus a DELETE (`scanSession` on `ErrNoRows`). On the rate-limited probe, the read happens during key resolution, so even the 429 answers cost a store read. Without a cookie, the same requests cost nothing. The session listener bounds exactly this (TM-27 `SessionLookupLimiter`). The app listener has no equivalent, and the Postgres pool is shared with the broker (leases, renew). The proof is 200 forged-cookie requests from one client: 201 store reads on the probe (199 of them refused 429), and 200 reads on `/v1/me`.
- **Required fix.** Add a local, per-client unknown-cookie lookup limiter to the app listener, mirroring the gateway's `SessionLookupLimiter`: keyed by `ratelimit.ClientKey(r, trusted)`, local, never shared. Consult it inside `requireAuth` and inside `SessionRateLimitKey`, before any `sessions.Get/Peek` for a cookie value. When it is exhausted, answer 429, or treat the request as unauthenticated on the probe, without touching the store. Invariant: the number of store reads per client per minute is bounded, however many distinct cookie values the client sends. Also remove the opportunistic `DELETE` from the read path in `scanSession`, so a miss never costs a write, and prune expired rows in the leader's sweep instead.
- **Security test.** `TestAppListener_UnknownCookieSprayBounded` in `internal/api/middleware_test.go`, with a wired-stack variant in `internal/backend/backend_test.go`. It fails today with 201 and 200 reads.

**Design (Decision 7).** Only lookups that miss spend the budget. `lookupSession` takes a token before the read and gives it back (`Refund`) when the read finds a session or the store fails transiently. A miss keeps the token spent. When the bucket is empty, there is no read at all.
- A valid session never drains its own client key, so 50 signed-in users behind one NAT are never throttled (Review Focus 5).
- During an active spray from the *same* client key, a valid user on that key can get 429 until the bucket refills (300/min). This matches the session listener's behaviour for cookies it has not seen, and is the residual.
- `NewAuthenticator` installs the default bound (300/min, burst 120), so a caller that never thought about it fails safe. The backend replaces it with `-session-lookup-rate` and the trusted-proxy list.

**Files:**
- Modify: `internal/ratelimit/ratelimit.go` (new method `Refund` after `Allow`, lines 79-114); test `internal/ratelimit/ratelimit_test.go`
- Create: `internal/api/session_lookup.go`
- Modify: `internal/api/auth.go:390-403` (default limiter in `NewAuthenticator`), `:316-337` (two `Authenticator` fields)
- Modify: `internal/api/principal.go:65-71` (new context key)
- Modify: `internal/api/middleware.go:90-125` (`requireAuth` reads through `lookupSession`)
- Modify: `internal/api/ratelimit_key.go:34-53` (`SessionRateLimitKey` reads through `lookupSession` and marks the miss)
- Modify: `internal/api/session_probe.go:45-55` (`SessionProbeHandler` never re-reads after the resolver)
- Modify: `internal/store/sessions.go:143-215,220-239` (`scanSession` loses the DELETE); new `PruneDeadSessions` in `internal/store/retention.go`
- Modify: `internal/backend/retention.go` (new `deadSessionSweepLoop`), `internal/backend/wire.go:443-451` (register it) and `:803-808` (install the configured limiter), `internal/backend/config.go:335-336` (flag help)
- Test: `internal/api/middleware_test.go`, `internal/backend/backend_test.go`, new `internal/store/sessions_pg_test.go`
- Docs: `docs/security/threat-model.md` §B2 "Rate limits" bullet (lines 175-190), `docs/adr/0006-rate-limit-state.md:86`, `docs/runbooks/observability.md:71`

**Interfaces:**
- Consumes: `ratelimit.ClientKey(r *http.Request, trusted []netip.Prefix) string`, `ratelimit.RetryAfterSeconds(time.Duration) int`, `ratelimit.New(ratePerMinute, burst, maxKeys int, now func() time.Time) *Limiter`, `gateway.SessionLookupBurst` (120), `Config.SessionLookupRate` (`-session-lookup-rate`, default 300).
- Produces:
  - `func (l *ratelimit.Limiter) Refund(key string)`: returns one token to an existing bucket, capped at burst. It is a no-op for an unknown key or a disabled limiter.
  - `type api.SessionLookupLimiter interface { Allow(key string) (bool, time.Duration); Refund(key string) }`
  - `const api.SessionLookupRate = 300`, `const api.SessionLookupBurst = 120`
  - `func (a *Authenticator) WithSessionLookupLimiter(l SessionLookupLimiter, trusted []netip.Prefix) *Authenticator`. A nil `l` disables the bound; tests that build an `Authenticator` literal get no limiter.
  - `func (a *Authenticator) lookupSession(r *http.Request, id string, slide bool) (*Session, error)`. It returns `*lookupLimitedError` (with field `retryAfter time.Duration`) when the client's budget is spent.
  - `RequireAuth`/`RequireAuthPassive` answer `429 RATE_LIMITED` with `Retry-After` when the budget is spent. The probe answers `200 {"authenticated":false}`.
  - Metric: `tinycdi_rate_limited_total{route="session_lookup"}` also counts app-listener refusals.
  - `func (d *store.DB) PruneDeadSessions(ctx context.Context, idle time.Duration, limit int64) (int64, error)`
  - `func deadSessionSweepLoop(db *store.DB, idle time.Duration, log *slog.Logger) func(ctx context.Context)` (package `backend`)
  - Test helpers in `internal/api/middleware_test.go`: `type lookupCounter` (counts `Get`+`Peek`) and `lookupSprayServer(t, counter, lookups) *httptest.Server`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/ratelimit/ratelimit_test.go`:

```go
// TestLimiter_RefundReturnsOneToken: Refund gives back exactly one token,
// never above burst, and is a no-op for a key the limiter has not seen.
func TestLimiter_RefundReturnsOneToken(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := New(60, 2, 100, func() time.Time { return now }) // frozen clock, burst 2
	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("token %d refused", i)
		}
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("third token allowed with a frozen clock")
	}
	l.Refund("k")
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("refunded token not available")
	}
	l.Refund("k")
	l.Refund("k")
	l.Refund("k") // capped at burst 2
	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("token %d after refunds refused", i)
		}
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("refunds exceeded burst")
	}
	l.Refund("never-seen") // must not create a bucket or panic
	var nilL *Limiter
	nilL.Refund("k")
}
```

Append to `internal/api/middleware_test.go`, and add `"fmt"` (already imported), `"sync/atomic"`, `"time"` and `"github.com/tinyorbitvn/tinycdi/internal/ratelimit"` to its imports:

```go
// lookupCounter counts every session-store read (Get and Peek).
type lookupCounter struct {
	SessionStore
	reads atomic.Int64
}

func (s *lookupCounter) Get(ctx context.Context, id string) (*Session, error) {
	s.reads.Add(1)
	return s.SessionStore.Get(ctx, id)
}

func (s *lookupCounter) Peek(ctx context.Context, id string) (*Session, error) {
	s.reads.Add(1)
	return s.SessionStore.Peek(ctx, id)
}

// lookupSprayServer mounts the production shape of the two cookie-reading
// routes: the probe behind the session-keyed login limiter (burst 1, so
// almost every probe is refused 429) and GET /v1/me behind passive auth.
func lookupSprayServer(t *testing.T, counter *lookupCounter, lookups SessionLookupLimiter) *httptest.Server {
	t.Helper()
	a := sessionKeyTestAuth(counter)
	a.WithSessionLookupLimiter(lookups, nil)
	mux := http.NewServeMux()
	MountSessionProbeRoute(mux, a, RateLimitWithKey(ratelimit.New(1, 1, 1000, nil), nil, nil, a.SessionRateLimitKey()))
	MountMeRoutes(mux, a, NewMeHandler(testSessionDomain))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestAppListener_UnknownCookieSprayBounded (R1-03): one client sends 200
// requests, each with a distinct forged session cookie, across the probe
// and /v1/me. Store reads stay within the unknown-cookie burst, and the
// refused /v1/me requests answer 429 with Retry-After, not 401.
func TestAppListener_UnknownCookieSprayBounded(t *testing.T) {
	counter := &lookupCounter{SessionStore: NewInMemorySessionStore(30 * time.Minute)}
	frozen := time.Unix(1_700_000_000, 0)
	const burst = 10
	srv := lookupSprayServer(t, counter, ratelimit.New(60, burst, 1000, func() time.Time { return frozen }))

	const n = 200
	limited := 0
	for i := 0; i < n; i++ {
		path := "/v1/session"
		if i%2 == 1 {
			path = "/v1/me"
		}
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.AddCookie(&http.Cookie{Name: "__Host-tcdi_session", Value: fmt.Sprintf("forged-%d", i)})
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s #%d: %v", path, i, err)
		}
		resp.Body.Close()
		if path == "/v1/me" && resp.StatusCode == http.StatusTooManyRequests {
			if resp.Header.Get("Retry-After") == "" {
				t.Fatalf("/v1/me #%d: 429 without Retry-After", i)
			}
			limited++
		}
	}
	if got := counter.reads.Load(); got > burst+2 {
		t.Fatalf("store reads = %d for %d forged cookies from one client, want <= %d", got, n, burst+2)
	}
	if limited == 0 {
		t.Fatal("no /v1/me request was refused 429 once the unknown-cookie budget was spent")
	}
}

// TestAppListener_ValidSessionsBehindOneClientNotThrottled (Review Focus
// 5): only misses spend the budget, so fifty signed-in users on one client
// address, five requests each, are all served by a lookup burst of 10.
func TestAppListener_ValidSessionsBehindOneClientNotThrottled(t *testing.T) {
	counter := &lookupCounter{SessionStore: NewInMemorySessionStore(30 * time.Minute)}
	frozen := time.Unix(1_700_000_000, 0)
	a := sessionKeyTestAuth(counter)
	a.WithSessionLookupLimiter(ratelimit.New(60, 10, 1000, func() time.Time { return frozen }), nil)
	mux := http.NewServeMux()
	MountSessionProbeRoute(mux, a, RateLimitWithKey(ratelimit.New(60, 20, 1000, nil), nil, nil, a.SessionRateLimitKey()))
	MountMeRoutes(mux, a, NewMeHandler(testSessionDomain))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for u := 0; u < 50; u++ {
		sess := saveSession(t, counter, fmt.Sprintf("sess-nat-%d", u))
		for j := 0; j < 5; j++ {
			path := "/v1/me"
			if j%2 == 0 {
				path = "/v1/session"
			}
			req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
			req.AddCookie(sess)
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("user %d %s: %v", u, path, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("user %d request %d %s = %d, want 200 — valid sessions must not spend the unknown-cookie budget", u, j, path, resp.StatusCode)
			}
		}
	}
}
```

Append to `internal/backend/backend_test.go` (add `"fmt"` to its imports if it is missing):

```go
// TestAppListener_UnknownCookieSprayRefused (R1-03, wired stack): the
// production app handler carries the default unknown-cookie bound (burst
// 120): a forged-cookie spray from one client gets 401 until the budget is
// spent, then 429 with Retry-After, never an unbounded run of store reads.
func TestAppListener_UnknownCookieSprayRefused(t *testing.T) {
	srv := httptest.NewServer(testAppHandler(t))
	t.Cleanup(srv.Close)
	// The limiter refills on the real clock (5 tokens/s at the default
	// 300/min), so a slow run may see a few extra non-429 answers; allow
	// refill-sized slack, never an unbounded run.
	first429, late := -1, 0
	for i := 0; i < 200; i++ {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/me", nil)
		req.AddCookie(&http.Cookie{Name: "__Host-tcdi_session", Value: fmt.Sprintf("forged-%d", i)})
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("GET /v1/me #%d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			if resp.Header.Get("Retry-After") == "" {
				t.Fatalf("#%d: 429 without Retry-After", i)
			}
			if first429 < 0 {
				first429 = i
			}
		} else if first429 >= 0 {
			late++
		}
	}
	if first429 < api.SessionLookupBurst || first429 > api.SessionLookupBurst+5 {
		t.Fatalf("first 429 at request %d, want ~%d (the default burst)", first429, api.SessionLookupBurst)
	}
	if late > 5 {
		t.Fatalf("%d non-429 answers after the budget was spent, want only refill-sized slack", late)
	}
}
```

Create `internal/store/sessions_pg_test.go`:

```go
// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func countSessions(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	if err := db.Pool().QueryRow(context.Background(), `SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestSessionStore_MissNeverWrites (R1-03): a read that finds no live
// session writes nothing — an expired row stays until the sweep, so a
// forged-cookie spray never turns into DELETE traffic.
func TestSessionStore_MissNeverWrites(t *testing.T) {
	db := newDB(t)
	ss := store.NewSessionStore(db, time.Minute, nil)
	ctx := context.Background()
	now := time.Now().UTC()
	expired := &store.Session{ID: "sess-expired", Issuer: "iss", Subject: "sub", TenantID: "tenant-a",
		CreatedAt: now.Add(-2 * time.Hour), LastSeenAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)}
	if err := ss.Save(ctx, expired); err != nil {
		t.Fatal(err)
	}
	for _, read := range []func(context.Context, string) (*store.Session, error){ss.Get, ss.Peek} {
		if _, err := read(ctx, "sess-expired"); !errors.Is(err, store.ErrSessionNotFound) {
			t.Fatalf("read expired = %v, want ErrSessionNotFound", err)
		}
	}
	if n := countSessions(t, db); n != 1 {
		t.Fatalf("sessions rows = %d after missed reads, want 1 (the read path must not delete)", n)
	}
}

// TestPruneDeadSessions_RemovesOnlyDeadRows (R1-03): the leader sweep
// deletes absolute-expired, idle-expired and stale-epoch rows and keeps a
// live session.
func TestPruneDeadSessions_RemovesOnlyDeadRows(t *testing.T) {
	db := newDB(t)
	idle := 30 * time.Minute
	ss := store.NewSessionStore(db, idle, nil)
	ctx := context.Background()
	now := time.Now().UTC()
	mk := func(id string, lastSeen, expires time.Time) {
		t.Helper()
		if err := ss.Save(ctx, &store.Session{ID: id, Issuer: "iss", Subject: id, TenantID: "tenant-a",
			CreatedAt: lastSeen, LastSeenAt: lastSeen, ExpiresAt: expires}); err != nil {
			t.Fatal(err)
		}
	}
	mk("sess-live", now, now.Add(time.Hour))
	mk("sess-absolute", now, now.Add(-time.Minute))
	mk("sess-idle", now.Add(-2*time.Hour), now.Add(time.Hour))
	mk("sess-stale", now, now.Add(time.Hour))
	if _, err := db.Pool().Exec(ctx,
		`UPDATE sessions SET epoch = 'stale-epoch' WHERE id = encode(sha256($1::bytea), 'hex')`, "sess-stale"); err != nil {
		t.Fatal(err)
	}
	n, err := db.PruneDeadSessions(ctx, idle, 500)
	if err != nil || n != 3 {
		t.Fatalf("PruneDeadSessions = %d, %v; want 3, nil", n, err)
	}
	if got := countSessions(t, db); got != 1 {
		t.Fatalf("rows left = %d, want 1", got)
	}
	if _, err := ss.Peek(ctx, "sess-live"); err != nil {
		t.Fatalf("live session gone: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests and see them fail**

```bash
go test ./internal/ratelimit/ -run TestLimiter_RefundReturnsOneToken -count=1 2>&1 | tail -3
go test ./internal/api/ -run 'TestAppListener_(UnknownCookieSprayBounded|ValidSessionsBehindOneClientNotThrottled)' -count=1 2>&1 | tail -5
go test ./internal/backend/ -run TestAppListener_UnknownCookieSprayRefused -count=1 2>&1 | tail -3
go test ./internal/store/ -run 'TestSessionStore_MissNeverWrites|TestPruneDeadSessions' -count=1 -v 2>&1 | grep -E '^(--- |ok|FAIL|.*undefined)' | head
```

Expected: ratelimit fails to compile (`l.Refund undefined`). api fails to compile (`undefined: SessionLookupLimiter`, `a.WithSessionLookupLimiter undefined`). backend fails to compile (`undefined: api.SessionLookupBurst`). store fails to compile (`db.PruneDeadSessions undefined`). With `PruneDeadSessions` stubbed, `TestSessionStore_MissNeverWrites` fails with `sessions rows = 0 after missed reads, want 1`. The store tests need Docker; a skip is not a pass.

- [ ] **Step 3: `Limiter.Refund`**

In `internal/ratelimit/ratelimit.go`, directly after `Allow`, add:

```go
// Refund returns one token to key's bucket, capped at burst. A caller
// that spends a token before an operation whose cost turns out to be
// nothing (a session lookup that found a live session) gives it back, so
// only the costly outcome is rate limited. Refunding a key the limiter
// has not seen, or on a disabled limiter, does nothing.
func (l *Limiter) Refund(key string) {
	if l == nil || l.perSec <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	el, ok := l.buckets[key]
	if !ok {
		return
	}
	b := el.Value.(*bucket)
	b.tokens++
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
}
```

- [ ] **Step 4: The api limiter**

Create `internal/api/session_lookup.go`:

```go
// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/ratelimit"
)

// session_lookup.go — the app listener's per-client bound on session-store
// reads for cookie values (TM-06). A random-cookie spray would otherwise
// turn request rate directly into Postgres reads on a pool the broker
// shares. Only reads that miss spend the budget: a lookup takes a token
// first and gives it back when it finds a live session (or the store
// fails), so signed-in users behind one address are never throttled by it.
// The bound is local to the replica, like the session listener's
// SessionLookupLimiter: a shared window would spend the reads it exists to
// prevent.

// SessionLookupRate and SessionLookupBurst are the default bound
// NewAuthenticator installs (per client, per replica) — the session
// listener's defaults. The backend replaces it with -session-lookup-rate.
const (
	SessionLookupRate    = 300
	SessionLookupBurst   = 120
	sessionLookupMaxKeys = 100_000
)

// SessionLookupLimiter is the per-client budget for session-store lookups;
// *ratelimit.Limiter implements it.
type SessionLookupLimiter interface {
	Allow(key string) (ok bool, retryAfter time.Duration)
	Refund(key string)
}

// lookupLimitedError is lookupSession's refusal: the client spent its
// unknown-cookie budget, and the store was not read.
type lookupLimitedError struct{ retryAfter time.Duration }

func (e *lookupLimitedError) Error() string { return "api: session lookup rate limited" }

// WithSessionLookupLimiter replaces the default unknown-cookie bound. trusted
// is the -trusted-proxies list ratelimit.ClientKey uses to find the client.
// A nil limiter disables the bound.
func (a *Authenticator) WithSessionLookupLimiter(l SessionLookupLimiter, trusted []netip.Prefix) *Authenticator {
	a.lookups, a.trusted = l, trusted
	return a
}

// lookupSession reads the session a cookie value names — Get (sliding the
// idle deadline) or Peek — through the per-client budget. It returns
// *lookupLimitedError without a store read when the budget is spent, and
// otherwise the store's own answer.
func (a *Authenticator) lookupSession(r *http.Request, id string, slide bool) (*Session, error) {
	var key string
	if a.lookups != nil {
		key = ratelimit.ClientKey(r, a.trusted)
		if ok, retry := a.lookups.Allow(key); !ok {
			if a.metrics != nil {
				a.metrics.IncRateLimited("session_lookup")
			}
			return nil, &lookupLimitedError{retryAfter: retry}
		}
	}
	var sess *Session
	var err error
	if slide {
		sess, err = a.sessions.Get(r.Context(), id)
	} else {
		sess, err = a.sessions.Peek(r.Context(), id)
	}
	if a.lookups != nil && !errors.Is(err, ErrSessionNotFound) {
		// A hit, or a store that could not answer: not the client's spray.
		a.lookups.Refund(key)
	}
	return sess, err
}

// writeLookupLimited answers a spent unknown-cookie budget: 429 with
// Retry-After, the same shape as every other limiter refusal.
func writeLookupLimited(w http.ResponseWriter, r *http.Request, e *lookupLimitedError) {
	w.Header().Set("Retry-After", strconv.Itoa(ratelimit.RetryAfterSeconds(e.retryAfter)))
	writeError(w, r, CodeRateLimited, "rate limit exceeded")
}

// sessionLookedUp reports whether SessionRateLimitKey already tried this
// request's cookie and found no session; the probe then answers without a
// second read.
func sessionLookedUp(r *http.Request) bool {
	v, _ := r.Context().Value(ctxKeySessionLookedUp).(bool)
	return v
}
```

In `internal/api/principal.go`, add `ctxKeySessionLookedUp` as the last entry of the `ctxKey` const block (after `ctxKeyRouteAudit`).

In `internal/api/auth.go`, add two fields to the `Authenticator` struct, after `auditSink`:

```go
	// lookups bounds session-store reads per client for cookie values that
	// turn out unknown (TM-06); trusted feeds ratelimit.ClientKey.
	lookups SessionLookupLimiter
	trusted []netip.Prefix
```

In `NewAuthenticator`, set the default in the `&Authenticator{...}` literal (after `sessions: sessions,`):

```go
		lookups:            ratelimit.New(SessionLookupRate, SessionLookupBurst, sessionLookupMaxKeys, nil),
```

Add `"github.com/tinyorbitvn/tinycdi/internal/ratelimit"` to `auth.go`'s imports.

- [ ] **Step 5: Route every cookie read through `lookupSession`**

In `internal/api/middleware.go`, `requireAuth`, replace:

```go
		var sess *Session
		if slide {
			sess, err = a.sessions.Get(r.Context(), c.Value)
		} else {
			sess, err = a.sessions.Peek(r.Context(), c.Value)
		}
		if err != nil {
			switch {
			case errors.Is(err, ErrSessionNotFound):
```

with:

```go
		sess, err := a.lookupSession(r, c.Value, slide)
		if err != nil {
			var lim *lookupLimitedError
			switch {
			case errors.As(err, &lim):
				writeLookupLimited(w, r, lim)
			case errors.Is(err, ErrSessionNotFound):
```

(`err` is already declared by the `r.Cookie` line, so write `sess, err := …` only if `sess` is new in scope; it is, so `:=` compiles.)

In `internal/api/ratelimit_key.go`, `SessionRateLimitKey`, replace the body after the cookie check with:

```go
		sess, err := a.lookupSession(r, c.Value, false)
		if err != nil {
			// Unknown, expired, refused by the unknown-cookie budget, or
			// a store outage: all mean "no authenticated key". The miss is
			// parked on the request so the probe answers without a second
			// read; RequireAuth still returns its own 503 on transient
			// errors.
			*r = *r.WithContext(context.WithValue(r.Context(), ctxKeySessionLookedUp, true))
			return ""
		}
		// The assignment mutates the request the middleware passes on —
		// reuse of the auth path's lookup, not a second store read.
		*r = *r.WithContext(context.WithValue(r.Context(), ctxKeySession, sess))
		return ratelimit.KeyDigest("sess:", sess.ID)
```

In the function's doc comment, after "Validity is checked with Peek — no idle slide, no write —", add: "through the per-client unknown-cookie budget (lookupSession),".

In `internal/api/session_probe.go`, `SessionProbeHandler`, replace:

```go
	} else if c, err := r.Cookie(a.cfg.SessionCookieName); err == nil && c.Value != "" {
		_, err := a.sessions.Peek(r.Context(), c.Value)
		authenticated = err == nil
	}
```

with:

```go
	} else if c, err := r.Cookie(a.cfg.SessionCookieName); err == nil && c.Value != "" && !sessionLookedUp(r) {
		// A spent unknown-cookie budget reads as signed out: the probe
		// is anonymous and must not cost the store a read (TM-06).
		_, err := a.lookupSession(r, c.Value, false)
		authenticated = err == nil
	}
```

Check that nothing else in `internal/api` reads the store for a raw cookie value:

```bash
rg -n 'a\.sessions\.(Get|Peek)\(' internal/api --glob '!*_test.go'
```

Expected: only `session_lookup.go` (the two reads), `session_probe.go` `SessionTouchHandler` (`a.sessions.Get(r.Context(), sess.ID)`, which runs behind `RequireAuthPassive`, so the session is already proven), and `auth.go` `LogoutHandler` (behind `RequireAuth`). Leave those two alone.

- [ ] **Step 6: The store read path writes nothing, and the leader prunes**

In `internal/store/sessions.go`:
- Change the `Get` doc comment's first sentence to: "Get returns the session and slides last_seen_at. Unknown, expired, or stale-epoch sessions are reported as ErrSessionNotFound and left in place for the leader's sweep (PruneDeadSessions), so a read never writes for a cookie it cannot resolve:".
- Replace `scanSession` with a version without the DELETE. Its signature becomes `func (s *SessionStore) scanSession(row pgx.Row, id string) (*Session, error)`, and the `errors.Is(err, pgx.ErrNoRows)` branch becomes:

```go
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
```

  Update its doc comment to: "scanSession materializes the sessionReturning/sessionColumns projection; no row is ErrSessionNotFound."
- Change both call sites (`Get` and `Peek`) to `return s.scanSession(row, id)`. If `key` becomes unused in either function, inline `sessionKey(id)` into the query arguments.

Append to `internal/store/retention.go`:

```go
// PruneDeadSessions deletes up to limit session rows that no read can
// resolve any more — a rotated epoch, a passed absolute expiry, or (with
// idle > 0) a last_seen_at outside the idle window — in one statement, and
// returns the count. Session reads never delete (a forged-cookie spray must
// not turn into write traffic), so this leader-only sweep is what bounds
// the table.
func (d *DB) PruneDeadSessions(ctx context.Context, idle time.Duration, limit int64) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	idleCond := "FALSE"
	args := []any{limit}
	if idle > 0 {
		idleCond = "last_seen_at <= now() - $2::interval"
		args = append(args, fmt.Sprintf("%dms", idle.Milliseconds()))
	}
	tag, err := d.pool.Exec(ctx, `
		DELETE FROM sessions
		WHERE id IN (
			SELECT id FROM sessions
			WHERE epoch <> `+currentEpochSQL+`
			   OR (expires_at IS NOT NULL AND expires_at <= now())
			   OR `+idleCond+`
			ORDER BY id
			LIMIT $1)`, args...)
	if err != nil {
		return 0, fmt.Errorf("store: prune dead sessions %w", err)
	}
	return tag.RowsAffected(), nil
}
```

In `internal/backend/retention.go`, add after `terminalRetentionSweepLoop`:

```go
// deadSessionSweepLoop is the leader-gated singleton that deletes portal
// session rows no read can resolve (rotated epoch, absolute or idle
// expiry). Session reads never delete, so this is the only thing that
// bounds the table; batches of terminalRetentionBatch rows, hourly.
func deadSessionSweepLoop(db *store.DB, idle time.Duration, log *slog.Logger) func(ctx context.Context) {
	return func(ctx context.Context) {
		sweep := func() {
			var total int64
			for {
				n, err := db.PruneDeadSessions(ctx, idle, terminalRetentionBatch)
				if err != nil {
					if !errors.Is(err, context.Canceled) {
						log.Error("dead session sweep failed", "err", err)
					}
					return
				}
				total += n
				if n < terminalRetentionBatch {
					break
				}
			}
			log.Info("dead session sweep", "sessions_pruned", total)
		}
		sweep()
		ticker := time.NewTicker(terminalRetentionInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweep()
			}
		}
	}
}
```

In `internal/backend/wire.go`, after the `if cfg.TerminalRetentionDays > 0 { … }` block (lines 443-451), add:

```go
	// Dead portal sessions (rotated epoch, absolute or idle expiry) are
	// pruned by the leader: session reads never delete, so a forged-cookie
	// spray cannot turn into write traffic (TM-06).
	b.singletons = append(b.singletons, deadSessionSweepLoop(db, cfg.SessionIdle, log))
```

In the same file, after the `trusted, err := ratelimit.ParseTrustedProxies(cfg.TrustedProxies)` block at lines 805-808, add:

```go
	// TM-06: the app listener bounds session-store reads for cookie values
	// that turn out unknown, per client and per replica, with the session
	// listener's budget (-session-lookup-rate, burst SessionLookupBurst).
	authn.WithSessionLookupLimiter(
		ratelimit.New(cfg.SessionLookupRate, gateway.SessionLookupBurst, rateLimitMaxKeys, nil), trusted)
```

In `internal/backend/config.go:335-336`, change the `-session-lookup-rate` help text to start: `"per-client requests/minute carrying a session cookie that does not resolve to a live session — on the app listener and for cookies this replica has never seen on the session listener (burst "`, and keep the rest.

- [ ] **Step 7: Run the focused tests**

```bash
go build ./... && go vet ./internal/api/ ./internal/store/ ./internal/backend/ ./internal/ratelimit/
go test ./internal/ratelimit/ -count=1 2>&1 | tail -2
go test ./internal/api/ -count=1 2>&1 | tail -3
go test ./internal/backend/ -count=1 2>&1 | tail -3
go test ./internal/store/ -count=1 2>&1 | tail -3
KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path --bin-dir "$PWD/bin")" go test -tags=integration -count=1 -run 'TestPGSessionStore|TestSessionEpochRotation|TestSessionTouch' ./tests/integration/ 2>&1 | tail -3
```

Expected: all `ok`. `TestSessionProbe_ReusesRateLimitLookup` (one Peek per valid probe) and `TestLogin_IPKeyedNoStoreRead` (no Peek on `/v1/login`) stay green unchanged. Their `sessionKeyTestAuth` literal has no limiter, which also proves the nil path. If an existing api test sends more than 120 requests with unknown cookies from one address, pass `env.auth.WithSessionLookupLimiter(nil, nil)` in that test only, and say so in the commit body.

- [ ] **Step 8: Docs**

- `docs/security/threat-model.md`, §B2 "Rate limits" bullet: after the sentence ending "…the callback also has a 10× per-IP ceiling.", add: "Session-store reads for cookie values are bounded per client on the app listener too: `lookupSession` (`internal/api/session_lookup.go`) spends a token from a local bucket (`-session-lookup-rate`, burst 120) before the read and returns it when the read finds a live session, so only unknown cookies spend the budget; when it is spent, authenticated routes answer `429` and the probe answers signed out, without a store read. Session reads never delete; the leader prunes dead rows (`PruneDeadSessions`)."
- `docs/adr/0006-rate-limit-state.md:86`: change the `-session-lookup-rate` row's text to "per-client bound on requests carrying a session cookie that does not resolve: on the app listener (`internal/api/session_lookup.go`, only misses spend) and for cookies the session listener has never seen — a **local** per-replica bucket only, so a random-cookie spray cannot multiply Postgres reads (`internal/backend/config.go`, `internal/gateway/proxy.go`, `internal/api/session_lookup.go`)".
- `docs/runbooks/observability.md:71`: change "`session_lookup` (unseen-cookie session-directory lookups — local per-replica bound, `-session-lookup-rate`)" to "`session_lookup` (session-store lookups for unknown cookies on either listener — local per-replica bound, `-session-lookup-rate`)".

Then run: `for t in .github/tests/*.test.sh; do bash "$t" || exit 1; done`. Expected: every script exits 0.

- [ ] **Step 9: Commit**

```bash
git add internal/ratelimit/ internal/api/session_lookup.go internal/api/auth.go internal/api/principal.go internal/api/middleware.go internal/api/ratelimit_key.go internal/api/session_probe.go internal/api/middleware_test.go internal/store/sessions.go internal/store/retention.go internal/store/sessions_pg_test.go internal/backend/retention.go internal/backend/wire.go internal/backend/config.go internal/backend/backend_test.go docs/security/threat-model.md docs/adr/0006-rate-limit-state.md docs/runbooks/observability.md
git commit -m "fix(auth): bound unknown-cookie store lookups per client on the app listener" -m "R1-03 (P2, TM-06): every session-store read for a cookie value on the app listener now spends a token from a local per-client bucket and gets it back on a hit, so a forged-cookie spray is bounded at the burst while valid sessions are never throttled. The probe no longer reads twice, and session reads no longer delete; the leader prunes dead rows." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 7: /v1 browser headers; cookie and ID-token contract (R1-10, R1-11, R1-12) — tier: integrate

**Findings.**
- **R1-10 (P3, TM-01), merged with AUTH-6.**
  - **Attack.** This is a defence-in-depth gap. The edge sends `/v1` on the portal host to the backend app listener (`deploy/helm/tinycdi/templates/ingress.yaml:33`, `httproute.yaml:53`). `wrapApp` (`internal/backend/wire.go:909-916`) sets none of the portal's browser headers. So `/v1` responses can be framed by any site (no `frame-ancestors`, no XFO). JSON that echoes user-controlled strings (workspace names, IdP display names, audit details) carries no `nosniff` and no CSP. Tenant-admin responses (`/v1/admin/tenants/{t}/users` with names and emails, `/audit`, `/sessions`) and user listings may stay in the browser's HTTP cache after sign-out on a shared machine. Only `/v1/me`, the probe, connections, logout, revoke-all and file transfers set `no-store`.
  - **Required fix.** Add an API header middleware outermost in `wrapApp`. It sets `X-Content-Type-Options: nosniff`, `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'`, `X-Frame-Options: DENY` and a strict `Referrer-Policy` on every app-listener response. It also sets `Cache-Control: no-store` unless the handler sets its own. Invariant: every response on the portal host carries nosniff and denies framing, whichever upstream served it.
  - **Security test.** `TestWrapApp_PortalBrowserHeaders` in `internal/backend/backend_test.go`: `GET /v1/me` (401), `GET /v1/session` (200) and an unknown `/v1` path (mux 404) through the production app handler. It asserts nosniff, a CSP containing `frame-ancestors 'none'`, and XFO `DENY` on all three. It fails today.
- **R1-11 (P3, TM-01).**
  - **Attack.** There is no direct exploit. `internal/api/openapi.yaml:1745-1761` names the cookie `tcdi_session` and says "`SameSite=Strict` on the portal origin" and "Combined with `SameSite=Strict` cookies this blocks cross-site request forgery". The code uses `__Host-tcdi_session` (`auth.go:123`) with `SameSiteLaxMode` (`auth.go:131-132`). Two `auth.go` comments, at `:62-65` ("the portal is a separate origin from the API") and `:851-853` ("portal and session origins live on separate registrable domains"), contradict the chart default, where portal and session are same-site. The web mock (`web/tests/mock-api/auth.ts:41,52`) also sets Strict.
  - **Required fix.** In openapi.yaml, the `sessionCookie` scheme becomes `__Host-tcdi_session` with `SameSite=Lax`, and CSRF is described as blocked by the `X-CSRF-Token` synchronizer token plus the Origin / `Sec-Fetch-Site` gate. Fix the two `auth.go` comments. Make the mock-api cookie Lax.
  - **Security test.** `TestOpenAPI_SessionCookieMatchesAuthDefaults` in `internal/api/openapi_contract_test.go`. It fails today.
- **R1-12 (P3, TM-20).**
  - **Attack.** There is no direct exploit. `openapi.yaml:203` says "The session keeps no ID token, so `id_token_hint` is not sent." `auth.go:170-173` says the token is "never written to a log line or an API response". `auth.go:160-162` says "ID/access tokens are never sent to the client". In fact `POST /v1/logout` and `POST /v1/me/sessions:revoke-all` return the raw ID token as `id_token_hint` in `endSessionUrl` (`endSessionURL`, `auth.go:812`). The browser then puts it in a top-level navigation URL, so it lands in browser history and the IdP's access logs. `logout_test.go:134-138` already asserts that the hint is the login ID token.
  - **Required fix.** Correct `openapi.yaml:203` and the `auth.go` comments to say the retained ID token is sent as `id_token_hint` in the end-session URL. Add one line to the threat model's B2 "Portal session" bullet stating that the token reaches the browser and the IdP URL at sign-out.
  - **Security test.** `TestOpenAPI_LogoutDocumentsIDTokenHint` in `internal/api/openapi_contract_test.go`. It fails while the description says "not sent".

**Files:**
- Create: `internal/api/headers.go`; test `internal/api/headers_test.go`
- Modify: `internal/backend/wire.go:906-916` (`wrapApp`); test `internal/backend/backend_test.go`
- Modify: `internal/api/openapi.yaml:186-205` (`logout` description), `:240-258` (`revokeAllSessions` description), `:1745-1761` (`securitySchemes`); regenerate `web/src/api/generated/schema.d.ts`
- Modify: `internal/api/auth.go:62-66` (`SameSite` field comment), `:160-162` (`Session` doc), `:170-173` (`IDToken` comment), `:851-853` (`sessionCookie` doc)
- Modify: `web/tests/mock-api/auth.ts:41,52`
- Test: `internal/api/openapi_contract_test.go`
- Docs: `docs/security/threat-model.md` §Boundary 2 (lines 144-170)

**Interfaces:**
- Consumes: `specDoc(t *testing.T) map[string]any` (`internal/api/openapi_contract_test.go:413`), `AuthConfig.withDefaults()` (`auth.go:122`), `testAppHandler(t) http.Handler` (`internal/backend/backend_test.go:335`).
- Produces:
  - `func api.APIBrowserHeaders(next http.Handler) http.Handler`
  - `const api.APIContentSecurityPolicy = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"`
  - Every app-listener response carries `X-Content-Type-Options: nosniff`, that CSP, `X-Frame-Options: DENY` and `Referrer-Policy: no-referrer`. `Cache-Control: no-store` is the default, and a handler that sets its own `Cache-Control` wins.

- [ ] **Step 1: Write the failing tests**

Create `internal/api/headers_test.go`:

```go
// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAPIBrowserHeaders_SetOnEveryResponse (R1-10): the app-listener
// header middleware pins the browser policy on JSON, error and mux-404
// answers alike, and a handler's own Cache-Control wins over the default.
func TestAPIBrowserHeaders_SetOnEveryResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/echo", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, CodeNotFound, "<img src=x onerror=alert(1)>")
	})
	mux.HandleFunc("GET /v1/cached", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, max-age=60")
		w.WriteHeader(http.StatusOK)
	})
	h := APIBrowserHeaders(mux)
	for _, path := range []string{"/v1/echo", "/v1/nope", "/v1/cached"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://portal.example.invalid"+path, nil))
		want := map[string]string{
			"X-Content-Type-Options":  "nosniff",
			"Content-Security-Policy": APIContentSecurityPolicy,
			"X-Frame-Options":         "DENY",
			"Referrer-Policy":         "no-referrer",
			"Cache-Control":           "no-store",
		}
		if path == "/v1/cached" {
			want["Cache-Control"] = "private, max-age=60"
		}
		for name, v := range want {
			if got := rec.Header().Get(name); got != v {
				t.Errorf("%s %d: %s = %q, want %q", path, rec.Code, name, got, v)
			}
		}
	}
}
```

Append to `internal/backend/backend_test.go`:

```go
// TestWrapApp_PortalBrowserHeaders (R1-10): every app-listener response
// on the portal host — an authenticated route's 401, the anonymous probe's
// 200, and the mux's own 404 — carries nosniff, a CSP that denies framing,
// XFO DENY and no-store.
func TestWrapApp_PortalBrowserHeaders(t *testing.T) {
	srv := httptest.NewServer(testAppHandler(t))
	t.Cleanup(srv.Close)
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/v1/me", http.StatusUnauthorized},
		{"/v1/workspaces", http.StatusUnauthorized},
		{"/v1/session", http.StatusOK},
		{"/v1/no-such-route", http.StatusNotFound},
	} {
		resp, err := http.Get(srv.URL + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("GET %s = %d, want %d", tc.path, resp.StatusCode, tc.want)
		}
		h := resp.Header
		if h.Get("X-Content-Type-Options") != "nosniff" ||
			!strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
			h.Get("X-Frame-Options") != "DENY" ||
			h.Get("Cache-Control") != "no-store" ||
			h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("GET %s: headers nosniff=%q csp=%q xfo=%q cache=%q referrer=%q",
				tc.path, h.Get("X-Content-Type-Options"), h.Get("Content-Security-Policy"),
				h.Get("X-Frame-Options"), h.Get("Cache-Control"), h.Get("Referrer-Policy"))
		}
	}
}
```

(`backend_test.go` already imports `io`, `net/http`, `net/http/httptest` and `strings`; add any that `go vet` reports missing.)

Append to `internal/api/openapi_contract_test.go` (add `"net/http"` if missing; it is imported today):

```go
// TestOpenAPI_SessionCookieMatchesAuthDefaults (R1-11): the published
// sessionCookie scheme names the cookie the Authenticator really sets, with
// its real SameSite mode, and the CSRF scheme names the controls that
// actually block cross-site forgery (the synchronizer token and the
// Origin / Sec-Fetch-Site gate), not a SameSite=Strict cookie.
func TestOpenAPI_SessionCookieMatchesAuthDefaults(t *testing.T) {
	var cfg AuthConfig
	cfg.withDefaults()
	schemes := specDoc(t)["components"].(map[string]any)["securitySchemes"].(map[string]any)
	cookie := schemes["sessionCookie"].(map[string]any)
	if cookie["name"] != cfg.SessionCookieName {
		t.Errorf("sessionCookie name = %v, want %q", cookie["name"], cfg.SessionCookieName)
	}
	if cfg.SameSite != http.SameSiteLaxMode {
		t.Fatalf("AuthConfig default SameSite = %v, want Lax — update this test with the contract", cfg.SameSite)
	}
	desc, _ := cookie["description"].(string)
	if !strings.Contains(desc, "SameSite=Lax") || strings.Contains(desc, "Strict") {
		t.Errorf("sessionCookie description must state SameSite=Lax and never Strict: %q", desc)
	}
	csrf, _ := schemes["csrfToken"].(map[string]any)["description"].(string)
	if strings.Contains(csrf, "Strict") || !strings.Contains(csrf, "Sec-Fetch-Site") || !strings.Contains(csrf, "Origin") {
		t.Errorf("csrfToken description must name the Origin / Sec-Fetch-Site gate and no Strict cookie: %q", csrf)
	}
}

// TestOpenAPI_LogoutDocumentsIDTokenHint (R1-12): logout and sign-out-
// everywhere return the retained ID token as id_token_hint in
// endSessionUrl (logout_test.go asserts it), so the contract must say so
// and must not claim the hint is not sent.
func TestOpenAPI_LogoutDocumentsIDTokenHint(t *testing.T) {
	paths := specDoc(t)["paths"].(map[string]any)
	for _, p := range []string{"/v1/logout", "/v1/me/sessions:revoke-all"} {
		desc, _ := paths[p].(map[string]any)["post"].(map[string]any)["description"].(string)
		if strings.Contains(desc, "not sent") || strings.Contains(desc, "keeps no ID token") {
			t.Errorf("%s description claims no ID token is sent: %q", p, desc)
		}
		if !strings.Contains(desc, "id_token_hint") {
			t.Errorf("%s description does not document id_token_hint", p)
		}
	}
}
```

- [ ] **Step 2: Run them and see them fail**

```bash
go test ./internal/api/ -run 'TestAPIBrowserHeaders|TestOpenAPI_(SessionCookieMatchesAuthDefaults|LogoutDocumentsIDTokenHint)' -count=1 2>&1 | tail -8
go test ./internal/backend/ -run TestWrapApp_PortalBrowserHeaders -count=1 2>&1 | tail -8
```

Expected: api fails to compile (`undefined: APIBrowserHeaders`, `undefined: APIContentSecurityPolicy`). With the middleware stubbed, `TestOpenAPI_SessionCookieMatchesAuthDefaults` fails with `sessionCookie name = tcdi_session, want "__Host-tcdi_session"`, and `TestOpenAPI_LogoutDocumentsIDTokenHint` fails with `/v1/logout description claims no ID token is sent` and `/v1/me/sessions:revoke-all description does not document id_token_hint`. Backend: `GET /v1/me: headers nosniff="" csp="" xfo="" …`.

- [ ] **Step 3: The middleware**

Create `internal/api/headers.go`:

```go
// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import "net/http"

// APIContentSecurityPolicy is the CSP on every app-listener response: the
// API serves JSON and redirects only, so nothing may load, frame it, set a
// base URL or submit a form from it.
const APIContentSecurityPolicy = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// APIBrowserHeaders pins the browser security policy on every response the
// app listener writes (TM-01): the edge routes /v1 on the portal host here,
// so these answers need the same framing and sniffing protection the
// frontend gives the SPA. Cache-Control defaults to no-store — tenant-admin
// listings and user data must not outlive a sign-out in a shared browser's
// HTTP cache — and a handler that sets its own Cache-Control wins, because
// it writes its headers after this middleware.
func APIBrowserHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", APIContentSecurityPolicy)
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
```

In `internal/backend/wire.go`, `wrapApp`, make the middleware outermost, and add to the doc comment "APIBrowserHeaders is outermost, so every app-listener answer (readiness included) carries the browser policy (TM-01)":

```go
	return api.APIBrowserHeaders(b.readyz(api.RequestID(api.Audit(b.log)(
		api.RequireTrustedOrigin(authn.SessionCookieName(), portalOrigins)(
			api.InstrumentHTTP(b.metrics, "app")(mux))))))
```

If an earlier task changed the inner chain, keep that inner chain exactly and only wrap the whole expression in `api.APIBrowserHeaders(…)`.

- [ ] **Step 4: Contract and comments**

`internal/api/openapi.yaml`, `components.securitySchemes`:

```yaml
    sessionCookie:
      type: apiKey
      in: cookie
      name: __Host-tcdi_session
      description: |
        OIDC-backed session cookie issued by the portal login flow
        (`__Host-` prefix: host-only, `Secure`, `Path=/`; `HttpOnly`,
        `SameSite=Lax`). Lax cookies ride top-level GET navigations, so
        the cookie alone never authorizes a state change: see `csrfToken`.
        Required on every endpoint.
    csrfToken:
      type: apiKey
      in: header
      name: X-CSRF-Token
      description: |
        CSRF token obtained from `GET /v1/me` and echoed on every mutating
        request (a synchronizer token derived from the session). A missing
        or mismatched token returns `403 CSRF_FAILED`. Independently, every
        state-changing request must carry an `Origin` equal to a configured
        portal origin, or `Sec-Fetch-Site: same-origin` when `Origin` is
        absent. These two checks, not the cookie's SameSite mode, block
        cross-site request forgery. Required on all non-GET operations.
```

`/v1/logout` description: replace the sentence "The session keeps no ID token, so `id_token_hint` is not sent." with: "The session's retained ID token is sent as `id_token_hint` on that URL, so the provider can skip its own confirmation page: the raw ID token therefore reaches the browser in this response and the provider in the navigation URL. A session whose sealed token cannot be opened sends `client_id` only."

`/v1/me/sessions:revoke-all` description: after "the portal navigates the browser there.", add: "As with logout, the caller's retained ID token is sent on that URL as `id_token_hint`."

Regenerate: `cd web && npm run generate && cd ..`.

`internal/api/auth.go`:
- `SameSite` field comment (lines 62-65): "SameSite for the session cookie. Defaults to SameSite=Lax: OIDC top-level GET navigations must carry the session, and the portal and session hosts are same-site in the default topology. Lax cookies ride top-level GETs, so CSRF rests on the synchronizer token (RequireCSRF) and the Origin / Sec-Fetch-Site gate (RequireTrustedOrigin), not on SameSite."
- `Session` doc (lines 160-162): replace "ID/access tokens are never sent to the client, never persisted here, and never logged." with "The access token is never kept. The ID token is kept sealed at rest (IDToken) and reaches the browser only as id_token_hint in the end-session URL at sign-out; it is never logged."
- `IDToken` field comment (lines 170-173): "IDToken is the raw OIDC id_token retained so logout can send id_token_hint (the provider then skips its own confirmation page). It is persisted only AEAD-sealed (store), never logged, and leaves the server only inside the end-session URL that POST /v1/logout and POST /v1/me/sessions:revoke-all return."
- `sessionCookie` doc (lines 851-853): "sessionCookie builds the host-only session cookie: the __Host- prefix and no Domain attribute scope it to the portal host exactly, so a session host on the same site can never set or read it. Secure, HttpOnly, SameSite per config (default Lax)."

`web/tests/mock-api/auth.ts`: on lines 41 and 52, change `SameSite=Strict` to `SameSite=Lax`.

- [ ] **Step 5: Threat model**

In `docs/security/threat-model.md`, §Boundary 2 "Controls", after the "Frontend security headers" bullet, add:

```markdown
- **API response headers**: `APIBrowserHeaders` (`internal/api/headers.go`)
  is the outermost app-listener middleware (`wrapApp`,
  `internal/backend/wire.go`). Every `/v1` answer carries
  `X-Content-Type-Options: nosniff`,
  `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'`,
  `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer` and, unless the
  handler sets its own, `Cache-Control: no-store`.
```

In the "Portal session" bullet, after "…logout degrades to a `client_id`-only end-session URL.", add: "The retained `id_token` reaches the browser at sign-out: `POST /v1/logout` and `POST /v1/me/sessions:revoke-all` return it as `id_token_hint` in `endSessionUrl`, so it lands in browser history and the provider's access logs."

- [ ] **Step 6: Run the focused tests**

```bash
go test ./internal/api/ -count=1 2>&1 | tail -3
go test ./internal/backend/ -count=1 2>&1 | tail -3
(cd web && npm run typecheck && npm run test:unit 2>&1 | tail -5)
for t in .github/tests/*.test.sh; do bash "$t" || exit 1; done
```

Expected: `ok` for both Go packages, the web unit tests pass, and every gate exits 0. `TestOpenAPIContract_*` stays green, because the schema bodies did not change.

- [ ] **Step 7: Commit**

```bash
git add internal/api/headers.go internal/api/headers_test.go internal/backend/wire.go internal/backend/backend_test.go internal/api/openapi.yaml internal/api/openapi_contract_test.go internal/api/auth.go web/src/api/generated/schema.d.ts web/tests/mock-api/auth.ts docs/security/threat-model.md
git commit -m "fix(api): pin browser security headers on /v1 and correct the cookie and id-token contract" -m "R1-10 (P3, TM-01): every app-listener response now carries nosniff, a framing-denying CSP, XFO DENY, Referrer-Policy no-referrer and a default Cache-Control no-store. R1-11 (P3, TM-01): the published contract names __Host-tcdi_session with SameSite=Lax and the real CSRF controls. R1-12 (P3, TM-20): the contract and comments state that sign-out returns the ID token as id_token_hint." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 8: Login with a valid session keeps it (R1-14) — tier: integrate

**Finding (R1-14, P3, TM-07; nearest B2/B3, TM-19).**
- **Attack.** Any web page sends a signed-in user on a top-level navigation to `https://<portal>/v1/login` (a link, `window.open` or `location=`). Lax cookies ride top-level GETs. With a live IdP SSO session, which is the normal case, the IdP redirects straight back without a prompt. The callback then runs with both the new login cookie and the old session cookie. It deletes the old session (`internal/api/auth.go:602-605`) and mints a new one for the same user. The victim's running desktop stream dies within one renew cycle, because its lease is bound to the deleted session (`liveLease`, `internal/broker/leases.go`). Their outstanding launch tickets fail. The open portal tab's CSRF token stops matching until it reloads `/v1/me`. Logout is POST plus CSRF, but this GET path reaches the same effect. Impact: nuisance-level denial of service, with no account confusion.
- **Required fix (brief).** In `LoginHandler`, when the request carries a session cookie that `Peek` validates, redirect to `PostLoginRedirect` without minting a new login. *Alternatively*, in the callback, delete the old session only when its principal (issuer, sub, tenant) differs from the new one. Keep `TestSessionFixationPrevented` green: a planted, invalid or foreign session is still deleted and rotated.
- **Security test (brief).** `TestLogin_ValidSessionShortCircuits` in `internal/api/auth_test.go`.

**Ruling: the callback alternative, not the `LoginHandler` short-circuit.** The brief's primary fix contradicts its own keep-green condition. `TestSessionFixationPrevented` (`auth_test.go:395-423`) plants a *valid* session (`Subject: "nobody"`) in the store and starts a login carrying it. A short-circuit on any valid session would answer that login with a redirect to `/`, keep the attacker's session in the victim's browser, and fail the test at `env.login`'s "did not redirect to issuer authorize". The callback rule handles both cases:
- a session of the *same* principal (issuer, subject, tenant) survives, so the desktop keeps running;
- any other session is deleted, as today.

The callback still mints a fresh session for the new login, so the browser's cookie rotates at every login, and the new session carries the groups the IdP just asserted. The old same-principal session lives on until its own idle or absolute expiry, or until sign-out-everywhere. The test is named for the behaviour that ships: `TestLogin_SamePrincipalKeepsLiveSession`.

**Files:**
- Modify: `internal/api/auth.go:600-605` (callback fixation block), `:584-590` (`CallbackHandler` doc comment)
- Test: `internal/api/auth_test.go` (after `TestSessionFixationPrevented`, line 423)
- Docs: `SECURITY.md:140-149` (rule 5), `docs/security/threat-model.md` §Boundary 2 "Portal session" bullet

**Interfaces:**
- Consumes: `testEnv.login(t, extraCookies ...*http.Cookie)` (`auth_test.go:173`), `testEnv.authedGet` (`auth_test.go:229`), `findCookie` (`auth_test.go:220`), `newTestEnv` (`auth_test.go:62`); `Principal{Issuer, Subject, TenantID}` (`principal.go:16`); oidctest default subject `user-1`, tenant `tenant-a`.
- Produces: `func samePrincipal(a, b Principal) bool` (unexported, `auth.go`). Callback behaviour: an existing session cookie whose session `Peek`s to the same (issuer, subject, tenant) is kept; anything else is deleted.

- [ ] **Step 1: Write the failing test**

Append to `internal/api/auth_test.go`, after `TestSessionFixationPrevented`:

```go
// TestLogin_SamePrincipalKeepsLiveSession (R1-14): a login that completes
// while the browser already holds a live session of the SAME principal —
// for example a cross-site page navigating the user to /v1/login with a
// live IdP SSO session — must not destroy that session: its desktop lease
// and CSRF token stay valid. The browser still gets a freshly minted
// session cookie. A different principal's session is still deleted
// (TestSessionFixationPrevented).
func TestLogin_SamePrincipalKeepsLiveSession(t *testing.T) {
	env := newTestEnv(t, nil)
	resp, cookies := env.login(t)
	resp.Body.Close()
	s1 := findCookie(cookies, env.auth.SessionCookieName())
	if s1 == nil {
		t.Fatal("first login set no session cookie")
	}

	resp2, cookies2 := env.login(t, s1)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusFound {
		t.Fatalf("second login callback = %d, want 302", resp2.StatusCode)
	}
	s2 := findCookie(cookies2, env.auth.SessionCookieName())
	if s2 == nil || s2.Value == s1.Value {
		t.Fatal("second login did not mint a fresh session cookie")
	}
	for name, c := range map[string]*http.Cookie{"S1 (pre-existing)": s1, "S2 (new)": s2} {
		r := env.authedGet(t, c, "/v1/me")
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("%s session on /v1/me = %d, want 200 — a same-principal login must not end the live session", name, r.StatusCode)
		}
	}
}
```

- [ ] **Step 2: Run it and see it fail**

Run: `go test ./internal/api/ -run 'TestLogin_SamePrincipalKeepsLiveSession|TestSessionFixationPrevented' -count=1 -v 2>&1 | grep -E '^(--- |ok|FAIL)|auth_test'`
Expected: `TestLogin_SamePrincipalKeepsLiveSession` FAILS with `S1 (pre-existing) session on /v1/me = 401, want 200`. `TestSessionFixationPrevented` PASSES.

- [ ] **Step 3: Implement**

In `internal/api/auth.go`, `CallbackHandler`, replace the fixation block:

```go
	// Session fixation: if the browser already carries a session cookie,
	// destroy that session. The new session always gets a fresh random ID.
	if old, err := r.Cookie(a.cfg.SessionCookieName); err == nil && old.Value != "" {
		_ = a.sessions.Delete(ctx, old.Value)
	}
```

with:

```go
	// Session fixation: a session cookie the browser already carries is
	// destroyed unless it is a live session of the principal that just
	// signed in. A planted, foreign, expired or unknown session dies here;
	// the same user's own session survives, so a login navigation a
	// cross-site page triggers (Lax cookies ride top-level GETs, and a live
	// IdP session answers without a prompt) cannot end their running
	// desktop. The new session always gets a fresh random ID either way.
	if old, err := r.Cookie(a.cfg.SessionCookieName); err == nil && old.Value != "" {
		prev, err := a.sessions.Peek(ctx, old.Value)
		if err != nil || !samePrincipal(prev.Principal, principal) {
			_ = a.sessions.Delete(ctx, old.Value)
		}
	}
```

Add after `CallbackHandler`:

```go
// samePrincipal reports whether two principals are the same account in the
// same tenant: the verified issuer, subject and tenant claim all match.
// Groups and display claims may differ between logins and do not count.
func samePrincipal(a, b Principal) bool {
	return a.Issuer == b.Issuer && a.Subject == b.Subject && a.TenantID == b.TenantID
}
```

In the `CallbackHandler` doc comment, replace "deletes any pre-existing session cookie (fixation)" with "deletes a pre-existing session cookie unless it is a live session of the same principal (fixation)".

The `Peek` here does not go through the Task 6 unknown-cookie budget (`lookupSession`). It runs only after a successful code exchange and ID-token verification, so it is not reachable by a cookie spray.

- [ ] **Step 4: Run the focused tests**

```bash
go test ./internal/api/ -count=1 2>&1 | tail -3
```

Expected: `ok`. `TestSessionFixationPrevented` (foreign valid session deleted), `TestOIDCLoginFlowSucceeds` and the revoke tests stay green.

- [ ] **Step 5: Docs**

- `SECURITY.md` rule 5 (line 140): after "Sessions use opaque IDs of at least 256 bits, rotated at login.", add: "A session cookie the browser already carries is deleted at login unless it is a live session of the same principal (issuer, subject, tenant); that one is kept, so a login navigation cannot end the user's running desktop." Add `TestLogin_SamePrincipalKeepsLiveSession` to its *Enforced:* list, after `TestSessionFixationPrevented`.
- `docs/security/threat-model.md`, §Boundary 2 "Portal session" bullet: append "At login, an existing session cookie is deleted unless it is a live session of the same principal, which is kept (`CallbackHandler`, `internal/api/auth.go`): a cross-site navigation to `/v1/login` cannot end the user's live desktop, and a planted or foreign session is still destroyed."

Run: `for t in .github/tests/*.test.sh; do bash "$t" || exit 1; done`. Expected: every script exits 0.

- [ ] **Step 6: Commit**

```bash
git add internal/api/auth.go internal/api/auth_test.go SECURITY.md docs/security/threat-model.md
git commit -m "fix(auth): a login started with a valid session keeps it" -m "R1-14 (P3, TM-07): the OIDC callback now deletes a pre-existing session cookie only when it is not a live session of the same principal (issuer, subject, tenant). A cross-site navigation to /v1/login can no longer end the user's running desktop. Planted or foreign sessions are still destroyed, and the cookie still rotates." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 9: Branding serves documented names only (R1-13) — tier: transcribe

**Finding (R1-13, P3, TM-05).**
- **Attack.** There is no escape: containment is correct. But TM-05 (`docs/security/threat-model.md:684`) says `serveBranding` "serve[s] fixed names only", and `build/frontend/main.go:288-317` serves *any* regular file inside the branding directory. That covers any file, any subdirectory, and the ConfigMap's `..data/` entries an operator places there. They are all publicly served on the portal origin. The proof wrote `sub/notes.txt` into the dir, and `GET /branding/sub/notes.txt` answered 200.
- **Required fix.** Reword TM-05 to "serves regular files inside the directory only", or, if fixed names are the intent, allowlist `branding.json`, `tokens.css`, `logo*.{svg,png}` and `favicon.{svg,ico}` in `serveBranding`.
- **Security test.** `TestBrandingDir_OnlyAllowlistedNames` (Plan ruling 4: the allowlist is enforced). It must fail on the current code.

**Ruling (Plan ruling 4): enforce the allowlist,** so TM-05's wording becomes true. The names are the ones `docs/branding.md` and the chart README document: `branding.json`, `tokens.css`, `favicon.svg`, `favicon.ico`, and logo files. Logo files are narrowed from "any regular file referenced from `branding.json`" to `logo*.svg` / `logo*.png`, where `*` is up to 64 of `[A-Za-z0-9_-]` (`logo.svg`, `logo-dark.svg` and `logo_2x.png` all match). The name is a single path segment, so subdirectories and `..data/` are never served.

**Files:**
- Modify: `build/frontend/main.go:272-317` (`serveBranding` and its doc comment; new `brandingNames` regexp and `brandingNameAllowed`)
- Test: `build/frontend/main_test.go` (new test after `TestBrandingDir_OtherNamesStay404`, line 413; `TestBrandingDir_NoTraversal` lines 306-330: the symlink name changes to an allowlisted one)
- Docs: `docs/branding.md:17-22,43-48`, `deploy/helm/tinycdi/README.md:225,238`

**Interfaces:**
- Consumes: `newTestHandlerBranding(t, sessionDomain, brandingDir string) http.Handler` (`main_test.go:46`), `newBrandingDir(t) string` (`:254`), `get(t, h, method, path) *http.Response` (`:71`).
- Produces: `func brandingNameAllowed(name string) bool` (package `main`, `build/frontend`). `/branding/<name>` answers 404 for every name outside `^(branding\.json|tokens\.css|favicon\.(svg|ico)|logo[A-Za-z0-9_-]{0,64}\.(svg|png))$`, whether or not the file exists. `/favicon.svg` and `/favicon.ico` keep their override.

- [ ] **Step 1: Write the failing test**

Append to `build/frontend/main_test.go`, after `TestBrandingDir_OtherNamesStay404`:

```go
// TestBrandingDir_OnlyAllowlistedNames (R1-13): /branding/ serves the
// documented names only — branding.json, tokens.css, favicon.svg/.ico and
// logo*.svg/.png — in the directory's top level. Any other file the
// operator (or a ConfigMap's ..data/ layout) puts there answers 404, even
// though it is a regular file inside the directory.
func TestBrandingDir_OnlyAllowlistedNames(t *testing.T) {
	dir := newBrandingDir(t) // branding.json, logo.svg, tokens.css
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("sub/notes.txt", "operator notes")
	write("notes.txt", "operator notes")
	write("..data/branding.json", `{"productName":"x"}`)
	write("logo-dark.png", "png-bytes")
	write("favicon.ico", "ico-bytes")
	write("logo.svg.bak", "backup")
	h := newTestHandlerBranding(t, "session.test", dir)

	for _, path := range []string{
		"/branding/sub/notes.txt",
		"/branding/notes.txt",
		"/branding/..data/branding.json",
		"/branding/logo.svg.bak",
	} {
		res := get(t, h, http.MethodGet, path)
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404 (not an allowlisted branding name)", path, res.StatusCode)
		}
	}
	for _, path := range []string{
		"/branding/branding.json",
		"/branding/tokens.css",
		"/branding/logo.svg",
		"/branding/logo-dark.png",
		"/branding/favicon.ico",
	} {
		res := get(t, h, http.MethodGet, path)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d, want 200", path, res.StatusCode)
		}
	}
}
```

- [ ] **Step 2: Run it and see it fail**

Run: `go test ./build/frontend/ -run TestBrandingDir_OnlyAllowlistedNames -count=1 2>&1 | tail -6`
Expected: FAIL with `/branding/sub/notes.txt: status 200, want 404`, the same for `/branding/notes.txt`, `/branding/..data/branding.json` and `/branding/logo.svg.bak`.

- [ ] **Step 3: Implement**

In `build/frontend/main.go`, add near `serveBranding` (`"regexp"` joins the imports if it is not already there):

```go
// brandingNames is the set of names /branding/ serves: the files the
// portal loads (branding.json, tokens.css), the favicon overrides, and
// logo files. A name is one path segment, so subdirectories and the
// ConfigMap's ..data/ entries never match.
var brandingNames = regexp.MustCompile(`^(branding\.json|tokens\.css|favicon\.(svg|ico)|logo[A-Za-z0-9_-]{0,64}\.(svg|png))$`)

// brandingNameAllowed reports whether name is a documented branding file.
func brandingNameAllowed(name string) bool { return brandingNames.MatchString(name) }
```

In `serveBranding`, directly after the `if !under || name == "" || hasDotDotSegment(name)` block, add:

```go
	if !brandingNameAllowed(name) {
		http.NotFound(w, r) // only the documented names are served (TM-05)
		return
	}
```

Rewrite the first sentence of `serveBranding`'s doc comment (line 272) as: "serveBranding serves the documented branding files — branding.json, tokens.css, favicon.svg/.ico and logo*.svg/.png (brandingNames) — from the branding directory (-branding-dir / TCDI_FRONTEND_BRANDING_DIR; the chart mounts a ConfigMap there)." Keep the rest.

In `TestBrandingDir_NoTraversal` (`main_test.go:306-330`), rename the escaping symlink from `evil.svg` to `logo-evil.svg` (both the `os.Symlink` target name and the `"/branding/evil.svg"` path). The test then still proves symlink containment, not just the allowlist.

- [ ] **Step 4: Run the package tests**

Run: `go test ./build/frontend/ -count=1 2>&1 | tail -3`
Expected: `ok`. `TestBrandingDir_ServesFiles`, `_NoTraversal`, `_FaviconOverride`, `_EmptyTokensFallback`, `_JSONAbsent` and `_OtherNamesStay404` stay green.

- [ ] **Step 5: Docs**

- `docs/branding.md`, the key table (lines 17-22): change the "logo files" row to `| logo files | named `logo*.svg` or `logo*.png` (for example `logo.svg`, `logo-dark.svg`), referenced from `branding.json`; served with the extension's content type |`.
- `docs/branding.md`, "Serving properties" (line 45): change "`/branding/` serves regular files only: no directory listing," to "`/branding/` serves only the names in the table above — `branding.json`, `tokens.css`, `favicon.svg`, `favicon.ico` and `logo*.svg`/`logo*.png` — as regular files in the directory's top level. Any other name, a subdirectory or a ConfigMap `..data/` entry answers 404. There is no directory listing,".
- `deploy/helm/tinycdi/README.md:225`: change "(regular files only," to "(only `branding.json`, `tokens.css`, `favicon.svg`/`.ico` and `logo*.svg`/`.png`, regular files only,". At line 238, change the logo row's text to "named `logo*.svg` or `logo*.png` (for example `logo.svg`) — served with their extension's content type".

Run: `for t in .github/tests/*.test.sh; do bash "$t" || exit 1; done`. Expected: every script exits 0 (the docs link and version gates included).

- [ ] **Step 6: Commit**

```bash
git add build/frontend/main.go build/frontend/main_test.go docs/branding.md deploy/helm/tinycdi/README.md
git commit -m "fix(frontend): serve only the documented branding file names" -m "R1-13 (P3, TM-05): /branding/ now answers 404 for every name other than branding.json, tokens.css, favicon.svg/.ico and logo*.svg/.png, so a stray file, a subdirectory or a ConfigMap ..data/ entry in the branding directory is never published on the portal origin. TM-05's fixed-names claim is now true." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 10: Admin-role revocation latency and the :apply contract (docs) (R1-17, R1-21) — tier: transcribe

**Findings (both doc-only).**
- **R1-17 (P3, TM-11).**
  - **Attack.** A tenant admin is removed from the admin group at the IdP, for example on a role change or offboarding. Their existing portal session keeps `IsTenantAdmin` for up to the absolute timeout (12 h by default). That carries tenant-wide read, stop/start, retained-disk attach/purge, quota, session-policy `:apply` and sign-out rights. The role is a pure function of the `groups` stored on the session row at login (`internal/store/sessions.go:93,113-124`), re-evaluated per request (`internal/api/middleware.go:117-122`) and never re-read from the IdP. `SECURITY.md:543-545` documents this latency for the login gate (`oidc.requiredGroups`) but not for the admin role, so an operator may assume group removal is immediate.
  - **Required fix.** Document, in `SECURITY.md` (deployment checklist, OIDC item) and in the TM-11 residual, that an admin-group removal takes effect at the user's next sign-in. The immediate remedy is `POST /v1/admin/tenants/{tenant}/users/{ownerRef}/sessions:revoke-all` by another tenant admin (`internal/api/auditroutes.go:95`).
  - **Security test.** `TestTenantAdminRole_DocumentedRevocationPath`, a docs test in `.github/tests`: the TM-11 residual and `SECURITY.md` name the sign-out remedy for a removed admin. It fails today.
- **R1-21 (P3, TM-17).**
  - **Attack.** A confirmed `{"dryRun":false}` call with a current `If-Match` restarts workspaces immediately, with no prior dry run. The dry run is a portal flow only (`internal/api/adminsessionpolicyapply.go:90` requires only `DryRun != nil`, and no server state links a confirm to an earlier dry run). This is within the tenant admin's privilege, so it is a doc-accuracy issue: TM-17 says ":apply … runs a dry run first", and `openapi.yaml:1238` says "Always dry run first".
  - **Required fix (Plan ruling 5: reword, no token).** TM-17's control becomes "the portal runs a dry run first; the API requires an explicit dryRun flag".
  - **Security test.** None in Go (a token was not adopted). The docs assertions below pin the corrected wording.

**This task creates `.github/tests/security-doc-claims.test.sh`.** Its helpers (`need`, `absent`, `need_row`) are defined in Global Constraints. Tasks 16, 18, 20, 25 and 29 append their own claims above its final `exit "$fails"` line. The script stands in for the brief's `TestTenantAdminRole_DocumentedRevocationPath`: each claim line is labelled with its finding id. Task 30 cites the script for these findings.

**Files:**
- Create: `.github/tests/security-doc-claims.test.sh`
- Modify: `SECURITY.md:532-538` (deployment checklist, OIDC item, `oidc.tenantAdminGroups` bullet)
- Modify: `docs/security/threat-model.md:690` (TM-11 Residual cell), `:696` (TM-17 Control and Residual cells)
- Modify: `internal/api/openapi.yaml:1235-1244` (`applyAdminSessionPolicy` description); regenerate `web/src/api/generated/schema.d.ts`
- Modify: `docs/runbooks/session-policy.md:129-131` (step 1, "Dry run")

**Interfaces:**
- Produces: `.github/tests/security-doc-claims.test.sh`, with:
  - `need <finding> <repo-relative file> <ERE>`: the file, flattened to one line (every whitespace run becomes one space), must match the ERE.
  - `absent <finding> <file> <ERE>`: the flattened file must not match.
  - `need_row <finding> <TM-NN> <ERE>`: the register row `| TM-NN | …` of `docs/security/threat-model.md` must match.

  Each failure prints `FAIL: <finding>: …` and adds 1 to `fails`. The script ends with `exit "$fails"`. Later tasks insert lines directly above that last line.

- [ ] **Step 1: Write the failing docs test**

Create `.github/tests/security-doc-claims.test.sh`:

```bash
#!/usr/bin/env bash
# security-doc-claims.test.sh — pins the security claims in prose that the
# v1.0 security review (docs/security/review-v1.0.md) corrected. Each line
# names its review finding and states what a document must say (need), must
# not say again (absent), or what a threat-model register row must carry
# (need_row). Files are flattened to one line with every whitespace run
# squeezed to one space, so a claim may wrap anywhere in the source.
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TM="docs/security/threat-model.md"
fails=0

flat() { tr -s '[:space:]' ' ' < "$ROOT/$1"; }

need() { # need <finding> <repo-relative file> <ERE>
  if [ ! -f "$ROOT/$2" ]; then
    echo "FAIL: $1: $2 does not exist"; fails=$((fails + 1)); return
  fi
  if ! flat "$2" | grep -qE -- "$3"; then
    echo "FAIL: $1: $2 does not state /$3/"; fails=$((fails + 1))
  fi
}

absent() { # absent <finding> <repo-relative file> <ERE>
  if [ ! -f "$ROOT/$2" ]; then
    echo "FAIL: $1: $2 does not exist"; fails=$((fails + 1)); return
  fi
  if flat "$2" | grep -qE -- "$3"; then
    echo "FAIL: $1: $2 still states /$3/"; fails=$((fails + 1))
  fi
}

need_row() { # need_row <finding> <TM-NN> <ERE>
  local row
  row="$(grep -E "^\| $2 \|" "$ROOT/$TM")"
  if [ -z "$row" ]; then
    echo "FAIL: $1: $TM has no register row $2"; fails=$((fails + 1)); return
  fi
  if ! printf '%s\n' "$row" | grep -qE -- "$3"; then
    echo "FAIL: $1: register row $2 does not state /$3/"; fails=$((fails + 1))
  fi
}

# R1-17: a removed tenant admin keeps the role until the next sign-in; the
# docs must say so and name the immediate remedy.
need_row R1-17 TM-11 'next sign-in'
need_row R1-17 TM-11 'users/\{ownerRef\}/sessions:revoke-all'
need R1-17 SECURITY.md 'admin group at the IdP takes effect at their next sign-in'
need R1-17 SECURITY.md 'users/\{ownerRef\}/sessions:revoke-all'

# R1-21: the server does not require a dry run before a confirmed :apply.
need_row R1-21 TM-17 'the API requires an explicit `dryRun` flag'
absent R1-21 "$TM" 'it runs a dry run first'
absent R1-21 internal/api/openapi.yaml 'Always dry run first'
need R1-21 internal/api/openapi.yaml 'The server does not require one'

exit "$fails"
```

- [ ] **Step 2: Run it and see it fail**

Run: `bash .github/tests/security-doc-claims.test.sh; echo "rc=$?"`
Expected: 8 `FAIL:` lines (every claim line), `rc=8`. Checked against `98c7f54`.

- [ ] **Step 3: Fix the docs**

`SECURITY.md`, deployment checklist item 1 (OIDC), `oidc.tenantAdminGroups` bullet: it ends "…the match is the same exact, no-full-path form as the login gate and the list must not be empty;". Replace that ending with:

```markdown
     no-full-path form as the login gate and the list must not be empty.
     The role follows the `groups` stored with the portal session at
     sign-in: removing someone from an admin group at the IdP takes effect
     at their next sign-in, up to the absolute timeout (12 h by default).
     To end it at once, another tenant admin signs them out with
     `POST /v1/admin/tenants/{tenant}/users/{ownerRef}/sessions:revoke-all`
     (per-user sign-out), which ends their portal sessions, leases and
     launch tickets in that tenant;
```

`docs/security/threat-model.md:690` (TM-11 row), Residual cell: it reads "A misconfigured list is a privilege grant: every member of a listed group gets tenant-wide read, stop/start, retained-disk attach/purge and admin rights." Append to it, inside the same cell:

```markdown
 The role follows the `groups` stored with the portal session at sign-in, so a removal from an admin group at the IdP takes effect at the user's next sign-in (up to the 12 h absolute timeout); the immediate remedy is another tenant admin's `POST /v1/admin/tenants/{tenant}/users/{ownerRef}/sessions:revoke-all`.
```

`docs/security/threat-model.md:696` (TM-17 row):
- In the Control cell, replace "it runs a dry run first;" with "the portal runs a dry run first, and the API requires an explicit `dryRun` flag but does not require an earlier dry run;".
- Append to the Residual cell: " A confirmed call with a current `If-Match` restarts at once, with no earlier dry run; that is within the tenant admin's privilege."

`internal/api/openapi.yaml`, `applyAdminSessionPolicy` description: replace "Always dry run first (`{"dryRun": true}` lists the affected workspaces and changes nothing); the confirmed call restarts each listed workspace" with:

```yaml
        allows (F3). Run a dry run first (`{"dryRun": true}` lists the
        affected workspaces and changes nothing); the portal always does.
        The server does not require one: every call must state `dryRun`,
        and a confirmed call (`{"dryRun": false}`) with a current
        `If-Match` restarts each affected workspace
```

Keep the line break layout consistent with the surrounding YAML block scalar, and keep the rest of the paragraph ("through the generation-fenced restart — …") unchanged. Then: `cd web && npm run generate && cd ..`.

`docs/runbooks/session-policy.md`, step 1 ("**Dry run.**", line 129): append "The portal always runs it before the confirm. The API does not enforce the order, so a scripted caller must run it itself."

- [ ] **Step 4: Run the gates**

```bash
bash .github/tests/security-doc-claims.test.sh; echo "rc=$?"
go test ./internal/api/ -run 'TestOpenAPI|TestApplySessionPolicy' -count=1 2>&1 | tail -2
for t in .github/tests/*.test.sh; do bash "$t" || exit 1; done
```

Expected: `rc=0` with no output, `ok`, and every gate exits 0 (including `threat-model-refs.test.sh`, because the register rows keep their cells, and `docs-gates.test.sh`).

- [ ] **Step 5: Commit**

```bash
git add .github/tests/security-doc-claims.test.sh SECURITY.md docs/security/threat-model.md internal/api/openapi.yaml web/src/api/generated/schema.d.ts docs/runbooks/session-policy.md
git commit -m "docs(security): state admin-role revocation latency and the :apply dry-run contract" -m "R1-17 (P3, TM-11): SECURITY.md and the TM-11 residual say an admin-group removal takes effect at the next sign-in and name the per-user sign-out as the immediate remedy. R1-21 (P3, TM-17): TM-17, the API contract and the runbook say the portal runs the dry run and the API only requires an explicit dryRun flag. New gate .github/tests/security-doc-claims.test.sh pins both." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 11: Foreign-owner tickets 404; NUL filters and cursors 400 (R1-18, R1-19) — tier: integrate

**Findings.**
- **R1-18 (P3, TM-04).**
  - **Attack.** A user of tenant A calls `POST /v1/workspaces/{id}/connections` (valid CSRF) on a workspace id owned by another tenant-A user. They get `403 FORBIDDEN` ("not allowed to connect to this workspace"), while an unknown or cross-tenant id gets `404`. This is an existence oracle, and it contradicts the threat-model line "A foreign workspace ID answers 404, never 403" (`docs/security/threat-model.md:206`). Impact is low: ids are 128-bit random, so the user must already know the id. `IssueTicket` returns `ErrNotFound` on a tenant mismatch but `ErrDenied` when `owner != p.Owner()` (`internal/broker/tickets.go:366-370`). `PublicIssueError` maps `ErrDenied` to `CodeForbidden` (`internal/broker/apishim.go:62-65`).
  - **Required fix.** Map the non-owner case to `ErrNotFound`, so the route matches every other id route.
  - **Security test.** `TestCreateConnection_ForeignOwnerIs404` in `internal/broker/tickets_test.go`, plus the API shim: same tenant, other owner → `api.CodeNotFound`.
- **R1-19 (P3, TM-04).**
  - **Attack.** A signed-in user sends `GET /v1/workspaces?phase=Running%00`, a name cursor whose key contains NUL, or a forged admin-users `lastSeen` cursor. The value reaches Postgres and fails (SQLSTATE 22021, or a parse error), and `writeBackendError`/`writeStoreError` answer 500. The client sees only the generic message, so nothing leaks. The cost is 5xx noise and error-budget alerts that any signed-in user can trigger. Locations: `internal/api/workspaces.go:433-434` (`?phase=` passed through), `internal/provisioning/service.go:382-384`, `internal/api/page.go:88-95` (cursor key not validated), `internal/store/users.go:73-79` (`lastSeen` cursor parsed in the store).
  - **Required fix.** Validate `phase` against the phase enum (400 otherwise). Reject cursor keys that are not valid UTF-8 or that contain NUL. Parse `lastSeen` cursor keys in the `ParsePage` consumers, answering 400.
  - **Security test.** `TestListWorkspaces_BadPhaseIs400` (`internal/api/workspaces_handlers_test.go`) and `TestAdminUsers_ForgedCursorIs400` (`internal/api/adminusers_test.go`).

**Files:**
- Modify: `internal/broker/tickets.go:368-370` (`IssueTicket` owner check), `:38-40` (`ErrDenied` comment)
- Test: `internal/broker/tickets_test.go` (after `TestIssueTicket_TenantAndOwnership`, line 128)
- Modify: `internal/api/page.go:88-95` (`ParsePage` cursor check; new `cursorTextOK`)
- Modify: `internal/api/workspaces.go:419-435` (`List`: phase check; new `validWorkspacePhase`)
- Modify: `internal/api/adminusers.go:60-68` (`List`: `lastSeen` cursor parse)
- Test: `internal/api/workspaces_handlers_test.go` (after `TestListWorkspaces_BadCursor`, line 858), `internal/api/adminusers_test.go` (after `TestAdminUsers_CursorCarriesSortKey`, line 117), `internal/api/page_test.go`

**Interfaces:**
- Consumes (broker tests): `setup(t) (db, b, clock, src)`, `seedWorkspace(t, db, tenantID, ownerSubject, wsUID)` (`internal/broker/main_test.go:170`), `readyBinding(ws, tenant, owner, gen, rtUID, now)`, `testPortalSession` (`main_test.go:182`), `alice`, `ctx` (`tickets_test.go:16-22`). API tests: `newWorkspaceEnv`, `newFakeBackend`, `defaultCatalog`, `defaultTenants`, `login`, `doReq` (`workspaces_handlers_test.go`), `errBody` (`data_test.go:390`), `newAdminUsersEnv`, `fakeUserDirectory`, `directoryUsers`, `adminUsersPath` (`adminusers_test.go`), `newFakeUserLimitSource`, `loginAdmin` (`adminquota_test.go:137`), `NextCursor(sort SortSpec, key, id string) string` (`page.go:44`).
- Produces:
  - `IssueTicket` returns `broker.ErrNotFound` for a same-tenant workspace the principal does not own. `ErrDenied` stays for a revoked generation (`tickets.go:380`).
  - `func cursorTextOK(s string) bool` (`internal/api/page.go`). `ParsePage` answers `400 INVALID_REQUEST "bad cursor"` when a cursor's key or id has invalid UTF-8 or a NUL.
  - `func validWorkspacePhase(s string) bool` (`internal/api/workspaces.go`). `GET /v1/workspaces?phase=` outside `Pending|Provisioning|Ready|Stopping|Stopped|Failed|Terminating` answers `400 INVALID_REQUEST`.
  - `GET /v1/admin/tenants/{t}/users?sort=±lastSeen&cursor=…` with a key that is not RFC 3339 answers `400 INVALID_REQUEST "bad cursor"` before any store read.

- [ ] **Step 1: Write the failing tests**

Append to `internal/broker/tickets_test.go`, after `TestIssueTicket_TenantAndOwnership`:

```go
// TestCreateConnection_ForeignOwnerIs404 (R1-18): a workspace owned by
// another user of the SAME tenant is indistinguishable from a missing one —
// IssueTicket answers ErrNotFound, and the public mapping is 404, never the
// 403 existence oracle.
func TestCreateConnection_ForeignOwnerIs404(t *testing.T) {
	db, b, clock, src := setup(t)
	carol := api.Principal{Issuer: "https://idp.example", Subject: "carol", TenantID: "tenant-a"}
	seedWorkspace(t, db, "tenant-a", carol.Owner(), "ws-carol")
	src.set(readyBinding("ws-carol", "tenant-a", carol.Owner(), 1, "rt-1", clock.Now()))

	_, err := b.IssueTicket(ctx, alice, "ws-carol", false, provisioning.SessionPolicy{}, testPortalSession)
	if !errors.Is(err, broker.ErrNotFound) {
		t.Fatalf("IssueTicket on a same-tenant foreign workspace = %v, want ErrNotFound", err)
	}
	if e := broker.PublicIssueError(err); e.Code != api.CodeNotFound {
		t.Fatalf("public mapping = %s, want %s", e.Code, api.CodeNotFound)
	}
}
```

Append to `internal/api/workspaces_handlers_test.go`, after `TestListWorkspaces_BadCursor`:

```go
// TestListWorkspaces_BadPhaseIs400 (R1-19): a phase filter outside the
// WorkspacePhase enum — a NUL byte, an unknown value, the wrong case — and
// a cursor whose key carries a NUL are client errors (400 INVALID_REQUEST),
// never a 500 from Postgres. A valid phase still filters.
func TestListWorkspaces_BadPhaseIs400(t *testing.T) {
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants())
	sess, csrf := login(t, env, "user-a")

	nulCursor := NextCursor(SortSpec{Field: "name"}, "a\x00b", "ws_x")
	for _, q := range []string{"phase=Running%00", "phase=Ready%00", "phase=Bogus", "phase=ready", "sort=name&cursor=" + nulCursor} {
		r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/workspaces?"+q, "", nil)
		if e := errBody(t, r); r.StatusCode != http.StatusBadRequest || e.Code != CodeInvalidRequest {
			t.Fatalf("%s: status=%d code=%s, want 400 INVALID_REQUEST", q, r.StatusCode, e.Code)
		}
	}
	for _, q := range []string{"phase=Ready", "phase=Stopped", "phase=Terminating"} {
		r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/workspaces?"+q, "", nil)
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("%s: status=%d, want 200", q, r.StatusCode)
		}
	}
}
```

Append to `internal/api/adminusers_test.go`, after `TestAdminUsers_CursorCarriesSortKey`:

```go
// TestAdminUsers_ForgedCursorIs400 (R1-19): a forged lastSeen cursor whose
// key is not a timestamp, and a cursor carrying a NUL, are refused 400
// before the store is called — never a 500 from the store's own parse or
// from Postgres.
func TestAdminUsers_ForgedCursorIs400(t *testing.T) {
	dir := &fakeUserDirectory{users: directoryUsers()}
	env := newAdminUsersEnv(t, dir, newFakeUserLimitSource())
	sess, csrf := loginAdmin(t, env, "admin-a")
	for _, tc := range []struct{ name, sort, cursor string }{
		{"lastSeen key not a time", "-lastSeen", NextCursor(SortSpec{Field: "lastSeen", Desc: true}, "not-a-time", "https://idp|bob")},
		{"NUL in key", "ownerRef", NextCursor(SortSpec{Field: "ownerRef"}, "https://idp|b\x00ob", "https://idp|bob")},
		{"NUL in id", "ownerRef", NextCursor(SortSpec{Field: "ownerRef"}, "https://idp|bob", "https://idp|b\x00ob")},
	} {
		dir.gotPage = store.UserPage{}
		r := doReq(t, env, sess, csrf, http.MethodGet,
			adminUsersPath+"?sort="+tc.sort+"&cursor="+url.QueryEscape(tc.cursor), "", nil)
		if e := errBody(t, r); r.StatusCode != http.StatusBadRequest || e.Code != CodeInvalidRequest {
			t.Fatalf("%s: status=%d code=%s, want 400 INVALID_REQUEST", tc.name, r.StatusCode, e.Code)
		}
		if dir.gotPage.Limit != 0 {
			t.Fatalf("%s: the store was called with %+v", tc.name, dir.gotPage)
		}
	}
}
```

Append to `internal/api/page_test.go` (add `"net/http"` to its imports; `httptest` is already there):

```go
// TestParsePage_CursorTextMustBePostgresSafe (R1-19): a cursor is decoded
// from client input, so a key or id Postgres cannot store as text (a NUL
// byte) is a bad cursor for every ParsePage consumer.
func TestParsePage_CursorTextMustBePostgresSafe(t *testing.T) {
	for _, c := range []struct{ key, id string }{{"a\x00", "ws_x"}, {"a", "ws_\x00"}} {
		cur := NextCursor(SortSpec{Field: "name"}, c.key, c.id)
		r := httptest.NewRequest(http.MethodGet, "/v1/x?sort=name&cursor="+cur, nil)
		if _, err := ParsePage(r, []string{"name"}, "name"); err == nil || err.Code != CodeInvalidRequest {
			t.Fatalf("cursor key=%q id=%q: err=%v, want INVALID_REQUEST", c.key, c.id, err)
		}
	}
	ok := NextCursor(SortSpec{Field: "name"}, "Desktop – é", "ws_x")
	if _, err := ParsePage(httptest.NewRequest(http.MethodGet, "/v1/x?sort=name&cursor="+ok, nil), []string{"name"}, "name"); err != nil {
		t.Fatalf("valid UTF-8 cursor refused: %v", err)
	}
}
```

- [ ] **Step 2: Run them and see them fail**

```bash
go test ./internal/broker/ -run TestCreateConnection_ForeignOwnerIs404 -count=1 2>&1 | tail -3
go test ./internal/api/ -run 'TestListWorkspaces_BadPhaseIs400|TestAdminUsers_ForgedCursorIs400|TestParsePage_CursorTextMustBePostgresSafe' -count=1 2>&1 | tail -6
```

Expected:
- broker: `IssueTicket on a same-tenant foreign workspace = broker: denied, want ErrNotFound` (Docker; a skip is not a pass);
- api: `phase=Running%00: status=200 …, want 400 INVALID_REQUEST`, `lastSeen key not a time: status=200 …`, and `cursor key="a\x00" id="ws_x": err=<nil>, want INVALID_REQUEST`.

- [ ] **Step 3: Implement**

`internal/broker/tickets.go`, `IssueTicket`: replace

```go
	if owner != p.Owner() {
		return Ticket{}, ErrDenied
	}
```

with:

```go
	// Another user's workspace in the same tenant is indistinguishable
	// from a missing one (TM-04): 404, never a 403 existence oracle.
	if owner != p.Owner() {
		return Ticket{}, ErrNotFound
	}
```

Update the `IssueTicket` doc comment (line 332): "Fails with ErrNotFound when the workspace is unknown, in another tenant or owned by someone else, and with ErrDenied when the principal's generation was revoked".

`internal/api/page.go`: add `"unicode/utf8"` to the imports and, below `ParsePage`:

```go
// cursorTextOK reports whether a decoded cursor field can reach Postgres as
// text: valid UTF-8 without a NUL byte. Anything else would fail in the
// database (SQLSTATE 22021) and surface as a 500; it is a bad cursor.
func cursorTextOK(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}
```

In `ParsePage`, extend the cursor check:

```go
		if err != nil || json.Unmarshal(raw, &c) != nil || c.ID == "" || c.Sort != s.String() ||
			!cursorTextOK(c.Key) || !cursorTextOK(c.ID) {
			return p, NewError(CodeInvalidRequest, "bad cursor")
		}
```

`internal/api/workspaces.go`: add `"slices"` to the imports, and add near `List`:

```go
// workspacePhases is the WorkspacePhase enum (openapi WorkspacePhase,
// api/v1 validation.go) — the only values the ?phase= filter accepts.
var workspacePhases = []cdiv1.WorkspacePhase{
	cdiv1.WorkspacePhasePending, cdiv1.WorkspacePhaseProvisioning, cdiv1.WorkspacePhaseReady,
	cdiv1.WorkspacePhaseStopping, cdiv1.WorkspacePhaseStopped, cdiv1.WorkspacePhaseFailed,
	cdiv1.WorkspacePhaseTerminating,
}

// validWorkspacePhase reports whether s is exactly one WorkspacePhase value.
func validWorkspacePhase(s string) bool {
	return slices.Contains(workspacePhases, cdiv1.WorkspacePhase(s))
}
```

In `List`, directly after the `ParsePage` error check, add:

```go
	phase := r.URL.Query().Get("phase")
	if phase != "" && !validWorkspacePhase(phase) {
		writeError(w, r, CodeInvalidRequest,
			"phase must be one of Pending, Provisioning, Ready, Stopping, Stopped, Failed, Terminating")
		return
	}
```

Then pass `phase` instead of `r.URL.Query().Get("phase")` to `h.backend.ListWorkspaces`.

`internal/api/adminusers.go`, `List`: directly after the `ParsePage` error check, add:

```go
	// The lastSeen sort keys on a timestamp the store parses; a forged key
	// is the client's error, refused before any read (never a 500).
	if page.After != nil && page.Sort.Field == "lastSeen" {
		if _, err := time.Parse(time.RFC3339Nano, page.After.Key); err != nil {
			WriteError(w, r, NewError(CodeInvalidRequest, "bad cursor"))
			return
		}
	}
```

- [ ] **Step 4: Run the focused tests**

```bash
go test ./internal/broker/ -count=1 2>&1 | tail -3
go test ./internal/api/ -count=1 2>&1 | tail -3
KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path --bin-dir "$PWD/bin")" go test -tags=integration -count=1 -run 'Connection|Ticket' ./tests/integration/ 2>&1 | tail -3
```

Expected: `ok` for all three. `TestIssueTicket_TenantAndOwnership` accepts `ErrNotFound`, `TestListWorkspaces_BadCursor` and `TestAdminUsers_CursorCarriesSortKey` stay green, and `rg -n 'ErrDenied' internal/broker/*_test.go tests/integration` finds no test that expects `ErrDenied` for a foreign owner. If one does, change it to `ErrNotFound`.

- [ ] **Step 5: Docs**

`internal/api/openapi.yaml`, `GET /v1/workspaces` `phase` parameter (line 308): change `description: Filter by lifecycle phase.` to `description: Filter by lifecycle phase; a value outside the WorkspacePhase enum is 400 INVALID_REQUEST.` Then `cd web && npm run generate && cd ..`.

The threat-model sentence "A foreign workspace ID answers 404, never 403." (`docs/security/threat-model.md:206`) is now true for the connection route. Leave it unchanged.

Run: `for t in .github/tests/*.test.sh; do bash "$t" || exit 1; done`. Expected: every script exits 0.

- [ ] **Step 6: Commit**

```bash
git add internal/broker/tickets.go internal/broker/tickets_test.go internal/api/page.go internal/api/page_test.go internal/api/workspaces.go internal/api/workspaces_handlers_test.go internal/api/adminusers.go internal/api/adminusers_test.go internal/api/openapi.yaml web/src/api/generated/schema.d.ts
git commit -m "fix(api): foreign-owner tickets answer 404 and NUL filters answer 400" -m "R1-18 (P3, TM-04): a connection request for another same-tenant user's workspace now answers 404 like every other id route, closing the 403 existence oracle. R1-19 (P3, TM-04): the phase filter is validated against the enum, cursors carrying NUL or invalid UTF-8 are bad cursors, and a forged lastSeen cursor is refused 400 before the store is read." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 12: Every refused ticket redemption consumes the ticket (R1-25) — tier: integrate

**Finding (R1-25, P3, new: Boundary 5 launch ticket; nearest TM-23).** `RedeemTicket` runs the consume UPDATE and every later check in one `WithTx`. A refusal after the consume (expired, generation revoked, dead portal session, live lease without takeover, lease-insert conflict) returns an error from the closure, so `WithTx` rolls the consume back.
- Attack: a non-takeover ticket refused with 409 `CONNECTION_IN_USE` stays redeemable. Anyone holding it, including an attacker who captured the POST body, can redeem it once the live lease ends inside the 60 s TTL. In merged mode each replica uses its own clock, so an "expired" refusal on a replica with a fast clock leaves the ticket redeemable on a replica with a slow clock.
- Docs and `TestRedeemTicket_ExpiredStillConsumed` claim "consumed on first attempt". That test is vacuous: its second attempt fails only because the ticket is still expired.
- Required fix: record the refusal in a local variable and return `nil` from the transaction closure after the consume (the pattern `ClaimTransfer` uses with `refusal`). The consume then commits, and the refusal is returned after the commit. Invariant: once a redemption attempt passes the audience check, `consumed_at` is set whatever the outcome. The audience mismatch is the only intended non-consuming refusal.
- Security test: `TestRedeemTicket_EveryRefusalConsumes` in `internal/broker/`. It is table-driven over expired, generation revoked, portal session deleted and live lease without takeover. Each case asserts `consumed_at IS NOT NULL` after the refusal, and asserts that a second attempt fails after the blocking condition is cleared.

**Files:**
- Modify: `internal/broker/tickets.go:18` (the `pgconn` import goes), `:444-447` (`RedeemTicket` doc comment), `:453-617` (`RedeemTicket` body)
- Create: `internal/broker/ticket_consume_test.go`
- Modify: `internal/broker/signout_test.go:108-130` (`TestRedeemTicket_DeadPortalSessionDenied`: the replay now fails as already used)
- Modify: `docs/security/threat-model.md:300-301` (Boundary 5 "Launch ticket" bullet)

**Interfaces:**
- Consumes: package `broker_test` helpers `setup`, `seedWorkspace`, `readyBinding`, `seedPortalSession`, `deletePortalSession`, `testPortalSession` (`main_test.go:170-285`); `ctx`, `alice`, `gwA` (`tickets_test.go:15-25`); `(*fakeClock).Advance` accepts a negative duration.
- Produces: `RedeemTicket` keeps its signature `func (b *Broker) RedeemTicket(ctx context.Context, gw GatewayIdentity, opaque string) (Lease, error)` and its sentinel errors. Behaviour change: after a refusal that follows the consume, a replay returns `ErrTicketInvalid` (consumed), not the original refusal. The gateway already maps both to a launch refusal, so no caller changes.

- [ ] **Step 1: Write the failing test**

Create `internal/broker/ticket_consume_test.go`:

```go
package broker_test

import (
	"errors"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// ticketConsumed reports whether the ticket's consumed_at is set.
func ticketConsumed(t *testing.T, db *store.DB, opaque string) bool {
	t.Helper()
	var consumed bool
	if err := db.Pool().QueryRow(ctx,
		`SELECT consumed_at IS NOT NULL FROM launch_ticket WHERE ticket_hash = sha256(convert_to($1, 'UTF8'))`,
		opaque).Scan(&consumed); err != nil {
		t.Fatalf("ticket row: %v", err)
	}
	return consumed
}

// TestRedeemTicket_EveryRefusalConsumes (R1-25): once a redemption attempt
// passes the audience check, the ticket is consumed whatever the outcome —
// the refusal commits the consume. A second attempt fails even after the
// blocking condition is cleared.
func TestRedeemTicket_EveryRefusalConsumes(t *testing.T) {
	cases := []struct {
		name  string
		block func(t *testing.T, db *store.DB, clock *fakeClock) (want error, clear func())
	}{
		{"expired", func(t *testing.T, _ *store.DB, clock *fakeClock) (error, func()) {
			clock.Advance(broker.TicketTTL + time.Second)
			return broker.ErrTicketExpired, func() { clock.Advance(-(broker.TicketTTL + time.Second)) }
		}},
		{"generation revoked", func(t *testing.T, db *store.DB, clock *fakeClock) (error, func()) {
			if _, err := db.Pool().Exec(ctx,
				`INSERT INTO workspace_revocation (workspace_id, runtime_generation, revoked_at) VALUES ('ws-1', 1, $1)`,
				clock.Now()); err != nil {
				t.Fatalf("seed revocation: %v", err)
			}
			return broker.ErrRevoked, func() {
				if _, err := db.Pool().Exec(ctx, `DELETE FROM workspace_revocation WHERE workspace_id = 'ws-1'`); err != nil {
					t.Fatalf("clear revocation: %v", err)
				}
			}
		}},
		{"portal session deleted", func(t *testing.T, db *store.DB, _ *fakeClock) (error, func()) {
			deletePortalSession(t, db, testPortalSession)
			return broker.ErrRevoked, func() { seedPortalSession(t, db, testPortalSession) }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, b, clock, src := setup(t)
			seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
			src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
			tk, err := b.IssueTicket(ctx, alice, "ws-1", false, provisioning.SessionPolicy{}, testPortalSession)
			if err != nil {
				t.Fatalf("IssueTicket: %v", err)
			}
			want, clear := tc.block(t, db, clock)
			if _, err := b.RedeemTicket(ctx, gwA, tk.Token); !errors.Is(err, want) {
				t.Fatalf("refused redeem = %v, want %v", err, want)
			}
			if !ticketConsumed(t, db, tk.Token) {
				t.Fatal("refused redeem left consumed_at NULL: the consume rolled back with the refusal")
			}
			clear()
			src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
			if _, err := b.RedeemTicket(ctx, gwA, tk.Token); !errors.Is(err, broker.ErrTicketInvalid) {
				t.Fatalf("second attempt = %v, want ErrTicketInvalid (already consumed)", err)
			}
		})
	}

	t.Run("live lease without takeover", func(t *testing.T) {
		db, b, clock, src := setup(t)
		seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
		src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
		t1, err := b.IssueTicket(ctx, alice, "ws-1", false, provisioning.SessionPolicy{}, testPortalSession)
		if err != nil {
			t.Fatalf("IssueTicket t1: %v", err)
		}
		t2, err := b.IssueTicket(ctx, alice, "ws-1", false, provisioning.SessionPolicy{}, testPortalSession)
		if err != nil {
			t.Fatalf("IssueTicket t2: %v", err)
		}
		l1, err := b.RedeemTicket(ctx, gwA, t1.Token)
		if err != nil {
			t.Fatalf("redeem t1: %v", err)
		}
		if _, err := b.RedeemTicket(ctx, gwA, t2.Token); !errors.Is(err, broker.ErrConnectionInUse) {
			t.Fatalf("redeem t2 while t1's lease lives = %v, want ErrConnectionInUse", err)
		}
		if !ticketConsumed(t, db, t2.Token) {
			t.Fatal("refused redeem left t2 unconsumed")
		}
		if err := b.RevokeLease(ctx, l1.ID); err != nil {
			t.Fatalf("RevokeLease: %v", err)
		}
		if l2, err := b.RedeemTicket(ctx, gwA, t2.Token); !errors.Is(err, broker.ErrTicketInvalid) {
			t.Fatalf("t2 second attempt = lease %q, err %v; want ErrTicketInvalid", l2.ID, err)
		}
	})
}
```

`launch_ticket.ticket_hash` is `sha256(opaque)` (`tickets.go:301-304`), so the SQL `sha256(convert_to($1,'UTF8'))` addresses the same row. Both tickets in the last case are issued before `t1` is redeemed, because `IssueTicket` itself refuses while a live lease exists (`tickets.go:405`).

- [ ] **Step 2: Run it and see it fail**

Run: `go test ./internal/broker/ -run TestRedeemTicket_EveryRefusalConsumes -count=1 2>&1 | grep -E '^\s*(---|ok|FAIL|\S+_test.go)'`
Expected (Docker required; a skip is not a pass): FAIL in all four subtests, with `refused redeem left consumed_at NULL: the consume rolled back with the refusal` (and `refused redeem left t2 unconsumed`).

- [ ] **Step 3: Implement the refusal-after-consume pattern**

In `internal/broker/tickets.go` `RedeemTicket`:

1. Directly after `var lease Lease`, add:

```go
	// refusal is a redemption refused AFTER the consume UPDATE: the closure
	// returns nil so the consume commits, and the refusal is returned once
	// the transaction is done (the ClaimTransfer pattern). Every attempt
	// that passes the audience check therefore spends the ticket.
	var refusal error
```

2. Leave the three refusals before the consume as they are (`ErrTicketInvalid` on no row, `ErrRevoked` on `revokedAt != nil`, `ErrTicketInvalid` on `consumedAt != nil`, `ErrDenied` on the audience mismatch). Nothing has been written yet when they return.

3. Change every refusal after the consume UPDATE to set `refusal` and return `nil`:
   - `if !now.Before(expiresAt) { return ErrTicketExpired }` → `{ refusal = ErrTicketExpired; return nil }`
   - `if revoked { return ErrRevoked }` → `{ refusal = ErrRevoked; return nil }`
   - `if !alive { return ErrRevoked }` → `{ refusal = ErrRevoked; return nil }`
   - in the live-lease `switch`, `default: return ErrConnectionInUse` → `default: refusal = ErrConnectionInUse; return nil`

4. Replace the lease INSERT and its 23505 branch. A unique violation aborts the Postgres transaction, so the consume could not commit after it. `ON CONFLICT DO NOTHING` keeps the transaction alive instead. The conflicts it can hit are `connection_lease_one_active` (the partial unique index: a concurrent redemption won) and a primary-key collision of a 128-bit random id. Both mean "no new lease here".

```go
		tag, err := tx.Exec(ctx,
			`INSERT INTO connection_lease
				(id, workspace_id, tenant_id, principal_subject,
				 runtime_generation, runtime_uid, fencing_version, gateway_id,
				 state, created_at, expires_at, last_renewed_at,
				 portal_session_digest)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'active', $9, $10, $9, $11)
			 ON CONFLICT DO NOTHING`,
			lease.ID, wsUID, tenantID, subject, int64(gen), runtimeUID,
			int64(fencing), gw.ID, now, lease.ExpiresAt, portalSession)
		if err != nil {
			return fmt.Errorf("broker: insert lease: %w", err)
		}
		if tag.RowsAffected() == 0 {
			// A concurrent redemption holds the workspace's single active
			// lease (partial unique index). DO NOTHING keeps the
			// transaction alive, so the consume still commits.
			refusal = ErrConnectionInUse
			return nil
		}
		return nil
	})
	if err != nil {
		return Lease{}, err
	}
	if refusal != nil {
		return Lease{}, refusal
	}
	return lease, nil
```

5. Delete the now-unused import `"github.com/jackc/pgx/v5/pgconn"` (line 18). `go build ./internal/broker/` must succeed.

6. Rewrite the doc comment's first sentence (lines 444-447) as: "RedeemTicket consumes the ticket on every attempt that passes the audience check — an expired, revoked or refused attempt included, because a refusal after the consume commits it — and claims the workspace's single active lease in the same transaction, bound to the incarnation recorded at issuance. A ticket never redeems twice, and a revoked ticket never redeems. On success it returns the new Lease."

- [ ] **Step 4: Fix the test that pinned the rollback**

`TestRedeemTicket_DeadPortalSessionDenied` (`internal/broker/signout_test.go:108-130`) loops two redemptions and expects `ErrRevoked` both times. That second `ErrRevoked` only held because the consume rolled back. Replace the doc comment's last sentence ("Denial is stable: while the session stays dead every replay fails the same way.") with "Denial is final: the refused attempt consumes the ticket, so a replay is refused as already used." Replace the loop with:

```go
	if _, err := b.RedeemTicket(ctx, gwA, tk.Token); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("redeem under dead session = %v, want ErrRevoked", err)
	}
	if _, err := b.RedeemTicket(ctx, gwA, tk.Token); !errors.Is(err, broker.ErrTicketInvalid) {
		t.Fatalf("replay under dead session = %v, want ErrTicketInvalid (consumed)", err)
	}
```

- [ ] **Step 5: Run the broker and gateway tests**

Run:
```bash
go test ./internal/broker/... -count=1 > /tmp/t12.log 2>&1; echo rc=$?; grep -E '^(--- FAIL|ok|FAIL)|_test.go:' /tmp/t12.log | head
go test ./internal/gateway/... ./internal/backend/ -count=1 -run 'Launch|Redeem|Ticket' 2>&1 | grep -E '^(--- FAIL|ok|FAIL)'
```
Expected: `ok` for every package (the whole broker package passed in about 50 s on the planning host). `TestRedeemTicket_ExpiredStillConsumed` stays green and is no longer vacuous.

- [ ] **Step 6: Docs**

`docs/security/threat-model.md`, Boundary 5 "Launch ticket" bullet (lines 300-301): replace "redeemed atomically (an expired ticket is consumed too)" with "consumed by every redemption attempt that passes the gateway-audience check, whatever its outcome (an expired, revoked or refused attempt commits the consume; `TestRedeemTicket_EveryRefusalConsumes`)". `TestRedeemTicket_EveryRefusalConsumes` now exists, so `threat-model-refs.test.sh` resolves it.

- [ ] **Step 7: Gates**

Run: `for t in .github/tests/*.test.sh; do bash "$t" >/dev/null || echo "FAIL $t"; done`
Expected: no output.

- [ ] **Step 8: Commit**

```bash
git add internal/broker/tickets.go internal/broker/ticket_consume_test.go internal/broker/signout_test.go docs/security/threat-model.md
git commit -m "fix(broker): every refused ticket redemption consumes the ticket

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 13: Upstream 1xx cannot strip pinned session headers (R1-04) — tier: design

**Finding (R1-04, P2, TM-29).** The attacker is the runtime pod, which is tenant-controlled: the user, or malware in the desktop, can replace whatever answers on the pod's `:8443`. The pod answers an allowlisted desktop path (for example `/vnc.html`) with `103 Early Hints`, then its real `200 text/html`.
- What happens: Go 1.26.9's `httputil.ReverseProxy` relays the 1xx through its `Got1xxResponse` hook (`net/http/httputil/reverseproxy.go:560-575`). The hook copies the 1xx headers into `rw.Header()`, writes the 1xx, then calls `clear(h)`. That wipes the CSP, COOP, CORP, Permissions-Policy, HSTS, nosniff and Origin-Agent-Cluster headers that `ServeHTTP` pinned. `responsePolicy` had already deleted the upstream copies, so the final response carries none of them. The 1xx headers (`Link`, `Set-Cookie`, a weak CSP) also reach the browser without passing through `responsePolicy`.
- Gain: without `Permissions-Policy: camera=(), microphone=(), …`, pod JavaScript on the platform's session origin can raise camera, microphone and geolocation prompts, and COOP/CORP isolation is lost. The impact stays on the workspace's own origin, in the owner's browser.
- Required fix: two changes, so the pinned set can never be lost. (1) The final response always carries the full pinned header set, even after `rw.Header()` was cleared. (2) Upstream 1xx responses are not relayed: `WriteHeader(c)` for 100–199, other than 101, is a no-op. Invariant: every proxied response carries exactly the gateway's policy headers, and no upstream informational response reaches the browser.
- Security test: `TestResponsePolicy_Upstream1xxCannotStripPinnedHeaders`. A hostile TLS upstream sends 103 (and, in a second case, 100) with `Link`, `Set-Cookie` and CSP, then 200. Assert the full pinned CSP (`frame-ancestors`, `connect-src wss://<host>`), COOP, CORP, `Permissions-Policy camera=()`, nosniff and HSTS on the final response, and that the client saw no 1xx. It fails today with every header empty.

**Design note (why not the brief's literal fix (1)).** The brief proposes writing the pinned set onto `resp.Header` in `responsePolicy`. On a normal response (no 1xx), `ReverseProxy` merges `resp.Header` into `rw.Header()` with `Add`, and `rw.Header()` already holds the pinned set, so every pinned header would go out twice. Instead, the writer that `serveProxy` hands to `g.proxy.ServeHTTP` (the existing `hijackCapture`) re-pins the set with `Set` at the first final `WriteHeader`. That one place covers every exit: the proxied response, the `ErrorHandler`'s 502, and a response after a dropped 1xx. `Set` never duplicates. The same writer drops 1xx other than 101. A 101 upgrade is not affected: `ReverseProxy` writes the 101 itself on the hijacked conn.

**Files:**
- Modify: `internal/gateway/proxy.go:396-403` (`ServeHTTP`: the seven `h.Set` lines become one `pinSessionHeaders` call), `:761-763` (`serveProxy`: `hijackCapture` gets `pin`), `:1243` (new `pinSessionHeaders` above `responsePolicy`), `:1264-1272` (`hijackCapture` gains `pin`, `wrote`, `WriteHeader`, `Write`)
- Create: `internal/gateway/proxy_1xx_test.go`
- Modify: `docs/security/threat-model.md:340-345` (Boundary 5 "Response policy" bullet)

**Interfaces:**
- Consumes: `sessionCSP(host string, embedders []string) string`, `g.permissions`, `g.embedders` (`proxy.go`); test helpers in package `gateway_test`: `newFakeBroker`, `(*fakeBroker).scriptTicket`, `(*fakeBroker).pointLeaseAt`, `newGateway`, `launchOK`, `upgrade`, `drain`, `testHost`, `testOrigin`, `testWSUID`, `gateway.SessionCookieName` (`helpers_test.go`).
- Produces: `func (g *Gateway) pinSessionHeaders(h http.Header, cspHost string)` (unexported; Task 16 does not need it). `hijackCapture` fields `pin func(http.Header)` and `wrote bool`, and methods `WriteHeader(int)` and `Write([]byte) (int, error)`.

- [ ] **Step 1: Write the failing test (and the Review Focus 2 guard)**

Create `internal/gateway/proxy_1xx_test.go`:

```go
package gateway_test

import (
	"crypto/sha1"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

// hostile1xxUpstream answers every request with an informational response
// carrying hostile headers, then a 200 HTML page — the runtime pod is
// tenant-controlled and may do exactly this.
func hostile1xxUpstream(t *testing.T, code int) *httptest.Server {
	t.Helper()
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Link", "</app/x.js>; rel=preload; as=script")
		h.Set("Set-Cookie", "planted=1; Path=/")
		h.Set("Content-Security-Policy", "default-src *")
		w.WriteHeader(code)
		h.Del("Link")
		h.Del("Set-Cookie")
		h.Del("Content-Security-Policy")
		h.Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<script>/* runtime page */</script>"))
	}))
	t.Cleanup(up.Close)
	return up
}

// TestResponsePolicy_Upstream1xxCannotStripPinnedHeaders (R1-04): an
// upstream 1xx must neither reach the browser nor erase the security
// headers ServeHTTP pins on every session-origin response.
func TestResponsePolicy_Upstream1xxCannotStripPinnedHeaders(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
	}{{"103 early hints", http.StatusEarlyHints}, {"100 continue", http.StatusContinue}} {
		t.Run(tc.name, func(t *testing.T) {
			fb := newFakeBroker(t)
			fb.scriptTicket("tk-1xx", testWSUID)
			fb.pointLeaseAt(t, "tk-1xx", hostile1xxUpstream(t, tc.code))
			srv := newGateway(t, fb, nil)
			cookie := launchOK(t, srv, testHost, "tk-1xx")

			req, err := http.NewRequest(http.MethodGet, srv.URL+"/vnc.html", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = testHost
			req.Header.Set("Cookie", gateway.SessionCookieName+"="+cookie)
			var infos atomic.Int32
			req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
				Got1xxResponse: func(int, textproto.MIMEHeader) error { infos.Add(1); return nil },
			}))
			resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
			if err != nil {
				t.Fatalf("roundtrip: %v", err)
			}
			defer drain(resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if n := infos.Load(); n != 0 {
				t.Errorf("client saw %d informational responses, want 0", n)
			}
			csp := resp.Header.Get("Content-Security-Policy")
			if !strings.Contains(csp, "frame-ancestors") || !strings.Contains(csp, "connect-src 'self' wss://"+testHost) {
				t.Errorf("CSP = %q, want the pinned session CSP", csp)
			}
			if got := resp.Header.Values("Content-Security-Policy"); len(got) != 1 {
				t.Errorf("CSP values = %q, want exactly one", got)
			}
			for name, want := range map[string]string{
				"Cross-Origin-Opener-Policy":   "same-origin",
				"Cross-Origin-Resource-Policy": "same-origin",
				"X-Content-Type-Options":       "nosniff",
				"Origin-Agent-Cluster":         "?1",
				"Strict-Transport-Security":    "max-age=63072000; includeSubDomains",
			} {
				if got := resp.Header.Values(name); len(got) != 1 || got[0] != want {
					t.Errorf("%s = %q, want exactly [%q]", name, got, want)
				}
			}
			if pp := resp.Header.Get("Permissions-Policy"); !strings.Contains(pp, "camera=()") {
				t.Errorf("Permissions-Policy = %q, want camera=()", pp)
			}
			for _, h := range []string{"Link", "Set-Cookie"} {
				if v := resp.Header.Get(h); v != "" {
					t.Errorf("%s = %q reached the client", h, v)
				}
			}
		})
	}
}

// TestProxy_WebSocketUpgradeSurvives1xxGuard (Review Focus 2): the 1xx
// guard drops 100 and 102–199 only; a 101 upgrade still switches and
// frames flow both ways.
func TestProxy_WebSocketUpgradeSurvives1xxGuard(t *testing.T) {
	echo := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: upgrade\r\n" +
			"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(c, rw)
	}))
	t.Cleanup(echo.Close)
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-1xx-ws", testWSUID)
	fb.pointLeaseAt(t, "tk-1xx-ws", echo)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-1xx-ws")

	up := upgrade(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if up.StatusCode != http.StatusSwitchingProtocols {
		drain(up)
		t.Fatalf("upgrade = %d, want 101", up.StatusCode)
	}
	defer up.Body.Close()
	rwc, ok := up.Body.(io.ReadWriteCloser)
	if !ok {
		t.Fatalf("101 body is %T, want a read-write stream", up.Body)
	}
	if _, err := rwc.Write([]byte("ping")); err != nil {
		t.Fatalf("write through stream: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(rwc, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo = %q, %v; want ping", buf, err)
	}
}
```

- [ ] **Step 2: Run them and see the security test fail**

Run: `go test ./internal/gateway/ -run 'TestResponsePolicy_Upstream1xx|TestProxy_WebSocketUpgradeSurvives1xxGuard' -count=1 2>&1 | grep -E '^\s*(---|ok|FAIL|\S+_test.go)'`
Expected: `TestResponsePolicy_Upstream1xxCannotStripPinnedHeaders` FAILs in both subtests with `client saw 1 informational responses, want 0`, `CSP = "", want the pinned session CSP` and every pinned header `= []` (checked on the planning host). `TestProxy_WebSocketUpgradeSurvives1xxGuard` PASSes. It is the guard for Step 3, not a failing test.

- [ ] **Step 3: Implement the pin helper and the 1xx guard**

`internal/gateway/proxy.go`:

1. In `ServeHTTP`, replace the block from `h := w.Header()` through `h.Set("Strict-Transport-Security", …)` (lines 396-403) with:

```go
	g.pinSessionHeaders(w.Header(), cspHost)
```

2. Directly above the `// responsePolicy is the ModifyResponse policy …` comment, add:

```go
// pinSessionHeaders sets the session-origin security policy on h (SEC-07).
// Set replaces every value, so pinning twice never duplicates a header.
func (g *Gateway) pinSessionHeaders(h http.Header, cspHost string) {
	h.Set("Content-Security-Policy", sessionCSP(cspHost, g.embedders))
	h.Set("Permissions-Policy", g.permissions)
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Origin-Agent-Cluster", "?1")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
}
```

3. Replace the `hijackCapture` struct (keep its existing doc comment, and append the paragraph shown) and add two methods after it:

```go
//
// It is also the 1xx guard (R1-04): the stdlib ReverseProxy relays an
// upstream informational response by copying its headers into Header(),
// calling WriteHeader(1xx) and then clearing the header map, which would
// erase the policy ServeHTTP pinned. The runtime is tenant-controlled, so
// informational responses other than 101 are dropped, and pin re-applies
// the session policy when the final status is written.
type hijackCapture struct {
	http.ResponseWriter
	onHijack func(net.Conn)
	wrapConn func(net.Conn) net.Conn
	pin      func(http.Header)
	wrote    bool
}

// WriteHeader drops 100 and 102–199, and re-pins the session policy
// before the first final (or 101) status reaches the client.
func (h *hijackCapture) WriteHeader(code int) {
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		return
	}
	if !h.wrote {
		h.wrote = true
		if h.pin != nil {
			h.pin(h.ResponseWriter.Header())
		}
	}
	h.ResponseWriter.WriteHeader(code)
}

func (h *hijackCapture) Write(b []byte) (int, error) {
	if !h.wrote {
		h.WriteHeader(http.StatusOK)
	}
	return h.ResponseWriter.Write(b)
}
```

4. In `serveProxy`, where `hw := &hijackCapture{ResponseWriter: w, …}` is built (line 761), add as the second field:

```go
		// serveProxy runs only on a matched workspace host, so the CSP
		// names r.Host exactly as ServeHTTP did.
		pin: func(h http.Header) { g.pinSessionHeaders(h, r.Host) },
```

`responsePolicy` stays as it is: it still deletes the upstream copies, so after the re-pin the response carries only the gateway's values.

- [ ] **Step 4: Run the gateway package**

Run: `gofmt -l internal/gateway; go test ./internal/gateway/ -count=1 > /tmp/t13.log 2>&1; echo rc=$?; grep -E '^(--- FAIL|ok|FAIL)|_test.go:' /tmp/t13.log | head`
Expected: no gofmt output; `ok` (the whole package took about 4 min on the planning host). The existing header tests (`TestHeaders_SessionResponses`, `TestHeaders_UpstreamCannotOverride`, `TestProxy_SecurityHeadersEverywhere`, `TestResponsePolicy_PinsCOOP`, `TestFraming_PortalOriginsOnEveryResponse`) stay green, which shows the re-pin adds no duplicate values.

- [ ] **Step 5: Docs**

`docs/security/threat-model.md`, Boundary 5 "Response policy" bullet (lines 340-345): after "so a hostile runtime cannot weaken them (`responsePolicy`, `internal/gateway/proxy.go`)", insert: "The proxy's response writer drops every upstream informational response other than `101` and re-applies the full set when the final status is written, so a `1xx` from the runtime can neither reach the browser nor clear the set (`hijackCapture`, `pinSessionHeaders`; `TestResponsePolicy_Upstream1xxCannotStripPinnedHeaders`)." The TM-29 register cells are updated in Task 30.

- [ ] **Step 6: Gates**

Run: `for t in .github/tests/*.test.sh; do bash "$t" >/dev/null || echo "FAIL $t"; done`
Expected: no output.

- [ ] **Step 7: Commit**

```bash
git add internal/gateway/proxy.go internal/gateway/proxy_1xx_test.go docs/security/threat-model.md
git commit -m "fix(gateway): upstream 1xx responses cannot strip pinned session headers

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 14: Session listener read bound (R1-05) — tier: design

**Finding (R1-05, P2, new: Boundary 5 "Timeouts").** `sessionServer` (`internal/backend/listeners.go:41-48`) sets only `ReadHeaderTimeout` (10 s) and `IdleTimeout` (120 s), with no `ReadTimeout`. `MaxHeaderBytes` is 1 MiB.
- Attack: an unauthenticated Internet client opens connections to `:8444`. On each one it sends complete headers, for example `POST /anything` with `Host: attacker.example` and `Content-Length: 200000`, or a `/v1/launch` POST. Then it never sends the body.
- Each connection pins a serve goroutine, a TLS conn and its buffers indefinitely. `IdleTimeout` does not apply while a body is being read. On the 421 path, net/http drains an unread body of 256 KiB or less after the handler returns, with no deadline, so not even the 421 is written. On `/v1/launch`, `ParseForm` blocks inside the handler. The launch limiter bounds only the arrival rate.
- The attack works when `:8444` is exposed directly or through TLS pass-through, which are the chart's documented options (`values.yaml:545-548`). In merged mode the same process also serves the public API.
- Required fix: bound every request's read phase on `:8444`, and lift the bound only where a request legitimately needs a long read. Also lower `MaxHeaderBytes` from 1 MiB. Invariant: no unauthenticated request on `:8444` can hold a conn or goroutine past a fixed bound.
- Security test: `TestSessionListener_StalledBodyBounded`. It stalls a declared body on `/v1/launch` and on a 421 path against `sessionServer(...)` with the real gateway, and asserts the server closes the conn within the bound. Companion: an admitted WebSocket stream lives past the bound.

**Approach (Plan ruling 3, checked against Go 1.26.9 at `98c7f54`).** The fix is `ReadTimeout` on `sessionServer` (30 s, a package var so tests can shorten it) plus `MaxHeaderBytes: 64 << 10`. Two facts make this enough without new gateway code:
- **Hijacked streams.** net/http clears a conn's deadlines when a handler hijacks it: `(*conn).hijackLocked` calls `rwc.SetDeadline(time.Time{})` (`$(go env GOROOT)/src/net/http/server.go:317-325`). The gateway's WebSocket path hijacks through `hijackCapture.Hijack` → `http.NewResponseController(...).Hijack()`, so an admitted stream outlives `ReadTimeout`. The comment in `listeners.go:18-22` and the test `TestSessionServerTimeouts`, which say deadlines "survive Hijack()", are wrong for this Go version. Both are corrected here. No explicit clear is added in the gateway: it would only duplicate the stdlib's.
- **The transfer leg.** It already extends its own deadlines after the broker claim: `rc.SetReadDeadline(time.Now().Add(transferStreamBudget))` (`internal/gateway/transfer.go:138-140`). A per-request `SetReadDeadline` overrides the server's `ReadTimeout`.

`WriteTimeout` stays zero, so a proxied download or document streams for as long as the client reads. Why not the per-request variant (`SetReadDeadline` at the top of `Gateway.ServeHTTP`): the server-level bound also covers the post-handler body drain, and any path served outside the gateway handler, with one field. The session listener negotiates HTTP/1.1 only (its `tls.Config` sets no `NextProtos`, `internal/backend/wire.go:942`), so there is no HTTP/2 stream case.

Review Focus 1 is pinned by `TestSessionListener_AdmittedStreamOutlivesReadBound` and `TestSessionListener_TransferUploadOutlivesReadBound`. They live in `internal/gateway`, because the launch and transfer fakes are there. They serve the real gateway handler from an `httptest` server whose `ReadTimeout` is 1 s, the same configuration `sessionServer` applies. Both pass before and after the fix: they are guards against a future change that breaks the two legitimate long reads.

**Files:**
- Modify: `internal/backend/listeners.go:18-22` (budget comment), `:37-48` (`sessionServer` and its comment; new `sessionReadTimeout` var above it)
- Create: `internal/backend/session_readbound_test.go`
- Modify: `internal/backend/listeners_test.go:114-133` (`TestSessionServerTimeouts`)
- Create: `internal/gateway/readbound_test.go`
- Modify: `docs/security/threat-model.md:360-363` (Boundary 5 "Timeouts" bullet)

**Interfaces:**
- Consumes: package `backend` test helper `stubBrokerClient` (`metrics_test.go:131`); `sessionhost.ParseDomain`; `gateway.New`, `gateway.Config{Identity, SessionDomain, ControlHosts, Broker}`. Package `gateway_test` helpers: `newFakeBroker`, `scriptTicket`, `pointLeaseAt`, `leaseOf`, `targetFor`, `newGatewayHandle(t, fb, mutate) (*gateway.Gateway, *httptest.Server)`, `launchOK`, `upgrade`, `drain`, `newXferStub`, `(*xferStub).file`, `xfID`, `xfPath`, `portalOrigin`, `fb.mu`, `fb.resolveT`, `fb.transfers`.
- Produces: `var sessionReadTimeout = 30 * time.Second` in package `backend` (unexported; tests assign it and restore it with `t.Cleanup`). `sessionServer` keeps its signature `func sessionServer(addr string, h http.Handler) *http.Server` and now sets `ReadTimeout: sessionReadTimeout` and `MaxHeaderBytes: 64 << 10`. Task 15 edits `wire.go` and `backend.go`, not this file.

- [ ] **Step 1: Write the failing test**

Create `internal/backend/session_readbound_test.go`:

```go
// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
	"github.com/tinyorbitvn/tinycdi/internal/sessionhost"
)

// TestSessionListener_StalledBodyBounded (R1-05): an unauthenticated
// client that sends complete headers and then stalls its declared body
// must not hold a conn or a goroutine past the listener's read bound —
// neither on a misdirected (421) path, where the server drains the body
// after the handler, nor on /v1/launch, where ParseForm blocks.
func TestSessionListener_StalledBodyBounded(t *testing.T) {
	old := sessionReadTimeout
	sessionReadTimeout = time.Second
	t.Cleanup(func() { sessionReadTimeout = old })

	dom, err := sessionhost.ParseDomain("session.example.test")
	if err != nil {
		t.Fatal(err)
	}
	gw, err := gateway.New(gateway.Config{
		Identity:      broker.GatewayIdentity{ID: "gw-test", Audience: "session.example.test"},
		SessionDomain: dom,
		ControlHosts:  []string{"backend.tinycdi.svc"},
		Broker:        stubBrokerClient{},
	})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	t.Cleanup(gw.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := sessionServer(ln.Addr().String(), gw)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	for name, head := range map[string]string{
		"misdirected 421": "POST /anything HTTP/1.1\r\nHost: attacker.example\r\nContent-Length: 200000\r\n\r\n",
		"launch": "POST /v1/launch?tcdi_tab=0123456789abcdef0123456789abcdef HTTP/1.1\r\n" +
			"Host: ws-deadbeef.session.example.test\r\nContent-Type: application/x-www-form-urlencoded\r\n" +
			"Content-Length: 4000\r\n\r\nticket=",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if _, err := fmt.Fprint(c, head); err != nil {
				t.Fatal(err)
			}
			// The server must close the conn within a few bounds; a read
			// that times out on our side means it is still held open.
			_ = c.SetReadDeadline(time.Now().Add(5 * sessionReadTimeout))
			_, err = io.Copy(io.Discard, c)
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatalf("conn still held open %v after a stalled body (bound %v)", 5*sessionReadTimeout, sessionReadTimeout)
			}
		})
	}
}
```

The test serves plain TCP. The bound under test is the `http.Server` configuration, which is the same with or without the TLS listener `serveTLS` wraps around it.

- [ ] **Step 2: Run it and see it fail**

Add the var alone first, so the failure is behavioural, not a compile error. In `internal/backend/listeners.go`, directly above `// sessionServer builds the session listener.`, add:

```go
// sessionReadTimeout is the session listener's whole-request read bound
// (R1-05): headers plus body must arrive within it, so an unauthenticated
// client that stalls a declared body cannot hold a conn or a serve
// goroutine. A var so tests can shorten it.
var sessionReadTimeout = 30 * time.Second
```

Run: `go test ./internal/backend/ -run TestSessionListener_StalledBodyBounded -count=1 2>&1 | grep -E '^\s*(---|ok|FAIL|\S+_test.go)'`
Expected: FAIL in both subtests after about 5 s each: `conn still held open 5s after a stalled body (bound 1s)` (checked on the planning host).

- [ ] **Step 3: Implement**

In `internal/backend/listeners.go`, replace the budget comment (lines 18-22, from `// Listener timeout budgets (SEC-23).` to `// them as absolute conn deadlines that survive Hijack().`) with:

```go
// Listener timeout budgets (SEC-23). The app, internal and metrics
// listeners bound every request phase — they serve no streaming or
// WebSocket endpoints. The session listener bounds the whole request read
// (sessionReadTimeout) but not writes: net/http clears a conn's deadlines
// when a handler hijacks it (server.go hijackLocked), so an admitted
// WebSocket stream outlives the bound, and the transfer leg extends its own
// read and write deadlines after its claim (gateway transferStreamBudget).
```

Replace `sessionServer` and its doc comment with:

```go
// sessionServer builds the session listener. ReadHeaderTimeout bounds
// header parsing, ReadTimeout bounds the whole request read (including the
// body drain net/http runs after a handler that left the body unread),
// IdleTimeout bounds idle keep-alive conns and MaxHeaderBytes caps header
// size. WriteTimeout stays zero: proxied desktop documents and downloads
// stream for as long as the client reads.
func sessionServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       sessionReadTimeout,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}
```

Replace `TestSessionServerTimeouts` in `internal/backend/listeners_test.go` (lines 114-133, doc comment included) with:

```go
// TestSessionServerTimeouts (SEC-23, R1-05): the session listener bounds
// header parsing, the whole request read, keep-alive idleness and header
// size. WriteTimeout stays zero so proxied downloads stream; hijacked
// WebSocket streams are unaffected by ReadTimeout because net/http clears
// a conn's deadlines on Hijack (TestSessionListener_AdmittedStreamOutlivesReadBound).
func TestSessionServerTimeouts(t *testing.T) {
	srv := sessionServer(":0", nil)
	if srv.ReadHeaderTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Fatal("ReadHeaderTimeout and IdleTimeout must be > 0")
	}
	if srv.ReadTimeout != sessionReadTimeout || srv.ReadTimeout <= 0 {
		t.Fatalf("ReadTimeout = %v, want sessionReadTimeout (%v) > 0", srv.ReadTimeout, sessionReadTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %v, want 0 (downloads stream)", srv.WriteTimeout)
	}
	if srv.MaxHeaderBytes != 64<<10 {
		t.Fatalf("MaxHeaderBytes = %d, want 64 KiB", srv.MaxHeaderBytes)
	}
}
```

Run: `go test ./internal/backend/ -run 'TestSessionListener_StalledBodyBounded|TestSessionServerTimeouts|TestServerTimeouts' -count=1 2>&1 | grep -E '^(--- FAIL|ok|FAIL)|_test.go:'`
Expected: `ok`.

- [ ] **Step 4: Add the Review Focus 1 guards**

Create `internal/gateway/readbound_test.go`:

```go
package gateway_test

import (
	"crypto/sha1"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

// testReadBound stands in for the backend session listener's ReadTimeout
// (internal/backend/listeners.go sessionReadTimeout, 30 s in production).
const testReadBound = time.Second

// boundedServer serves h the way the backend session listener does: with a
// whole-request ReadTimeout.
func boundedServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ReadTimeout = testReadBound
	srv.Config.ReadHeaderTimeout = testReadBound
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// TestSessionListener_AdmittedStreamOutlivesReadBound (R1-05, Review
// Focus 1): an admitted WebSocket stream lives past the listener's read
// bound — net/http clears the conn deadline when the upgrade is hijacked.
func TestSessionListener_AdmittedStreamOutlivesReadBound(t *testing.T) {
	echo := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: upgrade\r\n" +
			"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(c, rw)
	}))
	t.Cleanup(echo.Close)
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-rb-ws", testWSUID)
	fb.pointLeaseAt(t, "tk-rb-ws", echo)
	h, _ := newGatewayHandle(t, fb, nil)
	srv := boundedServer(t, h)
	cookie := launchOK(t, srv, testHost, "tk-rb-ws")

	up := upgrade(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if up.StatusCode != http.StatusSwitchingProtocols {
		drain(up)
		t.Fatalf("upgrade = %d, want 101", up.StatusCode)
	}
	defer up.Body.Close()
	rwc := up.Body.(io.ReadWriteCloser)
	time.Sleep(3 * testReadBound)
	if _, err := rwc.Write([]byte("late")); err != nil {
		t.Fatalf("write after the read bound: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(rwc, buf); err != nil || string(buf) != "late" {
		t.Fatalf("echo after the read bound = %q, %v; want the stream alive", buf, err)
	}
}

// slowBody yields n bytes, one per tick, so the whole body takes n*tick.
type slowBody struct {
	n    int
	tick time.Duration
}

func (s *slowBody) Read(p []byte) (int, error) {
	if s.n == 0 {
		return 0, io.EOF
	}
	time.Sleep(s.tick)
	s.n--
	p[0] = 'x'
	return 1, nil
}

// TestSessionListener_TransferUploadOutlivesReadBound (R1-05, Review
// Focus 1): a granted upload whose body takes longer than the listener's
// read bound completes — the transfer leg extends its own deadline after
// the claim.
func TestSessionListener_TransferUploadOutlivesReadBound(t *testing.T) {
	fb := newFakeBroker(t)
	stub := newXferStub(t)
	fb.scriptTicket("tk-rb-xfer", testWSUID)
	lease := fb.leaseOf(t, "tk-rb-xfer")
	tgt := targetFor(stub.srv)
	tgt.XferToken = "xt"
	fb.mu.Lock()
	fb.resolveT[lease.ID] = tgt
	fb.transfers[xfID] = broker.TransferGrant{ID: xfID, WorkspaceUID: testWSUID, Direction: "upload", Path: "slow.bin", MaxBytes: 100}
	fb.mu.Unlock()
	h, _ := newGatewayHandle(t, fb, func(c *gateway.Config) { c.PortalOrigins = []string{portalOrigin} })
	srv := boundedServer(t, h)
	cookie := launchOK(t, srv, testHost, "tk-rb-xfer")

	const size = 10
	req, err := http.NewRequest(http.MethodPut, srv.URL+xfPath, &slowBody{n: size, tick: 3 * testReadBound / size})
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = size
	req.Host = testHost
	req.Header.Set("Cookie", gateway.SessionCookieName+"="+cookie)
	req.Header.Set("Origin", portalOrigin)
	res, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatalf("slow upload: %v", err)
	}
	defer drain(res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("slow upload = %d, want 200", res.StatusCode)
	}
	if v, _ := stub.file("slow.bin"); len(v) != size {
		t.Fatalf("helper stored %d bytes, want %d", len(v), size)
	}
}
```

Run: `go test ./internal/gateway/ -run 'OutlivesReadBound' -count=1 -v 2>&1 | grep -E '^(--- |ok|FAIL)'`
Expected: `--- PASS` for both (about 3 s each).

- [ ] **Step 5: Run the backend package**

Run: `go test ./internal/backend/ -count=1 > /tmp/t14.log 2>&1; echo rc=$?; grep -E '^(--- FAIL|ok|FAIL)|_test.go:' /tmp/t14.log | head`
Expected: `ok` (Docker for the Postgres-backed tests; a skip is not a pass). `TestReadyz_*` and the drain tests that use `sessionServer` stay green.

- [ ] **Step 6: Docs**

`docs/security/threat-model.md`, Boundary 5 "Timeouts" bullet (lines 360-363): replace "the session listener bounds only the header read, because connections are hijacked for WebSocket (`internal/backend/listeners.go`)." with: "the session listener bounds the header read (10 s) and the whole request read (`sessionReadTimeout`, 30 s), so a client that stalls a declared body is cut at the bound (`TestSessionListener_StalledBodyBounded`). Headers are capped at 64 KiB. net/http clears the deadline when a WebSocket upgrade is hijacked, and the transfer leg extends its own deadline after the broker claim, so admitted streams and granted transfers outlive the bound (`TestSessionListener_AdmittedStreamOutlivesReadBound`, `TestSessionListener_TransferUploadOutlivesReadBound`; `internal/backend/listeners.go`)." Task 30 adds the register row for this control.

- [ ] **Step 7: Gates**

Run: `gofmt -l internal/backend internal/gateway; for t in .github/tests/*.test.sh; do bash "$t" >/dev/null || echo "FAIL $t"; done`
Expected: no output.

- [ ] **Step 8: Commit**

```bash
git add internal/backend/listeners.go internal/backend/listeners_test.go internal/backend/session_readbound_test.go internal/gateway/readbound_test.go docs/security/threat-model.md
git commit -m "fix(backend): bound request reads on the session listener

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 15: Health on control hosts only; control token length and failure limit (R1-26) — tier: integrate

**Finding (R1-26, P3, TM-26).** The `:8444` host-class claims overstate the boundary.
1. `b.sessionHandler = b.readyz(api.RequestID(gw))` (`internal/backend/wire.go:632`) answers `/healthz` and `/readyz` before the gateway's host routing (`internal/backend/backend.go:329-349`). Any Internet client gets them on any workspace or foreign Host of `:8444`, with no pinned CSP, COOP or nosniff, and they reveal the replica's draining state. The chart's probes use the app port (`deploy/helm/tinycdi/templates/backend.yaml:266-283`).
2. The control surface is gated by `Host: backend[.<ns>[.svc]]`, which the client chooses (`controlHostOK` reads `r.Host`, `internal/gateway/authorization.go:462`; Go does not tie SNI to Host). With a direct or pass-through exposure, an Internet client reaches `/v1/control/session` and `/v1/control/revoke` (list or revoke every lease on the replica). The only gate is a bearer token with no minimum length (`wire.go:75-84`) and no throttling of failures (`serveControl`, `internal/gateway/proxy.go:470-492`). An operator-chosen short token can be brute-forced at line rate.
- Required fix: health off the workspace and foreign hosts of `:8444`; refuse a control token shorter than 32 bytes at startup; add a per-client failure limiter to the control bearer check; state in the threat model that the control host class is a routing check, not a network boundary, and the bearer is the only gate.
- Security tests: `TestSessionListener_HealthOnlyOnControlHosts` in `internal/backend` (the health paths on a workspace host and a foreign host through `b.sessionHandler` never answer health) and `TestControlToken_MinLength` in `internal/backend/config_test.go`. This task adds `TestControlAuth_FailuresLimitedPerClient` in `internal/gateway` for the limiter.

**Design.** Health stays on the session listener's **control** hosts, because a split deployment (session listener only, `-listen=`; `TestRun_HoldsDrainWindow` runs one) has no other listener to probe. On every other Host, the health paths go to the gateway, which answers `404` (`/healthz`), `401` or `421` (`/readyz`), with the pinned session headers. The failure limiter spends a token only on a failed bearer check, but refuses every control request from a client whose budget is spent, the right token included. Otherwise a locked-out guesser would still learn the right token from a `200`. This needs a non-spending check, so `ratelimit.Limiter` gains `Peek`.

**Files:**
- Modify: `internal/ratelimit/ratelimit.go:119` (new `Peek` above `PerReplica`); test `internal/ratelimit/ratelimit_test.go` (append)
- Modify: `internal/gateway/proxy.go:237` (`Gateway` field), `:311-321` (construction; Task 13 shifts these lines, so find by symbol), `:456` (constants + `IsControlHost` above `controlAuth`), `:470-482` (`serveControl`)
- Create: `internal/gateway/control_limit_test.go`
- Modify: `internal/backend/config.go:73` (new `minControlTokenBytes`, `checkControlToken` above the `controlTokenUnsetWarning` comment), `internal/backend/wire.go:75-84` (call it), `:632` (session handler), `internal/backend/backend.go:324` (new `controlHostHealth` above `readyz`)
- Create: `internal/backend/session_health_test.go`
- Modify: `internal/backend/backend_test.go:49` (helpers), `:695,855,1038` (short tokens), `:958,1067,1081,1093` (health probes need the control Host), `:245-246` (`testSessionHandler` comment)
- Modify docs: `docs/security/threat-model.md:293-299` (Host classes bullet) and `:357-359` (Control surface bullet), `docs/runbooks/install.md:219`, `deploy/helm/tinycdi/README.md:113`, `docs/runbooks/observability.md:217`

**Interfaces:**
- Consumes: `ratelimit.New(ratePerMinute, burst, maxKeys int, now func() time.Time) *Limiter`, `ratelimit.ClientKey(r *http.Request, trusted []netip.Prefix) string`, `ratelimit.RetryAfterSeconds(time.Duration) int`; `g.cfg.TrustedProxies []netip.Prefix`; `g.controlHostOK(r)`. Backend test helpers `testSessionHandler(t, bc) (*Backend, http.Handler)` (built with `SessionControlHosts: "session.test"`), `newFakeBrokerClient(t)`. Gateway test helpers `newGateway`, `newFakeBroker`, `drain`, `testControlHost`; the default test `ControlToken` is `"control-test-token"`.
- Produces:
  - `func (l *Limiter) Peek(key string) (ok bool, retryAfter time.Duration)` (package `ratelimit`)
  - `const gateway.ControlFailureRate = 10`, `gateway.ControlFailureBurst = 10` (exported), `controlFailureMaxKeys = 10_000`
  - `func (g *Gateway) IsControlHost(r *http.Request) bool`
  - control refusal `429 {"error":"rate_limited"}` with `Retry-After`, audited as `denied`/`rate_limited`
  - `const minControlTokenBytes = 32` and `func checkControlToken(tok string) error` (package `backend`)
  - `func (b *Backend) controlHostHealth(gw *gateway.Gateway) http.Handler`
  - backend test helpers `const testControlToken = "0123456789abcdef0123456789abcdef"` and `func controlHostGet(c *http.Client, base, path string) (*http.Response, error)`

- [ ] **Step 1: Write the failing tests**

Create `internal/backend/session_health_test.go`:

```go
// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSessionListener_HealthOnlyOnControlHosts (R1-26): /healthz and
// /readyz answer on the session listener's control hosts only. On a
// workspace host or a foreign Host the gateway answers instead — never the
// health body, and always with the pinned session-origin CSP.
func TestSessionListener_HealthOnlyOnControlHosts(t *testing.T) {
	_, h := testSessionHandler(t, newFakeBrokerClient(t))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	get := func(host, path string) (int, string, http.Header) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
		if err != nil {
			t.Fatalf("GET %s on %s: %v", path, host, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw), resp.Header
	}
	for _, host := range []string{"ws-0000000a.session.test", "attacker.example"} {
		for _, path := range []string{"/healthz", "/readyz"} {
			code, body, hdr := get(host, path)
			if code == http.StatusOK || strings.Contains(body, `"status"`) {
				t.Errorf("%s on %s = %d %q, want the gateway's refusal, not health", path, host, code, body)
			}
			if !strings.Contains(hdr.Get("Content-Security-Policy"), "frame-ancestors") {
				t.Errorf("%s on %s: CSP %q, want the pinned session CSP", path, host, hdr.Get("Content-Security-Policy"))
			}
		}
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		if code, body, _ := get("session.test", path); code != http.StatusOK || !strings.Contains(body, `"ok"`) {
			t.Errorf("%s on the control host = %d %q, want 200 ok", path, code, body)
		}
	}
}

// TestControlToken_MinLength (R1-26): a configured control token shorter
// than 32 bytes is refused at startup; an empty token (control closed) and
// a 32-byte token are accepted.
func TestControlToken_MinLength(t *testing.T) {
	for _, tc := range []struct {
		tok string
		ok  bool
	}{
		{"", true},
		{"tok", false},
		{strings.Repeat("a", minControlTokenBytes-1), false},
		{strings.Repeat("a", minControlTokenBytes), true},
	} {
		if err := checkControlToken(tc.tok); (err == nil) != tc.ok {
			t.Errorf("checkControlToken(%d bytes) = %v, want ok=%v", len(tc.tok), err, tc.ok)
		}
	}
}
```

Move `TestControlToken_MinLength` into `internal/backend/config_test.go` (append it after `TestControlTokenUnsetWarning`, and drop it from the new file) if you prefer it next to the other config tests. Both files are package `backend`, and `config_test.go` already imports `strings`.

Create `internal/gateway/control_limit_test.go`:

```go
package gateway_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

// controlFrom sends GET /v1/control/session on the control host as client
// (the right-most X-Forwarded-For entry behind a trusted loopback proxy).
func controlFrom(t *testing.T, srv *httptest.Server, client, bearer string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/control/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = testControlHost
	req.Header.Set("X-Forwarded-For", client)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatalf("control request: %v", err)
	}
	drain(resp)
	return resp.StatusCode
}

// TestControlAuth_FailuresLimitedPerClient (R1-26): the bearer is the only
// gate on the control hosts, so failed guesses are budgeted per client.
// Once a client spends ControlFailureBurst failures, every control request
// from it answers 429 — the right token included — while another client
// is unaffected.
func TestControlAuth_FailuresLimitedPerClient(t *testing.T) {
	fb := newFakeBroker(t)
	srv := newGateway(t, fb, func(c *gateway.Config) {
		c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	})
	const attacker, operator = "198.51.100.7", "198.51.100.8"
	for i := 0; i < gateway.ControlFailureBurst; i++ {
		if got := controlFrom(t, srv, attacker, "guess"); got != http.StatusUnauthorized {
			t.Fatalf("failed guess %d = %d, want 401", i, got)
		}
	}
	if got := controlFrom(t, srv, attacker, "guess"); got != http.StatusTooManyRequests {
		t.Fatalf("guess over budget = %d, want 429", got)
	}
	if got := controlFrom(t, srv, attacker, "control-test-token"); got != http.StatusTooManyRequests {
		t.Fatalf("right token from a locked-out client = %d, want 429", got)
	}
	if got := controlFrom(t, srv, operator, "control-test-token"); got != http.StatusOK {
		t.Fatalf("right token from another client = %d, want 200", got)
	}
}
```

Append to `internal/ratelimit/ratelimit_test.go` (package `ratelimit`; it already imports `testing` and `time`):

```go
// TestLimiter_PeekDoesNotSpend: Peek answers what Allow would, without
// spending a token or tracking an unseen key.
func TestLimiter_PeekDoesNotSpend(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := New(60, 2, 10, func() time.Time { return now })
	if ok, _ := l.Peek("k"); !ok {
		t.Fatal("Peek on an unseen key refused")
	}
	if len(l.buckets) != 0 {
		t.Fatalf("Peek created %d buckets, want 0", len(l.buckets))
	}
	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("Allow %d refused inside the burst", i)
		}
	}
	for i := 0; i < 3; i++ {
		if ok, retry := l.Peek("k"); ok || retry <= 0 {
			t.Fatalf("Peek after the burst = %v, %v; want refused with a retry", ok, retry)
		}
	}
	now = now.Add(time.Second)
	if ok, _ := l.Peek("k"); !ok {
		t.Fatal("Peek after one refill refused")
	}
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("Peek spent the refilled token")
	}
}
```

- [ ] **Step 2: Run them and see them fail**

Run: `go test ./internal/backend/ -run 'TestSessionListener_HealthOnlyOnControlHosts' -count=1 2>&1 | grep -E '^\s*(---|ok|FAIL|\S+_test.go)'; go vet ./internal/gateway/ ./internal/ratelimit/ 2>&1 | head -4`
Expected: compile errors `undefined: minControlTokenBytes` / `undefined: checkControlToken` (backend), `undefined: gateway.ControlFailureBurst` (gateway) and `l.Peek undefined` (ratelimit). To see the behavioural failure of the health test first, temporarily comment out `TestControlToken_MinLength` and rerun: `/healthz on ws-0000000a.session.test = 200 "{\"status\":\"ok\"}"` and the same for `/readyz` and `attacker.example`, each with `CSP ""`. Restore the test afterwards.

- [ ] **Step 3: `ratelimit.Peek`**

In `internal/ratelimit/ratelimit.go`, directly above `// PerReplica divides a configured per-key budget`, add:

```go
// Peek reports what Allow(key) would answer now without spending a token
// and without creating a bucket for an unseen key (which would pass).
func (l *Limiter) Peek(key string) (ok bool, retryAfter time.Duration) {
	if l == nil || l.perSec <= 0 {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	el, found := l.buckets[key]
	if !found {
		return true, 0
	}
	b := el.Value.(*bucket)
	tokens := b.tokens
	if elapsed := now.Sub(b.last); elapsed > 0 {
		tokens += l.perSec * elapsed.Seconds()
		if tokens > l.burst {
			tokens = l.burst
		}
	}
	if tokens >= 1 {
		return true, 0
	}
	return false, time.Duration((1-tokens)/l.perSec*float64(time.Second) + 0.5)
}
```

- [ ] **Step 4: Gateway failure limiter and `IsControlHost`**

`internal/gateway/proxy.go`:

1. In `type Gateway struct`, after `permissions   string // Permissions-Policy pinned on every response`, add:

```go
	// controlFailures counts failed /v1/control/* bearer checks per client
	// key (R1-26): once a client spends its budget, every control request
	// from it answers 429 — the right token included — until it refills.
	controlFailures *ratelimit.Limiter
```

2. In `New`, directly after the `g := &Gateway{ … }` literal closes (before `for _, po := range cfg.PortalOrigins`), add:

```go
	g.controlFailures = ratelimit.New(ControlFailureRate, ControlFailureBurst, controlFailureMaxKeys, cfg.Now)
```

3. Directly above `func (g *Gateway) controlAuth(r *http.Request) bool {`, add:

```go
// Control-surface failure budget (R1-26): ControlFailureBurst failed
// bearer checks per client key, refilled at ControlFailureRate per minute.
// The bearer is the only gate on the control hosts — the host class is a
// Host-header routing check, not a network boundary — so guessing must not
// run at line rate. controlFailureMaxKeys caps the LRU of tracked clients.
const (
	ControlFailureRate    = 10
	ControlFailureBurst   = 10
	controlFailureMaxKeys = 10_000
)

// IsControlHost reports whether r's Host names a configured control host —
// the backend mounts its health endpoints on those hosts only.
func (g *Gateway) IsControlHost(r *http.Request) bool { return g.controlHostOK(r) }
```

4. In `serveControl`, replace the `if !g.controlAuth(r) { … }` block with:

```go
	client := ratelimit.ClientKey(r, g.cfg.TrustedProxies)
	if ok, retry := g.controlFailures.Peek(client); !ok {
		if action != "" {
			g.audit(r, action, "", observability.OutcomeDenied, "rate_limited")
		}
		w.Header().Set("Retry-After", strconv.Itoa(ratelimit.RetryAfterSeconds(retry)))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
		return
	}
	if !g.controlAuth(r) {
		g.controlFailures.Allow(client)
		if action != "" {
			g.audit(r, action, "", observability.OutcomeDenied, "unauthorized")
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
```

`proxy.go` already imports `strconv` and `ratelimit`. The existing `TestControlEndpoints_WrongBearerRejected` sends six failures and then the right token from one client. That stays under the burst of 10, so it is unchanged.

- [ ] **Step 5: Backend token check and control-host health**

`internal/backend/config.go`: directly above the comment `// controlTokenUnsetWarning is logged at startup when no control token is` (line 73), add:

```go
// minControlTokenBytes is the shortest control bearer the backend accepts
// (R1-26). On the session listener's control hosts the bearer is the only
// gate — the host class is a Host-header routing check, not a network
// boundary — so it must resist guessing on its own.
const minControlTokenBytes = 32

// checkControlToken refuses a configured control token shorter than
// minControlTokenBytes. An empty token is allowed: the control surface is
// then closed (fail closed).
func checkControlToken(tok string) error {
	if tok != "" && len(tok) < minControlTokenBytes {
		return fmt.Errorf("control token is %d bytes; it must be at least %d (for example: openssl rand -hex 32)", len(tok), minControlTokenBytes)
	}
	return nil
}
```

`internal/backend/wire.go` `wire()`: after the `if cfg.ControlTokenFile != "" { … }` block (it ends at line 81), and before `if cfg.ControlToken == "" {`, add:

```go
	if err := checkControlToken(cfg.ControlToken); err != nil {
		return err
	}
```

This covers both sources: the file read just above it, and `TCDI_CONTROL_TOKEN`, which `ParseFlags` already put into `cfg.ControlToken`.

`internal/backend/wire.go:632`: replace `b.sessionHandler = b.readyz(api.RequestID(gw))` with:

```go
	b.sessionHandler = api.RequestID(b.controlHostHealth(gw))
```

`internal/backend/backend.go`: directly above `// readyz wraps a listener handler:`, add (the file already imports `gateway`):

```go
// controlHostHealth mounts /healthz and /readyz on the session listener's
// control hosts only (R1-26), where a split deployment's probes send them.
// On a workspace or foreign Host every path, the health paths included,
// goes straight to the gateway, which pins the session-origin headers and
// answers 404, 401 or 421 by host class.
func (b *Backend) controlHostHealth(gw *gateway.Gateway) http.Handler {
	health := b.readyz(gw)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gw.IsControlHost(r) {
			health.ServeHTTP(w, r)
			return
		}
		gw.ServeHTTP(w, r)
	})
}
```

In `readyz`'s doc comment, replace "— even on the app listener, which has no health endpoint of its own" with "— on the app listener, and on the session listener's control hosts (`controlHostHealth`)".

- [ ] **Step 6: Existing backend tests that used a short token or probed health by IP**

`internal/backend/backend_test.go`:

1. After `func testLog() *slog.Logger { … }` (line 49), add:

```go
// testControlToken is a control bearer of the minimum accepted length
// (minControlTokenBytes).
const testControlToken = "0123456789abcdef0123456789abcdef"

// controlHostGet GETs path on the session listener at base with the
// control Host the drain tests configure (-session-control-hosts
// session.test): health answers on control hosts only.
func controlHostGet(c *http.Client, base, path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Host = "session.test"
	return c.Do(req)
}
```

2. Replace the three occurrences of `[]byte("tok")` (lines 695, 855, 1038: `TestRun_DrainsOnShutdown`, `TestRun_DrainPropagationWait`, `TestRun_HoldsDrainWindow`) with `[]byte(testControlToken)`.
3. Replace the four `insecure.Get(base + "/readyz")` and `insecure.Get(base + "/healthz")` calls (lines 958, 1067, 1081, 1093) with `controlHostGet(insecure, base, "/readyz")` and `controlHostGet(insecure, base, "/healthz")`.
4. `testSessionHandler`'s comment (lines 245-246): "the same readyz+RequestID+Gateway stack wire() builds" becomes "the same RequestID+controlHostHealth+Gateway stack wire() builds".

A quick sed for items 2 and 3:

```bash
sed -i -e 's/\[\]byte("tok")/[]byte(testControlToken)/' \
  -e 's#insecure\.Get(base + "\(/readyz\|/healthz\)")#controlHostGet(insecure, base, "\1")#' internal/backend/backend_test.go
```

- [ ] **Step 7: Run the focused and package tests**

Run:
```bash
go test ./internal/ratelimit/ ./internal/gateway/ -count=1 > /tmp/t15g.log 2>&1; echo rc=$?; grep -E '^(--- FAIL|ok|FAIL)|_test.go:' /tmp/t15g.log | head
go test ./internal/backend/ -count=1 > /tmp/t15b.log 2>&1; echo rc=$?; grep -E '^(--- FAIL|ok|FAIL)|_test.go:' /tmp/t15b.log | head
```
Expected: `ok` for all three packages (Docker for the backend's Postgres tests; a skip is not a pass). The drain tests (`TestRun_DrainsOnShutdown`, `TestRun_DrainPropagationWait`, `TestRun_HoldsDrainWindow`) pass with the control Host.

- [ ] **Step 8: Docs**

- `docs/security/threat-model.md`, Boundary 5 "Host classes" bullet (lines 293-299): append "The host class is a routing check on the client-chosen `Host` header, not a network boundary: a client that reaches `:8444` directly can name a control host. `/healthz` and `/readyz` answer on control hosts only (`controlHostHealth`, `TestSessionListener_HealthOnlyOnControlHosts`)."
- Same file, "Control surface" bullet (lines 357-359): replace it with "**Control surface**: `/v1/control/*` answers only on control hosts, and the bearer from `-control-token-file` is its only gate (`controlAuth`, `internal/gateway/proxy.go`). The backend refuses to start with a token shorter than 32 bytes (`checkControlToken`, `TestControlToken_MinLength`). Each client key may fail the bearer check 10 times, refilled at 10 per minute; after that every control request from that client answers `429`, the right token included (`TestControlAuth_FailuresLimitedPerClient`)."
- `docs/runbooks/install.md:219`, the `tinycdi-backend-control-token` row: after "set `backend.controlToken.existingSecret` (key `token`)." insert "The token must be at least 32 bytes (`openssl rand -hex 32`); a shorter one stops the backend at startup."
- `deploy/helm/tinycdi/README.md:113`: the description becomes "`/v1/control/*` bearer (at least 32 bytes); unset = routes closed".
- `docs/runbooks/observability.md:217`: "bearer-denied control attempts record `denied`/`unauthorized`" becomes "bearer-denied control attempts record `denied`/`unauthorized`, and attempts from a client over its failure budget record `denied`/`rate_limited`".

The TM-26 register cells are updated in Task 30.

- [ ] **Step 9: Gates**

Run: `gofmt -l internal/; for t in .github/tests/*.test.sh; do bash "$t" >/dev/null || echo "FAIL $t"; done`
Expected: no output.

- [ ] **Step 10: Commit**

```bash
git add internal/ratelimit/ratelimit.go internal/ratelimit/ratelimit_test.go internal/gateway/proxy.go internal/gateway/control_limit_test.go internal/backend/config.go internal/backend/wire.go internal/backend/backend.go internal/backend/session_health_test.go internal/backend/backend_test.go internal/backend/config_test.go docs/security/threat-model.md docs/runbooks/install.md deploy/helm/tinycdi/README.md docs/runbooks/observability.md
git commit -m "fix(backend): health only on control hosts; control token length and failure limit

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 16: In-pod trust covers the desktop leg (R1-27) — tier: integrate

**Finding (R1-27, P3, TM-37; also TM-36).** The in-pod trust residual understates the desktop-leg channel.
- Attack: a workspace is Isolated, with file transfer Disabled and clipboard Disabled. The user kills `tcdi-xferd` and binds `:8443` with the Secret's own cert and key, which the docs already say the user can do. Their server answers `GET /app/<anything>` with any file and a `Content-Disposition: attachment` header. The user then opens `https://<ws-host>/app/x` in their browser.
- What happens: the gateway's desktop leg allowlists `/app/`, `/assets/`, `/core/` and `/websockify` (`upstreamPathOK`, `internal/gateway/authorization.go:541-553`). It relays the body without `ClaimTransfer`, without the `maxFileBytes` cap and without a transfer audit row, and `responsePolicy` (`internal/gateway/proxy.go`) does not drop `Content-Disposition`. `/websockify` gives an uncapped inbound channel.
- The docs say a tampered helper "can mishandle only that workspace's own files, within the direction and size the claim allowed". That holds for the transfer leg only. SECURITY.md item 11 disclaims DLP only for "clipboard and other in-desktop restrictions", and the file-transfer docs present the broker claim as "the checks that decide".
- Required fix (docs): in TM-37's residual, threat-model §B6 "In-pod transfer helper", the release notes' "In-pod trust" and session-policy.md "Trust model", state that the file-transfer and clipboard policies bind only an unmodified runtime. A user who replaces the `:8443` server can move data either way over the desktop leg with no grant, cap or transfer audit. Extend SECURITY.md item 11 to cover file transfer explicitly. Optional hardening, adopted here: the desktop-leg `responsePolicy` drops `Content-Disposition`. That removes the one-click download but not the channel.
- Security test: a docs-consistency check that the TM-37 residual and the release-notes limitation mention the desktop leg (here: assertions appended to `.github/tests/security-doc-claims.test.sh`). Also keep the proof test, inverted, as a characterization test that documents the accepted behaviour: `TestDesktopLeg_ReplacedHelperRelaysBytesWithoutDisposition`. It fails before the fix on the `Content-Disposition` assertion.

The channel itself stays the owner-accepted "In-pod trust" limitation (Decision 1). This task corrects how it is stated and removes the one-click download.

**Files:**
- Modify: `internal/gateway/proxy.go` `responsePolicy` (its doc comment and the header list; about line 1243 before Task 13's edit)
- Create: `internal/gateway/desktopleg_trust_test.go`
- Modify: `docs/security/threat-model.md:340-345` (B5 "Response policy"), `:506-511` (§B6 "In-pod transfer helper", new bullet), `:716` (TM-37 Residual cell), `:735-738` (§7 accepted residual 1)
- Modify: `docs/releases/v1.0.0.md:94-106` ("In-pod trust (file transfer)")
- Modify: `docs/runbooks/session-policy.md:102-107` ("Trust model")
- Modify: `SECURITY.md:668-669` (deployment checklist item 11)
- Modify: `.github/tests/security-doc-claims.test.sh` (append; Task 10 created it)

**Interfaces:**
- Consumes: package `gateway_test` helpers `newFakeBroker`, `(*fakeBroker).scriptTicket`, `(*fakeBroker).pointLeaseAt`, `(*fakeBroker).completions() []transferDone`, `newGateway`, `launchOK`, `proxied`, `testHost`, `testOrigin`, `testWSUID`. The `security-doc-claims.test.sh` helpers `need`, `absent` and `need_row` (Global Constraints).
- Produces: `responsePolicy` also deletes `Content-Disposition` on runtime-proxied responses. The transfer leg is not affected: it writes its own `Content-Disposition` in `transferDownload` (`internal/gateway/transfer.go:268`) and never passes through `responsePolicy`, so `TestTransfer_DownloadRoundTrip` stays green.

- [ ] **Step 1: Write the failing characterization test**

Create `internal/gateway/desktopleg_trust_test.go`:

```go
package gateway_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDesktopLeg_ReplacedHelperRelaysBytesWithoutDisposition characterizes
// the accepted in-pod trust residual (TM-37): a process that replaces the
// pod's :8443 server can serve arbitrary bytes on an allowlisted desktop
// path, and the gateway relays them with no transfer grant, cap or
// transfer audit — the documented limit of the in-pod policies. The
// gateway's own hardening is that it drops the upstream's
// Content-Disposition, so the desktop leg never offers a one-click
// download.
func TestDesktopLeg_ReplacedHelperRelaysBytesWithoutDisposition(t *testing.T) {
	secret := bytes.Repeat([]byte("TENANT-SECRET-ROW;"), 4<<20/18)
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/exfil.bin" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="db-dump.bin"`)
		_, _ = w.Write(secret)
	}))
	t.Cleanup(up.Close)
	fb := newFakeBroker(t) // no transfer grants
	fb.scriptTicket("tk-inpod", testWSUID)
	fb.pointLeaseAt(t, "tk-inpod", up)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-inpod")

	resp := proxied(t, srv, testHost, "/app/exfil.bin", cookie, map[string]string{"Origin": testOrigin})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// The accepted residual: the bytes cross with no grant and no transfer
	// completion. If this ever stops holding, update TM-37 and the
	// release notes' "In-pod trust" limitation.
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, secret) {
		t.Fatalf("desktop leg: status=%d bytes=%d — the documented residual changed; update TM-37", resp.StatusCode, len(body))
	}
	if n := len(fb.completions()); n != 0 {
		t.Fatalf("desktop leg recorded %d transfer completions, want 0", n)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		t.Fatalf("Content-Disposition = %q relayed from the runtime, want it dropped", cd)
	}
}
```

- [ ] **Step 2: Run it and see it fail**

Run: `go test ./internal/gateway/ -run TestDesktopLeg_ReplacedHelper -count=1 2>&1 | grep -E '^\s*(---|ok|FAIL|\S+_test.go)'`
Expected: FAIL with `Content-Disposition = "attachment; filename=\"db-dump.bin\"" relayed from the runtime, want it dropped` (checked on the planning host). The two residual assertions above it pass.

- [ ] **Step 3: Drop `Content-Disposition` on the desktop leg**

In `internal/gateway/proxy.go` `responsePolicy`, add `"Content-Disposition"` to the deleted list, on the line after `"Refresh", "Link",`:

```go
		"Refresh", "Link", "Content-Disposition",
```

Then append to its doc comment (after "…so they can never weaken the policy ServeHTTP pinned."): "Content-Disposition is dropped too: the desktop leg carries no file transfers (those go through the claimed transfer leg, which sets its own), so a runtime cannot turn a desktop path into a one-click download (TM-37)."

Run: `go test ./internal/gateway/ -run 'TestDesktopLeg_|TestTransfer_|TestProxy_HostileUpstreamHeadersDropped|TestHeaders_' -count=1 2>&1 | grep -E '^(--- FAIL|ok|FAIL)'`
Expected: `ok`.

- [ ] **Step 4: Docs**

Make each edit below. Find each by its quoted text, because Tasks 12–15 moved lines in `threat-model.md`.

1. `docs/security/threat-model.md`, §B6 "In-pod transfer helper": after the bullet that starts "- A replaced helper can change or withhold the bytes of that workspace's own transfers." and ends "the transfer leg sends only the token (`transfer.go:210,248`).", add:

```markdown
- The same replaced server also answers the desktop leg, which the
  gateway proxies on the KasmVNC client paths `/app/`, `/assets/`,
  `/core/` and `/websockify` (`upstreamPathOK`,
  `internal/gateway/authorization.go`). There, bytes move in either
  direction with no `ClaimTransfer`, no byte cap and no transfer audit
  row. The gateway drops the runtime's `Content-Disposition` on that leg
  (`responsePolicy`), which removes the one-click download but not the
  channel. The file-transfer and clipboard policies therefore bind an
  unmodified runtime only
  (`TestDesktopLeg_ReplacedHelperRelaysBytesWithoutDisposition`).
```

2. Same file, register row `| TM-37 |`: replace its Residual cell (the last cell, "Any process the workspace user starts can read `xfer-token` and replace the helper on `:8443`; it can then mishandle only that workspace's own transfers, within the claimed direction and size.") with:

```text
Any process the workspace user starts can read `xfer-token` and replace the helper on `:8443`. On the transfer leg it can then mishandle only that workspace's own transfers, within the claimed direction and size. On the desktop leg (`/app/`, `/assets/`, `/core/`, `/websockify`) it can move data in either direction with no grant, byte cap or transfer audit: the file-transfer and clipboard policies bind an unmodified runtime only.
```

Keep it on the row's single line. Use no literal `|`.

3. Same file, §7 "Accepted residuals" item 1: replace "the broker claim and the gateway's byte cap are the controls." with "the broker claim and the gateway's byte cap are the controls on the transfer leg. The desktop leg carries data either way with no grant, so the file-transfer and clipboard policies bind an unmodified runtime only."

4. Same file, Boundary 5 "Response policy" bullet: after "(`responsePolicy`, `internal/gateway/proxy.go`)." add "The upstream's `Content-Disposition` is dropped as well, so a desktop path is never offered as a download."

5. `docs/releases/v1.0.0.md`, "In-pod trust (file transfer)": replace "Net effect: a tampered helper can mishandle only that workspace's own files, within the direction and size the claim allowed." with:

```markdown
  Net effect on the transfer leg: a tampered helper can mishandle only
  that workspace's own files, within the direction and size the claim
  allowed. The file-transfer and clipboard policies bind an unmodified
  runtime only. A user who replaces the `:8443` server can move data in
  either direction over the desktop leg (the KasmVNC client paths
  `/app/`, `/assets/`, `/core/` and `/websockify`) with no grant, no
  byte cap and no transfer audit row. The gateway drops any
  `Content-Disposition` the runtime sends there, so the browser offers no
  one-click download, but the channel remains.
```

Keep the following "See §5 of [ADR 0009](…)." sentence.

6. `docs/runbooks/session-policy.md`, "**Trust model.**" paragraph: replace "so a tampered helper can mishandle only that workspace's own files within the allowed direction and size." with "so on the transfer leg a tampered helper can mishandle only that workspace's own files within the allowed direction and size. The file-transfer and clipboard policies bind an unmodified runtime only: a user who replaces the in-pod `:8443` server can move data either way over the desktop leg with no grant, byte cap or transfer audit."

7. `SECURITY.md`, deployment checklist item 11 (line 668): replace "**Clipboard and other in-desktop restrictions are UX defaults, not DLP.** The workspace user controls their own runtime." with "**Clipboard, file-transfer and other in-desktop restrictions are UX defaults, not DLP.** The workspace user controls their own runtime: by replacing the in-pod `:8443` server they can move data over the desktop leg with no transfer grant, cap or audit row."

8. Append to `.github/tests/security-doc-claims.test.sh`, directly above the final `exit "$fails"` line:

```bash
# R1-27: the in-pod trust residual names the desktop leg everywhere it is stated.
need_row R1-27 TM-37 'desktop leg'
need_row R1-27 TM-37 'bind an unmodified runtime only'
need     R1-27 docs/security/threat-model.md 'TestDesktopLeg_ReplacedHelperRelaysBytesWithoutDisposition'
need     R1-27 docs/releases/v1.0.0.md 'over the desktop leg'
absent   R1-27 docs/releases/v1.0.0.md 'Net effect: a tampered helper can mishandle only'
need     R1-27 docs/runbooks/session-policy.md 'over the desktop leg'
need     R1-27 SECURITY.md 'Clipboard, file-transfer and other in-desktop restrictions are UX defaults, not DLP'
```

To check that the assertions bite, run `git stash push docs/ SECURITY.md && bash .github/tests/security-doc-claims.test.sh; git stash pop`. It must print a `FAIL: R1-27 …` line for each of the seven assertions (the old release-notes sentence trips the `absent` one), and exit non-zero.

- [ ] **Step 5: Gates**

Run: `for t in .github/tests/*.test.sh; do bash "$t" >/dev/null || echo "FAIL $t"; done`
Expected: no output. `threat-model-refs.test.sh` resolves `TestDesktopLeg_ReplacedHelperRelaysBytesWithoutDisposition`, and `docs-gates.test.sh` passes the edited docs.

- [ ] **Step 6: Commit**

```bash
git add internal/gateway/proxy.go internal/gateway/desktopleg_trust_test.go docs/security/threat-model.md docs/releases/v1.0.0.md docs/runbooks/session-policy.md SECURITY.md .github/tests/security-doc-claims.test.sh
git commit -m "docs(security): in-pod trust covers the desktop leg; drop upstream Content-Disposition

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 17: KasmVNC refuses runtime clipboard overrides (R1-06) — tier: integrate

**Finding (R1-06, P2, TM-36).**
- **Attack.** A tenant admin sets the clipboard ceiling to `Disabled`, or the template sets `Disabled`/`Send`. The workspace user then runs `vncconfig -set SendCutText=1` and `vncconfig -set AcceptCutText=1` (and `SendPrimary=1`) from a terminal in their desktop. Any program in the session can do the same, because it needs only `DISPLAY`. From then on, Xvnc relays the clipboard both ways for the rest of the session. Nothing restarts or is logged.
- **Root cause.** The KasmVNC launcher (`/usr/bin/kasmvncserver:2347-2378`) passes `-AllowOverride` only when `runtime_configuration.allow_override_standard_vnc_server_settings` is `true`. With `false` it passes nothing, so Xvnc's compiled default applies: `desktop,AcceptPointerEvents,SendCutText,AcceptCutText,SendPrimary,SetPrimary`.
- **Required fix.** In `build/linux-base/kasmvnc.yaml` and in the adapter's generated config (`build/kasm-adapter/entrypoint.sh`), set `allow_override_standard_vnc_server_settings: true` and `allow_override_list: []`. Invariant: Xvnc's cmdline always carries `-AllowOverride`, and its value never names `SendCutText`, `AcceptCutText`, `SendPrimary` or `SetPrimary`. Update the `kasmvnc.yaml` comment and the threat-model B6 clipboard bullet. State the remaining residual: the user can still replace Xvnc itself.
- **Security test.** `TestLinuxRuntimeClipboardParamsNotOverridable` in `tests/integration/linux_runtime_test.go`, plus the same assertion in the kasm adapter IT. Boot with `TCDI_CLIPBOARD_POLICY=Disabled`. Assert the Xvnc cmdline has `-AllowOverride` with no clipboard param in its value, and that `vncconfig -display :1 -set SendCutText=1` leaves `-get SendCutText` at `0`.

**Checked by the planner at `98c7f54`** with the built `tcdi/linux-base:it` image (KasmVNC 1.5.0) and `TCDI_CLIPBOARD_POLICY=Disabled`:
- Today the Xvnc cmdline has no `-AllowOverride`. `vncconfig -display :1 -set SendCutText=1` prints nothing, and `-get SendCutText` goes from `0` to `1`.
- With `/etc/kasmvnc/kasmvnc.yaml` carrying `allow_override_standard_vnc_server_settings: true` and `allow_override_list: []`, the cmdline carries `-AllowOverride` followed by an empty argument. `-set SendCutText=1` and `-set AcceptCutText=1` print `setting param SendCutText=1 failed` (and the same for `AcceptCutText`), and `-get SendCutText` stays `0`.
- `vncconfig` exits `0` in both cases. Assert on the value read back, never on the exit code.
- The cataloged `kasmweb/chrome@sha256:4999d22e…` ships KasmVNC 1.5.1, whose launcher has the same `runtime_configuration.allow_override_list` handling (`/usr/bin/kasmvncserver:2348-2374`). Its `/usr/bin/vncconfig` exists.
- `docker exec` as uid 1000 with `HOME=/home/workspace` reaches `:1` without setting `XAUTHORITY`.

**Where these tests run in CI** (checked at `98c7f54`):
- No CI job runs `TestLinuxRuntime*`. `ci.yml:128` skips them all, and they need the `tcdi/linux-base:it`, `tcdi/linux-desktop:it` and `tcdi/browser:it` images. The new linux-base test runs locally with images built (`TCDI_IT_BUILD=1`).
- The kasm assertion runs in CI's `kasm-contract` job, which runs `-run TestKasmAdapterChromium` once per catalog entry (`ci.yml:369-381`). The job also runs on every PR that touches `build/kasm-adapter/`.
- The always-on local check is `.github/tests/clipboard-policy.test.sh` (Step 1). It runs in the required `workflow-policy` job.

**Files:**
- Modify: `build/linux-base/kasmvnc.yaml:32-38` (comment), `:50-52` (`runtime_configuration`)
- Modify: `build/kasm-adapter/entrypoint.sh:217-219` (`runtime_configuration` in the generated `$HOME/.vnc/kasmvnc.yaml` heredoc)
- Modify: `.github/tests/clipboard-policy.test.sh:106-122` (section 4, hardening pins)
- Modify: `tests/integration/linux_runtime_test.go` (a new helper and a new top-level test, after `TestLinuxRuntimeReadinessAndHome`, which ends before the next `func Test`)
- Modify: `tests/integration/kasm_adapter_test.go` (a new subtest in `TestKasmAdapterChromium`, after the `ReadinessEndpointAndAuth` subtest, which ends before `t.Run("TransferHelper"` at `:546`)
- Modify: `.github/workflows/ci.yml:128-131` (the integration job's `-skip` regex and its comment). This one-token edit keeps the cluster-free integration job from failing on an image-bound test. It is the only workflow edit in this task.
- Modify: `docs/security/threat-model.md:418-429` (B6 "Clipboard policy (server side)" bullet), `:715` (TM-36 Residual cell only; Task 30 owns the Control and Pinned-by cells)
- Modify: `deploy/helm/tinycdi/values.yaml:872-873` (the `clipboardPolicy` comment)

**Interfaces:**
- Consumes: the `integration` package helpers `requireImage`, `selfSignedSecret`, `runContainer`, `waitHealthy`, `mustExec`, `execIn` (`tests/integration/linux_runtime_test.go:130-400`); `runKasmContainer`, `waitHealthyTimeout`, `kasmRunTimeout` (`kasm_adapter_test.go:46,213`).
- Produces:
  - `func assertClipboardParamsNotOverridable(t *testing.T, c string)` in `tests/integration/linux_runtime_test.go`, shared by both images.
  - Top-level `TestLinuxRuntimeClipboardParamsNotOverridable` (image-bound, in the verify command's skip list).
  - Subtest `TestKasmAdapterChromium/ClipboardParamsNotOverridable`.
  - Gate lines in `clipboard-policy.test.sh` that fail when either config lacks `allow_override_standard_vnc_server_settings: true` plus `allow_override_list: []`.

- [ ] **Step 1: Write the failing local gate**

In `.github/tests/clipboard-policy.test.sh`, section 4, insert this block directly above the line `ok "hardening pins (client override off, system floor closed, DLP logging off)"`:

```bash
# R1-06: without allow_override_standard_vnc_server_settings: true the
# KasmVNC launcher passes NO -AllowOverride, so Xvnc's compiled default
# (desktop,AcceptPointerEvents,SendCutText,AcceptCutText,SendPrimary,
# SetPrimary) lets any X client re-enable the clipboard with
# `vncconfig -set SendCutText=1`. Both configs must turn the override
# list on and leave it empty, so the launcher renders -AllowOverride "".
for f in "$SYS_YAML" "$ADAPTER_EP"; do
  rc_block="$(awk '/^runtime_configuration:/{on=1; print; next} on && /^[^ ]/{exit} on && /^[[:space:]]*#/{next} on{print}' "$f")"
  grep -qx '  allow_override_standard_vnc_server_settings: true' <<<"$rc_block" ||
    bad "$(basename "$(dirname "$f")")/$(basename "$f"): runtime_configuration must set allow_override_standard_vnc_server_settings: true (else Xvnc keeps its default AllowOverride list)"
  grep -qx '  allow_override_list: \[\]' <<<"$rc_block" ||
    bad "$(basename "$(dirname "$f")")/$(basename "$f"): runtime_configuration must set allow_override_list: [] (no runtime-overridable VNC parameter)"
  if grep -qiE 'clipboard|cut_text|primary' <<<"$rc_block"; then
    bad "$(basename "$(dirname "$f")")/$(basename "$f"): runtime_configuration names a clipboard key"
  fi
done
```

Then change that `ok` line to:

```bash
ok "hardening pins (client override off, override list empty, system floor closed, DLP logging off)"
```

- [ ] **Step 2: Run it and see it fail**

Run: `bash .github/tests/clipboard-policy.test.sh 2>&1 | grep -E '^(FAIL|ok)' | tail -6`
Expected: four `FAIL:` lines, two per file: `linux-base/kasmvnc.yaml: runtime_configuration must set allow_override_standard_vnc_server_settings: true …`, `… allow_override_list: [] …`, and the same two for `kasm-adapter/entrypoint.sh`. Exit code 1.

- [ ] **Step 3: Write the image ITs (they fail before the fix when images are built)**

In `tests/integration/linux_runtime_test.go`, add this helper directly after `assertNoSecret` (ends before `func assertXferContract`):

```go
// assertClipboardParamsNotOverridable pins R1-06 on a running runtime
// container started with TCDI_CLIPBOARD_POLICY=Disabled. Xvnc must carry
// -AllowOverride with a value that names no clipboard parameter, and an X
// client inside the session must not be able to flip one with vncconfig:
// the parameter reads back unchanged. vncconfig exits 0 even when the
// server refuses a set, so the read-back is the assertion.
func assertClipboardParamsNotOverridable(t *testing.T, c string) {
	t.Helper()
	argv := strings.Split(strings.TrimRight(
		mustExec(t, c, `tr '\0' '\n' </proc/"$(pgrep -x Xvnc | head -n1)"/cmdline`), "\n"), "\n")
	idx := -1
	for i, a := range argv {
		if a == "-AllowOverride" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("Xvnc cmdline has no -AllowOverride, so its compiled default (SendCutText, AcceptCutText, SendPrimary, SetPrimary) is runtime-overridable:\n%s", strings.Join(argv, " "))
	}
	val := ""
	if idx+1 < len(argv) {
		val = argv[idx+1]
	}
	for _, p := range []string{"SendCutText", "AcceptCutText", "SendPrimary", "SetPrimary"} {
		if strings.Contains(strings.ToLower(val), strings.ToLower(p)) {
			t.Fatalf("Xvnc -AllowOverride %q names clipboard parameter %s", val, p)
		}
	}
	for _, p := range []string{"SendCutText", "AcceptCutText", "SendPrimary"} {
		before := strings.TrimSpace(mustExec(t, c, "vncconfig -display :1 -get "+p))
		if before != "0" {
			t.Fatalf("%s = %q before the override attempt, want 0 under TCDI_CLIPBOARD_POLICY=Disabled", p, before)
		}
		out, _ := execIn(t, c, "vncconfig -display :1 -set "+p+"=1 2>&1")
		after := strings.TrimSpace(mustExec(t, c, "vncconfig -display :1 -get "+p))
		if after != "0" {
			t.Fatalf("vncconfig -set %s=1 took effect (now %q; vncconfig said %q): an X client can re-enable the clipboard", p, after, strings.TrimSpace(out))
		}
	}
}
```

Add this top-level test directly after `TestLinuxRuntimeReadinessAndHome`:

```go
// TestLinuxRuntimeClipboardParamsNotOverridable (R1-06, TM-36): with the
// clipboard policy Disabled, nothing inside the session can re-enable the
// clipboard at runtime through Xvnc's parameter-override channel.
func TestLinuxRuntimeClipboardParamsNotOverridable(t *testing.T) {
	if _, err := os.Stat(seccompFile); err != nil {
		t.Fatalf("runtime seccomp profile missing: %s", seccompFile)
	}
	requireImage(t, baseImage, "build/linux-base")
	runBytes := make([]byte, 4)
	if _, err := rand.Read(runBytes); err != nil {
		t.Fatal(err)
	}
	runID := fmt.Sprintf("%x", runBytes)
	secret, _ := selfSignedSecret(t)
	c := runContainer(t, runID, "clip", baseImage, secret, "",
		"-e", "TCDI_CLIPBOARD_POLICY=Disabled")
	waitHealthy(t, c)
	assertClipboardParamsNotOverridable(t, c)
}
```

In `tests/integration/kasm_adapter_test.go`, inside `TestKasmAdapterChromium`, add this subtest directly before `t.Run("TransferHelper", …)`:

```go
	// R1-06 (TM-36): the adapter's generated kasmvnc.yaml turns the
	// override list on and leaves it empty, so no X client in the session
	// can flip a clipboard parameter at runtime.
	t.Run("ClipboardParamsNotOverridable", func(t *testing.T) {
		secret, _ := selfSignedSecret(t)
		c := runKasmContainer(t, runID, "clip", kasmImage, adapterVol(), secret, "", layout,
			"-e", "TCDI_CLIPBOARD_POLICY=Disabled")
		waitHealthyTimeout(t, c, kasmRunTimeout)
		assertClipboardParamsNotOverridable(t, c)
	})
```

Both files already import `crypto/rand`, `fmt`, `os` and `strings`. Check with `go vet -tags=integration ./tests/integration/` (expected: no output).

In `.github/workflows/ci.yml:128`, change the skip regex to `'^TestLinuxRuntime(ReadinessAndHome|FirefoxFallback|Desktop|BrowserBaseline|ClipboardParamsNotOverridable)$|^TestKasmAdapter'`. In the comment below it (lines 130-131), change `TestLinuxRuntimeDesktop, TestLinuxRuntimeBrowserBaseline` to `TestLinuxRuntimeDesktop, TestLinuxRuntimeBrowserBaseline, TestLinuxRuntimeClipboardParamsNotOverridable`.

If Docker and the images are available, see it fail before the fix:

```bash
docker image inspect tcdi/linux-base:it >/dev/null 2>&1 || docker build -f build/linux-base/Dockerfile -t tcdi/linux-base:it .
go test -tags=integration -count=1 ./tests/integration -run '^TestLinuxRuntimeClipboardParamsNotOverridable$' -v 2>&1 | grep -E '^(=== RUN|--- |\s+linux_runtime_test)' | head
```

Expected: `--- FAIL: TestLinuxRuntimeClipboardParamsNotOverridable` with `Xvnc cmdline has no -AllowOverride …`. Without Docker the test cannot run, and a skip is not a pass. Step 2's gate is then the failing proof, and CI's `kasm-contract` job is the image-level check for the adapter.

- [ ] **Step 4: Fix the linux-base system config**

`build/linux-base/kasmvnc.yaml`, replace lines 50-52:

```yaml
runtime_configuration:
  allow_client_to_override_kasm_server_settings: false
  allow_override_standard_vnc_server_settings: false
```

with:

```yaml
# The launcher passes Xvnc an -AllowOverride list ONLY when
# allow_override_standard_vnc_server_settings is true. With false it passes
# none, and Xvnc's compiled default lets any X client in the session flip
# SendCutText/AcceptCutText/SendPrimary/SetPrimary with vncconfig. true plus
# an empty list renders -AllowOverride "": no parameter is runtime-settable.
runtime_configuration:
  allow_client_to_override_kasm_server_settings: false
  allow_override_standard_vnc_server_settings: true
  allow_override_list: []
```

In the clipboard comment (lines 32-38), replace the last sentence, "Client-side overrides are disabled entirely so the browser cannot re-enable server policy at runtime.", with: "Client-side overrides are disabled, and runtime_configuration below leaves no VNC parameter runtime-overridable, so neither the browser nor an X client inside the session can re-enable the clipboard after start."

The per-user `$HOME/.vnc/kasmvnc.yaml` that `build/linux-base/entrypoint.sh:142-158` rewrites on every boot sets only `logging` and `data_loss_prevention`, so the system `runtime_configuration` is the one in force. Leave the entrypoint unchanged.

- [ ] **Step 5: Fix the kasm adapter's generated config**

In `build/kasm-adapter/entrypoint.sh`, in the `cat > "$VNC_TMP" <<YAML` heredoc, replace:

```yaml
runtime_configuration:
  allow_client_to_override_kasm_server_settings: false
  allow_override_standard_vnc_server_settings: false
```

with:

```yaml
runtime_configuration:
  allow_client_to_override_kasm_server_settings: false
  # true + empty list => the launcher passes -AllowOverride "": no VNC
  # parameter (SendCutText/AcceptCutText/SendPrimary/SetPrimary included)
  # can be changed at runtime by an X client in the session.
  allow_override_standard_vnc_server_settings: true
  allow_override_list: []
```

The heredoc is unquoted (`<<YAML`). `[]` and `+` are literal in it. Do not add `$`, backticks or `\` to these lines.

- [ ] **Step 6: Run the gates and the ITs**

```bash
bash .github/tests/clipboard-policy.test.sh 2>&1 | tail -3
go vet -tags=integration ./tests/integration/
```

Expected: `clipboard-policy.test.sh: PASS`; vet prints nothing.

With Docker, rebuild and run the image test:

```bash
docker build -f build/linux-base/Dockerfile -t tcdi/linux-base:it . > /tmp/r1-06-build.log 2>&1; echo "build rc=$?"
go test -tags=integration -count=1 ./tests/integration -run '^TestLinuxRuntimeClipboardParamsNotOverridable$' -v 2>&1 | grep -E '^--- '
```

Expected: `build rc=0`, then `--- PASS: TestLinuxRuntimeClipboardParamsNotOverridable`. The kasm subtest runs in CI's `kasm-contract` job, because this task touches `build/kasm-adapter/` and `tests/integration/kasm_adapter_test.go`. Run it locally only when a cataloged `kasmweb/*` image is available: `docker build -f build/kasm-adapter/Dockerfile -t tcdi/kasm-adapter:it . && TCDI_IT_KASM_IMAGE="$(awk '!/^#/ && NF {print $1; exit}' build/kasm-catalog.txt)" go test -tags=integration -count=1 -timeout 20m ./tests/integration -run 'TestKasmAdapterChromium/ClipboardParamsNotOverridable' -v 2>&1 | grep -E '^\s*--- '`.

- [ ] **Step 7: Docs**

`docs/security/threat-model.md`, B6 bullet "Clipboard policy (server side)" (lines 418-429): replace the sentence "Client-side overrides and DLP logging stay off." with:

"Client-side overrides and DLP logging stay off, and `runtime_configuration` turns Xvnc's parameter-override list on with no entries (`allow_override_standard_vnc_server_settings: true`, `allow_override_list: []`, rendered as `-AllowOverride ""`). An X client in the session therefore cannot flip `SendCutText`, `AcceptCutText`, `SendPrimary` or `SetPrimary` with `vncconfig` (`.github/tests/clipboard-policy.test.sh`, `TestLinuxRuntimeClipboardParamsNotOverridable`, `TestKasmAdapterChromium`)."

At the end of the bullet, after "enforcement is the server's `SendCutText` / `AcceptCutText` (TM-36).", add:

"The policy binds an unmodified runtime: a process the workspace user starts can still stop Xvnc and run a server of its own, the same in-pod trust limit as the transfer helper (§7 item 1)."

TM-36 register row (line 715), Residual cell: replace `A running workspace keeps its start-time policy until it restarts (TM-17).` with `A running workspace keeps its start-time policy until it restarts (TM-17). The policy binds an unmodified runtime: the workspace user can replace Xvnc itself (§7 item 1).` Do not touch the Control or Pinned-by cells; Task 30 owns them.

`deploy/helm/tinycdi/values.yaml:872-873` (inside the commented `templates:` example): replace `# Enforced server-side by the session server's KasmVNC DLP block —` with `# Enforced server-side by the session server's KasmVNC DLP block (no VNC parameter is runtime-overridable) —`. This is a comment inside a commented example, so the default render is unchanged. Confirm: `cd deploy/helm && go test -count=1 -run TestDefaultRender ./ 2>&1 | tail -1` gives `ok`.

- [ ] **Step 8: Gates and commit**

```bash
go test -count=1 ./deploy/helm/ > /tmp/r1-06-helm.log 2>&1; echo "helm rc=$?"
for t in .github/tests/*.test.sh; do bash "$t" > /tmp/r1-06-gate.log 2>&1 || { echo "FAIL $t"; tail -20 /tmp/r1-06-gate.log; exit 1; }; done; echo gates-ok
git add build/linux-base/kasmvnc.yaml build/kasm-adapter/entrypoint.sh .github/tests/clipboard-policy.test.sh \
  tests/integration/linux_runtime_test.go tests/integration/kasm_adapter_test.go .github/workflows/ci.yml \
  docs/security/threat-model.md deploy/helm/tinycdi/values.yaml
git commit -m "fix(runtime): KasmVNC refuses runtime overrides of clipboard parameters" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

Expected: `helm rc=0`, `gates-ok`.

### Task 18: podPidsLimit install requirement and preflight check (R1-07) — tier: integrate

**Finding (R1-07, P2, new threat B6/B8; the nearest rows are TM-44 and TM-35).**
- **Attack.** An authenticated user with a shell in a workspace whose memory limit is about 8Gi runs a thread bomb, such as a pthread loop with 16 KiB stacks. The kubelet's default `podPidsLimit` is `-1`, and the product neither sets nor documents a value. So the process count is bounded only by the pod's memory cgroup, at about 29 KiB of charged memory per thread, which allows about 290k threads. The node's `kernel.threads-max` is about RAM/128 KiB (255,875 on a 31 GiB host). Once it is used up, every `fork`/`clone` on the node fails: the kubelet, containerd-shim and other tenants' workspaces on the shared pool.
- **Required fix (as amended by Decision 4).** Require a per-pod PID bound on the workspace pool through the kubelet's `podPidsLimit`. Document it in `install.md` step 9, and have preflight check it. Do **not** add the brief's in-pod `/sys/fs/cgroup/pids.max` check or an operator condition. Inside a container with a private cgroup namespace, that file shows the container's own cgroup, not the pod-level cgroup where kubelet applies `podPidsLimit`. This was checked on the planning host: under a parent slice with `pids.max=123`, a container read `38381`. An operator condition would need `nodes/proxy` RBAC.
- **Owner-accepted residual (Decision 1).** The product cannot set or enforce the kubelet setting, which is node configuration. The release notes and the threat model say so.
- **Security tests.**
  - New `pids` cases in `.github/tests/preflight.test.sh`: a node with `podPidsLimit: -1`, or without the key, WARNs and is named.
  - `security-doc-claims.test.sh` lines: `install.md`, the kind config, the release notes, the threat model and the preflight README name the bound.

  Both fail before this task.

**Files:**
- Modify: `hack/preflight/preflight.sh`: usage text `:47-74` (no new flag; the help line about checks only), a new `check_pids` after `check_apparmor` (which ends before `# ---- 6. wildcard DNS` at `:646`), and the call list `:999-1008`
- Modify: `.github/tests/preflight.test.sh`: kubectl shim `:16-86` (a new `configz` case), the "all green" block `:177-182`, and a new section after the "5b." AppArmor block (which ends before `echo "== 6. wildcard DNS"`)
- Modify: `hack/preflight/README.md:26-37` (check table)
- Modify: `hack/quickstart/kind-config.yaml` (top-level `kubeadmConfigPatches`)
- Modify: `docs/runbooks/install.md:57-68` (prerequisites paragraph) and step 9 (`:175-202`)
- Modify: `docs/releases/v1.0.0.md` (Known limitations, a new bullet after "Node drain loses running desktops")
- Modify: `docs/security/threat-model.md:404-409` (B6 "Pod hardening" bullet) and §7 Accepted residuals (`:731-762`, a new item 11)
- Modify: `.github/tests/security-doc-claims.test.sh` (created by Task 10; append above the final `exit "$fails"`)

**Interfaces:**
- Consumes: preflight helpers `kq`, `report`, `KOUT`/`KRC`/`KFORBID`, `NODE_LABEL`, `ALLOW_SHARED` (`hack/preflight/preflight.sh:135-170`); test helpers `run`, `expect`, `expect_out`, `expect_log`, `expect_no_log`, `expect_fix`, `expect_rc` (`.github/tests/preflight.test.sh:141-165`); the node-list shim branch keyed on `nodeInfo.operatingSystem` (`:76-81`, stubs `STUB_AA_NODES_LABELED`/`STUB_AA_NODES_ALL`).
- Produces:
  - Preflight check id `pids`, which prints `PASS`/`WARN` and never `FAIL`.
  - Shim stubs `STUB_PIDS` (`node=value;…`, where `absent` omits the key), `STUB_PIDS_DEFAULT` (default `4096`) and `STUB_CONFIGZ_FORBIDDEN`.
  - Release-notes bullet title `**Process count needs a kubelet bound.**` and threat-model §7 item 11 with the same title. Task 30 adds the register row and its "See TM-NN" reference.

- [ ] **Step 1: Write the failing tests**

In `.github/tests/preflight.test.sh`, inside the `cat > "$SHIM/kubectl" <<'EOF'` case, add this branch directly after the `*"get --raw /version"*)` branch (which ends with `;;` before `*"auth can-i create namespaces"*)`):

```bash
  *"get --raw /api/v1/nodes/"*"/proxy/configz"*)
    [ -n "${STUB_CONFIGZ_FORBIDDEN:-}" ] && forbidden nodes/proxy
    n="${args#*/api/v1/nodes/}"; n="${n%%/*}"
    lim="$(printf '%s' "${STUB_PIDS:-}" | tr ';' '\n' | sed -n "s|^$n=||p" | head -n1)"
    lim="${lim:-${STUB_PIDS_DEFAULT:-4096}}"
    if [ "$lim" = absent ]; then
      echo '{"kubeletconfig":{"maxPods":110}}'
    else
      echo "{\"kubeletconfig\":{\"maxPods\":110,\"podPidsLimit\":$lim}}"
    fi ;;
```

In the "all green" block, change the loop list and the summary to:

```bash
for c in k8s-version netpol storageclass host-users node-pool apparmor pids dns tls-secret oidc postgres; do
  expect "all green: $c" PASS "$c"
done
expect_out "summary line" '^preflight: 11 PASS, 0 WARN, 0 FAIL$'
```

Add this section directly before `echo "== 6. wildcard DNS"`:

```bash
echo "== 5c. process bound (kubelet podPidsLimit)"
run -- "${ALL[@]}"
expect "every workspace node bounded passes" PASS pids
expect_out "the lowest bound is shown" 'podPidsLimit set on 2 workspace node\(s\) \(lowest 4096\)'
expect_log "configz read through the node proxy" 'get --raw /api/v1/nodes/n1/proxy/configz'
expect_log "second pool node read" 'get --raw /api/v1/nodes/n2/proxy/configz'
run STUB_PIDS='n2=-1' -- "${ALL[@]}"
expect "a node with podPidsLimit -1 warns" WARN pids
expect_out "the unbounded node is named" 'unset or -1 on: n2'
expect_rc "an unbounded node is a WARN, not a FAIL" 0
expect_fix "fix lines"
run STUB_PIDS='n1=absent' -- "${ALL[@]}"
expect "a node without the key warns" WARN pids
expect_out "the node without the key is named" 'unset or -1 on: n1'
run STUB_PIDS='n1=0' -- "${ALL[@]}"
expect "podPidsLimit 0 is not a bound" WARN pids
run STUB_PIDS_DEFAULT=8192 STUB_PIDS='n1=2048' -- "${ALL[@]}"
expect "mixed positive bounds pass" PASS pids
expect_out "the lowest of mixed bounds is shown" '\(lowest 2048\)'
run STUB_CONFIGZ_FORBIDDEN=1 -- "${ALL[@]}"
expect "configz forbidden warns" WARN pids
expect_out "the missing permission is named" 'nodes/proxy'
run STUB_NODES_FORBIDDEN=1 -- "${ALL[@]}"
expect "node list forbidden warns" WARN pids
run STUB_AA_NODES_LABELED= -- "${ALL[@]}"
expect "no workspace node warns" WARN pids
expect_no_log "no configz read without a node" 'proxy/configz'
run STUB_PIDS='w2=-1' STUB_AA_NODES_ALL='w1|linux||\nw2|linux||\nw3|windows||\n' -- "${ALL[@]}" --allow-shared-nodes
expect "shared nodes: every Linux node is checked" WARN pids
expect_out "the shared unbounded node is named" 'unset or -1 on: w2'
expect_no_log "a non-Linux node is skipped" 'nodes/w3/proxy'
run -- "${ALL[@]}" --no-probe
expect "--no-probe still reads configz (read-only)" PASS pids
```

Append to `.github/tests/security-doc-claims.test.sh`, directly above the final `exit "$fails"`:

```bash
# R1-07: a workspace's process/thread count is bounded only by the kubelet's
# podPidsLimit; the install guide requires it, preflight checks it, the
# quickstart cluster runs bounded, and the residual is stated.
need R1-07 docs/runbooks/install.md 'podPidsLimit'
need R1-07 docs/runbooks/install.md 'kernel\.threads-max'
need R1-07 hack/quickstart/kind-config.yaml 'kind: KubeletConfiguration podPidsLimit: [1-9][0-9]*'
need R1-07 hack/preflight/README.md '\| `pids` \|'
need R1-07 docs/releases/v1.0.0.md 'Process count needs a kubelet bound\.'
need R1-07 docs/security/threat-model.md 'Process count needs a kubelet bound\.'
need R1-07 docs/security/threat-model.md '\*\*Pod hardening\*\*:[^*]*podPidsLimit'
```

- [ ] **Step 2: Run them and see them fail**

```bash
bash .github/tests/preflight.test.sh 2>&1 | grep -E '^FAIL' | head -12
bash .github/tests/security-doc-claims.test.sh 2>&1 | grep -E '^FAIL: R1-07' 
```

Expected for preflight:
- `FAIL: all green: pids (want PASS pids)`;
- `FAIL: summary line (no match for …11 PASS…)`;
- `FAIL: every workspace node bounded passes (want PASS pids)` and the other `pids` expectations (no `pids` line exists yet);
- `FAIL: configz read through the node proxy (log lacks …)`.

Expected for the doc claims: seven `FAIL: R1-07: …` lines.

- [ ] **Step 3: Implement the preflight check**

In `hack/preflight/preflight.sh`, add this function directly before the line `# ---- 6. wildcard DNS ------------------------------------------------------`:

```bash
# ---- 5c. process bound on the workspace nodes -----------------------------
# Kubernetes has no pod-spec process limit: a workspace's process and thread
# count is bounded only by the kubelet's podPidsLimit (default -1, none), or
# else by its memory limit, and one desktop can then exhaust the node's
# kernel.threads-max and break fork for every pod on it, the kubelet's
# included. The value is node configuration; the kubelet reports it on its
# configz endpoint, read here through the apiserver node proxy (get on
# nodes/proxy, an administrator right the platform itself never holds).
# Read-only: nothing is created, so --no-probe does not skip it. An in-pod
# check cannot replace this: a container's own pids.max is not the
# pod-level cgroup the kubelet bounds.
check_pids() {
  local lines name os rest lim lowest=""
  local -a nodes=() unbounded=() unknown=()
  local denied=0
  if [ "$ALLOW_SHARED" = 1 ]; then
    kq get nodes -o 'jsonpath={range .items[*]}{.metadata.name}{"|"}{.status.nodeInfo.operatingSystem}{"|"}{.spec.unschedulable}{"|"}{range .spec.taints[*]}{.key}{":"}{.effect}{";"}{end}{"\n"}{end}'
  else
    kq get nodes -l "$NODE_LABEL" -o 'jsonpath={range .items[*]}{.metadata.name}{"|"}{.status.nodeInfo.operatingSystem}{"|"}{.spec.unschedulable}{"|"}{range .spec.taints[*]}{.key}{":"}{.effect}{";"}{end}{"\n"}{end}'
  fi
  if [ "$KRC" != 0 ]; then
    if [ "$KFORBID" = 1 ]; then
      report WARN pids "not permitted to list nodes" "grant list on nodes, or check podPidsLimit in each workspace node's kubelet config by hand"
    else
      report WARN pids "could not list nodes" "see kubectl error output; check podPidsLimit in each workspace node's kubelet config by hand"
    fi
    return
  fi
  lines="$(printf '%s\n' "$KOUT" | sed '/^$/d')"
  while IFS='|' read -r name os rest; do
    [ -z "$name" ] && continue
    [ "$os" = linux ] || continue
    nodes+=("$name")
  done <<<"$lines"
  if [ "${#nodes[@]}" = 0 ]; then
    report WARN pids "no workspace node found" "fix the node pool first (see node-pool), then set podPidsLimit on its kubelets"
    return
  fi
  for name in "${nodes[@]}"; do
    if ! kq get --raw "/api/v1/nodes/$name/proxy/configz"; then
      [ "$KFORBID" = 1 ] && denied=1
      unknown+=("$name")
      continue
    fi
    lim="$(printf '%s' "$KOUT" | sed -n 's/.*"podPidsLimit":[[:space:]]*\(-\{0,1\}[0-9][0-9]*\).*/\1/p' | head -n1)"
    if [ -z "$lim" ] || [ "$lim" -le 0 ]; then
      unbounded+=("$name")
    elif [ -z "$lowest" ] || [ "$lim" -lt "$lowest" ]; then
      lowest="$lim"
    fi
  done
  if [ "${#unbounded[@]}" -gt 0 ]; then
    local detail="kubelet podPidsLimit unset or -1 on: ${unbounded[*]}"
    [ "${#unknown[@]}" -gt 0 ] && detail="$detail; not readable on: ${unknown[*]}"
    report WARN pids "$detail" \
      "set podPidsLimit (for example 4096) in the kubelet configuration of every workspace node; see docs/runbooks/install.md step 9"
  elif [ "${#unknown[@]}" -gt 0 ]; then
    if [ "$denied" = 1 ]; then
      report WARN pids "not permitted to read the kubelet configz (get nodes/proxy) on: ${unknown[*]}" \
        "grant get on nodes/proxy for this check, or check podPidsLimit in each workspace node's kubelet config by hand"
    else
      report WARN pids "could not read the kubelet configz on: ${unknown[*]}" \
        "check podPidsLimit in each workspace node's kubelet config by hand"
    fi
  else
    report PASS pids "podPidsLimit set on ${#nodes[@]} workspace node(s) (lowest $lowest)"
  fi
}

```

In the call list at the end of the file, add `check_pids` directly after `check_apparmor`:

```bash
check_node_pool
check_apparmor
check_pids
check_dns
```

In `usage()`, change the `--no-probe` line to:

```
  --no-probe                create nothing: NetworkPolicy, user-namespace and AppArmor checks WARN
```

Run:

```bash
bash .github/tests/preflight.test.sh > /tmp/r1-07-preflight.log 2>&1; echo "rc=$?"; grep -E '^FAIL' /tmp/r1-07-preflight.log | head
```

Expected: `rc=0` and no `FAIL` lines. The test's own shellcheck pass covers the new function.

- [ ] **Step 4: Bound the quickstart cluster**

Append to `hack/quickstart/kind-config.yaml`, after the `nodes:` list, at top level:

```yaml
# Workspace pods have no pod-spec process limit; the kubelet's podPidsLimit
# is the bound (docs/runbooks/install.md step 9). The quickstart runs with
# one so CI's kind clusters exercise a bounded node.
kubeadmConfigPatches:
  - |
    kind: KubeletConfiguration
    podPidsLimit: 4096
```

`hack/quickstart/sandbox-up.sh:52-53` derives its config with `sed` deleting only `extraPortMappings:` and `containerPort:…hostPort:` lines, so the patch carries over. Check the YAML parses: `python3 -c 'import yaml,sys; d=yaml.safe_load(open("hack/quickstart/kind-config.yaml")); assert "podPidsLimit: 4096" in d["kubeadmConfigPatches"][0]'` (expected: no output). The CI `quickstart` and `partitioned` jobs create the cluster from this file.

- [ ] **Step 5: Docs**

`hack/preflight/README.md`, check table: add this row directly after the `apparmor` row:

```markdown
| `pids` | every workspace node's kubelet reports `podPidsLimit` > 0 on its `configz` endpoint (read through the node proxy; read-only) | a node reports `-1`/`0` or no value (the node names are printed), `configz` not readable (no `get` on `nodes/proxy`), nodes could not be listed, no workspace node | never — the value is node configuration, not a chart value |
```

`docs/runbooks/install.md`, "Prerequisites check" paragraph: replace `AppArmor on the workspace nodes (it names the fitting `runtime.appArmor.requireRuntimeDefault` value), wildcard DNS,` with `AppArmor on the workspace nodes (it names the fitting `runtime.appArmor.requireRuntimeDefault` value), the kubelet process bound on the workspace nodes (`podPidsLimit`), wildcard DNS,`.

`docs/runbooks/install.md`, step 9: insert this paragraph directly before `   **Shared nodes (opt-out).**`:

```markdown
   **Process bound (required).** The pod spec has no process limit, so a
   workspace's process and thread count is bounded only by the kubelet's
   `podPidsLimit`, and its default (`-1`) is unlimited. Without it, one
   desktop can use up the node's `kernel.threads-max` and make `fork`
   fail for every pod on the node, the kubelet included. Set
   `podPidsLimit` (for example `4096`) in the kubelet configuration of
   every node that runs workspaces, or reserve PIDs with
   `--system-reserved=pid=<n>` and a `pid.available` eviction threshold.
   This is node configuration: TinyCDI cannot set it. The preflight
   `pids` check reads each workspace node's value from the kubelet
   `configz` endpoint and warns when it is unset or `-1`. On a shared
   node (`allowSharedNodes: true`) the same bound protects the unrelated
   workloads there.

```

`docs/releases/v1.0.0.md`, Known limitations: insert this bullet directly after the "**Node drain loses running desktops.**" bullet:

```markdown
- **Process count needs a kubelet bound.** Kubernetes has no per-pod
  process limit in the pod spec, so a workspace's process and thread count
  is bounded only by the kubelet's `podPidsLimit`. TinyCDI cannot set
  that value: it is node configuration. Without it, one workspace can
  exhaust its node's thread table and break every pod on the node. The
  install guide requires the setting on the workspace pool, and preflight
  checks it. See [install.md](../runbooks/install.md).
```

`docs/security/threat-model.md`, B6 "Pod hardening" bullet (`:404-409`): append this sentence after "…(`AppArmorNotRequired`, TM-41).":

"The pod spec has no process limit: the process and thread count is bounded by the kubelet's `podPidsLimit` on the workspace nodes, which the install guide requires and the preflight `pids` check reads. Without it, a workspace bounded only by its memory limit can exhaust the node's `kernel.threads-max`."

`docs/security/threat-model.md` §7: add after item 10:

```markdown
11. **Process count needs a kubelet bound.** The product cannot set or
    enforce the kubelet's `podPidsLimit`; it requires it at install and
    checks it in preflight. See [Pod hardening](#boundary-6--session-listener--workspace-pod-tcdi-xferd-8443--kasmvnc-1270018444).
```

Check the anchor before you write it. Run `python3 .github/scripts/check-doc-links.py docs/security/threat-model.md` after editing. If it reports the fragment, use the anchor it expects for the "Boundary 6" heading at `threat-model.md:367`.

- [ ] **Step 6: Run the doc gates and focused checks**

```bash
bash .github/tests/security-doc-claims.test.sh 2>&1 | grep -E '^FAIL' ; echo "claims rc=${PIPESTATUS[0]}"
bash .github/tests/docs-gates.test.sh > /tmp/r1-07-docs.log 2>&1; echo "docs rc=$?"; grep -E '^FAIL' /tmp/r1-07-docs.log | head
bash .github/tests/preflight.test.sh > /tmp/r1-07-preflight.log 2>&1; echo "preflight rc=$?"
```

Expected: no `FAIL` lines, and `claims rc=0`, `docs rc=0`, `preflight rc=0`.

- [ ] **Step 7: Gates and commit**

```bash
for t in .github/tests/*.test.sh; do bash "$t" > /tmp/r1-07-gate.log 2>&1 || { echo "FAIL $t"; tail -20 /tmp/r1-07-gate.log; exit 1; }; done; echo gates-ok
git add hack/preflight/preflight.sh hack/preflight/README.md .github/tests/preflight.test.sh \
  hack/quickstart/kind-config.yaml docs/runbooks/install.md docs/releases/v1.0.0.md \
  docs/security/threat-model.md .github/tests/security-doc-claims.test.sh
git commit -m "docs(install): require a kubelet podPidsLimit on workspace nodes and check it in preflight" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

Expected: `gates-ok`.

### Task 19: ClusterOnly egress in-tenant; NetworkPolicy before the pod (R1-28, R1-35) — tier: integrate

**Finding R1-28 (P3, TM-34).**
- **Attack.** A ClusterOnly workspace user in tenant A can open any TCP or UDP port on any pod in tenant B's managed namespace. The default egress peer is `namespaceSelector: {workspaces.cdi.tinyorbit.vn/tenant Exists}`, which matches every tenant's namespaces. Only the destination's own policy stops this today. Any service the platform later runs in a tenant namespace with a looser ingress rule is reachable across tenants.
- **Required fix (Decision 6).** With `ClusterOnlyEgress == nil`, the peer is `matchLabels: {workspaces.cdi.tinyorbit.vn/tenant: <the workspace's own tenant>}`. Cross-tenant reach stays behind the explicit selector or `unrestricted`. Update SECURITY.md item 23 and TM-34.
- **Security test.** `TestClusterOnlyEgressDefaultSameTenantOnly` in `internal/runtime/linux/backend_netpol_test.go`. The default peer does not match a namespace labelled with another tenant, and does match the workspace's own namespace and a second namespace of the same tenant.

**Finding R1-35 (P3, new threat B8).**
- **Attack.** On an install with `networkPolicy.enabled=false` (allowed in production, so the chart renders no namespace default-deny), `ensureService` keeps failing. Causes include the opt-in ResourceQuota counting `services`, a name conflict or an apiserver error. The runtime pod is already running, because `Ensure` creates Secret, PVC, Pod, Service and only then the NetworkPolicy (`internal/runtime/linux/backend.go:508-524`). The tenant-controlled pod then has unrestricted ingress and egress (other tenants, cloud metadata `169.254.169.254`, node ports) on every retry.
- **Required fix.** Call `ensureNetPol` before `ensurePod`. The policy selects by the workspace-UID label, so it can exist first.
- **Security test.** `TestEnsure_NetPolPrecedesPod` in `internal/runtime/linux/backend_netpol_test.go`. With Service creation failing through an interceptor, the NetworkPolicy already exists, and a create-order recorder shows it created before the Pod.

**How the operator learns the tenant (no new RBAC).** Every Workspace CR the API creates carries the label `workspaces.cdi.tinyorbit.vn/tenant: <tenantID>` (`internal/provisioning/k8sapplier.go:34,185`). The chart labels each managed namespace with the same value, `workspaces.cdi.tinyorbit.vn/tenant: {{ .tenant }}` (`deploy/helm/tinycdi/templates/namespaces.yaml:12`), and the backend maps the same tenant id to its namespaces. `buildNetPol` reads the label from the CR it already has, so no Namespace read is needed. A CR without the label fails closed to the workspace's own namespace (`kubernetes.io/metadata.name: <ws.Namespace>`). The `linux` package does not import `internal/provisioning`, so it uses a local constant with the same value.

**Files:**
- Modify: `internal/runtime/linux/backend.go`:
  - `:376-385` (`Options.ClusterOnlyEgress` comment);
  - `:484-525` (`Ensure`: order);
  - `:1544-1547` (`buildNetPol` call site);
  - `:1595-1640` (`clusterOnlyEgressRule`: signature, default branch, comment);
  - a new `labelTenant` const and a new `sameTenantNamespaces` func next to it.
- Modify: `cmd/operator/main.go:434-439` (`--runtime-clusteronly-egress` help text)
- Test: `internal/runtime/linux/backend_netpol_test.go` (new tests; update `TestClusterOnlyEgressDefaultRestricted` at `:287-322` and the drift check at the end of `TestClusterOnlyEgressRestrictionOnPolicy`, `:408-423`)
- Test: `internal/runtime/linux/backend_test.go:372-398` (`TestClusterOnlyEgress_DefaultRestricted`, `TestClusterOnlyEgress_UnrestrictedOptIn`: new signature)
- Docs: `SECURITY.md:410-415` (item 23); `docs/security/threat-model.md:381-384` (B6 NetworkPolicy bullet; Task 30 owns the TM-34 Control/Pinned-by cells); `deploy/helm/tinycdi/README.md:281,288-291`; `docs/releases/v1.0.0.md:50-51`

**Interfaces:**
- Consumes: `testWorkspace()`, `testTemplate()`, `backendScheme()` (`backend_test.go:36-82`); `netPolFor()` (`backend_netpol_test.go:30`); `NetPolName`, `PodName`, `ServiceName`.
- Produces:
  - `const labelTenant = "workspaces.cdi.tinyorbit.vn/tenant"` (package `linux`, unexported).
  - `func sameTenantNamespaces(ws *cdiv1.Workspace) *metav1.LabelSelector`.
  - `func clusterOnlyEgressRule(ws *cdiv1.Workspace, opts Options) networkingv1.NetworkPolicyEgressRule`, which replaces `clusterOnlyEgressRule(opts Options)`.
  - `Ensure` creation order: Secret → PVC (Retain) → NetworkPolicy → Pod → Service.

- [ ] **Step 1: Write the failing tests**

In `internal/runtime/linux/backend_netpol_test.go`, extend the import block to:

```go
import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
)
```

The `labels` package is imported as `k8slabels`, because package `linux` already declares a function `labels` (`backend.go`).

Append:

```go
// nsLabels is the label set a managed namespace carries: the apiserver's
// own name label plus the chart's tenant label (namespaces.yaml).
func nsLabels(name, tenant string) k8slabels.Set {
	s := k8slabels.Set{"kubernetes.io/metadata.name": name}
	if tenant != "" {
		s["workspaces.cdi.tinyorbit.vn/tenant"] = tenant
	}
	return s
}

// clusterOnlyPeerSelector converges ws with the default (nil) ClusterOnly
// option and returns the profile rule's namespace selector.
func clusterOnlyPeerSelector(t *testing.T, ws *cdiv1.Workspace) k8slabels.Selector {
	t.Helper()
	tpl := testTemplate(nil)
	tpl.Spec.NetworkProfile = cdiv1.NetworkProfileClusterOnly
	np, _ := netPolFor(t, Options{}, ws, tpl)
	rule := np.Spec.Egress[len(np.Spec.Egress)-1]
	if len(rule.To) != 1 || rule.To[0].NamespaceSelector == nil ||
		rule.To[0].PodSelector != nil || rule.To[0].IPBlock != nil || len(rule.Ports) != 0 {
		t.Fatalf("default ClusterOnly rule = %+v, want one namespace-selector peer on every port", rule)
	}
	sel, err := metav1.LabelSelectorAsSelector(rule.To[0].NamespaceSelector)
	if err != nil {
		t.Fatalf("namespace selector: %v", err)
	}
	return sel
}

// TestClusterOnlyEgressDefaultSameTenantOnly (R1-28, TM-34): with no
// operator narrowing, a ClusterOnly workspace reaches pods in its own
// tenant's managed namespaces (every one of them) and in no other
// tenant's. A Workspace CR without the tenant label fails closed to its
// own namespace.
func TestClusterOnlyEgressDefaultSameTenantOnly(t *testing.T) {
	ws := testWorkspace() // namespace tenant-a
	ws.Labels = map[string]string{"workspaces.cdi.tinyorbit.vn/tenant": "tenant-a"}
	sel := clusterOnlyPeerSelector(t, ws)
	for _, c := range []struct {
		name string
		ns   k8slabels.Set
		want bool
	}{
		{"own namespace", nsLabels("tenant-a", "tenant-a"), true},
		{"second namespace of the same tenant", nsLabels("tenant-a-batch", "tenant-a"), true},
		{"another tenant's namespace", nsLabels("tenant-b", "tenant-b"), false},
		{"unmanaged namespace", nsLabels("kube-system", ""), false},
	} {
		if got := sel.Matches(c.ns); got != c.want {
			t.Errorf("default ClusterOnly peer %q matches %s = %v, want %v", sel, c.name, got, c.want)
		}
	}

	unlabelled := testWorkspace()
	sel = clusterOnlyPeerSelector(t, unlabelled)
	if !sel.Matches(nsLabels("tenant-a", "tenant-a")) {
		t.Errorf("unlabelled CR: peer %q must match its own namespace", sel)
	}
	for _, other := range []k8slabels.Set{nsLabels("tenant-a-batch", "tenant-a"), nsLabels("tenant-b", "tenant-b")} {
		if sel.Matches(other) {
			t.Errorf("unlabelled CR: peer %q matches %v; it must fail closed to its own namespace", sel, other)
		}
	}
}

// TestEnsure_NetPolPrecedesPod (R1-35): the workspace's NetworkPolicy
// exists before its pod is created, so tenant code never runs
// unconfined, even while a later step (here the Service, refused by a
// quota) keeps failing and the namespace has no default-deny
// (networkPolicy.enabled=false).
func TestEnsure_NetPolPrecedesPod(t *testing.T) {
	ws := testWorkspace()
	tpl := testTemplate(nil)
	var order []string
	refused := apierrors.NewForbidden(schema.GroupResource{Resource: "services"},
		ServiceName(ws.UID), errors.New("exceeded quota: tcdi-quota, requested: services=1"))
	c := fake.NewClientBuilder().WithScheme(backendScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			switch obj.(type) {
			case *corev1.Pod:
				order = append(order, "Pod")
			case *corev1.Service:
				order = append(order, "Service")
				return refused
			case *networkingv1.NetworkPolicy:
				order = append(order, "NetworkPolicy")
			}
			return cl.Create(ctx, obj, opts...)
		},
	}).Build()
	if _, err := New(c, Options{}).Ensure(context.Background(), ws, tpl); err == nil {
		t.Fatal("Ensure succeeded although Service creation is refused")
	}
	np := &networkingv1.NetworkPolicy{}
	if err := c.Get(context.Background(),
		client.ObjectKey{Name: NetPolName(ws.UID), Namespace: ws.Namespace}, np); err != nil {
		t.Fatalf("runtime pod can run with no NetworkPolicy while the Service step fails: %v (create order %v)", err, order)
	}
	npAt, podAt := -1, -1
	for i, k := range order {
		switch k {
		case "NetworkPolicy":
			npAt = i
		case "Pod":
			podAt = i
		}
	}
	if npAt < 0 || podAt < 0 || npAt > podAt {
		t.Fatalf("create order %v, want NetworkPolicy before Pod", order)
	}
}
```

- [ ] **Step 2: Run them and see them fail**

Run: `go test ./internal/runtime/linux/ -run 'TestClusterOnlyEgressDefaultSameTenantOnly|TestEnsure_NetPolPrecedesPod' -count=1 2>&1 | grep -E '^(---|\s+backend_netpol_test)' | head`

Expected:
- `--- FAIL: TestClusterOnlyEgressDefaultSameTenantOnly` with `matches another tenant's namespace = true, want false`, and in the unlabelled block `matches …tenant-a-batch…`;
- `--- FAIL: TestEnsure_NetPolPrecedesPod` with `runtime pod can run with no NetworkPolicy while the Service step fails: … not found (create order [Pod Service])`.

- [ ] **Step 3: Scope the default ClusterOnly peer to the workspace's tenant**

In `internal/runtime/linux/backend.go`, replace the `clusterOnlyEgressRule` doc comment and the function's head and default branch:

```go
// clusterOnlyEgressRule builds the in-cluster egress rule for
// NetworkProfileClusterOnly. The zero option is RESTRICTED to the
// workspace's own tenant: egress reaches pods in the namespaces carrying
// its tenant label value (sameTenantNamespaces), never another tenant's.
// The Unrestricted opt-out admits any pod in any namespace on any port;
// confinement then rests entirely on destination-side ingress policies.
// A configured selector set replaces the default peer with matching pods
// in matching namespaces and/or the listed ports; it can never widen past
// pod peers (external IPs stay denied either way).
func clusterOnlyEgressRule(ws *cdiv1.Workspace, opts Options) networkingv1.NetworkPolicyEgressRule {
	co := opts.ClusterOnlyEgress
	if co == nil {
		return networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: sameTenantNamespaces(ws)}},
		}
	}
```

Leave the rest of the function (the `co.Unrestricted` branch and the selector branch) unchanged. Directly above `clusterOnlyEgressRule`, add:

```go
// labelTenant is the tenant label the API stamps on every Workspace CR
// (provisioning.LabelTenant) and the chart puts on every managed namespace
// (templates/namespaces.yaml), with the same tenant id as its value.
const labelTenant = "workspaces.cdi.tinyorbit.vn/tenant"

// sameTenantNamespaces selects the managed namespaces of ws's own tenant.
// A CR without the tenant label fails closed to its own namespace only:
// the default ClusterOnly peer never spans tenants.
func sameTenantNamespaces(ws *cdiv1.Workspace) *metav1.LabelSelector {
	if t := ws.Labels[labelTenant]; t != "" {
		return &metav1.LabelSelector{MatchLabels: map[string]string{labelTenant: t}}
	}
	return &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": ws.Namespace}}
}
```

In `buildNetPol`, change `profile = append(profile, clusterOnlyEgressRule(opts))` to `profile = append(profile, clusterOnlyEgressRule(ws, opts))`.

In the `Options.ClusterOnlyEgress` comment (lines 376-384), replace `Nil keeps the default posture — egress to pods in namespaces labelled workspaces.cdi.tinyorbit.vn/tenant (the managed namespaces).` with `Nil keeps the default posture: egress to pods in the namespaces of the workspace's own tenant (its workspaces.cdi.tinyorbit.vn/tenant label value), never another tenant's.`

In `cmd/operator/main.go`, in the `--runtime-clusteronly-egress` help, replace `"Empty keeps the restricted default: egress to pods in namespaces "+ "labelled workspaces.cdi.tinyorbit.vn/tenant only. "+` with:

```go
			"Empty keeps the restricted default: egress to pods in the namespaces "+
			"of the workspace's own tenant (its workspaces.cdi.tinyorbit.vn/tenant label value) only. "+
```

- [ ] **Step 4: Create the NetworkPolicy before the pod**

In `Ensure`, move the `ensureNetPol` call so the block reads:

```go
	if ws.Spec.DataPolicy == cdiv1.DataPolicyRetain {
		if _, err := b.ensurePVC(ctx, ws, tpl); err != nil {
			return runtime.Observation{}, err
		}
	}
	// The NetworkPolicy goes first: it selects by the workspace-UID label,
	// so it can exist before the pod, and tenant code never runs while a
	// later step (the Service) fails, even in a namespace without a
	// default-deny (networkPolicy.enabled=false).
	if err := b.ensureNetPol(ctx, ws, tpl); err != nil {
		return runtime.Observation{}, err
	}
	if err := b.ensurePod(ctx, ws, tpl, appArmor, seccomp); err != nil {
		return runtime.Observation{}, err
	}
	if err := b.ensureService(ctx, ws); err != nil {
		return runtime.Observation{}, err
	}
	return b.Observe(ctx, ws)
```

- [ ] **Step 5: Update the tests that pin the old default**

`internal/runtime/linux/backend_test.go`: replace `TestClusterOnlyEgress_DefaultRestricted` and fix the call in `TestClusterOnlyEgress_UnrestrictedOptIn`:

```go
// ClusterOnly egress defaults to RESTRICTED to the workspace's own tenant:
// with no operator narrowing configured the rule confines egress to pods
// in namespaces carrying the workspace's tenant label value, never the
// Unrestricted any-pod-anywhere opt-out. PodSelector stays unset and no
// ports are pinned; DNS is unaffected (it leads the egress list).
func TestClusterOnlyEgress_DefaultRestricted(t *testing.T) {
	ws := testWorkspace()
	ws.Labels = map[string]string{"workspaces.cdi.tinyorbit.vn/tenant": "tenant-a"}
	rule := clusterOnlyEgressRule(ws, Options{})
	if len(rule.To) != 1 || rule.To[0].NamespaceSelector == nil {
		t.Fatalf("%+v", rule)
	}
	sel := rule.To[0].NamespaceSelector
	if len(sel.MatchExpressions) != 0 || len(sel.MatchLabels) != 1 ||
		sel.MatchLabels["workspaces.cdi.tinyorbit.vn/tenant"] != "tenant-a" {
		t.Fatalf("default namespaceSelector = %+v, want matchLabels {tenant: tenant-a}", sel)
	}
	if rule.To[0].PodSelector != nil || len(rule.Ports) != 0 {
		t.Fatalf("%+v", rule)
	}
}
```

and in `TestClusterOnlyEgress_UnrestrictedOptIn`: `rule := clusterOnlyEgressRule(testWorkspace(), Options{ClusterOnlyEgress: &ClusterOnlyEgress{Unrestricted: true}})`.

`internal/runtime/linux/backend_netpol_test.go`, `TestClusterOnlyEgressDefaultRestricted`: after `ws := testWorkspace()` add `ws.Labels = map[string]string{"workspaces.cdi.tinyorbit.vn/tenant": "tenant-a"}`. Replace the final block, from `peer := rule.To[0]` to the end of the function, with:

```go
	peer := rule.To[0]
	if peer.PodSelector != nil || peer.NamespaceSelector == nil {
		t.Fatalf("default ClusterOnly peer = %+v, want {namespaceSelector: own tenant}", peer)
	}
	if ml := peer.NamespaceSelector.MatchLabels; len(peer.NamespaceSelector.MatchExpressions) != 0 ||
		len(ml) != 1 || ml["workspaces.cdi.tinyorbit.vn/tenant"] != "tenant-a" {
		t.Fatalf("default ClusterOnly namespaceSelector = %+v, want matchLabels {tenant: tenant-a}", peer.NamespaceSelector)
	}
}
```

Also update its comment's "want the single managed-namespaces peer" message to "want the single own-tenant peer".

`internal/runtime/linux/backend_netpol_test.go`, `TestClusterOnlyEgressRestrictionOnPolicy`: the drift half re-ensures `testWorkspace()`, which has no tenant label, with `Options{}`. The restored default is now the fail-closed own-namespace peer. Replace the final check:

```go
	if len(back.Spec.Egress[1].To) != 1 ||
		back.Spec.Egress[1].To[0].NamespaceSelector == nil ||
		len(back.Spec.Egress[1].To[0].NamespaceSelector.MatchExpressions) != 1 {
		t.Fatalf("stored policy was not reconciled back to the restricted default peer: %+v",
			back.Spec.Egress[1].To)
	}
```

with:

```go
	got := back.Spec.Egress[1].To
	if len(got) != 1 || got[0].PodSelector != nil || got[0].NamespaceSelector == nil ||
		len(got[0].NamespaceSelector.MatchExpressions) != 0 ||
		got[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != ws.Namespace {
		t.Fatalf("stored policy was not reconciled back to the restricted default peer (an unlabelled CR's own namespace): %+v", got)
	}
```

Run:

```bash
go build ./... && go vet ./internal/runtime/linux/ ./cmd/operator/
go test ./internal/runtime/linux/ ./cmd/operator/ -count=1 2>&1 | tail -3
```

Expected: `ok` for both packages; this includes the two new tests, `TestClusterOnlyEgressRestrictionOnPolicy` and `TestParseClusterOnlyEgress`. Then run the operator package, whose reconcile tests drive `Ensure` through envtest: `KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path --bin-dir "$PWD/bin")" go test ./internal/operator/ -count=1 -timeout 40m 2>&1 | tail -1` (expected: `ok`; the package takes about 10 minutes, the default `go test` timeout, so pass `-timeout`).

- [ ] **Step 6: Docs**

`SECURITY.md` item 23, replace the `ClusterOnly` sub-bullet's first two lines (`- `ClusterOnly` is restricted by default: egress reaches pods in the managed namespaces (labelled `workspaces.cdi.tinyorbit.vn/tenant`) and no pod outside them.`) with:

```markdown
    - `ClusterOnly` is restricted by default to the workspace's own
      tenant: egress reaches pods in the namespaces labelled with its
      tenant (`workspaces.cdi.tinyorbit.vn/tenant: <tenant>`) and no pod
      in another tenant's namespace or outside the managed namespaces.
      The opt-in
```

Keep the rest of the sub-bullet (from "`runtime.networkProfiles.clusterOnly.*` values" on) unchanged.

`docs/security/threat-model.md:381-384` (B6 NetworkPolicy bullet): replace `` `ClusterOnly` egress is restricted by default to pods in the managed namespaces; `` with `` `ClusterOnly` egress is restricted by default to pods in the workspace's own tenant's namespaces (its `workspaces.cdi.tinyorbit.vn/tenant` label value), and the policy is created before the pod so a runtime never runs without it (`Ensure`); ``.

`deploy/helm/tinycdi/README.md`:
- In the row `runtime.networkProfiles.clusterOnly.egressNamespaceSelector` (line 281), replace `Empty keeps the restricted default: egress to pods in namespaces labelled `workspaces.cdi.tinyorbit.vn/tenant` (the managed namespaces).` with `Empty keeps the restricted default: egress to pods in the namespaces of the workspace's own tenant.`
- In the "ClusterOnly egress" section (lines 288-291), replace `egress reaches pods in namespaces labelled `workspaces.cdi.tinyorbit.vn/tenant` — the managed namespaces — and no pod outside them.` with `egress reaches pods in the namespaces of the workspace's own tenant (labelled `workspaces.cdi.tinyorbit.vn/tenant: <tenant>`) and no pod in another tenant's namespace or outside the managed namespaces. Cross-tenant reach needs an explicit `egressNamespaceSelector` or `unrestricted: true`.`

`docs/releases/v1.0.0.md:50-51`: replace `pods in the managed namespaces only.` with `pods in the workspace's own tenant's namespaces only.`

Run: `bash .github/tests/docs-gates.test.sh > /tmp/r1-28-docs.log 2>&1; echo "rc=$?"; grep -E '^FAIL' /tmp/r1-28-docs.log | head` (expected: `rc=0`).

The chart and its default render do not change, because the default is in the operator binary.

- [ ] **Step 7: Gates and commit**

```bash
go test -count=1 ./deploy/helm/ > /tmp/r1-28-helm.log 2>&1; echo "helm rc=$?"
for t in .github/tests/*.test.sh; do bash "$t" > /tmp/r1-28-gate.log 2>&1 || { echo "FAIL $t"; tail -20 /tmp/r1-28-gate.log; exit 1; }; done; echo gates-ok
git add internal/runtime/linux/backend.go internal/runtime/linux/backend_netpol_test.go internal/runtime/linux/backend_test.go \
  cmd/operator/main.go SECURITY.md docs/security/threat-model.md deploy/helm/tinycdi/README.md docs/releases/v1.0.0.md
git commit -m "fix(runtime): ClusterOnly egress stays in-tenant and the NetworkPolicy precedes the pod" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

Expected: `helm rc=0`, `gates-ok`.

### Task 20: Userns browser profile pair requires hostUsers false (R1-29) — tier: integrate

**Finding R1-29 (P3, TM-47).**
- **Attack.** The shipped browser profile pair is the seccomp profile `localhost/profiles/chromium-userns.json` and the AppArmor profile `tinycdi-browser`. The seccomp profile allows `clone` and `unshare` with `CLONE_NEW*` flags, plus `setns` and `chroot`, for any process without `CAP_SYS_ADMIN` (`deploy/node-profiles/seccomp/chromium-userns.src.json`, last group). The AppArmor profile adds `userns,`. So every process in the pod can create user, PID and network namespaces, not only Chromium's zygote, and reach the namespace-gated kernel code behind them. Nothing ties the pair to `hostUsers: false`. The operator flag `--runtime-host-users` defaults to unset, and a template's `spec.linux.hostUsers: true` overrides the chart's `runtime.hostUsers: false`. With host users, the pod's UIDs are the node's UIDs, so a kernel bug reached this way lands as a node UID. The docs do not state the trade-off.
- **Required fix (Decision 1).** The operator refuses the userns profile unless the pod runs with an effective `hostUsers: false`, and the docs state the trade-off. The operator cannot read a node's profile file, so the gate matches the shipped profile by name; the docs and the TM-47 residual state that a custom `Localhost` profile allowing namespace creation is not detected (Step 6, pinned by two doc claims). Effective `hostUsers` is the template's `spec.linux.hostUsers`, else the operator default. The gate keys on the seccomp profile alone. Under `RuntimeDefault` seccomp, `clone` and `unshare` with namespace flags need `CAP_SYS_ADMIN`, which the pod drops, so the AppArmor `userns,` rule grants nothing without the seccomp profile. A refused template surfaces like every other `ErrTemplateRejected`: Degraded, reason `TemplateRejected` (`internal/operator/workspace_controller.go:541-544`), and no child object. The chart fails the render of a seeded template the operator would refuse.
- **Security tests.**
  - `TestEnsureUsernsProfileRequiresHostUsersFalse` in `internal/runtime/linux/userns_profile_test.go` (new). Matrix: no operator default and template unset → rejected; operator default `true` → rejected; template `true` over operator default `false` → rejected; operator default `false` → accepted; template `false` with no default → accepted; another localhost profile, or `RuntimeDefault`, with host users → accepted. A rejected Ensure creates no pod.
  - `TestSeededUsernsProfileRequiresHostUsersFalse` in `deploy/helm/chart_userns_profile_test.go` (new). `ci/example-values.yaml` (two userns seeds on the chart default `runtime.hostUsers: false`) renders. It fails with `requires hostUsers: false` for `runtime.hostUsers=null`, `runtime.hostUsers=true`, a seed with `spec.linux.hostUsers: true`, and a seed with `spec.linux.hostUsers: null` while `runtime.hostUsers=null`. A seed with `spec.linux.hostUsers: false` renders while `runtime.hostUsers=null`.
  - Docs assertions appended to `.github/tests/security-doc-claims.test.sh`.

**Checked at HEAD 98c7f54.**
- Only `internal/runtime/linux/seccomp_profile_test.go:124` (`TestBuildPodSeccompLocalhostHonored`, through `ensurePodSpec` with `Options{}`) and `internal/operator/snapshot_seccomp_test.go:128` (`TestTemplateValidLocalhostSeccompAnnotation`, `linux.Options{}`) run `Ensure` on the userns profile with host users. Both are updated in Step 5. `aaBuild` (`apparmor_setting_test.go:21`) calls `buildPod` directly and does not pass the gate. `internal/provisioning/template_family_test.go` never calls `Ensure`.
- The chart's default render seeds no templates (`values.yaml:847`), so the golden does not change. The quickstart values (`hack/quickstart/values*.yaml`, `runtime.hostUsers: null`) seed no localhost seccomp profile.
- The admission re-check `ValidateSeccompAnnotation` (`workspace_controller.go:501,1138,1318`) takes annotations only and has no operator options. It stays unchanged. The new gate runs in `Ensure`, the same place as the `adapter=kasm` image gate.
- In the chart, sprig `get` returns `""` for a missing key, so the template reads `hostUsers` with `hasKey` and `index` (a first draft that used `get` failed every seed with `linux: {}`).

**Files:**
- Modify: `internal/runtime/linux/backend.go`:
  - `Ensure`, directly after the `resolveSeccompProfile` call (`:494-497`): the new gate;
  - before the `// validSeccompProfileName:` comment (`:1037`): new const `usernsSeccompProfile` and funcs `requireOwnUserNamespace` and `effectiveHostUsers`;
  - `buildPod`, the `hostUsers` block (`:1355-1361`): use `effectiveHostUsers`.
- Modify: `deploy/helm/tinycdi/templates/workspacetemplates.yaml:77-79` (after the seccomp annotation is set)
- Create: `internal/runtime/linux/userns_profile_test.go`
- Create: `deploy/helm/chart_userns_profile_test.go`
- Modify: `internal/runtime/linux/seccomp_profile_test.go:122-127`; `internal/operator/snapshot_seccomp_test.go:128-131`
- Docs: `docs/security/threat-model.md:404-409` (B6 "Pod hardening" bullet) and the TM-47 Residual cell (`:726`; Task 30 owns its Control and Pinned-by cells); `SECURITY.md:360-363,379-380,398` (item 22); `deploy/node-profiles/README.md:3-8`; `deploy/helm/tinycdi/README.md:277`; `docs/releases/v1.0.0.md:149-154` (Known limitations)
- Modify: `.github/tests/security-doc-claims.test.sh` (created by Task 10; append above the final `exit "$fails"`)

Tasks 19 and 21 also edit `backend.go`, and Task 18 edits the same "Pod hardening" bullet and the same Known limitations list. Find the anchors below by text, not by line.

**Interfaces:**
- Consumes: `testWorkspace()`, `testTemplate()`, `backendScheme()`, `ensurePodSpecFor()` (`internal/runtime/linux/backend_test.go:36-97`); `ptr[T]` (`backend.go:1753`); `templateRejectedError`/`ErrTemplateRejected` (`backend.go:271-280`); chart test helpers `renderArgs`, `renderErrArgs`, `selectDocs` (`deploy/helm/chart_test.go:108-155`); `adoptTemplate`, `seccompReconcile` (`internal/operator`).
- Produces:
  - `const usernsSeccompProfile = "profiles/chromium-userns.json"` (package `linux`, unexported).
  - `func requireOwnUserNamespace(seccomp *corev1.SeccompProfile, tpl *cdiv1.WorkspaceTemplate, opts Options) error`, which returns a `*templateRejectedError` whose message contains `requires hostUsers: false`.
  - `func effectiveHostUsers(tpl *cdiv1.WorkspaceTemplate, opts Options) *bool`, which is now the single source of `pod.Spec.HostUsers`.
  - Chart render failure text `templates[<name>]: seccompProfile localhost/profiles/chromium-userns.json allows unprivileged user namespaces and requires hostUsers: false (...)`.
  - Release-notes bullet title `**The browser sandbox profile allows namespaces.**`.

- [ ] **Step 1: Write the failing tests**

Create `internal/runtime/linux/userns_profile_test.go`:

```go
// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// The browser seccomp profile (localhost/profiles/chromium-userns.json)
// lets the workspace user create user and network namespaces — the
// Chromium sandbox needs them. The profile is admitted only when the pod
// runs in its own user namespace (effective hostUsers: false), so what
// the user reaches through those namespaces never runs as a node UID.
// Any other profile is unaffected.

package linux

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEnsureUsernsProfileRequiresHostUsersFalse(t *testing.T) {
	userns := map[string]string{
		AnnotationSeccompProfile: "localhost/profiles/chromium-userns.json",
	}
	for _, tc := range []struct {
		name     string
		ann      map[string]string
		tplHU    *bool
		opts     Options
		rejected bool
	}{
		{"no operator default, template unset", userns, nil, Options{}, true},
		{"operator default true", userns, nil, Options{DefaultHostUsers: ptr(true)}, true},
		{"template true over default false", userns, ptr(true), Options{DefaultHostUsers: ptr(false)}, true},
		{"operator default false", userns, nil, Options{DefaultHostUsers: ptr(false)}, false},
		{"template false, no default", userns, ptr(false), Options{}, false},
		{"other localhost profile, host users", map[string]string{
			AnnotationSeccompProfile: "localhost/browser-sandbox"}, nil, Options{}, false},
		{"runtime default, host users", nil, nil, Options{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(backendScheme(t)).Build()
			ws := testWorkspace()
			tpl := testTemplate(tc.ann)
			tpl.Spec.Linux.HostUsers = tc.tplHU
			_, err := New(c, tc.opts).Ensure(context.Background(), ws, tpl)
			pod := &corev1.Pod{}
			getErr := c.Get(context.Background(), client.ObjectKey{
				Name: PodName(ws.UID), Namespace: ws.Namespace}, pod)
			if !tc.rejected {
				if err != nil {
					t.Fatalf("Ensure: %v", err)
				}
				if getErr != nil {
					t.Fatalf("pod not created: %v", getErr)
				}
				return
			}
			if !errors.Is(err, ErrTemplateRejected) {
				t.Fatalf("Ensure err = %v, want ErrTemplateRejected", err)
			}
			if !strings.Contains(err.Error(), "hostUsers: false") {
				t.Errorf("rejection %q does not name the hostUsers: false requirement", err)
			}
			if getErr == nil {
				t.Fatalf("rejected template still produced pod %s", pod.Name)
			}
		})
	}
}
```

Create `deploy/helm/chart_userns_profile_test.go`:

```go
// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package chart_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const usernsSeccompAnnotation = "localhost/profiles/chromium-userns.json"

// usernsTemplateValues writes a values overlay whose only seeded template
// uses the userns browser seccomp profile with the given spec.linux block.
func usernsTemplateValues(t *testing.T, linux string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "userns-values.yaml")
	body := fmt.Sprintf(`templates:
  - name: userns
    namespace: tinycdi-tenant-a
    image: browser
    seccompProfile: %s
    spec:
      revision: "2026-10-a"
      runtime: LinuxContainer
      experience: Browser
      linux: %s
      resources: {cpu: "1", memory: 2Gi, storage: 5Gi}
      bootDeadline: 5m
      networkProfile: Isolated
`, usernsSeccompAnnotation, linux)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The userns browser seccomp profile lets the workspace user create user
// and network namespaces, so a seeded template that selects it renders
// only when its pods run with hostUsers: false — from spec.linux.hostUsers
// or, when the template leaves it unset, from runtime.hostUsers.
func TestSeededUsernsProfileRequiresHostUsersFalse(t *testing.T) {
	example := filepath.Join("tinycdi", "ci", "example-values.yaml")

	// The shipped example seeds two userns templates on the chart default
	// runtime.hostUsers=false: it renders.
	n := 0
	for _, d := range selectDocs(renderArgs(t, "-f", example), "WorkspaceTemplate") {
		ann, _ := d["metadata"].(map[string]any)["annotations"].(map[string]any)
		if ann["workspaces.cdi.tinyorbit.vn/seccomp-profile"] == usernsSeccompAnnotation {
			n++
		}
	}
	if n < 2 {
		t.Fatalf("example-values renders %d userns templates, want the browser and kasm seeds", n)
	}

	for name, extra := range map[string][]string{
		"operator default unset": {"-f", example, "--set-json", "runtime.hostUsers=null"},
		"operator default true":  {"-f", example, "--set", "runtime.hostUsers=true"},
		"template true":          {"-f", example, "-f", usernsTemplateValues(t, "{hostUsers: true}")},
		"template null, operator default unset": {"-f", example,
			"-f", usernsTemplateValues(t, "{hostUsers: null}"), "--set-json", "runtime.hostUsers=null"},
	} {
		t.Run(name, func(t *testing.T) {
			out := renderErrArgs(t, extra...)
			if !strings.Contains(out, "requires hostUsers: false") {
				t.Errorf("render failed without naming the hostUsers requirement:\n%s", out)
			}
		})
	}

	// A template's own hostUsers: false wins over an unset operator default.
	renderArgs(t, "-f", example, "-f", usernsTemplateValues(t, "{hostUsers: false}"),
		"--set-json", "runtime.hostUsers=null")
}
```

The overlay replaces the `templates` list (Helm replaces lists, it does not merge them), which is why it is a file and not `--set templates[1]…`.

Append to `.github/tests/security-doc-claims.test.sh`, directly above the final `exit "$fails"`:

```bash
need R1-29 docs/security/threat-model.md '\*\*Pod hardening\*\*:[^*]*create user, PID and network namespaces[^*]*hostUsers: false'
need_row R1-29 TM-47 'chromium-userns\.json` the workspace user can create user, PID and network namespaces'
need R1-29 SECURITY.md 'chromium-userns\.json` lets the workspace user create user, PID and network namespaces'
need R1-29 deploy/node-profiles/README.md 'must run its pods with `hostUsers: false`'
need R1-29 deploy/helm/tinycdi/README.md 'chromium-userns\.json` needs an effective `false`'
need R1-29 docs/releases/v1.0.0.md 'The browser sandbox profile allows namespaces\.'
need R1-29 deploy/node-profiles/README.md 'a custom `Localhost` profile that also allows namespace creation is not detected'
need_row R1-29 TM-47 'a custom `Localhost` profile that allows namespace creation is not detected'
```

Use `[^*]*`, not `.{0,N}`, to stay inside one bullet. On the flattened threat model (one 80 KB line), GNU `grep -E 'Pod hardening.{0,900}…'` ran for more than 100 s without finishing; the `[^*]*` form takes 0.1 s.

- [ ] **Step 2: Run them and see them fail**

```bash
go test ./internal/runtime/linux/ -run TestEnsureUsernsProfileRequiresHostUsersFalse -count=1 > /tmp/r1-29-linux.log 2>&1; echo "rc=$?"; grep -E -- '--- FAIL|_test.go:[0-9]+' /tmp/r1-29-linux.log | head
(cd deploy/helm && go test . -run TestSeededUsernsProfileRequiresHostUsersFalse -count=1 > /tmp/r1-29-helm.log 2>&1; echo "rc=$?"); grep -E -- '--- FAIL|_test.go:[0-9]+' /tmp/r1-29-helm.log | head
bash .github/tests/security-doc-claims.test.sh 2>&1 | grep -E '^FAIL: R1-29' | wc -l
```

Expected (seen on a scratch copy of HEAD):
- linux `rc=1`, with `Ensure err = <nil>, want ErrTemplateRejected` for the three subtests `no_operator_default,_template_unset`, `operator_default_true` and `template_true_over_default_false`;
- helm `rc=1`, with `expected failure, render succeeded` for all four subtests;
- `8` doc-claim failures.

- [ ] **Step 3: Gate the userns profile in the backend**

In `internal/runtime/linux/backend.go`, `Ensure`: directly after

```go
	seccomp, err := resolveSeccompProfile(tpl.Annotations)
	if err != nil {
		return runtime.Observation{}, err
	}
```

insert

```go
	if err := requireOwnUserNamespace(seccomp, tpl, b.opts); err != nil {
		return runtime.Observation{}, err
	}
```

Directly above the comment line `// validSeccompProfileName: the node-loaded profile reference is a RELATIVE`, insert:

```go
// usernsSeccompProfile is the shipped browser seccomp profile
// (deploy/node-profiles/seccomp/chromium-userns.json). It allows clone
// and unshare with CLONE_NEW* flags (plus setns and chroot) for the
// Chromium sandbox, so the workspace user can create user and network
// namespaces.
const usernsSeccompProfile = "profiles/chromium-userns.json"

// requireOwnUserNamespace admits the userns browser profile only when the
// pod runs in its own user namespace (effective hostUsers false). The
// profile opens the namespace-gated kernel code to the workspace user;
// with host users the pod's UIDs are the node's UIDs, so a kernel bug
// reached that way lands as a node UID rather than an unmapped one.
// Other profiles pass untouched.
func requireOwnUserNamespace(seccomp *corev1.SeccompProfile, tpl *cdiv1.WorkspaceTemplate, opts Options) error {
	if seccomp == nil || seccomp.Type != corev1.SeccompProfileTypeLocalhost ||
		seccomp.LocalhostProfile == nil || *seccomp.LocalhostProfile != usernsSeccompProfile {
		return nil
	}
	if hu := effectiveHostUsers(tpl, opts); hu != nil && !*hu {
		return nil
	}
	return &templateRejectedError{reason: fmt.Sprintf(
		"annotation %s=localhost/%s allows unprivileged user namespaces and requires hostUsers: false "+
			"(spec.linux.hostUsers, or the operator's --runtime-host-users=false)",
		AnnotationSeccompProfile, usernsSeccompProfile)}
}

// effectiveHostUsers is the pod hostUsers value: the template field, else
// the operator default; nil leaves the pod field unset.
func effectiveHostUsers(tpl *cdiv1.WorkspaceTemplate, opts Options) *bool {
	if tpl.Spec.Linux != nil && tpl.Spec.Linux.HostUsers != nil {
		return tpl.Spec.Linux.HostUsers
	}
	return opts.DefaultHostUsers
}
```

In `buildPod`, replace

```go
	// hostUsers: template field → operator default; nil leaves the pod
	// field unset (D26 — opt-in until the R1 spike lands).
	hostUsers := opts.DefaultHostUsers
	if h := tpl.Spec.Linux.HostUsers; h != nil {
		hostUsers = h
	}
	pod.Spec.HostUsers = hostUsers
```

with

```go
	pod.Spec.HostUsers = effectiveHostUsers(tpl, opts)
```

The gate and the pod field now read one function, so they cannot disagree.

- [ ] **Step 4: Fail the chart render the same way**

In `deploy/helm/tinycdi/templates/workspacetemplates.yaml`, directly after

```
{{- if .seccompProfile }}
{{- $_ := set $ann "workspaces.cdi.tinyorbit.vn/seccomp-profile" (printf "localhost/%s" (.seccompProfile | trimPrefix "localhost/")) }}
{{- end }}
```

insert

```
{{- /* The userns browser seccomp profile lets the workspace user create
     user and network namespaces; the operator admits it only when the
     pod runs in its own user namespace. Fail the render the same way:
     effective hostUsers is spec.linux.hostUsers, else runtime.hostUsers. */ -}}
{{- if eq (index $ann "workspaces.cdi.tinyorbit.vn/seccomp-profile" | default "") "localhost/profiles/chromium-userns.json" }}
{{- $hostUsers := $.Values.runtime.hostUsers }}
{{- if and (hasKey $linux "hostUsers") (not (kindIs "invalid" (index $linux "hostUsers"))) }}
{{- $hostUsers = index $linux "hostUsers" }}
{{- end }}
{{- if not (and (kindIs "bool" $hostUsers) (not $hostUsers)) }}
{{- fail (printf "templates[%s]: seccompProfile localhost/profiles/chromium-userns.json allows unprivileged user namespaces and requires hostUsers: false (set spec.linux.hostUsers: false or runtime.hostUsers: false)" $tplName) }}
{{- end }}
{{- end }}
```

The check reads `$ann`, so it also covers the profile set verbatim through `annotations:`.

- [ ] **Step 5: Update the tests that ran the profile with host users, and rerun**

`internal/runtime/linux/seccomp_profile_test.go`: replace

```go
// A valid localhost profile still reaches the pod — the gate widens
// nothing, it only refuses what was never validated.
func TestBuildPodSeccompLocalhostHonored(t *testing.T) {
	pod := ensurePodSpec(t, map[string]string{
		AnnotationSeccompProfile: "localhost/profiles/chromium-userns.json",
	})
```

with

```go
// A valid localhost profile still reaches the pod — the gate widens
// nothing, it only refuses what was never validated. The userns browser
// profile needs hostUsers: false (TestEnsureUsernsProfileRequiresHostUsersFalse).
func TestBuildPodSeccompLocalhostHonored(t *testing.T) {
	pod := ensurePodSpecFor(t, testTemplate(map[string]string{
		AnnotationSeccompProfile: "localhost/profiles/chromium-userns.json",
	}), Options{DefaultHostUsers: ptr(false)})
```

`internal/operator/snapshot_seccomp_test.go`, `TestTemplateValidLocalhostSeccompAnnotation`: replace

```go
	tpl.Annotations[linux.AnnotationSeccompProfile] = "localhost/profiles/chromium-userns.json"
	ws := adoptWorkspace("fam32-aaaa1111", "", types.UID("ws-good-sec"))
```

with

```go
	tpl.Annotations[linux.AnnotationSeccompProfile] = "localhost/profiles/chromium-userns.json"
	hostUsers := false // the userns profile requires its own user namespace
	tpl.Spec.Linux.HostUsers = &hostUsers
	ws := adoptWorkspace("fam32-aaaa1111", "", types.UID("ws-good-sec"))
```

Run:

```bash
go test ./internal/runtime/linux/ -count=1 > /tmp/r1-29-linux.log 2>&1; echo "linux rc=$?"; grep -E -- '--- FAIL|_test.go:[0-9]+' /tmp/r1-29-linux.log | head
export KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path --bin-dir "$PWD/bin")"
go test ./internal/operator/ ./internal/provisioning/ ./cmd/operator/ -count=1 -timeout 40m > /tmp/r1-29-op.log 2>&1; echo "op rc=$?"; grep -E -- '^(ok|FAIL)|--- FAIL' /tmp/r1-29-op.log | head
(cd deploy/helm && go test . -count=1 -timeout 30m > /tmp/r1-29-helm.log 2>&1; echo "helm rc=$?"); grep -E -- '--- FAIL|_test.go:[0-9]+' /tmp/r1-29-helm.log | head
```

Expected: `linux rc=0`, `op rc=0` (three `ok` lines), `helm rc=0`. Without Step 5's two edits, `TestBuildPodSeccompLocalhostHonored` fails with `Ensure: linux backend: template rejected: annotation workspaces.cdi.tinyorbit.vn/seccomp-profile=localhost/profiles/chromium-userns.json allows unprivileged user namespaces and requires hostUsers: false …`. The `internal/operator` package needs about 10 minutes on a loaded host, which is the default `go test` timeout, so pass `-timeout`. The default chart render (golden) does not change.

- [ ] **Step 6: Docs**

`docs/security/threat-model.md`, B6 "Pod hardening" bullet: directly after the line ending `` `localhost/` (`buildPod`, `internal/runtime/linux/backend.go`). `` insert (before `` `runtime.appArmor.requireRuntimeDefault=false` drops… ``):

```markdown
  The browser seccomp profile `localhost/profiles/chromium-userns.json`
  lets the workspace user create user, PID and network namespaces, which
  the Chromium sandbox needs; it widens the kernel surface reachable from
  the pod, so the operator admits it only when the pod runs with
  `hostUsers: false` (`requireOwnUserNamespace`, TM-47), and the chart
  fails the render otherwise.
```

`docs/security/threat-model.md`, register row `| TM-47 |`: replace its last cell `| — |` (Residual) with

```
| With the userns browser profile `localhost/profiles/chromium-userns.json` the workspace user can create user, PID and network namespaces and reach the kernel code behind them; the profile is admitted only with an effective `hostUsers: false`, so they sit inside the pod's own user namespace. Accepted: the Chromium sandbox needs them. The check matches the shipped profile by name; a custom `Localhost` profile that allows namespace creation is not detected and needs `hostUsers: false` from the operator. |
```

Task 30 adds `requireOwnUserNamespace` and the two new tests to the row's Control and Pinned-by cells.

`SECURITY.md`, item 22 ("Pod security"):
- After the sub-bullet `` - seccomp `RuntimeDefault` or a validated `Localhost` profile, never `` / `` `Unconfined`; `` insert:

  ```markdown
      - the browser seccomp profile `localhost/profiles/chromium-userns.json`
        lets the workspace user create user, PID and network namespaces
        (the Chromium sandbox needs them), so it is admitted only on a pod
        with `hostUsers: false`: the operator rejects the template
        (`TemplateRejected`) and the chart fails the render otherwise;
  ```
- In the *Enforced:* list, replace `` `TestKasmAdapterInitAdmittedUnderQuota` (envtest: the kasm pod is `` / `admitted in a ResourceQuota-governed namespace).` with:

  ```markdown
      `TestKasmAdapterInitAdmittedUnderQuota` (envtest: the kasm pod is
      admitted in a ResourceQuota-governed namespace),
      `TestEnsureUsernsProfileRequiresHostUsersFalse`,
      `TestSeededUsernsProfileRequiresHostUsersFalse` (the userns browser
      profile needs `hostUsers: false`).
  ```
- In the *Not yet enforced:* list, replace `` - `readOnlyRootFilesystem` and `hostUsers: false` on runtime pods; `` with:

  ```markdown
      - `readOnlyRootFilesystem` on runtime pods, and `hostUsers: false`
        on runtime pods without the userns browser profile;
  ```

`deploy/node-profiles/README.md`: after the intro paragraph (ending `installation is GitOps/node-provisioning owned.`), add:

```markdown

The seccomp profile lets any process in the pod, not only Chromium,
create user, PID and network namespaces (`clone` and `unshare` with
`CLONE_NEW*` flags, plus `setns` and `chroot`). The workspace user can
therefore reach the kernel code behind those namespaces. A template that
selects the profile must run its pods with `hostUsers: false`, so every
namespace the user creates sits inside the pod's own user namespace: the
operator rejects the template otherwise (`TemplateRejected`), and the chart
fails the render. The pod's effective `hostUsers` is the template's
`spec.linux.hostUsers`, else the chart's `runtime.hostUsers` (default
`false`). The check matches this profile by name: a custom `Localhost`
profile that also allows namespace creation is not detected, so a
template that selects one must set `hostUsers: false` itself.
```

`deploy/helm/tinycdi/README.md:277` (`runtime.hostUsers` row): replace the cell ending `` `null` leaves the field unset (apiserver default — host user namespace) | `` with `` `null` leaves the field unset (apiserver default — host user namespace). A seeded template with `seccompProfile: localhost/profiles/chromium-userns.json` needs an effective `false` (its own `spec.linux.hostUsers`, else this value), or the render fails | ``.

`docs/releases/v1.0.0.md`, Known limitations: directly after the "DNS egress is open on every network profile." bullet (it ends with `` [the chart README](../../deploy/helm/tinycdi/README.md#clusteronly-egress). ``), insert:

```markdown
- **The browser sandbox profile allows namespaces.** The browser seccomp
  profile `localhost/profiles/chromium-userns.json` lets the workspace user
  create user, PID and network namespaces, which the Chromium sandbox
  needs. A template that selects it runs only with `hostUsers: false`; the
  operator and the chart refuse it otherwise. See
  [node profiles](../../deploy/node-profiles/README.md).
```

Run: `bash .github/tests/security-doc-claims.test.sh 2>&1 | grep -E '^FAIL: R1-29'; echo "claims rc=${PIPESTATUS[0]}"` (expected: no `FAIL: R1-29` line, `claims rc=0` once earlier tasks are green).

- [ ] **Step 7: Gates and commit**

```bash
gofmt -l internal/ deploy/ ; go vet ./internal/runtime/linux/ ./internal/operator/
for t in .github/tests/*.test.sh; do bash "$t" > /tmp/r1-29-gate.log 2>&1 || { echo "FAIL $t"; tail -20 /tmp/r1-29-gate.log; exit 1; }; done; echo gates-ok
git add internal/runtime/linux/backend.go internal/runtime/linux/userns_profile_test.go internal/runtime/linux/seccomp_profile_test.go \
  internal/operator/snapshot_seccomp_test.go deploy/helm/tinycdi/templates/workspacetemplates.yaml deploy/helm/chart_userns_profile_test.go \
  docs/security/threat-model.md SECURITY.md deploy/node-profiles/README.md deploy/helm/tinycdi/README.md docs/releases/v1.0.0.md \
  .github/tests/security-doc-claims.test.sh
git commit -m "fix(runtime): the userns browser profile pair requires hostUsers false" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

Expected: no `gofmt` output, `go vet` clean, `gates-ok`.

### Task 21: Remove WorkspaceTemplate browserPolicy (R1-08) — tier: integrate

**Finding R1-08 (P2, boundary 8).**
- **Attack.** `WorkspaceTemplate.spec.linux.browserPolicy` (`allowedDomains`, `deniedDomains`) is in the public CRD. Its schema description says it "restricts the in-session browser", and its Go doc says "the end user cannot widen this set". Nothing reads it: no code under `internal/`, `cmd/`, `build/` or `web/` refers to `BrowserPolicy`, and the pod spec, NetworkPolicy and browser managed policy are built without it. An admin who sets `allowedDomains: ["*.corp.example"]` believes the session browser is confined to that domain. In fact the network profile alone decides, so `InternetOnly` reaches the whole internet. The shipped sample (`config/samples/workspaces_v1_workspacetemplate.yaml:14-15`) models exactly that belief.
- **Required fix (Decision 5).** Remove the field. That covers the CRD field, the Go types, deepcopy, samples, docs, and any portal or OpenAPI mention. The release notes say per-template URL filtering is not offered in 1.0. No compatibility alias (Global Constraints: no upgrade path).
- **Security test.** `TestWorkspaceTemplate_BrowserPolicyFieldRemoved` in `api/v1/validation_test.go` (envtest). A `Strict` field-validation create of a template carrying `spec.linux.browserPolicy` is refused with `unknown field "spec.linux.browserPolicy"`. Before the fix the create succeeds.

**Checked at HEAD 98c7f54.** `rg -i 'browserPolicy|allowedDomains|deniedDomains'` outside generated files hits only `api/v1/workspacetemplate_types.go`, `api/v1/validation_test.go:89-91`, `config/samples/workspaces_v1_workspacetemplate.yaml:14-15` and `docs/security/review-v1.0.md` (finding text, which stays). There are no hits in `internal/api/openapi.yaml`, `web/`, `internal/`, `docs/` or the chart values and README. The generated hits are `api/v1/zz_generated.deepcopy.go`, both CRDs in `config/crd/bases/` and `deploy/helm/tinycdi/crds/` (the Workspace CRD embeds the template spec in `status.templateSnapshot`), and `deploy/helm/testdata/default-render.golden.yaml`. Regenerating removes 37 lines from the golden and adds none.

**Files:**
- Modify: `api/v1/workspacetemplate_types.go:13-26` (the `BrowserPolicy` type and the blank line after it) and `:65-69` (the `LinuxRuntimeSpec.BrowserPolicy` field, its comment and the blank line after it)
- Regenerate: `api/v1/zz_generated.deepcopy.go`; `config/crd/bases/workspaces.cdi.tinyorbit.vn_workspaces.yaml`, `config/crd/bases/workspaces.cdi.tinyorbit.vn_workspacetemplates.yaml` and their copies in `deploy/helm/tinycdi/crds/`; `deploy/helm/testdata/default-render.golden.yaml`
- Modify: `config/samples/workspaces_v1_workspacetemplate.yaml:14-15`
- Test: `api/v1/validation_test.go` (fixture `linuxTemplate`, `:89-91`; new test at the end of the file)
- Docs: `docs/releases/v1.0.0.md` (Known limitations, before the "DNS egress is open on every network profile." bullet at `:149`)

Tasks 19 and 20 edit `internal/runtime/linux/backend.go` but not this field; this task does not touch `backend.go`. Task 20 inserts a Known limitations bullet *after* the DNS egress bullet, and this task inserts one *before* it, so the anchors do not collide.

**Interfaces:**
- Consumes: envtest globals `dyn`, `tplGVR`, `testNS`, and helpers `linuxTemplate`, `mutate`, `live` (`api/v1/validation_test.go:23-200`).
- Produces:
  - The CRD field `spec.linux.browserPolicy` and the Go type `cdiv1.BrowserPolicy` no longer exist. A later task that adds a `LinuxRuntimeSpec` field must not reintroduce them.
  - Release-notes bullet title `**No per-template URL filtering.**`.

- [ ] **Step 1: Write the failing test**

Append to `api/v1/validation_test.go`:

```go

// spec.linux.browserPolicy is not part of the API: no runtime code ever
// applied it, so an admin could not rely on it to restrict the session
// browser. A strict create naming it is refused; the network profile is the
// only destination control.
func TestWorkspaceTemplate_BrowserPolicyFieldRemoved(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	obj := mutate(linuxTemplate("tpl-browserpolicy"), func(s map[string]interface{}) {
		live(s, "linux")["browserPolicy"] = map[string]interface{}{
			"allowedDomains": []interface{}{"*.corp.example"},
		}
	})
	_, err := dyn.Resource(tplGVR).Namespace(testNS).Create(ctx,
		&unstructured.Unstructured{Object: obj}, metav1.CreateOptions{FieldValidation: "Strict"})
	if err == nil || !strings.Contains(err.Error(), `unknown field "spec.linux.browserPolicy"`) {
		t.Fatalf("strict create with spec.linux.browserPolicy: err = %v, want unknown field", err)
	}
}
```

All imports (`context`, `strings`, `time`, `metav1`, `unstructured`) are already in the file.

- [ ] **Step 2: Run it and see it fail**

```bash
export KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path --bin-dir "$PWD/bin")"
go test ./api/v1/ -run TestWorkspaceTemplate_BrowserPolicyFieldRemoved -count=1 > /tmp/r1-08.log 2>&1; echo "rc=$?"; grep -E -- '--- FAIL|_test.go:[0-9]+' /tmp/r1-08.log
```

Expected: `rc=1` and `strict create with spec.linux.browserPolicy: err = <nil>, want unknown field` (seen on a scratch copy of HEAD).

- [ ] **Step 3: Remove the field**

In `api/v1/workspacetemplate_types.go`, delete the whole type, together with the blank line after it:

```go
// BrowserPolicy carries the admin-set restrictions applied to a Browser
// experience: which destinations the in-session browser may reach. The end
// user cannot widen this set.
type BrowserPolicy struct {
	// allowedDomains is the allowlist of DNS domains the session browser may
	// reach. Empty means the network profile alone decides.
	// +optional
	AllowedDomains []string `json:"allowedDomains,omitempty"`

	// deniedDomains is subtracted from the allowlist.
	// +optional
	DeniedDomains []string `json:"deniedDomains,omitempty"`
}

```

In `LinuxRuntimeSpec`, delete the field, together with the blank line after it:

```go
	// browserPolicy restricts the in-session browser when experience is
	// Browser.
	// +optional
	BrowserPolicy *BrowserPolicy `json:"browserPolicy,omitempty"`

```

In `config/samples/workspaces_v1_workspacetemplate.yaml`, delete:

```yaml
    browserPolicy:
      allowedDomains: ["*.corp.example"]
```

In `api/v1/validation_test.go`, `linuxTemplate`, replace

```go
				"image": "registry.local/tinycdi/browser@sha256:" + digest(),
				"browserPolicy": map[string]interface{}{
					"allowedDomains": []interface{}{"*.corp.example"},
				},
			},
```

with

```go
				"image": "registry.local/tinycdi/browser@sha256:" + digest(),
			},
```

Regenerate:

```bash
make manifests generate sync-chart-crds > /tmp/r1-08-gen.log 2>&1; echo "gen rc=$?"
(cd deploy/helm && ${TCDI_HELM:-helm} template tcdi ./tinycdi --namespace tcdi-system --include-crds -f tinycdi/ci/minimal-values.yaml > testdata/default-render.golden.yaml); echo "golden rc=$?"
rg -n -i 'browserPolicy|allowedDomains|deniedDomains' --glob '!docs/superpowers/**' --glob '!docs/security/review-v1.0.md' .
```

Expected: `gen rc=0` and `golden rc=0`. The search prints only the new test's own lines in `api/v1/validation_test.go`. `git diff --stat` shows the golden losing 37 lines and gaining none.

- [ ] **Step 4: Run it and see it pass**

```bash
go build ./... && go vet ./... > /tmp/r1-08-vet.log 2>&1; echo "vet rc=$?"
go test ./api/v1/ -count=1 > /tmp/r1-08.log 2>&1; echo "api rc=$?"; grep -E -- '--- FAIL|_test.go:[0-9]+' /tmp/r1-08.log | head
(cd deploy/helm && go test . -run 'Golden|DefaultRender|CRD' -count=1 > /tmp/r1-08-helm.log 2>&1; echo "helm rc=$?"); grep -E -- '--- FAIL' /tmp/r1-08-helm.log | head
```

Expected: `vet rc=0`, `api rc=0` (the whole envtest package, including `TestWorkspaceTemplateCreateValidation` on the trimmed fixture), and `helm rc=0` (`TestDefaultRenderUnchangedGolden`, `TestChartCRDsMatchGenerated` and `TestCRDsShippedButNotDeletedByUninstall` among them).

- [ ] **Step 5: Docs**

`docs/releases/v1.0.0.md`, Known limitations: directly before the bullet `- **DNS egress is open on every network profile.**`, insert:

```markdown
- **No per-template URL filtering.** A `WorkspaceTemplate` cannot limit
  which sites its browser opens. The template's network profile
  (`Isolated`, `ClusterOnly` or `InternetOnly`) is the only destination
  control, and it filters by address, not by name.
```

No other doc mentions the field (see "Checked at HEAD"). `docs/security/review-v1.0.md` keeps the finding text, and Task 30 records its status.

- [ ] **Step 6: Gates and commit**

```bash
for t in .github/tests/*.test.sh; do bash "$t" > /tmp/r1-08-gate.log 2>&1 || { echo "FAIL $t"; tail -20 /tmp/r1-08-gate.log; exit 1; }; done; echo gates-ok
git add api/v1/workspacetemplate_types.go api/v1/zz_generated.deepcopy.go api/v1/validation_test.go \
  config/crd/bases/workspaces.cdi.tinyorbit.vn_workspaces.yaml config/crd/bases/workspaces.cdi.tinyorbit.vn_workspacetemplates.yaml \
  deploy/helm/tinycdi/crds/workspaces.cdi.tinyorbit.vn_workspaces.yaml deploy/helm/tinycdi/crds/workspaces.cdi.tinyorbit.vn_workspacetemplates.yaml \
  deploy/helm/testdata/default-render.golden.yaml config/samples/workspaces_v1_workspacetemplate.yaml docs/releases/v1.0.0.md
git status --short   # expect nothing else modified by the generators
git commit -m "fix(crd)!: remove the unenforced WorkspaceTemplate browserPolicy" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

Expected: `gates-ok`, and `git status --short` lists no other generated file (the generators touch only the deepcopy and the two CRDs).

### Task 22: CA rotation severs removed-CA conns; canonical blocklist issuers (R1-30, R1-31) — tier: design

**Findings.**
- **R1-30 (P3, TM-38): rotating the internal client CA does not drop established connections, although the docs say it does.**
  - **Attack.** An attacker holds a stolen internal client certificate, or a key minted from a compromised internal CA. They keep one HTTP/1.1 keep-alive connection to `:9443` open and send a request at least every 120 s. Incident response follows the runbook and rotates the CA. New handshakes now fail, and resumption is refused too, but the attacker's existing connection keeps being served indefinitely. Through it they can call the operator routes (revoke leases, drain any workspace) or, with a non-operator CN, the gateway routes. The CA pool is consulted only at handshake time (`internal/backend/listeners.go:85-95`). `docs/runbooks/mtls-cert-revocation.md:117-119` says "rotate the CA (rotation drops every client at once …)".
  - **Required fix.** On every CA pool swap, walk the live internal connections and close each one whose verified chain no longer verifies against the new pool. Track every verified conn, not only when `-internal-revoked-list` is set.
  - **Security test.** `TestInternalListener_RemovedCASeversLiveConn` in `internal/backend/mtls_reload_test.go`. The established CA1 conn fails its next read within 5 s after CA1 leaves the bundle, while a CA2 conn is still served.
- **R1-31 (P3, TM-38): blocklist issuer matching uses Go's `pkix.Name.String()`, but the runbook tells operators to use `openssl -nameopt RFC2253`. The two differ for common corporate DNs, so a revocation silently does nothing.**
  - **Attack.** The install uses `certManager.issuerRef` with a corporate CA whose DN carries `emailAddress`, non-ASCII characters (`O=Société…`) or multi-valued RDNs. The operator blocklists a stolen operator cert by following the runbook. The file parses, the entry never matches, and the stolen cert keeps working with no warning. Proof: openssl prints `emailAddress=pki@corp.example,CN=Corp Issuing CA,O=Soci\C3\A9t\C3\A9 Exemple,C=VN`. Go's `Issuer.String()` prints `CN=Corp Issuing CA,O=Société Exemple,C=VN,1.2.840.113549.1.9.1=#0c10…`. The key is compared byte for byte (`internal/tlsreload/blocklist.go:137-141`, `:271-291`).
  - **Required fix.** Compare issuers canonically, not as strings. At load, warn on any entry whose issuer names no certificate in the current client-CA bundle. Fix the runbook so its command matches.
  - **Security test.** `TestBlocklistIssuerCanonicalMatch` in `internal/tlsreload/blocklist_test.go`. The openssl RFC 2253 form of a DN with `emailAddress` and UTF-8 must block the leaf. An entry naming an issuer absent from the client-CA bundle must produce a load warning.

**Design (planner's ruling).**
- **Canonical issuer key.** The blocklist keeps the `<serial-hex> <issuer-dn>` format. The issuer is parsed as an RFC 2253 / RFC 4514 string into the sorted multiset of `(attribute OID, decoded value)` pairs. The presented leaf's `RawIssuer` DER decodes into the same key.
  - This accepts every way of writing one DN: OpenSSL's `-nameopt RFC2253`, Go's `String()`, any attribute order, `\XX` hex escapes, `\,` escapes, `#<DER hex>` values, and the type aliases `CN`, `O`, `OU`, `C`, `L`, `ST`, `STREET`, `SERIALNUMBER`, `POSTALCODE`, `DC`, `UID`, `emailAddress`/`E`, and dotted OIDs.
  - Keying on the issuer's key ID was rejected because it changes the file format and every existing test.
  - A line whose issuer does not parse fails the whole file, as any malformed line already does.
- **Live-conn walk on CA swap.** `CAPool.Subscribe` runs subscribers with the new pool after each swap. A new `clientConns` registry in `internal/backend/listeners.go` records every verified conn with its presented chain, and on each swap closes those whose leaf no longer verifies (`x509.Verify` with `ExtKeyUsageClientAuth`). Registration re-verifies against the current pool under the registry lock, which closes the race: a handshake that verified against the old pool just before a swap is refused, or is caught by the walk.

**Files:**
- Create: `internal/tlsreload/dn.go` (`canonicalDN`, `canonicalRawName`)
- Modify: `internal/tlsreload/reloader.go:31-40` (`options`: new field `knownIssuers`)
- Modify: `internal/tlsreload/capool.go:19-94` (`CAPool` gains `certs`, `subs`, `Certs()`, `Subscribe()`; `loadPool` returns the parsed certs)
- Modify: `internal/tlsreload/blocklist.go:36-291` (`WithKnownIssuers`, `revokedEntry` comment, `Revocations`, `Blocked`, `Track`, `reload`, `NewBlocklist`, `parseBlocklist`, `parseBlocklistLine`, new `warnUnknownIssuers`)
- Modify: `internal/backend/listeners.go:85-185` (new `clientConns`, `installClientConnTracking`, `internalConnState`; `installRevocation` chains the prior `VerifyConnection`; `connStateDrop` delegates)
- Modify: `internal/backend/backend.go:88-92` (field `internalConns *clientConns`)
- Modify: `internal/backend/wire.go:470-519` (wire the registry and `WithKnownIssuers`), `wire.go:959-964` (`bind`: ConnState always set)
- Test: `internal/tlsreload/blocklist_test.go` (new `TestBlocklistIssuerCanonicalMatch`; `TestParseBlocklistFormat` and `TestParseBlocklistLineIssuerSpaces` move to canonical keys), `internal/backend/mtls_reload_test.go` (new test, `serveInternalTLSList` installs tracking the way `wire.go` does)
- Modify comment: `internal/backend/config.go:185-187` (`InternalRevokedList`; Step 7)
- Docs: `docs/runbooks/mtls-cert-revocation.md:18-40,114-119`, `docs/adr/0008-mtls-cert-revocation.md:36-42,54-68`, `docs/security/threat-model.md:526-536` (Boundary 7 bullets), `docs/security/test-inventory.md:466` (Step 7)

**Interfaces:**
- Consumes: `tlsreload.NewCAPool`, `tlsreload.NewBlocklist`, `installRevocation`, `hotReloadClientCAs` (unchanged signatures).
- Produces:
  - `func (p *tlsreload.CAPool) Certs() []*x509.Certificate`: the newest parsed bundle.
  - `func (p *tlsreload.CAPool) Subscribe(fn func(*x509.CertPool))`: runs `fn` with the new pool after every swap to a changed bundle.
  - `func tlsreload.WithKnownIssuers(certs func() []*x509.Certificate) tlsreload.Option`: a Blocklist warns at each load or swap on entries whose issuer names no cert in `certs()`.
  - Unexported in `internal/tlsreload`: `canonicalDN(s string) (string, error)`, `canonicalRawName(der []byte) (string, error)`.
  - In `internal/backend`: `type clientConns`, `newClientConns(pool *tlsreload.CAPool) *clientConns`, `(*clientConns).tracked() int`, `installClientConnTracking(cfg *tls.Config, conns *clientConns)`, and `internalConnState(conns *clientConns, bl *tlsreload.Blocklist) func(net.Conn, http.ConnState)`. Task 23 sets `ErrorLog` on the same `internalSrv` in `bind`.
  - `Revocations.entries` becomes `map[revokedEntry]string` (the value is the issuer as written in the file, used in warnings). `parseBlocklistLine` returns `(revokedEntry, string, error)`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/backend/mtls_reload_test.go`, and add `"bufio"` and `"errors"` to its imports (`net` is already imported). `newTestCANamed`/`sign` (`backend_test.go:485,513`), `dialMTLS`/`mtlsGet`/`clientTLSCfg` (`mtls_revocation_test.go:80-112`) and `writeFileAtomic`/`writeTestCert` already exist in package `backend`:

```go
// waitSevered polls conn until a read fails with something other than a
// deadline timeout (EOF or reset: the server closed it); it fails the
// test after d. A timeout means the conn is still open and idle.
func waitSevered(t *testing.T, conn net.Conn, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	buf := make([]byte, 1)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		_, err := conn.Read(buf)
		var ne net.Error
		if err != nil && !(errors.As(err, &ne) && ne.Timeout()) {
			return
		}
	}
	t.Fatal("established conn was not severed")
}

// TestInternalListener_RemovedCASeversLiveConn (R1-30, TM-38): when CA1
// leaves the client-CA bundle, an established keep-alive conn
// authenticated by a CA1 cert is closed on the same reload pass — a
// rotation must not leave a stolen cert's live conn served — while a conn
// from the still-trusted CA2 keeps answering.
func TestInternalListener_RemovedCASeversLiveConn(t *testing.T) {
	dir := t.TempDir()
	ca1 := newTestCANamed(t, "internal-ca-1")
	ca2 := newTestCANamed(t, "internal-ca-2")
	caFile := filepath.Join(dir, "clients-ca.pem")
	bundle := append(append([]byte(nil), ca1.pem...), ca2.pem...)
	if err := writeFileAtomic(caFile, bundle); err != nil {
		t.Fatal(err)
	}
	pool, err := tlsreload.NewCAPool(caFile, tlsreload.WithInterval(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go pool.Run(ctx)

	cf, kf := writeTestCert(t, dir, "internal.test")
	srvCert, err := tls.LoadX509KeyPair(cf, kf)
	if err != nil {
		t.Fatal(err)
	}
	addr := serveInternalTLS(t, srvCert, bundle, pool)

	conn1 := dialMTLS(t, addr, clientTLSCfg(ca1.sign(t, "operator")))
	br1 := bufio.NewReader(conn1)
	conn2 := dialMTLS(t, addr, clientTLSCfg(ca2.sign(t, "operator")))
	br2 := bufio.NewReader(conn2)
	if mtlsGet(t, conn1, br1) != http.StatusOK || mtlsGet(t, conn2, br2) != http.StatusOK {
		t.Fatal("setup request failed")
	}

	if err := writeFileAtomic(caFile, ca2.pem); err != nil {
		t.Fatal(err)
	}
	waitSevered(t, conn1, 5*time.Second)
	if mtlsGet(t, conn2, br2) != http.StatusOK {
		t.Fatal("conn from the still-trusted CA2 was severed")
	}
	_ = conn2.Close()
}
```

Append to `internal/tlsreload/blocklist_test.go`, and add `"bytes"` and `"encoding/asn1"` to its imports:

```go
// TestBlocklistIssuerCanonicalMatch (R1-31, TM-38): an entry's issuer
// matches canonically. The runbook's `openssl x509 -noout -issuer
// -nameopt RFC2253` form of a corporate DN (emailAddress attribute, UTF-8
// hex escapes, reversed order) blocks the leaf exactly like Go's
// pkix.Name.String() form; an issuer that is not a DN fails the file
// instead of loading an entry that can never match; an entry naming an
// issuer absent from the client-CA bundle logs a warning at load.
func TestBlocklistIssuerCanonicalMatch(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	emailOID := asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 1}
	caTpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName:   "Corp Issuing CA",
			Organization: []string{"Société Exemple"},
			Country:      []string{"VN"},
			ExtraNames:   []pkix.AttributeTypeAndValue{{Type: emailOID, Value: "pki@corp.example"}},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafTpl := &x509.Certificate{
		SerialNumber: big.NewInt(0x4f8b),
		Subject:      pkix.Name{CommonName: "operator"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTpl, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(t.TempDir(), "revoked-serials.txt")
	for _, issuer := range []string{
		`emailAddress=pki@corp.example,CN=Corp Issuing CA,O=Soci\C3\A9t\C3\A9 Exemple,C=VN`, // openssl -nameopt RFC2253
		leaf.Issuer.String(), // Go's pkix.Name.String()
		`C=VN, O=Société Exemple, CN=Corp Issuing CA, E=pki@corp.example`,
	} {
		writeList(t, file, "4f8b "+issuer+"\n")
		bl, err := NewBlocklist(file, WithLogger(testLogger()))
		if err != nil {
			t.Fatalf("NewBlocklist(%q): %v", issuer, err)
		}
		if err := bl.Track(nil, leaf); !errors.Is(err, ErrCertRevoked) {
			t.Errorf("issuer %q: leaf 4f8b not blocked (Track err = %v)", issuer, err)
		}
	}

	// An issuer that is not a DN fails the whole file — never a silently
	// dead entry.
	writeList(t, file, "4f8b not-a-dn\n")
	if _, err := NewBlocklist(file, WithLogger(testLogger())); err == nil {
		t.Error("an issuer that is not a DN loaded")
	}

	// An issuer absent from the client-CA bundle is a load-time warning;
	// a known one is silent.
	known := WithKnownIssuers(func() []*x509.Certificate { return []*x509.Certificate{ca} })
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	writeList(t, file, "4f8b CN=Some Other CA\n")
	if _, err := NewBlocklist(file, WithLogger(log), known); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "outside the client-CA bundle") || !strings.Contains(buf.String(), "CN=Some Other CA") {
		t.Errorf("unknown issuer: no load warning naming it, log = %s", buf.String())
	}
	buf.Reset()
	writeList(t, file, "4f8b "+leaf.Issuer.String()+"\n")
	if _, err := NewBlocklist(file, WithLogger(log), known); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "outside the client-CA bundle") {
		t.Errorf("known issuer warned: %s", buf.String())
	}
}
```

- [ ] **Step 2: Run the tests and watch them fail**

Run:
```bash
go test ./internal/backend/ -run TestInternalListener_RemovedCASeversLiveConn -count=1 2>&1 | tail -5
go test ./internal/tlsreload/ -run TestBlocklistIssuerCanonicalMatch -count=1 2>&1 | tail -5
```
Expected:
- The backend test FAILS with `mtls_reload_test.go:…: established conn was not severed`.
- The tlsreload test FAILS to compile with `undefined: WithKnownIssuers`. If you comment out the warning block, it fails with `issuer "emailAddress=pki@corp.example,…": leaf 4f8b not blocked`, which is the R1-31 gap.

- [ ] **Step 3: Canonical DN keys**

Create `internal/tlsreload/dn.go`:

```go
// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package tlsreload

import (
	"bytes"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// dnAttrOIDs maps the attribute type names a DN string may use — Go's
// pkix.Name.String() names and OpenSSL's RFC 2253 names — to their OIDs.
var dnAttrOIDs = map[string]string{
	"cn": "2.5.4.3", "c": "2.5.4.6", "l": "2.5.4.7", "st": "2.5.4.8",
	"street": "2.5.4.9", "o": "2.5.4.10", "ou": "2.5.4.11",
	"serialnumber": "2.5.4.5", "postalcode": "2.5.4.17",
	"dc": "0.9.2342.19200300.100.1.25", "uid": "0.9.2342.19200300.100.1.1",
	"emailaddress": "1.2.840.113549.1.9.1", "e": "1.2.840.113549.1.9.1",
}

// canonicalDN parses an RFC 2253 / RFC 4514 distinguished name — the
// form `openssl x509 -noout -issuer -nameopt RFC2253` prints and the
// form Go's pkix.Name.String() prints — into a canonical key: the sorted
// multiset of "OID=value" pairs, NUL-joined. Both renderings of one
// certificate name yield the same key whatever their attribute order,
// escaping (\XX hex bytes, \, …) or value encoding (#<DER hex>), and it
// equals canonicalRawName of the name's DER (ADR 0008, TM-38).
func canonicalDN(s string) (string, error) {
	var pairs []string
	i := 0
	for {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		j := strings.IndexByte(s[i:], '=')
		if j <= 0 {
			return "", fmt.Errorf("bad DN %q: want type=value", s)
		}
		typ := strings.ToLower(strings.TrimSpace(s[i : i+j]))
		oid, ok := dnAttrOIDs[typ]
		if !ok {
			if !isDottedOID(typ) {
				return "", fmt.Errorf("bad DN %q: unknown attribute type %q", s, typ)
			}
			oid = typ
		}
		i += j + 1
		val, n, err := dnValue(s[i:])
		if err != nil {
			return "", fmt.Errorf("bad DN %q: %w", s, err)
		}
		pairs = append(pairs, oid+"="+val)
		i += n
		if i >= len(s) {
			break
		}
		i++ // the unescaped ',', ';' or '+' separator
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "\x00"), nil
}

// dnValue decodes one attribute value at the start of s and reports how
// many bytes it consumed (up to, not including, the next unescaped
// separator). Unescaped leading and trailing spaces are not part of the
// value; an escaped space is.
func dnValue(s string) (string, int, error) {
	i := 0
	for i < len(s) && s[i] == ' ' {
		i++
	}
	if i < len(s) && s[i] == '#' {
		j := i + 1
		for j < len(s) && isHex(s[j]) {
			j++
		}
		der, err := hex.DecodeString(s[i+1 : j])
		if err != nil {
			return "", 0, fmt.Errorf("bad #hex value: %w", err)
		}
		var v string
		if rest, err := asn1.Unmarshal(der, &v); err != nil || len(rest) != 0 {
			return "", 0, errors.New("#hex value is not one DER string")
		}
		for j < len(s) && s[j] == ' ' {
			j++
		}
		if j < len(s) && !isDNSep(s[j]) {
			return "", 0, errors.New("junk after #hex value")
		}
		return v, j, nil
	}
	var b []byte
	trail := 0
	for i < len(s) && !isDNSep(s[i]) {
		c := s[i]
		if c == '\\' {
			switch {
			case i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]):
				x, _ := strconv.ParseUint(s[i+1:i+3], 16, 8)
				b = append(b, byte(x))
				i += 3
			case i+1 < len(s):
				b = append(b, s[i+1])
				i += 2
			default:
				return "", 0, errors.New("dangling escape")
			}
			trail = 0
			continue
		}
		if c == ' ' {
			trail++
		} else {
			trail = 0
		}
		b = append(b, c)
		i++
	}
	b = b[:len(b)-trail]
	if len(b) == 0 || !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return "", 0, errors.New("empty or invalid attribute value")
	}
	return string(b), i, nil
}

// canonicalRawName is canonicalDN for a DER-encoded certificate name
// (x509.Certificate.RawIssuer / RawSubject).
func canonicalRawName(der []byte) (string, error) {
	var rdns pkix.RDNSequence
	if rest, err := asn1.Unmarshal(der, &rdns); err != nil || len(rest) != 0 {
		return "", errors.New("tlsreload: unparsable certificate name")
	}
	var pairs []string
	for _, rdn := range rdns {
		for _, atv := range rdn {
			v, ok := atv.Value.(string)
			if !ok {
				return "", fmt.Errorf("tlsreload: name attribute %s is not a string", atv.Type)
			}
			pairs = append(pairs, atv.Type.String()+"="+v)
		}
	}
	if len(pairs) == 0 {
		return "", errors.New("tlsreload: empty certificate name")
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "\x00"), nil
}

func isDNSep(c byte) bool { return c == ',' || c == ';' || c == '+' }

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func isDottedOID(s string) bool {
	if !strings.Contains(s, ".") {
		return false
	}
	for _, p := range strings.Split(s, ".") {
		if p == "" {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}
```

This parser was checked during planning against the R1-31 DN. The openssl form, Go's `String()` form and `C=VN, O=Société Exemple, CN=Corp Issuing CA, E=pki@corp.example` all equal `canonicalRawName(leaf.RawIssuer)`. `not-a-dn`, `CN=`, `CN=a,`, `XX=1` and `CN=a\` are refused.

- [ ] **Step 4: Blocklist uses the canonical key and warns on unknown issuers**

`internal/tlsreload/reloader.go`: add to `options` (after `onSwap`):

```go
	// knownIssuers returns the client-CA bundle a Blocklist checks its
	// entries' issuers against (WithKnownIssuers); nil skips the check.
	knownIssuers func() []*x509.Certificate
```
and add `"crypto/x509"` to that file's imports.

`internal/tlsreload/blocklist.go`:
- After `WithReloadObserver`, add:

```go
// WithKnownIssuers gives a Blocklist the current client-CA bundle: each
// load and swap then warns about every entry whose issuer names no
// certificate in it — an entry that can never match a verified leaf
// issued directly by a bundle CA (TM-38).
func WithKnownIssuers(certs func() []*x509.Certificate) Option {
	return func(o *options) { o.knownIssuers = certs }
}
```

- Replace the `revokedEntry` comment (lines 47-52): "revokedEntry is one (issuer, serial) pair. issuer is the canonical name key (canonicalDN of the file's DN, canonicalRawName of a leaf's RawIssuer), so every spelling of one DN — OpenSSL's RFC 2253 output, Go's pkix.Name.String(), any attribute order or escaping — compares equal; serial is the serial number normalized to lowercase hex (big.Int.Text(16)), so file spellings differing in case, colons or leading-zero octets compare equal."
- Replace `Revocations` and its `Blocked` (lines 59-69) with:

```go
// Revocations is an immutable snapshot of one loaded blocklist — the set
// of (issuer, serial) pairs the handshake path matches the verified peer
// leaf against, each mapped to its issuer as written in the file (for
// warnings).
type Revocations struct {
	entries map[revokedEntry]string
}

// Blocked reports whether the (issuer, serial) pair is listed. issuer is
// any DN spelling canonicalDN accepts; an unparsable one is never listed.
func (s *Revocations) Blocked(issuer, serial string) bool {
	key, err := canonicalDN(issuer)
	if err != nil {
		return false
	}
	return s.blocked(key, serial)
}

func (s *Revocations) blocked(issuerKey, serial string) bool {
	_, ok := s.entries[revokedEntry{issuer: issuerKey, serial: serial}]
	return ok
}
```

- In `Track`, replace the first line `key := revokedEntry{issuer: leaf.Issuer.String()}` with:

```go
	// A leaf whose issuer name does not decode gets the empty key, which
	// no entry carries (canonicalDN never returns "").
	issuer, _ := canonicalRawName(leaf.RawIssuer)
	key := revokedEntry{issuer: issuer}
```
  and change `if b.cur.Blocked(key.issuer, key.serial) {` to `if b.cur.blocked(key.issuer, key.serial) {`.
- In `reload`, change `if set.Blocked(key.issuer, key.serial) {` to `if set.blocked(key.issuer, key.serial) {`. Directly after the `b.mu.Unlock()` that follows the sever loop, add `b.warnUnknownIssuers(set)`.
- In `NewBlocklist`, directly before `if b.onSwap != nil {`, add `b.warnUnknownIssuers(set)`.
- Add the method after `Tracked`:

```go
// warnUnknownIssuers logs one warning per entry whose issuer names no
// certificate in the WithKnownIssuers bundle: such an entry can never
// match a leaf a bundle CA issued directly. Loading still
// succeeds — the bundle may be mid-rotation.
func (b *Blocklist) warnUnknownIssuers(set *Revocations) {
	if b.knownIssuers == nil {
		return
	}
	known := map[string]bool{}
	for _, c := range b.knownIssuers() {
		if k, err := canonicalRawName(c.RawSubject); err == nil {
			known[k] = true
		}
	}
	for e, written := range set.entries {
		if !known[e.issuer] {
			b.log.Warn("tlsreload: revocation entry names an issuer outside the client-CA bundle; it can never match",
				"file", b.file, "serial", e.serial, "issuer", written)
		}
	}
}
```

- `parseBlocklist`: the return type becomes `(map[revokedEntry]string, error)`. Build it with `entries := map[revokedEntry]string{}`, call `e, written, err := parseBlocklistLine(line)`, and store `entries[e] = written`. Update its doc comment: "issuer: an RFC 2253 DN in any spelling, matched canonically". `loadBlocklist` is unchanged (it stores the map into `Revocations{entries: entries}`).
- `parseBlocklistLine` becomes:

```go
// parseBlocklistLine parses one "<serial-hex> <issuer-dn>" line. The
// serial is hex (colons stripped, case-insensitive, no 0x prefix,
// leading-zero octets optional) and normalizes via big.Int.Text(16); the
// issuer is the rest of the line, trimmed — a DN may carry spaces — and
// must parse as a DN (canonicalDN): an issuer that cannot match any
// certificate fails the line. It returns the entry and the issuer as
// written.
func parseBlocklistLine(line string) (revokedEntry, string, error) {
	i := strings.IndexAny(line, " \t")
	if i < 0 {
		return revokedEntry{}, "", errors.New("want \"<serial-hex> <issuer-dn>\"")
	}
	serialTok, issuer := line[:i], strings.TrimSpace(line[i:])
	if issuer == "" {
		return revokedEntry{}, "", errors.New("missing issuer DN")
	}
	hexStr := strings.ReplaceAll(serialTok, ":", "")
	if hexStr == "" {
		return revokedEntry{}, "", fmt.Errorf("bad serial %q", serialTok)
	}
	for _, r := range hexStr {
		if !('0' <= r && r <= '9') && !('a' <= r && r <= 'f') && !('A' <= r && r <= 'F') {
			return revokedEntry{}, "", fmt.Errorf("bad serial %q", serialTok)
		}
	}
	key, err := canonicalDN(issuer)
	if err != nil {
		return revokedEntry{}, "", err
	}
	n, _ := new(big.Int).SetString(hexStr, 16) // hex-only input cannot fail
	return revokedEntry{issuer: key, serial: n.Text(16)}, issuer, nil
}
```

Update the two parse tests in `internal/tlsreload/blocklist_test.go`, which compare internal keys:
- In `TestParseBlocklistFormat`, add the helper below to the file. Build `want` with `{issuer: mustCanonicalDN(t, "CN=tinycdi-internal-ca"), serial: "4f8b2c1a09d3e4f5"}`, `{issuer: mustCanonicalDN(t, "CN=Second CA, O=Org"), serial: "4ab"}` and `{issuer: mustCanonicalDN(t, "CN=zero"), serial: "0"}`. Its doc comment's "issuer compared byte-for-byte after a trim" becomes "issuer compared canonically".

  ```go
  func mustCanonicalDN(t *testing.T, s string) string {
  	t.Helper()
  	k, err := canonicalDN(s)
  	if err != nil {
  		t.Fatal(err)
  	}
  	return k
  }
  ```

- In `TestParseBlocklistLineIssuerSpaces`, use `e, written, err := parseBlocklistLine("4f8b   CN=Some CA, O=Org Inc  ")`. Assert `written == "CN=Some CA, O=Org Inc"`, `e.issuer == mustCanonicalDN(t, "O=Org Inc,CN=Some CA")` and `e.serial == "4f8b"`.

- [ ] **Step 5: CAPool exposes its certs and notifies subscribers**

`internal/tlsreload/capool.go`: add `"encoding/pem"` and `"sync"` to the imports. Replace the struct, `NewCAPool`, `reload` and `loadPool`, and add the two methods:

```go
type CAPool struct {
	file string
	options

	cur   atomic.Pointer[x509.CertPool]
	certs atomic.Pointer[[]*x509.Certificate]
	hash  [32]byte

	mu   sync.Mutex
	subs []func(*x509.CertPool)
}

// NewCAPool loads the bundle once and fails if it is unusable.
func NewCAPool(file string, opts ...Option) (*CAPool, error) {
	p := &CAPool{
		file:    file,
		options: options{interval: 30 * time.Second, log: slog.Default()},
	}
	for _, o := range opts {
		o(&p.options)
	}
	pool, certs, hash, err := loadPool(file)
	if err != nil {
		return nil, err
	}
	p.cur.Store(pool)
	p.certs.Store(&certs)
	p.hash = hash
	return p, nil
}

// Certs returns the newest parsed bundle's certificates (the blocklist's
// known-issuer check reads it).
func (p *CAPool) Certs() []*x509.Certificate {
	return *p.certs.Load()
}

// Subscribe registers fn to run with the new pool after every swap to a
// changed bundle — the internal listener severs live connections whose
// chain the new bundle no longer verifies (TM-38).
func (p *CAPool) Subscribe(fn func(*x509.CertPool)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subs = append(p.subs, fn)
}

func (p *CAPool) reload() {
	pool, certs, hash, err := loadPool(p.file)
	if err != nil {
		p.log.Error("tlsreload: CA reload failed, keeping previous pool",
			"file", p.file, "err", err)
		return
	}
	if hash == p.hash {
		return
	}
	p.cur.Store(pool)
	p.certs.Store(&certs)
	p.hash = hash
	p.log.Info("tlsreload: reloaded CA bundle", "file", p.file)
	p.mu.Lock()
	subs := append([]func(*x509.CertPool)(nil), p.subs...)
	p.mu.Unlock()
	for _, fn := range subs {
		fn(pool)
	}
}

func loadPool(file string) (*x509.CertPool, []*x509.Certificate, [32]byte, error) {
	pemBytes, err := os.ReadFile(file)
	if err != nil {
		return nil, nil, [32]byte{}, err
	}
	pool := x509.NewCertPool()
	var certs []*x509.Certificate
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			continue // the blocks AppendCertsFromPEM skips
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		pool.AddCert(c)
		certs = append(certs, c)
	}
	if len(certs) == 0 {
		return nil, nil, [32]byte{}, errors.New("tlsreload: CA file contains no PEM certificates")
	}
	return pool, certs, sha256.Sum256(pemBytes), nil
}
```

Run: `go test ./internal/tlsreload/ -count=1 2>&1 | tail -5`. Expected: `ok`. `TestBlocklistIssuerCanonicalMatch` now passes.

- [ ] **Step 6: The internal listener tracks every verified conn**

`internal/backend/listeners.go`: add `"crypto/x509"`, `"errors"` and `"sync"` to the imports. After `hotReloadClientCAs`, add:

```go
// errClientCARotated refuses a handshake whose chain verified against a
// client-CA bundle that was swapped out before the conn registered.
var errClientCARotated = errors.New("internal mTLS: client CA bundle rotated during the handshake")

// clientConns is the internal listener's live verified-conn registry
// (TM-38). Every conn whose client certificate verified is recorded with
// the chain it presented; a client-CA swap re-verifies each one against
// the new bundle and closes those that no longer verify, so rotating a CA
// out ends its established connections as well as new handshakes.
type clientConns struct {
	current func() *x509.CertPool

	// mu serializes registration against the swap walk: a conn verified
	// against the old bundle either fails track's re-verification or is
	// in the map when the walk runs.
	mu    sync.Mutex
	conns map[net.Conn][]*x509.Certificate
}

// newClientConns builds the registry over pool and subscribes it to the
// pool's swaps.
func newClientConns(pool *tlsreload.CAPool) *clientConns {
	c := &clientConns{current: pool.Pool, conns: map[net.Conn][]*x509.Certificate{}}
	pool.Subscribe(c.sever)
	return c
}

// verifyClientChain reports whether chain (leaf first, as presented)
// verifies as a client certificate against roots.
func verifyClientChain(chain []*x509.Certificate, roots *x509.CertPool) bool {
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	_, err := chain[0].Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: inter,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	return err == nil
}

// track re-verifies chain against the current bundle and registers conn.
// A nil conn checks without registering.
func (c *clientConns) track(conn net.Conn, chain []*x509.Certificate) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !verifyClientChain(chain, c.current()) {
		return errClientCARotated
	}
	if conn != nil {
		c.conns[conn] = chain
	}
	return nil
}

// sever closes every registered conn whose chain pool no longer verifies.
func (c *clientConns) sever(pool *x509.CertPool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for conn, chain := range c.conns {
		if !verifyClientChain(chain, pool) {
			_ = conn.Close()
			delete(c.conns, conn)
		}
	}
}

func (c *clientConns) drop(conn net.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.conns, conn)
}

// tracked reports the registry size.
func (c *clientConns) tracked() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.conns)
}

// installClientConnTracking wraps cfg.GetConfigForClient so every
// per-handshake clone registers its verified conn in conns (after any
// VerifyConnection already on the clone). Absent certs pass untouched:
// they reach the handler's 401.
func installClientConnTracking(cfg *tls.Config, conns *clientConns) {
	inner := cfg.GetConfigForClient
	if inner == nil {
		inner = func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return cfg.Clone(), nil
		}
	}
	cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		c, err := inner(hello)
		if err != nil {
			return nil, err
		}
		prev := c.VerifyConnection
		c.VerifyConnection = func(cs tls.ConnectionState) error {
			if prev != nil {
				if err := prev(cs); err != nil {
					return err
				}
			}
			if len(cs.PeerCertificates) == 0 {
				return nil
			}
			return conns.track(hello.Conn, cs.PeerCertificates)
		}
		return c, nil
	}
}
```

In `installRevocation`, keep the clone's existing `VerifyConnection`, so tracking still runs after the revocation check. Directly after the `if err != nil { return nil, err }` that follows `c, err := inner(hello)`, add `next := c.VerifyConnection`. Then replace

```go
			err := bl.Track(hello.Conn, leaf)
			if err == nil {
				return nil
			}
```
with
```go
			err := bl.Track(hello.Conn, leaf)
			if err == nil {
				if next != nil {
					return next(cs)
				}
				return nil
			}
```

Replace `connStateDrop` (lines 169-185) with:

```go
// internalConnState returns the internal listener's ConnState hook: it
// drops a conn from every live-conn registry (the client-CA registry and
// the ADR 0008 blocklist, either may be nil) when it leaves the server
// (StateClosed/StateHijacked), so neither registry can leak. Both key on
// the raw conn the handshake saw; the server's conn is the *tls.Conn
// wrapper, unwrapped here.
func internalConnState(conns *clientConns, bl *tlsreload.Blocklist) func(net.Conn, http.ConnState) {
	return func(c net.Conn, st http.ConnState) {
		if st != http.StateClosed && st != http.StateHijacked {
			return
		}
		if tc, ok := c.(*tls.Conn); ok {
			c = tc.NetConn()
		}
		if conns != nil {
			conns.drop(c)
		}
		if bl != nil {
			bl.Drop(c)
		}
	}
}

// connStateDrop is internalConnState for the blocklist alone.
func connStateDrop(bl *tlsreload.Blocklist) func(net.Conn, http.ConnState) {
	return internalConnState(nil, bl)
}
```

`internal/backend/backend.go`: after the `internalBlocklist` field (line 92), add:

```go
	// internalConns is the internal listener's verified-conn registry
	// (TM-38): a client-CA swap closes conns whose chain the new bundle
	// no longer verifies. Set whenever the internal listener is on.
	internalConns *clientConns
```

`internal/backend/wire.go`:
- After `hotReloadClientCAs(tlsCfg, caPool)` (line 495), add:

  ```go
  		// TM-38: a client-CA rotation also ends established conns whose
  		// chain the new bundle no longer verifies.
  		b.internalConns = newClientConns(caPool)
  		installClientConnTracking(tlsCfg, b.internalConns)
  ```

- In the `NewBlocklist` call (line 510), change its last line `tlsreload.WithReloadObserver(onErr, onSwap))` (no trailing comma at HEAD) to `tlsreload.WithReloadObserver(onErr, onSwap),` followed by a new line `tlsreload.WithKnownIssuers(caPool.Certs))`.
- In `bind` (lines 960-964), replace the `if bl := b.internalBlocklist; bl != nil { … }` block with:

  ```go
  	// Live-conn registries (TM-38, ADR 0008) track only conns the server
  	// still holds — drop each on close/hijack so neither can leak.
  	internalSrv.ConnState = internalConnState(b.internalConns, b.internalBlocklist)
  ```

`internal/backend/mtls_reload_test.go`, `serveInternalTLSList`: mirror `wire.go`. Directly after `hotReloadClientCAs(tlsCfg, pool)`, add `conns := newClientConns(pool)` and `installClientConnTracking(tlsCfg, conns)`. Replace the `if bl != nil { srv.ConnState = connStateDrop(bl) }` block with `srv.ConnState = internalConnState(conns, bl)`. Update its doc comment: "…plus the TM-38 client-CA conn registry, exactly as bind() wires them". `TestRevocationOffByteIdentical` builds its config with `hotReloadClientCAs` alone, so it is unaffected.

Run:
```bash
go build ./... && go vet ./internal/backend/ ./internal/tlsreload/
go test ./internal/backend/ -run 'TestInternalListener_|TestRevoked|TestRevocation' -count=1 2>&1 | tail -5
go test ./internal/tlsreload/ -count=1 2>&1 | tail -3
go test -race ./internal/backend/ -run 'TestInternalListener_RemovedCASeversLiveConn|TestRevokedLiveConnSevered' -count=3 2>&1 | tail -3
```
Expected: `ok` for each, with no race report.

- [ ] **Step 7: Docs the fix makes true or false**

- `docs/runbooks/mtls-cert-revocation.md`:
  - Replace the `ISSUER-DN` bullet (lines 32-36) with: "`ISSUER-DN` — the rest of the line, trimmed: the issuing CA's distinguished name. Use `openssl x509 -noout -issuer -nameopt RFC2253` output after `issuer=` (for the chart-issued self-signed CA, `CN=tinycdi-internal-ca`). The match is canonical: attribute order, escaping (`\C3\A9`, `\,`) and Go's `pkix.Name.String()` spelling all compare equal. A line whose issuer does not parse as a DN rejects the file. An entry whose issuer names no CA in the client-CA bundle loads, but logs `revocation entry names an issuer outside the client-CA bundle` because it can never match. The match is an (issuer, serial) pair — serials are only unique per issuer."
  - Replace the closing sentence (lines 117-119) with: "If the internal CA itself is compromised, blocklisting is NOT the fix — rotate the CA. When the new bundle lands (Secret sync plus one 30 s poll), the backend refuses new handshakes from the removed CA and closes every established connection whose certificate chain no longer verifies against the new bundle."
- `docs/adr/0008-mtls-cert-revocation.md`:
  - Replace the `ISSUER-DN` bullet (lines 37-42) with: "`ISSUER-DN` — the rest of the line, trimmed, parsed as an RFC 2253 / RFC 4514 DN and compared canonically with the presented leaf's `RawIssuer`: the sorted set of (attribute OID, decoded value) pairs (`canonicalDN`, `canonicalRawName` in `internal/tlsreload/dn.go`), so OpenSSL's `-nameopt RFC2253` output and Go's `pkix.Name.String()` match the same certificate. For the chart-issued CA this is `CN=tinycdi-internal-ca`. An issuer that does not parse rejects the file; one naming no certificate in the client-CA bundle logs a warning at load (`WithKnownIssuers`). The issuer half makes the match an (issuer, serial) pair — serial numbers are unique only per issuer, and the client-CA bundle may carry several CAs during a rotation."
  - In the enforcement paragraph (lines 54-62), add after the sentence ending "the 401 model for absent certs is unchanged.": "Independently of the list, `installClientConnTracking` registers every verified conn with its presented chain, and a client-CA swap (`CAPool.Subscribe`) closes each one whose chain no longer verifies, so a CA rotation ends established conns as well as new handshakes."
- `docs/security/threat-model.md`, Boundary 7 `Controls:`:
  - In the **Hot reload** bullet, after "reload on file change (`tlsreload.Reloader`, `hotReloadClientCAs` in `internal/backend/listeners.go`).", insert: "A client-CA swap also closes every established connection whose certificate chain no longer verifies against the new bundle (`clientConns`, `installClientConnTracking`)."
  - In the **Serial revocation** bullet, after "names a blocklist of (issuer, serial) pairs;", insert: "issuers compare canonically (any RFC 2253 spelling of the DN), and an entry naming an issuer outside the client-CA bundle logs a warning at load;".

- "Feature off is byte-identical" claims: with this task every internal handshake gets a `VerifyConnection` from `installClientConnTracking`, so the claim that an unset revocation flag leaves the handshake byte-identical becomes false. Reword each one to "with the flag unset no revocation hook is installed; the client-CA conn tracking (`installClientConnTracking`) still runs":
  - `docs/adr/0008-mtls-cert-revocation.md`, the sentence "With the flag unset no hook is installed and the handshake path is byte-identical." (lines 66-68). In the same ADR, drop the line numbers from `internal/backend/listeners.go:120` (line 57) and `internal/tlsreload/blocklist.go:74-90` (line 63); keep the file paths.
  - the comments at `internal/backend/wire.go:501-502`, `internal/backend/listeners.go:117-119` (`installRevocation` doc comment) and `internal/backend/config.go:185-187` (`InternalRevokedList`).
  - `docs/security/test-inventory.md:466`: "feature-off installs no `VerifyConnection` (byte-identical)" becomes "feature-off installs no revocation `VerifyConnection`".
  `TestRevocationOffByteIdentical` (`mtls_revocation_test.go:365`) calls only `hotReloadClientCAs`, not `installClientConnTracking`, so it still passes; leave it.

Run: `for t in .github/tests/*.test.sh; do bash "$t" || exit 1; done`. Expected: exit 0 (the docs gates check the edited links and anchors).

- [ ] **Step 8: Commit**

```bash
git add internal/tlsreload/dn.go internal/tlsreload/reloader.go internal/tlsreload/capool.go internal/tlsreload/blocklist.go internal/tlsreload/blocklist_test.go \
  internal/backend/listeners.go internal/backend/backend.go internal/backend/wire.go internal/backend/config.go internal/backend/mtls_reload_test.go \
  docs/runbooks/mtls-cert-revocation.md docs/adr/0008-mtls-cert-revocation.md docs/security/threat-model.md docs/security/test-inventory.md
git commit -m "fix(mtls): CA rotation severs removed-CA connections; blocklist matches issuers canonically

R1-30: every verified internal conn is registered with its chain; a
client-CA swap closes those the new bundle no longer verifies.
R1-31: blocklist issuers are matched as canonical DNs, so the runbook's
openssl RFC 2253 form matches; unknown issuers warn at load.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 23: Gateway role opt-in by CN; handshake and role refusals counted (R1-32, R1-33) — tier: design

**Findings.**
- **R1-32 (P3, new on Boundary 7, nearest TM-38): the internal listener gives the gateway role to any CN other than the operator's, although the chart deploys no gateway clients. With a shared ClusterIssuer, that means anyone who can get a certificate from it.**
  - **Attack.** `certManager.issuerRef` points at a cluster-shared ClusterIssuer, which is the chart's default `kind`. Then any workload holding a client-auth cert from that issuer is accepted on :9443, and so is any namespace user allowed to create a cert-manager `Certificate` against it.
    - With CN equal to the backend's literal `gatewayID` (`tinycdi-backend`), or any other CN, it can call redeem, renew, target and activity, and it can revoke any lease by id.
    - With CN=`operator`, it can revoke or drain any workspace.
  - **Proof.** A cert with CN `some-other-workload` POSTs `/internal/v1/broker/leases/lease-abc/revoke` and gets 200 (`internal/broker/httpapi/server.go:148-169,435-441`).
  - **Required fix.** Make the gateway role opt-in: add `-internal-gateway-cns`, an allowlist that is empty by default, and refuse every CN that is neither the operator CN nor allowlisted with a 403 counted as `role_mismatch`. The chart renders an empty list. Document that `certManager.issuerRef` must be a dedicated issuer, and say so in TM-38's residual.
  - **Security test.** `TestUnknownCN_Refused` in `internal/broker/httpapi/server_test.go`. A CA-signed cert whose non-operator CN is not on the allowlist gets 403 on every lease and transfer route, and the broker is never called.
- **R1-33 (P3, TM-40): TM-40's "every refusal is counted" holds only for identify-layer refusals. Handshake and role refusals go uncounted, to an unstructured log with no rate limit.**
  - **Attack.** Someone probes :9443 with a foreign-CA, expired or wrong-EKU certificate. The handshake refuses the cert, but nothing is added to `tinycdi_internal_auth_failures_total` and nothing is logged as `internal_auth_rejected`. Go's plain `http: TLS handshake error from …` line goes to stderr with no rate limit. A gateway cert calling an operator route, or the reverse, gets a 403 that is not counted either.
  - **Required fix.**
    - Give the internal `http.Server` an `ErrorLog`. It counts `reason="handshake"` and emits a rate-limited structured `internal_auth_rejected` WARN carrying the peer and the reason class, never cert material.
    - Count `requireGateway`/`requireOperator` refusals as `reason="role_mismatch"`.
  - **Security tests.**
    - `TestIdentify_HandshakeRejectionsCounted` in `internal/backend/`, on the real internal listener. The counter increments for a foreign-CA cert, and the WARN appears at most N times per burst.
    - `TestRoleMismatchCounted` in `internal/broker/httpapi/server_test.go`.

**Design (planner's ruling).**
- **Roles at `identify`.**
  - The operator CN keeps its role.
  - A CN listed in `Config.GatewayCNs` is a gateway.
  - Any other verified CN is refused in `identify` with 403 `FORBIDDEN`, counted and logged through `rejectAuth` as `role_mismatch`, before any handler runs.
  - `requireGateway`/`requireOperator` refuse through `rejectAuth` with the same reason.
  - `-internal-gateway-cns` (env `TCDI_INTERNAL_GATEWAY_CNS`, CSV/repeatable, empty by default) feeds the list. `validate` refuses a list that names the operator CN. The chart does not render the flag: the session gateway runs in-process, and only the operator calls :9443 (plan ruling 7).
- **SPIFFE SAN stays optional: a deliberate deviation from the brief.** The brief asks to require the URI SAN for allowlisted CNs. The SAN is signed by the same issuer as the CN, so requiring it adds no protection against an issuer that will sign any CN, and that issuer is the R1-32 threat. The CN allowlist plus a dedicated issuer is the control, and a SAN that is present must still match the CN.
- **Handshake refusals.** The internal server's `ErrorLog` is a `*log.Logger` over a writer that parses Go's `http: TLS handshake error from <peer>: <cause>` line.
  - It counts `handshake` on `tinycdi_internal_auth_failures_total`.
  - It logs a rate-limited `internal_auth_rejected` WARN with `reason=handshake`, `peer`, and `class`, one of `unknown_ca`, `expired`, `bad_eku`, `revoked` or `other`. The class goes only in the log, which keeps the metric's label set to five values.
  - Every other server error line becomes a rate-limited `internal_server_error` WARN.
  - The metric HELP ("…this counter is the complete record") becomes true and stays byte-identical.

**Files:**
- Modify: `internal/broker/httpapi/server.go:79-101` (`Config.GatewayCNs`), `:108-129` (`NewHandler`, `server`), `:133-138` (reason const `authFailRole`), `:147-170` (`identify`), `:174-197` (`rejectAuth` comment), `:432-450` (`requireGateway`, `requireOperator`)
- Modify: `internal/observability/metrics.go:99-104` (`internalAuthFailReasons` + comment)
- Modify: `internal/backend/config.go:183-187` (field), `:348-351` (flag), `:406-410` (env fallback), `validate()` at `:421` (operator-CN conflict)
- Modify: `internal/backend/wire.go:524-526` (pass `GatewayCNs`), `bind` at `wire.go:959` (`internalSrv.ErrorLog`)
- Modify: `internal/backend/listeners.go` (new `internalErrorLog` + writer, after `internalServer`)
- Test: `internal/broker/httpapi/server_test.go` (`newEnv` default `GatewayCNs: {"gw-1"}`; new tests), `internal/backend/backend_test.go:580,677,837,1020` (fixture `GatewayCNs`), `internal/backend/mtls_reload_test.go` (`serveInternalTLSList` sets `ErrorLog`), `internal/backend/mtls_revocation_test.go` (new test), `internal/backend/config_test.go` (new flag test)
- Docs: `docs/adr/0003-gateway-broker-internal-api.md:30-37`, `docs/security/threat-model.md:521-525` (Boundary 7 first bullet) and the TM-38 Residual cell (`:717`), `docs/runbooks/observability.md:74`, `deploy/helm/tinycdi/values.yaml:530-533`, `deploy/helm/tinycdi/README.md:124`

**Interfaces:**
- Consumes: Task 22's `bind` edits. This task adds `internalSrv.ErrorLog` next to the `internalSrv.ConnState = internalConnState(...)` line Task 22 writes, and edits `serveInternalTLSList` after Task 22's edit to it.
- Produces:
  - `httpapi.Config.GatewayCNs []string`.
  - Backend `Config.InternalGatewayCNs stringList`, set by the flag `-internal-gateway-cns` / `TCDI_INTERNAL_GATEWAY_CNS`. Task 24's owned-flag list names it, because the chart never renders it.
  - Metric reasons `role_mismatch` and `handshake` in `internalAuthFailReasons`.
  - `internalErrorLog(log *slog.Logger, metrics *observability.Metrics) *stdlog.Logger` in `internal/backend/listeners.go`.
  - Task 30 adds `TestUnknownCN_Refused` and `TestRoleMismatchCounted` to TM-40's Pinned-by cell, and `TestIdentify_HandshakeRejectionsCounted` too. It also writes the new register row for R1-32.

- [ ] **Step 1: Write the failing tests**

In `internal/broker/httpapi/server_test.go`, `newEnv`: the default config gains `GatewayCNs: []string{"gw-1"},` after `Audience`. The tests issue gateway certs as `gw-1`. A tweak can still override the list.

Append to `internal/broker/httpapi/server_test.go`:

```go
// internalRoutes is every route on the internal listener, with a body
// the handlers would accept far enough to call the broker.
var internalRoutes = []struct{ method, path, body string }{
	{http.MethodPost, "/internal/v1/broker/redeem", `{"ticket":"x"}`},
	{http.MethodPost, "/internal/v1/broker/leases/lease-abc/renew", `{}`},
	{http.MethodGet, "/internal/v1/broker/leases/lease-abc/target", ``},
	{http.MethodPost, "/internal/v1/broker/leases/lease-abc/revoke", ``},
	{http.MethodPost, "/internal/v1/broker/leases/lease-abc/activity", `{}`},
	{http.MethodPost, "/internal/v1/broker/leases/lease-abc/transfers/t-1/claim", `{}`},
	{http.MethodPost, "/internal/v1/broker/leases/lease-abc/transfers/t-1/complete", `{}`},
	{http.MethodPost, "/internal/v1/broker/workspaces/ws-1/revoke", `{}`},
	{http.MethodGet, "/internal/v1/broker/workspaces/ws-1/drain", ``},
}

func callRoute(t *testing.T, c *http.Client, base, method, path, body string) int {
	t.Helper()
	req, err := http.NewRequest(method, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// brokerUntouched fails the test if any broker method recorded a call.
func brokerUntouched(t *testing.T, fb *fakeBroker) {
	t.Helper()
	if fb.gotGW != (broker.GatewayIdentity{}) || fb.gotTicket != "" || fb.gotLeaseID != "" ||
		fb.gotWsUID != "" || fb.gotTransferID != "" {
		t.Fatalf("broker was called: %+v", fb)
	}
}

// TestUnknownCN_Refused (R1-32): the gateway role is opt-in. A cert the
// internal CA signed whose CN is neither the operator CN nor on
// GatewayCNs gets 403 on every route — lease, transfer and workspace —
// the broker is never called, and each refusal is counted as
// role_mismatch.
func TestUnknownCN_Refused(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg, nil)
	e := newEnv(t, &fakeBroker{revokeChanged: true}, func(c *httpapi.Config) { c.Metrics = m })
	cert := e.pki.issue(t, "some-other-workload", nil, false)
	c := e.client(t, &cert)
	for _, rt := range internalRoutes {
		if got := callRoute(t, c, e.srv.URL, rt.method, rt.path, rt.body); got != http.StatusForbidden {
			t.Errorf("%s %s: status %d, want 403", rt.method, rt.path, got)
		}
	}
	brokerUntouched(t, e.fb)
	want := fmt.Sprintf(`# HELP tinycdi_internal_auth_failures_total mTLS identity rejections on the internal broker listener, by bounded reason class — the per-request warn line is rate-limited, this counter is the complete record.
# TYPE tinycdi_internal_auth_failures_total counter
tinycdi_internal_auth_failures_total{reason="role_mismatch"} %d
`, len(internalRoutes))
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want),
		"tinycdi_internal_auth_failures_total"); err != nil {
		t.Fatal(err)
	}
}

// TestRoleMismatchCounted (R1-33, TM-40): a listed gateway calling an
// operator route and the operator calling a gateway route are refused
// 403 without reaching the broker, and both refusals are counted as
// role_mismatch and logged as internal_auth_rejected.
func TestRoleMismatchCounted(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg, nil)
	e := newEnv(t, &fakeBroker{}, func(c *httpapi.Config) { c.Metrics = m })
	gw := e.pki.issue(t, "gw-1", nil, false)
	op := e.pki.issue(t, httpapi.DefaultOperatorCN, nil, false)
	if got := callRoute(t, e.client(t, &gw), e.srv.URL, http.MethodGet,
		"/internal/v1/broker/workspaces/ws-1/drain", ""); got != http.StatusForbidden {
		t.Errorf("gateway on operator route: status %d, want 403", got)
	}
	if got := callRoute(t, e.client(t, &op), e.srv.URL, http.MethodPost,
		"/internal/v1/broker/redeem", `{"ticket":"x"}`); got != http.StatusForbidden {
		t.Errorf("operator on gateway route: status %d, want 403", got)
	}
	brokerUntouched(t, e.fb)
	want := `# HELP tinycdi_internal_auth_failures_total mTLS identity rejections on the internal broker listener, by bounded reason class — the per-request warn line is rate-limited, this counter is the complete record.
# TYPE tinycdi_internal_auth_failures_total counter
tinycdi_internal_auth_failures_total{reason="role_mismatch"} 2
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want),
		"tinycdi_internal_auth_failures_total"); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(e.logs.String(), `"reason":"role_mismatch"`); n != 2 {
		t.Errorf("role_mismatch warn lines = %d, want 2: %s", n, e.logs.String())
	}
}
```
Add `"fmt"` to the file's imports. `observability` is already imported, because `TestIdentify_RejectionsCounted` uses it.

Append to `internal/backend/mtls_revocation_test.go` (add `"sync"` to its imports):

```go
// lockedBuffer is a bytes.Buffer safe for the server's log goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestIdentify_HandshakeRejectionsCounted (R1-33, TM-40): on the real
// internal listener, a client cert from a foreign CA fails the TLS
// handshake — and each refusal is counted on
// tinycdi_internal_auth_failures_total{reason="handshake"}, while the
// structured internal_auth_rejected warn line is rate-limited (burst 10)
// and names the failure class and peer, never certificate material.
func TestIdentify_HandshakeRejectionsCounted(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCANamed(t, "internal-ca")
	foreign := newTestCANamed(t, "foreign-ca")
	caFile := filepath.Join(dir, "clients-ca.pem")
	if err := writeFileAtomic(caFile, ca.pem); err != nil {
		t.Fatal(err)
	}
	pool, err := tlsreload.NewCAPool(caFile)
	if err != nil {
		t.Fatal(err)
	}
	cf, kf := writeTestCert(t, dir, "internal.test")
	srvCert, err := tls.LoadX509KeyPair(cf, kf)
	if err != nil {
		t.Fatal(err)
	}
	var logs lockedBuffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg, nil)
	addr := serveInternalTLSList(t, srvCert, ca.pem, pool, nil, log, metrics)

	const probes = 15
	probe := foreign.sign(t, "operator")
	for i := 0; i < probes; i++ {
		if callMTLS(addr, probe) {
			t.Fatal("foreign-CA cert accepted")
		}
	}
	want := map[string]string{"reason": "handshake"}
	deadline := time.Now().Add(5 * time.Second)
	for metricSum(t, reg, "tinycdi_internal_auth_failures_total", want) < probes && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := metricSum(t, reg, "tinycdi_internal_auth_failures_total", want); got != probes {
		t.Fatalf("handshake refusals counted = %v, want %d", got, probes)
	}
	out := logs.String()
	n := strings.Count(out, `"msg":"internal_auth_rejected"`)
	if n < 1 || n > 11 { // burst 10, plus at most one 1/s refill during the loop
		t.Errorf("internal_auth_rejected lines = %d, want 1..11 (rate-limited)", n)
	}
	for _, want := range []string{`"reason":"handshake"`, `"class":"unknown_ca"`, `"peer":"127.0.0.1:`} {
		if !strings.Contains(out, want) {
			t.Errorf("warn line missing %s: %s", want, out)
		}
	}
	if strings.Contains(out, "BEGIN CERTIFICATE") || strings.Contains(out, "foreign-ca") {
		t.Errorf("warn line carries certificate material: %s", out)
	}
}
```

Append to `internal/backend/config_test.go`:

```go
// TestParseFlags_InternalGatewayCNs (R1-32): the gateway role on the
// internal listener is opt-in — the list is empty by default, set by the
// flag (CSV/repeatable) or TCDI_INTERNAL_GATEWAY_CNS with the flag
// winning, and may not name the operator CN.
func TestParseFlags_InternalGatewayCNs(t *testing.T) {
	cfg, err := ParseFlags(mergedArgs(), noEnv)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if len(cfg.InternalGatewayCNs) != 0 {
		t.Fatalf("default InternalGatewayCNs = %v, want empty", cfg.InternalGatewayCNs)
	}
	cfg, err = ParseFlags(mergedArgs(), envMap(map[string]string{"TCDI_INTERNAL_GATEWAY_CNS": "gw-a, gw-b"}))
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if got := strings.Join(cfg.InternalGatewayCNs, ","); got != "gw-a,gw-b" {
		t.Fatalf("env InternalGatewayCNs = %q, want gw-a,gw-b", got)
	}
	cfg, err = ParseFlags(withArg(mergedArgs(), "-internal-gateway-cns", "gw-c"),
		envMap(map[string]string{"TCDI_INTERNAL_GATEWAY_CNS": "gw-a"}))
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if got := strings.Join(cfg.InternalGatewayCNs, ","); got != "gw-c" {
		t.Fatalf("flag InternalGatewayCNs = %q, want gw-c (flag wins over env)", got)
	}
	if _, err := ParseFlags(withArg(mergedArgs(), "-internal-gateway-cns", "gw-a,operator"), noEnv); err == nil {
		t.Fatal("-internal-gateway-cns naming the operator CN must fail")
	}
}
```
If `config_test.go` does not import `strings` yet, add it.

- [ ] **Step 2: Run the tests and watch them fail**

Run:
```bash
go test ./internal/broker/httpapi/ -run 'TestUnknownCN_Refused|TestRoleMismatchCounted' -count=1 2>&1 | tail -8
go test ./internal/backend/ -run 'TestIdentify_HandshakeRejectionsCounted|TestParseFlags_InternalGatewayCNs' -count=1 2>&1 | tail -8
```
Expected:
- httpapi fails to compile with `unknown field GatewayCNs in struct literal`. With the `newEnv` line commented out, it fails with `status 200, want 403` (the R1-32 gap) and `role_mismatch` missing.
- backend fails to compile with `cfg.InternalGatewayCNs undefined` (`config_test.go` references it). To see the R1-33 gap itself, temporarily comment out `TestParseFlags_InternalGatewayCNs` and rerun: it fails with `handshake refusals counted = 0, want 15`. Restore the test afterwards.

- [ ] **Step 3: Roles in the internal handler**

`internal/broker/httpapi/server.go`:
- `Config`: after `OperatorCN`, add:

```go
	// GatewayCNs are the client-certificate CNs that hold the gateway role
	// on the lease and transfer routes. The role is opt-in: a verified CN
	// that is neither OperatorCN nor listed here is refused 403
	// (role_mismatch) before any route runs. Empty means no gateway
	// identity exists — the chart's session gateway runs in process.
	GatewayCNs []string
```
- Reason consts: add `authFailRole = "role_mismatch"` after `authFailSpiffe`.
- Replace `type server struct{ cfg Config }` with:

```go
type server struct {
	cfg        Config
	gatewayCNs map[string]struct{}
}
```
  and in `NewHandler` replace `s := &server{cfg: cfg}` with:

```go
	s := &server{cfg: cfg, gatewayCNs: make(map[string]struct{}, len(cfg.GatewayCNs))}
	for _, cn := range cfg.GatewayCNs {
		s.gatewayCNs[cn] = struct{}{}
	}
```
- `identify`: after the SPIFFE loop, before `gw := broker.GatewayIdentity{…}`, add:

```go
		if _, gateway := s.gatewayCNs[id]; !gateway && id != s.cfg.OperatorCN {
			s.rejectAuth(w, r, http.StatusForbidden, api.CodeForbidden,
				"client certificate CN holds no role on this listener", authFailRole)
			return
		}
```
  Its doc comment becomes: "identify maps the verified peer certificate onto broker.GatewayIdentity. Roles are opt-in: the CN must be the operator CN or one of GatewayCNs; any other CN is refused 403 role_mismatch."
- `rejectAuth` doc comment: replace its first two sentences with "rejectAuth answers an identify-layer or role refusal. Identify refusals never reach logRequests (identify wraps it), so every refusal is recorded here:".
- `requireGateway` and `requireOperator`: replace each `writeErr(w, r, http.StatusForbidden, api.CodeForbidden, "<msg>")` with `s.rejectAuth(w, r, http.StatusForbidden, api.CodeForbidden, "<msg>", authFailRole)`, keeping both messages unchanged.

`internal/observability/metrics.go:99-104`:

```go
	// internalAuthFailReasons are the refusal classes the internal broker
	// listener can emit: no peer cert presented, a verified cert without a
	// CN, a SPIFFE URI SAN disagreeing with the CN, a CN that holds no role
	// on the route (role_mismatch), or a refused TLS handshake (foreign or
	// expired CA, wrong EKU, revoked serial — the class is in the log).
	internalAuthFailReasons = map[string]struct{}{
		"no_cert": {}, "no_cn": {}, "spiffe_mismatch": {}, "role_mismatch": {}, "handshake": {},
	}
```

Fixtures. In `internal/backend/backend_test.go`, add `GatewayCNs: []string{"gw-test"},` to the `httpapi.Config` at `:580`, and `GatewayCNs: []string{"gw-split"},` to those at `:677`, `:837` and `:1020`. The CNs their clients present are `ca.sign(t, "gw-test")` at `:589` and `writeTestCert(t, dir, "gw-split")`.

Run: `go test ./internal/broker/httpapi/... -count=1 2>&1 | tail -5`. Expected: `ok` for `httpapi` and `httpapi/opclient` (the opclient tests present only the operator CN).

- [ ] **Step 4: The backend flag**

`internal/backend/config.go`:
- After `OperatorCN string` (line 184), add:

```go
	// InternalGatewayCNs are the client-certificate CNs holding the
	// gateway role on the internal listener (opt-in; empty = none — the
	// chart's gateway is in process). Never the operator CN.
	InternalGatewayCNs stringList
```
- After the `-operator-cn` flag (line 349), add:

```go
	fs.Var(&c.InternalGatewayCNs, "internal-gateway-cns",
		"client-certificate CNs holding the gateway role on the internal listener (repeatable or comma-separated; empty: no gateway identity)")
```
- After the `TCDI_LOGIN_KEY_FILE` fallback block (lines 406-410), add:

```go
	if !set["internal-gateway-cns"] {
		if v := envOr(getenv, "TCDI_INTERNAL_GATEWAY_CNS", ""); v != "" {
			_ = c.InternalGatewayCNs.Set(v)
		}
	}
```
- In `validate()`, after the tenant-admin-groups check, add:

```go
	for _, cn := range c.InternalGatewayCNs {
		if cn == c.OperatorCN {
			return fmt.Errorf("-internal-gateway-cns must not name the operator CN %q", cn)
		}
	}
```

`internal/backend/wire.go:524-526`: the `httpapi.Config` literal gains `GatewayCNs: cfg.InternalGatewayCNs,`.

- [ ] **Step 5: Handshake refusals reach the counter and a structured log**

`internal/backend/listeners.go`: add `stdlog "log"` and `"strings"` to the imports. After `internalServer`, add:

```go
const (
	// internalErrLogPerMinute/internalErrLogBurst bound the internal
	// server's error-log warn lines per class (handshake, server_error) —
	// the same budget shape as rejectAuth.
	internalErrLogPerMinute = 60
	internalErrLogBurst     = 10
	// tlsHandshakeErrPrefix is how net/http reports a failed handshake
	// through Server.ErrorLog.
	tlsHandshakeErrPrefix = "http: TLS handshake error from "
)

// internalErrorLog is the internal listener's http.Server.ErrorLog
// (TM-40). Go reports a refused handshake — a foreign or expired CA, a
// wrong EKU, a revoked serial — only through this logger, so each such
// line is counted on tinycdi_internal_auth_failures_total{reason=
// "handshake"} and logged as a rate-limited internal_auth_rejected warning
// with the peer and failure class; certificate material is never logged.
// Every other server error line becomes a rate-limited
// internal_server_error warning. log or metrics may be nil.
func internalErrorLog(log *slog.Logger, metrics *observability.Metrics) *stdlog.Logger {
	return stdlog.New(&internalErrorWriter{
		log:     log,
		metrics: metrics,
		limit:   ratelimit.New(internalErrLogPerMinute, internalErrLogBurst, 8, nil),
	}, "", 0)
}

type internalErrorWriter struct {
	log     *slog.Logger
	metrics *observability.Metrics
	limit   ratelimit.Allower
}

func (w *internalErrorWriter) Write(p []byte) (int, error) {
	line := strings.TrimSuffix(string(p), "\n")
	if rest, ok := strings.CutPrefix(line, tlsHandshakeErrPrefix); ok {
		peer, cause, _ := strings.Cut(rest, ": ")
		if w.metrics != nil {
			w.metrics.IncInternalAuthFailure("handshake")
		}
		if w.log != nil {
			if ok, _ := w.limit.Allow("handshake"); ok {
				w.log.Warn("internal_auth_rejected",
					"reason", "handshake",
					"class", handshakeClass(cause),
					"peer", peer)
			}
		}
		return len(p), nil
	}
	if w.log != nil {
		if ok, _ := w.limit.Allow("server_error"); ok {
			w.log.Warn("internal_server_error", "err", line)
		}
	}
	return len(p), nil
}

// handshakeClass buckets a handshake failure cause into a bounded class
// for the warn line.
func handshakeClass(cause string) string {
	switch {
	case strings.Contains(cause, "unknown authority"), strings.Contains(cause, errClientCARotated.Error()):
		return "unknown_ca"
	case strings.Contains(cause, "expired or is not yet valid"):
		return "expired"
	case strings.Contains(cause, "incompatible key usage"):
		return "bad_eku"
	case strings.Contains(cause, tlsreload.ErrCertRevoked.Error()):
		return "revoked"
	default:
		return "other"
	}
}
```
`errClientCARotated` comes from Task 22.

`internal/backend/wire.go`, `bind`: directly after Task 22's `internalSrv.ConnState = internalConnState(b.internalConns, b.internalBlocklist)`, add:

```go
	// TM-40: refused handshakes are counted and logged structurally.
	internalSrv.ErrorLog = internalErrorLog(b.log, b.metrics)
```

`internal/backend/mtls_reload_test.go`, `serveInternalTLSList`: directly after the `srv.ConnState = …` line, add:

```go
	if log != nil || metrics != nil {
		srv.ErrorLog = internalErrorLog(log, metrics)
	}
```
and extend its doc comment with "…and, when log or metrics is non-nil, the TM-40 handshake ErrorLog".

Run:
```bash
go build ./... && go vet ./internal/backend/ ./internal/broker/... ./internal/observability/
go test ./internal/backend/ -run 'TestIdentify_|TestParseFlags_|TestRouteIsolation|TestRevoked|TestInternalListener_|Split' -count=1 2>&1 | tail -5
go test ./internal/observability/ ./internal/broker/httpapi/... -count=1 2>&1 | tail -4
go test -race ./internal/backend/ -run TestIdentify_HandshakeRejectionsCounted -count=3 2>&1 | tail -3
```
Expected: `ok` for each, with no race report.

- [ ] **Step 6: Docs the fix makes true or false**

- `docs/adr/0003-gateway-broker-internal-api.md:30-37`: replace the two identity bullets with:

  ```
  - The client identity is the verified leaf certificate's `Subject CN`. An
    optional SPIFFE URI SAN, if present, must equal the CN or the request is
    rejected 403 (`identify` in `internal/broker/httpapi/server.go`). The
    identity maps to `broker.GatewayIdentity{ID: CN, Audience: <configured
    audience>}`.
  - Roles are opt-in by CN. The operator identity is the CN configured by
    `-operator-cn` (default `operator`). A gateway identity is a CN listed
    in `-internal-gateway-cns` (`TCDI_INTERNAL_GATEWAY_CNS`, empty by
    default: the chart's session gateway runs in process, so it renders no
    gateway CN). Any other CN the internal CA signed is refused 403 before
    a route runs, and a gateway on an operator route or the operator on a
    gateway route is refused 403; each refusal is counted as
    `role_mismatch`. Per-certificate revocation is ADR 0008.
  ```
- `docs/security/threat-model.md`, Boundary 7 first `Controls:` bullet: replace "`identify` asserts a peer certificate on every route and maps the client identity to a role (`requireGateway`, `requireOperator`); every refusal is counted and logged (TM-40)." with "`identify` asserts a peer certificate on every route and admits only the operator CN and the CNs listed in `-internal-gateway-cns` (empty by default); `requireGateway` and `requireOperator` keep each role on its routes. Every refusal — a refused handshake (through the server's `ErrorLog`), a missing or role-less identity, a role mismatch — is counted on `tinycdi_internal_auth_failures_total` and logged rate-limited (TM-40)."
- TM-38 row, Residual cell: append " The internal client CA must be a dedicated issuer that signs nothing else (the chart's `selfSigned` bootstrap, or an `Issuer` in the release namespace): any certificate it signs with the operator CN is the operator."
- `docs/runbooks/observability.md:74`: replace "(`reason` is `no_cert`, `no_cn` or `spiffe_mismatch`)" with "(`reason` is `no_cert`, `no_cn`, `spiffe_mismatch`, `role_mismatch` — a CN with no role on the route — or `handshake` — a refused TLS handshake, whose warn line names the class: `unknown_ca`, `expired`, `bad_eku`, `revoked` or `other`)".
- `deploy/helm/tinycdi/values.yaml:530`: replace `# Signer for the leaf Certificates. Ignored when selfSigned=true.` with:

  ```yaml
    # Signer for the leaf Certificates. Ignored when selfSigned=true. It also
    # signs the internal client CA, so it must be a dedicated issuer that
    # signs nothing else (or kind: Issuer in the release namespace): any
    # certificate it issues with the operator CN can drain workspaces.
  ```
- `deploy/helm/tinycdi/README.md:124`: change the description cell to "existing signer; required unless `selfSigned`. Use a dedicated issuer that signs nothing else (or `kind: Issuer` in the release namespace): it is the internal client CA".

The values change is a comment, so the default render is unchanged. Check it:

```bash
cd deploy/helm && go test -count=1 -run 'TestDefaultRender|TestValues|TestREADME' . 2>&1 | tail -3; cd ../..
for t in .github/tests/*.test.sh; do bash "$t" || exit 1; done
```
Expected: `ok`, then exit 0.

- [ ] **Step 7: Commit**

```bash
git add internal/broker/httpapi/server.go internal/broker/httpapi/server_test.go internal/observability/metrics.go \
  internal/backend/config.go internal/backend/config_test.go internal/backend/wire.go internal/backend/listeners.go \
  internal/backend/backend_test.go internal/backend/mtls_reload_test.go internal/backend/mtls_revocation_test.go \
  docs/adr/0003-gateway-broker-internal-api.md docs/security/threat-model.md docs/runbooks/observability.md \
  deploy/helm/tinycdi/values.yaml deploy/helm/tinycdi/README.md
git commit -m "fix(mtls): gateway role is opt-in by CN; count handshake and role refusals

R1-32: -internal-gateway-cns (empty by default) lists the gateway CNs;
any other non-operator CN is refused 403 role_mismatch at identify.
R1-33: the internal server's ErrorLog counts refused handshakes as
reason=handshake with a rate-limited structured warning; role refusals
in requireGateway/requireOperator are counted as role_mismatch.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 24: extraArgs/extraEnv cannot override rendered flags (R1-34) — tier: design

**Finding.**
- **R1-34 (P3, TM-42): the extraArgs and extraEnv deny lists are incomplete and asymmetric, so TM-42's guards can be bypassed. This covers unelected operator replicas, the split-mode broker, trusted proxies, the operator CN and the kasm adapter image.**
- **Attack (platform-operator misconfiguration, the actor TM-42 guards against).**
  - (a) `operator.replicas: 2` + `operator.extraArgs: ["--leader-elect=false"]` renders. Go's flag package keeps the last value, so two unelected reconcilers race on the same Workspaces.
  - (b) `-broker-url/-broker-ca/-mtls-cert/-mtls-key` are refused in `backend.extraArgs`, but `TCDI_BROKER_URL/…/TCDI_MTLS_KEY` in `backend.extraEnv` render.
  - (c) `TCDI_OPERATOR_CN`, `TCDI_TRUSTED_PROXIES`, `TCDI_CONTROL_TOKEN_FILE`, `TCDI_KASM_ADAPTER_IMAGE` and `TCDI_WATCH_NAMESPACES` are refused as env, but their flag forms render through extraArgs. extraArgs renders after the chart's own flag, so it wins.
  - Code: `deploy/helm/tinycdi/templates/_helpers.tpl:253,403-404,416`.
- **Required fix.**
  - Derive the denied extraArgs names from the flags the chart renders for each component, the way `tinycdi.managedEnvNames` derives env names. Allow-list only the documented knobs (`-rate-limit-replicas`, `-login-key-file`).
  - Make the two maps symmetric. Add `TCDI_BROKER_URL`, `TCDI_BROKER_CA`, `TCDI_MTLS_CERT`, `TCDI_MTLS_KEY`, `SSL_CERT_FILE` and `SSL_CERT_DIR` to `$deniedEnv`.
  - The leader guard also scans `operator.extraArgs` for `leader-elect`.
- **Security test.** `TestExtraArgsCannotOverrideRenderedFlags` in `deploy/helm/chart_extraenv_test.go`:
  - Every rendered flag in `<component>.extraArgs` fails without `dev.enabled`.
  - Every `$deniedEnv` name's flag form, and every denied flag's env form, fails too.
  - `TestOperatorLeaderElectionGuard` gains the `extraArgs=[--leader-elect=false]`, replicas=2 case.

**Design (planner's ruling).**
- **The brief's derivation needs a refactor at HEAD.** `managedEnvNames` can read env names back because each component's env is a named helper (`tinycdi.env.<component>`). The args are inline in `backend.yaml:136-255`, `operator.yaml:51-123` and `frontend.yaml:62-81`. This task moves each args block verbatim into a helper `tinycdi.args.<component>` in a new `templates/_args.tpl`. The Deployment templates include it, so the default render stays byte-identical. This was checked during planning against `testdata/default-render.golden.yaml`.
- **Deny sets.** The deny set of `<component>.extraArgs` is the union of:
  - the flag names the current render emits (`tinycdi.renderedFlagNames`, read back from the helper);
  - the flags the chart owns but renders only on some values, or never (`tinycdi.ownedFlags`, a static list per component);
  - the flag form of every `TCDI_` name in the static env deny map;

  minus `tinycdi.flagOverrides` (backend `rate-limit-replicas`, `login-key-file`).

  The env deny map is the static map (old `$deniedEnv` plus the six new names) merged over `TCDI_<UPPER_SNAKE>` of every component's denied flag. The two maps are therefore symmetric by construction.
- **Drift guard.** A test scans `_args.tpl` itself: any flag the helper can emit must be denied, and `ownedFlags` must cover each one that the minimal render does not emit. A new conditional flag that is missing from `ownedFlags` fails CI.
- **Leader election.** `--leader-elect` in `operator.extraArgs` with `operator.replicas > 1` fails in the E4 guard, outside the dev gate. Without this, `dev.enabled=true` would reopen unelected replicas.
- **Out of scope.** Unrendered tuning flags (`-login-rate`, `-launch-rate`, `-expiry-interval`, `-image-stale-after`) stay settable through extraArgs and their env forms. The docs that tell operators to use them (`install.md:371`, `capacity.md:280`, chart README "Rate limits") stay true.

**Files:**
- Create: `deploy/helm/tinycdi/templates/_args.tpl` (generated by the step-3 script)
- Modify: `deploy/helm/tinycdi/templates/backend.yaml:136-255`, `operator.yaml:51-123`, `frontend.yaml:62-81` (args body becomes one `include`)
- Modify: `deploy/helm/tinycdi/templates/_helpers.tpl:249-255` (E4 guard), `:398-436` (CHTR-3/SR-3-F2 block), after `:494` (new helpers)
- Modify: `deploy/helm/tinycdi/README.md:160` (extraArgs row), `:168-182` (deny table), `deploy/helm/tinycdi/values.yaml:893-905` (dev-gate comment)
- Test: `deploy/helm/chart_extraenv_test.go` (`deniedEnvNames`, new test), `deploy/helm/chart_guards_test.go:22` (`TestOperatorLeaderElectionGuard`)

**Interfaces:**
- Consumes:
  - Task 3's `-tenant-quota-ceilings` and Task 23's `-internal-gateway-cns`, both named in `ownedFlags` (backend).
  - Task 3 adds its flag to the backend args block before this task moves the block. Whatever the block holds at the time is moved verbatim. If Task 3 renders the flag only on some values, the drift test requires it in `ownedFlags`, where it already is.
- Produces:
  - Helpers `tinycdi.args.{backend,operator,frontend}`, `tinycdi.renderedFlagNames`, `tinycdi.ownedFlags`, `tinycdi.flagOverrides`, `tinycdi.staticDeniedEnv`, `tinycdi.deniedFlagNames`, `tinycdi.deniedEnvMap`.
  - Later tasks that add a chart-rendered flag must add it inside `_args.tpl`. Task 25 adds no flag.
  - Task 30 updates the TM-42 Control and Pinned-by cells: it adds `TestExtraArgsCannotOverrideRenderedFlags` and notes the extraArgs derivation.

- [ ] **Step 1: Write the failing tests**

In `deploy/helm/chart_extraenv_test.go`, extend `deniedEnvNames` after `"TCDI_TENANT_QUOTAS",`:

```go
	"TCDI_BROKER_URL",
	"TCDI_BROKER_CA",
	"TCDI_MTLS_CERT",
	"TCDI_MTLS_KEY",
	"SSL_CERT_FILE",
	"SSL_CERT_DIR",
```
and change its doc comment's first line to "deniedEnvNames mirrors tinycdi.staticDeniedEnv in templates/_helpers.tpl —". Add `"fmt"`, `"os"`, `"regexp"` and `"sort"` to the imports, then append:

```go
// documentedFlagOverrides are the extraArgs knobs that may repeat a
// chart-rendered backend flag (capacity.md, install.md).
var documentedFlagOverrides = map[string]bool{"rate-limit-replicas": true, "login-key-file": true}

// chartArgFlags scans templates/_args.tpl and returns, per component,
// every flag name tinycdi.args.<component> can emit — rendered by
// default or only on some values.
func chartArgFlags(t *testing.T) map[string][]string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("tinycdi", "templates", "_args.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	def := regexp.MustCompile(`define "tinycdi\.args\.([a-z]+)"`)
	flag := regexp.MustCompile(`^\s*- (?:\{\{ printf ")?-{1,2}([a-z][a-z0-9-]*)`)
	out := map[string][]string{}
	seen := map[string]bool{}
	cur := ""
	for _, line := range strings.Split(string(src), "\n") {
		if m := def.FindStringSubmatch(line); m != nil {
			cur = m[1]
			continue
		}
		if m := flag.FindStringSubmatch(line); m != nil && cur != "" && !seen[cur+"/"+m[1]] {
			seen[cur+"/"+m[1]] = true
			out[cur] = append(out[cur], m[1])
		}
	}
	if len(out["backend"]) < 30 || len(out["operator"]) < 10 || len(out["frontend"]) < 5 {
		t.Fatalf("_args.tpl scan found too few flags: %v", out)
	}
	return out
}

// argFlagName strips an args entry to its flag name.
func argFlagName(a string) string {
	n := strings.TrimLeft(a, "-")
	if i := strings.IndexAny(n, "= \t"); i >= 0 {
		n = n[:i]
	}
	return n
}

// flagEnvName is the env form a TinyCDI binary reads a flag from.
func flagEnvName(flag string) string {
	return "TCDI_" + strings.ToUpper(strings.ReplaceAll(flag, "-", "_"))
}

// TestExtraArgsCannotOverrideRenderedFlags (R1-34, TM-42): extraArgs
// renders after the chart's own flags and the last value of a flag wins,
// so without dev.enabled no <component>.extraArgs entry may name a flag
// the chart can render for that component (except the documented
// backend knobs), nor the flag form of a denied extraEnv name; and the
// deny lists are symmetric — every such flag's TCDI_ env form fails in
// extraEnv. Every arg the minimal render emits must come from
// tinycdi.args.<component>, so the scan sees it.
func TestExtraArgsCannotOverrideRenderedFlags(t *testing.T) {
	flags := chartArgFlags(t)
	docs := renderArgs(t, "-f", minimalValues)
	for c, names := range flags {
		inSrc := map[string]bool{}
		for _, n := range names {
			inSrc[n] = true
		}
		for _, a := range firstContainerArgs(deployment(docs, c)) {
			if !inSrc[argFlagName(a)] {
				t.Errorf("%s renders %q outside tinycdi.args.%s — the extraArgs guard cannot see it", c, a, c)
			}
		}
	}

	type probe struct {
		label, want string
		sets        []string
	}
	var probes []probe
	for c, names := range flags {
		for _, n := range names {
			if c == "backend" && documentedFlagOverrides[n] {
				continue
			}
			arg := "--" + n + "=x"
			probes = append(probes, probe{
				label: c + "/flag/" + n,
				want:  fmt.Sprintf("%s.extraArgs entry %q", c, arg),
				sets:  []string{"--set", c + ".extraArgs[0]=" + arg},
			})
			env := flagEnvName(n)
			probes = append(probes, probe{
				label: c + "/env/" + env,
				want:  fmt.Sprintf("%s.extraEnv entry %q", c, env),
				sets:  []string{"--set", c + ".extraEnv[0].name=" + env, "--set", c + ".extraEnv[0].value=x"},
			})
		}
	}
	for _, env := range deniedEnvNames {
		if !strings.HasPrefix(env, "TCDI_") {
			continue
		}
		arg := "--" + strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(env, "TCDI_"), "_", "-")) + "=x"
		for _, c := range []string{"backend", "operator", "frontend"} {
			probes = append(probes, probe{
				label: c + "/flagform/" + env,
				want:  fmt.Sprintf("%s.extraArgs entry %q", c, arg),
				sets:  []string{"--set", c + ".extraArgs[0]=" + arg},
			})
		}
	}
	sort.Slice(probes, func(i, j int) bool { return probes[i].label < probes[j].label })
	t.Run("denied", func(t *testing.T) {
		for _, p := range probes {
			t.Run(p.label, func(t *testing.T) {
				t.Parallel()
				out := renderErrArgs(t, append([]string{"-f", minimalValues}, p.sets...)...)
				if !strings.Contains(out, p.want) {
					t.Errorf("render must fail naming %s, got: %s", p.want, out)
				}
			})
		}
	})

	// The documented knobs and unrendered tuning flags still render, and
	// dev.enabled reopens a rendered flag.
	docs = renderArgs(t, "-f", minimalValues,
		"--set", "backend.extraArgs[0]=-rate-limit-replicas=4",
		"--set", "backend.extraArgs[1]=-login-key-file=/etc/login-keys/previous",
		"--set", "backend.extraArgs[2]=-login-rate=0",
		"--set", "backend.extraArgs[3]=--expiry-interval=45s")
	args := strings.Join(firstContainerArgs(deployment(docs, "backend")), " ")
	for _, want := range []string{"-rate-limit-replicas=4", "-login-key-file=/etc/login-keys/previous", "-login-rate=0"} {
		if !strings.Contains(args, want) {
			t.Errorf("documented extraArgs knob %s missing: %s", want, args)
		}
	}
	renderArgs(t, "-f", minimalValues, "--set", "dev.enabled=true", "--set", "backend.extraArgs[0]=-session-idle=1h")
}
```

In `deploy/helm/chart_guards_test.go`, `TestOperatorLeaderElectionGuard`: after the `for _, replicas := range []string{"2", "3"}` loop, insert:

```go
	// The flag form cannot bypass the guard (R1-34): extraArgs renders
	// after the chart's --leader-elect=true and the last value wins — and
	// dev.enabled does not reopen it.
	for _, arg := range []string{"--leader-elect=false", "-leader-elect=false", "--leader-elect false"} {
		out := renderErrArgs(t,
			"-f", minimalValues,
			"--set", "dev.enabled=true",
			"--set", "operator.extraArgs[0]="+arg)
		if !strings.Contains(out, "operator.leaderElect") || !strings.Contains(out, "operator.extraArgs") {
			t.Errorf("replicas=2 + extraArgs %q must fail naming operator.leaderElect, got: %s", arg, out)
		}
	}
```

- [ ] **Step 2: Run the tests and watch them fail**

Run:
```bash
cd deploy/helm
go test -count=1 -run 'TestExtraArgsCannotOverrideRenderedFlags|TestOperatorLeaderElectionGuard|TestExtraEnvDevSurfaceNamesDenied' . > /tmp/t24-red.log 2>&1; echo "rc=$?"
grep -E '^\s+--- FAIL|_args.tpl|render succeeded' /tmp/t24-red.log | head -20
cd ../..
```
Expected: `rc=1`, with these failures:
- `TestExtraArgsCannotOverrideRenderedFlags` fails at once with `open tinycdi/templates/_args.tpl: no such file or directory`.
- `TestOperatorLeaderElectionGuard` fails with `helm template […]: expected failure, render succeeded` at the `--leader-elect=false` case (R1-34 (a)).
- `TestExtraEnvDevSurfaceNamesDenied` fails with `expected failure, render succeeded` at the first new name, `TCDI_BROKER_URL` on backend (`renderErrArgs` calls `t.Fatalf`, so it stops there) (R1-34 (b)).

- [ ] **Step 3: Move the args into helpers**

Save this script outside the repo (for example `$TMPDIR/move-args.py`) and run it once:

```python
#!/usr/bin/env python3
"""Move each component's chart-rendered args into tinycdi.args.<component>
(templates/_args.tpl) so the extraArgs guard can derive the flag names."""
import pathlib, sys

T = pathlib.Path(sys.argv[1])  # deploy/helm/tinycdi/templates
PRELUDE = {
    "backend": [
        '{{- $revokedSerials := ((.Values.backend.tls.internal | default dict).revokedSerials) | default dict -}}',
        '{{- $loginSecret := .Values.backend.loginKeys.existingSecret | default (printf "%s-backend-login-keys" .Release.Name) -}}',
        '{{- $loginPrevious := false -}}',
        '{{- with lookup "v1" "Secret" $.Release.Namespace $loginSecret -}}',
        '{{- if hasKey (default dict .data) "previous" -}}',
        '{{- $loginPrevious = true -}}',
        '{{- end -}}',
        '{{- end -}}',
    ],
    "operator": [],
    "frontend": [
        '{{- $brandingCM := get (default dict .Values.frontend.branding) "configMap" -}}',
    ],
}
out = ['{{/*',
       'Chart-rendered container args, one helper per component (TM-42).',
       'The Deployment templates include these, and tinycdi.renderedFlagNames',
       'reads the flag names back so <component>.extraArgs can never repeat',
       'one: extraArgs renders after these and the last value of a flag wins.',
       'Each helper renders the arg ENTRIES (a YAML list), never the `args:` key.',
       'Every chart-rendered flag belongs here (TestExtraArgsCannotOverrideRenderedFlags).',
       '*/}}']
for comp in ("backend", "operator", "frontend"):
    f = T / f"{comp}.yaml"
    lines = f.read_text().split("\n")
    start = lines.index("          args:")
    end = lines.index("            {{- with .Values.%s.extraArgs }}" % comp)
    body = lines[start + 1:end]
    for l in body:
        if l.strip() and not l.startswith(" " * 12):
            sys.exit(f"{comp}: unexpected indentation: {l!r}")
    out.append("")
    out.append('{{- define "tinycdi.args.%s" -}}' % comp)
    out.extend(PRELUDE[comp])
    out.extend(l[12:] for l in body)
    out.append("{{- end }}")
    lines[start + 1:end] = ['            {{- include "tinycdi.args.%s" . | nindent 12 }}' % comp]
    f.write_text("\n".join(lines))
(T / "_args.tpl").write_text("\n".join(out) + "\n")
```

```bash
python3 "$TMPDIR/move-args.py" deploy/helm/tinycdi/templates
git diff --stat deploy/helm/tinycdi/templates
```
The `PRELUDE` lines repeat the variable definitions that the args reference from the top of `backend.yaml:25-31` and `frontend.yaml:5`, because a `define` cannot see the caller's variables. Before you run the script, check that they match those lines at HEAD, except that every `PRELUDE` line must end in ` -}}` (HEAD's `frontend.yaml:5` ends in ` }}`; copying that would emit a newline inside the `define` and break the golden). If an earlier task changed them, copy the current text into `PRELUDE` with the same ` -}}` ending. The originals stay in the Deployment templates, which still use them.

Run:
```bash
cd deploy/helm && go test -count=1 -run 'TestDefaultRenderUnchanged' . 2>&1 | tail -3; cd ../..
```
Expected: `ok`. The move does not change the default render. If the golden differs, the cause is a `PRELUDE` mismatch. Do not regenerate the golden for this step.

- [ ] **Step 4: Derived deny lists**

`deploy/helm/tinycdi/templates/_helpers.tpl`. In the E4 block (lines 249-255), extend the comment's last sentence with " --leader-elect in operator.extraArgs is refused with replicas > 1 even under dev.enabled: it renders after the chart's flag and wins." After its `{{- end -}}`, add:

```
{{- if gt (int .Values.operator.replicas) 1 -}}
{{- range $a := (.Values.operator.extraArgs | default (list)) -}}
{{- if regexMatch "^--?leader-elect([=\\s]|$)" (printf "%v" $a) -}}
{{- fail (printf "operator.extraArgs entry %q is refused when operator.replicas > 1 — set operator.leaderElect instead; unelected replicas would double-drive reconciliation" $a) -}}
{{- end -}}
{{- end -}}
{{- end -}}
```

Replace lines 398-436 with the following. They run from `{{- /* CHTR-3: extraArgs must not reopen …` through the `{{- end -}}` that closes the `range $c` extraEnv loop, just above `{{- /* A hostPath extraVolume …`.

```
{{- /* CHTR-3/SR-3-F2/TM-42: extraArgs renders after the chart's own flags
        and the last value of a flag wins, so no extraArgs entry may name a
        flag the chart renders for that component (tinycdi.renderedFlagNames,
        read back from tinycdi.args.<component>) or owns but renders only on
        some values (tinycdi.ownedFlags). extraEnv renders after the
        managed env block, so no entry may name a managed env var, a
        dev-surface or security env name (tinycdi.staticDeniedEnv), or the
        env form TCDI_<UPPER_SNAKE> of any denied flag. The flag and env
        deny lists are symmetric: every denied flag denies its env name and
        every denied TCDI_ env name denies its flag. */ -}}
{{- $deniedEnv := include "tinycdi.deniedEnvMap" (dict "root" $) | fromJson -}}
{{- range $c := list "backend" "operator" "frontend" -}}
{{- $cv := default dict (index $.Values $c) -}}
{{- include "tinycdi.denyExtraArgs" (dict "component" $c "args" (index $cv "extraArgs") "denied" (include "tinycdi.deniedFlagNames" (dict "component" $c "root" $) | fromJsonArray)) -}}
{{- include "tinycdi.denyExtraEnv" (dict "component" $c "env" (index $cv "extraEnv") "denied" $deniedEnv "root" $) -}}
{{- end -}}
```

After the `tinycdi.denyExtraArgs` define (ends at line 494), add:

```

{{/*
TM-42: every flag name the chart renders for a component — DERIVED by
rendering tinycdi.args.<component> and reading the names back (the same
shape as tinycdi.managedEnvNames; never a hand-maintained copy). Called
with {component, root}; renders a JSON array of names without dashes.
*/}}
{{- define "tinycdi.renderedFlagNames" -}}
{{- $names := list -}}
{{- range $a := (include (printf "tinycdi.args.%s" .component) .root | fromYamlArray) -}}
{{- $n := regexReplaceAll "^-+" (printf "%v" $a) "" -}}
{{- $n = regexReplaceAll "[=\\s].*$" $n "" -}}
{{- $names = append $names $n -}}
{{- end -}}
{{- toJson ($names | uniq) -}}
{{- end }}

{{/*
TM-42: flags the chart owns for a component but renders only on some
values (or, for the split-mode broker client and the dev surfaces, never).
An extraArgs entry naming one would switch on a surface the chart keeps
off or replace a chart-validated value. Every flag tinycdi.args.<component>
emits only conditionally must be listed here
(TestExtraArgsCannotOverrideRenderedFlags). Called with {component}.
*/}}
{{- define "tinycdi.ownedFlags" -}}
{{- $owned := dict
  "backend" (list "dev-insecure-db" "required-groups" "tenant-admin-groups" "oidc-post-logout-redirect" "tenant-quotas" "tenant-quota-ceilings" "upstream-ca" "control-token-file" "internal-client-ca" "internal-revoked-list" "internal-gateway-cns" "operator-cn" "metrics-listen" "trusted-proxies" "broker-url" "broker-ca" "mtls-cert" "mtls-key")
  "operator" (list "dev-allow-no-broker" "disable-builtin-egress-excepts" "metrics-bind-address" "metrics-secure" "metrics-cert-path" "metrics-cert-name" "metrics-cert-key" "internet-except-cidrs" "broker-internal-url" "broker-ca-file" "broker-client-cert-file" "broker-client-key-file" "kasm-adapter-image" "watch-namespaces" "gateway-namespace" "runtime-node-selector" "runtime-tolerations" "runtime-host-users" "runtime-apparmor-require-default" "runtime-topology-spread" "runtime-clusteronly-egress")
  "frontend" (list "web-root" "branding-dir")
-}}
{{- toJson (index $owned .component) -}}
{{- end }}

{{/*
TM-42: the documented extraArgs knobs that may repeat a rendered flag —
-rate-limit-replicas (an HPA's maxReplicas; capacity.md) and
-login-key-file (the rotation's "previous" key under helm template /
GitOps; install.md). Called with {component}.
*/}}
{{- define "tinycdi.flagOverrides" -}}
{{- toJson (index (dict "backend" (list "rate-limit-replicas" "login-key-file") "operator" (list) "frontend" (list)) .component) -}}
{{- end }}

{{/*
SR-3-F2/TM-42: env names refused in every component's extraEnv, mapped to
the chart value to set instead: dev-surface names, security env names the
chart wires as first-class values, the split-mode broker client, and the
outbound trust store. Renders a JSON object.
*/}}
{{- define "tinycdi.staticDeniedEnv" -}}
{{- toJson (dict
  "TCDI_DEV_INSECURE_DB" "database.tls.mode (verify-ca/verify-full)"
  "TCDI_DEV_ALLOW_NO_BROKER" "operator.devAllowNoBroker (dev-only)"
  "TCDI_METRICS_LISTEN" "backend.metrics.enabled"
  "TCDI_KASM_ADAPTER_IMAGE" "kasmAdapter.enabled + kasmAdapter.image.digest"
  "PGSSLMODE" "database.tls.mode"
  "TCDI_OPERATOR_CN" "backend.operatorCN"
  "TCDI_WATCH_NAMESPACES" "managedNamespaces"
  "TCDI_REQUIRED_GROUPS" "oidc.requiredGroups"
  "TCDI_TENANT_ADMIN_GROUPS" "oidc.tenantAdminGroups"
  "TCDI_CONTROL_TOKEN" "backend.controlToken.existingSecret"
  "TCDI_CONTROL_TOKEN_FILE" "backend.controlToken.existingSecret"
  "TCDI_TRUSTED_PROXIES" "backend.trustedProxies"
  "TCDI_UPSTREAM_CA" "backend.upstreamCA.existingSecret"
  "TCDI_OIDC_POST_LOGOUT_REDIRECT" "oidc.postLogoutRedirect"
  "TCDI_TENANT_QUOTAS" "managedNamespaces[].quota"
  "TCDI_TENANT_QUOTA_CEILINGS" "managedNamespaces[].quotaCeiling"
  "TCDI_BROKER_URL" "nothing — the chart deploys the merged backend; the split-mode broker client is dev-only"
  "TCDI_BROKER_CA" "nothing — the chart deploys the merged backend; the split-mode broker client is dev-only"
  "TCDI_MTLS_CERT" "nothing — the chart deploys the merged backend; the split-mode broker client is dev-only"
  "TCDI_MTLS_KEY" "nothing — the chart deploys the merged backend; the split-mode broker client is dev-only"
  "SSL_CERT_FILE" "backend.extraCA.existingSecret"
  "SSL_CERT_DIR" "backend.extraCA.existingSecret"
) -}}
{{- end }}

{{/*
TM-42: the extraArgs deny list of one component — rendered flags, owned
flags, and the flag form of every TCDI_ name in tinycdi.staticDeniedEnv,
minus the documented knobs (tinycdi.flagOverrides). Called with
{component, root}; renders a JSON array.
*/}}
{{- define "tinycdi.deniedFlagNames" -}}
{{- $denied := concat (include "tinycdi.renderedFlagNames" . | fromJsonArray) (include "tinycdi.ownedFlags" . | fromJsonArray) -}}
{{- range $env, $_ := (include "tinycdi.staticDeniedEnv" . | fromJson) -}}
{{- if hasPrefix "TCDI_" $env -}}
{{- $denied = append $denied (trimPrefix "TCDI_" $env | lower | replace "_" "-") -}}
{{- end -}}
{{- end -}}
{{- $allowed := include "tinycdi.flagOverrides" . | fromJsonArray -}}
{{- $out := list -}}
{{- range $n := ($denied | uniq) -}}
{{- if not (has $n $allowed) -}}{{- $out = append $out $n -}}{{- end -}}
{{- end -}}
{{- toJson $out -}}
{{- end }}

{{/*
TM-42: the extraEnv deny map shared by every component — the env form
TCDI_<UPPER_SNAKE> of every component's denied flags, then
tinycdi.staticDeniedEnv on top (its hints win). Called with {root};
renders a JSON object name -> chart value to use instead.
*/}}
{{- define "tinycdi.deniedEnvMap" -}}
{{- $m := dict -}}
{{- range $c := list "backend" "operator" "frontend" -}}
{{- range $f := (include "tinycdi.deniedFlagNames" (dict "component" $c "root" $.root) | fromJsonArray) -}}
{{- $_ := set $m (printf "TCDI_%s" ($f | upper | replace "-" "_")) (printf "the %s chart value that renders -%s (a flag and its env name are one setting)" $c $f) -}}
{{- end -}}
{{- end -}}
{{- toJson (merge (include "tinycdi.staticDeniedEnv" . | fromJson) $m) -}}
{{- end }}
```

`merge` keeps the first map's keys, so the static hints win, and `TestExtraEnvDeniedNamesSuggestValue` keeps its expected values. Also update the comment above `tinycdi.denyExtraEnv` (lines 601-610): "on the denied map (tinycdi.deniedEnvMap: dev-surface and security env names, the trust store, and the env form of every denied flag — the map value names the chart value to use instead)".

Run:
```bash
cd deploy/helm
go test -count=1 -run 'TestExtraArgsCannotOverrideRenderedFlags|TestOperatorLeaderElectionGuard|TestExtraEnv|TestHardeningDevGateBypasses|TestDefaultRenderUnchanged' . > /tmp/t24-green.log 2>&1; echo "rc=$?"
grep -E '^\s+--- FAIL|^(ok|FAIL)' /tmp/t24-green.log | head -20
cd ../..
```
Expected: `rc=0` and `ok`.
- If the drift check names a flag that is "rendered outside tinycdi.args", it lives outside the moved block; move it into the helper.
- If a `denied/<c>/flag/<n>` probe fails, `<n>` is conditional and missing from `ownedFlags`; add it.
- `TestExtraEnvManagedDuplicateDenied` still renders a cross-component name, `TCDI_OIDC_ISSUER` on the frontend. The backend reads that value from env and does not render it as a flag, so it is not in the flag-derived map.

Run the whole chart suite in the background (it takes about 7 minutes):

```bash
cd deploy/helm && go test -count=1 . > /tmp/t24-chart.log 2>&1; echo "rc=$?"; grep -E '^\s+--- FAIL|^(ok|FAIL)' /tmp/t24-chart.log | head -20; cd ../..
```
Expected: `rc=0`.

- [ ] **Step 5: Docs the fix makes false**

- `deploy/helm/tinycdi/README.md:160`: replace the description cell with:
  "escape hatch. Without `dev.enabled`, an `extraArgs` entry fails the render when it names any flag the chart renders or owns for that component (`tinycdi.args.<component>`, `tinycdi.ownedFlags`), or the flag form of a denied `extraEnv` name. Only backend `-rate-limit-replicas` and `-login-key-file` may repeat a rendered flag; flags the chart never renders (`-login-rate`, `-launch-rate`, `-expiry-interval`, `-image-stale-after`) stay free. Operator `--leader-elect` is refused with `operator.replicas>1` even under `dev.enabled`. `extraEnv` fails the render likewise on the deny-list below, on the `TCDI_<FLAG>` env form of any denied flag, or when an entry duplicates an env var the chart manages for that component — `extraEnv` renders after the managed env block, so a duplicate name would silently win. `backend.extraVolumes` hostPath fails the render unless `dev.enabled`."
- `README.md`, deny table after the `TCDI_TENANT_QUOTAS` row: add

  ```
  | `TCDI_BROKER_URL` / `TCDI_BROKER_CA` / `TCDI_MTLS_CERT` / `TCDI_MTLS_KEY` | nothing — the chart deploys the merged backend; the split-mode broker client is dev-only |
  | `SSL_CERT_FILE` / `SSL_CERT_DIR` | `backend.extraCA.existingSecret` |
  | `TCDI_<FLAG>` of any flag denied in that component's `extraArgs` | the chart value that renders the flag |
  ```
- `values.yaml:895-898`: replace "dangerous extraArgs (operator --dev-allow-no-broker / --disable-builtin-egress-excepts / metrics flags, backend --dev-insecure-db / --required-groups / --metrics-listen / the split-mode broker client flags);" with "extraArgs naming any flag the chart renders or owns for that component, or the flag form of a denied env name (backend -rate-limit-replicas and -login-key-file excepted);". In the env list, after `TCDI_TENANT_QUOTAS`, insert "TCDI_BROKER_URL/TCDI_BROKER_CA/TCDI_MTLS_CERT/TCDI_MTLS_KEY, SSL_CERT_FILE/SSL_CERT_DIR, the TCDI_ form of any denied flag". Then re-wrap the comment at about 72 columns.

The values change is a comment, so the golden stays the same. Run `cd deploy/helm && go test -count=1 -run 'TestDefaultRenderUnchanged|TestREADME|TestValues' . 2>&1 | tail -3; cd ../..`, then `for t in .github/tests/*.test.sh; do bash "$t" || exit 1; done`. Expected: `ok`, then exit 0.

- [ ] **Step 6: Commit**

```bash
git add deploy/helm/tinycdi/templates/_args.tpl deploy/helm/tinycdi/templates/backend.yaml deploy/helm/tinycdi/templates/operator.yaml \
  deploy/helm/tinycdi/templates/frontend.yaml deploy/helm/tinycdi/templates/_helpers.tpl deploy/helm/tinycdi/README.md \
  deploy/helm/tinycdi/values.yaml deploy/helm/chart_extraenv_test.go deploy/helm/chart_guards_test.go
git commit -m "fix(chart): extraArgs and extraEnv cannot override any chart-rendered flag

R1-34: args move into tinycdi.args.<component>; the extraArgs deny list
is derived from them plus the owned conditional flags, the env deny map
is its TCDI_ mirror plus the broker-client and trust-store names, and
--leader-elect in operator.extraArgs is refused with replicas > 1.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 25: Trusted-proxy guidance; dead podSecurity value removed (R1-15, R1-36) — tier: integrate

**Findings.**
- **R1-15 (P3, TM-06): the install guide tells operators to trust the whole pod CIDR for XFF. A workspace pod that can reach :8443 then chooses its own rate-limit key.**
  - **Attack.**
    - A user's workspace runs with the `clusterOnly.unrestricted` opt-out and connects straight to the backend Service on :8443 or :8444. The backend's ingress is `ipBlock 0.0.0.0/0` by default, and on Calico-like CNIs ipBlock matches pod IPs.
    - The user sends `X-Forwarded-For: <victim office NAT IP>`. Because the documented trust list is the whole pod CIDR, the spoofed entry becomes the key. The attacker drains the office's login budget, or rotates keys to escape its own.
    - Doc locations: `docs/runbooks/install.md:363-367` and chart `README.md:594-597`.
  - **Required fix.**
    - install.md lists only the ingress controller's pod IPs or node range, and says never to include ranges where workspace pods run.
    - Add a startup warning when `-trusted-proxies` overlaps a managed tenant namespace's pod CIDR, if that CIDR is known.
    - Give TM-34's residual one sentence: with `unrestricted`, the backend ingress (`edgeIngressCIDRs`, default `0.0.0.0/0`) admits workspace pods.
  - **Security test.** A chart test asserts that `networkPolicy.edgeIngress=ipBlock` with the default CIDRs renders a NOTES warning when `runtime.networkProfiles.clusterOnly.unrestricted=true`.
- **R1-36 (P3, new on B8): `podSecurity.platformEnforce` is a dead value.**
  - **Attack.** An operator sets `podSecurity.platformEnforce: restricted` and expects the release namespace to be PSS-labelled. Nothing is applied: `rg -n platformEnforce` hits only `values.yaml`, `values.schema.json` and `README.md`.
  - **Required fix.** Remove the value and update the README row, or use it.
  - **Security test.** A chart test that fails when a values key under `podSecurity` is unreferenced by any template.

**Design (planner's ruling).**
- **R1-15: documentation plus a NOTES warning. No startup check.** Neither the backend nor the chart knows the cluster pod CIDR, so the overlap check the brief makes conditional ("if known") is not possible.
  - NOTES gets a warning block. It prints when `clusterOnly.unrestricted` is true and the backend's edge ingress admits every source. That covers `edgeIngress=ipBlock` with `0.0.0.0/0` or `::/0` in `edgeIngressCIDRs`, `edgeIngress=any`, and `networkPolicy.enabled=false`.
  - The warning text avoids the phrase "ClusterOnly egress", because `chart_clusteronly_egress_test.go` requires that phrase on exactly one line.
- **R1-36: remove the value, don't use it.** The release namespace is created and labelled by the installer (`install.md:238-246`), and the chart never owns it. A value that labels it would only take effect when Helm creates the namespace. With the schema's `additionalProperties: false`, a values file that still sets `podSecurity.platformEnforce` now fails the render instead of being ignored. No upgrade path is kept (Global Constraints).

**Files:**
- Modify: `deploy/helm/tinycdi/templates/NOTES.txt:99-103` (new block after the ClusterOnly UNRESTRICTED block)
- Modify: `deploy/helm/tinycdi/values.yaml:340-358` (trustedProxies comment), `:884-891` (podSecurity comment + key)
- Modify: `deploy/helm/tinycdi/values.schema.json:528` (drop `platformEnforce`)
- Modify: `deploy/helm/tinycdi/README.md:98` (podSecurity row), `:594-597` (Traefik example)
- Modify: `docs/runbooks/install.md:363-367` (Traefik example + paragraph)
- Modify: `docs/security/threat-model.md:713` (TM-34 Residual cell, append only)
- Modify: `.github/tests/security-doc-claims.test.sh` (append above the final `exit "$fails"`; Task 10 created it)
- Test: `deploy/helm/chart_edgepolicy_test.go` (new `TestNotesWarnUnrestrictedEdgeOpen`), `deploy/helm/chart_hardening_test.go` (new `TestPodSecurityValuesAllReferenced`)

**Interfaces:**
- Consumes:
  - `renderNotes` (`chart_test.go:2511`) and `minimalValues` (`chart_edgepolicy_test.go:24`).
  - Task 10's `need`, `absent` and `need_row` helpers in `security-doc-claims.test.sh`.
  - Task 19 leaves the TM-34 Residual cell's text in place (it edits B6 prose and Task 30 owns Control/Pinned-by). This task appends one sentence to the end of that cell, whatever its text is at the time.
- Produces:
  - Task 30 adds the two new tests to the inventory: `TestNotesWarnUnrestrictedEdgeOpen` under TM-34/TM-06 and `TestPodSecurityValuesAllReferenced` in the new B8 row for R1-36.
  - It also adds the doc-claims script lines for R1-15/R1-36.

- [ ] **Step 1: Write the failing tests**

Append to `deploy/helm/chart_edgepolicy_test.go`:

```go
// TestNotesWarnUnrestrictedEdgeOpen (R1-15, TM-34): with the ClusterOnly
// unrestricted opt-out, workspace pods can reach any pod; when the
// backend's edge ingress also admits every source, NOTES warns once and
// tells the operator never to trust a workspace pod range in
// backend.trustedProxies. A narrowed edge range, or the restricted
// default, renders no warning.
func TestNotesWarnUnrestrictedEdgeOpen(t *testing.T) {
	const marker = "Edge ingress: WARNING"
	unrestricted := "runtime.networkProfiles.clusterOnly.unrestricted=true"
	cases := []struct {
		name string
		sets []string
		warn bool
	}{
		{"restricted default", nil, false},
		{"unrestricted, default 0.0.0.0/0", []string{"--set", unrestricted}, true},
		{"unrestricted, edgeIngress any", []string{"--set", unrestricted, "--set", "networkPolicy.edgeIngress=any"}, true},
		{"unrestricted, networkPolicy off", []string{"--set", unrestricted, "--set", "networkPolicy.enabled=false"}, true},
		{"unrestricted, narrowed CIDRs", []string{"--set", unrestricted, "--set-json", `networkPolicy.edgeIngressCIDRs=["192.0.2.0/24"]`}, false},
	}
	for _, tc := range cases {
		notes := renderNotes(t, append([]string{"-f", minimalValues}, tc.sets...)...)
		n := strings.Count(notes, marker)
		if tc.warn && (n != 1 || !strings.Contains(notes, "backend.trustedProxies") || !strings.Contains(notes, ":8443/:8444")) {
			t.Errorf("%s: want one %q naming backend.trustedProxies and :8443/:8444, got %d:\n%s", tc.name, marker, n, notes)
		}
		if !tc.warn && n != 0 {
			t.Errorf("%s: want no %q, got:\n%s", tc.name, marker, notes)
		}
		if strings.Count(notes, "ClusterOnly egress") > 1 {
			t.Errorf("%s: the edge warning must not repeat the ClusterOnly egress line:\n%s", tc.name, notes)
		}
	}
}
```

Append to `deploy/helm/chart_hardening_test.go`, and add `"regexp"` and `"gopkg.in/yaml.v3"` to its imports (the package already depends on yaml.v3 through `chart_test.go`):

```go
// TestPodSecurityValuesAllReferenced (R1-36): every key under podSecurity
// in values.yaml is read by some template — a value that does nothing
// tells an operator a control is on when it is not.
func TestPodSecurityValuesAllReferenced(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("tinycdi", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		PodSecurity map[string]any `yaml:"podSecurity"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	if len(values.PodSecurity) == 0 {
		t.Fatal("values.yaml has no podSecurity keys")
	}
	files, err := filepath.Glob(filepath.Join("tinycdi", "templates", "*"))
	if err != nil {
		t.Fatal(err)
	}
	var src strings.Builder
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src.Write(b)
	}
	for key := range values.PodSecurity {
		re := regexp.MustCompile(`\.Values\.podSecurity\.` + regexp.QuoteMeta(key) + `\b`)
		if !re.MatchString(src.String()) {
			t.Errorf("podSecurity.%s is set in values.yaml but no template reads it", key)
		}
	}
}
```

- [ ] **Step 2: Run the tests and watch them fail**

Run:
```bash
cd deploy/helm && go test -count=1 -run 'TestNotesWarnUnrestrictedEdgeOpen|TestPodSecurityValuesAllReferenced' . 2>&1 | grep -E 'want one|no template reads|^(ok|FAIL)' | head; cd ../..
```
Expected: `FAIL`, with these lines:
- `unrestricted, default 0.0.0.0/0: want one "Edge ingress: WARNING" … got 0`, and the same for the `any` and `networkPolicy off` cases (R1-15).
- `podSecurity.platformEnforce is set in values.yaml but no template reads it` (R1-36).

- [ ] **Step 3: NOTES warning**

`deploy/helm/tinycdi/templates/NOTES.txt`: directly after the `{{- end }}` that closes the `{{- if $co.unrestricted }}` block (line 103), insert:

```
{{- $np := default dict .Values.networkPolicy }}
{{- $edgeCIDRs := $np.edgeIngressCIDRs | default list }}
{{- $edgeOpen := or (not $np.enabled) (eq (toString $np.edgeIngress) "any") (and (eq (toString $np.edgeIngress) "ipBlock") (or (has "0.0.0.0/0" $edgeCIDRs) (has "::/0" $edgeCIDRs))) }}
{{- if and $co.unrestricted $edgeOpen }}

Edge ingress: WARNING — runtime.networkProfiles.clusterOnly.unrestricted=true lets ClusterOnly workspaces reach any pod, and the backend's edge ingress admits every source (networkPolicy.edgeIngressCIDRs holds 0.0.0.0/0 or ::/0, edgeIngress=any, or networkPolicy.enabled=false), so a workspace can call the backend :8443/:8444 directly. Narrow networkPolicy.edgeIngressCIDRs to the load-balancer range (or use edgeIngress: cilium), and never list a range where workspace pods run in backend.trustedProxies: a trusted peer chooses its own rate-limit key through X-Forwarded-For.
{{- end }}
```

Run: `cd deploy/helm && go test -count=1 -run 'TestNotesWarnUnrestrictedEdgeOpen|TestClusterOnlyEgress' . 2>&1 | tail -3; cd ../..`. Expected: `ok`. During planning this block and the test were checked in a scratch copy of the chart: the default, narrowed and `any`/off cases behave as listed.

- [ ] **Step 4: Remove `podSecurity.platformEnforce`**

- `deploy/helm/tinycdi/values.yaml:884-891`: replace the comment and block with:

  ```yaml
  # Pod Security Standards level for the managed (tenant) namespaces the
  # chart creates — their enforce and warn labels. The release namespace is
  # created and labelled by the installer (docs/runbooks/install.md,
  # "Install"); the chart never labels it. managedEnforce=privileged is
  # refused unless dev.enabled — it would reopen operator→privileged-pod
  # (CHTR-2).
  podSecurity:
    managedEnforce: restricted
  ```
- `deploy/helm/tinycdi/values.schema.json:528`: delete the `"platformEnforce": {…},` line. `podSecurity` keeps `"additionalProperties": false`.
- `deploy/helm/tinycdi/README.md:98`: replace the row with

  ```
  | `podSecurity.managedEnforce` | `restricted` | PSS `enforce`/`warn` label on the managed namespaces the chart creates (the installer labels the release namespace — see the install runbook); `privileged` needs `dev.enabled` (CHTR-2) |
  ```

Run:
```bash
cd deploy/helm
go test -count=1 -run 'TestPodSecurityValuesAllReferenced|TestHardeningManagedEnforce|TestDefaultRenderUnchanged|TestNotesWarnUnrestrictedEdgeOpen' . 2>&1 | tail -3
${TCDI_HELM:-helm} template tcdi ./tinycdi --namespace tcdi-system -f tinycdi/ci/minimal-values.yaml --set podSecurity.platformEnforce=baseline 2>&1 | grep -c 'schema'
cd ../..
```
Expected: `ok`, then `1`. The removed key now fails the schema. The default render does not change, so the golden needs no regeneration. This was checked during planning.

- [ ] **Step 5: Trusted-proxy guidance and TM-34 residual**

- `docs/runbooks/install.md:363-367`: replace the Traefik block with:

  ````
  ```yaml
  # Traefik or another ingress running as pods — the peer is the proxy pod
  # IP. List only addresses the proxy pods can hold: an IP pool dedicated
  # to the ingress namespace (shown) or, for a hostNetwork proxy, the node
  # subnet.
  backend:
    trustedProxies: ["10.42.250.0/24"]
  ```

  Never list the cluster pod CIDR or any range where workspace pods run.
  A peer inside `backend.trustedProxies` chooses, through
  `X-Forwarded-For`, the client address the backend keys its rate limits
  on and forwards toward workspace pods. With
  `runtime.networkProfiles.clusterOnly.unrestricted=true` a workspace can
  reach the backend directly (the edge ingress admits
  `networkPolicy.edgeIngressCIDRs`, `0.0.0.0/0` by default), and the chart
  NOTES print a warning for that combination.
  ````
- `deploy/helm/tinycdi/README.md:594-597`: replace the two comment lines and the `trustedProxies` line with:

  ```yaml
  # Traefik (or any ingress as pods) — the peer is the proxy pod's address.
  # List only an IP pool dedicated to the ingress namespace (or the node
  # subnet for a hostNetwork proxy). Never list the cluster pod CIDR or any
  # range where workspace pods run: a trusted peer picks its own client key.
  backend:
    trustedProxies: ["10.42.250.0/24"]    # IP pool of the ingress namespace
  ```
- `deploy/helm/tinycdi/values.yaml`, `trustedProxies` comment: after "See the README "Rate limits and trusted proxies" for Cilium/Traefik examples.", add the line "# Never list a range where workspace pods run (the cluster pod CIDR): a trusted peer picks its own client key through X-Forwarded-For." Wrap it at the comment's width.
- `docs/security/threat-model.md:713` (TM-34), Residual cell: append " With `unrestricted`, the backend's edge ingress (`networkPolicy.edgeIngressCIDRs`, default `0.0.0.0/0`) admits workspace pods too, so `backend.trustedProxies` must never cover a range where workspace pods run; the chart NOTES warn on that combination." before the closing ` |`.

- [ ] **Step 6: Doc-claims assertions**

Append to `.github/tests/security-doc-claims.test.sh`, directly above the final `exit "$fails"` line:

```bash
# R1-15: trusted-proxy guidance never trusts a workspace pod range; TM-34
# says the unrestricted opt-out exposes the backend edge to workspaces.
need R1-15 docs/runbooks/install.md '[Nn]ever list the cluster pod CIDR or any range where workspace pods run'
absent R1-15 docs/runbooks/install.md 'list the cluster pod CIDR \(shown'
need R1-15 deploy/helm/tinycdi/README.md '[Nn]ever list the cluster pod CIDR'
absent R1-15 deploy/helm/tinycdi/README.md 'pod CIDR containing the traefik pods'
need_row R1-15 TM-34 'edgeIngressCIDRs.*admits workspace pods'
need_row R1-15 TM-34 'backend\.trustedProxies` must never cover'
# R1-36: no dead podSecurity value is documented.
absent R1-36 deploy/helm/tinycdi/README.md 'platformEnforce'
absent R1-36 deploy/helm/tinycdi/values.yaml 'platformEnforce'
```

Run:
```bash
bash .github/tests/security-doc-claims.test.sh; echo "rc=$?"
git stash push -q docs/runbooks/install.md deploy/helm/tinycdi/README.md && bash .github/tests/security-doc-claims.test.sh | grep -c 'FAIL: R1-'; git stash pop -q
for t in .github/tests/*.test.sh; do bash "$t" || exit 1; done
```
Expected:
- `rc=0`.
- The stash check prints `5`: the two `need` and two `absent` R1-15 doc lines, plus the README `platformEnforce` line, all bite on the old text.
- The loop exits 0.

- [ ] **Step 7: Commit**

```bash
git add deploy/helm/tinycdi/templates/NOTES.txt deploy/helm/tinycdi/values.yaml deploy/helm/tinycdi/values.schema.json \
  deploy/helm/tinycdi/README.md docs/runbooks/install.md docs/security/threat-model.md \
  .github/tests/security-doc-claims.test.sh deploy/helm/chart_edgepolicy_test.go deploy/helm/chart_hardening_test.go
git commit -m "fix(chart): narrow trusted-proxy guidance and remove podSecurity.platformEnforce

R1-15: install guide and chart README list only ingress-owned ranges and
never a workspace pod range; NOTES warn when the ClusterOnly unrestricted
opt-out meets an all-addresses edge ingress; TM-34 residual says so.
R1-36: the unused podSecurity.platformEnforce value is removed.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 26: Train: build time carried to the manifest; stale scan refused before signing (R1-09, train) — tier: design

**Finding R1-09 (P2, TM-49): the train publishes and signs a scan and builtAt that can be about 30 days stale.**
- **Attack:** "The runtime-train publish waits on a manual approval. GitHub keeps a pending deployment for up to 30 days, and a 're-run failed jobs' of an older run re-enters the same publish. The approver may approve late, or re-run an old failed or rejected train. Either way, publish signs digests whose trivy scan is days to weeks old, without rescanning. It also stamps `runtime-images.json.builtAt` with the publish time. Operators copy that into `images.*.builtAt`, which becomes the `image-built-at` annotation. That annotation drives `-image-stale-after` (14 d) and `-image-block-after` (45 d, `409 IMAGE_STALE`). A ~30-day-old browser engine therefore reads as new, and the block fires up to ~30 days late."
- **Proof:** fixture SBOMs dated `2026-09-01` and `RT_TAG=rt-20260901.1` through the real `write-runtime-manifest.sh` gave `{"builtAt":"2026-10-14T15:54:54Z",…}`: the build date was lost, and no workflow passes `BUILT_AT`.
- **Required fix (train part; Task 27 is the release part):** "Record the build time in the build job and carry it to publish in the ref artifact or the image `org.opencontainers.image.created` label. Set `BUILT_AT` from it. write-runtime-manifest.sh should refuse a missing `BUILT_AT` instead of defaulting to now. In the publish job, re-run the trivy gate against the exact digests just before `cosign sign`, or fail when scan completion is older than N hours (for example 24 h)."
- **Security test:** "`.github/tests/write-runtime-manifest.test.sh`: a case where unset `BUILT_AT` fails, and `builtAt` equals the build-job timestamp, not now." Plus `.github/tests/check-scan-age.test.sh` and `release-topology.test.sh` train assertions. All fail today.

**Design ruling (scan-age bound, not an in-publish rescan).** The publish job holds `id-token: write`, `packages: write` and `contents: write`. SEC-05 (the comment on the `scan` job, `runtime-images.yml:502-503`) keeps every scanner out of jobs with those permissions. Trivy parses a downloaded, unsigned vulnerability DB, and a parser bug there must never sit next to the OIDC token. A rescan job before publish would run *before* the approval wait, so it would not shorten the window. So publish takes the brief's second option: it reads each scan leg's `completed_at` from the GitHub jobs API (`filter=latest`, so a job carried over by "re-run failed jobs" keeps its original time). It refuses to sign when any leg finished more than `SCAN_MAX_AGE_HOURS=24` hours earlier. The remedy is a fresh run, which rebuilds and rescans. The jobs API is outside the run's artifacts, so a forged artifact cannot fake it. Build time travels as a `<image>.built-at` file inside the existing `image-ref-<image>` artifact. It holds the same UTC second as the image's `org.opencontainers.image.created` label, and the manifest's `builtAt` is the earliest of the three.

**Files:**
- Create: `.github/scripts/earliest-built-at.sh`, `.github/scripts/check-scan-age.sh`, `.github/tests/check-scan-age.test.sh`
- Modify: `.github/scripts/write-runtime-manifest.sh:18-26` (header `Env:` lines, `BUILT_AT` required and validated)
- Modify: `.github/tests/write-runtime-manifest.test.sh` (new cases above the final summary, lines 121-124)
- Modify: `.github/workflows/runtime-images.yml`:
  - workflow `env:` (110-121): add `SCAN_MAX_AGE_HOURS`
  - `base` job: `compute tags + OCI labels` (206-224), `record image ref for downstream jobs` (245-254), `upload image ref` (256-261)
  - `profile` job: `compute tags + OCI labels` (367-384), `record image ref for downstream jobs` (431-440), `upload image ref` (442-447)
  - `publish` job: `validate refs + SBOMs (strict repo@sha256)` (726-736), a new step before `login to ghcr.io` (738), and `write runtime-images.json` (835-842)
- Modify: `.github/tests/release-topology.test.sh` (train block, after line 265: `order "runtime-images: rt derive before promote" …`)
- Modify: `.github/README.md:249-275` (the runtime-train manifest section)

**Interfaces:**
- Produces: `earliest-built-at.sh <refs-dir> <image>...` prints the earliest `YYYY-MM-DDTHH:MM:SSZ` among `<refs-dir>/<image>.built-at`. It exits 1, with an `::error::` on stderr, when a file is missing, is not exactly that form, or is not a real date. Task 27 reuses it from `stamp-image-digests.sh` and `collect-publish-inputs.sh` (one image at a time).
- Produces: `check-scan-age.sh <jobs.json> <max-age-hours> <scan job name>...`, where `jobs.json` is a JSON array of job objects (`.jobs[]` of `GET /repos/{o}/{r}/actions/runs/{id}/jobs?filter=latest`). Each named job must occur exactly once, with `conclusion == "success"` and `completed_at` no more than `max-age-hours` before now. The env `NOW_EPOCH` overrides "now" (tests only). Exit 0 or 1. Task 27 calls it from `release.yml`, and Task 29 adds the `kasm catalog scan (trivy gate + engine floor)` leg to the release call.
- Produces: the artifact `image-ref-<image>` now holds two files, `<image>.ref` and `<image>.built-at` (one line, `YYYY-MM-DDTHH:MM:SSZ`), in `runtime-images.yml`. Task 27 does the same in `release.yml`.
- Produces: workflow env `SCAN_MAX_AGE_HOURS: "24"` and the publish step name `refuse a stale scan (older than SCAN_MAX_AGE_HOURS)`.
- Consumes: `write-runtime-manifest.sh <refs-dir> <sboms-dir> <out-file>` with env `RT_TAG`, `BUILT_AT` (unchanged signature).

- [ ] **Step 1: Write the failing manifest tests**

In `.github/tests/write-runtime-manifest.test.sh`, add after line 13 (`fails=0`):

```bash
EARLIEST="$ROOT/.github/scripts/earliest-built-at.sh"
```

Insert above the final `if [ "$fails" -eq 0 ]; then echo "write-runtime-manifest: all cases pass"; fi`:

```bash
# --- R1-09: BUILT_AT is required — the publish clock is never builtAt ---
out="$(RT_TAG=rt-20261020.1 bash "$GEN" "$D/refs" "$D/sboms" "$D/nobuilt.json" 2>&1)" && rc=0 || rc=$?
{ [ "$rc" -ne 0 ] && [ ! -e "$D/nobuilt.json" ]; } \
  || { echo "FAIL: unset BUILT_AT accepted (builtAt would be the publish time)"; fails=1; }

# --- R1-09: BUILT_AT must be a UTC RFC 3339 second ---
out="$(RT_TAG=rt-20261020.1 BUILT_AT=yesterday \
  bash "$GEN" "$D/refs" "$D/sboms" "$D/badbuilt.json" 2>&1)" && rc=0 || rc=$?
{ [ "$rc" -ne 0 ] && [ ! -e "$D/badbuilt.json" ]; } \
  || { echo "FAIL: malformed BUILT_AT accepted"; fails=1; }

# --- R1-09: builtAt is the EARLIEST build-job time of the train ---
mkdir -p "$D/refs-ba"; cp "$D/refs/"*.ref "$D/refs-ba/"
printf '2026-09-01T04:23:00Z\n' > "$D/refs-ba/linux-base.built-at"
printf '2026-09-01T04:41:10Z\n' > "$D/refs-ba/linux-desktop.built-at"
printf '2026-09-01T04:40:02Z\n' > "$D/refs-ba/browser.built-at"
ba="$(bash "$EARLIEST" "$D/refs-ba" linux-base linux-desktop browser 2>&1)" && rc=0 || rc=$?
if [ "$rc" -ne 0 ] || [ "$ba" != "2026-09-01T04:23:00Z" ]; then
  echo "FAIL: earliest-built-at.sh gave '$ba' (rc=$rc), want 2026-09-01T04:23:00Z"; fails=1
else
  out="$(RT_TAG=rt-20261020.1 BUILT_AT="$ba" \
    bash "$GEN" "$D/refs-ba" "$D/sboms" "$D/early.json" 2>&1)" && rc=0 || rc=$?
  { [ "$rc" -eq 0 ] && jq -e '.builtAt == "2026-09-01T04:23:00Z"' "$D/early.json" >/dev/null 2>&1; } \
    || { echo "FAIL: manifest builtAt is not the build-job time: $out"; fails=1; }
fi

# --- R1-09: a missing or malformed build time fails closed ---
mkdir -p "$D/refs-noba"; cp "$D/refs-ba/"* "$D/refs-noba/"; rm "$D/refs-noba/browser.built-at"
bash "$EARLIEST" "$D/refs-noba" linux-base linux-desktop browser >/dev/null 2>&1 \
  && { echo "FAIL: earliest-built-at.sh accepted a missing browser.built-at"; fails=1; }
mkdir -p "$D/refs-badba"; cp "$D/refs-ba/"* "$D/refs-badba/"
printf '2026-09-31T25:00:00Z\n' > "$D/refs-badba/browser.built-at"
bash "$EARLIEST" "$D/refs-badba" linux-base linux-desktop browser >/dev/null 2>&1 \
  && { echo "FAIL: earliest-built-at.sh accepted an impossible date"; fails=1; }
printf '2026-09-01 04:40:02\n' > "$D/refs-badba/browser.built-at"
bash "$EARLIEST" "$D/refs-badba" linux-base linux-desktop browser >/dev/null 2>&1 \
  && { echo "FAIL: earliest-built-at.sh accepted a non-RFC 3339 time"; fails=1; }
```

- [ ] **Step 2: Write the failing scan-age test**

Create `.github/tests/check-scan-age.test.sh`:

```bash
#!/usr/bin/env bash
# check-scan-age.test.sh — R1-09 / TM-49: a signing job refuses digests
# whose trivy gate finished longer ago than the bound, a scan leg that is
# missing, duplicated or not successful, and malformed input. A fresh,
# successful scan set passes, including one exactly at the bound.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CHK="$ROOT/.github/scripts/check-scan-age.sh"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
fails=0

NOW_EPOCH="$(date -u -d 2026-10-14T12:00:00Z +%s)"; export NOW_EPOCH

job() { printf '{"name":"%s","conclusion":%s,"completed_at":%s}' "$1" "$2" "$3"; }
mk() { # mk <file> <job-json>... — a JSON array of the given jobs
  local f="$D/$1"; shift
  ( IFS=,; printf '[%s]\n' "$*" ) > "$f"
}
run() { # run <file> [max-hours] -> rc, out
  out="$(bash "$CHK" "$D/$1" "${2:-24}" 'scan linux-base' 'scan browser' 2>&1)"; rc=$?
}
pass() { run "$1" "${3:-24}"; [ "$rc" -eq 0 ] || { echo "FAIL: $2 refused: $out"; fails=1; }; }
refuse() { # refuse <file> <desc> <substr> [max-hours]
  run "$1" "${4:-24}"
  if [ "$rc" -eq 0 ]; then echo "FAIL: $2 accepted"; fails=1
  elif ! grep -qF -- "$3" <<< "$out"; then echo "FAIL: $2: want '$3' in: $out"; fails=1; fi
}

mk fresh.json  "$(job 'scan linux-base' '"success"' '"2026-10-14T03:00:00Z"')" \
               "$(job 'scan browser' '"success"' '"2026-10-13T13:00:00Z"')" \
               "$(job 'publish' null null)"
mk edge.json   "$(job 'scan linux-base' '"success"' '"2026-10-14T03:00:00Z"')" \
               "$(job 'scan browser' '"success"' '"2026-10-13T12:00:00Z"')"
mk stale.json  "$(job 'scan linux-base' '"success"' '"2026-10-14T03:00:00Z"')" \
               "$(job 'scan browser' '"success"' '"2026-09-15T09:00:00Z"')"
mk failed.json "$(job 'scan linux-base' '"success"' '"2026-10-14T03:00:00Z"')" \
               "$(job 'scan browser' '"failure"' '"2026-10-14T03:00:00Z"')"
mk missing.json "$(job 'scan linux-base' '"success"' '"2026-10-14T03:00:00Z"')"
mk dup.json    "$(job 'scan linux-base' '"success"' '"2026-10-14T03:00:00Z"')" \
               "$(job 'scan browser' '"success"' '"2026-10-14T03:00:00Z"')" \
               "$(job 'scan browser' '"success"' '"2026-09-01T00:00:00Z"')"
mk nodate.json "$(job 'scan linux-base' '"success"' '"2026-10-14T03:00:00Z"')" \
               "$(job 'scan browser' '"success"' null)"
echo '{"jobs":[]}' > "$D/object.json"

pass   fresh.json "fresh successful scans"
pass   edge.json  "a scan exactly 24 h old"
refuse stale.json   "a scan finished a month before signing" "older than 24 h"
refuse failed.json  "a failed scan leg"                      "not success"
refuse missing.json "a missing scan leg"                     "found 0"
refuse dup.json     "a duplicated scan leg name"             "found 2"
refuse nodate.json  "a scan leg without completed_at"        "no valid completed_at"
refuse object.json  "a jobs object instead of an array"      "not a JSON array"
refuse fresh.json   "a zero max age"                         "not a positive integer" 0

if [ "$fails" -eq 0 ]; then echo "check-scan-age: all cases pass"; fi
exit "$fails"
```

- [ ] **Step 3: Write the failing train topology assertions**

In `.github/tests/release-topology.test.sh`, insert after line 265 (`order "runtime-images: rt derive before promote" …`):

```bash
# R1-09: the train's builtAt is the build jobs' recorded time, never the
# publish clock, and publish refuses a scan older than the bound.
chk "runtime-images: base records its build time" "$TRAIN" 'refs/linux-base\.built-at'
chk "runtime-images: profile legs record their build time" "$TRAIN" 'refs/\$IMG\.built-at'
chk "runtime-images: label created = recorded build time" "$TRAIN" 'org\.opencontainers\.image\.created=\$BUILT_AT'
chk "runtime-images: manifest builtAt from build times" "$TRAIN" 'earliest-built-at\.sh refs linux-base linux-desktop browser\)'
chk "runtime-images: scan age bound wired" "$TRAIN" 'check-scan-age\.sh jobs\.json "\$SCAN_MAX_AGE_HOURS"'
order "runtime-images: scan age checked before signing" "$TRAIN" 'check-scan-age\.sh jobs\.json' 'name: cosign sign images'
chk_absent "runtime-images: manifest never defaults builtAt to now" \
  "$ROOT/.github/scripts/write-runtime-manifest.sh" 'BUILT_AT:-'
```

- [ ] **Step 4: Run the three tests and see them fail**

Run: `for t in write-runtime-manifest check-scan-age release-topology; do bash .github/tests/$t.test.sh; echo "rc=$?"; done 2>&1 | grep -E '^(FAIL|rc=)'`

Expected:
- `FAIL: unset BUILT_AT accepted (builtAt would be the publish time)`
- `FAIL: malformed BUILT_AT accepted`
- `FAIL: earliest-built-at.sh gave '…No such file…' (rc=127), want 2026-09-01T04:23:00Z`
- `rc=1` for `write-runtime-manifest`
- every `pass` case of `check-scan-age` fails with `No such file or directory`, then `rc=1`
- the seven new `FAIL: runtime-images: …` lines, then `rc=1`

- [ ] **Step 5: Add `earliest-built-at.sh`**

Create `.github/scripts/earliest-built-at.sh` (mode 0755):

```bash
#!/usr/bin/env bash
# earliest-built-at.sh — print the earliest build time recorded for a set
# of images (R1-09). Each build job writes <image>.built-at next to its
# <image>.ref in the image-ref-<image> artifact: the same UTC second it
# stamps into the image's org.opencontainers.image.created label. The
# runtime train's manifest builtAt and a release chart's
# images.<key>.builtAt come from here, never from the clock at publish
# time, so a late approval or a re-run of an old run cannot make an old
# image look new.
#
# Usage: earliest-built-at.sh <refs-dir> <image> [<image>...]
# Prints YYYY-MM-DDTHH:MM:SSZ; exits 1 on a missing, malformed or
# impossible time (fail closed — the caller never signs without one).
set -euo pipefail

DIR="${1:?usage: earliest-built-at.sh <refs-dir> <image> [<image>...]}"
shift
[ "$#" -gt 0 ] || { echo "::error::no images given" >&2; exit 1; }

min=""
for img in "$@"; do
  f="$DIR/$img.built-at"
  [ -f "$f" ] || { echo "::error::missing build time '$f' for image '$img'" >&2; exit 1; }
  v="$(tr -d '[:space:]' < "$f")"
  [[ "$v" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] \
    || { echo "::error::$f is not a UTC RFC 3339 second ('$v')" >&2; exit 1; }
  # GNU date normalises nothing back: round-trip to reject 2026-09-31 etc.
  [ "$(date -u -d "$v" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || true)" = "$v" ] \
    || { echo "::error::$f is not a real date ('$v')" >&2; exit 1; }
  # Fixed-width UTC form: lexical order is time order.
  if [ -z "$min" ] || [[ "$v" < "$min" ]]; then min="$v"; fi
done
echo "$min"
```

- [ ] **Step 6: Add `check-scan-age.sh`**

Create `.github/scripts/check-scan-age.sh` (mode 0755):

```bash
#!/usr/bin/env bash
# check-scan-age.sh — refuse to sign digests whose trivy gate finished too
# long ago (R1-09, TM-49). The signing job waits on a manual environment
# approval, which GitHub keeps pending for up to 30 days, and "re-run
# failed jobs" of an old run re-enters the signing job without re-running
# its scan legs. Signing then would vouch for a scan days or weeks old.
#
# Completion times come from the GitHub jobs API (latest attempt of each
# job: a leg carried over from an earlier attempt keeps its original
# completed_at), never from an artifact the run produced. Scanners stay
# out of the signing job (SEC-05); this check is the freshness bound.
#
# Usage: check-scan-age.sh <jobs.json> <max-age-hours> <scan job name>...
#   jobs.json — a JSON array of job objects, e.g.
#     gh api --paginate "repos/$R/actions/runs/$ID/jobs?filter=latest&per_page=100" \
#       --jq '.jobs[]' | jq -s .
# Env: NOW_EPOCH — override "now" (tests only).
set -euo pipefail

JOBS="${1:?usage: check-scan-age.sh <jobs.json> <max-age-hours> <scan job name>...}"
MAX_H="${2:?usage: check-scan-age.sh <jobs.json> <max-age-hours> <scan job name>...}"
shift 2
[ "$#" -gt 0 ] || { echo "::error::no scan job names given"; exit 1; }
[[ "$MAX_H" =~ ^[1-9][0-9]*$ ]] \
  || { echo "::error::max age '$MAX_H' is not a positive integer"; exit 1; }
jq -e 'type == "array"' "$JOBS" >/dev/null 2>&1 \
  || { echo "::error::$JOBS is not a JSON array of jobs"; exit 1; }

now="${NOW_EPOCH:-$(date -u +%s)}"
fail=0
for name in "$@"; do
  n="$(jq --arg n "$name" '[.[] | select(.name == $n)] | length' "$JOBS")"
  if [ "$n" -ne 1 ]; then
    echo "::error::expected exactly one job named '$name', found $n"; fail=1; continue
  fi
  concl="$(jq -r --arg n "$name" '.[] | select(.name == $n) | .conclusion // ""' "$JOBS")"
  done_at="$(jq -r --arg n "$name" '.[] | select(.name == $n) | .completed_at // ""' "$JOBS")"
  if [ "$concl" != "success" ]; then
    echo "::error::scan job '$name' concluded '$concl', not success"; fail=1; continue
  fi
  if ! t="$(date -u -d "$done_at" +%s 2>/dev/null)" || [ -z "$done_at" ]; then
    echo "::error::scan job '$name' has no valid completed_at ('$done_at')"; fail=1; continue
  fi
  age_s=$(( now - t ))
  if [ "$age_s" -gt $(( MAX_H * 3600 )) ]; then
    echo "::error::scan job '$name' completed $done_at ($(( age_s / 3600 )) h ago) — older than $MAX_H h; start a fresh run (rebuild + rescan) instead of approving or re-running this one"
    fail=1
  else
    echo "scan '$name' completed $done_at ($(( age_s / 3600 )) h ago)"
  fi
done
exit "$fail"
```

- [ ] **Step 7: Make the manifest writer require `BUILT_AT`**

In `.github/scripts/write-runtime-manifest.sh`:
- Replace the header line 19, `#      BUILT_AT — manifest timestamp (default: now, UTC)`, with:

  ```bash
  #      BUILT_AT — required; the earliest build time of the train's images
  #                 (earliest-built-at.sh over the build jobs' <image>.built-at),
  #                 UTC YYYY-MM-DDTHH:MM:SSZ. Never defaulted to the publish
  #                 clock (R1-09): a late approval must not make old images new.
  ```

- Replace line 26, `BUILT_AT="${BUILT_AT:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"`, with:

  ```bash
  BUILT_AT="${BUILT_AT:?BUILT_AT unset (the build-job time from earliest-built-at.sh)}"
  [[ "$BUILT_AT" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] \
    || { echo "::error::BUILT_AT '$BUILT_AT' is not UTC YYYY-MM-DDTHH:MM:SSZ"; exit 1; }
  ```

- [ ] **Step 8: Record the build time in the train's build jobs**

In `.github/workflows/runtime-images.yml`:

1. Workflow `env:` (lines 110-121): add after `COSIGN_BIN_SHA256: …` (line 118):

   ```yaml
     # R1-09: the signing job refuses a scan leg that finished longer ago
     # than this (check-scan-age.sh) — approve within a day, or start a
     # fresh run.
     SCAN_MAX_AGE_HOURS: "24"
   ```

2. `base` job, step `compute tags + OCI labels`: make the first line of `run: |` (above the `# PUB-2` comment) the following. Inside the `{ … }` group, add `echo "BUILT_AT=$BUILT_AT"` directly before `echo "OCI_LABELS<<__EOF__"`. Replace `echo "org.opencontainers.image.created=$(date -u +%Y-%m-%dT%H:%M:%SZ)"` with `echo "org.opencontainers.image.created=$BUILT_AT"`.

   ```bash
             # R1-09: one recorded build second — the image label and the
             # <image>.built-at file the publish job reads carry the same value.
             BUILT_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
   ```

3. `base` job, step `record image ref for downstream jobs`: append after the closing `fi`:

   ```bash
             echo "$BUILT_AT" > refs/linux-base.built-at
   ```

4. `base` job, step `upload image ref`: replace `path: refs/linux-base.ref` with:

   ```yaml
             path: |
               refs/linux-base.ref
               refs/linux-base.built-at
   ```

5. `profile` job: make the same three edits. In `compute tags + OCI labels`, add the same `BUILT_AT=…` line, the same `echo "BUILT_AT=$BUILT_AT"` before `echo "OCI_LABELS<<__EOF__"`, and `org.opencontainers.image.created=$BUILT_AT`. In `record image ref for downstream jobs`, append `echo "$BUILT_AT" > "refs/$IMG.built-at"` after the `fi`. In `upload image ref`, set:

   ```yaml
             path: |
               refs/${{ matrix.image }}.ref
               refs/${{ matrix.image }}.built-at
   ```

   `upload-artifact` stores both files at the artifact root (their common parent `refs/` is stripped), so every consumer that downloads `image-ref-<image>` into `refs/` gets `refs/<image>.ref` as before. The scan job reads `refs/$IMG.ref` only. `validate-image-refs.sh`, `cleanup-failed-digests.sh` and the publish loops glob `*.ref` only. Check this:

   ```bash
   grep -n '\*\.ref\|\$IMG\.ref' .github/workflows/runtime-images.yml .github/scripts/validate-image-refs.sh .github/scripts/cleanup-failed-digests.sh
   ```

   Every hit must be a `*.ref` glob or an exact `<img>.ref` path.

- [ ] **Step 9: Publish: validate the build times, refuse a stale scan, write the real builtAt**

In the `publish` job of `.github/workflows/runtime-images.yml`:

1. Step `validate refs + SBOMs (strict repo@sha256)`: append to its `run: |` block, after the SBOM `for` loop:

   ```bash
             # R1-09: every image must carry its build job's recorded time.
             .github/scripts/earliest-built-at.sh refs linux-base linux-desktop browser >/dev/null
   ```

2. Insert this new step directly after `validate refs + SBOMs (strict repo@sha256)` and before `login to ghcr.io`:

   ```yaml
         - name: refuse a stale scan (older than SCAN_MAX_AGE_HOURS)
           # R1-09: the runtime-train approval can come days after the scan,
           # and "re-run failed jobs" re-enters this job without rescanning.
           # Sign only digests whose trivy gate finished within the bound.
           # Completion times come from the jobs API (latest attempt per
           # job), not from any artifact of this run. Scanners never run in
           # this job (SEC-05) — the bound replaces a rescan here.
           env:
             GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
             GH_REPO: ${{ github.repository }}
           run: |
             gh api --paginate \
               "repos/$GH_REPO/actions/runs/$GITHUB_RUN_ID/jobs?filter=latest&per_page=100" \
               --jq '.jobs[]' | jq -s . > jobs.json
             .github/scripts/check-scan-age.sh jobs.json "$SCAN_MAX_AGE_HOURS" \
               "scan linux-base" "scan linux-desktop" "scan browser"
   ```

   The scan job's `name: scan ${{ matrix.image }}` renders as these three job names in the API.

3. Step `write runtime-images.json`: replace its comment and `run:` with:

   ```yaml
         - name: write runtime-images.json
           # Engine versions come from the built images themselves — the
           # SPDX SBOMs the build jobs generated and this job just attested
           # (SUPR-2). builtAt is the earliest build-job time of the three
           # images (the same second as their org.opencontainers.image.created
           # label), never the publish clock (R1-09).
           run: |
             BUILT_AT="$(.github/scripts/earliest-built-at.sh refs linux-base linux-desktop browser)"
             export BUILT_AT
             .github/scripts/write-runtime-manifest.sh refs sboms runtime-images.json
             jq . runtime-images.json
   ```

   `jobs.json` stays in the workspace root. The release step uploads only `runtime-images.json` and its bundle by explicit path, so it does not ride along.

- [ ] **Step 10: Document it**

In `.github/README.md`, directly after the paragraph that ends "``linux-base` carries no browser.`" (line 270) and before "Chart values stay GitOps-owned (P7)", add:

```markdown
`builtAt` is the earliest build time of the train's three images. Each
build job records one UTC second, stamps it into the image's
`org.opencontainers.image.created` label and writes it as
`<image>.built-at` next to the image ref. `write-runtime-manifest.sh`
refuses to run without it, so `builtAt` is never the publish time. The
publish job also refuses to sign when any scan leg finished more than
`SCAN_MAX_AGE_HOURS` (24) hours earlier (`check-scan-age.sh`, reading
the run's jobs API). An approval granted later than that, or a re-run
of an old run's failed publish, fails, and the remedy is a fresh run,
which rebuilds and rescans.
```

- [ ] **Step 11: Run the tests and lint the workflow**

Run:
```bash
for t in write-runtime-manifest check-scan-age release-topology script-checkout workflow-expressions; do bash .github/tests/$t.test.sh >/tmp/t26-$t.log 2>&1; echo "$t rc=$?"; grep -E '^FAIL' /tmp/t26-$t.log; done
python3 -c 'import yaml,sys; yaml.safe_load(open(".github/workflows/runtime-images.yml"))' && echo yaml-ok
command -v actionlint >/dev/null && actionlint .github/workflows/runtime-images.yml && echo actionlint-ok
```
Expected: every `rc=0`, no `FAIL`, `yaml-ok`, and `actionlint-ok` when actionlint is installed. CI's `workflow lint` job runs actionlint, yamllint and zizmor in any case. Then run the whole loop: `for t in .github/tests/*.test.sh; do bash "$t" >/dev/null 2>&1 || echo "FAIL $t"; done`. Expected: no output.

- [ ] **Step 12: Commit**

```bash
git add .github/scripts/earliest-built-at.sh .github/scripts/check-scan-age.sh \
  .github/scripts/write-runtime-manifest.sh .github/tests/check-scan-age.test.sh \
  .github/tests/write-runtime-manifest.test.sh .github/tests/release-topology.test.sh \
  .github/workflows/runtime-images.yml .github/README.md
git commit -m "ci(runtime): carry build time into the train manifest and refuse a stale scan before signing" \
  -m "R1-09 (train): builtAt is the earliest build-job time (image created label + <image>.built-at), never the publish clock; publish refuses a scan leg older than 24 h, read from the jobs API (scanners stay out of the signing job, SEC-05)." \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 27: Release: runtime builtAt stamped; stale scan refused before signing (R1-09, release) — tier: design

**Finding R1-09 (P2, TM-49), release part.** Task 26 fixed the runtime train. Here is the release side.
- **Attack:** "The image-age SLO check is fooled the same way, because it uses the release `createdAt` (check-runtime-image-age.sh). For v* releases, release.yml stamps only digests. The release-built runtime digests appear in no runtime-images.json, so a stock chart has no source for `builtAt`. A seeded `image: browser` template renders without `image-built-at`, and the 45-day block can never trigger for release-pinned runtime images." The release `publish` job also waits on the `release` environment approval, and it signs whatever scan the run produced, however old.
- **Proof:** `stamp-image-digests.sh` run on `values.yaml` with seven release refs: "builtAt unchanged after release stamping", with 7 digests stamped. The stamped chart, rendered with a seeded `image: browser` template, has `image: ghcr.io/tinyorbitvn/tinycdi-browser@sha256:bbbb…` and no `workspaces.cdi.tinyorbit.vn/image-built-at` annotation. `internal/provisioning/service.go:125-128` never blocks on an empty `builtAt` (`raw == ""`).
- **Required fix:** "Have release.yml stamp `images.<key>.builtAt` (and engines) for linuxDesktop and browser from the build time, and have collect-publish-inputs.sh verify it is set. … re-run the trivy gate against the exact digests just before `cosign sign`, or fail when scan completion is older than N hours (for example 24 h). Apply the same to release.yml `publish`."
- **Security test:** "`release-topology.test.sh`: release stamps `builtAt` for runtime images and publish rescans (or checks the scan age) before signing." Plus `publish-inputs.test.sh` cases. All fail today.

**Rulings.**
- The scan-age bound reuses Task 26's `check-scan-age.sh`, for the same reason (SEC-05: no scanner in a job holding `id-token: write`).
- `builtAt` is stamped for every values block that has a `builtAt:` field: `images.linuxBase`, `images.linuxDesktop` and `images.browser`.
- `engines.{chromium,firefox}` are **not** stamped. They feed only the stale-image view's labels, while `builtAt` drives the 45-day block. Stamping a YAML map with the awk stamper would need a second parser. The values comment says engines come from a train bump.

**Files:**
- Modify: `.github/scripts/stamp-image-digests.sh:12-55` (stamps `builtAt` from `<image>.built-at`)
- Modify: `.github/scripts/collect-publish-inputs.sh:27-28` (`SCRIPT_DIR` exists), `:62-70` (step 1: `exact_files` plus the build time), `:126-144` (step 5: chart `builtAt` == build time)
- Modify: `.github/tests/publish-inputs.test.sh:28-60` (`build_store` fixtures), `:132-150` (new mutations and `expect_fail` lines)
- Modify: `.github/workflows/release.yml`:
  - workflow `env:` (152-164): add `SCAN_MAX_AGE_HOURS`
  - `base` job: `compute tags + OCI labels` (383-406), `record image ref for downstream jobs` (434-443), `upload image ref` (445-450)
  - `images` job: `compute tags + OCI labels` (565-597), `record image ref for downstream jobs` (660-670), `upload image ref` (672-677)
  - `publish` job: a new step between `validate publish inputs (strict allowlist)` (1169-1181) and `login to ghcr.io` (1183)
- Modify: `.github/tests/release-topology.test.sh` (after line 61: `chk "release: digest stamping script wired" …`)
- Modify: `deploy/helm/tinycdi/values.yaml:46-50` (the `builtAt` comment), `deploy/helm/tinycdi/README.md:89` (the `images.<name>.builtAt` row), `.github/README.md` ("Release protection", after the code block that ends at line 118)

**Interfaces:**
- Consumes (Task 26): `.github/scripts/earliest-built-at.sh <refs-dir> <image>...` and `.github/scripts/check-scan-age.sh <jobs.json> <max-age-hours> <scan job name>...`. The `image-ref-<image>` artifact layout is `<image>.ref` plus `<image>.built-at`.
- Produces: in `release.yml`, every `image-ref-<image>` artifact holds `<image>.ref` and `<image>.built-at`. Dry runs write the file too, so the layout never depends on the mode.
- Produces: `stamp-image-digests.sh <values.yaml> <refs-dir>` also stamps `<section>.<key>.builtAt` when that block has a `builtAt:` field. It fails when such a block's image has no valid `<image>.built-at`.
- Produces: `collect-publish-inputs.sh` requires `image-ref-<img>` to hold exactly `<img>.ref` and `<img>.built-at`. It requires the chart's `builtAt` for `linux-base`, `linux-desktop` and `browser` to equal that file. Its error texts contain `build time` and `builtAt`.
- Produces: the `release.yml` publish step `refuse a stale scan (older than SCAN_MAX_AGE_HOURS)`. Its `legs` array is built from `RELEASE_IMAGES` as `scan <img>`. Task 29 appends the `kasm catalog scan (trivy gate + engine floor)` leg to that array.

- [ ] **Step 1: Write the failing collector cases**

In `.github/tests/publish-inputs.test.sh`, inside `build_store`, replace the per-image loop (lines 31-37, from `for img in "${IMGS[@]}"; do` through its `done`) with:

```bash
  for img in "${IMGS[@]}"; do
    mkdir -p "$dl/image-ref-$img" "$dl/sbom-$img"
    echo "ghcr.io/tinyorbitvn/tinycdi-$img@$(digest_for "$img")" \
      > "$dl/image-ref-$img/$img.ref"
    # R1-09: each build job records its build second next to the ref.
    echo "2026-10-20T03:10:00Z" > "$dl/image-ref-$img/$img.built-at"
    echo "{\"spdxVersion\":\"SPDX-2.3\",\"name\":\"$img\"}" \
      > "$dl/sbom-$img/sbom-$img.spdx.json"
  done
```

Replace the values fixture block (lines 48-56, from `{` through `} > "$cd_/tinycdi/values.yaml"`) with this one, which gives the three runtime blocks a `builtAt` field as the real `values.yaml` does:

```bash
  {
    echo "images:"
    for img in backend browser frontend operator; do
      printf '  %s:\n    repository: tinyorbitvn/tinycdi-%s\n    digest: ""\n' "$img" "$img"
      if [ "$img" = browser ]; then printf '    builtAt: ""\n'; fi
    done
    printf '  linuxBase:\n    repository: tinyorbitvn/tinycdi-linux-base\n    digest: ""\n    builtAt: ""\n'
    printf '  linuxDesktop:\n    repository: tinyorbitvn/tinycdi-linux-desktop\n    digest: ""\n    builtAt: ""\n'
    printf 'kasmAdapter:\n  image:\n    repository: tinyorbitvn/tinycdi-kasm-adapter\n    digest: ""\n'
  } > "$cd_/tinycdi/values.yaml"
```

Replace the `src_refs` copy loop (lines 59-61) with:

```bash
  for img in "${IMGS[@]}"; do
    cp "$dl/image-ref-$img/$img.ref" "$dl/image-ref-$img/$img.built-at" "$src_refs/"
  done
```

After the `mut_stowaway_bundle` definition (line 156), add:

```bash
# R1-09: the build time rides with the ref and the chart's runtime
# builtAt must equal it — never empty, never the publish clock.
mut_no_built_at()   { rm -f "$1/image-ref-browser/browser.built-at"; }
mut_bad_built_at()  { echo "yesterday" > "$1/image-ref-browser/browser.built-at"; }
mut_builtat_drift() { echo "2026-10-21T00:00:00Z" > "$1/image-ref-linux-desktop/linux-desktop.built-at"; }
mut_unstamped_builtat() {
  # a chart whose runtime builtAt fields were left empty
  local cd_="$WORK/chart-unstamped"
  rm -rf "$cd_"; mkdir -p "$cd_"
  tar -xzf "$1/release-chart/tinycdi-$CHART_VERSION.tgz" -C "$cd_"
  sed -i 's/^    builtAt: .*/    builtAt: ""/' "$cd_/tinycdi/values.yaml"
  rm -f "$1/release-chart/tinycdi-$CHART_VERSION.tgz"
  tar -czf "$1/release-chart/tinycdi-$CHART_VERSION.tgz" -C "$cd_" tinycdi
}
```

After `expect_fail "stowaway in chart artifact" "release-chart" mut_stowaway_bundle`, add:

```bash
expect_fail "missing build time"           "image-ref-browser" mut_no_built_at
expect_fail "malformed build time"         "build time"        mut_bad_built_at
expect_fail "chart builtAt != build time"  "builtAt"           mut_builtat_drift
expect_fail "runtime builtAt left empty"   "builtAt"           mut_unstamped_builtat
```

- [ ] **Step 2: Write the failing topology assertions**

In `.github/tests/release-topology.test.sh`, after line 61 (`chk "release: digest stamping script wired" "$REL" 'stamp-image-digests.sh'`), add:

```bash
# R1-09: release builds record their build second, the chart stamps it
# as the runtime images' builtAt, and publish refuses a stale scan.
chk "release: base records its build time" "$REL" 'refs/linux-base\.built-at'
chk "release: image legs record their build time" "$REL" 'refs/\$IMG\.built-at'
chk "release: label created = recorded build time" "$REL" 'org\.opencontainers\.image\.created=\$BUILT_AT'
chk "release: stamper writes builtAt" "$ROOT/.github/scripts/stamp-image-digests.sh" 'builtAt'
chk "release: collector verifies builtAt" "$ROOT/.github/scripts/collect-publish-inputs.sh" 'builtAt'
chk "release: scan age bound wired" "$REL" 'check-scan-age\.sh jobs\.json "\$SCAN_MAX_AGE_HOURS"'
order "release: scan age checked before signing" "$REL" 'check-scan-age\.sh jobs\.json' 'name: cosign sign images'
```

- [ ] **Step 3: Run them and see them fail**

Run: `for t in publish-inputs release-topology; do bash .github/tests/$t.test.sh 2>&1 | grep -E '^FAIL'; done`

Expected: `FAIL: happy path rejected:`. The collector refuses the new `.built-at` file with `image-ref-backend holds [backend.built-at backend.ref] — expected exactly [backend.ref]`, so most existing `expect_fail` cases also report the wrong message. Among them are the four new ones: `FAIL (missing build time): …`, `FAIL (malformed build time): …`, `FAIL (chart builtAt != build time): …` and `FAIL (runtime builtAt left empty): …`. The run ends with the seven new `FAIL: release: …` lines.

- [ ] **Step 4: Stamp `builtAt` in `stamp-image-digests.sh`**

In `.github/scripts/stamp-image-digests.sh`:
- Header: after the `#   "local" for unpushed rehearsal builds (skipped).` line (line 11), add:

  ```bash
  #   Next to each ref sits <image>.built-at (the build job's recorded UTC
  #   second, R1-09). A values block that has a builtAt field (the runtime
  #   images) gets it stamped; such a block without a valid .built-at fails.
  ```

- After `set -euo pipefail` (line 12), add `SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"`.
- Inside the `for f in "$REFS_DIR"/*.ref; do` loop, after `echo "stamped $section.$key.digest = $digest"` (line 54) and before `done`, add:

```bash
  # R1-09: runtime images carry builtAt — the stale-image block reads it
  # (image-built-at annotation). Stamp the build job's recorded time.
  if awk -v section="$section" -v key="$key" '
       /^[^ #]/                  { in_sec = ($0 ~ ("^" section ":")) }
       in_sec && /^  [a-zA-Z]+:/ { cur = substr($1, 1, length($1) - 1) }
       in_sec && cur == key && /^    builtAt:/ { found = 1 }
       END { exit !found }' "$VALUES"; then
    built_at="$("$SCRIPT_DIR/earliest-built-at.sh" "$REFS_DIR" "$name")" \
      || { echo "::error::$section.$key has a builtAt field but $name has no valid build time"; exit 1; }
    awk -v section="$section" -v key="$key" -v ba="$built_at" '
      /^[^ #]/              { in_sec = ($0 ~ ("^" section ":")) }
      in_sec && /^  [a-zA-Z]+:/ { cur = substr($1, 1, length($1) - 1) }
      in_sec && cur == key && /^    builtAt:/ { sub(/builtAt:.*/, "builtAt: \"" ba "\"") }
      { print }
    ' "$VALUES" > "$VALUES.stamped"
    mv "$VALUES.stamped" "$VALUES"
    echo "stamped $section.$key.builtAt = $built_at"
  fi
```

- [ ] **Step 5: Verify the build time and the stamped `builtAt` in the collector**

In `.github/scripts/collect-publish-inputs.sh`:
- Header list (lines 9-14): add the bullet `#   * every image-ref artifact to carry <img>.built-at, and the chart's runtime builtAt to equal it (R1-09)`.
- Step 1: replace `exact_files "$DL/image-ref-$img" "$img.ref"` with:

  ```bash
    exact_files "$DL/image-ref-$img" "$img.ref" "$img.built-at"
    "$SCRIPT_DIR/earliest-built-at.sh" "$DL/image-ref-$img" "$img" >/dev/null \
      || die "image-ref-$img carries no valid build time"
  ```

- Step 5: inside the `for img in "${expected[@]}"; do` loop, after the digest comparison (`|| die "chart $section.$key.digest ($stamped) != $img.ref digest (${ref#*@})"`), add:

```bash
  # R1-09: the runtime images' builtAt must be the build job's time —
  # an empty or publish-time value would defeat the stale-image block.
  ba="$(awk -v section="$section" -v key="$key" '
    /^[^ #]/                          { in_sec = ($0 ~ ("^" section ":")) }
    in_sec && /^  [a-zA-Z]+:/         { cur = substr($1, 1, length($1) - 1) }
    in_sec && cur == key && /^    builtAt:/ {
      v = $2; gsub(/[" ]/, "", v); print "field:" v; exit
    }' "$OUT/.chart-values.yaml")"
  case "$img" in
    linux-base|linux-desktop|browser)
      [ -n "$ba" ] || die "chart $section.$key has no builtAt field — the runtime image age cannot be enforced" ;;
  esac
  if [ -n "$ba" ]; then
    want_ba="$(tr -d '[:space:]' < "$DL/image-ref-$img/$img.built-at")"
    [ "${ba#field:}" = "$want_ba" ] \
      || die "chart $section.$key.builtAt ('${ba#field:}') != $img.built-at ($want_ba) — builtAt was not stamped from the build job"
  fi
```

- [ ] **Step 6: Record the build time in the release build jobs**

In `.github/workflows/release.yml`:

1. Workflow `env:`: after `COSIGN_BIN_SHA256: …` (line 164), add:

   ```yaml
     # R1-09: the publish job refuses a scan leg that finished longer ago
     # than this (check-scan-age.sh) — approve within a day, or re-tag.
     SCAN_MAX_AGE_HOURS: "24"
   ```

2. `base` job, `compute tags + OCI labels` (line 383). Make the first line of `run: |` (before `REF="$IMAGE_NS/tinycdi-linux-base"`) the following. Inside the `{ … }` group, add `echo "BUILT_AT=$BUILT_AT"` directly before `echo "OCI_LABELS<<__EOF__"`. Replace `echo "org.opencontainers.image.created=$(date -u +%Y-%m-%dT%H:%M:%SZ)"` with `echo "org.opencontainers.image.created=$BUILT_AT"`.

   ```bash
             # R1-09: one recorded build second — the image label and the
             # <image>.built-at file the chart and publish jobs read.
             BUILT_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
   ```

3. `base` job, `record image ref for downstream jobs`: after the closing `fi`, add `echo "$BUILT_AT" > refs/linux-base.built-at`. In `upload image ref`, replace `path: refs/linux-base.ref` with:

   ```yaml
             path: |
               refs/linux-base.ref
               refs/linux-base.built-at
   ```

4. `images` job: make the same three edits. In `compute tags + OCI labels` (line 565), add the same `BUILT_AT=…` first line above `REF="$IMAGE_NS/tinycdi-$IMG"`, the same `echo "BUILT_AT=$BUILT_AT"`, and `created=$BUILT_AT`. In `record image ref for downstream jobs`, add `echo "$BUILT_AT" > "refs/$IMG.built-at"` after the `fi`. In `upload image ref`, set:

   ```yaml
             path: |
               refs/${{ matrix.image }}.ref
               refs/${{ matrix.image }}.built-at
   ```

   Consumers that are unaffected, all of which read `<img>.ref` by exact name or `*.ref`: the scan job (`refs/$IMG.ref`), the chart job (`stamp-image-digests.sh` iterates `*.ref` and now also reads `<img>.built-at`), and `cleanup-failed-digests.sh` (`"$REFS"/*.ref`). The `chart` job downloads `image-ref-<img>` into one `refs/` directory (`-D refs`), so the `.built-at` files land beside the refs the stamper reads.

- [ ] **Step 7: Refuse a stale scan before signing**

In the `publish` job, insert after `validate publish inputs (strict allowlist)` and before `login to ghcr.io`:

```yaml
      - name: refuse a stale scan (older than SCAN_MAX_AGE_HOURS)
        # R1-09: the release approval can come days after the scan, and
        # "re-run failed jobs" re-enters this job without rescanning. Sign
        # only digests whose trivy gate finished within the bound.
        # Completion times come from the jobs API (latest attempt per
        # job), not from any artifact of this run. Scanners never run in
        # this job (SEC-05) — the bound replaces a rescan here.
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          GH_REPO: ${{ github.repository }}
        run: |
          gh api --paginate \
            "repos/$GH_REPO/actions/runs/$GITHUB_RUN_ID/jobs?filter=latest&per_page=100" \
            --jq '.jobs[]' | jq -s . > jobs.json
          mapfile -t legs < <(jq -r '.[] | "scan " + .' <<< "$RELEASE_IMAGES")
          .github/scripts/check-scan-age.sh jobs.json "$SCAN_MAX_AGE_HOURS" "${legs[@]}"
```

The scan job's `name: scan ${{ matrix.image }}` (line 736) renders as `scan <img>` for each entry of `RELEASE_IMAGES`. `jobs.json` stays in the workspace root, outside `bundle/`, so the sign-blob and release steps never pick it up.

- [ ] **Step 8: Document it**

- `deploy/helm/tinycdi/values.yaml`: replace the comment lines 46-50 (from `# builtAt is the image build timestamp (RFC 3339) from the runtime` through `# 409 IMAGE_STALE.`) with:

  ```yaml
    # builtAt is the image build timestamp (RFC 3339, UTC): the second the
    # build job recorded and stamped into the image's
    # org.opencontainers.image.created label. A release stamps it here for
    # linuxBase, linuxDesktop and browser; a runtime-train bump copies it
    # from runtime-images.json. It renders as the template's
    # workspaces.cdi.tinyorbit.vn/image-built-at annotation so the API can
    # report imageBuiltAt/imageStale on templates and workspaces. A stale
    # image past -image-block-after (45 d default) refuses create/start with
    # 409 IMAGE_STALE.
  ```

- `deploy/helm/tinycdi/README.md:89`: replace the description cell of the `images.<name>.builtAt` row with:

  ```markdown
  RFC 3339 build time: stamped by the release for the runtime images it ships, or copied from the runtime train's `runtime-images.json` → `image-built-at` annotation; an image older than `-image-block-after` (45 d default) refuses create/start with 409 IMAGE_STALE (runtime images only)
  ```

- `.github/README.md`, section "Release protection": after the code block that ends with `RELEASE_REVIEWERS="vanlongme,alice" .github/scripts/setup-repo-protection.sh --apply` and its closing fence, add:

  ```markdown
  Approve within a day of the scan. Both signing jobs (`release.yml`
  `publish` and `runtime-images.yml` `publish`) run
  `.github/scripts/check-scan-age.sh` before `cosign sign`. It refuses to
  sign when any scan leg of the run finished more than
  `SCAN_MAX_AGE_HOURS` (24) hours earlier, according to the jobs API. A
  late approval, or a re-run of an old run's failed publish, fails; start
  a fresh run instead. The release chart's `images.{linuxBase,linuxDesktop,browser}.builtAt`
  carry each image's recorded build time, and `collect-publish-inputs.sh`
  refuses a chart where they differ from it.
  ```

- [ ] **Step 9: Run the tests**

Run:
```bash
for t in publish-inputs release-topology write-runtime-manifest script-checkout workflow-expressions supply-chain-hardening; do bash .github/tests/$t.test.sh >/tmp/t27-$t.log 2>&1; echo "$t rc=$?"; grep -E '^FAIL' /tmp/t27-$t.log; done
python3 -c 'import yaml; yaml.safe_load(open(".github/workflows/release.yml"))' && echo yaml-ok
command -v actionlint >/dev/null && actionlint .github/workflows/release.yml && echo actionlint-ok
go test -count=1 ./deploy/helm/ 2>&1 | tail -3
```
Expected: every `rc=0` with no `FAIL`, `yaml-ok`, and `ok` for the chart tests. The `values.yaml` comment edit changes no render. If `TestDefaultRender` fails, regenerate the golden per Global Constraints. Then run `for t in .github/tests/*.test.sh; do bash "$t" >/dev/null 2>&1 || echo "FAIL $t"; done`. Expected: no output.

- [ ] **Step 10: Commit**

```bash
git add .github/scripts/stamp-image-digests.sh .github/scripts/collect-publish-inputs.sh \
  .github/tests/publish-inputs.test.sh .github/tests/release-topology.test.sh \
  .github/workflows/release.yml deploy/helm/tinycdi/values.yaml deploy/helm/tinycdi/README.md \
  .github/README.md
git commit -m "ci(release): stamp runtime builtAt and refuse a stale scan before signing" \
  -m "R1-09 (release): build jobs record <image>.built-at (= the image created label); the chart stamps it as images.{linuxBase,linuxDesktop,browser}.builtAt and the publish collector requires it; publish refuses a scan leg older than 24 h (jobs API, SEC-05 kept)." \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 28: Publish gating per job; required checks; unsigned chart never left tagged (R1-38, R1-39, R1-40) — tier: integrate

**Finding R1-38 (P3, TM-49): the tests TM-49 cites do not pin the signing chain.**
- **Attack:** "A careless or compromised change does any of the following, and the required 'workflow policy tests (.github)' check stays green: drops `scan` from the publish job's `needs`, so unscanned digests get signed; removes the release `needs.scan.result == 'success'` guard (the job runs under `always()`); deletes the `refs/heads/main` guard; grants `id-token: write` to an ungated job, such as runtime-images `meta`. The last one lets that job mint the `runtime-images.yml@refs/heads/main` identity without the `runtime-train` approval."
- **Proof:** with those mutations applied, `for t in .github/tests/*.test.sh` gave 26/26 rc=0.
- **Required fix and test:** YAML-parsed per-job assertions: exactly one `id-token: write` job per publishing workflow (images `promote`, release `publish`, runtime-images `publish`); that job needs `scan` and, under `always()`, requires `needs.scan.result == 'success'`; `environment: release` and `environment: runtime-train` on the signers; images `promote` and runtime-images `publish` restricted to `refs/heads/main`; no top-level `id-token`. The new test is `.github/tests/publish-gating.test.sh`.

**Finding R1-39 (P3, TM-49): the codified required-check set is narrower than documented.**
- **Attack:** "README says 'required status checks = every ci.yml job except `kasm adapter contract + catalog scan`'. The script header says every job except the network-dependent ones. The applied list has 8 contexts and also omits soak, fuzz, quickstart and partitioned. A careless change that makes the auth/session fuzzers crash, or breaks the kind end-to-end smoke (including the CHIPS partitioned-cookie smoke), merges with green required checks."
- **Required fix (Plan ruling 6):** required = every ci.yml job except the documented network-dependent `kasm adapter contract + catalog scan`, `quickstart (kind up.sh + portal smoke)` and `partitioned (kind up.sh + CHIPS cookie smoke)`. `go fuzz (auth/session parsing boundaries)` and `soak harness (npm ci, tsc, unit + dry-run)` become required.
- **Security test:** "in `setup-repo-protection.test.sh`, derive the ci.yml job `name:` set with python yaml and assert that `contexts` equals that set minus an explicit, documented exclusion list. It fails today."

**Finding R1-40 (P3, TM-49): the Helm chart is tagged before it is signed.**
- **Attack:** "If Fulcio or Rekor fails at :1324, `oci://ghcr.io/tinyorbitvn/charts/tinycdi:X.Y.Z` stays public and unsigned. Cleanup does not run, because publish is not an ancestor of cleanup. Consumers who do not verify install an unsigned chart. This contradicts the SUPR-13 principle ('a signing failure leaves nothing addressable')."
- **Required fix:** "push the chart without a tag (…), `cosign sign` the digest, and only then tag `X.Y.Z`. Alternatively, on sign failure, delete the just-pushed chart version."
- **Ruling:** the alternative is used. `helm push` always creates the `<version>` tag. Pushing by digest needs an OCI client the runner does not ship (oras or crane), plus a hand-built Helm manifest, which is new unpinned tooling in the one job that holds `id-token: write`. So `.github/scripts/push-sign-chart.sh` signs immediately after the push. If anything fails once the push has started, it deletes the GHCR version this run created (bounded by `RUN_STARTED_AT`, like `cleanup-failed-digests.sh`). The brief's `order` check cannot hold with `helm push`, so the test is a behaviour test of the script with stubbed `helm`, `docker`, `cosign` and `gh`.

Commit subject note: the index subject "…sign the chart before tagging" would misdescribe this mechanism. This task uses `ci: pin publish gating per job, codify required checks, never leave an unsigned chart tagged`, and Task 30 must quote that subject.

**Files:**
- Create: `.github/tests/publish-gating.test.sh`, `.github/scripts/push-sign-chart.sh`, `.github/tests/push-sign-chart.test.sh`
- Modify: `.github/scripts/setup-repo-protection.sh:21-27` (header item 4), `:368-371` (section-4 comment), `:377-386` (`contexts`)
- Modify: `.github/tests/setup-repo-protection.test.sh:138` (the converged stub's `contexts`), plus a new block above the final summary line (`:340`)
- Modify: `.github/README.md:136-142` ("Branch protection on `main`" bullet)
- Modify: `.github/workflows/release.yml:1298-1325` (step `push helm chart to OCI + cosign sign`)
- Modify: `.github/tests/release-topology.test.sh:83-84` at HEAD, 92-93 after Task 27 (the SUPR-1 chart-digest checks)
- Modify: `docs/security/provenance.md:59-61` (the chart-signing sentence)

**Interfaces:**
- Produces: `publish-gating.test.sh [<repo-root>]`. The optional argument runs the guards against a mutated copy, and CI calls it without one.
- Produces: `push-sign-chart.sh <chart.tgz>` with env `HELM`, `CHART_NAME`, `CHART_VERSION`, `CHART_OCI`, `RUN_STARTED_AT` and `GH_TOKEN`. Its stdout is exactly `ref=ghcr.io/<org>/<path>/<chart>@sha256:<64hex>`, and every tool's output goes to stderr. It exits non-zero after rolling back.
- Consumes: the publish job env `CHART_VERSION` and the workflow env `CHART_OCI=oci://ghcr.io/tinyorbitvn/charts` (`release.yml:155`). `steps.chart.outputs.ref` stays the output name (`release.yml:1347`).
- Task 29 changes release `publish.needs`, and `publish-gating.test.sh` keeps passing because it checks only that `scan` is in `needs`.

- [ ] **Step 1: Write the signing-chain guard and show it catches the R1-38 mutations**

Create `.github/tests/publish-gating.test.sh`:

```bash
#!/usr/bin/env bash
# publish-gating.test.sh — R1-38 / TM-49: per-job (YAML-parsed) guards on
# the signing chain. In each publishing workflow exactly one job holds
# `id-token: write`; it needs the scan job (and, under always(), requires
# its success); the release and train signers sit behind their
# environments; the main-channel and train signers run only on
# refs/heads/main; no workflow grants id-token at the top level.
#
# Usage: publish-gating.test.sh [<repo-root>] — the optional root lets a
# reviewer run the guards against a mutated copy of the workflows.
set -uo pipefail

ROOT="${1:-$(cd "$(dirname "$0")/../.." && pwd)}"
fails=0

python3 -I - "$ROOT" <<'PY' || fails=1
import sys
try:
    import yaml
except ImportError:
    print("FAIL: PyYAML is not importable — the signing-chain guards cannot run")
    sys.exit(1)
root = sys.argv[1]
bad = 0
def fail(msg):
    global bad
    print("FAIL:", msg)
    bad = 1
spec = {
    "images.yml":         {"signer": "promote", "env": None,            "main_only": True},
    "release.yml":        {"signer": "publish", "env": "release",       "main_only": False},
    "runtime-images.yml": {"signer": "publish", "env": "runtime-train", "main_only": True},
}
for wf, s in spec.items():
    with open(f"{root}/.github/workflows/{wf}") as fh:
        d = yaml.safe_load(fh)
    jobs = d["jobs"]
    if (d.get("permissions") or {}).get("id-token") == "write":
        fail(f"{wf}: workflow-level id-token: write")
    minting = sorted(j for j, b in jobs.items()
                     if (b.get("permissions") or {}).get("id-token") == "write")
    if minting != [s["signer"]]:
        fail(f"{wf}: id-token: write granted to {minting}, want only [{s['signer']}]")
    sj = jobs.get(s["signer"], {})
    needs = sj.get("needs") or []
    needs = [needs] if isinstance(needs, str) else needs
    if "scan" not in needs:
        fail(f"{wf}: {s['signer']} does not need the scan job (needs={needs})")
    cond = str(sj.get("if") or "")
    if "always()" in cond and "needs.scan.result == 'success'" not in cond:
        fail(f"{wf}: {s['signer']} runs under always() without requiring scan success")
    if s["env"] and sj.get("environment") != s["env"]:
        fail(f"{wf}: {s['signer']} environment={sj.get('environment')!r}, want {s['env']!r}")
    if s["main_only"] and "github.ref == 'refs/heads/main'" not in cond:
        fail(f"{wf}: {s['signer']} is not restricted to refs/heads/main (if={cond!r})")
sys.exit(bad)
PY

if [ "$fails" -eq 0 ]; then echo "publish-gating: signing chain pinned per job"; fi
exit "$fails"
```

Show that it passes on the tree and fails on the mutations the review applied:

```bash
bash .github/tests/publish-gating.test.sh; echo "rc=$?"
M="$(mktemp -d)"; mkdir -p "$M/.github/workflows"
cp .github/workflows/{images,release,runtime-images}.yml "$M/.github/workflows/"
sed -i -e 's/^    needs: \[scan\]$/    needs: [build]/' \
  -e "/^    if: github.ref == 'refs\/heads\/main'$/d" "$M/.github/workflows/images.yml"
sed -i -e 's/^    needs: \[meta, scan\]$/    needs: [meta, profile]/' \
  -e "/^    if: github.ref == 'refs\/heads\/main'$/d" \
  -e '/^      packages: read  # list the day/a\      id-token: write' "$M/.github/workflows/runtime-images.yml"
sed -i -e 's/^    needs: \[prepare, scan, chart, binaries\]$/    needs: [prepare, chart, binaries]/' \
  -e "/^          && needs.scan.result == 'success'$/d" "$M/.github/workflows/release.yml"
bash .github/tests/publish-gating.test.sh "$M"; echo "rc=$?"; rm -rf "$M"
```

Expected: first `publish-gating: signing chain pinned per job` and `rc=0`. Then exactly these seven lines, and `rc=1`:
```
FAIL: images.yml: promote does not need the scan job (needs=['build'])
FAIL: images.yml: promote is not restricted to refs/heads/main (if='')
FAIL: release.yml: publish does not need the scan job (needs=['prepare', 'chart', 'binaries'])
FAIL: release.yml: publish runs under always() without requiring scan success
FAIL: runtime-images.yml: id-token: write granted to ['meta', 'publish'], want only [publish]
FAIL: runtime-images.yml: publish does not need the scan job (needs=['meta', 'profile'])
FAIL: runtime-images.yml: publish is not restricted to refs/heads/main (if='')
```
This is R1-38's "fails before the fix" evidence. Every existing `.github/tests` script stays green on such a mutation, which is the finding itself.

- [ ] **Step 2: Write the failing required-check test**

In `.github/tests/setup-repo-protection.test.sh`, insert directly above the final `if [ "$fails" -eq 0 ]; then echo "setup-repo-protection: all guards pass"; fi`:

```bash
# R1-39 / TM-49: the required contexts are exactly the ci.yml job names
# minus a documented list of network-dependent jobs, and every excluded
# job is named in the script header and in .github/README.md.
EXCLUDED=(
  "kasm adapter contract + catalog scan"
  "quickstart (kind up.sh + portal smoke)"
  "partitioned (kind up.sh + CHIPS cookie smoke)"
)
python3 -I - "$ROOT" "${EXCLUDED[@]}" <<'PY' || fails=1
import json, re, sys
try:
    import yaml
except ImportError:
    print("FAIL: PyYAML is not importable — cannot derive the ci.yml job set")
    sys.exit(1)
root, excluded = sys.argv[1], set(sys.argv[2:])
with open(f"{root}/.github/workflows/ci.yml") as fh:
    ci = yaml.safe_load(fh)
jobs = {b.get("name", k) for k, b in ci["jobs"].items()}
with open(f"{root}/.github/scripts/setup-repo-protection.sh") as fh:
    src = fh.read()
m = re.search(r"cat > \"\$TMP/protection\.json\" <<'EOF'\n(.*?)\nEOF\n", src, re.S)
if not m:
    print("FAIL: protection.json heredoc not found in setup-repo-protection.sh")
    sys.exit(1)
contexts = set(json.loads(m.group(1))["required_status_checks"]["contexts"])
bad = 0
for x in sorted(excluded - jobs):
    print(f"FAIL: documented exclusion '{x}' is not a ci.yml job name"); bad = 1
for x in sorted(jobs - excluded - contexts):
    print(f"FAIL: ci.yml job '{x}' is neither a required check nor a documented exclusion"); bad = 1
for x in sorted(contexts - jobs):
    print(f"FAIL: required context '{x}' is not a ci.yml job name"); bad = 1
for x in sorted(contexts & excluded):
    print(f"FAIL: network-dependent job '{x}' is required"); bad = 1
sys.exit(bad)
PY
for x in "${EXCLUDED[@]}"; do
  grep -qF -- "\"$x\"" "$SCRIPT" \
    || { echo "FAIL: setup-repo-protection.sh header does not name excluded job \"$x\""; fails=1; }
  grep -qF -- "\`$x\`" "$ROOT/.github/README.md" \
    || { echo "FAIL: .github/README.md does not name excluded job \`$x\`"; fails=1; }
done
```

Run: `bash .github/tests/setup-repo-protection.test.sh 2>&1 | grep FAIL`
Expected (at HEAD):
```
FAIL: ci.yml job 'go fuzz (auth/session parsing boundaries)' is neither a required check nor a documented exclusion
FAIL: ci.yml job 'soak harness (npm ci, tsc, unit + dry-run)' is neither a required check nor a documented exclusion
FAIL: setup-repo-protection.sh header does not name excluded job "kasm adapter contract + catalog scan"
FAIL: setup-repo-protection.sh header does not name excluded job "quickstart (kind up.sh + portal smoke)"
FAIL: .github/README.md does not name excluded job `quickstart (kind up.sh + portal smoke)`
FAIL: setup-repo-protection.sh header does not name excluded job "partitioned (kind up.sh + CHIPS cookie smoke)"
FAIL: .github/README.md does not name excluded job `partitioned (kind up.sh + CHIPS cookie smoke)`
```

- [ ] **Step 3: Write the failing chart push/sign test**

Create `.github/tests/push-sign-chart.test.sh`:

```bash
#!/usr/bin/env bash
# push-sign-chart.test.sh — R1-40 / SUPR-13: the release chart is signed
# right after `helm push`, and any failure once the push has started
# deletes exactly the GHCR version this run created, so no unsigned
# chart stays tagged. A version that predates the run is never deleted.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SCRIPT="$ROOT/.github/scripts/push-sign-chart.sh"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
mkdir -p "$D/bin"; LOG="$D/calls.log"
fails=0
DG="sha256:$(printf 'c%.0s' $(seq 64))"

cat > "$D/bin/helm" <<'SH'
#!/usr/bin/env bash
echo "helm $*" >> "$STUB_LOG"
[ -n "${STUB_HELM_FAIL:-}" ] && { echo "Error: push failed" >&2; exit 1; }
echo "Pushed: ghcr.io/test/charts/tinycdi:1.0.1"
SH
cat > "$D/bin/docker" <<'SH'
#!/usr/bin/env bash
echo "docker $*" >> "$STUB_LOG"
[ -n "${STUB_NO_DIGEST:-}" ] && exit 1
printf 'Name:      %s\nMediaType: application/vnd.oci.image.manifest.v1+json\nDigest:    %s\n' "${@: -1}" "$STUB_DIGEST"
SH
cat > "$D/bin/cosign" <<'SH'
#!/usr/bin/env bash
echo "cosign $*" >> "$STUB_LOG"
[ -n "${STUB_COSIGN_FAIL:-}" ] && { echo "Error: signing failed (rekor 503)" >&2; exit 1; }
echo "tlog entry created"
SH
cat > "$D/bin/gh" <<'SH'
#!/usr/bin/env bash
echo "gh $*" >> "$STUB_LOG"
[ "$1" = api ] || exit 2; shift
if [ "$1" = "-X" ]; then exit 0; fi
jqexpr=""; while [ $# -gt 0 ]; do case "$1" in --jq) jqexpr="$2"; shift 2;; *) shift;; esac; done
jq -r "$jqexpr" "$STUB_VERSIONS"
SH
chmod +x "$D/bin/"*

# versions: 42 = this run's push of 1.0.1; 7 = an older 1.0.0; 9 = a
# 1.0.1 pushed before the run (an identical re-push answers with it).
cat > "$D/versions-fresh.json" <<'J'
[{"id":42,"created_at":"2026-10-14T10:05:00Z","metadata":{"container":{"tags":["1.0.1"]}}},
 {"id":7,"created_at":"2026-09-01T00:00:00Z","metadata":{"container":{"tags":["1.0.0"]}}}]
J
cat > "$D/versions-old.json" <<'J'
[{"id":9,"created_at":"2026-10-13T00:00:00Z","metadata":{"container":{"tags":["1.0.1"]}}}]
J
echo chart > "$D/tinycdi-1.0.1.tgz"

run() { # run <versions-file> [ENV=VAL...] -> rc, out (stdout); overrides last
  local v="$1"; shift
  : > "$LOG"
  out="$(env PATH="$D/bin:$PATH" STUB_LOG="$LOG" STUB_DIGEST="$DG" STUB_VERSIONS="$D/$v" \
    HELM="$D/bin/helm" CHART_NAME=tinycdi CHART_VERSION=1.0.1 \
    CHART_OCI=oci://ghcr.io/test/charts RUN_STARTED_AT=2026-10-14T10:00:00Z "$@" \
    bash "$SCRIPT" "$D/tinycdi-1.0.1.tgz" 2>"$D/err")"; rc=$?
}
deleted() { grep -E '^gh api -X DELETE ' "$LOG" | sed -E 's#.*/versions/##'; }

# 1. happy path: push, sign by digest, print only the ref, delete nothing.
run versions-fresh.json
[ "$rc" -eq 0 ] || { echo "FAIL: happy path exited $rc: $(cat "$D/err")"; fails=1; }
[ "$out" = "ref=ghcr.io/test/charts/tinycdi@$DG" ] \
  || { echo "FAIL: stdout is not exactly the ref line: '$out'"; fails=1; }
grep -q "^cosign sign --yes ghcr.io/test/charts/tinycdi@$DG\$" "$LOG" \
  || { echo "FAIL: chart not signed by digest"; fails=1; }
[ -z "$(deleted)" ] || { echo "FAIL: happy path deleted versions: $(deleted)"; fails=1; }

# 2. signing fails: this run's 1.0.1 (id 42) is deleted, nothing else.
run versions-fresh.json STUB_COSIGN_FAIL=1
[ "$rc" -ne 0 ] || { echo "FAIL: cosign failure did not fail the step"; fails=1; }
[ "$(deleted)" = "42" ] || { echo "FAIL: unsigned chart not rolled back (deleted='$(deleted)')"; fails=1; }
grep -q "packages/container/charts%2Ftinycdi/versions" "$LOG" \
  || { echo "FAIL: version lookup not on package charts%2Ftinycdi"; fails=1; }

# 3. digest lookup fails after the push: same rollback.
run versions-fresh.json STUB_NO_DIGEST=1
{ [ "$rc" -ne 0 ] && [ "$(deleted)" = "42" ]; } \
  || { echo "FAIL: unresolved digest did not roll back (rc=$rc deleted='$(deleted)')"; fails=1; }

# 4. helm push itself fails: whatever it may have tagged this run goes.
run versions-fresh.json STUB_HELM_FAIL=1
{ [ "$rc" -ne 0 ] && [ "$(deleted)" = "42" ]; } \
  || { echo "FAIL: failed push did not roll back (rc=$rc deleted='$(deleted)')"; fails=1; }

# 5. a 1.0.1 version that predates the run is never deleted.
run versions-old.json STUB_COSIGN_FAIL=1
{ [ "$rc" -ne 0 ] && [ -z "$(deleted)" ]; } \
  || { echo "FAIL: pre-run version touched (rc=$rc deleted='$(deleted)')"; fails=1; }

# 6. a malformed CHART_OCI is refused before anything is pushed.
run versions-fresh.json CHART_OCI=oci://docker.io/x
{ [ "$rc" -ne 0 ] && ! grep -q '^helm push' "$LOG"; } \
  || { echo "FAIL: foreign CHART_OCI accepted"; fails=1; }

if [ "$fails" -eq 0 ]; then echo "push-sign-chart: all cases pass"; fi
exit "$fails"
```

Run: `bash .github/tests/push-sign-chart.test.sh | head -3`
Expected: `FAIL: happy path exited 127: bash: …/push-sign-chart.sh: No such file or directory` and further `FAIL` lines.

- [ ] **Step 4: Codify the required checks (R1-39)**

In `.github/scripts/setup-repo-protection.sh`:
1. Replace header lines 21-27, from `#   4. Branch protection on `main` — required status checks = every ci.yml` through `#      workflow-policy job via check-kasm-catalog.sh). Enforce admins,`, with the block below. Line 28 (`#      dismiss stale reviews. Single maintainer today, so`) continues it unchanged. Keep each job name on one line, because the test greps for it, and do not write the phrase `chromium apt-pin freshness`: `release-topology.test.sh:291` refuses it in this file.

```bash
#   4. Branch protection on `main` — required status checks = every ci.yml
#      job EXCEPT the network-dependent ones (SUPF-4), which pull
#      multi-GB images or boot a kind cluster, so an outage upstream
#      would hold every merge hostage:
#        - "kasm adapter contract + catalog scan" (pulls the Docker Hub
#          kasmweb catalog images; main pushes, the weekly schedule and
#          the kasm path gate on PRs still run it, and the declared
#          catalog floor is checked by the required workflow-policy job);
#        - "quickstart (kind up.sh + portal smoke)";
#        - "partitioned (kind up.sh + CHIPS cookie smoke)".
#      .github/tests/setup-repo-protection.test.sh derives the ci.yml job
#      names and fails when a job is neither required nor listed here.
#      Enforce admins,
```

2. Replace the section-4 comment (lines 369-371, the three lines under the `# --- 4. branch protection on main ---` banner at 368, which stays) with:

```bash
# SUPF-4: required_approving_review_count 0 while the repo has a single
# maintainer; the contexts are every ci.yml job except the network-dependent
# ones listed in the header (item 4) — R1-39.
```

3. In the `protection.json` heredoc, replace `      "workflow lint (actionlint + yamllint + zizmor)"` (the last context, line 385) with:

```json
      "workflow lint (actionlint + yamllint + zizmor)",
      "soak harness (npm ci, tsc, unit + dry-run)",
      "go fuzz (auth/session parsing boundaries)"
```

Both new jobs report success when their PR path gate skips the work (`fuzz`), or run in a few minutes (`soak`), so requiring them does not slow unrelated merges.

4. In `.github/tests/setup-repo-protection.test.sh` line 138 (the `converged` branch-protection stub), replace `"dependency review (PRs)","workflow policy tests (.github)"],"checks":[]}` with `"dependency review (PRs)","workflow policy tests (.github)","soak harness (npm ci, tsc, unit + dry-run)","go fuzz (auth/session parsing boundaries)"],"checks":[]}`. A converged repo then matches the new body, and the converged and `err500*` scenarios plan no protection change.

5. In `.github/README.md`, replace the first seven lines of the "Branch protection on `main`" bullet (lines 136-142, from `- **Branch protection on `main`** — required status checks = every ci.yml` through `  Strict (up-to-date) mode, enforce admins, dismiss stale`) with the following. The rest of the bullet (`  reviews, conversation resolution required, …`) continues unchanged:

```markdown
- **Branch protection on `main`** — required status checks = every ci.yml
  job except the network-dependent ones, which pull multi-GB images or
  boot a kind cluster:
  `kasm adapter contract + catalog scan`,
  `quickstart (kind up.sh + portal smoke)` and
  `partitioned (kind up.sh + CHIPS cookie smoke)`.
  They still run in CI (the kind smokes on every push and pull request,
  the kasm scan on main pushes, weekly and on PRs touching the kasm
  paths) but do not block a merge; the fast kasm catalog policy leg runs
  inside the required workflow-policy job.
  `setup-repo-protection.test.sh` fails when a ci.yml job is neither
  required nor on this list. The browser engine freshness check lives in
  `runtime-freshness.yml` (daily), outside ci.yml, so it is never a
  required check. Strict (up-to-date) mode, enforce admins, dismiss stale
```

Applying the new context set to the live repository (`setup-repo-protection.sh --apply`) is an owner step after the public-repo reset. This task changes only the script.

- [ ] **Step 5: Never leave an unsigned chart tagged (R1-40)**

Create `.github/scripts/push-sign-chart.sh` (mode 0755):

```bash
#!/usr/bin/env bash
# push-sign-chart.sh — push the packaged Helm chart to GHCR and sign its
# digest keyless; never leave an unsigned chart version tagged (R1-40,
# SUPR-13).
#
# `helm push` uploads the chart and creates the <chart>:<version> tag in
# one call, so the chart cannot be signed before it is tagged. This script
# closes that window from the other side: when anything fails once the
# push has started (the push itself, the digest lookup, `cosign sign`), it
# deletes the GHCR package version tagged <version> that THIS run created
# (the tag goes with it) and exits non-zero. A version created before the
# run (created_at < RUN_STARTED_AT) is never deleted. With the version
# gone, the prepare job's existence check lets a re-run publish it again.
#
# Usage: push-sign-chart.sh <chart.tgz>
# Env:   HELM            helm binary
#        CHART_NAME      chart name (Chart.yaml name:)
#        CHART_VERSION   X.Y.Z — the tag helm push creates
#        CHART_OCI       oci://ghcr.io/<org>/<path>
#        RUN_STARTED_AT  ISO 8601 start of this run (deletion bound)
#        GH_TOKEN        token with packages: write (list + delete versions)
# Stdout: exactly one line, ref=ghcr.io/<org>/<path>/<chart>@sha256:<64hex>,
#         for $GITHUB_OUTPUT. Every tool's own output goes to stderr.
set -euo pipefail

PKG="${1:?usage: push-sign-chart.sh <chart.tgz>}"
: "${HELM:?}" "${CHART_NAME:?}" "${CHART_VERSION:?}" "${CHART_OCI:?}" "${RUN_STARTED_AT:?}"
[ -f "$PKG" ] || { echo "::error::chart package '$PKG' not found" >&2; exit 1; }
[[ "$CHART_OCI" =~ ^oci://ghcr\.io/([a-z0-9][a-z0-9-]*)/([a-z0-9][a-z0-9/_.-]*)$ ]] \
  || { echo "::error::CHART_OCI '$CHART_OCI' is not oci://ghcr.io/<org>/<path>" >&2; exit 1; }
ORG="${BASH_REMATCH[1]}"
PKG_PATH="${BASH_REMATCH[2]}/$CHART_NAME"
REPO="ghcr.io/$ORG/$PKG_PATH"
PKG_ENC="${PKG_PATH//\//%2F}"

state="start"   # start -> pushing -> signed
rollback() {
  [ "$state" = "pushing" ] || return 0
  echo "::error::$REPO:$CHART_VERSION is not signed — deleting the version this run pushed" >&2
  local ids id
  if ! ids="$(gh api --paginate "orgs/$ORG/packages/container/$PKG_ENC/versions?per_page=100" \
      --jq ".[] | select((.metadata.container.tags // []) | index(\"$CHART_VERSION\"))
                | select(.created_at >= \"$RUN_STARTED_AT\") | .id")"; then
    echo "::error::could not list $PKG_PATH versions — delete $REPO:$CHART_VERSION by hand" >&2
    return 0
  fi
  if [ -z "$ids" ]; then
    echo "::warning::no $REPO:$CHART_VERSION version created by this run — nothing to delete" >&2
    return 0
  fi
  for id in $ids; do
    if gh api -X DELETE "orgs/$ORG/packages/container/$PKG_ENC/versions/$id" >/dev/null; then
      echo "deleted unsigned chart version $id ($REPO:$CHART_VERSION)" >&2
    else
      echo "::error::could not delete version $id — delete $REPO:$CHART_VERSION by hand" >&2
    fi
  done
}
trap rollback EXIT

state="pushing"
"$HELM" push "$PKG" "$CHART_OCI" >&2
# SUPR-1: helm 4 writes Pushed:/Digest: to stderr — resolve the digest
# from the registry by tag instead of parsing streams.
digest="$(docker buildx imagetools inspect "$REPO:$CHART_VERSION" | awk '/^Digest:/ {print $2; exit}')"
[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] \
  || { echo "::error::could not resolve pushed chart digest from the registry" >&2; exit 1; }
# SEC-17: sign the pushed chart manifest by digest (keyless).
cosign sign --yes "$REPO@$digest" >&2
state="signed"
echo "ref=$REPO@$digest"
```

In `.github/workflows/release.yml`, replace the whole step `push helm chart to OCI + cosign sign` (lines 1298-1325) with:

```yaml
      - name: push helm chart to OCI + cosign sign
        id: chart
        # R1-40 / SUPR-13: `helm push` tags X.Y.Z in the same call that
        # uploads the chart, so the chart is signed right after the push and
        # push-sign-chart.sh deletes the version this run created if the
        # push, the digest lookup or the signature fails — an unsigned chart
        # never stays tagged. Stdout is only the ref= line.
        env:
          GHCR_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          GHCR_USER: ${{ github.actor }}
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: |
          printf '%s' "$GHCR_TOKEN" \
            | "$PWD/bin/helm" registry login ghcr.io \
              --username "$GHCR_USER" --password-stdin
          CHART_NAME="$(awk '/^name:/ {print $2; exit}' deploy/helm/tinycdi/Chart.yaml)"
          shopt -s nullglob
          pkgs=(bundle/*.tgz)
          [ "${#pkgs[@]}" -eq 1 ] \
            || { echo "::error::expected exactly one chart tgz, got ${#pkgs[@]}"; exit 1; }
          RUN_STARTED_AT="$(gh api "repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID" \
            --jq .run_started_at)"
          HELM="$PWD/bin/helm" CHART_NAME="$CHART_NAME" RUN_STARTED_AT="$RUN_STARTED_AT" \
            .github/scripts/push-sign-chart.sh "${pkgs[0]}" >> "$GITHUB_OUTPUT"
```

The job already holds `packages: write` (deleting a version) and checks out the repo. `CHART_VERSION` (job env) and `CHART_OCI` (workflow env) reach the script through the environment.

In `.github/tests/release-topology.test.sh`, replace the two lines (83-84 at HEAD; 92-93 after Task 27's insertion) (the `chk "release: chart digest via imagetools inspect" …` and `chk_absent "release: no stdout digest parse" …`) with:

```bash
chk "release: chart digest via imagetools inspect" "$ROOT/.github/scripts/push-sign-chart.sh" 'imagetools inspect'
chk_absent "release: no stdout digest parse" "$REL" 'out=.*helm.*push|helm push.*\$\('
# R1-40: the chart is pushed and signed by push-sign-chart.sh, which rolls
# back (deletes this run's version) when signing fails.
chk "release: chart pushed + signed by push-sign-chart.sh" "$REL" 'push-sign-chart\.sh "\$\{pkgs\[0\]\}"'
chk_absent "release: no inline helm push (rollback lives in the script)" "$REL" 'bin/helm" push'
```

In `docs/security/provenance.md`, after the sentence that ends `then `gh release create`.` (line 61), add:

```markdown
`helm push` tags the chart in the same call that uploads it, so the
chart is the one release artifact that exists tagged before it is
signed. `push-sign-chart.sh` signs it right after the push. If the push,
the digest lookup or the signature fails, it deletes the chart version
that run created, so an unsigned chart never stays tagged.
```

- [ ] **Step 6: Run the tests and lint**

Run:
```bash
for t in publish-gating setup-repo-protection push-sign-chart release-topology docs-gates; do bash .github/tests/$t.test.sh >/tmp/t28-$t.log 2>&1; echo "$t rc=$?"; grep -E '^FAIL' /tmp/t28-$t.log; done
python3 -c 'import yaml; yaml.safe_load(open(".github/workflows/release.yml"))' && echo yaml-ok
command -v actionlint >/dev/null && actionlint .github/workflows/release.yml && echo actionlint-ok
bash -n .github/scripts/setup-repo-protection.sh && echo syntax-ok
```
Expected: every `rc=0`, no `FAIL`, `yaml-ok` and `syntax-ok`. Then run `for t in .github/tests/*.test.sh; do bash "$t" >/dev/null 2>&1 || echo "FAIL $t"; done`. Expected: no output.

- [ ] **Step 7: Commit**

```bash
git add .github/tests/publish-gating.test.sh .github/tests/push-sign-chart.test.sh \
  .github/scripts/push-sign-chart.sh .github/scripts/setup-repo-protection.sh \
  .github/tests/setup-repo-protection.test.sh .github/tests/release-topology.test.sh \
  .github/workflows/release.yml .github/README.md docs/security/provenance.md
git commit -m "ci: pin publish gating per job, codify required checks, never leave an unsigned chart tagged" \
  -m "R1-38: publish-gating.test.sh pins id-token/needs/environment/main guards per job. R1-39: required contexts = ci.yml jobs minus the documented network-dependent ones (fuzz + soak added), tested. R1-40: push-sign-chart.sh signs right after helm push and deletes this run's chart version on failure." \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 29: Kasm catalog scan gates releases; no shared cache for main images (R1-37, R1-41) — tier: integrate

**Finding R1-37 (P3, TM-48): the kasm engine-floor and trivy gate are advisory, not blocking, at merge or release.**
- **Attack:** "A careless or compromised PR adds a `kasmweb/<x>@sha256:…` catalog entry. The entry declares `chromium 155 browser` while really bundling Chromium 130, or carries fixable CRITICALs. The PR also lists the image on `kasmAdapter.browserAllowlist`. check-kasm-catalog.sh, the only required kasm check, passes because it trusts the declared floor. The kasm-contract job measures `--version` and goes red, but it is not a required context, so the PR merges. A tag push then ships a chart whose allowlist admits the stale image. release.yml re-runs neither catalog script."
- **Required fix (option a, Plan ruling 1):** "run `scan-kasm-catalog.sh` … in release.yml … before publish, fail closed." Merges stay non-gated by the network-dependent kasm job, which is the owner decision Task 28 codifies.
- **Security test:** in `release-topology.test.sh`, "assert release.yml invokes `scan-kasm-catalog.sh` … in a job the `publish` job needs". It fails today. The docs claim is pinned in `security-doc-claims.test.sh`.
- CI run 38586060306 on `351029a` shows `kasm adapter contract + catalog scan` green, so the new release gate passes on today's catalog.

**Finding R1-41 (P3, new, supply chain): `:main`-channel signed images build from the shared GHA cache, which any main-branch job can poison.**
- **Attack:** "A compromised npm devDependency or Playwright download runs in ci.yml on push to `main`. It writes a main-scoped buildkit cache index for scope `backend` (the Cacheract technique). The next images.yml run imports the poisoned layer, signs it with `images.yml@refs/heads/main` and promotes `:main` / `:sha-*`."
- **Required fix:** "Drop `cache-from`/`cache-to` in images.yml, as in release.yml and runtime-images.yml."
- **Security test:** "`chk_absent "images: no gha cache" "$IMG" 'type=gha'`. It fails today."

**Decisions:**
- The release job runs both catalog scripts: `check-kasm-catalog.sh`, which is network-free and checks the declared floor and digest pins, and `scan-kasm-catalog.sh`. The job uses the same sha256-pinned trivy as the release `scan` job, through the workflow-level `TRIVY_VERSION`/`TRIVY_TGZ_SHA256`, which equal ci.yml's kasm-contract pin.
- It holds `contents: read` only (SEC-05). It needs only `prepare`, so it runs in parallel with the image builds.
- Its job name joins Task 27's scan-age bound, because a kasm scan that is days old at approval time is as stale as an image scan.
- `kasm-catalog` joins the `cleanup` needs. Otherwise a kasm failure, which skips publish, would leave the run's pushed digests behind, because `failure()` only sees ancestor jobs.
- `scan-kasm-catalog.sh` gains the empty `--config` (SUPR-8) that every other release trivy call has. It is now a release gate.

Doc-only parts: the `docs/kasm-images.md` "blocking CI gate" claim and the provenance cache statement are pinned in `security-doc-claims.test.sh` (Global Constraints). This task edits TM-48's prose bullet only. Task 30 owns the TM-48 and TM-49 Control and Pinned-by cells.

**Files:**
- Modify: `.github/workflows/release.yml`: a new job `kasm-catalog` before `chart:`; `publish`, `dryrun-summary` and `cleanup` needs/if; the cleanup comment; the publish stale-scan step's `legs`
- Modify: `.github/workflows/images.yml:219-223`, `:429-431` (cache lines) and the header (before `# Vulnerability gate (SEC-44)`)
- Modify: `.github/scripts/scan-kasm-catalog.sh:31` (trap), `:72` (trivy call); `.github/scripts/check-kasm-catalog.sh:6` (header)
- Modify: `.github/tests/release-topology.test.sh` (after the `release: no gha cache` check, line 50), `.github/tests/failed-digest-cleanup.test.sh:70`, `.github/tests/security-doc-claims.test.sh` (append)
- Modify: `docs/kasm-images.md:165-169`, `:202-204`; `.github/README.md:8-10` (workflow table); `docs/security/provenance.md:7`; `docs/security/threat-model.md` (Supply chain section, before `Register: TM-48, TM-49.`)

**Interfaces:**
- Consumes: Task 27's release publish step `refuse a stale scan (older than SCAN_MAX_AGE_HOURS)` and its line `mapfile -t legs < <(jq -r '.[] | "scan " + .' <<< "$RELEASE_IMAGES")`. Task 28's `publish-gating.test.sh` checks only that `scan` is in publish `needs`, so adding `kasm-catalog` keeps it green. Task 10's `security-doc-claims.test.sh` helpers.
- Produces: the release job id `kasm-catalog`, name `kasm catalog scan (trivy gate + engine floor)`. Task 30 cites `release-topology.test.sh` for R1-37 and R1-41.

- [ ] **Step 1: Write the failing tests**

In `.github/tests/release-topology.test.sh`, insert directly after the line `chk_absent "release: no gha cache" "$REL" …` (under `# SEC-16: no shared GHA cache on release image builds.`):

```bash
# R1-41: signed :main images build without the shared GHA cache.
chk_absent "images: no gha cache" "$IMG" 'type=gha'
# R1-37: every release re-runs the kasm catalog gate, and publish needs it.
chk "release: kasm catalog policy in the release" "$REL" 'run: \.github/scripts/check-kasm-catalog\.sh'
chk "release: kasm catalog scan in the release" "$REL" 'run: PATH="\$PWD/bin:\$PATH" \.github/scripts/scan-kasm-catalog\.sh'
chk "release: publish needs kasm-catalog" "$REL" 'needs: \[prepare, scan, kasm-catalog, chart, binaries\]'
chk "release: publish requires kasm-catalog success" "$REL" "needs\.kasm-catalog\.result == 'success'"
chk "release: kasm-catalog leg inside the scan-age bound" "$REL" 'legs\+=\("kasm catalog scan \(trivy gate \+ engine floor\)"\)'
chk "kasm scan: trivy --config empty" "$ROOT/.github/scripts/scan-kasm-catalog.sh" '--config "\$CFG"'
```

In `.github/tests/security-doc-claims.test.sh`, insert directly above the final `exit "$fails"`:

```bash
# R1-37: the measured kasm engine floor and trivy gate block releases, not
# merges; the docs must not call the CI job a blocking gate.
need R1-37 docs/kasm-images.md 'release\.yml `kasm-catalog` job'
absent R1-37 docs/kasm-images.md 'blocking CI gate and stays blocking'

# R1-41: no signing workflow builds from the shared GHA cache.
need R1-41 docs/security/provenance.md 'without (a|the) shared build cache'
```

- [ ] **Step 2: Run them and see them fail**

Run: `bash .github/tests/release-topology.test.sh; bash .github/tests/security-doc-claims.test.sh 2>&1 | grep -E 'R1-37|R1-41'`
Expected:
```
FAIL: images: no gha cache
FAIL: release: kasm catalog policy in the release
FAIL: release: kasm catalog scan in the release
FAIL: release: publish needs kasm-catalog
FAIL: release: publish requires kasm-catalog success
FAIL: release: kasm-catalog leg inside the scan-age bound
FAIL: kasm scan: trivy --config empty
FAIL: R1-37: docs/kasm-images.md does not state /release\.yml `kasm-catalog` job/
FAIL: R1-37: docs/kasm-images.md still states /blocking CI gate and stays blocking/
FAIL: R1-41: docs/security/provenance.md does not state /without (a|the) shared build cache/
```

- [ ] **Step 3: Gate the release on the kasm catalog (R1-37)**

In `.github/workflows/release.yml`:

1. Insert this job directly above `  chart:` (`name: helm chart (lint, digest-stamp, package)`):

```yaml
  kasm-catalog:
    # R1-37 / KASM-2: the chart ships kasmAdapter.browserAllowlist, so the
    # release re-runs the whole kasm catalog gate: the declared floor
    # (check-kasm-catalog.sh) and the measured engine floor + trivy gate
    # (scan-kasm-catalog.sh). In ci.yml the measured half reports but does
    # not block a merge (network-dependent); here it blocks publish.
    # SEC-05: read-only, no secrets, pushes nothing.
    name: kasm catalog scan (trivy gate + engine floor)
    needs: prepare
    runs-on: ubuntu-24.04
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1  # v7.0.1
        with:
          persist-credentials: false

      - name: install trivy ${{ env.TRIVY_VERSION }} (sha256-verified)
        run: |
          mkdir -p "$PWD/bin"
          curl -fsSL -o /tmp/trivy.tgz \
            "https://github.com/aquasecurity/trivy/releases/download/v${TRIVY_VERSION}/trivy_${TRIVY_VERSION}_Linux-64bit.tar.gz"
          echo "${TRIVY_TGZ_SHA256}  /tmp/trivy.tgz" | sha256sum -c -
          tar -xzf /tmp/trivy.tgz -C "$PWD/bin" trivy
          "$PWD/bin/trivy" version

      - name: catalog policy (digest pins + declared floor)
        run: .github/scripts/check-kasm-catalog.sh

      - name: catalog scan (trivy gate + measured engine floor)
        run: PATH="$PWD/bin:$PATH" .github/scripts/scan-kasm-catalog.sh

```

2. In both `publish` and `dryrun-summary`, replace `    needs: [prepare, scan, chart, binaries]` with `    needs: [prepare, scan, kasm-catalog, chart, binaries]`. In each job's `if:`, insert `          && needs.kasm-catalog.result == 'success'` directly after `          && needs.scan.result == 'success'`. Make both changes in both jobs.

3. In `cleanup`, replace `    needs: [prepare, base, images, scan, chart, binaries]` with `    needs: [prepare, base, images, scan, kasm-catalog, chart, binaries]`. In its comment, replace
```
    # Covers every pre-publish leg (prepare, base, images, scan, chart,
    # binaries). Legs this job does NOT cover: `publish` (its output may
```
with the following.
```
    # Covers every pre-publish leg (prepare, base, images, scan,
    # kasm-catalog, chart, binaries). Legs this job does NOT cover: `publish` (its output may
```
(The rest of the line is unchanged. `.github/.yamllint.yml` accepts the longer comment line.)

4. In the publish step `refuse a stale scan (older than SCAN_MAX_AGE_HOURS)` (Task 27), directly after the `mapfile -t legs …` line, add:
```bash
          legs+=("kasm catalog scan (trivy gate + engine floor)")
```

5. In `.github/tests/failed-digest-cleanup.test.sh:70`, replace `'needs: [prepare, base, images, scan, chart, binaries]'` with `'needs: [prepare, base, images, scan, kasm-catalog, chart, binaries]'`.

6. In `.github/scripts/scan-kasm-catalog.sh`:
   - Replace `IGN="$(mktemp)"; trap 'rm -f "$IGN"' EXIT` with `IGN="$(mktemp)"; CFG="$(mktemp)"; trap 'rm -f "$IGN" "$CFG"' EXIT`.
   - Replace `  if ! "$TRIVY" image --scanners vuln --severity CRITICAL,HIGH --ignore-unfixed \` with:
```bash
  # SUPR-8: the empty --config means a committed trivy.yaml can never
  # neuter the gate.
  if ! "$TRIVY" --config "$CFG" image --scanners vuln --severity CRITICAL,HIGH --ignore-unfixed \
```

7. In `.github/scripts/check-kasm-catalog.sh`, replace header line 6 `# scan-kasm-catalog.sh and runs on the kasm-contract schedule job.` with:
```bash
# scan-kasm-catalog.sh and runs in the ci.yml kasm-contract job and the
# release.yml kasm-catalog job.
```

- [ ] **Step 4: Build signed `:main` images without the shared cache (R1-41)**

In `.github/workflows/images.yml`:

1. In the linux-base build step, replace
```yaml
          cache-from: type=gha,scope=linux-base
          # cache writes only when publishing — a non-main dispatch must
          # never poison the shared scope the :main build consumes.
          cache-to: >-
            ${{ env.PUBLISH == 'true' && 'type=gha,mode=max,scope=linux-base' || '' }}
```
with
```yaml
          # No cache-from/cache-to (SEC-16): a signed :main build never
          # reads the shared GHA cache that other main-branch jobs write.
```

2. In the matrix build step, replace
```yaml
          cache-from: type=gha,scope=${{ matrix.image }}
          cache-to: >-
            ${{ env.PUBLISH == 'true' && format('type=gha,mode=max,scope={0}', matrix.image) || '' }}
```
with
```yaml
          # No cache-from/cache-to (SEC-16): see the linux-base build.
```

3. In the header, directly above `# Vulnerability gate (SEC-44): TRIVY_GATE_SEVERITY controls which`, insert (do not write the literal `type=gha`, because the new `chk_absent` would match the comment):
```yaml
# No shared GHA build cache (SEC-16): like release.yml and
# runtime-images.yml, these builds neither read nor write the GHA cache.
# Any main-branch job can write that cache, and these images are signed
# with the images.yml@refs/heads/main identity.
#
```

Cost: `:main` image builds lose their layer cache and take longer, the same as release and train builds today.

- [ ] **Step 5: Correct the docs**

`docs/kasm-images.md`, replace lines 165-169:
```markdown
`.github/scripts/check-kasm-catalog.sh` enforces that in
every PR; the heavyweight `.github/scripts/scan-kasm-catalog.sh` (trivy
gate + engine extraction) and the adapter contract test run in the
`kasm-contract` ci.yml job — weekly, on dispatch, on main pushes, and on
PRs touching the adapter/catalog surface.
```
with
```markdown
`.github/scripts/check-kasm-catalog.sh` enforces that, and the
declared floor, in every PR (the required workflow-policy job). The
heavyweight `.github/scripts/scan-kasm-catalog.sh` (trivy gate + measured
engine floor) and the adapter contract test run in the `kasm-contract`
ci.yml job — weekly, on dispatch, on main pushes, and on PRs touching
the adapter/catalog surface. There they report but do not block a
merge (the job is network-dependent). Every release runs both catalog
scripts again in the release.yml `kasm-catalog` job, which the publish
job needs: an image that fails the trivy gate or the measured engine
floor stops the release.
```
and in onboarding item 4, replace
```markdown
   and the installed engine must meet the declared floor. This is a
   blocking CI gate and stays blocking — the lag is the accepted
```
with
```markdown
   and the installed engine must meet the declared floor. The declared
   floor gates every merge; the measured engine gates every release
   (release.yml `kasm-catalog`). The lag is the accepted
```
(The next line, `   residual risk of the preview, not a target to grow. The cataloged`, stays.)

`.github/README.md` workflow table:
- `ci.yml` row: replace `and `TestKasmAdapterChromium`) |` with `and `TestKasmAdapterChromium`; reports, does not gate merges) |`.
- `images.yml` row: replace `` `kasm-adapter`) → isolated `` with `` `kasm-adapter`), without the shared build cache → isolated ``.
- `release.yml` row: replace `set → isolated trivy gate → `environment: release`` with ``set → isolated trivy gate + kasm catalog scan (`scan-kasm-catalog.sh`: trivy gate + measured engine floor) → `environment: release` ``.

`docs/security/provenance.md`, directly after the status paragraph that ends `this documents the mechanism a `vX.Y.Z` release exercises.` (line 7), add:
```markdown

All three signing workflows (`images.yml`, `release.yml`,
`runtime-images.yml`) build without a shared build cache: no build reads
or writes the GitHub Actions cache, so a cache entry another workflow
wrote cannot become a layer of a signed image.
```

`docs/security/threat-model.md`, Supply chain section: directly after the bullet `- Required checks and enforce-admins are codified in` / `` `.github/scripts/setup-repo-protection.sh`. `` and before `Register: TM-48, TM-49.`, add:
```markdown
- Kasm catalog: `check-kasm-catalog.sh` runs in the required
  workflow-policy job and requires every `kasmweb/*` reference to be a
  digest-pinned catalog entry whose declared engine floor meets its
  class budget. The measured engine floor and the trivy gate
  (`scan-kasm-catalog.sh`) report in the CI `kasm-contract` job and gate
  every release in the release.yml `kasm-catalog` job, which the publish
  job needs.
```

- [ ] **Step 6: Run the tests and lint**

```bash
for t in release-topology security-doc-claims failed-digest-cleanup publish-gating supply-chain-hardening; do bash .github/tests/$t.test.sh >/tmp/t29-$t.log 2>&1; echo "$t rc=$?"; grep -E '^FAIL' /tmp/t29-$t.log; done
yamllint -c .github/.yamllint.yml .github/workflows/ && echo yamllint-ok
command -v zizmor >/dev/null && zizmor --offline .github/workflows/ 2>&1 | tail -1
bash -n .github/scripts/scan-kasm-catalog.sh && echo sh-ok
for t in .github/tests/*.test.sh; do bash "$t" >/dev/null 2>&1 || echo "FAIL $t"; done
```
Expected: every `rc=0`, no `FAIL`, `yamllint-ok`, zizmor `No findings to report`, `sh-ok`, and no output from the final loop.

- [ ] **Step 7: Commit**

```bash
git add .github/workflows/release.yml .github/workflows/images.yml \
  .github/scripts/scan-kasm-catalog.sh .github/scripts/check-kasm-catalog.sh \
  .github/tests/release-topology.test.sh .github/tests/failed-digest-cleanup.test.sh \
  .github/tests/security-doc-claims.test.sh .github/README.md \
  docs/kasm-images.md docs/security/provenance.md docs/security/threat-model.md
git commit -m "ci: gate releases on the kasm catalog scan and build main images without the shared cache" \
  -m "R1-37: release.yml kasm-catalog job (check + scan-kasm-catalog.sh, pinned trivy, read-only) is needed by publish, dry-run summary and cleanup, and sits inside the scan-age bound. R1-41: images.yml drops cache-from/cache-to. Docs state merge vs release gating and the cache-free signing builds." \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 30: Review status, threat-model register, test inventory, links (all findings) — tier: integrate

**What this task does:** Tasks 1–29 fixed the findings. This task records the outcome in four places:

1. The report `docs/security/review-v1.0.md`: the status of each finding, an "After the fixes" verdict table, the accepted residual, and the residual-risk summary.
2. The threat-model register `docs/security/threat-model.md` §6: the Control and Pinned-by cells of every row a code fix changed, plus four new rows TM-51 to TM-54 for threats the register lacked. §7 gains the accepted PID residual if Task 18 did not already add it.
3. `docs/security/test-inventory.md`: a new section listing every security test the fixes added.
4. The links that the W2 cleanup turned into plain text: `README.md` Status, `SECURITY.md` and `.github/README.md`. They point back at the report.

A new gate, `.github/tests/security-review-status.test.sh`, keeps this true. It fails today, because every status reads `open`.

**Files:**
- Create: `.github/tests/security-review-status.test.sh`
- Modify: `docs/security/review-v1.0.md`:
  - header bullets, lines 8-12;
  - §3, after the paragraph ending "R1-36 and R1-41." (line 173);
  - §4 Status cells, lines 183-191 and 197-228;
  - §5 table, after the row "Single maintainer, no independent audit";
  - §6 "Release gate" paragraph, lines 256-260.
- Modify: `docs/security/threat-model.md`:
  - §6 register rows TM-01, 04, 05, 06, 07, 09, 13, 20, 23, 26, 29, 34, 36, 38, 40, 41, 42, 46, 47, 48, 49, 50;
  - new rows TM-51 to TM-54 after TM-50;
  - §7 (line 731 onward).
- Modify: `docs/security/test-inventory.md`: a new `## 12. Security-review fix tests` section at the end of the file, after §11.
- Modify: `README.md:96`, `SECURITY.md:8-10`, `.github/README.md:147-148`.
- Check only: `.github/workflows/release.yml:1355` (the review line). Gate: `.github/tests/release-notes-review-status.test.sh`.

**Interfaces:**
- Consumes: the commits of Tasks 1–29 on this branch, each with the subject from the plan's task index, which is copied below. Also the test functions and `.github/tests` scripts those commits added. Base commit: `98c7f54`.
- Produces: `.github/tests/security-review-status.test.sh`, the register rows `TM-51`..`TM-54`, and the inventory section heading `## 12. Security-review fix tests`. That heading's slug is `12-security-review-fix-tests`.

Finding → task commit subject → register row. Use these values exactly:

| Finding | Commit subject | Row |
|---|---|---|
| R1-01 | `fix(quota): settle releases a reservation only under the workspace row lock` | TM-09 |
| R1-02 | `feat(quota): operator-declared tenant quota ceilings bound tenant-admin writes` | TM-51 (new) |
| R1-03 | `fix(auth): bound unknown-cookie store lookups per client on the app listener` | TM-06 |
| R1-04 | `fix(gateway): upstream 1xx responses cannot strip pinned session headers` | TM-29 |
| R1-05 | `fix(backend): bound request reads on the session listener` | TM-52 (new) |
| R1-06 | `fix(runtime): KasmVNC refuses runtime overrides of clipboard parameters` | TM-36 |
| R1-07 | `docs(install): require a kubelet podPidsLimit on workspace nodes and check it in preflight` | TM-53 (new) |
| R1-08 | `fix(crd)!: remove the unenforced WorkspaceTemplate browserPolicy` | TM-46 |
| R1-09 | `ci(runtime): carry build time into the train manifest and refuse a stale scan before signing` and `ci(release): stamp runtime builtAt and refuse a stale scan before signing` | TM-49 |
| R1-10, R1-11 | `fix(api): pin browser security headers on /v1 and correct the cookie and id-token contract` | TM-01 |
| R1-12 | the same subject as R1-10 | TM-20 |
| R1-13 | `fix(frontend): serve only the documented branding file names` | TM-05 |
| R1-14 | `fix(auth): a login started with a valid session keeps it` | TM-07 |
| R1-15 | `fix(chart): narrow trusted-proxy guidance and remove podSecurity.platformEnforce` | TM-06 |
| R1-16 | `fix(audit): bound audit targets, log route patterns, audit single-statement writes in-tx` | TM-13 |
| R1-17 | `docs(security): state admin-role revocation latency and the :apply dry-run contract` | TM-11 |
| R1-18, R1-19 | `fix(api): foreign-owner tickets answer 404 and NUL filters answer 400` | TM-04 |
| R1-20 | `fix(quota): reserve exact template bytes and re-reserve on a revision move` | TM-09 |
| R1-21 | the same subject as R1-17 | TM-17 |
| R1-22, R1-23 | the same subject as R1-16 | TM-50 |
| R1-24 | `fix(retained): label retained PVCs with the workspace CR name` | TM-09 |
| R1-25 | `fix(broker): every refused ticket redemption consumes the ticket` | TM-23 |
| R1-26 | `fix(backend): health only on control hosts; control token length and failure limit` | TM-26 |
| R1-27 | `docs(security): in-pod trust covers the desktop leg; drop upstream Content-Disposition` | TM-37 |
| R1-28, R1-35 | `fix(runtime): ClusterOnly egress stays in-tenant and the NetworkPolicy precedes the pod` | TM-34 |
| R1-29 | `fix(runtime): the userns browser profile pair requires hostUsers false` | TM-47 |
| R1-30, R1-31 | `fix(mtls): CA rotation severs removed-CA connections; blocklist matches issuers canonically` | TM-38 |
| R1-32 | `fix(mtls): gateway role is opt-in by CN; count handshake and role refusals` | TM-54 (new) |
| R1-33 | the same subject as R1-32 | TM-40 |
| R1-34 | `fix(chart): extraArgs and extraEnv cannot override any chart-rendered flag` | TM-42 |
| R1-36 | the same subject as R1-15 | TM-41 |
| R1-37 | `ci: gate releases on the kasm catalog scan and build main images without the shared cache` | TM-48 |
| R1-38, R1-39, R1-40 | `ci: pin publish gating per job, codify required checks, never leave an unsigned chart tagged` | TM-49 |
| R1-41 | the same subject as R1-37 | TM-49 |

- [ ] **Step 1: Confirm the branch has every task commit**

```bash
cd /home/ubuntu/Workspace/tcdi-wt/V1-R1   # or the worktree you are in
git log --format='%s' 98c7f54..HEAD | grep -v '^docs(plan)' > /tmp/r1-subjects.txt; wc -l < /tmp/r1-subjects.txt
```

Expected: at least 29 lines, and every subject in the table above appears in `/tmp/r1-subjects.txt` as an exact line (`grep -Fx`). The plan's own `docs(plan)` commit is filtered out. Review fix-round commits may add extra lines; that is fine. Task 30's own commit comes later. STOP and report if a subject is missing or reworded: the report quotes the subjects verbatim.

- [ ] **Step 2: Collect the tests each commit added**

```bash
for h in $(git rev-list --reverse 98c7f54..HEAD); do
  echo "== $(git log -1 --format=%s "$h")"
  git show "$h" -U0 --format= -- '*_test.go' \
    | awk '/^\+\+\+ b\//{f=substr($0,7)} /^\+func Test[A-Za-z0-9_]+\(/{match($0,/Test[A-Za-z0-9_]+/); print "  " substr($0,RSTART,RLENGTH) "  " f}'
  git show "$h" --format= --name-status -- '.github/tests/*.test.sh' | sed 's/^/  gate: /'
done > /tmp/r1-tests.txt
wc -l < /tmp/r1-tests.txt
```

`/tmp/r1-tests.txt` is the source for the Pinned-by cells (Step 5) and the inventory section (Step 6). Each test goes to the finding of its commit. When a commit covers several findings, use the test names, which carry the brief's names, to assign each test. The names the brief required are below. Every one must appear in the file, either under that exact name or under the name the task recorded in its commit (`git show <h> --stat`). If one is missing, STOP and report it.

- R1-01 `TestSettleQuota_StartRaceKeepsReservation`, `TestSettleQuota_StoppedWorkspaceStillReleases`
- R1-02 `TestAdminQuota_PutAboveOperatorCeilingRefused`, `TestAdminQuota_UndeclaredTenantCreateRefused`, `TestAdminQuota_PutAtCeilingAccepted`
- R1-03 `TestAppListener_UnknownCookieSprayBounded`, `TestAppListener_ValidSessionsBehindOneClientNotThrottled`
- R1-04 `TestResponsePolicy_Upstream1xxCannotStripPinnedHeaders`, `TestProxy_WebSocketUpgradeSurvives1xxGuard`
- R1-05 `TestSessionListener_StalledBodyBounded`, `TestSessionListener_AdmittedStreamOutlivesReadBound`, `TestSessionListener_TransferUploadOutlivesReadBound`
- R1-06 `TestLinuxRuntimeClipboardParamsNotOverridable` (image IT), the `TestKasmAdapterChromium/ClipboardParamsNotOverridable` subtest (a `t.Run`, so the awk above does not list it; take it from Task 17's commit) and the `clipboard-policy.test.sh` gate
- R1-07 the `preflight.test.sh` and `security-doc-claims.test.sh` gates
- R1-08 `TestWorkspaceTemplate_BrowserPolicyFieldRemoved`
- R1-09 the `write-runtime-manifest.test.sh`, `check-scan-age.test.sh`, `publish-inputs.test.sh` and `release-topology.test.sh` gates
- R1-10 `TestWrapApp_PortalBrowserHeaders`; R1-11 `TestOpenAPI_SessionCookieMatchesAuthDefaults`; R1-12 `TestOpenAPI_LogoutDocumentsIDTokenHint`
- R1-13 `TestBrandingDir_OnlyAllowlistedNames`; R1-14 `TestLogin_ValidSessionShortCircuits`
- R1-15 `TestNotesWarnUnrestrictedEdgeOpen` and the `security-doc-claims.test.sh` gate; R1-16 `TestAudit_RequestLogNeverContainsRawOwnerRef`
- R1-17, R1-21 the `security-doc-claims.test.sh` gate
- R1-18 `TestCreateConnection_ForeignOwnerIs404`; R1-19 `TestListWorkspaces_BadPhaseIs400`, `TestAdminUsers_ForgedCursorIs400`
- R1-20 `TestStart_RevisionMoveReservesNewVector`
- R1-22 `TestAudited_TargetSanitizedAndBounded`, `TestAudit_InsertSanitizedTarget`; R1-23 `TestAdminUserLimit_AuditFailureRollsBackWrite`
- R1-24 `TestRetainedAttach_LabelIsCRName`; R1-25 `TestRedeemTicket_EveryRefusalConsumes`
- R1-26 `TestSessionListener_HealthOnlyOnControlHosts`, `TestControlToken_MinLength`
- R1-27 the `security-doc-claims.test.sh` gate and the inverted desktop-leg characterization test `TestDesktopLeg_ReplacedHelperRelaysBytesWithoutDisposition`
- R1-28 `TestClusterOnlyEgressDefaultSameTenantOnly`; R1-35 `TestEnsure_NetPolPrecedesPod`
- R1-29 `TestEnsureUsernsProfileRequiresHostUsersFalse`, `TestSeededUsernsProfileRequiresHostUsersFalse` and the `security-doc-claims.test.sh` gate
- R1-30 `TestInternalListener_RemovedCASeversLiveConn`; R1-31 `TestBlocklistIssuerCanonicalMatch`
- R1-32 `TestUnknownCN_Refused`; R1-33 `TestRoleMismatchCounted`, `TestIdentify_HandshakeRejectionsCounted`
- R1-34 `TestExtraArgsCannotOverrideRenderedFlags`; R1-36 `TestPodSecurityValuesAllReferenced`
- R1-37, R1-41 the `release-topology.test.sh` and `security-doc-claims.test.sh` gates; R1-38 `publish-gating.test.sh`; R1-39 `setup-repo-protection.test.sh`; R1-40 `push-sign-chart.test.sh`

- [ ] **Step 3: Write the failing gate**

Create `.github/tests/security-review-status.test.sh`:

```bash
#!/usr/bin/env bash
# security-review-status.test.sh — the v1.0 security review is closed out:
# every finding row in docs/security/review-v1.0.md §4 carries a final status
# ("fixed (<commit subject>)" or "accepted (owner)"), the public entry points
# link the report, and every test the inventory's review section names exists.
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
REV="$ROOT/docs/security/review-v1.0.md"
INV="$ROOT/docs/security/test-inventory.md"
fails=0
fail() { echo "FAIL: $*"; fails=$((fails + 1)); }

[ -f "$REV" ] || { echo "FAIL: $REV is missing"; exit 1; }

# 1. Every finding row (| R1-NN | Sev | Row | Title | Where | Status |) has a
#    final status in its last cell.
n=0
while IFS= read -r row; do
  n=$((n + 1))
  id="$(sed -E 's/^\| (R1-[0-9]{2}) .*/\1/' <<<"$row")"
  status="$(sed -E 's/.*\| ([^|]*[^| ]) *\|[[:space:]]*$/\1/' <<<"$row")"
  case "$status" in
    'fixed ('*')'*|'accepted (owner)'*) ;;
    *) fail "$id status is '$status', want 'fixed (<commit subject>)' or 'accepted (owner)'" ;;
  esac
done < <(grep -E '^\| R1-[0-9]{2} \|' "$REV")
[ "$n" -eq 41 ] || fail "found $n finding rows in review-v1.0.md, want 41"
grep -qE 'All findings are open' "$REV" && fail "review-v1.0.md header still says all findings are open"

# 2. The public entry points link the report.
for f in README.md SECURITY.md .github/README.md; do
  grep -qE '\]\((\.\./)?docs/security/review-v1\.0\.md(#[a-z0-9-]+)?\)' "$ROOT/$f" \
    || fail "$f does not link docs/security/review-v1.0.md"
done

# 3. The inventory's review section covers every finding, and every Go test it
#    names exists.
sec="$(awk '/^## 12\. Security-review fix tests/{f=1; next} f && /^## /{exit} f' "$INV")"
if [ -z "$sec" ]; then
  fail "test-inventory.md has no '## 12. Security-review fix tests' section"
else
  for i in $(seq 1 41); do
    id="$(printf 'R1-%02d' "$i")"
    grep -qE "\| $id( |,)" <<<"$sec" || fail "inventory review section has no row for $id"
  done
  while IFS= read -r t; do
    git -C "$ROOT" grep -qE "^func $t\(" -- '*.go' || fail "inventory names $t, which no Go file defines"
  done < <(grep -oE '`Test[A-Za-z0-9_]+`' <<<"$sec" | tr -d '`' | sort -u)
fi

exit "$fails"
```

Run: `bash .github/tests/security-review-status.test.sh | tail -6; echo "rc=${PIPESTATUS[0]}"`

Expected: non-zero rc, with 41 `status is 'open'` lines, `header still says all findings are open`, `README.md does not link …`, `SECURITY.md does not link …`, `.github/README.md does not link …` and `has no '## 12. Security-review fix tests' section`.

- [ ] **Step 4: Update the report**

In `docs/security/review-v1.0.md`:

(a) In the header, replace the bullet

```
- Result: **0 P0, 0 P1, 9 P2, 32 P3**. All findings are open at the
  reviewed commit.
```

with

```
- Result: **0 P0, 0 P1, 9 P2, 32 P3** at the reviewed commit.
- Fix status: every finding is fixed, each by the commit named in its
  Status cell. The node-level part of R1-07 is accepted by the owner
  (§5). See [After the fixes](#after-the-fixes).
```

(b) In §4, set each row's last cell (now `open`) to `fixed (`<subject>`)`, using the subject from the Interfaces table, in backticks. For R1-07, write ``fixed (`docs(install): require a kubelet podPidsLimit on workspace nodes and check it in preflight`); node-level enforcement accepted (owner)``. For R1-09, write ``fixed (`ci(runtime): carry build time into the train manifest and refuse a stale scan before signing`, `ci(release): stamp runtime builtAt and refuse a stale scan before signing`)``. Keep every other cell byte for byte.

(c) In §3, directly after the paragraph that ends "R1-36 and R1-41.", add:

```markdown
### After the fixes

The verdicts above are the review's record at the reviewed commit. After
the fixes, the rows below change verdict. Every other row stays Verified.

| Row | Verdict at review | After the fixes | Findings |
|---|---|---|---|
| TM-06 | Partial | Verified | R1-03, R1-15 |
| TM-09 | Partial | Verified | R1-01, R1-20, R1-24 |
| TM-26 | Partial | Verified | R1-26 |
| TM-29 | Gap | Verified | R1-04 |
| TM-36 | Partial | Verified | R1-06 |
| TM-38 | Partial | Verified | R1-30, R1-31 |
| TM-40 | Partial | Verified | R1-33 |
| TM-42 | Partial | Verified | R1-34 |
| TM-48 | Partial | Verified | R1-37 |
| TM-49 | Partial | Verified | R1-09, R1-38, R1-39, R1-40, R1-41 |
| TM-50 | Partial | Verified | R1-22, R1-23 |

Four register rows were added for findings that fit no row: TM-51
(R1-02), TM-52 (R1-05), TM-53 (R1-07) and TM-54 (R1-32). R1-35 is
recorded under TM-34, R1-36 under TM-41 and R1-08 under TM-46 (the field
was removed). After the fixes: 54 rows, all Verified. TM-53 carries the
accepted node-level residual. The R1-29 gate matches the shipped
`chromium-userns.json` profile by name; TM-47's residual states that a
custom `Localhost` profile allowing namespace creation is not detected
and needs `hostUsers: false` from the operator.
```

(d) In §5, add this row after "Single maintainer, no independent audit":

```markdown
| Workspace PID bound depends on node configuration | Accepted (owner): the node-level part of R1-07. Kubernetes has no pod-spec PID limit; the kubelet's `podPidsLimit` is node configuration this project does not ship. The install guide requires it and `hack/preflight/preflight.sh` warns for a workspace node without it. |
```

(e) In §6, replace the "Release gate" paragraph

```
**Release gate.** No P0 or P1 finding is open at the reviewed commit.
Each of the nine P2 findings must be fixed, or accepted with an owner
and a reason, before the stable release. This report's status column
records the outcome. The P3 findings are hardening and documentation
corrections.
```

with

```
**Release gate.** No P0 or P1 finding was open at the reviewed commit.
All nine P2 and all 32 P3 findings are fixed; the Status column names
each fix. The node-level part of R1-07 is accepted by the owner (§5).
The themes below describe the state at the reviewed commit.
```

Run: `grep -c '| open |' docs/security/review-v1.0.md` — expected `0`.

- [ ] **Step 5: Update the threat-model register**

In `docs/security/threat-model.md` §6, edit only the Control and Pinned-by cells of the rows named below. The Residual cells that Tasks 7, 10, 16, 18, 20 and 25 already corrected stay as they are. Each row stays on one line, and a literal pipe is written `\|`.

(a) Append each sentence below to the end of its row's Control cell, with one space before it:

| Row | Sentence appended to Control |
|---|---|
| TM-01 | Every app-listener response carries `X-Content-Type-Options: nosniff`, a CSP with `frame-ancestors 'none'` and `X-Frame-Options: DENY` from the outermost `wrapApp` middleware (`internal/backend/wire.go`); the OpenAPI contract states the `__Host-tcdi_session` cookie as `SameSite=Lax`, with CSRF blocked by the synchronizer token plus the Origin gate. |
| TM-04 | Connection issuance answers 404 for another owner's workspace in the same tenant; a list filter or cursor key with NUL or invalid UTF-8 answers 400. |
| TM-05 | Only the documented branding file names are served. |
| TM-06 | Unknown-cookie store lookups are bounded per client on the app listener by a local limiter, and a session miss never writes. |
| TM-07 | A login started with a valid portal session redirects without minting a new session. |
| TM-09 | Settle re-checks the workspace under its row lock and releases a reservation only while the workspace is deleted or wanted Stopped in the generation it observed; reservations carry exact template bytes, and a start that moves revision re-reserves the new vector; retained PVC labels carry the workspace CR name. |
| TM-13 | The per-request log records the route pattern, never the raw path. |
| TM-20 | The OpenAPI contract states that the retained ID token is sent as `id_token_hint` in the end-session URL. |
| TM-23 | Every redemption refused after the audience check still commits the consume. |
| TM-26 | On the session listener, `/healthz` and `/readyz` answer only on control hosts; the control token must be at least 32 bytes, and failed control attempts are limited per client. |
| TM-29 | `responsePolicy` writes the pinned header set onto every proxied response, and upstream 1xx responses other than 101 are not relayed. |
| TM-34 | Without `--runtime-clusteronly-egress`, the peer is limited to namespaces carrying the workspace's own tenant label value; the workspace NetworkPolicy is created before its pod. |
| TM-36 | Xvnc runs with an `-AllowOverride` list that names no clipboard parameter, so `vncconfig -set` cannot re-enable the clipboard at runtime. |
| TM-38 | A client-CA swap closes live internal connections whose chain no longer verifies; blocklist issuers are compared canonically, and an issuer absent from the client-CA bundle is reported at load. |
| TM-40 | TLS handshake refusals and role mismatches are counted (`reason="handshake"`, `reason="role_mismatch"`) with a rate-limited structured warning. |
| TM-41 | The chart has no unreferenced `podSecurity` value. |
| TM-42 | Every flag the chart renders is refused in `extraArgs`, every denied flag's env form is refused in `extraEnv` and the reverse, and the leader-election guard reads `operator.extraArgs`. |
| TM-46 | `WorkspaceTemplate` carries no browser URL policy, so no unenforced restriction is advertised. |
| TM-47 | The operator refuses a template that names the `chromium-userns` seccomp profile unless the pod runs with `hostUsers: false`. |
| TM-48 | Releases run the measured engine floor and trivy catalog scan before publish; merges are gated by the declared floor. |
| TM-49 | The train and release record the image build time as `builtAt` and refuse to sign when any scan leg finished more than 24 hours earlier (`check-scan-age.sh`); per-job publish gating is pinned by `publish-gating.test.sh`; a chart whose signing fails is deleted from the registry (`push-sign-chart.sh`); `:main` images build without the shared Actions cache. |
| TM-50 | `audited()` stores a bounded, valid-UTF-8 target, and quota and user-limit writes run inside the audited transaction. |

Check each appended sentence against the code the task changed before you write it (`git show <commit> --stat` for that finding). If a task implemented a detail differently, for example a different header set, state what the code does and keep the sentence's claim.

(b) Append the tests from Step 2 to each row's Pinned-by cell as backticked names, comma-separated. For a gate script, write its file name in backticks (for example `` `publish-gating.test.sh` ``). The threat-model gate checks only `Test…` names.

(c) Add four rows directly after the TM-50 row:

```markdown
| TM-51 | 2 | A tenant admin lifts their own tenant's quota without bound. | A tenant-admin quota write is refused unless the platform configuration declares `managedNamespaces[].quotaCeiling` for the tenant (rendered as `-tenant-quota-ceilings`), and a write above the ceiling is refused; a tenant with a declared `quota` stays read-only (`internal/api/adminquota.go`). | `TestAdminQuota_PutAboveOperatorCeilingRefused`, `TestAdminQuota_UndeclaredTenantCreateRefused`, `TestAdminQuota_PutAtCeilingAccepted` | The ceiling bounds the API-layer quota only; the opt-in `namespaceQuota` (TM-44) is the independent bound. |
| TM-52 | 5 | An unauthenticated client holds session-listener connections and goroutines by never finishing a request body. | The session listener bounds every request's read phase and caps request headers (`internal/backend/listeners.go`); an admitted WebSocket upgrade and a claimed transfer lift the bound for their own connection only. | `TestSessionListener_StalledBodyBounded`, `TestSessionListener_AdmittedStreamOutlivesReadBound`, `TestSessionListener_TransferUploadOutlivesReadBound` | Each connection can still be held for the bound; the per-client launch limiter and a buffering edge proxy narrow it further. |
| TM-53 | 8 | One workspace exhausts a shared node's process and thread table, failing forks for the kubelet and other tenants. | The install guide requires a kubelet `podPidsLimit` on workspace nodes; `hack/preflight/preflight.sh` reads each workspace node's kubelet configuration and warns when the limit is unset or `-1`; the kind quickstart sets it. | `preflight.test.sh`, `security-doc-claims.test.sh` | Kubernetes has no pod-spec PID limit, and the product cannot set kubelet configuration; a node without `podPidsLimit` bounds a pod's threads only by its memory limit (accepted, §7). |
| TM-54 | 7 | A workload holding a client certificate from the internal issuer calls the gateway routes on :9443. | The gateway role is granted only to CNs listed in `-internal-gateway-cns` (empty by default; the in-process gateway needs none); every other non-operator CN gets 403, counted as `role_mismatch` (`internal/broker/httpapi/server.go`). | `TestUnknownCN_Refused`, `TestRoleMismatchCounted` | `certManager.issuerRef` must be a dedicated issuer: anyone it signs for can present the operator CN. |
```

Replace each Pinned-by cell of the new rows with the names Step 2 found, if they differ.

(d) In §7, if no item names `podPidsLimit`, add this item after the last numbered item. Number it next, and renumber nothing:

```markdown
11. **Workspace PID bound depends on node configuration.** Kubernetes
    has no pod-spec PID limit. The install guide requires a kubelet
    `podPidsLimit` on workspace nodes, and preflight warns without one.
    See TM-53.
```

If Task 18 already added the item, change only its "See" reference to `TM-53`. Then check that `docs/releases/v1.0.0.md` "Known limitations" has the matching item: `grep -n 'podPidsLimit' docs/releases/v1.0.0.md`. Expected: at least one hit (Task 18 added it).

Run: `bash .github/tests/threat-model-refs.test.sh; echo rc=$?` — expected `rc=0`. A `FAIL: threat model cites TestX, which no Go file defines` means a name from (b) or (c) is wrong; fix it from Step 2.

- [ ] **Step 6: Add the inventory section**

At the end of `docs/security/test-inventory.md`, after §11, add the section below. It has one table row per finding, in order R1-01 to R1-41. Fill the Tests and File columns from Step 2. Each Go test name and each gate file name goes in backticks, and the File column names the test file path. The table below gives the full row list with the register rows. Write the tests exactly as `/tmp/r1-tests.txt` lists them:

```markdown
## 12. Security-review fix tests

Tests added by the fixes for the internal v1.0 security review
([report](review-v1.0.md)). Each was written to fail before its fix.
Tests that need a built runtime image run in CI only (§7).

| Finding | Tests | File | Row |
|---|---|---|---|
| R1-01 | `TestSettleQuota_StartRaceKeepsReservation`, `TestSettleQuota_StoppedWorkspaceStillReleases` | `internal/provisioning/recovery_test.go` | TM-09 |
```

…continued for R1-02 to R1-41. Use the register rows from the Interfaces table. When one test covers two findings, give it one row per finding. For R1-06, add "(image IT, run locally with the images built; the adapter subtest `TestKasmAdapterChromium/ClipboardParamsNotOverridable` runs in CI's `kasm-contract` job)" after the test name. For R1-07, name the gates only: `` `preflight.test.sh`, `security-doc-claims.test.sh` ``.

Also update §11 "Gaps a reviewer will notice". Add one bullet: "`podPidsLimit` is checked by preflight, not enforced by the product (TM-53)."

Run: `bash .github/tests/security-review-status.test.sh; echo rc=$?` — the inventory lines now pass. The link lines still fail until Step 7.

- [ ] **Step 7: Restore the report links**

`README.md:96`: replace

```
**Status:** TinyCDI 1.0 — see the [release notes](docs/releases/v1.0.0.md) and [known limitations](docs/releases/v1.0.0.md#known-limitations). Maintained by one person; no independent security audit has been done. Report vulnerabilities per [SECURITY.md](SECURITY.md).
```

with

```
**Status:** TinyCDI 1.0 — see the [release notes](docs/releases/v1.0.0.md) and [known limitations](docs/releases/v1.0.0.md#known-limitations). Maintained by one person. The maintainers' internal [security review](docs/security/review-v1.0.md) of 1.0.0 is complete; no independent security audit has been done. Report vulnerabilities per [SECURITY.md](SECURITY.md).
```

`SECURITY.md:8-10`: replace

```
> **Security review.** Each release's notes state its security-review
> status. The mandatory rules and the blocking CI scanners in this file
> are the bar; an independent external review remains an open item.
```

with

```
> **Security review.** The maintainers completed an internal security
> review of 1.0.0 before release ([report](docs/security/review-v1.0.md)).
> It is **not an independent audit**. Each release's notes state its
> security-review status. The mandatory rules and the blocking CI
> scanners in this file are the bar; an independent external review
> remains an open item.
```

`.github/README.md:147-148`: replace

```
  `release` and `runtime-train` environments, which require a reviewer
  approval before their jobs run.
```

with

```
  `release` and `runtime-train` environments, which require a reviewer
  approval before their jobs run (single-maintainer risk:
  [security review §5](../docs/security/review-v1.0.md#5-accepted-risks)).
```

Check the release-notes review line, which this task leaves unchanged: `grep -n 'review-v1.0.md' .github/workflows/release.yml` — expected one hit at about line 1355, `Internally security-reviewed for v1.0 (docs/security/review-v1.0.md); not an independent audit.` The report exists at that path, so the line is accurate.

- [ ] **Step 8: Run the gates**

```bash
bash .github/tests/security-review-status.test.sh; echo "status rc=$?"
bash .github/tests/release-notes-review-status.test.sh; echo "notes rc=$?"
bash .github/tests/threat-model-refs.test.sh; echo "tm rc=$?"
bash .github/tests/docs-gates.test.sh; echo "docs rc=$?"
for t in .github/tests/*.test.sh; do bash "$t" >/dev/null 2>&1 || echo "FAILED: $t"; done
```

Expected: every rc is `0`, and the loop prints nothing. `docs-gates.test.sh` runs the link checker. It checks the new `#after-the-fixes`, `#5-accepted-risks` and `review-v1.0.md` links and their anchors. It also runs the version and PR-reference grep. The review report is in scope for it, so a `#NN` written in prose fails the gate. Commit subjects contain none.

- [ ] **Step 9: Commit**

```bash
git add .github/tests/security-review-status.test.sh docs/security/review-v1.0.md \
  docs/security/threat-model.md docs/security/test-inventory.md \
  README.md SECURITY.md .github/README.md
git commit -m "docs(security): record R1 fix status in the review, threat model and test inventory" \
  -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```
