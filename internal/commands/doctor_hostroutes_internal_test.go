package commands

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/router"
)

const (
	hostRepo = "11111111-1111-4111-8111-111111111111"
	hostWt   = "22222222-2222-4222-8222-222222222221"
)

// answersOn is a ports.ListenerProbe answering from a fixed set of addresses.
type answersOn map[string]bool

// Answers is ports.ListenerProbe's Answers.
func (a answersOn) Answers(_ context.Context, host string, port int) bool {
	return a[net.JoinHostPort(host, strconv.Itoa(port))]
}

// modeSource is a doctorRouterSource whose only answer is the proxy's mode.
type modeSource struct {
	doctorRouterSource
	choice router.ModeChoice
	err    error
}

// ModeChoice is doctorRouterSource's ModeChoice.
func (m modeSource) ModeChoice(context.Context) (router.ModeChoice, error) { return m.choice, m.err }

// hostRegistration is a registered worktree with one host route per port.
func hostRegistration(ports ...int) *registry.WorktreeRegistration {
	record := registry.RegisteredWorktree{RepositoryID: hostRepo, WorktreeID: hostWt, Slug: "feature-login", Domain: "acme.localhost"}
	for i, port := range ports {
		record.HostRoutes = append(record.HostRoutes, registry.HostRoute{Name: "web" + strconv.Itoa(i), Port: port, Subdomain: "app" + strconv.Itoa(i)})
	}
	return &registry.WorktreeRegistration{RegisteredWorktree: record}
}

