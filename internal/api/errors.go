// Package api implements the public HTTP API of the tinycdi workspace
// platform. This file defines the stable error model returned for every
// 4xx/5xx response; the canonical contract is openapi.yaml in this directory
// and errors_test.go guards against drift between the two.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// ErrorCode is a stable, machine-readable code for public API failures.
// Clients may switch on it; it is part of the API contract. The set of values
// must match the ErrorCode enum in openapi.yaml exactly (asserted by test).
type ErrorCode string

const (
	CodeUnauthenticated      ErrorCode = "UNAUTHENTICATED"         // 401
	CodeCSRFFailed           ErrorCode = "CSRF_FAILED"             // 403
	CodeForbidden            ErrorCode = "FORBIDDEN"               // 403
	CodeNotFound             ErrorCode = "NOT_FOUND"               // 404
	CodeInvalidRequest       ErrorCode = "INVALID_REQUEST"         // 400
	CodeInvalidTemplate      ErrorCode = "INVALID_TEMPLATE"        // 422
	CodeInvalidState         ErrorCode = "INVALID_STATE"           // 409
	CodeIdempotencyConflict  ErrorCode = "IDEMPOTENCY_CONFLICT"    // 409
	CodeQuotaExhausted       ErrorCode = "QUOTA_EXHAUSTED"         // 409
	CodeQuotaNotConfigured   ErrorCode = "QUOTA_NOT_CONFIGURED"    // 409
	CodeQuotaManagedByConfig ErrorCode = "QUOTA_MANAGED_BY_CONFIG" // 409
	CodePreconditionFailed   ErrorCode = "PRECONDITION_FAILED"     // 412
	CodeConnectionInUse      ErrorCode = "CONNECTION_IN_USE"       // 409
	CodeImageStale           ErrorCode = "IMAGE_STALE"             // 409
	CodeRateLimited          ErrorCode = "RATE_LIMITED"            // 429
	CodeUnavailable          ErrorCode = "UNAVAILABLE"             // 503
	CodeInternal             ErrorCode = "INTERNAL"                // 500
)

// quotaNotConfiguredMessage is the operator-facing text of
// QUOTA_NOT_CONFIGURED: the tenant has no tenant_quota row, so admission
// fails closed. Clients switch on the code; the web copy mirrors this text.
const quotaNotConfiguredMessage = "No quota is configured for your tenant. Ask an administrator to set one."

// AllErrorCodes enumerates every code in the contract. Keep in sync with the
// constants above and the OpenAPI ErrorCode enum.
var AllErrorCodes = []ErrorCode{
	CodeUnauthenticated,
	CodeCSRFFailed,
	CodeForbidden,
	CodeNotFound,
	CodeInvalidRequest,
	CodeInvalidTemplate,
	CodeInvalidState,
	CodeIdempotencyConflict,
	CodeQuotaExhausted,
	CodeQuotaNotConfigured,
	CodeQuotaManagedByConfig,
	CodePreconditionFailed,
	CodeConnectionInUse,
	CodeImageStale,
	CodeRateLimited,
	CodeUnavailable,
	CodeInternal,
}

