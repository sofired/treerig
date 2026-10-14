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

// intPtr returns a pointer to a copy of n, for taking the address of a literal.
func intPtr(n int) *int { return &n }

// routedRuntime is newRuntime plus routes.
func routedRuntime(t *testing.T, fake *dockerFake, routes []RouteSpec) (*Compose, string) {
	t.Helper()
	controlHome := t.TempDir()
	rt := NewCompose(Options{
		ComposeFile: "/work/acme/docker/compose.yaml", ProjectRoot: "/work/acme",
		ConfigFile: "/work/acme/berth.toml", ProjectName: "acme", Domain: "acme.localhost",
		ControlHome: controlHome, LabelNamespace: ns, Routes: routes,
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
			ns + ".managed": "true", ns + ".repository-uuid": ref.RepositoryID, ns + ".worktree-uuid": ref.WorktreeID, ns + ".format": "1",
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
			ns + ".managed": "true", ns + ".repository-uuid": ref.RepositoryID, ns + ".worktree-uuid": ref.WorktreeID, ns + ".format": "1",
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
			ns + ".managed": "true", ns + ".repository-uuid": ref.RepositoryID, ns + ".worktree-uuid": ref.WorktreeID, ns + ".format": "1",
		}
		build, ok := builder["build"].(map[string]any)
		if !ok {
			t.Fatalf("expected builder.build, got %v", builder)
		}
		if labels := build["labels"].(map[string]any); !reflectEqual(labels, ownership) {
			t.Fatalf("build.labels = %v, want %v", labels, ownership)
		}
		if _, has := builder["networks"]; has {
			t.Fatalf("expected the routed builder to declare no networks, got %v", builder["networks"])
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

	// A routed service publishes its pinned loopback port and is otherwise
	// left alone; override_routes_test.go pins the shape. This exercises it
	// end to end through Up and the override file it writes.
	t.Run("publishes declared services on loopback, with no network or alias, leaving the rest alone", func(t *testing.T) {
		fake := newFake()
		port := 9091
		path := "/v2"
		routes := []RouteSpec{
			{Service: "api", Subdomain: "", HostPort: 20001},
			{Service: "worker", Subdomain: "jobs", Path: &path, Port: &port, HostPort: 20002},
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
			ns + ".managed": "true", ns + ".repository-uuid": ref.RepositoryID, ns + ".worktree-uuid": ref.WorktreeID, ns + ".format": "1",
		}
		if labels := api["labels"].(map[string]any); !reflectEqual(labels, wantAPILabels) {
			t.Fatalf("api labels = %v, want %v", labels, wantAPILabels)
		}
		for _, name := range []string{"api", "worker"} {
			entry := services[name].(map[string]any)
			if _, has := entry["networks"]; has {
				t.Fatalf("%s declares networks %v; want none", name, entry["networks"])
			}
			if ports, ok := entry["ports"].([]any); !ok || len(ports) != 1 {
				t.Fatalf("%s ports = %v, want one loopback entry", name, entry["ports"])
			}
		}
		workerPort := services["worker"].(map[string]any)["ports"].([]any)[0].(map[string]any)
		if workerPort["published"] != "20002" || workerPort["host_ip"] != "127.0.0.1" || workerPort["target"] != float64(9091) {
			t.Fatalf("worker port = %v", workerPort)
		}

		quiet := services["quiet"].(map[string]any)
		quietLabels := quiet["labels"].(map[string]any)
		if quietLabels[ns+".managed"] != "true" {
			t.Fatalf("expected quiet still stamped, got %v", quietLabels)
		}
		if _, routed := quiet["ports"]; routed {
			t.Fatalf("expected quiet to publish nothing (not routed), got %v", quiet["ports"])
		}

		for key := range doc["networks"].(map[string]any) {
			if strings.Contains(key, "router") {
				t.Fatalf("unexpected router network %q in the override", key)
			}
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
		if err := rt2.Up(ctx, ref, nil); err == nil || !strings.Contains(err.Error(), "[service.ghost] names a service that is not defined") {
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

	t.Run("an unset wait timeout emits the 120-second default, not any zero value", func(t *testing.T) {
		fake := newFake()
		controlHome := t.TempDir()
		// Built directly, not through routedRuntime, which always sets one:
		// this is specifically about leaving WaitTimeoutSeconds nil.
		rt := NewCompose(Options{
			ComposeFile: "/work/acme/docker/compose.yaml", ProjectRoot: "/work/acme",
			ConfigFile: "/work/acme/berth.toml", ProjectName: "acme", Domain: "acme.localhost",
			ControlHome: controlHome, LabelNamespace: ns,
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
			ControlHome: controlHome, LabelNamespace: ns,
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
