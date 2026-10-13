// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
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
		DataPolicyDefault:  "Ephemeral",
		ClipboardPolicy:    clipboard,
		FileTransferPolicy: "Disabled",
		NetworkProfile:     "InternetOnly",
		PublishedAt:        built,
		ImageBuiltAt:       &built,
		ImageStale:         &stale,
		ImageBlocked:       &stale,
		ImageEngines:       map[string]string{"chromium": "154.0.8037.92", "firefox": "153.4.0esr"},
	}
}

func TestOpenAPIContract_TemplateView(t *testing.T) {
	for _, clip := range []string{"Disabled", "Send", "Receive", "Bidirectional"} {
		t.Run("clipboard="+clip, func(t *testing.T) {
			requireValid(t, "TemplateView", contractTemplate(clip))
			requireValid(t, "TemplatePage", Page[templateView]{Items: []templateView{contractTemplate(clip)}, NextCursor: "c1"})
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
		}, {
			Type: "Degraded", Status: "True", Reason: "CleanupRetry",
			Message: "teardown step cleanup blocked; retrying",
			Params:  map[string]string{"step": "cleanup"}, LastTransitionTime: now,
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
	requireValid(t, "WorkspacePage", Page[WorkspaceView]{Items: []WorkspaceView{view}, NextCursor: "c1"})
}

func TestOpenAPIContract_QuotaView(t *testing.T) {
	usage := quotaAmounts{Workspaces: 2, RunningWorkspaces: 1, CPUMillicores: 4000, MemoryMib: 8192, StorageGib: 40}
	t.Run("with limits", func(t *testing.T) {
		limits := quotaAmounts{RunningWorkspaces: 4, CPUMillicores: 16000, MemoryMib: 32768, StorageGib: 200}
		requireValid(t, "QuotaView", quotaView{Tenant: "acme", Configured: true, Limits: &limits, Usage: usage})
	})
	t.Run("without limits", func(t *testing.T) {
		requireValid(t, "QuotaView", quotaView{Tenant: "acme", Usage: usage})
	})
	t.Run("configured is required", func(t *testing.T) {
		raw := `{"tenant":"acme","usage":{"workspaces":0,"runningWorkspaces":0,"cpuMillicores":0,"memoryMib":0,"storageGib":0}}`
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

func TestOpenAPIContract_WorkspaceEventPage(t *testing.T) {
	ts := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	items := []WorkspaceEvent{{
		ID: "StartRequested.4", Type: "Normal", Reason: "StartRequested",
		Message: "Starting the workspace was requested.", Count: 1, FirstTimestamp: &ts, LastTimestamp: &ts,
		Params: map[string]string{"revision": "4"},
	}}
	requireValid(t, "WorkspaceEventPage", WorkspaceEventPage{Page: Page[WorkspaceEvent]{Items: items}})
	requireValid(t, "WorkspaceEventPage", WorkspaceEventPage{Page: Page[WorkspaceEvent]{Items: items, NextCursor: "c1"}, Stale: true})
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
	requireValid(t, "RetainedDataPage", Page[retainedDataView]{Items: []retainedDataView{view}, NextCursor: "c1"})
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

// The clipboardPolicy direction semantics are defined ONCE, here, from the
// user's point of view — every other layer (CRD enum, gateway client flags,
// entrypoint DLP render) maps onto this text. The test pins the normative
// statements so the contract cannot silently drift, and ties the published
// enum to the CRD's own constants.
func TestClipboardPolicyDirectionSemantics(t *testing.T) {
	raw, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Enum        []string `yaml:"enum"`
					Description string   `yaml:"description"`
				} `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	prop, ok := spec.Components.Schemas["TemplateView"].Properties["clipboardPolicy"]
	if !ok {
		t.Fatal("TemplateView.clipboardPolicy missing from openapi.yaml")
	}

	// The published enum is exactly the CRD enum — no fifth value may
	// appear in one without the other.
	wantEnum := []string{
		string(cdiv1.ClipboardDisabled),
		string(cdiv1.ClipboardSend),
		string(cdiv1.ClipboardReceive),
		string(cdiv1.ClipboardBidirectional),
	}
	if len(prop.Enum) != len(wantEnum) {
		t.Fatalf("clipboardPolicy enum = %v, want %v", prop.Enum, wantEnum)
	}
	for i, v := range wantEnum {
		if prop.Enum[i] != v {
			t.Fatalf("clipboardPolicy enum = %v, want %v", prop.Enum, wantEnum)
		}
	}

	// Each value's documented direction must appear verbatim — the entry
	// point render and the gateway's client flags are pinned against these
	// statements (KasmVNC DLP keys in parentheses). Line wrapping in the
	// block scalar is normalized away first.
	desc := strings.Join(strings.Fields(prop.Description), " ")
	for _, fragment := range []string{
		"`Disabled` allows neither direction",
		"`Send` allows client to workspace only (`client_to_server`)",
		"`Receive` allows workspace to client only (`server_to_client`",
		"`Bidirectional` allows both directions",
		"normative definition of the direction semantics",
	} {
		if !strings.Contains(desc, fragment) {
			t.Errorf("clipboardPolicy description lacks %q:\n%s", fragment, desc)
		}
	}
}

// specOperation is one (method, raw path) pair of openapi.yaml.
type specOperation struct {
	method string // upper case
	path   string // as written in the spec, e.g. /v1/workspaces/{workspaceId}:start
}

// specOperations lists every operation of openapi.yaml with its raw path.
func specOperations(t *testing.T) []specOperation {
	t.Helper()
	raw, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	var out []specOperation
	for path, ops := range spec.Paths {
		for method := range ops {
			switch method {
			case "get", "put", "post", "delete", "patch":
				out = append(out, specOperation{strings.ToUpper(method), path})
			}
		}
	}
	return out
}

// dummyPath fills every {param} of a spec path with a placeholder id,
// keeping a :verb suffix ({workspaceId}:start -> x1:start).
func dummyPath(path string) string {
	segs := strings.Split(path, "/")
	for i, seg := range segs {
		if strings.HasPrefix(seg, "{") {
			_, suffix, _ := strings.Cut(seg, "}")
			segs[i] = "x1" + suffix
		}
	}
	return strings.Join(segs, "/")
}

// TestOpenAPI_EveryOperationMountedAndViceVersa pins the published contract
// to the mounted router in both directions: every spec operation resolves
// to a registered route (not the mux's own 404/405), and every pattern the
// Mount* functions register is a spec operation — a :verb dispatcher
// pattern counts when the spec publishes at least one verb under it.
func TestOpenAPI_EveryOperationMountedAndViceVersa(t *testing.T) {
	fx := newCoverageEnv(t, &captureSink{})
	for _, op := range specOperations(t) {
		req := httptest.NewRequest(op.method, dummyPath(op.path), nil)
		if _, pattern := fx.mux.ServeMux.Handler(req); pattern == "" {
			t.Errorf("spec operation %s %s is not mounted", op.method, op.path)
		}
	}

	ops := specOps(t)
	if len(fx.mux.patterns) == 0 {
		t.Fatal("the Mount* functions registered no pattern")
	}
	for _, p := range fx.mux.patterns {
		if !strings.Contains(p, " ") {
			t.Errorf("mounted pattern %q has no method", p)
			continue
		}
		np := normPattern(p)
		if ops[np] {
			continue
		}
		verbs := false
		if strings.HasSuffix(np, "/{}") {
			for op := range ops {
				if rest, ok := strings.CutPrefix(op, np); ok && strings.HasPrefix(rest, ":") {
					verbs = true
				}
			}
		}
		if !verbs {
			t.Errorf("mounted pattern %q is not in openapi.yaml", p)
		}
	}
}

// specDoc parses openapi.yaml once per call into plain JSON types.
func specDoc(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	return doc
}

// jsonPointerEscape escapes one JSON-pointer reference token.
func jsonPointerEscape(s string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(s)
}

// responseSchemaPointer resolves the JSON pointer of the application/json
// schema of (method, path, status) in the spec, following a
// components/responses $ref. ok is false when the operation does not
// declare the status; schema is "" for a declared status without a body.
func responseSchemaPointer(t *testing.T, doc map[string]any, method, path string, status int) (ptr string, ok bool) {
	t.Helper()
	code := strconv.Itoa(status)
	op, _ := doc["paths"].(map[string]any)[path].(map[string]any)[strings.ToLower(method)].(map[string]any)
	if op == nil {
		t.Fatalf("spec has no operation %s %s", method, path)
	}
	resp, _ := op["responses"].(map[string]any)[code].(map[string]any)
	if resp == nil {
		return "", false
	}
	base := "#/paths/" + jsonPointerEscape(path) + "/" + strings.ToLower(method) + "/responses/" + code
	if ref, _ := resp["$ref"].(string); ref != "" {
		base = ref
		name := strings.TrimPrefix(ref, "#/components/responses/")
		resp, _ = doc["components"].(map[string]any)["responses"].(map[string]any)[name].(map[string]any)
	}
	content, _ := resp["content"].(map[string]any)
	if content == nil {
		return "", true
	}
	if _, ok := content["application/json"]; !ok {
		t.Fatalf("%s %s %s declares no application/json body", method, path, code)
	}
	return base + "/content/application~1json/schema", true
}

// requireResponseMatchesSpec validates a recorded response against the
// operation's declared response for its status.
func requireResponseMatchesSpec(t *testing.T, doc map[string]any, method, path string, r *http.Response) {
	t.Helper()
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	ptr, ok := responseSchemaPointer(t, doc, method, path, r.StatusCode)
	if !ok {
		t.Fatalf("%s %s answered %d, which the spec does not declare: %s", method, path, r.StatusCode, body)
	}
	if ptr == "" {
		if len(bytes.TrimSpace(body)) != 0 {
			t.Fatalf("%s %s %d declares no body but got %s", method, path, r.StatusCode, body)
		}
		return
	}
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
	s, err := c.Compile(url + ptr)
	if err != nil {
		t.Fatalf("compile %s: %v", ptr, err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%s %s %d: body is not JSON: %v\n%s", method, path, r.StatusCode, err, body)
	}
	if err := s.Validate(inst); err != nil {
		t.Fatalf("%s %s %d does not match %s: %v\nbody: %s", method, path, r.StatusCode, ptr, err, body)
	}
}

// TestOpenAPIContract_ResponsesValidate drives the mounted router (fakes
// behind every Mount*) through one happy path per list, per resource GET
// and per admin mutation, plus one refusal per operation, and validates
// every body against the operation's declared response schema — the error
// bodies against Error.
func TestOpenAPIContract_ResponsesValidate(t *testing.T) {
	fx := newCoverageEnv(t, &captureSink{})
	env := fx.env
	doc := specDoc(t)
	sess, csrf := login(t, env, "user-a")

	cr := doReq(t, env, sess, csrf, "POST", "/v1/workspaces",
		`{"name":"contract-ws","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-contract-create"})
	if cr.StatusCode >= 300 {
		cr.Body.Close()
		t.Fatalf("create fixture workspace: status=%d", cr.StatusCode)
	}
	wsID := decodeBody[WorkspaceView](t, cr).ID
	// The fake backend records no desired state; the controller would.
	fx.backend.mu.Lock()
	ws := fx.backend.recs[wsID]
	ws.DesiredState = "Running"
	fx.backend.recs[wsID] = ws
	fx.backend.mu.Unlock()
	cr2 := doReq(t, env, sess, csrf, "POST", "/v1/workspaces",
		`{"name":"contract-del","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-contract-create2"})
	if cr2.StatusCode >= 300 {
		cr2.Body.Close()
		t.Fatalf("create second fixture workspace: status=%d", cr2.StatusCode)
	}
	delID := decodeBody[WorkspaceView](t, cr2).ID
	fx.backend.mu.Lock()
	wsDel := fx.backend.recs[delID]
	wsDel.DesiredState = "Running"
	fx.backend.recs[delID] = wsDel
	fx.backend.mu.Unlock()
	rec := seedRetained(fx.retained, "rd_contract0001", "tenant-a", envOwner(env, "user-a"), "LinuxContainer")
	sessA, csrfA := loginAdmin(t, env, "admin-a")
	anon := func(method, path string) *http.Response {
		req, _ := http.NewRequest(method, env.server.URL+path, nil)
		r, err := noRedirectClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	owner := "iss|contract"
	const unknownWS = "ws_00000000000000000000000999" // well-formed, never created
	star := map[string]string{"If-Match": "*"}
	stale := map[string]string{"If-Match": ETagOf("stale")}

	type call struct {
		method, spec string
		do           func() *http.Response
	}
	calls := []call{
		// Lists: a page, then a malformed cursor.
		{"GET", "/v1/workspaces", func() *http.Response { return doReq(t, env, sess, csrf, "GET", "/v1/workspaces", "", nil) }},
		{"GET", "/v1/workspaces", func() *http.Response {
			return doReq(t, env, sess, csrf, "GET", "/v1/workspaces?cursor=%21", "", nil)
		}},
		{"GET", "/v1/templates", func() *http.Response { return doReq(t, env, sess, csrf, "GET", "/v1/templates", "", nil) }},
		{"GET", "/v1/templates", func() *http.Response {
			return doReq(t, env, sess, csrf, "GET", "/v1/templates?limit=0", "", nil)
		}},
		{"GET", "/v1/data", func() *http.Response { return doReq(t, env, sess, csrf, "GET", "/v1/data", "", nil) }},
		{"GET", "/v1/data", func() *http.Response { return doReq(t, env, sess, csrf, "GET", "/v1/data?sort=bogus", "", nil) }},
		{"GET", "/v1/workspaces/{workspaceId}/events", func() *http.Response {
			return doReq(t, env, sess, csrf, "GET", "/v1/workspaces/"+wsID+"/events", "", nil)
		}},
		{"GET", "/v1/workspaces/{workspaceId}/events", func() *http.Response {
			return doReq(t, env, sess, csrf, "GET", "/v1/workspaces/"+unknownWS+"/events", "", nil)
		}},
		{"GET", "/v1/admin/tenants/{tenant}/user-limits", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "GET", userLimitsPath, "", nil)
		}},
		{"GET", "/v1/admin/tenants/{tenant}/user-limits", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "GET", userLimitsPath+"?cursor=%21", "", nil)
		}},
		// One GET per resource, then a refusal.
		{"GET", "/v1/me", func() *http.Response { return doReq(t, env, sess, csrf, "GET", "/v1/me", "", nil) }},
		{"GET", "/v1/me", func() *http.Response { return anon("GET", "/v1/me") }},
		{"GET", "/v1/workspaces/{workspaceId}", func() *http.Response {
			return doReq(t, env, sess, csrf, "GET", "/v1/workspaces/"+wsID, "", nil)
		}},
		{"GET", "/v1/workspaces/{workspaceId}", func() *http.Response {
			return doReq(t, env, sess, csrf, "GET", "/v1/workspaces/"+unknownWS, "", nil)
		}},
		// 400: malformed id, and a missing Idempotency-Key.
		{"GET", "/v1/workspaces/{workspaceId}", func() *http.Response {
			return doReq(t, env, sess, csrf, "GET", "/v1/workspaces/not-an-id", "", nil)
		}},
		{"DELETE", "/v1/workspaces/{workspaceId}", func() *http.Response {
			return doReq(t, env, sess, csrf, "DELETE", "/v1/workspaces/not-an-id", "", nil)
		}},
		{"POST", "/v1/workspaces/{workspaceId}:stop", func() *http.Response {
			return doReq(t, env, sess, csrf, "POST", "/v1/workspaces/"+wsID+":stop", "", map[string]string{"Idempotency-Key": "key-contract-stop"})
		}},
		{"POST", "/v1/workspaces/{workspaceId}:stop", func() *http.Response {
			return doReq(t, env, sess, csrf, "POST", "/v1/workspaces/not-an-id:stop", "", map[string]string{"Idempotency-Key": "key-contract-stop"})
		}},
		{"POST", "/v1/workspaces/{workspaceId}:start", func() *http.Response {
			return doReq(t, env, sess, csrf, "POST", "/v1/workspaces/"+wsID+":start", "", map[string]string{"Idempotency-Key": "key-contract-start"})
		}},
		{"DELETE", "/v1/workspaces/{workspaceId}", func() *http.Response {
			return doReq(t, env, sess, csrf, "DELETE", "/v1/workspaces/"+delID, "", nil)
		}},
		{"POST", "/v1/workspaces/{workspaceId}:start", func() *http.Response {
			return doReq(t, env, sess, csrf, "POST", "/v1/workspaces/"+wsID+":start", "", nil)
		}},
		{"GET", "/v1/data/{dataId}", func() *http.Response { return doReq(t, env, sess, csrf, "GET", "/v1/data/"+rec.ID, "", nil) }},
		{"GET", "/v1/data/{dataId}", func() *http.Response { return doReq(t, env, sess, csrf, "GET", "/v1/data/rd_unknown", "", nil) }},
		{"GET", "/v1/quota", func() *http.Response { return doReq(t, env, sess, csrf, "GET", "/v1/quota", "", nil) }},
		{"GET", "/v1/quota", func() *http.Response { return anon("GET", "/v1/quota") }},
		{"GET", "/v1/admin/tenants/{tenant}/quota", func() *http.Response { return doReq(t, env, sessA, csrfA, "GET", adminQuotaPath, "", nil) }},
		{"GET", "/v1/admin/tenants/{tenant}/quota", func() *http.Response { return doReq(t, env, sess, csrf, "GET", adminQuotaPath, "", nil) }},
		{"GET", "/v1/admin/tenants/{tenant}/user-limits/default", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "GET", userLimitDefaultPath, "", nil)
		}},
		{"GET", "/v1/admin/tenants/{tenant}/user-limits/default", func() *http.Response {
			return doReq(t, env, sess, csrf, "GET", userLimitDefaultPath, "", nil)
		}},
		{"GET", "/v1/admin/tenants/{tenant}/session-policy", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "GET", adminSessionPolicyPath, "", nil)
		}},
		{"GET", "/v1/admin/tenants/{tenant}/session-policy", func() *http.Response {
			return doReq(t, env, sess, csrf, "GET", adminSessionPolicyPath, "", nil)
		}},
		{"GET", "/v1/admin/tenants/{tenant}/audit", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "GET", adminAuditPath, "", nil)
		}},
		{"GET", "/v1/admin/tenants/{tenant}/audit", func() *http.Response {
			return doReq(t, env, sess, csrf, "GET", adminAuditPath, "", nil)
		}},
		{"GET", "/v1/admin/tenants/{tenant}/users", func() *http.Response { return doReq(t, env, sessA, csrfA, "GET", adminUsersPath, "", nil) }},
		{"GET", "/v1/admin/tenants/{tenant}/users", func() *http.Response { return doReq(t, env, sess, csrf, "GET", adminUsersPath, "", nil) }},
		{"GET", "/v1/admin/tenants/{tenant}/users/{ownerRef}", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "GET", adminUserPath("https://idp|alice"), "", nil)
		}},
		{"GET", "/v1/admin/tenants/{tenant}/users/{ownerRef}", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "GET", adminUserPath("https://idp|nobody"), "", nil)
		}},
		{"GET", "/v1/admin/tenants/{tenant}/sessions", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "GET", adminSessionsPath, "", nil)
		}},
		{"GET", "/v1/admin/tenants/{tenant}/sessions", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "GET", adminSessionsPath+"?sort=createdAt", "", nil)
		}},
		{"POST", "/v1/admin/tenants/{tenant}/users/{ownerRef}/sessions:revoke-all", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "POST", adminRevokePath("iss|contract"), "", nil)
		}},
		{"POST", "/v1/admin/tenants/{tenant}/users/{ownerRef}/sessions:revoke-all", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "POST", adminRevokePath("bad"), "", nil)
		}},
		{"GET", "/v1/admin/tenants/{tenant}/overview", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "GET", "/v1/admin/tenants/tenant-a/overview", "", nil)
		}},
		{"GET", "/v1/admin/tenants/{tenant}/overview", func() *http.Response {
			return doReq(t, env, sess, csrf, "GET", "/v1/admin/tenants/tenant-a/overview", "", nil)
		}},
		// One mutation per admin resource, then its stale precondition.
		{"PUT", "/v1/admin/tenants/{tenant}/quota", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "PUT", adminQuotaPath, adminQuotaBody, star)
		}},
		{"PUT", "/v1/admin/tenants/{tenant}/quota", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "PUT", adminQuotaPath, adminQuotaBody, stale)
		}},
		{"PUT", "/v1/admin/tenants/{tenant}/user-limits/{ownerRef}", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "PUT", userLimitPath(owner), `{"limit":2}`, star)
		}},
		{"PUT", "/v1/admin/tenants/{tenant}/user-limits/{ownerRef}", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "PUT", userLimitPath(owner), `{"limit":2}`, stale)
		}},
		{"DELETE", "/v1/admin/tenants/{tenant}/user-limits/{ownerRef}", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "DELETE", userLimitPath(owner), "", stale)
		}},
		{"DELETE", "/v1/admin/tenants/{tenant}/user-limits/{ownerRef}", func() *http.Response {
			g := decodeBody[userLimitPageJSON](t, doReq(t, env, sessA, csrfA, "GET", userLimitsPath, "", nil))
			tag := ""
			for _, it := range g.Items {
				if it.OwnerRef == owner {
					tag = it.ETag
				}
			}
			return doReq(t, env, sessA, csrfA, "DELETE", userLimitPath(owner), "", map[string]string{"If-Match": tag})
		}},
		{"PUT", "/v1/admin/tenants/{tenant}/user-limits/default", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "PUT", userLimitDefaultPath, `{"limit":3}`, star)
		}},
		{"PUT", "/v1/admin/tenants/{tenant}/user-limits/default", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "PUT", userLimitDefaultPath, `{"limit":3}`, stale)
		}},
		{"PUT", "/v1/admin/tenants/{tenant}/session-policy", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "PUT", adminSessionPolicyPath, `{"clipboard":"Disabled"}`, star)
		}},
		{"PUT", "/v1/admin/tenants/{tenant}/session-policy", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "PUT", adminSessionPolicyPath, `{"clipboard":"Disabled"}`, stale)
		}},
		{"POST", "/v1/admin/tenants/{tenant}/session-policy:apply", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "POST", adminSessionPolicyPath+":apply", `{"dryRun":true}`, map[string]string{"If-Match": ETagOf("v1")})
		}},
		{"POST", "/v1/admin/tenants/{tenant}/session-policy:apply", func() *http.Response {
			return doReq(t, env, sessA, csrfA, "POST", adminSessionPolicyPath+":apply", `{"dryRun":true}`, stale)
		}},
	}
	statuses := map[string]map[int]bool{}
	for _, c := range calls {
		r := c.do()
		key := c.method + " " + c.spec
		if statuses[key] == nil {
			statuses[key] = map[int]bool{}
		}
		statuses[key][r.StatusCode] = true
		requireResponseMatchesSpec(t, doc, c.method, c.spec, r)
	}
	// Every exercised operation saw a success and a refusal.
	for key, seen := range statuses {
		ok, refused := false, false
		for st := range seen {
			if st < 300 {
				ok = true
			} else {
				refused = true
			}
		}
		if !ok || !refused {
			t.Errorf("%s: statuses %v, want one success and one refusal", key, seen)
		}
	}
}
