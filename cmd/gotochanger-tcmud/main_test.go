package main

import "testing"

func TestVpdIdentifierDeterministic(t *testing.T) {
	a := vpdIdentifier("Library1-drive0")
	b := vpdIdentifier("Library1-drive0")
	if a != b {
		t.Errorf("vpdIdentifier(%q) = %x, then %x - want the same value both times", "Library1-drive0", a, b)
	}
}

func TestVpdIdentifierDiffersByName(t *testing.T) {
	a := vpdIdentifier("Library1-drive0")
	b := vpdIdentifier("Library1-drive1")
	if a == b {
		t.Errorf("vpdIdentifier for two different names both = %x, want distinct values", a)
	}
}

func TestVpdIdentifierForcesNAATypeNibble(t *testing.T) {
	for _, name := range []string{"Library1-changer0", "Library1-drive0", "default-drive3"} {
		got := vpdIdentifier(name)
		if got[0]&0xF0 != 0x50 {
			t.Errorf("vpdIdentifier(%q)[0] = %#x, want top nibble 0x5", name, got[0])
		}
	}
}
