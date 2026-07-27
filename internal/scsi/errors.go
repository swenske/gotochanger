package scsi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/swenske/gotochanger/internal/apiclient"
)

// senseForLibraryError classifies an error returned by a LibraryClient
// Move/Load/Unload call into a sense key/ASC/ASCQ triple, so a kernel-mode
// initiator sees a cause-specific CHECK CONDITION instead of one generic
// mechanical error for every possible failure. Only apiclient.APIError
// (what a real apiclient.Client actually returns - see apiclient.Client.do)
// carries enough information to distinguish causes; anything else (a
// transport-level failure, or a fake LibraryClient in a test that returns
// a plain error) falls back to the same generic sense this project has
// always used for an unrecognized Move/Load/Unload failure.
//
// The two causes distinguished here - a drive/robotic fault, and a
// cleaning-cartridge operation that couldn't proceed - are the only
// Library errors realistically reachable from Changer.moveMedium's
// Load/Unload/Move calls in practice (see library.Library's own error
// sentinels): every other one (ErrFull/ErrEmpty/ErrOutsideLogicalLibrary)
// is either already checked before the call happens (full/empty) or
// structurally unreachable from an already-scoped kernel-mode request (see
// cmd/gotochanger-tcmud's --logical-library flag).
func senseForLibraryError(err error) (key, asc, ascq uint8) {
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
		switch {
		case strings.Contains(apiErr.Message, "fault"):
			return SenseNotReady, AscLogicalUnitNotReady, AscqManualInterventionRequired
		case strings.Contains(apiErr.Message, "cleaning"):
			return SenseNotReady, AscCleaningFailure, AscqCleaningFailure
		}
	}
	return SenseAbortedCommand, AscMechanicalPositioningOrChangerError, AscqMechanicalPositioningOrChangerError
}
