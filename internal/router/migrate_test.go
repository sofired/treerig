package router_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/router"
)

// newRouterState is a RouterState rooted at root, as if root were $HOME.
func newRouterState(t *testing.T, root string) layout.RouterLayout {
	t.Helper()
	getenv := func(key string) (string, bool) {
		if key == "HOME" {
			return root, true
		}
		return "", false
	}
	state, err := layout.RouterState(getenv)
	if err != nil {
		t.Fatalf("RouterState: %v", err)
	}
	return state
}

// markAsControlHome stages the marker MigrateLegacyState requires before it
// will touch controlHome, as a real one already carries — from an earlier
// establishControlHome, TypeScript's or Go's — by the time a router ever
// runs against it.
func markAsControlHome(t *testing.T, controlHome string) {
	t.Helper()
	if err := os.MkdirAll(controlHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(controlHome, files.ControlHomeMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateLegacyState(t *testing.T) {
	t.Run("moves dynamic fragments, warns, then removes the old router directory and lock", func(t *testing.T) {
		controlHome := registrytest.TempDir(t)
		markAsControlHome(t, controlHome)
		state := newRouterState(t, registrytest.TempDir(t))

		legacyDynamic := filepath.Join(controlHome, "router", "dynamic")
		if err := os.MkdirAll(legacyDynamic, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(legacyDynamic, "worktree.yaml"), []byte("routers: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(controlHome, "router", "compose.yaml"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		legacyLockDir := filepath.Join(controlHome, "locks")
		if err := os.MkdirAll(legacyLockDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(legacyLockDir, "router"), nil, 0o600); err != nil {
			t.Fatal(err)
		}

		rec := &logger.Recording{}
		if err := router.MigrateLegacyState(controlHome, state, rec); err != nil {
			t.Fatalf("MigrateLegacyState: %v", err)
		}

		migrated := filepath.Join(state.DynamicDir, "worktree.yaml")
		if _, err := os.Stat(migrated); err != nil {
			t.Errorf("expected the fragment to have moved to %s: %v", migrated, err)
		}
		if _, err := os.Stat(filepath.Join(controlHome, "router")); !os.IsNotExist(err) {
			t.Errorf("expected the legacy router directory to be gone, stat err = %v", err)
		}
		if _, err := os.Stat(filepath.Join(legacyLockDir, "router")); !os.IsNotExist(err) {
			t.Errorf("expected the legacy router lock file to be gone, stat err = %v", err)
		}
		lines, _ := rec.Snapshot()
		found := false
		for _, line := range lines {
			if line == "warn: Moved 1 item(s) from this control home's router configuration ("+legacyDynamic+") to the shared router at "+state.DynamicDir+"." {
				found = true
			}
		}
		if !found {
			t.Errorf("expected a warning naming the move, got %v", lines)
		}
	})

	t.Run("is idempotent when there is nothing left under this control home", func(t *testing.T) {
		controlHome := registrytest.TempDir(t)
		markAsControlHome(t, controlHome)
		state := newRouterState(t, registrytest.TempDir(t))
		rec := &logger.Recording{}

		if err := router.MigrateLegacyState(controlHome, state, rec); err != nil {
			t.Fatalf("MigrateLegacyState: %v", err)
		}
		lines, _ := rec.Snapshot()
		if len(lines) != 0 {
			t.Errorf("expected no warnings when nothing needed migrating, got %v", lines)
		}
	})

	t.Run("removes the legacy router directory even when it holds only state that is not the dynamic directory", func(t *testing.T) {
		controlHome := registrytest.TempDir(t)
		markAsControlHome(t, controlHome)
		state := newRouterState(t, registrytest.TempDir(t))
		if err := os.MkdirAll(filepath.Join(controlHome, "router", "index"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(controlHome, "router", "protocol"), []byte("3\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		rec := &logger.Recording{}
		if err := router.MigrateLegacyState(controlHome, state, rec); err != nil {
			t.Fatalf("MigrateLegacyState: %v", err)
		}
		if _, err := os.Stat(filepath.Join(controlHome, "router")); !os.IsNotExist(err) {
			t.Errorf("expected the legacy router directory to be gone, stat err = %v", err)
		}
	})

	t.Run("never overwrites a same-named file already at the shared destination", func(t *testing.T) {
		controlHome := registrytest.TempDir(t)
		markAsControlHome(t, controlHome)
		state := newRouterState(t, registrytest.TempDir(t))

		legacyDynamic := filepath.Join(controlHome, "router", "dynamic")
		if err := os.MkdirAll(legacyDynamic, 0o755); err != nil {
			t.Fatal(err)
		}
		source := filepath.Join(legacyDynamic, "worktree.yaml")
		if err := os.WriteFile(source, []byte("routers: {mine: true}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// As if another control home's file of the same name won the race already.
		if err := os.MkdirAll(state.DynamicDir, 0o755); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(state.DynamicDir, "worktree.yaml")
		if err := os.WriteFile(destination, []byte("routers: {theirs: true}\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		rec := &logger.Recording{}
		if err := router.MigrateLegacyState(controlHome, state, rec); err != nil {
			t.Fatalf("MigrateLegacyState: %v", err)
		}

		kept, err := os.ReadFile(destination)
		if err != nil {
			t.Fatal(err)
		}
		if string(kept) != "routers: {theirs: true}\n" {
			t.Errorf("expected the destination to survive untouched, got %q", kept)
		}
		survived, err := os.ReadFile(source)
		if err != nil {
			t.Fatalf("expected the source to survive rather than be lost, got: %v", err)
		}
		if string(survived) != "routers: {mine: true}\n" {
			t.Errorf("expected the source's own contents to survive, got %q", survived)
		}
		// Migration is unfinished (a collision needs a person), so the
		// legacy directory must not have been removed out from under it.
		if _, err := os.Stat(filepath.Join(controlHome, "router")); err != nil {
			t.Errorf("expected the legacy router directory to survive, stat err = %v", err)
		}
		lines, _ := rec.Snapshot()
		if !containsSubstring(lines, "Kept "+source) || !containsSubstring(lines, destination) {
			t.Errorf("expected a warning naming both paths, got %v", lines)
		}
	})

	t.Run("moves a real subdirectory of dynamic/ instead of deleting it with the legacy directory", func(t *testing.T) {
		controlHome := registrytest.TempDir(t)
		markAsControlHome(t, controlHome)
		state := newRouterState(t, registrytest.TempDir(t))

		legacyDynamic := filepath.Join(controlHome, "router", "dynamic")
		nested := filepath.Join(legacyDynamic, "certs")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(nested, "worktree.pem"), []byte("real configuration\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		rec := &logger.Recording{}
		if err := router.MigrateLegacyState(controlHome, state, rec); err != nil {
			t.Fatalf("MigrateLegacyState: %v", err)
		}

		moved := filepath.Join(state.DynamicDir, "certs", "worktree.pem")
		contents, err := os.ReadFile(moved)
		if err != nil {
			t.Fatalf("expected the subdirectory to have moved to %s: %v", moved, err)
		}
		if string(contents) != "real configuration\n" {
			t.Errorf("expected the subdirectory's own contents to survive, got %q", contents)
		}
		if _, err := os.Stat(filepath.Join(controlHome, "router")); !os.IsNotExist(err) {
			t.Errorf("expected the legacy router directory to be gone, stat err = %v", err)
		}
	})

	t.Run("keeps a colliding subdirectory rather than losing it, the same as a file", func(t *testing.T) {
		controlHome := registrytest.TempDir(t)
		markAsControlHome(t, controlHome)
		state := newRouterState(t, registrytest.TempDir(t))

		legacyDynamic := filepath.Join(controlHome, "router", "dynamic")
		source := filepath.Join(legacyDynamic, "certs")
		if err := os.MkdirAll(source, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, "worktree.pem"), []byte("mine\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(state.DynamicDir, "certs")
		if err := os.MkdirAll(destination, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(destination, "worktree.pem"), []byte("theirs\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		rec := &logger.Recording{}
		if err := router.MigrateLegacyState(controlHome, state, rec); err != nil {
			t.Fatalf("MigrateLegacyState: %v", err)
		}

		if contents, err := os.ReadFile(filepath.Join(destination, "worktree.pem")); err != nil || string(contents) != "theirs\n" {
			t.Errorf("expected the destination subdirectory to survive untouched, got %q, %v", contents, err)
		}
		if contents, err := os.ReadFile(filepath.Join(source, "worktree.pem")); err != nil || string(contents) != "mine\n" {
			t.Errorf("expected the source subdirectory to survive rather than be lost, got %q, %v", contents, err)
		}
		if _, err := os.Stat(filepath.Join(controlHome, "router")); err != nil {
			t.Errorf("expected the legacy router directory to survive an unresolved collision, stat err = %v", err)
		}
	})

	t.Run("skips a directory berth did not create, leaving its router subdirectory untouched", func(t *testing.T) {
		// A project directory that simply happens to have a "router"
		// subdirectory of its own — never marked as a control home. See
		// TestRouterUp in internal/commands for the same scenario reached
		// the way it happens in production: BERTH_HOME set empty, resolving
		// to the current directory (layout.ResolveControlHome).
		project := registrytest.TempDir(t)
		importantDir := filepath.Join(project, "router", "dynamic")
		if err := os.MkdirAll(importantDir, 0o755); err != nil {
			t.Fatal(err)
		}
		importantFile := filepath.Join(importantDir, "not-berths.yaml")
		if err := os.WriteFile(importantFile, []byte("someone else's file\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		state := newRouterState(t, registrytest.TempDir(t))
		rec := &logger.Recording{}

		if err := router.MigrateLegacyState(project, state, rec); err != nil {
			t.Fatalf("MigrateLegacyState: %v", err)
		}

		if _, err := os.Stat(importantFile); err != nil {
			t.Errorf("expected the project's own file to survive untouched, got: %v", err)
		}
		if _, err := os.Stat(filepath.Join(project, "router")); err != nil {
			t.Errorf("expected the project's router directory to still exist, got: %v", err)
		}
		lines, _ := rec.Snapshot()
		if len(lines) != 0 {
			t.Errorf("expected no warnings for a directory that was never adopted, got %v", lines)
		}
	})

	t.Run("skips a directory that has the marker but no longer passes AssertAdoptableControlHome", func(t *testing.T) {
		// The marker alone is not enough: AssertAdoptableControlHome also
		// refuses the home directory itself outright, regardless of a
		// marker — exactly the kind of directory a misconfigured BERTH_HOME
		// could resolve to.
		home := registrytest.TempDir(t)
		if err := os.WriteFile(filepath.Join(home, files.ControlHomeMarker), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		routerDir := filepath.Join(home, "router")
		if err := os.MkdirAll(filepath.Join(routerDir, "dynamic"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", home) // what files.AssertAdoptableControlHome itself reads

		state := newRouterState(t, registrytest.TempDir(t))
		rec := &logger.Recording{}
		if err := router.MigrateLegacyState(home, state, rec); err != nil {
			t.Fatalf("MigrateLegacyState: %v", err)
		}

		if _, err := os.Stat(routerDir); err != nil {
			t.Errorf("expected the home directory's router subdirectory to still exist, got: %v", err)
		}
	})
}
