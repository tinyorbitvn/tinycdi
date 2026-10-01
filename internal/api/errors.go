// Package api implements the public HTTP API of the tinycdi workspace
// platform. This file defines the stable error model returned for every
// 4xx/5xx response; the canonical contract is openapi.yaml in this directory
// and errors_test.go guards against drift between the two.
package api

import (
	"encoding/json"
	"net/http"
)

// ErrorCode is a stable, machine-readable code for public API failures.
// Clients may switch on it; it is part of the API contract. The set of values
// must match the ErrorCode enum in openapi.yaml exactly (asserted by test).
type ErrorCode string

const (
	CodeUnauthenticated     ErrorCode = "UNAUTHENTICATED"      // 401
	CodeCSRFFailed          ErrorCode = "CSRF_FAILED"          // 403
	CodeForbidden           ErrorCode = "FORBIDDEN"            // 403
	CodeNotFound            ErrorCode = "NOT_FOUND"            // 404
	CodeInvalidRequest      ErrorCode = "INVALID_REQUEST"      // 400
	CodeInvalidTemplate     ErrorCode = "INVALID_TEMPLATE"     // 422
	CodeInvalidState        ErrorCode = "INVALID_STATE"        // 409
	CodeIdempotencyConflict ErrorCode = "IDEMPOTENCY_CONFLICT" // 409
	CodeQuotaExhausted      ErrorCode = "QUOTA_EXHAUSTED"      // 409
	CodeConnectionInUse     ErrorCode = "CONNECTION_IN_USE"    // 409
	CodeRateLimited         ErrorCode = "RATE_LIMITED"         // 429
	CodeUnavailable         ErrorCode = "UNAVAILABLE"          // 503
	CodeInternal            ErrorCode = "INTERNAL"             // 500
)

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
	CodeConnectionInUse,
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
	case CodeInvalidState, CodeIdempotencyConflict, CodeQuotaExhausted, CodeConnectionInUse:
		return http.StatusConflict
	case CodeInvalidTemplate:
		return http.StatusUnprocessableEntity
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
	Code      ErrorCode `json:"code"`
	Message   string    `json:"message"`
	Retryable bool      `json:"retryable"`
	RequestID string    `json:"requestId"`
}

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
// details.
func WriteError(w http.ResponseWriter, requestID string, e *Error) {
	e.RequestID = requestID
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Code.HTTPStatus())
	_ = json.NewEncoder(w).Encode(e)
}
