package commands_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/commands/commandstest"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry"
)

// nestedRefusal is the exact message a verb gives when it would wait on the
// lock its own caller — host's own hooks or commands — already holds.
func nestedRefusal(host string) string {
	return fmt.Sprintf(
		"This berth invocation runs inside %s's own hooks or commands, which may hold its lock, so it would wait on itself. "+
			"A hook or command can change other worktrees, but not its own.", host)
}

// inside sets BERTH_WORKTREE_ID in tc's environment, as a hook of that
// worktree would see it.
func inside(tc *commandstest.TestContext, worktreeID string) {
	tc.Environ = append(tc.Environ, "PATH=/usr/bin", "BERTH_WORKTREE_ID="+worktreeID)
}

// requireNestedRefusal fails t unless err is exactly the nested-invocation
// refusal naming host.
func requireNestedRefusal(t *testing.T, err error, host string) {
	t.Helper()
	want := nestedRefusal(host)
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

// Every verb that takes a worktree's operation lock refuses at once when it
// runs inside that worktree's own environment, where its caller holds the
// lock (#102), and proceeds for any other worktree's.
func TestNestedWorktreeLock(t *testing.T) {
	// A regression would wait on the lock rather than fail; the deadline
	// turns that into a quick failure instead of a 25-minute stall.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("down refuses inside its own worktree, before touching Docker", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		tc := w.Context("", acme, nil)
		inside(tc, ref.WorktreeID)
		before := len(w.Docker.Calls())

		requireNestedRefusal(t, commands.DownWithContext(ctx, tc.CommandContext), layout.WorktreeHost("feature-login", acme))
		if calls := w.Docker.Calls()[before:]; len(calls) != 0 {
			t.Fatalf("Docker was called after the refusal: %v", calls)
		}
	})

	t.Run("down proceeds inside another worktree's environment", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		tc := w.Context("", acme, nil)
		inside(tc, "00000000-0000-4000-8000-000000000000")

		if err := commands.DownWithContext(ctx, tc.CommandContext); err != nil {
			t.Fatalf("DownWithContext: %v", err)
		}
	})

	t.Run("down proceeds with no BERTH_WORKTREE_ID at all", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		tc := w.Context("", acme, nil)
		tc.Environ = []string{"PATH=/usr/bin"}

		if err := commands.DownWithContext(ctx, tc.CommandContext); err != nil {
			t.Fatalf("DownWithContext: %v", err)
		}
	})

	// Unapproved, as from a hook with no terminal: the refusal must come
	// before the confirmation prompt, and before the Docker inventory.
	t.Run("worktree remove refuses inside its own worktree before Docker or the prompt, and removes nothing", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		tc := w.Context("", acme, nil)
		inside(tc, ref.WorktreeID)
		before := len(w.Docker.Calls())

		_, err := commands.RemoveWithContext(ctx, tc.CommandContext, &commands.Env{CommandName: "berth"}, commands.RemoveOptions{})
		requireNestedRefusal(t, err, layout.WorktreeHost("feature-login", acme))
		if calls := w.Docker.Calls()[before:]; len(calls) != 0 {
			t.Fatalf("Docker was called after the refusal: %v", calls)
		}
		if left := w.Docker.Owned(ref, brand.LabelNamespace); len(left) == 0 {
			t.Fatal("resources were removed after the refusal")
		}
		if current, _ := tc.Registry.Current(ctx); current == nil {
			t.Fatal("the registration was removed after the refusal")
		}
	})

	t.Run("up refuses inside its own worktree, before registering or ensuring the router", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		tc := w.Context("", acme, nil)
		givePorts(tc)
		inside(tc, ref.WorktreeID)
		router := commandstest.NewFakeRouter(80)

		_, err := commands.UpWithContext(ctx, tc.CommandContext, router, &commandstest.FakeIndex{}, commands.UpOptions{})
		requireNestedRefusal(t, err, layout.WorktreeHost("feature-login", acme))
		if router.Ensures() != 0 {
			t.Fatalf("the router was ensured %d time(s) after the refusal", router.Ensures())
		}
	})

	// No registration exists yet to read a slug from — only identity
	// markers, created directly here rather than through `up`, which would
	// also register. The refusal falls back to the slug AllocateSlug would
	// itself start from for this branch, under the config's own domain.
	t.Run("up refuses inside its own worktree before it has ever registered, naming its likely slug", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		identity, err := tc.Registry.Identities.Ensure(ctx, w.Root)
		if err != nil {
			t.Fatalf("Ensure identity: %v", err)
		}
		inside(tc, identity.WorktreeID)
		router := commandstest.NewFakeRouter(80)

		_, err = commands.UpWithContext(ctx, tc.CommandContext, router, &commandstest.FakeIndex{}, commands.UpOptions{})
		requireNestedRefusal(t, err, layout.WorktreeHost("feature-login", acme))
		if router.Ensures() != 0 {
			t.Fatalf("the router was ensured %d time(s) after the refusal", router.Ensures())
		}
	})

	t.Run("up proceeds for a checkout with no identity yet, whatever the environment says", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		inside(tc, "00000000-0000-4000-8000-000000000000")

		result, err := commands.UpWithContext(ctx, tc.CommandContext, commandstest.NewFakeRouter(80), &commandstest.FakeIndex{}, commands.UpOptions{})
		if err != nil {
			t.Fatalf("Up: %v", err)
		}
		identity, err := tc.Registry.Identities.Inspect(ctx, w.Root, registry.InspectOptions{})
		if err != nil || identity == nil || identity.WorktreeID != result.Registration.WorktreeID {
			t.Fatalf("identity = %+v, %v", identity, err)
		}
	})

	t.Run("up proceeds inside another worktree's environment", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		tc := w.Context("", acme, nil)
		givePorts(tc)
		inside(tc, "00000000-0000-4000-8000-000000000000")

		if _, err := commands.UpWithContext(ctx, tc.CommandContext, commandstest.NewFakeRouter(80), &commandstest.FakeIndex{}, commands.UpOptions{}); err != nil {
			t.Fatalf("Up: %v", err)
		}
	})
}
