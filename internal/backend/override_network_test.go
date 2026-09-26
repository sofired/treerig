package backend

// Go-only pinning tests for the per-worktree router network #55 gives every
// routed worktree (docs/port/divergences.md row 31): there is no TypeScript
// `it` to mirror here, since TypeScript keeps every routed service on one
// shared external network. TestOverrideCorpus itself only proves every
// routed corpus case still runs without error, having skipped the
// byte-for-byte comparison for them; these tests pin the actual shape that
// replaces it.

import (
	"testing"

	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/worktree"
)

// networkFixtureRef is the worktree.Ref every test in this file builds its
// override document for.
var networkFixtureRef = worktree.Ref{
	RepositoryID: "11111111-1111-1111-1111-111111111111",
	WorktreeID:   "22222222-2222-2222-2222-222222222222",
	Slug:         "feature-login",
}

// composeWithRoute is a *Compose configured with one route on service,
// routing to the shared router network "berth-router" under label namespace
// "dev.berth".
func composeWithRoute(service string, port *int) *Compose {
	return NewCompose(Options{
		ComposeFile: "/work/acme/docker/compose.yaml", ProjectRoot: "/work/acme",
		ConfigFile: "/work/acme/berth.toml", ProjectName: "acme",
		ControlHome: "/control", Domain: "acme.localhost", LabelNamespace: "dev.berth",
		Routes:        []RouteSpec{{Service: service, Port: port}},
		RouterNetwork: "berth-router",
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

func TestOverrideNetworkShape(t *testing.T) {
	t.Run("a routed worktree's override document declares its own owned router network, not the shared external one", func(t *testing.T) {
		services, volumes, networks := decodeModel(t, `{"services":{"web":{"expose":["8080"]}}}`)
		rt := composeWithRoute("web", nil)

		document, err := rt.overrideDocument(networkFixtureRef, services, volumes, networks)
		if err != nil {
			t.Fatalf("overrideDocument: %v", err)
		}
		outNetworks, _ := document.get("networks")
		networksObj := outNetworks.(*jsonKV)
		routerNet, present := networksObj.get(routerNetworkKey)
		if !present {
			t.Fatalf("no %q network declared: %+v", routerNetworkKey, networksObj.keys)
		}
		routerNetObj := routerNet.(*jsonKV)
		if _, external := routerNetObj.get("external"); external {
			t.Fatal("router network is external; want owned")
		}
		name, _ := routerNetObj.get("name")
		wantName := RouterNetworkName(networkFixtureRef.RepositoryID, networkFixtureRef.WorktreeID)
		if name != wantName {
			t.Fatalf("network name = %v, want %v", name, wantName)
		}
		labelsValue, present := routerNetObj.get("labels")
		if !present {
			t.Fatal("router network declares no labels")
		}
		labelsObj := labelsValue.(*jsonKV)
		routedBy, _ := labelsObj.get("dev.berth.router")
		if routedBy != "berth-router" {
			t.Fatalf("dev.berth.router label = %v, want berth-router", routedBy)
		}
		managed, _ := labelsObj.get("dev.berth.managed")
		if managed != "true" {
			t.Fatalf("dev.berth.managed label = %v, want true", managed)
		}
		repositoryLabel, _ := labelsObj.get("dev.berth.repository-uuid")
		if repositoryLabel != networkFixtureRef.RepositoryID {
			t.Fatalf("repository-uuid label = %v, want %v", repositoryLabel, networkFixtureRef.RepositoryID)
		}
	})

	t.Run("a routed service joins the router network under exactly one alias, its own route name", func(t *testing.T) {
		services, volumes, networks := decodeModel(t, `{"services":{"web":{"expose":["8080"]}}}`)
		rt := composeWithRoute("web", nil)

		document, err := rt.overrideDocument(networkFixtureRef, services, volumes, networks)
		if err != nil {
			t.Fatalf("overrideDocument: %v", err)
		}
		outServices, _ := document.get("services")
		webEntry := outServices.(*jsonKV).values["web"].(*jsonKV)
		networksValue, present := webEntry.get("networks")
		if !present {
			t.Fatal("routed service declares no networks")
		}
		// The service declares no explicit `networks` key, so it also joins
		// Compose's own implicit default network, unaffected by routing;
		// routerNetworkKey is appended alongside it, not in place of it.
		webNetworks := networksValue.(*jsonKV)
		if len(webNetworks.keys) != 2 || webNetworks.keys[0] != "default" || webNetworks.keys[1] != routerNetworkKey {
			t.Fatalf("service networks = %v, want exactly [default %s]", webNetworks.keys, routerNetworkKey)
		}
		routerEntry := webNetworks.values[routerNetworkKey].(*jsonKV)
		aliasesValue, present := routerEntry.get("aliases")
		if !present {
			t.Fatal("router network entry declares no aliases")
		}
		aliases, ok := aliasesValue.([]string)
		if !ok || len(aliases) != 1 {
			t.Fatalf("aliases = %#v, want exactly one alias", aliasesValue)
		}
		wantName := RouteName(networkFixtureRef, RouteSpec{Service: "web"})
		if aliases[0] != wantName {
			t.Fatalf("alias = %q, want %q", aliases[0], wantName)
		}
		wantProject := layout.ComposeProjectName(networkFixtureRef.RepositoryID, networkFixtureRef.WorktreeID)
		if wantName != wantProject+"-web" {
			t.Fatalf("RouteName = %q, want %q", wantName, wantProject+"-web")
		}
	})

	t.Run("a routed service carries no Traefik container labels: nothing reads them once the Docker provider is gone", func(t *testing.T) {
		services, volumes, networks := decodeModel(t, `{"services":{"web":{"expose":["8080"]}}}`)
		rt := composeWithRoute("web", nil)

		document, err := rt.overrideDocument(networkFixtureRef, services, volumes, networks)
		if err != nil {
			t.Fatalf("overrideDocument: %v", err)
		}
		outServices, _ := document.get("services")
		webEntry := outServices.(*jsonKV).values["web"].(*jsonKV)
		labelsValue, _ := webEntry.get("labels")
		labelsObj := labelsValue.(*jsonKV)
		for _, key := range labelsObj.keys {
			if key == "traefik.enable" || key == "dev.berth.router" {
				t.Fatalf("service carries a Traefik/routing label %q; want ownership labels only", key)
			}
		}
	})

	t.Run("refuses outright rather than repurposing a top-level network the consumer already declares under the reserved key", func(t *testing.T) {
		services, volumes, networks := decodeModel(t, `{"services":{"web":{"expose":["8080"]}},"networks":{"`+routerNetworkKey+`":{}}}`)
		rt := composeWithRoute("web", nil)

		_, err := rt.overrideDocument(networkFixtureRef, services, volumes, networks)
		if err == nil {
			t.Fatal("expected a refusal, got nil")
		}
		wantSubstr := "/work/acme/docker/compose.yaml already declares a network named " + routerNetworkKey + ", which berth reserves for its own per-worktree router network. Rename it."
		if err.Error() != wantSubstr {
			t.Fatalf("error = %q, want %q", err.Error(), wantSubstr)
		}
	})

	t.Run("refuses outright rather than repurposing a routed service's own network entry under the reserved key", func(t *testing.T) {
		services, volumes, networks := decodeModel(t, `{"services":{"web":{"expose":["8080"],"networks":{"`+routerNetworkKey+`":{}}}}}`)
		rt := composeWithRoute("web", nil)

		_, err := rt.overrideDocument(networkFixtureRef, services, volumes, networks)
		if err == nil {
			t.Fatal("expected a refusal, got nil")
		}
		wantSubstr := "Service web in /work/acme/docker/compose.yaml already declares a network named " + routerNetworkKey + ", which berth reserves for its own per-worktree router network. Rename it."
		if err.Error() != wantSubstr {
			t.Fatalf("error = %q, want %q", err.Error(), wantSubstr)
		}
	})

	t.Run("RouterNetworkName is pure and stable for the same repository and worktree IDs", func(t *testing.T) {
		a := RouterNetworkName(networkFixtureRef.RepositoryID, networkFixtureRef.WorktreeID)
		b := RouterNetworkName(networkFixtureRef.RepositoryID, networkFixtureRef.WorktreeID)
		if a != b {
			t.Fatalf("RouterNetworkName is not stable: %q vs %q", a, b)
		}
		other := RouterNetworkName("33333333-3333-3333-3333-333333333333", networkFixtureRef.WorktreeID)
		if a == other {
			t.Fatalf("RouterNetworkName collides across repositories: %q", a)
		}
	})
}
