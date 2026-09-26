package registry_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/registry"
)

// claimedHome is a control home berth created, as up leaves one: writes refuse any other.
func claimedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	// A distinct temp directory, nowhere near home: AssertAdoptableControlHome's
	// own home-family checks need something to compare against, never
	// exercised by this fixture.
	if err := files.SecureControlDirectories(layout.ControlLayout(home), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	return home
}

// testUserHome is a fixed, harmless placeholder: none of this file's fixture
// control homes are ever near it, so AssertAdoptableControlHome's own
// home-family checks never actually trigger from it — it only needs to be
// non-empty (#80: an empty userHome resolves as the current directory,
// exactly the ambiguity a real userHome always avoids).
var testUserHome = filepath.Join(os.TempDir(), "berth-test-user-home-does-not-exist")

var testWriter = registry.Writer{Logger: logger.Silent{}, Invocation: "berth worktree remove", UserHome: testUserHome}

// requireRegistryFree proves the registry lock was released: another holder takes it at once.
func requireRegistryFree(t *testing.T, controlHome string) {
	t.Helper()
	probe := lock.New(lock.RegistryScope(controlHome), lock.Options{Timeout: time.Second, Retry: 10 * time.Millisecond, Logger: logger.Silent{}})
	if _, err := probe.Acquire(context.Background(), "probe"); err != nil {
		t.Fatalf("the registry lock was left held: %v", err)
	}
	if err := probe.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreWrites(t *testing.T) {
	t.Run("marks a completed registration pending, keeping every other field and the key order", func(t *testing.T) {
		controlHome := claimedHome(t)
		writeMinimalRecord(t, controlHome, repoA, wtA, "feature-login", acmeDomain, "complete")
		store := registry.NewStore(controlHome, repoA)

		if err := store.MarkPending(context.Background(), wtA, "feature-login", testWriter); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(store.RecordPath(wtA))
		if err != nil {
			t.Fatal(err)
		}
		want := `{
  "version": 1,
  "repositoryId": "` + repoA + `",
  "worktreeId": "` + wtA + `",
  "slug": "feature-login",
  "domain": "acme.localhost",
  "initialization": "pending"
}
`
		if string(data) != want {
			t.Fatalf("record:\n%s\nwant:\n%s", data, want)
		}
		requireRegistryFree(t, controlHome)
	})

	t.Run("refuses to mark a registration another slug now owns, leaving it as it was", func(t *testing.T) {
		controlHome := claimedHome(t)
		writeMinimalRecord(t, controlHome, repoA, wtA, "renamed-since", acmeDomain, "complete")
		store := registry.NewStore(controlHome, repoA)
		before, _ := os.ReadFile(store.RecordPath(wtA))

		err := store.MarkPending(context.Background(), wtA, "feature-login", testWriter)
		requireErrorContainsRegistry(t, err, "Worktree feature-login no longer owns registration")
		after, _ := os.ReadFile(store.RecordPath(wtA))
		if string(after) != string(before) {
			t.Fatal("the record changed")
		}
		requireRegistryFree(t, controlHome)
	})

	t.Run("refuses to mark a registration that is missing", func(t *testing.T) {
		store := registry.NewStore(claimedHome(t), repoA)
		err := store.MarkPending(context.Background(), wtA, "feature-login", testWriter)
		requireErrorContainsRegistry(t, err, "Worktree registration is missing")
	})

	t.Run("removes a registration once, then reports there was none", func(t *testing.T) {
		controlHome := claimedHome(t)
		writeMinimalRecord(t, controlHome, repoA, wtA, "feature-login", acmeDomain, "complete")
		store := registry.NewStore(controlHome, repoA)

		removed, err := store.Remove(context.Background(), wtA, "feature-login", testWriter)
		if err != nil || !removed {
			t.Fatalf("first Remove: %v, %v", removed, err)
		}
		if _, err := os.Stat(store.RecordPath(wtA)); !os.IsNotExist(err) {
			t.Fatalf("record still there: %v", err)
		}
		removed, err = store.Remove(context.Background(), wtA, "feature-login", testWriter)
		if err != nil || removed {
			t.Fatalf("second Remove: %v, %v", removed, err)
		}
		requireRegistryFree(t, controlHome)
	})

	t.Run("retains a registration whose slug changed concurrently", func(t *testing.T) {
		controlHome := claimedHome(t)
		writeMinimalRecord(t, controlHome, repoA, wtA, "renamed-since", acmeDomain, "complete")
		store := registry.NewStore(controlHome, repoA)

		removed, err := store.Remove(context.Background(), wtA, "feature-login", testWriter)
		requireErrorContainsRegistry(t, err, "Worktree feature-login changed concurrently; its registration was retained.")
		if removed {
			t.Fatal("reported removed")
		}
		if _, err := os.Stat(store.RecordPath(wtA)); err != nil {
			t.Fatalf("record gone: %v", err)
		}
		requireRegistryFree(t, controlHome)
	})

	t.Run("writes nothing when cancelled before the lock is taken", func(t *testing.T) {
		controlHome := claimedHome(t)
		writeMinimalRecord(t, controlHome, repoA, wtA, "feature-login", acmeDomain, "complete")
		store := registry.NewStore(controlHome, repoA)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		removed, err := store.Remove(ctx, wtA, "feature-login", testWriter)
		if err == nil || removed {
			t.Fatalf("Remove under a cancelled context: %v, %v", removed, err)
		}
		if _, err := os.Stat(store.RecordPath(wtA)); err != nil {
			t.Fatalf("record gone: %v", err)
		}
	})
}
