package store

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ResetToFactory replaces the live database with a brand new, empty one -
// same schema, zero rows - so the next daemon startup (SeedDefaults, etc.)
// rebuilds the VTL exactly as it would for a genuinely fresh install: empty
// topology, no users/tokens (bootstrap required again), wizard not
// completed. Rather than duplicating Restore's swap/reopen/re-init logic, a
// reset is simply "restore from an empty database": build a scratch store
// at a temp path next to the live one, Open() it (creates every schema,
// all empty), Close() it, then hand it to the already-hardened Restore.
func (s *Store) ResetToFactory() error {
	dir := filepath.Dir(s.path)
	scratchPath := filepath.Join(dir, fmt.Sprintf(".reset-empty-%d.db", time.Now().UnixNano()))

	scratch := New(scratchPath)
	if err := scratch.Open(); err != nil {
		return fmt.Errorf("build empty database: %w", err)
	}
	if err := scratch.Close(); err != nil {
		_ = os.Remove(scratchPath)
		return fmt.Errorf("close empty database: %w", err)
	}

	if err := s.Restore(scratchPath); err != nil {
		_ = os.Remove(scratchPath)
		return fmt.Errorf("install empty database: %w", err)
	}
	return nil
}
