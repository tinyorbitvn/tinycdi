// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
	"github.com/tinyorbitvn/tinycdi/internal/sessionhost"
)

// TestSessionListener_StalledBodyBounded (R1-05): an unauthenticated
// client that sends complete headers and then stalls its declared body
// must not hold a conn or a goroutine past the listener's read bound —
// neither on a misdirected (421) path, where the server drains the body
// after the handler, nor on /v1/launch, where ParseForm blocks.
func TestSessionListener_StalledBodyBounded(t *testing.T) {
	old := sessionReadTimeout
	sessionReadTimeout = time.Second
	t.Cleanup(func() { sessionReadTimeout = old })

	dom, err := sessionhost.ParseDomain("session.example.test")
	if err != nil {
		t.Fatal(err)
	}
	gw, err := gateway.New(gateway.Config{
		Identity:      broker.GatewayIdentity{ID: "gw-test", Audience: "session.example.test"},
		SessionDomain: dom,
		ControlHosts:  []string{"backend.tinycdi.svc"},
		Broker:        stubBrokerClient{},
	})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	t.Cleanup(gw.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := sessionServer(ln.Addr().String(), gw)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	for name, head := range map[string]string{
		"misdirected 421": "POST /anything HTTP/1.1\r\nHost: attacker.example\r\nContent-Length: 200000\r\n\r\n",
		"launch": "POST /v1/launch?tcdi_tab=0123456789abcdef0123456789abcdef HTTP/1.1\r\n" +
			"Host: ws-deadbeef.session.example.test\r\nContent-Type: application/x-www-form-urlencoded\r\n" +
			"Content-Length: 4000\r\n\r\nticket=",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if _, err := fmt.Fprint(c, head); err != nil {
				t.Fatal(err)
			}
			// The server must close the conn within a few bounds; a read
			// that times out on our side means it is still held open.
			_ = c.SetReadDeadline(time.Now().Add(5 * sessionReadTimeout))
			_, err = io.Copy(io.Discard, c)
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatalf("conn still held open %v after a stalled body (bound %v)", 5*sessionReadTimeout, sessionReadTimeout)
			}
		})
	}
}
