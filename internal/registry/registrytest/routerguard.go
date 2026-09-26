package registrytest

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/layout"
)

// defaultRouterProject mirrors internal/router's own DefaultName
// (brand.Name + "-router") — a local, decoupled copy, the same pattern
// internal/backend's routedByLabel already uses to avoid a dependency
// between two packages that only need to agree on one literal, not on
// internal/router's whole API.
const defaultRouterProject = brand.Name + "-router"

// RequireIsolatedRouterState fails t immediately if getenv would resolve
// the same router state directory (internal/layout.RouterState) as the
// real, unmodified process environment does — the machine-wide directory a
// real `berth router up` on this account uses
// ($XDG_STATE_HOME/berth/router, or $HOME/.local/state/berth/router when
// that is unset, on Linux; $HOME/Library/Application Support/berth/router
// on macOS).
//
// Every integration test that builds a CommandContext or spawns the real
// binary for a verb that can reach the shared router (up, down, worktree
// remove, router up/down, doctor) must override enough of HOME,
// XDG_STATE_HOME and BERTH_ROUTER_PORT that the two can never coincide.
// This is the safety net for the day one forgets to: issue #55 found the
// hard way that a getenv overriding only BERTH_HOME let `worktree remove`
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
				"override HOME (or XDG_STATE_HOME) — and BERTH_ROUTER_PORT, if the test can reach a real router — "+
				"so the two never coincide",
			testState.StateDir)
	}
}

// RequireNoRealRouter fails t immediately if any container on this
// machine's Docker daemon belongs to Compose project "berth-router" — the
// shared router's own default project name (internal/router's DefaultName),
// which a test reaching the unmodified cli.Main entry point has no way to
// override: unlike a CommandContext a test builds directly, cli.Main
// exposes no ProjectName hook at all, so a test that goes through it (a
// spawned real binary, or cli.Main called in-process) can be isolated by
// router *state* (RequireIsolatedRouterState) but not by router *project*.
// This is that residual gap's own guard, read-only
// (`docker ps --filter label=com.docker.compose.project=berth-router`,
// never a write) and judged acceptable rather than fixed outright: see
// legacy_integration_test.go's own comment on why. A daemon this cannot
// even ask (docker missing, unreachable) is not itself a reason to fail —
// the test almost certainly needs Docker for other reasons and will find
// out from those instead — unless BERTH_TEST_REQUIRE_DOCKER=1 says Docker
// must answer, as in CI: there a failed query means this guard could not
// do its job, so it fails rather than let the test go ahead unchecked.
func RequireNoRealRouter(t testing.TB) {
	t.Helper()
	output, err := exec.Command("docker", "ps",
		"--filter", "label=com.docker.compose.project="+defaultRouterProject,
		"--format", "{{.ID}}",
	).Output()
	if err != nil {
		if os.Getenv("BERTH_TEST_REQUIRE_DOCKER") == "1" {
			t.Fatalf("could not ask Docker whether a real shared router (Compose project %q) is running: %v", defaultRouterProject, err)
		}
		return
	}
	if ids := strings.Fields(string(output)); len(ids) > 0 {
		t.Fatalf(
			"a real shared router is running on this daemon (Compose project %q, %d container(s)); "+
				"this test must run on a machine with no live shared router",
			defaultRouterProject, len(ids))
	}
}
