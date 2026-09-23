package registry_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/run"
)

type identityFixture struct {
	base, root, common string
	git                *registrytest.GitRunner
	identities         *registry.IdentityService
	runner             run.Runner
}

func newIdentityFixture(t *testing.T, branch string) identityFixture {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "worktree")
	common := filepath.Join(base, "common")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(common, "worktrees", "worktree"), 0o700); err != nil {
		t.Fatalf("mkdir git dir: %v", err)
	}
	runner, g := registrytest.NewFakeRunner()
	g.Add(root, branch, common)
	return identityFixture{
		base: base, root: root, common: common, git: g, runner: runner,
		identities: registry.NewIdentityService(root, runner),
	}
}

func TestIdentityServiceInspect(t *testing.T) {
	ctx := context.Background()

	t.Run("treats a missing worktree marker as not registered", func(t *testing.T) {
		f := newIdentityFixture(t, "feature/one")
		identity, err := f.identities.Inspect(ctx, f.root, registry.InspectOptions{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if identity != nil {
			t.Fatalf("expected no identity, got %+v", identity)
		}
	})

	t.Run("creates then reads back the same markers", func(t *testing.T) {
		f := newIdentityFixture(t, "feature/two")
		first, err := f.identities.Ensure(ctx, f.root)
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		if !uuidPattern.MatchString(first.RepositoryID) || !uuidPattern.MatchString(first.WorktreeID) {
			t.Fatalf("expected UUIDs, got %+v", first)
		}

		second, err := f.identities.Inspect(ctx, f.root, registry.InspectOptions{})
		if err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		if second == nil || second.RepositoryID != first.RepositoryID || second.WorktreeID != first.WorktreeID {
			t.Fatalf("expected the same identity back, got %+v", second)
		}
		if second.Branch != "feature/two" || second.Root != f.root {
			t.Fatalf("unexpected root/branch: %+v", second)
		}
	})

	t.Run("rejects a symlinked identity-marker parent before creating markers", func(t *testing.T) {
		f := newIdentityFixture(t, "feature/three")
		markerDirectory := filepath.Join(f.common, "worktrees", "worktree", "berth")
		outside := filepath.Join(f.base, "outside-markers")
		if err := os.MkdirAll(outside, 0o700); err != nil {
			t.Fatalf("mkdir outside: %v", err)
		}
		if err := os.Symlink(outside, markerDirectory); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		_, err := f.identities.Ensure(ctx, f.root)
		requireErrorContainsRegistry(t, err, "Unsafe identity marker path")

		if _, err := os.ReadFile(filepath.Join(outside, "worktree-id")); err == nil {
			t.Fatalf("expected nothing to have been written outside the marker directory")
		}
	})

	t.Run("preserves identity when a worktree moves", func(t *testing.T) {
		f := newIdentityFixture(t, "feature/move")
		first, err := f.identities.Ensure(ctx, f.root)
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}

		moved := filepath.Join(f.base, "moved-worktree")
		if err := os.MkdirAll(moved, 0o700); err != nil {
			t.Fatalf("mkdir moved: %v", err)
		}
		// Git reports the same administrative directories for the moved root;
		// only the worktree's own path changed.
		f.git.SetIdentity(moved, "feature/move", f.common, filepath.Join(f.common, "worktrees", "worktree"))
		f.git.SetActive([]string{moved})
		movedIdentities := registry.NewIdentityService(moved, f.runner)

		second, err := movedIdentities.Inspect(ctx, moved, registry.InspectOptions{})
		if err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		if second == nil {
			t.Fatalf("expected an identity after the move")
		}
		if second.WorktreeID != first.WorktreeID || second.RepositoryID != first.RepositoryID {
			t.Fatalf("expected identity to survive the move: first=%+v second=%+v", first, second)
		}
		if second.Root != moved {
			t.Fatalf("expected root to be the moved path, got %q", second.Root)
		}
	})

	t.Run("ignores an active sibling worktree that has no worktree marker", func(t *testing.T) {
		f := newIdentityFixture(t, "feature/registered")
		if _, err := f.identities.Ensure(ctx, f.root); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		sibling := filepath.Join(f.base, "unregistered-worktree")
		if err := os.MkdirAll(sibling, 0o700); err != nil {
			t.Fatalf("mkdir sibling: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(f.common, "worktrees", "unregistered-worktree"), 0o700); err != nil {
			t.Fatalf("mkdir sibling git dir: %v", err)
		}
		f.git.Add(sibling, "feature/unregistered", f.common)
		siblingIdentities := registry.NewIdentityService(sibling, f.runner)

		identity, err := siblingIdentities.Inspect(ctx, sibling, registry.InspectOptions{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if identity != nil {
			t.Fatalf("expected no identity for an unregistered sibling, got %+v", identity)
		}
	})
}

func TestIdentityServiceRemoveWorktreeMarker(t *testing.T) {
	ctx := context.Background()

	t.Run("removes the marker, and is a no-op once it is gone", func(t *testing.T) {
		f := newIdentityFixture(t, "feature/remove")
		identity, err := f.identities.Ensure(ctx, f.root)
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}

		if err := f.identities.RemoveWorktreeMarker(*identity); err != nil {
			t.Fatalf("RemoveWorktreeMarker: %v", err)
		}
		after, err := f.identities.Inspect(ctx, f.root, registry.InspectOptions{})
		if err != nil {
			t.Fatalf("Inspect after removal: %v", err)
		}
		if after != nil {
			t.Fatalf("expected no identity after removal, got %+v", after)
		}

		// Idempotent: removing an already-gone marker is not an error.
		if err := f.identities.RemoveWorktreeMarker(*identity); err != nil {
			t.Fatalf("expected removing an already-gone marker to be a no-op, got %v", err)
		}
	})

	t.Run("refuses when the marker changed identity before cleanup ran", func(t *testing.T) {
		f := newIdentityFixture(t, "feature/changed")
		identity, err := f.identities.Ensure(ctx, f.root)
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		marker := filepath.Join(identity.GitDir, "berth", "worktree-id")
		if err := os.WriteFile(marker, []byte("2b41d9d4-ed88-49c1-be9c-5439bdf6a431\n"), 0o600); err != nil {
			t.Fatalf("overwrite marker: %v", err)
		}

		err = f.identities.RemoveWorktreeMarker(*identity)
		requireErrorContainsRegistry(t, err, "Worktree identity changed before cleanup completed")
	})
}

func TestIdentityServiceListAndActive(t *testing.T) {
	ctx := context.Background()

	t.Run("lists active worktrees, including a detached one", func(t *testing.T) {
		f := newIdentityFixture(t, "feature/one")
		second := filepath.Join(f.base, "detached")
		if err := os.MkdirAll(filepath.Join(f.common, "worktrees", "detached"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.MkdirAll(second, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		f.git.Add(second, "", f.common)

		listed, err := f.identities.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(listed) != 2 {
			t.Fatalf("expected 2 worktrees, got %d: %+v", len(listed), listed)
		}
		byRoot := map[string]string{}
		for _, w := range listed {
			byRoot[w.Root] = w.Branch
		}
		if byRoot[f.root] != "feature/one" {
			t.Fatalf("expected feature/one for %s, got %+v", f.root, byRoot)
		}
		if byRoot[second] != "" {
			t.Fatalf("expected an empty branch for the detached worktree, got %+v", byRoot)
		}
	})

	t.Run("active identities skip a worktree Git lists but that has no marker", func(t *testing.T) {
		f := newIdentityFixture(t, "feature/one")
		if _, err := f.identities.Ensure(ctx, f.root); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		sibling := filepath.Join(f.base, "sibling")
		if err := os.MkdirAll(filepath.Join(f.common, "worktrees", "sibling"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.MkdirAll(sibling, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		f.git.Add(sibling, "feature/sibling", f.common)

		active, err := f.identities.ActiveIdentities(ctx)
		if err != nil {
			t.Fatalf("ActiveIdentities: %v", err)
		}
		if len(active) != 1 || active[0].Root != f.root {
			t.Fatalf("expected only the registered worktree, got %+v", active)
		}
	})

	t.Run("active identities skip a worktree whose root no longer exists", func(t *testing.T) {
		f := newIdentityFixture(t, "feature/one")
		if _, err := f.identities.Ensure(ctx, f.root); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		if err := os.RemoveAll(f.root); err != nil {
			t.Fatalf("remove root: %v", err)
		}

		active, err := f.identities.ActiveIdentities(ctx)
		if err != nil {
			t.Fatalf("ActiveIdentities: %v", err)
		}
		if len(active) != 0 {
			t.Fatalf("expected no active identities once the root is gone, got %+v", active)
		}
	})
}

func requireErrorContainsRegistry(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q, got nil", substr)
	}
	if !contains(err.Error(), substr) {
		t.Fatalf("expected error containing %q, got %q", substr, err.Error())
	}
}
