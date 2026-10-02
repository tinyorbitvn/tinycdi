package gateway

// Session activity reporting (design §8): the gateway reports
// "connected" when an interactive stream is admitted, "disconnect" when it
// closes, and "input" — at most once per InputReportInterval — when a
// client->upstream WebSocket binary frame carries an RFB KeyEvent (type 4)
// or PointerEvent (type 5, including KasmVNC's extended pointer message).
// WebSocket pings/pongs, text frames and upstream->client video are never
// reported. Stream open/close doubles as the broker's stream-count feed
// for /drain (the broker derives open streams from these events).
//
// Ordering: events for one session travel a single FIFO channel drained by
// one sender goroutine, so the broker sees them in causal order — a fenced
// stream's "disconnect" can never land after its successor's "connected"
// and arm the disconnect grace window while a stream is still open.
// Reports are best-effort convenience signals, not a security boundary:
// failures are dropped (a dead lease is handled by the fail-closed renew
// loop regardless).

import (
	"context"
	"encoding/binary"
	"net"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
)

// InputReportInterval is the minimum spacing between "input" activity
// reports per session — enough to hold the idle clock off without
// write-amplifying the store.
const InputReportInterval = 15 * time.Second

// activityQueueLen bounds the per-session report buffer; production
// traffic produces a handful of events per stream plus one input report
// per InputReportInterval, so a full queue is unreachable in practice.
const activityQueueLen = 64

// activityFlushBudget bounds the sender's post-death drain: when a session
// dies, tail events (the closing stream's "disconnect" enqueued by the
// proxy defer AFTER s.done closed) still get this long to reach the
// broker. Without it the sender exits on s.done and drops the disconnect —
// for kills whose lease is still live server-side (renew_deadline,
// broker_unreachable) that leak leaves open_streams stuck and the
// operator's drain step burns its whole budget.
const activityFlushBudget = 2 * time.Second

// activityEvent is one queued report: the signal plus the stream epoch of
// the stream it describes (0 for input, which is not stream-scoped, and when
// there is no session directory). The epoch lets the broker drop the late
// report of a stream another epoch already fenced.
type activityEvent struct {
	typ   broker.ActivityEventType
	epoch uint64
}

// activitySender drains the session's event queue in order. When the
// session dies it runs one bounded tail flush before exiting.
func (g *Gateway) activitySender(s *session) {
	for {
		select {
		case <-s.done:
			g.flushActivity(s)
			return
		case <-g.done:
			return
		case ev := <-s.events:
			g.reportActivity(context.Background(), s, ev)
		}
	}
}

// flushActivity delivers events queued at/after session death until the
// queue drains or the budget expires. The disconnect the killed conn's
// deferred close enqueues lands inside this window.
func (g *Gateway) flushActivity(s *session) {
	ctx, cancel := context.WithTimeout(context.Background(), activityFlushBudget)
	defer cancel()
	for {
		select {
		case ev := <-s.events:
			g.reportActivity(ctx, s, ev)
		case <-ctx.Done():
			return
		case <-g.done:
			return
		}
	}
}

// enqueueActivity queues one event for the ordered sender. It prefers the
// queue even on a dead session — the sender's tail flush still delivers —
// and only drops when the queue is full after death (the second select
// keeps a dead session's producer from blocking forever).
//
// The event counts as pending from this call — under the session lock —
// until its broker call returned, so Drain can never observe an empty
// queue and no in-flight call while a report is still on its way.
func (s *session) enqueueActivity(t broker.ActivityEventType, epoch uint64) {
	ev := activityEvent{typ: t, epoch: epoch}
	s.mu.Lock()
	s.pendingReports++
	s.mu.Unlock()
	select {
	case s.events <- ev:
		return
	default:
	}
	select {
	case s.events <- ev:
	case <-s.done:
		s.reportDone() // dropped: it will never reach the broker
	}
}

// reportDone retires one pending report (delivered, failed or dropped).
func (s *session) reportDone() {
	s.mu.Lock()
	s.pendingReports--
	s.mu.Unlock()
}

