package brokerclient_test

// Contract tests for the remote BrokerClient: an httptest mTLS server plays
// the broker's internal listener. Covers mTLS enforcement (no client cert /
// wrong CA), the redeem/renew/target/revoke wire shapes, and the typed error
// mapping (401/403/404/409/410/5xx).

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
	"github.com/tinyorbitvn/tinycdi/internal/gateway/brokerclient"
)

// --- test PKI ---------------------------------------------------------------

type testPKI struct {
	caPEM      []byte
	serverCert tls.Certificate
	clientCert tls.Certificate
}

func newCA(t *testing.T, cn string) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func issueCert(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, server bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	if server {
		tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		tpl.DNSNames = []string{"localhost"}
	} else {
		tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func newTestPKI(t *testing.T) testPKI {
	t.Helper()
	ca, caKey, caPEM := newCA(t, "test-broker-ca")
	return testPKI{
		caPEM:      caPEM,
		serverCert: issueCert(t, ca, caKey, "broker-internal", true),
		clientCert: issueCert(t, ca, caKey, "gw-test", false),
	}
}

func certPool(t *testing.T, pemBytes []byte) *x509.CertPool {
	t.Helper()
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(pemBytes) {
		t.Fatal("bad CA PEM")
	}
	return p
}

// --- fake broker internal listener -----------------------------------------

type fakeBrokerAPI struct {
	srv      *httptest.Server
	lastBody map[string]json.RawMessage
	lastPath string
	lastReq  string

	// scripted behaviour
	lease   broker.Lease
	target  broker.Target
	status  int // when != 0, every endpoint answers this status + error body
	code    string
	gotAuth bool // client cert was verified-present
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code": code, "message": msg, "retryable": false, "requestId": "srv-req-1",
	})
}