// HTTPStatus maps an error code to its HTTP status. Unknown codes map to 500.
func (c ErrorCode) HTTPStatus() int {
	switch c {
	case CodeInvalidRequest:
		return http.StatusBadRequest
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodeCSRFFailed, CodeForbidden:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeInvalidState, CodeIdempotencyConflict, CodeQuotaExhausted, CodeQuotaNotConfigured, CodeQuotaManagedByConfig, CodeConnectionInUse, CodeImageStale:
		return http.StatusConflict
	case CodeInvalidTemplate:
		return http.StatusUnprocessableEntity
	case CodePreconditionFailed:
		return http.StatusPreconditionFailed
	case CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// Retryable reports whether retrying a request that failed with this code may
// succeed without further user action, per the contract table in openapi.yaml.
func (c ErrorCode) Retryable() bool {
	switch c {
	case CodeInvalidState, CodeRateLimited, CodeUnavailable, CodeInternal:
		return true
	default:
		return false
	}
}

// Error is the uniform body serialized for every 4xx/5xx response.
type Error struct {
	Code      ErrorCode     `json:"code"`
	Message   string        `json:"message"`
	Retryable bool          `json:"retryable"`
	RequestID string        `json:"requestId"`
	Details   *ErrorDetails `json:"details,omitempty"`
}

// ErrorDetails carries machine-readable context a client may switch on
// alongside the stable code. Optional; absent on most errors.
type ErrorDetails struct {
	// ReasonReleasePending marks a QUOTA_EXHAUSTED refusal whose shortfall
	// is covered by reservations a deleted or stopped workspace still
	// holds pending the runtime-absence proof — it clears on the next
	// recovery pass, so the request may be retried (retryable is true and
	// Retry-After is set).
	Reason string `json:"reason,omitempty"`
	// TemplateName, AgeDays, LimitDays and Pinned carry an IMAGE_STALE
	// refusal's context: the template whose runtime image is over
	// -image-block-after, the observed vs allowed age in whole days, and
	// whether the workspace is pinned to the stale revision (it can start
	// again only after an administrator publishes a fresh image).
	TemplateName string `json:"templateName,omitempty"`
	AgeDays      int    `json:"ageDays,omitempty"`
	LimitDays    int    `json:"limitDays,omitempty"`
	Pinned       bool   `json:"pinned,omitempty"`
	// Params carries the values a reason's message interpolates, as a
	// flat string map (the same convention as WorkspaceEvent.params).
	// UserLimitReached sets "limit" (the principal's maximum concurrent
	// running workspaces) and "current" (the running workspaces the
	// principal already holds).
	Params map[string]string `json:"params,omitempty"`
}

// ReasonReleasePending is the details.reason value of a transient,
// teardown-pending QUOTA_EXHAUSTED (see ErrorDetails).
const ReasonReleasePending = "release_pending"

// ReasonUserLimitReached is the details.reason value of a QUOTA_EXHAUSTED
// refused by a per-principal running limit rather than the tenant quota:
// the caller already holds their maximum of concurrent running
// workspaces. Params carry ParamKeyLimit and ParamKeyCurrent.
const ReasonUserLimitReached = "UserLimitReached"

// Params keys of the UserLimitReached detail — the contract the docs and
// the portal copy interpolate against; tests assert these constants so a
// rename cannot drift the wire keys away from the documentation.
const (
	// ParamKeyLimit is the principal's maximum concurrent running
	// workspaces.
	ParamKeyLimit = "limit"
	// ParamKeyCurrent is the running workspaces the principal already
	// holds.
	ParamKeyCurrent = "current"
)

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// NewError builds an Error for code; Retryable is filled from the code's
// contract decision.
func NewError(code ErrorCode, message string) *Error {
	return &Error{
		Code:      code,
		Message:   message,
		Retryable: code.Retryable(),
	}
}

// WriteError renders e as the JSON error body: Content-Type application/json,
// status from the code, RequestID filled from the request-scoped correlation
// ID. Callers must pass already-sanitized messages — this never adds internal
// details. When the request runs inside an audited route the stable code is
// recorded on the route's audit event.
func WriteError(w http.ResponseWriter, r *http.Request, e *Error) {
	if ra := routeAuditFromContext(r.Context()); ra != nil {
		ra.errCode = string(e.Code)
	}
	e.RequestID = RequestIDFromContext(r.Context())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Code.HTTPStatus())
	_ = json.NewEncoder(w).Encode(e)
}

// writeQuotaExceeded maps a quota refusal onto QUOTA_EXHAUSTED. A
// release-pending refusal (QuotaExceededError.ReleasePending — the shortfall
// is held by deleted/stopped workspaces whose release is already pending
// with Recovery) keeps the same code for compatibility but additionally
// carries retryable=true, details.reason=release_pending and a Retry-After
// header (retryAfter seconds, clamped to [1, 30]: the recovery cadence is
// 30 s, so a release can never be further out than that). A genuine
// over-limit stays the plain non-retryable 409.
func writeQuotaExceeded(w http.ResponseWriter, r *http.Request, err error, retryAfter int) {
	var q *provisioning.QuotaExceededError
	if errors.As(err, &q) && q.ReleasePending {
		e := NewError(CodeQuotaExhausted, "quota exhausted")
		e.Retryable = true
		e.Details = &ErrorDetails{Reason: ReasonReleasePending}
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterClamped(retryAfter)))
		WriteError(w, r, e)
		return
	}
	writeError(w, r, CodeQuotaExhausted, "quota exhausted")
}

// retryAfterClamped bounds a Retry-After estimate to [1, 30]: the recovery
// cadence is 30 s, so a release can never be further out than that.
func retryAfterClamped(secs int) int {
	switch {
	case secs < 1:
		return 1
	case secs > 30:
		return 30
	}
	return secs
}

// writeUserLimitReached maps a per-principal limit refusal onto
// QUOTA_EXHAUSTED with details.reason UserLimitReached and params
// {limit, current}. As with the tenant-level release_pending variant, a
// refusal whose shortfall is covered only by the owner's own
// teardown-pending holds is transient: it keeps the same code and reason
// but carries retryable=true and a Retry-After header.
func writeUserLimitReached(w http.ResponseWriter, r *http.Request, err error, retryAfter int) {
	var u *provisioning.UserLimitError
	e := NewError(CodeQuotaExhausted, "per-user running-workspace limit reached")
	if errors.As(err, &u) {
		e.Details = &ErrorDetails{
			Reason: ReasonUserLimitReached,
			Params: map[string]string{
				ParamKeyLimit:   strconv.FormatInt(u.Limit, 10),
				ParamKeyCurrent: strconv.FormatInt(u.Current, 10),
			},
		}
		if u.ReleasePending {
			e.Retryable = true
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterClamped(retryAfter)))
		}
	}
	WriteError(w, r, e)
}

// writeImageStale renders an E3 stale-image refusal: the message names the
// template (and says a pinned workspace can start again only after an
// administrator publishes a fresh image); details carry
// templateName/ageDays/limitDays/pinned for clients that render their own
// copy.
func writeImageStale(w http.ResponseWriter, r *http.Request, err error) {
	e := NewError(CodeImageStale, err.Error())
	var ise *provisioning.ImageStaleError
	if errors.As(err, &ise) {
		e.Details = &ErrorDetails{
			TemplateName: ise.TemplateName,
			AgeDays:      ise.AgeDays,
			LimitDays:    ise.LimitDays,
			Pinned:       ise.Pinned,
		}
	}
	WriteError(w, r, e)
}
