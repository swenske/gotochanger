package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Path returns the on-disk path of the SQLite database file (<data_dir>/state.db).
func (s *Store) Path() string {
	return s.path
}

// sqliteMagic is the fixed 16-byte header every SQLite database file starts
// with (see https://www.sqlite.org/fileformat.html#the_database_header) -
// checked before ever handing an uploaded file to the sqlite3 driver, so an
// obviously-not-a-database upload is rejected with a clear error instead of
// a confusing driver failure.
var sqliteMagic = []byte("SQLite format 3\x00")

// backupNamePartRE keeps generated backup filenames filesystem- and
// URL-safe: only letters, digits, underscore, dot and hyphen.
var backupNamePartRE = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func sanitizeBackupNamePart(s string) string {
	s = backupNamePartRE.ReplaceAllString(strings.TrimSpace(s), "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return "vtl"
	}
	return s
}

// BackupFilename builds the "<vtl_name>_<timestamp>.db" filename used for
// both the manual on-demand download and scheduled backup files, per the
// convention: file name = the VTL name + timestamp.
func BackupFilename(vtlName string, at time.Time) string {
	return fmt.Sprintf("%s_%s.db", sanitizeBackupNamePart(vtlName), at.UTC().Format("20060102-150405"))
}

// VacuumSnapshot writes a consistent, compacted copy of the live database to
// destPath using SQLite's `VACUUM INTO` (supported since SQLite 3.27; the
// vendored driver bundles 3.39.4), which takes its own internal read lock
// and produces a complete, self-contained snapshot without needing to stop
// the daemon or use the lower-level backup C API. destPath must not already
// exist - VACUUM INTO refuses to overwrite a file.
func (s *Store) VacuumSnapshot(destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return fmt.Errorf("create backup directory: %w", err)
	}
	if _, err := s.db.Exec("VACUUM INTO ?", destPath); err != nil {
		return fmt.Errorf("snapshot database: %w", err)
	}
	return nil
}

// CreateBackupFile snapshots the live database directly into backupsDir
// under the standard "<vtl_name>_<timestamp>.db" name and returns the
// filename. Used by both the manual "download now" handler (which streams
// the file back and then deletes it) and the scheduled backup ticker (which
// leaves it in place for later download/pruning).
func (s *Store) CreateBackupFile(backupsDir, vtlName string) (string, error) {
	at := time.Now()
	name := BackupFilename(vtlName, at)
	dest := filepath.Join(backupsDir, name)
	if err := s.VacuumSnapshot(dest); err != nil {
		return "", err
	}
	// Recorded here rather than at each of this method's three call sites
	// (manual download, the scheduled backup ticker, the pre-reset safety
	// backup) so gotochanger_last_backup_timestamp reflects all of them from
	// one place. Persisted (not just an in-memory value) so it survives a
	// restart and isn't lost the way the capped, in-memory-only event
	// history would eventually lose it.
	if err := s.SetSetting("last_backup_at", strconv.FormatInt(at.Unix(), 10)); err != nil {
		return "", err
	}
	return name, nil
}

