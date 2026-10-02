//go:build integration

// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// connectionOn polls GET /v1/workspaces/{id}/connection on the replica the
// way the portal does: session cookie, passive (no CSRF on GET).
func (f *restartFixture) connectionOn(t *testing.T, r *replica, sessionID string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, r.appURL+"/v1/workspaces/"+f.wsUID+"/connection", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", restartSessionCookie+"="+sessionID)
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatalf("connection poll on %s: %v", r.name, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("connection body %q: %v", body, err)
	}
	return v
}

// TestTwoReplicas_ConnectionStaysConnectedWhileStreaming (FX-R18): the
// portal's disconnect signal is GET /connection (D15), and its requests
// alternate between replicas. With the launch redeemed on one replica, the
// session cookie rehydrated on both, and the stream open on the other, the
// answer must stay connected on the same lease and stream epoch across
// several 10 s lease renewals on both replicas — a flap here would make the
// portal reload or relaunch a healthy desktop.
func TestTwoReplicas_ConnectionStaysConnectedWhileStreaming(t *testing.T) {
	f := newRestartFixture(t)
	a := f.startReplicaWith(t, "a", "-renew-interval", "10s", "-revoke-deadline", "30s")
	defer a.cleanup(t)
	b := f.startReplicaWith(t, "b", "-renew-interval", "10s", "-revoke-deadline", "30s")
	defer b.cleanup(t)

	sess, csrf := f.portalLogin(t, a, a)
	cookie := f.launch(t, b, f.issueTicket(t, a, sess, csrf))
	// The desktop's asset requests land on either replica (cookie rehydration).
	for _, r := range []*replica{a, b, a, b} {
		drainBody(f.sessionGet(t, r, "/", cookie))
	}
	stream := f.wsOpen(t, a, cookie)
	defer stream.Body.Close()

	var leaseRef any
	renewed := map[any]bool{}
	for i := 0; i < 6; i++ {
		r := []*replica{a, b}[i%2]
		st := f.connectionOn(t, r, sess)
		if st["state"] != "connected" || st["leaseActive"] != true || st["streamEpoch"] != float64(1) {
			t.Fatalf("poll %d on %s = %v, want connected, active, streamEpoch 1", i, r.name, st)
		}
		if leaseRef == nil {
			leaseRef = st["leaseRef"]
		} else if st["leaseRef"] != leaseRef {
			t.Fatalf("poll %d on %s: leaseRef changed %v -> %v", i, r.name, leaseRef, st["leaseRef"])
		}
		renewed[st["lastRenewedAt"]] = true
		time.Sleep(5 * time.Second)
	}
	if len(renewed) < 2 {
		t.Fatalf("lease was never renewed during the observation window: %v", renewed)
	}
}
