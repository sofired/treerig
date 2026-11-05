package registry_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/run/fakerun"
)

// addLinked registers a linked worktree called name under the fixture's common
// directory, with the gitdir file Git writes, and returns its root.
func addLinked(t *testing.T, f identityFixture, name string) string {
	t.Helper()
	root := filepath.Join(f.base, name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.common, "worktrees", name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.common, "worktrees", name, "gitdir"), []byte(filepath.Join(root, ".git")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git.Add(root, "feature/"+name, f.common)
	return root
}

func TestIdentityServiceEntries(t *testing.T) {
	ctx := context.Background()

	t.Run("lists each worktree with its identity and whether its directory is there", func(t *testing.T) {
		f := newIdentityFixture(t, "main")
		main, err := f.identities.Ensure(ctx, f.root)
		if err != nil {
			t.Fatal(err)
		}
		linked := addLinked(t, f, "feature-a")
		other, err := f.identities.Ensure(ctx, linked)
		if err != nil {
			t.Fatal(err)
		}
		entries, err := f.identities.Entries(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 {
			t.Fatalf("entries = %+v", entries)
		}
		if e := entries[0]; e.WorktreeID != main.WorktreeID || e.RepositoryID != main.RepositoryID || !e.Exists || e.Prunable || e.Locked {
			t.Fatalf("main = %+v", e)
		}
		if e := entries[1]; e.WorktreeID != other.WorktreeID || e.Root != linked || !e.Exists || e.Branch != "feature/feature-a" {
			t.Fatalf("linked = %+v", e)
		}
	})

	t.Run("reads the identity of a prunable entry whose directory is gone from Git's own directory for it", func(t *testing.T) {
		f := newIdentityFixture(t, "main")
		main, err := f.identities.Ensure(ctx, f.root)
		if err != nil {
			t.Fatal(err)
		}
		linked := addLinked(t, f, "feature-b")
		other, err := f.identities.Ensure(ctx, linked)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(linked); err != nil {
			t.Fatal(err)
		}
		entries, err := f.identities.Entries(ctx)
		if err != nil {
			t.Fatal(err)
		}
		gone := entries[1]
		if gone.Exists || !gone.Prunable || gone.Locked || gone.WorktreeID != other.WorktreeID || gone.RepositoryID != main.RepositoryID || gone.IdentityErr != nil {
			t.Fatalf("gone = %+v", gone)
		}
		if gone.PrunableReason == "" {
			t.Fatal("Git's reason for calling it prunable was dropped")
		}
	})

	t.Run("reports a locked entry as locked, and not prunable", func(t *testing.T) {
		f := newIdentityFixture(t, "main")
		if _, err := f.identities.Ensure(ctx, f.root); err != nil {
			t.Fatal(err)
		}
		linked := addLinked(t, f, "feature-c")
		if _, err := f.identities.Ensure(ctx, linked); err != nil {
			t.Fatal(err)
		}
		f.git.SetLocked(linked, true)
		if err := os.RemoveAll(linked); err != nil {
			t.Fatal(err)
		}
		entries, err := f.identities.Entries(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if e := entries[1]; !e.Locked || e.Prunable || e.Exists {
			t.Fatalf("locked = %+v", e)
		}
	})

	t.Run("keeps listing when one entry's marker cannot be read", func(t *testing.T) {
		f := newIdentityFixture(t, "main")
		main, err := f.identities.Ensure(ctx, f.root)
		if err != nil {
			t.Fatal(err)
		}
		linked := addLinked(t, f, "feature-d")
		if _, err := f.identities.Ensure(ctx, linked); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(f.common, "worktrees", "feature-d", "treerig", "worktree-id")
		if err := os.WriteFile(marker, []byte("format 2\nwritten-by-the-future\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		entries, err := f.identities.Entries(ctx)
		if err != nil {
			t.Fatalf("one unreadable marker must not fail the list: %v", err)
		}
		if entries[0].WorktreeID != main.WorktreeID {
			t.Fatalf("main = %+v", entries[0])
		}
		if entries[1].IdentityErr == nil || entries[1].WorktreeID != "" {
			t.Fatalf("linked = %+v, want its identity error", entries[1])
		}
	})

	t.Run("gives a worktree that was never registered no identity", func(t *testing.T) {
		f := newIdentityFixture(t, "main")
		linked := addLinked(t, f, "feature-e")
		entries, err := f.identities.Entries(ctx)
		if err != nil || len(entries) != 2 {
			t.Fatalf("%+v, %v", entries, err)
		}
		if e := entries[1]; e.WorktreeID != "" || e.IdentityErr != nil || e.Root != linked {
			t.Fatalf("linked = %+v", e)
		}
	})

	t.Run("fails when Git does", func(t *testing.T) {
		runner := &fakerun.Runner{Handler: func(context.Context, run.Spec) (run.Result, error) {
			return run.Result{ExitCode: 128, Stderr: "fatal: detected dubious ownership\n"}, nil
		}}
		service := registry.NewIdentityService(registrytest.TempDir(t), runner, nil)
		if _, err := service.Entries(ctx); err == nil {
			t.Fatal("expected Git's failure")
		}
	})
}
