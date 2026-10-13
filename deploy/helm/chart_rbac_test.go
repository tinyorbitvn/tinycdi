package chart_test

import (
	"strings"
	"testing"
)

// The operator reads EndpointSlices (ConnectionReady, F2): list+watch must
// be in its ClusterRole, which the chart binds only inside managed namespaces.
func TestOperatorRoleReadsEndpointSlices(t *testing.T) {
	has := func(v any, want string) bool {
		for _, x := range toSlice(v) {
			if x == want {
				return true
			}
		}
		return false
	}
	for _, d := range selectDocs(render(t, "minimal-values.yaml"), "ClusterRole") {
		if name, _ := meta(d); !strings.HasSuffix(name, "-manager-role") {
			continue
		}
		for _, r := range toSlice(d["rules"]) {
			rm, _ := r.(map[string]any)
			if has(rm["apiGroups"], "discovery.k8s.io") && has(rm["resources"], "endpointslices") &&
				has(rm["verbs"], "list") && has(rm["verbs"], "watch") {
				return
			}
		}
		t.Fatal("operator manager-role has no discovery.k8s.io endpointslices list+watch rule")
	}
	t.Fatal("no manager-role ClusterRole rendered")
}
