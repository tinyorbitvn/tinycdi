// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// specSchema compiles components.schemas.<name> of openapi.yaml (OpenAPI 3.1,
// i.e. JSON Schema 2020-12) with format assertions on, so a handler struct
// that drifts from the published contract fails here instead of in the web.
func specSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	// Round-trip through JSON so the compiler sees plain JSON types.
	js, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	const url = "https://tinycdi.invalid/openapi.json"
	if err := c.AddResource(url, parsed); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(url + "#/components/schemas/" + name)
	if err != nil {
		t.Fatalf("compile schema %s: %v", name, err)
	}
	return s
}

// requireValid marshals v exactly as a handler writes it and validates the
// JSON against the named openapi schema.
func requireValid(t *testing.T, schema string, v any) {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := specSchema(t, schema).Validate(inst); err != nil {
		t.Fatalf("%s does not match openapi.yaml: %v\nbody: %s", schema, err, body)
	}
}

func contractTemplate(clipboard string) templateView {
	built := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	stale := false
	return templateView{
		ID:          "tpl_01J4ZB3N1RXD7P2V8W5K0H6Q4M",
		Name:        "linux-firefox-desktop",
		Family:      "linux-firefox-desktop",
		Description: "Firefox on a Linux desktop",
		Revision:    7,
		Runtime:     "LinuxContainer",
		Experience:  "Desktop",
		Resources:   templateResources{CPUMillis: 4000, MemoryMiB: 8192, StorageGiB: 20},
		LifecycleDefaults: lifecycleDefaults{
			IdleTimeoutSeconds: 1800, DisconnectGraceSeconds: 600, MaxRunningSeconds: 28800,
		},
		DataPolicyDefault: "Ephemeral",
		ClipboardPolicy:   clipboard,
		NetworkProfile:    "InternetOnly",
		PublishedAt:       built,
		ImageBuiltAt:      &built,
		ImageStale:        &stale,
	}
}

func TestOpenAPIContract_TemplateView(t *testing.T) {
	for _, clip := range []string{"Disabled", "Send", "Receive", "Bidirectional"} {
		t.Run("clipboard="+clip, func(t *testing.T) {
			requireValid(t, "TemplateView", contractTemplate(clip))
			requireValid(t, "TemplateList", templateList{Items: []templateView{contractTemplate(clip)}})
		})
	}
	t.Run("clipboard=unknown is rejected", func(t *testing.T) {
		body, _ := json.Marshal(contractTemplate("Enabled"))
		inst, _ := jsonschema.UnmarshalJSON(bytes.NewReader(body))
		if err := specSchema(t, "TemplateView").Validate(inst); err == nil {
			t.Fatal("clipboardPolicy Enabled must not be in the openapi enum")
		}
	})
}

func TestOpenAPIContract_SessionProbe(t *testing.T) {
	requireValid(t, "SessionProbe", sessionProbeView{Authenticated: true})
	requireValid(t, "SessionProbe", sessionProbeView{Authenticated: false})
	t.Run("extra keys are rejected", func(t *testing.T) {
		inst, _ := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(`{"authenticated":true,"subject":"alice"}`)))
		if err := specSchema(t, "SessionProbe").Validate(inst); err == nil {
			t.Fatal("SessionProbe must be exactly {authenticated}")
		}
	})
}

func TestOpenAPIContract_WorkspaceView(t *testing.T) {
	now := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	stale := true
	view := WorkspaceView{
		ID:    "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E",
		Name:  "research-desktop",
		Owner: Owner{Subject: "user-a", DisplayName: "User A"},
		Template: templateSummary{
			ID: "tpl_01J4ZB3N1RXD7P2V8W5K0H6Q4M", Name: "linux-firefox-desktop",
			Family: "linux-firefox-desktop", Revision: 7,
			Runtime: "LinuxContainer", Experience: "Desktop",
		},
		Phase: "Ready",
		Conditions: []workspaceCondition{{
			Type: "RuntimeReady", Status: "True", Reason: "PodReady", LastTransitionTime: now,
		}},
		DesiredState:    "Running",
		DataPolicy:      "Retain",
		RetainedDataRef: "rd_01J4Z8KQ2M9XNBV3T7YH0R6D5E",
		CreatedAt:       now,
		UpdatedAt:       now,
		ImageBuiltAt:    &now,
		ImageStale:      &stale,
	}
	requireValid(t, "WorkspaceView", view)
	requireValid(t, "WorkspaceList", WorkspaceList{Items: []WorkspaceView{view}})
}

