package commands

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/router"
)

// writeSharedRecordFor plants a shared-service record routing mailpit's web
// UI on container port 8025, pinned to hostPort, under controlHome.
func writeSharedRecordFor(t *testing.T, controlHome, repository string, hostPort int) {
	t.Helper()
	store := registry.NewStore(controlHome, repository)
	routed := 8025
	if _, err := store.SyncSharedPorts(t.Context(), registry.Writer{Invocation: "test", UserHome: registrytest.TempDir(t)}, registry.SharedPortPlan{
		Name: "mailpit", Domain: "acme.localhost", Ports: []int{8025}, Routed: &routed,
		Range: layout.PortRange{Low: hostPort, High: hostPort}, Probe: registrytest.FreeProbe{},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSharedRoutesInDoctor(t *testing.T) {
	t.Run("the route probe covers a shared service's host", func(t *testing.T) {
		state := routerStateWithRoutes(t, nil)
		if err := router.WriteSharedFragment(state, routeRepo, "mailpit", "acme.localhost", 30777); err != nil {
			t.Fatal(err)
		}
		got, err := routeTargets(state)
		if err != nil {
			t.Fatal(err)
		}
		want := routeTarget{claim: "mailpit.shared.acme.localhost", host: "mailpit.shared.acme.localhost", path: "/", targetPort: 30777}
		found := false
		for _, target := range got {
			found = found || target == want
		}
		if !found {
			t.Fatalf("targets = %+v, want one %+v", got, want)
		}
	})

	t.Run("a shared route and a service route claiming one host conflict, each named", func(t *testing.T) {
		state := routerStateWithRoutes(t, map[string]string{"web": "Host(`mailpit.shared.acme.localhost`)"})
		if err := router.WriteSharedFragment(state, routeRepo, "mailpit", "acme.localhost", 30777); err != nil {
			t.Fatal(err)
		}
		checks, err := routeChecks(state, registrytest.TempDir(t), "treerig")
		if err != nil {
			t.Fatal(err)
		}
		var conflicts []Check
		for _, check := range checks {
			if check.ID == "router.route-conflict" {
				conflicts = append(conflicts, check)
			}
		}
		if len(conflicts) != 1 || !strings.Contains(conflicts[0].Detail, "mailpit (shared service of repository "+routeRepo+")") {
			t.Fatalf("conflicts = %+v", conflicts)
		}
	})

	t.Run("a shared route file counts among the route files", func(t *testing.T) {
		state := layout.RouterLayout{DynamicDir: registrytest.TempDir(t)}
		if err := router.WriteSharedFragment(state, routeRepo, "mailpit", "acme.localhost", 30777); err != nil {
			t.Fatal(err)
		}
		checks, err := routeChecks(state, registrytest.TempDir(t), "treerig")
		if err != nil {
			t.Fatal(err)
		}
		if checks[0].ID != "router.routes" || checks[0].Detail != "1 route file; no two routers share a rule" {
			t.Fatalf("checks = %+v", checks)
		}
	})

	t.Run("orphaned routes: warns about a shared file whose record holds another port", func(t *testing.T) {
		state := layout.RouterLayout{DynamicDir: registrytest.TempDir(t)}
		if err := router.WriteSharedFragment(state, routeRepo, "mailpit", "acme.localhost", 30501); err != nil {
			t.Fatal(err)
		}
		controlHome := registrytest.TempDir(t)
		registrytest.ClaimControlHome(t, controlHome)
		writeSharedRecordFor(t, controlHome, routeRepo, 30777)
		got, err := orphanedRoutesChecks(state, controlHome, "treerig")
		want := Check{ID: "router.orphaned-routes", Status: StatusWarn, Subject: "routes",
			Detail: "1 route file disagrees with its registration or shared-service record (shared service mailpit: points at port 30501, but its record says 30777); the next 'treerig up' or 'treerig router up' repairs it"}
		if err != nil || len(got) != 1 || got[0] != want {
			t.Fatalf("got %+v, %v", got, err)
		}
	})

	t.Run("orphaned routes: a shared file of a repository this control home does not know is a notice", func(t *testing.T) {
		state := layout.RouterLayout{DynamicDir: registrytest.TempDir(t)}
		if err := router.WriteSharedFragment(state, routeRepo, "mailpit", "acme.localhost", 30501); err != nil {
			t.Fatal(err)
		}
		got, err := orphanedRoutesChecks(state, registrytest.TempDir(t), "treerig")
		if err != nil || len(got) != 1 || got[0].Instance != "other-home" || got[0].Status != StatusOK ||
			!strings.Contains(got[0].Detail, filepath.Base(router.SharedFragmentPath(state, routeRepo, "mailpit"))) ||
			!strings.Contains(got[0].Detail, "no registration or shared-service record of this control home") ||
			!strings.Contains(got[0].Detail, "worktree or shared service is gone") {
			t.Fatalf("got %+v, %v", got, err)
		}
	})

	t.Run("a conflict names a shared service by its repository's worktree when one is registered", func(t *testing.T) {
		claims := sharedClaims([]router.SharedFragmentInfo{{RepositoryID: routeRepo, Name: "mailpit"}},
			map[string]registry.RegisteredWorktree{"k": {RepositoryID: routeRepo, Slug: "acme-main", Domain: "acme.localhost"}})
		if want := "mailpit (shared service of the repository of the worktree acme-main.acme.localhost)"; claims[0].identity != want {
			t.Errorf("identity = %q, want %q", claims[0].identity, want)
		}
	})
}
