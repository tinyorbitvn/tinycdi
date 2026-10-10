// S3x assertions for the optional ValidatingAdmissionPolicy guarding the
// operator-owned workspaces.cdi.tinyorbit.vn/template-snapshot mirror
// annotation (admissionPolicy.enabled, default OFF).
//
// Helpers are shared with chart_test.go (same package).
package chart_test

import (
	"strings"
	"testing"
)

const (
	vapKind    = "ValidatingAdmissionPolicy"
	vapbKind   = "ValidatingAdmissionPolicyBinding"
	snapAnnot  = "workspaces.cdi.tinyorbit.vn/template-snapshot"
	operatorSA = "system:serviceaccount:tcdi-system:operator"
)

func vapSpec(t *testing.T, docs []doc) map[string]any {
	t.Helper()
	vaps := selectDocs(docs, vapKind)
	if len(vaps) != 1 {
		t.Fatalf("expected exactly 1 ValidatingAdmissionPolicy, got %d", len(vaps))
	}
	spec, _ := vaps[0]["spec"].(map[string]any)
	if spec == nil {
		t.Fatal("ValidatingAdmissionPolicy has no spec")
	}
	return spec
}

// TestAdmissionPolicyOffByDefault: neither the default render nor any ci
// fixture that leaves admissionPolicy.enabled unset may emit the objects.
func TestAdmissionPolicyOffByDefault(t *testing.T) {
	for _, vf := range lintValues {
		if vf == "admissionpolicy-values.yaml" {
			continue
		}
		docs := render(t, vf)
		if n := len(selectDocs(docs, vapKind)) + len(selectDocs(docs, vapbKind)); n != 0 {
			t.Errorf("%s: rendered %d admission policy objects with admissionPolicy.enabled unset", vf, n)
		}
	}
}

// TestAdmissionPolicyObjects: the enabled render emits exactly one policy
// and one binding, Deny-actioned, failing closed, matched only to
// workspaces CREATE/UPDATE in the managedNamespaces.
func TestAdmissionPolicyObjects(t *testing.T) {
	docs := render(t, "admissionpolicy-values.yaml")

	spec := vapSpec(t, docs)
	if spec["failurePolicy"] != "Fail" {
		t.Errorf("failurePolicy = %v, want Fail (the guard must fail closed)", spec["failurePolicy"])
	}

	mc, _ := spec["matchConstraints"].(map[string]any)
	rules := toSlice(mc["resourceRules"])
	if len(rules) != 1 {
		t.Fatalf("expected exactly 1 resourceRule, got %d", len(rules))
	}
	rule, _ := rules[0].(map[string]any)
	if got := toSlice(rule["apiGroups"]); len(got) != 1 || got[0] != "workspaces.cdi.tinyorbit.vn" {
		t.Errorf("apiGroups = %v, want [workspaces.cdi.tinyorbit.vn]", got)
	}
	if got := toSlice(rule["apiVersions"]); len(got) != 1 || got[0] != "v1alpha1" {
		t.Errorf("apiVersions = %v, want [v1alpha1]", got)
	}
	if got := toSlice(rule["resources"]); len(got) != 1 || got[0] != "workspaces" {
		t.Errorf("resources = %v, want [workspaces] (not workspaces/status)", got)
	}
	ops := map[string]bool{}
	for _, o := range toSlice(rule["operations"]) {
		ops[o.(string)] = true
	}
	if !ops["CREATE"] || !ops["UPDATE"] || len(ops) != 2 {
		t.Errorf("operations = %v, want exactly CREATE+UPDATE", rule["operations"])
	}

	conds := toSlice(spec["matchConditions"])
	if len(conds) != 1 {
		t.Fatalf("expected exactly 1 matchCondition (managed namespaces), got %d", len(conds))
	}
	cond, _ := conds[0].(map[string]any)
	expr, _ := cond["expression"].(string)
	for _, ns := range managedNamespaces {
		if !strings.Contains(expr, "'"+ns+"'") {
			t.Errorf("matchCondition %q does not name managed namespace %q", expr, ns)
		}
	}
	if !strings.Contains(expr, "namespace") || !strings.Contains(expr, " in ") {
		t.Errorf("matchCondition %q is not a namespace membership check", expr)
	}
}

// TestAdmissionPolicyExpressions: the CEL must exempt the operator
// ServiceAccount (system:serviceaccount:<release-ns>:operator — the chart
// names the SA "operator" in Release.Namespace) plus any
// extraAllowedUsernames, deny a CREATE carrying the annotation, and deny
// an UPDATE whose annotation value differs from oldObject's.
func TestAdmissionPolicyExpressions(t *testing.T) {
	docs := render(t, "admissionpolicy-values.yaml")
	spec := vapSpec(t, docs)
	vals := toSlice(spec["validations"])
	if len(vals) != 2 {
		t.Fatalf("expected 2 validations (create + update), got %d", len(vals))
	}
	var createExpr, updateExpr string
	for _, v := range vals {
		m, _ := v.(map[string]any)
		e, _ := m["expression"].(string)
		if strings.Contains(e, "oldObject") {
			updateExpr = e
		} else {
			createExpr = e
		}
	}
	if createExpr == "" || updateExpr == "" {
		t.Fatalf("expected one create-scoped and one oldObject-comparing validation, got %v", vals)
	}
	for _, u := range []string{operatorSA, "system:serviceaccount:gitops-system:workspace-restore"} {
		if !strings.Contains(createExpr, "'"+u+"'") || !strings.Contains(updateExpr, "'"+u+"'") {
			t.Errorf("allowed identity %q missing from a validation expression", u)
		}
	}
	if !strings.Contains(createExpr, "'"+snapAnnot+"' in") {
		t.Errorf("create validation does not test annotation presence:\n%s", createExpr)
	}
	if !strings.Contains(updateExpr, "oldObject.metadata.annotations") ||
		!strings.Contains(updateExpr, "object.metadata.annotations") ||
		!strings.Contains(updateExpr, snapAnnot) {
		t.Errorf("update validation does not diff oldObject vs object annotation:\n%s", updateExpr)
	}
}

// TestAdmissionPolicyBinding: the binding activates the policy with Deny
// and nothing weaker (no Warn/Audit-only deployment).
func TestAdmissionPolicyBinding(t *testing.T) {
	docs := render(t, "admissionpolicy-values.yaml")
	bindings := selectDocs(docs, vapbKind)
	if len(bindings) != 1 {
		t.Fatalf("expected exactly 1 ValidatingAdmissionPolicyBinding, got %d", len(bindings))
	}
	vapName, _ := meta(selectDocs(docs, vapKind)[0])
	bindName, _ := meta(bindings[0])
	spec, _ := bindings[0]["spec"].(map[string]any)
	if spec["policyName"] != vapName {
		t.Errorf("binding %q policyName = %v, want %q", bindName, spec["policyName"], vapName)
	}
	actions := toSlice(spec["validationActions"])
	if len(actions) != 1 || actions[0] != "Deny" {
		t.Errorf("validationActions = %v, want exactly [Deny]", actions)
	}
}