func TestHostRouteChecks(t *testing.T) {
	ctx := context.Background()
	bridge := modeSource{choice: router.ModeChoice{Mode: router.ModeBridge}}
	host := modeSource{choice: router.ModeChoice{Mode: router.ModeHost}}

	// withFile writes the route file the record produces for registration's
	// routes, so a check about listeners is not also about a missing file.
	withFile := func(t *testing.T, registration *registry.WorktreeRegistration) layout.RouterLayout {
		t.Helper()
		state := layout.RouterStateFrom(registrytest.TempDir(t))
		for _, route := range registration.HostRoutes {
			doc := hostRouteDocument(registration.RegisteredWorktree, route)
			if err := router.WriteHostFragment(state, hostRepo, hostWt, route.Name, doc); err != nil {
				t.Fatal(err)
			}
		}
		return state
	}
	options := func(state layout.RouterLayout, listeners answersOn, source doctorRouterSource) doctorOptions {
		return doctorOptions{CommandName: "berth", RouterState: state, Listeners: listeners, Router: source, DockerAnswered: true}
	}
	single := func(t *testing.T, checks []Check) Check {
		t.Helper()
		if len(checks) != 1 {
			t.Fatalf("checks = %+v", checks)
		}
		return checks[0]
	}

	t.Run("is ok when 127.0.0.1 answers", func(t *testing.T) {
		registration := hostRegistration(5173)
		check := single(t, hostRouteChecks(ctx, options(withFile(t, registration), answersOn{"127.0.0.1:5173": true}, bridge), registration))
		if check.Status != StatusOK || check.ID != "checkout.host-routes" || check.Instance != "web0" || check.Subject != "web0 host route" || check.Detail != "127.0.0.1:5173 answers" {
			t.Fatalf("check = %+v", check)
		}
	})

	t.Run("says without a warning that nothing is listening: a stopped dev server is normal", func(t *testing.T) {
		registration := hostRegistration(5173)
		check := single(t, hostRouteChecks(ctx, options(withFile(t, registration), answersOn{}, bridge), registration))
		if check.Status != StatusOK || check.Detail != "nothing is listening on 127.0.0.1:5173; requests for it get a 502 until a process is" {
			t.Fatalf("check = %+v", check)
		}
	})

	t.Run("warns to bind 127.0.0.1 when only ::1 answers in bridge mode", func(t *testing.T) {
		registration := hostRegistration(5173)
		check := single(t, hostRouteChecks(ctx, options(withFile(t, registration), answersOn{"[::1]:5173": true}, bridge), registration))
		want := "Port 5173 answers on ::1 but not on 127.0.0.1. The router cannot reach ::1 on Docker Desktop and OrbStack. Bind the process to 127.0.0.1 instead (its host or listen address setting)."
		if check.Status != StatusWarn || check.Detail != want {
			t.Fatalf("check = %+v", check)
		}
	})

	t.Run("is ok, with the advice, when only ::1 answers and the proxy is in host mode", func(t *testing.T) {
		registration := hostRegistration(5173)
		check := single(t, hostRouteChecks(ctx, options(withFile(t, registration), answersOn{"[::1]:5173": true}, host), registration))
		if check.Status != StatusOK || !strings.Contains(check.Detail, "the router in host mode reaches ::1; bind 127.0.0.1 to work on Docker Desktop and OrbStack too") {
			t.Fatalf("check = %+v", check)
		}
	})

	t.Run("warns when only ::1 answers and the mode cannot be read, or there is no router or no Docker", func(t *testing.T) {
		registration := hostRegistration(5173)
		state := withFile(t, registration)
		unreadable := modeSource{err: errors.New("no docker")}
		noDocker := options(state, answersOn{"[::1]:5173": true}, bridge)
		noDocker.DockerAnswered = false
		for name, opts := range map[string]doctorOptions{
			"unreadable": options(state, answersOn{"[::1]:5173": true}, unreadable),
			"no router":  options(state, answersOn{"[::1]:5173": true}, nil),
			"no docker":  noDocker,
		} {
			check := single(t, hostRouteChecks(ctx, opts, registration))
			if check.Status != StatusWarn || !strings.Contains(check.Detail, "its mode could not be read here. Bind the process to 127.0.0.1") {
				t.Errorf("%s: check = %+v", name, check)
			}
		}
	})

	t.Run("warns when a recorded route has no route file, saying how to put it back", func(t *testing.T) {
		registration := hostRegistration(5173)
		state := layout.RouterStateFrom(registrytest.TempDir(t))
		checks := hostRouteChecks(ctx, options(state, answersOn{"127.0.0.1:5173": true}, bridge), registration)
		if len(checks) != 2 {
			t.Fatalf("checks = %+v", checks)
		}
		file := checks[0]
		want := "its route file is missing from " + state.DynamicDir + ", so the router does not route it. Run 'berth route add web0 --port 5173 --subdomain app0' again with the same fields to write it ('berth route ls --json' lists them)."
		if file.Status != StatusWarn || file.ID != "checkout.host-route-files" || file.Instance != "web0" || file.Detail != want {
			t.Fatalf("file check = %+v", file)
		}
		if checks[1].ID != "checkout.host-routes" || checks[1].Status != StatusOK {
			t.Fatalf("listener check = %+v", checks[1])
		}
	})

	t.Run("fails, naming the error, when the route file cannot be looked for", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads any directory")
		}
		registration := hostRegistration(5173)
		state := withFile(t, registration)
		if err := os.Chmod(state.DynamicDir, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(state.DynamicDir, 0o755) })
		checks := hostRouteChecks(ctx, options(state, answersOn{"127.0.0.1:5173": true}, bridge), registration)
		if len(checks) != 2 {
			t.Fatalf("checks = %+v", checks)
		}
		file := checks[0]
		if file.Status != StatusFail || file.ID != "checkout.host-route-files" || file.Instance != "web0" || !strings.Contains(file.Detail, "permission denied") {
			t.Fatalf("file check = %+v, want a failure naming the error, not a missing file", file)
		}
	})

	t.Run("one line per route, in the record's order", func(t *testing.T) {
		registration := hostRegistration(5173, 5174)
		checks := hostRouteChecks(ctx, options(withFile(t, registration), answersOn{"127.0.0.1:5174": true}, bridge), registration)
		if len(checks) != 2 || checks[0].Instance != "web0" || checks[0].Status != StatusOK || checks[1].Instance != "web1" || checks[1].Status != StatusOK {
			t.Fatalf("checks = %+v", checks)
		}
	})

	t.Run("asks the proxy's mode only when it matters", func(t *testing.T) {
		registration := hostRegistration(5173)
		asked := &countingModes{}
		opts := options(withFile(t, registration), answersOn{"127.0.0.1:5173": true}, asked)
		hostRouteChecks(ctx, opts, registration)
		if asked.n != 0 {
			t.Fatalf("asked the mode %d times for a route that answers on 127.0.0.1", asked.n)
		}
	})
}

