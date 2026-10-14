package api

import (
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// The revision a workspace view reports is the template's spec.revision
// label verbatim. spec.revision is required (MinLength 1), so every v1 row
// carries it; the numeric family counter is never shown in its place.
func TestTemplateRevisionOf_LabelOnly(t *testing.T) {
	if got := templateRevisionOf(&provisioning.TemplateInfo{Revision: 3, RevisionLabel: "2026-10-a"}); got != "2026-10-a" {
		t.Fatalf("label row = %q, want 2026-10-a", got)
	}
	if got := templateRevisionOf(&provisioning.TemplateInfo{Revision: 3}); got != "" {
		t.Fatalf("label-less row = %q, want \"\" (no numeric stand-in)", got)
	}
}
