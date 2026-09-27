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

	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/router"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/run/fakerun"
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

// healthyRouterDocker answers as though the shared router were already up,
// correctly configured, and at the current protocol, so Ensure takes its
// fast path: this package's own tests only check its wiring (the state
// directory, port and network reaching router.New, and a verified up
// reporting where it is up), not Ensure's full behaviour, which
// internal/router's suite already covers.
type healthyRouterDocker struct {
	mu      sync.Mutex
	state   layout.RouterLayout
	network string
	port    int
	// home is stamped as RouterHomeLabel on every container docFor answers,
	// so Owner/checkOwnership see this router as owned by whatever berth
	// root the test itself resolved, rather than refusing it as foreign.
	home string
}

// Which always answers true.
func (d *healthyRouterDocker) Which(string) bool { return true }

// Run answers as a healthy, current-protocol router.
func (d *healthyRouterDocker) Run(_ context.Context, spec run.Spec) (run.Result, error) {
	args := spec.Args
	switch {
	case len(args) > 0 && args[0] == "compose" && sliceContainsArg(args, "ps"):
		return run.Result{Stdout: "c-" + args[len(args)-1]}, nil
	case len(args) > 0 && args[0] == "compose" && sliceContainsArg(args, "up"):
		return run.Result{}, nil
	case len(args) > 0 && args[0] == "compose" && sliceContainsArg(args, "down"):
		return run.Result{}, nil
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

// docFor is what `docker inspect` answers for one healthy, current-protocol
// service container.
func (d *healthyRouterDocker) docFor(service string) router.ContainerInspect {
	labels := map[string]string{
		"com.docker.compose.project": router.DefaultName,
		"com.docker.compose.service": service,
		"dev.berth.component":        service,
		"dev.berth.router-protocol":  strconv.Itoa(router.Protocol),
		router.RouterHomeLabel:       d.home,
	}
	hardened := router.InspectHostConfig{CapAdd: []string{}, CapDrop: []string{"ALL"}, ReadonlyRootfs: true, SecurityOpt: []string{"no-new-privileges:true"}}
	healthy := router.InspectState{Status: "running", Health: router.InspectHealth{Status: "healthy"}}

	switch service {
	case router.RouterService:
		host := hardened
		host.CapAdd = []string{"CAP_NET_BIND_SERVICE"}
		return router.ContainerInspect{
			Config:     router.InspectConfig{Cmd: router.RouterCommand(), Entrypoint: router.RouterImageEntrypoint, Image: router.RouterImage, Labels: labels},
			HostConfig: host,
			Mounts:     []router.InspectMount{{Type: "bind", Source: d.state.DynamicDir, Destination: router.DynamicConfigMount, RW: false}},
			NetworkSettings: router.InspectNetworkSettings{
				Networks: map[string]json.RawMessage{d.network: json.RawMessage("{}")},
				Ports:    map[string][]router.InspectPortBinding{"80/tcp": {{HostIP: "127.0.0.1", HostPort: strconv.Itoa(d.port)}}},
			},
			State: healthy,
		}
	default: // index
		for key, value := range router.IndexRouteLabels(d.network).Map() {
			labels[key] = value
		}
		return router.ContainerInspect{
			Config:     router.InspectConfig{Cmd: router.IndexCommand, Entrypoint: router.IndexImageEntrypoint, Image: router.IndexImage, Labels: labels},
			HostConfig: hardened,
			Mounts:     []router.InspectMount{{Type: "bind", Source: d.state.IndexDir, Destination: router.IndexMount, RW: false}},
			NetworkSettings: router.InspectNetworkSettings{
				Networks: map[string]json.RawMessage{d.network: json.RawMessage("{}")},
				Ports:    map[string][]router.InspectPortBinding{},
			},
			State: healthy,
		}
	}
}

func TestRouterUp(t *testing.T) {
	t.Run("starts the router and reports where it is up", func(t *testing.T) {
		home := registrytest.TempDir(t)
		controlHome := registrytest.TempDir(t)
		getenv := routerEnvGetenv(map[string]string{"HOME": home, "BERTH_CONTROL_HOME": controlHome})
		state, err := layout.RouterState(getenv)
		if err != nil {
			t.Fatal(err)
		}
		berthRoot, err := layout.ResolveHome(getenv, "")
		if err != nil {
			t.Fatal(err)
		}
		rec := &logger.Recording{}
		env := &commands.Env{
			CommandName: "berth", Invocation: "berth router up", Logger: rec, Getenv: getenv,
			Runner: &healthyRouterDocker{state: state, network: router.DefaultName, port: 80, home: berthRoot},
		}

		if err := commands.RouterUp(context.Background(), env); err != nil {
			t.Fatalf("RouterUp: %v", err)
		}

		lines, _ := rec.Snapshot()
		want := "info: Router is up on 127.0.0.1:80 (network " + router.DefaultName + "); index at " + router.IndexURL(80) + "."
		if !slicesContainsLine(lines, want) {
			t.Errorf("expected %q, got %v", want, lines)
		}
	})

	// #15: `router up` never regenerated the real page — Ensure's own write()
	// writes the placeholder only when the index file is missing outright,
	// for the index container's health check, and nothing after Ensure ever
	// replaced it. Deleting the page and running `router up` again must show
	// every checkout again, not leave the placeholder in place.
	t.Run("refreshes a missing index page once the router has started, showing every checkout again", func(t *testing.T) {
		home := registrytest.TempDir(t)
		controlHome := registrytest.TempDir(t)
		getenv := routerEnvGetenv(map[string]string{"HOME": home, "BERTH_CONTROL_HOME": controlHome})
		state, err := layout.RouterState(getenv)
		if err != nil {
			t.Fatal(err)
		}
		berthRoot, err := layout.ResolveHome(getenv, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := registrytest.WriteRecord(controlHome, registrytest.RecordJSON{
			Version: 1, RepositoryID: "11111111-1111-4111-8111-111111111111",
			WorktreeID: "22222222-2222-4222-8222-222222222221",
			Slug:       "feature-a", Domain: "acme.localhost", Initialization: "complete",
		}); err != nil {
			t.Fatal(err)
		}
		rec := &logger.Recording{}
		env := &commands.Env{
			CommandName: "berth", Invocation: "berth router up", Logger: rec, Getenv: getenv, Cwd: registrytest.TempDir(t),
			Runner: &healthyRouterDocker{state: state, network: router.DefaultName, port: 80, home: berthRoot},
		}

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
		// function, fed the very env RouterUp will resolve BERTH_HOME
		// through — so the symlink lands exactly where RouterUp will look,
		// under the berth root (#80) rather than a hand-built guess.
		// RouterUp must refuse before commands.routerHold ever takes the
		// router lock (which would create <target>/lock and its holder
		// record) or Ensure's own write() ever writes a stack file into it.
		target := registrytest.TempDir(t)
		fakeHome := registrytest.TempDir(t)
		berthHome := registrytest.TempDir(t)
		getenv := routerEnvGetenv(map[string]string{"HOME": fakeHome, "BERTH_HOME": berthHome})
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
			CommandName: "berth", Invocation: "berth router up", Logger: rec, Getenv: getenv,
			Runner: &healthyRouterDocker{network: router.DefaultName, port: 80},
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
		env := &commands.Env{CommandName: "berth", Logger: rec, Getenv: getenv, Runner: fake}

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

// TestRouterStatusPortLine is #79: beside its status line, `router status`
// says on stdout which port it resolved and where that came from.
func TestRouterStatusPortLine(t *testing.T) {
	t.Run("says where the port came from, on stdout with the status line", func(t *testing.T) {
		home := registrytest.TempDir(t)
		getenv := routerEnvGetenv(map[string]string{"HOME": home})
		root := filepath.Join(home, ".berth")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "settings.toml"), []byte("[router]\nport = 8080\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		rec := &logger.Recording{}
		// A runner that answers every call with empty success: no container
		// is listed, so the router reads as reachable but not running.
		env := &commands.Env{CommandName: "berth", Logger: rec, Getenv: getenv, Runner: &fakerun.Runner{}}

		failed, err := commands.RouterStatus(context.Background(), env)
		if err != nil {
			t.Fatalf("RouterStatus: %v", err)
		}
		if failed {
			t.Error("expected failed = false")
		}
		_, printed := rec.Snapshot()
		want := []string{
			"not running (would use 127.0.0.1:8080, network berth-router)",
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
		env := &commands.Env{CommandName: "berth", Logger: &logger.Recording{}, Getenv: getenv, Runner: fake}

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
		root := filepath.Join(home, ".berth")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "settings.toml"), []byte("[router]\nport = \"eighty\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		getenv := routerEnvGetenv(map[string]string{"HOME": home, "BERTH_ROUTER_PORT": "not-a-port"})
		fake := &fakerun.Runner{}
		env := &commands.Env{CommandName: "berth", Logger: &logger.Recording{}, Getenv: getenv, Runner: fake}

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
