package commands_test

// Pinning tests for #55's routing cleanup: `down` and `worktree remove`
// must leave a worktree unrouted, whatever else about its Docker footprint
// they do or do not find. This behaviour follows from #55's move to the
// file provider and each worktree's own dedicated router network.

import (
	"context"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/backend"
	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/commands/commandstest"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/router"
	"github.com/sofired/berth/internal/worktree"
)

func TestWorktreeRoutingCleanup(t *testing.T) {
	ctx := context.Background()

	t.Run("down removes this worktree's own route fragments, leaving another worktree's alone", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api", "mail")
		other := worktree.Ref{RepositoryID: "11111111-1111-1111-1111-111111111111", WorktreeID: "22222222-2222-2222-2222-222222222222", Slug: "other"}
		doc := router.NewFragmentDocument("x", "Host(`x.localhost`)", "x", 8080)
		if err := router.WriteFragment(w.RouterState, ref.RepositoryID, ref.WorktreeID, "api", doc); err != nil {
			t.Fatal(err)
		}
		if err := router.WriteFragment(w.RouterState, ref.RepositoryID, ref.WorktreeID, "mail", doc); err != nil {
			t.Fatal(err)
		}
		if err := router.WriteFragment(w.RouterState, other.RepositoryID, other.WorktreeID, "web", doc); err != nil {
			t.Fatal(err)
		}
		tc := w.Context("", acme, nil)

		if err := commands.DownWithContext(ctx, tc.CommandContext); err != nil {
			t.Fatal(err)
		}

		for _, service := range []string{"api", "mail"} {
			path := router.FragmentPath(w.RouterState, ref.RepositoryID, ref.WorktreeID, service)
			if exists, err := files.PathExists(path); err != nil {
				t.Fatal(err)
			} else if exists {
				t.Errorf("expected %s's fragment gone after down, still there: %s", service, path)
			}
		}
		otherPath := router.FragmentPath(w.RouterState, other.RepositoryID, other.WorktreeID, "web")
		if exists, err := files.PathExists(otherPath); err != nil {
			t.Fatal(err)
		} else if !exists {
			t.Error("expected another worktree's fragment to survive this worktree's down")
		}
	})

	t.Run("worktree remove removes this worktree's own route fragments", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		doc := router.NewFragmentDocument("x", "Host(`x.localhost`)", "x", 8080)
		if err := router.WriteFragment(w.RouterState, ref.RepositoryID, ref.WorktreeID, "api", doc); err != nil {
			t.Fatal(err)
		}
		tc := w.Context("", acme, nil)

		if _, err := commands.RemoveWithContext(ctx, tc.CommandContext, &commands.Env{CommandName: "berth"}, approved(commands.RemoveOptions{})); err != nil {
			t.Fatal(err)
		}

		path := router.FragmentPath(w.RouterState, ref.RepositoryID, ref.WorktreeID, "api")
		if exists, err := files.PathExists(path); err != nil {
			t.Fatal(err)
		} else if exists {
			t.Errorf("expected the fragment gone after worktree remove, still there: %s", path)
		}
	})

	t.Run("down and worktree remove tolerate a worktree that was never routed at all", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		tc := w.Context("", acme, nil)

		if err := commands.DownWithContext(ctx, tc.CommandContext); err != nil {
			t.Fatalf("down on an unrouted worktree: %v", err)
		}
	})

	// RouterNetworkName is what backend.overrideDocument names a worktree's
	// own network, and what commands.unrouteWorktree (through
	// routerFromContext) must derive identically from the same two IDs
	// alone, with no config in scope, so `worktree remove` can disconnect
	// the right network for a target it may have no berth.toml for at all.
	t.Run("RouterNetworkName is derivable from IDs alone, matching what overrideDocument names", func(t *testing.T) {
		ref := worktree.Ref{RepositoryID: "11111111-1111-1111-1111-111111111111", WorktreeID: "22222222-2222-2222-2222-222222222222"}
		if got, want := backend.RouterNetworkName(ref.RepositoryID, ref.WorktreeID), backend.RouterNetworkName(ref.RepositoryID, ref.WorktreeID); got != want {
			t.Fatalf("RouterNetworkName is not stable: %q vs %q", got, want)
		}
	})
}

