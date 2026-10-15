package commands_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/commands/commandstest"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
)

// sharedChecks are the Shared services section's checks with this id.
func sharedChecks(t *testing.T, tc *commandstest.TestContext, id string) []commands.Check {
	t.Helper()
	checks, err := commands.SharedDoctorForTest(context.Background(), tc.CommandContext)
	if err != nil {
		t.Fatal(err)
	}
	var found []commands.Check
	for _, check := range checks {
		if check.ID == id {
			found = append(found, check)
		}
	}
	return found
}

// only is the single check in checks, failing the test if there is not one.
func only(t *testing.T, checks []commands.Check) commands.Check {
	t.Helper()
	if len(checks) != 1 {
		t.Fatalf("checks = %+v, want exactly one", checks)
	}
	return checks[0]
}

func TestDoctorSharedServices(t *testing.T) {
	t.Run("has no section for a repository that declares none and runs none", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		tc := w.Context("", acme, nil)
		checks, err := commands.SharedDoctorForTest(context.Background(), tc.CommandContext)
		if err != nil || len(checks) != 0 {
			t.Fatalf("checks = %+v, %v", checks, err)
		}
	})

	t.Run("a healthy shared service is ok in every line", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		checks, err := commands.SharedDoctorForTest(context.Background(), tc.CommandContext)
		if err != nil {
			t.Fatal(err)
		}
		if len(checks) == 0 {
			t.Fatal("no section")
		}
		for _, check := range checks {
			if check.Status != commands.StatusOK {
				t.Errorf("%s %s: %s %s", check.ID, check.Instance, check.Status, check.Detail)
			}
		}
		if check := only(t, sharedChecks(t, tc, "shared.connection")); check.Detail != "connected" || check.Instance != "mailpit" {
			t.Errorf("connection = %+v", check)
		}
		if got := sharedChecks(t, tc, "shared.port"); len(got) != 2 {
			t.Errorf("ports = %+v", got)
		}
	})

	t.Run("a stopped, absent or unhealthy service says so, with the way out", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		id := w.mailpit(t, tc)
		w.Docker.SetHealth(id, "unhealthy")
		if check := only(t, sharedChecks(t, tc, "shared.service")); check.Status != commands.StatusWarn || !strings.Contains(check.Detail, "health check fails") {
			t.Errorf("unhealthy: %+v", check)
		}
		w.Docker.SetStatus(id, "exited")
		if check := only(t, sharedChecks(t, tc, "shared.service")); check.Status != commands.StatusWarn || !strings.Contains(check.Detail, "stopped") || !strings.Contains(check.Detail, "treerig shared up") {
			t.Errorf("stopped: %+v", check)
		}
		if err := w.sharedDown(tc, commands.SharedDownOptions{}); err != nil {
			t.Fatal(err)
		}
		if check := only(t, sharedChecks(t, tc, "shared.service")); check.Status != commands.StatusSkip || !strings.Contains(check.Detail, "not running") {
			t.Errorf("absent: %+v", check)
		}
	})

	t.Run("warns when the definition differs, naming who started it", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		w.Docker.SetSharedService("mailpit", mailpitDefinition("axllent/mailpit:v2"))
		check := only(t, sharedChecks(t, tc, "shared.service"))
		if check.Status != commands.StatusWarn || !strings.Contains(check.Detail, "different definition than this worktree's (started by feature-login.acme.localhost)") ||
			!strings.Contains(check.Detail, "treerig shared up --recreate mailpit") {
			t.Errorf("check = %+v", check)
		}
	})

	t.Run("reports a running consumer that is not connected: up but not connected", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		if err := w.Docker.Detach(w.mailpit(t, tc), w.Docker.WorktreeDefaultNetwork(refOf(result))); err != nil {
			t.Fatal(err)
		}
		check := only(t, sharedChecks(t, tc, "shared.connection"))
		if check.Status != commands.StatusWarn || !strings.Contains(check.Detail, "up but not connected: api (network ") ||
			!strings.Contains(check.Detail, "treerig up' or 'treerig shared up' repairs it") {
			t.Errorf("check = %+v", check)
		}
	})

	t.Run("skips the connection line while none of the worktree's consumers runs", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		if err := commands.DownWithContext(context.Background(), tc.CommandContext); err != nil {
			t.Fatal(err)
		}
		if check := only(t, sharedChecks(t, tc, "shared.connection")); check.Status != commands.StatusSkip {
			t.Errorf("check = %+v", check)
		}
	})

	t.Run("a pinned port the running container does not publish is a warning", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		// The record moves on without the container.
		store := tc.Registry.Store(result.Registration.RepositoryID)
		if _, err := store.SyncSharedPorts(context.Background(), registry.Writer{UserHome: w.UserHome}, registry.SharedPortPlan{
			Name: "mailpit", Domain: acme, Ports: []int{8025, 1025}, Reallocate: []int{1025}, Range: registrytest.PortRange, Probe: w.Probe,
		}); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, check := range sharedChecks(t, tc, "shared.port") {
			if check.Instance == "mailpit:1025" {
				found = true
				if check.Status != commands.StatusWarn || !strings.Contains(check.Detail, "does not publish it") {
					t.Errorf("check = %+v", check)
				}
			}
		}
		if !found {
			t.Fatal("no line for port 1025")
		}
	})

	t.Run("a pinned port something else holds while the service is down is a warning that names the way out", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		record, _ := tc.Registry.Store(result.Registration.RepositoryID).FindShared("mailpit")
		if err := w.sharedDown(tc, commands.SharedDownOptions{}); err != nil {
			t.Fatal(err)
		}
		w.Probe.Occupy(record.Ports[1025])
		var held *commands.Check
		for _, check := range sharedChecks(t, tc, "shared.port") {
			if check.Instance == "mailpit:1025" {
				held = &check
			}
		}
		if held == nil || held.Status != commands.StatusWarn || !strings.Contains(held.Detail, "treerig shared up --reallocate-port mailpit:1025") {
			t.Fatalf("check = %+v", held)
		}
	})

	t.Run("a running instance with no record of its ports is reported", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		if err := os.Remove(layout.SharedLayout(w.ControlHome, result.Registration.RepositoryID).RecordPath("mailpit")); err != nil {
			t.Fatal(err)
		}
		if check := only(t, sharedChecks(t, tc, "shared.record")); check.Status != commands.StatusWarn || !strings.Contains(check.Detail, "no record of its pinned ports") {
			t.Errorf("check = %+v", check)
		}
	})

	t.Run("an unreadable record is a failure, named by repository and file", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		bad := layout.SharedLayout(w.ControlHome, result.Registration.RepositoryID).RecordPath("broken")
		if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		check := only(t, sharedChecks(t, tc, "shared.unreadable-record"))
		if check.Status != commands.StatusFail || check.Instance != result.Registration.RepositoryID+"/broken.json" {
			t.Errorf("check = %+v", check)
		}
	})

	t.Run("a shared container on the network of a stopped or removed worktree, or a foreign one, is reported and never touched", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		id := w.mailpit(t, tc)
		repository := result.Registration.RepositoryID

		gone := w.Docker.AddNetwork("gone_default", map[string]string{
			brand.ManagedLabel(brand.LabelNamespace): "true", brand.RepositoryLabel(brand.LabelNamespace): repository,
			brand.WorktreeLabel(brand.LabelNamespace): "6f2a1c3e-0000-4000-8000-000000000001",
		})
		foreign := w.Docker.AddNetwork("corp", map[string]string{})
		w.Docker.Attach(id, gone, "mailpit")
		w.Docker.Attach(id, foreign, "mailpit")
		before := len(w.Docker.Calls())

		var got = map[string]commands.Check{}
		for _, check := range sharedChecks(t, tc, "shared.attachment") {
			got[check.Instance] = check
		}
		if len(got) != 2 {
			t.Fatalf("attachments = %+v", got)
		}
		if c := got["mailpit:gone_default"]; c.Status != commands.StatusWarn || !strings.Contains(c.Detail, "attached to the network of a removed worktree, which is stopped or gone") {
			t.Errorf("gone: %+v", c)
		}
		if c := got["mailpit:corp"]; c.Status != commands.StatusWarn || !strings.Contains(c.Detail, "carries no worktree's labels") {
			t.Errorf("foreign: %+v", c)
		}
		for _, call := range w.Docker.Calls()[before:] {
			if strings.Contains(call, "disconnect") || strings.Contains(call, " rm ") {
				t.Errorf("doctor touched something: %s", call)
			}
		}
	})

	t.Run("a container no declared service accounts for is information, as is a running instance with no registered worktree", func(t *testing.T) {
		w := newSharedWorld(t)
		w.Docker.SetSharedService("db", map[string]any{"image": "busybox"})
		w.Docker.SetSharedService("mailpit", map[string]any{"image": "mailpit", "depends_on": map[string]any{"db": map[string]any{}}})
		tc := w.context("")
		w.mustUp(t, tc)
		if check := only(t, sharedChecks(t, tc, "shared.orphan")); check.Status != commands.StatusOK || check.Instance != "db" {
			t.Errorf("orphan = %+v", check)
		}
		if got := sharedChecks(t, tc, "shared.unused"); len(got) != 0 {
			t.Errorf("unused = %+v while a worktree is registered", got)
		}
		if _, err := remove(t, tc, approved(commands.RemoveOptions{})); err != nil {
			t.Fatal(err)
		}
		if check := only(t, sharedChecks(t, tc, "shared.unused")); check.Status != commands.StatusOK || !strings.Contains(check.Detail, "no registered worktree") {
			t.Errorf("unused = %+v", check)
		}
	})

	t.Run("a shared file the worktree cannot use is a failure that says why", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		w.Docker.SetSharedService("mailpit", map[string]any{"image": "i", "build": map[string]any{"context": "."}})
		check := only(t, sharedChecks(t, tc, "shared.definition"))
		if check.Status != commands.StatusFail || !strings.Contains(check.Detail, "has build:") {
			t.Errorf("check = %+v", check)
		}
	})

	t.Run("a shared file that publishes a port of its own is a failure that says where to declare it", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		entry := mailpitDefinition("axllent/mailpit:v1.31.3")
		entry["ports"] = []any{map[string]any{"target": 8025, "published": "8025"}}
		w.Docker.SetSharedService("mailpit", entry)
		check := only(t, sharedChecks(t, tc, "shared.definition"))
		if check.Status != commands.StatusFail || !strings.Contains(check.Detail, "has ports:") || !strings.Contains(check.Detail, "[shared.service.mailpit]") {
			t.Errorf("check = %+v", check)
		}
	})
}

func TestDoctorLocksShowsTheSharedLock(t *testing.T) {
	w := newSharedWorld(t)
	tc := w.context("")
	result := w.mustUp(t, tc)
	scope := lock.SharedScope(w.ControlHome, result.Registration.RepositoryID)

	// Released: nothing to say.
	checks, err := commands.LockChecksForTest(w.ControlHome)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range checks {
		if check.ID == "locks.shared" {
			t.Fatalf("a free shared lock is reported: %+v", check)
		}
	}

	holder := lock.New(scope, lock.Options{Logger: logger.Silent{}, Watchdog: -1, Holder: "treerig up"})
	if _, err := holder.Acquire(context.Background(), "treerig up"); err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	checks, err = commands.LockChecksForTest(w.ControlHome)
	if err != nil {
		t.Fatal(err)
	}
	var found *commands.Check
	for _, check := range checks {
		if check.ID == "locks.shared" {
			found = &check
		}
	}
	if found == nil || found.Status != commands.StatusOK || found.Instance != result.Registration.RepositoryID ||
		found.Subject != "shared services of the repository of feature-login.acme.localhost" || !strings.Contains(found.Detail, "held by") {
		t.Fatalf("check = %+v", found)
	}
}
