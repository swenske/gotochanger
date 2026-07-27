package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/library"
	"github.com/swenske/gotochanger/internal/secrethash"
)

// reconfigureFromStore reloads the full topology from the database and
// applies it to the running Library without a restart. Used after any
// Admin/wizard change that affects physical elements (magazines, drive
// devices, I/O slot count), and also after tape-type/tape-set catalog
// changes: Library resolves a tape set's barcode format and storage folder
// from its own live config.Library.TapeTypes/TapeSets (see
// resolveTapeSetLocked in internal/library), so it needs to see catalog
// writes too, not just physical-element ones.
func (s *Server) reconfigureFromStore() error {
	if s.topology == nil {
		return nil
	}
	lc, err := s.topology.LoadTopology()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.cfg.Library = lc
	newCfg := s.cfg
	s.mu.Unlock()
	return s.lib.Reconfigure(newCfg)
}

// ---- drive types ----

func (s *Server) handleListDriveTypes(w http.ResponseWriter, r *http.Request) {
	dts, err := s.topology.ListDriveTypes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, dts)
}

func (s *Server) handleCreateDriveType(w http.ResponseWriter, r *http.Request) {
	var dt config.DriveType
	if err := decodeJSON(r, &dt); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.topology.CreateDriveType(dt); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusCreated, dt)
}

func (s *Server) handleUpdateDriveType(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var dt config.DriveType
	if err := decodeJSON(r, &dt); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.topology.UpdateDriveType(name, dt); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, dt)
}

func (s *Server) handleDeleteDriveType(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.topology.DeleteDriveType(name); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- tape types ----

func (s *Server) handleListTapeTypes(w http.ResponseWriter, r *http.Request) {
	tts, err := s.topology.ListTapeTypes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, tts)
}

func (s *Server) handleCreateTapeType(w http.ResponseWriter, r *http.Request) {
	var tt config.TapeType
	if err := decodeJSON(r, &tt); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := config.ValidateTapeType(tt); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.topology.CreateTapeType(tt); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	if err := s.reconfigureFromStore(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, tt)
}

func (s *Server) handleUpdateTapeType(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var tt config.TapeType
	if err := decodeJSON(r, &tt); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	tt.Name = name
	if err := config.ValidateTapeType(tt); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.topology.UpdateTapeType(name, tt); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err := s.reconfigureFromStore(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, tt)
}

func (s *Server) handleDeleteTapeType(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.topology.DeleteTapeType(name); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err := s.reconfigureFromStore(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- tape sets ----

func (s *Server) handleListTapeSets(w http.ResponseWriter, r *http.Request) {
	sets, err := s.topology.ListTapeSets()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, sets)
}

// validateTapeSetFolder requires an absolute path and creates it so a tape
// set is immediately usable as a volume-file destination.
func validateTapeSetFolder(path string) error {
	if path == "" || path[0] != '/' {
		return errInvalidFolder
	}
	return os.MkdirAll(path, 0o770)
}

var errInvalidFolder = writeErrorSentinel("storage_folder must be an absolute path")

type writeErrorSentinel string

func (e writeErrorSentinel) Error() string { return string(e) }

// knownTapeTypeNames returns the set of tape type names currently in the
// catalog, for config.ValidateTapeSet's existence check.
func (s *Server) knownTapeTypeNames() (map[string]bool, error) {
	tts, err := s.topology.ListTapeTypes()
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(tts))
	for _, tt := range tts {
		known[tt.Name] = true
	}
	return known, nil
}

// createTapeSetRequest is the request body for POST /api/v1/tape-sets: the
// persisted config.TapeSetConfig fields plus a one-time InitialTapeCount
// directive (how many cartridges to auto-generate immediately). Tape count
// isn't part of config.TapeSetConfig itself since it's a creation action,
// not a persistent property of the tape set (see gotochanger-changer's
// wizard step 6, which uses the same convention for its own tape count).
type createTapeSetRequest struct {
	Name             string `json:"name"`
	TapeType         string `json:"tape_type"`
	StorageFolder    string `json:"storage_folder"`
	InitialTapeCount int    `json:"initial_tape_count"`
}

type createTapeSetResponse struct {
	config.TapeSetConfig
	Cartridges []*library.Volume `json:"cartridges"`
}

func (s *Server) handleCreateTapeSet(w http.ResponseWriter, r *http.Request) {
	var req createTapeSetRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.InitialTapeCount < 1 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("initial_tape_count must be at least 1"))
		return
	}
	ts := config.TapeSetConfig{Name: req.Name, TapeType: req.TapeType, StorageFolder: req.StorageFolder}
	known, err := s.knownTapeTypeNames()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := config.ValidateTapeSet(ts, known); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := validateTapeSetFolder(ts.StorageFolder); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.topology.CreateTapeSet(ts); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	if err := s.reconfigureFromStore(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	cartridges, err := s.lib.CreateTapeSetCartridges(ts.Name, req.InitialTapeCount)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, createTapeSetResponse{TapeSetConfig: ts, Cartridges: cartridges})
}

