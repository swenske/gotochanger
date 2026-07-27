package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/swenske/gotochanger/internal/config"
)

// authSchema is applied in addition to initSchema/initTopologySchema's
// tables. Users and API tokens used to live in separate users.json/
// tokens.json files (see MigrateUsersAndTokensFromJSON below for the
// one-time upgrade path); they now live here instead, alongside every
// other piece of persisted state.
const authSchema = `
CREATE TABLE IF NOT EXISTS users (
	username TEXT PRIMARY KEY COLLATE NOCASE,
	password_hash TEXT NOT NULL,
	role TEXT NOT NULL,
	must_change_password INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
	name TEXT PRIMARY KEY,
	hash TEXT NOT NULL UNIQUE,
	role TEXT NOT NULL,
	created_at TEXT NOT NULL
);
`

// initAuthSchema is called by Open() alongside initSchema/initTopologySchema.
func (s *Store) initAuthSchema() error {
	_, err := s.db.Exec(authSchema)
	return err
}

// ---- users ----

func (s *Store) ListUserRows() ([]config.UserRow, error) {
	rows, err := s.db.Query("SELECT username, password_hash, role, must_change_password, created_at, updated_at FROM users ORDER BY username")
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	var out []config.UserRow
	for rows.Next() {
		u, err := scanUserRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) GetUserRow(username string) (config.UserRow, bool, error) {
	row := s.db.QueryRow("SELECT username, password_hash, role, must_change_password, created_at, updated_at FROM users WHERE username = ?", username)
	u, err := scanUserRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return config.UserRow{}, false, nil
	}
	if err != nil {
		return config.UserRow{}, false, fmt.Errorf("get user %s: %w", username, err)
	}
	return u, true, nil
}

// userRowScanner is satisfied by both *sql.Row and *sql.Rows.
type userRowScanner interface {
	Scan(dest ...any) error
}

func scanUserRow(row userRowScanner) (config.UserRow, error) {
	var u config.UserRow
	var mustChange int
	var createdAt, updatedAt string
	if err := row.Scan(&u.Username, &u.PasswordHash, &u.Role, &mustChange, &createdAt, &updatedAt); err != nil {
		return config.UserRow{}, err
	}
	u.MustChangePassword = mustChange != 0
	u.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	u.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return u, nil
}

// CreateUserRow inserts a new user, rejecting a duplicate (case-insensitive)
// username.
func (s *Store) CreateUserRow(u config.UserRow) error {
	_, err := s.db.Exec("INSERT INTO users (username, password_hash, role, must_change_password, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
		u.Username, u.PasswordHash, u.Role, boolToInt(u.MustChangePassword),
		u.CreatedAt.UTC().Format(time.RFC3339Nano), u.UpdatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("user %s already exists or is invalid: %w", u.Username, err)
	}
	return nil
}

