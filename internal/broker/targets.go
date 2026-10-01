package broker

import (
	"context"
	"fmt"

	"github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

// ProtocolKasmVNCWebSocket is the Linux streaming protocol the gateway
// proxies to the workspace runtime (design §6/§7).
const ProtocolKasmVNCWebSocket = "kasmvnc-websocket"

// runtimeStreamingPort is the Service port the runtime exposes the
// streaming endpoint on (mirrors internal/runtime/linux streamingPort).
const runtimeStreamingPort int32 = 8443

// Credential is the upstream authentication material for one workspace
// runtime, resolved from the per-workspace Secret. It never leaves the
// internal broker API — never logged, never serialized to clients.
type Credential struct {
	Username string
	Password string
	// TLSCA is the pinned trust anchor for the upstream TLS endpoint
	// (the workspace Secret's tls.crt; the runtime cert is self-signed).
	TLSCA []byte
}

// CredentialSource resolves the per-workspace runtime credential. The
// production implementation is K8sCredentialSource, which reads the
// workspace Secret through a namespaced client limited to managed
// namespaces (credentials.go).
type CredentialSource interface {
	RuntimeCredential(ctx context.Context, workspaceUID PlatformID, binding RuntimeBinding) (Credential, error)
}

// Target is the internal upstream resolution for a live lease: which runtime
// Service to reach, with what trust and which credentials to inject.
// INTERNAL ONLY — it is never serialized to the public API or the browser;
// credentials flow server-side gateway→runtime only.
type Target struct {
	Protocol   string `json:"protocol"` // e.g. "kasmvnc-websocket"; windows: guacamole mapping
	ServiceDNS string `json:"serviceDNS"`
	// UpstreamURL is the full https:// dial URL built from ServiceDNS;
	// it wins over ServiceDNS for the gateway's upstream transport.
	UpstreamURL string `json:"upstreamURL"`
	// TLSServerName pins TLS SNI/verification to the runtime Service name:
	// the per-workspace cert carries SANs for Pod/Service names, not the
	// .svc FQDN in the dial address.
	TLSServerName string `json:"tlsServerName"`
	// TLSCA is the pinned runtime certificate (the Secret's tls.crt — a
	// self-signed leaf used as its own trust anchor). Wire name: caPEM.
	TLSCA    []byte `json:"caPEM"`
	Username string `json:"username"` // runtime credential injected upstream, never from client
	Password string `json:"password"`
}

// credentialSource returns the configured CredentialSource, falling back to
// the synthesized dev/test source (lazy, once per broker) when none is wired.
func (b *Broker) credentialSource() CredentialSource {
	if b.creds != nil {
		return b.creds
	}
	b.synthOnce.Do(func() {
		s, err := newSynthesizedCredentials()
		b.synthErr = err
		b.synthCreds = s
	})
	if b.synthErr != nil {
		return errCredentialSource{b.synthErr}
	}
	return b.synthCreds
}

type errCredentialSource struct{ err error }

func (e errCredentialSource) RuntimeCredential(context.Context, PlatformID, RuntimeBinding) (Credential, error) {
	return Credential{}, e.err
}

// serviceDNS builds the cluster-internal host:port the gateway dials for a
// workspace runtime. When the binding carries a serviceRef it wins; otherwise
// the deterministic child name ws-<cruid> is derived from the Workspace CR's
// metadata.uid — the same name the operator creates — never from the
// platform workspace id.
func serviceDNS(binding RuntimeBinding) (svcName, dns string) {
	name := binding.ServiceName
	if name == "" {
		name = linux.ServiceName(binding.CRUID)
	}
	port := binding.ServicePort
	if port == 0 {
		port = runtimeStreamingPort
	}
	if binding.Namespace == "" {
		return name, fmt.Sprintf("%s:%d", name, port)
	}
	return name, fmt.Sprintf("%s.%s.svc:%d", name, binding.Namespace, port)
}

// ResolveTarget returns the upstream target for a live lease held by this
// gateway. Fails with ErrDenied for a different gateway, ErrLeaseInvalid for
// an expired/revoked/superseded lease and ErrStaleBinding when the bound
// incarnation is no longer current.
func (b *Broker) ResolveTarget(ctx context.Context, gw GatewayIdentity, leaseID string) (Target, error) {
	now := b.now()
	l, err := b.liveLease(ctx, gw, leaseID, now)
	if err != nil {
		return Target{}, err
	}
	binding, err := b.boundCurrent(ctx, l, now)
	if err != nil {
		return Target{}, err
	}
	cred, err := b.credentialSource().RuntimeCredential(ctx, PlatformID(l.WorkspaceUID), binding)
	if err != nil {
		return Target{}, err
	}
	svcName, dns := serviceDNS(binding)
	return Target{
		Protocol:      ProtocolKasmVNCWebSocket,
		ServiceDNS:    dns,
		UpstreamURL:   "https://" + dns,
		TLSServerName: svcName,
		TLSCA:         cred.TLSCA,
		Username:      cred.Username,
		Password:      cred.Password,
	}, nil
}
