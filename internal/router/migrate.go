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
// Called once Ensure has verified the router; a failure here should not cost
// the caller a router that just came up clean, so callers are expected to
// log it and carry on rather than fail the command.
func MigrateLegacyState(controlHome string, state layout.RouterLayout, log logger.Logger) error {
	legacyRouterDir := filepath.Join(controlHome, "router")
	legacyDynamicDir := filepath.Join(legacyRouterDir, "dynamic")

	entries, err := os.ReadDir(legacyDynamicDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var moved []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		moved = append(moved, entry.Name())
	}
	if len(moved) > 0 {
		if err := os.MkdirAll(state.DynamicDir, 0o755); err != nil {
			return err
		}
		for _, name := range moved {
			if err := os.Rename(filepath.Join(legacyDynamicDir, name), filepath.Join(state.DynamicDir, name)); err != nil {
				return err
			}
		}
		log.Warn(fmt.Sprintf(
			"Moved %d file(s) from this control home's router configuration (%s) to the shared router at %s.",
			len(moved), legacyDynamicDir, state.DynamicDir))
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
