package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/registry"
)

// TestDoctorReportsNewerFormats covers where doctor reports state a newer berth
// wrote (docs/design/state.md): each store's own check fails, carrying the
// exact refusal every other command gives, so the line says what to do.
func TestDoctorReportsNewerFormats(t *testing.T) {
	const upgrade = ", written by a newer berth, and this build reads format "

	t.Run("fails the control home check for a marker a newer berth wrote", func(t *testing.T) {
		opts := testOpts(t, nil)
		if err := os.MkdirAll(opts.ControlHome, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(opts.ControlHome, files.ControlHomeMarker), []byte("format 3\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		got := section(sections, "Registry")[1]
		want := "Refusing to use the control home " + opts.ControlHome + ": it is format 3" + upgrade + "2. Upgrade berth, then run this again."
		if got.Status != StatusFail || got.Subject != "control home" || got.Detail != want {
			t.Fatalf("got %+v\nwant detail %s", got, want)
		}
		// The records under a home berth refuses are not read, so they are not "none yet".
		var registrations Check
		for _, check := range section(sections, "Registry") {
			if check.ID == "registry.registrations" {
				registrations = check
			}
		}
		if registrations.Status != StatusSkip || registrations.Detail != "control home not readable" {
			t.Fatalf("registrations = %+v", registrations)
		}
	})

	t.Run("passes the control home check for this build's own marker", func(t *testing.T) {
		opts := testOpts(t, nil)
		if err := os.MkdirAll(opts.ControlHome, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(opts.ControlHome, files.ControlHomeMarker), []byte(files.ControlHomeFormat), 0o600); err != nil {
			t.Fatal(err)
		}
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if got := section(sections, "Registry")[1]; got.Status != StatusOK {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("fails the router state check for a state file a newer berth wrote", func(t *testing.T) {
		var state layout.RouterLayout
		opts := testOpts(t, func(o *doctorOptions) {
			state = layout.RouterStateFrom(o.Home)
			o.RouterState = state
		})
		if err := os.MkdirAll(state.StateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(state.StateFile, []byte(`{"version":2}`), 0o600); err != nil {
			t.Fatal(err)
		}
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		var found *Check
		for _, check := range section(sections, "Registry") {
			if check.ID == "registry.router-state" {
				found = &check
			}
		}
		want := "Refusing to use the router state " + state.StateFile + ": it is format 2" + upgrade + "1. Upgrade berth, then run this again."
		if found == nil || found.Status != StatusFail || found.Detail != want {
			t.Fatalf("got %+v\nwant detail %s", found, want)
		}
	})

	t.Run("passes the router state check when there is no state file yet", func(t *testing.T) {
		opts := testOpts(t, func(o *doctorOptions) { o.RouterState = layout.RouterStateFrom(o.Home) })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, check := range section(sections, "Registry") {
			if check.ID == "registry.router-state" && check.Status != StatusOK {
				t.Fatalf("got %+v", check)
			}
		}
	})

	t.Run("fails a registration a newer berth wrote as an unreadable record, with the upgrade instruction", func(t *testing.T) {
		opts := testOpts(t, nil)
		if err := os.MkdirAll(opts.ControlHome, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(opts.ControlHome, files.ControlHomeMarker), []byte(files.ControlHomeFormat), 0o600); err != nil {
			t.Fatal(err)
		}
		repositoryID, worktreeID := randomTestUUID(), randomTestUUID()
		dir := registry.RegistryDirectory(opts.ControlHome, repositoryID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, worktreeID+".json")
		if err := os.WriteFile(file, []byte(`{"version":2,"anything":"else"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		var found *Check
		for _, check := range section(sections, "Registry") {
			if check.ID == "registry.unreadable-record" {
				found = &check
			}
		}
		if found == nil || found.Status != StatusFail || !strings.HasPrefix(found.Detail, "Refusing to use the registration "+file+": it is format 2") ||
			!strings.HasSuffix(found.Detail, "Upgrade berth, then run this again.") {
			t.Fatalf("got %+v", found)
		}
	})

	t.Run("does not tell the user to move a shared-service record a newer berth wrote aside", func(t *testing.T) {
		newer := registry.RegistryProblem{File: "x.json", Reason: "Refusing to use the shared-service record x.json: it is format 2" + upgrade + "1. Upgrade berth, then run this again.", Newer: true}
		if got := unreadableRecordDetail(newer); got != newer.Reason {
			t.Errorf("got %q", got)
		}
		damaged := registry.RegistryProblem{File: "x.json", Reason: "Shared-service record is not valid JSON: x.json"}
		if got := unreadableRecordDetail(damaged); !strings.Contains(got, "Move the file aside") {
			t.Errorf("got %q", got)
		}
	})

	t.Run("warns, and does not promise recovery, for a lock whose unreleased record a newer berth wrote", func(t *testing.T) {
		scope := lock.Scope{Target: filepath.Join(t.TempDir(), "worktree"), Description: "worktree main"}
		if err := os.WriteFile(scope.Target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(scope.Target+".holder", []byte(`{"version":2,"anything":"else"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		check, ok := lockCheck("locks.test", "test lock", scope, "")
		want := "unreleased, and its holder record is format 2" + upgrade + "1. Upgrade berth, then run this again"
		if !ok || check.Status != StatusWarn || check.Detail != want {
			t.Fatalf("got %+v, %v\nwant detail %s", check, ok, want)
		}
	})

	t.Run("reports a checkout's unreadable identity marker once, not again under missing checkouts", func(t *testing.T) {
		f := newCheckoutFixture(t, "./verify")
		opts := f.opts()
		failure := errors.New("Invalid identity marker: x. A marker is the line \"format 1\" and then the checkout's UUID.")
		opts.RegistryFor = func(*config.Loaded) (checkoutRegistry, error) {
			return fakeCheckoutRegistry{
				current:         func(context.Context) (*registry.WorktreeRegistration, error) { return nil, failure },
				pruneCandidates: func(context.Context) ([]registry.RegisteredWorktree, error) { return nil, failure },
			}, nil
		}
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		fails := 0
		for _, check := range section(sections, "This checkout") {
			if check.Status == StatusFail && check.Detail == failure.Error() {
				fails++
			}
			if check.ID == "checkout.missing-scan" && check.Status != StatusSkip {
				t.Errorf("missing-scan = %+v", check)
			}
		}
		if fails != 1 {
			t.Fatalf("the failure is reported %d times", fails)
		}
	})
}
