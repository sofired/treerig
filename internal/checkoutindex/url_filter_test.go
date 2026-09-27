package checkoutindex

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/sofired/berth/internal/checkoutindex/card"
	"github.com/sofired/berth/internal/registry"
)

// TestBuildSnapshotURLFilterGolden checks serviceBelongsToHost against
// internal/checkoutindex/testdata/url-filter.json, a hand-maintained
// fixture: every WHATWG URL.parse normalization the filter depends on —
// scheme and hostname case-folding, a default port dropped, a port's
// leading zeros, a trailing dot on the hostname, and an IPv6 literal. Its
// origin is in git history.
func TestBuildSnapshotURLFilterGolden(t *testing.T) {
	data, err := os.ReadFile("testdata/url-filter.json")
	if err != nil {
		t.Fatalf("reading fixture data: %v", err)
	}
	var fixture struct {
		Host  string `json:"host"`
		Cases []struct {
			Name       string `json:"name"`
			URL        string `json:"url"`
			RouterPort int    `json:"routerPort"`
			Kept       bool   `json:"kept"`
			KeptURL    string `json:"keptUrl"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("parsing fixture data: %v", err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("empty corpus in hand-maintained testdata/url-filter.json")
	}

	record := registry.RegisteredWorktree{RepositoryID: testRepo, WorktreeID: testWorktreeIDs[0], Slug: "feature-a", Domain: "acme.localhost"}

	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			input := SnapshotInput{
				Registrations: []registry.RegisteredWorktree{record},
				Cards: map[string]card.IndexCard{
					EntryKey(record.RepositoryID, record.WorktreeID): {
						Project: "acme", Domain: record.Domain, Slug: record.Slug, Host: fixture.Host,
						Root: "/work/feature-a", Branch: "feature/a", LabelNamespace: "dev.berth",
						Services:  []card.IndexService{{Name: "svc", URL: c.URL, Label: "Service", Purpose: ""}},
						UpdatedAt: "2026-01-01T00:00:00.000Z",
					},
				},
				RouterPort:  c.RouterPort,
				GeneratedAt: "2026-01-01T00:00:00.000Z",
			}
			entry := findBySlug(BuildSnapshot(input).Entries, record.Slug)
			if entry == nil {
				t.Fatal("registration not found in the snapshot")
			}
			kept := len(entry.Services) > 0
			if kept != c.Kept {
				t.Fatalf("kept = %v, want %v (url %q, routerPort %d)", kept, c.Kept, c.URL, c.RouterPort)
			}
			if kept && entry.Services[0].URL != c.KeptURL {
				t.Errorf("kept url = %q, want %q", entry.Services[0].URL, c.KeptURL)
			}
		})
	}
}
