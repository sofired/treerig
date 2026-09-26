//go:build integration

package commands_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/cli"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry/registrytest"
)

// The clean break's promise: whatever the TypeScript build left on this
// machine — a registration, a home, an index card, and a Compose project
// whose every resource carries its labels — the Go build's own `worktree
// remove` takes away completely, verified by those labels.

const legacyNamespace = "dev.berth-test-legacy"

// dockerAvailable skips the test when no Docker daemon answers, unless
// BERTH_TEST_REQUIRE_DOCKER=1 makes its absence a failure instead.
func dockerAvailable(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("BERTH_TEST_REQUIRE_DOCKER") == "1" {
			t.Fatalf("docker is required (BERTH_TEST_REQUIRE_DOCKER=1) but unavailable: %v", err)
		}
		t.Skip("docker is not available")
	}
}

// repoRoot returns the repository root, found relative to this test file
// rather than the working directory `go test` was invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// node locates a Node 24 binary to run the TypeScript build under test,
// preferring PATH and falling back to the pinned install location; it skips
// the test when neither is found.
func node(t *testing.T) string {
	t.Helper()
	if found, err := exec.LookPath("node"); err == nil {
		return found
	}
	home, _ := os.UserHomeDir()
	candidate := filepath.Join(home, ".local", "opt", "node-v24.14.1-linux-x64", "bin", "node")
	if _, err := os.Stat(candidate); err != nil {
		t.Skip("node 24 is not available to run the TypeScript build")
	}
	return candidate
}

// gitIn runs git with args in dir, isolated from any user or system Git
// config, and fails the test if it exits non-zero.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

// dockerIDs runs docker with args and splits its output into whitespace-
// separated fields, the shape `docker ps -q` and similar id-listing calls
// return.
func dockerIDs(t *testing.T, args ...string) []string {
	t.Helper()
	output, err := exec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("docker %v: %v", args, err)
	}
	return strings.Fields(string(output))
}

