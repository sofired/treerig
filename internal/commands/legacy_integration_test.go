//go:build integration

package commands_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/backend"
	"github.com/sofired/berth/internal/cli"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/run/osrun"
	"github.com/sofired/berth/internal/worktree"
)

// The clean break's promise: whatever a build from before this lock was
// ported to flock(2) left on this machine — a registration, a home, an
// index card, and a Compose project whose every resource carries its
// labels — this build's own `worktree remove` takes away completely,
// verified by those labels.

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

// legacyRegistration is what writeLegacyUp reports about the worktree it
// hand-registered: its durable identity, and the slug, domain and label
// namespace it wrote into the registry.
type legacyRegistration struct {
	RepositoryID, WorktreeID, Slug, Domain string
}

// legacyRecordJSON is one registry record in exactly the shape
// internal/registry writes and reads it (see registry/write.go's own,
// unexported recordJSON): version, camelCase keys, initialization last.
// Reproduced here rather than reused, since every writer that could build
// one for us (registry.Registry.Ensure, registry.Store.Ensure) also
// upgrades the control-home marker on its way through — exactly what
// writeLegacyUp exists to leave alone.
type legacyRecordJSON struct {
	Version        int     `json:"version"`
	RepositoryID   string  `json:"repositoryId"`
	WorktreeID     string  `json:"worktreeId"`
	Slug           string  `json:"slug"`
	Domain         string  `json:"domain"`
	LabelNamespace *string `json:"labelNamespace,omitempty"`
	Initialization string  `json:"initialization"`
}

// writeLegacyUp hand-writes exactly what a build from before this lock was
// ported left behind for one checkout, already holding a loadable
// berth.toml and Compose file: a control home whose marker is empty (what
// files.LegacyControlHome reads; a build with no such marker at all left it
// that way with its own touchSecure), that worktree's identity markers, its
// registry record in the one shape both the current and every earlier
// build read, and a running Compose project carrying every ownership label
// a real `up` would give it — brought up with no routes and no shared
// router, so this never touches a developer's real one.
//
// It never goes through commands.UpWithContext, registry.Registry.Ensure,
// registry.Store.Ensure, or files.SecureWorktreeDirectories's own caller in
// that path: every one of those calls establishControlHome, which upgrades
// an empty or missing marker to this build's own format as its first act
// (internal/files.go). SecureWorktreeDirectories is still used here, for
// every directory a worktree needs and its permissions, but the marker it
// leaves behind is reset to empty immediately after, undoing exactly that
// one upgrade.
//
// With the old build gone, this is the only place its historical on-disk
// shape still exists: hand-reproduced here rather than run through a
// second implementation, since there is no longer a real one to run.
// legacyRecordJSON's field order and tags, and the marker-reset step above,
// must be kept in step by hand with whatever internal/registry actually
// reads and writes if that shape is ever revisited — nothing checks the
// two still agree.
func writeLegacyUp(t *testing.T, controlHome, checkout string) legacyRegistration {
	t.Helper()
	ctx := context.Background()

	loaded, err := config.LoadNearest(checkout)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	labelNamespace := loaded.Config.LabelNamespace()

	identities := registry.NewIdentityService(checkout, osrun.New())
	identity, err := identities.Ensure(ctx, checkout)
	if err != nil {
		t.Fatalf("identity markers: %v", err)
	}

	worktreeLayout := layout.WorktreeLayout(controlHome, identity.RepositoryID, identity.WorktreeID)
	// registrytest.TempDir, not controlHome's own parent: AssertAdoptableControlHome's
	// home-family checks need some unrelated directory to compare the
	// control home against, never exercised by this fixture.
	if err := files.SecureWorktreeDirectories(worktreeLayout, registrytest.TempDir(t)); err != nil {
		t.Fatalf("secure worktree directories: %v", err)
	}
	markerPath := filepath.Join(controlHome, files.ControlHomeMarker)
	if err := os.WriteFile(markerPath, nil, 0o600); err != nil {
		t.Fatalf("reset control home marker to empty: %v", err)
	}

	slug, err := registry.AllocateSlug(*identity, loaded.Config.Project.Domain, nil, "")
	if err != nil {
		t.Fatalf("allocate slug: %v", err)
	}
	record := legacyRecordJSON{
		Version: 1, RepositoryID: identity.RepositoryID, WorktreeID: identity.WorktreeID,
		Slug: slug, Domain: loaded.Config.Project.Domain, LabelNamespace: &labelNamespace,
		Initialization: string(registry.Complete),
	}
	if err := files.AtomicJSON(worktreeLayout.RecordPath, record); err != nil {
		t.Fatalf("write registry record: %v", err)
	}

	ref := worktree.Ref{RepositoryID: identity.RepositoryID, WorktreeID: identity.WorktreeID, Slug: slug}
	runtime := backend.NewCompose(backend.Options{
		ComposeFile:    filepath.Join(checkout, loaded.Config.Compose.File),
		ProjectRoot:    loaded.Root,
		ConfigFile:     loaded.File,
		ProjectName:    loaded.Config.Project.Name,
		Domain:         loaded.Config.Project.Domain,
		ControlHome:    controlHome,
		LabelNamespace: labelNamespace,
		Runner:         osrun.New(),
		Logger:         logger.Silent{},
		Environ:        os.Environ(),
		// No routes and a network name nothing joins: a legacy build's own
		// worktree never touched the shared router, and overrideDocument
		// only reaches RouterNetwork once Routes is non-empty.
		RouterNetwork: "unused",
	})
	// Registered before anything reaches Docker, so an `up` that fails
	// halfway leaves nothing behind on the daemon: everything this fixture
	// creates carries its own worktree label, and only that is swept.
	owned := "label=" + labelNamespace + ".worktree-uuid=" + identity.WorktreeID
	t.Cleanup(func() {
		for _, id := range dockerIDs(t, "ps", "-aq", "--filter", owned) {
			_ = exec.Command("docker", "rm", "--force", id).Run()
		}
		for _, id := range dockerIDs(t, "volume", "ls", "-q", "--filter", owned) {
			_ = exec.Command("docker", "volume", "rm", id).Run()
		}
		for _, id := range dockerIDs(t, "network", "ls", "-q", "--filter", owned) {
			_ = exec.Command("docker", "network", "rm", id).Run()
		}
	})
	if err := runtime.Up(ctx, ref, nil); err != nil {
		t.Fatalf("compose up: %v", err)
	}

	return legacyRegistration{
		RepositoryID: identity.RepositoryID, WorktreeID: identity.WorktreeID, Slug: slug, Domain: loaded.Config.Project.Domain,
	}
}

