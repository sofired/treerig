package checkoutindex

import (
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/checkoutindex/card"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/worktree"
	"github.com/sofired/treerig/internal/worktreestate"
)

// testRepoID is testRepo as a variable, for the *string a snapshot input takes.
var testRepoID = testRepo

func TestHostRoutesOnTheIndex(t *testing.T) {
	path := "/v1"
	record := testRegistrations()[0]
	record.HostRoutes = []registry.HostRoute{
		{Name: "web", Port: 5173, Subdomain: "app"},
		{Name: "api-v1", Port: 8080, Path: &path},
	}
	build := func(routerPort int, withCard bool, containers []worktreestate.Container, others ...registry.RegisteredWorktree) IndexEntry {
		input := SnapshotInput{
			Registrations: append([]registry.RegisteredWorktree{record}, others...), RouterPort: routerPort, Containers: containers,
			Identities: []registry.WorktreeIdentity{testIdentity(record, "feature/a")}, CurrentRepositoryID: &testRepoID,
			States: map[string]worktreestate.Result{EntryKey(testRepo, record.WorktreeID): {State: worktree.StateStopped}},
		}
		if withCard {
			input.Cards = map[string]card.IndexCard{EntryKey(testRepo, record.WorktreeID): testCard(record, "acme")}
		}
		snapshot := BuildSnapshot(input)
		for _, entry := range snapshot.Entries {
			if entry.WorktreeID == record.WorktreeID {
				return entry
			}
		}
		t.Fatal("no entry for the record")
		return IndexEntry{}
	}

	t.Run("lists each host route as a link built from the record, after the services", func(t *testing.T) {
		entry := build(80, false, []worktreestate.Container{})
		if len(entry.Services) != 2 {
			t.Fatalf("services = %+v", entry.Services)
		}
		web, api := entry.Services[0], entry.Services[1]
		if web.Name != "web" || web.Label != "web" || web.Purpose != "host process on port 5173" || web.URL != "http://app.feature-a.acme.localhost" || !web.Available {
			t.Errorf("web = %+v", web)
		}
		if api.Name != "api-v1" || api.URL != "http://feature-a.acme.localhost/v1" || api.Purpose != "host process on port 8080" {
			t.Errorf("api-v1 = %+v", api)
		}
	})

	t.Run("folds in the router's port", func(t *testing.T) {
		entry := build(8080, false, []worktreestate.Container{})
		if entry.Services[0].URL != "http://app.feature-a.acme.localhost:8080" {
			t.Fatalf("URL = %q", entry.Services[0].URL)
		}
	})

	t.Run("a host route does not change the row's state: only containers do", func(t *testing.T) {
		with := build(80, true, []worktreestate.Container{})
		record.HostRoutes = nil
		without := build(80, true, []worktreestate.Container{})
		if with.State != without.State {
			t.Fatalf("state with host routes %s, without %s", with.State, without.State)
		}
	})

	t.Run("a worktree whose directory is missing has no available link", func(t *testing.T) {
		record.HostRoutes = []registry.HostRoute{{Name: "web", Port: 5173, Subdomain: "app"}}
		input := SnapshotInput{
			Registrations: []registry.RegisteredWorktree{record}, RouterPort: 80, Containers: []worktreestate.Container{},
			CurrentRepositoryID: &testRepoID,
			States:              map[string]worktreestate.Result{EntryKey(testRepo, record.WorktreeID): {State: worktree.StateDirectoryMissing}},
		}
		entry := BuildSnapshot(input).Entries[0]
		if entry.State != worktree.StateDirectoryMissing || len(entry.Services) != 1 || entry.Services[0].Available {
			t.Fatalf("entry = %+v", entry)
		}
	})

	t.Run("renders in the page", func(t *testing.T) {
		record.HostRoutes = []registry.HostRoute{{Name: "web", Port: 5173, Subdomain: "app"}}
		snapshot := BuildSnapshot(SnapshotInput{
			Registrations: []registry.RegisteredWorktree{record}, RouterPort: 80, Containers: []worktreestate.Container{},
			Identities: []registry.WorktreeIdentity{testIdentity(record, "feature/a")}, CurrentRepositoryID: &testRepoID,
		})
		page := RenderIndexHTML(snapshot)
		if !strings.Contains(page, "http://app.feature-a.acme.localhost") || !strings.Contains(page, "host process on port 5173") {
			t.Fatalf("the page lacks the host route")
		}
	})
}
