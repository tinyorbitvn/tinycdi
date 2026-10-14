// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// AuditSource reads persisted audit events (store.DB).
type AuditSource interface {
	ListAudit(ctx context.Context, f store.AuditFilter, limit int, after *store.AuditCursor) ([]store.AuditRecord, error)
}

type adminAuditEntry struct {
	ID         string            `json:"id"`
	CreatedAt  time.Time         `json:"createdAt"`
	Actor      string            `json:"actor"`
	Action     string            `json:"action"`
	TargetKind string            `json:"targetKind"`
	TargetID   string            `json:"targetId"`
	Outcome    string            `json:"outcome"`
	ErrorCode  string            `json:"errorCode,omitempty"`
	RequestID  string            `json:"requestId"`
	Details    map[string]string `json:"details"`
}

// AdminAuditHandler serves GET /v1/admin/tenants/{tenant}/audit (ADR 0011).
type AdminAuditHandler struct {
	source  AuditSource
	tenants TenantResolver
	audit   observability.AuditSink
}

func NewAdminAuditHandler(src AuditSource, t TenantResolver) *AdminAuditHandler {
	return &AdminAuditHandler{source: src, tenants: t}
}

func (h *AdminAuditHandler) WithAuditSink(s observability.AuditSink) *AdminAuditHandler {
	h.audit = s
	return h
}

func MountAdminAuditRoutes(mux RouteMux, authn *Authenticator, h *AdminAuditHandler) {
	mux.Handle(routeAdminAuditGet, authn.RequireAuthPassive(
		audited(h.audit, routeAdminAuditGet, http.HandlerFunc(h.List))))
}

var auditActionPattern = regexp.MustCompile(`^[a-z0-9_.-]{1,128}$`)

func (h *AdminAuditHandler) List(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantAdminPrincipal(h.tenants, w, r); !ok {
		return
	}
	page, perr := ParsePage(r, []string{"createdAt"}, "-createdAt")
	if perr != nil {
		WriteError(w, r, perr)
		return
	}
	if !page.Sort.Desc {
		writeError(w, r, CodeInvalidRequest, "audit lists newest first only (sort=-createdAt)")
		return
	}
	q := r.URL.Query()
	f := store.AuditFilter{Tenant: r.PathValue("tenant")}
	if v := q.Get("action"); v != "" {
		if !auditActionPattern.MatchString(v) {
			writeError(w, r, CodeInvalidRequest, "bad action filter")
			return
		}
		f.Action = v
	}
	if v := q.Get("actor"); v != "" {
		switch {
		case v == "anonymous" || (strings.HasPrefix(v, "oidc:") && len(v) <= 64):
			f.ActorRef = v
		case validOwnerRef(v) == "":
			f.ActorRef = targetActorRef(v)
		default:
			writeError(w, r, CodeInvalidRequest, "actor must be an actor reference or an issuer|sub owner reference")
			return
		}
	}
	if v := q.Get("target"); v != "" {
		if len(v) > 256 {
			writeError(w, r, CodeInvalidRequest, "bad target filter")
			return
		}
		f.TargetID = v
	}
	for _, p := range []struct {
		key string
		dst **time.Time
	}{{"since", &f.Since}, {"until", &f.Until}} {
		if v := q.Get(p.key); v != "" {
			ts, err := time.Parse(time.RFC3339, v)
			if err != nil {
				writeError(w, r, CodeInvalidRequest, p.key+" must be RFC 3339")
				return
			}
			*p.dst = &ts
		}
	}
	if f.Since != nil && f.Until != nil && !f.Since.Before(*f.Until) {
		writeError(w, r, CodeInvalidRequest, "since must be before until")
		return
	}
	var after *store.AuditCursor
	if page.After != nil {
		at, err1 := time.Parse(time.RFC3339Nano, page.After.Key)
		id, err2 := strconv.ParseInt(page.After.ID, 10, 64)
		if err1 != nil || err2 != nil {
			writeError(w, r, CodeInvalidRequest, "bad cursor")
			return
		}
		after = &store.AuditCursor{CreatedAt: at, ID: id}
	}
	rows, err := h.source.ListAudit(r.Context(), f, page.Limit, after)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	out := Page[adminAuditEntry]{Items: make([]adminAuditEntry, 0, len(rows))}
	if len(rows) > page.Limit {
		last := rows[page.Limit-1]
		out.NextCursor = NextCursor(page.Sort, last.CreatedAt.UTC().Format(time.RFC3339Nano), strconv.FormatInt(last.ID, 10))
		rows = rows[:page.Limit]
	}
	for _, rec := range rows {
		details := rec.Details
		if details == nil {
			details = map[string]string{} // the schema requires an object, never null
		}
		out.Items = append(out.Items, adminAuditEntry{
			ID: strconv.FormatInt(rec.ID, 10), CreatedAt: rec.CreatedAt, Actor: rec.ActorRef, Action: rec.Action,
			TargetKind: rec.TargetKind, TargetID: rec.TargetID, Outcome: rec.Outcome, ErrorCode: rec.ErrorCode,
			RequestID: rec.RequestID, Details: details,
		})
	}
	respondJSON(w, out)
}
