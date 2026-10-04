// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package gateway

import (
	"encoding/binary"
	"net"
	"time"
)

// gracefulCloseBudget bounds how long a draining replica waits for a frame
// boundary (and how long the close write itself may take) before it falls
// back to the bare Close() every other path uses.
const gracefulCloseBudget = time.Second

// wsCloseGoingAway is the RFC 6455 close frame a draining replica writes
// before dropping a stream conn (FX-R32): FIN + opcode 0x8 plus a two-byte
// 1001 "going away" payload, unmasked as server->client frames always are.
// A clean close is what lets the KasmVNC client's own reconnect setting
// retry the websocket inside the frame — an abrupt TCP close surfaces as
// code 1006, which the client treats as a failure, not a retry cue.
var wsCloseGoingAway = []byte{0x88, 0x02, 0x03, 0xE9}

// wsBoundary tracks the byte position of the server->client frame stream
// on a proxied websocket. The proxy copy loop feeds it every byte range it
// writes; atBoundary reports exactly when the next byte would start a new
// frame — the only place a control frame may be injected without
// corrupting the stream. Only headers are parsed; payloads are counted,
// never read.
//
// The tracker starts at a boundary: the 101 response headers travel the
// hijack's bufio writer, which bypasses this conn — Write only ever sees
// post-handshake frame bytes.
type wsBoundary struct {
	hdr  [14]byte // current frame header: 2 + 8-byte length + 4-byte mask
	hdrN int      // header bytes collected so far
	want int      // header bytes the current frame needs (0 = undecided)
	left uint64   // payload bytes remaining in the current frame
}

// atBoundary is true when the next written byte begins a fresh frame: no
// frame header is half-collected and no frame payload is in flight.
func (t *wsBoundary) atBoundary() bool {
	return t.want == 0 && t.hdrN == 0 && t.left == 0
}

// feed consumes the byte range the copy loop is about to write.
func (t *wsBoundary) feed(b []byte) {
	for len(b) > 0 {
		if t.left > 0 {
			n := uint64(len(b))
			if n > t.left {
				n = t.left
			}
			t.left -= n
			b = b[n:]
			continue
		}
		// Header phase: the first two bytes decide the full header size.
		if t.hdrN < 2 {
			n := 2 - t.hdrN
			if n > len(b) {
				n = len(b)
			}
			copy(t.hdr[t.hdrN:], b[:n])
			t.hdrN += n
			b = b[n:]
			if t.hdrN < 2 {
				return
			}
			t.want = 2
			switch t.hdr[1] & 0x7f {
			case 126:
				t.want += 2
			case 127:
				t.want += 8
			}
			if t.hdr[1]&0x80 != 0 { // masked payload: +4-byte key
				t.want += 4
			}
		}
		n := t.want - t.hdrN
		if n > len(b) {
			n = len(b)
		}
		copy(t.hdr[t.hdrN:], b[:n])
		t.hdrN += n
		b = b[n:]
		if t.hdrN < t.want {
			return
		}
		switch t.hdr[1] & 0x7f {
		case 126:
			t.left = uint64(binary.BigEndian.Uint16(t.hdr[2:4]))
		case 127:
			t.left = binary.BigEndian.Uint64(t.hdr[2:10])
		default:
			t.left = uint64(t.hdr[1] & 0x7f)
		}
		t.hdrN, t.want = 0, 0
	}
}

// gracefulClose ends the websocket the way a draining replica should
// (FX-R32): a real close frame at a frame boundary inside
// gracefulCloseBudget, then the conn closes — and the call only returns
// once that work landed, so Drain can cancel the stream's request ctx
// without racing the close frame off the wire. If the stream is already at
// a boundary the frame goes out immediately; mid-frame it waits for the
// copy loop to reach the next one (Write injects the close); no boundary
// inside the budget falls back to the bare Close() every other path uses —
// a missed clean close only costs the client its in-frame retry, never its
// last-resort re-navigation.
func (c *sniffingConn) gracefulClose() {
	// The abort timer is armed before the lock: c.mu can sit behind a
	// Conn.Write parked on TCP backpressure, and only abort (which closes
	// without the lock) can unblock that writer — and with it, this call.
	timer := time.AfterFunc(gracefulCloseBudget, c.abort)
	c.mu.Lock()
	// done is lazily created: sniffingConns built by hand (tests) skip the
	// constructor, and a nil channel would hang the wait — or panic on
	// close — instead of reporting the close landed.
	if c.done == nil {
		c.done = make(chan struct{})
	}
	switch {
	case !c.closed && c.out.atBoundary():
		_ = c.Conn.SetWriteDeadline(time.Now().Add(gracefulCloseBudget))
		_, _ = c.Conn.Write(wsCloseGoingAway)
		_ = c.Conn.Close()
		c.finishLocked()
	case !c.closePending:
		c.closePending = true
	}
	c.mu.Unlock()
	<-c.done
	timer.Stop()
}

// finishLocked marks the conn's graceful-close work done once, releasing
// every gracefulClose caller waiting on done. Callers hold c.mu.
func (c *sniffingConn) finishLocked() {
	c.closed = true
	if c.done == nil {
		c.done = make(chan struct{})
	}
	select {
	case <-c.done:
	default:
		close(c.done)
	}
}

// abort is the budget-expired half of gracefulClose: no frame boundary came
// in time, so the conn dies like it always has. The Close() runs BEFORE the
// mutex — a copy-loop Write parked on backpressure holds c.mu, so grabbing
// it first would let one stalled client hold Drain open; Close() interrupts
// that write and the bookkeeping settles after.
func (c *sniffingConn) abort() {
	_ = c.Conn.Close()
	c.mu.Lock()
	c.finishLocked()
	c.mu.Unlock()
}

// gracefulCloseConn routes a stream conn through the frame-aware close when
// it can — conns the boundary tracker does not wrap fall back to Close().
func gracefulCloseConn(c net.Conn) {
	if sc, ok := c.(*sniffingConn); ok {
		sc.gracefulClose()
		return
	}
	_ = c.Close()
}
