package api

import "github.com/swenske/gotochanger/internal/secrethash"

// Password hashing is implemented by internal/secrethash (PBKDF2-HMAC-
// SHA256, RFC 8018) - relocated there so internal/library can hash/verify
// PIN codes with the exact same primitive without creating an import cycle
// (internal/library must not import internal/api). These wrappers keep the
// existing call sites in this package unchanged.

// hashPassword returns an encoded "pbkdf2-sha256$iterations$salt$hash"
// string (base64 raw-standard encoding) suitable for storage.
func hashPassword(password string) (string, error) {
	return secrethash.Hash(password)
}

// verifyPassword reports whether password matches the previously stored
// encoded hash, using a constant-time comparison.
func verifyPassword(password, encoded string) bool {
	return secrethash.Verify(password, encoded)
}
