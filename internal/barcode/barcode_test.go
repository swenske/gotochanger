package barcode

import (
	"strings"
	"testing"
)

func TestGenerateLTO(t *testing.T) {
	spec := Spec{Family: FamilyLTO, MediaID: "L8", VolSerLength: 6}
	got, err := Generate(spec, 1)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got != "000001L8" {
		t.Fatalf("Generate(1) = %q, want %q", got, "000001L8")
	}
	got, err = Generate(spec, 42)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got != "000042L8" {
		t.Fatalf("Generate(42) = %q, want %q", got, "000042L8")
	}
}

func TestGenerateDLT(t *testing.T) {
	spec := Spec{Family: FamilyDLT, MediaID: "4", VolSerLength: 6}
	got, err := Generate(spec, 3)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got != "0000034" {
		t.Fatalf("Generate(3) = %q, want %q", got, "0000034")
	}
}

func TestGenerateSDLT(t *testing.T) {
	spec := Spec{Family: FamilySDLT, MediaID: "S2", VolSerLength: 6}
	got, err := Generate(spec, 7)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got != "000007S2" {
		t.Fatalf("Generate(7) = %q, want %q", got, "000007S2")
	}
}

func TestGenerateDDS(t *testing.T) {
	spec := Spec{Family: FamilyDDS, MediaID: "D6", VolSerLength: 6}
	got, err := Generate(spec, 1)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got != "000001D6" {
		t.Fatalf("Generate(1) = %q, want %q", got, "000001D6")
	}
}

func TestGenerateAIT(t *testing.T) {
	spec := Spec{Family: FamilyAIT, MediaID: "A1", VolSerLength: 6}
	got, err := Generate(spec, 1)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got != "000001A1" {
		t.Fatalf("Generate(1) = %q, want %q", got, "000001A1")
	}
}

func TestGenerateGeneric(t *testing.T) {
	spec := Spec{Family: FamilyGeneric, MediaID: "", VolSerLength: 8}
	got, err := Generate(spec, 1)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got != "00000001" {
		t.Fatalf("Generate(1) = %q, want %q", got, "00000001")
	}
}

func TestGenerateOverflow(t *testing.T) {
	spec := Spec{Family: FamilyLTO, MediaID: "L8", VolSerLength: 6}
	if _, err := Generate(spec, 1000000); err == nil {
		t.Fatalf("Generate(1000000) with VolSerLength=6: expected overflow error, got nil")
	}
}

func TestValidateSpecRejectsBadLTO(t *testing.T) {
	cases := []Spec{
		{Family: FamilyLTO, MediaID: "L8", VolSerLength: 5},  // wrong volser length
		{Family: FamilyLTO, MediaID: "L", VolSerLength: 6},   // media id too short
		{Family: FamilyLTO, MediaID: "L88", VolSerLength: 6}, // media id too long
		{Family: FamilyLTO, MediaID: "l8", VolSerLength: 6},  // lowercase media id
	}
	for _, spec := range cases {
		if err := ValidateSpec(spec); err == nil {
			t.Errorf("ValidateSpec(%+v): expected error, got nil", spec)
		}
	}
}

func TestValidateSpecAcceptsGoodSpecs(t *testing.T) {
	cases := []Spec{
		{Family: FamilyLTO, MediaID: "L8", VolSerLength: 6},
		{Family: FamilyDLT, MediaID: "", VolSerLength: 6},
		{Family: FamilyDLT, MediaID: "4", VolSerLength: 6},
		{Family: FamilySDLT, MediaID: "S", VolSerLength: 6},
		{Family: FamilySDLT, MediaID: "S2", VolSerLength: 6},
		{Family: FamilyDDS, MediaID: "D1", VolSerLength: 6},
		{Family: FamilyAIT, MediaID: "A1", VolSerLength: 6},
		{Family: Family3592, MediaID: "J1", VolSerLength: 6},
		{Family: FamilyGeneric, MediaID: "", VolSerLength: 8},
	}
	for _, spec := range cases {
		if err := ValidateSpec(spec); err != nil {
			t.Errorf("ValidateSpec(%+v): unexpected error: %v", spec, err)
		}
	}
}

func TestSpecForRejectsUnknownFamily(t *testing.T) {
	if _, err := SpecFor("bogus", "XX", 6); err == nil {
		t.Fatalf("SpecFor(bogus): expected error, got nil")
	}
}

func TestValidateRejectsWrongLength(t *testing.T) {
	spec := Spec{Family: FamilyLTO, MediaID: "L8", VolSerLength: 6}
	err := Validate(spec, "00001L8")
	if err == nil {
		t.Fatalf("Validate: expected error for wrong length, got nil")
	}
	if !strings.Contains(err.Error(), "8 characters") {
		t.Errorf("error should mention expected length: %v", err)
	}
}

func TestValidateRejectsBadCharset(t *testing.T) {
	spec := Spec{Family: FamilyLTO, MediaID: "L8", VolSerLength: 6}
	err := Validate(spec, "abcdefL8")
	if err == nil {
		t.Fatalf("Validate: expected error for lowercase charset, got nil")
	}
	if !strings.Contains(err.Error(), "uppercase") {
		t.Errorf("error should mention charset: %v", err)
	}
}

func TestValidateRejectsWrongMediaID(t *testing.T) {
	spec := Spec{Family: FamilyLTO, MediaID: "L8", VolSerLength: 6}
	err := Validate(spec, "000001L9")
	if err == nil {
		t.Fatalf("Validate: expected error for wrong media id, got nil")
	}
	if !strings.Contains(err.Error(), "media id") {
		t.Errorf("error should mention media id: %v", err)
	}
}

func TestValidateAcceptsWellFormed(t *testing.T) {
	spec := Spec{Family: FamilyLTO, MediaID: "L8", VolSerLength: 6}
	if err := Validate(spec, "MYOWN1L8"); err != nil {
		t.Fatalf("Validate: unexpected error: %v", err)
	}
}

func TestNextAvailableSkipsTaken(t *testing.T) {
	spec := Spec{Family: FamilyLTO, MediaID: "L8", VolSerLength: 6}
	taken := map[string]bool{"000001L8": true, "000002L8": true}
	out, err := NextAvailable(spec, func(bc string) bool { return taken[bc] }, 2)
	if err != nil {
		t.Fatalf("NextAvailable: %v", err)
	}
	want := []string{"000003L8", "000004L8"}
	if len(out) != len(want) {
		t.Fatalf("NextAvailable = %v, want %v", out, want)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Errorf("NextAvailable[%d] = %q, want %q", i, out[i], want[i])
		}
	}
}

func TestNextAvailableReturnsExactCount(t *testing.T) {
	spec := Spec{Family: FamilyGeneric, MediaID: "", VolSerLength: 6}
	out, err := NextAvailable(spec, func(string) bool { return false }, 5)
	if err != nil {
		t.Fatalf("NextAvailable: %v", err)
	}
	if len(out) != 5 {
		t.Fatalf("NextAvailable returned %d barcodes, want 5", len(out))
	}
}

func TestKnownFamiliesIncludesAllUsedFamilies(t *testing.T) {
	families := KnownFamilies()
	want := []Family{FamilyLTO, FamilyDLT, FamilySDLT, FamilyDDS, FamilyAIT, Family3592, FamilyGeneric}
	if len(families) != len(want) {
		t.Fatalf("KnownFamilies() = %v, want %v", families, want)
	}
}
