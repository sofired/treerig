package router_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/router"
)

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

func TestMigrateLegacyState(t *testing.T) {
	t.Run("moves dynamic fragments, warns, then removes the old router directory and lock", func(t *testing.T) {
		controlHome := registrytest.TempDir(t)
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
			if line == "warn: Moved 1 file(s) from this control home's router configuration ("+legacyDynamic+") to the shared router at "+state.DynamicDir+"." {
				found = true
			}
		}
		if !found {
			t.Errorf("expected a warning naming the move, got %v", lines)
		}
	})

	t.Run("is idempotent when there is nothing left under this control home", func(t *testing.T) {
		controlHome := registrytest.TempDir(t)
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
}
