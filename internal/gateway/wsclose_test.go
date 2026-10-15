// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package gateway

import (
	"bytes"
	"net"
	"testing"
	"time"
)

// readAll drains the pipe into one accumulated slice until it errors.
func readAll(c net.Conn, acc chan<- []byte) {
	var out []byte
	buf := make([]byte, 512)
	for {
		n, err := c.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			acc <- out
			return
		}
	}
}

// TestWsBoundary tracks frame boundaries through arbitrary chunkings — the
// copy loop splits frames wherever the buffer cut falls, so every possible
// split point must still yield the same boundary stream.
func TestWsBoundary(t *testing.T) {
	stream := append(frame(0x2, bytes.Repeat([]byte{1}, 3), false),
		append(frame(0x9, nil, false), frame(0x2, bytes.Repeat([]byte{2}, 200), false)...)...)

	var tr wsBoundary
	if !tr.atBoundary() {
		t.Fatal("a fresh tracker starts at a boundary")
	}
	tr.feed(stream)
	if !tr.atBoundary() {
		t.Fatal("not at a boundary after the full stream")
	}

	// Byte-by-byte must reach the same states at the same positions.
	var inc wsBoundary
	for i := 0; i < len(stream); i++ {
		var whole wsBoundary
		whole.feed(stream[:i+1])
		inc.feed(stream[i : i+1])
		if inc.atBoundary() != whole.atBoundary() {
			t.Fatalf("boundary mismatch after %d bytes: inc=%v whole=%v", i+1, inc.atBoundary(), whole.atBoundary())
		}
	}
}

// TestSniffingGracefulClose_Boundary: a conn already sitting at a frame
// boundary gets the 1001 close frame immediately, then dies.
func TestSniffingGracefulClose_Boundary(t *testing.T) {
	left, right := net.Pipe()
	defer right.Close()
	sc := &sniffingConn{Conn: left, sniffer: &wsFrameSniffer{}, done: make(chan struct{})}

	got := make(chan []byte, 1)
	go readAll(right, got)

	payload := frame(0x2, []byte("ab"), false)
	if _, err := sc.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	sc.gracefulClose()

	select {
	case b := <-got:
		want := append(append([]byte{}, payload...), wsCloseGoingAway...)
		if !bytes.Equal(b, want) {
			t.Fatalf("stream = %x, want %x", b, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("conn not closed at a boundary")
	}
	if _, err := sc.Write([]byte{0x1}); err == nil {
		t.Fatal("write after close did not fail")
	}
}

// TestSniffingGracefulClose_MidFrame: mid-frame the close is deferred to the
// boundary the copy loop reaches — never injected inside frame bytes.
func TestSniffingGracefulClose_MidFrame(t *testing.T) {
	left, right := net.Pipe()
	defer right.Close()
	sc := &sniffingConn{Conn: left, sniffer: &wsFrameSniffer{}, done: make(chan struct{})}

	got := make(chan []byte, 1)
	go readAll(right, got)

	f := frame(0x2, bytes.Repeat([]byte{7}, 10), false)
	all := f
	// Stop mid-payload: the stream is inside a frame.
	if _, err := sc.Write(all[:len(all)-6]); err != nil {
		t.Fatalf("partial write: %v", err)
	}
	// gracefulClose blocks until the close lands (drainStreams runs it in a
	// goroutine for exactly this reason).
	closed := make(chan struct{})
	go func() { sc.gracefulClose(); close(closed) }()
	for i := 0; i < 100; i++ { // wait until the pending close is armed
		sc.mu.Lock()
		p := sc.closePending
		sc.mu.Unlock()
		if p {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The rest of the frame flows through untouched, THEN the close lands.
	if _, err := sc.Write(all[len(all)-6:]); err == nil {
		t.Fatal("the boundary-completing write did not report close")
	}
	<-closed
	select {
	case b := <-got:
		want := append(append([]byte{}, all...), wsCloseGoingAway...)
		if !bytes.Equal(b, want) {
			t.Fatalf("stream = %x, want %x", b, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("conn not closed at the boundary")
	}
}

// TestSniffingGracefulClose_NoBoundary: a conn stuck mid-frame falls back to
// a bare close when the budget expires — never a corrupt stream.
func TestSniffingGracefulClose_NoBoundary(t *testing.T) {
	left, right := net.Pipe()
	sc := &sniffingConn{Conn: left, sniffer: &wsFrameSniffer{}, done: make(chan struct{})}

	got := make(chan []byte, 1)
	go readAll(right, got)

	f := frame(0x2, bytes.Repeat([]byte{7}, 10), false)
	all := f
	if _, err := sc.Write(all[:len(all)-6]); err != nil {
		t.Fatalf("partial write: %v", err)
	}

	sc.gracefulClose()
	select {
	case b := <-got:
		// The bare close lands with the stream mid-frame and no close
		// frame was ever injected into it.
		if bytes.Contains(b[len(all)-6:], wsCloseGoingAway) {
			t.Fatalf("close frame injected mid-frame: %x", b)
		}
	case <-time.After(gracefulCloseBudget + 2*time.Second):
		t.Fatal("bare close did not happen after the budget")
	}
}

// TestSniffingGracefulClose_StalledWriter: a conn whose copy-loop Write is
// parked on TCP backpressure must not hold gracefulClose open — the
// budget-expired abort closes the conn without waiting on the writer lock,
// so Drain stays inside its window.
func TestSniffingGracefulClose_StalledWriter(t *testing.T) {
	left, right := net.Pipe()
	defer right.Close()
	sc := &sniffingConn{Conn: left, sniffer: &wsFrameSniffer{}, done: make(chan struct{})}

	writeDone := make(chan error, 1)
	go func() {
		// net.Pipe is unbuffered: this blocks until the peer reads (it
		// never does) or the conn dies.
		_, err := sc.Write(frame(0x2, bytes.Repeat([]byte{7}, 10), false))
		writeDone <- err
	}()
	// Let the writer park inside Conn.Write holding c.mu.
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	sc.gracefulClose()
	if d := time.Since(start); d > gracefulCloseBudget+time.Second {
		t.Fatalf("gracefulClose stalled %s behind a blocked writer", d)
	}
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("parked write did not error after the abort")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parked write never unblocked")
	}
}
