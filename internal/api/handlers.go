package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/library"
)

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	logicalLib := r.Header.Get("X-Logical-Library")
	if logicalLib != "" {
		writeJSON(w, http.StatusOK, s.lib.LogicalLibraryStatus(logicalLib))
	} else {
		writeJSON(w, http.StatusOK, s.lib.Status())
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.lib.Events())
}

func (s *Server) handleListVolumes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.lib.AllVolumes())
}

func (s *Server) handleListOutsideVolumes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.lib.OutsideVolumes())
}

func (s *Server) handleListLogicalLibraries(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.lib.ListLogicalLibraries())
}

func (s *Server) handleGetLogicalLibrary(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	lib := s.lib.GetLogicalLibrary(name)
	if lib == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("logical library %s not found", name))
		return
	}
	writeJSON(w, http.StatusOK, lib)
}

// handleCreateLogicalLibrary and handleUpdateLogicalLibrary accept the same
// config-style shape the setup wizard already sends (drive indices,
// magazine IDs, io-slot indices), not raw element structs - Library
// resolves those against the live drives/slots/ioslots itself, which is
// also where exclusivity (an element can't belong to two logical
// libraries) is enforced. On success the assignment is also persisted to
// the topology store so it survives a restart.
func (s *Server) handleCreateLogicalLibrary(w http.ResponseWriter, r *http.Request) {
	var req config.LogicalLibraryConfig
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	lib, err := s.lib.AddLogicalLibrary(req)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	if s.topology != nil {
		if err := s.topology.CreateLogicalLibrary(req); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	s.ReconcileKernelModeInstancesAsync()
	writeJSON(w, http.StatusCreated, lib)
}

func (s *Server) handleUpdateLogicalLibrary(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req config.LogicalLibraryConfig
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	lib, err := s.lib.UpdateLogicalLibrary(name, req)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	if s.topology != nil {
		if err := s.topology.UpdateLogicalLibrary(name, req); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	// Also covers a rename (req.Name != name): reconcile always diffs
	// against the *current* full set of logical libraries, so the old
	// name's instance gets disabled and the new name's gets enabled,
	// with no special-casing needed here.
	s.ReconcileKernelModeInstancesAsync()
	writeJSON(w, http.StatusOK, lib)
}

func (s *Server) handleDeleteLogicalLibrary(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.lib.DeleteLogicalLibrary(name); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	if s.topology != nil {
		if err := s.topology.DeleteLogicalLibrary(name); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	s.ReconcileKernelModeInstancesAsync()
	w.WriteHeader(http.StatusNoContent)
}

// handleUnassignedElements lists drives/slots/io-slots not currently
// assigned to any logical library, so the Admin UI can offer them for
// assignment.
func (s *Server) handleUnassignedElements(w http.ResponseWriter, r *http.Request) {
	drives, slots, ioslots := s.lib.UnassignedElements()
	writeJSON(w, http.StatusOK, map[string]any{
		"drives":  drives,
		"slots":   slots,
		"ioslots": ioslots,
	})
}

func (s *Server) handleDeleteOutsideVolume(w http.ResponseWriter, r *http.Request) {
	bc := r.PathValue("barcode")
	if err := s.lib.DeleteOutsideVolume(bc); err != nil {
		s.emitFailure(r, library.EventCodeMediaOutsideDeleteFailure, "failed to delete outside volume", err, map[string]string{"volume": bc})
		writeError(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListOffsiteVolumes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.lib.OffsiteVolumes())
}

type offsiteSendRequest struct {
	FromKind string `json:"from_kind"`
	FromAddr int    `json:"from_address"`
}

func (s *Server) handleOffsiteSend(w http.ResponseWriter, r *http.Request) {
	var req offsiteSendRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ref, err := elementRef(req.FromKind, strconv.Itoa(req.FromAddr))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	vol, err := s.lib.OffsiteSend(ref)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, vol)
}

type offsiteRecallRequest struct {
	Barcode string `json:"barcode"`
	ToKind  string `json:"to_kind"`
	ToAddr  int    `json:"to_address"`
}

func (s *Server) handleOffsiteRecall(w http.ResponseWriter, r *http.Request) {
	var req offsiteRecallRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ref, err := elementRef(req.ToKind, strconv.Itoa(req.ToAddr))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.OffsiteRecall(req.Barcode, ref); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, s.lib.Status())
}

// openDoorRequest carries an optional PIN, checked against a magazine's/
// mailbox's own configured PIN if one exists (see
// Library.checkMagazinePINLocked/checkMailboxPINLocked) - an empty PIN is
// fine and always succeeds when no PIN is configured at all.
type openDoorRequest struct {
	PIN string `json:"pin,omitempty"`
}

func (s *Server) handleOpenIODoor(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req openDoorRequest
	if err := decodeJSON(r, &req); err != nil {
		s.emitFailure(r, library.EventCodeRoboticsDoorIOOpenFailure, "failed to open IO door", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.OpenIODoor(id, req.PIN); err != nil {
		s.emitFailure(r, library.EventCodeRoboticsDoorIOOpenFailure, "failed to open IO door", err, nil)
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, s.lib.Status())
}

type closeDoorRequest struct {
	Actions []library.DoorAction `json:"actions"`
}

func (s *Server) handleCloseIODoor(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req closeDoorRequest
	if err := decodeJSON(r, &req); err != nil {
		s.emitFailure(r, library.EventCodeRoboticsDoorIOCloseFailure, "failed to close IO door", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.CloseIODoor(id, req.Actions); err != nil {
		s.emitFailure(r, library.EventCodeRoboticsDoorIOCloseFailure, "failed to close IO door", err, nil)
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, s.lib.Status())
}

func (s *Server) handleOpenStorageDoor(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req openDoorRequest
	if err := decodeJSON(r, &req); err != nil {
		s.emitFailure(r, library.EventCodeRoboticsDoorStorageOpenFailure, "failed to open storage door", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.OpenStorageDoor(id, req.PIN); err != nil {
		s.emitFailure(r, library.EventCodeRoboticsDoorStorageOpenFailure, "failed to open storage door", err, nil)
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, s.lib.Status())
}

func (s *Server) handleCloseStorageDoor(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req closeDoorRequest
	if err := decodeJSON(r, &req); err != nil {
		s.emitFailure(r, library.EventCodeRoboticsDoorStorageCloseFailure, "failed to close storage door", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.CloseStorageDoor(id, req.Actions); err != nil {
		s.emitFailure(r, library.EventCodeRoboticsDoorStorageCloseFailure, "failed to close storage door", err, nil)
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, s.lib.Status())
}

type loadRequest struct {
	FromKind string `json:"from_kind"`
	FromAddr int    `json:"from_address"`
	Drive    int    `json:"drive"`
}

func (s *Server) handleLoad(w http.ResponseWriter, r *http.Request) {
	var req loadRequest
	if err := decodeJSON(r, &req); err != nil {
		s.emitFailure(r, library.EventCodeRoboticsLoadFailure, "failed to load volume", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ref, err := elementRef(req.FromKind, strconv.Itoa(req.FromAddr))
	if err != nil {
		s.emitFailure(r, library.EventCodeRoboticsLoadFailure, "failed to load volume", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.Load(ref, req.Drive, r.Header.Get("X-Logical-Library")); err != nil {
		s.emitFailure(r, library.EventCodeRoboticsLoadFailure, "failed to load volume", err, map[string]string{"drive": strconv.Itoa(req.Drive)})
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, s.lib.Status())
}

type unloadRequest struct {
	Drive  int    `json:"drive"`
	ToKind string `json:"to_kind"`
	ToAddr int    `json:"to_address"`
}

func (s *Server) handleUnload(w http.ResponseWriter, r *http.Request) {
	var req unloadRequest
	if err := decodeJSON(r, &req); err != nil {
		s.emitFailure(r, library.EventCodeRoboticsUnloadFailure, "failed to unload volume", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ref, err := elementRef(req.ToKind, strconv.Itoa(req.ToAddr))
	if err != nil {
		s.emitFailure(r, library.EventCodeRoboticsUnloadFailure, "failed to unload volume", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.Unload(req.Drive, ref, r.Header.Get("X-Logical-Library")); err != nil {
		s.emitFailure(r, library.EventCodeRoboticsUnloadFailure, "failed to unload volume", err, map[string]string{"drive": strconv.Itoa(req.Drive)})
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, s.lib.Status())
}

type moveRequest struct {
	FromKind string `json:"from_kind"`
	FromAddr int    `json:"from_address"`
	ToKind   string `json:"to_kind"`
	ToAddr   int    `json:"to_address"`
}

func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	var req moveRequest
	if err := decodeJSON(r, &req); err != nil {
		s.emitFailure(r, library.EventCodeRoboticsMoveFailure, "failed to move volume", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	from, err := elementRef(req.FromKind, strconv.Itoa(req.FromAddr))
	if err != nil {
		s.emitFailure(r, library.EventCodeRoboticsMoveFailure, "failed to move volume", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	to, err := elementRef(req.ToKind, strconv.Itoa(req.ToAddr))
	if err != nil {
		s.emitFailure(r, library.EventCodeRoboticsMoveFailure, "failed to move volume", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.Move(from, to, r.Header.Get("X-Logical-Library")); err != nil {
		s.emitFailure(r, library.EventCodeRoboticsMoveFailure, "failed to move volume", err, nil)
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, s.lib.Status())
}

type driveFaultRequest struct {
	Fault bool `json:"fault"`
}

func (s *Server) handleDriveFault(w http.ResponseWriter, r *http.Request) {
	idx, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		s.emitFailure(r, library.EventCodeDriveFaultSetFailure, "failed to update drive fault state", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req driveFaultRequest
	if err := decodeJSON(r, &req); err != nil {
		s.emitFailure(r, library.EventCodeDriveFaultSetFailure, "failed to update drive fault state", err, map[string]string{"drive": strconv.Itoa(idx)})
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.SetDriveFault(idx, req.Fault); err != nil {
		s.emitFailure(r, library.EventCodeDriveFaultSetFailure, "failed to update drive fault state", err, map[string]string{"drive": strconv.Itoa(idx)})
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, s.lib.Status())
}

type volumeWriteProtectRequest struct {
	WriteProtected bool `json:"write_protected"`
}

func (s *Server) handleSetVolumeWriteProtect(w http.ResponseWriter, r *http.Request) {
	bc := r.PathValue("barcode")
	var req volumeWriteProtectRequest
	if err := decodeJSON(r, &req); err != nil {
		s.emitFailure(r, library.EventCodeMediaVolumeWriteProtectSetFailure, "failed to update volume write-protect state", err, map[string]string{"volume": bc})
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.SetVolumeWriteProtect(bc, req.WriteProtected); err != nil {
		s.emitFailure(r, library.EventCodeMediaVolumeWriteProtectSetFailure, "failed to update volume write-protect state", err, map[string]string{"volume": bc})
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, s.lib.Status())
}

type roboticFaultRequest struct {
	Active  bool   `json:"active"`
	Kind    string `json:"kind,omitempty"`
	Message string `json:"message,omitempty"`
}

func (s *Server) handleRoboticFault(w http.ResponseWriter, r *http.Request) {
	var req roboticFaultRequest
	if err := decodeJSON(r, &req); err != nil {
		s.emitFailure(r, library.EventCodeRoboticsFaultSetFailure, "failed to update robotic fault state", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.SetRoboticFault(req.Active, req.Kind, req.Message); err != nil {
		s.emitFailure(r, library.EventCodeRoboticsFaultSetFailure, "failed to update robotic fault state", err, map[string]string{"kind": req.Kind})
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, s.lib.Status())
}

type createTokenRequest struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

type createTokenResponse struct {
	Name  string `json:"name"`
	Role  Role   `json:"role"`
	Token string `json:"token"`
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.tokens.List())
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var req createTokenRequest
	if err := decodeJSON(r, &req); err != nil || req.Name == "" {
		s.emitFailure(r, library.EventCodeConfigTokenCreateFailure, "failed to create API token", err, nil)
		writeError(w, http.StatusBadRequest, fmt.Errorf("token name is required"))
		return
	}
	role, err := ParseRole(req.Role)
	if err != nil {
		s.emitFailure(r, library.EventCodeConfigTokenCreateFailure, "failed to create API token", err, map[string]string{"name": req.Name})
		writeError(w, http.StatusBadRequest, err)
		return
	}
	raw, err := s.tokens.Add(req.Name, role)
	if err != nil {
		s.emitFailure(r, library.EventCodeConfigTokenCreateFailure, "failed to create API token", err, map[string]string{"name": req.Name, "role": string(role)})
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.emitEvent(r, library.Event{Code: library.EventCodeConfigTokenCreateSuccess, Message: "created API token", Detail: map[string]string{"name": req.Name, "role": string(role)}})
	writeJSON(w, http.StatusCreated, createTokenResponse{Name: req.Name, Role: role, Token: raw})
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.tokens.Revoke(name); err != nil {
		s.emitFailure(r, library.EventCodeConfigTokenRevokeFailure, "failed to revoke API token", err, map[string]string{"name": name})
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.emitEvent(r, library.Event{Code: library.EventCodeConfigTokenRevokeSuccess, Message: "revoked API token", Detail: map[string]string{"name": name}})
	w.WriteHeader(http.StatusNoContent)
}
