//go:build integration

package commands_test

// Covers the router-and-doctor edge cases that need a full worktree on a
// real daemon (issue #69): a worktree's second service routed on its own
// subdomain, its services reaching each other by name over the worktree's
// own network, an unmatched host on the same router still getting its own
// 404, and doctor flagging a real second container that claims a worktree's
// host. These need both the Compose backend's `up` (exercised end to end by
// this file's own newWalk-based sibling, up_docker_integration_test.go) and
// `doctor` (internal/commands/doctor.go), so they live here rather than in
// internal/router/router_integration_test.go.
//
// Assertions stick to what a user can observe through the router — a host
// answers, an unmatched one 404s, one service reaches another by name — and
// to doctor's own report text, never to the routing labels' own mechanics,
// so they survive #55's move to file-provider routing.

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/router"
)

// routesCompose is two tiny HTTP servers, so `up` has a second service to
// route on its own subdomain and a name for the first to reach it by.
const routesCompose = `services:
  api:
    image: busybox:1.37.0
    init: true
    command: ["sh", "-c", "mkdir -p /www && echo 'hello from api' > /www/index.html && exec httpd -f -p 8080 -h /www"]
    expose: ["8080"]
  worker:
    image: busybox:1.37.0
    init: true
    command: ["sh", "-c", "mkdir -p /www && echo 'hello from worker' > /www/index.html && exec httpd -f -p 9090 -h /www"]
    expose: ["9090"]
`

// routesToml is berth.toml for routesCompose: api at the worktree host
// itself, worker on the "jobs" subdomain under it. %s takes the run's own
// label namespace.
const routesToml = `[project]
name = "routes"
domain = "routes.localhost"
label-namespace = %q

[compose]
file = "compose.yaml"

[[service]]
name = "api"

[[service]]
name = "worker"
subdomain = "jobs"
`

// newRoutesWalk is newWalkFixture's own recipe, over routesCompose/routesToml
// instead of walkCompose/walkToml: two services, one of them on a
// subdomain, and no hooks — this file's tests never look at hook stages.
func newRoutesWalk(t *testing.T) *walk {
	t.Helper()
	return newWalkFixture(t, "routes", "feature/route", routesToml, routesCompose)
}

// routerChecks is the "Router" section's own checks out of doctor's report,
// or nil if doctor produced no such section.
func routerChecks(sections []commands.Section) []commands.Check {
	for _, section := range sections {
		if section.Title == "Router" {
			return section.Checks
		}
	}
	return nil
}

