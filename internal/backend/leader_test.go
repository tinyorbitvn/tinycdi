// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// countingApplier records how often each (workspace, revision) is applied.
type countingApplier struct {
	mu sync.Mutex
	n  map[string]int
}

func newCountingApplier() *countingApplier { return &countingApplier{n: map[string]int{}} }

func (c *countingApplier) Apply(_ context.Context, in provisioning.Intent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n[fmt.Sprintf("%s/%d", in.WorkspaceUID, in.Revision)]++
	return nil
}

func (c *countingApplier) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.n))
	for k, v := range c.n {
		out[k] = v
	}
	return out
}

// loopEvents is an ordered log of singleton-loop starts and stops across
// replicas, plus the high-water mark of concurrently running loops.
type loopEvents struct {
	mu      sync.Mutex
	log     []string
	running int
	maxRun  int
}

func (e *loopEvents) started(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, "start:"+id)
	e.running++
	if e.running > e.maxRun {
		e.maxRun = e.running
	}
}

func (e *loopEvents) stopped(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, "stop:"+id)
	e.running--
}

func (e *loopEvents) snapshot() ([]string, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.log...), e.maxRun
}

// replica is one Backend with only singleton loops wired — enough to drive
// Run, the leader election and loop shutdown.
type replica struct {
	id      string
	b       *Backend
	cancel  context.CancelFunc
	done    chan struct{} // closed when Run returns
	started chan struct{} // closed on first loop start
	once    sync.Once
}

func startReplica(t *testing.T, db *store.DB, id string, retry time.Duration, ev *loopEvents, extra ...func(context.Context)) *replica {
	t.Helper()
	r := &replica{id: id, b: &Backend{log: testLog()}, done: make(chan struct{}), started: make(chan struct{})}
	r.b.singletons = append(r.b.singletons, func(ctx context.Context) {
		r.once.Do(func() { close(r.started) })
		if ev != nil {
			ev.started(id)
			defer ev.stopped(id)
		}
		<-ctx.Done()
	})
	r.b.singletons = append(r.b.singletons, extra...)
	r.b.electSingletons(db, retry)
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() { _ = r.b.Run(ctx); close(r.done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(20 * time.Second):
		}
	})
	return r
}

func (r *replica) isLeader() bool {
	select {
	case <-r.started:
		return true
	default:
		return false
	}
}

// appendTestIntent seeds a workspace and one start intent for it.
func appendTestIntent(t *testing.T, db *store.DB, uid string) {
	t.Helper()
	seedWorkspace(t, db, "tenant-a", "owner-a", uid)
	err := db.WithTx(context.Background(), func(tx store.Tx) error {
		_, err := provisioning.AppendIntent(context.Background(), tx, provisioning.PlatformID(uid), provisioning.IntentStart)
		return err
	})
	if err != nil {
		t.Fatalf("append intent %s: %v", uid, err)
	}
}

