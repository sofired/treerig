package router_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/router"
)

// hostState is a router layout over a scratch berth root, with nothing
// created: WriteHostFragment must be able to start from that.
func hostState(t *testing.T) layout.RouterLayout {
	t.Helper()
	return layout.RouterStateFrom(registrytest.TempDir(t))
}

// writeHost writes a host route's file as the record would produce it and
// returns its listing.
func writeHost(t *testing.T, state layout.RouterLayout, repo, wt, name, rule string, port int) router.FragmentInfo {
	t.Helper()
	if err := router.WriteHostFragment(state, repo, wt, name, router.HostFragmentDocument(repo, wt, name, rule, port)); err != nil {
		t.Fatal(err)
	}
	return router.FragmentInfo{RepositoryID: repo, WorktreeID: wt, Service: name, Path: router.HostFragmentPath(state, repo, wt, name), HostRoute: true}
}

func TestHostRouteName(t *testing.T) {
	name := router.HostRouteName(fragRepoA, fragWtA, "web")
	want := layout.ComposeProjectName(fragRepoA, fragWtA) + "_host_web"
	if name != want {
		t.Fatalf("HostRouteName = %q, want %q", name, want)
	}
	if strings.Contains(layout.ComposeProjectName(fragRepoA, fragWtA)+"-web", "_host_") {
		t.Fatal("a service's router name could be a host route's")
	}
}

