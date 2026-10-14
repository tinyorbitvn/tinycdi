package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

func failedEnv(t *testing.T, obs ObservedStatus) (*fakeWorkspaceBackend, *testEnv, *http.Cookie, *http.Cookie) {
	t.Helper()
	be := newFakeBackend()
	sv := &fakeStatusView{}
	sv.set("ws_failed0001", obs)
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants(),
		func(h *WorkspaceHandler) { h.WithStatusView(sv) })
	sess, csrf := login(t, env, "user-a")
	be.recs["ws_failed0001"] = provisioning.WorkspaceRecord{
		ID: "ws_failed0001", TenantID: "tenant-a", Owner: env.issuer.URL() + "|user-a",
		OwnerIssuer: env.issuer.URL(), OwnerSub: "user-a", Name: "lost",
		Template: provisioning.TemplateInfo{ID: "tpl_linuxdesktop", Name: "linuxdesktop", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop"},
		Phase: "Ready", DesiredState: "Running", DataPolicy: "Retain", Revision: 3,
		CreatedAt: time.Now().UTC().Add(-time.Hour), UpdatedAt: time.Now().UTC(),
	}
	return be, env, sess, csrf
}

func failedObs(reason string) ObservedStatus {
	return ObservedStatus{Fresh: true, Found: true, IntentSettled: true, Phase: "Failed",
		FailureReason: reason, RuntimeGeneration: 4}
}

func startFailed(t *testing.T, env *testEnv, sess, csrf *http.Cookie, key string) *http.Response {
	t.Helper()
	return doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces/ws_failed0001:start", "",
		map[string]string{"Idempotency-Key": key})
}

func TestStart_FailedRecoverableRestartsWithObservedGeneration(t *testing.T) {
	for _, reason := range []string{"RuntimeLost", "BootDeadlineExceeded", "FailedCleanup"} {
		be, env, sess, csrf := failedEnv(t, failedObs(reason))
		r := startFailed(t, env, sess, csrf, "key-recover-01")
		if r.StatusCode != http.StatusAccepted {
			t.Fatalf("%s: status %d, want 202", reason, r.StatusCode)
		}
		if len(be.restartCalls) != 1 || be.restartCalls[0] != 4 || be.signalCalls != 0 {
			t.Fatalf("%s: restart=%v signal=%d, want one restart fenced on generation 4", reason, be.restartCalls, be.signalCalls)
		}
	}
}

func TestStart_FailedUnrecoverableIs409WithReason(t *testing.T) {
	for _, reason := range []string{"HomeVolumeLost", "ImagePolicyDenied", "TemplateRejected"} {
		be, env, sess, csrf := failedEnv(t, failedObs(reason))
		r := startFailed(t, env, sess, csrf, "key-recover-02")
		e := decodeErr(t, r)
		if r.StatusCode != http.StatusConflict || e.Code != CodeInvalidState || e.Details == nil || e.Details.Reason != reason {
			t.Fatalf("%s: status=%d err=%+v, want 409 INVALID_STATE with details.reason", reason, r.StatusCode, e)
		}
		if len(be.restartCalls) != 0 || be.signalCalls != 0 {
			t.Fatalf("%s: an unrecoverable failure must not reach the service", reason)
		}
	}
}

func TestStart_StaleOrUnsettledStatusIsAPlainStart(t *testing.T) {
	stale := failedObs("RuntimeLost")
	stale.Fresh = false
	unsettled := failedObs("RuntimeLost") // the restart is applied, the operator has not reconciled it yet
	unsettled.IntentSettled = false
	for name, obs := range map[string]ObservedStatus{"stale informer": stale, "unsettled intent": unsettled} {
		be, env, sess, csrf := failedEnv(t, obs)
		r := startFailed(t, env, sess, csrf, "key-recover-03")
		if r.StatusCode != http.StatusAccepted || len(be.restartCalls) != 0 || be.signalCalls != 1 {
			t.Fatalf("%s: status=%d restart=%v signal=%d, want a plain start and no restart", name, r.StatusCode, be.restartCalls, be.signalCalls)
		}
	}
}

func TestStart_StoppedFailedWorkspaceIsAPlainStart(t *testing.T) {
	be, env, sess, csrf := failedEnv(t, failedObs("HomeVolumeLost"))
	rec := be.recs["ws_failed0001"]
	rec.DesiredState = "Stopped" // stop, then start: the way out of a refused reason once the cause is fixed
	be.recs["ws_failed0001"] = rec
	r := startFailed(t, env, sess, csrf, "key-recover-05")
	if r.StatusCode != http.StatusAccepted || len(be.restartCalls) != 0 || be.signalCalls != 1 {
		t.Fatalf("status=%d restart=%v signal=%d, want a plain start", r.StatusCode, be.restartCalls, be.signalCalls)
	}
}

func TestStart_RestartNotRunningIs409(t *testing.T) {
	be, env, sess, csrf := failedEnv(t, failedObs("RuntimeLost"))
	be.restartErr = provisioning.ErrNotRunning // a stop raced the recovery
	r := startFailed(t, env, sess, csrf, "key-recover-06")
	if e := decodeErr(t, r); r.StatusCode != http.StatusConflict || e.Code != CodeInvalidState {
		t.Fatalf("status=%d err=%+v, want 409 INVALID_STATE", r.StatusCode, e)
	}
}

func TestStart_ForeignFailedWorkspaceIs404BeforeStatus(t *testing.T) {
	be, env, _, _ := failedEnv(t, failedObs("RuntimeLost"))
	sess, csrf := login(t, env, "user-b")
	r := startFailed(t, env, sess, csrf, "key-recover-04")
	if r.StatusCode != http.StatusNotFound || len(be.restartCalls) != 0 || be.signalCalls != 0 {
		t.Fatalf("status=%d restart=%v signal=%d, want 404 and no mutation", r.StatusCode, be.restartCalls, be.signalCalls)
	}
}
