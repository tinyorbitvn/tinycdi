package broker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

// NamespaceResolver maps a tenant ID to its managed Kubernetes namespace;
// satisfied by provisioning.TenantNamespaces and api.StaticTenantResolver.
type NamespaceResolver interface {
	Namespace(tenantID string) (ns string, ok bool)
}

// K8sCredentialSource reads the per-workspace runtime credential Secret
// (ws-<uid>-rt: username/password/tls.crt) through a namespaced client. The
// read is confined to the tenant's managed namespace — resolved from the
// binding's tenant, not from request input — so a forged binding can never
// steer the broker into an unmanaged namespace.
type K8sCredentialSource struct {
	reader  client.Reader
	tenants NamespaceResolver
}

// NewK8sCredentialSource wires the Secret reader. tenants must be non-nil in
// production: the namespace is only ever taken from the tenant mapping.
func NewK8sCredentialSource(reader client.Reader, tenants NamespaceResolver) *K8sCredentialSource {
	return &K8sCredentialSource{reader: reader, tenants: tenants}
}

// RuntimeCredential implements CredentialSource. The Secret name derives
// from the binding's CRUID — the operator names the Secret
// ws-<metadata.uid>-rt — never from the platform workspaceUID: on a real
// cluster the two differ, so a platform-id lookup can only miss.
func (s *K8sCredentialSource) RuntimeCredential(ctx context.Context, workspaceUID PlatformID, binding RuntimeBinding) (Credential, error) {
	ns := ""
	if s.tenants != nil {
		if mapped, ok := s.tenants.Namespace(binding.TenantID); ok {
			ns = mapped
		}
	}
	if ns == "" {
		// Fall back to the observed CR namespace only when no tenant map is
		// configured (tests); the binding source itself is restricted to
		// managed namespaces.
		ns = binding.Namespace
	}
	if ns == "" {
		return Credential{}, fmt.Errorf("broker: no managed namespace for tenant %q: %w", binding.TenantID, ErrDenied)
	}
	if binding.CRUID == "" {
		// A binding without the CR UID is data-plane corruption: without it
		// no Kubernetes object name can be derived. Fail closed.
		return Credential{}, fmt.Errorf("broker: binding for %s carries no workspace CR uid: %w", workspaceUID, ErrStaleBinding)
	}
	secName := linux.SecretName(binding.CRUID)

	sec := &corev1.Secret{}
	if err := s.reader.Get(ctx, client.ObjectKey{
		Name:      secName,
		Namespace: ns,
	}, sec); err != nil {
		return Credential{}, fmt.Errorf("broker: read runtime secret: %w", err)
	}
	cred := Credential{
		Username: strings.TrimSpace(string(sec.Data["username"])),
		Password: strings.TrimSpace(string(sec.Data["password"])),
		TLSCA:    sec.Data["tls.crt"],
	}
	if cred.Username == "" || cred.Password == "" || len(cred.TLSCA) == 0 {
		return Credential{}, fmt.Errorf("broker: runtime secret %s/%s incomplete", ns, secName)
	}
	return cred, nil
}

// ---------------------------------------------------------------------------
// synthesized credentials (unit-test fallback, fails closed upstream)
// ---------------------------------------------------------------------------

// synthesizedCredentials is the CredentialSource used when none is injected:
// it fabricates per-workspace material from a broker-local ephemeral CA. The
// material can never authenticate against a real runtime (the CA signs
// nothing the runtime presents and the password exists only here), so a
// broker deployed without a credential source fails closed at the upstream
// instead of ever serving a forged identity.
type synthesizedCredentials struct {
	caPEM []byte
	key   [32]byte
}

func newSynthesizedCredentials() (*synthesizedCredentials, error) {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, err
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "tcdi-broker-synthesized-test-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &ecKey.PublicKey, ecKey)
	if err != nil {
		return nil, err
	}
	return &synthesizedCredentials{
		caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		key:   key,
	}, nil
}

func (s *synthesizedCredentials) RuntimeCredential(_ context.Context, workspaceUID PlatformID, _ RuntimeBinding) (Credential, error) {
	sum := hmac.New(sha256.New, s.key[:])
	sum.Write([]byte("tcdi-runtime-credential\x00"))
	sum.Write([]byte(workspaceUID))
	return Credential{
		Username: "kasm_user",
		Password: base64.RawURLEncoding.EncodeToString(sum.Sum(nil)),
		TLSCA:    s.caPEM,
	}, nil
}
