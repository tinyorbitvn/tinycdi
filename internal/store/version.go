// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store

import (
	"errors"
	"time"
)

// ErrVersionMismatch is returned by every conditional admin write (tenant
// quota, user limits, session-policy ceiling) when its precondition does
// not hold: the expected version no longer names the row, a create found
// a row already there, or an update found none. The API maps it to 412
// PRECONDITION_FAILED.
var ErrVersionMismatch = errors.New("version mismatch")

// FormatVersion renders a row's updated_at as its opaque change token
// (RFC3339Nano, UTC). Writes stamp updated_at with clock_timestamp(), so
// the token changes on every committed write.
func FormatVersion(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// parseVersion reads a change token back into the timestamp the
// conditional UPDATE/DELETE compares. A token that does not parse can
// never equal a row's updated_at: callers answer ErrVersionMismatch
// instead of letting Postgres's cast fail.
func parseVersion(v string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, v)
	return t, err == nil
}
