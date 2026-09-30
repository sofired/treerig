package commands_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/commands/commandstest"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/registry"
)

// serviceConfig is a config with an indexed service, a subdomain-only
// service and a subdomain-plus-path service, covering the shapes URLs must
// render.
func serviceConfig() config.Config {
	label := "API"
	purpose := "REST surface"
	path := "/latest"
	return config.Config{
		Project: config.Project{Name: "acme", Domain: acmeDomain},
		Compose: config.Compose{File: "compose.yaml"},
		Service: []config.Service{
			{Name: "api", Index: &config.ServiceIndex{Label: label, Purpose: purpose}},
			{Name: "mail", Subdomain: "mail"},
			{Name: "docs", Subdomain: "docs", Path: &path},
		},
	}
}

func TestWorktreeURLs(t *testing.T) {
	cfg := serviceConfig()

	t.Run("routes the empty subdomain at the host root and prefixes the others, in config order", func(t *testing.T) {
		got := commands.WorktreeURLs(cfg, "feature-login", acmeDomain, 80)
		want := []string{
			"http://feature-login.acme.localhost",
			"http://mail.feature-login.acme.localhost",
			"http://docs.feature-login.acme.localhost/latest",
		}
		if len(got) != 3 {
			t.Fatalf("got %+v", got)
		}
		for i, w := range want {
			if got[i].URL != w {
				t.Fatalf("index %d: got %q, want %q", i, got[i].URL, w)
			}
		}
		if got[0].Label == nil || *got[0].Label != "API" || got[0].Purpose == nil || *got[0].Purpose != "REST surface" {
			t.Fatalf("expected api's index label/purpose: %+v", got[0])
		}
		if got[1].Label != nil || got[2].Label != nil {
			t.Fatalf("expected mail/docs to have no index label: %+v %+v", got[1], got[2])
		}
	})

	t.Run("uses the registered domain, not the config domain, so a stale record routes where it is actually registered", func(t *testing.T) {
		got := commands.WorktreeURLs(cfg, "main", "old.localhost", 80)
		if got[0].URL != "http://main.old.localhost" {
			t.Fatalf("got %q", got[0].URL)
		}
	})

	t.Run("appends the router port when it is not 80", func(t *testing.T) {
		got := commands.WorktreeURLs(cfg, "main", acmeDomain, 8080)
		want := []string{
			"http://main.acme.localhost:8080",
			"http://mail.main.acme.localhost:8080",
			"http://docs.main.acme.localhost:8080/latest",
		}
		for i, w := range want {
			if got[i].URL != w {
				t.Fatalf("index %d: got %q, want %q", i, got[i].URL, w)
			}
		}
	})

	t.Run("returns nothing for a project with no routed services", func(t *testing.T) {
		bare := config.Config{Project: config.Project{Name: "x", Domain: "x.localhost"}, Compose: config.Compose{File: "c.yaml"}}
		got := commands.WorktreeURLs(bare, "main", "x.localhost", 80)
		if len(got) != 0 {
			t.Fatalf("expected no URLs, got %+v", got)
		}
	})
}