func (s *Server) handleUpdateTapeSet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var ts config.TapeSetConfig
	if err := decodeJSON(r, &ts); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ts.Name = name
	known, err := s.knownTapeTypeNames()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := config.ValidateTapeSet(ts, known); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := validateTapeSetFolder(ts.StorageFolder); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.topology.UpdateTapeSet(name, ts); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err := s.reconfigureFromStore(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, ts)
}

var errTapeSetNotEmpty = writeErrorSentinel("tape set still has cartridges; delete them first")

func (s *Server) handleDeleteTapeSet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	for _, v := range s.lib.AllVolumes() {
		if v.TapeSet == name {
			writeError(w, http.StatusConflict, errTapeSetNotEmpty)
			return
		}
	}
	if err := s.topology.DeleteTapeSet(name); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err := s.reconfigureFromStore(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// addTapeSetTapesRequest is the request body for
// POST /api/v1/tape-sets/{name}/tapes: either Count (bulk, auto-generated
// barcodes) or Barcode (a single, manually-chosen barcode), mutually
// exclusive - Barcode takes precedence if both are set.
type addTapeSetTapesRequest struct {
	Count   int    `json:"count,omitempty"`
	Barcode string `json:"barcode,omitempty"`
}

func (s *Server) handleAddTapeSetTapes(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req addTapeSetTapesRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var vols []*library.Volume
	var err error
	if req.Barcode != "" {
		var v *library.Volume
		v, err = s.lib.CreateManualCartridge(name, req.Barcode)
		if v != nil {
			vols = []*library.Volume{v}
		}
	} else {
		if req.Count < 1 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("count must be at least 1 (or set barcode for a single manually-numbered cartridge)"))
			return
		}
		vols, err = s.lib.CreateTapeSetCartridges(name, req.Count)
	}
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, vols)
}

// handleFSBrowse lists a directory's immediate children, backing the Admin
// UI's tape-set storage-folder picker. Not jailed to any root - an Admin
// token is already trusted to point storage_folder at any absolute path
// (validateTapeSetFolder MkdirAll's it unconditionally), so this endpoint
// carries no additional privilege.
func (s *Server) handleFSBrowse(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = s.cfg.DataDir
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("path must be absolute"))
		return
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.IsDir() {
		writeError(w, http.StatusBadRequest, fmt.Errorf("not a directory: %s", path))
		return
	}
	dirents, err := os.ReadDir(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	resp := fsBrowseResponse{Path: path}
	if parent := filepath.Dir(path); parent != path {
		resp.Parent = parent
	}
	for _, d := range dirents {
		isDir := d.IsDir()
		if d.Type()&os.ModeSymlink != 0 {
			if st, err := os.Stat(filepath.Join(path, d.Name())); err == nil {
				isDir = st.IsDir()
			}
		}
		resp.Entries = append(resp.Entries, fsEntry{Name: d.Name(), IsDir: isDir})
	}
	writeJSON(w, http.StatusOK, resp)
}

type fsEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
}

type fsBrowseResponse struct {
	Path    string    `json:"path"`
	Parent  string    `json:"parent,omitempty"`
	Entries []fsEntry `json:"entries"`
}

// ---- magazines ----
// Magazine changes affect physical slot addressing, so these hot-apply via
// reconfigureFromStore instead of only touching the database.

func (s *Server) handleListMagazines(w http.ResponseWriter, r *http.Request) {
	mags, err := s.topology.ListMagazines()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, mags)
}

