package commands_test

import (
	"context"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/checkoutindex/card"
	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/commands/commandstest"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/router"
	"github.com/sofired/berth/internal/worktree"
)

// renameWorld is a registered, stopped worktree "feature-login" with a
// recorded port, a route file and an index card at its old hostname: what a
// worktree looks like after `down`, plus the leftovers a crash can leave.
type renameWorld struct {
	*commandstest.World
	ref worktree.Ref
	tc  *commandstest.TestContext
}

// newRenameWorld builds a renameWorld. running leaves its containers up.
func newRenameWorld(t *testing.T, running bool) *renameWorld {
	t.Helper()
	w := commandstest.New(t, "feature/login", acme)
	ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api", "mail")
	w.SetPorts(ref, map[string]int{"api": 30001})
	tc := w.Context("", acme, nil)
	if !running {
		if err := commands.DownWithContext(context.Background(), tc.CommandContext); err != nil {
			t.Fatal(err)
		}
	}
	rw := &renameWorld{World: w, ref: ref, tc: tc}
	doc := router.NewFragmentDocument("route-api", "Host(`api.feature-login.acme.localhost`) && PathPrefix(`/v1`)", 30001)
	if err := router.WriteFragment(w.RouterState, ref.RepositoryID, ref.WorktreeID, "api", doc); err != nil {
		t.Fatal(err)
	}
	if err := card.WriteIndexCard(w.ControlHome, ref.RepositoryID, ref.WorktreeID, card.IndexCard{
		Project: "acme", Domain: acme, Slug: "feature-login", Host: "feature-login." + acme, Root: w.Root, Branch: "feature/login",
		LabelNamespace: brand.LabelNamespace, UpdatedAt: "2026-01-01T00:00:00.000Z",
		Services: []card.IndexService{{Name: "api", URL: "http://api.feature-login.acme.localhost/v1"}},
	}); err != nil {
		t.Fatal(err)
	}
	return rw
}

// record is the worktree's registration as it stands on disk.
func (rw *renameWorld) record(t *testing.T) registry.RegisteredWorktree {
	t.Helper()
	found, err := registry.NewStore(rw.ControlHome, rw.ref.RepositoryID).Find(rw.ref.WorktreeID)
	if err != nil || found == nil {
		t.Fatalf("record = %v, %v", found, err)
	}
	return *found
}

// rule is the routing rule of the worktree's route file for the api service.
func (rw *renameWorld) rule(t *testing.T) string {
	t.Helper()
	doc, err := router.ReadFragment(rw.RouterState, router.FragmentPath(rw.RouterState, rw.ref.RepositoryID, rw.ref.WorktreeID, "api"))
	if err != nil {
		t.Fatal(err)
	}
	return doc.HTTP.Routers["route-api"].Rule
}

// rename runs RenameWithContext against the world's context.
func (rw *renameWorld) rename(slug string) (*commands.RenameResult, error) {
	return commands.RenameWithContext(context.Background(), rw.tc.CommandContext, commands.RenameOptions{Slug: slug})
}

