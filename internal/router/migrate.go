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

// legacyRouterLayout is what a TypeScript-era control home laid the shared
// router out as, before protocol 4 moved it to a machine-wide state
// directory (#12, #13): src/layout.ts's own routerDir and everything under
// it. layout.Control no longer carries these — nothing else reads them — so
// this is the one place that writes down what that layout was.
type legacyRouterLayout struct {
	RouterDir    string
	ComposeFile  string
	ProtocolFile string
	DynamicDir   string
	LockPath     string
}

// legacyRouter derives a TypeScript-era control home's router layout.
func legacyRouter(controlHome string) legacyRouterLayout {
	routerDir := filepath.Join(controlHome, "router")
	return legacyRouterLayout{
		RouterDir:    routerDir,
		ComposeFile:  filepath.Join(routerDir, "compose.yaml"),
		ProtocolFile: filepath.Join(routerDir, "protocol"),
		DynamicDir:   filepath.Join(routerDir, "dynamic"),
		LockPath:     filepath.Join(controlHome, "locks", "router"),
	}
}

// MigrateLegacyState moves what this control home left under <home>/router
// into the shared router's own state directory, then removes both the old
// router directory and the old router lock file, so this control home stops
// carrying a copy of state that is now machine-wide. Idempotent: once a
// control home has been migrated, later calls find nothing left to do.
//
// Only ever touches a directory this tool itself created as a control home:
// isBerthControlHome requires files.ControlHomeMarker as a plain file plus a
// pass of files.AssertAdoptableControlHome, and controlHome is skipped
// silently otherwise. Without this, an empty BERTH_HOME resolves to the
// current directory (layout.ResolveControlHome), and any directory that
// happens to hold a "router" subdirectory of its own — a checkout of this
// very project, say — would have it deleted.
//
// Called under the router's lock (the same one Ensure just held), once
// Ensure has verified the router: two homes migrating at once, or two `up`
// runs racing from the same home, are then never actually concurrent. Each
// entry is still handled defensively even so — tolerating one already gone,
// and never overwriting one already at the destination — since holding the
// lock is what makes migration safe, not what this function assumes about
// its caller. A failure here should not cost the caller a router that just
// came up clean, so callers are expected to log it and carry on rather than
// fail the command.
func MigrateLegacyState(controlHome string, state layout.RouterLayout, log logger.Logger) error {
	if !isBerthControlHome(controlHome) {
		return nil
	}
	legacy := legacyRouter(controlHome)

	entries, err := os.ReadDir(legacy.DynamicDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Every entry, files and directories alike: Traefik's file provider
	// reads subdirectories too, so one can be real configuration, not just
	// scaffolding safe to discard with the directory that held it.
	var candidates []string
	for _, entry := range entries {
		candidates = append(candidates, entry.Name())
	}

	moved := 0
	kept := false
	if len(candidates) > 0 {
		if err := os.MkdirAll(state.DynamicDir, 0o755); err != nil {
			return err
		}
		for _, name := range candidates {
			source := filepath.Join(legacy.DynamicDir, name)
			destination := filepath.Join(state.DynamicDir, name)
			// Never overwrite: a name already at the shared destination —
			// another control home's entry of the same name, migrated
			// first — is left where it is, on both sides, for a person to
			// resolve; silently replacing it would lose whichever one lost the race.
			if _, err := os.Lstat(destination); err == nil {
				kept = true
				log.Warn(fmt.Sprintf(
					"Kept %s: the shared router already has an entry at %s. Resolve this by hand, then remove %s.",
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
			"Moved %d item(s) from this control home's router configuration (%s) to the shared router at %s.",
			moved, legacy.DynamicDir, state.DynamicDir))
	}
	if kept {
		// Migration is not finished: removing the legacy directory now
		// would destroy the entry just kept above along with it.
		return nil
	}

	if err := files.RemoveWithin(controlHome, legacy.RouterDir); err != nil {
		return err
	}
	if err := os.Remove(legacy.LockPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// isBerthControlHome reports whether controlHome is one this tool created:
// files.ControlHomeMarker exists there as a plain, non-symlinked file, and
// the directory itself still passes files.AssertAdoptableControlHome. Both
// are required — a marker alone does not prove the directory has not been
// repurposed since — and any failure to confirm either (including one this
// function cannot explain, such as a permission error) is treated the same
// as "no": ownership must be proven, never assumed, before anything here is
// ever deleted.
func isBerthControlHome(controlHome string) bool {
	info, err := os.Lstat(filepath.Join(controlHome, files.ControlHomeMarker))
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return files.AssertAdoptableControlHome(controlHome) == nil
}
