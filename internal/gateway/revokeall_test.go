package gateway_test

// ADR 0007: sign-out-everywhere propagation. The principal-scoped revoke
// lands at the shared lease store as ordinary lease deaths — this test
// scripts exactly that (failRenew = ErrRevoked, what a committed
// RevokePrincipalSessions produces for every replica) and checks the
// observable contract: EVERY revoked session's stream closes within one
// renew cycle on the replica holding it, and every revoked session's
// workspace cookie replays to 401 — including on a replica that never saw
// either session.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
)

func TestSignoutAll_RevokeEndsEverySessionStream(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-all-1", testWSUID)
	fb.scriptTicket("tk-all-2", testWSUID2)
	_, srvA := newReplica(t, fb, "gw-a")
	_, srvB := newReplica(t, fb, "gw-b")
	_, srvC := newReplica(t, fb, "gw-c")

	// Two sessions of the same principal, one lease each, held by
	// different replicas.
	cookie1 := launchOK(t, srvA, testHost, "tk-all-1")
	lease1 := fb.leaseOf(t, "tk-all-1")
	cookie2 := launchOK(t, srvB, testHost2, "tk-all-2")
	lease2 := fb.leaseOf(t, "tk-all-2")

	resp1 := upgrade(t, srvA, testHost, "/websockify", cookie1, map[string]string{"Origin": testOrigin})
	if resp1.StatusCode != http.StatusSwitchingProtocols {
		drain(resp1)
		t.Fatalf("upgrade 1 = %d, want 101", resp1.StatusCode)
	}
	defer resp1.Body.Close()
	resp2 := upgrade(t, srvB, testHost2, "/websockify", cookie2, map[string]string{"Origin": testOrigin2})
	if resp2.StatusCode != http.StatusSwitchingProtocols {
		drain(resp2)
		t.Fatalf("upgrade 2 = %d, want 101", resp2.StatusCode)
	}
	defer resp2.Body.Close()
	wsWrite(t, resp1, []byte("x"))
	wsRead(t, resp1, 1, 2*time.Second)
	wsWrite(t, resp2, []byte("x"))
	wsRead(t, resp2, 1, 2*time.Second)

	// Sign-out-everywhere commits at the store: both leases die together.
	// Each replica's renew loop must observe its own lease's death at the
	// next renew and kill the stream inside one renew cycle.
	seenA, seenB := fb.renewCount("gw-a"), fb.renewCount("gw-b")
	fb.failRenew(lease1.ID, broker.ErrRevoked)
	fb.failRenew(lease2.ID, broker.ErrRevoked)
	deadline := time.Now().Add(2 * time.Second)
	for (fb.renewCount("gw-a") <= seenA || fb.renewCount("gw-b") <= seenB) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if fb.renewCount("gw-a") <= seenA || fb.renewCount("gw-b") <= seenB {
		t.Fatal("a replica never renewed after the revoke-all")
	}

	for i, resp := range []*http.Response{resp1, resp2} {
		done := make(chan error, 1)
		go func(r *http.Response) {
			_, err := r.Body.Read(make([]byte, 1))
			done <- err
		}(resp)
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("stream %d stayed readable after revoke-all", i)
			}
		case <-time.After(2 * testRenewInterval):
			t.Fatalf("stream %d survived a full renew cycle past revoke-all", i)
		}
	}

	// Every revoked session's cookie is dead everywhere: replayed on the
	// replica that served it AND on replica C which never saw either.
	for i, tc := range []struct {
		srv    *httptest.Server
		host   string
		cookie string
	}{
		{srvA, testHost, cookie1},
		{srvB, testHost2, cookie2},
		{srvC, testHost, cookie1},
		{srvC, testHost2, cookie2},
	} {
		resp := proxied(t, tc.srv, tc.host, "/", tc.cookie, nil)
		drain(resp)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("replayed cookie %d = %d, want 401", i, resp.StatusCode)
		}
	}
}
