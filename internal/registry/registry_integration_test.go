//go:build integration

// Port of tests/integration/worktree-registry-git.integration.test.ts's
// read-path titles: real Git, real worktrees, no fakes. WorktreeRegistry's
// one write path (Ensure) is not part of this package — see registry.go's
// package comment — so where the TypeScript suite calls `.ensure()` to set
// a fixture up, this file uses registrytest.Register (identity markers via
// the real, lock-free IdentityService.Ensure, plus a hand-written record)
// to reach the same on-disk state, then exercises only read methods.
package registry_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/run"
)

// execRunner is a minimal real-process run.Runner for this integration
// test. internal/run/osrun — the real implementation another work package
// is building for the lock layer — is not a dependency of this read-path
// package, so this stands in for it here.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, spec run.Spec) (run.Result, error) {
	cmd := exec.CommandContext(ctx, spec.Command, spec.Args...)
	if spec.Dir != "" {
		cmd.Dir = spec.Dir
	}
	if spec.Env != nil {
		cmd.Env = spec.Env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	result := run.Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if exitErr, ok := runErr.(*exec.ExitError); ok {
		result.ExitCode = exitErr.ExitCode()
	} else if runErr != nil {
		return result, runErr
	}
	if !spec.AllowFailure && result.ExitCode != 0 {
		return result, run.Failure(spec, result)
	}
	return result, nil
}

