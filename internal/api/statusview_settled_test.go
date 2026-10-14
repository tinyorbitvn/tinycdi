package api

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// clientListCache serves List from a fake client (label selectors applied,
// unlike imagestale_r4_test.go's listCache); the embedded nil Cache is never
// reached because K8sStatusView.WorkspaceStatus only lists.
type clientListCache struct {
	crcache.Cache
	c client.Client
}

func (l clientListCache) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return l.c.List(ctx, list, opts...)
}

func TestK8sStatusView_IntentSettledAndGeneration(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cdiv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cr := func(name, wsID string, specRev, appliedRev int64) *cdiv1.Workspace {
		return &cdiv1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns-a", Labels: map[string]string{
				provisioning.LabelWorkspaceUID: wsID, provisioning.LabelTenant: "tenant-a"}},
			Spec: cdiv1.WorkspaceSpec{RuntimeGeneration: 4, IntentRevision: specRev},
			Status: cdiv1.WorkspaceStatus{
				Phase: cdiv1.WorkspacePhaseFailed, LastAppliedIntentRevision: appliedRev,
				Conditions: []metav1.Condition{{Type: cdiv1.ConditionDegraded, Status: metav1.ConditionTrue,
					Reason: "RuntimeLost", LastTransitionTime: metav1.NewTime(time.Now())}},
			},
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cr("ws-settled", "ws_settled01", 3, 3), cr("ws-behind", "ws_behind001", 3, 2)).Build()
	sv := &K8sStatusView{cache: clientListCache{c: c}, now: time.Now, maxStale: time.Minute,
		lastKnown: map[string]ObservedStatus{}}
	sv.MarkSynced()

	got, err := sv.WorkspaceStatus(context.Background(), "tenant-a", "ws_settled01")
	if err != nil || !got.Fresh || !got.Found || !got.IntentSettled || got.RuntimeGeneration != 4 || got.FailureReason != "RuntimeLost" {
		t.Fatalf("settled CR projected as %+v (err %v)", got, err)
	}
	got, err = sv.WorkspaceStatus(context.Background(), "tenant-a", "ws_behind001")
	if err != nil || !got.Found || got.IntentSettled {
		t.Fatalf("CR whose latest intent is not yet applied projected as %+v (err %v): must not be settled", got, err)
	}
}
