package scsi

import "testing"

func TestFixedSenseLayout(t *testing.T) {
	b := FixedSense(SenseNotReady, AscMediumNotPresent, AscqMediumNotPresent, senseFlags{})
	if len(b) != fixedSenseLength {
		t.Fatalf("length = %d, want %d", len(b), fixedSenseLength)
	}
	if b[0] != 0x70 {
		t.Errorf("response code = %#x, want 0x70", b[0])
	}
	if key := b[2] & 0x0F; key != SenseNotReady {
		t.Errorf("sense key = %#x, want %#x", key, SenseNotReady)
	}
	if b[7] != fixedSenseLength-8 {
		t.Errorf("additional sense length = %d, want %d", b[7], fixedSenseLength-8)
	}
	if b[12] != AscMediumNotPresent || b[13] != AscqMediumNotPresent {
		t.Errorf("ASC/ASCQ = %#x/%#x, want %#x/%#x", b[12], b[13], AscMediumNotPresent, AscqMediumNotPresent)
	}
}

func TestFixedSenseFlags(t *testing.T) {
	b := FixedSense(SenseVolumeOverflow, AscEndOfPartitionMediumDetected, AscqEndOfPartitionMediumDetected, senseFlags{EOM: true, Filemark: true, ILI: true})
	if b[2]&0x80 == 0 {
		t.Error("FILEMARK bit not set")
	}
	if b[2]&0x40 == 0 {
		t.Error("EOM bit not set")
	}
	if b[2]&0x20 == 0 {
		t.Error("ILI bit not set")
	}
	if key := b[2] & 0x0F; key != SenseVolumeOverflow {
		t.Errorf("sense key = %#x, want %#x", key, SenseVolumeOverflow)
	}
}

func TestFixedSenseWithInfo(t *testing.T) {
	b := FixedSenseWithInfo(SenseNoSense, 0, 0, senseFlags{ILI: true}, 7)
	if b[0]&0x80 == 0 {
		t.Error("VALID bit not set")
	}
	got := uint32(b[3])<<24 | uint32(b[4])<<16 | uint32(b[5])<<8 | uint32(b[6])
	if got != 7 {
		t.Errorf("information field = %d, want 7", got)
	}
}
