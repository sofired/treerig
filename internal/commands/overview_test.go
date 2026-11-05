package commands

// Round 2 of review's item 2: newOverview must refuse a symlinked
// router state directory the same way RouterUp already does, before it ever
// creates the lock file or writes the index page into whatever the symlink
// points at. A white-box (package commands) test file, not commandstest,
// which itself imports this package and would cycle back into it.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/overview"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/router"
	"github.com/sofired/treerig/internal/run/fakerun"
)

// symlinkedCommandContext builds a minimal CommandContext whose router state
// directory is a symlink to target, for exercising newOverview's own
// router.SecureState refusal in isolation from a full Env/config-on-disk
// harness (Context(env), which `list` itself goes through, needs
// more setup than this fix's own seam does).
func symlinkedCommandContext(t *testing.T, target string) (*CommandContext, *logger.Recording) {
	t.Helper()
	controlHome := t.TempDir()
	stateParent := t.TempDir()
	stateDir := filepath.Join(stateParent, "router")
	if err := os.Symlink(target, stateDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	routerState := layout.RouterLayout{
		StateDir:  stateDir,
		IndexDir:  filepath.Join(stateDir, "index"),
		IndexFile: filepath.Join(stateDir, "index", "index.html"),
		LockPath:  filepath.Join(stateDir, "lock"),
	}
	log := &logger.Recording{}
	reg := registry.New(registry.Options{
		ControlHome: controlHome, Root: t.TempDir(), Runner: &fakerun.Runner{}, Logger: log, CommandName: "treerig",
	})
	return &CommandContext{
		CommandName: "treerig", Invocation: "treerig", Logger: log, Runner: &fakerun.Runner{},
		ControlHome: controlHome, RouterPort: 80, RouterState: routerState, Registry: reg,
	}, log
}

// TestNewOverviewRefusesASymlinkedRouterStateDirectory is the "list --refresh"
// side of item 2: a symlinked router state directory must fail newOverview
// outright, the way `list --refresh` needs it to (a refresh that never starts
// must not report success and print nothing useful).
func TestNewOverviewRefusesASymlinkedRouterStateDirectory(t *testing.T) {
	cctx, _ := symlinkedCommandContext(t, t.TempDir())

	if _, err := newOverview(cctx, refreshesPage); err == nil {
		t.Fatal("expected newOverview to refuse a symlinked router state directory")
	}
}

// TestRefreshOverviewBestEffortWarnsInsteadOfFailingOnASymlinkedRouterState is
// the best-effort side of item 2: down and worktree remove must not fail
// their own command over this — they warn, in RefreshBestEffort's own
// wording, and move on.
func TestRefreshOverviewBestEffortWarnsInsteadOfFailingOnASymlinkedRouterState(t *testing.T) {
	cctx, log := symlinkedCommandContext(t, t.TempDir())

	refreshOverviewBestEffort(context.Background(), cctx, "down")

	lines, _ := log.Snapshot()
	wantPrefix := "warn: Could not refresh the overview after down: "
	if len(lines) != 1 || !strings.HasPrefix(lines[0], wantPrefix) {
		t.Fatalf("lines = %v, want exactly one line starting %q", lines, wantPrefix)
	}
}

// TestRouterLockScope replaces internal/lock's own deleted TestRouterScope
// (#80, review round 1 item 1): routerLockScope is now the one place that
// builds the shared router's lock scope, always from an already-resolved
// layout.RouterLayout, never a raw Getenv, so its own test moved here.
func TestRouterLockScope(t *testing.T) {
	state := layout.RouterLayout{LockPath: filepath.Join(t.TempDir(), "router", "lock")}

	t.Run("names the scope and its project marker explicitly", func(t *testing.T) {
		scope := routerLockScope(state, "treerig-router-explicit")
		if scope.Description != "shared router" {
			t.Errorf("Description = %q, want %q", scope.Description, "shared router")
		}
		if scope.Markers.Project != "treerig-router-explicit" {
			t.Errorf("Markers.Project = %q, want %q", scope.Markers.Project, "treerig-router-explicit")
		}
		if scope.Markers.WorktreeID != "" {
			t.Errorf("Markers.WorktreeID = %q, want empty: the router is not a worktree", scope.Markers.WorktreeID)
		}
	})

	t.Run("resolves router.ResolveProjectName's own default when given no project name", func(t *testing.T) {
		scope := routerLockScope(state, "")
		if scope.Markers.Project != router.ResolveProjectName() {
			t.Errorf("Markers.Project = %q, want %q", scope.Markers.Project, router.ResolveProjectName())
		}
	})

	t.Run("targets state's own lock file, named for whichever variable governs the router's state directory", func(t *testing.T) {
		scope := routerLockScope(state, "treerig-router")
		if filepath.Base(scope.Target) != "lock" {
			t.Errorf("Target = %q, want a file named lock", scope.Target)
		}
		if scope.HomeEnvVar != brand.HomeEnv {
			t.Errorf("HomeEnvVar = %q, want %q: the router has no control-home override of its own", scope.HomeEnvVar, brand.HomeEnv)
		}
	})
}

// TestSayUnknownCauses: a cause several rows share is said once, for the first
// of them, and one already given (Docker's, a repository Git was not asked
// about) is not said again.
func TestSayUnknownCauses(t *testing.T) {
	log := &logger.Recording{}
	cctx := &CommandContext{CommandName: "treerig", Logger: log}
	dockerError := "Cannot connect"
	entries := []ListEntry{
		{Host: "a.acme.localhost", Hint: "The volume is not mounted."},
		{Host: "b.acme.localhost", Hint: "The volume is not mounted."},
		{Host: "c.acme.localhost", Hint: "The volume is not mounted."},
		{Host: "d.acme.localhost", Hint: "Git is offline."},
		{Host: "e.acme.localhost", Hint: "Docker didn't answer: Cannot connect"},
		{Host: "f.acme.localhost", Hint: "treerig couldn't check it: x."},
		{Host: "g.acme.localhost"},
	}
	sayUnknownCauses(cctx, entries, overview.IndexSnapshot{DockerError: &dockerError, GitHints: []string{"treerig couldn't check it: x."}})
	lines, _ := log.Snapshot()
	want := []string{
		"info: a.acme.localhost and 2 more are unknown. The volume is not mounted.",
		"info: d.acme.localhost is unknown. Git is offline.",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("lines = %q, want %q", lines, want)
	}
}
