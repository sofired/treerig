package checkoutindex

import (
	"strings"
	"testing"

	"github.com/sofired/berth/internal/checkoutindex/card"
	"github.com/sofired/berth/internal/registry"
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
	build := func(routerPort int, withCard bool, containers []ContainerRecord, others ...registry.RegisteredWorktree) IndexEntry {
		input := SnapshotInput{
			Registrations: append([]registry.RegisteredWorktree{record}, others...), RouterPort: routerPort, Containers: containers,
			Identities: []registry.WorktreeIdentity{testIdentity(record, "feature/a")}, CurrentRepositoryID: &testRepoID,
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
		entry := build(80, false, []ContainerRecord{})
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
		entry := build(8080, false, []ContainerRecord{})
		if entry.Services[0].URL != "http://app.feature-a.acme.localhost:8080" {
			t.Fatalf("URL = %q", entry.Services[0].URL)
		}
	})

	t.Run("a host route does not change the row's status: only containers do", func(t *testing.T) {
		with := build(80, true, []ContainerRecord{})
		record.HostRoutes = nil
		without := build(80, true, []ContainerRecord{})
		if with.Status != without.Status {
			t.Fatalf("status with host routes %s, without %s", with.Status, without.Status)
		}
	})

	t.Run("a checkout Git no longer lists has no available link", func(t *testing.T) {
		record.HostRoutes = []registry.HostRoute{{Name: "web", Port: 5173, Subdomain: "app"}}
		input := SnapshotInput{
			Registrations: []registry.RegisteredWorktree{record}, RouterPort: 80, Containers: []ContainerRecord{},
			CurrentRepositoryID: &testRepoID,
		}
		entry := BuildSnapshot(input).Entries[0]
		if entry.Status != StatusMissing || len(entry.Services) != 1 || entry.Services[0].Available {
			t.Fatalf("entry = %+v", entry)
		}
	})

	t.Run("renders in the page", func(t *testing.T) {
		record.HostRoutes = []registry.HostRoute{{Name: "web", Port: 5173, Subdomain: "app"}}
		snapshot := BuildSnapshot(SnapshotInput{
			Registrations: []registry.RegisteredWorktree{record}, RouterPort: 80, Containers: []ContainerRecord{},
			Identities: []registry.WorktreeIdentity{testIdentity(record, "feature/a")}, CurrentRepositoryID: &testRepoID,
		})
		page := RenderIndexHTML(snapshot)
		if !strings.Contains(page, "http://app.feature-a.acme.localhost") || !strings.Contains(page, "host process on port 5173") {
			t.Fatalf("the page lacks the host route")
		}
	})
}
