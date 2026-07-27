package api

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/swenske/gotochanger/internal/config"
)

// ErrBootstrapRequired is returned by Authenticate when the built-in Admin
// account has not had its password set yet.
var ErrBootstrapRequired = errors.New("initial admin password has not been set")

// ErrInvalidCredentials is returned by Authenticate/ChangePassword when a
// username/password pair does not match. It is intentionally generic (does
// not distinguish "unknown user" from "wrong password") to avoid leaking
// account existence (OWASP A07).
var ErrInvalidCredentials = errors.New("invalid username or password")

// ErrLastAdmin is returned when an operation would leave the system with no
// remaining administrator account.
var ErrLastAdmin = errors.New("cannot remove or demote the last remaining admin")

// ErrAccountLocked is returned by Authenticate when too many failed
// attempts have been made against an account recently.
var ErrAccountLocked = errors.New("account temporarily locked after too many failed login attempts")

// errUserNotFound is returned by lookups for an unknown username.
var errUserNotFound = errors.New("user not found")

// usernameRE restricts usernames to safe, unambiguous characters.
var usernameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)

const (
	maxFailedLogins  = 5
	loginLockoutTime = 15 * time.Minute
)

// DefaultAdminUsername is the built-in account created on first run. Its
// password must be set via SetInitialAdminPassword before it can log in.
const DefaultAdminUsername = "Admin"

// UserRecordStore is everything UserStore needs from the SQLite-backed
// topology store (internal/store/auth.go) to manage user accounts.
// Defined as an interface, like TopologyStore/BackupStore, so this package
// doesn't need to import internal/store directly; *store.Store satisfies
// it.
type UserRecordStore interface {
	ListUserRows() ([]config.UserRow, error)
	GetUserRow(username string) (config.UserRow, bool, error)
	CreateUserRow(config.UserRow) error
	UpdateUserRow(username string, row config.UserRow) error
	DeleteUserRow(username string) error
}