func TestOpenAPIContract_QuotaView(t *testing.T) {
	usage := quotaAmounts{Workspaces: 2, RunningWorkspaces: 1, CPUMillicores: 4000, MemoryMib: 8192, StorageGib: 40}
	users := []userUsage{{Subject: "user-a", DisplayName: "User A", Usage: usage}}
	t.Run("with limits", func(t *testing.T) {
		limits := quotaAmounts{RunningWorkspaces: 4, CPUMillicores: 16000, MemoryMib: 32768, StorageGib: 200}
		requireValid(t, "QuotaView", quotaView{Tenant: "acme", Configured: true, Limits: &limits, Usage: usage, Users: users})
	})
	t.Run("without limits", func(t *testing.T) {
		requireValid(t, "QuotaView", quotaView{Tenant: "acme", Usage: usage, Users: users})
	})
	t.Run("configured is required", func(t *testing.T) {
		raw := `{"tenant":"acme","usage":{"workspaces":0,"runningWorkspaces":0,"cpuMillicores":0,"memoryMib":0,"storageGib":0},"users":[]}`
		inst, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(raw)))
		if err != nil {
			t.Fatal(err)
		}
		if err := specSchema(t, "QuotaView").Validate(inst); err == nil {
			t.Fatal("QuotaView without configured validates; the field must be required so a client never reads 'absent' as 'unlimited'")
		}
	})
}

// TestOpenAPIContract_ConnectionStatus (R9a): leaseRef and streamEpoch are in
// the published contract; leaseRef is the 16-hex lease reference, absent
// without an active lease.
func TestOpenAPIContract_ConnectionStatus(t *testing.T) {
	renewed := time.Date(2026, 10, 2, 5, 0, 0, 0, time.UTC)
	t.Run("active lease", func(t *testing.T) {
		requireValid(t, "ConnectionStatus", ConnectionStatus{
			State: "connected", LeaseActive: true, LastRenewedAt: &renewed,
			LeaseRef: "0123456789abcdef", StreamEpoch: 3,
		})
	})
	t.Run("no lease", func(t *testing.T) {
		requireValid(t, "ConnectionStatus", ConnectionStatus{State: "none"})
	})
	t.Run("leaseRef is 16 hex chars", func(t *testing.T) {
		inst, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(
			`{"state":"connected","leaseActive":true,"leaseRef":"not-hex","streamEpoch":1}`)))
		if err != nil {
			t.Fatal(err)
		}
		if err := specSchema(t, "ConnectionStatus").Validate(inst); err == nil {
			t.Fatal("a leaseRef that is not 16 hex chars validates")
		}
	})
}

func TestOpenAPIContract_WorkspaceEventList(t *testing.T) {
	ts := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	items := []WorkspaceEvent{{
		ID: "StartRequested.4", Type: "Normal", Reason: "StartRequested",
		Message: "Starting the workspace was requested.", Count: 1, FirstTimestamp: &ts, LastTimestamp: &ts,
	}}
	requireValid(t, "WorkspaceEventList", WorkspaceEventList{Items: items})
	requireValid(t, "WorkspaceEventList", WorkspaceEventList{Items: items, Stale: true})
}

// GET /v1/data/{dataId} answers with the same record GET /v1/data lists.
func TestOpenAPIContract_RetainedDataView(t *testing.T) {
	view := retainedDataView{
		ID: "rd_01J4Z8KQ2M9XNBV3T7YH0R6D5E", State: "Attached",
		Owner:                  Owner{Subject: "user-a", DisplayName: "User A"},
		SizeGiB:                20,
		Runtime:                "LinuxContainer",
		SourceWorkspaceName:    "research-desktop",
		ConsumingWorkspaceID:   "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E",
		RetainedAt:             time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC),
		PurgeConfirmationNonce: "nonce",
	}
	requireValid(t, "RetainedDataView", view)
	requireValid(t, "RetainedDataList", retainedDataList{Items: []retainedDataView{view}})
}

func TestOpenAPIContract_GetRetainedDataPath(t *testing.T) {
	raw, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			Parameters []map[string]any          `yaml:"parameters"`
			Responses  map[string]map[string]any `yaml:"responses"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	op, ok := spec.Paths["/v1/data/{dataId}"]["get"]
	if !ok {
		t.Fatal("GET /v1/data/{dataId} missing from openapi.yaml")
	}
	if len(op.Parameters) != 1 {
		t.Fatalf("GET /v1/data/{dataId} parameters = %v, want the DataId path parameter", op.Parameters)
	}
	for _, code := range []string{"200", "404"} {
		if _, ok := op.Responses[code]; !ok {
			t.Fatalf("GET /v1/data/{dataId} lacks a %s response", code)
		}
	}
}
