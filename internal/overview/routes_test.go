package overview

import (
	"slices"
	"testing"

	"github.com/sofired/treerig/internal/overview/card"
	"github.com/sofired/treerig/internal/worktree"
)

// TestSnapshotRoutes covers IndexEntry.Routes, the services a `list` URL column
// is built from: every service the card routed under the worktree's own host,
// linked or not, and whether a container of it runs.
func TestSnapshotRoutes(t *testing.T) {
	t.Run("lists every routed service in the card's order, with whether it runs", func(t *testing.T) {
		input := baseSnapshotInput(t)
		a := testRegistrations()[0]
		// A service the config never gave an overview block: routed, not linked.
		unlinked := card.IndexService{Name: "worker", URL: "http://worker.feature-a.acme.localhost"}
		input.Cards[EntryKey(a.RepositoryID, a.WorktreeID)] = testCard(a, "acme", apiService, unlinked, hosted(a, mailService)[0])
		input.Containers = mustParseInventory(t, testLine(a, "api"), testLine(a, "worker", "exited", "Exited (1) 1 minute ago"),
			testLine(a, "mail", "running", "Up 2 seconds (health: starting)"))

		entry := findBySlug(BuildSnapshot(input).Entries, "feature-a")
		want := []RoutedService{
			{Name: "api", URL: "http://feature-a.acme.localhost", Running: true},
			{Name: "worker", URL: "http://worker.feature-a.acme.localhost", Running: false},
			{Name: "mail", URL: "http://mail.feature-a.acme.localhost", Running: false},
		}
		if !slices.Equal(entry.Routes, want) {
			t.Fatalf("routes = %+v, want %+v", entry.Routes, want)
		}
		if len(entry.Services) != 2 {
			t.Fatalf("links = %+v, want only the two that opted into the overview", entry.Services)
		}
	})

	t.Run("drops a service the card puts under another host or router port", func(t *testing.T) {
		input := baseSnapshotInput(t)
		a := testRegistrations()[0]
		elsewhere := card.IndexService{Name: "api", URL: "http://api.example.com"}
		otherPort := card.IndexService{Name: "mail", URL: "http://mail.feature-a.acme.localhost:8080"}
		input.Cards[EntryKey(a.RepositoryID, a.WorktreeID)] = testCard(a, "acme", elsewhere, otherPort)

		if entry := findBySlug(BuildSnapshot(input).Entries, "feature-a"); len(entry.Routes) != 0 {
			t.Fatalf("routes = %+v", entry.Routes)
		}
	})

	t.Run("runs nothing in a worktree whose directory is missing, whatever Docker lists", func(t *testing.T) {
		input := baseSnapshotInput(t)
		gone := findBySlug(BuildSnapshot(input).Entries, "gone")
		if gone.State != worktree.StateDirectoryMissing || len(gone.Routes) != 1 || gone.Routes[0].Running {
			t.Fatalf("gone = %+v", gone)
		}
	})

	t.Run("has no routes for a worktree no card describes", func(t *testing.T) {
		input := baseSnapshotInput(t)
		a := testRegistrations()[0]
		delete(input.Cards, EntryKey(a.RepositoryID, a.WorktreeID))
		if entry := findBySlug(BuildSnapshot(input).Entries, "feature-a"); len(entry.Routes) != 0 {
			t.Fatalf("routes = %+v", entry.Routes)
		}
	})
}