func TestWorktreeSubdomainRoutingAndDoctorConflictOnARealDaemon(t *testing.T) {
	w := newRoutesWalk(t)
	const host = "feature-route.routes.localhost"
	const workerHost = "jobs." + host

	result := w.up(t)

	// The worktree's second service answers on its own subdomain, alongside the first at the host root.
	w.eventually(t, host, func(status int, body string) bool { return status == http.StatusOK && body == "hello from api\n" })
	w.eventually(t, workerHost, func(status int, body string) bool { return status == http.StatusOK && body == "hello from worker\n" })

	// An unmatched host on the same router still gets Traefik's own 404, not either service's page.
	if status, body := w.get("nobody." + host); status != http.StatusNotFound {
		t.Fatalf("unmatched host answered %d %q", status, body)
	}

	// Service discovery inside the worktree: api reaches worker by name, over the worktree's own network.
	project := layout.ComposeProjectName(result.Registration.RepositoryID, result.Registration.WorktreeID)
	apiIDs := dockerIDs(t, "compose", "-p", project, "ps", "-q", "api")
	if len(apiIDs) != 1 {
		t.Fatalf("api container ids = %v", apiIDs)
	}
	reached, err := exec.Command("docker", "exec", apiIDs[0], "wget", "-qO-", "http://worker:9090/").Output()
	if err != nil {
		t.Fatalf("api reaching worker by name: %v", err)
	}
	if string(reached) != "hello from worker\n" {
		t.Fatalf("worker over the worktree's own network answered %q", reached)
	}

	// doctor sees both services' own route fragments, and no two routers
	// sharing a rule yet. Under #55 doctor reads route fragments
	// (router.ListFragments), never container labels, so the Router section
	// gains more checks than a pre-#55 build's own two (status, routes):
	// status, its owner (#82), routes, doctor's address-pools check, and the
	// port it resolved and where that came from (#79).
	sections, err := commands.DoctorSections(context.Background(), w.env, w.svc, w.state)
	if err != nil {
		t.Fatalf("DoctorSections: %v", err)
	}
	checks := routerChecks(sections)
	if len(checks) != 5 {
		t.Fatalf("router checks before the impostor = %+v", checks)
	}
	if last := checks[4]; last.Status != commands.StatusOK || last.Subject != "router port" || !strings.Contains(last.Detail, fmt.Sprintf("port %d from BERTH_ROUTER_PORT", w.port)) {
		t.Fatalf("router port before the impostor = %+v", last)
	}
	if checks[0].Status != commands.StatusOK || !strings.Contains(checks[0].Detail, fmt.Sprintf("running on 127.0.0.1:%d", w.port)) || !strings.Contains(checks[0].Detail, w.svc.Network) {
		t.Fatalf("router status before the impostor = %+v", checks[0])
	}
	if checks[1].Status != commands.StatusOK || checks[1].Subject != "router owner" || !strings.HasPrefix(checks[1].Detail, "this berth root (") {
		t.Fatalf("router owner before the impostor = %+v", checks[1])
	}
	if checks[2].Status != commands.StatusOK || !strings.Contains(checks[2].Detail, "2 route fragments; no two routers share a rule") {
		t.Fatalf("routes before the impostor = %+v", checks[2])
	}

	// Not a Compose service, so not a replica of anything: a second,
	// unrelated route fragment claiming the worktree's own host. Under #55
	// only a fragment claims a route at all — Traefik's Docker-provider
	// discovery is gone, so a container's own labels do nothing — so this
	// writes one directly rather than labelling a container, proving doctor
	// reads a claimed route from the fragment itself.
	impostorRepositoryID, impostorWorktreeID := "33333333-3333-3333-3333-333333333333", "44444444-4444-4444-4444-444444444444"
	t.Cleanup(func() { _ = router.RemoveWorktreeFragments(w.state, impostorRepositoryID, impostorWorktreeID) })
	impostorDoc := router.NewFragmentDocument("impostor", fmt.Sprintf("Host(`%s`)", host), "impostor", 8080)
	if err := router.WriteFragment(w.state, impostorRepositoryID, impostorWorktreeID, "impostor", impostorDoc); err != nil {
		t.Fatalf("writing the impostor's route fragment: %v", err)
	}

	sections, err = commands.DoctorSections(context.Background(), w.env, w.svc, w.state)
	if err != nil {
		t.Fatalf("DoctorSections with the impostor's fragment written: %v", err)
	}
	checks = routerChecks(sections)
	var conflict *commands.Check
	for i := range checks {
		if checks[i].Status == commands.StatusFail && strings.Contains(checks[i].Detail, "is claimed by") {
			conflict = &checks[i]
		}
	}
	if conflict == nil {
		t.Fatalf("expected a claimed-route failure once the impostor's fragment existed, got %+v", checks)
	}
	wantClaimant := fmt.Sprintf("%s.%s.impostor", impostorRepositoryID, impostorWorktreeID)
	if !strings.HasPrefix(conflict.Detail, fmt.Sprintf("Host(`%s`) is claimed by ", host)) ||
		!strings.Contains(conflict.Detail, host+" (api)") || !strings.Contains(conflict.Detail, wantClaimant) {
		t.Fatalf("conflict detail = %q", conflict.Detail)
	}

	if err := router.RemoveWorktreeFragments(w.state, impostorRepositoryID, impostorWorktreeID); err != nil {
		t.Fatalf("removing the impostor's route fragment: %v", err)
	}
	w.downAndRemove(t, result, host)
}
