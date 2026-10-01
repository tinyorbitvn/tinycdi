//go:build integration

// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package integration

// Two-replica restart drill for the merged backend (design §3.6, decisions
// D19–D23). Two Backend instances share one Postgres, one -gateway-id, one
// login-key set, one fake OIDC issuer, one fake runtime upstream and the
// envtest apiserver as the workspace binding source. The drill asserts a
// session survives replica restarts end to end: cookie rehydration, stream
// resume, rolling restart, sealed login state, certificate rotation and
// clean stream accounting when a replica dies hard.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/jackc/pgx/v5"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	workspacev1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
	"github.com/tinyorbitvn/tinycdi/internal/backend"
	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

const (
	restartTenant        = "tenant-it"
	restartNamespace     = "ns-it"
	restartSessionHost   = "session.test"
	restartSessionOrigin = "https://session.test"
	restartPortalOrigin  = "https://portal.test"
	restartPortalPath    = "/v1/auth/callback"
	restartGatewayID     = "tcdi-it-gw"
	restartGatewayAud    = "session.test"
	restartUpstreamName  = "ws-rt"
	restartSubject       = "it-user"
	restartLoginCookie   = "__Host-tcdi_login"
	restartSessionCookie = "__Host-tcdi_session"
	restartCSRFCookie    = "tcdi_csrf"
	restartCSRFHeader    = "X-CSRF-Token"
)

// ---------------------------------------------------------------------------
// Cluster-DNS shim
// ---------------------------------------------------------------------------

// envtest has no cluster DNS, but the broker resolves a workspace runtime to
// https://<service>.<namespace>.svc:<port>. The shim points the process-wide
// resolver at an in-process DNS server answering every "*.svc" name with
// 127.0.0.1 — where the fake runtime upstream listens. Every other name is
// forwarded verbatim to the nameserver configured in /etc/resolv.conf so
// unrelated lookups in this test binary keep working.
var (
	svcDNSOnce sync.Once
	svcDNSErr  error
)

func ensureSvcDNS(t *testing.T) {
	t.Helper()
	svcDNSOnce.Do(func() { svcDNSErr = startSvcDNS() })
	if svcDNSErr != nil {
		t.Fatalf("svc DNS shim: %v", svcDNSErr)
	}
}

func startSvcDNS() error {
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		return fmt.Errorf("svc DNS listen: %w", err)
	}
	go svcDNSLoop(ln, systemNameserver())
	addr := ln.LocalAddr().String()
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			if !strings.HasPrefix(network, "udp") {
				// The stub never truncates a response, so the resolver only
				// reaches for TCP after a malformed reply — fail instead of
				// serving length-prefixed framing it cannot read.
				return nil, errors.New("svc DNS shim: udp only")
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
	return nil
}

func svcDNSLoop(ln *net.UDPConn, upstream string) {
	buf := make([]byte, 4<<10)
	for {
		n, peer, err := ln.ReadFromUDP(buf)
		if err != nil {
			return // socket lives for the whole test binary
		}
		pkt := append([]byte(nil), buf[:n]...)
		go func() {
			if resp := svcDNSAnswer(pkt, upstream); resp != nil {
				_, _ = ln.WriteToUDP(resp, peer)
			}
		}()
	}
}

func svcDNSAnswer(pkt []byte, upstream string) []byte {
	var p dnsmessage.Parser
	hdr, err := p.Start(pkt)
	if err != nil {
		return nil
	}
	q, err := p.Question()
	if err != nil || hdr.Response {
		return refuseDNS(pkt)
	}
	name := strings.ToLower(strings.TrimSuffix(q.Name.String(), "."))
	if !strings.HasSuffix(name, ".svc") {
		return forwardDNS(pkt, upstream)
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 hdr.ID,
		Response:           true,
		OpCode:             hdr.OpCode,
		Authoritative:      true,
		RecursionDesired:   hdr.RecursionDesired,
		RecursionAvailable: true,
		RCode:              dnsmessage.RCodeSuccess,
	})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return refuseDNS(pkt)
	}
	if err := b.Question(q); err != nil {
		return refuseDNS(pkt)
	}
	if q.Type == dnsmessage.TypeA {
		if err := b.StartAnswers(); err != nil {
			return refuseDNS(pkt)
		}
		if err := b.AResource(
			dnsmessage.ResourceHeader{
				Name:  q.Name,
				Type:  dnsmessage.TypeA,
				Class: dnsmessage.ClassINET,
				TTL:   30,
			},
			dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}},
		); err != nil {
			return refuseDNS(pkt)
		}
	}
	resp, err := b.Finish()
	if err != nil {
		return refuseDNS(pkt)
	}
	return resp
}

