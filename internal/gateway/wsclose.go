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

// gracefulClose ends the websocket the way a draining replica should (FX-R32):
// a real close frame at a frame boundary inside gracefulCloseBudget, then the
// conn closes. If the stream is already at a boundary the frame goes out
// immediately; mid-frame it waits for the copy loop to reach the next one
// (Write injects the close); no boundary inside the budget falls back to the
// bare Close() every other path uses — a missed clean close only costs the
// client its in-frame retry, never its last-resort re-navigation.
func (c *sniffingConn) gracefulClose() {
	c.mu.Lock()
	if c.closed || c.closePending {
		c.mu.Unlock()
		return
	}
	if c.out.atBoundary() {
		c.closed = true
		c.mu.Unlock()
		_ = c.Conn.SetWriteDeadline(time.Now().Add(gracefulCloseBudget))
		_, _ = c.Conn.Write(wsCloseGoingAway)
		_ = c.Conn.Close()
		return
	}
	c.closePending = true
	c.mu.Unlock()
	time.AfterFunc(gracefulCloseBudget, c.abort)
}

// abort is the budget-expired half of gracefulClose: no frame boundary came
// in time, so the conn dies like it always has.
func (c *sniffingConn) abort() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	_ = c.Conn.Close()
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
