package commands

// Round 2 of review's item 2: newCheckoutIndex must refuse a symlinked
// router state directory the same way RouterUp already does, before it ever
// creates the lock file or writes the index page into whatever the symlink
// points at. A white-box (package commands) test file, not commandstest,
// which itself imports this package and would cycle back into it.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/run/fakerun"
)

// symlinkedCommandContext builds a minimal CommandContext whose router state
// directory is a symlink to target, for exercising newCheckoutIndex's own
// router.SecureState refusal in isolation from a full Env/config-on-disk
// harness (Context(env), which RefreshIndex itself goes through, needs
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
		ControlHome: controlHome, Root: t.TempDir(), Runner: &fakerun.Runner{}, Logger: log, CommandName: "berth",
	})
	return &CommandContext{
		CommandName: "berth", Invocation: "berth", Logger: log, Runner: &fakerun.Runner{},
		ControlHome: controlHome, RouterPort: 80, RouterState: routerState, Registry: reg,
	}, log
}

// TestNewCheckoutIndexRefusesASymlinkedRouterStateDirectory is the "index
// verb" side of item 2: a symlinked router state directory must fail
// newCheckoutIndex outright, the way RefreshIndex needs it to (a refresh
// that never starts must not report success and print nothing useful).
func TestNewCheckoutIndexRefusesASymlinkedRouterStateDirectory(t *testing.T) {
	cctx, _ := symlinkedCommandContext(t, t.TempDir())

	if _, err := newCheckoutIndex(cctx); err == nil {
		t.Fatal("expected newCheckoutIndex to refuse a symlinked router state directory")
	}
}

// TestRefreshIndexBestEffortWarnsInsteadOfFailingOnASymlinkedRouterState is
// the best-effort side of item 2: down and worktree remove must not fail
// their own command over this — they warn, in RefreshBestEffort's own
// wording, and move on.
func TestRefreshIndexBestEffortWarnsInsteadOfFailingOnASymlinkedRouterState(t *testing.T) {
	cctx, log := symlinkedCommandContext(t, t.TempDir())

	refreshIndexBestEffort(context.Background(), cctx, "down")

	lines, _ := log.Snapshot()
	wantPrefix := "warn: Could not refresh the checkout index after down: "
	if len(lines) != 1 || !strings.HasPrefix(lines[0], wantPrefix) {
		t.Fatalf("lines = %v, want exactly one line starting %q", lines, wantPrefix)
	}
}
