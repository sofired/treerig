package backend

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/run"
)

// routedRuntime is newRuntime plus routes and a router network, matching the
// TypeScript suite's own runtime(fake, routes) helper.
func intPtr(n int) *int { return &n }

// routedRuntime is newRuntime plus routes and a router network, matching the
// TypeScript suite's own runtime(fake, routes) helper.
func routedRuntime(t *testing.T, fake *dockerFake, routes []RouteSpec) (*Compose, string) {
	t.Helper()
	controlHome := t.TempDir()
	rt := NewCompose(Options{
		ComposeFile: "/work/acme/docker/compose.yaml", ProjectRoot: "/work/acme",
		ConfigFile: "/work/acme/berth.toml", ProjectName: "acme", Domain: "acme.localhost",
		ControlHome: controlHome, LabelNamespace: ns, Routes: routes, RouterNetwork: "berth-router",
		Runner: fake, Logger: logger.Silent{}, WaitTimeoutSeconds: intPtr(45),
	})
	return rt, controlHome
}

// overridePathFor is the path the runtime writes berth.override.yaml to
// under a worktree's home, for tests that read it back directly.
func overridePathFor(controlHome string) string {
	return filepath.Join(layout.WorktreeLayout(controlHome, ref.RepositoryID, ref.WorktreeID).Home, "berth.override.yaml")
}

// overrideDoc reads and decodes the override document written to disk.
func overrideDoc(t *testing.T, controlHome string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(overridePathFor(controlHome))
	if err != nil {
		t.Fatalf("read override: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode override: %v\n%s", err, data)
	}
	return doc
}

