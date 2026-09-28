package commands_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/commands/commandstest"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry/registrytest"
)

func TestRegisteredWorktreeCandidates(t *testing.T) {
	ctx := context.Background()

	t.Run("lists every registered worktree by slug, with its domain and last-known path", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		other := w.Worktree("other", "feature/other")
		loginIdentity := register(t, w, w.Root, acmeDomain, "feature-login", "complete")
		register(t, w, other, acmeDomain, "feature-other", "complete")
		w.WriteIndexCard(loginIdentity.RepositoryID, loginIdentity.WorktreeID, "acme", acmeDomain, "feature-login",
			"feature-login."+acmeDomain, w.Root, "feature/login")
		// feature-other gets no card, so its own candidate completes with no known path.

		cctx := w.Context(w.Root, acmeDomain, nil)
		candidates, err := commands.RegisteredWorktreeCandidates(ctx, cctx.CommandContext)
		if err != nil {
			t.Fatalf("RegisteredWorktreeCandidates: %v", err)
		}
		if len(candidates) != 2 {
			t.Fatalf("expected 2 candidates, got %+v", candidates)
		}
		if candidates[0].Slug != "feature-login" || candidates[0].Domain != acmeDomain || candidates[0].Root != w.Root {
			t.Fatalf("unexpected candidate 0: %+v", candidates[0])
		}
		if candidates[1].Slug != "feature-other" || candidates[1].Root != "" {
			t.Fatalf("unexpected candidate 1 (expected no known path): %+v", candidates[1])
		}
	})

	t.Run("a repository with nothing registered answers no candidates and no error", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		cctx := w.Context(w.Root, acmeDomain, nil)
		candidates, err := commands.RegisteredWorktreeCandidates(ctx, cctx.CommandContext)
		if err != nil {
			t.Fatalf("RegisteredWorktreeCandidates: %v", err)
		}
		if len(candidates) != 0 {
			t.Fatalf("expected no candidates, got %+v", candidates)
		}
	})
}

func TestOrphanWorktrees(t *testing.T) {
	t.Run("lists every worktree that has a lock file but no registration, deduplicating its operation and development locks", func(t *testing.T) {
		controlHome := registrytest.TempDir(t)
		repositoryID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		worktreeID := "11111111-1111-4111-8111-111111111111"
		dir := filepath.Join(layout.ControlLayout(controlHome).LocksDir, "worktrees", repositoryID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, worktreeID), nil, 0o644); err != nil {
			t.Fatalf("write lock file: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, worktreeID+".dev"), nil, 0o644); err != nil {
			t.Fatalf("write development lock file: %v", err)
		}

		orphans, err := commands.OrphanWorktrees(controlHome)
		if err != nil {
			t.Fatalf("OrphanWorktrees: %v", err)
		}
		if len(orphans) != 1 || orphans[0].RepositoryID != repositoryID || orphans[0].WorktreeID != worktreeID {
			t.Fatalf("orphans = %+v, want a single %s/%s", orphans, repositoryID, worktreeID)
		}
	})

	t.Run("a control home with no worktree locks at all answers no orphans and no error", func(t *testing.T) {
		controlHome := registrytest.TempDir(t)
		orphans, err := commands.OrphanWorktrees(filepath.Join(controlHome, "nonexistent"))
		if err != nil {
			t.Fatalf("OrphanWorktrees: %v", err)
		}
		if len(orphans) != 0 {
			t.Fatalf("expected no orphans, got %+v", orphans)
		}
	})

	t.Run("skips a stray file that is not a worktree UUID once its own suffixes are stripped", func(t *testing.T) {
		controlHome := registrytest.TempDir(t)
		repositoryID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		worktreeID := "11111111-1111-4111-8111-111111111111"
		dir := filepath.Join(layout.ControlLayout(controlHome).LocksDir, "worktrees", repositoryID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, worktreeID), nil, 0o644); err != nil {
			t.Fatalf("write lock file: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".DS_Store"), nil, 0o644); err != nil {
			t.Fatalf("write stray file: %v", err)
		}

		orphans, err := commands.OrphanWorktrees(controlHome)
		if err != nil {
			t.Fatalf("OrphanWorktrees: %v", err)
		}
		if len(orphans) != 1 || orphans[0].WorktreeID != worktreeID {
			t.Fatalf("orphans = %+v, want only %s (the stray .DS_Store must not be offered)", orphans, worktreeID)
		}
	})
}
