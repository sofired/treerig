package router_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/router"
)

// mapSharedRegistrations is a router.SharedRegistrations answering from a
// fixed table keyed "<repository>/<name>"; an absent key is Unregistered.
type mapSharedRegistrations map[string]router.SharedRegistration

// LookupShared is router.SharedRegistrations' LookupShared.
func (m mapSharedRegistrations) LookupShared(repositoryID, name string) (router.SharedRegistration, error) {
	if registration, ok := m[repositoryID+"/"+name]; ok {
		return registration, nil
	}
	return router.SharedRegistration{Status: router.Unregistered}, nil
}

// routedAt is a shared record routing its web UI on host port.
func routedAt(port int) router.SharedRegistration {
	return router.SharedRegistration{Status: router.Registered, Domain: "acme.localhost", HostPort: port}
}

func TestSharedRouteFiles(t *testing.T) {
	state := fragmentState(t)
	key := fragRepoA + "/mailpit"
	write := func(t *testing.T, port int) router.SharedFragmentInfo {
		t.Helper()
		if err := router.WriteSharedFragment(state, fragRepoA, "mailpit", "acme.localhost", port); err != nil {
			t.Fatal(err)
		}
		return router.SharedFragmentInfo{RepositoryID: fragRepoA, Name: "mailpit", Path: router.SharedFragmentPath(state, fragRepoA, "mailpit")}
	}

	t.Run("names the file shared.<repository>.<name>.yaml and the router <shared project>_shared_<name>", func(t *testing.T) {
		write(t, 30010)
		path := router.SharedFragmentPath(state, fragRepoA, "mailpit")
		if filepath.Base(path) != "shared."+fragRepoA+".mailpit.yaml" {
			t.Errorf("path = %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		name := layout.SharedProjectName(fragRepoA) + "_shared_mailpit"
		if router.SharedRouteName(fragRepoA, "mailpit") != name {
			t.Errorf("SharedRouteName = %s", router.SharedRouteName(fragRepoA, "mailpit"))
		}
		for _, want := range []string{name, "Host(`mailpit.shared.acme.localhost`)", "http://treerig-loopback:30010"} {
			if !strings.Contains(string(data), want) {
				t.Errorf("file lacks %q:\n%s", want, data)
			}
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o644 {
			t.Errorf("mode = %v, %v", info, err)
		}
	})

	t.Run("is invisible to the service and worktree sweeps", func(t *testing.T) {
		write(t, 30010)
		listed, err := router.ListFragments(state)
		if err != nil {
			t.Fatal(err)
		}
		for _, fragment := range listed {
			if strings.HasPrefix(filepath.Base(fragment.Path), "shared.") {
				t.Errorf("ListFragments lists %s", fragment.Path)
			}
		}
		// Even a worktree whose ID is the shared file's own repository part.
		if err := router.RemoveWorktreeFragments(state, fragRepoA, fragWtA); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(router.SharedFragmentPath(state, fragRepoA, "mailpit")); err != nil {
			t.Errorf("a worktree sweep removed it: %v", err)
		}
	})

	t.Run("lists only files that parse as shared.<uuid>.<name>.yaml", func(t *testing.T) {
		state := fragmentState(t)
		if err := router.WriteSharedFragment(state, fragRepoA, "mailpit", "acme.localhost", 30010); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{
			"shared.not-a-uuid.mailpit.yaml", "shared." + fragRepoA + ".Mail_Pit.yaml", "shared." + fragRepoA + ".a.b.yaml",
			"shared." + fragRepoA + ".mailpit.txt", "shared." + fragRepoA + "..yaml", "shared.yaml",
		} {
			if err := os.WriteFile(filepath.Join(state.DynamicDir, name), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		listed, err := router.ListSharedFragments(state)
		if err != nil || len(listed) != 1 || listed[0].Name != "mailpit" || listed[0].RepositoryID != fragRepoA {
			t.Fatalf("ListSharedFragments = %+v, %v", listed, err)
		}
	})

	t.Run("lists nothing when there is no dynamic directory", func(t *testing.T) {
		listed, err := router.ListSharedFragments(layout.RouterLayout{DynamicDir: filepath.Join(t.TempDir(), "missing")})
		if err != nil || len(listed) != 0 {
			t.Fatalf("ListSharedFragments = %+v, %v", listed, err)
		}
	})

	t.Run("remove tolerates there being nothing to remove", func(t *testing.T) {
		if err := router.RemoveSharedFragment(layout.RouterLayout{DynamicDir: filepath.Join(t.TempDir(), "missing")}, fragRepoA, "mailpit"); err != nil {
			t.Fatal(err)
		}
		write(t, 30010)
		if err := router.RemoveSharedFragment(state, fragRepoA, "mailpit"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(router.SharedFragmentPath(state, fragRepoA, "mailpit")); !os.IsNotExist(err) {
			t.Errorf("file still there: %v", err)
		}
	})

	t.Run("finds nothing wrong with a file that matches its record", func(t *testing.T) {
		fragment := write(t, 30010)
		problem, err := router.ClassifySharedFragment(state, mapSharedRegistrations{key: routedAt(30010)}, fragment)
		if err != nil || problem != nil {
			t.Fatalf("ClassifySharedFragment = %+v, %v", problem, err)
		}
	})

	t.Run("calls a file whose record routes nothing no-port, and one pointing elsewhere stale", func(t *testing.T) {
		fragment := write(t, 30010)
		problem, err := router.ClassifySharedFragment(state, mapSharedRegistrations{key: {Status: router.Registered, Domain: "acme.localhost"}}, fragment)
		if err != nil || problem == nil || problem.Kind != router.RouteNoPort || !problem.Repairable() {
			t.Fatalf("no-port = %+v, %v", problem, err)
		}
		if got := problem.Describe(); got != "shared service mailpit: its record routes no port" {
			t.Errorf("Describe = %q", got)
		}
		problem, err = router.ClassifySharedFragment(state, mapSharedRegistrations{key: routedAt(30020)}, fragment)
		if err != nil || problem == nil || problem.Kind != router.RouteStalePort || problem.Routed != 30010 || problem.Recorded != 30020 {
			t.Fatalf("stale = %+v, %v", problem, err)
		}
		if got := problem.Describe(); got != "shared service mailpit: points at port 30010, but its record says 30020" {
			t.Errorf("Describe = %q", got)
		}
	})

	t.Run("reports a file with no record, and never repairs it", func(t *testing.T) {
		fragment := write(t, 30010)
		unregistered, err := router.ClassifySharedFragment(state, mapSharedRegistrations{}, fragment)
		if err != nil || unregistered == nil || unregistered.Kind != router.RouteUnregistered || unregistered.Repairable() {
			t.Fatalf("unregistered = %+v, %v", unregistered, err)
		}
		unknown, err := router.ClassifySharedFragment(state, mapSharedRegistrations{key: {Status: router.UnknownRepository}}, fragment)
		if err != nil || unknown == nil || unknown.Kind != router.RouteUnknownRepository || unknown.Repairable() {
			t.Fatalf("unknown = %+v, %v", unknown, err)
		}
		repaired, err := router.RepairSharedRoutes(context.Background(), state, mapSharedRegistrations{})
		if err != nil || len(repaired) != 0 {
			t.Fatalf("RepairSharedRoutes = %+v, %v", repaired, err)
		}
		if _, err := os.Stat(fragment.Path); err != nil {
			t.Errorf("an unmatched file was removed: %v", err)
		}
	})

	t.Run("repair rewrites a stale file to the recorded port and removes one the record does not route", func(t *testing.T) {
		fragment := write(t, 30010)
		repaired, err := router.RepairSharedRoutes(context.Background(), state, mapSharedRegistrations{key: routedAt(30020)})
		if err != nil || len(repaired) != 1 {
			t.Fatalf("RepairSharedRoutes = %+v, %v", repaired, err)
		}
		data, _ := os.ReadFile(fragment.Path)
		if !strings.Contains(string(data), "http://treerig-loopback:30020") || strings.Contains(string(data), "30010") ||
			!strings.Contains(string(data), "Host(`mailpit.shared.acme.localhost`)") {
			t.Errorf("rewritten file:\n%s", data)
		}
		repaired, err = router.RepairSharedRoutes(context.Background(), state, mapSharedRegistrations{key: {Status: router.Registered, Domain: "acme.localhost"}})
		if err != nil || len(repaired) != 1 {
			t.Fatalf("RepairSharedRoutes = %+v, %v", repaired, err)
		}
		if _, err := os.Stat(fragment.Path); !os.IsNotExist(err) {
			t.Errorf("file still there: %v", err)
		}
	})

	t.Run("repair stops for a cancelled context", func(t *testing.T) {
		write(t, 30010)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := router.RepairSharedRoutes(ctx, state, mapSharedRegistrations{key: routedAt(30020)}); err == nil {
			t.Fatal("a cancelled repair succeeded")
		}
	})
}

// TestSharedNameAlphabetAgreesWithTheRegistry pins that the route file name
// parser accepts exactly the names the registry accepts for a record file, so
// a record can always be found for the route file it wrote and vice versa.
func TestSharedNameAlphabetAgreesWithTheRegistry(t *testing.T) {
	state := fragmentState(t)
	for _, name := range []string{
		"mailpit", "a", "0", "a-b", "-a", "a_b", "A", "a.b", "", strings.Repeat("a", 63), strings.Repeat("a", 64), "über",
	} {
		path := filepath.Join(state.DynamicDir, "shared."+fragRepoA+"."+name+".yaml")
		if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
			continue // not a name a file can have
		}
		listed, err := router.ListSharedFragments(state)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, fragment := range listed {
			found = found || fragment.Path == path
		}
		if found != registry.ValidSharedName(name) {
			t.Errorf("%q: listed = %v, registry.ValidSharedName = %v", name, found, registry.ValidSharedName(name))
		}
		_ = os.Remove(path)
	}
}