func systemNameserver() string {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" {
			return net.JoinHostPort(f[1], "53")
		}
	}
	return ""
}

// forwardDNS relays a raw DNS query packet to the upstream resolver and
// returns its verbatim response.
func forwardDNS(pkt []byte, upstream string) []byte {
	if upstream != "" {
		c, err := net.DialTimeout("udp", upstream, 3*time.Second)
		if err == nil {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(3 * time.Second))
			if _, err = c.Write(pkt); err == nil {
				buf := make([]byte, 4<<10)
				if n, err := c.Read(buf); err == nil {
					return buf[:n]
				}
			}
		}
	}
	return refuseDNS(pkt)
}

func refuseDNS(pkt []byte) []byte {
	var p dnsmessage.Parser
	hdr, err := p.Start(pkt)
	if err != nil {
		return nil
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 hdr.ID,
		Response:           true,
		RecursionDesired:   hdr.RecursionDesired,
		RecursionAvailable: true,
		RCode:              dnsmessage.RCodeRefused,
	})
	resp, err := b.Finish()
	if err != nil {
		return nil
	}
	return resp
}

// ---------------------------------------------------------------------------
// Fixture: one Postgres, one envtest binding source, one fake runtime
// upstream, one fake OIDC issuer — shared by every replica of the drill.
// ---------------------------------------------------------------------------

type restartFixture struct {
	db         *store.DB
	dsn        string
	dir        string
	iss        *oidctest.Issuer
	upstream   *httptest.Server
	upCertPEM  []byte
	wsUID      string
	owner      string
	kubeconfig string
	sessCert   string
	sessKey    string
	loginKey   string
}

// restartDB is newDB plus the DSN: each replica opens its own pool through
// it, so the drill can sever one replica's connections by application_name.
func restartDB(t *testing.T) (*store.DB, string) {
	t.Helper()
	ensurePostgres(t)
	ctx := context.Background()

	admin, err := store.Open(ctx, pgAdminDSN)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	name := fmt.Sprintf("it_rs_%d_%d", time.Now().UnixNano()%1_000_000, dbCounter.Add(1))
	if _, err := admin.Pool().Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatalf("create database: %v", err)
	}
	dbURL := strings.Replace(pgAdminDSN, "/postgres?", "/"+name+"?", 1)
	db, err := store.Open(ctx, dbURL)
	if err != nil {
		admin.Close()
		t.Fatalf("connect %s: %v", name, err)
	}
	if err := db.Migrate(ctx); err != nil {
		db.Close()
		admin.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Pool().Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		admin.Close()
	})
	return db, dbURL
}

// writeCertPair writes a self-signed ECDSA server certificate for cn with
// the given SANs into dir/<cn>/, returning cert and key paths.
func writeCertPair(t *testing.T, dir, cn string, dnsNames []string, ips []net.IP) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, "tls.crt")
	keyPath = filepath.Join(dir, "tls.key")
	writeFile(t, certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	writeFile(t, keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPath, keyPath
}

func writeKubeconfig(t *testing.T, cfg *rest.Config) string {
	t.Helper()
	cc := clientcmdapi.NewConfig()
	cc.Clusters["it"] = &clientcmdapi.Cluster{Server: cfg.Host, CertificateAuthorityData: cfg.CAData}
	cc.AuthInfos["it"] = &clientcmdapi.AuthInfo{ClientCertificateData: cfg.CertData, ClientKeyData: cfg.KeyData}
	cc.Contexts["it"] = &clientcmdapi.Context{Cluster: "it", AuthInfo: "it"}
	cc.CurrentContext = "it"
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*cc, path); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

// fakeRuntimeUpstream is the stand-in workspace runtime: a TLS server
// carrying a certificate for the workspace Service name that answers 200 to
// plain GETs and completes the websocket upgrade with a raw echo loop.
func fakeRuntimeUpstream(t *testing.T, svcName string) (*httptest.Server, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: svcName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{svcName},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(runtimeEchoHandler))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// runtimeEchoHandler answers the upgrade offer with a bare 101 and echoes
// every byte back — enough for the proxy's duplex path and resume checks.
func runtimeEchoHandler(w http.ResponseWriter, r *http.Request) {
	if upgradeOffered(r) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		c, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") +
			"258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\nConnection: upgrade\r\n" +
			"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) +
			"\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(struct{ io.Writer }{c}, struct{ io.Reader }{rw})
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func upgradeOffered(r *http.Request) bool {
	conn := false
	for _, v := range r.Header.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				conn = true
			}
		}
	}
	return conn && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func newRestartFixture(t *testing.T) *restartFixture {
	t.Helper()
	ensureSvcDNS(t)
	if k8sClient == nil || testEnvConfig == nil {
		t.Skip("envtest unavailable (KUBEBUILDER_ASSETS)")
	}
	db, dsn := restartDB(t)

	iss, err := oidctest.NewIssuer()
	if err != nil {
		t.Fatalf("oidctest: %v", err)
	}
	iss.Subject = restartSubject
	iss.TenantID = restartTenant
	t.Cleanup(iss.Close)

	dir := t.TempDir()
	sessCert, sessKey := writeCertPair(t, filepath.Join(dir, "session"), "it-backend-session",
		[]string{restartSessionHost}, []net.IP{net.ParseIP("127.0.0.1")})
	loginKey := filepath.Join(dir, "login.key")
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatal(err)
	}
	// Written base64: raw bytes whose edges are ASCII whitespace would be
	// trimmed below 32 bytes by the loader.
	writeFile(t, loginKey, []byte(base64.StdEncoding.EncodeToString(keyBytes)))

	up, upCert := fakeRuntimeUpstream(t, restartUpstreamName)
	upPort := up.Listener.Addr().(*net.TCPAddr).Port

	f := &restartFixture{
		db:         db,
		dsn:        dsn,
		dir:        dir,
		iss:        iss,
		upstream:   up,
		upCertPEM:  upCert,
		kubeconfig: writeKubeconfig(t, testEnvConfig),
		sessCert:   sessCert,
		sessKey:    sessKey,
		loginKey:   loginKey,
	}
	f.seedWorkspace(t, int32(upPort))
	return f
}

