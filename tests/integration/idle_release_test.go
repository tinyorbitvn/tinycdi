//go:build integration

// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// ticketStatus posts one launch-ticket request without takeover and returns
// the HTTP status: 201 issued, 409 CONNECTION_IN_USE while a lease is live.
func (f *restartFixture) ticketStatus(t *testing.T, r *replica, sessionID, csrf string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, r.appURL+"/v1/workspaces/"+f.wsUID+"/connections", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", restartSessionCookie+"="+sessionID)
	req.Header.Set(restartCSRFHeader, csrf)
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatalf("ticket request on %s: %v", r.name, err)
	}
	drainBody(resp)
	return resp.StatusCode
}

// TestTwoReplicas_IdleLeaseReleasedAfterStreamCloses: the launch and the
// stream land on replica a, replica b serves an asset request from a
// rehydrated copy. Right after the stream closes a plain reopen is refused
// (409), and within G + one renew tick — well under the 30 s lease TTL, so
// this is a release, not a lapse — it succeeds without takeover and the
// lease row is revoked.
func TestTwoReplicas_IdleLeaseReleasedAfterStreamCloses(t *testing.T) {
	f := newRestartFixture(t)
	a := f.startReplicaWith(t, "a", "-idle-session-release", "3s")
	defer a.cleanup(t)
	b := f.startReplicaWith(t, "b", "-idle-session-release", "3s")
	defer b.cleanup(t)

	sess, csrf := f.portalLogin(t, a, a)
	cookie := f.launch(t, a, f.issueTicket(t, a, sess, csrf))
	drainBody(f.sessionGet(t, a, "/", cookie))
	drainBody(f.sessionGet(t, b, "/", cookie)) // b attaches a copy; it never claims
	stream := f.wsOpen(t, a, cookie)
	wsEcho(t, stream)
	eventually(t, "stream counted as connected", 10*time.Second, func() bool {
		return f.connectionOn(t, a, sess)["state"] == "connected"
	})
	leaseID := f.activeLeaseID(t)
	if leaseID == "" {
		t.Fatal("no active lease while streaming")
	}

	stream.Body.Close()
	if code := f.ticketStatus(t, a, sess, csrf); code != http.StatusConflict {
		t.Fatalf("plain reopen right after the close = %d, want 409", code)
	}
	eventually(t, "plain reopen succeeds after the idle release", 15*time.Second, func() bool {
		return f.ticketStatus(t, a, sess, csrf) == http.StatusCreated
	})
	var state string
	if err := f.db.Pool().QueryRow(context.Background(),
		`SELECT state FROM connection_lease WHERE id = $1`, leaseID).Scan(&state); err != nil {
		t.Fatalf("lease row: %v", err)
	}
	if state != "revoked" {
		t.Fatalf("lease state = %q, want revoked (released by the stream owner, not lapsed)", state)
	}
}
