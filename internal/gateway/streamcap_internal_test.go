// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package gateway

// The slot-release clamp (internal: acquire/release are unexported). A
// release with no slot held for the workspace must leave both counts
// untouched and log one ERROR — a negative in-flight count would
// under-report saturation and let acquire admit past the cap.

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func newStreamCapGateway(buf *bytes.Buffer) *Gateway {
	return &Gateway{
		cfg: Config{
			UpstreamStreamCap: 4,
			Logger:            slog.New(slog.NewTextHandler(buf, nil)),
		},
		upstreamSlots: map[string]int{},
	}
}

func TestStreamCap_ReleaseWithoutSlotRefused(t *testing.T) {
	var buf bytes.Buffer
	g := newStreamCapGateway(&buf)

	// Unpaired release on a workspace that never held a slot: nothing
	// decrements and exactly one ERROR is logged.
	g.releaseUpstreamStream("ws-x")
	if g.upstreamInFlight != 0 || len(g.upstreamSlots) != 0 {
		t.Fatalf("unpaired release moved counts: inFlight=%d slots=%v, want untouched", g.upstreamInFlight, g.upstreamSlots)
	}
	if n := strings.Count(buf.String(), "level=ERROR"); n != 1 {
		t.Fatalf("unpaired release logged %d ERRORs, want 1", n)
	}

	// A release naming the WRONG workspace must not eat a live slot or
	// drop the replica-wide count under it.
	if !g.acquireUpstreamStream("ws-a") {
		t.Fatal("acquire refused on an empty workspace")
	}
	g.releaseUpstreamStream("ws-b")
	if total, per := g.upstreamInFlight, g.upstreamSlots["ws-a"]; total != 1 || per != 1 {
		t.Fatalf("wrong-workspace release moved counts: inFlight=%d ws-a=%d, want 1/1", total, per)
	}

	// The paired release still works; a SECOND release of the same slot
	// is the unpaired one refused — the count stops at zero.
	g.releaseUpstreamStream("ws-a")
	g.releaseUpstreamStream("ws-a")
	if total, per := g.upstreamInFlight, g.upstreamSlots["ws-a"]; total != 0 || per != 0 {
		t.Fatalf("counts after paired + unpaired release = %d/%d, want 0/0 — no negatives", total, per)
	}
	if n := strings.Count(buf.String(), "level=ERROR"); n != 3 {
		t.Fatalf("ERROR count = %d, want 3 (one per unpaired release)", n)
	}
}
