# ADR 0002 — API conventions

Status: Accepted

## Context

TinyCDI has one public HTTP API, served under `/v1` on the portal origin and
described by [`internal/api/openapi.yaml`](../../internal/api/openapi.yaml)
(OpenAPI 3.1). The portal (`web/`) is its only client; the portal's types
are generated from the document (`npm run generate`, `web/package.json:13`).
Handler output is checked against the document's schemas
(`TestOpenAPIContract_ResponsesValidate`,
`internal/api/openapi_contract_test.go`), and the
error-code enum against the Go constants by `TestErrorCodesMatchOpenAPI`
(`internal/api/errors_test.go`).

This ADR records the conventions every operation follows, so a new
operation looks like the existing ones and a client can rely on them.

## Decision

### Resources and custom methods

- Resource paths are nouns under `/v1`. An action that is not create, read,
  update or delete is a custom method: `POST` with a `:verb` last segment.
  The API has eight (`grep -nE '^  /v1/[^ ]*:[a-z-]+:$' internal/api/openapi.yaml`):
  - `POST /v1/session:touch`
  - `POST /v1/me/sessions:revoke-all`
  - `POST /v1/workspaces/{workspaceId}:start`
  - `POST /v1/workspaces/{workspaceId}:stop`
  - `POST /v1/admin/tenants/{tenant}/session-policy:apply`
  - `POST /v1/admin/tenants/{tenant}/users/{ownerRef}/sessions:revoke-all`
  - `POST /v1/data/{dataId}:attach`
  - `POST /v1/data/{dataId}:purge`
- Dispatch: a method on a collection member is registered once, as
  `POST /v1/workspaces/{ref}` or `POST /v1/data/{ref}`
  (`internal/api/auditroutes.go`). `api.ActionRouter`
  (`internal/api/actions.go`) splits `{ref}` at the `:` and calls the
  verb's handler; an unknown verb, an empty id or a second `:` is
  `404 NOT_FOUND`. An id never contains `:`. A method whose last segment is
  fixed (`session:touch`, `sessions:revoke-all`, `session-policy:apply`) is
  a plain `ServeMux` route.
- Lifecycle requests are recorded intents. The API writes the intent to the
  transactional outbox (`provisioning.AppendIntent`,
  `internal/provisioning/outbox.go`) and the operator reconciles the
  `Workspace` custom resource. `POST /v1/workspaces` answers `201`;
  `:start`, `:stop` and `DELETE /v1/workspaces/{workspaceId}` answer `202`
  (`internal/api/workspaces.go`). End users never write custom
  resources (`openapi.yaml`).

### Lists

- A list takes three query parameters (`ParsePage`,
  `internal/api/page.go`):
  - `limit`: default 50, maximum 500 (`DefaultPageLimit`, `MaxPageLimit`,
    `page.go`);
  - `sort`: one allowlisted field, with a `-` prefix for descending
    (`parseSort`, `page.go`);
  - `cursor`: an opaque keyset position, pinned to the sort it was minted
    under (`Cursor`, `page.go`).

  A malformed value, or a cursor minted under another sort, is
  `400 INVALID_REQUEST`.
- The response is `{"items": [...], "nextCursor": "..."}`; `nextCursor` is
  omitted on the last page (`Page[T]`, `page.go`). There is no offset
  paging and no total count.

### Errors

- Every `4xx`/`5xx` body is one envelope (`WriteError`,
  `internal/api/errors.go`; the `Error` and `ErrorBody` schemas in
  `openapi.yaml`):

  ```json
  {"error": {"code": "...", "message": "...", "retryable": false,
             "requestId": "...", "details": {}}}
  ```

  `code` is a stable enum value clients switch on. `message` is
  human-readable and not part of the contract. `retryable` says whether the
  same request may succeed later without further user action. `requestId`
  correlates with server logs and audit events. `details` is optional.
- The codes, their HTTP status (`HTTPStatus`, `errors.go`) and their
  default `retryable` (`Retryable`, `errors.go`). The constants are
  at `errors.go`.

  | Code | HTTP | Retryable |
  |---|---|---|
  | `INVALID_REQUEST` | 400 | no |
  | `UNAUTHENTICATED` | 401 | no |
  | `CSRF_FAILED` | 403 | no |
  | `FORBIDDEN` | 403 | no |
  | `NOT_FOUND` | 404 | no |
  | `INVALID_STATE` | 409 | yes |
  | `IDEMPOTENCY_CONFLICT` | 409 | no |
  | `QUOTA_EXHAUSTED` | 409 | no |
  | `QUOTA_NOT_CONFIGURED` | 409 | no |
  | `QUOTA_MANAGED_BY_CONFIG` | 409 | no |
  | `QUOTA_ABOVE_CEILING` | 409 | no |
  | `CONNECTION_IN_USE` | 409 | no |
  | `IMAGE_STALE` | 409 | no |
  | `ALREADY_EXISTS` | 409 | no |
  | `PRECONDITION_FAILED` | 412 | no |
  | `INVALID_TEMPLATE` | 422 | no |
  | `PRECONDITION_REQUIRED` | 428 | no |
  | `RATE_LIMITED` | 429 | yes |
  | `INTERNAL` | 500 | yes |
  | `UNAVAILABLE` | 503 | yes |

