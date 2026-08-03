// Package instanceid derives a stable, anonymous, per-installation
// identifier for a future telemetry feature to report, so distinct
// installs can be counted without any personally-identifying data.
//
// It has no dependency on any other internal package (same "leaf
// package" convention as internal/secrethash, internal/barcode).
package instanceid

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// pepper is a fixed, source-committed constant mixed into the HMAC so
// the reported ID never doubles as, or trivially maps back to, the real
// OS-level identifier if telemetry logs are ever exposed. This is
// hygiene, not secrecy - anyone who can read /etc/machine-id already has
// local access.
const pepper = "gotochanger-instance-id-v1"

// hardwareIDPaths and dockerEnvPath are vars, not consts, so tests can
// point them at a fake path instead of the real filesystem - same idiom
// internal/api/kernel_mode.go uses for kernelTCMUDPath etc. dockerEnvPath
// is duplicated from that idiom rather than imported: internal/store
// (this package's only caller) must never depend on internal/api.
var (
	hardwareIDPaths = []string{
		"/etc/machine-id",
		"/sys/class/dmi/id/product_uuid",
	}
	dockerEnvPath = "/.dockerenv"
)

// knownPlaceholders are well-documented non-unique sentinel values -
// systemd's own "not yet initialized" marker, and the conventional
// all-zeros forms - not an attempt to enumerate every vendor-baked
// placeholder, which varies per base image and isn't reliably
// enumerable; the random fallback in Generate is the actual safety net
// for everything else.
var knownPlaceholders = map[string]bool{
	"uninitialized":                        true,
	"00000000000000000000000000000000":     true,
	"00000000-0000-0000-0000-000000000000": true,
}

// Generate derives a new instance ID: an HMAC-SHA256 hash (hex-encoded)
// of the first safely-readable hardware identifier, or - if none is
// available - a purely random ID via crypto/rand, the same primitive
// internal/api's generateToken/randomID use. The raw hardware value is
// never stored, logged, or returned. Meant to be called at most once per
// installation's lifetime; the caller is responsible for persisting the
// result.
func Generate() (string, error) {
	if id, ok := hardwareID(); ok {
		return id, nil
	}
	return randomID()
}

// hardwareID returns a derived ID from the first safely-readable,
// non-placeholder source in hardwareIDPaths, skipping all of them
// entirely when running inside a Docker container - some base images
// bake in a shared /etc/machine-id that every container started from
// that image layer would otherwise inherit, silently colliding across
// distinct installs.
func hardwareID() (string, bool) {
	if _, err := os.Stat(dockerEnvPath); err == nil {
		return "", false
	}
	for _, path := range hardwareIDPaths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(raw))
		if v == "" || knownPlaceholders[v] {
			continue
		}
		return hashHardwareValue(v), true
	}
	return "", false
}

func hashHardwareValue(v string) string {
	mac := hmac.New(sha256.New, []byte(pepper))
	mac.Write([]byte(v))
	return hex.EncodeToString(mac.Sum(nil))
}

func randomID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random instance id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
