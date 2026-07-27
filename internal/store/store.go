// Package store persists the dynamic library state (element -> volume
// assignments) to a SQLite database, so a service restart does not lose track of
// which cartridges are loaded where.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "github.com/mattn/go-sqlite3"
	"github.com/swenske/gotochanger/internal/library"
)

// Store is a SQLite-backed store for library state.
type Store struct {
	mu   sync.Mutex
	db   *sql.DB
	path string
}

// New returns a Store writing to path (e.g. <data_dir>/state.db).
func New(path string) *Store {
	return &Store{path: path}
}

// Open opens the SQLite database and initializes the schema.
func (s *Store) Open() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	var err error
	s.db, err = sql.Open("sqlite3", s.path)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}

	if err := s.initSchema(); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}
	if err := s.initTopologySchema(); err != nil {
		return fmt.Errorf("init topology schema: %w", err)
	}
	if err := s.initAuthSchema(); err != nil {
		return fmt.Errorf("init auth schema: %w", err)
	}

	return nil
}

// initSchema initializes the database schema.
func (s *Store) initSchema() error {
	schema := `
	CREATE TABLE IF NOT EXISTS state (
		id INTEGER PRIMARY KEY,
		data TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS config (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	`
	_, err := s.db.Exec(schema)
	return err
}

// Load reads the previously persisted state, if any. A missing file is not
// an error: it simply means this is the first run.
func (s *Store) Load() (*library.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var data []byte
	err := s.db.QueryRow("SELECT data FROM state WHERE id = 1").Scan(&data)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read state: %w", err)
	}

	var st library.State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	return &st, nil
}

// Save atomically writes state to the database.
func (s *Store) Save(state library.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec("INSERT OR REPLACE INTO state (id, data) VALUES (1, ?)", data)
	if err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	return tx.Commit()
}

// Close closes the database connection.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}
