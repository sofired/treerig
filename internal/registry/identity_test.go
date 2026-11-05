package registry_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/run/fakerun"
)

type identityFixture struct {
	base, root, common string
	git                *registrytest.GitRunner
	identities         *registry.IdentityService
	runner             run.Runner
}

// newIdentityFixture builds a worktree and its git common directory under a
// fresh temp directory, registers them with a fake git runner on branch, and
// returns an identityFixture with an IdentityService ready to answer for it.
func newIdentityFixture(t *testing.T, branch string) identityFixture {
	t.Helper()
	base := registrytest.TempDir(t)
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
		identities: registry.NewIdentityService(root, runner, nil),
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
		markerDirectory := filepath.Join(f.common, "worktrees", "worktree", "treerig")
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
		movedIdentities := registry.NewIdentityService(moved, f.runner, nil)

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
		siblingIdentities := registry.NewIdentityService(sibling, f.runner, nil)

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
		marker := filepath.Join(identity.GitDir, "treerig", "worktree-id")
		if err := os.WriteFile(marker, []byte(registry.IdentityMarkerContents("2b41d9d4-ed88-49c1-be9c-5439bdf6a431")), 0o600); err != nil {
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

// requireErrorContainsRegistry fails the test unless err is non-nil and its
// message contains substr.
func requireErrorContainsRegistry(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q, got nil", substr)
	}
	if !contains(err.Error(), substr) {
		t.Fatalf("expected error containing %q, got %q", substr, err.Error())
	}
}

func TestIdentityServiceRepository(t *testing.T) {
	ctx := context.Background()

	t.Run("answers the repository's identity inside a work tree, and says so", func(t *testing.T) {
		f := newIdentityFixture(t, "feature/one")
		created, err := f.identities.Ensure(ctx, f.root)
		if err != nil {
			t.Fatal(err)
		}
		id, present, inside, err := f.identities.Repository(ctx, f.root)
		if err != nil || !inside || !present || id != created.RepositoryID {
			t.Fatalf("Repository = %q, %v, %v, %v; want %q present and inside", id, present, inside, err, created.RepositoryID)
		}
	})

	t.Run("inside a work tree treerig has no identity for, it is present false and inside true", func(t *testing.T) {
		f := newIdentityFixture(t, "feature/one")
		id, present, inside, err := f.identities.Repository(ctx, f.root)
		if err != nil || !inside || present || id != "" {
			t.Fatalf("Repository = %q, %v, %v, %v", id, present, inside, err)
		}
	})

	t.Run("outside every repository is inside false and no error, asked in the C locale", func(t *testing.T) {
		f := newIdentityFixture(t, "feature/one")
		runner := &recordingRunner{Runner: f.runner}
		identities := registry.New(registry.Options{ControlHome: f.base, Root: f.root, Runner: runner, Environ: []string{"PATH=/bin", "LANG=de_DE.UTF-8"}}).Identities
		outside := filepath.Join(f.base, "elsewhere")
		id, present, inside, err := identities.Repository(ctx, outside)
		if err != nil || inside || present || id != "" {
			t.Fatalf("Repository = %q, %v, %v, %v", id, present, inside, err)
		}
		want := []string{"LC_ALL=C", "GIT_OPTIONAL_LOCKS=0", "PATH=/bin", "LANG=de_DE.UTF-8"}
		if len(runner.specs) != 1 || !slices.Equal(runner.specs[0].Env, want) {
			t.Fatalf("specs = %+v, want one call in the allow-listed environment %v", runner.specs, want)
		}
	})

	t.Run("every Git question is bounded and runs without what a repository's configuration could execute", func(t *testing.T) {
		f := newIdentityFixture(t, "feature/one")
		runner := &recordingRunner{Runner: f.runner}
		environ := []string{
			"PATH=/bin", "HOME=/home/x", "TMPDIR=/tmp", "LANG=C.UTF-8", "LC_TIME=de_DE", "LC_ALL=de_DE",
			"GIT_DIR=/elsewhere", "GIT_EXEC_PATH=/evil", "GIT_SSH_COMMAND=x", "EDITOR=vi", "SECRET=1",
			"XDG_CONFIG_HOME=/home/x/.config", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_WORK_TREE=/w", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/evil", "GIT_CONFIG_PARAMETERS='a=b'",
		}
		identities := registry.New(registry.Options{ControlHome: f.base, Root: f.root, Runner: runner, Environ: environ}).Identities
		_, _, _, _ = identities.Repository(ctx, f.root)
		_, _ = identities.Entries(ctx)
		_, _ = identities.Inspect(ctx, f.root, registry.InspectOptions{})
		if len(runner.specs) < 4 {
			t.Fatalf("specs = %d", len(runner.specs))
		}
		wantEnv := []string{"LC_ALL=C", "GIT_OPTIONAL_LOCKS=0", "PATH=/bin", "HOME=/home/x", "TMPDIR=/tmp", "LANG=C.UTF-8", "LC_TIME=de_DE",
			"XDG_CONFIG_HOME=/home/x/.config", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}
		for _, spec := range runner.specs {
			head := strings.Join(spec.Args[:6], " ")
			if head != "-c core.fsmonitor=false -c core.hooksPath=/dev/null -c safe.bareRepository=explicit" {
				t.Errorf("args = %v, want the hardening flags first", spec.Args)
			}
			if !slices.Equal(spec.Env, wantEnv) {
				t.Errorf("env = %v, want %v", spec.Env, wantEnv)
			}
			if spec.Timeout <= 0 || spec.OutputLimit <= 0 || spec.OutputLimit > 8*1024*1024 {
				t.Errorf("timeout %v, output limit %d: want both bounded", spec.Timeout, spec.OutputLimit)
			}
		}
	})

	t.Run("any other Git failure is an error carrying Git's words, not an answer", func(t *testing.T) {
		for _, stderr := range []string{
			"fatal: detected dubious ownership in repository at '/srv/x'",
			"fatal: bad object HEAD",
			"fatal: this operation must be run in a work tree",
		} {
			runner := &fakerun.Runner{Handler: func(context.Context, run.Spec) (run.Result, error) {
				return run.Result{ExitCode: 128, Stderr: stderr + "\n"}, nil
			}}
			id, present, inside, err := registry.NewIdentityService(t.TempDir(), runner, nil).Repository(ctx, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), stderr) || inside {
				t.Fatalf("%q: Repository = %q, %v, %v, %v; want an error naming Git's words", stderr, id, present, inside, err)
			}
		}
	})

	t.Run("still reports a Git that could not be started", func(t *testing.T) {
		runner := &fakerun.Runner{Handler: func(context.Context, run.Spec) (run.Result, error) {
			return run.Result{}, &run.StartError{Command: "git", Code: "ENOENT", Detail: "was not found on PATH (ENOENT).", ExitCode: 127}
		}}
		_, _, _, err := registry.NewIdentityService(t.TempDir(), runner, nil).Repository(ctx, t.TempDir())
		var start *run.StartError
		if !errors.As(err, &start) {
			t.Fatalf("err = %v, want the start error", err)
		}
	})

	t.Run("RepositoryID outside every repository is still the Git failure", func(t *testing.T) {
		f := newIdentityFixture(t, "feature/one")
		_, _, err := f.identities.RepositoryID(ctx, filepath.Join(f.base, "elsewhere"))
		if err == nil || !strings.Contains(err.Error(), "not a git repository") {
			t.Fatalf("err = %v", err)
		}
	})
}

// recordingRunner records every spec it is asked to run, then runs it.
type recordingRunner struct {
	run.Runner
	specs []run.Spec
}

// Run records spec and passes it on.
func (r *recordingRunner) Run(ctx context.Context, spec run.Spec) (run.Result, error) {
	r.specs = append(r.specs, spec)
	return r.Runner.Run(ctx, spec)
}
