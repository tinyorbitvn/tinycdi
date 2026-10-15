package backend

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

func wsWith(phase cdiv1.WorkspacePhase, gen int64, started *metav1.Time) *cdiv1.Workspace {
	ws := &cdiv1.Workspace{}
	ws.Labels = map[string]string{provisioning.LabelWorkspaceUID: "ws_1"}
	ws.Status.Phase = phase
	ws.Status.ObservedRuntimeGeneration = gen
	ws.Status.StartedAt = started
	return ws
}

func TestBootReadyTransition(t *testing.T) {
	at := metav1.NewTime(time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC))
	cases := []struct {
		name     string
		old, new *cdiv1.Workspace
		want     bool
	}{
		{"provisioning to ready", wsWith(cdiv1.WorkspacePhaseProvisioning, 2, nil), wsWith(cdiv1.WorkspacePhaseReady, 2, &at), true},
		{"ready resync", wsWith(cdiv1.WorkspacePhaseReady, 2, &at), wsWith(cdiv1.WorkspacePhaseReady, 2, &at), false},
		{"ready under a new generation", wsWith(cdiv1.WorkspacePhaseReady, 2, &at), wsWith(cdiv1.WorkspacePhaseReady, 3, &at), true},
		{"ready without startedAt", nil, wsWith(cdiv1.WorkspacePhaseReady, 2, nil), false},
		{"failed", nil, wsWith(cdiv1.WorkspacePhaseFailed, 2, &at), false},
	}
	for _, c := range cases {
		var old any
		if c.old != nil {
			old = c.old
		}
		id, gen, got, ok := bootReadyTransition(old, c.new)
		if ok != c.want {
			t.Fatalf("%s: ok = %v", c.name, ok)
		}
		if ok && (id != "ws_1" || gen != c.new.Status.ObservedRuntimeGeneration || !got.Equal(at.Time)) {
			t.Fatalf("%s: %s %d %v", c.name, id, gen, got)
		}
	}
}
