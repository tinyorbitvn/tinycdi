package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestETagOf_StrongQuotedStable(t *testing.T) {
	a := ETagOf("2026-10-13T10:00:00.123456789Z")
	if !strings.HasPrefix(a, `"`) || !strings.HasSuffix(a, `"`) || strings.HasPrefix(a, `W/`) {
		t.Fatalf("not strong quoted: %s", a)
	}
	if a != ETagOf("2026-10-13T10:00:00.123456789Z") || a == ETagOf("other") {
		t.Fatal("not stable/distinct")
	}
}

func TestCheckIfMatch_Matrix(t *testing.T) {
	cur := "v1"
	tag := ETagOf(cur)
	cases := []struct {
		name, hdr string
		exists    bool
		want      ErrorCode
	}{
		{"missing", "", true, CodePreconditionRequired},
		{"missing-new", "", false, CodePreconditionRequired},
		{"match", tag, true, ""},
		{"stale", ETagOf("v0"), true, CodePreconditionFailed},
		{"unquoted", strings.Trim(tag, `"`), true, CodePreconditionFailed},
		{"weak-echo", "W/" + tag, true, ""},
		{"weak-stale", "W/" + ETagOf("v0"), true, CodePreconditionFailed},
		{"weak-unquoted", "W/" + strings.Trim(tag, `"`), true, CodePreconditionFailed},
		{"list-weak-match", `W/"zzz", W/` + tag, true, ""},
		{"list-no-match", `"zzz", W/` + ETagOf("v0"), true, CodePreconditionFailed},
		{"star-in-list", `*, "zzz"`, true, CodePreconditionFailed},
		{"star-new", "*", false, ""},
		{"star-existing", "*", true, CodePreconditionFailed},
		{"list-with-match", `"zzz", ` + tag, true, ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPut, "/", nil)
		if c.hdr != "" {
			r.Header.Set("If-Match", c.hdr)
		}
		got := CheckIfMatch(r, cur, c.exists)
		if (got == nil) != (c.want == "") || (got != nil && got.Code != c.want) {
			t.Errorf("%s: got %v want %q", c.name, got, c.want)
		}
	}
}
