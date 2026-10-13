# TinyCDI v1.0.0 — W0 Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use sdd-orca:subagent-driven-development (recommended) or sdd-orca:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Lay the breaking foundation for the public v1.0.0. The work is a squashed baseline schema, the CRD at `workspaces.cdi.tinyorbit.vn/v1`, the `/v1` API redesigned in place (paging, ETag, error envelope, `:verb` actions) with the portal on it, chart 1.0.0 with a restricted ClusterOnly default, and the Chromium 155 / `kasmweb/chrome` catalog fix.

**Architecture:** Go monorepo. `internal/store` holds Postgres and the migrations. `api/v1alpha1` holds the CRD types, which become `api/v1`. `internal/api` holds the HTTP handlers on a stdlib `ServeMux`, mounted from `internal/backend/wire.go`. `internal/operator` and `internal/runtime/linux` are the controller and pod builder. `web/` is React+TS using `openapi-fetch` types generated from `internal/api/openapi.yaml`. `deploy/helm/tinycdi` is the chart, tested from `deploy/helm`. Nothing is upgraded in place. Every task may break compatibility with earlier builds, and no task adds a shim.

**Tech Stack:** Go 1.26, pgx v5, controller-runtime + controller-gen v0.22.0, envtest, Postgres 18 (tests start it with `docker run`), React + TypeScript + vitest + Playwright, Helm v4.1.3.

**Spec:** `docs/superpowers/specs/2026-10-13-tinycdi-v1-public-design.md` (§4 W0). Read it before any task.

## Global Constraints

- No upgrade path. Never add a compatibility alias, fallback branch, dual read or "legacy" handling for data or objects written by earlier builds.
- CRD group `workspaces.cdi.tinyorbit.vn`, version `v1` only (served + storage). Package path `github.com/tinyorbitvn/tinycdi/api/v1`.
- HTTP prefix stays `/v1`.
- List query parameters: `limit` (default 50, max 500), `cursor` (opaque), `sort` (allowlisted field, `-` prefix = descending). Response `{"items":[...],"nextCursor":"..."}`, with `nextCursor` omitted on the last page.
- Error body: `{"error":{"code":<ErrorCode>,"message":<string>,"retryable":<bool>,"requestId":<string>,"details":{...}?}}`.
- Mutable admin resources return a strong quoted `ETag`. PUT/PATCH/DELETE require `If-Match`. Missing → 428 `PRECONDITION_REQUIRED`. Stale or malformed → 412 `PRECONDITION_FAILED`. `If-Match: *` is accepted only when the resource does not exist yet, else 412.
- Custom methods use `:verb` paths: `POST /v1/workspaces/{id}:start`, `:stop`; `POST /v1/data/{id}:attach`, `:purge`.
- These security behaviors are unchanged: `__Host-` cookies only, `X-CSRF-Token` on mutations, PKCE login, rate limits, tenant isolation, audit events per route.
- Chart version `1.0.0`. The chart `crds/` directory must equal `config/crd/bases` byte for byte.
- Browser pin `CHROMIUM_APT_VERSION=155.0.8059.39-1~deb12u1`. Kasm floors stay: browser class ≥ pin major − 2, desktop class ≥ pin major − 4.
- Commit messages are conventional (`feat:`, `fix:`, `refactor:`, `test:`, `chore:`, `docs:`). Every commit ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Never push and never touch tags. Never edit `SECURITY.md` beyond the lines a task names.

## Review Focus

1. **A list request whose `cursor` was minted under a different `sort`, or tampered with.** Expected: 400 `INVALID_REQUEST`, never a silently wrong page. Pinned in Task 3, `TestCursor_RejectsSortMismatchAndTamper`.
2. **A connection request on a principal authenticated without a portal session.** Today `api/connections.go:178` issues a ticket with an empty session id, which writes a NULL digest. After W0 the column is `NOT NULL`, and the request must fail closed with 401 `UNAUTHENTICATED` and no ticket row. Pinned in Task 1, `TestConnectionCreate_RequiresPortalSession`.
3. **Concurrent admin edits.** Two admins GET the same quota, both PUT with the same ETag. Expected: exactly one 200 and one 412, and the stored value is the winner's. Pinned in Task 5, `TestAdminQuota_ConcurrentIfMatchOneWins`.
4. **A `:verb` the resource does not support** (`POST /v1/workspaces/{id}:explode`) and an id that itself contains a colon. Expected: 404 `NOT_FOUND` for an unknown verb, and an id with an unexpected colon must not reach a handler with a truncated id. Pinned in Task 3, `TestActionRouter_UnknownVerbAndColonInID`.
5. **A ClusterOnly workspace on a default install trying to reach a pod in an unmanaged namespace** (for example `kube-system` on a non-DNS port). Expected: denied by the rendered NetworkPolicy. Pinned in Task 7, `TestClusterOnlyEgress_DefaultRestricted`.

---

## File map

| Area | Files |
|---|---|
| Store | `internal/store/migrations/0001_baseline.sql` (new; 001–024 deleted), `internal/store/migrate.go`, `internal/store/testdata/schema.snapshot.sql` (new), `internal/store/baseline_test.go` (new), `internal/store/sessions.go` |
| Broker / API | `internal/broker/{tickets,leases,connection_state,activity}.go`, `internal/api/{auth,connections}.go`, `internal/backend/wire.go` |
| CRD | `api/v1/*` (moved from `api/v1alpha1/*`), `PROJECT`, `config/crd/bases/*`, `deploy/helm/tinycdi/crds/*`, `Makefile` (new `sync-chart-crds`), every importer |
| API primitives | `internal/api/errors.go`, `internal/api/etag.go` (new), `internal/api/page.go` (new), `internal/api/actions.go` (new), `internal/api/openapi.yaml` |
| API routes | `internal/api/{workspaces,events,templates,data,adminquota,adminuserlimit,adminsessionpolicy,auditroutes}.go`, `internal/store/{quota,sessionpolicy,userlimit}.go`, `internal/provisioning/service.go`, `internal/provisioning/retained.go` |
| Web | `web/src/api/{client,errors}.ts`, `web/src/api/generated/schema.d.ts`, `web/src/design/Pagination.tsx`, `web/src/app/me.tsx`, `web/src/admin/*`, `web/src/workspaces/*`, `web/src/data/*`, `web/src/templates/*`, `web/src/session/SessionPage.tsx`, `web/tests/mock-api/*` |
| Chart / config | `deploy/helm/tinycdi/{Chart.yaml,values.yaml,values.schema.json,templates/*,README.md}`, `deploy/helm/*_test.go`, `deploy/helm/testdata/*`, `cmd/operator/main.go`, `internal/runtime/linux/backend.go`, `internal/backend/config.go`, `.github/workflows/ci.yml`, `hack/quickstart/*` |
| Kasm | `build/browser/Dockerfile`, `build/kasm-catalog.txt`, `build/kasm-adapter/browser-shim.sh`, `tests/integration/kasm_adapter_test.go`, `tests/integration/testdata/browser-image-baseline.json`, docs touched by the existing branch |

---

### Task 1: Baseline schema, NOT NULL session binding, legacy touch removed

**Files:**
- Delete: `internal/store/migrations/001_*.sql` … `024_*.sql`
- Create: `internal/store/migrations/0001_baseline.sql`, `internal/store/testdata/schema.snapshot.sql`, `internal/store/baseline_test.go`
- Modify: `internal/store/migrations/README.md`, `internal/store/migrate.go`, `internal/store/sessions.go`, `internal/broker/tickets.go`, `internal/broker/leases.go`, `internal/broker/connection_state.go`, `internal/broker/activity.go`, `internal/api/connections.go`, `internal/api/auth.go`, `internal/backend/wire.go`
- Delete tests: `tests/integration/migration017_test.go`, `migration018_test.go`, `migration019_test.go`; the functions `TestMigration012_Idempotent` (`tests/integration/principal_directory_test.go`), `TestMigration011_Idempotent` (`internal/broker/sessions_test.go`), `TestMigration020_Idempotent` (`internal/broker/signout_test.go`), `TestMigration023_Idempotent` (`internal/broker/revokeall_test.go`), `TestMigration014_ApplyTwice` and `TestRetainStop_LegacyHeldRowConverges` (`internal/provisioning/retain_stop_quota_test.go`), `TestPGSessionStore_IDTokenLegacyNull` stays (NULL `id_token` is semantic, see below), `TestTouchPrincipal_*` (`tests/integration/session_touch_test.go`)

**Interfaces:**
- Consumes: nothing.
- Produces:
  - Schema with `connection_lease.portal_session_digest bytea NOT NULL` and `launch_ticket.portal_session_digest bytea NOT NULL`.
  - New table `audit_event` with the exact DDL below.
  - New table `session_file_transfer` with the exact DDL below.
  - `broker.IssueTicket(ctx, p, workspaceID string, takeover bool, policy provisioning.SessionPolicy, sessionID string)` returns `*api.Error` `UNAUTHENTICATED` when `sessionID == ""`.
  - The store interface loses `TouchPrincipal`.

