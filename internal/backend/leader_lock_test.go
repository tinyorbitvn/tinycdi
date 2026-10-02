// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// blackholeProxy forwards TCP to target until blackhole is called; after that
// it keeps the sockets open but silently drops every byte in both directions —
// a partition the kernel never reports as an error.
type blackholeProxy struct {
	ln   net.Listener
	hole atomic.Bool
	mu   sync.Mutex
	cs   []net.Conn
}

func newBlackholeProxy(t *testing.T, target string) *blackholeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &blackholeProxy{ln: ln}
	t.Cleanup(func() {
		_ = ln.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, c := range p.cs {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			p.mu.Lock()
			p.cs = append(p.cs, c, up)
			p.mu.Unlock()
			go p.pipe(up, c)
			go p.pipe(c, up)
		}
	}()
	return p
}

func (p *blackholeProxy) pipe(dst, src net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 && !p.hole.Load() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *blackholeProxy) addr() string { return p.ln.Addr().String() }

// TestLeader_IterationSkippedWhenLockLost (R9f): the holder is cut off from
// the server by a silent partition (no RST, no FIN). Before each iteration it
// checks the lock on its own connection with a 2 s timeout; the check fails,
// so the loops are stopped within a few seconds instead of running on as a
// zombie holder until the kernel gives up.
func TestLeader_IterationSkippedWhenLockLost(t *testing.T) {
	db := newDB(t)
	cc := db.Pool().Config().ConnConfig
	proxy := newBlackholeProxy(t, net.JoinHostPort(cc.Host, strconv.Itoa(int(cc.Port))))

	cfg := leaderConnConfig(db)
	cfg.Host, cfg.Port = "127.0.0.1", mustPort(t, proxy.addr())
	cfg.Fallbacks = nil
	cfg.TLSConfig = nil

	b := &Backend{log: testLog()}
	var iterations atomic.Int64
	started := make(chan struct{})
	loopStopped := make(chan struct{})
	loop := func(ctx context.Context) {
		close(started)
		for {
			select {
			case <-ctx.Done():
				close(loopStopped)
				return
			case <-time.After(50 * time.Millisecond):
				iterations.Add(1)
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	held := make(chan struct{})
	go func() { b.holdLeadership(ctx, cfg, []func(context.Context){loop}); close(held) }()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("loops never started")
	}
	proxy.hole.Store(true)
	t0 := time.Now()

	select {
	case <-loopStopped:
	case <-time.After(8 * time.Second):
		t.Fatalf("loops still running %v after a silent partition; the lock was never re-checked", time.Since(t0))
	}
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("holdLeadership did not return after losing the lock")
	}
}

// TestLeader_ConnParamsSet (R9f): the lock connection asks the server to send
// TCP keepalives, so a silently dead client does not leave a zombie lock
// holder for hours.
func TestLeader_ConnParamsSet(t *testing.T) {
	db := newDB(t)
	cfg := leaderConnConfig(db)
	want := map[string]string{
		"tcp_keepalives_idle":     "5",
		"tcp_keepalives_interval": "2",
		"tcp_keepalives_count":    "3",
	}
	for k, v := range want {
		if got := cfg.RuntimeParams[k]; got != v {
			t.Errorf("RuntimeParams[%s] = %q, want %q", k, got, v)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	for k, v := range want {
		var got string
		if err := conn.QueryRow(ctx, `SELECT current_setting($1)`, k).Scan(&got); err != nil {
			t.Fatalf("show %s: %v", k, err)
		}
		if got != v {
			t.Errorf("server setting %s = %q, want %q", k, got, v)
		}
	}
}

func mustPort(t *testing.T, addr string) uint16 {
	t.Helper()
	_, ps, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.ParseUint(ps, 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	return uint16(n)
}