// TestUnrouteWorktreeDisconnectFailure pins unrouteWorktree's own
// hard-fail/best-effort split (internal/commands/router.go): a Docker
// failure disconnecting the shared router from this worktree's own network
// is only a warning, and down/worktree remove still succeed — but a
// cancellation is never downgraded that way.
func TestUnrouteWorktreeDisconnectFailure(t *testing.T) {
	ctx := context.Background()

	// A container under the router's own default project/service, so
	// unrouteWorktree's DisconnectNetwork call finds a router to disconnect
	// (and so a call) rather than returning early for "no container".
	addRouterContainer := func(w *commandstest.World) {
		w.Docker.Add("container", map[string]string{
			"com.docker.compose.project": router.DefaultName,
			"com.docker.compose.service": "router",
		})
	}

	t.Run("down warns and still succeeds when the router's own network disconnect fails", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		addRouterContainer(w)
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		w.Docker.FailNetworkDisconnect(1, "Error response from daemon: permission denied")
		tc := w.Context("", acme, nil)

		if err := commands.DownWithContext(ctx, tc.CommandContext); err != nil {
			t.Fatalf("expected down to succeed despite the disconnect failure, got %v", err)
		}
		lines, _ := tc.Logger.Snapshot()
		found := false
		for _, line := range lines {
			if strings.HasPrefix(line, "warn:") && strings.Contains(line, "permission denied") &&
				strings.Contains(line, "A later 'berth down' or 'berth worktree remove' retries disconnecting this network") {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected a warning naming the disconnect failure and its recovery, got %v", lines)
		}
	})

	t.Run("worktree remove warns and still succeeds when the router's own network disconnect fails", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		addRouterContainer(w)
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		w.Docker.FailNetworkDisconnect(1, "Error response from daemon: permission denied")
		tc := w.Context("", acme, nil)

		if _, err := commands.RemoveWithContext(ctx, tc.CommandContext, &commands.Env{CommandName: "berth"}, approved(commands.RemoveOptions{})); err != nil {
			t.Fatalf("expected worktree remove to succeed despite the disconnect failure, got %v", err)
		}
		lines, _ := tc.Logger.Snapshot()
		found := false
		for _, line := range lines {
			if strings.HasPrefix(line, "warn:") && strings.Contains(line, "permission denied") &&
				strings.Contains(line, "A later 'berth down' or 'berth worktree remove' retries disconnecting this network") {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected a warning naming the disconnect failure and its recovery, got %v", lines)
		}
	})

	t.Run("down returns the disconnect error instead of warning when the context is already cancelled", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		addRouterContainer(w)
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		w.Docker.FailNetworkDisconnect(1, "Error response from daemon: permission denied")
		cancelCtx, cancel := context.WithCancel(ctx)
		// Cancels ahead of the first docker call this run makes — inside
		// unrouteWorktree's own DisconnectNetwork, since RemoveWorktreeFragments
		// (file I/O only) runs before it and nothing earlier in
		// DownWithContext talks to Docker — so the lock is already held and
		// unrouteWorktree is already running by the time it fires: this
		// proves the ctx.Err() check itself, not merely that a pre-cancelled
		// context is refused somewhere upstream.
		w.Docker.BeforeFirstCall(cancel)
		tc := w.Context("", acme, nil)

		err := commands.DownWithContext(cancelCtx, tc.CommandContext)
		if err == nil {
			t.Fatal("expected an error, since the context was cancelled by the time the disconnect failed")
		}
		lines, _ := tc.Logger.Snapshot()
		for _, line := range lines {
			if strings.HasPrefix(line, "warn:") && strings.Contains(line, "permission denied") {
				t.Fatalf("expected the disconnect failure returned as an error, not downgraded to a warning: %v", lines)
			}
		}
	})
}

