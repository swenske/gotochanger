package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swenske/gotochanger/internal/config"
)

func TestUserRowCRUDRoundTrip(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	u := config.UserRow{Username: "Admin", PasswordHash: "pbkdf2-sha256$1$AA$AA", Role: "admin", MustChangePassword: true, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateUserRow(u); err != nil {
		t.Fatalf("create user: %v", err)
	}

	got, ok, err := s.GetUserRow("Admin")
	if err != nil || !ok {
		t.Fatalf("get user: ok=%v err=%v", ok, err)
	}
	if got.Username != "Admin" || got.PasswordHash != u.PasswordHash || got.Role != "admin" || !got.MustChangePassword {
		t.Fatalf("unexpected row: %+v", got)
	}
	if !got.CreatedAt.Equal(now) || !got.UpdatedAt.Equal(now) {
		t.Fatalf("timestamps did not round-trip: got created=%v updated=%v, want %v", got.CreatedAt, got.UpdatedAt, now)
	}

	// Lookup must be case-insensitive (COLLATE NOCASE), matching the old
	// JSON store's lower-cased map key semantics.
	if _, ok, err := s.GetUserRow("admin"); err != nil || !ok {
		t.Fatalf("expected case-insensitive lookup to find the user, ok=%v err=%v", ok, err)
	}

	later := now.Add(time.Hour)
	u.PasswordHash = "pbkdf2-sha256$1$BB$BB"
	u.MustChangePassword = false
	u.UpdatedAt = later
	if err := s.UpdateUserRow("Admin", u); err != nil {
		t.Fatalf("update user: %v", err)
	}
	got, _, _ = s.GetUserRow("Admin")
	if got.PasswordHash != "pbkdf2-sha256$1$BB$BB" || got.MustChangePassword {
		t.Fatalf("update did not apply: %+v", got)
	}

	if err := s.DeleteUserRow("Admin"); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, ok, _ := s.GetUserRow("Admin"); ok {
		t.Fatalf("expected user gone after delete")
	}
	if err := s.DeleteUserRow("Admin"); err == nil {
		t.Fatalf("expected deleting an already-deleted user to error")
	}
}

func TestCreateUserRowRejectsDuplicateCaseInsensitive(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	if err := s.CreateUserRow(config.UserRow{Username: "Admin", Role: "admin", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create first user: %v", err)
	}
	if err := s.CreateUserRow(config.UserRow{Username: "admin", Role: "viewer", CreatedAt: now, UpdatedAt: now}); err == nil {
		t.Fatalf("expected a case-insensitive duplicate username to be rejected")
	}
}

func TestTokenRowCRUDRoundTrip(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	tok := config.TokenRow{Name: "bootstrap", Hash: "deadbeef", Role: "admin", CreatedAt: now}
	if err := s.CreateTokenRow(tok); err != nil {
		t.Fatalf("create token: %v", err)
	}
	got, err := s.ListTokenRows()
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	if len(got) != 1 || got[0].Name != "bootstrap" || got[0].Hash != "deadbeef" || got[0].Role != "admin" {
		t.Fatalf("unexpected tokens: %+v", got)
	}

	if err := s.CreateTokenRow(config.TokenRow{Name: "bootstrap", Hash: "othervalue", Role: "viewer", CreatedAt: now}); err == nil {
		t.Fatalf("expected a duplicate token name to be rejected")
	}

	// Revoke on an unknown name is a silent no-op, matching the pre-SQLite
	// TokenStore.Revoke's documented behavior.
	if err := s.DeleteTokenRow("no-such-token"); err != nil {
		t.Fatalf("expected deleting an unknown token to be a no-op, got: %v", err)
	}

	if err := s.DeleteTokenRow("bootstrap"); err != nil {
		t.Fatalf("delete token: %v", err)
	}
	got, err = s.ListTokenRows()
	if err != nil {
		t.Fatalf("list tokens after delete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no tokens after delete, got %+v", got)
	}
}

// TestMigrateUsersAndTokensFromJSONImportsLegacyFiles reproduces a real
// pre-SQLite upgrade: existing users.json/tokens.json content (in the exact
// on-disk shape internal/api/users.go and tokens.go used to write) must be
// imported verbatim - same password/token hashes, no re-hashing - and the
// migration must never run again once the tables are populated.
func TestMigrateUsersAndTokensFromJSONImportsLegacyFiles(t *testing.T) {
	tmp := t.TempDir()
	usersPath := filepath.Join(tmp, "users.json")
	tokensPath := filepath.Join(tmp, "tokens.json")

	usersJSON := `{"users":[
		{"username":"Admin","password_hash":"pbkdf2-sha256$600000$AAAA$BBBB","role":"admin","must_change_password":false,"created_at":"2025-01-01T00:00:00Z","updated_at":"2025-06-01T00:00:00Z"},
		{"username":"alice","password_hash":"pbkdf2-sha256$600000$CCCC$DDDD","role":"operator","must_change_password":true,"created_at":"2025-02-01T00:00:00Z","updated_at":"2025-02-01T00:00:00Z"}
	]}`
	if err := os.WriteFile(usersPath, []byte(usersJSON), 0o600); err != nil {
		t.Fatalf("write legacy users file: %v", err)
	}
	tokensJSON := `{"tokens":[
		{"name":"bootstrap","hash":"deadbeef00000000000000000000000000000000000000000000000000beef","role":"admin","created_at":"2025-01-01T00:00:00Z"}
	]}`
	if err := os.WriteFile(tokensPath, []byte(tokensJSON), 0o600); err != nil {
		t.Fatalf("write legacy tokens file: %v", err)
	}

	s := newTestStore(t)
	if err := s.MigrateUsersAndTokensFromJSON(usersPath, tokensPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	admin, ok, err := s.GetUserRow("Admin")
	if err != nil || !ok {
		t.Fatalf("expected Admin to survive migration, ok=%v err=%v", ok, err)
	}
	if admin.PasswordHash != "pbkdf2-sha256$600000$AAAA$BBBB" {
		t.Fatalf("expected the exact legacy password hash to be carried over verbatim, got %q", admin.PasswordHash)
	}
	alice, ok, err := s.GetUserRow("alice")
	if err != nil || !ok || alice.Role != "operator" || !alice.MustChangePassword {
		t.Fatalf("expected alice to survive migration with her original fields, got %+v (ok=%v err=%v)", alice, ok, err)
	}

	toks, err := s.ListTokenRows()
	if err != nil || len(toks) != 1 || toks[0].Name != "bootstrap" || toks[0].Hash != "deadbeef00000000000000000000000000000000000000000000000000beef" {
		t.Fatalf("expected the legacy bootstrap token to survive migration verbatim, got %+v (err=%v)", toks, err)
	}

	// Re-running (e.g. on a second daemon restart) must be a no-op: adding
	// a new admin directly to users.json on disk must NOT reappear, since
	// the tables are no longer empty.
	if err := os.WriteFile(usersPath, []byte(`{"users":[{"username":"ShouldNotAppear","role":"admin"}]}`), 0o600); err != nil {
		t.Fatalf("rewrite legacy users file: %v", err)
	}
	if err := s.MigrateUsersAndTokensFromJSON(usersPath, tokensPath); err != nil {
		t.Fatalf("re-running migration should be a no-op, got: %v", err)
	}
	if _, ok, _ := s.GetUserRow("ShouldNotAppear"); ok {
		t.Fatalf("expected re-running the migration to NOT re-import from a changed JSON file")
	}
}

// TestMigrateUsersAndTokensFromJSONNoLegacyFilesIsANoOp confirms a fresh
// install (no users.json/tokens.json at all) doesn't error - the migration
// is meant to be safe to call unconditionally at every startup.
func TestMigrateUsersAndTokensFromJSONNoLegacyFilesIsANoOp(t *testing.T) {
	tmp := t.TempDir()
	s := newTestStore(t)
	if err := s.MigrateUsersAndTokensFromJSON(filepath.Join(tmp, "no-users.json"), filepath.Join(tmp, "no-tokens.json")); err != nil {
		t.Fatalf("expected no error when no legacy files exist, got: %v", err)
	}
	rows, err := s.ListUserRows()
	if err != nil || len(rows) != 0 {
		t.Fatalf("expected no users, got %+v (err=%v)", rows, err)
	}
}

// TestMigrateTokensFromJSONDedupesLegacyDuplicateNames guards against the
// pre-SQLite tokens.json format's lack of a uniqueness constraint (Add
// never checked for an existing name) - a real legacy file with a
// duplicate name must not abort the whole migration.
func TestMigrateTokensFromJSONDedupesLegacyDuplicateNames(t *testing.T) {
	tmp := t.TempDir()
	tokensPath := filepath.Join(tmp, "tokens.json")
	tokensJSON := `{"tokens":[
		{"name":"dup","hash":"first0000000000000000000000000000000000000000000000000000000hash","role":"admin","created_at":"2025-01-01T00:00:00Z"},
		{"name":"dup","hash":"second000000000000000000000000000000000000000000000000000000hash","role":"viewer","created_at":"2025-01-02T00:00:00Z"}
	]}`
	if err := os.WriteFile(tokensPath, []byte(tokensJSON), 0o600); err != nil {
		t.Fatalf("write legacy tokens file: %v", err)
	}
	s := newTestStore(t)
	if err := s.MigrateUsersAndTokensFromJSON(filepath.Join(tmp, "no-users.json"), tokensPath); err != nil {
		t.Fatalf("expected migration to tolerate a duplicate legacy token name, got: %v", err)
	}
	toks, err := s.ListTokenRows()
	if err != nil || len(toks) != 1 || toks[0].Hash != "first0000000000000000000000000000000000000000000000000000000hash" {
		t.Fatalf("expected exactly one token, keeping the first occurrence, got %+v (err=%v)", toks, err)
	}
}