// seedWorkspace inserts the workspace row and its envtest projection: a
// Ready Workspace CR carrying the fake upstream's serviceRef plus the
// runtime credential Secret the broker resolves through the tenant map.
func (f *restartFixture) seedWorkspace(t *testing.T, upPort int32) {
	t.Helper()
	ctx := context.Background()

	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		t.Fatal(err)
	}
	f.wsUID = "ws_" + hex.EncodeToString(rnd[:])
	f.owner = f.iss.URL() + "|" + restartSubject

	if _, err := f.db.Pool().Exec(ctx,
		`INSERT INTO workspaces (id, tenant_id, owner_subject, request_id) VALUES ($1, $2, $3, $4)`,
		f.wsUID, restartTenant, f.owner, "req-"+f.wsUID); err != nil {
		t.Fatalf("seed workspace row: %v", err)
	}

	if err := k8sClient.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: restartNamespace},
	}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}

	ws := &workspacev1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      provisioning.WorkspaceCRName(provisioning.PlatformID(f.wsUID)),
			Namespace: restartNamespace,
			Labels: map[string]string{
				provisioning.LabelWorkspaceUID: f.wsUID,
				provisioning.LabelTenant:       restartTenant,
			},
		},
		Spec: workspacev1alpha1.WorkspaceSpec{
			TemplateRef: workspacev1alpha1.TemplateReference{Name: "tpl-it"},
			OwnerSubject: workspacev1alpha1.OwnerSubject{
				Issuer:  f.iss.URL(),
				Subject: restartSubject,
			},
			DesiredState:      workspacev1alpha1.DesiredStateRunning,
			DataPolicy:        workspacev1alpha1.DataPolicyEphemeral,
			RuntimeGeneration: 1,
			IntentRevision:    1,
		},
	}
	if err := k8sClient.Create(ctx, ws); err != nil {
		t.Fatalf("create workspace CR: %v", err)
	}
	cruid := ws.UID

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-" + string(cruid) + "-rt",
			Namespace: restartNamespace,
		},
		StringData: map[string]string{
			"username": "kasm_user",
			"password": "it-upstream-secret",
			"tls.crt":  string(f.upCertPEM),
		},
	}
	if err := k8sClient.Create(ctx, sec); err != nil {
		t.Fatalf("create runtime secret: %v", err)
	}

	ws.Status = workspacev1alpha1.WorkspaceStatus{
		Phase:                     workspacev1alpha1.WorkspacePhaseReady,
		ObservedRuntimeGeneration: 1,
		RuntimeUID:                "rt-" + string(cruid),
		StartedAt:                 &metav1.Time{Time: time.Now()},
		ServiceRef:                &workspacev1alpha1.ServiceReference{Name: restartUpstreamName, Port: upPort},
	}
	if err := k8sClient.Status().Update(ctx, ws); err != nil {
		t.Fatalf("publish workspace status: %v", err)
	}
}

