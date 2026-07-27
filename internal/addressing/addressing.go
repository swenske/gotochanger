// Package addressing renumbers a Status's physical slot/ioslot/drive
// addresses into the dense, near-zero range every Bareos-facing or
// SCSI-facing protocol this project speaks assumes an addressable
// changer's elements occupy.
//
// Originally private to cmd/gotochanger-changer (as presentedAddressing/
// buildPresentedAddressing), extracted here so cmd/gotochanger-tcmud's
// SMC-3 READ ELEMENT STATUS/MOVE MEDIUM handling (see internal/scsi) can
// reuse the exact same renumbering instead of duplicating it - both
// binaries face the same underlying problem (a real protocol on the
// outside assumes dense-from-a-small-number addressing; gotochangerd's own
// physical addresses, especially once scoped to one logical library, often
// aren't) even though the outside protocols themselves (Bareos's
// changer-script convention vs. real SCSI CDBs) are completely different.
package addressing

import (
	"fmt"
	"sort"

	"github.com/swenske/gotochanger/internal/library"
)

// Addressing renumbers the slots/ioslots in a Status into the dense,
// 1-based range a changer's addressable elements are conventionally
// expected to occupy (storage slots first, then I/O slots right after -
// the same "contiguous" convention already used for the whole physical
// library, applied here to whatever set of elements is actually in scope
// for this invocation).
//
// When unscoped, that set is the entire physical library, whose addresses
// are already dense from 1 by construction, so this is a no-op identity
// mapping. When scoped to one logical library, the library's elements can
// start at an arbitrary physical address (e.g. a second logical library
// carved out of magazines/mailboxes added after the first) and can have
// gaps - without renumbering, physical addresses like 41-45 or I/O slots
// physically sitting at 101-105 leak straight through to a consumer (Bareos,
// or a real SCSI initiator's READ ELEMENT STATUS) that assumes a changer's
// element count and its highest element address agree.
//
// Drives get the same treatment, separately and 0-based (matching both
// Bareos's "drive index" convention and a SCSI changer's data-transfer-
// element numbering): the presented index is always the 0-based position
// within *this invocation's own* set of drives, sorted by physical index,
// not gotochangerd's global physical Drive.Index.
type Addressing struct {
	toPresented map[int]int
	toPhysical  map[int]int

	driveToPresented map[int]int
	driveToPhysical  map[int]int
}

// Build computes an Addressing from a Status - typically one already
// scoped to a single logical library (library.Library.LogicalLibraryStatus),
// or the whole physical library's own Status for an unscoped view.
func Build(st library.Status) Addressing {
	a := Addressing{
		toPresented:      map[int]int{},
		toPhysical:       map[int]int{},
		driveToPresented: map[int]int{},
		driveToPhysical:  map[int]int{},
	}

	slots := append([]*library.Slot(nil), st.Slots...)
	sort.Slice(slots, func(i, j int) bool { return slots[i].Address < slots[j].Address })
	ioslots := append([]*library.IOSlot(nil), st.IOSlots...)
	sort.Slice(ioslots, func(i, j int) bool { return ioslots[i].Address < ioslots[j].Address })
	drives := append([]*library.Drive(nil), st.Drives...)
	sort.Slice(drives, func(i, j int) bool { return drives[i].Index < drives[j].Index })

	n := 1
	for _, s := range slots {
		a.toPresented[s.Address] = n
		a.toPhysical[n] = s.Address
		n++
	}
	for _, io := range ioslots {
		a.toPresented[io.Address] = n
		a.toPhysical[n] = io.Address
		n++
	}
	for i, d := range drives {
		a.driveToPresented[d.Index] = i
		a.driveToPhysical[i] = d.Index
	}
	return a
}

// Present translates a physical slot/ioslot address to the one shown to
// the outside protocol. Falls back to the physical address itself for
// anything not in scope (e.g. a drive's Origin pointing outside the
// current logical library shouldn't happen, but this avoids ever printing
// a zero/garbage address).
func (a Addressing) Present(physical int) int {
	if p, ok := a.toPresented[physical]; ok {
		return p
	}
	return physical
}

// Physical translates an address the outside protocol gave us back to the
// real physical address the API expects.
func (a Addressing) Physical(presented int) (int, error) {
	if p, ok := a.toPhysical[presented]; ok {
		return p, nil
	}
	return 0, fmt.Errorf("invalid slot address %d", presented)
}

// PresentDrive/PhysicalDrive are the drive-index equivalents of
// Present/Physical above.
func (a Addressing) PresentDrive(physical int) int {
	if p, ok := a.driveToPresented[physical]; ok {
		return p
	}
	return physical
}

func (a Addressing) PhysicalDrive(presented int) (int, error) {
	if p, ok := a.driveToPhysical[presented]; ok {
		return p, nil
	}
	return 0, fmt.Errorf("invalid drive index %d", presented)
}
