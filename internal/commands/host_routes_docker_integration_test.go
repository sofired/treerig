//go:build integration

package commands_test

// Host routes (#6) on a real daemon, Linux native Engine, host mode: a
// listener on the host outside Docker answers at its worktree hostname
// through the real proxy, `down` keeps the route and `worktree remove`
// removes it. Docker Desktop and OrbStack cannot be tested here; the macOS
// check is in docs/design/routing.md (Testing).
//
// Assertions are what a user can observe: the hostname's answer, the route
// files that exist, and doctor's report text.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/router"
)

// hostServer is a listener on the host, outside Docker, answering every
// request with body. Close stops it.
type hostServer struct {
	port   int
	server *http.Server
}

// startHostServer listens on network/address ("tcp4" "127.0.0.1:0", or "tcp6"
// "[::1]:0") and serves body. It skips the test when the address cannot be
// bound, such as ::1 on a host without IPv6.
func startHostServer(t *testing.T, network, address, body string) *hostServer {
	t.Helper()
	listener, err := net.Listen(network, address)
	if err != nil {
		t.Skipf("cannot listen on %s: %v", address, err)
	}
	server := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, body) }),
		ReadHeaderTimeout: time.Second,
	}
	go func() { _ = server.Serve(listener) }()
	h := &hostServer{port: listener.Addr().(*net.TCPAddr).Port, server: server}
	t.Cleanup(h.Close)
	return h
}

// Close stops the server; closing twice is fine.
func (h *hostServer) Close() { _ = h.server.Close() }

// hostRouteFiles are the host route files the walk's router holds for worktreeID.
func (w *walk) hostRouteFiles(t *testing.T, worktreeID string) []router.FragmentInfo {
	t.Helper()
	fragments, err := router.ListHostFragments(w.state)
	if err != nil {
		t.Fatal(err)
	}
	var mine []router.FragmentInfo
	for _, fragment := range fragments {
		if fragment.WorktreeID == worktreeID {
			mine = append(mine, fragment)
		}
	}
	return mine
}

// routeProbeFor is doctor's router.route-probe line for claim, and whether
// there is one.
func routeProbeFor(t *testing.T, w *walk, claim string) (commands.Check, bool) {
	t.Helper()
	sections, err := commands.DoctorSections(context.Background(), w.env, w.svc, w.state)
	if err != nil {
		t.Fatalf("DoctorSections: %v", err)
	}
	for _, check := range routerChecks(sections) {
		if check.ID == "router.route-probe" && check.Instance == claim {
			return check, true
		}
	}
	return commands.Check{}, false
}

// checkoutHostRouteLines are doctor's checkout lines about host routes.
func checkoutHostRouteLines(t *testing.T, w *walk) []commands.Check {
	t.Helper()
	sections, err := commands.DoctorSections(context.Background(), w.env, w.svc, w.state)
	if err != nil {
		t.Fatalf("DoctorSections: %v", err)
	}
	var lines []commands.Check
	for _, section := range sections {
		for _, check := range section.Checks {
			if strings.HasPrefix(check.ID, "checkout.host-route") {
				lines = append(lines, check)
			}
		}
	}
	return lines
}

