// Package barcode generates and validates tape cartridge barcodes per
// tape-family convention (LTO, DLT, SDLT, DDS/DAT, AIT/SAIT, IBM 3592, and a
// generic fallback for non-physical/custom types). It has no dependency on
// any other internal package, so it can be imported by both
// internal/config (for validating a tape type's format) and
// internal/library (for generating/checking actual cartridge barcodes)
// without an import cycle.
package barcode

import "fmt"

// Family identifies which barcode convention a tape type follows. lto, dlt,
// and sdlt are real, published vendor formats (see IBM's LTO Ultrium
// Cartridge Label Specification and HPE/vendor DLT/SDLT tape bar code
// documentation). dds, ait, and 3592 have no published external barcode
// standard — gotochanger defines its own convention for them, mirroring the
// lto/sdlt 6-character-volser + 2-character-media-id shape for consistency
// rather than presenting it as a real vendor spec.
type Family string

const (
	FamilyLTO     Family = "lto"
	FamilyDLT     Family = "dlt"
	FamilySDLT    Family = "sdlt"
	FamilyDDS     Family = "dds"
	FamilyAIT     Family = "ait"
	Family3592    Family = "3592"
	FamilyGeneric Family = "generic"
)

// KnownFamilies lists every supported Family, in a stable order suitable
// for validation error messages and populating a UI dropdown.
func KnownFamilies() []Family {
	return []Family{FamilyLTO, FamilyDLT, FamilySDLT, FamilyDDS, FamilyAIT, Family3592, FamilyGeneric}
}

// Spec fully describes one tape type's barcode format: a sequential
// "volume identifier" of VolSerLength characters, followed by a fixed
// MediaID suffix (empty for families/types with no fixed suffix).
type Spec struct {
	Family       Family
	MediaID      string
	VolSerLength int
}

// shape describes the structural constraints a Family imposes on its Spec.
type shape struct {
	volSerLength             func(int) bool
	volSerDesc               string
	mediaLenMin, mediaLenMax int
}

func shapeFor(f Family) (shape, error) {
	fixed6 := func(n int) bool { return n == 6 }
	switch f {
	case FamilyLTO:
		return shape{fixed6, "exactly 6", 2, 2}, nil
	case FamilyDLT:
		return shape{fixed6, "exactly 6", 0, 1}, nil
	case FamilySDLT:
		return shape{fixed6, "exactly 6", 1, 2}, nil
	case FamilyDDS, FamilyAIT, Family3592:
		return shape{fixed6, "exactly 6", 2, 2}, nil
	case FamilyGeneric:
		return shape{func(n int) bool { return n >= 1 && n <= 32 }, "between 1 and 32", 0, 0}, nil
	default:
		return shape{}, fmt.Errorf("unknown barcode family %q (known families: %v)", f, KnownFamilies())
	}
}

// SpecFor builds a Spec from a tape type's raw catalog fields, validating
// only that family is recognized (shape/format consistency is checked
// separately by ValidateSpec, since building a Spec and validating it are
// useful as separate steps to callers).
func SpecFor(family, mediaID string, volSerLength int) (Spec, error) {
	f := Family(family)
	if _, err := shapeFor(f); err != nil {
		return Spec{}, err
	}
	return Spec{Family: f, MediaID: mediaID, VolSerLength: volSerLength}, nil
}

// ValidateSpec checks that a tape type's own barcode-format definition is
// internally consistent for its family, e.g. lto requires exactly a
// 2-character media id and a 6-character volume identifier.
func ValidateSpec(spec Spec) error {
	sh, err := shapeFor(spec.Family)
	if err != nil {
		return err
	}
	if !sh.volSerLength(spec.VolSerLength) {
		return fmt.Errorf("family %s requires a volume-identifier length %s, got %d", spec.Family, sh.volSerDesc, spec.VolSerLength)
	}
	if len(spec.MediaID) < sh.mediaLenMin || len(spec.MediaID) > sh.mediaLenMax {
		if sh.mediaLenMin == sh.mediaLenMax {
			return fmt.Errorf("family %s requires a %d-character media id, got %q (%d characters)", spec.Family, sh.mediaLenMin, spec.MediaID, len(spec.MediaID))
		}
		return fmt.Errorf("family %s requires a media id between %d and %d characters, got %q (%d characters)", spec.Family, sh.mediaLenMin, sh.mediaLenMax, spec.MediaID, len(spec.MediaID))
	}
	if !isUpperAlnum(spec.MediaID) {
		return fmt.Errorf("media id %q must contain only uppercase letters A-Z and digits 0-9", spec.MediaID)
	}
	return nil
}

func isUpperAlnum(s string) bool {
	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// Validate checks that a concrete barcode string conforms to spec: correct
// total length, uppercase-alphanumeric charset only (matching the real
// USS-39/Code 39 restriction the lto/dlt/sdlt specs impose), and the
// correct fixed MediaID suffix when spec.MediaID is set. The returned error
// names exactly which check failed.
func Validate(spec Spec, bc string) error {
	total := spec.VolSerLength + len(spec.MediaID)
	if len(bc) != total {
		return fmt.Errorf("barcode %q: must be exactly %d characters for family %s (%d-character volume identifier + %d-character media id %q), got %d",
			bc, total, spec.Family, spec.VolSerLength, len(spec.MediaID), spec.MediaID, len(bc))
	}
	if !isUpperAlnum(bc) {
		return fmt.Errorf("barcode %q: must contain only uppercase letters A-Z and digits 0-9", bc)
	}
	if spec.MediaID != "" {
		if suffix := bc[spec.VolSerLength:]; suffix != spec.MediaID {
			return fmt.Errorf("barcode %q: must end with media id %q for this tape type, got %q", bc, spec.MediaID, suffix)
		}
	}
	return nil
}

// Generate returns the barcode for the given 1-based sequence number under
// spec, e.g. Generate(Spec{FamilyLTO, "L8", 6}, 1) -> "000001L8". The
// sequential portion is decimal, zero-padded to VolSerLength.
func Generate(spec Spec, seq int) (string, error) {
	if seq < 1 {
		return "", fmt.Errorf("sequence number must be at least 1, got %d", seq)
	}
	volser := fmt.Sprintf("%0*d", spec.VolSerLength, seq)
	if len(volser) > spec.VolSerLength {
		return "", fmt.Errorf("sequence number %d does not fit in a %d-character volume identifier", seq, spec.VolSerLength)
	}
	return volser + spec.MediaID, nil
}

// NextAvailable generates up to count barcodes under spec that taken
// reports as not already in use, skipping forward past any sequence number
// already taken. Safe to call repeatedly against a growing taken set (e.g.
// to top up a tape set) since sequence numbers are derived fresh each call
// rather than from a persisted counter.
func NextAvailable(spec Spec, taken func(string) bool, count int) ([]string, error) {
	if count < 1 {
		return nil, fmt.Errorf("count must be at least 1, got %d", count)
	}
	out := make([]string, 0, count)
	for seq := 1; len(out) < count; seq++ {
		bc, err := Generate(spec, seq)
		if err != nil {
			return out, err
		}
		if taken(bc) {
			continue
		}
		out = append(out, bc)
	}
	return out, nil
}
