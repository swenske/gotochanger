package api

import (
	"net/http"
	"regexp"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/library"
	"github.com/swenske/gotochanger/internal/secrethash"
)

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, UpdateSettingsResult{Config: s.settings.Current(), RestartRequired: restartRequiredFields})
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req UpdateSettingsRequest
	if err := decodeJSON(r, &req); err != nil {
		s.emitFailure(r, library.EventCodeConfigSettingsUpdateFailure, "failed to update settings", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.settings.Update(req)
	if err != nil {
		s.emitFailure(r, library.EventCodeConfigSettingsUpdateFailure, "failed to update settings", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.emitEvent(r, library.Event{Code: library.EventCodeConfigSettingsUpdateSuccess, Message: "updated settings"})
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleGetLatencySettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.settings.CurrentLatency())
}

func (s *Server) handleUpdateLatencySettings(w http.ResponseWriter, r *http.Request) {
	var ls config.LatencySettings
	if err := decodeJSON(r, &ls); err != nil {
		s.emitFailure(r, library.EventCodeConfigSettingsUpdateFailure, "failed to update latency settings", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.settings.UpdateLatency(ls)
	if err != nil {
		s.emitFailure(r, library.EventCodeConfigSettingsUpdateFailure, "failed to update latency settings", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.emitEvent(r, library.Event{Code: library.EventCodeConfigSettingsUpdateSuccess, Message: "updated latency settings"})
	writeJSON(w, http.StatusOK, result)
}

// pinRE restricts a PIN to exactly 4 digits, per the request's "4-digit PIN
// code" wording.
var pinRE = regexp.MustCompile(`^[0-9]{4}$`)

var errInvalidPINFormat = writeErrorSentinel("PIN must be exactly 4 digits")

// pinSettingsResponse never exposes the stored hash - only whether a PIN is
// currently configured, mirroring how UserInfo excludes PasswordHash.
type pinSettingsResponse struct {
	Configured bool `json:"configured"`
}

func (s *Server) handleGetPINSettings(w http.ResponseWriter, r *http.Request) {
	hash, _, err := s.topology.GetSetting("magazine_pin_hash")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, pinSettingsResponse{Configured: hash != ""})
}

type updatePINRequest struct {
	MagazinePIN string `json:"magazine_pin"`
}

// handleUpdatePINSettings sets or clears the single global magazine PIN
// (empty magazine_pin clears it - presence-implies-protection, there is no
// separate enabled/disabled flag). Admin-only, like every other Admin
// config page except Backup's manual-download exception.
func (s *Server) handleUpdatePINSettings(w http.ResponseWriter, r *http.Request) {
	var req updatePINRequest
	if err := decodeJSON(r, &req); err != nil {
		s.emitFailure(r, library.EventCodeConfigSettingsUpdateFailure, "failed to update PIN settings", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	hash := ""
	if req.MagazinePIN != "" {
		if !pinRE.MatchString(req.MagazinePIN) {
			s.emitFailure(r, library.EventCodeConfigSettingsUpdateFailure, "failed to update PIN settings", errInvalidPINFormat, nil)
			writeError(w, http.StatusBadRequest, errInvalidPINFormat)
			return
		}
		var err error
		hash, err = secrethash.Hash(req.MagazinePIN)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := s.topology.SetSetting("magazine_pin_hash", hash); err != nil {
		s.emitFailure(r, library.EventCodeConfigSettingsUpdateFailure, "failed to update PIN settings", err, nil)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.lib.UpdateMagazinePINHash(hash)
	s.emitEvent(r, library.Event{Code: library.EventCodeConfigSettingsUpdateSuccess, Message: "updated PIN settings"})
	writeJSON(w, http.StatusOK, pinSettingsResponse{Configured: hash != ""})
}

func (s *Server) handleGetCleaningSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.settings.CurrentCleaning())
}

func (s *Server) handleUpdateCleaningSettings(w http.ResponseWriter, r *http.Request) {
	var cs config.CleaningSettings
	if err := decodeJSON(r, &cs); err != nil {
		s.emitFailure(r, library.EventCodeConfigSettingsUpdateFailure, "failed to update cleaning settings", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.settings.UpdateCleaning(cs)
	if err != nil {
		s.emitFailure(r, library.EventCodeConfigSettingsUpdateFailure, "failed to update cleaning settings", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.emitEvent(r, library.Event{Code: library.EventCodeConfigSettingsUpdateSuccess, Message: "updated cleaning settings"})
	writeJSON(w, http.StatusOK, result)
}
