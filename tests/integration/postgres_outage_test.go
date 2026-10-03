//go:build integration

// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package integration

// Postgres outage drill (decision E6): the fail-closed budget after the last
// successful lease renew is -revoke-deadline (gateway default 30 s). An
// outage shorter than the deadline keeps streams; a longer one closes them
// and users reconnect with the same cookie while the lease row is still
// live, else re-launch. The app listener must answer 503 UNAVAILABLE while
// the store is unreachable — never hang and never fake an auth verdict.
//
// Each drill test runs its own Postgres container on a FIXED loopback port:
// `docker stop`/`docker start` is the failover shape (all backend
// connections severed at once) and — unlike the suite's shared ephemeral
// port — a bound port survives the restart, so the replica's DSN keeps
// working after recovery without re-wiring the fixture.
// Replica flags pin -renew-interval 100ms/-revoke-deadline 30s so the renew
// phase is tight around the outage start and the kill lands where the
// deadline says.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
	"github.com/tinyorbitvn/tinycdi/internal/backend"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// outagePG is one drill test's dedicated Postgres container: fixed port so
// the DSN survives docker stop/start.
type outagePG struct {
	container string
	adminDSN  string
}

// freeLoopbackPort asks the kernel for an unused port. The gap between
// closing the probe socket and docker binding it is a small race —
// startOutagePG retries the run on a bind failure.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// startOutagePG launches the pinned Postgres image on a fixed loopback port
// and waits for readiness.
func startOutagePG(t *testing.T) *outagePG {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("postgres outage drill needs docker")
	}
	var pwBytes [16]byte
	if _, err := rand.Read(pwBytes[:]); err != nil {
		t.Fatal(err)
	}
	pw := hex.EncodeToString(pwBytes[:])
	var sfx [4]byte
	if _, err := rand.Read(sfx[:]); err != nil {
		t.Fatal(err)
	}
	p := &outagePG{container: fmt.Sprintf("tcdi-it-pgo-%d-%s", os.Getpid(), hex.EncodeToString(sfx[:]))}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		port := freeLoopbackPort(t)
		out, err := exec.Command("docker", "run", "-d",
			"--name", p.container,
			"-e", "POSTGRES_PASSWORD="+pw,
			"-e", "POSTGRES_DB=postgres",
			"-p", fmt.Sprintf("127.0.0.1:%d:5432", port),
			pgImage).CombinedOutput()
		if err == nil {
			p.adminDSN = fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%d/postgres?sslmode=disable&pool_max_conns=16", pw, port)
			break
		}
		lastErr = fmt.Errorf("docker run postgres: %v\n%s", err, out)
		if !strings.Contains(string(out), "port is already allocated") &&
			!strings.Contains(string(out), "address already in use") {
			break
		}
	}
	if p.adminDSN == "" {
		t.Fatalf("start outage postgres: %v", lastErr)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", p.container).Run()
	})
	p.waitReady(t, 60*time.Second)
	return p
}

