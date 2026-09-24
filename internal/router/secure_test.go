package router_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/router"
)

// TestSecureStateRefusesAPlantedSymlink pins the fix to secureState's
// check-then-mutate ordering: a symlinked state directory must be refused
// before anything is created inside, or chmod'd on, whatever it points to.
func TestSecureStateRefusesAPlantedSymlink(t *testing.T) {
	t.Run("refuses a symlinked state directory without creating or changing anything in its target", func(t *testing.T) {
		parent := registrytest.TempDir(t)
		target := registrytest.TempDir(t)
		if err := os.Chmod(target, 0o750); err != nil {
			t.Fatal(err)
		}
		stateDir := filepath.Join(parent, "router")
		if err := os.Symlink(target, stateDir); err != nil {
			t.Fatal(err)
		}
		state := newState(stateDir)

		fake := newDockerFake()
		fake.mu.Lock()
		fake.ids = map[string]string{} // no evidence to gather before write() is reached
		fake.mu.Unlock()

		svc, err := router.New(router.Options{State: state, Port: 80, Runner: fake, Logger: &logger.Recording{}})
		if err != nil {
			t.Fatal(err)
		}

		if err := svc.Ensure(context.Background()); err == nil {
			t.Fatal("expected Ensure to refuse a symlinked state directory")
		}

		entries, err := os.ReadDir(target)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("expected nothing created in the symlink's target, got %v", entries)
		}
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o750 {
			t.Fatalf("expected the target directory's mode to stay 0750, got %o", info.Mode().Perm())
		}
	})
}
