package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestParsePage_DefaultsAndBounds(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/workspaces", nil)
	p, e := ParsePage(r, []string{"createdAt", "name"}, "-createdAt")
	if e != nil || p.Limit != 50 || p.Sort.Field != "createdAt" || !p.Sort.Desc || p.After != nil {
		t.Fatalf("%+v %v", p, e)
	}
	for _, q := range []string{"limit=0", "limit=501", "limit=x", "sort=secret", "sort=--name"} {
		r := httptest.NewRequest("GET", "/v1/workspaces?"+q, nil)
		if _, e := ParsePage(r, []string{"createdAt", "name"}, "-createdAt"); e == nil || e.Code != CodeInvalidRequest {
			t.Errorf("%s: want INVALID_REQUEST", q)
		}
	}
	r = httptest.NewRequest("GET", "/v1/workspaces?limit=500&sort=name", nil)
	if p, e := ParsePage(r, []string{"createdAt", "name"}, "-createdAt"); e != nil || p.Limit != 500 || p.Sort.Desc {
		t.Fatalf("%+v %v", p, e)
	}
}

func TestCursor_RoundTrip(t *testing.T) {
	c := NextCursor(SortSpec{Field: "name", Desc: true}, "alpha|β", "ws_1")
	r := httptest.NewRequest("GET", "/v1/workspaces?sort=-name&cursor="+url.QueryEscape(c), nil)
	p, e := ParsePage(r, []string{"createdAt", "name"}, "-createdAt")
	if e != nil || p.After == nil || p.After.Key != "alpha|β" || p.After.ID != "ws_1" {
		t.Fatalf("%+v %v", p, e)
	}
}

func TestCursor_RejectsSortMismatchAndTamper(t *testing.T) {
	c := NextCursor(SortSpec{Field: "name"}, "a", "ws_1")
	for _, q := range []string{"sort=-name&cursor=" + c, "sort=createdAt&cursor=" + c, "sort=name&cursor=" + c[:len(c)-2], "sort=name&cursor=%21%21"} {
		r := httptest.NewRequest("GET", "/v1/workspaces?"+q, nil)
		if _, e := ParsePage(r, []string{"createdAt", "name"}, "-createdAt"); e == nil || e.Code != CodeInvalidRequest {
			t.Errorf("%s: want INVALID_REQUEST", q)
		}
	}
}

// TestParsePage_CursorTextMustBePostgresSafe (R1-19): a cursor is decoded
// from client input, so a key or id Postgres cannot store as text (a NUL
// byte) is a bad cursor for every ParsePage consumer.
func TestParsePage_CursorTextMustBePostgresSafe(t *testing.T) {
	for _, c := range []struct{ key, id string }{{"a\x00", "ws_x"}, {"a", "ws_\x00"}} {
		cur := NextCursor(SortSpec{Field: "name"}, c.key, c.id)
		r := httptest.NewRequest(http.MethodGet, "/v1/x?sort=name&cursor="+cur, nil)
		if _, err := ParsePage(r, []string{"name"}, "name"); err == nil || err.Code != CodeInvalidRequest {
			t.Fatalf("cursor key=%q id=%q: err=%v, want INVALID_REQUEST", c.key, c.id, err)
		}
	}
	ok := NextCursor(SortSpec{Field: "name"}, "Desktop – é", "ws_x")
	if _, err := ParsePage(httptest.NewRequest(http.MethodGet, "/v1/x?sort=name&cursor="+ok, nil), []string{"name"}, "name"); err != nil {
		t.Fatalf("valid UTF-8 cursor refused: %v", err)
	}
}