// waitReady polls until Postgres answers on adminDSN.
func (p *outagePG) waitReady(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var err error
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		db, e := store.Open(ctx, p.adminDSN)
		cancel()
		if e == nil {
			db.Close()
			return
		}
		err = e
		if time.Now().After(deadline) {
			t.Fatalf("postgres %s did not become ready: %v", p.container, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// stop halts the container and returns the instant `docker stop` was issued
// — the test's "outage began" timestamp.
func (p *outagePG) stop(t *testing.T) time.Time {
	t.Helper()
	t0 := time.Now()
	if out, err := exec.Command("docker", "stop", p.container).CombinedOutput(); err != nil {
		t.Fatalf("docker stop %s: %v\n%s", p.container, err, out)
	}
	return t0
}

// start brings the container back (fixed port: the DSN is unchanged) and
// waits for Postgres to accept connections again — restart plus crash
// recovery is not instant.
func (p *outagePG) start(t *testing.T) {
	t.Helper()
	if out, err := exec.Command("docker", "start", p.container).CombinedOutput(); err != nil {
		t.Fatalf("docker start %s: %v\n%s", p.container, err, out)
	}
	p.waitReady(t, 30*time.Second)
}

// outageDB creates a fresh migrated database inside the drill container,
// mirroring restartDB: the returned DSN is what backend replicas dial.
func (p *outagePG) outageDB(t *testing.T) (*store.DB, string) {
	t.Helper()
	ctx := context.Background()

	admin, err := store.Open(ctx, p.adminDSN)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	name := fmt.Sprintf("it_pgo_%d_%d", time.Now().UnixNano()%1_000_000, dbCounter.Add(1))
	if _, err := admin.Pool().Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatalf("create database: %v", err)
	}
	dbURL := strings.Replace(p.adminDSN, "/postgres?", "/"+name+"?", 1)
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

// newOutageFixture is newRestartFixture over the drill's own Postgres
// container — the restart-drill machinery (OIDC issuer, fake runtime
// upstream, svc DNS shim, envtest binding source) is unchanged.
func newOutageFixture(t *testing.T) (*restartFixture, *outagePG) {
	t.Helper()
	ensureSvcDNS(t)
	if k8sClient == nil || testEnvConfig == nil {
		t.Skip("envtest unavailable (KUBEBUILDER_ASSETS)")
	}
	pg := startOutagePG(t)
	db, dsn := pg.outageDB(t)

	iss, err := oidctest.NewIssuer()
	if err != nil {
		t.Fatalf("oidctest: %v", err)
	}
	iss.Subject = restartSubject
	iss.TenantID = restartTenant
	t.Cleanup(iss.Close)

	dir := t.TempDir()
	sessCert, sessKey := writeCertPair(t, filepath.Join(dir, "session"), "it-backend-session",
		[]string{restartSessionDomain, "*." + restartSessionDomain}, []net.IP{net.ParseIP("127.0.0.1")})
	loginKey := filepath.Join(dir, "login.key")
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatal(err)
	}
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
	return f, pg
}

// syncLogBuf is a goroutine-safe slog sink: backend goroutines keep writing
// while the test reads it (leader election runs through the outage).
type syncLogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *syncLogBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncLogBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// startReplicaLogged is startReplicaWith but with slog captured so the
// drill can assert WHY a session died (renew_deadline vs broker_unreachable
// vs a terminal broker error).
func (f *restartFixture) startReplicaLogged(t *testing.T, name string, extra ...string) (*replica, *syncLogBuf) {
	t.Helper()
	cfg, err := backend.ParseFlags(append(f.flags(name), extra...), func(string) string { return "" })
	if err != nil {
		t.Fatalf("ParseFlags(%s): %v", name, err)
	}
	logs := &syncLogBuf{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
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
	r.appURL = "http://" + app
	r.sessURL = "https://" + session
	r.client = &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	go func() { r.done <- b.Run(ctx) }()
	return r, logs
}

// wsEchoWithin is wsEcho bounded by a deadline — used where socket liveness
// is the assertion itself, so a missing echo must fail, not hang the suite.
func wsEchoWithin(t *testing.T, resp *http.Response, timeout time.Duration) {
	t.Helper()
	w, ok := resp.Body.(io.Writer)
	if !ok {
		t.Fatal("upgraded body is not writable")
	}
	payload := []byte("ping-frame")
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("ws write: %v", err)
	}
	type echoRes struct {
		buf []byte
		err error
	}
	done := make(chan echoRes, 1)
	go func() {
		buf := make([]byte, len(payload))
		_, err := io.ReadFull(resp.Body, buf)
		done <- echoRes{buf, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("ws echo read: %v", r.err)
		}
		if string(r.buf) != string(payload) {
			t.Fatalf("ws echo mismatch %q", r.buf)
		}
	case <-time.After(timeout):
		t.Fatalf("ws echo timed out after %v", timeout)
	}
}

// wsWaitClose blocks until the upgraded conn yields an error or the timeout
// passes, returning the wall time the close was observed. Wall (not the
// monotonic reading) because the anchor timestamps come from Postgres
// columns — same clock domain, no mono/wall drift in the deltas.
func wsWaitClose(resp *http.Response, timeout time.Duration) (closedAt time.Time, closed bool) {
	done := make(chan struct{}, 1)
	go func() {
		_, _ = resp.Body.Read(make([]byte, 1))
		done <- struct{}{}
	}()
	select {
	case <-done:
		return time.Now().Round(0), true
	case <-time.After(timeout):
		return time.Now().Round(0), false
	}
}

// leaseRenewedAt reads the active lease's last_renewed_at — the timestamp
// the broker wrote on the last successful renew, i.e. the backend's own
// fail-closed epoch (same process clock as the gateway's renew loop).
func leaseRenewedAt(t *testing.T, f *restartFixture) time.Time {
	t.Helper()
	var ts time.Time
	err := f.db.Pool().QueryRow(context.Background(),
		`SELECT last_renewed_at FROM connection_lease
		 WHERE workspace_id = $1 AND state = 'active'`, f.wsUID).Scan(&ts)
	if err != nil {
		t.Fatalf("last_renewed_at: %v", err)
	}
	return ts
}

// awaitFreshRenew polls until last_renewed_at advances past prev, so the
// outage starts immediately after a known-good renew — the fail-closed
// clock then runs almost exactly from outage start.
func awaitFreshRenew(t *testing.T, f *restartFixture, prev time.Time) time.Time {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var ts time.Time
		if err := f.db.Pool().QueryRow(context.Background(),
			`SELECT last_renewed_at FROM connection_lease
			 WHERE workspace_id = $1 AND state = 'active'`, f.wsUID).Scan(&ts); err == nil {
			if ts.After(prev) {
				return ts
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("lease was not renewed inside 15 s")
	return time.Time{}
}

// leaseStillLive reports whether the workspace's newest lease row would
// still serve: state 'active' and not yet expired.
func leaseStillLive(t *testing.T, f *restartFixture) bool {
	t.Helper()
	var live bool
	if err := f.db.Pool().QueryRow(context.Background(),
		`SELECT state = 'active' AND expires_at > now()
		 FROM connection_lease WHERE workspace_id = $1
		 ORDER BY created_at DESC LIMIT 1`, f.wsUID).Scan(&live); err != nil {
		t.Fatalf("lease liveness: %v", err)
	}
	return live
}

// workspacesGet performs GET /v1/workspaces on the replica's app listener
// with the API session cookie and reports status, error code and latency.
func workspacesGet(t *testing.T, r *replica, sessionID string) (status int, code string, took time.Duration) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, r.appURL+"/v1/workspaces", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", restartSessionCookie+"="+sessionID)
	start := time.Now()
	resp, err := r.client.Do(req)
	took = time.Since(start)
	if err != nil {
		t.Fatalf("GET /v1/workspaces: %v", err)
	}
	defer drainBody(resp)
	var v struct {
		Code string `json:"code"`
	}
	body, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(body, &v)
	return resp.StatusCode, v.Code, took
}

// TestPGOutage_ShortKeepsStreams (E6): a 10 s outage is inside the 30 s
// fail-closed budget — the open socket survives and echoes after recovery.
func TestPGOutage_ShortKeepsStreams(t *testing.T) {
	f, pg := newOutageFixture(t)
	a := f.startReplicaWith(t, "a", "-renew-interval", "100ms", "-revoke-deadline", "30s")
	defer a.cleanup(t)

	sess, csrf := f.portalLogin(t, a, a)
	cookie := f.launch(t, a, f.issueTicket(t, a, sess, csrf))
	stream := f.wsOpen(t, a, cookie)
	defer stream.Body.Close()
	wsEcho(t, stream)

	t0 := pg.stop(t)
	// Mid-outage the socket must still serve — the data path never touches
	// Postgres, and the 10 s silence is inside the 30 s fail-closed budget.
	wsEchoWithin(t, stream, 5*time.Second)
	if d := time.Since(t0); d < 10*time.Second {
		time.Sleep(10*time.Second - d)
	}
	pg.start(t)

	// After recovery the same socket still echoes end to end, and the
	// lease renew loop picks up again (proves the store reconnected).
	wsEchoWithin(t, stream, 5*time.Second)
	awaitFreshRenew(t, f, leaseRenewedAt(t, f))
	t.Logf("short outage: socket stayed open and echoes after recovery")
}

// TestPGOutage_LongClosesStreams (E6): a 45 s outage exceeds the 30 s
// fail-closed deadline — the socket dies ~30 s after the last successful
// renew, i.e. between 30 s and 40 s after the outage began.
func TestPGOutage_LongClosesStreams(t *testing.T) {
	f, pg := newOutageFixture(t)
	a, logs := f.startReplicaLogged(t, "a", "-renew-interval", "100ms", "-revoke-deadline", "30s")
	defer a.cleanup(t)

	sess, csrf := f.portalLogin(t, a, a)
	cookie := f.launch(t, a, f.issueTicket(t, a, sess, csrf))
	stream := f.wsOpen(t, a, cookie)
	defer stream.Body.Close()
	wsEcho(t, stream)

	// Pin the renew phase: prev was committed before fresh, so its broker
	// response necessarily reached the gateway while Postgres was still up —
	// it is provably counted. The gateway's kill is strictly after its own
	// lastRenewOK + 30 s, and lastRenewOK >= prev, so close lands strictly
	// after prev + 30 s. prev sits within ~one renew interval of the outage
	// start — for this stream the outage began when renews stopped landing.
	prev := leaseRenewedAt(t, f)
	awaitFreshRenew(t, f, prev)
	time.Sleep(150 * time.Millisecond)
	t0 := pg.stop(t).Round(0) // wall clock — same domain as the DB columns
	closeAt, closed := wsWaitClose(stream, 42*time.Second)
	if !closed {
		t.Fatal("stream still open 42 s into a Postgres outage")
	}
	sinceRenew := closeAt.Sub(prev)
	elapsed := closeAt.Sub(t0)
	t.Logf("long outage: stream closed %.2f s after docker stop, %.2f s after last counted renew",
		elapsed.Seconds(), sinceRenew.Seconds())
	killLogged := false
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "session closed") {
			t.Logf("gateway log: %s", line)
			if strings.Contains(line, "renew_deadline") {
				killLogged = true
			}
		}
	}
	if !killLogged {
		t.Fatal("session was not torn down by the renew deadline")
	}
	// Fail-closed: the kill lands strictly after the 30 s budget expires at
	// the last counted renew (renew_deadline) — the spec's 30–40 s window
	// measured against the stream's outage epoch.
	if sinceRenew < 30*time.Second || sinceRenew > 40*time.Second {
		t.Fatalf("stream closed %.2f s after the renew budget epoch, want 30–40 s", sinceRenew.Seconds())
	}
	// Docker-stop epoch sanity: the close must still sit inside the spec
	// window. One renew interval below 30 s covers the (rare) case where no
	// renew was counted inside the shutdown window itself.
	if elapsed < 29500*time.Millisecond || elapsed > 40*time.Second {
		t.Fatalf("stream closed %.2f s after docker stop, want ~30 s", elapsed.Seconds())
	}
	if d := time.Since(t0); d < 45*time.Second {
		time.Sleep(45*time.Second - d)
	}
	pg.start(t)
}

// TestPGOutage_ReconnectAfterRecovery (E6): after the long outage the lease
// row decides the reconnect path — still live: the same cookie re-opens a
// stream (cookie rehydration, D19); expired: a fresh launch ticket must be
// issued. The test records which outcome applied.
func TestPGOutage_ReconnectAfterRecovery(t *testing.T) {
	f, pg := newOutageFixture(t)
	a := f.startReplicaWith(t, "a", "-renew-interval", "100ms", "-revoke-deadline", "30s")
	defer a.cleanup(t)

	sess, csrf := f.portalLogin(t, a, a)
	cookie := f.launch(t, a, f.issueTicket(t, a, sess, csrf))
	stream := f.wsOpen(t, a, cookie)
	defer stream.Body.Close()
	wsEcho(t, stream)
	tickets := f.ticketCount(t)

	t0 := pg.stop(t)
	if d := 45*time.Second - time.Since(t0); d > 0 {
		time.Sleep(d)
	}
	pg.start(t)

	// The socket the outage killed stays dead — resume means a new one.
	if leaseStillLive(t, f) {
		// Lease outlived the outage: the same cookie must re-open a stream.
		t.Logf("lease still live after recovery — cookie resume path")
		resumed := f.wsOpen(t, a, cookie)
		defer resumed.Body.Close()
		wsEcho(t, resumed)
		if got := f.ticketCount(t); got != tickets {
			t.Fatalf("launch_ticket rows grew on cookie resume: %d -> %d", tickets, got)
		}
		return
	}

	// Lease expired during the outage: the dead cookie is refused and the
	// user re-launches with a new ticket on the still-valid API session.
	t.Logf("lease expired during outage — re-launch path")
	if r := f.wsTry(a, cookie); r != nil {
		if r.StatusCode == http.StatusSwitchingProtocols {
			r.Body.Close()
			t.Fatal("expired-lease cookie re-opened a stream after the outage")
		}
		drainBody(r)
	}
	newCookie := f.launch(t, a, f.issueTicket(t, a, sess, csrf))
	resumed := f.wsOpen(t, a, newCookie)
	defer resumed.Body.Close()
	wsEcho(t, resumed)
	if got := f.ticketCount(t); got <= tickets {
		t.Fatalf("re-launch issued no new ticket: %d -> %d", tickets, got)
	}
}

// TestPGOutage_APIAnswers503 (E6): with Postgres down, an authenticated
// GET /v1/workspaces answers 503 UNAVAILABLE inside 5 s — not a hang, and
// not a fake auth verdict (401/500 misleads the portal into re-login).
func TestPGOutage_APIAnswers503(t *testing.T) {
	f, pg := newOutageFixture(t)
	a := f.startReplica(t, "a")
	defer a.cleanup(t)

	sess, _ := f.portalLogin(t, a, a)
	eventually(t, "baseline GET /v1/workspaces", 20*time.Second, func() bool {
		s, _, _ := workspacesGet(t, a, sess)
		return s == http.StatusOK
	})

	pg.stop(t)
	status, code, took := workspacesGet(t, a, sess)
	if status != http.StatusServiceUnavailable || code != "UNAVAILABLE" {
		t.Fatalf("GET /v1/workspaces during outage = %d %q, want 503 UNAVAILABLE", status, code)
	}
	if took > 5*time.Second {
		t.Fatalf("GET /v1/workspaces took %v during outage, want <= 5 s", took)
	}
	t.Logf("during outage GET /v1/workspaces = 503 UNAVAILABLE in %v", took)

	pg.start(t)
	// After recovery the API serves the same session again without re-login.
	eventually(t, "GET /v1/workspaces healthy after recovery", 15*time.Second, func() bool {
		s, _, _ := workspacesGet(t, a, sess)
		return s == http.StatusOK
	})
}
