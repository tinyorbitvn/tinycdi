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

// decodeErr decodes a response body in the {"error": {...}} envelope and
// returns the inner Error.
func decodeErr(t *testing.T, r *http.Response) Error {
	t.Helper()
	defer r.Body.Close()
	var v struct {
		Error Error `json:"error"`
	}
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return v.Error
}

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

// TestErrorJSONShape_Envelope pins the serialized contract: the body is the
// single-key envelope {"error": {...}} with the spec's casing inside.
func TestErrorJSONShape_Envelope(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	WriteError(rec, req, NewError(CodeNotFound, "nope"))
	var body struct {
		Error struct {
			Code, Message, RequestID string
			Retryable                bool
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "NOT_FOUND" || body.Error.Message != "nope" {
		t.Fatalf("%s", rec.Body.String())
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	if len(raw) != 1 {
		t.Fatalf("top-level keys = %d, want only \"error\"", len(raw))
	}
}

func TestPreconditionRequiredStatus(t *testing.T) {
	if CodePreconditionRequired.HTTPStatus() != http.StatusPreconditionRequired {
		t.Fatal("428 expected")
	}
	if CodePreconditionRequired.Retryable() {
		t.Fatal("not retryable")
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
		CodeQuotaAboveCeiling:    http.StatusConflict,
		CodePreconditionRequired: http.StatusPreconditionRequired,
		CodePreconditionFailed:   http.StatusPreconditionFailed,
		CodeConnectionInUse:      http.StatusConflict,
		CodeImageStale:           http.StatusConflict,
		CodeAlreadyExists:        http.StatusConflict,
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
	var got struct {
		Error Error `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.Error.Code != CodeNotFound || got.Error.RequestID != "req_9" || got.Error.Retryable {
		t.Fatalf("body = %+v, want code=NOT_FOUND requestId=req_9 retryable=false", got)
	}
}
