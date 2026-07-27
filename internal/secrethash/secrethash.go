// Package secrethash hashes and verifies short secrets (account passwords,
// PIN codes) with PBKDF2-HMAC-SHA256 (RFC 8018), built entirely from the
// standard library. This avoids adding golang.org/x/crypto (whose recent
// releases require a newer Go toolchain than Debian trixie ships, which
// would break offline package builds) while still following current OWASP
// guidance for PBKDF2-SHA256 iteration counts.
//
// It has no dependency on any other internal package (same "leaf package"
// convention as internal/barcode), so it can be imported by both
// internal/api (account passwords) and internal/library (the magazine/
// mailbox PIN codes in "Robotics fault simulation"-style gated state)
// without creating an import cycle - internal/library must not import
// internal/api, since internal/api already imports internal/library.
package secrethash

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

const (
	iterations = 600_000
	saltLen    = 16
	keyLen     = 32
)

// Hash returns an encoded "pbkdf2-sha256$iterations$salt$hash" string
// (base64 raw-standard encoding) suitable for storage.
func Hash(secret string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := pbkdf2HMACSHA256([]byte(secret), salt, iterations, keyLen)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s",
		iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// Verify reports whether secret matches a previously stored encoded hash
// (see Hash), using a constant-time comparison. Returns false, not an
// error, on any malformed input - a safe default-deny.
func Verify(secret, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iters, err := strconv.Atoi(parts[1])
	if err != nil || iters <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got := pbkdf2HMACSHA256([]byte(secret), salt, iters, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// pbkdf2HMACSHA256 implements PBKDF2 (RFC 8018) with HMAC-SHA256 as the
// pseudorandom function.
func pbkdf2HMACSHA256(password, salt []byte, iterations, keyLen int) []byte {
	hLen := sha256.Size
	numBlocks := (keyLen + hLen - 1) / hLen
	dk := make([]byte, 0, numBlocks*hLen)

	for block := 1; block <= numBlocks; block++ {
		mac := hmac.New(sha256.New, password)
		mac.Write(salt)
		var be [4]byte
		binary.BigEndian.PutUint32(be[:], uint32(block))
		mac.Write(be[:])
		u := mac.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)

		for i := 1; i < iterations; i++ {
			mac := hmac.New(sha256.New, password)
			mac.Write(u)
			u = mac.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}
