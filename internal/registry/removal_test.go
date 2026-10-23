package registry_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/stateformat"
)

func TestRemovalMarker(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*registry.Store, string) {
		t.Helper()
		controlHome := claimedHome(t)
		writeMinimalRecord(t, controlHome, repoA, wtA, "feature-login", acmeDomain, "complete")
		return registry.NewStore(controlHome, repoA), controlHome
	}

	t.Run("is absent until a removal starts, and then says so", func(t *testing.T) {
		store, controlHome := setup(t)
		if removing, err := store.Removing(wtA); err != nil || removing {
			t.Fatalf("Removing = %v, %v; want false before any removal", removing, err)
		}
		if err := store.MarkRemoving(ctx, wtA, "feature-login", testWriter); err != nil {
			t.Fatal(err)
		}
		if removing, err := store.Removing(wtA); err != nil || !removing {
			t.Fatalf("Removing = %v, %v; want true once marked", removing, err)
		}
		file := filepath.Join(controlHome, "repositories", repoA, "worktrees", wtA+".removing")
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "format 1\n" {
			t.Fatalf("marker = %q, want its format line alone", data)
		}
		info, err := os.Stat(file)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("marker mode = %v, %v; want 0600", info.Mode().Perm(), err)
		}
		requireRegistryFree(t, controlHome)
	})

	t.Run("leaves the registration untouched, and a scan of the registry still reads it", func(t *testing.T) {
		store, _ := setup(t)
		before, err := os.ReadFile(store.RecordPath(wtA))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.MarkRemoving(ctx, wtA, "feature-login", testWriter); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(store.RecordPath(wtA))
		if err != nil || string(before) != string(after) {
			t.Fatalf("the record changed: %v\n%s\n%s", err, before, after)
		}
		records, problems, err := store.Scan()
		if err != nil || len(records) != 1 || len(problems) != 0 {
			t.Fatalf("Scan = %v, %v, %v; the marker is not a record", records, problems, err)
		}
	})

	t.Run("is idempotent", func(t *testing.T) {
		store, _ := setup(t)
		for range 2 {
			if err := store.MarkRemoving(ctx, wtA, "feature-login", testWriter); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("goes with the registration when removal completes", func(t *testing.T) {
		store, _ := setup(t)
		if err := store.MarkRemoving(ctx, wtA, "feature-login", testWriter); err != nil {
			t.Fatal(err)
		}
		if removed, err := store.Remove(ctx, wtA, "feature-login", testWriter); err != nil || !removed {
			t.Fatalf("Remove = %v, %v", removed, err)
		}
		if removing, err := store.Removing(wtA); err != nil || removing {
			t.Fatalf("Removing = %v, %v; the marker should be gone", removing, err)
		}
	})

	t.Run("is dropped by a removal that finds the registration already gone", func(t *testing.T) {
		store, _ := setup(t)
		if err := store.MarkRemoving(ctx, wtA, "feature-login", testWriter); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(store.RecordPath(wtA)); err != nil {
			t.Fatal(err)
		}
		if removed, err := store.Remove(ctx, wtA, "feature-login", testWriter); err != nil || removed {
			t.Fatalf("Remove = %v, %v; there was no registration to remove", removed, err)
		}
		if removing, _ := store.Removing(wtA); removing {
			t.Fatal("an orphaned marker survived")
		}
	})

	t.Run("is taken away by clearing, which does not mind there being none", func(t *testing.T) {
		store, _ := setup(t)
		if err := store.ClearRemoving(ctx, wtA, testWriter); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkRemoving(ctx, wtA, "feature-login", testWriter); err != nil {
			t.Fatal(err)
		}
		if err := store.ClearRemoving(ctx, wtA, testWriter); err != nil {
			t.Fatal(err)
		}
		if removing, _ := store.Removing(wtA); removing {
			t.Fatal("the marker survived clearing")
		}
	})

	t.Run("refuses to mark a registration another slug now owns, writing nothing", func(t *testing.T) {
		store, _ := setup(t)
		if err := store.MarkRemoving(ctx, wtA, "someone-else", testWriter); err == nil {
			t.Fatal("expected a refusal")
		}
		if removing, _ := store.Removing(wtA); removing {
			t.Fatal("a marker was written")
		}
	})

	t.Run("refuses to mark a worktree that is not registered", func(t *testing.T) {
		store, _ := setup(t)
		if err := store.MarkRemoving(ctx, wtB, "feature-login", testWriter); err == nil {
			t.Fatal("expected a refusal")
		}
	})

	t.Run("refuses a marker of a newer format, and neither rewrites nor clears it", func(t *testing.T) {
		store, controlHome := setup(t)
		file := filepath.Join(controlHome, "repositories", repoA, "worktrees", wtA+".removing")
		newer := "format 2\nsomething a newer treerig added\n"
		if err := os.WriteFile(file, []byte(newer), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := store.Removing(wtA)
		if !errors.Is(err, stateformat.ErrNewer) {
			t.Fatalf("Removing: %v, want the newer-format refusal", err)
		}
		want := "Refusing to use the removal marker " + file + ": it is format 2, written by a newer treerig, and this build reads format 1. Upgrade treerig, then run this again."
		if err == nil || err.Error() != want {
			t.Fatalf("Removing: %v\nwant: %s", err, want)
		}
		if err := store.MarkRemoving(ctx, wtA, "feature-login", testWriter); !errors.Is(err, stateformat.ErrNewer) {
			t.Fatalf("MarkRemoving: %v, want the newer-format refusal", err)
		}
		if err := store.ClearRemoving(ctx, wtA, testWriter); !errors.Is(err, stateformat.ErrNewer) {
			t.Fatalf("ClearRemoving: %v, want the newer-format refusal", err)
		}
		if data, _ := os.ReadFile(file); string(data) != newer {
			t.Fatalf("the newer marker was changed: %q", data)
		}
	})

	t.Run("is not deleted, nor is the registration, when it is of a newer format", func(t *testing.T) {
		store, controlHome := setup(t)
		file := filepath.Join(controlHome, "repositories", repoA, "worktrees", wtA+".removing")
		if err := os.WriteFile(file, []byte("format 2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		removed, err := store.Remove(ctx, wtA, "feature-login", testWriter)
		if !errors.Is(err, stateformat.ErrNewer) || removed {
			t.Fatalf("Remove = %v, %v; want the newer-format refusal", removed, err)
		}
		if _, statErr := os.Stat(store.RecordPath(wtA)); statErr != nil {
			t.Fatalf("the registration went: %v", statErr)
		}
		if data, _ := os.ReadFile(file); string(data) != "format 2\n" {
			t.Fatalf("the newer marker was changed: %q", data)
		}
	})

	t.Run("does not take a file with no version, or an unknown one, for a marker", func(t *testing.T) {
		store, controlHome := setup(t)
		file := filepath.Join(controlHome, "repositories", repoA, "worktrees", wtA+".removing")
		for _, contents := range []string{"", "removing\n", "format 0\n", "format one\n", "format 1\nextra\n"} {
			if err := os.WriteFile(file, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			removing, err := store.Removing(wtA)
			if err == nil || removing || errors.Is(err, stateformat.ErrNewer) || !strings.Contains(err.Error(), file) {
				t.Fatalf("%q: Removing = %v, %v; want an error naming the file", contents, removing, err)
			}
		}
	})

	t.Run("does not follow a symbolic link", func(t *testing.T) {
		store, controlHome := setup(t)
		file := filepath.Join(controlHome, "repositories", repoA, "worktrees", wtA+".removing")
		target := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.WriteFile(target, []byte("format 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, file); err != nil {
			t.Fatal(err)
		}
		if removing, err := store.Removing(wtA); err == nil || removing {
			t.Fatalf("Removing = %v, %v; want a refusal", removing, err)
		}
	})
}