Semantic NULLs stay nullable. Do NOT make these `NOT NULL`: `connection_lease.session_digest` (NULL until bound), `connection_lease.stream_owner_tab/stream_owner_epoch`, `sessions.id_token` (also NULL when no sealer is configured), `sessions.expires_at`, `quota_reservation.restart_*`, `tenant_session_policy.*` (NULL = FollowTemplate), `launch_ticket.clipboard_policy/session_policy` (NULL = zero policy).

- [ ] **Step 1: Produce the baseline from a fully migrated database**

On the current tree (before deleting anything), start Postgres 18 and apply all migrations, then dump the schema:

```bash
docker run -d --rm --name tcdi-base -e POSTGRES_PASSWORD=p -p 55432:5432 postgres:18.0
sleep 4
cat > /tmp/migrate_main.go <<'EOF'
package main
import ("context";"os";"github.com/tinyorbitvn/tinycdi/internal/store")
func main(){ db,err:=store.Open(context.Background(), os.Getenv("DSN")); if err!=nil{panic(err)}; if err:=db.Migrate(context.Background()); err!=nil{panic(err)} }
EOF
```

`store.Open` may have a different name. Check `internal/store/pg_test.go:126` `newDB` for the exact constructor and use it. Run it with `DSN=postgres://postgres:p@localhost:55432/postgres?sslmode=disable go run /tmp/migrate_main.go`. Then:

```bash
docker exec tcdi-base pg_dump -U postgres --schema-only --no-owner --no-privileges postgres > /tmp/full.sql
```

- [ ] **Step 2: Hand-clean into `0001_baseline.sql`**

Create `internal/store/migrations/0001_baseline.sql` from `/tmp/full.sql`:
- Keep every table, index, constraint, sequence and function the tables use.
- Drop `schema_migrations`, which the runner creates.
- Drop `SET` and `SELECT pg_catalog.set_config` lines and comments.
- Change both `portal_session_digest` columns to `NOT NULL`.
- Keep the 009 seed of `platform_meta` (`INSERT INTO platform_meta ... ON CONFLICT DO NOTHING`; copy the statement from the old `009_*.sql`).
- Make the whole file idempotent (`CREATE TABLE IF NOT EXISTS`, `CREATE INDEX IF NOT EXISTS`) as `migrations/README.md` requires.

Append the two new tables verbatim:

```sql
CREATE TABLE IF NOT EXISTS audit_event (
    id           bigserial PRIMARY KEY,
    tenant       text        NOT NULL,
    actor_ref    text        NOT NULL,
    action       text        NOT NULL,
    target_kind  text        NOT NULL DEFAULT '',
    target_id    text        NOT NULL DEFAULT '',
    outcome      text        NOT NULL CHECK (outcome IN ('success','denied','error')),
    error_code   text        NOT NULL DEFAULT '',
    request_id   text        NOT NULL DEFAULT '',
    details      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS audit_event_tenant_created_idx ON audit_event (tenant, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS audit_event_created_idx ON audit_event (created_at);

CREATE TABLE IF NOT EXISTS session_file_transfer (
    id                  text        PRIMARY KEY,
    workspace_id        text        NOT NULL,
    tenant              text        NOT NULL,
    principal_issuer    text        NOT NULL,
    principal_subject   text        NOT NULL,
    runtime_generation  bigint      NOT NULL,
    runtime_uid         text        NOT NULL,
    direction           text        NOT NULL CHECK (direction IN ('upload','download')),
    path                text        NOT NULL,
    declared_bytes      bigint      NOT NULL CHECK (declared_bytes >= 0),
    bytes_done          bigint      NOT NULL DEFAULT 0,
    status              text        NOT NULL DEFAULT 'pending'
                         CHECK (status IN ('pending','claimed','completed','failed','expired')),
    resolved_policy     text        NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    expires_at          timestamptz NOT NULL,
    claimed_at          timestamptz,
    completed_at        timestamptz
);
CREATE INDEX IF NOT EXISTS session_file_transfer_ws_idx ON session_file_transfer (workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS session_file_transfer_expiry_idx ON session_file_transfer (status, expires_at);
```

Then `git rm internal/store/migrations/0[0-2][0-9]_*.sql`, keeping `0001_baseline.sql`.

- [ ] **Step 3: Pad migration versions to four digits**

In `internal/store/migrate.go`, `migrationVersion` already parses the integer prefix, so `0001` → 1 needs no code change. Update the README rules: one baseline, new files `0002_*.sql` onward, forward-only, idempotent, no `CONCURRENTLY`. Delete the Go DDL in `(*DB).RotateSessionEpoch` (`internal/store/sessions.go` ~336-345) that re-creates 009 objects "for dumps taken before migration 009". Keep the function's real work.

- [ ] **Step 4: Write the failing baseline tests**

Create `internal/store/baseline_test.go` (package `store_test`, reuse `newDB(t)` from `pg_test.go`):

```go
func TestBaseline_SchemaMatchesSnapshot(t *testing.T) {
	db := newDB(t)
	got := dumpSchema(t, db) // pg_dump --schema-only via the test container, normalized
	want, err := os.ReadFile("testdata/schema.snapshot.sql")
	if err != nil { t.Fatal(err) }
	if string(want) != got {
		t.Fatalf("schema drifted from testdata/schema.snapshot.sql; regenerate with TCDI_UPDATE_SNAPSHOT=1\n%s", diffLines(string(want), got))
	}
}

func TestBaseline_MigrateIsIdempotent(t *testing.T) {
	db := newDB(t) // already migrated once
	if err := db.Migrate(context.Background()); err != nil { t.Fatal(err) }
	var n int
	if err := db.Pool().QueryRow(context.Background(), `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil { t.Fatal(err) }
	if n != 1 { t.Fatalf("schema_migrations rows = %d, want 1", n) }
}