// TestRemoveWithContextNetworkKept pins round 2 of #55's review: a first
// cut let Reap's own ErrNetworkKept tolerance (internal/backend/compose.go)
// read as success, so RemoveWithContext went on to delete the worktree's
// home, registration and identity marker over a network Compose was still
// refusing to remove — a permanent leak nothing could ever retry against,
// since nothing was left registered to find. This proves the fix: the
// worktree stays registered, and a second `worktree remove` genuinely
// finishes the job once the network clears.
func TestRemoveWithContextNetworkKept(t *testing.T) {
	ctx := context.Background()

	// newKeptNetworkWorld registers a worktree with one container and one
	// router network, then makes every `network rm` fail as Compose does
	// when something is still attached. Also stages a fake router container
	// under the shared router's own default project/service, so
	// DisconnectNetwork's own containerID lookup finds one to disconnect —
	// without it, DisconnectNetwork returns early for "no container" and
	// never issues a call at all, the same fixture requirement
	// TestUnrouteWorktreeDisconnectFailure above needed.
	newKeptNetworkWorld := func(t *testing.T) (*commandstest.World, worktree.Ref) {
		t.Helper()
		w := commandstest.New(t, "feature/login", acme)
		w.Docker.Add("container", map[string]string{
			"com.docker.compose.project": router.DefaultName,
			"com.docker.compose.service": "router",
		})
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		w.Docker.Add("network", commandstest.Ownership(ref, brand.LabelNamespace))
		w.Docker.FailNetworkRemove(1, "Error response from daemon: error while removing network: has active endpoints")
		return w, ref
	}

	t.Run("keeps the registration and home, and warns with the retry command, when the router network cannot be removed", func(t *testing.T) {
		w, ref := newKeptNetworkWorld(t)
		tc := w.Context("", acme, nil)

		result, err := commands.RemoveWithContext(ctx, tc.CommandContext, &commands.Env{CommandName: "berth"}, approved(commands.RemoveOptions{}))
		if err != nil {
			t.Fatal(err)
		}
		if !result.NetworkKept {
			t.Fatal("expected NetworkKept")
		}
		if result.Home || result.Registration || result.Marker {
			t.Fatalf("expected the home directory, registration and identity marker all kept, got %+v", result)
		}
		for _, resource := range result.Resources {
			if resource.Kind == "network" {
				t.Fatalf("expected the kept network excluded from the summary, got %+v", result.Resources)
			}
		}
		// unrouteWorktree's own disconnect, plus one retried immediately
		// before Reap's own removal attempt — narrowing the race its
		// comment describes, per finding 1(b).
		disconnects := 0
		for _, call := range w.Docker.Calls() {
			if strings.HasPrefix(call, "network disconnect ") {
				disconnects++
			}
		}
		if disconnects != 2 {
			t.Fatalf("network disconnect calls = %d, want 2 (unrouteWorktree's own, plus the retry before Reap)", disconnects)
		}

		found, err := tc.Registry.Store(ref.RepositoryID).Find(ref.WorktreeID)
		if err != nil {
			t.Fatal(err)
		}
		if found == nil {
			t.Fatal("expected the registration to survive")
		}
		home := layout.WorktreeLayout(w.ControlHome, ref.RepositoryID, ref.WorktreeID).Home
		if exists, err := files.PathExists(home); err != nil {
			t.Fatal(err)
		} else if !exists {
			t.Fatal("expected the home directory to survive")
		}

		lines, _ := tc.Logger.Snapshot()
		wantWarn := false
		for _, line := range lines {
			if strings.HasPrefix(line, "warn:") && strings.Contains(line, "still attached elsewhere") &&
				strings.Contains(line, "Run 'berth worktree remove' again") {
				wantWarn = true
			}
		}
		if !wantWarn {
			t.Fatalf("expected a warning naming the retry, got %v", lines)
		}
	})

	t.Run("a second remove completes once the network clears", func(t *testing.T) {
		w, ref := newKeptNetworkWorld(t)
		tc := w.Context("", acme, nil)
		if _, err := commands.RemoveWithContext(ctx, tc.CommandContext, &commands.Env{CommandName: "berth"}, approved(commands.RemoveOptions{})); err != nil {
			t.Fatal(err)
		}

		w.Docker.FailNetworkRemove(0, "")
		tc2 := w.Context("", acme, nil)
		result, err := commands.RemoveWithContext(ctx, tc2.CommandContext, &commands.Env{CommandName: "berth"}, approved(commands.RemoveOptions{}))
		if err != nil {
			t.Fatal(err)
		}
		if result.AlreadyGone {
			t.Fatal("expected the second remove to finish the job it found pending, not to find nothing left")
		}
		if result.NetworkKept {
			t.Fatal("expected the network gone this time")
		}
		if !result.Home || !result.Registration {
			t.Fatalf("expected the second remove to finish the home directory and registration, got %+v", result)
		}
		found, err := tc2.Registry.Store(ref.RepositoryID).Find(ref.WorktreeID)
		if err != nil {
			t.Fatal(err)
		}
		if found != nil {
			t.Fatal("expected the registration finally gone")
		}
	})
}