// dsnFor returns this fixture's DSN tagged with a per-replica
// application_name, so tests can sever one replica's connections in
// pg_stat_activity without touching the other's.
func (f *restartFixture) dsnFor(replica string) string {
	return f.dsn + "&application_name=tcdi-it-" + replica
}

// flags is the shared replica flag set — one gateway id, one login key set,
// one binding source — with only the listen addresses left at :0.
func (f *restartFixture) flags(replica string) []string {
	return []string{
		"-listen", "127.0.0.1:0", // plain HTTP — the drill drives it directly
		"-database-url", f.dsnFor(replica),
		"-dev-insecure-db",
		"-oidc-issuer", f.iss.URL(),
		"-oidc-client-id", f.iss.ClientID,
		"-oidc-redirect-url", restartPortalOrigin + restartPortalPath,
		"-tenant-namespaces", restartTenant + "=" + restartNamespace,
		"-kubeconfig", f.kubeconfig,
		"-login-key-file", f.loginKey,
		"-portal-origin", restartPortalOrigin,
		"-session-listen", "127.0.0.1:0",
		"-session-tls-cert", f.sessCert,
		"-session-tls-key", f.sessKey,
		"-session-origin", restartSessionOrigin,
		"-session-allowed-hosts", restartSessionHost,
		"-session-cookie-mode", "lax",
		"-gateway-id", restartGatewayID,
		"-gateway-audience", restartGatewayAud,
		"-renew-interval", "500ms",
		"-revoke-deadline", "5s",
		"-internal-listen", "",
		"-metrics-listen", "",
	}
}

// ---------------------------------------------------------------------------
// Replica lifecycle and clients
// ---------------------------------------------------------------------------

type replica struct {
	name    string
	b       *backend.Backend
	cancel  context.CancelFunc
	done    chan error
	stopped bool
	appURL  string
	sessURL string
	client  *http.Client
}