// countingModes is a doctorRouterSource that counts ModeChoice calls.
type countingModes struct {
	doctorRouterSource
	n int
}

// ModeChoice is doctorRouterSource's ModeChoice.
func (c *countingModes) ModeChoice(context.Context) (router.ModeChoice, error) {
	c.n++
	return router.ModeChoice{Mode: router.ModeBridge}, nil
}

func TestLookupCarriesHostRoutesAsTheFilesMustSayThem(t *testing.T) {
	controlHome := registrytest.TempDir(t)
	path := "/v1"
	if _, err := registrytest.WriteRecord(controlHome, registrytest.RecordJSON{
		Version: 1, RepositoryID: hostRepo, WorktreeID: hostWt, Slug: "feature-a", Domain: "acme.localhost", Initialization: "complete",
		HostRoutes: []registrytest.HostRouteJSON{
			{Name: "web", Port: 5173},
			{Name: "api", Port: 8080, Subdomain: "api", Path: &path},
		},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := controlHomeRegistrations{controlHome: controlHome}.Lookup(hostRepo, hostWt)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]router.HostTarget{
		"web": {Rule: "Host(`feature-a.acme.localhost`)", Port: 5173},
		"api": {Rule: "Host(`api.feature-a.acme.localhost`) && PathPrefix(`/v1`)", Port: 8080},
	}
	if len(got.HostRoutes) != 2 || got.HostRoutes["web"] != want["web"] || got.HostRoutes["api"] != want["api"] {
		t.Fatalf("HostRoutes = %+v, want %+v", got.HostRoutes, want)
	}
}

func TestDoctorReportsHostRouteFiles(t *testing.T) {
	controlHome := registrytest.TempDir(t)
	if _, err := registrytest.WriteRecord(controlHome, registrytest.RecordJSON{
		Version: 1, RepositoryID: hostRepo, WorktreeID: hostWt, Slug: "feature-a", Domain: "acme.localhost", Initialization: "complete",
		HostRoutes: []registrytest.HostRouteJSON{{Name: "web", Port: 5173}},
	}); err != nil {
		t.Fatal(err)
	}
	state := layout.RouterStateFrom(registrytest.TempDir(t))
	write := func(name, rule string, port int) {
		t.Helper()
		if err := router.WriteHostFragment(state, hostRepo, hostWt, name, router.HostFragmentDocument(hostRepo, hostWt, name, rule, port)); err != nil {
			t.Fatal(err)
		}
	}
	write("web", "Host(`feature-a.acme.localhost`)", 5174)
	write("gone", "Host(`gone.feature-a.acme.localhost`)", 1)

	checks, err := orphanedRoutesChecks(state, controlHome, "berth")
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 || checks[0].Status != StatusWarn || checks[0].ID != "router.orphaned-routes" {
		t.Fatalf("checks = %+v", checks)
	}
	detail := checks[0].Detail
	for _, want := range []string{
		"2 route files disagree with the registrations",
		"web (host route): routes Host(`feature-a.acme.localhost`) to port 5174, but its registration records Host(`feature-a.acme.localhost`) to port 5173",
		"gone (host route): its registration records no such host route",
		"the next 'berth up' or 'berth router up' repairs them",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, detail)
		}
	}
}

