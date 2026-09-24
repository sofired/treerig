package router

// Migrating TypeScript-era state is new to the Go port (#12, #13): the
// TypeScript build always ran the router from inside whichever control home
// invoked it, so it never had a separate machine-wide location to migrate
// into. Go's does, so every control home that still carries the old
// per-home router state needs to hand it over once, the first time its new,
// machine-wide router has verified.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/logger"
)

// MigrateLegacyState moves what this control home left under <home>/router
// into the shared router's own state directory, then removes both the old
// router directory and the old router lock file, so this control home stops
// carrying a copy of state that is now machine-wide. Idempotent: once a
// control home has been migrated, later calls find nothing left to do.
//
// Called under the router's lock (the same one Ensure just held), once
// Ensure has verified the router: two homes migrating at once, or two `up`
// runs racing from the same home, are then never actually concurrent. Each
// file is still handled defensively even so — tolerating one already gone,
// and never overwriting one already at the destination — since holding the
// lock is what makes migration safe, not what this function assumes about
// its caller. A failure here should not cost the caller a router that just
// came up clean, so callers are expected to log it and carry on rather than
// fail the command.
func MigrateLegacyState(controlHome string, state layout.RouterLayout, log logger.Logger) error {
	legacyRouterDir := filepath.Join(controlHome, "router")
	legacyDynamicDir := filepath.Join(legacyRouterDir, "dynamic")

	entries, err := os.ReadDir(legacyDynamicDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var candidates []string
	for _, entry := range entries {
		if !entry.IsDir() {
			candidates = append(candidates, entry.Name())
		}
	}

	moved := 0
	kept := false
	if len(candidates) > 0 {
		if err := os.MkdirAll(state.DynamicDir, 0o755); err != nil {
			return err
		}
		for _, name := range candidates {
			source := filepath.Join(legacyDynamicDir, name)
			destination := filepath.Join(state.DynamicDir, name)
			// Never overwrite: a name already at the shared destination —
			// another control home's file of the same name, migrated
			// first — is left where it is, on both sides, for a person to
			// resolve; silently replacing it would lose whichever one lost the race.
			if _, err := os.Lstat(destination); err == nil {
				kept = true
				log.Warn(fmt.Sprintf(
					"Kept %s: the shared router already has a file at %s. Resolve this by hand, then remove %s.",
					source, destination, source))
				continue
			} else if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if err := os.Rename(source, destination); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					// Gone already — nothing left for this call to do for it.
					continue
				}
				return err
			}
			moved++
		}
	}
	if moved > 0 {
		log.Warn(fmt.Sprintf(
			"Moved %d file(s) from this control home's router configuration (%s) to the shared router at %s.",
			moved, legacyDynamicDir, state.DynamicDir))
	}
	if kept {
		// Migration is not finished: removing the legacy directory now
		// would destroy the file just kept above along with it.
		return nil
	}

	if err := files.RemoveWithin(controlHome, legacyRouterDir); err != nil {
		return err
	}
	legacyLock := filepath.Join(controlHome, "locks", "router")
	if err := os.Remove(legacyLock); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