func (execRunner) Which(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

const integrationDomain = "acme.localhost"

// git runs a real `git` command, failing the test on a non-zero exit.
func git(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Registry Tests", "GIT_AUTHOR_EMAIL=tests@example.invalid",
		"GIT_COMMITTER_NAME=Registry Tests", "GIT_COMMITTER_EMAIL=tests@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// repository is a fresh Git repository with one empty commit on main.
func repository(t *testing.T) (base, repoDir string) {
	t.Helper()
	base = registrytest.TempDir(t)
	repoDir = filepath.Join(base, "repository")
	git(t, "init", "--initial-branch=main", repoDir)
	git(t, "-C", repoDir, "commit", "--allow-empty", "-m", "initial")
	return base, repoDir
}

// registerWorktree stands in for `registry.ensure()`: it creates real
// identity markers (via the lock-free IdentityService.Ensure) and writes a
// fixture record directly, bypassing the registry lock this package does
// not implement.
func registerWorktree(t *testing.T, controlHome, worktreeDir, slug string) *registry.WorktreeIdentity {
	t.Helper()
	identities := registry.NewIdentityService(worktreeDir, execRunner{})
	identity, err := registrytest.Register(context.Background(), controlHome, identities, worktreeDir, integrationDomain, slug, nil, "complete")
	if err != nil {
		t.Fatalf("registerWorktree: %v", err)
	}
	return identity
}

func newRegistry(controlHome, root string) *registry.Registry {
	return registry.New(registry.Options{
		ControlHome: controlHome, Root: root, Domain: integrationDomain,
		Runner: execRunner{}, Logger: &logger.Recording{},
	})
}

func TestWorktreeRegistryWithRealGit(t *testing.T) {
	ctx := context.Background()

	t.Run("lists and prunes repository records from an unregistered sibling", func(t *testing.T) {
		base, repoDir := repository(t)
		controlHome := filepath.Join(base, "control")
		worktreeDir := filepath.Join(base, "registered-worktree")
		git(t, "-C", repoDir, "worktree", "add", "-b", "feature/registered", worktreeDir)
		registered := registerWorktree(t, controlHome, worktreeDir, "feature-registered")
		git(t, "-C", repoDir, "worktree", "remove", worktreeDir)

		sibling := newRegistry(controlHome, repoDir)

		identity, err := sibling.Identities.Inspect(ctx, repoDir, registry.InspectOptions{})
		if err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		if identity != nil {
			t.Fatalf("expected the primary checkout itself to have no worktree marker, got %+v", identity)
		}

		list, err := sibling.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 1 || list[0].WorktreeID != registered.WorktreeID || list[0].Slug != "feature-registered" {
			t.Fatalf("unexpected list: %+v", list)
		}

		candidates, err := sibling.PruneCandidates(ctx)
		if err != nil {
			t.Fatalf("PruneCandidates: %v", err)
		}
		if len(candidates) != 1 || candidates[0].WorktreeID != registered.WorktreeID {
			t.Fatalf("unexpected prune candidates: %+v", candidates)
		}
	})

	t.Run("treats a Git-listed worktree with a missing root as an orphan", func(t *testing.T) {
		base, repoDir := repository(t)
		controlHome := filepath.Join(base, "control")
		worktreeDir := filepath.Join(base, "missing-worktree")
		git(t, "-C", repoDir, "worktree", "add", "-b", "feature/missing", worktreeDir)

		registerWorktree(t, controlHome, repoDir, "main")
		missing := registerWorktree(t, controlHome, worktreeDir, "feature-missing")
		if err := os.RemoveAll(worktreeDir); err != nil {
			t.Fatalf("remove worktree: %v", err)
		}

		reg := newRegistry(controlHome, repoDir)
		candidates, err := reg.PruneCandidates(ctx)
		if err != nil {
			t.Fatalf("PruneCandidates: %v", err)
		}
		if len(candidates) != 1 || candidates[0].WorktreeID != missing.WorktreeID {
			t.Fatalf("expected only the missing worktree as a candidate, got %+v", candidates)
		}
	})

	t.Run("does not reuse identity when Git reuses a worktree path and administrative slot", func(t *testing.T) {
		_, repoDir := repository(t)
		worktreeDir := filepath.Join(filepath.Dir(repoDir), "reused-worktree")
		git(t, "-C", repoDir, "worktree", "add", "-b", "feature/first", worktreeDir)
		first, err := registry.NewIdentityService(worktreeDir, execRunner{}).Ensure(ctx, worktreeDir)
		if err != nil {
			t.Fatalf("Ensure (first): %v", err)
		}

		git(t, "-C", repoDir, "worktree", "remove", "--force", worktreeDir)
		git(t, "-C", repoDir, "worktree", "add", "-b", "feature/second", worktreeDir)
		second, err := registry.NewIdentityService(worktreeDir, execRunner{}).Ensure(ctx, worktreeDir)
		if err != nil {
			t.Fatalf("Ensure (second): %v", err)
		}

		if second.RepositoryID != first.RepositoryID {
			t.Fatalf("expected the same repository, got %s and %s", first.RepositoryID, second.RepositoryID)
		}
		if second.WorktreeID == first.WorktreeID {
			t.Fatalf("expected a fresh worktree identity once Git recreated the administrative slot")
		}
	})

	t.Run("retains identity across a real Git worktree move", func(t *testing.T) {
		base, repoDir := repository(t)
		original := filepath.Join(base, "original-worktree")
		moved := filepath.Join(base, "moved-worktree")
		git(t, "-C", repoDir, "worktree", "add", "-b", "feature/move", original)
		first, err := registry.NewIdentityService(original, execRunner{}).Ensure(ctx, original)
		if err != nil {
			t.Fatalf("Ensure (original): %v", err)
		}

		git(t, "-C", repoDir, "worktree", "move", original, moved)
		second, err := registry.NewIdentityService(moved, execRunner{}).Ensure(ctx, moved)
		if err != nil {
			t.Fatalf("Ensure (moved): %v", err)
		}

		if second.RepositoryID != first.RepositoryID || second.WorktreeID != first.WorktreeID {
			t.Fatalf("expected identity to survive the move: first=%+v second=%+v", first, second)
		}
		if second.Root != moved {
			t.Fatalf("expected root to be the moved path, got %q", second.Root)
		}
	})

	t.Run("allocates distinct slugs for separate clones sharing a branch and a domain", func(t *testing.T) {
		base, repoDir := repository(t)
		git(t, "-C", repoDir, "checkout", "-b", "feature/shared")
		clone := filepath.Join(base, "clone")
		git(t, "clone", "--branch", "feature/shared", repoDir, clone)

		first, err := registry.NewIdentityService(repoDir, execRunner{}).Ensure(ctx, repoDir)
		if err != nil {
			t.Fatalf("Ensure (repository): %v", err)
		}
		second, err := registry.NewIdentityService(clone, execRunner{}).Ensure(ctx, clone)
		if err != nil {
			t.Fatalf("Ensure (clone): %v", err)
		}
		if second.RepositoryID == first.RepositoryID {
			t.Fatalf("expected the clone to be a distinct repository")
		}

		firstSlug, err := registry.AllocateSlug(*first, integrationDomain, nil, "")
		if err != nil {
			t.Fatalf("AllocateSlug (first): %v", err)
		}
		if firstSlug != "feature-shared" {
			t.Fatalf("got %q", firstSlug)
		}
		secondSlug, err := registry.AllocateSlug(*second, integrationDomain, []registry.RegisteredWorktree{
			{RepositoryID: first.RepositoryID, WorktreeID: first.WorktreeID, Slug: firstSlug, Domain: integrationDomain},
		}, "")
		if err != nil {
			t.Fatalf("AllocateSlug (second): %v", err)
		}
		if !regexp.MustCompile(`^feature-shared-[a-f0-9]{12}$`).MatchString(secondSlug) {
			t.Fatalf("got %q", secondSlug)
		}
	})

	// macOS mounts a symlinked /tmp -> /private/tmp, so a worktree's own
	// path and what `git rev-parse --show-toplevel` reports can differ only
	// by which spelling was used to reach it. CanonicalPath (realpath(3))
	// is what keeps identity the same either way; this simulates that with
	// a symlinked temp directory on Linux.
	t.Run("resolves identity the same way through a symlinked ancestor directory (macOS-style /private paths)", func(t *testing.T) {
		real := t.TempDir()
		linked := filepath.Join(filepath.Dir(real), "linked-"+filepath.Base(real))
		if err := os.Symlink(real, linked); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove(linked) })

		repoDir := filepath.Join(linked, "repository")
		git(t, "init", "--initial-branch=main", repoDir)
		git(t, "-C", repoDir, "commit", "--allow-empty", "-m", "initial")

		viaSymlink, err := registry.NewIdentityService(repoDir, execRunner{}).Ensure(ctx, repoDir)
		if err != nil {
			t.Fatalf("Ensure (via symlink): %v", err)
		}
		canonicalRepo, err := filepath.EvalSymlinks(repoDir)
		if err != nil {
			t.Fatalf("EvalSymlinks: %v", err)
		}
		if viaSymlink.Root != canonicalRepo {
			t.Fatalf("expected Root to be canonicalized: got %q, want %q", viaSymlink.Root, canonicalRepo)
		}

		viaReal, err := registry.NewIdentityService(canonicalRepo, execRunner{}).Ensure(ctx, canonicalRepo)
		if err != nil {
			t.Fatalf("Ensure (via real path): %v", err)
		}
		if viaReal.RepositoryID != viaSymlink.RepositoryID || viaReal.WorktreeID != viaSymlink.WorktreeID {
			t.Fatalf("expected the same identity through either spelling: symlink=%+v real=%+v", viaSymlink, viaReal)
		}
	})
}
