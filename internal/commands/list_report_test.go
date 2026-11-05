package commands

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/overview"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/text"
	"github.com/sofired/treerig/internal/worktree"
)

// TestListReportMapsTheSnapshot pins `list --json`'s document: explicit keys,
// [] rather than null for a worktree with no services or no URL and for an
// empty overview, and the failure notices the page shows.
func TestListReportMapsTheSnapshot(t *testing.T) {
	branch, dockerErr := "main", "docker is not running"
	snapshot := overview.IndexSnapshot{
		GeneratedAt:     "2026-09-28T10:00:00.000Z",
		DockerReachable: true,
		DockerError:     &dockerErr,
		IdentityError:   &overview.IdentityFailure{Reason: "git failed", RowsUnknown: true},
		Entries: []overview.IndexEntry{{
			RepositoryID: "r", WorktreeID: "w", Project: "acme", Domain: "acme.localhost", Slug: "main",
			Host: "main.acme.localhost", Branch: &branch, Root: "/src/acme", State: worktree.StateRunning,
			Services: []overview.IndexServiceLink{{Name: "api", Label: "API", URL: "http://main.acme.localhost", Available: true}},
			Routes: []overview.RoutedService{
				{Name: "api", URL: "http://main.acme.localhost", Running: true},
				{Name: "worker", URL: "http://worker.main.acme.localhost"},
			},
		}, {Slug: "bare", State: worktree.StateDirectoryMissing, Services: []overview.IndexServiceLink{{Name: "db", Label: "db", Purpose: "store"}}}},
		CardReadFailures: 2,
	}
	current := &registry.WorktreeIdentity{RepositoryID: "r", WorktreeID: "w"}
	out, err := text.SafeJSON(listReport(snapshot, "http://index.localhost", current, true))
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "url": "http://index.localhost",
  "generatedAt": "2026-09-28T10:00:00.000Z",
  "dockerReachable": true,
  "dockerError": "docker is not running",
  "identityError": {
    "reason": "git failed",
    "rowsUnknown": true
  },
  "cardReadFailures": 2,
  "entries": [
    {
      "repositoryId": "r",
      "worktreeId": "w",
      "project": "acme",
      "domain": "acme.localhost",
      "slug": "main",
      "host": "main.acme.localhost",
      "branch": "main",
      "root": "/src/acme",
      "current": true,
      "state": "running",
      "urls": [
        {
          "service": "api",
          "url": "http://main.acme.localhost"
        }
      ],
      "services": [
        {
          "service": "api",
          "label": "API",
          "url": "http://main.acme.localhost",
          "available": true
        }
      ]
    },
    {
      "repositoryId": "",
      "worktreeId": "",
      "project": "",
      "domain": "",
      "slug": "bare",
      "host": "",
      "current": false,
      "state": "directory_missing",
      "urls": [],
      "services": [
        {
          "service": "db",
          "label": "db",
          "purpose": "store",
          "url": ""
        }
      ]
    }
  ]
}`
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}

	// No URL loads while the router does not serve this root's URLs.
	stopped, err := text.SafeJSON(listReport(snapshot, "u", nil, false))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(stopped, `"urls": []`) != 2 {
		t.Fatalf("urls must be empty while the router does not serve them:\n%s", stopped)
	}

	// Nothing is checked while Docker is out of reach: available is absent,
	// not false.
	snapshot.DockerReachable = false
	unchecked, err := text.SafeJSON(listReport(snapshot, "u", nil, true))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(unchecked, "available") {
		t.Fatalf("available must be absent when Docker was not reachable:\n%s", unchecked)
	}

	empty, err := text.SafeJSON(listReport(overview.IndexSnapshot{DockerReachable: true}, "u", nil, true))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(empty, `"entries": []`) {
		t.Fatalf("empty overview = %s", empty)
	}
}

// TestListReportListsSharedServices pins the "shared" array: absent when there
// are no routed shared UIs, and otherwise one entry each, with "available"
// present only when Docker was asked.
func TestListReportListsSharedServices(t *testing.T) {
	snapshot := overview.IndexSnapshot{DockerReachable: true}
	none, err := text.SafeJSON(listReport(snapshot, "u", nil, true))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(none, `"shared"`) {
		t.Fatalf("shared must be absent when there are none:\n%s", none)
	}

	snapshot.Shared = []overview.IndexSharedService{{
		RepositoryID: routeRepo, Name: "mailpit", Project: "acme", Domain: "acme.localhost",
		Host: "mailpit.shared.acme.localhost", URL: "http://mailpit.shared.acme.localhost", Available: true,
	}}
	out, err := text.SafeJSON(listReport(snapshot, "u", nil, true))
	if err != nil {
		t.Fatal(err)
	}
	var report ListReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Shared) != 1 || report.Shared[0].Service != "mailpit" || report.Shared[0].URL != "http://mailpit.shared.acme.localhost" ||
		report.Shared[0].Available == nil || !*report.Shared[0].Available {
		t.Fatalf("shared = %+v", report.Shared)
	}

	snapshot.DockerReachable = false
	unchecked, err := text.SafeJSON(listReport(snapshot, "u", nil, true))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(unchecked, "available") {
		t.Fatalf("available must be absent when Docker was not reachable:\n%s", unchecked)
	}
}

// TestListJSON runs `list --all --json` through List itself, on fake Git and
// Docker runners: exactly one document on stdout, the table absent.
func TestListJSON(t *testing.T) {
	base := registrytest.TempDir(t)
	checkout := filepath.Join(base, "checkout")
	controlHome := filepath.Join(base, "control")
	for _, dir := range []string{checkout, controlHome} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	toml := "[project]\nname = \"acme\"\ndomain = \"acme.localhost\"\n[compose]\nfile = \"compose.yaml\"\n[service.api]\n"
	if err := os.WriteFile(filepath.Join(checkout, brand.ConfigFile), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	runner, git := registrytest.NewFakeRunner()
	git.Add(checkout, "main", filepath.Join(base, "common"))
	log := &logger.Recording{}
	env := &Env{
		CommandName: "treerig", Invocation: "treerig list --all --json", Logger: log, Runner: runner, Cwd: checkout,
		Getenv: func(key string) (string, bool) {
			switch key {
			case "HOME":
				return base, true
			case "TREERIG_CONTROL_HOME":
				return controlHome, true
			}
			return "", false
		},
	}
	// Docker is not there in this harness, so the command ends in an error
	// after it has printed its document.
	_ = List(context.Background(), env, ListOptions{All: true, JSON: true})
	_, printed := log.Snapshot()
	if len(printed) != 1 {
		t.Fatalf("stdout = %q, want one document", printed)
	}
	var report ListReport
	if err := json.Unmarshal([]byte(printed[0]), &report); err != nil {
		t.Fatalf("stdout is not a JSON document: %v\n%q", err, printed)
	}
	if report.URL == "" || report.GeneratedAt == "" || report.Entries == nil {
		t.Fatalf("report = %+v", report)
	}
	if !strings.HasPrefix(printed[0], "{") {
		t.Fatalf("stdout does not start a JSON object (the table?): %q", printed[0])
	}
}
