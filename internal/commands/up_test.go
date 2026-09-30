package commands_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/checkoutindex/card"
	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/commands/commandstest"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/run"
)

// up runs UpWithContext against tc's CommandContext, the shared entry point
// every subtest in this file calls.
func up(t *testing.T, tc *commandstest.TestContext, router commands.Router, index commands.Index, options commands.UpOptions) (*commands.UpResult, error) {
	t.Helper()
	return commands.UpWithContext(context.Background(), tc.CommandContext, router, index, options)
}

// givePorts sets an explicit port on every [service.<name>] the world's default
// config declares: World's fake Compose file exposes none, and a [service.<name>]
// with neither is exactly the "declares no port" refusal — real behavior
// this package's other Up tests are not about.
func givePorts(tc *commandstest.TestContext) {
	port := 8080
	for i := range tc.Loaded.Config.Service {
		tc.Loaded.Config.Service[i].Port = &port
	}
}

func TestUpWithContext(t *testing.T) {
	t.Run("registers, starts services, runs prepare and seed the first time, and prints the host and URLs", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		router := commandstest.NewFakeRouter(80, "berth-router")
		index := &commandstest.FakeIndex{}

		result, err := up(t, tc, router, index, commands.UpOptions{})
		if err != nil {
			t.Fatalf("Up: %v", err)
		}
		if result.Registration.Slug != "feature-login" || result.Registration.Domain != acme {
			t.Fatalf("unexpected registration: %+v", result.Registration)
		}
		if router.Ensures() != 1 {
			t.Fatalf("expected the router to be ensured once, got %d", router.Ensures())
		}
		if len(result.Hooks) != 0 {
			t.Fatalf("expected no hooks configured, got %v", result.Hooks)
		}
		if len(result.URLs) != 2 {
			t.Fatalf("expected two service URLs, got %+v", result.URLs)
		}

		state, err := tc.Registry.Store(result.Registration.RepositoryID).Initialization(result.Registration.WorktreeID, result.Registration.Slug)
		if err != nil || state != registry.Complete {
			t.Fatalf("expected completed initialization, got %v, %v", state, err)
		}

		if got := index.Reasons(); len(got) != 1 || got[0] != "up" {
			t.Fatalf("expected one index refresh reason [up], got %v", got)
		}

		lines, _ := tc.Logger.Snapshot()
		found := false
		for _, line := range lines {
			if strings.Contains(line, "feature-login.acme.localhost is up.") {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected an 'is up' line, got %v", lines)
		}

		upCalls := 0
		for _, line := range w.Docker.Calls() {
			if strings.HasPrefix(line, "compose ") && strings.Contains(line, " up ") {
				upCalls++
			}
		}
		if upCalls != 1 {
			t.Fatalf("expected exactly one compose up call, got %d in %v", upCalls, w.Docker.Calls())
		}

		got, err := card.ReadIndexCard(w.ControlHome, result.Registration.RepositoryID, result.Registration.WorktreeID)
		if err != nil {
			t.Fatalf("ReadIndexCard: %v", err)
		}
		if got == nil || got.Project != tc.Loaded.Config.Project.Name || got.Slug != "feature-login" || got.Domain != acme {
			t.Fatalf("unexpected index card: %+v", got)
		}
	})

	t.Run("runs only prepare on a later up, since seed only runs the first time", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		tc.Loaded.Config.Hooks = config.Hooks{Prepare: ns("./hooks/prepare"), Seed: ns("./hooks/seed")}
		router := commandstest.NewFakeRouter(80, "berth-router")
		index := &commandstest.FakeIndex{}

		first, err := up(t, tc, router, index, commands.UpOptions{})
		if err != nil {
			t.Fatalf("first Up: %v", err)
		}
		if want := []config.HookStage{config.Prepare, config.Seed}; !slices.Equal(first.Hooks, want) {
			t.Fatalf("first Up hooks = %v, want %v", first.Hooks, want)
		}

		second, err := up(t, tc, router, index, commands.UpOptions{})
		if err != nil {
			t.Fatalf("second Up: %v", err)
		}
		if want := []config.HookStage{config.Prepare}; !slices.Equal(second.Hooks, want) {
			t.Fatalf("second Up hooks = %v, want %v", second.Hooks, want)
		}
	})

	// Found in review, round 3 of #55: a worktree that drops its last
	// [service.<name>] route used to leave the shared router connected to its
	// now-unused dedicated network forever — nothing ever called
	// DisconnectNetwork for it, since that only ran on the connect side.
	t.Run("disconnects the shared router from its own network when a later up declares no routes at all", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		router := commandstest.NewFakeRouter(80, "berth-router")
		index := &commandstest.FakeIndex{}

		if _, err := up(t, tc, router, index, commands.UpOptions{}); err != nil {
			t.Fatalf("first Up: %v", err)
		}
		connected := router.Connected()
		if len(connected) != 1 {
			t.Fatalf("expected exactly one ConnectNetwork call, got %v", connected)
		}

		// Found in review, round 4 of #55: a first cut disconnected the
		// network before removing this worktree's stale fragments, leaving
		// a window where a still-live fragment's rule pointed at a network
		// the router had already left, rather than the fragment simply
		// being gone. This checks the order directly, at the exact moment
		// DisconnectNetwork is called: every fragment under the dynamic
		// directory must already be gone, matching unrouteWorktree's own
		// order for `down` and `worktree remove` (fragments first, network
		// second).
		var remainingAtDisconnect []os.DirEntry
		router.BeforeDisconnectNetwork(func() {
			entries, err := os.ReadDir(tc.RouterState.DynamicDir)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			remainingAtDisconnect = entries
		})

		tc.Loaded.Config.Service = nil
		if _, err := up(t, tc, router, index, commands.UpOptions{}); err != nil {
			t.Fatalf("second Up (no routes): %v", err)
		}
		if got := router.Connected(); len(got) != 1 {
			t.Fatalf("expected no additional ConnectNetwork call once routes dropped to none, got %v", got)
		}
		disconnected := router.Disconnected()
		if len(disconnected) != 1 || disconnected[0] != connected[0] {
			t.Fatalf("expected DisconnectNetwork called once with %q, got %v", connected[0], disconnected)
		}
		if len(remainingAtDisconnect) != 0 {
			t.Fatalf("expected every stale fragment already removed before DisconnectNetwork ran, got %v", remainingAtDisconnect)
		}
	})

	t.Run("propagates a hook failure and never completes initialization", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		tc.Loaded.Config.Hooks = config.Hooks{Prepare: ns("./hooks/prepare")}
		originalHandler := w.Runner.Handler
		w.Runner.Handler = func(ctx context.Context, spec run.Spec) (run.Result, error) {
			if spec.Command == "/repo/hooks/prepare" || strings.HasSuffix(spec.Command, "/hooks/prepare") {
				return run.Result{ExitCode: 3}, nil
			}
			return originalHandler(ctx, spec)
		}
		router := commandstest.NewFakeRouter(80, "berth-router")
		index := &commandstest.FakeIndex{}

		_, err := up(t, tc, router, index, commands.UpOptions{})
		wantErr(t, err, "The prepare hook failed \\(exit 3\\)")

		registered, findErr := tc.Registry.Current(context.Background())
		if findErr != nil || registered == nil {
			t.Fatalf("expected the registration to still exist, got %v, %v", registered, findErr)
		}
		state, err := tc.Registry.Store(registered.RepositoryID).Initialization(registered.WorktreeID, registered.Slug)
		if err != nil || state != registry.Pending {
			t.Fatalf("expected initialization to remain pending after a hook failure, got %v, %v", state, err)
		}
		if len(index.Reasons()) != 0 {
			t.Fatalf("expected no index refresh after a failed up, got %v", index.Reasons())
		}
	})

	t.Run("refuses to relabel a worktree that still owns resources under its old namespace", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register(w.Root, acme, "feature-login", "dev.predecessor", "api", "mail")
		tc := w.Context("", acme, nil) // config's own namespace defaults to brand.LabelNamespace ("dev.berth")
		givePorts(tc)
		router := commandstest.NewFakeRouter(80, "berth-router")
		index := &commandstest.FakeIndex{}

		_, err := up(t, tc, router, index, commands.UpOptions{})
		wantErr(t, err, "labelled under dev\\.predecessor, but berth\\.toml now says dev\\.berth")
		if router.Ensures() != 0 {
			t.Fatalf("expected the router never to be ensured once relabelling is refused, got %d", router.Ensures())
		}
		if len(w.Docker.Owned(ref, "dev.predecessor")) == 0 {
			t.Fatalf("expected the old namespace's resources to remain untouched")
		}
	})

	t.Run("refuses when the registration was removed while waiting for the router", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		router := commandstest.NewFakeRouter(80, "berth-router")
		router.BeforeEnsure(func() {
			identity, err := tc.Registry.Identities.Inspect(context.Background(), tc.Registry.Identities.Root, registry.InspectOptions{})
			if err != nil || identity == nil {
				t.Fatalf("inspect: %v, %v", identity, err)
			}
			if _, err := tc.Registry.Store(identity.RepositoryID).Remove(context.Background(), identity.WorktreeID, "feature-login", registry.Writer{Logger: tc.Logger, Invocation: "berth", UserHome: tc.UserHome}); err != nil {
				t.Fatalf("remove: %v", err)
			}
		})
		index := &commandstest.FakeIndex{}

		_, err := up(t, tc, router, index, commands.UpOptions{})
		wantErr(t, err, "was removed while 'berth up' was waiting for the shared router")
	})

	t.Run("refuses when the registration was relabelled while waiting for the router", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		router := commandstest.NewFakeRouter(80, "berth-router")
		relabelled := false
		router.BeforeEnsure(func() {
			if relabelled {
				return
			}
			relabelled = true
			other := registry.New(registry.Options{
				ControlHome: tc.ControlHome, Root: tc.Loaded.Root, Domain: acme,
				LabelNamespace: "dev.other", Runner: tc.Runner, Logger: tc.Logger,
				UserHome: tc.UserHome,
			})
			if _, err := other.Ensure(context.Background(), ""); err != nil {
				t.Fatalf("relabel: %v", err)
			}
		})
		index := &commandstest.FakeIndex{}

		_, err := up(t, tc, router, index, commands.UpOptions{})
		wantErr(t, err, "was re-labelled under dev.other while 'berth up' was waiting for the shared router")
	})

	t.Run("refuses when the checkout's identity markers changed while waiting for the router", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		router := commandstest.NewFakeRouter(80, "berth-router")
		router.BeforeEnsure(func() {
			identity, err := tc.Registry.Identities.Inspect(context.Background(), tc.Registry.Identities.Root, registry.InspectOptions{})
			if err != nil || identity == nil {
				t.Fatalf("inspect: %v, %v", identity, err)
			}
			// Overwritten with a different, still well-formed UUID: exactly
			// what a concurrent process re-minting the marker would leave, and
			// distinct from the "registration was removed" case above, which
			// never touches the marker itself.
			marker := filepath.Join(identity.GitDir, brand.MarkerDir, "worktree-id")
			if err := os.WriteFile(marker, []byte("11111111-1111-4111-8111-111111111111\n"), 0o600); err != nil {
				t.Fatalf("rewrite worktree marker: %v", err)
			}
		})
		index := &commandstest.FakeIndex{}

		_, err := up(t, tc, router, index, commands.UpOptions{})
		wantErr(t, err, "This checkout's berth markers changed or were removed while 'berth up' was waiting for the shared router")
	})

	// #19: the base loader accepts a control character in [service.index]
	// label or purpose — down, worktree remove, status and the rest load
	// the same file and proceed — but `up` itself is one of the few
	// callers that refuses one, and it
	// must refuse before making any Docker call.
	t.Run("refuses a control character in a service index label or purpose, before ensuring the router", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		tc.Loaded.Config.Service[0].Index = &config.ServiceIndex{Label: "a\tb"}
		router := commandstest.NewFakeRouter(80, "berth-router")
		index := &commandstest.FakeIndex{}

		_, err := up(t, tc, router, index, commands.UpOptions{})
		want := tc.Loaded.File + " is not a valid berth.toml:\n  service.api.index.label: must not contain control characters"
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if router.Ensures() != 0 {
			t.Fatalf("router.Ensures() = %d, want 0: up must refuse before touching Docker", router.Ensures())
		}
	})
}
