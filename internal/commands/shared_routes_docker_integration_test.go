//go:build integration

package commands_test

// The routed web UI of a shared service (#123, docs/design/routing.md,
// decisions 19 and 20) on a real daemon, through a real proxy: the UI answers
// at <name>.shared.<domain> on the router's port, the index lists it, doctor
// probes its route, `shared down` unroutes it and `shared up` routes it again,
// and the router repairs a route file that points at a stale port from the
// record. The stand-in for Mailpit is the busybox httpd of the suite's other
// shared test. Every port comes from the test range, never the default one.

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/router"
)

func TestSharedWebUIThroughTheProxyOnARealDaemon(t *testing.T) {
	w := newWalkFixture(t, "sharedui", "feature/ui", sharedWalkToml, sharedWalkCompose)
	w.writeShared(t, w.checkout, "shared v1")
	ctx := context.Background()
	const uiHost = "mailpit.shared.shared-walk.localhost"
	served := func(status int, body string) bool { return status == http.StatusOK && body == "shared v1\n" }

	result := w.up(t)
	repository := result.Registration.RepositoryID
	record, err := registry.NewStore(w.controlHome, repository).FindShared("mailpit")
	if err != nil || record == nil || record.Routed != 8025 || record.Host() != uiHost {
		t.Fatalf("record = %+v, %v", record, err)
	}
	routePath := router.SharedFragmentPath(w.state, repository, "mailpit")

	t.Run("the UI answers at <name>.shared.<domain> through the proxy", func(t *testing.T) {
		w.eventually(t, uiHost, served)
		// The raw port is not routed: its host is unmatched.
		if status, _ := w.get("smtp.shared.shared-walk.localhost"); status != http.StatusNotFound {
			t.Errorf("an unrouted shared host answered %d", status)
		}
		doc, err := router.ReadFragment(w.state, routePath)
		if err != nil || doc.TargetPort() != record.Ports[8025] {
			t.Fatalf("route file target = %d (%v), want %d", doc.TargetPort(), err, record.Ports[8025])
		}
	})

	t.Run("the worktree's own sweeps leave the shared route alone", func(t *testing.T) {
		if err := router.RemoveWorktreeFragments(w.state, repository, result.Registration.WorktreeID); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(routePath); err != nil {
			t.Fatalf("the shared route file went with the worktree's: %v", err)
		}
	})

	t.Run("the index lists it in a group of its own, and shows the checkout's state", func(t *testing.T) {
		page, err := os.ReadFile(w.state.IndexFile)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`id="shared-services"`, `href="http://` + uiHost, "feature-ui.shared-walk.localhost"} {
			if !strings.Contains(string(page), want) {
				t.Errorf("the index page lacks %q", want)
			}
		}
		if strings.Contains(string(page), "Docker could not be asked") {
			t.Error("a shared container made the inventory unreadable")
		}
		// The class name is also in the page's stylesheet: look for the element.
		if strings.Contains(string(page), `<span class="service-link service-disabled"`) {
			t.Error("the running UI is greyed out")
		}
	})

	t.Run("doctor probes the route and finds the file for the record", func(t *testing.T) {
		sections, err := commands.DoctorSections(ctx, w.env, w.svc, w.state)
		if err != nil {
			t.Fatal(err)
		}
		probed, routed := false, false
		for _, section := range sections {
			for _, check := range section.Checks {
				switch {
				case check.ID == "router.route-probe" && check.Instance == uiHost:
					probed = check.Status == commands.StatusOK
				case check.ID == "shared.route" && check.Instance == "mailpit":
					routed = check.Status == commands.StatusOK
				case check.ID == "router.route-conflict", check.ID == "router.orphaned-routes":
					t.Errorf("doctor reports %s: %s", check.ID, check.Detail)
				}
			}
		}
		if !probed || !routed {
			t.Errorf("probed = %v, shared.route ok = %v", probed, routed)
		}
	})

	t.Run("the router repairs a route file pointing at a stale port from the record", func(t *testing.T) {
		if err := router.WriteSharedFragment(w.state, repository, "mailpit", record.Domain, record.Ports[8025]+1); err != nil {
			t.Fatal(err)
		}
		if err := w.svc.Ensure(ctx); err != nil {
			t.Fatal(err)
		}
		w.eventually(t, uiHost, served)
	})

	t.Run("shared down unroutes it and keeps the record; shared up routes it again", func(t *testing.T) {
		cctx := w.context(t)
		if err := commands.SharedDownWithContext(ctx, cctx, w.env, commands.SharedDownOptions{}); err != nil {
			t.Fatalf("shared down: %v", err)
		}
		if _, err := os.Stat(routePath); !os.IsNotExist(err) {
			t.Fatalf("route file after shared down: %v", err)
		}
		w.eventually(t, uiHost, func(status int, _ string) bool { return status == http.StatusNotFound })
		if kept, err := registry.NewStore(w.controlHome, repository).FindShared("mailpit"); err != nil || kept == nil || kept.Routed != 8025 {
			t.Fatalf("record after shared down = %+v, %v", kept, err)
		}

		cctx = w.context(t)
		if err := commands.SharedUpWithContext(ctx, cctx, commands.SharedRouter(w.env, w.svc, w.state), commands.BestEffortIndex(cctx), commands.SharedUpOptions{}); err != nil {
			t.Fatalf("shared up: %v", err)
		}
		w.eventually(t, uiHost, served)
	})

	t.Run("down --volumes unroutes it and releases the record", func(t *testing.T) {
		cctx := w.context(t)
		if err := commands.SharedDownWithContext(ctx, cctx, w.env, commands.SharedDownOptions{Volumes: true, Yes: true}); err != nil {
			t.Fatalf("shared down --volumes: %v", err)
		}
		if _, err := os.Stat(routePath); !os.IsNotExist(err) {
			t.Fatalf("route file after shared down --volumes: %v", err)
		}
		if gone, err := registry.NewStore(w.controlHome, repository).FindShared("mailpit"); err != nil || gone != nil {
			t.Fatalf("record after shared down --volumes = %+v, %v", gone, err)
		}
		w.eventually(t, uiHost, func(status int, _ string) bool { return status == http.StatusNotFound })
	})

	w.downAndRemove(t, result, "feature-ui.shared-walk.localhost")
}
