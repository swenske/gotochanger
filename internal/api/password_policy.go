package api

import (
	"fmt"
	"strings"
	"unicode"
)

const (
	minPasswordLength = 12
	maxPasswordLength = 128
)

// commonPasswords is a small blocklist of extremely common/breached
// passwords, checked case-insensitively. It is intentionally short: the
// bulk of the policy's strength comes from length + character diversity
// requirements below (NIST SP 800-63B favors length over forced rotation
// or complex composition rules, but a composition floor plus a blocklist
// check is still a reasonable defense-in-depth measure here).
var commonPasswords = map[string]bool{
	"password": true, "password123": true, "123456789012": true,
	"qwertyuiopas": true, "letmeinletme": true, "administrator": true,
	"changeme1234": true, "welcome123456": true, "iloveyou12345": true,
	"admin12345678": true, "passw0rd12345": true, "trustno1trust": true,
}

// ValidatePassword enforces the password policy: minimum length, a mix of
// character classes, and rejection of the account's own username or a
// common/breached password.
func ValidatePassword(password, username string) error {
	if len(password) < minPasswordLength {
		return fmt.Errorf("password must be at least %d characters long", minPasswordLength)
	}
	if len(password) > maxPasswordLength {
		return fmt.Errorf("password must be at most %d characters long", maxPasswordLength)
	}
	if username != "" && strings.EqualFold(password, username) {
		return fmt.Errorf("password must not be the same as the username")
	}

	var hasLower, hasUpper, hasDigit, hasSpecial bool
	for _, r := range password {
		switch {
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsUpper(r):
			hasUpper = true
		case unicode.IsDigit(r):
			hasDigit = true
		case unicode.IsSpace(r):
			// whitespace does not count toward any class but is allowed
		default:
			hasSpecial = true
		}
	}
	classes := 0
	for _, ok := range []bool{hasLower, hasUpper, hasDigit, hasSpecial} {
		if ok {
			classes++
		}
	}
	if classes < 3 {
		return fmt.Errorf("password must contain at least 3 of: lowercase letters, uppercase letters, digits, special characters")
	}

	if commonPasswords[strings.ToLower(password)] {
		return fmt.Errorf("password is too common, choose a different one")
	}
	return nil
}
