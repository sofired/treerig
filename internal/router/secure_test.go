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
// check-then-mutate ordering, and to its own place at the front of every
// entry point that can write into the state directory. A symlinked state
// directory must be refused before anything is created inside, or chmod'd
// on, whatever it points to — including the router lock (created the
// instant commands.routerHold's lock.New is acquired) and the stack files
// write() would otherwise write.
func TestSecureStateRefusesAPlantedSymlink(t *testing.T) {
	t.Run("refuses a symlinked state directory before the lock or the stack files ever touch its target", func(t *testing.T) {
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

		// The exported wrapper commands.RouterUp calls before ever taking
		// the router lock (see commands.routerHold): must refuse on its own.
		if err := router.SecureState(state); err == nil {
			t.Fatal("expected SecureState to refuse a symlinked state directory")
		}

		svc, err := router.New(router.Options{State: state, Port: 80, Runner: fake, Logger: &logger.Recording{}})
		if err != nil {
			t.Fatal(err)
		}
		// Ensure repeats the same check as its own first step, so write()
		// is never reached either — proven below by the target staying
		// empty despite it being ready to write.
		if err := svc.Ensure(context.Background()); err == nil {
			t.Fatal("expected Ensure to refuse a symlinked state directory")
		}

		entries, err := os.ReadDir(target)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("expected nothing created in the symlink's target — no lock, no holder record, no stack file — got %v", entries)
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
