package api

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/library"
)

var errBackupUnavailable = writeErrorSentinel("backup/restore is not available on this server")

// maxRestoreUploadBytes caps an uploaded restore file at 2 GiB - generous
// for a topology+dynamic-state database (typically KB to low MB), while
// still bounding memory/disk use against an abusive upload.
const maxRestoreUploadBytes = 2 << 30

// ---- manual backup (operator+): trigger and download in one request ----

// handleBackupDownload snapshots the live database with VACUUM INTO (see
// store.Store.VacuumSnapshot) into a scratch subdirectory of backupsDir,
// streams it back as an attachment named "<vtl_name>_<timestamp>.db", and
// deletes the scratch file - a manual download isn't kept as a stored
// backup the way a scheduled one is.
func (s *Server) handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	if s.backup == nil {
		s.emitFailure(r, library.EventCodeConfigBackupCreateFailure, "backup unavailable", errBackupUnavailable, nil)
		writeError(w, http.StatusServiceUnavailable, errBackupUnavailable)
		return
	}
	vtlName := s.settings.Current().Library.Name
	scratchDir := filepath.Join(s.backupsDir, ".manual")
	name, err := s.backup.CreateBackupFile(scratchDir, vtlName)
	if err != nil {
		s.emitFailure(r, library.EventCodeConfigBackupCreateFailure, "failed to create backup", err, nil)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	path := filepath.Join(scratchDir, name)
	defer os.Remove(path)

	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	if info, err := f.Stat(); err == nil {
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	}
	s.emitEvent(r, library.Event{Code: library.EventCodeConfigBackupCreateSuccess, Message: "created manual backup " + name})
	_, _ = io.Copy(w, f)
}

// ---- scheduled backup configuration (admin) ----

// BackupScheduleInfo is the schedule settings surfaced to the Admin UI/CLI.
type BackupScheduleInfo struct {
	Interval  string `json:"interval"`           // duration string ("24h"); empty disables scheduled backups
	Retention int    `json:"retention"`          // how many scheduled backups to keep; 0/unset = unlimited
	LastRun   string `json:"last_run,omitempty"` // RFC3339, empty if never run
}

// BackupScheduleRequest carries a partial update, both fields optional (nil
// = leave unchanged), matching UpdateSettingsRequest's convention.
type BackupScheduleRequest struct {
	Interval  *string `json:"interval,omitempty"`
	Retention *int    `json:"retention,omitempty"`
}

func (s *Server) currentBackupSchedule() BackupScheduleInfo {
	var info BackupScheduleInfo
	if s.topology == nil {
		return info
	}
	info.Interval, _, _ = s.topology.GetSetting("backup_schedule_interval")
	retentionRaw, _, _ := s.topology.GetSetting("backup_schedule_retention")
	info.Retention, _ = strconv.Atoi(retentionRaw)
	info.LastRun, _, _ = s.topology.GetSetting("backup_schedule_last_run")
	return info
}

func (s *Server) handleGetBackupSchedule(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.currentBackupSchedule())
}

func (s *Server) handleUpdateBackupSchedule(w http.ResponseWriter, r *http.Request) {
	var req BackupScheduleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Interval != nil && *req.Interval != "" {
		if _, err := config.ParseDuration(*req.Interval); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("interval: %w", err))
			return
		}
	}
	if req.Retention != nil && *req.Retention < 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("retention must not be negative"))
		return
	}
	if s.topology == nil {
		writeError(w, http.StatusServiceUnavailable, errBackupUnavailable)
		return
	}
	if req.Interval != nil {
		if err := s.topology.SetSetting("backup_schedule_interval", *req.Interval); err != nil {
			s.emitFailure(r, library.EventCodeConfigBackupScheduleUpdateFailure, "failed to update backup schedule", err, nil)
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if req.Retention != nil {
		if err := s.topology.SetSetting("backup_schedule_retention", strconv.Itoa(*req.Retention)); err != nil {
			s.emitFailure(r, library.EventCodeConfigBackupScheduleUpdateFailure, "failed to update backup schedule", err, nil)
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	s.emitEvent(r, library.Event{Code: library.EventCodeConfigBackupScheduleUpdateSuccess, Message: "updated backup schedule"})
	writeJSON(w, http.StatusOK, s.currentBackupSchedule())
}

// ---- stored (scheduled) backups (admin) ----

func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	if s.backup == nil {
		writeJSON(w, http.StatusOK, []struct{}{})
		return
	}
	files, err := s.backup.ListBackupFiles(s.backupsDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, files)
}

func (s *Server) handleDownloadStoredBackup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("filename")
	path, err := s.backup.BackupFilePath(s.backupsDir, name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("backup %s not found", name))
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	if info, err := f.Stat(); err == nil {
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	}
	_, _ = io.Copy(w, f)
}

func (s *Server) handleDeleteStoredBackup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("filename")
	if err := s.backup.DeleteBackupFile(s.backupsDir, name); err != nil {
		s.emitFailure(r, library.EventCodeConfigBackupDeleteFailure, "failed to delete backup", err, nil)
		writeError(w, http.StatusNotFound, err)
		return
	}
	s.emitEvent(r, library.Event{Code: library.EventCodeConfigBackupDeleteSuccess, Message: "deleted backup " + name})
	w.WriteHeader(http.StatusNoContent)
}

// ---- restore (admin) ----

// handleRestore accepts a raw database file upload (Content-Type doesn't
// matter - the body is read as opaque bytes, same convention as
// /api/v1/snmp/mib's download direction but reversed), writes it to a
// scratch file next to the live database, and hands it to
// store.Store.Restore, which validates it, swaps it in, and reopens it.
// Restore does not attempt to hot-reload the running Library/Server state
// (see Restore's doc comment for why) - a successful restore instead
// triggers a deliberate process exit via the restart callback registered by
// main.go, relying on the daemon's systemd unit (Restart=on-failure) to
// bring it back up against the new database within a couple of seconds.
// This is the same endpoint used by both Admin > Backup's restore form and
// wizard step 1's "restore instead of configuring from scratch" option -
// both are reached only after bootstrap/login, so the caller already holds
// an Admin session either way.
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	if s.backup == nil {
		s.emitFailure(r, library.EventCodeConfigRestoreFailure, "restore unavailable", errBackupUnavailable, nil)
		writeError(w, http.StatusServiceUnavailable, errBackupUnavailable)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRestoreUploadBytes)
	dir := filepath.Dir(s.backup.Path())
	tmp, err := os.CreateTemp(dir, "restore-upload-*.db")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	tmpPath := tmp.Name()

	_, copyErr := io.Copy(tmp, r.Body)
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmpPath)
		err := copyErr
		if err == nil {
			err = closeErr
		}
		s.emitFailure(r, library.EventCodeConfigRestoreFailure, "failed to receive uploaded backup", err, nil)
		writeError(w, http.StatusBadRequest, fmt.Errorf("read upload: %w", err))
		return
	}

	if err := s.backup.Restore(tmpPath); err != nil {
		_ = os.Remove(tmpPath) // no-op if Restore already consumed/renamed it
		s.emitFailure(r, library.EventCodeConfigRestoreFailure, "restore failed", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}

	s.emitEvent(r, library.Event{Code: library.EventCodeConfigRestoreSuccess, Message: "database restored; service restarting"})
	writeJSON(w, http.StatusOK, map[string]string{"message": "restore successful; the service is restarting to apply it"})

	s.mu.RLock()
	restart := s.restartFunc
	s.mu.RUnlock()
	if restart != nil {
		restart()
	}
}
