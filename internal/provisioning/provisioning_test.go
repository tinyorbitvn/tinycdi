package provisioning_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// memStore is an in-memory IntentStore for dispatcher unit tests.
type memStore struct {
	mu      sync.Mutex
	pending map[provisioning.PlatformID][]provisioning.Intent
}

func newMemStore() *memStore {
	return &memStore{pending: map[provisioning.PlatformID][]provisioning.Intent{}}
}

func (m *memStore) add(in provisioning.Intent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending[in.WorkspaceUID] = append(m.pending[in.WorkspaceUID], in)
}

func (m *memStore) PendingWorkspaces(context.Context) ([]provisioning.PlatformID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []provisioning.PlatformID
	for uid, list := range m.pending {
		if len(list) > 0 {
			out = append(out, uid)
		}
	}
	return out, nil
}

func (m *memStore) PendingIntents(_ context.Context, uid provisioning.PlatformID) ([]provisioning.Intent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]provisioning.Intent{}, m.pending[uid]...), nil
}

func (m *memStore) MarkDispatched(_ context.Context, uid provisioning.PlatformID, rev uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := m.pending[uid]
	for i, in := range list {
		if in.Revision == rev {
			m.pending[uid] = append(list[:i], list[i+1:]...)
			return nil
		}
	}
	return errors.New("intent not found")
}

func (m *memStore) pendingCount(uid provisioning.PlatformID) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending[uid])
}

// recorder is an applier that records applied intents; gate blocks a UID.
type recorder struct {
	mu      sync.Mutex
	applied []provisioning.Intent
	gate    map[provisioning.PlatformID]chan struct{}
}

