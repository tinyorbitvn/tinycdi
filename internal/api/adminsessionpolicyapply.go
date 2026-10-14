// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

const (
	// maxPolicyApply bounds the affected rows one :apply call lists and
	// restarts; the response says truncated when more qualify.
	maxPolicyApply = 500
	// maxPolicyScan bounds the running workspaces one call examines (one
	// query). Compliant workspaces never become affected, so the scan must
	// not stop at the first 500 running ones: oldest-first paging by running
	// count alone could hide looser workspaces behind a compliant prefix.
	maxPolicyScan = 5000
)

// PolicyApplyBackend is the provisioning and store surface :apply uses.
type PolicyApplyBackend interface {
	ListRunningWorkspaces(ctx context.Context, tenantID string, limit int) ([]provisioning.WorkspaceRecord, error)
	ResolveSessionPolicy(ctx context.Context, tenantID string, tpl provisioning.TemplateCatalogEntry) (provisioning.SessionPolicy, error)
	RestartWorkspace(ctx context.Context, tenantID, caller, ownerScope, wsID, idemKey string, fromGeneration int64, bodyHash []byte) (provisioning.WorkspaceRecord, error)
	HasLiveLease(ctx context.Context, workspaceID string, now time.Time) (bool, error)
}

// RunningPolicySource is the informer view of applied policies (*K8sStatusView).
type RunningPolicySource interface {
	RunningPolicies(ctx context.Context, tenantID string) (map[string]AppliedPolicy, bool, error)
}

type applyRequest struct {
	DryRun        *bool `json:"dryRun"`
	IncludeActive bool  `json:"includeActive"`
}

type applyAffected struct {
	WorkspaceID string                     `json:"workspaceId"`
	Name        string                     `json:"name"`
	Owner       Owner                      `json:"owner"`
	Applied     provisioning.SessionPolicy `json:"applied"`
	Effective   provisioning.SessionPolicy `json:"effective"`
	Live        bool                       `json:"live"`
	Result      string                     `json:"result"`
}

type applyResult struct {
	DryRun    bool            `json:"dryRun"`
	Truncated bool            `json:"truncated"`
	Affected  []applyAffected `json:"affected"`
}

// WithApply enables POST …/session-policy:apply. dir resolves owner display
// names (nil degrades to the subject).
func (h *AdminSessionPolicyHandler) WithApply(be PolicyApplyBackend, running RunningPolicySource, dir Directory) *AdminSessionPolicyHandler {
	h.apply, h.running, h.dir = be, running, dir
	return h
}