func TestWorktreeRename(t *testing.T) {
	t.Run("moves the registration and index card to the new slug and removes the route files, keeping ports and home", func(t *testing.T) {
		rw := newRenameWorld(t, false)
		paths := layout.WorktreeLayout(rw.ControlHome, rw.ref.RepositoryID, rw.ref.WorktreeID)

		result, err := rw.rename("login")
		if err != nil {
			t.Fatal(err)
		}

		record := rw.record(t)
		if record.Slug != "login" || record.Domain != acme || result.Current.Slug != "login" || result.Previous.Slug != "feature-login" {
			t.Fatalf("record = %+v, result = %+v", record, result)
		}
		if !reflect.DeepEqual(record.Ports, map[string]int{"api": 30001}) {
			t.Fatalf("ports = %v", record.Ports)
		}
		if exists(t, router.FragmentPath(rw.RouterState, rw.ref.RepositoryID, rw.ref.WorktreeID, "api")) {
			t.Fatal("the route file survived: it would route the old hostname")
		}
		moved, err := card.ReadIndexCard(rw.ControlHome, rw.ref.RepositoryID, rw.ref.WorktreeID)
		if err != nil || moved == nil {
			t.Fatalf("card = %v, %v", moved, err)
		}
		if moved.Slug != "login" || moved.Host != "login.acme.localhost" || moved.Services[0].URL != "http://api.login.acme.localhost/v1" {
			t.Fatalf("card = %+v", moved)
		}
		if !result.Card {
			t.Fatalf("result = %+v", result)
		}
		if !exists(t, paths.Home) {
			t.Fatal("the home went")
		}
		// berth deletes no volume: the one `down` kept is still the worktree's own, found by its labels.
		if owned := rw.Docker.Owned(rw.ref, brand.LabelNamespace); len(owned) != 1 {
			t.Fatalf("owned after rename = %v, want the one volume", owned)
		}
		lines, _ := rw.tc.Logger.Snapshot()
		if last := lines[len(lines)-1]; last != "info: Renamed feature-login.acme.localhost to login.acme.localhost. Run 'berth up' to start it there." {
			t.Fatalf("last line = %q", last)
		}
	})

	t.Run("frees the old slug for another worktree and keeps the compose project name", func(t *testing.T) {
		rw := newRenameWorld(t, false)
		before := layout.ComposeProjectName(rw.ref.RepositoryID, rw.ref.WorktreeID)
		other := rw.Worktree("other", "feature/other")
		otherRef := rw.Register(other, acme, "other", brand.LabelNamespace, "api")
		if err := commands.DownWithContext(context.Background(), rw.Context(other, acme, nil).CommandContext); err != nil {
			t.Fatal(err)
		}
		if _, err := rw.rename("login"); err != nil {
			t.Fatal(err)
		}

		otherCtx := rw.Context(other, acme, nil)
		if _, err := commands.RenameWithContext(context.Background(), otherCtx.CommandContext, commands.RenameOptions{Slug: "feature-login"}); err != nil {
			t.Fatalf("the freed slug was refused: %v", err)
		}
		if after := layout.ComposeProjectName(rw.ref.RepositoryID, rw.ref.WorktreeID); after != before {
			t.Fatalf("compose project = %s, was %s", after, before)
		}
		if owned := rw.Docker.Owned(otherRef, brand.LabelNamespace); len(owned) != 1 {
			t.Fatalf("the other worktree owns %v", owned)
		}
	})

	t.Run("refuses while the worktree has containers, running or stopped, and changes nothing", func(t *testing.T) {
		rw := newRenameWorld(t, true)

		_, err := rw.rename("login")

		wantErr(t, err, `feature-login\.acme\.localhost still has 2 containers, .* Run 'berth down' first`)
		if got := rw.record(t).Slug; got != "feature-login" {
			t.Fatalf("slug = %q", got)
		}
		if got := rw.rule(t); !strings.Contains(got, "api.feature-login.") {
			t.Fatalf("rule = %q", got)
		}
	})

	t.Run("refuses a slug another worktree holds in the same domain", func(t *testing.T) {
		rw := newRenameWorld(t, false)
		other := rw.Worktree("other", "feature/other")
		otherRef := rw.Register(other, acme, "taken", brand.LabelNamespace, "api")
		rw.WriteIndexCard(otherRef.RepositoryID, otherRef.WorktreeID, "acme", acme, "taken", "taken."+acme, other, "feature/other")

		_, err := rw.rename("taken")

		wantErr(t, err, `The slug "taken" is held in acme\.localhost by `+regexp.QuoteMeta(other)+`\. Choose another slug\.`)
		if got := rw.record(t).Slug; got != "feature-login" {
			t.Fatalf("slug = %q", got)
		}
		if got := rw.rule(t); !strings.Contains(got, "api.feature-login.") {
			t.Fatalf("a refused rename touched the route: %q", got)
		}
	})

	t.Run("prints a holder's last-known path with control characters made harmless", func(t *testing.T) {
		rw := newRenameWorld(t, false)
		other := rw.Worktree("other", "feature/other")
		otherRef := rw.Register(other, acme, "taken", brand.LabelNamespace, "api")
		rw.WriteIndexCard(otherRef.RepositoryID, otherRef.WorktreeID, "acme", acme, "taken", "taken."+acme, "/checkouts/\x1b[31mred", "feature/other")

		_, err := rw.rename("taken")

		wantErr(t, err, `held in acme\.localhost by /checkouts/\x{FFFD}\[31mred\. Choose another slug\.`)
		if strings.Contains(err.Error(), "\x1b") {
			t.Fatalf("the refusal carries an escape: %q", err)
		}
	})

	t.Run("leaves the record at the old slug when the route files cannot be removed", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can remove files from a read-only directory")
		}
		rw := newRenameWorld(t, false)
		if err := os.Chmod(rw.RouterState.DynamicDir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(rw.RouterState.DynamicDir, 0o755) })

		_, err := rw.rename("login")

		wantErr(t, err, `Could not remove this worktree's route files before renaming it to login\.acme\.localhost`)
		if got := rw.record(t).Slug; got != "feature-login" {
			t.Fatalf("slug = %q", got)
		}
		if moved, _ := card.ReadIndexCard(rw.ControlHome, rw.ref.RepositoryID, rw.ref.WorktreeID); moved.Slug != "feature-login" {
			t.Fatalf("the card moved ahead of a failed route removal: %+v", moved)
		}
	})

	t.Run("accepts a slug another worktree holds in a different domain", func(t *testing.T) {
		rw := newRenameWorld(t, false)
		other := rw.Worktree("other", "feature/other")
		rw.Register(other, "other.localhost", "taken", brand.LabelNamespace, "api")

		if _, err := rw.rename("taken"); err != nil {
			t.Fatal(err)
		}
		if got := rw.record(t).Slug; got != "taken" {
			t.Fatalf("slug = %q", got)
		}
	})

	t.Run("refuses slugs that cannot be hostname labels, and the reserved one", func(t *testing.T) {
		rw := newRenameWorld(t, false)
		for slug, pattern := range map[string]string{
			"shared":                   `"shared" is reserved`,
			"":                         `cannot be empty`,
			"Login":                    `not a valid hostname label`,
			"a.b":                      `not a valid hostname label`,
			"-login":                   `not a valid hostname label`,
			"login-":                   `not a valid hostname label`,
			"log in":                   `not a valid hostname label`,
			strings.Repeat("a", 64):    `longer than 63 characters`,
			"login.acme.localhost":     `not a valid hostname label`,
			"../escape":                `not a valid hostname label`,
			"valid\nnewline":           `not a valid hostname label`,
			"ünicode":                  `not a valid hostname label`,
			"api`) || Host(`evil.test": `not a valid hostname label`,
		} {
			_, err := rw.rename(slug)
			wantErr(t, err, pattern)
		}
		if got := rw.record(t).Slug; got != "feature-login" {
			t.Fatalf("slug = %q", got)
		}
	})

	t.Run("renaming to the slug it already holds changes nothing", func(t *testing.T) {
		rw := newRenameWorld(t, true)

		result, err := rw.rename("feature-login")
		if err != nil {
			t.Fatal(err)
		}
		if !result.Unchanged {
			t.Fatalf("result = %+v", result)
		}
		lines, _ := rw.tc.Logger.Snapshot()
		if last := lines[len(lines)-1]; last != "info: This worktree is already registered as feature-login (feature-login.acme.localhost); nothing to rename." {
			t.Fatalf("last line = %q", last)
		}
	})

	t.Run("finishes a rename interrupted after the card moved, even when retried with another slug", func(t *testing.T) {
		rw := newRenameWorld(t, false)
		// The state a crash between migrating and committing leaves: the
		// card already at a new host, the record not.
		if moved, err := card.Rehost(rw.ControlHome, rw.ref.RepositoryID, rw.ref.WorktreeID, "first-try", "first-try.acme.localhost"); err != nil || !moved {
			t.Fatal(moved, err)
		}

		if _, err := rw.rename("login"); err != nil {
			t.Fatal(err)
		}
		if got := rw.record(t).Slug; got != "login" {
			t.Fatalf("slug = %q", got)
		}
		moved, _ := card.ReadIndexCard(rw.ControlHome, rw.ref.RepositoryID, rw.ref.WorktreeID)
		if moved.Slug != "login" || moved.Host != "login.acme.localhost" || moved.Services[0].URL != "http://api.login.acme.localhost/v1" {
			t.Fatalf("card = %+v", moved)
		}
	})

	t.Run("refuses while a container is stopped but not removed", func(t *testing.T) {
		rw := newRenameWorld(t, true)
		for _, id := range rw.Docker.Owned(rw.ref, brand.LabelNamespace) {
			rw.Docker.SetStatus(id, "exited")
		}

		_, err := rw.rename("login")

		wantErr(t, err, `still has 2 containers, .* Run 'berth down' first`)
		if got := rw.record(t).Slug; got != "feature-login" {
			t.Fatalf("slug = %q", got)
		}
	})

	t.Run("works for a worktree that never ran: no route files, no card", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace)
		tc := w.Context("", acme, nil)
		if err := commands.DownWithContext(context.Background(), tc.CommandContext); err != nil {
			t.Fatal(err)
		}

		result, err := commands.RenameWithContext(context.Background(), tc.CommandContext, commands.RenameOptions{Slug: "login"})
		if err != nil {
			t.Fatal(err)
		}
		if result.Card {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("refuses an unregistered checkout, naming the way forward", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)

		_, err := commands.RenameWithContext(context.Background(), tc.CommandContext, commands.RenameOptions{Slug: "login"})

		wantErr(t, err, `This worktree is not registered\. Run 'berth up' first\.`)
	})

	t.Run("refuses inside its own worktree's hooks, before Docker", func(t *testing.T) {
		rw := newRenameWorld(t, false)
		inside(rw.tc, rw.ref.WorktreeID)
		before := len(rw.Docker.Calls())

		_, err := rw.rename("login")

		requireNestedRefusal(t, err, "feature-login.acme.localhost")
		if calls := rw.Docker.Calls()[before:]; len(calls) != 0 {
			t.Fatalf("Docker was called after the refusal: %v", calls)
		}
	})

	t.Run("refuses while an unreadable record might hold the hostname", func(t *testing.T) {
		rw := newRenameWorld(t, false)
		dir := registry.RegistryDirectory(rw.ControlHome, rw.ref.RepositoryID)
		if err := os.WriteFile(dir+"/00000000-0000-4000-8000-000000000001.json", []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}

		_, err := rw.rename("login")

		wantErr(t, err, `Refusing to allocate a hostname while the registry has unreadable records`)
		if got := rw.record(t).Slug; got != "feature-login" {
			t.Fatalf("slug = %q", got)
		}
	})

	t.Run("up keeps the renamed slug rather than allocating from the branch again", func(t *testing.T) {
		rw := newRenameWorld(t, false)
		if _, err := rw.rename("login"); err != nil {
			t.Fatal(err)
		}

		ensured, err := rw.tc.Registry.Ensure(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		if ensured.Slug != "login" {
			t.Fatalf("slug after Ensure = %q", ensured.Slug)
		}
	})
}
