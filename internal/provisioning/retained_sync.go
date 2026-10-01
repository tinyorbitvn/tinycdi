package provisioning

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

// Retained-inventory PVC annotation keys stamped by the operator's
// retention step (internal/operator/retention.go — the write side of this
// metadata contract). The operator imports this package, so the keys are
// duplicated verbatim here rather than shared; both sides reference the
// same design §5 contract and PVC metadata remains the source of truth.
const (
	annotationRetainedTenant          = "workspaces.cdi.tinyorbit.vn/retained-tenant"
	annotationRetainedOwner           = "workspaces.cdi.tinyorbit.vn/retained-owner"
	annotationRetainedSourceWorkspace = "workspaces.cdi.tinyorbit.vn/retained-source-workspace"
	annotationRetainedRuntime         = "workspaces.cdi.tinyorbit.vn/retained-runtime"
)

// RetainedSyncResult is the per-sweep read model: what one pass found.
type RetainedSyncResult struct {
	// Imported is the number of records created this sweep.
	Imported int
	// UnregisteredPVCs counts retained-labelled PVCs that had no record
	// at scan time (imported during the sweep — they were leaks until now).
	UnregisteredPVCs int
	// MissingVolumes counts live records whose PVC is absent from the
	// managed namespaces — genuine drift needing operator attention.
	MissingVolumes int
}

// Leaks is the orphan count reported on the pvc_leaks gauge.
func (r RetainedSyncResult) Leaks() int { return r.UnregisteredPVCs + r.MissingVolumes }

// RetainedSync periodically reconciles the operator's retained-inventory
// PVC metadata — the source of truth for retained datasets (design §5,
// reconstructable after API DB loss) — into API-DB retained_data records
// so /v1/data reflects the cluster. It is level-based and idempotent:
// each sweep lists retained-labelled PVCs in every managed namespace and
// imports the ones without a record (dataset identity = PVC UID, never
// the reusable PVC name). It reports orphans in both directions and
// deletes nothing — purge is the user-confirmed, audited path.
type RetainedSync struct {
	Client  client.Client
	Tenants TenantNamespaces
	Records *RetainedStore
	// Metrics, when non-nil, receives the per-sweep orphan total on the
	// pvc_leaks gauge.
	Metrics *observability.Metrics
	// Poll bounds the sweep cadence; <=0 uses 30 s.
	Poll time.Duration
	Log  *slog.Logger
}

// NewRetainedSync builds the sweeper over a cluster client, the managed
// namespace allowlist and the retained store.
func NewRetainedSync(c client.Client, tenants TenantNamespaces, records *RetainedStore, metrics *observability.Metrics, log *slog.Logger) *RetainedSync {
	if log == nil {
		log = slog.Default()
	}
	return &RetainedSync{Client: c, Tenants: tenants, Records: records,
		Metrics: metrics, Poll: 30 * time.Second, Log: log}
}

// Run sweeps until ctx is cancelled.
func (s *RetainedSync) Run(ctx context.Context) error {
	poll := s.Poll
	if poll <= 0 {
		poll = 30 * time.Second
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		if _, err := s.SyncOnce(ctx); err != nil {
			s.Log.Warn("retained sync sweep", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// SyncOnce performs one reconciliation pass over every managed namespace
// and reports the per-direction drift counts on Metrics (pvc_leaks).
func (s *RetainedSync) SyncOnce(ctx context.Context) (RetainedSyncResult, error) {
	var res RetainedSyncResult
	for tenantID, ns := range s.Tenants {
		r, err := s.syncNamespace(ctx, tenantID, ns)
		if err != nil {
			return res, err
		}
		res.Imported += r.Imported
		res.UnregisteredPVCs += r.UnregisteredPVCs
		res.MissingVolumes += r.MissingVolumes
	}
	if s.Metrics != nil {
		s.Metrics.SetPVCLeaks(float64(res.Leaks()))
	}
	return res, nil
}

// syncNamespace imports unregistered retained PVCs in ns and counts
// records whose volumes vanished.
func (s *RetainedSync) syncNamespace(ctx context.Context, tenantID, ns string) (RetainedSyncResult, error) {
	var res RetainedSyncResult
	var pvcs corev1.PersistentVolumeClaimList
	if err := s.Client.List(ctx, &pvcs, client.InNamespace(ns),
		client.MatchingLabels{linux.LabelDataRetained: "true"}); err != nil {
		return res, fmt.Errorf("retained sync: list PVCs in %s: %w", ns, err)
	}
	known, err := s.Records.RetainedPVCUIDs(ctx, tenantID)
	if err != nil {
		return res, fmt.Errorf("retained sync: live records for %s: %w", tenantID, err)
	}
	present := make(map[string]bool, len(pvcs.Items))
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		present[string(pvc.UID)] = true
		if _, ok := known[string(pvc.UID)]; ok {
			continue
		}
		res.UnregisteredPVCs++
		info, derr := s.diskInfo(ctx, tenantID, pvc)
		if derr != nil {
			s.Log.Warn("retained sync: undecodable PVC",
				"pvc", ns+"/"+pvc.Name, "err", derr)
			continue
		}
		if _, ierr := s.Records.ImportRetained(ctx, *info); ierr != nil {
			s.Log.Warn("retained sync: import",
				"pvc", ns+"/"+pvc.Name, "err", ierr)
			continue
		}
		res.Imported++
	}
	for uid := range known {
		if !present[uid] {
			res.MissingVolumes++
		}
	}
	return res, nil
}

// diskInfo decodes one retained-labelled PVC into the store's import
// record. The platform workspace id (ws_…) is recovered from the CR name
// in the workspace-name label — the applier's deterministic WorkspaceCRName
// ("ws_<hex>" -> "ws-<hex>") round-trips.
func (s *RetainedSync) diskInfo(ctx context.Context, tenantID string, pvc *corev1.PersistentVolumeClaim) (*RetainedDiskInfo, error) {
	ann := pvc.Annotations
	info := &RetainedDiskInfo{
		PVCNamespace: pvc.Namespace,
		PVCName:      pvc.Name,
		PVCUID:       string(pvc.UID),
		TenantID:     tenantID,
		Owner:        ann[annotationRetainedOwner],
		Runtime:      ann[annotationRetainedRuntime],
	}
	if t := ann[annotationRetainedTenant]; t != "" {
		info.TenantID = t
	}
	if info.Runtime == "" {
		info.Runtime = "LinuxContainer"
	}
	crName := pvc.Labels[linux.LabelWorkspaceName]
	if !strings.HasPrefix(crName, "ws-") {
		return nil, fmt.Errorf("workspace-name label %q is not a platform CR name", crName)
	}
	info.SourceWorkspaceID = "ws_" + strings.TrimPrefix(crName, "ws-")
	// The display name lives on the workspaces row (immutable after
	// create); the CR-name annotation is the fallback after DB loss.
	info.SourceWorkspaceName = ann[annotationRetainedSourceWorkspace]
	var name string
	err := s.Records.db.Pool().QueryRow(ctx,
		`SELECT name FROM workspaces WHERE id = $1`, info.SourceWorkspaceID).Scan(&name)
	switch {
	case err == nil && name != "":
		info.SourceWorkspaceName = name
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("lookup source workspace %s: %w", info.SourceWorkspaceID, err)
	}
	if q := pvc.Spec.Resources.Requests.Storage(); q != nil {
		info.SizeBytes = q.Value()
	}
	return info, nil
}
