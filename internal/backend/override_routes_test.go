package backend

// Pinning tests for what a routed service's override looks like: the
// service publishes its pinned loopback port and is otherwise left alone.
// There is no router network, alias or routing label; the proxy reaches the
// service at 127.0.0.1:<port>.

import (
	"testing"

	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/worktree"
)

// routesFixtureRef is the worktree.Ref every test in this file builds its
// override document for.
var routesFixtureRef = worktree.Ref{
	RepositoryID: "11111111-1111-1111-1111-111111111111",
	WorktreeID:   "22222222-2222-2222-2222-222222222222",
	Slug:         "feature-login",
}

// composeWithRoute is a *Compose configured with one route on service,
// published on loopback port hostPort (none when zero), under label
// namespace "dev.treerig".
func composeWithRoute(service string, port *int, hostPort int) *Compose {
	return NewCompose(Options{
		ComposeFile: "/work/acme/docker/compose.yaml", ProjectRoot: "/work/acme",
		ConfigFile: "/work/acme/treerig.toml", ProjectName: "acme",
		ControlHome: "/control", Domain: "acme.localhost", LabelNamespace: "dev.treerig",
		Routes: []RouteSpec{{Service: service, Port: port, HostPort: hostPort}},
	})
}

// decodeModel decodes a `docker compose config --format json` fixture into
// its services, volumes and networks sections, failing the test on error.
func decodeModel(t *testing.T, doc string) (services, volumes, networks *jsonKV) {
	t.Helper()
	services, volumes, networks, err := decodeOrderedComposeModel([]byte(doc), "compose.yaml")
	if err != nil {
		t.Fatalf("decodeOrderedComposeModel: %v", err)
	}
	return services, volumes, networks
}

func TestOverrideRouteShape(t *testing.T) {
	t.Run("a routed service's override has exactly the loopback ports entry and the labels, and no networks key", func(t *testing.T) {
		services, volumes, networks := decodeModel(t, `{"services":{"web":{"expose":["8080"]}}}`)
		rt := composeWithRoute("web", nil, 20123)

		document, err := rt.overrideDocument(routesFixtureRef, services, volumes, networks)
		if err != nil {
			t.Fatalf("overrideDocument: %v", err)
		}
		outServices, _ := document.get("services")
		webEntry := outServices.(*jsonKV).values["web"].(*jsonKV)
		if got := webEntry.keys; len(got) != 2 || got[0] != "labels" || got[1] != "ports" {
			t.Fatalf("routed service keys = %v, want [labels ports]", got)
		}
		if _, has := webEntry.get("networks"); has {
			t.Fatal("routed service declares networks; want none")
		}
		portsValue, _ := webEntry.get("ports")
		entries := portsValue.([]any)
		if len(entries) != 1 {
			t.Fatalf("ports = %v, want exactly one entry", entries)
		}
		entry := entries[0].(*jsonKV)
		for key, want := range map[string]any{"published": "20123", "host_ip": "127.0.0.1", "protocol": "tcp"} {
			if got, _ := entry.get(key); got != want {
				t.Fatalf("ports[0].%s = %v, want %v", key, got, want)
			}
		}
		if target, _ := entry.get("target"); target != 8080 {
			t.Fatalf("ports[0].target = %v, want 8080", target)
		}
	})

	t.Run("the top-level networks section declares no router network", func(t *testing.T) {
		services, volumes, networks := decodeModel(t, `{"services":{"web":{"expose":["8080"]}}}`)
		rt := composeWithRoute("web", nil, 20123)

		document, err := rt.overrideDocument(routesFixtureRef, services, volumes, networks)
		if err != nil {
			t.Fatalf("overrideDocument: %v", err)
		}
		outNetworks, _ := document.get("networks")
		for _, key := range outNetworks.(*jsonKV).keys {
			if key != "default" {
				t.Fatalf("unexpected network %q in the override", key)
			}
		}
	})

	t.Run("a routed service with no assigned port publishes nothing", func(t *testing.T) {
		services, volumes, networks := decodeModel(t, `{"services":{"web":{"expose":["8080"]}}}`)
		rt := composeWithRoute("web", nil, 0)

		document, err := rt.overrideDocument(routesFixtureRef, services, volumes, networks)
		if err != nil {
			t.Fatalf("overrideDocument: %v", err)
		}
		outServices, _ := document.get("services")
		webEntry := outServices.(*jsonKV).values["web"].(*jsonKV)
		if _, has := webEntry.get("ports"); has {
			t.Fatal("service publishes ports without an assigned host port")
		}
	})

	t.Run("a routed service carries no Traefik or routing labels, ownership labels only", func(t *testing.T) {
		services, volumes, networks := decodeModel(t, `{"services":{"web":{"expose":["8080"]}}}`)
		rt := composeWithRoute("web", nil, 20123)

		document, err := rt.overrideDocument(routesFixtureRef, services, volumes, networks)
		if err != nil {
			t.Fatalf("overrideDocument: %v", err)
		}
		outServices, _ := document.get("services")
		webEntry := outServices.(*jsonKV).values["web"].(*jsonKV)
		labelsValue, _ := webEntry.get("labels")
		for _, key := range labelsValue.(*jsonKV).keys {
			if key == "traefik.enable" || key == "dev.treerig.router" {
				t.Fatalf("service carries a routing label %q; want ownership labels only", key)
			}
		}
	})

	t.Run("RouteName is the project name and the service, unique across worktrees", func(t *testing.T) {
		name := RouteName(routesFixtureRef, RouteSpec{Service: "web"})
		wantProject := layout.ComposeProjectName(routesFixtureRef.RepositoryID, routesFixtureRef.WorktreeID)
		if name != wantProject+"-web" {
			t.Fatalf("RouteName = %q, want %q", name, wantProject+"-web")
		}
		other := RouteName(worktree.Ref{RepositoryID: routesFixtureRef.RepositoryID, WorktreeID: "33333333-3333-3333-3333-333333333333"}, RouteSpec{Service: "web"})
		if other == name {
			t.Fatalf("RouteName collides across worktrees: %q", name)
		}
	})
}