func TestTypeScriptWritesGoRemoves(t *testing.T) {
	dockerAvailable(t)
	nodeBin := node(t)
	root := repoRoot(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	controlHome := filepath.Join(base, "control")
	checkout := filepath.Join(base, "legacy")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	config := "[project]\nname = \"legacy\"\ndomain = \"legacy.localhost\"\nlabel-namespace = \"" + legacyNamespace + "\"\n" +
		"[compose]\nfile = \"compose.yaml\"\n"
	compose := "services:\n  api:\n    image: busybox:1.37.0\n    command: [\"sleep\", \"600\"]\n    volumes: [\"data:/data\"]\nvolumes:\n  data: {}\n"
	for name, contents := range map[string]string{"berth.toml": config, "compose.yaml": compose} {
		if err := os.WriteFile(filepath.Join(checkout, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, checkout, "init", "--initial-branch=main", ".")
	gitIn(t, checkout, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--allow-empty", "-m", "initial")

	// Up, by the TypeScript build.
	up := exec.Command(nodeBin, filepath.Join(root, "tools", "oracle", "legacy-up.ts"), controlHome, checkout)
	up.Dir = root
	up.Env = append(os.Environ(), "BERTH_HOME="+controlHome)
	var upOut, upErr bytes.Buffer
	up.Stdout, up.Stderr = &upOut, &upErr
	if err := up.Run(); err != nil {
		t.Fatalf("legacy up: %v\n%s", err, upErr.String())
	}
	var registered struct{ RepositoryID, WorktreeID, Slug string }
	if err := json.Unmarshal(upOut.Bytes(), &registered); err != nil {
		t.Fatalf("legacy up said %q: %v", upOut.String(), err)
	}
	owned := []string{"label=" + legacyNamespace + ".worktree-uuid=" + registered.WorktreeID}
	t.Cleanup(func() {
		// Whatever the test left, by the labels this test's namespace alone carries.
		for _, id := range dockerIDs(t, "ps", "-aq", "--filter", owned[0]) {
			_ = exec.Command("docker", "rm", "--force", id).Run()
		}
		for _, id := range dockerIDs(t, "volume", "ls", "-q", "--filter", owned[0]) {
			_ = exec.Command("docker", "volume", "rm", id).Run()
		}
		for _, id := range dockerIDs(t, "network", "ls", "-q", "--filter", owned[0]) {
			_ = exec.Command("docker", "network", "rm", id).Run()
		}
	})
	if running := dockerIDs(t, "ps", "-q", "--filter", owned[0]); len(running) != 1 {
		t.Fatalf("the TypeScript build left %d running containers, want 1", len(running))
	}
	identityMarker := filepath.Join(checkout, ".git", "berth", "worktree-id")
	if found, _ := files.PathExists(identityMarker); !found {
		t.Fatalf("the TypeScript build left no identity marker at %s", identityMarker)
	}
	if legacy, err := files.LegacyControlHome(controlHome); err != nil || !legacy {
		t.Fatalf("the TypeScript build's control home is not recognised as its: %v, %v", legacy, err)
	}

	// Remove, by the Go build. The router state directory and port are
	// isolated the same way newWalk (up_docker_integration_test.go) isolates
	// its own: `worktree remove` calls unrouteWorktree, which reaches the
	// shared router's machine-wide state (internal/layout.RouterState) and
	// best-effort refreshes its index page — neither of which cli.Main lets
	// a test override with a commands.SharedRouter the way a CommandContext
	// built directly can (up_docker_integration_test.go's own path), so a
	// getenv that forwarded HOME/XDG_STATE_HOME/BERTH_ROUTER_PORT to the
	// real environment reached the real router: its machine-wide lock and
	// index page were genuinely taken and rewritten by an early version of
	// this test. RequireIsolatedRouterState is the safety net in case this
	// ever regresses again.
	previous, _ := os.Getwd()
	if err := os.Chdir(checkout); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(previous)
	// BERTH_CONTROL_HOME, not BERTH_HOME: the TypeScript build's BERTH_HOME
	// was the control home directly, and controlHome is exactly where it
	// wrote everything above — the Go build's own BERTH_HOME would instead
	// derive <controlHome>/control-home (#80), missing it entirely. Pointing
	// BERTH_CONTROL_HOME straight at it is how someone adopting an existing
	// TypeScript-era control home into the Go build points at it too.
	//
	// BERTH_HOME still needs isolating on its own: the router's state now
	// lives there (#80), and worktree remove's own best-effort index
	// refresh takes the router's lock and writes its page, so leaving
	// BERTH_HOME unset here would fall through to this developer's real
	// $HOME/.berth/router — touching their real router's lock and index
	// page from a test. Pointed at this test's own base instead, under a
	// directory of its own, distinct from goHome (below): BERTH_HOME and
	// HOME resolving to the very same directory would itself trip
	// files.AssertSafeHome's "it is your home directory" refusal.
	// BERTH_ROUTER_PORT isolates the port the same way
	// up_docker_integration_test.go's own newWalk does, since the router's
	// state directory alone does not stop this test's own router from
	// binding the real one's loopback port (#55).
	goHome := filepath.Join(base, "go-home")
	goBerthHome := filepath.Join(base, "go-berth-home")
	routerPort := freePort(t)
	getenv := func(key string) (string, bool) {
		switch key {
		case "BERTH_CONTROL_HOME":
			return controlHome, true
		case "BERTH_HOME":
			return goBerthHome, true
		case "HOME":
			return goHome, true
		case "BERTH_ROUTER_PORT":
			return strconv.Itoa(routerPort), true
		}
		return "", false
	}
	registrytest.RequireIsolatedRouterState(t, getenv)
	// Round 2 of #55's review: even with the router *state* isolated above,
	// `worktree remove` going through cli.Main still reaches the real
	// Docker daemon's own "berth-router" project — the shared router's
	// default Compose project name, which cli.Main exposes no hook to
	// override (unlike a CommandContext built directly, which can set
	// commands.SharedRouter to a router of its own name, the way
	// up_docker_integration_test.go's newWalk does). unrouteWorktree's own
	// DisconnectNetwork therefore runs `docker compose --project-name
	// berth-router ps …` against whatever is really running under that
	// name on this machine. Judged an accepted gap rather than fixed
	// outright: fixing it properly needs either a new env hook on cli.Main
	// (which the round-2 review explicitly asked not to add) or a
	// dedicated ProjectName-injection seam on the entry point itself,
	// neither small. RequireNoRealRouter is the guard in its place: a
	// read-only check that no container is actually running under that
	// project, so this test fails loudly instead of silently touching a
	// real router if one exists.
	registrytest.RequireNoRealRouter(t)
	var stdout, stderr bytes.Buffer
	code := cli.Main([]string{"berth", "worktree", "remove", "--yes"}, strings.NewReader(""), &stdout, &stderr, getenv)
	if code != 0 {
		t.Fatalf("go remove exited %d:\n%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Removed main.legacy.localhost: 1 container, 1 volume, 1 network, home directory, registration, identity marker.") {
		t.Fatalf("stderr = %s", stderr.String())
	}

	for _, kind := range [][]string{{"ps", "-aq"}, {"volume", "ls", "-q"}, {"network", "ls", "-q"}, {"image", "ls", "-q"}} {
		if left := dockerIDs(t, append(kind, "--filter", owned[0])...); len(left) != 0 {
			t.Fatalf("%v left behind: %v", kind, left)
		}
	}
	paths := layout.WorktreeLayout(controlHome, registered.RepositoryID, registered.WorktreeID)
	for _, path := range []string{paths.Home, paths.QuarantinePath, paths.RecordPath, identityMarker} {
		if found, _ := files.PathExists(path); found {
			t.Fatalf("%s survived", path)
		}
	}
	marker, err := os.ReadFile(filepath.Join(controlHome, files.ControlHomeMarker))
	if err != nil || string(marker) != files.ControlHomeFormat {
		t.Fatalf("control home marker = %q, %v; want the Go build's format written", marker, err)
	}

	// And the TypeScript build agrees nothing is registered.
	list := exec.Command(nodeBin, filepath.Join(root, "src", "cli", "main.ts"), "worktree", "list", "--all", "--json")
	list.Dir = checkout
	list.Env = append(os.Environ(), "BERTH_HOME="+controlHome)
	output, err := list.Output()
	if err != nil || strings.TrimSpace(string(output)) != "[]" {
		t.Fatalf("typescript list: %q, %v", output, err)
	}
}
