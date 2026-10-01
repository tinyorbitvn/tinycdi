# ADR 0002: Public API contract — OpenAPI 3.1, idempotency, ticket flow và error model
> **English note:** this document is in Vietnamese — it is the original design/ADR kept for reference. Current platform state is described by `README.md`, `docs/images.md`, `docs/compatibility.md` and `docs/runbooks/`.


Ngày: 2026-09-30.
Trạng thái: **Proposed** — contract viết trước implementation. Spec chuẩn tắc: `internal/api/openapi.yaml`; error model Go: `internal/api/errors.go` kèm drift-guard test `errors_test.go` so enum spec với constant Go.

## Bối cảnh

Design §4–§6 (`docs/architecture.md`) cần một public HTTP API duy nhất cho portal và end user: tạo/liệt kê/xem/xóa workspace, start/stop, cấp launch ticket, catalog template, và retained-data (list/attach/purge). End user **không bao giờ ghi CR trực tiếp** — API dịch intent thành Workspace CR qua service account + transactional outbox. Đợt kiểm chứng (ADR 0001) đã chốt semantics ticket/lease ở mức fixture; ADR này chuyển nó thành contract sản phẩm trên portal origin.

## Quyết định

1. **Resource shapes.**
   - `WorkspaceView` chỉ chứa thông tin UI cần: `id`, `name`, `template` (id + revision bất biến), `phase`, `conditions` (summary type/status/reason/lastTransitionTime), `desiredState`, `dataPolicy`, timestamps. **Không** serialize runtime credentials, tên/UID Kubernetes, `runtimeGeneration`/`runtimeUID` — các giá trị fencing này là internals, UI chỉ cần `phase` + `conditions` để biết connectable.
   - `CreateWorkspaceRequest` **không có trường owner** — owner lấy từ verified principal (OIDC issuer+sub). `name`, `templateRef` bắt buộc; `desiredState` mặc định `Stopped`; `dataPolicy` mặc định theo template; `retainedDataRef` tùy chọn để attach disk retained ngay lúc create. `templateRef`/`dataPolicy`/`retainedDataRef` immutable sau create.
   - `TemplateView`: `runtime`/`experience`/`resources`/`lifecycleDefaults`/`dataPolicyDefault`/`clipboardPolicy`/`revision`; end user không bao giờ nhập raw PodSpec/command/VM manifest.
   - `RetainedDataView` có máy trạng thái `Retained → Attaching → Attached` và `Retained → Purging → Purged`, và trường `purgeConfirmationNonce` mới mỗi lần đọc.
   - `LaunchTicket`: `ticket` opaque + `launchUrl` trên **session origin** + `expiresAt`. ID mọi resource là server-generated có prefix (`ws_`, `tpl_`, `rd_`).

2. **Idempotency**. `Idempotency-Key` header **bắt buộc** trên `POST /v1/workspaces`, `POST /v1/workspaces/{id}/start`, `POST /v1/data/{id}/attach`; **tùy chọn nhưng được honor** trên stop/delete/purge (vốn idempotent theo state). Semantics: cùng key + cùng body → trả kết quả đã ghi; cùng key + khác body → `409 IDEMPOTENCY_CONFLICT`. Key scoped theo principal, giữ 24h. `POST .../connections` không nhận key — mỗi lần gọi mint ticket mới, tự giới hạn bởi TTL 60s + single-use.

3. **Connection ticket flow** (ADR 0001 → contract). `POST /v1/workspaces/{id}/connections` yêu cầu phase `Ready` + `desiredState Running`. Response body trả `ticket` + `launchUrl`; browser **POST ticket tới session origin** — ticket không bao giờ trong query string/log, server chỉ lưu hash. Gateway redeem atomic qua broker, đặt host-only `Secure`/`HttpOnly`/`SameSite` cookie rồi redirect URL sạch. Một workspace tối đa một interactive lease: có lease sống mà không `takeover` → `409 CONNECTION_IN_USE`; `takeover: true` revoke+fence session cũ **trước** khi ticket mới dùng được.

