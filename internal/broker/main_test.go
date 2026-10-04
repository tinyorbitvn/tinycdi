package broker_test

// Shared Postgres harness for the broker contract tests — same pattern as
// tests/integration/pg_helpers_test.go (own container, per-test database,
// embedded migrations), minus the integration build tag: these tests are the
// package's behavioural contract and must run under plain `go test`. They
// skip cleanly when docker is unavailable.
//
// Note: the container name is unique per test binary run and Postgres is
// started lazily on first use — parallel `go test` runs (e.g. other workers
// in the shared checkout) can no longer steal or remove each other's DB.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// pgImage is the locked postgres candidate digest from
// docs/compatibility.md (data_plane.postgresql).
const pgImage = "docker.io/library/postgres:18.0@sha256:41fc5342eefba6cc2ccda736aaf034bbbb7c3df0fdb81516eba1ba33f360162c"

var (
	pgAdminDSN  string
	pgContainer string
	pgOnce      sync.Once
	pgStartErr  error
	dbCounter   atomic.Int64
)

func TestMain(m *testing.M) {
	code := m.Run()
	if pgContainer != "" {
		_ = exec.Command("docker", "rm", "-f", pgContainer).Run()
	}
	os.Exit(code)
}

// errDockerMissing marks the docker-less environment skip path.
var errDockerMissing = fmt.Errorf("docker not found")

