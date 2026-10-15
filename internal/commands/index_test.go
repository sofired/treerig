package commands

// Round 2 of review's item 2: newCheckoutIndex must refuse a symlinked
// router state directory the same way RouterUp already does, before it ever
// creates the lock file or writes the index page into whatever the symlink
// points at. A white-box (package commands) test file, not commandstest,
// which itself imports this package and would cycle back into it.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/checkoutindex"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/router"
	"github.com/sofired/treerig/internal/run/fakerun"
	"github.com/sofired/treerig/internal/text"
)

// symlinkedCommandContext builds a minimal CommandContext whose router state
// directory is a symlink to target, for exercising newCheckoutIndex's own
// router.SecureState refusal in isolation from a full Env/config-on-disk
// harness (Context(env), which RefreshIndex itself goes through, needs
// more setup than this fix's own seam does).
func symlinkedCommandContext(t *testing.T, target string) (*CommandContext, *logger.Recording) {
	t.Helper()
	controlHome := t.TempDir()
	stateParent := t.TempDir()
	stateDir := filepath.Join(stateParent, "router")
	if err := os.Symlink(target, stateDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	routerState := layout.RouterLayout{
		StateDir:  stateDir,
		IndexDir:  filepath.Join(stateDir, "index"),
		IndexFile: filepath.Join(stateDir, "index", "index.html"),
		LockPath:  filepath.Join(stateDir, "lock"),
	}
	log := &logger.Recording{}
	reg := registry.New(registry.Options{
		ControlHome: controlHome, Root: t.TempDir(), Runner: &fakerun.Runner{}, Logger: log, CommandName: "treerig",
	})
	return &CommandContext{
		CommandName: "treerig", Invocation: "treerig", Logger: log, Runner: &fakerun.Runner{},
		ControlHome: controlHome, RouterPort: 80, RouterState: routerState, Registry: reg,
	}, log
}

// TestNewCheckoutIndexRefusesASymlinkedRouterStateDirectory is the "index
// verb" side of item 2: a symlinked router state directory must fail
// newCheckoutIndex outright, the way RefreshIndex needs it to (a refresh
// that never starts must not report success and print nothing useful).
func TestNewCheckoutIndexRefusesASymlinkedRouterStateDirectory(t *testing.T) {
	cctx, _ := symlinkedCommandContext(t, t.TempDir())

	if _, err := newCheckoutIndex(cctx); err == nil {
		t.Fatal("expected newCheckoutIndex to refuse a symlinked router state directory")
	}
}

// TestRefreshIndexBestEffortWarnsInsteadOfFailingOnASymlinkedRouterState is
// the best-effort side of item 2: down and worktree remove must not fail
// their own command over this — they warn, in RefreshBestEffort's own
// wording, and move on.
func TestRefreshIndexBestEffortWarnsInsteadOfFailingOnASymlinkedRouterState(t *testing.T) {
	cctx, log := symlinkedCommandContext(t, t.TempDir())

	refreshIndexBestEffort(context.Background(), cctx, "down")

	lines, _ := log.Snapshot()
	wantPrefix := "warn: Could not refresh the checkout index after down: "
	if len(lines) != 1 || !strings.HasPrefix(lines[0], wantPrefix) {
		t.Fatalf("lines = %v, want exactly one line starting %q", lines, wantPrefix)
	}
}

// TestRouterLockScope replaces internal/lock's own deleted TestRouterScope
// (#80, review round 1 item 1): routerLockScope is now the one place that
// builds the shared router's lock scope, always from an already-resolved
// layout.RouterLayout, never a raw Getenv, so its own test moved here.
func TestRouterLockScope(t *testing.T) {
	state := layout.RouterLayout{LockPath: filepath.Join(t.TempDir(), "router", "lock")}

	t.Run("names the scope and its project marker explicitly", func(t *testing.T) {
		scope := routerLockScope(state, "treerig-router-explicit")
		if scope.Description != "shared router" {
			t.Errorf("Description = %q, want %q", scope.Description, "shared router")
		}
		if scope.Markers.Project != "treerig-router-explicit" {
			t.Errorf("Markers.Project = %q, want %q", scope.Markers.Project, "treerig-router-explicit")
		}
		if scope.Markers.WorktreeID != "" {
			t.Errorf("Markers.WorktreeID = %q, want empty: the router is not a worktree", scope.Markers.WorktreeID)
		}
	})

	t.Run("resolves router.ResolveProjectName's own default when given no project name", func(t *testing.T) {
		scope := routerLockScope(state, "")
		if scope.Markers.Project != router.ResolveProjectName() {
			t.Errorf("Markers.Project = %q, want %q", scope.Markers.Project, router.ResolveProjectName())
		}
	})

	t.Run("targets state's own lock file, named for whichever variable governs the router's state directory", func(t *testing.T) {
		scope := routerLockScope(state, "treerig-router")
		if filepath.Base(scope.Target) != "lock" {
			t.Errorf("Target = %q, want a file named lock", scope.Target)
		}
		if scope.HomeEnvVar != brand.HomeEnv {
			t.Errorf("HomeEnvVar = %q, want %q: the router has no control-home override of its own", scope.HomeEnvVar, brand.HomeEnv)
		}
	})
}

// TestIndexReportMapsTheSnapshot pins `index --json`'s document: explicit
// keys, [] rather than null for a checkout with no services or an empty
// index, and the failure notices the text form shows.
func TestIndexReportMapsTheSnapshot(t *testing.T) {
	branch, dockerErr := "main", "docker is not running"
	snapshot := checkoutindex.IndexSnapshot{
		GeneratedAt:     "2026-09-28T10:00:00.000Z",
		DockerReachable: true,
		DockerError:     &dockerErr,
		IdentityError:   &checkoutindex.IdentityFailure{Reason: "git failed", RowsUnknown: true},
		Entries: []checkoutindex.IndexEntry{{
			RepositoryID: "r", WorktreeID: "w", Project: "acme", Domain: "acme.localhost", Slug: "main",
			Host: "main.acme.localhost", Branch: &branch, Root: "/src/acme", Status: checkoutindex.StatusRunning,
			Services: []checkoutindex.IndexServiceLink{{Name: "api", Label: "API", URL: "http://main.acme.localhost", Available: true}},
		}, {Slug: "bare", Status: checkoutindex.StatusMissing, Services: []checkoutindex.IndexServiceLink{{Name: "db", Label: "db", Purpose: "store"}}}},
		CardReadFailures: 2,
	}
	out, err := text.SafeJSON(indexReport(snapshot, "http://index.localhost"))
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
      "status": "running",
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
      "status": "missing",
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

	// Nothing is checked while Docker is out of reach: available is absent,
	// not false.
	snapshot.DockerReachable = false
	unchecked, err := text.SafeJSON(indexReport(snapshot, "u"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(unchecked, "available") {
		t.Fatalf("available must be absent when Docker was not reachable:\n%s", unchecked)
	}

	empty, err := text.SafeJSON(indexReport(checkoutindex.IndexSnapshot{DockerReachable: true}, "u"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(empty, `"entries": []`) {
		t.Fatalf("empty index = %s", empty)
	}
}

// TestRefreshIndexJSON runs `index --json` through RefreshIndex itself, on
// fake Git and Docker runners: exactly one document on stdout, the text form
// absent.
func TestRefreshIndexJSON(t *testing.T) {
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
		CommandName: "treerig", Invocation: "treerig index", Logger: log, Runner: runner, Cwd: checkout,
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
	if err := RefreshIndex(context.Background(), env, OutputOptions{JSON: true}); err != nil {
		t.Fatalf("RefreshIndex: %v", err)
	}
	_, printed := log.Snapshot()
	if len(printed) != 1 {
		t.Fatalf("stdout = %q, want one document", printed)
	}
	var report IndexReport
	if err := json.Unmarshal([]byte(printed[0]), &report); err != nil {
		t.Fatalf("stdout is not a JSON document: %v\n%q", err, printed)
	}
	if report.URL == "" || report.GeneratedAt == "" || report.Entries == nil {
		t.Fatalf("report = %+v", report)
	}
	if !strings.HasPrefix(printed[0], "{") {
		t.Fatalf("stdout does not start a JSON object (the text form?): %q", printed[0])
	}
}

// TestIndexReportListsSharedServices pins `index --json`'s additive "shared"
// array: absent when there are no routed shared UIs, and otherwise one entry
// each, with "available" present only when Docker was asked.
func TestIndexReportListsSharedServices(t *testing.T) {
	snapshot := checkoutindex.IndexSnapshot{DockerReachable: true}
	none, err := text.SafeJSON(indexReport(snapshot, "u"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(none, `"shared"`) {
		t.Fatalf("shared must be absent when there are none:\n%s", none)
	}

	snapshot.Shared = []checkoutindex.IndexSharedService{{
		RepositoryID: routeRepo, Name: "mailpit", Project: "acme", Domain: "acme.localhost",
		Host: "mailpit.shared.acme.localhost", URL: "http://mailpit.shared.acme.localhost", Available: true,
	}}
	out, err := text.SafeJSON(indexReport(snapshot, "u"))
	if err != nil {
		t.Fatal(err)
	}
	var report IndexReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Shared) != 1 || report.Shared[0].Service != "mailpit" || report.Shared[0].URL != "http://mailpit.shared.acme.localhost" ||
		report.Shared[0].Available == nil || !*report.Shared[0].Available {
		t.Fatalf("shared = %+v", report.Shared)
	}

	snapshot.DockerReachable = false
	unchecked, err := text.SafeJSON(indexReport(snapshot, "u"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(unchecked, "available") {
		t.Fatalf("available must be absent when Docker was not reachable:\n%s", unchecked)
	}
}