func TestHostFragmentFiles(t *testing.T) {
	t.Run("names the file host.<repository>.<worktree>.<name>.yaml", func(t *testing.T) {
		state := hostState(t)
		path := router.HostFragmentPath(state, fragRepoA, fragWtA, "web")
		want := filepath.Join(state.DynamicDir, "host."+fragRepoA+"."+fragWtA+".web.yaml")
		if path != want {
			t.Fatalf("path = %q, want %q", path, want)
		}
	})

	t.Run("writes a document routing the rule to berth-loopback at the port, at 0644, creating the directory", func(t *testing.T) {
		state := hostState(t)
		fragment := writeHost(t, state, fragRepoA, fragWtA, "web", "Host(`feature.acme.localhost`)", 5173)
		info, err := os.Stat(fragment.Path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o644 {
			t.Errorf("mode = %v", info.Mode().Perm())
		}
		doc, err := router.ReadFragment(state, fragment.Path)
		if err != nil {
			t.Fatal(err)
		}
		name := router.HostRouteName(fragRepoA, fragWtA, "web")
		entry, ok := doc.HTTP.Routers[name]
		if !ok || entry.Rule != "Host(`feature.acme.localhost`)" || entry.Service != name || doc.TargetPort() != 5173 {
			t.Fatalf("document = %+v", doc)
		}
		if got := doc.HTTP.Services[name].LoadBalancer.Servers[0].URL; got != "http://berth-loopback:5173" {
			t.Errorf("target = %q", got)
		}
	})

	t.Run("lists host files and never service files, and the service sweeps never list host files", func(t *testing.T) {
		state := hostState(t)
		host := writeHost(t, state, fragRepoA, fragWtA, "web", "Host(`h`)", 5173)
		if err := router.WriteFragment(state, fragRepoA, fragWtA, "web", router.NewFragmentDocument("p-web", "Host(`h`)", 30010)); err != nil {
			t.Fatal(err)
		}
		hosts, err := router.ListHostFragments(state)
		if err != nil || len(hosts) != 1 || hosts[0] != host {
			t.Fatalf("ListHostFragments = %+v, %v", hosts, err)
		}
		services, err := router.ListFragments(state)
		if err != nil || len(services) != 1 || services[0].HostRoute || services[0].Service != "web" {
			t.Fatalf("ListFragments = %+v, %v", services, err)
		}
	})

	t.Run("a service sweep removes the service's file and keeps the host route's", func(t *testing.T) {
		state := hostState(t)
		host := writeHost(t, state, fragRepoA, fragWtA, "web", "Host(`h`)", 5173)
		if err := router.WriteFragment(state, fragRepoA, fragWtA, "web", router.NewFragmentDocument("p-web", "Host(`h`)", 30010)); err != nil {
			t.Fatal(err)
		}
		if err := router.RemoveWorktreeFragments(state, fragRepoA, fragWtA); err != nil {
			t.Fatal(err)
		}
		if err := router.RemoveWorktreeFragmentsExcept(state, fragRepoA, fragWtA, map[string]bool{}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(host.Path); err != nil {
			t.Fatalf("the host route's file went with the services': %v", err)
		}
		if _, err := os.Stat(router.FragmentPath(state, fragRepoA, fragWtA, "web")); !os.IsNotExist(err) {
			t.Fatalf("the service's file survived: %v", err)
		}
	})

	t.Run("skips files that only look like host routes", func(t *testing.T) {
		state := hostState(t)
		writeHost(t, state, fragRepoA, fragWtA, "web", "Host(`h`)", 5173)
		for _, name := range []string{
			"host.yaml",
			"host." + fragRepoA + ".yaml",
			"host.not-a-uuid." + fragWtA + ".web.yaml",
			"host." + fragRepoA + "." + fragWtA + ".yaml",
			"host." + fragRepoA + "." + fragWtA + ".Web.yaml",
			"host." + fragRepoA + "." + fragWtA + ".web.yaml.tmp.1.abc",
			"shared." + fragRepoA + ".web.yaml",
		} {
			if err := os.WriteFile(filepath.Join(state.DynamicDir, name), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		hosts, err := router.ListHostFragments(state)
		if err != nil || len(hosts) != 1 {
			t.Fatalf("ListHostFragments = %+v, %v", hosts, err)
		}
	})

	t.Run("lists nothing for a directory that does not exist", func(t *testing.T) {
		hosts, err := router.ListHostFragments(hostState(t))
		if err != nil || hosts != nil {
			t.Fatalf("ListHostFragments = %+v, %v", hosts, err)
		}
	})

	t.Run("a repository's service route named host does not collide with a host route", func(t *testing.T) {
		state := hostState(t)
		writeHost(t, state, fragRepoA, fragWtA, "web", "Host(`h`)", 5173)
		// A service called "host" under the same worktree.
		if err := router.WriteFragment(state, fragRepoA, fragWtA, "host", router.NewFragmentDocument("p-host", "Host(`h`)", 30010)); err != nil {
			t.Fatal(err)
		}
		services, _ := router.ListFragments(state)
		hosts, _ := router.ListHostFragments(state)
		if len(services) != 1 || services[0].Service != "host" || len(hosts) != 1 || hosts[0].Service != "web" {
			t.Fatalf("services = %+v, hosts = %+v", services, hosts)
		}
	})

	t.Run("removes one route's file or a worktree's, only that worktree's, and tolerates none", func(t *testing.T) {
		state := hostState(t)
		web := writeHost(t, state, fragRepoA, fragWtA, "web", "Host(`h`)", 5173)
		api := writeHost(t, state, fragRepoA, fragWtA, "api", "Host(`a.h`)", 8080)
		other := writeHost(t, state, fragRepoB, fragWtB, "web", "Host(`o`)", 5174)
		if err := router.RemoveHostFragment(state, fragRepoA, fragWtA, "web"); err != nil {
			t.Fatal(err)
		}
		if err := router.RemoveHostFragment(state, fragRepoA, fragWtA, "web"); err != nil {
			t.Fatalf("removing a missing file: %v", err)
		}
		exists := func(f router.FragmentInfo) bool { _, err := os.Stat(f.Path); return err == nil }
		if exists(web) || !exists(api) || !exists(other) {
			t.Fatalf("after RemoveHostFragment: web %v api %v other %v", exists(web), exists(api), exists(other))
		}
		if err := router.RemoveWorktreeHostFragments(state, fragRepoA, fragWtA); err != nil {
			t.Fatal(err)
		}
		if exists(api) || !exists(other) {
			t.Fatalf("after RemoveWorktreeHostFragments: api %v other %v", exists(api), exists(other))
		}
		if err := router.RemoveWorktreeHostFragments(hostState(t), fragRepoA, fragWtA); err != nil {
			t.Fatalf("no directory: %v", err)
		}
	})

	t.Run("refuses to write through a symbolic link planted as the dynamic directory", func(t *testing.T) {
		state := hostState(t)
		outside := registrytest.TempDir(t)
		if err := os.MkdirAll(state.StateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, state.DynamicDir); err != nil {
			t.Fatal(err)
		}
		err := router.WriteHostFragment(state, fragRepoA, fragWtA, "web", router.HostFragmentDocument(fragRepoA, fragWtA, "web", "Host(`h`)", 1))
		if err == nil {
			t.Fatal("wrote through a symbolic link")
		}
		if entries, _ := os.ReadDir(outside); len(entries) != 0 {
			t.Fatalf("wrote into %s: %v", outside, entries)
		}
	})
}

func TestClassifyAndRepairHostRoutes(t *testing.T) {
	const rule = "Host(`feature.acme.localhost`)"
	key := fragRepoA + "/" + fragWtA
	registered := func(routes map[string]router.HostTarget) mapRegistrations {
		return mapRegistrations{key: {Status: router.Registered, HostRoutes: routes}}
	}

	t.Run("finds nothing wrong with a file the record produces", func(t *testing.T) {
		state := hostState(t)
		fragment := writeHost(t, state, fragRepoA, fragWtA, "web", rule, 5173)
		problem, err := router.ClassifyFragment(state, registered(map[string]router.HostTarget{"web": {Rule: rule, Port: 5173}}), fragment)
		if err != nil || problem != nil {
			t.Fatalf("ClassifyFragment = %+v, %v", problem, err)
		}
	})

	t.Run("calls a file whose record holds no such host route no-host-route, even when a service port has its name", func(t *testing.T) {
		state := hostState(t)
		fragment := writeHost(t, state, fragRepoA, fragWtA, "web", rule, 5173)
		regs := mapRegistrations{key: {Status: router.Registered, Ports: map[string]int{"web": 5173}}}
		problem, err := router.ClassifyFragment(state, regs, fragment)
		if err != nil || problem == nil || problem.Kind != router.RouteNoHostRoute || !problem.Repairable() {
			t.Fatalf("ClassifyFragment = %+v, %v", problem, err)
		}
		if got := problem.Describe(); got != "web (host route): its registration records no such host route" {
			t.Errorf("Describe = %q", got)
		}
	})

	t.Run("calls a file on another port stale", func(t *testing.T) {
		state := hostState(t)
		fragment := writeHost(t, state, fragRepoA, fragWtA, "web", rule, 5173)
		problem, err := router.ClassifyFragment(state, registered(map[string]router.HostTarget{"web": {Rule: rule, Port: 5174}}), fragment)
		if err != nil || problem == nil || problem.Kind != router.RouteStaleHostRoute || problem.Routed != 5173 || problem.Recorded != 5174 {
			t.Fatalf("ClassifyFragment = %+v, %v", problem, err)
		}
	})

	t.Run("calls a file claiming another rule stale: the worktree's hostname changed", func(t *testing.T) {
		state := hostState(t)
		fragment := writeHost(t, state, fragRepoA, fragWtA, "web", rule, 5173)
		now := "Host(`renamed.acme.localhost`)"
		problem, err := router.ClassifyFragment(state, registered(map[string]router.HostTarget{"web": {Rule: now, Port: 5173}}), fragment)
		if err != nil || problem == nil || problem.Kind != router.RouteStaleHostRoute || problem.RoutedRule != rule || problem.RecordedRule != now {
			t.Fatalf("ClassifyFragment = %+v, %v", problem, err)
		}
		want := "web (host route): routes Host(`feature.acme.localhost`) to port 5173, but its registration records Host(`renamed.acme.localhost`) to port 5173"
		if got := problem.Describe(); got != want {
			t.Errorf("Describe = %q", got)
		}
	})

	t.Run("judges registration before anything else: unregistered and unknown are reported and not repairable", func(t *testing.T) {
		state := hostState(t)
		fragment := writeHost(t, state, fragRepoA, fragWtA, "web", rule, 5173)
		for status, kind := range map[router.RegistrationStatus]router.RouteProblemKind{
			router.Unregistered: router.RouteUnregistered, router.UnknownRepository: router.RouteUnknownRepository,
		} {
			problem, err := router.ClassifyFragment(state, mapRegistrations{key: {Status: status}}, fragment)
			if err != nil || problem == nil || problem.Kind != kind || problem.Repairable() {
				t.Fatalf("status %d: ClassifyFragment = %+v, %v", status, problem, err)
			}
		}
	})

	t.Run("does not judge a file it cannot read", func(t *testing.T) {
		state := hostState(t)
		fragment := writeHost(t, state, fragRepoA, fragWtA, "web", rule, 5173)
		if err := os.WriteFile(fragment.Path, []byte("not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		problem, err := router.ClassifyFragment(state, registered(map[string]router.HostTarget{"web": {Rule: rule, Port: 5174}}), fragment)
		if err != nil || problem != nil {
			t.Fatalf("ClassifyFragment = %+v, %v", problem, err)
		}
	})

	t.Run("repair removes the file of a dropped route, rewrites a stale one whole, and leaves the rest", func(t *testing.T) {
		state := hostState(t)
		writeHost(t, state, fragRepoA, fragWtA, "ok", rule, 5173)
		writeHost(t, state, fragRepoA, fragWtA, "dropped", rule, 5174)
		stale := writeHost(t, state, fragRepoA, fragWtA, "stale", rule, 5175)
		orphan := writeHost(t, state, fragRepoB, fragWtB, "web", rule, 5176)
		now := "Host(`renamed.acme.localhost`)"
		regs := registered(map[string]router.HostTarget{
			"ok": {Rule: rule, Port: 5173}, "stale": {Rule: now, Port: 5199},
		})
		repaired, err := router.RepairRoutes(context.Background(), state, regs)
		if err != nil || len(repaired) != 2 {
			t.Fatalf("RepairRoutes = %+v, %v", repaired, err)
		}
		if _, err := os.Stat(router.HostFragmentPath(state, fragRepoA, fragWtA, "dropped")); !os.IsNotExist(err) {
			t.Errorf("the dropped route's file survived: %v", err)
		}
		doc, err := router.ReadFragment(state, stale.Path)
		if err != nil {
			t.Fatal(err)
		}
		want := router.HostFragmentDocument(fragRepoA, fragWtA, "stale", now, 5199)
		if firstRuleOf(doc) != now || doc.TargetPort() != 5199 || len(doc.HTTP.Routers) != len(want.HTTP.Routers) {
			t.Errorf("the stale file = %+v", doc)
		}
		if _, err := os.Stat(orphan.Path); err != nil {
			t.Errorf("an unmatched file was touched: %v", err)
		}
		if again, _ := router.RepairRoutes(context.Background(), state, regs); len(again) != 0 {
			t.Errorf("a second repair still found %+v", again)
		}
	})

	t.Run("repair never creates the file of a recorded route, whatever else it repairs", func(t *testing.T) {
		state := hostState(t)
		writeHost(t, state, fragRepoA, fragWtA, "ok", rule, 5173)
		writeHost(t, state, fragRepoA, fragWtA, "stale", rule, 5175)
		regs := registered(map[string]router.HostTarget{
			"ok": {Rule: rule, Port: 5173}, "stale": {Rule: rule, Port: 5199}, "web": {Rule: rule, Port: 5174},
		})
		repaired, err := router.RepairRoutes(context.Background(), state, regs)
		if err != nil || len(repaired) != 1 {
			t.Fatalf("RepairRoutes = %+v, %v", repaired, err)
		}
		if _, err := os.Stat(router.HostFragmentPath(state, fragRepoA, fragWtA, "web")); !os.IsNotExist(err) {
			t.Fatalf("repair created a file: %v", err)
		}
	})

	t.Run("service and host files of one worktree are judged each against its own part of the record", func(t *testing.T) {
		state := hostState(t)
		if err := router.WriteFragment(state, fragRepoA, fragWtA, "web", router.NewFragmentDocument("p-web", "Host(`h`)", 30010)); err != nil {
			t.Fatal(err)
		}
		writeHost(t, state, fragRepoA, fragWtA, "web", rule, 5173)
		regs := mapRegistrations{key: {
			Status: router.Registered, Ports: map[string]int{"web": 30010},
			HostRoutes: map[string]router.HostTarget{"web": {Rule: rule, Port: 5173}},
		}}
		problems, err := router.FindRouteProblems(state, regs)
		if err != nil || len(problems) != 0 {
			t.Fatalf("FindRouteProblems = %+v, %v", problems, err)
		}
	})
}

// firstRuleOf is the rule of a document's only router.
func firstRuleOf(doc router.FragmentDocument) string {
	for _, entry := range doc.HTTP.Routers {
		return entry.Rule
	}
	return ""
}
