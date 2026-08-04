package scsi

import "testing"

// TestFamilyForRealisticIdentityCoverage confirms exactly which
// generations have a Milestone 5 realistic identity profile defined -
// LTO-8/LTO-9 only (see DriveFamilies' own doc comment for why DDS/DLT are
// deliberately left blank). A caller (cmd/gotochanger-tcmud) must check
// for the zero value before swapping RealisticIdentity in; this test
// guards that the zero-value/non-zero-value split stays exactly as
// documented.
func TestFamilyForRealisticIdentityCoverage(t *testing.T) {
	for _, tc := range []struct {
		generation    string
		wantVendor    string
		wantRealistic bool
	}{
		{"LTO-8", "IBM", true},
		{"LTO-9", "IBM", true},
		{"DDS", "", false},
		{"DLT", "", false},
		{"Unlimited", "", false},
		{"totally-unknown-generation", "", false},
	} {
		fam := FamilyFor(tc.generation)
		hasRealistic := fam.RealisticIdentity != (Identity{})
		if hasRealistic != tc.wantRealistic {
			t.Errorf("generation %q: RealisticIdentity set = %v, want %v", tc.generation, hasRealistic, tc.wantRealistic)
		}
		if tc.wantRealistic && fam.RealisticIdentity.Vendor != tc.wantVendor {
			t.Errorf("generation %q: RealisticIdentity.Vendor = %q, want %q", tc.generation, fam.RealisticIdentity.Vendor, tc.wantVendor)
		}
		// The default Identity is always populated and always distinct
		// from RealisticIdentity when one exists - a caller swapping
		// blindly must never end up with an empty vendor/product either
		// way.
		if fam.Identity == (Identity{}) {
			t.Errorf("generation %q: default Identity is zero-value", tc.generation)
		}
	}
}

func TestDefaultAndRealisticChangerIdentityAreDistinctAndNonEmpty(t *testing.T) {
	if DefaultChangerIdentity == (Identity{}) {
		t.Error("DefaultChangerIdentity is zero-value")
	}
	if RealisticChangerIdentity == (Identity{}) {
		t.Error("RealisticChangerIdentity is zero-value")
	}
	if DefaultChangerIdentity == RealisticChangerIdentity {
		t.Error("DefaultChangerIdentity and RealisticChangerIdentity must be distinct")
	}
}
