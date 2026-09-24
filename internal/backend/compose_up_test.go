package backend

import (
	"context"
	"encoding/json"
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
		builderLabels := builder["labels"].(map[string]any)
		if builderLabels["traefik.enable"] != "true" {
			t.Fatalf("expected builder to be routed too, got %v", builderLabels)
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

	t.Run("routes declared services with Traefik labels and the shared network, leaving the rest alone", func(t *testing.T) {
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
			"traefik.enable": "true", "traefik.docker.network": "berth-router",
			routedByLabel: "berth-router",
			"traefik.http.routers." + project + "-api.rule":                      "Host(`feature-login.acme.localhost`)",
			"traefik.http.routers." + project + "-api.entrypoints":               "web",
			"traefik.http.services." + project + "-api.loadbalancer.server.port": "8080", // from expose
		}
		if labels := api["labels"].(map[string]any); !reflectEqual(labels, wantAPILabels) {
			t.Fatalf("api labels = %v, want %v", labels, wantAPILabels)
		}
		wantAPINetworks := map[string]any{"default": map[string]any{}, "berth-router": map[string]any{}}
		if networks := api["networks"].(map[string]any); !reflectEqual(networks, wantAPINetworks) {
			t.Fatalf("api networks = %v, want %v", networks, wantAPINetworks)
		}

		worker := services["worker"].(map[string]any)
		workerLabels := worker["labels"].(map[string]any)
		if got, want := workerLabels["traefik.http.routers."+project+"-worker.rule"], "Host(`jobs.feature-login.acme.localhost`) && PathPrefix(`/v2`)"; got != want {
			t.Fatalf("worker rule = %v, want %v", got, want)
		}
		if got, want := workerLabels["traefik.http.services."+project+"-worker.loadbalancer.server.port"], "9091"; got != want { // explicit port wins
			t.Fatalf("worker port = %v, want %v", got, want)
		}

		quiet := services["quiet"].(map[string]any)
		quietLabels := quiet["labels"].(map[string]any)
		if quietLabels[ns+".managed"] != "true" {
			t.Fatalf("expected quiet still stamped, got %v", quietLabels)
		}
		for k := range quietLabels {
			if strings.HasPrefix(k, "traefik.") {
				t.Fatalf("expected quiet to carry no traefik labels, got %v", quietLabels)
			}
		}
		networks := doc["networks"].(map[string]any)
		if got, want := networks["berth-router"], map[string]any{"external": true}; !reflectEqual(got, want) {
			t.Fatalf("networks[berth-router] = %v, want %v", got, want)
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

func reflectEqual(a, b any) bool {
	return reflect.DeepEqual(a, b)
}
