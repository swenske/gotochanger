package store

import (
	"fmt"

	"github.com/swenske/gotochanger/internal/instanceid"
)

// instanceIDSettingKey is deliberately prefixed with "telemetry_" rather
// than a bare "instance_id" - "instance" already has a different,
// established meaning in this codebase (a gotochanger-tcmud kernel-mode
// instance, keyed by logical library name), and this value's only
// purpose is telemetry.
const instanceIDSettingKey = "telemetry_instance_id"

// InstanceID returns this install's stable, anonymous instance ID,
// generating and persisting one via instanceid.Generate on first call.
// SeedDefaults already does this once at every startup, so in practice
// every other caller just gets a plain read of the persisted value.
func (s *Store) InstanceID() (string, error) {
	if v, ok, err := s.GetSetting(instanceIDSettingKey); err != nil {
		return "", err
	} else if ok {
		return v, nil
	}
	id, err := instanceid.Generate()
	if err != nil {
		return "", fmt.Errorf("generate instance id: %w", err)
	}
	if err := s.SetSetting(instanceIDSettingKey, id); err != nil {
		return "", err
	}
	return id, nil
}
