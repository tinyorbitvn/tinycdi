// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

// Shared Postgres harness for the backend contract tests — same pattern as
// internal/broker/main_test.go (own container, per-test database, embedded
// migrations). Skips cleanly when docker is unavailable; container names
// carry the tcdi-t15- prefix required of worker-owned docker objects.

import (
	"context"
	"crypto/rand"
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

var errDockerMissing = errors.New("docker not found")

func TestMain(m *testing.M) {
	code := m.Run()
	if pgContainer != "" {
		_ = exec.Command("docker", "rm", "-f", pgContainer).Run()
	}
	os.Exit(code)
}

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
	pgContainer = fmt.Sprintf("tcdi-t15-pg-%d-%s", os.Getpid(), hex.EncodeToString(sfx[:]))

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
	db := newBareDB(t)
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// newBareDB creates a fresh, empty database (no migrations applied) and
// returns a pool on it.
func newBareDB(t *testing.T) *store.DB {
	t.Helper()
	ensurePostgres(t)
	ctx := context.Background()

	admin, err := store.Open(ctx, pgAdminDSN)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	name := fmt.Sprintf("t15_%d_%d", time.Now().UnixNano()%1_000_000, dbCounter.Add(1))
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

// fakeBindings is a BindingSource the tests drive directly.
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
