package commands_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/commands/commandstest"
	"github.com/sofired/berth/internal/registry"
)

// nestedRefusal is the start of the message a verb gives when it would wait
// on a lock its own caller holds.
const nestedRefusal = "This command would wait for the worktree lock its own caller holds"

// inside sets BERTH_WORKTREE_ID in tc's environment, as a hook of that
// worktree would see it.
func inside(tc *commandstest.TestContext, worktreeID string) {
	tc.Environ = append(tc.Environ, "PATH=/usr/bin", "BERTH_WORKTREE_ID="+worktreeID)
}

// requireNestedRefusal fails t unless err is the nested-invocation refusal.
func requireNestedRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), nestedRefusal) {
		t.Fatalf("err = %v, want the nested-invocation refusal", err)
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

		requireNestedRefusal(t, commands.DownWithContext(ctx, tc.CommandContext))
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
		requireNestedRefusal(t, err)
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
		router := commandstest.NewFakeRouter(80, "berth-router")

		_, err := up(t, tc, router, &commandstest.FakeIndex{}, commands.UpOptions{})
		requireNestedRefusal(t, err)
		if router.Ensures() != 0 {
			t.Fatalf("the router was ensured %d time(s) after the refusal", router.Ensures())
		}
	})

	t.Run("up proceeds for a checkout with no identity yet, whatever the environment says", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		inside(tc, "00000000-0000-4000-8000-000000000000")

		result, err := up(t, tc, commandstest.NewFakeRouter(80, "berth-router"), &commandstest.FakeIndex{}, commands.UpOptions{})
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

		if _, err := up(t, tc, commandstest.NewFakeRouter(80, "berth-router"), &commandstest.FakeIndex{}, commands.UpOptions{}); err != nil {
			t.Fatalf("Up: %v", err)
		}
	})
}