// TestWorktreeRemoveTearsDownAnOldControlHomeByOwnership proves this build's
// clean break with what came before it: a control home a pre-flock build
// left behind — its registration, home, identity marker, and a Compose
// project whose resources carry its ownership labels — is torn down
// completely by `worktree remove`, found and removed by those labels alone,
// never by guessing at a shape only this build ever wrote.
func TestWorktreeRemoveTearsDownAnOldControlHomeByOwnership(t *testing.T) {
	dockerAvailable(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	controlHome := filepath.Join(base, "control")
	checkout := filepath.Join(base, "legacy")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[project]\nname = \"legacy\"\ndomain = \"legacy.localhost\"\nlabel-namespace = \"" + legacyNamespace + "\"\n" +
		"[compose]\nfile = \"compose.yaml\"\n"
	compose := "services:\n  api:\n    image: busybox:1.37.0\n    command: [\"sleep\", \"600\"]\n    volumes: [\"data:/data\"]\nvolumes:\n  data: {}\n"
	for name, contents := range map[string]string{"berth.toml": cfg, "compose.yaml": compose} {
		if err := os.WriteFile(filepath.Join(checkout, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, checkout, "init", "--initial-branch=main", ".")
	gitIn(t, checkout, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--allow-empty", "-m", "initial")

	// Up, by hand, the way a pre-flock build of berth would have left it.
	registered := writeLegacyUp(t, controlHome, checkout)
	// writeLegacyUp registered the Docker cleanup itself, before its `up`.
	owned := []string{"label=" + legacyNamespace + ".worktree-uuid=" + registered.WorktreeID}
	if running := dockerIDs(t, "ps", "-q", "--filter", owned[0]); len(running) != 1 {
		t.Fatalf("writeLegacyUp left %d running containers, want 1", len(running))
	}
	identityMarker := filepath.Join(checkout, ".git", "berth", "worktree-id")
	if found, _ := files.PathExists(identityMarker); !found {
		t.Fatalf("writeLegacyUp left no identity marker at %s", identityMarker)
	}
	if legacy, err := files.LegacyControlHome(controlHome); err != nil || !legacy {
		t.Fatalf("the hand-written control home is not recognised as pre-flock: %v, %v", legacy, err)
	}

	// Remove, by this build. The router state directory and port are
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
	// BERTH_CONTROL_HOME, not BERTH_HOME: a pre-flock build's own BERTH_HOME
	// was the control home directly, and controlHome is exactly where
	// writeLegacyUp wrote everything above — this build's own BERTH_HOME
	// would instead derive <controlHome>/control-home (#80), missing it
	// entirely. Pointing BERTH_CONTROL_HOME straight at it is how someone
	// adopting an existing pre-flock control home into this build points at
	// it too.
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
		t.Fatalf("control home marker = %q, %v; want this build's own format written", marker, err)
	}

	// And this build's own registry agrees nothing is registered.
	scan, err := registry.ScanRegistrations(controlHome)
	if err != nil {
		t.Fatalf("scan registrations: %v", err)
	}
	if len(scan.Records) != 0 {
		t.Fatalf("registry scan = %+v after removal, want none", scan.Records)
	}
}
