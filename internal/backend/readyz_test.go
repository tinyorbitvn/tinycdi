// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func readyzStatus(h http.Handler) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rec.Code
}

func healthzStatus(h http.Handler) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	return rec.Code
}

// TestReadyz_FalseBeforeCacheSync (R1c): /readyz answers 200 only once Run is
// serving AND the Workspace informer cache has synced. Liveness never waits.
func TestReadyz_FalseBeforeCacheSync(t *testing.T) {
	b := &Backend{log: testLog()}
	h := b.readyz(http.NotFoundHandler())

	if got := readyzStatus(h); got != http.StatusServiceUnavailable {
		t.Fatalf("fresh backend /readyz = %d, want 503", got)
	}
	b.markServing()
	if got := readyzStatus(h); got != http.StatusServiceUnavailable {
		t.Fatalf("serving but cache not synced: /readyz = %d, want 503", got)
	}
	if got := healthzStatus(h); got != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200 regardless of readiness", got)
	}
	b.markCacheSynced()
	if got := readyzStatus(h); got != http.StatusOK {
		t.Fatalf("serving and synced: /readyz = %d, want 200", got)
	}

	// The reverse order must not matter, and synced alone is not ready.
	b2 := &Backend{log: testLog()}
	h2 := b2.readyz(http.NotFoundHandler())
	b2.markCacheSynced()
	if got := readyzStatus(h2); got != http.StatusServiceUnavailable {
		t.Fatalf("synced but Run not serving: /readyz = %d, want 503", got)
	}
	b2.markServing()
	if got := readyzStatus(h2); got != http.StatusOK {
		t.Fatalf("synced then serving: /readyz = %d, want 200", got)
	}
}

// blockingServer returns a namedServer whose handler holds every request
// until release is closed (or hold elapses) — an in-flight request that
// outlives a graceful shutdown.
func blockingServer(t *testing.T, name string, hold time.Duration, release <-chan struct{}) (namedServer, <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/slow" {
			w.WriteHeader(http.StatusOK)
			return
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-time.After(hold):
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	})
	var srv *http.Server
	switch name {
	case "app":
		srv = appServer(ln.Addr().String(), h)
	case "session":
		srv = sessionServer(ln.Addr().String(), h)
	default:
		srv = internalServer(ln.Addr().String(), h)
	}
	return namedServer{name: name, srv: srv, ln: ln}, entered
}

// TestReadyz_FalseAfterShutdownStarts (R1c): once Run begins shutting down,
// /readyz fails immediately — while an in-flight request still holds the
// shutdown open — so the pod leaves the Service before sockets close.
func TestReadyz_FalseAfterShutdownStarts(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	releaseAll := func() { once.Do(func() { close(release) }) }
	defer releaseAll()

	srv, entered := blockingServer(t, "app", time.Minute, release)
	b := &Backend{log: testLog(), servers: []namedServer{srv}}
	srv.srv.Handler = b.readyz(srv.srv.Handler)
	b.markCacheSynced()

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- b.Run(ctx) }()

	url := "http://" + srv.ln.Addr().String()
	waitFor(t, 5*time.Second, "/readyz 200 once Run serves", func() bool {
		resp, err := http.Get(url + "/readyz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	go func() {
		resp, err := http.Get(url + "/slow")
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	cancel()

	waitFor(t, 5*time.Second, "readiness to drop after shutdown starts", func() bool { return !b.ready.Load() })
	select {
	case <-runDone:
		t.Fatal("Run returned while a request was still in flight")
	default:
	}
	h := b.readyz(http.NotFoundHandler())
	if got := readyzStatus(h); got != http.StatusServiceUnavailable {
		t.Fatalf("/readyz after shutdown start = %d, want 503", got)
	}
	releaseAll()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the request finished")
	}
}

// TestShutdown_WithinGracePeriod (R1d): a slow in-flight request on each of
// the three listeners must not stack shutdown budgets — Run shares one
// deadline and shuts the listeners down in parallel, returning well inside
// the pod's 30 s terminationGracePeriodSeconds.
func TestShutdown_WithinGracePeriod(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	var servers []namedServer
	var entered []<-chan struct{}
	for _, name := range []string{"app", "session", "internal"} {
		s, e := blockingServer(t, name, 40*time.Second, release)
		servers = append(servers, s)
		entered = append(entered, e)
	}
	b := &Backend{log: testLog(), servers: servers}
	b.markCacheSynced()

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- b.Run(ctx) }()

	for i, s := range servers {
		url := "http://" + s.ln.Addr().String() + "/slow"
		go func() {
			resp, err := http.Get(url)
			if err == nil {
				resp.Body.Close()
			}
		}()
		select {
		case <-entered[i]:
		case <-time.After(5 * time.Second):
			t.Fatalf("slow request never reached listener %s", s.name)
		}
	}

	start := time.Now()
	cancel()
	select {
	case <-runDone:
		if d := time.Since(start); d >= 25*time.Second {
			t.Fatalf("Run took %v to shut down, want < 25s", d)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("Run did not return within 25s of ctx cancel (budgets stack per listener)")
	}
}
