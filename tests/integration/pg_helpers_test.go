//go:build integration

// Package integration holds the real-PostgreSQL admission tests.
package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	workspacev1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// pgImage is the locked postgres candidate digest from
// docs/compatibility.md (data_plane.postgresql).
const pgImage = "docker.io/library/postgres:18.0@sha256:41fc5342eefba6cc2ccda736aaf034bbbb7c3df0fdb81516eba1ba33f360162c"

// Postgres is started lazily on first use (newDB) under a container name
// unique to this test-binary run — parallel `go test -tags=integration` runs
// in the shared checkout can no longer delete each other's database.
var (
	pgAdminDSN  string
	pgContainer string
	pgOnce      sync.Once
	pgStartErr  error
	dbCounter   atomic.Int64
	k8sClient   client.Client
	testEnvStop func()
)

// errDockerMissing marks the docker-less environment skip path.
var errDockerMissing = fmt.Errorf("docker not found")

func TestMain(m *testing.M) {
	// envtest: real apiserver + etcd with the Workspace/WorkspaceTemplate
	// CRDs installed from config/crd/bases.
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		matches, _ := filepath.Glob(repoPath("bin/k8s/*-linux-amd64"))
		if len(matches) > 0 {
			os.Setenv("KUBEBUILDER_ASSETS", matches[0])
		}
	}
	if os.Getenv("KUBEBUILDER_ASSETS") != "" {
		scheme := runtime.NewScheme()
		if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
			panic(err)
		}
		if err := corev1.AddToScheme(scheme); err != nil {
			panic(err)
		}
		env := &envtest.Environment{
			CRDDirectoryPaths:     []string{repoPath("config/crd/bases")},
			ErrorIfCRDPathMissing: true,
		}
		cfg, err := env.Start()
		if err != nil {
			fmt.Fprintf(os.Stderr, "envtest start: %v\n", err)
			os.Exit(1)
		}
		k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
		if err != nil {
			panic(err)
		}
		for _, ns := range []string{"ns-a", "ns-b", "ns-e2e", "ns-crash"} {
			if err := k8sClient.Create(context.Background(), &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: ns},
			}); err != nil {
				panic(err)
			}
		}
		testEnvStop = func() { _ = env.Stop() }
	}

	code := m.Run()
	if testEnvStop != nil {
		testEnvStop()
	}
	if pgContainer != "" {
		_ = exec.Command("docker", "rm", "-f", pgContainer).Run()
	}
	os.Exit(code)
}

// startPostgres launches this run's own pinned Postgres container on a
// unique name and waits for readiness, storing the admin DSN on success.
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
	pgContainer = fmt.Sprintf("tcdi-it-pg-%d-%s", os.Getpid(), hex.EncodeToString(sfx[:]))

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
	// "5432/tcp -> 127.0.0.1:NNNNN" or plain "127.0.0.1:NNNNN"
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
// is unavailable and fail loudly when the pinned image won't start.
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

func repoPath(rel string) string {
	wd, _ := os.Getwd()
	return filepath.Join(wd, "..", "..", rel)
}

func countWorkspaceCRs(t *testing.T, ns string) int {
	t.Helper()
	list := &workspacev1alpha1.WorkspaceList{}
	if err := k8sClient.List(context.Background(), list, client.InNamespace(ns)); err != nil {
		t.Fatalf("list workspaces in %s: %v", ns, err)
	}
	return len(list.Items)
}

// newDB creates a fresh database inside the shared container, migrates it
// and registers cleanup. Each test gets an isolated schema.
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

// eventually polls cond until it returns true or the deadline passes.
func eventually(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// workspaceCRCount counts Workspace CRs carrying the given workspace UID
// label — namespace-independent so shared envtest namespaces stay usable.
func workspaceCRByUID(t *testing.T, wsUID string) *workspacev1alpha1.Workspace {
	t.Helper()
	list := &workspacev1alpha1.WorkspaceList{}
	if err := k8sClient.List(context.Background(), list,
		client.MatchingLabels{"workspaces.cdi.tinyorbit.vn/workspace-uid": wsUID}); err != nil {
		t.Fatalf("list workspace CRs: %v", err)
	}
	if len(list.Items) == 0 {
		return nil
	}
	return &list.Items[0]
}
