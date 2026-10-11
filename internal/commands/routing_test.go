package commands_test

// Pinning tests for the routing cleanup: `down` and `worktree remove` must
// leave a worktree unrouted, whatever else about its Docker footprint they
// do or do not find. A worktree's routes are the route files under the
// router's dynamic directory; removing them is all the cleanup there is.

import (
	"context"
	"strings"
	"testing"

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
		doc := router.NewFragmentDocument("x", "Host(`x.localhost`)", 20080)
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
		doc := router.NewFragmentDocument("x", "Host(`x.localhost`)", 20080)
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
}

// isRouterCall reports whether a rendered docker call is aimed at the shared
// router: a network connect or disconnect, the router's Compose project (as a
// flag or a label filter), or the router service's label filter. A call about
// a worktree's own project or services is not.
func isRouterCall(call string) bool {
	return strings.HasPrefix(call, "network connect ") || strings.HasPrefix(call, "network disconnect ") ||
		strings.Contains(call, "--project-name "+router.DefaultName) ||
		strings.Contains(call, "com.docker.compose.project="+router.DefaultName) ||
		strings.Contains(call, "com.docker.compose.service=router")
}

// TestRoutingNeverTouchesTheRouterContainer pins that unrouting is file
// removal alone: neither `down` nor `worktree remove` asks Docker about the
// router, connects or disconnects anything.
func TestRoutingNeverTouchesTheRouterContainer(t *testing.T) {
	ctx := context.Background()

	routerCalls := func(w *commandstest.World) []string {
		var found []string
		for _, call := range w.Docker.Calls() {
			if isRouterCall(call) {
				found = append(found, call)
			}
		}
		return found
	}

	t.Run("recognizes connects, project flags, label filters and service filters aimed at the router, and no worktree's", func(t *testing.T) {
		for _, call := range []string{
			"network connect berth-net abc",
			"network disconnect berth-net abc",
			"compose --project-name " + router.DefaultName + " ps",
			"ps --all --filter label=com.docker.compose.project=" + router.DefaultName + " --format {{.ID}}",
			"ps --filter label=com.docker.compose.service=router --quiet",
		} {
			if !isRouterCall(call) {
				t.Errorf("isRouterCall(%q) = false, want true", call)
			}
		}
		for _, call := range []string{
			"ps --all --filter label=com.docker.compose.project=acme-feature-login --quiet",
			"inspect abc123",
			"container ls --filter label=com.docker.compose.service=api",
		} {
			if isRouterCall(call) {
				t.Errorf("isRouterCall(%q) = true, want false", call)
			}
		}
	})

	t.Run("down removes the route files without a call about the router", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		w.Docker.Add("container", map[string]string{
			"com.docker.compose.project": router.DefaultName,
			"com.docker.compose.service": "router",
		})
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		doc := router.NewFragmentDocument("x", "Host(`x.localhost`)", 20080)
		if err := router.WriteFragment(w.RouterState, ref.RepositoryID, ref.WorktreeID, "api", doc); err != nil {
			t.Fatal(err)
		}
		tc := w.Context("", acme, nil)

		if err := commands.DownWithContext(ctx, tc.CommandContext); err != nil {
			t.Fatal(err)
		}
		if calls := routerCalls(w); len(calls) != 0 {
			t.Fatalf("expected no router calls, got %v", calls)
		}
	})

	t.Run("worktree remove removes the route files without a call about the router", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		tc := w.Context("", acme, nil)

		if _, err := commands.RemoveWithContext(ctx, tc.CommandContext, &commands.Env{CommandName: "berth"}, approved(commands.RemoveOptions{})); err != nil {
			t.Fatal(err)
		}
		if calls := routerCalls(w); len(calls) != 0 {
			t.Fatalf("expected no router calls, got %v", calls)
		}
	})
}

// TestRemoveWithContextNetworkKept pins that Reap's ErrNetworkKept
// tolerance (internal/backend/compose.go) never reads as success: when
// Compose refuses to remove a network something is attached to, the
// worktree stays registered with its home and identity marker, so a second
// `worktree remove` finishes the job once the network clears.
func TestRemoveWithContextNetworkKept(t *testing.T) {
	ctx := context.Background()

	// newKeptNetworkWorld registers a worktree with one container and one
	// network, then makes every `network rm` fail as Compose does when
	// something is still attached.
	newKeptNetworkWorld := func(t *testing.T) (*commandstest.World, worktree.Ref) {
		t.Helper()
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		w.Docker.Add("network", commandstest.Ownership(ref, brand.LabelNamespace))
		w.Docker.FailNetworkRemove(1, "Error response from daemon: error while removing network: has active endpoints")
		return w, ref
	}

	t.Run("keeps the registration and home, and warns with the retry command, when a network cannot be removed", func(t *testing.T) {
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
			if strings.HasPrefix(line, "warn:") && strings.Contains(line, "still in use") &&
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