func (f *restartFixture) startReplica(t *testing.T, name string) *replica {
	t.Helper()
	cfg, err := backend.ParseFlags(f.flags(name), func(string) string { return "" })
	if err != nil {
		t.Fatalf("ParseFlags(%s): %v", name, err)
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	b, err := backend.New(context.Background(), cfg, log)
	if err != nil {
		t.Fatalf("backend.New(%s): %v", name, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &replica{
		name:   name,
		b:      b,
		cancel: cancel,
		done:   make(chan error, 1),
	}
	app, session, _, _ := b.Addrs()
	r.appURL = "http://" + app // the app listener runs plain HTTP in the drill
	r.sessURL = "https://" + session
	r.client = &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	go func() { r.done <- b.Run(ctx) }()
	return r
}

// stop cancels the replica's context — the graceful path (drain, listener
// shutdown) — and waits for Run to return.
func (r *replica) stop(t *testing.T) {
	t.Helper()
	if r.stopped {
		return
	}
	r.stopped = true
	r.cancel()
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatalf("replica %s run: %v", r.name, err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("replica %s did not shut down", r.name)
	}
}

func (r *replica) cleanup(t *testing.T) {
	t.Helper()
	r.stop(t)
}

// ---------------------------------------------------------------------------
// Flow helpers: portal login, ticket issue, session launch, session traffic
// ---------------------------------------------------------------------------

// portalLogin runs the OIDC round trip: GET /v1/login on loginOn, the fake
// issuer redirect, then the callback against callbackOn — which may be the
// other replica. Returns the API session and CSRF values.
func (f *restartFixture) portalLogin(t *testing.T, loginOn, callbackOn *replica) (sessionID, csrf string) {
	t.Helper()
	resp, err := loginOn.client.Get(loginOn.appURL + "/v1/login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	loginLoc := resp.Header.Get("Location")
	var loginCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == restartLoginCookie {
			loginCookie = c
		}
	}
	drainBody(resp)
	if resp.StatusCode != http.StatusFound || loginLoc == "" {
		t.Fatalf("login status=%d location=%q", resp.StatusCode, loginLoc)
	}
	if loginCookie == nil {
		t.Fatalf("login set no %s cookie", restartLoginCookie)
	}

	// The fake issuer redirects straight back with code+state.
	resp2, err := callbackOn.client.Get(loginLoc)
	if err != nil {
		t.Fatalf("issuer authorize: %v", err)
	}
	cbLoc := resp2.Header.Get("Location")
	drainBody(resp2)
	cb, err := url.Parse(cbLoc)
	if err != nil || cb.Query().Get("code") == "" || cb.Query().Get("state") == "" {
		t.Fatalf("issuer callback location %q", cbLoc)
	}

	req, err := http.NewRequest(http.MethodGet,
		callbackOn.appURL+restartPortalPath+"?"+cb.RawQuery, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(loginCookie)
	resp3, err := callbackOn.client.Do(req)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	cookies := resp3.Cookies()
	drainBody(resp3)
	for _, c := range cookies {
		if c.Name == restartSessionCookie {
			sessionID = c.Value
		}
		if c.Name == restartCSRFCookie {
			csrf = c.Value
		}
	}
	if sessionID == "" || csrf == "" {
		t.Fatalf("callback issued no session/csrf cookie (status %d)", resp3.StatusCode)
	}
	return sessionID, csrf
}

// issueTicket mints one launch ticket for the fixture workspace through the
// replica's app listener. Retried until the replica's binding informer has
// observed the Ready workspace.
func (f *restartFixture) issueTicket(t *testing.T, r *replica, sessionID, csrf string) string {
	t.Helper()
	var ticket string
	eventually(t, "launch ticket issue", 20*time.Second, func() bool {
		req, err := http.NewRequest(http.MethodPost,
			r.appURL+"/v1/workspaces/"+f.wsUID+"/connections", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Cookie", restartSessionCookie+"="+sessionID)
		req.Header.Set(restartCSRFHeader, csrf)
		resp, err := r.client.Do(req)
		if err != nil {
			return false
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			return false
		}
		var v struct {
			Ticket string `json:"ticket"`
		}
		if err := json.Unmarshal(body, &v); err != nil || v.Ticket == "" {
			return false
		}
		ticket = v.Ticket
		return true
	})
	return ticket
}

// launch POSTs the ticket to the replica's session listener and returns the
// session cookie value the replica issued.
func (f *restartFixture) launch(t *testing.T, r *replica, ticket string) string {
	t.Helper()
	form := url.Values{"ticket": {ticket}}
	req, err := http.NewRequest(http.MethodPost, r.sessURL+"/v1/launch",
		strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = restartSessionHost
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", restartPortalOrigin)
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer drainBody(resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("launch status=%d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == restartSessionCookie {
			return c.Value
		}
	}
	t.Fatal("launch set no session cookie")
	return ""
}

// sessionGet performs a cookie-authenticated GET on the replica's session
// listener.
func (f *restartFixture) sessionGet(t *testing.T, r *replica, path, cookie string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, r.sessURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = restartSessionHost
	req.Header.Set("Cookie", restartSessionCookie+"="+cookie)
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatalf("session GET %s: %v", path, err)
	}
	return resp
}

// wsOpen opens a websocket upgrade on the replica's session listener,
// requiring the 101 switch.
func (f *restartFixture) wsOpen(t *testing.T, r *replica, cookie string) *http.Response {
	t.Helper()
	resp := f.wsTry(r, cookie)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		drainBody(resp)
		t.Fatalf("websocket upgrade = %d, want 101", resp.StatusCode)
	}
	return resp
}

// wsTry attempts the upgrade once, returning the response whatever its
// status; a 101 response carries the live socket in Body.
func (f *restartFixture) wsTry(r *replica, cookie string) *http.Response {
	req, err := http.NewRequest(http.MethodGet, r.sessURL+"/websockify", nil)
	if err != nil {
		return nil
	}
	req.Host = restartSessionHost
	req.Header.Set("Connection", "upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Origin", restartSessionOrigin)
	req.Header.Set("Cookie", restartSessionCookie+"="+cookie)
	resp, err := r.client.Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		return nil
	}
	return resp
}

// wsEcho proves the open stream round-trips bytes through gateway and
// upstream.
func wsEcho(t *testing.T, resp *http.Response) {
	t.Helper()
	w, ok := resp.Body.(io.Writer)
	if !ok {
		t.Fatal("upgraded body is not writable")
	}
	payload := []byte("ping-frame")
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("ws write: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("ws echo read: %v", err)
	}
	if string(buf) != string(payload) {
		t.Fatalf("ws echo mismatch %q", buf)
	}
}

// wsClosed reports whether the upgraded connection has gone away: a read
// that returns any error within the deadline means the peer closed it.
func wsClosed(resp *http.Response) bool {
	done := make(chan error, 1)
	go func() {
		_, err := resp.Body.Read(make([]byte, 1))
		done <- err
	}()
	select {
	case err := <-done:
		return err != nil
	case <-time.After(5 * time.Second):
		return false
	}
}

func drainBody(resp *http.Response) {
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// ---------------------------------------------------------------------------
// DB assertions
// ---------------------------------------------------------------------------

func (f *restartFixture) activeLeaseID(t *testing.T) string {
	t.Helper()
	var id string
	err := f.db.Pool().QueryRow(context.Background(),
		`SELECT id FROM connection_lease WHERE workspace_id = $1 AND state = 'active'`,
		f.wsUID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("lease query: %v", err)
	}
	return id
}

func (f *restartFixture) ticketCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.db.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM launch_ticket WHERE workspace_id = $1`,
		f.wsUID).Scan(&n); err != nil {
		t.Fatalf("ticket count: %v", err)
	}
	return n
}

func (f *restartFixture) openStreams(t *testing.T) int {
	t.Helper()
	var n int
	err := f.db.Pool().QueryRow(context.Background(),
		`SELECT open_streams FROM workspace_activity
		 WHERE workspace_id = $1 AND runtime_generation = 1`, f.wsUID).Scan(&n)
	if err != nil {
		return -1
	}
	return n
}

// ---------------------------------------------------------------------------
// The drill
// ---------------------------------------------------------------------------

// TestRestart_CookieWorksOnOtherReplica (D19): a session cookie minted by
// replica A authorizes a request on replica B — B rebuilds the session from
// cookie digest → live lease through the shared Postgres.
func TestRestart_CookieWorksOnOtherReplica(t *testing.T) {
	f := newRestartFixture(t)
	a := f.startReplica(t, "a")
	defer a.cleanup(t)
	b := f.startReplica(t, "b")
	defer b.cleanup(t)

	sess, csrf := f.portalLogin(t, a, a)
	cookie := f.launch(t, a, f.issueTicket(t, a, sess, csrf))

	resp := f.sessionGet(t, b, "/", cookie)
	defer drainBody(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / on second replica = %d, want 200", resp.StatusCode)
	}
}

// TestRestart_StreamResumesAfterReplicaStops (D23): an open stream on A dies
// when A's context is cancelled; a new stream to B with the same cookie
// opens inside the 15 s resume budget without any new launch ticket.
func TestRestart_StreamResumesAfterReplicaStops(t *testing.T) {
	f := newRestartFixture(t)
	a := f.startReplica(t, "a")
	defer a.cleanup(t)
	b := f.startReplica(t, "b")
	defer b.cleanup(t)

	sess, csrf := f.portalLogin(t, a, a)
	cookie := f.launch(t, a, f.issueTicket(t, a, sess, csrf))
	tickets := f.ticketCount(t)

	stream := f.wsOpen(t, a, cookie)
	wsEcho(t, stream)

	a.stop(t)
	if !wsClosed(stream) {
		stream.Body.Close()
		t.Fatal("stream survived the replica stop")
	}
	stream.Body.Close()

	var resumed *http.Response
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if r := f.wsTry(b, cookie); r != nil {
			if r.StatusCode == http.StatusSwitchingProtocols {
				resumed = r
				break
			}
			drainBody(r)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if resumed == nil {
		t.Fatal("stream did not resume on the second replica within 15 s")
	}
	defer resumed.Body.Close()
	wsEcho(t, resumed)

	if got := f.ticketCount(t); got != tickets {
		t.Fatalf("launch_ticket rows grew during resume: %d -> %d", tickets, got)
	}
}

// TestRestart_RollingRestartBothReplicas: a rolling restart of both replicas
// (stop A, start A′, stop B, start B′) never forces a re-launch — the lease
// row and the launch_ticket table are untouched; the client just reconnects
// with the same cookie after each stream close.
func TestRestart_RollingRestartBothReplicas(t *testing.T) {
	f := newRestartFixture(t)
	a := f.startReplica(t, "a")
	defer a.cleanup(t)
	b := f.startReplica(t, "b")
	defer b.cleanup(t)

	sess, csrf := f.portalLogin(t, a, a)
	cookie := f.launch(t, a, f.issueTicket(t, a, sess, csrf))
	tickets := f.ticketCount(t)

	stream := f.wsOpen(t, a, cookie)
	wsEcho(t, stream)
	leaseID := f.activeLeaseID(t)
	if leaseID == "" {
		t.Fatal("no active lease after launch")
	}

	// Replica A rolls: its stream dies, the client reconnects to B.
	a.stop(t)
	if !wsClosed(stream) {
		t.Fatal("stream on A survived A's stop")
	}
	stream.Body.Close()

	var onB *http.Response
	eventually(t, "stream reopens on B", 15*time.Second, func() bool {
		if r := f.wsTry(b, cookie); r != nil {
			if r.StatusCode == http.StatusSwitchingProtocols {
				onB = r
				return true
			}
			drainBody(r)
		}
		return false
	})
	wsEcho(t, onB)

	// A′ is healthy before B rolls — one replica is always serving.
	a2 := f.startReplica(t, "a2")
	defer a2.cleanup(t)
	waitReady(t, f, a2)

	b.stop(t)
	if !wsClosed(onB) {
		t.Fatal("stream on B survived B's stop")
	}
	onB.Body.Close()

	var onA2 *http.Response
	eventually(t, "stream reopens on A′", 15*time.Second, func() bool {
		if r := f.wsTry(a2, cookie); r != nil {
			if r.StatusCode == http.StatusSwitchingProtocols {
				onA2 = r
				return true
			}
			drainBody(r)
		}
		return false
	})
	wsEcho(t, onA2)

	b2 := f.startReplica(t, "b2")
	defer b2.cleanup(t)

	if got := f.activeLeaseID(t); got != leaseID {
		t.Fatalf("lease changed across the rolling restart: %s -> %s", leaseID, got)
	}
	if got := f.ticketCount(t); got != tickets {
		t.Fatalf("launch_ticket rows grew across the rolling restart: %d -> %d", tickets, got)
	}
	onA2.Body.Close()
}

// waitReady blocks until the replica can answer an authenticated session
// request — its binding informer has synced and it would serve real
// traffic. The cookie itself need not be bound on this replica.
func waitReady(t *testing.T, f *restartFixture, r *replica) {
	t.Helper()
	eventually(t, "replica "+r.name+" serving", 15*time.Second, func() bool {
		req, err := http.NewRequest(http.MethodGet, r.sessURL+"/", nil)
		if err != nil {
			return false
		}
		req.Host = restartSessionHost
		resp, err := r.client.Do(req)
		if err != nil {
			return false
		}
		drainBody(resp)
		return resp.StatusCode == http.StatusUnauthorized
	})
}

// TestRestart_LoginSurvivesReplicaChange (D20): a login started on A lands
// its sealed state in the __Host-tcdi_login cookie, so the callback can
// complete on B — the session cookie is issued there.
func TestRestart_LoginSurvivesReplicaChange(t *testing.T) {
	f := newRestartFixture(t)
	a := f.startReplica(t, "a")
	defer a.cleanup(t)
	b := f.startReplica(t, "b")
	defer b.cleanup(t)

	sess, csrf := f.portalLogin(t, a, b)
	if sess == "" || csrf == "" {
		t.Fatal("login started on A did not complete on B")
	}

	// The issued session is usable on the replica that completed the login.
	req, err := http.NewRequest(http.MethodGet, b.appURL+"/v1/workspaces", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", restartSessionCookie+"="+sess)
	resp, err := b.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer drainBody(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/workspaces on B = %d, want 200", resp.StatusCode)
	}
}

// TestRestart_CertRotationKeepsStreams (D21): rotating the session
// listener's certificate files changes what new handshakes see while the
// already-upgraded stream keeps echoing on its established TLS connection.
func TestRestart_CertRotationKeepsStreams(t *testing.T) {
	f := newRestartFixture(t)
	a := f.startReplica(t, "a")
	defer a.cleanup(t)

	sess, csrf := f.portalLogin(t, a, a)
	cookie := f.launch(t, a, f.issueTicket(t, a, sess, csrf))
	stream := f.wsOpen(t, a, cookie)
	defer stream.Body.Close()
	wsEcho(t, stream)

	// Rotate the session cert pair: new CN, atomic rename into place.
	newCert, newKey := writeCertPair(t, f.dir, "it-backend-session-rotated",
		[]string{restartSessionHost}, []net.IP{net.ParseIP("127.0.0.1")})
	if err := os.Rename(newCert, f.sessCert); err != nil {
		t.Fatalf("rotate cert: %v", err)
	}
	if err := os.Rename(newKey, f.sessKey); err != nil {
		t.Fatalf("rotate key: %v", err)
	}

	// A new handshake must present the rotated certificate. The reloader
	// polls the files, so allow well past its interval.
	eventually(t, "rotated session certificate", 90*time.Second, func() bool {
		conn, err := tls.Dial("tcp", strings.TrimPrefix(a.sessURL, "https://"),
			&tls.Config{InsecureSkipVerify: true})
		if err != nil {
			return false
		}
		defer conn.Close()
		peer := conn.ConnectionState().PeerCertificates
		return len(peer) > 0 && peer[0].Subject.CommonName == "it-backend-session-rotated"
	})

	// The established stream still echoes on the old TLS connection.
	wsEcho(t, stream)
}

// TestRestart_HardKillExpiresLeaseCleanly: when a replica vanishes without
// draining — no lease renewals, no disconnect report reaches the broker —
// the lease expires at its TTL and the next session lookup lazily closes
// stream accounting: open_streams returns to 0.
func TestRestart_HardKillExpiresLeaseCleanly(t *testing.T) {
	f := newRestartFixture(t)
	a := f.startReplica(t, "a")
	defer a.cleanup(t)
	b := f.startReplica(t, "b")
	defer b.cleanup(t)

	sess, csrf := f.portalLogin(t, a, a)
	cookie := f.launch(t, a, f.issueTicket(t, a, sess, csrf))
	stream := f.wsOpen(t, a, cookie)
	defer stream.Body.Close()

	// Wait for the "connected" signal to land in Postgres, then sever A's
	// database: the replica keeps running but can neither renew nor report —
	// the observable equivalent of a kernel-killed process.
	eventually(t, "open_streams = 1", 10*time.Second, func() bool {
		return f.openStreams(t) == 1
	})
	leaseID := f.activeLeaseID(t)
	if leaseID == "" {
		t.Fatal("no active lease to expire")
	}
	unsever := f.severDB(t, "a")
	defer unsever() // idempotent — also called explicitly below

	// The reaper kills a conn every sweep, but pgx reconnects in the gaps
	// and a renewal that lands between sweeps still commits. Pin the lease
	// row FOR UPDATE for the TTL window: every renewal that escapes the
	// reaper blocks on the lock and is killed in turn — expires_at is
	// frozen at its pre-sever value.
	unlock := f.lockLeaseRow(t, leaseID)
	time.Sleep(broker.LeaseTTL + 3*time.Second)
	unlock()

	// The same cookie on B must now be refused — and resolving it lazily
	// marks the lease expired, closing the stream accounting in the same
	// transaction.
	eventually(t, "open_streams zeroed after lease expiry", 20*time.Second, func() bool {
		resp := f.sessionGet(t, b, "/", cookie)
		defer drainBody(resp)
		return resp.StatusCode == http.StatusUnauthorized && f.openStreams(t) == 0
	})

	unsever()
	a.stop(t)
}

// lockLeaseRow holds FOR UPDATE on the lease row inside a fixture
// transaction; the returned func releases it. A renewal that slips between
// the connection reaper's sweeps blocks on this lock until the reaper kills
// its conn — the lease's expiry can no longer slide forward.
func (f *restartFixture) lockLeaseRow(t *testing.T, leaseID string) (unlock func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	var id string
	if err := tx.QueryRow(ctx,
		`SELECT id FROM connection_lease WHERE id = $1 FOR UPDATE`, leaseID).Scan(&id); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("lock lease row: %v", err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = tx.Rollback(rctx)
		})
	}
}

// severDB terminates every backend connection of the named replica, then
// keeps reaping new ones — pgx pools reconnect, so a single kill would only
// hiccup. The returned function stops the reaper.
func (f *restartFixture) severDB(t *testing.T, replica string) (unsever func()) {
	t.Helper()
	appName := "tcdi-it-" + replica
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, err := f.db.Pool().Exec(context.Background(), `
				SELECT pg_terminate_backend(pid)
				FROM pg_stat_activity
				WHERE application_name = $1 AND pid <> pg_backend_pid()`, appName); err != nil {
				t.Logf("terminate %s conns: %v", appName, err)
			}
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()
	// Prove the sever bit before returning: no live connection of the
	// replica remains.
	eventually(t, "replica "+replica+" connections severed", 10*time.Second, func() bool {
		var n int
		if err := f.db.Pool().QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity WHERE application_name = $1`,
			appName).Scan(&n); err != nil {
			return false
		}
		return n == 0
	})
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			<-done
		})
	}
}

// TestSvcDNS_ResolvesUpstream guards the fixture machinery itself: cluster
// Service names must resolve to loopback for the fake runtime upstream.
func TestSvcDNS_ResolvesUpstream(t *testing.T) {
	ensureSvcDNS(t)
	addrs, err := net.DefaultResolver.LookupHost(context.Background(),
		restartUpstreamName+"."+restartNamespace+".svc")
	if err != nil {
		t.Fatalf("resolve svc name: %v", err)
	}
	if len(addrs) != 1 || addrs[0] != "127.0.0.1" {
		t.Fatalf("svc name resolved to %v, want [127.0.0.1]", addrs)
	}
}
