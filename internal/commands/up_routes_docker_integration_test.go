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
// so they survive changes to how routes are carried.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/checkoutindex"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/router"
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

// routesToml is treerig.toml for routesCompose: api at the worktree host
// itself, worker on the "jobs" subdomain under it. %s takes the run's own
// label namespace.
const routesToml = `[project]
name = "routes"
domain = "routes.localhost"
label-namespace = %q

[compose]
file = "compose.yaml"

[service.api]

[service.worker]
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

	// Service discovery inside the worktree: api reaches worker by name, over the worktree's own default network.
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

	// The routes are files naming loopback ports, and no network of the
	// worktree's carries a route.
	w.requireNoRoutingNetwork(t, result.Registration.WorktreeID)
	if routes := w.routeFiles(t, result.Registration.WorktreeID); len(routes) != 2 {
		t.Fatalf("route files = %+v", routes)
	}

	// doctor reads the route files and probes each route through the proxy.
	sections, err := commands.DoctorSections(context.Background(), w.env, w.svc, w.state)
	if err != nil {
		t.Fatalf("DoctorSections: %v", err)
	}
	checks := routerChecks(sections)
	byID := func(id string) []commands.Check {
		var found []commands.Check
		for _, check := range checks {
			if check.ID == id {
				found = append(found, check)
			}
		}
		return found
	}
	if status := byID("router.container"); len(status) != 1 || status[0].Status != commands.StatusOK ||
		!strings.Contains(status[0].Detail, fmt.Sprintf("running on 127.0.0.1:%d, host mode", w.port)) {
		t.Fatalf("router status before the impostor = %+v", status)
	}
	if owner := byID("router.owner"); len(owner) != 1 || owner[0].Status != commands.StatusOK || !strings.HasPrefix(owner[0].Detail, "this treerig root (") {
		t.Fatalf("router owner before the impostor = %+v", owner)
	}
	if mode := byID("router.mode"); len(mode) != 1 || mode[0].Status != commands.StatusOK || !strings.Contains(mode[0].Detail, "host mode") {
		t.Fatalf("router mode before the impostor = %+v", mode)
	}
	if bind := byID("router.bind"); len(bind) != 1 || bind[0].Status != commands.StatusOK {
		t.Fatalf("router bind before the impostor = %+v", bind)
	}
	if routes := byID("router.routes"); len(routes) != 1 || routes[0].Status != commands.StatusOK || !strings.Contains(routes[0].Detail, "2 route files; no two routers share a rule") {
		t.Fatalf("routes before the impostor = %+v", routes)
	}
	// Every route answers: the index, the host root and the subdomain.
	probes := byID("router.route-probe")
	answered := map[string]bool{}
	for _, probe := range probes {
		if probe.Status != commands.StatusOK {
			t.Fatalf("route probe = %+v", probe)
		}
		answered[probe.Instance] = true
	}
	for _, claim := range []string{router.IndexHost, host, workerHost} {
		if !answered[claim] {
			t.Fatalf("no passing probe for %s in %+v", claim, probes)
		}
	}
	if port := byID("router.port"); len(port) != 1 || port[0].Status != commands.StatusOK || !strings.Contains(port[0].Detail, fmt.Sprintf("port %d from TREERIG_ROUTER_PORT", w.port)) {
		t.Fatalf("router port before the impostor = %+v", port)
	}

	// A second, unrelated route file claiming the worktree's own host. Only a
	// route file claims a route, so this writes one directly rather than
	// labelling a container, proving doctor reads a claimed route from the
	// file itself.
	impostorRepositoryID, impostorWorktreeID := "33333333-3333-3333-3333-333333333333", "44444444-4444-4444-4444-444444444444"
	t.Cleanup(func() { _ = router.RemoveWorktreeFragments(w.state, impostorRepositoryID, impostorWorktreeID) })
	impostorDoc := router.NewFragmentDocument("impostor", fmt.Sprintf("Host(`%s`)", host), 8080)
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

// TestIndexIconsAreServedOnARealDaemon proves the favicon files reach a
// browser: after `up`, the page and every icon it links answer 200 with the
// right Content-Type, both on the index container's own loopback port and
// through the proxy at the index host, and the bytes served are the bytes
// written. The page also carries a Content-Security-Policy with no data:
// source, so these same-origin files are the only icons it can show.
func TestIndexIconsAreServedOnARealDaemon(t *testing.T) {
	w := newRoutesWalk(t)
	w.up(t)
	state, found, err := router.ReadState(w.state)
	if err != nil || !found || state.IndexPort == 0 {
		t.Fatalf("router state = %+v, found %v, err %v", state, found, err)
	}
	loopback := func(t *testing.T, path string) (*http.Response, []byte) {
		t.Helper()
		resp, err := walkClient.Get(fmt.Sprintf("http://127.0.0.1:%d%s", state.IndexPort, path))
		if err != nil {
			t.Fatalf("GET %s on the index's own port: %v", path, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading %s from the index's own port: %v", path, err)
		}
		return resp, body
	}
	proxied := func(t *testing.T, path string) (*http.Response, []byte) {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", w.port, path), nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Host = router.IndexHost
		resp, err := walkClient.Do(request)
		if err != nil {
			t.Fatalf("GET %s through the proxy: %v", path, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading %s through the proxy: %v", path, err)
		}
		return resp, body
	}
	// Traefik learns the index route from its file provider a moment after up.
	w.eventually(t, router.IndexHost, func(status int, _ string) bool { return status == http.StatusOK })

	for _, route := range []struct {
		via string
		get func(*testing.T, string) (*http.Response, []byte)
	}{{"the index's own port", loopback}, {"the proxy", proxied}} {
		get := route.get
		t.Run("the page and its icons answer 200 with their types through "+route.via, func(t *testing.T) {
			resp, page := get(t, "/index.html")
			if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
				t.Fatalf("index.html answered %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
			}
			if bytes.Contains(page, []byte("data:")) {
				t.Error("the page carries a data: source")
			}
			hrefs := regexp.MustCompile(`<link rel="icon" type="([^"]+)"[^>]* href="([^"]+)">`).FindAllSubmatch(page, -1)
			if len(hrefs) != len(checkoutindex.IconNames()) {
				t.Fatalf("the page links %d icons, want %d", len(hrefs), len(checkoutindex.IconNames()))
			}
			for _, link := range hrefs {
				name, declared := string(link[2]), string(link[1])
				resp, body := get(t, "/"+name)
				if resp.StatusCode != http.StatusOK {
					t.Errorf("%s answered %d", name, resp.StatusCode)
				}
				if got := resp.Header.Get("Content-Type"); got != declared {
					t.Errorf("%s is served as %q, but the page links it as %q", name, got, declared)
				}
				want, err := os.ReadFile(filepath.Join(w.state.IndexDir, name))
				if err != nil || len(body) == 0 || !bytes.Equal(body, want) {
					t.Errorf("%s served %d bytes, which are not the %d written (%v)", name, len(body), len(want), err)
				}
			}
		})
	}
}
