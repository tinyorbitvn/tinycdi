# TinyCDI v1.0.0 — W2 Cleanup and Docs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use sdd-orca:subagent-driven-development (recommended) or sdd-orca:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the compatibility shims left after W0/W1 (C1). Make every published doc describe only v1.0.0 (C2). Rewrite the ADRs as current state with gap-free numbering (C3). Rebuild the threat model and test inventory for the v1.0.0 architecture as R1's input (C4). Add CI gates that keep all of this true.

**Architecture:** Tasks 1–4 are code removals. Each one deletes a branch that only served data or objects from earlier internal builds, and proves with tests that the v1 path it leaves behind is covered. Task 4 ends with the C1 grep gate. Task 5 builds the docs gates and deletes the history documents. Tasks 6–8 rewrite and renumber the ADRs. Tasks 9–12 rewrite the operator and user docs. Tasks 13–14 rebuild the security documents, turn every docs gate on at full scope, and check that everything still relevant in `docs/superpowers/` has a home outside it. Tasks run in order on one branch, one commit each, in one PR.

**Tech Stack:** Go 1.26, pgx v5, controller-runtime + envtest, Postgres 18 test container (`postgres:18.0`), Helm v4.1.3, React + TypeScript + vitest + Playwright. The gate scripts use bash, GNU grep/awk and python3 (standard library only). The CI runner is `ubuntu-24.04`, which has python3 but no ripgrep.

**Spec:** `docs/superpowers/specs/2026-10-13-tinycdi-v1-public-design.md`. §6 (C1–C4) is the scope; §2 gate 6, §3, §7.7 and §8 R1 are context. Read §6 before any task.

**Codebase:** `main` at `b41be7d`, with W0 and W1 merged. Branch `v1/w2-cleanup`. Line numbers below were checked against `b41be7d`. If a line moved, find it again by symbol.

## Decisions

These are owner decisions. Each line gives the decision, then the reason, then the cost if it is wrong.

1. The plan runs through the sdd-run workflow: tasks run one after another on one branch, with one commit per task and one PR. Task numbers are integers 1..14 with no letter suffixes. — Reason: letter suffixes broke the workflow's ordinal mapping in the last wave. — Cost if wrong: none; renumbering a plan is mechanical.
2. W2 does not delete `docs/superpowers/` (this spec and all plans, including this one). W2 moves every piece of their content that is still relevant into ADRs, the release notes and the runbooks, so that the deletion becomes a mechanical step before R3, owned by the advisor. A gate rejects any reference into `docs/superpowers/` from outside it (Task 5). — Reason: the executing workflow reads the plan. — Cost if wrong: a fact that lived only in a plan is lost at deletion; Task 14 Step 5 is the check against that.
3. Items deferred into C1 from the W0/W1 reviews, each checked against the code at `b41be7d`:
   - the stream-owner nullable claim branch in the broker (present; Task 1);
   - `adoptSnapshotFromPod` and the annotation fallbacks (present; Task 2);
   - the stranded held-row repairs in `internal/provisioning/recovery.go` (present; Task 3);
   - ADR 0008's stale port line (0008 is deleted in Task 6, and the corrected facts go into the threat model);
   - `web/scripts/screenshots.mjs` missing the `xfer-token` Secret key (present; Task 4);
   - the broker client's bodiless-204 revoke branch in `internal/gateway/brokerclient/client.go` (present; Task 1);
   - the live-catalog fallback in `(*Service).startSessionPolicy` for rows without a recorded clipboard policy (present; Task 3);
   - the claim-annotation repair in `(*RetainedApplier).applyAttach` for CRs created before single-write creation (present; Task 3, replaced by a refusal).
   — Reason: review findings. — Cost if wrong: one dead branch survives.
4. C1 removals must not change v1 behavior. Each removal task proves with tests that the remaining v1 path is covered, and runs the relevant Go, envtest and web tests. — Reason: W2 is cleanup, not features. — Cost if wrong: a regression lands disguised as cleanup.
5. The gates are real CI. The C1 legacy grep gate and the C2 docs gates (link checker, plus the version and PR-reference grep with an allowlist) are `.github/tests/*.test.sh` scripts. The existing `workflow-policy` job ("workflow policy tests (.github)", `.github/workflows/ci.yml:284-305`) already runs `for t in .github/tests/*.test.sh; do bash "$t"; done`, so a new `*.test.sh` is picked up with no workflow edit. The link checker is an offline python3 standard-library script, with no new network dependency. — Reason: gates nobody runs rot. — Cost if wrong: some CI minutes.
6. ADR renumbering uses an explicit old→new table (Task 6) and updates every inbound reference in the repo, Go comments included. W0 produced no "API conventions" ADR, so 0002 is rewritten into it. ADR 0011 (audit store) exists. — Reason: spec C3 asks for a gap-free set. — Cost if wrong: a stale ADR number in a code comment; Task 6's reference check catches it.
7. `docs/releases/v1.0.0.md` covers features, install and known limitations, with no internal version history and no PR numbers. Its known limitations include the in-pod trust model: the workspace user can read `xfer-token` and could replace `tcdi-xferd` on `:8443`, and the broker claim is the authoritative check. — Reason: owner. — Cost if wrong: an operator over-trusts the helper.
8. The workflow's verify command is:
   `go build ./... && go vet ./... && make test && go test -count=1 ./deploy/helm/ && KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path --bin-dir "$PWD/bin")" go test -tags=integration -count=1 -skip '^TestLinuxRuntime(ReadinessAndHome|FirefoxFallback|Desktop|BrowserBaseline)$|^TestKasmAdapter' ./tests/integration/... && (cd web && npm ci && npm run typecheck && npm run lint:strings && npm run test:unit && npm run build)`
   Add to it `for t in .github/tests/*.test.sh; do bash "$t" || exit 1; done`. Each task names its own focused commands. — Reason: owner. — Cost if wrong: none.

## User decisions (2026-10-14)

The owner accepted every plan-review recommendation. These bind the tasks below; where a task's text disagrees, this block wins.

1. The one-time reset procedure (spec §8 R2–R3, R5) stays only in the spec. `docs/superpowers/` is deleted only after the R2 archive push, so the private archive keeps the procedure. No W2 task copies it into public docs.
2. The gateway refuses a launch or upgrade without a tab id with `400 owner_tab_required` (Task 1). The PR note adds a KasmVNC reconnect step to the R5 browser checklist.
3. The chart README has no "Removed values" table. This is a recorded spec exception (W0.5 vs C2): 1.0.0 is the first public chart, and the values schema refuses unknown keys.
4. Stamped-pod snapshot adoption stays as the recovery path for a Workspace restored without status (Task 2).
5. The retained-attach repair becomes a fail-closed refusal (`lacks claim annotation`), not a deletion (Task 3).
6. The soak test's `/desktop` fallback stays (test tooling). Only its comment is reworded (Task 4).
7. `sessions.epoch DEFAULT ''` stays. It is a column default, not a dual read.
8. `internal/api/openapi.yaml` `info.version` becomes `1.0.0` (Task 6). Regenerate `web/src/api/generated/schema.d.ts` if the generator emits the version.
9. `capacity.md` keeps the 20-session numbers, labelled "not yet re-measured on the single backend".
10. A `SECURITY.md` rule with no test is kept and marked "*Not pinned by a test.*". The rule is not dropped.
11. The `E2E-V050-RAMP` tracking tag stays. C1 leaves tracking tags alone.

## Plan rulings

These are the planner's rulings, made against the code. Each line gives the ruling, then the reason, then the cost if it is wrong.

1. **The stream claim requires a valid tab id (Task 1).**
   - The gateway answers `400 {"error":"owner_tab_required"}` to a launch POST or a WebSocket upgrade that carries no valid `tcdi_tab`. The check runs before the ticket is redeemed and before the stream is claimed.
   - `Broker.ClaimStream` returns the new `ErrOwnerTabInvalid` without writing.
   - The column `connection_lease.stream_owner_epoch` is dropped.
   - Two CHECK constraints pin the shape: `stream_owner_tab` is 32 lowercase hex characters, and `stream_epoch = 0` exactly when `stream_owner_tab IS NULL`.

   Reason: the portal sends the id on every launch (`web/src/session/launch.ts:301`) and on every frame WebSocket (`launch.ts:192`). The NULL branch and the epoch guard served only clients and replicas from before the column existed (`sessions.go:343-390`, `leases.go:134-141`). Cost if wrong: a client that never sends the id cannot connect. The portal is the only client (spec §1).
2. **Snapshot adoption keeps only the stamped-pod proof (Task 2).** Every pod built since W0 carries the `template-hash`, `template-name` and `template-revision` stamps (`internal/runtime/linux/backend.go:120-135`, `:1223`). The pre-stamp ("v0.5 pod") branch is removed: the spec-rebuild proof, `StampPodTemplateIdentity` and `podSpecBuiltEqual`. An owned pod without a hash stamp now holds `Degraded/TemplateRevisionGone`. The stamped branch stays and is renamed `adoptSnapshotFromStampedPod`, because it is also the recovery for a Workspace whose status subresource was lost, for example a CR restored without status. The read fallbacks to the `template-snapshot` annotation are removed: `recordedSnapshot` (`workspace_controller.go:1039-1052`) and `K8sRunningSource.lifecycleFor` (`internal/broker/running_source.go:165-195`). The operator still writes the annotation as a mirror for human readers, and code never reads it. — Reason: Decision 4. Removing the stamped path would change what a restore does. — Cost if wrong: one branch that a restore never reaches.
3. **The stranded quota-row repairs are removed (Task 3).** This covers `repairRetainedDiskHolds`, `settleEmptyReservations`, the action kinds `ActionHoldRetainedDisk` and `ActionSettleReservation`, and the dead `Service.ReleaseQuota`, which has no caller. — Reason: every v1 release path holds the retained disk and settles the empty row in the same transaction (`recovery.go:251-269`, `retained.go:313-330`). — Cost if wrong: an empty held row would block nothing, because it holds zero resources. A retained disk that went uncounted would under-count quota. Task 3 Step 1 is the STOP check for this.
4. **`templateRevisionOf` returns `RevisionLabel` only (Task 3).** — Reason: `spec.revision` is required with `MinLength=1` (`api/v1/workspacetemplate_types.go:188-191`), so every v1 row carries the label. — Cost if wrong: an empty revision string shows in the UI.
5. **The chart's pre-1.0 values guard is removed (Task 3).** This covers `tinycdi.legacyValues` and the schema keys `api`, `gateway`, `portal`, `sessionHost` and `images.{api,gateway,portal}`. Top-level `additionalProperties: false` then rejects those keys as a plain schema error. — Reason: no public install ever had those keys. — Cost if wrong: a bare schema error instead of a hint.
6. **The allowlist gates share one engine, `.github/scripts/allowlist-grep.sh` (Task 4).**
   - Entries have the form `<glob> :: <ERE> :: <reason>`, and a reason needs at least 10 characters.
   - Globs follow bash `[[ == ]]` semantics, so `*` also matches `/`.
   - A stale entry, one that matches nothing, fails the gate on a full-scope run.
   - The C1 gate checks two patterns over Go, TS/TSX, the chart (excluding `*.md`) and `internal/api/openapi.yaml`. The first is `legacy|deprecated|v1alpha1`, case-insensitive (spec C1). The second catches internal version references in code comments; it is the spec's "comments that point at internal versions" made checkable. The version pattern skips `v0.` glued to `@`, `/`, `.` or `-` (module paths, image tags) but catches `pre-vN` and `Test…V0N` names.

   Reason: one tested engine instead of two. Cost if wrong: none.
7. **The docs gates (Task 5, full scope from Task 14).**
   - The link checker is `.github/scripts/check-doc-links.py`. It checks file targets and `#fragment`s against GitHub heading slugs. It also fails any link into `docs/superpowers/` from outside. The fence ignores git pathspec exclusions of that folder (`:(exclude…)`, `:!`), which the Task 6 and Task 13 gate scripts use.
   - The version gate is `.github/scripts/check-doc-versions.sh` over `docs/**/*.md` (minus `docs/superpowers/`), the root `*.md`, `deploy/helm/tinycdi/README.md` and `.github/README.md`.
   - Its PR-reference regex is refined to `(^|[^0-9A-Za-z&/_#-])#[0-9]{2,3}([^0-9A-Za-z_-]|$)`, so heading anchors like `#10-finding-tag-index` and hex colors do not trip it. It also rejects `v1alpha1`.
   - `.github/README.md` is added to the scope because spec C2 rewrites it.

   Reason: spec §7.7. Cost if wrong: an allowlist entry or two.
8. **ADR numbering (Task 6):**

   | Old | New | File after |
   |---|---|---|
   | 0001 runtime streaming | 0001 | `0001-runtime-streaming.md` (rewritten in English, Task 7) |
   | 0002 public API | 0002 | `0002-api-conventions.md` (replaced, Task 6) |
   | 0003–0007 | 0003–0007 | unchanged names |
   | 0008 KasmVNC brute force | — | deleted; content moves into the threat model (Task 6) |
   | 0009 mTLS cert revocation | 0008 | `0008-mtls-cert-revocation.md` |
   | 0010 session data-transfer policy | 0009 | `0009-session-data-transfer-policy.md` |
   | 0011 audit store and retention | 0010 | `0010-audit-store-and-retention.md` |

   Renames run in ascending order (0009, then 0010, then 0011), so a reference is never rewritten twice. All ADRs are in English. — Reason: spec C3. 0001 and 0002 are written in Vietnamese today, and the repo is public. — Cost if wrong: one more rename pass.
9. **Threat-model rows are renumbered `TM-01…TM-NN` in boundary order (Task 13).** Every `S<n>` reference outside `docs/security/` is updated. The old→new table goes in the commit message body only. — Reason: spec §3 asks for renumbering. A new prefix makes a stale `S17` easy to grep instead of silently meaning another row. — Cost if wrong: one more rename.
10. **`docs/security/review-v1.0.md` is deleted (Task 5).**
    - Inbound links are removed. `SECURITY.md` and `README.md` state the review status without a link.
    - `.github/workflows/release.yml:1355` and `.github/tests/release-notes-review-status.test.sh:16` are left unchanged. They print the path as text, and R1 writes the new report at the same path before any v1.0.0 tag.
    - The "frozen review record" guard in `.github/tests/failed-digest-cleanup.test.sh:161-170` is removed, because the record it froze is gone. That test's doc loop (`:178`) moves from `docs/releases/v1.1.0.md` to the new `docs/releases/v1.0.0.md`.

    Reason: spec C2 and §8 R1. Cost if wrong: R1 must re-add the links, which Task 14 Step 6 notes in the PR body.
11. **Tracking tags in code comments stay out of C1.** These are `SR-*`, `SEC-*`, `FX-R*`, `R-V*`, `V3.*`, `D<n>`, `E<n>` and similar, about 550 occurrences. They are not version references. `test-inventory.md` keeps a short glossary of the tag families. — Reason: scope. Spec C1 names version comments only. — Cost if wrong: noise for a reader.
12. **The one-time reset procedure is not product docs.** This is spec §8 R2–R3 (archive, force-push) and the R5 dev reinstall. W2 does not copy it into `docs/`. Generic release steps (tag, then the release workflow, then the `release` environment approval) must be in `.github/README.md` (Task 10). — Reason: product docs describe the product. — Cost if wrong: see Open questions.
13. **`docs/releases/v1.0.0.md` replaces the old internal file at the same path (Task 9).** — Reason: spec C2. — Cost if wrong: none.

## Global Constraints

- No upgrade path. Never add a compatibility alias, fallback branch, dual read or "legacy" handling for data or objects written by earlier builds (spec §1).
- Schema edits go into `internal/store/migrations/0001_baseline.sql`. After any edit, regenerate the snapshot with `TCDI_UPDATE_SNAPSHOT=1 go test ./internal/store/ -run TestBaseline_SchemaMatchesSnapshot -count=1`, then rerun without the variable.
- After any edit under `api/v1/` (comments included, because they render into CRD descriptions), run `make manifests generate sync-chart-crds`. The chart `crds/` directory must equal `config/crd/bases` byte for byte.
- After any change that alters the default chart render (templates, CRDs), regenerate the golden from `deploy/helm/` with `${TCDI_HELM:-helm} template tcdi ./tinycdi --namespace tcdi-system --include-crds -f tinycdi/ci/minimal-values.yaml > testdata/default-render.golden.yaml` (`deploy/helm/chart_defaultrender_test.go:13`).
- After any edit to `internal/api/openapi.yaml`, run `cd web && npm run generate`.
- Docs are English. They describe only v1.0.0. They carry no internal version history (`v0.x`, `v1.1`, `v1.2`, `rc.N`), no PR or issue numbers, and no review-tag prose ("fixed in", "since", "no longer", "previously").
- Every fact a doc states must be checkable in the code. Each doc task lists its facts with the source `file:line`. Re-check them with the given command before writing.
- Keep these machine-edited anchors byte-compatible in substance:
  - `docs/images.md`: the lines starting `- Chromium:` and `- Firefox ESR` (`.github/scripts/bump-browser-pin.sh:53-54`), and a mention matching `verify-blob|sigstore\.json` (`.github/tests/supply-chain-hardening.test.sh:124`).
  - `docs/compatibility.md`: the line containing `current ARG` and the table row starting `| Firefox ESR fallback |` with `deb sha256 \`<hex>\`` (`bump-browser-pin.sh:117-123`).
  - Every `kasmweb/*` reference in docs must stay a digest-pinned catalog entry (`.github/scripts/check-kasm-catalog.sh`).
  - `.github/README.md` keeps lines matching `cosign verify-blob .*runtime-images\.json`, `runtime-images\.yml@refs/heads/main` and `TRAIN_ATTESTATIONS_ENABLED`. `docs/security/provenance.md` keeps lines matching `pullable|digest-addressable` and `never (signed|tagged)|unsigned` (`.github/tests/supply-chain-hardening.test.sh:120-123,166-167,188-191`).
- `.github/tests` scripts:
  - Start with `#!/usr/bin/env bash` and `set -uo pipefail`.
  - Set `ROOT="$(cd "$(dirname "$0")/../.." && pwd)"`.
  - Print `FAIL: …` per failure and end with `exit "$fails"`.
  - Use only GNU grep, awk, sed, git and python3, with no network access and no `rg`.
  - The job runs them as `bash "$t"`, so the executable bit is optional.
- Tests that need Postgres start `postgres:18.0` through the package's own helper. They skip without Docker, and **a skip is not a pass**. Run them on a host with Docker.
- Do not edit `docs/superpowers/**`, `.github/workflows/release.yml` or any tag. Never push.
- Commit messages are conventional (`refactor:`, `docs:`, `test:`, `chore:`, `ci:`). Every commit ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **A KasmVNC in-frame reconnect or retry without `tcdi_tab` after Task 1.** Expected: every WebSocket the frame opens carries the id, because the gateway re-asserts it as the client's `path` setting on the launch redirect. This holds for both the linux-base client and the kasm adapter's unmodified client. A request that truly lacks the id gets `400 owner_tab_required` and never claims. Pinned in Task 1:
   - `TestLaunch_RedirectCarriesOwnerTabPath` (the valid-id case is now the only success);
   - `TestLaunch_RequiresOwnerTab` (the ticket is not burned);
   - `TestStreamClaim_RequiresOwnerTab`;
   - the existing `web/tests/unit/session/launch.test.ts:68,153`.

   No in-repo test drives a real KasmVNC client through the gateway. Both shipped clients (`@kasmtech/novnc` 1.3.0, in `tcdi/linux-base` and the catalog `kasmweb/chrome`) read `path` from the frame URL and reuse it on reconnect; this was checked inside the images during plan review. `TestKasmAdapterChromium` and CI's `kasm-contract` job cover only the adapter container. The browser checklist (spec §8 R5) is the end-to-end check.
2. **A Workspace whose status was lost while its stamped pod runs** (a CR restored without status). Expected: the snapshot is adopted from the live revision the pod names, and the pod is untouched. An owned pod with no hash stamp holds `Degraded/TemplateRevisionGone`, and its pod is untouched. Pinned in Task 2:
   - `TestAdoptSnapshot_StampedPodAdoptsLiveRevision` (kept);
   - `TestAdoptSnapshot_UnstampedPodHolds` (new);
   - `TestAdoptSnapshot_StampedPodWithoutHashHolds` (new);
   - `TestRecordedSnapshot_IgnoresAnnotation` (new).
3. **An ADR number in a code comment that means the pre-rename ADR.** Example: `ADR 0010 T11` in `internal/gateway/transfer.go` must become `ADR 0009 §5`, never `ADR 0008` or `ADR 0010`. Expected: every reference names the ADR whose title matches its topic. Pinned in Task 6: Step 4's count invariant (the reference count for each old number equals the count for its new number, renamed in ascending order so no number is renamed twice), and `adr-refs.test.sh`, which checks that every `ADR NNNN` reference and every `adr/NNNN-<slug>.md` path resolves and that the set is gap-free. Task 8 Step 2 checks each `ADR 0009 §N` topic against its section by hand.
4. **A doc link whose anchor points at a heading that a rewrite renamed.** Expected: the link checker fails it, naming the file, line and fragment. Pinned in Task 5 fixtures (`broken anchor` case). It is live from Task 5 on, so every later doc task runs it.
5. **An allowlist entry that silently covers nothing after cleanup, or one with no reason.** Expected: the gate fails with `stale allowlist entry` or `malformed allowlist entry`. Pinned in Task 4 fixtures (`stale`, `short reason`), shared by both gates.

---

## File map

| Area | Files |
|---|---|
| Stream owner (T1) | `internal/store/migrations/0001_baseline.sql`, `internal/store/testdata/schema.snapshot.sql`, `internal/broker/{sessions.go,tickets.go,leases.go,connection_state.go}`, `internal/gateway/{launch.go,proxy.go,authorization.go}`, `internal/api/openapi.yaml`, `web/src/api/generated/schema.d.ts`, `web/src/session/SessionPage.tsx`, `web/tests/mock-api/session.ts`, `internal/api/connection_status.go`, `internal/backend/backend_test.go`, `internal/gateway/brokerclient/{client.go,client_test.go}`, `tests/integration/*`, `web/tests-portal/harness.ts` |
| Snapshot (T2) | `internal/operator/workspace_controller.go`, `internal/runtime/{backend.go,linux/backend.go}`, `internal/broker/running_source.go`, `internal/operator/{snapshot_adopt_test.go,resnapshot_test.go,finalizer_test.go}`, `internal/broker/operator_stopped_envtest_test.go` (no `api/v1` edit) |
| Quota, revision, session-policy start, retained attach, chart (T3) | `internal/provisioning/{recovery.go,service.go,retained_applier.go,loop_hardening_test.go,template_family_test.go,retained_applier_test.go}`, `internal/api/workspaces.go`, `deploy/helm/tinycdi/{templates/_helpers.tpl,values.schema.json,testdata/schema-violation-values.yaml}`, `deploy/helm/{chart_test.go,chart_sessiondomain_test.go}` |
| C1 sweep + gate (T4) | about 70 Go/TS/chart files (list in Task 4), `web/scripts/screenshots.mjs`, `.github/scripts/allowlist-grep.sh`, `.github/tests/{legacy-shims.test.sh,legacy-shims.allow}` |
| Docs gates + deletions (T5) | `.github/scripts/{check-doc-links.py,check-doc-versions.sh}`, `.github/tests/{docs-gates.test.sh,docs-versions.allow,failed-digest-cleanup.test.sh}`, `docs/releases/*`, `docs/runbooks/upgrade.md`, `docs/security/review-v1.0.md`, `README.md`, `docs/README.md` |
| ADRs (T6–T8) | `docs/adr/*`, `.github/tests/adr-refs.test.sh`, every file referencing ADR 0008–0011 (about 75 files, 141 references after Task 5) |
| Release + contract docs (T9) | `docs/releases/v1.0.0.md`, `docs/versioning.md` (absorbs `upgrade.md`'s generic 1.x upgrade procedure), `SECURITY.md` |
| Install docs (T10) | `deploy/helm/tinycdi/README.md`, `docs/runbooks/install.md`, `docs/quickstart.md`, `.github/README.md`, `README.md` |
| Reference docs (T11) | `docs/{compatibility,images,kasm-images,architecture,development,lifecycle-reasons,branding}.md`, `docs/security/{provenance,vulnerability-policy}.md` |
| Runbooks (T12) | `docs/runbooks/{audit.md (new),session-policy,capacity,observability,disaster-recovery,backup-restore,retained-data,tenant-quotas,stuck-finalizer,mtls-cert-revocation}.md`, `docs/README.md`, `docs/releases/v1.0.0.md` (audit runbook link) |
| Security (T13–T14) | `docs/security/{threat-model,test-inventory}.md`, 40 files with `S<n>` references |

Task order: 1 → 2 → … → 14. Tasks 1–4 are independent of each other in content, but Task 4's gate assumes Tasks 1–3 have landed. Tasks 6–14 assume Task 5's gates exist.

---
### Task 1: Stream claims require the tab id; drop the pre-column epoch guard (C1) — tier: design

**Files:**
- Modify: `internal/store/migrations/0001_baseline.sql:57-66` (`connection_lease`); regenerate `internal/store/testdata/schema.snapshot.sql`
- Modify: `internal/broker/tickets.go:36-58` (new sentinel), `internal/broker/sessions.go:320-401` (`ownerTabOrNull` removed, `ClaimStream`), `internal/broker/leases.go:36-43,100-142` (`loadLease`), `internal/broker/connection_state.go:35-100` (`ConnectionState`)
- Modify: `internal/gateway/launch.go:116-137,303-310,378` (`desktopPath`, launch handler), `internal/gateway/proxy.go:705-745` (upgrade branch), `internal/gateway/authorization.go:430-445` (`setStreamOwner`), `internal/gateway/rehydrate.go:36-38` (interface comment only)
- Modify: `internal/api/connection_status.go:38-42` (`StreamOwnerTab` comment only)
- Modify: `internal/gateway/brokerclient/client.go:26` (`"io"` import), `:391-406` (`RevokeLeaseChanged`: the bodiless-204 branch removed); test `internal/gateway/brokerclient/client_test.go:237-239` (fake `/revoke` route), `:473-485` (`TestRevokeLeaseChanged`)
- Modify tests: `internal/broker/{connection_state_test.go,sessions_test.go,lease_session_test.go}` (`gatewayshim_internal_test.go` stays unchanged: its `ClaimStream(..., "")` calls hit a recording fake or the lease-id guard, never the broker), `internal/gateway/{helpers_test.go:380-415,505-525,622-720,owner_tab_test.go,launch_test.go:119-188,connheaders_test.go:96,fuzz_test.go:270}`, `tests/integration/{backend_restart_test.go:842-925,pre_stop_drain_window_test.go:21}`, `internal/backend/backend_test.go:728,752,885,907`
- Modify: `internal/api/openapi.yaml` (the `streamOwnerTab` description, lines 2998-3009), then `web/src/api/generated/schema.d.ts` (generated)
- Modify: `web/src/session/SessionPage.tsx:274-287` (comment only), `web/tests/unit/session/SessionPage.test.tsx:792,935` (renamed cases), `web/tests/mock-api/session.ts:44,186-197`, `web/tests-portal/harness.ts:150-158` (`openStream` takes a required tab)

**Interfaces:**
- Consumes: `broker.ValidStreamOwnerTab(s string) bool` and `broker.StreamOwnerTabLen = 32` (`sessions.go:320-340`, unchanged); gateway `streamOwnerTabParam = "tcdi_tab"` (`authorization.go:745`).
- Produces:
  - `var broker.ErrOwnerTabInvalid = errors.New("broker: stream claim needs a 32-hex tab id")`
  - `func (b *Broker) ClaimStream(ctx, gw GatewayIdentity, leaseID string, fence Fence, ownerTab string) (uint64, error)` — same signature; returns `ErrOwnerTabInvalid` before any read or write when `!ValidStreamOwnerTab(ownerTab)`
  - gateway error body `{"error":"owner_tab_required"}` with HTTP 400 on `POST /v1/launch` and on a WebSocket upgrade without a valid `tcdi_tab` query value
  - schema: `connection_lease.stream_owner_epoch` no longer exists; new constraints `connection_lease_stream_owner_tab_check` and `connection_lease_stream_owner_check`
  - test helpers in `internal/gateway/helpers_test.go`: `const testTabID = "0123456789abcdef0123456789abcdef"` (moved from `owner_tab_test.go:20`), `withTab(path string) string`, and `upgradeExact(t, srv, host, path, cookie, hdr)`, which sends the path verbatim. `upgrade` and `doLaunch` add the tab id when the path or query has none.
  - broker test const `testTab = "abababababababababababababababab"` (package `broker_test`, in `connection_state_test.go`).

How the frame keeps the id (Review Focus 1, checked at `b41be7d`): `desktopPath` (`launch.go:134-136`) appends `&path=websockify%3Ftcdi_tab%3D<id>` to the 303 `Location`. Both KasmVNC clients the gateway serves — `tcdi/linux-base` and the cataloged `kasmweb/chrome@sha256:4999d22e…` (each ships `@kasmtech/novnc` 1.3.0 under `/usr/share/kasmvnc/www`) — read `path` from the page URL's query (`initSetting("path","websockify")` → `.*[?&]path=([^&#]*)`, URI-decoded) and open `wss://<host>/<path>`, reconnects included. No automated test drives a real KasmVNC client through the gateway: `TestKasmAdapterChromium` (CI `kasm-contract`) checks the adapter container only, and the portal e2e (`web/tests-portal`) uses a fake upstream page that opens no WebSocket.

- [ ] **Step 1: Write the failing broker test**

Append to `internal/broker/connection_state_test.go`, and add `"errors"` to its import block (the file does not import it today). The file's package already has `setup`, `seedWorkspace`, `readyBinding` (`main_test.go`), `leaseForSess`, `fenceOf` (`leases_test.go`), and `ctx`, `alice`, `gwA` (`tickets_test.go`):

```go
// TestClaimStream_RequiresValidOwnerTab: a claim without a well-formed tab
// id is refused before anything is written — the epoch does not move and
// no owner is recorded.
func TestClaimStream_RequiresValidOwnerTab(t *testing.T) {
	const ws = "ws_0000000000000011"
	const portalSess = "portal-session-1"
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), ws)
	src.set(readyBinding(ws, "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	lease := leaseForSess(t, db, b, gwA, broker.PlatformID(ws), false, portalSess)

	for _, bad := range []string{"", "not-hex!", "0123456789abcdef", "0123456789ABCDEF0123456789ABCDEF"} {
		if _, err := b.ClaimStream(ctx, gwA, lease.ID, fenceOf(lease), bad); !errors.Is(err, broker.ErrOwnerTabInvalid) {
			t.Fatalf("ClaimStream(%q) err = %v, want ErrOwnerTabInvalid", bad, err)
		}
	}
	st, err := b.ConnectionState(ctx, broker.PlatformID(ws), portalSess)
	if err != nil || st.StreamEpoch != 0 || st.StreamOwnerTab != "" {
		t.Fatalf("after refused claims: %+v err=%v, want epoch 0 and no owner", st, err)
	}
}
```

- [ ] **Step 2: Run it and see it fail**

Run: `go test ./internal/broker/ -run TestClaimStream_RequiresValidOwnerTab -count=1`
Expected: FAIL to compile with `undefined: broker.ErrOwnerTabInvalid`. Docker is needed, because a skip is not a pass.

- [ ] **Step 3: Schema**

In `internal/store/migrations/0001_baseline.sql`, delete the line `    stream_owner_epoch bigint,` (line 60). Add a trailing comma to the last constraint line, `connection_lease_state_check` (line 64), and add these two constraints after it, before the closing `);` (alphabetical, the order `pg_dump` writes them in the snapshot):

```sql
    CONSTRAINT connection_lease_stream_owner_check CHECK (((stream_epoch = 0) = (stream_owner_tab IS NULL))),
    CONSTRAINT connection_lease_stream_owner_tab_check CHECK ((stream_owner_tab ~ '^[0-9a-f]{32}$'::text))
```

Both hold for every row any v1 code path writes: `ClaimStream` is the only writer of `stream_epoch` and `stream_owner_tab` (`rg -n 'stream_epoch|stream_owner_tab' -g '*.go' internal | grep -v _test`), and every `INSERT INTO connection_lease` (`tickets.go:595` and the test seeds) leaves both at their defaults (`0`, `NULL`). A NULL tab passes the regex CHECK (SQL CHECK accepts NULL), and `stream_epoch` is `NOT NULL`, so the second CHECK never evaluates to NULL.

In the file header comment (lines 1-9), keep the sentence about meaningful NULLs, but change "no stream owner" to "no stream claimed yet". Regenerate the snapshot (Docker; the store helper starts `postgres:18.0`):

```bash
TCDI_UPDATE_SNAPSHOT=1 go test ./internal/store/ -run TestBaseline_SchemaMatchesSnapshot -count=1
go test ./internal/store/ -run TestBaseline -count=1 -v 2>&1 | grep -E '^(--- |ok|FAIL)'
git diff internal/store/testdata/schema.snapshot.sql
```

Expected: four `--- PASS: TestBaseline_*` lines, and a snapshot diff of exactly: `-    stream_owner_epoch bigint,`, the `connection_lease_state_check` line gaining a trailing comma, and the two new `CONSTRAINT` lines.

- [ ] **Step 4: Broker**

`internal/broker/tickets.go`: add this to the sentinel `var (...)` block, after `ErrStaleBinding`:

```go
	// ErrOwnerTabInvalid — a stream claim without a well-formed tab id
	// (32 lowercase hex). Every claim names the claiming tab.
	ErrOwnerTabInvalid = errors.New("broker: stream claim needs a 32-hex tab id")
```

`internal/broker/sessions.go`: in the `StreamOwnerTabLen` comment (lines 320-322), drop "(see migration 018)". In the `ValidStreamOwnerTab` comment (lines 324-327), replace "it must be stored as NULL and never trusted as a match" with "a claim carrying it is refused (ErrOwnerTabInvalid)". Delete `ownerTabOrNull` (lines 342-351). In `ClaimStream`, rewrite the doc comment's last paragraph as: "ownerTab is the claiming tab's id. It must be well formed, or the claim is refused with ErrOwnerTabInvalid before anything is read. It lands on the lease in the same row update that bumps stream_epoch, so the epoch and its owner never diverge." Make the first statement of the body:

```go
	if !ValidStreamOwnerTab(ownerTab) {
		return 0, ErrOwnerTabInvalid
	}
```

Replace the UPDATE (lines 383-391) and its comment with:

```go
		if err := tx.QueryRow(ctx,
			`UPDATE connection_lease SET stream_epoch = stream_epoch + 1, stream_owner_tab = $2
			 WHERE id = $1 AND state = 'active' RETURNING stream_epoch`, l.ID, ownerTab).
			Scan(&epoch); err != nil {
```

`internal/broker/leases.go`:
- In `loadLease`, drop `ownerEpoch` from the `var` block, from the SELECT list (`l.stream_owner_epoch`) and from `Scan`.
- Replace lines 134-141 with:

  ```go
  	if ownerTab != nil {
  		l.StreamOwnerTab = *ownerTab
  	}
  ```

- Reword the `StreamOwnerTab` field comment (lines 38-43): "the tab id the current stream was claimed with; empty before the first claim. Gateway-side correlator only: never a metric label or log field."

`internal/broker/connection_state.go`:
- Make the same removal in `ConnectionState`: drop `ownerEpoch`, the column and the scan target.
- Set `st.StreamOwnerTab` from `ownerTab` when the portal-session gate holds, without the epoch comparison.
- In the `StreamOwnerTab` field comment (lines 38-44), replace "(empty without a lease, or when the claim carried no valid id)" with "(empty without a lease or before the first claim)" and delete "and only while the stored id still names the current stream (stream_owner_epoch = stream_epoch)".
- In the `ConnectionState` doc comment (lines 55-63), delete the last sentence, "It is also gated on stream_owner_epoch = stream_epoch: … read as absent."

`internal/api/connection_status.go:38-42` (`StreamOwnerTab` comment): drop "(migration 018)" and replace "Absent when the claim carried no id" with "Absent before the first stream claim".

Run: `go build ./internal/broker/ ./internal/api/ && rg -n 'stream_owner_epoch|ownerTabOrNull|predating the col|claim carried no|stored as NULL' internal/broker internal/api/connection_status.go -g '!*_test.go'` — expected: no output. (The test files are fixed in Step 7.)

- [ ] **Step 5: Gateway**

`internal/gateway/launch.go`: in the launch handler, directly after the `missing_ticket` check (line 306) and before `RedeemTicket` (line 309), add:

```go
	// Every launch names the claiming tab (the portal's submitLaunch puts it
	// on the POST's query). Refuse before redeeming, so a bad request never
	// burns the single-use ticket.
	ownerTab := r.URL.Query().Get(streamOwnerTabParam)
	if !broker.ValidStreamOwnerTab(ownerTab) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "owner_tab_required"})
		return
	}
```

At line 378, pass `ownerTab` instead of `r.URL.Query().Get(streamOwnerTabParam)`. In `desktopPath`, drop the `if broker.ValidStreamOwnerTab(ownerTab)` guard so the `&path=` suffix is always appended. Rewrite the doc paragraph (lines 122-126) to say the id is "re-asserted as the client's `path` setting so every WebSocket the frame opens claims under it", and drop the "(legacy) claim" sentence.

`internal/gateway/proxy.go`: in the `if isUpgrade(r)` block, after the `bad_origin` check (line 716) and before `admitUpgrade`, add:

```go
		ownerTab := r.URL.Query().Get(streamOwnerTabParam)
		if !broker.ValidStreamOwnerTab(ownerTab) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "owner_tab_required"})
			return
		}
```

Then call `g.claimStream(r.Context(), s, ownerTab)` at line 732. The check covers every upgrade the proxy admits: upgrades only reach this block on allowlisted paths (`upstreamPathOK`, `authorization.go:540-556`), and the only WebSocket either KasmVNC client opens is the `path` setting. Leave the frame-document `docOwnerTab` logic (lines 679-703) unchanged: document loads are not claims.

`internal/gateway/authorization.go:430-445`: `setStreamOwner` no longer normalizes, because its only caller passes a validated id. Delete the `if !broker.ValidStreamOwnerTab(ownerTab) { ownerTab = "" }` lines (440-442), and in its doc comment (lines 432-435) replace `("" when the claim carried no valid id, matching the NULL the broker stores)` with `(always a validated id)`.

`internal/gateway/rehydrate.go:36-38`: the `ClaimStream` interface comment becomes "ClaimStream bumps the lease's stream epoch for the claiming tab: ownerTab is the validated tab id the client sent on the upgrade; an invalid one is refused with broker.ErrOwnerTabInvalid."

No other list of gateway error codes exists: `rg -n '"stream_in_use"|"bad_form"|"missing_ticket"' -g '*.go' -g '*.ts' -g '*.tsx' -g '*.md' . | grep -v _test | grep -v docs/superpowers` shows only `launch.go:300,305` and `proxy.go:719`. The portal never reads the launch response (it is a form POST targeting the frame), so add no portal mapping.

- [ ] **Step 6: Gateway tests**

`internal/gateway/helpers_test.go`:
- Move `const testTabID = "0123456789abcdef0123456789abcdef"` here from `owner_tab_test.go:20`.
- Add:

  ```go
  // withTab appends this test's tab id to a request path that carries none —
  // every portal launch and frame WebSocket names its tab.
  func withTab(path string) string {
  	if strings.Contains(path, "tcdi_tab=") {
  		return path
  	}
  	sep := "?"
  	if strings.Contains(path, "?") {
  		sep = "&"
  	}
  	return path + sep + "tcdi_tab=" + testTabID
  }
  ```

- Make `doLaunch` (line 622) POST to `withTab("/v1/launch")`.
- Rename the current `upgrade` body to `upgradeExact`, and make `upgrade` call `upgradeExact(t, srv, host, withTab(path), cookie, hdr)`.
- `fakeBroker.ClaimStream` (line 404): make its first statement `if !broker.ValidStreamOwnerTab(ownerTab) { return 0, broker.ErrOwnerTabInvalid }`, mirroring the broker, and drop "(an invalid/empty claim stores NULL, i.e. no owner)" from the `ownerTabOf` comment (lines 515-518).
- Raw upgrade requests built outside these helpers: only `connheaders_test.go:96` (`TestProxy_UpgradeConnectionTokensCannotStripInjected`) needs `srv.URL+withTab("/websockify")`. `proxy_test.go:144` (h2c, refused as `bad_upgrade` first) and `:185` (stray `Upgrade` without `Connection`, not an upgrade) stay as they are.

`internal/gateway/launch_test.go` (the 303 now always ends in `&path=…`):
- `TestLaunch_RedirectLoadsDesktopWithRemoteResize` (line 119): after `q.Del("reconnect_delay")` (line 144), add `if got, want := q.Get("path"), "websockify?tcdi_tab="+testTabID; got != want { t.Fatalf("redirect path setting = %q, want %q", got, want) }` and then `q.Del("path")`, so the exact-params comparison at line 145 still holds.
- `TestLaunch_RedirectCarriesRecordedClipboardPolicy` (line 156): replace `!strings.HasSuffix(loc, tc.want)` (line 182) with `!strings.Contains(loc, tc.want+"&path=")`.

`internal/gateway/fuzz_test.go`: in `FuzzLaunchRedeem`'s seed list, add `{testHost, "POST", "tcdi_tab=" + testTabID, "ticket=" + validTicket, originOK},` after line 270. Without it no seed reaches the 303 branch, because every other seed now answers 400.

`internal/gateway/owner_tab_test.go`:
- Add `"io"` to its import block (the new tests read response bodies).
- Fix the file comment (lines 6-10) and the `TestLaunch_RedirectCarriesOwnerTabPath` comment (lines 42-47): delete "An absent or malformed id leaves `path` at the client's default (a legacy, NULL claim)."
- In `TestLaunch_RedirectCarriesOwnerTabPath`, keep the `valid id` case. Delete the `absent`, `malformed` and `too short` cases and the now-dead `if tc.want == ""` branch; the new `TestLaunch_RequiresOwnerTab` below covers those inputs.
- In `TestStreamClaim_CarriesOwnerTab`, keep only the `tab id` case. Delete `no id` and `malformed id passes through`, and drop "a missing one claims as legacy ("")" from its comment (lines 85-87).
- Add:

```go
// TestLaunch_RequiresOwnerTab: a launch POST without a well-formed tab id
// is refused with 400 owner_tab_required BEFORE the ticket is redeemed —
// the same ticket still launches once the tab id is present.
func TestLaunch_RequiresOwnerTab(t *testing.T) {
	for _, q := range []string{"", "tcdi_tab=not-hex!", "tcdi_tab=0123456789abcdef"} {
		t.Run("query="+q, func(t *testing.T) {
			fb := newFakeBroker(t)
			fb.scriptTicket("tk-rq", testWSUID)
			srv := newGateway(t, fb, nil)
			hdr := map[string]string{"Origin": testOrigin, "Sec-Fetch-Site": "same-origin"}
			resp := doLaunchQuery(t, srv, testHost, "tk-rq", q, hdr)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "owner_tab_required") {
				t.Fatalf("launch = %d %s, want 400 owner_tab_required", resp.StatusCode, body)
			}
			ok := doLaunchQuery(t, srv, testHost, "tk-rq", "tcdi_tab="+testTabID, hdr)
			defer drain(ok)
			if ok.StatusCode != http.StatusSeeOther {
				t.Fatalf("relaunch with a tab id = %d, want 303 (the refused launch must not burn the ticket)", ok.StatusCode)
			}
		})
	}
}

// TestStreamClaim_RequiresOwnerTab: an upgrade without a well-formed tab id
// is refused with 400 owner_tab_required and never reaches the claim.
func TestStreamClaim_RequiresOwnerTab(t *testing.T) {
	for _, path := range []string{"/websockify", "/websockify?tcdi_tab=zz"} {
		t.Run(path, func(t *testing.T) {
			fb := newFakeBroker(t)
			fb.scriptTicket("tk-rc", testWSUID)
			_, srv := newReplica(t, fb, "gw-1")
			cookie := launchOK(t, srv, testHost, "tk-rc")
			lease := fb.leaseOf(t, "tk-rc")
			resp := upgradeExact(t, srv, testHost, path, cookie, map[string]string{"Origin": testOrigin})
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "owner_tab_required") {
				t.Fatalf("upgrade %s = %d %s, want 400 owner_tab_required", path, resp.StatusCode, body)
			}
			fb.mu.Lock()
			_, claimed := fb.ownerTabs[lease.ID]
			fb.mu.Unlock()
			if claimed {
				t.Fatal("a refused upgrade reached the session directory")
			}
		})
	}
}
```

Run: `go test ./internal/gateway/... -count=1 > /tmp/w2-t1-gw.log 2>&1; echo rc=$?; grep -nE '^(--- FAIL|    --- FAIL|FAIL)' /tmp/w2-t1-gw.log | head -30`
Expected: `rc=0` and no FAIL lines (the package takes about 4 minutes). The log also holds an `http: panic serving … fake directory: lookup panicked` stack: that panic is injected on purpose by a rehydrate test and is not a failure. With Step 5 and the `helpers_test.go` edits in place but none of the other test edits, exactly these fail (checked on a scratch clone of `b41be7d`): `TestLaunch_RedirectLoadsDesktopWithRemoteResize`, `TestLaunch_RedirectCarriesRecordedClipboardPolicy`, `TestLaunch_RedirectCarriesOwnerTabPath/{absent,malformed,too_short}`, `TestStreamClaim_CarriesOwnerTab/{no_id,malformed_id_passes_through}` and `TestProxy_UpgradeConnectionTokensCannotStripInjected`; any other failure is a tab-less launch or upgrade the list above missed. Wrap its path in `withTab` unless the test is about refusal.

- [ ] **Step 7: Broker tests, integration, backend, OpenAPI, portal**

1. **Broker tests.** In `internal/broker/{sessions_test.go,lease_session_test.go,connection_state_test.go}` (all `package broker_test`):
   - Replace every `b.ClaimStream(..., "")` with `b.ClaimStream(..., testTab)` (`sessions_test.go:185,189,215,334,348,382,387,428,433`, `lease_session_test.go:178,210`, `connection_state_test.go:196`), and add the package-level `const testTab = "abababababababababababababababab"` to `connection_state_test.go`. Leave `gatewayshim_internal_test.go` (`package broker`) unchanged: its two `lg.ClaimStream(..., "")` calls (lines 118, 179) go to a `recordingSurface` fake or stop at `leaseIDOK`, never at `Broker.ClaimStream`.
   - `lease_session_test.go:178` expects `ErrRevoked` for a dead lease, so it must pass `testTab`. The tab check runs first.
   - In `connection_state_test.go`, delete `TestConnection_StreamOwnerEpochStale` (lines 273-305, with its comment) and the "legacy claim" and "malformed claim" blocks (lines 255-271) of `TestConnection_StreamOwnerTab`. In that test's comment (lines 209-214), replace "a claim without an id stores NULL — and" with "and". In the `TestValidStreamOwnerTab` comment (lines 307-309), replace "Anything else is stored as NULL and can never match a real tab" with "Anything else is refused at the claim". The Step 1 test covers both removed blocks.
2. **Integration.** The `restartFixture` helpers serve every fixture test (`TestRestart_*`, `TestTwoReplicas_*`, `TestPGOutage_*` in `backend_restart_test.go`, `pre_stop_drain_test.go`, `pre_stop_drain_window_test.go`, `postgres_outage_test.go`). In `tests/integration/backend_restart_test.go`:
   - In `launch` (line 842), POST to `r.sessURL+"/v1/launch?tcdi_tab="+restartTab` (line 845).
   - In `wsTry` (line 911), GET `r.sessURL+"/websockify?tcdi_tab="+restartTab` (line 912).
   - Add `const restartTab = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"` at file scope, commented "the stream-owner tab id the fixture launches and streams under".
   - In `pre_stop_drain_window_test.go`'s `launchTry` (line 21), POST to `r.sessURL+"/v1/launch?tcdi_tab="+restartTab`.

   **Backend.** In `internal/backend/backend_test.go`, `TestRun_DrainsOnShutdown` and `TestRun_DrainPropagationWait` launch and then upgrade on `base+"/"`. Change both launch URLs (lines 728, 885) to `base+"/v1/launch?tcdi_tab="+drainTab` and both upgrade URLs (lines 752, 907) to `base+"/?tcdi_tab="+drainTab`, with a file-scope `const drainTab = "efefefefefefefefefefefefefefefef"`. Leave `backend_test.go:389-391,596` unchanged: they assert 404 on listeners that do not mount these routes.
3. **OpenAPI.** In `internal/api/openapi.yaml`, replace the `streamOwnerTab` sentence "Absent when the claim carried no id (legacy client) and never served…" (lines 3006-3009) with "Absent before the first stream claim, and never served…" (keep the rest). Then run `cd web && npm run generate`; the only expected change in `web/src/api/generated/schema.d.ts` is that description.
4. **Portal.**
   - `web/src/session/SessionPage.tsx:286-287`: replace "Absent (a legacy claim stored NULL) falls back to the epochs." with "Absent (no claim yet, or a report for another portal session) falls back to the epochs."
   - In `web/tests/unit/session/SessionPage.test.tsx`, rename the case at 792 to `"a report without an owner id decides by epoch arithmetic"` and the one at 935 to `"without an owner id the epoch fallback still parks on 'elsewhere'"`. Remove "pre-FX-R31" and "legacy" from their comments. The behavior is unchanged: the portal still handles a missing id.
   - In `web/tests/mock-api/session.ts`, line 44's doc becomes `/** The tab id the current stream was claimed with; absent before the first claim. */`. In the `/_control/session/stream` handler (lines 186-197), make the first statement after the `leaseId === undefined` check `if (typeof req.body?.streamOwnerTab !== "string") return gwErr(400, "owner_tab_required");` (the gateway-shaped error helper from `./core.ts`, already imported), replace the two lines that set or delete the owner with `owners.set(leaseId, req.body.streamOwnerTab);`, and drop "a legacy claim: NULL, like the broker" and "(FX-R31)" from the comments.
   - In `web/tests-portal/harness.ts`, make `openStream`'s third parameter required (`streamOwnerTab: string`), send `data: { workspaceId, streamOwnerTab }`, and reword its comment (lines 150-152) to "…its stream epoch advances and the claiming tab's id lands on the same write…". Every caller (`session-reconnect.spec.ts:110,174`, `session-lease.spec.ts:115,154,161`) already passes a tab.

- [ ] **Step 8: Broker client — a revoke answer without a body is an error**

`(*Client).RevokeLeaseChanged` (`internal/gateway/brokerclient/client.go:391-406`) reads an empty 204 (`errors.Is(err, io.EOF)`, "a broker that predates the report") as "revoked". The broker's revoke handler always answers `200 {"revoked": <bool>}` (`internal/broker/httpapi/server.go:413-430`, `writeJSON(w, http.StatusOK, map[string]bool{"revoked": changed})` at `:429`), so that branch serves only an older broker. The only production caller is the gateway's revoke route (`internal/gateway/proxy.go:564`); every other `RevokeLeaseChanged` is a test fake (`internal/gateway/helpers_test.go:313`, `internal/backend/backend_test.go:210,454,1268`, `internal/backend/metrics_test.go:143`) and is unaffected.

1. **Failing test first.** In `internal/gateway/brokerclient/client_test.go`, in the fake's `/revoke` case (lines 237-239), replace the comment and the encode line with:

```go
			// A lease named "...-dead" is one the broker no longer holds live;
			// "...-empty" gets a bodiless 204, which the broker never sends.
			if strings.Contains(r.URL.Path, "-empty/") {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"revoked": !strings.Contains(r.URL.Path, "-dead/")})
```

   In `TestRevokeLeaseChanged` (line 475), append after the `lease-dead` check:

```go
	if changed, err := c.RevokeLeaseChanged(context.Background(), "lease-empty"); err == nil || changed {
		t.Fatalf("bodiless 204: changed=%v err=%v, want an error and false", changed, err)
	}
```

   and extend its comment (lines 473-474) with a third line: `// and an answer without that body is an error, never "revoked".`

   Run: `go test ./internal/gateway/brokerclient/ -run TestRevokeLeaseChanged -count=1` — expected: FAIL, `bodiless 204: changed=true err=<nil>, want an error and false`.

2. **Remove the branch.** Replace `RevokeLeaseChanged` (doc comment included, lines 391-406) with:

```go
// RevokeLeaseChanged revokes the lease and reports whether a live lease was
// actually revoked, from the broker's {"revoked": bool} answer. An answer
// without that body is an error.
func (c *Client) RevokeLeaseChanged(ctx context.Context, leaseID string) (bool, error) {
	var out struct {
		Revoked bool `json:"revoked"`
	}
	if err := c.do(ctx, "revoke", http.MethodPost,
		leasesPath+url.PathEscape(leaseID)+"/revoke", struct{}{}, &out); err != nil {
		return false, err
	}
	return out.Revoked, nil
}
```

   Delete the `"io"` import (line 26): the removed `io.EOF` was its only use. `errors` stays (used at `:69,78,90`). Leave `TestRevokeLease_204` (line 488) unchanged: `RevokeLease` passes no output value and ignores the body.

   Run: `go vet ./internal/gateway/brokerclient/ && go test ./internal/gateway/brokerclient/ -count=1` — expected: `ok` (checked on a scratch clone of `b41be7d`).

- [ ] **Step 9: Run the focused suites**

```bash
LOG=/tmp/w2-t1.log
{ go build ./... && go vet ./... && go vet -tags=integration ./tests/integration/... && go test -count=1 ./internal/broker/... ./internal/gateway/... ./internal/store/... ./internal/backend/... ./internal/api/...; } > "$LOG" 2>&1; echo "rc=$?"; grep -nE '^(--- FAIL|    --- FAIL|FAIL)' "$LOG" | head -30
KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path --bin-dir "$PWD/bin")" go test -tags=integration -count=1 -v -run '^(TestRestart_|TestTwoReplicas_|TestPGOutage_)' ./tests/integration/... > /tmp/w2-t1-it.log 2>&1; echo "rc=$?"; grep -nE '^(--- FAIL|--- SKIP|FAIL)' /tmp/w2-t1-it.log | head
(cd web && npm ci && npm run typecheck && npx vitest run tests/unit/session > /tmp/w2-t1-web.log 2>&1; echo "rc=$?"; grep -nE 'FAIL|✗' /tmp/w2-t1-web.log | head)
(cd web && npx playwright test --config playwright.portal.config.ts > /tmp/w2-t1-pw.log 2>&1; echo "rc=$?"; grep -nE '✘|failed' /tmp/w2-t1-pw.log | head)
```

Expected: `rc=0` four times, and no `--- SKIP` in the integration log (the fixture tests need Docker; a skip is not a pass). The integration run took about 5 minutes on a scratch clone with these edits, all passing. The portal e2e (`web/tests-portal`, `playwright.portal.config.ts`) builds and runs the real `cmd/backend`, so it exercises the new launch check through the portal's own `submitLaunch`; it needs Go, openssl and the Playwright browsers (`npx playwright install chromium` if missing). `tests/session-policy.spec.ts` under the default config uses the mock API only and does not cover this change.

No automated test runs a real KasmVNC client through the gateway (see the note under **Interfaces**), so the client side of Review Focus 1 rests on the checked client code. Do not cite `TestKasmAdapterChromium` or CI `kasm-contract` as coverage for the `path` setting: it tests the adapter container only. Write in the commit body: "KasmVNC clients (linux-base, kasmweb/chrome catalog digest) read `path` from the frame URL; no in-repo test drives a real client through the gateway."

- [ ] **Step 10: Commit**

```bash
git add -A internal/store internal/broker internal/gateway internal/backend internal/api tests/integration web/src web/tests web/tests-portal
git commit -m "refactor(broker,gateway): require the stream-owner tab id on every claim

Drop the NULL-claim branch and the stream_owner_epoch guard, which served
only clients and replicas without the owner columns. Launch and upgrade
without a valid tcdi_tab now answer 400 owner_tab_required, before the
ticket is redeemed. The broker client no longer reads a bodiless revoke
answer as revoked; the broker always reports {"revoked": bool}.

KasmVNC clients (linux-base, kasmweb/chrome catalog digest) read path
from the frame URL; no in-repo test drives a real client through the
gateway.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Snapshot adoption keeps only the stamped-pod proof; annotation fallbacks removed (C1) — tier: design

**Files:**
- Modify: `internal/operator/workspace_controller.go:74-79` (`AnnotationTemplateSnapshot` comment), `:110-118` (`ReasonTemplateRevisionGone` comment), `:436-455` (call site), `:1033-1052` (`recordedSnapshot`), `:1082-1086` (`mirrorSnapshotAnnotation` comment), `:1099-1210` (`adoptSnapshotFromPod`, renamed `adoptSnapshotFromStampedPod`), `:1284-1288` (`holdTemplateRevisionGone` comment)
- Modify: `internal/runtime/backend.go:87-121` (interface: drop `StampPodTemplateIdentity`; reword `PodMatchesTemplate` and the `PodTemplateIdentity.Name` comment), `internal/runtime/linux/backend.go:120-135` (stamp comments), `:1382-1413` (`PodMatchesTemplate`), `:1444-1447` (`PodTemplateIdentity` comment), `:1460-1478` (`StampPodTemplateIdentity`, deleted), `:1480-1531` (`podSpecBuiltEqual` and `stripAdmissionTolerations`, deleted; both have no other caller)
- Modify: `internal/broker/running_source.go:102-109` (`snapshotPolicy`, deleted), `:111-128` (`policyFor` comment), `:149-154` (`snapshotHeld` comment), `:165-195` (`lifecycleFor`)
- Modify tests: `internal/operator/{snapshot_adopt_test.go,resnapshot_test.go,finalizer_test.go:101-104}`, `internal/broker/operator_stopped_envtest_test.go:244-338` (`TestK8sRunningSource_PolicyFromRecordedSnapshot`, which seeds the snapshot through the annotation today). `finalizer_test.go`'s `fakeBackend` is the only test implementation of `runtime.Backend` with a `StampPodTemplateIdentity` method (`progress_reasons_test.go:39` embeds the interface and needs no change), and `internal/runtime/linux` has no `PodMatchesTemplate` test.
- No change to `api/v1/workspace_types.go`: its comments at lines 165-171 and 258-262 already call the annotation a read-only mirror, which stays true. So no `make manifests`, CRD sync or golden regeneration is needed.

**Interfaces:**
- Consumes: `runtime.PodTemplateIdentity{Owned bool; Name, Revision string}` from `(*linux.Backend).PodTemplateIdentity` (`linux/backend.go:1448`), `holdTemplateRevisionGone`, `holdTemplateInvalid`, `snapshotTemplate`, and `recordSnapshot`, all unchanged.
- Produces:
  - `func (r *WorkspaceReconciler) adoptSnapshotFromStampedPod(ctx context.Context, ws *cdiv1.Workspace, applied *AppliedIntent) (snap *templateSnapshot, res ctrl.Result, done bool, err error)`, with the same return contract as today
  - `runtime.Backend` without `StampPodTemplateIdentity`
  - `PodMatchesTemplate` true only for an owned pod whose `template-hash` stamp equals `templateHash(tpl)`
  - `recordedSnapshot(ws) *templateSnapshot`, which returns `ws.Status.TemplateSnapshot` or nil

- [ ] **Step 1: Write the failing tests**

In `internal/operator/snapshot_adopt_test.go`, add:

```go
// An owned pod without the identity and hash stamps cannot prove its
// revision: the workspace holds Degraded/TemplateRevisionGone and the pod
// is left exactly as it was. Every pod this operator builds carries the
// stamps.
//
// templateRef names the exact revision object the pod was built from, so
// the removed spec-rebuild proof would have matched and adopted it: this is
// the case that proves the proof is gone.
func TestAdoptSnapshot_UnstampedPodHolds(t *testing.T) {
	s := snapScheme(t)
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
	ws := adoptWorkspace("fam32-aaaa1111", "", types.UID("ws-unstamped"))
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(ws, tpl).WithStatusSubresource(ws).Build()
	pod := seedBuiltPod(t, c, ws, tpl)
	unstamp(t, c, pod) // existing helper: drops name, revision and hash stamps and updates
	before := pod.ResourceVersion

	got := adoptReconcile(t, s, c, ws)
	assertRevisionGone(t, got) // also fails if a snapshot was recorded
	after := &corev1.Pod{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), after); err != nil {
		t.Fatalf("pod gone: %v", err)
	}
	if after.ResourceVersion != before {
		t.Fatal("the held workspace's pod was modified")
	}
}

// A pod with the name/revision stamps but no template-hash stamp cannot
// prove its content either: PodMatchesTemplate is the hash comparison
// alone, so the workspace holds and the pod is left as it was.
func TestAdoptSnapshot_StampedPodWithoutHashHolds(t *testing.T) {
	s := snapScheme(t)
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
	ws := adoptWorkspace("fam32", "", types.UID("ws-nohash"))
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(ws, tpl).WithStatusSubresource(ws).Build()
	pod := seedBuiltPod(t, c, ws, tpl)
	delete(pod.Annotations, linux.AnnotationTemplateHash)
	if err := c.Update(context.Background(), pod); err != nil {
		t.Fatalf("drop hash stamp: %v", err)
	}
	before := pod.ResourceVersion

	got := adoptReconcile(t, s, c, ws)
	assertRevisionGone(t, got)
	after := &corev1.Pod{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), after); err != nil {
		t.Fatalf("pod gone: %v", err)
	}
	if after.ResourceVersion != before {
		t.Fatal("the held workspace's pod was modified")
	}
}

// recordedSnapshot reads status only: the template-snapshot annotation is a
// mirror for human readers and is never a source.
func TestRecordedSnapshot_IgnoresAnnotation(t *testing.T) {
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
	ws := adoptWorkspace("fam32", "", types.UID("ws-anno"))
	ws.Annotations = map[string]string{AnnotationTemplateSnapshot: snapshotAnnotation(t, tpl, "fam32-aaaa1111", 1)}
	if got := recordedSnapshot(ws); got != nil {
		t.Fatalf("recordedSnapshot = %+v, want nil without status", got)
	}
}
```

Use the existing helpers in `snapshot_adopt_test.go`: `unstamp(t, c, pod)` (line 344, it updates the pod in the fake client, so `pod.ResourceVersion` is the stored one afterwards), `assertRevisionGone(t, got)` (line 628; it also fails when `status.templateSnapshot` is set), `adoptTemplate` (44), `adoptWorkspace` (70), `snapshotAnnotation` (99), `seedBuiltPod` (117) and `adoptReconcile` (134); `snapScheme` is in `snapshot_verify_test.go:41`. The file already imports `linux`, `corev1`, `client`, `fake` and `types`.

Do not use `adoptWorkspace("fam32", …)` for the unstamped case: `resolveTemplate` reads `spec.templateRef` by exact name, `fam32` names no object, and today's code already holds that case, so the test would pass before the change.

- [ ] **Step 2: Run them and see them fail**

Run: `go test ./internal/operator/ -run 'TestAdoptSnapshot_UnstampedPodHolds|TestAdoptSnapshot_StampedPodWithoutHashHolds|TestRecordedSnapshot_IgnoresAnnotation' -count=1 -v`
Expected: three FAILs (checked on a scratch clone of `b41be7d`). The first two report `want Degraded=True reason=TemplateRevisionGone` because today's spec-rebuild proof adopts the pod; the third reports the annotation's snapshot.

- [ ] **Step 3: Operator**

In `internal/operator/workspace_controller.go`:
- Rename `adoptSnapshotFromPod` to `adoptSnapshotFromStampedPod` (definition plus the call at line 443). Replace its doc comment with:

  ```go
  // adoptSnapshotFromStampedPod recovers status.templateSnapshot for a
  // workspace whose status was lost (for example a CR restored without its
  // status subresource) while its incarnation pod still runs. The pod's
  // operator-written template-name/template-revision stamps name the
  // candidate revision; the LIVE object of that revision supplies the
  // content, and the pod's template-hash stamp must equal its hash. The
  // writer-controlled template-snapshot annotation is never consulted.
  //
  // Returns (nil, _, false, nil) when there is no owned pod: the caller
  // takes a fresh snapshot of the live template. When the pod exists but
  // cannot prove its revision, done is true and the returned ctrl.Result is
  // the Degraded/TemplateRevisionGone hold — the pod is left untouched.
  ```

- In the body, replace the `var live …; if ident.Name == "" { …resolveTemplate… } else { … }` block (lines 1142-1167) with `if ident.Name == "" { return gone("the running pod carries no template identity stamp") }`, then `live := &cdiv1.WorkspaceTemplate{}` and the former `else` body (the `r.Get` by `ident.Name` and the revision check) at function level. Delete the stamp backfill `if ident.Name == "" { … StampPodTemplateIdentity … }` (lines 1187-1195). Keep the seccomp content gate and `PodMatchesTemplate`. In the comment above `PodMatchesTemplate` (lines 1176-1179), replace "hash stamp, or an exact spec rebuild for pods built before the stamps —" with "its template-hash stamp —". `resolveTemplate` keeps its other caller (line 483).
- In the comment at lines 437-442, the call site now reads "Status-loss recovery: …", using the first paragraph of the new doc comment.
- `recordedSnapshot`: the body becomes `return ws.Status.TemplateSnapshot`. Its doc comment (lines 1033-1038) becomes "recordedSnapshot returns the operator-recorded template snapshot, status.templateSnapshot, or nil. The annotation mirror is never read." Its callers (`lifecycle.go:133` deadline requeues, `retention.go:156` retained-disk runtime label) then read status only.
- Comments that describe the removed upgrade path:
  - `AnnotationTemplateSnapshot` (lines 74-78): replace "for consumers that predate the status field" with "for human readers".
  - `ReasonTemplateRevisionGone` (lines 110-117): replace "upgrade-path hold: a running workspace" with "status-loss hold: a running workspace", and "(the pod predates the identity stamp, or the stamped revision object was pruned or republished)" with "(the pod carries no identity or hash stamp, or the stamped revision object was pruned or republished)".
  - `mirrorSnapshotAnnotation` (lines 1082-1086): replace "for consumers that predate the status field (the broker's expiry projection reads status first and falls back to the annotation only for pre-upgrade rows)" with "for human readers; no code reads it".
  - `holdTemplateRevisionGone` (lines 1284-1288): replace "upgrade-path hold" with "status-loss hold" and "the pod predates identity stamps" with "the pod carries no identity or hash stamp".

- [ ] **Step 4: Runtime backend**

- In `internal/runtime/backend.go`:
  - Delete `StampPodTemplateIdentity` and its comment (lines 104-109) from the `Backend` interface.
  - Replace the `PodMatchesTemplate` comment (lines 87-94) with "PodMatchesTemplate reports whether the workspace's current runtime incarnation exists, is owned by ws (controller ownerRef plus the managed label set), and carries a template-hash stamp equal to tpl's hash. It is the content proof for status-loss recovery; a false answer means the stamped revision may NOT be trusted."
  - In the `PodTemplateIdentity.Name` comment (lines 119-121), replace "Empty means the pod predates the stamp — its revision cannot be proven." with "Empty means the pod carries no identity stamp — its revision cannot be proven."
- In `internal/runtime/linux/backend.go`:
  - In `PodMatchesTemplate`, replace the pre-stamp block (lines 1402-1412, from `// Pre-stamp pod:` through `return podSpecBuiltEqual(…)`) with `return false, nil`, and replace its doc comment (lines 1382-1389) with "PodMatchesTemplate implements runtime.Backend: the incarnation pod must exist, be operator-owned (controller ownerRef to this Workspace UID plus the managed label set for the live generation) and carry a template-hash stamp equal to templateHash(tpl). false means the stamped revision may not be trusted."
  - Delete `StampPodTemplateIdentity` with its comment (lines 1460-1478), and `podSpecBuiltEqual` and `stripAdmissionTolerations` with their comments (lines 1480-1531). Neither has another caller. Keep the `apiequality` import: `ensureNetPol` still uses it (about line 1506 before the deletion).
  - In the `PodTemplateIdentity` comment (lines 1444-1447), replace "Empty Name/Revision mark a pod that predates the stamps" with "Empty Name/Revision mark a pod without the identity stamps".
  - In the `AnnotationTemplateHash` comment (lines 120-124), replace "the snapshot-upgrade adoption proof compares it against a candidate record before trusting a pre-status workspace's annotation" with "status-loss recovery compares it against the live revision the identity stamps name". In the `AnnotationTemplateName` comment (lines 127-135), replace "On upgrade the operator reads" with "When status.templateSnapshot is lost the operator reads".
- In `internal/operator/finalizer_test.go`, delete `fakeBackend.StampPodTemplateIdentity` and its comment (lines 101-104).

- [ ] **Step 5: Broker running source**

In `internal/broker/running_source.go`:
- `lifecycleFor` (line 174): delete the annotation branch (`if !held { … }`, lines 181-192) and the `held` parameter. Its only caller is `policyFor` (line 132): call `s.lifecycleFor(ctx, ws)`. `policyFor` keeps `held := snapshotHeld(ws)` for the stricter-cap rule.
- The `lifecycleFor` doc comment (lines 165-173) becomes: "lifecycleFor returns the recorded snapshot lifecycle, else the resolved template's, else nil. Only the operator-recorded status.templateSnapshot is trusted (workspaces/status is writable by the operator service account alone); the annotation mirror is never read."
- Delete the `snapshotPolicy` type and its comment (lines 102-109); nothing else uses it. Keep the `encoding/json` import (line 81 still unmarshals the applied-intent annotation) and the `operator` import (`snapshotHeld` uses `operator.ReasonTemplate*`).
- In the `policyFor` comment (lines 111-128), replace "(never admitted, or the annotation was lost)" with "(never admitted, or its status was lost)" and "are the one case where the annotation is provably untrusted: their caps come from" with "take their caps from". In the `snapshotHeld` comment (lines 149-154), delete the last sentence ("While held, the snapshot annotation is provably untrusted — … from it.").

- [ ] **Step 6: Tests to delete or adjust**

- In `internal/operator/snapshot_adopt_test.go`, delete these tests (with their leading comments), which only exercised the removed pre-stamp proof. Step 1's two hold tests replace the first:
  - `TestAdoptSnapshot_ScheduledUnstampedHashAdopts` (194)
  - `TestAdoptSnapshot_UnstampedPodCandidateMatch` (359)
  - `TestAdoptSnapshot_UnstampedPodCandidateMismatch` (414)
  - `TestAdoptSnapshot_UnstampedPodRotatedMismatch` (466)
  - `TestAdoptSnapshot_UnstampedPodRotatedIdentical` (497)
- Keep `TestAdoptSnapshot_StampedPodAdoptsLiveRevision`, `_AnnotationNeverConsulted`, `_ForgedNetworkProfileLeavesNetPol`, `_RevisionGone`, `_RequiresPod`, `_ForeignPodNoAdopt` and the `unstamp` helper (Step 1 uses it). In `_AnnotationNeverConsulted` (221-270), delete the "Pre-V3.2 legacy shape" block (the `legacy`/`legacyRaw` variables) and its `"legacy shape"` map entry, and drop "or legacy-shaped" from its comment (line 219).
- Replace the file comment of `snapshot_adopt_test.go` (lines 4-15) with: "Status-loss recovery for status.templateSnapshot. A running workspace without the status record re-establishes it from the RUNNING incarnation, never from the annotation: the operator-owned pod names its template revision in stamped metadata (workspaces.cdi.tinyorbit.vn/template-name|template-revision), the LIVE revision object supplies the content, and the pod's template-hash stamp must match it. A pod that cannot prove its revision — no identity or hash stamp, a pruned or republished object, a mismatched hash — holds the workspace Degraded/TemplateRevisionGone with the pod left running; a workspace with no pod re-snapshots the live template. The annotation is never consulted."
- Reword the helper comments so they do not name builds: `adoptTemplate` (lines 41-43: "the revision object a workspace was admitted under"), `adoptWorkspace` (lines 67-69: "a Running workspace whose status was lost — no status.templateSnapshot"), `snapshotAnnotation` (lines 96-98: "the annotation bytes the operator mirrors — used to prove the annotation is never read"), `unstamp` (line 343: "an owned pod without the identity stamps").
- `internal/operator/resnapshot_test.go`, `TestSnapshot_UpgradeAdoption` (line 279; rename it `TestSnapshot_StatusLossAdoption`):
  - Subtest `"pre-stamp pod adopts resolved revision, pod untouched"` (lines 489-528): rename it `"unstamped pod holds degraded even when it matches, pod untouched"`. Keep the setup through `seedAnnotation(…)` (line 509), then add `markPodReady(t, c, pod)` and replace everything after `reconcile(t, r, key)` with `assertPodHeldGone(t, key, pod)` (the helper at line 347 needs a Ready pod). Replace the setup comment (lines 499-501) with "An owned pod without identity stamps cannot prove its revision, even when templateRef names the exact object it was built from; its annotation is never a source."
  - Subtest `"pre-stamp pod rotated revision holds degraded"` (line 530): rename it `"unstamped pod under a re-pointed ref holds degraded"`; its body stays.
  - `seedRunning`'s `scheduled.Spec.DeprecatedServiceAccount = …` (line 322) only faked a scheduler default for the removed spec-rebuild compare. Delete it, so Task 4's allowlist needs no entry for it here.
  - Replace the test's comment (lines 272-278) with "TestSnapshot_StatusLossAdoption: status-loss recovery end to end on a real apiserver. A running workspace without status.templateSnapshot re-establishes its record from the incarnation pod's stamped template identity + the LIVE revision object — the annotation is never consulted. Pods without the stamps, or whose stamped revision is gone, hold Degraded/TemplateRevisionGone with the pod left running; stop/start re-snapshots." Leave the file comment (lines 4-9) alone: it is about re-snapshotting, not adoption.
- `internal/operator/snapshot_adopt_test.go:160` (`TestAdoptSnapshot_StampedPodAdoptsLiveRevision`) sets `pod.Spec.DeprecatedServiceAccount` on a stamped pod: delete that line too, for the same reason.
- `internal/broker/operator_stopped_envtest_test.go`, `TestK8sRunningSource_PolicyFromRecordedSnapshot` (line 244): it seeds the recorded lifecycle through the annotation, so it fails once `lifecycleFor` stops reading it. Seed `status.templateSnapshot` instead. Replace the `snapJSON` block (lines 276-290) with:

  ```go
  	// The admitted snapshot (status.templateSnapshot, recorded at first
  	// admit) carries the lifecycle the workspace actually runs under.
  	admitted := tpl.Spec.DeepCopy()
  	admitted.Revision = "r1"
  	admitted.Lifecycle = cdiv1.LifecycleDefaults{
  		IdleTimeout:       metav1.Duration{Duration: 7 * time.Minute},
  		DisconnectTimeout: metav1.Duration{Duration: 2 * time.Minute},
  		MaxDuration:       metav1.Duration{Duration: time.Hour},
  		DataPolicy:        cdiv1.DataPolicyEphemeral,
  	}
  	snap := &cdiv1.TemplateSnapshot{
  		Name: "linux-desktop-aaaa1111", UID: "uid-r1", Revision: "r1",
  		SpecHash: "sha256:" + fmt.Sprintf("%064x", 1), Spec: *admitted,
  		RuntimeGeneration: 1, SourceRef: "linux-desktop",
  	}
  ```

  Delete the `Annotations:` line of the workspace (line 295), and set `TemplateSnapshot: snap` in the `ws.Status = cdiv1.WorkspaceStatus{…}` literal (line 313). The CRD requires `sourceRef` (min length 1), `name`, `uid`, `revision`, `specHash` and `spec` on the status record. `TestK8sRunningSource_HeldWorkspaceNeverReadsAnnotation` (line 348) stays: it still proves held workspaces ignore the annotation.

- [ ] **Step 7: Run**

```bash
make setup-envtest > /dev/null && export KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path --bin-dir "$PWD/bin")"
LOG=/tmp/w2-t2.log
{ go build ./... && go vet ./... && go test -count=1 ./internal/operator/... ./internal/runtime/... ./internal/broker/...; } > "$LOG" 2>&1; echo "rc=$?"; grep -nE '^(--- FAIL|    --- FAIL|--- SKIP|FAIL)' "$LOG" | head -30
go test -tags=integration -count=1 -skip '^TestLinuxRuntime(ReadinessAndHome|FirefoxFallback|Desktop|BrowserBaseline)$|^TestKasmAdapter' ./tests/integration/... > /tmp/w2-t2-it.log 2>&1; echo "rc=$?"; grep -nE '^(--- FAIL|FAIL)' /tmp/w2-t2-it.log | head
grep -rnE 'adoptSnapshotFromPod|StampPodTemplateIdentity|podSpecBuiltEqual|stripAdmissionTolerations|snapshotPolicy|[Pp]re-stamp' --include='*.go' internal tests cmd api; echo "leftovers rc=$?"
git status --porcelain api config deploy
```

Expected: `rc=0` twice, no FAIL or SKIP lines (`internal/operator`'s envtest suites fail, and `internal/broker`'s envtest suites skip, without `KUBEBUILDER_ASSETS`; the operator package takes about 6 minutes), `leftovers rc=1` (no matches), and an empty `git status` for `api`, `config` and `deploy` (Task 2 changes no CRD text).

- [ ] **Step 8: Commit**

```bash
git add -A internal/operator internal/runtime internal/broker
git commit -m "refactor(operator): adopt a lost snapshot only from a stamped pod

Remove the spec-rebuild proof for pods without identity stamps, the stamp
backfill, and the template-snapshot annotation fallbacks in
recordedSnapshot and the running-set lifecycle. Every pod this operator
builds carries the stamps; an unstamped owned pod now holds
TemplateRevisionGone.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Quota stranded-row repairs, the numeric revision fallback and the chart's pre-1.0 values guard removed (C1) — tier: integrate

**Files:**
- Modify: `internal/provisioning/recovery.go:47-66` (action kinds), `:308-347` (`repairRetainedDiskHolds`, deleted), `:349-378` (`settleEmptyReservations`, deleted), `:544-566` (their calls in `Recover`)
- Modify: `internal/provisioning/service.go:805-811` (`(*Service).ReleaseQuota`, deleted: no callers), `:159-161` (`RevisionLabel` comment)
- Modify: `internal/api/workspaces.go:349-359` (`templateRevisionOf`) and its `"strconv"` import (the fallback is its only use)
- Modify: `internal/provisioning/service.go:567-576,742-770` (`startSessionPolicy`: catalog fallback and nil result removed), `internal/provisioning/retained_applier.go:134-159` (`applyAttach`: claim-annotation repair becomes a refusal); tests: new `TestStart_PolicyFromRecordedSnapshotOnly` in `internal/provisioning/template_family_test.go`, new `TestRetainedApplierRefusesUnstampedCR` in `internal/provisioning/retained_applier_test.go`
- Modify tests: `internal/provisioning/loop_hardening_test.go:171-222` (`TestRecovery_SettlesStrandedEmptyRows`, deleted) and its header comment `:9-10`; `tests/integration/delete_finalization_test.go:294-328` (`TestDeleteFinalization_StuckRowSelfHeals`, deleted: it seeds a released row and expects `repairRetainedDiskHolds` to re-hold it); new test in `internal/provisioning/recovery_test.go`; new test `internal/api/templaterevision_test.go`
- Modify: `deploy/helm/tinycdi/templates/_helpers.tpl:171-174` (the `chart 0.2.0` invariant bullet), `:231` (include), `:485-512` (`tinycdi.legacyValues`, deleted); `deploy/helm/tinycdi/values.schema.json:26-28,102` (and the trailing comma on `"browser"`, line 25) and the top-level `api` (273), `gateway` (328) and `portal` (358) objects; `deploy/helm/tinycdi/testdata/schema-violation-values.yaml`; `deploy/helm/chart_test.go:3083-3103` (`TestRemovedValuesFail`); `deploy/helm/chart_sessiondomain_test.go:88-98` (`TestSessionHostValueFails`)

**Interfaces:**
- Consumes: `(*Recovery).SettleQuota` (`recovery.go:231`), `holdRetainedDisk` (`:294`), `releaseDiskQuota` (`retained.go:313`), `restoreDiskQuota` (`retained.go:341`), all unchanged.
- Produces:
  - `RecoveryActionKind` values are `redeliver_intent`, `release_quota`, `hold_quota` and `quarantine_intent` only
  - `templateRevisionOf(t *provisioning.TemplateInfo) string` returns `t.RevisionLabel`
  - chart: setting `api.*`, `gateway.*`, `portal.*`, `sessionHost` or `images.{api,gateway,portal}` fails as a schema violation

- [ ] **Step 1: STOP check — every v1 release keeps the retained disk counted in the same transaction**

```bash
rg -n '\bRelease\(ctx, tx|ReleaseQuota\(' -g '*.go' internal tests
rg -n 'releaseDiskQuota\(|restoreDiskQuota\(|holdRetainedDisk\(' -g '*.go' internal
```

Expected:
- In non-test code, `Release(ctx, tx` is called only at `recovery.go:264`, followed in the same `WithTx` closure by `holdRetainedDisk` at `:269`, and inside `(*Service).ReleaseQuota` (`service.go:809`). The test callers `tests/integration/api_admission_test.go:402,412,421` and `internal/provisioning/loop_hardening_test.go:131` are expected and are not release paths.
- `.ReleaseQuota(` has no caller.
- `releaseDiskQuota` settles an all-zero row in its own transaction (`retained.go:313-330`).
- `ImportRetained` re-holds through `restoreDiskQuota` (`retained.go:527`).

If any other caller releases a deleted workspace's reservation without `holdRetainedDisk` in the same transaction, **stop**. Keep `repairRetainedDiskHolds` and report the caller in the commit body.

- [ ] **Step 2: Write the v1-path test first (it must pass before and after the removal)**

Add to `internal/provisioning/recovery_test.go`:

```go
// TestSettleQuota_DeletedWorkspaceKeepsRetainedDiskHeld: settling a deleted
// workspace whose home was retained frees its compute and, in the SAME
// transaction, re-holds the retained disk — no later repair pass needed.
func TestSettleQuota_DeletedWorkspaceKeepsRetainedDiskHeld(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	const tenant = "tenant-rh"
	seedHeldWorkspace(t, db, tenant, "ws_rhsrc", quotaVec)
	if _, err := db.Pool().Exec(ctx, `UPDATE workspaces SET state = 'deleted' WHERE id = 'ws_rhsrc'`); err != nil {
		t.Fatal(err)
	}
	if _, err := provisioning.NewRetainedStore(db).ImportRetained(ctx, provisioning.RetainedDiskInfo{
		TenantID: tenant, Owner: "iss|sub-1", SourceWorkspaceID: "ws_rhsrc",
		Runtime: "LinuxContainer", SizeBytes: 3 << 30,
		PVCNamespace: "ns-it", PVCName: "pvc-rh", PVCUID: "uid-pvc-rh",
	}); err != nil {
		t.Fatalf("import: %v", err)
	}
	rec := provisioning.NewRecovery(db, &fakeObserver{gone: true})
	if err := rec.SettleQuota(ctx, tenant, "ws_rhsrc"); err != nil {
		t.Fatalf("settle: %v", err)
	}
	state, v, _, _ := resState(t, db, "ws_rhsrc")
	if state != "held" || v != (provisioning.ResourceVector{DiskBytes: 3 << 30}) {
		t.Fatalf("after settle: %s %+v, want held disk-only 3 GiB", state, v)
	}
	actions, err := rec.Recover(ctx, &recordingApplier{})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("recovery found work after an in-transaction settle: %+v", actions)
	}
}
```

Run: `go test ./internal/provisioning/ -run 'TestSettleQuota_DeletedWorkspaceKeepsRetainedDiskHeld|TestPurge_SettlesEmptyReservation' -count=1 -v`
Expected: PASS for both. Docker is required. These two tests are the proof that the v1 paths (the settle keeps the disk, the purge settles the empty row) need no recovery pass. If the first one reports a non-empty `actions`, read them before going on, because a pre-existing seed (an undispatched intent from `seedHeldWorkspace`) may legitimately produce one. If so, assert only that no action has kind `hold_retained_disk` or `settle_reservation`.

- [ ] **Step 3: Remove the repairs and the dead method**

In `internal/provisioning/recovery.go`:
- Delete `ActionHoldRetainedDisk` and `ActionSettleReservation` (lines 61-66).
- Delete `repairRetainedDiskHolds` and `settleEmptyReservations`.
- Delete the two blocks in `Recover` that call them (`--- keep retained disks counted ---` and `--- settle reservations left holding nothing ---`).
- Keep `holdRetainedDisk` and its doc comment unchanged, because `SettleQuota` uses it. (The "stranded by releases that predate" wording is in the deleted functions' docs and in the `ActionSettleReservation` comment, which go with them.)
- Line 60 (`ActionQuarantineIntent = "quarantine_intent"`) stays; `ActionHoldRetainedDisk` (61) has no doc comment of its own. `ProofQuotaSettled` (`quota.go:149-153`) stays, because `releaseDiskQuota` (`retained.go:330`) still uses it.

In `internal/provisioning/service.go`, delete `(*Service).ReleaseQuota` with its two-line doc comment (lines 805-811). In `internal/provisioning/loop_hardening_test.go`, delete `TestRecovery_SettlesStrandedEmptyRows` (lines 171-222, doc comment included), and in the file's header comment change line 10 `(releaseDiskQuota in-tx, plus the recovery sweep for stranded rows);` to `(releaseDiskQuota in-tx);`. `seedWorkspaceRow` stays: other tests use it. In `tests/integration/delete_finalization_test.go`, delete `TestDeleteFinalization_StuckRowSelfHeals` (lines 294-328, doc comment included). It hand-seeds a `released` reservation next to a live retained dataset and expects a recovery pass to re-hold it, which is exactly the removed repair. The v1 path it guarded is pinned by Step 2's test and by `TestDeleteFinalization_RetainKeepsDataRecord` (line 235), which deletes a Retain workspace end to end and asserts that only the retained disk stays held.

Run:

```bash
go build ./... && go vet ./internal/provisioning/ && go vet -tags=integration ./tests/integration/ && echo vet-ok
go test ./internal/provisioning/... -count=1 > /tmp/w2-t3-prov.log 2>&1; echo rc=$?; grep -nE '^(--- FAIL|FAIL|panic)' /tmp/w2-t3-prov.log | head
rg -n 'repairRetainedDiskHolds|settleEmptyReservations|ActionHoldRetainedDisk|ActionSettleReservation|ReleaseQuota\(|StuckRowSelfHeals' -g '*.go' . ; echo "leftovers rc=$?"
```

Expected: `vet-ok`, `rc=0`, and `leftovers rc=1` (no match).

- [ ] **Step 4: Template revision — failing test, then fix**

Create `internal/api/templaterevision_test.go`:

```go
package api

import (
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// The revision a workspace view reports is the template's spec.revision
// label verbatim. spec.revision is required (MinLength 1), so every v1 row
// carries it; the numeric family counter is never shown in its place.
func TestTemplateRevisionOf_LabelOnly(t *testing.T) {
	if got := templateRevisionOf(&provisioning.TemplateInfo{Revision: 3, RevisionLabel: "2026-10-a"}); got != "2026-10-a" {
		t.Fatalf("label row = %q, want 2026-10-a", got)
	}
	if got := templateRevisionOf(&provisioning.TemplateInfo{Revision: 3}); got != "" {
		t.Fatalf("label-less row = %q, want \"\" (no numeric stand-in)", got)
	}
}
```

Run: `go test ./internal/api/ -run TestTemplateRevisionOf_LabelOnly -count=1` — expected: FAIL, `label-less row = "3"`.

Change `templateRevisionOf` to `return t.RevisionLabel` and remove the `"strconv"` import from `internal/api/workspaces.go` (the deleted `strconv.FormatInt` was its only use; `go build` fails otherwise). Use the doc comment "templateRevisionOf reports the workspace's template revision: the verbatim spec.revision label." In `internal/provisioning/service.go:159-161`, change the `RevisionLabel` comment to `// RevisionLabel is the raw spec.revision identifier ("2026-10-b").`

Run: `go test ./internal/api/... -count=1 > /tmp/w2-t3-api.log 2>&1; echo rc=$?; grep -nE '^(--- FAIL|FAIL)' /tmp/w2-t3-api.log | head`. Any test that expected a numeric revision from a label-less fixture gets a `RevisionLabel` added to its fixture.

- [ ] **Step 5: Chart — failing tests, then remove the guard**

In `deploy/helm/chart_test.go`, replace `TestRemovedValuesFail` (lines 3083-3103) with:

```go
// TestUnknownComponentKeysRejected: the chart's components are backend,
// frontend and operator; any other top-level component key — and any
// images.* entry outside the release set — is a schema violation.
func TestUnknownComponentKeysRejected(t *testing.T) {
	for _, set := range []string{
		"api.replicas=1", "gateway.replicas=1", "portal.replicas=1",
		"images.api.repository=x/y", "images.gateway.repository=x/y", "images.portal.repository=x/y",
		"sessionHost=session.lab.example.net",
	} {
		key := strings.SplitN(strings.TrimPrefix(set, "images."), ".", 2)[0]
		key = strings.SplitN(key, "=", 2)[0]
		out := renderErrArgs(t, "-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"), "--set", set)
		if !strings.Contains(out, key) {
			t.Errorf("--set %s must fail naming %q, got: %s", set, key, out)
		}
		if strings.Contains(out, "migrate these values") {
			t.Errorf("--set %s still hits the removed migration hint: %s", set, out)
		}
	}
}
```

Delete `TestSessionHostValueFails` from `chart_sessiondomain_test.go`; the table above covers it. In `deploy/helm/tinycdi/testdata/schema-violation-values.yaml`, change `images.api` to `images.backend` and `repository: legacy/api` to `repository: example/backend`, keeping the bad digest. Update its header comment to "backend digest is not a sha256:<64hex> reference".

Run: `go test ./deploy/helm/ -run 'TestUnknownComponentKeysRejected|TestSchemaRejectsBadValues' -count=1` — expected: FAIL on the "migration hint" assertion.

Then remove the guard:
- `_helpers.tpl`: delete the `tinycdi.legacyValues` block (lines 485-512: the `0.2.0 migration guard` comment at 485-491 and the define at 492-512) with the blank line after it, the `{{- include "tinycdi.legacyValues" . -}}` line (231), and the `chart 0.2.0 replaced the api/gateway/portal components…` bullet with its `sessionHost (0.1.x) moved to sessionDomain` continuation (lines 171-174). The `Install-time invariants. Rendering FAILS when violated:` header then leads straight into the `portalHost must differ…` bullet.
- `values.schema.json`: delete `"api"`, `"gateway"` and `"portal"` under `images.properties` (lines 26-28) and drop the trailing comma from the `"browser"` line (25) that now ends the object; delete `"sessionHost"` (line 102) and the top-level `"api"` (273), `"gateway"` (328) and `"portal"` (358) property objects, each from its `"<key>": {` line through its matching 4-space-indented `},`. Keep `"additionalProperties": false` at the top level and under `images`. Helm then reports, for example, `- at '/images': additional properties 'api' not allowed` and `- at '': additional properties 'sessionHost' not allowed` (checked with helm v4.1.3).

Run:

```bash
go test -count=1 ./deploy/helm/ > /tmp/w2-t3-helm.log 2>&1; echo rc=$?; grep -nE '^(--- FAIL|FAIL)' /tmp/w2-t3-helm.log | head
python3 -c 'import json,sys; json.load(open("deploy/helm/tinycdi/values.schema.json"))' && echo schema-json-ok
```

Expected: `rc=0` and `schema-json-ok`. The golden is unchanged: `git diff --quiet deploy/helm/testdata/ && echo golden-unchanged`.

- [ ] **Step 6: Start resolves its session policy from the recorded snapshot only**

`(*Service).startSessionPolicy` (`internal/provisioning/service.go:742-770`) treats `rec.Template.ClipboardPolicy == ""` as "a workspace row predating the snapshot field": it reads the live catalog (`s.templates.Get`) and, if that is empty too, returns `nil` ("the pre-field behaviour"). Every v1 row records the policy: create and attach stamp `req.Template.ClipboardPolicy` (`service.go:310`, `retained.go:637`), which the API fills from the catalog entry (`internal/api/workspaces.go:541`, `internal/api/data.go:398`), which copies `spec.clipboardPolicy` (`internal/provisioning/k8sapplier.go:632`). That field is `+required` with an enum that excludes `""` (`api/v1/workspacetemplate_types.go:224-229`, `api/v1/validation.go:103-105`), and a family re-point copies the newest revision's policy (`template_family.go:151`). So the fallback serves only rows from earlier builds.

Decision: the start resolves exactly as create does (`service.go:309-312`): `resolveSessionPolicyTx` over the recorded `ClipboardPolicy` and `FileTransferPolicy`, always returning a policy. No error for `""`: create already passes the recorded value through unchecked, and an error here would add a refusal path that no v1 row reaches. v1 behaviour is unchanged, because every v1 row's policy is non-empty and the catalog was never consulted for it. No schema change: the policy lives inside the `workspaces.template` jsonb document (`internal/store/migrations/0001_baseline.sql:25`), not in a column, so `NOT NULL` cannot express it. (`launch_ticket.clipboard_policy`, line 99, is a different value: NULL there is the v1 state of a ticket whose best-effort policy resolution failed, `internal/api/connections.go:175-183`.)

1. **Failing test first.** In `internal/provisioning/template_family_test.go`, insert before the `// TestStart_ConcurrentIdempotent:` comment (line 257). `familyWorkspace` (line 91) seeds a row whose `TemplateInfo` has no `ClipboardPolicy`, so it stands in for the removed case:

```go
// TestStart_PolicyFromRecordedSnapshotOnly: a start resolves its session
// policy from the policies recorded on the workspace row, never from the
// live catalog, and every start intent carries a policy. The seed row
// records no clipboard policy while the catalog revision says
// Bidirectional; the start must not pick the catalog value up.
func TestStart_PolicyFromRecordedSnapshotOnly(t *testing.T) {
	cat := newFakeTemplateLookup()
	e := familyEntry("linuxdesk", "aaaa1111", "2026-10-a", "OnStart")
	e.ClipboardPolicy = provisioning.PolicyBidirectional
	cat.put(e)
	db, svc, ws := familyWorkspace(t, cat)

	if _, err := svc.SignalWorkspace(context.Background(), familyTenant, "iss|sub", "", ws, "fam-start-pol", provisioning.IntentStart, []byte("{}")); err != nil {
		t.Fatalf("start: %v", err)
	}
	intents := pendingIntents(t, db, ws)
	sp := intents[len(intents)-1].Spec.SessionPolicy
	if sp == nil {
		t.Fatal("start intent carries no session policy")
	}
	if sp.Clipboard == provisioning.PolicyBidirectional {
		t.Fatalf("start policy %+v came from the live catalog, want the recorded snapshot", sp)
	}
}
```

   Run: `go test ./internal/provisioning/ -run TestStart_PolicyFromRecordedSnapshotOnly -count=1 -v` — expected: FAIL, `start policy &{Clipboard:Bidirectional FileTransfer:Disabled MaxFileBytes:0} came from the live catalog`. Docker is required; a skip is not a pass.

2. **Remove the fallback.** Replace `startSessionPolicy` with its doc comment (lines 742-770) with:

```go
// startSessionPolicy resolves the session data-transfer policy a start
// intent carries: the meet of the tenant ceiling and the policies recorded
// on rec.Template — already the post-re-point snapshot when the family
// moved. It resolves exactly as create does, so every start intent carries
// a policy.
func (s *Service) startSessionPolicy(ctx context.Context, tx store.Tx, tenantID string, rec *WorkspaceRecord) (*SessionPolicy, error) {
	sp, err := resolveSessionPolicyTx(ctx, s.db, tx, tenantID, TemplateCatalogEntry{
		ClipboardPolicy:    rec.Template.ClipboardPolicy,
		FileTransferPolicy: rec.Template.FileTransferPolicy,
	}, s.fileBound)
	if err != nil {
		return nil, fmt.Errorf("start: resolve session policy: %w", err)
	}
	return &sp, nil
}
```

   In the start branch of `SignalWorkspace` (lines 567-576), the result is never nil now, so replace the `if sp != nil { … }` block with:

```go
				if sigSpec == nil {
					sigSpec = &IntentSpec{}
				}
				sigSpec.SessionPolicy = sp
```

   The restart caller (line 682) passes `sp` straight into the intent and needs no edit. `s.templates` stays: `startTemplateTarget` (line 531) still uses it. Leave the applier's `in.Spec.SessionPolicy != nil` checks (`k8sapplier.go:281,371`) as they are: stop intents carry no policy.

   Run: `go vet ./internal/provisioning/ && go test ./internal/provisioning/ -run 'TestStart_|SessionPolicy' -count=1` — expected: `ok`. No existing test asserts the catalog fill or a nil start policy (checked on a scratch clone of `b41be7d`: the whole `./internal/provisioning/...` package passes with this change).

- [ ] **Step 7: Retained attach refuses a CR without the claim reference instead of repairing it**

`(*RetainedApplier).applyAttach` (`internal/provisioning/retained_applier.go:134-159`) patches the claim annotations onto the consuming CR when they are missing: "a CR left behind by a delivery that predates single-write creation". In v1 the CR is created carrying them in one write: `Apply` sets `in.CRAnnotations = claim.annotations()` before the inner create (`retained_applier.go:60`, `k8sapplier.go:207-209`), and the backend always wraps the K8s applier in the retained one (`internal/backend/wire.go:222`). A replay finds the CR already stamped, so the patch is a no-op on every v1 path: this is an upgrade repair, not replay convergence. Removing it without a check would retarget the volume to a CR that does not name the claim (the operator would then build a new empty home), so the branch becomes a fail-closed check.

1. **Failing test first.** In `internal/provisioning/retained_applier_test.go`, add `"strings"` to the standard-library imports (after `"context"`, line 11) and append:

```go
// TestRetainedApplierRefusesUnstampedCR: the consuming CR is created
// carrying the claim reference, so a CR that exists for the same request
// without it was not created by this claim. The attach fails before the
// volume is retargeted and the record stays Attaching; the CR is not
// patched.
func TestRetainedApplierRefusesUnstampedCR(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	rstore := provisioning.NewRetainedStore(db)
	svc := provisioning.NewService(db)
	const tenant = "tenant-unst"
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.SetQuota(ctx, tx, tenant, provisioning.ResourceVector{
			RunningSlots: 10, CPUMillis: 1 << 20, MemoryBytes: 1 << 40, DiskBytes: 1 << 40})
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	seedWorkspaceRow(t, db, "ws_unstsrc01", tenant, "iss|sub-1", "src-unst")
	rec, err := rstore.ImportRetained(ctx, provisioning.RetainedDiskInfo{
		TenantID: tenant, Owner: "iss|sub-1", SourceWorkspaceID: "ws_unstsrc01",
		Runtime: "LinuxContainer", SizeBytes: 2 << 30,
		PVCNamespace: "ns-it", PVCName: "pvc-unst", PVCUID: "uid-pvc-unst",
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	ws, err := svc.AttachRetained(ctx, tenant, "iss|sub-1", "iss|sub-1", rec.ID, "k-unst",
		provisioning.AttachRequest{
			Name:     "consumer-unst",
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
	create := pending[0]

	pvc := retainedPVC("ns-it", "pvc-unst", "cruid-src", "ws-unstsrc01", nil)
	kc := fake.NewClientBuilder().WithScheme(retainedApplyScheme(t)).WithObjects(pvc).Build()
	tmap := provisioning.TenantNamespaces{tenant: "ns-it"}
	// The same request's CR, created without the claim reference.
	inner := provisioning.NewK8sApplier(kc, tmap)
	if err := inner.Apply(ctx, create); err != nil {
		t.Fatalf("seed unstamped CR: %v", err)
	}

	applier := provisioning.NewRetainedApplier(inner, kc, tmap, rstore)
	err = applier.Apply(ctx, create)
	if err == nil || !strings.Contains(err.Error(), "lacks claim annotation") {
		t.Fatalf("apply onto unstamped CR: err = %v, want a missing-claim refusal", err)
	}
	cr := &cdiv1.Workspace{}
	if err := kc.Get(ctx, client.ObjectKey{Namespace: "ns-it", Name: provisioning.WorkspaceCRName(create.WorkspaceUID)}, cr); err != nil {
		t.Fatalf("consuming CR: %v", err)
	}
	if v, ok := cr.Annotations[provisioning.AnnotationRetainedDataRef]; ok {
		t.Fatalf("unstamped CR was patched with the claim reference %q", v)
	}
	gotPVC := &corev1.PersistentVolumeClaim{}
	if err := kc.Get(ctx, client.ObjectKey{Namespace: "ns-it", Name: "pvc-unst"}, gotPVC); err != nil {
		t.Fatalf("pvc: %v", err)
	}
	if gotPVC.Labels["workspaces.cdi.tinyorbit.vn/workspace-uid"] != "cruid-src" {
		t.Fatalf("pvc retargeted despite the refusal: %v", gotPVC.Labels)
	}
	got, err := rstore.GetRetained(ctx, tenant, rec.ID)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if got.State != provisioning.RetainedStateAttaching {
		t.Fatalf("record state = %s, want Attaching", got.State)
	}
}
```

   Run: `go test ./internal/provisioning/ -run TestRetainedApplier -count=1 -v` — expected: `TestRetainedApplierSameRevisionTwice` passes and `TestRetainedApplierRefusesUnstampedCR` FAILs with `apply onto unstamped CR: err = <nil>`.

2. **Replace the repair with the check.** In `applyAttach`, replace lines 134-159 (from `// The CR normally already carries the claim reference from its create.` through the closing `}` of `if changed { … }`) with:

```go
	// The CR is created carrying the claim reference (Apply sets it on the
	// create), so a replay finds it already stamped. A CR without it was
	// not created by this claim: refuse before the volume is retargeted.
	ws := &cdiv1.Workspace{}
	if err := a.Client.Get(ctx, client.ObjectKey{
		Namespace: claim.ns, Name: WorkspaceCRName(in.WorkspaceUID),
	}, ws); err != nil {
		return fmt.Errorf("attach retained: get workspace CR: %w", err)
	}
	for k, v := range claim.annotations() {
		if ws.Annotations[k] != v {
			return fmt.Errorf("attach retained: workspace CR %s lacks claim annotation %s=%q", ws.Name, k, v)
		}
	}
```

   `ws.UID` is still used for the PVC label below, and `client.MergeFrom` is still used by the PVC patch, so no import changes.

   Run:

```bash
go vet ./internal/provisioning/ && go vet -tags=integration ./tests/integration/ && echo vet-ok
go test ./internal/provisioning/... -count=1 > /tmp/w2-t3-prov2.log 2>&1; echo rc=$?; grep -nE '^(--- FAIL|FAIL|panic)' /tmp/w2-t3-prov2.log | head
KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path --bin-dir "$PWD/bin")" go test -tags=integration -count=1 -v -run '^(TestSEC01_|TestFXR20_AttachMountsRetainedVolume)' ./tests/integration/... > /tmp/w2-t3-it.log 2>&1; echo rc=$?; grep -nE '^(--- FAIL|--- SKIP|FAIL)' /tmp/w2-t3-it.log | head
grep -n 'predates single-write\|row predating\|pre-field behaviour\|s.templates.Get' internal/provisioning/retained_applier.go internal/provisioning/service.go; echo "leftovers rc=$?"
```

   Expected: `vet-ok`, `rc=0` twice with no `--- SKIP`, and `leftovers rc=1`. The integration tests cover the real-API attach path (`TestSEC01_ClaimedRecordApplies` replays an attach; `TestSEC01_UnclaimedRecordNeverMutated` and `TestSEC01_CreateRejectsBareRef` fail in `verifyClaim`, before this check).

- [ ] **Step 8: Commit**

```bash
git add -A internal/provisioning internal/api deploy/helm tests/integration
git commit -m "refactor: drop stranded-row quota repairs, numeric revision fallback, chart values guard

Every v1 release holds the retained disk and settles empty rows in the
same transaction; spec.revision is required; no public install carries
the pre-1.0 component keys, so the schema rejects them directly. A start
resolves its session policy from the recorded snapshot only, as create
does, and a retained attach refuses a consuming CR that lacks the claim
reference instead of patching it.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Internal-version wording out of code, screenshot Secret fixed, and the C1 grep gate (C1) — tier: integrate

**Files:**
- Create: `.github/scripts/allowlist-grep.sh`, `.github/tests/legacy-shims.test.sh`, `.github/tests/legacy-shims.allow`
- Modify: `web/scripts/screenshots.mjs:214-225` (`stageSecret`)
- Modify: every file the two gate patterns hit at this point. The list is about 70 files (73 at `b41be7d` with Task 3 applied and Tasks 1–2 not yet applied, 174 hit lines); regenerate it with Step 2's run. The non-test hits, with their target wording, are in Step 3's table. Test files get the same treatment.
- Modify: `internal/store/migrate.go:91-92`, `internal/store/baseline_test.go:56-76` and the header of `internal/store/migrations/0001_baseline.sql:3-9` (outside the gate's scope, reworded for consistency)
- Modify: `api/v1/workspacetemplate_types.go:70-74,94-97` (wording); then run `make manifests generate sync-chart-crds` and regenerate the golden. Both CRDs change: `workspaces.cdi.tinyorbit.vn_workspacetemplates.yaml` and `workspaces.cdi.tinyorbit.vn_workspaces.yaml`, because the Workspace status embeds the template spec. `api/v1/workspace_types.go` has no hit and is not edited.

**Interfaces:**
- Consumes: nothing from earlier tasks except that Tasks 1–3 have removed their branches. This task's gate must pass on the result.
- Produces:
  - `.github/scripts/allowlist-grep.sh -p <ERE> -a <allowlist> [-i] [-s] [-r <root>] -- <pathspec>...`. Exit 0 when clean, 1 with `FAIL:` lines, 2 on usage error. `-s` skips the stale-entry check (partial scope).
  - allowlist entry format `<glob> :: <ERE> :: <reason>`, with the reason at least 10 characters
  - `.github/tests/legacy-shims.allow`, the reviewed C1 allowlist. Task 5's docs gate reuses the engine with its own allowlist.

- [ ] **Step 1: Write the engine's fixture test first**

Create `.github/tests/legacy-shims.test.sh`:

```bash
#!/usr/bin/env bash
# legacy-shims.test.sh — C1 gate: no compatibility shim, "legacy"/
# "deprecated" handling, v1alpha1 reference or internal-version comment in
# the Go, TS and chart sources, except the reviewed allowlist entries in
# .github/tests/legacy-shims.allow (each with a reason). Also self-tests the
# shared allowlist engine (.github/scripts/allowlist-grep.sh).
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ENGINE="$ROOT/.github/scripts/allowlist-grep.sh"
fails=0
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# ---- engine fixtures ---------------------------------------------------
FX="$TMP/fx"; mkdir -p "$FX/sub"
git -C "$FX" init -q
printf 'clean\na legacy branch\n' > "$FX/a.go"
printf 'upstream legacy quirk\n' > "$FX/sub/b.go"
printf 'legacy in docs\n' > "$FX/c.md"
git -C "$FX" add -A

expect() { # expect <name> <want-rc> <want-grep-or-empty> <allowlist-body> [-s]
  local name="$1" want="$2" grepfor="$3" body="$4" extra="${5:-}"
  printf '%s\n' "$body" > "$TMP/allow"
  out="$(bash "$ENGINE" -i $extra -p 'legacy' -a "$TMP/allow" -r "$FX" -- '*.go' 2>&1)"; rc=$?
  if [ "$rc" -ne "$want" ]; then echo "FAIL: engine/$name: rc=$rc want $want: $out"; fails=1; return; fi
  if [ -n "$grepfor" ] && ! grep -qF -- "$grepfor" <<< "$out"; then
    echo "FAIL: engine/$name: output lacks '$grepfor': $out"; fails=1
  fi
}
expect "unallowed hit fails" 1 "a.go:2" \
  'sub/*.go :: upstream legacy quirk :: upstream quirk name kept verbatim'
expect "all allowed passes" 0 "" \
  'sub/*.go :: upstream legacy quirk :: upstream quirk name kept verbatim
a.go :: legacy branch :: fixture entry with a long enough reason'
expect "stale entry fails" 1 "stale allowlist entry" \
  'sub/*.go :: upstream legacy quirk :: upstream quirk name kept verbatim
a.go :: legacy branch :: fixture entry with a long enough reason
*.go :: never matches :: fixture entry that covers nothing'
expect "stale ignored with -s" 0 "" \
  'sub/*.go :: upstream legacy quirk :: upstream quirk name kept verbatim
a.go :: legacy branch :: fixture entry with a long enough reason
*.go :: never matches :: fixture entry that covers nothing' -s
expect "short reason fails" 1 "malformed allowlist entry" \
  'sub/*.go :: upstream legacy quirk :: too short
a.go :: legacy branch :: fixture entry with a long enough reason'
expect "glob star crosses slash" 0 "" \
  '*.go :: legacy :: one entry for every Go file in the fixture'
printf 'a legacy line in a new file\n' > "$FX/new.go"   # untracked: never git-added
expect "untracked file is checked" 1 "new.go:1" \
  'sub/*.go :: upstream legacy quirk :: upstream quirk name kept verbatim
a.go :: legacy branch :: fixture entry with a long enough reason'

# ---- live gate ---------------------------------------------------------
# One allowlist serves both patterns, so the two pattern runs skip the
# stale check (-s) and a combined third run enforces it.
SCOPE=('*.go' '*.ts' '*.tsx' 'deploy/helm/**' 'internal/api/openapi.yaml' ':(exclude)*.md')
ALLOW="$ROOT/.github/tests/legacy-shims.allow"
SHIMS='legacy|deprecated|v1alpha1'
# VERS: v0.x not glued to a module path/tag (so controller-runtime@v0.25.0
# and image tags are not hits); v1.1+; pre-vN / pre-1.0; "until|since|before
# vN"; rc.N; and test names carrying a version (TestX_V040Budgets).
VERS='(^|[^0-9A-Za-z.@/-])v0\.[0-9]|\bv1\.[1-9]([^0-9]|$)|\bpre-(v[0-9]|1\.0)|\b(until|since|before) v[0-9]|\brc\.[0-9]|\bTest[A-Za-z0-9_]*V0[0-9]'
out="$(bash "$ENGINE" -i -s -p "$SHIMS" -a "$ALLOW" -r "$ROOT" -- "${SCOPE[@]}" 2>&1)" \
  || { echo "$out"; echo "FAIL: legacy/deprecated/v1alpha1 references outside the allowlist"; fails=1; }
out="$(bash "$ENGINE" -s -p "$VERS" -a "$ALLOW" -r "$ROOT" -- "${SCOPE[@]}" 2>&1)" \
  || { echo "$out"; echo "FAIL: internal version references in code comments"; fails=1; }
out="$(bash "$ENGINE" -i -p "$SHIMS|$VERS" -a "$ALLOW" -r "$ROOT" -- "${SCOPE[@]}" 2>&1)"
if grep -qE 'stale allowlist entry|malformed allowlist entry' <<< "$out"; then
  grep -E 'stale allowlist entry|malformed allowlist entry' <<< "$out"
  echo "FAIL: every allowlist entry must be well formed and cover a live hit"; fails=1
fi

# (Step 4 appends the screenshot Secret check here.)

[ "$fails" -eq 0 ] && echo "legacy-shims: engine fixtures and live gate pass"
exit "$fails"
```

Run: `bash .github/tests/legacy-shims.test.sh` — expected: fails, because `allowlist-grep.sh` does not exist.

- [ ] **Step 2: Write the engine**

Create `.github/scripts/allowlist-grep.sh`:

```bash
#!/usr/bin/env bash
# allowlist-grep.sh — fail on every match of an ERE in the files a git
# pathspec selects (tracked, plus untracked files not ignored, so a new file
# is checked before `git add`), unless a reviewed allowlist entry covers it.
#
#   allowlist-grep.sh -p '<ERE>' -a <allowlist> [-i] [-s] [-r <repo root>] -- <pathspec>...
#
#   -i  case-insensitive match of <ERE>
#   -s  skip the stale-entry check (use when checking part of the scope)
#
# Allowlist: one entry per line, "<glob> :: <ERE> :: <reason>"; blank lines
# and lines starting with # are ignored. A hit is allowed when its path
# matches <glob> (bash [[ == ]] semantics: * also matches /) and the hit's
# line matches <ERE> (case-sensitive). The reason must be at least 10
# characters. Unless -s is given, an entry that covers no hit is stale and
# fails the run. Exit: 0 clean, 1 findings, 2 usage.
set -uo pipefail

usage() { echo "usage: $0 -p ERE -a allowlist [-i] [-s] [-r root] -- pathspec..." >&2; exit 2; }
pat="" allow="" icase="" skipstale=0 root="."
while getopts "p:a:isr:" o; do
  case "$o" in
    p) pat="$OPTARG" ;;
    a) allow="$OPTARG" ;;
    i) icase="-i" ;;
    s) skipstale=1 ;;
    r) root="$OPTARG" ;;
    *) usage ;;
  esac
done
shift $((OPTIND - 1))
[ "${1:-}" = "--" ] && shift
{ [ -n "$pat" ] && [ -n "$allow" ] && [ "$#" -gt 0 ]; } || usage
[ -f "$allow" ] || { echo "FAIL: allowlist $allow not found"; exit 1; }

globs=() eres=() used=() fails=0
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in ''|'#'*) continue ;; esac
  g="${line%% :: *}"; rest="${line#* :: }"
  e="${rest%% :: *}"; why="${rest#* :: }"
  if [ "$g" = "$line" ] || [ "$e" = "$rest" ] || [ "${#why}" -lt 10 ]; then
    echo "FAIL: malformed allowlist entry (want '<glob> :: <ERE> :: <reason of 10+ chars>'): $line"
    fails=1; continue
  fi
  globs+=("$g"); eres+=("$e"); used+=(0)
done < "$allow"

while IFS= read -r -d '' f; do
  while IFS= read -r hit; do
    n="${hit%%:*}"; text="${hit#*:}"; ok=0
    for i in "${!globs[@]}"; do
      # shellcheck disable=SC2053  # the glob is intentionally unquoted
      if [[ "$f" == ${globs[$i]} ]] && grep -qE -- "${eres[$i]}" <<< "$text"; then
        used[$i]=1; ok=1
      fi
    done
    [ "$ok" -eq 1 ] || { echo "FAIL: $f:$n: ${text:0:160}"; fails=1; }
  done < <(grep -nIE $icase -- "$pat" "$root/$f" 2>/dev/null)
done < <(git -C "$root" ls-files -z --cached --others --exclude-standard -- "$@")

if [ "$skipstale" -eq 0 ]; then
  for i in "${!globs[@]}"; do
    [ "${used[$i]}" -eq 1 ] || { echo "FAIL: stale allowlist entry (covers no hit): ${globs[$i]} :: ${eres[$i]}"; fails=1; }
  done
fi
exit "$fails"
```

Create an empty `.github/tests/legacy-shims.allow` that holds only this header:

```text
# legacy-shims.allow — reviewed exceptions to the C1 gate
# (.github/tests/legacy-shims.test.sh). One per line:
#   <glob> :: <ERE matched against the line> :: <reason, 10+ chars>
# Only upstream names (Kubernetes API fields, KasmVNC quirks, generator
# and module version stamps) belong here — never a TinyCDI history note.
```

Run: `bash .github/tests/legacy-shims.test.sh > /tmp/w2-t4-gate.log 2>&1; echo rc=$?; grep -c '^FAIL: [^e]' /tmp/w2-t4-gate.log; grep -n 'engine/' /tmp/w2-t4-gate.log`
Expected: `rc=1`, no `engine/` lines (the fixtures pass), and a list of live hits. That list is Step 3's work list. List the files with `grep -E '^FAIL: [^ ]+:[0-9]+:' /tmp/w2-t4-gate.log | sed -E 's/^FAIL: ([^:]+):.*/\1/' | sort -u`. The following hits belong to Tasks 1 and 2 and must already be gone; if one is still listed, the owning task missed it, so fix it as that task specifies, not with new wording: `internal/broker/sessions.go` ("legacy claim"), `internal/gateway/launch.go` ("NULL (legacy) claim"), `internal/api/openapi.yaml` and `web/src/api/generated/schema.d.ts` ("no id (legacy client)"), `web/src/session/SessionPage.tsx` ("a legacy claim stored NULL"), `web/tests/unit/session/SessionPage.test.tsx` ("a legacy claim", "the legacy fallback"), `web/tests/mock-api/session.ts`, `internal/broker/connection_state_test.go`, `internal/gateway/owner_tab_test.go` (Task 1); `internal/operator/workspace_controller.go` ("Pre-stamp pod (v0.5 builds)"), `internal/runtime/linux/backend.go:1496` (`a.DeprecatedServiceAccount`, inside the deleted `podSpecBuiltEqual`), and the pre-stamp cases in `internal/operator/snapshot_adopt_test.go` (Task 2).

- [ ] **Step 3: Sweep the live hits**

Rules for each hit:
1. **A comment or test name that narrates history** ("v0.1 behaviour", "until v0.3", "pre-1.0 posture", "rc.2-era", "legacy claim", "Fails on v0.5.0"): rewrite it to state the current behavior and why, or delete the clause. Rename test functions that carry a version or "Legacy", for example:
   - `TestLogin_NoLegacyCookieDeletion` becomes `TestLogin_OnlyHostPrefixedCookies`;
   - `TestPGSessionStore_IDTokenLegacyNull` becomes `TestPGSessionStore_IDTokenAbsentIsNull`;
   - `TestRateLimitDefaults_V040Budgets` (`internal/backend/wire_test.go:142`) becomes `TestRateLimitDefaults_Budgets`;
   - `TestRehydrate_NilDirectoryKeepsV01Behaviour` (`internal/gateway/rehydrate_test.go:364`) becomes `TestRehydrate_NilDirectorySingleProcess`;
   - `TestBaseline_MigrateRefusesPre1Database` becomes `TestBaseline_MigrateRefusesUnknownMigration` (see the `migrate.go` row below).

   `docs/security/threat-model.md:187,602` cite `TestRateLimitDefaults_V040Budgets`. Do not edit docs here: Task 13 rebuilds that file and must use the new name. No other doc outside `docs/superpowers/` cites a renamed test.

   Keep each test's assertions; only names and comments change.
2. **A negative guard test** (it asserts that a retired name is never emitted, such as the `tcdi_csrf`/`tcdi_session_origin` cookies in `internal/api/me_test.go:186-200` and `auth_test.go:277-281`, or the node-selector annotation in `internal/runtime/linux/backend_test.go:264-292` and `deploy/helm/chart_test.go:449-452`): keep the assertion, and call the names "retired cookie names" or "retired annotation" (identifier `retiredAnnotation`, comment "not part of the v1 template API"). Do not cite a version.
3. **An upstream name**: add an allowlist entry with the reason. Append exactly these entries below the header (a gate run against `b41be7d` plus Task 3 showed each one covering live hits, and nothing else upstream was hit):

```text
internal/gateway/proxy.go :: legacy Sec-WebSocket-Origin :: KasmVNC upstream requires the Sec-WebSocket-Origin header by that name
deploy/helm/*.yaml :: controller-gen\.kubebuilder\.io/version: v0\.[0-9]+ :: controller-gen version stamp written by the generator
```

   - The `proxy.go` entry covers `internal/gateway/proxy.go:976` ("KasmVNC requires the legacy Sec-WebSocket-Origin").
   - No entry for `DeprecatedServiceAccount`: Task 2 deletes both test lines (`snapshot_adopt_test.go:160`, `resnapshot_test.go:322`) and `podSpecBuiltEqual` (`internal/runtime/linux/backend.go:1496`), so `grep -rn DeprecatedServiceAccount internal/` prints nothing. If it prints a line, Task 2 is incomplete: delete the line, do not allowlist it.
   - The `controller-gen` entry covers the `controller-gen.kubebuilder.io/version: v0.22.0` stamp in both chart CRDs (line 6) and in `deploy/helm/testdata/default-render.golden.yaml` (lines 8 and 742). The glob's `*` crosses `/`.
   - Do not add an entry for `cmd/operator/main.go`'s `controller-runtime@v0.25.0` URLs (`:501,513`). They are not hits, because the version pattern skips a `v0.` glued to `@`, so such an entry would be stale and fail the gate.

Then rewrite the non-test hits below. Line numbers are from `b41be7d`; re-locate them by text.

| File:line | Today | Rewrite to |
|---|---|---|
| `internal/api/statusview.go:429-430` | "(every v0.1 workspace at upgrade time) falls back to its template's age" | "A workspace whose template carried no image age when it was created falls back to its template's current age" (the branch stays; it serves templates that gain the annotation later) |
| `internal/api/workspaces.go:351` | handled in Task 3 | — |
| `internal/provisioning/service.go:169-176` (`:161` is reworded in Task 3) | "Empty on rows written before the field existed" (twice) | Drop both clauses. `ClipboardPolicy` ends "…the template leg the session-policy meet is computed against (ADR 0010)." because `spec.clipboardPolicy` is required (`api/v1/workspacetemplate_types.go:228-229`). `FileTransferPolicy` ends "snapshotted like ClipboardPolicy; empty resolves as Disabled (meetCeiling)." (`sessionpolicy.go:102-105`; the CRD default is `Disabled`, `workspacetemplate_types.go:235`). Leave the `ADR 0010` number alone; Task 6 renumbers it |
| `internal/provisioning/service.go:183-184` | "exact lookup never re-derives the family's newest member (the legacy catalog-name fallback is gone)." | "exact lookup never re-derives the family's newest member." |
| `internal/provisioning/sessionpolicy.go:41` | "and — from v1.3 — on each file-transfer row" | "and on each file-transfer row" |
| `internal/provisioning/sessionpolicy.go:89-90` | "Absent row is FollowTemplate on every channel: the upgrade default" | "Absent row is FollowTemplate on every channel: the default for a tenant with no ceiling" |
| `internal/operator/workspace_controller.go:1110` | removed in Task 2 | — |
| `internal/gateway/authorization.go:701-704`, `rehydrate.go:32`, `proxy.go:149` | "v0.1 behaviour is unchanged", "behaves as in v0.1", "Nil keeps v0.1 single-process" | "without a session directory (single-process mode)" |
| `internal/backend/wire.go:118,571` | "(the v0.1 behaviour)", "exactly as in v0.1" | State the behavior: "the certificate's Subject CN", "in-process, one replica" |
| `internal/runtime/linux/backend.go:381,396` | "restores the pre-1.0 any-pod-any-namespace posture" | "is the opt-out: egress to any pod in any namespace" |
| `internal/store/migrations/0001_baseline.sql:3-9` | "Generated from pg_dump … pre-1.0 migrations 001…024 … from pre-1.0 databases" | "The v1 baseline schema. Installs start from this file; later releases add numbered migrations after it." Keep the NULL-meaning sentence, then regenerate the snapshot |
| `internal/store/migrate.go:91-92` | "the database was written by a pre-1.0 build and cannot be upgraded: reinstall with a fresh database" | "this build cannot migrate a database written by a different build: install on a fresh database, or run the build that wrote it". In `baseline_test.go`, rename `TestBaseline_MigrateRefusesPre1Database` to `TestBaseline_MigrateRefusesUnknownMigration` and assert `"does not embed"` |
| `api/v1/workspacetemplate_types.go:70-74` | "Opt-in in v0.2; it becomes the default only after the R1 spike proves…" | Replace the last two sentences of the `hostUsers` comment ("Unset leaves the pod field nil — the operator's default applies when configured. Opt-in in v0.2; … (design §3.7, D26).") with: "Unset applies the operator default (`--runtime-host-users`), and leaves the pod field nil when the operator has none. The chart sets `runtime.hostUsers: false`." Sources: the flag defaults to unset (`cmd/operator/main.go:231`), and the chart passes `--runtime-host-users={{ .Values.runtime.hostUsers }}` (`deploy/helm/tinycdi/templates/operator.yaml:94`) with `hostUsers: false` (`values.yaml:713`) |
| `api/v1/workspacetemplate_types.go:94-97` | "v0.2 does not maintain a tested handler matrix" | "No runtime handler matrix is tested; the value is passed through unchanged." |
| `deploy/helm/tinycdi/values.yaml:709` | "false since v0.2 (verified …)" | "false: runtime pods run in their own user namespace (verified on the reference environment — see docs/compatibility.md)." |
| `deploy/helm/tinycdi/values.yaml:735` | "keep the pre-v0.3.1 packed-by-scoring placement" | "keep the scheduler's default packed-by-scoring placement" |
| `deploy/helm/tinycdi/values.yaml:747` | "restores the pre-1.0 posture" | "allows any pod in any namespace" |
| `web/src/api/client.ts:10` | "the tcdi_csrf cookie is gone in v0.2" | "the CSRF token comes from GET /v1/me, never from a cookie" |
| `web/src/app/me.tsx:142` | "(Me/useMe/useLoader; folded in v0.3)" | delete the parenthesis |
| `web/src/i18n/en.ts:4` | "English-only in v0.2 (D33)" | "English is the source catalog; vi mirrors every key" |
| `web/src/workspaces/reasons.tsx:2-3,10,260-262` | "and, since v0.4, an optional `params` string map" (wrapped across lines 2-3), "to the server message (or the raw token), unchanged from v0.3.x.", "(an older server never sent it)", "exactly as v0.3.x did." | "and an optional `params` string map"; "to the server message (or the raw token)."; "(the server sent none)"; "message — or the raw token when there is none." |
| `web/src/session/useConnectionWatch.ts:20,50` | "the rc.3 herd", "measured at fleet scale on rc.3" | "a fleet-wide reconnect herd", "measured at fleet scale" |
| `web/src/admin/WorkspacesAdminPage.tsx:86`, `web/src/workspaces/WorkspaceDetailPage.tsx:256` | "no takeover in v1.2" | "no admin takeover of another user's session" |
| `internal/api/openapi.yaml:2322-2327` (`templateRevision`) and `web/src/api/generated/schema.d.ts:1246-1250` | "Rows written before v0.3 do not carry the label and fall back to the numeric `template.revision`; empty when neither exists." | Delete that sentence; the description ends "…to a newer revision of its template family." (label-only, Task 3 Step 4). Then `cd web && npm run generate` |
| `deploy/helm/tinycdi/values.yaml:735`, `deploy/helm/chart_topologyspread_test.go:8`, `chart_hostaliases_parentrefs_test.go:11,108`, `chart_routeannotations_test.go:22` | "pre-v0.3.1 …", "pre-v0.3.2 render", "pre-v0.4 render" | For values.yaml see the row above. In the tests: "the render with the value unset" / "the default render" |
| `internal/provisioning/k8sapplier.go:418-420` (`ImageUpdate`) | `(OnStart \| Pinned); "" on objects published before the field existed, which callers must read as the OnStart default.` | `(OnStart \| Pinned); the CRD defaults it to OnStart, and callers test only for Pinned.` (default: `api/v1/workspacetemplate_types.go:178`). No code branch reads the empty value (`template_family.go:117`, `internal/api/statusview.go:561` compare with Pinned only) |
| `internal/provisioning/quota.go:420-421` (`checkUserLimit`) | "else unlimited (no rows — the upgrade default)." | "else unlimited (no rows)." |
| `internal/provisioning/userlimit_test.go:93-94` | "clearing the default restores unlimited — the upgrade default." | "clearing the default restores unlimited." |
| `internal/store/sessionpolicy_test.go:15-16` | "no row at all is the upgrade default and means the same thing" | "no row at all means the same thing" |
| `internal/api/openapi.yaml:2649-2650` (`AdminSessionPolicyView` source enum) and `web/src/api/generated/schema.d.ts:1487` (via `npm run generate`) | "every channel follows the template, the upgrade default." | "every channel follows the template." (no row is the normal state of a tenant that never set a ceiling) |
| `internal/backend/config.go:399` | `envOr's empty-string "unset" keeps the upgrade path at the "tenant-admin" default;` | `envOr's empty-string "unset" resolves to the "tenant-admin" default;` |
| `internal/operator/snapshot_seccomp_test.go:96` | "— the shape an operator could have written before the gate existed —" | "— for example one edited through the status subresource —" (the verification is the v1 rule that a recorded snapshot is never trusted, lines 115-119; it is not an upgrade path) |
| `internal/api/imagestale_r4_test.go:129-130` (`oldWorkspaces`) | "seeds n workspace records that predate the create-time image snapshot" | "seeds n workspace records that carry no create-time image snapshot" (a v1 row has no `ImageBuiltAt` when its template had no image-built-at annotation at create; see the `statusview.go` row above) |
| `tests/integration/session_idtoken_test.go:126-127` (the test Rule 1 renames to `TestPGSessionStore_IDTokenAbsentIsNull`) | "A session row that predates the id_token column (or was saved without one) reads back…" | "A session saved without an id_token (no sealer configured, or no token to keep) reads back…" (`internal/store/sessions.go:104-110` writes NULL in both cases) |
| `tests/integration/api_admission_test.go:738-739` | "A row carried over from a dump written under a different epoch — or one predating the epoch column — is rejected." | "A row carried over from a dump written under a different epoch is rejected." |
| `tests/soak/soak.ts:666-668,776` (`ApiDriver`) | "The mock predates T2.5's scriptable /connection endpoint, so state falls back to probing the session origin's /desktop page (200 = connected)."; "// Endpoint absent (pre-T2.5 mock): probe the session origin instead." | "State comes from GET /connection; when that answers 404, the driver probes the session origin's /desktop page instead (200 = connected)."; "// /connection answered 404: probe the session origin instead." (test tooling, not stored data: the 404 branch stays) |

For test files, run `bash .github/tests/legacy-shims.test.sh 2>&1 | grep -E '^FAIL: ([^ :]*_test\.go|[^ :]*\.(test|spec)\.tsx?|web/tests[^ :]*|tests/[^ :]*):[0-9]+:'` and apply rules 1 and 2. This covers Go tests (chart tests in `deploy/helm/` included), `tests/integration`, `tests/soak`, `web/tests/**` (including `mock-api/` and `unit/helpers.tsx`) and `web/tests-portal/*.spec.ts`. `snapshot_adopt_test.go` hits that survived Task 2 are reworded the same way.

**Citations into the planning folder.** `docs/superpowers/` (the spec and the plans) is deleted before release, so a code comment that cites "plan ruling N" or "(ruling N)" will point at nothing. These are not version hits, so the gate does not find them. Delete the parenthetical, or the clause, and keep the fact the comment states. The full list at `b41be7d` (re-locate by text):

| File:line | Today | Rewrite to |
|---|---|---|
| `internal/xferd/xferd.go:105` | "… answer anything without them here (plan ruling 24)." | "… answer anything without them here." |
| `internal/broker/credentials_envtest_test.go:226` | "… there is no fallback (plan ruling 25)." | "… there is no fallback." |
| `internal/gateway/transfer.go:165` | "… the desktop proxy uses (plan ruling 13)." | "… the desktop proxy uses." |
| `tests/integration/linux_runtime_test.go:719` | "reach KasmVNC (plan ruling 24), so …" | "reach KasmVNC, so …" |
| `web/src/session/transfer.ts:6` | "(plan ruling 12), so it works in both cookie modes." | "so it works in both cookie modes." (rejoin the sentence) |
| `web/tests-portal/session-transfer.spec.ts:17` | "… and with the partitioned one (plan ruling 12)." | "… and with the partitioned one." |
| `internal/api/filetransfers.go:69,88,147` | "(ruling 15)", "(ruling 11)", "(ruling 16)" | drop each parenthetical |
| `internal/store/filetransfer.go:12` | "(ADR 0010 T11; plan ruling 15)" | leave it; Task 6 rewrites this line |

Check: `git grep -nE '[Pp]lan ruling [0-9]+|\(rulings? [0-9]+\)' -- '*.go' '*.ts' '*.tsx' '*.mjs' '*.sh' '*.yaml' '*.tpl' ':!internal/store/filetransfer.go'` prints nothing. Feature tags such as `(F1)` and `(F5)` stay, under ruling 11.

- [ ] **Step 4: Screenshot Secret**

In `web/scripts/screenshots.mjs`, `stageSecret()` (lines 214-225): after the `username` write (line 223), add the transfer credential every workspace Secret carries. `generateCredentials` (`internal/runtime/linux/backend.go:765-812`) returns exactly five keys at `:806-810`: `username`, `password`, `tls.crt`, `tls.key` and `xfer-token`. `tcdi-xferd` exits at start without `xfer-token` (`cmd/tcdi-xferd/main.go:25-34`) and serves `:8443` (`:57`).

```js
  // tcdi-xferd owns :8443 and refuses to start without its credential.
  writeFileSync(path.join(dir, "xfer-token"), randomBytes(32).toString("base64url") + "\n", { mode: 0o600 });
```

`randomBytes` is already imported from `node:crypto` (line 28), and `writeFileSync` from `node:fs` (line 22). Add no import. Add a regression check at the end of `.github/tests/legacy-shims.test.sh`, replacing the `# (Step 4 appends the screenshot Secret check here.)` line. It reads the key list from `generateCredentials` itself, so a key the operator adds later fails the gate until the harness stages it:

```bash
# The screenshot harness stages every key the operator writes into a
# workspace Secret: the keys of generateCredentials' returned map.
keys="$(awk '/^func generateCredentials\(/{f=1} f&&/^}/{exit} f' "$ROOT/internal/runtime/linux/backend.go" \
  | grep -oE '^[[:space:]]*"[a-z0-9.-]+":' | tr -d ' \t":')"
[ -n "$keys" ] || { echo "FAIL: no Secret keys read from generateCredentials in internal/runtime/linux/backend.go"; fails=1; }
for key in $keys; do
  grep -qF "\"$key\"" "$ROOT/web/scripts/screenshots.mjs" \
    || { echo "FAIL: web/scripts/screenshots.mjs stageSecret lacks the $key Secret key"; fails=1; }
done
```

Checked at `b41be7d`: the awk extraction yields `username password tls.crt tls.key xfer-token`, and before the `xfer-token` write the check prints exactly one `FAIL: … lacks the xfer-token Secret key`. `tls.crt` and `tls.key` already appear as string literals (`path.join(dir, "tls.key")`, line 218).

- [ ] **Step 5: Regenerate and run**

```bash
make manifests generate sync-chart-crds > /tmp/w2-t4-gen.log 2>&1; echo rc=$?
(cd deploy/helm && ${TCDI_HELM:-helm} template tcdi ./tinycdi --namespace tcdi-system --include-crds -f tinycdi/ci/minimal-values.yaml > testdata/default-render.golden.yaml)
TCDI_UPDATE_SNAPSHOT=1 go test ./internal/store/ -run TestBaseline_SchemaMatchesSnapshot -count=1 > /dev/null
(cd web && npm run generate > /dev/null)
bash .github/tests/legacy-shims.test.sh; echo "gate rc=$?"
LOG=/tmp/w2-t4.log
{ go build ./... && go vet ./... && make test && go test -count=1 ./deploy/helm/; } > "$LOG" 2>&1; echo "rc=$?"; grep -nE '^(--- FAIL|FAIL|panic)' "$LOG" | head -30
(cd web && npm run typecheck && npm run lint:strings && npm run test:unit > /tmp/w2-t4-web.log 2>&1; echo "web rc=$?"; grep -nE 'FAIL|✗' /tmp/w2-t4-web.log | head)
node --check web/scripts/screenshots.mjs && echo screenshots-syntax-ok
```

Expected: `gate rc=0` with the line `legacy-shims: engine fixtures and live gate pass`, `rc=0`, `web rc=0`, and `screenshots-syntax-ok`. `git diff --stat deploy/helm/testdata/` shows only CRD description text.

- [ ] **Step 6: Commit**

```bash
git add -A .github/scripts/allowlist-grep.sh .github/tests/legacy-shims.test.sh .github/tests/legacy-shims.allow \
  web internal api config deploy cmd tests
git commit -m "chore: remove internal-version wording from code and gate C1 shims in CI

Add the shared allowlist grep engine and the legacy-shims gate (legacy,
deprecated, v1alpha1 and internal version references in Go, TS, chart and
OpenAPI sources). Stage xfer-token in the screenshot harness's Secret.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Docs gates (link checker, version grep, superpowers fence) and deletion of the history documents (C2) — tier: integrate

**Files:**
- Create: `.github/scripts/check-doc-links.py`, `.github/scripts/check-doc-versions.sh`, `.github/tests/docs-gates.test.sh`, `.github/tests/docs-versions.allow`
- Delete: `docs/releases/{v0.3.0,v0.3.1,v0.3.2,v0.4.0,v0.5.0,v0.5.1,v1.0.0,v1.1.0,v1.2.0}.md`, `docs/runbooks/upgrade.md`, `docs/security/review-v1.0.md`
- Modify (the 18 Markdown links to deleted files that the link checker reports after Step 4's `git rm`; their prose is rewritten in later tasks): `README.md:96`, `SECURITY.md:9,36,660`, `docs/README.md:24,44-52,66`, `docs/versioning.md:21`, `docs/adr/0005-backend-frontend-operator.md:5`, `docs/security/{threat-model.md:4,provenance.md:154}`
- Do NOT edit in this task (plain-text mentions of deleted paths, no link syntax, so the link checker passes; each is rewritten by the task named): `deploy/helm/tinycdi/README.md:64,93,119` and `.github/README.md:148` and `docs/runbooks/install.md:455` (Task 10), `docs/development.md:56` (Task 11), `docs/runbooks/capacity.md:86` (Task 12), `docs/adr/0005-backend-frontend-operator.md:343` (Task 7), `docs/adr/0009-mtls-cert-revocation.md:326` and `docs/adr/0010-session-data-transfer-policy.md:453` (renamed in Task 6, rewritten in Task 8), `docs/security/threat-model.md:601,622` (Task 13)
- Modify: `.github/tests/failed-digest-cleanup.test.sh:161-171,178`, `.github/scripts/setup-repo-protection.sh:30` (comment cites `review-v1.0.md §7`)
- Modify: `README.md` (`## Documentation`, adding a link to `CONTRIBUTING.md`)

**Interfaces:**
- Consumes: `.github/scripts/allowlist-grep.sh` (Task 4), with its flags `-p -a -s -r` and its allowlist format.
- Produces (every later docs task runs these on the files it touches):
  - `python3 .github/scripts/check-doc-links.py [--root DIR] [FILE...]`, which exits 0 when clean and 1 with `FAIL: file:line: target — reason`
  - `bash .github/scripts/check-doc-versions.sh [FILE...]`. Without arguments it covers the whole docs scope with the stale check; with arguments it checks just those files. `TCDI_DOCS_ROOT` and `TCDI_DOCS_ALLOW` override the root and allowlist (used by the fixtures).
  - `.github/tests/docs-gates.test.sh`, which runs the fixtures, the full-scope link check and the `docs/superpowers` fence now. Task 14 adds the full-scope version gate. The fence (`sp_refs`) ignores pathspec exclusions of the folder (`':(exclude,glob)docs/superpowers/**'`, `':!docs/superpowers/**'`, `-g '!docs/superpowers/**'`), because Task 6's `adr-refs.test.sh` and Task 13's `threat-model-refs.test.sh` carry `':(exclude,glob)docs/superpowers/**'`; any other mention of `docs/superpowers` outside the folder fails it.

- [ ] **Step 1: Write the fixture test first**

Create `.github/tests/docs-gates.test.sh`:

```bash
#!/usr/bin/env bash
# docs-gates.test.sh — the docs gates: (1) every relative Markdown link
# resolves, anchors included; (2) nothing outside the planning folder
# (docs/superpowers/) refers into it, because that folder is removed before
# release; (3) the published docs carry no internal version history or PR
# numbers (check-doc-versions.sh, allowlist .github/tests/docs-versions.allow).
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
LINKS="$ROOT/.github/scripts/check-doc-links.py"
VERSIONS="$ROOT/.github/scripts/check-doc-versions.sh"
SP_DIR="docs/superpowers"
fails=0
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# ---- link checker fixtures ---------------------------------------------
FX="$TMP/links"; mkdir -p "$FX/docs/sub" "$FX/$SP_DIR"
git -C "$FX" init -q
cat > "$FX/docs/a.md" <<'MD'
# Title — with dash
## Same
## Same
## `code` heading
[ok](sub/b.md#section-one) [ok2](#same-1) [ok3](#title--with-dash) [ok4](#code-heading)
[bad file](sub/missing.md) [bad anchor](sub/b.md#nope)
[fenced-off](superpowers/plan.md) [web](https://example.com/x#y)
~~~
[inside a fence](nowhere.md)
~~~
`[inside a code span](nowhere.md)`
[ref]: sub/b.md#section-one
MD
printf '# B\n## Section one\n' > "$FX/docs/sub/b.md"
printf '# P\n[back](../a.md)\n' > "$FX/$SP_DIR/plan.md"
git -C "$FX" add -A
out="$(python3 -I "$LINKS" --root "$FX" 2>&1)"; rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL: link fixtures: rc=$rc want 1: $out"; fails=1; }
for want in 'sub/missing.md — docs/sub/missing.md does not exist' \
            'sub/b.md#nope — no heading #nope' \
            "links into $SP_DIR/"; do
  grep -qF -- "$want" <<< "$out" || { echo "FAIL: link fixtures: missing '$want' in: $out"; fails=1; }
done
[ "$(grep -c '^FAIL:' <<< "$out")" -eq 3 ] \
  || { echo "FAIL: link fixtures: want exactly 3 findings (valid links, fences, code spans and URLs pass): $out"; fails=1; }

# ---- planning-folder fence ---------------------------------------------
# A reference into $SP_DIR/ from outside it breaks when the folder is
# removed. A pathspec that EXCLUDES the folder (":(exclude,glob)$SP_DIR/**",
# ":!$SP_DIR/**", rg's "!$SP_DIR/**") is not a reference: it is stripped
# before the check. Untracked files are searched too, so a new file is
# caught before it is committed.
sp_refs() { # sp_refs <repo root> -> offending lines, empty when clean
  git -C "$1" grep -n -I --untracked -e "$SP_DIR" -- ":(exclude,glob)$SP_DIR/**" \
    ':(exclude).github/tests/docs-gates.test.sh' ':(exclude).github/scripts/check-doc-links.py' \
    | sed -E 's#(:\(exclude[^)]*\)|:?!)docs/superpowers/\*\*##g' | grep -F -- "$SP_DIR"
}
mkdir -p "$FX/tools"
printf '%s\n' "git grep -e x -- ':(exclude,glob)$SP_DIR/**' ':!$SP_DIR/**'" "rg x -g '!$SP_DIR/**'" > "$FX/tools/excludes.sh"
printf '%s\n' "cat $SP_DIR/plan.md" > "$FX/tools/reads.sh"
out="$(sp_refs "$FX")"
grep -q '^tools/reads.sh:1:' <<< "$out" || { echo "FAIL: fence fixtures: a real reference is not caught: $out"; fails=1; }
grep -q '^tools/excludes.sh:' <<< "$out" && { echo "FAIL: fence fixtures: a pathspec exclusion is reported: $out"; fails=1; }

# ---- version gate fixtures ---------------------------------------------
FV="$TMP/vers"; mkdir -p "$FV/docs"
git -C "$FV" init -q
cat > "$FV/docs/x.md" <<'MD'
Since v0.3 the budget doubled.
See PR #117 for details.
Tag v1.0.0-rc.1 was cut.
The CRD is v1alpha1.
Upgrade from v1.2 first.
kind v0.33.0 or newer
[tags](#10-finding-tag-index) and color #123456 and TLSv1.2 and v1.0.0-rc.N
MD
git -C "$FV" add -A
printf '%s\n' 'docs/x.md :: kind v0\.33\.0 :: third-party tool version (kind), not TinyCDI history' > "$TMP/allow"
out="$(TCDI_DOCS_ROOT="$FV" TCDI_DOCS_ALLOW="$TMP/allow" bash "$VERSIONS" 2>&1)"; rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL: version fixtures: rc=$rc want 1: $out"; fails=1; }
for n in 1 2 3 4 5; do
  grep -q "^FAIL: docs/x.md:$n:" <<< "$out" || { echo "FAIL: version fixtures: line $n not flagged: $out"; fails=1; }
done
for n in 6 7; do
  grep -q "^FAIL: docs/x.md:$n:" <<< "$out" && { echo "FAIL: version fixtures: line $n must pass: $out"; fails=1; }
done

# ---- live: links ---------------------------------------------------------
out="$(python3 -I "$LINKS" --root "$ROOT" 2>&1)" || { echo "$out"; echo "FAIL: broken documentation links"; fails=1; }

# ---- live: nothing outside the planning folder references it -------------
refs="$(sp_refs "$ROOT")"
[ -z "$refs" ] || { echo "$refs"; echo "FAIL: references into $SP_DIR/ outside it (it is removed before release)"; fails=1; }

[ "$fails" -eq 0 ] && echo "docs-gates: fixtures, links and the $SP_DIR fence pass"
exit "$fails"
```

Run: `bash .github/tests/docs-gates.test.sh` — expected: it fails, because the checkers do not exist yet.

- [ ] **Step 2: Write the link checker**

Create `.github/scripts/check-doc-links.py`. This exact file was run at `b41be7d`: on a clean checkout it reports `check-doc-links: 59 files, every relative link resolves`, and it passes the fixtures above. Its `slug()` was compared with `github-slugger` 2.0.0 (the slug library GitHub's renderer follows) over all 546 headings in the repo plus edge cases (em dash → `--`, `1.x` → `1x`, code spans, `&`, `[]`, non-ASCII letters, emoji): no difference. Known gap: `_emphasis_` written with underscores keeps the `_` in the slug (no heading uses it today).

```python
#!/usr/bin/env python3
"""check-doc-links.py — offline check of every relative link in the
repository's Markdown files, tracked or new (docs/superpowers/ excluded):

  * the target file or directory must exist;
  * a #fragment into a Markdown file must name one of its headings (GitHub
    slug rules) or an explicit <a id|name="..."> anchor;
  * no file outside docs/superpowers/ may link into docs/superpowers/.

    check-doc-links.py [--root DIR] [FILE...]

With FILE arguments only those files are checked. Exit 0 when every link
resolves, 1 with one "FAIL: file:line: target — reason" line per broken
link. Standard library only; http(s), mailto and other URL schemes are
skipped, so the check never touches the network.
"""
import argparse
import os
import re
import subprocess
import sys
import unicodedata
from urllib.parse import unquote

INLINE = re.compile(r'!?\[(?:[^\[\]\\]|\\.|\[[^\]]*\])*\]\(\s*<?([^)\s>]+)>?(?:\s+(?:"[^"]*"|\'[^\']*\'))?\s*\)')
REFDEF = re.compile(r'^\s{0,3}\[[^\]]+\]:\s*<?(\S+?)>?(?:\s+["\'(].*)?\s*$')
HEADING = re.compile(r'^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$')
FENCE = re.compile(r'^\s{0,3}(`{3,}|~{3,})')
CODESPAN = re.compile(r'(`+)(?:(?!\1).)+?\1')
HTML_ANCHOR = re.compile(r'<a\s+[^>]*(?:id|name)\s*=\s*"([^"]+)"', re.I)
SCHEME = re.compile(r'^[a-zA-Z][a-zA-Z0-9+.-]*:')
FORBIDDEN = 'docs/superpowers/'


def slug(text):
    """GitHub heading slug: link text only, lower-case, keep letters,
    digits, '-' and '_', each space becomes '-', drop everything else."""
    text = re.sub(r'!?\[([^\]]*)\]\([^)]*\)', r'\1', text)
    text = CODESPAN.sub(lambda m: m.group(0).strip('`'), text)
    out = []
    for ch in text.strip().lower():
        if ch == ' ':
            out.append('-')
        elif ch in '-_' or unicodedata.category(ch)[0] in ('L', 'N') or unicodedata.category(ch) == 'Mn':
            out.append(ch)
    return ''.join(out)


def lines_outside_fences(path):
    """Yield (lineno, line) for lines that are not inside fenced code."""
    fence = None
    with open(path, encoding='utf-8', errors='replace') as fh:
        for n, line in enumerate(fh, 1):
            m = FENCE.match(line)
            if m:
                mark = m.group(1)
                if fence is None:
                    fence = mark[0] * 3
                elif mark.startswith(fence):
                    fence = None
                continue
            if fence is None:
                yield n, line.rstrip('\n')


_anchor_cache = {}


def anchors(path):
    if path not in _anchor_cache:
        seen, found = {}, set()
        for _, line in lines_outside_fences(path):
            for a in HTML_ANCHOR.findall(line):
                found.add(a)
            m = HEADING.match(line)
            if not m:
                continue
            base = slug(m.group(2))
            k = seen.get(base, 0)
            found.add(base if k == 0 else f'{base}-{k}')
            seen[base] = k + 1
        _anchor_cache[path] = found
    return _anchor_cache[path]


def tracked_markdown(root):
    """Tracked and untracked-but-not-ignored Markdown files that exist on
    disk, so a new doc is checked before it is committed."""
    out = subprocess.run(
        ['git', '-C', root, 'ls-files', '-z', '--cached', '--others', '--exclude-standard',
         '--', '*.md', ':(exclude,glob)docs/superpowers/**'],
        check=True, capture_output=True).stdout.decode()
    return sorted({p for p in out.split('\0') if p and os.path.isfile(os.path.join(root, p))})


def check(root, rel):
    fails = []
    src = os.path.join(root, rel)
    src_in_sp = rel.startswith(FORBIDDEN)
    for n, line in lines_outside_fences(src):
        bare = CODESPAN.sub('', line)
        targets = INLINE.findall(bare)
        m = REFDEF.match(bare)
        if m:
            targets.append(m.group(1))
        for t in targets:
            if SCHEME.match(t) or t.startswith('//'):
                continue
            path, _, frag = t.partition('#')
            path = unquote(path)
            if path == '':
                dest_rel = rel
            elif path.startswith('/'):
                dest_rel = os.path.normpath(path.lstrip('/'))
            else:
                dest_rel = os.path.normpath(os.path.join(os.path.dirname(rel), path))
            if dest_rel.startswith('..'):
                fails.append(f'FAIL: {rel}:{n}: {t} — points outside the repository')
                continue
            dest = os.path.join(root, dest_rel)
            if not os.path.exists(dest):
                fails.append(f'FAIL: {rel}:{n}: {t} — {dest_rel} does not exist')
                continue
            if not src_in_sp and (dest_rel + '/').startswith(FORBIDDEN):
                fails.append(f'FAIL: {rel}:{n}: {t} — links into {FORBIDDEN}, which is removed before release')
                continue
            if frag and dest_rel.endswith('.md') and os.path.isfile(dest):
                if unquote(frag) not in anchors(dest):
                    fails.append(f'FAIL: {rel}:{n}: {t} — no heading #{frag} in {dest_rel}')
    return fails


def main():
    ap = argparse.ArgumentParser(description=__doc__.split('\n')[0])
    ap.add_argument('--root', default=os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..'))
    ap.add_argument('files', nargs='*')
    a = ap.parse_args()
    root = os.path.abspath(a.root)
    files = a.files or tracked_markdown(root)
    fails = []
    for rel in files:
        fails += check(root, os.path.relpath(os.path.join(root, rel), root))
    for f in fails:
        print(f)
    if not fails:
        print(f'check-doc-links: {len(files)} files, every relative link resolves')
    return 1 if fails else 0


if __name__ == '__main__':
    sys.exit(main())
```

- [ ] **Step 3: Write the version gate**

Create `.github/scripts/check-doc-versions.sh`:

```bash
#!/usr/bin/env bash
# check-doc-versions.sh — the published docs describe only the current
# release: no internal version history (v0.x, v1.1, v1.2), release-candidate
# names (rc.<digit>), PR or issue numbers (#NN, #NNN) or v1alpha1. Reviewed
# exceptions (third-party version strings, examples of the tag format) live
# in .github/tests/docs-versions.allow, each with a reason.
#
#   check-doc-versions.sh            # the whole docs scope, stale check on (CI)
#   check-doc-versions.sh FILE...    # only these files (repo-relative), while editing
#
# TCDI_DOCS_ROOT / TCDI_DOCS_ALLOW override the repo root and the allowlist
# (fixtures). Write a release-candidate placeholder as -rc.N, not a digit.
set -uo pipefail
SELF_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ROOT="${TCDI_DOCS_ROOT:-$SELF_ROOT}"
ALLOW="${TCDI_DOCS_ALLOW:-$SELF_ROOT/.github/tests/docs-versions.allow}"
PAT='(^|[^0-9A-Za-z.])v0\.[0-9]|(^|[^0-9A-Za-z.])v1\.[12]([^0-9]|$)|\brc\.[0-9]|(^|[^0-9A-Za-z&/_#-])#[0-9]{2,3}([^0-9A-Za-z_-]|$)|v1alpha1'
ENGINE="$SELF_ROOT/.github/scripts/allowlist-grep.sh"
if [ "$#" -gt 0 ]; then
  exec bash "$ENGINE" -s -p "$PAT" -a "$ALLOW" -r "$ROOT" -- "$@"
fi
exec bash "$ENGINE" -p "$PAT" -a "$ALLOW" -r "$ROOT" -- \
  ':(glob)docs/**/*.md' ':(exclude,glob)docs/superpowers/**' ':(glob)*.md' \
  'deploy/helm/tinycdi/README.md' '.github/README.md'
```

Create `.github/tests/docs-versions.allow` with only this header. Task 14 fills it.

```text
# docs-versions.allow — reviewed exceptions to the docs version gate
# (.github/scripts/check-doc-versions.sh). One per line:
#   <glob> :: <ERE matched against the line> :: <reason, 10+ chars>
# Only third-party version strings (tools, upstream APIs) and examples of
# the release tag format belong here — never TinyCDI release history.
```

Run: `bash .github/tests/docs-gates.test.sh; echo rc=$?`
Expected: `rc=0`, with the line `docs-gates: fixtures, links and the docs/superpowers fence pass`. The deletions have not happened yet, so the live link check passes.

- [ ] **Step 4: Delete the history documents**

```bash
git rm -q docs/releases/v0.3.0.md docs/releases/v0.3.1.md docs/releases/v0.3.2.md docs/releases/v0.4.0.md \
  docs/releases/v0.5.0.md docs/releases/v0.5.1.md docs/releases/v1.0.0.md docs/releases/v1.1.0.md \
  docs/releases/v1.2.0.md docs/runbooks/upgrade.md docs/security/review-v1.0.md
python3 .github/scripts/check-doc-links.py
```

Expected: exactly 18 `FAIL:` lines, all `does not exist`: `README.md:96`, `SECURITY.md:9,36,660`, `docs/README.md:24,44,46-52,66`, `docs/adr/0005-backend-frontend-operator.md:5`, `docs/security/provenance.md:154`, `docs/security/threat-model.md:4`, `docs/versioning.md:21`. Fix each one exactly as below (in the table, ` / ` separates source lines). Leave the surrounding prose; later tasks rewrite it. Do not touch the plain-text mentions listed under **Files** ("Do NOT edit").

| Site | Replace | With |
|---|---|---|
| `README.md:95-97` | `**Status:** the 1.0 release line. An internal pre-1.0 security review is` / `complete ([report](docs/security/review-v1.0.md)); it is **not an` / `independent audit**.` | `**Status:** the 1.0 release line. Each release's notes state its` / `security-review status; it is not an independent audit.` (the following sentence, "The versioning and stability contract is in [`docs/versioning.md`](docs/versioning.md).", stays) |
| `SECURITY.md:8-12` | the five `>` lines from `> **TinyCDI completed an internal pre-1.0 security review** — report:` to `> in this file are the bar.` | `> **Security review.** Each release's notes state its security-review` / `> status. The mandatory rules and the blocking CI scanners in this file` / `> are the bar; an independent external review remains an open item.` (the `>` line and the `> **Architecture note:**` block after it stay) |
| `SECURITY.md:35-37` | `(see` / `[docs/runbooks/upgrade.md](docs/runbooks/upgrade.md) and` / `[docs/versioning.md](docs/versioning.md))` | `(see` / `[docs/versioning.md](docs/versioning.md))` |
| `SECURITY.md:660` | `follow [upgrade](docs/runbooks/upgrade.md) and` | `follow [the versioning contract](docs/versioning.md) and` |
| `docs/versioning.md:20-21` | `` from `v0.5.1` to `1.0.0` — as described in`` / `[runbooks/upgrade.md](runbooks/upgrade.md).` | `` from `v0.5.1` to `1.0.0`.`` (Task 9 rewrites this bullet) |
| `docs/README.md:24` | the bullet `- [runbooks/upgrade.md](runbooks/upgrade.md) — upgrade ordering and rollback` | delete the line |
| `docs/README.md:42-53` | the whole `## Releases` section (heading, eight bullets, trailing blank line) | delete; Task 9 adds it back for v1.0.0 |
| `docs/README.md:66-68` | the three-line bullet `- [security/review-v1.0.md](security/review-v1.0.md) — the v1.0 internal …residuals` | delete |
| `docs/adr/0005-backend-frontend-operator.md:5` | `[docs/security/review-v1.0.md](../security/review-v1.0.md)` | `` `docs/security/review-v1.0.md` `` (Task 7 rewrites the status line) |
| `docs/security/provenance.md:154` | ``[`review-v1.0.md` §7](review-v1.0.md)`` | `` `review-v1.0.md` §7`` (Task 11 drops it) |
| `docs/security/threat-model.md:4` | ``[`review-v1.0.md`](review-v1.0.md)`` | `` `review-v1.0.md` `` (Task 13 rewrites the file) |

`.github/scripts/setup-repo-protection.sh:30`: the comment line `#      joins (the trigger recorded in review-v1.0.md §7): set` becomes `#      joins: set` (the trigger is stated in the comment itself; the cited section is gone).

`.github/tests/failed-digest-cleanup.test.sh`:
- Delete lines 161-171: the comment `# the frozen v1.0 review record must not carry this fix's closure —` through the `fi` that closes the `git rev-parse --verify -q origin/main` block, plus the blank line after it. The record it froze no longer exists.
- Replace the loop header at line 178 (`for doc in "$ROOT/docs/releases/v1.1.0.md" "$ROOT/docs/security/provenance.md"; do`) with these two lines, keeping the loop body (`grep -qiE "$ABORT_RE" "$doc" && …`) and `done` unchanged:

```bash
for doc in "$ROOT"/docs/releases/*.md "$ROOT/docs/security/provenance.md"; do
  [ -f "$doc" ] || continue
```

  Until Task 9 writes `docs/releases/v1.0.0.md` the directory is gone, the glob stays literal and `[ -f ]` skips it; from Task 9 on, every release note is covered. (Verified: the test passes at the end of this task.)

In `README.md` `## Documentation` (lines 80-87), add this as the last bullet:

```markdown
- [Contributing](CONTRIBUTING.md) — CI checks and how changes are proposed
```

In the same list, change the Runbooks bullet's text "install, upgrade, retained data, DR, capacity" to "install, retained data, DR, capacity, audit".

- [ ] **Step 5: Run the gates and the affected tests**

```bash
bash .github/tests/docs-gates.test.sh; echo "docs-gates rc=$?"
for t in .github/tests/failed-digest-cleanup.test.sh .github/tests/release-notes-review-status.test.sh .github/tests/supply-chain-hardening.test.sh .github/tests/legacy-shims.test.sh; do bash "$t" > /tmp/w2-t5-sh.log 2>&1 || { echo "FAIL $t"; tail -n 5 /tmp/w2-t5-sh.log; }; done
git grep -n -e 'runbooks/upgrade\.md' -e 'review-v1\.0\.md' -e 'releases/v0\.' -e 'releases/v1\.[12]' -- ':!docs/superpowers/**' ':!.github/workflows/release.yml' ':!.github/tests/release-notes-review-status.test.sh'
python3 .github/scripts/check-doc-links.py; echo "links rc=$?"
```

Expected:
- `docs-gates rc=0`, no `FAIL` lines from the loop, and `links rc=0` with `check-doc-links: 48 files, every relative link resolves`.
- The `git grep` prints exactly 16 plain-text mentions, no link syntax: `.github/README.md:148`, `deploy/helm/tinycdi/README.md:64,93,119`, `docs/adr/0005-backend-frontend-operator.md:5,343`, `docs/adr/0009-mtls-cert-revocation.md:326`, `docs/adr/0010-session-data-transfer-policy.md:453`, `docs/development.md:56`, `docs/runbooks/capacity.md:86`, `docs/runbooks/install.md:455`, `docs/security/provenance.md:154`, `docs/security/threat-model.md:4,600,601,622`. Each is rewritten by the task named under **Files**. Write the count into the commit body.
- `legacy-shims.test.sh` is unaffected (its scope has no `*.md`); it is listed to show Task 4's gate still passes.

`release.yml:1355` and `release-notes-review-status.test.sh:16` keep the review path as text: R1 recreates the file (Plan ruling 10).

- [ ] **Step 6: Commit**

```bash
git add -A -- .github docs README.md SECURITY.md ':(exclude)docs/superpowers'
git status --short   # staged: 4 new gate files, 11 deletions, 9 modified files; nothing under docs/superpowers/
git commit -m "docs: add the docs gates and delete the internal release history

Offline link checker (anchors, no links into the planning folder), the
version and PR-reference gate (fixtures now, full scope at the end of W2),
and removal of the pre-1.0 release notes, the upgrade runbook and the old
internal review report. CONTRIBUTING.md is linked from the README.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: ADR renumbering, ADR 0002 replaced by "API conventions", ADR 0008 folded into the threat model (C3) — tier: design

**Files:**
- Delete: `docs/adr/0008-kasmvnc-bruteforce.md`, `docs/adr/0002-public-api.md`
- Create: `docs/adr/0002-api-conventions.md`, `.github/tests/adr-refs.test.sh`
- Rename: `docs/adr/0009-mtls-cert-revocation.md` → `0008-mtls-cert-revocation.md`, `0010-session-data-transfer-policy.md` → `0009-…`, `0011-audit-store-and-retention.md` → `0010-…`
- Modify: `docs/security/threat-model.md` (new subsection under `### Boundary 6`, line 374; references at 606 and 720), `docs/security/test-inventory.md:142`, `docs/README.md` (the `## ADRs` section; find it by heading, because Task 5 shifts its lines)
- Modify: every file that references ADR 0009, 0010 or 0011, via the scripted rename in Step 4. After Task 5's deletions this is 141 references (0009: 42, 0010: 82, 0011: 17) in about 75 files, Go and TS comments, Go flag-help and metric-help strings, chart templates, values and CRD descriptions included.
- Modify by hand (Step 4): the line-wrapped reference `deploy/helm/tinycdi/values.yaml:298-299` (`(ADR` / `# 0009, SR-3-F5)`), which the scripted rename cannot see; the `T10`/`T11` markers listed in Step 4; the assertion message at `internal/provisioning/sessionpolicy_test.go:34`
- Modify: lines 1–N of every remaining ADR (Step 4's header normalization: H1 `# ADR NNNN — <Title>`, then `Status: Accepted`, with the old status paragraph and date line removed), so `adr-refs.test.sh` passes at the end of this task
- Modify: `internal/api/openapi.yaml:44-63` (the `info.description` "Errors" paragraph and `license`), then `web/src/api/generated/schema.d.ts`
- Regenerate: CRDs (`make manifests generate sync-chart-crds`) and the chart golden

**Interfaces:**
- Consumes: the docs gates from Task 5.
- Produces:
  - final ADR numbers: 0001 runtime streaming, 0002 API conventions, 0003 gateway↔broker internal API, 0004 launch origin policy, 0005 backend/frontend/operator, 0006 rate-limit state, 0007 sign out everywhere, 0008 mTLS cert revocation, 0009 session data-transfer policy, 0010 audit store and retention. Tasks 7–14 use these numbers.
  - threat-model anchor `#kasmvnc-credential-brute-force` (heading `#### KasmVNC credential brute force`), used by Task 13
  - `.github/tests/adr-refs.test.sh`, green from this task on (Tasks 7 and 8 must keep it green)
  - code references into ADR 0009 that keep the existing section numbers `§1`–`§5` and `§5 (i)` (Task 8 keeps those numbers)

- [ ] **Step 1: Write the reference test first**

Create `.github/tests/adr-refs.test.sh`:

```bash
#!/usr/bin/env bash
# adr-refs.test.sh — the ADR set is numbered without gaps (0001..N), every
# "ADR NNNN" reference anywhere in the repo names an existing ADR, and every
# docs/adr/NNNN-<slug>.md path reference names the real file.
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
fails=0
mapfile -t files < <(cd "$ROOT/docs/adr" && ls | grep -E '^[0-9]{4}-[a-z0-9-]+\.md$' | sort)
[ "${#files[@]}" -gt 0 ] || { echo "FAIL: no ADRs found"; exit 1; }
i=1
for f in "${files[@]}"; do
  want="$(printf '%04d' "$i")"
  [ "${f:0:4}" = "$want" ] || { echo "FAIL: ADR numbering gap or duplicate at $f (want $want)"; fails=1; }
  head -n 1 "$ROOT/docs/adr/$f" | grep -qE "^# ADR ${f:0:4} — " \
    || { echo "FAIL: docs/adr/$f: first line must be '# ADR ${f:0:4} — <title>'"; fails=1; }
  grep -qE '^Status: Accepted$' "$ROOT/docs/adr/$f" \
    || { echo "FAIL: docs/adr/$f: needs the line 'Status: Accepted'"; fails=1; }
  i=$((i + 1))
done
max="$(printf '%04d' $((i - 1)))"
while IFS= read -r ref; do
  n="$(grep -oE '[0-9]{4}' <<< "$ref")"
  ls "$ROOT/docs/adr/$n-"*.md > /dev/null 2>&1 \
    || { echo "FAIL: reference '$ref' names no ADR (highest is $max)"; fails=1; }
done < <(git -C "$ROOT" grep -ohE 'ADR[ _-]?[0-9]{4}' -- ':(exclude,glob)docs/superpowers/**' | sort -u)
while IFS= read -r ref; do
  [ -f "$ROOT/docs/$ref" ] || { echo "FAIL: path reference '$ref' does not exist"; fails=1; }
done < <(git -C "$ROOT" grep -ohE 'adr/[0-9]{4}-[a-z0-9-]+\.md' -- ':(exclude,glob)docs/superpowers/**' | sort -u)
[ "$fails" -eq 0 ] && echo "adr-refs: $((i - 1)) ADRs, gap-free, every reference resolves"
exit "$fails"
```

Run: `bash .github/tests/adr-refs.test.sh; echo rc=$?` — expected: `rc=1`, with eleven `needs the line 'Status: Accepted'` lines and two `first line must be` lines (0001, 0002). The reference checks pass at this point (0001–0011 exist). Step 4 normalizes every ADR's first lines and Step 5 writes the new 0002, so the test passes at the end of this task. It has to: CI's `workflow-policy` job and the workflow's verify command (Decision 8) run every `.github/tests/*.test.sh` on every commit. The test excludes `docs/superpowers/**` (the spec and plans cite the old numbers and are deleted before release); the planner checked that `git grep … -- ':(exclude,glob)docs/superpowers/**'` drops those files.

- [ ] **Step 2: Record the reference counts before renaming**

```bash
for n in 0009 0010 0011; do printf '%s ' $n; git grep -ohE "ADR[ _-]?$n|adr/$n-|(^|[^0-9])$n-(mtls|session-data|audit-store)" -- ':!docs/superpowers/**' | wc -l; done | tee /tmp/w2-t6-before.txt
```

- [ ] **Step 3: Fold ADR 0008 into the threat model, then delete it**

Before writing, check each fact:

```bash
sed -n 14,24p build/linux-base/kasmvnc.yaml; sed -n 78,82p build/linux-base/kasmvnc.yaml
sed -n 200,212p build/kasm-adapter/entrypoint.sh; sed -n 236,240p build/kasm-adapter/entrypoint.sh
sed -n 98,110p internal/xferd/xferd.go     # the 401 for a request without Basic credentials
sed -n 63,75p internal/xferd/xferd.go      # forwarding headers passed through to KasmVNC
sed -n 1662,1677p internal/runtime/linux/backend.go   # ingress: TCP streamingPort (8443) from gateway-role pods only
rg -n 'streamingPort int32' internal/runtime/linux/backend.go
sed -n 505,515p tests/integration/kasm_adapter_test.go
rg -n 'func TestProxy_StripsClientAuth' internal/gateway
rg -n 'func \(g \*Gateway\) rewriteForwarded' internal/gateway/proxy.go
```

Expected at `b41be7d`: `blacklist_threshold: 5` / `blacklist_timeout: 10` at `kasmvnc.yaml:80-81` and `entrypoint.sh:238-239`; `interface: 127.0.0.1` / `websocket_port: 8444` at `kasmvnc.yaml:20-21` and `entrypoint.sh:201-202`; `streamingPort int32 = 8443` at `backend.go:139`; `TestProxy_StripsClientAuth` at `internal/gateway/proxy_test.go:364`; `rewriteForwarded` at `proxy.go:1005`.

Add a subsection to `docs/security/threat-model.md`, at the end of `### Boundary 6` (before `### Boundary 7`):

```markdown
#### KasmVNC credential brute force

KasmVNC enforces a per-address credential blacklist (`blacklist_threshold:
5` failures, `blacklist_timeout: 10` seconds; `build/linux-base/kasmvnc.yaml`
and the kasm adapter's generated config in `build/kasm-adapter/entrypoint.sh`).
No client can feed that check a guess:

- KasmVNC listens on `127.0.0.1:8444` (TLS) inside the pod. `tcdi-xferd`
  owns the pod's only exposed port, `:8443`, and reverse-proxies the desktop
  to it. A desktop request without Basic credentials is answered by
  `tcdi-xferd` with its own `401` and never forwarded, so anonymous probes
  never reach KasmVNC.
- The pod NetworkPolicy admits ingress on TCP `8443` from gateway pods only.
  Xvnc also binds `:8444/udp` (KasmVNC has no switch to turn its UDP
  transport off); no ingress rule admits UDP, and the kasm adapter contract
  test asserts no other UDP socket exists.
- The gateway overwrites `Authorization` on every proxied request with the
  workspace's own credential (`TestProxy_StripsClientAuth`), so a client
  cannot present a guess through the gateway either.
- The blacklist stays on as defense in depth. It keys on the forwarded
  client address, which the gateway rebuilds and `tcdi-xferd` passes
  through unchanged.

Residual: a process inside the pod can reach `127.0.0.1:8444` directly, but
it already runs as the desktop user, who can read the credential from the
workspace Secret mount, so it gains nothing.
```

Correct anything the commands contradict (for example, if `kasmvnc.yaml` no longer sets the blacklist at lines 80-81). Then:
- `threat-model.md:606` (S18) and `:720` say "(see *KasmVNC credential brute force* above)" in place of "ADR 0008".
- `test-inventory.md:142` says "(threat model, KasmVNC credential brute force)".
- In `docs/README.md` `## ADRs`, delete the three-line `adr/0008-kasmvnc-bruteforce.md` bullet.
- Task 5 already deleted `docs/releases/v0.5.0.md`, the only other file citing ADR 0008.
- Run `git rm -q docs/adr/0008-kasmvnc-bruteforce.md`.

Run: `git grep -nE 'ADR[ _-]?0008|adr/0008|0008-kasmvnc' -- ':!docs/superpowers/**'` — expected: no output. Step 4 depends on that.

- [ ] **Step 4: Rename 0009 → 0008, then 0010 → 0009, then 0011 → 0010, in that order**

```bash
renum() { # renum <old> <new> <slug>
  local old="$1" new="$2" slug="$3"
  git mv "docs/adr/$old-$slug.md" "docs/adr/$new-$slug.md"
  git grep -lE "ADR[ _-]?$old|$old-$slug" -- ':!docs/superpowers/**' | while IFS= read -r f; do
    perl -pi -e "s/\bADR([ _-]?)$old\b/ADR\${1}$new/g; s/(^|[^0-9])$old-$slug/\${1}$new-$slug/g" "$f"
  done
}
renum 0009 0008 mtls-cert-revocation
renum 0010 0009 session-data-transfer-policy
renum 0011 0010 audit-store-and-retention
for n in 0008 0009 0010; do printf '%s ' $n; git grep -ohE "ADR[ _-]?$n|adr/$n-|(^|[^0-9])$n-(mtls|session-data|audit-store)" -- ':!docs/superpowers/**' | wc -l; done | tee /tmp/w2-t6-after.txt
paste /tmp/w2-t6-before.txt /tmp/w2-t6-after.txt
```

Expected (the planner ran this on a clone with Task 5's deletions applied): `0009 42 / 0008 42`, `0010 82 / 0009 82`, `0011 17 / 0010 17`. Ascending order is safe because Step 3 removed every ADR 0008 reference first, so `0009 → 0008` cannot merge into an existing number, and each later pass only matches the old number, never one an earlier pass wrote.

The rename cannot see a reference wrapped between `ADR` and the number. There is exactly one: `deploy/helm/tinycdi/values.yaml:298-299` reads `(ADR` at the end of line 298 and `# 0009, SR-3-F5)` on line 299. Change `0009` on line 299 to `0008`. Check that no other wrapped one exists: `git grep -nE '\bADR\s*$' -- ':!docs/superpowers/**'`. Expected: besides `values.yaml:298`, only `SECURITY.md:294` and `docs/security/test-inventory.md:322` (next lines start with the unchanged `0005` and `0006`) and prose lines inside ADRs 0008 and 0009 ("this ADR", "the ADR").

**Task markers.** Old ADR 0010's `## Implementation plan` named two change sets, `T10` (the tenant ceiling and its admin API) and `T11` (file transfer). Task 8 deletes that section, so no code may cite them. Task 8 keeps ADR 0009's existing section numbers: `§1` policy model and lattice, `§2` resolution and recorded result, `§3` when a change takes effect, `§4` admin API, `§5` file transfer with `(i)` the in-pod helper. Line numbers in this step are at `b41be7d`; Tasks 1–5 edit some of these files, so find each line by its quoted text. The 39 existing `ADR 0010 §N` references outside `docs/adr/` (one of them wrapped, `internal/provisioning/sessionpolicy_internal_test.go:13-14`) (now `ADR 0009 §N`) already use those numbers. Replace the markers:

1. `git grep -lE 'ADR 0009 T11' -- ':!docs/superpowers/**' | xargs perl -pi -e 's/ADR 0009 T11/ADR 0009 §5/g'`. This covers the 14 one-line references: `build/frontend/main.go:165`, `deploy/helm/tinycdi/README.md:250`, `deploy/helm/tinycdi/values.yaml:376`, `docs/adr/0005-backend-frontend-operator.md:161`, `internal/api/filetransfers.go:48`, `internal/backend/config.go:130`, `internal/backend/wire.go:438`, `internal/gateway/transfer.go:19`, `internal/provisioning/retained.go:134`, `internal/provisioning/service.go:33`, `internal/provisioning/sessionpolicy.go:31`, `internal/store/filetransfer.go:12`, `web/src/session/transfer.ts:2` and `web/tests/unit/admin/sessionpolicy.test.tsx:82`.
2. In `internal/store/filetransfer.go:12`, also drop `; plan ruling 15`. The line becomes `// File-transfer row lifetimes (ADR 0009 §5).`
3. Wrapped `T11`: `deploy/helm/tinycdi/templates/backend.yaml:253` and `internal/provisioning/template_family.go:45` start with `# T11);` and `// T11):`. Replace `T11` with `§5` on those two lines.
4. `T10`: `internal/api/adminsessionpolicy.go:96` `ADR 0009 (T10)` → `ADR 0009 §4`; `internal/backend/wire.go:752` `(ADR 0009, T10)` → `(ADR 0009 §4)`; `internal/store/sessionpolicy_test.go:14` `(ADR 0009, T10)` → `(ADR 0009 §1)`.
5. `internal/provisioning/sessionpolicy_test.go:34`: the message `"fileTransfer resolves Disabled until T11: %+v"` becomes `"a template with no file-transfer policy must resolve Disabled, got %+v"`.

Run: `git grep -nE '\bT1[01]\b' -- ':!docs/superpowers/**' ':!docs/adr/0009-session-data-transfer-policy.md' ':!deploy/helm/testdata/default-render.golden.yaml'` — expected: no output. ADR 0009 itself keeps its markers until Task 8 rewrites it. The golden is regenerated below.

**ADR headers.** Normalize the first lines of every ADR except 0002, which Step 5 writes whole. Run from the repo root:

```bash
python3 - <<'PY'
import pathlib, re
titles = {
    "0001": "Runtime streaming",
    "0003": "Internal gateway↔broker API (mTLS)",
    "0004": "Launch POST origin policy",
    "0005": "Backend, frontend, operator and per-workspace session hosts",
    "0006": "Rate-limiter state placement",
    "0007": "Sign out everywhere",
    "0008": "Per-certificate revocation on the internal mTLS API",
    "0009": "Session data-transfer policy",
    "0010": "Audit store and retention",
}
for n, title in titles.items():
    (p,) = pathlib.Path("docs/adr").glob(n + "-*.md")
    lines = p.read_text(encoding="utf-8").split("\n")[1:]  # drop the old H1
    out, skipping = [], False
    for ln in lines:
        if re.match(r"^(Status|Trạng thái):", ln):
            skipping = True            # drop the whole status paragraph
            continue
        if skipping:
            if ln.strip() == "":
                skipping = False
            continue
        if re.match(r"^(Date|Ngày):", ln):
            continue                   # drop the date line
        out.append(ln)
    while out and out[0].strip() == "":
        out.pop(0)
    p.write_text(f"# ADR {n} — {title}\n\nStatus: Accepted\n\n" + "\n".join(out), encoding="utf-8")
PY
for f in docs/adr/*.md; do sed -n 1,3p "$f"; done
```

Expected: each file opens with `# ADR NNNN — <Title>`, a blank line and `Status: Accepted`. The bodies are unchanged; Tasks 7 and 8 rewrite them. The 0005 status paragraph held the only remaining link into `docs/security/` from an ADR header; it goes with the paragraph.

Regenerate the derived files: `make manifests generate sync-chart-crds`, the chart golden (Global Constraints) and `cd web && npm run generate`. The rename already edited the CRD descriptions and `schema.d.ts` in place, so regeneration must produce no further diff there; the golden picks up the `backend.yaml` comment. Expected: `git status` shows only the renames, the reference and marker edits and the ADR header edits.

- [ ] **Step 5: Write ADR 0002 "API conventions" (English), replacing 0002-public-api**

Check the facts first:

```bash
sed -n 13,16p internal/api/page.go; sed -n 30,57p internal/api/page.go; sed -n 59,98p internal/api/page.go
sed -n 21,40p internal/api/errors.go; sed -n 70,108p internal/api/errors.go; sed -n 110,116p internal/api/errors.go; sed -n 188,198p internal/api/errors.go
sed -n 204,255p internal/api/errors.go | grep -n 'Retry-After\|retryAfterClamped'
sed -n 10,40p internal/api/etag.go
rg -n 'CheckIfMatch\(r' internal/api --glob '!*_test.go'
rg -n 'Header\(\)\.Set\("ETag"' internal/api --glob '!*_test.go'
sed -n 11,31p internal/api/actions.go
grep -nE '^  /v1/[^ ]*:[a-z-]+:$' internal/api/openapi.yaml
sed -n 64,111p internal/api/auditroutes.go | grep -n 'POST\|PUT\|DELETE'
rg -n 'Idempotency-Key' internal/api/workspaces.go internal/api/data.go
sed -n 611,630p internal/api/workspaces.go; sed -n 706,719p internal/api/workspaces.go
sed -n 20,30p internal/provisioning/idempotency.go; sed -n 145,152p internal/provisioning/idempotency.go
sed -n 55,62p internal/api/auth.go; sed -n 126,130p internal/api/auth.go; sed -n 147,172p internal/api/middleware.go; sed -n 178,199p internal/api/middleware.go
rg -n 'csrfToken|CSRFToken' internal/api/me.go; sed -n 1750,1757p internal/api/openapi.yaml
rg -n 'func tenantAdminPrincipal' internal/api
rg -n 'func AppendIntent' internal/provisioning/outbox.go; sed -n 22,28p internal/api/openapi.yaml
rg -n 'func TestErrorCodesMatchOpenAPI|func TestOpenAPIContract_' internal/api | head -3
```

Create `docs/adr/0002-api-conventions.md` with this outline. Each bullet is a fact the section must state, with its source as checked at `b41be7d`:

```markdown
# ADR 0002 — API conventions

Status: Accepted

## Context
- One public HTTP API under `/v1` on the portal origin, described by
  `internal/api/openapi.yaml` (OpenAPI 3.1). The portal (`web/`) is its only
  client; its types are generated from the document (`npm run generate`,
  `web/package.json:13`). Handler output is checked against the document's
  schemas (`TestOpenAPIContract_*`, `internal/api/openapi_contract_test.go`),
  and the error-code enum by `TestErrorCodesMatchOpenAPI`
  (`internal/api/errors_test.go:30`).

## Decision
### Resources and custom methods
- Resource paths are nouns under `/v1`. An action that is not create, read,
  update or delete is a custom method: `POST` with a `:verb` last segment.
  The full list (`grep -nE '^  /v1/[^ ]*:[a-z-]+:$' openapi.yaml`, eight
  entries): `POST /v1/session:touch`, `POST /v1/me/sessions:revoke-all`,
  `POST /v1/workspaces/{workspaceId}:start`, `:stop`,
  `POST /v1/admin/tenants/{tenant}/session-policy:apply`,
  `POST /v1/admin/tenants/{tenant}/users/{ownerRef}/sessions:revoke-all`,
  `POST /v1/data/{dataId}:attach`, `:purge`.
- Dispatch: a method on a collection member is registered once as
  `POST /v1/workspaces/{ref}` or `POST /v1/data/{ref}`
  (`internal/api/auditroutes.go:105-110`), and `api.ActionRouter`
  (`internal/api/actions.go:11-31`) splits `{ref}` at the `:` and calls the
  verb's handler; an unknown verb, an empty id or a second `:` is
  `404 NOT_FOUND`. An id never contains `:`. A method on a fixed path
  (`session:touch`, `sessions:revoke-all`, `session-policy:apply`) is a
  plain ServeMux route.
- Lifecycle requests are recorded intents: the API writes the intent to
  the transactional outbox (`provisioning.AppendIntent`,
  `internal/provisioning/outbox.go:106`) and the operator reconciles the
  `Workspace` CR. `POST /v1/workspaces` answers `201`; `:start`, `:stop`
  and `DELETE /v1/workspaces/{workspaceId}` answer `202`. End users never
  write custom resources (`openapi.yaml:25-28`).
### Lists
- Query parameters `limit` (default 50, maximum 500;
  `DefaultPageLimit`/`MaxPageLimit`, `page.go:14-15`), `sort` (an
  allowlisted field, `-` prefix for descending; `parseSort`, `page.go:59-65`)
  and `cursor` (opaque keyset position, pinned to the sort it was minted
  under; `page.go:30-45`). A malformed value, or a cursor from another sort,
  is `400 INVALID_REQUEST` (`ParsePage`, `page.go:67-98`).
- Response `{"items": [...], "nextCursor": "..."}`; `nextCursor` is omitted
  on the last page (`Page[T]`, `page.go:54-57`). No offset paging and no
  total count.
### Errors
- Every 4xx/5xx body is the envelope `{"error": {"code", "message",
  "retryable", "requestId", "details"?}}` (`WriteError`, `errors.go:188-198`;
  `Error`/`ErrorBody` in `openapi.yaml` components). `code` is a stable enum
  clients switch on; `message` is human-readable and not part of the
  contract; `retryable` says whether the same request may succeed later;
  `requestId` correlates with logs and audit events.
- A table of the 18 codes with their HTTP status and default `retryable`,
  generated from `errors.go:22-39` (constants with status comments),
  `HTTPStatus` (`errors.go:71-98`) and `Retryable` (`errors.go:100-107`:
  only `INVALID_STATE`, `RATE_LIMITED`, `UNAVAILABLE`, `INTERNAL` default to
  true).
- A quota refusal that a pending release will clear and a per-user-limit
  refusal carry `retryable: true` and a `Retry-After` clamped to 1..30 s
  (`errors.go:204-255`); `RATE_LIMITED` (429) carries `Retry-After`.
### Concurrency
- Admin resources with a stored version return a strong `ETag`
  (`ETagOf`, SHA-256 of the version, `etag.go:10-14`) on read: the tenant
  quota, the default and per-user limits, and the session-policy ceiling
  (`adminquota.go:271`, `adminuserlimit.go:225,322`,
  `adminsessionpolicy.go:185`).
- Their writes require `If-Match` (`CheckIfMatch`, `etag.go:16-40`, strong
  comparison, a list of tags accepted): `PUT …/quota`
  (`adminquota.go:341`), `PUT`/`DELETE …/user-limits/{ownerRef}` and
  `PUT …/user-limits/default` (`adminuserlimit.go:210,249,299`),
  `PUT …/session-policy` (`adminsessionpolicy.go:283`) and
  `POST …/session-policy:apply` (`adminsessionpolicyapply.go:101`). Missing
  → `428 PRECONDITION_REQUIRED`; stale → `412 PRECONDITION_FAILED`;
  `If-Match: *` only creates a resource that does not exist yet. The API has
  no `PATCH`.
### Idempotency
- `Idempotency-Key` (8..128 characters) is required on `POST /v1/workspaces`
  (`workspaces.go:475-479`), `:start` (`signal(..., keyRequired=true)`,
  `workspaces.go:611-626,707-709`) and `:attach` (`data.go:355-359`), and
  optional on `:stop`, `DELETE /v1/workspaces/{workspaceId}` and `:purge`
  (`data.go:432-436`). A missing or malformed required key is
  `400 INVALID_REQUEST`.
- Same key and same body return the recorded result; same key with a
  different body is `409 IDEMPOTENCY_CONFLICT`. Keys are scoped to the
  authenticated principal (`provisioning/idempotency.go:20-30`) and pruned
  after 24 hours (`PruneIdempotency`, `idempotency.go:145-152`).
### Authentication and CSRF
- The OIDC-backed session cookie authenticates; every `GET` is passive and
  never extends the idle deadline (`RequireAuthPassive`,
  `middleware.go:86`; `openapi.yaml` `info.description`).
- Every unsafe method needs the `X-CSRF-Token` header (default of
  `CSRFHeader`, `auth.go:59-61,128-129`; `openapi.yaml` security scheme
  `csrfToken`) equal, in constant time, to the token `GET /v1/me` returns as
  `csrfToken` (`me.go:42,79`); otherwise `403 CSRF_FAILED`
  (`RequireCSRF`, `middleware.go:147-172`). Cookie-authenticated unsafe
  requests also pass an Origin / `Sec-Fetch-Site` check
  (`RequireTrustedOrigin`, `middleware.go:178-199`).
- `/v1/admin/tenants/{tenant}/…` requires the tenant-admin role for that
  tenant (`tenantAdminPrincipal`, `adminquota.go:218`). Every mutating
  route and every admin route is audited (ADR 0010).
### Compatibility within /v1
- Additive changes only within `/v1`: new operations, optional fields and
  enum values; clients ignore unknown fields (`docs/versioning.md`).

## Consequences
- Cursor paging has no "jump to page N" and no total count.
- Every admin write is a read-modify-write that carries the ETag it read.
- One error envelope means one client error path; the code enum is part of
  the contract and grows only additively.

## Alternatives considered
- Offset/limit paging: rejected, unstable under concurrent writes and its
  cost grows with the offset.
- Weak ETags or `Last-Modified`: rejected, second resolution and no byte
  identity.
- Per-route error shapes: rejected, every client would need one parser per
  route.
```

Verify the codes table and the custom-method list against the commands above. Delete `docs/adr/0002-public-api.md`. In `docs/README.md` `## ADRs`, point the 0002 bullet at the new file: "[adr/0002-api-conventions.md](adr/0002-api-conventions.md) — API conventions: resources, custom methods, paging, errors, ETags, idempotency".

In `internal/api/openapi.yaml` `info.description` (lines 52-58), replace the flat error body "`{code, message, retryable, requestId}`" with "`{\"error\": {code, message, retryable, requestId, details?}}`". Keep "(see ADR 0002)" at line 27: the new ADR states the outbox rule. The `license` block (lines 60-62: `name: Proprietary`, `identifier: LicenseRef-Proprietary`) becomes `name: MIT` and `identifier: MIT`, matching `LICENSE`. Run `cd web && npm run generate`; `info` does not render into `schema.d.ts`, so expect no diff from these two edits.

- [ ] **Step 6: Run**

```bash
bash .github/tests/adr-refs.test.sh; echo "adr-refs rc=$?"
for t in .github/tests/*.test.sh; do bash "$t" > /tmp/w2-t6-sh.log 2>&1 || { echo "FAIL $t"; tail -n 5 /tmp/w2-t6-sh.log; }; done
bash .github/scripts/check-doc-versions.sh docs/adr/0002-api-conventions.md; echo "versions rc=$?"
LOG=/tmp/w2-t6.log
{ go build ./... && go vet ./... && go test -count=1 ./internal/api/... ./internal/backend/ ./internal/observability/ ./internal/provisioning/ ./internal/store/ ./internal/tlsreload/ ./deploy/helm/; } > "$LOG" 2>&1; echo "rc=$?"; grep -nE '^(--- FAIL|FAIL)' "$LOG" | head
(cd web && npm run typecheck > /tmp/w2-t6-web.log 2>&1 && npm run test:unit >> /tmp/w2-t6-web.log 2>&1; echo "web rc=$?")
```

Expected: `adr-refs: 10 ADRs, gap-free, every reference resolves` and `adr-refs rc=0`, no `FAIL .github/tests/…` line (the docs-gates link check stays green: the only links this task changes are the `docs/README.md` ADR bullets, which the rename and Step 5 rewrite), `versions rc=0`, `rc=0` and `web rc=0`. `internal/provisioning` and `internal/store` start Postgres through Docker; a skip is not a pass.

- [ ] **Step 7: Commit**

```bash
git add -A -- . ':(exclude)docs/superpowers'
git status --short | grep -v '^[MADR] ' ; echo "(no unstaged or untracked lines expected outside docs/superpowers)"
git commit -m "docs(adr): renumber the ADRs without gaps and replace 0002 with API conventions

0008 (KasmVNC brute force) moves into the threat model with the current
port facts; 0009/0010/0011 become 0008/0009/0010 with every reference
updated; adr-refs.test.sh keeps references resolvable.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: ADRs 0001 and 0003–0007 rewritten as current state (C3) — tier: integrate

**Files:**
- Modify: `docs/adr/0001-runtime-streaming.md` (67 lines, Vietnamese; rewrite it in English), `0003-gateway-broker-internal-api.md` (163), `0004-launch-origin-policy.md` (130), `0005-backend-frontend-operator.md` (344), `0006-rate-limit-state.md` (424), `0007-sign-out-everywhere.md` (211)
- Modify: `docs/README.md` (one-line descriptions of these ADRs)

**Interfaces:**
- Consumes: Task 6's numbers. ADR 0002 is API conventions, 0009 is data transfer and 0010 is audit.
- Produces: six ADRs, link-check clean and version-gate clean, that keep `adr-refs.test.sh` green (Task 6 already set their H1 and status lines; keep lines 1–3 exactly as Task 6 wrote them).

Rules for every ADR in Tasks 7 and 8:
- The first line is `# ADR NNNN — <Title>`. Line 3 is `Status: Accepted`. There is no date line.
- Use the sections `## Context`, `## Decision`, `## Consequences` and optionally `## Alternatives considered`. Remove `Amendments`, `Addendum`, `Implementation notes (as merged …)`, `Rollout` and `Compatibility`. Fold whatever is still true into Decision, as present-tense facts.
- Remove every release name, rc, PR number, review tag narrative ("SR-3-F5", "FX-R26", "RL-1 (#88)") and "previously/since/no longer" sentence. A review tag may stay as a bare parenthetical only if a code comment cites it.
- Every number, flag, default and path is checked with the command listed under the ADR.
- Name schema objects (tables, indexes, constraints) from `internal/store/migrations/0001_baseline.sql`, never numbered migration files such as `021_rate_limit_window` or `023_principal_revocation_indexes`: the schema is one baseline file. Check: `grep -nE '\b0[0-9]{2}_[a-z_]+' docs/adr/*.md` prints nothing.
- Every Go test an ADR names must exist. Check: `for n in $(grep -ohE '\bTest[A-Z][A-Za-z0-9_]+' docs/adr/*.md | sort -u); do git grep -qE "func $n\b" -- '*.go' || echo "missing: $n"; done` prints nothing. At `b41be7d` it prints `missing: TestMigration023_Idempotent` (ADR 0007's implementation notes); the rewrite cites `TestBaseline_MigrateIsIdempotent` (`internal/store/baseline_test.go:41`) instead or drops the reference.
- Code comments cite labels inside ADRs. Keep every label listed under an ADR's step as a visible label in the rewrite (a bold lead-in or a heading), so those comments still resolve. List the citations with `git grep -nE 'ADR 000N[ ,(]+[A-Za-z(§]' -- ':!docs/superpowers/**'` for the ADR you are writing.

- [ ] **Step 1: ADR 0001 — Runtime streaming (translate and update)**

Check: `rg -n 'KasmVNC|Guacamole|Windows' docs/adr/0001-runtime-streaming.md | head -20; sed -n 62,65p internal/broker/tickets.go; sed -n 14,22p internal/broker/leases.go; ls build/; grep -n '^## Runtime contract' docs/images.md`. Expected: `TicketTTL = 60 * time.Second`, `LeaseTTL = 30 * time.Second` (`tickets.go:63,65`); the lease fence `(WorkspaceUID, RuntimeGeneration, RuntimeUID)` (`leases.go:15-21`); runtime images `linux-base`, `linux-desktop`, `browser`, `kasm-adapter` under `build/`.

Required content:
- **Context:** browser access to Linux desktops and single-app browsers.
- **Decision:** KasmVNC (websocket, TLS) in the workspace pod behind the authenticating session gateway. A single-use 60 s launch ticket is redeemed by a POST to the session host (ADR 0004). A connection lease is fenced by `runtimeGeneration` and `runtimeUID` (ADR 0003). The pod's `:8443` is served by `tcdi-xferd` in front of KasmVNC on loopback (ADR 0009).
- **Windows:** the RDP/Guacamole path is not implemented in v1.0.0. Say this as a fact with no roadmap language.
- **Contracts the runtime image must meet:** link `docs/images.md#runtime-contract`.
- **Leases:** a redeemed ticket becomes a connection lease with a 30 s sliding TTL that the gateway renews (`LeaseTTL`, `tickets.go:65`).
- **Consequences** and **Alternatives considered:** translate the existing Vietnamese sections (Hệ quả, Alternatives đã xem xét). Keep the substance and drop any dated "verified on" notes.
- Delete the `> **English note:**` blockquote, the `*(Ghi chú: …)*` evidence note, the `## Contracts cần chứng minh` evidence table (its PROVEN/STATIC/VENDOR states are history; the link to `docs/images.md#runtime-contract` replaces it) and `## Nguồn`. Keep a `## References` list only for external vendor documentation links that the old `## Nguồn` cites.

- [ ] **Step 2: ADR 0003 — Internal gateway↔broker API (mTLS)**

Check:

```bash
rg -n '"internal-listen"|"operator-cn"|"internal-revoked-list"|"broker-url"' internal/backend/config.go
sed -n 104,126p internal/broker/httpapi/server.go
sed -n 145,172p internal/broker/httpapi/server.go
sed -n 431,450p internal/broker/httpapi/server.go
sed -n 48,52p internal/broker/httpapi/server.go
rg -n 'type LocalGateway' internal/broker/gatewayshim.go
sed -n 409p deploy/helm/tinycdi/templates/_helpers.tpl
```

Required content:
- **Transport and identity:** the internal listener (`-internal-listen`, default `:9443`, `config.go:343`) serves TLS with client certificates verified against the internal CA (`VerifyClientCertIfGiven`; a request without a certificate gets `401`). The identity is the certificate CN; a SPIFFE URI SAN, if present, must equal the CN (`server.go:145-172`). The operator identity is the CN `-operator-cn` (default `operator`, `server.go:48-52`, `config.go:348`). Per-certificate revocation is ADR 0008.
- **Routes** (`server.go:117-125`), two families:
  - lease-scoped gateway routes, refused to the operator identity (`requireGateway`, `server.go:431-440`): `POST /internal/v1/broker/redeem`, `POST …/leases/{id}/renew`, `GET …/leases/{id}/target`, `POST …/leases/{id}/revoke`, `POST …/leases/{id}/activity`, `POST …/leases/{id}/transfers/{tid}/claim`, `POST …/leases/{id}/transfers/{tid}/complete`; give each one line of purpose;
  - workspace-scoped operator routes, admitted only for the operator CN (`requireOperator`, `server.go:442-450`): `POST …/workspaces/{uid}/revoke`, `GET …/workspaces/{uid}/drain`.
- **Deployment:** in the chart's (only) mode the session gateway runs inside the backend process and calls the broker in process through `broker.LocalGateway` (`internal/broker/gatewayshim.go`), so the operator is the only remote client. A backend started with `-broker-url` (split/test mode, `config.go:373`) is a remote gateway client of the lease routes; the chart refuses `broker-url` in `backend.extraArgs` (`_helpers.tpl:409`). Do not write "operator routes only": the listener mounts both families.
- Fold the "Addendum: session activity + operator revocation" (activity and operator revocation routes, identity split, semantics) into Decision. Drop the "v0.2 update" blockquote (the `> **v0.2 update:**` block right after the status line). There is no CN allowlist: one operator CN, and any other CN the CA signed is a gateway identity.

- [ ] **Step 3: ADR 0004 — Launch POST origin policy**

Check: `rg -n 'portal-origin|launchOriginOK|Sec-Fetch|fetch-metadata|ticket_in_query|owner_tab_required' internal/gateway/launch.go internal/backend/config.go | head -20`. Expected: `launchOriginOK` (`launch.go:152`), the `Sec-Fetch-Site` switch (`launch.go:268-290`), `ticket_in_query` (`launch.go:294-295`), `-portal-origin` (`config.go:359`), and Task 1's `owner_tab_required` check.

Required content: `POST /v1/launch` on the session host accepts the configured portal origins (`-portal-origin`) or the workspace's own origin. It applies the fetch-metadata gate. It refuses a ticket in the query (`403 ticket_in_query`), and it refuses a request without a valid `tcdi_tab` with `400 owner_tab_required` before the ticket is redeemed (Task 1). State both as plain facts. Fold "## Amendments (2026-10-01 security fixes)" into Decision as current rules, and drop the whole `> **v0.2 update:**` blockquote after the status line (it holds the "premise is superseded" sentence). Keep the cookie modes (`lax`, `partitioned`; `-session-cookie-mode`, `config.go` session listener block) as a fact in Decision.

- [ ] **Step 4: ADR 0005 — Backend, frontend, operator and per-workspace session hosts**

Check:

```bash
sed -n 12,30p docs/images.md
rg -n '"-listen"|session-listen|internal-listen|metrics-listen' internal/backend/config.go | head
rg -n 'endpointReady|EndpointSlice' internal/runtime/linux/backend.go | head -5
rg -n 'UpstreamDialBudget' internal/gateway/dial.go
sed -n 10,16p web/src/session/coldConnect.ts
sed -n 9,12p deploy/helm/tinycdi/templates/backend.yaml; sed -n 214p deploy/helm/tinycdi/templates/backend.yaml
sed -n 175,177p deploy/helm/tinycdi/templates/_helpers.tpl; sed -n 76p deploy/helm/tinycdi/templates/httproute.yaml
```

Required content, rewritten as current architecture (currently 19 history lines):
- **Components:** `backend` (public API on `:8443`, `config.go:273`; session listener on `:8444`, `config.go:319`; internal mTLS on `:9443`, `config.go:343`; metrics on `:9090` only when `backend.metrics.enabled`, `templates/backend.yaml:11,214`, because `-metrics-listen` defaults to empty, `config.go:354`), `frontend` (static SPA server) and `operator`.
- **Hosts:** every workspace is served on `<label>.<sessionDomain>` through one wildcard edge route, and `portalHost` must be disjoint from `sessionDomain`.
- **State:** the broker lives in process, and Postgres is the only state store.
- **Readiness:** a workspace is Ready only once a ready EndpointSlice endpoint backs the current pod (`endpointReady`, `internal/runtime/linux/backend.go:595-600`). The gateway retries a refused upstream dial within a 15 s budget (`UpstreamDialBudget`, `internal/gateway/dial.go:11`). On a cold first connect the portal re-mints a launch ticket with 1 s, 2 s, 4 s backoff within a 60 s total budget (`COLD_BACKOFF_MS`, `COLD_BUDGET_MS`, `web/src/session/coldConnect.ts:13-16`).
- Drop every "v0.1/v0.2" narrative line and the `Supersedes:` paragraph after the status line (Task 6 already removed the status paragraph with "accepted ahead of independent review").
- Drop the closing bullets about `docs/runbooks/upgrade.md` and "the security reviewer rewrites them at the G2 gate" (the last lines of the file).
- Drop "Migration `011_lease_session.sql` adds:" (rules above). State the columns as columns of `connection_lease` in the baseline (`0001_baseline.sql:57-59` after Task 1): `session_digest`, `stream_epoch`, and `stream_owner_tab` (32 lowercase hex, the tab that owns the stream; `stream_epoch = 0` exactly when it is NULL — Task 1's CHECK constraints). Do not mention `stream_owner_epoch`; Task 1 drops it.
- Keep the content-security-policy table (it carries `ADR 0009 §5` after Task 6).

- [ ] **Step 5: ADR 0006 — Rate-limiter state placement**

Check:

```bash
rg -n '"(login|launch|session-lookup)-rate"|"rate-limit-replicas"|"trusted-proxies"' internal/backend/config.go
sed -n 315,316p internal/backend/config.go; sed -n 333,336p internal/backend/config.go
sed -n 84,85p internal/gateway/proxy.go
rg -n 'loginRateBurst|launchRateBurst|windowLimit|cfg.LaunchRate\+' internal/backend/ratelimit.go | head
rg -n '^func ' internal/ratelimit/shared.go | head; sed -n 176,180p internal/store/migrations/0001_baseline.sql
git grep -nE 'ADR 0006[ ,(+]|RL-CEILING|RL-DEFAULT' -- ':!docs/superpowers/**' ':!docs/adr/**'
```

Required content:
- **Decision:** option B, Postgres-backed shared fixed-minute windows (table `rate_limit_window`, `0001_baseline.sql:176`; one `INSERT … ON CONFLICT` per check, `internal/store/rate_limit.go`; `SharedLimiter`, `internal/ratelimit/shared.go:112-251`). The shared window bound is rate + burst per minute (`internal/backend/ratelimit.go:96,106`).
- Keep these labels, which code comments cite (`internal/backend/ratelimit.go:33,43,87`, `internal/gateway/proxy.go:172`, `internal/ratelimit/shared.go:48,57`, `internal/store/rate_limit.go:19`, `internal/backend/config.go:201`, `internal/backend/wire_test.go:36,132`, `tests/integration/rate_limit_pg_test.go:8`): **option B**; **G1** fail-open with a floor (a store error falls back to the divided local limiter); **G2** split mode stays local-only (a `-broker-url` gateway has no Postgres handle); **(i)** effective bound (healthy: the shared window is the aggregate bound and each pod's local bucket is the undivided rate+burst, a store-protection prefilter; degraded: the divided bucket decides); **(ii)** one upsert per check, leader-only cleanup; **RL-CEILING** (the healthy-mode local ceiling is undivided) and **RL-DEFAULT** (the default budgets below). State each as the current rule, not as an amendment.
- A table of flag, default and meaning: `-login-rate` 60/min, burst 20, on `/v1/login`, `/v1/auth/callback` and `GET /v1/session` (`config.go:315-316`); `-launch-rate` 120/min, burst 40, on `/v1/launch` (`config.go:333-334`); `-session-lookup-rate` 300/min, burst 120, a local per-replica bucket for unknown session cookies (`config.go:335-336`, `gateway/proxy.go:84-85`); `-rate-limit-replicas` 1, the divisor of the degraded-mode floor (`config.go:369`); `-trusted-proxies`, the prefixes whose forwarding headers set the client key (`config.go:367`). Budgets are aggregate across replicas while Postgres is healthy.
- Condense Options A, B and C, the inventory table "(main @ 93178cc)" and the comparison (about 60% of the file) into `## Alternatives considered`, three to six lines for A and C. B is the decision.
- Remove the advisor/date line ("The advisor picked option B (2026-10-05)"), v0.3/v0.4 history, PR numbers and the "B6 gate" wording; name the test `tests/integration/rate_limit_pg_test.go` instead.

- [ ] **Step 6: ADR 0007 — Sign out everywhere**

Check:

```bash
rg -n 'revoke-all|RevokeAll' internal/api/openapi.yaml internal/api/revokeall.go internal/api/adminsessions.go | head
sed -n 2761,2763p internal/api/openapi.yaml
sed -n 69p internal/store/migrations/0001_baseline.sql; sed -n 106p internal/store/migrations/0001_baseline.sql
rg -n 'routeSessionRevokeAll|routeAdminUserRevokeAll' internal/api/auditroutes.go
```

Required content: `POST /v1/me/sessions:revoke-all` ends every portal session, lease and unconsumed ticket of the calling principal, the caller's own session included. The tenant-admin variant `POST /v1/admin/tenants/{tenant}/users/{ownerRef}/sessions:revoke-all` does the same for a named user in the admin's tenant. The admin session list shows `leaseRef` (16 hex characters, a SHA-256 prefix of the lease id; `openapi.yaml:2763`), never lease ids. Both routes are audited (`auditroutes.go:69,95`). Replace "## Implementation notes (as merged in #117)" with `## Decision` subsections describing the code as it is. Replace "### Index migration" with "### Indexes", naming `connection_lease_principal_active` and `launch_ticket_principal_outstanding` (`0001_baseline.sql:69,106`) without migration numbers. Remove the v0.4/v0.5 context sentences ("v0.4 (#107, threat-model S17) made…", "is the v0.5 SIGNOUT-ALL task") and the `F5` slate tag; keep `S17`/`S19` only where Task 13 will renumber them (they are threat-model ids).

- [ ] **Step 7: Update the index lines and run the gates**

In `docs/README.md`, the bullets for 0001 and 0003–0007 each get one line of what the ADR decides, with no status words like "(proposed)" or "(accepted)" and no versions.

```bash
for f in docs/adr/000{1,3,4,5,6,7}-*.md; do head -n 3 "$f"; done
bash .github/scripts/check-doc-versions.sh docs/adr/000{1,3,4,5,6,7}-*.md docs/README.md; echo "versions rc=$?"
python3 .github/scripts/check-doc-links.py docs/adr/000{1,3,4,5,6,7}-*.md docs/README.md; echo "links rc=$?"
bash .github/tests/adr-refs.test.sh; echo "adr-refs rc=$?"
bash .github/tests/docs-gates.test.sh > /tmp/w2-t7-gates.log 2>&1; echo "docs-gates rc=$?"
grep -nP '[\x{0100}-\x{1EFF}]' docs/adr/0001-runtime-streaming.md | head -3; echo "(no Vietnamese diacritics expected)"
grep -nE '\b0[0-9]{2}_[a-z_]+' docs/adr/000{1,3,4,5,6,7}-*.md; echo "(no migration file names expected)"
for l in 'option B' 'G1' 'G2' 'RL-CEILING' 'RL-DEFAULT'; do grep -q -- "$l" docs/adr/0006-rate-limit-state.md || echo "missing label $l in ADR 0006"; done
for n in $(grep -ohE '\bTest[A-Z][A-Za-z0-9_]+' docs/adr/*.md | sort -u); do git grep -qE "func $n\b" -- '*.go' || echo "missing: $n"; done
```

Expected: `versions rc=0`, `links rc=0`, `adr-refs rc=0`, `docs-gates rc=0`, and no diacritics, migration-name, missing-label or `missing:` test lines (ADRs 0008–0010 are not rewritten yet; if a `missing:` line names a test cited only by them, Task 8 fixes it).

- [ ] **Step 8: Commit**

```bash
git add -- docs/adr docs/README.md
git commit -m "docs(adr): rewrite ADRs 0001 and 0003-0007 as the current design

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: ADRs 0008–0010 rewritten as current state, carrying the W1 design rulings (C3) — tier: integrate

**Files:**
- Modify: `docs/adr/0008-mtls-cert-revocation.md` (340 lines), `docs/adr/0009-session-data-transfer-policy.md` (509), `docs/adr/0010-audit-store-and-retention.md` (63), `docs/README.md`
- No code changes: the section numbers below are the ones the code already cites, so no `ADR 0009 §N` reference moves.

**Interfaces:**
- Consumes: Task 6's numbers and the `ADR 0009 §N` references in code after Task 6 (about 54: `§1` 9, `§2` 16, `§3` 5, `§4` 2, `§5` 22 of which 7 are `§5 (i)`; `git grep -nE 'ADR 0009 ?§' -- ':!docs/superpowers/**' ':!docs/adr/**'`, plus the wrapped `internal/provisioning/sessionpolicy_internal_test.go:13-14`).
- Produces: ADR 0009 with exactly these `## Decision` subsections, numbered as old ADR 0010 numbered them, so every existing code reference stays true:
  - `### §1 Policy model: a tenant ceiling` (code cites it for the lattice and the meet);
  - `### §2 Resolution: one function, recorded result` (cited for the recorded policy on the ticket, the CR and the outbox intent);
  - `### §3 When a change takes effect` (cited for the two clocks and `:apply`);
  - `### §4 Admin API` (cited by the admin handler and its wiring);
  - `### §5 File transfer`, containing `#### (i) In-pod transfer helper` and `#### Trust model` (cited as `§5` and `§5 (i)`).
  Task 9 and Task 13 cite ADR 0009 §5 for the in-pod trust model; Task 12 checks the runbook against §5 and Task 14 maps `:apply` to §3.

- [ ] **Step 1: ADR 0008 — Per-certificate revocation on the internal mTLS API**

Check:

```bash
sed -n 350,351p internal/backend/config.go
sed -n 495,520p internal/backend/wire.go
sed -n 120,135p internal/backend/listeners.go
rg -n '^func ' internal/tlsreload/blocklist.go | head; sed -n 74,90p internal/tlsreload/blocklist.go
sed -n 296,306p deploy/helm/tinycdi/values.yaml; sed -n 540,544p deploy/helm/tinycdi/values.yaml
rg -n 'revocation|revoked' internal/observability/metrics.go | head -5
ls docs/runbooks/mtls-cert-revocation.md
rg -n 'func Test' internal/tlsreload/blocklist_test.go internal/backend/mtls_revocation_test.go
```

Required content:
- **Decision:**
  - `-internal-revoked-list` (`config.go:350`) names a file of `<serial-hex> <issuer-dn>` lines (blank lines and `#` comments ignored). The backend polls it and swaps it in like the CA pool. A bad file at boot fails startup; a bad reload keeps the last good list (`wire.go:497-519`).
  - The listener's `VerifyConnection` hook (`installRevocation`, `listeners.go:120`) aborts the handshake of a listed (issuer, serial) leaf, and a reload that adds an entry closes the live connections already using it (`Blocklist`, `blocklist.go:74-90`). Unset flag: no hook installed.
  - Chart: `backend.tls.internal.revokedSerials.existingSecret` and `.key` (default `revoked-serials.txt`), mounted read-only at `/etc/internal-revoked/` (`values.yaml:298-306`). Metrics: the aborted-handshake counter, the loaded-entries gauge and the reload-error counter (`internal/observability/metrics.go`, names from the command above).
  - Certificate lifetimes: cert-manager `certManager.duration` (default `2160h`, 90 days) and `renewBefore` (`360h`) (`values.yaml:542-543`). An operator may shorten them; the blocklist covers a stolen key until its certificate expires.
  - Link the procedure: [docs/runbooks/mtls-cert-revocation.md](../runbooks/mtls-cert-revocation.md). Replace `## Operator runbook — revoke one certificate` with that link and a one-line summary. Fold `## Chart values (additive)` into the chart bullet above.
- **Consequences:** include, as a stated limitation, that the internal mTLS **clients** (the operator's broker client, `internal/broker/httpapi/opclient/client.go:120-130`) verify the server chain but do not check serials against the list. Task 9 copies this into the release notes.
- **Alternatives considered:** a CRL (rejected: the chart's cert-manager `ca`/`selfSigned` issuers publish no CRL and set no distribution point, so a CRL needs a new signing and publishing subsystem); OCSP (rejected for the same reason: the shipped PKI runs no responder). One line each.
- Keep the label `SR-3-F5` once, as a bare parenthetical after the title sentence of Context: code cites `ADR 0008 (SR-3-F5)` (`internal/backend/wire.go:497`, `deploy/helm/chart_revocation_test.go`, `values.yaml:298-299`).
- Delete `## Compatibility`, `## Rollout` and the v1.0/v1.1 narrative ("held at v1.0", "target v1.1", "Implement in **v1.1**", the `v1.1.0-rc.1` line). `## Test plan` becomes a short `### Tests` list at the end of `## Decision`, naming the existing tests from the last command.

- [ ] **Step 2: ADR 0009 — Session data-transfer policy (clipboard and file transfer)**

Check every fact below before writing it:

```bash
sed -n 50,62p internal/provisioning/sessionpolicy.go; rg -n '^func (meet|meetCeiling|resolveSessionPolicyTx)' internal/provisioning/sessionpolicy.go
sed -n 14,18p internal/store/sessionpolicy.go
rg -n '"file-transfer-max-bytes"' internal/backend/config.go; sed -n 376,379p deploy/helm/tinycdi/values.yaml
sed -n 12,21p internal/store/filetransfer.go
sed -n 60,86p internal/broker/transfers.go
sed -n 68,72p internal/api/filetransfers.go; sed -n 86,100p internal/api/filetransfers.go; sed -n 144,150p internal/api/filetransfers.go
sed -n 22,33p internal/gateway/transfer.go; sed -n 163,170p internal/gateway/transfer.go; rg -n 'func TestTransfer_DesktopProxyStillRefusesXfer' internal/gateway
sed -n 28,36p internal/xferd/xferd.go; sed -n 63,110p internal/xferd/xferd.go; sed -n 125,150p internal/xferd/xferd.go; sed -n 195,200p internal/xferd/xferd.go
sed -n 17,20p internal/api/adminsessionpolicyapply.go; sed -n 40,58p internal/api/adminsessionpolicyapply.go; sed -n 86,104p internal/api/adminsessionpolicyapply.go
rg -n '"xfer-token"|secretMode :=|FSGroup|RunAsUser|envClipboardPolicy =' internal/runtime/linux/backend.go | head
sed -n 160,180p build/linux-base/entrypoint.sh
sed -n 55,90p web/src/session/transfer.ts
git grep -nE 'ADR 0009 ?§' -- ':!docs/superpowers/**' ':!docs/adr/**'
```

Required structure and facts (old ADR 0010's sections keep their numbers; rewrite their content as current state):
- `## Context`: copying data in and out of a remote desktop is a data-loss channel. Tenants need a ceiling that templates cannot exceed.
- `## Decision`:
  - `### §1 Policy model: a tenant ceiling`: two channels, clipboard and file transfer, each a direction lattice (Disabled bottom, Bidirectional top, the two single directions incomparable; clipboard `Send`/`Receive`, file transfer `Upload`/`Download`). The effective policy is the meet of the template's own policy and the tenant ceiling: the ceiling only narrows, never widens. Reproduce the table at `sessionpolicy.go:58-59`. A missing ceiling row, or a NULL channel, means FollowTemplate (`PolicyFollowTemplate`, `sessionpolicy.go:19-23`; `internal/store/sessionpolicy.go:16`). The ceiling's `maxFileBytes` may only lower the platform bound `-file-transfer-max-bytes` (`config.go:304`; chart `backend.fileTransfer.maxFileBytes`, default `104857600`, `values.yaml:379`).
  - `### §2 Resolution: one function, recorded result`: one resolver (`resolveSessionPolicyTx`, `sessionpolicy.go:126`) computes the policy; consumers never re-implement the lattice. The result is recorded on the launch ticket, on the outbox intent and the Workspace CR's `spec.sessionPolicy`, and on each file-transfer row; the runtime and audit read the record and never re-derive it.
  - `### §3 When a change takes effect`: new tickets get a change immediately. A running workspace's server-side enforcement (the pod env `TCDI_CLIPBOARD_POLICY`, `backend.go:177`, and the helper's direction and cap) changes at its next start; until then the client flags restrain an unmodified client only. `POST /v1/admin/tenants/{tenant}/session-policy:apply` restarts now the running workspaces that are looser than the ceiling: the body is `{"dryRun": bool, "includeActive"?: bool}` with `dryRun` required (`adminsessionpolicyapply.go:40-44,90`); workspaces with an active session are skipped unless `includeActive` is true; at most 500 affected workspaces per call, with `truncated` when more qualify (`maxPolicyApply`, `adminsessionpolicyapply.go:17-20`); it requires `If-Match` (ADR 0002). Delete the old sentence "v1 adds no 'restart affected workspaces' action".
  - `### §4 Admin API`: `GET`/`PUT /v1/admin/tenants/{tenant}/session-policy` (tenant-admin, strong ETag and `If-Match` per ADR 0002, audited per ADR 0010) and `:apply` (§3). Every ceiling row is API-managed; there is no config declaration (`internal/api/adminsessionpolicy.go:95-97`).
  - `### §5 File transfer`, then `#### (i) In-pod transfer helper`, then `#### Trust model`. Each fact is verified by the commands above:
    - one file at a time, by name, in the flat folder `~/transfers` (`xferd.go:34`); no listing endpoint;
    - a name of 1..255 bytes of valid UTF-8 with no `/`, `\`, `;`, `%`, `..`, NUL or control character, and not `.`, is required at mint (`validTransferName`, `filetransfers.go:86-100`) and again in the helper (`xferd.go:85-90`);
    - only the workspace owner mints, not a tenant admin (`filetransfers.go:146-148`); the mint route sits behind the session-keyed rate limit that sign-out-everywhere uses (`filetransfers.go:68-72`);
    - a minted row waits at most 2 minutes for its claim, a claimed row counts as a live stream for at most 30 minutes, and finished rows are kept 7 days for audit joins (`FileTransferTTL`, `FileTransferClaimDeadAfter`, `FileTransferKeep`, `store/filetransfer.go:15-20`);
    - the broker's `ClaimTransfer` (`transfers.go:60-86`) is the authoritative check: the lease is live and bound to the current incarnation; the grant belongs to the lease's workspace, principal and incarnation; the method matches the direction; no other transfer stream is live on the workspace; the policy in force now still allows the direction and the declared size; success and refusals are audited in the same transaction (fail closed);
    - the gateway serves `/v1/transfers/{id}/content` on the workspace's session host (`transferPathRE`, `gateway/transfer.go:22-32`) and builds `/xfer/files/<name>` itself (`transfer.go:163-168`); the generic desktop proxy answers `404` for `/xfer` and KasmVNC's own upload/download paths (`TestTransfer_DesktopProxyStillRefusesXfer`, `transfer_test.go:228`);
    - `(i)`: `tcdi-xferd` owns the pod's only exposed port `:8443` and reverse-proxies the desktop to KasmVNC on `127.0.0.1:8444`; it checks `X-Tcdi-Xfer-Token` in constant time against the Secret key `xfer-token` (`xferd.go:28,146`; `backend.go:810`); it enforces the size cap (`xferd.go:198`) and an `os.Root` jail on `~/transfers` (`xferd.go:125-140`); it answers a desktop request without Basic credentials with its own `401` (`xferd.go:102-109`); it passes the gateway's forwarding headers through (`xferd.go:63-75`);
    - downloads use a credentialed CORS `fetch` and then a blob with `<a download>` (`web/src/session/transfer.ts:55-90`); the helper refuses a file larger than the cap.
  - `#### Trust model`, stated plainly: "`xfer-token` is readable by the workspace user (the Secret is mounted 0440 with fsGroup 1000, and the desktop runs as uid 1000; `backend.go:1103,1237,1239`), and `tcdi-xferd` runs as that same user. A workspace user can therefore read the token and could replace the helper on `:8443`. The token only proves 'the gateway or a process in this pod'; the broker claim, which a pod cannot mint or forge, is what authorizes a transfer, and the jail and cap protect the user from the gateway, not the platform from the user."
- `## Consequences`: the two clocks of §3; a looser server-side policy persists on a running workspace until restart or `:apply`; tab memory holds a download up to the cap; the trust model above.
- `## Alternatives considered`, one line each with its reason: the operator-mediated stream (`pods/exec` or a sidecar; old `#### (ii)`), KasmVNC's built-in upload/download (needs an owner kasmpasswd entry that also opens port relay), client-side-only transfer (no channel; a hint is not enforcement), a second pod port for the helper (the NetworkPolicy admits one port; the helper owns `:8443` instead), and a top-level download navigation (it would miss a `partitioned` session cookie).
- Delete `## Compatibility` and `## Implementation plan` (`### T10 …`, `### T11 …`) entirely, the `#### Threat analysis of (i)` subsection (Task 13 owns threats; keep any still-true control as a bullet in §5), and every v1.2/v1.3 mention ("v1.2 clipboard fix", "available in v1.3"). Check: `grep -nE '\bT1[01]\b|v1\.[23]' docs/adr/0009-session-data-transfer-policy.md` prints nothing.

Afterwards, run `git grep -nE 'ADR 0009 ?§[0-9]' -- ':!docs/superpowers/**' ':!docs/adr/**'` and check each reference against the heading it names: lattice/meet/ceiling-narrows → §1; recorded policy on ticket, CR, intent or pod → §2; tightening clocks or `:apply` → §3; admin handler → §4; transfer route, helper, `:8443`, `/xfer/` → §5 or §5 (i). The planner checked all of them at `b41be7d` against old ADR 0010's headings, which these keep. Fix any reference that does not match.

- [ ] **Step 3: ADR 0010 — Audit store and retention**

Check:

```bash
sed -n 302,303p internal/backend/config.go; sed -n 446,447p internal/backend/config.go; sed -n 374p deploy/helm/tinycdi/values.yaml
sed -n 94,97p internal/backend/retention.go
sed -n 36,38p internal/api/auditstore.go; sed -n 16,18p internal/api/auditroutes.go; rg -n 'admin.audit.get' internal/api/auditroutes.go
```

Expected: `-audit-retention-days` default 90, valid 1..3650 (`config.go:302,446-447`); chart `backend.retention.auditDays: 90`; daily prune in batches of 1000 (`retention.go:95-96`); `notPersisted` holds only `session.touch` (`auditstore.go:36-38`); `http.request` is the per-request record next to the domain events (`auditroutes.go:16-17`).

Required content: Task 6 already set the H1 and `Status: Accepted` and removed the status paragraph and date line. Keep the Decision, Alternatives and Consequences text, which matches the code above. Rewrite `## Context` in the present tense, without "were"/"could not"/"was swallowed": the stdout JSONL stream (`observability.JSONSink` behind `GuardedSink`) cannot be read in the product and its sink drops write failures by design, so administrators need a durable store that fails closed. Make sure Consequences states which events are stdout-only: the gateway-local launch and stream events, `session.touch` and the per-request `http.request` record. Link `docs/runbooks/audit.md` (created in Task 12). Until Task 12 lands, write the path in backticks rather than as a link, so the link check stays green. Task 12 turns it into a link.

In `docs/README.md` `## ADRs`, the bullets for 0008, 0009 and 0010 each get one line of what the ADR decides, with no status words ("proposed", "accepted"), no review tags (`SR-3-F5`) and no versions (the 0009 bullet says "v1.3" today): 0008 "per-certificate revocation on the internal mTLS listener: a polled serial blocklist", 0009 "tenant ceilings for clipboard and file transfer, and the in-pod transfer helper", 0010 "the durable audit store, written in the mutation's transaction, with pseudonymous actors and 90-day retention".

- [ ] **Step 4: Run and commit**

```bash
bash .github/tests/adr-refs.test.sh; echo "adr-refs rc=$?"
bash .github/scripts/check-doc-versions.sh docs/adr/*.md docs/README.md; echo "versions rc=$?"
python3 .github/scripts/check-doc-links.py docs/adr/*.md docs/README.md; echo "links rc=$?"
bash .github/tests/docs-gates.test.sh > /tmp/w2-t8-gates.log 2>&1; echo "docs-gates rc=$?"
rg -n '\bT1[01]\b|Proposed|Amendment|Addendum|Rollout|as merged|Trạng thái' docs/adr/ ; echo "(no lines expected)"
for n in $(grep -ohE '\bTest[A-Z][A-Za-z0-9_]+' docs/adr/*.md | sort -u); do git grep -qE "func $n\b" -- '*.go' || echo "missing: $n"; done
git add -- docs/adr docs/README.md
git commit -m "docs(adr): rewrite ADRs 0008-0010 as the current design

ADR 0009 now records the file-transfer constants, the dedicated gateway
leg, the in-pod helper and its trust model, and session-policy:apply.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

Expected: `adr-refs rc=0` (all ten ADRs pass the H1, status and reference checks), `versions rc=0`, `links rc=0`, `docs-gates rc=0`, and no pattern or `missing:` lines. This task edits only `docs/adr/` and `docs/README.md`.

---

### Task 9: Release notes for v1.0.0, the versioning contract and the security policy (C2) — tier: design

**Files:**
- Replace: `docs/releases/v1.0.0.md` (same path; ruling 13)
- Rewrite: `docs/versioning.md` (46 lines; it also absorbs the generic 1.x upgrade procedure from the deleted `docs/runbooks/upgrade.md`, Step 4), `SECURITY.md` (`## Supported versions`, lines 25-40, every "(v0.2 design)" / version-history phrase in the file — 25 version-gate hits at `b41be7d` — plus the stale facts and the 14 nonexistent test names listed in Step 5)
- Modify: `docs/README.md` (re-add a `## Releases` section with exactly one bullet)

**Interfaces:**
- Consumes: ADR numbers from Task 6 (0003 internal API, 0006 rate limits, 0008 mTLS revocation, 0009 data transfer §5, 0010 audit), the threat model (linked as a file only, because Task 13 rebuilds it), and `docs/runbooks/audit.md`, which Task 12 creates. Until then, refer to it in backticks, not as a link.
- Constraint from Task 12: Task 12 keeps the headings `### Log integrity — what TinyCDI guarantees vs what you must provide` (observability.md) and `## Sign-in rate limits and NAT` (capacity.md) unchanged, so the fragments used in Step 3 stay valid.
- Produces: the v1.0.0 release notes that R5/R6 publish, the 1.x contract text, and the support table. Task 10 links `docs/versioning.md#upgrading-within-1x` from the chart README and install runbook.

- [ ] **Step 1: Check every fact this task states**

```bash
sed -n 83,110p docs/security/provenance.md            # "## Verification side (what a consumer runs)": cosign verify shapes
sed -n 126,131p docs/security/provenance.md           # digest-cleanup check-then-act residual
sed -n 147,155p docs/security/provenance.md           # "## What a signature does not prove": single maintainer
sed -n 66,68p docs/compatibility.md                   # Safari / WebKit out of scope
sed -n 541,548p docs/images.md                        # image known limitations: apt pins
# in-pod trust model (item 1)
sed -n 1103p internal/runtime/linux/backend.go        # secretMode 0o440
sed -n 1142p internal/runtime/linux/backend.go        # Secret mounted read-only at /run/secrets/tcdi in the one "desktop" container
sed -n 1235,1240p internal/runtime/linux/backend.go   # pod runs as uid/gid 1000, fsGroup 1000
sed -n 805,811p internal/runtime/linux/backend.go     # the Secret holds username, password, tls.crt, tls.key, xfer-token
sed -n 21,30p cmd/tcdi-xferd/main.go                  # the helper reads xfer-token from the same mount
sed -n 186,191p build/linux-base/entrypoint.sh        # the entrypoint polls both PIDs every 2 s and exits when either dies
sed -n 75,86p internal/broker/transfers.go            # what ClaimTransfer checks
sed -n 117,124p internal/gateway/transfer.go          # gateway claims before it touches the pod
sed -n 194,214p internal/gateway/transfer.go; sed -n 259,276p internal/gateway/transfer.go   # gateway caps both directions at grant.MaxBytes
sed -n 672,690p internal/api/openapi.yaml             # only the owner mints a grant; TTL 2 minutes
# the other items
grep -n 'sessionStreamCap' deploy/helm/tinycdi/values.yaml   # item 3: default 32
sed -n 627p docs/security/threat-model.md | cut -c1-200      # item 3: the KasmVNC fd row (old S39)
sed -n 499,515p internal/backend/wire.go; grep -n 'NewBlocklist' -r internal --include='*.go' | grep -v _test   # item 4: only the backend's :9443 listener checks serials
sed -n 347,352p internal/broker/activity.go           # item 5: one workspace_revocation row per revoked runtime generation
grep -rnE 'DELETE\s+FROM\s+workspace_revocation' --include='*.go' internal; echo "(no output expected: the table is never pruned)"
sed -n 29,45p internal/api/auditstore.go              # item 6: notPersisted + plain WriteAudit callers stay stdout-only
grep -n 'g.audit(r, "' internal/gateway/*.go | grep -v _test | cut -c1-120 | head   # item 6: gateway events go to stdout only
sed -n 175,190p internal/ratelimit/ratelimit.go; sed -n 258,261p internal/ratelimit/ratelimit.go   # item 7: key = client IP, IPv6 folded to /64
sed -n 1601,1620p internal/runtime/linux/backend.go   # item 8: DNS egress rule on every profile
sed -n 410,426p internal/operator/workspace_controller.go; sed -n 30,47p api/v1/failure.go   # item 9
sed -n 1370,1380p internal/api/openapi.yaml; sed -n 1443,1450p internal/api/openapi.yaml       # item 10
grep -n 'GroupVersion = ' api/v1/groupversion_info.go
grep -nE '^version:|^appVersion:' deploy/helm/tinycdi/Chart.yaml
ls hack/quickstart/up.sh
```

If a command contradicts a sentence below, write what the code says instead and mention the correction in the commit body.

- [ ] **Step 2: Write `docs/releases/v1.0.0.md`**

It needs this exact outline. Each bullet is a fact the section must state. There are no PR numbers, no rc names, no internal wave names (W0/W1/F1…) and no "new in"/"previously" sentences: this is the first public release, so everything is simply what the product does.

```markdown
# TinyCDI 1.0.0

TinyCDI is a Kubernetes-native platform for browser-delivered Linux
desktops and single-app browser workspaces. <two more sentences: who runs
it (a platform team on their own cluster) and what a user sees (a portal,
one click to a desktop in a browser tab)>.

## Features
### For users
- Portal: create, start, stop, delete workspaces from tenant templates;
  persistent home volumes; retained data re-attach (`:attach`).
- Desktop in the browser over KasmVNC, behind an authenticating session
  gateway; one host per workspace (`<label>.<sessionDomain>`).
- Clipboard and file transfer within the tenant's ceiling: one file at a
  time into and out of `~/transfers`, up to the platform cap.
- Sign out everywhere.
### For tenant admins
- Overview (capacity and health), workspaces, quota and per-user limits,
  session policy (clipboard and file-transfer ceiling, with `:apply` to
  restart looser running workspaces), templates, retained data, the user
  directory (users who have signed in), active sessions with per-user
  sign-out, and the audit log viewer.
### For platform operators
- One Helm chart (`oci://ghcr.io/tinyorbitvn/charts/tinycdi`), three
  components (backend, frontend, operator), Postgres as the only state
  store, CRDs `workspaces.cdi.tinyorbit.vn/v1`.
- Internal mTLS between operator and backend with per-certificate
  revocation (ADR 0008); restricted egress by default for `ClusterOnly`
  workspaces; optional admission policy; Prometheus metrics, dashboards
  and alerts; audit events persisted with retention (default 90 days).
- Signed images and chart with SBOM attestations; runtime images pinned by
  digest.

## Install
- Requirements: link `docs/compatibility.md`.
- `helm install tinycdi oci://ghcr.io/tinyorbitvn/charts/tinycdi --version 1.0.0 -n tinycdi-system -f my-values.yaml`;
  full procedure `docs/runbooks/install.md`; a local trial
  `hack/quickstart/up.sh` (`docs/quickstart.md`).
- Images: `ghcr.io/tinyorbitvn/tinycdi-<image>@sha256:<digest>`; the chart
  pins digests. Verify with the `cosign verify` command in
  `docs/security/provenance.md` (link
  `../security/provenance.md#verification-side-what-a-consumer-runs`;
  the heading is `## Verification side (what a consumer runs)`).
- This release has no upgrade path from any earlier build: install fresh.

## Known limitations
<the list in Step 3, one bullet each, each ending with where to read more>

## Security
- Reporting: `SECURITY.md`. Design: `docs/security/threat-model.md`.
  <one sentence: TinyCDI is maintained by one person and has had no
  independent security audit.>
```

Replace each `<…>` with the sentences it describes. The image list (`tinycdi-<image>`) stays generic. R5/R6 attach the digest table to the GitHub release; the doc does not carry digests.

Feature-bullet sources (check each before writing): admin pages `web/src/admin/AdminLayout.tsx:10-17` (`/admin`, `/admin/workspaces`, `/admin/quota`, `/admin/session-policy`, `/admin/templates`, `/admin/data`, `/admin/users`, `/admin/audit`); `:apply` `internal/api/openapi.yaml:1231-1250` (dry run first; skips workspaces with a live session unless `includeActive`); live sessions `openapi.yaml:1443-1450`; per-user sign-out `openapi.yaml:1489-1500`; user directory "everyone who has signed in" `openapi.yaml:1370-1380`; audit retention default 90 `deploy/helm/tinycdi/values.yaml:374`; chart templates `admissionpolicy.yaml`, `dashboards.yaml`, `prometheusrule.yaml` under `deploy/helm/tinycdi/templates/` (all default off); ClusterOnly restricted default `deploy/helm/tinycdi/README.md` `#### ClusterOnly egress` and `TestClusterOnlyEgressDefaultRestricted` (`internal/runtime/linux/backend_netpol_test.go:287`); signed images + SBOMs `docs/security/provenance.md:9-35`; the internal mTLS listener is the backend's `:9443` (`deploy/helm/tinycdi/templates/backend.yaml:10`).

Link rules: every reference becomes a relative link from `docs/releases/` (`../compatibility.md`, `../runbooks/install.md`, `../quickstart.md`, `../../SECURITY.md`, `../adr/0008-mtls-cert-revocation.md`, …). Link a file, not a fragment, unless the fragment is named in this task (the threat model and ADR 0009 are rebuilt or renamed by other tasks, so link those files without a fragment). `docs/runbooks/audit.md` does not exist until Task 12: name it in backticks, never as a link.

Wording rule: `.github/tests/failed-digest-cleanup.test.sh` scans `docs/releases/*.md` for `(abort|cancel)[a-z]* (run|workflow|job)|or aborted` and fails on any match, negations included. Never write "cancelled run", "aborted workflow", "cancelled job" or similar; write "a run stopped before cleanup" instead.

- [ ] **Step 3: The known-limitations list (verbatim facts; write each as one bullet of the form `- **<title>.** <text>`)**

Each item gives the facts to state, their sources (checked in Step 1) and the link target. Do not strengthen any claim: no "cannot", "isolated" or "encrypted" beyond what is written here.

1. **In-pod trust (file transfer).** State: the workspace pod runs one container (`desktop`) as uid/gid 1000 with `fsGroup` 1000, and its Secret is mounted read-only at `/run/secrets/tcdi` with mode `0440` (`internal/runtime/linux/backend.go:1103,1142,1235-1240`). So the workspace user can read every key of that Secret, `xfer-token` included (`backend.go:805-811`). `tcdi-xferd` runs as that same user in the same container (`cmd/tcdi-xferd/main.go:21-30`), so the user can also stop it or try to serve the pod's port 8443 with a program of their own; the entrypoint restarts the container when the helper's process exits (`build/linux-base/entrypoint.sh:186-191`, a 2-second poll), which narrows but does not close that window. The in-pod checks (the token, the helper's `TCDI_XFER_POLICY`/`TCDI_XFER_MAX_BYTES` floor) are therefore defense in depth, not the control. The authoritative checks run outside the pod: only the workspace owner can mint a grant, through the API with a session and CSRF token, and a grant expires after 2 minutes (`internal/api/openapi.yaml:672-690`); the gateway claims it through the broker (`Broker.ClaimTransfer`, `internal/broker/transfers.go:75-86`) before it contacts the pod, and the claim checks the live lease, the principal, the runtime incarnation, the direction, one live transfer per workspace and the policy in force at that moment, and writes the audit row in the same transaction; the gateway then caps the bytes it moves in either direction at the grant's size (`internal/gateway/transfer.go:194-214,259-276`). Nothing in the pod's Secret lets it mint or claim a grant. Net effect, in one sentence: a tampered helper can mishandle only that workspace's own files, within the direction and size the claim allowed. Also say "port 8443 of the workspace pod" explicitly, because the backend's app listener is also `:8443`. → ADR 0009 (`../adr/0009-session-data-transfer-policy.md`, file only; name "§5" in the text).
2. **File transfer scope.** One file at a time per workspace (a second claim while one stream is live is refused, `internal/broker/transfers.go:156-165`), by name, in the flat folder `~/transfers`, with no listing (`internal/xferd/xferd.go:30,97-111`). A download is held in the browser tab's memory up to `maxFileBytes`. → `../runbooks/session-policy.md#file-transfer`.
3. **KasmVNC under connection saturation.** State: the gateway caps live upstream connections per workspace per backend replica at `backend.sessionStreamCap` (default 32, `deploy/helm/tinycdi/values.yaml:282`); over the cap it answers `429` and the client retries. The cap is per replica, so N replicas admit N × cap. The image lowers KasmVNC's file-descriptor limit below `FD_SETSIZE`, so overload becomes `EMFILE` refusals (`build/linux-base/entrypoint.sh:26-48`); a direct burst far above the cap can still crash Xvnc and restart the container, and that path is not reachable through the gateway. Source: the threat-model row "Reconnect storm could crash a workspace runtime (KasmVNC fd bound)" (`docs/security/threat-model.md:627` at `b41be7d`, old id `S39`). → `../security/threat-model.md` (file only; Task 13 renumbers the row).
4. **mTLS revocation is server-side only.** The backend's internal mTLS listener (`:9443`) rejects client certificates whose serial is on the blocklist (`internal/backend/wire.go:510`, the only `NewBlocklist` caller). The internal client (the operator) does not check the server certificate's serial. → ADR 0008 (`../adr/0008-mtls-cert-revocation.md`).
5. **Revocation records are not pruned.** Each revoked workspace runtime generation adds one `workspace_revocation` row (`internal/broker/activity.go:347-352`), and no job deletes them (Step 1 grep). The table is small, but it grows. → ADR 0003 (`../adr/0003-gateway-broker-internal-api.md`), which describes the row.
6. **Some audit events are stdout-only.** Every audited API route and the file-transfer claim and completion are stored in Postgres (ADR 0010). These go to the log stream only: the gateway's events (`launch.redeem`, `launch.host_mismatch`, `session.lookup`, `session.stream`, `session.list`, `session.revoke`, `session.host_mismatch`; `internal/gateway/*.go` `g.audit(...)`), `session.touch` (`internal/api/auditstore.go:36-38`), the per-request `http.request` record, the sign-out `session.revoke` record (`internal/api/auth.go:790`) and `admin.quota.config_apply` (`internal/backend/tenantquota.go:79`). Delivery, retention and tamper evidence for the log stream are the operator's responsibility. → ADR 0010 (`../adr/0010-audit-store-and-retention.md`) and `../runbooks/observability.md#log-integrity--what-tinycdi-guarantees-vs-what-you-must-provide`.
7. **Rate-limit budgets are per client address.** The sign-in and launch budgets key on the client IP (behind trusted proxies, the right-most untrusted `X-Forwarded-For` entry), with IPv6 folded to its /64 (`internal/ratelimit/ratelimit.go:175-190,260`). A client that holds many addresses or /64 prefixes gets one budget per address or prefix. → ADR 0006 (`../adr/0006-rate-limit-state.md`) and `../runbooks/capacity.md#sign-in-rate-limits-and-nat`.
8. **DNS egress is open on every network profile.** Every workspace NetworkPolicy allows UDP and TCP 53 to the cluster DNS pods (`k8s-app: kube-dns`), `Isolated` included (`internal/runtime/linux/backend.go:1601-1620`; `Isolated` is "DNS only", `:1635`). Whether names outside the cluster resolve depends on the cluster DNS configuration. → `../../deploy/helm/tinycdi/README.md#clusteronly-egress` (it states the DNS rule at lines 372-373).
9. **Node drain loses running desktops.** A Ready workspace whose pod is deleted, evicted or failed goes `Failed` with reason `RuntimeLost` and is not rebuilt automatically (`internal/operator/workspace_controller.go:410-426`). The owner or an admin presses Start, which starts a new runtime generation on the same home volume (`api/v1/failure.go:30-36`). → `../lifecycle-reasons.md`.
10. **The admin user directory lists signed-in users only.** It lists everyone who has signed in to the tenant (`internal/api/openapi.yaml:1370-1380`); users who have never signed in are not listed. The session list shows live leases only, not past sessions (`openapi.yaml:1443-1450`); past sign-ins are in the audit log. → `../runbooks/tenant-quotas.md` (file only; Task 12 adds its `### Users` section).
11. **Failed-digest cleanup is check-then-act.** A version could gain a tag or a referrer between the check and the deletion (`docs/security/provenance.md:126-131`). Do not mention cancelled or aborted runs (wording rule above). → `../security/provenance.md`.
12. **Browser support.** Safari/WebKit is out of scope (`docs/compatibility.md:67-68`). → `../compatibility.md`. The apt-pinned browser images need repinning when Debian security updates supersede the pins. → `../images.md#known-limitations`.
13. **Single maintainer, no independent audit.** The same account can author, merge and approve a publish; the recorded `release` and `runtime-train` environment approvals are the control (`docs/security/provenance.md:147-162`). No independent security audit has been done. → `../../SECURITY.md` and `../security/provenance.md#what-a-signature-does-not-prove`.

- [ ] **Step 4: Rewrite `docs/versioning.md`**

```markdown
# Versioning

TinyCDI follows [semantic versioning](https://semver.org). Releases are
`MAJOR.MINOR.PATCH`; a pre-release carries a `-rc.N` suffix and is never
marked latest.

## The 1.x contract — breaking changes only in a new major
- **The `/v1` HTTP API** as described by `internal/api/openapi.yaml`, with
  the conventions of ADR 0002. A minor release may add operations, fields
  and enum values (clients must tolerate unknown fields and values); it
  never removes or changes the meaning of an existing one.
- **The Helm chart values** (chart README). A value is deprecated for at
  least one minor release, with a documented replacement, before a major
  release removes it.
- **The CRDs** `workspaces.cdi.tinyorbit.vn/v1` (`Workspace`,
  `WorkspaceTemplate`). Create and edit `Workspace` objects only through
  the API; the CRD schema follows the same additive rule as the API.

## Not covered by the contract
- The internal mTLS broker API on the backend's `:9443` listener, which
  the operator calls (ADR 0003): both ship in the same release and are
  upgraded together.
- Prometheus metric names and labels, and the log and audit field layout:
  best effort, changes are listed in the release notes.
- Container image internals (filesystem layout, entrypoints, tooling).

## Upgrading within 1.x
<the procedure below, as prose plus one bash block>

## Supported versions
- See `SECURITY.md#supported-versions`.
```

`## Upgrading within 1.x` carries the generic upgrade procedure that lived in the deleted `docs/runbooks/upgrade.md` (`## What is safe to upgrade while sessions run`, `## Before you start`, `## Procedure`, `### GitOps note`; read them with `git show b41be7d:docs/runbooks/upgrade.md | sed -n 598,712p`). Keep only these facts, each checked against its source; drop every version-specific section, the `operator` row's annotation list (Task 2 makes the operator stop reading `template-snapshot`) and the "Upgrading FROM v0.3.0" paragraph:
- Read the target release's notes first. Take the backup set from `backup-restore.md` before upgrading. A downgrade is a restore from that backup.
- The backend applies database migrations at start. It refuses to start on a database that holds a migration it does not embed, for example one written by a newer release (`internal/store/migrate.go:89-93`; `internal/backend/wire.go:164`).
- Helm never upgrades `crds/`: run `kubectl diff -f deploy/helm/tinycdi/crds/`, then `kubectl apply -f` after review. Only additive schema changes are safe under running objects.
- Pin images by digest (`images.*.digest`), never by tag; verify them first with the `cosign verify` command in `docs/security/provenance.md`.
- Render and diff (`helm template … | kubectl diff -f -`), then `helm upgrade tinycdi oci://ghcr.io/tinyorbitvn/charts/tinycdi --version <1.x.y> -n tinycdi-system -f my-values.yaml`, then `kubectl -n tinycdi-system rollout status deployment/backend deployment/operator deployment/frontend`. Seeded templates diff as a create+delete pair of `<name>-<hash8>` revision objects.
- Order with the least user impact: CRDs, then backend, then operator, then frontend.
- Session impact: with `backend.replicas: 2` (`values.yaml:249`) and `maxUnavailable: 0`, a rollout replaces one pod at a time. A client whose stream drops reconnects with the same cookie inside its live lease. Users must relaunch only when every backend replica is down longer than the 30 s lease TTL (`internal/broker/tickets.go:65`). A stopping backend fails readiness, waits `-drain-propagation-delay` (default 5 s), then drains its streams and keeps serving for `-drain-window` (default 8 s) (`internal/backend/config.go:331-332`). The frontend drains for `frontend.drainDelay` (`values.yaml:494`, 8 s).
- Argo CD: apply `crds/` first (outside the app, or in an earlier sync wave), then hard-refresh the application (`argocd app get <app> --hard-refresh`) before it syncs objects that use new fields.
- Post-checks: `tinycdi_lease_failures_total` back to baseline, `tinycdi_quota_drift` at 0 (`internal/observability/metrics.go:226,234`), and one create → connect → delete round trip.

The rc-suffix sentence must match `.github/README.md` `## Release semantics` (line 364 at `b41be7d`): a tag with a `-` suffix is a pre-release, is not marked latest and gets no `:latest` image tag. Every reference above becomes a relative link from `docs/` (`../SECURITY.md#supported-versions`, `adr/0002-api-conventions.md`, `adr/0003-gateway-broker-internal-api.md`, `runbooks/backup-restore.md`, `security/provenance.md`, `../deploy/helm/tinycdi/README.md`). Keep the heading text `## Upgrading within 1.x` exactly: Task 10 links its slug `#upgrading-within-1x`.

- [ ] **Step 5: `SECURITY.md`**

The `## Supported versions` table becomes two rows: `main` (unreleased, fixes land here first) and the latest `1.x` minor (security and bug fixes ship as a new patch release). Then add one paragraph: only the latest 1.x minor is supported, and images are deployed by digest. Delete the `0.5.x` and `0.4` rows, the upgrade-path sentence and the `upgrade.md` link. In the rest of the file, run `grep -nE '(v0\.|v1\.[12]|\(v0\.2 design\)|pre-1\.0|SR-[0-9]|FX-R)' SECURITY.md`. Turn each hit into a present-tense statement, or delete it. A hardening rule stays; its history goes. A "*(v0.2 design)*" marker is deleted; the sentence it marks stays only if its source below confirms it. These hits need more than deleting a marker (line numbers at `b41be7d`):
- 14-23, the `> **Architecture note:**` blockquote: delete it whole. `README.md` and `docs/architecture.md` describe the components.
- 106, link text "ADR 0005 v0.2 components and session hosts": becomes "ADR 0005 components and session hosts".
- 179, "in v0.2 derived, not stored": "derived, not stored" (`internal/api/auth.go:1044-1050`).
- 225-226, "The per-workspace host-binding, cookie-mode and iframe-isolation e2e checks are *(v0.2 design)* and land with the session-host work": delete the sentence. It promises future work.
- 249-250, "in v0.2, one identity shared by every backend replica": "one identity shared by every backend replica (`-gateway-id`)".
- 261 onward, "*(v0.2 design, ADR 0005:* the broker moves in-process …": present tense: the broker runs in-process in `backend`, and the mTLS listener `:9443` serves only the operator (`deploy/helm/tinycdi/templates/backend.yaml:10-13`).
- 269-270, "**Session proxy** (the v0.1 gateway; the backend's session listener in v0.2)": "**Session proxy** (the backend's session listener, `:8444`)".
- 358-364: certificate hot-reload is no longer missing. Move it out of "*Not yet enforced:*" and state it: the backend's app, session and internal listeners and the frontend reload their certificate when the files change (`internal/backend/wire.go:466,937,945`, `build/frontend/main.go:104`), and the operator's client does too (`internal/broker/httpapi/opclient/client.go:105`). Keep the other two "not yet enforced" items.
- 602-612, "in v0.2 the broker is in-process": "the broker runs in-process in the backend". `api.operatorCN` does not exist: the value is `backend.operatorCN` (`deploy/helm/tinycdi/templates/backend.yaml:210-211`).
- 660, the upgrade rule (Task 5 pointed it at `docs/versioning.md`): "follow [the upgrade steps](docs/versioning.md#upgrading-within-1x) and diff CRDs before applying them."
- 674-676, "**Accepted residual risk for v0.2:** `kasmweb/*` browser images may lag … by up to 4 Chromium major versions": the catalog floor is 2 majors behind the native Chromium pin for browser-class entries and 4 for desktop-class entries (`.github/scripts/check-kasm-catalog.sh:13-16`). Write "**Accepted residual risk:**" with those two numbers.
- 687-688, "(the v0.1 api/gateway pods; the backend in v0.2)": "(the backend pods)".

Test names: 14 `*Enforced:*` names in `SECURITY.md` match no Go test at `b41be7d`, and `web/tests/unit/connect.test.tsx` does not exist. List them with:

```bash
for t in $(grep -oE '`Test[A-Za-z0-9_*]+' SECURITY.md | tr -d '`' | sort -u); do git grep -qE "func ${t//\*/[A-Za-z0-9_]*}\\b" -- '*.go' || echo "MISSING $t"; done
```

Known successors: `TestClusterOnlyEgressDefaultUnrestricted` → `TestClusterOnlyEgressDefaultRestricted` (`internal/runtime/linux/backend_netpol_test.go:287`; the sentence it pins must say restricted by default); `TestClusterOnlyEgressDefaultRendersNoFlag` → `TestClusterOnlyEgress_DefaultRendersNoFlagAndNoWarning` (`deploy/helm/chart_clusteronly_egress_test.go:48`); `TestAPIBrokerReachableFromOperator` → `TestBackendBrokerReachableFromOperator`; `TestMetricsNotOnPublicGatewayService` → `TestMetricsNotOnPublicBackendService`; `TestPortalCSP` → `TestFrontendCSP`; `web/tests/unit/connect.test.tsx` → `web/tests/unit/session/launch.test.ts`. For each other missing name, find the test that asserts the same behavior (`git grep -n 'func Test' -- '<package>/*_test.go'`) and use it. If none exists, delete the name, and if a rule is then left with no test, replace its `*Enforced:*` line with `*Not pinned by a test.*`. Never invent a test name. Re-run the loop: it must print nothing.

In `docs/README.md`, add:

```markdown
## Releases

- [releases/v1.0.0.md](releases/v1.0.0.md) — what 1.0.0 contains, how to install it, known limitations
```

- [ ] **Step 6: Run and commit**

```bash
bash .github/scripts/check-doc-versions.sh docs/releases/v1.0.0.md docs/versioning.md SECURITY.md docs/README.md; echo "versions rc=$?"
python3 .github/scripts/check-doc-links.py docs/releases/v1.0.0.md docs/versioning.md SECURITY.md docs/README.md; echo "links rc=$?"
bash .github/tests/failed-digest-cleanup.test.sh > /tmp/w2-t9.log 2>&1; echo "fdc rc=$?"; grep FAIL /tmp/w2-t9.log | head
bash .github/tests/docs-gates.test.sh; echo "docs-gates rc=$?"
awk '/^## Known limitations/{f=1;next} /^## /{f=0} f && /^- \*\*/' docs/releases/v1.0.0.md | wc -l
grep -niE '(abort|cancel)[a-z]* (run|workflow|job)|or aborted' docs/releases/v1.0.0.md; echo "(no lines expected)"
grep -niE 'xfer-token.*(only|cannot)|cannot (read|replace)|isolated from the (user|workspace)' docs/releases/v1.0.0.md; echo "(no lines expected: item 1 must not overstate)"
for t in $(grep -oE '`Test[A-Za-z0-9_*]+' SECURITY.md | tr -d '`' | sort -u); do git grep -qE "func ${t//\*/[A-Za-z0-9_]*}\\b" -- '*.go' || echo "MISSING $t"; done; echo "(no MISSING lines expected)"
```

Expected: every rc is 0, the release notes list 13 limitations, and the three checks print only their trailer lines.

```bash
git add docs/releases/v1.0.0.md docs/versioning.md SECURITY.md docs/README.md
git commit -m "docs: v1.0.0 release notes, the 1.x versioning contract and support policy

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 10: Chart README, install runbook, quickstart, repository and CI READMEs (C2) — tier: integrate

**Files:**
- Modify: `deploy/helm/tinycdi/README.md` (871 lines): drop the v0.2 blockquote (10-14), rewrite `## Upgrade` (53-95) and delete `## Removed values` (96-105). Also clear the other gate and tag hits (`grep -nE` below).
- Modify: `docs/runbooks/install.md` (487 lines): line 408 "pre-v0.3 row", and line 455, which links the deleted upgrade runbook.
- Modify: `docs/quickstart.md` (line 23: `kind v0.33.0` is a real external version and stays; Task 14 allowlists it)
- Modify: `.github/README.md` (425 lines): review link at :148, tracking tags (11 hits), release semantics (:360-370)
- Modify: `README.md` (110 lines): the `**Status:**` paragraph (95-100)

**Interfaces:**
- Consumes: `docs/versioning.md#upgrading-within-1x` and `docs/releases/v1.0.0.md` (Task 9).
- Produces: user-facing install docs with no version history. Task 14 allowlists the external version strings that remain here.

Rules for Tasks 10–12:
- (a) Version-history sentences become present tense, or are deleted.
- (b) A tracking tag (`E13`, `D26`, `SR-5-F2`, `FX-R31`, `V3.26`, `SEC-…`, `RL-…`) is deleted from user docs together with its parenthesis. The sentence keeps its substance. The exception is a tag that names a threat-model row, which becomes its `TM-` id in Task 13.
- (c) Links to deleted files go to the doc that now holds the fact.
- (d) No new content beyond what the code does today, checked with the command the step lists.
- (e) No guarantee stronger than the code: do not add "cannot", "only", "isolated", "encrypted" or "never" unless the step gives the source that enforces it. Where a doc already says one of these about the workspace pod's Secret or `tcdi-xferd`, correct it (the workspace user can read the mounted Secret; see Task 9 Step 3 item 1).
- (f) Text that CI scripts read is kept: the Global Constraints anchors in `docs/images.md` and `docs/compatibility.md`; in `.github/README.md` the patterns `cosign verify-blob .*runtime-images\.json`, `runtime-images\.yml@refs/heads/main` and `TRAIN_ATTESTATIONS_ENABLED` (`.github/tests/supply-chain-hardening.test.sh:120-123,166-167`); in `docs/security/provenance.md` the patterns `pullable|digest-addressable` and `never (signed|tagged)|unsigned` (`supply-chain-hardening.test.sh:188-191`). Every `kasmweb/*` reference stays digest-pinned and cataloged: `bash .github/scripts/check-kasm-catalog.sh` exits 0.

Tag scan used by Tasks 10–12: `TAG='\b(SR-[0-9]+(-F[0-9]+)?|SEC-[0-9A-Z-]+|FX-R[0-9]+|R-V[0-9.]+|V3\.[0-9]+|D[0-9]{1,2}|E[0-9]{1,2}|W[0-9]\.[0-9]|RL-[A-Z0-9-]+)\b'`.

- [ ] **Step 1: Chart README**

Check: `rg -n 'browserAllowlist' deploy/helm/tinycdi/values.yaml deploy/helm/tinycdi/templates/*.tpl | head -3; sed -n 1,12p build/kasm-catalog.txt`

- Replace lines 10-14 (the "v0.2" blockquote) with nothing. The intro paragraph above already describes the components.
- `## Upgrade` becomes:

~~~markdown
## Upgrade

```bash
helm upgrade tinycdi oci://ghcr.io/tinyorbitvn/charts/tinycdi --version <1.x.y> -n tinycdi-system -f my-values.yaml
```

Within 1.x, upgrades follow [the versioning contract](../../../docs/versioning.md#upgrading-within-1x).
Seeded WorkspaceTemplates are immutable `<name>-<hash8>` revision objects —
a spec change creates a new revision and removes the superseded one, so
upgrades and rollbacks never hit the CEL immutability wall.
~~~

- The "Kasm note (E13)" block (lines 70-95) becomes the subsection `### Kasm browser templates` under `### Kasm workspace images` (line 665). It keeps the facts (the ≤2-major browser gate, `kasmAdapter.browserAllowlist` ships empty, options A and B with their YAML), drops "E13" and "after this upgrade", and replaces its `docs/runbooks/upgrade.md` reference with the link `../../../docs/kasm-images.md`. Check the facts it keeps: `grep -n 'browserAllowlist' deploy/helm/tinycdi/values.yaml` (`browserAllowlist: []`, line 102) and `sed -n 13,16p .github/scripts/check-kasm-catalog.sh` (browser floor = native Chromium major − 2, desktop − 4).
- Delete `## Removed values` (96-105). The ClusterOnly restricted default is already described under `#### ClusterOnly egress` (line 333). Check that section says it in present tense. A values file carrying `templates[].nodeSelector` is refused by the schema (`additionalProperties: false`, tested in Task 3). That behavior needs no history table. See Open question 3.
- Clear the rest (with `TAG` set as in Task 10): `grep -nE "$TAG|v0\.|pre-1\.0|previously|no longer|#[0-9]{2,3}\b" deploy/helm/tinycdi/README.md`. For example, `(default OFF, S36)` at line 430 becomes a `TM-` id in Task 13. Leave it here for now, since the version gate does not flag it. The hits at `b41be7d` besides the blocks above (8 tag hits in all, including `E13` ×3, `E4` ×2, `SR-3-F5`, `SEC-33`, `SEC-09`):
  - 116-120 (`## Uninstall and CRD policy`), "see `docs/runbooks/upgrade.md` → "GitOps note"" (plain text after Task 5): the sentence ends after "…before the app syncs CRs that use the new fields" and adds "(see [Upgrading within 1.x](../../../docs/versioning.md#upgrading-within-1x))".
  - 327, "keeps pre-v0.3.1 packed-by-scoring placement": "keeps packed-by-scoring placement".
  - 338-341, `#### ClusterOnly egress`, "(the pre-1.0 posture — confinement then rests …)": "To allow egress to any pod in any namespace (confinement then rests …)". The restricted default stays stated in the first sentence.
  - 463, "This was GitHub issue #13.": delete the sentence.
  - 663, "the v0.1 `nodeSelector` entry field was removed in v0.3 and fails the render": "a top-level `nodeSelector` field on a template entry fails the render (values schema)".

- [ ] **Step 2: Install runbook, quickstart, top-level README**

- `install.md:408`: "A session without a retained token (pre-v0.3 row, rotated key)" becomes "A session without a retained token (for example after a key rotation)". Check with `sed -n 400,412p docs/runbooks/install.md`.
- `install.md:455`: the sentence becomes "Upgrade order, session impact and rollback: [Upgrading within 1.x](../versioning.md#upgrading-within-1x)." Do not restate the order here; Task 9 wrote it there (CRDs, backend, operator, frontend; rollback is a restore from backup). Check: `sed -n 444,457p docs/runbooks/install.md`.
- `quickstart.md`: no change apart from Task 14's allowlist entry for `kind v0.33.0`. Re-read it for history wording: `grep -nE "$TAG|previously|no longer|pre-" docs/quickstart.md`.
- `README.md` `**Status:**` paragraph (95-100): "TinyCDI 1.0 — see the [release notes](docs/releases/v1.0.0.md) and [known limitations](docs/releases/v1.0.0.md#known-limitations). Maintained by one person; no independent security audit has been done. Report vulnerabilities per [SECURITY.md](SECURITY.md)." Task 5 already removed the `review-v1.0.md` link here. Confirm with `sed -n 90,110p README.md`.

- [ ] **Step 3: `.github/README.md`**

Check: `sed -n 140,152p .github/README.md; sed -n 355,372p .github/README.md; rg -n 'environment: release|environment: runtime-train' .github/workflows/*.yml`

- `:148`: Task 5 already made "(docs/security/review-v1.0.md …)" plain text. Now rewrite the sentence so it states the rule (the `release` and `runtime-train` environments require a reviewer approval) without referring to the review report.
- Release semantics (around 360-370): a tag push `vX.Y.Z` produces images `:X.Y.Z` plus `:latest`, chart `X.Y.Z`, and a release marked latest. A tag with a `-` suffix (`vX.Y.Z-rc.N`) produces a pre-release that is not marked latest. Publishing waits for the `release` environment approval. Use the placeholder form `vX.Y.Z` so the example triggers neither gate. That replaces `v1.2.3`/`rc.1`, which hit the gate.
- Clear tags and history (with `TAG` set as in Task 10): `grep -nE "$TAG|previously|no longer|until verified" .github/README.md`. The variable table entries (`TRAIN_ATTESTATIONS_ENABLED`, line 184) state their current default and meaning only. For that row: "same for the runtime train's publish job; unset means off (`runtime-images.yml:767` runs the step only when it is `true`). `setup-repo-protection.sh` does not set it (it sets `ATTESTATIONS_ENABLED` and `CODE_SCANNING_ENABLED`, `:473`); create it manually to enable." Drop "until verified on a tag build". Keep the literal string `TRAIN_ATTESTATIONS_ENABLED` (rule (f)).

- [ ] **Step 4: Run and commit**

```bash
TAG='\b(SR-[0-9]+(-F[0-9]+)?|SEC-[0-9A-Z-]+|FX-R[0-9]+|R-V[0-9.]+|V3\.[0-9]+|D[0-9]{1,2}|E[0-9]{1,2}|W[0-9]\.[0-9]|RL-[A-Z0-9-]+)\b'
F="deploy/helm/tinycdi/README.md docs/runbooks/install.md docs/quickstart.md .github/README.md README.md"
bash .github/scripts/check-doc-versions.sh $F; echo "versions rc=$? (quickstart's kind v0.33.0 is the only expected hit until Task 14)"
python3 .github/scripts/check-doc-links.py $F; echo "links rc=$?"
grep -nE "$TAG|pre-1\.0|pre-v[0-9]|[Pp]reviously|no longer|upgrade\.md" $F ; echo "(no lines expected)"
for t in .github/tests/supply-chain-hardening.test.sh .github/tests/release-topology.test.sh .github/tests/docs-gates.test.sh; do bash "$t" > /tmp/w2-t10-t.log 2>&1 || { echo "FAIL $t"; grep FAIL /tmp/w2-t10-t.log | head -5; }; done
bash .github/scripts/check-kasm-catalog.sh > /tmp/w2-t10-kasm.log 2>&1; echo "kasm-catalog rc=$?"
git add $F
git commit -m "docs: chart, install, quickstart and CI READMEs describe 1.x only

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

Expected: `versions rc=1` with exactly one `FAIL` line, `docs/quickstart.md:23` (Task 14 allowlists it); `links rc=0`; no history lines; no `FAIL <test>` lines; `kasm-catalog rc=0`. No Go test reads these READMEs (`git grep -l 'README' -- 'deploy/helm/*_test.go'` prints nothing at `b41be7d`), so no Go test runs here.

---

### Task 11: Reference docs — compatibility, images, Kasm images, architecture, development, lifecycle reasons, branding, provenance, vulnerability policy (C2) — tier: integrate

**Files:**
- Modify: `docs/compatibility.md` (137), `docs/images.md` (583), `docs/kasm-images.md` (351), `docs/architecture.md` (207), `docs/development.md` (120), `docs/lifecycle-reasons.md` (151), `docs/branding.md` (103), `docs/security/provenance.md` (162), `docs/security/vulnerability-policy.md` (153)

**Interfaces:**
- Consumes: the Task 10 rules (a)–(d) and the `TAG` scan, the ADR numbers from Task 6, and `docs/versioning.md#upgrading-within-1x` (Task 9).
- Produces: reference docs with no version history. `bump-browser-pin` anchors are untouched (Global Constraints: the `- Chromium:` and `- Firefox ESR` lines and the `verify-blob|sigstore.json` mention in images.md; the `current ARG` line and the `| Firefox ESR fallback |` row with `deb sha256` in compatibility.md). So is the `/xfer/` + `tcdi-xferd` runtime contract in `images.md` `## Runtime contract` (lines 93-180).

- [ ] **Step 1: Record the anchors before editing**

```bash
TAG='\b(SR-[0-9]+(-F[0-9]+)?|SEC-[0-9A-Z-]+|FX-R[0-9]+|R-V[0-9.]+|V3\.[0-9]+|D[0-9]{1,2}|E[0-9]{1,2}|W[0-9]\.[0-9]|RL-[A-Z0-9-]+)\b'
grep -nE '^- Chromium:|^- Firefox ESR|verify-blob|sigstore\.json' docs/images.md > /tmp/w2-t11-anchors.txt
grep -nE 'current ARG|\| Firefox ESR fallback \|' docs/compatibility.md >> /tmp/w2-t11-anchors.txt
cut -d: -f2- /tmp/w2-t11-anchors.txt > /tmp/w2-t11-anchors.before
bash .github/tests/bump-browser-pin.test.sh > /tmp/w2-t11-bump.log 2>&1; echo "bump-browser-pin rc=$?"
```

Expected: `rc=0`. `bump-browser-pin.test.sh` and `check-browser-freshness.test.sh` run the bump script against fixture copies, not the real docs, so they cannot catch a damaged anchor. The `diff` in Step 5 is the guard. Expected `/tmp/w2-t11-anchors.before` at `b41be7d`: 9 lines (`images.md` 61, 62, 77, 78, 349, 350, 537; `compatibility.md` 28, 39). None of them may change in this task.

- [ ] **Step 2: compatibility.md, architecture.md, development.md**

- `compatibility.md:75` heading becomes `## hostUsers: false (default)`. Line 86 "since v0.2" goes. Remove `(D26 / R1)` and `(D24)` (line 88). Line 85 "This is the authoritative R1 result:" becomes "This is the reference result:". Line 130 "the E12 sandboxed-runtime matrix" becomes "the sandboxed-runtime matrix". Lines 34/36 (`controller-runtime v0.25`, `k8s.io v0.37`) are real external versions and stay. Task 14 allowlists them.
- `architecture.md` (207 lines at `b41be7d`): line 1 is a Vietnamese title, lines 2-9 a blockquote ("supersedes the component split in the original design below … shipped v0.2 layout"), lines 11-39 the English `## Current components (English summary)`, and lines 41-207 (from the `---` rule to the end) the original Vietnamese design, sections `## 1.` to `## 10.` (`grep -n '^#' docs/architecture.md`). The rewritten file is:
  - `# Architecture`, then one sentence: the components of a TinyCDI install and where each lives in the repo.
  - `## Components`: the bullets of lines 13-35, kept. Correct and extend them with these facts:
    - Backend: four listeners — `:8443` app (`/v1`), `:8444` session gateway, `:9443` internal mTLS (operator only), `:9090` metrics (optional) (`deploy/helm/tinycdi/templates/backend.yaml:1-11`).
    - A new bullet, **State**: Postgres is the only state store — tickets, leases, quota, file-transfer grants and the audit log (`internal/store/migrations/0001_baseline.sql`).
    - Runtime images: the pod's `:8443` is served by `tcdi-xferd` (`cmd/tcdi-xferd/main.go:56`). It serves `/xfer/files/{name}` and reverse-proxies everything else to KasmVNC on `127.0.0.1:8444` (`internal/xferd/xferd.go:1-4`; `build/linux-base/kasmvnc.yaml:21`). Link `images.md#runtime-contract`.
    - A new bullet, **Kasm adapter** (`build/kasm-adapter`): runs cataloged `kasmweb/*` images behind the same contract. Link `kasm-images.md`.
  - The closing paragraph of lines 37-39 (ADRs, compatibility, development), kept.
  - Delete lines 41-207. The `v1alpha1` at line 99 goes with them. The ADRs hold the current decisions.
  No file links to an `architecture.md#…` fragment (`git grep -n 'architecture\.md#'` prints nothing), so renaming the heading breaks nothing.
- `development.md:10`: `(workspaces/v1alpha1)` becomes `(workspaces.cdi.tinyorbit.vn/v1)`. Lines 46-60, the large-index rule: "applies each `NNN_name.sql`" becomes "applies each `NNNN_name.sql`" (the one file is `internal/store/migrations/0001_baseline.sql`); the parenthesis "(the 020/023 builds on `connection_lease` / `launch_ticket` are the documented example, SR-4-F5)" becomes "(for example an index on `connection_lease` or `launch_ticket`, both append-mostly)"; "Until the runner gains a no-transaction mode, an index … ships instead as a **documented `CREATE INDEX CONCURRENTLY IF NOT EXISTS` runbook step** in `docs/runbooks/upgrade.md`" becomes "… ships instead as a **documented `CREATE INDEX CONCURRENTLY IF NOT EXISTS` step** in the release notes of the release that adds the migration, run by the operator before `helm upgrade`". This matches `internal/store/migrations/README.md:26-35`. Line 42: make `internal/store/migrations/README.md` a link (`../internal/store/migrations/README.md`). Lines 83-88 (`## Release-candidate e2e smoke`): make `hack/e2e/README.md` a link (`../hack/e2e/README.md`) and add one sentence linking `../tests/soak/README.md` as the soak and drill harness, run out of band. Today neither README is linked from `docs/`.

- [ ] **Step 3: images.md and kasm-images.md**

- `## Runtime contract` (spec C2: the `/xfer/` + `tcdi-xferd` contract): confirm it states that `tcdi-xferd` owns `:8443`, that KasmVNC listens on `127.0.0.1:8444`, the `X-Tcdi-Xfer-Token` header and the `xfer-token` Secret key, the `/xfer/files/<name>` route and the size cap. Check with `grep -nE 'xferd|/xfer/|xfer-token|X-Tcdi-Xfer-Token|127\.0\.0\.1:8444' docs/images.md`. At `b41be7d` lines 95-124 already state all of these (sources: `cmd/tcdi-xferd/main.go:22-56`, `internal/xferd/xferd.go:1-4,30,97-111`, `build/linux-base/kasmvnc.yaml:21`, `build/linux-base/entrypoint.sh:168-191`). Two corrections:
  - Line 144, the Secret-key table row "`xfer-token` | yes | transfer credential; read by `tcdi-xferd` only (it exits at start without it)" overstates. The Secret is mounted mode `0440` with `fsGroup` 1000 into the one container that runs as uid 1000 (`internal/runtime/linux/backend.go:1103,1142,1235-1240`), so the workspace user can read it too. The row becomes "transfer credential `tcdi-xferd` checks on `/xfer/` requests (it exits at start without it). The workspace user can read it as well; the broker's transfer claim is the authoritative check (ADR 0009 §5)".
  - Line 107, "(ADR 0010 §5 (i))" (Task 6 renamed it to ADR 0009): link `adr/0009-session-data-transfer-policy.md`, file only.
- `images.md:281`: the `### Budgets (V3.26, measured 2026-10-03, …)` heading becomes `### Budgets`. The measurement conditions (trivy and Docker versions, 8-core host) move into one sentence under it. Tool versions are external version strings, and Task 14 allowlists them if the gate flags them. The table (284-293) compares against images that no longer ship: delete the "old `linux-desktop` (openbox + xterm)" column, and in the `browser` column keep only the value after `→` (for example "1.743 GiB", "376"). The paragraph headings "**Fonts (added 2026-10-03; …)**" and "**Transfer helper (added 2026-10-14, F4).**" become "**Fonts.**" and "**Transfer helper.**"; inside the fonts paragraph, "the trivy rows predate them — the runtime-train scan re-measures on publish" becomes "the runtime-train scan measures the trivy rows on every publish".
- `## Scan results` (484-531): keep one subsection, the current images' result (`### 2026-10-03 (V3.26: base split + XFCE desktop) — …`, line 486), titled `### Current images` with the tool versions and the date in its first sentence. Delete `### 2026-09-30 (before the split)` (508-531).
- `images.md:564-567`: drop "(FX-R15: firefox-esr 140.16.0esr vanished and the images workflow went red on the first cache miss)". The rule it illustrates stays, and "no longer exists" there is present tense and stays.
- `### Upgrading an existing desktop workspace` (253-279): delete the whole subsection. It describes moving a home from the openbox/xterm desktop image, which no v1 release ships (`sed -n 253,280p docs/images.md`). The integration subtest it cites (`tests/integration/linux_desktop_test.go:339`) stays; this task changes no code.
- `kasm-images.md`: lines 29, 165, 188, 198, 266, 299, 302, 309 and 340-341 lose their tags and version history (`E13`, `v0.3`, `SEC-44`, `V3.24`, `FX-R18`, `FX-R32`, `rc.5/rc.6`). For example, line 198 "(E13 — the old pin−4 acceptance ended in v0.3; only such images may …)" becomes "(only such images may …)". Line 340 "rc.5, not reproduced on rc.6" becomes "observed intermittently in testing". An upstream KasmVNC version string such as `1.5.x` stays, and Task 14 allowlists it.

- [ ] **Step 4: lifecycle-reasons.md, branding.md, provenance.md, vulnerability-policy.md**

- `lifecycle-reasons.md:100`: "Since v0.4, `WorkspaceEvent` and `WorkspaceCondition` may carry `params`" becomes "`WorkspaceEvent` and `WorkspaceCondition` may carry `params`". Delete line 123's "An operator older than v0.4 writes no annotation" sentence. Check that the `## Failed` table (37-58) matches the code's recoverable set:

```bash
sed -n 26,47p api/v1/failure.go     # FailureRecoverable / FailureUnrecoverable
sed -n 677,700p internal/api/workspaces.go   # :start uses them
```

`BootDeadlineExceeded`, `FailedCleanup` and `RuntimeLost` recover on Start; `ImagePolicyDenied`, `HomeVolumeLost` and `TemplateRejected` are refused. Fix the table if the code differs.
- `branding.md` (with `TAG` set as in Task 10): `grep -nE "$TAG|v0\.|previously|no longer" docs/branding.md`. Rewrite each hit as present tense.
- `provenance.md:116`: drop "(SR-5-F3)". Lines 126-131: "a small TOCTOU residual accepted for v1.1" becomes "a small check-then-act residual, listed in the release notes' known limitations". Line 154: drop "(SR-5-F2, accepted residual — `review-v1.0.md` §7)", which Task 5 made plain text. Keep "accepted residual" as plain words.
- `vulnerability-policy.md` (line numbers at `b41be7d`):
  - 78, "The bookworm base stays for v0.1.0 **without the browser image** — the structural fixes remain open:" becomes "The runtime images stay on bookworm; the structural fixes remain open:" (the browser image ships: `build/release-images.txt`).
  - 107, drop "(D27)".
  - 124-127, the `kasmweb/*` bullet. "the portal surfaces the stale-image warning past 14 days but blocks nothing in v0.2 (D28)" is wrong for 1.x. The API marks a template image older than `-image-stale-after` (default 14 days, `internal/api/templates.go:12`) as `imageStale` (advisory), and refuses create and start with `409 IMAGE_STALE` once it is older than `-image-block-after` (default 45 days, `templates.go:17`; `0` disables; a missing `imageBuiltAt` never blocks) (`internal/backend/config.go:311-314`). Write that.
  - 133, drop "— SEC-19".
  - 149-151, `## 6. Open decisions for the security owner` item 2 "Bookworm vs trixie vs hybrid for the release after v0.1.0 (§4)": becomes "Bookworm vs trixie vs hybrid for the runtime bases (§4)". Keep items 1 and 3.

- [ ] **Step 5: Run and commit**

```bash
TAG='\b(SR-[0-9]+(-F[0-9]+)?|SEC-[0-9A-Z-]+|FX-R[0-9]+|R-V[0-9.]+|V3\.[0-9]+|D[0-9]{1,2}|E[0-9]{1,2}|W[0-9]\.[0-9]|RL-[A-Z0-9-]+)\b'
F="docs/compatibility.md docs/images.md docs/kasm-images.md docs/architecture.md docs/development.md docs/lifecycle-reasons.md docs/branding.md docs/security/provenance.md docs/security/vulnerability-policy.md"
grep -nE '^- Chromium:|^- Firefox ESR|verify-blob|sigstore\.json' docs/images.md | cut -d: -f2- > /tmp/w2-t11-anchors.after
grep -nE 'current ARG|\| Firefox ESR fallback \|' docs/compatibility.md | cut -d: -f2- >> /tmp/w2-t11-anchors.after
diff /tmp/w2-t11-anchors.before /tmp/w2-t11-anchors.after && echo "anchors unchanged"
for t in .github/tests/bump-browser-pin.test.sh .github/tests/check-browser-freshness.test.sh .github/tests/kasm-catalog.test.sh .github/tests/supply-chain-hardening.test.sh .github/tests/failed-digest-cleanup.test.sh .github/tests/docs-gates.test.sh; do bash "$t" > /tmp/w2-t11-t.log 2>&1 || { echo "FAIL $t"; grep FAIL /tmp/w2-t11-t.log | head -5; }; done
bash .github/scripts/check-kasm-catalog.sh > /tmp/w2-t11-kasm.log 2>&1; echo "kasm-catalog rc=$?"
grep -n 'read by `tcdi-xferd` only' docs/images.md; echo "(no lines expected)"
bash .github/scripts/check-doc-versions.sh $F; echo "versions rc=$? (only external tool and library versions expected; Task 14 allowlists them)"
python3 .github/scripts/check-doc-links.py $F; echo "links rc=$?"
grep -nE "$TAG|[Pp]reviously|no longer|before the split|pre-1\.0" $F ; echo "(only 'no longer' in a non-history sense may remain, e.g. 'a pin that no longer exists')"
git add $F
git commit -m "docs: reference docs describe the current platform only

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

Expected: `anchors unchanged`, no `FAIL` lines, `kasm-catalog rc=0` and `links rc=0`. The version output is `rc=1` with exactly two `FAIL` lines, `docs/compatibility.md:34` and `:36` (`controller-runtime v0.25`, `k8s.io v0.37`), which go into Task 14's allowlist; any other hit is a miss in this task.

---

### Task 12: Runbooks — new audit runbook, file-transfer section check, history removed, docs index (C2) — tier: integrate

**Files:**
- Create: `docs/runbooks/audit.md`
- Modify: `docs/runbooks/capacity.md` (646 lines, 37 gate hits and 12 tags), `observability.md` (227), `disaster-recovery.md` (480), `backup-restore.md` (157), `retained-data.md` (133), `tenant-quotas.md` (127), `stuck-finalizer.md` (101), `mtls-cert-revocation.md` (119), `session-policy.md` (158; the `## File transfer` section gains the facts listed in Step 2), `docs/adr/0010-audit-store-and-retention.md` (the backticked path becomes a link), `docs/releases/v1.0.0.md` (one link to `audit.md`), `docs/README.md` (final index)

**Interfaces:**
- Consumes: ADR 0010 (Task 8), Task 10's rules and `TAG` scan, and `/v1/admin/tenants/{tenant}/audit`, `/overview` and `/users` from `internal/api/openapi.yaml`.
- Produces: `docs/runbooks/audit.md`, which ADR 0010 and the release notes link to (this task adds both links; Task 9 named the file in backticks), and the final `docs/README.md` index, which Task 14's gates scan.
- Keeps (Task 9 links them): the headings `### Log integrity — what TinyCDI guarantees vs what you must provide` in `observability.md`, `## Sign-in rate limits and NAT` in `capacity.md` and `## File transfer` in `session-policy.md`, byte for byte.

- [ ] **Step 1: Write `docs/runbooks/audit.md`**

Check (sources at `b41be7d`):

```bash
sed -n 1295,1352p internal/api/openapi.yaml            # audit list: action, actor, target, since, until + PageLimit, Cursor, Sort
sed -n 116,141p internal/observability/metrics.go      # auditEventActions: every action name the code emits
sed -n 45,51p internal/observability/audit.go          # ActorRef: "oidc:" + hex(first 8 bytes of SHA-256(issuer \x00 subject)), or "anonymous"
sed -n 29,45p internal/api/auditstore.go               # notPersisted = session.touch; plain WriteAudit callers stay stdout-only
sed -n 218,236p internal/api/auditroutes.go            # audited(): a failed insert answers 500 INTERNAL, nothing the handler wrote is sent
sed -n 302,303p internal/backend/config.go; sed -n 446,448p internal/backend/config.go   # -audit-retention-days, default 90, 1..3650
sed -n 93,135p internal/backend/retention.go           # leader prunes once at election, then every 24 h, batches of 1000
grep -n 'audit_write_errors_total' internal/observability/metrics.go   # tinycdi_audit_write_errors_total{event}
sed -n 10,17p web/src/admin/AdminLayout.tsx            # /admin/audit
sed -n 15,45p docs/adr/0011-audit-store-and-retention.md   # (0010 after Task 6) the decision this runbook operates
```

The runbook has these sections and states these facts. Do not add claims beyond them.

```markdown
# Runbook — Audit log

<one paragraph: TinyCDI writes every audited action twice — one row in
the `audit_event` table in Postgres and one JSON line on the backend's
stdout. This runbook covers the stored log; the log stream is in
observability.md.>

## What is recorded
- Every audited API route (`auditedRoutes`, `internal/api/auditroutes.go`)
  except `session.touch`, plus the file-transfer claim and completion
  rows the broker writes, is stored (ADR 0010).
- A table of the stored actions, grouped User (`workspace.create`,
  `.start`, `.stop`, `.delete`, `connection.create`, `session.logout`,
  `session.revoke_all`), Data (`data.attach`, `data.purge`), File transfer
  (`file.transfer.upload`, `file.transfer.download`) and Admin (every
  `admin.*` name in `auditEventActions` except `admin.quota.config_apply`),
  columns: action — when it is written. Take the "when" from the route
  in `internal/api/openapi.yaml`.
- Not stored, log stream only: the gateway's events (`launch.redeem`,
  `launch.host_mismatch`, `session.lookup`, `session.stream`,
  `session.list`, `session.revoke`, `session.host_mismatch`),
  `session.touch`, `http.request`, the sign-out `session.revoke` record
  (`internal/api/auth.go:790`) and `admin.quota.config_apply`
  (`internal/backend/tenantquota.go:79`).
- Each row: tenant, actor, action, target kind and id, request id,
  outcome (`success`, `failure`, `denied`), error code, details, time.
  File-transfer rows carry the file name and byte count, never content.
- The actor is a pseudonym, `oidc:` plus 16 hex characters of the SHA-256
  of issuer and subject, or `anonymous`. It is a join key, not a secret:
  anyone who knows a user's issuer and subject can compute it. No raw
  subject, email, display name, source address, token or cookie is
  stored; detail keys that look like credentials are redacted.
## Reading it
- Portal: Admin → Audit (`/admin/audit`), tenant admins only.
- API: `GET /v1/admin/tenants/{tenant}/audit`, newest first, with
  `action`, `actor`, `target`, `since` (inclusive), `until` (exclusive)
  and the paging parameters of ADR 0002 (`limit`, `cursor`; sort
  `-createdAt` only). `actor` takes a pseudonym, or an `issuer|sub`
  owner reference that the server pseudonymizes, so you can search for a
  known user.
- Reading the log is itself audited (`admin.audit.get`).
## Retention
- `-audit-retention-days` (chart `backend.retention.auditDays`), default
  90, range 1 to 3650; it cannot be turned off. The backend replica that
  holds the leader lock deletes older rows once when it becomes leader
  and then every 24 hours, 1000 rows per batch.
## When the store is unavailable
- An audited request whose row cannot be inserted fails with
  `500 INTERNAL`, and nothing the handler produced reaches the client.
  For a mutation that runs in a database transaction, the change and the
  row commit or roll back together; a change that reaches only
  Kubernetes cannot be rolled back, so its response fails instead.
  Unaudited reads keep working.
- Every failed insert or amend is logged at error level with the request
  id and counted on `tinycdi_audit_write_errors_total{event}`. Alert on it.
## Exporting for long-term retention
- The log stream carries every event, stored or not. Ship it to your log
  store to keep events beyond the database window
  (`observability.md#log-integrity--what-tinycdi-guarantees-vs-what-you-must-provide`).
```

Every `<…>` is replaced by the sentences it describes; every reference becomes a relative link from `docs/runbooks/`. Check the `limit`/`cursor` parameter names with `grep -n 'PageLimit:\|Cursor:' -A4 internal/api/openapi.yaml`.

In `docs/adr/0010-audit-store-and-retention.md`, make the backticked runbook path a link (`../runbooks/audit.md`). In `docs/releases/v1.0.0.md` Known limitations item "Some audit events are stdout-only", add the link `../runbooks/audit.md`.

In `observability.md` `## Audit events` (151-227), fix these statements, which contradict the code since the audit store exists:
- 153-159, "emitted as a dedicated **audit event stream**: one JSON object per line on the component's stdout": keep it, and add one sentence after it: "Audited API actions are also stored in Postgres and readable in the portal; see [audit.md](audit.md) for what is stored and for how long."
- 197-200, "raw OIDC subjects/emails never reach the record — only the salted ref": the ref is not salted (`internal/observability/audit.go:45-51`). Write "only the pseudonymous ref (an unsalted hash: anyone who knows the issuer and subject can compute it)".
- 207-210, "A failing sink never fails the request": "A failing stdout write never fails the request". Then add: "A failing audit-store insert on an audited route does fail it (`500 INTERNAL`, see [audit.md](audit.md#when-the-store-is-unavailable))."
- 211-213, "It does **not** guarantee delivery or tamper evidence — the stream is stdout JSONL": "For the log stream it does **not** guarantee delivery or tamper evidence — it is stdout JSONL". The bullets after it stay.
- Keep the heading `### Log integrity — what TinyCDI guarantees vs what you must provide` byte for byte: Task 9 and this task link its slug.
- The "Emitted events" table (176-187) lists fewer actions than the code emits. Add one row per missing name in `auditEventActions` (`file.transfer.upload`/`.download`, `session.revoke_all`, `session.touch`, `session.lookup`, `session.stream`, every missing `admin.*`), each with its source route from `openapi.yaml`, or replace the table with a pointer to the table in `audit.md` plus the gateway-only rows. Pick one; do not leave the table incomplete.

The threat model repeats the stdout-only claim (`docs/security/threat-model.md:235,705` at `b41be7d`); Task 13 fixes it there.

- [ ] **Step 2: Check the file-transfer section in `session-policy.md`**

W1 already added `## File transfer` (lines 52-96). At `b41be7d` it states the `~/transfers` folder, one file at a time, the size cap (`backend.fileTransfer.maxFileBytes`, default 104857600, `deploy/helm/tinycdi/values.yaml:379`; tenant `maxFileBytes` may only lower it), when a change takes effect, the audit actions and the single-use 2-minute grant (`internal/store/filetransfer.go:15`). Check: `sed -n 52,96p docs/runbooks/session-policy.md`. Add exactly these missing facts, then a link to `../adr/0009-session-data-transfer-policy.md`:
- Only the workspace owner can start a transfer; a tenant admin gets `404` on another user's workspace (`internal/api/openapi.yaml:672-690`).
- The folder is flat and there is no listing: a transfer names one file (`internal/xferd/xferd.go:30,97-111`). A second transfer on the same workspace is refused while one is streaming (`internal/broker/transfers.go:156-165`).
- Trust model, one paragraph, the same facts as Task 9 Step 3 item 1, shorter: the workspace user can read the `xfer-token` key and can interfere with `tcdi-xferd`, because both run as the same user in the same container; the owner-only grant, the broker's claim and the gateway's byte cap are the checks that hold, so a tampered helper can mishandle only that workspace's own files within the allowed direction and size. Do not write that the token is secret from the user.

- [ ] **Step 3: capacity.md**

Check (with `TAG` set as in Task 10): `grep -n '^#' docs/runbooks/capacity.md; grep -nE "$TAG|v0\.|rc\.[0-9]" docs/runbooks/capacity.md | cut -c1-120`

Line numbers at `b41be7d`:
- 14: `api/v1alpha1/workspacetemplate_types.go` becomes `api/v1/workspacetemplate_types.go`. 94: `internal/store/migrations/001_control_plane.sql` becomes `internal/store/migrations/0001_baseline.sql`.
- 80-86: delete the sentence "Until v1.1 both tables grew without bound — the upgrade note for migrations 020/023's index builds (`docs/runbooks/upgrade.md`) is why that mattered." The sweep description before it stays.
- 37-38 and 354-356: the 20-session numbers were measured with separate api, gateway and portal Deployments. Keep the numbers and say so without version names: "Measured with the API, gateway and portal as separate Deployments; not yet re-measured on the single backend." Drop "v0.1"/"v0.2" and the component-name remark; `tests/soak` stays named as the harness.
- 247: drop "(v0.5 RL-CEILING amendment; …)" up to its closing parenthesis; the sentence keeps the rule it states.
- 338: "measured in the v0.3 drill" becomes "measured in an outage drill".
- `## Soak at scale on the dev cluster (E9)` (409) becomes `## Soak at scale`. "The advisor's operating rule for shared infra" becomes "A practical rule for a shared node pool". The sizing math (a worked example on a three-node pool, 2026-10-04) and "Measured baselines" stay.
- The four run subsections (467-646) become one `### Soak results` subsection (not "Measured results": line 366 already has that heading) with three `####` parts, each titled by what was measured and its date, with no build names, rc numbers or `FX-R…` tags:
  - `#### 60 sessions × 60 min (2026-10-04)`: the `v0.3.0-rc.3` run (517-572).
  - `#### Backend restart, rollout and session-TLS rotation under 60 sessions (2026-10-04)`: the drill results of the `v0.3.0-rc.4` run (573-613).
  - `#### Internal-CA rotation and rollout rerun (2026-10-04)`: the `v0.3.0-rc.5` subsection (614-646) without its "Supply chain/upgrade" bullet, which is about one release.
  Delete the `v0.3.0-rc.2` run (467-516); rc.3 repeats its shape.
- `## The 20-session load gate — measured` and `## Postgres outage / failover timing — measured`: keep the numbers and drop build names and tags.
- `## Sign-in rate limits and NAT`: keep the heading byte for byte (Task 9 links it); tags go (`RL-…`); link ADR 0006 (`../adr/0006-rate-limit-state.md`).
After the edits, `bash .github/scripts/check-doc-versions.sh docs/runbooks/capacity.md` must print nothing (37 hits at `b41be7d`).

- [ ] **Step 4: The other runbooks**

With `TAG` set as in Task 10, for each of `observability.md`, `disaster-recovery.md`, `backup-restore.md`, `retained-data.md`, `tenant-quotas.md`, `stuck-finalizer.md` and `mtls-cert-revocation.md`:

```bash
grep -nE "$TAG|v0\.|v1\.[12]|rc\.[0-9]|[Pp]reviously|no longer|pre-1\.0|upgrade\.md" <file>
```

Rewrite each hit under rules (a)–(d). Known hits:
- `observability.md:131` and `disaster-recovery.md:147`: "measured in the v0.3 outage drill" becomes "measured in an outage drill".
- `disaster-recovery.md:452`: "A second drill (v0.5, kind quickstart …)" becomes "A second drill (kind quickstart …)".
- "no longer" in `backup-restore.md:65,117,144`, `retained-data.md:124` and `disaster-recovery.md:134,359,384`: keep it where it means a present-tense state ("a volume that no longer exists"). Rewrite it where it describes a past fix ("the wedge is no longer silent" becomes "the wedge is visible: …").

Additions (spec C2: runbooks describe v1.0.0 features):
- `observability.md`: one subsection, `### Admin overview`. It covers what `GET /v1/admin/tenants/{tenant}/overview` and `/admin` show, and the thresholds the overview uses. State: live desktop sessions (active, unexpired leases), Ready workspaces per template, provisioning p95 over the last 24 hours from the boot ledger (`workspace_boot`, `internal/store/overview.go:31`; ledger rows kept 7 days, `internal/backend/retention.go:99`), workspaces stuck deleting for more than 10 minutes (`StuckFinalizerAfter`, `internal/api/statusview.go:635`), and phase counts; the answer is cached 15 seconds per tenant (`internal/api/adminoverview.go:27`); `fresh: false` means the Kubernetes watch is stale and the Kubernetes-derived counts are empty; counts only, no identities (`internal/api/openapi.yaml:1525-1540`). Reading it is audited (`admin.overview.get`). Link `stuck-finalizer.md`.
- `tenant-quotas.md`: one subsection, `### Users`. It covers the user directory (`GET /v1/admin/tenants/{tenant}/users`, `/admin/users`), states that it lists principals who have signed in, and documents per-user sign-out (`…/users/{ownerRef}/sessions:revoke-all`, ADR 0007). Facts: the directory lists everyone who has signed in to the tenant, with last-seen time, active-workspace count and per-user limit, sortable by `ownerRef`, `displayName`, `lastSeen` (`openapi.yaml:1370-1380`); users who never signed in are not listed. The session list (`GET …/sessions`, optional `ownerRef`) shows live leases only and never the lease id (`leaseRef` instead) (`openapi.yaml:1443-1450`). Per-user sign-out ends the user's portal sessions in that tenant and revokes their active leases and outstanding launch tickets in one transaction; the workspaces keep running; it needs the `X-CSRF-Token` header and is audited as `admin.session.revoke` (`openapi.yaml:1489-1500`). Name the heading exactly `### Users` (the release notes link the file).

- [ ] **Step 5: Final `docs/README.md` index**

Rebuild the index in this order. Every file under `docs/` except `docs/superpowers/` gets exactly one bullet, with a one-line description:
1. Getting started (quickstart, install, versioning)
2. Using and operating (each runbook, audit included)
3. Reference (architecture, compatibility, images, kasm-images, lifecycle-reasons, branding, development)
4. Security (threat model, test inventory, provenance, vulnerability policy)
5. Decisions (ADR 0001–0010)
6. Releases

`runbooks/install.md` appears once, under Getting started, not again under Using and operating. Keep the two existing bullets that point outside `docs/` (`../SECURITY.md` under Security and `../deploy/node-profiles/README.md` under Reference). The `architecture.md` description becomes "the components of an install and where each lives in the repo" (Task 11 removed the original design). `kasm-images.md` is not indexed at `b41be7d` and gets a bullet under Reference. No description may carry a version, a status word ("proposed", "accepted") or a review tag.

Check:

```bash
comm -23 <(cd docs && find . -name '*.md' -not -path './superpowers/*' -not -name README.md | sed 's|^\./||' | sort) \
         <(grep -oE '\]\([^)#]+\.md' docs/README.md | sed -E 's/^\]\(//; s|^\./||' | sort -u)
```

Expected: no output, meaning every doc is indexed.

- [ ] **Step 6: Run and commit**

```bash
F="docs/runbooks/*.md docs/adr/0010-audit-store-and-retention.md docs/releases/v1.0.0.md docs/README.md"
bash .github/scripts/check-doc-versions.sh $F; echo "versions rc=$?"
python3 .github/scripts/check-doc-links.py $F; echo "links rc=$?"
bash .github/tests/docs-gates.test.sh; echo "docs-gates rc=$?"
bash .github/tests/failed-digest-cleanup.test.sh > /tmp/w2-t12-fdc.log 2>&1; echo "fdc rc=$?"
grep -nE 'salted|A failing sink never fails' docs/runbooks/observability.md; echo "(no lines expected)"
grep -nE '^### Log integrity — what TinyCDI guarantees vs what you must provide$' docs/runbooks/observability.md | wc -l   # 1
grep -nE '^## Sign-in rate limits and NAT$' docs/runbooks/capacity.md | wc -l                                      # 1
grep -nE '^### Users$' docs/runbooks/tenant-quotas.md | wc -l                                                      # 1
for a in $(sed -n '/auditEventActions = map/,/^\t}/p' internal/observability/metrics.go | grep -oE '"[a-z_.]+"' | tr -d '"'); do grep -qF "\`$a\`" docs/runbooks/audit.md || echo "audit.md misses $a"; done; echo "(no 'misses' lines expected)"
git add docs/runbooks docs/adr/0010-audit-store-and-retention.md docs/releases/v1.0.0.md docs/README.md
git commit -m "docs(runbooks): audit runbook, admin overview and users, history removed

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

Expected: every rc is 0, each heading count is 1, and the two checks print only their trailer lines. The runbooks carry no external version strings that need the allowlist; if the gate flags one, add it in Task 14 and record it in the commit body.

---

### Task 13: Threat model rebuilt for the v1.0.0 architecture, with `TM-NN` ids and a `tcdi-xferd` boundary (C4) — tier: design

**Files:**
- Rewrite: `docs/security/threat-model.md` (751 lines)
- Create: `.github/tests/threat-model-refs.test.sh`
- Modify: every file outside `docs/security/threat-model.md` that cites a threat-model item `S<n>` or `A6-S<n>`, 40 files at `b41be7d` once Tasks 5–8 have deleted the release notes, `review-v1.0.md` and ADR 0008 (checked in a scratch clone; the `git grep -lIP` in Step 4 lists them). Examples: `deploy/helm/tinycdi/{README.md,values.yaml,templates/admissionpolicy.yaml,templates/backend.yaml,ci/*.yaml}`, `deploy/helm/chart_*_test.go`, `config/default/kustomization.yaml`, `config/admissionpolicy/policy.yaml`, `hack/quickstart/restore-drill.sh`, `docs/security/test-inventory.md` (Task 14 rewrites it next) and `internal/{broker,api,gateway,backend,ratelimit,operator,observability}/*.go`. One hit is runtime text: the HELP string of `tinycdi_lease_session_missing_total` (`internal/observability/metrics.go:299`), asserted verbatim by `internal/broker/lease_session_test.go:98`. Step 4 rewrites both the same way; metrics are outside the 1.x contract (spec C2).

**Interfaces:**
- Consumes: the threat-model subsection `#### KasmVNC credential brute force` (Task 6, anchor `#kasmvnc-credential-brute-force`, which Task 9's release notes may link), ADR 0009 §5's trust model (Task 8), Task 9 Step 3's known-limitations items 1, 3–8 and 11–13 (titles listed in Step 3 below) and `docs/runbooks/audit.md` (Task 12).
- Produces:
  - the register ids `TM-01…TM-NN`, used by Task 14's test inventory;
  - the mapping file `/tmp/w2-tm-map.tsv` (`S<n>\tTM-NN` or `S<n>\t-` for a dropped row; an S row split in two maps to the first of its TM rows), whose table goes in the commit message (ruling 9);
  - `.github/tests/threat-model-refs.test.sh`.

The document's target shape, with sections in this order:

```markdown
# Threat model

<one paragraph: what this describes (the v1.0.0 architecture as built,
with code and test references), what it is not (not an independent
audit; SECURITY.md is the normative policy), and how to read an id
(TM-NN rows in §6; every row names its control and its pinning test).>

## 1. System in one paragraph
## 2. Assets
## 3. Actors
## 4. Trust boundaries and data flows
### Boundary 1 — browser ↔ edge
### Boundary 2 — edge ↔ frontend and backend app listener :8443
### Boundary 3 — backend ↔ OIDC IdP
### Boundary 4 — backend ↔ Postgres
### Boundary 5 — browser ↔ session listener :8444 (launch, desktop proxy, transfer leg)
### Boundary 6 — session listener ↔ workspace pod (`tcdi-xferd` :8443 → KasmVNC 127.0.0.1:8444)
#### KasmVNC credential brute force
#### In-pod transfer helper
### Boundary 7 — operator ↔ internal mTLS listener :9443
### Boundary 8 — operator ↔ Kubernetes API (including the kasm adapter)
## 5. Cross-cutting controls
### Secrets handling
### Supply chain
### Audit
### Failure posture
## 6. Threat register
| ID | Boundary | Threat | Control | Pinned by | Residual |
## 7. Accepted residuals
## 8. Review guide
```

- [ ] **Step 1: Write the reference gate first**

Create `.github/tests/threat-model-refs.test.sh` (checked in a scratch clone of `b41be7d`: it fails with `no register rows` before Step 3 and passes after Steps 3–4):

```bash
#!/usr/bin/env bash
# threat-model-refs.test.sh — the threat register (docs/security/threat-model.md
# §6) has rows TM-01..TM-N without gaps, each with a Control and a Pinned by
# cell; every Go test the document names exists; every TM-NN cited anywhere
# resolves to a row; and no pre-rebuild item id (S<n>, A6-S<n>) is cited
# anywhere (they were renumbered to TM-NN).
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TM="$ROOT/docs/security/threat-model.md"
fails=0
mapfile -t ids < <(grep -oE '^\| TM-[0-9]{2} \|' "$TM" | grep -oE 'TM-[0-9]{2}')
[ "${#ids[@]}" -gt 0 ] || { echo "FAIL: no register rows (| TM-NN | …) in $TM"; exit 1; }
i=1
for id in "${ids[@]}"; do
  want="$(printf 'TM-%02d' "$i")"
  [ "$id" = "$want" ] || { echo "FAIL: register row $id out of order (want $want)"; fails=1; }
  i=$((i + 1))
done
# Columns: | ID | Boundary | Threat | Control | Pinned by | Residual |
# (a literal pipe inside a cell is written \| and is masked first).
while IFS= read -r bad; do
  echo "FAIL: register row without a Control or Pinned by cell: $bad"; fails=1
done < <(grep -E '^\| TM-[0-9]{2} \|' "$TM" | sed 's/\\|/¦/g' \
  | awk -F'|' '{c=$5; p=$6; gsub(/[ \t]/,"",c); gsub(/[ \t]/,"",p);
               if (NF < 8 || c == "" || c == "—" || p == "" || p == "—") print $2}')
while IFS= read -r t; do
  git -C "$ROOT" grep -qE "^func $t\(" -- '*.go' || { echo "FAIL: threat model cites $t, which no Go file defines"; fails=1; }
done < <(grep -oE '`Test[A-Za-z0-9_]+`' "$TM" | tr -d '`' | sort -u)
while IFS= read -r ref; do
  printf '%s\n' "${ids[@]}" | grep -qx "$ref" || { echo "FAIL: '$ref' is cited but has no register row"; fails=1; }
done < <(git -C "$ROOT" grep -ohE '\bTM-[0-9]{2}\b' -- ':(exclude,glob)docs/superpowers/**' | sort -u)
# Old item ids: S<n> or A6-S<n>, not preceded by a letter+slash (NFS/S3) or
# another word char or dash (FX-S1), not followed by a word char or dash
# (S3-fuse), by /NFS-style words (S3/NFS), or by an object-storage noun
# (S3 bucket). go.sum and lockfiles are excluded (base64 such as "+S0=").
old="$(git -C "$ROOT" grep -nIP '(?<![A-Za-z]/)(?<![\w-])(?:A6-)?S[0-9]{1,2}(?![\w-])(?!/[A-Z]{2,})(?! (?:bucket|buckets|storage|object|API|compatible)\b)' -- \
  ':(exclude,glob)docs/superpowers/**' ':(exclude).github/tests/threat-model-refs.test.sh' \
  ':(exclude)go.sum' ':(exclude,glob)**/package-lock.json' | head -20)"
[ -z "$old" ] || { echo "FAIL: pre-rebuild threat-model ids still cited:"; echo "$old"; fails=1; }
[ "$fails" -eq 0 ] && echo "threat-model-refs: ${#ids[@]} rows, every TM reference resolves, no S<n> ids left"
exit "$fails"
```

The register-row checks need the exact column order `| ID | Boundary | Threat | Control | Pinned by | Residual |`, so a row has 8 `|`-separated fields. Write a literal pipe inside a cell as `\|`.

Run: `bash .github/tests/threat-model-refs.test.sh; echo rc=$?`. Expected: `rc=1` with `no register rows`. Then check the id regex against its known false positives and against the current tree:

```bash
RE='(?<![A-Za-z]/)(?<![\w-])(?:A6-)?S[0-9]{1,2}(?![\w-])(?!/[A-Z]{2,})(?! (?:bucket|buckets|storage|object|API|compatible)\b)'
echo 'NFS/S3 S3/NFS S3 bucket S3-fuse A6-S12 (S17) E7/S18 S30/S36 xS3 S100' | grep -oP "$RE" | tr '\n' ' '; echo
git grep -nIP "$RE" -- ':!docs/superpowers/**' ':!docs/security/threat-model.md' ':(exclude)go.sum' ':(exclude,glob)**/package-lock.json' | cut -c1-140 > /tmp/w2-t13-srefs.txt; wc -l < /tmp/w2-t13-srefs.txt
```

Expected: the first line prints exactly `A6-S12 S17 S18 S30 S36 `. At `b41be7d` plus Tasks 5–8, every line of `/tmp/w2-t13-srefs.txt` is a threat-model citation; the storage mentions in `docs/runbooks/disaster-recovery.md:58,78` (`S3/NFS`, `NFS/S3-fuse`) are not hits. Read the file. If a later task added a non-threat-model `S<n>` (for example `S3 compatible`), widen the final negative lookahead in the gate and in Step 4 identically, and note the case in the commit body.

- [ ] **Step 2: Classify every S-row and write the mapping**

For each of the 50 rows in the current `## 6. Known gaps and open items` table (`awk -F'|' '/^\| S[0-9]/{print $2"|"$3}' docs/security/threat-model.md`):
- **Drop** (`S<n>\t-`) exactly two rows, whose subject is not a control in v1.0.0:
  - S13 (G0–G5 merged without independent review) is a process item. It becomes the §7 residual "Single maintainer, no independent audit".
  - S25 (dead auth config knobs) described fields that were deleted. Its guard `TestAuthConfig_NoDeadSessionKnobs` (`internal/api/auth_test.go:482`) stays in the test inventory, not in the register.
- **Keep** the other 48 rows, assigned to boundaries as below, numbered `TM-01…` in table order (Boundary 1 first, cross-cutting `X` last; inside a boundary, keep the S order). S50 splits into two rows. The KasmVNC brute-force row is new. That makes 50 rows. If a merge or split changes this, keep the boundary order and re-derive the numbers; the gate checks the sequence.

  | Boundary | S rows (title at `b41be7d`) |
  |---|---|
  | 1 browser ↔ edge | S7 (iframe sandbox, CSP, COOP/CORP, `lax` same-site) |
  | 2 edge ↔ frontend + app `:8443` | S1 (CSRF), S6 (passive auth), S9 (tenant scope), S10 (branding dir), S14 (rate limits), S17 (sign-out vs streams), S19 (sign-out-everywhere), S20 (per-principal limits), S27 (decoder detail), S41 (tenant-admin group), S42 (audit read), S43 (user directory), S44 (admin sessions + revoke), S45 (overview), S46 (failed-workspace recovery), S47 (`:apply`), S48 (transfer mint) |
  | 3 backend ↔ OIDC | S2 (login cookie), S26 (post-logout redirect) |
  | 4 backend ↔ Postgres | S23 (Postgres-only restore), S24 (intent-stream drift) |
  | 5 session listener `:8444` | S3 (cookie digest), S4 (`stream_epoch`; now also the tab id from Task 1), S5 (host binding), S8 (listener isolation), S31 (cookie spray), S32 (abandoned launch), S33 (session COOP), S50a (transfer leg: claim + policy recheck), S50b (`/xfer/` refused by the desktop proxy) |
  | 6 session listener ↔ pod | S18 (client-address chain), new (KasmVNC credential brute force), S34 (ClusterOnly egress, now restricted by default), S39 (KasmVNC fd bound), S40 (clipboard policy reaches the session server), S49 (`tcdi-xferd` token and jail) |
  | 7 internal mTLS `:9443` | S15 (cert/CA reload + serial revocation), S28 (bounded `runtimeGeneration`), S29 (auth failures logged) |
  | 8 operator ↔ Kubernetes API | S16 (AppArmor opt-out), S22 (deploy-time guards), S30 (snapshot in status; Task 2 removed the annotation read), S35 (namespace ResourceQuota), S36 (opt-in admission policy), S37 (template guard), S38 (seccomp profile name) |
  | X cross-cutting | S11 (kasm catalog gate), S12 (runtime image train), S21 (audit coverage + fail-closed persistence) |

- For a kept row, the Control cell names the function and file that the old row cites. Confirm each with `git grep -n 'func .*<Name>'`, because Tasks 1–4 renamed or removed some, for example `adoptSnapshotFromStampedPod` and the dropped `stream_owner_epoch`. The Pinned by cell names the tests the old row cites, plus the tests Tasks 1–3 added for that control. Use Task 4 Step 3's new test names, never the old ones:
  - `TestRateLimitDefaults_V040Budgets` (old lines 187 and 602) becomes `TestRateLimitDefaults_Budgets`;
  - `TestRehydrate_NilDirectoryKeepsV01Behaviour` becomes `TestRehydrate_NilDirectorySingleProcess`;
  - `TestLogin_NoLegacyCookieDeletion` becomes `TestLogin_OnlyHostPrefixedCookies`;
  - `TestPGSessionStore_IDTokenLegacyNull` becomes `TestPGSessionStore_IDTokenAbsentIsNull`;
  - `TestBaseline_MigrateRefusesPre1Database` becomes `TestBaseline_MigrateRefusesUnknownMigration`.

  Drop every name the gate reports missing. A row without a Go test names its gate script or CI job instead, for example `.github/tests/xfer-entrypoint.test.sh` or the `kasm-contract` job.
- These rows are required by spec C4. Write them with at least the cells below; every symbol and test name was checked at `b41be7d`:

  | Row | Control (file:line at `b41be7d`) | Pinned by |
  |---|---|---|
  | S44 admin revoke (B2) | `MountAdminSessionRoutes` wraps `RevokeAll` in `audited(…, authn.RequireCSRF(…))` (`internal/api/adminsessions.go:62-66`). `tenantAdminPrincipal` gates list and revoke (`:77`, `:135`). The list exposes only `leaseRef`, a SHA-256 prefix, never the lease id (`:71`). | `TestAdminSessions_RevokeAllTargetsTheNamedUser`, `TestAdminSessions_RevokeAllRefusals`, `TestAdminSessions_ListHidesLeaseIDs`, `TestAdminSessions_PagingCursorHasNoLeaseID`, `TestAudit_AdminSessionRevoke` |
  | S42 audit read (B2) | `AdminAuditHandler.List` is tenant-admin only (`internal/api/adminaudit.go:59-60`). `DB.ListAudit` scopes by tenant in SQL with a keyset cursor (`internal/store/audit.go:99`). The read is itself audited. | `TestAdminAudit_Authz`, `TestAdminAudit_ListPagesAndFilters`, `TestAdminAudit_RejectsBadInput`, `TestAudit_InsertListFilterCursor` |
  | S47 `:apply` (B2) | `AdminSessionPolicyHandler.Apply` (`internal/api/adminsessionpolicyapply.go:75`) is mounted behind `RequireCSRF` (`internal/api/adminsessionpolicy.go:149`). `If-Match` pins the ceiling version. It does a dry run first. Live sessions are skipped unless `includeActive`. At most `maxPolicyApply = 500` rows per call (`adminsessionpolicyapply.go:19`). Restarts go through the generation-fenced `RestartWorkspace`. | `TestApplySessionPolicy_AuthzAndTenantIsolation`, `TestApplySessionPolicy_Preconditions`, `TestApplySessionPolicy_DryRunListsLooserOnly`, `TestApplySessionPolicy_ConfirmSkipsLiveUnlessIncluded`, `TestApplySessionPolicy_MovedGeneration`, `TestApplySessionPolicy_TruncatesAtCap` |
  | S45 overview (B2) | `AdminOverviewHandler.Get` is tenant-admin only (`internal/api/adminoverview.go:77-78`). It returns counts only, from a cache kept per tenant. | `TestAdminOverview_NonAdminRefused`, `TestAdminOverview_CacheIsPerTenant`, `TestAdminOverview_AggregatesAndCaches` |
  | S48 transfer mint (B2) | `FileTransferHandler.Create` (`internal/api/filetransfers.go:114`) allows the owner only and requires Ready. `validTransferName` (`:89`) accepts single-file names only. Rows are single use and expire after 2 minutes. | `TestMintFileTransfer_RejectsBadNames`, `TestMintFileTransfer_PolicyOwnerAndReadiness`, `TestMintFileTransfer_AuditsMintAndRefusal`, `TestFileTransfer_InsertAndSweep` |
  | S50a transfer leg (B5) | `Gateway.serveTransfer` (`internal/gateway/transfer.go:42`) runs these checks in order: Origin (`:44`), session cookie, host and live lease (`:76-91`), then `Broker.ClaimTransfer` (`:120`) before any byte reaches the pod. The claim (`internal/broker/transfers.go:86`) re-resolves the policy (`:101`) and refuses at `:166`. The gateway enforces the grant's byte cap (`transfer.go:194,203,259,273`). It forces `Content-Type: application/octet-stream` and a `Content-Disposition` built from the grant (`:266-267`). It sends only `X-Tcdi-Xfer-Token` upstream (`:210,248`). | `TestTransfer_UploadRoundTrip`, `TestTransfer_DownloadRoundTrip`, `TestTransfer_UploadOverCapRefused`, `TestTransfer_DownloadOverCapAborts`, `TestTransfer_Refusals`, `TestTransfer_BrokerRefusalsMapped`, `TestClaimTransfer_HappyPathIsSingleUse`, `TestClaimTransfer_PolicyTightenedAfterMint`, `TestClaimTransfer_RejectsMismatches`, `TestClaimFileTransfer_OnePerWorkspaceConcurrent`, `TestCompleteTransfer_BoundToLeaseWorkspaceAndSurvivesRevoke` |
  | S50b `/xfer/` proxy leg (B5) | `serveProxy` refuses any path that is not on `upstreamPathOK`'s allowlist (`internal/gateway/proxy.go:601`, `internal/gateway/authorization.go:545`). Only the transfer leg builds `/xfer/files/<name>` (`transfer.go:166-167`). | `TestTransfer_DesktopProxyStillRefusesXfer` |
  | S49 `tcdi-xferd` (B6) | `internal/xferd/xferd.go` compares the token in constant time, and an empty token refuses everything (`:145-146`). It strips the token before proxying to the desktop (`:66`). Names are checked by `ValidName` (`:84`) and confined by an `os.Root` jail (`:129`). An unknown policy fails closed (`:114`). The size cap is enforced on upload (`:214`). The token is a separate per-workspace Secret key, `xfer-token` (`internal/runtime/linux/backend.go:796-810`). The helper is started and supervised by both entrypoints (`build/linux-base/entrypoint.sh:172-191`). | `TestXferd_TokenPolicyAndCap`, `TestXferd_JailRefusesEscapes`, `TestXferd_DownloadAndUnknownPolicyFailsClosed`, `TestXferd_ProxiesDesktopAndStripsToken`, `TestXferd_ProxiesWebSocketUpgrade`, `TestEnsureSecret_CarriesXferTokenAndNeverRotatesIt`, `.github/tests/xfer-entrypoint.test.sh` |
  | new: KasmVNC credential brute force (B6) | `tcdi-xferd` answers a desktop request without Basic credentials with its own 401 (`internal/xferd/xferd.go:106-110`). The gateway overwrites `Authorization`. KasmVNC listens on `127.0.0.1:8444` only (`build/linux-base/kasmvnc.yaml:20-21`; for the adapter, `build/kasm-adapter/entrypoint.sh:202,254`). The pod NetworkPolicy admits TCP 8443 from gateway pods only (`internal/runtime/linux/backend.go:1660-1672`). The blacklist is 5 failures / 10 s (`build/linux-base/kasmvnc.yaml:80-81`). Link the `#### KasmVNC credential brute force` subsection. | `TestProxy_StripsClientAuth`, `TestXferd_ProxiesDesktopAndStripsToken`, `TestIngressPeerScopedToGatewayNamespace`, `.github/tests/xfer-entrypoint.test.sh` |
  | S21 audit (X) | `audited()` emits one event per covered route (`internal/api/auditroutes.go`). `DurableAuditSink` persists every action except `session.touch` (`internal/api/auditstore.go:36-38,67`). It fails closed when persisting fails. | `TestAuditCoverage_SpecMatchesTable`, `TestAuditCoverage_MountedMuxEmitsEventPerRoute`, `TestAudited_PersistFailureFailsClosed`, `TestAudited_NoTxMutationPersistFailureFailsClosed`, `TestAudited_TouchStaysStdoutOnly`, `TestAudit_AmendAndPrune` |

Write `/tmp/w2-tm-map.tsv` with one line per old id (50 lines), tab-separated:

```
S1	TM-04
S13	-
…
```

Check: `cut -f1 /tmp/w2-tm-map.tsv | sort -u | wc -l` gives `50`, and `cut -f2 /tmp/w2-tm-map.tsv | grep -v '^-$' | sort -u | sort -t- -k2n | tail -1` is the highest TM id you will write.

- [ ] **Step 3: Rewrite the document**

Rules:
- Each register row has the columns `| ID | Boundary | Threat | Control | Pinned by | Residual |`, in that order (the gate parses them):
  - `ID` is TM-NN.
  - `Boundary` is 1–8, or X for cross-cutting.
  - `Threat` is one sentence, centred on the attacker.
  - `Control` says what the code does, with `file.go` and function names, each checked by grep.
  - `Pinned by` lists the test names (or a gate script or CI job). Every Go test named anywhere in the file must exist; the gate checks this.
  - `Residual` is `—` or one sentence.
- No "v1.0 review: held", "fixed in …", "landed since", "model change vs v0.1", PR numbers or "A6-S<n>/D<n>/E<n>" citations. The old header paragraph (lines 3-15) goes, including its citation key (lines 12-15) and the link to the deleted `review-v1.0.md`. Use the target header paragraph above.
- Ports, as the code sets them:
  - backend app `:8443`, session `:8444` and internal `:9443` (`internal/backend/config.go:273,319,343`);
  - frontend `:8443` (`build/frontend/main.go:65`);
  - in the pod, `tcdi-xferd` on `:8443` (`cmd/tcdi-xferd/main.go:57`), proxying to KasmVNC at `https://127.0.0.1:8444` (`cmd/tcdi-xferd/main.go:47`, `build/linux-base/kasmvnc.yaml:20-21`);
  - the gateway reaches the pod through Service `ws-<uid>` `:8443` (`internal/runtime/linux/backend.go:11,139`).
- **Carry `#### KasmVNC credential brute force` over from Task 6, unchanged.** Before you overwrite the file, save it: `cp docs/security/threat-model.md /tmp/w2-t13-old-tm.md`. Paste the subsection from `awk '/^#### KasmVNC credential brute force/{f=1;print;next} f&&/^#{2,4} /{exit} f' /tmp/w2-t13-old-tm.md` under `### Boundary 6`, before `#### In-pod transfer helper`. Change only `S<n>` ids to TM ids, and the old line references that Task 6 Step 3 rewrote. Check the paste: `diff <(awk '/^#### KasmVNC credential brute force/{f=1;print;next} f&&/^#{2,4} /{exit} f' /tmp/w2-t13-old-tm.md) <(awk '/^#### KasmVNC credential brute force/{f=1;print;next} f&&/^#{2,4} /{exit} f' docs/security/threat-model.md)`. Expected: no output, or only id renames.
- Section 6 of the old file ("Known gaps and open items for the v1.0 review") becomes §6 Threat register (controls) plus §7 Accepted residuals.
- §7 has one bullet per residual that Task 9 Step 3 also lists as a known limitation. Each bullet links to its register row. These are items 1, 3–8 and 11–13; items 2, 9 and 10 are functional scope, not security residuals. Check the titles against Task 9 Step 3 before writing, and report any mismatch in the commit body. The bullets:
  1. In-pod trust (file transfer): see `#### In-pod transfer helper`, row S49.
  2. KasmVNC under connection saturation: row S39.
  3. mTLS revocation is server-side only: row S15.
  4. Revocation records are not pruned: row S28.
  5. Some audit events are stdout-only: row S21.
  6. Rate-limit budgets are per prefix: row S14.
  7. DNS egress is open on every network profile: row S34.
  8. Failed-digest cleanup is check-then-act: row S12.
  9. Browser support. Only the apt-pin staleness is a residual here: row S12.
  10. Single maintainer, no independent audit: no row (S13 dropped).
- The old "What the v1.0 reviewer should re-verify" (`### …`, line 711) and "What the reviewer should read first" (`## 7.`, line 742) become `## 8. Review guide`. It gives the order to read code in, and the questions a reviewer should answer, without version names. This is R1's input (spec C4).
  - Keep the four re-verify items: the `Authorization` overwrite, the enforcing CNI, the lease session re-check and the lock order.
  - Drop the clause "the NULL-digest exemption still matches the rolling-upgrade story" (line 729), because no NULL digest exists after W0 and Task 1.
  - Add two questions: "can any path reach `/xfer/` without `ClaimTransfer`?" and "can a pod-side process obtain anything beyond its own workspace?".
- Fix the audit contradiction. Boundary 2's "Audit trail" bullet (old lines 228-240), row S21 (line 609) and the review item at lines 703-709 still say that the audit sink is stdout JSONL only. The truth, from the code:
  - every event emitted through `audited()` (`internal/api/auditroutes.go`) and the broker's transfer claim and completion is persisted to `audit_event` in the audited transaction. If persisting fails, the request fails (`internal/api/auditstore.go`, `TestAudited_PersistFailureFailsClosed`). See ADR 0010 and `docs/runbooks/audit.md`.
  - these stay stdout-only: `session.touch` (`auditstore.go:36-38`); the per-request `http.request` record (`internal/api/middleware.go:452-454`); the gateway-local launch and control-surface events (`internal/gateway/launch.go:421`); the sign-out `session.revoke` record (`internal/api/auth.go:788`); and the startup `admin.quota.config_apply` record (`internal/backend/tenantquota.go:77`).
  - State all five, and say that tamper evidence for the log stream is the operator's.
- `#### In-pod transfer helper` (Boundary 6) states ADR 0009 §5's trust model and links it. State exactly these facts, without adding guarantees:
  - The Secret is mounted with mode 0440 and `fsGroup` 1000. The desktop and `tcdi-xferd` both run as uid 1000 in one container (`internal/runtime/linux/backend.go:1103,1236-1247`; `build/linux-base/Dockerfile:106`). So any process the workspace user starts can read `xfer-token`, the KasmVNC password and the TLS key.
  - Such a process can kill `tcdi-xferd` (same uid) and bind `:8443` (an unprivileged port) with the same certificate, so the gateway cannot tell the difference. It holds the port only until the entrypoint's supervision loop sees the helper gone and exits the container: about 14 s, a 2 s poll then a 12 s grace (`build/linux-base/entrypoint.sh:191-192`, `build/kasm-adapter/entrypoint.sh:276-277`). It can do this again after each restart.
  - The token therefore proves only "the gateway, or a process in this pod". The broker's `ClaimTransfer` is the authoritative check, and the gateway calls it before sending any byte to the pod (`internal/gateway/transfer.go:120`; `internal/broker/transfers.go:86,101,166`).
  - A replaced helper can change or withhold the bytes of that workspace's own transfers. It cannot exceed the grant's direction or size: the gateway counts and caps the bytes and forces the download's content type (`transfer.go:194-214,259-274`). The audited byte count is the gateway's own count. It sees no portal cookie or session credential: the transfer leg sends only the token (`transfer.go:210,248`).
- Every `S<n>` that the document cites internally becomes its TM id.

Check the controls as you write them, for example:

```bash
git grep -nE 'func (\([^)]*\) )?(RequireCSRF|csrfTokenFor)\b' -- internal/api | head -2
git grep -nE 'func TestTransfer_DesktopProxyStillRefusesXfer|func upstreamPathOK' -- internal/gateway | head -3
git grep -nE '^func Test' -- internal/xferd/xferd_test.go internal/gateway/transfer_test.go internal/broker/transfers_test.go
git grep -nE 'secretMode|RunAsUser|FSGroup' -- internal/runtime/linux/backend.go | head -5
git grep -nE '^while pgrep|^sleep 12' -- build/linux-base/entrypoint.sh build/kasm-adapter/entrypoint.sh
```

- [ ] **Step 4: Renumber the references outside the document**

The regex is the gate's (Step 1). `A6-S<n>` is replaced as a whole, so `(FX-R26 / A6-S18)` becomes `(FX-R26 / TM-NN)`. This was checked in a scratch clone: the gate passes after the loop, and `go build ./...`, `go vet` and `go test ./deploy/helm/ ./internal/observability/` stay green.

```bash
RE='(?<![A-Za-z]/)(?<![\w-])(?:A6-)?(S[0-9]{1,2})(?![\w-])(?!/[A-Z]{2,})(?! (?:bucket|buckets|storage|object|API|compatible)\b)'
git grep -lIP "$RE" -- ':!docs/superpowers/**' ':!docs/security/threat-model.md' ':(exclude)go.sum' ':(exclude,glob)**/package-lock.json' > /tmp/w2-t13-files.txt; wc -l < /tmp/w2-t13-files.txt
while IFS= read -r f; do
  RE="$RE" perl -i -pe 'BEGIN { open my $m, "<", "/tmp/w2-tm-map.tsv" or die; while (<$m>) { chomp; my ($o, $n) = split /\t/; $map{$o} = $n } $re = qr/$ENV{RE}/ }
    s{$re}{ (defined $map{$1} && $map{$1} ne "-") ? $map{$1} : $& }ge' "$f"
done < /tmp/w2-t13-files.txt
git grep -nIP "$RE" -- ':!docs/superpowers/**' ':!docs/security/threat-model.md' ':(exclude)go.sum' ':(exclude,glob)**/package-lock.json'
git diff --stat -- internal/observability/metrics.go internal/broker/lease_session_test.go
```

Expected: about 40 files, and the stat shows both metric files changed by the same number of lines (the HELP string and its test move together). The `git grep` lists only references to dropped rows (S13, S25; none exist at `b41be7d`). Rewrite each by hand: delete the parenthetical, or cite the TM row that now covers the topic. Then:
- `make manifests generate sync-chart-crds` (only if an `api/v1` comment changed);
- regenerate the chart golden (Global Constraints);
- `cd web && npm run generate` (only if `openapi.yaml` changed).

- [ ] **Step 5: Check that every pinned test exists, then run the gates**

```bash
grep -oE '`Test[A-Za-z0-9_]+`' docs/security/threat-model.md | tr -d '`' | sort -u > /tmp/w2-t13-tests.txt
while IFS= read -r t; do git grep -qE "^func $t\(" -- '*.go' || echo "MISSING $t"; done < /tmp/w2-t13-tests.txt
grep -oE '`\.github/tests/[a-z0-9-]+\.test\.sh`' docs/security/threat-model.md | tr -d '`' | sort -u | while read -r p; do [ -f "$p" ] || echo "MISSING $p"; done
for k in 'tcdi-xferd' '/xfer/' 'revoke' 'audit' ':apply' 'overview'; do printf '%s: ' "$k"; grep -E '^\| TM-[0-9]{2} \|' docs/security/threat-model.md | grep -cF -- "$k"; done
bash .github/tests/threat-model-refs.test.sh; echo "tm-refs rc=$?"
bash .github/scripts/check-doc-versions.sh docs/security/threat-model.md; echo "versions rc=$?"
python3 .github/scripts/check-doc-links.py; echo "links rc=$?"   # full scope: catches inbound #anchors the rebuild renamed
bash .github/tests/docs-gates.test.sh > /tmp/w2-t13-docs.log 2>&1; echo "docs-gates rc=$?"
LOG=/tmp/w2-t13.log; { go build ./... && go vet ./... && go test -count=1 ./deploy/helm/ ./internal/observability/ && go test -count=1 -run 'LeaseSession' ./internal/broker/; } > "$LOG" 2>&1; echo "go rc=$?"; grep -nE '^(--- FAIL|FAIL)' "$LOG" | head
```

Expected:
- no `MISSING` lines;
- each of the six C4 keywords counts at least 1 row;
- `tm-refs rc=0`, `versions rc=0`, `links rc=0`, `docs-gates rc=0` and `go rc=0`.

The `LeaseSession` broker tests need Docker for Postgres. A skip is not a pass, so run them with `-v` and confirm that `postgres test container unavailable` does not appear.

- [ ] **Step 6: Commit (with the mapping table)**

```bash
{ echo "docs(security): rebuild the threat model for v1.0.0 with TM-NN ids"; echo
  echo "Adds the tcdi-xferd boundary and the /xfer/ leg, moves the register to"
  echo "control/test/residual rows, and renumbers every reference."; echo
  echo "Old -> new:"; awk -F'\t' '{printf "  %s -> %s\n", $1, ($2 == "-" ? "dropped" : $2)}' /tmp/w2-tm-map.tsv; echo
  echo "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"; } > /tmp/w2-t13-msg.txt
git add -A docs/security/threat-model.md .github/tests/threat-model-refs.test.sh $(cat /tmp/w2-t13-files.txt)
git commit -F /tmp/w2-t13-msg.txt
```

---

### Task 14: Test inventory rebuilt, every docs gate on at full scope, and the carry-over audit of `docs/superpowers/` (C2, C4) — tier: integrate

**Files:**
- Rewrite: `docs/security/test-inventory.md` (611 lines)
- Modify: `.github/tests/docs-gates.test.sh` (add the full-scope version gate), `.github/tests/docs-versions.allow` (filled)
- Modify: any doc that the carry-over audit (Step 5) finds is missing a fact

**Interfaces:**
- Consumes: the TM ids from Task 13, every doc from Tasks 5–12, and the gates from Tasks 4–5.
- Produces: a test inventory the R1 review reads; all gates green at full scope; the PR-body note for R1.

- [ ] **Step 1: Rebuild the test inventory**

Keep the eleven-section structure at `b41be7d`, with these headings in order:
`## 1. How the tests run`, `## 2. Auth, session and API surface`, `## 3. Session gateway (listener :8444)` (keep its `### Fuzz targets` subsection), `## 4. Broker, tickets, leases, internal mTLS`, `## 5. Store`, `## 6. Runtime pods, operator, chart`, `## 7. Integration tests (tests/integration, -tags=integration)`, `## 8. CI scanners and gates`, `## 9. Repo-policy automation`, `## 10. Tag glossary` (renamed from `Finding-tag index`) and `## 11. Gaps a reviewer will notice`. Make these changes:
- Every entry cites the `TM-NN` ids it pins. Task 13 Step 4 already rewrote `S<n>` and `A6-S<n>` here, so check that the TM id still names the right row now that this file is rewritten. Tracking tags (`SR-…`, `FX-R…`) may stay beside a TM id (ruling 11), but never in place of one. Drop PR numbers (`#131`) and version tags (`v1.1`) from the prose.
- Add the W0 and W1 suites, each test function with one line saying what it pins and its TM id. The files, all present at `b41be7d`:
  - §2: `internal/api/{actions,etag,page,templates,workspaces,adminsessions,adminaudit,auditstore,adminsessionpolicyapply,adminoverview,adminusers,filetransfers,recovery,statusview_settled}_test.go`
  - §3: `internal/gateway/{transfer,dial}_test.go`
  - §4: `internal/broker/transfers_test.go`
  - §5: `internal/store/{baseline,audit,filetransfer,leases,overview,txhook,users,userlimit}_test.go`
  - §6:
    - `internal/xferd/xferd_test.go`, `api/v1/failure_test.go`
    - `internal/operator/{crd_v1_envtest,endpoint_ready,recovery}_test.go`
    - `internal/provisioning/{boot,restart,list_page}_test.go`
    - `internal/backend/{bootrecorder,retention}_test.go`
    - `internal/runtime/linux/home_volume_test.go`
    - `deploy/helm/{chart_filetransfer,chart_rbac}_test.go`
  - §8/§9: `.github/tests/xfer-entrypoint.test.sh`
  - Web:
    - `web/tests/unit/{admin/audit.test.tsx,admin/overview.test.tsx,session/coldConnect.test.ts,session/filetransfer.test.tsx,session/transfer.test.ts,api/errors.test.ts,design/useServerPage.test.tsx}`
    - `web/tests-portal/session-transfer.spec.ts`
    - `hack/quickstart/smoke/cold-connect.spec.ts`

```bash
for f in internal/xferd/xferd_test.go internal/gateway/transfer_test.go internal/gateway/dial_test.go internal/broker/transfers_test.go \
         internal/api/filetransfers_test.go internal/api/adminsessions_test.go internal/api/adminaudit_test.go internal/api/auditroutes_test.go \
         internal/api/auditstore_test.go internal/api/adminsessionpolicyapply_test.go internal/api/adminoverview_test.go internal/api/adminusers_test.go \
         internal/api/recovery_test.go internal/api/actions_test.go internal/api/etag_test.go internal/api/page_test.go \
         internal/store/baseline_test.go internal/store/audit_test.go internal/store/filetransfer_test.go internal/store/txhook_test.go \
         internal/operator/endpoint_ready_test.go internal/operator/recovery_test.go internal/provisioning/restart_test.go \
         internal/backend/retention_test.go api/v1/failure_test.go deploy/helm/chart_filetransfer_test.go; do
  [ -f "$f" ] && { echo "== $f"; grep -oE '^func Test[A-Za-z0-9_]+' "$f" | sed 's/^func //'; } || echo "ABSENT $f"; done > /tmp/w2-t14-suites.txt
grep -c ABSENT /tmp/w2-t14-suites.txt
git ls-files 'web/tests/*.spec.ts' 'web/tests-portal/*.spec.ts' 'hack/quickstart/smoke/*.spec.ts'
```

  Expected: `0` ABSENT. The Playwright specs are in `web/tests/`, `web/tests-portal/` and `hack/quickstart/smoke/`. There is no `web/tests/e2e/`.
- Replace names that no longer exist. Each was deleted or renamed at W0 (`git grep` finds no `func` for it at `b41be7d`):
  - `TestMigration023_Idempotent` (line 253) → `TestBaseline_MigrateIsIdempotent` (`internal/store/baseline_test.go`).
  - The migration file list at lines 345-346 (`tests/integration/migration017_test.go`, `migration018_test.go`, deleted at W0) → `TestBaseline_SchemaMatchesSnapshot`, `TestBaseline_MigrateRefusesUnknownMigration` (Task 4's new name for `TestBaseline_MigrateRefusesPre1Database`) and `TestBaseline_PortalSessionDigestNotNull`.
  - Use the new names from Task 4 Step 3 everywhere: `TestRateLimitDefaults_Budgets`, `TestRehydrate_NilDirectorySingleProcess`, `TestLogin_OnlyHostPrefixedCookies`, `TestPGSessionStore_IDTokenAbsentIsNull` and `TestBaseline_MigrateRefusesUnknownMigration`. Never use the old names. Drop the names of tests that Tasks 1–3 deleted; the MISSING check below lists them.
  - `TestClusterOnlyEgressDefaultUnrestricted` (line 402) → `TestClusterOnlyEgressDefaultRestricted` and `TestClusterOnlyEgressUnrestrictedOptInOnPolicy` (`internal/runtime/linux/backend_netpol_test.go:287,327`), plus `TestClusterOnlyEgress_DefaultRestricted` and `TestClusterOnlyEgress_UnrestrictedOptIn` (`backend_test.go:375,392`).
  - `TestClusterOnlyEgressDeprecationNotice` and its paragraph (lines 405-407) → `TestClusterOnlyEgress_DefaultRendersNoFlagAndNoWarning` and `TestClusterOnlyEgress_UnrestrictedRendersFlag` (`deploy/helm/chart_clusteronly_egress_test.go:48,77`).
- Write the `partitioned.spec.ts` references (lines 21, 497) as `hack/quickstart/smoke/partitioned.spec.ts`.
- Add new rows in §9 for Task 4's `legacy-shims.test.sh`, Task 5's `docs-gates.test.sh`, Task 6's `adr-refs.test.sh` and Task 13's `threat-model-refs.test.sh`. Add `xfer-entrypoint.test.sh` there too if it is not listed.
- Replace `## 10. Finding-tag index` with `## 10. Tag glossary`. Write one row per tag family that still appears in code, meaning "an internal review or design finding id; the comment beside it states the rule", with the count the command below prints. Ruling 11 keeps these tags in code. At `b41be7d` the counts are about SR 57, SEC 215, FX-R 191, R-V 21, V3 106, D 221, E 185, RL 20, KASM 35.

```bash
for re in '\bSR-[0-9]+(-F[0-9]+)?\b' '\bSEC-[0-9]+\b' '\bFX-R[0-9]+\b' '\bR-V[0-9]+' '\bV3\.[0-9]+\b' '\bD[0-9]{1,2}\b' '\bE[0-9]{1,2}\b' '\bRL-[0-9A-Z]+' '\bKASM-[0-9]+\b'; do
  printf '%s\t%s\n' "$re" "$(git grep -ohE "$re" -- '*.go' '*.ts' '*.tsx' '*.yaml' '*.tpl' '*.sh' ':!docs/**' | wc -l)"; done
```

- `## 11. Gaps a reviewer will notice` lists only gaps that still hold, with no history ("closed in …" rows go). Each gap that matches a §7 residual of the threat model links to it.

Check: every test name and file cited exists, and no old id is left.

```bash
grep -oE '`Test[A-Za-z0-9_]+`' docs/security/test-inventory.md | tr -d '`' | sort -u | while read -r t; do git grep -qE "^func $t\(" -- '*.go' || echo "MISSING $t"; done
grep -oE '`[A-Za-z0-9_./-]+\.(go|ts|tsx|sh)`' docs/security/test-inventory.md | tr -d '`' | sort -u | while read -r p; do git ls-files --error-unmatch "$p" > /dev/null 2>&1 || git ls-files | grep -q "/$p$" || echo "NO FILE $p"; done
grep -nE '(^|[^A-Za-z0-9-])(A6-)?S[0-9]{1,2}\b|#[0-9]{3}\b|\bv0\.[0-9]|\bv1\.[12]\b' docs/security/test-inventory.md | head
bash .github/tests/threat-model-refs.test.sh; echo "tm-refs rc=$?"
```

Expected: no output from the first three commands, then `tm-refs rc=0`.

- [ ] **Step 2: Turn on the full-scope version gate**

Run the gate over the whole scope and list the remaining hits:

```bash
bash .github/scripts/check-doc-versions.sh 2>&1 | tee /tmp/w2-t14-versions.txt | tail -40
```

Each remaining hit is either a real external version string, which gets an allowlist entry, or a miss from Tasks 9–13, which you fix in the doc. The ERE is matched against the whole hit line, so write it against the line as the output prints it. Expected allowlist entries (format `glob :: ERE :: reason`, ruling 6), checked at `b41be7d` against `docs/quickstart.md:23` (`| kind | v0.33.0 or newer | …`) and `docs/compatibility.md:34,36`; check each against the output again:

```
docs/quickstart.md :: \| kind \| v0\.33\.0 :: upstream kind release the quickstart requires
docs/compatibility.md :: controller-runtime v0\.25|k8s\.io v0\.37 :: upstream Go module versions the build pins
```

No entry is needed for KasmVNC: the gate's pattern only matches `v`-prefixed versions, and `KasmVNC 1.5` has no `v`. Any `rc.5`/`rc.6` left in `docs/kasm-images.md` is a Task 11 miss: fix the doc. Add an entry only for a hit you see in the output. A pattern that matches nothing is reported stale and fails the gate. Then add to `.github/tests/docs-gates.test.sh`, after the live link check:

```bash
# live: the version and PR-reference gate over the whole docs scope,
# with the stale-entry check (every allowlist entry must still match).
out="$(bash "$ROOT/.github/scripts/check-doc-versions.sh" 2>&1)" \
  || { echo "FAIL: docs version gate"; echo "$out"; fails=1; }
```

Use the variable names the file already uses (`ROOT`, `fails`). Check with `sed -n 1,30p .github/tests/docs-gates.test.sh`. Change the file's last-but-one line to `[ "$fails" -eq 0 ] && echo "docs-gates: fixtures, links, the $SP_DIR fence and the version gate pass"`.

Run: `bash .github/tests/docs-gates.test.sh; echo rc=$?`. Expected: `rc=0`.

- [ ] **Step 3: Run every `.github/tests` gate**

```bash
for t in .github/tests/*.test.sh; do bash "$t" > /tmp/w2-t14-$(basename "$t").log 2>&1 || echo "FAIL $t"; done; echo done
```

Expected: only `done`. A failure in `legacy-shims.test.sh` means a doc or code edit in Tasks 6–13 reintroduced a pattern, or made an allowlist entry stale. Fix the source, not the allowlist, unless the hit is a real upstream string.

- [ ] **Step 4: Check that nothing outside `docs/superpowers/` refers into it**

```bash
git grep -nIE --untracked 'docs/superpowers|superpowers/(specs|plans)' -- ':!docs/superpowers/**' ':!.github/tests/docs-gates.test.sh' ':!.github/scripts/check-doc-links.py' \
  | sed -E 's#(:\(exclude[^)]*\)|:?!)docs/superpowers/\*\*##g' | grep -E 'docs/superpowers|superpowers/(specs|plans)'
```

Expected: no output. The `sed` drops pathspec exclusions of the folder (`':(exclude,glob)docs/superpowers/**'` in `adr-refs.test.sh`, `threat-model-refs.test.sh` and `check-doc-versions.sh`), which are not references, exactly as Task 5's `sp_refs` does. Task 5's fence enforces the same thing; this broader pattern (`superpowers/(specs|plans)` also catches a relative path) is the manual confirmation before handing off.

- [ ] **Step 5: Carry-over audit — every still-relevant fact in `docs/superpowers/` has a home outside it (Decision 2)**

Walk these sources and confirm that each listed fact exists at its destination. Every check is a `grep` that must print at least one line (in the Check column, `\|` is the table escape for a shell or regex `|`). Each destination was produced by the task named in brackets; a miss means that task's output lost the fact. Fix the destination doc; do not edit `docs/superpowers/`.

| # | Source | Fact | Destination [task] | Check |
|---|---|---|---|---|
| 1 | spec §4 W0.1 | one baseline schema; the backend refuses an unknown or older database | `docs/versioning.md`, `docs/development.md` [10, 11] | `grep -n 'baseline' docs/development.md docs/versioning.md` |
| 2 | spec §4 W0.2 | CRD `v1` only, group `workspaces.cdi.tinyorbit.vn` | `docs/versioning.md`, `docs/architecture.md` [9, 11] | `grep -nE 'workspaces\.cdi\.tinyorbit\.vn/v1\b' docs/versioning.md docs/architecture.md` |
| 3 | spec §4 W0.3 | lists (`limit` default 50, max 500, `cursor`, `nextCursor`), strong ETag + `If-Match` (428/412), error envelope, `:verb` methods | ADR 0002 [6] | `for k in nextCursor If-Match precondition_required '{"error"' ':start'; do grep -qF -- "$k" docs/adr/0002-api-conventions.md \|\| echo "MISS $k"; done` (expected: no output) |
| 4 | spec §6 C2 | the 1.x contract covers `/v1`, chart values and CRD `v1`; internal mTLS and metrics are outside it | `docs/versioning.md` [9] | `grep -nE 'metrics' docs/versioning.md && grep -nE 'mTLS' docs/versioning.md` |
| 5 | spec §4 W0.5 | ClusterOnly egress restricted to tenant namespaces by default; `runtime.networkProfiles.clusterOnly.unrestricted: true` opts out | chart README [10] | `grep -n 'unrestricted' deploy/helm/tinycdi/README.md` |
| 6 | spec §4 W0.6 | Chromium apt pin; `kasmweb/chrome` catalog entry, class `browser`, floor 154; catalog floor rule | `docs/images.md`, `docs/kasm-images.md` [11] | `grep -n 'kasmweb/chrome' docs/kasm-images.md && grep -n 'browserAllowlist' docs/kasm-images.md && grep -n '^- Chromium:' docs/images.md` |
| 7 | spec §6 C2 | the `/xfer/` + `tcdi-xferd` runtime contract: `:8443` served by the helper, KasmVNC on `127.0.0.1:8444`, Secret key `xfer-token` | `docs/images.md` [11] | `grep -nE '127\.0\.0\.1:8444' docs/images.md && grep -n 'xfer-token' docs/images.md` |
| 8 | spec §7 | how the tests run (Postgres container, envtest, kind e2e, Playwright) | `docs/security/test-inventory.md` §1 [14] | `grep -nE 'postgres:18|envtest|Playwright' docs/security/test-inventory.md \| head -3` |
| 9 | W1 user decision 1 | an audited write with no DB transaction answers 500 when the audit write fails after the change applied | ADR 0010 [8] | `grep -n '500' docs/adr/0010-audit-store-and-retention.md` |
| 10 | W1 ruling 3, user decision 9 | audit is fail-closed; every admin read writes a durable row; retention 90 days | ADR 0010, `docs/runbooks/audit.md` [8, 12] | `grep -n '90' docs/runbooks/audit.md docs/adr/0010-audit-store-and-retention.md` |
| 11 | W1 ruling 4 | stdout-only events: `session.touch`, `http.request`, gateway-local events | ADR 0010, release notes, threat model [8, 9, 13] | `grep -ln 'session.touch' docs/adr/0010-*.md docs/releases/v1.0.0.md docs/security/threat-model.md \| wc -l` prints `3` |
| 12 | W1 rulings 5, 6 | `:start` on a recoverable `Failed` restarts at a new generation (leases and tickets of the old one revoked); `:apply` restarts through the same fence | ADR 0009 §3, `docs/lifecycle-reasons.md` [8, 11] | `grep -niE 'generation' docs/adr/0009-*.md \| head -2` |
| 13 | W1 rulings 7, 8 | recoverable `BootDeadlineExceeded`, `FailedCleanup`, `RuntimeLost`; unrecoverable `ImagePolicyDenied`, `HomeVolumeLost`, `TemplateRejected`; a lost pod becomes `Failed/RuntimeLost` | `docs/lifecycle-reasons.md` [11] | `for k in BootDeadlineExceeded FailedCleanup RuntimeLost ImagePolicyDenied HomeVolumeLost TemplateRejected; do grep -q "$k" docs/lifecycle-reasons.md \|\| echo "MISS $k"; done` (expected: no output) |
| 14 | W1 rulings 9, 10 | Ready waits for a ready EndpointSlice endpoint; 15 s gateway dial retry; portal re-mint; CI cold-connect gate (20 runs, p95 < 20 s) | ADR 0005 [7], test inventory §8 [14] | `grep -n 'EndpointSlice' docs/adr/0005-*.md && grep -n 'cold-connect' docs/security/test-inventory.md` |
| 15 | W1 rulings 11, 12, 16 | single file name in flat `~/transfers`, no listing; download via CORS fetch into tab memory; only the owner mints | ADR 0009 §5 [8], `docs/runbooks/session-policy.md#file-transfer` [12], release notes item 2 [9] | `grep -n 'transfers' docs/runbooks/session-policy.md \| head -2 && grep -niE 'owner' docs/adr/0009-*.md \| head -2` |
| 16 | W1 ruling 13 | the desktop proxy 404s `/xfer/`; only the transfer leg reaches the helper | threat model row S50b [13] | `grep -n 'TestTransfer_DesktopProxyStillRefusesXfer' docs/security/threat-model.md` |
| 17 | W1 ruling 15 | platform bound `backend.fileTransfer.maxFileBytes` (default 104857600); rows expire after 2 min; one stream per workspace; a claim older than 30 min is dead | chart README [10], ADR 0009 §5 [8] | `grep -n 'maxFileBytes' deploy/helm/tinycdi/README.md && grep -nE '30 min' docs/adr/0009-*.md` |
| 18 | W1 rulings 17, 18, 23 | user directory lists signed-in principals only; detail route by base64url `ownerRef`; admin sessions list exposes `leaseRef`, never a lease id | `docs/runbooks/tenant-quotas.md` [12], ADR 0007 [7], threat model [13] | `grep -n 'signed in' docs/runbooks/tenant-quotas.md && grep -n 'leaseRef' docs/adr/0007-*.md docs/security/threat-model.md` |
| 19 | W1 rulings 19, 20 | a stuck finalizer is `deletionTimestamp` older than 10 min; provisioning p95 from `workspace_boot` | `docs/runbooks/observability.md` `### Admin overview` [12] | `grep -nE '10 min' docs/runbooks/observability.md && grep -n 'p95' docs/runbooks/observability.md` |
| 20 | W1 rulings 24, 25 | `tcdi-xferd` answers an anonymous desktop request with 401 and passes forwarding headers through; `xfer-token` is created with the Secret, never backfilled, and the helper exits without it | threat model `#### KasmVNC credential brute force` [6, 13], ADR 0009 §5 [8] | `grep -n '401' docs/security/threat-model.md && grep -n 'xfer-token' docs/adr/0009-*.md` |
| 21 | Decision 7, W1 ruling 25 | in-pod trust: the workspace user can read `xfer-token` and could replace `tcdi-xferd` on `:8443`; the broker claim is authoritative | ADR 0009 §5 [8], release notes [9], threat model §7 [13] | `grep -ln 'ClaimTransfer' docs/adr/0009-*.md docs/releases/v1.0.0.md docs/security/threat-model.md \| wc -l` prints `3` |
| 22 | this plan, rulings 1–2 | a launch or WebSocket without a valid `tcdi_tab` gets `400 owner_tab_required`; only a stamped pod is adopted | ADR 0004 [7], threat model row S4 [13] | `grep -nE 'owner_tab_required\|tcdi_tab' docs/adr/0004-*.md docs/security/threat-model.md` |
| 23 | spec §8 R1–R6 (generic part only, ruling 12) | release = tag, release workflow, approval of the `release` environment | `.github/README.md` [10] | `grep -nE 'release.*environment\|environment.*release' .github/README.md` |
| 24 | spec §10 | the catalog floor rule and the image-size baseline with its reason | `docs/kasm-images.md` [11], test inventory §8 [14] | `grep -niE 'floor' docs/kasm-images.md && grep -niE 'baseline' docs/security/test-inventory.md` |

Then read the remaining sources for anything that changes product behaviour and is not in the table:
- the W1 plan's rulings: `sed -n 15,55p docs/superpowers/plans/2026-10-13-tinycdi-v1-w1-features.md`. Rows 9–21 above cover user decisions 1 and 9 and rulings 3–20 and 23–25. Rulings 1, 2, 21 and 22 are build mechanics, with no doc home.
- the W0 plan's task headings: `grep -n '^### Task' docs/superpowers/plans/2026-10-13-tinycdi-v1-w0-foundation.md`. Rows 1–7 cover them.
- this plan's Plan rulings 1–13.

For any behaviour-changing item you find, add a row to this table in the PR body, and find or write its home. The spec's one-time reset procedure (§8 R2–R3, the R5 dev reinstall) is not product documentation (ruling 12) and gets no home; see Open question 1.

Run every check. Each must print at least one line, or print nothing where the row says so. Fix a miss in the destination doc, then rerun `bash .github/tests/docs-gates.test.sh`.

- [ ] **Step 6: PR-body note for R1, then commit**

Write `/tmp/w2-pr-note.md`, which the controller pastes into the PR body:

```markdown
### Notes for the release wave
- `docs/security/review-v1.0.md` is deleted. R1 writes the new review and
  must restore the links Task 5 turned into plain text: `README.md`
  (Status), `SECURITY.md`, `.github/README.md` (environments), and the
  review line in `release.yml` (unchanged by W2; it still names the old
  file, and `release-notes-review-status.test.sh` pins that line).
- `docs/superpowers/` is kept for the executing workflow. Every
  still-relevant fact has a home outside it (Task 14 Step 5); the advisor
  deletes it before R3.
- Threat-model ids are now `TM-NN`; the old -> new table is in the
  threat-model commit.
- Spec exceptions taken: `docs/superpowers/` is not deleted in W2
  (Decision 2); the chart README has no "removed values" table (spec
  W0.5 vs C2: 1.0.0 is the first public chart, and the values schema
  refuses unknown keys).
- R5 browser checklist: include a desktop reconnect (drop the stream,
  watch it resume), because the gateway now refuses a WebSocket without
  `tcdi_tab` and no in-repo test drives a real KasmVNC client through it.
```

```bash
git add .github/tests/docs-gates.test.sh .github/tests/docs-versions.allow \
  docs deploy/helm/tinycdi/README.md .github/README.md README.md SECURITY.md ':(exclude)docs/superpowers'
git status --short -- docs/superpowers   # must still show the untracked plan as `??`, never staged
git commit -m "docs(security): rebuild the test inventory; docs version gate on at full scope

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

## Wave exit

Run on the branch head after Task 14:

```bash
docker info > /dev/null 2>&1 || echo "STOP: no Docker; the Postgres-backed tests would skip, and a skip is not a pass"
LOG=/tmp/w2-exit.log
( go build ./... && go vet ./... && make test && go test -count=1 ./deploy/helm/ \
  && KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path --bin-dir "$PWD/bin")" go test -tags=integration -count=1 \
       -skip '^TestLinuxRuntime(ReadinessAndHome|FirefoxFallback|Desktop|BrowserBaseline)$|^TestKasmAdapter' ./tests/integration/... \
  && (cd web && npm ci && npm run typecheck && npm run lint:strings && npm run test:unit && npm run build) \
  && for t in .github/tests/*.test.sh; do bash "$t" || exit 1; done ) > "$LOG" 2>&1; echo "rc=$?"
grep -nE '^(--- FAIL|FAIL)|Error:' "$LOG" | head -40
go test -count=1 -v -run '^TestBaseline_SchemaMatchesSnapshot$' ./internal/store/ 2>&1 | grep -E 'container unavailable|--- (SKIP|PASS|FAIL)'
```

Expected: no `STOP` line, `rc=0`, no FAIL lines, and the last command prints `--- PASS`. A `--- SKIP` with `postgres test container unavailable` means Postgres never started, so every Postgres-backed test in the run skipped silently (`go test` without `-v` does not print skips). Fix Docker and rerun the whole block. The run is wrapped in a subshell `( … )`, so the gate loop's `exit 1` ends only the subshell, not your shell.

Then confirm the acceptance checks of spec §6:
- C1: `bash .github/tests/legacy-shims.test.sh` (full scope, stale check on) exits 0.
- C2: `bash .github/tests/docs-gates.test.sh` exits 0. `ls docs/releases` shows only `v1.0.0.md`. `docs/runbooks/upgrade.md` and `docs/security/review-v1.0.md` are gone. `docs/runbooks/audit.md` exists. `README.md` links `CONTRIBUTING.md`.
- C3: `bash .github/tests/adr-refs.test.sh` exits 0 with 10 ADRs.
- C4: `bash .github/tests/threat-model-refs.test.sh` exits 0, which means the register is gap-free, every row has a Control and a Pinned by cell, every cited Go test exists, and no `S<n>` id is left. The register also has a row for each of the six spec C4 subjects. Each count must be at least 1:
  `for k in 'tcdi-xferd' '/xfer/' 'revoke' 'audit' ':apply' 'overview'; do printf '%s: ' "$k"; grep -E '^\| TM-[0-9]{2} \|' docs/security/threat-model.md | grep -cF -- "$k"; done`
  `docs/security/test-inventory.md` cites only `TM-NN` ids, and its MISSING / NO FILE checks (Task 14 Step 1) print nothing.

## Open questions

These need the owner's call. The plan proceeds on the stated default.

1. **Where does the one-time reset procedure live once `docs/superpowers/` is deleted?** This is spec §8 R2–R3 (archive, force-push the fresh history) and the R5 dev reinstall. Default: it lives only in the spec, the advisor reads it before deleting `docs/superpowers/`, and it never enters product docs (ruling 12). Reviewer: agree. Also do the deletion commit after R2, so the archive (`tinyorbitvn/tinycdi-archive`) holds the spec, and the advisor reads R3–R6 from there.
2. **Is it acceptable that the session gateway refuses a launch or WebSocket upgrade without `tcdi_tab` (`400 owner_tab_required`)?** Default: yes. The portal is the only client and always sends it. A third-party client of the session host is not part of the 1.x contract (`docs/versioning.md`). Reviewer: agree. Both shipped KasmVNC clients reuse the `path` from the frame URL on reconnect (checked in the images). No in-repo test drives a real client through the gateway, so the R5 browser checklist must include a reconnect: kill the stream, then watch it resume.
3. **Should the chart README keep a "Removed values" table for the release notes to copy (spec §4 W0.5)?** Default: no. 1.0.0 is the first public release, so there is no earlier public chart to migrate from. Spec C2 asks for docs without internal history, and the values schema already refuses unknown keys (Task 3). If the owner wants the table, Task 10 keeps it as `## Values not accepted` without pre-1.0 wording, and Task 9 copies it. Reviewer: agree to drop. Record it as a spec exception (W0.5 vs C2) in the PR body.
4. **Should stamped-pod adoption stay as status-loss recovery (ruling 2)?** Default: keep it. It is the only path that recovers a Workspace restored without status while its pod runs. Reviewer: agree. Task 2 now also pins a stamped pod without a hash (`TestAdoptSnapshot_StampedPodWithoutHashHolds`).
