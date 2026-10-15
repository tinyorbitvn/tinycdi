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
// §13.1.1). exists reports whether the resource has a stored version; "*" is
// accepted only for a resource that does not exist yet. The header may carry
// a comma-separated list of entity tags; any one naming the current version
// passes.
//
// Deliberate deviation from RFC 9110 §8.8.3.2: the comparison ignores a weak
// indicator ("W/") on the client's tag. The API only ever emits strong tags
// (ETagOf, a content hash of the store version), but intermediaries that
// compress a response (for example a CDN applying zstd/gzip) weaken the tag
// in transit, so the client echoes W/"…" for a version that is byte-for-byte
// the one we issued. The opaque-tag still has to match exactly, so a stale or
// foreign tag remains 412; the version check itself is the store's
// compare-and-swap, which the tag only selects.
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
	want := opaqueTag(ETagOf(currentVersion))
	for _, tag := range strings.Split(h, ",") {
		if opaqueTag(tag) == want {
			return nil
		}
	}
	return NewError(CodePreconditionFailed, "resource was modified")
}

// opaqueTag returns an entity tag's quoted opaque-tag with any weak
// indicator removed (see CheckIfMatch for why weakness is ignored).
func opaqueTag(tag string) string {
	return strings.TrimPrefix(strings.TrimSpace(tag), "W/")
}
