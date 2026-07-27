package main

import (
	"strings"
	"testing"

	"github.com/swenske/gotochanger/internal/library"
)

func TestElementKindByAddress(t *testing.T) {
	st := library.Status{
		Slots: []*library.Slot{
			{Address: 1},
			{Address: 20},
		},
		IOSlots: []*library.IOSlot{
			{Address: 21},
			{Address: 24},
		},
	}

	tests := []struct {
		name    string
		addr    int
		want    string
		wantErr bool
	}{
		{name: "slot lower bound", addr: 1, want: "slot"},
		{name: "slot upper bound", addr: 20, want: "slot"},
		{name: "ioslot lower bound", addr: 21, want: "ioslot"},
		{name: "ioslot upper bound", addr: 24, want: "ioslot"},
		{name: "unknown address", addr: 25, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := elementKindByAddress(st, tt.addr)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for address %d", tt.addr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("kind mismatch: got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTransferKindsExportPath(t *testing.T) {
	st := library.Status{
		Slots:   []*library.Slot{{Address: 1}, {Address: 4}, {Address: 20}},
		IOSlots: []*library.IOSlot{{Address: 21}, {Address: 22}, {Address: 23}, {Address: 24}},
	}

	fromKind, toKind, err := transferKinds(st, 4, 22)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fromKind != "slot" || toKind != "ioslot" {
		t.Fatalf("unexpected kinds: got %s -> %s, want slot -> ioslot", fromKind, toKind)
	}
}

func TestTransferKindsUnknownDestination(t *testing.T) {
	st := library.Status{
		Slots:   []*library.Slot{{Address: 1}, {Address: 2}},
		IOSlots: []*library.IOSlot{{Address: 3}},
	}

	_, _, err := transferKinds(st, 2, 99)
	if err == nil {
		t.Fatal("expected error for unknown destination")
	}
	if !strings.Contains(err.Error(), "invalid destination address 99") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// Addressing itself (formerly presentedAddressing/buildPresentedAddressing,
// private to this binary) now lives in internal/addressing, shared with
// cmd/gotochanger-tcmud - see internal/addressing/addressing_test.go for
// its own tests.
