// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// TenantQuota is one declared tenant limit: the Helm chart renders the
// managedNamespaces[].quota blocks into the backend's -tenant-quotas flag and
// the leader upserts them into tenant_quota at startup.
type TenantQuota struct {
	TenantID string
	Limits   ResourceVector
}

// quantity is a JSON string or number holding a Kubernetes quantity. The
// chart emits strings; a bare YAML number (cpu: 4) renders as a JSON number.
type quantity string

func (q *quantity) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*q = quantity(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return errors.New("want a Kubernetes quantity string or number")
	}
	*q = quantity(n.String())
	return nil
}

// tenantQuotaJSON is the wire shape. Every limit is required: a partial
// block would silently mean "none allowed" for the missing dimension.
type tenantQuotaJSON struct {
	Tenant            string    `json:"tenant"`
	RunningWorkspaces *float64  `json:"runningWorkspaces"`
	CPU               *quantity `json:"cpu"`
	Memory            *quantity `json:"memory"`
	Storage           *quantity `json:"storage"`
}

// ParseTenantQuotas decodes the -tenant-quotas JSON: an array of
// {tenant, runningWorkspaces, cpu, memory, storage}. cpu is cores or
// millicores ("4", "1.5", "500m"); memory and storage are Kubernetes
// quantities ("8Gi", "100G"). Blank input, "[]" and "null" mean no quotas.
// The result is sorted by tenant. Unknown fields, duplicate tenants and
// negative or unparsable amounts are errors — nothing is guessed.
func ParseTenantQuotas(raw string) ([]TenantQuota, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var entries []tenantQuotaJSON
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("want a JSON array of {tenant, runningWorkspaces, cpu, memory, storage}: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after the JSON array")
	}

	seen := map[string]bool{}
	out := make([]TenantQuota, 0, len(entries))
	for i, e := range entries {
		if e.Tenant == "" {
			return nil, fmt.Errorf("entry %d: tenant is required", i)
		}
		if seen[e.Tenant] {
			return nil, fmt.Errorf("tenant %q is listed twice", e.Tenant)
		}
		seen[e.Tenant] = true
		q, err := e.toQuota()
		if err != nil {
			return nil, fmt.Errorf("tenant %q: %w", e.Tenant, err)
		}
		out = append(out, q)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TenantID < out[j].TenantID })
	return out, nil
}

func (e tenantQuotaJSON) toQuota() (TenantQuota, error) {
	q := TenantQuota{TenantID: e.Tenant}
	if e.RunningWorkspaces == nil {
		return q, errors.New("runningWorkspaces is required")
	}
	w := *e.RunningWorkspaces
	if w < 0 || w != math.Trunc(w) || w > math.MaxInt32 {
		return q, fmt.Errorf("runningWorkspaces must be a whole number >= 0, got %v", w)
	}
	q.Limits.RunningSlots = int64(w)

	cpu, err := parseQuantity("cpu", e.CPU)
	if err != nil {
		return q, err
	}
	if cpu.Format != resource.DecimalSI {
		return q, fmt.Errorf("cpu %q must be cores or millicores (e.g. 4, 1.5, 500m)", cpu.String())
	}
	q.Limits.CPUMillis = cpu.MilliValue()

	mem, err := parseQuantity("memory", e.Memory)
	if err != nil {
		return q, err
	}
	q.Limits.MemoryBytes = mem.Value()

	disk, err := parseQuantity("storage", e.Storage)
	if err != nil {
		return q, err
	}
	q.Limits.DiskBytes = disk.Value()
	return q, nil
}

func parseQuantity(field string, in *quantity) (resource.Quantity, error) {
	if in == nil {
		return resource.Quantity{}, fmt.Errorf("%s is required", field)
	}
	q, err := resource.ParseQuantity(string(*in))
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("%s %q is not a Kubernetes quantity", field, string(*in))
	}
	if q.Sign() < 0 {
		return resource.Quantity{}, fmt.Errorf("%s %q must not be negative", field, string(*in))
	}
	return q, nil
}

// UpsertQuota writes limit for tenantID and reports whether it changed the
// row. An identical row is left untouched (updated_at included), so repeated
// passes — every restart, every leader hand-over — are no-ops. Limits below
// current usage are accepted: held reservations stay, new ones are refused.
func UpsertQuota(ctx context.Context, tx store.Tx, tenantID string, limit ResourceVector) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO tenant_quota (tenant_id, max_running_slots, max_cpu_millis, max_memory_bytes, max_disk_bytes)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id) DO UPDATE SET
			max_running_slots = EXCLUDED.max_running_slots,
			max_cpu_millis    = EXCLUDED.max_cpu_millis,
			max_memory_bytes  = EXCLUDED.max_memory_bytes,
			max_disk_bytes    = EXCLUDED.max_disk_bytes,
			updated_at        = now()
		WHERE (tenant_quota.max_running_slots, tenant_quota.max_cpu_millis,
		       tenant_quota.max_memory_bytes, tenant_quota.max_disk_bytes)
		      IS DISTINCT FROM
		      (EXCLUDED.max_running_slots, EXCLUDED.max_cpu_millis,
		       EXCLUDED.max_memory_bytes, EXCLUDED.max_disk_bytes)`,
		tenantID, limit.RunningSlots, limit.CPUMillis, limit.MemoryBytes, limit.DiskBytes)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ApplyTenantQuotas upserts exactly the listed tenants in one transaction and
// returns how many rows it created or changed. Tenants not listed are never
// read or written. quotas arrive sorted from ParseTenantQuotas; the sort here
// keeps the row-lock order stable for any caller.
func ApplyTenantQuotas(ctx context.Context, db *store.DB, quotas []TenantQuota) (int, error) {
	if len(quotas) == 0 {
		return 0, nil
	}
	sorted := append([]TenantQuota(nil), quotas...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].TenantID < sorted[j].TenantID })
	changed := 0
	err := db.WithTx(ctx, func(tx store.Tx) error {
		changed = 0
		for _, q := range sorted {
			did, err := UpsertQuota(ctx, tx, q.TenantID, q.Limits)
			if err != nil {
				return fmt.Errorf("tenant %q: %w", q.TenantID, err)
			}
			if did {
				changed++
			}
		}
		return nil
	})
	return changed, err
}
