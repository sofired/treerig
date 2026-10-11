package router_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/router"
)

// registeredWith is a registration holding port for service.
func registeredWith(service string, port int) router.Registration {
	return router.Registration{Status: router.Registered, Ports: map[string]int{service: port}}
}

// mapRegistrations is a router.Registrations answering from a fixed table
// keyed "<repository>/<worktree>"; an absent key is Unregistered.
type mapRegistrations map[string]router.Registration

// Lookup is router.Registrations' Lookup.
func (m mapRegistrations) Lookup(repositoryID, worktreeID string) (router.Registration, error) {
	if registration, ok := m[repositoryID+"/"+worktreeID]; ok {
		return registration, nil
	}
	return router.Registration{Status: router.Unregistered}, nil
}

func TestClassifyFragment(t *testing.T) {
	state := fragmentState(t)
	write := func(t *testing.T, service string, port int) router.FragmentInfo {
		t.Helper()
		if err := router.WriteFragment(state, fragRepoA, fragWtA, service, router.NewFragmentDocument("p-"+service, "Host(`h`)", port)); err != nil {
			t.Fatal(err)
		}
		return router.FragmentInfo{RepositoryID: fragRepoA, WorktreeID: fragWtA, Service: service, Path: router.FragmentPath(state, fragRepoA, fragWtA, service)}
	}
	key := fragRepoA + "/" + fragWtA

	t.Run("finds nothing wrong with a file that matches its registration", func(t *testing.T) {
		fragment := write(t, "api", 30010)
		problem, err := router.ClassifyFragment(state, mapRegistrations{key: registeredWith("api", 30010)}, fragment)
		if err != nil || problem != nil {
			t.Fatalf("ClassifyFragment = %+v, %v", problem, err)
		}
	})

	t.Run("calls a file with no registration unregistered", func(t *testing.T) {
		fragment := write(t, "api", 30010)
		problem, err := router.ClassifyFragment(state, mapRegistrations{}, fragment)
		if err != nil || problem == nil || problem.Kind != router.RouteUnregistered {
			t.Fatalf("ClassifyFragment = %+v, %v", problem, err)
		}
		if got := problem.Describe(); got != "api: no registration of this control home holds its worktree" {
			t.Errorf("Describe = %q", got)
		}
	})

	t.Run("calls a file whose registration holds no port for the service no-port", func(t *testing.T) {
		fragment := write(t, "api", 30010)
		problem, err := router.ClassifyFragment(state, mapRegistrations{key: registeredWith("web", 30011)}, fragment)
		if err != nil || problem == nil || problem.Kind != router.RouteNoPort {
			t.Fatalf("ClassifyFragment = %+v, %v", problem, err)
		}
		if got := problem.Describe(); got != "api: its registration records no port for the service" {
			t.Errorf("Describe = %q", got)
		}
	})

	t.Run("calls a file pointing at another port than the recorded one stale, with both ports", func(t *testing.T) {
		fragment := write(t, "api", 30010)
		problem, err := router.ClassifyFragment(state, mapRegistrations{key: registeredWith("api", 30020)}, fragment)
		if err != nil || problem == nil || problem.Kind != router.RouteStalePort || problem.Routed != 30010 || problem.Recorded != 30020 {
			t.Fatalf("ClassifyFragment = %+v, %v", problem, err)
		}
		if got := problem.Describe(); got != "api: points at port 30010, but its registration records 30020" {
			t.Errorf("Describe = %q", got)
		}
	})

	t.Run("calls a file of a repository this control home does not know unknown-repository, and not repairable", func(t *testing.T) {
		fragment := write(t, "api", 30010)
		regs := mapRegistrations{key: {Status: router.UnknownRepository}}
		problem, err := router.ClassifyFragment(state, regs, fragment)
		if err != nil || problem == nil || problem.Kind != router.RouteUnknownRepository || problem.Repairable() {
			t.Fatalf("ClassifyFragment = %+v, %v", problem, err)
		}
		if got := problem.Describe(); got != "api: this control home does not know its repository" {
			t.Errorf("Describe = %q", got)
		}
	})

	t.Run("calls a file of a known repository with no record for its worktree unregistered, and not repairable", func(t *testing.T) {
		fragment := write(t, "api", 30010)
		regs := mapRegistrations{key: {Status: router.Unregistered}}
		problem, err := router.ClassifyFragment(state, regs, fragment)
		if err != nil || problem == nil || problem.Kind != router.RouteUnregistered || problem.Repairable() {
			t.Fatalf("ClassifyFragment = %+v, %v", problem, err)
		}
	})

	t.Run("repairs only the problems that name a record of this control home", func(t *testing.T) {
		for kind, want := range map[router.RouteProblemKind]bool{router.RouteUnregistered: false, router.RouteUnknownRepository: false, router.RouteNoPort: true, router.RouteStalePort: true} {
			if got := (router.RouteProblem{Kind: kind}).Repairable(); got != want {
				t.Errorf("%s: Repairable = %v, want %v", kind, got, want)
			}
		}
	})

	t.Run("does not judge a file it cannot read or that names no loopback target", func(t *testing.T) {
		fragment := write(t, "api", 30010)
		if err := os.WriteFile(fragment.Path, []byte("not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		regs := mapRegistrations{key: registeredWith("api", 30020)}
		if problem, err := router.ClassifyFragment(state, regs, fragment); err != nil || problem != nil {
			t.Fatalf("unreadable file: %+v, %v", problem, err)
		}
		if err := os.WriteFile(fragment.Path, []byte(`{"http":{"services":{"n":{"loadBalancer":{"servers":[{"url":"http://elsewhere:1"}]}}}}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if problem, err := router.ClassifyFragment(state, regs, fragment); err != nil || problem != nil {
			t.Fatalf("foreign target: %+v, %v", problem, err)
		}
	})

	t.Run("reports a record that cannot be read as an error, never as unregistered", func(t *testing.T) {
		fragment := write(t, "api", 30010)
		regs := &fakeRegistrations{err: os.ErrPermission}
		if _, err := router.ClassifyFragment(state, regs, fragment); err == nil {
			t.Fatal("expected the lookup error")
		}
	})
}

func TestFindAndRepairRoutes(t *testing.T) {
	setup := func(t *testing.T) (state layout.RouterLayout, regs mapRegistrations) {
		t.Helper()
		layoutState := fragmentState(t)
		doc := func(port int) router.FragmentDocument { return router.NewFragmentDocument("p", "Host(`h`)", port) }
		for _, w := range []struct {
			repo, wt, service string
			port              int
		}{
			{fragRepoA, fragWtA, "ok", 30001},
			{fragRepoA, fragWtA, "stale", 30002},
			{fragRepoA, fragWtA, "dropped", 30003},
			{fragRepoB, fragWtB, "orphan", 30004},
		} {
			if err := router.WriteFragment(layoutState, w.repo, w.wt, w.service, doc(w.port)); err != nil {
				t.Fatal(err)
			}
		}
		return layoutState, mapRegistrations{
			fragRepoA + "/" + fragWtA: {Status: router.Registered, Ports: map[string]int{"ok": 30001, "stale": 30099}},
		}
	}

	t.Run("finds every file that disagrees and only those, without changing anything", func(t *testing.T) {
		state, regs := setup(t)
		problems, err := router.FindRouteProblems(state, regs)
		if err != nil {
			t.Fatal(err)
		}
		kinds := map[string]router.RouteProblemKind{}
		for _, problem := range problems {
			kinds[problem.Fragment.Service] = problem.Kind
		}
		want := map[string]router.RouteProblemKind{"stale": router.RouteStalePort, "dropped": router.RouteNoPort, "orphan": router.RouteUnregistered}
		if len(kinds) != len(want) {
			t.Fatalf("problems = %v, want %v", kinds, want)
		}
		for service, kind := range want {
			if kinds[service] != kind {
				t.Errorf("%s: %q, want %q", service, kinds[service], kind)
			}
		}
		if again, _ := router.FindRouteProblems(state, regs); len(again) != 3 {
			t.Errorf("finding changed something: %d problems left", len(again))
		}
	})

	t.Run("removes the portless file, retargets the stale one, and leaves the good and the unmatched ones", func(t *testing.T) {
		state, regs := setup(t)
		repaired, err := router.RepairRoutes(context.Background(), state, regs)
		if err != nil || len(repaired) != 2 {
			t.Fatalf("RepairRoutes = %+v, %v", repaired, err)
		}
		fragments, err := router.ListFragments(state)
		if err != nil {
			t.Fatal(err)
		}
		ports := map[string]int{}
		for _, fragment := range fragments {
			doc, err := router.ReadFragment(state, fragment.Path)
			if err != nil {
				t.Fatal(err)
			}
			ports[fragment.Service] = doc.TargetPort()
		}
		if len(ports) != 3 || ports["ok"] != 30001 || ports["stale"] != 30099 || ports["orphan"] != 30004 {
			t.Fatalf("files left = %v, want ok on 30001, stale on 30099 and the untouched orphan on 30004", ports)
		}
		if again, _ := router.RepairRoutes(context.Background(), state, regs); len(again) != 0 {
			t.Errorf("a second repair still found %+v", again)
		}
		if listed, _ := router.FindRouteProblems(state, regs); len(listed) != 1 || listed[0].Kind != router.RouteUnregistered {
			t.Errorf("the unmatched file should still be listed, got %+v", listed)
		}
	})

	t.Run("keeps the rule when it retargets a file", func(t *testing.T) {
		state, regs := setup(t)
		if _, err := router.RepairRoutes(context.Background(), state, regs); err != nil {
			t.Fatal(err)
		}
		doc, err := router.ReadFragment(state, router.FragmentPath(state, fragRepoA, fragWtA, "stale"))
		if err != nil {
			t.Fatal(err)
		}
		if rule := doc.HTTP.Routers["p"].Rule; rule != "Host(`h`)" {
			t.Errorf("rule = %q", rule)
		}
	})

	t.Run("does not act when the second read no longer shows the problem", func(t *testing.T) {
		state, _ := setup(t)
		regs := newFakeRegistrations(registeredWith("other", 30004), registeredWith("orphan", 30004))
		fragment := router.FragmentPath(state, fragRepoB, fragWtB, "orphan")
		// Only the orphan's directory holds a file the flipping fake judges.
		if err := router.RemoveWorktreeFragments(state, fragRepoA, fragWtA); err != nil {
			t.Fatal(err)
		}
		repaired, err := router.RepairRoutes(context.Background(), state, regs)
		if err != nil || len(repaired) != 0 {
			t.Fatalf("RepairRoutes = %+v, %v", repaired, err)
		}
		if _, err := os.Stat(fragment); err != nil {
			t.Fatalf("the file was removed on a stale read: %v", err)
		}
	})

	t.Run("stops at a cancelled context before touching a file", func(t *testing.T) {
		state, regs := setup(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		repaired, err := router.RepairRoutes(ctx, state, regs)
		if !errors.Is(err, context.Canceled) || len(repaired) != 0 {
			t.Fatalf("RepairRoutes = %+v, %v", repaired, err)
		}
		if problems, _ := router.FindRouteProblems(state, regs); len(problems) != 3 {
			t.Errorf("a cancelled repair changed files: %d problems left", len(problems))
		}
	})

	t.Run("never removes or rewrites a file of an unknown repository", func(t *testing.T) {
		state, _ := setup(t)
		regs := mapRegistrations{fragRepoB + "/" + fragWtB: {Status: router.UnknownRepository}}
		if _, err := router.RepairRoutes(context.Background(), state, regs); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(router.FragmentPath(state, fragRepoB, fragWtB, "orphan")); err != nil {
			t.Fatalf("the unknown repository's file is gone: %v", err)
		}
	})

	t.Run("stops at a record it cannot read, reporting what it had repaired", func(t *testing.T) {
		state, _ := setup(t)
		repaired, err := router.RepairRoutes(context.Background(), state, &fakeRegistrations{err: os.ErrPermission})
		if err == nil || len(repaired) != 0 {
			t.Fatalf("RepairRoutes = %+v, %v", repaired, err)
		}
	})
}
