// Package api exposes the REST API used by the web UI, gotochangerctl and
// the gotochanger-changer Bareos compatibility shim.
package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/swenske/gotochanger/internal/config"
)

// TokenRecord is a single API token, stored hashed (never in plaintext).
// Role scopes what the token's bearer is permitted to do, mirroring user
// account roles (admin/operator/viewer).
type TokenRecord struct {
	Name      string    `json:"name"`
	Hash      string    `json:"hash"` // hex sha256 of the raw token
	Role      Role      `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

func tokenRecordFromRow(row config.TokenRow) TokenRecord {
	return TokenRecord{Name: row.Name, Hash: row.Hash, Role: Role(row.Role), CreatedAt: row.CreatedAt}
}

// TokenRecordStore is everything TokenStore needs from the SQLite-backed
// topology store (internal/store/auth.go) to manage API tokens. Defined as
// an interface, like UserRecordStore, so this package doesn't need to
// import internal/store directly.
type TokenRecordStore interface {
	ListTokenRows() ([]config.TokenRow, error)
	CreateTokenRow(config.TokenRow) error
	DeleteTokenRow(name string) error
}

// TokenStore manages API tokens used to authenticate requests made over the
// TCP listener (the Unix socket listener is trusted implicitly via
// filesystem permissions and does not consult TokenStore). Persisted via
// TokenRecordStore (SQLite - this used to be a JSON file), but keeps an
// in-memory read-through cache - unlike UserStore, Verify runs on every
// single token-authenticated HTTP request, so hitting SQLite per-request
// would be an avoidable regression versus the old in-memory linear scan.
// Add/Revoke still go through the store as the durable source of truth,
// refreshing the cache afterward.
type TokenStore struct {
	mu    sync.RWMutex
	store TokenRecordStore
	toks  []TokenRecord
}

// LoadOrBootstrapTokenStore wraps store, creating a single freshly
// generated admin-scoped "bootstrap" token if the tokens table is empty.
// The plaintext bootstrap token is returned only on first creation so it
// can be printed once to the service log; it is never stored or logged
// again.
func LoadOrBootstrapTokenStore(store TokenRecordStore) (*TokenStore, string, error) {
	ts := &TokenStore{store: store}
	rows, err := store.ListTokenRows()
	if err != nil {
		return nil, "", fmt.Errorf("list tokens: %w", err)
	}
	if len(rows) > 0 {
		ts.toks = make([]TokenRecord, len(rows))
		for i, row := range rows {
			ts.toks[i] = tokenRecordFromRow(row)
		}
		return ts, "", nil
	}

	raw, err := generateToken()
	if err != nil {
		return nil, "", err
	}
	row := config.TokenRow{Name: "bootstrap", Hash: hashToken(raw), Role: string(RoleAdmin), CreatedAt: time.Now().UTC()}
	if err := store.CreateTokenRow(row); err != nil {
		return nil, "", err
	}
	ts.toks = []TokenRecord{tokenRecordFromRow(row)}
	return ts, raw, nil
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Verify reports whether raw matches any stored token, using a
// constant-time comparison to avoid timing side-channels (OWASP A02/A07).
// It returns the token's scoped role on success.
func (ts *TokenStore) Verify(raw string) (Role, bool) {
	if raw == "" {
		return "", false
	}
	want := hashToken(raw)
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	for _, t := range ts.toks {
		if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(want)) == 1 {
			return t.Role, true
		}
	}
	return "", false
}

// Add generates and stores a new named token scoped to role, returning its
// plaintext value once.
func (ts *TokenStore) Add(name string, role Role) (string, error) {
	raw, err := generateToken()
	if err != nil {
		return "", err
	}
	row := config.TokenRow{Name: name, Hash: hashToken(raw), Role: string(role), CreatedAt: time.Now().UTC()}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if err := ts.store.CreateTokenRow(row); err != nil {
		return "", err
	}
	ts.toks = append(ts.toks, tokenRecordFromRow(row))
	return raw, nil
}

// Revoke removes every token with the given name.
func (ts *TokenStore) Revoke(name string) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if err := ts.store.DeleteTokenRow(name); err != nil {
		return err
	}
	kept := ts.toks[:0]
	for _, t := range ts.toks {
		if t.Name != name {
			kept = append(kept, t)
		}
	}
	ts.toks = kept
	return nil
}

// List returns metadata (not the tokens themselves) for display purposes.
func (ts *TokenStore) List() []TokenRecord {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	out := make([]TokenRecord, len(ts.toks))
	copy(out, ts.toks)
	return out
}