// startPostgres launches this run's own pinned Postgres container on a unique
// name and waits for readiness, storing the admin DSN on success.
func startPostgres() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return errDockerMissing
	}
	var pwBytes [16]byte
	if _, err := rand.Read(pwBytes[:]); err != nil {
		return err
	}
	pw := hex.EncodeToString(pwBytes[:])
	var sfx [4]byte
	if _, err := rand.Read(sfx[:]); err != nil {
		return err
	}
	pgContainer = fmt.Sprintf("tcdi-it-w3brk-pg-%d-%s", os.Getpid(), hex.EncodeToString(sfx[:]))

	run := exec.Command("docker", "run", "-d",
		"--name", pgContainer,
		"-e", "POSTGRES_PASSWORD="+pw,
		"-e", "POSTGRES_DB=postgres",
		"-p", "127.0.0.1::5432",
		pgImage)
	if out, err := run.CombinedOutput(); err != nil {
		return fmt.Errorf("docker run postgres: %v\n%s", err, out)
	}

	portOut, err := exec.Command("docker", "port", pgContainer, "5432").Output()
	if err != nil {
		return fmt.Errorf("docker port: %w", err)
	}
	var host string
	for _, line := range strings.Split(strings.TrimSpace(string(portOut)), "\n") {
		line = strings.TrimSpace(line)
		if i := strings.LastIndex(line, "->"); i >= 0 {
			line = strings.TrimSpace(line[i+2:])
		}
		if strings.HasPrefix(line, "127.0.0.1:") {
			host = line
			break
		}
	}
	if host == "" {
		return fmt.Errorf("no 127.0.0.1 port mapping for postgres: %q", portOut)
	}
	pgAdminDSN = fmt.Sprintf("postgres://postgres:%s@%s/postgres?sslmode=disable&pool_max_conns=16", pw, host)

	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		db, err := store.Open(ctx, pgAdminDSN)
		cancel()
		if err == nil {
			db.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("postgres did not become ready: %w", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// ensurePostgres lazily starts the run's container; tests skip when docker
// is unavailable, and fail loudly when the pinned image won't start.
func ensurePostgres(t *testing.T) {
	t.Helper()
	pgOnce.Do(func() { pgStartErr = startPostgres() })
	if errors.Is(pgStartErr, errDockerMissing) {
		t.Skipf("postgres test container unavailable: %v", pgStartErr)
	}
	if pgStartErr != nil {
		t.Fatalf("postgres test container: %v", pgStartErr)
	}
}

// newDB creates a fresh migrated database inside the shared container; each
// test gets an isolated schema.
func newDB(t *testing.T) *store.DB {
	t.Helper()
	ensurePostgres(t)
	ctx := context.Background()

	admin, err := store.Open(ctx, pgAdminDSN)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	name := fmt.Sprintf("it_%d_%d", time.Now().UnixNano()%1_000_000, dbCounter.Add(1))
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
	return db
}

// seedWorkspace inserts the workspace row tickets/leases reference.
func seedWorkspace(t *testing.T, db *store.DB, tenantID, ownerSubject, wsUID string) {
	t.Helper()
	_, err := db.Pool().Exec(context.Background(),
		`INSERT INTO workspaces (id, tenant_id, owner_subject, request_id) VALUES ($1, $2, $3, $4)`,
		wsUID, tenantID, ownerSubject, "req-"+wsUID)
	if err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
}

// seedPortalSession inserts the sessions row a ticket's
// portal_session_digest points at: redemption re-checks that the issuing
// portal session still exists (S17). The row key is the same digest form
// the API's store writes (hex of SHA-256).
func seedPortalSession(t *testing.T, db *store.DB, portalSessionID string) {
	t.Helper()
	sum := sha256.Sum256([]byte(portalSessionID))
	_, err := db.Pool().Exec(context.Background(), `
		INSERT INTO sessions (id, issuer, subject, tenant_id, groups,
			created_at, last_seen_at, expires_at, epoch)
		VALUES ($1, 'iss', 'sub', 'tenant-a', '[]', now(), now(),
			now() + interval '1 hour',
			(SELECT value FROM platform_meta WHERE key = 'session_epoch'))
		ON CONFLICT (id) DO NOTHING`,
		hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatalf("seed portal session: %v", err)
	}
}

// deletePortalSession removes the seeded sessions row — what the API's
// session delete does at sign-out.
func deletePortalSession(t *testing.T, db *store.DB, portalSessionID string) {
	t.Helper()
	sum := sha256.Sum256([]byte(portalSessionID))
	tag, err := db.Pool().Exec(context.Background(),
		`DELETE FROM sessions WHERE id = $1`, hex.EncodeToString(sum[:]))
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("delete portal session: %v (rows=%d)", err, tag.RowsAffected())
	}
}

// fakeClock is the deterministic time source injected into the broker.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeBindings is a BindingSource the tests drive directly: the operator's
// observed incarnation is whatever the test last set.
type fakeBindings struct {
	mu sync.Mutex
	m  map[broker.PlatformID]broker.RuntimeBinding
}

func newFakeBindings() *fakeBindings {
	return &fakeBindings{m: map[broker.PlatformID]broker.RuntimeBinding{}}
}

func (f *fakeBindings) set(b broker.RuntimeBinding) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[b.WorkspaceUID] = b
}

func (f *fakeBindings) CurrentBinding(_ context.Context, wsUID broker.PlatformID) (broker.RuntimeBinding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.m[wsUID]
	if !ok {
		return broker.RuntimeBinding{}, broker.ErrNotFound
	}
	return b, nil
}

// readyBinding returns a fresh Ready binding for wsUID anchored at now.
func readyBinding(wsUID broker.PlatformID, tenantID, owner string, gen uint64, rtUID string, now time.Time) broker.RuntimeBinding {
	return broker.RuntimeBinding{
		WorkspaceUID:      wsUID,
		TenantID:          tenantID,
		OwnerSubject:      owner,
		Phase:             "Ready",
		RuntimeGeneration: gen,
		RuntimeUID:        rtUID,
		ObservedAt:        now,
	}
}

// setup returns a migrated DB plus a broker on a fake clock and fake bindings.
func setup(t *testing.T) (*store.DB, *broker.Broker, *fakeClock, *fakeBindings) {
	t.Helper()
	db := newDB(t)
	clock := &fakeClock{now: time.Now()}
	src := newFakeBindings()
	b := broker.New(db, src, broker.WithClock(clock))
	return db, b, clock, src
}