func TestBaseline_PortalSessionDigestNotNull(t *testing.T) {
	db := newDB(t)
	for _, tbl := range []string{"connection_lease", "launch_ticket"} {
		var nullable string
		err := db.Pool().QueryRow(context.Background(),
			`SELECT is_nullable FROM information_schema.columns WHERE table_name=$1 AND column_name='portal_session_digest'`, tbl).Scan(&nullable)
		if err != nil { t.Fatal(err) }
		if nullable != "NO" { t.Fatalf("%s.portal_session_digest nullable=%s", tbl, nullable) }
	}
}
```

`dumpSchema` runs `docker exec <container> pg_dump --schema-only --no-owner --no-privileges <db>`. It strips `--` comment lines, `SET` lines and blank-line runs. When `TCDI_UPDATE_SNAPSHOT=1` it writes the snapshot instead of comparing. Use the container name and database name that `startPostgres`/`newDB` already expose; if they are unexported, add an exported test helper in `pg_test.go`. `db.Pool()` may be named differently, so use the accessor `newDB` callers already use.

- [ ] **Step 5: Run and see them fail, then generate the snapshot**

Run `go test ./internal/store/ -run TestBaseline -count=1`. Expected: `SchemaMatchesSnapshot` FAIL (missing file). Then run `TCDI_UPDATE_SNAPSHOT=1 go test ./internal/store/ -run TestBaseline_SchemaMatchesSnapshot -count=1` and re-run without the variable. Expected: all three PASS.

- [ ] **Step 6: Failing test — connections require a portal session**

In `internal/api/connections_test.go`, add:

```go
func TestConnectionCreate_RequiresPortalSession(t *testing.T) {
	h, issuer := newConnectionHandlerForTest(t) // use the file's existing constructor helper
	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws_1/connections", strings.NewReader(`{}`))
	req = req.WithContext(withPrincipalNoSession(req.Context())) // principal set, SessionFromContext → (nil,false)
	rec := httptest.NewRecorder()
	h.Create(rec, req)
	if rec.Code != http.StatusUnauthorized { t.Fatalf("status=%d want 401", rec.Code) }
	if issuer.calls != 0 { t.Fatalf("IssueTicket called %d times, want 0", issuer.calls) }
}
```

Adapt helper names to the ones already in `connections_test.go`. If no principal-only helper exists, write `withPrincipalNoSession` next to the test.

- [ ] **Step 7: Make it pass and delete the NULL-digest branches**

- `internal/api/connections.go` (~178): if `SessionFromContext` gives no session, call `writeError(w, r, CodeUnauthenticated, "portal session required")` and return before `IssueTicket`.
- `internal/broker/tickets.go`: `portalSessionDigest("")` must no longer return nil. `IssueTicket` returns `NewError(CodeUnauthenticated, ...)` on `sessionID == ""`. Remove the nil-digest insert path.
- `internal/broker/leases.go` (`loadLease` ~119 and the comment ~214-226): delete the "NULL digest counts as ok" exemption. A lease always has a digest and the session check always applies. `Lease.PortalSessionDigest` stays a non-empty string.
- `internal/broker/connection_state.go` (~103-107) and `activity.go` (~170-176): remove the `""` legacy branches.
- `internal/api/auth.go` (`newInputHook` ~934-985): the throttle key is always `"s:"+digest`. Delete the `"p:"+principal` key and the `TouchPrincipal` call.
- `internal/store/sessions.go`: delete `TouchPrincipalSQL`, `TouchPrincipalIdleSQL`, `TouchPrincipal`. Also delete `TouchPrincipal` from the interface in `auth.go` (~200-210), from `InMemorySessionStore`, and from the wiring in `internal/backend/wire.go` (~732, ~945).
- Update the four tests that build legacy leases with `IssueTicket(..., "")` (`internal/broker/signout_test.go:61`, `revokeall_test.go:111`, `connection_state_test.go:255`, `lease_session_test.go:268`). They now pass a seeded session id from `seedPortalSession` (`internal/broker/main_test.go:184`). Where a test asserted the legacy exemption, delete it.

- [ ] **Step 8: Delete migration-specific tests**

Delete the files and functions listed under **Files → Delete tests**. `TestMigrate_ConcurrentReplicas` (`internal/backend/migrate_lock_test.go`) stays: it still proves single application under concurrency.

- [ ] **Step 9: Run the affected packages**

```bash
go build ./... && go vet ./...
go test -count=1 ./internal/store/... ./internal/broker/... ./internal/api/... ./internal/backend/... ./internal/provisioning/...
```

Expected: PASS. Docker must be available; the tests skip without it, and that is not a pass. Also run `KUBEBUILDER_ASSETS=$(bin/setup-envtest use -p path) go test -tags=integration -count=1 -run 'Session|Principal|Retention|Lease' ./tests/integration/...`. Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add -A internal/store internal/broker internal/api internal/backend internal/provisioning tests/integration
git commit -m "feat(store)!: squash migrations into one baseline; portal session binding NOT NULL

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: CRD `workspaces.cdi.tinyorbit.vn/v1`

**Files:**
- Move: `api/v1alpha1/*` → `api/v1/*` (`git mv`); package name `v1`.
- Modify: `PROJECT` (version `v1`), `Makefile` (new target), every importer (63 files: `rg -l 'api/v1alpha1'`), the three Go tests containing the string `tinyorbit.vn/v1alpha1`, `config/samples/*`, `config/admissionpolicy/policy.yaml`, `deploy/helm/tinycdi/templates/{admissionpolicy,workspacetemplates}.yaml`, `deploy/helm/testdata/default-render.golden.yaml`, `web/src/templates/types.ts`, `hack/quickstart/sandbox-check.sh`, `internal/operator/workspace_controller.go`, `internal/runtime/linux/backend.go`, `internal/provisioning/k8sapplier.go`
- Regenerate: `api/v1/zz_generated.deepcopy.go`, `config/crd/bases/*`, `deploy/helm/tinycdi/crds/*`

**Interfaces:**
- Consumes: Task 1 (nothing CRD-specific).
- Produces:
  - `import cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"`, with `cdiv1.GroupVersion = schema.GroupVersion{Group: "workspaces.cdi.tinyorbit.vn", Version: "v1"}`.
  - `WorkspaceSpec.SessionPolicy SessionPolicySpec` (value, **required**, no pointer).
  - `SessionPolicySpec{Clipboard ClipboardPolicy; FileTransfer FileTransferPolicy}`.
  - `type FileTransferPolicy string` with the constants `FileTransferDisabled="Disabled"`, `FileTransferUpload="Upload"`, `FileTransferDownload="Download"`, `FileTransferBidirectional="Bidirectional"`.
  - `WorkspaceTemplateSpec.FileTransferPolicy FileTransferPolicy` (`+kubebuilder:default=Disabled`).
  - `TemplateSnapshot.SourceRef` is required (non-empty).

- [ ] **Step 1: Move the package and fix imports**

```bash
git mv api/v1alpha1 api/v1
sed -i 's/^package v1alpha1/package v1/' api/v1/*.go
rg -l 'tinycdi/api/v1alpha1' | xargs sed -i 's#tinycdi/api/v1alpha1#tinycdi/api/v1#g'
rg -l 'v1alpha1\.' -g '*.go' | xargs sed -i 's/\bv1alpha1\./cdiv1./g'
```

Then fix the import aliases by hand. Every file importing `.../api/v1` must alias it `cdiv1` to avoid colliding with `corev1`/`metav1`. In `api/v1/groupversion_info.go`, set `Version: "v1"` and the `+groupName=workspaces.cdi.tinyorbit.vn` marker. Add `// +kubebuilder:storageversion` on both root kinds. In `PROJECT`, change both resource `version:` fields to `v1`.

- [ ] **Step 2: Make SessionPolicy required and add file transfer**

In `api/v1/workspace_types.go`:

```go
// FileTransferPolicy is the session file-transfer direction policy.
// +kubebuilder:validation:Enum=Disabled;Upload;Download;Bidirectional
type FileTransferPolicy string

const (
	FileTransferDisabled      FileTransferPolicy = "Disabled"
	FileTransferUpload        FileTransferPolicy = "Upload"
	FileTransferDownload      FileTransferPolicy = "Download"
	FileTransferBidirectional FileTransferPolicy = "Bidirectional"
)

type SessionPolicySpec struct {
	// +required
	// +kubebuilder:validation:Enum=Disabled;Send;Receive;Bidirectional
	Clipboard ClipboardPolicy `json:"clipboard"`
	// +required
	FileTransfer FileTransferPolicy `json:"fileTransfer"`
}
```

and in `WorkspaceSpec`: `// +required` `SessionPolicy SessionPolicySpec \`json:"sessionPolicy"\``. In `workspacetemplate_types.go`, add `// +optional` `// +kubebuilder:default=Disabled` `FileTransferPolicy FileTransferPolicy \`json:"fileTransferPolicy,omitempty"\``.

Update `internal/provisioning/k8sapplier.go` (~201, 284, 361) so `sessionPolicySpec` returns a value, and set `FileTransfer` from the resolved policy. Until F4 wires the resolver, the provisioning `SessionPolicy` gains a field `FileTransfer cdiv1.FileTransferPolicy` resolved as `Disabled` everywhere.

- [ ] **Step 3: Delete the fallbacks**

- `internal/runtime/linux/backend.go` `buildPod` (~1039-1046): read `ws.Spec.SessionPolicy.Clipboard` only. Delete the `tpl.Spec.ClipboardPolicy` default.
- `internal/operator/workspace_controller.go` `snapshotStale` (~1205-1243) and `resolveTemplate` (~926-931): delete the `snap.SourceRef == ""` branch, the recorded-name fallback and the `LabelCatalogName` comparison. An empty `SourceRef` is now a validation error; add `+kubebuilder:validation:MinLength=1` on `TemplateSnapshot.SourceRef`. If `provisioning.ResolveTemplateByName` has no other caller, delete it and its test.
- Keep `podSpecBuiltEqual`'s `DeprecatedServiceAccount = ""`. It is core Kubernetes normalization.

- [ ] **Step 4: Add a CRD sync target and test**

In `Makefile`, after `manifests`:

```make
.PHONY: sync-chart-crds
sync-chart-crds: manifests ## Copy generated CRDs into the Helm chart.
	cp config/crd/bases/*.yaml deploy/helm/tinycdi/crds/
```

Add to `deploy/helm/chart_test.go`:

```go
func TestChartCRDsMatchGenerated(t *testing.T) {
	gen, _ := filepath.Glob("../../config/crd/bases/*.yaml")
	if len(gen) == 0 { t.Fatal("no generated CRDs") }
	for _, g := range gen {
		want, _ := os.ReadFile(g)
		got, err := os.ReadFile(filepath.Join("tinycdi/crds", filepath.Base(g)))
		if err != nil || !bytes.Equal(want, got) { t.Fatalf("%s differs from chart copy; run make sync-chart-crds", filepath.Base(g)) }
	}
}
```

- [ ] **Step 5: Write failing envtest coverage**

In `internal/operator` (next to the existing envtest suite), add `TestCRDv1_SessionPolicyRequiredAndFileTransferDefault`:
1. Create a `WorkspaceTemplate` without `fileTransferPolicy` and read it back. Expect `Disabled`.
2. Create a `Workspace` without `sessionPolicy`. Expect an admission (schema) error mentioning `sessionPolicy`.
3. Create a `Workspace` with `sessionPolicy.fileTransfer: Sideways`. Expect an enum error.
4. Assert `apiextensions` reports exactly one version, `v1`, with `served && storage`.

- [ ] **Step 6: Regenerate and run**

```bash
make generate manifests sync-chart-crds
go build ./... && go vet ./...
make test
go test -count=1 ./deploy/helm/
rg -n 'v1alpha1' --glob '!docs/**' --glob '!work/**' --glob '!**/node_modules/**' .
```

Expected: tests PASS. The last `rg` prints nothing. Docs are rewritten in W2 and are excluded here. Regenerate the golden file with the chart test's documented update flag when `TestDefaultRenderUnchangedGolden` fails only on the version string.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -m "feat(api)!: promote CRDs to workspaces.cdi.tinyorbit.vn/v1; required sessionPolicy with fileTransfer

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: API primitives — error envelope, ETag, cursor paging, `:verb` router

**Files:**
- Modify: `internal/api/errors.go`, `internal/api/errors_test.go`, `internal/api/openapi.yaml` (components only)
- Create: `internal/api/etag.go`, `internal/api/etag_test.go`, `internal/api/page.go`, `internal/api/page_test.go`, `internal/api/actions.go`, `internal/api/actions_test.go`

**Interfaces:**
- Consumes: none.
- Produces (used by Tasks 4–5):
  - `const CodePreconditionRequired ErrorCode = "PRECONDITION_REQUIRED"` → 428, not retryable.
  - `WriteError(w, r, *Error)` writes `{"error": <Error>}`.
  - `func ETagOf(version string) string` gives a strong quoted tag (`"` + base64url(sha256(version))[:22] + `"`).
  - `func CheckIfMatch(r *http.Request, currentVersion string, exists bool) *Error` returns nil when the request may proceed.
  - `type PageRequest struct{ Limit int; Sort SortSpec; After *Cursor }`.
  - `type SortSpec struct{ Field string; Desc bool }`.
  - `type Cursor struct{ Sort string; Key string; ID string }`.
  - `func ParsePage(r *http.Request, allowed []string, defaultSort string) (PageRequest, *Error)`.
  - `func (c Cursor) Encode() string`.
  - `func NextCursor(sort SortSpec, key, id string) string`.
  - `type Page[T any] struct{ Items []T \`json:"items"\`; NextCursor string \`json:"nextCursor,omitempty"\` }`.
  - `type ActionRouter struct{...}`, `func NewActionRouter() *ActionRouter`, `func (a *ActionRouter) Handle(verb string, h http.HandlerFunc)`, `func (a *ActionRouter) ServeHTTP(w, r)`.
  - `func ActionID(r *http.Request) string` gives the id part.
  - The router is mounted as `mux.Handle("POST /v1/workspaces/{ref}", ar)` and reads `r.PathValue("ref")`.

- [ ] **Step 1: Failing tests for the envelope and the new code**

In `internal/api/errors_test.go`, replace `TestErrorJSONShape` with:

```go
func TestErrorJSONShape_Envelope(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	WriteError(rec, req, NewError(CodeNotFound, "nope"))
	var body struct{ Error struct{ Code, Message, RequestID string; Retryable bool } `json:"error"` }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil { t.Fatal(err) }
	if body.Error.Code != "NOT_FOUND" || body.Error.Message != "nope" { t.Fatalf("%s", rec.Body.String()) }
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	if len(raw) != 1 { t.Fatalf("top-level keys = %d, want only \"error\"", len(raw)) }
}

func TestPreconditionRequiredStatus(t *testing.T) {
	if CodePreconditionRequired.HTTPStatus() != http.StatusPreconditionRequired { t.Fatal("428 expected") }
	if CodePreconditionRequired.Retryable() { t.Fatal("not retryable") }
}
```

Run `go test ./internal/api/ -run 'TestErrorJSONShape_Envelope|TestPreconditionRequiredStatus'`. Expected: FAIL.

- [ ] **Step 2: Implement the envelope**

In `errors.go`:
- Add `CodePreconditionRequired` to the const block, to `AllErrorCodes`, to `HTTPStatus()` (428) and to `Retryable()` (false).
- In `WriteError`, encode `struct{ Error *Error \`json:"error"\` }{e}` instead of `e`.

In `openapi.yaml`:
- Add `PRECONDITION_REQUIRED` to the `ErrorCode` enum (~1436).
- Rename the component `Error` (~1482) to `ErrorBody` with the same properties.
- Add a new component `Error: {type: object, required: [error], additionalProperties: false, properties: {error: {$ref: '#/components/schemas/ErrorBody'}}}`.

Run the two tests plus `TestErrorCodesMatchOpenAPI` and `TestOpenAPIContract_`. Expected: PASS.

- [ ] **Step 3: Failing ETag tests**

`internal/api/etag_test.go`:

```go
func TestETagOf_StrongQuotedStable(t *testing.T) {
	a := ETagOf("2026-10-13T10:00:00.123456789Z")
	if !strings.HasPrefix(a, `"`) || !strings.HasSuffix(a, `"`) || strings.HasPrefix(a, `W/`) { t.Fatalf("not strong quoted: %s", a) }
	if a != ETagOf("2026-10-13T10:00:00.123456789Z") || a == ETagOf("other") { t.Fatal("not stable/distinct") }
}

func TestCheckIfMatch_Matrix(t *testing.T) {
	cur := "v1"; tag := ETagOf(cur)
	cases := []struct{ name, hdr string; exists bool; want ErrorCode }{
		{"missing", "", true, CodePreconditionRequired},
		{"missing-new", "", false, CodePreconditionRequired},
		{"match", tag, true, ""},
		{"stale", ETagOf("v0"), true, CodePreconditionFailed},
		{"unquoted", strings.Trim(tag, `"`), true, CodePreconditionFailed},
		{"weak", "W/" + tag, true, CodePreconditionFailed},
		{"star-new", "*", false, ""},
		{"star-existing", "*", true, CodePreconditionFailed},
		{"list-with-match", `"zzz", ` + tag, true, ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPut, "/", nil)
		if c.hdr != "" { r.Header.Set("If-Match", c.hdr) }
		got := CheckIfMatch(r, cur, c.exists)
		if (got == nil) != (c.want == "") || (got != nil && got.Code != c.want) { t.Errorf("%s: got %v want %q", c.name, got, c.want) }
	}
}
```

- [ ] **Step 4: Implement `etag.go`**

```go
package api

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
)

// ETagOf derives the strong entity tag for a resource's opaque store version.
func ETagOf(version string) string {
	sum := sha256.Sum256([]byte(version))
	return `"` + base64.RawURLEncoding.EncodeToString(sum[:])[:22] + `"`
}

// CheckIfMatch enforces the If-Match precondition on a mutation (RFC 9110
// §13.1.1, strong comparison). exists reports whether the resource has a stored
// version; "*" is accepted only for a resource that does not exist yet.
func CheckIfMatch(r *http.Request, currentVersion string, exists bool) *Error {
	h := strings.TrimSpace(r.Header.Get("If-Match"))
	if h == "" {
		return NewError(CodePreconditionRequired, "If-Match header required")
	}
	if h == "*" {
		if exists {
			return NewError(CodePreconditionFailed, "resource already exists")
		}
		return nil
	}
	if !exists {
		return NewError(CodePreconditionFailed, "resource does not exist")
	}
	want := ETagOf(currentVersion)
	for _, tag := range strings.Split(h, ",") {
		if strings.TrimSpace(tag) == want {
			return nil
		}
	}
	return NewError(CodePreconditionFailed, "resource was modified")
}
```

Run `go test ./internal/api/ -run 'ETag|IfMatch'`. Expected: PASS.

- [ ] **Step 5: Failing paging tests**

`internal/api/page_test.go`:

```go
func TestParsePage_DefaultsAndBounds(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/workspaces", nil)
	p, e := ParsePage(r, []string{"createdAt", "name"}, "-createdAt")
	if e != nil || p.Limit != 50 || p.Sort.Field != "createdAt" || !p.Sort.Desc || p.After != nil { t.Fatalf("%+v %v", p, e) }
	for _, q := range []string{"limit=0", "limit=501", "limit=x", "sort=secret", "sort=--name"} {
		r := httptest.NewRequest("GET", "/v1/workspaces?"+q, nil)
		if _, e := ParsePage(r, []string{"createdAt", "name"}, "-createdAt"); e == nil || e.Code != CodeInvalidRequest { t.Errorf("%s: want INVALID_REQUEST", q) }
	}
	r = httptest.NewRequest("GET", "/v1/workspaces?limit=500&sort=name", nil)
	if p, e := ParsePage(r, []string{"createdAt", "name"}, "-createdAt"); e != nil || p.Limit != 500 || p.Sort.Desc { t.Fatalf("%+v %v", p, e) }
}

func TestCursor_RoundTrip(t *testing.T) {
	c := NextCursor(SortSpec{Field: "name", Desc: true}, "alpha|β", "ws_1")
	r := httptest.NewRequest("GET", "/v1/workspaces?sort=-name&cursor="+url.QueryEscape(c), nil)
	p, e := ParsePage(r, []string{"createdAt", "name"}, "-createdAt")
	if e != nil || p.After == nil || p.After.Key != "alpha|β" || p.After.ID != "ws_1" { t.Fatalf("%+v %v", p, e) }
}

func TestCursor_RejectsSortMismatchAndTamper(t *testing.T) {
	c := NextCursor(SortSpec{Field: "name"}, "a", "ws_1")
	for _, q := range []string{"sort=-name&cursor=" + c, "sort=createdAt&cursor=" + c, "sort=name&cursor=" + c[:len(c)-2], "sort=name&cursor=%21%21"} {
		r := httptest.NewRequest("GET", "/v1/workspaces?"+q, nil)
		if _, e := ParsePage(r, []string{"createdAt", "name"}, "-createdAt"); e == nil || e.Code != CodeInvalidRequest { t.Errorf("%s: want INVALID_REQUEST", q) }
	}
}
```

- [ ] **Step 6: Implement `page.go`**

```go
package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

const (
	DefaultPageLimit = 50
	MaxPageLimit     = 500
)

type SortSpec struct {
	Field string
	Desc  bool
}

func (s SortSpec) String() string {
	if s.Desc {
		return "-" + s.Field
	}
	return s.Field
}

// Cursor is the keyset position after the last returned item. Sort pins the
// ordering it was minted under; Key is the sort column's value rendered as a
// string by the list's store function; ID breaks ties.
type Cursor struct {
	Sort string `json:"s"`
	Key  string `json:"k"`
	ID   string `json:"i"`
}

func (c Cursor) Encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func NextCursor(sort SortSpec, key, id string) string {
	return Cursor{Sort: sort.String(), Key: key, ID: id}.Encode()
}

type PageRequest struct {
	Limit int
	Sort  SortSpec
	After *Cursor
}

type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"nextCursor,omitempty"`
}

func parseSort(v string, allowed []string) (SortSpec, bool) {
	s := SortSpec{Field: v}
	if strings.HasPrefix(v, "-") {
		s = SortSpec{Field: v[1:], Desc: true}
	}
	return s, slices.Contains(allowed, s.Field)
}

// ParsePage reads limit, sort and cursor. Every malformed value is a 400
// INVALID_REQUEST; a cursor minted under a different sort is malformed.
func ParsePage(r *http.Request, allowed []string, defaultSort string) (PageRequest, *Error) {
	q := r.URL.Query()
	p := PageRequest{Limit: DefaultPageLimit}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxPageLimit {
			return p, NewError(CodeInvalidRequest, "limit must be 1..500")
		}
		p.Limit = n
	}
	sv := q.Get("sort")
	if sv == "" {
		sv = defaultSort
	}
	s, ok := parseSort(sv, allowed)
	if !ok {
		return p, NewError(CodeInvalidRequest, "unsupported sort")
	}
	p.Sort = s
	if v := q.Get("cursor"); v != "" {
		raw, err := base64.RawURLEncoding.DecodeString(v)
		var c Cursor
		if err != nil || json.Unmarshal(raw, &c) != nil || c.ID == "" || c.Sort != s.String() {
			return p, NewError(CodeInvalidRequest, "bad cursor")
		}
		p.After = &c
	}
	return p, nil
}
```

Run `go test ./internal/api/ -run 'ParsePage|Cursor'`. Expected: PASS.

- [ ] **Step 7: Failing action-router tests**

`internal/api/actions_test.go`:

```go
func TestActionRouter_DispatchesVerb(t *testing.T) {
	ar := NewActionRouter()
	var gotID string
	ar.Handle("start", func(w http.ResponseWriter, r *http.Request) { gotID = ActionID(r); w.WriteHeader(202) })
	mux := http.NewServeMux()
	mux.Handle("POST /v1/workspaces/{ref}", ar)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/workspaces/ws_abc:start", nil))
	if rec.Code != 202 || gotID != "ws_abc" { t.Fatalf("code=%d id=%q", rec.Code, gotID) }
}

func TestActionRouter_UnknownVerbAndColonInID(t *testing.T) {
	ar := NewActionRouter()
	called := false
	ar.Handle("start", func(w http.ResponseWriter, r *http.Request) { called = true })
	mux := http.NewServeMux()
	mux.Handle("POST /v1/workspaces/{ref}", ar)
	for _, p := range []string{"/v1/workspaces/ws_abc:explode", "/v1/workspaces/ws_abc", "/v1/workspaces/:start", "/v1/workspaces/ws:abc:start"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", p, nil))
		var body struct{ Error struct{ Code string } `json:"error"` }
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != 404 || body.Error.Code != "NOT_FOUND" { t.Errorf("%s: code=%d body=%s", p, rec.Code, rec.Body.String()) }
	}
	if called { t.Fatal("handler must not run for malformed refs") }
}
```

- [ ] **Step 8: Implement `actions.go`**

```go
package api

import (
	"context"
	"net/http"
	"strings"
)

type actionIDKey struct{}

// ActionRouter dispatches POST {collection}/{id}:{verb} custom methods.
// ServeMux cannot match a wildcard that is only part of a segment, so the
// route registers {ref} and this splits it. An id must not contain ':'.
type ActionRouter struct{ verbs map[string]http.HandlerFunc }

func NewActionRouter() *ActionRouter { return &ActionRouter{verbs: map[string]http.HandlerFunc{}} }

func (a *ActionRouter) Handle(verb string, h http.HandlerFunc) { a.verbs[verb] = h }

func (a *ActionRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, verb, ok := strings.Cut(r.PathValue("ref"), ":")
	h := a.verbs[verb]
	if !ok || id == "" || strings.Contains(verb, ":") || h == nil {
		writeError(w, r, CodeNotFound, "not found")
		return
	}
	h(w, r.WithContext(context.WithValue(r.Context(), actionIDKey{}, id)))
}

// ActionID is the resource id of an ActionRouter request.
func ActionID(r *http.Request) string {
	id, _ := r.Context().Value(actionIDKey{}).(string)
	return id
}
```

Run `go test ./internal/api/ -run ActionRouter`. Expected: PASS.

- [ ] **Step 9: OpenAPI components for paging and ETag**

In `openapi.yaml`:
- Replace the `PageToken` parameter (~1324) with `Cursor` (`in: query, name: cursor, schema: {type: string}`).
- Set `PageLimit` to max 500 / default 50.
- Add the parameter `Sort` (`in: query, name: sort, schema: {type: string}`).
- Add the parameter `IfMatch` (`in: header, name: If-Match, required: true, schema: {type: string}`) and the header `ETag` (`schema: {type: string}`).
- Add the responses `PreconditionRequired` (428) and `PreconditionFailed` (412), both with the `Error` schema.

Run `go test ./internal/api/ -run 'OpenAPI|ErrorCodes'`. Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add internal/api
git commit -m "feat(api)!: error envelope, strong ETag/If-Match, keyset cursor paging and :verb action router

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Workspace, template, data and event routes on the conventions

**Files:**
- Modify: `internal/api/{workspaces,events,templates,data,auditroutes}.go` and their tests, `internal/api/openapi.yaml` (these paths), `internal/provisioning/service.go` (`ListWorkspaces`, cursor helpers), `internal/provisioning/retained.go` (`ListRetained`), `internal/backend/wire.go` (`catalogAdapter.List`, mounts), `internal/api/auditroutes_test.go` (`normalizePath`)

**Interfaces:**
- Consumes:
  - From Task 3: `ParsePage`, `PageRequest`, `Page[T]`, `NextCursor`, `NewActionRouter`, `ActionID`, `WriteError`.
- Produces (HTTP):
  - `GET /v1/workspaces?limit&cursor&sort` (sort ∈ `createdAt`,`name`; default `-createdAt`) returns `Page[Workspace]`.
  - `POST /v1/workspaces/{id}:start` and `:stop` return 202, same bodies as today.
  - `DELETE /v1/workspaces/{id}`, `GET /v1/workspaces/{id}`, `POST /v1/workspaces` are unchanged in shape.
  - `GET /v1/workspaces/{id}/events?limit&cursor` (sort fixed `-time`) returns `Page[WorkspaceEvent]`.
  - `GET /v1/templates?limit&cursor&sort` (sort ∈ `name`; default `name`) returns `Page[Template]`.
  - `GET /v1/data?limit&cursor&sort` (sort ∈ `retainedAt`,`name`; default `-retainedAt`) returns `Page[RetainedData]`.
  - `POST /v1/data/{id}:attach` and `:purge`.
  - The old `/start`, `/stop`, `/attach`, `/purge` subpaths return 404 (no alias).
- Produces (Go):
  - `provisioning.ListWorkspaces(ctx, scope, api.PageRequest-equivalent)` takes `provisioning.ListPage{Limit int; SortField string; Desc bool; AfterKey, AfterID string}` and returns `([]WorkspaceRecord, nextKey, nextID string, err)`.
  - The same shape for `ListRetained`.
  - Handlers convert with `NextCursor`.

- [ ] **Step 1: Failing HTTP tests**

In `internal/api/workspaces_test.go`, add:
- `TestWorkspacesList_PagingStableUnderInsert`: seed 7 workspaces, page with `limit=3&sort=name` collecting all names, inserting one new workspace whose name sorts first between page 1 and page 2. Assert the 7 seeded names each appear exactly once, in order, with no duplicate.
- `TestWorkspacesList_SortDescCreatedAt`: `sort=-createdAt` returns newest first.
- `TestWorkspaceActions_VerbPaths`: `POST /v1/workspaces/{id}:start` → 202. `POST /v1/workspaces/{id}/start` → 404. `POST /v1/workspaces/{id}:start` without the CSRF header → 403 `CSRF_FAILED` (security unchanged).

In `data_test.go`, add `TestDataActions_VerbPaths` with the same three assertions for `:attach`/`:purge`. In `templates_test.go`, add `TestTemplatesList_Paged` (limit=1 walks all templates via `nextCursor`). In `events_test.go`, add `TestEvents_Paged`.

Run `go test ./internal/api/ -run 'WorkspacesList|WorkspaceActions|DataActions|TemplatesList_Paged|Events_Paged'`. Expected: FAIL.

- [ ] **Step 2: Store keyset by sort field**

In `internal/provisioning/service.go`, replace `encodeCursor`/`decodeCursor` and the `pageToken` path in `ListWorkspaces` with `ListPage`:
- The ORDER BY is `(<col>, id)`, where `<col>` is `created_at` for `createdAt` and `name` for `name`. Use a fixed map; never interpolate client input.
- The keyset predicate is `(<col>, id) > ($k, $i)`, or `<` when descending.
- Fetch `LIMIT n+1`. `nextKey` is the last row's key rendered as a string (RFC3339Nano for timestamps), returned only when the extra row exists.
- Delete `ErrBadCursor` if it becomes unused.

Do the same in `internal/provisioning/retained.go` `ListRetained` with `retained_at`/`name`. Templates are in-memory: sort by name, then skip to `name > afterKey`.

- [ ] **Step 3: Handlers and routes**

- `WorkspaceHandler.List`: `p, e := ParsePage(r, []string{"createdAt","name"}, "-createdAt")`, map to `ListPage`, and respond `Page[Workspace]{Items, NextCursor}`. Delete `WorkspaceList`/`nextPageToken`.
- Same for `DataHandler.List`, `TemplateHandler.List` (delete `templateList`) and `WorkspaceHandler.Events` (fixed sort `-time`; allowed list `[]string{"time"}`, default `-time`).
- In the Mount functions, replace `"POST /v1/workspaces/{workspaceId}/start"` and `/stop` with one `ActionRouter` (`start`→`h.Start`, `stop`→`h.Stop`) mounted at `"POST /v1/workspaces/{ref}"`. The handlers read the id via `ActionID(r)` instead of `r.PathValue("workspaceId")`. Same for data `attach`/`purge` at `"POST /v1/data/{ref}"`.
- Audit: route constants in `auditroutes.go` (~47-67) become `"POST /v1/workspaces/{ref}:start"` etc., used only as audit labels. The ActionRouter path must emit the per-verb audit event. Wrap each verb handler with the same audit middleware the old route used. `normalizePath` in `auditroutes_test.go` must map `{ref}:start` to `{}:start` (keep the suffix).
- OpenAPI: rewrite these path items to the new paths, parameters (`Cursor`, `PageLimit`, `Sort`) and `Page`-shaped list schemas (`WorkspacePage`, `TemplatePage`, `RetainedDataPage`, `WorkspaceEventPage`). Delete the old subpaths.

- [ ] **Step 4: Run**

```bash
go test -count=1 ./internal/api/... ./internal/provisioning/... ./internal/backend/...
```

Expected: PASS, including `TestAuditCoverage_SpecMatchesTable` and `TestAuditCoverage_MountedMuxEmitsEventPerRoute`.

- [ ] **Step 5: Commit**

```bash
git add -A internal
git commit -m "feat(api)!: cursor-paged lists and :verb actions for workspaces, data, templates and events

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Admin routes on ETag/If-Match and paged user lists

**Files:**
- Modify: `internal/api/{adminquota,adminuserlimit,adminsessionpolicy,quota}.go` and their tests, `internal/store/{quota,sessionpolicy,userlimit}.go` (or the files where `SetLimits`, `SetCeiling` and the user-limit upserts live), `internal/api/openapi.yaml`

**Interfaces:**
- Consumes:
  - From Task 3: `ETagOf`, `CheckIfMatch`, `ParsePage`, `Page[T]`, `NextCursor`, `CodePreconditionRequired`.
- Produces (HTTP):
  - `GET /v1/admin/tenants/{t}/quota` returns the quota object without `users[]` and without `version`, plus an `ETag` header.
  - `PUT .../quota` requires `If-Match` and returns the new object + `ETag`.
  - `GET /v1/admin/tenants/{t}/user-limits?limit&cursor&sort` (sort ∈ `ownerRef`; default `ownerRef`) returns `Page[UserLimit]`, where each item carries its own `etag` field.
  - `PUT /v1/admin/tenants/{t}/user-limits/{ownerRef}` requires `If-Match` (`*` to create).
  - `DELETE /v1/admin/tenants/{t}/user-limits/{ownerRef}` requires `If-Match`.
  - `GET|PUT /v1/admin/tenants/{t}/user-limits/default` uses `ETag` / `If-Match`.
  - `GET|PUT /v1/admin/tenants/{t}/session-policy` uses `ETag` / `If-Match` (`*` when no row).
  - `GET /v1/quota` (self) keeps its shape minus the `users[]` array.
- Produces (Go):
  - The store mutators take `expectedVersion string` and `create bool`, and return `(newVersion string, err error)` with `ErrVersionMismatch` (one sentinel in `internal/store`, replacing `ErrQuotaVersionMismatch` and its session-policy twin).

- [ ] **Step 1: Failing tests**

In `adminquota_test.go`:
- `TestAdminQuota_IfMatchMatrix`: no header → 428 `PRECONDITION_REQUIRED`; stale ETag → 412; matching ETag → 200 with a new, different `ETag`; `*` on an existing row → 412; `*` on a fresh tenant → 200.
- `TestAdminQuota_ConcurrentIfMatchOneWins`: two goroutines PUT different limits with the same ETag behind a `sync.WaitGroup` barrier. Exactly one 200 and one 412; a GET returns the winner's limits.

In `adminuserlimit_test.go`:
- `TestUserLimits_PagedAndPerItemETag`: seed 5 overrides, walk with `limit=2`, every item has a non-empty quoted `etag`.
- `TestUserLimit_PutRequiresIfMatch`: 428 / 412 / 200 matrix on `PUT .../user-limits/{ownerRef}`, and `*` creates.
- `TestUserLimit_DeleteRequiresIfMatch`.

In `adminsessionpolicy_test.go`, add `TestSessionPolicy_IfMatchMatrix` (same matrix; an all-defaults PUT deletes the row, after which `*` is valid again).

Each test also asserts the existing security behavior: missing CSRF → 403; a non-admin principal → 403; an admin of another tenant → 403.

Run `go test ./internal/api/ -run 'AdminQuota|UserLimit|SessionPolicy'`. Expected: FAIL.

- [ ] **Step 2: Store**

- One sentinel `store.ErrVersionMismatch`.
- `SetLimits`, `SetCeiling` and the user-limit set/delete/default functions take `(expectedVersion string, create bool)`. They do the conditional `UPDATE ... WHERE updated_at = $v` when `!create`, or `INSERT ... ON CONFLICT DO NOTHING` when `create`. Zero rows affected → `ErrVersionMismatch`. They return the new `updated_at` as RFC3339Nano.
- Add an `updated_at timestamptz NOT NULL DEFAULT now()` column to `user_session_limit` and `tenant_user_limit_default` in `0001_baseline.sql` if missing, then regenerate the snapshot (`TCDI_UPDATE_SNAPSHOT=1`). This edits the baseline, which is allowed before 1.0.

- [ ] **Step 3: Handlers**

- Every GET of a mutable admin resource sets `w.Header().Set("ETag", ETagOf(version))` and drops `version` from the body.
- Every mutation calls `if e := CheckIfMatch(r, curVersion, exists); e != nil { WriteError(w, r, e); return }` after loading the current row inside the same request.
- The store still enforces the version in the UPDATE, so a race after the check also yields 412.
- Map `store.ErrVersionMismatch` → 412.
- Split the user-limits list into the paged collection; delete the embedded `users[]` from quota and user-limits responses.
- Update `openapi.yaml`: `IfMatch` parameter + `ETag` response header + 412/428 responses on each operation, and the new user-limit item paths.

- [ ] **Step 4: Contract coverage for every operation**

This step runs now because all of the W0 routes exist at this point. In `internal/api/openapi_contract_test.go`, add `TestOpenAPI_EveryOperationMountedAndViceVersa`:
- Build the real mux via the same `Mount*Routes` calls `wire.go` uses, with fakes.
- Enumerate the spec operations (`specOps(t)` from `auditroutes_test.go`).
- Assert both directions:
  - every spec operation resolves on the mux to a non-404 handler (send the method and path with dummy ids and a `:verb` suffix where present; any status but the mux's own 404/405 counts);
  - every route pattern registered by the Mount functions appears in the spec.

Then add `TestOpenAPIContract_ResponsesValidate`. For each operation with a fake-backed happy path (all list operations, one GET per resource, one mutation per admin resource), call the handler. Then validate the JSON body against the operation's response schema with the existing `santhosh-tekuri/jsonschema` compiler. Also validate one error response per operation (a 404 or 412) against `Error`.

- [ ] **Step 5: Run**

```bash
go test -count=1 ./internal/api/... ./internal/store/...
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add -A internal
git commit -m "feat(api)!: strong ETag/If-Match on quota, user-limits and session-policy; paged user limits

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: Portal on the new API

**Files:**
- Modify: `web/src/api/generated/schema.d.ts` (regenerate), `web/src/api/client.ts`, `web/src/api/errors.ts`, `web/src/app/me.tsx`, `web/src/workspaces/api.ts`, `web/src/templates/useTemplates.ts`, `web/src/admin/api.ts`, `web/src/admin/*Page.tsx`, `web/src/data/DataListPage.tsx`, `web/src/session/SessionPage.tsx`, `web/src/workspaces/WorkspaceDetailPage.tsx`, `web/src/design/Pagination.tsx`, `web/tests/mock-api/*`, and the affected `*.test.ts(x)` and Playwright specs

**Interfaces:**
- Consumes: the OpenAPI from Tasks 3–5.
- Produces:
  - `useServerPage<T>(fetchPage: (cursor?: string, sort?: string) => Promise<{items: T[]; nextCursor?: string}>, opts: {sort: string})` returns `{items, loading, error, hasNext, next(), reset(sort)}`. This hook in `web/src/design/useServerPage.ts` replaces `usePagination`+`listAll` for collections.
  - `unwrap` reads `body.error.code`.
  - Admin writers send `If-Match` from the stored `ETag` response header.

- [ ] **Step 1: Regenerate types and fix the compile**

```bash
cd web && npm ci && npm run generate && npm run typecheck
```

Expected: typecheck FAIL, listing every call site that uses the old paths or shapes. That list is the work list.

- [ ] **Step 2: Failing unit tests**

- `web/src/api/errors.test.ts`: `unwrap` on `{error:{code:"PRECONDITION_REQUIRED",...}}` throws a `PortalApiError` with code `PRECONDITION_REQUIRED`. `KNOWN_CODES` includes it (compare against the generated enum, not a hand list).
- `web/src/api/client.test.ts`: the CSRF retry middleware triggers on `{error:{code:"CSRF_FAILED"}}`.
- `web/src/design/useServerPage.test.tsx`: `next()` appends the second page using `nextCursor`; `reset("-name")` clears items and refetches with the new sort; no `nextCursor` gives `hasNext=false`.

Run `npm run test:unit`. Expected: FAIL.

- [ ] **Step 3: Implement**

- `errors.ts`: derive `KNOWN_CODES` from the generated `components["schemas"]["ErrorCode"]` union and read `body.error`.
- `client.ts` `unwrap` and the CSRF middleware read `body.error.code`.
- Create `useServerPage.ts`.
- Replace `listAll`/`MAX_PAGES` loops (`app/me.tsx`, `workspaces/api.ts`, `templates/useTemplates.ts`) with paged fetches.
- Table pages (`admin/{Templates,UsersAdmin,WorkspacesAdmin,DataAdmin}Page.tsx`, `data/DataListPage.tsx`): `useTableSort` drives the server `sort` param, and a "Load more" control replaces the truncated notice. Delete the "4000" comment and `listAll`.
- Action calls become `api.POST("/v1/workspaces/{workspaceId}:start", ...)` (or whatever the generated path type is); `SessionPage.tsx:751` and `WorkspaceDetailPage.tsx:208` use the shared `startWorkspace`.
- Admin writers read `res.response.headers.get("ETag")` and send it as `If-Match`. On 412, show the existing "changed by someone else, reload" copy (add en+vi strings if missing).
- Update `web/tests/mock-api` to serve the new shapes.

- [ ] **Step 4: Run the web suite**

```bash
cd web && npm run typecheck && npm run lint:strings && npm run test:unit && npm run build && npx playwright install chromium && npm run test:e2e
```

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add -A web
git commit -m "feat(web)!: portal on the redesigned /v1 API (server paging, error envelope, ETag writes)

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: Chart 1.0.0, restricted ClusterOnly default, legacy config removed, upgrade job removed

**Files:**
- Modify: `deploy/helm/tinycdi/Chart.yaml` (`version: 1.0.0`, `appVersion: "1.0.0"`), `values.yaml` (~684, ~740-759), `values.schema.json` (~514-520), `templates/operator.yaml` (~104-114), `templates/NOTES.txt` (~99-120), `templates/workspacetemplates.yaml` (~84-85), `templates/_helpers.tpl` (~432), `deploy/helm/tinycdi/README.md` (upgrade sections, ClusterOnly section, value tables), `deploy/helm/chart_clusteronly_egress_test.go`, `deploy/helm/chart_test.go` (~280, ~441-443), `deploy/helm/chart_extraenv_test.go` (~38, ~96), `deploy/helm/testdata/legacy-nodeselector-values.yaml` (delete), `deploy/helm/testdata/default-render.golden.yaml`, `cmd/operator/main.go` (~107, ~428, ~575), `internal/runtime/linux/backend.go` (`clusterOnlyEgressRule` ~1576), `internal/runtime/linux/backend_test.go` (~256-289), `internal/backend/config.go` (~15, ~388-400), `.github/workflows/ci.yml` (`upgrade` job ~578+ and its path gate ~603), `hack/quickstart/upgrade-test.sh` (delete), `hack/quickstart/smoke/upgrade.spec.ts` (delete)

**Interfaces:**
- Consumes: Task 2 (CRD v1 names in templates).
- Produces:
  - Operator flag `--runtime-clusteronly-egress`. Absent → restricted default; value `unrestricted` → any-pod egress; the existing selector JSON form → narrowed as today.
  - Chart value `runtime.networkProfiles.clusterOnly.unrestricted` (bool, default false).

- [ ] **Step 1: Failing tests**

In `internal/runtime/linux/backend_test.go`:

```go
func TestClusterOnlyEgress_DefaultRestricted(t *testing.T) {
	rule := clusterOnlyEgressRule(Options{})
	if len(rule.To) != 1 || rule.To[0].NamespaceSelector == nil { t.Fatalf("%+v", rule) }
	me := rule.To[0].NamespaceSelector.MatchExpressions
	if len(me) != 1 || me[0].Key != "workspaces.cdi.tinyorbit.vn/tenant" || me[0].Operator != metav1.LabelSelectorOpExists { t.Fatalf("%+v", me) }
	if rule.To[0].PodSelector != nil || len(rule.Ports) != 0 { t.Fatalf("%+v", rule) }
}

func TestClusterOnlyEgress_UnrestrictedOptIn(t *testing.T) {
	rule := clusterOnlyEgressRule(Options{ClusterOnlyEgress: &ClusterOnlyEgress{Unrestricted: true}})
	if len(rule.To) != 2 { t.Fatalf("%+v", rule) } // any pod, any namespace (pod peers only)
}
```

In `deploy/helm/chart_clusteronly_egress_test.go`, replace `TestClusterOnlyEgressDeprecationNotice` and `...DefaultRendersNoFlag` with:
- `TestClusterOnlyEgress_DefaultRendersNoFlagAndNoWarning`: the default render has no `--runtime-clusteronly-egress` and the NOTES output has no "ClusterOnly egress: WARNING".
- `TestClusterOnlyEgress_UnrestrictedRendersFlag`: `unrestricted: true` renders `--runtime-clusteronly-egress=unrestricted` and NOTES prints one line stating the opt-out.

In `deploy/helm/chart_test.go`, add `TestChartVersion1`: `Chart.yaml` `version` and `appVersion` are `1.0.0`.

Run `go test ./internal/runtime/linux/ -run ClusterOnly` and `go test ./deploy/helm/ -run 'ClusterOnly|ChartVersion1'`. Expected: FAIL.

- [ ] **Step 2: Operator and backend**

- `backend.go`:
  - `ClusterOnlyEgress` gains `Unrestricted bool`.
  - `clusterOnlyEgressRule`: `nil` → one peer `{NamespaceSelector: {MatchExpressions: [{Key: "workspaces.cdi.tinyorbit.vn/tenant", Operator: Exists}]}}`.
  - `Unrestricted` → the two historical peers.
  - Otherwise today's narrowing.
  - Rewrite the doc comment (no "historical posture" wording).
- `cmd/operator/main.go` `parseClusterOnlyEgress`: the literal `unrestricted` → `&ClusterOnlyEgress{Unrestricted: true}`.
- `internal/backend/config.go`:
  - delete the `TCDI_LOGIN_KEY_FILES` fallback (~388-389) and the `TCDI_GW_CONTROL_TOKEN` fallback (~397-400);
  - delete the package-doc mention (~15);
  - update `config_test.go` so the old names are ignored: set only `TCDI_GW_CONTROL_TOKEN`, expect the control token empty or a startup error per the current required-ness.

- [ ] **Step 3: Chart**

- `values.yaml`: add `unrestricted: false` under `runtime.networkProfiles.clusterOnly`. Rewrite the comment block (~735-750) to describe the restricted default and the opt-out, with no "DEPRECATED"/"planned v1.3" wording. Fix the stale node-selector comment (~684).
- `values.schema.json`: add `unrestricted: {type: boolean}`.
- `templates/operator.yaml`: render `--runtime-clusteronly-egress=unrestricted` when `unrestricted`. Rendering both `unrestricted` and selectors fails with `fail "clusterOnly.unrestricted cannot be combined with egress selectors"`.
- `NOTES.txt` (~99-120): replace the warning block with a one-line note printed only when `unrestricted` is true.
- `workspacetemplates.yaml` (~84-85): delete the legacy `nodeSelector` hard-fail. `values.schema.json` must reject unknown template keys already; verify with `helm template` that `templates[0].nodeSelector` fails schema validation, else add `additionalProperties: false` to the template entry schema.
- Delete `TestLegacyNodeSelectorRejected` and its fixture.
- `_helpers.tpl` (~432) and `chart_extraenv_test.go`: remove `TCDI_GW_CONTROL_TOKEN` from the extraEnv deny-list only if the list exists to block the legacy name. If the list blocks it because it is a secret-bearing name, keep it. Read the comment at `_helpers.tpl` and decide by that.
- `Chart.yaml` → `1.0.0`.
- README: delete the "Upgrading to 0.3.0" section (~90) and every other "Upgrading"/version-history section. Rewrite the ClusterOnly section. Add a "Removed values" table with the rows `templates[].nodeSelector` and the implicit unrestricted ClusterOnly default.
- Regenerate the golden render via the chart test's update flag.

- [ ] **Step 4: Remove the upgrade path**

- `.github/workflows/ci.yml`: delete the `upgrade` job (~578 to its end) and any `needs:` references to it.
- `git rm hack/quickstart/upgrade-test.sh hack/quickstart/smoke/upgrade.spec.ts`.
- Remove any `shellcheck` invocation that lists `upgrade-test.sh` (~618).
- Run `.github/tests/*.test.sh` locally (the workflow-policy job) to confirm no test references the removed job: `for f in .github/tests/*.test.sh; do bash "$f" || echo "FAIL $f"; done`.

- [ ] **Step 5: Run**

```bash
go test -count=1 ./internal/runtime/... ./cmd/... ./internal/backend/... ./deploy/helm/
helm lint --strict deploy/helm/tinycdi
for f in .github/tests/*.test.sh; do bash "$f" >/dev/null 2>&1 || echo "FAIL $f"; done
```

Expected: PASS, `helm lint` clean, no `FAIL` lines.

- [ ] **Step 6: Commit**

```bash
git add -A deploy cmd internal .github hack
git commit -m "feat(chart)!: chart 1.0.0 with restricted ClusterOnly egress by default; drop legacy env and upgrade job

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: Chromium 155 pin and kasm catalog on `kasmweb/chrome`

**Files:**
- Bring in commits `6ec1564` and `97a6a88` from the local branch `fix/chromium-155-pin` (`git cherry-pick 6ec1564 97a6a88`). In `97a6a88`, remove the "WIP" wording from the commit message via `git commit --amend` after the cherry-pick.
- Modify: `build/kasm-adapter/browser-shim.sh`, `tests/integration/kasm_adapter_test.go`, `tests/integration/testdata/browser-image-baseline.json`, `tests/integration/linux_desktop_test.go` (~406-450 comment only).
- The cherry-picked commits also touch `docs/releases/v1.2.0.md`. Drop that hunk (`git checkout HEAD~2 -- docs/releases/v1.2.0.md` after the picks, or edit it out during the pick). W2 deletes that file anyway.

**Interfaces:**
- Consumes: none.
- Produces:
  - The catalog holds exactly `kasmweb/chrome@sha256:4999d22efdde2ae7e432c6eb969685e578498c83491e6ef8a0853adc1324003b  chromium  154  browser`.
  - The adapter supports both the Debian chromium wrapper layout and the Google Chrome layout.

- [ ] **Step 1: Cherry-pick and confirm the static gates**

```bash
git cherry-pick 6ec1564 97a6a88
.github/scripts/check-kasm-catalog.sh && bash .github/tests/kasm-catalog.test.sh && go test -count=1 ./deploy/helm/
```

Expected: PASS. If `kasm-catalog.test.sh` has a different name, use `ls .github/tests | grep -i kasm`.

- [ ] **Step 2: Run the contract test against chrome and see it fail**

```bash
docker build -f build/kasm-adapter/Dockerfile -t tcdi/kasm-adapter:it .
TCDI_IT_KASM_IMAGE=kasmweb/chrome@sha256:4999d22efdde2ae7e432c6eb969685e578498c83491e6ef8a0853adc1324003b \
  KUBEBUILDER_ASSETS=$(bin/setup-envtest use -p path) \
  go test -tags=integration -count=1 -run TestKasmAdapterChromium ./tests/integration/ 2>&1 | tee /tmp/kasm-it.log | tail -40
```

Expected: FAIL in 7 of 8 subtests on `/usr/bin/chromium-orig: No such file or directory`.

- [ ] **Step 3: Make the test layout-aware**

In `tests/integration/kasm_adapter_test.go`, add a layout probe run once per image. Before starting the session, `docker run --rm --entrypoint sh <image> -c 'ls /usr/bin/chromium-orig /opt/google/chrome/google-chrome 2>/dev/null'` picks:

```go
type kasmBrowserLayout struct {
	SessionCmd     string // binary the session command launches
	RendererPgrep  string // pgrep -f pattern matching sandboxed renderers
	ProcessPattern string // pgrep pattern for the browser main process
	PkillPattern   string // pkill -f pattern that ends the browser
	PolicyDir      string // managed policy directory checked by ReadinessEndpointAndAuth
	RelaunchBin    string // path RelaunchPathsStaySandboxed invokes directly
}

var (
	layoutDebianChromium = kasmBrowserLayout{"/usr/bin/chromium-orig", "chromium.*--type=renderer", "chromiu[m]", "/usr/lib/chromium/chromiu[m]", "/etc/chromium", "/usr/bin/chromium"}
	layoutGoogleChrome   = kasmBrowserLayout{"/opt/google/chrome/google-chrome", "chrome.*--type=renderer", "chrom[e]", "/opt/google/chrome/chrom[e]", "/etc/opt/chrome", "/usr/bin/google-chrome"}
)
```

Replace the hard-coded literals at `kasmSessionCmd` (~46), `pgrep 'chromium.*--type=renderer'` (~301), `pgrep 'chromiu[m].*'+dir` (~626), `pkill '/usr/lib/chromium/chromiu[m]'` (~652) and `/etc/chromium` (~468) with the layout's fields. The assertions themselves (sandboxed renderers, no sandbox-killing flags, relaunch paths stay sandboxed, the policy file is present, session ends with the payload) are unchanged and must hold for both layouts.

- [ ] **Step 4: Make the shim and the policy targets cover chrome**

`build/kasm-adapter/browser-shim.sh` (~33-36) already lists `/opt/google/chrome/google-chrome`. Confirm that `AdapterBrowserWrappers` (`internal/runtime/linux/backend.go` ~181-199) includes `/usr/bin/google-chrome` and `/usr/bin/google-chrome-stable`, and that `AdapterPolicyTargets` (~204-207) includes `/etc/opt/chrome/policies/managed`. Add any missing entries, and extend the backend unit test that lists these paths.

- [ ] **Step 5: Re-baseline the browser image size**

Build the browser image locally (`docker build -t tcdi/linux-base:local build/linux-base`, then `build/browser` with the base arg the agent used) and read its size: `docker image inspect -f '{{.Size}}' <img>`. In `tests/integration/testdata/browser-image-baseline.json`, set `sizeBytes` to that value, set `pkgCount` to the measured package count, and keep the 2 % rule. Update `_comment` to: "Baseline refreshed for chromium 155.0.8059.39-1~deb12u1 (Debian security update); the 2 % limit absorbs patch-level growth only."

- [ ] **Step 6: Run the contract suite on every catalog entry, then the baseline**

```bash
while read -r ref _; do case "$ref" in ''|'#'*) continue;; esac
  TCDI_IT_KASM_IMAGE="$ref" KUBEBUILDER_ASSETS=$(bin/setup-envtest use -p path) \
    go test -tags=integration -count=1 -run TestKasmAdapterChromium ./tests/integration/ || echo "FAIL $ref"
done < build/kasm-catalog.txt
KUBEBUILDER_ASSETS=$(bin/setup-envtest use -p path) go test -tags=integration -count=1 -run TestLinuxRuntimeBrowserBaseline ./tests/integration/
PATH="$PWD/bin:$PATH" .github/scripts/scan-kasm-catalog.sh
```

Expected: no `FAIL` line, the baseline PASS, the scan PASS (trivy 0 HIGH/CRITICAL fixable).

- [ ] **Step 7: Commit**

```bash
git add -A build tests internal
git commit -m "fix(kasm): adapter contract covers the Google Chrome layout; re-baseline browser image for chromium 155

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

## Wave exit (whole-branch verification)

```bash
go build ./... && go vet ./...
make test
go test -count=1 ./deploy/helm/
KUBEBUILDER_ASSETS=$(bin/setup-envtest use -p path) go test -tags=integration -race -count=1 \
  -skip '^TestLinuxRuntime(ReadinessAndHome|FirefoxFallback|Desktop|BrowserBaseline)$|^TestKasmAdapter' ./tests/integration/...
(cd web && npm ci && npm run typecheck && npm run lint:strings && npm run test:unit && npm run build)
for f in .github/tests/*.test.sh; do bash "$f" >/dev/null 2>&1 || echo "FAIL $f"; done
rg -n 'v1alpha1|pageToken|nextPageToken|TCDI_GW_CONTROL_TOKEN|TCDI_LOGIN_KEY_FILES|TouchPrincipal' \
  --glob '!docs/**' --glob '!work/**' --glob '!**/node_modules/**' --glob '!docs/superpowers/**' .
```

Expected: everything PASS, and the final `rg` prints nothing. The fresh-install kind e2e (`hack/quickstart/up.sh` + portal smoke) runs in CI's `quickstart` job when the branch's PR opens. W0 is done only when that job is green.
