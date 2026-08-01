package api

import (
	"encoding/json"
	"testing"

	"github.com/swenske/gotochanger/internal/library"
)

// TestBroadcasterAbsorbsRealisticBurstWithoutDropping is the regression
// test for the SSE buffer-overflow bug: an "Opening…"/"Closing…" door
// overlay would sometimes flash briefly or not appear at all, because a
// realistic burst of live messages (arm narration + batched busy-element
// notifications, clustered right at a Move/Load/Unload's start/end, with
// more than one such operation able to finish around the same moment on
// a multi-drive library) could exceed the subscriber channel's old
// 8-message buffer, and broadcast silently drops on a full channel (see
// its own doc comment) rather than blocking. This sends a burst modeled
// on two concurrent Move-shaped operations plus one door phase
// transition (well under the new 64-deep buffer, comfortably over the
// old 8) to a subscriber that isn't draining yet, then confirms every
// single message survives once it does drain - including, critically,
// the door phase message even though it isn't the first or last in the
// burst.
func TestBroadcasterAbsorbsRealisticBurstWithoutDropping(t *testing.T) {
	b := NewBroadcaster()
	ch := b.Subscribe()
	defer b.Unsubscribe(ch)

	const burstSize = 20
	doorPhaseIndex := burstSize / 2
	for i := 0; i < burstSize; i++ {
		if i == doorPhaseIndex {
			b.NotifyPhase("magazine", "Magazine1", "opening")
			continue
		}
		b.NotifyElementBusy([]string{"slot:1", "drive:0"}, i%2 == 0)
	}

	for i := 0; i < burstSize; i++ {
		select {
		case msg := <-ch:
			if i == doorPhaseIndex {
				if msg.event != "phase" {
					t.Fatalf("message %d: expected the door phase transition to survive the burst in order, got event %q", i, msg.event)
				}
			}
		default:
			t.Fatalf("message %d of %d missing - the subscriber channel dropped a message it should have buffered", i, burstSize)
		}
	}
}

// TestNotifyElementBusySendsOneMessagePerCall confirms NotifyElementBusy
// batches every key into a single SSE message rather than one per key -
// this is the other half of the fix (halving the live-traffic burst
// Move/Load/Unload contribute at their start/end), alongside the widened
// buffer tested above.
func TestNotifyElementBusySendsOneMessagePerCall(t *testing.T) {
	b := NewBroadcaster()
	ch := b.Subscribe()
	defer b.Unsubscribe(ch)

	b.NotifyElementBusy([]string{"slot:3", "drive:1"}, true)

	var msg struct {
		Keys []string `json:"keys"`
		Busy bool     `json:"busy"`
	}
	select {
	case got := <-ch:
		if got.event != "busy" {
			t.Fatalf("expected event %q, got %q", "busy", got.event)
		}
		if err := json.Unmarshal([]byte(got.data), &msg); err != nil {
			t.Fatalf("decode busy message: %v", err)
		}
	default:
		t.Fatalf("expected one buffered busy message, got none")
	}
	if len(msg.Keys) != 2 || msg.Keys[0] != "slot:3" || msg.Keys[1] != "drive:1" || !msg.Busy {
		t.Fatalf("expected a single message carrying both keys, got %+v", msg)
	}

	select {
	case extra := <-ch:
		t.Fatalf("expected exactly one message for a single NotifyElementBusy call, got a second: %+v", extra)
	default:
	}
}

// Compile-time check that Broadcaster still satisfies both interfaces
// after the NotifyElementBusy signature change.
var (
	_ library.Notifier      = (*Broadcaster)(nil)
	_ library.PhaseNotifier = (*Broadcaster)(nil)
)
