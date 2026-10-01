package gateway

// Unit tests for the WebSocket frame sniffer (internal: the type is
// unexported). Detection is best-effort over a byte stream — frames split
// across reads must still parse.

import (
	"testing"
)

func sniffed(t *testing.T, chunks ...[]byte) int {
	t.Helper()
	n := 0
	s := &wsFrameSniffer{onInput: func() { n++ }}
	for _, c := range chunks {
		s.feed(c)
	}
	return n
}

func frame(opcode byte, payload []byte, masked bool) []byte {
	var b []byte
	b = append(b, 0x80|opcode)
	maskBit := byte(0)
	if masked {
		maskBit = 0x80
	}
	switch n := len(payload); {
	case n < 126:
		b = append(b, maskBit|byte(n))
	case n < 1<<16:
		b = append(b, maskBit|126, byte(n>>8), byte(n))
	default:
		b = append(b, maskBit|127, 0, 0, 0, 0, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	if masked {
		key := []byte{0x11, 0x22, 0x33, 0x44}
		b = append(b, key...)
		for i, p := range payload {
			b = append(b, p^key[i%4])
		}
		return b
	}
	return append(b, payload...)
}

var rfbKeyEvent = []byte{4, 1, 0, 0, 0, 0, 0x61}   // KeyEvent, keysym 'a'
var rfbPointerEvent = []byte{5, 0x1, 0, 10, 0, 10} // PointerEvent mask=1 x=10 y=10

func TestSniffer_KeyAndPointerAreInput(t *testing.T) {
	if n := sniffed(t, frame(0x2, rfbKeyEvent, true), frame(0x2, rfbPointerEvent, true)); n != 2 {
		t.Fatalf("key+pointer frames -> %d inputs, want 2", n)
	}
}

func TestSniffer_SplitAcrossReads(t *testing.T) {
	f := frame(0x2, rfbKeyEvent, true)
	if n := sniffed(t, f[:3], f[3:6], f[6:]); n != 1 {
		t.Fatalf("split frame -> %d inputs, want 1", n)
	}
}

func TestSniffer_NonInputIgnored(t *testing.T) {
	n := sniffed(t,
		frame(0x9, []byte("ping"), true),                 // ws ping
		frame(0xA, []byte("pong"), true),                 // ws pong
		frame(0x1, []byte{4, 0, 0, 0}, true),             // TEXT starting with 4: not RFB
		frame(0x2, []byte{3, 0, 0, 0, 0, 0}, true),       // binary: FramebufferUpdateRequest
		frame(0x2, []byte{6, 0, 0, 0, 0, 0, 0, 0}, true), // binary: ClientCutText
		frame(0x8, nil, true),                            // close
	)
	if n != 0 {
		t.Fatalf("non-input frames -> %d inputs, want 0", n)
	}
}

func TestSniffer_UnmaskedAlsoDetected(t *testing.T) {
	// Nonconforming (unmasked) client frames are still inspected — the
	// signal is advisory either way.
	if n := sniffed(t, frame(0x2, rfbKeyEvent, false)); n != 1 {
		t.Fatalf("unmasked key frame -> %d inputs, want 1", n)
	}
}

func TestSniffer_GarbageResyncs(t *testing.T) {
	// A non-frame blob mid-stream must not wedge the parser: the oversized
	// "length" drops the buffer and the next real frame still detects.
	garbage := []byte{0x82, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	if n := sniffed(t, garbage, frame(0x2, rfbKeyEvent, true)); n != 1 {
		t.Fatalf("garbage then key -> %d inputs, want 1", n)
	}
}