func (r *recorder) Apply(ctx context.Context, in provisioning.Intent) error {
	r.mu.Lock()
	ch := r.gate[in.WorkspaceUID]
	r.mu.Unlock()
	if ch != nil {
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	r.applied = append(r.applied, in)
	r.mu.Unlock()
	return nil
}

func (r *recorder) kinds(uid provisioning.PlatformID) []provisioning.IntentKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []provisioning.IntentKind
	for _, in := range r.applied {
		if in.WorkspaceUID == uid {
			out = append(out, in.Kind)
		}
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestDispatcherSerializesPartitions: each workspace's intents apply in
// revision order while a blocked workspace does not stall another.
func TestDispatcherSerializesPartitions(t *testing.T) {
	ms := newMemStore()
	ms.add(provisioning.Intent{WorkspaceUID: "a", Revision: 1, Kind: provisioning.IntentCreate})
	ms.add(provisioning.Intent{WorkspaceUID: "a", Revision: 2, Kind: provisioning.IntentStop})
	ms.add(provisioning.Intent{WorkspaceUID: "b", Revision: 1, Kind: provisioning.IntentCreate})

	rec := &recorder{gate: map[provisioning.PlatformID]chan struct{}{"a": make(chan struct{})}}
	d := provisioning.NewDispatcher(ms, rec, provisioning.WithPollInterval(5*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	defer func() { cancel(); <-done }()

	// B converges while A's goroutine is parked inside Apply.
	waitFor(t, "b applied", func() bool { return len(rec.kinds("b")) == 1 })
	close(rec.gate["a"])
	waitFor(t, "a applied in order", func() bool {
		k := rec.kinds("a")
		return len(k) == 2 && k[0] == provisioning.IntentCreate && k[1] == provisioning.IntentStop
	})
	waitFor(t, "a drained", func() bool { return ms.pendingCount("a") == 0 })
}

// TestDispatcherRedeliversUnacked: an intent applied but never acked is
// redelivered; the Consumer contract drops the replay so the applier sees
// it exactly once.
func TestDispatcherRedeliversUnacked(t *testing.T) {
	ms := newMemStore()
	ms.add(provisioning.Intent{WorkspaceUID: "w", Revision: 1, Kind: provisioning.IntentCreate, RequestID: "req-1"})

	rec := &recorder{}
	consumer := provisioning.NewConsumer(rec)
	var crashed sync.Once
	d := provisioning.NewDispatcher(ms, consumer,
		provisioning.WithPollInterval(5*time.Millisecond),
		provisioning.WithAfterApplyHook(func(provisioning.Intent) error {
			var err error
			crashed.Do(func() { err = errors.New("crash before ack") })
			return err
		}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	defer func() { cancel(); <-done }()

	waitFor(t, "intent acked after redelivery", func() bool { return ms.pendingCount("w") == 0 })
	if got := len(rec.kinds("w")); got != 1 {
		t.Fatalf("applier saw create %d times, want exactly 1", got)
	}
	if consumer.Dropped() == 0 {
		t.Fatalf("consumer should have dropped the redelivered intent")
	}
}

// ctxStore makes memStore honour a cancelled context on MarkDispatched, as
// the PostgreSQL-backed Outbox does.
type ctxStore struct{ *memStore }

func (c ctxStore) MarkDispatched(ctx context.Context, uid provisioning.PlatformID, rev uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.memStore.MarkDispatched(ctx, uid, rev)
}

// cancelAfterApply applies an intent and then cancels the dispatcher's
// context, as a leader losing the lock does between Apply and the ack.
type cancelAfterApply struct {
	*recorder
	cancel context.CancelFunc
}

func (c cancelAfterApply) Apply(ctx context.Context, in provisioning.Intent) error {
	err := c.recorder.Apply(ctx, in)
	c.cancel()
	return err
}

// TestDispatcherAcksAppliedIntentOnShutdown: an intent whose Apply succeeded
// is recorded as dispatched even when the dispatcher is stopped right after
// Apply, so a failover does not replay it (FX-R14).
func TestDispatcherAcksAppliedIntentOnShutdown(t *testing.T) {
	ms := newMemStore()
	ms.add(provisioning.Intent{WorkspaceUID: "w", Revision: 1, Kind: provisioning.IntentStart})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := provisioning.NewDispatcher(ctxStore{ms}, cancelAfterApply{&recorder{}, cancel},
		provisioning.WithPollInterval(5*time.Millisecond))
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatcher did not stop")
	}
	if n := ms.pendingCount("w"); n != 0 {
		t.Fatalf("applied intent left undispatched (%d pending): a new leader would replay it", n)
	}
}

// TestConsumerDropsStaleAndClosed: revision <= lastApplied and any intent
// after delete are dropped before reaching the applier.
func TestConsumerDropsStaleAndClosed(t *testing.T) {
	rec := &recorder{}
	c := provisioning.NewConsumer(rec)
	ctx := context.Background()

	must := func(in provisioning.Intent) {
		t.Helper()
		if err := c.Apply(ctx, in); err != nil {
			t.Fatalf("apply rev %d: %v", in.Revision, err)
		}
	}
	must(provisioning.Intent{WorkspaceUID: "w", Revision: 1, Kind: provisioning.IntentCreate})
	must(provisioning.Intent{WorkspaceUID: "w", Revision: 3, Kind: provisioning.IntentStart})
	// delayed rev 2 arrives after 3 was applied
	must(provisioning.Intent{WorkspaceUID: "w", Revision: 2, Kind: provisioning.IntentStop})
	if got := len(rec.kinds("w")); got != 2 {
		t.Fatalf("applier saw %d intents, want 2 (rev2 dropped)", got)
	}
	must(provisioning.Intent{WorkspaceUID: "w", Revision: 4, Kind: provisioning.IntentDelete})
	if !c.Closed("w") {
		t.Fatalf("stream should be closed after delete")
	}
	must(provisioning.Intent{WorkspaceUID: "w", Revision: 5, Kind: provisioning.IntentStart})
	if got := len(rec.kinds("w")); got != 3 {
		t.Fatalf("applier saw %d intents, want 3", got)
	}
	if c.Dropped() != 2 {
		t.Fatalf("dropped = %d, want 2", c.Dropped())
	}
}

func TestResourceVectorExceeds(t *testing.T) {
	limit := provisioning.ResourceVector{RunningSlots: 10, CPUMillis: 4000, MemoryBytes: 8 << 30, DiskBytes: 100 << 30}
	used := provisioning.ResourceVector{RunningSlots: 9, CPUMillis: 3500, MemoryBytes: 1 << 30, DiskBytes: 0}
	fit := provisioning.ResourceVector{RunningSlots: 1, CPUMillis: 500}
	if err := fit.Exceeds(used, limit); err != "" {
		t.Fatalf("expected fit, got exceeds on %s", err)
	}
	tooMany := provisioning.ResourceVector{RunningSlots: 2}
	if dim := tooMany.Exceeds(used, limit); dim != "runningSlots" {
		t.Fatalf("expected runningSlots overflow, got %q", dim)
	}
}

func TestAbsenceProofValidation(t *testing.T) {
	if provisioning.ProofUnspecified.Valid() {
		t.Fatal("unspecified proof must be invalid")
	}
	for _, p := range []provisioning.AbsenceProof{provisioning.ProofNeverCreated, provisioning.ProofRuntimeAbsent} {
		if !p.Valid() {
			t.Fatalf("proof %q should be valid", p)
		}
	}
}

func TestDeterministicRequestID(t *testing.T) {
	a := provisioning.DeterministicRequestID("t1", "key-1")
	if provisioning.DeterministicRequestID("t1", "key-1") != a {
		t.Fatal("same tenant+key must yield the same request ID")
	}
	if provisioning.DeterministicRequestID("t1", "key-2") == a ||
		provisioning.DeterministicRequestID("t2", "key-1") == a {
		t.Fatal("different tenant or key must yield different request IDs")
	}
	if len(a) != 36 {
		t.Fatalf("request ID %q is not a UUID", a)
	}
}

func TestRequestHash(t *testing.T) {
	req := provisioning.CreateRequest{OwnerSubject: "s", Template: provisioning.TemplateInfo{Name: "t"}}
	if string(provisioning.RequestHash(req)) != string(provisioning.RequestHash(req)) {
		t.Fatal("same request must hash identically")
	}
	other := req
	other.Template.Name = "u"
	if string(provisioning.RequestHash(other)) == string(provisioning.RequestHash(req)) {
		t.Fatal("different request must hash differently")
	}
}
