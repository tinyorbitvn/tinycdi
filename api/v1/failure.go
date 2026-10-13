package v1

// Failure reasons are written on the Degraded condition of a Failed
// Workspace. The operator writes them; the API reads them to decide whether
// :start may recover the workspace with a new runtime generation (F1).
// docs/lifecycle-reasons.md explains each one to operators.
const (
	// FailureBootDeadlineExceeded — the incarnation did not become Ready
	// within the template's bootDeadline.
	FailureBootDeadlineExceeded = "BootDeadlineExceeded"
	// FailureFailedCleanup — the failed incarnation was torn down after
	// the cleanup delay.
	FailureFailedCleanup = "FailedCleanup"
	// FailureRuntimeLost — a Ready incarnation's pod disappeared, is
	// terminating, or failed (node loss, eviction, manual delete).
	FailureRuntimeLost = "RuntimeLost"
	// FailureImagePolicyDenied — admission or the registry refused the
	// runtime image by policy. A restart would be refused the same way.
	FailureImagePolicyDenied = "ImagePolicyDenied"
	// FailureHomeVolumeLost — a Retain workspace's home volume is gone. A
	// restart would silently start on an empty home.
	FailureHomeVolumeLost = "HomeVolumeLost"
	// FailureTemplateRejected — the runtime backend refused the template's
	// requested configuration.
	FailureTemplateRejected = "TemplateRejected"
)

// FailureRecoverable reports whether :start may recover a Failed workspace
// with this reason by minting a new runtime generation.
func FailureRecoverable(reason string) bool {
	switch reason {
	case FailureBootDeadlineExceeded, FailureFailedCleanup, FailureRuntimeLost:
		return true
	}
	return false
}

// FailureUnrecoverable reports whether the reason is terminal: Failed is
// entered at once and :start is refused until an administrator fixes the
// cause.
func FailureUnrecoverable(reason string) bool {
	switch reason {
	case FailureImagePolicyDenied, FailureHomeVolumeLost, FailureTemplateRejected:
		return true
	}
	return false
}