func TestRouteChecksSeesHostRouteConflicts(t *testing.T) {
	controlHome := registrytest.TempDir(t)
	state := layout.RouterStateFrom(registrytest.TempDir(t))
	const otherWt = "22222222-2222-4222-8222-222222222222"
	if _, err := registrytest.WriteRecord(controlHome, registrytest.RecordJSON{
		Version: 1, RepositoryID: hostRepo, WorktreeID: hostWt, Slug: "feature-a", Domain: "acme.localhost", Initialization: "complete",
		Ports: map[string]int{"api": 30010}, HostRoutes: []registrytest.HostRouteJSON{{Name: "web", Port: 5173}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := registrytest.WriteRecord(controlHome, registrytest.RecordJSON{
		Version: 1, RepositoryID: hostRepo, WorktreeID: otherWt, Slug: "feature-b", Domain: "acme.localhost", Initialization: "complete",
		Ports: map[string]int{"api": 30011},
	}); err != nil {
		t.Fatal(err)
	}
	// feature-b's api service and feature-a's host route both claim the same host.
	rule := "Host(`feature-a.acme.localhost`)"
	if err := router.WriteHostFragment(state, hostRepo, hostWt, "web", router.HostFragmentDocument(hostRepo, hostWt, "web", rule, 5173)); err != nil {
		t.Fatal(err)
	}
	if err := router.WriteFragment(state, hostRepo, otherWt, "api", router.NewFragmentDocument("p-api", rule, 30011)); err != nil {
		t.Fatal(err)
	}
	checks, err := routeChecks(state, controlHome, "berth")
	if err != nil {
		t.Fatal(err)
	}
	var conflict *Check
	for i := range checks {
		if checks[i].ID == "router.route-conflict" {
			conflict = &checks[i]
		}
	}
	if conflict == nil || conflict.Status != StatusFail ||
		!strings.Contains(conflict.Detail, "feature-a.acme.localhost (host route web)") || !strings.Contains(conflict.Detail, "feature-b.acme.localhost (api)") {
		t.Fatalf("checks = %+v", checks)
	}
}

func TestDoctorCheckoutSectionListsHostRoutes(t *testing.T) {
	f := newCheckoutFixture(t, "")
	record := f.register("feature-login")
	if _, err := registrytest.WriteRecord(f.controlHome, registrytest.RecordJSON{
		Version: 1, RepositoryID: record.RepositoryID, WorktreeID: record.WorktreeID,
		Slug: record.Slug, Domain: record.Domain, Initialization: "complete",
		HostRoutes: []registrytest.HostRouteJSON{{Name: "web", Port: 5173, Subdomain: "app"}},
	}); err != nil {
		t.Fatal(err)
	}
	opts := f.opts()
	opts.Listeners = answersOn{"127.0.0.1:5173": true}
	opts.RouterState = layout.RouterStateFrom(registrytest.TempDir(t))
	sections, err := doctorSections(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	checkReport(t, sections)
	var routes, files []Check
	for _, check := range sectionByID(sections, "checkout") {
		switch check.ID {
		case "checkout.host-routes":
			routes = append(routes, check)
		case "checkout.host-route-files":
			files = append(files, check)
		}
	}
	if len(routes) != 1 || routes[0].Status != StatusOK || routes[0].Instance != "web" {
		t.Fatalf("host route lines = %+v", routes)
	}
	if len(files) != 1 || files[0].Status != StatusWarn {
		t.Fatalf("a route with no file should warn: %+v", files)
	}
}

func TestRouteChecksSeesAHostRouteAndASharedServiceClaimingOneRule(t *testing.T) {
	controlHome := registrytest.TempDir(t)
	state := layout.RouterStateFrom(registrytest.TempDir(t))
	if _, err := registrytest.WriteRecord(controlHome, registrytest.RecordJSON{
		Version: 1, RepositoryID: hostRepo, WorktreeID: hostWt, Slug: "feature-a", Domain: "acme.localhost", Initialization: "complete",
		HostRoutes: []registrytest.HostRouteJSON{{Name: "web", Port: 5173}},
	}); err != nil {
		t.Fatal(err)
	}
	// A shared service's UI is routed at mailpit.shared.acme.localhost; a host
	// route written for that very host (a hand-edited record, or a domain
	// whose slug allocator did not reserve "shared") claims the same rule.
	rule := router.SharedRouteRule("mailpit", "acme.localhost")
	if err := router.WriteSharedFragment(state, hostRepo, "mailpit", "acme.localhost", 30777); err != nil {
		t.Fatal(err)
	}
	if err := router.WriteHostFragment(state, hostRepo, hostWt, "web", router.HostFragmentDocument(hostRepo, hostWt, "web", rule, 5173)); err != nil {
		t.Fatal(err)
	}
	checks, err := routeChecks(state, controlHome, "berth")
	if err != nil {
		t.Fatal(err)
	}
	var conflict *Check
	for i := range checks {
		if checks[i].ID == "router.route-conflict" {
			conflict = &checks[i]
		}
	}
	if conflict == nil || conflict.Status != StatusFail ||
		!strings.Contains(conflict.Detail, rule+" is claimed by") ||
		!strings.Contains(conflict.Detail, "feature-a.acme.localhost (host route web)") ||
		!strings.Contains(conflict.Detail, "mailpit (shared service of ") {
		t.Fatalf("checks = %+v", checks)
	}
}
