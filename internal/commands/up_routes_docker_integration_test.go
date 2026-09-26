//go:build integration

package commands_test

// Covers the router-and-doctor edge cases that need a full worktree on a
// real daemon (issue #69): a worktree's second service routed on its own
// subdomain, its services reaching each other by name over the worktree's
// own network, an unmatched host on the same router still getting its own
// 404, and doctor flagging a real second container that claims a worktree's
// host. Ported from the routing and doctor blocks of
// tests/integration/router-docker.integration.test.ts ("routes a worktree
// by hostname over the shared network..." and "lets doctor read the running
// router and its routes from Docker..."), which
// internal/router/router_integration_test.go's own header notes were not
// portable when WP3 landed: they needed the Compose backend's `up` (now
// exercised end to end by this file's own newWalk-based sibling,
// up_docker_integration_test.go) and the `doctor` port (now
// internal/commands/doctor.go), both landed since.
//
// Assertions stick to what a user can observe through the router — a host
// answers, an unmatched one 404s, one service reaches another by name — and
// to doctor's own report text, never to the routing labels' own mechanics,
// so they survive #55's move to file-provider routing.

import (
	"context"
	"errors"
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

	// doctor sees both services routed, and no two routers sharing a rule yet.
	sections, err := commands.DoctorSections(context.Background(), w.env, w.svc, w.state)
	if err != nil {
		t.Fatalf("DoctorSections: %v", err)
	}
	checks := routerChecks(sections)
	if len(checks) != 2 {
		t.Fatalf("router checks before the impostor = %+v", checks)
	}
	if checks[0].Status != commands.StatusOK || !strings.Contains(checks[0].Detail, fmt.Sprintf("running on 127.0.0.1:%d", w.port)) || !strings.Contains(checks[0].Detail, w.svc.Network) {
		t.Fatalf("router status before the impostor = %+v", checks[0])
	}
	if checks[1].Status != commands.StatusOK || !strings.Contains(checks[1].Detail, "3 routed container(s)") {
		t.Fatalf("routes before the impostor = %+v", checks[1])
	}

	// Not a Compose service, so not a replica of anything: a second, unrelated
	// container claiming the worktree's own host, the way TypeScript's own
	// "lets doctor read the running router..." case proves the same claim.
	impostorName := "impostor-" + randomSuffix(t)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "--force", impostorName).Run() })
	if _, err := exec.Command("docker", "run", "--detach", "--rm", "--name", impostorName,
		"--label", router.RoutedByLabel+"="+w.svc.Network,
		"--label", fmt.Sprintf("traefik.http.routers.impostor.rule=Host(`%s`)", host),
		"busybox:1.37.0", "sleep", "300").Output(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("starting the impostor: %v\n%s", err, exit.Stderr)
		}
		t.Fatalf("starting the impostor: %v", err)
	}

	sections, err = commands.DoctorSections(context.Background(), w.env, w.svc, w.state)
	if err != nil {
		t.Fatalf("DoctorSections with the impostor up: %v", err)
	}
	checks = routerChecks(sections)
	var conflict *commands.Check
	for i := range checks {
		if checks[i].Status == commands.StatusFail && strings.Contains(checks[i].Detail, "is claimed by") {
			conflict = &checks[i]
		}
	}
	if conflict == nil {
		t.Fatalf("expected a claimed-route failure once the impostor was up, got %+v", checks)
	}
	if !strings.HasPrefix(conflict.Detail, fmt.Sprintf("Host(`%s`) is claimed by ", host)) || !strings.Contains(conflict.Detail, impostorName) {
		t.Fatalf("conflict detail = %q", conflict.Detail)
	}

	if err := exec.Command("docker", "rm", "--force", impostorName).Run(); err != nil {
		t.Fatalf("removing the impostor: %v", err)
	}
	w.downAndRemove(t, result, host)
}