func (s *Server) handleCreateMagazine(w http.ResponseWriter, r *http.Request) {
	var m config.MagazineConfig
	if err := decodeJSON(r, &m); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := config.ValidateMagazine(m); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.topology.CreateMagazine(m); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	if err := s.reconfigureFromStore(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (s *Server) handleUpdateMagazine(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var m config.MagazineConfig
	if err := decodeJSON(r, &m); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	m.ID = id
	if err := config.ValidateMagazine(m); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.topology.UpdateMagazine(id, m); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err := s.reconfigureFromStore(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// handleDeleteMagazine refuses to remove a magazine that still has volumes
// in its slots, rather than silently orphaning them.
func (s *Server) handleDeleteMagazine(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, slot := range s.lib.Status().Slots {
		if slot.MagazineID == id && slot.Volume != nil {
			writeError(w, http.StatusConflict, errMagazineNotEmpty)
			return
		}
	}
	if slices.Contains(s.lib.Status().Doors.OpenMagazines, id) {
		writeError(w, http.StatusConflict, errMagazineDoorOpen)
		return
	}
	if err := s.topology.DeleteMagazine(id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err := s.reconfigureFromStore(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var errMagazineNotEmpty = writeErrorSentinel("magazine still has volumes in its slots; unload or delete them first")
var errMagazineDoorOpen = writeErrorSentinel("magazine's storage door is open; close it first")

// ---- mailboxes ----
// Mirrors magazine handling exactly: mailbox changes affect physical
// I/O-slot addressing, so these hot-apply via reconfigureFromStore too.

// mailboxView is the mailbox response DTO: never the raw PIN hash, only
// whether one is configured, mirroring how UserInfo excludes PasswordHash.
type mailboxView struct {
	ID          string `json:"id"`
	Slots       int    `json:"slots"`
	BaseAddress int    `json:"base_address"`
	PINSet      bool   `json:"pin_set"`
}

func mailboxViewFrom(m config.MailboxConfig) mailboxView {
	return mailboxView{ID: m.ID, Slots: m.Slots, BaseAddress: m.BaseAddress, PINSet: m.PINHash != ""}
}

// mailboxRequest's PIN uses pointer semantics so a blank field can't be
// silently misread as "leave unchanged" (there'd be no way to ever clear a
// PIN through the UI otherwise): nil means not provided at all (create:
// no PIN; update: leave the existing PIN untouched), a non-nil empty
// string is an explicit clear, and a non-nil non-empty string sets a new
// PIN (validated against pinRE, hashed via secrethash before storage).
type mailboxRequest struct {
	ID    string  `json:"id"`
	Slots int     `json:"slots"`
	PIN   *string `json:"pin,omitempty"`
}

// resolveMailboxPINHash validates/hashes req.PIN into a MailboxConfig's
// PINHash field, or writes a 400 and returns ok=false on an invalid format.
func (s *Server) resolveMailboxPINHash(w http.ResponseWriter, pin string) (string, bool) {
	if pin == "" {
		return "", true
	}
	if !pinRE.MatchString(pin) {
		writeError(w, http.StatusBadRequest, errInvalidPINFormat)
		return "", false
	}
	hash, err := secrethash.Hash(pin)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return "", false
	}
	return hash, true
}

func (s *Server) handleListMailboxes(w http.ResponseWriter, r *http.Request) {
	mbs, err := s.topology.ListMailboxes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	views := make([]mailboxView, len(mbs))
	for i, m := range mbs {
		views[i] = mailboxViewFrom(m)
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) handleCreateMailbox(w http.ResponseWriter, r *http.Request) {
	var req mailboxRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	m := config.MailboxConfig{ID: req.ID, Slots: req.Slots}
	if req.PIN != nil {
		hash, ok := s.resolveMailboxPINHash(w, *req.PIN)
		if !ok {
			return
		}
		m.PINHash = hash
	}
	if err := config.ValidateMailbox(m); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.topology.CreateMailbox(m); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	if err := s.reconfigureFromStore(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, mailboxViewFrom(m))
}

func (s *Server) handleUpdateMailbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req mailboxRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	m := config.MailboxConfig{ID: id, Slots: req.Slots}
	if req.PIN == nil {
		existing, err := s.topology.ListMailboxes()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		for _, e := range existing {
			if e.ID == id {
				m.PINHash = e.PINHash
				break
			}
		}
	} else {
		hash, ok := s.resolveMailboxPINHash(w, *req.PIN)
		if !ok {
			return
		}
		m.PINHash = hash
	}
	if err := config.ValidateMailbox(m); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.topology.UpdateMailbox(id, m); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err := s.reconfigureFromStore(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, mailboxViewFrom(m))
}

// handleDeleteMailbox refuses to remove a mailbox that still has volumes in
// its I/O slots, rather than silently orphaning them.
func (s *Server) handleDeleteMailbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, io := range s.lib.Status().IOSlots {
		if io.MailboxID == id && io.Volume != nil {
			writeError(w, http.StatusConflict, errMailboxNotEmpty)
			return
		}
	}
	if slices.Contains(s.lib.Status().Doors.OpenMailboxes, id) {
		writeError(w, http.StatusConflict, errMailboxDoorOpen)
		return
	}
	if err := s.topology.DeleteMailbox(id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err := s.reconfigureFromStore(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var errMailboxNotEmpty = writeErrorSentinel("mailbox still has volumes in its I/O slots; unload or delete them first")
var errMailboxDoorOpen = writeErrorSentinel("mailbox's I/O door is open; close it first")

// ---- drive devices ----
// Unlike magazines/mailboxes, a drive device has no separate ID - it's
// addressed by its position in the list, the same convention Load/Unload
// and the fault endpoint already use ("drive:0", .../drives/{index}/fault).
// Create always appends (index = len(devices)), so it never disturbs an
// existing drive's index/device-path pairing, which is what
// Library.Reconfigure relies on to carry a loaded volume across a topology
// change. Delete refuses only when the drive being removed itself has a
// volume loaded - mirrors handleDeleteMagazine/handleDeleteMailbox, which
// have the same "only guard the entity itself" limitation for the
// elements addressed after it.

// driveDeviceView is the Admin > Drives response DTO: the physical device
// (index/path/linked drive-type name) joined against that drive type's
// catalog entry for display, resolved server-side so the frontend doesn't
// need a second fetch+lookup. Model/Generation/Capacity are simply blank
// when the device isn't linked, or when the linked type was since deleted -
// same best-effort display convention as TapeSetConfig.TapeType.
type driveDeviceView struct {
	Index      int    `json:"index"`
	DevicePath string `json:"device_path"`
	DriveType  string `json:"drive_type,omitempty"`
	Model      string `json:"model,omitempty"`
	Generation string `json:"generation,omitempty"`
	Capacity   string `json:"capacity,omitempty"`
}

func driveDeviceViews(devices []config.DriveDeviceConfig, driveTypes []config.DriveType) []driveDeviceView {
	byName := make(map[string]config.DriveType, len(driveTypes))
	for _, dt := range driveTypes {
		byName[dt.Name] = dt
	}
	out := make([]driveDeviceView, len(devices))
	for i, d := range devices {
		v := driveDeviceView{Index: i, DevicePath: d.DevicePath, DriveType: d.DriveType}
		if dt, ok := byName[d.DriveType]; ok {
			v.Model = dt.Model
			v.Generation = dt.Generation
			v.Capacity = dt.Capacity
		}
		out[i] = v
	}
	return out
}

func (s *Server) handleListDrives(w http.ResponseWriter, r *http.Request) {
	devices, err := s.topology.ListDriveDevices()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	driveTypes, err := s.topology.ListDriveTypes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, driveDeviceViews(devices, driveTypes))
}

type driveDeviceRequest struct {
	DevicePath string `json:"device_path"`
	DriveType  string `json:"drive_type,omitempty"`
}

func (s *Server) handleCreateDrive(w http.ResponseWriter, r *http.Request) {
	var req driveDeviceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	devices, err := s.topology.ListDriveDevices()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	path := req.DevicePath
	if path == "" {
		s.mu.RLock()
		dataDir := s.cfg.DataDir
		s.mu.RUnlock()
		path = fmt.Sprintf("%s/drives/drive%d", dataDir, len(devices))
	} else if path[0] != '/' {
		writeError(w, http.StatusBadRequest, errInvalidDevicePath)
		return
	}
	devices = append(devices, config.DriveDeviceConfig{DevicePath: path, DriveType: req.DriveType})
	if err := s.topology.SaveDriveDevices(devices); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.reconfigureFromStore(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	driveTypes, err := s.topology.ListDriveTypes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	views := driveDeviceViews(devices, driveTypes)
	writeJSON(w, http.StatusCreated, views[len(views)-1])
}

func (s *Server) handleUpdateDrive(w http.ResponseWriter, r *http.Request) {
	idx, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req driveDeviceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.DevicePath == "" || req.DevicePath[0] != '/' {
		writeError(w, http.StatusBadRequest, errInvalidDevicePath)
		return
	}
	devices, err := s.topology.ListDriveDevices()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if idx < 0 || idx >= len(devices) {
		writeError(w, http.StatusNotFound, errDriveNotFound)
		return
	}
	devices[idx] = config.DriveDeviceConfig{DevicePath: req.DevicePath, DriveType: req.DriveType}
	if err := s.topology.SaveDriveDevices(devices); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.reconfigureFromStore(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	driveTypes, err := s.topology.ListDriveTypes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, driveDeviceViews(devices, driveTypes)[idx])
}

func (s *Server) handleDeleteDrive(w http.ResponseWriter, r *http.Request) {
	idx, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	for _, d := range s.lib.Status().Drives {
		if d.Index == idx && d.Volume != nil {
			writeError(w, http.StatusConflict, errDriveNotEmpty)
			return
		}
	}
	devices, err := s.topology.ListDriveDevices()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if idx < 0 || idx >= len(devices) {
		writeError(w, http.StatusNotFound, errDriveNotFound)
		return
	}
	devices = append(devices[:idx], devices[idx+1:]...)
	if err := s.topology.SaveDriveDevices(devices); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.reconfigureFromStore(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var errDriveNotEmpty = writeErrorSentinel("drive has a volume loaded; unload it first")
var errDriveNotFound = writeErrorSentinel("drive not found")
var errInvalidDevicePath = writeErrorSentinel("device_path must be an absolute path")