// UpdateUserRow replaces an existing user's mutable fields (password hash,
// role, must-change-password flag, updated-at). Username/created-at are
// never changed once a row exists.
func (s *Store) UpdateUserRow(username string, u config.UserRow) error {
	res, err := s.db.Exec("UPDATE users SET password_hash = ?, role = ?, must_change_password = ?, updated_at = ? WHERE username = ?",
		u.PasswordHash, u.Role, boolToInt(u.MustChangePassword), u.UpdatedAt.UTC().Format(time.RFC3339Nano), username)
	if err != nil {
		return fmt.Errorf("update user %s: %w", username, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("user %s not found", username)
	}
	return nil
}

func (s *Store) DeleteUserRow(username string) error {
	res, err := s.db.Exec("DELETE FROM users WHERE username = ?", username)
	if err != nil {
		return fmt.Errorf("delete user %s: %w", username, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("user %s not found", username)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- tokens ----

func (s *Store) ListTokenRows() ([]config.TokenRow, error) {
	rows, err := s.db.Query("SELECT name, hash, role, created_at FROM tokens ORDER BY rowid")
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()
	var out []config.TokenRow
	for rows.Next() {
		var t config.TokenRow
		var createdAt string
		if err := rows.Scan(&t.Name, &t.Hash, &t.Role, &createdAt); err != nil {
			return nil, fmt.Errorf("scan token: %w", err)
		}
		t.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateTokenRow inserts a new token, rejecting a duplicate name.
func (s *Store) CreateTokenRow(t config.TokenRow) error {
	_, err := s.db.Exec("INSERT INTO tokens (name, hash, role, created_at) VALUES (?, ?, ?, ?)",
		t.Name, t.Hash, t.Role, t.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("token %s already exists or is invalid: %w", t.Name, err)
	}
	return nil
}

// DeleteTokenRow removes the token named name, silently succeeding if no
// such token exists - mirrors the pre-SQLite TokenStore.Revoke's "silently
// no-op if none match" behavior.
func (s *Store) DeleteTokenRow(name string) error {
	if _, err := s.db.Exec("DELETE FROM tokens WHERE name = ?", name); err != nil {
		return fmt.Errorf("delete token %s: %w", name, err)
	}
	return nil
}

// ---- one-time migration from the pre-SQLite users.json/tokens.json files ----

// legacyUserRecord/legacyUserFile/legacyTokenRecord/legacyTokenFile mirror
// the exact on-disk JSON shapes internal/api/users.go and tokens.go used to
// write (Role stored as a plain string, same as config.UserRow/TokenRow) -
// duplicated here rather than imported, since internal/store must not
// import internal/api (internal/api already imports internal/store).
type legacyUserRecord struct {
	Username           string    `json:"username"`
	PasswordHash       string    `json:"password_hash"`
	Role               string    `json:"role"`
	MustChangePassword bool      `json:"must_change_password"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type legacyUserFile struct {
	Users []*legacyUserRecord `json:"users"`
}

type legacyTokenRecord struct {
	Name      string    `json:"name"`
	Hash      string    `json:"hash"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

type legacyTokenFile struct {
	Tokens []legacyTokenRecord `json:"tokens"`
}

// MigrateUsersAndTokensFromJSON imports pre-existing users.json/tokens.json
// content into the users/tokens tables above, verbatim (no re-hashing, no
// re-validation), the first time each respective table is empty. "Table has
// zero rows" is itself a sufficient, unambiguous one-time guard here (unlike
// e.g. migrateLegacyLatencySetting in topology.go, where an empty/zero
// value is ambiguous with "never configured") - a fresh install seeds
// nothing into users/tokens until bootstrap, so zero rows always means
// "not migrated yet", never "deliberately empty". Never deletes or
// rewrites the original JSON files - they're simply left in place, unread,
// once migrated, matching this project's "don't destroy legacy data" rule
// (the same reasoning documented for repairLegacyTapeTypeBarcodeFormats).
// Call this before opening either file as JSON at startup.
func (s *Store) MigrateUsersAndTokensFromJSON(usersPath, tokensPath string) error {
	if err := s.migrateUsersFromJSON(usersPath); err != nil {
		return fmt.Errorf("migrate users from %s: %w", usersPath, err)
	}
	if err := s.migrateTokensFromJSON(tokensPath); err != nil {
		return fmt.Errorf("migrate tokens from %s: %w", tokensPath, err)
	}
	return nil
}

func (s *Store) migrateUsersFromJSON(path string) error {
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&n); err != nil {
		return fmt.Errorf("count users: %w", err)
	}
	if n > 0 {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read legacy users file: %w", err)
	}
	var uf legacyUserFile
	if err := json.Unmarshal(data, &uf); err != nil {
		return fmt.Errorf("parse legacy users file: %w", err)
	}
	for _, u := range uf.Users {
		if u == nil {
			continue
		}
		if err := s.CreateUserRow(config.UserRow{
			Username:           u.Username,
			PasswordHash:       u.PasswordHash,
			Role:               u.Role,
			MustChangePassword: u.MustChangePassword,
			CreatedAt:          u.CreatedAt,
			UpdatedAt:          u.UpdatedAt,
		}); err != nil {
			return fmt.Errorf("migrate user %s: %w", u.Username, err)
		}
	}
	return nil
}

func (s *Store) migrateTokensFromJSON(path string) error {
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM tokens").Scan(&n); err != nil {
		return fmt.Errorf("count tokens: %w", err)
	}
	if n > 0 {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read legacy tokens file: %w", err)
	}
	var tf legacyTokenFile
	if err := json.Unmarshal(data, &tf); err != nil {
		return fmt.Errorf("parse legacy tokens file: %w", err)
	}
	// The pre-SQLite tokens.json format never enforced unique names (Add
	// just appended), so a real legacy file could in principle contain a
	// duplicate - the new tokens table does enforce uniqueness. Keep only
	// the first occurrence of any name rather than letting an unlikely,
	// pre-existing data quirk abort the whole migration.
	seen := map[string]bool{}
	for _, t := range tf.Tokens {
		if seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		if err := s.CreateTokenRow(config.TokenRow{Name: t.Name, Hash: t.Hash, Role: t.Role, CreatedAt: t.CreatedAt}); err != nil {
			return fmt.Errorf("migrate token %s: %w", t.Name, err)
		}
	}
	return nil
}