4. **Error model.** Mọi 4xx/5xx trả `{code, message, retryable, requestId}` — `code` là enum ổn định client được switch, `message` không nằm contract, `requestId` tương quan log/audit. Enum (khớp 1:1 `errors.go`, được test chống drift): `UNAUTHENTICATED` 401, `CSRF_FAILED` 403, `FORBIDDEN` 403, `NOT_FOUND` 404, `INVALID_REQUEST` 400, `INVALID_TEMPLATE` 422, `INVALID_STATE` 409, `IDEMPOTENCY_CONFLICT` 409, `QUOTA_EXHAUSTED` 409, `CONNECTION_IN_USE` 409, `RATE_LIMITED` 429, `UNAVAILABLE` 503, `INTERNAL` 500. Retryable=true chỉ cho `INVALID_STATE` (retry khi resource settle sang phase tương thích — client phải đọc lại state, không blind-retry), `RATE_LIMITED` (honor `Retry-After`), `UNAVAILABLE`, `INTERNAL`. Quyết định đã chốt: `QUOTA_EXHAUSTED` → **409** (không transient; giữ 429 chỉ cho RATE_LIMITED).
   - *Go contract:* tên export đã chốt (`Code*`, `HTTPStatus()`, `Retryable()`, `NewError(code, message)`, `WriteError(w, requestID, e)`) — chỉ được thêm code, không đổi tên; `CONNECTION_IN_USE` là code thêm duy nhất.

5. **Versioning `/v1`.** Path prefix `/v1/` cho toàn bộ public surface; breaking change yêu cầu `/v2` song song. Additive field/code mới là backward compatible; client phải coi code lạ như `INTERNAL` (retryable). Internal surface (`/internal/v1/bootstrap/*` của Windows bootstrap gate) tách origin + mTLS, không nằm trong spec public này.

6. **Security schemes.** `sessionCookie` (apiKey in cookie `tcdi_session`, OIDC-backed, `SameSite=Strict`) bắt buộc mọi endpoint; `X-CSRF-Token` header bắt buộc trên mọi mutation — thiếu/lệch → `403 CSRF_FAILED`. Tenant scoping: list/get chỉ thấy resource mình sở hữu (tenant admin thấy tenant scope); truy cập cross-tenant trả `404 NOT_FOUND` không tiết lộ tồn tại. Portal và session dùng hai registrable domain riêng; `launchUrl` chỉ sang session origin.

## Hệ quả

- Handler, broker/gateway và portal implement theo contract này; client portal generate từ OpenAPI — không đọc Kubernetes trực tiếp.
- Drift guard `errors_test.go` fail nếu enum spec và constant Go lệch — đổi error code phải sửa cả hai và review ADR.
- Purge cần nonce từ lần đọc gần nhất → UX xác nhận hai bước là contract, không phải chi tiết UI.
- Reconnect cùng lease dùng session cookie (không ticket mới); sau incarnation replacement (`runtimeUID` đổi) user phải lấy ticket mới — flow đã có sẵn.

## Alternatives đã xem xét

- GraphQL/RPC thay REST+OpenAPI: codegen portal client và lint toolchain trên OpenAPI 3.1 chuẩn hơn — chọn OpenAPI.
- Ticket trong query string/redirect trực tiếp: lọt vào log/history/referer — từ chối, POST sang session origin.
- Trả `runtimeGeneration`/`runtimeUID` trong view để client tự fence: làm lộ internals không cần thiết và tạo đường phụ thuộc sai — chỉ server/gateway dùng fencing.
- `QUOTA_EXHAUSTED` = 429: trộn với RATE_LIMITED retryable — chọn 409 để 429 luôn nghĩa là "retry được sau Retry-After".

## Nguồn

- Design: [docs/architecture.md](../architecture.md) §4–§6
- Ticket/lease semantics chứng minh: [ADR 0001](0001-runtime-streaming.md)
- Contract Go đã chốt trong design review: export names for `internal/api/errors.go`
