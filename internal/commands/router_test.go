package commands_test

// Wiring checks for the router verbs: the right project, port and state
// directory reach router.New, a verified `up` reports where it is up, and
// `status`/`down` reach the router package correctly. internal/router's own
// suite covers Ensure/Verify/Status/Down's full behaviour; this package only
// has its own seam (Env, not CommandContext — the router needs no project
// config) to get wrong.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/router"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/run/fakerun"
)

// routerEnvGetenv is a layout.Getenv backed by a fixed map, "ok" following
// whether the key is present the way os.LookupEnv reports a variable that
// was set (even to "").
func routerEnvGetenv(values map[string]string) layout.Getenv {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

// sliceContainsArg reports whether want is one of args.
func sliceContainsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// healthyRouterDocker answers as though the shared router were already up
// and healthy, on a native Linux engine, so Ensure takes its fast path: this
// package's own tests only check wiring (the state directory, port, mode and
// ports reaching router.New, and a verified up reporting where it is up),
// not Ensure's full behaviour, which internal/router's suite covers.
type healthyRouterDocker struct {
	mu   sync.Mutex
	port int
	// mode is the mode label the router container carries; "host" when empty.
	mode string
	// unhealthy makes the router container report an unhealthy state.
	unhealthy bool
	// home is stamped as RouterHomeLabel on every container docFor answers,
	// so Owner/checkOwnership see this router as owned by whatever treerig
	// root the test itself resolved, rather than refusing it as foreign.
	home string
}

// owned makes the containers this fake answers carry home as their owner,
// and gives them port 80 unless a test chose one.
func (d *healthyRouterDocker) owned(home string) {
	d.home = home
	if d.port == 0 {
		d.port = 80
	}
}

// Which always answers true.
func (d *healthyRouterDocker) Which(string) bool { return true }

// nativeLinuxInfo is `docker info --format {{json .}}` as a native, rootful
// Linux engine reports the fields mode detection reads.
const nativeLinuxInfo = `{"Name":"cc-worker","OperatingSystem":"Ubuntu 24.04.4 LTS","OSType":"linux",` +
	`"SecurityOptions":["name=apparmor","name=seccomp,profile=builtin","name=cgroupns"]}`

// Run answers as a healthy router on a native Linux engine.
func (d *healthyRouterDocker) Run(_ context.Context, spec run.Spec) (run.Result, error) {
	args := spec.Args
	switch {
	case len(args) > 0 && args[0] == "info":
		return run.Result{Stdout: nativeLinuxInfo}, nil
	case len(args) > 1 && args[0] == "context" && args[1] == "inspect":
		return run.Result{Stdout: "unix:///var/run/docker.sock\n"}, nil
	case len(args) > 0 && args[0] == "compose" && sliceContainsArg(args, "ps"):
		return run.Result{Stdout: "c-" + args[len(args)-1]}, nil
	case len(args) > 0 && args[0] == "inspect":
		d.mu.Lock()
		doc := d.docFor(strings.TrimPrefix(args[1], "c-"))
		d.mu.Unlock()
		encoded, err := json.Marshal([]router.ContainerInspect{doc})
		if err != nil {
			return run.Result{}, err
		}
		return run.Result{Stdout: string(encoded)}, nil
	default:
		return run.Result{}, nil
	}
}

// docFor is what `docker inspect` answers for one running service container.
func (d *healthyRouterDocker) docFor(service string) router.ContainerInspect {
	labels := map[string]string{
		"com.docker.compose.project":  router.DefaultName,
		"com.docker.compose.service":  service,
		"dev.treerig.component":       service,
		"dev.treerig.router-protocol": strconv.Itoa(router.Protocol),
		router.RouterHomeLabel:        d.home,
	}
	health := "healthy"
	if d.unhealthy && service == router.RouterService {
		health = "unhealthy"
	}
	state := router.InspectState{Status: "running", Health: router.InspectHealth{Status: health}}
	if service == router.RouterService {
		mode := d.mode
		if mode == "" {
			mode = string(router.ModeHost)
		}
		labels[router.RouterModeLabel] = mode
		labels[router.RouterPortLabel] = strconv.Itoa(d.port)
	}
	return router.ContainerInspect{Config: router.InspectConfig{Labels: labels}, State: state}
}

// scratchRouterEnv is an Env over a scratch HOME whose treerig root holds a
// settings.toml with the test port range plus extra, a fake probe that binds
// nothing, and Linux as the client OS.
func scratchRouterEnv(t *testing.T, extra string, runner run.Runner) (*commands.Env, layout.RouterLayout, *logger.Recording) {
	t.Helper()
	home := registrytest.TempDir(t)
	controlHome := registrytest.TempDir(t)
	getenv := routerEnvGetenv(map[string]string{"HOME": home, "TREERIG_CONTROL_HOME": controlHome})
	treerigRoot, err := layout.ResolveHome(getenv, "")
	if err != nil {
		t.Fatal(err)
	}
	registrytest.WriteScratchSettings(t, treerigRoot, extra)
	state := layout.RouterStateFrom(treerigRoot)
	if docker, ok := runner.(interface{ owned(home string) }); ok {
		docker.owned(treerigRoot)
	}
	rec := &logger.Recording{}
	env := &commands.Env{
		CommandName: "treerig", Invocation: "treerig router up", Logger: rec, Getenv: getenv, Cwd: registrytest.TempDir(t),
		Runner: runner, OS: "linux", PortProbe: registrytest.FreeProbe{},
	}
	return env, state, rec
}

func TestRouterUp(t *testing.T) {
	t.Run("starts the router in the mode Docker reports and says where it is up", func(t *testing.T) {
		env, state, rec := scratchRouterEnv(t, "", &healthyRouterDocker{})

		if err := commands.RouterUp(context.Background(), env); err != nil {
			t.Fatalf("RouterUp: %v", err)
		}

		lines, _ := rec.Snapshot()
		want := "info: Router is up on 127.0.0.1:80 (host mode); index at " + router.IndexURL(80) + "."
		if !slicesContainsLine(lines, want) {
			t.Errorf("expected %q, got %v", want, lines)
		}
		stack, err := os.ReadFile(state.ComposeFile)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(stack), `"network_mode": "host"`) {
			t.Errorf("expected a host-mode stack, got:\n%s", stack)
		}
		saved, found, err := router.ReadState(state)
		if err != nil || !found {
			t.Fatalf("router state: found %v, err %v", found, err)
		}
		for _, port := range []int{saved.IndexPort, saved.PingPort} {
			if !registrytest.PortRange.Contains(port) {
				t.Errorf("port %d is outside the test range %s", port, registrytest.PortRange)
			}
		}
	})

	t.Run("passes settings.toml's [router] mode through to the stack", func(t *testing.T) {
		env, state, rec := scratchRouterEnv(t, "[router]\nmode = \"bridge\"\n", &healthyRouterDocker{mode: "bridge"})

		if err := commands.RouterUp(context.Background(), env); err != nil {
			t.Fatalf("RouterUp: %v", err)
		}

		stack, err := os.ReadFile(state.ComposeFile)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`"127.0.0.1:80:80"`, `treerig-loopback:host-gateway`} {
			if !strings.Contains(string(stack), want) {
				t.Errorf("expected %s in the bridge-mode stack, got:\n%s", want, stack)
			}
		}
		if strings.Contains(string(stack), `"network_mode"`) {
			t.Errorf("a bridge-mode stack must not set network_mode:\n%s", stack)
		}
		lines, _ := rec.Snapshot()
		if want := "info: Router is up on 127.0.0.1:80 (bridge mode); index at " + router.IndexURL(80) + "."; !slicesContainsLine(lines, want) {
			t.Errorf("expected %q, got %v", want, lines)
		}
		saved, _, err := router.ReadState(state)
		if err != nil {
			t.Fatal(err)
		}
		if saved.PingPort != 0 {
			t.Errorf("bridge mode allocates no ping port, got %d", saved.PingPort)
		}
	})

	t.Run("rejects a malformed [router] mode, naming the settings file", func(t *testing.T) {
		env, _, _ := scratchRouterEnv(t, "[router]\nmode = \"hybrid\"\n", &healthyRouterDocker{})

		err := commands.RouterUp(context.Background(), env)
		if err == nil {
			t.Fatal("expected a refusal")
		}
		for _, want := range []string{"settings.toml", "router.mode", `"host" or "bridge"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q lacks %q", err, want)
			}
		}
	})

	t.Run("refuses a daemon it cannot route through, with the reason", func(t *testing.T) {
		env, _, _ := scratchRouterEnv(t, "", &rootlessDocker{})

		err := commands.RouterUp(context.Background(), env)
		if err == nil || !strings.Contains(err.Error(), "Rootless Docker is not supported") {
			t.Fatalf("expected the rootless refusal, got %v", err)
		}
	})

	// #15: `router up` never regenerated the real page — Ensure's own write()
	// writes the placeholder only when the index file is missing outright,
	// for the index container's health check, and nothing after Ensure ever
	// replaced it. Deleting the page and running `router up` again must show
	// every checkout again, not leave the placeholder in place.
	t.Run("refreshes a missing index page once the router has started, showing every checkout again", func(t *testing.T) {
		env, state, rec := scratchRouterEnv(t, "", &healthyRouterDocker{})
		controlHome, _ := env.Getenv("TREERIG_CONTROL_HOME")
		if _, err := registrytest.WriteRecord(controlHome, registrytest.RecordJSON{
			Version: 1, RepositoryID: "11111111-1111-4111-8111-111111111111",
			WorktreeID: "22222222-2222-4222-8222-222222222221",
			Slug:       "feature-a", Domain: "acme.localhost", Initialization: "complete",
		}); err != nil {
			t.Fatal(err)
		}
		_ = rec

		if err := commands.RouterUp(context.Background(), env); err != nil {
			t.Fatalf("RouterUp: %v", err)
		}
		if err := os.Remove(state.IndexFile); err != nil {
			t.Fatalf("removing the index page: %v", err)
		}

		if err := commands.RouterUp(context.Background(), env); err != nil {
			t.Fatalf("second RouterUp: %v", err)
		}

		page, err := os.ReadFile(state.IndexFile)
		if err != nil {
			t.Fatalf("reading the regenerated index page: %v", err)
		}
		if !strings.Contains(string(page), "feature-a.acme.localhost") {
			t.Errorf("expected the regenerated page to list feature-a.acme.localhost, got:\n%s", page)
		}
	})

	t.Run("refuses a symlinked state directory before the lock or the stack files ever touch its target", func(t *testing.T) {
		// Resolved through layout.RouterStateDir itself — production's own
		// function, fed the very env RouterUp will resolve TREERIG_HOME
		// through — so the symlink lands exactly where RouterUp will look,
		// under the treerig root (#80) rather than a hand-built guess.
		// RouterUp must refuse before commands.routerHold ever takes the
		// router lock (which would create <target>/lock and its holder
		// record) or Ensure's own write() ever writes a stack file into it.
		target := registrytest.TempDir(t)
		fakeHome := registrytest.TempDir(t)
		treerigHome := registrytest.TempDir(t)
		getenv := routerEnvGetenv(map[string]string{"HOME": fakeHome, "TREERIG_HOME": treerigHome})
		plantedStateDir, err := layout.RouterStateDir(getenv)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(plantedStateDir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, plantedStateDir); err != nil {
			t.Fatal(err)
		}

		rec := &logger.Recording{}
		env := &commands.Env{
			CommandName: "treerig", Invocation: "treerig router up", Logger: rec, Getenv: getenv,
			Runner: &healthyRouterDocker{port: 80},
		}

		err = commands.RouterUp(context.Background(), env)
		if err == nil {
			t.Fatal("expected RouterUp to refuse a symlinked state directory")
		}
		// Matched by message, not merely non-nil: Ensure can fail for other
		// reasons too (the fake docker above carries no state, so a router
		// that got past secureState would fail verification instead), and
		// only this message proves the symlink is what stopped it.
		const wantRefusal = "Refusing symlinked or non-directory managed path"
		if !strings.Contains(err.Error(), wantRefusal) {
			t.Fatalf("expected the symlink refusal (%q), got: %v", wantRefusal, err)
		}

		entries, err := os.ReadDir(target)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("expected nothing created in the symlink's target — no lock, no protocol record — got %v", entries)
		}
	})
}

func TestRouterStatus(t *testing.T) {
	t.Run("prints the report and signals failure without an error, when Docker cannot be asked", func(t *testing.T) {
		home := registrytest.TempDir(t)
		getenv := routerEnvGetenv(map[string]string{"HOME": home})
		rec := &logger.Recording{}
		fake := &fakerun.Runner{Handler: func(_ context.Context, spec run.Spec) (run.Result, error) {
			return run.Result{}, &run.StartError{Command: spec.Command, Code: "ENOENT", Detail: "is not on PATH (ENOENT).", ExitCode: 127}
		}}
		env := &commands.Env{CommandName: "treerig", Logger: rec, Getenv: getenv, Runner: fake}

		failed, err := commands.RouterStatus(context.Background(), env)
		if err != nil {
			t.Fatalf("RouterStatus: %v", err)
		}
		if !failed {
			t.Error("expected failed = true")
		}
		lines, printed := rec.Snapshot()
		if len(printed) != 0 {
			t.Errorf("expected nothing on stdout, got %v", printed)
		}
		if !containsSubstringLine(lines, "Router state unknown") {
			t.Errorf("expected a line naming Router state unknown, got %v", lines)
		}
	})
}

func TestRouterStatusRunning(t *testing.T) {
	t.Run("says the mode and port the running router's container reports", func(t *testing.T) {
		env, _, _ := scratchRouterEnv(t, "", &healthyRouterDocker{mode: "bridge", port: 8080})

		failed, err := commands.RouterStatus(context.Background(), env)
		if err != nil || failed {
			t.Fatalf("RouterStatus: failed %v, err %v", failed, err)
		}
		_, printed := env.Logger.(*logger.Recording).Snapshot()
		if len(printed) == 0 || printed[0] != "running on 127.0.0.1:8080, bridge mode" {
			t.Fatalf("stdout = %q", printed)
		}
	})

	t.Run("reports an unhealthy router as degraded and fails", func(t *testing.T) {
		env, _, _ := scratchRouterEnv(t, "", &healthyRouterDocker{unhealthy: true})

		failed, err := commands.RouterStatus(context.Background(), env)
		if err != nil || !failed {
			t.Fatalf("RouterStatus: failed %v, err %v", failed, err)
		}
		lines, printed := env.Logger.(*logger.Recording).Snapshot()
		if len(printed) == 0 || printed[0] != "degraded on 127.0.0.1:80, host mode (1 problem(s) reported on stderr)" {
			t.Fatalf("stdout = %q", printed)
		}
		if !containsSubstringLine(lines, "Router problem: router: not healthy") {
			t.Fatalf("stderr = %q", lines)
		}
	})
}

// rootlessDocker is healthyRouterDocker on a rootless engine.
type rootlessDocker struct{ healthyRouterDocker }

// Run answers `docker info` as a rootless engine and everything else as
// healthyRouterDocker does.
func (d *rootlessDocker) Run(ctx context.Context, spec run.Spec) (run.Result, error) {
	if len(spec.Args) > 0 && spec.Args[0] == "info" {
		return run.Result{Stdout: `{"Name":"cc-worker","OperatingSystem":"Ubuntu 24.04.4 LTS","OSType":"linux","SecurityOptions":["name=seccomp,profile=builtin","name=rootless"]}`}, nil
	}
	return d.healthyRouterDocker.Run(ctx, spec)
}

// TestRouterStatusPortLine is #79: beside its status line, `router status`
// says on stdout which port it resolved and where that came from.
func TestRouterStatusPortLine(t *testing.T) {
	t.Run("says where the port came from, on stdout with the status line", func(t *testing.T) {
		home := registrytest.TempDir(t)
		getenv := routerEnvGetenv(map[string]string{"HOME": home})
		root := filepath.Join(home, ".treerig")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "settings.toml"), []byte("[router]\nport = 8080\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		rec := &logger.Recording{}
		// A runner that answers every call with empty success: no container
		// is listed, so the router reads as reachable but not running.
		env := &commands.Env{CommandName: "treerig", Logger: rec, Getenv: getenv, Runner: &fakerun.Runner{}}

		failed, err := commands.RouterStatus(context.Background(), env)
		if err != nil {
			t.Fatalf("RouterStatus: %v", err)
		}
		if failed {
			t.Error("expected failed = false")
		}
		_, printed := rec.Snapshot()
		want := []string{
			"not running (would use 127.0.0.1:8080)",
			"port 8080 from " + filepath.Join(root, "settings.toml"),
		}
		if len(printed) != len(want) || printed[0] != want[0] || printed[1] != want[1] {
			t.Fatalf("stdout = %q, want %q", printed, want)
		}
	})
}

func TestRouterDown(t *testing.T) {
	t.Run("stops the router by project name alone", func(t *testing.T) {
		home := registrytest.TempDir(t)
		getenv := routerEnvGetenv(map[string]string{"HOME": home})
		fake := &fakerun.Runner{}
		env := &commands.Env{CommandName: "treerig", Logger: &logger.Recording{}, Getenv: getenv, Runner: fake}

		if err := commands.RouterDown(context.Background(), env); err != nil {
			t.Fatalf("RouterDown: %v", err)
		}
		found := false
		for _, line := range fake.Argv() {
			if strings.Contains(line, "compose --project-name "+router.DefaultName+" down --remove-orphans") {
				found = true
			}
		}
		if !found {
			t.Errorf("expected a down call, got %v", fake.Argv())
		}
	})

	t.Run("stops the router even when settings.toml is malformed: down needs no port", func(t *testing.T) {
		home := registrytest.TempDir(t)
		root := filepath.Join(home, ".treerig")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "settings.toml"), []byte("[router]\nport = \"eighty\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		getenv := routerEnvGetenv(map[string]string{"HOME": home, "TREERIG_ROUTER_PORT": "not-a-port"})
		fake := &fakerun.Runner{}
		env := &commands.Env{CommandName: "treerig", Logger: &logger.Recording{}, Getenv: getenv, Runner: fake}

		if err := commands.RouterDown(context.Background(), env); err != nil {
			t.Fatalf("RouterDown: %v", err)
		}
		found := false
		for _, line := range fake.Argv() {
			if strings.Contains(line, "compose --project-name "+router.DefaultName+" down --remove-orphans") {
				found = true
			}
		}
		if !found {
			t.Errorf("expected a down call, got %v", fake.Argv())
		}
	})
}

// slicesContainsLine reports whether want is one of lines, exactly.
func slicesContainsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

// containsSubstringLine reports whether any of lines contains substr.
func containsSubstringLine(lines []string, substr string) bool {
	for _, line := range lines {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}