// reportActivity posts one activity event to the broker, bounded by ctx
// and at most 5 s. Called only by activitySender/flushActivity, so
// per-session ordering is preserved on the wire. The event stays counted in
// pendingReports (set at enqueue) until the broker call returns, which is
// how Drain knows a dequeued disconnect has actually reached the broker.
func (g *Gateway) reportActivity(ctx context.Context, s *session, ev activityEvent) {
	defer s.reportDone()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := g.cfg.Broker.ReportActivity(ctx, g.cfg.Identity, s.leaseID(),
		s.fenceSnapshot(), broker.ActivityEvent{Type: ev.typ, StreamEpoch: ev.epoch})
	cancel()
	if err != nil && g.cfg.Logger != nil {
		g.cfg.Logger.Debug("activity report failed", "type", string(ev.typ), "err", err)
	}
}

// noteInput records one observed RFB input signal, rate-limited to one
// broker report per InputReportInterval per session.
func (g *Gateway) noteInput(s *session) {
	if s.claimInputReport(g.now(), g.cfg.InputReportInterval) {
		s.enqueueActivity(broker.ActivityInput, 0)
	}
}

// claimInputReport admits at most one input report per interval.
func (s *session) claimInputReport(now time.Time, interval time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deadLocked() {
		return false
	}
	if !s.lastInputReport.IsZero() && now.Sub(s.lastInputReport) < interval {
		return false
	}
	s.lastInputReport = now
	return true
}

// ---------------------------------------------------------------------------
// WebSocket client->upstream frame sniffing
// ---------------------------------------------------------------------------

// wsFrameSniffer incrementally parses WebSocket frames off the client side
// of an upgraded stream and fires onInput when a binary frame's first
// payload byte is an RFB KeyEvent (4) or PointerEvent (5). Detection is
// best-effort: malformed or oversized input drops the pending parse and
// resynchronizes on the next bytes — every byte still passes through
// untouched.
type wsFrameSniffer struct {
	buf     []byte
	onInput func()
}

// sniffingConn wraps the hijacked client conn; Read feeds the sniffer
// before passing bytes up to the proxy's copy loop unchanged.
type sniffingConn struct {
	net.Conn
	sniffer *wsFrameSniffer
}

func (c *sniffingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.sniffer.feed(b[:n])
	}
	return n, err
}

// RFB client->server message types that count as user input.
const (
	rfbMsgKeyEvent     = 4
	rfbMsgPointerEvent = 5
)

// wsMaxFrame caps the parse buffer; beyond it the stream is treated as
// unparseable and the pending buffer is dropped (detection only).
const wsMaxFrame = 16 << 20

// feed consumes newly-read client bytes and parses complete WS frames.
func (s *wsFrameSniffer) feed(b []byte) {
	s.buf = append(s.buf, b...)
	for {
		// header: 2 bytes + optional extended length + optional mask key
		if len(s.buf) < 2 {
			return
		}
		op := s.buf[0] & 0x0f
		masked := s.buf[1]&0x80 != 0
		l := uint64(s.buf[1] & 0x7f)
		hdr := 2
		switch l {
		case 126:
			hdr += 2
		case 127:
			hdr += 8
		}
		if len(s.buf) < hdr {
			return
		}
		switch l {
		case 126:
			l = uint64(binary.BigEndian.Uint16(s.buf[2:4]))
		case 127:
			l = binary.BigEndian.Uint64(s.buf[2:10])
		}
		keyOff := hdr
		if masked {
			hdr += 4
		}
		if l > wsMaxFrame {
			s.buf = s.buf[:0] // unparseable/oversized: resync on next bytes
			return
		}
		if uint64(len(s.buf)) < uint64(hdr)+l {
			if cap(s.buf) < int(l)+hdr {
				s.buf = append(s.buf[:0:0], s.buf...) // grow for the big frame
			}
			return
		}
		payload := s.buf[hdr : hdr+int(l)]
		if op == 0x2 && l > 0 { // binary data frame
			b0 := payload[0]
			if masked {
				b0 ^= s.buf[keyOff]
			}
			if (b0 == rfbMsgKeyEvent || b0 == rfbMsgPointerEvent) && s.onInput != nil {
				s.onInput()
			}
		}
		s.buf = append(s.buf[:0:0], s.buf[hdr+int(l):]...)
	}
}