func TestUrlsCommand(t *testing.T) {
	ctx := context.Background()

	t.Run("prints each routed service, and returns them", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		register(t, w, w.Root, acmeDomain, "feature-login", "complete")
		cctx := w.Context(w.Root, acmeDomain, nil)

		list, err := commands.UrlsWithContext(ctx, cctx.CommandContext, commands.OutputOptions{})
		if err != nil {
			t.Fatalf("UrlsWithContext: %v", err)
		}
		if len(list) != 2 || list[0].Service != "api" || list[1].Service != "mail" {
			t.Fatalf("unexpected list: %+v", list)
		}
		if len(cctx.Logger.Printed) != 2 {
			t.Fatalf("expected 2 printed lines, got %+v", cctx.Logger.Printed)
		}
	})

	t.Run("refuses when this worktree is not registered", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		cctx := w.Context(w.Root, acmeDomain, nil)

		_, err := commands.UrlsWithContext(ctx, cctx.CommandContext, commands.OutputOptions{})
		if err == nil {
			t.Fatalf("expected an error")
		}
		if err.Error() != "This worktree is not registered. Run 'berth up' first." {
			t.Fatalf("unexpected message: %q", err.Error())
		}
		scan, err := registry.ScanRegistrations(w.ControlHome)
		if err != nil {
			t.Fatalf("ScanRegistrations: %v", err)
		}
		if len(scan.Records) != 0 {
			t.Fatalf("expected no registry records, got %+v", scan.Records)
		}
	})

	t.Run("warns when the project routes nothing", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", "bare.localhost")
		register(t, w, w.Root, "bare.localhost", "main", "complete")
		cctx := w.Context(w.Root, "bare.localhost", nil)
		cctx.Loaded.Config.Service = nil

		list, err := commands.UrlsWithContext(ctx, cctx.CommandContext, commands.OutputOptions{})
		if err != nil {
			t.Fatalf("UrlsWithContext: %v", err)
		}
		if len(list) != 0 {
			t.Fatalf("expected no routed services, got %+v", list)
		}
		found := false
		for _, line := range cctx.Logger.Lines {
			if line == "warn: No [service.<name>] tables in berth.toml; nothing is routed." {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected the routing warning, got %+v", cctx.Logger.Lines)
		}
	})
}

// `urls --json` prints the URLReportEntry array and nothing else on stdout.
func TestUrlsCommandJSON(t *testing.T) {
	ctx := context.Background()

	t.Run("prints one array of the routed services", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		register(t, w, w.Root, acmeDomain, "feature-login", "complete")
		cctx := w.Context(w.Root, acmeDomain, nil)

		if _, err := commands.UrlsWithContext(ctx, cctx.CommandContext, commands.OutputOptions{JSON: true}); err != nil {
			t.Fatalf("UrlsWithContext: %v", err)
		}
		if len(cctx.Logger.Printed) != 1 {
			t.Fatalf("stdout = %q, want one document", cctx.Logger.Printed)
		}
		var got []map[string]any
		if err := json.Unmarshal([]byte(cctx.Logger.Printed[0]), &got); err != nil {
			t.Fatalf("stdout is not one JSON document: %v\n%q", err, cctx.Logger.Printed)
		}
		if len(got) != 2 || got[0]["service"] != "api" || got[1]["service"] != "mail" {
			t.Fatalf("got %+v", got)
		}
		// The service key is "service" here as in status and index.
		if _, ok := got[0]["name"]; ok {
			t.Fatalf("got a name key: %+v", got[0])
		}
	})

	t.Run("prints [] and warns on stderr when nothing is routed", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", "bare.localhost")
		register(t, w, w.Root, "bare.localhost", "main", "complete")
		cctx := w.Context(w.Root, "bare.localhost", nil)
		cctx.Loaded.Config.Service = nil

		if _, err := commands.UrlsWithContext(ctx, cctx.CommandContext, commands.OutputOptions{JSON: true}); err != nil {
			t.Fatalf("UrlsWithContext: %v", err)
		}
		lines, printed := cctx.Logger.Snapshot()
		if len(printed) != 1 || printed[0] != "[]" {
			t.Fatalf("stdout = %q", printed)
		}
		if len(lines) == 0 || !strings.HasPrefix(lines[len(lines)-1], "warn: No [service.<name>] tables") {
			t.Fatalf("lines = %q", lines)
		}
	})

	t.Run("still refuses an unregistered worktree, with nothing on stdout", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		cctx := w.Context(w.Root, acmeDomain, nil)

		if _, err := commands.UrlsWithContext(ctx, cctx.CommandContext, commands.OutputOptions{JSON: true}); err == nil {
			t.Fatal("expected an error")
		}
		if len(cctx.Logger.Printed) != 0 {
			t.Fatalf("stdout = %q", cctx.Logger.Printed)
		}
	})
}
