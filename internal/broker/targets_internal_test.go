package broker

// Unit tests for the identity seams: every Kubernetes object
// name the broker derives must come from the binding's CRUID — the
// Workspace CR's metadata.uid — never from the platform WorkspaceUID.

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

// serviceDNS falls back to the deterministic operator child name
// ws-<cruid> when the binding carries no serviceRef.
func TestServiceDNS_DerivesFromCRUID(t *testing.T) {
	b := RuntimeBinding{
		WorkspaceUID: "ws_platform01", // platform id — must NOT appear in the name
		CRUID:        types.UID("4f4b0f2a-9d1c-4e2a-b1c3-7d8e9f0a1b2c"),
		Namespace:    "ns-t",
	}
	name, dns := serviceDNS(b)
	if want := "ws-4f4b0f2a-9d1c-4e2a-b1c3-7d8e9f0a1b2c"; name != want {
		t.Fatalf("service name = %q, want %q (CR-UID-derived)", name, want)
	}
	if want := name + ".ns-t.svc:8443"; dns != want {
		t.Fatalf("dns = %q, want %q", dns, want)
	}
	// serviceRef wins when the operator reports one.
	b.ServiceName, b.ServicePort = "ws-explicit", 9443
	name, dns = serviceDNS(b)
	if name != "ws-explicit" || dns != "ws-explicit.ns-t.svc:9443" {
		t.Fatalf("serviceRef override = %q %q", name, dns)
	}
}

// A binding without a CR UID is data-plane corruption: the credential
// source must fail closed instead of deriving a garbage Secret name.
func TestK8sCredentialSource_RequiresCRUID(t *testing.T) {
	src := NewK8sCredentialSource(nil, nil)
	_, err := src.RuntimeCredential(context.Background(), "ws_platform01", RuntimeBinding{
		WorkspaceUID: "ws_platform01",
		CRUID:        "",
		Namespace:    "ns-t",
	})
	if err == nil || !errors.Is(err, ErrStaleBinding) {
		t.Fatalf("RuntimeCredential without CRUID = %v, want ErrStaleBinding", err)
	}
}