// BackupFileInfo describes one stored backup file for the Admin UI/CLI.
type BackupFileInfo struct {
	Name      string    `json:"name"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// validBackupFilename rejects anything that isn't a plain "<stuff>.db" leaf
// name - no path separators, no "..", nothing that could escape backupsDir
// when joined onto it (OWASP A01: path traversal).
func validBackupFilename(name string) bool {
	if name == "" || name != filepath.Base(name) || strings.Contains(name, "..") {
		return false
	}
	return strings.HasSuffix(name, ".db")
}

// ListBackupFiles lists backupsDir's *.db files, newest first. A missing
// directory (no backup has ever been taken) is not an error - it just
// yields an empty list.
func (s *Store) ListBackupFiles(backupsDir string) ([]BackupFileInfo, error) {
	entries, err := os.ReadDir(backupsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list backups: %w", err)
	}
	out := make([]BackupFileInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !validBackupFilename(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupFileInfo{Name: e.Name(), SizeBytes: info.Size(), CreatedAt: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// DeleteBackupFile removes one stored backup file by name.
func (s *Store) DeleteBackupFile(backupsDir, name string) error {
	if !validBackupFilename(name) {
		return fmt.Errorf("invalid backup filename %q", name)
	}
	if err := os.Remove(filepath.Join(backupsDir, name)); err != nil {
		return fmt.Errorf("delete backup %s: %w", name, err)
	}
	return nil
}

// PruneBackupFiles deletes the oldest scheduled backups beyond the newest
// `keep` files. keep <= 0 disables pruning (unlimited retention).
func (s *Store) PruneBackupFiles(backupsDir string, keep int) error {
	if keep <= 0 {
		return nil
	}
	files, err := s.ListBackupFiles(backupsDir)
	if err != nil {
		return err
	}
	if len(files) <= keep {
		return nil
	}
	for _, f := range files[keep:] {
		if err := s.DeleteBackupFile(backupsDir, f.Name); err != nil {
			return err
		}
	}
	return nil
}

// BackupFilePath joins backupsDir and a validated filename, for streaming a
// stored backup back to the client.
func (s *Store) BackupFilePath(backupsDir, name string) (string, error) {
	if !validBackupFilename(name) {
		return "", fmt.Errorf("invalid backup filename %q", name)
	}
	return filepath.Join(backupsDir, name), nil
}

// validateBackupSource checks that path looks like a gotochanger database
// before Restore ever closes the live database: the SQLite file header must
// match, and the file must already contain this package's `config` table
// (present in every gotochanger database since it's created unconditionally
// by initSchema, even on a freshly seeded, otherwise-empty install) - this
// catches "wrong file" mistakes (e.g. uploading a random SQLite file, or the
// bareos catalog by accident) with a clear error instead of leaving the
// daemon with a half-swapped, unopenable database.
func validateBackupSource(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open uploaded file: %w", err)
	}
	header := make([]byte, len(sqliteMagic))
	_, err = f.Read(header)
	f.Close()
	if err != nil {
		return fmt.Errorf("read uploaded file: %w", err)
	}
	for i, b := range sqliteMagic {
		if header[i] != b {
			return fmt.Errorf("uploaded file is not a SQLite database")
		}
	}

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return fmt.Errorf("open uploaded database: %w", err)
	}
	defer db.Close()
	var name string
	err = db.QueryRow("SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'config'").Scan(&name)
	if err != nil {
		return fmt.Errorf("uploaded file does not look like a gotochanger backup: %w", err)
	}
	return nil
}

// Restore replaces the live database with the one at tmpPath (already
// validated and written to disk by the caller - see the API layer's restore
// handler) and reopens it in place. tmpPath must be on the same filesystem
// as the store's own database file, since the swap is done with os.Rename
// for atomicity.
//
// This does *not* attempt to hot-reload the running Library or Server state
// from the new database - the restored data can be arbitrarily different
// from what's currently loaded in memory (different topology, different
// element->volume assignments entirely), which is a fundamentally different
// operation from the small, additive topology tweaks Library.Reconfigure
// handles. The caller is expected to trigger a process restart immediately
// after a successful Restore (the daemon's systemd unit has
// Restart=on-failure, so a deliberate non-zero exit picks the new database
// up cleanly on the next start).
func (s *Store) Restore(tmpPath string) error {
	if err := validateBackupSource(tmpPath); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.db != nil {
		if err := s.db.Close(); err != nil {
			return fmt.Errorf("close database before restore: %w", err)
		}
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		// Reopen the original (untouched) database so a rename failure
		// (e.g. tmpPath ended up on a different filesystem) doesn't leave
		// the daemon with its store closed and every subsequent operation
		// failing until a manual restart.
		if db, reopenErr := sql.Open("sqlite3", s.path); reopenErr == nil {
			s.db = db
		}
		return fmt.Errorf("install restored database: %w", err)
	}

	db, err := sql.Open("sqlite3", s.path)
	if err != nil {
		return fmt.Errorf("reopen database after restore: %w", err)
	}
	s.db = db
	if err := s.initSchema(); err != nil {
		return fmt.Errorf("init schema after restore: %w", err)
	}
	if err := s.initTopologySchema(); err != nil {
		return fmt.Errorf("init topology schema after restore: %w", err)
	}
	return nil
}