// UserInfo is the API/UI-facing view of a user account: it never includes
// the password hash.
type UserInfo struct {
	Username           string    `json:"username"`
	Role               Role      `json:"role"`
	MustChangePassword bool      `json:"must_change_password"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func userInfoFromRow(row config.UserRow) UserInfo {
	return UserInfo{
		Username:           row.Username,
		Role:               Role(row.Role),
		MustChangePassword: row.MustChangePassword,
		CreatedAt:          row.CreatedAt,
		UpdatedAt:          row.UpdatedAt,
	}
}

// UserStore manages local user accounts, persisted via UserRecordStore
// (SQLite - see internal/store/auth.go; this used to be a JSON file,
// migrated forward by Store.MigrateUsersAndTokensFromJSON). Reads go
// straight to the store on every call rather than through an in-memory
// cache - unlike TokenStore.Verify (hit on every API request), account
// lookups here are already gated by a deliberately-slow PBKDF2 hash
// comparison, so one extra SQLite round trip per call is negligible. Only
// the failed-login rate-limiting counters stay in-memory, same as before.
type UserStore struct {
	mu     sync.RWMutex
	store  UserRecordStore
	failed map[string]*loginFailure
}

type loginFailure struct {
	count       int
	lockedUntil time.Time
}

// LoadOrBootstrapUserStore wraps store, seeding a single Admin account
// (password not yet set) if the users table is empty.
func LoadOrBootstrapUserStore(store UserRecordStore) (*UserStore, error) {
	us := &UserStore{store: store, failed: map[string]*loginFailure{}}
	rows, err := store.ListUserRows()
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	if len(rows) > 0 {
		return us, nil
	}
	now := time.Now().UTC()
	admin := config.UserRow{Username: DefaultAdminUsername, Role: string(RoleAdmin), MustChangePassword: true, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateUserRow(admin); err != nil {
		return nil, err
	}
	return us, nil
}

// BootstrapRequired reports whether the default Admin account still has no
// password set.
func (us *UserStore) BootstrapRequired() bool {
	row, ok, err := us.store.GetUserRow(DefaultAdminUsername)
	if err != nil || !ok {
		return false
	}
	return row.PasswordHash == ""
}

// SetInitialAdminPassword sets the Admin account's password. It only
// succeeds while BootstrapRequired is true, preventing it from being used
// to reset an already-configured account.
func (us *UserStore) SetInitialAdminPassword(password string) error {
	us.mu.Lock()
	defer us.mu.Unlock()

	row, ok, err := us.store.GetUserRow(DefaultAdminUsername)
	if err != nil {
		return err
	}
	if !ok || row.PasswordHash != "" {
		return fmt.Errorf("initial admin password has already been set")
	}
	if err := ValidatePassword(password, row.Username); err != nil {
		return err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	row.PasswordHash = hash
	row.MustChangePassword = false
	row.UpdatedAt = time.Now().UTC()
	return us.store.UpdateUserRow(row.Username, row)
}

// Authenticate verifies a username/password pair and returns the matching
// user's public info. Accounts are temporarily locked after too many
// consecutive failed attempts (basic brute-force protection).
func (us *UserStore) Authenticate(username, password string) (UserInfo, error) {
	us.mu.Lock()
	defer us.mu.Unlock()

	key := strings.ToLower(username)
	if fl, ok := us.failed[key]; ok && time.Now().Before(fl.lockedUntil) {
		return UserInfo{}, ErrAccountLocked
	}

	row, ok, err := us.store.GetUserRow(username)
	if err != nil {
		return UserInfo{}, err
	}
	if !ok {
		// Still run a hash comparison against a dummy value so the response
		// timing does not reveal whether the account exists.
		verifyPassword(password, "pbkdf2-sha256$1$AA$AA")
		return UserInfo{}, ErrInvalidCredentials
	}
	if row.PasswordHash == "" {
		return UserInfo{}, ErrBootstrapRequired
	}
	if !verifyPassword(password, row.PasswordHash) {
		fl := us.failed[key]
		if fl == nil {
			fl = &loginFailure{}
			us.failed[key] = fl
		}
		fl.count++
		if fl.count >= maxFailedLogins {
			fl.lockedUntil = time.Now().Add(loginLockoutTime)
			fl.count = 0
		}
		return UserInfo{}, ErrInvalidCredentials
	}
	delete(us.failed, key)
	return userInfoFromRow(row), nil
}

// ChangePassword lets a logged-in user change their own password.
func (us *UserStore) ChangePassword(username, currentPassword, newPassword string) error {
	us.mu.Lock()
	defer us.mu.Unlock()

	row, ok, err := us.store.GetUserRow(username)
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidCredentials
	}
	if row.PasswordHash == "" || !verifyPassword(currentPassword, row.PasswordHash) {
		return ErrInvalidCredentials
	}
	if err := ValidatePassword(newPassword, row.Username); err != nil {
		return err
	}
	hash, err := hashPassword(newPassword)
	if err != nil {
		return err
	}
	row.PasswordHash = hash
	row.MustChangePassword = false
	row.UpdatedAt = time.Now().UTC()
	return us.store.UpdateUserRow(row.Username, row)
}

// CreateUser is used by admins to provision a new account with an initial
// password. The new user is required to change it at next login.
func (us *UserStore) CreateUser(username string, role Role, initialPassword string) (UserInfo, error) {
	us.mu.Lock()
	defer us.mu.Unlock()

	if !usernameRE.MatchString(username) {
		return UserInfo{}, fmt.Errorf("invalid username %q: must be 1-32 letters, digits, dots, dashes or underscores", username)
	}
	if _, exists, err := us.store.GetUserRow(username); err != nil {
		return UserInfo{}, err
	} else if exists {
		return UserInfo{}, fmt.Errorf("user %q already exists", username)
	}
	if err := ValidatePassword(initialPassword, username); err != nil {
		return UserInfo{}, err
	}
	hash, err := hashPassword(initialPassword)
	if err != nil {
		return UserInfo{}, err
	}
	now := time.Now().UTC()
	row := config.UserRow{Username: username, PasswordHash: hash, Role: string(role), MustChangePassword: true, CreatedAt: now, UpdatedAt: now}
	if err := us.store.CreateUserRow(row); err != nil {
		return UserInfo{}, err
	}
	return userInfoFromRow(row), nil
}

// AdminResetPassword lets an admin set a new password for another account
// without knowing the current one; the target user must change it at next
// login.
func (us *UserStore) AdminResetPassword(username, newPassword string) error {
	us.mu.Lock()
	defer us.mu.Unlock()

	row, ok, err := us.store.GetUserRow(username)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("user %q: %w", username, errUserNotFound)
	}
	if err := ValidatePassword(newPassword, row.Username); err != nil {
		return err
	}
	hash, err := hashPassword(newPassword)
	if err != nil {
		return err
	}
	row.PasswordHash = hash
	row.MustChangePassword = true
	row.UpdatedAt = time.Now().UTC()
	return us.store.UpdateUserRow(row.Username, row)
}

// SetRole changes a user's role, refusing to demote the last remaining
// admin account.
func (us *UserStore) SetRole(username string, role Role) error {
	us.mu.Lock()
	defer us.mu.Unlock()

	row, ok, err := us.store.GetUserRow(username)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("user %q: %w", username, errUserNotFound)
	}
	if row.Role == string(RoleAdmin) && role != RoleAdmin {
		n, err := us.countAdminsLocked()
		if err != nil {
			return err
		}
		if n <= 1 {
			return ErrLastAdmin
		}
	}
	row.Role = string(role)
	row.UpdatedAt = time.Now().UTC()
	return us.store.UpdateUserRow(row.Username, row)
}

// DeleteUser removes an account, refusing to delete the last remaining
// admin account.
func (us *UserStore) DeleteUser(username string) error {
	us.mu.Lock()
	defer us.mu.Unlock()

	row, ok, err := us.store.GetUserRow(username)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("user %q: %w", username, errUserNotFound)
	}
	if row.Role == string(RoleAdmin) {
		n, err := us.countAdminsLocked()
		if err != nil {
			return err
		}
		if n <= 1 {
			return ErrLastAdmin
		}
	}
	return us.store.DeleteUserRow(username)
}

// List returns every account's public info, sorted by username.
func (us *UserStore) List() []UserInfo {
	us.mu.RLock()
	defer us.mu.RUnlock()
	rows, err := us.store.ListUserRows()
	if err != nil {
		return nil
	}
	out := make([]UserInfo, 0, len(rows))
	for _, row := range rows {
		out = append(out, userInfoFromRow(row))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out
}

// Get returns a single account's public info and role, used by session
// refresh logic to pick up role changes without re-authenticating.
func (us *UserStore) Get(username string) (UserInfo, bool) {
	us.mu.RLock()
	defer us.mu.RUnlock()
	row, ok, err := us.store.GetUserRow(username)
	if err != nil || !ok {
		return UserInfo{}, false
	}
	return userInfoFromRow(row), true
}

// countAdminsLocked assumes the caller already holds us.mu.
func (us *UserStore) countAdminsLocked() (int, error) {
	rows, err := us.store.ListUserRows()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, row := range rows {
		if row.Role == string(RoleAdmin) {
			n++
		}
	}
	return n, nil
}
