package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestErrorCodesMatchOpenAPI is the drift guard: the ErrorCode enum in
// openapi.yaml and the Go constants in errors.go must be identical sets.
func TestErrorCodesMatchOpenAPI(t *testing.T) {
	raw, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var spec struct {
		Components struct {
			Schemas struct {
				ErrorCode struct {
					Enum []string `yaml:"enum"`
				} `yaml:"ErrorCode"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}

	specCodes := spec.Components.Schemas.ErrorCode.Enum
	if len(specCodes) == 0 {
		t.Fatal("openapi.yaml ErrorCode enum is empty or missing")
	}

	goCodes := make([]string, 0, len(AllErrorCodes))
	for _, c := range AllErrorCodes {
		goCodes = append(goCodes, string(c))
	}
	sort.Strings(specCodes)
	sort.Strings(goCodes)

	if len(specCodes) != len(goCodes) {
		t.Fatalf("error code drift: spec has %v, Go has %v", specCodes, goCodes)
	}
	for i := range specCodes {
		if specCodes[i] != goCodes[i] {
			t.Fatalf("error code drift: spec has %v, Go has %v", specCodes, goCodes)
		}
	}
}

// TestErrorJSONShape pins the serialized contract: exactly the keys the spec
// promises, with the spec's casing.
func TestErrorJSONShape(t *testing.T) {
	e := NewError(CodeQuotaExhausted, "running quota exhausted")
	e.RequestID = "req_1"
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := map[string]any{
		"code":      "QUOTA_EXHAUSTED",
		"message":   "running quota exhausted",
		"retryable": false,
		"requestId": "req_1",
	}
	if len(m) != len(want) {
		t.Fatalf("error body keys = %v, want exactly %v", m, want)
	}
	for k, v := range want {
		if m[k] != v {
			t.Fatalf("error body[%q] = %v, want %v", k, m[k], v)
		}
	}
}

// TestErrorCodeTableCoverage ensures every declared code has the HTTP status
// and retryable decision documented in the spec's ErrorCode table.
func TestErrorCodeTableCoverage(t *testing.T) {
	wantStatus := map[ErrorCode]int{
		CodeUnauthenticated:      http.StatusUnauthorized,
		CodeCSRFFailed:           http.StatusForbidden,
		CodeForbidden:            http.StatusForbidden,
		CodeNotFound:             http.StatusNotFound,
		CodeInvalidRequest:       http.StatusBadRequest,
		CodeInvalidTemplate:      http.StatusUnprocessableEntity,
		CodeInvalidState:         http.StatusConflict,
		CodeIdempotencyConflict:  http.StatusConflict,
		CodeQuotaExhausted:       http.StatusConflict,
		CodeQuotaNotConfigured:   http.StatusConflict,
		CodeQuotaManagedByConfig: http.StatusConflict,
		CodePreconditionFailed:   http.StatusPreconditionFailed,
		CodeConnectionInUse:      http.StatusConflict,
		CodeImageStale:           http.StatusConflict,
		CodeRateLimited:          http.StatusTooManyRequests,
		CodeUnavailable:          http.StatusServiceUnavailable,
		CodeInternal:             http.StatusInternalServerError,
	}
	wantRetryable := map[ErrorCode]bool{
		CodeInvalidState: true,
		CodeRateLimited:  true,
		CodeUnavailable:  true,
		CodeInternal:     true,
	}
	for _, code := range AllErrorCodes {
		st, ok := wantStatus[code]
		if !ok {
			t.Fatalf("no expected HTTP status declared for %q", code)
		}
		if got := code.HTTPStatus(); got != st {
			t.Fatalf("HTTPStatus(%q) = %d, want %d", code, got, st)
		}
		if got := code.Retryable(); got != wantRetryable[code] {
			t.Fatalf("Retryable(%q) = %v, want %v", code, got, wantRetryable[code])
		}
	}
}

// TestWriteError verifies the HTTP rendering: JSON content type, status from
// the code, requestId filled from the call, and no internal detail leaking.
func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req = req.WithContext(contextWithRequestID(req.Context(), "req_9"))
	WriteError(rec, req, NewError(CodeNotFound, "workspace not found"))

	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusNotFound)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var got Error
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.Code != CodeNotFound || got.RequestID != "req_9" || got.Retryable {
		t.Fatalf("body = %+v, want code=NOT_FOUND requestId=req_9 retryable=false", got)
	}
}
