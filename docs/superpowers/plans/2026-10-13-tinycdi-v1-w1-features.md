# TinyCDI v1.0.0 — W1 Features Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use sdd-orca:subagent-driven-development (recommended) or sdd-orca:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the eight W1 features on top of the W0 foundation: Failed-workspace recovery (F1), a fixed cold first connect (F2), `session-policy:apply` (F3), file transfer through `tcdi-xferd` (F4), admin sessions and sign-out (F5), a durable audit log and viewer (F6), the admin overview (F7) and the tenant user directory (F8).

**Architecture:** Go monorepo. `internal/store` holds Postgres and the single baseline schema. `internal/provisioning` holds the intent/outbox service and the session-policy resolver. `internal/broker` holds tickets, leases and the gateway surface. `internal/gateway` is the session listener, which runs in the backend process and reaches the broker through `broker.LocalGateway`. `internal/api` holds the HTTP handlers, mounted from `internal/backend/wire.go`. `internal/operator` and `internal/runtime/linux` are the controller and pod builder. `build/linux-base` and `build/kasm-adapter` are the runtime images. `web/` is the React portal on types generated from `internal/api/openapi.yaml`. Every task is one reviewable, independently testable change on the branch. Later tasks consume the interfaces earlier tasks name in their **Produces** blocks.

**Tech Stack:** Go 1.26, pgx v5, controller-runtime + controller-gen v0.22.0, envtest, Postgres 18 (tests start it with `docker run postgres:18.0`, pinned in each package's `pg_test.go`/`main_test.go`), React + TypeScript + vitest + Playwright, Helm v4.1.3, kind (CI `quickstart` job).

**Spec:** `docs/superpowers/specs/2026-10-13-tinycdi-v1-public-design.md` (§5 W1, §7 testing). ADR inputs: `docs/adr/0010-session-data-transfer-policy.md` §5 and T11. Read both before any task.

**Codebase:** `main` at `5706fe5` (W0 merged). Line numbers below were checked against `5706fe5`; re-locate by symbol if they moved. The W1 branch starts from that commit.

## Plan rulings

Each line gives the decision, then the reason, then the cost if the decision is wrong.

**User decisions (2026-10-14).** The user accepted every recommendation from the plan review. These are binding:
- An audited write with no DB transaction (user-limit writes, Kubernetes-only paths) returns 500 when the audit write fails after the change applied. ADR 0011 states this.
- Recovery restarts even if the old pod still exists (lease and ticket revocation is atomic). No Stop button for an unrecoverable `Failed` workspace in W1.
- A pod lost to a node drain becomes `Failed/RuntimeLost` and needs a manual Start.
- Ready waits on a ready EndpointSlice endpoint (the kubelet's authenticated readiness probe of :8443); no separate stream-port probe.
- The cold-connect gate runs in the `quickstart` job. Measure, then set the job timeout (90 min if needed). If the post-fix p95 already passes, Task 10 keeps only the CI gate.
- Transfer minting uses the per-session limiter, not a per-principal bucket.
- Downloads use CORS `fetch` then a blob (Origin header is the CSRF fence), not a download-manager link.
- User-detail URLs carry the ownerRef as base64url.
- Every admin read writes a durable audit row.

1. Schema changes go into `0001_baseline.sql` (outcome vocabulary, one audit index, new table `workspace_boot`), and the snapshot is regenerated. No `0002_*.sql` is added. — Reason: the spec says "W1 adds no migrations", and no v1 database exists yet. — Cost if wrong: a dev database built from the W0 baseline must be recreated (`make` e2e already starts from empty).
2. The `audit_event.outcome` CHECK becomes `('success','denied','failure')`. — Reason: this matches `observability.AuditOutcome`, so no mapping layer is needed. — Cost if wrong: a one-line CHECK edit.
3. Fail-closed audit uses a context-carried `store.TxHook`. It inserts the row inside the first transaction of the request that commits, and the row is amended with the final outcome after the handler returns. A request with no committing transaction inserts its row before the response is released, from a buffered response. If that insert fails, the request returns 500. — Reason: there is exactly one transaction helper (`store.DB.WithTx`), so handlers need no plumbing. — Cost if wrong: in a request with several transactions, the row commits with the first one. The amend still corrects the outcome. A non-transactional mutation (one that reaches only Kubernetes) gets "response fails closed", not a rollback.
4. Only events emitted through `audited()` (and the broker's transfer claim/complete) are persisted. `session.touch`, the per-request `http.request` record and gateway-local events stay stdout-only. — Reason: volume. The activity beat and per-request records are not admin-relevant state changes. — Cost if wrong: one entry in `notPersisted`.
5. Recovery and `:apply` use a new `Service.RestartWorkspace(..., fromGeneration int64, ...)` rather than a new outbox kind. The outbox records a `start` intent. The method is fenced on the generation the caller observed. — Reason: the applier already handles a generation bump while desired is Running (`ensurePod` deletes a pod of a fenced-off generation). The fence makes a double-clicked recovery bump only once. — Cost if wrong: a `restart` outbox kind would need a CHECK edit plus applier code.
6. The F1 recovery decision is made in the API from the CR status (the DB never holds `Failed`). The order is: an owner-scoped DB read first (SEC-21), then `StatusView`. Start on `Failed` with a recoverable reason restarts whether or not the failed pod still exists, because the generation fence deletes a leftover pod. — Reason: the spec says "with no pod", but waiting up to `FailedCleanupDelay` (5 min) buys no safety. — Cost if wrong: one extra `obs.RuntimeUID == ""` condition in `Start`.
7. Failure reasons live as constants plus `FailureRecoverable(reason)` in `api/v1/failure.go`. They are written on the Degraded condition. The recoverable reasons are `BootDeadlineExceeded`, `FailedCleanup` and `RuntimeLost`. The unrecoverable reasons are `ImagePolicyDenied`, `HomeVolumeLost` and `TemplateRejected`. Each unrecoverable reason goes `Failed` immediately, and `reconcileFailed` never overwrites it with `FailedCleanup`. `TemplateInvalid` keeps its Pending hold. — Reason: one source for the operator and the API; the API must not import `internal/operator`. — Cost if wrong: a reason moves between two lists.
8. A Ready incarnation whose pod disappears, is terminating, or reports `PodFailed` goes `Failed/RuntimeLost`, with no same-generation rebuild. — Reason: today such a pod is observed `Terminating` against the old incarnation's boot anchor and latches `Failed/BootDeadlineExceeded`, which nothing can leave except stop then start. F1 names that state and makes `:start` recover it with a generation bump that revokes the old leases (the spec's e2e step is "delete pod → start"). A silent automatic rebuild would change lifecycle semantics beyond F1. — Cost if wrong: users must press Start after a node loss. That is the spec's stated UX.
9. F2's "probe of the stream port" means the operator requires a ready EndpointSlice endpoint whose `targetRef.uid` is the current pod. The kubelet's readiness probe already dials `:8443` in the pod. The fix adds the gateway dial retry (budget 15 s). The portal retries by minting a new ticket, because tickets are consumed on the first redeem attempt, inside a 60 s budget (`broker.TicketTTL`). — Reason: the operator cannot dial pods (NetworkPolicy and RBAC by design). Opening operator→pod ingress widens the boundary. — Cost if wrong: the spike (Task 9 Step 1) shows a different cause. The implementer then stops and reports before Step 3.
10. The cold-connect gate (20 runs, p95 < 20 s) runs as a step of the existing CI `quickstart` job. The job timeout is raised to 75 min. — Reason: that job already has a fresh kind install. — Cost if wrong: about +20 min CI on quickstart-relevant PRs. Move it to a dispatch-only job.
11. File-transfer paths are a single file name inside the flat transfer root `$HOME/transfers`, with no `/`, `\`, `;`, `%`, `..`, NUL or control characters (the gateway leg rebuilds the path through `cleanedProxyPath`, so a name it would refuse must be refused at mint). Download asks for the name of a file the user saved there; there is no listing endpoint. — Reason: the smallest surface that meets the ADR. — Cost if wrong: a listing route is a follow-up.
12. Browser downloads use a credentialed CORS `fetch()` of the single-use content URL, then a blob with `<a download>`. The bytes are bounded by `maxFileBytes`. — Reason: it works the same in the `lax` and `partitioned` cookie modes. A top-level navigation would miss a partitioned cookie. — Cost if wrong: tab memory up to the cap (default 100 MiB).
13. The generic desktop proxy keeps `/xfer/` off its allowlist (404). Only the transfer leg builds `/xfer/files/<name>`, and it validates that path through `cleanedProxyPath`. — Reason: the claim, policy recheck and caps must not be bypassable through the desktop proxy. — Cost if wrong: none for security. The ADR wording "joins the allowlist" is met by the dedicated leg.
14. KasmVNC is rebound to `127.0.0.1:8444` (TLS kept), and `tcdi-xferd` owns `:8443`. It proxies to loopback and pins the exact staged certificate. — Reason: this is ADR §5's reference shape, and Pod, Service and NetworkPolicy stay byte-identical. — Cost if wrong: the video path takes one extra in-pod hop. The ADR's second-port variant is the fallback.
15. The platform file bound is the backend flag `-file-transfer-max-bytes` (chart `backend.fileTransfer.maxFileBytes`, default 104857600). A tenant `maxFileBytes` must be ≤ that bound. Transfer rows expire after 2 min. One stream per workspace is allowed, and a claim older than 30 min counts as dead. Minting rides the existing session-keyed limiter (`sessionLimit`). — Reason: the ADR leaves the constants to T11. — Cost if wrong: constants only.
16. Only the workspace owner mints transfers; a tenant admin cannot mint into someone else's session. — Reason: the claim requires the caller to hold the live lease, so an admin mint could never be claimed. — Cost if wrong: none.
17. The user detail route becomes `/admin/users/:ownerRef`, and F8 adds `GET /v1/admin/tenants/{tenant}/users/{ownerRef}`. — Reason: the detail page must not scan the whole collection, and `subject` alone is not unique across issuers. — Cost if wrong: one extra audited admin read.
18. F5 lists connection leases, as the spec says ("live leases"). Portal sign-in sessions without a stream are not listed. "Sign out everywhere" still revokes them too. The list exposes `leaseRef` (16 hex of the SHA-256 of the id), never the lease id. — Reason: the spec's text, and the lease id is an internal handle. — Cost if wrong: a second list source.
19. A stuck finalizer is a Workspace with `deletionTimestamp` older than 10 min that still has finalizers. — Reason: the runbook says "stuck Terminating past a few minutes". — Cost if wrong: one constant.
20. Provisioning p95 comes from a new baseline table, `workspace_boot`. Its `requested_at` is written in the intent transaction (create/start/restart). Its `ready_at` is written by a backend informer hook from the CR's `status.startedAt`. That hook also feeds the dead `ObserveProvisioningLatency`. — Reason: the DB has no ready timestamp, and the CR keeps only the latest incarnation. — Cost if wrong: one table and one hook.
21. ADR 0010 §3's sentence "v1 adds no restart action" is left for W2 C3, which rewrites the ADRs. Task 2 adds only the new ADR 0011 (audit store), because F6 asks for it now. — Reason: W2 owns ADR rewrites. — Cost if wrong: a stale sentence until W2.
22. `internal/backend/wire.go` `appMux` switches from 14 positional parameters to an `appRoutes` struct in Task 2. — Reason: W1 adds five handlers, and positional growth is how wiring bugs hide. — Cost if wrong: none (mechanical).
23. F8 lists only `principal_directory` rows, that is, principals who have signed in. A quota override for a principal who never signed in is not listed, and its detail route returns 404. — Reason: the spec says "everyone who has signed in". — Cost if wrong: a UNION with override and workspace owners in `ListTenantUsers`.
24. `tcdi-xferd` answers a desktop request that carries no Basic credentials with its own 401 and never forwards it. It passes the gateway-built `X-Forwarded-*`/`Forwarded`/`X-Real-Ip` headers through unchanged. — Reason: behind the helper every upstream connection comes from 127.0.0.1, and KasmVNC's brute-force blacklist (5 failures, 10 s) keys on the forwarded address, so anonymous probes must not reach it and the real client address must. — Cost if wrong: a client lockout until the blacklist window passes.
25. The transfer credential is a new Secret key `xfer-token`, created with the Secret in `generateCredentials` and read by the helper only. `K8sCredentialSource` refuses a Secret without it as incomplete, like one without a password. There is no backfill, no `transfer_unsupported` code and no helper fallback; the helper exits at start without the token. — Reason: ADR 0010 §5 asks for a per-workspace credential separate from the KasmVNC password, and there is no upgrade path (Global Constraints). — Cost if wrong: none on a fresh install.

## Global Constraints

- No upgrade path. Never add a compatibility alias, fallback branch, dual read or "legacy" handling for data or objects written by earlier builds. A Secret, row or CR without a W1 field is a fresh-install impossibility, not a case to handle.
- Schema: no new migration file. Schema edits go into `internal/store/migrations/0001_baseline.sql`. After any edit, regenerate the snapshot: `TCDI_UPDATE_SNAPSHOT=1 go test ./internal/store/ -run TestBaseline_SchemaMatchesSnapshot -count=1`, then rerun without the variable.
- CRD group `workspaces.cdi.tinyorbit.vn`, version `v1` only. After any `api/v1` edit run `make manifests generate sync-chart-crds`. The chart `crds/` directory must equal `config/crd/bases` byte for byte.
- HTTP prefix stays `/v1`. Lists accept `limit` (default 50, max 500), `cursor` (opaque) and `sort` (allowlisted, `-` = descending). They return `{"items":[...],"nextCursor":"..."}`, with `nextCursor` omitted on the last page. Use `api.ParsePage`, `api.NextCursor` and `api.Page[T]` (`internal/api/page.go`).
- Error body: `{"error":{"code","message","retryable","requestId","details"?}}` through `api.WriteError`/`writeError`. Never add a public API error code outside `AllErrorCodes`. The internal broker mTLS wire may add internal-only codes next to `codeRevoked` (`internal/broker/httpapi/server.go:38`), and the session-host gateway keeps its flat `{"error":"<code>"}` vocabulary (`internal/gateway/launch.go`).
- Mutable admin resources return a strong `ETag` (`api.ETagOf`). PUT/PATCH/DELETE and `:apply` require `If-Match` (`api.CheckIfMatch`): missing → 428, stale → 412.
- Custom methods use `:verb` paths. W1 adds the literal-segment routes `POST /v1/admin/tenants/{tenant}/session-policy:apply` and `POST /v1/admin/tenants/{tenant}/users/{ownerRef}/sessions:revoke-all`. ServeMux matches a literal last segment containing `:`, so these need no `ActionRouter`.
- Admin routes gate on `tenantAdminPrincipal` (`internal/api/adminquota.go:218`). Reads mount `authn.RequireAuthPassive(audited(...))`. Writes mount `authn.RequireAuth(audited(..., authn.RequireCSRF(...)))`.
- **New API operation checklist.** It applies to every task that adds an operation. Items 3–5 are what `auditroutes_test.go` and `openapi_contract_test.go` enforce, and they fail otherwise.
  1. A path and schemas in `internal/api/openapi.yaml`.
  2. A `routeX` const and an `auditActionX` const in `internal/api/auditroutes.go`, plus an `auditedRoutes` entry.
  3. The action string added to `auditEventActions` in `internal/observability/metrics.go:121`.
  4. A mount in `newCoverageEnv` (`auditroutes_test.go:186`) and a driver in the `routes` table of `TestAuditCoverage_MountedMuxEmitsEventPerRoute`.
  5. One success call (<300) and one refusal call in `TestOpenAPIContract_ResponsesValidate` (`openapi_contract_test.go:513`).
  6. Admin routes only: a mount in `newAdminGroupEnv` (`tenantadmin_test.go:45`) and a `…_CustomAdminGroups` 403 case.
  7. A mount in `appMux` (`internal/backend/wire.go`) and its `backend_test.go` caller.
  8. `cd web && npm run generate`.
  9. A route in `web/tests/mock-api/` (the area that owns the prefix).
- Audit detail keys must not match `observability.sensitiveFieldRe` (`token|cookie|…|session|…|ticket…`), or the value is redacted. Use `counts`, `target`, `phase`, `direction`, `path`, `bytes`, `policy`, `dryRun`, `includeActive`, `affected`, `restarted`.
- UI: every user-visible string is a key in `web/src/i18n/en/<area>.ts` and the same key in `vi/<area>.ts`, because `satisfies Record<keyof typeof …>` fails compilation on a missing key. `npm run lint:strings` must pass. Every new page or flow gets a Playwright spec in `web/tests/*.spec.ts` against the mock API.
- Threat model: every new read/write surface adds one row to the `| # | Item | State today |` table in `docs/security/threat-model.md` §6, continuing the numbering. Rows S42–S50 are assigned per task below.
- Tests that need Postgres start the pinned `postgres:18.0` container through the package's own `ensurePostgres`/`newDB` helper. They skip without Docker, and **a skip is not a pass**. Run them on a host with Docker.
- `tcdi-xferd` (`cmd/tcdi-xferd`, `internal/xferd`) uses the Go standard library only. It must build with `CGO_ENABLED=0`.
- Commit messages are conventional (`feat:`, `fix:`, `refactor:`, `test:`, `chore:`, `docs:`). Every commit ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Never push and never touch tags. Do not edit `SECURITY.md`, `docs/releases/*` or the spec.
- E2E steps added to `hack/quickstart/smoke/*.spec.ts` must at least compile locally: `cd hack/quickstart/smoke && npm ci && npx playwright test --list` (and `--list -c <config>` for any new config). This compiles every spec without a cluster. They run for real in CI's `quickstart` job (kind) when the branch's PR opens. W1 is done only when that job is green.

## Review Focus

1. **A tenant ceiling tightened between a transfer mint and its claim.** Expected: the claim re-resolves the policy, refuses (the gateway content route answers 403 `{"error":"transfer_denied"}`; a mint under the tightened ceiling gets the API's 403 `FORBIDDEN` with `details.reason` = `transfer_denied`), and marks the row `failed`. No bytes move. Pinned in Task 11, `TestClaimTransfer_PolicyTightenedAfterMint`.
2. **Two transfer claims for the same workspace at once** (two tabs, or a replayed content URL with a second minted id). Expected: exactly one claim wins, and the other is refused while the first is `claimed`. Pinned in Task 11, `TestClaimFileTransfer_OnePerWorkspaceConcurrent`.
3. **The audit store fails during an audited mutation.** Expected: the mutation rolls back (no committed state change) and the client gets 500 `INTERNAL`. An audited admin read with a failed insert also returns 500, and no response body leaks. Pinned in Task 1, `TestWithTx_TxHookFailureRollsBackMutation`, and Task 1a, `TestAudited_PersistFailureFailsClosed` and `TestAudited_NoTxMutationPersistFailureFailsClosed`.
4. **A recovery start sent twice** (double-click, or two tabs) while the CR still reads `Failed`. Expected: exactly one generation bump. The second call is a no-op returning the current record. Pinned in Task 6a, `TestRestartWorkspace_GenerationFenced`.
5. **A transfer name that tries to escape the jail**: `../x`, an absolute path, a symlink inside the root pointing outside, a NUL byte, `.` or `..`. Expected: refused at mint (400) and again by the helper (400), with nothing written outside `$HOME/transfers`. Pinned in Task 11a, `TestMintFileTransfer_RejectsBadNames`, and Task 12, `TestXferd_JailRefusesEscapes`.

---

## File map

| Area | Files |
|---|---|
| Audit (F6) | `internal/store/{audit.go,txhook.go,store.go}`, `internal/api/{auditstore.go,auditroutes.go,adminaudit.go}`, `internal/backend/{retention.go,config.go,wire.go}`, `docs/adr/0011-audit-store-and-retention.md`, `web/src/admin/AuditPage.tsx` |
| Users / sessions (F8, F5) | `internal/store/{users.go,leases.go}`, `internal/api/{adminusers.go,adminsessions.go}`, `web/src/admin/{UsersAdminPage.tsx,UserSessionsSection.tsx}`, `hack/quickstart/{realm-export.json,common.sh,up.sh}` (seeded `demo-admin`), `hack/quickstart/smoke/smoke.spec.ts` |
| Overview (F7) | `internal/store/overview.go`, `internal/provisioning/outbox.go`, `internal/api/{adminoverview.go,statusview.go}`, `internal/backend/bootrecorder.go`, `web/src/admin/OverviewPage.tsx` |
| Recovery (F1) | `api/v1/failure.go`, `internal/operator/workspace_controller.go`, `internal/runtime/{backend.go,linux/backend.go}`, `internal/provisioning/service.go`, `internal/broker/activity.go`, `internal/api/workspaces.go`, `docs/lifecycle-reasons.md` |
| Policy + apply (F4a, F3) | `api/v1/workspace_types.go`, `internal/provisioning/{sessionpolicy.go,k8sapplier.go,service.go}`, `internal/api/{adminsessionpolicy.go,adminsessionpolicyapply.go,statusview.go}`, `internal/runtime/linux/backend.go`, `web/src/admin/SessionPolicyPage.tsx` |
| Cold connect (F2) | `internal/runtime/linux/backend.go`, `internal/operator/workspace_controller.go`, `internal/gateway/{proxy.go,dial.go}`, `web/src/session/{coldConnect.ts,SessionPage.tsx}`, `hack/quickstart/smoke/{cold-connect.spec.ts,playwright.cold-connect.config.ts}`, `.github/workflows/ci.yml` |
| File transfer (F4) | `internal/store/filetransfer.go`, `internal/api/filetransfers.go`, `internal/broker/{transfers.go,gatewayshim.go,targets.go,credentials.go,httpapi/server.go}`, `internal/gateway/{transfer.go,brokerclient/client.go,proxy.go}`, `cmd/tcdi-xferd/main.go`, `internal/xferd/*`, `build/{linux-base,kasm-adapter}/*`, `web/src/session/{FileTransfer.tsx,transfer.ts}`, `build/frontend/main.go`, `hack/quickstart/smoke/{transfer.ts,smoke.spec.ts,partitioned.spec.ts}`, `docs/runbooks/session-policy.md`, `docs/images.md` |

Task order: 1 → 1a → 2 → 3 → 4 → 5 → 6 → 6a → 7 → 8 → 9 → 10 → 11 → 11a → 12 → 13 → 14. Tasks 3–5 depend only on 1, 1a and 2. Task 8 consumes 6a and 7. Tasks 11, 11a and 12–14 consume 7.

---
### Task 1: Durable audit store and transaction hook (F6, part 1a) — tier: integrate

Sizing: Task 1 and Task 1a were one task; they are split so each commit is green on its own. Task 1 adds only exported store functions nothing calls yet (`WithTx` behaves as before without a hook). Task 1a swaps the live audit sink.

**Files:**
- Modify: `internal/store/migrations/0001_baseline.sql` (the `audit_event` block, lines 320-334), `internal/store/testdata/schema.snapshot.sql` (regenerated)
- Create: `internal/store/audit.go`, `internal/store/audit_test.go`, `internal/store/txhook.go`, `internal/store/txhook_test.go`
- Modify: `internal/store/store.go:82-103` (`WithTx`)

**Interfaces:**
- Consumes: `store.DB.WithTx(ctx, func(store.Tx) error) error` (`store.go:82`), `store.Tx = pgx.Tx` (`store.go:21`), `store.DB.Pool()`.
- Produces (used by Tasks 1a, 2, 4, 8, 11):
  - `type store.AuditRecord struct{ ID int64; Tenant, ActorRef, Action, TargetKind, TargetID, Outcome, ErrorCode, RequestID string; Details map[string]string; CreatedAt time.Time }`
  - `type store.RowQuerier interface{ QueryRow(ctx context.Context, sql string, args ...any) pgx.Row }`
  - `func store.InsertAudit(ctx context.Context, q store.RowQuerier, r store.AuditRecord) (int64, error)`
  - `func (d *store.DB) AmendAudit(ctx context.Context, id int64, targetID, outcome, errorCode string, details map[string]string) error` (the target is amended too: a create handler learns its target id only after the transaction committed)
  - `type store.AuditFilter struct{ Tenant, Action, ActorRef, TargetID string; Since, Until *time.Time }`
  - `type store.AuditCursor struct{ CreatedAt time.Time; ID int64 }`
  - `func (d *store.DB) ListAudit(ctx context.Context, f store.AuditFilter, limit int, after *store.AuditCursor) ([]store.AuditRecord, error)`. It returns up to `limit+1` rows, newest first.
  - `func (d *store.DB) PruneAudit(ctx context.Context, before time.Time, batch int64) (int64, error)`
  - `type store.AuditStore`, `func store.NewAuditStore(db *store.DB) *store.AuditStore`, `func (s *store.AuditStore) InsertAudit(ctx context.Context, tx store.Tx, r store.AuditRecord) (int64, error)` (nil tx = pool), `func (s *store.AuditStore) AmendAudit(...)` (same signature as on DB)
  - `type store.TxHook interface{ PreCommit(ctx context.Context, tx store.Tx) error; Committed() }`, `func store.ContextWithTxHook(ctx context.Context, h store.TxHook) context.Context`, `func store.TxHookFromContext(ctx context.Context) store.TxHook`

- [ ] **Step 1: Baseline: outcome vocabulary and the action index**

In `0001_baseline.sql`, change the `audit_event.outcome` CHECK to `CHECK (outcome IN ('success','denied','failure'))`. After the two existing `audit_event` indexes, add:

```sql
CREATE INDEX IF NOT EXISTS audit_event_tenant_action_idx ON audit_event (tenant, action, created_at DESC, id DESC);
```

Regenerate the snapshot: `TCDI_UPDATE_SNAPSHOT=1 go test ./internal/store/ -run TestBaseline_SchemaMatchesSnapshot -count=1`. Then run `go test ./internal/store/ -run TestBaseline -count=1` without the variable. Expected: PASS (needs Docker; the snapshot diff shows the new CHECK and the new index only).

- [ ] **Step 2: Write the failing store tests**

`internal/store/audit_test.go` (package `store_test`, uses `newDB(t)` from `pg_test.go`):

```go
package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func TestAudit_InsertListFilterCursor(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := store.InsertAudit(ctx, db.Pool(), store.AuditRecord{
			Tenant: "t1", ActorRef: "oidc:aa", Action: "workspace.start", TargetKind: "workspace",
			TargetID: fmt.Sprintf("ws_%d", i), Outcome: "success", RequestID: "r", Details: map[string]string{"k": "v"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.InsertAudit(ctx, db.Pool(), store.AuditRecord{Tenant: "t2", ActorRef: "oidc:bb", Action: "workspace.stop", Outcome: "denied"}); err != nil {
		t.Fatal(err)
	}
	first, err := db.ListAudit(ctx, store.AuditFilter{Tenant: "t1"}, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 || first[0].TargetID != "ws_4" || first[1].TargetID != "ws_3" {
		t.Fatalf("first page = %+v, want ws_4, ws_3 (+1 lookahead)", first)
	}
	if first[0].Details["k"] != "v" || first[0].Outcome != "success" {
		t.Fatalf("round trip lost fields: %+v", first[0])
	}
	second, err := db.ListAudit(ctx, store.AuditFilter{Tenant: "t1"}, 2, &store.AuditCursor{CreatedAt: first[1].CreatedAt, ID: first[1].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 3 || second[0].TargetID != "ws_2" || second[2].TargetID != "ws_0" {
		t.Fatalf("second page = %+v, want ws_2, ws_1, ws_0", second)
	}
	only, _ := db.ListAudit(ctx, store.AuditFilter{Tenant: "t1", TargetID: "ws_3"}, 10, nil)
	if len(only) != 1 {
		t.Fatalf("target filter = %d rows, want 1", len(only))
	}
	byActor, _ := db.ListAudit(ctx, store.AuditFilter{Tenant: "t2", ActorRef: "oidc:bb", Action: "workspace.stop"}, 10, nil)
	if len(byActor) != 1 || byActor[0].Outcome != "denied" {
		t.Fatalf("tenant isolation / actor+action filter: %+v", byActor)
	}
	future := time.Now().Add(time.Hour)
	none, _ := db.ListAudit(ctx, store.AuditFilter{Tenant: "t1", Since: &future}, 10, nil)
	if len(none) != 0 {
		t.Fatalf("since filter leaked %d rows", len(none))
	}
}

func TestAudit_OutcomeVocabulary(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	if _, err := store.InsertAudit(ctx, db.Pool(), store.AuditRecord{Tenant: "t", ActorRef: "a", Action: "x", Outcome: "failure"}); err != nil {
		t.Fatalf("failure outcome refused: %v", err)
	}
	if _, err := store.InsertAudit(ctx, db.Pool(), store.AuditRecord{Tenant: "t", ActorRef: "a", Action: "x", Outcome: "error"}); err == nil {
		t.Fatal("outcome outside the observability vocabulary accepted")
	}
}

func TestAudit_AmendAndPrune(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	var ids []int64
	for _, target := range []string{"old-a", "old-b", "new"} {
		id, err := store.InsertAudit(ctx, db.Pool(), store.AuditRecord{Tenant: "t", ActorRef: "a", Action: "x", TargetID: target, Outcome: "success"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := db.AmendAudit(ctx, ids[2], "ws_late", "failure", "QUOTA_EXHAUSTED", map[string]string{"counts": "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE audit_event SET created_at = now() - interval '100 days' WHERE target_id LIKE 'old-%'`); err != nil {
		t.Fatal(err)
	}
	n, err := db.PruneAudit(ctx, time.Now().Add(-90*24*time.Hour), 1)
	if err != nil || n != 2 {
		t.Fatalf("PruneAudit = %d, %v; want 2 rows over batch size 1", n, err)
	}
	left, _ := db.ListAudit(ctx, store.AuditFilter{Tenant: "t"}, 10, nil)
	if len(left) != 1 || left[0].Outcome != "failure" || left[0].ErrorCode != "QUOTA_EXHAUSTED" || left[0].Details["counts"] != "1" || left[0].TargetID != "ws_late" {
		t.Fatalf("after amend+prune: %+v", left)
	}
}
```

`internal/store/txhook_test.go`:

```go
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

type insertHook struct {
	outcome   string
	pre       int
	committed bool
}

func (h *insertHook) PreCommit(ctx context.Context, tx store.Tx) error {
	h.pre++
	_, err := store.InsertAudit(ctx, tx, store.AuditRecord{Tenant: "t", ActorRef: "a", Action: "x", Outcome: h.outcome})
	return err
}

func (h *insertHook) Committed() { h.committed = true }

func TestWithTx_TxHookFailureRollsBackMutation(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	write := func(ctx context.Context, tenant string) error {
		return db.WithTx(ctx, func(tx store.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO tenant_user_limit_default (tenant_id, max_running) VALUES ($1, 1)`, tenant)
			return err
		})
	}
	ok := &insertHook{outcome: "success"}
	if err := write(store.ContextWithTxHook(ctx, ok), "ok"); err != nil {
		t.Fatal(err)
	}
	if !ok.committed {
		t.Fatal("Committed not called after COMMIT")
	}
	bad := &insertHook{outcome: "bogus"} // violates the outcome CHECK
	if err := write(store.ContextWithTxHook(ctx, bad), "bad"); err == nil {
		t.Fatal("a failing PreCommit must fail the transaction")
	}
	if bad.committed {
		t.Fatal("Committed called for a rolled-back transaction")
	}
	skipped := &insertHook{outcome: "success"}
	boom := errors.New("boom")
	if err := db.WithTx(store.ContextWithTxHook(ctx, skipped), func(store.Tx) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("body error = %v, want boom", err)
	}
	if skipped.pre != 0 || skipped.committed {
		t.Fatalf("hook ran for a failed body: pre=%d committed=%v", skipped.pre, skipped.committed)
	}
	var mut, audit int
	_ = db.Pool().QueryRow(ctx, `SELECT count(*) FROM tenant_user_limit_default WHERE tenant_id = 'bad'`).Scan(&mut)
	_ = db.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_event`).Scan(&audit)
	if mut != 0 || audit != 1 {
		t.Fatalf("mutation rows=%d audit rows=%d; want 0 and 1 (fail closed)", mut, audit)
	}
	if err := write(ctx, "nohook"); err != nil {
		t.Fatalf("WithTx without a hook: %v", err)
	}
}
```

Run `go test ./internal/store/ -run 'TestAudit_|TestWithTx_TxHook' -count=1`. Expected: FAIL to compile (`store.InsertAudit`, `store.ContextWithTxHook` undefined).

- [ ] **Step 3: Implement the store side**

`internal/store/txhook.go`:

```go
package store

import "context"

// TxHook runs inside every WithTx transaction whose context carries it.
// PreCommit runs after the transaction body succeeded and before COMMIT —
// an error rolls the whole transaction back; Committed runs after COMMIT
// succeeded. The API's audit wrapper uses it to write the audit row in the
// same transaction as the audited mutation (ADR 0011).
type TxHook interface {
	PreCommit(ctx context.Context, tx Tx) error
	Committed()
}

type txHookKey struct{}

// ContextWithTxHook returns ctx carrying h.
func ContextWithTxHook(ctx context.Context, h TxHook) context.Context {
	return context.WithValue(ctx, txHookKey{}, h)
}

// TxHookFromContext returns the hook ctx carries, or nil.
func TxHookFromContext(ctx context.Context) TxHook {
	h, _ := ctx.Value(txHookKey{}).(TxHook)
	return h
}
```

In `WithTx` (`store.go`), replace the tail after `if err = fn(tx); err != nil { return err }` with:

```go
	hook := TxHookFromContext(ctx)
	if hook != nil {
		if err = hook.PreCommit(ctx, tx); err != nil {
			return err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit %w", err)
	}
	if hook != nil {
		hook.Committed()
	}
	return nil
```

`PreCommit` errors pass through unchanged so callers can `errors.Is` them; the deferred rollback already runs for any non-nil `err`.

`internal/store/audit.go`:

```go
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AuditRecord is one row of audit_event: a domain audit event persisted
// next to its stdout record (ADR 0011). ActorRef is already pseudonymous.
type AuditRecord struct {
	ID         int64
	Tenant     string
	ActorRef   string
	Action     string
	TargetKind string
	TargetID   string
	Outcome    string
	ErrorCode  string
	RequestID  string
	Details    map[string]string
	CreatedAt  time.Time
}

// RowQuerier is satisfied by *pgxpool.Pool and pgx.Tx.
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func detailsJSON(d map[string]string) (string, error) {
	if d == nil {
		d = map[string]string{}
	}
	b, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("store: audit details %w", err)
	}
	return string(b), nil
}

// InsertAudit writes r through q (the pool, or the audited mutation's
// transaction) and returns the row id.
func InsertAudit(ctx context.Context, q RowQuerier, r AuditRecord) (int64, error) {
	details, err := detailsJSON(r.Details)
	if err != nil {
		return 0, err
	}
	var id int64
	if err := q.QueryRow(ctx, `
		INSERT INTO audit_event (tenant, actor_ref, action, target_kind, target_id, outcome, error_code, request_id, details)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb)
		RETURNING id`,
		r.Tenant, r.ActorRef, r.Action, r.TargetKind, r.TargetID, r.Outcome, r.ErrorCode, r.RequestID, details,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: insert audit %w", err)
	}
	return id, nil
}

// AmendAudit rewrites the target, outcome, error code and details of a row
// inserted inside the audited mutation's transaction: the handler may refuse
// after COMMIT, and a create handler learns its target id only afterwards.
func (d *DB) AmendAudit(ctx context.Context, id int64, targetID, outcome, errorCode string, details map[string]string) error {
	raw, err := detailsJSON(details)
	if err != nil {
		return err
	}
	if _, err := d.pool.Exec(ctx,
		`UPDATE audit_event SET target_id = $2, outcome = $3, error_code = $4, details = $5::jsonb WHERE id = $1`,
		id, targetID, outcome, errorCode, raw); err != nil {
		return fmt.Errorf("store: amend audit %w", err)
	}
	return nil
}

// AuditFilter narrows ListAudit; empty fields do not filter. Since is
// inclusive, Until exclusive.
type AuditFilter struct {
	Tenant   string
	Action   string
	ActorRef string
	TargetID string
	Since    *time.Time
	Until    *time.Time
}

// AuditCursor is the keyset position (created_at, id) of the last row of
// the previous page; rows strictly older follow.
type AuditCursor struct {
	CreatedAt time.Time
	ID        int64
}

// ListAudit returns up to limit+1 rows of f.Tenant, newest first; the
// extra row tells the caller another page exists.
func (d *DB) ListAudit(ctx context.Context, f AuditFilter, limit int, after *AuditCursor) ([]AuditRecord, error) {
	args := []any{f.Tenant}
	where := "tenant = $1"
	add := func(cond string, v any) {
		args = append(args, v)
		where += fmt.Sprintf(" AND "+cond, len(args))
	}
	if f.Action != "" {
		add("action = $%d", f.Action)
	}
	if f.ActorRef != "" {
		add("actor_ref = $%d", f.ActorRef)
	}
	if f.TargetID != "" {
		add("target_id = $%d", f.TargetID)
	}
	if f.Since != nil {
		add("created_at >= $%d", *f.Since)
	}
	if f.Until != nil {
		add("created_at < $%d", *f.Until)
	}
	if after != nil {
		args = append(args, after.CreatedAt, after.ID)
		where += fmt.Sprintf(" AND (created_at, id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, limit+1)
	rows, err := d.pool.Query(ctx, `
		SELECT id, tenant, actor_ref, action, target_kind, target_id, outcome, error_code, request_id, details, created_at
		FROM audit_event WHERE `+where+fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("store: list audit %w", err)
	}
	defer rows.Close()
	var out []AuditRecord
	for rows.Next() {
		var r AuditRecord
		var raw []byte
		if err := rows.Scan(&r.ID, &r.Tenant, &r.ActorRef, &r.Action, &r.TargetKind, &r.TargetID,
			&r.Outcome, &r.ErrorCode, &r.RequestID, &raw, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan audit %w", err)
		}
		if err := json.Unmarshal(raw, &r.Details); err != nil {
			return nil, fmt.Errorf("store: audit details %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneAudit deletes rows created before `before`, batch rows per
// statement so one prune never holds a long lock.
func (d *DB) PruneAudit(ctx context.Context, before time.Time, batch int64) (int64, error) {
	var total int64
	for {
		tag, err := d.pool.Exec(ctx, `
			DELETE FROM audit_event WHERE id IN (
				SELECT id FROM audit_event WHERE created_at < $1 ORDER BY created_at LIMIT $2)`, before, batch)
		if err != nil {
			return total, fmt.Errorf("store: prune audit %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < batch {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}

// AuditStore adapts the DB to the API's durable audit sink.
type AuditStore struct{ db *DB }

// NewAuditStore wraps db.
func NewAuditStore(db *DB) *AuditStore { return &AuditStore{db: db} }

// InsertAudit inserts through tx, or through the pool when tx is nil.
func (s *AuditStore) InsertAudit(ctx context.Context, tx Tx, r AuditRecord) (int64, error) {
	if tx != nil {
		return InsertAudit(ctx, tx, r)
	}
	return InsertAudit(ctx, s.db.pool, r)
}

// AmendAudit forwards to DB.AmendAudit.
func (s *AuditStore) AmendAudit(ctx context.Context, id int64, targetID, outcome, errorCode string, details map[string]string) error {
	return s.db.AmendAudit(ctx, id, targetID, outcome, errorCode, details)
}
```

Run the Step 2 command. Expected: PASS.

- [ ] **Step 4: Run the store package and commit**

```bash
go build ./... && go vet ./internal/store/ && go test -count=1 ./internal/store/
git add internal/store
git commit -m "feat(store): audit_event persistence, keyset listing, prune and the WithTx pre-commit hook

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

Expected: PASS (with Docker running; a skipped Postgres test is not a pass).

---

### Task 1a: Fail-closed `audited()`, durable sink, 90-day retention (F6, part 1b) — tier: design

**Files:**
- Create: `internal/api/auditstore.go`, `internal/api/auditstore_test.go`
- Modify: `internal/api/auditroutes.go:171-243` (`audited` and its doc comment) and `:258-267` (`lateAuditSink`)
- Modify: `internal/backend/wire.go:182` (`appAudit`) and the singleton registration (413-434), `internal/backend/retention.go`, `internal/backend/retention_test.go` (create), `internal/backend/config.go` (field at 127, flag at 284, validation at 423), `internal/backend/config_test.go`
- Modify: `internal/observability/metrics.go:113` (`retentionTables`)
- Modify: `deploy/helm/tinycdi/values.yaml:370`, `deploy/helm/tinycdi/values.schema.json:202-208`, `deploy/helm/tinycdi/templates/backend.yaml:249`, `deploy/helm/tinycdi/README.md:239`, `deploy/helm/chart_retention_test.go`, `deploy/helm/testdata/default-render.golden.yaml` (regenerated)

**Interfaces:**
- Consumes (Task 1): `store.AuditRecord`, `store.AuditStore`, `store.NewAuditStore`, `store.TxHook`, `store.ContextWithTxHook`, `store.TxHookFromContext`, `store.DB.PruneAudit`. Existing: `observability.AuditEvent`, `observability.AuditSink`, `observability.RedactDetails`, `auditedRoutes`, `routeAudit`, `lateAuditSink`.
- Produces (used by Tasks 2, 4, 8):
  - `type api.DurableAuditSink`, `func api.NewDurableAuditSink(stdout observability.AuditSink, db api.AuditStore) *api.DurableAuditSink`, where `type api.AuditStore interface{ InsertAudit(ctx context.Context, tx store.Tx, r store.AuditRecord) (int64, error); AmendAudit(ctx context.Context, id int64, targetID, outcome, errorCode string, details map[string]string) error }`
  - `func auditTargetKind(action string) string` (unexported, `internal/api`)
  - `lateAuditSink` forwards `Persists`/`PersistAudit`/`AmendAudit` to the sink it resolves, so `session.revoke_all` is persisted too.
  - Backend flag `-audit-retention-days` / env `TCDI_AUDIT_RETENTION_DAYS` (default 90, range 1..3650). Chart value `backend.retention.auditDays`.

- [ ] **Step 1: Write the failing `audited()` tests**

`internal/api/auditstore_test.go`:

```go
package api

import (
	"context"
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

type fakeDurable struct {
	mu          sync.Mutex
	stdout      []observability.AuditEvent
	persisted   []observability.AuditEvent
	inTx        int
	amended     []observability.AuditEvent
	failPersist error
}

func (f *fakeDurable) WriteAudit(_ context.Context, e observability.AuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stdout = append(f.stdout, e)
	return nil
}
func (f *fakeDurable) Persists(action string) bool { return action != auditActionSessionTouch }
func (f *fakeDurable) PersistAudit(_ context.Context, tx store.Tx, e observability.AuditEvent) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPersist != nil {
		return 0, f.failPersist
	}
	f.persisted = append(f.persisted, e)
	if tx != nil {
		f.inTx++
	}
	return int64(len(f.persisted)), nil
}
func (f *fakeDurable) AmendAudit(_ context.Context, _ int64, e observability.AuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.amended = append(f.amended, e)
	return nil
}

func TestAudited_PersistFailureFailsClosed(t *testing.T) {
	sink := &fakeDurable{failPersist: errors.New("db down")}
	h := audited(sink, routeAdminQuotaGet, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"x"`)
		respondJSON(w, map[string]string{"quota": "should-not-leak"})
	}))
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Request-Id", "req-1") // set by an outer middleware before audited()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/tenants/t/quota", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when the audit row cannot be stored", rec.Code)
	}
	if rec.Header().Get("ETag") != "" || rec.Header().Get("X-Request-Id") != "req-1" {
		t.Fatalf("handler headers leaked or outer headers lost: %v", rec.Header())
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Error.Code != "INTERNAL" {
		t.Fatalf("body is not the INTERNAL error envelope (no handler content): %s", rec.Body.String())
	}
	if len(sink.stdout) != 1 || sink.stdout[0].Outcome != observability.OutcomeFailure || sink.stdout[0].ErrorCode != "INTERNAL" {
		t.Fatalf("stdout records = %+v, want one failure/INTERNAL record (stdout survives an audit-store outage)", sink.stdout)
	}
}

func TestAudited_NoTxMutationPersistFailureFailsClosed(t *testing.T) {
	sink := &fakeDurable{failPersist: errors.New("db down")}
	h := audited(sink, routeAdminQuotaSet, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, map[string]string{"quota": "should-not-leak"}) // a K8s-only or single-statement mutation
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/admin/tenants/t/quota", nil))
	if rec.Code != http.StatusInternalServerError || bytes.Contains(rec.Body.Bytes(), []byte("should-not-leak")) {
		t.Fatalf("code=%d body=%s; want 500 and no handler body", rec.Code, rec.Body.String())
	}
}

func TestAudited_TxHookPersistsOnceThenAmends(t *testing.T) {
	sink := &fakeDurable{}
	var fakeTx store.Tx = struct{ store.Tx }{}
	h := audited(sink, routeAdminQuotaSet, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hook := store.TxHookFromContext(r.Context())
		if hook == nil {
			t.Fatal("a mutation must carry the audit tx hook")
		}
		for i := 0; i < 2; i++ { // two committed transactions in one request
			if err := hook.PreCommit(r.Context(), fakeTx); err != nil {
				t.Fatal(err)
			}
			hook.Committed()
		}
		auditSetTarget(r.Context(), "ws_late") // learned after the transactions committed
		writeError(w, r, CodeQuotaExhausted, "late refusal")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/admin/tenants/t/quota", nil))
	if len(sink.persisted) != 1 || sink.inTx != 1 {
		t.Fatalf("persisted=%d inTx=%d, want exactly one in-transaction row", len(sink.persisted), sink.inTx)
	}
	if len(sink.amended) != 1 || sink.amended[0].Outcome != observability.OutcomeFailure ||
		sink.amended[0].ErrorCode != "QUOTA_EXHAUSTED" || sink.amended[0].TargetUID != "ws_late" {
		t.Fatalf("amend = %+v, want the final failure outcome and the late target", sink.amended)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want the handler's 409", rec.Code)
	}
}

func TestAudited_PanicAfterCommitAmendsFailure(t *testing.T) {
	sink := &fakeDurable{}
	var fakeTx store.Tx = struct{ store.Tx }{}
	h := audited(sink, routeAdminQuotaSet, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hook := store.TxHookFromContext(r.Context())
		if err := hook.PreCommit(r.Context(), fakeTx); err != nil {
			t.Fatal(err)
		}
		hook.Committed()
		panic("boom")
	}))
	func() {
		defer func() { _ = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/v1/admin/tenants/t/quota", nil))
	}()
	if len(sink.amended) != 1 || sink.amended[0].Outcome != observability.OutcomeFailure || sink.amended[0].ErrorCode != "panic" {
		t.Fatalf("amend = %+v, want failure/panic for the committed row", sink.amended)
	}
}

type plainSink struct{ n int }

func (p *plainSink) WriteAudit(context.Context, observability.AuditEvent) error { p.n++; return nil }

func TestAudited_PlainSinkDoesNotBuffer(t *testing.T) {
	sink := &plainSink{}
	rec := httptest.NewRecorder()
	h := audited(sink, routeAdminQuotaGet, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("x"))
		if rec.Body.Len() != 1 {
			t.Fatal("a sink without persistence must not hold the response back")
		}
	}))
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/tenants/t/quota", nil))
	if sink.n != 1 {
		t.Fatalf("stdout records = %d, want 1", sink.n)
	}
}

func TestLateAuditSink_ForwardsPersistence(t *testing.T) {
	inner := &fakeDurable{}
	var late observability.AuditSink = lateAuditSink{func() observability.AuditSink { return inner }}
	d, ok := late.(durableAudit)
	if !ok || !d.Persists(auditActionSessionRevokeAll) || d.Persists(auditActionSessionTouch) {
		t.Fatalf("lateAuditSink must expose the inner sink's persistence (ok=%v)", ok)
	}
	if _, err := d.PersistAudit(context.Background(), nil, observability.AuditEvent{Action: auditActionSessionRevokeAll}); err != nil || len(inner.persisted) != 1 {
		t.Fatalf("PersistAudit not forwarded: err=%v persisted=%d", err, len(inner.persisted))
	}
	none := lateAuditSink{func() observability.AuditSink { return &plainSink{} }}
	if none.Persists(auditActionSessionRevokeAll) {
		t.Fatal("a plain inner sink persists nothing")
	}
}

func TestAudited_NoTxPathPersistsAfterHandler(t *testing.T) {
	sink := &fakeDurable{}
	h := audited(sink, routeAdminQuotaGet, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if store.TxHookFromContext(r.Context()) != nil {
			t.Fatal("reads carry no tx hook")
		}
		respondJSON(w, map[string]string{"ok": "1"})
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/tenants/t/quota", nil))
	if rec.Code != http.StatusOK || len(sink.persisted) != 1 || sink.inTx != 0 {
		t.Fatalf("code=%d persisted=%d inTx=%d", rec.Code, len(sink.persisted), sink.inTx)
	}
}

func TestAudited_TouchStaysStdoutOnly(t *testing.T) {
	sink := &fakeDurable{}
	h := audited(sink, routeSessionTouch, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if store.TxHookFromContext(r.Context()) != nil {
			t.Fatal("session.touch is not persisted, so it carries no hook")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/session:touch", nil))
	if len(sink.persisted) != 0 || len(sink.stdout) != 1 {
		t.Fatalf("persisted=%d stdout=%d", len(sink.persisted), len(sink.stdout))
	}
}

func TestAuditTargetKind(t *testing.T) {
	for action, want := range map[string]string{
		"workspace.start": "workspace", "connection.create": "workspace", "file.transfer.upload": "workspace",
		"data.purge": "data", "admin.quota.set": "tenant", "session.revoke_all": "session", "other": "",
	} {
		if got := auditTargetKind(action); got != want {
			t.Errorf("auditTargetKind(%q) = %q, want %q", action, got, want)
		}
	}
}
```

Run `go test ./internal/api/ -run 'TestAudited_|TestAuditTargetKind' -count=1`. Expected: FAIL to compile.

- [ ] **Step 2: Implement the durable sink and the new `audited()`**

`internal/api/auditstore.go`:

```go
package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// AuditStore is the Postgres surface of DurableAuditSink
// (store.AuditStore). tx nil inserts through the pool.
type AuditStore interface {
	InsertAudit(ctx context.Context, tx store.Tx, r store.AuditRecord) (int64, error)
	AmendAudit(ctx context.Context, id int64, targetID, outcome, errorCode string, details map[string]string) error
}

// durableAudit is the extra surface audited() looks for on its sink.
type durableAudit interface {
	observability.AuditSink
	Persists(action string) bool
	PersistAudit(ctx context.Context, tx store.Tx, e observability.AuditEvent) (int64, error)
	AmendAudit(ctx context.Context, id int64, e observability.AuditEvent) error
}

// notPersisted are actions that stay stdout-only: session.touch is the
// portal's activity beat and changes no state an administrator audits.
var notPersisted = map[string]bool{auditActionSessionTouch: true}

// DurableAuditSink writes every event to the stdout sink and, through
// audited(), persists domain events to audit_event (ADR 0011). Plain
// WriteAudit callers (the authenticator's revoke records, config apply)
// stay stdout-only.
type DurableAuditSink struct {
	stdout observability.AuditSink
	db     AuditStore
}

// NewDurableAuditSink wires the stdout sink (normally a GuardedSink) and
// the store.
func NewDurableAuditSink(stdout observability.AuditSink, db AuditStore) *DurableAuditSink {
	return &DurableAuditSink{stdout: stdout, db: db}
}

func (s *DurableAuditSink) WriteAudit(ctx context.Context, e observability.AuditEvent) error {
	return s.stdout.WriteAudit(ctx, e)
}

func (s *DurableAuditSink) Persists(action string) bool { return !notPersisted[action] }

func (s *DurableAuditSink) PersistAudit(ctx context.Context, tx store.Tx, e observability.AuditEvent) (int64, error) {
	return s.db.InsertAudit(ctx, tx, auditRecordOf(e))
}

func (s *DurableAuditSink) AmendAudit(ctx context.Context, id int64, e observability.AuditEvent) error {
	r := auditRecordOf(e)
	return s.db.AmendAudit(ctx, id, r.TargetID, r.Outcome, r.ErrorCode, r.Details)
}

func auditRecordOf(e observability.AuditEvent) store.AuditRecord {
	return store.AuditRecord{
		Tenant: e.Tenant, ActorRef: e.Actor, Action: e.Action,
		TargetKind: auditTargetKind(e.Action), TargetID: e.TargetUID,
		Outcome: string(e.Outcome), ErrorCode: e.ErrorCode, RequestID: e.RequestID,
		Details: observability.RedactDetails(e.Details),
	}
}

// auditTargetKind names what TargetUID identifies, derived from the
// action's namespace.
func auditTargetKind(action string) string {
	switch {
	case strings.HasPrefix(action, "workspace."), strings.HasPrefix(action, "connection."),
		strings.HasPrefix(action, "file.transfer."):
		return "workspace"
	case strings.HasPrefix(action, "data."):
		return "data"
	case strings.HasPrefix(action, "admin."):
		return "tenant"
	case strings.HasPrefix(action, "session."):
		return "session"
	}
	return ""
}

// auditTxHook persists the route's audit row inside the first
// transaction of the request that commits (store.TxHook).
type auditTxHook struct {
	sink  durableAudit
	build func() observability.AuditEvent

	mu        sync.Mutex
	pending   int64
	committed int64
}

func (h *auditTxHook) PreCommit(ctx context.Context, tx store.Tx) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.committed != 0 {
		return nil
	}
	id, err := h.sink.PersistAudit(ctx, tx, h.build())
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	h.pending = id
	return nil
}

func (h *auditTxHook) Committed() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.committed == 0 {
		h.committed = h.pending
	}
}

func (h *auditTxHook) committedID() int64 {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.committed
}

// bufferedResponse holds the handler's status and body until the audit
// row is stored. Headers go to the real writer's map; base is that map as
// it was before the handler ran, so a fail-closed answer can restore it.
// It deliberately implements neither http.Flusher nor http.Hijacker and has
// no Unwrap: a streaming or upgrading handler must not be mounted under a
// durable audited() route (a ResponseController call on it fails loudly
// instead of releasing bytes before the row is stored). No audited route
// streams today.
type bufferedResponse struct {
	w      http.ResponseWriter
	base   http.Header
	status int
	body   []byte
}

func newBufferedResponse(w http.ResponseWriter) *bufferedResponse {
	return &bufferedResponse{w: w, base: w.Header().Clone()}
}

func (b *bufferedResponse) Header() http.Header { return b.w.Header() }
func (b *bufferedResponse) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}
func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	b.body = append(b.body, p...)
	return len(p), nil
}
func (b *bufferedResponse) flush() {
	b.w.WriteHeader(statusOrOK(b.status))
	if len(b.body) > 0 {
		_, _ = b.w.Write(b.body)
	}
}

// discardForError drops everything the handler produced (body and every
// header it set) so the fail-closed error envelope is the whole response.
func (b *bufferedResponse) discardForError() {
	h := b.w.Header()
	for k := range h {
		delete(h, k)
	}
	for k, v := range b.base {
		h[k] = v
	}
	b.body = nil
}
```

In `auditroutes.go`, replace the body of `audited` (keep its doc comment, then append one paragraph: "With a durable sink (DurableAuditSink) the event is also persisted: inside the mutation's transaction through a store.TxHook, else before the buffered response is released; a failed insert answers 500 INTERNAL and nothing the handler wrote reaches the client (ADR 0011). A sink without persistence (and session.touch) keeps the pass-through status recorder."):

```go
func audited(sink observability.AuditSink, pattern string, next http.Handler) http.Handler {
	rt, ok := auditedRoutes[pattern]
	if !ok {
		panic("audited: route " + pattern + " missing from auditedRoutes")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sink == nil {
			next.ServeHTTP(w, r)
			return
		}
		ra := &routeAudit{}
		switch rt.targetKey {
		case "":
		case actionTarget:
			ra.target = ActionID(r)
		default:
			ra.target = r.PathValue(rt.targetKey)
		}
		ctx := context.WithValue(r.Context(), ctxKeyRouteAudit, ra)
		durable, _ := sink.(durableAudit)
		if durable != nil && !durable.Persists(rt.action) {
			durable = nil
		}
		event := func(status int, completed bool) observability.AuditEvent {
			var actor, tenant string
			if p, ok := PrincipalFromContext(ctx); ok {
				actor = observability.ActorRef(p.Issuer, p.Subject)
				tenant = p.TenantID
				if p.IsTenantAdmin() {
					auditSetDetail(ctx, "role", TenantAdminGroup)
				}
			}
			outcome := outcomeFor(statusOrOK(status))
			errCode := ra.errCode
			if !completed {
				outcome = observability.OutcomeFailure
				if errCode == "" {
					errCode = "panic"
				}
			}
			action := rt.action
			if ra.action != "" {
				action = ra.action
			}
			return observability.AuditEvent{
				Actor: actorOrAnonymous(actor), Action: action, TargetUID: ra.target, Tenant: tenant,
				RequestID: RequestIDFromContext(ctx), Outcome: outcome, ErrorCode: errCode, Details: ra.details,
			}
		}
		var hook *auditTxHook // nil for reads and non-persisted routes
		if durable != nil && r.Method != http.MethodGet {
			hook = &auditTxHook{sink: durable, build: func() observability.AuditEvent { return event(http.StatusOK, true) }}
			ctx = store.ContextWithTxHook(ctx, hook)
		}
		r = r.WithContext(ctx)

		// persist stores or amends the row for the final event e. It runs on a
		// context detached from the client connection: a hung-up client must
		// not turn a committed mutation into a lost or stale audit row.
		persist := func(e observability.AuditEvent) error {
			pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditPersistTimeout)
			defer cancel()
			if id := hook.committedID(); id != 0 {
				// The row committed with the mutation; the amend records the
				// final target and outcome. A failed amend leaves the
				// committed row as inserted.
				_ = durable.AmendAudit(pctx, id, e)
				return nil
			}
			_, err := durable.PersistAudit(pctx, nil, e)
			return err
		}

		var rw http.ResponseWriter
		var buf *bufferedResponse
		var rec *statusRecorder
		if durable != nil {
			buf = newBufferedResponse(w)
			rw = buf
		} else {
			rec = &statusRecorder{ResponseWriter: w}
			rw = rec
		}
		completed := false
		defer func() {
			if completed {
				return
			}
			e := event(0, false)
			if durable != nil {
				_ = persist(e)
			}
			_ = sink.WriteAudit(ctx, e)
		}()
		next.ServeHTTP(rw, r)
		completed = true
		var status int
		if buf != nil {
			status = buf.status
		} else {
			status = rec.status
		}
		e := event(status, true)
		var persistErr error
		if durable != nil {
			persistErr = persist(e)
			if persistErr != nil {
				e.Outcome, e.ErrorCode = observability.OutcomeFailure, string(CodeInternal)
			}
		}
		_ = sink.WriteAudit(ctx, e) // stdout is written even when the store is down
		if persistErr != nil {
			buf.discardForError()
			WriteError(w, r, NewError(CodeInternal, "audit record could not be stored"))
			return
		}
		if buf != nil {
			buf.flush()
		}
	})
}
```

Add next to the other `auditroutes.go` constants: `const auditPersistTimeout = 5 * time.Second` (import `time`). Add the `store` import too.

`PrincipalFromContext(ctx)` must see the principal that `RequireAuth` placed in the request context *before* `audited` runs. That is today's mount order, so `ctx` derives from it. `statusRecorder` stays (it is used by `auth.go:508` and `middleware.go`).

Make `lateAuditSink` (`auditroutes.go:258`) forward persistence, or `session.revoke_all` (mounted through it, `MountRevokeAllRoute`) would silently stay stdout-only because the `sink.(durableAudit)` assertion in `audited` fails on the wrapper:

```go
func (s lateAuditSink) durable() durableAudit {
	d, _ := s.get().(durableAudit)
	return d
}

func (s lateAuditSink) Persists(action string) bool {
	d := s.durable()
	return d != nil && d.Persists(action)
}

func (s lateAuditSink) PersistAudit(ctx context.Context, tx store.Tx, e observability.AuditEvent) (int64, error) {
	d := s.durable()
	if d == nil {
		return 0, errors.New("audit: no durable sink attached")
	}
	return d.PersistAudit(ctx, tx, e)
}

func (s lateAuditSink) AmendAudit(ctx context.Context, id int64, e observability.AuditEvent) error {
	d := s.durable()
	if d == nil {
		return errors.New("audit: no durable sink attached")
	}
	return d.AmendAudit(ctx, id, e)
}
```

(`errors` import too.) `session.touch` goes through the same wrapper and is excluded by `Persists`.

Run the Step 1 command, then the whole package: `go test ./internal/api/ -count=1`. Expected: PASS. In particular `TestAuditCoverage_*` still emits exactly one event per route (its `captureSink` has no persistence, so those routes take the pass-through branch).

- [ ] **Step 3: Wire it: sink, retention flag, leader prune loop, metrics label**

In `internal/backend/wire.go:182`, replace the `appAudit` statement (182-183) with the snippet below. `db` is already open above it (`wire.go:155-165`), and `store` and `os` are already imported. `appAudit` keeps being passed as `observability.AuditSink` to `tenantQuotaSingleton`, `newAppHandler` and every `WithAuditSink(appAudit)`; `tenantQuotaSingleton`'s config-apply record stays stdout-only.

```go
	appAudit := api.NewDurableAuditSink(
		observability.NewGuardedSink(observability.NewJSONSink(os.Stdout), log, metrics.IncAuditWriteError),
		store.NewAuditStore(db))
```

In `config.go`:
- add `AuditRetentionDays int` next to `TerminalRetentionDays` (`config.go:127`);
- add, after the `terminal-retention-days` flag (`config.go:284-285`), `fs.IntVar(&c.AuditRetentionDays, "audit-retention-days", envInt(getenv, "TCDI_AUDIT_RETENTION_DAYS", 90), "days audit_event rows are kept; a daily leader task prunes older rows (env TCDI_AUDIT_RETENTION_DAYS)")`;
- next to the terminal-retention check (`config.go:423`), add the validation `if c.AuditRetentionDays < 1 || c.AuditRetentionDays > 3650 { return errors.New("-audit-retention-days must be 1..3650") }`.

In `retention.go` add:

```go
const (
	auditRetentionInterval = 24 * time.Hour
	auditRetentionBatch    = 1000
)

// auditRetentionLoop is the leader singleton that prunes audit_event rows
// older than days, once at election and then daily (ADR 0011).
func auditRetentionLoop(db *store.DB, days int, m *observability.Metrics, log *slog.Logger) func(ctx context.Context) {
	return func(ctx context.Context) {
		prune := func() {
			n, err := db.PruneAudit(ctx, time.Now().Add(-time.Duration(days)*24*time.Hour), auditRetentionBatch)
			switch {
			case err != nil && errors.Is(err, context.Canceled):
			case err != nil:
				log.Error("audit retention prune failed", "err", err)
			default:
				log.Info("audit retention prune", "rows_pruned", n)
				if m != nil {
					m.AddRetentionRowsPruned("audit_event", float64(n))
				}
			}
		}
		prune()
		ticker := time.NewTicker(auditRetentionInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				prune()
			}
		}
	}
}
```

Register it in `wire.go` after the `terminalRetentionSweepLoop` block (`wire.go:427-430`) and before `b.electSingletons` (`wire.go:434`), unconditionally (the range check in `config.go` means it is never disabled): `b.singletons = append(b.singletons, auditRetentionLoop(db, cfg.AuditRetentionDays, metrics, log))`. In `metrics.go:113`, add `"audit_event": {}` to `retentionTables` (the `retention_rows_pruned_total` family is seeded from that map, so the new label starts at 0).

Create `internal/backend/retention_test.go` (package `backend`; `newDB` is in `pg_test.go`):

```go
package backend

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestAuditRetentionLoop_PrunesOldRowsAtStart(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	for _, age := range []string{"100 days", "1 day"} {
		if _, err := db.Pool().Exec(ctx, `
			INSERT INTO audit_event (tenant, actor_ref, action, outcome, created_at)
			VALUES ('t', 'a', 'x', 'success', now() - $1::interval)`, age); err != nil {
			t.Fatal(err)
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		auditRetentionLoop(db, 90, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))(runCtx)
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		_ = db.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_event`).Scan(&n)
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit_event rows = %d after 10s, want 1 (the 100-day-old row pruned, the 1-day-old kept)", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
}
```

Add to `config_test.go`:

```go
func TestParseFlags_AuditRetentionDays(t *testing.T) {
	cfg, err := ParseFlags(mergedArgs(), noEnv)
	if err != nil || cfg.AuditRetentionDays != 90 {
		t.Fatalf("default = %d, %v; want 90", cfg.AuditRetentionDays, err)
	}
	cfg, err = ParseFlags(mergedArgs(), envMap(map[string]string{"TCDI_AUDIT_RETENTION_DAYS": "30"}))
	if err != nil || cfg.AuditRetentionDays != 30 {
		t.Fatalf("env = %d, %v; want 30", cfg.AuditRetentionDays, err)
	}
	for _, bad := range []string{"0", "-1", "3651"} {
		if _, err := ParseFlags(withArg(mergedArgs(), "-audit-retention-days", bad), noEnv); err == nil {
			t.Fatalf("-audit-retention-days=%s accepted", bad)
		}
	}
}
```

Run `go build ./... && go test ./internal/backend/ ./internal/observability/ -count=1`. Expected: PASS (the retention-loop test needs Docker).

- [ ] **Step 4: Chart value**

- `values.yaml`, under `backend.retention`, add `auditDays: 90` with a comment: "Days audit_event rows are kept (ADR 0011); a daily leader task prunes older rows. 1..3650."
- `values.schema.json`, under `backend.retention.properties`, add `"auditDays": {"type": "integer", "minimum": 1, "maximum": 3650}`.
- `templates/backend.yaml`, after the `-terminal-retention-days` arg (line 249), add a one-line comment (`# Audit retention (ADR 0011): the leader's daily prune.`) and `- -audit-retention-days={{ .Values.backend.retention.auditDays }}`.
- `README.md`, after the `backend.retention.terminalRowDays` row (line 239), add a row: `backend.retention.auditDays` | `90` | days an `audit_event` row is kept before the leader's daily prune deletes it (1..3650).

Append to `deploy/helm/chart_retention_test.go`:

```go
func TestAuditRetentionDefaultAndCustom(t *testing.T) {
	if args := backendArgsWith(t); !hasArg(args, "-audit-retention-days=90") {
		t.Errorf("default render lacks -audit-retention-days=90: %v", args)
	}
	if args := backendArgsWith(t, "backend.retention.auditDays=30"); !hasArg(args, "-audit-retention-days=30") {
		t.Errorf("backend.retention.auditDays=30 not rendered: %v", args)
	}
	out := renderErrArgs(t, "-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "backend.retention.auditDays=0")
	if !strings.Contains(out, "auditDays") && !strings.Contains(out, "minimum") {
		t.Errorf("auditDays=0 must be rejected by the schema, got: %s", out)
	}
}
```

The new arg changes the default render, so `TestDefaultRenderUnchangedGolden` fails until the golden is regenerated. Regenerate it with the command from `deploy/helm/chart_defaultrender_test.go`, then check that the diff is exactly the comment line and `-audit-retention-days=90`:

```bash
cd deploy/helm && helm template tcdi ./tinycdi --namespace tcdi-system --include-crds \
  -f tinycdi/ci/minimal-values.yaml > testdata/default-render.golden.yaml && cd ../.. \
  && git diff --stat deploy/helm/testdata/default-render.golden.yaml
```

Run `go test ./internal/backend/ -run 'TestParseFlags_AuditRetentionDays|TestAuditRetentionLoop' -count=1 && go test -count=1 ./deploy/helm/`. Expected: PASS (needs `helm` v4.1.3 on PATH).

- [ ] **Step 5: Full package run and commit**

```bash
go build ./... && go vet ./internal/... && go test -count=1 ./internal/store/ ./internal/api/ ./internal/backend/ ./internal/observability/ ./deploy/helm/
git add internal/api/auditstore.go internal/api/auditstore_test.go internal/api/auditroutes.go \
  internal/backend internal/observability/metrics.go deploy/helm
git commit -m "feat(audit): persist domain audit events fail-closed with 90-day retention

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

Expected: PASS (with Docker running).

---

### Task 2: Audit viewer API, `/admin/audit` page, ADR 0011, `appRoutes` wiring (F6, part 2) — tier: integrate

**Files:**
- Create: `internal/api/adminaudit.go`, `internal/api/adminaudit_test.go`
- Modify: `internal/api/auditroutes.go`, `internal/api/openapi.yaml`, `internal/observability/metrics.go:121`, `internal/api/auditroutes_test.go`, `internal/api/openapi_contract_test.go`, `internal/api/tenantadmin_test.go`
- Modify: `internal/backend/wire.go` (`appMux` → `appRoutes`), `internal/backend/backend_test.go:366`
- Create: `web/src/admin/AuditPage.tsx`, `web/tests/unit/admin/audit.test.tsx`
- Modify: `web/src/admin/{routes.tsx,AdminLayout.tsx,api.ts,admin.css}`, `web/src/i18n/{en,vi}/admin.ts`, `web/tests/mock-api/admin.ts`, `web/tests/admin.spec.ts`, `web/tests/a11y.spec.ts`, `web/src/api/generated/schema.d.ts` (generated)
- Create: `docs/adr/0011-audit-store-and-retention.md`. Modify: `docs/README.md` (ADR index), `docs/security/threat-model.md` (row S42)

**Interfaces:**
- Consumes: `store.DB.ListAudit`, `store.AuditFilter`, `store.AuditCursor`, `store.AuditRecord` (Task 1); `targetActorRef` (`adminuserlimit.go:406`), `validOwnerRef` (`adminuserlimit.go:414`), `tenantAdminPrincipal` (`adminquota.go:218`), `ParsePage`/`NextCursor`/`Page[T]` (`page.go`), `observability.ActorRef(issuer, subject)` (`audit.go:45`).
- Produces:
  - `GET /v1/admin/tenants/{tenant}/audit?action=&actor=&target=&since=&until=&limit=&cursor=&sort=-createdAt` returns `Page[AdminAuditEntry]`. The audit action is `admin.audit.get`.
  - `type AuditSource interface{ ListAudit(ctx context.Context, f store.AuditFilter, limit int, after *store.AuditCursor) ([]store.AuditRecord, error) }`, `func NewAdminAuditHandler(src AuditSource, t TenantResolver) *AdminAuditHandler`, `(*AdminAuditHandler).WithAuditSink(observability.AuditSink) *AdminAuditHandler`, `func MountAdminAuditRoutes(mux RouteMux, authn *Authenticator, h *AdminAuditHandler)`
  - `type appRoutes struct` in `internal/backend/wire.go`, with one field per API handler. `appMux(authn *api.Authenticator, h appRoutes, loginLimit, sessionLimit, callbackLimit func(http.Handler) http.Handler) *http.ServeMux`. Tasks 3, 4, 5, 8 and 11 add fields to it.
  - Web: `listAdminAuditPage(api: ApiClient, tenant: string, filter: AuditFilter, cursor?: string): Promise<ServerPage<AdminAuditEntry>>`, `type AuditFilter = { action?: string; actor?: string; target?: string; since?: string; until?: string }`; route `/admin/audit`.

- [ ] **Step 1: Refactor `appMux` to a struct (no behaviour change)**

In `wire.go`, define:

```go
// appRoutes carries every API handler appMux mounts. The first seven are
// mounted unconditionally, as before; a nil admin* field skips that
// resource's mount (unit tests mount a subset).
type appRoutes struct {
	ws                 *api.WorkspaceHandler
	tpl                *api.TemplateHandler
	conn               *api.ConnectionHandler
	me                 *api.MeHandler
	connStatus         *api.ConnectionStatusHandler
	data               *api.DataHandler
	quota              *api.QuotaHandler
	adminQuota         *api.AdminQuotaHandler
	adminUserLimits    *api.AdminUserLimitsHandler
	adminSessionPolicy *api.AdminSessionPolicyHandler
	adminAudit         *api.AdminAuditHandler
}
```

Change the signature at `wire.go:766` (14 positional parameters: `authn, ws, tpl, conn, me, connStatus, data, quota, adminQuota, adminUserLimits, adminSessionPolicy, loginLimit, sessionLimit, callbackLimit`) to `func appMux(authn *api.Authenticator, h appRoutes, loginLimit, sessionLimit, callbackLimit func(http.Handler) http.Handler) *http.ServeMux`. In the body, replace each positional parameter with its field: `api.MountMeRoutes(mux, authn, h.me)`, `api.MountWorkspaceRoutes(mux, authn, h.ws, h.tpl)`, `api.MountConnectionRoutes(mux, authn, h.conn)`, `api.MountConnectionStatusRoutes(mux, authn, h.connStatus)`, `api.MountDataRoutes(mux, authn, h.data)`, `api.MountQuotaRoutes(mux, authn, h.quota)`, and `if h.adminQuota != nil { ... }`, `if h.adminUserLimits != nil { ... }`, `if h.adminSessionPolicy != nil { ... }` for the three existing admin mounts. The two callers are the only ones in the repo (`rg -n 'appMux\(' --glob '*.go'`):

- `wire.go:755` becomes
  ```go
  mux := appMux(authn, appRoutes{
  	ws: wsHandler, tpl: tplHandler, conn: connHandler, me: meHandler, connStatus: connStatusHandler,
  	data: dataHandler, quota: quotaHandler, adminQuota: adminQuotaHandler,
  	adminUserLimits: adminUserLimitsHandler, adminSessionPolicy: adminSessionPolicyHandler,
  }, loginLimit, sessionLimit, callbackLimit)
  ```
- `backend_test.go:366` (`testAppHandler`) becomes `mux := appMux(authn, appRoutes{ws: ws, tpl: tpl, conn: conn, me: me, connStatus: connStatus, data: data, quota: quota}, pass, pass, pass)`.

Run `go build ./... && go test ./internal/backend/ -count=1`. Expected: PASS. Commit: `refactor(backend): appMux takes an appRoutes struct` (with the attribution line).

- [ ] **Step 2: Contract first**

In `openapi.yaml`, add the path `/v1/admin/tenants/{tenant}/audit` (GET):
- `operationId: listAdminAudit`, `summary: Audit log (paged)`, tag `admin`, a description that says: newest first, requires the tenant-admin role on the named tenant, the read is itself audited, retention is the platform setting (default 90 days), and `since` is inclusive and `until` exclusive;
- parameters: `$ref: "#/components/parameters/Tenant"`, then inline query parameters `action` (`string`, maxLength 128), `actor` (`string`, maxLength 512, described as "an actor reference `oidc:<hex>`/`anonymous`, or an `issuer|sub` owner reference, which is pseudonymized server-side"), `target` (`string`, maxLength 256), `since` and `until` (`string`, `format: date-time`), then the shared `$ref: "#/components/parameters/PageLimit"`, `Cursor` and `Sort` (`openapi.yaml:1436-1465`; the shared `Sort` has no enum, so the operation description states "Sort: `-createdAt` only (default)");
- responses, in the order the user-limits list uses (`openapi.yaml:858`): `200` → `AdminAuditPage`; `400` BadRequest, `401` Unauthorized, `403` Forbidden, `429` RateLimited, `500` InternalError, `503` Unavailable (each `$ref: "#/components/responses/<Name>"`). A GET declares no `428`.

Add the schemas:

```yaml
    AdminAuditEntry:
      type: object
      additionalProperties: false
      required: [id, createdAt, actor, action, targetKind, targetId, outcome, requestId, details]
      properties:
        id: {type: string}
        createdAt: {type: string, format: date-time}
        actor: {type: string, description: "Pseudonymous actor reference (oidc:<16 hex>) or anonymous."}
        action: {type: string}
        targetKind: {type: string, enum: ["", workspace, data, tenant, session], description: "What targetId identifies; derived from the action namespace (auditTargetKind)."}
        targetId: {type: string}
        outcome: {type: string, enum: [success, denied, failure]}
        errorCode: {type: string}
        requestId: {type: string}
        details: {type: object, additionalProperties: {type: string}}
    AdminAuditPage:
      type: object
      additionalProperties: false
      required: [items]
      properties:
        items: {type: array, items: {$ref: '#/components/schemas/AdminAuditEntry'}}
        nextCursor: {$ref: '#/components/schemas/NextCursor'}
```

Run `cd web && npm run generate`. Expected: `schema.d.ts` gains `AdminAuditEntry`, `AdminAuditPage` and `listAdminAudit`. `go test ./internal/api/ -run 'TestAuditCoverage_SpecMatchesTable|TestOpenAPI_EveryOperationMountedAndViceVersa' -count=1` now FAILS (the operation has no route yet); Step 4 turns it green.

- [ ] **Step 3: Failing handler tests**

`internal/api/adminaudit_test.go` imports `bytes`, `context`, `fmt`, `log/slog`, `net/http`, `net/http/httptest`, `net/url`, `strings`, `testing`, `time`, `internal/api/oidctest`, `internal/observability` and `internal/store`. It defines one shared env builder, which Task 3's `newAdminUsersEnv` reuses, and `newAdminAuditEnv` on top of it. The builder is `newSessionPolicyEnv` (`adminsessionpolicy_test.go:73`) with the mount abstracted:

```go
// newAdminMountEnv builds a testEnv whose mux carries /auth/login,
// /auth/callback and whatever mount adds, behind RequestID(Audit(logger)).
func newAdminMountEnv(t *testing.T, mount func(mux *http.ServeMux, a *Authenticator)) *testEnv {
	t.Helper()
	iss, err := oidctest.NewIssuer()
	if err != nil {
		t.Fatalf("oidctest.NewIssuer: %v", err)
	}
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, nil))
	sessions := NewInMemorySessionStore(30 * time.Minute)
	a, err := NewAuthenticator(context.Background(), AuthConfig{
		Issuer:      iss.URL(),
		ClientID:    iss.ClientID,
		RedirectURL: "https://portal.test/auth/callback",
		LoginSealer: testLoginSealer(t),
	}, sessions, logger)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/auth/login", http.HandlerFunc(a.LoginHandler))
	mux.Handle("/auth/callback", http.HandlerFunc(a.CallbackHandler))
	mount(mux, a)
	srv := httptest.NewServer(RequestID(Audit(logger)(mux)))
	env := &testEnv{issuer: iss, auth: a, store: sessions, server: srv, logs: logBuf}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return env
}

func newAdminAuditEnv(t *testing.T, src AuditSource) *testEnv {
	t.Helper()
	return newAdminMountEnv(t, func(mux *http.ServeMux, a *Authenticator) {
		MountAdminAuditRoutes(mux, a, NewAdminAuditHandler(src, defaultTenants()))
	})
}
```

```go
type fakeAuditSource struct {
	gotFilter store.AuditFilter
	gotAfter  *store.AuditCursor
	rows      []store.AuditRecord
}

func (f *fakeAuditSource) ListAudit(_ context.Context, fl store.AuditFilter, limit int, after *store.AuditCursor) ([]store.AuditRecord, error) {
	f.gotFilter, f.gotAfter = fl, after
	if len(f.rows) > limit+1 {
		return f.rows[:limit+1], nil
	}
	return f.rows, nil
}

func auditRows(n int) []store.AuditRecord {
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	out := make([]store.AuditRecord, n)
	for i := range out {
		out[i] = store.AuditRecord{ID: int64(n - i), Tenant: "tenant-a", ActorRef: "oidc:0011223344556677",
			Action: "workspace.start", TargetKind: "workspace", TargetID: fmt.Sprintf("ws_%d", i),
			Outcome: "success", RequestID: "req", Details: map[string]string{}, CreatedAt: base.Add(-time.Duration(i) * time.Minute)}
	}
	return out
}

func TestAdminAudit_ListPagesAndFilters(t *testing.T) {
	src := &fakeAuditSource{rows: auditRows(3)}
	env := newAdminAuditEnv(t, src)
	sess, csrf := loginAdmin(t, env, "admin-a")
	res := doReq(t, env, sess, csrf, http.MethodGet,
		"/v1/admin/tenants/tenant-a/audit?limit=2&action=workspace.start&actor=https%3A%2F%2Fidp%7Calice&since=2026-10-01T00:00:00Z", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	page := decodeBody[Page[adminAuditEntry]](t, res)
	if len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatalf("page = %+v, want 2 items and a cursor", page)
	}
	if src.gotFilter.Tenant != "tenant-a" || src.gotFilter.Action != "workspace.start" ||
		src.gotFilter.ActorRef != observability.ActorRef("https://idp", "alice") || src.gotFilter.Since == nil {
		t.Fatalf("filter = %+v", src.gotFilter)
	}
	res = doReq(t, env, sess, csrf, http.MethodGet, "/v1/admin/tenants/tenant-a/audit?limit=2&cursor="+url.QueryEscape(page.NextCursor), "", nil)
	if res.StatusCode != http.StatusOK || src.gotAfter == nil || src.gotAfter.ID != 2 {
		t.Fatalf("cursor not decoded: status=%d after=%+v", res.StatusCode, src.gotAfter)
	}
}

func TestAdminAudit_RejectsBadInput(t *testing.T) {
	env := newAdminAuditEnv(t, &fakeAuditSource{})
	sess, csrf := loginAdmin(t, env, "admin-a")
	for _, q := range []string{"sort=createdAt", "since=yesterday", "actor=nobody", "since=2026-10-02T00:00:00Z&until=2026-10-01T00:00:00Z", "action=" + strings.Repeat("a", 129)} {
		res := doReq(t, env, sess, csrf, http.MethodGet, "/v1/admin/tenants/tenant-a/audit?"+q, "", nil)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, res.StatusCode)
		}
	}
}

func TestAdminAudit_Authz(t *testing.T) {
	env := newAdminAuditEnv(t, &fakeAuditSource{})
	sess, csrf := login(t, env, "user-a")
	if res := doReq(t, env, sess, csrf, http.MethodGet, "/v1/admin/tenants/tenant-a/audit", "", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin: %d", res.StatusCode)
	}
	asess, acsrf := loginAdmin(t, env, "admin-a")
	if res := doReq(t, env, asess, acsrf, http.MethodGet, "/v1/admin/tenants/tenant-b/audit", "", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("other tenant: %d", res.StatusCode)
	}
}
```

Then apply checklist items 4 to 6 to the shared tests (line numbers are origin/main `5706fe5`):

- `auditroutes_test.go`, `newCoverageEnv` (`:186`): after the `MountAdminSessionPolicyRoutes(...)` line add `MountAdminAuditRoutes(mux, a, NewAdminAuditHandler(&fakeAuditSource{rows: auditRows(2)}, tenants).WithAuditSink(sink))`. The two rows make the contract test validate a real `AdminAuditEntry`, not an empty page.
- `auditroutes_test.go`, `TestAuditCoverage_MountedMuxEmitsEventPerRoute` (`:282`): in the `routes` table, after the `session-policy` PUT entry, add the driver below. The `op` strings in this table use the normalized `{}` form (`normPattern`), not `{tenant}`:
  ```go
  {"GET /v1/admin/tenants/{}/audit", auditActionAdminAuditGet, func() {
  	doReq(t, env, sessA, csrfA, http.MethodGet, adminAuditPath, "", nil).Body.Close()
  }},
  ```
  with `const adminAuditPath = "/v1/admin/tenants/tenant-a/audit"` declared in `adminaudit_test.go`.
- `openapi_contract_test.go`, `TestOpenAPIContract_ResponsesValidate` (`:513`): in the `calls` slice (spec paths are the raw `{tenant}` form), after the `session-policy` GET pair add one success and one refusal. The refusal must be a status the spec declares for the operation (403):
  ```go
  {"GET", "/v1/admin/tenants/{tenant}/audit", func() *http.Response {
  	return doReq(t, env, sessA, csrfA, "GET", adminAuditPath, "", nil)
  }},
  {"GET", "/v1/admin/tenants/{tenant}/audit", func() *http.Response {
  	return doReq(t, env, sess, csrf, "GET", adminAuditPath, "", nil)
  }},
  ```
- `tenantadmin_test.go`: add `aud *fakeAuditSource` to `adminGroupFixtures` (`:31`), set `aud: &fakeAuditSource{}` in the literal in `newAdminGroupEnv` (`:45`), mount `MountAdminAuditRoutes(mux, a, NewAdminAuditHandler(fx.aud, defaultTenants()))` after the session-policy mount, and add `TestAdminAudit_CustomAdminGroups` after `TestAdminSessionPolicy_CustomAdminGroups` (`:260`) by copying it with `adminAuditPath` in place of `adminSessionPolicyPath`.

Run `go test ./internal/api/ -run 'AdminAudit|AuditCoverage|OpenAPI' -count=1`. Expected: FAIL to compile (undefined `AuditSource`, `NewAdminAuditHandler`, `MountAdminAuditRoutes`, `routeAdminAuditGet`).

- [ ] **Step 4: Implement `adminaudit.go`**

```go
package api

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// AuditSource reads persisted audit events (store.DB).
type AuditSource interface {
	ListAudit(ctx context.Context, f store.AuditFilter, limit int, after *store.AuditCursor) ([]store.AuditRecord, error)
}

type adminAuditEntry struct {
	ID         string            `json:"id"`
	CreatedAt  time.Time         `json:"createdAt"`
	Actor      string            `json:"actor"`
	Action     string            `json:"action"`
	TargetKind string            `json:"targetKind"`
	TargetID   string            `json:"targetId"`
	Outcome    string            `json:"outcome"`
	ErrorCode  string            `json:"errorCode,omitempty"`
	RequestID  string            `json:"requestId"`
	Details    map[string]string `json:"details"`
}

// AdminAuditHandler serves GET /v1/admin/tenants/{tenant}/audit (ADR 0011).
type AdminAuditHandler struct {
	source  AuditSource
	tenants TenantResolver
	audit   observability.AuditSink
}

func NewAdminAuditHandler(src AuditSource, t TenantResolver) *AdminAuditHandler {
	return &AdminAuditHandler{source: src, tenants: t}
}

func (h *AdminAuditHandler) WithAuditSink(s observability.AuditSink) *AdminAuditHandler {
	h.audit = s
	return h
}

func MountAdminAuditRoutes(mux RouteMux, authn *Authenticator, h *AdminAuditHandler) {
	mux.Handle(routeAdminAuditGet, authn.RequireAuthPassive(
		audited(h.audit, routeAdminAuditGet, http.HandlerFunc(h.List))))
}

var auditActionPattern = regexp.MustCompile(`^[a-z0-9_.-]{1,128}$`)

func (h *AdminAuditHandler) List(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantAdminPrincipal(h.tenants, w, r); !ok {
		return
	}
	page, perr := ParsePage(r, []string{"createdAt"}, "-createdAt")
	if perr != nil {
		WriteError(w, r, perr)
		return
	}
	if !page.Sort.Desc {
		writeError(w, r, CodeInvalidRequest, "audit lists newest first only (sort=-createdAt)")
		return
	}
	q := r.URL.Query()
	f := store.AuditFilter{Tenant: r.PathValue("tenant")}
	if v := q.Get("action"); v != "" {
		if !auditActionPattern.MatchString(v) {
			writeError(w, r, CodeInvalidRequest, "bad action filter")
			return
		}
		f.Action = v
	}
	if v := q.Get("actor"); v != "" {
		switch {
		case v == "anonymous" || (strings.HasPrefix(v, "oidc:") && len(v) <= 64):
			f.ActorRef = v
		case validOwnerRef(v) == "":
			f.ActorRef = targetActorRef(v)
		default:
			writeError(w, r, CodeInvalidRequest, "actor must be an actor reference or an issuer|sub owner reference")
			return
		}
	}
	if v := q.Get("target"); v != "" {
		if len(v) > 256 {
			writeError(w, r, CodeInvalidRequest, "bad target filter")
			return
		}
		f.TargetID = v
	}
	for _, p := range []struct {
		key string
		dst **time.Time
	}{{"since", &f.Since}, {"until", &f.Until}} {
		if v := q.Get(p.key); v != "" {
			ts, err := time.Parse(time.RFC3339, v)
			if err != nil {
				writeError(w, r, CodeInvalidRequest, p.key+" must be RFC 3339")
				return
			}
			*p.dst = &ts
		}
	}
	if f.Since != nil && f.Until != nil && !f.Since.Before(*f.Until) {
		writeError(w, r, CodeInvalidRequest, "since must be before until")
		return
	}
	var after *store.AuditCursor
	if page.After != nil {
		at, err1 := time.Parse(time.RFC3339Nano, page.After.Key)
		id, err2 := strconv.ParseInt(page.After.ID, 10, 64)
		if err1 != nil || err2 != nil {
			writeError(w, r, CodeInvalidRequest, "bad cursor")
			return
		}
		after = &store.AuditCursor{CreatedAt: at, ID: id}
	}
	rows, err := h.source.ListAudit(r.Context(), f, page.Limit, after)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	out := Page[adminAuditEntry]{Items: make([]adminAuditEntry, 0, len(rows))}
	if len(rows) > page.Limit {
		last := rows[page.Limit-1]
		out.NextCursor = NextCursor(page.Sort, last.CreatedAt.UTC().Format(time.RFC3339Nano), strconv.FormatInt(last.ID, 10))
		rows = rows[:page.Limit]
	}
	for _, rec := range rows {
		details := rec.Details
		if details == nil {
			details = map[string]string{} // the schema requires an object, never null
		}
		out.Items = append(out.Items, adminAuditEntry{
			ID: strconv.FormatInt(rec.ID, 10), CreatedAt: rec.CreatedAt, Actor: rec.ActorRef, Action: rec.Action,
			TargetKind: rec.TargetKind, TargetID: rec.TargetID, Outcome: rec.Outcome, ErrorCode: rec.ErrorCode,
			RequestID: rec.RequestID, Details: details,
		})
	}
	respondJSON(w, out)
}
```

In `auditroutes.go`, add:
- `auditActionAdminAuditGet = "admin.audit.get"`;
- `routeAdminAuditGet = "GET /v1/admin/tenants/{tenant}/audit"`;
- the `auditedRoutes` entry `routeAdminAuditGet: {auditActionAdminAuditGet, "tenant"}`.

Add `"admin.audit.get"` to `auditEventActions` in `metrics.go`. While there, also add the missing `"admin.session_policy.get", "admin.session_policy.set", "admin.session_policy.clear"`.

In `wire.go` `newAppHandler`, after `adminSessionPolicyHandler` (~line 704), add `adminAuditHandler := api.NewAdminAuditHandler(db, tenants).WithAuditSink(appAudit)` (`*store.DB` satisfies `AuditSource`) and pass `adminAudit: adminAuditHandler` in the `appRoutes` literal. In `appMux`, add `if h.adminAudit != nil { api.MountAdminAuditRoutes(mux, authn, h.adminAudit) }`. `backend_test.go` needs no change (the field is nil there).

Run the Step 3 command, then `go test ./internal/api/ ./internal/backend/ -count=1`. Expected: PASS.

- [ ] **Step 5: Web page**

`web/src/admin/api.ts` (append; `unwrap`, `ServerPage` and `components` are already imported there):

```ts
export type AdminAuditEntry = components["schemas"]["AdminAuditEntry"];
export type AuditFilter = { action?: string; actor?: string; target?: string; since?: string; until?: string };

// GET .../audit: newest first (the only order), filtered server-side. Empty
// filter fields are omitted. exactOptionalPropertyTypes is on, so the query
// is built with conditional spreads, not undefined values.
export async function listAdminAuditPage(
  api: ApiClient,
  tenant: string,
  filter: AuditFilter,
  cursor?: string,
): Promise<ServerPage<AdminAuditEntry>> {
  return unwrap(
    await api.GET("/v1/admin/tenants/{tenant}/audit", {
      params: {
        path: { tenant },
        query: {
          sort: "-createdAt",
          ...(filter.action ? { action: filter.action } : {}),
          ...(filter.actor ? { actor: filter.actor } : {}),
          ...(filter.target ? { target: filter.target } : {}),
          ...(filter.since ? { since: filter.since } : {}),
          ...(filter.until ? { until: filter.until } : {}),
          ...(cursor ? { cursor } : {}),
        },
      },
    }),
  );
}
```

`web/src/admin/AuditPage.tsx` (imports as in `UsersAdminPage.tsx`: `useMeLoaded` from `../app/me`, `useApi` from `../api/context`, `AdminLayout`, `ApiErrorAlert`, `formatDateTime` from `./format`; from `../design`: `Badge`, `Button`, `Input`, `Pagination`, `Table`, `useServerPage`, type `Tone`; `Column` from `../design/Table`):

```tsx
const OUTCOME_TONE: Record<AdminAuditEntry["outcome"], Tone> = {
  success: "success",
  denied: "warning",
  failure: "danger",
};

// A datetime-local value ("2026-10-13T09:30") is local time; the API wants RFC 3339.
function toIso(local: string): string | undefined {
  if (!local) return undefined;
  const d = new Date(local);
  return Number.isNaN(d.getTime()) ? undefined : d.toISOString();
}

export function AuditPage() {
  const api = useApi();
  const me = useMeLoaded();
  const tenant = me.data?.tenant;
  const [draft, setDraft] = useState({ action: "", actor: "", target: "", since: "", until: "" });
  // The filter the list is loaded with; changes only on Apply / Clear.
  const [applied, setApplied] = useState<AuditFilter>({});
  const load = useCallback(
    (cursor?: string) =>
      tenant === undefined
        ? Promise.resolve({ items: [] as AdminAuditEntry[] })
        : listAdminAuditPage(api, tenant, applied, cursor),
    [api, tenant, applied],
  );
  const list = useServerPage(load, { sort: "-createdAt", key: JSON.stringify([tenant, applied]) });

  const apply = (e: FormEvent) => {
    e.preventDefault();
    setApplied({
      ...(draft.action.trim() ? { action: draft.action.trim() } : {}),
      ...(draft.actor.trim() ? { actor: draft.actor.trim() } : {}),
      ...(draft.target.trim() ? { target: draft.target.trim() } : {}),
      ...(toIso(draft.since) ? { since: toIso(draft.since)! } : {}),
      ...(toIso(draft.until) ? { until: toIso(draft.until)! } : {}),
    });
  };
  const clear = () => {
    setDraft({ action: "", actor: "", target: "", since: "", until: "" });
    setApplied({});
  };

  const columns: Column<AdminAuditEntry>[] = [
    { key: "time", header: t("admin.audit.column.time"), rowHeader: true,
      render: (r) => <time dateTime={r.createdAt}>{formatDateTime(r.createdAt)}</time> },
    { key: "actor", header: t("admin.audit.column.actor"), hideOnMobile: true, render: (r) => <code>{r.actor}</code> },
    { key: "action", header: t("admin.audit.column.action"), render: (r) => <code>{r.action}</code> },
    { key: "target", header: t("admin.audit.column.target"),
      render: (r) => (r.targetId ? <code>{r.targetId}</code> : "—") },
    { key: "outcome", header: t("admin.audit.column.outcome"),
      render: (r) => <Badge tone={OUTCOME_TONE[r.outcome]}>{t(`admin.audit.outcome.${r.outcome}`)}</Badge> },
    { key: "details", header: t("admin.audit.column.details"), hideOnMobile: true,
      render: (r) => Object.entries(r.details).map(([k, v]) => `${k}=${v}`).join(", ") || "—" },
  ];

  return (
    <AdminLayout
      title={t("admin.audit.title")}
      description={t("admin.audit.description")}
      actions={<Button icon={<IconRefresh />} onClick={list.refresh} loading={list.loading && list.loaded}>{t("admin.action.refresh")}</Button>}
    >
      <form className="tc-admin-filters" onSubmit={apply}>
        <Input label={t("admin.audit.filter.action")} value={draft.action} onChange={(e) => setDraft({ ...draft, action: e.target.value })} />
        <Input label={t("admin.audit.filter.actor")} hint={t("admin.audit.filter.actorHint")} value={draft.actor} onChange={(e) => setDraft({ ...draft, actor: e.target.value })} />
        <Input label={t("admin.audit.filter.target")} value={draft.target} onChange={(e) => setDraft({ ...draft, target: e.target.value })} />
        <Input label={t("admin.audit.filter.since")} type="datetime-local" value={draft.since} onChange={(e) => setDraft({ ...draft, since: e.target.value })} />
        <Input label={t("admin.audit.filter.until")} type="datetime-local" value={draft.until} onChange={(e) => setDraft({ ...draft, until: e.target.value })} />
        <Button type="submit" variant="primary">{t("admin.audit.filter.apply")}</Button>
        <Button onClick={clear}>{t("admin.audit.filter.clear")}</Button>
      </form>
      <ApiErrorAlert error={me.error ?? list.error} onRetry={me.error ? me.reload : list.refresh} />
      <Table
        caption={t("admin.audit.caption")}
        columns={columns}
        rows={list.items}
        rowKey={(r) => r.id}
        loading={(tenant === undefined || !list.loaded) && !list.error && !me.error}
        density="compact"
        empty={t("admin.audit.empty")}
      />
      <Pagination
        loaded={list.items.length}
        hasNext={list.hasNext}
        loading={list.loading}
        onLoadMore={list.next}
        label={t("common.pagination.label", { name: t("admin.audit.caption") })}
      />
    </AdminLayout>
  );
}
```

`admin.action.refresh` (`en/admin.ts:33`) and `common.pagination.label` (`en/common.ts:12`) already exist. Add a `.tc-admin-filters` rule to `web/src/admin/admin.css`: `display: flex; flex-wrap: wrap; align-items: flex-end; gap: var(--tc-space-3); margin-block-end: var(--tc-space-4);`. `AuditPage.tsx` also imports `useCallback`, `useState` and `type FormEvent` from `react`, `IconRefresh` from `../design/icons`, `t` from `../i18n`, and `listAdminAuditPage`, `type AdminAuditEntry`, `type AuditFilter` from `./api`.

Strings (en, then vi in the same keys):

| key | en | vi |
|---|---|---|
| `admin.nav.audit` | Audit log | Nhật ký kiểm toán |
| `admin.route.audit` | Audit log | Nhật ký kiểm toán |
| `admin.audit.title` | Audit log | Nhật ký kiểm toán |
| `admin.audit.description` | Every administrative and mutating action in this tenant, newest first. Events older than the platform retention period (90 days by default) are removed. | Mọi thao tác quản trị và thay đổi trong tenant này, mới nhất trước. Sự kiện cũ hơn thời hạn lưu của nền tảng (mặc định 90 ngày) sẽ bị xoá. |
| `admin.audit.filter.action` | Action | Hành động |
| `admin.audit.filter.actor` | Actor | Người thực hiện |
| `admin.audit.filter.actorHint` | An actor reference (oidc:…) or a user reference (issuer\|subject) | Mã người thực hiện (oidc:…) hoặc mã người dùng (issuer\|subject) |
| `admin.audit.filter.target` | Target | Đối tượng |
| `admin.audit.filter.since` | From | Từ |
| `admin.audit.filter.until` | To | Đến |
| `admin.audit.filter.apply` | Apply | Áp dụng |
| `admin.audit.filter.clear` | Clear | Xoá lọc |
| `admin.audit.column.time` | Time | Thời gian |
| `admin.audit.column.actor` | Actor | Người thực hiện |
| `admin.audit.column.action` | Action | Hành động |
| `admin.audit.column.target` | Target | Đối tượng |
| `admin.audit.column.outcome` | Outcome | Kết quả |
| `admin.audit.column.details` | Details | Chi tiết |
| `admin.audit.outcome.success` | Success | Thành công |
| `admin.audit.outcome.denied` | Denied | Bị từ chối |
| `admin.audit.outcome.failure` | Failed | Thất bại |
| `admin.audit.caption` | Audit events | Sự kiện kiểm toán |
| `admin.audit.empty` | No audit events match. | Không có sự kiện nào khớp. |

In the `.ts` catalogs write the hint with a plain `|` (`issuer|subject`); the backslash above only escapes the pipe inside this markdown table. The description deliberately does not interpolate the retention: the backend flag is operator-set, so the page must not claim a number it cannot read.

Add `{ path: "/admin/audit", labelKey: "admin.nav.audit" }` to `ADMIN_SECTIONS` (`AdminLayout.tsx:9`) after the users entry, and add to `routes.tsx` (before the `/admin/*` catch-all) `{ path: "/admin/audit", title: t("admin.route.audit"), requires: "tenant-admin", render: () => <AuditPage /> }` with `import { AuditPage } from "./AuditPage";`.

In `web/tests/mock-api/admin.ts` (inside `adminArea`, next to `adminSessionPolicy`):
- add `export const AUDIT_ROWS = 60` and a `function auditRows()` that builds 60 deterministic rows `i = 0..59`: `id: String(60 - i)`, `createdAt: new Date(Date.UTC(2026, 9, 13, 12, 0) - i * 60_000).toISOString()`, `actor: "oidc:0011223344556677"`, `action: ["workspace.start", "workspace.stop", "admin.quota.set", "admin.user_limit.get", "data.purge"][i % 5]`, `targetKind`/`targetId` (`"workspace"`/`ws_audit${i}` for `workspace.*`, otherwise `"tenant"`/`TENANT`), `outcome: i % 7 === 0 ? "denied" : "success"`, `errorCode: i % 7 === 0 ? "FORBIDDEN" : undefined`, `requestId: "req-" + i`, `details: {}`. Row 0 is a denied `workspace.start`; `workspace.start` rows total 12. The 60 rows exceed the mock's default page (`paginate` limit 50, `core.ts:160`), so the page needs "Load more";
- a handler `adminAudit(req, tenant)` that returns the same 403 as `adminUserLimits` for a non-admin or foreign tenant (`!t.isAdmin() || tenant !== t.me.tenant`), `405` for non-GET, applies `t.takeFailure(req)`, filters on the `action`, `actor` and `target` (`targetId`) query parameters by equality and on `since` (inclusive) and `until` (exclusive) by `Date.parse`, rejects any `sort` other than `-createdAt` with `err(400, "INVALID_REQUEST", "audit lists newest first only", false)`, and returns `paginate(req, rows, { createdAt: byString((a) => a.createdAt) }, "-createdAt", (a) => a.id)`;
- in `api(req)` add, next to `mSP`: `const mAU = path.match(/^\/v1\/admin\/tenants\/([^/]+)\/audit$/); if (mAU) return adminAudit(req, mAU[1]);`.

`web/tests/unit/admin/audit.test.tsx` (pattern of `users.test.tsx`: `createMockApi({ areas: [adminArea] })`, `loginCookies()`, `makeAdmin(api)`, `renderWithApi(<AuditPage />, api)`; the request log is `api.state.requests`, with `.method`, `.path` and `.query`):
- `it("lists the newest page and the denied outcome")`: `await screen.findByRole("table", { name: "Audit events" })`; expect `within(table).getAllByText("Denied").length > 0` and the first data row to contain `workspace.start`;
- `it("Apply sends the action filter")`: `fireEvent.change(screen.getByLabelText("Action"), { target: { value: "workspace.start" } })`, click "Apply"; `await waitFor` for an `api.state.requests` entry with `path === "/v1/admin/tenants/" + TENANT + "/audit"` whose `new URLSearchParams(r.query).get("action") === "workspace.start"`, then expect every body row to be `workspace.start` and `sort=-createdAt` on every request;
- `it("Clear drops the filter")`: Apply, then click "Clear"; the next audit request carries no `action`.

`web/tests/admin.spec.ts`: rename `"a tenant admin sees all seven admin pages"` to `"a tenant admin sees all eight admin pages"`, add `await expect(adminNav.getByRole("tab", { name: "Audit log" })).toBeVisible();` after the Users tab check, and add this test (the file's `makeAdmin`, `login` and `MOCK` helpers):

```ts
test("audit log lists, filters and pages", async ({ page, request }) => {
  await makeAdmin(request);
  const errors: string[] = [];
  page.on("console", (m) => {
    if (m.type() === "error") errors.push(m.text());
  });
  await login(page, "/admin/audit");
  await expect(page.getByRole("heading", { name: "Audit log", exact: true })).toBeVisible();

  const table = page.getByRole("table", { name: "Audit events" });
  await expect(table.getByRole("row")).toHaveCount(51); // header + 50
  await page.getByRole("button", { name: "Load more" }).click();
  await expect(table.getByRole("row")).toHaveCount(61); // header + 60
  await expect(table.getByText("Denied").first()).toBeVisible();

  await page.getByLabel("Action", { exact: true }).fill("workspace.start");
  await page.getByRole("button", { name: "Apply" }).click();
  await expect(table.getByRole("row")).toHaveCount(13); // header + 12
  await expect(table.getByRole("cell", { name: "workspace.stop" })).toHaveCount(0);

  await page.getByRole("button", { name: "Clear" }).click();
  await expect(table.getByRole("row")).toHaveCount(51);
  expect(errors).toEqual([]);
});
```

`web/tests/a11y.spec.ts`: add `"/admin/audit"` to `ROUTES` after `"/admin/users"` (the axe sweep then covers the filter form and the table in both themes).

Run:

```bash
cd web && npm run typecheck && npm run lint:strings && npx vitest run tests/unit/admin/audit.test.tsx tests/unit/i18n tests/unit/admin/gate.test.tsx && npx playwright test tests/admin.spec.ts tests/a11y.spec.ts -g "audit|admin pages"
```

Expected: PASS.

- [ ] **Step 6: ADR 0011, index and threat-model row**

Create `docs/adr/0011-audit-store-and-retention.md`:

```markdown
# ADR 0011 — Audit store and retention

Status: accepted (F6 of the v1.0.0 W1 slate; implemented with the audit
viewer)

Date: 2026-10-14

## Context

Audit events were a stdout JSONL stream only (`observability.JSONSink`
behind `GuardedSink`). Administrators could not read them in the product,
and a write failure was swallowed by design.

## Decision

- **Store.** Domain audit events (every route in `auditedRoutes` except
  `session.touch`, plus the file-transfer claim/complete records the broker
  writes) are persisted to `audit_event` (`internal/store/audit.go`). The
  stdout record is unchanged and still written for every event.
- **One function, fail closed.** `audited()` writes both. On a mutating
  request it installs a `store.TxHook`; the first transaction of the
  request that commits inserts the row in the same transaction, so the
  mutation and its record commit or roll back together. The row is
  amended with the final outcome after the handler returns. A request that
  commits no transaction inserts its row before the buffered response is
  released; if the insert fails the client gets `500 INTERNAL` and nothing
  the handler produced. A mutation that reaches only Kubernetes cannot be
  rolled back — its response fails closed instead.
- **Pseudonymization.** `actor_ref` is `observability.ActorRef(issuer,
  subject)` (`oidc:` + 8 bytes of SHA-256, hex), or `anonymous`. A target
  user appears only as the same pseudonym in the event's details
  (`owner` or `target`, via `targetActorRef`). No raw subject, email,
  display name, source address, token or cookie is written; detail keys
  that look like credentials are redacted before storage
  (`observability.RedactDetails`). File-transfer events record the file
  name and byte count, never content. The viewer accepts an `issuer|sub`
  filter and pseudonymizes it server-side, so an administrator can search
  by a known user without the table ever holding the identity. The
  pseudonym is a join key across events, not a secret: anyone who can guess
  an `issuer|sub` can compute it.
- **Retention.** `-audit-retention-days` (chart
  `backend.retention.auditDays`, default 90, 1..3650). A backend leader
  singleton prunes older rows once at election and then daily, in batches
  of 1000.
- **Read surface.** `GET /v1/admin/tenants/{tenant}/audit`, tenant-admin
  only, newest first, keyset-paged on `(created_at, id)`, filtered by
  action, actor, target and time range. Reading the log is itself audited
  (`admin.audit.get`).

## Alternatives considered

- **Stdout only, with a cluster log shipper.** Rejected: the product could
  not show the log, and a lost line could not fail the mutation.
- **A separate audit database.** Rejected: it cannot share the mutation's
  transaction, which is what makes "no record, no change" true.

## Consequences

- Every audited request does one extra insert; the table is indexed on
  `(tenant, created_at desc, id desc)` and `(tenant, action, created_at desc, id desc)`.
- An audit-store outage makes audited routes fail instead of running
  unrecorded. Unaudited reads keep working.
- Gateway-local events (launch, stream) stay stdout-only.
```

In `docs/README.md`, under `## ADRs` after the `adr/0010-session-data-transfer-policy.md` entry (`:93-95`), add:

```markdown
- [adr/0011-audit-store-and-retention.md](adr/0011-audit-store-and-retention.md) —
  durable `audit_event` store written in the audited mutation's transaction
  (fail closed), pseudonymous actors, 90-day retention and the tenant-admin
  viewer (accepted)
```

Append threat-model row S42 to the `| # | Item | State today |` table in `docs/security/threat-model.md` §6, directly after row S41 (`:628`; the table's last row on `origin/main`):

`| S42 | Audit store and viewer | Implemented: domain events persist to audit_event in the audited mutation's transaction (fail closed, ADR 0011); actor and target are pseudonymous refs; the viewer is tenant-admin only, tenant-scoped in SQL, keyset-paged, and itself audited; retention 90 days by default |`

- [ ] **Step 7: Commit**

```bash
go test -count=1 ./internal/api/ ./internal/backend/
git add internal/api internal/backend internal/observability web docs/adr/0011-audit-store-and-retention.md docs/README.md docs/security/threat-model.md
git commit -m "feat(admin): audit log viewer at /admin/audit with ADR 0011

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 3: Tenant user directory `GET /v1/admin/tenants/{tenant}/users` (F8) — tier: integrate

**Files:**
- Create: `internal/store/users.go`, `internal/store/users_test.go`
- Create: `internal/api/adminusers.go`, `internal/api/adminusers_test.go`
- Modify: `internal/api/{auditroutes.go,openapi.yaml,adminuserlimit.go,auditroutes_test.go,openapi_contract_test.go,tenantadmin_test.go}`, `internal/observability/metrics.go:121`
- Modify: `internal/backend/wire.go` (`appRoutes.adminUsers`), `internal/backend/backend_test.go`
- Modify: `web/src/admin/{api.ts,UsersAdminPage.tsx,routes.tsx}`, `web/src/i18n/{en,vi}/admin.ts`, `web/tests/mock-api/admin.ts`, `web/tests/unit/admin/users.test.tsx` (exists: its `userRowOf`, `UsersAdminPage` and `UserDetailPage` cases are rewritten below), `web/tests/admin.spec.ts`, `web/tests/a11y.spec.ts`, `web/src/api/generated/schema.d.ts` (generated)
- Modify: `docs/security/threat-model.md` (row S43)

**Interfaces:**
- Consumes: `UserLimitSource.Report` (`adminuserlimit.go:23`), `(*AdminUserLimitsHandler).entry` (`adminuserlimit.go:384`, extracted below), `effectiveUserLimit` (`adminquota.go:279`), `validOwnerRef` (`adminuserlimit.go:414`), `targetActorRef` (`:406`), `store.PrincipalDirectory`/`DirectoryEntry` (`store/directory.go`, table `principal_directory`), and from Task 2 `appRoutes`, `newAdminMountEnv` (`adminaudit_test.go`) and the `AdminLayout`/`ADMIN_SECTIONS` web scaffolding.
- Produces (Task 4 consumes the detail route and `getAdminUser`):
  - `type store.TenantUser struct{ OwnerRef, Subject, DisplayName, Email string; LastSeenAt time.Time; Workspaces int64 }`
  - `type store.UserPage struct{ Limit int; Sort string; Desc bool; After *store.UserCursor }`, with `Sort` one of `"ownerRef"`, `"displayName"`, `"lastSeen"`; `type store.UserCursor struct{ Key, OwnerRef string }` (for `lastSeen`, `Key` is RFC3339Nano)
  - `func (d *store.DB) ListTenantUsers(ctx context.Context, tenantID string, p store.UserPage) ([]store.TenantUser, error)`. It returns up to `p.Limit+1` rows.
  - `func (d *store.DB) GetTenantUser(ctx context.Context, tenantID, ownerRef string) (store.TenantUser, bool, error)`
  - `GET /v1/admin/tenants/{tenant}/users?sort=ownerRef|displayName|lastSeen` (default `ownerRef`, `-` allowed) returns `Page[AdminTenantUser]`. The audit action is `admin.user.list`.
  - `GET /v1/admin/tenants/{tenant}/users/{ownerRef}` returns `AdminTenantUser`, or 404 `NOT_FOUND` when the principal never signed in to the tenant. The audit action is `admin.user.get`.
  - JSON `AdminTenantUser = {ownerRef, subject, displayName, email, lastSeenAt, workspaces, limits: AdminUserLimitEntry}`
  - `type UserDirectorySource interface{ ListTenantUsers(...); GetTenantUser(...) }` (the two store signatures above), `func NewAdminUsersHandler(src UserDirectorySource, limits UserLimitSource, t TenantResolver) *AdminUsersHandler`, `WithAuditSink`, `func MountAdminUserRoutes(mux RouteMux, authn *Authenticator, h *AdminUsersHandler)`
  - Web: `listAdminUserPage(api, tenant, cursor?, sort?, limit?)`, `getAdminUser(api, tenant, ownerRef)`, `userDetailPath(ownerRef: string)` and `ownerRefFromParam(param: string): string | undefined`; the route is `/admin/users/:ownerRef`, where the path segment is the base64url of the UTF-8 owner reference (see Step 5 for why), rendering `<UserDetailPage ownerRef=... />` with the decoded reference. `UserDetailPage` loads `{ tenant: string; user: AdminTenantUser | undefined; disks }` (Task 4 reads `u.data.tenant` and `u.data.user.ownerRef`/`.displayName`).

- [ ] **Step 1: Failing store test**

`internal/store/users_test.go`:

```go
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func seedSession(t *testing.T, db *store.DB, id, tenant, issuer, subject string, seen time.Time) {
	t.Helper()
	if _, err := db.Pool().Exec(context.Background(),
		`INSERT INTO sessions (id, issuer, subject, tenant_id, created_at, last_seen_at) VALUES ($1, $2, $3, $4, $5, $5)`,
		id, issuer, subject, tenant, seen); err != nil {
		t.Fatal(err)
	}
}

func TestTenantUsers_ListSortsPagesAndCounts(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	dir := store.NewPrincipalDirectory(db)
	for _, e := range []store.DirectoryEntry{
		{OwnerRef: "https://idp|alice", Subject: "alice", DisplayName: "Alice", Email: "a@x"},
		{OwnerRef: "https://idp|bob", Subject: "bob", DisplayName: "Bob"},
		{OwnerRef: "https://idp|carol", Subject: "carol", DisplayName: "Carol"},
	} {
		if err := dir.Remember(ctx, "t1", e); err != nil {
			t.Fatal(err)
		}
	}
	if err := dir.Remember(ctx, "t2", store.DirectoryEntry{OwnerRef: "https://idp|mallory", Subject: "mallory"}); err != nil {
		t.Fatal(err)
	}
	seedWorkspace(t, db, "t1", "https://idp|bob", "ws-b1")
	seedWorkspace(t, db, "t1", "https://idp|bob", "ws-b2")
	future := time.Now().Add(time.Hour)
	seedSession(t, db, "s1", "t1", "https://idp", "carol", future)

	page, err := db.ListTenantUsers(ctx, "t1", store.UserPage{Limit: 2, Sort: "ownerRef"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 3 || page[0].OwnerRef != "https://idp|alice" || page[1].OwnerRef != "https://idp|bob" {
		t.Fatalf("page 1 = %+v", page)
	}
	if page[1].Workspaces != 2 || page[0].Email != "a@x" {
		t.Fatalf("counts/fields: %+v", page[:2])
	}
	next, err := db.ListTenantUsers(ctx, "t1", store.UserPage{Limit: 2, Sort: "ownerRef", After: &store.UserCursor{Key: page[1].OwnerRef, OwnerRef: page[1].OwnerRef}})
	if err != nil || len(next) != 1 || next[0].OwnerRef != "https://idp|carol" {
		t.Fatalf("page 2 = %+v, %v", next, err)
	}
	byLastSeen, _ := db.ListTenantUsers(ctx, "t1", store.UserPage{Limit: 10, Sort: "lastSeen", Desc: true})
	if byLastSeen[0].OwnerRef != "https://idp|carol" || byLastSeen[0].LastSeenAt.Sub(future).Abs() > time.Millisecond {
		t.Fatalf("lastSeen must fold in session activity: %+v", byLastSeen[0])
	}
	u, ok, err := db.GetTenantUser(ctx, "t1", "https://idp|bob")
	if err != nil || !ok || u.Workspaces != 2 {
		t.Fatalf("GetTenantUser = %+v %v %v", u, ok, err)
	}
	if _, ok, _ := db.GetTenantUser(ctx, "t1", "https://idp|mallory"); ok {
		t.Fatal("cross-tenant principal visible")
	}
}

func TestTenantUsers_DisplayNameFallsBackToSubject(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	if err := store.NewPrincipalDirectory(db).Remember(ctx, "t1", store.DirectoryEntry{OwnerRef: "https://idp|dave", Subject: "dave"}); err != nil {
		t.Fatal(err)
	}
	u, ok, err := db.GetTenantUser(ctx, "t1", "https://idp|dave")
	if err != nil || !ok || u.DisplayName != "dave" || u.Workspaces != 0 {
		t.Fatalf("GetTenantUser = %+v %v %v, want display name = subject and no workspaces", u, ok, err)
	}
}
```

Run `go test ./internal/store/ -run TestTenantUsers -count=1`. Expected: FAIL to compile (`store.UserPage`, `ListTenantUsers`, `GetTenantUser` undefined). The test needs Docker (`newDB` starts `postgres:18.0`); a skip is not a pass.

- [ ] **Step 2: Implement `internal/store/users.go`**

```go
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TenantUser is one principal that signed in to a tenant (principal_directory),
// with activity and ownership folded in. Display data only.
type TenantUser struct {
	OwnerRef    string
	Subject     string
	DisplayName string
	Email       string
	LastSeenAt  time.Time
	Workspaces  int64
}

// UserCursor is the keyset position of the previous page's last row.
type UserCursor struct {
	Key      string // the sort column's value (RFC3339Nano for lastSeen)
	OwnerRef string
}

// UserPage selects one page of ListTenantUsers.
type UserPage struct {
	Limit int
	Sort  string // ownerRef | displayName | lastSeen
	Desc  bool
	After *UserCursor
}

// tenantUsersCTE folds session activity (last_seen_at) and active-workspace
// counts into each directory row. The sessions lookup splits owner_ref at the
// first '|' (validOwnerRef guarantees the issuer holds none) so it rides the
// sessions_issuer_subject index. A principal without a display name shows
// (and sorts under) its subject, as the owner views do.
const tenantUsersCTE = `
WITH u AS (
	SELECT d.owner_ref, d.subject, COALESCE(NULLIF(d.display_name, ''), d.subject) AS display_name, d.email,
		GREATEST(d.last_login_at, COALESCE((
			SELECT max(s.last_seen_at) FROM sessions s
			WHERE s.tenant_id = d.tenant_id
			  AND s.issuer = split_part(d.owner_ref, '|', 1)
			  AND s.subject = substr(d.owner_ref, length(split_part(d.owner_ref, '|', 1)) + 2)), d.last_login_at)) AS last_seen,
		(SELECT count(*) FROM workspaces w
		 WHERE w.tenant_id = d.tenant_id AND w.owner_subject = d.owner_ref AND w.state = 'active') AS workspaces
	FROM principal_directory d WHERE d.tenant_id = $1)
SELECT owner_ref, subject, display_name, email, last_seen, workspaces FROM u`

var userSortColumns = map[string]string{"ownerRef": "owner_ref", "displayName": "display_name", "lastSeen": "last_seen"}

// ListTenantUsers pages the tenant's directory; it returns up to p.Limit+1 rows.
func (d *DB) ListTenantUsers(ctx context.Context, tenantID string, p UserPage) ([]TenantUser, error) {
	col, ok := userSortColumns[p.Sort]
	if !ok {
		return nil, fmt.Errorf("store: unknown user sort %q", p.Sort)
	}
	dir, cmp := "ASC", ">"
	if p.Desc {
		dir, cmp = "DESC", "<"
	}
	args := []any{tenantID}
	where := ""
	if p.After != nil {
		var key any = p.After.Key
		if p.Sort == "lastSeen" {
			ts, err := time.Parse(time.RFC3339Nano, p.After.Key)
			if err != nil {
				return nil, fmt.Errorf("store: user cursor %w", err)
			}
			key = ts
		}
		args = append(args, key, p.After.OwnerRef)
		where = fmt.Sprintf(" WHERE (%s, owner_ref) %s ($2, $3)", col, cmp)
	}
	args = append(args, p.Limit+1)
	rows, err := d.pool.Query(ctx, tenantUsersCTE+where+
		fmt.Sprintf(" ORDER BY %s %s, owner_ref %s LIMIT $%d", col, dir, dir, len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("store: list tenant users %w", err)
	}
	return pgx.CollectRows(rows, scanTenantUser)
}

// GetTenantUser returns one directory row; ok=false when the principal
// never signed in to tenantID.
func (d *DB) GetTenantUser(ctx context.Context, tenantID, ownerRef string) (TenantUser, bool, error) {
	rows, err := d.pool.Query(ctx, tenantUsersCTE+" WHERE owner_ref = $2", tenantID, ownerRef)
	if err != nil {
		return TenantUser{}, false, fmt.Errorf("store: get tenant user %w", err)
	}
	u, err := pgx.CollectExactlyOneRow(rows, scanTenantUser)
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantUser{}, false, nil
	}
	if err != nil {
		return TenantUser{}, false, fmt.Errorf("store: get tenant user %w", err)
	}
	return u, true, nil
}

func scanTenantUser(row pgx.CollectableRow) (TenantUser, error) {
	var u TenantUser
	err := row.Scan(&u.OwnerRef, &u.Subject, &u.DisplayName, &u.Email, &u.LastSeenAt, &u.Workspaces)
	return u, err
}
```

Run the Step 1 command. Expected: PASS.

- [ ] **Step 3: Contract, then failing API tests**

Apply the New API operation checklist (Global Constraints) for both operations. Items 1-3 (`auditroutes.go`, `metrics.go:121`, `openapi.yaml`):
- `routeAdminUsersList = "GET /v1/admin/tenants/{tenant}/users"` (`auditActionAdminUserList = "admin.user.list"`, target key `"tenant"`);
- `routeAdminUserGet = "GET /v1/admin/tenants/{tenant}/users/{ownerRef}"` (`auditActionAdminUserGet = "admin.user.get"`, target key `"tenant"`; the handler sets `auditSetDetail(ctx, "target", targetActorRef(ownerRef))`).

The Go 1.22 mux gives a literal segment precedence over a `{ownerRef}` wildcard. A `/` inside an ownerRef (an `https://` issuer) arrives percent-encoded (`%2F`), and `r.PathValue` decodes it. The existing user-limits routes rely on the same behaviour.

OpenAPI schema:

```yaml
    AdminTenantUser:
      type: object
      additionalProperties: false
      required: [ownerRef, subject, displayName, email, lastSeenAt, workspaces, limits]
      properties:
        ownerRef: {type: string}
        subject: {type: string}
        displayName: {type: string}
        email: {type: string}
        lastSeenAt: {type: string, format: date-time}
        workspaces: {type: integer, format: int64, minimum: 0}
        limits: {$ref: '#/components/schemas/AdminUserLimitEntry'}
    AdminTenantUserPage:
      type: object
      additionalProperties: false
      required: [items]
      properties:
        items: {type: array, items: {$ref: '#/components/schemas/AdminTenantUser'}}
        nextCursor: {$ref: '#/components/schemas/NextCursor'}
```

Operations (modelled on `listAdminUserLimits` and `putAdminUserLimit`, `openapi.yaml:858` and `:898`):
- `GET /v1/admin/tenants/{tenant}/users`: `operationId: listAdminTenantUsers`, tag `admin`, parameters `Tenant`, `PageLimit`, `Cursor`, `Sort` (`$ref: "#/components/parameters/..."`; the shared `Sort` has no enum, so the description states "Sort: `ownerRef` (default), `displayName`, `lastSeen`; a `-` prefix descends"). Responses `200` → `AdminTenantUserPage`, `400`, `401`, `403`, `429`, `500`, `503` (`$ref: "#/components/responses/..."`).
- `GET /v1/admin/tenants/{tenant}/users/{ownerRef}`: `operationId: getAdminTenantUser`, tag `admin`, parameters `Tenant` and `OwnerRef`. Responses `200` → `AdminTenantUser`, `400`, `401`, `403`, `404`, `429`, `500`, `503`.
- Both descriptions say: tenant-admin role on the named tenant; lists everyone who has signed in to the tenant (the principal directory), not only principals with usage or a limit; the detail answers `404` for a principal who never signed in.

Then run `cd web && npm run generate`. Until the routes exist `TestAuditCoverage_SpecMatchesTable` and `TestOpenAPI_EveryOperationMountedAndViceVersa` fail; Step 4 turns them green.

`internal/api/adminusers_test.go` (imports `context`, `net/http`, `net/url`, `testing`, `time`, `internal/store`). The env helper reuses Task 2's `newAdminMountEnv`:

```go
func newAdminUsersEnv(t *testing.T, src UserDirectorySource, limits UserLimitSource) *testEnv {
	t.Helper()
	return newAdminMountEnv(t, func(mux *http.ServeMux, a *Authenticator) {
		MountAdminUserRoutes(mux, a, NewAdminUsersHandler(src, limits, defaultTenants()))
	})
}

const adminUsersPath = "/v1/admin/tenants/tenant-a/users"

// adminUserPath is the detail path of ownerRef, escaped like userLimitPath.
func adminUserPath(ownerRef string) string { return adminUsersPath + "/" + url.PathEscape(ownerRef) }
```

```go
type fakeUserDirectory struct {
	users   []store.TenantUser
	gotPage store.UserPage
}

func (f *fakeUserDirectory) ListTenantUsers(_ context.Context, tenant string, p store.UserPage) ([]store.TenantUser, error) {
	f.gotPage = p
	if tenant != "tenant-a" {
		return nil, nil
	}
	if len(f.users) > p.Limit+1 {
		return f.users[:p.Limit+1], nil
	}
	return f.users, nil
}

func (f *fakeUserDirectory) GetTenantUser(_ context.Context, tenant, ref string) (store.TenantUser, bool, error) {
	for _, u := range f.users {
		if tenant == "tenant-a" && u.OwnerRef == ref {
			return u, true, nil
		}
	}
	return store.TenantUser{}, false, nil
}

func directoryUsers() []store.TenantUser {
	seen := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	return []store.TenantUser{
		{OwnerRef: "https://idp|alice", Subject: "alice", DisplayName: "Alice", LastSeenAt: seen, Workspaces: 1},
		{OwnerRef: "https://idp|bob", Subject: "bob", DisplayName: "Bob", LastSeenAt: seen, Workspaces: 0},
		{OwnerRef: "https://idp|carol", Subject: "carol", DisplayName: "Carol", LastSeenAt: seen, Workspaces: 3},
	}
}

func TestAdminUsers_ListIncludesUsersWithoutWorkspacesOrLimits(t *testing.T) {
	limits := newFakeUserLimitSource()
	limits.seed("https://idp|carol", 2) // an override row; Report rebuilds UserLimits from these seeds
	env := newAdminUsersEnv(t, &fakeUserDirectory{users: directoryUsers()}, limits)
	sess, csrf := loginAdmin(t, env, "admin-a")
	res := doReq(t, env, sess, csrf, http.MethodGet, adminUsersPath+"?limit=2", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	page := decodeBody[Page[adminTenantUser]](t, res)
	if len(page.Items) != 2 || page.Items[1].OwnerRef != "https://idp|bob" || page.NextCursor == "" {
		t.Fatalf("page = %+v (bob has no workspace and no override, and must still be listed)", page)
	}
	res = doReq(t, env, sess, csrf, http.MethodGet, adminUserPath("https://idp|carol"), "", nil)
	u := decodeBody[adminTenantUser](t, res)
	if u.Limits.Limit == nil || *u.Limits.Limit != 2 || u.Limits.ETag == "" || u.Workspaces != 3 {
		t.Fatalf("detail = %+v", u)
	}
}

func TestAdminUsers_DetailNotFoundAndBadRef(t *testing.T) {
	env := newAdminUsersEnv(t, &fakeUserDirectory{users: directoryUsers()}, &fakeUserLimitSource{})
	sess, csrf := loginAdmin(t, env, "admin-a")
	if res := doReq(t, env, sess, csrf, http.MethodGet, adminUserPath("https://idp|nobody"), "", nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown user: %d", res.StatusCode)
	}
	if res := doReq(t, env, sess, csrf, http.MethodGet, adminUserPath("no-pipe"), "", nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad ref: %d", res.StatusCode)
	}
	if res := doReq(t, env, sess, csrf, http.MethodGet, adminUsersPath+"?sort=email", "", nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad sort: %d", res.StatusCode)
	}
}
```

`fakeUserLimitSource`, `newFakeUserLimitSource()` and its `seed(owner, max)` are in `adminuserlimit_test.go:31-60`. `Report` ignores any `report.UserLimits` you set on the struct and rebuilds the override maps from `seed`, so always seed through `seed`.

Add a sort/page test for the cursor key, appended to `adminusers_test.go`:

```go
func TestAdminUsers_CursorCarriesSortKey(t *testing.T) {
	dir := &fakeUserDirectory{users: directoryUsers()}
	env := newAdminUsersEnv(t, dir, newFakeUserLimitSource())
	sess, csrf := loginAdmin(t, env, "admin-a")
	res := doReq(t, env, sess, csrf, http.MethodGet, adminUsersPath+"?limit=2&sort=-lastSeen", "", nil)
	page := decodeBody[Page[adminTenantUser]](t, res)
	if dir.gotPage.Sort != "lastSeen" || !dir.gotPage.Desc || page.NextCursor == "" {
		t.Fatalf("page request = %+v, cursor %q", dir.gotPage, page.NextCursor)
	}
	res = doReq(t, env, sess, csrf, http.MethodGet, adminUsersPath+"?limit=2&sort=-lastSeen&cursor="+url.QueryEscape(page.NextCursor), "", nil)
	if res.StatusCode != http.StatusOK || dir.gotPage.After == nil || dir.gotPage.After.OwnerRef != "https://idp|bob" ||
		dir.gotPage.After.Key != "2026-10-13T09:00:00Z" {
		t.Fatalf("cursor not decoded: status=%d after=%+v", res.StatusCode, dir.gotPage.After)
	}
	// A cursor minted under another sort is refused.
	res = doReq(t, env, sess, csrf, http.MethodGet, adminUsersPath+"?limit=2&sort=ownerRef&cursor="+url.QueryEscape(page.NextCursor), "", nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("cross-sort cursor: %d, want 400", res.StatusCode)
	}
}
```

Then the checklist items 4 to 6 (line numbers are origin/main `5706fe5`; Task 2 added the audit lines next to these):
- `auditroutes_test.go`, `newCoverageEnv` (`:186`): after the audit mount add `MountAdminUserRoutes(mux, a, NewAdminUsersHandler(&fakeUserDirectory{users: directoryUsers()}, newFakeUserLimitSource(), tenants).WithAuditSink(sink))`.
- `auditroutes_test.go`, `TestAuditCoverage_MountedMuxEmitsEventPerRoute` (`:282`), `routes` table (normalized `{}` ops):
  ```go
  {"GET /v1/admin/tenants/{}/users", auditActionAdminUserList, func() {
  	doReq(t, env, sessA, csrfA, http.MethodGet, adminUsersPath, "", nil).Body.Close()
  }},
  {"GET /v1/admin/tenants/{}/users/{}", auditActionAdminUserGet, func() {
  	doReq(t, env, sessA, csrfA, http.MethodGet, adminUserPath("https://idp|alice"), "", nil).Body.Close()
  }},
  ```
- `openapi_contract_test.go`, `TestOpenAPIContract_ResponsesValidate` (`:513`) `calls` (raw `{tenant}` spec paths), one success and one refusal per operation; the refusals are 403 for the list and 404 for the detail, both declared:
  ```go
  {"GET", "/v1/admin/tenants/{tenant}/users", func() *http.Response { return doReq(t, env, sessA, csrfA, "GET", adminUsersPath, "", nil) }},
  {"GET", "/v1/admin/tenants/{tenant}/users", func() *http.Response { return doReq(t, env, sess, csrf, "GET", adminUsersPath, "", nil) }},
  {"GET", "/v1/admin/tenants/{tenant}/users/{ownerRef}", func() *http.Response {
  	return doReq(t, env, sessA, csrfA, "GET", adminUserPath("https://idp|alice"), "", nil)
  }},
  {"GET", "/v1/admin/tenants/{tenant}/users/{ownerRef}", func() *http.Response {
  	return doReq(t, env, sessA, csrfA, "GET", adminUserPath("https://idp|nobody"), "", nil)
  }},
  ```
- `tenantadmin_test.go`: add `ud *fakeUserDirectory` to `adminGroupFixtures` (`:31`) and `ud: &fakeUserDirectory{users: directoryUsers()}` in the literal in `newAdminGroupEnv` (`:45`); mount `MountAdminUserRoutes(mux, a, NewAdminUsersHandler(fx.ud, fx.uls, defaultTenants()))` after the audit mount; add `TestAdminUsers_CustomAdminGroups` after `TestAdminSessionPolicy_CustomAdminGroups` (`:260`) by copying it with `adminUsersPath`.

Run `go test ./internal/api/ -run 'AdminUsers|AuditCoverage|OpenAPI' -count=1`. Expected: FAIL to compile (undefined `UserDirectorySource`, `NewAdminUsersHandler`, `MountAdminUserRoutes`, `routeAdminUsersList`).

- [ ] **Step 4: Implement `internal/api/adminusers.go`**

```go
package api

import (
	"context"
	"net/http"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// UserDirectorySource reads the tenant's principal directory (store.DB).
type UserDirectorySource interface {
	ListTenantUsers(ctx context.Context, tenantID string, p store.UserPage) ([]store.TenantUser, error)
	GetTenantUser(ctx context.Context, tenantID, ownerRef string) (store.TenantUser, bool, error)
}

type adminTenantUser struct {
	OwnerRef    string              `json:"ownerRef"`
	Subject     string              `json:"subject"`
	DisplayName string              `json:"displayName"`
	Email       string              `json:"email"`
	LastSeenAt  time.Time           `json:"lastSeenAt"`
	Workspaces  int64               `json:"workspaces"`
	Limits      adminUserLimitEntry `json:"limits"`
}

// AdminUsersHandler serves the tenant user directory (F8): everyone who
// signed in to the tenant, not only principals holding workspaces.
type AdminUsersHandler struct {
	dir     UserDirectorySource
	limits  UserLimitSource
	tenants TenantResolver
	audit   observability.AuditSink
}

func NewAdminUsersHandler(src UserDirectorySource, limits UserLimitSource, t TenantResolver) *AdminUsersHandler {
	return &AdminUsersHandler{dir: src, limits: limits, tenants: t}
}

func (h *AdminUsersHandler) WithAuditSink(s observability.AuditSink) *AdminUsersHandler {
	h.audit = s
	return h
}

func MountAdminUserRoutes(mux RouteMux, authn *Authenticator, h *AdminUsersHandler) {
	mux.Handle(routeAdminUsersList, authn.RequireAuthPassive(audited(h.audit, routeAdminUsersList, http.HandlerFunc(h.List))))
	mux.Handle(routeAdminUserGet, authn.RequireAuthPassive(audited(h.audit, routeAdminUserGet, http.HandlerFunc(h.Get))))
}

var userDirectorySorts = []string{"ownerRef", "displayName", "lastSeen"}

func (h *AdminUsersHandler) List(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantAdminPrincipal(h.tenants, w, r); !ok {
		return
	}
	page, perr := ParsePage(r, userDirectorySorts, "ownerRef")
	if perr != nil {
		WriteError(w, r, perr)
		return
	}
	tenant := r.PathValue("tenant")
	p := store.UserPage{Limit: page.Limit, Sort: page.Sort.Field, Desc: page.Sort.Desc}
	if page.After != nil {
		p.After = &store.UserCursor{Key: page.After.Key, OwnerRef: page.After.ID}
	}
	users, err := h.dir.ListTenantUsers(r.Context(), tenant, p)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	rep, err := h.limits.Report(r.Context(), tenant)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	out := Page[adminTenantUser]{Items: make([]adminTenantUser, 0, len(users))}
	if len(users) > page.Limit {
		last := users[page.Limit-1]
		out.NextCursor = NextCursor(page.Sort, userSortKey(page.Sort.Field, last), last.OwnerRef)
		users = users[:page.Limit]
	}
	for _, u := range users {
		out.Items = append(out.Items, tenantUserView(rep, u))
	}
	respondJSON(w, out)
}

func (h *AdminUsersHandler) Get(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantAdminPrincipal(h.tenants, w, r); !ok {
		return
	}
	ref := r.PathValue("ownerRef")
	if msg := validOwnerRef(ref); msg != "" {
		writeError(w, r, CodeInvalidRequest, msg)
		return
	}
	auditSetDetail(r.Context(), "target", targetActorRef(ref))
	tenant := r.PathValue("tenant")
	u, ok, err := h.dir.GetTenantUser(r.Context(), tenant, ref)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if !ok {
		writeError(w, r, CodeNotFound, "user has not signed in to this tenant")
		return
	}
	rep, err := h.limits.Report(r.Context(), tenant)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	respondJSON(w, tenantUserView(rep, u))
}

func userSortKey(field string, u store.TenantUser) string {
	switch field {
	case "displayName":
		return u.DisplayName
	case "lastSeen":
		return u.LastSeenAt.UTC().Format(time.RFC3339Nano)
	}
	return u.OwnerRef
}

func tenantUserView(rep store.QuotaReport, u store.TenantUser) adminTenantUser {
	limits := userLimitEntry(rep, u.OwnerRef, Owner{Subject: u.Subject, DisplayName: u.DisplayName})
	return adminTenantUser{
		OwnerRef: u.OwnerRef, Subject: u.Subject, DisplayName: u.DisplayName, Email: u.Email,
		LastSeenAt: u.LastSeenAt, Workspaces: u.Workspaces, Limits: limits,
	}
}
```

`(*AdminUserLimitsHandler).entry` (`adminuserlimit.go:384`) reads no handler field. Move its body verbatim to a package function `func userLimitEntry(rep store.QuotaReport, ref string, owner Owner) adminUserLimitEntry` and make the method `return userLimitEntry(rep, ref, owner)`; the existing `List` and `Put` callers keep calling the method.

Add the audit actions to the constants in `auditroutes.go`: `auditActionAdminUserList = "admin.user.list"`, `auditActionAdminUserGet = "admin.user.get"`, the two `route…` consts from Step 3, and the `auditedRoutes` entries `routeAdminUsersList: {auditActionAdminUserList, "tenant"}` and `routeAdminUserGet: {auditActionAdminUserGet, "tenant"}`. Add `"admin.user.list", "admin.user.get"` to `auditEventActions` (`metrics.go:121`).

Wire in `wire.go` `newAppHandler`, after `adminAuditHandler` (Task 2): `adminUsersHandler := api.NewAdminUsersHandler(db, api.NewAdminUserLimitSource(db), tenants).WithAuditSink(appAudit)` (`*store.DB` satisfies `UserDirectorySource`), pass `adminUsers: adminUsersHandler` in the `appRoutes` literal, add the field `adminUsers *api.AdminUsersHandler` to `appRoutes`, and in `appMux` add `if h.adminUsers != nil { api.MountAdminUserRoutes(mux, authn, h.adminUsers) }`. `backend_test.go` needs no change (the field is nil there). Run the Step 3 command, then `go test ./internal/api/ ./internal/backend/ -count=1`. Expected: PASS.

- [ ] **Step 5: Web switches to the directory**

`web/src/admin/api.ts` (append; `exactOptionalPropertyTypes` is on, so build the query with conditional spreads like `listAdminUserLimitPage` does):

```ts
export type AdminTenantUser = components["schemas"]["AdminTenantUser"];

// GET .../users: everyone who signed in to the tenant (the principal
// directory). `sort` is ownerRef | displayName | lastSeen, `-` = descending.
export async function listAdminUserPage(
  api: ApiClient,
  tenant: string,
  cursor?: string,
  sort?: string,
  limit?: number,
): Promise<ServerPage<AdminTenantUser>> {
  return unwrap(
    await api.GET("/v1/admin/tenants/{tenant}/users", {
      params: {
        path: { tenant },
        query: { ...(sort ? { sort } : {}), ...(limit ? { limit } : {}), ...(cursor ? { cursor } : {}) },
      },
    }),
  );
}

export async function getAdminUser(api: ApiClient, tenant: string, ownerRef: string): Promise<AdminTenantUser> {
  return unwrap(await api.GET("/v1/admin/tenants/{tenant}/users/{ownerRef}", { params: { path: { tenant, ownerRef } } }));
}
```

`scanAdminUserLimits` loses its only caller (`UsersAdminPage.tsx`); delete it and the then-unused `SCAN_PAGE_LIMIT`/`scanCollection` imports only if `rg -n 'scanAdminUserLimits|SCAN_PAGE_LIMIT|scanCollection' web/src` shows no other user (`SCAN_PAGE_LIMIT` and `scanCollection` are used by other scans in `api.ts`, so only the function goes). `listAdminUserLimitPage` stays (`UserLimitsSection.tsx:172`).

`web/src/admin/UsersAdminPage.tsx`:
- `AdminUserRow` gains `ownerRef: string`, `email: string`, `lastSeenAt: string` and `workspaces: number`; `limitsEntry` becomes `AdminUserLimitEntry` taken from `user.limits`. The new signature is `userRowOf(user: AdminTenantUser, disks: ReadonlyMap<string, number>): AdminUserRow`: `subject: user.subject`, `displayName: user.displayName || user.subject`, `running: user.limits.running`, `override: user.limits.limit`, `effective: user.limits.effective`, `retainedDisks: disks.get(user.subject) ?? 0`, `limitsEntry: user.limits`, plus the four new fields. Replace the `listAdminUserLimitPage`/`scanAdminUserLimits`/`AdminUserLimitEntry` imports with `listAdminUserPage`, `getAdminUser`, `type AdminTenantUser` (keep `type AdminUserLimitEntry` for the row type).
- `userDetailPath(ownerRef: string)` stops percent-encoding and carries the reference as base64url. Reason: an owner reference is `https://idp.example.com|sub`, and `build/frontend/main.go:235` answers 404 for any path whose last segment, after decoding `%2F`, contains a `.` (its "looks like a missing asset" rule), so a reload or deep link of `/admin/users/https%3A%2F%2Fidp.example.com%7Csub` would 404 for nearly every real issuer. A base64url segment (`[A-Za-z0-9_-]`) has no dot or slash and needs no server change. The list calls it with `r.ownerRef`. `rowKey` becomes `(r) => r.ownerRef` (`subject` is not unique across issuers). Add and export:
  ```ts
  export function userDetailPath(ownerRef: string): string {
    const bin = Array.from(new TextEncoder().encode(ownerRef), (b) => String.fromCharCode(b)).join("");
    const seg = btoa(bin).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
    return "/admin/users/" + seg;
  }

  // Inverse of the path segment userDetailPath builds; undefined for a segment that is not valid base64url UTF-8.
  export function ownerRefFromParam(param: string): string | undefined {
    try {
      const bin = atob(param.replaceAll("-", "+").replaceAll("_", "/"));
      return new TextDecoder("utf-8", { fatal: true }).decode(Uint8Array.from(bin, (c) => c.charCodeAt(0)));
    } catch {
      return undefined;
    }
  }
  ```
- `USER_SORT_FIELDS = { user: "displayName", lastSeen: "lastSeen" } as const`; the initial sort is `{ key: "lastSeen", direction: "descending" }`; the load callback calls `listAdminUserPage(api, tenant, cursor, sort)` and its empty fallback is `{ items: [] as AdminTenantUser[] }`.
- Append two columns after `disks` (appending keeps the existing Playwright `getByRole("cell").nth(2)` index on the effective limit valid): `{ key: "lastSeen", header: t("admin.users.column.lastSeen"), sortable: true, hideOnMobile: true, render: (r) => <time dateTime={r.lastSeenAt} title={formatDateTime(r.lastSeenAt)}>{formatAgo(r.lastSeenAt)}</time> }` and `{ key: "workspaces", header: t("admin.users.column.workspaces"), align: "end", hideOnMobile: true, render: (r) => r.workspaces }`. Import `formatAgo` from `./format` (`formatAge`/`formatAgo`/`formatDateTime` are the helpers there; there is no `formatRelative`).
- Delete the `<p className="tc-admin-muted">{t("admin.users.note")}</p>` line and the three comments that say the page is not a directory (the interface comment above `AdminUserRow`, and the `// The collection sorts on ownerRef only` comment).
- Detail: replace `loadUserDetail` with the version below and `UserDetailPage({ subject, now })` with `UserDetailPage({ ownerRef, now })`:
  ```tsx
  interface UserDetailData {
    tenant: string;
    user: AdminTenantUser | undefined;
    disks: ScopedRetainedData[];
  }

  // The directory answers 404 for a principal who never signed in; that is the
  // not-found state, not an error alert.
  async function loadUserDetail(api: ApiClient, ownerRef: string): Promise<UserDetailData> {
    const me = await loadMe(api);
    const [user, disks] = await Promise.all([
      getAdminUser(api, me.tenant, ownerRef).catch((e: unknown) => {
        if (isPortalApiError(e) && e.httpStatus === 404) return undefined;
        throw e;
      }),
      scanScopedData(api, "tenant"),
    ]);
    return { tenant: me.tenant, user, disks: user ? disks.filter((d) => d.owner?.subject === user.subject) : [] };
  }
  ```
  with `import { isPortalApiError } from "../api/errors";`. In the component: `useLoader((a) => loadUserDetail(a, ownerRef), "admin-user:" + ownerRef)`; `row = u.data?.user ? userRowOf(u.data.user, new Map([[u.data.user.subject, u.data.disks.length]])) : undefined`; the workspaces filter is `(ws.data ?? []).filter((w) => w.owner?.subject === u.data?.user?.subject)` (display filter only: `WorkspaceView.owner` carries no issuer, and authorization never reads it); the `LimitDialog` receives `target={row.limitsEntry}` as before. Add two rows to the `DescriptionList` after "Owner reference": `{ term: t("admin.userDetail.field.email"), detail: row.email || "—" }` and `{ term: t("admin.userDetail.field.lastSeen"), detail: <time dateTime={row.lastSeenAt}>{formatDateTime(row.lastSeenAt)}</time> }`.
- Task 4 later renders `UserSessionsSection` in this page from `u.data.tenant` and `u.data.user`.

In `routes.tsx`, the path becomes `/admin/users/:ownerRef` (the router already decodes `:param` segments, `app/router.tsx:31`, which is a no-op for base64url) and renders `<UserDetailPage ownerRef={ownerRefFromParam(p.ownerRef!) ?? ""} />` (import `ownerRefFromParam` next to the page components). `loadUserDetail` returns `{ tenant, user: undefined, disks: [] }` without calling the API when `ownerRef === ""`, so a mangled link shows the not-found state.

Strings (en, then vi in the same keys; `|` in the table is plain in the `.ts` files):

| key | en | vi |
|---|---|---|
| `admin.users.column.lastSeen` | Last seen | Hoạt động gần nhất |
| `admin.users.column.workspaces` | Workspaces | Workspace |
| `admin.userDetail.field.email` | Email | Email |
| `admin.userDetail.field.lastSeen` | Last seen | Hoạt động gần nhất |

Changed values (same keys, both locales): `admin.users.description` becomes "Everyone who has signed in to this tenant." / "Mọi người đã đăng nhập vào tenant này."; `admin.users.empty.all` becomes "No one has signed in to this tenant yet." / "Chưa có ai đăng nhập vào tenant này."; `admin.userDetail.notFound.body` becomes "This user has not signed in to this tenant." / "Người dùng này chưa đăng nhập vào tenant này.". Delete `admin.users.note` from `en/admin.ts:222` and `vi/admin.ts:221`, and update the comment above `admin.users.title` (`en/admin.ts:217`, "principals known to the tenant (quota usage or a per-user limit row)") to say the page lists the principal directory. `docs/releases/v1.2.0.md:426` documents the old limitation; releases are frozen history and stay untouched.

`web/tests/mock-api/admin.ts` (the contract mock; `OTHER_USERS` is at `:58`, `ownerRefOf` at `:214`):
- export `SIGNED_IN_ONLY: Owner[] = [{ subject: "user-01J4ZDMGH", displayName: "Margaret Hamilton" }, { subject: "user-01J4ZDKJN", displayName: "Katherine Johnson" }]`: principals who signed in and hold no workspace, disk or override;
- `function directoryUsers()` returns one `{ ownerRef, subject, displayName, email, lastSeenAt, workspaces, limits }` per principal of `[t.me, ...OTHER_USERS, ...SIGNED_IN_ONLY]` (5 rows). `lastSeenAt` is a fixed ISO instant per principal (stagger them by an hour so the `lastSeen` order is deterministic), `workspaces` counts `state.workspaces` whose `t.ownerOf(id).subject` is the principal, and `limits` is `entryOf({ ownerRef, subject, displayName, running })` (the existing helper at `:404`, `running` from `quotaBody().perUser`, 0 when absent);
- `adminUsers(req, tenant, ownerRef)`: the same 403 as `adminUserLimits` for a non-admin or foreign tenant, `405` for non-GET, `t.takeFailure(req)`, then for the collection `paginate(req, directoryUsers(), { ownerRef: byString((u) => u.ownerRef), displayName: byString((u) => u.displayName), lastSeen: byString((u) => u.lastSeenAt) }, "ownerRef", (u) => u.ownerRef)` and for an item the matching row or `err(404, "NOT_FOUND", "user has not signed in to this tenant", false)`;
- in `api(req)` add, next to `mUL`: `const mU = path.match(/^\/v1\/admin\/tenants\/([^/]+)\/users(?:\/([^/]+))?$/); if (mU) return adminUsers(req, mU[1], mU[2] === undefined ? undefined : decodeURIComponent(mU[2]));`.

`web/tests/unit/admin/users.test.tsx` (existing; every case keeps its name unless noted):
- Replace the `lRow` fixture with `uRow(overrides: Partial<AdminTenantUser> = {})` returning `{ ownerRef: "iss|u1", subject: "u1", displayName: "User One", email: "", lastSeenAt: "2026-10-13T09:00:00Z", workspaces: 0, limits: { ownerRef: "iss|u1", subject: "u1", displayName: "User One", limit: null, effective: null, running: 0 } , ...overrides }` and import `AdminTenantUser` instead of `AdminUserLimitEntry`.
- `userRowOf` cases: `userRowOf(uRow({ workspaces: 3, limits: { ...uRow().limits, running: 2, limit: 7, effective: 7, etag: '"ul-3"' } }), counts)`; expect `{ subject: "u1", ownerRef: "iss|u1", running: 2, override: 7, effective: 7, retainedDisks: 2, workspaces: 3 }`; add the case "maps a principal with no override to override: null" (`uRow()` gives `override: null`, `effective: null`). The display-name and filter cases swap `lRow(...)` for `uRow(...)` (the display name fallback is `uRow({ displayName: "" })`).
- Add `it("userDetailPath is a dot-free, slash-free segment that round-trips")`: for each of `"https://idp.example.com/realms/x|alice@example.com"` and `"https://idp.invalid|usér-1"`, `userDetailPath(ref)` matches `/^\/admin\/users\/[A-Za-z0-9_-]+$/` and `ownerRefFromParam(userDetailPath(ref).split("/").pop()!)` equals `ref`; and `ownerRefFromParam("***")` is `undefined` (import `userDetailPath`, `ownerRefFromParam`).
- `UsersAdminPage` "lists the per-user limits items…": rename to "lists the signed-in principals with workspaces, limits and disk counts". Names to find: Ada, Grace, Linus plus `Margaret Hamilton` (a signed-in user with no workspace). The Grace link `href` is `userDetailPath("https://idp.invalid|" + OTHER_USERS[0]!.subject)`. The request check reads `r.path === "/v1/admin/tenants/" + TENANT + "/users"` and expects `sort` to equal `-lastSeen`. Replace the "not a full tenant directory" assertion with `expect(screen.queryByText(/not a full tenant directory/)).not.toBeInTheDocument()`.
- Replace "includes limits-only principals" with "lists a signed-in user who holds nothing": Margaret's row exists, has `0` in the running cell, `Default` (inherit) in the override cell and links to her detail path.
- `UserDetailPage` cases render `<UserDetailPage ownerRef={GRACE_REF} />` with `const GRACE_REF = "https://idp.invalid|" + GRACE`, `LINUS_REF` likewise; the not-found case renders `ownerRef="https://idp.invalid|user-nobody"` and still expects "User not found" and the "Back to users" link (the mock answers 404 now). Add to the first detail case: `expect(screen.getByText("Last seen", { selector: "dt" })).toBeInTheDocument()`.

`web/tests/admin.spec.ts`, in "a tenant admin opens a user's detail and edits their running limit" (`:137`):
- after the Grace and Linus row checks add `await expect(table.getByRole("row", { name: /Margaret Hamilton/ })).toBeVisible();` (a user with no workspaces is listed) and fix the stale comment above it ("The paged user directory lists everyone who signed in");
- `await page.waitForURL(/\/admin\/users\/user-01J4ZDGRC$/)` becomes `await page.waitForURL("**/admin/users/" + Buffer.from("https://idp.invalid|user-01J4ZDGRC").toString("base64url"))`;
- after the heading check add `await expect(page.getByText("Last seen", { exact: true })).toBeVisible();`;
- add a second test `"a signed-in user with no workspaces has a detail page"`: `makeAdmin`, `login(page, "/admin/users")`, click the "Margaret Hamilton" link, expect heading "Margaret Hamilton", expect the workspaces table to show "This user has no workspaces." and "Last seen" to be visible;
- the later `graceRow.getByRole("cell").nth(2)` check is unchanged (the new columns are appended).
- in "a regular user is redirected away from /admin/users and tenant reads stay 403" (`:122`) add `const dir = await page.request.get("/v1/admin/tenants/acme/users"); expect(dir.status()).toBe(403);` next to the user-limits check.

`web/tests/a11y.spec.ts:36`: replace the string `"/admin/users/user-01J4ZDGRC"` in `ROUTES` with `"/admin/users/aHR0cHM6Ly9pZHAuaW52YWxpZHx1c2VyLTAxSjRaREdSQw"` (the base64url of `https://idp.invalid|user-01J4ZDGRC`, the mock's Grace Hopper).

Run:

```bash
cd web && npm run generate && npm run typecheck && npm run lint:strings && npx vitest run tests/unit/admin tests/unit/i18n && npx playwright test tests/admin.spec.ts tests/a11y.spec.ts -g "user|axe"
```

Expected: PASS. (`-g "user|axe"` covers the users tests and the whole axe sweep, including the changed detail route.)

- [ ] **Step 6: Threat model and commit**

Append row S43 to the `| # | Item | State today |` table in `docs/security/threat-model.md` §6, after S42 (Task 2):

`| S43 | Tenant user directory | Implemented: GET /v1/admin/tenants/{t}/users[/{ownerRef}] reads principal_directory scoped by tenant in SQL; tenant-admin only; display data only (names and emails of the caller's own tenant members), never an authorization input; the detail route is addressed by owner reference, carried base64url in the portal URL; detail reads are audited with a pseudonymous target |`

```bash
go test -count=1 ./internal/store/ ./internal/api/ ./internal/backend/
git add internal/store/users.go internal/store/users_test.go internal/api internal/backend internal/observability web docs/security/threat-model.md
git commit -m "feat(admin): tenant user directory backs the Users page

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Admin live sessions and "sign out everywhere" for a user (F5) — tier: integrate

**Files:**
- Create: `internal/store/leases.go`, `internal/store/leases_test.go`
- Create: `internal/api/adminsessions.go`, `internal/api/adminsessions_test.go`
- Modify: `internal/api/{auditroutes.go,openapi.yaml,auditroutes_test.go,openapi_contract_test.go,tenantadmin_test.go}`, `internal/observability/metrics.go:121`
- Modify: `internal/backend/wire.go` (`appRoutes.adminSessions` plus its `appMux` mount; `backend_test.go` needs no change because a nil `appRoutes` field skips the mount)
- Create: `web/src/admin/UserSessionsSection.tsx`. Modify: `web/src/admin/{api.ts,UsersAdminPage.tsx}`, `web/src/i18n/{en,vi}/admin.ts`, `web/tests/mock-api/admin.ts`, `web/tests/admin.spec.ts`
- Kind e2e prerequisites (no task seeds an admin login today: the quickstart realm has one user per tenant and no `groups` claim): Modify `hack/quickstart/{realm-export.json,common.sh,up.sh}`, `docs/quickstart.md`. Modify `hack/quickstart/smoke/smoke.spec.ts` (admin constants plus one admin test)
- Modify: `docs/security/threat-model.md` (row S44)

**Interfaces:**
- Consumes: `api.PrincipalRevoker` / `api.RevokeAllResult` (`revokeall.go:25-41`); `broker.PublicRevoker{B: brk}` (the same value `wire.go` passes to `Authenticator.WithPrincipalRevoker`; it satisfies `PrincipalRevoker`); `Directory`/`resolveOwners` (`principal.go`), `validOwnerRef`/`targetActorRef`/`writeStoreError` (`adminuserlimit.go`), `sessionRevokeTimeout` (`auth.go`), `tenantAdminPrincipal`; `appRoutes` (Task 2); from Task 3, `UserDetailPage({ ownerRef, now })` in `UsersAdminPage.tsx`, which loads `{ tenant, user, disks }` (read as `u.data.tenant` and `u.data.user`, `user: AdminTenantUser` with `ownerRef` and `displayName`), the route `/admin/users/:ownerRef` whose path segment is the base64url of the UTF-8 owner reference (`userDetailPath`, `ownerRefFromParam`; `build/frontend/main.go:235` 404s a last segment containing a dot, so never percent-encode the ref into the page path).
- Produces:
  - `type store.LiveLease struct{ ID, WorkspaceID, WorkspaceName, OwnerRef string; CreatedAt time.Time; LastRenewedAt *time.Time; ExpiresAt time.Time }`
  - `type store.LeaseFilter struct{ OwnerRef string; Limit int; After *store.LeaseCursor }`, `type store.LeaseCursor struct{ CreatedAt time.Time; ID string }`. `ID` is the **workspace id**: at most one lease per workspace is active (`connection_lease_one_active`), so `(created_at, workspace_id)` is a total order over live leases and the lease id never has to leave the store.
  - `func (d *store.DB) ListLiveLeases(ctx context.Context, tenantID string, now time.Time, f store.LeaseFilter) ([]store.LiveLease, error)`. It returns up to `f.Limit+1` rows, newest first. Task 8 reuses the active-lease predicate through `HasLiveLease`.
  - `func (d *store.DB) HasLiveLease(ctx context.Context, workspaceID string, now time.Time) (bool, error)`
  - `GET /v1/admin/tenants/{tenant}/sessions?ownerRef=&limit=&cursor=&sort=-createdAt` returns `Page[AdminLiveSession]`. The audit action is `admin.session.list`.
  - `POST /v1/admin/tenants/{tenant}/users/{ownerRef}/sessions:revoke-all` returns 200 `{sessions, tickets, leases}`. The audit action is `admin.session.revoke`.
  - `func leaseRef(id string) string` returns 16 hex chars of sha256(id), the same value `ConnectionState.LeaseRef` (`broker/connection_state.go`) gives the owner's tab. The lease id is a fencing handle and is never returned, not even inside the cursor.
  - `NewAdminSessionsHandler(src LiveLeaseSource, dir Directory, revoker PrincipalRevoker, t TenantResolver) *AdminSessionsHandler`, `MountAdminSessionRoutes(mux, authn, h)`
  - Web: `listAdminSessionPage(api, tenant, ownerRef?, cursor?)`, `revokeAdminUserSessions(api, tenant, ownerRef): Promise<AdminRevokeResult>`, `<UserSessionsSection tenant ownerRef displayName />` (heading "Live sessions", a `region` landmark).
  - Kind quickstart: dev-realm admin user `demo-admin` (env `TCDI_QS_ADMIN_USER`, constant `ADMIN_USER` in `smoke.spec.ts`), member of the realm group `tenant-admin` that the new `groups` ID-token mapper emits, in tenant `tenant-a`, same password as `demo`. Tasks 5 and 8 reuse it.

- [ ] **Step 1: Failing store test**

`internal/store/leases_test.go`:

```go
package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func seedLiveLease(t *testing.T, db *store.DB, id, ws, tenant, owner, state string, created, expires time.Time) {
	t.Helper()
	if _, err := db.Pool().Exec(context.Background(), `
		INSERT INTO connection_lease (id, workspace_id, tenant_id, principal_subject, runtime_generation, runtime_uid,
			fencing_version, gateway_id, state, created_at, expires_at, portal_session_digest)
		VALUES ($1, $2, $3, $4, 1, 'uid', 1, 'gw', $5, $6, $7, '\x00')`,
		id, ws, tenant, owner, state, created, expires); err != nil {
		t.Fatal(err)
	}
}

func TestLiveLeases_ListFiltersAndPages(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	now := time.Now()
	// lease-a1 and lease-a2 share created_at: the workspace id breaks the tie.
	created := []time.Time{now.Add(-3 * time.Minute), now.Add(-2 * time.Minute), now.Add(-2 * time.Minute)}
	for i := 0; i < 3; i++ {
		ws := fmt.Sprintf("ws-a%d", i)
		seedWorkspace(t, db, "t1", "https://idp|alice", ws)
		seedLiveLease(t, db, fmt.Sprintf("lease-a%d", i), ws, "t1", "https://idp|alice", "active", created[i], now.Add(time.Minute))
	}
	seedWorkspace(t, db, "t1", "https://idp|bob", "ws-b")
	seedLiveLease(t, db, "lease-b", "ws-b", "t1", "https://idp|bob", "active", now.Add(-10*time.Minute), now.Add(-time.Second)) // expired by time
	seedWorkspace(t, db, "t1", "https://idp|bob", "ws-b2")
	seedLiveLease(t, db, "lease-b2", "ws-b2", "t1", "https://idp|bob", "revoked", now, now.Add(time.Minute))
	seedWorkspace(t, db, "t2", "https://idp|eve", "ws-e")
	seedLiveLease(t, db, "lease-e", "ws-e", "t2", "https://idp|eve", "active", now, now.Add(time.Minute))

	page, err := db.ListLiveLeases(ctx, "t1", now, store.LeaseFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 3 || page[0].ID != "lease-a2" || page[1].ID != "lease-a1" {
		t.Fatalf("page = %+v; want newest first, live only, tenant only", page)
	}
	rest, _ := db.ListLiveLeases(ctx, "t1", now, store.LeaseFilter{Limit: 2, After: &store.LeaseCursor{CreatedAt: page[1].CreatedAt, ID: page[1].WorkspaceID}})
	if len(rest) != 1 || rest[0].ID != "lease-a0" {
		t.Fatalf("rest = %+v", rest)
	}
	// A cursor parked on the first of two equal created_at rows still returns the second.
	tie, _ := db.ListLiveLeases(ctx, "t1", now, store.LeaseFilter{Limit: 5, After: &store.LeaseCursor{CreatedAt: page[0].CreatedAt, ID: page[0].WorkspaceID}})
	if len(tie) != 2 || tie[0].ID != "lease-a1" || tie[1].ID != "lease-a0" {
		t.Fatalf("tie = %+v; the (created_at, workspace_id) keyset must not skip equal timestamps", tie)
	}
	// The tenant filter is part of the SQL: t2's lease is invisible to t1 and the reverse.
	if eve, _ := db.ListLiveLeases(ctx, "t2", now, store.LeaseFilter{Limit: 10}); len(eve) != 1 || eve[0].ID != "lease-e" {
		t.Fatalf("t2 = %+v", eve)
	}
	if none, _ := db.ListLiveLeases(ctx, "t1", now, store.LeaseFilter{Limit: 10, OwnerRef: "https://idp|eve"}); len(none) != 0 {
		t.Fatalf("another tenant's principal listed under t1: %+v", none)
	}
	bob, _ := db.ListLiveLeases(ctx, "t1", now, store.LeaseFilter{Limit: 10, OwnerRef: "https://idp|bob"})
	if len(bob) != 0 {
		t.Fatalf("expired/revoked leases listed: %+v", bob)
	}
	if live, _ := db.HasLiveLease(ctx, "ws-a0", now); !live {
		t.Fatal("HasLiveLease(ws-a0) = false")
	}
	if live, _ := db.HasLiveLease(ctx, "ws-b", now); live {
		t.Fatal("HasLiveLease on an expired lease = true")
	}
}
```

`seedWorkspace(t, db, tenant, ownerSubject, wsUID)` and `newDB` are the existing helpers in `internal/store/pg_test.go`. Run `go test ./internal/store/ -run TestLiveLeases -count=1`. Expected: FAIL to compile (`db.ListLiveLeases undefined`).

- [ ] **Step 2: Implement `internal/store/leases.go`**

```go
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// LiveLease is one active, unexpired connection lease — a live desktop
// session — as the admin sessions table shows it.
type LiveLease struct {
	ID            string
	WorkspaceID   string
	WorkspaceName string
	OwnerRef      string
	CreatedAt     time.Time
	LastRenewedAt *time.Time
	ExpiresAt     time.Time
}

// LeaseCursor is the keyset position (created_at, workspace id) of the last
// row of the previous page. ID is the WORKSPACE id: only one lease per
// workspace is active (connection_lease_one_active), so the pair is unique
// among live leases and the lease id itself never leaves the store.
type LeaseCursor struct {
	CreatedAt time.Time
	ID        string
}

// LeaseFilter narrows ListLiveLeases.
type LeaseFilter struct {
	OwnerRef string
	Limit    int
	After    *LeaseCursor
}

// liveLeasePredicate is the single definition of "live": state active and
// not past expires_at. HasLiveLease and ListLiveLeases share it.
const liveLeasePredicate = `l.state = 'active' AND l.expires_at > $2`

// ListLiveLeases returns up to f.Limit+1 live leases of tenantID, newest first.
func (d *DB) ListLiveLeases(ctx context.Context, tenantID string, now time.Time, f LeaseFilter) ([]LiveLease, error) {
	args := []any{tenantID, now}
	where := `l.tenant_id = $1 AND ` + liveLeasePredicate
	if f.OwnerRef != "" {
		args = append(args, f.OwnerRef)
		where += fmt.Sprintf(" AND l.principal_subject = $%d", len(args))
	}
	if f.After != nil {
		args = append(args, f.After.CreatedAt, f.After.ID)
		where += fmt.Sprintf(" AND (l.created_at, l.workspace_id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, f.Limit+1)
	rows, err := d.pool.Query(ctx, `
		SELECT l.id, l.workspace_id, w.name, l.principal_subject, l.created_at, l.last_renewed_at, l.expires_at
		FROM connection_lease l JOIN workspaces w ON w.id = l.workspace_id AND w.tenant_id = l.tenant_id
		WHERE `+where+fmt.Sprintf(` ORDER BY l.created_at DESC, l.workspace_id DESC LIMIT $%d`, len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("store: list live leases %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (LiveLease, error) {
		var l LiveLease
		err := row.Scan(&l.ID, &l.WorkspaceID, &l.WorkspaceName, &l.OwnerRef, &l.CreatedAt, &l.LastRenewedAt, &l.ExpiresAt)
		return l, err
	})
}

// HasLiveLease reports whether workspaceID has a live lease at now.
func (d *DB) HasLiveLease(ctx context.Context, workspaceID string, now time.Time) (bool, error) {
	var live bool
	if err := d.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM connection_lease l WHERE l.workspace_id = $1 AND `+liveLeasePredicate+`)`,
		workspaceID, now).Scan(&live); err != nil {
		return false, fmt.Errorf("store: has live lease %w", err)
	}
	return live, nil
}
```

Run the Step 1 command. Expected: PASS.

- [ ] **Step 3: Contract and failing API tests**

Apply the New API operation checklist.

`internal/api/auditroutes.go`: add the constants and the two `auditedRoutes` entries (the target is the tenant; the revoked principal travels in `details.target`):

```go
const (
	auditActionAdminSessionList   = "admin.session.list"
	auditActionAdminSessionRevoke = "admin.session.revoke"

	routeAdminSessionsList  = "GET /v1/admin/tenants/{tenant}/sessions"
	routeAdminUserRevokeAll = "POST /v1/admin/tenants/{tenant}/users/{ownerRef}/sessions:revoke-all"
)
```

and in `auditedRoutes`: `routeAdminSessionsList: {auditActionAdminSessionList, "tenant"}, routeAdminUserRevokeAll: {auditActionAdminSessionRevoke, "tenant"},`. In `internal/observability/metrics.go:121` add `"admin.session.list": {}, "admin.session.revoke": {},` to `auditEventActions`.

OpenAPI: add the two paths and the schemas below to `internal/api/openapi.yaml` (the shared `Tenant`, `OwnerRef`, `PageLimit`, `Cursor`, `Sort` parameters and the `BadRequest`/`Unauthorized`/`Forbidden`/`RateLimited`/`InternalError`/`Unavailable` responses already exist):

```yaml
  /v1/admin/tenants/{tenant}/sessions:
    get:
      operationId: listAdminSessions
      summary: Live desktop sessions of a tenant (paged)
      description: |
        Administrator list of the tenant's live connection leases (state
        `active` and not past `expiresAt`), read from the shared PostgreSQL
        session directory, so every backend replica answers the same.
        `ownerRef` narrows the list to one principal. Items never carry the
        lease id: `leaseRef` is a stable, non-secret reference. Sort:
        `-createdAt` only (newest first; anything else is `INVALID_REQUEST`).
        Requires the tenant-admin role on the named tenant.
      tags: [admin]
      parameters:
        - $ref: "#/components/parameters/Tenant"
        - name: ownerRef
          in: query
          required: false
          description: Only this principal's sessions (`issuer|sub`).
          schema:
            type: string
            minLength: 3
            maxLength: 512
        - $ref: "#/components/parameters/PageLimit"
        - $ref: "#/components/parameters/Cursor"
        - $ref: "#/components/parameters/Sort"
      responses:
        "200":
          description: One page of live sessions.
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/AdminLiveSessionPage"
        "400":
          $ref: "#/components/responses/BadRequest"
        "401":
          $ref: "#/components/responses/Unauthorized"
        "403":
          $ref: "#/components/responses/Forbidden"
        "429":
          $ref: "#/components/responses/RateLimited"
        "500":
          $ref: "#/components/responses/InternalError"
        "503":
          $ref: "#/components/responses/Unavailable"

  /v1/admin/tenants/{tenant}/users/{ownerRef}/sessions:revoke-all:
    post:
      operationId: revokeAdminUserSessions
      summary: Sign a user out everywhere
      description: |
        The administrator variant of `POST /v1/me/sessions:revoke-all`: in
        one store transaction it ends every portal session the named
        principal holds in the named tenant and revokes their active
        connection leases and outstanding launch tickets. The administrator's
        own session is untouched (unless they name themselves). Workspaces
        keep running. Requires the tenant-admin role on the named tenant and
        the `X-CSRF-Token` header; audited as `admin.session.revoke`.
      tags: [admin]
      parameters:
        - $ref: "#/components/parameters/Tenant"
        - $ref: "#/components/parameters/OwnerRef"
      responses:
        "200":
          description: What the revocation destroyed (all zero when the principal had nothing).
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/AdminRevokeResult"
        "400":
          $ref: "#/components/responses/BadRequest"
        "401":
          $ref: "#/components/responses/Unauthorized"
        "403":
          $ref: "#/components/responses/Forbidden"
        "429":
          $ref: "#/components/responses/RateLimited"
        "500":
          $ref: "#/components/responses/InternalError"
        "503":
          $ref: "#/components/responses/Unavailable"
```

```yaml
    AdminLiveSession:
      type: object
      additionalProperties: false
      required: [leaseRef, workspaceId, workspaceName, ownerRef, owner, createdAt, expiresAt]
      properties:
        leaseRef: {type: string, pattern: '^[0-9a-f]{16}$', description: "Stable non-secret reference (SHA-256 prefix of the lease id)."}
        workspaceId: {type: string}
        workspaceName: {type: string}
        ownerRef: {type: string}
        owner: {$ref: '#/components/schemas/Owner'}
        createdAt: {type: string, format: date-time}
        lastRenewedAt: {type: string, format: date-time}
        expiresAt: {type: string, format: date-time}
    AdminLiveSessionPage:
      type: object
      additionalProperties: false
      required: [items]
      properties:
        items: {type: array, items: {$ref: '#/components/schemas/AdminLiveSession'}}
        nextCursor: {$ref: '#/components/schemas/NextCursor'}
    AdminRevokeResult:
      type: object
      additionalProperties: false
      required: [sessions, tickets, leases]
      properties:
        sessions: {type: integer, minimum: 0}
        tickets: {type: integer, minimum: 0}
        leases: {type: integer, minimum: 0}
```

`internal/api/adminsessions_test.go`:

```go
package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

const adminSessionsPath = "/v1/admin/tenants/tenant-a/sessions"

func adminRevokePath(ownerRef string) string {
	return "/v1/admin/tenants/tenant-a/users/" + url.PathEscape(ownerRef) + "/sessions:revoke-all"
}

type fakeLeaseSource struct {
	leases []store.LiveLease // newest first; returned whole, like the store's limit+1 read
	got    store.LeaseFilter
	calls  int
}

func (f *fakeLeaseSource) ListLiveLeases(_ context.Context, tenant string, _ time.Time, fl store.LeaseFilter) ([]store.LiveLease, error) {
	f.calls++
	f.got = fl
	if tenant != "tenant-a" {
		return nil, nil
	}
	return f.leases, nil
}

type fakeAdminRevoker struct {
	tenant, issuer, subject string
	calls                   int
	err                     error
}

func (f *fakeAdminRevoker) RevokePrincipalSessions(_ context.Context, tenant, issuer, subject string) (RevokeAllResult, error) {
	f.calls++
	f.tenant, f.issuer, f.subject = tenant, issuer, subject
	return RevokeAllResult{Sessions: 2, Tickets: 1, Leases: 1}, f.err
}

// newAdminSessionsEnv mounts the two F5 routes over RequestID(Audit(logger)(mux)),
// built like newSessionPolicyEnv (adminsessionpolicy_test.go).
func newAdminSessionsEnv(t *testing.T, src LiveLeaseSource, rev PrincipalRevoker, opts ...func(*AdminSessionsHandler)) *testEnv {
	t.Helper()
	iss, err := oidctest.NewIssuer()
	if err != nil {
		t.Fatalf("oidctest.NewIssuer: %v", err)
	}
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, nil))
	sessions := NewInMemorySessionStore(30 * time.Minute)
	a, err := NewAuthenticator(context.Background(), AuthConfig{
		Issuer:      iss.URL(),
		ClientID:    iss.ClientID,
		RedirectURL: "https://portal.test/auth/callback",
		LoginSealer: testLoginSealer(t),
	}, sessions, logger)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	mux := http.NewServeMux()
	h := NewAdminSessionsHandler(src, nil, rev, defaultTenants())
	for _, o := range opts {
		o(h)
	}
	mux.Handle("/auth/login", http.HandlerFunc(a.LoginHandler))
	mux.Handle("/auth/callback", http.HandlerFunc(a.CallbackHandler))
	MountAdminSessionRoutes(mux, a, h)
	srv := httptest.NewServer(RequestID(Audit(logger)(mux)))
	env := &testEnv{issuer: iss, auth: a, store: sessions, server: srv, logs: logBuf}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return env
}

func bodyOf(t *testing.T, res *http.Response) string {
	t.Helper()
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAdminSessions_ListHidesLeaseIDs(t *testing.T) {
	now := time.Now()
	src := &fakeLeaseSource{leases: []store.LiveLease{{ID: "lease-secret-1", WorkspaceID: "ws_1", WorkspaceName: "dev", OwnerRef: "https://idp|alice", CreatedAt: now, ExpiresAt: now.Add(time.Minute)}}}
	env := newAdminSessionsEnv(t, src, &fakeAdminRevoker{})
	sess, csrf := loginAdmin(t, env, "admin-a")
	res := doReq(t, env, sess, csrf, http.MethodGet, adminSessionsPath+"?ownerRef="+url.QueryEscape("https://idp|alice"), "", nil)
	status, body := res.StatusCode, bodyOf(t, res)
	if status != http.StatusOK || strings.Contains(body, "lease-secret-1") {
		t.Fatalf("status %d body %s: the lease id must never be returned", status, body)
	}
	if !strings.Contains(body, leaseRef("lease-secret-1")) || src.got.OwnerRef != "https://idp|alice" {
		t.Fatalf("body %s filter %+v", body, src.got)
	}
}

// The cursor carries the keyset position (created_at, workspace id); the
// lease id appears neither in items nor in the cursor.
func TestAdminSessions_PagingCursorHasNoLeaseID(t *testing.T) {
	now := time.Now()
	src := &fakeLeaseSource{leases: []store.LiveLease{
		{ID: "lease-secret-3", WorkspaceID: "ws_3", OwnerRef: "https://idp|a", CreatedAt: now, ExpiresAt: now.Add(time.Minute)},
		{ID: "lease-secret-2", WorkspaceID: "ws_2", OwnerRef: "https://idp|a", CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute)},
		{ID: "lease-secret-1", WorkspaceID: "ws_1", OwnerRef: "https://idp|a", CreatedAt: now.Add(-2 * time.Minute), ExpiresAt: now.Add(time.Minute)},
	}}
	env := newAdminSessionsEnv(t, src, &fakeAdminRevoker{})
	sess, csrf := loginAdmin(t, env, "admin-a")
	page := decodeBody[Page[adminLiveSession]](t, doReq(t, env, sess, csrf, http.MethodGet, adminSessionsPath+"?limit=2", "", nil))
	if len(page.Items) != 2 || page.NextCursor == "" || src.got.Limit != 2 {
		t.Fatalf("page = %+v filter %+v, want 2 items and a cursor", page, src.got)
	}
	raw, err := base64.RawURLEncoding.DecodeString(page.NextCursor)
	if err != nil || strings.Contains(string(raw), "lease-secret") || !strings.Contains(string(raw), "ws_2") {
		t.Fatalf("cursor %q decodes to %q, err %v: want the workspace id, never the lease id", page.NextCursor, raw, err)
	}
	decodeBody[Page[adminLiveSession]](t, doReq(t, env, sess, csrf, http.MethodGet, adminSessionsPath+"?limit=2&cursor="+page.NextCursor, "", nil))
	if src.got.After == nil || src.got.After.ID != "ws_2" {
		t.Fatalf("second read got After %+v, want workspace ws_2", src.got.After)
	}
}

func TestAdminSessions_ListRefusals(t *testing.T) {
	src := &fakeLeaseSource{}
	env := newAdminSessionsEnv(t, src, &fakeAdminRevoker{})
	sess, csrf := loginAdmin(t, env, "admin-a")
	cases := []struct {
		name, path string
		want       int
	}{
		{"other tenant", "/v1/admin/tenants/tenant-b/sessions", http.StatusForbidden},
		{"ascending sort", adminSessionsPath + "?sort=createdAt", http.StatusBadRequest},
		{"bad ownerRef", adminSessionsPath + "?ownerRef=bad", http.StatusBadRequest},
		{"bad cursor", adminSessionsPath + "?cursor=%21", http.StatusBadRequest},
	}
	for _, c := range cases {
		res := doReq(t, env, sess, csrf, http.MethodGet, c.path, "", nil)
		res.Body.Close()
		if res.StatusCode != c.want {
			t.Fatalf("%s: status %d, want %d", c.name, res.StatusCode, c.want)
		}
	}
	usess, ucsrf := loginGroups(t, env, "user-a") // no groups claim: not an administrator
	res := doReq(t, env, usess, ucsrf, http.MethodGet, adminSessionsPath, "", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin: %d", res.StatusCode)
	}
	if src.calls != 0 {
		t.Fatalf("the lease source was read %d times by refused requests", src.calls)
	}
}

func TestAdminSessions_RevokeAllTargetsTheNamedUser(t *testing.T) {
	rev := &fakeAdminRevoker{}
	env := newAdminSessionsEnv(t, &fakeLeaseSource{}, rev)
	sess, csrf := loginAdmin(t, env, "admin-a")
	res := doReq(t, env, sess, csrf, http.MethodPost, adminRevokePath("https://idp|alice"), "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	if c := res.Header.Values("Set-Cookie"); len(c) != 0 {
		t.Fatalf("the admin's own cookie must not be cleared: %v", c)
	}
	got := decodeBody[adminRevokeResult](t, res)
	if rev.tenant != "tenant-a" || rev.issuer != "https://idp" || rev.subject != "alice" {
		t.Fatalf("revoked %s/%s/%s", rev.tenant, rev.issuer, rev.subject)
	}
	if got.Sessions != 2 || got.Tickets != 1 || got.Leases != 1 {
		t.Fatalf("result %+v", got)
	}
}

func TestAdminSessions_RevokeAllRefusals(t *testing.T) {
	rev := &fakeAdminRevoker{}
	env := newAdminSessionsEnv(t, &fakeLeaseSource{}, rev)
	sess, csrf := loginAdmin(t, env, "admin-a")
	path := adminRevokePath("https://idp|alice")
	wrong := &http.Cookie{Name: env.auth.CSRFHeader(), Value: "wrong"}
	if res := doReq(t, env, sess, wrong, http.MethodPost, path, "", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("bad CSRF: %d", res.StatusCode)
	}
	other := "/v1/admin/tenants/tenant-b/users/" + url.PathEscape("https://idp|alice") + "/sessions:revoke-all"
	if res := doReq(t, env, sess, csrf, http.MethodPost, other, "", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("other tenant: %d", res.StatusCode)
	}
	if res := doReq(t, env, sess, csrf, http.MethodPost, adminRevokePath("bad"), "", nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad ownerRef: %d", res.StatusCode)
	}
	usess, ucsrf := loginGroups(t, env, "user-a")
	if res := doReq(t, env, usess, ucsrf, http.MethodPost, path, "", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin: %d", res.StatusCode)
	}
	if rev.calls != 0 {
		t.Fatalf("revoker called %d times by refused requests", rev.calls)
	}
}

func TestAdminSessions_RevokeFailureIs500(t *testing.T) {
	rev := &fakeAdminRevoker{err: context.DeadlineExceeded}
	env := newAdminSessionsEnv(t, &fakeLeaseSource{}, rev)
	sess, csrf := loginAdmin(t, env, "admin-a")
	res := doReq(t, env, sess, csrf, http.MethodPost, adminRevokePath("https://idp|alice"), "", nil)
	if e := decodeErr(t, res); res.StatusCode != http.StatusInternalServerError || e.Code != CodeInternal {
		t.Fatalf("status %d code %q, want 500 INTERNAL", res.StatusCode, e.Code)
	}
}

// The audit record for the revoke names the admin as actor, the tenant as
// target and the revoked principal only as a pseudonymous ref.
func TestAudit_AdminSessionRevoke(t *testing.T) {
	sink := &captureSink{}
	env := newAdminSessionsEnv(t, &fakeLeaseSource{}, &fakeAdminRevoker{},
		func(h *AdminSessionsHandler) { h.WithAuditSink(sink) })
	sess, csrf := loginAdmin(t, env, "admin-a")
	doReq(t, env, sess, csrf, http.MethodPost, adminRevokePath("https://idp|alice"), "", nil).Body.Close()
	e := requireEvent(t, sink, "admin.session.revoke")
	if e.Outcome != observability.OutcomeSuccess || e.Tenant != "tenant-a" || e.TargetUID != "tenant-a" {
		t.Fatalf("event %+v", e)
	}
	if e.Details["target"] != targetActorRef("https://idp|alice") || e.Details["counts"] != "sessions=2,tickets=1,leases=1" {
		t.Fatalf("details %+v", e.Details)
	}
	if strings.Contains(sink.raw(), "alice") {
		t.Fatalf("the revoked subject leaked into the audit record: %s", sink.raw())
	}
	usess, ucsrf := loginGroups(t, env, "user-a")
	doReq(t, env, usess, ucsrf, http.MethodPost, adminRevokePath("https://idp|alice"), "", nil).Body.Close()
	var denied int
	for _, ev := range sink.get() {
		if ev.Action == "admin.session.revoke" && ev.Outcome == observability.OutcomeDenied {
			denied++
		}
	}
	if denied != 1 {
		t.Fatalf("denied revoke events = %d, want 1: attempted privilege use must be auditable", denied)
	}
}
```

The checklist's shared-test items:

- `auditroutes_test.go`, `newCoverageEnv`, after `MountAdminSessionPolicyRoutes(...)`:

  ```go
  	MountAdminSessionRoutes(mux, a, NewAdminSessionsHandler(
  		&fakeLeaseSource{leases: []store.LiveLease{{ID: "lease-cov-1", WorkspaceID: "ws_0000000000000000000000000c", WorkspaceName: "cov", OwnerRef: "iss|sub", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute)}}},
  		newFakeDirectory(), &fakePrincipalRevoker{res: RevokeAllResult{Leases: 1}}, tenants).WithAuditSink(sink))
  ```

  and two drivers in the `routes` table of `TestAuditCoverage_MountedMuxEmitsEventPerRoute`, before `POST /v1/logout`:

  ```go
  		{"GET /v1/admin/tenants/{}/sessions", auditActionAdminSessionList, func() {
  			doReq(t, env, sessA, csrfA, http.MethodGet, adminSessionsPath, "", nil).Body.Close()
  		}},
  		{"POST /v1/admin/tenants/{}/users/{}/sessions:revoke-all", auditActionAdminSessionRevoke, func() {
  			doReq(t, env, sessA, csrfA, http.MethodPost, adminRevokePath("iss|sub"), "", nil).Body.Close()
  		}},
  ```

- `openapi_contract_test.go`, in the `calls` slice of `TestOpenAPIContract_ResponsesValidate` (the 200 body is validated against `AdminLiveSessionPage`, the refusals against `Error`):

  ```go
  		{"GET", "/v1/admin/tenants/{tenant}/sessions", func() *http.Response {
  			return doReq(t, env, sessA, csrfA, "GET", adminSessionsPath, "", nil)
  		}},
  		{"GET", "/v1/admin/tenants/{tenant}/sessions", func() *http.Response {
  			return doReq(t, env, sessA, csrfA, "GET", adminSessionsPath+"?sort=createdAt", "", nil)
  		}},
  		{"POST", "/v1/admin/tenants/{tenant}/users/{ownerRef}/sessions:revoke-all", func() *http.Response {
  			return doReq(t, env, sessA, csrfA, "POST", adminRevokePath("iss|contract"), "", nil)
  		}},
  		{"POST", "/v1/admin/tenants/{tenant}/users/{ownerRef}/sessions:revoke-all", func() *http.Response {
  			return doReq(t, env, sessA, csrfA, "POST", adminRevokePath("bad"), "", nil)
  		}},
  ```

- `tenantadmin_test.go`, in `newAdminGroupEnv` after `MountAdminSessionPolicyRoutes(...)`: `MountAdminSessionRoutes(mux, a, NewAdminSessionsHandler(&fakeLeaseSource{}, nil, &fakePrincipalRevoker{}, defaultTenants()))`, and next to `TestAdminSessionPolicy_CustomAdminGroups`:

  ```go
  // TestAdminSessions_CustomAdminGroups: both F5 routes honour the configured admin group.
  func TestAdminSessions_CustomAdminGroups(t *testing.T) {
  	env, _ := newAdminGroupEnv(t, []string{testCustomAdminGroup})
  	calls := []struct{ method, path string }{
  		{http.MethodGet, adminSessionsPath},
  		{http.MethodPost, adminRevokePath("https://idp|alice")},
  	}
  	sess, csrf := loginGroups(t, env, "admin-a", testCustomAdminGroup)
  	for _, c := range calls {
  		r := doReq(t, env, sess, csrf, c.method, c.path, "", nil)
  		r.Body.Close()
  		if r.StatusCode != http.StatusOK {
  			t.Fatalf("member %s: status=%d, want 200", c.method, r.StatusCode)
  		}
  	}
  	sess, csrf = loginGroups(t, env, "admin-b", TenantAdminGroup)
  	for _, c := range calls {
  		r := doReq(t, env, sess, csrf, c.method, c.path, "", nil)
  		r.Body.Close()
  		if r.StatusCode != http.StatusForbidden {
  			t.Fatalf("non-member %s: status=%d, want 403", c.method, r.StatusCode)
  		}
  	}
  }
  ```

Run `go test ./internal/api/ -run 'AdminSessions|AdminSession_|AuditCoverage|OpenAPI|TestAudit_AdminSessionRevoke' -count=1`. Expected: FAIL to compile (`undefined: NewAdminSessionsHandler`).

- [ ] **Step 4: Implement `internal/api/adminsessions.go`**

```go
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// LiveLeaseSource lists live connection leases from Postgres (store.DB) —
// every replica answers the same, unlike gateway-local state.
type LiveLeaseSource interface {
	ListLiveLeases(ctx context.Context, tenantID string, now time.Time, f store.LeaseFilter) ([]store.LiveLease, error)
}

type adminLiveSession struct {
	LeaseRef      string     `json:"leaseRef"`
	WorkspaceID   string     `json:"workspaceId"`
	WorkspaceName string     `json:"workspaceName"`
	OwnerRef      string     `json:"ownerRef"`
	Owner         Owner      `json:"owner"`
	CreatedAt     time.Time  `json:"createdAt"`
	LastRenewedAt *time.Time `json:"lastRenewedAt,omitempty"`
	ExpiresAt     time.Time  `json:"expiresAt"`
}

// adminRevokeResult is the POST ...:revoke-all body (openapi AdminRevokeResult).
type adminRevokeResult struct {
	Sessions int `json:"sessions"`
	Tickets  int `json:"tickets"`
	Leases   int `json:"leases"`
}

// AdminSessionsHandler lists live sessions and signs a user out everywhere (F5).
type AdminSessionsHandler struct {
	leases  LiveLeaseSource
	dir     Directory
	revoker PrincipalRevoker
	tenants TenantResolver
	audit   observability.AuditSink
	now     func() time.Time
}

func NewAdminSessionsHandler(src LiveLeaseSource, dir Directory, revoker PrincipalRevoker, t TenantResolver) *AdminSessionsHandler {
	return &AdminSessionsHandler{leases: src, dir: dir, revoker: revoker, tenants: t, now: time.Now}
}

func (h *AdminSessionsHandler) WithAuditSink(s observability.AuditSink) *AdminSessionsHandler {
	h.audit = s
	return h
}

func MountAdminSessionRoutes(mux RouteMux, authn *Authenticator, h *AdminSessionsHandler) {
	mux.Handle(routeAdminSessionsList, authn.RequireAuthPassive(
		audited(h.audit, routeAdminSessionsList, http.HandlerFunc(h.List))))
	mux.Handle(routeAdminUserRevokeAll, authn.RequireAuth(
		audited(h.audit, routeAdminUserRevokeAll, authn.RequireCSRF(http.HandlerFunc(h.RevokeAll)))))
}

// leaseRef is the stable public reference of a lease; the id itself is a
// fencing credential the gateway presents and never leaves the backend.
func leaseRef(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:8])
}

func (h *AdminSessionsHandler) List(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantAdminPrincipal(h.tenants, w, r); !ok {
		return
	}
	page, perr := ParsePage(r, []string{"createdAt"}, "-createdAt")
	if perr != nil {
		WriteError(w, r, perr)
		return
	}
	if !page.Sort.Desc {
		writeError(w, r, CodeInvalidRequest, "sessions list newest first only (sort=-createdAt)")
		return
	}
	f := store.LeaseFilter{Limit: page.Limit}
	if ref := r.URL.Query().Get("ownerRef"); ref != "" {
		if msg := validOwnerRef(ref); msg != "" {
			writeError(w, r, CodeInvalidRequest, msg)
			return
		}
		f.OwnerRef = ref
	}
	if page.After != nil {
		at, err := time.Parse(time.RFC3339Nano, page.After.Key)
		if err != nil {
			writeError(w, r, CodeInvalidRequest, "bad cursor")
			return
		}
		f.After = &store.LeaseCursor{CreatedAt: at, ID: page.After.ID}
	}
	tenant := r.PathValue("tenant")
	rows, err := h.leases.ListLiveLeases(r.Context(), tenant, h.now(), f)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	out := Page[adminLiveSession]{Items: make([]adminLiveSession, 0, len(rows))}
	if len(rows) > page.Limit {
		last := rows[page.Limit-1]
		// The keyset tiebreak is the WORKSPACE id (one live lease per workspace),
		// so neither items nor the opaque cursor ever carry a lease id.
		out.NextCursor = NextCursor(page.Sort, last.CreatedAt.UTC().Format(time.RFC3339Nano), last.WorkspaceID)
		rows = rows[:page.Limit]
	}
	refs := make([]string, len(rows))
	for i, l := range rows {
		refs[i] = l.OwnerRef
	}
	owners := resolveOwners(r.Context(), h.dir, tenant, refs)
	for _, l := range rows {
		out.Items = append(out.Items, adminLiveSession{
			LeaseRef: leaseRef(l.ID), WorkspaceID: l.WorkspaceID, WorkspaceName: l.WorkspaceName,
			OwnerRef: l.OwnerRef, Owner: owners[l.OwnerRef], CreatedAt: l.CreatedAt,
			LastRenewedAt: l.LastRenewedAt, ExpiresAt: l.ExpiresAt,
		})
	}
	respondJSON(w, out)
}

func (h *AdminSessionsHandler) RevokeAll(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantAdminPrincipal(h.tenants, w, r); !ok {
		return
	}
	ref := r.PathValue("ownerRef")
	if msg := validOwnerRef(ref); msg != "" {
		writeError(w, r, CodeInvalidRequest, msg)
		return
	}
	ctx := r.Context()
	auditSetDetail(ctx, "target", targetActorRef(ref))
	if h.revoker == nil {
		writeError(w, r, CodeUnavailable, "sign-out unavailable")
		return
	}
	iss, sub, _ := strings.Cut(ref, "|")
	revCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionRevokeTimeout)
	res, err := h.revoker.RevokePrincipalSessions(revCtx, r.PathValue("tenant"), iss, sub)
	cancel()
	if err != nil {
		writeError(w, r, CodeInternal, "could not sign the user out")
		return
	}
	auditSetDetail(ctx, "counts", "sessions="+strconv.Itoa(res.Sessions)+
		",tickets="+strconv.Itoa(res.Tickets)+",leases="+strconv.Itoa(res.Leases))
	respondJSON(w, adminRevokeResult{Sessions: res.Sessions, Tickets: res.Tickets, Leases: res.Leases})
}
```

Tenant isolation, in three layers: `tenantAdminPrincipal` refuses any `{tenant}` other than the caller's own (403, before any read or revoke); `ListLiveLeases` filters `l.tenant_id = $1` in SQL; and `Broker.RevokePrincipalSessions` deletes and revokes only rows with `tenant_id = $1`. The `ownerRef` is split at the first `|` exactly as `validOwnerRef` checks it, so an administrator of tenant A naming a tenant-B principal under their own tenant path touches nothing. The revoke runs on a context detached from the request's cancellation but not from its values, so Task 1's `store.TxHook` still inserts the `admin.session.revoke` audit row inside the broker's transaction (fail closed).

Wire in `wire.go`, `newAppHandler` (it already has `db`, `directory` and `tenants` in scope): add the field `adminSessions *api.AdminSessionsHandler` to `appRoutes` and set `adminSessions: api.NewAdminSessionsHandler(db, directory, broker.PublicRevoker{B: brk}, tenants).WithAuditSink(appAudit)` where the other handlers are put into `appRoutes`. `*store.DB` satisfies `LiveLeaseSource`, and `broker.PublicRevoker{B: brk}` is the same value `wire.go` passes to `authn.WithPrincipalRevoker`. In `appMux` add `if h.adminSessions != nil { api.MountAdminSessionRoutes(mux, authn, h.adminSessions) }`. Run the Step 3 command, then `go test ./internal/backend/ -count=1`. Expected: PASS.

- [ ] **Step 5: Web section on the user detail page**

`web/src/admin/api.ts` (the file already imports `unwrap`, `ApiClient`, `components` and `ServerPage`):

```ts
export type AdminLiveSession = components["schemas"]["AdminLiveSession"];
export type AdminRevokeResult = components["schemas"]["AdminRevokeResult"];

// GET /v1/admin/tenants/{tenant}/sessions — live leases from Postgres, newest first.
export async function listAdminSessionPage(
  api: ApiClient,
  tenant: string,
  ownerRef?: string,
  cursor?: string,
): Promise<ServerPage<AdminLiveSession>> {
  return unwrap(
    await api.GET("/v1/admin/tenants/{tenant}/sessions", {
      params: {
        path: { tenant },
        query: { sort: "-createdAt", ...(ownerRef ? { ownerRef } : {}), ...(cursor ? { cursor } : {}) },
      },
    }),
  );
}

// POST .../users/{ownerRef}/sessions:revoke-all — "sign out everywhere" for another user.
export async function revokeAdminUserSessions(api: ApiClient, tenant: string, ownerRef: string): Promise<AdminRevokeResult> {
  return unwrap(
    await api.POST("/v1/admin/tenants/{tenant}/users/{ownerRef}/sessions:revoke-all", {
      params: { path: { tenant, ownerRef } },
    }),
  );
}
```

`web/src/admin/UserSessionsSection.tsx`. The design imports (`ConfirmDialog`, `Section`, `Table`, `Pagination`, `useServerPage`, `Column`) all come from `../design`; `useServerPage` lives in `web/src/design/useServerPage.ts`, there is no `hooks/` directory, and `formatDateTime` is `./format`:

```tsx
import { useCallback, useState } from "react";
import { Button, ConfirmDialog, Pagination, Section, Table, useServerPage } from "../design";
import type { Column } from "../design/Table";
import { useApi } from "../api/context";
import { t } from "../i18n";
import { ApiErrorAlert } from "./ApiErrorAlert";
import { listAdminSessionPage, revokeAdminUserSessions, type AdminLiveSession } from "./api";
import { formatDateTime } from "./format";

// The "Live sessions" section of the user detail page (F5): the user's live
// desktop leases, read from Postgres, and the confirmed "Sign out everywhere".
export function UserSessionsSection({
  tenant,
  ownerRef,
  displayName,
}: {
  tenant: string;
  ownerRef: string;
  displayName: string;
}) {
  const api = useApi();
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<unknown>(null);
  const [result, setResult] = useState<string | null>(null);
  const load = useCallback(
    (cursor?: string) => listAdminSessionPage(api, tenant, ownerRef, cursor),
    [api, tenant, ownerRef],
  );
  const list = useServerPage(load, { sort: "-createdAt", key: `${tenant}|${ownerRef}` });

  const columns: Column<AdminLiveSession>[] = [
    { key: "workspace", header: t("admin.sessions.column.workspace"), rowHeader: true, render: (s) => s.workspaceName || s.workspaceId },
    { key: "started", header: t("admin.sessions.column.started"), render: (s) => formatDateTime(s.createdAt) },
    {
      key: "renewed",
      header: t("admin.sessions.column.lastActive"),
      hideOnMobile: true,
      render: (s) => (s.lastRenewedAt ? formatDateTime(s.lastRenewedAt) : "—"),
    },
    { key: "ref", header: t("admin.sessions.column.ref"), hideOnMobile: true, render: (s) => <code>{s.leaseRef}</code> },
  ];

  async function signOut() {
    setBusy(true);
    setActionError(null);
    try {
      const r = await revokeAdminUserSessions(api, tenant, ownerRef);
      setConfirming(false);
      setResult(t("admin.sessions.done", { sessions: r.sessions, leases: r.leases }));
      list.reset();
    } catch (e) {
      setActionError(e);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Section
      title={t("admin.sessions.title")}
      actions={
        <Button size="sm" variant="danger" onClick={() => { setActionError(null); setConfirming(true); }}>
          {t("admin.sessions.signOutAll")}
        </Button>
      }
    >
      <ApiErrorAlert error={list.error} onRetry={list.refresh} />
      <Table
        caption={t("admin.sessions.caption")}
        columns={columns}
        rows={list.items}
        rowKey={(s) => s.leaseRef}
        density="compact"
        loading={!list.loaded && !list.error}
        empty={t("admin.sessions.empty")}
      />
      <Pagination
        loaded={list.items.length}
        hasNext={list.hasNext}
        loading={list.loading}
        onLoadMore={list.next}
        label={t("common.pagination.label", { name: t("admin.sessions.caption") })}
      />
      {result ? <p role="status">{result}</p> : null}
      <ConfirmDialog
        open={confirming}
        title={t("admin.sessions.confirm.title", { name: displayName })}
        confirmLabel={t("admin.sessions.signOutAll")}
        cancelLabel={t("common.cancel")}
        busy={busy}
        onConfirm={() => void signOut()}
        onCancel={() => setConfirming(false)}
      >
        <p>{t("admin.sessions.confirm.body")}</p>
        <ApiErrorAlert error={actionError} />
      </ConfirmDialog>
    </Section>
  );
}
```

`ConfirmDialog` takes its body as children and uses `destructive` (default true, role `alertdialog`), `busy`, `confirmLabel`, `cancelLabel`; `Button` uses `variant`. Do not use `tone` or `body` props: they do not exist.

In `UsersAdminPage.tsx`, import `UserSessionsSection` and render it in `UserDetailPage` inside the loaded-user branch, directly after the workspaces `Section` and before the disks `Section`. Use Task 3's loaded data (`u.data.user` is defined in this branch):

```tsx
<UserSessionsSection tenant={u.data.tenant} ownerRef={u.data.user.ownerRef} displayName={row.displayName} />
```

Strings, added to the default-export object of `web/src/i18n/en/admin.ts` and `web/src/i18n/vi/admin.ts` (the `vi` object is checked against the `en` keys, so a missing key fails `typecheck`):

| key | en | vi |
|---|---|---|
| `admin.sessions.title` | Live sessions | Phiên đang hoạt động |
| `admin.sessions.caption` | Live desktop sessions of this user | Các phiên desktop đang hoạt động của người dùng này |
| `admin.sessions.column.workspace` | Workspace | Workspace |
| `admin.sessions.column.started` | Started | Bắt đầu |
| `admin.sessions.column.lastActive` | Last renewed | Gia hạn gần nhất |
| `admin.sessions.column.ref` | Reference | Mã tham chiếu |
| `admin.sessions.empty` | No live sessions. | Không có phiên nào đang hoạt động. |
| `admin.sessions.signOutAll` | Sign out everywhere | Đăng xuất khỏi mọi nơi |
| `admin.sessions.confirm.title` | Sign {name} out everywhere? | Đăng xuất {name} khỏi mọi nơi? |
| `admin.sessions.confirm.body` | Every portal session of this user ends and their open desktops disconnect. Their workspaces keep running. | Mọi phiên portal của người dùng này sẽ kết thúc và các desktop đang mở sẽ ngắt kết nối. Workspace vẫn tiếp tục chạy. |
| `admin.sessions.done` | Signed out: {sessions} portal sessions, {leases} desktop sessions. | Đã đăng xuất: {sessions} phiên portal, {leases} phiên desktop. |

`web/tests/mock-api/admin.ts` (inside `adminArea`; the composer already enforces the session cookie and the CSRF header on mutating routes, so the mock needs no CSRF code of its own). Add the seed next to `seedWorkspaces()`:

```ts
interface MockLiveSession {
  leaseRef: string;
  workspaceId: string;
  workspaceName: string;
  ownerRef: string;
  owner: Owner;
  createdAt: string;
  lastRenewedAt?: string;
  expiresAt: string;
}

// Two live sessions for Grace, one for Linus (newest first by createdAt).
function seedLiveSessions(): MockLiveSession[] {
  const [grace, linus] = OTHER_USERS;
  const row = (leaseRef: string, workspaceId: string, workspaceName: string, o: Owner, createdAt: string): MockLiveSession => ({
    leaseRef, workspaceId, workspaceName, ownerRef: ownerRefOf(o.subject), owner: o,
    createdAt, lastRenewedAt: createdAt, expiresAt: "2099-01-01T00:00:00Z",
  });
  return [
    row("0123456789abcdef", SEED_WORKSPACE_IDS.graceReady, "grace-analysis", grace, "2026-10-13T08:00:00Z"),
    row("fedcba9876543210", "ws_01J4ZGRACEREADY02", "grace-notes", grace, "2026-10-13T08:05:00Z"),
    row("00112233445566aa", SEED_WORKSPACE_IDS.linusStopped, "linus-desktop", linus, "2026-10-13T08:10:00Z"),
  ];
}
```

Inside `adminArea(ctx)` add `let liveSessions = seedLiveSessions();`, reset it in `reset()` (`liveSessions = seedLiveSessions();`), and add the handlers and the two route matches at the top of `api(req)` next to the `mUL`/`mSP` matches:

```ts
  function adminSessions(req: MockRequest, tenant: string): MockResponse {
    if (!t.isAdmin() || tenant !== t.me.tenant) {
      return err(403, "FORBIDDEN", "tenant administration requires the tenant-admin role", false);
    }
    if (req.method !== "GET") return err(405, "INVALID_REQUEST", "method not allowed", false);
    if ((req.query.get("sort") || "-createdAt") !== "-createdAt") {
      return err(400, "INVALID_REQUEST", "sessions list newest first only (sort=-createdAt)", false);
    }
    const failed = t.takeFailure(req);
    if (failed) return failed;
    const owner = req.query.get("ownerRef");
    const rows = liveSessions.filter((s) => !owner || s.ownerRef === owner);
    return paginate(req, rows, { createdAt: byString((s) => s.createdAt) }, "-createdAt", (s) => s.workspaceId);
  }

  function adminRevokeSessions(req: MockRequest, tenant: string, rawRef: string): MockResponse {
    if (!t.isAdmin() || tenant !== t.me.tenant) {
      return err(403, "FORBIDDEN", "tenant administration requires the tenant-admin role", false);
    }
    if (req.method !== "POST") return err(405, "INVALID_REQUEST", "method not allowed", false);
    let ref: string;
    try {
      ref = decodeURIComponent(rawRef);
    } catch {
      return err(400, "INVALID_REQUEST", "ownerRef must be an issuer|sub reference", false);
    }
    if (!ref.includes("|") || ref.endsWith("|") || ref.startsWith("|")) {
      return err(400, "INVALID_REQUEST", "ownerRef must be an issuer|sub reference", false);
    }
    const gone = liveSessions.filter((s) => s.ownerRef === ref);
    liveSessions = liveSessions.filter((s) => s.ownerRef !== ref);
    return ok(200, { sessions: gone.length > 0 ? 1 : 0, tickets: 0, leases: gone.length });
  }
```

```ts
    const mSess = path.match(/^\/v1\/admin\/tenants\/([^/]+)\/sessions$/);
    if (mSess) return adminSessions(req, mSess[1]);
    const mRevoke = path.match(/^\/v1\/admin\/tenants\/([^/]+)\/users\/([^/]+)\/sessions:revoke-all$/);
    if (mRevoke) return adminRevokeSessions(req, mRevoke[1], mRevoke[2]);
```

Playwright test in `web/tests/admin.spec.ts` (reuses `makeAdmin`, `login`, `MOCK`; the user detail URL is Task 3's `/admin/users/<encoded ownerRef>`):

```ts
test("admin signs a user out everywhere", async ({ page, request }) => {
  await makeAdmin(request);
  await login(page, "/admin/users");
  await page.getByRole("table", { name: "Tenant users" }).getByRole("link", { name: "Grace Hopper" }).click();
  await page.waitForURL(/\/admin\/users\/.+/);

  const section = page.getByRole("region", { name: "Live sessions" });
  const table = section.getByRole("table", { name: "Live desktop sessions of this user" });
  await expect(table.locator("tbody tr")).toHaveCount(2);
  await expect(table.getByText("grace-analysis")).toBeVisible();
  await expect(table.getByText("linus-desktop")).toHaveCount(0);

  await section.getByRole("button", { name: "Sign out everywhere" }).click();
  const dialog = page.getByRole("alertdialog", { name: "Sign Grace Hopper out everywhere?" });
  await dialog.getByRole("button", { name: "Sign out everywhere" }).click();

  await expect(section.getByRole("status")).toContainText("Signed out: 1 portal sessions, 2 desktop sessions.");
  await expect(table.getByText("No live sessions.")).toBeVisible();

  const reqs = await (await request.get(`${MOCK}/_control/requests`)).json();
  expect(
    reqs.requests.some(
      (r: { method: string; path: string; headers: Record<string, string> }) =>
        r.method === "POST" &&
        decodeURIComponent(r.path) === "/v1/admin/tenants/acme/users/https://idp.invalid|user-01J4ZDGRC/sessions:revoke-all" &&
        Boolean(r.headers["x-csrf-token"]),
    ),
  ).toBeTruthy();
});
```

Run:

```bash
cd web && npm run generate && npm run typecheck && npm run lint:strings && npx playwright test tests/admin.spec.ts -g "signs a user out everywhere"
```

Expected: PASS.

- [ ] **Step 6: Kind e2e: an admin login in the dev realm, and the F5 round trip**

The quickstart realm (`hack/quickstart/realm-export.json`) has the `demo` and `demo-noquota` users, no group and no `groups` claim, so no kind test can log in as a tenant administrator today. Seed one. The backend default `oidc.tenantAdminGroups` is `["tenant-admin"]` and the claim name defaults to `groups`; `hack/quickstart/values.yaml` overrides neither.

`hack/quickstart/realm-export.json`: add the realm group after `"ssoSessionMaxLifespan": 36000,`

```json
  "groups": [
    { "name": "tenant-admin", "path": "/tenant-admin" }
  ],
```

append this mapper to the client's `protocolMappers` array (after the `tenant_id` mapper, with a comma):

```json
        {
          "name": "groups",
          "protocol": "openid-connect",
          "protocolMapper": "oidc-group-membership-mapper",
          "consentRequired": false,
          "config": {
            "claim.name": "groups",
            "full.path": "false",
            "id.token.claim": "true",
            "access.token.claim": "true",
            "userinfo.token.claim": "true"
          }
        }
```

and append this user to `users` (same tenant as `demo`, same password):

```json
    {
      "username": "@ADMIN_USER@",
      "email": "demo-admin@tcdi.localtest.me",
      "emailVerified": true,
      "firstName": "Demo",
      "lastName": "Admin",
      "enabled": true,
      "groups": ["/tenant-admin"],
      "attributes": {
        "tenant_id": [
          "tenant-a"
        ]
      },
      "credentials": [
        {
          "type": "password",
          "value": "@DEMO_PASSWORD@",
          "temporary": false
        }
      ]
    }
```

`hack/quickstart/common.sh`, after the `NOQUOTA_USER` line:

```bash
# A tenant administrator of tenant-a (member of the realm group tenant-admin) for the admin e2e.
ADMIN_USER="${ADMIN_USER:-demo-admin}"
```

`hack/quickstart/up.sh`: add `-e "s|@ADMIN_USER@|${ADMIN_USER}|g" \` after the `@NOQUOTA_USER@` substitution (line 197), add `TCDI_QS_ADMIN_USER=${ADMIN_USER}` after `TCDI_QS_NOQUOTA_USER=${NOQUOTA_USER}` in the env heredoc (line 332), and a line `  Admin    ${ADMIN_USER} / ${DEMO_PASSWORD}   (tenant administrator)` under the `Login` line of the closing message. `docs/quickstart.md` line 87: "...imported from `realm-export.json`: one confidential client, the `demo` user and the `demo-admin` tenant administrator".

`hack/quickstart/smoke/smoke.spec.ts`: next to `NOQUOTA_USER` add

```ts
const ADMIN_USER = process.env.TCDI_QS_ADMIN_USER ?? "demo-admin";
// The tenant id the dev realm maps demo and demo-admin to (hack/quickstart/values.yaml).
const TENANT = process.env.TCDI_QS_TENANT ?? "tenant-a";
```

and append one test at the end of the file:

```ts
test("admin: the live session is listed from Postgres, Sign out everywhere ends it, the audit row exists", async ({ browser }) => {
  // The user opens a desktop, which mints a live connection lease.
  const userCtx = await browser.newContext({ ignoreHTTPSErrors: true });
  const userPage = await userCtx.newPage();
  await login(userPage);
  const name = `adm-${Date.now().toString(36)}`;
  const id = await createBrowserWorkspace(userPage, name);
  await connectSessionFrame(userPage, id);

  const adminCtx = await browser.newContext({ ignoreHTTPSErrors: true });
  const page = await adminCtx.newPage();
  await login(page, ADMIN_USER);

  type Live = { ownerRef: string; workspaceId: string; leaseRef: string };
  const listed = async (): Promise<Live[]> => {
    const r = await page.request.get(`/v1/admin/tenants/${TENANT}/sessions`);
    expect(r.status()).toBe(200);
    return ((await r.json()) as { items: Live[] }).items;
  };
  await expect
    .poll(async () => (await listed()).some((s) => s.workspaceId === id), { timeout: 60_000 })
    .toBe(true);
  const mine = (await listed()).find((s) => s.workspaceId === id)!;
  expect(mine.leaseRef).toMatch(/^[0-9a-f]{16}$/);
  const ownerRef = mine.ownerRef;

  // F8 (Task 3): the tenant directory lists the signed-in user with a workspace count.
  const dir = await page.request.get(`/v1/admin/tenants/${TENANT}/users?sort=-lastSeen&limit=500`);
  expect(dir.status()).toBe(200);
  const users = ((await dir.json()) as { items: { ownerRef: string; workspaces: number; lastSeenAt: string }[] }).items;
  const me = users.find((u) => u.ownerRef === ownerRef);
  expect(me?.workspaces).toBeGreaterThanOrEqual(1);
  expect(me?.lastSeenAt).toBeTruthy();
  await page.goto("/admin/users");
  await expect(page.locator(`a[href="/admin/users/${Buffer.from(ownerRef).toString("base64url")}"]`)).toBeVisible();

  // The user detail page shows the session and signs the user out after a confirmation.
  // Task 3's detail route carries the owner reference as base64url (a dotted issuer would 404 percent-encoded).
  await page.goto(`/admin/users/${Buffer.from(ownerRef).toString("base64url")}`);
  const section = page.getByRole("region", { name: "Live sessions" });
  await expect(section.getByText(name)).toBeVisible();
  await section.getByRole("button", { name: "Sign out everywhere" }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Sign out everywhere" }).click();
  await expect(section.getByRole("status")).toContainText("Signed out");

  // Effects: the principal has no live lease, their portal session is gone, the audit row is there.
  await expect.poll(async () => (await listed()).some((s) => s.ownerRef === ownerRef)).toBe(false);
  expect((await userCtx.request.get("/v1/me")).status()).toBe(401);
  const audit = await page.request.get(`/v1/admin/tenants/${TENANT}/audit?action=admin.session.revoke`);
  expect(audit.status()).toBe(200);
  const rows = ((await audit.json()) as { items: { action: string; outcome: string; details?: Record<string, string> }[] }).items;
  expect(rows.some((e) => e.outcome === "success" && Number(e.details?.counts?.match(/leases=(\d+)/)?.[1] ?? 0) >= 1)).toBe(true);

  await userCtx.close();
  await adminCtx.close();
});
```

Checks that need no cluster, then the smoke compiles:

```bash
python3 -c 'import json; json.load(open("hack/quickstart/realm-export.json"))' && echo realm-json-ok
sed -e 's|@CLIENT_SECRET@|x|g' -e 's|@PORTAL_HOST@|p|g' -e 's|@DEMO_USER@|demo|g' -e 's|@DEMO_PASSWORD@|pw|g' -e 's|@NOQUOTA_USER@|demo-noquota|g' -e 's|@ADMIN_USER@|demo-admin|g' hack/quickstart/realm-export.json | python3 -c 'import json,sys; r=json.load(sys.stdin); print([u["username"] for u in r["users"]])'
bash -n hack/quickstart/common.sh && bash -n hack/quickstart/up.sh && echo shell-ok
(cd hack/quickstart/smoke && npm ci --silent && npx playwright test --list > /tmp/f5-smoke-list.log 2>&1; echo "rc=$?"; grep -c 'admin: the live session' /tmp/f5-smoke-list.log)
```

Expected: `realm-json-ok`, the user list shows `demo`, `demo-noquota` and `demo-admin`, `shell-ok`, `rc=0` and `1`. The test runs for real in CI's `quickstart` job; the plan is done only when that job is green.

- [ ] **Step 7: Threat model and commit**

Row S44: `| S44 | Admin live sessions and sign-out | Implemented: the list reads connection_lease (live = active and unexpired) from Postgres, scoped by tenant in SQL and by the tenant-admin gate on the path tenant; neither items nor the opaque cursor carry a lease id (leaseRef = SHA-256 prefix, keyset = workspace id); the revoke reuses the principal revoker transaction (sessions, tickets, leases), is CSRF-guarded, tenant-admin only and audited as admin.session.revoke with a pseudonymous target, inside the same transaction; the dev realm gains a tenant-admin user for the kind e2e |`

```bash
go test -count=1 ./internal/store/ ./internal/api/ ./internal/backend/
git add internal/store/leases.go internal/store/leases_test.go internal/api internal/backend internal/observability web hack/quickstart docs/quickstart.md docs/security/threat-model.md
git commit -m "feat(admin): list live sessions and sign a user out everywhere

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 5: Tenant overview: live sessions, running per template, provisioning p95, stuck finalizers (F7) — tier: integrate

**Files:**
- Modify: `internal/store/migrations/0001_baseline.sql` (new table `workspace_boot`), `internal/store/testdata/schema.snapshot.sql`
- Modify: `internal/provisioning/outbox.go:113-158` (`appendIntent`)
- Create: `internal/provisioning/boot_test.go`
- Create: `internal/store/overview.go`, `internal/store/overview_test.go`
- Create: `internal/backend/bootrecorder.go`, `internal/backend/bootrecorder_test.go`
- Modify: `internal/backend/retention.go` (Task 1's `auditRetentionLoop` also prunes `workspace_boot`), `internal/observability/metrics.go` (`retentionTables`, near line 113)
- Modify: `internal/api/statusview.go` (add `TenantCounts`), `internal/api/statusview_test.go`
- Create: `internal/api/adminoverview.go`, `internal/api/adminoverview_test.go`
- Modify: `internal/api/{auditroutes.go,openapi.yaml,auditroutes_test.go,openapi_contract_test.go,tenantadmin_test.go}`, `internal/observability/metrics.go:121`, `internal/backend/wire.go` (`appRoutes.adminOverview`, its mount, and the recorder registration; `backend_test.go` needs no change because a nil `appRoutes` field skips the mount)
- Create: `web/tests/unit/admin/overview.test.tsx` (no overview unit test exists). Modify: `web/src/admin/{OverviewPage.tsx,api.ts}`, `web/src/i18n/{en,vi}/admin.ts`, `web/tests/mock-api/admin.ts`, `web/tests/admin.spec.ts`
- Modify: `hack/quickstart/smoke/smoke.spec.ts` (one admin test; the admin login and `ADMIN_USER`/`TENANT` constants come from Task 4)
- Modify: `docs/security/threat-model.md` (row S45)

**Interfaces:**
- Consumes: `appendIntent` (`outbox.go:113`, called in-tx by create, `SignalWorkspace`, the retained-attach create and Task 6a's `RestartWorkspace`), `K8sStatusView` (`statusview.go:91`, with its `cache`, `synced`/`lastEvent`/`maxStale` freshness fields), `observability.Metrics.ObserveProvisioningLatency(result string, d time.Duration)` (`metrics.go:371`, currently no callers; `result` is bounded to success/failure/other), `crcache.Cache` (`wire.go:326` `kcache`), `liveLeasePredicate` (const in `internal/store/leases.go`, Task 4; alias `l`, `now` bound as `$2`), `appRoutes` (Task 2), the admin login in the dev realm (Task 4).
- Produces:
  - Baseline table `workspace_boot(workspace_id text, runtime_generation bigint, tenant_id text, requested_at timestamptz DEFAULT now(), ready_at timestamptz NULL, PK(workspace_id, runtime_generation))`, plus index `(tenant_id, ready_at)`.
  - `type store.TenantOverview struct{ LiveSessions int64; ProvisioningP95Seconds *float64; ProvisioningSamples int64 }`, `func (d *store.DB) TenantOverview(ctx context.Context, tenantID string, now time.Time) (store.TenantOverview, error)`
  - `func (d *store.DB) MarkBootReady(ctx context.Context, workspaceID string, gen int64, readyAt time.Time) (time.Duration, bool, error)`. `ok` is true only for the first marking.
  - `func (d *store.DB) PruneWorkspaceBoot(ctx context.Context, before time.Time) (int64, error)`
  - `type api.TenantCounts struct{ Fresh bool; RunningByTemplate map[string]int; Phases map[string]int; StuckFinalizers int }`, `func (v *api.K8sStatusView) TenantCounts(ctx context.Context, tenantID string, stuckAfter time.Duration) (api.TenantCounts, error)`
  - `const api.StuckFinalizerAfter = 10 * time.Minute`
  - `func backend.registerBootRecorder(ctx context.Context, c crcache.Cache, db *store.DB, m *observability.Metrics, log *slog.Logger) error`
  - `GET /v1/admin/tenants/{tenant}/overview` returns `AdminOverview`. The audit action is `admin.overview.get`, and the response is cached 15 s per tenant.

- [ ] **Step 1: Baseline table**

Add after `workspace_activity` in `0001_baseline.sql`:

```sql
-- One row per runtime incarnation asked to boot (create/start intents with
-- desired Running). requested_at is written in the intent transaction;
-- ready_at by the backend when the CR first reports Ready for that
-- generation (status.startedAt). Feeds the overview's provisioning p95.
-- No FK: rows are pruned on their own 7-day horizon.
CREATE TABLE IF NOT EXISTS workspace_boot (
    workspace_id text NOT NULL,
    runtime_generation bigint NOT NULL,
    tenant_id text NOT NULL,
    requested_at timestamp with time zone DEFAULT now() NOT NULL,
    ready_at timestamp with time zone,
    CONSTRAINT workspace_boot_pkey PRIMARY KEY (workspace_id, runtime_generation)
);
CREATE INDEX IF NOT EXISTS workspace_boot_tenant_ready ON workspace_boot USING btree (tenant_id, ready_at);
```

Regenerate the snapshot: `TCDI_UPDATE_SNAPSHOT=1 go test ./internal/store/ -run TestBaseline_SchemaMatchesSnapshot -count=1`, then run `go test ./internal/store/ -run TestBaseline -count=1` without the variable. Expected: PASS. (If Task 1 already regenerated the snapshot, rebase the new table onto its result; the two edits touch different tables.)

- [ ] **Step 2: Failing test: intents record a boot request**

`internal/provisioning/boot_test.go`:

```go
package provisioning_test

import (
	"context"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func bootRows(t *testing.T, db *store.DB, wsID string) []int64 {
	t.Helper()
	rows, err := db.Pool().Query(context.Background(),
		`SELECT runtime_generation FROM workspace_boot WHERE workspace_id = $1 ORDER BY runtime_generation`, wsID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var gens []int64
	for rows.Next() {
		var g int64
		if err := rows.Scan(&g); err != nil {
			t.Fatal(err)
		}
		gens = append(gens, g)
	}
	return gens
}

func TestAppendIntent_RecordsBootRequestPerGeneration(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	const tenant = "tenant-boot"
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.SetQuota(ctx, tx, tenant, provisioning.ResourceVector{
			RunningSlots: 4, CPUMillis: 8000, MemoryBytes: 16 << 30, DiskBytes: 40 << 30})
	}); err != nil {
		t.Fatal(err)
	}
	svc := provisioning.NewService(db)
	req := provisioning.CreateRequest{
		OwnerIssuer: "iss", OwnerSubject: "sub", Name: "boot-ws",
		Template: provisioning.TemplateInfo{ID: "tpl_x", Name: "x", Runtime: "linux", Experience: "desktop"},
		Vector:   quotaVec, DesiredState: "Running", DataPolicy: "Ephemeral",
	}
	res, err := svc.CreateWorkspace(ctx, tenant, "key-boot-1", req, provisioning.RequestHash(req))
	if err != nil {
		t.Fatal(err)
	}
	if got := bootRows(t, db, res.ID); len(got) != 1 {
		t.Fatalf("after create: %v, want one boot row", got)
	}
	if _, err := svc.SignalWorkspace(ctx, tenant, "iss|sub", "", res.ID, "key-boot-stop", provisioning.IntentStop, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if got := bootRows(t, db, res.ID); len(got) != 1 {
		t.Fatalf("a stop must not record a boot: %v", got)
	}
	if _, err := svc.SignalWorkspace(ctx, tenant, "iss|sub", "", res.ID, "key-boot-start", provisioning.IntentStart, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	got := bootRows(t, db, res.ID)
	if len(got) != 2 || got[1] <= got[0] {
		t.Fatalf("after start: %v, want a second row for the bumped generation", got)
	}
}
```

Run `go test ./internal/provisioning/ -run TestAppendIntent_RecordsBootRequest -count=1`. Expected: FAIL (`after create: [], want one boot row`).

- [ ] **Step 3: Record the boot in `appendIntent`**

In `appendIntent`, after the `INSERT INTO outbox_intent` succeeds and before `return rev, nil`, add:

```go
	if (kind == IntentCreate || kind == IntentStart) && desired == "Running" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workspace_boot (workspace_id, runtime_generation, tenant_id)
			VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
			workspaceUID, gen, tenantID); err != nil {
			return 0, fmt.Errorf("append intent: record boot %w", err)
		}
	}
```

Run the Step 2 command, then `go test ./internal/provisioning/ -count=1`. Expected: PASS.

- [ ] **Step 4: Failing store tests for the overview aggregate**

`internal/store/overview_test.go`:

```go
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func seedBoot(t *testing.T, db *store.DB, ws string, gen int64, tenant string, requested time.Time) {
	t.Helper()
	if _, err := db.Pool().Exec(context.Background(),
		`INSERT INTO workspace_boot (workspace_id, runtime_generation, tenant_id, requested_at) VALUES ($1, $2, $3, $4)`,
		ws, gen, tenant, requested); err != nil {
		t.Fatal(err)
	}
}

func TestOverview_LiveSessionsAndP95(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	now := time.Now()
	for i, secs := range []int{10, 20, 30, 40, 100} {
		ws := "ws-p" + string(rune('a'+i))
		seedBoot(t, db, ws, 1, "t1", now.Add(-time.Hour))
		if _, ok, err := db.MarkBootReady(ctx, ws, 1, now.Add(-time.Hour).Add(time.Duration(secs)*time.Second)); err != nil || !ok {
			t.Fatalf("MarkBootReady(%s) = %v %v", ws, ok, err)
		}
	}
	seedBoot(t, db, "ws-old", 1, "t1", now.Add(-48*time.Hour))
	_, _, _ = db.MarkBootReady(ctx, "ws-old", 1, now.Add(-47*time.Hour)) // outside the 24 h window
	seedBoot(t, db, "ws-pending", 1, "t1", now)                         // never Ready: no sample

	seedWorkspace(t, db, "t1", "https://idp|alice", "ws-live")
	seedLiveLease(t, db, "lease-live", "ws-live", "t1", "https://idp|alice", "active", now, now.Add(time.Minute))
	seedWorkspace(t, db, "t1", "https://idp|alice", "ws-dead")
	seedLiveLease(t, db, "lease-dead", "ws-dead", "t1", "https://idp|alice", "active", now.Add(-time.Hour), now.Add(-time.Minute))

	o, err := db.TenantOverview(ctx, "t1", now)
	if err != nil {
		t.Fatal(err)
	}
	if o.LiveSessions != 1 || o.ProvisioningSamples != 5 || o.ProvisioningP95Seconds == nil {
		t.Fatalf("overview = %+v", o)
	}
	if p := *o.ProvisioningP95Seconds; p < 87 || p > 89 { // percentile_cont(0.95) over 10,20,30,40,100 = 88
		t.Fatalf("p95 = %v, want 88", p)
	}
	empty, _ := db.TenantOverview(ctx, "t-none", now)
	if empty.ProvisioningP95Seconds != nil || empty.LiveSessions != 0 {
		t.Fatalf("empty tenant = %+v", empty)
	}
}

func TestMarkBootReady_FirstWinsAndPrune(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	now := time.Now()
	seedBoot(t, db, "ws", 3, "t1", now.Add(-30*time.Second))
	d, ok, err := db.MarkBootReady(ctx, "ws", 3, now)
	if err != nil || !ok || d < 29*time.Second || d > 31*time.Second {
		t.Fatalf("first = %v %v %v", d, ok, err)
	}
	if _, ok, _ := db.MarkBootReady(ctx, "ws", 3, now.Add(time.Minute)); ok {
		t.Fatal("a second replica must not re-mark the same incarnation")
	}
	if _, ok, _ := db.MarkBootReady(ctx, "ws", 4, now); ok {
		t.Fatal("unknown generation marked")
	}
	if n, err := db.PruneWorkspaceBoot(ctx, now.Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("prune = %d %v", n, err)
	}
}
```

`seedLiveLease` is defined in `leases_test.go` (Task 4; the name `seedLease` is taken by `retention_test.go`). Run `go test ./internal/store/ -run 'TestOverview_|TestMarkBootReady' -count=1`. Expected: FAIL to compile.

- [ ] **Step 5: Implement `internal/store/overview.go`**

```go
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TenantOverview is the Postgres half of the admin overview (F7); the
// Kubernetes half (phases, templates, finalizers) comes from the informer.
type TenantOverview struct {
	LiveSessions           int64
	ProvisioningP95Seconds *float64 // nil when no incarnation became Ready in 24 h
	ProvisioningSamples    int64
}

// TenantOverview runs the overview's single aggregate query. "Live" is the
// shared liveLeasePredicate (leases.go), so the count matches the sessions
// list; the clamp absorbs clock skew between the database (requested_at) and
// the Kubernetes status.startedAt (ready_at).
func (d *DB) TenantOverview(ctx context.Context, tenantID string, now time.Time) (TenantOverview, error) {
	var o TenantOverview
	err := d.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM connection_lease l WHERE l.tenant_id = $1 AND `+liveLeasePredicate+`),
			b.p95, b.samples
		FROM (
			SELECT percentile_cont(0.95) WITHIN GROUP (
			           ORDER BY GREATEST(extract(epoch FROM (ready_at - requested_at)), 0)::float8) AS p95,
			       count(*) AS samples
			FROM workspace_boot
			WHERE tenant_id = $1 AND ready_at IS NOT NULL AND ready_at >= $2::timestamptz - interval '24 hours'
		) b`, tenantID, now).Scan(&o.LiveSessions, &o.ProvisioningP95Seconds, &o.ProvisioningSamples)
	if err != nil {
		return TenantOverview{}, fmt.Errorf("store: tenant overview %w", err)
	}
	return o, nil
}

// MarkBootReady records that incarnation (workspaceID, gen) became Ready
// at readyAt and returns its boot latency. ok is false when the row is
// unknown or already marked — every backend replica sees the same informer
// event and exactly one wins.
func (d *DB) MarkBootReady(ctx context.Context, workspaceID string, gen int64, readyAt time.Time) (time.Duration, bool, error) {
	var secs float64
	err := d.pool.QueryRow(ctx, `
		UPDATE workspace_boot SET ready_at = $3
		WHERE workspace_id = $1 AND runtime_generation = $2 AND ready_at IS NULL
		RETURNING GREATEST(extract(epoch FROM ($3::timestamptz - requested_at)), 0)::float8`,
		workspaceID, gen, readyAt).Scan(&secs)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: mark boot ready %w", err)
	}
	return time.Duration(secs * float64(time.Second)), true, nil
}

// PruneWorkspaceBoot deletes boot rows requested before `before`.
func (d *DB) PruneWorkspaceBoot(ctx context.Context, before time.Time) (int64, error) {
	tag, err := d.pool.Exec(ctx, `DELETE FROM workspace_boot WHERE requested_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("store: prune workspace_boot %w", err)
	}
	return tag.RowsAffected(), nil
}
```

Run the Step 4 command. Expected: PASS.

- [ ] **Step 6: Boot recorder (informer hook) with a unit test**

`internal/backend/bootrecorder.go`:

```go
package backend

import (
	"context"
	"log/slog"
	"time"

	toolscache "k8s.io/client-go/tools/cache"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// bootReadyTransition reports the (workspace id, generation, startedAt) of
// an incarnation that just became Ready, or ok=false.
func bootReadyTransition(oldObj, newObj any) (string, int64, time.Time, bool) {
	nw, ok := newObj.(*cdiv1.Workspace)
	if !ok || nw.Status.Phase != cdiv1.WorkspacePhaseReady || nw.Status.StartedAt == nil {
		return "", 0, time.Time{}, false
	}
	if ow, ok := oldObj.(*cdiv1.Workspace); ok && ow.Status.Phase == cdiv1.WorkspacePhaseReady &&
		ow.Status.ObservedRuntimeGeneration == nw.Status.ObservedRuntimeGeneration {
		return "", 0, time.Time{}, false
	}
	id := nw.Labels[provisioning.LabelWorkspaceUID]
	if id == "" {
		return "", 0, time.Time{}, false
	}
	return id, nw.Status.ObservedRuntimeGeneration, nw.Status.StartedAt.Time, true
}

// registerBootRecorder writes workspace_boot.ready_at and feeds the
// provisioning-latency histogram when a Workspace incarnation turns Ready.
func registerBootRecorder(ctx context.Context, c crcache.Cache, db *store.DB, m *observability.Metrics, log *slog.Logger) error {
	inf, err := c.GetInformer(ctx, &cdiv1.Workspace{})
	if err != nil {
		return err
	}
	// At most four writes in flight: a backend restart replays every Ready CR as
	// an Add event, and each of those is a (no-op) UPDATE.
	sem := make(chan struct{}, 4)
	mark := func(id string, gen int64, at time.Time) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return
		}
		mctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		d, ok, err := db.MarkBootReady(mctx, id, gen, at)
		if err != nil {
			log.Warn("boot recorder: mark ready failed", "workspace", id, "generation", gen, "err", err)
			return
		}
		if ok && m != nil {
			m.ObserveProvisioningLatency("success", d)
		}
	}
	_, err = inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if id, gen, at, ok := bootReadyTransition(nil, obj); ok {
				go mark(id, gen, at)
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			if id, gen, at, ok := bootReadyTransition(oldObj, newObj); ok {
				go mark(id, gen, at)
			}
		},
	})
	return err
}
```

On an informer resync or backend restart, the Add events replay Ready CRs. `MarkBootReady`'s `ready_at IS NULL` guard makes that a no-op, and every replica runs the hook (no leader gate needed: exactly one UPDATE wins, so exactly one replica observes the histogram). `Status.ObservedRuntimeGeneration` and `Status.StartedAt` exist (`api/v1/workspace_types.go:217` and `:270`), and `cdiv1.WorkspacePhaseReady` is in `api/v1/validation.go`.

In `wire.go` (`wireMerged`), register the hook before the cache starts, right after the `statusView, err := api.NewK8sStatusView(ctx, kcache)` block (`wire.go:361`; `metrics` may be nil and `log` is in scope there):

```go
	if err := registerBootRecorder(ctx, kcache, db, metrics, log); err != nil {
		return fmt.Errorf("boot recorder: %w", err)
	}
```

`internal/backend/bootrecorder_test.go`:

```go
package backend

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

func wsWith(phase cdiv1.WorkspacePhase, gen int64, started *metav1.Time) *cdiv1.Workspace {
	ws := &cdiv1.Workspace{}
	ws.Labels = map[string]string{provisioning.LabelWorkspaceUID: "ws_1"}
	ws.Status.Phase = phase
	ws.Status.ObservedRuntimeGeneration = gen
	ws.Status.StartedAt = started
	return ws
}

func TestBootReadyTransition(t *testing.T) {
	at := metav1.NewTime(time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC))
	cases := []struct {
		name     string
		old, new *cdiv1.Workspace
		want     bool
	}{
		{"provisioning to ready", wsWith(cdiv1.WorkspacePhaseProvisioning, 2, nil), wsWith(cdiv1.WorkspacePhaseReady, 2, &at), true},
		{"ready resync", wsWith(cdiv1.WorkspacePhaseReady, 2, &at), wsWith(cdiv1.WorkspacePhaseReady, 2, &at), false},
		{"ready under a new generation", wsWith(cdiv1.WorkspacePhaseReady, 2, &at), wsWith(cdiv1.WorkspacePhaseReady, 3, &at), true},
		{"ready without startedAt", nil, wsWith(cdiv1.WorkspacePhaseReady, 2, nil), false},
		{"failed", nil, wsWith(cdiv1.WorkspacePhaseFailed, 2, &at), false},
	}
	for _, c := range cases {
		var old any
		if c.old != nil {
			old = c.old
		}
		id, gen, got, ok := bootReadyTransition(old, c.new)
		if ok != c.want {
			t.Fatalf("%s: ok = %v", c.name, ok)
		}
		if ok && (id != "ws_1" || gen != c.new.Status.ObservedRuntimeGeneration || !got.Equal(at.Time)) {
			t.Fatalf("%s: %s %d %v", c.name, id, gen, got)
		}
	}
}
```

In `retention.go` (Task 1's `auditRetentionLoop`), add a constant `workspaceBootRetention = 7 * 24 * time.Hour` and, at the end of the `prune` closure (after the audit prune's `switch`), the sibling prune:

```go
			bn, berr := db.PruneWorkspaceBoot(ctx, time.Now().Add(-workspaceBootRetention))
			switch {
			case berr != nil && errors.Is(berr, context.Canceled):
			case berr != nil:
				log.Error("workspace_boot prune failed", "err", berr)
			default:
				if bn > 0 {
					log.Info("workspace_boot prune", "rows_pruned", bn)
				}
				if m != nil {
					m.AddRetentionRowsPruned("workspace_boot", float64(bn))
				}
			}
```

and add `"workspace_boot": {}` to the `retentionTables` map in `metrics.go` (next to Task 1's `"audit_event"`). Run `go test ./internal/backend/ -run TestBootReadyTransition -count=1`. Expected: PASS.

- [ ] **Step 7: Informer counts in the status view**

Append to `statusview.go`:

```go
// StuckFinalizerAfter is how long a Workspace may sit with a deletion
// timestamp and finalizers before the overview counts it as stuck.
const StuckFinalizerAfter = 10 * time.Minute

// TenantCounts is the Kubernetes half of the admin overview.
type TenantCounts struct {
	Fresh             bool
	RunningByTemplate map[string]int // spec.templateRef.name -> Ready workspaces
	Phases            map[string]int
	StuckFinalizers   int
}

// TenantCounts aggregates the tenant's Workspace CRs from the informer
// cache. Fresh is false when the watch is stale; the counts are then
// empty rather than last-known, because aggregates have no per-object
// snapshot.
func (v *K8sStatusView) TenantCounts(ctx context.Context, tenantID string, stuckAfter time.Duration) (TenantCounts, error) {
	out := TenantCounts{RunningByTemplate: map[string]int{}, Phases: map[string]int{}}
	v.mu.Lock()
	synced, last := v.synced, v.lastEvent
	v.mu.Unlock()
	now := v.now()
	if !synced || now.Sub(last) > v.maxStale {
		return out, nil
	}
	var list cdiv1.WorkspaceList
	if err := v.cache.List(ctx, &list, client.MatchingLabels{provisioning.LabelTenant: tenantID}); err != nil {
		return out, err
	}
	out.Fresh = true
	for i := range list.Items {
		ws := &list.Items[i]
		phase := string(ws.Status.Phase)
		if phase == "" {
			phase = string(cdiv1.WorkspacePhasePending)
		}
		out.Phases[phase]++
		if ws.Status.Phase == cdiv1.WorkspacePhaseReady {
			out.RunningByTemplate[ws.Spec.TemplateRef.Name]++
		}
		if ws.DeletionTimestamp != nil && len(ws.Finalizers) > 0 && now.Sub(ws.DeletionTimestamp.Time) > stuckAfter {
			out.StuckFinalizers++
		}
	}
	return out, nil
}
```

Unit test in `statusview_test.go`. The existing tests build `&K8sStatusView{...}` literals (see `TestK8sStatusView_FakeClockStaleness`) and there is no fake cache, so wrap controller-runtime's fake client (`listCache` in `imagestale_r4_test.go` ignores label selectors, so it cannot prove tenant filtering) (already used in `internal/operator` tests) in a `crcache.Cache` whose only working method is `List`. Add the imports `"sigs.k8s.io/controller-runtime/pkg/client/fake"` (the file already imports `crcache`, `client`, `metav1`, `runtime`, `provisioning` and `cdiv1`):

```go
// selectorCache is a crcache.Cache whose List reads a fake client, so label
// selectors are honoured (the package's existing listCache, imagestale_r4_test.go,
// returns a fixed slice and ignores them). Every other method would nil-panic.
type selectorCache struct {
	crcache.Cache
	c client.Client
}

func (l selectorCache) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return l.c.List(ctx, list, opts...)
}

func TestK8sStatusView_TenantCounts(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cdiv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	mk := func(name, tenant, tpl string, phase cdiv1.WorkspacePhase) *cdiv1.Workspace {
		ws := &cdiv1.Workspace{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ns-" + tenant,
			Labels: map[string]string{provisioning.LabelTenant: tenant},
		}}
		ws.Spec.TemplateRef.Name = tpl
		ws.Status.Phase = phase
		return ws
	}
	stuck := mk("stuck", "tenant-a", "chrome-r1", cdiv1.WorkspacePhaseTerminating)
	old := metav1.NewTime(now.Add(-11 * time.Minute))
	stuck.DeletionTimestamp, stuck.Finalizers = &old, []string{"workspaces.cdi.tinyorbit.vn/test"}
	young := mk("young", "tenant-a", "chrome-r1", cdiv1.WorkspacePhaseTerminating)
	recent := metav1.NewTime(now.Add(-time.Minute))
	young.DeletionTimestamp, young.Finalizers = &recent, []string{"workspaces.cdi.tinyorbit.vn/test"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		mk("r1", "tenant-a", "chrome-r1", cdiv1.WorkspacePhaseReady),
		mk("r2", "tenant-a", "chrome-r1", cdiv1.WorkspacePhaseReady),
		mk("f1", "tenant-a", "chrome-r1", cdiv1.WorkspacePhaseFailed),
		stuck, young,
		mk("other", "tenant-b", "other-r1", cdiv1.WorkspacePhaseReady),
	).Build()
	clock := now
	sv := &K8sStatusView{
		cache: selectorCache{c: c}, now: func() time.Time { return clock },
		maxStale: 15 * time.Second, lastKnown: map[string]ObservedStatus{},
	}
	sv.MarkSynced()

	got, err := sv.TenantCounts(context.Background(), "tenant-a", StuckFinalizerAfter)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Fresh || got.RunningByTemplate["chrome-r1"] != 2 || len(got.RunningByTemplate) != 1 ||
		got.Phases["Failed"] != 1 || got.Phases["Terminating"] != 2 || got.StuckFinalizers != 1 {
		t.Fatalf("tenant-a counts = %+v: tenant-b's workspace must not leak, only the 11-minute deletion is stuck", got)
	}

	clock = clock.Add(16 * time.Second) // the watch went quiet
	stale, err := sv.TenantCounts(context.Background(), "tenant-a", StuckFinalizerAfter)
	if err != nil || stale.Fresh || len(stale.RunningByTemplate) != 0 || stale.StuckFinalizers != 0 {
		t.Fatalf("stale counts = %+v, err %v: want Fresh=false and empty", stale, err)
	}
}
```

Run `go test ./internal/api/ -run TestK8sStatusView_TenantCounts -count=1`. Expected: FAIL to compile first (`TenantCounts undefined`), PASS after the `statusview.go` append above.

- [ ] **Step 8: API, contract and failing tests**

Apply the New API operation checklist.

`internal/api/auditroutes.go`: add `auditActionAdminOverviewGet = "admin.overview.get"` and `routeAdminOverviewGet = "GET /v1/admin/tenants/{tenant}/overview"`, and the `auditedRoutes` entry `routeAdminOverviewGet: {auditActionAdminOverviewGet, "tenant"},`. In `internal/observability/metrics.go:121` add `"admin.overview.get": {}` to `auditEventActions`.

OpenAPI: add the path (same response set as the sessions list: `200` `AdminOverview`, then `401`, `403`, `429`, `500`, `503`, each `$ref: "#/components/responses/<Name>"`; no `400`, the route has no input besides `{tenant}`):

```yaml
  /v1/admin/tenants/{tenant}/overview:
    get:
      operationId: getAdminOverview
      summary: Tenant health overview
      description: |
        Live desktop sessions (active, unexpired connection leases), Ready
        workspaces per template, the provisioning p95 over the last 24 hours
        (request to Ready, from the boot ledger), the Workspaces stuck
        deleting, and the phase counts. One aggregate query plus the
        operator's status counts; the answer is cached for 15 seconds per
        tenant. `fresh: false` means the Kubernetes watch is stale and the
        Kubernetes-derived counts are empty. Counts only, no identities.
        Requires the tenant-admin role on the named tenant.
      tags: [admin]
      parameters:
        - $ref: "#/components/parameters/Tenant"
      responses:
        "200":
          description: The tenant overview.
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/AdminOverview"
```

and the schema:

```yaml
    AdminOverview:
      type: object
      additionalProperties: false
      required: [liveSessions, runningByTemplate, phases, provisioning, stuckFinalizers, fresh, generatedAt]
      properties:
        liveSessions: {type: integer, format: int64, minimum: 0}
        runningByTemplate:
          type: array
          items:
            type: object
            additionalProperties: false
            required: [template, count]
            properties:
              template: {type: string}
              count: {type: integer, minimum: 0}
        phases: {type: object, additionalProperties: {type: integer, minimum: 0}}
        provisioning:
          type: object
          additionalProperties: false
          required: [samples]
          properties:
            p95Seconds: {type: number, minimum: 0, description: "Absent when no workspace became Ready in the last 24 hours."}
            samples: {type: integer, format: int64, minimum: 0}
        stuckFinalizers: {type: integer, minimum: 0}
        fresh: {type: boolean, description: "False when the Kubernetes watch is stale; Kubernetes counts are then empty."}
        generatedAt: {type: string, format: date-time}
```

`internal/api/adminoverview.go`:

```go
package api

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// OverviewSource is the Postgres aggregate (store.DB).
type OverviewSource interface {
	TenantOverview(ctx context.Context, tenantID string, now time.Time) (store.TenantOverview, error)
}

// TenantCounter is the informer aggregate (*K8sStatusView).
type TenantCounter interface {
	TenantCounts(ctx context.Context, tenantID string, stuckAfter time.Duration) (TenantCounts, error)
}

const overviewCacheTTL = 15 * time.Second

type templateCount struct {
	Template string `json:"template"`
	Count    int    `json:"count"`
}

type overviewProvisioning struct {
	P95Seconds *float64 `json:"p95Seconds,omitempty"`
	Samples    int64    `json:"samples"`
}

type adminOverview struct {
	LiveSessions      int64                `json:"liveSessions"`
	RunningByTemplate []templateCount      `json:"runningByTemplate"`
	Phases            map[string]int       `json:"phases"`
	Provisioning      overviewProvisioning `json:"provisioning"`
	StuckFinalizers   int                  `json:"stuckFinalizers"`
	Fresh             bool                 `json:"fresh"`
	GeneratedAt       time.Time            `json:"generatedAt"`
}

// AdminOverviewHandler serves GET /v1/admin/tenants/{tenant}/overview (F7),
// cached per tenant for 15 s so a dashboard left open costs one query per
// 15 s per replica.
type AdminOverviewHandler struct {
	db      OverviewSource
	k8s     TenantCounter
	tenants TenantResolver
	audit   observability.AuditSink
	now     func() time.Time

	mu    sync.Mutex
	cache map[string]adminOverview
}

func NewAdminOverviewHandler(db OverviewSource, k8s TenantCounter, t TenantResolver) *AdminOverviewHandler {
	return &AdminOverviewHandler{db: db, k8s: k8s, tenants: t, now: time.Now, cache: map[string]adminOverview{}}
}

func (h *AdminOverviewHandler) WithAuditSink(s observability.AuditSink) *AdminOverviewHandler {
	h.audit = s
	return h
}

func MountAdminOverviewRoutes(mux RouteMux, authn *Authenticator, h *AdminOverviewHandler) {
	mux.Handle(routeAdminOverviewGet, authn.RequireAuthPassive(
		audited(h.audit, routeAdminOverviewGet, http.HandlerFunc(h.Get))))
}

func (h *AdminOverviewHandler) Get(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantAdminPrincipal(h.tenants, w, r); !ok {
		return
	}
	tenant := r.PathValue("tenant")
	now := h.now()
	h.mu.Lock()
	cached, ok := h.cache[tenant]
	h.mu.Unlock()
	if ok && now.Sub(cached.GeneratedAt) < overviewCacheTTL {
		respondJSON(w, cached)
		return
	}
	agg, err := h.db.TenantOverview(r.Context(), tenant, now)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	counts, err := h.k8s.TenantCounts(r.Context(), tenant, StuckFinalizerAfter)
	if err != nil {
		writeError(w, r, CodeUnavailable, "workspace status unavailable")
		return
	}
	out := adminOverview{
		LiveSessions: agg.LiveSessions, Phases: counts.Phases, StuckFinalizers: counts.StuckFinalizers,
		Provisioning: overviewProvisioning{P95Seconds: agg.ProvisioningP95Seconds, Samples: agg.ProvisioningSamples},
		Fresh:        counts.Fresh, GeneratedAt: now.UTC(), RunningByTemplate: []templateCount{},
	}
	for name, n := range counts.RunningByTemplate {
		out.RunningByTemplate = append(out.RunningByTemplate, templateCount{Template: name, Count: n})
	}
	sort.Slice(out.RunningByTemplate, func(i, j int) bool {
		a, b := out.RunningByTemplate[i], out.RunningByTemplate[j]
		return a.Count > b.Count || (a.Count == b.Count && a.Template < b.Template)
	})
	h.mu.Lock()
	h.cache[tenant] = out
	h.mu.Unlock()
	respondJSON(w, out)
}
```

Before writing, check that `CodeUnavailable` is in `AllErrorCodes`. `revokeall.go` already uses it.

`internal/api/adminoverview_test.go`:

```go
type fakeOverviewDB struct{ calls map[string]int }

func (f *fakeOverviewDB) TenantOverview(_ context.Context, tenant string, _ time.Time) (store.TenantOverview, error) {
	f.calls[tenant]++
	p := 42.5
	live := int64(3)
	if tenant == "tenant-b" {
		live = 7
	}
	return store.TenantOverview{LiveSessions: live, ProvisioningP95Seconds: &p, ProvisioningSamples: 9}, nil
}

type fakeCounter struct{}

func (fakeCounter) TenantCounts(context.Context, string, time.Duration) (TenantCounts, error) {
	return TenantCounts{Fresh: true, RunningByTemplate: map[string]int{"b": 1, "a": 2}, Phases: map[string]int{"Ready": 3}, StuckFinalizers: 1}, nil
}

func TestAdminOverview_AggregatesAndCaches(t *testing.T) {
	db := &fakeOverviewDB{calls: map[string]int{}}
	h := NewAdminOverviewHandler(db, fakeCounter{}, defaultTenants())
	clock := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return clock }
	env := newAdminOverviewEnv(t, h)
	sess, csrf := loginAdmin(t, env, "admin-a")
	get := func() adminOverview {
		res := doReq(t, env, sess, csrf, http.MethodGet, "/v1/admin/tenants/tenant-a/overview", "", nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status %d", res.StatusCode)
		}
		return decodeBody[adminOverview](t, res)
	}
	o := get()
	if o.LiveSessions != 3 || o.StuckFinalizers != 1 || *o.Provisioning.P95Seconds != 42.5 ||
		len(o.RunningByTemplate) != 2 || o.RunningByTemplate[0].Template != "a" {
		t.Fatalf("overview = %+v", o)
	}
	clock = clock.Add(10 * time.Second)
	get()
	if db.calls["tenant-a"] != 1 {
		t.Fatalf("calls within 15 s = %d, want 1 (cached)", db.calls["tenant-a"])
	}
	clock = clock.Add(6 * time.Second)
	get()
	if db.calls["tenant-a"] != 2 {
		t.Fatalf("calls after 16 s = %d, want 2", db.calls["tenant-a"])
	}
}
```

Replace `testTenants()` with `defaultTenants()` (it maps `tenant-a` and `tenant-b`), and define the env helper in the same file, built like `newSessionPolicyEnv` (`adminsessionpolicy_test.go:73`; imports `bytes`, `log/slog`, `net/http/httptest`, `oidctest`):

```go
func newAdminOverviewEnv(t *testing.T, h *AdminOverviewHandler) *testEnv {
	t.Helper()
	iss, err := oidctest.NewIssuer()
	if err != nil {
		t.Fatalf("oidctest.NewIssuer: %v", err)
	}
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, nil))
	sessions := NewInMemorySessionStore(30 * time.Minute)
	a, err := NewAuthenticator(context.Background(), AuthConfig{
		Issuer:      iss.URL(),
		ClientID:    iss.ClientID,
		RedirectURL: "https://portal.test/auth/callback",
		LoginSealer: testLoginSealer(t),
	}, sessions, logger)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/auth/login", http.HandlerFunc(a.LoginHandler))
	mux.Handle("/auth/callback", http.HandlerFunc(a.CallbackHandler))
	MountAdminOverviewRoutes(mux, a, h)
	srv := httptest.NewServer(RequestID(Audit(logger)(mux)))
	env := &testEnv{issuer: iss, auth: a, store: sessions, server: srv, logs: logBuf}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return env
}
```

Make `fakeOverviewDB` tenant-aware so a cache that ignored the tenant key would show: return `LiveSessions: 3` for `tenant-a` and `7` for `tenant-b`, and count `calls` per tenant (`calls map[string]int`; adjust the assertions above to `db.calls["tenant-a"]`). Add these tests to `adminoverview_test.go` (every test builds `db := &fakeOverviewDB{calls: map[string]int{}}`):

```go
// The cache key is the tenant: an administrator of tenant-b never gets tenant-a's cached answer.
func TestAdminOverview_CacheIsPerTenant(t *testing.T) {
	db := &fakeOverviewDB{calls: map[string]int{}}
	h := NewAdminOverviewHandler(db, fakeCounter{}, defaultTenants())
	env := newAdminOverviewEnv(t, h)
	sa, ca := loginAdmin(t, env, "admin-a")
	a := decodeBody[adminOverview](t, doReq(t, env, sa, ca, http.MethodGet, "/v1/admin/tenants/tenant-a/overview", "", nil))
	env.issuer.TenantID = "tenant-b"
	sb, cb := loginAdmin(t, env, "admin-b")
	b := decodeBody[adminOverview](t, doReq(t, env, sb, cb, http.MethodGet, "/v1/admin/tenants/tenant-b/overview", "", nil))
	if a.LiveSessions != 3 || b.LiveSessions != 7 || db.calls["tenant-b"] != 1 {
		t.Fatalf("a = %+v, b = %+v, calls = %v: each tenant needs its own cache entry", a, b, db.calls)
	}
	// tenant-b's administrator naming tenant-a is refused and never reaches the aggregate.
	before := db.calls["tenant-a"]
	if res := doReq(t, env, sb, cb, http.MethodGet, "/v1/admin/tenants/tenant-a/overview", "", nil); res.StatusCode != http.StatusForbidden || db.calls["tenant-a"] != before {
		t.Fatalf("cross-tenant overview: status %d, calls %v", res.StatusCode, db.calls)
	}
}

func TestAdminOverview_NonAdminRefused(t *testing.T) {
	db := &fakeOverviewDB{calls: map[string]int{}}
	env := newAdminOverviewEnv(t, NewAdminOverviewHandler(db, fakeCounter{}, defaultTenants()))
	sess, csrf := loginGroups(t, env, "user-a")
	if res := doReq(t, env, sess, csrf, http.MethodGet, "/v1/admin/tenants/tenant-a/overview", "", nil); res.StatusCode != http.StatusForbidden || len(db.calls) != 0 {
		t.Fatalf("non-admin: status %d, calls %v", res.StatusCode, db.calls)
	}
}
```

Shared-test checklist items:

- `auditroutes_test.go`, in `newCoverageEnv` after the Task 4 sessions mount: `MountAdminOverviewRoutes(mux, a, NewAdminOverviewHandler(&fakeOverviewDB{calls: map[string]int{}}, fakeCounter{}, tenants).WithAuditSink(sink))`, and a driver in the `routes` table before `POST /v1/logout`:

  ```go
  		{"GET /v1/admin/tenants/{}/overview", auditActionAdminOverviewGet, func() {
  			doReq(t, env, sessA, csrfA, http.MethodGet, "/v1/admin/tenants/tenant-a/overview", "", nil).Body.Close()
  		}},
  ```

- `openapi_contract_test.go`, two entries in the `calls` slice of `TestOpenAPIContract_ResponsesValidate` (the 200 body is validated against `AdminOverview`, the refusal against `Error`):

  ```go
  		{"GET", "/v1/admin/tenants/{tenant}/overview", func() *http.Response {
  			return doReq(t, env, sessA, csrfA, "GET", "/v1/admin/tenants/tenant-a/overview", "", nil)
  		}},
  		{"GET", "/v1/admin/tenants/{tenant}/overview", func() *http.Response {
  			return doReq(t, env, sess, csrf, "GET", "/v1/admin/tenants/tenant-a/overview", "", nil)
  		}},
  ```

- `tenantadmin_test.go`: in `newAdminGroupEnv` after the sessions mount, `MountAdminOverviewRoutes(mux, a, NewAdminOverviewHandler(&fakeOverviewDB{calls: map[string]int{}}, fakeCounter{}, defaultTenants()))`, and

  ```go
  func TestAdminOverview_CustomAdminGroups(t *testing.T) {
  	env, _ := newAdminGroupEnv(t, []string{testCustomAdminGroup})
  	const path = "/v1/admin/tenants/tenant-a/overview"
  	sess, csrf := loginGroups(t, env, "admin-a", testCustomAdminGroup)
  	r := doReq(t, env, sess, csrf, http.MethodGet, path, "", nil)
  	r.Body.Close()
  	if r.StatusCode != http.StatusOK {
  		t.Fatalf("member GET: status=%d, want 200", r.StatusCode)
  	}
  	sess, csrf = loginGroups(t, env, "admin-b", TenantAdminGroup)
  	r = doReq(t, env, sess, csrf, http.MethodGet, path, "", nil)
  	r.Body.Close()
  	if r.StatusCode != http.StatusForbidden {
  		t.Fatalf("non-member GET: status=%d, want 403", r.StatusCode)
  	}
  }
  ```

Wire in `wire.go` `newAppHandler` (it receives `statusView *api.K8sStatusView` and has `db`, `tenants`): add the field `adminOverview *api.AdminOverviewHandler` to `appRoutes`, set `adminOverview: api.NewAdminOverviewHandler(db, statusView, tenants).WithAuditSink(appAudit)`, and mount it in `appMux` with `if h.adminOverview != nil { api.MountAdminOverviewRoutes(mux, authn, h.adminOverview) }`. Run `go test ./internal/api/ -run 'AdminOverview|TenantCounts|AuditCoverage|OpenAPI' -count=1 && go test ./internal/backend/ -count=1`. Expected: PASS.

- [ ] **Step 9: Overview cards**

In `web/src/admin/api.ts`:

```ts
export type AdminOverview = components["schemas"]["AdminOverview"];

export async function fetchAdminOverview(api: ApiClient, tenant: string): Promise<AdminOverview> {
  return unwrap(await api.GET("/v1/admin/tenants/{tenant}/overview", { params: { path: { tenant } } }));
}
```

In `OverviewPage.tsx` (imports `Alert, Card, Grid, Section, Spinner, StatusPill` from `../design`; add `Badge`, and `loadMe` and `useLoader` from `../app/me`, where `useLoader` is already imported):
- The health data loads through its own loader so a failing overview call (for example `503`) does not blank the capacity and phase sections, and each loader shows its own `ApiErrorAlert`:

  ```tsx
  async function loadHealth(api: ApiClient): Promise<AdminOverview> {
    const me = await loadMe(api);
    return fetchAdminOverview(api, me.tenant);
  }
  ```

  In `OverviewPage`: `const h = useLoader(loadHealth, "overview-health");` and a second `<ApiErrorAlert error={h.error} onRetry={h.reload} />` next to the existing one. Do not touch `loadOverview`.
- The `byPhase` section keeps its client-side counts.
- A new `Section` titled `admin.overview.health.title` comes first, rendered when `h.data` is set (a `Spinner` with `t("admin.overview.loading")` while `h.loading && !h.data`), with a `Grid gap={3} min="xs"` of four `Card as="div" className="tc-admin-stat"`s (the pattern of the phase cards):
  - live sessions (`h.data.liveSessions`);
  - provisioning p95: `` `${Math.round(p95)} s` `` when `h.data.provisioning.p95Seconds` is defined, else `t("admin.overview.health.noSamples")`, plus the `admin.overview.health.samples` line with `n: h.data.provisioning.samples`;
  - stuck finalizers (`h.data.stuckFinalizers`), with `<Badge tone="warning">` and a `<Link to="/admin/workspaces">` when > 0;
  - running by template, as a `<ul className="tc-admin-list">` of `` `${template} — ${count}` `` (the entries are the Workspace's template revision object names, for example `chrome-r1`), or `admin.overview.health.noneRunning`.
- When `h.data.fresh === false`, render `<Alert tone="warning">{t("admin.overview.health.stale")}</Alert>` above the cards. `Badge` takes `tone: Tone` (`neutral | info | success | warning | danger | accent`) and renders the class `tc-badge--<tone>`.

Strings:

| key | en | vi |
|---|---|---|
| `admin.overview.health.title` | Health | Tình trạng |
| `admin.overview.health.liveSessions` | Live sessions | Phiên đang hoạt động |
| `admin.overview.health.p95` | Start time (p95, 24 h) | Thời gian khởi động (p95, 24 giờ) |
| `admin.overview.health.samples` | {n} starts measured | Đã đo {n} lần khởi động |
| `admin.overview.health.noSamples` | No starts in 24 h | Không có lần khởi động nào trong 24 giờ |
| `admin.overview.health.stuck` | Stuck deletions | Xoá bị kẹt |
| `admin.overview.health.running` | Running by template | Đang chạy theo template |
| `admin.overview.health.noneRunning` | Nothing running | Không có workspace nào đang chạy |
| `admin.overview.health.stale` | Workspace status is out of date; Kubernetes counts are hidden until the watch recovers. | Trạng thái workspace chưa cập nhật; số liệu Kubernetes bị ẩn cho đến khi kết nối theo dõi phục hồi. |

Mock, in `web/tests/mock-api/admin.ts` `adminArea`: add the route match `^/v1/admin/tenants/([^/]+)/overview$` to `api(req)` next to the other admin matches, served by

```ts
  // GET /v1/admin/tenants/{tenant}/overview — fixed counts; /_control/admin/overview {fresh} flips the stale flag.
  let overviewFresh = true; // reset to true in reset()
  function adminOverview(req: MockRequest, tenant: string): MockResponse {
    if (!t.isAdmin() || tenant !== t.me.tenant) {
      return err(403, "FORBIDDEN", "tenant administration requires the tenant-admin role", false);
    }
    if (req.method !== "GET") return err(405, "INVALID_REQUEST", "method not allowed", false);
    const failed = t.takeFailure(req);
    if (failed) return failed;
    return ok(200, {
      liveSessions: 2,
      runningByTemplate: overviewFresh
        ? [{ template: "chrome-r1", count: 2 }, { template: "linux-r3", count: 1 }]
        : [],
      phases: overviewFresh ? { Ready: 3, Stopped: 1 } : {},
      provisioning: { p95Seconds: 12.4, samples: 7 },
      stuckFinalizers: 1,
      fresh: overviewFresh,
      generatedAt: "2026-10-13T09:00:00Z",
    });
  }
```

and the control case `case "/_control/admin/overview": overviewFresh = req.body?.fresh !== false; return ok(200, { fresh: overviewFresh });` in `control(req)`.

Unit test `web/tests/unit/admin/overview.test.tsx` (pattern of `quota.test.tsx`: `createMockApi({ areas: [adminArea, dataArea] })`, `loginCookies()`, `makeAdmin(api)`, `renderWithApi(<OverviewPage />, api)`):

```tsx
import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import { createMockApi, loginCookies, renderWithApi } from "../helpers";
import { control, makeAdmin } from "./helpers";
import { OverviewPage } from "../../../src/admin/OverviewPage";
import { adminArea } from "../../mock-api/admin.ts";
import { dataArea } from "../../mock-api/data.ts";

function setup() {
  const api = createMockApi({ areas: [adminArea, dataArea] });
  loginCookies();
  makeAdmin(api);
  return api;
}

describe("overview: health cards", () => {
  it("shows live sessions, p95, sample count, the stuck-deletion warning and running per template", async () => {
    renderWithApi(<OverviewPage />, setup());
    const health = await screen.findByRole("region", { name: "Health" });
    expect(health).toHaveTextContent("12 s");
    expect(health).toHaveTextContent("7 starts measured");
    expect(health).toHaveTextContent("Stuck deletions");
    expect(health.querySelector(".tc-badge--warning")).not.toBeNull();
    expect(health).toHaveTextContent("chrome-r1 — 2");
  });

  it("says so when the Kubernetes watch is stale", async () => {
    const api = setup();
    control(api, "/_control/admin/overview", { fresh: false });
    renderWithApi(<OverviewPage />, api);
    expect(await screen.findByText(/Workspace status is out of date/)).toBeInTheDocument();
    expect(screen.getByText("Nothing running")).toBeInTheDocument();
  });
});
```

Playwright: add a new test to `web/tests/admin.spec.ts` (no existing test title contains "overview", so `-g overview` would match nothing):

```ts
test("overview health cards show the aggregate and the stale notice", async ({ page, request }) => {
  await makeAdmin(request);
  await login(page, "/admin");
  const health = page.getByRole("region", { name: "Health" });
  await expect(health).toContainText("12 s");
  await expect(health).toContainText("7 starts measured");
  await expect(health.getByText("chrome-r1 — 2")).toBeVisible();
  await request.post(`${MOCK}/_control/admin/overview`, { data: { fresh: false } });
  await page.reload();
  await expect(page.getByText(/Workspace status is out of date/)).toBeVisible();
});
```

Run:

```bash
cd web && npm run generate && npm run typecheck && npm run lint:strings && npx vitest run tests/unit/admin && npx playwright test tests/admin.spec.ts -g "overview health"
```

Expected: PASS (the existing `quota.test.tsx` renders `OverviewPage` too and keeps passing because the mock now serves the overview route).

- [ ] **Step 10: Kind e2e: the overview counts a live session and a boot**

Append one test to `hack/quickstart/smoke/smoke.spec.ts` (Task 4 added `ADMIN_USER`, `TENANT` and the `demo-admin` realm user). A user opens a desktop, which mints a live lease and, through the intent transaction and the informer hook, a `workspace_boot` row with `ready_at`; the administrator's overview must show all three. The answer is cached for 15 s, so the test polls.

```ts
test("admin overview: a live session, a running workspace and a measured boot are counted", async ({ browser }) => {
  const userCtx = await browser.newContext({ ignoreHTTPSErrors: true });
  const userPage = await userCtx.newPage();
  await login(userPage);
  const id = await createBrowserWorkspace(userPage, `ovw-${Date.now().toString(36)}`);
  await connectSessionFrame(userPage, id);

  const adminCtx = await browser.newContext({ ignoreHTTPSErrors: true });
  const page = await adminCtx.newPage();
  await login(page, ADMIN_USER);

  type Overview = {
    liveSessions: number;
    runningByTemplate: { template: string; count: number }[];
    provisioning: { p95Seconds?: number; samples: number };
    stuckFinalizers: number;
    fresh: boolean;
  };
  const overview = async (): Promise<Overview> => {
    const r = await page.request.get(`/v1/admin/tenants/${TENANT}/overview`);
    expect(r.status()).toBe(200);
    return (await r.json()) as Overview;
  };
  await expect
    .poll(async () => {
      const o = await overview();
      return (
        o.fresh &&
        o.liveSessions >= 1 &&
        o.runningByTemplate.reduce((n, t) => n + t.count, 0) >= 1 &&
        o.provisioning.samples >= 1 &&
        (o.provisioning.p95Seconds ?? 0) > 0
      );
    }, { timeout: 90_000, intervals: [3_000], message: "overview counts the live session, the running workspace and the measured boot" })
    .toBe(true);
  expect((await overview()).stuckFinalizers).toBeGreaterThanOrEqual(0);

  // The UI shows the same cards.
  await page.goto("/admin");
  await expect(page.getByRole("region", { name: "Health" })).toContainText("Live sessions");

  await userCtx.close();
  await adminCtx.close();
});
```

Compile check (no cluster): `(cd hack/quickstart/smoke && npx playwright test --list > /tmp/f7-smoke-list.log 2>&1; echo "rc=$?"; grep -c 'admin overview' /tmp/f7-smoke-list.log)`. Expected: `rc=0` and `1`. It runs for real in CI's `quickstart` job.

- [ ] **Step 11: Threat model and commit**

Row S45: `| S45 | Admin overview | Implemented: tenant-admin only; one tenant-scoped aggregate query plus informer counts filtered by the tenant label; cached 15 s per tenant per replica; returns counts only, no identities; the boot ledger holds workspace ids and timestamps only and is pruned after 7 days |`

```bash
go test -count=1 ./internal/store/ ./internal/provisioning/ ./internal/api/ ./internal/backend/
git add internal/store internal/provisioning/outbox.go internal/provisioning/boot_test.go internal/api internal/backend internal/observability web hack/quickstart/smoke/smoke.spec.ts docs/security/threat-model.md
git commit -m "feat(admin): overview health cards backed by one aggregate and informer counts

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 6: Failed-workspace recovery, part 1: failure vocabulary and operator (F1) — tier: design

This task and Task 6a deliver F1. Task 6 is the operator half: it names the failure reasons, stops rebuilding a lost pod inside its generation, and makes unrecoverable failures terminal. It needs envtest. Task 6a is the control-plane half (`:start` recovery, generation fence, revocation, portal, e2e). After Task 6 alone the build and every test are green; a `Failed/RuntimeLost` workspace is still stuck until Task 6a lands.

**Files:**
- Create: `api/v1/failure.go`, `api/v1/failure_test.go`
- Modify: `internal/operator/workspace_controller.go` (imports, reason consts 91-125, `reconcileRunning` 392-565, `reconcileFailed` 641-661), `internal/operator/status.go:38`
- Modify: `internal/runtime/backend.go` (`Observation.Message`), `internal/runtime/linux/backend.go` (`ErrHomeVolumeLost`, `ensurePVC` 735-793, `Observe` 511-565, new `waitingMessage`)
- Create: `internal/runtime/linux/home_volume_test.go`, `internal/operator/recovery_test.go` (envtest)
- Modify: `internal/operator/workspace_controller_test.go` (subtest "pod replacement keeps generation and changes runtimeUID", ~515-550)
- Modify: `docs/lifecycle-reasons.md` (`## Failed` section)

**Interfaces:**
- Consumes: `linux.ErrNameConflict` / `ErrRetainedClaimMissing` / `ErrTemplateRejected` (the `Ensure` error switch), `Backend.Observe`, `writeStatus`, `failedCleanupRemaining` (`lifecycle.go:193`), `degradedTransitionTime`.
- Produces (Task 6a and Task 8 consume `FailureRecoverable`/`FailureUnrecoverable`):
  - `api/v1`: `const FailureBootDeadlineExceeded = "BootDeadlineExceeded"`, `FailureFailedCleanup = "FailedCleanup"`, `FailureRuntimeLost = "RuntimeLost"`, `FailureImagePolicyDenied = "ImagePolicyDenied"`, `FailureHomeVolumeLost = "HomeVolumeLost"`, `FailureTemplateRejected = "TemplateRejected"`; `func FailureRecoverable(reason string) bool`; `func FailureUnrecoverable(reason string) bool`
  - `internal/runtime`: `Observation.Message string` (the waiting container's message; never shown to users verbatim)
  - `linux.ErrHomeVolumeLost`
  - Operator behaviour: a Ready incarnation whose pod is gone, terminating or failed goes `Failed/RuntimeLost` and is not rebuilt in the same generation; `ImagePolicyDenied`, `HomeVolumeLost` and `TemplateRejected` go `Failed` at once and keep their reason through the cleanup pass.

- [ ] **Step 1: Failure vocabulary in `api/v1`**

`api/v1/failure.go`:

```go
package v1

// Failure reasons are written on the Degraded condition of a Failed
// Workspace. The operator writes them; the API reads them to decide whether
// :start may recover the workspace with a new runtime generation (F1).
// docs/lifecycle-reasons.md explains each one to operators.
const (
	// FailureBootDeadlineExceeded — the incarnation did not become Ready
	// within the template's bootDeadline.
	FailureBootDeadlineExceeded = "BootDeadlineExceeded"
	// FailureFailedCleanup — the failed incarnation was torn down after
	// the cleanup delay.
	FailureFailedCleanup = "FailedCleanup"
	// FailureRuntimeLost — a Ready incarnation's pod disappeared, is
	// terminating, or failed (node loss, eviction, manual delete).
	FailureRuntimeLost = "RuntimeLost"
	// FailureImagePolicyDenied — admission or the registry refused the
	// runtime image by policy. A restart would be refused the same way.
	FailureImagePolicyDenied = "ImagePolicyDenied"
	// FailureHomeVolumeLost — a Retain workspace's home volume is gone. A
	// restart would silently start on an empty home.
	FailureHomeVolumeLost = "HomeVolumeLost"
	// FailureTemplateRejected — the runtime backend refused the template's
	// requested configuration.
	FailureTemplateRejected = "TemplateRejected"
)

// FailureRecoverable reports whether :start may recover a Failed workspace
// with this reason by minting a new runtime generation.
func FailureRecoverable(reason string) bool {
	switch reason {
	case FailureBootDeadlineExceeded, FailureFailedCleanup, FailureRuntimeLost:
		return true
	}
	return false
}

// FailureUnrecoverable reports whether the reason is terminal: Failed is
// entered at once and :start is refused until an administrator fixes the
// cause.
func FailureUnrecoverable(reason string) bool {
	switch reason {
	case FailureImagePolicyDenied, FailureHomeVolumeLost, FailureTemplateRejected:
		return true
	}
	return false
}
```

`api/v1/failure_test.go`:

```go
package v1

import "testing"

func TestFailureClassesAreDisjointAndComplete(t *testing.T) {
	all := []string{FailureBootDeadlineExceeded, FailureFailedCleanup, FailureRuntimeLost,
		FailureImagePolicyDenied, FailureHomeVolumeLost, FailureTemplateRejected}
	for _, r := range all {
		if FailureRecoverable(r) == FailureUnrecoverable(r) {
			t.Errorf("%s must be exactly one of recoverable/unrecoverable", r)
		}
	}
	for _, r := range []string{"", "TemplateInvalid", "Nominal"} {
		if FailureRecoverable(r) || FailureUnrecoverable(r) {
			t.Errorf("%q is not a failure reason", r)
		}
	}
}
```

In `workspace_controller.go`, make the existing consts aliases: `ReasonTemplateRejected = cdiv1.FailureTemplateRejected`, `ReasonBootDeadlineExceeded = cdiv1.FailureBootDeadlineExceeded`. In `status.go:38`, use `ReasonFailedCleanup = cdiv1.FailureFailedCleanup`. Run:

```bash
go test ./api/v1/ -count=1 && make manifests generate sync-chart-crds && git status --short config deploy
```

Expected: PASS, and `git status` prints nothing for `config` and `deploy` (no type changed, so no CRD diff). `TemplateInvalid` is deliberately in neither list: it stays a Pending hold (ruling 7).

- [ ] **Step 2: Backend signals: `Observation.Message`, `ErrHomeVolumeLost`**

In `internal/runtime/backend.go`, add this to `Observation`, after `Reason`:

```go
	// Message is the waiting container's status message when Reason is a
	// waiting reason (e.g. the registry's text behind ErrImagePull). Used
	// only to classify policy denials; never surfaced verbatim.
	Message string
```

In `internal/runtime/linux/backend.go`:

```go
// ErrHomeVolumeLost reports that a Retain workspace whose home was already
// provisioned has no home PVC: building a fresh one would silently start
// the user on an empty home.
var ErrHomeVolumeLost = errors.New("linux backend: home volume lost")

// waitingMessage returns the first waiting container's message.
func waitingMessage(pod *corev1.Pod) string {
	for _, list := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, cs := range list {
			if w := cs.State.Waiting; w != nil && w.Message != "" {
				return w.Message
			}
		}
	}
	return ""
}
```

In `Observe`, next to `obs.Reason = podReason(pod)`, add `obs.Message = waitingMessage(pod)`. In `ensurePVC`, in the own-volume branch (the code after the `if ref := ws.Annotations[AnnotationRetainedPVC]; ref != "" { ... }` attach block, where `Get` of `PVCName(uid)` fails), replace `if !apierrors.IsNotFound(err) { return nil, err }` with:

```go
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	// The home was provisioned for an earlier incarnation; its absence is
	// data loss, not a first boot.
	if ws.Status.ObservedRuntimeGeneration > 0 {
		return nil, ErrHomeVolumeLost
	}
```

`ensurePVC` is reached only for `DataPolicyRetain` (`Ensure` guards the call), and the attach path (`AnnotationRetainedPVC`) returns before this branch, so an attached disk that vanished still surfaces as the existing wrapped `retained PVC ... not found` error.

`internal/runtime/linux/home_volume_test.go` (the helpers `backendScheme`, `testWorkspace` and `testTemplate` live in `backend_test.go`, same package):

```go
package linux

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
)

func retainWorkspace(observedGen int64) *cdiv1.Workspace {
	ws := testWorkspace()
	ws.Spec.DataPolicy = cdiv1.DataPolicyRetain
	ws.Status.ObservedRuntimeGeneration = observedGen
	return ws
}

func TestEnsure_RetainHomeMissingAfterFirstBootIsLost(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(backendScheme(t)).Build()
	_, err := New(c, Options{}).Ensure(context.Background(), retainWorkspace(1), testTemplate(nil))
	if !errors.Is(err, ErrHomeVolumeLost) {
		t.Fatalf("Ensure = %v, want ErrHomeVolumeLost", err)
	}
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(context.Background(), pvcs); err != nil || len(pvcs.Items) != 0 {
		t.Fatalf("a fresh empty home was created: %d claims, err %v", len(pvcs.Items), err)
	}
}

func TestEnsure_RetainFirstBootCreatesTheHome(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(backendScheme(t)).Build()
	if _, err := New(c, Options{}).Ensure(context.Background(), retainWorkspace(0), testTemplate(nil)); err != nil {
		t.Fatalf("first boot: %v", err)
	}
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(context.Background(), pvcs); err != nil || len(pvcs.Items) != 1 {
		t.Fatalf("first boot must create the home: %d claims, err %v", len(pvcs.Items), err)
	}
}

func TestWaitingMessage(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}}, // no message: skipped
		ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull", Message: "denied by policy"}}}},
	}}
	if got := waitingMessage(pod); got != "denied by policy" {
		t.Fatalf("waitingMessage = %q", got)
	}
	if got := waitingMessage(&corev1.Pod{}); got != "" {
		t.Fatalf("waitingMessage of an empty pod = %q", got)
	}
}
```

Run `go test ./internal/runtime/linux/ -count=1`. Expected: PASS (the first test fails to compile until `ErrHomeVolumeLost` exists, so write it after the code above).

- [ ] **Step 3: Failing envtest for the operator**

`internal/operator/recovery_test.go` (helpers `startEnv`, `newNamespace`, `newTemplate`, `newWorkspace`, `newReconciler`, `reconcile`, `markPodReady`, `thePod`, `getWorkspace` are in `workspace_controller_test.go`; `degradedReasonOf` is written in Step 4):

```go
package operator

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
	tcdiruntime "github.com/tinyorbitvn/tinycdi/internal/runtime"
)

func readyWorkspace(t *testing.T, name string, mutate func(*cdiv1.Workspace)) (*WorkspaceReconciler, types.NamespacedName, *cdiv1.Workspace, func()) {
	t.Helper()
	env, c := startEnv(t)
	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-"+name, nil)
	ws := newWorkspace(t, c, ns, name, "tpl-"+name, mutate)
	key := types.NamespacedName{Name: ws.Name, Namespace: ns}
	r := newReconciler(c)
	reconcile(t, r, key)
	markPodReady(t, c, thePod(t, c, ns, ws.UID))
	reconcile(t, r, key)
	if got := getWorkspace(t, c, key); got.Status.Phase != cdiv1.WorkspacePhaseReady {
		t.Fatalf("setup: phase %q, want Ready", got.Status.Phase)
	}
	return r, key, ws, func() { _ = env.Stop() }
}

func TestRecovery_DeletedPodGoesRuntimeLostWithoutRebuild(t *testing.T) {
	r, key, ws, stop := readyWorkspace(t, "ws-lost", nil)
	defer stop()
	ctx := context.Background()
	pod := thePod(t, r.Client, key.Namespace, ws.UID)
	if err := r.Client.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, key)
	got := getWorkspace(t, r.Client, key)
	if got.Status.Phase != cdiv1.WorkspacePhaseFailed || degradedReasonOf(got) != cdiv1.FailureRuntimeLost {
		t.Fatalf("phase=%q reason=%q, want Failed/RuntimeLost", got.Status.Phase, degradedReasonOf(got))
	}
	reconcile(t, r, key)
	pods := &corev1.PodList{}
	if err := r.Client.List(ctx, pods); err != nil {
		t.Fatal(err)
	}
	for _, p := range pods.Items {
		if p.Namespace == key.Namespace && p.DeletionTimestamp == nil {
			t.Fatalf("pod %s rebuilt inside the lost generation", p.Name)
		}
	}
}

func TestRecovery_NewGenerationAfterRuntimeLostComesBackReady(t *testing.T) {
	r, key, ws, stop := readyWorkspace(t, "ws-recover", nil)
	defer stop()
	ctx := context.Background()
	old := thePod(t, r.Client, key.Namespace, ws.UID)
	if err := r.Client.Delete(ctx, old); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, key)
	cur := getWorkspace(t, r.Client, key)
	cur.Spec.RuntimeGeneration = 2 // what the applier writes for RestartWorkspace's start intent
	cur.Spec.IntentRevision = 2
	if err := r.Client.Update(ctx, cur); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, key)
	pod := thePod(t, r.Client, key.Namespace, ws.UID)
	if pod.UID == old.UID {
		t.Fatal("recovery reused the lost incarnation")
	}
	markPodReady(t, r.Client, pod)
	reconcile(t, r, key)
	got := getWorkspace(t, r.Client, key)
	if got.Status.Phase != cdiv1.WorkspacePhaseReady || got.Status.ObservedRuntimeGeneration != 2 {
		t.Fatalf("phase=%q gen=%d, want Ready/2", got.Status.Phase, got.Status.ObservedRuntimeGeneration)
	}
	if degradedReasonOf(got) != "" {
		t.Fatalf("Degraded still %q after recovery", degradedReasonOf(got))
	}
}

func TestRecovery_UnrecoverableReasonsFailImmediatelyAndStick(t *testing.T) {
	env, c := startEnv(t)
	defer func() { _ = env.Stop() }()
	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-hv", nil)
	ws := newWorkspace(t, c, ns, "ws-hv", "tpl-hv", func(w *cdiv1.Workspace) { w.Spec.DataPolicy = cdiv1.DataPolicyRetain })
	key := types.NamespacedName{Name: ws.Name, Namespace: ns}
	now := time.Now()
	r := newReconciler(c)
	r.Now = func() time.Time { return now }
	reconcile(t, r, key)
	markPodReady(t, c, thePod(t, c, ns, ws.UID))
	reconcile(t, r, key) // envtest has no volume controller: the claim never binds, the pod was still observed (observedRuntimeGeneration 1)
	// The home claim is deleted underneath the workspace, then a new generation starts.
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(context.Background(), pvcs); err != nil {
		t.Fatal(err)
	}
	for i := range pvcs.Items {
		if pvcs.Items[i].Namespace == ns {
			pvcs.Items[i].Finalizers = nil // no kube-controller-manager here to release pvc-protection
			if err := c.Update(context.Background(), &pvcs.Items[i]); err != nil {
				t.Fatal(err)
			}
			if err := c.Delete(context.Background(), &pvcs.Items[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	cur := getWorkspace(t, c, key)
	cur.Spec.RuntimeGeneration = 2
	cur.Spec.IntentRevision = 2
	if err := c.Update(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, key)
	got := getWorkspace(t, c, key)
	if got.Status.Phase != cdiv1.WorkspacePhaseFailed || degradedReasonOf(got) != cdiv1.FailureHomeVolumeLost {
		t.Fatalf("phase=%q reason=%q, want Failed/HomeVolumeLost at once", got.Status.Phase, degradedReasonOf(got))
	}
	now = now.Add(FailedCleanupDelay + time.Minute)
	reconcile(t, r, key)
	if got := getWorkspace(t, c, key); degradedReasonOf(got) != cdiv1.FailureHomeVolumeLost {
		t.Fatalf("cleanup overwrote the unrecoverable reason with %q", degradedReasonOf(got))
	}
	if err := c.List(context.Background(), pvcs); err != nil {
		t.Fatal(err)
	}
	for _, p := range pvcs.Items {
		if p.Namespace == ns && p.DeletionTimestamp == nil {
			t.Fatalf("a fresh empty home %s was created", p.Name)
		}
	}
}

func TestRecovery_ImagePolicyDeniedOnlyFromPolicyPullMessages(t *testing.T) {
	env, c := startEnv(t)
	defer func() { _ = env.Stop() }()
	for _, tc := range []struct {
		name, msg string
		denied    bool
	}{
		{"policy", "rpc error: failed to pull: 403 Forbidden: image rejected by policy", true},
		{"missing credentials stays a retried pull", "pull access denied, repository does not exist or may require authorization", false},
		{"network error stays a retried pull", "dial tcp 10.0.0.1:443: connection refused", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := newNamespace(t, c)
			newTemplate(t, c, ns, "tpl-ip", nil)
			ws := newWorkspace(t, c, ns, "ws-ip", "tpl-ip", nil)
			key := types.NamespacedName{Name: ws.Name, Namespace: ns}
			r := newReconciler(c)
			reconcile(t, r, key)
			pod := thePod(t, c, ns, ws.UID)
			pod.Status.Phase = corev1.PodPending
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "desktop",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "ErrImagePull", Message: tc.msg}},
			}}
			if err := c.Status().Update(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
			reconcile(t, r, key) // well before the 5 minute boot deadline
			got := getWorkspace(t, c, key)
			if tc.denied {
				if got.Status.Phase != cdiv1.WorkspacePhaseFailed || degradedReasonOf(got) != cdiv1.FailureImagePolicyDenied {
					t.Fatalf("phase=%q reason=%q, want Failed/ImagePolicyDenied", got.Status.Phase, degradedReasonOf(got))
				}
				return
			}
			if got.Status.Phase != cdiv1.WorkspacePhaseProvisioning || degradedReasonOf(got) != "" {
				t.Fatalf("phase=%q reason=%q, want Provisioning without Degraded", got.Status.Phase, degradedReasonOf(got))
			}
		})
	}
}

func TestAdmissionImageDenied(t *testing.T) {
	forbidden := func(msg string) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "ws-x", errors.New(msg))
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"image-policy webhook", forbidden(`admission webhook "policy.sigstore.dev" denied the request: image ghcr.io/x@sha256:00 has no matching signatures`), true},
		{"validating admission policy", forbidden(`ValidatingAdmissionPolicy 'images' with binding 'images' denied request: image must come from the internal registry`), true},
		{"quota", forbidden(`exceeded quota: tenant, requested: pods=1, used: pods=10, limited: pods=10`), false},
		{"pod security", forbidden(`violates PodSecurity "restricted:latest": allowPrivilegeEscalation != false`), false},
		{"not a forbidden error", errors.New(`admission webhook "x" denied the request: image`), false},
	}
	for _, tc := range cases {
		if got := admissionImageDenied(tc.err); got != tc.want {
			t.Errorf("%s: admissionImageDenied = %v, want %v", tc.name, got, tc.want)
		}
	}
	if !imagePolicyDenied(tcdiruntime.Observation{Reason: "ImagePullBackOff", Message: "rejected by policy"}) ||
		imagePolicyDenied(tcdiruntime.Observation{Reason: "Provisioning", Message: "rejected by policy"}) {
		t.Error("imagePolicyDenied must key on a pull-failure reason AND a policy message")
	}
}
```

Run the new tests. `startEnv` falls back to `bin/k8s/1.37.0-linux-amd64` when `KUBEBUILDER_ASSETS` is unset, so after `make setup-envtest` this needs no variable; otherwise `export KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path)"` first:

```bash
make setup-envtest && go test ./internal/operator/ -run 'TestRecovery_|TestAdmissionImageDenied' -count=1
```

Expected: FAIL to compile (`degradedReasonOf`, `admissionImageDenied`, `imagePolicyDenied` do not exist yet). After Step 4 compiles, the pre-change behaviour would have failed the assertions: the deleted pod latches `BootDeadlineExceeded` or is rebuilt.

- [ ] **Step 4: Implement the operator changes**

In `reconcileRunning`, directly after the `Failed && LastAppliedIntentRevision == applied.Revision` guard, add:

```go
	// --- runtime lost (F1) ---------------------------------------------------
	// A Ready incarnation of the applied generation whose pod is gone,
	// terminating or failed is not rebuilt inside the same generation: it
	// goes Failed/RuntimeLost and waits for :start, which mints a new
	// generation and revokes the old one's leases and tickets.
	if ws.Status.Phase == cdiv1.WorkspacePhaseReady && ws.Status.RuntimeUID != "" &&
		ws.Status.ObservedRuntimeGeneration == applied.RuntimeGeneration {
		obs, err := r.Backend.Observe(ctx, ws)
		if err != nil {
			return ctrl.Result{}, err
		}
		if runtimeLost(ws, obs) {
			logf.FromContext(ctx).Info("runtime lost; waiting for an explicit start",
				"runtimeUID", ws.Status.RuntimeUID, "observed", obs.RuntimeUID, "reason", obs.Reason)
			return ctrl.Result{}, r.writeStatus(ctx, ws, applied, obs, cdiv1.WorkspacePhaseFailed,
				errors.New(cdiv1.FailureRuntimeLost), nil)
		}
	}
```

and the helper:

```go
// runtimeLost reports whether the observed runtime is no longer the Ready
// incarnation status recorded.
func runtimeLost(ws *cdiv1.Workspace, obs tcdiruntime.Observation) bool {
	switch {
	case obs.RuntimeUID == "", obs.RuntimeUID != ws.Status.RuntimeUID:
		return true
	case obs.Reason == "Terminating", obs.Reason == "PodFailed", obs.Reason == "PodExited":
		return true
	}
	return false
}

// imageDeniedPattern matches a node-level image policy refusal (for example
// CRI-O's "Running image ... is rejected by policy"). A bare "denied" or
// "unauthorized" is deliberately NOT enough: "pull access denied, repository
// does not exist or may require authorization" is a typo or a missing pull
// secret, which the normal boot deadline reports and an administrator fixes
// in place.
var imageDeniedPattern = regexp.MustCompile(`(?i)\bpolicy\b`)

// imagePolicyDenied classifies a pull failure the node's image policy
// refused (as opposed to a registry or network error, which is retried).
func imagePolicyDenied(obs tcdiruntime.Observation) bool {
	return (obs.Reason == "ErrImagePull" || obs.Reason == "ImagePullBackOff") && imageDeniedPattern.MatchString(obs.Message)
}

// admissionImageDenied classifies a pod-create refusal by an admission
// webhook or ValidatingAdmissionPolicy that names the image (an image
// signature verifier, a registry allowlist). Quota and PodSecurity refusals
// are different conditions and keep their retry path.
func admissionImageDenied(err error) bool {
	if !apierrors.IsForbidden(err) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "image") &&
		(strings.Contains(msg, "admission webhook") || strings.Contains(msg, "validatingadmissionpolicy"))
}
```

In the `Ensure` error switch, add these two cases before `default`:

```go
	case errors.Is(berr, linux.ErrHomeVolumeLost):
		statusErr = errors.New(cdiv1.FailureHomeVolumeLost)
		obs, _ = r.Backend.Observe(ctx, ws)
	case admissionImageDenied(berr):
		logf.FromContext(ctx).Info("runtime image refused by admission policy", "error", berr)
		statusErr = errors.New(cdiv1.FailureImagePolicyDenied)
		obs, _ = r.Backend.Observe(ctx, ws)
```

After the `switch` on `berr`, and before the phase block, add:

```go
	if statusErr == nil && imagePolicyDenied(obs) {
		statusErr = errors.New(cdiv1.FailureImagePolicyDenied)
	}
```

Replace the phase `switch` with:

```go
	switch {
	case statusErr != nil && cdiv1.FailureUnrecoverable(statusErr.Error()):
		phase = cdiv1.WorkspacePhaseFailed
	case obs.StorageReady && obs.RuntimeReady && obs.ConnectionReady:
		phase = cdiv1.WorkspacePhaseReady
	case !r.now().Before(deadline):
		phase = cdiv1.WorkspacePhaseFailed
		statusErr = errors.New(ReasonBootDeadlineExceeded)
	}
```

Leave `anchor, err := r.bootAnchor(...)` where it is: it only persists the incarnation-start annotation, which is harmless for an unrecoverable failure.

Add `"strings"` to the import block of `workspace_controller.go` (`regexp`, `errors`, `apierrors`, `metav1` and `linux` are already imported).

In `reconcileFailed`, replace `statusErr := errors.New(ReasonBootDeadlineExceeded)` and the cleanup branch with:

```go
	reason := degradedReasonOf(ws)
	if reason == "" {
		reason = ReasonBootDeadlineExceeded
	}
	res := ctrl.Result{}
	if remaining := r.failedCleanupRemaining(ws); remaining > 0 {
		res.RequeueAfter = remaining
	} else {
		if err := r.Backend.DeleteRuntime(ctx, ws); err != nil {
			logf.FromContext(ctx).Error(err, "failed-incarnation cleanup; will retry")
			return ctrl.Result{}, err
		}
		// An unrecoverable reason is the operator-facing truth and must
		// survive cleanup; a recoverable one reports the teardown.
		if !cdiv1.FailureUnrecoverable(reason) {
			reason = ReasonFailedCleanup
		}
	}
	return res, r.writeStatus(ctx, ws, applied, obs, cdiv1.WorkspacePhaseFailed, errors.New(reason), nil)
```

with:

```go
// degradedReasonOf returns the reason of a True Degraded condition, or "".
func degradedReasonOf(ws *cdiv1.Workspace) string {
	for _, c := range ws.Status.Conditions {
		if c.Type == cdiv1.ConditionDegraded && c.Status == metav1.ConditionTrue {
			return c.Reason
		}
	}
	return ""
}
```

`writeStatus` already maps a `BootDeadlineExceeded` statusErr onto `RuntimeReady`. Leave that. The other reasons only set `Degraded`, which is what `failureReasonOf` reads.

Run the Step 3 command. Expected: PASS.

One existing test encodes the behaviour ruling 8 replaces. In `internal/operator/workspace_controller_test.go`, the subtest `t.Run("pod replacement keeps generation and changes runtimeUID", ...)` (~515-550) deletes a Ready pod and expects an in-generation rebuild. Replace the whole subtest with the new contract; the template-deletion half is kept, because the recorded snapshot must still drive the rebuild:

```go
	t.Run("a lost pod is RuntimeLost; the next generation rebuilds from the recorded snapshot", func(t *testing.T) {
		ns := newNamespace(t, c)
		newTemplate(t, c, ns, "tpl-repl", nil)
		ws := newWorkspace(t, c, ns, "ws-repl", "tpl-repl", nil)
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}
		r := newReconciler(c)

		reconcile(t, r, key)
		pod1 := thePod(t, c, ns, ws.UID)
		markPodReady(t, c, pod1)
		reconcile(t, r, key)
		wsNow := getWorkspace(t, c, key)
		uid1 := wsNow.Status.RuntimeUID
		if uid1 == "" || uid1 != string(pod1.UID) {
			t.Fatalf("runtimeUID=%q want %q", uid1, pod1.UID)
		}

		// Template snapshot is immutable for the running generation: deleting
		// the template must not stop convergence.
		if err := c.Delete(context.Background(), &cdiv1.WorkspaceTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "tpl-repl", Namespace: ns},
		}); err != nil {
			t.Fatalf("delete template: %v", err)
		}

		if err := c.Delete(context.Background(), pod1); err != nil {
			t.Fatalf("delete pod: %v", err)
		}
		reconcile(t, r, key)
		wsNow = getWorkspace(t, c, key)
		if wsNow.Status.Phase != cdiv1.WorkspacePhaseFailed || degradedReasonOf(wsNow) != cdiv1.FailureRuntimeLost {
			t.Fatalf("phase=%q reason=%q, want Failed/RuntimeLost", wsNow.Status.Phase, degradedReasonOf(wsNow))
		}
		if pods := listOwned(t, c, ns, ws.UID, &corev1.PodList{}, "pods"); pods != 0 {
			t.Fatalf("a lost pod was rebuilt inside its generation (%d pods)", pods)
		}

		// What the applier writes for RestartWorkspace's start intent.
		wsNow.Spec.RuntimeGeneration = 2
		wsNow.Spec.IntentRevision = 2
		if err := c.Update(context.Background(), wsNow); err != nil {
			t.Fatalf("new generation: %v", err)
		}
		reconcile(t, r, key)
		pod2 := thePod(t, c, ns, ws.UID)
		if pod2.UID == pod1.UID {
			t.Fatalf("pod was not actually recreated")
		}
		markPodReady(t, c, pod2)
		reconcile(t, r, key)
		wsNow = getWorkspace(t, c, key)
		if wsNow.Status.RuntimeUID == uid1 || wsNow.Status.RuntimeUID != string(pod2.UID) {
			t.Fatalf("runtimeUID=%q want new %q", wsNow.Status.RuntimeUID, pod2.UID)
		}
		if wsNow.Status.ObservedRuntimeGeneration != 2 {
			t.Fatalf("observedRuntimeGeneration=%d, want 2", wsNow.Status.ObservedRuntimeGeneration)
		}
	})
```

(`TestSnapshot_UnchangedWithinGeneration` in `resnapshot_test.go` is unaffected: its pod is never marked Ready, so the lost-runtime rule does not apply.)

Then run the whole operator suite: `go test ./internal/operator/ -count=1`. Expected: PASS. In particular `TestLifecycle_ExplicitRetryAfterFailed`, `TestLifecycle_FailedCleanupDeadline`, `TestProgress_FailedKeepsStalledStep` and `TestTemplateRejectedAppArmor` stay green (the last one now reaches `Failed` instead of staying `Provisioning`; its assertions only read the Degraded reason and the pod count). If any other test deletes a Ready pod and expects a rebuild, it encodes the old contract: change it the same way.

In `docs/lifecycle-reasons.md`, replace the body of `## Failed` with:

```markdown
Once `phase` is `Failed` the three step conditions are frozen: the reason on
`ConnectionReady` is the step that stalled (for example `ImagePullBackOff`),
and it stays after the five-minute cleanup of the failed incarnation.
`Degraded` carries the failure reason:

| Reason | Meaning | Start |
|---|---|---|
| `BootDeadlineExceeded` | The desktop did not become ready within the template's boot deadline. `RuntimeReady` reads the same reason. | Recovers: a new runtime generation on the same home. |
| `FailedCleanup` | The failed desktop was removed after five minutes. | Recovers. |
| `RuntimeLost` | A running desktop's pod was deleted, evicted or failed. It is not rebuilt automatically. | Recovers. Old sessions are revoked. |
| `ImagePolicyDenied` | A node image policy or an admission policy refused the image. A registry error (typo, missing pull secret) is not this reason; it ends as `BootDeadlineExceeded`. | Refused (409 `INVALID_STATE`, `details.reason`). |
| `HomeVolumeLost` | A Retain workspace's home disk is gone after it had been used. The platform never starts it on an empty disk. | Refused. |
| `TemplateRejected` | The runtime refused the template's configuration. | Refused. |

Recovery mints a new runtime generation: the pod is rebuilt on the same home
volume, and leases and launch tickets of the failed generation are revoked
in the same transaction. A refused reason stays until the cause is fixed;
then stop and start the workspace (a start after a stop always begins a
fresh generation), or delete it.
```

Commit this task:

```bash
go build ./... && go vet ./... && go test -count=1 ./api/... ./internal/runtime/... ./internal/operator/
git add api/v1/failure.go api/v1/failure_test.go internal/operator internal/runtime docs/lifecycle-reasons.md
git commit -m "feat(operator): failure reasons, RuntimeLost and unrecoverable failures

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6a: Recover a `Failed` workspace with `:start` (F1) — tier: design

The control-plane half of F1. It consumes Task 6's `cdiv1.FailureRecoverable` / `FailureUnrecoverable` and the operator behaviour (a `Failed/RuntimeLost` CR).

**Files:**
- Modify: `internal/broker/activity.go:303-353` (extract `RevokeGenerationTx`; the function is at 320-353), `internal/broker/activity_e2e_test.go`
- Modify: `internal/provisioning/service.go` (new `RestartWorkspace`, `WithGenerationRevoker`), create `internal/provisioning/restart_test.go`
- Modify: `internal/api/statusview.go` (`ObservedStatus.RuntimeGeneration`, `IntentSettled`), `internal/api/workspaces.go` (imports, `workspaceBackend`, `signal`, `writeBackendError`), `internal/api/workspaces_handlers_test.go` (the fake `workspaceBackend`), create `internal/api/recovery_test.go` and `internal/api/statusview_settled_test.go`, `internal/api/openapi.yaml` (`:start` description, `ErrorBody.details.reason` doc), regenerate `web/src/api/generated/schema.d.ts`
- Modify: `internal/backend/wire.go` (wire the revoker after `broker.New`, ~line 408)
- Modify: `web/src/workspaces/{reasons.tsx,WorkspaceDetailPage.tsx}`, `web/src/admin/WorkspacesAdminPage.tsx` (`canStart`), `web/src/i18n/{en,vi}/workspaces.ts`, `web/tests/workspaces.spec.ts`, `web/tests/unit/workspaces/{reasons,detail}.test.tsx`, `web/tests/unit/admin/workspaces.test.tsx`
- Modify: `docs/lifecycle-reasons.md` (API refusal reasons table), `docs/security/threat-model.md` (row S46)
- Modify: `hack/quickstart/smoke/smoke.spec.ts` (recovery e2e)

**Interfaces:**
- Consumes: Task 6 (`cdiv1.FailureRecoverable`, `cdiv1.FailureUnrecoverable`), `SignalWorkspace` internals (`getWorkspaceTx`, `startSessionPolicy`, `CheckImageBlock`, `appendIntent`, `BeginIdempotent`/`CompleteIdempotent`, `scopedIdemKey`), `Broker.checkRevokeBound`, `K8sStatusView.WorkspaceStatus`, `failureReasonOf`.
- Produces (Task 8 consumes `RestartWorkspace` and `ObservedStatus.RuntimeGeneration`):
  - `func (b *broker.Broker) RevokeGenerationTx(ctx context.Context, tx store.Tx, workspaceUID broker.PlatformID, runtimeGeneration uint64, now time.Time) (int, error)`
  - `type provisioning.GenerationRevoker func(ctx context.Context, tx store.Tx, workspaceID string, generation int64) error`, `func (s *Service) WithGenerationRevoker(r GenerationRevoker) *Service`
  - `func (s *provisioning.Service) RestartWorkspace(ctx context.Context, tenantID, caller, ownerScope, wsID, idemKey string, fromGeneration int64, bodyHash []byte) (provisioning.WorkspaceRecord, error)`
  - `var provisioning.ErrNotRunning = errors.New("workspace is not running")`
  - `api.ObservedStatus.RuntimeGeneration int64` (the CR's `spec.runtimeGeneration`) and `api.ObservedStatus.IntentSettled bool` (the operator has applied the CR's latest `spec.intentRevision`, so `Phase`/`FailureReason` describe the current generation)
  - `:start` on a `Failed` workspace with an unrecoverable reason returns 409 `INVALID_STATE` with `details.reason` = the failure reason.

- [ ] **Step 1: Broker: revoke a generation inside a caller's transaction**

Refactor `RevokeWorkspaceLeases` (`activity.go:320-353`; the doc comment starts at 303) into:

```go
// RevokeGenerationTx is RevokeWorkspaceLeases inside the caller's
// transaction: it revokes leases of generations <= runtimeGeneration,
// records the workspace_revocation fence (which rejects tickets and lease
// renewals bound to those generations) and zeroes their open-stream
// counters. RestartWorkspace calls it in the same transaction that mints
// the next generation (F1).
func (b *Broker) RevokeGenerationTx(ctx context.Context, tx store.Tx, workspaceUID PlatformID, runtimeGeneration uint64, now time.Time) (int, error) {
	if err := b.checkRevokeBound(ctx, tx, workspaceUID, runtimeGeneration); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE connection_lease SET state = 'revoked', closed_at = $3
		WHERE workspace_id = $1 AND runtime_generation <= $2 AND state = 'active'`,
		workspaceUID, int64(runtimeGeneration), now)
	if err != nil {
		return 0, fmt.Errorf("broker: revoke workspace leases: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workspace_revocation (workspace_id, runtime_generation, revoked_at)
		SELECT $1, $2, $3 WHERE EXISTS (SELECT 1 FROM workspaces WHERE id = $1)
		ON CONFLICT DO NOTHING`, workspaceUID, int64(runtimeGeneration), now); err != nil {
		return 0, fmt.Errorf("broker: record workspace revocation: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workspace_activity SET
			open_streams = 0,
			disconnected_since = COALESCE(disconnected_since, $3),
			updated_at = $3
		WHERE workspace_id = $1 AND runtime_generation <= $2 AND open_streams > 0`,
		workspaceUID, int64(runtimeGeneration), now); err != nil {
		return 0, fmt.Errorf("broker: close revoked streams: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func (b *Broker) RevokeWorkspaceLeases(ctx context.Context, workspaceUID PlatformID, runtimeGeneration uint64) (int, error) {
	now := b.now()
	var n int
	err := b.db.WithTx(ctx, func(tx store.Tx) error {
		var err error
		n, err = b.RevokeGenerationTx(ctx, tx, workspaceUID, runtimeGeneration, now)
		return err
	})
	return n, err
}
```

Keep the existing doc comment of `RevokeWorkspaceLeases` on it. Add this pin to `internal/broker/activity_e2e_test.go` (same package `broker_test`; helpers `setup`, `seedWorkspace`, `markRunning`, `readyBinding`, `seedLease`, `alice`, `gwA`, `ctx` already exist there or in `main_test.go`/`activity_test.go`). The end-to-end pin (service restart plus this broker revoker against real Postgres) is added in Step 3, where `WithGenerationRevoker` exists.

```go
// TestRevokeGenerationTx_RollsBackWithCallersTx: the revocation lives and dies
// with the caller's transaction, so a failed restart leaves the old
// generation connected.
func TestRevokeGenerationTx_RollsBackWithCallersTx(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	markRunning(t, db, "ws-1", 3)
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 3, "rt-3", clock.Now()))
	seedLease(t, db, "lease-g3", "ws-1", "tenant-a", alice.Owner(),
		3, "rt-3", 1, gwA.ID, clock.Now().Add(broker.LeaseTTL))

	boom := errors.New("caller failed after revoking")
	err := db.WithTx(ctx, func(tx store.Tx) error {
		if n, err := b.RevokeGenerationTx(ctx, tx, "ws-1", 3, clock.Now()); err != nil || n != 1 {
			t.Fatalf("revoke in tx = %d, %v; want 1, nil", n, err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithTx = %v, want the caller's error", err)
	}
	var state string
	var fences int
	if err := db.Pool().QueryRow(ctx, `SELECT state FROM connection_lease WHERE id='lease-g3'`).Scan(&state); err != nil || state != "active" {
		t.Fatalf("lease state = %q (err %v), want active after rollback", state, err)
	}
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM workspace_revocation WHERE workspace_id='ws-1'`).Scan(&fences); err != nil || fences != 0 {
		t.Fatalf("workspace_revocation rows = %d (err %v), want 0 after rollback", fences, err)
	}
}
```

Run `go test ./internal/broker/ -count=1` (needs Docker for Postgres; a skip is not a pass). Expected: PASS (existing revoke tests unchanged, the new test green).

- [ ] **Step 2: Failing provisioning tests for `RestartWorkspace`**

`internal/provisioning/restart_test.go`:

```go
package provisioning_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

type revokeCall struct {
	ws  string
	gen int64
}

func runningWorkspace(t *testing.T, db *store.DB, svc *provisioning.Service, tenant, name string) provisioning.WorkspaceRecord {
	t.Helper()
	ctx := context.Background()
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.SetQuota(ctx, tx, tenant, provisioning.ResourceVector{
			RunningSlots: 4, CPUMillis: 8000, MemoryBytes: 16 << 30, DiskBytes: 40 << 30})
	}); err != nil {
		t.Fatal(err)
	}
	req := provisioning.CreateRequest{
		OwnerIssuer: "iss", OwnerSubject: "sub", Name: name,
		Template: provisioning.TemplateInfo{ID: "tpl_x", Name: "x", Runtime: "linux", Experience: "desktop"},
		Vector:   quotaVec, DesiredState: "Running", DataPolicy: "Retain",
	}
	rec, err := svc.CreateWorkspace(ctx, tenant, "key-"+name, req, provisioning.RequestHash(req))
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func generationOf(t *testing.T, db *store.DB, ws string) int64 {
	t.Helper()
	var g int64
	if err := db.Pool().QueryRow(context.Background(), `SELECT runtime_generation FROM workspaces WHERE id = $1`, ws).Scan(&g); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestRestartWorkspace_BumpsGenerationRevokesOldAndAppendsStart(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	var calls []revokeCall
	svc := provisioning.NewService(db).WithGenerationRevoker(func(_ context.Context, _ store.Tx, ws string, gen int64) error {
		calls = append(calls, revokeCall{ws, gen})
		return nil
	})
	rec := runningWorkspace(t, db, svc, "tenant-r", "r1")
	g0 := generationOf(t, db, rec.ID)
	out, err := svc.RestartWorkspace(ctx, "tenant-r", "iss|sub", "iss|sub", rec.ID, "key-restart-1", g0, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if out.DesiredState != "Running" || generationOf(t, db, rec.ID) != g0+1 {
		t.Fatalf("record %+v gen %d, want Running and %d", out, generationOf(t, db, rec.ID), g0+1)
	}
	if len(calls) != 1 || calls[0] != (revokeCall{rec.ID, g0}) {
		t.Fatalf("revocations = %+v, want the old generation %d", calls, g0)
	}
	var start *provisioning.Intent
	pending := mustPending(t, db, rec.ID)
	for i := range pending {
		if pending[i].Kind == provisioning.IntentStart {
			start = &pending[i]
		}
	}
	if start == nil || start.RuntimeGeneration != g0+1 || start.DesiredState != "Running" {
		t.Fatalf("start intent = %+v", start)
	}
}

func TestRestartWorkspace_KeyReplaysAcrossRestartAndPlainStart(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	revokes := 0
	svc := provisioning.NewService(db).WithGenerationRevoker(func(context.Context, store.Tx, string, int64) error {
		revokes++
		return nil
	})
	rec := runningWorkspace(t, db, svc, "tenant-k", "k1")
	g0 := generationOf(t, db, rec.ID)
	if _, err := svc.RestartWorkspace(ctx, "tenant-k", "iss|sub", "iss|sub", rec.ID, "key-replay-01", g0, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	// The same request retried (a lost response). The CR has left Failed by
	// now, so the API routes it to the plain start path: the stored result
	// must come back, not an IDEMPOTENCY_CONFLICT, and nothing bumps again.
	again, err := svc.SignalWorkspace(ctx, "tenant-k", "iss|sub", "iss|sub", rec.ID, "key-replay-01", provisioning.IntentStart, []byte("{}"))
	if err != nil || !again.Replayed {
		t.Fatalf("plain start replay = %+v, %v; want the recorded result", again, err)
	}
	again, err = svc.RestartWorkspace(ctx, "tenant-k", "iss|sub", "iss|sub", rec.ID, "key-replay-01", g0, []byte("{}"))
	if err != nil || !again.Replayed {
		t.Fatalf("restart replay = %+v, %v; want the recorded result", again, err)
	}
	if g := generationOf(t, db, rec.ID); g != g0+1 || revokes != 1 {
		t.Fatalf("generation %d revokes %d after replays, want %d and 1", g, revokes, g0+1)
	}
}

func TestRestartWorkspace_GenerationFenced(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	var mu sync.Mutex
	revokes := 0
	svc := provisioning.NewService(db).WithGenerationRevoker(func(context.Context, store.Tx, string, int64) error {
		mu.Lock()
		revokes++
		mu.Unlock()
		return nil
	})
	rec := runningWorkspace(t, db, svc, "tenant-f", "f1")
	g0 := generationOf(t, db, rec.ID)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ { // a double click: two different idempotency keys, same observed generation
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.RestartWorkspace(ctx, "tenant-f", "iss|sub", "iss|sub", rec.ID, "key-dbl-"+string(rune('a'+i)), g0, []byte("{}"))
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("restart: %v", err)
		}
	}
	if g := generationOf(t, db, rec.ID); g != g0+1 || revokes != 1 {
		t.Fatalf("generation %d revokes %d, want exactly one bump to %d", g, revokes, g0+1)
	}
}

func TestRestartWorkspace_RefusesStoppedAndForeignAndRollsBackOnRevokeError(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	failing := errors.New("revoke failed")
	fail := false
	svc := provisioning.NewService(db).WithGenerationRevoker(func(context.Context, store.Tx, string, int64) error {
		if fail {
			return failing
		}
		return nil
	})
	rec := runningWorkspace(t, db, svc, "tenant-s", "s1")
	g0 := generationOf(t, db, rec.ID)
	if _, err := svc.RestartWorkspace(ctx, "tenant-s", "iss|eve", "iss|eve", rec.ID, "key-foreign", g0, []byte("{}")); !errors.Is(err, provisioning.ErrWorkspaceNotFound) {
		t.Fatalf("foreign caller: %v, want ErrWorkspaceNotFound", err)
	}
	fail = true
	if _, err := svc.RestartWorkspace(ctx, "tenant-s", "iss|sub", "iss|sub", rec.ID, "key-rb", g0, []byte("{}")); !errors.Is(err, failing) {
		t.Fatalf("revoke error: %v", err)
	}
	if generationOf(t, db, rec.ID) != g0 {
		t.Fatal("a failed revoke must roll back the generation bump")
	}
	fail = false
	if _, err := svc.SignalWorkspace(ctx, "tenant-s", "iss|sub", "", rec.ID, "key-s-stop", provisioning.IntentStop, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RestartWorkspace(ctx, "tenant-s", "iss|sub", "iss|sub", rec.ID, "key-s-r", generationOf(t, db, rec.ID), []byte("{}")); !errors.Is(err, provisioning.ErrNotRunning) {
		t.Fatalf("stopped: %v, want ErrNotRunning", err)
	}
}
```

Run `go test ./internal/provisioning/ -run TestRestartWorkspace -count=1` (needs Docker for Postgres; a skip is not a pass). Expected: FAIL to compile (`WithGenerationRevoker`, `RestartWorkspace`, `ErrNotRunning` do not exist yet).

- [ ] **Step 3: Implement `RestartWorkspace`**

In `service.go`, add a `revokeGeneration GenerationRevoker` field to `Service`, plus:

```go
// GenerationRevoker fences a runtime generation inside the caller's
// transaction (broker.RevokeGenerationTx in production).
type GenerationRevoker func(ctx context.Context, tx store.Tx, workspaceID string, generation int64) error

// ErrNotRunning refuses a restart of a workspace whose desired state is
// not Running (a stopped workspace uses a plain start).
var ErrNotRunning = errors.New("workspace is not running")

// WithGenerationRevoker installs the revoker RestartWorkspace calls.
func (s *Service) WithGenerationRevoker(r GenerationRevoker) *Service {
	s.revokeGeneration = r
	return s
}

// RestartWorkspace mints a new runtime generation for a workspace that is
// wanted Running but whose current incarnation is lost or failed (F1), or
// whose session policy must be re-applied (F3). In one transaction it:
// bumps runtime_generation, revokes the previous generation's leases and
// tickets, re-stamps the resolved session policy, and appends a start
// intent; the applier moves spec.runtimeGeneration and the operator
// replaces the pod on the same home volume.
//
// fromGeneration is the generation the caller observed. When the row has
// already moved past it (a concurrent restart won), the call is a no-op
// returning the current record — a double-click bumps once. The running
// quota reservation is kept, because the workspace never stopped holding it.
// The template is not re-pointed, because a restart is not an upgrade.
func (s *Service) RestartWorkspace(ctx context.Context, tenantID, caller, ownerScope, wsID, idemKey string, fromGeneration int64, bodyHash []byte) (res WorkspaceRecord, err error) {
	if s.revokeGeneration == nil {
		return res, errors.New("restart: no generation revoker configured")
	}
	err = s.db.WithTx(ctx, func(tx store.Tx) error {
		rec, err := getWorkspaceTx(ctx, tx, tenantID, ownerScope, wsID, "FOR UPDATE")
		if err != nil {
			return err
		}
		scopedKey := ""
		if idemKey != "" {
			scopedKey = scopedIdemKey(caller, idemKey)
			idem, replayed, err := BeginIdempotent(ctx, tx, tenantID, scopedKey, string(IntentStart)+":"+wsID, bodyHash)
			if err != nil {
				return err
			}
			if replayed {
				if err := json.Unmarshal(idem.Result, &res); err != nil {
					return fmt.Errorf("restart: stored result %w", err)
				}
				res.Replayed = true
				return nil
			}
		}
		if rec.isDeleted() {
			return ErrWorkspaceClosed
		}
		if rec.DesiredState != "Running" {
			return ErrNotRunning
		}
		var gen int64
		if err := tx.QueryRow(ctx, `SELECT runtime_generation FROM workspaces WHERE id = $1`, wsID).Scan(&gen); err != nil {
			return fmt.Errorf("restart: read generation %w", err)
		}
		if gen == fromGeneration {
			if err := CheckImageBlock(rec.Template.ImageBuiltAt, s.blockAfter, time.Now(), rec.Template.Name, true); err != nil {
				return err
			}
			sp, err := s.startSessionPolicy(ctx, tx, tenantID, rec)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE workspaces SET runtime_generation = runtime_generation + 1,
					phase = 'Provisioning', updated_at = now()
				WHERE id = $1 AND tenant_id = $2`, wsID, tenantID); err != nil {
				return fmt.Errorf("restart: bump generation %w", err)
			}
			if err := s.revokeGeneration(ctx, tx, wsID, gen); err != nil {
				return err
			}
			if _, err := appendIntent(ctx, tx, PlatformID(wsID), IntentStart, &IntentSpec{SessionPolicy: sp}); err != nil {
				return err
			}
		}
		out, err := getWorkspaceTx(ctx, tx, tenantID, "", wsID)
		if err != nil {
			return err
		}
		res = *out
		if idemKey != "" {
			return CompleteIdempotent(ctx, tx, tenantID, scopedKey, res)
		}
		return nil
	})
	return res, err
}
```

Notes, all verified against the code:
- `s.blockAfter` is the real field name. A restart does not re-point the template (`startTemplateTarget` is not called), so the image-age check passes `pinned=true`: a Failed workspace on an image older than `-image-block-after` is refused with `IMAGE_STALE`, like any other start.
- The idempotency operation is `"start:"+wsID`, the same string `SignalWorkspace` records. The API sends a retried recovery request down whichever path the CR state selects at that moment; a different operation string would turn the retry into `IDEMPOTENCY_CONFLICT` instead of replaying the stored result (pinned by `TestRestartWorkspace_KeyReplaysAcrossRestartAndPlainStart`).
- A start intent whose `IntentSpec` carries only `SessionPolicy` leaves `spec.templateRef` alone: `k8sapplier.applySignal` touches the template only when `Spec.TemplateName != ""`, and it takes `spec.runtimeGeneration` from the intent's generation (`in.RuntimeGeneration > ws.Spec.RuntimeGeneration`). The CRD allows the generation to rise while Running.
- The fence is atomic in SQL: `getWorkspaceTx(... "FOR UPDATE")` locks the row, the generation is read under that lock, and the bump, the revocation and the intent commit together. A second caller blocks on the lock, then (READ COMMITTED) reads the bumped generation and becomes a no-op. A failed revoke rolls the bump back.

Add the end-to-end pin to `internal/broker/activity_e2e_test.go` (it needs both packages, and `broker_test` already imports `provisioning`):

```go
// TestRestartWorkspace_FencesOldGenerationEndToEnd: provisioning's
// RestartWorkspace with the broker as revoker kills the previous generation's
// lease and outstanding ticket in its own transaction (F1), while the new
// generation connects normally.
func TestRestartWorkspace_FencesOldGenerationEndToEnd(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	markRunning(t, db, "ws-1", 3)
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 3, "rt-3", clock.Now()))
	seedLease(t, db, "lease-g3", "ws-1", "tenant-a", alice.Owner(),
		3, "rt-3", 1, gwA.ID, clock.Now().Add(broker.LeaseTTL))
	tk, err := b.IssueTicket(ctx, alice, "ws-1", true, provisioning.SessionPolicy{}, testPortalSession)
	if err != nil {
		t.Fatalf("issue under generation 3: %v", err)
	}

	svc := provisioning.NewService(db).WithGenerationRevoker(
		func(ctx context.Context, tx store.Tx, ws string, gen int64) error {
			_, err := b.RevokeGenerationTx(ctx, tx, broker.PlatformID(ws), uint64(gen), clock.Now())
			return err
		})
	if _, err := svc.RestartWorkspace(ctx, "tenant-a", alice.Owner(), alice.Owner(), "ws-1", "key-restart-1", 3, []byte("{}")); err != nil {
		t.Fatalf("restart: %v", err)
	}

	var state string
	if err := db.Pool().QueryRow(ctx, `SELECT state FROM connection_lease WHERE id='lease-g3'`).Scan(&state); err != nil || state != "revoked" {
		t.Fatalf("old lease state = %q (err %v), want revoked", state, err)
	}
	fence := broker.Fence{WorkspaceUID: "ws-1", RuntimeGeneration: 3, RuntimeUID: "rt-3", FencingVersion: 1}
	if _, err := b.RenewLease(ctx, gwA, "lease-g3", fence); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("renew of the old lease = %v, want ErrRevoked", err)
	}
	if _, err := b.RedeemTicket(ctx, gwA, tk.Token); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("redeem of the old ticket = %v, want ErrRevoked", err)
	}
	if _, err := b.IssueTicket(ctx, alice, "ws-1", false, provisioning.SessionPolicy{}, testPortalSession); !errors.Is(err, broker.ErrDenied) {
		t.Fatalf("issue for the old generation = %v, want ErrDenied", err)
	}

	// The restarted generation (the DB row now says 4) connects.
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 4, "rt-4", clock.Now()))
	tk4, err := b.IssueTicket(ctx, alice, "ws-1", false, provisioning.SessionPolicy{}, testPortalSession)
	if err != nil {
		t.Fatalf("issue for generation 4: %v", err)
	}
	lease, err := b.RedeemTicket(ctx, gwA, tk4.Token)
	if err != nil || lease.RuntimeGeneration != 4 {
		t.Fatalf("redeem for generation 4 = %+v, %v", lease, err)
	}
}

```

Run the Step 2 command, `go test ./internal/provisioning/ -count=1` and `go test ./internal/broker/ -count=1`. Expected: PASS. The fenced test proves the `FOR UPDATE` row lock serializes the two calls, so the second one reads `gen != fromGeneration`.

- [ ] **Step 4: API: Start on `Failed` recovers or refuses**

In `statusview.go`, add two fields to `ObservedStatus`:

```go
	// RuntimeGeneration is the CR's spec.runtimeGeneration — the generation
	// a restart is fenced on.
	RuntimeGeneration int64
	// IntentSettled reports that the operator has applied the CR's latest
	// spec.intentRevision (status.lastAppliedIntentRevision equals it), so
	// Phase and FailureReason describe the current generation. Between the
	// applier bumping the spec and the operator reconciling, the CR still
	// reads the PREVIOUS generation's Failed status; a second recovery
	// during that window would bump the generation again.
	IntentSettled bool
```

Set them in `WorkspaceStatus` next to `obs.Phase = ...`: `obs.RuntimeGeneration = ws.Spec.RuntimeGeneration` and `obs.IntentSettled = ws.Status.LastAppliedIntentRevision == ws.Spec.IntentRevision`. (`remember`/`snapshot` need not carry them: recovery acts only on a Fresh projection.)

In `workspaces.go`, add the import `cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"` (the file has no CRD import today), and add `RestartWorkspace(ctx context.Context, tenantID, caller, ownerScope, wsID, idemKey string, fromGeneration int64, bodyHash []byte) (provisioning.WorkspaceRecord, error)` to `workspaceBackend` (`workspaces.go:127`). In `signal` (`workspaces.go:605`), replace the single `rec, err := h.backend.SignalWorkspace(...)` call with:

```go
	var rec provisioning.WorkspaceRecord
	if kind == provisioning.IntentStart {
		rec, err = h.startOrRecover(r.Context(), p, id, key, sum[:])
	} else {
		rec, err = h.backend.SignalWorkspace(r.Context(), p.TenantID, p.Owner(), ownerScope(p), id, key, kind, sum[:])
	}
	if err != nil {
		var refused *recoveryRefused
		if errors.As(err, &refused) {
			e := NewError(CodeInvalidState, "the workspace failed with "+refused.reason+" and cannot be restarted; an administrator must fix the cause")
			e.Details = &ErrorDetails{Reason: refused.reason}
			WriteError(w, r, e)
			return
		}
		h.writeBackendError(w, r, err)
		return
	}
```

and:

```go
// recoveryRefused is a :start on a Failed workspace whose reason a new
// generation cannot fix.
type recoveryRefused struct{ reason string }

func (e *recoveryRefused) Error() string { return "unrecoverable failure: " + e.reason }

// startOrRecover routes :start. A workspace wanted Running whose CR reports
// Failed is recovered with a new runtime generation when the reason is
// recoverable (F1); every other start is the plain SignalWorkspace path.
// The owner-scoped DB read runs first (SEC-21), so a foreign workspace is a
// 404 before any CR state is consulted. Only a Fresh, settled projection
// acts: a stale informer, or a CR whose Failed status still belongs to the
// previous generation (the restart is applied but not yet reconciled), falls
// through to the plain start, which is a no-op for a workspace wanted Running.
// A leftover pod of the failed generation needs no check here: the restart
// revokes the old generation in the same transaction, a fresh ticket needs a
// Ready binding of the new generation, and the operator replaces the pod
// before that.
func (h *WorkspaceHandler) startOrRecover(ctx context.Context, p Principal, id, key string, bodyHash []byte) (provisioning.WorkspaceRecord, error) {
	cur, err := h.backend.GetWorkspace(ctx, p.TenantID, ownerScope(p), id)
	if err != nil {
		return cur, err
	}
	if cur.DesiredState == "Running" && h.statusView != nil {
		obs, err := h.statusView.WorkspaceStatus(ctx, p.TenantID, id)
		if err == nil && obs.Fresh && obs.Found && obs.IntentSettled && obs.Phase == string(cdiv1.WorkspacePhaseFailed) {
			switch {
			case cdiv1.FailureRecoverable(obs.FailureReason):
				return h.backend.RestartWorkspace(ctx, p.TenantID, p.Owner(), ownerScope(p), id, key, obs.RuntimeGeneration, bodyHash)
			case cdiv1.FailureUnrecoverable(obs.FailureReason):
				return cur, &recoveryRefused{reason: obs.FailureReason}
			}
		}
	}
	return h.backend.SignalWorkspace(ctx, p.TenantID, p.Owner(), ownerScope(p), id, key, provisioning.IntentStart, bodyHash)
}
```

`ErrorDetails.Reason` already exists (`errors.go:120`, it carries `release_pending`/`UserLimitReached` today) and `WriteError` leaves details alone for `CodeInvalidState`, so setting `e.Details` on the `*Error` from `NewError` is enough.

Map `provisioning.ErrNotRunning` to `CodeInvalidState`: add it to the existing `errors.Is(err, provisioning.ErrWorkspaceClosed), errors.Is(err, provisioning.ErrInvalidState)` case of `writeBackendError`.

`openapi.yaml`: the `:start` operation (`/v1/workspaces/{workspaceId}:start`, ~line 461) already declares a `409` response. Append this paragraph to its `description`:

```yaml
        A `Failed` workspace that is wanted `Running` is recovered by `:start`
        when its failure reason is recoverable (`BootDeadlineExceeded`,
        `FailedCleanup`, `RuntimeLost`): the platform mints a new runtime
        generation, revokes the failed generation's leases and launch tickets,
        and rebuilds the runtime on the same home volume. Repeating the call
        before the new generation is observed does not mint another one.
        A workspace that failed with `ImagePolicyDenied`, `HomeVolumeLost` or
        `TemplateRejected` is refused with `409 INVALID_STATE` and
        `details.reason` set to that reason; stop and start it again after
        the cause is fixed, or delete it.
```

and append to `ErrorBody.details.reason`'s description (~line 1686-1700): "On `:start` of a Failed workspace: the unrecoverable failure reason (`ImagePolicyDenied`, `HomeVolumeLost`, `TemplateRejected`)." Then `cd web && npm run generate` (the description is carried into `web/src/api/generated/schema.d.ts`; commit the regenerated file).

In `internal/api/workspaces_handlers_test.go`, add `restartCalls []int64` and `signalCalls int` fields to `fakeWorkspaceBackend` (struct at line 22). Increment `signalCalls` at the top of `SignalWorkspace` (under `f.mu`, before the `signalErr` check), and add:

```go
func (f *fakeWorkspaceBackend) RestartWorkspace(_ context.Context, tenantID, _, ownerScope, id, _ string, fromGeneration int64, _ []byte) (provisioning.WorkspaceRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.recs[id]
	if !ok || rec.TenantID != tenantID || (ownerScope != "" && rec.Owner != ownerScope) {
		return provisioning.WorkspaceRecord{}, provisioning.ErrWorkspaceNotFound
	}
	if f.restartErr != nil {
		return provisioning.WorkspaceRecord{}, f.restartErr
	}
	f.restartCalls = append(f.restartCalls, fromGeneration)
	rec.Phase = "Provisioning"
	f.recs[id] = rec
	return rec, nil
}
```

(with a third new field `restartErr error`). Create `internal/api/recovery_test.go`. It reuses `fakeStatusView` (`statusview_test.go:50`, scripted per workspace id), `newWorkspaceEnv`/`doReq`/`login`/`decodeErr` and `testEnv`:

```go
package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

func failedEnv(t *testing.T, obs ObservedStatus) (*fakeWorkspaceBackend, *testEnv, *http.Cookie, *http.Cookie) {
	t.Helper()
	be := newFakeBackend()
	sv := &fakeStatusView{}
	sv.set("ws_failed0001", obs)
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants(),
		func(h *WorkspaceHandler) { h.WithStatusView(sv) })
	sess, csrf := login(t, env, "user-a")
	be.recs["ws_failed0001"] = provisioning.WorkspaceRecord{
		ID: "ws_failed0001", TenantID: "tenant-a", Owner: env.issuer.URL() + "|user-a",
		OwnerIssuer: env.issuer.URL(), OwnerSub: "user-a", Name: "lost",
		Template: provisioning.TemplateInfo{ID: "tpl_linuxdesktop", Name: "linuxdesktop", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop"},
		Phase: "Ready", DesiredState: "Running", DataPolicy: "Retain", Revision: 3,
		CreatedAt: time.Now().UTC().Add(-time.Hour), UpdatedAt: time.Now().UTC(),
	}
	return be, env, sess, csrf
}

func failedObs(reason string) ObservedStatus {
	return ObservedStatus{Fresh: true, Found: true, IntentSettled: true, Phase: "Failed",
		FailureReason: reason, RuntimeGeneration: 4}
}

func startFailed(t *testing.T, env *testEnv, sess, csrf *http.Cookie, key string) *http.Response {
	t.Helper()
	return doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces/ws_failed0001:start", "",
		map[string]string{"Idempotency-Key": key})
}

func TestStart_FailedRecoverableRestartsWithObservedGeneration(t *testing.T) {
	for _, reason := range []string{"RuntimeLost", "BootDeadlineExceeded", "FailedCleanup"} {
		be, env, sess, csrf := failedEnv(t, failedObs(reason))
		r := startFailed(t, env, sess, csrf, "key-recover-01")
		if r.StatusCode != http.StatusAccepted {
			t.Fatalf("%s: status %d, want 202", reason, r.StatusCode)
		}
		if len(be.restartCalls) != 1 || be.restartCalls[0] != 4 || be.signalCalls != 0 {
			t.Fatalf("%s: restart=%v signal=%d, want one restart fenced on generation 4", reason, be.restartCalls, be.signalCalls)
		}
	}
}

func TestStart_FailedUnrecoverableIs409WithReason(t *testing.T) {
	for _, reason := range []string{"HomeVolumeLost", "ImagePolicyDenied", "TemplateRejected"} {
		be, env, sess, csrf := failedEnv(t, failedObs(reason))
		r := startFailed(t, env, sess, csrf, "key-recover-02")
		e := decodeErr(t, r)
		if r.StatusCode != http.StatusConflict || e.Code != CodeInvalidState || e.Details == nil || e.Details.Reason != reason {
			t.Fatalf("%s: status=%d err=%+v, want 409 INVALID_STATE with details.reason", reason, r.StatusCode, e)
		}
		if len(be.restartCalls) != 0 || be.signalCalls != 0 {
			t.Fatalf("%s: an unrecoverable failure must not reach the service", reason)
		}
	}
}

func TestStart_StaleOrUnsettledStatusIsAPlainStart(t *testing.T) {
	stale := failedObs("RuntimeLost")
	stale.Fresh = false
	unsettled := failedObs("RuntimeLost") // the restart is applied, the operator has not reconciled it yet
	unsettled.IntentSettled = false
	for name, obs := range map[string]ObservedStatus{"stale informer": stale, "unsettled intent": unsettled} {
		be, env, sess, csrf := failedEnv(t, obs)
		r := startFailed(t, env, sess, csrf, "key-recover-03")
		if r.StatusCode != http.StatusAccepted || len(be.restartCalls) != 0 || be.signalCalls != 1 {
			t.Fatalf("%s: status=%d restart=%v signal=%d, want a plain start and no restart", name, r.StatusCode, be.restartCalls, be.signalCalls)
		}
	}
}

func TestStart_StoppedFailedWorkspaceIsAPlainStart(t *testing.T) {
	be, env, sess, csrf := failedEnv(t, failedObs("HomeVolumeLost"))
	rec := be.recs["ws_failed0001"]
	rec.DesiredState = "Stopped" // stop, then start: the way out of a refused reason once the cause is fixed
	be.recs["ws_failed0001"] = rec
	r := startFailed(t, env, sess, csrf, "key-recover-05")
	if r.StatusCode != http.StatusAccepted || len(be.restartCalls) != 0 || be.signalCalls != 1 {
		t.Fatalf("status=%d restart=%v signal=%d, want a plain start", r.StatusCode, be.restartCalls, be.signalCalls)
	}
}

func TestStart_RestartNotRunningIs409(t *testing.T) {
	be, env, sess, csrf := failedEnv(t, failedObs("RuntimeLost"))
	be.restartErr = provisioning.ErrNotRunning // a stop raced the recovery
	r := startFailed(t, env, sess, csrf, "key-recover-06")
	if e := decodeErr(t, r); r.StatusCode != http.StatusConflict || e.Code != CodeInvalidState {
		t.Fatalf("status=%d err=%+v, want 409 INVALID_STATE", r.StatusCode, e)
	}
}

func TestStart_ForeignFailedWorkspaceIs404BeforeStatus(t *testing.T) {
	be, env, _, _ := failedEnv(t, failedObs("RuntimeLost"))
	sess, csrf := login(t, env, "user-b")
	r := startFailed(t, env, sess, csrf, "key-recover-04")
	if r.StatusCode != http.StatusNotFound || len(be.restartCalls) != 0 || be.signalCalls != 0 {
		t.Fatalf("status=%d restart=%v signal=%d, want 404 and no mutation", r.StatusCode, be.restartCalls, be.signalCalls)
	}
}
```

`internal/api/statusview_settled_test.go` pins the two new projection fields without envtest: `WorkspaceStatus` reads only `cache.List`, so a controller-runtime fake client behind a minimal cache wrapper is enough.

```go
package api

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// listCache serves List from a fake client; the embedded nil Cache is never
// reached because K8sStatusView.WorkspaceStatus only lists.
type listCache struct {
	crcache.Cache
	c client.Client
}

func (l listCache) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return l.c.List(ctx, list, opts...)
}

func TestK8sStatusView_IntentSettledAndGeneration(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cdiv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cr := func(name, wsID string, specRev, appliedRev int64) *cdiv1.Workspace {
		return &cdiv1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns-a", Labels: map[string]string{
				provisioning.LabelWorkspaceUID: wsID, provisioning.LabelTenant: "tenant-a"}},
			Spec: cdiv1.WorkspaceSpec{RuntimeGeneration: 4, IntentRevision: specRev},
			Status: cdiv1.WorkspaceStatus{
				Phase: cdiv1.WorkspacePhaseFailed, LastAppliedIntentRevision: appliedRev,
				Conditions: []metav1.Condition{{Type: cdiv1.ConditionDegraded, Status: metav1.ConditionTrue,
					Reason: "RuntimeLost", LastTransitionTime: metav1.NewTime(time.Now())}},
			},
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cr("ws-settled", "ws_settled01", 3, 3), cr("ws-behind", "ws_behind001", 3, 2)).Build()
	sv := &K8sStatusView{cache: listCache{c: c}, now: time.Now, maxStale: time.Minute,
		lastKnown: map[string]ObservedStatus{}}
	sv.MarkSynced()

	got, err := sv.WorkspaceStatus(context.Background(), "tenant-a", "ws_settled01")
	if err != nil || !got.Fresh || !got.Found || !got.IntentSettled || got.RuntimeGeneration != 4 || got.FailureReason != "RuntimeLost" {
		t.Fatalf("settled CR projected as %+v (err %v)", got, err)
	}
	got, err = sv.WorkspaceStatus(context.Background(), "tenant-a", "ws_behind001")
	if err != nil || !got.Found || got.IntentSettled {
		t.Fatalf("CR whose latest intent is not yet applied projected as %+v (err %v): must not be settled", got, err)
	}
}
```

Expected after Step 4's `statusview.go` change: PASS. (`provisioning.LabelWorkspaceUID` and `LabelTenant` are the labels `WorkspaceStatus` selects on.)

Run `go test ./internal/api/ -count=1`. Expected: PASS (`TestOpenAPIContract_*` and the error-code drift guard stay green: no new code, only a description).

Wire in `internal/backend/wire.go`, in `wireMerged`, directly after the `brk := broker.New(...)` call (it ends at ~line 408; `svc` is built at line 209 and handed to `newAppHandler` at line 520, so the revoker is installed before any request). Merged mode is the only mode that serves the API: `wireSplit` builds a gateway only, so there is no second `provisioning.Service` to wire.

```go
	svc.WithGenerationRevoker(func(ctx context.Context, tx store.Tx, ws string, gen int64) error {
		_, err := brk.RevokeGenerationTx(ctx, tx, broker.PlatformID(ws), uint64(gen), time.Now())
		return err
	})
```

Run `go build ./... && go test ./internal/backend/ -count=1`. Expected: PASS (`wire.go` imports `time`, `store` and `broker` already).

- [ ] **Step 5: Portal and docs**

In `web/src/workspaces/reasons.tsx`, add `RuntimeLost`, `ImagePolicyDenied` and `HomeVolumeLost` to `REASON_TEXT` (`TemplateRejected` and `BootDeadlineExceeded` are already there), and export:

```ts
/** Failure reasons a new generation cannot fix (api/v1 FailureUnrecoverable): Start is not offered. */
export const UNRECOVERABLE_FAILURES: ReadonlySet<string> = new Set([
  "ImagePolicyDenied",
  "HomeVolumeLost",
  "TemplateRejected",
]);
```

In `WorkspaceDetailPage.tsx` (line 251) change the start condition to `const unrecoverable = ws.phase === "Failed" && UNRECOVERABLE_FAILURES.has(ws.failureReason ?? "");` and `const canStart = ws.phase === "Stopped" || (ws.phase === "Failed" && !unrecoverable);`. Next to the `updateAvailable` alert (line ~394) add `{unrecoverable ? <Alert tone="danger">{t("workspaces.detail.failed.unrecoverable")}</Alert> : null}`. In `web/src/admin/WorkspacesAdminPage.tsx` (`canStart`, line 64) apply the same rule (`ws.phase === "Failed"` is startable only when `!UNRECOVERABLE_FAILURES.has(ws.failureReason ?? "")`), importing the set from `../workspaces/reasons`. A refused reason is left by stop then start (a stop is accepted on a Failed workspace that is wanted Running); the portal offers Delete for it, and the lifecycle doc names the stop-and-start path for administrators.

Strings, in both `web/src/i18n/en/workspaces.ts` and `web/src/i18n/vi/workspaces.ts` (the `vi` file is `satisfies Record<keyof typeof workspaces, string>`, so a missing key fails `npm run typecheck`):

| key | en | vi |
|---|---|---|
| `workspaces.reason.runtimeLost` | The desktop stopped unexpectedly (its container was removed or failed). Start it again to recover; your home folder is kept. | Desktop dừng bất ngờ (container bị xoá hoặc lỗi). Hãy khởi động lại để khôi phục; thư mục home vẫn được giữ. |
| `workspaces.reason.imagePolicyDenied` | The desktop image was refused by a cluster policy. | Image của desktop bị chính sách của cụm từ chối. |
| `workspaces.reason.homeVolumeLost` | The home disk of this workspace no longer exists. | Ổ đĩa home của workspace này không còn tồn tại. |
| `workspaces.detail.failed.unrecoverable` | Starting again would fail the same way. Ask your administrator to fix the cause, or delete this workspace. | Khởi động lại sẽ lỗi tương tự. Hãy nhờ quản trị viên khắc phục nguyên nhân, hoặc xoá workspace này. |

Tests. In `web/tests/unit/workspaces/reasons.test.tsx` add `"RuntimeLost", "ImagePolicyDenied", "HomeVolumeLost"` to the `tokens` list of "maps every curated event and condition token". In `web/tests/unit/workspaces/detail.test.tsx` add (same `WS_ID`, `readyWorkspace`, `createMockApi` as the existing Failed test):

```tsx
  it("detail: a recoverable failure offers Retry start", async () => {
    const api = createMockApi();
    api.state.workspaces.set(
      WS_ID,
      readyWorkspace({ id: WS_ID, phase: "Failed", failureReason: "RuntimeLost" }),
    );
    loginCookies();
    renderWithApi(<WorkspaceDetailPage workspaceId={WS_ID} pollIntervalMs={60_000} />, api);
    expect(await screen.findByRole("button", { name: "Retry start" })).toBeInTheDocument();
    expect(screen.queryByText(/would fail the same way/)).not.toBeInTheDocument();
  }, 20000);

  it("detail: an unrecoverable failure offers no start and says why", async () => {
    const api = createMockApi();
    api.state.workspaces.set(
      WS_ID,
      readyWorkspace({ id: WS_ID, phase: "Failed", failureReason: "HomeVolumeLost" }),
    );
    loginCookies();
    renderWithApi(<WorkspaceDetailPage workspaceId={WS_ID} pollIntervalMs={60_000} />, api);
    expect(await screen.findByText(/Starting again would fail the same way/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Retry start" })).not.toBeInTheDocument();
  }, 20000);
```

and in `web/tests/unit/admin/workspaces.test.tsx` (next to the `canStart({ ...base, phase: "Failed" })` assertion, line 51) assert `canStart({ ...base, phase: "Failed", failureReason: "HomeVolumeLost" })` is `false` and `"RuntimeLost"` is `true`. The Playwright test against the mock API (`web/tests/workspaces.spec.ts`, after the "stable codes surface" test; it reuses `login`, `patchWorkspace`, `TEMPLATE_ID` and the per-test `/_control/reset`):

```ts
test("a Failed workspace offers Retry start unless its reason is unrecoverable", async ({ page, request }) => {
  await login(page);
  await page.getByRole("link", { name: "New workspace" }).click();
  await page.waitForURL("/workspaces/new");
  await page.locator('input[name="name"]').fill("failed-desktop");
  await page.locator('select[name="template"]').selectOption(TEMPLATE_ID);
  await page.getByRole("button", { name: "Create workspace" }).click();
  await page.waitForURL(/\/workspaces\/ws_/);
  const wsId = page.url().split("/").pop()!;

  await patchWorkspace(request, wsId, { phase: "Failed", desiredState: "Running", failureReason: "RuntimeLost" });
  await expect(page.getByRole("button", { name: "Retry start" })).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText("The desktop stopped unexpectedly").first()).toBeVisible();

  await patchWorkspace(request, wsId, { failureReason: "HomeVolumeLost" });
  await expect(page.getByText("Starting again would fail the same way")).toBeVisible({ timeout: 15_000 });
  await expect(page.getByRole("button", { name: "Retry start" })).toHaveCount(0);
});
```

Run `cd web && npm run typecheck && npm run lint:strings && npx vitest run tests/unit/workspaces tests/unit/admin/workspaces.test.tsx && npx playwright test tests/workspaces.spec.ts`. Expected: PASS.

In `docs/lifecycle-reasons.md`, under `## API refusal reasons`, add three rows to the table (the `details.reason` namespace is separate from condition reasons, but these tokens are shared with `Degraded`):

```markdown
| `ImagePolicyDenied` | `INVALID_STATE` | — | `:start` on a `Failed` workspace whose image was refused by a node or admission policy; a new generation would be refused the same way. Fix the policy or the template image, then stop and start the workspace |
| `HomeVolumeLost` | `INVALID_STATE` | — | `:start` on a `Failed` Retain workspace whose home volume no longer exists; the platform never starts it on an empty home. Restore the volume (or delete the workspace), then stop and start it |
| `TemplateRejected` | `INVALID_STATE` | — | `:start` on a `Failed` workspace whose template configuration the runtime refuses. Publish a corrected template revision, then stop and start the workspace |
```

Threat-model row S46 (`docs/security/threat-model.md` §6, after the last existing row):

`| S46 | Failed-workspace recovery | Implemented: :start on Failed is owner-scoped (DB read before CR state, SEC-21) and acts only on a fresh, settled CR projection; recovery bumps runtimeGeneration and revokes the old generation's leases and tickets in one transaction (workspace_revocation fence, TestRestartWorkspace_FencesOldGenerationEndToEnd); fenced on the observed generation under the workspace row lock so concurrent calls bump once; unrecoverable reasons are refused with 409 INVALID_STATE; a lost pod is never rebuilt inside its generation |`

- [ ] **Step 6: Kind e2e step**

The deterministic proof that the old lease and ticket are rejected is `TestRestartWorkspace_FencesOldGenerationEndToEnd` (Postgres). The kind step cannot prove it alone: the 30 s lease TTL expires the old lease by itself while the pod restarts. What the e2e step proves is the rest of the chain on real pods: delete pod, `Failed/RuntimeLost`, `Start` recovers on the same home volume (the file survives), and a new connect works under the new generation with a different lease.

Append to `hack/quickstart/smoke/smoke.spec.ts` (`login`, `createBrowserWorkspace`, `connectSessionFrame`, `inHome`, `kubectl`, `runtimePod`, `waitPhase`, `HOME_DIR` and `Page` already exist in the file):

```ts
async function connectionStatus(page: Page, id: string): Promise<{ leaseActive: boolean; leaseRef?: string }> {
  return page.evaluate(async (wid) => {
    const r = await fetch(`/v1/workspaces/${encodeURIComponent(wid)}/connection`);
    return (await r.json()) as { leaseActive: boolean; leaseRef?: string };
  }, id);
}

async function failureReason(page: Page, id: string): Promise<string> {
  return page.evaluate(async (wid) => {
    const r = await fetch(`/v1/workspaces/${encodeURIComponent(wid)}`);
    return ((await r.json()) as { failureReason?: string }).failureReason ?? "";
  }, id);
}

test("a deleted runtime pod is recovered by Start on the same home, and a new session connects", async ({ page }) => {
  const stamp = Date.now().toString(36);
  const token = `recover-${stamp}`;
  const file = `${HOME_DIR}/recover.txt`;
  await login(page);
  const id = await createBrowserWorkspace(page, `rc-${stamp}`, "Retain");
  await connectSessionFrame(page, id);
  inHome(id, 'printf %s "$1" > "$2" && sync', token, file);
  const before = await connectionStatus(page, id);
  expect(before.leaseActive, "lease while connected").toBe(true);

  kubectl("delete", "pod", runtimePod(id), "--wait=false");
  await page.goto(`/workspaces/${encodeURIComponent(id)}`);
  await waitPhase(page, id, "Failed", 3 * 60_000);
  expect(await failureReason(page, id)).toBe("RuntimeLost");
  const retry = page.getByRole("button", { name: "Retry start" });
  await expect(retry).toBeVisible({ timeout: 60_000 });
  await retry.click();
  await waitPhase(page, id, "Ready", 10 * 60_000);

  expect(inHome(id, 'cat "$1"', file), "home file survives recovery").toBe(token);
  await connectSessionFrame(page, id);
  const after = await connectionStatus(page, id);
  expect(after.leaseActive, "a new session is connected").toBe(true);
  expect(after.leaseRef, "the new generation holds a different lease").not.toBe(before.leaseRef);
});
```

Run `cd hack/quickstart/smoke && npm ci && npx playwright test --list`. Expected: the new test is listed and there are no TypeScript errors. It runs for real in CI's `quickstart` job (kind).

- [ ] **Step 7: Commit**

```bash
go build ./... && go vet ./... && go test -count=1 ./api/... ./internal/provisioning/ ./internal/broker/ ./internal/api/ ./internal/backend/ ./internal/runtime/... ./internal/operator/
cd web && npm run generate && npm run typecheck && npm run lint:strings && npx vitest run && npx playwright test tests/workspaces.spec.ts && cd ..
git add internal web docs/lifecycle-reasons.md docs/security/threat-model.md hack/quickstart/smoke/smoke.spec.ts
git commit -m "feat(lifecycle): recover a Failed workspace with start on a new generation

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 7: File-transfer policy leg: template field, resolver, bound, CR stamp, admin controls (F4, part 1) — tier: integrate

**Files:**
- Modify: `api/v1/workspace_types.go:75-88` (`SessionPolicySpec.MaxFileBytes`); regenerate CRDs and chart copies
- Modify: `internal/provisioning/sessionpolicy.go` (`SessionPolicy.MaxFileBytes`, `meetCeiling`, resolver signatures, `PolicyWithin`), `internal/provisioning/sessionpolicy_internal_test.go`, `internal/provisioning/sessionpolicy_test.go`
- Modify: `internal/provisioning/service.go` (`TemplateInfo.FileTransferPolicy`, `Service.fileBound`, `WithFileTransferMaxBytes`, create path ~284, `startSessionPolicy` ~610), `internal/provisioning/retained.go` (`RetainedStore.fileBound`, `WithFileTransferMaxBytes`, call at :625), `internal/provisioning/template_family.go:43,147,179`, `internal/provisioning/{template_family_test.go,sessionpolicy_test.go}`, `internal/provisioning/k8sapplier.go` (catalog entry ~623, `TemplateCatalogEntry`, `sessionPolicySpec` 309)
- Modify: `internal/runtime/linux/backend.go` (env next to `envClipboardPolicy`, ~1058; add the `strconv` import), `internal/runtime/linux/backend_clipboard_test.go`
- Modify: `internal/api/{adminsessionpolicy.go,adminsessionpolicy_test.go,templates.go:99,171,events.go:144,events_test.go:388,data.go:398,openapi.yaml}`, `internal/api/workspaces.go` (`TemplateEntry` struct :91, `TemplateInfo` build :536)
- Modify: `internal/backend/{config.go,config_test.go,wire.go}` (flag, `WithFileTransferMaxBytes`, `WithFileTransferBound`, template view mapping ~1011)
- Modify: `deploy/helm/tinycdi/{values.yaml,values.schema.json,templates/backend.yaml,README.md}`, `deploy/helm/testdata/default-render.golden.yaml` (regenerated), create `deploy/helm/chart_filetransfer_test.go`
- Modify: `web/src/admin/{SessionPolicyPage.tsx,api.ts}`, `web/src/i18n/{en,vi}/admin.ts`, `web/tests/mock-api/{admin.ts,fixtures.ts,demo.ts}`, `web/tests/unit/admin/sessionpolicy.test.tsx`, `web/tests/session-policy.spec.ts`

**Interfaces:**
- Consumes: `store.SessionPolicyCeiling{FileTransfer *string, FileMaxBytes *int64}`, `store.SessionPolicyWrite`, `meet`.
- Produces (Tasks 8, 11, 13 and 14 rely on these):
  - `cdiv1.SessionPolicySpec.MaxFileBytes int64` (`json:"maxFileBytes,omitempty"`, minimum 0; 0 means file transfer is off)
  - `provisioning.SessionPolicy{Clipboard string; FileTransfer cdiv1.FileTransferPolicy; MaxFileBytes int64}`, where `MaxFileBytes` is `json:"maxFileBytes,omitempty"`
  - `provisioning.TemplateCatalogEntry.FileTransferPolicy string`, `provisioning.TemplateInfo.FileTransferPolicy string` (`json:"fileTransferPolicy,omitempty"`)
  - `func meetCeiling(tpl TemplateCatalogEntry, c store.SessionPolicyCeiling, bound int64) SessionPolicy`
  - `func resolveSessionPolicyTx(ctx context.Context, db *store.DB, tx store.Tx, tenantID string, tpl TemplateCatalogEntry, bound int64) (SessionPolicy, error)`
  - `func (s *Service) WithFileTransferMaxBytes(bound int64) *Service`, `func (s *Service) FileTransferMaxBytes() int64`
  - `func provisioning.PolicyWithin(applied, effective SessionPolicy) bool` (true when `applied` grants nothing `effective` does not)
  - `func provisioning.DirectionAllows(policy cdiv1.FileTransferPolicy, direction string) bool` (`direction` is `"upload"` or `"download"`)
  - Pod env `TCDI_XFER_POLICY` (`Disabled|Upload|Download|Bidirectional`) and `TCDI_XFER_MAX_BYTES` (decimal)
  - `func (r *provisioning.RetainedStore) WithFileTransferMaxBytes(bound int64) *RetainedStore`; `provisioning.SkipReasonFileTransferPolicyChanged = "file-transfer-policy-changed"`
  - Backend flag `-file-transfer-max-bytes` / env `TCDI_FILE_TRANSFER_MAX_BYTES` (default 104857600, range 1..10737418240). Chart value `backend.fileTransfer.maxFileBytes`.
  - `func (h *api.AdminSessionPolicyHandler) WithFileTransferBound(bound int64) *api.AdminSessionPolicyHandler`
  - `GET /v1/templates` items gain `fileTransferPolicy`; `ResolvedSessionPolicy` gains `maxFileBytes`; `PUT session-policy` accepts every `fileTransfer.mode` and `maxFileBytes` from 1 to the bound.

- [ ] **Step 1: CRD field**

In `SessionPolicySpec` (`api/v1/workspace_types.go`), after `FileTransfer`, add:

```go
	// maxFileBytes is the resolved per-file size bound for file transfer:
	// the tenant ceiling's maxFileBytes clipped to the platform bound.
	// 0 when fileTransfer is Disabled. Rendered into the runtime pod as
	// TCDI_XFER_MAX_BYTES; the in-pod transfer helper hard-caps on it.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxFileBytes int64 `json:"maxFileBytes,omitempty"`
```

Run `make manifests generate sync-chart-crds && git diff --stat config/crd deploy/helm/tinycdi/crds`. Expected: both CRD copies change identically.

- [ ] **Step 2: Failing resolver tests**

Replace `TestSessionPolicyMeetCeiling_FileTransferInert` in `internal/provisioning/sessionpolicy_internal_test.go` with:

```go
func TestSessionPolicyMeetCeiling_FileTransferLeg(t *testing.T) {
	up, dl, bi, off := FileTransferUpload, FileTransferDownload, PolicyBidirectional, PolicyDisabled
	small, huge := int64(1024), int64(1<<40)
	const bound = int64(100 << 20)
	cases := []struct {
		name     string
		tpl      string
		ceil     store.SessionPolicyCeiling
		wantMode cdiv1.FileTransferPolicy
		wantMax  int64
	}{
		{"no row follows template, bound applies", "Bidirectional", store.SessionPolicyCeiling{}, cdiv1.FileTransferBidirectional, bound},
		{"template disabled stays disabled", "Disabled", store.SessionPolicyCeiling{HasRow: true, FileTransfer: &bi}, cdiv1.FileTransferDisabled, 0},
		{"empty template leg is disabled", "", store.SessionPolicyCeiling{}, cdiv1.FileTransferDisabled, 0},
		{"ceiling narrows to upload", "Bidirectional", store.SessionPolicyCeiling{HasRow: true, FileTransfer: &up}, cdiv1.FileTransferUpload, bound},
		{"incomparable directions meet to disabled", "Download", store.SessionPolicyCeiling{HasRow: true, FileTransfer: &up}, cdiv1.FileTransferDisabled, 0},
		{"ceiling disabled", "Download", store.SessionPolicyCeiling{HasRow: true, FileTransfer: &off}, cdiv1.FileTransferDisabled, 0},
		{"ceiling size below bound wins", "Download", store.SessionPolicyCeiling{HasRow: true, FileTransfer: &dl, FileMaxBytes: &small}, cdiv1.FileTransferDownload, small},
		{"ceiling size above bound is clipped", "Download", store.SessionPolicyCeiling{HasRow: true, FileMaxBytes: &huge}, cdiv1.FileTransferDownload, bound},
	}
	for _, c := range cases {
		sp := meetCeiling(TemplateCatalogEntry{ClipboardPolicy: PolicyDisabled, FileTransferPolicy: c.tpl}, c.ceil, bound)
		if sp.FileTransfer != c.wantMode || sp.MaxFileBytes != c.wantMax {
			t.Errorf("%s: got %s/%d, want %s/%d", c.name, sp.FileTransfer, sp.MaxFileBytes, c.wantMode, c.wantMax)
		}
	}
}

func TestPolicyWithin(t *testing.T) {
	eff := SessionPolicy{Clipboard: ClipboardSend, FileTransfer: cdiv1.FileTransferUpload, MaxFileBytes: 1000}
	for _, c := range []struct {
		applied SessionPolicy
		want    bool
	}{
		{SessionPolicy{Clipboard: ClipboardSend, FileTransfer: cdiv1.FileTransferUpload, MaxFileBytes: 1000}, true},
		{SessionPolicy{Clipboard: PolicyDisabled, FileTransfer: cdiv1.FileTransferDisabled}, true},
		{SessionPolicy{Clipboard: PolicyBidirectional, FileTransfer: cdiv1.FileTransferUpload, MaxFileBytes: 1000}, false},
		{SessionPolicy{Clipboard: ClipboardSend, FileTransfer: cdiv1.FileTransferBidirectional, MaxFileBytes: 1000}, false},
		{SessionPolicy{Clipboard: ClipboardSend, FileTransfer: cdiv1.FileTransferUpload, MaxFileBytes: 2000}, false},
		{SessionPolicy{Clipboard: ClipboardReceive, FileTransfer: cdiv1.FileTransferDisabled}, false},
	} {
		if got := PolicyWithin(c.applied, eff); got != c.want {
			t.Errorf("PolicyWithin(%+v) = %v, want %v", c.applied, got, c.want)
		}
	}
}

func TestDirectionAllows(t *testing.T) {
	if !DirectionAllows(cdiv1.FileTransferBidirectional, "upload") || !DirectionAllows(cdiv1.FileTransferDownload, "download") {
		t.Fatal("allowed direction refused")
	}
	if DirectionAllows(cdiv1.FileTransferUpload, "download") || DirectionAllows(cdiv1.FileTransferDisabled, "upload") || DirectionAllows(cdiv1.FileTransferBidirectional, "sideways") {
		t.Fatal("refused direction allowed")
	}
}
```

Append to `internal/provisioning/sessionpolicy_test.go` (package `provisioning_test`; `strPtr` and `recoveryDB` already exist there) the DB-backed proof that the platform bound and the stored ceiling both reach `ResolveSessionPolicy`:

```go
func TestResolveSessionPolicy_FileTransferLeg(t *testing.T) {
	db := recoveryDB(t)
	const bound = int64(100 << 20)
	svc := provisioning.NewService(db).WithFileTransferMaxBytes(bound)
	ctx := context.Background()
	tpl := provisioning.TemplateCatalogEntry{
		ClipboardPolicy:    provisioning.PolicyDisabled,
		FileTransferPolicy: provisioning.PolicyBidirectional,
	}

	got, err := svc.ResolveSessionPolicy(ctx, "tenant-ft", tpl)
	if err != nil {
		t.Fatal(err)
	}
	if got.FileTransfer != cdiv1.FileTransferBidirectional || got.MaxFileBytes != bound {
		t.Fatalf("no ceiling row: %+v, want Bidirectional/%d", got, bound)
	}

	small := int64(1 << 20)
	if _, err := store.NewSessionPolicyStore(db).SetCeiling(ctx, "tenant-ft", store.SessionPolicyWrite{
		FileTransfer: strPtr(provisioning.FileTransferUpload), FileMaxBytes: &small,
	}, "", true); err != nil {
		t.Fatal(err)
	}
	got, err = svc.ResolveSessionPolicy(ctx, "tenant-ft", tpl)
	if err != nil {
		t.Fatal(err)
	}
	if got.FileTransfer != cdiv1.FileTransferUpload || got.MaxFileBytes != small {
		t.Fatalf("Upload/1MiB ceiling: %+v, want Upload/%d", got, small)
	}
}
```

Run `go test ./internal/provisioning/ -run 'MeetCeiling|PolicyWithin|DirectionAllows' -count=1`. Expected: FAIL to compile (`MaxFileBytes`, `PolicyWithin`, `DirectionAllows` and the `bound` parameter do not exist yet).

- [ ] **Step 3: Implement the resolver leg**

In `sessionpolicy.go`, update the `FileTransferUpload`/`FileTransferDownload` const comment to "File-transfer channel directions (ADR 0010 T11)". Then:

```go
type SessionPolicy struct {
	Clipboard    string                   `json:"clipboard"`
	FileTransfer cdiv1.FileTransferPolicy `json:"fileTransfer"`
	// MaxFileBytes is the per-file bound: the ceiling's file_max_bytes
	// clipped to the platform bound (-file-transfer-max-bytes); 0 when
	// FileTransfer is Disabled.
	MaxFileBytes int64 `json:"maxFileBytes,omitempty"`
}

func meetCeiling(tpl TemplateCatalogEntry, c store.SessionPolicyCeiling, bound int64) SessionPolicy {
	var clipCeil, ftCeil string
	if c.Clipboard != nil {
		clipCeil = *c.Clipboard
	}
	if c.FileTransfer != nil {
		ftCeil = *c.FileTransfer
	}
	ftTpl := tpl.FileTransferPolicy
	if ftTpl == "" {
		ftTpl = PolicyDisabled
	}
	sp := SessionPolicy{
		Clipboard:    meet(tpl.ClipboardPolicy, clipCeil, ClipboardSend, ClipboardReceive),
		FileTransfer: cdiv1.FileTransferPolicy(meet(ftTpl, ftCeil, FileTransferUpload, FileTransferDownload)),
	}
	if sp.FileTransfer != cdiv1.FileTransferDisabled {
		sp.MaxFileBytes = bound
		if c.FileMaxBytes != nil && *c.FileMaxBytes < bound {
			sp.MaxFileBytes = *c.FileMaxBytes
		}
	}
	return sp
}

// PolicyWithin reports whether a workspace running with applied grants
// nothing the effective policy does not: every channel of applied is at or
// below effective in the lattice, and its size bound is no larger. F3 uses
// it to find running workspaces a tightened ceiling has made too loose.
func PolicyWithin(applied, effective SessionPolicy) bool {
	return meet(applied.Clipboard, effective.Clipboard, ClipboardSend, ClipboardReceive) == applied.Clipboard &&
		meet(string(applied.FileTransfer), string(effective.FileTransfer), FileTransferUpload, FileTransferDownload) == string(applied.FileTransfer) &&
		applied.MaxFileBytes <= effective.MaxFileBytes
}

// DirectionAllows reports whether policy permits a transfer in direction
// ("upload" = device to workspace, "download" = workspace to device).
func DirectionAllows(policy cdiv1.FileTransferPolicy, direction string) bool {
	switch direction {
	case "upload":
		return policy == cdiv1.FileTransferUpload || policy == cdiv1.FileTransferBidirectional
	case "download":
		return policy == cdiv1.FileTransferDownload || policy == cdiv1.FileTransferBidirectional
	}
	return false
}
```

`PolicyWithin` uses `meet(applied, effective)` with `effective` acting as a ceiling. `meet(x, ceiling)` returns `x` exactly when `x ≤ ceiling` for every real ceiling value. For a Disabled ceiling, `meet` returns Disabled, which equals `applied` only when `applied` is Disabled. That is correct.

Add `bound int64` as the last parameter of `resolveSessionPolicyTx` and pass it into `meetCeiling`. `ResolveSessionPolicy` passes `s.fileBound`. Add `FileTransferPolicy string` to `TemplateCatalogEntry` (`k8sapplier.go:397`, next to `ClipboardPolicy`), set it at `k8sapplier.go:623` with `e.FileTransferPolicy = string(t.Spec.FileTransferPolicy)`, and add `FileTransferPolicy string \`json:"fileTransferPolicy,omitempty"\`` to `TemplateInfo` (`service.go:154`).

In `service.go`:
- add the field `fileBound int64` to `Service`, plus:

```go
// WithFileTransferMaxBytes sets the platform per-file bound
// (-file-transfer-max-bytes) every resolved policy is clipped to.
func (s *Service) WithFileTransferMaxBytes(bound int64) *Service {
	s.fileBound = bound
	s.retained.fileBound = bound
	return s
}

// FileTransferMaxBytes returns the platform per-file bound.
func (s *Service) FileTransferMaxBytes() int64 { return s.fileBound }
```

- add `fileBound int64` to `RetainedStore` (`retained.go:130`) and `func (r *RetainedStore) WithFileTransferMaxBytes(bound int64) *RetainedStore { r.fileBound = bound; return r }`. Its `resolveSessionPolicyTx` call (`retained.go:625`, inside `(s *RetainedStore) AttachRetained`, so the receiver is `s`) passes `s.fileBound` and adds `FileTransferPolicy: req.Template.FileTransferPolicy` to the entry. `wire.go` builds a second, standalone `retained := provisioning.NewRetainedStore(db)` for the data handler (`api.NewDataHandler(retained, ...)`), so setting the bound only on `svc` would leave `POST /v1/data/{id}:attach` stamping `MaxFileBytes = 0`; Step 5 wires both;
- the create path (`service.go:284`) passes `TemplateCatalogEntry{ClipboardPolicy: req.Template.ClipboardPolicy, FileTransferPolicy: req.Template.FileTransferPolicy}` and `s.fileBound`;
- `startSessionPolicy` builds `TemplateCatalogEntry{ClipboardPolicy: clipboard, FileTransferPolicy: rec.Template.FileTransferPolicy}` (when it consults the live catalog because `clipboard == ""`, it also takes `cur.FileTransferPolicy`) and passes `s.fileBound`.

In `template_family.go:147`, add `FileTransferPolicy: newest.FileTransferPolicy`. At `:179`, add a guard case after the clipboard one: `case cur.FileTransferPolicy != next.FileTransferPolicy: return SkipReasonFileTransferPolicyChanged`. Define `SkipReasonFileTransferPolicyChanged = "file-transfer-policy-changed"` (the tokens are kebab-case, see `SkipReasonClipboardPolicyChanged = "clipboard-policy-changed"` at `:43`) next to it. The curated event text lives only in the Go map `templateSkipMessages` (`internal/api/events.go:144`); there is no web or i18n copy of the skip reasons. Add the entry:

```go
	provisioning.SkipReasonFileTransferPolicyChanged: "The workspace stayed on its recorded template revision (file-transfer-policy-changed): the newest published revision sets a different file-transfer policy.",
```

and add `provisioning.SkipReasonFileTransferPolicyChanged` to the reason list at `internal/api/events_test.go:388` and a `{"file-transfer-policy-changed", func(e *provisioning.TemplateCatalogEntry) { e.FileTransferPolicy = "Upload" }}` row beside the clipboard rows at `internal/provisioning/template_family_test.go:445` and `:544` (copy the clipboard row's shape in each table).

In `k8sapplier.go` `sessionPolicySpec`, return `cdiv1.SessionPolicySpec{Clipboard: ..., FileTransfer: ft, MaxFileBytes: sp.MaxFileBytes}`. Force `MaxFileBytes` to 0 when `ft` is Disabled.

Update the two `TemplateInfo` construction sites to carry the field: `internal/api/workspaces.go:536` and `internal/api/data.go:398` (`FileTransferPolicy: tpl.FileTransferPolicy`). Both read the API `TemplateEntry` struct (`workspaces.go:91`, add `FileTransferPolicy string` after `ClipboardPolicy`). Then add the field beside each remaining `ClipboardPolicy` hit: `wire.go:1011` (`FileTransferPolicy: e.FileTransferPolicy`) and `templates.go` (`templateView`, `:99`: `FileTransferPolicy string \`json:"fileTransferPolicy"\``; the mapping at `:171`). In that mapping render an empty catalog value as `Disabled` (`ft := e.FileTransferPolicy; if ft == "" { ft = "Disabled" }`), the same reading `meetCeiling` gives it, so the response always satisfies the enum.

Run `go build ./... && go test ./internal/provisioning/ -count=1`. Expected: PASS. The existing DB-backed `TestService_SessionPolicyRidesIntents` still passes (template leg empty → Disabled).

- [ ] **Step 4: Pod env, with a test**

In `internal/runtime/linux/backend.go`, add:

```go
	// envXferPolicy / envXferMaxBytes carry spec.sessionPolicy.fileTransfer
	// and .maxFileBytes to the runtime; tcdi-xferd fails closed (Disabled)
	// on an unset or unknown policy.
	envXferPolicy   = "TCDI_XFER_POLICY"
	envXferMaxBytes = "TCDI_XFER_MAX_BYTES"
```

The clipboard env is a single statement at `~1057-1058` (`ctr.Env = append(ctr.Env, corev1.EnvVar{Name: envClipboardPolicy, ...})`). Extend that same `append` with two more values, and add `"strconv"` to the import block (the file does not import it yet):

```go
	ctr.Env = append(ctr.Env,
		corev1.EnvVar{Name: envClipboardPolicy, Value: string(ws.Spec.SessionPolicy.Clipboard)},
		corev1.EnvVar{Name: envXferPolicy, Value: string(ws.Spec.SessionPolicy.FileTransfer)},
		corev1.EnvVar{Name: envXferMaxBytes, Value: strconv.FormatInt(ws.Spec.SessionPolicy.MaxFileBytes, 10)})
```

The statement sits before the `kasm := ...` branch, so both the linux-base and the kasm-adapter pods carry the env. Append to `internal/runtime/linux/backend_clipboard_test.go` (same package, helpers `testTemplate`, `testWorkspace`, `ensurePodSpecWS` and `envOf` already exist):

```go
func TestPodCarriesFileTransferEnv(t *testing.T) {
	for _, adapter := range []cdiv1.RuntimeAdapter{cdiv1.AdapterNone, cdiv1.AdapterKasm} {
		tpl := testTemplate(nil)
		opts := Options{}
		if adapter == cdiv1.AdapterKasm {
			tpl.Spec.Linux.Adapter = adapter
			opts.KasmAdapterImage = testAdapterImage
		}
		ws := testWorkspace()
		ws.Spec.SessionPolicy = cdiv1.SessionPolicySpec{
			Clipboard:    cdiv1.ClipboardDisabled,
			FileTransfer: cdiv1.FileTransferUpload,
			MaxFileBytes: 4096,
		}
		ctr := ensurePodSpecWS(t, ws, tpl, opts).Spec.Containers[0]
		for k, want := range map[string]string{"TCDI_XFER_POLICY": "Upload", "TCDI_XFER_MAX_BYTES": "4096"} {
			if v, ok := envOf(&ctr, k); !ok || v != want {
				t.Errorf("adapter=%q: env %s = %q (present=%v), want %q", adapter, k, v, ok, want)
			}
		}
	}
}
```

Run `go test ./internal/runtime/linux/ -run TestPodCarriesFileTransferEnv -count=1`. Expected: PASS.

- [ ] **Step 5: Platform bound flag and chart value**

`config.go`:
- add `FileTransferMaxBytes int64` and `fs.Int64Var(&c.FileTransferMaxBytes, "file-transfer-max-bytes", envInt64(getenv, "TCDI_FILE_TRANSFER_MAX_BYTES", 104857600), "platform per-file bound for session file transfer; a tenant maxFileBytes may only lower it (env TCDI_FILE_TRANSFER_MAX_BYTES)")`;
- validation, beside the `-terminal-retention-days` check (`config.go:~423`): `if c.FileTransferMaxBytes < 1 || c.FileTransferMaxBytes > 10<<30 { return errors.New("-file-transfer-max-bytes must be 1..10737418240") }`.

`envInt64` does not exist (only `envInt`, `config.go:237`): add it right after `envInt`, same shape, using `strconv.ParseInt(v, 10, 64)`.

Add `TestParseFlags_FileTransferMaxBytes` to `config_test.go`, shaped like `TestParseFlags_AuditRetentionDays` (Task 1a): `ParseFlags(mergedArgs(), noEnv)` gives 104857600; `envMap(map[string]string{"TCDI_FILE_TRANSFER_MAX_BYTES": "1048576"})` gives 1048576; and `ParseFlags(withArg(mergedArgs(), "-file-transfer-max-bytes", bad), noEnv)` errors for `bad` in `"0"`, `"-1"`, `"10737418241"`.

In `wire.go`:
- line ~209: add `.WithFileTransferMaxBytes(cfg.FileTransferMaxBytes)` to the `svc := provisioning.NewService(db)...` chain, and change line ~214 to `retained := provisioning.NewRetainedStore(db).WithFileTransferMaxBytes(cfg.FileTransferMaxBytes)` (the data handler's attach path uses this second store);
- line ~728: add `.WithFileTransferBound(cfg.FileTransferMaxBytes)` to the `api.NewAdminSessionPolicyHandler(...)` chain.

Chart:
- `values.yaml` gets `backend.fileTransfer.maxFileBytes: 104857600`, commented "Platform per-file bound for session file transfer (ADR 0010 T11). A tenant ceiling may only lower it.";
- the schema gets `"fileTransfer": {"type": "object", "additionalProperties": false, "properties": {"maxFileBytes": {"type": "integer", "minimum": 1, "maximum": 10737418240}}}` under `backend.properties`;
- the template gets the arg `- -file-transfer-max-bytes={{ int64 .Values.backend.fileTransfer.maxFileBytes }}`;
- the README gets a row.

`deploy/helm/chart_filetransfer_test.go` (package `chart_test`, like every file in that directory):

```go
package chart_test

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestFileTransferBoundDefaultAndCustom(t *testing.T) {
	if args := backendArgsWith(t); !hasArg(args, "-file-transfer-max-bytes=104857600") {
		t.Errorf("default render lacks the 100 MiB bound: %v", args)
	}
	if args := backendArgsWith(t, "backend.fileTransfer.maxFileBytes=1048576"); !hasArg(args, "-file-transfer-max-bytes=1048576") {
		t.Errorf("custom bound not rendered: %v", args)
	}
	out := renderErrArgs(t, "-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "backend.fileTransfer.maxFileBytes=0")
	if !strings.Contains(out, "maxFileBytes") && !strings.Contains(out, "minimum") {
		t.Errorf("maxFileBytes=0 must be rejected by the schema: %s", out)
	}
}
```

Large integers render in scientific notation without `int64`, so the test also pins the exact decimal. The CRD field (Step 1) and the new arg both change the default render, so regenerate the golden after `make sync-chart-crds` and the chart edits, and review the diff (only the `maxFileBytes` CRD property and the one `-file-transfer-max-bytes=104857600` arg line may change):

```bash
cd deploy/helm && helm template tcdi ./tinycdi --namespace tcdi-system --include-crds \
  -f tinycdi/ci/minimal-values.yaml > testdata/default-render.golden.yaml && cd ../.. \
  && git diff --stat deploy/helm/testdata
```

Run `go test ./internal/backend/ -run FileTransfer -count=1 && go test -count=1 ./deploy/helm/`. Expected: PASS (the whole chart suite, including `TestDefaultRenderUnchangedGolden`).

- [ ] **Step 6: Admin PUT accepts the file-transfer channel**

In `adminsessionpolicy.go`:
- add `"fmt"` and `"strconv"` to the imports;
- add `fileBound int64` to the handler, plus `WithFileTransferBound(bound int64) *AdminSessionPolicyHandler`;
- replace the inert block in `Put` with:

```go
	if req.FileTransfer != nil {
		ft := req.FileTransfer
		if ft.Mode != nil && *ft.Mode != provisioning.PolicyFollowTemplate {
			if !validCeilingValue(*ft.Mode, provisioning.FileTransferUpload, provisioning.FileTransferDownload) {
				writeError(w, r, CodeInvalidRequest,
					"fileTransfer.mode must be one of FollowTemplate, Disabled, Upload, Download, Bidirectional")
				return
			}
			mode := *ft.Mode
			write.FileTransfer = &mode
			auditSetDetail(r.Context(), "policy", "fileTransfer="+mode)
		}
		if ft.MaxFileBytes != nil {
			if *ft.MaxFileBytes < 1 || (h.fileBound > 0 && *ft.MaxFileBytes > h.fileBound) {
				writeError(w, r, CodeInvalidRequest,
					fmt.Sprintf("fileTransfer.maxFileBytes must be 1..%d (the platform bound)", h.fileBound))
				return
			}
			n := *ft.MaxFileBytes
			write.FileMaxBytes = &n
			auditSetDetail(r.Context(), "bytes", strconv.FormatInt(n, 10))
		}
	}
```

Move the `var write store.SessionPolicyWrite` declaration (and the clipboard assignment) above this block. `write.Defaults()` then covers all three fields. The doc comment loses its "inert" sentences and says: "fileTransfer.mode narrows the file channel; maxFileBytes lowers the per-file bound and may not exceed the platform bound (-file-transfer-max-bytes)."

In `adminsessionpolicy_test.go`:
- `TestAdminSessionPolicy_PutValidation` (`:205`) still lists the two inert cases (`"fileTransfer mode"` with `Upload` and `"fileTransfer bound"` with `maxFileBytes:1024`); both are valid now and would answer 200. Replace them with `"fileTransfer bad mode": {"clipboard":"Disabled","fileTransfer":{"mode":"Sideways"}}` and `"fileTransfer zero bound": {"clipboard":"Disabled","fileTransfer":{"mode":"Upload","maxFileBytes":0}}`, and fix its doc comment (the channel is no longer inert).
- In `TestAdminSessionPolicy_Contract` (`:361`) add a second resolved value, `provisioning.SessionPolicy{Clipboard: provisioning.PolicyDisabled, FileTransfer: cdiv1.FileTransferUpload, MaxFileBytes: 1 << 20}`, and `requireValid(t, "ResolvedSessionPolicy", ...)` it.
- Add the test below. The env helper is `newSessionPolicyEnv(t, src, opts ...func(*AdminSessionPolicyHandler))` (already variadic), the source fake is `&fakeSessionPolicySource{}`, and `loginAdmin`/`doReq` take and return `*http.Cookie`:

```go
func TestAdminSessionPolicy_FileTransferWrites(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{`{"clipboard":"FollowTemplate","fileTransfer":{"mode":"Upload","maxFileBytes":1048576}}`, http.StatusOK},
		{`{"clipboard":"FollowTemplate","fileTransfer":{"mode":"Sideways"}}`, http.StatusBadRequest},
		{`{"clipboard":"FollowTemplate","fileTransfer":{"mode":"Upload","maxFileBytes":0}}`, http.StatusBadRequest},
		{`{"clipboard":"FollowTemplate","fileTransfer":{"mode":"Upload","maxFileBytes":104857601}}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		src := &fakeSessionPolicySource{}
		env := newSessionPolicyEnv(t, src, func(h *AdminSessionPolicyHandler) { h.WithFileTransferBound(104857600) })
		sess, csrf := loginAdmin(t, env, "admin-a")
		res := doReq(t, env, sess, csrf, http.MethodPut, adminSessionPolicyPath, c.body,
			map[string]string{"If-Match": "*"})
		res.Body.Close()
		if res.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", c.body, res.StatusCode, c.want)
		}
		if c.want == http.StatusOK {
			w := src.lastWrite
			if w.FileTransfer == nil || *w.FileTransfer != "Upload" || w.FileMaxBytes == nil || *w.FileMaxBytes != 1048576 {
				t.Errorf("store write = %+v, want Upload/1048576", w)
			}
		} else if src.setCall != 0 {
			t.Errorf("%s reached the store", c.body)
		}
	}
}
```

OpenAPI (`internal/api/openapi.yaml`; the line numbers are origin/main):
- `putAdminSessionPolicy` description (`:1115-1118`): replace the "`fileTransfer` is inert in this version ... `400 INVALID_REQUEST`" sentence with "`fileTransfer.mode` narrows the file-transfer channel the same way `clipboard` does; `fileTransfer.maxFileBytes` lowers the per-file bound and may not exceed the platform bound (`-file-transfer-max-bytes`), else `400 INVALID_REQUEST`.";
- `FileTransferCeiling` (`:2224-2246`): description becomes "The tenant's file-transfer ceiling over the template's `fileTransferPolicy`: `mode` narrows the direction, `maxFileBytes` lowers the per-file size bound."; `mode` loses "Inert until v1.3 adds the template leg."; `maxFileBytes` description becomes "Per-file size bound, 1 up to the platform bound; `null` = the platform bound.";
- `PutAdminSessionPolicy` description (`:2281`): drop "`fileTransfer` is the inert v1.3 channel and only its defaults are accepted in this version.";
- `ResolvedSessionPolicy` (`:2290`): the `fileTransfer` description (`:2308-2312`) drops "Inert until v1.3 — always resolves `Disabled` ..." and says "The resolved file-transfer direction policy: the meet of the template's `fileTransferPolicy` and the tenant ceiling."; it gains `maxFileBytes: {type: integer, format: int64, minimum: 0, description: "Per-file bound in bytes; 0 when fileTransfer is Disabled."}`, not in `required` (the schema has `additionalProperties: false`, so the new JSON key would fail the contract test without it);
- `TemplateView` (`:2535`): add `fileTransferPolicy` to its `required` list after `clipboardPolicy` and a property `fileTransferPolicy: {type: string, enum: [Disabled, Upload, Download, Bidirectional], description: "File transfer between the user's device and the workspace home (Upload = device to workspace, Download = workspace to device). Disabled unless the template opts in; the tenant ceiling can only narrow it."}`.

Run `cd web && npm run generate && cd .. && go test ./internal/api/ -count=1`. Expected: PASS. The `Disabled` normalization in the `templateView` mapping (Step 3) keeps Go catalog fixtures valid; fix any hand-built `templateView` fixture the contract test still flags by adding `FileTransferPolicy: "Disabled"`.

- [ ] **Step 7: Session-policy page controls go live**

`web/src/admin/api.ts`: next to `ClipboardCeiling` (`:165`) add `export type FileTransferCeiling = components["schemas"]["FileTransferCeiling"];`.

`web/src/admin/SessionPolicyPage.tsx`:

```tsx
const FT_MODES: readonly FileTransferCeiling["mode"][] = [
  "FollowTemplate", "Disabled", "Upload", "Download", "Bidirectional",
];
const FT_LABEL: Record<FileTransferCeiling["mode"], MessageKey> = {
  FollowTemplate: "admin.policy.option.followTemplate",
  Disabled: "admin.policy.ft.option.disabled",
  Upload: "admin.policy.ft.option.upload",
  Download: "admin.policy.ft.option.download",
  Bidirectional: "admin.policy.ft.option.bidirectional",
};
const MIB = 1048576;
const mibOf = (bytes: number | null | undefined): string => (bytes ? String(bytes / MIB) : "");

export function fileTransferLabel(policy: string): string {
  const key = FT_LABEL[policy as FileTransferCeiling["mode"]];
  return key ? t(key) : policy;
}

// meetFileTransfer is the same direction lattice as meetClipboard with
// Upload/Download as the two incomparable directions (ADR 0010 §1).
export function meetFileTransfer(template: string, ceiling: string): string {
  switch (ceiling) {
    case "Disabled":
      return "Disabled";
    case "Upload":
    case "Download":
      return template === ceiling || template === "Bidirectional" ? ceiling : "Disabled";
    default:
      return template; // FollowTemplate, Bidirectional, or unrecognised: narrows nothing
  }
}
```

In `SessionPolicyContent` add state `ftMode` (init `policy.fileTransfer.mode`) and `ftMax` (init `mibOf(policy.fileTransfer.maxFileBytes)`), reset both in the existing `useEffect([policy])`. Derive:

```tsx
const ftBytes = ftMax === "" ? null : Math.round(Number(ftMax) * MIB);
const ftInvalid = ftBytes !== null && !(ftBytes >= 1);
const dirty =
  ceiling !== policy.clipboard ||
  ftMode !== policy.fileTransfer.mode ||
  ftMax !== mibOf(policy.fileTransfer.maxFileBytes);
```

Replace the disabled file-transfer `Select` with a live one (`value={ftMode}`, `onChange={(e) => setFtMode(e.target.value as FileTransferCeiling["mode"])}`, `options={FT_MODES.map((v) => ({ value: v, label: t(FT_LABEL[v]) }))}`, same `name="file-transfer-ceiling"`, label and hint keys). Below it add:

```tsx
<Input
  label={t("admin.policy.ft.maxLabel")}
  hint={t("admin.policy.ft.maxHint")}
  name="file-max-mib"
  type="number"
  min={0}
  step="any"
  value={ftMax}
  onChange={(e) => setFtMax(e.target.value)}
/>
```

(`Input` comes from `../design`; import `type FileTransferCeiling` from `./api`.) The save body becomes `{ clipboard: ceiling, fileTransfer: { mode: ftMode, maxFileBytes: ftBytes } }`, and the Save button is `disabled={!dirty || ftInvalid}`. The server enforces the platform bound (400 `INVALID_REQUEST`, shown by `ApiErrorAlert`).

The preview table (caption unchanged: existing specs select it by name) gains two columns after `effective`: `templateFt` (`header: t("admin.policy.preview.templateFt")`, `render: (tpl) => fileTransferLabel(tpl.fileTransferPolicy)`) and `effectiveFt` (`header: t("admin.policy.preview.effectiveFt")`, `render: (tpl) => fileTransferLabel(meetFileTransfer(tpl.fileTransferPolicy, ftMode))`, sortable on the same value).

Strings in `web/src/i18n/{en,vi}/admin.ts`. Replace `admin.policy.fileTransfer.hint` (currently "Available in v1.3."), `admin.policy.appliesNote` and `admin.policy.preview.description`, and add the rest:

| key | en | vi |
|---|---|---|
| `admin.policy.fileTransfer.hint` | Upload is device → workspace, Download is workspace → device. Files land in the Transfers folder of the workspace home. | Tải lên là thiết bị → workspace, Tải xuống là workspace → thiết bị. Tệp nằm trong thư mục Transfers của thư mục chính workspace. |
| `admin.policy.appliesNote` | Tightening applies to new connections and new file transfers right away; a running workspace's clipboard limits follow at its next start. | Siết chặt áp dụng ngay với các kết nối mới và lượt truyền tệp mới; giới hạn clipboard của workspace đang chạy có hiệu lực ở lần khởi động tiếp theo. |
| `admin.policy.preview.description` | What users get for each template: the template's own clipboard and file-transfer policy narrowed by the selected ceilings. | Người dùng nhận được gì với từng template: chính sách clipboard và truyền tệp của template, được thu hẹp bởi các giới hạn đã chọn. |
| `admin.policy.ft.option.upload` | Upload only | Chỉ tải lên |
| `admin.policy.ft.option.download` | Download only | Chỉ tải xuống |
| `admin.policy.ft.option.bidirectional` | Both directions | Cả hai chiều |
| `admin.policy.ft.option.disabled` | Disabled | Tắt |
| `admin.policy.ft.maxLabel` | Largest file (MiB) | Tệp lớn nhất (MiB) |
| `admin.policy.ft.maxHint` | Empty uses the platform limit. | Để trống để dùng giới hạn của nền tảng. |
| `admin.policy.preview.templateFt` | Template file transfer | Truyền tệp của template |
| `admin.policy.preview.effectiveFt` | Effective file transfer | Truyền tệp hiệu lực |

Mock API (`web/tests/mock-api/`):
- `fixtures.ts` (`TEMPLATE_LINUX` :40 gets `fileTransferPolicy: "Bidirectional"`, `TEMPLATE_BROWSER` :60 gets `"Disabled"`) and `demo.ts` (`:33`, `:48`, `:63`: `"Bidirectional"`, `"Disabled"`, `"Disabled"`). `TemplateFixture` is the generated `TemplateView`, so `npm run typecheck` fails until the required field is present everywhere.
- `admin.ts`: the stored row becomes `sessionPolicy: { clipboard: string; fileTransfer: string; maxFileBytes: number | null } | undefined` (`:182`). `sessionPolicyBody()` returns `fileTransfer: { mode: t.sessionPolicy?.fileTransfer ?? "FollowTemplate", maxFileBytes: t.sessionPolicy?.maxFileBytes ?? null }`. In the PUT branch (`:571`) replace the "inert" refusal with:

```ts
      const FT_MODES = ["FollowTemplate", "Disabled", "Upload", "Download", "Bidirectional"];
      const PLATFORM_FILE_BOUND = 104857600;
      const ft = (b.fileTransfer ?? {}) as { mode?: string; maxFileBytes?: number | null };
      const ftMode = ft.mode ?? "FollowTemplate";
      const ftMax = ft.maxFileBytes ?? null;
      if (
        !FT_MODES.includes(ftMode) ||
        (ftMax !== null && (!Number.isInteger(ftMax) || ftMax < 1 || ftMax > PLATFORM_FILE_BOUND))
      ) {
        return err(400, "INVALID_REQUEST", "fileTransfer.mode or maxFileBytes is invalid", false);
      }
      if (clip === "FollowTemplate" && ftMode === "FollowTemplate" && ftMax === null) {
        t.sessionPolicy = undefined;
      } else {
        t.sessionPolicy = { clipboard: clip, fileTransfer: ftMode, maxFileBytes: ftMax };
      }
```

  and drop the old `if (clip === "FollowTemplate") ... else ...` assignment. The `/_control/admin/session-policy` handler (`:669`) builds the same object: `{ clipboard: <as today>, fileTransfer: String(req.body?.fileTransfer ?? "FollowTemplate"), maxFileBytes: typeof req.body?.maxFileBytes === "number" ? req.body.maxFileBytes : null }`.
- Leave the launch-ticket `sessionPolicy` in `workspaces.ts` alone; Task 14 rewrites it.

Existing assertions that encoded the inert control must flip:
- `web/tests/unit/admin/sessionpolicy.test.tsx` test "renders the five ceiling options, the inert file-transfer control ...": replace `expect(screen.getByLabelText("File transfer", { exact: true })).toBeDisabled()` with `toBeEnabled()` plus `toHaveValue("FollowTemplate")`, and rename it (no "inert").
- `web/tests/session-policy.spec.ts` first test: replace the "inert v1.3 channel" comment and `toBeDisabled()` with `await expect(page.getByLabel("File transfer", { exact: true })).toHaveValue("FollowTemplate");`.

New unit tests in `sessionpolicy.test.tsx` (import `meetFileTransfer` beside `meetClipboard`):

```tsx
describe("meetFileTransfer: direction lattice", () => {
  const cases: Array<[string, string, string]> = [
    ["Bidirectional", "FollowTemplate", "Bidirectional"],
    ["Upload", "Bidirectional", "Upload"],
    ["Bidirectional", "Upload", "Upload"],
    ["Bidirectional", "Download", "Download"],
    ["Upload", "Download", "Disabled"],
    ["Download", "Upload", "Disabled"],
    ["Upload", "Upload", "Upload"],
    ["Disabled", "Bidirectional", "Disabled"],
    ["Bidirectional", "Disabled", "Disabled"],
  ];
  for (const [template, ceiling, want] of cases) {
    it(`${template} ∩ ${ceiling} = ${want}`, () => {
      expect(meetFileTransfer(template, ceiling)).toBe(want);
    });
  }
});
```

and, inside `describe("session policy page", ...)`:

```tsx
  it("the file-transfer ceiling is live: mode and size ride the PUT", async () => {
    const api = setup();
    renderWithApi(<SessionPolicyPage />, api);
    const ft = await screen.findByLabelText("File transfer", { exact: true });
    fireEvent.change(ft, { target: { value: "Upload" } });
    fireEvent.change(screen.getByLabelText("Largest file (MiB)"), { target: { value: "10" } });
    fireEvent.click(screen.getByRole("button", { name: "Save ceiling" }));
    await waitFor(() =>
      expect(
        api.state.requests.some((r) => {
          if (r.method !== "PUT" || r.path !== "/v1/admin/tenants/acme/session-policy") return false;
          const body = JSON.parse(r.rawBody) as { fileTransfer?: { mode?: string; maxFileBytes?: number } };
          return body.fileTransfer?.mode === "Upload" && body.fileTransfer?.maxFileBytes === 10485760;
        }),
      ).toBe(true),
    );
  });
```

Playwright, appended to `web/tests/session-policy.spec.ts` (same file as the clipboard-ceiling test; it reuses `makeAdmin`, `login` and the per-test reset):

```ts
test("admin sets an Upload-only file-transfer ceiling with a size bound", async ({ page, request }) => {
  await makeAdmin(request);
  await login(page);

  const ft = page.getByLabel("File transfer", { exact: true });
  await expect(ft).toHaveValue("FollowTemplate");
  await ft.selectOption("Upload");
  await page.getByLabel("Largest file (MiB)").fill("10");

  // linux-firefox-desktop's file-transfer policy is Bidirectional, so the
  // preview shows Bidirectional ∧ Upload = Upload before anything is saved.
  const preview = page.getByRole("table", { name: "Effective clipboard policy by template" });
  const linuxRow = preview.getByRole("row", { name: /linux-firefox-desktop/ });
  await expect(linuxRow.getByRole("cell", { name: "Upload only" }).first()).toBeVisible();

  await page.getByRole("button", { name: "Save ceiling" }).click();
  await expect(page.getByText("A ceiling is set for this tenant.")).toBeVisible();

  await page.reload();
  await expect(page.getByLabel("File transfer", { exact: true })).toHaveValue("Upload");
  await expect(page.getByLabel("Largest file (MiB)")).toHaveValue("10");
});
```

Run:

```bash
cd web && npm run generate && npm run typecheck && npm run lint:strings && npx vitest run tests/unit/admin && npx playwright test tests/session-policy.spec.ts
```

Expected: PASS (vitest and the whole `session-policy.spec.ts`, including the flipped assertion).

- [ ] **Step 8: Commit**

```bash
make manifests generate sync-chart-crds
go build ./... && go vet ./... && go test -count=1 ./api/... ./internal/provisioning/ ./internal/api/ ./internal/backend/ ./internal/operator/ ./internal/runtime/... ./deploy/helm/
git add api/v1 config/crd deploy/helm internal web
git commit -m "feat(policy): resolve the file-transfer channel and bound into the session policy

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 8: `session-policy:apply` restarts running workspaces that are looser than the ceiling (F3) — tier: integrate

**Files:**
- Modify: `internal/provisioning/service.go` (`WorkspaceRecord.RuntimeGeneration`, shared `workspaceSelectCols`, `scanWorkspace`, new `ListRunningWorkspaces`, and `RestartWorkspace` from Task 6a), `internal/provisioning/restart_test.go`
- Modify: `internal/api/statusview.go` (add `RunningPolicies`), `internal/api/statusview_test.go`
- Create: `internal/api/adminsessionpolicyapply.go`, `internal/api/adminsessionpolicyapply_test.go`
- Modify: `internal/api/{adminsessionpolicy.go,auditroutes.go,openapi.yaml,auditroutes_test.go,openapi_contract_test.go,tenantadmin_test.go}`, `internal/observability/metrics.go` (`auditEventActions`, ~line 122), `internal/backend/wire.go`
- Create: `web/src/admin/ApplyPolicyDialog.tsx`
- Modify: `web/src/admin/{SessionPolicyPage.tsx,api.ts}`, `web/src/i18n/{en,vi}/admin.ts`, `web/tests/mock-api/admin.ts`, `web/tests/session-policy.spec.ts`
- Modify: `hack/quickstart/smoke/smoke.spec.ts` (one `:apply` test; the `demo-admin` realm user, `ADMIN_USER` and `TENANT` come from Task 4)
- Modify: `docs/security/threat-model.md` (row S47), `docs/runbooks/session-policy.md` ("When does a change take effect?")

**Interfaces:**
- Consumes: `Service.RestartWorkspace(ctx, tenantID, caller, ownerScope, wsID, idemKey string, fromGeneration int64, bodyHash []byte) (WorkspaceRecord, error)` (Task 6a; it no-ops and returns the current record when the row's generation is no longer `fromGeneration`; `idemKey ""` skips idempotency; it re-stamps the resolved session policy on the start intent), the `IntentSettled` rule of `api.ObservedStatus` (Task 6a: `status.lastAppliedIntentRevision == spec.intentRevision`), `Service.ResolveSessionPolicy` and `provisioning.PolicyWithin` (Task 7), `store.DB.HasLiveLease(ctx, workspaceID, now)` (Task 4), the audit viewer `GET /v1/admin/tenants/{tenant}/audit?action=` (Task 2, used by the smoke), the `demo-admin` quickstart user with `ADMIN_USER`/`TENANT` smoke constants (Task 4), `SessionPolicySource.Ceiling`, `CheckIfMatch`, `ETagOf`, `resolveOwners`.
- Produces:
  - `func (s *provisioning.Service) ListRunningWorkspaces(ctx context.Context, tenantID string, limit int) ([]provisioning.WorkspaceRecord, error)` (one query, oldest first, tenant-scoped); `provisioning.WorkspaceRecord.RuntimeGeneration int64` (`json:"-"`)
  - `type api.AppliedPolicy struct{ Policy provisioning.SessionPolicy; RuntimeGeneration int64; IntentSettled bool }`, `func (v *K8sStatusView) RunningPolicies(ctx context.Context, tenantID string) (map[string]api.AppliedPolicy, bool, error)` (the bool is freshness)
  - `func (h *api.AdminSessionPolicyHandler) WithApply(be PolicyApplyBackend, running RunningPolicySource, dir Directory) *AdminSessionPolicyHandler`
  - `POST /v1/admin/tenants/{tenant}/session-policy:apply` with body `{"dryRun": bool, "includeActive": bool}` and `If-Match` on the policy ETag. It returns `{dryRun, truncated, affected: [{workspaceId, name, owner, applied, effective, live, result}]}`, where `result` is one of `pending`, `restarted`, `skipped_active`, `skipped_moved` or `failed`. The audit action is `admin.session_policy.apply`.
  - `const api.maxPolicyApply = 500` (affected rows per call), `const api.maxPolicyScan = 5000` (running workspaces examined per call)
  - Web: `applyAdminSessionPolicy(api, tenant, body, etag)`, `<ApplyPolicyDialog>`.

- [ ] **Step 1: `RuntimeGeneration` on the record and `ListRunningWorkspaces`, test first**

Append to `internal/provisioning/restart_test.go` (`runningWorkspace`, `generationOf` and `recoveryDB` come from Task 6a's `restart_test.go` and `recovery_test.go`):

```go
func TestListRunningWorkspaces_TenantScopedRunningOnly(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	svc := provisioning.NewService(db)
	a := runningWorkspace(t, db, svc, "tenant-l", "l1")
	b := runningWorkspace(t, db, svc, "tenant-l", "l2")
	runningWorkspace(t, db, svc, "tenant-other", "o1")
	if _, err := svc.SignalWorkspace(ctx, "tenant-l", "iss|sub", "", b.ID, "key-l-stop", provisioning.IntentStop, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	got, err := svc.ListRunningWorkspaces(ctx, "tenant-l", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("running = %+v, want only %s", got, a.ID)
	}
	if want := generationOf(t, db, a.ID); got[0].RuntimeGeneration != want {
		t.Fatalf("RuntimeGeneration = %d, want %d", got[0].RuntimeGeneration, want)
	}
	capped, _ := svc.ListRunningWorkspaces(ctx, "tenant-l", 0)
	if len(capped) != 0 {
		t.Fatal("limit 0 returns nothing")
	}
}
```

In `service.go`, `getWorkspace` (`:709`) and the list query (`:370`) each spell the same 20-column projection, and both feed `scanWorkspace`. Hoist it into one constant, append `runtime_generation` as its last column, and use it in both plus the new method:

```go
// workspaceSelectCols is the projection scanWorkspace reads (keep the
// order in step with it).
const workspaceSelectCols = `id, tenant_id, owner_subject, owner_issuer, owner_sub, name,
		template, desired_state, data_policy, phase, COALESCE(failure_reason, ''),
		COALESCE(retained_data_ref, ''), request_id, intent_revision, created_at, updated_at,
		COALESCE((SELECT running_slots FROM quota_reservation WHERE workspace_id = workspaces.id AND state = 'held'), 0),
		COALESCE((SELECT cpu_millis FROM quota_reservation WHERE workspace_id = workspaces.id AND state = 'held'), 0),
		COALESCE((SELECT memory_bytes FROM quota_reservation WHERE workspace_id = workspaces.id AND state = 'held'), 0),
		COALESCE((SELECT disk_bytes FROM quota_reservation WHERE workspace_id = workspaces.id AND state = 'held'), 0),
		runtime_generation`
```

`getWorkspace` becomes `query := "SELECT " + workspaceSelectCols + " FROM workspaces WHERE id = $1 AND tenant_id = $2"`; the list query becomes `q := "SELECT " + workspaceSelectCols + " FROM workspaces WHERE " + strings.Join(conds, " AND ") + ks.orderLimit`. Add `RuntimeGeneration int64 \`json:"-"\`` to `WorkspaceRecord` (after `UpdatedAt`) and one more scan target at the end of `scanWorkspace`: `, &rec.RuntimeGeneration`. In Task 6a's `RestartWorkspace`, drop the separate `SELECT runtime_generation FROM workspaces` query and its `gen` variable: use `rec.RuntimeGeneration` (the row is already read `FOR UPDATE` by `getWorkspaceTx` in the same transaction), including in `s.revokeGeneration(ctx, tx, wsID, rec.RuntimeGeneration)`.

Then add the method:

```go
// ListRunningWorkspaces returns up to limit active workspaces of tenantID
// whose desired state is Running, oldest first, in ONE query with the same
// projection as GetWorkspace (F3 policy apply). Tenant scoping is in the
// WHERE clause; a workspace being deleted is excluded.
func (s *Service) ListRunningWorkspaces(ctx context.Context, tenantID string, limit int) ([]WorkspaceRecord, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.Pool().Query(ctx, `
		SELECT `+workspaceSelectCols+` FROM workspaces
		WHERE tenant_id = $1 AND state = 'active' AND desired_state = 'Running' AND phase <> 'Terminating'
		ORDER BY created_at, id LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("list running: %w", err)
	}
	defer rows.Close()
	var out []WorkspaceRecord
	for rows.Next() {
		rec, err := scanWorkspace(rows)
		if err != nil {
			return nil, fmt.Errorf("list running: %w", err)
		}
		out = append(out, *rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list running: %w", err)
	}
	return out, nil
}
```

Run `go test ./internal/provisioning/ -run 'TestListRunningWorkspaces|TestRestartWorkspace' -count=1`. Expected: PASS (the `RestartWorkspace` tests prove the Task 6a change is behaviour-neutral).

- [ ] **Step 2: `RunningPolicies` on the status view**

Append to `statusview.go`:

```go
// AppliedPolicy is what a running Workspace CR was built with: the session
// policy the applier stamped (spec.sessionPolicy), the generation it
// belongs to, and whether the operator has applied the CR's latest intent
// (status.lastAppliedIntentRevision == spec.intentRevision, the rule behind
// ObservedStatus.IntentSettled from Task 6a). An unsettled CR is mid-change:
// a restart or stop/start is applied to the spec but not reconciled yet.
type AppliedPolicy struct {
	Policy            provisioning.SessionPolicy
	RuntimeGeneration int64
	IntentSettled     bool
}

// RunningPolicies maps each Running-desired Workspace CR of tenantID
// (keyed by platform workspace id) to its applied policy. fresh is false
// when the watch is stale; callers must not act on a stale map.
func (v *K8sStatusView) RunningPolicies(ctx context.Context, tenantID string) (map[string]AppliedPolicy, bool, error) {
	v.mu.Lock()
	synced, last := v.synced, v.lastEvent
	v.mu.Unlock()
	if !synced || v.now().Sub(last) > v.maxStale {
		return nil, false, nil
	}
	var list cdiv1.WorkspaceList
	if err := v.cache.List(ctx, &list, client.MatchingLabels{provisioning.LabelTenant: tenantID}); err != nil {
		return nil, false, err
	}
	out := make(map[string]AppliedPolicy, len(list.Items))
	for i := range list.Items {
		ws := &list.Items[i]
		id := ws.Labels[provisioning.LabelWorkspaceUID]
		if id == "" || ws.Spec.DesiredState != cdiv1.DesiredStateRunning || ws.DeletionTimestamp != nil {
			continue
		}
		sp := ws.Spec.SessionPolicy
		out[id] = AppliedPolicy{
			Policy: provisioning.SessionPolicy{
				Clipboard: string(sp.Clipboard), FileTransfer: sp.FileTransfer, MaxFileBytes: sp.MaxFileBytes,
			},
			RuntimeGeneration: ws.Spec.RuntimeGeneration,
			IntentSettled:     ws.Status.LastAppliedIntentRevision == ws.Spec.IntentRevision,
		}
	}
	return out, true, nil
}
```

The existing status-view tests build the view without an informer (`&K8sStatusView{now: time.Now, maxStale: ..., lastKnown: ...}`); no cache fake exists in `internal/api` (the only cache-backed test is the envtest one). Add this to `statusview_test.go` (add the import `"sigs.k8s.io/controller-runtime/pkg/client/fake"`; `runtime`, `metav1`, `crcache`, `client`, `cdiv1`, `provisioning` are already imported there):

```go
// policyListCache serves only List from a fake client: RunningPolicies
// reads nothing else from the cache, so the embedded nil Cache is never hit.
type policyListCache struct {
	crcache.Cache
	c client.Client
}

func (p policyListCache) List(ctx context.Context, l client.ObjectList, opts ...client.ListOption) error {
	return p.c.List(ctx, l, opts...)
}

func TestK8sStatusView_RunningPolicies(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cdiv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	mk := func(name, tenant, uid string, desired cdiv1.DesiredState, clip cdiv1.ClipboardPolicy, gen int64) *cdiv1.Workspace {
		return &cdiv1.Workspace{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "ns-" + tenant,
				Labels: map[string]string{provisioning.LabelTenant: tenant, provisioning.LabelWorkspaceUID: uid},
			},
			Spec: cdiv1.WorkspaceSpec{
				DesiredState: desired, RuntimeGeneration: gen, IntentRevision: 4,
				SessionPolicy: cdiv1.SessionPolicySpec{
					Clipboard: clip, FileTransfer: cdiv1.FileTransferUpload, MaxFileBytes: 4096,
				},
			},
			Status: cdiv1.WorkspaceStatus{LastAppliedIntentRevision: 4},
		}
	}
	unsettled := mk("d", "tenant-a", "ws_d", cdiv1.DesiredStateRunning, cdiv1.ClipboardSend, 2)
	unsettled.Status.LastAppliedIntentRevision = 3 // the operator has not reconciled revision 4 yet
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		unsettled,
		mk("a", "tenant-a", "ws_a", cdiv1.DesiredStateRunning, cdiv1.ClipboardBidirectional, 3),
		mk("b", "tenant-a", "ws_b", cdiv1.DesiredStateStopped, cdiv1.ClipboardSend, 1),
		mk("c", "tenant-b", "ws_c", cdiv1.DesiredStateRunning, cdiv1.ClipboardSend, 1),
	).Build()
	sv := &K8sStatusView{
		cache: policyListCache{c: c}, now: time.Now, maxStale: time.Minute,
		lastKnown: map[string]ObservedStatus{},
	}
	sv.MarkSynced()

	got, fresh, err := sv.RunningPolicies(context.Background(), "tenant-a")
	if err != nil || !fresh {
		t.Fatalf("fresh=%v err=%v", fresh, err)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v, want ws_a and ws_d (Stopped and other-tenant CRs excluded)", got)
	}
	a := got["ws_a"]
	if a.Policy.Clipboard != "Bidirectional" || a.Policy.FileTransfer != cdiv1.FileTransferUpload ||
		a.Policy.MaxFileBytes != 4096 || a.RuntimeGeneration != 3 || !a.IntentSettled {
		t.Fatalf("ws_a = %+v", a)
	}
	if got["ws_d"].IntentSettled {
		t.Fatalf("ws_d = %+v, want IntentSettled=false (revision 3 applied, 4 wanted)", got["ws_d"])
	}

	stale := &K8sStatusView{cache: policyListCache{c: c}, now: time.Now, maxStale: time.Minute, lastKnown: map[string]ObservedStatus{}}
	if _, fresh, _ := stale.RunningPolicies(context.Background(), "tenant-a"); fresh {
		t.Fatal("an unsynced view must not report fresh")
	}
}
```

Run `go test ./internal/api/ -run TestK8sStatusView_RunningPolicies -count=1`. Expected: PASS.

- [ ] **Step 3: Contract and failing handler tests**

Apply the New API operation checklist for `routeAdminSessionPolicyApply = "POST /v1/admin/tenants/{tenant}/session-policy:apply"` (`auditActionAdminSessionPolicyApply = "admin.session_policy.apply"`, target key `"tenant"`):
- `auditroutes.go`: add the two constants (beside `auditActionAdminSessionPolicyClear` and `routeAdminSessionPolicyPut`) and `routeAdminSessionPolicyApply: {auditActionAdminSessionPolicyApply, "tenant"},` to `auditedRoutes`;
- `internal/observability/metrics.go`: add `"admin.session_policy.apply": {}` to `auditEventActions` (~line 122);
- the literal last segment `session-policy:apply` registers on `ServeMux` as is; no `ActionRouter` is involved.

OpenAPI (a new path next to `/v1/admin/tenants/{tenant}/session-policy`, `operationId: applyAdminSessionPolicy`, `tags: [admin]`, parameters `Tenant` and `IfMatch`, a required `application/json` body of `ApplySessionPolicyRequest`; the description says: dry run first, the confirmed call restarts through the generation-fenced restart, live sessions are skipped unless `includeActive`, at most 500 affected workspaces per call, tenant-admin role, CSRF, `If-Match` on the policy ETag):

```yaml
    ApplySessionPolicyRequest:
      type: object
      additionalProperties: false
      required: [dryRun]
      properties:
        dryRun: {type: boolean, description: "true lists the affected workspaces without restarting them."}
        includeActive: {type: boolean, default: false, description: "Also restart workspaces with a live desktop session."}
    ApplySessionPolicyResult:
      type: object
      additionalProperties: false
      required: [dryRun, truncated, affected]
      properties:
        dryRun: {type: boolean}
        truncated: {type: boolean, description: "More workspaces are affected (or running) than one call handles; only the first 500 affected are listed. Call again after they restart."}
        affected:
          type: array
          items:
            type: object
            additionalProperties: false
            required: [workspaceId, name, owner, applied, effective, live, result]
            properties:
              workspaceId: {type: string}
              name: {type: string}
              owner: {$ref: '#/components/schemas/Owner'}
              applied: {$ref: '#/components/schemas/ResolvedSessionPolicy'}
              effective: {$ref: '#/components/schemas/ResolvedSessionPolicy'}
              live: {type: boolean}
              result: {type: string, enum: [pending, restarted, skipped_active, skipped_moved, failed]}
```

Responses: `200` (`ApplySessionPolicyResult`), `400` BadRequest, `401` Unauthorized, `403` Forbidden, `409` Conflict (no ceiling row), `412` PreconditionFailed, `428` PreconditionRequired, `429` RateLimited, `500` InternalError, `503` Unavailable (stale workspace status). Also, in the `putAdminSessionPolicy` description, replace "while a running workspace's server-side clipboard enforcement follows at its next start — pod specs are immutable" with the same sentence plus "; `POST .../session-policy:apply` restarts the running workspaces that are looser than the ceiling".

`internal/api/adminsessionpolicyapply_test.go` (package `api`; `fakeSessionPolicySource`, `newSessionPolicyEnv`, `adminSessionPolicyPath`, `loginAdmin`, `login`, `doReq`, `decodeBody`, `decodeErr` are the existing helpers; `doReq` dereferences the CSRF cookie, so a missing token is `&http.Cookie{Value: "wrong-token"}`, never nil):

```go
type fakeApplyBackend struct {
	running   []provisioning.WorkspaceRecord
	effective provisioning.SessionPolicy
	live      map[string]bool
	restarted map[string]int64
	// movedIDs are workspaces whose generation already moved: RestartWorkspace
	// no-ops and returns the current (moved) record.
	movedIDs map[string]bool
}

func (f *fakeApplyBackend) ListRunningWorkspaces(_ context.Context, _ string, limit int) ([]provisioning.WorkspaceRecord, error) {
	if len(f.running) > limit {
		return f.running[:limit], nil
	}
	return f.running, nil
}

func (f *fakeApplyBackend) ResolveSessionPolicy(context.Context, string, provisioning.TemplateCatalogEntry) (provisioning.SessionPolicy, error) {
	return f.effective, nil
}

func (f *fakeApplyBackend) RestartWorkspace(_ context.Context, _, _, _, id, _ string, gen int64, _ []byte) (provisioning.WorkspaceRecord, error) {
	if f.movedIDs[id] {
		return provisioning.WorkspaceRecord{ID: id, RuntimeGeneration: gen + 7}, nil
	}
	f.restarted[id] = gen
	return provisioning.WorkspaceRecord{ID: id, RuntimeGeneration: gen + 1}, nil
}

func (f *fakeApplyBackend) HasLiveLease(_ context.Context, id string, _ time.Time) (bool, error) {
	return f.live[id], nil
}

type fakeRunningPolicies map[string]AppliedPolicy

func (f fakeRunningPolicies) RunningPolicies(context.Context, string) (map[string]AppliedPolicy, bool, error) {
	return f, true, nil
}

func applyFixture() (*fakeApplyBackend, fakeRunningPolicies) {
	tight := provisioning.SessionPolicy{Clipboard: "Disabled", FileTransfer: "Disabled"}
	loose := provisioning.SessionPolicy{Clipboard: "Bidirectional", FileTransfer: "Disabled"}
	be := &fakeApplyBackend{
		effective: tight,
		live:      map[string]bool{"ws_live": true},
		restarted: map[string]int64{},
		running: []provisioning.WorkspaceRecord{
			{ID: "ws_loose", Name: "a", Owner: "https://idp|alice"},
			{ID: "ws_live", Name: "b", Owner: "https://idp|bob"},
			{ID: "ws_ok", Name: "c", Owner: "https://idp|carol"},
		},
	}
	cr := fakeRunningPolicies{
		"ws_loose": {Policy: loose, RuntimeGeneration: 3, IntentSettled: true},
		"ws_live":  {Policy: loose, RuntimeGeneration: 5, IntentSettled: true},
		"ws_ok":    {Policy: tight, RuntimeGeneration: 1, IntentSettled: true},
	}
	return be, cr
}

// newApplyEnv mounts the session-policy routes over a ceiling row
// (version v1, clipboard Disabled) with the apply seam wired to the fakes,
// and returns the row's ETag.
func newApplyEnv(t *testing.T, be *fakeApplyBackend, cr fakeRunningPolicies) (*testEnv, string) {
	t.Helper()
	src := &fakeSessionPolicySource{hasRow: true, version: 1, clip: "Disabled"}
	env := newSessionPolicyEnv(t, src, func(h *AdminSessionPolicyHandler) { h.WithApply(be, cr, nil) })
	return env, ETagOf("v1")
}

const applyPath = adminSessionPolicyPath + ":apply"

type applyResultJSON struct {
	DryRun    bool `json:"dryRun"`
	Truncated bool `json:"truncated"`
	Affected  []struct {
		WorkspaceID string `json:"workspaceId"`
		Live        bool   `json:"live"`
		Result      string `json:"result"`
	} `json:"affected"`
}

func TestApplySessionPolicy_DryRunListsLooserOnly(t *testing.T) {
	be, cr := applyFixture()
	env, etag := newApplyEnv(t, be, cr)
	sess, csrf := loginAdmin(t, env, "admin-a")
	res := doReq(t, env, sess, csrf, http.MethodPost, applyPath, `{"dryRun":true}`, map[string]string{"If-Match": etag})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	out := decodeBody[applyResultJSON](t, res)
	if len(out.Affected) != 2 || len(be.restarted) != 0 {
		t.Fatalf("affected=%+v restarted=%v; dry run lists ws_loose and ws_live and restarts nothing", out.Affected, be.restarted)
	}
	for _, a := range out.Affected {
		if a.Result != "pending" {
			t.Fatalf("dry-run result %q", a.Result)
		}
	}
}

func TestApplySessionPolicy_ConfirmSkipsLiveUnlessIncluded(t *testing.T) {
	be, cr := applyFixture()
	env, etag := newApplyEnv(t, be, cr)
	sess, csrf := loginAdmin(t, env, "admin-a")
	res := doReq(t, env, sess, csrf, http.MethodPost, applyPath, `{"dryRun":false}`, map[string]string{"If-Match": etag})
	out := decodeBody[applyResultJSON](t, res)
	results := map[string]string{}
	for _, a := range out.Affected {
		results[a.WorkspaceID] = a.Result
	}
	if results["ws_loose"] != "restarted" || results["ws_live"] != "skipped_active" || be.restarted["ws_loose"] != 3 {
		t.Fatalf("results=%v restarted=%v", results, be.restarted)
	}
	if _, ok := be.restarted["ws_live"]; ok {
		t.Fatal("a live session was restarted without includeActive")
	}
	res = doReq(t, env, sess, csrf, http.MethodPost, applyPath, `{"dryRun":false,"includeActive":true}`, map[string]string{"If-Match": etag})
	res.Body.Close()
	if res.StatusCode != http.StatusOK || be.restarted["ws_live"] != 5 {
		t.Fatalf("includeActive: status %d restarted=%v", res.StatusCode, be.restarted)
	}
}

// A workspace whose generation moved before our restart (another restart
// won) is reported skipped_moved, not restarted.
func TestApplySessionPolicy_MovedGeneration(t *testing.T) {
	be, cr := applyFixture()
	be.movedIDs = map[string]bool{"ws_loose": true}
	env, etag := newApplyEnv(t, be, cr)
	sess, csrf := loginAdmin(t, env, "admin-a")
	res := doReq(t, env, sess, csrf, http.MethodPost, applyPath, `{"dryRun":false}`, map[string]string{"If-Match": etag})
	out := decodeBody[applyResultJSON](t, res)
	results := map[string]string{}
	for _, a := range out.Affected {
		results[a.WorkspaceID] = a.Result
	}
	if results["ws_loose"] != "skipped_moved" {
		t.Fatalf("results=%v, want ws_loose skipped_moved", results)
	}
	if _, ok := be.restarted["ws_loose"]; ok {
		t.Fatal("a moved workspace was restarted")
	}
}

// A CR whose latest intent the operator has not reconciled is mid-change
// (an earlier restart or stop/start): listed, never restarted again.
func TestApplySessionPolicy_UnsettledIntentIsNotRestarted(t *testing.T) {
	be, cr := applyFixture()
	u := cr["ws_loose"]
	u.IntentSettled = false
	cr["ws_loose"] = u
	env, etag := newApplyEnv(t, be, cr)
	sess, csrf := loginAdmin(t, env, "admin-a")
	res := doReq(t, env, sess, csrf, http.MethodPost, applyPath, `{"dryRun":false,"includeActive":true}`, map[string]string{"If-Match": etag})
	out := decodeBody[applyResultJSON](t, res)
	results := map[string]string{}
	for _, a := range out.Affected {
		results[a.WorkspaceID] = a.Result
	}
	if results["ws_loose"] != "skipped_moved" || results["ws_live"] != "restarted" {
		t.Fatalf("results=%v, want ws_loose skipped_moved and ws_live restarted", results)
	}
	if _, ok := be.restarted["ws_loose"]; ok {
		t.Fatal("an unsettled workspace was restarted")
	}
}

func TestApplySessionPolicy_TruncatesAtCap(t *testing.T) {
	be, cr := applyFixture()
	be.running = nil
	for i := 0; i < maxPolicyApply+5; i++ {
		id := fmt.Sprintf("ws_%04d", i)
		be.running = append(be.running, provisioning.WorkspaceRecord{ID: id, Name: id, Owner: "https://idp|u"})
		cr[id] = AppliedPolicy{Policy: provisioning.SessionPolicy{Clipboard: "Bidirectional", FileTransfer: "Disabled"}, RuntimeGeneration: 1, IntentSettled: true}
	}
	env, etag := newApplyEnv(t, be, cr)
	sess, csrf := loginAdmin(t, env, "admin-a")
	res := doReq(t, env, sess, csrf, http.MethodPost, applyPath, `{"dryRun":true}`, map[string]string{"If-Match": etag})
	out := decodeBody[applyResultJSON](t, res)
	if len(out.Affected) != maxPolicyApply || !out.Truncated {
		t.Fatalf("affected=%d truncated=%v, want %d/true", len(out.Affected), out.Truncated, maxPolicyApply)
	}
}

func TestApplySessionPolicy_Preconditions(t *testing.T) {
	be, cr := applyFixture()
	env, _ := newApplyEnv(t, be, cr)
	sess, csrf := loginAdmin(t, env, "admin-a")
	if res := doReq(t, env, sess, csrf, http.MethodPost, applyPath, `{"dryRun":true}`, nil); res.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("no If-Match: %d", res.StatusCode)
	}
	if res := doReq(t, env, sess, csrf, http.MethodPost, applyPath, `{"dryRun":true}`, map[string]string{"If-Match": `"stale"`}); res.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match: %d", res.StatusCode)
	}
	if res := doReq(t, env, sess, csrf, http.MethodPost, applyPath, `{"dryRun":"yes"}`, map[string]string{"If-Match": `"x"`}); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad body: %d", res.StatusCode)
	}
	bad := doReq(t, env, sess, &http.Cookie{Value: "wrong-token"}, http.MethodPost, applyPath, `{"dryRun":false}`, map[string]string{"If-Match": ETagOf("v1")})
	if e := decodeErr(t, bad); bad.StatusCode != http.StatusForbidden || e.Code != CodeCSRFFailed {
		t.Fatalf("no CSRF: status=%d code=%q, want 403 CSRF_FAILED", bad.StatusCode, e.Code)
	}
	if len(be.restarted) != 0 {
		t.Fatalf("a refused call restarted %v", be.restarted)
	}
}

// Tenant isolation and role: a regular user and an admin of another tenant
// are refused before any workspace is read or restarted.
func TestApplySessionPolicy_AuthzAndTenantIsolation(t *testing.T) {
	be, cr := applyFixture()
	env, etag := newApplyEnv(t, be, cr)
	sessU, csrfU := login(t, env, "user-a")
	res := doReq(t, env, sessU, csrfU, http.MethodPost, applyPath, `{"dryRun":false}`, map[string]string{"If-Match": etag})
	if e := decodeErr(t, res); res.StatusCode != http.StatusForbidden || e.Code != CodeForbidden {
		t.Fatalf("user: status=%d code=%q", res.StatusCode, e.Code)
	}
	sessA, csrfA := loginAdmin(t, env, "admin-a")
	res = doReq(t, env, sessA, csrfA, http.MethodPost, "/v1/admin/tenants/tenant-b/session-policy:apply",
		`{"dryRun":false}`, map[string]string{"If-Match": etag})
	if e := decodeErr(t, res); res.StatusCode != http.StatusForbidden || e.Code != CodeForbidden {
		t.Fatalf("other tenant: status=%d code=%q", res.StatusCode, e.Code)
	}
	if len(be.restarted) != 0 {
		t.Fatalf("a refused call restarted %v", be.restarted)
	}
}
```

The file's imports: `context`, `fmt`, `net/http`, `testing`, `time` and `internal/provisioning`.

Wire the other test mounts:
- `auditroutes_test.go`, `newCoverageEnv` (`:241`): change the mount to `MountAdminSessionPolicyRoutes(mux, a, NewAdminSessionPolicyHandler(&fakeSessionPolicySource{}, tenants).WithAuditSink(sink).WithApply(&fakeApplyBackend{restarted: map[string]int64{}, live: map[string]bool{}}, fakeRunningPolicies{}, nil))`. In `TestAuditCoverage_MountedMuxEmitsEventPerRoute`, add right after the `PUT .../session-policy` driver (the PUT driver creates the row, version `v1`; the order matters):

```go
		{"POST /v1/admin/tenants/{}/session-policy:apply", auditActionAdminSessionPolicyApply, func() {
			doReq(t, env, sessA, csrfA, http.MethodPost, adminSessionPolicyPath+":apply",
				`{"dryRun":true}`, map[string]string{"If-Match": ETagOf("v1")}).Body.Close()
		}},
```

- `openapi_contract_test.go`, `TestOpenAPIContract_ResponsesValidate`: after the two `PUT .../session-policy` entries (success with `star` creates `v1`, then `stale`), add a success and a refusal:

```go
		{"POST", "/v1/admin/tenants/{tenant}/session-policy:apply", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "POST", adminSessionPolicyPath+":apply", `{"dryRun":true}`, map[string]string{"If-Match": ETagOf("v1")})
		}},
		{"POST", "/v1/admin/tenants/{tenant}/session-policy:apply", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "POST", adminSessionPolicyPath+":apply", `{"dryRun":true}`, stale)
		}},
```

- `tenantadmin_test.go`: the `newAdminGroupEnv` mount (`:81`) already registers the route (it lives in `MountAdminSessionPolicyRoutes`); extend `TestAdminSessionPolicy_CustomAdminGroups` (`:260`) with a non-member `POST adminSessionPolicyPath+":apply"` (`{"dryRun":true}`, `If-Match: *`) that must answer 403.

Run `go test ./internal/api/ -run 'ApplySessionPolicy|AuditCoverage|OpenAPI|CustomAdminGroups' -count=1`. Expected: FAIL to compile (`WithApply`, `maxPolicyApply` and the route constants do not exist yet).

- [ ] **Step 4: Implement `adminsessionpolicyapply.go`**

```go
package api

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

const (
	// maxPolicyApply bounds the affected rows one :apply call lists and
	// restarts; the response says truncated when more qualify.
	maxPolicyApply = 500
	// maxPolicyScan bounds the running workspaces one call examines (one
	// query). Compliant workspaces never become affected, so the scan must
	// not stop at the first 500 running ones: oldest-first paging by running
	// count alone could hide looser workspaces behind a compliant prefix.
	maxPolicyScan = 5000
)

// PolicyApplyBackend is the provisioning and store surface :apply uses.
type PolicyApplyBackend interface {
	ListRunningWorkspaces(ctx context.Context, tenantID string, limit int) ([]provisioning.WorkspaceRecord, error)
	ResolveSessionPolicy(ctx context.Context, tenantID string, tpl provisioning.TemplateCatalogEntry) (provisioning.SessionPolicy, error)
	RestartWorkspace(ctx context.Context, tenantID, caller, ownerScope, wsID, idemKey string, fromGeneration int64, bodyHash []byte) (provisioning.WorkspaceRecord, error)
	HasLiveLease(ctx context.Context, workspaceID string, now time.Time) (bool, error)
}

// RunningPolicySource is the informer view of applied policies (*K8sStatusView).
type RunningPolicySource interface {
	RunningPolicies(ctx context.Context, tenantID string) (map[string]AppliedPolicy, bool, error)
}

type applyRequest struct {
	DryRun        *bool `json:"dryRun"`
	IncludeActive bool  `json:"includeActive"`
}

type applyAffected struct {
	WorkspaceID string                     `json:"workspaceId"`
	Name        string                     `json:"name"`
	Owner       Owner                      `json:"owner"`
	Applied     provisioning.SessionPolicy `json:"applied"`
	Effective   provisioning.SessionPolicy `json:"effective"`
	Live        bool                       `json:"live"`
	Result      string                     `json:"result"`
}

type applyResult struct {
	DryRun    bool            `json:"dryRun"`
	Truncated bool            `json:"truncated"`
	Affected  []applyAffected `json:"affected"`
}

// WithApply enables POST …/session-policy:apply. dir resolves owner display
// names (nil degrades to the subject).
func (h *AdminSessionPolicyHandler) WithApply(be PolicyApplyBackend, running RunningPolicySource, dir Directory) *AdminSessionPolicyHandler {
	h.apply, h.running, h.dir = be, running, dir
	return h
}

// Apply restarts the tenant's running workspaces whose applied session
// policy grants more than the current ceiling allows (F3). dryRun lists
// them; the confirmed call restarts each through RestartWorkspace, fenced
// on the generation the CR reports, skipping live sessions unless
// includeActive. If-Match pins the ceiling version the admin reviewed.
// Tenant isolation: the tenant comes from the verified principal (the path
// value must equal it) and every query below is scoped to it.
func (h *AdminSessionPolicyHandler) Apply(w http.ResponseWriter, r *http.Request) {
	p, ok := tenantAdminPrincipal(h.tenants, w, r)
	if !ok {
		return
	}
	if h.apply == nil || h.running == nil {
		writeError(w, r, CodeUnavailable, "policy apply unavailable")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
	if err != nil {
		writeError(w, r, CodeInvalidRequest, "unreadable or oversized body")
		return
	}
	var req applyRequest
	if decodeJSON(body, &req) != nil || req.DryRun == nil {
		writeError(w, r, CodeInvalidRequest, "body must be {\"dryRun\": bool, \"includeActive\"?: bool}")
		return
	}
	ctx := r.Context()
	tenant := p.TenantID
	cur, err := h.source.Ceiling(ctx, tenant)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if e := CheckIfMatch(r, cur.Version, cur.HasRow); e != nil {
		WriteError(w, r, e)
		return
	}
	if !cur.HasRow {
		writeError(w, r, CodeInvalidState, "no ceiling is set; every workspace already follows its template")
		return
	}
	applied, fresh, err := h.running.RunningPolicies(ctx, tenant)
	if err != nil || !fresh {
		writeError(w, r, CodeUnavailable, "workspace status is out of date; retry shortly")
		return
	}
	recs, err := h.apply.ListRunningWorkspaces(ctx, tenant, maxPolicyScan+1)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	out := applyResult{DryRun: *req.DryRun, Affected: []applyAffected{}}
	if len(recs) > maxPolicyScan {
		out.Truncated = true
		recs = recs[:maxPolicyScan]
	}
	// One resolution per distinct template policy pair: the ceiling is read
	// once per pair, and every workspace of a pair is judged against the
	// same value.
	type tplKey struct{ clipboard, fileTransfer string }
	effective := map[tplKey]provisioning.SessionPolicy{}
	now := time.Now()
	restarted := 0
	for _, rec := range recs {
		cr, ok := applied[rec.ID]
		if !ok {
			continue // no CR yet: the create intent stamps the current policy
		}
		k := tplKey{rec.Template.ClipboardPolicy, rec.Template.FileTransferPolicy}
		eff, seen := effective[k]
		if !seen {
			eff, err = h.apply.ResolveSessionPolicy(ctx, tenant, provisioning.TemplateCatalogEntry{
				ClipboardPolicy: k.clipboard, FileTransferPolicy: k.fileTransfer,
			})
			if err != nil {
				writeStoreError(w, r, err)
				return
			}
			effective[k] = eff
		}
		if provisioning.PolicyWithin(cr.Policy, eff) {
			continue
		}
		if len(out.Affected) == maxPolicyApply {
			out.Truncated = true
			break
		}
		live, err := h.apply.HasLiveLease(ctx, rec.ID, now)
		if err != nil {
			writeStoreError(w, r, err)
			return
		}
		a := applyAffected{
			WorkspaceID: rec.ID, Name: rec.Name,
			Owner:   Owner{Subject: ownerRefSubject(rec.Owner), DisplayName: ownerRefSubject(rec.Owner)},
			Applied: cr.Policy, Effective: eff, Live: live, Result: "pending",
		}
		switch {
		case out.DryRun:
		case !cr.IntentSettled:
			// A restart or stop/start is applied to the CR but the operator has
			// not reconciled it: the generation is already moving.
			a.Result = "skipped_moved"
		case live && !req.IncludeActive:
			a.Result = "skipped_active"
		default:
			got, err := h.apply.RestartWorkspace(ctx, tenant, p.Owner(), "", rec.ID, "", cr.RuntimeGeneration, nil)
			switch {
			case err != nil:
				a.Result = "failed"
			case got.RuntimeGeneration != cr.RuntimeGeneration+1:
				a.Result = "skipped_moved" // another restart moved the generation first
			default:
				a.Result = "restarted"
				restarted++
			}
		}
		out.Affected = append(out.Affected, a)
	}
	// Real display names for the (at most 500) owners, one directory lookup.
	refs := make([]string, 0, len(out.Affected))
	byRef := make(map[string]string, len(out.Affected))
	for _, rec := range recs {
		byRef[rec.ID] = rec.Owner
	}
	for _, a := range out.Affected {
		refs = append(refs, byRef[a.WorkspaceID])
	}
	owners := resolveOwners(ctx, h.dir, tenant, refs)
	for i := range out.Affected {
		out.Affected[i].Owner = owners[byRef[out.Affected[i].WorkspaceID]]
	}
	auditSetDetail(ctx, "dryRun", strconv.FormatBool(out.DryRun))
	auditSetDetail(ctx, "includeActive", strconv.FormatBool(req.IncludeActive))
	auditSetDetail(ctx, "affected", strconv.Itoa(len(out.Affected)))
	auditSetDetail(ctx, "restarted", strconv.Itoa(restarted))
	respondJSON(w, out)
}
```

`RestartWorkspace` returns the current record unchanged when the generation already moved, so the handler compares generations (`WorkspaceRecord.RuntimeGeneration`, added in Step 1). A CR whose latest intent the operator has not reconciled (`IntentSettled` false: an earlier restart or lifecycle change is still being applied) is listed but not restarted and reported as `skipped_moved`, so one `:apply` never stacks a second generation bump on an unreconciled first. A restart error other than a moved generation (for example `ErrNotRunning` when the workspace was stopped meanwhile) is reported per row as `failed` and does not abort the batch; the audit record keeps the counts.

Add the fields `apply PolicyApplyBackend`, `running RunningPolicySource` and `dir Directory` to `AdminSessionPolicyHandler`. In `MountAdminSessionPolicyRoutes`, add:

```go
	mux.Handle(routeAdminSessionPolicyApply, authn.RequireAuth(
		audited(h.audit, routeAdminSessionPolicyApply, authn.RequireCSRF(http.HandlerFunc(h.Apply)))))
```

Wiring (`wire.go`, on the `adminSessionPolicyHandler` chain ~728): `.WithApply(policyApplyBackend{svc, db}, statusView, directory)` (`directory := api.NewDirectory(db)` is declared earlier in `newAppHandler`, ~671), with a small adapter in `wire.go` (imports: `time`, `store` and `provisioning` are already imported there; check with `go build`):

```go
// policyApplyBackend joins the provisioning service and the lease store
// into api.PolicyApplyBackend.
type policyApplyBackend struct {
	*provisioning.Service
	db *store.DB
}

func (b policyApplyBackend) HasLiveLease(ctx context.Context, id string, now time.Time) (bool, error) {
	return b.db.HasLiveLease(ctx, id, now)
}
```

The route is mounted by `MountAdminSessionPolicyRoutes`, which `appMux` already calls, so `appMux` and its `backend_test.go` caller need no change. `svc.RestartWorkspace` needs `WithGenerationRevoker` wired by Task 6a (otherwise it errors per row as `failed`).

Run the Step 3 command and `go test ./internal/provisioning/ ./internal/backend/ -count=1`. Expected: PASS.

- [ ] **Step 5: "Apply now" flow**

`web/src/admin/api.ts`:

```ts
export type ApplySessionPolicyResult = components["schemas"]["ApplySessionPolicyResult"];

export async function applyAdminSessionPolicy(
  api: ApiClient,
  tenant: string,
  body: { dryRun: boolean; includeActive?: boolean },
  etag: string | undefined,
): Promise<ApplySessionPolicyResult> {
  return unwrap(
    await api.POST("/v1/admin/tenants/{tenant}/session-policy:apply", {
      params: { path: { tenant }, header: { "If-Match": etag ?? "*" } },
      body,
    }),
  );
}
```

`web/src/admin/ApplyPolicyDialog.tsx` (the page passes `policyText` so the dialog does not import the page and create an import cycle):

```tsx
import { useEffect, useState } from "react";
import { Alert, Badge, Button, Checkbox, Dialog, Spinner, Table } from "../design";
import type { Column } from "../design/Table";
import { t, type MessageKey } from "../i18n";
import { useApi } from "../api/context";
import { applyAdminSessionPolicy, type ApplySessionPolicyResult } from "./api";
import { ApiErrorAlert } from "./ApiErrorAlert";

type Row = ApplySessionPolicyResult["affected"][number];

const RESULT_LABEL: Record<Row["result"], MessageKey> = {
  pending: "admin.policy.apply.result.pending",
  restarted: "admin.policy.apply.result.restarted",
  skipped_active: "admin.policy.apply.result.skipped_active",
  skipped_moved: "admin.policy.apply.result.skipped_moved",
  failed: "admin.policy.apply.result.failed",
};

export function ApplyPolicyDialog({
  tenant,
  etag,
  policyText,
  onClose,
}: {
  tenant: string;
  etag: string | undefined;
  policyText: (p: Row["applied"]) => string;
  onClose: () => void;
}) {
  const api = useApi();
  const [data, setData] = useState<ApplySessionPolicyResult | null>(null);
  const [done, setDone] = useState(false);
  const [includeActive, setIncludeActive] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  // The dry run: what the confirmed call would restart. The dialog mounts
  // fresh on every open (the page renders it conditionally).
  useEffect(() => {
    let cancelled = false;
    applyAdminSessionPolicy(api, tenant, { dryRun: true }, etag)
      .then((r) => {
        if (!cancelled) setData(r);
      })
      .catch((e) => {
        if (!cancelled) setError(e);
      });
    return () => {
      cancelled = true;
    };
  }, [api, tenant, etag]);

  async function confirm() {
    setBusy(true);
    setError(null);
    try {
      setData(await applyAdminSessionPolicy(api, tenant, { dryRun: false, includeActive }, etag));
      setDone(true);
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  }

  const rows = data?.affected ?? [];
  const restarted = rows.filter((r) => r.result === "restarted").length;
  const columns: Column<Row>[] = [
    { key: "workspace", header: t("admin.policy.apply.column.workspace"), rowHeader: true, render: (r) => r.name },
    { key: "owner", header: t("admin.policy.apply.column.owner"), render: (r) => r.owner.displayName },
    {
      key: "change",
      header: t("admin.policy.apply.column.change"),
      render: (r) => (
        <>
          {policyText(r.applied)} → {policyText(r.effective)}{" "}
          {r.live ? <Badge tone="warning">{t("admin.policy.apply.live")}</Badge> : null}
        </>
      ),
    },
    { key: "result", header: t("admin.policy.apply.column.result"), render: (r) => t(RESULT_LABEL[r.result]) },
  ];

  return (
    <Dialog
      open
      onClose={() => {
        if (!busy) onClose();
      }}
      dismissible={!busy}
      size="lg"
      title={t("admin.policy.apply.title")}
      footer={
        <>
          <Button onClick={onClose} disabled={busy}>
            {t("admin.policy.apply.close")}
          </Button>
          {data && rows.length > 0 && !done ? (
            <Button variant="primary" loading={busy} onClick={() => void confirm()}>
              {t("admin.policy.apply.confirm")}
            </Button>
          ) : null}
        </>
      }
    >
      <ApiErrorAlert error={error} />
      {!data && !error ? <Spinner label={t("admin.policy.apply.loading")} /> : null}
      {data && rows.length === 0 ? <p>{t("admin.policy.apply.none")}</p> : null}
      {data && rows.length > 0 ? (
        <>
          <p className="tc-admin-muted">{t("admin.policy.apply.description")}</p>
          {data.truncated ? <Alert tone="warning">{t("admin.policy.apply.truncated")}</Alert> : null}
          <Table
            caption={t("admin.policy.apply.caption")}
            columns={columns}
            rows={rows}
            rowKey={(r) => r.workspaceId}
            density="compact"
          />
          {done ? (
            <p role="status">
              {t("admin.policy.apply.summary", { restarted, skipped: rows.length - restarted })}
            </p>
          ) : (
            <Checkbox
              label={t("admin.policy.apply.includeActive")}
              checked={includeActive}
              onChange={(e) => setIncludeActive(e.target.checked)}
            />
          )}
        </>
      ) : null}
    </Dialog>
  );
}
```

In `SessionPolicyPage.tsx` (`SessionPolicyContent`, after Task 7's `dirty`): add `const [applyOpen, setApplyOpen] = useState(false);`, next to the Save button

```tsx
{policy.source === "TenantRow" && !dirty ? (
  <Button onClick={() => setApplyOpen(true)}>{t("admin.policy.apply.button")}</Button>
) : null}
```

and at the end of the fragment

```tsx
{applyOpen ? (
  <ApplyPolicyDialog
    tenant={policy.tenant}
    etag={policy.etag}
    policyText={(p) =>
      t("admin.policy.apply.policyText", {
        clipboard: clipboardPolicyLabel(p.clipboard),
        fileTransfer: fileTransferLabel(p.fileTransfer),
      })
    }
    onClose={() => setApplyOpen(false)}
  />
) : null}
```

Strings (`web/src/i18n/{en,vi}/admin.ts`; also replace `admin.policy.appliesNote` once more, building on Task 7's text):

| key | en | vi |
|---|---|---|
| `admin.policy.appliesNote` | Tightening applies to new connections and new file transfers right away; a running workspace's clipboard limits follow at its next start, or at once with Apply now. | Siết chặt áp dụng ngay với các kết nối mới và lượt truyền tệp mới; giới hạn clipboard của workspace đang chạy có hiệu lực ở lần khởi động tiếp theo, hoặc ngay với Áp dụng ngay. |
| `admin.policy.apply.button` | Apply now | Áp dụng ngay |
| `admin.policy.apply.title` | Apply the ceiling to running workspaces | Áp dụng giới hạn cho workspace đang chạy |
| `admin.policy.apply.description` | These workspaces run with a looser policy than the ceiling. Applying restarts them; open desktops disconnect and reconnect to the new policy. Home folders are kept. | Các workspace này đang chạy với chính sách lỏng hơn giới hạn. Áp dụng sẽ khởi động lại chúng; desktop đang mở sẽ ngắt và kết nối lại với chính sách mới. Thư mục chính được giữ nguyên. |
| `admin.policy.apply.caption` | Running workspaces looser than the ceiling | Workspace đang chạy lỏng hơn giới hạn |
| `admin.policy.apply.loading` | Checking running workspaces | Đang kiểm tra các workspace đang chạy |
| `admin.policy.apply.close` | Close | Đóng |
| `admin.policy.apply.policyText` | Clipboard: {clipboard}; files: {fileTransfer} | Clipboard: {clipboard}; tệp: {fileTransfer} |
| `admin.policy.apply.includeActive` | Also restart workspaces someone is using right now | Khởi động lại cả workspace đang có người dùng |
| `admin.policy.apply.confirm` | Restart affected workspaces | Khởi động lại các workspace bị ảnh hưởng |
| `admin.policy.apply.none` | Every running workspace already follows the ceiling. | Mọi workspace đang chạy đã tuân theo giới hạn. |
| `admin.policy.apply.truncated` | Only the first 500 affected workspaces are listed. Apply again after they restart. | Chỉ liệt kê 500 workspace bị ảnh hưởng đầu tiên. Hãy áp dụng lại sau khi chúng khởi động lại. |
| `admin.policy.apply.live` | Live session | Đang có phiên |
| `admin.policy.apply.result.pending` | Will restart | Sẽ khởi động lại |
| `admin.policy.apply.result.restarted` | Restarted | Đã khởi động lại |
| `admin.policy.apply.result.skipped_active` | Skipped (in use) | Bỏ qua (đang dùng) |
| `admin.policy.apply.result.skipped_moved` | Skipped (already restarting) | Bỏ qua (đang khởi động lại) |
| `admin.policy.apply.result.failed` | Failed | Thất bại |
| `admin.policy.apply.summary` | {restarted} restarted, {skipped} skipped. | {restarted} đã khởi động lại, {skipped} bỏ qua. |
| `admin.policy.apply.column.workspace` | Workspace | Workspace |
| `admin.policy.apply.column.owner` | Owner | Chủ sở hữu |
| `admin.policy.apply.column.change` | Applied → ceiling | Hiện tại → giới hạn |
| `admin.policy.apply.column.result` | Result | Kết quả |

Mock (`web/tests/mock-api/admin.ts`): in `api()` add, before the `mSP` match, `const mApply = path.match(/^\/v1\/admin\/tenants\/([^/]+)\/session-policy:apply$/); if (mApply) return adminSessionPolicyApply(req, mApply[1]);` with

```ts
  // POST …/session-policy:apply — two running workspaces looser than any
  // ceiling that narrows clipboard: one idle, one with a live session.
  function adminSessionPolicyApply(req: MockRequest, tenant: string): MockResponse {
    if (req.method !== "POST") return err(405, "INVALID_REQUEST", "method not allowed", false);
    if (!t.isAdmin() || tenant !== t.me.tenant) {
      return err(403, "FORBIDDEN", "session policy administration requires the tenant-admin role", false);
    }
    const pre = checkIfMatch(req, sessionPolicyTag());
    if (pre) return pre;
    if (!t.sessionPolicy) {
      return err(409, "INVALID_STATE", "no ceiling is set; every workspace already follows its template", false);
    }
    const b = req.body as { dryRun?: unknown; includeActive?: unknown } | null;
    if (!b || typeof b.dryRun !== "boolean") {
      return err(400, "INVALID_REQUEST", "invalid request body", false);
    }
    const applied = { clipboard: "Bidirectional", fileTransfer: "Disabled" };
    const effective = { clipboard: t.sessionPolicy.clipboard, fileTransfer: "Disabled" };
    const owner = { subject: "alice", displayName: "Alice" };
    const rows =
      t.sessionPolicy.clipboard === "Bidirectional"
        ? []
        : [
            { workspaceId: "ws_apply_1", name: "research-desktop", owner, applied, effective, live: false },
            { workspaceId: "ws_apply_2", name: "build-box", owner, applied, effective, live: true },
          ];
    const affected = rows.map((r) => ({
      ...r,
      result: b.dryRun
        ? "pending"
        : r.live && b.includeActive !== true
          ? "skipped_active"
          : "restarted",
    }));
    return ok(200, { dryRun: b.dryRun, truncated: false, affected });
  }
```

Playwright, appended to `web/tests/session-policy.spec.ts`:

```ts
test("apply now lists looser workspaces and restarts them", async ({ page, request }) => {
  await makeAdmin(request);
  await request.post(`${MOCK}/_control/admin/session-policy`, { data: { clipboard: "Disabled" } });
  await login(page);

  await page.getByRole("button", { name: "Apply now" }).click();
  const dialog = page.getByRole("dialog", { name: "Apply the ceiling to running workspaces" });
  await expect(dialog.getByRole("row", { name: /research-desktop/ })).toBeVisible();
  await expect(dialog.getByRole("row", { name: /build-box/ }).getByText("Live session")).toBeVisible();

  await dialog.getByLabel("Also restart workspaces someone is using right now").check();
  await dialog.getByRole("button", { name: "Restart affected workspaces" }).click();
  await expect(dialog.getByRole("row", { name: /research-desktop/ }).getByText("Restarted")).toBeVisible();
  await expect(dialog.getByRole("row", { name: /build-box/ }).getByText("Restarted")).toBeVisible();
  await expect(dialog.getByText("2 restarted, 0 skipped.")).toBeVisible();
});

test("apply now has nothing to do when every workspace follows the ceiling", async ({ page, request }) => {
  await makeAdmin(request);
  await request.post(`${MOCK}/_control/admin/session-policy`, { data: { clipboard: "Bidirectional" } });
  await login(page);
  await page.getByRole("button", { name: "Apply now" }).click();
  await expect(
    page.getByRole("dialog").getByText("Every running workspace already follows the ceiling."),
  ).toBeVisible();
  await expect(page.getByRole("dialog").getByRole("button", { name: "Restart affected workspaces" })).toHaveCount(0);
});
```

Run:

```bash
cd web && npm run generate && npm run typecheck && npm run lint:strings && npx vitest run tests/unit/admin && npx playwright test tests/session-policy.spec.ts
```

Expected: PASS.

- [ ] **Step 6: Kind e2e: `:apply` against a real pod**

Spec section 7.5 lists `:apply` in the kind admin flow. Tasks 4 and 5 own the rest of that flow (sessions, sign-out, audit row, users, overview) and seed `demo-admin` in `tenant-a` with the `ADMIN_USER` and `TENANT` constants in `hack/quickstart/smoke/smoke.spec.ts`; this step adds only the policy-apply test, and nothing here edits `realm-export.json`, `common.sh` or `up.sh`.

The test is deterministic by construction: it clones a Bidirectional template (`cloneBrowserTemplate`, existing helper), frees the tenant's two running slots first (earlier smokes leave workspaces running), starts a workspace on the clone, opens a real session (a live lease), then narrows the ceiling to Disabled and checks `:apply` end to end: dry run lists it as live, a confirmed call without `includeActive` skips it and leaves the pod untouched, a stale `If-Match` is refused, and the "Apply now" dialog with "also restart" replaces the pod so `TCDI_CLIPBOARD_POLICY` becomes `Disabled`.

Append after the last test in `hack/quickstart/smoke/smoke.spec.ts` (`login`, `createWorkspaceOnTemplate`, `connectSessionFrame`, `stopWorkspace`, `waitForConnect`, `podPolicy`, `cloneBrowserTemplate`, `ADMIN_USER`, `TENANT` already exist at that point):

```ts
// Frees running-quota slots: the tenant runs two workspaces at most and the
// earlier smokes leave some behind. Stops every Running workspace the page's
// user owns, then waits until they are Stopped.
async function stopAllRunning(page: Page) {
  const list = await page.evaluate(async () => {
    const r = await fetch("/v1/workspaces?limit=100");
    return r.ok ? ((await r.json()) as { items: { id: string; desiredState: string }[] }).items : [];
  });
  for (const w of list.filter((x) => x.desiredState === "Running")) {
    await stopWorkspace(page, w.id);
  }
}

test("admin: session-policy apply restarts a workspace that is looser than the ceiling", async ({ browser }) => {
  const stamp = Date.now().toString(36);
  const tplName = `apply-bidi-${stamp}`;
  cloneBrowserTemplate(tplName, `Apply Bidi ${stamp}`, "Bidirectional");

  // The workspace owner: a Bidirectional workspace with an open session.
  const userCtx = await browser.newContext({ ignoreHTTPSErrors: true });
  const user = await userCtx.newPage();
  await login(user);
  await stopAllRunning(user);
  const id = await createWorkspaceOnTemplate(user, `apply-${stamp}`, new RegExp(tplName, "i"));
  await expect.poll(() => podPolicy(id), { timeout: 120_000 }).toBe("Bidirectional");
  await connectSessionFrame(user, id);

  // The tenant administrator.
  const adminCtx = await browser.newContext({ ignoreHTTPSErrors: true });
  const admin = await adminCtx.newPage();
  await login(admin, ADMIN_USER);
  const policyUrl = `/v1/admin/tenants/${TENANT}/session-policy`;
  const csrf = async () => ((await (await admin.request.get("/v1/me")).json()) as { csrfToken: string }).csrfToken;
  const etagNow = async () => (await admin.request.get(policyUrl)).headers()["etag"] ?? "*";
  const post = async (path: string, body: unknown, ifMatch: string) =>
    admin.request.post(path, {
      data: body,
      headers: { "X-CSRF-Token": await csrf(), "If-Match": ifMatch },
    });
  type Row = { workspaceId: string; live: boolean; result: string; applied: { clipboard: string }; effective: { clipboard: string } };
  const mine = (j: { affected: Row[] }) => j.affected.find((a) => a.workspaceId === id);

  try {
    // Narrow the ceiling to Disabled. Nothing is applied to running pods yet.
    const put = await admin.request.put(policyUrl, {
      data: { clipboard: "Disabled", fileTransfer: { mode: "FollowTemplate" } },
      headers: { "X-CSRF-Token": await csrf(), "If-Match": await etagNow() },
    });
    expect(put.status()).toBe(200);
    expect(podPolicy(id)).toBe("Bidirectional");

    // Dry run: lists the workspace as live, restarts nothing.
    const dry = await post(`${policyUrl}:apply`, { dryRun: true }, await etagNow());
    expect(dry.status()).toBe(200);
    const dryRow = mine((await dry.json()) as { affected: Row[] });
    expect(dryRow, "dry run lists the Bidirectional workspace").toBeTruthy();
    expect(dryRow).toMatchObject({ live: true, result: "pending", applied: { clipboard: "Bidirectional" }, effective: { clipboard: "Disabled" } });

    // A confirmed call without includeActive skips the live workspace.
    const skip = await post(`${policyUrl}:apply`, { dryRun: false }, await etagNow());
    expect(skip.status()).toBe(200);
    expect(mine((await skip.json()) as { affected: Row[] })?.result).toBe("skipped_active");
    expect(podPolicy(id)).toBe("Bidirectional");

    // A stale If-Match is refused with the envelope's PRECONDITION_FAILED.
    const stale = await post(`${policyUrl}:apply`, { dryRun: true }, '"stale"');
    expect(stale.status()).toBe(412);
    expect(((await stale.json()) as { error: { code: string } }).error.code).toBe("PRECONDITION_FAILED");

    // The UI: "Apply now" lists the live workspace; restarting it replaces the pod.
    await admin.goto("/admin/session-policy");
    await admin.getByRole("button", { name: "Apply now" }).click();
    const dialog = admin.getByRole("dialog", { name: "Apply the ceiling to running workspaces" });
    const row = dialog.getByRole("row", { name: new RegExp(`apply-${stamp}`) });
    await expect(row.getByText("Live session")).toBeVisible();
    await dialog.getByLabel("Also restart workspaces someone is using right now").check();
    await dialog.getByRole("button", { name: "Restart affected workspaces" }).click();
    await expect(row.getByText("Restarted")).toBeVisible();

    // The replacement pod carries the narrowed policy.
    await expect.poll(() => podPolicy(id), { timeout: 5 * 60_000, intervals: [3_000] }).toBe("Disabled");

    // The apply is audited (Task 2's viewer).
    const audit = await admin.request.get(`/v1/admin/tenants/${TENANT}/audit?action=admin.session_policy.apply`);
    expect(audit.status()).toBe(200);
    const rows = ((await audit.json()) as { items: { outcome: string }[] }).items;
    expect(rows.some((e) => e.outcome === "success")).toBe(true);
  } finally {
    // Restore the ceiling (all FollowTemplate deletes the row) and free the slot.
    await admin.request.put(policyUrl, {
      data: { clipboard: "FollowTemplate", fileTransfer: { mode: "FollowTemplate" } },
      headers: { "X-CSRF-Token": await csrf(), "If-Match": await etagNow() },
    });
    await stopWorkspace(user, id);
    await userCtx.close();
    await adminCtx.close();
  }
});
```

`createWorkspaceOnTemplate` matches the option label by the catalog name (the clone's object name), exactly as the clipboard smoke does. `stopWorkspace` waits for `Stopped`.

Compile check, no cluster needed:

```bash
cd hack/quickstart/smoke && npx playwright test --list > "$TMPDIR/apply-smoke-list.log" 2>&1; echo "rc=$?"; grep -c 'session-policy apply restarts' "$TMPDIR/apply-smoke-list.log"
```

Expected: `rc=0` and `1`. The test runs for real in CI's `quickstart` job; Task 8 is done only when that job is green.

- [ ] **Step 7: Threat model, runbook and commit**

Row S47, `docs/security/threat-model.md` (three columns, as the neighbouring rows): `| S47 | Session-policy apply | Implemented: tenant-admin, CSRF and If-Match on the ceiling version; dry run first; restarts go through RestartWorkspace (generation-fenced, leases of the old generation revoked, unreconciled CRs skipped); live sessions are skipped unless includeActive; at most 500 per call over a tenant-scoped scan of at most 5000; audited with counts |`

`docs/runbooks/session-policy.md`, "When does a change take effect?" (about lines 48-63): replace the closing paragraph

```
If a ceiling must take effect everywhere *now*, ask users to stop and
start their workspaces; no platform-side restart action is triggered.
```

with

````
If a ceiling must take effect everywhere *now*, apply it: the admin page's
**Apply now** button (shown while a ceiling row exists and the form has no
unsaved edits) or `POST .../session-policy:apply`. Both restart running
workspaces whose applied policy is looser than the ceiling; home volumes
are kept and open desktops reconnect to the new policy.

1. **Dry run.** `{"dryRun": true}` restarts nothing and lists every running
   workspace that is looser than the stored ceiling, with its owner, the
   applied and the resulting policy, and whether a session is live.
2. **Confirm.** `{"dryRun": false}` restarts the listed workspaces, except
   those with a live session. Add `"includeActive": true` to restart those
   too; their users are disconnected.
3. **Limits.** One call lists and restarts at most 500 workspaces
   (`truncated: true` says more qualify; call again once they have
   restarted). A workspace whose earlier restart is not reconciled yet is
   reported `skipped_moved`, never restarted twice.
4. **Preconditions.** Tenant administrators only, CSRF token, and
   `If-Match` carrying the ceiling's current `ETag` (a stale tag is `412`;
   with no ceiling row the call is `409`).
5. **Audit.** Every call, dry or confirmed, is recorded as
   `admin.session_policy.apply` with `dryRun`, `includeActive`, `affected`
   and `restarted` counts.

```bash
etag=$(curl -sD- -o /dev/null "https://<portal>/v1/admin/tenants/<tenant>/session-policy" \
  -b cookies.txt | awk 'tolower($1)=="etag:"{print $2}' | tr -d '\r')
curl -X POST "https://<portal>/v1/admin/tenants/<tenant>/session-policy:apply" \
  -b cookies.txt -H "X-CSRF-Token: <token>" -H "If-Match: $etag" \
  -H "Content-Type: application/json" \
  -d '{"dryRun":true}'
```

Without Apply now, workspaces pick the ceiling up at their next stop and start.
````

Then fix the "Verify" bullet that says affected workspaces show `spec.sessionPolicy` "at their next start": append "(or right after an apply)".

```bash
go test -count=1 ./internal/provisioning/ ./internal/api/ ./internal/backend/
git add internal web hack/quickstart/smoke/smoke.spec.ts docs/security/threat-model.md docs/runbooks/session-policy.md
git commit -m "feat(admin): session-policy apply restarts workspaces looser than the ceiling

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 9: Cold-connect spike, Ready gated on the ready EndpointSlice, gateway dial retry (F2, part 1) — tier: design

**Files:**
- Create: `hack/quickstart/smoke/cold-connect.spec.ts`, `hack/quickstart/smoke/playwright.cold-connect.config.ts` (both live in `hack/quickstart/smoke/`, next to `smoke.spec.ts`; never under `web/`)
- Modify: `internal/runtime/linux/backend.go` (`Observe` ~549; new `endpointReady`, `ReadyEndpointSlice`; add the `discoveryv1 "k8s.io/api/discovery/v1"` import), `internal/runtime/linux/backend_test.go` (add the same import), `internal/runtime/backend.go:33` (`ConnectionReady` doc comment)
- Modify: `internal/operator/workspace_controller.go` (RBAC markers 227-236; `requeueNotReady` ~154 and the requeue at ~558), `config/rbac/role.yaml` (generated), `deploy/helm/tinycdi/templates/rbac.yaml` (operator ClusterRole `-manager-role`), `deploy/helm/testdata/default-render.golden.yaml` (regenerated; `TestDefaultRenderUnchangedGolden` is byte-exact)
- Create: `deploy/helm/chart_rbac_test.go` (no such file exists; chart tests are `chart_*_test.go`)
- Modify: `internal/operator/workspace_controller_test.go:208-221` (`markPodReady` also publishes the slice; new `markPodReadyOnly`, `publishReadyEndpoint`), `internal/operator/hold_phase_test.go:46-57` (`servingPod` publishes the slice), `internal/broker/held_expiry_test.go:~243` and `tests/integration/operator_stopped_test.go:~219` (the other two places that flip a pod Ready); Create: `internal/operator/endpoint_ready_test.go`
- Create: `internal/gateway/dial.go`, `internal/gateway/dial_test.go`
- Modify: `internal/gateway/proxy.go` (`Config.UpstreamDialBudget` after `UpstreamResponseHeaderTimeout` ~184, defaults ~276, `Transport.DialContext` in `ensureTarget` ~880)
- Modify: `docs/lifecycle-reasons.md` (new `WaitingForEndpoint` row)
- Modify (portal wording for the new reason token): `web/src/progress/derive.ts` (`KNOWN_REASONS` ~97), `web/src/workspaces/reasons.tsx` (`REASON_TEXT` ~33), `web/src/i18n/en/workspaces.ts` and `web/src/i18n/vi/workspaces.ts` (next to `workspaces.reason.notReady`), `web/tests/unit/workspaces/reasons.test.tsx` (token list ~35), `web/tests/unit/progress/derive.test.ts`

**Interfaces:**
- Consumes: `linux.ServiceName(uid CRUID)` (`backend.go:229`, `CRUID = types.UID`), `linux.PodName`, `linux.LabelWorkspaceUID`, `podReady`, `streamingPort` (typed `int32`), `ptr[T]`, `gateway.Config`, `ensureTarget`, operator `requeueNotReady`.
- Produces (Task 10 consumes the report path and the gate env vars; no other task reads the per-run fields):
  - `func linux.ReadyEndpointSlice(pod *corev1.Pod, serviceName string) *discoveryv1.EndpointSlice` (for tests and envtest helpers)
  - `Observation.ConnectionReady` = pod Ready ∧ Service exists ∧ a ready endpoint in the Service's EndpointSlices whose `targetRef.uid` is the pod. Otherwise `Reason` = `WaitingForEndpoint` (new condition-reason token; `ConnectionReady=False/WaitingForEndpoint` with `RuntimeReady=True`).
  - `gateway.Config.UpstreamDialBudget time.Duration` (default `gateway.UpstreamDialBudget = 15 * time.Second`), and `func dialWithRetry(budget time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error)` (unexported)
  - Cold-connect report `test-results/cold-connect.json` (relative to `hack/quickstart/smoke`): `{"runs":[{"readyToConnectedMs":n,"mintMs":n,"launchStatus":n,"launchMs":n,"frameMs":n,"canvasMs":n,"upstream5xx":n,"connectionPosts":n,"wallMs":n}],"p95Ms":n}`. p95 is nearest-rank over `readyToConnectedMs` (the ceil(0.95 n)-th smallest value; 19th of 20). Env: `TCDI_QS_COLD_RUNS` (default 20), `TCDI_QS_COLD_P95_MS` (default 20000; the spec ALWAYS fails when `p95Ms >= TCDI_QS_COLD_P95_MS`).
- Why the operator-side check is an EndpointSlice and not a dial (plan ruling 9, verified): the pod's `readinessProbe` is `exec /opt/tcdi/healthcheck.sh` (`backend.go:1023`; `build/linux-base/healthcheck.sh`, same script in `build/kasm-adapter`), which runs `xdpyinfo` and then an authenticated `curl https://127.0.0.1:8443/` and exits 0 only for 200/301/302. So pod `Ready=True` already proves the session host answers on the stream port inside the pod. What it cannot prove is that the Service route (EndpointSlice, kube-proxy, CNI policy) has caught up; the slice check closes that gap and the gateway dial retry covers the remainder. The operator cannot dial pods (NetworkPolicy ingress allows gateway pods only).

- [ ] **Step 1: Spike: measure before changing anything**

`hack/quickstart/smoke/playwright.cold-connect.config.ts` is a copy of `playwright.config.ts` (same `use`, `workers: 1`, `retries: 0`, `reporter`) with these three differences:

```ts
  testMatch: "cold-connect.spec.ts",
  // TCDI_QS_COLD_RUNS sequential cold boots; each is ~1.5-3 min on kind
  // (boot, measure, delete, wait out release_pending).
  timeout: (Number(process.env.TCDI_QS_COLD_RUNS ?? "20") * 4 + 5) * 60_000,
  outputDir: "test-results",
```

(`outputDir` stays `test-results`; Playwright empties it when a run starts, and the spec writes the report during the run, so the report survives.)

`hack/quickstart/smoke/cold-connect.spec.ts`. Its constants are copied from the top of `smoke.spec.ts` (`USER` defaults to `demo`, `PASSWORD` to `tcdi-demo-dev-only`, `SESSION_DOMAIN` from `TCDI_QS_SESSION_DOMAIN`), so both specs read the same env names:

```ts
import { mkdirSync, writeFileSync } from "node:fs";
import { expect, test, type Page, type Response } from "@playwright/test";

// Cold first connect (F2): N fresh workspaces, each timed from the moment
// the portal offers Connect (the workspace is Ready) to the moment the
// session reports Connected. After every run it rewrites
// test-results/cold-connect.json, and it fails when p95 >= TCDI_QS_COLD_P95_MS.
const RUNS = Number(process.env.TCDI_QS_COLD_RUNS ?? "20");
const P95_MS = Number(process.env.TCDI_QS_COLD_P95_MS ?? "20000");
const USER = process.env.TCDI_QS_USER ?? "demo";
const PASSWORD = process.env.TCDI_QS_PASSWORD ?? "tcdi-demo-dev-only";
const SESSION_DOMAIN =
  process.env.TCDI_QS_SESSION_DOMAIN ??
  `session.${process.env.TCDI_QS_DOMAIN ?? "tcdi.localtest.me"}`;

type Run = {
  readyToConnectedMs: number; // Connect visible -> portal badge "Connected"
  mintMs: number; //            ... -> POST /v1/workspaces/{id}/connections answered
  launchStatus: number; //      status of the first https://<ws>.<session>/v1/launch response (303 = redeemed)
  launchMs: number; //          ... -> that response
  frameMs: number; //           ... -> the session frame is on the workspace origin
  canvasMs: number; //          ... -> the KasmVNC canvas is visible
  upstream5xx: number; //       responses >= 500 from the workspace origin before Connected (gateway 502 = dial/redeem gave up)
  connectionPosts: number; //   ticket mints during the run (> 1: the watch or a cold re-mint fired)
  wallMs: number; //            create -> delete accepted (CI budget input)
};

async function login(page: Page) {
  await page.goto("/");
  await expect(page.locator("#username")).toBeVisible();
  await page.locator("#username").fill(USER);
  await page.locator("#password").fill(PASSWORD);
  await page.locator("#kc-login").click();
  await expect(page.getByRole("heading", { name: "Workspaces", exact: true })).toBeVisible();
}

// Creates an Ephemeral browser workspace (no retained disk piles up over
// the runs). A just-deleted workspace's running-quota slot is held until
// the backend's recovery pass proves the runtime gone, so the API answers a
// retryable 409 details.reason=release_pending: retry only that, as
// smoke.spec.ts createWorkspaceOnTemplate does.
async function createWorkspace(page: Page, name: string): Promise<string> {
  const deadline = Date.now() + 120_000;
  for (;;) {
    await page.goto("/workspaces/new");
    await page.locator('input[name="name"]').fill(name);
    const option = page.locator('select[name="template"] option', { hasText: /browser/i }).first();
    await expect(option).toBeAttached();
    await page.locator('select[name="template"]').selectOption((await option.getAttribute("value")) ?? "");
    await page.locator('select[name="dataPolicy"]').selectOption("Ephemeral");
    const [resp] = await Promise.all([
      page.waitForResponse((r) => r.url().endsWith("/v1/workspaces") && r.request().method() === "POST"),
      page.getByRole("button", { name: "Create workspace" }).click(),
    ]);
    if (resp.status() === 201) {
      await page.waitForURL(/\/workspaces\/ws_/);
      return decodeURIComponent(new URL(page.url()).pathname.split("/").pop() ?? "");
    }
    const body = (await resp.json().catch(() => ({}))) as { error?: { details?: { reason?: string } } };
    expect(
      body?.error?.details?.reason === "release_pending" && Date.now() < deadline,
      `workspace create refused (${resp.status()}): ${JSON.stringify(body)}`,
    ).toBeTruthy();
    await page.waitForTimeout(5_000);
  }
}

// Wait for the accepted DELETE before any navigation (a goto that beats the
// in-flight request aborts it). The next create waits out release_pending.
async function deleteWorkspace(page: Page, id: string) {
  await page.goto(`/workspaces/${encodeURIComponent(id)}`);
  await page.getByRole("button", { name: "Delete", exact: true }).click();
  const [resp] = await Promise.all([
    page.waitForResponse((r) => r.url().endsWith(`/v1/workspaces/${id}`) && r.request().method() === "DELETE"),
    page.getByRole("button", { name: "Confirm delete" }).click(),
  ]);
  expect(resp.status(), "delete is accepted").toBe(202);
}

// Nearest-rank percentile: the ceil(0.95 n)-th smallest value.
function p95(xs: number[]): number {
  const s = [...xs].sort((a, b) => a - b);
  return s[Math.min(s.length - 1, Math.ceil(0.95 * s.length) - 1)];
}

function writeReport(runs: Run[]) {
  const report = { runs, p95Ms: p95(runs.map((r) => r.readyToConnectedMs)) };
  mkdirSync("test-results", { recursive: true });
  writeFileSync("test-results/cold-connect.json", JSON.stringify(report, null, 2));
  return report;
}

test("cold first connect p95", async ({ page }) => {
  await login(page);
  const runs: Run[] = [];
  for (let i = 0; i < RUNS; i++) {
    const created = Date.now();
    const id = await createWorkspace(page, `cold-${Date.now().toString(36)}-${i}`);
    const origin = `https://${id.replaceAll("_", "-").toLowerCase()}.${SESSION_DOMAIN}`;
    let upstream5xx = 0;
    let connectionPosts = 0;
    const onResponse = (r: Response) => {
      if (r.url().startsWith(`${origin}/`) && r.status() >= 500) upstream5xx++;
      if (r.request().method() === "POST" && r.url().endsWith(`/v1/workspaces/${id}/connections`)) connectionPosts++;
    };
    page.on("response", onResponse);

    const connect = page.getByRole("link", { name: "Connect" });
    await expect(connect).toBeVisible({ timeout: 10 * 60_000 });
    const t0 = Date.now(); // the portal offers Connect: the workspace is Ready
    const mint = page.waitForResponse((r) => r.url().endsWith(`/v1/workspaces/${id}/connections`) && r.request().method() === "POST");
    const launch = page.waitForResponse((r) => r.url().startsWith(`${origin}/v1/launch`), { timeout: 120_000 });
    await connect.click();
    await mint;
    const mintMs = Date.now() - t0;
    const launchResp = await launch;
    const launchMs = Date.now() - t0;
    const inSession = () => page.frames().find((f) => f.url().startsWith(`${origin}/`) && !f.url().includes("/v1/launch"));
    await expect.poll(() => inSession() !== undefined, { timeout: 120_000, intervals: [100] }).toBe(true);
    const frameMs = Date.now() - t0;
    await expect(inSession()!.locator("canvas:visible").first()).toBeVisible({ timeout: 120_000 });
    const canvasMs = Date.now() - t0;
    await expect(page.getByText("Connected", { exact: true })).toBeVisible({ timeout: 120_000 });
    const readyToConnectedMs = Date.now() - t0;
    page.off("response", onResponse);

    await deleteWorkspace(page, id);
    runs.push({
      readyToConnectedMs, mintMs, launchStatus: launchResp.status(), launchMs, frameMs, canvasMs,
      upstream5xx, connectionPosts, wallMs: Date.now() - created,
    });
    writeReport(runs); // after every run: a timeout still leaves the data
  }
  const report = writeReport(runs);
  console.log(`cold-connect p95=${report.p95Ms}ms over ${runs.length} runs (gate < ${P95_MS} ms)`);
  expect(report.p95Ms, "cold-connect p95 (F2 gate)").toBeLessThan(P95_MS);
});
```

Verify the file compiles and registers one test (install the pinned Playwright first when `node_modules` is absent):

```bash
cd hack/quickstart/smoke && { [ -d node_modules ] || npm ci; } && npx playwright test --list -c playwright.cold-connect.config.ts > /tmp/cold-list.log 2>&1; echo "rc=$?"; grep -c 'cold first connect p95' /tmp/cold-list.log
```

Expected: `rc=0`, then `1`.

**Spike run** (needs the kind quickstart: `hack/quickstart/up.sh`, about 20 min; the header of `hack/quickstart/up.sh` lists its env). It measures the baseline, so the gate is switched off for it. Run it on the tree as it stands before Step 2:

```bash
cd hack/quickstart/smoke && export KUBECONFIG="$HOME/.local/state/tcdi-quickstart/${TCDI_QS_CLUSTER:-tcdi-quickstart}/kubeconfig"
TCDI_QS_COLD_RUNS=5 TCDI_QS_COLD_P95_MS=100000000 npx playwright test -c playwright.cold-connect.config.ts > /tmp/cold-spike.log 2>&1; echo "rc=$?"
jq -c '{p95Ms, runs:[.runs[]|{readyToConnectedMs,mintMs,launchStatus,launchMs,frameMs,canvasMs,upstream5xx,connectionPosts,wallMs}]}' test-results/cold-connect.json
```

What the numbers mean. Per run the timeline is four segments after t0 (Connect visible): **mint** = `mintMs`; **redeem** = `launchMs - mintMs`; **frame** = `frameMs - launchMs`; **stream** = `canvasMs - frameMs`. Decide with these exact rules, in order:
1. **STOP, report** (do not start Step 2) if `p95Ms < 20000` AND every run has `launchStatus == 303`, `upstream5xx == 0` and `connectionPosts == 1`: the stall does not reproduce on this cluster. Paste the JSON into the task report; the CI gate (Task 10 Step 4) stays the arbiter.
2. **STOP, report** if in at least 3 of the 5 runs `mintMs > 5000` and `mint` is the largest segment: the cause is the API (mint), outside Steps 2-5.
3. Otherwise continue with Steps 2-5 when any run shows `launchStatus != 303` (the redeem path: ticket expired or consumed, F2 candidate 2; Task 10 is the fix), `upstream5xx > 0` (the gateway answered 502: its dial or redeem gave up, candidate 3; Steps 4-5 are the fix), or a `stream` segment above 8000 ms with a Ready-looking pod (candidate 1; Steps 2-3 are the fix). Name the matching candidate in the report.

If no kind cluster is available to the implementer, say so in the task report and continue with Steps 2-5 (the CI gate measures them). Commit the spec and config:

```bash
git add hack/quickstart/smoke/cold-connect.spec.ts hack/quickstart/smoke/playwright.cold-connect.config.ts
git commit -m "test(quickstart): cold-connect benchmark spec

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

- [ ] **Step 2: Failing backend test: Ready needs the pod's ready endpoint**

Add to `internal/runtime/linux/backend_test.go`, plus the import `discoveryv1 "k8s.io/api/discovery/v1"`. The file's fixtures are `backendScheme(t)`, `testWorkspace()` and `testTemplate(nil)`, with a `fake.NewClientBuilder()` client and `New(c, Options{})` (see `ensurePodSpecWS`). `clientgoscheme.AddToScheme` already registers `discoveryv1`. `Ensure` creates the pod and Service; the test then plays the kubelet (pod Ready, UID stamped, which the fake client does not assign) and, per case, the endpointslice controller.

```go
func TestObserve_ConnectionReadyNeedsReadyEndpointOfThisPod(t *testing.T) {
	cases := []struct {
		name  string
		slice func(pod *corev1.Pod, svc string) *discoveryv1.EndpointSlice
		want  bool
	}{
		{"no slice yet", func(*corev1.Pod, string) *discoveryv1.EndpointSlice { return nil }, false},
		{"slice for this pod, not ready", func(p *corev1.Pod, svc string) *discoveryv1.EndpointSlice {
			s := ReadyEndpointSlice(p, svc)
			s.Endpoints[0].Conditions.Ready = ptr(false)
			return s
		}, false},
		{"slice for an old pod uid", func(p *corev1.Pod, svc string) *discoveryv1.EndpointSlice {
			s := ReadyEndpointSlice(p, svc)
			s.Endpoints[0].TargetRef.UID = "old-pod-uid"
			return s
		}, false},
		{"ready slice for this pod", ReadyEndpointSlice, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			ws := testWorkspace()
			kc := fake.NewClientBuilder().WithScheme(backendScheme(t)).Build()
			b := New(kc, Options{})
			if _, err := b.Ensure(ctx, ws, testTemplate(nil)); err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			pod := &corev1.Pod{}
			if err := kc.Get(ctx, client.ObjectKey{Name: PodName(ws.UID), Namespace: ws.Namespace}, pod); err != nil {
				t.Fatalf("pod: %v", err)
			}
			pod.UID = types.UID("pod-uid-1")
			if err := kc.Update(ctx, pod); err != nil {
				t.Fatalf("stamp pod uid: %v", err)
			}
			pod.Status.Phase = corev1.PodRunning
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
			if err := kc.Status().Update(ctx, pod); err != nil {
				t.Fatalf("mark pod ready: %v", err)
			}
			if s := c.slice(pod, ServiceName(ws.UID)); s != nil {
				if err := kc.Create(ctx, s); err != nil {
					t.Fatalf("create slice: %v", err)
				}
			}
			obs, err := b.Observe(ctx, ws)
			if err != nil {
				t.Fatal(err)
			}
			if !obs.RuntimeReady {
				t.Fatalf("RuntimeReady = false, want true (the pod is Ready)")
			}
			if obs.ConnectionReady != c.want {
				t.Errorf("ConnectionReady = %v, want %v (reason %q)", obs.ConnectionReady, c.want, obs.Reason)
			}
			if !c.want && obs.Reason != "WaitingForEndpoint" {
				t.Errorf("reason %q, want WaitingForEndpoint", obs.Reason)
			}
		})
	}
}
```

Run `go test ./internal/runtime/linux/ -run TestObserve_ConnectionReadyNeedsReadyEndpoint -count=1`. Expected: FAIL to compile (`ReadyEndpointSlice` undefined).

- [ ] **Step 3: Implement the endpoint gate**

In `internal/runtime/linux/backend.go` (add the import `discoveryv1 "k8s.io/api/discovery/v1"`):

```go
// endpointReady reports whether the Service's EndpointSlices carry a ready
// endpoint that targets podUID. The kubelet's readiness probe already
// proved the stream port answers inside the pod (healthcheck.sh curls
// 127.0.0.1:8443 with the runtime credential); this proves the Service
// route the gateway dials has caught up with that pod (F2).
func (b *Backend) endpointReady(ctx context.Context, ns, service string, podUID types.UID) (bool, error) {
	if podUID == "" {
		return false, nil
	}
	var slices discoveryv1.EndpointSliceList
	if err := b.client.List(ctx, &slices, client.InNamespace(ns),
		client.MatchingLabels{discoveryv1.LabelServiceName: service}); err != nil {
		return false, err
	}
	for _, s := range slices.Items {
		for _, ep := range s.Endpoints {
			if ep.TargetRef != nil && ep.TargetRef.UID == podUID &&
				ep.Conditions.Ready != nil && *ep.Conditions.Ready {
				return true, nil
			}
		}
	}
	return false, nil
}

// ReadyEndpointSlice is the EndpointSlice the endpointslice controller
// publishes for a ready pod behind service. Tests use it where no
// controller runs (fake clients, envtest).
func ReadyEndpointSlice(pod *corev1.Pod, service string) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: service + "-ready", Namespace: pod.Namespace,
			Labels: map[string]string{discoveryv1.LabelServiceName: service},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"10.0.0.1"},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr(true)},
			TargetRef:  &corev1.ObjectReference{Kind: "Pod", Name: pod.Name, Namespace: pod.Namespace, UID: pod.UID},
		}},
		Ports: []discoveryv1.EndpointPort{{Port: ptr(streamingPort)}},
	}
}
```

In `Observe`, replace the comment and `obs.ConnectionReady = obs.RuntimeReady && obs.ServiceRef != nil` (~549) with:

```go
	// ConnectionReady: the runtime is ready, the fronting Service exists and
	// its EndpointSlices already route to THIS pod (F2).
	if obs.RuntimeReady && obs.ServiceRef != nil {
		ready, err := b.endpointReady(ctx, ws.Namespace, ServiceName(uid), types.UID(obs.RuntimeUID))
		if err != nil {
			return obs, err
		}
		obs.ConnectionReady = ready
		if !ready {
			obs.Reason = "WaitingForEndpoint"
		}
	}
```

Update the `ConnectionReady` comment in `internal/runtime/backend.go:33` to "the streaming endpoint is reachable: the runtime is ready and the internal Service routes to this incarnation's pod".

The operator controller needs no other change to carry the reason: `workspace_controller.go:777-781` copies a non-`Ready` `obs.Reason` onto `ConnectionReady=False`. One requeue tweak keeps the wait short: the endpointslice controller trails the kubelet by well under a second, and no watch on EndpointSlices is added, so shorten the poll while only the endpoint is missing. Next to `requeueNotReady` (~154) add `requeueEndpointWait = time.Second` with a one-line doc comment, and at ~558:

```go
	if phase != cdiv1.WorkspacePhaseReady &&
		phase != cdiv1.WorkspacePhaseFailed {
		res.RequeueAfter = requeueNotReady
		if obs.RuntimeReady && !obs.ConnectionReady {
			res.RequeueAfter = requeueEndpointWait
		}
	}
```

RBAC: add `// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch` to the marker block (`workspace_controller.go:236`). Run `make manifests`; `config/rbac/role.yaml` gains the rule. The operator reads through the manager's cache, so `list` and `watch` are both needed (the first read starts the informer); the chart binds this ClusterRole by RoleBinding in each managed namespace only, so the grant stays namespace-scoped. In `deploy/helm/tinycdi/templates/rbac.yaml`, add to the operator ClusterRole (`{{ .Release.Name }}-manager-role`, after the `networkpolicies` rule):

```yaml
# Observe reads the Service's EndpointSlices: ConnectionReady needs a ready
# endpoint for this pod (F2). Read-only.
- apiGroups: ["discovery.k8s.io"]
  resources: ["endpointslices"]
  verbs: ["get", "list", "watch"]
```

Then regenerate the byte-exact render golden (the command is in `deploy/helm/chart_defaultrender_test.go`'s header) and add the chart test in a new `deploy/helm/chart_rbac_test.go`:

```bash
cd deploy/helm && helm template tcdi ./tinycdi --namespace tcdi-system --include-crds -f tinycdi/ci/minimal-values.yaml > testdata/default-render.golden.yaml; cd ../..
git diff --stat deploy/helm/testdata/default-render.golden.yaml   # only the new rule
```

```go
package chart_test

import (
	"strings"
	"testing"
)

// The operator reads EndpointSlices (ConnectionReady, F2): list+watch must
// be in its ClusterRole, which the chart binds only inside managed namespaces.
func TestOperatorRoleReadsEndpointSlices(t *testing.T) {
	has := func(v any, want string) bool {
		for _, x := range toSlice(v) {
			if x == want {
				return true
			}
		}
		return false
	}
	for _, d := range selectDocs(render(t, "minimal-values.yaml"), "ClusterRole") {
		if name, _ := meta(d); !strings.HasSuffix(name, "-manager-role") {
			continue
		}
		for _, r := range toSlice(d["rules"]) {
			rm, _ := r.(map[string]any)
			if has(rm["apiGroups"], "discovery.k8s.io") && has(rm["resources"], "endpointslices") &&
				has(rm["verbs"], "list") && has(rm["verbs"], "watch") {
				return
			}
		}
		t.Fatal("operator manager-role has no discovery.k8s.io endpointslices list+watch rule")
	}
	t.Fatal("no manager-role ClusterRole rendered")
}
```

Run `go test ./deploy/helm/ -count=1`. Expected: PASS (golden and the new test).

Test helpers. In `internal/operator/workspace_controller_test.go` (imports `discoveryv1 "k8s.io/api/discovery/v1"`) split `markPodReady` into the kubelet half and the endpointslice half:

```go
// markPodReadyOnly simulates the kubelet alone: Pod scheduled + Ready
// (readinessProbe exec of the runtime healthcheck passing).
func markPodReadyOnly(t *testing.T, c client.Client, pod *corev1.Pod) {
	t.Helper()
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, Reason: "Scheduled"},
		{Type: corev1.PodReady, Status: corev1.ConditionTrue, Reason: "Ready"},
	}
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("mark pod ready: %v", err)
	}
}

// publishReadyEndpoint simulates the endpointslice controller: a recreated
// pod reuses the slice name with a new pod UID, so the old slice is replaced.
func publishReadyEndpoint(t *testing.T, c client.Client, pod *corev1.Pod) {
	t.Helper()
	ctx := context.Background()
	slice := linux.ReadyEndpointSlice(pod, linux.ServiceName(types.UID(pod.Labels[linux.LabelWorkspaceUID])))
	_ = c.Delete(ctx, &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: slice.Name, Namespace: slice.Namespace}})
	if err := c.Create(ctx, slice); err != nil {
		t.Fatalf("publish endpoint slice: %v", err)
	}
}

// markPodReady is what the cluster does for a healthy pod: kubelet Ready,
// then the slice that routes the Service to it.
func markPodReady(t *testing.T, c client.Client, pod *corev1.Pod) {
	t.Helper()
	markPodReadyOnly(t, c, pod)
	publishReadyEndpoint(t, c, pod)
}
```

Three more places flip a pod Ready and then expect `Ready`; each must publish the slice or its phase stays Provisioning:
- `internal/operator/hold_phase_test.go` `servingPod` (fake client): after the `c.Status().Update`, call `publishReadyEndpoint(t, c, pod)` (the pod carries `LabelWorkspaceUID` from `seedBuiltPod`; it has the stamped `pod.UID`).
- `internal/broker/held_expiry_test.go` (~243, after `kc.Status().Update(ctx, pod)`): `if err := kc.Create(ctx, linux.ReadyEndpointSlice(pod, linux.ServiceName(ws.UID))); err != nil { t.Fatalf("publish endpoint slice: %v", err) }`.
- `tests/integration/operator_stopped_test.go` (~219, inside the `if pod.Status.Phase != corev1.PodRunning` block, after the status update): publish the slice the same way, ignoring `AlreadyExists` (`apierrors.IsAlreadyExists`) because the block can run on several polls. The pod's UID changes only on recreation, which this test does not do.

New controller test `internal/operator/endpoint_ready_test.go` (same package, envtest like `TestReconcileLinuxIdempotent`):

```go
package operator

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
)

// F2: a Ready pod is not a Ready workspace until the Service routes to it.
func TestReconcile_WaitingForEndpoint(t *testing.T) {
	env, c := startEnv(t)
	defer func() { _ = env.Stop() }()
	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-ep", nil)
	ws := newWorkspace(t, c, ns, "ws-ep", "tpl-ep", nil)
	key := types.NamespacedName{Name: ws.Name, Namespace: ns}
	r := newReconciler(c)

	reconcile(t, r, key)
	pod := thePod(t, c, ns, ws.UID)
	markPodReadyOnly(t, c, pod) // kubelet says Ready; no EndpointSlice yet
	res := reconcile(t, r, key)

	got := getWorkspace(t, c, key)
	if got.Status.Phase == cdiv1.WorkspacePhaseReady {
		t.Fatalf("phase Ready without a ready endpoint (conds=%v)", got.Status.Conditions)
	}
	if rt := condition(got, cdiv1.ConditionRuntimeReady); rt == nil || rt.Status != metav1.ConditionTrue {
		t.Fatalf("RuntimeReady = %+v, want True", rt)
	}
	cc := condition(got, cdiv1.ConditionConnectionReady)
	if cc == nil || cc.Status != metav1.ConditionFalse || cc.Reason != "WaitingForEndpoint" {
		t.Fatalf("ConnectionReady = %+v, want False/WaitingForEndpoint", cc)
	}
	if res.RequeueAfter != requeueEndpointWait {
		t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, requeueEndpointWait)
	}

	publishReadyEndpoint(t, c, pod)
	reconcile(t, r, key)
	if got = getWorkspace(t, c, key); got.Status.Phase != cdiv1.WorkspacePhaseReady {
		t.Fatalf("phase %q after the slice appeared, want Ready (conds=%v)", got.Status.Phase, got.Status.Conditions)
	}
}
```

In `docs/lifecycle-reasons.md`, add a row to the "Starting" table after `NotReady`: ``| `WaitingForEndpoint` | `ConnectionReady` only: the desktop answers inside its pod (`RuntimeReady` is `True`), and the cluster has not yet routed the workspace Service to it (seconds) |``.

Portal wording for the new token (an unknown token renders as a generic "working on it" step with the raw token, and `derive.ts` flags it `unknownReason`):
- `web/src/progress/derive.ts`: add `"WaitingForEndpoint",` to `KNOWN_REASONS` after `"WaitingForDisk",`. Do not add it to `DESKTOP_REASONS`: it appears on `ConnectionReady` while `RuntimeReady` is `True`, so the existing `connect` step (`reason: conn.reason`) carries it.
- `web/src/workspaces/reasons.tsx`: add `WaitingForEndpoint: "workspaces.reason.waitingForEndpoint",` to the "Pod observation tokens" group.
- `web/src/i18n/en/workspaces.ts`: `"workspaces.reason.waitingForEndpoint": "The desktop is up; the cluster is routing connections to it.",` and in `vi/workspaces.ts`: `"workspaces.reason.waitingForEndpoint": "Desktop đã chạy; cụm đang định tuyến kết nối tới nó.",` (next to `workspaces.reason.notReady`).
- `web/tests/unit/workspaces/reasons.test.tsx`: add `"WaitingForEndpoint"` to the `tokens` array.
- `web/tests/unit/progress/derive.test.ts`: add

```ts
  it("the connect step carries WaitingForEndpoint as a known reason", () => {
    const m = deriveProgress(
      ws({
        conditions: [
          cond("StorageReady", "True", "Ready"),
          cond("RuntimeReady", "True", "Ready"),
          cond("ConnectionReady", "False", "WaitingForEndpoint"),
        ],
      }),
      undefined,
      NOW,
    );
    expect(active(m).id).toBe("connect");
    expect(active(m).reason).toBe("WaitingForEndpoint");
    expect(active(m).unknownReason).toBeFalsy();
  });
```

(place it next to the test at `derive.test.ts:77` that already asserts an active `connect` step; if `unknownReason` is not on the step type used by `active()`, assert `m!.steps[m!.current].unknownReason`.)

Run, in order. Expected: all PASS. The three web commands cover typecheck, strings and the touched units:

```bash
go test ./internal/runtime/linux/ -run TestObserve_ConnectionReadyNeedsReadyEndpoint -count=1
KUBEBUILDER_ASSETS="$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.23 use -p path 1.33.x)" go test ./internal/operator/ ./internal/broker/ ./tests/integration/ -count=1
(cd web && npm run typecheck && npm run lint:strings && npx vitest run tests/unit/progress tests/unit/workspaces tests/unit/i18n)
```

(Use the same `setup-envtest` invocation Task 6 Step 3 uses if it differs. The `tests/integration` and `internal/broker` packages also need Docker for Postgres; `go test ./internal/operator/ ./internal/runtime/... -count=1` alone is the minimum when Docker is unavailable, and CI runs the rest.)

- [ ] **Step 4: Gateway dial retry, test first**

`internal/gateway/dial_test.go`:

```go
package gateway_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

// TestProxy_DialRetriesUntilUpstreamListens: the first request after Ready
// can race the Service route; the gateway retries the connect inside its
// budget instead of answering 502.
func TestProxy_DialRetriesUntilUpstreamListens(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("up")) })
	first := httptest.NewTLSServer(ok)
	addr, tlsCfg := first.Listener.Addr().String(), first.TLS
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-cold", testWSUID)
	fb.pointLeaseAt(t, "tk-cold", first)
	srv := newGateway(t, fb, func(c *gateway.Config) { c.UpstreamDialBudget = 5 * time.Second })
	cookie := launchOK(t, srv, testHost, "tk-cold")
	first.Close() // nothing listens now

	go func() {
		time.Sleep(700 * time.Millisecond)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return
		}
		second := &httptest.Server{Listener: ln, TLS: tlsCfg, Config: &http.Server{Handler: ok}}
		second.StartTLS()
		t.Cleanup(second.Close)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/", nil)
	req.Host = testHost
	req.Header.Set("Cookie", gateway.SessionCookieName+"="+cookie)
	resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 after the upstream came up inside the budget", resp.StatusCode)
	}
}

func TestProxy_DialBudgetExhaustedIs502(t *testing.T) {
	dead := httptest.NewTLSServer(http.NotFoundHandler())
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-dead", testWSUID)
	fb.pointLeaseAt(t, "tk-dead", dead)
	srv := newGateway(t, fb, func(c *gateway.Config) { c.UpstreamDialBudget = 300 * time.Millisecond })
	cookie := launchOK(t, srv, testHost, "tk-dead")
	dead.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Host = testHost
	req.Header.Set("Cookie", gateway.SessionCookieName+"="+cookie)
	start := time.Now()
	resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(resp)
	if resp.StatusCode != http.StatusBadGateway || time.Since(start) > 3*time.Second {
		t.Fatalf("status %d after %v, want 502 within the budget", resp.StatusCode, time.Since(start))
	}
}
```

Run `go test ./internal/gateway/ -run 'TestProxy_Dial' -count=1`. Expected: FAIL to compile (`UpstreamDialBudget` undefined).

- [ ] **Step 5: Implement the retrying dialer**

`internal/gateway/dial.go`:

```go
package gateway

import (
	"context"
	"net"
	"time"
)

// UpstreamDialBudget is the default total time the gateway keeps retrying a
// refused or unreachable upstream connect before answering 502 (F2).
const UpstreamDialBudget = 15 * time.Second

// dialWithRetry returns a DialContext that retries failed connects with
// exponential backoff (200 ms doubling to 2 s) until budget elapses or ctx
// ends. Each attempt is bounded by 3 s. A cold workspace's Service route can
// trail its pod's readiness by a moment; a refused first dial must not
// become the user's first impression.
func dialWithRetry(budget time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		deadline := time.Now().Add(budget)
		backoff := 200 * time.Millisecond
		for {
			conn, err := d.DialContext(ctx, network, addr)
			if err == nil {
				return conn, nil
			}
			if ctx.Err() != nil || time.Now().Add(backoff).After(deadline) {
				return nil, err
			}
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, err
			case <-timer.C:
			}
			if backoff *= 2; backoff > 2*time.Second {
				backoff = 2 * time.Second
			}
		}
	}
}
```

In `proxy.go`:
- add `UpstreamDialBudget time.Duration` to `Config` with a doc comment ("total retry budget for refused upstream connects; default UpstreamDialBudget");
- in the defaults block next to `UpstreamResponseHeaderTimeout`, add `if cfg.UpstreamDialBudget <= 0 { cfg.UpstreamDialBudget = UpstreamDialBudget }`;
- in `ensureTarget`'s transport, add `DialContext: dialWithRetry(g.cfg.UpstreamDialBudget),`.

Run `go test ./internal/gateway/ -count=1`. Expected: PASS, including `TestProxy_UpstreamResponseHeaderTimeout`.

- [ ] **Step 6: Post-fix measurement (when Step 1 had a cluster)**

Rebuild the quickstart from this tree (`hack/quickstart/down.sh && hack/quickstart/up.sh` builds the images from the working tree and `kind load`s them), then run the benchmark again with the gate OFF and compare with the Step 1 JSON:

```bash
cd hack/quickstart/smoke && export KUBECONFIG="$HOME/.local/state/tcdi-quickstart/${TCDI_QS_CLUSTER:-tcdi-quickstart}/kubeconfig"
TCDI_QS_COLD_RUNS=5 TCDI_QS_COLD_P95_MS=100000000 npx playwright test -c playwright.cold-connect.config.ts > /tmp/cold-post.log 2>&1; echo "rc=$?"
jq -c '{p95Ms, up5xx:([.runs[].upstream5xx]|add), posts:([.runs[].connectionPosts]|max)}' test-results/cold-connect.json
```

Expected: `rc=0`, `up5xx` 0, and `p95Ms` not worse than the Step 1 baseline. Put both JSON summaries in the task report. A remaining `launchStatus != 303` or `posts > 1` is what Task 10 fixes. Skip this step only when no cluster is available (say so in the report).

- [ ] **Step 7: Commit**

```bash
make manifests
go build ./... && go vet ./... && go test -count=1 ./internal/runtime/... ./internal/gateway/ ./deploy/helm/
KUBEBUILDER_ASSETS="$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.23 use -p path 1.33.x)" go test -count=1 ./internal/operator/
(cd web && npm run typecheck && npm run lint:strings && npx vitest run)
git add internal/runtime internal/operator internal/gateway internal/broker tests/integration config/rbac deploy/helm docs/lifecycle-reasons.md web/src web/tests
git commit -m "fix(connect): gate Ready on the pod's ready endpoint and retry the upstream dial

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

The envtest operator suite must pass before the commit. The spike spec and config were committed in Step 1.

---
### Task 10: Portal re-mints a cold first connect; CI cold-connect gate (F2, part 2) — tier: integrate

**Files:**
- Create: `web/src/session/coldConnect.ts`, `web/tests/unit/session/coldConnect.test.ts`
- Modify: `web/src/session/SessionPage.tsx` (`SessionPageProps` ~75, `launch` ~350-400, `forget` ~220, `relaunch`/`checkEnded` ~523-545, the `useConnectionWatch` `onEvent` ~606, the mount cleanup ~684, `onFrameLoad` ~846), `web/tests/unit/session/SessionPage.test.tsx`
- Modify: `.github/workflows/ci.yml` (job `quickstart`, display name `quickstart (kind up.sh + portal smoke)`, `timeout-minutes: 45` at ~455: a gate step, a summary step, a report upload)

**Interfaces:**
- Consumes: Task 9's `hack/quickstart/smoke/cold-connect.spec.ts` + `playwright.cold-connect.config.ts` (they live in `hack/quickstart/smoke/`, NOT `web/`) and its report `test-results/cold-connect.json` (`runs[].readyToConnectedMs`, `runs[].wallMs`, `runs[].upstream5xx`, `runs[].connectionPosts`, `p95Ms`) and env (`TCDI_QS_COLD_RUNS`, `TCDI_QS_COLD_P95_MS`); `ConnectionStatus` (`state`, `leaseActive`; from `components["schemas"]["ConnectionStatus"]`), `launch(mode, takeover)`, `fetchConnection()`, `checkEnded()`, `useConnectionWatch`'s `relaunched` / `frame-navigated` events.
- Produces:
  - `export const COLD_CONFIRM_MS = 8_000; export const COLD_BACKOFF_MS: readonly number[] = [1_000, 2_000, 4_000]; export const COLD_BUDGET_MS = 60_000;`
  - `export interface ColdAttempt { startedAt: number; relaunches: number }`
  - `export function startCold(now: number): ColdAttempt`
  - `export function streamConfirmed(s: Pick<ConnectionStatus, "state" | "leaseActive">): boolean` (`connected` already means "an open stream on a live lease"; the epoch is not consulted, because a backend without a session directory reports epoch 0 for every stream)
  - `export type ColdStep = { kind: "relaunch"; delayMs: number } | { kind: "give-up" }`
  - `export function nextColdStep(a: ColdAttempt, now: number, confirmMs = COLD_CONFIRM_MS): ColdStep`
  - `SessionPageProps.coldConfirmMs?: number` (default `COLD_CONFIRM_MS`; injectable for tests like `loadTimeoutMs`)
- Why a re-mint (plan ruling 9, verified): `broker.RedeemTicket` (`internal/broker/tickets.go:447`) marks the ticket consumed on the first redemption attempt, even an expired one; `TicketTTL` is 60 s (`tickets.go:63`) and a redeem happens within a second of the mint, so the ticket's lifetime is not the limit: single use is. A redeem that fails therefore cannot be retried with the same ticket. The existing `useConnectionWatch` already repairs a stream that dies after the frame loaded (frame reload while the lease is live, a takeover-less re-mint when the lease is gone, at most 2 per 5 minutes, first action 5 s plus poll after Connected). The cold window below must not fight it: when the watch navigates or re-mints, the cold window ends and the watch owns recovery.

- [ ] **Step 1: Failing unit tests**

`web/tests/unit/session/coldConnect.test.ts`:

```ts
import { describe, expect, it } from "vitest";
import {
  COLD_BACKOFF_MS,
  COLD_BUDGET_MS,
  COLD_CONFIRM_MS,
  nextColdStep,
  startCold,
  streamConfirmed,
} from "../../../src/session/coldConnect";

describe("streamConfirmed", () => {
  it("needs an open stream on a live lease", () => {
    expect(streamConfirmed({ state: "connected", leaseActive: true })).toBe(true);
    expect(streamConfirmed({ state: "disconnected", leaseActive: true })).toBe(false);
    expect(streamConfirmed({ state: "stale", leaseActive: true })).toBe(false);
    expect(streamConfirmed({ state: "none", leaseActive: false })).toBe(false);
    expect(streamConfirmed({ state: "connected", leaseActive: false })).toBe(false);
  });
});

describe("nextColdStep", () => {
  it("backs off 1 s, 2 s, 4 s, then keeps 4 s", () => {
    const a = startCold(0);
    const delays: number[] = [];
    let now = COLD_CONFIRM_MS;
    for (let i = 0; i < 4; i++) {
      const step = nextColdStep(a, now);
      if (step.kind !== "relaunch") throw new Error(`gave up at ${i}`);
      delays.push(step.delayMs);
      a.relaunches++;
      now += step.delayMs + COLD_CONFIRM_MS;
    }
    expect(delays).toEqual([...COLD_BACKOFF_MS, COLD_BACKOFF_MS[COLD_BACKOFF_MS.length - 1]]);
  });

  it("gives up when another confirm window would end past the budget", () => {
    const a = startCold(0);
    expect(nextColdStep(a, COLD_BUDGET_MS - COLD_CONFIRM_MS - 500).kind).toBe("give-up");
    expect(nextColdStep(startCold(0), COLD_CONFIRM_MS).kind).toBe("relaunch");
  });
});
```

Run `cd web && npx vitest run tests/unit/session/coldConnect.test.ts`. Expected: FAIL (module not found).

- [ ] **Step 2: Implement `coldConnect.ts`**

```ts
import type { components } from "../api/generated/schema";

type ConnectionStatus = components["schemas"]["ConnectionStatus"];

// Cold first connect (F2). A freshly Ready workspace can load the session
// frame before its stream comes up. Launch tickets are single-use (the
// gateway consumes one on the first redeem attempt), so the portal recovers
// by minting a NEW ticket, with backoff, inside a 60 s budget, and never by
// reloading the old one.

/** How long after the frame loads the stream must be confirmed. */
export const COLD_CONFIRM_MS = 8_000;
/** Delay before each re-mint; the last value repeats. */
export const COLD_BACKOFF_MS: readonly number[] = [1_000, 2_000, 4_000];
/** Total time from the first ticket to giving up. */
export const COLD_BUDGET_MS = 60_000;

export interface ColdAttempt {
  startedAt: number;
  relaunches: number;
}

export type ColdStep = { kind: "relaunch"; delayMs: number } | { kind: "give-up" };

export function startCold(now: number): ColdAttempt {
  return { startedAt: now, relaunches: 0 };
}

/**
 * The /connection report proves a stream is up: `connected` is "an open
 * stream on a live lease" (broker connection_state.go). The stream epoch is
 * not consulted: a backend without a session directory reports epoch 0
 * for every stream, and requiring epoch > 0 would re-mint forever there.
 */
export function streamConfirmed(s: Pick<ConnectionStatus, "state" | "leaseActive">): boolean {
  return s.state === "connected" && s.leaseActive;
}

/** What to do after a confirm window ended without a confirmed stream. */
export function nextColdStep(a: ColdAttempt, now: number, confirmMs: number = COLD_CONFIRM_MS): ColdStep {
  const delayMs = COLD_BACKOFF_MS[Math.min(a.relaunches, COLD_BACKOFF_MS.length - 1)];
  if (now + delayMs + confirmMs > a.startedAt + COLD_BUDGET_MS) return { kind: "give-up" };
  return { kind: "relaunch", delayMs };
}
```

`SessionPage.tsx` imports the same `../api/generated/schema` path (line 14). Run the Step 1 command. Expected: PASS.

- [ ] **Step 3: Wire it into `SessionPage`**

In `SessionPage.tsx` (import `COLD_CONFIRM_MS, nextColdStep, startCold, streamConfirmed, type ColdAttempt` from `./coldConnect`):

1. Props: add `coldConfirmMs?: number` to `SessionPageProps` ("How long after the frame loads the stream must show on /connection before a fresh ticket is minted (F2). Injectable for tests.") and `coldConfirmMs = COLD_CONFIRM_MS` to the destructured defaults.
2. Refs, next to `loadTimer`: `const cold = useRef<ColdAttempt | null>(null);`, `const coldTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);`, `const coldRelaunch = useRef(false);`. In the mount effect's cleanup (next to `clearLoadTimer()`), add `clearTimeout(coldTimer.current);`.
3. `forget()` (~220) also ends the window: add `cold.current = null; clearTimeout(coldTimer.current);` (refs only, so its `[workspaceId]` deps stay).
4. `launch` (~350):
   - at the very top (before the `inflight` guard) consume the flag: `const fromCold = coldRelaunch.current; coldRelaunch.current = false; if (!fromCold) cold.current = null;` A launch the user, the resume fallback or the first load starts opens a fresh 60 s budget; only `checkCold`'s own relaunch keeps the running one. (If `inflight` swallows a cold relaunch, the window just ends: nothing re-arms it.)
   - in the `"frame"` branch, right after `submitLaunch(ticket, frameName, workspaceId, sessionDomain)`: `if (cold.current === null) cold.current = startCold(Date.now());`. The `"tab"` branch already calls `forget()`, which clears it.
5. In `useConnectionWatch`'s `onEvent`, the `"relaunched"` and `"frame-navigated"` cases also end the cold window (`cold.current = null; clearTimeout(coldTimer.current);`): once the watch has navigated the frame or minted a ticket, it owns recovery, and a cold takeover must not revoke the lease its claim is in flight on.
6. Add, right after `checkEnded` (~545), so that `launch` and `checkEnded` are in scope:

```tsx
  // F2: after the frame loads, the stream must show up on /connection
  // within coldConfirmMs; otherwise mint a fresh ticket (tickets are
  // single-use) with backoff, inside COLD_BUDGET_MS. A confirmed stream, a
  // watch recovery, or any later user-driven launch ends the cold window.
  const checkCold = useCallback(async () => {
    const attempt = cold.current;
    if (!attempt || !mounted.current) return;
    let confirmed = false;
    try {
      confirmed = streamConfirmed(await fetchConnection());
    } catch (e) {
      if (isUnauthenticated(e)) {
        cold.current = null;
        dispatch({ type: "signed-out" });
        return;
      }
    }
    // Ended, replaced or handed to the watch while the poll was in flight.
    if (!mounted.current || cold.current !== attempt) return;
    if (confirmed) {
      cold.current = null;
      return;
    }
    const step = nextColdStep(attempt, Date.now(), coldConfirmMs);
    if (step.kind === "give-up") {
      cold.current = null;
      setRecovering(false);
      dispatch({ type: "offline", reason: "exhausted" }); // Reconnect / open in a new tab
      void checkEnded();
      return;
    }
    attempt.relaunches++;
    coldTimer.current = setTimeout(() => {
      if (!mounted.current || cold.current !== attempt) return;
      coldRelaunch.current = true;
      setFrameKey((k) => k + 1);
      void launch("frame", true);
    }, step.delayMs);
  }, [fetchConnection, launch, checkEnded, coldConfirmMs]);

  // The window only lives while a launch is in flight or its desktop is up.
  useEffect(() => {
    const s = state.status;
    if (s !== "requesting" && s !== "connecting" && s !== "connected") {
      cold.current = null;
      clearTimeout(coldTimer.current);
    }
  }, [state.status]);
```

7. In `onFrameLoad`, after `dispatch({ type: "frame-loaded" });` add `if (cold.current) { clearTimeout(coldTimer.current); coldTimer.current = setTimeout(() => void checkCold(), coldConfirmMs); }`. `onFrameLoad` is a plain function, so it always reads the latest `checkCold`.

`armLoadTimer` is NOT changed. A frame that never loads (embedding refused, network swallowed) keeps ending in `blocked/timeout` after `loadTimeoutMs`, exactly as the existing tests expect; the cold window only starts to matter once a frame has loaded. A `launch` with `takeover: true` supersedes this tab's own stalled lease, which is the designed answer for "my previous stream is gone". If another tab took the session over meanwhile, `checkCold`'s `fetchConnection()` reports that tab's `connected` stream and the window ends without a takeover.

Add the test to `web/tests/unit/session/SessionPage.test.tsx` (append a new `describe`; it uses the file's `setupScripted`, `leaseRefOf`, `fireEvent`, `screen`, `waitFor` and real timers with small intervals). jsdom never loads the cross-origin session document and reports `about:blank`, which `onFrameLoad` ignores, so the test makes `contentWindow.location` unreadable like a real cross-origin frame:

```tsx
describe("SessionPage cold connect (F2)", () => {
  it("cold connect re-mints a ticket once and stops when the stream is confirmed", async () => {
    vi.spyOn(HTMLIFrameElement.prototype, "contentWindow", "get").mockReturnValue({
      get location(): Location {
        throw new DOMException("cross-origin", "SecurityError");
      },
    } as unknown as Window);
    const { ws, control, submitted, ticketPosts } = setupScripted({
      lease: false,
      marker: false,
      props: { pollIntervalMs: 20, inFrameRetryMs: 60_000, coldConfirmMs: 150 },
    });
    const ref = leaseRefOf("lease_cold");
    // The lease is live but the stream never claims: the cold stall.
    control.connection = () => ({ state: "disconnected", leaseActive: true, leaseRef: ref, streamEpoch: 0 });

    await waitFor(() => expect(submitted).toHaveLength(1));
    fireEvent.load(await screen.findByTitle(`Desktop: ${ws.name}`));

    // 150 ms confirm window + the 1 s first backoff: one re-mint, as a takeover.
    await waitFor(() => expect(ticketPosts()).toHaveLength(2), { timeout: 4_000 });
    expect(JSON.parse(ticketPosts()[0].rawBody)).toEqual({ takeover: false });
    expect(JSON.parse(ticketPosts()[1].rawBody)).toEqual({ takeover: true });
    await waitFor(() => expect(submitted).toHaveLength(2));

    // The second attempt's stream comes up: no third mint, ever.
    control.connection = () => ({ state: "connected", leaseActive: true, leaseRef: ref, streamEpoch: 1 });
    fireEvent.load(await screen.findByTitle(`Desktop: ${ws.name}`));
    await new Promise((r) => setTimeout(r, 600)); // > coldConfirmMs + a poll
    expect(ticketPosts()).toHaveLength(2);
  });
});
```

Run:

```bash
cd web && npm run typecheck && npx vitest run tests/unit/session > /tmp/t10-unit.log 2>&1; echo "rc=$?"; grep -nE 'FAIL|✗|×' /tmp/t10-unit.log | head
npx playwright test > /tmp/t10-pw.log 2>&1; echo "rc=$?"; grep -nE 'failed|✘' /tmp/t10-pw.log | head
```

Expected: `rc=0` twice, no failure lines. The existing session unit tests keep passing because the cold window only runs after a `frame-loaded`, which jsdom never fires in them; the mock API reports `connected` for every minted lease, so the Playwright specs confirm at once and the cold path stays idle.

- [ ] **Step 4: CI gate in the quickstart job**

In `.github/workflows/ci.yml`, job `quickstart` (display name `quickstart (kind up.sh + portal smoke)`; keep the name):
- `timeout-minutes: 45` (~455) becomes `timeout-minutes: 75` (ruling 10). Budget check: Task 9's report has `wallMs` per run (create to accepted delete, release_pending wait included). The cold step costs about `20 * median(wallMs)`; require `45 + ceil(20 * median(wallMs) / 60000) + 5 <= timeout-minutes`, so a median above ~1.5 min per run needs a larger value (`jq '[.runs[].wallMs]|sort|.[length/2|floor]' test-results/cold-connect.json` on the Task 9 spike report; raise to the next multiple of 5 and say so in the task report);
- after the `portal smoke` step (~520-526) and before `diagnostics`, add the gate, a summary that also runs on failure, and the report upload:

```yaml
      - name: cold-connect gate (F2, 20 cold boots, p95 < 20 s)
        if: steps.gate.outputs.run == 'true'
        working-directory: hack/quickstart/smoke
        env:
          TCDI_QS_COLD_RUNS: "20"
          TCDI_QS_COLD_P95_MS: "20000"
        run: |
          export KUBECONFIG="$HOME/.local/state/tcdi-quickstart/${TCDI_QS_CLUSTER}/kubeconfig"
          # The spec itself fails when p95 >= TCDI_QS_COLD_P95_MS ...
          npx playwright test -c playwright.cold-connect.config.ts
          # ... and this re-checks the recorded report: all 20 runs present, p95 < 20000.
          jq -e --argjson n "$TCDI_QS_COLD_RUNS" --argjson max "$TCDI_QS_COLD_P95_MS" \
            '(.runs | length) == $n and .p95Ms < $max' test-results/cold-connect.json
      - name: cold-connect summary
        if: always() && steps.gate.outputs.run == 'true'
        working-directory: hack/quickstart/smoke
        run: |
          if [ -f test-results/cold-connect.json ]; then
            jq -r '"cold-connect: p95 \(.p95Ms) ms, worst \([.runs[].readyToConnectedMs] | max) ms over \(.runs | length) runs; gateway 5xx \([.runs[].upstream5xx] | add); extra ticket mints \([.runs[].connectionPosts - 1] | add)"' \
              test-results/cold-connect.json | tee -a "$GITHUB_STEP_SUMMARY"
          else
            echo "cold-connect: no report written" | tee -a "$GITHUB_STEP_SUMMARY"
          fi
      - name: upload cold-connect report
        if: always() && steps.gate.outputs.run == 'true'
        uses: actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a  # v7.0.1
        with:
          name: cold-connect
          path: hack/quickstart/smoke/test-results/cold-connect.json
          if-no-files-found: ignore
```

The exact local command for the same gate (needs the kind quickstart up): `cd hack/quickstart/smoke && TCDI_QS_COLD_RUNS=20 TCDI_QS_COLD_P95_MS=20000 npx playwright test -c playwright.cold-connect.config.ts && jq -e '(.runs | length) == 20 and .p95Ms < 20000' test-results/cold-connect.json`; either `npx` or `jq` exits non-zero when p95 is 20000 ms or more.

Validate the workflow locally (CI also runs actionlint, yamllint and zizmor over it): `actionlint .github/workflows/ci.yml` if installed, else `python3 -c 'import yaml; yaml.safe_load(open(".github/workflows/ci.yml"))'`. Expected: no errors. The step name avoids `: ` and `${{ }}` so the YAML stays valid and zizmor has nothing to flag.

- [ ] **Step 5: Commit**

```bash
cd web && npm run typecheck && npm run lint:strings && npx vitest run && cd ..
git add web/src/session web/tests/unit/session .github/workflows/ci.yml
git commit -m "fix(session): re-mint a cold first connect with backoff; gate cold-connect p95 in CI

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 11: Transfer rows: store, broker claim/complete with policy recheck, gateway surface (F4, part 2) — tier: design

Task 11 and Task 11a together are the old single "Task 11" (it was ~1150 lines across 25 files). Task 11 is the security-critical core (rows, atomic claim, recheck, audit) plus the mechanical interface widening; Task 11a is the mint API, wiring and sweep. Each leaves build and tests green: until 11a installs the resolver, `ClaimTransfer` refuses everything (`ErrTransferDenied`), and nothing mints rows yet.

`session_file_transfer` already exists in `internal/store/migrations/0001_baseline.sql:336` (W0) with the ADR T11 columns (id, workspace_id, tenant, principal_issuer, principal_subject, runtime_generation, runtime_uid, direction, path, declared_bytes, bytes_done, status `pending|claimed|completed|failed|expired`, resolved_policy, created_at, expires_at, claimed_at, completed_at). This task adds no schema.

**Files:**
- Create: `internal/store/filetransfer.go`, `internal/store/filetransfer_test.go`
- Create: `internal/broker/transfers.go`, `internal/broker/transfers_test.go`
- Modify: `internal/broker/tickets.go:201` (field `Broker.transferPolicy`), `internal/broker/gatewayshim.go` (`GatewaySurface`, `LocalGateway`, `TransferIDOK`), `internal/broker/gatewayshim_internal_test.go:16` (`recordingSurface`)
- Modify: `internal/broker/httpapi/server.go` (`BrokerAPI`, two routes, `brokerError`), `internal/broker/httpapi/server_test.go` (`fakeBroker`, new test), `internal/broker/httpapi/opclient/client_test.go:33` (`stubBroker`)
- Modify: `internal/gateway/brokerclient/{client.go,client_test.go}` (two methods, `sentinelFor`, fake `/transfers/` routes), `internal/gateway/proxy.go:37-48` (`BrokerClient`), `internal/gateway/helpers_test.go:61,99` (`fakeBroker`, `newFakeBroker`)
- Modify: `internal/backend/backend_test.go` (`fakeBrokerClient` ~69, `fakeInternalBroker` ~430, `internalAdapter` ~1240), `internal/backend/metrics_test.go:130` (`stubBrokerClient`)

**Interfaces:**
- Consumes: `store.InsertAudit` and the `success|denied|failure` outcome vocabulary (Task 1); `Broker.liveLease`, `Broker.loadLease`, `Broker.boundCurrent` (`leases.go`); `observability.ActorRef`.
- Produces (Tasks 11a, 13 and 14 rely on these):
  - `type store.FileTransfer struct{ ID, WorkspaceID, Tenant, PrincipalIssuer, PrincipalSubject string; RuntimeGeneration int64; RuntimeUID, Direction, Path string; DeclaredBytes int64; ResolvedPolicy string; ExpiresAt time.Time }`
  - `func store.InsertFileTransfer(ctx context.Context, db *store.DB, t store.FileTransfer) error` (runs in `db.WithTx`, so a request's audit row commits or rolls back with it), `func (d *store.DB) SweepFileTransfers(ctx context.Context, now time.Time) (expired, failed, pruned int64, err error)`
  - `const store.FileTransferTTL = 2 * time.Minute`, `store.FileTransferClaimDeadAfter = 30 * time.Minute`, `store.FileTransferKeep = 7 * 24 * time.Hour`
  - `type broker.TransferPolicy struct{ Mode string; MaxBytes int64 }`, `type broker.TransferPolicyResolver func(ctx context.Context, tenantID, workspaceID string) (broker.TransferPolicy, error)`, `func (b *broker.Broker) WithTransferPolicy(r broker.TransferPolicyResolver) *broker.Broker`
  - `type broker.TransferGrant struct{ ID, WorkspaceUID, Direction, Path string; MaxBytes int64 }` (JSON tags `id`, `workspaceUID`, `direction`, `path`, `maxBytes`)
  - `func (b *broker.Broker) ClaimTransfer(ctx context.Context, gw broker.GatewayIdentity, leaseID, transferID, method string) (broker.TransferGrant, error)`
  - `func (b *broker.Broker) CompleteTransfer(ctx context.Context, gw broker.GatewayIdentity, leaseID, transferID string, bytesDone int64, ok bool) error`
  - `broker.ErrTransferInvalid`, `broker.ErrTransferDenied`, `broker.ErrTransferBusy`
  - `func broker.TransferIDOK(id string) bool` (`^xf_[a-z2-7]{26}$`)
  - The same two methods on `broker.GatewaySurface`, `*broker.LocalGateway`, `httpapi.BrokerAPI`, `gateway.BrokerClient` and `*brokerclient.Client`. Internal routes: `POST /internal/v1/broker/leases/{id}/transfers/{tid}/claim` (`{"method":"GET|PUT"}` → `TransferGrant` JSON) and `POST /internal/v1/broker/leases/{id}/transfers/{tid}/complete` (`{"bytes":n,"ok":bool}` → 204). Internal-only error codes `TRANSFER_INVALID` 410, `TRANSFER_DENIED` 403, `TRANSFER_BUSY` 409 (same status as the lease errors, told apart by code; they are not public `ErrorCode`s and need no `AllErrorCodes` entry, like `REVOKED`).
  - Broker audit events (Task 1 `audit_event`, one row in the same transaction as the state change): action `file.transfer.upload|download`, target kind `workspace`, target id = workspace UID, request id = transfer id, `details`: `phase` (`claim|complete`), `direction`, `path`, `declared`, `policy`, plus `bytes` (transferred) on complete and `reason` (`policy|busy|mismatch`) on a refusal. Outcomes `success`, `denied` (claim refusals, error code `FORBIDDEN` or `INVALID_STATE`), `failure` (a stream that ended not-ok).
- Fake-broker note for Task 13: this task adds the scriptable gateway `fakeBroker` transfer methods (`transfers`, `claimErr`, `completed`, `completions()`), because widening `gateway.BrokerClient` breaks the build otherwise. Task 13 Step 1 must not add them again.

- [ ] **Step 1: Store: insert and sweep, tests first**

`internal/store/filetransfer_test.go`:

```go
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func transferRow(id, ws string, expires time.Time) store.FileTransfer {
	return store.FileTransfer{
		ID: id, WorkspaceID: ws, Tenant: "t1", PrincipalIssuer: "https://idp", PrincipalSubject: "alice",
		RuntimeGeneration: 1, RuntimeUID: "uid-1", Direction: "upload", Path: "report.pdf",
		DeclaredBytes: 10, ResolvedPolicy: "Bidirectional", ExpiresAt: expires,
	}
}

func transferStatus(t *testing.T, db *store.DB, id string) string {
	t.Helper()
	var s string
	if err := db.Pool().QueryRow(context.Background(), `SELECT status FROM session_file_transfer WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFileTransfer_InsertAndSweep(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	now := time.Now()
	seedWorkspace(t, db, "t1", "https://idp|alice", "ws-x")
	for _, r := range []store.FileTransfer{
		transferRow("xf_live", "ws-x", now.Add(time.Minute)),
		transferRow("xf_stale", "ws-x", now.Add(-time.Second)),
		transferRow("xf_dead", "ws-x", now.Add(time.Minute)),
		transferRow("xf_old", "ws-x", now.Add(-8*24*time.Hour)),
	} {
		if err := store.InsertFileTransfer(ctx, db, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE session_file_transfer SET status = 'claimed', claimed_at = $1 WHERE id = 'xf_dead'`, now.Add(-31*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE session_file_transfer SET status = 'completed', created_at = $1 WHERE id = 'xf_old'`, now.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	expired, failed, pruned, err := db.SweepFileTransfers(ctx, now)
	if err != nil || expired != 1 || failed != 1 || pruned != 1 {
		t.Fatalf("sweep = %d/%d/%d %v, want 1/1/1", expired, failed, pruned, err)
	}
	if transferStatus(t, db, "xf_live") != "pending" || transferStatus(t, db, "xf_stale") != "expired" || transferStatus(t, db, "xf_dead") != "failed" {
		t.Fatal("wrong states after sweep")
	}
	if err := store.InsertFileTransfer(ctx, db, transferRow("xf_live", "ws-x", now)); err == nil {
		t.Fatal("duplicate id accepted")
	}
}
```

`internal/store/filetransfer.go`:

```go
package store

import (
	"context"
	"fmt"
	"time"
)

// File-transfer row lifetimes (ADR 0010 T11; plan ruling 15).
const (
	// FileTransferTTL is how long a minted row may wait for its claim.
	FileTransferTTL = 2 * time.Minute
	// FileTransferClaimDeadAfter is how long a claimed row counts as a live
	// stream; past it the sweep fails the row and frees the workspace.
	FileTransferClaimDeadAfter = 30 * time.Minute
	// FileTransferKeep is how long finished rows stay for audit joins.
	FileTransferKeep = 7 * 24 * time.Hour
)

// FileTransfer is one minted, single-use transfer grant.
type FileTransfer struct {
	ID                string
	WorkspaceID       string
	Tenant            string
	PrincipalIssuer   string
	PrincipalSubject  string
	RuntimeGeneration int64
	RuntimeUID        string
	Direction         string // upload | download
	Path              string // a single file name inside the transfer root
	DeclaredBytes     int64
	ResolvedPolicy    string // the file-transfer mode in force at mint
	ExpiresAt         time.Time
}

// InsertFileTransfer records a pending grant.
func InsertFileTransfer(ctx context.Context, db *DB, t FileTransfer) error {
	// In a transaction so the request-scoped audit hook (Task 1) commits the
	// audit row with the grant, or rolls both back.
	if err := db.WithTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO session_file_transfer (id, workspace_id, tenant, principal_issuer, principal_subject,
				runtime_generation, runtime_uid, direction, path, declared_bytes, resolved_policy, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			t.ID, t.WorkspaceID, t.Tenant, t.PrincipalIssuer, t.PrincipalSubject,
			t.RuntimeGeneration, t.RuntimeUID, t.Direction, t.Path, t.DeclaredBytes, t.ResolvedPolicy, t.ExpiresAt)
		return err
	}); err != nil {
		return fmt.Errorf("store: insert file transfer %w", err)
	}
	return nil
}

// SweepFileTransfers expires unclaimed rows past their TTL, fails claims
// older than FileTransferClaimDeadAfter, and prunes rows created before
// now - FileTransferKeep that are no longer pending or claimed.
func (d *DB) SweepFileTransfers(ctx context.Context, now time.Time) (expired, failed, pruned int64, err error) {
	tag, err := d.pool.Exec(ctx,
		`UPDATE session_file_transfer SET status = 'expired' WHERE status = 'pending' AND expires_at <= $1`, now)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("store: expire transfers %w", err)
	}
	expired = tag.RowsAffected()
	tag, err = d.pool.Exec(ctx,
		`UPDATE session_file_transfer SET status = 'failed', completed_at = $1
		 WHERE status = 'claimed' AND claimed_at <= $2`, now, now.Add(-FileTransferClaimDeadAfter))
	if err != nil {
		return expired, 0, 0, fmt.Errorf("store: fail dead transfers %w", err)
	}
	failed = tag.RowsAffected()
	tag, err = d.pool.Exec(ctx,
		`DELETE FROM session_file_transfer WHERE created_at < $1 AND status NOT IN ('pending', 'claimed')`,
		now.Add(-FileTransferKeep))
	if err != nil {
		return expired, failed, 0, fmt.Errorf("store: prune transfers %w", err)
	}
	return expired, failed, tag.RowsAffected(), nil
}
```

Run `go test ./internal/store/ -run TestFileTransfer -count=1`. Expected: PASS (it fails to compile before the file exists).

- [ ] **Step 2: Broker claim and complete, tests first**

`internal/broker/transfers_test.go` (package `broker_test`, using `setup`, `seedActivityWorkspace`, `alice`, `gwA` from `main_test.go`/`activity_test.go`/`tickets_test.go`):

```go
package broker_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func mintRow(t *testing.T, db *store.DB, id, ws, direction string, gen int64, rtUID string, expires time.Time) {
	t.Helper()
	iss, sub, _ := strings.Cut(alice.Owner(), "|")
	if err := store.InsertFileTransfer(context.Background(), db, store.FileTransfer{
		ID: id, WorkspaceID: ws, Tenant: "tenant-a", PrincipalIssuer: iss, PrincipalSubject: sub,
		RuntimeGeneration: gen, RuntimeUID: rtUID, Direction: direction, Path: "a.txt",
		DeclaredBytes: 10, ResolvedPolicy: "Bidirectional", ExpiresAt: expires,
	}); err != nil {
		t.Fatal(err)
	}
}

func policyMax(mode string, max int64) broker.TransferPolicyResolver {
	return func(context.Context, string, string) (broker.TransferPolicy, error) {
		return broker.TransferPolicy{Mode: mode, MaxBytes: max}, nil
	}
}

func policy(mode string) broker.TransferPolicyResolver { return policyMax(mode, 1<<20) }

func rowStatus(t *testing.T, db *store.DB, id string) string {
	t.Helper()
	var s string
	if err := db.Pool().QueryRow(context.Background(), `SELECT status FROM session_file_transfer WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// auditRows counts audit_event rows of one action and outcome.
func auditRows(t *testing.T, db *store.DB, action, outcome string) int {
	t.Helper()
	var n int
	if err := db.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM audit_event WHERE action = $1 AND outcome = $2`, action, outcome).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestClaimTransfer_HappyPathIsSingleUse(t *testing.T) {
	db, b, clock, src := setup(t)
	b.WithTransferPolicy(policy("Bidirectional"))
	lease, _ := seedActivityWorkspace(t, db, src, clock, "ws_xf1", 1, "uid-1", 1)
	mintRow(t, db, "xf_one", "ws_xf1", "upload", 1, "uid-1", clock.Now().Add(time.Minute))
	g, err := b.ClaimTransfer(context.Background(), gwA, lease, "xf_one", "PUT")
	if err != nil || g.Direction != "upload" || g.Path != "a.txt" || g.MaxBytes != 10 {
		t.Fatalf("grant = %+v, %v; upload cap is min(declared, policy)", g, err)
	}
	if _, err := b.ClaimTransfer(context.Background(), gwA, lease, "xf_one", "PUT"); !errors.Is(err, broker.ErrTransferInvalid) {
		t.Fatalf("second claim: %v, want ErrTransferInvalid", err)
	}
	if err := b.CompleteTransfer(context.Background(), gwA, lease, "xf_one", 10, true); err != nil {
		t.Fatal(err)
	}
	var details string
	_ = db.Pool().QueryRow(context.Background(),
		`SELECT details::text FROM audit_event WHERE action = 'file.transfer.upload' AND details->>'phase' = 'complete'`).Scan(&details)
	if rowStatus(t, db, "xf_one") != "completed" || auditRows(t, db, "file.transfer.upload", "success") != 2 ||
		!strings.Contains(details, `"bytes": "10"`) || !strings.Contains(details, `"declared": "10"`) {
		t.Fatalf("status %q, success audit rows %d, complete details %s; want completed, claim+complete with declared and transferred bytes",
			rowStatus(t, db, "xf_one"), auditRows(t, db, "file.transfer.upload", "success"), details)
	}
}

func TestClaimTransfer_PolicyTightenedAfterMint(t *testing.T) {
	db, b, clock, src := setup(t)
	b.WithTransferPolicy(policy("Download")) // the ceiling moved to Download after an upload was minted
	lease, _ := seedActivityWorkspace(t, db, src, clock, "ws_xf2", 1, "uid-1", 1)
	mintRow(t, db, "xf_up", "ws_xf2", "upload", 1, "uid-1", clock.Now().Add(time.Minute))
	if _, err := b.ClaimTransfer(context.Background(), gwA, lease, "xf_up", "PUT"); !errors.Is(err, broker.ErrTransferDenied) {
		t.Fatalf("claim: %v, want ErrTransferDenied", err)
	}
	if got := rowStatus(t, db, "xf_up"); got != "failed" {
		t.Fatalf("status %q, want failed (the denial is recorded, the row is spent)", got)
	}
	if n := auditRows(t, db, "file.transfer.upload", "denied"); n != 1 {
		t.Fatalf("denied audit rows = %d, want 1 (a refused transfer is as visible as a completed one)", n)
	}

	// The size ceiling shrank below the declared size: same refusal.
	b.WithTransferPolicy(policyMax("Bidirectional", 5))
	mintRow(t, db, "xf_big", "ws_xf2", "upload", 1, "uid-1", clock.Now().Add(time.Minute))
	if _, err := b.ClaimTransfer(context.Background(), gwA, lease, "xf_big", "PUT"); !errors.Is(err, broker.ErrTransferDenied) {
		t.Fatalf("claim over the tightened size: %v, want ErrTransferDenied", err)
	}
}

func TestClaimTransfer_RejectsMismatches(t *testing.T) {
	db, b, clock, src := setup(t)
	b.WithTransferPolicy(policy("Bidirectional"))
	lease, _ := seedActivityWorkspace(t, db, src, clock, "ws_xf3", 2, "uid-2", 1)
	seedActivityWorkspace(t, db, src, clock, "ws_other", 1, "uid-o", 1)
	mintRow(t, db, "xf_method", "ws_xf3", "download", 2, "uid-2", clock.Now().Add(time.Minute))
	mintRow(t, db, "xf_oldgen", "ws_xf3", "upload", 1, "uid-1", clock.Now().Add(time.Minute))
	mintRow(t, db, "xf_foreign", "ws_other", "upload", 1, "uid-o", clock.Now().Add(time.Minute))
	mintRow(t, db, "xf_expired", "ws_xf3", "upload", 2, "uid-2", clock.Now().Add(-time.Second))
	for id, method := range map[string]string{"xf_method": "PUT", "xf_oldgen": "PUT", "xf_foreign": "PUT", "xf_expired": "PUT", "xf_missing": "PUT"} {
		if _, err := b.ClaimTransfer(context.Background(), gwA, lease, id, method); !errors.Is(err, broker.ErrTransferInvalid) {
			t.Errorf("%s: %v, want ErrTransferInvalid", id, err)
		}
	}
	// A mismatch does not spend the row (the wrong-method or stale-incarnation
	// caller is not the grant's holder), and a foreign workspace's id is not
	// ours to audit.
	if rowStatus(t, db, "xf_method") != "pending" || rowStatus(t, db, "xf_foreign") != "pending" {
		t.Fatal("a mismatched claim spent the row")
	}
	if n := auditRows(t, db, "file.transfer.download", "denied") + auditRows(t, db, "file.transfer.upload", "denied"); n != 2 {
		t.Fatalf("denied audit rows = %d, want 2 (xf_method, xf_oldgen)", n)
	}
}

func TestClaimFileTransfer_OnePerWorkspaceConcurrent(t *testing.T) {
	db, b, clock, src := setup(t)
	b.WithTransferPolicy(policy("Bidirectional"))
	lease, _ := seedActivityWorkspace(t, db, src, clock, "ws_xf4", 1, "uid-1", 1)
	mintRow(t, db, "xf_a", "ws_xf4", "upload", 1, "uid-1", clock.Now().Add(time.Minute))
	mintRow(t, db, "xf_b", "ws_xf4", "upload", 1, "uid-1", clock.Now().Add(time.Minute))
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, id := range []string{"xf_a", "xf_b"} {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			_, errs[i] = b.ClaimTransfer(context.Background(), gwA, lease, id, "PUT")
		}(i, id)
	}
	wg.Wait()
	ok, busy := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, broker.ErrTransferBusy):
			busy++
		default:
			t.Fatalf("unexpected: %v", err)
		}
	}
	if ok != 1 || busy != 1 {
		t.Fatalf("ok=%d busy=%d, want exactly one stream per workspace", ok, busy)
	}
	if auditRows(t, db, "file.transfer.upload", "success") != 1 || auditRows(t, db, "file.transfer.upload", "denied") != 1 {
		t.Fatal("want one success and one denied (busy) audit row")
	}
}

func TestCompleteTransfer_BoundToLeaseWorkspaceAndSurvivesRevoke(t *testing.T) {
	db, b, clock, src := setup(t)
	b.WithTransferPolicy(policy("Bidirectional"))
	lease, _ := seedActivityWorkspace(t, db, src, clock, "ws_xf5", 1, "uid-1", 1)
	other, _ := seedActivityWorkspace(t, db, src, clock, "ws_xf6", 1, "uid-6", 1)
	mintRow(t, db, "xf_c", "ws_xf5", "download", 1, "uid-1", clock.Now().Add(time.Minute))
	if _, err := b.ClaimTransfer(context.Background(), gwA, lease, "xf_c", "GET"); err != nil {
		t.Fatal(err)
	}
	if err := b.CompleteTransfer(context.Background(), gwA, other, "xf_c", 5, true); !errors.Is(err, broker.ErrTransferInvalid) {
		t.Fatalf("complete from another workspace's lease: %v, want ErrTransferInvalid", err)
	}
	// The stream was authorized at claim: a revoke mid-stream must still let
	// the row close with its byte count.
	if err := b.RevokeLease(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	if err := b.CompleteTransfer(context.Background(), gwA, lease, "xf_c", 5, false); err != nil {
		t.Fatalf("complete after revoke: %v", err)
	}
	if err := b.CompleteTransfer(context.Background(), gwA, lease, "xf_c", 5, true); !errors.Is(err, broker.ErrTransferInvalid) {
		t.Fatalf("second complete: %v, want ErrTransferInvalid", err)
	}
	if rowStatus(t, db, "xf_c") != "failed" || auditRows(t, db, "file.transfer.download", "failure") != 1 {
		t.Fatal("an ok=false completion must record status failed and a failure audit row")
	}
}
```

Run `go test ./internal/broker/ -run 'ClaimTransfer|ClaimFileTransfer|CompleteTransfer' -count=1`. Expected: FAIL to compile (`WithTransferPolicy`, `ClaimTransfer` undefined).

- [ ] **Step 3: Implement `internal/broker/transfers.go`**

```go
package broker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

var (
	// ErrTransferInvalid: unknown, spent, expired, foreign or mismatched grant.
	ErrTransferInvalid = errors.New("broker: transfer invalid, used or expired")
	// ErrTransferDenied: the policy in force now refuses the direction or size.
	ErrTransferDenied = errors.New("broker: transfer denied by policy")
	// ErrTransferBusy: another transfer stream is live on the workspace.
	ErrTransferBusy = errors.New("broker: a transfer is already in progress")
)

// TransferPolicy is the file-transfer leg of the policy in force now.
type TransferPolicy struct {
	Mode     string // Disabled | Upload | Download | Bidirectional
	MaxBytes int64
}

// TransferPolicyResolver re-resolves the workspace's policy at claim time
// (template ∧ current tenant ceiling ∧ platform bound).
type TransferPolicyResolver func(ctx context.Context, tenantID, workspaceID string) (TransferPolicy, error)

// TransferGrant is what a successful claim lets the gateway move.
type TransferGrant struct {
	ID           string `json:"id"`
	WorkspaceUID string `json:"workspaceUID"`
	Direction    string `json:"direction"`
	Path         string `json:"path"`
	MaxBytes     int64  `json:"maxBytes"`
}

// WithTransferPolicy installs the claim-time policy resolver. Without one,
// every claim is refused (fail closed).
func (b *Broker) WithTransferPolicy(r TransferPolicyResolver) *Broker {
	b.transferPolicy = r
	return b
}

// directionAllowed mirrors provisioning.DirectionAllows on the string mode
// the resolver returns (the broker does not import provisioning).
func directionAllowed(mode, direction string) bool {
	switch direction {
	case "upload":
		return mode == "Upload" || mode == "Bidirectional"
	case "download":
		return mode == "Download" || mode == "Bidirectional"
	}
	return false
}

func methodDirection(method string) string {
	switch method {
	case "PUT":
		return "upload"
	case "GET":
		return "download"
	}
	return ""
}

// ClaimTransfer spends a minted grant for the gateway session holding
// leaseID: pending → claimed in one conditional update, after checking the
// lease is live and bound to the current incarnation, the grant belongs to
// the lease's workspace, principal and incarnation, the method matches the
// direction, no other transfer stream is live on the workspace (the
// workspace row lock serializes claims), and the policy in force NOW still
// allows the direction and the declared size. A policy refusal marks the
// row failed. Success and every refusal of a grant in the lease's own
// workspace are audited in the same transaction, so a failed audit insert
// fails the claim (fail closed).
func (b *Broker) ClaimTransfer(ctx context.Context, gw GatewayIdentity, leaseID, transferID, method string) (TransferGrant, error) {
	if b.transferPolicy == nil {
		return TransferGrant{}, ErrTransferDenied
	}
	now := b.now()
	l, err := b.liveLease(ctx, gw, leaseID, now)
	if err != nil {
		return TransferGrant{}, err
	}
	binding, err := b.boundCurrent(ctx, l, now)
	if err != nil {
		return TransferGrant{}, err
	}
	// Resolved before the row lock so the resolver's own reads never run
	// under it. An error fails the claim closed.
	pol, err := b.transferPolicy(ctx, l.TenantID, l.WorkspaceUID)
	if err != nil {
		return TransferGrant{}, fmt.Errorf("broker: resolve transfer policy: %w", err)
	}
	want := methodDirection(method)
	iss, sub, _ := strings.Cut(l.PrincipalSubject, "|")
	actor := observability.ActorRef(iss, sub)

	var grant TransferGrant
	var refusal error
	err = b.db.WithTx(ctx, func(tx store.Tx) error {
		// The first statement: serialize claims per workspace. NO KEY UPDATE
		// does not block foreign-key lookups on the workspace row.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM workspaces WHERE id = $1 FOR NO KEY UPDATE`, l.WorkspaceUID); err != nil {
			return fmt.Errorf("broker: lock workspace %w", err)
		}
		var (
			ws, rowIss, rowSub, rtUID, dir, path string
			gen, declared                        int64
		)
		err := tx.QueryRow(ctx, `
			SELECT workspace_id, principal_issuer, principal_subject, runtime_generation, runtime_uid,
			       direction, path, declared_bytes
			FROM session_file_transfer
			WHERE id = $1 AND status = 'pending' AND expires_at > $2 FOR UPDATE`, transferID, now).
			Scan(&ws, &rowIss, &rowSub, &gen, &rtUID, &dir, &path, &declared)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && ws != l.WorkspaceUID) {
			// Unknown, spent, expired, or another workspace's grant: nothing
			// of this workspace to audit.
			return ErrTransferInvalid
		}
		if err != nil {
			return fmt.Errorf("broker: load transfer %w", err)
		}
		details := map[string]string{
			"phase": "claim", "direction": dir, "path": path,
			"declared": strconv.FormatInt(declared, 10), "policy": pol.Mode,
		}
		emit := func(outcome, errCode, reason string) error {
			if reason != "" {
				details["reason"] = reason
			}
			_, err := store.InsertAudit(ctx, tx, store.AuditRecord{
				Tenant: l.TenantID, ActorRef: actor, Action: "file.transfer." + dir,
				TargetKind: "workspace", TargetID: l.WorkspaceUID, RequestID: transferID,
				Outcome: outcome, ErrorCode: errCode, Details: details,
			})
			return err
		}
		if rowIss+"|"+rowSub != l.PrincipalSubject ||
			uint64(gen) != binding.RuntimeGeneration || rtUID != binding.RuntimeUID || dir != want {
			refusal = ErrTransferInvalid // the audit row commits, the grant stays pending
			return emit("denied", "INVALID_STATE", "mismatch")
		}
		var live int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM session_file_transfer
			WHERE workspace_id = $1 AND status = 'claimed' AND claimed_at > $2`,
			ws, now.Add(-store.FileTransferClaimDeadAfter)).Scan(&live); err != nil {
			return fmt.Errorf("broker: live transfers %w", err)
		}
		if live > 0 {
			refusal = ErrTransferBusy
			return emit("denied", "INVALID_STATE", "busy")
		}
		if !directionAllowed(pol.Mode, dir) || pol.MaxBytes <= 0 || (dir == "upload" && declared > pol.MaxBytes) {
			if _, err := tx.Exec(ctx,
				`UPDATE session_file_transfer SET status = 'failed', completed_at = $2 WHERE id = $1 AND status = 'pending'`,
				transferID, now); err != nil {
				return fmt.Errorf("broker: fail denied transfer %w", err)
			}
			refusal = ErrTransferDenied
			return emit("denied", "FORBIDDEN", "policy")
		}
		tag, err := tx.Exec(ctx, `
			UPDATE session_file_transfer SET status = 'claimed', claimed_at = $2
			WHERE id = $1 AND status = 'pending'`, transferID, now)
		if err != nil {
			return fmt.Errorf("broker: claim transfer %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrTransferInvalid
		}
		if err := emit("success", "", ""); err != nil {
			return err
		}
		capBytes := pol.MaxBytes
		if dir == "upload" {
			capBytes = declared // declared <= pol.MaxBytes, checked above
		}
		grant = TransferGrant{ID: transferID, WorkspaceUID: ws, Direction: dir, Path: path, MaxBytes: capBytes}
		return nil
	})
	if err != nil {
		return TransferGrant{}, err
	}
	if refusal != nil {
		return TransferGrant{}, refusal
	}
	return grant, nil
}

// CompleteTransfer closes a claimed grant with the byte count the gateway
// moved and records the outcome. The lease is loaded without the liveness
// gate: the stream was authorized at claim, so a lease revoked mid-stream may
// still close its row. The gateway identity and the lease's workspace still
// bind, so one workspace's lease can never close another's grant.
func (b *Broker) CompleteTransfer(ctx context.Context, gw GatewayIdentity, leaseID, transferID string, bytesDone int64, ok bool) error {
	l, _, _, err := b.loadLease(ctx, leaseID)
	if err != nil {
		return err
	}
	if l.GatewayID != gw.ID {
		return ErrDenied
	}
	if bytesDone < 0 {
		bytesDone = 0
	}
	now := b.now()
	status, outcome := "completed", "success"
	if !ok {
		status, outcome = "failed", "failure"
	}
	return b.db.WithTx(ctx, func(tx store.Tx) error {
		var iss, sub, dir, path, policy string
		var declared int64
		err := tx.QueryRow(ctx, `
			UPDATE session_file_transfer SET status = $2, bytes_done = $3, completed_at = $4
			WHERE id = $1 AND status = 'claimed' AND workspace_id = $5
			RETURNING principal_issuer, principal_subject, direction, path, declared_bytes, resolved_policy`,
			transferID, status, bytesDone, now, l.WorkspaceUID).
			Scan(&iss, &sub, &dir, &path, &declared, &policy)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTransferInvalid
		}
		if err != nil {
			return fmt.Errorf("broker: complete transfer %w", err)
		}
		_, err = store.InsertAudit(ctx, tx, store.AuditRecord{
			Tenant: l.TenantID, ActorRef: observability.ActorRef(iss, sub), Action: "file.transfer." + dir,
			TargetKind: "workspace", TargetID: l.WorkspaceUID, RequestID: transferID,
			Outcome: outcome,
			Details: map[string]string{
				"phase": "complete", "direction": dir, "path": path, "policy": policy,
				"declared": strconv.FormatInt(declared, 10), "bytes": strconv.FormatInt(bytesDone, 10),
			},
		})
		return err
	})
}
```

Add `transferPolicy TransferPolicyResolver` to the `Broker` struct (`tickets.go:201`, next to `inputHook`). Run the Step 2 command. Expected: PASS. If `TestClaimFileTransfer_OnePerWorkspaceConcurrent` is flaky, the workspace `FOR NO KEY UPDATE` is not the first statement. It must run before the transfer row is read. `policy` in the completion audit is the policy recorded at mint; the claim row carries the policy in force at claim.

- [ ] **Step 4: Surface the methods to the gateway**

Widening `GatewaySurface`, `httpapi.BrokerAPI` and `gateway.BrokerClient` breaks every implementer, so all of them change in this step. Production implementers: `*broker.Broker` (Step 3), `*broker.LocalGateway`, `*brokerclient.Client`. Test implementers: `recordingSurface` (`gatewayshim_internal_test.go`), `fakeBroker` (`httpapi/server_test.go`), `stubBroker` (`opclient/client_test.go`), `fakeBroker` (`gateway/helpers_test.go`), `fakeBrokerClient`, `fakeInternalBroker` and `internalAdapter` (`backend/backend_test.go`), `stubBrokerClient` (`backend/metrics_test.go`). `activityBroker` embeds `*fakeBroker` and needs nothing.

- `gatewayshim.go`: add both methods to the `GatewaySurface` interface:

```go
	ClaimTransfer(ctx context.Context, gw GatewayIdentity, leaseID, transferID, method string) (TransferGrant, error)
	CompleteTransfer(ctx context.Context, gw GatewayIdentity, leaseID, transferID string, bytesDone int64, ok bool) error
```

  then add (the file already imports `strings`):

```go
// ClaimTransfer spends a minted transfer grant for the pinned identity.
func (l *LocalGateway) ClaimTransfer(ctx context.Context, _ GatewayIdentity, leaseID, transferID, method string) (TransferGrant, error) {
	if !leaseIDOK(leaseID) {
		return TransferGrant{}, ErrLeaseInvalid
	}
	if !TransferIDOK(transferID) {
		return TransferGrant{}, ErrTransferInvalid
	}
	g, err := l.b.ClaimTransfer(ctx, l.id, leaseID, transferID, method)
	return g, classify("transfer-claim", err)
}

// CompleteTransfer closes a claimed grant.
func (l *LocalGateway) CompleteTransfer(ctx context.Context, _ GatewayIdentity, leaseID, transferID string, bytesDone int64, ok bool) error {
	if !leaseIDOK(leaseID) {
		return ErrLeaseInvalid
	}
	if !TransferIDOK(transferID) {
		return ErrTransferInvalid
	}
	return classify("transfer-complete", l.b.CompleteTransfer(ctx, l.id, leaseID, transferID, bytesDone, ok))
}

// TransferIDOK bounds transfer ids accepted from the gateway (xf_ + 26 base32).
func TransferIDOK(id string) bool {
	if len(id) != 29 || !strings.HasPrefix(id, "xf_") {
		return false
	}
	for _, c := range id[3:] {
		if !(c >= 'a' && c <= 'z') && !(c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}
```

  `classify` passes the transfer errors through (its `default` branch wraps with `%w`, so `errors.Is` still matches). `gatewayshim_internal_test.go`: `recordingSurface` gets the two methods, then the test below is added next to `TestLocalGateway_PinsIdentity` (which keeps asserting exactly 7 recorded calls, so it is not extended):

```go
func (r *recordingSurface) ClaimTransfer(_ context.Context, gw GatewayIdentity, _, _, _ string) (TransferGrant, error) {
	r.gotIDs = append(r.gotIDs, gw)
	r.calls = append(r.calls, "transfer-claim")
	return TransferGrant{}, r.err
}

func (r *recordingSurface) CompleteTransfer(_ context.Context, gw GatewayIdentity, _, _ string, _ int64, _ bool) error {
	r.gotIDs = append(r.gotIDs, gw)
	r.calls = append(r.calls, "transfer-complete")
	return r.err
}

func TestLocalGateway_TransferPinsIdentityAndBoundsIDs(t *testing.T) {
	const tid = "xf_abcdefghijklmnopqrstuvwxyz"
	rs := &recordingSurface{}
	lg, _ := NewLocalGateway(rs, pinned, "operator")
	spoof := GatewayIdentity{ID: "operator", Audience: "evil.example.test"}
	ctx := context.Background()
	_, _ = lg.ClaimTransfer(ctx, spoof, "lease-1", tid, "PUT")
	_ = lg.CompleteTransfer(ctx, spoof, "lease-1", tid, 3, true)
	if len(rs.gotIDs) != 2 || rs.gotIDs[0] != pinned || rs.gotIDs[1] != pinned {
		t.Fatalf("transfer calls presented %+v, want the pinned identity", rs.gotIDs)
	}
	for _, bad := range []string{"", "xf_bad", "xf_ABCDEFGHIJKLMNOPQRSTUVWXYZ", "xf_abcdefghijklmnopqrstuvwxy1", "../" + tid} {
		if _, err := lg.ClaimTransfer(ctx, pinned, "lease-1", bad, "GET"); !errors.Is(err, ErrTransferInvalid) {
			t.Errorf("claim %q err=%v", bad, err)
		}
		if err := lg.CompleteTransfer(ctx, pinned, "lease-1", bad, 0, true); !errors.Is(err, ErrTransferInvalid) {
			t.Errorf("complete %q err=%v", bad, err)
		}
	}
	if _, err := lg.ClaimTransfer(ctx, pinned, "a/b", tid, "GET"); !errors.Is(err, ErrLeaseInvalid) {
		t.Errorf("bad lease id err=%v", err)
	}
	if len(rs.calls) != 2 {
		t.Fatalf("invalid input reached the broker: %v", rs.calls)
	}
}
```
- `httpapi/server.go`: add the two methods to the `BrokerAPI` interface (same signatures as `GatewaySurface`). Register, next to the other lease routes:

```go
	mux.HandleFunc("POST /internal/v1/broker/leases/{id}/transfers/{tid}/claim", s.claimTransfer)
	mux.HandleFunc("POST /internal/v1/broker/leases/{id}/transfers/{tid}/complete", s.completeTransfer)
```

  Lease errors share these statuses (`ErrRevoked` 410, `ErrDenied` 403, `ErrStaleBinding` 409), so the transfer errors carry their own internal-only codes, declared next to `codeRevoked`:

```go
const (
	codeTransferInvalid = api.ErrorCode("TRANSFER_INVALID")
	codeTransferDenied  = api.ErrorCode("TRANSFER_DENIED")
	codeTransferBusy    = api.ErrorCode("TRANSFER_BUSY")
)
```

  In `brokerError`, add before the `ErrRevoked` case:

```go
	case errors.Is(err, broker.ErrTransferInvalid):
		writeErr(w, r, http.StatusGone, codeTransferInvalid, "transfer invalid, used or expired")
	case errors.Is(err, broker.ErrTransferDenied):
		writeErr(w, r, http.StatusForbidden, codeTransferDenied, "transfer denied by policy")
	case errors.Is(err, broker.ErrTransferBusy):
		writeErr(w, r, http.StatusConflict, codeTransferBusy, "a transfer is already in progress")
```

  and the handlers (same shape as `activity`):

```go
// transferID validates the {tid} path value of the transfer routes.
func transferID(w http.ResponseWriter, r *http.Request) (string, bool) {
	tid := r.PathValue("tid")
	if !broker.TransferIDOK(tid) {
		writeErr(w, r, http.StatusBadRequest, api.CodeInvalidRequest, "bad transfer id")
		return "", false
	}
	return tid, true
}

type claimTransferRequest struct {
	Method string `json:"method"`
}

// claimTransfer spends a minted transfer grant for the calling gateway's
// lease. Errors: 400 bad ids or method, 403 denied (foreign gateway or
// policy), 409 stale binding or another transfer live, 410 dead lease or
// invalid grant, 503 stale binding view.
func (s *server) claimTransfer(w http.ResponseWriter, r *http.Request) {
	if !s.requireGateway(w, r) {
		return
	}
	id, ok := leaseID(w, r)
	if !ok {
		return
	}
	tid, ok := transferID(w, r)
	if !ok {
		return
	}
	var req claimTransferRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Method != http.MethodGet && req.Method != http.MethodPut {
		writeErr(w, r, http.StatusBadRequest, api.CodeInvalidRequest, "method must be GET or PUT")
		return
	}
	grant, err := s.cfg.Broker.ClaimTransfer(r.Context(), gatewayFrom(r.Context()), id, tid, req.Method)
	if err != nil {
		brokerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, grant)
}

type completeTransferRequest struct {
	Bytes int64 `json:"bytes"`
	OK    bool  `json:"ok"`
}

// completeTransfer closes a claimed grant with the bytes moved.
func (s *server) completeTransfer(w http.ResponseWriter, r *http.Request) {
	if !s.requireGateway(w, r) {
		return
	}
	id, ok := leaseID(w, r)
	if !ok {
		return
	}
	tid, ok := transferID(w, r)
	if !ok {
		return
	}
	var req completeTransferRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Bytes < 0 {
		writeErr(w, r, http.StatusBadRequest, api.CodeInvalidRequest, "bytes must not be negative")
		return
	}
	if err := s.cfg.Broker.CompleteTransfer(r.Context(), gatewayFrom(r.Context()), id, tid, req.Bytes, req.OK); err != nil {
		brokerError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

- `brokerclient/client.go`:

```go
// ClaimTransfer spends a minted transfer grant; the gateway identity is the
// mTLS client certificate.
func (c *Client) ClaimTransfer(ctx context.Context, _ broker.GatewayIdentity, leaseID, transferID, method string) (broker.TransferGrant, error) {
	var out broker.TransferGrant
	err := c.do(ctx, "transfer-claim", http.MethodPost,
		leasesPath+url.PathEscape(leaseID)+"/transfers/"+url.PathEscape(transferID)+"/claim",
		struct {
			Method string `json:"method"`
		}{Method: method}, &out)
	if err != nil {
		return broker.TransferGrant{}, err
	}
	return out, nil
}

// CompleteTransfer closes a claimed grant with the bytes moved.
func (c *Client) CompleteTransfer(ctx context.Context, _ broker.GatewayIdentity, leaseID, transferID string, bytesDone int64, ok bool) error {
	return c.do(ctx, "transfer-complete", http.MethodPost,
		leasesPath+url.PathEscape(leaseID)+"/transfers/"+url.PathEscape(transferID)+"/complete",
		struct {
			Bytes int64 `json:"bytes"`
			OK    bool  `json:"ok"`
		}{Bytes: bytesDone, OK: ok}, nil)
}
```

  At the very top of `sentinelFor` (before the "410 means ErrRevoked" shortcut, because `TRANSFER_INVALID` also rides on 410) add:

```go
	switch code {
	case "TRANSFER_INVALID":
		return broker.ErrTransferInvalid
	case "TRANSFER_DENIED":
		return broker.ErrTransferDenied
	case "TRANSFER_BUSY":
		return broker.ErrTransferBusy
	}
```

- `gateway/proxy.go` `BrokerClient` (lines 37-48): add

```go
	// ClaimTransfer spends a minted transfer grant for the lease (method GET
	// for a download, PUT for an upload); CompleteTransfer closes it with the
	// bytes moved.
	ClaimTransfer(ctx context.Context, gw broker.GatewayIdentity, leaseID, transferID, method string) (broker.TransferGrant, error)
	CompleteTransfer(ctx context.Context, gw broker.GatewayIdentity, leaseID, transferID string, bytesDone int64, ok bool) error
```

- Test fakes. `internal/gateway/helpers_test.go`: add to `fakeBroker` the fields `transfers map[string]broker.TransferGrant` (transfer id → the grant `ClaimTransfer` returns once), `claimErr error` and `completed []transferDone`; set `transfers: map[string]broker.TransferGrant{}` in `newFakeBroker`; add:

```go
type transferDone struct {
	id    string
	bytes int64
	ok    bool
}

func (f *fakeBroker) ClaimTransfer(_ context.Context, _ broker.GatewayIdentity, leaseID, transferID, method string) (broker.TransferGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return broker.TransferGrant{}, f.claimErr
	}
	g, ok := f.transfers[transferID]
	if !ok || (method == http.MethodPut) != (g.Direction == "upload") {
		return broker.TransferGrant{}, broker.ErrTransferInvalid
	}
	delete(f.transfers, transferID) // single use
	return g, nil
}

func (f *fakeBroker) CompleteTransfer(_ context.Context, _ broker.GatewayIdentity, _, transferID string, bytesDone int64, ok bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed = append(f.completed, transferDone{transferID, bytesDone, ok})
	return nil
}

func (f *fakeBroker) completions() []transferDone {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]transferDone(nil), f.completed...)
}
```

  `internal/backend/backend_test.go`: give `fakeBrokerClient` the same two methods as inert stubs (`return broker.TransferGrant{}, broker.ErrTransferInvalid` and `return nil`), `fakeInternalBroker` the same two stubs, and `internalAdapter` forwarding stubs (`return a.fb.ClaimTransfer(ctx, gw, leaseID, transferID, method)` and the matching `CompleteTransfer`). `internal/backend/metrics_test.go` `stubBrokerClient` and `opclient/client_test.go` `stubBroker`: the two stubs returning `broker.TransferGrant{}, errors.New("stub")` / `errors.New("stub")` (use `broker.ErrTransferInvalid` and `nil` where the file does not import `errors`).

`httpapi/server_test.go`: add the fields `grant broker.TransferGrant`, `transferErr error`, `gotTransferID`, `gotMethod string`, `gotBytes int64`, `gotOK bool` to `fakeBroker` and the methods:

```go
func (f *fakeBroker) ClaimTransfer(_ context.Context, gw broker.GatewayIdentity, leaseID, transferID, method string) (broker.TransferGrant, error) {
	f.gotGW, f.gotLeaseID, f.gotTransferID, f.gotMethod = gw, leaseID, transferID, method
	return f.grant, f.transferErr
}

func (f *fakeBroker) CompleteTransfer(_ context.Context, gw broker.GatewayIdentity, leaseID, transferID string, bytesDone int64, ok bool) error {
	f.gotGW, f.gotLeaseID, f.gotTransferID, f.gotBytes, f.gotOK = gw, leaseID, transferID, bytesDone, ok
	return f.transferErr
}
```

Then add the test (the internal listener's error body is the flat `api.Error` shape `{"code":…}` that `writeErr` encodes; the public API's `{"error":{…}}` envelope does not apply on this listener):

```go
func TestTransferClaim_OK_AndErrorMapping(t *testing.T) {
	const tid = "xf_abcdefghijklmnopqrstuvwxyz"
	const base = "/internal/v1/broker/leases/lease-abc/transfers/"
	fb := &fakeBroker{grant: broker.TransferGrant{ID: tid, WorkspaceUID: "ws-1", Direction: "upload", Path: "a.txt", MaxBytes: 5}}
	e := newEnv(t, fb)
	cert := e.pki.issue(t, "gw-1", nil, false)
	resp, err := e.client(t, &cert).Post(e.srv.URL+base+tid+"/claim",
		"application/json", strings.NewReader(`{"method":"PUT"}`))
	if err != nil {
		t.Fatal(err)
	}
	var g broker.TransferGrant
	_ = json.NewDecoder(resp.Body).Decode(&g)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || g != fb.grant || fb.gotMethod != "PUT" || fb.gotTransferID != tid || fb.gotGW.ID != "gw-1" {
		t.Fatalf("claim: %d %+v method=%q", resp.StatusCode, g, fb.gotMethod)
	}
	for _, tc := range []struct {
		err  error
		want int
		code string
	}{
		{broker.ErrTransferInvalid, http.StatusGone, "TRANSFER_INVALID"},
		{broker.ErrTransferDenied, http.StatusForbidden, "TRANSFER_DENIED"},
		{broker.ErrTransferBusy, http.StatusConflict, "TRANSFER_BUSY"},
		{broker.ErrRevoked, http.StatusGone, "REVOKED"},
	} {
		e := newEnv(t, &fakeBroker{transferErr: tc.err})
		cert := e.pki.issue(t, "gw-1", nil, false)
		resp, err := e.client(t, &cert).Post(e.srv.URL+base+tid+"/claim",
			"application/json", strings.NewReader(`{"method":"GET"}`))
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != tc.want || body.Code != tc.code {
			t.Fatalf("claim(%v) = %d %q, want %d %q", tc.err, resp.StatusCode, body.Code, tc.want, tc.code)
		}
	}
	resp, err = e.client(t, &cert).Post(e.srv.URL+base+tid+"/complete",
		"application/json", strings.NewReader(`{"bytes":5,"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || fb.gotBytes != 5 || !fb.gotOK {
		t.Fatalf("complete: %d bytes=%d ok=%v", resp.StatusCode, fb.gotBytes, fb.gotOK)
	}
	for _, bad := range []struct{ path, body string }{
		{"xf_..%2Fx/claim", `{"method":"GET"}`},   // malformed transfer id
		{tid + "/claim", `{"method":"DELETE"}`},   // unknown method
		{tid + "/complete", `{"bytes":-1,"ok":true}`}, // negative byte count
	} {
		resp, err := e.client(t, &cert).Post(e.srv.URL+base+bad.path, "application/json", strings.NewReader(bad.body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s %s: %d, want 400", bad.path, bad.body, resp.StatusCode)
		}
	}
	op := e.pki.issue(t, httpapi.DefaultOperatorCN, nil, false)
	resp, err = e.client(t, &op).Post(e.srv.URL+base+tid+"/claim", "application/json", strings.NewReader(`{"method":"GET"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("operator identity on the claim route: %d, want 403", resp.StatusCode)
	}
}
```

Brokerclient tests. The fake listener's `/internal/v1/broker/leases/` handler gets two more cases before its `default:` (they run only when `f.status == 0`):

```go
		case strings.HasSuffix(r.URL.Path, "/claim"):
			var body struct {
				Method string `json:"method"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.lastBody["method"] = json.RawMessage(`"` + body.Method + `"`)
			_ = json.NewEncoder(w).Encode(broker.TransferGrant{ID: "xf_abcdefghijklmnopqrstuvwxyz", WorkspaceUID: "ws-1", Direction: "upload", Path: "a.txt", MaxBytes: 5})
		case strings.HasSuffix(r.URL.Path, "/complete"):
			var body struct {
				Bytes int64 `json:"bytes"`
				OK    bool  `json:"ok"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.lastBody["bytes"] = json.RawMessage(fmt.Sprint(body.Bytes))
			w.WriteHeader(http.StatusNoContent)
```

and, next to `TestRenewLease_Revoked410`:

```go
func TestTransfer_RoundTripAndSentinelsByCode(t *testing.T) {
	const tid = "xf_abcdefghijklmnopqrstuvwxyz"
	pki := newTestPKI(t)
	api := newFakeBrokerAPI(t, pki)
	c := clientFor(t, api, pki)
	g, err := c.ClaimTransfer(context.Background(), testGW, "lease-1", tid, "PUT")
	if err != nil || g.Direction != "upload" || g.MaxBytes != 5 || string(api.lastBody["method"]) != `"PUT"` ||
		api.lastPath != "/internal/v1/broker/leases/lease-1/transfers/"+tid+"/claim" {
		t.Fatalf("claim = %+v, %v, path %q", g, err, api.lastPath)
	}
	if err := c.CompleteTransfer(context.Background(), testGW, "lease-1", tid, 5, true); err != nil || string(api.lastBody["bytes"]) != "5" {
		t.Fatalf("complete: %v, bytes %s", err, api.lastBody["bytes"])
	}
	for _, tc := range []struct {
		status int
		code   string
		want   error
	}{
		{http.StatusGone, "TRANSFER_INVALID", broker.ErrTransferInvalid},
		{http.StatusForbidden, "TRANSFER_DENIED", broker.ErrTransferDenied},
		{http.StatusConflict, "TRANSFER_BUSY", broker.ErrTransferBusy},
		{http.StatusGone, "REVOKED", broker.ErrRevoked},
	} {
		api.status, api.code = tc.status, tc.code
		_, err := c.ClaimTransfer(context.Background(), testGW, "lease-1", tid, "PUT")
		if !errors.Is(err, tc.want) {
			t.Fatalf("%d %s: err = %v, want %v", tc.status, tc.code, err, tc.want)
		}
	}
}
```

Run `go build ./... && go vet ./internal/... && go test ./internal/broker/... ./internal/gateway/... ./internal/backend/... -count=1`. Expected: PASS (`go vet` compiles every test package, which proves no implementer was missed).

- [ ] **Step 5: Commit**

```bash
go vet ./... && go test -count=1 ./internal/store/ ./internal/broker/... ./internal/gateway/... ./internal/backend/
git add internal
git commit -m "feat(transfer): transfer rows with atomic claim, policy recheck and audited completion

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 11a: Transfer mint API, claim-time policy resolver, expiry sweep (F4, part 2b) — tier: integrate

**Files:**
- Create: `internal/api/filetransfers.go`, `internal/api/filetransfers_test.go`
- Modify: `internal/api/{auditroutes.go,errors.go,openapi.yaml,statusview.go,auditroutes_test.go,openapi_contract_test.go}`, `internal/api/workspaces_handlers_test.go:224` (extract `newEnvWithRoutes`)
- Modify: `internal/observability/metrics.go:121` (`auditEventActions`)
- Modify: `internal/backend/{wire.go,retention.go}` (mint handler, claim-time policy resolver, sweep singleton)
- Modify: `web/src/api/generated/schema.d.ts` (regenerated), `docs/security/threat-model.md` (row S48)

**Interfaces:**
- Consumes: Task 11 (`store.FileTransfer`, `store.InsertFileTransfer`, `store.FileTransferTTL`, `store.SweepFileTransfers`, `broker.TransferPolicy`, `Broker.WithTransferPolicy`); `provisioning.DirectionAllows`, `Service.ResolveSessionPolicy`, `SessionPolicy.FileTransfer|MaxFileBytes`, `TemplateInfo.FileTransferPolicy`, `TemplateCatalogEntry.FileTransferPolicy` (Task 7); `appRoutes` (Task 2); `sessionhost.Domain.Origin`; `StatusView`.
- Produces (Task 14 relies on these):
  - `POST /v1/workspaces/{workspaceId}/file-transfers` (session cookie + CSRF, session-keyed rate limit) with body `{direction: "upload"|"download", path: <name>, bytes: <int>}` returns 201 `{id, direction, path, declaredBytes, maxBytes, expiresAt, contentUrl}`. A policy refusal is 403 `FORBIDDEN` with `details.reason: "transfer_denied"`; a workspace that is not `Ready` is 409 `INVALID_STATE`; another user's workspace is 404.
  - Audit actions `file.transfer.upload` / `file.transfer.download`, `details.phase=mint`, plus `direction`, `path`, `policy` and `bytes` (declared).
  - `contentUrl` = `<session origin>/v1/transfers/<id>/content`; the id matches `^xf_[a-z2-7]{26}$`.
  - `api.ReasonTransferDenied = "transfer_denied"`; the generated web types.

- [ ] **Step 1: Mint API contract, tests first**

Apply the New API operation checklist for `routeFileTransferCreate = "POST /v1/workspaces/{id}/file-transfers"` (the Go mux uses the `{id}` wildcard like the sibling `/connections` route; the OpenAPI path key is `/v1/workspaces/{workspaceId}/file-transfers`, matching `components/parameters/WorkspaceId`).

1. `internal/api/auditroutes.go`: add to the action consts `auditActionFileTransferUpload = "file.transfer.upload"` and `auditActionFileTransferDownload = "file.transfer.download"`, to the route consts `routeFileTransferCreate = "POST /v1/workspaces/{id}/file-transfers"`, and to `auditedRoutes` `routeFileTransferCreate: {auditActionFileTransferUpload, "id"},` with the comment "the handler switches to the download action via auditSetAction once the direction decodes; a request denied earlier keeps the upload attempt". The target is the workspace id (`TargetKind` `workspace` via Task 1's `auditTargetKind`).
2. `internal/observability/metrics.go:121`: add `"file.transfer.upload": {}, "file.transfer.download": {}` to `auditEventActions`.
3. `internal/api/errors.go`: next to `ReasonUserLimitReached` add `const ReasonTransferDenied = "transfer_denied"` (the `details.reason` of a mint refused by the policy in force; `transfer_denied` is a reason on the public `FORBIDDEN` code, not a new `ErrorCode`, so `AllErrorCodes` is unchanged).
4. `internal/api/openapi.yaml`: add `transfer_denied` to the `ErrorDetails.reason` enum (`enum: [release_pending, UserLimitReached, transfer_denied]`, plus one description sentence: "`transfer_denied` on `FORBIDDEN` means the policy in force refuses the requested file-transfer direction"; if an earlier task already extended the enum, append to its current list). Without it `TestOpenAPIContract_ResponsesValidate` rejects the 403 body. Add the path item and the schemas:

```yaml
  /v1/workspaces/{workspaceId}/file-transfers:
    post:
      operationId: createFileTransfer
      summary: Mint a single-use file-transfer grant for a running workspace
      description: |
        Resolves the file-transfer policy in force now (template policy met
        against the tenant ceiling and the platform bound), checks the
        direction and, for an upload, the declared size, and records a
        single-use grant bound to the workspace's current runtime
        incarnation and to the caller. The browser then moves the bytes with
        a credentialed request to `contentUrl` on the workspace's session
        origin within 2 minutes; the gateway claims the grant (policy is
        checked again) and streams the body. Only the workspace owner may
        mint; a tenant administrator gets `404` on another user's workspace.
        The workspace must be `Ready`, otherwise `409 INVALID_STATE`. A policy
        that refuses the direction answers `403 FORBIDDEN` with
        `details.reason: transfer_denied`.
      tags: [workspaces]
      security:
        - sessionCookie: []
          csrfToken: []
      parameters:
        - $ref: "#/components/parameters/WorkspaceId"
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: "#/components/schemas/CreateFileTransferRequest"
      responses:
        "201":
          description: Grant minted.
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/FileTransfer"
        "400":
          $ref: "#/components/responses/BadRequest"
        "401":
          $ref: "#/components/responses/Unauthorized"
        "403":
          $ref: "#/components/responses/Forbidden"
        "404":
          $ref: "#/components/responses/NotFound"
        "409":
          $ref: "#/components/responses/Conflict"
        "429":
          $ref: "#/components/responses/RateLimited"
        "500":
          $ref: "#/components/responses/InternalError"
        "503":
          $ref: "#/components/responses/Unavailable"
```

Schemas (under `components/schemas`):

```yaml
    CreateFileTransferRequest:
      type: object
      additionalProperties: false
      required: [direction, path]
      properties:
        direction: {type: string, enum: [upload, download]}
        path:
          type: string
          minLength: 1
          maxLength: 255
          description: A file name inside the workspace's Transfers folder ($HOME/transfers). No directories.
        bytes: {type: integer, format: int64, minimum: 0, description: "Upload size in bytes (required for upload)."}
    FileTransfer:
      type: object
      additionalProperties: false
      required: [id, direction, path, declaredBytes, maxBytes, expiresAt, contentUrl]
      properties:
        id: {type: string, pattern: '^xf_[a-z2-7]{26}$'}
        direction: {type: string, enum: [upload, download]}
        path: {type: string}
        declaredBytes: {type: integer, format: int64}
        maxBytes: {type: integer, format: int64}
        expiresAt: {type: string, format: date-time}
        contentUrl: {type: string, format: uri, description: "Single-use URL on the workspace's session origin: PUT the bytes (upload) or GET them (download) with credentials, within 2 minutes."}
```

5. `internal/api/statusview.go`: add `RuntimeUID string` and `ObservedRuntimeGeneration int64` to `ObservedStatus`, set in `WorkspaceStatus` next to `obs.Phase` from `ws.Status.RuntimeUID` and `ws.Status.ObservedRuntimeGeneration` (the same two fields `K8sBindingSource` projects, `bindings.go:98-99`, so the mint binds to exactly what the claim compares). They feed only the mint; do not copy them into `WorkspaceView` (the contract drops fencing values) and leave `remember`/`snapshot` alone (a stale view never mints).
6. Contract and coverage tests. In `newCoverageEnv` (`auditroutes_test.go:186`), after `MountConnectionRoutes`, mount `MountFileTransferRoutes(mux, a, NewFileTransferHandler(be, tdeps, tdeps, d).WithAuditSink(sink), noop)` with `tdeps := &fakeTransferDeps{policy: provisioning.SessionPolicy{FileTransfer: "Bidirectional", MaxFileBytes: 1 << 20}, status: ObservedStatus{Fresh: true, Found: true, Phase: "Ready", RuntimeUID: "uid-cov", ObservedRuntimeGeneration: 1}}` (the type is defined below). In `TestAuditCoverage_MountedMuxEmitsEventPerRoute`, add to `routes` before the `DELETE /v1/workspaces/{}` driver:

```go
		{"POST /v1/workspaces/{}/file-transfers", auditActionFileTransferUpload, func() {
			doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces/"+wsID+"/file-transfers",
				`{"direction":"upload","path":"a.txt","bytes":1}`, nil).Body.Close()
		}},
```

   The download action is not driven there (the coverage test requires exactly the table action per driven route; the download variant is pinned by `TestMintFileTransfer_AuditsMintAndRefusal`). In `TestOpenAPIContract_ResponsesValidate` (`openapi_contract_test.go:513`) add to `calls`, next to the `createConnection` entries (or at the end of the workspace group), one success and two refusals:

```go
		{"POST", "/v1/workspaces/{workspaceId}/file-transfers", func() *http.Response {
			return doReq(t, env, sess, csrf, "POST", "/v1/workspaces/"+wsID+"/file-transfers",
				`{"direction":"upload","path":"a.txt","bytes":1}`, nil)
		}},
		{"POST", "/v1/workspaces/{workspaceId}/file-transfers", func() *http.Response {
			return doReq(t, env, sess, csrf, "POST", "/v1/workspaces/"+wsID+"/file-transfers",
				`{"direction":"upload","path":"a.txt"}`, nil) // 400: an upload needs bytes
		}},
		{"POST", "/v1/workspaces/{workspaceId}/file-transfers", func() *http.Response {
			return doReq(t, env, sess, csrf, "POST", "/v1/workspaces/"+unknownWS+"/file-transfers",
				`{"direction":"upload","path":"a.txt","bytes":1}`, nil) // 404
		}},
```

`internal/api/filetransfers_test.go`:

```go
func TestMintFileTransfer_RejectsBadNames(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../x", "a/b", "/etc/passwd", `a\b`, "a\x00b", "a\nb",
		strings.Repeat("x", 256), "\xff\xfe", "a;b", "a..b", "50%2e.txt", "100%.txt"} {
		if msg := validTransferName(name); msg == "" {
			t.Errorf("validTransferName(%q) accepted", name)
		}
	}
	for _, name := range []string{"report.pdf", "Ảnh chụp.png", ".hidden", "a b c.txt", "x-y_z.tar.gz"} {
		if msg := validTransferName(name); msg != "" {
			t.Errorf("validTransferName(%q) refused: %s", name, msg)
		}
	}
}

func TestMintFileTransfer_PolicyOwnerAndReadiness(t *testing.T) {
	env, deps, domain, _ := newFileTransferEnv(t)
	sess, csrf := login(t, env, "user-a")
	post := func(sess, csrf *http.Cookie, body string) *http.Response {
		return doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces/ws_xfer00001/file-transfers", body, nil)
	}
	deps.policy = provisioning.SessionPolicy{FileTransfer: "Upload", MaxFileBytes: 1000}

	res := post(sess, csrf, `{"direction":"upload","path":"a.txt","bytes":10}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d %+v", res.StatusCode, decodeErr(t, res))
	}
	out := decodeBody[fileTransferView](t, res)
	origin, _ := domain.Origin("ws_xfer00001")
	if !regexp.MustCompile(`^xf_[a-z2-7]{26}$`).MatchString(out.ID) || out.MaxBytes != 1000 ||
		out.ContentURL != origin+"/v1/transfers/"+out.ID+"/content" {
		t.Fatalf("view = %+v", out)
	}
	if got := deps.inserted[0]; got.RuntimeUID != "uid-7" || got.RuntimeGeneration != 3 || got.DeclaredBytes != 10 {
		t.Fatalf("row not bound to the observed incarnation: %+v", got)
	}
	cases := []struct {
		name string
		body string
		want int
	}{
		{"download under Upload policy", `{"direction":"download","path":"a.txt"}`, http.StatusForbidden},
		{"over the cap", `{"direction":"upload","path":"a.txt","bytes":1001}`, http.StatusBadRequest},
		{"upload without bytes", `{"direction":"upload","path":"a.txt"}`, http.StatusBadRequest},
		{"bad name", `{"direction":"upload","path":"../a.txt","bytes":1}`, http.StatusBadRequest},
		{"bad direction", `{"direction":"sideways","path":"a.txt"}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		if res := post(sess, csrf, c.body); res.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", c.name, res.StatusCode, c.want)
		}
	}
	if e := decodeErr(t, post(sess, csrf, `{"direction":"download","path":"a.txt"}`)); e.Details == nil || e.Details.Reason != "transfer_denied" {
		t.Errorf("denied download details = %+v, want reason transfer_denied", e.Details)
	}
	bsess, bcsrf := login(t, env, "user-b")
	if res := post(bsess, bcsrf, `{"direction":"upload","path":"a.txt","bytes":1}`); res.StatusCode != http.StatusNotFound {
		t.Fatalf("another user minting on user-a's workspace: %d, want 404", res.StatusCode)
	}
	deps.status.Phase = "Provisioning"
	if res := post(sess, csrf, `{"direction":"upload","path":"a.txt","bytes":1}`); res.StatusCode != http.StatusConflict {
		t.Fatalf("not Ready: %d", res.StatusCode)
	}
	if len(deps.inserted) != 1 {
		t.Fatalf("refused mints inserted rows: %d", len(deps.inserted))
	}
}
```

```go
func TestMintFileTransfer_AuditsMintAndRefusal(t *testing.T) {
	env, deps, _, sink := newFileTransferEnv(t)
	sess, csrf := login(t, env, "user-a")
	deps.policy = provisioning.SessionPolicy{FileTransfer: "Upload", MaxFileBytes: 1000}
	doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces/ws_xfer00001/file-transfers",
		`{"direction":"upload","path":"a.txt","bytes":10}`, nil).Body.Close()
	doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces/ws_xfer00001/file-transfers",
		`{"direction":"download","path":"a.txt"}`, nil).Body.Close() // refused: the policy is Upload
	var mint, refused *observability.AuditEvent
	for _, e := range sink.get() {
		e := e
		switch e.Action {
		case auditActionFileTransferUpload:
			mint = &e
		case auditActionFileTransferDownload:
			refused = &e
		}
	}
	if mint == nil || mint.Outcome != observability.OutcomeSuccess || mint.TargetUID != "ws_xfer00001" ||
		mint.Details["phase"] != "mint" || mint.Details["direction"] != "upload" || mint.Details["path"] != "a.txt" ||
		mint.Details["bytes"] != "10" || mint.Details["policy"] != "Upload" {
		t.Fatalf("mint event = %+v", mint)
	}
	if refused == nil || refused.Outcome != observability.OutcomeDenied || refused.ErrorCode != string(CodeForbidden) ||
		refused.TargetUID != "ws_xfer00001" || refused.Details["direction"] != "download" || refused.Details["policy"] != "Upload" {
		t.Fatalf("refusal event = %+v; a refused transfer must be audited like a completed one", refused)
	}
}
```

`decodeErr` (`errors_test.go:16`) decodes the public `{"error":{…}}` envelope and returns the inner `Error`, so `Details.Reason` is read directly. Add the env and fakes to the same test file (imports: `context`, `net/http`, `regexp`, `strings`, `sync`, `testing`, `time`, `observability`, `provisioning`, `sessionhost`, `store`):

```go
type fakeTransferDeps struct {
	mu       sync.Mutex
	policy   provisioning.SessionPolicy
	status   ObservedStatus
	inserted []store.FileTransfer
}

func (d *fakeTransferDeps) ResolveSessionPolicy(context.Context, string, provisioning.TemplateCatalogEntry) (provisioning.SessionPolicy, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.policy, nil
}

func (d *fakeTransferDeps) InsertFileTransfer(_ context.Context, t store.FileTransfer) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.inserted = append(d.inserted, t)
	return nil
}

func (d *fakeTransferDeps) WorkspaceStatus(context.Context, string, string) (ObservedStatus, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status, nil
}

func newFileTransferEnv(t *testing.T) (*testEnv, *fakeTransferDeps, sessionhost.Domain, *captureSink) {
	t.Helper()
	be := newFakeBackend()
	deps := &fakeTransferDeps{status: ObservedStatus{
		Fresh: true, Found: true, Phase: "Ready", RuntimeUID: "uid-7", ObservedRuntimeGeneration: 3,
	}}
	domain, err := sessionhost.ParseDomain("session.example.test")
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	h := NewFileTransferHandler(be, deps, deps, domain).WithAuditSink(sink)
	env := newEnvWithRoutes(t, func(mux *http.ServeMux, a *Authenticator) {
		MountFileTransferRoutes(mux, a, h, func(next http.Handler) http.Handler { return next })
	})
	seedWorkspace(be, env, "user-a", "ws_xfer00001", "w", time.Now())
	return env, deps, domain, sink
}
```

`newEnvWithRoutes` is `newWorkspaceEnv` (`workspaces_handlers_test.go:224`) with the handler construction taken out. Extract it once, and `newWorkspaceEnv` becomes a call to it:

```go
// newEnvWithRoutes builds an authenticated test server; mount adds the
// routes under test next to /auth/login and /auth/callback.
func newEnvWithRoutes(t *testing.T, mount func(mux *http.ServeMux, a *Authenticator)) *testEnv {
	t.Helper()
	iss, err := oidctest.NewIssuer()
	if err != nil {
		t.Fatalf("oidctest.NewIssuer: %v", err)
	}
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, nil))
	sessions := NewInMemorySessionStore(30 * time.Minute)
	a, err := NewAuthenticator(context.Background(), AuthConfig{
		Issuer:      iss.URL(),
		ClientID:    iss.ClientID,
		RedirectURL: "https://portal.test/auth/callback",
		LoginSealer: testLoginSealer(t),
	}, sessions, logger)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/auth/login", http.HandlerFunc(a.LoginHandler))
	mux.Handle("/auth/callback", http.HandlerFunc(a.CallbackHandler))
	mount(mux, a)
	srv := httptest.NewServer(RequestID(Audit(logger)(mux)))
	env := &testEnv{issuer: iss, auth: a, store: sessions, server: srv, logs: logBuf}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return env
}

func newWorkspaceEnv(t *testing.T, be workspaceBackend, cat TemplateCatalog, tenants TenantResolver, opts ...func(*WorkspaceHandler)) *testEnv {
	t.Helper()
	h := NewWorkspaceHandler(be, cat, tenants)
	for _, o := range opts {
		o(h)
	}
	th := NewTemplateHandler(cat, tenants)
	return newEnvWithRoutes(t, func(mux *http.ServeMux, a *Authenticator) { MountWorkspaceRoutes(mux, a, h, th) })
}
```

`MountFileTransferRoutes` takes `RouteMux`, which `*http.ServeMux` and the coverage env's `*recordingMux` both satisfy. Run `go test ./internal/api/ -run 'MintFileTransfer|AuditCoverage|OpenAPI' -count=1`. Expected: FAIL to compile (`validTransferName`, `NewFileTransferHandler` undefined).

- [ ] **Step 2: Implement `internal/api/filetransfers.go`**

```go
package api

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/sessionhost"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// FileTransferDeps is what minting needs beyond the workspace backend.
type FileTransferDeps interface {
	ResolveSessionPolicy(ctx context.Context, tenantID string, tpl provisioning.TemplateCatalogEntry) (provisioning.SessionPolicy, error)
	InsertFileTransfer(ctx context.Context, t store.FileTransfer) error
}

type createFileTransferRequest struct {
	Direction string `json:"direction"`
	Path      string `json:"path"`
	Bytes     *int64 `json:"bytes"`
}

type fileTransferView struct {
	ID            string    `json:"id"`
	Direction     string    `json:"direction"`
	Path          string    `json:"path"`
	DeclaredBytes int64     `json:"declaredBytes"`
	MaxBytes      int64     `json:"maxBytes"`
	ExpiresAt     time.Time `json:"expiresAt"`
	ContentURL    string    `json:"contentUrl"`
}

// FileTransferHandler mints single-use transfer grants (ADR 0010 T11).
type FileTransferHandler struct {
	backend workspaceBackend
	deps    FileTransferDeps
	status  StatusView
	domain  sessionhost.Domain
	audit   observability.AuditSink
	now     func() time.Time
	maxBody int64
}

func NewFileTransferHandler(be workspaceBackend, deps FileTransferDeps, status StatusView, domain sessionhost.Domain) *FileTransferHandler {
	return &FileTransferHandler{backend: be, deps: deps, status: status, domain: domain, now: time.Now, maxBody: 4 << 10}
}

func (h *FileTransferHandler) WithAuditSink(s observability.AuditSink) *FileTransferHandler {
	h.audit = s
	return h
}

// MountFileTransferRoutes mounts the mint route behind the session-keyed
// rate limit sign-out-everywhere uses (ruling 15).
func MountFileTransferRoutes(mux RouteMux, authn *Authenticator, h *FileTransferHandler, limit func(http.Handler) http.Handler) {
	mux.Handle(routeFileTransferCreate, authn.RequireAuth(limit(
		audited(h.audit, routeFileTransferCreate, authn.RequireCSRF(http.HandlerFunc(h.Create))))))
}

var transferIDEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func newTransferID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "xf_" + strings.ToLower(transferIDEncoding.EncodeToString(b[:])), nil
}

// validTransferName accepts one file name inside the flat transfer root:
// 1..255 bytes of valid UTF-8, no path separator, NUL or control
// character, no ";", "%" or "..", and not ".". The in-pod helper re-checks the same
// rule and opens the name through an os.Root (ruling 11).
func validTransferName(name string) string {
	switch {
	case name == "" || len(name) > 255:
		return "path must be 1..255 bytes"
	case !utf8.ValidString(name):
		return "path must be valid UTF-8"
	case name == "." || name == "..":
		return "path must name a file"
	case strings.ContainsAny(name, "/\\"):
		return "path must be a file name without directories"
	case strings.ContainsAny(name, ";%") || strings.Contains(name, ".."):
		// The gateway leg rebuilds /xfer/files/<name> through
		// cleanedProxyPath, which refuses ";", "..", and %-encodings of
		// banned bytes. Refuse them here so a claim is never spent on a
		// name the leg will 404.
		return "path must not contain ';', '%' or '..'"
	}
	for _, r := range name {
		if r == 0 || unicode.IsControl(r) {
			return "path must not contain control characters"
		}
	}
	return ""
}

func (h *FileTransferHandler) Create(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, r, CodeUnauthenticated, "authentication required")
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	if !workspaceIDPattern.MatchString(id) {
		writeError(w, r, CodeInvalidRequest, "bad workspace id")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
	if err != nil {
		writeError(w, r, CodeInvalidRequest, "unreadable or oversized body")
		return
	}
	var req createFileTransferRequest
	if decodeJSON(body, &req) != nil || (req.Direction != "upload" && req.Direction != "download") {
		writeError(w, r, CodeInvalidRequest, `direction must be "upload" or "download"`)
		return
	}
	if req.Direction == "download" {
		auditSetAction(ctx, auditActionFileTransferDownload)
	}
	auditSetDetail(ctx, "phase", "mint")
	auditSetDetail(ctx, "direction", req.Direction)
	if msg := validTransferName(req.Path); msg != "" {
		writeError(w, r, CodeInvalidRequest, msg)
		return
	}
	auditSetDetail(ctx, "path", req.Path)
	// Owner only — a tenant admin acts on others' workspaces but never
	// moves their files (ruling 16): the scope is always the caller.
	rec, err := h.backend.GetWorkspace(ctx, p.TenantID, p.Owner(), id)
	if errors.Is(err, provisioning.ErrWorkspaceNotFound) {
		writeError(w, r, CodeNotFound, "workspace not found")
		return
	}
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	obs, err := h.status.WorkspaceStatus(ctx, p.TenantID, id)
	if err != nil || !obs.Fresh || !obs.Found || obs.Phase != "Ready" || obs.RuntimeUID == "" {
		writeError(w, r, CodeInvalidState, "the workspace must be running to transfer files")
		return
	}
	pol, err := h.deps.ResolveSessionPolicy(ctx, p.TenantID, provisioning.TemplateCatalogEntry{
		ClipboardPolicy: rec.Template.ClipboardPolicy, FileTransferPolicy: rec.Template.FileTransferPolicy,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	auditSetDetail(ctx, "policy", string(pol.FileTransfer))
	if !provisioning.DirectionAllows(pol.FileTransfer, req.Direction) {
		e := NewError(CodeForbidden, "file "+req.Direction+" is not allowed for this workspace")
		e.Details = &ErrorDetails{Reason: ReasonTransferDenied}
		WriteError(w, r, e)
		return
	}
	var declared int64
	if req.Direction == "upload" {
		if req.Bytes == nil || *req.Bytes < 0 || *req.Bytes > pol.MaxFileBytes {
			writeError(w, r, CodeInvalidRequest, "bytes must be 0.."+strconv.FormatInt(pol.MaxFileBytes, 10)+" for an upload")
			return
		}
		declared = *req.Bytes
		auditSetDetail(ctx, "bytes", strconv.FormatInt(declared, 10))
	}
	tid, err := newTransferID()
	if err != nil {
		writeError(w, r, CodeInternal, "could not mint a transfer")
		return
	}
	origin, err := h.domain.Origin(id)
	if err != nil {
		writeError(w, r, CodeInternal, "session origin unavailable")
		return
	}
	expires := h.now().Add(store.FileTransferTTL).UTC()
	if err := h.deps.InsertFileTransfer(ctx, store.FileTransfer{
		ID: tid, WorkspaceID: id, Tenant: p.TenantID, PrincipalIssuer: p.Issuer, PrincipalSubject: p.Subject,
		RuntimeGeneration: obs.ObservedRuntimeGeneration, RuntimeUID: obs.RuntimeUID,
		Direction: req.Direction, Path: req.Path, DeclaredBytes: declared,
		ResolvedPolicy: string(pol.FileTransfer), ExpiresAt: expires,
	}); err != nil {
		writeStoreError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(fileTransferView{
		ID: tid, Direction: req.Direction, Path: req.Path, DeclaredBytes: declared, MaxBytes: pol.MaxFileBytes,
		ExpiresAt: expires, ContentURL: strings.TrimSuffix(origin, "/") + "/v1/transfers/" + tid + "/content",
	})
}
```

`sessionhost.Domain.Origin(id)` returns `"https://" + host` as a string (`sessionhost.go:165`). `respondJSON` always writes 200, which is why the 201 is encoded directly.

- [ ] **Step 3: Wiring, claim-time resolver and expiry sweep (`internal/backend`)**

- `appRoutes` (Task 2) gets the field `fileTransfers *api.FileTransferHandler`. In `newAppHandler`, where the `appRoutes{...}` literal is built, set `fileTransfers: api.NewFileTransferHandler(svc, fileTransferDeps{svc, db}, statusView, sessionDomain).WithAuditSink(appAudit)`, with the adapter (in `wire.go`)

```go
type fileTransferDeps struct {
	*provisioning.Service
	db *store.DB
}

func (d fileTransferDeps) InsertFileTransfer(ctx context.Context, t store.FileTransfer) error {
	return store.InsertFileTransfer(ctx, d.db, t)
}
```

- in `appMux`, next to `api.MountConnectionRoutes`: `if h.fileTransfers != nil { api.MountFileTransferRoutes(mux, authn, h.fileTransfers, sessionLimit) }`. `sessionLimit` is the session-keyed limiter (`RateLimitWithKey(loginLimiter, …, authn.SessionRateLimitKey())`), shared with touch, probe and sign-out-everywhere (ruling 15);
- the claim-time resolver in `newAppHandler`, right after `broker.WithInputHook(authn.InputHook())(brk)` (`wire.go:732`; `brk` is built earlier from `broker.New(db, bindings, opts...)` and has no `brk.With…` calls, so the resolver is installed here where `svc` is in scope). It reads the workspace's recorded template snapshot, the same input `startSessionPolicy` stamps on the pod, and the current tenant ceiling:

```go
	brk.WithTransferPolicy(func(ctx context.Context, tenantID, wsID string) (broker.TransferPolicy, error) {
		rec, err := svc.GetWorkspace(ctx, tenantID, "", wsID)
		if err != nil {
			return broker.TransferPolicy{}, err
		}
		sp, err := svc.ResolveSessionPolicy(ctx, tenantID, provisioning.TemplateCatalogEntry{
			ClipboardPolicy: rec.Template.ClipboardPolicy, FileTransferPolicy: rec.Template.FileTransferPolicy,
		})
		if err != nil {
			return broker.TransferPolicy{}, err
		}
		return broker.TransferPolicy{Mode: string(sp.FileTransfer), MaxBytes: sp.MaxFileBytes}, nil
	})
```

- a leader singleton in `retention.go` (the file already imports `errors`, `slog`, `time` and `store`):

```go
// fileTransferSweepLoop expires unclaimed transfer grants, fails dead
// claims and prunes finished rows every 30 s (leader only).
func fileTransferSweepLoop(db *store.DB, log *slog.Logger) func(ctx context.Context) {
	return func(ctx context.Context) {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if e, f, p, err := db.SweepFileTransfers(ctx, time.Now()); err != nil && !errors.Is(err, context.Canceled) {
					log.Error("file transfer sweep failed", "err", err)
				} else if e+f+p > 0 {
					log.Info("file transfer sweep", "expired", e, "failed", f, "pruned", p)
				}
			}
		}
	}
}
```

  registered next to `rateLimitWindowSweepLoop` (`wire.go:420`), before `b.electSingletons(...)`: `b.singletons = append(b.singletons, fileTransferSweepLoop(db, log))`.

Run `go build ./... && go test ./internal/api/ ./internal/backend/ ./internal/broker/... ./internal/observability/ -count=1`. Expected: PASS.

- [ ] **Step 4: Regenerate the web API types**

Run `cd web && npm run generate && git diff --stat src/api/generated/schema.d.ts && npm run test:unit`. Expected: the diff shows the new `/v1/workspaces/{workspaceId}/file-transfers` path, `CreateFileTransferRequest`, `FileTransfer` and the `transfer_denied` member of `ErrorDetails.reason`; the unit tests PASS. The generated file is committed here so the checked-in types match `openapi.yaml`. Task 14 only runs `npm run generate` again (a no-op diff for these entries) and owns the mock-API route and the UI.

- [ ] **Step 5: Threat model and commit**

Row S48: `| S48 | File-transfer grants | Implemented: owner-only mint (tenant admins cannot), Ready required, single-file names only (no separators, controls, . or ..), 2-min single-use rows bound to workspace, principal, generation and runtime UID, inserted in the audited request's transaction; claim re-resolves the policy and the size ceiling (tighten-after-mint is refused, the row is spent and the refusal recorded), checks the lease binding and allows one live stream per workspace under a workspace row lock; mint, claim, refusal and completion each write an audit row in the same transaction as the state change (`file.transfer.upload\|download`, with path, declared and transferred bytes and resolved policy; denials included); a revoked lease can still close its own stream's row but never another workspace's; mint rides the session rate limit |`

```bash
go vet ./... && go test -count=1 ./internal/store/ ./internal/api/ ./internal/broker/... ./internal/gateway/... ./internal/backend/ ./internal/observability/
git add internal web/src/api/generated/schema.d.ts docs/security/threat-model.md
git commit -m "feat(transfer): mint single-use transfer grants and sweep expired ones

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 12: `tcdi-xferd` in-pod transfer helper, `:8443` takeover, transfer credential (F4, part 3) — tier: design

**Files:**
- Create: `internal/xferd/xferd.go`, `internal/xferd/xferd_test.go`, `cmd/tcdi-xferd/main.go`
- Modify: `internal/runtime/linux/backend.go:694-735` (`generateCredentials`: key `xfer-token`), `internal/runtime/linux/backend_test.go`
- Modify: `internal/broker/targets.go:21-27,41-56,105-130` (`Credential.XferToken`, `Target.XferToken`), `internal/broker/credentials.go:81-90` (read `xfer-token`, required like `password`), synthesized credentials (`credentials.go:139-147`), `internal/gateway/brokerclient/client.go:160-200` (`targetJSON.XferToken`), `internal/gateway/brokerclient/client_test.go:172,226-236,393-408` (fake target + happy-path assert)
- Modify (test Secrets that must now carry the key): `internal/broker/credentials_envtest_test.go:109`, `tests/integration/backend_restart_test.go:546`, `internal/operator/workspace_controller_test.go:322`
- Modify: `build/linux-base/{Dockerfile,entrypoint.sh,kasmvnc.yaml}`, `build/kasm-adapter/{Dockerfile,entrypoint.sh,main.go}`
- Create: `.github/tests/xfer-entrypoint.test.sh`
- Modify: `tests/integration/kasm_adapter_test.go:245-262,445-452,507-518,565,776` (installed-files lists, UDP/loopback asserts, transfer contract subtest), `tests/integration/linux_runtime_test.go:151-200,436-460,645-653` (`selfSignedSecret` writes `xfer-token`, the blacklist control moves to `:8444`, shared `assertXferContract` + a `TransferHelper` subtest), `tests/integration/testdata/browser-image-baseline.json` (baseline refresh with the reason), `.github/workflows/runtime-images.yml:74-77` (path filter), `.github/workflows/ci.yml:336` (`PATHS_RE` of the adapter-contract gate)
- Modify: `docs/images.md` (runtime contract, budgets), `docs/kasm-images.md:84-95,126-138`, `docs/security/threat-model.md` (row S49)

**Interfaces:**
- Consumes: pod env `TCDI_XFER_POLICY` and `TCDI_XFER_MAX_BYTES` (rendered by the operator in Task 7).
- Produces (Task 13 relies on these):
  - In-pod HTTP surface on `:8443` (TLS, the workspace Secret's own cert):
    - `GET /xfer/files/{name}` returns 200 with the bytes and `Content-Length`;
    - `PUT /xfer/files/{name}` returns 200 `{"bytes":n}`;
    - both require header `X-Tcdi-Xfer-Token: <Secret xfer-token>`;
    - errors: 400 bad name, 401 bad token, 403 direction refused by the local policy floor, 404 no such file, 409 the name is not a regular file, 413 over `TCDI_XFER_MAX_BYTES`, 503 transfer root unusable;
    - everything else is reverse-proxied to KasmVNC on `https://127.0.0.1:8444`.
  - `const xferd.TokenHeader = "X-Tcdi-Xfer-Token"`
  - `type xferd.Config struct{ Root string; Token []byte; MaxBytes int64; Policy string; Upstream *url.URL; UpstreamServerName string; UpstreamCAPEM []byte }`, `func xferd.New(cfg xferd.Config) (http.Handler, error)`, `func xferd.ValidName(name string) bool`
  - `broker.Target.XferToken string` (wire name `xferToken`; always set: `K8sCredentialSource` refuses a Secret without it, like a missing `password`)
  - Secret key `xfer-token`: 32 random bytes, base64url, newline-terminated, created with the Secret in `generateCredentials` (never rotated; there is no backfill path because every Secret is created by this code)

- [ ] **Step 1: Helper tests first**

`internal/xferd/xferd_test.go`:

```go
package xferd_test

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/xferd"
)

const token = "tok-123"

type rig struct {
	srv      *httptest.Server
	root     string
	outside  string
	upstream *httptest.Server
	gotPath  string
	gotToken string
	gotXFF   string
}

func newRig(t *testing.T, policy string, max int64) *rig {
	t.Helper()
	r := &rig{}
	base := t.TempDir()
	r.root = filepath.Join(base, "transfers")
	r.outside = filepath.Join(base, "secret.txt")
	if err := os.WriteFile(r.outside, []byte("do not touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.upstream = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.gotPath, r.gotToken, r.gotXFF = req.URL.Path, req.Header.Get(xferd.TokenHeader), req.Header.Get("X-Forwarded-For")
		_, _ = io.WriteString(w, "desktop")
	}))
	t.Cleanup(r.upstream.Close)
	up, _ := url.Parse(r.upstream.URL)
	h, err := xferd.New(xferd.Config{
		Root: r.root, Token: []byte(token), MaxBytes: max, Policy: policy,
		Upstream: up, UpstreamServerName: "example.com",
		UpstreamCAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: r.upstream.Certificate().Raw}),
	})
	if err != nil {
		t.Fatal(err)
	}
	r.srv = httptest.NewTLSServer(h)
	t.Cleanup(r.srv.Close)
	return r
}

func (r *rig) do(t *testing.T, method, rawPath, tok, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, r.srv.URL+rawPath, strings.NewReader(body))
	if tok != "" {
		req.Header.Set(xferd.TokenHeader, tok)
	}
	req.SetBasicAuth("kasm_user", "pw")
	res, err := r.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestXferd_JailRefusesEscapes(t *testing.T) {
	r := newRig(t, "Bidirectional", 1<<20)
	if err := os.MkdirAll(r.root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(r.outside, filepath.Join(r.root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..", filepath.Join(r.root, "up")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"/xfer/files/..%2Fsecret.txt", "/xfer/files/%2E%2E", "/xfer/files/link", "/xfer/files/up%2Fsecret.txt",
		"/xfer/files/a%2Fb", "/xfer/files/%00", "/xfer/files/", "/xfer/files/a%5Cb", "/xfer/files/x/../../secret.txt",
	} {
		for _, m := range []string{http.MethodGet, http.MethodPut} {
			if res := r.do(t, m, p, token, "pwned"); res.StatusCode == http.StatusOK {
				t.Errorf("%s %s = 200, want refusal", m, p)
			}
		}
	}
	if b, _ := os.ReadFile(r.outside); string(b) != "do not touch" {
		t.Fatalf("file outside the jail changed: %q", b)
	}
	if err := os.RemoveAll(r.root); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(r.outside), r.root); err != nil {
		t.Fatal(err)
	}
	if res := r.do(t, http.MethodGet, "/xfer/files/secret.txt", token, ""); res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("transfer root replaced by a symlink: %d, want 503", res.StatusCode)
	}
}

func TestXferd_TokenPolicyAndCap(t *testing.T) {
	r := newRig(t, "Upload", 8)
	if res := r.do(t, http.MethodPut, "/xfer/files/a.txt", "", "12345"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", res.StatusCode)
	}
	if res := r.do(t, http.MethodPut, "/xfer/files/a.txt", "tok-124", "12345"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", res.StatusCode)
	}
	if res := r.do(t, http.MethodPut, "/xfer/files/a.txt", token, "12345"); res.StatusCode != http.StatusOK {
		t.Fatalf("upload: %d", res.StatusCode)
	}
	if b, _ := os.ReadFile(filepath.Join(r.root, "a.txt")); string(b) != "12345" {
		t.Fatalf("uploaded content %q", b)
	}
	if res := r.do(t, http.MethodPut, "/xfer/files/big.bin", token, "123456789"); res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("over cap: %d", res.StatusCode)
	}
	entries, _ := os.ReadDir(r.root)
	for _, e := range entries {
		if e.Name() != "a.txt" {
			t.Fatalf("leftover after refused upload: %s", e.Name())
		}
	}
	if res := r.do(t, http.MethodGet, "/xfer/files/a.txt", token, ""); res.StatusCode != http.StatusForbidden {
		t.Fatalf("download under an Upload floor: %d", res.StatusCode)
	}
}

func TestXferd_DownloadAndUnknownPolicyFailsClosed(t *testing.T) {
	r := newRig(t, "Download", 1<<20)
	_ = os.MkdirAll(r.root, 0o700)
	_ = os.WriteFile(filepath.Join(r.root, "Ảnh.png"), []byte("img"), 0o600)
	res := r.do(t, http.MethodGet, "/xfer/files/"+url.PathEscape("Ảnh.png"), token, "")
	if b, _ := io.ReadAll(res.Body); res.StatusCode != http.StatusOK || string(b) != "img" || res.ContentLength != 3 {
		t.Fatalf("download: %d %q len=%d", res.StatusCode, b, res.ContentLength)
	}
	if res := r.do(t, http.MethodGet, "/xfer/files/missing", token, ""); res.StatusCode != http.StatusNotFound {
		t.Fatalf("missing: %d", res.StatusCode)
	}
	z := newRig(t, "Sideways", 1<<20)
	if res := z.do(t, http.MethodPut, "/xfer/files/a", token, "x"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown policy: %d, want 403 (fail closed)", res.StatusCode)
	}
}

func TestXferd_ProxiesDesktopAndStripsToken(t *testing.T) {
	r := newRig(t, "Disabled", 0)
	req, _ := http.NewRequest(http.MethodGet, r.srv.URL+"/websockify?x=1", nil)
	req.SetBasicAuth("kasm_user", "pw")
	req.Header.Set(xferd.TokenHeader, token)
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	res, err := r.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if b, _ := io.ReadAll(res.Body); res.StatusCode != http.StatusOK || string(b) != "desktop" {
		t.Fatalf("proxy: %d %q", res.StatusCode, b)
	}
	if r.gotPath != "/websockify" || r.gotToken != "" || r.gotXFF != "203.0.113.9" {
		t.Fatalf("upstream saw path %q token %q xff %q; the token must never reach KasmVNC and the client chain must", r.gotPath, r.gotToken, r.gotXFF)
	}
	anonReq, _ := http.NewRequest(http.MethodGet, r.srv.URL+"/", nil)
	anon, err := r.srv.Client().Do(anonReq)
	if err != nil {
		t.Fatal(err)
	}
	anon.Body.Close()
	if anon.StatusCode != http.StatusUnauthorized || r.gotPath != "/websockify" {
		t.Fatalf("anonymous request: %d (upstream path %q); it must be answered locally", anon.StatusCode, r.gotPath)
	}
}

func TestXferd_ProxiesWebSocketUpgrade(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "no upgrade", http.StatusBadRequest)
			return
		}
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = rw.Flush()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(rw, buf); err == nil {
			_, _ = c.Write(buf)
		}
	}))
	t.Cleanup(up.Close)
	upURL, _ := url.Parse(up.URL)
	h, err := xferd.New(xferd.Config{
		Root: filepath.Join(t.TempDir(), "transfers"), Token: []byte(token), MaxBytes: 1, Policy: "Disabled",
		Upstream: upURL, UpstreamServerName: "example.com",
		UpstreamCAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: up.Certificate().Raw}),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	conn, err := tls.Dial("tcp", srv.Listener.Addr().String(), &tls.Config{RootCAs: pool, ServerName: "example.com", MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "GET /websockify HTTP/1.1\r\nHost: example.com\r\nAuthorization: Basic a2FzbV91c2VyOnB3\r\n"+
		"Connection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, nil)
	if err != nil || res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade through the helper: %v %v", res, err)
	}
	_, _ = io.WriteString(conn, "ping")
	got := make([]byte, 4)
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "ping" {
		t.Fatalf("websocket bytes did not round-trip through the helper: %q %v", got, err)
	}
}
```

Run `go test ./internal/xferd/ -count=1`. Expected: FAIL to compile (package missing).

- [ ] **Step 2: Implement `internal/xferd/xferd.go` (stdlib only)**

```go
// Package xferd is the in-pod file-transfer helper (ADR 0010 §5 (i)). It
// owns the runtime's single admitted port, serves /xfer/files/{name}
// against a jailed transfer root, and reverse-proxies everything else to
// KasmVNC on loopback.
package xferd

import (
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// TokenHeader carries the per-workspace transfer credential.
const TokenHeader = "X-Tcdi-Xfer-Token"

const filesPrefix = "/xfer/files/"

// Config is the helper's whole input; main fills it from the pod.
type Config struct {
	Root               string // $HOME/transfers
	Token              []byte // Secret xfer-token; empty refuses every transfer (fail closed)
	MaxBytes           int64  // TCDI_XFER_MAX_BYTES (local floor)
	Policy             string // TCDI_XFER_POLICY: Disabled|Upload|Download|Bidirectional
	Upstream           *url.URL
	UpstreamServerName string // a SAN of the staged cert ("localhost")
	UpstreamCAPEM      []byte // the staged tls.crt, pinned as the only root
}

type helper struct {
	cfg   Config
	proxy *httputil.ReverseProxy
}

// New returns the helper's handler.
func New(cfg Config) (http.Handler, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cfg.UpstreamCAPEM) {
		return nil, errors.New("xferd: upstream certificate is not PEM")
	}
	if cfg.Upstream == nil {
		return nil, errors.New("xferd: no upstream")
	}
	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: pool, ServerName: cfg.UpstreamServerName, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: false,
	}
	up := cfg.Upstream
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up)
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Del(TokenHeader)
			// Rewrite drops the forwarding headers. The gateway already
			// rebuilt them from its trusted chain, and KasmVNC keys its
			// brute-force blacklist on them (gateway rewriteForwarded), so
			// they pass through unchanged; only the gateway can reach :8443.
			for _, k := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "Forwarded", "X-Real-Ip"} {
				if v, ok := pr.In.Header[k]; ok {
					pr.Out.Header[k] = v
				}
			}
		},
		Transport:     tr,
		FlushInterval: -1,
	}
	return &helper{cfg: cfg, proxy: proxy}, nil
}

// ValidName is the same single-file-name rule the backend applies at mint.
func ValidName(name string) bool {
	if name == "" || len(name) > 255 || !utf8.ValidString(name) || name == "." || name == ".." ||
		strings.ContainsAny(name, "/\\;%") || strings.Contains(name, "..") {
		return false
	}
	for _, r := range name {
		if r == 0 || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (h *helper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/xfer" || strings.HasPrefix(r.URL.Path, "/xfer/") {
		h.transfer(w, r)
		return
	}
	// All upstream traffic now comes from 127.0.0.1, so KasmVNC's
	// brute-force blacklist would lock every client out after a few
	// anonymous probes. The gateway injects Basic credentials on every
	// request; answer anything without them here (plan ruling 24).
	if _, _, ok := r.BasicAuth(); !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="tcdi"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	h.proxy.ServeHTTP(w, r)
}

func (h *helper) allows(direction string) bool {
	switch h.cfg.Policy {
	case "Bidirectional":
		return true
	case "Upload":
		return direction == "upload"
	case "Download":
		return direction == "download"
	}
	return false // Disabled, unset or unknown: fail closed
}

// openRoot refuses a transfer root that is not a real directory: the home
// volume is user-writable, so a symlink planted at $HOME/transfers must
// never move the jail.
func (h *helper) openRoot() (*os.Root, error) {
	if err := os.Mkdir(h.cfg.Root, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	fi, err := os.Lstat(h.cfg.Root)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 {
		return nil, errors.New("xferd: transfer root is not a directory")
	}
	return os.OpenRoot(h.cfg.Root)
}

func (h *helper) transfer(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	got := []byte(r.Header.Get(TokenHeader))
	if len(h.cfg.Token) == 0 || subtle.ConstantTimeCompare(got, h.cfg.Token) != 1 {
		http.Error(w, "transfer credential required", http.StatusUnauthorized)
		return
	}
	raw := r.URL.EscapedPath()
	if !strings.HasPrefix(raw, filesPrefix) {
		http.NotFound(w, r)
		return
	}
	name, err := url.PathUnescape(strings.TrimPrefix(raw, filesPrefix))
	if err != nil || !ValidName(name) {
		http.Error(w, "bad file name", http.StatusBadRequest)
		return
	}
	var direction string
	switch r.Method {
	case http.MethodGet:
		direction = "download"
	case http.MethodPut:
		direction = "upload"
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.allows(direction) || h.cfg.MaxBytes <= 0 {
		http.Error(w, "file "+direction+" is not allowed", http.StatusForbidden)
		return
	}
	root, err := h.openRoot()
	if err != nil {
		http.Error(w, "transfer folder unavailable", http.StatusServiceUnavailable)
		return
	}
	defer root.Close()
	if direction == "download" {
		h.download(w, root, name)
		return
	}
	h.upload(w, r, root, name)
}

func (h *helper) download(w http.ResponseWriter, root *os.Root, name string) {
	fi, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		http.Error(w, "no such file", http.StatusNotFound)
		return
	}
	if err != nil || !fi.Mode().IsRegular() {
		http.Error(w, "not a regular file", http.StatusConflict)
		return
	}
	if fi.Size() > h.cfg.MaxBytes {
		http.Error(w, "file is larger than the transfer limit", http.StatusRequestEntityTooLarge)
		return
	}
	f, err := root.Open(name)
	if err != nil {
		http.Error(w, "cannot open file", http.StatusConflict)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, io.LimitReader(f, fi.Size()))
}

func (h *helper) upload(w http.ResponseWriter, r *http.Request, root *os.Root, name string) {
	if r.ContentLength > h.cfg.MaxBytes {
		http.Error(w, "file is larger than the transfer limit", http.StatusRequestEntityTooLarge)
		return
	}
	if fi, err := root.Lstat(name); err == nil && !fi.Mode().IsRegular() {
		http.Error(w, "the name is taken by a folder or link", http.StatusConflict)
		return
	}
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmp := ".tcdi-xfer-" + hex.EncodeToString(rnd[:]) + ".part"
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		http.Error(w, "cannot write file", http.StatusServiceUnavailable)
		return
	}
	n, copyErr := io.Copy(f, io.LimitReader(r.Body, h.cfg.MaxBytes+1))
	closeErr := f.Close()
	if n > h.cfg.MaxBytes {
		_ = root.Remove(tmp)
		http.Error(w, "file is larger than the transfer limit", http.StatusRequestEntityTooLarge)
		return
	}
	if copyErr != nil || closeErr != nil {
		_ = root.Remove(tmp)
		http.Error(w, "upload interrupted", http.StatusBadRequest)
		return
	}
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		http.Error(w, "cannot place file", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int64{"bytes": n})
}
```

`os.Root.Lstat`, `Open`, `OpenFile`, `Remove` and `Rename` are Go 1.25+ API, and the module is on Go 1.26. `Root` refuses `..` and any symlink that resolves outside the root, so the lexical check in `ValidName` is the first fence and `Root` is the second.

Run `go test ./internal/xferd/ -count=1 -race`. Expected: PASS.

- [ ] **Step 3: `cmd/tcdi-xferd/main.go`**

```go
// Command tcdi-xferd is the in-pod file-transfer helper (see internal/xferd).
// It reads everything from the pod: the transfer credential from the
// mounted Secret, the TLS pair the entrypoint staged on /run/tcdi, and the
// policy floor from the operator-rendered env.
package main

import (
	"crypto/tls"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/xferd"
)

func main() {
	secretDir := envOr("TCDI_SECRET_DIR", "/run/secrets/tcdi")
	rt := envOr("TCDI_RUNTIME_DIR", "/run/tcdi")
	home := envOr("HOME", "/home/workspace")
	token, err := os.ReadFile(filepath.Join(secretDir, "xfer-token"))
	if err != nil {
		// Every workspace Secret is created with the key; a missing one is a
		// broken pod, not a mode. Exit so the supervisor restarts the pod.
		log.Fatalf("tcdi-xferd: transfer credential: %v", err)
	}
	tok := strings.TrimRight(string(token), "\r\n")
	if tok == "" {
		log.Fatal("tcdi-xferd: transfer credential is empty")
	}
	max, err := strconv.ParseInt(os.Getenv("TCDI_XFER_MAX_BYTES"), 10, 64)
	if err != nil || max < 0 {
		max = 0
	}
	certPEM, err := os.ReadFile(filepath.Join(rt, "tls.crt"))
	if err != nil {
		log.Fatalf("tcdi-xferd: staged certificate: %v", err)
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(rt, "tls.crt"), filepath.Join(rt, "tls.key"))
	if err != nil {
		log.Fatalf("tcdi-xferd: staged key pair: %v", err)
	}
	up, _ := url.Parse("https://127.0.0.1:" + envOr("TCDI_XFER_UPSTREAM_PORT", "8444"))
	h, err := xferd.New(xferd.Config{
		Root: filepath.Join(home, "transfers"), Token: []byte(tok),
		MaxBytes: max, Policy: os.Getenv("TCDI_XFER_POLICY"),
		Upstream: up, UpstreamServerName: "localhost", UpstreamCAPEM: certPEM,
	})
	if err != nil {
		log.Fatalf("tcdi-xferd: %v", err)
	}
	srv := &http.Server{
		Addr:              ":8443",
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12},
	}
	log.Printf("tcdi-xferd: serving :8443 (policy %q, cap %d bytes)", os.Getenv("TCDI_XFER_POLICY"), max)
	log.Fatal(srv.ListenAndServeTLS("", ""))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
```

The cert's SANs include `localhost` (`generateCredentials`), so `UpstreamServerName: "localhost"` verifies against the pinned leaf. The helper never logs the token or a file name.

Run `CGO_ENABLED=0 go build -o /dev/null ./cmd/tcdi-xferd && go vet ./cmd/tcdi-xferd ./internal/xferd`. Expected: exit 0.

- [ ] **Step 4: Transfer credential in the Secret and the target**

In `generateCredentials` (`backend.go:694`), add a third random draw next to the password and put the key in the returned map. The Secret is created with it, so `ensureSecret` is unchanged:

```go
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		return nil, err
	}
	// …
		"xfer-token": []byte(base64.RawURLEncoding.EncodeToString(tok) + "\n"),
```

Test in `backend_test.go` (`strings` is already imported there; check with `rg -n '"strings"' internal/runtime/linux/backend_test.go`, add it if absent):

```go
func TestEnsureSecret_CarriesXferTokenAndNeverRotatesIt(t *testing.T) {
	ws := testWorkspace()
	c := fake.NewClientBuilder().WithScheme(backendScheme(t)).WithObjects(ws).Build()
	b := New(c, defaultPlacementOptions())
	sec, err := b.ensureSecret(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	tok := strings.TrimSpace(string(sec.Data["xfer-token"]))
	if len(tok) != 43 || tok == strings.TrimSpace(string(sec.Data["password"])) {
		t.Fatalf("xfer-token = %q; want 32 random bytes, base64url, distinct from the password", tok)
	}
	again, err := b.ensureSecret(context.Background(), ws)
	if err != nil || string(again.Data["xfer-token"]) != string(sec.Data["xfer-token"]) {
		t.Fatalf("xfer-token rotated on the second reconcile (%v)", err)
	}
}
```

Broker: `Credential.XferToken string` (doc: "per-workspace transfer credential the gateway injects as X-Tcdi-Xfer-Token"); in `K8sCredentialSource.RuntimeCredential` set `XferToken: strings.TrimSpace(string(sec.Data["xfer-token"]))` and add `cred.XferToken == ""` to the completeness check (`credentials.go:86`), so a Secret without the key fails like one without a password; the synthesized source derives one with a second label (replace the body of `synthesizedCredentials.RuntimeCredential` with a local `derive := func(label string) string { sum := hmac.New(sha256.New, s.key[:]); sum.Write([]byte(label)); sum.Write([]byte(workspaceUID)); return base64.RawURLEncoding.EncodeToString(sum.Sum(nil)) }` and return `Username: "kasm_user", Password: derive("tcdi-runtime-credential\x00"), XferToken: derive("tcdi-xfer-token\x00"), TLSCA: s.caPEM`; the password label is unchanged); `Target.XferToken string \`json:"xferToken"\`` set in `ResolveTarget`; `targetJSON.XferToken string \`json:"xferToken"\`` copied in `target()`. The broker HTTP server marshals `broker.Target` directly, so the wire name follows the tag.

Test Secrets that must now carry the key (otherwise the completeness check fails them): `internal/broker/credentials_envtest_test.go:109` adds `"xfer-token": []byte("xt-test\n")`; `tests/integration/backend_restart_test.go:546` adds `"xfer-token": "xt-test"`; `internal/operator/workspace_controller_test.go:322` adds `"xfer-token"` to its key list. In `brokerclient/client_test.go`, set `XferToken: "xt-test"` in the fake's `broker.Target` (`:172`), add `"xferToken": f.target.XferToken` to the `/target` document (`:226-236`), and in `TestResolveTarget_HappyPath` assert `tgt.XferToken == "xt-test"`. Add a case to the credentials envtest (`credentials_envtest_test.go`) that deletes the key from the Secret and expects `RuntimeCredential` to fail with `incomplete`. Never log the token: `rg -n 'Password' internal/gateway/*.go internal/broker/*.go | rg -i 'log|slog'` must show no new hits for `XferToken`.

Run `go test ./internal/runtime/linux/ ./internal/broker/... ./internal/gateway/... ./internal/operator/... -count=1`. Expected: PASS (the envtest packages need `KUBEBUILDER_ASSETS`, as in the repo's `make test`).

- [ ] **Step 5: Images and entrypoints**

`build/linux-base/kasmvnc.yaml` `network:` block (lines 17-20): `interface: 127.0.0.1` and `websocket_port: 8444`. Update the comment above it to say KasmVNC listens on loopback only and `tcdi-xferd` owns `:8443`. Fix the two header comments that still say KasmVNC serves `:8443` (`build/linux-base/entrypoint.sh:14`, `build/kasm-adapter/entrypoint.sh:25`) and the `Contract` block of `build/linux-base/Dockerfile` (the `:8443` bullet now reads "served by tcdi-xferd, which proxies the desktop to KasmVNC on 127.0.0.1:8444").

`build/linux-base/Dockerfile`: put the build image arg before the first `FROM` (the existing `FROM debian:bookworm-slim@...` stays the last stage, so it stays the default target and `build/browser`/`build/linux-desktop` still `FROM` the finished image) and add a builder stage:

```dockerfile
ARG BUILD_IMAGE=docker.io/library/golang:1.26@sha256:d7722066f0b60ceccb6c0643cbed1f5f9e15506ac237146c95333504d7805d89
FROM ${BUILD_IMAGE} AS xferd
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/tcdi-xferd/ ./cmd/tcdi-xferd/
COPY internal/xferd/ ./internal/xferd/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/tcdi-xferd ./cmd/tcdi-xferd
```

then, in the runtime stage next to the other `COPY` lines (before `COPY build/linux-base/kasmvnc.yaml`): `COPY --from=xferd /out/tcdi-xferd /usr/local/bin/tcdi-xferd`. The digest is the one `build/kasm-adapter/Dockerfile` already pins; keep the two in step. The helper imports only the standard library, so the stage needs no `go mod download`. The build context stays the repo root (`.dockerignore` does not exclude `cmd/` or `internal/`).

`build/linux-base/entrypoint.sh`: after the `kasmvncserver` launch and before `shutdown()`:

```bash
# tcdi-xferd owns :8443 (ADR 0010 §5 (i)): it serves /xfer/ against
# $HOME/transfers and reverse-proxies everything else to KasmVNC, which
# listens on 127.0.0.1:8444 only. The pod's published contract (one HTTPS
# port, the Secret's own cert) is unchanged.
XFERD_BIN="${TCDI_XFERD_BIN:-/usr/local/bin/tcdi-xferd}"
[ -x "$XFERD_BIN" ] || fail "transfer helper missing: $XFERD_BIN"
"$XFERD_BIN" >> "$HOME/.vnc/xferd.log" 2>&1 &
XFERD_PID=$!
```

Change `shutdown()` to `kill "$XFERD_PID" 2>/dev/null || true` before `kasmvncserver -kill`, and the supervisor loop to `while pgrep -x Xvnc >/dev/null 2>&1 && kill -0 "$XFERD_PID" 2>/dev/null; do sleep 2; done`, so the pod restarts when either process dies. The healthcheck keeps probing `https://127.0.0.1:8443/` and so checks both hops.

`build/kasm-adapter/entrypoint.sh`: in the rendered `kasmvnc.yaml`, use `interface: 127.0.0.1` and `websocket_port: 8444`; on the `kasmvncserver` line, use `-websocketPort 8444 -interface 127.0.0.1`. Add the same launch block with `XFERD_BIN="${TCDI_XFERD_BIN:-/opt/tcdi/tcdi-xferd}"` (the adapter entrypoint hard-codes `/opt/tcdi`, as it does for `xstartup.sh`). Make the same `shutdown` and supervisor change. Applied to the origin/main copies of both entrypoints, these edits are `shellcheck -x -S warning` clean and `.github/tests/xfer-entrypoint.test.sh` below passes. Update the UDP note: Xvnc's UDP transport now binds `:8444/udp`.

`build/kasm-adapter/Dockerfile` builder: add `COPY cmd/tcdi-xferd/ ./cmd/tcdi-xferd/` and `COPY internal/xferd/ ./internal/xferd/`, then `RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -o tcdi-xferd ./cmd/tcdi-xferd`. In the final stage, add `COPY --from=builder /workspace/tcdi-xferd /tcdi-xferd`.

`build/kasm-adapter/main.go`: replace the final `fmt.Printf` (it prints `len(scripts)`; the binary is not in that slice) with the block below, and update the file's header comment to say it also installs `tcdi-xferd`. After the script loop,

```go
	src := os.Getenv("TCDI_XFERD_SRC")
	if src == "" {
		src = "/tcdi-xferd"
	}
	bin, err := os.ReadFile(src)
	if err != nil {
		fatal("read %s: %v", src, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tcdi-xferd"), bin, 0o755); err != nil {
		fatal("write tcdi-xferd: %v", err)
	}
```


```go
	fmt.Printf("install-adapter: wrote %d adapter files to %s\n", len(scripts)+1, dir)
```

`tests/integration/kasm_adapter_test.go`: the UDP awk filter's port becomes `20FC` (8444) and its message `Xvnc :8444` (`:513-514`; also the `:508` comment). The `get_users` probe at `:518` and the authenticated probe at `:565` keep `:8443`, because they now cross the helper. `:776` greps `websocket_port: 8444`. Add `"tcdi-xferd"` to both installed-file lists (`kasmInit` at `:253-254` and the `InitInstallsAdapterScripts` subtest at `:446-447`). The helper is not subPath-mounted, so `kasmAdapterFiles`/`kasmMountArgs` stay unchanged. Add a subtest that runs the shared contract on the real kasmweb image (the CI loop at `ci.yml:370-382` runs `TestKasmAdapterChromium` once per `build/kasm-catalog.txt` entry, so this is the "adapter contract on every catalog entry" gate):

```go
	t.Run("TransferHelper", func(t *testing.T) {
		secret, password := selfSignedSecret(t)
		c := runKasmContainer(t, runID, "xfer", kasmImage, adapterVol(), secret, "", layout, xferEnv...)
		waitHealthyTimeout(t, c, kasmRunTimeout)
		assertXferContract(t, c, secret)
		assertNoSecret(t, c, password)
	})
```

`tests/integration/linux_runtime_test.go` (same package, `integration` tag):

1. `selfSignedSecret` (`:151-202`) writes the credential next to `username`, and the permission loop includes it:

```go
	writeFile(t, filepath.Join(dir, "xfer-token"), []byte("xt-"+password+"\n"))
	// ...
	for _, f := range []string{"tls.crt", "tls.key", "password", "username", "xfer-token"} {
```

2. `ReadinessProbeIsNotAnAuthFailure` (`:645-653`): anonymous requests to `:8443` are now answered by the helper (ruling 24) and never reach KasmVNC, so the "Control" loop must target the loopback upstream, otherwise it proves nothing and fails. Change that one command to `https://127.0.0.1:8444/` and add, before it, an assertion that eight anonymous `:8443` requests leave no `blacklisted` marker in `cat /home/workspace/.vnc/*.log`.

3. Add `xferEnv` and `assertXferContract` above `TestLinuxRuntimeReadinessAndHome`, and a subtest after `BaseImage`:

```go
// xferEnv is the pod env the operator renders from the resolved transfer
// policy (TCDI_XFER_POLICY / TCDI_XFER_MAX_BYTES); the image tests set it by
// hand. 1 MiB keeps the over-cap probe cheap.
var xferEnv = []string{"-e", "TCDI_XFER_POLICY=Bidirectional", "-e", "TCDI_XFER_MAX_BYTES=1048576"}

// assertXferContract drives the in-pod transfer helper through the published
// :8443 port (ADR 0010 §5 (i)): credential required, upload/download round
// trip on the home volume, jail refusals, hard cap with no leftovers, and
// KasmVNC reachable only on loopback :8444. secretDir is the host directory
// mounted at /run/secrets/tcdi.
func assertXferContract(t *testing.T, c, secretDir string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(secretDir, "xfer-token"))
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(raw))
	const base = "https://127.0.0.1:8443/xfer/files/"
	hdr := `-H "X-Tcdi-Xfer-Token: $(cat /run/secrets/tcdi/xfer-token)"`
	code := func(args string) string {
		return strings.TrimSpace(mustExec(t, c, "curl -sk --path-as-is -o /dev/null -w '%{http_code}' "+args+" || true"))
	}
	if got := code("-X PUT --data-binary hello " + base + "a.txt"); got != "401" {
		t.Fatalf("upload without the credential: %s, want 401", got)
	}
	if got := code(`-X PUT --data-binary hello -H "X-Tcdi-Xfer-Token: wrong" ` + base + "a.txt"); got != "401" {
		t.Fatalf("upload with a wrong credential: %s, want 401", got)
	}
	if out := strings.TrimSpace(mustExec(t, c, "curl -sk -X PUT --data-binary hello "+hdr+" "+base+"a.txt")); out != `{"bytes":5}` {
		t.Fatalf("upload = %q, want {\"bytes\":5}", out)
	}
	if out := mustExec(t, c, "curl -sk "+hdr+" "+base+"a.txt"); out != "hello" {
		t.Fatalf("download = %q, want hello", out)
	}
	mustExec(t, c, "grep -qx hello /home/workspace/transfers/a.txt")
	for _, name := range []string{"..%2Fsecret", "%2E%2E", "a%2Fb"} {
		if got := code("-X PUT --data-binary x " + hdr + " " + base + name); got != "400" {
			t.Fatalf("jail escape %q: %s, want 400", name, got)
		}
	}
	if got := code(hdr + " " + base + "missing.bin"); got != "404" {
		t.Fatalf("missing file: %s, want 404", got)
	}
	if got := strings.TrimSpace(mustExec(t, c, "head -c 1048577 /dev/zero | curl -sk -o /dev/null -w '%{http_code}' -X PUT --data-binary @- "+hdr+" "+base+"big.bin || true")); got != "413" {
		t.Fatalf("over-cap upload: %s, want 413", got)
	}
	if out := strings.TrimSpace(mustExec(t, c, "ls -A /home/workspace/transfers")); out != "a.txt" {
		t.Fatalf("transfer folder after refusals = %q, want only a.txt (no temp or escaped files)", out)
	}
	// KasmVNC is loopback-only on :8444 (20FC); nothing but the helper
	// listens beyond loopback on 8443 (20FB).
	if out := strings.TrimSpace(mustExec(t, c, `awk 'NR>1 && $4=="0A" {split($2,a,":"); if (a[2]=="20FC" && a[1]!="0100007F") print $2}' /proc/net/tcp`)); out != "" {
		t.Fatalf("KasmVNC listens beyond loopback: %s", out)
	}
	if out := strings.TrimSpace(mustExec(t, c, `awk 'NR>1 && $4=="0A" && $2=="0100007F:20FC" {print "up"}' /proc/net/tcp`)); out != "up" {
		t.Fatalf("KasmVNC is not listening on 127.0.0.1:8444")
	}
	if strings.Contains(dockerCombined("logs", c), token) {
		t.Fatalf("transfer credential leaked into the logs of %s", c)
	}
}
```

```go
	t.Run("TransferHelper", func(t *testing.T) {
		secret, password := selfSignedSecret(t)
		c := runContainer(t, runID, "xfer", baseImage, secret, "", xferEnv...)
		waitHealthy(t, c)
		assertXferContract(t, c, secret)
		assertNoSecret(t, c, password)
	})
```

Run (needs Docker): `docker build -f build/linux-base/Dockerfile -t tcdi/linux-base:it . && TCDI_IT_BUILD=1 go test -tags=integration ./tests/integration -run 'TestLinuxRuntimeReadinessAndHome/(BaseImage|TransferHelper|ReadinessProbeIsNotAnAuthFailure)' -count=1 -v`. Expected: PASS. Reviewed against a real build of the linux-base image from these exact edits: healthy, desktop through the helper 200, anonymous 401, `/xfer/` round trip, jail and cap refusals, KasmVNC on `127.0.0.1:8444` only, and the control blacklist trips on `:8444` but not on `:8443`. The kasm leg runs per catalog entry with `TCDI_IT_KASM_IMAGE=<ref> TCDI_IT_BUILD=1 go test -tags=integration ./tests/integration -run TestKasmAdapterChromium -count=1 -v`.

Image budget (spec section 7 "image baseline sizes with reasons recorded"): the helper adds one static binary to `linux-base`, which `build/browser` and `build/linux-desktop` inherit. Measured: 6.95 MB layer with `-ldflags="-s -w"` (about 12 MiB on the containerd `.Size` metric the test uses), inside the browser baseline's 2 % headroom (37.8 MB of 1,890,040,137). Build the browser image and run `TestLinuxRuntimeBrowserBaseline -v`; if it passes, leave the JSON alone. If it fails, set `sizeBytes` in `tests/integration/testdata/browser-image-baseline.json` to the measured value and append the reason to its `_comment` ("+ tcdi-xferd static helper in linux-base (F4); no other package or software change"), and record the same sentence and the measured delta in the `Budgets` part of `docs/images.md`.

CI wiring, so the new sources actually trigger the gates: in `.github/workflows/runtime-images.yml` add `- cmd/tcdi-xferd/**` and `- internal/xferd/**` under `on.push.paths` (after `- build/linux-base/**`, line 76); in `.github/workflows/ci.yml:336` add `cmd/tcdi-xferd/|internal/xferd/|` after `build/kasm-adapter/|` in `PATHS_RE`. Run `bash .github/tests/workflow-expressions.test.sh` afterwards. Expected: exit 0.

- [ ] **Step 6: Entrypoint contract test**

`.github/tests/xfer-entrypoint.test.sh` (`chmod +x`):

```bash
#!/usr/bin/env bash
# xfer-entrypoint.test.sh — contract coverage for the in-pod transfer
# helper wiring (ADR 0010 §5 (i)): both runtime entrypoints start
# tcdi-xferd, supervise it next to Xvnc and stop it on shutdown; KasmVNC
# listens on 127.0.0.1:8444 only; the images ship the helper.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BASE_EP="$ROOT/build/linux-base/entrypoint.sh"
ADAPTER_EP="$ROOT/build/kasm-adapter/entrypoint.sh"
SYS_YAML="$ROOT/build/linux-base/kasmvnc.yaml"
fails=0
bad() { echo "FAIL: $1"; fails=1; }
ok() { echo "ok: $1"; }

for f in "$BASE_EP" "$ADAPTER_EP"; do
  bash -n "$f" || bad "bash -n $f"
done
command -v shellcheck >/dev/null 2>&1 || bad "shellcheck not installed"
shellcheck -x -S warning "$BASE_EP" "$ADAPTER_EP" || bad "shellcheck"
ok "lint"

for f in "$BASE_EP" "$ADAPTER_EP"; do
  grep -q 'XFERD_PID=\$!' "$f" || bad "$f does not start tcdi-xferd in the background"
  grep -q 'kill -0 "\$XFERD_PID"' "$f" || bad "$f does not supervise tcdi-xferd"
  grep -q 'kill "\$XFERD_PID"' "$f" || bad "$f does not stop tcdi-xferd on shutdown"
done
ok "both entrypoints start, supervise and stop the helper"

grep -Eq '^\s*interface: 127\.0\.0\.1' "$SYS_YAML" || bad "linux-base kasmvnc.yaml not loopback-bound"
grep -Eq '^\s*websocket_port: 8444' "$SYS_YAML" || bad "linux-base kasmvnc.yaml not on 8444"
grep -Eq '^\s*interface: 127\.0\.0\.1' "$ADAPTER_EP" || bad "adapter yaml not loopback-bound"
grep -Eq '^\s*websocket_port: 8444' "$ADAPTER_EP" || bad "adapter yaml not on 8444"
grep -q -- '-websocketPort 8444' "$ADAPTER_EP" || bad "adapter kasmvncserver flag not 8444"
grep -q -- '-interface 127.0.0.1' "$ADAPTER_EP" || bad "adapter kasmvncserver still binds beyond loopback"
if grep -Eq 'websocket_port: 8443|-websocketPort 8443|interface: 0\.0\.0\.0|-interface 0\.0\.0\.0' "$SYS_YAML" "$ADAPTER_EP"; then
  bad "a KasmVNC listener is still on :8443 or 0.0.0.0"
fi
ok "KasmVNC loopback-only on 8444"

grep -q 'COPY --from=xferd /out/tcdi-xferd /usr/local/bin/tcdi-xferd' "$ROOT/build/linux-base/Dockerfile" \
  || bad "linux-base image does not ship tcdi-xferd"
grep -q 'go build -trimpath -o tcdi-xferd ./cmd/tcdi-xferd' "$ROOT/build/kasm-adapter/Dockerfile" \
  || bad "kasm-adapter image does not build tcdi-xferd"
grep -q '"tcdi-xferd"' "$ROOT/build/kasm-adapter/main.go" || bad "install-adapter does not install tcdi-xferd"
ok "images ship the helper"

if grep -En 'xfer-token' "$BASE_EP" "$ADAPTER_EP" | grep -Ev '^\S+:\s*#'; then
  bad "an entrypoint reads the transfer credential (only the helper may)"
fi
ok "credential stays inside the helper"

exit "$fails"
```

Run `bash .github/tests/xfer-entrypoint.test.sh && bash .github/tests/clipboard-policy.test.sh`. Expected: both exit 0. The clipboard test's byte-identical-function check still holds, because only the launch and port lines changed.

- [ ] **Step 7: Docs, threat model, commit**

- `docs/images.md` runtime contract (`rg -n '8443' docs/images.md` for the lines): the `:8443` endpoint is now served by `tcdi-xferd`, which proxies the desktop to KasmVNC on `127.0.0.1:8444`. Document `/xfer/files/{name}` (GET/PUT), the `X-Tcdi-Xfer-Token` credential from the Secret file `xfer-token`, env `TCDI_XFER_POLICY`/`TCDI_XFER_MAX_BYTES`, and the transfer folder `$HOME/transfers`. A custom image built `FROM` linux-base inherits all of it, and one that replaces the entrypoint must start `/usr/local/bin/tcdi-xferd`. Also add the budget sentence from Step 5 (helper size, baseline untouched or re-issued with the reason).
- `docs/kasm-images.md:84-95,126-138`: `-websocketPort 8444 -interface 127.0.0.1`, the helper on `:8443`, and the listening sockets `:8443/tcp` (helper), `127.0.0.1:8444/tcp` and `:8444/udp` (Xvnc).
- Row S49: `| S49 | In-pod transfer helper | Implemented: tcdi-xferd owns :8443 with the workspace's own cert and proxies the desktop to loopback-only KasmVNC (:8444, pinned cert); /xfer/ needs the per-workspace Secret credential (constant-time compare, never forwarded upstream or logged); single-file names jailed by os.Root under a non-symlink $HOME/transfers; temp-file-then-rename; hard cap at TCDI_XFER_MAX_BYTES; unknown policy fails closed; anonymous desktop requests answered locally so KasmVNC's loopback blacklist cannot be tripped |`

```bash
go vet ./... && go vet -tags=integration ./tests/integration && go test -count=1 -race ./internal/xferd/ ./internal/runtime/linux/ ./internal/broker/... ./internal/gateway/... ./internal/operator/...
CGO_ENABLED=0 go build -o /dev/null ./cmd/tcdi-xferd
for t in .github/tests/*.test.sh; do bash "$t" >/dev/null || echo "FAIL $t"; done
make docker-build-linux-base IMAGE_PREFIX=tcdi/ IMAGE_TAG=it > /tmp/xferd-build.log 2>&1; echo "rc=$?"; tail -n 5 /tmp/xferd-build.log
git add internal/xferd cmd/tcdi-xferd internal/runtime/linux internal/broker internal/gateway internal/operator build .github tests/integration docs
git commit -m "feat(runtime): tcdi-xferd owns :8443, serves jailed transfers, proxies the desktop to loopback KasmVNC

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

Expected: no `FAIL` lines and `rc=0`. If Docker is unavailable on the runner, skip the image build here; the CI image jobs build it.

---
### Task 13: Gateway transfer leg `/v1/transfers/{id}/content` (F4, part 4) — tier: integrate

**Files:**
- Create: `internal/gateway/transfer.go`, `internal/gateway/transfer_test.go`
- Modify: `internal/gateway/proxy.go:367-434` (`ServeHTTP`: a `transfer` route)
- Modify: `docs/security/threat-model.md` (row S50), `deploy/helm/tinycdi/README.md` (body-size note for ingress controllers that cap request bodies)

**Interfaces:**
- Consumes: `BrokerClient.ClaimTransfer`/`CompleteTransfer`, `broker.TransferGrant`, `broker.ErrTransfer*` (Task 11); the scriptable gateway `fakeBroker` (`transfers`, `claimErr`, `completions()`, `transferDone`) that Task 11 Step 4 adds to `internal/gateway/helpers_test.go`; `broker.Target.XferToken` (always set once Task 12 lands), `xferd.TokenHeader` value `X-Tcdi-Xfer-Token`, and the helper's `/xfer/files/{name}` contract (Task 12); `Gateway.lookupSession`, `ensureTarget`, `session.resolved`, `acquireUpstreamStream`/`releaseUpstreamStream`/`writeStreamCapRefused`, `cleanedProxyPath`, `parseOrigin`/`originsEqual`, `writeJSON`.
- Produces (Task 14 relies on these): on a workspace host,
  - `OPTIONS /v1/transfers/{id}/content` returns 204 for an allowed Origin, with `Access-Control-Allow-Methods: GET, PUT`, `Access-Control-Allow-Headers: Content-Type` and `Access-Control-Max-Age: 600`;
  - `PUT /v1/transfers/{id}/content` (body = the file) returns 200 `{"bytes":n}`;
  - `GET /v1/transfers/{id}/content` returns 200 with the bytes, `Content-Disposition: attachment; filename*=UTF-8''<name>` and `Content-Length`;
  - every answer to an allowed Origin carries `Access-Control-Allow-Origin: <origin>`, `Access-Control-Allow-Credentials: true` and `Vary: Origin`;
  - errors are JSON `{"error": <code>}`: `bad_origin` 403, `unauthorized` 401, `not_found` 404, `transfer_invalid` 410, `transfer_denied` 403, `transfer_busy` 409, `too_large` 413, `file_not_found` 404, `length_required` 411, `upstream_failed` 502, and the existing stream-cap refusal.

- [ ] **Step 1: Confirm the fake broker (no code)**

Task 11 Step 4 already added the transfer methods and fields to the gateway `fakeBroker` in `helpers_test.go` (it had to, because widening `gateway.BrokerClient` breaks the build otherwise). Do not add them again. Check: `rg -n 'func \(f \*fakeBroker\) (ClaimTransfer|CompleteTransfer|completions)|type transferDone' internal/gateway/helpers_test.go` must print exactly four lines. Expected: four lines.

- [ ] **Step 2: Transfer leg tests**

`internal/gateway/transfer_test.go`:

```go
package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

const (
	portalOrigin = "https://portal.test"
	xfID         = "xf_abcdefghijklmnopqrstuvwxyz"
	xfPath       = "/v1/transfers/" + xfID + "/content"
)

// xferStub stands in for tcdi-xferd: it checks the injected credential
// and stores/serves files by name.
type xferStub struct {
	mu    sync.Mutex
	files map[string]string
	paths []string
	srv   *httptest.Server
}

func newXferStub(t *testing.T) *xferStub {
	s := &xferStub{files: map[string]string{"report.pdf": "PDFDATA"}}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.paths = append(s.paths, r.URL.EscapedPath())
		if r.Header.Get("X-Tcdi-Xfer-Token") != "xt" || r.Header.Get("Cookie") != "" {
			http.Error(w, "bad credential", http.StatusUnauthorized)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/xfer/files/")
		switch r.Method {
		case http.MethodPut:
			b, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "short body", http.StatusBadRequest)
				return
			}
			s.files[name] = string(b)
			_ = json.NewEncoder(w).Encode(map[string]int{"bytes": len(b)})
		case http.MethodGet:
			v, ok := s.files[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, v)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *xferStub) file(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.files[name]
	return v, ok
}

func (s *xferStub) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

func transferRig(t *testing.T, grant broker.TransferGrant, xferToken string) (*fakeBroker, *httptest.Server, *xferStub, string) {
	t.Helper()
	fb := newFakeBroker(t)
	stub := newXferStub(t)
	fb.scriptTicket("tk-xfer", testWSUID)
	lease := fb.leaseOf(t, "tk-xfer")
	tgt := targetFor(stub.srv)
	tgt.XferToken = xferToken
	fb.mu.Lock()
	fb.resolveT[lease.ID] = tgt
	if grant.ID != "" {
		fb.transfers[grant.ID] = grant
	}
	fb.mu.Unlock()
	srv := newGateway(t, fb, func(c *gateway.Config) { c.PortalOrigins = []string{portalOrigin} })
	cookie := launchOK(t, srv, testHost, "tk-xfer")
	return fb, srv, stub, cookie
}

func transferReq(t *testing.T, srv *httptest.Server, method, path, cookie, origin, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Host = testHost
	if cookie != "" {
		req.Header.Set("Cookie", gateway.SessionCookieName+"="+cookie)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	res, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestTransfer_UploadRoundTrip(t *testing.T) {
	fb, srv, stub, cookie := transferRig(t, broker.TransferGrant{ID: xfID, WorkspaceUID: testWSUID, Direction: "upload", Path: "Ảnh 1.png", MaxBytes: 100}, "xt")
	res := transferReq(t, srv, http.MethodPut, xfPath, cookie, portalOrigin, "hello")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("upload: %d", res.StatusCode)
	}
	if res.Header.Get("Access-Control-Allow-Origin") != portalOrigin || res.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("CORS headers: %v", res.Header)
	}
	if v, _ := stub.file("Ảnh 1.png"); v != "hello" || stub.seen()[0] != "/xfer/files/%E1%BA%A2nh%201.png" {
		t.Fatalf("helper saw %q at %v", v, stub.seen())
	}
	if got := fb.completions(); len(got) != 1 || got[0] != (transferDone{xfID, 5, true}) {
		t.Fatalf("completions = %+v", got)
	}
	if res := transferReq(t, srv, http.MethodPut, xfPath, cookie, portalOrigin, "again"); res.StatusCode != http.StatusGone {
		t.Fatalf("replayed id: %d, want 410", res.StatusCode)
	}
}

func TestTransfer_DownloadRoundTrip(t *testing.T) {
	fb, srv, _, cookie := transferRig(t, broker.TransferGrant{ID: xfID, WorkspaceUID: testWSUID, Direction: "download", Path: "report.pdf", MaxBytes: 100}, "xt")
	res := transferReq(t, srv, http.MethodGet, xfPath, cookie, portalOrigin, "")
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || string(b) != "PDFDATA" ||
		res.Header.Get("Content-Disposition") != `attachment; filename*=UTF-8''report.pdf` {
		t.Fatalf("download: %d %q %v", res.StatusCode, b, res.Header)
	}
	if got := fb.completions(); len(got) != 1 || got[0] != (transferDone{xfID, 7, true}) {
		t.Fatalf("completions = %+v", got)
	}
}

func TestTransfer_DownloadOverCapAborts(t *testing.T) {
	fb, srv, _, cookie := transferRig(t, broker.TransferGrant{ID: xfID, WorkspaceUID: testWSUID, Direction: "download", Path: "report.pdf", MaxBytes: 3}, "xt")
	res := transferReq(t, srv, http.MethodGet, xfPath, cookie, portalOrigin, "")
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode == http.StatusOK && len(b) > 3 {
		t.Fatalf("over-cap download delivered %d bytes", len(b))
	}
	if got := fb.completions(); len(got) != 1 || got[0].ok {
		t.Fatalf("completions = %+v, want one failed", got)
	}
}

func TestTransfer_UploadOverCapRefused(t *testing.T) {
	fb, srv, stub, cookie := transferRig(t, broker.TransferGrant{ID: xfID, WorkspaceUID: testWSUID, Direction: "upload", Path: "a.bin", MaxBytes: 4}, "xt")
	if res := transferReq(t, srv, http.MethodPut, xfPath, cookie, portalOrigin, "0123456789"); res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("over cap: %d", res.StatusCode)
	}
	if _, ok := stub.file("a.bin"); ok {
		t.Fatal("an over-cap upload reached the helper as a complete file")
	}
	if got := fb.completions(); len(got) != 1 || got[0].ok {
		t.Fatalf("completions = %+v, want one failed", got)
	}
}

func TestTransfer_Refusals(t *testing.T) {
	grant := broker.TransferGrant{ID: xfID, WorkspaceUID: testWSUID, Direction: "upload", Path: "a.txt", MaxBytes: 100}
	_, srv, _, cookie := transferRig(t, grant, "xt")
	cases := []struct {
		name, method, path, cookie, origin string
		want                               int
	}{
		{"no session", http.MethodPut, xfPath, "", portalOrigin, http.StatusUnauthorized},
		{"foreign origin", http.MethodPut, xfPath, cookie, "https://evil.test", http.StatusForbidden},
		{"no origin", http.MethodPut, xfPath, cookie, "", http.StatusForbidden},
		{"malformed id", http.MethodPut, "/v1/transfers/xf_BAD/content", cookie, portalOrigin, http.StatusNotFound},
		{"traversal", http.MethodPut, "/v1/transfers/" + xfID + "/../content", cookie, portalOrigin, http.StatusNotFound},
		{"wrong method", http.MethodPost, xfPath, cookie, portalOrigin, http.StatusMethodNotAllowed},
	}
	for _, c := range cases {
		if res := transferReq(t, srv, c.method, c.path, c.cookie, c.origin, "x"); res.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", c.name, res.StatusCode, c.want)
		}
	}
	pre, _ := http.NewRequest(http.MethodOptions, srv.URL+xfPath, nil)
	pre.Host = testHost
	pre.Header.Set("Origin", portalOrigin)
	pre.Header.Set("Access-Control-Request-Method", "PUT")
	res, err := srv.Client().Transport.(*http.Transport).RoundTrip(pre)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent || res.Header.Get("Access-Control-Allow-Methods") != "GET, PUT" ||
		res.Header.Get("Access-Control-Allow-Origin") != portalOrigin {
		t.Fatalf("preflight: %d %v", res.StatusCode, res.Header)
	}
}

func TestTransfer_BrokerRefusalsMapped(t *testing.T) {
	for _, c := range []struct {
		err  error
		want int
	}{
		{broker.ErrTransferInvalid, http.StatusGone},
		{broker.ErrTransferDenied, http.StatusForbidden},
		{broker.ErrTransferBusy, http.StatusConflict},
	} {
		fb, srv, stub, cookie := transferRig(t, broker.TransferGrant{}, "xt")
		fb.mu.Lock()
		fb.claimErr = c.err
		fb.mu.Unlock()
		if res := transferReq(t, srv, http.MethodPut, xfPath, cookie, portalOrigin, "x"); res.StatusCode != c.want {
			t.Errorf("%v: %d, want %d", c.err, res.StatusCode, c.want)
		}
		if len(stub.seen()) != 0 || len(fb.completions()) != 0 {
			t.Errorf("%v: a refused claim reached the helper or completed", c.err)
		}
	}
}

func TestTransfer_DesktopProxyStillRefusesXfer(t *testing.T) {
	_, srv, stub, cookie := transferRig(t, broker.TransferGrant{}, "xt")
	res := proxied(t, srv, testHost, "/xfer/files/report.pdf", cookie, nil)
	drain(res)
	if res.StatusCode != http.StatusNotFound || len(stub.seen()) != 0 {
		t.Fatalf("generic proxy reached /xfer/: %d %v", res.StatusCode, stub.seen())
	}
	// KasmVNC's own upload/download surface stays off the allowlist too
	// (ADR 0010 §5: the helper is the only transfer path).
	for _, p := range []string{"/xfer", "/api/upload", "/api/download", "/upload", "/downloads/a", "/Downloads/a"} {
		r := proxied(t, srv, testHost, p, cookie, nil)
		drain(r)
		if r.StatusCode != http.StatusNotFound {
			t.Errorf("generic proxy served %s: %d, want 404", p, r.StatusCode)
		}
	}
	if len(stub.seen()) != 0 {
		t.Fatalf("a refused desktop path reached the helper: %v", stub.seen())
	}
}
```

Run `go test ./internal/gateway/ -run Transfer -count=1`. Expected: FAIL (no route; the PUT reaches `serveProxy` and gets 404).

- [ ] **Step 3: Implement `internal/gateway/transfer.go`**

```go
package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/ratelimit"
)

// TransferPathPrefix is the session-host route family for single-use file
// transfers (ADR 0010 T11). The id is an identifier, not a credential:
// the session cookie, the live lease and the broker's single-use claim
// authorize the request.
const TransferPathPrefix = "/v1/transfers/"

const (
	xferTokenHeader = "X-Tcdi-Xfer-Token"
	// transferStreamBudget bounds one transfer's socket time; it matches
	// store.FileTransferClaimDeadAfter so a dead claim and a dead stream
	// age out together.
	transferStreamBudget = 30 * time.Minute
)

var transferPathRE = regexp.MustCompile(`^/v1/transfers/(xf_[a-z2-7]{26})/content$`)

// transferOriginOK: a configured portal origin or the workspace's own
// origin. A missing Origin is refused: browsers send it on every CORS
// request and on same-origin PUTs, and requiring it is the CSRF fence for
// a same-site sibling.
func (g *Gateway) transferOriginOK(o, reqHost string) bool {
	return o != "" && g.launchOriginOK(o, reqHost)
}

func (g *Gateway) serveTransfer(w http.ResponseWriter, r *http.Request, wsID string) {
	origin := r.Header.Get("Origin")
	if !g.transferOriginOK(origin, r.Host) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "bad_origin"})
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Add("Vary", "Origin")
	h.Set("Access-Control-Expose-Headers", "Content-Length, Content-Disposition")
	h.Set("Cache-Control", "no-store")
	clean, ok := cleanedProxyPath(r.URL)
	m := transferPathRE.FindStringSubmatch(clean)
	if !ok || m == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	transferID := m[1]
	if r.Method == http.MethodOptions {
		switch r.Header.Get("Access-Control-Request-Method") {
		case http.MethodGet, http.MethodPut:
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
			return
		}
		h.Set("Access-Control-Allow-Methods", "GET, PUT")
		h.Set("Access-Control-Allow-Headers", "Content-Type")
		h.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		h.Set("Allow", "GET, PUT, OPTIONS")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	s, err := g.lookupSession(r, wsID)
	if err != nil {
		var lim *lookupLimitedError
		if errors.As(err, &lim) {
			h.Set("Retry-After", strconv.Itoa(ratelimit.RetryAfterSeconds(lim.retryAfter)))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
		return
	}
	if s == nil || s.workspaceUID() != wsID {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if err := g.ensureTarget(r.Context(), s); err != nil {
		if terminalBrokerErr(err) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
		return
	}
	t, up, rt := s.resolved()
	if t == nil || up == nil || rt == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
		return
	}
	if r.Method == http.MethodPut && r.ContentLength < 0 {
		// Browsers send a Blob body with Content-Length; a chunked upload
		// cannot be checked against the cap before the claim spends it.
		writeJSON(w, http.StatusLengthRequired, map[string]string{"error": "length_required"})
		return
	}
	if !g.acquireUpstreamStream(wsID) {
		g.writeStreamCapRefused(w, r, wsID)
		return
	}
	defer g.releaseUpstreamStream(wsID)

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	grant, err := g.cfg.Broker.ClaimTransfer(ctx, g.cfg.Identity, s.leaseID(), transferID, r.Method)
	cancel()
	if err != nil {
		writeTransferClaimError(w, err)
		return
	}
	var n int64
	var done bool
	// Completion is recorded even when the client went away or the
	// download aborted (panic(http.ErrAbortHandler) still runs defers):
	// a fresh, bounded context.
	defer func() {
		cctx, ccancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
		defer ccancel()
		if err := g.cfg.Broker.CompleteTransfer(cctx, g.cfg.Identity, s.leaseID(), transferID, n, done); err != nil && g.cfg.Logger != nil {
			g.cfg.Logger.Warn("transfer completion not recorded", "err", err)
		}
	}()
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(transferStreamBudget))
	_ = rc.SetWriteDeadline(time.Now().Add(transferStreamBudget))
	if r.Method == http.MethodPut {
		n, done = g.transferUpload(w, r, grant, t, up, rt)
	} else {
		n, done = g.transferDownload(w, r, grant, t, up, rt)
	}
}

func writeTransferClaimError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, broker.ErrTransferInvalid):
		writeJSON(w, http.StatusGone, map[string]string{"error": "transfer_invalid"})
	case errors.Is(err, broker.ErrTransferDenied):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "transfer_denied"})
	case errors.Is(err, broker.ErrTransferBusy):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "transfer_busy"})
	case terminalBrokerErr(err):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	default:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
	}
}

// helperURL builds the upstream /xfer/files/<name> URL from the resolved
// target authority (never client input) and re-checks it with the same
// parser-differential guard the desktop proxy uses (plan ruling 13).
func helperURL(up *url.URL, name string) (*url.URL, bool) {
	u := &url.URL{Scheme: up.Scheme, Host: up.Host, Path: "/xfer/files/" + name, RawPath: "/xfer/files/" + url.PathEscape(name)}
	clean, ok := cleanedProxyPath(u)
	if !ok || clean != u.Path || strings.Count(clean, "/") != 3 {
		return nil, false
	}
	return u, true
}

// countingReader counts bytes read and fails once more than max arrive.
type countingReader struct {
	r   io.Reader
	n   int64
	max int64
}

var errTransferTooLarge = errors.New("gateway: transfer exceeds its cap")

func (c *countingReader) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	c.n += int64(k)
	if c.n > c.max {
		return k, errTransferTooLarge
	}
	return k, err
}

func (g *Gateway) transferUpload(w http.ResponseWriter, r *http.Request, grant broker.TransferGrant, t *broker.Target, up *url.URL, rt http.RoundTripper) (int64, bool) {
	if r.ContentLength > grant.MaxBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "too_large"})
		return 0, false
	}
	u, ok := helperURL(up, grant.Path)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return 0, false
	}
	body := &countingReader{r: r.Body, max: grant.MaxBytes}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPut, u.String(), body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream_failed"})
		return 0, false
	}
	req.ContentLength = r.ContentLength
	req.Header.Set(xferTokenHeader, t.XferToken)
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := rt.RoundTrip(req)
	if err != nil {
		if errors.Is(err, errTransferTooLarge) || body.n > grant.MaxBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "too_large"})
			return body.n, false
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream_failed"})
		return body.n, false
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
	switch res.StatusCode {
	case http.StatusOK:
		writeJSON(w, http.StatusOK, map[string]int64{"bytes": body.n})
		return body.n, true
	case http.StatusRequestEntityTooLarge:
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "too_large"})
	case http.StatusForbidden:
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "transfer_denied"})
	default:
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream_failed"})
	}
	return body.n, false
}

func (g *Gateway) transferDownload(w http.ResponseWriter, r *http.Request, grant broker.TransferGrant, t *broker.Target, up *url.URL, rt http.RoundTripper) (int64, bool) {
	u, ok := helperURL(up, grant.Path)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return 0, false
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream_failed"})
		return 0, false
	}
	req.Header.Set(xferTokenHeader, t.XferToken)
	res, err := rt.RoundTrip(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream_failed"})
		return 0, false
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusNotFound:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "file_not_found"})
		return 0, false
	case res.StatusCode == http.StatusRequestEntityTooLarge || res.ContentLength > grant.MaxBytes:
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "too_large"})
		return 0, false
	case res.StatusCode != http.StatusOK:
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream_failed"})
		return 0, false
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(grant.Path))
	if res.ContentLength >= 0 {
		h.Set("Content-Length", strconv.FormatInt(res.ContentLength, 10))
	}
	w.WriteHeader(http.StatusOK)
	n, err := io.Copy(w, io.LimitReader(res.Body, grant.MaxBytes+1))
	if err != nil || n > grant.MaxBytes || (res.ContentLength >= 0 && n != res.ContentLength) {
		// Headers are gone; abort the connection so the browser sees a
		// failed download, not a truncated file.
		panic(http.ErrAbortHandler)
	}
	return n, true
}
```

`TestTransfer_DownloadOverCapAborts` asserts that an aborted download still records `ok=false`. The gateway's `statusRecorder` must keep `Unwrap()` so `http.NewResponseController` reaches the real connection; it already does (`proxy.go:1291`).

In `ServeHTTP`, add before `default:`:

```go
	case strings.HasPrefix(r.URL.Path, TransferPathPrefix):
		route = "transfer"
		if !wsOK {
			writeJSON(rec, http.StatusMisdirectedRequest, map[string]string{"error": "bad_host"})
			break
		}
		g.serveTransfer(rec, r, wsID)
```

`upstreamPathOK` stays unchanged: `/xfer/` remains off the desktop allowlist.

Run `go test ./internal/gateway/... -count=1 -race`. Expected: PASS.

- [ ] **Step 4: Docs, threat model, commit**

- Chart README, "Exposure" section: uploads stream through the session host. An ingress controller that caps request bodies (ingress-nginx defaults to 1 MiB) needs `nginx.ingress.kubernetes.io/proxy-body-size` at least `backend.fileTransfer.maxFileBytes` on the session route. Traefik and Gateway API implementations have no default cap.
- Row S50: `| S50 | Gateway transfer leg | Implemented: /v1/transfers/{id}/content on the workspace host only; Origin required and limited to portal origins or the workspace origin (CORS with credentials, preflight answered without a session); session cookie + host match + live lease; broker claim (single use, policy re-resolved, one stream per workspace); counts against the per-workspace stream cap; upstream path built from the grant and re-checked by cleanedProxyPath; credential injected server-side; bytes counted and aborted at the grant cap; completion recorded even on abort; the desktop proxy still 404s /xfer/ |`

```bash
go vet ./internal/gateway/... && go test -count=1 -race ./internal/gateway/...
git add internal/gateway docs/security/threat-model.md deploy/helm/tinycdi/README.md
git commit -m "feat(gateway): single-use transfer leg on the session host

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 14: Portal upload/download on the session page, browser e2e, kind smoke round-trip (F4, part 5) — tier: integrate

**Files:**
- Create: `web/src/session/transfer.ts`, `web/src/session/FileTransfer.tsx`, `web/tests/unit/session/transfer.test.ts`, `web/tests/unit/session/filetransfer.test.tsx`
- Modify: `web/src/session/SessionPage.tsx` (resolved file-transfer policy from the ticket, render `FileTransfer`), `web/src/session/session.css`, `web/src/i18n/{en,vi}/session.ts`, `web/src/api/generated/schema.d.ts` (regenerated)
- Modify: `build/frontend/main.go:160-172` (`frontendCSP`: `connect-src`), `build/frontend/main_test.go` (`TestSecurityHeaders` ~95, `TestFrontendCSP` 132 and 140, `TestFrontendCSP_WildcardSessionDomain` ~158 and ~172)
- Modify: `web/tests/mock-api/workspaces.ts` (mint route, per-workspace file-transfer control, ticket policy), `web/tests-portal/serve.ts` (router grant registration, broker claim/complete, upstream `/xfer/`)
- Create: `web/tests-portal/session-transfer.spec.ts`
- Create: `hack/quickstart/smoke/transfer.ts` (round-trip steps shared by both kind smokes)
- Modify: `hack/quickstart/smoke/smoke.spec.ts` (`cloneBrowserTemplate` extra spec, `freeRunningSlots`, upload→download test, Disabled-hidden assertion), `hack/quickstart/smoke/partitioned.spec.ts` (the same round trip under the partitioned cookie)
- Modify: `docs/runbooks/session-policy.md` (file-transfer section: admin enable/disable, size cap, user-facing behaviour; drops the "inert" sentences), `docs/security/threat-model.md` (the "Frontend security headers" control bullet, ~line 134: add `connect-src`), `docs/adr/0005-backend-frontend-operator.md` (~line 160, portal header table: add a `connect-src` row)

**Interfaces:**
- Consumes: the mint API `POST /v1/workspaces/{workspaceId}/file-transfers` and its 201 body `{id, direction, path, declaredBytes, maxBytes, expiresAt, contentUrl}` (Task 11a); the session-host content route, its CORS contract and its flat gateway error vocabulary `{"error": <code>}` (Task 13: `bad_origin`, `unauthorized`, `not_found`, `transfer_invalid`, `transfer_denied`, `transfer_busy`, `too_large`, `file_not_found`, `length_required`, `upstream_failed`, `rate_limited`, `too_many_streams`); the broker claim/complete wire and `TRANSFER_*` codes (Task 11), `target.xferToken` (Task 12); `ResolvedSessionPolicy.fileTransfer` (a string enum) and `maxFileBytes` on the launch ticket, the live Session policy admin controls, and the pod env `TCDI_XFER_POLICY` (Task 7); `sessionOrigin(workspaceId, sessionDomain)` (`web/src/session/launch.ts:36`). API errors from the mint call are the envelope `{"error":{"code",...}}` (`PortalApiError`, `httpStatus`); only the gateway answers use the flat shape.
- Produces:
  - `export type FileTransferPolicy = "Disabled" | "Upload" | "Download" | "Bidirectional"`
  - `export function transferDirections(p: FileTransferPolicy | undefined): { upload: boolean; download: boolean }`
  - `export function validTransferName(name: string): boolean`
  - `export function checkedContentUrl(contentUrl: string, workspaceId: string, sessionDomain: string): string`
  - `export class TransferError extends Error { readonly code: string }`
  - `export async function putTransferContent(url: string, body: Blob, fetchImpl?: typeof fetch): Promise<number>`, `export async function getTransferContent(url: string, fetchImpl?: typeof fetch): Promise<Blob>`, `export function saveBlob(blob: Blob, name: string): void`
  - `export function FileTransfer(props: { workspaceId: string; sessionDomain: string; policy: FileTransferPolicy; maxFileBytes: number }): JSX.Element`

- [ ] **Step 1: Helpers, tests first**

`web/tests/unit/session/transfer.test.ts`:

```ts
import { describe, expect, it, vi } from "vitest";
import {
  TransferError,
  checkedContentUrl,
  getTransferContent,
  putTransferContent,
  transferDirections,
  validTransferName,
} from "../../../src/session/transfer";

const WS = "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E";
const DOMAIN = "session.example.test";
const ORIGIN = "https://ws-01j4z8kq2m9xnbv3t7yh0r6d5e.session.example.test";
const ID = "xf_abcdefghijklmnopqrstuvwxyz";

describe("transferDirections", () => {
  it("maps each policy", () => {
    expect(transferDirections("Disabled")).toEqual({ upload: false, download: false });
    expect(transferDirections("Upload")).toEqual({ upload: true, download: false });
    expect(transferDirections("Download")).toEqual({ upload: false, download: true });
    expect(transferDirections("Bidirectional")).toEqual({ upload: true, download: true });
    expect(transferDirections(undefined)).toEqual({ upload: false, download: false });
  });
});

describe("validTransferName", () => {
  it("accepts single file names and refuses the rest", () => {
    for (const ok of ["report.pdf", "Ảnh chụp.png", ".hidden", "a b.txt"]) expect(validTransferName(ok)).toBe(true);
    for (const bad of ["", ".", "..", "a/b", "a\\b", "x\u0000", "x\n", "é".repeat(128)]) expect(validTransferName(bad)).toBe(false);
  });
});

describe("checkedContentUrl", () => {
  it("accepts only the workspace's own session origin and route", () => {
    expect(checkedContentUrl(`${ORIGIN}/v1/transfers/${ID}/content`, WS, DOMAIN)).toBe(`${ORIGIN}/v1/transfers/${ID}/content`);
    for (const bad of [
      `https://evil.test/v1/transfers/${ID}/content`,
      `https://ws-other.session.example.test/v1/transfers/${ID}/content`,
      `${ORIGIN}/v1/launch`,
      `${ORIGIN}/v1/transfers/${ID}/content?x=1`,
      `http://ws-01j4z8kq2m9xnbv3t7yh0r6d5e.session.example.test/v1/transfers/${ID}/content`,
    ]) {
      expect(() => checkedContentUrl(bad, WS, DOMAIN)).toThrow(TransferError);
    }
  });
});

describe("content requests", () => {
  it("PUTs with credentials and returns the byte count", async () => {
    const f = vi.fn(async () => new Response(JSON.stringify({ bytes: 5 }), { status: 200 }));
    await expect(putTransferContent("https://h/v1/transfers/x/content", new Blob(["hello"]), f)).resolves.toBe(5);
    expect(f).toHaveBeenCalledWith("https://h/v1/transfers/x/content", expect.objectContaining({ method: "PUT", credentials: "include", mode: "cors" }));
  });
  it("surfaces the gateway error code", async () => {
    const f = vi.fn(async () => new Response(JSON.stringify({ error: "transfer_denied" }), { status: 403 }));
    await expect(getTransferContent("https://h/x", f)).rejects.toMatchObject({ code: "transfer_denied" });
    const g = vi.fn(async () => new Response("garbage", { status: 502 }));
    await expect(getTransferContent("https://h/x", g)).rejects.toMatchObject({ code: "upstream_failed" });
    const n = vi.fn(async () => {
      throw new TypeError("Failed to fetch");
    });
    await expect(getTransferContent("https://h/x", n)).rejects.toMatchObject({ code: "network" });
  });
});
```

Run `cd web && npx vitest run tests/unit/session/transfer.test.ts`. Expected: FAIL (module missing).

- [ ] **Step 2: Implement `web/src/session/transfer.ts`**

```ts
// File transfer between the browser and the workspace's Transfers folder
// (ADR 0010 T11). The portal mints a single-use grant on the API, then
// moves the bytes with a credentialed CORS request to the workspace's own
// session host; the gateway claims the grant and streams to the in-pod
// helper. A download lands in a Blob and is saved with <a download>
// (plan ruling 12), so it works in both cookie modes.
import { sessionOrigin } from "./launch";

export type FileTransferPolicy = "Disabled" | "Upload" | "Download" | "Bidirectional";

export function transferDirections(p: FileTransferPolicy | undefined): { upload: boolean; download: boolean } {
  return {
    upload: p === "Upload" || p === "Bidirectional",
    download: p === "Download" || p === "Bidirectional",
  };
}

const encoder = new TextEncoder();

/** The backend's single-file-name rule (internal/api/filetransfers.go). */
export function validTransferName(name: string): boolean {
  const bytes = encoder.encode(name).length;
  if (bytes === 0 || bytes > 255 || name === "." || name === "..") return false;
  if (/[/\\]/.test(name)) return false;
  // eslint-disable-next-line no-control-regex
  return !/[\u0000-\u001f\u007f-\u009f]/.test(name);
}

export class TransferError extends Error {
  readonly code: string;
  constructor(code: string) {
    super(`file transfer failed: ${code}`);
    this.code = code;
  }
}

const CONTENT_PATH = /^\/v1\/transfers\/xf_[a-z2-7]{26}\/content$/;

/**
 * The content URL comes from the API; it is used only when it names this
 * workspace's own session origin and the transfer route, so a confused or
 * hostile response cannot point a credentialed request anywhere else.
 */
export function checkedContentUrl(contentUrl: string, workspaceId: string, sessionDomain: string): string {
  let u: URL;
  try {
    u = new URL(contentUrl);
  } catch {
    throw new TransferError("bad_url");
  }
  if (u.origin !== sessionOrigin(workspaceId, sessionDomain) || !CONTENT_PATH.test(u.pathname) || u.search || u.hash) {
    throw new TransferError("bad_url");
  }
  return u.href;
}

async function send(url: string, init: RequestInit, fetchImpl: typeof fetch): Promise<Response> {
  let res: Response;
  try {
    res = await fetchImpl(url, { ...init, credentials: "include", mode: "cors", cache: "no-store" });
  } catch {
    throw new TransferError("network");
  }
  if (!res.ok) {
    let code = "upstream_failed";
    try {
      const body = (await res.json()) as { error?: unknown };
      if (typeof body.error === "string") code = body.error;
    } catch {
      /* non-JSON error body */
    }
    throw new TransferError(code);
  }
  return res;
}

export async function putTransferContent(url: string, body: Blob, fetchImpl: typeof fetch = fetch): Promise<number> {
  const res = await send(url, { method: "PUT", body, headers: { "Content-Type": "application/octet-stream" } }, fetchImpl);
  const out = (await res.json()) as { bytes?: number };
  return typeof out.bytes === "number" ? out.bytes : body.size;
}

export async function getTransferContent(url: string, fetchImpl: typeof fetch = fetch): Promise<Blob> {
  const res = await send(url, { method: "GET" }, fetchImpl);
  return res.blob();
}

export function saveBlob(blob: Blob, name: string): void {
  const href = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = href;
  a.download = name;
  a.rel = "noopener";
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(href), 60_000);
}
```

`Content-Type: application/octet-stream` makes the PUT a preflighted CORS request. The gateway answers the preflight (Task 13), and `Access-Control-Allow-Headers: Content-Type` covers it. Run the Step 1 command. Expected: PASS.

- [ ] **Step 3: Regenerate the API types, then the component**

Run `cd web && npm run generate`, then `git diff --stat src/api/generated/schema.d.ts`. Expected: the new `/v1/workspaces/{workspaceId}/file-transfers` path (the OpenAPI path key uses `{workspaceId}`, not the Go route's `{id}`), `CreateFileTransferRequest`, `FileTransfer` and `ResolvedSessionPolicy.maxFileBytes`. Tasks 7 and 11a already regenerated and committed these types, so an empty diff is the expected result and is correct; the point of this step is that `npm run typecheck` below sees the types.

`web/src/session/FileTransfer.tsx`:

```tsx
import { useRef, useState } from "react";
import { t } from "../i18n";
import { useApi } from "../api/context";
import { unwrap } from "../api/client";
import { isPortalApiError } from "../api/errors";
import { Button } from "../design/Button";
import { Input } from "../design/Field";
import { useToast } from "../design/Toast";
import {
  TransferError,
  checkedContentUrl,
  getTransferContent,
  putTransferContent,
  saveBlob,
  transferDirections,
  validTransferName,
  type FileTransferPolicy,
} from "./transfer";

export interface FileTransferProps {
  workspaceId: string;
  sessionDomain: string;
  policy: FileTransferPolicy;
  maxFileBytes: number;
}

type MessageKey = Parameters<typeof t>[0];

function failureKey(err: unknown): MessageKey {
  if (err instanceof TransferError) {
    switch (err.code) {
      case "transfer_denied":
        return "session.transfer.error.denied";
      case "transfer_busy":
        return "session.transfer.error.busy";
      case "too_large":
        return "session.transfer.error.tooLarge";
      case "file_not_found":
        return "session.transfer.error.notFound";
      case "transfer_invalid":
        return "session.transfer.error.expired";
      case "rate_limited":
      case "too_many_streams":
        return "session.transfer.error.rateLimited";
      case "unauthorized":
        return "session.transfer.error.session";
    }
    return "session.transfer.error.generic";
  }
  if (isPortalApiError(err)) {
    if (err.httpStatus === 403) return "session.transfer.error.denied";
    if (err.httpStatus === 409) return "session.transfer.error.notRunning";
    if (err.httpStatus === 429) return "session.transfer.error.rateLimited";
  }
  return "session.transfer.error.generic";
}

function mib(bytes: number): string {
  return (bytes / 1048576).toFixed(bytes % 1048576 === 0 ? 0 : 1);
}

export function FileTransfer({ workspaceId, sessionDomain, policy, maxFileBytes }: FileTransferProps) {
  const api = useApi();
  const { toast } = useToast();
  const fileInput = useRef<HTMLInputElement>(null);
  const [busy, setBusy] = useState<"upload" | "download" | null>(null);
  const [name, setName] = useState("");
  const dirs = transferDirections(policy);
  if (!dirs.upload && !dirs.download) return null;

  const mint = async (direction: "upload" | "download", path: string, bytes?: number) => {
    const grant = unwrap(
      await api.POST("/v1/workspaces/{workspaceId}/file-transfers", {
        params: { path: { workspaceId } },
        body: { direction, path, ...(bytes === undefined ? {} : { bytes }) },
      }),
    );
    return checkedContentUrl(grant.contentUrl, workspaceId, sessionDomain);
  };

  const upload = async (file: File) => {
    if (!validTransferName(file.name)) {
      toast({ tone: "danger", title: t("session.transfer.error.badName") });
      return;
    }
    if (file.size > maxFileBytes) {
      toast({ tone: "danger", title: t("session.transfer.error.tooLarge"), description: t("session.transfer.limit", { mib: mib(maxFileBytes) }) });
      return;
    }
    setBusy("upload");
    try {
      const url = await mint("upload", file.name, file.size);
      await putTransferContent(url, file);
      toast({ tone: "success", title: t("session.transfer.uploaded", { name: file.name }) });
    } catch (err) {
      toast({ tone: "danger", title: t(failureKey(err)) });
    } finally {
      setBusy(null);
      if (fileInput.current) fileInput.current.value = "";
    }
  };

  const download = async () => {
    const wanted = name.trim();
    if (!validTransferName(wanted)) {
      toast({ tone: "danger", title: t("session.transfer.error.badName") });
      return;
    }
    setBusy("download");
    try {
      const url = await mint("download", wanted);
      saveBlob(await getTransferContent(url), wanted);
      toast({ tone: "success", title: t("session.transfer.downloaded", { name: wanted }) });
    } catch (err) {
      toast({ tone: "danger", title: t(failureKey(err)) });
    } finally {
      setBusy(null);
    }
  };

  return (
    <section className="tc-session__transfer" aria-label={t("session.transfer.region")}>
      <p className="tc-session__transfer-hint">
        {t("session.transfer.folderHint")} {t("session.transfer.limit", { mib: mib(maxFileBytes) })}
      </p>
      {dirs.upload ? (
        <>
          <input
            ref={fileInput}
            type="file"
            className="tc-sr-only"
            aria-label={t("session.transfer.uploadInput")}
            onChange={(e) => {
              const f = e.currentTarget.files?.[0];
              if (f) void upload(f);
            }}
          />
          <Button size="sm" variant="secondary" loading={busy === "upload"} disabled={busy !== null} onClick={() => fileInput.current?.click()}>
            {t("session.transfer.upload")}
          </Button>
        </>
      ) : null}
      {dirs.download ? (
        <form
          className="tc-session__transfer-download"
          onSubmit={(e) => {
            e.preventDefault();
            void download();
          }}
        >
          <Input
            label={t("session.transfer.downloadName")}
            hideLabel
            value={name}
            placeholder={t("session.transfer.downloadPlaceholder")}
            onChange={(e) => setName(e.currentTarget.value)}
          />
          <Button size="sm" variant="secondary" type="submit" loading={busy === "download"} disabled={busy !== null || name.trim() === ""}>
            {t("session.transfer.download")}
          </Button>
        </form>
      ) : null}
    </section>
  );
}
```

`SessionPage.tsx`:
- imports: `import { FileTransfer } from "./FileTransfer";` and `import type { FileTransferPolicy } from "./transfer";`;
- next to `resolvedClipboard` (`SessionPage.tsx:173`), keep `const [resolvedTransfer, setResolvedTransfer] = useState<{ policy: FileTransferPolicy; maxFileBytes: number }>({ policy: "Disabled", maxFileBytes: 0 })`;
- in `noteResolvedPolicy` (`SessionPage.tsx:177`), add `setResolvedTransfer({ policy: (ticket.sessionPolicy?.fileTransfer ?? "Disabled") as FileTransferPolicy, maxFileBytes: ticket.sessionPolicy?.maxFileBytes ?? 0 })`;
- render `{connected && sessionDomain ? <FileTransfer workspaceId={workspaceId} sessionDomain={sessionDomain} policy={resolvedTransfer.policy} maxFileBytes={resolvedTransfer.maxFileBytes} /> : null}` between the toolbar's closing `</div>` and `<div className="tc-session__stage" ref={stageRef} …>` (`SessionPage.tsx:919`).

The policy comes from the ticket, which is the resolved policy recorded at launch, never the catalog (`resolvedTransfer` starts `Disabled`, so nothing shows before the first ticket). When it is `Disabled` the component renders nothing. The bar is shown only while `connected`, so a stopped or reconnecting workspace never offers a transfer.

`session.css` (token names verified against the file; `web/tests/unit/design/token-refs.test.ts` fails on an unknown token):

```css
.tc-session__transfer {
  display: flex;
  flex-wrap: wrap;
  gap: var(--tc-space-2);
  align-items: center;
  padding: var(--tc-space-2) var(--tc-space-3);
  background: var(--tc-color-surface);
  border-bottom: 1px solid var(--tc-color-border);
}
.tc-session__transfer-download {
  display: flex;
  gap: var(--tc-space-2);
  align-items: flex-end;
}
.tc-session__transfer-hint {
  margin: 0;
  flex-basis: 100%;
  color: var(--tc-color-text-muted);
  font-size: var(--tc-text-sm);
}
```

Strings (`en/session.ts`; add inside the default export, before the closing `} as const;`):

```ts
  "session.transfer.region": "File transfer",
  "session.transfer.folderHint": "Files move to and from the Transfers folder in your workspace home.",
  "session.transfer.limit": "Up to {mib} MiB per file.",
  "session.transfer.upload": "Upload file",
  "session.transfer.uploadInput": "Choose a file to upload",
  "session.transfer.download": "Download",
  "session.transfer.downloadName": "File name in the Transfers folder",
  "session.transfer.downloadPlaceholder": "report.pdf",
  "session.transfer.uploaded": "Uploaded {name} to Transfers.",
  "session.transfer.downloaded": "Downloaded {name}.",
  "session.transfer.error.badName": "Use a single file name without folders.",
  "session.transfer.error.tooLarge": "The file is larger than the transfer limit.",
  "session.transfer.error.denied": "File transfer in this direction is not allowed for this workspace.",
  "session.transfer.error.busy": "Another transfer is in progress for this workspace. Try again when it finishes.",
  "session.transfer.error.notFound": "No file with that name is in the Transfers folder.",
  "session.transfer.error.expired": "The transfer expired before it started. Try again.",
  "session.transfer.error.notRunning": "The workspace must be running to transfer files.",
  "session.transfer.error.rateLimited": "Too many transfers in a short time. Wait a moment and try again.",
  "session.transfer.error.session": "The session is no longer connected. Reconnect and try again.",
  "session.transfer.error.generic": "The transfer failed. Try again.",
```

`vi/session.ts` (before the closing `} satisfies Record<keyof typeof session, string>;`):

```ts
  "session.transfer.region": "Truyền tệp",
  "session.transfer.folderHint": "Tệp được chuyển vào và lấy ra từ thư mục Transfers trong thư mục chính của workspace.",
  "session.transfer.limit": "Tối đa {mib} MiB mỗi tệp.",
  "session.transfer.upload": "Tải tệp lên",
  "session.transfer.uploadInput": "Chọn tệp để tải lên",
  "session.transfer.download": "Tải xuống",
  "session.transfer.downloadName": "Tên tệp trong thư mục Transfers",
  "session.transfer.downloadPlaceholder": "baocao.pdf",
  "session.transfer.uploaded": "Đã tải {name} lên Transfers.",
  "session.transfer.downloaded": "Đã tải xuống {name}.",
  "session.transfer.error.badName": "Hãy dùng một tên tệp, không kèm thư mục.",
  "session.transfer.error.tooLarge": "Tệp lớn hơn giới hạn truyền.",
  "session.transfer.error.denied": "Workspace này không cho phép truyền tệp theo chiều này.",
  "session.transfer.error.busy": "Workspace đang có một lượt truyền khác. Hãy thử lại khi lượt đó xong.",
  "session.transfer.error.notFound": "Không có tệp nào với tên đó trong thư mục Transfers.",
  "session.transfer.error.expired": "Lượt truyền đã hết hạn trước khi bắt đầu. Hãy thử lại.",
  "session.transfer.error.notRunning": "Workspace phải đang chạy mới truyền được tệp.",
  "session.transfer.error.rateLimited": "Quá nhiều lượt truyền trong thời gian ngắn. Hãy đợi một lát rồi thử lại.",
  "session.transfer.error.session": "Phiên không còn kết nối. Hãy kết nối lại rồi thử lại.",
  "session.transfer.error.generic": "Truyền tệp thất bại. Hãy thử lại.",
```

`web/tests/unit/session/filetransfer.test.tsx` (the component's policy rendering and its client-side refusals; the mock API records every request):

```tsx
import { describe, expect, it } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import { FileTransfer } from "../../../src/session/FileTransfer";
import { createMockApi } from "../../mock-api/handler.ts";
import { renderWithApi } from "../helpers";

const WS = "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E";

function mounted(policy: "Disabled" | "Upload" | "Download" | "Bidirectional", maxFileBytes = 1024) {
  const api = createMockApi();
  const { unmount } = renderWithApi(
    <FileTransfer workspaceId={WS} sessionDomain="session.example.test" policy={policy} maxFileBytes={maxFileBytes} />,
    api,
  );
  return { api, unmount };
}

const mints = (api: ReturnType<typeof createMockApi>) => api.state.requests.filter((r) => r.path.endsWith("/file-transfers"));

describe("FileTransfer", () => {
  it("renders nothing when the resolved policy is Disabled", () => {
    mounted("Disabled");
    expect(screen.queryByRole("region", { name: "File transfer" })).toBeNull();
  });

  it("offers only the allowed directions", () => {
    const up = mounted("Upload");
    expect(screen.getByLabelText("Choose a file to upload")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Download" })).toBeNull();
    up.unmount();
    const down = mounted("Download");
    expect(screen.queryByLabelText("Choose a file to upload")).toBeNull();
    expect(screen.getByRole("button", { name: "Download" })).toBeTruthy();
    down.unmount();
    mounted("Bidirectional");
    expect(screen.getByLabelText("Choose a file to upload")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Download" })).toBeTruthy();
  });

  it("refuses an over-limit file before minting anything", async () => {
    const { api } = mounted("Upload", 4);
    fireEvent.change(screen.getByLabelText("Choose a file to upload"), {
      target: { files: [new File(["12345"], "big.bin")] },
    });
    expect(await screen.findByText("The file is larger than the transfer limit.")).toBeTruthy();
    expect(mints(api)).toEqual([]);
  });
});
```

Run `cd web && npm run typecheck && npm run lint:strings && npm run test:unit`. Expected: exit 0.

- [ ] **Step 4: Portal CSP allows the session hosts for `connect-src`**

Without it the credentialed `fetch()` to `https://ws-….<sessionDomain>` is blocked (`default-src 'self'` is the `connect-src` fallback). In `build/frontend/main.go`, `frontendCSP` (line 166) becomes:

```go
func frontendCSP(domain *sessionhost.Domain) string {
	if domain == nil {
		return cspBase + "; connect-src 'self'; frame-src 'none'; form-action 'self'"
	}
	origin := "https://" + domain.Wildcard()
	return cspBase + "; connect-src 'self' " + origin + "; frame-src " + origin + "; form-action 'self' " + origin
}
```

and the doc comment above it gains one line: "connect-src: the file-transfer content requests (ADR 0010 T11) go to the workspace's own session host; the portal opens no other cross-origin connection."

`build/frontend/main_test.go`: the two exact-match expectations change, `TestFrontendCSP` line 132 to `base+"; connect-src 'self'; frame-src 'none'; form-action 'self'"` and line 140 to

```go
		want := base + "; connect-src 'self' https://" + d.Wildcard() + "; frame-src https://" + d.Wildcard() + "; form-action 'self' https://" + d.Wildcard()
```

and the `Contains` lists gain `"connect-src 'self' https://*.session.test"` (`TestSecurityHeaders`, next to line 95), `"connect-src 'self' https://*.session.example.com"` (`TestFrontendCSP_WildcardSessionDomain`, next to line 158) and `"connect-src 'self' https://*.session.example.com:8443"` (next to line 172). Run `go test ./build/frontend/ -count=1`. Expected: PASS. `web/tests-portal/portal-csp.spec.ts` reads the live header; it runs in Step 6.

Docs for the header change: in `docs/security/threat-model.md` (the "Frontend security headers" bullet, ~line 134) add `connect-src 'self' https://*.<sessionDomain>` to the CSP list, and in `docs/adr/0005-backend-frontend-operator.md` (~line 160) add this row to the portal header table:

```
| portal | `connect-src` | `'self' https://*.<sessionDomain>` (file-transfer content requests, ADR 0010 T11) |
```

- [ ] **Step 5: Mock API and portal harness**

`web/tests/mock-api/workspaces.ts` (symbols, not line numbers: the file is `workspacesArea(ctx)`; area-private state lives in the factory closure, not in `MockState`):
- add `sessionHostLabel` to the existing `import { … } from "./core.ts"` list;
- after `const { state } = ctx;` add `const fileTransfer = new Map<string, { policy: string; maxFileBytes: number }>();` and make `reset()` start with `fileTransfer.clear();` (`/_control/reset` calls it after clearing the core state);
- in `control()`, add this route BEFORE the `const wsm = p.match(/^\/_control\/workspaces\/([^/]+)(\/events)?$/)` line, because `wsm` would otherwise take `fileTransfer` for a workspace id and answer 404:

```ts
    // POST /_control/workspaces/fileTransfer {workspaceId, policy, maxFileBytes}
    // — the resolved file-transfer vector the next ticket records.
    if (p === "/_control/workspaces/fileTransfer" && req.method === "POST") {
      const b = (req.body ?? {}) as { workspaceId: string; policy: string; maxFileBytes: number };
      fileTransfer.set(b.workspaceId, { policy: b.policy, maxFileBytes: b.maxFileBytes });
      return ok(200, { workspaceId: b.workspaceId, ...fileTransfer.get(b.workspaceId) });
    }
```

- in `createConnection`, replace `fileTransfer: { mode: "Disabled", maxFileBytes: null },` (the old shape was an object, which the contract's string enum never allowed) with
  `fileTransfer: fileTransfer.get(ws.id)?.policy ?? "Disabled", maxFileBytes: fileTransfer.get(ws.id)?.maxFileBytes ?? 0,`;
- in `api()`, the route regex becomes `/^\/v1\/workspaces\/([^/:]+)(:start|:stop|\/connections|\/events|\/file-transfers)?$/` and the dispatch list gains `if (sub === "/file-transfers" && method === "POST") return mintTransfer(req, ws);`;
- the handler (it mirrors the real mint: 403 `FORBIDDEN` with `details.reason: "transfer_denied"` when the policy refuses the direction, 409 `INVALID_STATE` when the workspace is not Ready, 400 `INVALID_REQUEST` for a bad name or an upload over the cap):

```ts
  function mintTransfer(req: MockRequest, ws: WorkspaceFixture): MockResponse {
    const b = (req.body ?? {}) as { direction?: string; path?: string; bytes?: number };
    const ft = fileTransfer.get(ws.id) ?? { policy: "Disabled", maxFileBytes: 0 };
    if (!b.path || b.path === "." || b.path === ".." || /[/\\\u0000-\u001f]/.test(b.path)) {
      return err(400, "INVALID_REQUEST", "path must be a single file name", false);
    }
    if (ws.phase !== "Ready") return err(409, "INVALID_STATE", "the workspace must be running to transfer files", false);
    const allowed =
      ft.policy === "Bidirectional" || (ft.policy === "Upload" && b.direction === "upload") || (ft.policy === "Download" && b.direction === "download");
    if (!allowed) {
      const denied = err(403, "FORBIDDEN", `file ${b.direction} is not allowed for this workspace`, false);
      (denied.body as { error: { details?: unknown } }).error.details = { reason: "transfer_denied" };
      return denied;
    }
    if (b.direction === "upload" && (b.bytes === undefined || b.bytes < 0 || b.bytes > ft.maxFileBytes)) {
      return err(400, "INVALID_REQUEST", "bytes is over the transfer limit", false);
    }
    const alphabet = "abcdefghijklmnopqrstuvwxyz234567";
    const id = "xf_" + Array.from({ length: 26 }, () => alphabet[Math.floor(Math.random() * 32)]).join("");
    return ok(201, {
      id,
      direction: b.direction,
      path: b.path,
      declaredBytes: b.direction === "upload" ? b.bytes : 0,
      maxBytes: ft.maxFileBytes,
      expiresAt: new Date(ctx.now() + 120_000).toISOString(),
      contentUrl: `${ctx.sessionOriginUrl.protocol}//${sessionHostLabel(ws.id)}.${ctx.sessionDomain}/v1/transfers/${id}/content`,
    });
  }
```

`web/tests-portal/serve.ts` (the harness comment block at the top gains one line: "also registers file-transfer grants and fakes the broker claim/complete and the in-pod helper"):
- upstream state, next to `upstreamBehavior` (line ~243, before `brokerReset` can run): `const xferFiles = new Map<string, Buffer>();`;
- broker state: add to `brokerState` the fields `transfers: new Map<string, { workspaceUID: string; direction: string; path: string; maxBytes: number; used: boolean }>()` and `completed: [] as Array<{ id: string; bytes: number; ok: boolean }>`; `brokerReset()` also clears `brokerState.transfers`, `brokerState.completed` and `xferFiles`; and next to `brokerRegisterTicket`:

```ts
function brokerRegisterTransfer(id: string, workspaceUID: string, direction: string, path: string, maxBytes: number) {
  brokerState.transfers.set(id, { workspaceUID, direction, path, maxBytes, used: false });
}
```

- router (`handleApi`): add `const XFER_RE = /^\/v1\/workspaces\/([^/]+)\/file-transfers$/;` beside `CONN_RE`; inside `handleApi` add `const xferMatch = req.method === "POST" ? req.url?.match(XFER_RE) : null;`, change the pass-through guard to `if (!connMatch && !xferMatch && !isMe)`, and in the 2xx rewrite `try` turn `if (connMatch) {…} else {…}` into:

```ts
              if (connMatch) {
                /* unchanged */
              } else if (xferMatch) {
                // The real API + broker pair: the API mints the row, the broker
                // later claims it; the contentUrl names the active session site.
                const wsId = xferMatch[1];
                brokerRegisterTransfer(
                  json.id, wsId, json.direction, json.path,
                  json.direction === "upload" ? Math.min(json.declaredBytes, json.maxBytes) : json.maxBytes,
                );
                json.contentUrl = `https://${workspaceLabel(wsId)}.${sessionDomain()}/v1/transfers/${json.id}/content`;
              } else {
                json.sessionDomain = sessionDomain();
              }
```

- broker handler: the request body is parsed into `let body: { ticket?: string } = {};`; widen it to `let body: { ticket?: string; method?: string; bytes?: number; ok?: boolean } = {};`. Right after the `/internal/v1/broker/redeem` branch and before `const m = req.url?.match(LEASES_RE)`, add (the fake answers in the broker's flat `{code,message,…}` shape via `apiErr`, which `brokerclient.sentinelFor` maps by `TRANSFER_INVALID`):

```ts
      const XFER_LEASE_RE = /^\/internal\/v1\/broker\/leases\/([^/]+)\/transfers\/([^/]+)\/(claim|complete)$/;
      const xm = req.url?.match(XFER_LEASE_RE);
      if (xm) {
        const lease = brokerState.leases.get(xm[1]);
        const tr = brokerState.transfers.get(xm[2]);
        if (xm[3] === "claim") {
          const wantMethod = tr?.direction === "upload" ? "PUT" : "GET";
          if (!lease || brokerState.revoked.has(xm[1]) || !tr || tr.used || tr.workspaceUID !== lease.workspaceUID || body.method !== wantMethod) {
            apiErr(res, 410, "TRANSFER_INVALID", "transfer invalid, used or expired");
            return;
          }
          tr.used = true;
          res.writeHead(200, { "content-type": "application/json" });
          res.end(JSON.stringify({ id: xm[2], workspaceUID: tr.workspaceUID, direction: tr.direction, path: tr.path, maxBytes: tr.maxBytes }));
          return;
        }
        brokerState.completed.push({ id: xm[2], bytes: body.bytes ?? 0, ok: body.ok === true });
        res.writeHead(204);
        res.end();
        return;
      }
```

- the `target` response object gains `xferToken: "xt-e2e",` (the wire name is `xferToken`, Task 12);
- upstream (`https.createServer(tlsOpts, (req, res) => {` at line ~282): insert before the Basic-auth check the stub of `tcdi-xferd` — it demands the transfer credential and refuses the desktop credentials, so a gateway that leaked the Basic header or the session cookie would fail the test:

```ts
  if (req.url?.startsWith("/xfer/files/")) {
    if (req.headers["x-tcdi-xfer-token"] !== "xt-e2e" || req.headers.authorization !== undefined || req.headers.cookie !== undefined) {
      res.writeHead(401);
      res.end("bad transfer credential");
      return;
    }
    const name = decodeURIComponent(req.url.slice("/xfer/files/".length));
    if (req.method === "PUT") {
      const chunks: Buffer[] = [];
      req.on("data", (c: Buffer) => chunks.push(c));
      req.on("end", () => {
        const data = Buffer.concat(chunks);
        xferFiles.set(name, data);
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({ bytes: data.length }));
      });
      return;
    }
    const f = xferFiles.get(name);
    if (!f) {
      res.writeHead(404);
      res.end();
      return;
    }
    res.writeHead(200, { "content-type": "application/octet-stream", "content-length": String(f.length) });
    res.end(f);
    return;
  }
```

- control server (`const control = http.createServer(…)`, next to `GET /healthz`): `if (req.method === "GET" && req.url === "/broker/transfers") return reply(200, { completed: brokerState.completed });` and add `GET /broker/transfers — transfers the fake broker saw completed` to the control-API comment at the top of the file.

- [ ] **Step 6: Browser e2e in both cookie modes**

`web/tests-portal/session-transfer.spec.ts`:

```ts
import fs from "node:fs";
import {
  HARNESS_CONTROL,
  MOCK_API,
  expect,
  login,
  openSession,
  resetState,
  seedReadyWorkspace,
  test,
  watchConsole,
} from "./harness.ts";

// session-transfer: upload then download through the real gateway and
// frontend, against the fake broker and a stub tcdi-xferd. Runs once per
// site layout, so the credentialed CORS request is proven with the
// SameSite=Lax cookie and with the partitioned one (plan ruling 12).
const WS = "ws_e2e0003aabbcc";

test("upload then download a file through the session host", async ({ page, request, harnessMode }) => {
  const violations = watchConsole(page);
  await resetState(request, harnessMode);
  await seedReadyWorkspace(request, WS);
  const ctl = await request.post(`${MOCK_API}/_control/workspaces/fileTransfer`, {
    data: { workspaceId: WS, policy: "Bidirectional", maxFileBytes: 1048576 },
  });
  expect(ctl.ok()).toBeTruthy();
  await login(page);
  await openSession(page, harnessMode, WS);

  await page.getByLabel("Choose a file to upload").setInputFiles({
    name: "Ảnh e2e.txt",
    mimeType: "text/plain",
    buffer: Buffer.from("hello transfer"),
  });
  await expect(page.getByText("Uploaded Ảnh e2e.txt to Transfers.")).toBeVisible();

  await page.getByLabel("File name in the Transfers folder").fill("Ảnh e2e.txt");
  const [download] = await Promise.all([page.waitForEvent("download"), page.getByRole("button", { name: "Download" }).click()]);
  expect(download.suggestedFilename()).toBe("Ảnh e2e.txt");
  expect(fs.readFileSync(await download.path(), "utf8")).toBe("hello transfer");

  // The gateway records completion after the response is written, so poll.
  const completed = async () => ((await (await request.get(`${HARNESS_CONTROL}/broker/transfers`)).json()) as { completed: unknown[] }).completed;
  await expect.poll(async () => (await completed()).length).toBe(2);
  expect(await completed()).toEqual([
    expect.objectContaining({ bytes: 14, ok: true }),
    expect.objectContaining({ bytes: 14, ok: true }),
  ]);
  expect(violations, "no CSP violations (connect-src reaches the session host)").toEqual([]);
});

test("the controls are hidden when the resolved policy is Disabled", async ({ page, request, harnessMode }) => {
  await resetState(request, harnessMode);
  await seedReadyWorkspace(request, WS);
  await login(page);
  await openSession(page, harnessMode, WS);
  await expect(page.getByRole("region", { name: "File transfer" })).toHaveCount(0);
});

test("an over-limit file is refused before anything is minted", async ({ page, request, harnessMode }) => {
  const mints: string[] = [];
  page.on("request", (r) => {
    if (r.method() === "POST" && r.url().endsWith("/file-transfers")) mints.push(r.url());
  });
  await resetState(request, harnessMode);
  await seedReadyWorkspace(request, WS);
  await request.post(`${MOCK_API}/_control/workspaces/fileTransfer`, {
    data: { workspaceId: WS, policy: "Upload", maxFileBytes: 4 },
  });
  await login(page);
  await openSession(page, harnessMode, WS);
  await expect(page.getByRole("button", { name: "Download" })).toHaveCount(0);
  await page.getByLabel("Choose a file to upload").setInputFiles({ name: "big.bin", mimeType: "application/octet-stream", buffer: Buffer.alloc(5) });
  await expect(page.getByText("The file is larger than the transfer limit.")).toBeVisible();
  expect(mints, "no grant minted for an over-limit file").toEqual([]);
  const done = await (await request.get(`${HARNESS_CONTROL}/broker/transfers`)).json();
  expect(done.completed).toEqual([]);
});

test("a direction the resolved policy refuses is not offered", async ({ page, request, harnessMode }) => {
  await resetState(request, harnessMode);
  await seedReadyWorkspace(request, WS);
  await request.post(`${MOCK_API}/_control/workspaces/fileTransfer`, {
    data: { workspaceId: WS, policy: "Download", maxFileBytes: 1048576 },
  });
  await login(page);
  await openSession(page, harnessMode, WS);
  await expect(page.getByRole("button", { name: "Download" })).toBeVisible();
  await expect(page.getByLabel("Choose a file to upload")).toHaveCount(0);
});
```

`seedReadyWorkspace(request, id)` (`harness.ts:123`) takes the id verbatim and it must satisfy `ws_` + `[a-z0-9]{8,60}`; `ws_e2e0003aabbcc` is free (the other specs use `ws_e2e0001aabbcc`, `ws_e2e0002aabbcc` and `ws_csp0001aabbcc`; re-check with `rg -n 'ws_e2e0003' web/tests-portal`).

Run `cd web && npx playwright test --config playwright.portal.config.ts session-transfer.spec.ts portal-csp.spec.ts > /tmp/pw-transfer.log 2>&1; echo "rc=$?"; grep -nE '✘|failed|Error' /tmp/pw-transfer.log | head -20`. Expected: `rc=0`; both projects (`lax`, `partitioned`) pass.

- [ ] **Step 7: Kind smoke round-trip, in both cookie modes**

The `lax` kind job (`quickstart`, `playwright.config.ts` → `smoke.spec.ts`) and the `partitioned` kind job (`playwright.partitioned.config.ts` → `partitioned.spec.ts`, installed with `values-partitioned.yaml`) are separate installs, so each spec runs the round trip once. The steps are shared in a new helper module (not a spec, so Playwright's `testMatch` ignores it).

`hack/quickstart/smoke/transfer.ts`:

```ts
import * as fs from "node:fs";
import { expect, type Page } from "@playwright/test";

// Upload then download one file through the session page's transfer bar,
// checking the bytes on the pod's disk on both legs, then try a name that
// would escape the Transfers folder. `inHome(script, ...args)` runs
// `sh -c script _ args…` in the workspace's desktop container (kubectl
// exec); `transfersDir` is the folder inside the pod ($HOME/transfers).
export async function transferRoundTrip(
  page: Page,
  stamp: string,
  transfersDir: string,
  inHome: (script: string, ...args: string[]) => string,
) {
  const up = `up-${stamp}.txt`;
  await page.getByLabel("Choose a file to upload").setInputFiles({
    name: up,
    mimeType: "text/plain",
    buffer: Buffer.from(`hello ${stamp}`),
  });
  await expect(page.getByText(`Uploaded ${up} to Transfers.`)).toBeVisible({ timeout: 60_000 });
  expect(inHome(`cat "${transfersDir}/$1"`, up)).toBe(`hello ${stamp}`);

  const down = `down-${stamp}.txt`;
  inHome(`printf 'from the pod %s' "$2" > "${transfersDir}/$1"`, down, stamp);
  await page.getByLabel("File name in the Transfers folder").fill(down);
  const [dl] = await Promise.all([
    page.waitForEvent("download", { timeout: 60_000 }),
    page.getByRole("button", { name: "Download" }).click(),
  ]);
  expect(dl.suggestedFilename()).toBe(down);
  expect(fs.readFileSync(await dl.path(), "utf8")).toBe(`from the pod ${stamp}`);

  // An escape attempt never reaches the API: the name is refused client-side
  // (the server and the in-pod jail refuse it too, Tasks 11-13).
  await page.getByLabel("File name in the Transfers folder").fill("../.bashrc");
  await page.getByRole("button", { name: "Download" }).click();
  await expect(page.getByText("Use a single file name without folders.")).toBeVisible();
  console.log(`file transfer round-trip ok: ${up} up, ${down} down`);
}
```

`hack/quickstart/smoke/smoke.spec.ts` (locate by symbol; line numbers move as earlier tasks add tests):
- `import { transferRoundTrip } from "./transfer";`
- `cloneBrowserTemplate(name, display, policy, extraSpec: Record<string, unknown> = {})` (`smoke.spec.ts:158`), with `spec: { ...src.spec, revision: \`clip-${name}\`, clipboardPolicy: policy, ...extraSpec }`;
- a helper next to `stopWorkspace`, because `runningWorkspaces` is 2 and the earlier smokes leave workspaces running; the clipboard test's `runningWorkspaceWithPolicy` finds only clipboard-policy matches and can re-find a pod that is still terminating, so this one asks the API:

```ts
// Stops every Ready workspace of the signed-in tenant so the next create
// has a running slot (the create retries through release_pending).
async function freeRunningSlots(page: Page) {
  const ids = await page.evaluate(async () => {
    const r = await fetch("/v1/workspaces?phase=Ready");
    return r.ok ? (((await r.json()) as { items?: Array<{ id: string }> }).items ?? []).map((w) => w.id) : [];
  });
  for (const id of ids) await stopWorkspace(page, id);
}
```

- a new test after the clipboard test (end of file):

```ts
test("file transfer: upload then download through the Transfers folder", async ({ page }) => {
  const stamp = Date.now().toString(36);
  const tplName = `xfer-bidi-${stamp}`;
  // The seeded catalog is all Disabled; the transfer path needs its own entry.
  cloneBrowserTemplate(tplName, `Transfer ${stamp}`, "Disabled", { fileTransferPolicy: "Bidirectional" });
  await login(page);
  await freeRunningSlots(page);
  const id = await createWorkspaceOnTemplate(page, `xfer-${stamp}`, new RegExp(tplName, "i"));
  try {
    // The pod object lands during provisioning: poll until it carries the rendered policy.
    await expect
      .poll(
        () => {
          try {
            return podEnv(id)["TCDI_XFER_POLICY"] ?? null;
          } catch {
            return null;
          }
        },
        { timeout: 120_000 },
      )
      .toBe("Bidirectional");
    expect(podEnv(id)["TCDI_XFER_MAX_BYTES"], "the platform default bound").toBe("104857600");
    await connectSessionFrame(page, id);
    await transferRoundTrip(page, stamp, `${HOME_DIR}/transfers`, (script, ...args) => inHome(id, script, ...args));
  } finally {
    await stopWorkspace(page, id);
  }
});
```

- in the first test ("log in, create a browser workspace and see the session frame load"), after its last assertion (`expect(tickets, "no second launch ticket in 120 s").toEqual([]);`), add the Disabled negative on the seeded template: `await expect(page.getByRole("region", { name: "File transfer" })).toHaveCount(0);`.

`hack/quickstart/smoke/partitioned.spec.ts` (the same round trip under the `SameSite=None; Secure; Partitioned` cookie; the first test leaves its workspace running, which uses one of the two slots):
- imports: `import * as fs from "node:fs"; import * as os from "node:os"; import * as path from "node:path"; import { transferRoundTrip } from "./transfer";`
- constants beside `SYSTEM_NAMESPACE`: `const TENANT_NAMESPACE = process.env.TCDI_QS_TENANT_NAMESPACE ?? "tinycdi-tenant-a"; const HOME_DIR = "/home/workspace";`
- `createBrowserWorkspace(page, name, label = /browser/i)` takes the template label (`hasText: label` instead of `/browser/i`); existing callers are unchanged;
- a new test at the end of the file:

```ts
test("partitioned cookie mode: file transfer round trip over the CHIPS cookie", async ({ page }) => {
  test.setTimeout(20 * 60_000);
  const stamp = Date.now().toString(36);
  const tplName = `xfer-part-${stamp}`;
  const src = (
    JSON.parse(
      kubectlIn(TENANT_NAMESPACE, "get", "workspacetemplates", "-l", "workspaces.cdi.tinyorbit.vn/catalog-name=browser", "-o", "json"),
    ) as { items: Array<Record<string, any>> }
  ).items[0];
  expect(src, "seeded browser template").toBeTruthy();
  const clone = {
    apiVersion: src.apiVersion,
    kind: src.kind,
    metadata: {
      name: tplName,
      namespace: TENANT_NAMESPACE,
      labels: { "workspaces.cdi.tinyorbit.vn/catalog-name": tplName },
      annotations: { ...(src.metadata.annotations ?? {}), "workspaces.cdi.tinyorbit.vn/display-name": `Transfer ${stamp}` },
    },
    spec: { ...src.spec, revision: `xfer-${tplName}`, fileTransferPolicy: "Bidirectional" },
  };
  const f = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "tcdi-tpl-")), "tpl.json");
  fs.writeFileSync(f, JSON.stringify(clone));
  kubectlIn(TENANT_NAMESPACE, "apply", "-f", f);

  await login(page);
  const workspaceId = await createBrowserWorkspace(page, `xfer-${stamp}`, new RegExp(tplName, "i"));
  try {
    const connect = await waitForConnect(page);
    const launchRespP = page.waitForResponse(
      (r) => /\/v1\/launch/.test(r.url()) && r.request().method() === "POST",
      { timeout: 120_000 },
    );
    await connect.click();
    await page.waitForURL(`**/workspaces/${encodeURIComponent(workspaceId)}/session`);
    // The transfer's credentialed fetch rides this cookie.
    expectHostOnlySecure(await mintedSessionCookie(await launchRespP), "None", true);
    await expect(page.locator(".tc-session__status")).toHaveText("Connected", { timeout: 5 * 60_000 });

    const pod = kubectlIn(
      TENANT_NAMESPACE, "get", "pods", "-l",
      `workspaces.cdi.tinyorbit.vn/workspace-name=${workspaceId.replaceAll("_", "-").toLowerCase()}`,
      "-o", "jsonpath={.items[0].metadata.name}",
    );
    await transferRoundTrip(page, stamp, `${HOME_DIR}/transfers`, (script, ...args) =>
      kubectlIn(TENANT_NAMESPACE, "exec", pod, "-c", "desktop", "--", "sh", "-c", script, "_", ...args),
    );
  } finally {
    await page.goto(`/workspaces/${encodeURIComponent(workspaceId)}`);
    await page.getByRole("button", { name: "Stop", exact: true }).click();
    await waitPhase(page, workspaceId, "Stopped");
  }
});
```

`podEnv(workspaceId)` (`smoke.spec.ts:528`) already returns the desktop container's env map. `fs`, `os` and `path` are already imported in `smoke.spec.ts`. Check that both specs load (Playwright transpiles the TypeScript; the smoke package has no `tsc`) and list the new test:

```bash
cd hack/quickstart/smoke && npx playwright test --list > /tmp/smoke-list.log 2>&1; echo "rc=$?"; grep -c 'file transfer' /tmp/smoke-list.log
npx playwright test -c playwright.partitioned.config.ts --list > /tmp/part-list.log 2>&1; echo "rc=$?"; grep -c 'file transfer' /tmp/part-list.log
```

Expected: `rc=0` with `1`, then `rc=0` with `1`. The runs themselves happen in the CI `quickstart` and `partitioned` jobs.

- [ ] **Step 8: Runbook, docs and commit**

ADR 0010 T11 "Docs to touch" and who owns each: openapi.yaml (Tasks 7, 11a); `docs/images.md` runtime contract and `docs/kasm-images.md` (Task 12); threat-model rows S48 (Task 11a), S49 (Task 12), S50 (Task 13); the `connect-src` bullet (Step 4 above); the admin enable/disable and size-cap runbook (this step, nobody else owns it); `docs/releases/v1.3.0.md` and the ADR status line are replaced by the W3 release notes (`docs/releases/v1.0.0.md`) and the W3 docs pass (spec C2), not by W1.

`docs/runbooks/session-policy.md` (user-facing behaviour lives here too; the repo has no user guide, and `README.md` has no session section):
- the title becomes `# Runbook — Tenant session policy (clipboard and file-transfer ceilings)`;
- the first paragraph becomes: "A tenant's **session policy** is the ceiling over what data may move between a user's device and their workspace (ADR 0010). It has two channels: the **clipboard** and **file transfer**.";
- in "What the ceiling does", after the table add: "For file transfer the ceiling values mean: `Upload` is device → workspace, `Download` is workspace → device. The effective policy is the meet of the template's `spec.fileTransferPolicy` (default `Disabled`) and the ceiling, so `Upload ∩ Download = Disabled`, and a ceiling never widens a template.";
- in "Change the ceiling" → **API**, replace the sentence "`fileTransfer` accepts only its defaults in v1.2." with "`fileTransfer` is `{"mode": …, "maxFileBytes": …}` (see File transfer below).";
- insert this section before "## When does a change take effect?":

```markdown
## File transfer

File transfer moves one file at a time between the browser and the
**Transfers** folder of the workspace home (`~/transfers`). The portal
session page shows an upload control and a download control only for
the directions the workspace's resolved policy allows; with `Disabled`
nothing is shown.

**Enable or disable it.**

1. Allow it on the template: set `spec.fileTransferPolicy` to `Upload`,
   `Download` or `Bidirectional` on the `WorkspaceTemplate` (default
   `Disabled`; `GET /v1/templates` shows it as `fileTransferPolicy`).
2. Check the tenant ceiling. With no row, or `fileTransfer.mode:
   FollowTemplate`, the template's policy applies. To turn file transfer
   off for the whole tenant, open **Admin → Session policy → File
   transfer** and choose `Disabled` (or `PUT` `{"fileTransfer":{"mode":
   "Disabled","maxFileBytes":null}}`). The preview table on that page
   shows the effective policy per template before you save.

**Size cap.** The per-file limit is the smaller of two values:

- the platform bound, chart value `backend.fileTransfer.maxFileBytes`
  (backend flag `-file-transfer-max-bytes`, default 104857600 = 100 MiB,
  allowed 1 to 10737418240);
- the tenant's `fileTransfer.maxFileBytes` (1 up to the platform bound;
  `null` means "use the platform bound").

An ingress controller that caps request bodies must allow at least the
platform bound on the session route (chart README, "Exposure").

**When a change takes effect.** Mint and claim re-resolve the policy for
every transfer, so a narrower ceiling or a lower cap blocks the next
transfer immediately, including on running workspaces. The in-pod helper
enforces the policy and cap it booted with (`TCDI_XFER_POLICY`,
`TCDI_XFER_MAX_BYTES`), so a **wider** policy or a higher cap reaches a
running workspace at its next start.

**Verify.** `kubectl -n <tenant-ns> get workspace <name> -o
jsonpath='{.spec.sessionPolicy}'` shows `fileTransfer` and `maxFileBytes`;
the audit log (`/admin/audit`, actions `file.transfer.upload` and
`file.transfer.download`) records every mint, claim and completion with
the phase, path, byte count and outcome. A file moves only once: each grant is
single use and expires after 2 minutes.
```

Docs and commit:

```bash
cd web && npm run typecheck && npm run lint:strings && npm run test:unit && npm run build > /tmp/web-build.log 2>&1; echo "rc=$?"; tail -n 3 /tmp/web-build.log; cd ..
go test -count=1 ./build/frontend/
git add web build/frontend hack/quickstart/smoke docs/runbooks/session-policy.md docs/security/threat-model.md docs/adr/0005-backend-frontend-operator.md
git commit -m "feat(portal): upload and download files from the session page

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

Expected: `rc=0`.

---
## Wave exit

Run from the repository root on the integrated branch. Store tests need Docker for the `postgres:18.0` test container.

- [ ] **Step 1: Go**

```bash
LOG=/tmp/w1-go.log
{ go build ./... && go vet ./... && make test && go test -count=1 ./deploy/helm/; } > "$LOG" 2>&1; echo "rc=$?"; grep -nE '^(--- FAIL|FAIL|panic:)' "$LOG" | head -40
```

Expected: `rc=0`, no lines.

- [ ] **Step 2: envtest**

```bash
LOG=/tmp/w1-envtest.log
KUBEBUILDER_ASSETS=$(bin/setup-envtest use -p path) go test -tags=integration -race -count=1 \
  -skip '^TestLinuxRuntime(ReadinessAndHome|FirefoxFallback|Desktop|BrowserBaseline)$|^TestKasmAdapter' ./tests/integration/... \
  > "$LOG" 2>&1; echo "rc=$?"; grep -nE '^(--- FAIL|FAIL)' "$LOG" | head -20
```

This is the W0 wave-exit skip list: the skipped tests need real runtime images and run in CI. Expected: `rc=0`. Task 6's `RuntimeLost`/`ImagePolicyDenied` and Task 9's `WaitingForEndpoint` controller tests run here.

- [ ] **Step 3: Web**

```bash
cd web && LOG=/tmp/w1-web.log
{ npm run typecheck && npm run lint:strings && npm run test:unit && npm run build; } > "$LOG" 2>&1; echo "rc=$?"; grep -nE 'error|FAIL|✗' "$LOG" | head -30
npx playwright test > /tmp/w1-pw.log 2>&1; echo "rc=$?"; grep -nE '✘|failed' /tmp/w1-pw.log | head
npx playwright test --config playwright.portal.config.ts > /tmp/w1-pw-portal.log 2>&1; echo "rc=$?"; grep -nE '✘|failed' /tmp/w1-pw-portal.log | head
cd ..
```

Expected: `rc=0` three times.

- [ ] **Step 4: Contract scripts and smoke compile**

```bash
for t in .github/tests/*.test.sh; do bash "$t" > /tmp/w1-sh.log 2>&1 || { echo "FAIL $t"; tail -n 5 /tmp/w1-sh.log; }; done
(cd hack/quickstart/smoke && npx playwright test --list > /tmp/w1-smoke-list.log 2>&1; echo "rc=$?")
(cd hack/quickstart/smoke && npx playwright test -c playwright.cold-connect.config.ts --list > /tmp/w1-cold-list.log 2>&1; echo "rc=$?")
(cd hack/quickstart/smoke && npx playwright test -c playwright.partitioned.config.ts --list > /tmp/w1-part-list.log 2>&1; echo "rc=$?")
```

Expected: no `FAIL` lines; `rc=0` three times.

- [ ] **Step 5: CI**

Push the branch and open the PR (the orchestrator does this). The `quickstart` job must be green: it runs the kind smoke, including recovery (Task 6a), admin sessions/sign-out/users/audit (Task 4), overview (Task 5), apply-now (Task 8) and the file-transfer round-trip in both cookie modes (Task 14), plus the cold-connect gate (Task 10, p95 < 20 s over 20 runs). Wait on it with one background `gh run watch <run-id> --exit-status`, not a polling loop.

- [ ] **Step 6: Spec gate checklist (spec §5, W1)**

Tick each against the evidence above: F1 recovery on `Failed` (Tasks 6–6a, envtest + smoke); F2 cold connect p95 < 20 s (Tasks 9–10, CI gate); F3 apply-now restarts only looser running workspaces (Task 8); F4 upload/download e2e with policy recheck, caps and jail (Tasks 7, 11, 11a, 12–14, portal e2e in both cookie modes, kind smoke); F5 live sessions and sign out everywhere (Task 4, smoke); F6 durable fail-closed audit, viewer and retention (Tasks 1, 1a, 2); F7 overview cards (Task 5, smoke); F8 user directory (Task 3; listed in the Task 4 smoke); threat-model rows S42–S50 present; no new migration file (`git diff --name-only main -- internal/store/migrations | grep -v 0001_baseline` prints nothing).
