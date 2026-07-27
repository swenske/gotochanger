package scsi

import (
	"fmt"

	"github.com/swenske/gotochanger/internal/library"
)

// fakeClient is a minimal in-memory LibraryClient for tests - Move/Load/
// Unload mutate the Status it holds directly, mirroring enough of
// gotochangerd's real element-occupancy semantics for these handlers' own
// dispatch logic to be exercised without a real daemon.
type fakeClient struct {
	st        library.Status
	statusErr error
	failMove  bool
	moveErr   error // takes precedence over failMove when set - lets a test control exactly what error Load/Unload/Move return
}

func (f *fakeClient) Status() (library.Status, error) { return f.st, f.statusErr }

func (f *fakeClient) findSlot(addr int) *library.Slot {
	for _, s := range f.st.Slots {
		if s.Address == addr {
			return s
		}
	}
	return nil
}

func (f *fakeClient) findIOSlot(addr int) *library.IOSlot {
	for _, s := range f.st.IOSlots {
		if s.Address == addr {
			return s
		}
	}
	return nil
}

func (f *fakeClient) findDrive(idx int) *library.Drive {
	for _, d := range f.st.Drives {
		if d.Index == idx {
			return d
		}
	}
	return nil
}

// slot returns a pointer to the Volume field of the named element, so
// callers can both read and clear/set it in place.
func (f *fakeClient) slot(kind string, addr int) **library.Volume {
	switch kind {
	case "slot":
		if s := f.findSlot(addr); s != nil {
			return &s.Volume
		}
	case "ioslot":
		if s := f.findIOSlot(addr); s != nil {
			return &s.Volume
		}
	}
	return nil
}

func (f *fakeClient) Load(fromKind string, fromAddr, drive int) error {
	if f.moveErr != nil {
		return f.moveErr
	}
	if f.failMove {
		return fmt.Errorf("simulated failure")
	}
	src := f.slot(fromKind, fromAddr)
	d := f.findDrive(drive)
	if src == nil || *src == nil || d == nil || d.Volume != nil {
		return fmt.Errorf("invalid load")
	}
	d.Volume = *src
	*src = nil
	return nil
}

func (f *fakeClient) Unload(drive int, toKind string, toAddr int) error {
	if f.moveErr != nil {
		return f.moveErr
	}
	if f.failMove {
		return fmt.Errorf("simulated failure")
	}
	d := f.findDrive(drive)
	dst := f.slot(toKind, toAddr)
	if d == nil || d.Volume == nil || dst == nil || *dst != nil {
		return fmt.Errorf("invalid unload")
	}
	*dst = d.Volume
	d.Volume = nil
	return nil
}

func (f *fakeClient) Move(fromKind string, fromAddr int, toKind string, toAddr int) error {
	if f.moveErr != nil {
		return f.moveErr
	}
	if f.failMove {
		return fmt.Errorf("simulated failure")
	}
	src := f.slot(fromKind, fromAddr)
	dst := f.slot(toKind, toAddr)
	if src == nil || *src == nil || dst == nil || *dst != nil {
		return fmt.Errorf("invalid move")
	}
	*dst = *src
	*src = nil
	return nil
}
