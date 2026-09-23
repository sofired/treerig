package commands_test

// Wiring checks for the router verbs: the right project, port and state
// directory reach router.New, a verified `up` migrates legacy state and
// reports where it is up, and `status`/`down` reach the router package
// correctly. internal/router's own suite covers Ensure/Verify/Status/Down's
// full behaviour; this package only has its own seam (Env, not
// CommandContext — the router needs no project config) to get wrong.

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

func routerEnvGetenv(values map[string]string) layout.Getenv {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

func sliceContainsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// healthyRouterDocker answers as though the shared router were already up,
// correctly configured, and at the current protocol, so Ensure's protocol
// dance takes its fast path: this package's own tests only need to check its
// wiring (the state directory, port and network reaching router.New; a
// verified up migrating legacy state and reporting where it is up), not
// re-prove Ensure's full behaviour, which internal/router's suite already covers.
type healthyRouterDocker struct {
	mu      sync.Mutex
	state   layout.RouterLayout
	network string
	port    int
}

func (d *healthyRouterDocker) Which(string) bool { return true }

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

func (d *healthyRouterDocker) docFor(service string) router.ContainerInspect {
	labels := map[string]string{
		"com.docker.compose.project": router.DefaultName,
		"com.docker.compose.service": service,
		"dev.berth.component":        service,
		"dev.berth.router-protocol":  strconv.Itoa(router.Protocol),
	}
	hardened := router.InspectHostConfig{CapAdd: []string{}, CapDrop: []string{"ALL"}, ReadonlyRootfs: true, SecurityOpt: []string{"no-new-privileges:true"}}
	healthy := router.InspectState{Status: "running", Health: router.InspectHealth{Status: "healthy"}}

	switch service {
	case router.RouterService:
		host := hardened
		host.CapAdd = []string{"CAP_NET_BIND_SERVICE"}
		return router.ContainerInspect{
			Config:     router.InspectConfig{Cmd: router.RouterCommand(d.network), Image: router.RouterImage, Labels: labels},
			HostConfig: host,
			Mounts:     []router.InspectMount{{Type: "bind", Source: d.state.DynamicDir, Destination: router.DynamicConfigMount, RW: false}},
			NetworkSettings: router.InspectNetworkSettings{
				Networks: map[string]json.RawMessage{d.network: json.RawMessage("{}"), router.DefaultName + "_docker-api": json.RawMessage("{}")},
				Ports:    map[string][]router.InspectPortBinding{"80/tcp": {{HostIP: "127.0.0.1", HostPort: strconv.Itoa(d.port)}}},
			},
			State: healthy,
		}
	case router.DockerAPIService:
		access := router.DockerAPIAccess()
		env := make([]string, 0, len(access))
		for name, value := range access {
			env = append(env, name+"="+value)
		}
		return router.ContainerInspect{
			Config:     router.InspectConfig{Env: env, Image: router.DockerAPIImage, Labels: labels},
			HostConfig: hardened,
			Mounts:     []router.InspectMount{{Type: "bind", Source: router.DockerSocket, Destination: router.DockerSocket, RW: false}},
			NetworkSettings: router.InspectNetworkSettings{
				Networks: map[string]json.RawMessage{router.DefaultName + "_docker-api": json.RawMessage("{}")},
				Ports:    map[string][]router.InspectPortBinding{},
			},
			State: healthy,
		}
	default: // index
		for key, value := range router.IndexRouteLabels(d.network).Map() {
			labels[key] = value
		}
		return router.ContainerInspect{
			Config:     router.InspectConfig{Cmd: router.IndexCommand, Image: router.IndexImage, Labels: labels},
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
		getenv := routerEnvGetenv(map[string]string{"HOME": home, "BERTH_HOME": controlHome})
		state, err := layout.RouterState(getenv)
		if err != nil {
			t.Fatal(err)
		}
		rec := &logger.Recording{}
		env := &commands.Env{
			CommandName: "berth", Invocation: "berth router up", Logger: rec, Getenv: getenv,
			Runner: &healthyRouterDocker{state: state, network: router.DefaultName, port: 80},
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

	t.Run("migrates a control home's earlier router state once the new router has verified", func(t *testing.T) {
		home := registrytest.TempDir(t)
		controlHome := registrytest.TempDir(t)
		getenv := routerEnvGetenv(map[string]string{"HOME": home, "BERTH_HOME": controlHome})
		state, err := layout.RouterState(getenv)
		if err != nil {
			t.Fatal(err)
		}
		legacyDynamic := filepath.Join(controlHome, "router", "dynamic")
		if err := os.MkdirAll(legacyDynamic, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(legacyDynamic, "fragment.yaml"), []byte("routers: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		rec := &logger.Recording{}
		env := &commands.Env{
			CommandName: "berth", Invocation: "berth router up", Logger: rec, Getenv: getenv,
			Runner: &healthyRouterDocker{state: state, network: router.DefaultName, port: 80},
		}

		if err := commands.RouterUp(context.Background(), env); err != nil {
			t.Fatalf("RouterUp: %v", err)
		}

		if _, err := os.Stat(filepath.Join(state.DynamicDir, "fragment.yaml")); err != nil {
			t.Errorf("expected the fragment to have migrated: %v", err)
		}
		if _, err := os.Stat(filepath.Join(controlHome, "router")); !os.IsNotExist(err) {
			t.Errorf("expected the legacy router directory to be gone, stat err = %v", err)
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
}

func slicesContainsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

func containsSubstringLine(lines []string, substr string) bool {
	for _, line := range lines {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}
