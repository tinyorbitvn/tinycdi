package gateway_test

// S17: sign-out propagation. The revocation the portal's sign-out writes
// to the shared lease store reaches a live stream on ANY replica within
// one renew cycle (the renew loop's terminal-error path), and the revoked
// lease's workspace cookie is dead server-side everywhere — a replayed
// cookie can never rehydrate.

import (
	"net/http"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
)

// TestSignout_RevokeEndsStreamOnOtherReplica: replica B holds a rehydrated
// session and an open stream; a revoke landing "through replica A" (the
// store row, scripted here as the terminal renew failure the real
// RevokePortalSession produces) is observed by B's renew loop at its next
// renew and the socket dies inside one renew cycle.
func TestSignout_RevokeEndsStreamOnOtherReplica(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-signout", testWSUID)
	_, srvA := newReplica(t, fb, "gw-a")
	_, srvB := newReplica(t, fb, "gw-b")

	cookie := launchOK(t, srvA, testHost, "tk-signout")
	lease := fb.leaseOf(t, "tk-signout")

	// The cookie rehydrates a session on B — the replica that never saw
	// the launch.
	pre := proxied(t, srvB, testHost, "/", cookie, nil)
	drain(pre)
	if pre.StatusCode != http.StatusOK {
		t.Fatalf("rehydrate on B = %d, want 200", pre.StatusCode)
	}
	resp := upgrade(t, srvB, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if resp.StatusCode != http.StatusSwitchingProtocols {
		drain(resp)
		t.Fatalf("upgrade = %d, want 101", resp.StatusCode)
	}
	defer resp.Body.Close()
	wsWrite(t, resp, []byte("x"))
	wsRead(t, resp, 1, 2*time.Second)

	// Sign-out: the lease dies at the store. B must see it at its next
	// renew — count renews to prove the kill lands inside one cycle.
	seen := fb.renewCount("gw-b")
	fb.failRenew(lease.ID, broker.ErrRevoked)
	deadline := time.Now().Add(2 * time.Second)
	for fb.renewCount("gw-b") <= seen && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if fb.renewCount("gw-b") <= seen {
		t.Fatal("replica B never renewed after the revoke")
	}

	// The renew that observed the dead lease killed the session: the open
	// socket must already be closing — it cannot survive another cycle.
	done := make(chan error, 1)
	go func() {
		_, err := resp.Body.Read(make([]byte, 1))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stream stayed readable after sign-out revoke")
		}
	case <-time.After(2 * testRenewInterval):
		t.Fatal("stream survived a full renew cycle past the revoke")
	}
}

// TestSignout_CookieCannotRehydrateAfterRevoke: once the lease is revoked
// the workspace cookie is dead server-side — replayed against the replica
// that held the stream AND against a third replica that never saw it, it
// resolves to a dead lease and gets 401 without minting a session.
func TestSignout_CookieCannotRehydrateAfterRevoke(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-signout-2", testWSUID)
	_, srvA := newReplica(t, fb, "gw-a")
	_, srvB := newReplica(t, fb, "gw-b")
	_, srvC := newReplica(t, fb, "gw-c")

	cookie := launchOK(t, srvA, testHost, "tk-signout-2")
	lease := fb.leaseOf(t, "tk-signout-2")

	// Session materializes on B, then sign-out revokes the lease at the
	// store.
	pre := proxied(t, srvB, testHost, "/", cookie, nil)
	drain(pre)
	if pre.StatusCode != http.StatusOK {
		t.Fatalf("rehydrate on B = %d, want 200", pre.StatusCode)
	}
	fb.failRenew(lease.ID, broker.ErrRevoked)

	// Replay on B: its own renew loop kills the cached session inside one
	// cycle; a request after that re-walks the digest to the dead row.
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp := proxied(t, srvB, testHost, "/", cookie, nil)
		drain(resp)
		if resp.StatusCode == http.StatusUnauthorized {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replayed cookie on B = %d, want 401", resp.StatusCode)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Replay on C, which never saw the cookie: the digest resolves to a
	// revoked lease — a definitive "no session", not a rebuild.
	post := proxied(t, srvC, testHost, "/", cookie, nil)
	drain(post)
	if post.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replayed cookie on C = %d, want 401", post.StatusCode)
	}
}