- A `QUOTA_EXHAUSTED` refusal that a pending release will clear, for the
  tenant quota or for a per-user limit, carries `retryable: true` and a
  `Retry-After` clamped to 1..30 seconds (`writeQuotaExceeded`,
  `writeUserLimitReached`, `retryAfterClamped`, `errors.go`).
  `RATE_LIMITED` carries `Retry-After` (`internal/api/middleware.go`).

### Concurrency

- Admin resources with a stored version carry a strong `ETag` (`ETagOf`, a
  SHA-256 digest of the version, `internal/api/etag.go`):
  - `GET …/quota` (`adminquota.go`);
  - `GET …/user-limits/default` (`adminuserlimit.go`);
  - each entry of `GET …/user-limits`, as its `etag` field
    (`adminuserlimit.go`), and the `PUT …/user-limits/{ownerRef}`
    response (`adminuserlimit.go`);
  - `GET …/session-policy` (`adminsessionpolicy.go`).
- Their writes require `If-Match` (`CheckIfMatch`, `etag.go`; a
  comma-separated list of tags accepted, any member naming the current
  version passes):
  - `PUT …/quota` (`adminquota.go`);
  - `PUT …/user-limits/{ownerRef}`, `DELETE …/user-limits/{ownerRef}` and
    `PUT …/user-limits/default` (`adminuserlimit.go`);
  - `PUT …/session-policy` (`adminsessionpolicy.go`);
  - `POST …/session-policy:apply` (`adminsessionpolicyapply.go`).

  A missing `If-Match` is `428 PRECONDITION_REQUIRED`; a stale one is
  `412 PRECONDITION_FAILED`. `If-Match: *` only creates a resource that
  does not exist yet. The API has no `PATCH`.
- The comparison ignores a weak indicator (`W/`) on the client's tag and
  matches the opaque part exactly. This is a deliberate deviation from the
  strong comparison RFC 9110 §13.1.1 requires: the tag is a content hash
  the server issued strong, and a compressing CDN (gzip/zstd) weakens it
  in transit, so a browser behind one echoes `W/"…"` for the exact version
  it read. Without the deviation every `If-Match` write behind such a proxy
  is `412`. The store's compare-and-swap on the version still decides the
  write.
- Every `/v1` response carries `Cache-Control: no-store, no-transform`
  (`APICacheControl`, `internal/api/headers.go`): `no-store` keeps
  tenant data out of shared browser caches, and `no-transform`
  (RFC 9111 §5.2.2.6) asks intermediaries not to recode the body, which
  is what weakens the tag. It is set on every response rather than only
  those with an `ETag` so no route can miss it.

### Idempotency

- `Idempotency-Key` (8..128 characters) is required on:
  - `POST /v1/workspaces` (`internal/api/workspaces.go`);
  - `:start` (`signal(..., keyRequired=true)`, `workspaces.go`);
  - `:attach` (`internal/api/data.go`).

  It is optional on `:stop`, `DELETE /v1/workspaces/{workspaceId}`
  (`workspaces.go`) and `:purge` (`data.go`). A missing
  required key, or a malformed key, is `400 INVALID_REQUEST`.
- The same key with the same body returns the recorded result; the same key
  with a different body is `409 IDEMPOTENCY_CONFLICT`. Keys are scoped to
  the authenticated principal (`scopedIdemKey`,
  `internal/provisioning/idempotency.go`) and pruned after 24 hours
  (`PruneIdempotency`, `idempotency.go`).

### Authentication and CSRF

- The OIDC-backed session cookie authenticates. Every `GET` is passive and
  never extends the idle deadline (`RequireAuthPassive`,
  `internal/api/middleware.go`; the "Session idle timeout" section of
  `openapi.yaml` `info.description`).
- Every unsafe method needs the `X-CSRF-Token` header (the default of
  `CSRFHeader`, `internal/api/auth.go`; the `csrfToken`
  security scheme in `openapi.yaml`). Its value must equal, in constant
  time, the `csrfToken` that `GET /v1/me` returns (`me.go`);
  otherwise the request is `403 CSRF_FAILED` (`RequireCSRF`,
  `middleware.go`). Cookie-authenticated unsafe requests also pass
  an `Origin` / `Sec-Fetch-Site` check (`RequireTrustedOrigin`,
  `middleware.go`).
- `/v1/admin/tenants/{tenant}/…` requires the tenant-admin role for that
  tenant (`tenantAdminPrincipal`, `adminquota.go`). Every mutating route
  and every admin route is audited (`auditroutes.go`;
  [ADR 0010](0010-audit-store-and-retention.md)).

### Compatibility within /v1

- Changes within `/v1` are additive only: new operations, optional fields
  and enum values. Clients ignore fields they do not know
  ([versioning](../versioning.md)).

## Consequences

- Cursor paging has no "jump to page N" and no total count.
- Every admin write is a read-modify-write that carries the `ETag` it read.
- One error envelope means one client error path. The code enum is part of
  the contract and only grows.

## Alternatives considered

- **Offset/limit paging.** Rejected: unstable under concurrent writes, and
  its cost grows with the offset.
- **Weak ETags or `Last-Modified`.** Rejected: one-second resolution and no
  byte identity. (Accepting a weakened echo of a strong tag in `If-Match`
  is not this: the server still issues and checks the exact strong value.)
- **Per-route error shapes.** Rejected: every client would need one parser
  per route.
