package broker

// Freshness-gate unit test for K8sBindingSource — no envtest needed: the gate
// short-circuits before the cache is ever touched.

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestK8sBindingSource_FreshnessGate(t *testing.T) {
	src := &K8sBindingSource{}
	if _, err := src.CurrentBinding(context.Background(), "ws-1"); !errors.Is(err, ErrFreshness) {
		t.Fatalf("unsynced source = %v, want ErrFreshness", err)
	}

	src.MarkSynced()
	src.lastEvent = time.Now().Add(-MaxBindingAge - time.Second)
	if _, err := src.CurrentBinding(context.Background(), "ws-1"); !errors.Is(err, ErrFreshness) {
		t.Fatalf("stale source = %v, want ErrFreshness", err)
	}
}