// keysOf returns m's keys in sorted order, for deterministic assertions on a
// decoded document's shape.
func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func TestComposeUp(t *testing.T) {
	ctx := context.Background()

	t.Run("stamps ownership labels onto every service and every non-external volume and network", func(t *testing.T) {
		fake := newFake()
		rt, controlHome := routedRuntime(t, fake, nil)
		ensureHome(t, controlHome)
		if err := rt.Up(ctx, ref, []string{"api", "worker", "quiet"}); err != nil {
			t.Fatal(err)
		}
		doc := overrideDoc(t, controlHome)
		services := doc["services"].(map[string]any)
		if want := []string{"api", "builder", "quiet", "worker"}; !slices.Equal(keysOf(services), want) {
			t.Fatalf("services = %v, want %v", keysOf(services), want)
		}
		api := services["api"].(map[string]any)
		wantLabels := map[string]any{
			ns + ".managed": "true", ns + ".repository-uuid": ref.RepositoryID, ns + ".worktree-uuid": ref.WorktreeID,
		}
		if labels := api["labels"].(map[string]any); !reflectEqual(labels, wantLabels) {
			t.Fatalf("api labels = %v, want %v", labels, wantLabels)
		}
		volumes := doc["volumes"].(map[string]any)
		if want := []string{"data"}; !slices.Equal(keysOf(volumes), want) { // "shared" is external
			t.Fatalf("volumes = %v, want %v", keysOf(volumes), want)
		}
		networks := doc["networks"].(map[string]any)
		if want := []string{"backend", "default"}; !slices.Equal(keysOf(networks), want) { // "corp" is external
			t.Fatalf("networks = %v, want %v", keysOf(networks), want)
		}
	})

	t.Run("labels a service Compose reports only because COMPOSE_PROFILES activated it (#54)", func(t *testing.T) {
		fake := newFake()
		controlHome := t.TempDir()
		rt := NewCompose(Options{
			ComposeFile: "/work/acme/docker/compose.yaml", ProjectRoot: "/work/acme",
			ConfigFile: "/work/acme/berth.toml", ProjectName: "acme", Domain: "acme.localhost",
			ControlHome: controlHome, LabelNamespace: ns,
			Runner: fake, Logger: logger.Silent{},
			Environ: []string{"PATH=/bin", "COMPOSE_PROFILES=debug"},
		})
		ensureHome(t, controlHome)
		if err := rt.Up(ctx, ref, nil); err != nil {
			t.Fatal(err)
		}
		doc := overrideDoc(t, controlHome)
		services := doc["services"].(map[string]any)
		debugOnly, ok := services["debugOnly"].(map[string]any)
		if !ok {
			t.Fatalf("services = %v, want debugOnly labeled too", keysOf(services))
		}
		wantLabels := map[string]any{
			ns + ".managed": "true", ns + ".repository-uuid": ref.RepositoryID, ns + ".worktree-uuid": ref.WorktreeID,
		}
		if labels := debugOnly["labels"].(map[string]any); !reflectEqual(labels, wantLabels) {
			t.Fatalf("debugOnly labels = %v, want %v", labels, wantLabels)
		}
		// The config read that built the override and the `up` that follows it
		// share one environment, so Compose activates exactly the profile the
		// override already labeled for — never a wider or narrower set.
		got := environ(fake.composeCalls("up")[0])
		if got["COMPOSE_PROFILES"] != "debug" {
			t.Fatalf("COMPOSE_PROFILES = %q, want it to reach the `up` call unchanged", got["COMPOSE_PROFILES"])
		}
	})

	t.Run("refuses to start a named service Compose left out of the model it labels, before writing or starting anything", func(t *testing.T) {
		fake := newFake()
		controlHome := t.TempDir()
		// No COMPOSE_PROFILES: `compose config` omits debugOnly, yet
		// `compose up debugOnly` would enable its profile and start it unlabelled.
		rt := NewCompose(Options{
			ComposeFile: "/work/acme/docker/compose.yaml", ProjectRoot: "/work/acme",
			ConfigFile: "/work/acme/berth.toml", ProjectName: "acme", Domain: "acme.localhost",
			ControlHome: controlHome, LabelNamespace: ns,
			Runner: fake, Logger: logger.Silent{},
			Environ: []string{"PATH=/bin"},
		})
		ensureHome(t, controlHome)
		err := rt.Up(ctx, ref, []string{"api", "debugOnly"})
		if err == nil || !strings.Contains(err.Error(), "Not starting debugOnly: Compose does not enable it in this configuration") {
			t.Fatalf("err = %v, want a refusal naming debugOnly", err)
		}
		if calls := fake.composeCalls("up"); len(calls) != 0 {
			t.Fatalf("compose up ran %d time(s), want none", len(calls))
		}
		if _, statErr := os.Stat(overridePathFor(controlHome)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("override at %s: stat err = %v, want it never written", overridePathFor(controlHome), statErr)
		}
	})

	t.Run("starts a named service once its profile is active, labelled like any other", func(t *testing.T) {
		fake := newFake()
		controlHome := t.TempDir()
		rt := NewCompose(Options{
			ComposeFile: "/work/acme/docker/compose.yaml", ProjectRoot: "/work/acme",
			ConfigFile: "/work/acme/berth.toml", ProjectName: "acme", Domain: "acme.localhost",
			ControlHome: controlHome, LabelNamespace: ns,
			Runner: fake, Logger: logger.Silent{},
			Environ: []string{"PATH=/bin", "COMPOSE_PROFILES=debug"},
		})
		ensureHome(t, controlHome)
		if err := rt.Up(ctx, ref, []string{"debugOnly"}); err != nil {
			t.Fatal(err)
		}
		services := overrideDoc(t, controlHome)["services"].(map[string]any)
		if _, ok := services["debugOnly"]; !ok {
			t.Fatalf("services = %v, want debugOnly labelled", keysOf(services))
		}
	})

	t.Run("stamps build labels on services that build, and only those, so the images they produce are owned too", func(t *testing.T) {
		fake := newFake()
		rt, controlHome := routedRuntime(t, fake, []RouteSpec{{Service: "builder", Subdomain: "b"}})
		ensureHome(t, controlHome)
		if err := rt.Up(ctx, ref, nil); err != nil {
			t.Fatal(err)
		}
		doc := overrideDoc(t, controlHome)
		services := doc["services"].(map[string]any)
		builder := services["builder"].(map[string]any)
		ownership := map[string]any{
			ns + ".managed": "true", ns + ".repository-uuid": ref.RepositoryID, ns + ".worktree-uuid": ref.WorktreeID,
		}
		build, ok := builder["build"].(map[string]any)
		if !ok {
			t.Fatalf("expected builder.build, got %v", builder)
		}
		if labels := build["labels"].(map[string]any); !reflectEqual(labels, ownership) {
			t.Fatalf("build.labels = %v, want %v", labels, ownership)
		}
		builderNetworks, ok := builder["networks"].(map[string]any)
		if !ok {
			t.Fatalf("expected builder to be routed too (joining the router network), got %v", builder)
		}
		routerEntry, ok := builderNetworks[routerNetworkKey].(map[string]any)
		if !ok {
			t.Fatalf("expected builder to join the router network, got %v", builderNetworks)
		}
		wantAlias := []any{project + "-builder"}
		if aliases := routerEntry["aliases"]; !reflectEqual(aliases, wantAlias) {
			t.Fatalf("builder router aliases = %v, want %v", aliases, wantAlias)
		}
		api := services["api"].(map[string]any)
		if _, ok := api["build"]; ok {
			t.Fatalf("expected api to have no build entry, got %v", api)
		}
	})

	t.Run("refuses to run without the worktree home, which only the command layer creates", func(t *testing.T) {
		fake := newFake()
		rt, controlHome := routedRuntime(t, fake, nil)
		err := rt.Up(ctx, ref, nil)
		if err == nil || !strings.Contains(err.Error(), "does not exist. `up` creates it before starting services, so this is a bug in berth") {
			t.Fatalf("got %v", err)
		}
		if _, statErr := os.Stat(overridePathFor(controlHome)); statErr == nil {
			t.Fatal("expected no override to be written")
		}
		if len(fake.calls) != 0 {
			t.Fatalf("expected nothing spawned, got %+v", fake.calls)
		}
	})

	// Go-only, per #55/row 31: TypeScript stamps Traefik labels onto a routed
	// service and joins it to one shared external network; Go instead joins
	// it, under an alias, to this worktree's own owned router network — see
	// override_network_test.go for the dedicated pinning tests of that
	// shape. This is the same corpus scenario TestOverrideCorpus's "routing"
	// cases skip the byte comparison for, exercised here end to end through
	// Up and the override file it writes.
	t.Run("routes declared services onto the worktree's own router network, each under its own alias, leaving the rest alone", func(t *testing.T) {
		fake := newFake()
		port := 9091
		path := "/v2"
		routes := []RouteSpec{
			{Service: "api", Subdomain: ""},
			{Service: "worker", Subdomain: "jobs", Path: &path, Port: &port},
		}
		rt, controlHome := routedRuntime(t, fake, routes)
		ensureHome(t, controlHome)
		if err := rt.Up(ctx, ref, nil); err != nil {
			t.Fatal(err)
		}
		doc := overrideDoc(t, controlHome)
		services := doc["services"].(map[string]any)
		api := services["api"].(map[string]any)
		wantAPILabels := map[string]any{
			ns + ".managed": "true", ns + ".repository-uuid": ref.RepositoryID, ns + ".worktree-uuid": ref.WorktreeID,
		}
		if labels := api["labels"].(map[string]any); !reflectEqual(labels, wantAPILabels) {
			t.Fatalf("api labels = %v, want %v", labels, wantAPILabels)
		}
		wantAPINetworks := map[string]any{"default": map[string]any{}, routerNetworkKey: map[string]any{"aliases": []any{project + "-api"}}}
		if networks := api["networks"].(map[string]any); !reflectEqual(networks, wantAPINetworks) {
			t.Fatalf("api networks = %v, want %v", networks, wantAPINetworks)
		}

		worker := services["worker"].(map[string]any)
		workerNetworks := worker["networks"].(map[string]any)
		workerRouterEntry := workerNetworks[routerNetworkKey].(map[string]any)
		if got, want := workerRouterEntry["aliases"], []any{project + "-worker"}; !reflectEqual(got, want) {
			t.Fatalf("worker aliases = %v, want %v", got, want)
		}

		quiet := services["quiet"].(map[string]any)
		quietLabels := quiet["labels"].(map[string]any)
		if quietLabels[ns+".managed"] != "true" {
			t.Fatalf("expected quiet still stamped, got %v", quietLabels)
		}
		if _, routed := quiet["networks"]; routed {
			t.Fatalf("expected quiet to declare no networks override (not routed), got %v", quiet["networks"])
		}

		networks := doc["networks"].(map[string]any)
		routerNetwork := networks[routerNetworkKey].(map[string]any)
		if _, external := routerNetwork["external"]; external {
			t.Fatal("router network is external; want owned")
		}
		if got, want := routerNetwork["name"], RouterNetworkName(ref.RepositoryID, ref.WorktreeID); got != want {
			t.Fatalf("networks[%s].name = %v, want %v", routerNetworkKey, got, want)
		}
		routerLabels := routerNetwork["labels"].(map[string]any)
		if got, want := routerLabels[routedByLabel], "berth-router"; got != want {
			t.Fatalf("networks[%s].labels[%s] = %v, want %v", routerNetworkKey, routedByLabel, got, want)
		}
	})

	t.Run("refuses to route a service with no port, and one the Compose file does not define", func(t *testing.T) {
		fake := newFake()
		rt, controlHome := routedRuntime(t, fake, []RouteSpec{{Service: "quiet", Subdomain: "q"}})
		ensureHome(t, controlHome)
		if err := rt.Up(ctx, ref, nil); err == nil || !strings.Contains(err.Error(), "Service quiet declares no port") {
			t.Fatalf("got %v", err)
		}

		fake2 := newFake()
		rt2, controlHome2 := routedRuntime(t, fake2, []RouteSpec{{Service: "ghost", Subdomain: ""}})
		ensureHome(t, controlHome2)
		if err := rt2.Up(ctx, ref, nil); err == nil || !strings.Contains(err.Error(), "[[service]] ghost is not defined") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("runs compose up detached with build, orphan removal, and a bounded wait, naming the requested services", func(t *testing.T) {
		fake := newFake()
		rt, controlHome := routedRuntime(t, fake, nil)
		ensureHome(t, controlHome)
		if err := rt.Up(ctx, ref, []string{"api"}); err != nil {
			t.Fatal(err)
		}
		up := fake.composeCalls("up")
		if len(up) != 1 {
			t.Fatalf("expected one up call, got %d", len(up))
		}
		want := []string{
			"compose", "--project-name", project,
			"--project-directory", "/work/acme/docker",
			"-f", "/work/acme/docker/compose.yaml",
			"-f", overridePathFor(controlHome),
			"up", "--detach", "--build", "--remove-orphans", "--wait", "--wait-timeout", "45",
			"api",
		}
		if !slices.Equal(up[0].Args, want) {
			t.Fatalf("up args = %v, want %v", up[0].Args, want)
		}
		if up[0].Output != run.Tee {
			t.Fatalf("expected the up call to tee output, got %v", up[0].Output)
		}
	})

	t.Run("an unset wait timeout emits the 120-second default, matching TypeScript's ?? — not any zero value", func(t *testing.T) {
		fake := newFake()
		controlHome := t.TempDir()
		// Built directly, not through routedRuntime, which always sets one:
		// this is specifically about leaving WaitTimeoutSeconds nil.
		rt := NewCompose(Options{
			ComposeFile: "/work/acme/docker/compose.yaml", ProjectRoot: "/work/acme",
			ConfigFile: "/work/acme/berth.toml", ProjectName: "acme", Domain: "acme.localhost",
			ControlHome: controlHome, LabelNamespace: ns, RouterNetwork: "berth-router",
			Runner: fake, Logger: logger.Silent{},
		})
		ensureHome(t, controlHome)
		if err := rt.Up(ctx, ref, nil); err != nil {
			t.Fatal(err)
		}
		up := fake.composeCalls("up")
		if len(up) != 1 {
			t.Fatalf("expected one up call, got %d", len(up))
		}
		if !slices.Contains(up[0].Args, "120") {
			t.Fatalf("expected --wait-timeout 120 in %v", up[0].Args)
		}
	})

	t.Run("an explicit zero wait timeout passes through, unlike an unset one", func(t *testing.T) {
		fake := newFake()
		controlHome := t.TempDir()
		rt := NewCompose(Options{
			ComposeFile: "/work/acme/docker/compose.yaml", ProjectRoot: "/work/acme",
			ConfigFile: "/work/acme/berth.toml", ProjectName: "acme", Domain: "acme.localhost",
			ControlHome: controlHome, LabelNamespace: ns, RouterNetwork: "berth-router",
			Runner: fake, Logger: logger.Silent{}, WaitTimeoutSeconds: intPtr(0),
		})
		ensureHome(t, controlHome)
		if err := rt.Up(ctx, ref, nil); err != nil {
			t.Fatal(err)
		}
		up := fake.composeCalls("up")
		if len(up) != 1 {
			t.Fatalf("expected one up call, got %d", len(up))
		}
		if !slices.Contains(up[0].Args, "0") {
			t.Fatalf("expected --wait-timeout 0 (not the 120 default) in %v", up[0].Args)
		}
	})

	t.Run("reads the config without the override, since the override does not exist yet", func(t *testing.T) {
		fake := newFake()
		rt, controlHome := routedRuntime(t, fake, nil)
		ensureHome(t, controlHome)
		if err := rt.Up(ctx, ref, nil); err != nil {
			t.Fatal(err)
		}
		config := fake.composeCalls("config")
		if len(config) != 1 {
			t.Fatalf("expected one config call, got %d", len(config))
		}
		fCount := 0
		for _, a := range config[0].Args {
			if a == "-f" {
				fCount++
			}
		}
		if fCount != 1 {
			t.Fatalf("expected exactly one -f, got %d in %v", fCount, config[0].Args)
		}
		if !slices.Contains(config[0].Args, "--project-directory") {
			t.Fatalf("expected --project-directory, got %v", config[0].Args)
		}
	})
}

// reflectEqual is reflect.DeepEqual under a short name, for table-driven
// cases that compare decoded documents field by field.
func reflectEqual(a, b any) bool {
	return reflect.DeepEqual(a, b)
}