func dispatcherLoop(db *store.DB, a provisioning.WorkspaceApplier) func(context.Context) {
	d := provisioning.NewDispatcher(provisioning.NewOutbox(db), a, provisioning.WithPollInterval(25*time.Millisecond))
	return func(ctx context.Context) { _ = d.Run(ctx) }
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSingletonLoops_OneRunner (R1b): two replicas on one database, each with
// the outbox dispatcher registered — only the lock holder runs it, so each
// intent is applied exactly once.
func TestSingletonLoops_OneRunner(t *testing.T) {
	db := newDB(t)
	applier := newCountingApplier()
	a := startReplica(t, db, "a", 200*time.Millisecond, nil, dispatcherLoop(db, applier))
	b := startReplica(t, db, "b", 200*time.Millisecond, nil, dispatcherLoop(db, applier))

	waitFor(t, 10*time.Second, "a leader", func() bool { return a.isLeader() || b.isLeader() })
	const n = 8
	for i := 0; i < n; i++ {
		appendTestIntent(t, db, fmt.Sprintf("ws_one%04d", i))
	}
	waitFor(t, 10*time.Second, "all intents applied", func() bool { return len(applier.snapshot()) == n })
	time.Sleep(time.Second) // give a second (wrong) runner time to double-apply

	if a.isLeader() && b.isLeader() {
		t.Fatal("both replicas ran the singleton loops")
	}
	for k, c := range applier.snapshot() {
		if c != 1 {
			t.Errorf("intent %s applied %d times, want 1", k, c)
		}
	}
}

// TestSingletonLoops_Failover (R1b, FX-R14): stopping the holder hands the
// loops to the other replica, which applies a new intent. Every intent is
// applied exactly once: the holder's last acknowledgement lands before it
// releases the lock, so the successor never replays it. The test waits on
// observable events (the lock changing hands, the intents being applied), not
// on wall-clock sleeps, and injects a short retry interval so the hand-over
// does not wait out leaderRetryInterval.
func TestSingletonLoops_Failover(t *testing.T) {
	db := newDB(t)
	applier := newCountingApplier()
	const retry = 200 * time.Millisecond
	a := startReplica(t, db, "a", retry, nil, dispatcherLoop(db, applier))
	b := startReplica(t, db, "b", retry, nil, dispatcherLoop(db, applier))

	waitFor(t, 15*time.Second, "a leader", func() bool { return a.isLeader() || b.isLeader() })
	holder, other := a, b
	if b.isLeader() {
		holder, other = b, a
	}
	appendTestIntent(t, db, "ws_fail0001")
	waitFor(t, 10*time.Second, "first intent applied", func() bool { return len(applier.snapshot()) == 1 })

	// Stop the holder immediately after its first Apply: this is the window
	// in which a failed acknowledgement would be replayed by the successor.
	holder.cancel()
	select {
	case <-holder.done:
	case <-time.After(10 * time.Second):
		t.Fatal("holder did not stop")
	}

	// The successor takes the lock; only then is the second intent appended,
	// so the apply below can only have come from the new leader.
	waitFor(t, 15*time.Second, "the other replica to take the loops", other.isLeader)
	appendTestIntent(t, db, "ws_fail0002")
	waitFor(t, 10*time.Second, "failover apply by the other replica", func() bool { return len(applier.snapshot()) == 2 })
	for k, c := range applier.snapshot() {
		if c != 1 {
			t.Errorf("intent %s applied %d times, want 1", k, c)
		}
	}
}

// TestSingletonLoops_LostConnectionStopsLoops (R1b): killing the lock
// connection stops the holder's loops, and no other replica starts its loops
// until they have stopped — the loops never overlap.
func TestSingletonLoops_LostConnectionStopsLoops(t *testing.T) {
	db := newDB(t)
	ev := &loopEvents{}
	a := startReplica(t, db, "a", 300*time.Millisecond, ev)
	waitFor(t, 10*time.Second, "a to lead", a.isLeader)
	b := startReplica(t, db, "b", 300*time.Millisecond, ev)
	_ = b

	var pid int
	if err := db.Pool().QueryRow(context.Background(), `
		SELECT pid FROM pg_locks
		WHERE locktype = 'advisory' AND granted
		  AND ((classid::bigint << 32) | objid::bigint) = $1`, leaderLockKey).Scan(&pid); err != nil {
		t.Fatalf("find lock connection: %v", err)
	}
	if _, err := db.Pool().Exec(context.Background(), `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatalf("terminate lock connection: %v", err)
	}

	// a's loops must stop, and some replica (a again or b) must lead again.
	waitFor(t, 15*time.Second, "restart after loss", func() bool {
		log, _ := ev.snapshot()
		starts := 0
		for _, e := range log {
			if len(e) > 5 && e[:5] == "start" {
				starts++
			}
		}
		return starts >= 2
	})
	log, maxRun := ev.snapshot()
	if len(log) < 3 || log[0] != "start:a" || log[1] != "stop:a" {
		t.Fatalf("event order = %v, want start:a, stop:a before any other start", log)
	}
	if maxRun != 1 {
		t.Fatalf("loops overlapped: %d running at once; events=%v", maxRun, log)
	}
}