// Apply restarts the tenant's running workspaces whose applied session
// policy grants more than the current ceiling allows (F3). dryRun lists
// them; the confirmed call restarts each through RestartWorkspace, fenced
// on the generation the CR reports, skipping live sessions unless
// includeActive. If-Match pins the ceiling version the admin reviewed.
// Tenant isolation: the tenant comes from the verified principal (the path
// value must equal it) and every query below is scoped to it.
func (h *AdminSessionPolicyHandler) Apply(w http.ResponseWriter, r *http.Request) {
	p, ok := tenantAdminPrincipal(h.tenants, w, r)
	if !ok {
		return
	}
	if h.apply == nil || h.running == nil {
		writeError(w, r, CodeUnavailable, "policy apply unavailable")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
	if err != nil {
		writeError(w, r, CodeInvalidRequest, "unreadable or oversized body")
		return
	}
	var req applyRequest
	if decodeJSON(body, &req) != nil || req.DryRun == nil {
		writeError(w, r, CodeInvalidRequest, "body must be {\"dryRun\": bool, \"includeActive\"?: bool}")
		return
	}
	ctx := r.Context()
	tenant := p.TenantID
	cur, err := h.source.Ceiling(ctx, tenant)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if e := CheckIfMatch(r, cur.Version, cur.HasRow); e != nil {
		WriteError(w, r, e)
		return
	}
	if !cur.HasRow {
		writeError(w, r, CodeInvalidState, "no ceiling is set; every workspace already follows its template")
		return
	}
	applied, fresh, err := h.running.RunningPolicies(ctx, tenant)
	if err != nil || !fresh {
		writeError(w, r, CodeUnavailable, "workspace status is out of date; retry shortly")
		return
	}
	recs, err := h.apply.ListRunningWorkspaces(ctx, tenant, maxPolicyScan+1)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	out := applyResult{DryRun: *req.DryRun, Affected: []applyAffected{}}
	if len(recs) > maxPolicyScan {
		out.Truncated = true
		recs = recs[:maxPolicyScan]
	}
	// One resolution per distinct template policy pair: the ceiling is read
	// once per pair, and every workspace of a pair is judged against the
	// same value.
	type tplKey struct{ clipboard, fileTransfer string }
	effective := map[tplKey]provisioning.SessionPolicy{}
	now := time.Now()
	restarted := 0
	for _, rec := range recs {
		cr, ok := applied[rec.ID]
		if !ok {
			continue // no CR yet: the create intent stamps the current policy
		}
		k := tplKey{rec.Template.ClipboardPolicy, rec.Template.FileTransferPolicy}
		eff, seen := effective[k]
		if !seen {
			eff, err = h.apply.ResolveSessionPolicy(ctx, tenant, provisioning.TemplateCatalogEntry{
				ClipboardPolicy: k.clipboard, FileTransferPolicy: k.fileTransfer,
			})
			if err != nil {
				writeStoreError(w, r, err)
				return
			}
			effective[k] = eff
		}
		if provisioning.PolicyWithin(cr.Policy, eff) {
			continue
		}
		if len(out.Affected) == maxPolicyApply {
			out.Truncated = true
			break
		}
		live, err := h.apply.HasLiveLease(ctx, rec.ID, now)
		if err != nil {
			writeStoreError(w, r, err)
			return
		}
		a := applyAffected{
			WorkspaceID: rec.ID, Name: rec.Name,
			Owner:   Owner{Subject: ownerRefSubject(rec.Owner), DisplayName: ownerRefSubject(rec.Owner)},
			Applied: cr.Policy, Effective: eff, Live: live, Result: "pending",
		}
		switch {
		case out.DryRun:
		case !cr.IntentSettled:
			// A restart or stop/start is applied to the CR but the operator has
			// not reconciled it: the generation is already moving.
			a.Result = "skipped_moved"
		case live && !req.IncludeActive:
			a.Result = "skipped_active"
		default:
			got, err := h.apply.RestartWorkspace(ctx, tenant, p.Owner(), "", rec.ID, "", cr.RuntimeGeneration, nil)
			switch {
			case err != nil:
				a.Result = "failed"
			case got.RuntimeGeneration != cr.RuntimeGeneration+1:
				a.Result = "skipped_moved" // another restart moved the generation first
			default:
				a.Result = "restarted"
				restarted++
			}
		}
		out.Affected = append(out.Affected, a)
	}
	// Real display names for the (at most 500) owners, one directory lookup.
	refs := make([]string, 0, len(out.Affected))
	byRef := make(map[string]string, len(out.Affected))
	for _, rec := range recs {
		byRef[rec.ID] = rec.Owner
	}
	for _, a := range out.Affected {
		refs = append(refs, byRef[a.WorkspaceID])
	}
	owners := resolveOwners(ctx, h.dir, tenant, refs)
	for i := range out.Affected {
		out.Affected[i].Owner = owners[byRef[out.Affected[i].WorkspaceID]]
	}
	auditSetDetail(ctx, "dryRun", strconv.FormatBool(out.DryRun))
	auditSetDetail(ctx, "includeActive", strconv.FormatBool(req.IncludeActive))
	auditSetDetail(ctx, "affected", strconv.Itoa(len(out.Affected)))
	auditSetDetail(ctx, "restarted", strconv.Itoa(restarted))
	respondJSON(w, out)
}
