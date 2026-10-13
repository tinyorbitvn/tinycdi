package api

import (
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
