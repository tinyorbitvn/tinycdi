package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestActionRouter_DispatchesVerb(t *testing.T) {
	ar := NewActionRouter()
	var gotID string
	ar.Handle("start", func(w http.ResponseWriter, r *http.Request) {
		gotID = ActionID(r)
		w.WriteHeader(202)
	})
	mux := http.NewServeMux()
	mux.Handle("POST /v1/workspaces/{ref}", ar)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/workspaces/ws_abc:start", nil))
	if rec.Code != 202 || gotID != "ws_abc" {
		t.Fatalf("code=%d id=%q", rec.Code, gotID)
	}
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
		var body struct {
			Error struct {
				Code string
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != 404 || body.Error.Code != "NOT_FOUND" {
			t.Errorf("%s: code=%d body=%s", p, rec.Code, rec.Body.String())
		}
	}
	if called {
		t.Fatal("handler must not run for malformed refs")
	}
}
