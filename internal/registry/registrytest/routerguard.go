package registrytest

import (
	"os"
	"testing"

	"github.com/sofired/treerig/internal/layout"
)

// RequireIsolatedRouterState fails t immediately if getenv would resolve
// the same router state directory (internal/layout.RouterState) as the
// real, unmodified process environment does — the treerig root a real
// `treerig router up` on this account uses (`$TREERIG_HOME/router`, or
// `~/.treerig/router` when `TREERIG_HOME` is unset).
//
// Every integration test that builds a CommandContext or spawns the real
// binary for a verb that can reach the shared router (up, down, worktree
// remove, router up/down, doctor) must override `TREERIG_HOME` (as of #80,
// the router's state follows the treerig root alone — `HOME` and
// `XDG_STATE_HOME` no longer matter to it) and `TREERIG_ROUTER_PORT` that the
// two can never coincide. This is the safety net for the day one forgets
// to: issue #55 found the hard way that a getenv overriding only
// `TREERIG_HOME` — before #80, when the router's state followed a fixed,
// per-account XDG/Application Support path instead — let `worktree remove`
// reach the real router's machine-wide lock and rewrite its real index
// page, with no Docker-resource check (container/network/volume counts)
// able to notice, since the damage was a plain file write, not a Docker
// call. Call this right after building the getenv a test will use, before
// running anything through it.
func RequireIsolatedRouterState(t testing.TB, getenv func(string) (string, bool)) {
	t.Helper()
	testState, err := layout.RouterState(getenv)
	if err != nil {
		// Can't be resolved at all, so it certainly isn't the real one.
		return
	}
	realState, err := layout.RouterState(func(key string) (string, bool) { return os.LookupEnv(key) })
	if err != nil {
		return
	}
	if testState.StateDir == realState.StateDir {
		t.Fatalf(
			"this test's router state directory (%s) is the real one this machine's own account would use; "+
				"override TREERIG_HOME — and TREERIG_ROUTER_PORT, if the test can reach a real router — "+
				"so the two never coincide",
			testState.StateDir)
	}
}
