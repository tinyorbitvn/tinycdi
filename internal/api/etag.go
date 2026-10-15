package api

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
)

// ETagOf derives the strong entity tag for a resource's opaque store version.
func ETagOf(version string) string {
	sum := sha256.Sum256([]byte(version))
	return `"` + base64.RawURLEncoding.EncodeToString(sum[:])[:22] + `"`
}

// CheckIfMatch enforces the If-Match precondition on a mutation (RFC 9110
// §13.1.1, strong comparison). exists reports whether the resource has a stored
// version; "*" is accepted only for a resource that does not exist yet.
func CheckIfMatch(r *http.Request, currentVersion string, exists bool) *Error {
	h := strings.TrimSpace(r.Header.Get("If-Match"))
	if h == "" {
		return NewError(CodePreconditionRequired, "If-Match header required")
	}
	if h == "*" {
		if exists {
			return NewError(CodePreconditionFailed, "resource already exists")
		}
		return nil
	}
	if !exists {
		return NewError(CodePreconditionFailed, "resource does not exist")
	}
	want := ETagOf(currentVersion)
	for _, tag := range strings.Split(h, ",") {
		if strings.TrimSpace(tag) == want {
			return nil
		}
	}
	return NewError(CodePreconditionFailed, "resource was modified")
}