func leaseDoc(l broker.Lease) map[string]any {
	return map[string]any{
		"leaseId":           l.ID,
		"workspaceUID":      l.WorkspaceUID,
		"tenantID":          l.TenantID,
		"principalSubject":  l.PrincipalSubject,
		"runtimeGeneration": l.RuntimeGeneration,
		"runtimeUID":        l.RuntimeUID,
		"fencingVersion":    l.FencingVersion,
		"gatewayID":         l.GatewayID,
		"expiresAt":         l.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
}

func newFakeBrokerAPI(t *testing.T, pki testPKI) *fakeBrokerAPI {
	f := &fakeBrokerAPI{
		lease: broker.Lease{
			ID: "lease-1", WorkspaceUID: "ws-1", TenantID: "tenant-a",
			PrincipalSubject: "iss|alice", RuntimeGeneration: 3,
			RuntimeUID: "rt-9", FencingVersion: 2, GatewayID: "gw-test",
			ExpiresAt: time.Now().Add(30 * time.Second),
		},
		target: broker.Target{
			Protocol:      "kasmvnc-websocket",
			ServiceDNS:    "ws-1.ns.svc:8443",
			UpstreamURL:   "https://ws-1.ns.svc:8443",
			TLSServerName: "ws-1",
			TLSCA:         []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"),
			Username:      "kasm_user",
			Password:      "s3cret",
		},
		lastBody: map[string]json.RawMessage{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/v1/broker/redeem", func(w http.ResponseWriter, r *http.Request) {
		f.capture(r)
		if f.status != 0 {
			writeErr(w, f.status, f.code, "scripted")
			return
		}
		var body struct {
			Ticket string `json:"ticket"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Ticket == "" {
			writeErr(w, http.StatusUnauthorized, "UNAUTHENTICATED", "bad ticket")
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(leaseDoc(f.lease))
	})
	mux.HandleFunc("/internal/v1/broker/leases/", func(w http.ResponseWriter, r *http.Request) {
		f.capture(r)
		if f.status != 0 {
			writeErr(w, f.status, f.code, "scripted")
			return
		}
		switch {
		case len(r.URL.Path) > len("/internal/v1/broker/leases/") &&
			r.URL.Path[len(r.URL.Path)-6:] == "/renew":
			var body struct {
				Fence struct {
					WorkspaceUID      string `json:"workspaceUID"`
					RuntimeGeneration uint64 `json:"runtimeGeneration"`
					RuntimeUID        string `json:"runtimeUID"`
					Version           uint64 `json:"version"`
				} `json:"fence"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Fence.RuntimeUID != f.lease.RuntimeUID {
				writeErr(w, http.StatusConflict, "INVALID_STATE", "stale fence")
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(leaseDoc(f.lease))
		case len(r.URL.Path) > 7 && r.URL.Path[len(r.URL.Path)-7:] == "/target":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"upstreamURL":   f.target.UpstreamURL,
				"tlsServerName": f.target.TLSServerName,
				"serviceDNS":    f.target.ServiceDNS,
				"caPEM":         f.target.TLSCA,
				"username":      f.target.Username,
				"password":      f.target.Password,
				"protocol":      f.target.Protocol,
			})
		case len(r.URL.Path) > 7 && r.URL.Path[len(r.URL.Path)-7:] == "/revoke":
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/activity"):
			var body struct {
				Fence struct {
					Version uint64 `json:"version"`
				} `json:"fence"`
				Type string `json:"type"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.lastBody["type"] = json.RawMessage(`"` + body.Type + `"`)
			f.lastBody["version"] = json.RawMessage(json.Number(fmt.Sprint(body.Fence.Version)).String())
			w.WriteHeader(http.StatusNoContent)
		default:
			writeErr(w, http.StatusNotFound, "NOT_FOUND", "no route")
		}
	})
	f.srv = httptest.NewUnstartedServer(mux)
	clientCAPool := certPool(t, pki.caPEM)
	f.srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{pki.serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAPool,
		MinVersion:   tls.VersionTLS12,
	}
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBrokerAPI) capture(r *http.Request) {
	f.lastPath = r.URL.Path
	f.lastReq = r.Header.Get("X-Request-Id")
	f.gotAuth = len(r.TLS.PeerCertificates) > 0
}

// clientFor builds a Client against the fake broker, via file-based config
// (the production path) using temp PEM files.
func clientFor(t *testing.T, api *fakeBrokerAPI, pki testPKI) gateway.BrokerClient {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pki.clientCert.Certificate[0]})
	keyDER, err := x509.MarshalECPrivateKey(pki.clientCert.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	c, err := brokerclient.New(brokerclient.Config{
		BaseURL:  api.srv.URL,
		CertFile: write("client.crt", certPEM),
		KeyFile:  write("client.key", keyPEM),
		CAFile:   write("ca.crt", pki.caPEM),
		Timeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("brokerclient.New: %v", err)
	}
	return c
}

var testGW = broker.GatewayIdentity{ID: "gw-test", Audience: "session.test"}

func TestRedeemTicket_HappyPath(t *testing.T) {
	pki := newTestPKI(t)
	api := newFakeBrokerAPI(t, pki)
	c := clientFor(t, api, pki)

	l, err := c.RedeemTicket(context.Background(), testGW, "opaque-ticket")
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if l.ID != "lease-1" || l.RuntimeUID != "rt-9" || l.FencingVersion != 2 {
		t.Fatalf("bad lease decode: %+v", l)
	}
	if !api.gotAuth {
		t.Fatal("server saw no client certificate — mTLS not enforced")
	}
	if api.lastReq == "" {
		t.Fatal("X-Request-Id not sent")
	}
	if api.lastPath != "/internal/v1/broker/redeem" {
		t.Fatalf("redeem hit %q", api.lastPath)
	}
}

func TestRenewLease_FenceOnWire(t *testing.T) {
	pki := newTestPKI(t)
	api := newFakeBrokerAPI(t, pki)
	c := clientFor(t, api, pki)

	fence := broker.Fence{WorkspaceUID: "ws-1", RuntimeGeneration: 3, RuntimeUID: "rt-9", FencingVersion: 2}
	l, err := c.RenewLease(context.Background(), testGW, "lease-1", fence)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if l.ID != "lease-1" {
		t.Fatalf("bad lease: %+v", l)
	}
	if want := "/internal/v1/broker/leases/lease-1/renew"; api.lastPath != want {
		t.Fatalf("renew hit %q, want %q", api.lastPath, want)
	}
}

func TestRenewLease_StaleFence409(t *testing.T) {
	pki := newTestPKI(t)
	api := newFakeBrokerAPI(t, pki)
	c := clientFor(t, api, pki)

	fence := broker.Fence{WorkspaceUID: "ws-1", RuntimeGeneration: 1, RuntimeUID: "OLD-rt", FencingVersion: 1}
	_, err := c.RenewLease(context.Background(), testGW, "lease-1", fence)
	if !errors.Is(err, broker.ErrStaleBinding) {
		t.Fatalf("409 stale renew err = %v, want ErrStaleBinding", err)
	}
}

func TestRenewLease_Revoked410(t *testing.T) {
	pki := newTestPKI(t)
	api := newFakeBrokerAPI(t, pki)
	c := clientFor(t, api, pki)

	// Flip the fake into Gone mode for the renew path only by scripting
	// status+code on all endpoints.
	api.status, api.code = http.StatusGone, "INVALID_STATE"
	_, err := c.RenewLease(context.Background(), testGW, "lease-1",
		broker.Fence{RuntimeUID: "rt-9"})
	if !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("410 renew err = %v, want ErrRevoked", err)
	}
	var be *brokerclient.Error
	if !errors.As(err, &be) || be.Status != http.StatusGone {
		t.Fatalf("typed error lost: %v", err)
	}
	if be.RequestID != "srv-req-1" {
		t.Fatalf("request id not propagated: %+v", be)
	}
}

func TestRedeemTicket_Unauthorized401(t *testing.T) {
	pki := newTestPKI(t)
	api := newFakeBrokerAPI(t, pki)
	c := clientFor(t, api, pki)

	_, err := c.RedeemTicket(context.Background(), testGW, "") // server 401s empty ticket
	if !errors.Is(err, broker.ErrTicketInvalid) {
		t.Fatalf("401 redeem err = %v, want ErrTicketInvalid", err)
	}
}

func TestResolveTarget_HappyPath(t *testing.T) {
	pki := newTestPKI(t)
	api := newFakeBrokerAPI(t, pki)
	c := clientFor(t, api, pki)

	tgt, err := c.ResolveTarget(context.Background(), testGW, "lease-1")
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	if tgt.UpstreamURL != "https://ws-1.ns.svc:8443" || tgt.TLSServerName != "ws-1" {
		t.Fatalf("bad target decode: %+v", tgt)
	}
	if tgt.Username != "kasm_user" || tgt.Password != "s3cret" {
		t.Fatal("credentials not decoded")
	}
}

func TestResolveTarget_NotFound404(t *testing.T) {
	pki := newTestPKI(t)
	api := newFakeBrokerAPI(t, pki)
	api.status, api.code = http.StatusNotFound, "NOT_FOUND"
	c := clientFor(t, api, pki)

	_, err := c.ResolveTarget(context.Background(), testGW, "gone")
	if !errors.Is(err, broker.ErrLeaseInvalid) && !errors.Is(err, broker.ErrNotFound) {
		t.Fatalf("404 target err = %v, want lease-invalid family", err)
	}
}

func TestRevokeLease_204(t *testing.T) {
	pki := newTestPKI(t)
	api := newFakeBrokerAPI(t, pki)
	c := clientFor(t, api, pki)

	if err := c.RevokeLease(context.Background(), "lease-1"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if api.lastPath != "/internal/v1/broker/leases/lease-1/revoke" {
		t.Fatalf("revoke hit %q", api.lastPath)
	}
}

func TestReportActivity_204(t *testing.T) {
	pki := newTestPKI(t)
	api := newFakeBrokerAPI(t, pki)
	c := clientFor(t, api, pki)

	fence := broker.Fence{WorkspaceUID: "ws-1", RuntimeGeneration: 3, RuntimeUID: "rt-9", FencingVersion: 2}
	err := c.ReportActivity(context.Background(), testGW, "lease-1", fence,
		broker.ActivityEvent{Type: broker.ActivityInput, ReceivedAt: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatalf("activity: %v", err)
	}
	if api.lastPath != "/internal/v1/broker/leases/lease-1/activity" {
		t.Fatalf("activity hit %q", api.lastPath)
	}
	if string(api.lastBody["type"]) != `"input"` {
		t.Fatalf("activity type on wire = %s", api.lastBody["type"])
	}
	if string(api.lastBody["version"]) != `2` {
		t.Fatalf("fence version on wire = %s", api.lastBody["version"])
	}
}

func TestTransient5xx_NotSentinel(t *testing.T) {
	pki := newTestPKI(t)
	api := newFakeBrokerAPI(t, pki)
	api.status, api.code = http.StatusServiceUnavailable, "UNAVAILABLE"
	c := clientFor(t, api, pki)

	_, err := c.RenewLease(context.Background(), testGW, "lease-1", broker.Fence{})
	if err == nil {
		t.Fatal("503 silently succeeded")
	}
	// must NOT map onto a terminal sentinel — the gateway's fail-closed
	// window relies on transient errors staying non-terminal.
	for _, s := range []error{broker.ErrRevoked, broker.ErrStaleBinding, broker.ErrLeaseInvalid, broker.ErrDenied} {
		if errors.Is(err, s) {
			t.Fatalf("503 mapped to terminal %v", s)
		}
	}
}

func TestWrongServerCARejected(t *testing.T) {
	pki := newTestPKI(t)
	api := newFakeBrokerAPI(t, pki)
	// client trusts a DIFFERENT CA → handshake must fail
	_, _, foreignCAPEM := newCA(t, "foreign-ca")
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pki.clientCert.Certificate[0]})
	keyDER, _ := x509.MarshalECPrivateKey(pki.clientCert.PrivateKey.(*ecdsa.PrivateKey))
	c, err := brokerclient.New(brokerclient.Config{
		BaseURL:  api.srv.URL,
		CertFile: write("c.crt", certPEM),
		KeyFile:  write("c.key", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		CAFile:   write("ca.crt", foreignCAPEM),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.RedeemTicket(context.Background(), testGW, "t")
	if err == nil {
		t.Fatal("wrong-CA broker accepted")
	}
	var be *brokerclient.Error
	if errors.As(err, &be) && be.Status != 0 {
		t.Fatalf("wrong-CA should fail at handshake, got HTTP %d", be.Status)
	}
}

func TestNoClientCert_Rejected(t *testing.T) {
	pki := newTestPKI(t)
	api := newFakeBrokerAPI(t, pki)
	// TLS config without client cert → server (RequireAndVerifyClientCert)
	// must refuse; the client surfaces a transport error, not a 2xx.
	c, err := brokerclient.New(brokerclient.Config{
		BaseURL: api.srv.URL,
		TLSConfig: &tls.Config{
			RootCAs:    certPool(t, pki.caPEM),
			MinVersion: tls.VersionTLS12,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.RedeemTicket(context.Background(), testGW, "t")
	if err == nil {
		t.Fatal("broker accepted a client with no certificate")
	}
}

func TestNew_ValidatesConfig(t *testing.T) {
	if _, err := brokerclient.New(brokerclient.Config{}); err == nil {
		t.Fatal("empty config accepted")
	}
	if _, err := brokerclient.New(brokerclient.Config{BaseURL: "http://broker"}); err == nil {
		t.Fatal("non-https base URL accepted")
	}
	u, _ := url.Parse("https://x")
	_ = u
}
