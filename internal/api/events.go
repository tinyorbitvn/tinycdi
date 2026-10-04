// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// IntentRecord is one recorded API-side lifecycle step (the outbox's
// create/start/stop/delete history for the workspace).
type IntentRecord struct {
	Kind     string
	Revision uint64
	At       time.Time
	// Reason is the recorded cause of a platform-initiated stop.
	Reason string
}

// IntentLog is the read seam over a workspace's recorded lifecycle intents.
// The Postgres implementation is provisioning.Service.IntentHistory behind
// NewIntentLog.
type IntentLog interface {
	IntentHistory(ctx context.Context, tenantID, workspaceUID string) ([]IntentRecord, error)
}

// serviceIntentLog adapts provisioning.Service to IntentLog.
type serviceIntentLog struct{ svc *provisioning.Service }

// NewIntentLog exposes the service's recorded intent history as an
// IntentLog for the events endpoint.
func NewIntentLog(svc *provisioning.Service) IntentLog { return serviceIntentLog{svc: svc} }

func (l serviceIntentLog) IntentHistory(ctx context.Context, tenantID, workspaceUID string) ([]IntentRecord, error) {
	recs, err := l.svc.IntentHistory(ctx, tenantID, provisioning.PlatformID(workspaceUID))
	if err != nil {
		return nil, err
	}
	out := make([]IntentRecord, 0, len(recs))
	for _, r := range recs {
		out = append(out, IntentRecord{Kind: string(r.Kind), Revision: r.Revision, At: r.CreatedAt, Reason: r.Reason})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Public JSON shapes (must match openapi.yaml exactly).
// ---------------------------------------------------------------------------

// WorkspaceEvent is one curated lifecycle or condition event (openapi
// WorkspaceEvent). Messages are fixed catalog strings — raw Kubernetes or
// operator error text is never forwarded to the portal. Params carries the
// structured values the message (or its event id) interpolates — revision,
// recorded cause, condition type/status, teardown step — so a client can
// localize the full text without parsing English.
type WorkspaceEvent struct {
	// ID is stable across reads: the condition type + reason for observed
	// conditions, the reason + intent revision for lifecycle steps. Clients
	// use it as a list key.
	ID             string            `json:"id"`
	Type           string            `json:"type"` // Normal | Warning
	Reason         string            `json:"reason"`
	Message        string            `json:"message"`
	Params         map[string]string `json:"params,omitempty"`
	Count          int64             `json:"count,omitempty"`
	FirstTimestamp *time.Time        `json:"firstTimestamp,omitempty"`
	LastTimestamp  *time.Time        `json:"lastTimestamp,omitempty"`
}

// WorkspaceEventList is the events response, newest first.
type WorkspaceEventList struct {
	Items []WorkspaceEvent `json:"items"`
	// Stale is true when the observed-state informer cannot prove freshness:
	// condition events then show last-known state and never claim readiness.
	Stale bool `json:"stale,omitempty"`
}

// reasonTokenPattern restricts forwarded reason strings to machine tokens:
// a reason that is not one is replaced so operator-generated free text can
// never reach the portal.
var reasonTokenPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,63}$`)

func curatedReason(raw string) string {
	if reasonTokenPattern.MatchString(raw) {
		return raw
	}
	return "Unknown"
}

// intentEvent maps one recorded lifecycle intent to its curated event.
// Unknown kinds are skipped — forward-compatible with new intent kinds.
// Params carries the intent revision (the same value the event id
// interpolates) and, for platform-initiated stops, the recorded cause.
func intentEvent(in IntentRecord) (WorkspaceEvent, bool) {
	ts := in.At
	ev := WorkspaceEvent{Type: "Normal", FirstTimestamp: &ts, LastTimestamp: &ts}
	switch in.Kind {
	case string(provisioning.IntentCreate):
		ev.Reason, ev.Message = "Created", "The workspace was created."
	case string(provisioning.IntentStart):
		ev.Reason, ev.Message = "StartRequested", "Starting the workspace was requested."
	case string(provisioning.IntentStop):
		ev.Reason, ev.Message = "StopRequested", "Stopping the workspace was requested."
		switch in.Reason {
		case "max_duration":
			ev.Reason, ev.Message = "MaxDurationReached", "The workspace reached its maximum running time and was stopped."
		case "idle_timeout":
			ev.Reason, ev.Message = "IdleTimeout", "The workspace was stopped after the input-idle timeout."
		case "disconnect_timeout":
			ev.Reason, ev.Message = "DisconnectTimeout", "The workspace was stopped after the disconnect grace window."
		}
	case string(provisioning.IntentDelete):
		ev.Reason, ev.Message = "DeleteRequested", "Deleting the workspace was requested."
	default:
		return WorkspaceEvent{}, false
	}
	ev.ID = fmt.Sprintf("%s.%d", ev.Reason, in.Revision)
	ev.Params = map[string]string{"revision": strconv.FormatUint(in.Revision, 10)}
	if in.Kind == string(provisioning.IntentStop) && in.Reason != "" {
		ev.Params["cause"] = in.Reason
	}
	return ev, true
}

// templateSkipMessages maps each recorded guard-skip token (E2) to its
// curated message. The token is named verbatim — it is the event's
// machine-readable cause; raw catalog detail is never forwarded.
var templateSkipMessages = map[string]string{
	provisioning.SkipReasonRuntimeChanged:    "The workspace stayed on its recorded template revision (runtime-changed): the newest published revision uses a different runtime.",
	provisioning.SkipReasonExperienceChanged: "The workspace stayed on its recorded template revision (experience-changed): the newest published revision offers a different experience.",
	provisioning.SkipReasonDataPolicyChanged: "The workspace stayed on its recorded template revision (data-policy-changed): the newest published revision declares a different data policy.",
	provisioning.SkipReasonStorageSmaller:    "The workspace stayed on its recorded template revision (storage-smaller): the newest published revision requests less storage.",
}

// templateSkipEvent surfaces a start intent whose family re-point was
// refused by the compatibility guard (E2): the skip reason is recorded on
// the start intent, and the workspace still started — on its recorded
// revision — so the curated event is emitted alongside StartRequested, not
// in its place.
func templateSkipEvent(in IntentRecord) (WorkspaceEvent, bool) {
	if in.Kind != string(provisioning.IntentStart) {
		return WorkspaceEvent{}, false
	}
	msg, ok := templateSkipMessages[in.Reason]
	if !ok {
		return WorkspaceEvent{}, false
	}
	ts := in.At
	return WorkspaceEvent{
		ID:             fmt.Sprintf("TemplateUpdateSkipped.%d", in.Revision),
		Type:           "Warning",
		Reason:         "TemplateUpdateSkipped",
		Message:        msg,
		Params:         map[string]string{"revision": strconv.FormatUint(in.Revision, 10), "skipReason": in.Reason},
		FirstTimestamp: &ts,
		LastTimestamp:  &ts,
	}, true
}

// conditionEvent maps one observed condition state to its curated event.
// The message comes only from this table — never from the CR's free-text
// message, which may contain node names, image references or other
// platform internals. Params names the condition type and status that
// selected the message plus any structured values the operator recorded
// alongside the condition (teardown step, drain budget).
func conditionEvent(c workspaceCondition) WorkspaceEvent {
	ts := c.LastTransitionTime
	params := map[string]string{
		"condition": c.Type,
		"status":    c.Status,
	}
	maps.Copy(params, c.Params)
	ev := WorkspaceEvent{
		ID:             c.Type + "." + curatedReason(c.Reason),
		Type:           "Normal",
		Reason:         curatedReason(c.Reason),
		Params:         params,
		FirstTimestamp: &ts,
		LastTimestamp:  &ts,
	}
	switch {
	case c.Status == "Unknown":
		ev.Type = "Warning"
	case c.Type == "Degraded" && c.Status == "True":
		ev.Type = "Warning"
	}
	switch c.Type {
	case "Admitted":
		if c.Status == "True" {
			ev.Message = "The platform accepted the workspace request."
		} else {
			ev.Message = "The platform has not accepted the workspace request yet."
		}
	case "StorageReady":
		if c.Status == "True" {
			ev.Message = "Workspace storage is ready."
		} else {
			ev.Message = "Workspace storage is not ready yet."
		}
	case "RuntimeReady":
		if c.Status == "True" {
			ev.Message = "The workspace runtime is ready."
		} else {
			ev.Message = "The workspace runtime is not ready yet."
		}
	case "ConnectionReady":
		if c.Status == "True" {
			ev.Message = "The workspace is ready to connect."
		} else {
			ev.Message = "The workspace is not ready to connect."
		}
	case "Degraded":
		if c.Status == "True" {
			ev.Message = "The workspace reported a problem. If it does not recover, stop and start it again or contact an administrator."
		} else if c.Status == "Unknown" {
			ev.Message = "The workspace's reported state is stale; the last known state is shown."
		} else {
			ev.Message = "The workspace reports no problems."
		}
	default:
		ev.Message = "The workspace reported a status change."
	}
	return ev
}

// WithIntentLog attaches the API-side lifecycle history used by the events
// endpoint. Nil is allowed: events then contain the record's create event
// and condition transitions only.
func (h *WorkspaceHandler) WithIntentLog(l IntentLog) *WorkspaceHandler {
	h.intentLog = l
	return h
}

// Events handles GET /v1/workspaces/{workspaceId}/events: curated events
// from the API's own lifecycle and observed condition transitions, newest
// first. Visibility is exactly the workspace read's: a foreign or unknown
// id is a 404.
func (h *WorkspaceHandler) Events(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principalOrFail(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !workspaceIDPattern.MatchString(id) {
		writeError(w, r, CodeInvalidRequest, "bad workspace id")
		return
	}
	rec, err := h.backend.GetWorkspace(r.Context(), p.TenantID, ownerScope(p), id)
	if err != nil {
		h.writeBackendError(w, r, err)
		return
	}
	var obs ObservedStatus
	if h.statusView != nil {
		if o, err := h.statusView.WorkspaceStatus(r.Context(), p.TenantID, id); err == nil {
			obs = o
		}
	}
	out := WorkspaceEventList{Items: []WorkspaceEvent{}}
	// The API's own lifecycle: recorded intents, plus the create timestamp
	// as a floor when the log is unavailable.
	intents, err := h.intentLogHistory(r.Context(), p.TenantID, id)
	for _, in := range intents {
		if ev, ok := intentEvent(in); ok {
			out.Items = append(out.Items, ev)
		}
		if ev, ok := templateSkipEvent(in); ok {
			out.Items = append(out.Items, ev)
		}
	}
	if err != nil || len(intents) == 0 {
		ts := rec.CreatedAt
		out.Items = append(out.Items, WorkspaceEvent{
			ID: "Created", Type: "Normal", Reason: "Created", Message: "The workspace was created.",
			FirstTimestamp: &ts, LastTimestamp: &ts,
		})
	}
	// Observed condition transitions, messages curated server-side. A stale
	// informer cannot prove any condition is current: the list is flagged
	// stale and shows the degraded last-known state — ConnectionReady never
	// True, and no *Ready condition claims readiness.
	conds := obs.Conditions
	if !obs.Fresh {
		out.Stale = true
		conds = staleConditions(obs)
	}
	if obs.Found || obs.Fresh {
		for _, c := range conds {
			if out.Stale && c.Status == "True" && strings.HasSuffix(c.Type, "Ready") {
				continue
			}
			out.Items = append(out.Items, conditionEvent(c))
		}
	}
	if obs.FailureReason != "" {
		// The failure happened when Degraded=True last transitioned, not
		// when the informer was last read: timestamps stay stable across
		// reads.
		ev := WorkspaceEvent{
			ID:   "Failed." + curatedReason(obs.FailureReason),
			Type: "Warning", Reason: curatedReason(obs.FailureReason),
			Message: "The workspace failed. Stop and start it again; if the failure repeats, contact an administrator.",
		}
		for _, c := range obs.Conditions {
			if c.Type == "Degraded" && c.Status == "True" && !c.LastTransitionTime.IsZero() {
				ts := c.LastTransitionTime
				ev.FirstTimestamp, ev.LastTimestamp = &ts, &ts
				break
			}
		}
		out.Items = append(out.Items, ev)
	}
	sort.SliceStable(out.Items, func(i, j int) bool {
		var a, b time.Time
		if out.Items[i].LastTimestamp != nil {
			a = *out.Items[i].LastTimestamp
		}
		if out.Items[j].LastTimestamp != nil {
			b = *out.Items[j].LastTimestamp
		}
		return a.After(b)
	})
	respondJSON(w, out)
}

// intentLogHistory reads the intent log when wired; err is nil when the
// seam is absent (the caller then falls back to the create event).
func (h *WorkspaceHandler) intentLogHistory(ctx context.Context, tenantID, id string) ([]IntentRecord, error) {
	if h.intentLog == nil {
		return nil, nil
	}
	return h.intentLog.IntentHistory(ctx, tenantID, id)
}
