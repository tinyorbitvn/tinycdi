package chart_test

// Sign-out and the identity provider (FX-R21): oidc.endSession (default
// true) renders --oidc-end-session, and oidc.postLogoutRedirect (default
// empty) renders --oidc-post-logout-redirect only when set; the schema
// rejects a non-https redirect.

import (
	"path/filepath"
	"strings"
	"testing"
)

func backendArgsWith(t *testing.T, sets ...string) []string {
	t.Helper()
	args := []string{"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml")}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	dep := deployment(renderArgs(t, args...), "backend")
	if dep == nil {
		t.Fatal("no backend Deployment rendered")
	}
	return firstContainerArgs(dep)
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func anyArgPrefix(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

func TestSignOutDefaults(t *testing.T) {
	args := backendArgsWith(t)
	if !hasArg(args, "--oidc-end-session=true") {
		t.Errorf("default render lacks --oidc-end-session=true: %v", args)
	}
	if anyArgPrefix(args, "--oidc-post-logout-redirect") {
		t.Errorf("post-logout redirect must be off by default (the URI has to be registered at the provider): %v", args)
	}
}

func TestSignOutEndSessionOff(t *testing.T) {
	args := backendArgsWith(t, "oidc.endSession=false")
	if !hasArg(args, "--oidc-end-session=false") {
		t.Errorf("oidc.endSession=false not rendered: %v", args)
	}
}

func TestSignOutPostLogoutRedirect(t *testing.T) {
	args := backendArgsWith(t, "oidc.postLogoutRedirect=https://portal.lab.example.net/signed-out")
	if !hasArg(args, "--oidc-post-logout-redirect=https://portal.lab.example.net/signed-out") {
		t.Errorf("oidc.postLogoutRedirect not rendered: %v", args)
	}
	out := renderErrArgs(t, "-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "oidc.postLogoutRedirect=javascript:alert(1)")
	if !strings.Contains(out, "postLogoutRedirect") && !strings.Contains(out, "pattern") {
		t.Errorf("a non-https postLogoutRedirect must be rejected by the schema, got: %s", out)
	}
	out = renderErrArgs(t, "-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "oidc.endSession=maybe")
	if !strings.Contains(out, "endSession") && !strings.Contains(out, "boolean") {
		t.Errorf("a non-boolean endSession must be rejected by the schema, got: %s", out)
	}
}