func TestHostRouteAnswersAtTheWorktreeHostnameOnARealDaemon(t *testing.T) {
	w := newRoutesWalk(t)
	const host = "feature-route.routes.localhost"
	const webHost = "app." + host
	ctx := context.Background()

	server := startHostServer(t, "tcp4", "127.0.0.1:0", "hello from the host\n")
	result := w.up(t)
	registration := result.Registration

	add := func(options commands.RouteAddOptions) {
		t.Helper()
		if err := commands.RouteAddWithContext(ctx, w.context(t), w.svc, options); err != nil {
			t.Fatalf("route add %s: %v", options.Name, err)
		}
	}
	add(commands.RouteAddOptions{Name: "web", Port: server.port, Subdomain: "app"})

	// The process on the host answers at its worktree hostname, through the proxy.
	w.eventually(t, webHost, func(status int, body string) bool { return status == http.StatusOK && body == "hello from the host\n" })
	// The services of the same worktree answer beside it.
	w.eventually(t, host, func(status int, body string) bool { return status == http.StatusOK && body == "hello from api\n" })
	if files := w.hostRouteFiles(t, registration.WorktreeID); len(files) != 1 || files[0].Service != "web" {
		t.Fatalf("host route files = %+v", files)
	}

	// doctor: the process answers on 127.0.0.1, the file is there.
	lines := checkoutHostRouteLines(t, w)
	if len(lines) != 1 || lines[0].ID != "checkout.host-routes" || lines[0].Status != commands.StatusOK ||
		lines[0].Detail != fmt.Sprintf("127.0.0.1:%d answers", server.port) {
		t.Fatalf("doctor's host route lines = %+v", lines)
	}

	// doctor probes the host route through the proxy, the one check of that hop.
	if probe, found := routeProbeFor(t, w, webHost); !found || probe.Status != commands.StatusOK {
		t.Fatalf("route probe for %s = %+v (found %v)", webHost, probe, found)
	}

	// A second `up` leaves the host route alone, and it keeps answering.
	w.up(t)
	w.eventually(t, webHost, func(status int, body string) bool { return status == http.StatusOK && body == "hello from the host\n" })

	// down stops the services and unroutes them, and keeps the host route.
	if err := commands.DownWithContext(ctx, w.context(t)); err != nil {
		t.Fatalf("down: %v", err)
	}
	w.eventually(t, host, func(status int, _ string) bool { return status == http.StatusNotFound })
	w.requireNoRouteFiles(t, registration.WorktreeID)
	if status, body := w.get(webHost); status != http.StatusOK || body != "hello from the host\n" {
		t.Fatalf("after down, the host route answered %d %q", status, body)
	}
	if files := w.hostRouteFiles(t, registration.WorktreeID); len(files) != 1 {
		t.Fatalf("down changed the host route files: %+v", files)
	}

	// With nothing listening the route answers 502, and doctor says so.
	server.Close()
	w.eventually(t, webHost, func(status int, _ string) bool { return status == http.StatusBadGateway })
	lines = checkoutHostRouteLines(t, w)
	if len(lines) != 1 || lines[0].Status != commands.StatusOK ||
		lines[0].Detail != fmt.Sprintf("nothing is listening on 127.0.0.1:%d; requests for it get a 502 until a process is", server.port) {
		t.Fatalf("doctor with nothing listening = %+v", lines)
	}

	if probe, found := routeProbeFor(t, w, webHost); !found || probe.Status != commands.StatusOK ||
		probe.Detail != fmt.Sprintf("%s: nothing is listening on 127.0.0.1:%d; requests for it get a 502 until a process is", webHost, server.port) {
		t.Fatalf("route probe with nothing listening = %+v (found %v)", probe, found)
	}

	// route rm unroutes it; a removed route is a 404 again.
	if err := commands.RouteRmWithContext(ctx, w.context(t), "web"); err != nil {
		t.Fatalf("route rm: %v", err)
	}
	w.eventually(t, webHost, func(status int, _ string) bool { return status == http.StatusNotFound })
	if files := w.hostRouteFiles(t, registration.WorktreeID); len(files) != 0 {
		t.Fatalf("route rm left %+v", files)
	}

	// worktree remove removes a route that is still there, and the record.
	add(commands.RouteAddOptions{Name: "web", Port: startHostServer(t, "tcp4", "127.0.0.1:0", "again\n").port, Subdomain: "app"})
	w.eventually(t, webHost, func(status int, body string) bool { return status == http.StatusOK && body == "again\n" })
	if _, err := commands.RemoveWithContext(ctx, w.context(t), w.env, commands.RemoveOptions{Yes: true}); err != nil {
		t.Fatalf("worktree remove: %v", err)
	}
	w.eventually(t, webHost, func(status int, _ string) bool { return status == http.StatusNotFound })
	if files := w.hostRouteFiles(t, registration.WorktreeID); len(files) != 0 {
		t.Fatalf("worktree remove left %+v", files)
	}
}

// The proxy in host mode maps treerig-loopback to ::1 as well, so a listener on
// ::1 alone answers through it (docs/design/routing.md, Open items); doctor
// still tells the person to bind 127.0.0.1, for Docker Desktop and OrbStack.
func TestHostRouteToAnIPv6OnlyListenerOnARealDaemon(t *testing.T) {
	w := newRoutesWalk(t)
	const webHost = "app.feature-route.routes.localhost"
	server := startHostServer(t, "tcp6", "[::1]:0", "hello from ::1\n")
	w.up(t)

	if err := commands.RouteAddWithContext(context.Background(), w.context(t), w.svc, commands.RouteAddOptions{Name: "web", Port: server.port, Subdomain: "app"}); err != nil {
		t.Fatal(err)
	}
	w.eventually(t, webHost, func(status int, body string) bool { return status == http.StatusOK && body == "hello from ::1\n" })

	lines := checkoutHostRouteLines(t, w)
	if len(lines) != 1 || lines[0].Status != commands.StatusOK ||
		!strings.Contains(lines[0].Detail, "the router in host mode reaches ::1; bind 127.0.0.1 to work on Docker Desktop and OrbStack too") {
		t.Fatalf("doctor's host route lines = %+v", lines)
	}
}
