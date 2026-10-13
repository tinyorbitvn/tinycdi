package v1

import "testing"

func TestFailureClassesAreDisjointAndComplete(t *testing.T) {
	all := []string{FailureBootDeadlineExceeded, FailureFailedCleanup, FailureRuntimeLost,
		FailureImagePolicyDenied, FailureHomeVolumeLost, FailureTemplateRejected}
	for _, r := range all {
		if FailureRecoverable(r) == FailureUnrecoverable(r) {
			t.Errorf("%s must be exactly one of recoverable/unrecoverable", r)
		}
	}
	for _, r := range []string{"", "TemplateInvalid", "Nominal"} {
		if FailureRecoverable(r) || FailureUnrecoverable(r) {
			t.Errorf("%q is not a failure reason", r)
		}
	}
}
